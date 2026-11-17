package web

import (
	"errors"
	"net/http"
	"strings"

	"recipes/internal/auth"
	"recipes/internal/store"
)

// handleLoginForm shows the admin login form. Logged-in users are redirected to
// the ?next= path, or home.
func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	denyFraming(w)
	next := safeNext(r.URL.Query().Get("next"))
	if currentUser(r) != nil {
		http.Redirect(w, r, orHome(next), http.StatusSeeOther)
		return
	}
	data := s.newPageData(r)
	data["Title"] = "Вход"
	data["Username"] = ""
	data["Next"] = next
	s.render(w, r, "login", http.StatusOK, data)
}

// handleLoginSubmit verifies credentials and starts a session.
func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	next := safeNext(r.PostFormValue("next"))

	renderErr := func() {
		denyFraming(w)
		data := s.newPageData(r)
		data["Title"] = "Вход"
		data["Error"] = "Неверный логин или пароль"
		data["Username"] = username
		data["Next"] = next
		s.render(w, r, "login", http.StatusUnauthorized, data)
	}

	user, err := s.store.GetUserByUsername(r.Context(), username)
	if errors.Is(err, store.ErrNotFound) {
		// Run a dummy hash check to keep timing roughly uniform.
		auth.CheckPassword("$2a$12$0000000000000000000000000000000000000000000000000000u", password)
		renderErr()
		return
	} else if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if !auth.CheckPassword(user.PasswordHash, password) {
		renderErr()
		return
	}

	if err := s.login(w, r, user.ID); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, orHome(next), http.StatusSeeOther)
}

// orHome returns path, or "/" when it is empty.
func orHome(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

// handleLogout clears the session.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.logout(w, r); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
