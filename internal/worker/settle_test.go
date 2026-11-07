package worker

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"clippet/internal/db"
)

// The settle hook (§20 erratum 331) is only worth waiting on if it really is
// LAST. These pin that: at the instant Deps.OnSettled is called, every effect
// the tests used to race — the row, the log line written after it, the repair
// counter, the run's untrack — is already there.

// settleSnapshot is what the world looked like when one job settled.
type settleSnapshot struct {
	s        Settled
	logs     string
	holds    bool
	repaired int64
}

// recordSettles wraps the harness's settle hook so each settle also captures a
// snapshot, taken inside the call itself.
func recordSettles(h *harness) func() []settleSnapshot {
	var mu sync.Mutex
	var got []settleSnapshot
	h.w.deps.OnSettled = func(s Settled) {
		snap := settleSnapshot{s: s, logs: h.logs.String(), holds: h.w.holdsJob(s.JobID)}
		if h.w.deps.Repairs != nil {
			snap.repaired = h.w.deps.Repairs.Get().Repaired
		}
		mu.Lock()
		got = append(got, snap)
		mu.Unlock()
		h.settles.add(s)
	}
	return func() []settleSnapshot {
		mu.Lock()
		defer mu.Unlock()
		return append([]settleSnapshot(nil), got...)
	}
}

// A notify job's settle comes after its row (failed), after the WARN line the
// row flip precedes, and after the run has left the live set.
func TestSettleComesAfterARunsLastEffect(t *testing.T) {
	h := newHarness(t)
	snaps := recordSettles(h)
	hk := newHook(t, reply{status: 400, body: `{"message":"Cannot send an empty message","code":50006}`})
	h.withNotifier(t, hk.endpoint(), 0)
	c := h.readyWithNotify(t, false)
	j := h.waitNotify(t, c.ID, func(j db.Job) bool { return j.State == db.JobFailed })
	var snap *settleSnapshot
	for _, s := range snaps() {
		if s.s.JobID == j.ID {
			snap = &s
		}
	}
	if snap == nil {
		t.Fatalf("the notify job %d never settled; settles %+v", j.ID, snaps())
	}
	if snap.s.Kind != db.KindNotify || snap.s.ClipID != c.ID || snap.s.Repair {
		t.Fatalf("settle = %+v", snap.s)
	}
	if snap.holds {
		t.Fatal("the job settled while this process still held it")
	}
	if !strings.Contains(snap.logs, "discord webhook rejected; not retrying") {
		t.Fatalf("the job settled before its WARN line was written:\n%s", snap.logs)
	}
	// A transcode settles too, and once per run.
	n := 0
	for _, s := range snaps() {
		if s.s.ClipID == c.ID && s.s.Kind == db.KindTranscode {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the transcode settled %d time(s), want 1", n)
	}
}

// A repaired row settles after the counter, the per-row line and the pass's
// summary line — the order gate 26a's poll lost to.
func TestSettleComesAfterARepairsCounterAndLines(t *testing.T) {
	h := newHarness(t)
	h.w.deps.Repairs = &Repairs{}
	snaps := recordSettles(h)
	_, j := h.stuckJob(time.Hour)
	if n := h.w.repairOnce(context.Background(), h.repairLog(), time.Minute); n != 1 {
		t.Fatalf("repairOnce repaired %d, want 1", n)
	}
	got := snaps()
	if len(got) != 1 || got[0].s.JobID != j.ID || !got[0].s.Repair || got[0].s.Kind != db.KindTranscode {
		t.Fatalf("settles = %+v, want the one repair", got)
	}
	if got[0].repaired != 1 {
		t.Fatalf("the repair settled before its counter was recorded (repaired=%d)", got[0].repaired)
	}
	for _, line := range []string{"repaired a job that was running with no worker", "repaired=1 alive_here=0"} {
		if !strings.Contains(got[0].logs, line) {
			t.Fatalf("the repair settled before %q was written:\n%s", line, got[0].logs)
		}
	}
}

// nil is production: a worker with no hook runs a job exactly as before.
func TestSettleHookNilIsANoOp(t *testing.T) {
	h := newHarness(t)
	h.w.deps.OnSettled = nil
	c, _ := h.newClip("mp4", 100, true)
	h.start()
	h.waitClip(c.ID, db.ClipReady)
	h.assertNoErrorLogs()
}

// A hook that panics is one ERROR line, never the worker's crash.
func TestSettleHookPanicIsOneLine(t *testing.T) {
	h := newHarness(t)
	h.w.deps.OnSettled = func(Settled) { panic("boom") }
	c, _ := h.newClip("mp4", 100, true)
	h.start()
	h.waitClip(c.ID, db.ClipReady)
	h.w.settled(Settled{JobID: 1}) // the same guard, called directly: the panic is recovered here too
	h.assertLog("ERROR", "settle hook panicked")
}
