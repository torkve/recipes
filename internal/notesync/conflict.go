package notesync

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"

	"recipes/internal/models"
	"recipes/internal/store"
)

// imgMarkerRE matches the inline-image placeholder the iCloud parser puts in a
// note body. It is stripped from the fingerprint so an imaged note and its
// imported recipe (which carries a re-hosted <img> tag instead) hash equal.
var imgMarkerRE = regexp.MustCompile(`@@IMG:[^@]*@@`)

// imgMarker is the body placeholder for the image with the given id (the iCloud
// attachment record name), matching what the iCloud note parser emits.
func imgMarker(id string) string { return "@@IMG:" + id + "@@" }

// Decision is the outcome of comparing a recipe and its linked note against the
// last-synced base.
type Decision int

const (
	DecisionNoOp        Decision = iota // neither side changed
	DecisionApplyRemote                 // only the note changed -> import
	DecisionApplyLocal                  // only the recipe changed -> push
	DecisionConflict                    // both changed -> manual resolution
)

// Classify is the pure three-way decision over whether each side changed since
// the common base.
func Classify(localChanged, remoteChanged bool) Decision {
	switch {
	case !localChanged && !remoteChanged:
		return DecisionNoOp
	case !localChanged && remoteChanged:
		return DecisionApplyRemote
	case localChanged && !remoteChanged:
		return DecisionApplyLocal
	default:
		return DecisionConflict
	}
}

// fingerprint hashes a normalized projection of a recipe/note so that
// HTML-cosmetic differences (tags, whitespace) do not create phantom conflicts.
// Images are intentionally excluded: a note's image bytes and a recipe's
// re-hosted /uploads/ filename would never match and would force false conflicts.
// fingerprint projects a recipe/note to a stable hash. Image bytes/markers are
// excluded from the text projection (a note's image bytes and a recipe's re-hosted
// <img> never match), but the image COUNT is folded in so adding/removing an image is
// a detected change. The count is appended only when > 0, so image-less recipes/notes
// hash exactly as before this was added — no mass reclassification of existing sync
// state. (A same-count image replacement is not detected; that's an accepted limit.)
func fingerprint(title string, ings []models.IngredientBlock, plainSteps string, imageCount int) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(title))
	b.WriteByte('\n')
	for _, blk := range ings {
		b.WriteString(strings.TrimSpace(blk.Subtitle))
		b.WriteByte('\x1f')
		for _, it := range blk.Items {
			b.WriteString(strings.TrimSpace(it))
			b.WriteByte('\x1e')
		}
		b.WriteByte('\n')
	}
	b.WriteString(strings.Join(strings.Fields(imgMarkerRE.ReplaceAllString(plainSteps, "")), " "))
	if imageCount > 0 {
		b.WriteByte('\x1d')
		b.WriteString(strconv.Itoa(imageCount))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// HashRecipe fingerprints the app-side recipe. Inline images are counted from the
// <img src="/uploads/…"> tags that get pushed as note attachments.
func HashRecipe(r *models.Recipe) string {
	return fingerprint(r.Title, r.Ingredients, store.PlainTextHTML(r.StepsHTML),
		len(imgSrcRE.FindAllString(r.StepsHTML, -1)))
}

// HashNote fingerprints the backend note using the same projection as HashRecipe, so
// the two are directly comparable. The image count is the note's inline markers.
func HashNote(n Note) string {
	return fingerprint(n.Title, ChecklistsToIngredients(n.Checklists), store.PlainTextHTML(n.BodyHTML),
		len(n.Images))
}
