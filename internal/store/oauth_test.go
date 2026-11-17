package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"recipes/internal/models"
)

func oauthFixture(t *testing.T) (*Store, *models.User, time.Time) {
	t.Helper()
	s := newTestStore(t)
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "cook", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	if err := s.CreateOAuthClient(ctx, models.OAuthClient{
		ID: "c1", Name: "Claude", RedirectURIs: []string{"https://claude.ai/api/mcp/auth_callback"}, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return s, u, now
}

func tok(hash, kind string, exp time.Time) models.OAuthToken {
	return models.OAuthToken{Hash: hash, Kind: kind, ExpiresAt: exp}
}

func TestOAuthClientAndCode(t *testing.T) {
	s, u, now := oauthFixture(t)
	ctx := context.Background()
	c, err := s.GetOAuthClient(ctx, "c1")
	if err != nil || c.Name != "Claude" || len(c.RedirectURIs) != 1 {
		t.Fatalf("GetOAuthClient = %+v, %v", c, err)
	}
	if _, err := s.GetOAuthClient(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown client: %v", err)
	}

	code := models.OAuthCode{CodeHash: "h", ClientID: "c1", UserID: u.ID, RedirectURI: "r", CodeChallenge: "ch",
		Scope: "recipes:read", Resource: "res", ExpiresAt: now.Add(time.Minute)}
	if err := s.SaveOAuthCode(ctx, code); err != nil {
		t.Fatal(err)
	}
	got, err := s.ConsumeOAuthCode(ctx, "h")
	if err != nil || got.CodeChallenge != "ch" || !got.ExpiresAt.Equal(code.ExpiresAt) {
		t.Fatalf("ConsumeOAuthCode = %+v, %v", got, err)
	}
	if _, err := s.ConsumeOAuthCode(ctx, "h"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second consume: err = %v, want ErrNotFound", err)
	}
}

func TestOAuthTokenRotationAndReuse(t *testing.T) {
	s, u, now := oauthFixture(t)
	ctx := context.Background()
	gid, err := s.CreateOAuthGrant(ctx,
		models.OAuthGrant{ClientID: "c1", UserID: u.ID, Scope: "recipes:read", Resource: "res", CreatedAt: now},
		tok("a1", models.OAuthAccess, now.Add(time.Hour)), tok("r1", models.OAuthRefresh, now.Add(48*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}

	g, exp, err := s.LookupOAuthAccessToken(ctx, "a1", now)
	if err != nil || g.ID != gid || g.ClientName != "Claude" || !exp.Equal(now.Add(time.Hour)) {
		t.Fatalf("lookup a1 = %+v %v %v", g, exp, err)
	}
	if _, _, err := s.LookupOAuthAccessToken(ctx, "a1", now.Add(2*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired access: err = %v", err)
	}
	if _, _, err := s.LookupOAuthAccessToken(ctx, "r1", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("refresh token accepted as access: err = %v", err)
	}

	if _, err := s.RotateOAuthRefreshToken(ctx, "r1", "other", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rotation by another client: err = %v", err)
	}
	g, err = s.RotateOAuthRefreshToken(ctx, "r1", "c1", now.Add(time.Minute),
		tok("a2", models.OAuthAccess, now.Add(2*time.Hour)), tok("r2", models.OAuthRefresh, now.Add(72*time.Hour)))
	if err != nil || g.ID != gid {
		t.Fatalf("rotate r1 = %+v, %v", g, err)
	}
	if _, _, err := s.LookupOAuthAccessToken(ctx, "a1", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old access token survived rotation: err = %v", err)
	}
	if _, _, err := s.LookupOAuthAccessToken(ctx, "a2", now); err != nil {
		t.Fatalf("new access token: %v", err)
	}

	// Presenting the rotated r1 again revokes the grant, killing r2 and a2.
	if _, err := s.RotateOAuthRefreshToken(ctx, "r1", "c1", now); !errors.Is(err, ErrTokenReuse) {
		t.Fatalf("reuse: err = %v, want ErrTokenReuse", err)
	}
	if _, _, err := s.LookupOAuthAccessToken(ctx, "a2", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("access token survived reuse revocation: err = %v", err)
	}
	if _, err := s.RotateOAuthRefreshToken(ctx, "r2", "c1", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("refresh token survived reuse revocation: err = %v", err)
	}
}

func TestOAuthExpiredRefreshAndRevocation(t *testing.T) {
	s, u, now := oauthFixture(t)
	ctx := context.Background()
	g := models.OAuthGrant{ClientID: "c1", UserID: u.ID, Scope: "recipes:read", Resource: "res", CreatedAt: now}
	if _, err := s.CreateOAuthGrant(ctx, g, tok("r1", models.OAuthRefresh, now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotateOAuthRefreshToken(ctx, "r1", "c1", now.Add(2*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired refresh: err = %v", err)
	}

	gid, err := s.CreateOAuthGrant(ctx, g, tok("a2", models.OAuthAccess, now.Add(time.Hour)), tok("r2", models.OAuthRefresh, now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeOAuthTokenGrant(ctx, "r2"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LookupOAuthAccessToken(ctx, "a2", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("access token survived revocation: err = %v", err)
	}
	if err := s.RevokeOAuthTokenGrant(ctx, "unknown"); err != nil {
		t.Fatalf("revoking unknown token: %v", err)
	}
	if err := s.DeleteOAuthGrant(ctx, gid, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting revoked grant: err = %v", err)
	}
}

func TestOAuthListDeleteAndPurge(t *testing.T) {
	s, u, now := oauthFixture(t)
	ctx := context.Background()
	other, _ := s.CreateUser(ctx, "other", "hash", false)
	g := models.OAuthGrant{ClientID: "c1", UserID: u.ID, Scope: "recipes:read", Resource: "res", CreatedAt: now}
	gid, err := s.CreateOAuthGrant(ctx, g, tok("r1", models.OAuthRefresh, now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListOAuthGrants(ctx, u.ID)
	if err != nil || len(list) != 1 || list[0].ClientName != "Claude" {
		t.Fatalf("ListOAuthGrants = %+v, %v", list, err)
	}
	if err := s.DeleteOAuthGrant(ctx, gid, other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting another user's grant: err = %v", err)
	}

	// A client with a grant is kept; once the grant expires and is purged, the
	// client is unused and collected.
	if n, _ := s.DeleteUnusedOAuthClients(ctx, now.Add(time.Hour)); n != 0 {
		t.Fatalf("client with a grant deleted (%d)", n)
	}
	if err := s.PurgeExpiredOAuth(ctx, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListOAuthGrants(ctx, u.ID); len(list) != 0 {
		t.Fatalf("grant with only expired tokens survived purge: %+v", list)
	}
	if n, _ := s.DeleteUnusedOAuthClients(ctx, now.Add(time.Hour)); n != 1 {
		t.Fatalf("unused client not deleted (%d)", n)
	}
	if n, _ := s.CountOAuthClients(ctx); n != 0 {
		t.Fatalf("clients left: %d", n)
	}
}
