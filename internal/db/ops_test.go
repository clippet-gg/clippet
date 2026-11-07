package db

import (
	"context"
	"errors"
	"testing"
)

// enqueueKind puts a job of an arbitrary kind on a clip, so the unstick tests
// can prove the §5 rule from BOTH sides: a transcode job moves its clip, and
// every other kind must leave it exactly as it was.
func enqueueKind(t *testing.T, d *DB, clipID, kind string, now int64) Job {
	t.Helper()
	res, err := d.rw.ExecContext(context.Background(), `
INSERT INTO jobs (clip_id, kind, state, stage, run_after, created_at, updated_at)
VALUES (?, ?, 'queued', ?, ?, ?, ?)`, clipID, kind, JobStageQueued, now, now, now)
	if err != nil {
		t.Fatalf("insert %s job: %v", kind, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	j, err := getJob(context.Background(), d.ro, id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// markRunning claims a job the way the worker would, without going through a
// loop-specific claim: state running, started_at = at.
func markRunning(t *testing.T, d *DB, jobID, at int64) {
	t.Helper()
	if _, err := d.rw.ExecContext(context.Background(),
		"UPDATE jobs SET state = 'running', started_at = ?, updated_at = ? WHERE id = ?", at, at, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
}

func TestListJobsFilters(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	c1, j1 := mkClip(t, d, u, 1000, nil, false)
	c2, _ := mkClip(t, d, u, 1100, nil, false)
	notify := enqueueKind(t, d, c2.ID, KindNotify, 1200)

	all, err := d.ListJobs(ctx, JobQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("ListJobs: %d rows, want 3", len(all))
	}
	// Newest first, by id.
	if all[0].ID != notify.ID {
		t.Fatalf("ListJobs[0] = #%d, want the newest #%d", all[0].ID, notify.ID)
	}
	transcodes, err := d.ListJobs(ctx, JobQuery{Kind: KindTranscode})
	if err != nil || len(transcodes) != 2 {
		t.Fatalf("kind filter: %d rows (%v), want 2", len(transcodes), err)
	}
	mine, err := d.ListJobs(ctx, JobQuery{ClipID: c1.ID})
	if err != nil || len(mine) != 1 || mine[0].ID != j1.ID {
		t.Fatalf("clip filter: %v %v", mine, err)
	}
	queued, err := d.ListJobs(ctx, JobQuery{State: JobQueued})
	if err != nil || len(queued) != 3 {
		t.Fatalf("state filter: %d rows (%v), want 3", len(queued), err)
	}
	// Since bounds updated_at from below: only the notify job was touched at 1200.
	recent, err := d.ListJobs(ctx, JobQuery{Since: 1200})
	if err != nil || len(recent) != 1 || recent[0].ID != notify.ID {
		t.Fatalf("since filter: %v %v", recent, err)
	}
	one, err := d.ListJobs(ctx, JobQuery{Limit: 1})
	if err != nil || len(one) != 1 || one[0].ID != notify.ID {
		t.Fatalf("limit: %v %v", one, err)
	}
	// A limit past the ceiling is clamped rather than refused.
	if rows, err := d.ListJobs(ctx, JobQuery{Limit: MaxJobListLimit + 1000}); err != nil || len(rows) != 3 {
		t.Fatalf("clamped limit: %d rows (%v)", len(rows), err)
	}
	// Nothing matches: an empty result, never an error.
	if rows, err := d.ListJobs(ctx, JobQuery{Kind: KindGIF}); err != nil || len(rows) != 0 {
		t.Fatalf("no matches: %d rows (%v)", len(rows), err)
	}
}

func TestJobKindStateCounts(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	c, _ := mkClip(t, d, u, 1000, nil, false)
	enqueueKind(t, d, c.ID, KindNotify, 1000)
	counts, err := d.JobKindStateCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts[KindTranscode][JobQueued] != 1 || counts[KindNotify][JobQueued] != 1 {
		t.Fatalf("counts = %v", counts)
	}
	if counts[KindGIF][JobDone] != 0 {
		t.Fatalf("a kind with no rows must read 0, got %v", counts[KindGIF])
	}
}

// ClipIDsWithLiveJobs is the interlock `admin orphans --delete` asks about
// before it removes an object no column names (§20 erratum 150): a job's
// outputs can be referenced by its CHECKPOINT alone for as long as the job
// stays claimable. Every kind that writes objects counts; `notify`, which
// writes none, is the one exclusion.
func TestClipIDsWithLiveJobs(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	setState := func(jobID int64, state string) {
		t.Helper()
		if _, err := d.rw.ExecContext(ctx, "UPDATE jobs SET state = ? WHERE id = ?", state, jobID); err != nil {
			t.Fatal(err)
		}
	}
	queued, _ := mkClip(t, d, u, 1000, nil, false)
	running, jr := mkClip(t, d, u, 1100, nil, false)
	markRunning(t, d, jr.ID, 1100)
	done, jd := mkClip(t, d, u, 1200, nil, false)
	setState(jd.ID, JobDone)
	// A finished transcode, then a TRIM that is still queued: the clip is
	// `ready` and yet somebody is about to write objects under it.
	trimmed, jt := mkClip(t, d, u, 1300, nil, false)
	setState(jt.ID, JobDone)
	enqueueKind(t, d, trimmed.ID, KindTrim, 1300)
	// Terminal states of every shape are not live.
	failed, jf := mkClip(t, d, u, 1400, nil, false)
	setState(jf.ID, JobFailed)
	cancelled, jc := mkClip(t, d, u, 1500, nil, false)
	setState(jc.ID, JobCancelled)

	live, err := d.ClipIDsWithLiveJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{queued.ID, running.ID, trimmed.ID} {
		if !live[id] {
			t.Errorf("clip %s has a claimable job and is not in the live set: %v", id, live)
		}
	}
	for _, id := range []string{done.ID, failed.ID, cancelled.ID} {
		if live[id] {
			t.Errorf("clip %s has only terminal jobs and is in the live set: %v", id, live)
		}
	}
	if len(live) != 3 {
		t.Fatalf("live set = %v, want exactly the three claimable clips", live)
	}
	// A clip with two live jobs appears once, not twice.
	enqueueKind(t, d, queued.ID, KindGIF, 1600)
	if live, err := d.ClipIDsWithLiveJobs(ctx); err != nil || len(live) != 3 || !live[queued.ID] {
		t.Fatalf("a second live job changed the set: %v %v", live, err)
	}

	// NOTIFY is the one non-terminal kind that cannot reference an object: it
	// posts to a webhook and writes nothing to the bucket. A ready clip whose
	// only live job is an announcement — the longest-lived queued row there is
	// when Discord is down — must NOT be shielded from the extras sweep.
	announced, ja := mkClip(t, d, u, 1700, nil, false)
	setState(ja.ID, JobDone)
	enqueueKind(t, d, announced.ID, KindNotify, 1700)
	live, err = d.ClipIDsWithLiveJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if live[announced.ID] {
		t.Errorf("a queued notify job put clip %s in the live set: %v", announced.ID, live)
	}
	if len(live) != 3 {
		t.Fatalf("live set = %v, want the same three clips", live)
	}
	// And it is an EXCLUSION, not an allowlist: a kind nobody has written yet
	// is protected by default.
	future, jfu := mkClip(t, d, u, 1800, nil, false)
	setState(jfu.ID, JobDone)
	enqueueKind(t, d, future.ID, "some-future-kind", 1800)
	if live, err := d.ClipIDsWithLiveJobs(ctx); err != nil || !live[future.ID] {
		t.Fatalf("an unknown live kind is not protected: %v %v", live, err)
	}
}

func TestQueueHealthCountsAndAges(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	now := int64(100000)

	// Nothing at all: every number is 0, and the ages are 0 rather than "now".
	if q, err := d.QueueHealth(ctx, now); err != nil || q != (QueueHealth{}) {
		t.Fatalf("empty queue: %+v %v", q, err)
	}

	c1, j1 := mkClip(t, d, u, now-500, nil, false) // queued, run_after = now-500
	c2, j2 := mkClip(t, d, u, now-300, nil, false)
	markRunning(t, d, j2.ID, now-200)
	// A job waiting out a backoff is deliberately NOT counted as an old queued
	// job: it is not claimable yet, so counting it would report a stuck queue
	// every time a storage blip requeued something.
	c3, j3 := mkClip(t, d, u, now-400, nil, false)
	if _, err := d.rw.ExecContext(ctx, "UPDATE jobs SET run_after = ? WHERE id = ?", now+600, j3.ID); err != nil {
		t.Fatal(err)
	}

	q, err := d.QueueHealth(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if q.Queued != 2 || q.Running != 1 {
		t.Fatalf("counts: %+v", q)
	}
	if q.OldestQueuedAgeS != 500 {
		t.Fatalf("oldest claimable queued age = %d, want 500 (the backed-off job must not count)", q.OldestQueuedAgeS)
	}
	if q.LongestRunningAgeS != 200 {
		t.Fatalf("longest running age = %d, want 200", q.LongestRunningAgeS)
	}
	if q.Failed24h != 0 {
		t.Fatalf("failed_24h = %d, want 0", q.Failed24h)
	}

	// Fail the running one: it leaves running, joins failed_24h, and the clip
	// follows because it is a transcode (§5).
	if out, err := d.FailAttempt(ctx, j2.ID, now-200, c2.ID, "bad file", FailPermanent, Limits{}, now-100); err != nil || out != OutcomeFailed {
		t.Fatalf("FailAttempt: %v %v", out, err)
	}
	q, err = d.QueueHealth(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if q.Running != 0 || q.Failed24h != 1 || q.LongestRunningAgeS != 0 {
		t.Fatalf("after a failure: %+v", q)
	}
	// A failure older than 24 h drops out of the window.
	if q, err := d.QueueHealth(ctx, now+90000); err != nil || q.Failed24h != 0 {
		t.Fatalf("failed_24h past the window: %+v %v", q, err)
	}
	// A clock that stepped backwards reads as age 0, never as a negative age.
	if q, err := d.QueueHealth(ctx, now-10000); err != nil || q.OldestQueuedAgeS != 0 {
		t.Fatalf("backwards clock: %+v %v", q, err)
	}
	_ = c1
	_ = c3
	_ = j1
}

func TestUnstickJobRequeuesATranscodeAndMovesItsClip(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	now := int64(100000)
	c, j := mkClip(t, d, u, now-10000, nil, false)
	// Claim it the way the worker does, so the clip really is `processing`.
	if _, _, ok, err := d.ClaimJob(ctx, now-9000); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if got := clipState(t, d, c.ID); got != ClipProcessing {
		t.Fatalf("clip state after claim = %s", got)
	}

	// Too young: the age guard is the ONLY thing standing between this command
	// and a live encode, so a refusal must change nothing at all.
	if _, err := d.UnstickJob(ctx, j.ID, now-9500, Limits{}, now); !errors.Is(err, ErrNotStale) {
		t.Fatalf("young job: %v, want ErrNotStale", err)
	}
	if got := jobState(t, d, j.ID); got != JobRunning {
		t.Fatalf("a refused unstick changed the job state to %s", got)
	}
	if got := clipState(t, d, c.ID); got != ClipProcessing {
		t.Fatalf("a refused unstick changed the clip state to %s", got)
	}

	res, err := d.UnstickJob(ctx, j.ID, now-1000, Limits{}, now)
	if err != nil {
		t.Fatalf("UnstickJob: %v", err)
	}
	if res.Action != UnstickRequeued || res.Kind != KindTranscode || res.ClipStateAfter != ClipQueued {
		t.Fatalf("result = %+v, want a requeued transcode whose clip is queued", res)
	}
	after, err := getJob(ctx, d.ro, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != JobQueued || after.Attempts != 1 || after.LastError == nil || *after.LastError != UnstickReason {
		t.Fatalf("job after unstick: state=%s attempts=%d err=%v", after.State, after.Attempts, after.LastError)
	}
	if after.RunAfter <= now {
		t.Fatalf("run_after = %d, want a backoff past now=%d", after.RunAfter, now)
	}
	// A second call is a clean refusal, not a second attempt spent.
	if _, err := d.UnstickJob(ctx, j.ID, now, Limits{}, now); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("second unstick: %v, want ErrStateMismatch", err)
	}
	if again, _ := getJob(ctx, d.ro, j.ID); again.Attempts != 1 {
		t.Fatalf("a refused unstick spent an attempt: %d", again.Attempts)
	}
}

func TestUnstickReleasesACheckpointedJobWithoutSpendingAnAttempt(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	now := int64(100000)
	c, j := mkClip(t, d, u, now-10000, nil, false)
	cj, _, ok, err := d.ClaimJob(ctx, now-9000)
	if err != nil || !ok || cj.ID != j.ID {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := d.SaveCheckpoint(ctx, cj.ID, *cj.StartedAt, `{"rendition_key":"r"}`, now-8000); err != nil {
		t.Fatal(err)
	}
	res, err := d.UnstickJob(ctx, j.ID, now-1000, Limits{}, now)
	if err != nil {
		t.Fatalf("UnstickJob: %v", err)
	}
	if res.Action != UnstickReleased || res.ClipStateAfter != ClipQueued {
		t.Fatalf("result = %+v, want a released job whose clip went back to queued", res)
	}
	after, err := getJob(ctx, d.ro, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != JobQueued || after.Attempts != 0 || after.TransientFailures != 0 {
		t.Fatalf("a checkpointed release spent a counter: %+v", after)
	}
	if after.CheckpointJSON == nil {
		t.Fatal("the checkpoint was dropped, so the retry is no longer finalize-only")
	}
	_ = c
}

func TestUnstickNeverMovesAClipForANonTranscodeJob(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	now := int64(100000)
	c, j := mkClip(t, d, u, now-10000, nil, false)
	if _, _, ok, err := d.ClaimJob(ctx, now-9500); err != nil || !ok {
		t.Fatal(err)
	}
	if ok, _, err := d.FinalizeTranscode(ctx, j.ID, c.ID, ready(), false, now-9000); err != nil || !ok {
		t.Fatalf("finalize: %v %v", ok, err)
	}
	// A trim and a gif run against a clip that is READY for the whole run: the
	// §5 rule is that unsticking one must not touch clips.state, whatever
	// happens to the job.
	for _, kind := range []string{KindTrim, KindGIF, KindNotify} {
		mj := enqueueKind(t, d, c.ID, kind, now-5000)
		markRunning(t, d, mj.ID, now-5000)
		res, err := d.UnstickJob(ctx, mj.ID, now-1000, Limits{}, now)
		if err != nil {
			t.Fatalf("%s: UnstickJob: %v", kind, err)
		}
		if res.ClipStateAfter != ClipReady {
			t.Fatalf("%s: the clip left `ready` (now %s) — only a transcode job may move it (§5)", kind, res.ClipStateAfter)
		}
		if got := clipState(t, d, c.ID); got != ClipReady {
			t.Fatalf("%s: clip state = %s, want ready", kind, got)
		}
	}
}

func TestUnstickStaleJobsSelectsOnlyTheOldOnes(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	now := int64(100000)
	old, oldJob := mkClip(t, d, u, now-10000, nil, false)
	fresh, freshJob := mkClip(t, d, u, now-10, nil, false)
	markRunning(t, d, oldJob.ID, now-9000)
	markRunning(t, d, freshJob.ID, now-5)

	results, err := d.UnstickStaleJobs(ctx, now-1000, Limits{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].JobID != oldJob.ID {
		t.Fatalf("results = %+v, want only the old job #%d", results, oldJob.ID)
	}
	if got := jobState(t, d, freshJob.ID); got != JobRunning {
		t.Fatalf("the fresh job was repaired anyway: %s", got)
	}
	if got := jobState(t, d, oldJob.ID); got != JobQueued {
		t.Fatalf("the old job = %s, want queued", got)
	}
	// A second sweep finds nothing and is a clean empty result.
	if again, err := d.UnstickStaleJobs(ctx, now-1000, Limits{}, now); err != nil || len(again) != 0 {
		t.Fatalf("second sweep: %+v %v", again, err)
	}
	_, _ = old, fresh
}

func TestUnstickJobErrorsForMissingAndNonRunning(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	_, j := mkClip(t, d, u, 1000, nil, false)
	if _, err := d.UnstickJob(ctx, 99999, 2000, Limits{}, 2000); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing job: %v, want ErrNotFound", err)
	}
	// Queued, not running: there is nothing stuck about it.
	if _, err := d.UnstickJob(ctx, j.ID, 2000, Limits{}, 2000); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("queued job: %v, want ErrStateMismatch", err)
	}
}

func TestUnstickExhaustsTheAttemptBudgetIntoAFailedClip(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	now := int64(100000)
	c, j := mkClip(t, d, u, now-10000, nil, false)
	lim := Limits{MaxAttempts: 1, MaxTransient: 10}
	if _, _, ok, err := d.ClaimJob(ctx, now-9000); err != nil || !ok {
		t.Fatal(err)
	}
	res, err := d.UnstickJob(ctx, j.ID, now-1000, lim, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != UnstickFailed || res.ClipStateAfter != ClipFailed {
		t.Fatalf("result = %+v, want the last attempt to fail the clip", res)
	}
	// The public failure reason must say what happened and nothing about this
	// instance: the share page renders it to any viewer.
	got, err := d.GetClip(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FailureReason == nil || *got.FailureReason != UnstickReason {
		t.Fatalf("failure_reason = %v, want %q", got.FailureReason, UnstickReason)
	}
}

func TestIntegrityCheckOnALiveDatabase(t *testing.T) {
	d := openTest(t)
	got, err := d.IntegrityCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "ok" {
		t.Fatalf("IntegrityCheck = %q, want ok", got)
	}
}

func clipState(t *testing.T, d *DB, id string) string {
	t.Helper()
	c, err := d.GetClip(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return c.State
}

func jobState(t *testing.T, d *DB, id int64) string {
	t.Helper()
	j, err := getJob(context.Background(), d.ro, id)
	if err != nil {
		t.Fatal(err)
	}
	return j.State
}
