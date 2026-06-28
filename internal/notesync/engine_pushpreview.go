package notesync

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"recipes/internal/models"
	"recipes/internal/store"
)

// PushItem is one recipe a push would create or overwrite, with a line diff of
// its content against the current iCloud note.
type PushItem struct {
	RecipeID     int64
	Title        string
	Op           string // "create" | "update"
	Note         string // optional flag, e.g. "будет создана заново"
	LocalImages  int    // image count on the app side (context only)
	RemoteImages int    // image count on the iCloud side (context only)
	Diff         []DiffLine
}

// PushPreview is the read-only plan of an outbound sync: the notes that would be
// created/overwritten (with diffs) and how many recipes are already up to date.
// Push never deletes notes, so there is no deletion section.
type PushPreview struct {
	Items   []PushItem
	Skipped int
}

// HasChanges reports whether the push would create or overwrite anything.
func (p PushPreview) HasChanges() bool { return len(p.Items) > 0 }

// Creates returns the items that would create a new note.
func (p PushPreview) Creates() []PushItem { return p.filter("create") }

// Updates returns the items that would overwrite an existing note.
func (p PushPreview) Updates() []PushItem { return p.filter("update") }

func (p PushPreview) filter(op string) []PushItem {
	var out []PushItem
	for _, it := range p.Items {
		if it.Op == op {
			out = append(out, it)
		}
	}
	return out
}

// blockBreakRE matches block-level boundaries in step HTML so the projection can
// turn each paragraph/list item into its own diff line (PlainTextHTML alone
// collapses the whole body to one line).
var blockBreakRE = regexp.MustCompile(`(?i)</(p|div|li|h[1-6]|tr|ul|ol|blockquote)>|<br\s*/?>`)

// stepLines renders step HTML to plain text, one line per block, with image
// markers and empty lines dropped (images are excluded from the diff per the
// same rule as the conflict fingerprint).
func stepLines(html string) []string {
	withBreaks := blockBreakRE.ReplaceAllString(html, "\n")
	var lines []string
	for _, seg := range strings.Split(withBreaks, "\n") {
		t := store.PlainTextHTML(imgMarkerRE.ReplaceAllString(seg, " "))
		t = strings.TrimSpace(t)
		if t != "" {
			lines = append(lines, t)
		}
	}
	return lines
}

// stepLinesKeepMarkers is like stepLines but preserves @@IMG@@ markers; the push path
// uses it so inline images survive into the note body (they're placed by the provider).
func stepLinesKeepMarkers(html string) []string {
	withBreaks := blockBreakRE.ReplaceAllString(html, "\n")
	var lines []string
	for _, seg := range strings.Split(withBreaks, "\n") {
		if t := strings.TrimSpace(store.PlainTextHTML(seg)); t != "" {
			lines = append(lines, t)
		}
	}
	return lines
}

// projectForDiff renders a recipe/note into a canonical, human-readable set of
// lines for diffing. Both sides go through the same projection (and the same
// ingredient model via ChecklistsToIngredients), so unchanged content yields
// identical lines. Images are intentionally excluded — only their counts are
// surfaced separately, as context.
func projectForDiff(title string, ings []models.IngredientBlock, stepsHTML string) []string {
	lines := []string{"Название: " + strings.TrimSpace(title), "Ингредиенты:"}
	for _, blk := range ings {
		if s := strings.TrimSpace(blk.Subtitle); s != "" {
			lines = append(lines, "  ["+s+"]")
		}
		for _, it := range blk.Items {
			if v := strings.TrimSpace(it); v != "" {
				lines = append(lines, "  • "+v)
			}
		}
	}
	lines = append(lines, "Шаги:")
	return append(lines, stepLines(stepsHTML)...)
}

// PlanPushDiff computes, without performing any writes, what PushUser would do:
// the notes it would create or overwrite, each with a content diff against the
// current iCloud note, plus a count of recipes already in sync. It uses the same
// classifier as PushUser so the preview can never disagree with the executor.
func (e *Engine) PlanPushDiff(ctx context.Context, userID int64) (PushPreview, error) {
	l := e.userLock(userID)
	l.Lock()
	defer l.Unlock()

	var prev PushPreview
	acct, err := e.store.GetICloudAccount(ctx, userID)
	if err != nil {
		return prev, err
	}
	// restoreSession may persist a refreshed session blob even on this read-only
	// preview; that is pre-existing behavior shared with Pull, not new here.
	sess, err := e.restoreSession(ctx, acct)
	if err != nil {
		return prev, err
	}
	root := FolderID(acct.NotesFolder)

	// One zone scan yields every remote note; index by id for diffing updates.
	_, notes, _, err := e.provider.FetchZone(ctx, sess, root, "")
	if err != nil {
		return prev, err
	}
	remoteByID := make(map[NoteID]Note, len(notes))
	for _, n := range notes {
		remoteByID[n.ID] = n
	}

	ids, err := e.store.ListRecipeIDsByOwner(ctx, userID)
	if err != nil {
		return prev, err
	}
	for _, id := range ids {
		rec, err := e.store.GetRecipe(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return prev, err
		}

		op, err := e.classifyPush(ctx, rec)
		if err != nil {
			return prev, err
		}
		if op == pushSkip {
			prev.Skipped++
			continue
		}

		item := PushItem{RecipeID: rec.ID, Title: rec.Title, LocalImages: len(rec.Images)}
		local := projectForDiff(rec.Title, rec.Ingredients, rec.StepsHTML)
		var remote []string
		if op == pushCreate {
			item.Op = "create"
		} else {
			item.Op = "update"
			// ICloudNoteID is non-nil here (classifyPush returns create when nil).
			if n, ok := remoteByID[NoteID(*rec.ICloudNoteID)]; ok {
				remote = projectForDiff(n.Title, ChecklistsToIngredients(n.Checklists), n.BodyHTML)
				item.RemoteImages = len(n.Images)
			} else {
				// Linked note not under the bound folder (or deleted remotely):
				// push will recreate it, so show the content as all-new.
				item.Note = "будет создана заново"
			}
		}
		item.Diff = LineDiff(remote, local)
		// Images are excluded from the text projection, so surface the count delta as
		// explicit +/- diff lines (one per added/removed image).
		for k := item.RemoteImages; k < item.LocalImages; k++ {
			item.Diff = append(item.Diff, DiffLine{Op: diffAdd, Text: "изображение"})
		}
		for k := item.LocalImages; k < item.RemoteImages; k++ {
			item.Diff = append(item.Diff, DiffLine{Op: diffDel, Text: "изображение"})
		}
		prev.Items = append(prev.Items, item)
	}
	return prev, nil
}
