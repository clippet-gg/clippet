package sweeper

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"clippet/internal/db"
)

// failDestroys makes every DeletePrefix fail until the returned function is
// called. FailNext is one-shot and keyed by op, so a test that needs several
// consecutive failures has to use the hook.
func failDestroys(fx *fixture) func() {
	fx.fake.Hook = func(_ context.Context, op, _ string) error {
		if op == "DeletePrefix" {
			return errors.New("bucket unreachable")
		}
		return nil
	}
	return func() { fx.fake.Hook = nil }
}

// TestFailingCandidatesDoNotCrowdOutTheRest is §20 erratum 101's sweeper half.
// SweepCandidates returns the same ids every tick, RunOnce walks them
// synchronously, and one blackholing prefix costs up to STORAGE_OP_TIMEOUT
// (30 s) of a 55 s tick — so before the backoff, a couple of them could
// consume every tick forever and nothing behind them was ever destroyed. The
// rule: every failed candidate waits out an exponential backoff, and from its
// SECOND consecutive failure on it is also charged to a per-tick budget of
// failingPerTick.
func TestFailingCandidatesDoNotCrowdOutTheRest(t *testing.T) {
	fx := newFixture(t)
	var bad []db.Clip
	for i := 0; i < 3; i++ {
		c := fx.ready()
		fx.expire(c, time.Duration(i+1)*time.Minute)
		bad = append(bad, c)
	}
	s := fx.sweeper(fx.cfg())
	heal := failDestroys(fx)

	// Tick 1: all three are fresh, so all three are attempted, and all fail.
	st := s.RunOnce(context.Background())
	if st.Checked != 3 || st.Failed != 3 || st.Backoff != 0 {
		t.Fatalf("tick 1 Stats = %+v; every fresh candidate must be attempted", st)
	}
	if fx.logs.count("sweep: destroy failed; backing this clip off", slog.LevelWarn) != 3 {
		t.Fatal("a failing Destroy must name its clip at WARN")
	}

	// Tick 2, inside the backoff: nothing is attempted at all.
	st = s.RunOnce(context.Background())
	if st.Checked != 3 || st.Backoff != 3 || st.Failed != 0 || st.Destroyed != 0 {
		t.Fatalf("tick 2 Stats = %+v; a backed-off candidate must not be retried", st)
	}

	// Tick 3, past the backoff: the FIRST retry is free for all of them, so a
	// whole batch that failed because storage was down is retried at once.
	fx.now = fx.now.Add(failureBackoffBase + time.Second)
	st = s.RunOnce(context.Background())
	if st.Checked != 3 || st.Failed != 3 || st.Backoff != 0 {
		t.Fatalf("tick 3 Stats = %+v; the first retry after a failure is free", st)
	}

	// Tick 4: they have now failed twice each, so the per-tick budget bites
	// and a persistently bad prefix can no longer eat the tick.
	fx.now = fx.now.Add(2*failureBackoffBase + time.Second)
	st = s.RunOnce(context.Background())
	if st.Checked != 3 || st.Failed != failingPerTick || st.Backoff != 3-failingPerTick {
		t.Fatalf("tick 4 Stats = %+v; want %d retried and the rest backed off", st, failingPerTick)
	}

	// Healed: every candidate that goes through is forgotten, so no backoff
	// follows it into the next tick.
	heal()
	for i := 0; i < 6 && fx.remaining(bad) > 0; i++ {
		fx.now = fx.now.Add(failureBackoffCap)
		s.RunOnce(context.Background())
	}
	if n := fx.remaining(bad); n != 0 {
		t.Fatalf("%d clips still undestroyed after the bucket healed", n)
	}
	s.failMu.Lock()
	left := len(s.failing)
	s.failMu.Unlock()
	if left != 0 {
		t.Fatalf("%d failure records survived a successful Destroy", left)
	}
}

// The backoff itself: base·2^(n−1), capped, and never zero or negative.
func TestBackoffFor(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want time.Duration
	}{
		{0, failureBackoffBase},
		{1, failureBackoffBase},
		{2, 2 * failureBackoffBase},
		{3, 4 * failureBackoffBase},
		{99, failureBackoffCap},
	} {
		if got := backoffFor(tc.n); got != tc.want {
			t.Errorf("backoffFor(%d) = %s, want %s", tc.n, got, tc.want)
		}
	}
}

// The retry map is bounded like every other cache in this process: a run of
// failures over many clips cannot grow it without limit.
func TestFailingMapIsBounded(t *testing.T) {
	fx := newFixture(t)
	s := fx.sweeper(fx.cfg())
	for i := 0; i < failingMax+50; i++ {
		// No clip rows: the write-through UPSERT is keyed by a foreign key onto
		// `clips`, so every one of these is a no-op on disk (§20 erratum 232)
		// and what is being measured is the MAP's bound, as before.
		s.recordFailure(context.Background(), "clip-"+strconv.Itoa(i), fx.now.Add(time.Duration(i)*time.Second))
	}
	s.failMu.Lock()
	n := len(s.failing)
	s.failMu.Unlock()
	if n > failingMax {
		t.Fatalf("retry map holds %d entries, cap is %d", n, failingMax)
	}
}

// remaining counts how many of the clips still have a row (destroyed rows are
// gone).
func (fx *fixture) remaining(cs []db.Clip) int {
	fx.t.Helper()
	n := 0
	for _, c := range cs {
		if fx.state(c.ID) != "gone" {
			n++
		}
	}
	return n
}
