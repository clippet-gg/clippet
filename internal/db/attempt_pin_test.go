package db

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// jobFacts renders a row's columns BY VALUE (the struct holds pointers, and
// comparing those compares addresses two reads never share), so "the row is
// unchanged" is one string comparison rather than a list of fields somebody
// will forget to extend.
func jobFacts(j Job) string {
	s := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	i := func(p *int64) int64 {
		if p == nil {
			return -1
		}
		return *p
	}
	return fmt.Sprintf("state=%s stage=%s attempts=%d transient=%d run_after=%d updated=%d checkpoint=%s last_error=%s started=%d finished=%d",
		j.State, j.Stage, j.Attempts, j.TransientFailures, j.RunAfter, j.UpdatedAt,
		s(j.CheckpointJSON), s(j.LastError), i(j.StartedAt), i(j.FinishedAt))
}

// The three job-row writes a run makes on its own behalf — SetJobStage,
// SaveCheckpoint, CancelJob — are pinned to the ATTEMPT (§20 erratum 291): a
// handle whose row was repaired out from under it and claimed again writes
// nothing and is told so. Before the pin every one of them matched on the id
// alone, so an attempt an operator had unstuck could write its stage, its
// checkpoint (naming ITS keys) or a `cancelled` over the attempt now running.
func TestAttemptPinnedWritesRefuseAStaleHandle(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	mkClip(t, d, u, 1, nil, false)
	old, _ := claim(t, d, 1000)
	if old.StartedAt == nil || *old.StartedAt != 1000 {
		t.Fatalf("claim stamp = %v, want 1000", old.StartedAt)
	}
	// The operator unsticks it (it is old enough: started_at <= notAfter), which
	// spends one crash attempt and requeues it, and the queue claims it again
	// with a fresh stamp. Two claims can never share one: an unstick needs at
	// least 30 s of age and the stamp is whole seconds.
	if _, err := d.UnstickJob(ctx, old.ID, 1000, Limits{}, 1100); err != nil {
		t.Fatalf("UnstickJob: %v", err)
	}
	cur, _ := claim(t, d, 2000)
	if cur.ID != old.ID || cur.StartedAt == nil || *cur.StartedAt != 2000 {
		t.Fatalf("re-claim = id %d stamp %v, want the same job at 2000", cur.ID, cur.StartedAt)
	}
	before, err := getJob(ctx, d.ro, old.ID)
	if err != nil {
		t.Fatal(err)
	}

	// The old handle: every write refused as a state mismatch, the row untouched.
	if err := d.SetJobStage(ctx, old.ID, *old.StartedAt, "encode", 2001); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("SetJobStage through the old handle: %v, want ErrStateMismatch", err)
	}
	if err := d.SaveCheckpoint(ctx, old.ID, *old.StartedAt, `{"rendition_key":"stale"}`, 2001); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("SaveCheckpoint through the old handle: %v, want ErrStateMismatch", err)
	}
	if err := d.CancelJob(ctx, old.ID, *old.StartedAt, 2001); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("CancelJob through the old handle: %v, want ErrStateMismatch", err)
	}
	after, err := getJob(ctx, d.ro, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if jobFacts(after) != jobFacts(before) {
		t.Fatalf("a refused write changed the row:\n before %s\n after  %s", jobFacts(before), jobFacts(after))
	}
	if after.State != JobRunning || after.Stage != JobStageQueued || after.CheckpointJSON != nil || after.Attempts != 1 {
		t.Fatalf("row after the refusals = %+v, want running, stage queued, no checkpoint, one crash attempt", after)
	}

	// The current handle: the same three writes land.
	if err := d.SetJobStage(ctx, cur.ID, *cur.StartedAt, "encode", 2002); err != nil {
		t.Fatalf("SetJobStage through the current handle: %v", err)
	}
	if err := d.SaveCheckpoint(ctx, cur.ID, *cur.StartedAt, `{"rendition_key":"r"}`, 2003); err != nil {
		t.Fatalf("SaveCheckpoint through the current handle: %v", err)
	}
	got, _ := getJob(ctx, d.ro, cur.ID)
	if got.Stage != "encode" || got.CheckpointJSON == nil || *got.CheckpointJSON != `{"rendition_key":"r"}` {
		t.Fatalf("row after the current handle's writes = %+v", got)
	}
	if err := d.CancelJob(ctx, cur.ID, *cur.StartedAt, 2004); err != nil {
		t.Fatalf("CancelJob through the current handle: %v", err)
	}
	if got, _ = getJob(ctx, d.ro, cur.ID); got.State != JobCancelled || got.FinishedAt == nil || *got.FinishedAt != 2004 {
		t.Fatalf("row after the cancel = %+v, want cancelled at 2004", got)
	}
	// A row that has ENDED is nobody's attempt any more, whatever the stamp.
	if err := d.CancelJob(ctx, cur.ID, *cur.StartedAt, 2005); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("CancelJob on a cancelled row: %v, want ErrStateMismatch", err)
	}
	// An id with no row is still ErrNotFound, not a mismatch, on all three.
	for name, err := range map[string]error{
		"SetJobStage":    d.SetJobStage(ctx, 424242, 1, "x", 1),
		"SaveCheckpoint": d.SaveCheckpoint(ctx, 424242, 1, "{}", 1),
		"CancelJob":      d.CancelJob(ctx, 424242, 1, 1),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s on an unknown id: %v, want ErrNotFound", name, err)
		}
	}
}

// The four ENDINGS a run writes — FailAttempt (and FailAttemptAfter),
// ReleaseJob, RequeueWithBackoff and CompleteJob — are pinned to the ATTEMPT
// too (§20 erratum 293). Erratum 291 left them on `state = 'running'` alone,
// which the transcode path survived because the pinned checkpoint precedes
// every one of them there; the media kinds have no checkpoint, so a run whose
// row an operator had unstuck could still spend the NEW attempt's counter,
// take its job off the queue or mark it `done`.
func TestAttemptPinnedEndingsRefuseAStaleHandle(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	c, _ := readyClip(t, d)
	if _, err := d.EnqueueTrim(ctx, c.ID, MarshalTrimParams(TrimParams{StartMS: 0, EndMS: 2000}), 2000); err != nil {
		t.Fatal(err)
	}
	old, _, ok, err := d.ClaimMediaJob(ctx, 3000)
	if err != nil || !ok {
		t.Fatalf("ClaimMediaJob: ok=%v err=%v", ok, err)
	}
	// The operator unsticks it: one crash attempt, requeued with a backoff.
	if _, err := d.UnstickJob(ctx, old.ID, *old.StartedAt, Limits{}, 3100); err != nil {
		t.Fatalf("UnstickJob: %v", err)
	}
	cur, _, ok, err := d.ClaimMediaJob(ctx, 9000)
	if err != nil || !ok || cur.ID != old.ID {
		t.Fatalf("re-claim = %+v ok=%v err=%v, want the same job", cur, ok, err)
	}
	before, err := getJob(ctx, d.ro, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale := *old.StartedAt
	for name, err := range map[string]error{
		"FailAttempt":        firstErr(d.FailAttempt(ctx, old.ID, stale, c.ID, "x", FailCrash, Limits{}, 9001)),
		"FailAttemptAfter":   firstErr(d.FailAttemptAfter(ctx, old.ID, stale, c.ID, "x", FailCrash, Limits{}, 9001, time.Minute)),
		"ReleaseJob":         d.ReleaseJob(ctx, old.ID, stale, c.ID, 9001),
		"RequeueWithBackoff": d.RequeueWithBackoff(ctx, old.ID, stale, c.ID, "x", time.Minute, 9001),
		"CompleteJob":        d.CompleteJob(ctx, old.ID, stale, JobStageSkipped, "x", 9001),
	} {
		if !errors.Is(err, ErrStateMismatch) {
			t.Fatalf("%s through the old handle: %v, want ErrStateMismatch", name, err)
		}
	}
	after, err := getJob(ctx, d.ro, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if jobFacts(after) != jobFacts(before) {
		t.Fatalf("a refused ending changed the row:\n before %s\n after  %s", jobFacts(before), jobFacts(after))
	}
	if after.State != JobRunning || after.Attempts != 1 {
		t.Fatalf("row after the refusals = %+v, want running with the repair's ONE crash attempt", after)
	}
	if got, _ := getClip(ctx, d.ro, c.ID); got.State != ClipReady {
		t.Fatalf("a refused ending moved the clip to %s", got.State)
	}

	// An id with no row at all is ErrNotFound on every one of them, never a
	// mismatch — the caller's answer to the two is the same, but an operator
	// reading a log line is owed the difference.
	for name, err := range map[string]error{
		"FailAttempt":        firstErr(d.FailAttempt(ctx, 424242, 1, c.ID, "x", FailCrash, Limits{}, 9002)),
		"ReleaseJob":         d.ReleaseJob(ctx, 424242, 1, c.ID, 9002),
		"RequeueWithBackoff": d.RequeueWithBackoff(ctx, 424242, 1, c.ID, "x", time.Minute, 9002),
		"CompleteJob":        d.CompleteJob(ctx, 424242, 1, JobStageSkipped, "x", 9002),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s on an unknown id: %v, want ErrNotFound", name, err)
		}
	}

	// The CURRENT handle ends its own row.
	if err := d.CompleteJob(ctx, cur.ID, *cur.StartedAt, JobStageSkipped, "done by its own attempt", 9003); err != nil {
		t.Fatalf("CompleteJob through the current handle: %v", err)
	}
	if got, _ := getJob(ctx, d.ro, cur.ID); got.State != JobDone || got.FinishedAt == nil || *got.FinishedAt != 9003 {
		t.Fatalf("row after the current handle's complete = %+v, want done at 9003", got)
	}
}

// firstErr drops FailAttempt's Outcome so the two-value calls fit the same
// table as the three one-value ones.
func firstErr(_ Outcome, err error) error { return err }

// CompleteJob refuses a TRANSCODE job whatever stamp it is handed: only
// `transcode` moves a clip's state, and this verb never touches a clip (§5).
// The kind is now part of the pinned statement rather than a read before it,
// so the refusal has to be proved on a row that is otherwise perfectly this
// attempt's.
func TestCompleteJobStillRefusesATranscodeJob(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	mkClip(t, d, u, 1, nil, false)
	j, _ := claim(t, d, 1000)
	if err := d.CompleteJob(ctx, j.ID, *j.StartedAt, JobStageDelivered, "no", 1001); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("CompleteJob on a transcode job: %v, want ErrStateMismatch", err)
	}
	if got, _ := getJob(ctx, d.ro, j.ID); got.State != JobRunning || got.FinishedAt != nil {
		t.Fatalf("the transcode row moved: %+v", got)
	}
}

// The REPAIR is the one caller that is not pinned (`unpinned`), and this is
// the case that needs it: a `running` row with NO started_at. ClaimJob always
// sets one, so such a row is already inconsistent — which is what staleEnough
// counts as stale, and what no pinned write could ever match.
func TestTheRepairStillReachesARunningRowWithNoStamp(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	c, j := mkClip(t, d, u, 1, nil, false)
	if _, err := d.exec(ctx, "UPDATE jobs SET state = 'running', started_at = NULL WHERE id = ?", j.ID); err != nil {
		t.Fatal(err)
	}
	res, err := d.UnstickJob(ctx, j.ID, 5000, Limits{}, 5000)
	if err != nil {
		t.Fatalf("UnstickJob on a stampless running row: %v", err)
	}
	if res.Action != UnstickRequeued {
		t.Fatalf("action = %v, want a requeue (no checkpoint)", res.Action)
	}
	got, _ := getJob(ctx, d.ro, j.ID)
	if got.State != JobQueued || got.Attempts != 1 || got.LastError == nil || *got.LastError != UnstickReason {
		t.Fatalf("row after the repair = %+v, want queued with one crash attempt and the unstick reason", got)
	}
	if gc, _ := getClip(ctx, d.ro, c.ID); gc.State != ClipQueued {
		t.Fatalf("clip = %s, want queued (the transcode coupling)", gc.State)
	}
}
