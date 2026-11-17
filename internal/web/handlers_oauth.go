package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"recipes/internal/oauth"
	"recipes/internal/store"
)

// scopeLabels describes OAuth scopes to the user on the consent and
// connections pages.
var scopeLabels = map[string]string{
	oauth.ScopeRead:  "Просматривать и искать рецепты и категории",
	oauth.ScopeWrite: "Создавать и изменять рецепты от вашего имени",
}

func labelScopes(scopes []string) []string {
	out := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		if l, ok := scopeLabels[sc]; ok {
			out = append(out, l)
		} else {
			out = append(out, sc)
		}
	}
	return out
}

// denyFraming forbids rendering the response inside a frame, so an approval
// button cannot be overlaid by another site (clickjacking).
func denyFraming(w http.ResponseWriter) {
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
}

// handleOAuthAuthorize shows the consent page for an OAuth authorization
// request (GET /oauth/authorize).
func (s *Server) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	denyFraming(w)
	if s.oauth == nil {
		http.NotFound(w, r)
		return
	}
	req, ok := s.parseAuthorize(w, r, r.URL.Query())
	if !ok {
		return
	}
	redirectHost := req.RedirectURI
	if u, err := url.Parse(req.RedirectURI); err == nil {
		redirectHost = u.Host
	}
	name := req.ClientName
	if name == "" {
		name = "Без названия"
	}
	data := s.newPageData(r)
	data["Title"] = "Доступ к книге"
	data["ClientName"] = name
	data["RedirectHost"] = redirectHost
	data["ScopeLabels"] = labelScopes(req.Scopes())
	data["Params"] = req.Params()
	s.render(w, r, "oauth_consent", http.StatusOK, data)
}

// handleOAuthDecision records the user's answer to the consent page
// (POST /oauth/authorize) and redirects back to the client.
func (s *Server) handleOAuthDecision(w http.ResponseWriter, r *http.Request) {
	denyFraming(w)
	if s.oauth == nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	req, ok := s.parseAuthorize(w, r, r.PostForm)
	if !ok {
		return
	}
	if r.PostForm.Get("decision") != "allow" {
		http.Redirect(w, r, s.oauth.Deny(req), http.StatusSeeOther)
		return
	}
	target, err := s.oauth.Approve(r.Context(), req, currentUser(r).ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// parseAuthorize validates authorization request parameters, handling every
// failure itself: protocol errors redirect to the client, untrusted clients get
// an error page. ok is false when a response was written.
func (s *Server) parseAuthorize(w http.ResponseWriter, r *http.Request, params url.Values) (*oauth.AuthorizeRequest, bool) {
	req, err := s.oauth.ParseAuthorize(r.Context(), params)
	if err == nil {
		return req, true
	}
	var re *oauth.RedirectError
	switch {
	case errors.As(err, &re):
		http.Redirect(w, r, re.URL, http.StatusSeeOther)
	case errors.Is(err, oauth.ErrBadClient):
		data := s.newPageData(r)
		data["Title"] = "Доступ к книге"
		data["Error"] = "Некорректный запрос авторизации: приложение не зарегистрировано или адрес возврата не совпадает."
		s.render(w, r, "oauth_consent", http.StatusBadRequest, data)
	default:
		s.serverError(w, err)
	}
	return nil, false
}

// handleConnections lists the applications the current user has authorized.
func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil {
		http.NotFound(w, r)
		return
	}
	grants, err := s.store.ListOAuthGrants(r.Context(), currentUser(r).ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	type row struct {
		ID         int64
		ClientName string
		Scopes     []string
		CreatedAt  string
		LastUsedAt string
	}
	rows := make([]row, 0, len(grants))
	for _, g := range grants {
		name := g.ClientName
		if name == "" {
			name = "Без названия"
		}
		rows = append(rows, row{
			ID:         g.ID,
			ClientName: name,
			Scopes:     labelScopes(strings.Fields(g.Scope)),
			CreatedAt:  g.CreatedAt.Local().Format("02.01.2006 15:04"),
			LastUsedAt: g.LastUsedAt.Local().Format("02.01.2006 15:04"),
		})
	}
	data := s.newPageData(r)
	data["Title"] = "Подключённые приложения"
	data["Grants"] = rows
	data["MCPURL"] = s.oauth.Resource()
	s.render(w, r, "admin_connections", http.StatusOK, data)
}

// handleConnectionRevoke revokes one of the current user's grants.
func (s *Server) handleConnectionRevoke(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	err = s.store.DeleteOAuthGrant(r.Context(), id, currentUser(r).ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/connections", http.StatusSeeOther)
}

// safeNext returns next when it is a same-site absolute path, else "".
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return ""
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return ""
	}
	return next
}
