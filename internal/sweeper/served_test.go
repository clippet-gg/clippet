package sweeper

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"clippet/internal/cost"
)

// The served-bytes history's whole lifecycle is one DELETE on the tick that
// already prunes sessions (ROADMAP #39, §20 erratum 270; migration 0017).
func TestTheTickPrunesTheServedBytesHistory(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	s := fx.sweeper(fx.cfg())

	old := cost.Day(fx.now.AddDate(0, 0, -(cost.RetentionDays + 1)))
	edge := cost.Day(fx.now.AddDate(0, 0, -cost.RetentionDays))
	today := cost.Day(fx.now)
	for _, day := range []string{old, edge, today} {
		if err := fx.db.AddBytesServed(ctx, day, 10); err != nil {
			t.Fatal(err)
		}
	}

	s.RunOnce(ctx)

	// The day that has fallen out of the window is gone; the boundary day and
	// today are kept — the window is 400 days and the prune is strictly before.
	start, end := cost.MonthRange(fx.now)
	if _, month, err := fx.db.BytesServed(ctx, today, start, end); err != nil || month < 10 {
		t.Fatalf("today's row did not survive the prune: %d (%v)", month, err)
	}
	// The whole table, read through the one statement there is: 20 bytes means
	// two rows of ten survived and the third went.
	if _, all, err := fx.db.BytesServed(ctx, today, "0000-01-01", "9999-12-31"); err != nil || all != 20 {
		t.Fatalf("the table holds %d bytes after the prune, want 20 (the two days inside the window) — %v", all, err)
	}
	// It is bookkeeping, not an event: nothing above INFO is logged, and a tick
	// that removed nothing says nothing at all.
	before := fx.logs.count("sweep: pruned served-bytes days", slog.LevelInfo)
	if before != 1 {
		t.Fatalf("%d prune lines for one removed day, want 1", before)
	}
	s.RunOnce(ctx)
	if n := fx.logs.count("sweep: pruned served-bytes days", slog.LevelInfo); n != before {
		t.Errorf("a prune that removed nothing still logged (%d lines)", n)
	}
	if lvl := fx.logs.maxLevel(); lvl > slog.LevelWarn {
		t.Errorf("the served-bytes prune logged at %s", lvl)
	}
}

// A tick whose context is already done prunes nothing and does not panic — the
// shape every other bookkeeping call in RunOnce has.
func TestThePruneSkipsACancelledTick(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fx.sweeper(fx.cfg()).pruneServed(ctx, time.Now())
}
