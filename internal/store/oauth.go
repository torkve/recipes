package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"recipes/internal/models"
)

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// CreateOAuthClient registers a client.
func (s *Store) CreateOAuthClient(ctx context.Context, c models.OAuthClient) error {
	uris, err := json.Marshal(c.RedirectURIs)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO oauth_clients (id, name, redirect_uris, created_at) VALUES (?, ?, ?, ?)`,
		c.ID, c.Name, string(uris), fmtTime(c.CreatedAt))
	return err
}

// GetOAuthClient loads a client by id, or ErrNotFound.
func (s *Store) GetOAuthClient(ctx context.Context, id string) (*models.OAuthClient, error) {
	var c models.OAuthClient
	var uris, created string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, redirect_uris, created_at FROM oauth_clients WHERE id = ?`, id).
		Scan(&c.ID, &c.Name, &uris, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(uris), &c.RedirectURIs); err != nil {
		return nil, err
	}
	c.CreatedAt = parseTime(created)
	return &c, nil
}

// CountOAuthClients returns the number of registered clients.
func (s *Store) CountOAuthClients(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_clients`).Scan(&n)
	return n, err
}

// DeleteUnusedOAuthClients removes clients registered before cutoff that hold
// no grant and no pending code, and returns how many were removed.
func (s *Store) DeleteUnusedOAuthClients(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM oauth_clients
		WHERE created_at < ?
		  AND NOT EXISTS (SELECT 1 FROM oauth_grants g WHERE g.client_id = oauth_clients.id)
		  AND NOT EXISTS (SELECT 1 FROM oauth_codes c WHERE c.client_id = oauth_clients.id)`,
		fmtTime(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SaveOAuthCode stores a pending authorization code.
func (s *Store) SaveOAuthCode(ctx context.Context, c models.OAuthCode) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO oauth_codes (code_hash, client_id, user_id, redirect_uri, code_challenge, scope, resource, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.CodeHash, c.ClientID, c.UserID, c.RedirectURI, c.CodeChallenge, c.Scope, c.Resource, fmtTime(c.ExpiresAt))
	return err
}

// ConsumeOAuthCode deletes and returns the code with the given hash, so each
// code is redeemable at most once. Returns ErrNotFound for an unknown or
// already-used code; expiry is the caller's check.
func (s *Store) ConsumeOAuthCode(ctx context.Context, hash string) (*models.OAuthCode, error) {
	var c models.OAuthCode
	var exp string
	err := s.db.QueryRowContext(ctx, `
		DELETE FROM oauth_codes WHERE code_hash = ?
		RETURNING code_hash, client_id, user_id, redirect_uri, code_challenge, scope, resource, expires_at`, hash).
		Scan(&c.CodeHash, &c.ClientID, &c.UserID, &c.RedirectURI, &c.CodeChallenge, &c.Scope, &c.Resource, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.ExpiresAt = parseTime(exp)
	return &c, nil
}

func insertTokens(ctx context.Context, tx *sql.Tx, grantID int64, toks ...models.OAuthToken) error {
	for _, t := range toks {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO oauth_tokens (token_hash, grant_id, kind, expires_at) VALUES (?, ?, ?, ?)`,
			t.Hash, grantID, t.Kind, fmtTime(t.ExpiresAt)); err != nil {
			return err
		}
	}
	return nil
}

// CreateOAuthGrant records a new grant together with its first tokens and
// returns the grant id.
func (s *Store) CreateOAuthGrant(ctx context.Context, g models.OAuthGrant, toks ...models.OAuthToken) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO oauth_grants (client_id, user_id, scope, resource, created_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		g.ClientID, g.UserID, g.Scope, g.Resource, fmtTime(g.CreatedAt), fmtTime(g.CreatedAt))
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := insertTokens(ctx, tx, id, toks...); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

const grantCols = `g.id, g.client_id, c.name, g.user_id, g.scope, g.resource, g.created_at, g.last_used_at`

func scanGrant(row interface{ Scan(...any) error }, extra ...any) (*models.OAuthGrant, error) {
	var g models.OAuthGrant
	var created, used string
	dest := append([]any{&g.ID, &g.ClientID, &g.ClientName, &g.UserID, &g.Scope, &g.Resource, &created, &used}, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	g.CreatedAt, g.LastUsedAt = parseTime(created), parseTime(used)
	return &g, nil
}

// RotateOAuthRefreshToken redeems the refresh token with hash oldHash for
// clientID: it marks the token used, drops the grant's other access tokens and
// stores the replacement tokens, returning the grant. A token that was already
// rotated revokes its whole grant and yields ErrTokenReuse; an unknown, expired
// or other-client token yields ErrNotFound.
func (s *Store) RotateOAuthRefreshToken(ctx context.Context, oldHash, clientID string, now time.Time, toks ...models.OAuthToken) (*models.OAuthGrant, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var exp string
	var used bool
	g, err := scanGrant(tx.QueryRowContext(ctx, `
		SELECT `+grantCols+`, t.expires_at, t.used
		FROM oauth_tokens t
		JOIN oauth_grants g ON g.id = t.grant_id
		JOIN oauth_clients c ON c.id = g.client_id
		WHERE t.token_hash = ? AND t.kind = ?`, oldHash, models.OAuthRefresh), &exp, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if g.ClientID != clientID {
		return nil, ErrNotFound
	}
	if used {
		if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_grants WHERE id = ?`, g.ID); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrTokenReuse
	}
	if !parseTime(exp).After(now) {
		return nil, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE oauth_tokens SET used = 1 WHERE token_hash = ?`, oldHash); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM oauth_tokens WHERE grant_id = ? AND kind = ?`, g.ID, models.OAuthAccess); err != nil {
		return nil, err
	}
	if err := insertTokens(ctx, tx, g.ID, toks...); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE oauth_grants SET last_used_at = ? WHERE id = ?`, fmtTime(now), g.ID); err != nil {
		return nil, err
	}
	g.LastUsedAt = now
	return g, tx.Commit()
}

// LookupOAuthAccessToken returns the grant of an unexpired access token and the
// token's expiry, or ErrNotFound.
func (s *Store) LookupOAuthAccessToken(ctx context.Context, hash string, now time.Time) (*models.OAuthGrant, time.Time, error) {
	var exp string
	g, err := scanGrant(s.db.QueryRowContext(ctx, `
		SELECT `+grantCols+`, t.expires_at
		FROM oauth_tokens t
		JOIN oauth_grants g ON g.id = t.grant_id
		JOIN oauth_clients c ON c.id = g.client_id
		WHERE t.token_hash = ? AND t.kind = ? AND t.expires_at > ?`,
		hash, models.OAuthAccess, fmtTime(now)), &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, ErrNotFound
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	return g, parseTime(exp), nil
}

// TouchOAuthGrant records use of a grant at now, at most once per minute.
func (s *Store) TouchOAuthGrant(ctx context.Context, id int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE oauth_grants SET last_used_at = ? WHERE id = ? AND last_used_at < ?`,
		fmtTime(now), id, fmtTime(now.Add(-time.Minute)))
	return err
}

// RevokeOAuthTokenGrant deletes the grant (and so every token) that the token
// with the given hash belongs to. Unknown tokens are not an error.
func (s *Store) RevokeOAuthTokenGrant(ctx context.Context, hash string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM oauth_grants WHERE id = (SELECT grant_id FROM oauth_tokens WHERE token_hash = ?)`, hash)
	return err
}

// ListOAuthGrants returns a user's grants ordered by last use, newest first.
func (s *Store) ListOAuthGrants(ctx context.Context, userID int64) ([]models.OAuthGrant, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+grantCols+`
		FROM oauth_grants g JOIN oauth_clients c ON c.id = g.client_id
		WHERE g.user_id = ?
		ORDER BY g.last_used_at DESC, g.id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.OAuthGrant
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}

// DeleteOAuthGrant revokes a user's grant, or returns ErrNotFound when the user
// holds no grant with that id.
func (s *Store) DeleteOAuthGrant(ctx context.Context, id, userID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM oauth_grants WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PurgeExpiredOAuth deletes expired codes and tokens, then grants left with no
// unexpired refresh token.
func (s *Store) PurgeExpiredOAuth(ctx context.Context, now time.Time) error {
	ts := fmtTime(now)
	for _, q := range []string{
		`DELETE FROM oauth_codes WHERE expires_at <= ?`,
		`DELETE FROM oauth_tokens WHERE expires_at <= ?`,
		`DELETE FROM oauth_grants WHERE NOT EXISTS (
		   SELECT 1 FROM oauth_tokens t WHERE t.grant_id = oauth_grants.id AND t.kind = 'refresh' AND t.used = 0 AND t.expires_at > ?)`,
	} {
		if _, err := s.db.ExecContext(ctx, q, ts); err != nil {
			return err
		}
	}
	return nil
}
