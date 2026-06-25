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

	t.Run("etag conflict is recorded, not overwritten", func(t *testing.T) {
		eng, st, fp, uid := newTestEngine(t)
		mustBind(t, eng, uid)
		id := newPushedRecipe(t, eng, st, uid, "Окрошка", "<p>Старый текст.</p>")
		rec, _ := st.GetRecipe(ctx, id)
		in := recipeInput("Окрошка", rec.CategoryID, "<p>Новый текст.</p>", uid)
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
		if rep.Conflicts != 1 || rep.Updated != 0 {
			t.Fatalf("conflict push: %+v want Conflicts=1", rep)
		}
		conflicts, _ := eng.Conflicts(ctx)
		if len(conflicts) != 1 || conflicts[0].Kind != models.ConflictBothChanged {
			t.Fatalf("expected both_changed conflict, got %+v", conflicts)
		}
	})
}
