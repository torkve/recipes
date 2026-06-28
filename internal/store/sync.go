package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"time"

	"recipes/internal/models"
)

// --- iCloud accounts --------------------------------------------------------

func scanICloudAccount(row interface{ Scan(...any) error }) (*models.ICloudAccount, error) {
	var a models.ICloudAccount
	var blob []byte
	var created string
	if err := row.Scan(&a.ID, &a.UserID, &a.AppleID, &blob, &a.NotesFolder, &created); err != nil {
		return nil, err
	}
	a.SessionBlob = blob
	a.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return &a, nil
}

// EnsureReplicaUUID returns the user's stable 16-byte Apple Notes CRDT replica id,
// minting and persisting one on first use. It identifies this app as a single
// replica across all in-place note updates so the version-vector counters advance
// monotonically (a fresh id each push is exactly what made updates not propagate).
// Returns ErrNotFound if the user has no iCloud binding.
func (s *Store) EnsureReplicaUUID(ctx context.Context, userID int64) ([]byte, error) {
	var u []byte
	row := s.db.QueryRowContext(ctx, `SELECT replica_uuid FROM icloud_accounts WHERE user_id = ?`, userID)
	if err := row.Scan(&u); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if len(u) == 16 {
		return u, nil
	}
	u = make([]byte, 16)
	if _, err := rand.Read(u); err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE icloud_accounts SET replica_uuid = ? WHERE user_id = ?`, u, userID); err != nil {
		return nil, err
	}
	return u, nil
}

// GetICloudAccount returns the iCloud binding for a user, or ErrNotFound.
func (s *Store) GetICloudAccount(ctx context.Context, userID int64) (*models.ICloudAccount, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, apple_id, session_blob, notes_folder, created_at FROM icloud_accounts WHERE user_id = ?`, userID)
	a, err := scanICloudAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// ListICloudAccounts returns all bound accounts (used by the pull worker).
func (s *Store) ListICloudAccounts(ctx context.Context) ([]models.ICloudAccount, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, apple_id, session_blob, notes_folder, created_at FROM icloud_accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.ICloudAccount
	for rows.Next() {
		a, err := scanICloudAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// UpsertICloudAccount creates or updates the user's binding apple id, returning
// the account. Session and folder are left untouched on update.
func (s *Store) UpsertICloudAccount(ctx context.Context, userID int64, appleID string) (*models.ICloudAccount, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO icloud_accounts (user_id, apple_id, session_blob, notes_folder, created_at)
		VALUES (?, ?, NULL, '', ?)
		ON CONFLICT(user_id) DO UPDATE SET apple_id = excluded.apple_id`,
		userID, appleID, now)
	if err != nil {
		return nil, err
	}
	return s.GetICloudAccount(ctx, userID)
}

// SaveICloudSession stores the (already encrypted) session blob for a user.
func (s *Store) SaveICloudSession(ctx context.Context, userID int64, blob []byte) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE icloud_accounts SET session_blob = ? WHERE user_id = ?`, blob, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetNotesFolder records the chosen root Notes folder for a user.
func (s *Store) SetNotesFolder(ctx context.Context, userID int64, folder string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE icloud_accounts SET notes_folder = ? WHERE user_id = ?`, folder, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteICloudAccount removes a user's binding (unbind).
func (s *Store) DeleteICloudAccount(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM icloud_accounts WHERE user_id = ?`, userID)
	return err
}

// --- sync state -------------------------------------------------------------

// GetSyncState returns the sync state for a recipe, or ErrNotFound.
func (s *Store) GetSyncState(ctx context.Context, recipeID int64) (*models.SyncState, error) {
	var st models.SyncState
	var last string
	err := s.db.QueryRowContext(ctx,
		`SELECT recipe_id, note_id, last_synced_at, local_hash, remote_hash FROM sync_state WHERE recipe_id = ?`, recipeID).
		Scan(&st.RecipeID, &st.NoteID, &last, &st.LocalHash, &st.RemoteHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	st.LastSyncedAt, _ = time.Parse(time.RFC3339, last)
	return &st, nil
}

// UpsertSyncState inserts or replaces the sync state for a recipe.
func (s *Store) UpsertSyncState(ctx context.Context, st models.SyncState) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sync_state (recipe_id, note_id, last_synced_at, local_hash, remote_hash)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(recipe_id) DO UPDATE SET
			note_id = excluded.note_id,
			last_synced_at = excluded.last_synced_at,
			local_hash = excluded.local_hash,
			remote_hash = excluded.remote_hash`,
		st.RecipeID, st.NoteID, now, st.LocalHash, st.RemoteHash)
	return err
}

// --- conflicts --------------------------------------------------------------

// InsertConflict records a sync conflict for manual resolution.
func (s *Store) InsertConflict(ctx context.Context, c models.SyncConflict) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO sync_conflicts (recipe_id, note_id, kind, detail, created_at, resolved)
		VALUES (?, ?, ?, ?, ?, 0)`,
		c.RecipeID, c.NoteID, c.Kind, c.Detail, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListConflicts returns unresolved conflicts, newest first.
func (s *Store) ListConflicts(ctx context.Context) ([]models.SyncConflict, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, recipe_id, note_id, kind, detail, created_at, resolved
		FROM sync_conflicts WHERE resolved = 0 ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.SyncConflict
	for rows.Next() {
		c, err := scanConflict(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// GetConflict returns a conflict by id, or ErrNotFound.
func (s *Store) GetConflict(ctx context.Context, id int64) (*models.SyncConflict, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, recipe_id, note_id, kind, detail, created_at, resolved
		FROM sync_conflicts WHERE id = ?`, id)
	c, err := scanConflict(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// ResolveConflict marks a conflict resolved.
func (s *Store) ResolveConflict(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE sync_conflicts SET resolved = 1 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanConflict(row interface{ Scan(...any) error }) (*models.SyncConflict, error) {
	var c models.SyncConflict
	var recipeID sql.NullInt64
	var noteID sql.NullString
	var created string
	var resolved int
	if err := row.Scan(&c.ID, &recipeID, &noteID, &c.Kind, &c.Detail, &created, &resolved); err != nil {
		return nil, err
	}
	if recipeID.Valid {
		c.RecipeID = &recipeID.Int64
	}
	if noteID.Valid {
		c.NoteID = &noteID.String
	}
	c.CreatedAt, _ = time.Parse(time.RFC3339, created)
	c.Resolved = resolved != 0
	return &c, nil
}
