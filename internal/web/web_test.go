package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"officebot/internal/config"
	"officebot/internal/participants"
	"officebot/internal/poll"
	"officebot/internal/slack/slacktest"
	"officebot/internal/stats"
	"officebot/internal/storage"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// testEnv is a web interface with a fake Slack (API and OpenID Connect).
type testEnv struct {
	t     *testing.T
	ctx   context.Context
	cfg   *config.Config
	store *storage.Store
	api   *slacktest.Fake
	svc   *poll.Service
	stats *stats.Service
	srv   *Server
	http  *httptest.Server
	oidc  *httptest.Server

	mu     sync.Mutex
	claims map[string]any // claims of the next ID token; nonce is filled in
}

func newTestEnv(t *testing.T, extra map[string]string) *testEnv {
	t.Helper()
	e := &testEnv{t: t, ctx: context.Background(), api: slacktest.New()}
	e.oidc = httptest.NewServer(http.HandlerFunc(e.serveToken))
	t.Cleanup(e.oidc.Close)

	// The web server needs its own URL in the config before it starts.
	var handler http.Handler
	e.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(e.http.Close)

	get := map[string]string{
		"DRY_RUN": "true", "SLACK_SUMMARY_CHANNEL_ID": "CSUM", "SLACK_USER_IDS": "U1,U2,U3", "BOT_LANGUAGE": "de",
		"WEB_LISTEN_ADDR": ":0", "WEB_BASE_URL": e.http.URL, "SLACK_CLIENT_ID": "cid", "SLACK_CLIENT_SECRET": "secret",
	}
	for k, v := range extra {
		get[k] = v
	}
	cfg, err := config.LoadFrom(func(k string) string { return get[k] })
	if err != nil {
		t.Fatal(err)
	}
	e.cfg = cfg
	e.store, err = storage.Open(e.ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.store.Close() })
	for _, u := range []string{"U1", "U2", "U3"} {
		e.api.AddUser(u, "Name "+u)
	}
	people := participants.New(e.api, participants.Source{UserIDs: cfg.UserIDs}, "T1", "UBOT", quiet)
	e.svc = poll.NewService(e.store, e.api, people, cfg, "T1", nil, nil, quiet)
	e.stats = stats.New(e.store, cfg, "T1", quiet)
	e.svc.SyncStats = e.stats.SyncPolls
	e.srv, err = New(Deps{Store: e.store, Svc: e.svc, Stats: e.stats, People: people, Slack: e.api, Cfg: cfg, TeamID: "T1", Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	e.srv.OIDC = OIDC{AuthorizeURL: e.oidc.URL + "/authorize", TokenURL: e.oidc.URL + "/token", Client: e.oidc.Client()}
	handler = e.srv.Handler()
	return e
}

func (e *testEnv) serveToken(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/token" || r.PostFormValue("client_secret") != "secret" || r.PostFormValue("code") != "good-code" {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_code"})
		return
	}
	e.mu.Lock()
	claims := e.claims
	e.mu.Unlock()
	payload, _ := json.Marshal(claims)
	token := "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "access_token": "xoxp", "id_token": token})
}

// client returns a browser-like client with cookies that does not follow redirects.
func (e *testEnv) client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (e *testEnv) addWebUser(id, role string) {
	e.t.Helper()
	if err := e.store.AddWebUser(e.ctx, "T1", storage.WebUser{SlackUserID: id, Role: role, DisplayName: "Name " + id}, "cli", time.Now()); err != nil {
		e.t.Fatal(err)
	}
}

// login runs the Slack login for userID and returns the final response.
// modify may change the claims before Slack issues the token.
func (e *testEnv) login(c *http.Client, userID string, modify func(map[string]any)) *http.Response {
	e.t.Helper()
	res := e.get(c, "/auth/start?return=%2Fstats")
	if res.StatusCode != http.StatusFound {
		e.t.Fatalf("auth/start: %d", res.StatusCode)
	}
	loc, _ := url.Parse(res.Header.Get("Location"))
	q := loc.Query()
	if q.Get("client_id") != "cid" || q.Get("team") != "T1" || q.Get("redirect_uri") != e.http.URL+"/auth/callback" || q.Get("scope") != "openid profile" {
		e.t.Fatalf("unexpected authorize request: %s", loc)
	}
	claims := map[string]any{
		"iss": "https://slack.com", "aud": "cid", "exp": e.srv.Now().Add(time.Hour).Unix(), "nonce": q.Get("nonce"),
		"https://slack.com/user_id": userID, "https://slack.com/team_id": "T1", "name": "Slack " + userID,
	}
	if modify != nil {
		modify(claims)
	}
	e.mu.Lock()
	e.claims = claims
	e.mu.Unlock()
	return e.get(c, "/auth/callback?code=good-code&state="+url.QueryEscape(q.Get("state")))
}

func (e *testEnv) get(c *http.Client, path string) *http.Response {
	e.t.Helper()
	res, err := c.Get(e.http.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { res.Body.Close() })
	return res
}

// post submits a form like a browser of this origin; csrf "" uses the
// token from the page at csrfFrom.
func (e *testEnv) post(c *http.Client, path string, form url.Values, origin string) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.http.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	res, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { res.Body.Close() })
	return res
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (e *testEnv) csrf(c *http.Client, page string) string {
	e.t.Helper()
	body := e.body(e.get(c, page))
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		e.t.Fatalf("no CSRF token on %s", page)
	}
	return m[1]
}

func (e *testEnv) body(res *http.Response) string {
	e.t.Helper()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func TestLogin(t *testing.T) {
	e := newTestEnv(t, nil)
	e.addWebUser("U1", storage.RoleAdmin)
	c := e.client()

	if res := e.get(c, "/access"); res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/login?return=") {
		t.Fatalf("anonymous request not redirected to login: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	res := e.login(c, "U1", nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/stats" {
		t.Fatalf("login: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	var session *http.Cookie
	for _, ck := range res.Cookies() {
		if ck.Name == "officebot_session" {
			session = ck
		}
	}
	if session == nil || !session.HttpOnly || session.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie missing or not protected: %+v", res.Cookies())
	}
	if res := e.get(c, "/access"); res.StatusCode != http.StatusOK {
		t.Fatalf("access page after login: %d", res.StatusCode)
	}
	if u, _ := e.store.GetWebUser(e.ctx, "T1", "U1"); u.DisplayName != "Slack U1" || u.LastLoginAt.IsZero() {
		t.Errorf("login not recorded: %+v", u)
	}
	if h := e.get(c, "/").Header.Get("Content-Security-Policy"); !strings.Contains(h, "default-src 'self'") {
		t.Errorf("CSP header missing: %q", h)
	}

	// Revoking access ends the session at once.
	e.addWebUser("U2", storage.RoleAdmin)
	if err := e.store.RemoveWebUser(e.ctx, "T1", "U1", "U2", time.Now()); err != nil {
		t.Fatal(err)
	}
	if res := e.get(c, "/"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("revoked session still valid: %d", res.StatusCode)
	}
}

func TestLoginRejected(t *testing.T) {
	e := newTestEnv(t, nil)
	e.addWebUser("U1", storage.RoleAdmin)
	for name, tc := range map[string]struct {
		user   string
		modify func(map[string]any)
		status int
	}{
		"foreign workspace": {"U1", func(c map[string]any) { c["https://slack.com/team_id"] = "T2" }, http.StatusForbidden},
		"wrong nonce":       {"U1", func(c map[string]any) { c["nonce"] = "other" }, http.StatusForbidden},
		"wrong audience":    {"U1", func(c map[string]any) { c["aud"] = "other-app" }, http.StatusForbidden},
		"wrong issuer":      {"U1", func(c map[string]any) { c["iss"] = "https://evil.example" }, http.StatusForbidden},
		"expired":           {"U1", func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }, http.StatusForbidden},
		"no web access":     {"U3", nil, http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			c := e.client()
			res := e.login(c, tc.user, tc.modify)
			if res.StatusCode != tc.status || len(res.Cookies()) > 0 {
				t.Errorf("status %d (want %d), cookies %v", res.StatusCode, tc.status, res.Cookies())
			}
		})
	}

	t.Run("state used twice", func(t *testing.T) {
		c := e.client()
		res := e.get(c, "/auth/start")
		loc, _ := url.Parse(res.Header.Get("Location"))
		state := loc.Query().Get("state")
		e.mu.Lock()
		e.claims = map[string]any{"iss": "https://slack.com", "aud": "cid", "exp": time.Now().Add(time.Hour).Unix(), "nonce": loc.Query().Get("nonce"),
			"https://slack.com/user_id": "U1", "https://slack.com/team_id": "T1"}
		e.mu.Unlock()
		if res := e.get(c, "/auth/callback?code=good-code&state="+url.QueryEscape(state)); res.StatusCode != http.StatusSeeOther {
			t.Fatalf("first use: %d", res.StatusCode)
		}
		if res := e.get(e.client(), "/auth/callback?code=good-code&state="+url.QueryEscape(state)); res.StatusCode != http.StatusBadRequest {
			t.Errorf("second use: %d", res.StatusCode)
		}
	})

	t.Run("unknown state", func(t *testing.T) {
		if res := e.get(e.client(), "/auth/callback?code=good-code&state=forged"); res.StatusCode != http.StatusBadRequest {
			t.Errorf("forged state: %d", res.StatusCode)
		}
	})

	t.Run("open redirect", func(t *testing.T) {
		for _, p := range []string{"//evil.example", "https://evil.example", "/\\evil.example", "/auth/start"} {
			if got := safeReturn(p); got != "/" {
				t.Errorf("safeReturn(%q) = %q", p, got)
			}
		}
	})
}

func TestRolesAndCSRF(t *testing.T) {
	e := newTestEnv(t, nil)
	e.addWebUser("U1", storage.RoleAdmin)
	e.addWebUser("U2", storage.RoleViewer)

	viewer := e.client()
	e.login(viewer, "U2", nil)
	if res := e.get(viewer, "/"); res.StatusCode != http.StatusOK {
		t.Errorf("viewer overview: %d", res.StatusCode)
	}
	if res := e.get(viewer, "/access"); res.StatusCode != http.StatusForbidden {
		t.Errorf("viewer on admin page: %d", res.StatusCode)
	}

	admin := e.client()
	e.login(admin, "U1", nil)
	token := e.csrf(admin, "/access")
	form := url.Values{"user": {"U2"}, "role": {"admin"}}

	if res := e.post(admin, "/access/role", form, e.http.URL); res.StatusCode != http.StatusForbidden {
		t.Errorf("POST without CSRF token: %d", res.StatusCode)
	}
	form.Set("csrf", token)
	if res := e.post(admin, "/access/role", form, "https://evil.example"); res.StatusCode != http.StatusForbidden {
		t.Errorf("POST from foreign origin: %d", res.StatusCode)
	}
	if res := e.post(admin, "/access/role", form, e.http.URL); res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "msg=saved") {
		t.Errorf("valid POST: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if u, _ := e.store.GetWebUser(e.ctx, "T1", "U2"); u.Role != storage.RoleAdmin {
		t.Errorf("role not changed: %s", u.Role)
	}
	// The viewer, now admin, sees admin pages without logging in again.
	if res := e.get(viewer, "/access"); res.StatusCode != http.StatusOK {
		t.Errorf("role change not applied to running session: %d", res.StatusCode)
	}

	// The last admin cannot remove themselves.
	if err := e.store.RemoveWebUser(e.ctx, "T1", "U2", "U1", time.Now()); err != nil {
		t.Fatal(err)
	}
	res := e.post(admin, "/access/remove", url.Values{"csrf": {token}, "user": {"U1"}}, e.http.URL)
	if !strings.Contains(res.Header.Get("Location"), "err=last_admin") {
		t.Errorf("removing the last admin: %s", res.Header.Get("Location"))
	}

	// Adding a person looks the name up in Slack.
	res = e.post(admin, "/access/add", url.Values{"csrf": {token}, "user": {"U3"}, "role": {"viewer"}}, e.http.URL)
	if !strings.Contains(res.Header.Get("Location"), "msg=added") {
		t.Errorf("add: %s", res.Header.Get("Location"))
	}
	if u, err := e.store.GetWebUser(e.ctx, "T1", "U3"); err != nil || u.DisplayName != "Name U3" || u.AddedBy != "U1" {
		t.Errorf("added user: %+v %v", u, err)
	}

	// Logging out ends the session.
	if res := e.post(admin, "/logout", url.Values{"csrf": {token}}, e.http.URL); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout: %d", res.StatusCode)
	}
	if res := e.get(admin, "/access"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("session valid after logout: %d", res.StatusCode)
	}
}

func TestDevLogin(t *testing.T) {
	e := newTestEnv(t, nil)
	e.addWebUser("U1", storage.RoleAdmin)
	c := e.client()
	if res := e.get(c, "/auth/dev?user=U1"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("dev login with DRY_RUN on localhost: %d", res.StatusCode)
	}
	e.srv.Cfg.DryRun = false
	defer func() { e.srv.Cfg.DryRun = true }()
	if res := e.get(e.client(), "/auth/dev?user=U1"); res.StatusCode != http.StatusNotFound {
		t.Errorf("dev login without DRY_RUN: %d", res.StatusCode)
	}
}

func TestTextsComplete(t *testing.T) {
	for k := range textsDE {
		if _, ok := textsEN[k]; !ok {
			t.Errorf("%s missing in English", k)
		}
	}
	for k := range textsEN {
		if _, ok := textsDE[k]; !ok {
			t.Errorf("%s missing in German", k)
		}
	}
}

// TestTextKeysExist checks every key used in templates and code.
func TestTextKeysExist(t *testing.T) {
	keyRe := regexp.MustCompile(`\.T "([a-z_]+\.[a-z_.]+)"`)
	files, _ := filepath.Glob("templates/*.html")
	goFiles, _ := filepath.Glob("*.go")
	codeRe := regexp.MustCompile(`(?:tr\([a-zA-Z.()]+, |fail\(w, r, [a-zA-Z.]+, )"([a-z_]+\.[a-z_.]+)"`)
	redirRe := regexp.MustCompile(`redirect\(w, r, [^,]+, "([a-z_]*)", "([a-z_]*)"\)`)
	used := map[string]bool{}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, m := range keyRe.FindAllStringSubmatch(string(b), -1) {
			used[m[1]] = true
		}
	}
	for _, f := range goFiles {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, m := range codeRe.FindAllStringSubmatch(string(b), -1) {
			used[m[1]] = true
		}
		for _, m := range redirRe.FindAllStringSubmatch(string(b), -1) {
			if m[1] != "" {
				used["msg."+m[1]] = true
			}
			if m[2] != "" {
				used["err."+m[2]] = true
			}
		}
	}
	for _, s := range []string{"office", "remote", "away", "no_answer"} {
		used["status."+s] = true
	}
	if len(used) < 50 {
		t.Fatalf("only %d keys found; is the pattern still right?", len(used))
	}
	for k := range used {
		for lang, m := range texts {
			if _, ok := m[k]; !ok {
				t.Errorf("%s: key %q is used but missing", lang, k)
			}
		}
	}
}

// TestTextFormats checks that texts with placeholders render without
// formatting errors (e.g. an unescaped percent sign).
func TestTextFormats(t *testing.T) {
	verb := regexp.MustCompile(`%[ds]`)
	for lang, m := range texts {
		for k, v := range m {
			verbs := verb.FindAllString(v, -1)
			if len(verbs) == 0 {
				continue
			}
			args := make([]any, len(verbs))
			for i, vb := range verbs {
				args[i] = "x"
				if vb == "%d" {
					args[i] = 1
				}
			}
			if out := tr(lang, k, args...); strings.Contains(out, "%!") {
				t.Errorf("%s %s: %q", lang, k, out)
			}
		}
	}
}

func TestAnalystRole(t *testing.T) {
	for _, perPerson := range []bool{true, false} {
		t.Run(fmt.Sprintf("per_person=%v", perPerson), func(t *testing.T) {
			e := newTestEnv(t, map[string]string{"WEB_STATS_PER_PERSON": fmt.Sprint(perPerson)})
			e.addWebUser("U1", storage.RoleAdmin)
			e.addWebUser("U2", storage.RoleAnalyst)
			e.addWebUser("U3", storage.RoleViewer)

			analyst := e.client()
			e.login(analyst, "U2", nil)
			for _, p := range []string{"/", "/stats", "/stats.csv"} {
				if res := e.get(analyst, p); res.StatusCode != http.StatusOK {
					t.Errorf("analyst on %s: %d", p, res.StatusCode)
				}
			}
			for _, p := range []string{"/weeks", "/days", "/access", "/audit", "/retention", "/absences"} {
				if res := e.get(analyst, p); res.StatusCode != http.StatusForbidden {
					t.Errorf("analyst on admin page %s: %d", p, res.StatusCode)
				}
			}
			want := http.StatusForbidden
			if perPerson {
				want = http.StatusOK
			}
			if res := e.get(analyst, "/stats/persons.csv"); res.StatusCode != want {
				t.Errorf("analyst per-person export: %d, want %d", res.StatusCode, want)
			}
			if body := e.body(e.get(analyst, "/stats")); strings.Contains(body, `name="persons"`) != perPerson {
				t.Errorf("per-person switch shown = %v, want %v", !perPerson, perPerson)
			}

			viewer := e.client()
			e.login(viewer, "U3", nil)
			if res := e.get(viewer, "/stats/persons.csv"); res.StatusCode != http.StatusForbidden {
				t.Errorf("viewer per-person export: %d", res.StatusCode)
			}
		})
	}
}

func TestLanguageSwitch(t *testing.T) {
	e := newTestEnv(t, map[string]string{"BOT_LANGUAGE": "de"})
	e.addWebUser("U1", storage.RoleAdmin)
	c := e.client()

	// English by default, even with a German bot language.
	if body := e.body(e.get(c, "/login")); !strings.Contains(body, `<html lang="en">`) {
		t.Error("login page not English by default")
	}
	res := e.get(c, "/lang?to=de&return="+url.QueryEscape("/login?return=%2Fstats"))
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login?return=%2Fstats" {
		t.Fatalf("switch: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if body := e.body(e.get(c, "/login")); !strings.Contains(body, `<html lang="de">`) {
		t.Error("login page not German after the switch")
	}
	e.login(c, "U1", nil)
	if body := e.body(e.get(c, "/stats")); !strings.Contains(body, `<html lang="de">`) || !strings.Contains(body, "return=%2fstats") && !strings.Contains(body, "return=%2Fstats") {
		t.Error("German not kept after login or switch does not return to the page")
	}

	// Only local targets and known languages.
	if loc := e.get(c, "/lang?to=xx&return=https://evil.example").Header.Get("Location"); loc != "/" {
		t.Errorf("foreign return target: %s", loc)
	}
	if body := e.body(e.get(c, "/")); !strings.Contains(body, `<html lang="en">`) {
		t.Error("unknown language does not fall back to English")
	}
}
