package store

import (
	"context"
	"strings"

	"recipes/internal/models"
)

// buildFTSQuery turns free user input into a safe FTS5 MATCH expression:
// each whitespace-separated token becomes a double-quoted prefix term, joined
// by implicit AND. Quoting (with internal quotes doubled) neutralizes FTS5
// query operators in user input, so arbitrary text cannot cause a syntax error
// or injection. Returns "" when there is nothing to search for.
func buildFTSQuery(input string) string {
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return ""
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		esc := strings.ReplaceAll(f, `"`, `""`)
		parts = append(parts, `"`+esc+`"*`)
	}
	return strings.Join(parts, " ")
}

// SearchRecipes runs a full-text search over title, ingredients and steps,
// optionally restricted to a category, ordered by relevance. An empty/whitespace
// query falls back to the newest-first listing.
func (s *Store) SearchRecipes(ctx context.Context, query string, categoryID *int64) ([]models.Recipe, error) {
	match := buildFTSQuery(query)
	if match == "" {
		return s.ListRecipes(ctx, categoryID, 0, 0)
	}

	q := `
		SELECT r.id, r.title, r.category_id, r.created_at, c.name
		FROM recipes_fts f
		JOIN recipes r ON r.id = f.rowid
		JOIN categories c ON c.id = r.category_id
		WHERE recipes_fts MATCH ?`
	args := []any{match}
	if categoryID != nil {
		q += ` AND r.category_id = ?`
		args = append(args, *categoryID)
	}
	q += ` ORDER BY rank`
	return s.queryRecipeList(ctx, q, args...)
}
