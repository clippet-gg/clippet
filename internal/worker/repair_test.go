package worker

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
)

// The automatic stuck-job repair (ROADMAP #12, §20 errata 230-231). Every test
// here asserts one half of the same claim: a row NOBODY is running is repaired
// without a restart, and a row THIS PROCESS is running is never touched,
// whatever its age says.
//
// The candidate query's own kind-blindness is pinned in internal/db
// (TestStaleRunningJobsCoversEveryKindAndOnlyRunningRows, all seven kinds);
// what these tests add is the worker's half — the live-claim filter, the
// accounting, and the loop.

// repairLog is the logger repairOnce is called with in these tests (the loop
// passes one with loop=repair; nothing depends on the attribute).
func (h *harness) repairLog() *slog.Logger { return h.w.log.With("loop", "repair") }

// stuckJob leaves the clip's own transcode job `running` with an old started_at
// and NO process on it — the row a `kill -9` mid-encode leaves behind (E2E step
// 73) — by claiming it as the worker would, never running it, and then moving
// the harness clock on. (The claim must happen at the CURRENT time: a claim
// dated in the past would also have to satisfy `run_after <= now`, which the
// row it is about was created with.)
func (h *harness) stuckJob(startedAgo time.Duration) (db.Clip, db.Job) {
	h.t.Helper()
	c, _ := h.newClip("mp4", 1024, true)
	j, got, ok, err := h.db.ClaimJob(context.Background(), h.clock.Now().Unix())
	if err != nil || !ok {
		h.t.Fatalf("ClaimJob: ok=%v err=%v", ok, err)
	}
	if got.ID != c.ID {
		h.t.Fatalf("claimed %s, wanted %s (another clip is in the way)", got.ID, c.ID)
	}
	if state := h.clipRow(c.ID).State; state != db.ClipProcessing {
		h.t.Fatalf("clip is %s after the claim, want processing (this is the stuck shape)", state)
	}
	h.clock.Advance(startedAgo)
	return c, j
}

// reclaimStale is stuckJob for a row a previous repair requeued: past the crash
// backoff FailAttempt set, claimed again, and then aged.
func (h *harness) reclaimStale(age time.Duration) {
	h.t.Helper()
	h.clock.Advance(time.Hour) // past any backoff a previous attempt scheduled
	if _, _, ok, err := h.db.ClaimJob(context.Background(), h.clock.Now().Unix()); err != nil || !ok {
		h.t.Fatalf("re-claim: ok=%v err=%v", ok, err)
	}
	h.clock.Advance(age)
}

func (h *harness) clipRow(id string) db.Clip {
	h.t.Helper()
	c, err := h.db.GetClip(context.Background(), id)
	if err != nil {
		h.t.Fatalf("GetClip(%s): %v", id, err)
	}
	return c
}

// jobOf reads a clip's newest job (in these tests each clip has exactly the one
// the test is about).
func (h *harness) jobOf(clipID string) db.Job {
	h.t.Helper()
	j, err := h.db.GetLatestJob(context.Background(), clipID)
	if err != nil {
		h.t.Fatalf("GetLatestJob(%s): %v", clipID, err)
	}
	return j
}

// A job left running with no worker is requeued — the whole point — with
// `unstick`'s own accounting, wording and §5 clip coupling, and NOT at boot.
func TestAutomaticRepairRequeuesAJobLeftRunningWithNoWorker(t *testing.T) {
	h := newHarness(t)
	h.w.deps.Repairs = &Repairs{}
	c, j := h.stuckJob(2 * time.Hour)

	if n := h.w.repairOnce(context.Background(), h.repairLog(), time.Hour); n != 1 {
		t.Fatalf("repairOnce repaired %d jobs, want 1\nlogs:\n%s", n, h.logs.String())
	}
	after := h.jobOf(c.ID)
	if after.ID != j.ID {
		t.Fatalf("the repair created a new job #%d; it must repair #%d in place", after.ID, j.ID)
	}
	if after.State != db.JobQueued {
		t.Fatalf("job is %s after the repair, want queued", after.State)
	}
	if after.Attempts != 1 {
		t.Errorf("attempts = %d, want exactly one crash attempt spent (§11)", after.Attempts)
	}
	if after.LastError == nil || *after.LastError != db.UnstickReason {
		t.Fatalf("last_error = %v, want the operator's own sentence %q", after.LastError, db.UnstickReason)
	}
	if !db.IsOperatorReason(*after.LastError) {
		t.Error("the repair's reason is not recognised as an OPERATOR sentence, so a route could show it to an owner")
	}
	if got := h.clipRow(c.ID).State; got != db.ClipQueued {
		t.Errorf("clip is %s, want queued: only a transcode moves its clip, and it moves it back (§5)", got)
	}
	got := h.w.deps.Repairs.Get()
	if got.Repaired != 1 || got.Requeued != 1 || got.Released != 0 || got.Failed != 0 || got.Errors != 0 {
		t.Errorf("counters = %+v, want one requeued repair", got)
	}
	h.assertLog("INFO", "repaired a job that was running with no worker")
	h.assertNoErrorLogs()

	// A second pass finds nothing: the row is queued now, and the repair is not
	// a thing that can happen twice to one attempt.
	if n := h.w.repairOnce(context.Background(), h.repairLog(), time.Hour); n != 0 {
		t.Fatalf("a second pass repaired %d jobs", n)
	}
	if got := h.w.deps.Repairs.Get(); got.Repaired != 1 {
		t.Errorf("counters after the second pass = %+v", got)
	}
}

// The property this whole design exists for: a job this process is RUNNING is
// never repaired, even when the age test says it is stale — here with an age of
// zero, so age offers it no protection at all and only the live-claim set does.
func TestAutomaticRepairNeverTouchesAJobThisProcessIsRunning(t *testing.T) {
	h := newHarness(t)
	h.w.deps.Repairs = &Repairs{}
	c, _ := h.newClip("mp4", 1024, true)
	started := make(chan struct{})
	exited := make(chan time.Time, 1)
	h.runner.encodeFn = blockingEncode(started, exited, 0)
	h.start()
	<-started

	j := h.jobOf(c.ID)
	if j.State != db.JobRunning {
		t.Fatalf("the encode's job is %v, want running", j.State)
	}
	if !h.w.holdsJob(j.ID) {
		t.Fatal("the worker does not report the job it is running as live: nothing protects it from the repair")
	}
	// after = 0 → notAfter is NOW, so the row IS a candidate: every job that
	// has started is stale by that measure. Without the live filter this pass
	// would repair an encode that is running.
	cands, err := h.db.StaleRunningJobs(context.Background(), h.clock.Now().Unix(), 0)
	if err != nil || len(cands) != 1 {
		t.Fatalf("candidates = %d (%v), want the live job to BE a candidate for this test to mean anything", len(cands), err)
	}
	for i := 0; i < 3; i++ {
		if n := h.w.repairOnce(context.Background(), h.repairLog(), 0); n != 0 {
			t.Fatalf("pass %d repaired a job an encode is holding\nlogs:\n%s", i, h.logs.String())
		}
	}
	if got := h.jobOf(c.ID); got.State != db.JobRunning || got.Attempts != 0 {
		t.Fatalf("the live job is now state=%s attempts=%d: the repair touched it", got.State, got.Attempts)
	}
	if got := h.clipRow(c.ID).State; got != db.ClipProcessing {
		t.Fatalf("the clip is %s: the repair moved a clip that is mid-encode", got)
	}
	if got := h.w.deps.Repairs.Get(); got != (RepairStatus{}) {
		t.Fatalf("counters = %+v, want nothing counted", got)
	}
	if strings.Contains(h.logs.String(), "repaired a job") {
		t.Fatalf("the log claims a repair:\n%s", h.logs.String())
	}
	// The encode is still there and still ends the ordinary way.
	h.w.Cancel(context.Background(), c.ID)
	<-exited
	h.assertNoErrorLogs()
}

// …and the moment that run lets go, the same row IS repairable: the guard is
// "somebody is on it now", not "somebody was".
func TestAutomaticRepairIsFreeToActOnceTheRunHasGone(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 1024, true)
	started := make(chan struct{})
	exited := make(chan time.Time, 1)
	h.runner.encodeFn = blockingEncode(started, exited, 0)
	cancel := h.start()
	<-started
	j := h.jobOf(c.ID)

	// Cancel the loops' context and let the run exit — NOT Stop(), which also
	// tells the repair to stand down (a stopping worker repairs nothing). Its
	// release puts the job back to queued, so to have a stuck ROW again the test
	// re-claims it by hand, the same two lines stuckJob uses.
	cancel()
	<-exited
	h.until("the cancelled run released its job", func() bool {
		return !h.w.holdsJob(j.ID) && h.jobOf(c.ID).State == db.JobQueued
	})
	if h.w.holdsJob(j.ID) {
		t.Fatal("the worker still reports a job whose run has exited as live")
	}
	if got := h.jobOf(c.ID); got.State != db.JobQueued {
		t.Fatalf("the cancelled run left its job %s, want queued (§10.1's release)", got.State)
	}
	h.reclaimStale(2 * time.Hour)
	if n := h.w.repairOnce(context.Background(), h.repairLog(), time.Hour); n != 1 {
		t.Fatalf("repairOnce repaired %d jobs after the run had gone\nlogs:\n%s", n, h.logs.String())
	}
}

// A checkpointed job is RELEASED with no attempt spent — the boot reconcile's
// rule, which the automatic repair inherits by calling the same code.
func TestAutomaticRepairReleasesACheckpointedJob(t *testing.T) {
	h := newHarness(t)
	h.w.deps.Repairs = &Repairs{}
	c, j := h.stuckJob(2 * time.Hour)
	if err := h.db.SaveCheckpoint(context.Background(), j.ID, *j.StartedAt, `{"rendition_key":"r"}`, h.clock.Now().Unix()); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	if n := h.w.repairOnce(context.Background(), h.repairLog(), time.Hour); n != 1 {
		t.Fatalf("repairOnce repaired %d jobs, want 1", n)
	}
	after := h.jobOf(c.ID)
	if after.State != db.JobQueued || after.Attempts != 0 {
		t.Fatalf("job = state %s attempts %d, want queued with NO attempt spent (its outputs are complete)",
			after.State, after.Attempts)
	}
	if got := h.w.deps.Repairs.Get(); got.Released != 1 || got.Requeued != 0 || got.Repaired != 1 {
		t.Errorf("counters = %+v, want one release", got)
	}
	if got := h.clipRow(c.ID).State; got != db.ClipQueued {
		t.Errorf("clip = %s, want queued", got)
	}
}

// The failure doctrine after a repair: each pass spends one crash attempt, and
// the last one FAILS the job — and, for a transcode, the clip, with the
// operator's sentence the share page is worded for (§11, erratum 146).
func TestAutomaticRepairRespectsTheAttemptBudget(t *testing.T) {
	h := newHarness(t)
	h.w.deps.Repairs = &Repairs{}
	c, _ := h.stuckJob(2 * time.Hour)

	for i := 1; i <= h.cfg.Transcode.MaxAttempts; i++ {
		if i > 1 {
			// Re-claim the row the previous pass requeued and age it again: a
			// crash loop is exactly this shape.
			h.reclaimStale(2 * time.Hour)
		}
		if n := h.w.repairOnce(context.Background(), h.repairLog(), time.Hour); n != 1 {
			t.Fatalf("pass %d repaired %d jobs", i, n)
		}
		if got := h.jobOf(c.ID).Attempts; got != i {
			t.Fatalf("pass %d left attempts=%d", i, got)
		}
	}
	after := h.jobOf(c.ID)
	if after.State != db.JobFailed {
		t.Fatalf("job is %s after %d repairs, want failed at the cap", after.State, h.cfg.Transcode.MaxAttempts)
	}
	got := h.clipRow(c.ID)
	if got.State != db.ClipFailed {
		t.Fatalf("clip is %s, want failed (§5: a transcode moves its clip)", got.State)
	}
	if got.FailureReason == nil || *got.FailureReason != db.UnstickReason {
		t.Fatalf("failure_reason = %v, want the sentence worded for the share page", got.FailureReason)
	}
	if c := h.w.deps.Repairs.Get(); c.Repaired != 3 || c.Failed != 1 || c.Requeued != 2 {
		t.Errorf("counters = %+v, want three repairs of which the last failed", c)
	}
	h.assertNoErrorLogs()
}

// A media kind is repaired by the same pass and leaves its READY clip alone —
// the other half of the §5 coupling. The kinds here are enqueued through their
// own routes rather than inserted, so this is the shape a real instance has.
func TestAutomaticRepairRepairsMediaKindsAndLeavesTheClipReady(t *testing.T) {
	h := newHarness(t)
	h.w.deps.Repairs = &Repairs{}
	ctx := context.Background()
	c := h.readyClipWithoutAWorker()

	if _, err := h.db.EnqueueGIF(ctx, c.ID, h.clock.Now().Unix()); err != nil {
		t.Fatalf("EnqueueGIF: %v", err)
	}
	if _, err := h.db.EnqueuePoster(ctx, c.ID, db.MarshalPosterParams(db.PosterParams{Seconds: 1}), h.clock.Now().Unix()); err != nil {
		t.Fatalf("EnqueuePoster: %v", err)
	}
	// Claim them the way the media loop does, then age them.
	claimed := 0
	for {
		_, _, ok, err := h.db.ClaimMediaJob(ctx, h.clock.Now().Unix())
		if err != nil {
			t.Fatalf("ClaimMediaJob: %v", err)
		}
		if !ok {
			break
		}
		claimed++
	}
	if claimed != 2 {
		t.Fatalf("claimed %d media jobs, want 2", claimed)
	}
	h.clock.Advance(2 * time.Hour)

	if n := h.w.repairOnce(ctx, h.repairLog(), time.Hour); n != 2 {
		t.Fatalf("repairOnce repaired %d media jobs, want 2\nlogs:\n%s", n, h.logs.String())
	}
	for _, kind := range []string{db.KindGIF, db.KindPoster} {
		j, err := h.db.LatestJobOfKind(ctx, c.ID, kind)
		if err != nil {
			t.Fatalf("LatestJobOfKind(%s): %v", kind, err)
		}
		if j.State != db.JobQueued || j.Attempts != 1 {
			t.Errorf("the %s job is state=%s attempts=%d after the repair", kind, j.State, j.Attempts)
		}
	}
	after := h.clipRow(c.ID)
	if after.State != db.ClipReady || after.FailureReason != nil {
		t.Fatalf("the clip is %s/%v: a media repair must leave a ready clip exactly as ready as it was",
			after.State, after.FailureReason)
	}
	if got := h.w.deps.Repairs.Get(); got.Repaired != 2 || got.Requeued != 2 {
		t.Errorf("counters = %+v, want two requeued repairs", got)
	}
	h.assertNoErrorLogs()
}

// readyClipWithoutAWorker makes a `ready` clip with no worker running, so a
// test can leave media jobs `running` without anything executing them.
func (h *harness) readyClipWithoutAWorker() db.Clip {
	h.t.Helper()
	ctx := context.Background()
	c, j := h.newClip("mp4", 1024, true)
	if _, _, ok, err := h.db.ClaimJob(ctx, h.clock.Now().Unix()); err != nil || !ok {
		h.t.Fatalf("ClaimJob: ok=%v err=%v", ok, err)
	}
	src := int64(6000)
	ok, _, err := h.db.FinalizeTranscode(ctx, j.ID, c.ID, db.ReadyFields{
		RenditionKey: "r", RenditionSize: 10, RenditionETag: "re",
		PosterKey: "p", PosterSize: 5, PosterETag: "pe", PosterWidth: 1280, PosterHeight: 720,
		Width: 1280, Height: 720, DurationMS: 6000, FPSNum: 30, FPSDen: 1, HasAudio: true,
		SourceDurationMS: &src,
	}, false, h.clock.Now().Unix())
	if err != nil || !ok {
		h.t.Fatalf("FinalizeTranscode: ok=%v err=%v", ok, err)
	}
	return h.clipRow(c.ID)
}

// The loop is OFF unless JOBS_UNSTICK_AFTER says otherwise, and either way the
// boot log says which — an operator must not have to work that out.
func TestRepairLoopSaysWhetherItIsOn(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		h := newHarness(t) // the harness config leaves JobsUnstickAfter zero
		h.start()
		h.assertLog("INFO", "automatic stuck-job repair is off (JOBS_UNSTICK_AFTER=0)")
		if strings.Contains(h.logs.String(), "repair is on") {
			t.Fatalf("the loop announced itself with the feature off:\n%s", h.logs.String())
		}
	})
	t.Run("on", func(t *testing.T) {
		h := newHarnessCfg(t, func(c *config.Config) {
			c.Transcode.HardTimeout = 20 * time.Second
			c.JobsUnstickAfter = time.Minute
		})
		h.start()
		h.assertLog("INFO", "automatic stuck-job repair is on (JOBS_UNSTICK_AFTER)")
		h.assertLog("INFO", "after=1m0s")
		if strings.Contains(h.logs.String(), "level=WARN") {
			t.Fatalf("an age past the hard timeout warned about nothing:\n%s", h.logs.String())
		}
	})
	t.Run("raised to the sanity floor", func(t *testing.T) {
		h := newHarnessCfg(t, func(c *config.Config) {
			c.Transcode.HardTimeout = 10 * time.Second
			c.JobsUnstickAfter = time.Second
		})
		h.start()
		h.assertLog("INFO", "raised_to_floor=true")
		h.assertLog("INFO", "after=30s")
	})
	t.Run("under the hard timeout warns about a SECOND process", func(t *testing.T) {
		h := newHarnessCfg(t, func(c *config.Config) {
			c.Transcode.HardTimeout = time.Hour
			c.JobsUnstickAfter = time.Minute
		})
		h.start()
		h.assertLog("INFO", "automatic stuck-job repair is on (JOBS_UNSTICK_AFTER)")
		// The warning is about the process this one cannot see into, and says so:
		// its own claims are excluded by claim and not by age.
		h.assertLog("WARN", "JOBS_UNSTICK_AFTER is shorter than TRANSCODE_HARD_TIMEOUT")
		h.assertLog("WARN", "SECOND process")
	})
}

// And with the loop actually running, a stuck row is repaired by the TICK —
// nobody calls repairOnce here, which is what "without a restart" means.
func TestRepairLoopRepairsOnItsOwn(t *testing.T) {
	h := newHarness(t)
	h.w.deps.Repairs = &Repairs{}
	_, j := h.stuckJob(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The plan is built by hand: config.UnstickPlan's tick floor is five
	// seconds, which is right for an instance and far too long for a test.
	mark := h.settles.mark()
	h.w.wg.Add(1)
	go h.w.repairLoop(ctx, config.UnstickPlan{Configured: time.Second, After: time.Second,
		Floor: time.Second, Every: 10 * time.Millisecond})

	// The row goes back to queued BEFORE the counter is recorded (repairLoop:
	// UnstickJob, then Repairs.Record), so a poll that stopped on the row read
	// the counter a few microseconds early — under -race on a loaded box, gate
	// 26a. The repair's SETTLE comes after the counter, the log line and the
	// pass's summary (§20 erratum 331): wait for it, then read once.
	h.settledAfter(mark, func(s Settled) bool { return s.Repair && s.JobID == j.ID })
	cancel()
	if got := h.jobOf(j.ClipID); got.State != db.JobQueued {
		t.Fatalf("the repaired job is %s, want queued", got.State)
	}
	if got := h.w.deps.Repairs.Get(); got.Repaired != 1 {
		t.Fatalf("counters = %+v after the loop's own repair", got)
	}
	h.assertLog("INFO", "repaired a job that was running with no worker")
	h.assertNoErrorLogs()
}
