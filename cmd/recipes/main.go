// Command recipes runs the family cookbook web service.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"recipes/internal/config"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("recipes: %v", err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Ensure runtime directories exist.
	for _, dir := range []string{cfg.DataDir, cfg.UploadsDir(), cfg.SessionsDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM. NotifyContext stops the notifier
	// when the context is cancelled, so the goroutine never leaks even if the
	// server fails to start.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	idleClosed := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("recipes: graceful shutdown failed: %v", err)
		}
		close(idleClosed)
	}()

	log.Printf("recipes: listening on %s (data dir %s)", cfg.Addr, cfg.DataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-idleClosed
	return nil
}
