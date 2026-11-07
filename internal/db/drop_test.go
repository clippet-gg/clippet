package db

import (
	"context"
	"path/filepath"
	"testing"
)

// The drop folder's decisions table (ROADMAP #42 (b), §20 erratum 296;
// migration 0018): what has already been ingested or refused, keyed by what a
// directory listing knows.

func TestDropSeenRemembersOneDecisionPerFile(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)

	// A file nobody has decided about.
	seen, err := d.DropSeen(ctx, "clip.mp4", 1000, 1700)
	if err != nil || seen {
		t.Fatalf("an empty table reads %v, %v", seen, err)
	}

	if err := d.MarkDropSeen(ctx, "clip.mp4", 1000, 1700, 1800, "Ab3XyZ9kLmQp"); err != nil {
		t.Fatal(err)
	}
	if seen, err := d.DropSeen(ctx, "clip.mp4", 1000, 1700); err != nil || !seen {
		t.Fatalf("the decision was not remembered: %v, %v", seen, err)
	}

	// The key is all three facts. A file of the same name that has GROWN, or
	// been re-saved, is a different file and is looked at again — which is what
	// makes "fix it and touch it" a true instruction.
	for _, k := range []struct {
		size, mtime int64
		what        string
	}{
		{1001, 1700, "a different size"},
		{1000, 1701, "a different mtime"},
	} {
		if seen, err := d.DropSeen(ctx, "clip.mp4", k.size, k.mtime); err != nil || seen {
			t.Fatalf("%s reads as already decided (%v, %v)", k.what, seen, err)
		}
	}
	// And so is a different NAME with the same size and mtime.
	if seen, err := d.DropSeen(ctx, "other.mp4", 1000, 1700); err != nil || seen {
		t.Fatalf("a different name reads as already decided (%v, %v)", seen, err)
	}
}

// A REFUSAL is remembered exactly as firmly as a success: without a row the
// watcher would retry a file it cannot ingest every few seconds, for ever.
func TestDropSeenRemembersARefusal(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)

	if err := d.MarkDropSeen(ctx, "broken.mp4", 12, 1700, 1800, ""); err != nil {
		t.Fatal(err)
	}
	if seen, err := d.DropSeen(ctx, "broken.mp4", 12, 1700); err != nil || !seen {
		t.Fatalf("a refusal was not remembered: %v, %v", seen, err)
	}
	var clip *string
	if err := d.ro.QueryRowContext(ctx,
		`SELECT clip_id FROM drop_seen WHERE name = ?`, "broken.mp4").Scan(&clip); err != nil {
		t.Fatal(err)
	}
	if clip != nil {
		t.Fatalf("a refusal stored a clip id: %q", *clip)
	}
}

// The same key written twice is the crash window: the newer decision wins
// rather than the write failing on a conflict nobody could act on.
func TestDropSeenUpsertsTheSameKey(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)

	if err := d.MarkDropSeen(ctx, "clip.mp4", 5, 1700, 1800, ""); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkDropSeen(ctx, "clip.mp4", 5, 1700, 1900, "Ab3XyZ9kLmQp"); err != nil {
		t.Fatalf("the second decision was refused: %v", err)
	}
	var clip string
	var at int64
	if err := d.ro.QueryRowContext(ctx,
		`SELECT clip_id, at FROM drop_seen WHERE name = ?`, "clip.mp4").Scan(&clip, &at); err != nil {
		t.Fatal(err)
	}
	if clip != "Ab3XyZ9kLmQp" || at != 1900 {
		t.Fatalf("the newer decision did not win: %q at %d", clip, at)
	}
}

// Retention: the table is a set of decisions, not a history.
func TestPruneDropSeen(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)

	for _, at := range []int64{100, 200, 300} {
		if err := d.MarkDropSeen(ctx, "clip.mp4", at, at, at, ""); err != nil {
			t.Fatal(err)
		}
	}
	n, err := d.PruneDropSeen(ctx, 250)
	if err != nil || n != 2 {
		t.Fatalf("PruneDropSeen(250) = %d, %v", n, err)
	}
	if seen, _ := d.DropSeen(ctx, "clip.mp4", 300, 300); !seen {
		t.Fatal("the surviving decision was pruned")
	}
	if seen, _ := d.DropSeen(ctx, "clip.mp4", 100, 100); seen {
		t.Fatal("an old decision survived the prune")
	}
}

// TestMigrate0018UpgradesAnOlderDatabase applies every migration EXCEPT 0018
// (by version, not by position — a later feature adds files to that directory),
// proves the table is genuinely new, and then upgrades. Copied in shape from
// 0017's, because an existing instance turning the drop folder on is the only
// way anybody will ever reach this migration.
func TestMigrate0018UpgradesAnOlderDatabase(t *testing.T) {
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
		if m.version == 18 {
			skipped++
			continue
		}
		if err := d.applyMigration(ctx, m); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}
	if skipped != 1 {
		t.Fatalf("0018 is missing from the embedded migrations: %d files", len(files))
	}
	// The table does not exist yet, and that failure IS the proof it is new.
	if err := d.MarkDropSeen(ctx, "clip.mp4", 1, 2, 3, ""); err == nil {
		t.Fatal("drop_seen existed before 0018")
	}
	// 0019 (Discord sign-in) is already applied above, so a plain Migrate is the out-of-order
	// shape AUDIT #23 refuses; the helper asserts that refusal and applies 0018 alone.
	applySkipped(t, d, 18)
	if err := d.MarkDropSeen(ctx, "clip.mp4", 1, 2, 3, "Ab3XyZ9kLmQp"); err != nil {
		t.Fatalf("MarkDropSeen on the upgraded database: %v", err)
	}
	if seen, err := d.DropSeen(ctx, "clip.mp4", 1, 2); err != nil || !seen {
		t.Fatalf("the upgraded database reads back %v (%v)", seen, err)
	}
}
