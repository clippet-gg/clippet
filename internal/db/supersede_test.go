package db

import (
	"context"
	"errors"
	"testing"
)

// liveJobsOfKind lists the clip's queued and running jobs of one kind.
func liveJobsOfKind(t *testing.T, d *DB, clipID, kind string) []Job {
	t.Helper()
	rows, err := d.ro.QueryContext(context.Background(),
		"SELECT "+jobCols+" FROM jobs WHERE clip_id = ? AND kind = ? AND state IN ('queued','running') ORDER BY id",
		clipID, kind)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, j)
	}
	return out
}

// runningTierJob queues and claims one job of kind for the clip, so the row is
// `running` exactly as a worker holding it would leave it.
func runningTierJob(t *testing.T, d *DB, clipID, kind string, now int64) Job {
	t.Helper()
	ctx := context.Background()
	var err error
	switch kind {
	case KindSocial:
		_, err = d.EnqueueSocial(ctx, clipID, now)
	case KindFull:
		_, err = d.EnqueueFull(ctx, clipID, now)
	}
	if err != nil {
		t.Fatalf("enqueue %s: %v", kind, err)
	}
	j, _, ok, err := d.ClaimMediaJob(ctx, now)
	if err != nil || !ok || j.Kind != kind {
		t.Fatalf("claim = %+v ok=%v err=%v, want a running %s job", j, ok, err, kind)
	}
	return j
}

// TestSwapSupersedesARunningTierJob is erratum 237's open ending, closed (237
// (a)): a trim or a replacement that commits while a tier job of the kind it
// queues is RUNNING used to leave that job alone — and the job, encoding the
// rendition the commit had just replaced, then lost its ETag race, so the new
// rendition was never given a card copy (or a Full) at all. The commit now ends
// the stale attempt's row (`cancelled`, the same hand MarkDeleting uses) and
// queues the replacement, so the clip keeps a job behind its `encoding` and the
// stale worker's every ending is erratum 293's "not this attempt's row".
func TestSwapSupersedesARunningTierJob(t *testing.T) {
	ctx := context.Background()

	assertSuperseded := func(t *testing.T, d *DB, clipID, kind string, stale Job) {
		t.Helper()
		got, err := getJob(ctx, d.ro, stale.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != JobCancelled {
			t.Fatalf("the stale %s job is %q after the commit, want %q", kind, got.State, JobCancelled)
		}
		live := liveJobsOfKind(t, d, clipID, kind)
		if len(live) != 1 || live[0].State != JobQueued || live[0].ID == stale.ID {
			t.Fatalf("live %s jobs after the commit = %+v, want ONE fresh queued job", kind, live)
		}
		// The stale attempt's endings, each refused as not its row (erratum 293)…
		if err := d.CompleteJob(ctx, stale.ID, ClaimStamp(stale), JobStageSkipped, "skipped: the rendition changed", 3000); !errors.Is(err, ErrStateMismatch) {
			t.Fatalf("CompleteJob on the superseded row = %v, want ErrStateMismatch", err)
		}
		if _, err := d.FailAttempt(ctx, stale.ID, ClaimStamp(stale), clipID, "boom", FailPermanent, Limits{}, 3000); !errors.Is(err, ErrStateMismatch) {
			t.Fatalf("FailAttempt on the superseded row = %v, want ErrStateMismatch", err)
		}
		// …and its tier footnote writes nothing: the fresh job keeps the promise.
		if err := d.SettleTierEncoding(ctx, clipID, kind, TierSkipped, 3000); err != nil {
			t.Fatal(err)
		}
		if err := d.SettleTierEncoding(ctx, clipID, kind, TierFailed, 3000); err != nil {
			t.Fatal(err)
		}
		c, err := d.GetClip(ctx, clipID)
		if err != nil {
			t.Fatal(err)
		}
		state := c.LightState
		if kind == KindFull {
			state = c.FullState
		}
		if state != TierEncoding {
			t.Fatalf("the %s tier reads %q after the stale job's endings, want %q (a job is coming)", kind, state, TierEncoding)
		}
	}

	t.Run("a trim over a running card copy", func(t *testing.T) {
		d := openTest(t)
		c, _ := readyClip(t, d)
		stale := runningTierJob(t, d, c.ID, KindSocial, 2000)
		trim := mustEnqueueTrim(t, d, c.ID, 2001)
		f := swapFields()
		f.QueueSocial = true
		f.Tiers = TrimTierFields(true, 1280, 720, 30, 1, "h264", true)
		if _, ok, _, err := d.SwapRendition(ctx, trim, c.ID, f, TrimParams{StartMS: 1000, EndMS: 3000}, 2002); err != nil || !ok {
			t.Fatalf("SwapRendition = %v, %v", ok, err)
		}
		// The stale job's commit, against the rendition it read when it started.
		sw, err := d.SetSocial(ctx, stale.ID, c.ID, SocialFields{Key: "s-stale", Size: 4, ETag: "se", Width: 640, Height: 360}, "re", 2003)
		if err != nil || sw.OK {
			t.Fatalf("the stale commit = %+v, %v; want refused", sw, err)
		}
		assertSuperseded(t, d, c.ID, KindSocial, stale)
	})

	t.Run("a trim that queues no card copy leaves the running job alone", func(t *testing.T) {
		d := openTest(t)
		c, _ := readyClip(t, d)
		stale := runningTierJob(t, d, c.ID, KindSocial, 2000)
		trim := mustEnqueueTrim(t, d, c.ID, 2001)
		f := swapFields()
		f.Tiers = TrimTierFields(true, 1280, 720, 30, 1, "h264", false)
		if _, ok, _, err := d.SwapRendition(ctx, trim, c.ID, f, TrimParams{StartMS: 1000, EndMS: 3000}, 2002); err != nil || !ok {
			t.Fatalf("SwapRendition = %v, %v", ok, err)
		}
		got, _ := getJob(ctx, d.ro, stale.ID)
		if got.State != JobRunning {
			t.Fatalf("the running job is %q after a commit that queued nothing, want running", got.State)
		}
	})

	t.Run("a queued card copy is kept, not doubled", func(t *testing.T) {
		d := openTest(t)
		c, _ := readyClip(t, d)
		trim := mustEnqueueTrim(t, d, c.ID, 2000)       // the trim is running…
		queued, err := d.EnqueueSocial(ctx, c.ID, 2001) // …and the card copy still queued
		if err != nil {
			t.Fatal(err)
		}
		f := swapFields()
		f.QueueSocial = true
		f.Tiers = TrimTierFields(true, 1280, 720, 30, 1, "h264", true)
		if _, ok, _, err := d.SwapRendition(ctx, trim, c.ID, f, TrimParams{StartMS: 1000, EndMS: 3000}, 2002); err != nil || !ok {
			t.Fatalf("SwapRendition = %v, %v", ok, err)
		}
		live := liveJobsOfKind(t, d, c.ID, KindSocial)
		if len(live) != 1 || live[0].ID != queued.ID {
			t.Fatalf("live social jobs = %+v, want the one already queued (it reads the new rendition)", live)
		}
	})

	t.Run("a replacement over a running card copy and a running Full", func(t *testing.T) {
		d := openTest(t)
		c, _ := readyClip(t, d)
		staleSocial := runningTierJob(t, d, c.ID, KindSocial, 2000)
		staleFull := runningTierJob(t, d, c.ID, KindFull, 2000)
		job, err := d.EnqueueReplace(ctx, c.ID, MarshalReplaceParams(ReplaceParams{
			SourceKey: newSource().Key, SourceETag: newSource().ETag, SourceSize: newSource().Size, Spool: ".upload-x-1"}), 2001)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := d.ClaimMediaJob(ctx, 2001); err != nil {
			t.Fatal(err)
		}
		f := replaceFields()
		f.QueueSocial = true
		f.Tiers.FullState, f.Tiers.LightState, f.Tiers.QueueFull = TierEncoding, TierEncoding, true
		if _, ok, err := d.SwapSource(ctx, job.ID, c.ID, f, newSource(), 2002); err != nil || !ok {
			t.Fatalf("SwapSource = %v, %v", ok, err)
		}
		assertSuperseded(t, d, c.ID, KindSocial, staleSocial)
		assertSuperseded(t, d, c.ID, KindFull, staleFull)
	})
}
