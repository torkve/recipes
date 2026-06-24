package notesync

import (
	"context"
	"log"
	"time"

	"recipes/internal/store"
)

// Worker periodically pulls all bound accounts. It is owned by the caller's
// context and returns promptly on cancellation (graceful shutdown).
type Worker struct {
	engine   *Engine
	store    *store.Store
	interval time.Duration
}

// NewWorker creates a pull worker with the given tick interval.
func NewWorker(e *Engine, st *store.Store, interval time.Duration) *Worker {
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	return &Worker{engine: e, store: st, interval: interval}
}

// Run loops until ctx is cancelled, pulling every bound account each tick.
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	log.Printf("notesync: pull worker started (every %s)", w.interval)
	for {
		select {
		case <-ctx.Done():
			log.Printf("notesync: pull worker stopped")
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

func (w *Worker) tick(ctx context.Context) {
	accts, err := w.store.ListICloudAccounts(ctx)
	if err != nil {
		log.Printf("notesync: worker list accounts: %v", err)
		return
	}
	for _, a := range accts {
		if len(a.SessionBlob) == 0 || a.NotesFolder == "" {
			continue // not fully bound yet
		}
		rep, err := w.engine.PullUser(ctx, a.UserID)
		if err != nil {
			log.Printf("notesync: worker pull user %d: %v", a.UserID, err)
			continue
		}
		if rep.Created+rep.Updated+rep.Conflicts > 0 {
			log.Printf("notesync: pulled user %d: created=%d updated=%d conflicts=%d",
				a.UserID, rep.Created, rep.Updated, rep.Conflicts)
		}
	}
}
