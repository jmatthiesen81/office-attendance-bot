package web

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// keysMaxAge is how long fetched signing keys are used before they are
	// loaded again.
	keysMaxAge = 24 * time.Hour
	// keysMinRefetch limits reloads triggered by an unknown key ID, so tokens
	// with made-up key IDs cannot make the bot hammer Slack.
	keysMinRefetch = time.Minute
)

// keyCache holds Slack's public signing keys (JWKS), by key ID.
type keyCache struct {
	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
	triedAt   time.Time
}

// key returns the public key with the ID kid. It loads the keys when none
// are cached, when they are older than keysMaxAge or, at most once per
// keysMinRefetch, when kid is unknown (Slack rotated its keys).
func (c *keyCache) key(ctx context.Context, o OIDC, kid string, now time.Time) (*rsa.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k, ok := c.keys[kid]
	stale := c.keys == nil || now.Sub(c.fetchedAt) > keysMaxAge
	if ok && !stale {
		return k, nil
	}
	if stale || now.Sub(c.triedAt) >= keysMinRefetch {
		c.triedAt = now
		keys, err := fetchKeys(ctx, o)
		if err != nil {
			// Keep using cached keys if Slack is briefly unreachable.
			if ok {
				return k, nil
			}
			return nil, err
		}
		c.keys, c.fetchedAt = keys, now
		if k, ok = keys[kid]; ok {
			return k, nil
		}
	}
	return nil, fmt.Errorf("unknown signing key %q", kid)
}

// fetchKeys loads the RSA keys from the JWKS endpoint.
func fetchKeys(ctx context.Context, o OIDC) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.KeysURL, nil)
	if err != nil {
		return nil, err
	}
	res, err := o.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("signing keys: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("signing keys: HTTP %d", res.StatusCode)
	}
	var body struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("signing keys: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range body.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() < 2048 {
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, errors.New("signing keys: no usable RSA key")
	}
	return keys, nil
}

// verifyIDToken checks the RS256 signature of an ID token against Slack's
// published keys and returns its payload.
func (s *Server) verifyIDToken(ctx context.Context, token string) ([]byte, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed ID token")
	}
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("ID token header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(rawHeader, &header); err != nil {
		return nil, fmt.Errorf("ID token header: %w", err)
	}
	// Only RS256: never let the token choose "none" or an HMAC algorithm.
	if header.Alg != "RS256" {
		return nil, fmt.Errorf("unexpected ID token algorithm %q", header.Alg)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("ID token signature: %w", err)
	}
	pub, err := s.keys.key(ctx, s.OIDC, header.Kid, s.Now())
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return nil, errors.New("invalid ID token signature")
	}
	return base64.RawURLEncoding.DecodeString(parts[1])
}
