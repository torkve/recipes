// Package web implements the HTTP layer: routing, templates, sessions, CSRF
// protection and the public + admin handlers.
package web

import (
	"embed"
	"html/template"
	"io/fs"
	"log"
	"net/http"

	"github.com/gorilla/csrf"
	"github.com/gorilla/sessions"

	"recipes/internal/auth"
	"recipes/internal/config"
	"recipes/internal/store"
)

//go:embed static
var staticFS embed.FS

// Server holds the dependencies shared by all handlers.
type Server struct {
	cfg       *config.Config
	store     *store.Store
	sessions  *sessions.FilesystemStore
	templates map[string]*template.Template
	handler   http.Handler
}

// NewServer wires templates, session store, CSRF protection and routes.
func NewServer(cfg *config.Config, st *store.Store, keys *auth.Keys) (*Server, error) {
	tmpls, err := loadTemplates(templateFuncs())
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:       cfg,
		store:     st,
		sessions:  newSessionStore(cfg.SessionsDir(), keys.SessionAuth, keys.SessionEnc, cfg.SecureCookies),
		templates: tmpls,
	}

	csrfMW := csrf.Protect(
		keys.CSRF,
		csrf.Secure(cfg.SecureCookies),
		csrf.Path("/"),
		csrf.SameSite(csrf.SameSiteLaxMode),
		csrf.FieldName(csrfFieldName),
	)

	s.handler = logging(s.withUser(s.markPlaintext(csrfMW(s.routes()))))
	return s, nil
}

// markPlaintext flags requests as plaintext HTTP for gorilla/csrf when secure
// cookies are disabled (local/dev over plain HTTP). Without it, csrf enforces a
// strict Referer check intended for TLS, which rejects ordinary form posts.
// In production (SecureCookies=true) requests are left unmarked so the full
// TLS-grade Origin/Referer checks apply.
func (s *Server) markPlaintext(next http.Handler) http.Handler {
	if s.cfg.SecureCookies {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, csrf.PlaintextHTTPRequest(r))
	})
}

// Handler returns the fully-wrapped HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Health.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})

	// Public.
	mux.HandleFunc("GET /{$}", s.handleHome)

	// Auth.
	mux.HandleFunc("GET /admin/login", s.handleLoginForm)
	mux.HandleFunc("POST /admin/login", s.handleLoginSubmit)
	mux.HandleFunc("POST /admin/logout", s.requireAuth(s.handleLogout))

	// Embedded static assets.
	sub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))

	// Uploaded images (served from disk under the data dir).
	mux.Handle("GET /uploads/", http.StripPrefix("/uploads/", http.FileServer(http.Dir(s.cfg.UploadsDir()))))

	return mux
}

// logging is a minimal request logger middleware.
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		log.Printf("%s %s", r.Method, r.URL.Path)
	})
}
