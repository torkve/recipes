package notesync

import (
	"context"
	"testing"

	"recipes/internal/models"
	"recipes/internal/store"
)

// recipeInput builds a minimal RecipeInput for the given owner.
func recipeInput(title string, catID int64, steps string, uid int64) store.RecipeInput {
	return store.RecipeInput{
		Title:       title,
		CategoryID:  catID,
		Ingredients: []models.IngredientBlock{{Items: []string{"соль"}}},
		StepsHTML:   steps,
		OwnerID:     &uid,
	}
}

// newPushedRecipe creates a recipe and pushes it once so it is linked to a note
// with a recorded sync state, returning the recipe id.
func newPushedRecipe(t *testing.T, eng *Engine, st *store.Store, uid int64, title, steps string) int64 {
	t.Helper()
	ctx := context.Background()
	cat, _ := st.GetOrCreateCategory(ctx, "Блюда", models.SourceManual)
	rec, err := st.CreateRecipe(ctx, recipeInput(title, cat.ID, steps, uid))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.PushUser(ctx, uid); err != nil {
		t.Fatal(err)
	}
	return rec.ID
}

// TestPushClassification characterizes PushUser's create/update/skip/conflict
// decisions so the later classifier extraction stays behavior-preserving.
func TestPushClassification(t *testing.T) {
	ctx := context.Background()

	t.Run("new recipe is created", func(t *testing.T) {
		eng, st, fp, uid := newTestEngine(t)
		mustBind(t, eng, uid)
		newPushedRecipe(t, eng, st, uid, "Винегрет", "<p>Нарезать.</p>")
		if len(fp.pushed) != 1 {
			t.Fatalf("expected note pushed, got %d", len(fp.pushed))
		}
	})

	t.Run("linked unchanged recipe is skipped", func(t *testing.T) {
		eng, st, fp, uid := newTestEngine(t)
		mustBind(t, eng, uid)
		newPushedRecipe(t, eng, st, uid, "Борщ", "<p>Варить.</p>")
		before := fp.pushCount
		rep, err := eng.PushUser(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Skipped != 1 || rep.Created != 0 || rep.Updated != 0 {
			t.Fatalf("unchanged push: %+v want Skipped=1", rep)
		}
		if fp.pushCount != before {
			t.Fatalf("skip still pushed a note: %d -> %d", before, fp.pushCount)
		}
	})

	t.Run("linked changed recipe is updated", func(t *testing.T) {
		eng, st, fp, uid := newTestEngine(t)
		mustBind(t, eng, uid)
		id := newPushedRecipe(t, eng, st, uid, "Плов", "<p>Шаг один.</p>")
		rec, _ := st.GetRecipe(ctx, id)
		in := recipeInput("Плов", rec.CategoryID, "<p>Шаг один и два.</p>", uid)
		in.ICloudNoteID = rec.ICloudNoteID
		in.ICloudEtag = rec.ICloudEtag
		if err := st.UpdateRecipe(ctx, id, in); err != nil {
			t.Fatal(err)
		}
		before := fp.pushCount
		rep, err := eng.PushUser(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Updated != 1 || rep.Created != 0 || rep.Skipped != 0 {
			t.Fatalf("changed push: %+v want Updated=1", rep)
		}
		if fp.pushCount != before+1 {
			t.Fatalf("update did not push: %d -> %d", before, fp.pushCount)
		}
	})

	t.Run("genuine remote change is recorded as a conflict", func(t *testing.T) {
		eng, st, fp, uid := newTestEngine(t)
		mustBind(t, eng, uid)
		id := newPushedRecipe(t, eng, st, uid, "Окрошка", "<p>Старый текст.</p>")
		rec, _ := st.GetRecipe(ctx, id)
		// The note changed on another device since our last sync...
		fp.notes = []Note{{ID: NoteID(*rec.ICloudNoteID), Etag: "live", Title: "Окрошка",
			Checklists: [][]string{{"соль"}}, BodyHTML: "<p>Изменено в iCloud.</p>"}}
		// ...and locally too.
		in := recipeInput("Окрошка", rec.CategoryID, "<p>Новый текст.</p>", uid)
		in.ICloudNoteID = rec.ICloudNoteID
		in.ICloudEtag = rec.ICloudEtag
		if err := st.UpdateRecipe(ctx, id, in); err != nil {
			t.Fatal(err)
		}
		rep, err := eng.PushUser(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Conflicts != 1 || rep.Updated != 0 {
			t.Fatalf("conflict push: %+v want Conflicts=1", rep)
		}
		conflicts, _ := eng.Conflicts(ctx)
		if len(conflicts) != 1 || conflicts[0].Kind != models.ConflictBothChanged {
			t.Fatalf("expected both_changed conflict, got %+v", conflicts)
		}
	})

	// Regression: a stale stored etag with UNCHANGED remote content must replace, not
	// falsely conflict (the bug this work fixes).
	t.Run("unchanged remote replaces without conflict", func(t *testing.T) {
		eng, st, fp, uid := newTestEngine(t)
		mustBind(t, eng, uid)
		id := newPushedRecipe(t, eng, st, uid, "Борщ", "<p>Варить.</p>")
		rec, _ := st.GetRecipe(ctx, id)
		fp.notes = []Note{{ID: NoteID(*rec.ICloudNoteID), Etag: "drifted", Title: "Борщ",
			Checklists: [][]string{{"соль"}}, BodyHTML: "<p>Варить.</p>"}}
		in := recipeInput("Борщ", rec.CategoryID, "<p>Варить два часа.</p>", uid)
		in.ICloudNoteID = rec.ICloudNoteID
		in.ICloudEtag = rec.ICloudEtag
		if err := st.UpdateRecipe(ctx, id, in); err != nil {
			t.Fatal(err)
		}
		rep, err := eng.PushUser(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Conflicts != 0 || rep.Updated != 1 {
			t.Fatalf("unchanged-remote push: %+v want Updated=1 Conflicts=0", rep)
		}
	})

	t.Run("vanished remote note is recreated, not a conflict", func(t *testing.T) {
		eng, st, fp, uid := newTestEngine(t)
		mustBind(t, eng, uid)
		id := newPushedRecipe(t, eng, st, uid, "Плов", "<p>Шаг.</p>")
		rec, _ := st.GetRecipe(ctx, id)
		fp.notes = nil // the linked note is gone from iCloud
		in := recipeInput("Плов", rec.CategoryID, "<p>Шаг два.</p>", uid)
		in.ICloudNoteID = rec.ICloudNoteID
		in.ICloudEtag = rec.ICloudEtag
		if err := st.UpdateRecipe(ctx, id, in); err != nil {
			t.Fatal(err)
		}
		before := fp.pushCount
		rep, err := eng.PushUser(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Conflicts != 0 || fp.pushCount != before+1 {
			t.Fatalf("vanished push: %+v pushCount %d->%d, want recreate + no conflict", rep, before, fp.pushCount)
		}
	})

	t.Run("etag conflict during replace is recorded (TOCTOU backstop)", func(t *testing.T) {
		eng, st, fp, uid := newTestEngine(t)
		mustBind(t, eng, uid)
		id := newPushedRecipe(t, eng, st, uid, "Суп", "<p>Старо.</p>")
		rec, _ := st.GetRecipe(ctx, id)
		// Remote present and unchanged (hash gate passes), but the delete races a
		// remote edit and PushNote reports an etag conflict.
		fp.notes = []Note{{ID: NoteID(*rec.ICloudNoteID), Etag: "e", Title: "Суп",
			Checklists: [][]string{{"соль"}}, BodyHTML: "<p>Старо.</p>"}}
		in := recipeInput("Суп", rec.CategoryID, "<p>Ново.</p>", uid)
		in.ICloudNoteID = rec.ICloudNoteID
		in.ICloudEtag = rec.ICloudEtag
		if err := st.UpdateRecipe(ctx, id, in); err != nil {
			t.Fatal(err)
		}
		fp.pushConflict = true
		rep, err := eng.PushUser(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Conflicts != 1 {
			t.Fatalf("TOCTOU push: %+v want Conflicts=1", rep)
		}
	})
}
