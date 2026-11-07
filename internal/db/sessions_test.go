package db

import (
	"context"
	"errors"
	"testing"
)

// The §20 erratum 105 ceiling: a touch may slide expires_at, but never past
// max_expires_at, and GetSession refuses a row whose ceiling has passed even
// though the once-a-minute touch has not run yet.
func TestSessionMaxExpiresCeiling(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")

	const created, ceiling = 1000, 1000 + 3600
	if err := d.CreateSession(ctx, "capped", u.ID, created, created+600, ceiling, "ua"); err != nil {
		t.Fatal(err)
	}
	// A touch well inside the ceiling slides to exactly what was asked for.
	if err := d.TouchSession(ctx, "capped", 1200, 1200+600); err != nil {
		t.Fatal(err)
	}
	s, _, err := d.GetSession(ctx, "capped", 1200)
	if err != nil {
		t.Fatal(err)
	}
	if s.ExpiresAt != 1800 || s.MaxExpiresAt != ceiling {
		t.Fatalf("inside the ceiling: expires=%d max=%d", s.ExpiresAt, s.MaxExpiresAt)
	}
	// A touch that would slide PAST it is clamped, not honoured.
	if err := d.TouchSession(ctx, "capped", 4000, 4000+3600); err != nil {
		t.Fatal(err)
	}
	s, _, err = d.GetSession(ctx, "capped", 4000)
	if err != nil {
		t.Fatal(err)
	}
	if s.ExpiresAt != ceiling {
		t.Fatalf("clamped expires_at = %d, want %d", s.ExpiresAt, ceiling)
	}
	if s.LastSeenAt != 4000 {
		t.Fatalf("last_seen_at = %d, want 4000", s.LastSeenAt)
	}
	// Past the ceiling the row stops resolving, even though expires_at is
	// only just behind: the touch gate is a minute wide and the cap is not.
	if _, _, err := d.GetSession(ctx, "capped", ceiling+1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("past the ceiling: %v, want ErrNotFound", err)
	}
	// A row with NO ceiling recorded (0) is unbounded, never instantly dead —
	// the safe direction for a row an older binary wrote (§20 erratum 105).
	if err := d.CreateSession(ctx, "uncapped", u.ID, created, created+600, 0, "ua"); err != nil {
		t.Fatal(err)
	}
	if err := d.TouchSession(ctx, "uncapped", 1200, 99999); err != nil {
		t.Fatal(err)
	}
	s, _, err = d.GetSession(ctx, "uncapped", 1200)
	if err != nil {
		t.Fatal(err)
	}
	if s.ExpiresAt != 99999 {
		t.Fatalf("uncapped expires_at = %d, want 99999", s.ExpiresAt)
	}
}

// ListSessions is what GET /api/auth/sessions renders: this user's live rows,
// newest first, and nobody else's.
func TestListSessions(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	mine := mkUser(t, d, "carol")
	theirs := mkUser(t, d, "stranger")

	for _, s := range []struct {
		hash            string
		user            string
		created         int64
		expires, ceilAt int64
	}{
		{"old", mine.ID, 1000, 9000, 9000},
		{"new", mine.ID, 2000, 9000, 9000},
		{"expired", mine.ID, 500, 900, 9000},    // expires_at behind now
		{"capped-out", mine.ID, 500, 9000, 900}, // ceiling behind now
		{"stranger", theirs.ID, 3000, 9000, 9000},
	} {
		if err := d.CreateSession(ctx, s.hash, s.user, s.created, s.expires, s.ceilAt, "ua-"+s.hash); err != nil {
			t.Fatal(err)
		}
	}
	got, err := d.ListSessions(ctx, mine.ID, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].TokenHash != "new" || got[1].TokenHash != "old" {
		t.Fatalf("ListSessions = %+v, want [new old]", got)
	}
	if got[0].UserAgent != "ua-new" {
		t.Errorf("user agent = %q", got[0].UserAgent)
	}
	// An empty result is a slice, never nil: the DTO renders `[]`.
	empty, err := d.ListSessions(ctx, "nobody", 1500)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("ListSessions(nobody) = %v, %v", empty, err)
	}
}

// DeleteExpiredSessions is the only pruner (the sweeper's one call), so it has
// to cover both ways a row stops authenticating. A row past its CEILING is
// already invisible to every read and unusable by every caller, and nothing
// pulls its expires_at down — TouchSession is reached only for rows that
// still resolve — so with `expires_at <= ?` alone it sat in the table until
// its own expiry arrived, up to SESSION_TTL later. Migration 0005's backfill
// makes exactly that population on an upgraded instance.
func TestDeleteExpiredSessionsPrunesBothWaysARowDies(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	const now = 5000
	for _, s := range []struct {
		hash            string
		expires, ceilAt int64
	}{
		{"live", now + 3600, now + 7200},
		{"expired", now - 1, now + 7200},      // the sliding expiry arrived
		{"capped-out", now + 999999, now - 1}, // the ceiling arrived; expires_at is months away
		{"uncapped", now + 3600, 0},           // no ceiling recorded (an older binary's row)
		{"both", now - 1, now - 1},            // dead twice over, counted once
	} {
		if err := d.CreateSession(ctx, s.hash, u.ID, 1000, s.expires, s.ceilAt, "ua"); err != nil {
			t.Fatal(err)
		}
	}
	// The premise: the capped-out row cannot authenticate but is still there.
	if _, _, err := d.GetSession(ctx, "capped-out", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("premise broken -- a row past its ceiling still resolves: %v", err)
	}
	n, err := d.DeleteExpiredSessions(ctx, now)
	if err != nil || n != 3 {
		t.Fatalf("DeleteExpiredSessions = %d, %v; want 3 (expired, capped-out, both)", n, err)
	}
	for _, live := range []string{"live", "uncapped"} {
		if _, _, err := d.GetSession(ctx, live, now); err != nil {
			t.Errorf("%s was pruned although it still authenticates: %v", live, err)
		}
	}
	// A second pass removes nothing: the pruner is idempotent.
	if n, err := d.DeleteExpiredSessions(ctx, now); err != nil || n != 0 {
		t.Fatalf("second pass = %d, %v; want 0", n, err)
	}
}

// DeleteOtherSessions is "log out everywhere but here"; the empty keep-hash is
// the admin's "everywhere, full stop".
func TestDeleteOtherAndAllSessions(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	mine := mkUser(t, d, "carol")
	theirs := mkUser(t, d, "stranger")
	for _, h := range []string{"a", "b", "c"} {
		if err := d.CreateSession(ctx, h, mine.ID, 1000, 9000, 9000, "ua"); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.CreateSession(ctx, "z", theirs.ID, 1000, 9000, 9000, "ua"); err != nil {
		t.Fatal(err)
	}
	n, err := d.DeleteOtherSessions(ctx, mine.ID, "b")
	if err != nil || n != 2 {
		t.Fatalf("DeleteOtherSessions = %d, %v; want 2", n, err)
	}
	if _, _, err := d.GetSession(ctx, "b", 1500); err != nil {
		t.Fatalf("the kept session must survive: %v", err)
	}
	// The other user is untouched: this is per-account revocation.
	if _, _, err := d.GetSession(ctx, "z", 1500); err != nil {
		t.Fatalf("another user's session must survive: %v", err)
	}
	// "" keeps nothing.
	if n, err = d.DeleteOtherSessions(ctx, mine.ID, ""); err != nil || n != 1 {
		t.Fatalf("DeleteOtherSessions(keep nothing) = %d, %v; want 1", n, err)
	}
	if n, err = d.DeleteAllSessions(ctx); err != nil || n != 1 {
		t.Fatalf("DeleteAllSessions = %d, %v; want 1 (the stranger's)", n, err)
	}
}

// SetPassword rotates the hash and sweeps every OTHER session in one
// transaction (§20 erratum 108).
func TestSetPasswordRotatesAndSweeps(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	for _, h := range []string{"here", "laptop", "phone"} {
		if err := d.CreateSession(ctx, h, u.ID, 1000, 9000, 9000, "ua"); err != nil {
			t.Fatal(err)
		}
	}
	n, err := d.SetPassword(ctx, u.ID, "$2a$12$rotated", "here")
	if err != nil || n != 2 {
		t.Fatalf("SetPassword = %d, %v; want 2", n, err)
	}
	got, err := d.GetUserByID(ctx, u.ID)
	if err != nil || got.PasswordHash != "$2a$12$rotated" {
		t.Fatalf("hash = %q, %v", got.PasswordHash, err)
	}
	if _, _, err := d.GetSession(ctx, "here", 1500); err != nil {
		t.Fatalf("the caller's own session must survive: %v", err)
	}
	for _, gone := range []string{"laptop", "phone"} {
		if _, _, err := d.GetSession(ctx, gone, 1500); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v, want ErrNotFound", gone, err)
		}
	}
	// A user that is not there is ErrNotFound and changes nothing.
	if _, err := d.SetPassword(ctx, "ghost", "x", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetPassword(ghost) = %v, want ErrNotFound", err)
	}
}

// RevokeUserTokens is the admin's `tokens revoke <user> --all`: every LIVE
// token of that user, nobody else's, and already-revoked rows are not
// re-stamped.
func TestRevokeUserTokens(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	mine := mkUser(t, d, "carol")
	theirs := mkUser(t, d, "stranger")
	revoked := int64(500)
	for _, tk := range []APIToken{
		{ID: "t1", UserID: mine.ID, Name: "a", TokenHash: "h1", Prefix: "p1", Scopes: []string{"upload"}, CreatedAt: 1000},
		{ID: "t2", UserID: mine.ID, Name: "b", TokenHash: "h2", Prefix: "p2", Scopes: []string{"read"}, CreatedAt: 1000},
		{ID: "t3", UserID: mine.ID, Name: "c", TokenHash: "h3", Prefix: "p3", Scopes: []string{"upload"}, CreatedAt: 1000, RevokedAt: &revoked},
		{ID: "t4", UserID: theirs.ID, Name: "d", TokenHash: "h4", Prefix: "p4", Scopes: []string{"upload"}, CreatedAt: 1000},
	} {
		if err := d.CreateToken(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	n, err := d.RevokeUserTokens(ctx, mine.ID, 2000)
	if err != nil || n != 2 {
		t.Fatalf("RevokeUserTokens = %d, %v; want 2", n, err)
	}
	live, err := d.ListTokens(ctx, mine.ID)
	if err != nil || len(live) != 0 {
		t.Fatalf("live tokens after revoke: %v, %v", live, err)
	}
	if live, err = d.ListTokens(ctx, theirs.ID); err != nil || len(live) != 1 {
		t.Fatalf("the stranger's token must survive: %v, %v", live, err)
	}
	// A second pass touches nothing: the WHERE clause is "live only", so a
	// re-run never re-stamps a row that was already revoked.
	if n, err = d.RevokeUserTokens(ctx, mine.ID, 3000); err != nil || n != 0 {
		t.Fatalf("a second revoke touches nothing: %d, %v", n, err)
	}
}

// The `read` scope round-trips through the comma-separated column, and
// HasScope reads it back (§20 erratum 111).
func TestTokenScopesRoundTrip(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	if err := d.CreateToken(ctx, APIToken{ID: "t1", UserID: u.ID, Name: "both", TokenHash: "h1",
		Prefix: "p1", Scopes: []string{"upload", "read"}, CreatedAt: 1000}); err != nil {
		t.Fatal(err)
	}
	got, _, err := d.GetTokenByHash(ctx, "h1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Scopes) != 2 || got.Scopes[0] != "upload" || got.Scopes[1] != "read" {
		t.Fatalf("scopes = %v", got.Scopes)
	}
	if !got.HasScope("read") || !got.HasScope("upload") || got.HasScope("admin") {
		t.Fatalf("HasScope disagrees with %v", got.Scopes)
	}
	// A token minted with NO scopes still defaults to `upload` in the column,
	// which is the pre-scope behaviour every existing row has.
	if err := d.CreateToken(ctx, APIToken{ID: "t2", UserID: u.ID, Name: "bare", TokenHash: "h2",
		Prefix: "p2", CreatedAt: 1000}); err != nil {
		t.Fatal(err)
	}
	got, _, err = d.GetTokenByHash(ctx, "h2")
	if err != nil || len(got.Scopes) != 1 || got.Scopes[0] != "upload" {
		t.Fatalf("bare token scopes = %v, %v", got.Scopes, err)
	}
}
