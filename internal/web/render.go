package web

import (
	"bytes"
	"html/template"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/csrf"
)

// csrfFieldName is the form field gorilla/csrf expects the token in.
const csrfFieldName = "csrf_token"

// templateFuncs are helpers available inside all templates.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"fmtDate": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.Local().Format("02.01.2006")
		},
	}
}

// pageData is the template payload; render seeds common fields.
type pageData map[string]any

// newPageData builds the base template payload shared by every page.
func (s *Server) newPageData(r *http.Request) pageData {
	return pageData{
		"SiteName":  s.cfg.SiteName,
		"User":      currentUser(r),
		"CSRFField": csrf.TemplateField(r),
		"Path":      r.URL.Path,
	}
}

// render executes a page template (wrapped in the base layout) into a buffer
// first, so a template error never produces a half-written response.
func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, status int, data pageData) {
	t, ok := s.templates[page]
	if !ok {
		log.Printf("web: unknown template %q", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", data); err != nil {
		log.Printf("web: render %s: %v", page, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}
