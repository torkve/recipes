package store

import (
	"html"
	"regexp"
	"strings"

	"recipes/internal/models"
)

// PlainTextHTML strips tags/entities from HTML, yielding text for hashing and
// indexing. Exported for the sync layer's conflict fingerprinting.
func PlainTextHTML(s string) string { return htmlToText(s) }

// NormalizeName produces the uniqueness key for a category name: trimmed,
// internal whitespace collapsed to single spaces, and lowercased. This makes
// "Супы", " супы " and "СУПЫ" collide so duplicates are rejected.
func NormalizeName(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

var tagRE = regexp.MustCompile(`<[^>]*>`)

// htmlToText strips HTML tags and unescapes entities, yielding plain text
// suitable for full-text indexing of recipe steps.
func htmlToText(s string) string {
	s = tagRE.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

// ingredientsToText flattens ingredient blocks into a single searchable string.
func ingredientsToText(blocks []models.IngredientBlock) string {
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Subtitle != "" {
			b.WriteString(blk.Subtitle)
			b.WriteByte(' ')
		}
		for _, it := range blk.Items {
			b.WriteString(it)
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
