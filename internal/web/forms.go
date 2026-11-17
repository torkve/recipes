package web

import (
	"net/http"
	"strings"

	"recipes/internal/models"
)

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
