package web

import (
	"errors"
	"net/http"
	"strings"

	"recipes/internal/auth"
	"recipes/internal/store"
)

// handleLoginForm shows the admin login form. Logged-in users are redirected home.
func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if currentUser(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	data := s.newPageData(r)
	data["Title"] = "Вход"
	data["Username"] = ""
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

	renderErr := func() {
		data := s.newPageData(r)
		data["Title"] = "Вход"
		data["Error"] = "Неверный логин или пароль"
		data["Username"] = username
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
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLogout clears the session.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.logout(w, r); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
