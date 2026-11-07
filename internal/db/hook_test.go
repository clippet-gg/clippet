package db

import (
	"context"
	"testing"
)

// The clip webhook's queue rules (ROADMAP #54, §20 erratum 319).

// hookJobs returns every `hook` job of a clip, oldest first.
func hookJobs(t *testing.T, d *DB, clipID string) []Job {
	t.Helper()
	rows, err := d.ro.QueryContext(context.Background(),
		"SELECT "+jobCols+" FROM jobs WHERE clip_id = ? AND kind = ? ORDER BY id", clipID, KindHook)
	if err != nil {
		t.Fatalf("hook jobs: %v", err)
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

// TestFinalizeEnqueuesHook: QueueHook puts exactly one `hook` job in the ready
// commit, listed and UNLISTED alike and whatever the notify bit says; without
// it nothing; and the already-applied path (a second finalize) adds none.
func TestFinalizeEnqueuesHook(t *testing.T) {
	cases := []struct {
		name             string
		hook, notify     bool
		unlisted         bool
		wantHook, wantNt int
	}{
		{"hook + listed", true, false, false, 1, 0},
		{"hook + unlisted", true, false, true, 1, 0},
		{"hook + notify + listed", true, true, false, 1, 1},
		{"hook + notify + unlisted", true, true, true, 1, 0},
		{"no hook", false, true, false, 0, 1},
		{"neither", false, false, false, 0, 0},
	}
	for _, c := range cases {
		d := openTest(t)
		ctx := context.Background()
		u := mkUser(t, d, "a")
		mkClip(t, d, u, 1, nil, c.unlisted)
		j, clip := claim(t, d, 10)
		f := ready()
		f.QueueHook = c.hook
		ok, _, err := d.FinalizeTranscode(ctx, j.ID, clip.ID, f, c.notify, 30)
		if err != nil || !ok {
			t.Fatalf("%s: Finalize ok=%v err=%v", c.name, ok, err)
		}
		got := hookJobs(t, d, clip.ID)
		if len(got) != c.wantHook {
			t.Fatalf("%s: %d hook job(s), want %d", c.name, len(got), c.wantHook)
		}
		if n := len(notifyJobs(t, d, clip.ID)); n != c.wantNt {
			t.Fatalf("%s: %d notify job(s), want %d", c.name, n, c.wantNt)
		}
		if c.wantHook == 0 {
			continue
		}
		h := got[0]
		if h.State != JobQueued || h.Stage != JobStageQueued || h.ParamsJSON != "{}" || h.RunAfter != 30 || h.Attempts != 0 {
			t.Fatalf("%s: hook job = %+v", c.name, h)
		}
		// The second finalize finds the clip ready already (n == 0): no flip,
		// no second delivery.
		if ok, _, err := d.FinalizeTranscode(ctx, j.ID, clip.ID, f, c.notify, 31); err != nil || ok {
			t.Fatalf("%s: re-finalize ok=%v err=%v", c.name, ok, err)
		}
		if got := hookJobs(t, d, clip.ID); len(got) != 1 {
			t.Fatalf("%s: re-finalize made %d hook jobs", c.name, len(got))
		}
	}
}

// TestHookIsNotQueuedByAnEdit: the webhook fires once per clip, when it FIRST
// becomes ready. A trim's and a replacement's commits carry ReadyFields too,
// and neither may read the instruction.
func TestHookIsNotQueuedByAnEdit(t *testing.T) {
	ctx := context.Background()

	t.Run("trim", func(t *testing.T) {
		d := openTest(t)
		c, _ := readyClip(t, d)
		job := mustEnqueueTrim(t, d, c.ID, 2000)
		f := trimmedFields()
		f.QueueHook = true
		if _, ok, _, err := d.SwapRendition(ctx, job, c.ID, f, TrimParams{StartMS: 1000, EndMS: 3000}, 2001); err != nil || !ok {
			t.Fatalf("SwapRendition ok=%v err=%v", ok, err)
		}
		if got := hookJobs(t, d, c.ID); len(got) != 0 {
			t.Fatalf("a trim queued %d hook job(s)", len(got))
		}
	})

	t.Run("replace", func(t *testing.T) {
		d := openTest(t)
		c, _ := readyClip(t, d)
		job, err := d.EnqueueReplace(ctx, c.ID, MarshalReplaceParams(ReplaceParams{SourceKey: "k", SourceETag: "e", SourceSize: 1, Spool: ".upload-x-1"}), 2000)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := d.ClaimMediaJob(ctx, 2001); err != nil {
			t.Fatal(err)
		}
		f := replaceFields()
		f.QueueHook = true
		if _, ok, err := d.SwapSource(ctx, job.ID, c.ID, f, newSource(), 2002); err != nil || !ok {
			t.Fatalf("SwapSource ok=%v err=%v", ok, err)
		}
		if got := hookJobs(t, d, c.ID); len(got) != 0 {
			t.Fatalf("a replacement queued %d hook job(s)", len(got))
		}
	})
}

// TestHookJobBudgetAndClaim: five attempts whatever the transcode caps say,
// claimed by the notify loop and by neither of the others, the clip ready
// throughout.
func TestHookJobBudgetAndClaim(t *testing.T) {
	if l := limitsFor(KindHook, Limits{MaxAttempts: 99, MaxTransient: 99}); l.MaxAttempts != HookMaxAttempts || l.MaxTransient != HookMaxAttempts {
		t.Fatalf("limitsFor(hook) = %+v, want %d/%d", l, HookMaxAttempts, HookMaxAttempts)
	}
	if HookMaxAttempts != 5 {
		t.Fatalf("HookMaxAttempts = %d; DEPLOY §11.1 promises five", HookMaxAttempts)
	}
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	mkClip(t, d, u, 1, nil, false)
	tj, clip := claim(t, d, 10)
	f := ready()
	f.QueueHook = true
	if ok, _, err := d.FinalizeTranscode(ctx, tj.ID, clip.ID, f, false, 30); err != nil || !ok {
		t.Fatalf("finalize: %v %v", ok, err)
	}
	if _, _, ok, err := d.ClaimMediaJob(ctx, 40); err != nil || ok {
		t.Fatalf("the media loop claimed a hook job: ok=%v err=%v", ok, err)
	}
	if _, _, ok, err := d.ClaimJob(ctx, 40); err != nil || ok {
		t.Fatalf("the transcode loop claimed a hook job: ok=%v err=%v", ok, err)
	}
	lim := Limits{MaxAttempts: 99, MaxTransient: 99}
	now := int64(40)
	var waits []int64
	for attempt := 1; attempt <= HookMaxAttempts; attempt++ {
		j, _, ok, err := d.ClaimNotifyJob(ctx, now)
		if err != nil || !ok || j.Kind != KindHook {
			t.Fatalf("attempt %d: claim: %+v ok=%v err=%v", attempt, j, ok, err)
		}
		out, err := d.FailAttempt(ctx, j.ID, ClaimStamp(j), clip.ID, "HTTP 500", FailCrash, lim, now)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		want := OutcomeRequeued
		if attempt == HookMaxAttempts {
			want = OutcomeFailed
		}
		if out != want {
			t.Fatalf("attempt %d: outcome %v, want %v", attempt, out, want)
		}
		j2, _ := getJob(ctx, d.ro, j.ID)
		if out == OutcomeRequeued {
			waits = append(waits, j2.RunAfter-now)
		}
		now = j2.RunAfter
	}
	// +30s, +60s, +120s, +240s: the schedule DEPLOY §11.1 states.
	if want := []int64{30, 60, 120, 240}; len(waits) != len(want) || waits[0] != 30 || waits[1] != 60 || waits[2] != 120 || waits[3] != 240 {
		t.Fatalf("backoff = %v, want %v", waits, want)
	}
	c, _ := d.GetClip(ctx, clip.ID)
	if c.State != ClipReady || c.FailureReason != nil {
		t.Fatalf("clip = %s / %v — a failed delivery is not a failed clip", c.State, c.FailureReason)
	}
}

// TestLiveJobsIgnoreAQueuedHook: a hook writes no object, so a queued one must
// not hide a clip's leftovers from `admin orphans` (notify's reason).
func TestLiveJobsIgnoreAQueuedHook(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	mkClip(t, d, u, 1, nil, false)
	tj, clip := claim(t, d, 10)
	f := ready()
	f.QueueHook = true
	if ok, _, err := d.FinalizeTranscode(ctx, tj.ID, clip.ID, f, true, 30); err != nil || !ok {
		t.Fatalf("finalize: %v %v", ok, err)
	}
	if n := len(hookJobs(t, d, clip.ID)); n != 1 {
		t.Fatalf("%d hook jobs", n)
	}
	live, err := d.ClipIDsWithLiveJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if live[clip.ID] {
		t.Fatalf("a queued hook (and notify) job counted as live work on %s", clip.ID)
	}
}
