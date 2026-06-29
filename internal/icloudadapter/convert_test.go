package icloudadapter

import (
	"reflect"
	"testing"

	"recipes/internal/notesync"
)

// The recipe projection must round-trip: ingredient blocks (including "# subtitle"
// headers) and step lines (including inline-image markers) survive toParagraphs
// followed by fromParagraphs unchanged.
func TestRecipeProjectionRoundTrip(t *testing.T) {
	cases := []struct {
		name       string
		checklists [][]string
		steps      string
	}{
		{"plain", [][]string{{"мука", "соль"}}, "шаг один\nшаг два"},
		{"subtitles", [][]string{{"# Тесто", "мука"}, {"# Крем", "сахар"}}, "испечь"},
		{"inline image on its own line", [][]string{{"яйцо"}}, "шаг один\n@@IMG:pic.png@@\nшаг два"},
		{"no ingredients", nil, "просто шаг"},
		{"no steps", [][]string{{"вода"}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			blocks, steps := fromParagraphs(toParagraphs(c.checklists, c.steps))
			if !reflect.DeepEqual(blocks, c.checklists) {
				t.Errorf("checklists = %v, want %v", blocks, c.checklists)
			}
			if steps != c.steps {
				t.Errorf("steps = %q, want %q", steps, c.steps)
			}
		})
	}
}

// A full note round-trips through toNote/fromNote: identity fields, title, the
// recipe projection, and images.
func TestNoteRoundTrip(t *testing.T) {
	in := notesync.Note{
		ID:         "N1",
		FolderID:   "F1",
		Etag:       "e1",
		Title:      "Песочное печенье",
		Checklists: [][]string{{"250 г муки"}},
		BodyHTML:   "замесить\n@@IMG:pic.png@@\nиспечь",
		Images:     []notesync.NoteImage{{ID: "pic.png", Ref: "https://x/y", ContentType: "image/png"}},
		RawBody:    []byte{1, 2, 3},
	}
	got := fromNote(toNote(in))
	if got.ID != in.ID || got.FolderID != in.FolderID || got.Etag != in.Etag || got.Title != in.Title {
		t.Fatalf("identity/title mismatch: %+v", got)
	}
	if !reflect.DeepEqual(got.Checklists, in.Checklists) || got.BodyHTML != in.BodyHTML {
		t.Fatalf("projection mismatch: checklists=%v body=%q", got.Checklists, got.BodyHTML)
	}
	if !reflect.DeepEqual(got.Images, in.Images) {
		t.Fatalf("images mismatch: %+v", got.Images)
	}
	if !reflect.DeepEqual(got.RawBody, in.RawBody) {
		t.Fatalf("raw body not preserved")
	}
}
