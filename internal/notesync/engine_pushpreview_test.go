package notesync

import (
	"context"
	"strings"
	"testing"

	"recipes/internal/models"
)

// diffText joins a diff into "+/-/= " prefixed lines for easy assertions.
func diffText(item PushItem) string {
	var b strings.Builder
	for _, l := range item.Diff {
		b.WriteString(l.Sign())
		b.WriteString(l.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

func TestPlanPushDiffCreate(t *testing.T) {
	ctx := context.Background()
	eng, st, _, uid := newTestEngine(t)
	mustBind(t, eng, uid)

	cat, _ := st.GetOrCreateCategory(ctx, "Салаты", models.SourceManual)
	if _, err := st.CreateRecipe(ctx, recipeInput("Винегрет", cat.ID, "<p>Нарезать.</p>", uid)); err != nil {
		t.Fatal(err)
	}

	prev, err := eng.PlanPushDiff(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(prev.Creates()) != 1 || len(prev.Updates()) != 0 || prev.Skipped != 0 {
		t.Fatalf("plan = %+v, want 1 create", prev)
	}
	item := prev.Creates()[0]
	if item.Op != "create" || item.Title != "Винегрет" {
		t.Fatalf("bad create item: %+v", item)
	}
	// A create has no remote side: every diff line is an addition.
	for _, l := range item.Diff {
		if l.Op != diffAdd {
			t.Fatalf("create diff has non-add line %q (%d)", l.Text, l.Op)
		}
	}
	if !strings.Contains(diffText(item), "+Шаги:") || !strings.Contains(diffText(item), "+Нарезать.") {
		t.Fatalf("create diff missing step content:\n%s", diffText(item))
	}
}

func TestPlanPushDiffUpdateShowsChange(t *testing.T) {
	ctx := context.Background()
	eng, st, fp, uid := newTestEngine(t)
	mustBind(t, eng, uid)

	id := newPushedRecipe(t, eng, st, uid, "Плов", "<p>Шаг один.</p>")
	rec, _ := st.GetRecipe(ctx, id)

	// Simulate the live remote note carrying the originally-pushed content.
	fp.notes = []Note{{
		ID: NoteID(*rec.ICloudNoteID), FolderID: "root", Etag: Etag(*rec.ICloudEtag),
		Title: "Плов", Checklists: [][]string{{"соль"}}, BodyHTML: "<p>Шаг один.</p>",
	}}

	// Local edit to the steps.
	in := recipeInput("Плов", rec.CategoryID, "<p>Шаг один и два.</p>", uid)
	in.ICloudNoteID = rec.ICloudNoteID
	in.ICloudEtag = rec.ICloudEtag
	if err := st.UpdateRecipe(ctx, id, in); err != nil {
		t.Fatal(err)
	}

	prev, err := eng.PlanPushDiff(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(prev.Updates()) != 1 || len(prev.Creates()) != 0 {
		t.Fatalf("plan = %+v, want 1 update", prev)
	}
	item := prev.Updates()[0]
	if item.Note != "" {
		t.Fatalf("unexpected flag on update with present remote: %q", item.Note)
	}
	txt := diffText(item)
	if !strings.Contains(txt, "-Шаг один.") || !strings.Contains(txt, "+Шаг один и два.") {
		t.Fatalf("update diff does not show the step change:\n%s", txt)
	}
}

func TestPlanPushDiffUpdateMissingRemote(t *testing.T) {
	ctx := context.Background()
	eng, st, fp, uid := newTestEngine(t)
	mustBind(t, eng, uid)

	id := newPushedRecipe(t, eng, st, uid, "Окрошка", "<p>Старый.</p>")
	rec, _ := st.GetRecipe(ctx, id)
	fp.notes = nil // remote note not returned by the zone scan

	in := recipeInput("Окрошка", rec.CategoryID, "<p>Новый.</p>", uid)
	in.ICloudNoteID = rec.ICloudNoteID
	in.ICloudEtag = rec.ICloudEtag
	if err := st.UpdateRecipe(ctx, id, in); err != nil {
		t.Fatal(err)
	}

	prev, err := eng.PlanPushDiff(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(prev.Updates()) != 1 {
		t.Fatalf("plan = %+v, want 1 update", prev)
	}
	item := prev.Updates()[0]
	if item.Note != "будет создана заново" {
		t.Fatalf("missing-remote flag = %q", item.Note)
	}
	for _, l := range item.Diff {
		if l.Op != diffAdd {
			t.Fatalf("missing-remote diff should be all-add, got %q (%d)", l.Text, l.Op)
		}
	}
}

func TestPlanPushDiffSkipsUnchanged(t *testing.T) {
	ctx := context.Background()
	eng, st, fp, uid := newTestEngine(t)
	mustBind(t, eng, uid)

	id := newPushedRecipe(t, eng, st, uid, "Борщ", "<p>Варить.</p>")
	rec, _ := st.GetRecipe(ctx, id)
	fp.notes = []Note{{ID: NoteID(*rec.ICloudNoteID), Title: "Борщ",
		Checklists: [][]string{{"соль"}}, BodyHTML: "<p>Варить.</p>"}}

	prev, err := eng.PlanPushDiff(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if prev.HasChanges() || prev.Skipped != 1 {
		t.Fatalf("plan = %+v, want no changes and Skipped=1", prev)
	}
}

// diffHas reports whether the diff contains a line with the given op and text.
func diffHas(item PushItem, op DiffOp, text string) bool {
	for _, l := range item.Diff {
		if l.Op == op && l.Text == text {
			return true
		}
	}
	return false
}

// Adding an inline image surfaces the recipe as a change with a "+ изображение" line.
func TestPlanPushDiffImageAdded(t *testing.T) {
	ctx := context.Background()
	eng, st, fp, uid := newTestEngine(t)
	mustBind(t, eng, uid)

	id := newPushedRecipe(t, eng, st, uid, "Сырники", "<p>Жарить.</p>")
	rec, _ := st.GetRecipe(ctx, id)
	fp.notes = []Note{{ID: NoteID(*rec.ICloudNoteID), Title: "Сырники",
		Checklists: [][]string{{"соль"}}, BodyHTML: "<p>Жарить.</p>"}}

	in := recipeInput("Сырники", rec.CategoryID, `<p>Жарить.</p><img src="/uploads/pic.png">`, uid)
	in.ICloudNoteID = rec.ICloudNoteID
	in.ICloudEtag = rec.ICloudEtag
	if err := st.UpdateRecipe(ctx, id, in); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddImage(ctx, id, "pic.png", "image/png"); err != nil {
		t.Fatal(err)
	}

	prev, err := eng.PlanPushDiff(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(prev.Updates()) != 1 {
		t.Fatalf("adding an image should surface one update, got %+v", prev)
	}
	if !diffHas(prev.Updates()[0], diffAdd, "изображение") {
		t.Fatalf("expected a '+ изображение' line, got %+v", prev.Updates()[0].Diff)
	}
}

// Removing an image shows a "− изображение" line (remote has it, local doesn't).
func TestPlanPushDiffImageRemoved(t *testing.T) {
	ctx := context.Background()
	eng, st, fp, uid := newTestEngine(t)
	mustBind(t, eng, uid)

	id := newPushedRecipe(t, eng, st, uid, "Сырники", "<p>Жарить.</p>")
	rec, _ := st.GetRecipe(ctx, id)
	// Remote note carries an image; the recipe has none. A text edit makes it an update.
	fp.notes = []Note{{ID: NoteID(*rec.ICloudNoteID), Title: "Сырники",
		Checklists: [][]string{{"соль"}}, BodyHTML: "<p>Жарить.</p>",
		Images: []NoteImage{{ID: "ATT-1"}}}}
	in := recipeInput("Сырники", rec.CategoryID, "<p>Жарить быстро.</p>", uid)
	in.ICloudNoteID = rec.ICloudNoteID
	in.ICloudEtag = rec.ICloudEtag
	if err := st.UpdateRecipe(ctx, id, in); err != nil {
		t.Fatal(err)
	}

	prev, err := eng.PlanPushDiff(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(prev.Updates()) != 1 {
		t.Fatalf("want one update, got %+v", prev)
	}
	if !diffHas(prev.Updates()[0], diffDel, "изображение") {
		t.Fatalf("expected a '− изображение' line, got %+v", prev.Updates()[0].Diff)
	}
}
