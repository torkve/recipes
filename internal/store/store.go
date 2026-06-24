// Package store is the SQLite persistence layer: schema migrations and CRUD
// for recipes, categories, users, images and iCloud sync state.
//
// It uses the pure-Go modernc.org/sqlite driver (no CGO) so the service builds
// as a static binary and the whole database is a single file under the data dir.
package store

import (
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Store wraps the database handle and exposes typed query methods.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the sqlite database at path and applies
// connection pragmas. The schema is created/upgraded by Migrate.
func Open(path string) (*Store, error) {
	// WAL for concurrent readers during writes; foreign_keys for referential
	// integrity; busy_timeout so brief lock contention waits instead of failing.
	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(on)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// modernc/sqlite is safe for concurrent use, but a single writer avoids
	// SQLITE_BUSY churn; readers still proceed under WAL.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{db: db}, nil
}

// DB exposes the underlying handle (used by tests and session storage).
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }
