package db

import (
	"context"
	"path/filepath"
	"testing"
)

// The persisted destroy-failure backoff (N004, migration 0013, §20 erratum
// 232). The POLICY is the sweeper's; what is pinned here is that a schedule
// written by one process is readable by the next one, bounded, and unable to
// outlive the clip it describes.

func TestSweepFailureRoundTripsAndClears(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	c, _ := mkClip(t, d, u, 1000, nil, false)

	if rows, err := d.LoadSweepFailures(ctx, 10); err != nil || len(rows) != 0 {
		t.Fatalf("a fresh database has no backoff: %v %v", rows, err)
	}
	f := SweepFailure{ClipID: c.ID, Failures: 2, LastAttemptAt: 1500, NextAttemptAt: 1740}
	if err := d.SetSweepFailure(ctx, f); err != nil {
		t.Fatalf("SetSweepFailure: %v", err)
	}
	rows, err := d.LoadSweepFailures(ctx, 10)
	if err != nil || len(rows) != 1 || rows[0] != f {
		t.Fatalf("LoadSweepFailures = %+v (%v), want exactly %+v", rows, err, f)
	}
	// The same clip failing again REPLACES its schedule (the count is the
	// sweeper's, not an increment here).
	f.Failures, f.LastAttemptAt, f.NextAttemptAt = 3, 1800, 2280
	if err := d.SetSweepFailure(ctx, f); err != nil {
		t.Fatalf("SetSweepFailure again: %v", err)
	}
	rows, _ = d.LoadSweepFailures(ctx, 10)
	if len(rows) != 1 || rows[0] != f {
		t.Fatalf("after a second failure: %+v, want %+v", rows, f)
	}
	if err := d.ClearSweepFailure(ctx, c.ID); err != nil {
		t.Fatalf("ClearSweepFailure: %v", err)
	}
	if rows, _ := d.LoadSweepFailures(ctx, 10); len(rows) != 0 {
		t.Fatalf("a cleared schedule survived: %+v", rows)
	}
	// Clearing what is not there is not an error: every candidate that never
	// failed takes that path.
	if err := d.ClearSweepFailure(ctx, c.ID); err != nil {
		t.Fatalf("ClearSweepFailure on a missing row: %v", err)
	}
}

// A schedule cannot outlive its clip: the row is keyed by a foreign key, and
// PRAGMA foreign_keys is on in both pools, so a destroyed clip takes it along
// exactly as its jobs, tags and memberships go (§12).
func TestSweepFailureDiesWithItsClip(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	c, _ := mkClip(t, d, u, 1000, nil, false)
	if err := d.SetSweepFailure(ctx, SweepFailure{ClipID: c.ID, Failures: 1, LastAttemptAt: 1, NextAttemptAt: 2}); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkDeleting(ctx, c.ID, 1100); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteClipRow(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := d.CountSweepFailures(ctx); err != nil || n != 0 {
		t.Fatalf("sweep_failures holds %d rows (%v) after the clip was destroyed", n, err)
	}
	// And a schedule for a clip that is ALREADY gone writes nothing rather than
	// failing: an owner DELETE landing while the sweeper's own Destroy of the
	// same prefix fails is a real race, and this is its outcome.
	if err := d.SetSweepFailure(ctx, SweepFailure{ClipID: c.ID, Failures: 1, LastAttemptAt: 1, NextAttemptAt: 2}); err != nil {
		t.Fatalf("SetSweepFailure for a destroyed clip: %v", err)
	}
	if n, _ := d.CountSweepFailures(ctx); n != 0 {
		t.Fatalf("a schedule was written for a clip that no longer exists (%d rows)", n)
	}
}

// The load is bounded, and it takes the rows whose next attempt is FURTHEST
// away — the ones the in-memory eviction policy keeps, because the ones due
// soonest are about to be retried anyway.
func TestLoadSweepFailuresIsBoundedAndTakesTheMostSuppressed(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	var ids []string
	for i := 0; i < 5; i++ {
		c, _ := mkClip(t, d, u, 1000, nil, false)
		ids = append(ids, c.ID)
		if err := d.SetSweepFailure(ctx, SweepFailure{ClipID: c.ID, Failures: i + 1,
			LastAttemptAt: 1000, NextAttemptAt: int64(1000 + i*60)}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := d.LoadSweepFailures(ctx, 2)
	if err != nil || len(rows) != 2 {
		t.Fatalf("LoadSweepFailures(2) = %d rows (%v)", len(rows), err)
	}
	if rows[0].ClipID != ids[4] || rows[1].ClipID != ids[3] {
		t.Fatalf("loaded %s and %s, want the two furthest-away schedules (%s, %s)",
			rows[0].ClipID, rows[1].ClipID, ids[4], ids[3])
	}
	if rows, err := d.LoadSweepFailures(ctx, 0); err != nil || rows != nil {
		t.Fatalf("a zero limit must read nothing: %v %v", rows, err)
	}
}

// TestMigrate0013UpgradesAnOlderDatabase is the upgrade path an install takes:
// a populated database that has never seen 0013 gains the table, and the
// feature works on it immediately.
func TestMigrate0013UpgradesAnOlderDatabase(t *testing.T) {
	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "clippet.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	files, err := migrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	// Everything EXCEPT 0013, found by version rather than by position (a later
	// migration lands in that directory every time a feature needs a column, and
	// the clip helpers below write every column the CURRENT schema has — 0014's
	// filmstrip columns included — so the "older" database is one that has never
	// seen 0013, not one frozen before it).
	if _, err := d.rw.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	skipped := 0
	for _, m := range files {
		if m.version == 13 {
			skipped++
			continue
		}
		if err := d.applyMigration(ctx, m); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}
	if skipped != 1 {
		t.Fatalf("0013 is missing from the embedded migrations: %d files", len(files))
	}
	u := mkUser(t, d, "upgrader")
	c, _ := mkClip(t, d, u, 1000, nil, false)
	// The table does not exist yet, and that failure IS the proof it is new.
	if _, err := d.CountSweepFailures(ctx); err == nil {
		t.Fatal("sweep_failures existed before 0013")
	}

	applySkipped(t, d, 13)
	if n, err := d.CountSweepFailures(ctx); err != nil || n != 0 {
		t.Fatalf("after 0013 the table holds %d rows (%v), want an empty one", n, err)
	}
	f := SweepFailure{ClipID: c.ID, Failures: 1, LastAttemptAt: 2000, NextAttemptAt: 2120}
	if err := d.SetSweepFailure(ctx, f); err != nil {
		t.Fatalf("SetSweepFailure on the upgraded database: %v", err)
	}
	rows, err := d.LoadSweepFailures(ctx, 10)
	if err != nil || len(rows) != 1 || rows[0] != f {
		t.Fatalf("the upgraded database reads back %+v (%v), want %+v", rows, err, f)
	}
	// The clip that existed BEFORE the migration still reads and still destroys.
	if _, err := d.GetClip(ctx, c.ID); err != nil {
		t.Fatalf("a pre-0013 clip no longer reads: %v", err)
	}
}
