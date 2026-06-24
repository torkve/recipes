package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"recipes/internal/models"
)

func scanUser(row interface{ Scan(...any) error }) (*models.User, error) {
	var u models.User
	var created string
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.IsAdmin, &created); err != nil {
		return nil, err
	}
	u.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return &u, nil
}

// CreateUser inserts a user with an already-hashed password. Returns
// ErrDuplicate if the username is taken.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string, isAdmin bool) (*models.User, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, is_admin, created_at) VALUES (?, ?, ?, ?)`,
		username, passwordHash, isAdmin, now)
	if err != nil {
		if isUniqueErr(err) {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &models.User{ID: id, Username: username, PasswordHash: passwordHash, IsAdmin: isAdmin}, nil
}

// GetUserByUsername returns a user by username, or ErrNotFound.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*models.User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, is_admin, created_at FROM users WHERE username = ?`, username)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

// GetUser returns a user by id, or ErrNotFound.
func (s *Store) GetUser(ctx context.Context, id int64) (*models.User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, is_admin, created_at FROM users WHERE id = ?`, id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

// CountUsers returns the number of users (used to decide admin bootstrap).
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// UpdatePassword sets a new password hash for a user.
func (s *Store) UpdatePassword(ctx context.Context, id int64, passwordHash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, id)
	return err
}
