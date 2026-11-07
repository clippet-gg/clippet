package db

import (
	"context"
	"path/filepath"
	"testing"
)

// The per-day served-bytes counter (ROADMAP #39, §20 erratum 270; migration
// 0017): one row per UTC day, incremented, read one day and one month at a
// time, pruned by age.

func TestBytesServedAddsRatherThanSets(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)

	// Nothing recorded reads as zero, not as an error: a day with no serving on
	// it has no row, and the surfaces print 0 for it.
	today, month, err := d.BytesServed(ctx, "2026-09-11", "2026-09-01", "2026-10-01")
	if err != nil || today != 0 || month != 0 {
		t.Fatalf("an empty table reads %d, %d, %v", today, month, err)
	}

	// Three flushes of the same day accumulate; the row is never overwritten,
	// which is what makes the in-memory window safe to flush repeatedly.
	for _, n := range []int64{1_000, 2_500, 7} {
		if err := d.AddBytesServed(ctx, "2026-09-11", n); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.AddBytesServed(ctx, "2026-09-12", 40); err != nil {
		t.Fatal(err)
	}
	// A neighbouring month, which must never be counted into this one.
	if err := d.AddBytesServed(ctx, "2026-08-31", 1_000_000); err != nil {
		t.Fatal(err)
	}
	if err := d.AddBytesServed(ctx, "2026-10-01", 2_000_000); err != nil {
		t.Fatal(err)
	}
	today, month, err = d.BytesServed(ctx, "2026-09-11", "2026-09-01", "2026-10-01")
	if err != nil {
		t.Fatal(err)
	}
	if today != 3_507 {
		t.Errorf("today = %d, want 3507 (1000+2500+7)", today)
	}
	if month != 3_547 {
		t.Errorf("month = %d, want 3547 (the two September days and neither neighbour)", month)
	}

	// Zero and negative write nothing at all — a flush with an empty window
	// costs no statement.
	if err := d.AddBytesServed(ctx, "2026-09-13", 0); err != nil {
		t.Fatal(err)
	}
	if err := d.AddBytesServed(ctx, "2026-09-13", -10); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := d.ro.QueryRowContext(ctx, "SELECT COUNT(*) FROM bytes_served WHERE day = '2026-09-13'").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("a zero flush created %d row(s)", rows)
	}
}

func TestPruneBytesServedKeepsTheWindow(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	for _, day := range []string{"2025-01-01", "2025-06-30", "2026-09-10", "2026-09-11"} {
		if err := d.AddBytesServed(ctx, day, 5); err != nil {
			t.Fatal(err)
		}
	}
	n, err := d.PruneBytesServed(ctx, "2026-09-10")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("pruned %d rows, want the two from 2025", n)
	}
	// The boundary day itself is KEPT: the prune is strictly-before.
	today, month, err := d.BytesServed(ctx, "2026-09-10", "2026-09-01", "2026-10-01")
	if err != nil || today != 5 || month != 10 {
		t.Errorf("after the prune: %d, %d, %v", today, month, err)
	}
	// Pruning again removes nothing and is not an error — which is what it does
	// on every sweeper tick of a healthy instance.
	if n, err := d.PruneBytesServed(ctx, "2026-09-10"); err != nil || n != 0 {
		t.Errorf("the second prune removed %d rows (%v)", n, err)
	}
}

// TestMigrate0017UpgradesAnOlderDatabase applies every migration EXCEPT 0017
// (by version, not by position — a later feature adds files to that
// directory), proves the table is genuinely new, and then upgrades.
func TestMigrate0017UpgradesAnOlderDatabase(t *testing.T) {
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
	if _, err := d.rw.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	skipped := 0
	for _, m := range files {
		if m.version == 17 {
			skipped++
			continue
		}
		if err := d.applyMigration(ctx, m); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}
	if skipped != 1 {
		t.Fatalf("0017 is missing from the embedded migrations: %d files", len(files))
	}
	// The table does not exist yet, and that failure IS the proof it is new.
	if err := d.AddBytesServed(ctx, "2026-09-11", 1); err == nil {
		t.Fatal("bytes_served existed before 0017")
	}
	// Later migrations (0018, 0019) are already applied above, so a plain Migrate is the
	// out-of-order shape AUDIT #23 refuses; the helper asserts that refusal and applies 0017 alone.
	applySkipped(t, d, 17)
	if err := d.AddBytesServed(ctx, "2026-09-11", 42); err != nil {
		t.Fatalf("AddBytesServed on the upgraded database: %v", err)
	}
	today, _, err := d.BytesServed(ctx, "2026-09-11", "2026-09-01", "2026-10-01")
	if err != nil || today != 42 {
		t.Fatalf("the upgraded database reads back %d (%v)", today, err)
	}
}
