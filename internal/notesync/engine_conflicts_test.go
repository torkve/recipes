package notesync

import (
	"context"
	"strings"
	"testing"
)

// ConflictsDetailed enriches a recipe conflict with the title and a remote-vs-local
// diff (remote on "-", local on "+").
func TestConflictsDetailed(t *testing.T) {
	ctx := context.Background()
	eng, st, fp, uid := newTestEngine(t)
	mustBind(t, eng, uid)

	id := newPushedRecipe(t, eng, st, uid, "Окрошка", "<p>Старо.</p>")
	rec, _ := st.GetRecipe(ctx, id)
	// Both sides diverge → a real conflict is recorded by the push.
	fp.notes = []Note{{ID: NoteID(*rec.ICloudNoteID), Etag: "live", Title: "Окрошка",
		Checklists: [][]string{{"соль"}}, BodyHTML: "<p>Версия из iCloud.</p>"}}
	in := recipeInput("Окрошка", rec.CategoryID, "<p>Версия в приложении.</p>", uid)
	in.ICloudNoteID = rec.ICloudNoteID
	in.ICloudEtag = rec.ICloudEtag
	if err := st.UpdateRecipe(ctx, id, in); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.PushUser(ctx, uid); err != nil {
		t.Fatal(err)
	}

	views, err := eng.ConflictsDetailed(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("want 1 conflict view, got %d", len(views))
	}
	v := views[0]
	if v.Title != "Окрошка" {
		t.Fatalf("title = %q, want Окрошка", v.Title)
	}
	var hasAdd, hasDel bool
	for _, l := range v.Diff {
		if l.Op == diffAdd && strings.Contains(l.Text, "Версия в приложении") {
			hasAdd = true
		}
		if l.Op == diffDel && strings.Contains(l.Text, "Версия из iCloud") {
			hasDel = true
		}
	}
	if !hasAdd || !hasDel {
		t.Fatalf("diff should show remote removed + local added, got %+v", v.Diff)
	}
}
