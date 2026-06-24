package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"recipes/internal/models"
	"recipes/internal/store"
)

var errNoCategory = errors.New("category required")

// parseIngredients builds ingredient blocks from the form. Each block has a
// subtitle field (ing_subtitle) and a newline-separated items textarea
// (ing_items); the two are parallel arrays in document order. Empty blocks
// (no subtitle and no items) are dropped.
func parseIngredients(r *http.Request) []models.IngredientBlock {
	subtitles := r.PostForm["ing_subtitle"]
	itemsRaw := r.PostForm["ing_items"]

	n := len(subtitles)
	if len(itemsRaw) > n {
		n = len(itemsRaw)
	}

	var blocks []models.IngredientBlock
	for i := 0; i < n; i++ {
		var subtitle string
		if i < len(subtitles) {
			subtitle = strings.TrimSpace(subtitles[i])
		}
		var items []string
		if i < len(itemsRaw) {
			for _, line := range strings.Split(itemsRaw[i], "\n") {
				if v := strings.TrimSpace(line); v != "" {
					items = append(items, v)
				}
			}
		}
		if subtitle == "" && len(items) == 0 {
			continue
		}
		blocks = append(blocks, models.IngredientBlock{Subtitle: subtitle, Items: items})
	}
	return blocks
}

// resolveCategory returns the category id for the submitted form, creating a new
// category when the free-text "new_category" field is filled (taking precedence
// over the select). Returns errNoCategory when neither is provided.
func (s *Server) resolveCategory(ctx context.Context, r *http.Request) (int64, error) {
	if newName := strings.TrimSpace(r.PostFormValue("new_category")); newName != "" {
		cat, err := s.store.GetOrCreateCategory(ctx, newName, models.SourceManual)
		if err != nil {
			return 0, err
		}
		return cat.ID, nil
	}

	idStr := strings.TrimSpace(r.PostFormValue("category_id"))
	if idStr == "" {
		return 0, errNoCategory
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, errNoCategory
	}
	// Validate the category exists.
	if _, err := s.store.GetCategory(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return 0, errNoCategory
		}
		return 0, err
	}
	return id, nil
}
