package db

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// clipDigest renders the columns a job could plausibly touch, dereferenced,
// so a diff is readable (a %+v of a Clip is a wall of pointer addresses).
func clipDigest(c Clip) string {
	reason := "<nil>"
	if c.FailureReason != nil {
		reason = *c.FailureReason
	}
	return fmt.Sprintf("state=%s failure_reason=%s updated_at=%d ready_at=%v views=%d rendition=%v expires_at=%v",
		c.State, reason, c.UpdatedAt, deref(c.ReadyAt), c.Views, derefStr(c.RenditionKey), deref(c.ExpiresAt))
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func derefStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// notifyJobs returns every notify job of a clip, oldest first.
func notifyJobs(t *testing.T, d *DB, clipID string) []Job {
	t.Helper()
	rows, err := d.ro.QueryContext(context.Background(),
		"SELECT "+jobCols+" FROM jobs WHERE clip_id = ? AND kind = 'notify' ORDER BY id", clipID)
	if err != nil {
		t.Fatalf("notify jobs: %v", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, j)
	}
	return out
}

// TestFinalizeEnqueuesNotify: the notify job is created in the SAME commit as
// the ready flip, exactly once, and only for a LISTED clip with a webhook
// configured.
func TestFinalizeEnqueuesNotify(t *testing.T) {
	cases := []struct {
		name     string
		notify   bool
		unlisted bool
		want     int
	}{
		{"configured + listed", true, false, 1},
		{"configured + unlisted", true, true, 0},
		{"not configured + listed", false, false, 0},
		{"not configured + unlisted", false, true, 0},
	}
	for _, c := range cases {
		d := openTest(t)
		ctx := context.Background()
		u := mkUser(t, d, "a")
		mkClip(t, d, u, 1, nil, c.unlisted)
		j, clip := claim(t, d, 10)
		ok, _, err := d.FinalizeTranscode(ctx, j.ID, clip.ID, ready(), c.notify, 30)
		if err != nil || !ok {
			t.Fatalf("%s: Finalize ok=%v err=%v", c.name, ok, err)
		}
		got := notifyJobs(t, d, clip.ID)
		if len(got) != c.want {
			t.Fatalf("%s: %d notify job(s), want %d", c.name, len(got), c.want)
		}
		if c.want == 0 {
			continue
		}
		n := got[0]
		if n.State != JobQueued || n.Stage != JobStageQueued || n.ParamsJSON != "{}" || n.RunAfter != 30 || n.Attempts != 0 {
			t.Fatalf("%s: notify job = %+v", c.name, n)
		}
		// A lost-race finalize (the clip is no longer processing) enqueues
		// nothing: no ready flip, no announcement.
		if ok, _, err := d.FinalizeTranscode(ctx, j.ID, clip.ID, ready(), c.notify, 31); err != nil || ok {
			t.Fatalf("%s: re-finalize ok=%v err=%v", c.name, ok, err)
		}
		if got := notifyJobs(t, d, clip.ID); len(got) != 1 {
			t.Fatalf("%s: re-finalize made %d notify jobs", c.name, len(got))
		}
	}
}

// TestNotifyJobNeverTouchesTheClip is the §5 rule, driven through every path
// that ends a job: the clip must come out of each one byte-identical.
func TestNotifyJobNeverTouchesTheClip(t *testing.T) {
	paths := []struct {
		name string
		run  func(t *testing.T, d *DB, job Job, clipID string)
	}{
		{"FailAttempt crash (requeue)", func(t *testing.T, d *DB, job Job, clipID string) {
			if out, err := d.FailAttempt(context.Background(), job.ID, ClaimStamp(job), clipID, "boom", FailCrash, Limits{}, 100); err != nil || out != OutcomeRequeued {
				t.Fatalf("FailAttempt: %v %v", out, err)
			}
		}},
		{"FailAttempt permanent", func(t *testing.T, d *DB, job Job, clipID string) {
			if out, err := d.FailAttempt(context.Background(), job.ID, ClaimStamp(job), clipID, "HTTP 404", FailPermanent, Limits{}, 100); err != nil || out != OutcomeFailed {
				t.Fatalf("FailAttempt: %v %v", out, err)
			}
		}},
		{"FailAttempt transient", func(t *testing.T, d *DB, job Job, clipID string) {
			if out, err := d.FailAttempt(context.Background(), job.ID, ClaimStamp(job), clipID, "io", FailTransient, Limits{}, 100); err != nil || out != OutcomeRequeued {
				t.Fatalf("FailAttempt: %v %v", out, err)
			}
		}},
		{"ReleaseJob", func(t *testing.T, d *DB, job Job, clipID string) {
			if err := d.ReleaseJob(context.Background(), job.ID, ClaimStamp(job), clipID, 100); err != nil {
				t.Fatalf("ReleaseJob: %v", err)
			}
		}},
		{"CompleteJob", func(t *testing.T, d *DB, job Job, clipID string) {
			if err := d.CompleteJob(context.Background(), job.ID, ClaimStamp(job), JobStageDelivered, "delivered: HTTP 200", 100); err != nil {
				t.Fatalf("CompleteJob: %v", err)
			}
		}},
		{"ReconcileOnStartup", func(t *testing.T, d *DB, job Job, clipID string) {
			if _, _, _, err := d.ReconcileOnStartup(context.Background(), Limits{MaxAttempts: 9, MaxTransient: 9}, 100); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			j, err := getJob(context.Background(), d.ro, job.ID)
			if err != nil || j.State != JobQueued || j.Attempts != 1 {
				t.Fatalf("reconciled notify job = %+v (%v)", j, err)
			}
		}},
	}
	for _, p := range paths {
		d := openTest(t)
		ctx := context.Background()
		u := mkUser(t, d, "a")
		mkClip(t, d, u, 1, nil, false)
		tj, clip := claim(t, d, 10)
		if ok, _, err := d.FinalizeTranscode(ctx, tj.ID, clip.ID, ready(), true, 30); err != nil || !ok {
			t.Fatalf("%s: finalize: %v %v", p.name, ok, err)
		}
		before, _ := d.GetClip(ctx, clip.ID)
		njob, nclip, ok, err := d.ClaimNotifyJob(ctx, 40)
		if err != nil || !ok || njob.Kind != KindNotify {
			t.Fatalf("%s: claim notify: %+v ok=%v err=%v", p.name, njob, ok, err)
		}
		// The claim itself must not have moved the clip either.
		if nclip.State != ClipReady {
			t.Fatalf("%s: ClaimJob moved the clip to %s", p.name, nclip.State)
		}
		p.run(t, d, njob, clip.ID)
		after, _ := d.GetClip(ctx, clip.ID)
		// DeepEqual, not ==: the Clip's nullable columns are pointers, and a
		// second read allocates new ones. Every VALUE must be unchanged.
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("%s: the clip changed\nbefore %s\nafter  %s", p.name, clipDigest(before), clipDigest(after))
		}
	}
}

// TestClaimIsPartitionedByKind: the job queue is THREE queues, one per worker
// loop (§20 errata 67 and 79). No claim can take another's work, whatever the
// id order — which is the whole point: an older queued notify must not be
// claimed ahead of a newer transcode somebody is waiting on, and a trim that
// takes minutes must not be in front of an announcement that takes 200 ms.
func TestClaimIsPartitionedByKind(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	// Clip A finishes first, so its notify job is OLDER than clip B's
	// transcode job. Under one FIFO it would be claimed first.
	a, _ := mkClip(t, d, u, 1, nil, false)
	ja, _ := claim(t, d, 10)
	if ok, _, err := d.FinalizeTranscode(ctx, ja.ID, a.ID, ready(), true, 20); err != nil || !ok {
		t.Fatalf("finalize: %v %v", ok, err)
	}
	b, _ := mkClip(t, d, u, 30, nil, false)
	notifyJob := notifyJobs(t, d, a.ID)[0]
	bJob, err := d.GetLatestJob(ctx, b.ID)
	if err != nil || bJob.Kind != KindTranscode || bJob.ID <= notifyJob.ID {
		t.Fatalf("precondition: want a transcode job newer than notify job %d, got %+v (%v)", notifyJob.ID, bJob, err)
	}

	// The transcode loop skips the older notify and takes the transcode.
	got, clip, ok, err := d.ClaimJob(ctx, 40)
	if err != nil || !ok {
		t.Fatalf("ClaimJob: ok=%v err=%v", ok, err)
	}
	if got.Kind != KindTranscode || got.ID != bJob.ID || clip.ID != b.ID {
		t.Fatalf("ClaimJob took %+v, want the transcode job %d of clip %s", got, bJob.ID, b.ID)
	}
	// Nothing else is claimable for it: the notify job is not its work.
	if j, _, ok, err := d.ClaimJob(ctx, 40); ok || err != nil {
		t.Fatalf("ClaimJob claimed %+v (ok=%v err=%v), want nothing", j, ok, err)
	}

	// The media loop cannot see the notify job at all.
	if j, _, ok, err := d.ClaimMediaJob(ctx, 40); ok || err != nil {
		t.Fatalf("ClaimMediaJob claimed %+v (ok=%v err=%v), want nothing: a notify is not media work", j, ok, err)
	}

	// The notify loop takes the notify and never a transcode.
	got, clip, ok, err = d.ClaimNotifyJob(ctx, 40)
	if err != nil || !ok {
		t.Fatalf("ClaimNotifyJob: ok=%v err=%v", ok, err)
	}
	if got.Kind != KindNotify || got.ID != notifyJob.ID || clip.ID != a.ID {
		t.Fatalf("ClaimNotifyJob took %+v, want notify job %d of clip %s", got, notifyJob.ID, a.ID)
	}
	if j, _, ok, err := d.ClaimNotifyJob(ctx, 40); ok || err != nil {
		t.Fatalf("ClaimNotifyJob claimed %+v (ok=%v err=%v), want nothing", j, ok, err)
	}

	// The two media kinds belong to the media loop, and to nothing else.
	for _, kind := range MediaKinds() {
		c, _ := mkClip(t, d, u, 50, nil, false)
		if _, err := d.exec(ctx, "UPDATE jobs SET kind = ? WHERE clip_id = ?", kind, c.ID); err != nil {
			t.Fatalf("forge kind: %v", err)
		}
		if j, _, ok, err := d.ClaimJob(ctx, 60); ok || err != nil {
			t.Fatalf("ClaimJob claimed a %q job: %+v (ok=%v err=%v)", kind, j, ok, err)
		}
		if j, _, ok, err := d.ClaimNotifyJob(ctx, 60); ok || err != nil {
			t.Fatalf("ClaimNotifyJob claimed a %q job: %+v (ok=%v err=%v)", kind, j, ok, err)
		}
		j, _, ok, err := d.ClaimMediaJob(ctx, 60)
		if err != nil || !ok || j.Kind != kind {
			t.Fatalf("ClaimMediaJob = %+v ok=%v err=%v, want the %q job", j, ok, err, kind)
		}
	}

	// An UNRECOGNISED kind belongs to the notify loop, by exclusion, so the
	// worker fails it loudly instead of it sitting queued for ever.
	x, _ := mkClip(t, d, u, 70, nil, false)
	if _, err := d.exec(ctx, "UPDATE jobs SET kind = 'from-the-future' WHERE clip_id = ?", x.ID); err != nil {
		t.Fatalf("forge kind: %v", err)
	}
	if j, _, ok, err := d.ClaimMediaJob(ctx, 80); ok || err != nil {
		t.Fatalf("ClaimMediaJob claimed an unknown kind: %+v (ok=%v err=%v)", j, ok, err)
	}
	j, _, ok, err := d.ClaimNotifyJob(ctx, 80)
	if err != nil || !ok || j.Kind != "from-the-future" {
		t.Fatalf("ClaimNotifyJob = %+v ok=%v err=%v, want the unknown 'from-the-future' job", j, ok, err)
	}
}

// TestNotifyAttemptCapIsThree: three crash-class failures whatever the
// transcode caps say, and the clip stays ready throughout.
func TestNotifyAttemptCapIsThree(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	mkClip(t, d, u, 1, nil, false)
	tj, clip := claim(t, d, 10)
	if ok, _, err := d.FinalizeTranscode(ctx, tj.ID, clip.ID, ready(), true, 30); err != nil || !ok {
		t.Fatalf("finalize: %v %v", ok, err)
	}
	// Deliberately generous transcode caps: a notify job must ignore them.
	lim := Limits{MaxAttempts: 99, MaxTransient: 99}
	now := int64(40)
	for attempt := 1; attempt <= NotifyMaxAttempts; attempt++ {
		j, _, ok, err := d.ClaimNotifyJob(ctx, now)
		if err != nil || !ok || j.Kind != KindNotify {
			t.Fatalf("attempt %d: claim: %+v ok=%v err=%v", attempt, j, ok, err)
		}
		out, err := d.FailAttempt(ctx, j.ID, ClaimStamp(j), clip.ID, "HTTP 500", FailCrash, lim, now)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		want := OutcomeRequeued
		if attempt == NotifyMaxAttempts {
			want = OutcomeFailed
		}
		if out != want {
			t.Fatalf("attempt %d: outcome %v, want %v", attempt, out, want)
		}
		j2, _ := getJob(ctx, d.ro, j.ID)
		if j2.Attempts != attempt {
			t.Fatalf("attempt %d: attempts = %d", attempt, j2.Attempts)
		}
		now = j2.RunAfter
	}
	j, _ := d.GetLatestJob(ctx, clip.ID)
	if j.State != JobFailed {
		t.Fatalf("job = %+v, want failed at the cap", j)
	}
	c, _ := d.GetClip(ctx, clip.ID)
	if c.State != ClipReady || c.FailureReason != nil {
		t.Fatalf("clip = %s / %v — a failed announcement is not a failed clip", c.State, c.FailureReason)
	}
}

// TestFailAttemptAfterFloorsTheBackoff: a Retry-After wins when it is longer
// than the computed backoff, and never shortens it.
func TestFailAttemptAfterFloorsTheBackoff(t *testing.T) {
	cases := []struct {
		name     string
		minDelay time.Duration
		want     int64 // run_after - now
	}{
		{"no floor", 0, 30},
		{"shorter than the backoff", 5 * time.Second, 30},
		{"longer than the backoff", 90 * time.Second, 90},
	}
	for _, c := range cases {
		d := openTest(t)
		ctx := context.Background()
		u := mkUser(t, d, "a")
		mkClip(t, d, u, 1, nil, false)
		tj, clip := claim(t, d, 10)
		if ok, _, err := d.FinalizeTranscode(ctx, tj.ID, clip.ID, ready(), true, 30); err != nil || !ok {
			t.Fatalf("%s: finalize: %v %v", c.name, ok, err)
		}
		j, _, _, err := d.ClaimNotifyJob(ctx, 40)
		if err != nil {
			t.Fatalf("%s: claim: %v", c.name, err)
		}
		if _, err := d.FailAttemptAfter(ctx, j.ID, ClaimStamp(j), clip.ID, "429", FailCrash, Limits{}, 1000, c.minDelay); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got, _ := getJob(ctx, d.ro, j.ID)
		if got.RunAfter-1000 != c.want {
			t.Errorf("%s: run_after = now+%d, want now+%d", c.name, got.RunAfter-1000, c.want)
		}
	}
}

// TestCompleteJobRefusesTranscode: the §5 coupling can only be written by
// FinalizeTranscode, never bypassed through the generic completion.
func TestCompleteJobRefusesTranscode(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	mkClip(t, d, u, 1, nil, false)
	j, c := claim(t, d, 10)
	if err := d.CompleteJob(ctx, j.ID, ClaimStamp(j), JobStageDelivered, "nope", 20); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("CompleteJob on a transcode job = %v, want ErrStateMismatch", err)
	}
	gj, _ := d.GetLatestJob(ctx, c.ID)
	gc, _ := d.GetClip(ctx, c.ID)
	if gj.State != JobRunning || gc.State != ClipProcessing {
		t.Fatalf("job = %s clip = %s", gj.State, gc.State)
	}
	if err := d.CompleteJob(ctx, 99999, 0, JobStageSkipped, "", 20); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CompleteJob on a missing job = %v, want ErrNotFound", err)
	}
}

func TestJobKindCounts(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	if got, err := d.JobKindCounts(ctx); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	mkClip(t, d, u, 1, nil, false)
	mkClip(t, d, u, 1, nil, false)
	j, c := claim(t, d, 10)
	if ok, _, err := d.FinalizeTranscode(ctx, j.ID, c.ID, ready(), true, 30); err != nil || !ok {
		t.Fatalf("finalize: %v %v", ok, err)
	}
	got, err := d.JobKindCounts(ctx)
	if err != nil || got[KindTranscode] != 2 || got[KindNotify] != 1 {
		t.Fatalf("JobKindCounts = %v (%v)", got, err)
	}
}
