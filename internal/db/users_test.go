package db

import (
	"context"
	"errors"
	"testing"

	"clippet/internal/ids"
)

// CreateFirstUser is the atomic half of REGISTRATION=first (§20 erratum 106):
// the emptiness test is part of the INSERT, so the second caller cannot win a
// race the first one already ran, however long the two spent in bcrypt.
func TestCreateFirstUser(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u, err := d.CreateFirstUser(ctx, "u1", "carol", "$2a$12$notahash", 1000)
	if err != nil {
		t.Fatalf("the first account must go in: %v", err)
	}
	if u.ID != "u1" || u.Username != "carol" || u.CreatedAt != 1000 {
		t.Errorf("returned row = %+v", u)
	}
	// A second call is refused by the STATEMENT, not by a prior read — and it
	// is refused whatever the username, since the door is "the table is
	// empty" rather than "this name is free".
	if _, err := d.CreateFirstUser(ctx, "u2", "friend", "$2a$12$notahash", 1001); !errors.Is(err, ErrConflict) {
		t.Errorf("second CreateFirstUser = %v, want ErrConflict", err)
	}
	// …and it wrote nothing.
	n, err := d.CountUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("users = %d, want 1", n)
	}
}

// TestListUserSummariesCounts: the inventory `clippet admin users` prints —
// who exists, when, and how much they own. Tokens are the LIVE ones, because
// a revoked token is not a credential and an operator reading this line wants
// to know what can still upload.
func TestListUserSummariesCounts(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	empty := mkUser(t, d, "empty")
	busy := mkUser(t, d, "busy")

	c, _ := mkClip(t, d, busy, 1000, nil, false)
	if _, err := d.CreateCollection(ctx, ids.New(), busy.ID, "Best of", 1000); err != nil {
		t.Fatal(err)
	}
	live := APIToken{ID: ids.New(), UserID: busy.ID, Name: "live", TokenHash: "h1", Prefix: "p1", CreatedAt: 1000}
	revoked := int64(1001)
	dead := APIToken{ID: ids.New(), UserID: busy.ID, Name: "dead", TokenHash: "h2", Prefix: "p2", CreatedAt: 1000, RevokedAt: &revoked}
	for _, tok := range []APIToken{live, dead} {
		if err := d.CreateToken(ctx, tok); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := d.ListUserSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]UserSummary{}
	for _, r := range rows {
		got[r.Username] = r
	}
	if len(rows) != 2 {
		t.Fatalf("summaries = %+v, want 2", rows)
	}
	if e := got["empty"]; e.ID != empty.ID || e.Clips != 0 || e.Collections != 0 || e.Tokens != 0 {
		t.Errorf("empty = %+v", e)
	}
	if b := got["busy"]; b.Clips != 1 || b.Collections != 1 || b.Tokens != 1 {
		t.Errorf("busy = %+v (one live token, not the revoked one)", b)
	}
	// A tombstone still counts as a clip: the row is there, its objects may
	// still be there, and its owner is not a candidate for the prune.
	if err := d.MarkDeleting(ctx, c.ID, 2000); err != nil {
		t.Fatal(err)
	}
	rows, err = d.ListUserSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Username == "busy" && r.Clips != 1 {
			t.Errorf("a deleting clip must still count against its owner: %+v", r)
		}
	}
}

// TestPruneUsersOnlyRemovesOwnersOfNothing is the whole safety story of the
// one command that deletes a user: the pattern, the age, and the two
// ownership guards that stop a cascade from orphaning a bucket (§20 erratum
// 86).
func TestPruneUsersOnlyRemovesOwnersOfNothing(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)

	// Four e2e-shaped users: one clean, one with a clip, one with a
	// collection, one too young. Plus a real user the pattern must not name.
	clean := mkUser(t, d, "e2e-clean")
	withClip := mkUser(t, d, "e2e-withclip")
	withCol := mkUser(t, d, "e2e-withcollection")
	young := mkUser(t, d, "e2e-young")
	carol := mkUser(t, d, "carol")
	if _, err := d.rw.ExecContext(ctx, "UPDATE users SET created_at = ? WHERE id <> ?", 1000, young.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.rw.ExecContext(ctx, "UPDATE users SET created_at = ? WHERE id = ?", 9000, young.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := mkClipNoTags(t, d, withClip, 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateCollection(ctx, ids.New(), withCol.ID, "Mine", 1000); err != nil {
		t.Fatal(err)
	}
	// The clean user's sessions and tokens are what the cascade has to take.
	if err := d.CreateSession(ctx, "sessionhash", clean.ID, 1000, 99999, 0, "ua"); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateToken(ctx, APIToken{ID: ids.New(), UserID: clean.ID, Name: "t", TokenHash: "th", Prefix: "pf", CreatedAt: 1000}); err != nil {
		t.Fatal(err)
	}

	// An empty pattern is refused rather than read as "everybody".
	for _, bad := range []string{"", "   "} {
		if _, err := d.PruneUsers(ctx, bad, 5000); !errors.Is(err, ErrInvalidPattern) {
			t.Fatalf("PruneUsers(%q) = %v, want ErrInvalidPattern", bad, err)
		}
	}
	if n, _ := d.CountUsers(ctx); n != 5 {
		t.Fatalf("a refused prune deleted something: %d users", n)
	}

	gone, err := d.PruneUsers(ctx, "e2e-%", 5000)
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 1 || gone[0].Username != "e2e-clean" {
		t.Fatalf("pruned %+v, want only e2e-clean", gone)
	}
	left := map[string]bool{}
	rows, err := d.ListUserSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		left[r.Username] = true
	}
	for _, want := range []string{"e2e-withclip", "e2e-withcollection", "e2e-young", "carol"} {
		if !left[want] {
			t.Errorf("%s was pruned and must not have been", want)
		}
	}
	if left["e2e-clean"] {
		t.Error("e2e-clean survived")
	}
	// The cascade took the session and the token with it.
	var n int
	if err := d.ro.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE user_id = ?", clean.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d sessions survived their user (%v)", n, err)
	}
	if err := d.ro.QueryRowContext(ctx, "SELECT COUNT(*) FROM api_tokens WHERE user_id = ?", clean.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d tokens survived their user (%v)", n, err)
	}
	// Matching nothing is an empty result and no error — a prune that finds
	// no candidates is a normal outcome, not a failure.
	if gone, err := d.PruneUsers(ctx, "e2e-%", 5000); err != nil || len(gone) != 0 {
		t.Fatalf("second prune = %+v %v", gone, err)
	}
	_ = carol
}
