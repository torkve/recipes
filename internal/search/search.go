// Package search provides recipe search: lexical (SQLite FTS) always, plus an
// optional semantic (embedding) arm fused with Reciprocal Rank Fusion. It owns
// the HTTP-talking embedding logic so the store stays pure SQL.
package search

import (
	"context"
	"log"
	"math"
	"sort"
	"sync/atomic"

	"recipes/internal/models"
	"recipes/internal/store"
)

// rrfK is the Reciprocal Rank Fusion constant; semTopK caps how many semantic
// neighbours feed the fusion.
const (
	rrfK    = 60.0
	semTopK = 50
)

// Embedder produces a query embedding and identifies its model/dimension. The
// concrete implementation is internal/embed.Client; search defines the interface
// it consumes so it can be faked in tests and disabled (nil) entirely.
type Embedder interface {
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
	Model() string
	Dim() int
}

// snapshot is an immutable in-memory copy of the semantic index for one model,
// swapped atomically by RefreshSnapshot so the query path does no SQL.
type snapshot struct {
	model   string
	entries []store.RecipeVector
}

// Service runs queries against the store, optionally enriched by semantic search.
type Service struct {
	store    *store.Store
	emb      Embedder // nil => lexical-only
	minScore float64  // minimum cosine similarity for a semantic hit
	snap     atomic.Pointer[snapshot]
}

// New builds a Service. Pass a nil Embedder (not a nil *embed.Client wrapped in
// the interface) to run lexical-only. minScore is the minimum cosine similarity a
// semantic hit must clear (0 keeps all scored hits).
func New(st *store.Store, emb Embedder, minScore float64) *Service {
	return &Service{store: st, emb: emb, minScore: minScore}
}

// RefreshSnapshot reloads the semantic index for the configured model into
// memory. No-op when semantic search is disabled.
func (s *Service) RefreshSnapshot(ctx context.Context) error {
	if s.emb == nil {
		return nil
	}
	entries, err := s.store.EmbeddingsForModel(ctx, s.emb.Model())
	if err != nil {
		return err
	}
	s.snap.Store(&snapshot{model: s.emb.Model(), entries: entries})
	return nil
}

// Search returns recipes for a query, optionally restricted to a category
// subtree. An empty query browses (newest-first). When the semantic arm is
// available it is fused with the lexical (FTS) results via RRF; any semantic
// failure degrades gracefully to lexical-only.
func (s *Service) Search(ctx context.Context, query string, categoryIDs []int64) ([]models.Recipe, error) {
	if query == "" {
		return s.store.ListRecipes(ctx, categoryIDs, 0, 0)
	}
	lexical, err := s.store.SearchRecipes(ctx, query, categoryIDs)
	if err != nil {
		return nil, err
	}
	semantic := s.semanticIDs(ctx, query, categoryIDs)
	if len(semantic) == 0 {
		return lexical, nil
	}
	return s.fuse(ctx, lexical, semantic)
}

// semanticIDs returns recipe ids ranked by embedding cosine similarity to the
// query, restricted to categoryIDs when given. Returns nil (→ lexical-only) when
// semantic search is disabled, the snapshot is empty, or the embedder errors.
func (s *Service) semanticIDs(ctx context.Context, query string, categoryIDs []int64) []int64 {
	if s.emb == nil {
		return nil
	}
	snap := s.snap.Load()
	if snap == nil || len(snap.entries) == 0 {
		return nil
	}
	qv, err := s.emb.EmbedQuery(ctx, query)
	if err != nil {
		log.Printf("search: query embed failed, falling back to lexical: %v", err)
		return nil
	}

	var allow map[int64]bool
	if len(categoryIDs) > 0 {
		allow = make(map[int64]bool, len(categoryIDs))
		for _, id := range categoryIDs {
			allow[id] = true
		}
	}

	type scored struct {
		id    int64
		score float64
	}
	var hits []scored
	for _, e := range snap.entries {
		if allow != nil && !allow[e.CategoryID] {
			continue
		}
		score := cosine(qv, e.Vec)
		if score < s.minScore {
			continue // not similar enough to count as a semantic match
		}
		hits = append(hits, scored{e.ID, score})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].id < hits[j].id
	})
	if len(hits) > semTopK {
		hits = hits[:semTopK]
	}
	ids := make([]int64, len(hits))
	for i, h := range hits {
		ids[i] = h.id
	}
	return ids
}

// fuse merges the lexical recipe list and the semantic id ranking with RRF and
// returns recipes in fused order, fetching any semantic-only recipes.
func (s *Service) fuse(ctx context.Context, lexical []models.Recipe, semantic []int64) ([]models.Recipe, error) {
	rf := map[int64]float64{} // id -> reciprocal-rank-fusion score
	for i, r := range lexical {
		rf[r.ID] += 1.0 / (rrfK + float64(i+1))
	}
	for i, id := range semantic {
		rf[id] += 1.0 / (rrfK + float64(i+1))
	}

	byID := make(map[int64]models.Recipe, len(lexical))
	for _, r := range lexical {
		byID[r.ID] = r
	}
	// Materialize semantic-only recipes not already in the lexical results.
	var missing []int64
	for id := range rf {
		if _, ok := byID[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		extra, err := s.store.RecipesByIDs(ctx, missing)
		if err != nil {
			return nil, err
		}
		for _, r := range extra {
			byID[r.ID] = r
		}
	}

	ids := make([]int64, 0, len(rf))
	for id := range rf {
		if _, ok := byID[id]; ok { // drop ids whose recipe vanished (stale snapshot)
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		if rf[ids[i]] != rf[ids[j]] {
			return rf[ids[i]] > rf[ids[j]]
		}
		return ids[i] < ids[j]
	})
	out := make([]models.Recipe, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return out, nil
}

// cosine returns the cosine similarity of two equal-length vectors (0 when a
// length/zero-norm issue makes it undefined).
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
