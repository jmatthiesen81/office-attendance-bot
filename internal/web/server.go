// Package web serves the optional web interface for administration and
// statistics (ADR 0003). People log in with Slack (OpenID Connect); who may
// log in and with which role is stored in the database.
package web

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"officebot/internal/config"
	"officebot/internal/participants"
	"officebot/internal/poll"
	"officebot/internal/slack"
	"officebot/internal/stats"
	"officebot/internal/storage"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Directory lists the Slack users of the workspace.
type Directory interface {
	Directory(ctx context.Context, channelID string) ([]participants.Entry, error)
}

// Deps are the components the web interface works with.
type Deps struct {
	Store  *storage.Store
	Svc    *poll.Service
	Stats  *stats.Service
	People Directory
	Slack  slack.API
	Cfg    *config.Config
	TeamID string
	Log    *slog.Logger
}

// OIDC holds the Slack OpenID Connect endpoints; tests point them to a fake.
type OIDC struct {
	AuthorizeURL string
	TokenURL     string
	Client       *http.Client
}

// SlackOIDC are Slack's endpoints for "Sign in with Slack".
var SlackOIDC = OIDC{
	AuthorizeURL: "https://slack.com/openid/connect/authorize",
	TokenURL:     "https://slack.com/api/openid.connect.token",
	Client:       &http.Client{Timeout: 15 * time.Second},
}

// Server is the web interface.
type Server struct {
	Deps
	OIDC OIDC
	Now  func() time.Time

	pages   map[string]*template.Template
	limiter *limiter
}

// New creates the web interface.
func New(d Deps) (*Server, error) {
	s := &Server{Deps: d, OIDC: SlackOIDC, Now: time.Now, limiter: newLimiter(30, 2*time.Second)}
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) parseTemplates() error {
	files, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return err
	}
	s.pages = map[string]*template.Template{}
	for _, f := range files {
		name := strings.TrimSuffix(strings.TrimPrefix(f, "templates/"), ".html")
		if name == "layout" {
			continue
		}
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", f)
		if err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
		s.pages[name] = t
	}
	return nil
}

// Handler returns the HTTP handler with all routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	mux.HandleFunc("GET /login", s.handleLogin)
	mux.HandleFunc("GET /auth/start", s.limited(s.handleAuthStart))
	mux.HandleFunc("GET /auth/callback", s.limited(s.handleAuthCallback))
	mux.HandleFunc("GET /auth/dev", s.limited(s.handleDevLogin))
	mux.Handle("POST /logout", s.auth(storage.RoleViewer, s.handleLogout))
	mux.HandleFunc("GET /lang", s.handleLang)

	s.routes(mux)
	return s.secure(mux)
}

// routes registers the pages behind the login.
func (s *Server) routes(mux *http.ServeMux) {
	viewer, analyst, admin := storage.RoleViewer, storage.RoleAnalyst, storage.RoleAdmin
	mux.Handle("GET /{$}", s.auth(viewer, s.handleOverview))
	mux.Handle("GET /stats", s.auth(viewer, s.handleStats))
	mux.Handle("GET /stats.csv", s.auth(viewer, s.handleStatsCSV))
	mux.Handle("GET /stats/persons.csv", s.auth(analyst, s.handlePersonsCSV))

	mux.Handle("GET /weeks", s.auth(admin, s.handleWeeks))
	mux.Handle("GET /weeks/go", s.auth(admin, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/weeks/"+url.PathEscape(r.URL.Query().Get("week")), http.StatusSeeOther)
	}))
	mux.Handle("GET /weeks/{week}", s.auth(admin, s.handleWeek))
	mux.Handle("POST /weeks/{week}/attendance", s.auth(admin, s.handleWeekAttendance))
	mux.Handle("POST /weeks/{week}/{action}", s.auth(admin, func(w http.ResponseWriter, r *http.Request) {
		if a := r.PathValue("action"); a != "refresh" && a != "resync" {
			http.NotFound(w, r)
			return
		}
		s.handleWeekRefresh(w, r)
	}))

	mux.Handle("GET /absences", s.auth(admin, s.handleAbsences))
	mux.Handle("POST /absences/add", s.auth(admin, s.handleAbsenceAdd))
	mux.Handle("POST /absences/delete", s.auth(admin, s.handleAbsenceDelete))

	mux.Handle("GET /languages", s.auth(admin, s.handleLanguages))
	mux.Handle("POST /languages/set", s.auth(admin, s.handleLanguageSet))
	mux.Handle("POST /languages/reset", s.auth(admin, s.handleLanguageReset))

	mux.Handle("GET /closed-days", s.auth(admin, s.handleClosedDays))
	mux.Handle("POST /closed-days/add", s.auth(admin, s.handleClosedDayAdd))
	mux.Handle("POST /closed-days/remove", s.auth(admin, s.handleClosedDayRemove))

	mux.Handle("GET /audit", s.auth(admin, s.handleAudit))

	mux.Handle("GET /days", s.auth(admin, s.handleDays))
	mux.Handle("GET /days/go", s.auth(admin, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/days/"+url.PathEscape(r.URL.Query().Get("date")), http.StatusSeeOther)
	}))
	mux.Handle("GET /days/{date}", s.auth(admin, s.handleDay))
	mux.Handle("POST /days/{date}", s.auth(admin, s.handleDaySave))
	mux.Handle("POST /days/{date}/unverify", s.auth(admin, s.handleDayUnverify))

	mux.Handle("GET /retention", s.auth(admin, s.handleRetention))
	mux.Handle("POST /retention/preview", s.auth(admin, s.handlePurgePreview))
	mux.Handle("POST /retention/purge", s.auth(admin, s.handlePurge))

	mux.Handle("GET /access", s.auth(admin, s.handleAccess))
	mux.Handle("POST /access/add", s.auth(admin, s.handleAccessAdd))
	mux.Handle("POST /access/role", s.auth(admin, s.handleAccessRole))
	mux.Handle("POST /access/remove", s.auth(admin, s.handleAccessRemove))
}

// Run serves the web interface until ctx is cancelled and removes expired
// sessions once an hour.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.Cfg.WebListenAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(s.Log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	s.Log.Info("web interface listening", "addr", s.Cfg.WebListenAddr, "url", s.Cfg.WebBaseURL.String())

	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case err := <-errc:
			return fmt.Errorf("web interface: %w", err)
		case <-t.C:
			if n, err := s.Store.DeleteExpiredWebState(ctx, s.Cfg.WebSessionIdle, s.Now()); err != nil {
				s.Log.Warn("delete expired web sessions", "err", err)
			} else if n > 0 {
				s.Log.Debug("deleted expired web sessions", "count", n)
			}
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		}
	}
}

// secure sets the security headers on every response.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store")
		if s.Cfg.WebBaseURL.Scheme == "https" {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// ------------------------------------------------------------------ rendering

// Page is the data every template gets.
type Page struct {
	Title     string
	Nav       string
	Lang      string
	User      *storage.WebUser
	CSRF      string
	Message   string // success message (translated)
	Error     string // error message (translated)
	Data      any
	DevLogin  []storage.WebUser
	PerPerson bool
	Return    string // current page, target of the language switch
}

// T translates a key into the page language.
func (p *Page) T(key string, args ...any) string { return tr(p.Lang, key, args...) }

// IsAdmin reports whether the logged-in person is an admin.
func (p *Page) IsAdmin() bool { return p.User != nil && p.User.Role == storage.RoleAdmin }

// render writes a page. The template is executed into a buffer first, so a
// template error never produces half a page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, p *Page) {
	if v := viewerFrom(r); v != nil && p.User == nil {
		p.User, p.CSRF = &v.User, v.Session.CSRFToken
	}
	if p.Lang == "" {
		p.Lang = s.lang(r)
	}
	if p.Return == "" {
		// After a form submission, the switch leads to the start page: a
		// POST target cannot be fetched with GET.
		p.Return = "/"
		if r.Method == http.MethodGet && r.URL.Path != "/lang" {
			p.Return = safeReturn(r.URL.RequestURI())
		}
	}
	if p.Message == "" {
		if k := r.URL.Query().Get("msg"); k != "" && hasText(p.Lang, "msg."+k) {
			p.Message = p.T("msg." + k)
		}
	}
	if p.Error == "" {
		if k := r.URL.Query().Get("err"); k != "" && hasText(p.Lang, "err."+k) {
			p.Error = p.T("err." + k)
		}
	}
	t, ok := s.pages[name]
	if !ok {
		s.Log.Error("unknown template", "name", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout.html", p); err != nil {
		s.Log.Error("render page", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// fail renders an error page.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, key string, err error) {
	if err != nil {
		s.Log.Error("web request failed", "path", r.URL.Path, "err", err)
	}
	s.render(w, r, status, "error", &Page{Title: tr(s.lang(r), "error.title"), Error: tr(s.lang(r), key)})
}

// redirect answers a form submission (POST/redirect/GET). msg and errKey
// are text keys without their "msg."/"err." prefix.
func redirect(w http.ResponseWriter, r *http.Request, path, msg, errKey string) {
	q := url.Values{}
	if msg != "" {
		q.Set("msg", msg)
	}
	if errKey != "" {
		q.Set("err", errKey)
	}
	if len(q) > 0 {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		path += sep + q.Encode()
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// langCookie stores the language chosen with the switch in the header.
const langCookie = "officebot_lang"

// lang returns the language of the web interface for the request: the one
// chosen with the switch, otherwise English. It is independent of the bot
// language, so everybody gets the same interface until they switch.
func (s *Server) lang(r *http.Request) string {
	if c, err := r.Cookie(langCookie); err == nil && c.Value == "de" {
		return "de"
	}
	return "en"
}

// handleLang switches the interface language and returns to the page. It
// needs no login, so the switch also works on the login page.
func (s *Server) handleLang(w http.ResponseWriter, r *http.Request) {
	lang := r.URL.Query().Get("to")
	if lang != "de" && lang != "en" {
		lang = "en"
	}
	http.SetCookie(w, &http.Cookie{
		Name:     langCookie,
		Value:    lang,
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		Secure:   s.Cfg.WebBaseURL.Scheme == "https",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, safeReturn(r.URL.Query().Get("return")), http.StatusSeeOther)
}
