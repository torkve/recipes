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

	"recipes/internal/auth"
	"recipes/internal/config"
	"recipes/internal/icloud"
	"recipes/internal/notesync"
	"recipes/internal/store"
	"recipes/internal/web"
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

	// Open and migrate the database.
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()

	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelSetup()

	if err := st.Migrate(setupCtx); err != nil {
		return err
	}

	// Bootstrap the first admin account from the environment, if configured.
	if err := auth.BootstrapAdmin(setupCtx, st, cfg.AdminUsername, cfg.AdminPassword); err != nil {
		return err
	}

	// Load (or generate and persist) session/CSRF secret keys.
	keys, err := auth.LoadOrCreateKeys(cfg.KeysPath())
	if err != nil {
		return err
	}

	// Build the iCloud sync engine when enabled (default off): the
	// reverse-engineered iCloud client ships dark until configured.
	var engine *notesync.Engine
	if cfg.ICloudEnabled {
		provider := icloud.New(nil, cfg.ICloudSRPVariant)
		engine, err = notesync.NewEngine(st, provider, provider, keys.SyncEnc, cfg.UploadsDir())
		if err != nil {
			return err
		}
		log.Printf("recipes: iCloud sync enabled")
	}

	srvHandler, err := web.NewServer(cfg, st, keys, engine)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srvHandler.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM. NotifyContext stops the notifier
	// when the context is cancelled, so the goroutine never leaks even if the
	// server fails to start.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start the background pull worker (stops on ctx cancellation).
	if engine != nil {
		worker := notesync.NewWorker(engine, st, time.Duration(cfg.ICloudPullMinutes)*time.Minute)
		go worker.Run(ctx)
	}

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
