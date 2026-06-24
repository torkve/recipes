package web

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"path/filepath"
	"strings"
)

//go:embed templates
var templatesFS embed.FS

const baseTemplate = "templates/base.html"

// loadTemplates parses each page template together with the shared base layout
// and any partials (files whose base name starts with "_"), returning a map
// keyed by page name (file name without extension).
func loadTemplates(fm template.FuncMap) (map[string]*template.Template, error) {
	entries, err := fs.Glob(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}

	var partials []string
	for _, e := range entries {
		if strings.HasPrefix(filepath.Base(e), "_") {
			partials = append(partials, e)
		}
	}

	pages := map[string]*template.Template{}
	for _, e := range entries {
		bn := filepath.Base(e)
		if e == baseTemplate || strings.HasPrefix(bn, "_") {
			continue
		}
		files := append([]string{baseTemplate, e}, partials...)
		t, err := template.New("base").Funcs(fm).ParseFS(templatesFS, files...)
		if err != nil {
			return nil, fmt.Errorf("web: parse template %s: %w", e, err)
		}
		pages[strings.TrimSuffix(bn, ".html")] = t
	}
	return pages, nil
}
