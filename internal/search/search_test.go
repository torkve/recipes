package search

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"recipes/internal/models"
	"recipes/internal/store"
)

// fakeEmbedder returns a fixed query vector (or an error), and counts calls.
type fakeEmbedder struct {
	vec   []float32
	err   error
	calls int
}

func (f *fakeEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.vec, nil
}
func (f *fakeEmbedder) Model() string { return "fake" }
func (f *fakeEmbedder) Dim() int      { return len(f.vec) }

// EmbedPassages lets the fake double as a PassageEmbedder for the worker; every
// passage gets the same vector (enough to exercise backfill bookkeeping).
func (f *fakeEmbedder) EmbedPassages(ctx context.Context, texts []string) ([][]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = f.vec
	}
	return out, nil
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

// seed creates a recipe in category cat with the given embedding vector.
func seed(t *testing.T, st *store.Store, title string, catID int64, model string, vec []float32) int64 {
	t.Helper()
	ctx := context.Background()
	r, err := st.CreateRecipe(ctx, store.RecipeInput{
		Title: title, CategoryID: catID,
		Ingredients: []models.IngredientBlock{{Items: []string{"x"}}},
		StepsHTML:   "<p>x</p>",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertEmbedding(ctx, r.ID, model, len(vec), vec); err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func titles(rs []models.Recipe) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Title
	}
	return out
}
func contains(rs []models.Recipe, title string) bool {
	for _, r := range rs {
		if r.Title == title {
			return true
		}
	}
	return false
}

func TestSearchHybridSurfacesSemanticNeighbour(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	cat, _ := st.GetOrCreateCategory(ctx, "Завтрак", models.SourceManual)
	seed(t, st, "Блины", cat.ID, "fake", []float32{1, 0, 0})
	seed(t, st, "Оладьи", cat.ID, "fake", []float32{0.9, 0.1, 0}) // semantically near, no lexical overlap
	seed(t, st, "Борщ", cat.ID, "fake", []float32{0, 0, 1})       // far

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	svc := New(st, emb, 0)
	if err := svc.RefreshSnapshot(ctx); err != nil {
		t.Fatal(err)
	}

	res, err := svc.Search(ctx, "блины", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Lexical alone finds only "Блины"; the semantic arm must surface "Оладьи".
	if !contains(res, "Оладьи") {
		t.Fatalf("semantic neighbour not surfaced: %v", titles(res))
	}
	if len(res) == 0 || res[0].Title != "Блины" {
		t.Fatalf("expected Блины first, got %v", titles(res))
	}
	if !contains(res, "Борщ") {
		t.Errorf("all snapshot entries should rank; missing Борщ: %v", titles(res))
	}
}

func TestSearchThresholdDropsWeakHits(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	cat, _ := st.GetOrCreateCategory(ctx, "Завтрак", models.SourceManual)
	seed(t, st, "Блины", cat.ID, "fake", []float32{1, 0, 0})
	seed(t, st, "Оладьи", cat.ID, "fake", []float32{0.9, 0.1, 0}) // cosine ≈ 0.994 to the query
	seed(t, st, "Борщ", cat.ID, "fake", []float32{0, 0, 1})       // cosine 0 — unrelated

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	svc := New(st, emb, 0.5) // gate out weak matches
	if err := svc.RefreshSnapshot(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Search(ctx, "блины", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The strong neighbour passes the gate; the unrelated recipe is dropped.
	if !contains(res, "Оладьи") {
		t.Errorf("strong semantic neighbour should survive the threshold: %v", titles(res))
	}
	if contains(res, "Борщ") {
		t.Errorf("below-threshold hit should be dropped: %v", titles(res))
	}
}

func TestSearchEmptyQueryBypassesSemantic(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	cat, _ := st.GetOrCreateCategory(ctx, "Завтрак", models.SourceManual)
	seed(t, st, "Блины", cat.ID, "fake", []float32{1, 0, 0})
	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	svc := New(st, emb, 0)
	_ = svc.RefreshSnapshot(ctx)

	res, err := svc.Search(ctx, "", nil) // browse
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("browse returned %d, want 1", len(res))
	}
	if emb.calls != 0 {
		t.Fatalf("empty query must not embed (calls=%d)", emb.calls)
	}
}

func TestSearchEmbedErrorFallsBackToLexical(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	cat, _ := st.GetOrCreateCategory(ctx, "Завтрак", models.SourceManual)
	seed(t, st, "Блины", cat.ID, "fake", []float32{1, 0, 0})
	seed(t, st, "Оладьи", cat.ID, "fake", []float32{1, 0, 0})
	emb := &fakeEmbedder{err: errors.New("down")}
	svc := New(st, emb, 0)
	_ = svc.RefreshSnapshot(ctx)

	res, err := svc.Search(ctx, "блины", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Embedder down → lexical-only: only "Блины", never the semantic-only "Оладьи".
	if contains(res, "Оладьи") || !contains(res, "Блины") {
		t.Fatalf("expected lexical-only [Блины], got %v", titles(res))
	}
}

func TestWorkerBackfillIndexesAndRefreshes(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	cat, _ := st.GetOrCreateCategory(ctx, "Завтрак", models.SourceManual)
	// Two recipes with NO embeddings yet.
	for _, title := range []string{"Блины", "Оладьи"} {
		if _, err := st.CreateRecipe(ctx, store.RecipeInput{
			Title: title, CategoryID: cat.ID,
			Ingredients: []models.IngredientBlock{{Items: []string{"x"}}},
			StepsHTML:   "<p>x</p>",
		}); err != nil {
			t.Fatal(err)
		}
	}
	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	svc := New(st, emb, 0)
	w := NewWorker(svc, st, emb, time.Minute)

	w.tick(ctx) // one backfill pass + snapshot refresh

	if miss, _ := st.RecipeIDsMissingEmbedding(ctx, emb.Model()); len(miss) != 0 {
		t.Fatalf("backfill left %d recipes unindexed", len(miss))
	}
	if snap := svc.snap.Load(); snap == nil || len(snap.entries) != 2 {
		t.Fatalf("snapshot not refreshed after backfill: %+v", snap)
	}

	// Cancelled context: a tick must not index.
	st2 := newStore(t)
	cat2, _ := st2.GetOrCreateCategory(ctx, "Супы", models.SourceManual)
	if _, err := st2.CreateRecipe(ctx, store.RecipeInput{
		Title: "Борщ", CategoryID: cat2.ID,
		Ingredients: []models.IngredientBlock{{Items: []string{"x"}}}, StepsHTML: "<p>x</p>",
	}); err != nil {
		t.Fatal(err)
	}
	emb2 := &fakeEmbedder{vec: []float32{1, 0, 0}}
	w2 := NewWorker(New(st2, emb2, 0), st2, emb2, time.Minute)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	w2.backfill(cancelled)
	if miss, _ := st2.RecipeIDsMissingEmbedding(ctx, emb2.Model()); len(miss) != 1 {
		t.Fatalf("cancelled backfill should index nothing, missing=%d", len(miss))
	}
}

func TestSearchSemanticRespectsCategoryFilter(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	breakfast, _ := st.GetOrCreateCategory(ctx, "Завтрак", models.SourceManual)
	soups, _ := st.GetOrCreateCategory(ctx, "Супы", models.SourceManual)
	seed(t, st, "Оладьи", breakfast.ID, "fake", []float32{1, 0, 0})
	seed(t, st, "Похлёбка", soups.ID, "fake", []float32{1, 0, 0}) // identical vector, different category
	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	svc := New(st, emb, 0)
	_ = svc.RefreshSnapshot(ctx)

	// Restrict to the breakfast category: the soup must not appear via the
	// semantic arm even though its vector matches.
	res, err := svc.Search(ctx, "блины", []int64{breakfast.ID})
	if err != nil {
		t.Fatal(err)
	}
	if contains(res, "Похлёбка") {
		t.Fatalf("category filter not applied to semantic arm: %v", titles(res))
	}
}
