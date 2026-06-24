package store

import (
	"context"
	"path/filepath"
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
