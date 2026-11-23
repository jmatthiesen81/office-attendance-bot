package web

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeJWKS serves the public halves of keys as a JWKS document.
type fakeJWKS struct {
	mu      sync.Mutex
	keys    map[string]*rsa.PrivateKey
	fetches int
}

func (f *fakeJWKS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches++
	var out []map[string]string
	for kid, k := range f.keys {
		out = append(out, map[string]string{
			"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes()),
		})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": out})
}

func (f *fakeJWKS) set(keys map[string]*rsa.PrivateKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = keys
}

func (f *fakeJWKS) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetches
}

func newRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func signToken(t *testing.T, key *rsa.PrivateKey, alg, kid string, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(map[string]string{"alg": alg, "kid": kid, "typ": "JWT"})
	p, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(p)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// newVerifyEnv returns a test environment whose server checks ID tokens
// against a fake JWKS endpoint, and a clock the test can move.
func newVerifyEnv(t *testing.T, keys map[string]*rsa.PrivateKey) (*testEnv, *fakeJWKS, *time.Time) {
	t.Helper()
	e := newTestEnv(t, nil)
	jwks := &fakeJWKS{keys: keys}
	ks := httptest.NewServer(jwks)
	t.Cleanup(ks.Close)
	e.srv.OIDC.KeysURL = ks.URL
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	e.srv.Now = func() time.Time { return now }
	return e, jwks, &now
}

func TestVerifyIDToken(t *testing.T) {
	good, other := newRSAKey(t), newRSAKey(t)
	e, _, _ := newVerifyEnv(t, map[string]*rsa.PrivateKey{"k1": good})
	ctx := context.Background()
	claims := map[string]any{"iss": "https://slack.com", "https://slack.com/user_id": "U1"}

	payload, err := e.srv.verifyIDToken(ctx, signToken(t, good, "RS256", "k1", claims))
	if err != nil {
		t.Fatalf("valid token refused: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil || got["https://slack.com/user_id"] != "U1" {
		t.Fatalf("payload = %s, %v", payload, err)
	}

	valid := signToken(t, good, "RS256", "k1", claims)
	tampered := valid[:len(valid)-4] + "AAAA"
	for name, token := range map[string]string{
		"wrong key":       signToken(t, other, "RS256", "k1", claims),
		"unknown key ID":  signToken(t, other, "RS256", "k9", claims),
		"tampered":        tampered,
		"HS256":           signToken(t, good, "HS256", "k1", claims),
		"alg none":        signToken(t, good, "none", "k1", claims),
		"two parts":       "a.b",
		"garbage header":  "!!!.e30.sig",
		"unsigned (test)": "eyJhbGciOiJSUzI1NiJ9.e30.sig",
	} {
		if _, err := e.srv.verifyIDToken(ctx, token); err == nil {
			t.Errorf("%s: token accepted", name)
		}
	}
}

func TestVerifyIDTokenKeyRotation(t *testing.T) {
	k1, k2 := newRSAKey(t), newRSAKey(t)
	e, jwks, now := newVerifyEnv(t, map[string]*rsa.PrivateKey{"k1": k1})
	ctx := context.Background()
	claims := map[string]any{"iss": "https://slack.com"}

	if _, err := e.srv.verifyIDToken(ctx, signToken(t, k1, "RS256", "k1", claims)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.verifyIDToken(ctx, signToken(t, k1, "RS256", "k1", claims)); err != nil {
		t.Fatal(err)
	}
	if n := jwks.count(); n != 1 {
		t.Fatalf("%d key fetches, want 1 (cached)", n)
	}

	// Slack rotates to k2. Within the minute after the last fetch, an
	// unknown key ID does not trigger another fetch.
	jwks.set(map[string]*rsa.PrivateKey{"k2": k2})
	if _, err := e.srv.verifyIDToken(ctx, signToken(t, k2, "RS256", "k2", claims)); err == nil {
		t.Fatal("unknown key accepted without refetch")
	}
	if n := jwks.count(); n != 1 {
		t.Fatalf("%d key fetches, want 1 (refetch limited)", n)
	}
	*now = now.Add(keysMinRefetch)
	if _, err := e.srv.verifyIDToken(ctx, signToken(t, k2, "RS256", "k2", claims)); err != nil {
		t.Fatalf("rotated key refused: %v", err)
	}
	if n := jwks.count(); n != 2 {
		t.Fatalf("%d key fetches, want 2", n)
	}
}

func TestVerifyIDTokenKeysUnreachable(t *testing.T) {
	k1 := newRSAKey(t)
	e, _, now := newVerifyEnv(t, map[string]*rsa.PrivateKey{"k1": k1})
	ctx := context.Background()
	token := signToken(t, k1, "RS256", "k1", map[string]any{})
	if _, err := e.srv.verifyIDToken(ctx, token); err != nil {
		t.Fatal(err)
	}
	// Keys are stale and Slack is down: the cached key keeps working.
	e.srv.OIDC.KeysURL = "http://127.0.0.1:1/keys"
	*now = now.Add(keysMaxAge + time.Hour)
	if _, err := e.srv.verifyIDToken(ctx, token); err != nil {
		t.Fatalf("cached key not used while Slack is unreachable: %v", err)
	}
}

func TestLoginChecksSignature(t *testing.T) {
	// The fake token endpoint of testEnv issues unsigned tokens, so with
	// signature checks on, every login fails.
	e, _, _ := newVerifyEnv(t, map[string]*rsa.PrivateKey{"k1": newRSAKey(t)})
	e.addWebUser("U1", "admin")
	if res := e.login(e.client(), "U1", nil); res.StatusCode != http.StatusBadGateway {
		t.Fatalf("login with unsigned token: %d, want %d", res.StatusCode, http.StatusBadGateway)
	}

	// WEB_VERIFY_ID_TOKEN=false restores the old behaviour.
	e2 := newTestEnv(t, map[string]string{"WEB_VERIFY_ID_TOKEN": "false"})
	e2.srv.OIDC.KeysURL = "http://127.0.0.1:1/keys"
	e2.addWebUser("U1", "admin")
	if res := e2.login(e2.client(), "U1", nil); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("login without signature check: %d, want %d", res.StatusCode, http.StatusSeeOther)
	}
}
