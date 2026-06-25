// Package web implements the HTTP layer: routing, templates, sessions, CSRF
// protection and the public + admin handlers.
package web

import (
	"embed"
	"html/template"
	"io/fs"
	"log"
	"net/http"

	"github.com/gorilla/sessions"

	"recipes/internal/auth"
	"recipes/internal/config"
	"recipes/internal/notesync"
	"recipes/internal/search"
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
	engine    *notesync.Engine // nil when iCloud sync is disabled
	search    *search.Service
	handler   http.Handler
}

// NewServer wires templates, session store, CSRF protection and routes. engine
// may be nil, in which case the iCloud sync routes report the feature is off.
func NewServer(cfg *config.Config, st *store.Store, keys *auth.Keys, engine *notesync.Engine, searchSvc *search.Service) (*Server, error) {
	tmpls, err := loadTemplates(templateFuncs())
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:       cfg,
		store:     st,
		sessions:  newSessionStore(cfg.SessionsDir(), keys.SessionAuth, keys.SessionEnc, cfg.SecureCookies),
		templates: tmpls,
		engine:    engine,
		search:    searchSvc,
	}

	// CSRF protection via the stdlib: deny unsafe (non-GET/HEAD/OPTIONS)
	// cross-origin requests using Sec-Fetch-Site / Origin. It needs no token, no
	// TLS, and no per-request scheme inspection, so it works the same on plain
	// HTTP (local) and behind a TLS-terminating proxy (which preserves Host). The
	// session cookie's SameSite=Lax is the residual defense for header-less
	// requests. Placed outside withUser so denied requests skip the session load.
	cop := http.NewCrossOriginProtection()
	s.handler = logging(cop.Handler(s.withUser(s.routes())))
	return s, nil
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
	mux.HandleFunc("GET /recipes/{id}", s.handleRecipeView)

	// Auth.
	mux.HandleFunc("GET /admin/login", s.handleLoginForm)
	mux.HandleFunc("POST /admin/login", s.handleLoginSubmit)
	mux.HandleFunc("POST /admin/logout", s.requireAuth(s.handleLogout))

	// Admin: recipe management (all behind auth).
	mux.HandleFunc("GET /admin", s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/recipes", http.StatusSeeOther)
	}))
	mux.HandleFunc("GET /admin/recipes", s.requireAuth(s.handleAdminRecipes))
	mux.HandleFunc("GET /admin/recipes/new", s.requireAuth(s.handleRecipeNew))
	mux.HandleFunc("POST /admin/recipes", s.requireAuth(s.handleRecipeCreate))
	mux.HandleFunc("POST /admin/recipes/upload", s.requireAuth(s.handleUpload))
	mux.HandleFunc("GET /admin/recipes/{id}/edit", s.requireAuth(s.handleRecipeEditForm))
	mux.HandleFunc("POST /admin/recipes/{id}", s.requireAuth(s.handleRecipeUpdate))
	mux.HandleFunc("POST /admin/recipes/{id}/delete", s.requireAuth(s.handleRecipeDelete))

	// Admin: category (reference) management.
	mux.HandleFunc("GET /admin/categories", s.requireAuth(s.handleAdminCategories))
	mux.HandleFunc("POST /admin/categories/{id}/rename", s.requireAuth(s.handleCategoryRename))
	mux.HandleFunc("POST /admin/categories/{id}/parent", s.requireAuth(s.handleCategorySetParent))
	mux.HandleFunc("POST /admin/categories/{id}/delete", s.requireAuth(s.handleCategoryDelete))

	// Admin: iCloud sync (handlers report 404 when the feature is disabled).
	mux.HandleFunc("GET /admin/sync", s.requireAuth(s.handleSyncStatus))
	mux.HandleFunc("POST /admin/sync/bind", s.requireAuth(s.handleSyncBind))
	mux.HandleFunc("POST /admin/sync/bind/2fa", s.requireAuth(s.handleSyncBind2FA))
	mux.HandleFunc("POST /admin/sync/bind/cancel", s.requireAuth(s.handleSyncCancel))
	mux.HandleFunc("POST /admin/sync/folder", s.requireAuth(s.handleSyncSetFolder))
	mux.HandleFunc("POST /admin/sync/pull", s.requireAuth(s.handleSyncPull))
	mux.HandleFunc("POST /admin/sync/push", s.requireAuth(s.handleSyncPush))
	mux.HandleFunc("POST /admin/sync/conflicts/{id}/resolve", s.requireAuth(s.handleSyncResolve))
	mux.HandleFunc("POST /admin/sync/unbind", s.requireAuth(s.handleSyncUnbind))

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
