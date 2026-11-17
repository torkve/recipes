package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

func TestCreateOAuthClientCapped(t *testing.T) {
	s, u, now := oauthFixture(t) // c1 created at now
	ctx := context.Background()
	client := func(id string, minutes int) models.OAuthClient {
		return models.OAuthClient{ID: id, RedirectURIs: []string{"x"}, CreatedAt: now.Add(time.Duration(minutes) * time.Minute)}
	}
	if err := s.CreateOAuthClientCapped(ctx, client("c2", 1), 3); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOAuthClientCapped(ctx, client("c3", 2), 3); err != nil {
		t.Fatal(err)
	}
	// c1 is the oldest but in use; at the cap c2, the oldest unused, goes.
	if _, err := s.CreateOAuthGrant(ctx, models.OAuthGrant{ClientID: "c1", UserID: u.ID, Scope: "s", Resource: "r", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOAuthClientCapped(ctx, client("c4", 3), 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetOAuthClient(ctx, "c2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("c2 not evicted: %v", err)
	}
	for _, id := range []string{"c1", "c3", "c4"} {
		if _, err := s.GetOAuthClient(ctx, id); err != nil {
			t.Fatalf("%s missing: %v", id, err)
		}
	}
	// With every client in use (grant or pending code), registration fails and
	// leaves nothing behind.
	for _, id := range []string{"c3", "c4"} {
		if err := s.SaveOAuthCode(ctx, models.OAuthCode{CodeHash: "h" + id, ClientID: id, UserID: u.ID, ExpiresAt: now.Add(time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateOAuthClientCapped(ctx, client("c5", 4), 3); !errors.Is(err, ErrClientLimit) {
		t.Fatalf("full cap: err = %v, want ErrClientLimit", err)
	}
	if _, err := s.GetOAuthClient(ctx, "c5"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("refused client was stored: %v", err)
	}
}

// TestCreateOAuthClientCappedConcurrent pins that parallel registrations never
// exceed the cap.
func TestCreateOAuthClientCappedConcurrent(t *testing.T) {
	s, _, now := oauthFixture(t)
	ctx := context.Background()
	const max = 10
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := models.OAuthClient{ID: fmt.Sprintf("p%d", i), RedirectURIs: []string{"x"}, CreatedAt: now.Add(time.Duration(i) * time.Second)}
			if err := s.CreateOAuthClientCapped(ctx, c, max); err != nil {
				t.Errorf("register %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if n, _ := s.CountOAuthClients(ctx); n > max {
		t.Fatalf("%d clients registered, cap is %d", n, max)
	}
}
