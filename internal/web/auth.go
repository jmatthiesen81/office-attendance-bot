package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"officebot/internal/config"
	"officebot/internal/storage"
)

// viewer is the logged-in person of a request.
type viewer struct {
	User    storage.WebUser
	Session storage.WebSession
}

type ctxKey struct{}

func viewerFrom(r *http.Request) *viewer {
	v, _ := r.Context().Value(ctxKey{}).(*viewer)
	return v
}

// cookieName returns the session cookie name. The __Host- prefix makes the
// browser refuse the cookie unless it is Secure, host-only and has Path=/;
// plain http is only allowed for localhost (development).
func (s *Server) cookieName() string {
	if s.Cfg.WebBaseURL.Scheme == "https" {
		return "__Host-officebot_session"
	}
	return "officebot_session"
}

func (s *Server) setSessionCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   s.Cfg.WebBaseURL.Scheme == "https",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// session loads the session and person of a request. The person is read on
// every request, so revoked access and role changes apply at once.
func (s *Server) session(r *http.Request) (*viewer, error) {
	c, err := r.Cookie(s.cookieName())
	if err != nil || c.Value == "" {
		return nil, storage.ErrNotFound
	}
	sess, err := s.Store.GetWebSession(r.Context(), s.TeamID, hashToken(c.Value), s.Cfg.WebSessionIdle, s.Now())
	if err != nil {
		return nil, err
	}
	u, err := s.Store.GetWebUser(r.Context(), s.TeamID, sess.SlackUserID)
	if err != nil {
		return nil, err
	}
	return &viewer{User: *u, Session: *sess}, nil
}

// auth protects a handler: it requires a session with at least role and,
// for anything but GET, a valid CSRF token and a same-origin request.
func (s *Server) auth(role string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.session(r)
		if err != nil {
			if !errors.Is(err, storage.ErrNotFound) {
				s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
				return
			}
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login?return="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			s.fail(w, r, http.StatusForbidden, "err.session_expired", nil)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, v))
		if !storage.RoleAtLeast(v.User.Role, role) {
			s.fail(w, r, http.StatusForbidden, "err.forbidden", nil)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			if !s.sameOrigin(r) || !validCSRF(r, v.Session.CSRFToken) {
				s.Log.Warn("rejected request without valid CSRF token or origin", "path", r.URL.Path, "user", v.User.SlackUserID)
				s.fail(w, r, http.StatusForbidden, "err.csrf", nil)
				return
			}
		}
		next(w, r)
	})
}

// sameOrigin checks Origin (or, if a browser omits it, Referer) against
// WEB_BASE_URL.
func (s *Server) sameOrigin(r *http.Request) bool {
	want := s.Cfg.WebBaseURL.Scheme + "://" + s.Cfg.WebBaseURL.Host
	if o := r.Header.Get("Origin"); o != "" {
		return o == want
	}
	ref := r.Header.Get("Referer")
	return ref == want || strings.HasPrefix(ref, want+"/")
}

func validCSRF(r *http.Request, token string) bool {
	got := r.PostFormValue("csrf")
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// randomToken returns 32 random bytes, base64url encoded.
func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// safeReturn accepts only local paths as redirect target after the login.
func safeReturn(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "\\\r\n") || strings.HasPrefix(p, "/auth/") {
		return "/"
	}
	return p
}

// ---------------------------------------------------------------- handlers

func (s *Server) devLoginEnabled() bool {
	return s.Cfg.DryRun && config.IsLocalhost(s.Cfg.WebBaseURL)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if v, err := s.session(r); err == nil && v != nil {
		http.Redirect(w, r, safeReturn(r.URL.Query().Get("return")), http.StatusSeeOther)
		return
	}
	p := &Page{Title: tr(s.lang(r), "login.title"), Data: url.QueryEscape(safeReturn(r.URL.Query().Get("return")))}
	if s.devLoginEnabled() {
		p.DevLogin, _ = s.Store.ListWebUsers(r.Context(), s.TeamID)
	}
	s.render(w, r, http.StatusOK, "login", p)
}

func (s *Server) handleAuthStart(w http.ResponseWriter, r *http.Request) {
	state, nonce := randomToken(), randomToken()
	returnTo := safeReturn(r.URL.Query().Get("return"))
	if err := s.Store.CreateLoginState(r.Context(), hashToken(state), nonce, returnTo, s.Now().Add(10*time.Minute)); err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	q := url.Values{
		"response_type": {"code"},
		"scope":         {"openid profile"},
		"client_id":     {s.Cfg.SlackClientID},
		"redirect_uri":  {s.redirectURI()},
		"state":         {state},
		"nonce":         {nonce},
		"team":          {s.TeamID},
	}
	http.Redirect(w, r, s.OIDC.AuthorizeURL+"?"+q.Encode(), http.StatusFound)
}

func (s *Server) redirectURI() string { return s.Cfg.WebBaseURL.String() + "/auth/callback" }

// idClaims are the claims of Slack's ID token that the login checks.
type idClaims struct {
	Iss    string          `json:"iss"`
	Aud    json.RawMessage `json:"aud"`
	Exp    int64           `json:"exp"`
	Nonce  string          `json:"nonce"`
	UserID string          `json:"https://slack.com/user_id"`
	TeamID string          `json:"https://slack.com/team_id"`
	Name   string          `json:"name"`
}

func (c *idClaims) hasAudience(clientID string) bool {
	var one string
	if json.Unmarshal(c.Aud, &one) == nil {
		return one == clientID
	}
	var many []string
	if json.Unmarshal(c.Aud, &many) == nil {
		for _, a := range many {
			if a == clientID {
				return true
			}
		}
	}
	return false
}

func (s *Server) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		s.Log.Info("slack login cancelled or refused", "error", e)
		s.fail(w, r, http.StatusForbidden, "err.login_cancelled", nil)
		return
	}
	nonce, returnTo, err := s.Store.ConsumeLoginState(r.Context(), hashToken(q.Get("state")), s.Now())
	if errors.Is(err, storage.ErrNotFound) {
		s.fail(w, r, http.StatusBadRequest, "err.login_state", nil)
		return
	}
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	claims, err := s.exchangeCode(r.Context(), q.Get("code"))
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "err.login_failed", err)
		return
	}
	if err := s.checkClaims(claims, nonce); err != nil {
		s.Log.Warn("slack login rejected", "err", err, "user", claims.UserID, "team", claims.TeamID)
		s.fail(w, r, http.StatusForbidden, "err.login_failed", nil)
		return
	}
	s.startSession(w, r, claims.UserID, claims.Name, returnTo, "slack")
}

// checkClaims validates the ID token. Its signature is not verified: the
// token comes directly from Slack's token endpoint over TLS (OIDC Core
// 3.1.3.7), so issuer, audience, expiry, nonce and workspace are checked.
func (s *Server) checkClaims(c *idClaims, nonce string) error {
	switch {
	case c.Iss != "https://slack.com":
		return fmt.Errorf("unexpected issuer %q", c.Iss)
	case !c.hasAudience(s.Cfg.SlackClientID):
		return errors.New("token not issued for this app")
	case time.Unix(c.Exp, 0).Before(s.Now().Add(-time.Minute)):
		return errors.New("token expired")
	case c.Nonce == "" || subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(nonce)) != 1:
		return errors.New("nonce mismatch")
	case c.TeamID != s.TeamID:
		return fmt.Errorf("foreign workspace %q", c.TeamID)
	case c.UserID == "":
		return errors.New("token without user ID")
	}
	return nil
}

// exchangeCode trades the authorization code for the ID token.
func (s *Server) exchangeCode(ctx context.Context, code string) (*idClaims, error) {
	if code == "" {
		return nil, errors.New("callback without code")
	}
	form := url.Values{
		"client_id":     {s.Cfg.SlackClientID},
		"client_secret": {s.Cfg.SlackClientSecret},
		"code":          {code},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {s.redirectURI()},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.OIDC.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := s.OIDC.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer res.Body.Close()
	var body struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("token response (HTTP %d): %w", res.StatusCode, err)
	}
	if !body.OK || body.IDToken == "" {
		return nil, fmt.Errorf("token request refused: %s", body.Error)
	}
	parts := strings.Split(body.IDToken, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed ID token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("ID token payload: %w", err)
	}
	var c idClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("ID token claims: %w", err)
	}
	return &c, nil
}

// startSession logs userID in if they have web access.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID, name, returnTo, method string) {
	ctx := r.Context()
	now := s.Now()
	if _, err := s.Store.GetWebUser(ctx, s.TeamID, userID); err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
			return
		}
		s.Log.Info("login without web access refused", "user", userID)
		_ = s.Store.Audit(ctx, s.TeamID, userID, "login.denied", map[string]any{"name": name}, now)
		s.fail(w, r, http.StatusForbidden, "err.no_access", nil)
		return
	}
	id := randomToken()
	sess := storage.WebSession{IDHash: hashToken(id), SlackUserID: userID, CSRFToken: randomToken(),
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(s.Cfg.WebSessionTTL)}
	if err := s.Store.CreateWebSession(ctx, s.TeamID, sess); err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	if err := s.Store.RecordWebLogin(ctx, s.TeamID, userID, name, now); err != nil {
		s.Log.Warn("record login", "err", err)
	}
	_ = s.Store.Audit(ctx, s.TeamID, userID, "login", map[string]any{"method": method}, now)
	s.Log.Info("web login", "user", userID, "method", method)
	s.setSessionCookie(w, id, int(s.Cfg.WebSessionTTL.Seconds()))
	http.Redirect(w, r, safeReturn(returnTo), http.StatusSeeOther)
}

// handleDevLogin logs in without Slack. It only exists with DRY_RUN=true and
// a localhost WEB_BASE_URL, i.e. never in production.
func (s *Server) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	if !s.devLoginEnabled() {
		http.NotFound(w, r)
		return
	}
	s.startSession(w, r, r.URL.Query().Get("user"), "", r.URL.Query().Get("return"), "dev")
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	v := viewerFrom(r)
	if err := s.Store.DeleteWebSession(r.Context(), v.Session.IDHash); err != nil {
		s.Log.Warn("delete session", "err", err)
	}
	s.setSessionCookie(w, "", -1)
	redirect(w, r, "/login", "logged_out", "")
}

// ----------------------------------------------------------------- limiter

// limiter is a token bucket for the login endpoints.
type limiter struct {
	mu     sync.Mutex
	tokens float64
	burst  float64
	every  time.Duration
	last   time.Time
}

func newLimiter(burst int, every time.Duration) *limiter {
	return &limiter{tokens: float64(burst), burst: float64(burst), every: every}
}

func (l *limiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() {
		l.tokens = min(l.burst, l.tokens+float64(now.Sub(l.last))/float64(l.every))
	}
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

func (s *Server) limited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow(s.Now()) {
			s.fail(w, r, http.StatusTooManyRequests, "err.rate_limited", nil)
			return
		}
		next(w, r)
	}
}
