package store

import (
	"context"
	"fmt"
	"time"

	"recipes/internal/models"
)

// schema is the full database schema. Every statement is idempotent
// (IF NOT EXISTS), so applying it on every boot both creates a fresh database
// and is a no-op on an existing one.
const schema = `
CREATE TABLE IF NOT EXISTS users (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  is_admin      INTEGER NOT NULL DEFAULT 0,
  created_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS categories (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  name       TEXT NOT NULL,
  name_norm  TEXT NOT NULL UNIQUE,
  parent_id  INTEGER REFERENCES categories(id) ON DELETE SET NULL,
  source     TEXT NOT NULL DEFAULT 'manual',
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS recipes (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  title            TEXT NOT NULL,
  category_id      INTEGER NOT NULL REFERENCES categories(id) ON DELETE RESTRICT,
  ingredients_json TEXT NOT NULL DEFAULT '[]',
  steps_html       TEXT NOT NULL DEFAULT '',
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL,
  icloud_note_id   TEXT,
  icloud_etag      TEXT,
  owner_id         INTEGER REFERENCES users(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_recipes_category ON recipes(category_id);
CREATE INDEX IF NOT EXISTS idx_recipes_created ON recipes(created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_recipes_note
  ON recipes(icloud_note_id) WHERE icloud_note_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS recipe_images (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  recipe_id    INTEGER NOT NULL REFERENCES recipes(id) ON DELETE CASCADE,
  filename     TEXT NOT NULL,
  content_type TEXT NOT NULL,
  created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_recipe_images_recipe ON recipe_images(recipe_id);

-- Full-text index over recipe title, ingredients and steps (plain text).
-- Maintained explicitly from Go inside the same transaction as recipe writes;
-- rowid equals the recipe id.
CREATE VIRTUAL TABLE IF NOT EXISTS recipes_fts USING fts5(
  title, ingredients, steps,
  tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TABLE IF NOT EXISTS icloud_accounts (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id      INTEGER NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
  apple_id     TEXT NOT NULL,
  session_blob BLOB,
  notes_folder TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sync_state (
  recipe_id      INTEGER PRIMARY KEY REFERENCES recipes(id) ON DELETE CASCADE,
  note_id        TEXT NOT NULL,
  last_synced_at TEXT NOT NULL,
  local_hash     TEXT NOT NULL DEFAULT '',
  remote_hash    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS sync_conflicts (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  recipe_id  INTEGER REFERENCES recipes(id) ON DELETE CASCADE,
  note_id    TEXT,
  kind       TEXT NOT NULL,
  detail     TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  resolved   INTEGER NOT NULL DEFAULT 0
);
`

// builtinCategories are the default categories seeded on first migration.
var builtinCategories = []string{
	"Супы",
	"Салаты",
	"Второе",
	"Выпечка",
	"Соусы и намазки",
	"Напитки",
}

// Migrate creates the schema (idempotently) and seeds the builtin categories.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("store: migrate schema: %w", err)
	}
	if err := s.seedCategories(ctx); err != nil {
		return fmt.Errorf("store: seed categories: %w", err)
	}
	return nil
}

func (s *Store) seedCategories(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339)
	for _, name := range builtinCategories {
		// INSERT OR IGNORE: the UNIQUE(name_norm) constraint makes re-seeding a
		// no-op and avoids clobbering a user-renamed category.
		_, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO categories (name, name_norm, parent_id, source, created_at)
			 VALUES (?, ?, NULL, ?, ?)`,
			name, NormalizeName(name), models.SourceBuiltin, now)
		if err != nil {
			return err
		}
	}
	return nil
}
