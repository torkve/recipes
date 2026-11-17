package web

import (
	"context"
	"net/http"
	"net/url"

	"recipes/internal/models"
)

type ctxKey int

const userCtxKey ctxKey = iota

// withUser loads the current user (if logged in) and stores it in the request
// context so handlers and templates can tell guests from members.
func (s *Server) withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := s.sessionUserID(r); ok {
			if u, err := s.store.GetUser(r.Context(), id); err == nil {
				r = r.WithContext(context.WithValue(r.Context(), userCtxKey, u))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// currentUser returns the authenticated user from the request context, or nil.
func currentUser(r *http.Request) *models.User {
	u, _ := r.Context().Value(userCtxKey).(*models.User)
	return u
}

// requireAuth blocks unauthenticated access to admin handlers, redirecting to login.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if currentUser(r) == nil {
			target := "/admin/login"
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				target += "?next=" + url.QueryEscape(r.URL.RequestURI())
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}
