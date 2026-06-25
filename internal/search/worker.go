package search

import (
	"context"
	"log"
	"time"

	"recipes/internal/store"
)

// embedBatch caps how many recipe passages are embedded per request.
const embedBatch = 16

// PassageEmbedder embeds recipe documents for the backfill index. The concrete
// implementation is internal/embed.Client (which also satisfies Embedder).
type PassageEmbedder interface {
	EmbedPassages(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
	Dim() int
}

// Worker keeps the semantic index up to date out-of-band: it embeds recipes that
// lack a current-model vector and refreshes the Service snapshot. Running it as a
// backfill (rather than inline on recipe writes) covers both the web-admin and
// iCloud-import write paths and keeps writes working when the embedder is down.
type Worker struct {
	svc      *Service
	store    *store.Store
	emb      PassageEmbedder
	interval time.Duration
}

// NewWorker creates a backfill worker. interval <= 0 defaults to 5 minutes.
func NewWorker(svc *Service, st *store.Store, emb PassageEmbedder, interval time.Duration) *Worker {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &Worker{svc: svc, store: st, emb: emb, interval: interval}
}

// Run indexes once immediately (so a fresh deploy is searchable quickly) then on
// each tick, until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	log.Printf("search: embedding backfill worker started (every %s, model %s)", w.interval, w.emb.Model())
	w.tick(ctx)
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("search: embedding backfill worker stopped")
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

func (w *Worker) tick(ctx context.Context) {
	w.backfill(ctx)
	// Always refresh so already-stored vectors load even when the embedder is
	// down and no new ones were produced this tick.
	if err := w.svc.RefreshSnapshot(ctx); err != nil {
		log.Printf("search: snapshot refresh: %v", err)
	}
}

// backfill embeds recipes missing a current-model vector. On an embedder error it
// stops and retries next tick (recipes stay lexically searchable meanwhile).
func (w *Worker) backfill(ctx context.Context) {
	model := w.emb.Model()
	ids, err := w.store.RecipeIDsMissingEmbedding(ctx, model)
	if err != nil {
		log.Printf("search: list missing embeddings: %v", err)
		return
	}
	if len(ids) == 0 {
		return
	}
	for i := 0; i < len(ids); i += embedBatch {
		select {
		case <-ctx.Done():
			return
		default:
		}
		end := i + embedBatch
		if end > len(ids) {
			end = len(ids)
		}
		var texts []string
		var have []int64
		for _, id := range ids[i:end] {
			txt, ok, err := w.store.RecipeEmbedInput(ctx, id)
			if err != nil {
				log.Printf("search: embed input for recipe %d: %v", id, err)
				continue
			}
			if ok {
				texts = append(texts, txt)
				have = append(have, id)
			}
		}
		if len(texts) == 0 {
			continue
		}
		vecs, err := w.emb.EmbedPassages(ctx, texts)
		if err != nil {
			log.Printf("search: backfill embed (%d recipes): %v", len(texts), err)
			return // embedder unavailable; retry next tick
		}
		for j, v := range vecs {
			if err := w.store.UpsertEmbedding(ctx, have[j], model, len(v), v); err != nil {
				log.Printf("search: upsert embedding %d: %v", have[j], err)
			}
		}
		log.Printf("search: embedded %d recipe(s)", len(vecs))
	}
}
