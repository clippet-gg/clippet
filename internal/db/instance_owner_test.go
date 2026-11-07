package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"clippet/internal/ids"
)

// The owner (migration 0025, §20 erratum 349): the first account created on an
// instance, on every path that creates one; handed to the oldest remaining
// account when it is deleted; moved by SetOwner; never two.

func ownerNames(t *testing.T, d *DB) []string {
	t.Helper()
	rows, err := d.ListUserSummaries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		if r.Owner {
			out = append(out, r.Username)
		}
	}
	return out
}

func wantOwner(t *testing.T, d *DB, want string) {
	t.Helper()
	got := ownerNames(t, d)
	if want == "" {
		if len(got) != 0 {
			t.Fatalf("owners = %v, want none", got)
		}
		return
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("owners = %v, want exactly %q", got, want)
	}
}

// TestFirstAccountIsTheOwner: the first account is the owner whichever verb
// made it, the later ones are not, and IsOwner says so per id.
func TestFirstAccountIsTheOwner(t *testing.T) {
	ctx := context.Background()
	for name, create := range map[string]func(d *DB) (User, error){
		"CreateFirstUser": func(d *DB) (User, error) { return d.CreateFirstUser(ctx, ids.New(), "first", "h", 1000) },
		"CreateUser":      func(d *DB) (User, error) { return d.CreateUser(ctx, ids.New(), "first", "h", 1000) },
	} {
		t.Run(name, func(t *testing.T) {
			d := openTest(t)
			wantOwner(t, d, "")
			first, err := create(d)
			if err != nil {
				t.Fatal(err)
			}
			// Same second, created after: a member.
			second := mkUser(t, d, "second")
			wantOwner(t, d, "first")
			for id, want := range map[string]bool{first.ID: true, second.ID: false, "nobody": false} {
				got, err := d.IsOwner(ctx, id)
				if err != nil || got != want {
					t.Fatalf("IsOwner(%s) = %v, %v; want %v", id, got, err, want)
				}
			}
		})
	}
}

// TestOwnerSurvivesItsDeletion: pruning the owner hands the role to the oldest
// remaining account — also when the one next in line goes in the same prune —
// and a table emptied and refilled gives it to the new first account.
func TestOwnerSurvivesItsDeletion(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	for i, n := range []string{"alpha", "bravo", "charlie", "delta"} {
		if _, err := d.CreateUser(ctx, ids.New(), n, "h", int64(1000+i)); err != nil {
			t.Fatal(err)
		}
	}
	wantOwner(t, d, "alpha")
	if _, err := d.PruneUsers(ctx, "alpha", 5000); err != nil {
		t.Fatal(err)
	}
	wantOwner(t, d, "bravo")
	// bravo (the owner) and charlie (next in line) in one statement: delta.
	if gone, err := d.PruneUsers(ctx, "%r%", 5000); err != nil || len(gone) != 2 {
		t.Fatalf("prune = %v, %v", gone, err)
	}
	wantOwner(t, d, "delta")
	if _, err := d.PruneUsers(ctx, "%", 5000); err != nil {
		t.Fatal(err)
	}
	wantOwner(t, d, "")
	mkUser(t, d, "echo")
	mkUser(t, d, "foxtrot")
	wantOwner(t, d, "echo")
}

// TestSetOwnerMovesTheRole: one owner after every move; an unknown name
// changes nothing; the index refuses a second owner written by hand.
func TestSetOwnerMovesTheRole(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	mkUser(t, d, "one")
	two := mkUser(t, d, "two")
	if u, err := d.SetOwner(ctx, "TWO"); err != nil || u.ID != two.ID {
		t.Fatalf("SetOwner = %+v, %v", u, err)
	}
	wantOwner(t, d, "two")
	if _, err := d.SetOwner(ctx, "two"); err != nil { // idempotent
		t.Fatal(err)
	}
	wantOwner(t, d, "two")
	if _, err := d.SetOwner(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetOwner(nobody) = %v, want ErrNotFound", err)
	}
	wantOwner(t, d, "two")
	if _, err := d.rw.ExecContext(ctx, "UPDATE users SET owner = 1 WHERE username = 'one'"); err == nil {
		t.Fatal("a second owner was written")
	}
	wantOwner(t, d, "two")
}

// TestOwnerMigrationBackfillsTheOldest: an instance that already has accounts
// when 0025 lands gets the OLDEST as its owner (created_at, then insertion
// order inside one second) — not the lowest id.
func TestOwnerMigrationBackfillsTheOldest(t *testing.T) {
	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "clippet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.rw.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	ms, err := migrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	var m25 migration
	for _, m := range ms {
		if m.version == 25 {
			m25 = m
			continue
		}
		if m.version > 25 {
			continue
		}
		if err := d.applyMigration(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if m25.version != 25 {
		t.Fatal("no migration 0025")
	}
	// zzz is the oldest by created_at; within 2000, "bbb" went in before "aaa".
	for _, u := range []struct {
		id, name string
		at       int64
	}{{"00000000000a", "late", 3000}, {"zzzzzzzzzzzz", "zzz", 1000}, {"000000000001", "bbb", 2000}, {"000000000000", "aaa", 2000}} {
		if _, err := d.rw.ExecContext(ctx, "INSERT INTO users (id, username, password_hash, created_at) VALUES (?, ?, 'h', ?)", u.id, u.name, u.at); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.applyMigration(ctx, m25); err != nil {
		t.Fatal(err)
	}
	wantOwner(t, d, "zzz")
	if _, err := d.PruneUsers(ctx, "zzz", 5000); err != nil {
		t.Fatal(err)
	}
	wantOwner(t, d, "bbb")
}
