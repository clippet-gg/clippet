package sweeper

import (
	"context"
	"testing"
	"time"

	"clippet/internal/db"
)

// The destroy-failure backoff SURVIVES a restart (N004, migration 0013, §20
// erratum 232). Erratum 101(a) argued a restart plausibly fixes storage, so
// forgetting the schedule then is right; the restart nobody performed —
// `restart: unless-stopped` plus a process dying every few minutes — is the
// case that argument misses, and these tests are that case.

// deletePrefixCalls counts the Destroys the Fake has actually been asked for,
// which is what "did the restarted process hammer storage again?" means.
func deletePrefixCalls(fx *fixture) int {
	n := 0
	for _, c := range fx.fake.CallsSnapshot() {
		if len(c) >= len("DeletePrefix") && c[:len("DeletePrefix")] == "DeletePrefix" {
			n++
		}
	}
	return n
}

func TestTheDestroyFailureBackoffSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	c := fx.ready()
	fx.expire(c, time.Minute)
	heal := failDestroys(fx)
	defer heal()

	// Process 1: one tick, one failed Destroy, one schedule.
	s1 := fx.sweeper(fx.cfg())
	if st := s1.RunOnce(ctx); st.Failed != 1 || st.Backoff != 0 {
		t.Fatalf("tick 1 Stats = %+v, want one failure", st)
	}
	rows, err := fx.db.LoadSweepFailures(ctx, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("the failure was not written down: %+v %v", rows, err)
	}
	if rows[0].ClipID != c.ID || rows[0].Failures != 1 {
		t.Fatalf("schedule = %+v, want one failure for %s", rows[0], c.ID)
	}
	if rows[0].NextAttemptAt != fx.now.Add(backoffFor(1)).Unix() {
		t.Errorf("next_attempt_at = %d, want %d (the same backoff the map holds)",
			rows[0].NextAttemptAt, fx.now.Add(backoffFor(1)).Unix())
	}
	if rows[0].LastAttemptAt != fx.now.Unix() {
		t.Errorf("last_attempt_at = %d, want the moment of the failure %d", rows[0].LastAttemptAt, fx.now.Unix())
	}
	attemptsBefore := deletePrefixCalls(fx)

	// Process 2: a NEW Sweeper over the same database, exactly as a restarted
	// container is. It must NOT attempt the clip again — that is the whole
	// point — and it must say so in the same Stats field the in-memory backoff
	// uses.
	s2 := fx.sweeper(fx.cfg())
	st := s2.RunOnce(ctx)
	if st.Checked != 1 || st.Backoff != 1 || st.Failed != 0 || st.Destroyed != 0 {
		t.Fatalf("the restarted process's tick = %+v, want the candidate backed off", st)
	}
	if got := deletePrefixCalls(fx) - attemptsBefore; got != 0 {
		t.Fatalf("the restarted process made %d more Destroy attempts: the backoff was forgotten", got)
	}
	if n := fx.logs.count("sweep: restored the destroy-failure backoff a previous process left behind", 0); n != 1 {
		t.Errorf("the restore was not logged once (%d)", n)
	}

	// Past the backoff it is attempted again, and the count CONTINUES from what
	// the previous process recorded rather than starting over: a crash loop
	// cannot walk the schedule back to zero.
	fx.now = fx.now.Add(backoffFor(1) + time.Second)
	if st := s2.RunOnce(ctx); st.Failed != 1 {
		t.Fatalf("past the backoff: %+v, want one attempt", st)
	}
	rows, _ = fx.db.LoadSweepFailures(ctx, 10)
	if len(rows) != 1 || rows[0].Failures != 2 {
		t.Fatalf("schedule after the restarted process failed too: %+v, want failures=2", rows)
	}

	// And when storage comes back, the schedule is DELETED — first from the
	// table, then with the clip.
	heal()
	fx.now = fx.now.Add(failureBackoffCap)
	if st := s2.RunOnce(ctx); st.Destroyed != 1 {
		t.Fatalf("healed tick = %+v, want the clip destroyed", st)
	}
	if n, err := fx.db.CountSweepFailures(ctx); err != nil || n != 0 {
		t.Fatalf("sweep_failures holds %d rows (%v) after a successful Destroy", n, err)
	}
	if fx.state(c.ID) != "gone" {
		t.Fatalf("clip %s survived the healed tick", c.ID)
	}
}

// A tick whose context is already done does not consume the one load: the flag
// that says "this process has its memory" must mean it, or a run cancelled at
// the wrong moment would leave the process blind for its whole life.
func TestACancelledTickDoesNotBurnTheOneLoad(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	c := fx.ready()
	if err := fx.db.SetSweepFailure(ctx, db.SweepFailure{ClipID: c.ID, Failures: 2,
		LastAttemptAt: fx.now.Unix(), NextAttemptAt: fx.now.Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	s := fx.sweeper(fx.cfg())

	dead, cancel := context.WithCancel(ctx)
	cancel()
	s.RunOnce(dead)
	s.failMu.Lock()
	loaded, n := s.loaded, len(s.failing)
	s.failMu.Unlock()
	if loaded || n != 0 {
		t.Fatalf("a cancelled tick loaded (%v) / restored %d schedules", loaded, n)
	}

	s.RunOnce(ctx)
	s.failMu.Lock()
	loaded, n = s.loaded, len(s.failing)
	s.failMu.Unlock()
	if !loaded || n != 1 {
		t.Fatalf("the next live tick loaded=%v with %d schedules, want the one that was written", loaded, n)
	}
	if due, fails := s.retryDue(c.ID, fx.now); due || fails != 2 {
		t.Fatalf("restored schedule: due=%v failures=%d, want backed off with two failures", due, fails)
	}
}

// The other direction of the same rule: a schedule for a clip somebody has
// destroyed is not restored, because the row went with the clip.
func TestARestoredScheduleCannotOutliveItsClip(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	c := fx.ready()
	// A schedule that is already DUE, so the tick attempts the clip rather than
	// backing it off.
	if err := fx.db.SetSweepFailure(ctx, db.SweepFailure{ClipID: c.ID, Failures: 3,
		LastAttemptAt: fx.now.Add(-time.Hour).Unix(), NextAttemptAt: fx.now.Add(-time.Minute).Unix()}); err != nil {
		t.Fatal(err)
	}
	fx.expire(c, time.Minute)
	// Destroy it for real (storage is healthy), which drops the row.
	s := fx.sweeper(fx.cfg())
	if st := s.RunOnce(ctx); st.Destroyed != 1 {
		t.Fatalf("Stats = %+v", st)
	}
	if n, _ := fx.db.CountSweepFailures(ctx); n != 0 {
		t.Fatalf("%d schedules survived the clip", n)
	}
	s2 := fx.sweeper(fx.cfg())
	s2.RunOnce(ctx)
	s2.failMu.Lock()
	n := len(s2.failing)
	s2.failMu.Unlock()
	if n != 0 {
		t.Fatalf("a restarted process restored %d schedules for clips that no longer exist", n)
	}
}
