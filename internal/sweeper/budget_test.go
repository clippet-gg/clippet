package sweeper

import (
	"context"
	"log/slog"
	"testing"

	"clippet/internal/httpx"
)

const budgetWarn = "storage budget exceeded (advisory: no upload is ever refused because of it, unless STORAGE_BUDGET_HARD=1)"

// budgetWarnHard is the same crossing on an instance that armed the cap (§20
// erratum 277). The two sentences mean opposite things to whoever reads the
// log, so they are two sentences.
const budgetWarnHard = "storage budget exceeded (STORAGE_BUDGET_HARD=1: new clips are being refused with 507)"

// TestStorageBudgetIsEdgeTriggered: a WARN on the crossing, silence while it
// stays crossed, and an INFO on the way back under. A line that fired every
// tick would be a line an operator filters out.
func TestStorageBudgetIsEdgeTriggered(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	holder := &httpx.StorageBudget{}
	cfg := fx.cfg()
	cfg.StorageBudgetBytes = 1 // every clip's 1-byte source crosses it
	s := fx.sweeper(cfg).WithBudget(holder)

	// Under the line: measured, published, and nothing said.
	s.RunOnce(ctx)
	if b := holder.Get(); !b.Known || b.Used != 0 || b.Budget != 1 || b.Over {
		t.Fatalf("first reading = %+v", b)
	}
	if n := fx.logs.count(budgetWarn, slog.LevelWarn); n != 0 {
		t.Fatalf("%d warnings while under the budget", n)
	}

	// Two clips of one byte each cross it. The WARN fires ONCE.
	fx.clip()
	fx.clip()
	s.RunOnce(ctx)
	s.RunOnce(ctx)
	s.RunOnce(ctx)
	if b := holder.Get(); !b.Over || b.Used != 2 {
		t.Fatalf("reading over the budget = %+v", b)
	}
	if n := fx.logs.count(budgetWarn, slog.LevelWarn); n != 1 {
		t.Fatalf("%d warnings for one crossing, want exactly 1", n)
	}

	// …and coming back under says so, once.
	deleteEveryClip(t, fx)
	s.RunOnce(ctx)
	s.RunOnce(ctx)
	if b := holder.Get(); b.Over || b.Used != 0 {
		t.Fatalf("reading after the clips went = %+v", b)
	}
	if n := fx.logs.count("storage back under budget", slog.LevelInfo); n != 1 {
		t.Fatalf("%d 'back under' lines, want exactly 1", n)
	}
	// The advisory line is never an ERROR: it is not a human-needed event.
	if lvl := fx.logs.maxLevel(); lvl > slog.LevelWarn {
		t.Errorf("the budget check logged at %s", lvl)
	}
}

// TestStorageBudgetOffStillPublishesTheReading: with STORAGE_BUDGET_BYTES
// unset the measurement is taken and published anyway, and NOTHING else
// happens — no line, no verdict, `Over` false by construction.
//
// It read "measures nothing" until ROADMAP #39 (§20 erratum 270), and the
// change is deliberate: the stored figure is now the first number of the
// storage-cost line on /healthz and on the settings page, and both read this
// cached reading rather than growing an aggregate over `clips` of their own —
// which is the rule erratum 157 set for exactly this number. What still depends
// on a budget existing is the WARNING, and the `storage_budget` object, which
// httpapi gates on `Budget > 0`.
func TestStorageBudgetOffStillPublishesTheReading(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	holder := &httpx.StorageBudget{}
	fx.clip()
	s := fx.sweeper(fx.cfg()).WithBudget(holder) // StorageBudgetBytes is 0
	s.RunOnce(ctx)
	b := holder.Get()
	if !b.Known || b.Used != 1 {
		t.Fatalf("the reading with the budget off = %+v, want the one measured byte", b)
	}
	if b.Budget != 0 || b.Over {
		t.Fatalf("a verdict with no budget configured: %+v", b)
	}
	if n := fx.logs.count(budgetWarn, slog.LevelWarn); n != 0 {
		t.Errorf("%d budget warnings with the feature off", n)
	}
}

// TestStorageBudgetHardSaysSoInTheWarning: with STORAGE_BUDGET_HARD=1 the same
// crossing is not a number to watch — uploads are being refused from that
// moment — and the log line says which of the two it is (§20 erratum 277). The
// sweeper still refuses nothing itself: the cap lives at the one place a clip is
// created and reads the database live, never this cached reading.
func TestStorageBudgetHardSaysSoInTheWarning(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	holder := &httpx.StorageBudget{}
	cfg := fx.cfg()
	cfg.StorageBudgetBytes = 1
	cfg.StorageBudgetHard = true
	s := fx.sweeper(cfg).WithBudget(holder)
	fx.clip()
	fx.clip()
	s.RunOnce(ctx)
	s.RunOnce(ctx)
	if n := fx.logs.count(budgetWarnHard, slog.LevelWarn); n != 1 {
		t.Errorf("%d hard-cap warnings for one crossing, want exactly 1", n)
	}
	if n := fx.logs.count(budgetWarn, slog.LevelWarn); n != 0 {
		t.Errorf("a capped instance logged the advisory sentence %d time(s)", n)
	}
}

// TestStorageBudgetWithNoHolderIsHarmless: a Sweeper built without WithBudget
// (every test that does not care, and any future caller) must not panic.
func TestStorageBudgetWithNoHolderIsHarmless(t *testing.T) {
	fx := newFixture(t)
	cfg := fx.cfg()
	cfg.StorageBudgetBytes = 1
	fx.clip()
	fx.sweeper(cfg).RunOnce(context.Background())
}

// deleteEveryClip tombstones every clip, which is what takes the instance
// back under its budget: a `deleting` row is already gone everywhere else, so
// StorageUsed stops counting it at that moment rather than when the objects
// finally go.
func deleteEveryClip(t *testing.T, fx *fixture) {
	t.Helper()
	ctx := context.Background()
	rows, err := fx.db.ListAllClipIDsAndKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if err := fx.db.MarkDeleting(ctx, r.ID, fx.now.Unix()); err != nil {
			t.Fatal(err)
		}
	}
}
