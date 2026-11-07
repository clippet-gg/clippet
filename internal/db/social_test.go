package db

import (
	"context"
	"testing"
)

// The §17 #3 database half: the job is queued by the very commit that
// publishes the rendition it will describe, it writes five columns and
// nothing else, and a rendition that moved underneath it throws its work
// away.

// socialJobs counts a clip's social jobs by state.
func socialJobs(t *testing.T, d *DB, clipID string) (queued, done, total int) {
	t.Helper()
	rows, err := d.ro.QueryContext(context.Background(),
		"SELECT state, COUNT(*) FROM jobs WHERE clip_id = ? AND kind = ? GROUP BY state", clipID, KindSocial)
	if err != nil {
		t.Fatalf("count social jobs: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		total += n
		switch state {
		case JobQueued:
			queued += n
		case JobDone:
			done += n
		}
	}
	return queued, done, total
}

// TestFinalizeQueuesTheSocialJobOnlyWhenAsked: the ready flip and the social
// enqueue are ONE commit, and QueueSocial is the whole of the decision — db
// never re-derives it (the skip rule lives in the worker, where the config
// is).
func TestFinalizeQueuesTheSocialJobOnlyWhenAsked(t *testing.T) {
	ctx := context.Background()
	for _, want := range []bool{false, true} {
		d := openTest(t)
		u := mkUser(t, d, "a")
		mkClip(t, d, u, 1, nil, false)
		j, c := claim(t, d, 10)
		f := ready()
		f.QueueSocial = want
		if ok, _, err := d.FinalizeTranscode(ctx, j.ID, c.ID, f, false, 20); err != nil || !ok {
			t.Fatalf("finalize: ok=%v err=%v", ok, err)
		}
		queued, _, total := socialJobs(t, d, c.ID)
		if want && (queued != 1 || total != 1) {
			t.Fatalf("QueueSocial=true → queued=%d total=%d, want exactly one queued job", queued, total)
		}
		if !want && total != 0 {
			t.Fatalf("QueueSocial=false → %d social jobs, want none", total)
		}
		// The clip is ready either way, and the job that was (or was not)
		// queued changed nothing about it.
		gc, err := d.GetClip(ctx, c.ID)
		if err != nil || gc.State != ClipReady || gc.SocialKey != nil {
			t.Fatalf("clip = %q social_key=%v err=%v", gc.State, gc.SocialKey, err)
		}
	}
}

// TestEnqueueSocialAdmitsOnlyOne: the at-most-one guard, which is what lets
// two commit points (a finalize and a trim's swap) both ask without either
// having to know about the other.
func TestEnqueueSocialAdmitsOnlyOne(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "a")
	mkClip(t, d, u, 1, nil, false)
	j, c := claim(t, d, 10)

	if _, err := d.EnqueueSocial(ctx, c.ID, 20); err != ErrStateMismatch {
		t.Fatalf("EnqueueSocial on a processing clip = %v, want ErrStateMismatch", err)
	}
	if ok, _, err := d.FinalizeTranscode(ctx, j.ID, c.ID, ready(), false, 20); err != nil || !ok {
		t.Fatalf("finalize: ok=%v err=%v", ok, err)
	}
	first, err := d.EnqueueSocial(ctx, c.ID, 21)
	if err != nil || first.Kind != KindSocial || first.State != JobQueued {
		t.Fatalf("EnqueueSocial = %+v err=%v", first, err)
	}
	if _, err := d.EnqueueSocial(ctx, c.ID, 22); err != ErrConflict {
		t.Fatalf("second EnqueueSocial = %v, want ErrConflict", err)
	}
	if _, err := d.EnqueueSocial(ctx, "nosuchclip12", 23); err != ErrNotFound {
		t.Fatalf("EnqueueSocial on a missing clip = %v, want ErrNotFound", err)
	}
	if _, _, total := socialJobs(t, d, c.ID); total != 1 {
		t.Fatalf("%d social jobs, want 1", total)
	}
}

// TestSetSocialWritesFiveColumnsAndNothingElse is the §5 rule: the clip is
// ready before, during and after, and the row is otherwise byte-identical.
func TestSetSocialWritesFiveColumnsAndNothingElse(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "a")
	mkClip(t, d, u, 1, nil, false)
	j, c := claim(t, d, 10)
	f := ready()
	f.QueueSocial = true
	if ok, _, err := d.FinalizeTranscode(ctx, j.ID, c.ID, f, false, 20); err != nil || !ok {
		t.Fatalf("finalize: ok=%v err=%v", ok, err)
	}
	before, _ := d.GetClip(ctx, c.ID)
	sj, _, ok, err := d.ClaimMediaJob(ctx, 30)
	if err != nil || !ok || sj.Kind != KindSocial {
		t.Fatalf("ClaimMediaJob = %+v ok=%v err=%v, want the social job", sj, ok, err)
	}
	swap, err := d.SetSocial(ctx, sj.ID, c.ID, SocialFields{Key: "s", Size: 7, ETag: "se", Width: 1280, Height: 720},
		*before.RenditionETag, 40)
	if err != nil || !swap.OK || len(swap.Superseded) != 0 {
		t.Fatalf("SetSocial = %+v err=%v", swap, err)
	}
	after, _ := d.GetClip(ctx, c.ID)
	switch {
	case after.State != ClipReady || after.FailureReason != nil:
		t.Fatalf("clip is %q/%v after a social encode", after.State, after.FailureReason)
	case after.SocialKey == nil || *after.SocialKey != "s" || after.SocialSize == nil || *after.SocialSize != 7:
		t.Fatalf("social key/size not written: %+v", after)
	case after.SocialETag == nil || *after.SocialETag != "se":
		t.Fatalf("social etag not written: %v", after.SocialETag)
	case after.SocialWidth == nil || *after.SocialWidth != 1280 || after.SocialHeight == nil || *after.SocialHeight != 720:
		t.Fatalf("social dims not written: %v x %v", after.SocialWidth, after.SocialHeight)
	case *after.RenditionKey != *before.RenditionKey || *after.PosterKey != *before.PosterKey:
		t.Fatalf("a social encode moved the rendition or the poster")
	case after.ReadyAt == nil || *after.ReadyAt != *before.ReadyAt:
		t.Fatalf("ready_at moved: %v → %v", before.ReadyAt, after.ReadyAt)
	case after.UpdatedAt == before.UpdatedAt:
		t.Fatalf("updated_at must move: the derived routes date from it (§8.5)")
	}
	gj, _ := d.LatestJobOfKind(ctx, c.ID, KindSocial)
	if gj.State != JobDone {
		t.Fatalf("social job = %q, want done", gj.State)
	}
}

// TestSetSocialRefusesAMovedRendition: the ETag predicate. A trim that lands
// mid-run means the card file describes frames nobody can see any more, and
// publishing it is worse than not publishing it — nothing downstream could
// ever tell.
func TestSetSocialRefusesAMovedRendition(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "a")
	mkClip(t, d, u, 1, nil, false)
	j, c := claim(t, d, 10)
	if ok, _, err := d.FinalizeTranscode(ctx, j.ID, c.ID, ready(), false, 20); err != nil || !ok {
		t.Fatalf("finalize: ok=%v err=%v", ok, err)
	}
	sj, err := d.EnqueueSocial(ctx, c.ID, 21)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := d.ClaimMediaJob(ctx, 22); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	swap, err := d.SetSocial(ctx, sj.ID, c.ID, SocialFields{Key: "s", Size: 7, ETag: "se", Width: 1280, Height: 720},
		"a-rendition-that-is-no-longer-there", 30)
	if err != nil || swap.OK || swap.Reason != "the rendition changed" {
		t.Fatalf("SetSocial = %+v err=%v, want a refusal naming the rendition", swap, err)
	}
	after, _ := d.GetClip(ctx, c.ID)
	if after.SocialKey != nil {
		t.Fatalf("a refused swap wrote %v", *after.SocialKey)
	}
}

// TestSwapRenditionInvalidatesTheSocialFile is erratum 70's reservation, now
// due: a trim replaces the frames the card file was made from, so the columns
// are NULLed, the old key comes back among the superseded ones and — when the
// caller asks — a fresh job is queued by the same statement.
func TestSwapRenditionInvalidatesTheSocialFile(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "a")
	mkClip(t, d, u, 1, nil, false)
	j, c := claim(t, d, 10)
	if ok, _, err := d.FinalizeTranscode(ctx, j.ID, c.ID, ready(), false, 20); err != nil || !ok {
		t.Fatalf("finalize: ok=%v err=%v", ok, err)
	}
	sj, err := d.EnqueueSocial(ctx, c.ID, 21)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := d.ClaimMediaJob(ctx, 22); err != nil || !ok {
		t.Fatalf("claim social: ok=%v err=%v", ok, err)
	}
	if swap, err := d.SetSocial(ctx, sj.ID, c.ID,
		SocialFields{Key: "social-old", Size: 7, ETag: "se", Width: 1280, Height: 720}, "re", 23); err != nil || !swap.OK {
		t.Fatalf("SetSocial: %+v %v", swap, err)
	}

	tj, err := d.EnqueueTrim(ctx, c.ID, MarshalTrimParams(TrimParams{StartMS: 0, EndMS: 2000}), 30)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := d.ClaimMediaJob(ctx, 31); err != nil || !ok {
		t.Fatalf("claim trim: ok=%v err=%v", ok, err)
	}
	f := ready()
	f.RenditionKey, f.RenditionETag, f.PosterKey = "r2", "re2", "p2"
	f.QueueSocial = true
	superseded, ok, _, err := d.SwapRendition(ctx, tj.ID, c.ID, f, TrimParams{StartMS: 0, EndMS: 2000}, 40)
	if err != nil || !ok {
		t.Fatalf("SwapRendition: ok=%v err=%v", ok, err)
	}
	var sawSocial bool
	for _, k := range superseded {
		if k == "social-old" {
			sawSocial = true
		}
	}
	if !sawSocial {
		t.Fatalf("superseded = %v, want the old social key among them (erratum 70)", superseded)
	}
	after, _ := d.GetClip(ctx, c.ID)
	if after.SocialKey != nil || after.SocialSize != nil || after.SocialETag != nil ||
		after.SocialWidth != nil || after.SocialHeight != nil {
		t.Fatalf("the trim left social_* behind: %+v", after)
	}
	if queued, _, _ := socialJobs(t, d, c.ID); queued != 1 {
		t.Fatalf("%d queued social jobs after a trim, want exactly one", queued)
	}
}

// TestSocialIsMediaWork: the social kind belongs to the media loop — the one
// whose ffmpeg runs are bounded by the TRANSCODE_CONCURRENCY semaphore — and
// to neither of the other two.
func TestSocialIsMediaWork(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "a")
	mkClip(t, d, u, 1, nil, false)
	j, c := claim(t, d, 10)
	f := ready()
	f.QueueSocial = true
	if ok, _, err := d.FinalizeTranscode(ctx, j.ID, c.ID, f, false, 20); err != nil || !ok {
		t.Fatalf("finalize: ok=%v err=%v", ok, err)
	}
	if gj, _, ok, err := d.ClaimJob(ctx, 30); ok || err != nil {
		t.Fatalf("ClaimJob claimed a social job: %+v (ok=%v err=%v)", gj, ok, err)
	}
	if gj, _, ok, err := d.ClaimNotifyJob(ctx, 30); ok || err != nil {
		t.Fatalf("ClaimNotifyJob claimed a social job: %+v (ok=%v err=%v)", gj, ok, err)
	}
	gj, _, ok, err := d.ClaimMediaJob(ctx, 30)
	if err != nil || !ok || gj.Kind != KindSocial {
		t.Fatalf("ClaimMediaJob = %+v ok=%v err=%v, want the social job", gj, ok, err)
	}
}

// TestSocialCapsAreItsOwn: a crash-class ending spends SocialMaxAttempts, an
// environment-class one the instance's TRANSCODE_MAX_TRANSIENT (§20 erratum
// 89), and neither ever touches the clip.
func TestSocialCapsAreItsOwn(t *testing.T) {
	ctx := context.Background()
	lim := Limits{MaxAttempts: 3, MaxTransient: 10}
	if got := Cap(KindSocial, FailCrash, lim); got != SocialMaxAttempts {
		t.Fatalf("crash cap = %d, want %d", got, SocialMaxAttempts)
	}
	if got := Cap(KindSocial, FailTransient, lim); got != 10 {
		t.Fatalf("transient cap = %d, want the instance's 10", got)
	}

	d := openTest(t)
	u := mkUser(t, d, "a")
	mkClip(t, d, u, 1, nil, false)
	j, c := claim(t, d, 10)
	f := ready()
	f.QueueSocial = true
	if ok, _, err := d.FinalizeTranscode(ctx, j.ID, c.ID, f, false, 20); err != nil || !ok {
		t.Fatalf("finalize: ok=%v err=%v", ok, err)
	}
	for i := 1; i <= SocialMaxAttempts; i++ {
		sj, _, ok, err := d.ClaimMediaJob(ctx, int64(30+i*100))
		if err != nil || !ok {
			t.Fatalf("claim %d: ok=%v err=%v", i, ok, err)
		}
		out, err := d.FailAttempt(ctx, sj.ID, ClaimStamp(sj), c.ID, "boom", FailCrash, lim, int64(31+i*100))
		if err != nil {
			t.Fatal(err)
		}
		want := OutcomeRequeued
		if i == SocialMaxAttempts {
			want = OutcomeFailed
		}
		if out != want {
			t.Fatalf("attempt %d → %v, want %v", i, out, want)
		}
		gc, _ := d.GetClip(ctx, c.ID)
		if gc.State != ClipReady || gc.FailureReason != nil {
			t.Fatalf("attempt %d moved the clip to %q/%v", i, gc.State, gc.FailureReason)
		}
	}
}
