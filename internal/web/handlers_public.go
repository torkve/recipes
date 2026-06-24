package web

import (
	"errors"
	"html/template"
	"net/http"
	"strconv"

	"recipes/internal/sanitize"
	"recipes/internal/store"
)

// handleHome renders the public catalog: all recipes newest-first, with a
// category navigation. An optional ?cat=<id> restricts the list to a category.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	cats, err := s.store.ListCategories(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}

	var selected int64
	var filter *int64
	if c := r.URL.Query().Get("cat"); c != "" {
		if id, err := strconv.ParseInt(c, 10, 64); err == nil && id > 0 {
			selected = id
			filter = &id
		}
	}

	recipes, err := s.store.ListRecipes(ctx, filter, 0, 0)
	if err != nil {
		s.serverError(w, err)
		return
	}

	data := s.newPageData(r)
	data["Title"] = ""
	data["Categories"] = cats
	data["Recipes"] = recipes
	data["SelectedCat"] = selected
	data["Query"] = ""
	s.render(w, r, "home", http.StatusOK, data)
}

// handleRecipeView renders a single recipe page.
func (s *Server) handleRecipeView(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	rec, err := s.store.GetRecipe(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}

	data := s.newPageData(r)
	data["Title"] = rec.Title
	data["Recipe"] = rec
	// Re-sanitize on render as defense in depth (also covers imported data).
	data["StepsHTMLSafe"] = template.HTML(sanitize.StepsHTML(rec.StepsHTML)) //nolint:gosec // sanitized above
	s.render(w, r, "recipe", http.StatusOK, data)
}
