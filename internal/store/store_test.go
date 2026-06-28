package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"recipes/internal/models"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func TestMigrateSeedsBuiltinCategories(t *testing.T) {
	s := newTestStore(t)
	cats, err := s.ListCategories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cats) != len(builtinCategories) {
		t.Fatalf("got %d categories, want %d", len(cats), len(builtinCategories))
	}
	// Re-migrating must be a no-op (no duplicates).
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	cats2, _ := s.ListCategories(context.Background())
	if len(cats2) != len(builtinCategories) {
		t.Fatalf("re-migrate changed category count to %d", len(cats2))
	}
}

func TestEnsureReplicaUUID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// No iCloud binding yet → ErrNotFound.
	if _, err := s.EnsureReplicaUUID(ctx, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("EnsureReplicaUUID without account = %v, want ErrNotFound", err)
	}

	u, err := s.CreateUser(ctx, "alice", "h", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertICloudAccount(ctx, u.ID, "alice@icloud.com"); err != nil {
		t.Fatal(err)
	}

	first, err := s.EnsureReplicaUUID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 16 {
		t.Fatalf("replica uuid len = %d, want 16", len(first))
	}
	// Stable across calls — never a fresh one (that was the propagation bug).
	again, err := s.EnsureReplicaUUID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, again) {
		t.Fatalf("replica uuid changed: %x -> %x", first, again)
	}

	// Idempotent migration must not drop the persisted id.
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	afterMigrate, err := s.EnsureReplicaUUID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, afterMigrate) {
		t.Fatalf("re-migrate changed replica uuid: %x -> %x", first, afterMigrate)
	}
}

func TestEmbeddingsStore(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cat, _ := s.GetOrCreateCategory(ctx, "Супы", models.SourceManual)
	rec, err := s.CreateRecipe(ctx, RecipeInput{
		Title: "Борщ", CategoryID: cat.ID,
		Ingredients: []models.IngredientBlock{{Items: []string{"свёкла"}}},
		StepsHTML:   "<p>Варить.</p>",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Codec round-trip.
	vec := []float32{0.5, -1.25, 3.0, 0}
	if got := decodeVec(encodeVec(vec)); !reflect.DeepEqual(got, vec) {
		t.Fatalf("vec round-trip: got %v want %v", got, vec)
	}

	const model = "test-model"
	// Missing before upsert.
	miss, _ := s.RecipeIDsMissingEmbedding(ctx, model)
	if len(miss) != 1 || miss[0] != rec.ID {
		t.Fatalf("missing before upsert = %v, want [%d]", miss, rec.ID)
	}
	// Upsert, then it's present and loadable with the category.
	if err := s.UpsertEmbedding(ctx, rec.ID, model, len(vec), vec); err != nil {
		t.Fatal(err)
	}
	if miss, _ := s.RecipeIDsMissingEmbedding(ctx, model); len(miss) != 0 {
		t.Fatalf("missing after upsert = %v, want none", miss)
	}
	got, _ := s.EmbeddingsForModel(ctx, model)
	if len(got) != 1 || got[0].ID != rec.ID || got[0].CategoryID != cat.ID || !reflect.DeepEqual(got[0].Vec, vec) {
		t.Fatalf("EmbeddingsForModel = %+v", got)
	}
	// A different (stale) model reports the recipe as missing again.
	if miss, _ := s.RecipeIDsMissingEmbedding(ctx, "other-model"); len(miss) != 1 {
		t.Fatalf("stale-model missing = %v, want 1", miss)
	}
	// RecipeEmbedInput flattens title+ingredients+steps.
	txt, ok, err := s.RecipeEmbedInput(ctx, rec.ID)
	if err != nil || !ok || !strings.Contains(txt, "Борщ") || !strings.Contains(txt, "свёкла") || !strings.Contains(txt, "Варить") {
		t.Fatalf("RecipeEmbedInput = %q ok=%v err=%v", txt, ok, err)
	}
	// Deleting the recipe cascades the embedding away.
	if _, err := s.DeleteRecipe(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.EmbeddingsForModel(ctx, model); len(got) != 0 {
		t.Fatalf("embedding survived recipe delete: %+v", got)
	}
}

func TestSetCategoryParent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	a, _ := s.CreateCategoryWithParent(ctx, "A", nil, models.SourceManual)
	b, _ := s.CreateCategoryWithParent(ctx, "B", nil, models.SourceManual)
	c, _ := s.CreateCategoryWithParent(ctx, "C", nil, models.SourceManual)

	// Set B under A.
	if err := s.SetCategoryParent(ctx, b.ID, &a.ID); err != nil {
		t.Fatalf("set B<-A: %v", err)
	}
	if got, _ := s.GetCategory(ctx, b.ID); got.ParentID == nil || *got.ParentID != a.ID {
		t.Fatalf("B parent = %v, want %d", got.ParentID, a.ID)
	}
	// Set C under B (A->B->C chain).
	if err := s.SetCategoryParent(ctx, c.ID, &b.ID); err != nil {
		t.Fatalf("set C<-B: %v", err)
	}

	// Self-parent is a cycle.
	if err := s.SetCategoryParent(ctx, a.ID, &a.ID); !errors.Is(err, ErrCycle) {
		t.Fatalf("self-parent: got %v, want ErrCycle", err)
	}
	// A under C would close the A->B->C->A cycle.
	if err := s.SetCategoryParent(ctx, a.ID, &c.ID); !errors.Is(err, ErrCycle) {
		t.Fatalf("descendant-parent: got %v, want ErrCycle", err)
	}
	// The rejected writes left A at the top level.
	if got, _ := s.GetCategory(ctx, a.ID); got.ParentID != nil {
		t.Fatalf("A parent changed despite cycle rejection: %v", got.ParentID)
	}

	// Clearing the parent moves B back to the top level.
	if err := s.SetCategoryParent(ctx, b.ID, nil); err != nil {
		t.Fatalf("clear B parent: %v", err)
	}
	if got, _ := s.GetCategory(ctx, b.ID); got.ParentID != nil {
		t.Fatalf("B parent not cleared: %v", got.ParentID)
	}

	// Unknown category / unknown parent.
	if err := s.SetCategoryParent(ctx, 99999, &a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: got %v, want ErrNotFound", err)
	}
	missing := int64(99999)
	if err := s.SetCategoryParent(ctx, a.ID, &missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown parent: got %v, want ErrNotFound", err)
	}
}

func TestGetOrCreateCategoryDedupByNorm(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	a, err := s.GetOrCreateCategory(ctx, "Десерты", models.SourceManual)
	if err != nil {
		t.Fatal(err)
	}
	// Different case / spacing must resolve to the same category.
	b, err := s.GetOrCreateCategory(ctx, "  десерты ", models.SourceManual)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatalf("expected same category, got %d and %d", a.ID, b.ID)
	}
}

func TestCreateRecipeAndFTSSearch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	cat, err := s.GetOrCreateCategory(ctx, "Супы", models.SourceManual)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateRecipe(ctx, RecipeInput{
		Title:      "Борщ украинский",
		CategoryID: cat.ID,
		Ingredients: []models.IngredientBlock{
			{Subtitle: "Основа", Items: []string{"свёкла", "капуста", "картофель"}},
		},
		StepsHTML: "<p>Варить бульон, добавить овощи.</p>",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Search by title token.
	res, err := s.SearchRecipes(ctx, "борщ", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("search by title: got %d results, want 1", len(res))
	}
	// Search by ingredient token (prefix).
	res, err = s.SearchRecipes(ctx, "свёкл", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("search by ingredient: got %d results, want 1", len(res))
	}
	// Non-matching query returns nothing.
	res, err = s.SearchRecipes(ctx, "пельмени", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Fatalf("non-matching search: got %d results, want 0", len(res))
	}
}

func TestDeleteCategoryInUse(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cat, _ := s.GetOrCreateCategory(ctx, "Напитки", models.SourceManual)
	if _, err := s.CreateRecipe(ctx, RecipeInput{Title: "Компот", CategoryID: cat.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteCategory(ctx, cat.ID); err != ErrCategoryInUse {
		t.Fatalf("expected ErrCategoryInUse, got %v", err)
	}
}
