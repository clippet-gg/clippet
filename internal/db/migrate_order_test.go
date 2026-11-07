package db

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrateRefusesAGapFillingFile (AUDIT #23): a migration file whose version
// is BELOW max(applied) is refused by name, before anything is applied.
//
// The scenario is the ordinary one, not an exotic one: this repository's
// migration numbers already have holes (0006, 0009, 0010), somebody fills one
// on a branch, and the file applies FIRST on a fresh database and LAST on every
// database that already ran 0017. Two instances of one build, two schemas, no
// message anywhere. Nothing here can renumber the file for the operator, so the
// refusal names it and says where it has to go.
//
// The database is driven to a state no fixture can otherwise reach by recording
// a version above every embedded file: every real file is then "unapplied and
// below max(applied)", which is exactly the shape a gap-filler has.
func TestMigrateRefusesAGapFillingFile(t *testing.T) {
	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "clippet.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.rw.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.rw.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, applied_at) VALUES (9999, 1)"); err != nil {
		t.Fatal(err)
	}

	err = d.Migrate(ctx)
	if !errors.Is(err, ErrMigrationOutOfOrder) {
		t.Fatalf("Migrate = %v, want ErrMigrationOutOfOrder", err)
	}
	if !strings.Contains(err.Error(), "0001_init.sql") {
		t.Errorf("the refusal does not name the file: %v", err)
	}
	if !strings.Contains(err.Error(), "9999") {
		t.Errorf("the refusal does not name the applied version it would land behind: %v", err)
	}
	// Nothing was applied: the check runs over the whole list before the first
	// transaction, so a refused Migrate leaves the database where it was.
	got, err := d.AppliedVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 9999 {
		t.Fatalf("applied versions after a refusal = %v, want only the planted 9999", got)
	}
	var n int
	if err := d.rw.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='clips'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("a refused Migrate applied 0001 anyway")
	}
}

// TestMigrateIsUnchangedOnTheOrdinaryPaths: a fresh database (max(applied)=0),
// a second Migrate over a fully applied one, and a database whose highest
// applied version is the highest file all pass exactly as before. The refusal
// above may cost nothing that works today.
func TestMigrateIsUnchangedOnTheOrdinaryPaths(t *testing.T) {
	ctx := context.Background()
	d := openTest(t) // a fresh database, fully migrated by the helper
	before, err := d.AppliedVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 {
		t.Fatal("the fixture applied nothing")
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("a second Migrate: %v", err)
	}
	after, err := d.AppliedVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("a second Migrate applied %d → %d versions", len(before), len(after))
	}
}

// TestCheckMigrationOrder is the rule itself, over a synthetic file list: the
// hole this repository would most plausibly grow (0009, between two applied
// files) and every neighbour of it.
func TestCheckMigrationOrder(t *testing.T) {
	files := []migration{{version: 1, name: "0001_init.sql"}, {version: 9, name: "0009_gap.sql"}, {version: 17, name: "0017_last.sql"}}
	for _, tc := range []struct {
		name       string
		applied    []int
		maxApplied int
		wantErr    bool
		wantName   string
	}{
		{name: "a fresh database applies all three", applied: nil, maxApplied: 0},
		{name: "the gap filler arrives after 0017", applied: []int{1, 17}, maxApplied: 17, wantErr: true, wantName: "0009_gap.sql"},
		{name: "the gap filler arrives while 0001 is the newest", applied: []int{1}, maxApplied: 1},
		{name: "the ordinary upgrade: 0017 is the only unapplied file", applied: []int{1, 9}, maxApplied: 9},
		{name: "everything applied", applied: []int{1, 9, 17}, maxApplied: 17},
		{name: "0001 skipped on a database that ran 0009", applied: []int{9}, maxApplied: 9, wantErr: true, wantName: "0001_init.sql"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			have := make(map[int]bool, len(tc.applied))
			for _, v := range tc.applied {
				have[v] = true
			}
			err := checkMigrationOrder(files, have, tc.maxApplied)
			if tc.wantErr != (err != nil) {
				t.Fatalf("checkMigrationOrder = %v, wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.wantName) {
				t.Fatalf("the refusal names the wrong file (want %s): %v", tc.wantName, err)
			}
		})
	}
}

// TestMaxOf: AppliedVersions is ascending, and an empty list is a fresh
// database rather than a panic.
func TestMaxOf(t *testing.T) {
	for _, tc := range []struct {
		in   []int
		want int
	}{{nil, 0}, {[]int{}, 0}, {[]int{1}, 1}, {[]int{1, 9, 17}, 17}} {
		if got := maxOf(tc.in); got != tc.want {
			t.Errorf("maxOf(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
