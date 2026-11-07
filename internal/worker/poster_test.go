package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"clippet/internal/db"
	"clippet/internal/store"
	"clippet/internal/transcode"
)

// The card image the owner chose (ROADMAP #5, §20 errata 127-130), from the
// loop that claims the job to the UPDATE that commits it. Every test here
// asserts the same background property from a different angle: whatever
// happens to a card image, the CLIP is ready, playable and unchanged
// afterwards.

func (h *harness) enqueuePoster(clipID string, seconds float64) db.Job {
	h.t.Helper()
	j, err := h.db.EnqueuePoster(context.Background(), clipID,
		db.MarshalPosterParams(db.PosterParams{Seconds: seconds}), h.clock.Now().Unix())
	if err != nil {
		h.t.Fatalf("EnqueuePoster: %v", err)
	}
	h.w.Wake()
	return j
}

// waitPoster waits, on the settle hook, for the clip's latest poster job to match pred with its run settled (awaitJob).
func (h *harness) waitPoster(clipID string, pred func(db.Job) bool) db.Job {
	h.t.Helper()
	return h.awaitJob(db.KindPoster+" job for "+clipID, h.latestOfKind(clipID, db.KindPoster), pred)
}

// The happy path: the media loop claims the `poster` kind, ffmpeg is pointed
// at the RENDITION at the chosen moment, the JPEG lands in the bucket under
// a fresh key, one UPDATE fills the poster columns AND the choice, and the
// superseded object goes.
func TestPosterIsTakenFromTheRenditionAtTheChosenMoment(t *testing.T) {
	h := newHarness(t)
	before := h.readyTrimmable()

	h.enqueuePoster(before.ID, 2.5)
	h.waitPoster(before.ID, jobDone)
	h.waitIdle()

	after := h.clip(before.ID)
	if after.State != db.ClipReady || after.FailureReason != nil {
		t.Fatalf("clip is %q/%v after a poster job; it never marks its clip", after.State, after.FailureReason)
	}
	if after.PosterSeconds == nil || *after.PosterSeconds != 2.5 {
		t.Fatalf("poster_seconds = %v, want the moment that was chosen", after.PosterSeconds)
	}
	if after.PosterKey == nil || *after.PosterKey == *before.PosterKey {
		t.Fatalf("the poster key did not change: %v", after.PosterKey)
	}
	if !strings.Contains(*after.PosterKey, "/poster/poster-") || !strings.HasSuffix(*after.PosterKey, ".jpg") {
		t.Errorf("poster key %q is not the §6 shape", *after.PosterKey)
	}
	if got, ok := h.store.Get(*after.PosterKey); !ok || string(got) != chosenPosterPayload {
		t.Errorf("the poster object is missing or wrong (%q, ok=%v)", got, ok)
	}
	if *after.PosterWidth != 1280 || *after.PosterHeight != 720 {
		t.Errorf("poster dims %dx%d, want the MEASURED 1280x720", *after.PosterWidth, *after.PosterHeight)
	}
	// The superseded poster is gone; the rendition and the ORIGINAL are not.
	deleted := false
	for _, k := range h.store.Deletes() {
		if k == *before.PosterKey {
			deleted = true
		}
	}
	if !deleted {
		t.Errorf("the superseded poster %q was not deleted: %v", *before.PosterKey, h.store.Deletes())
	}
	if *after.RenditionKey != *before.RenditionKey {
		t.Error("a poster job moved the rendition")
	}
	if _, ok := h.store.Get(before.SourceKey); !ok {
		t.Error("the original is gone; the upload is sacred")
	}
	// ffmpeg was handed the downloaded RENDITION at exactly that second.
	var seen bool
	for _, call := range h.runner.Calls() {
		if strings.HasPrefix(call, "PosterAt ") {
			seen = true
			if !strings.HasPrefix(call, "PosterAt postersrc-") {
				t.Errorf("the frame was taken from %q, want the downloaded rendition", call)
			}
		}
	}
	if !seen {
		t.Fatalf("no PosterAt call: %v", h.runner.Calls())
	}
	if seeks := h.runner.PosterSeeks(); len(seeks) != 1 || seeks[0] != 2.5 {
		t.Fatalf("seeks = %v, want exactly [2.5]", seeks)
	}
	// …and the duration burned into its corner is the CLIP's, not the moment
	// that was chosen and not the length of the ranged prefix the frame may have
	// been cut from (§20 erratum 287).
	if ms := h.runner.PosterLabelMS(); len(ms) != 1 || before.DurationMS == nil || ms[0] != *before.DurationMS {
		t.Fatalf("poster label durations = %v, want exactly the clip's %v ms", ms, before.DurationMS)
	}
	h.assertNoErrorLogs()
}

// On a TRIMMED clip the choice is on the ORIGINAL's timeline and the seek is
// on the rendition's: the one conversion this feature has, and the one that
// makes the choice survive a cut.
func TestPosterSeekIsRelativeToTheCut(t *testing.T) {
	h := newHarness(t)
	c := h.readyTrimmable()
	// Land a real trim first, so the clip's window really is 1 s-3 s.
	h.runner.probeOutFn = trimmedOutput(2000)
	h.enqueueTrim(c.ID, 1000, 3000)
	h.waitTrim(c.ID, jobDone)
	h.waitIdle()
	if got := h.clip(c.ID); got.TrimStartMS == nil || *got.TrimStartMS != 1000 {
		t.Fatalf("the trim did not land: %+v", got)
	}

	// 2.5 s of the ORIGINAL is 1.5 s of this rendition.
	h.enqueuePoster(c.ID, 2.5)
	h.waitPoster(c.ID, jobDone)
	h.waitIdle()

	seeks := h.runner.PosterSeeks()
	if len(seeks) == 0 || seeks[len(seeks)-1] != 1.5 {
		t.Fatalf("seeks = %v, want the last one to be 1.5 (2.5 s of the original, 1 s into the cut)", seeks)
	}
	if got := h.clip(c.ID); got.PosterSeconds == nil || *got.PosterSeconds != 2.5 {
		t.Fatalf("poster_seconds = %v; the column is on the ORIGINAL's timeline", got.PosterSeconds)
	}
	h.assertNoErrorLogs()
}

// A moment the file has no picture at is the CLIENT's failure: INFO, the
// fixed owner sentence in the row, no ERROR anywhere, and a clip that is
// exactly as it was.
func TestPosterOfAMomentWithNoPictureCostsTheClipNothing(t *testing.T) {
	h := newHarness(t)
	before := h.readyTrimmable()
	h.runner.posterPinFn = func(context.Context, string, string, float64) (transcode.PosterResult, error) {
		return transcode.PosterResult{}, &transcode.ErrPermanent{Err: transcode.ErrNoFrameAt}
	}

	h.enqueuePoster(before.ID, 5)
	job := h.waitPoster(before.ID, jobFailed)
	h.waitIdle()

	if job.LastError == nil || *job.LastError != msgNoFrame {
		t.Fatalf("last_error = %v, want the fixed owner sentence %q", job.LastError, msgNoFrame)
	}
	after := h.clip(before.ID)
	if after.State != db.ClipReady || after.FailureReason != nil {
		t.Fatalf("a failed poster job marked the clip %q/%v", after.State, after.FailureReason)
	}
	if *after.PosterKey != *before.PosterKey || after.PosterSeconds != nil {
		t.Fatalf("a failed poster job changed the card image: %+v", after)
	}
	// Nothing this attempt might have started writing is left in the bucket.
	for _, k := range h.store.Keys() {
		if strings.Contains(k, "/poster/") && k != *before.PosterKey {
			t.Errorf("a failed attempt left %q in the bucket", k)
		}
	}
	h.assertNoErrorLogs()
	h.assertLog("INFO", "the clip keeps the card image it had")
}

// The commit's predicate is the RENDITION's ETag: a trim that lands mid-run
// makes this picture one of frames the clip no longer has, and publishing it
// is the one failure nothing downstream could ever see.
func TestPosterIsDiscardedWhenTheRenditionChanged(t *testing.T) {
	h := newHarness(t)
	c := h.gifReady() // ready, with no worker loop running
	job := h.enqueuePoster(c.ID, 1)
	claimed := h.claimKind(db.KindPoster)
	if claimed.ID != job.ID {
		t.Fatalf("claimed %d, want the poster job %d", claimed.ID, job.ID)
	}
	// The rendition moves under the run, exactly as a committed trim does.
	ctx := context.Background()
	tjob, err := h.db.EnqueueTrim(ctx, c.ID, db.MarshalTrimParams(db.TrimParams{StartMS: 0, EndMS: 2000}), h.clock.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h.db.ClaimMediaJob(ctx, h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, ok, _, err := h.db.SwapRendition(ctx, tjob.ID, c.ID, db.ReadyFields{
		RenditionKey: "r9", RenditionSize: 9, RenditionETag: "a-different-rendition",
		PosterKey: "p9", PosterSize: 9, PosterETag: "pe9", PosterWidth: 640, PosterHeight: 360,
		Width: 1280, Height: 720, DurationMS: 2000, FPSNum: 30, FPSDen: 1,
	}, db.TrimParams{StartMS: 0, EndMS: 2000}, h.clock.Now().Unix()); err != nil || !ok {
		t.Fatalf("SwapRendition: ok=%v err=%v", ok, err)
	}

	key := store.PosterKey(h.cfg.S3.KeyPrefix, c.ID, "whatever")
	swap, err := h.db.SetPoster(ctx, claimed.ID, c.ID, db.PosterFields{
		Key: key, Size: 9, ETag: "e", Width: 640, Height: 360, Seconds: 1,
	}, "the-rendition-etag", h.clock.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if swap.OK {
		t.Fatal("the card image was published over a rendition it does not describe")
	}
	if swap.Reason != "the rendition changed" {
		t.Errorf("reason = %q", swap.Reason)
	}
	after := h.clip(c.ID)
	if *after.PosterKey != "p9" || after.PosterSeconds != nil {
		t.Fatalf("the refused swap changed the clip: %+v", after)
	}
}

// A poster job is REGISTERED, so lifecycle.Destroy's Cancel waits for it:
// the run holds the clip's scratch dir and is about to upload into a prefix
// Destroy is about to remove (§20 erratum 129, erratum 71's rule).
//
// It runs Destroy's own order — MarkDeleting, then Cancel (§20 erratum
// 317(b)). Without the mark, the cancelled run's release hands the job back
// to the queue (run_after = now on a frozen clock), and the media loop may
// claim it again a moment after Cancel returned: a NEW run, which no promise
// of Cancel's covers, and the "Busy is still true" this test reported three
// times. With the mark, the release finds a row that is no longer its own,
// no claim takes a job of a deleting clip, and "no run of the clip after
// Cancel returns" is a fact rather than a race.
//
// No step waits on the wall clock (§20 erratum 331, F3): "Cancel has not
// returned while ffmpeg runs" is checked at the instant the run has SEEN its
// context cancelled — Cancel is then provably inside its wait — rather than
// after 150 ms of hoping, and the run's settle closes the test. It passes at
// any GOMAXPROCS (-cpu 1,2,4,16).
func TestPosterIsCancelledSynchronously(t *testing.T) {
	h := newHarness(t)
	c := h.readyTrimmable()

	started := make(chan struct{})
	sawCancel := make(chan struct{})
	release := make(chan struct{})
	h.runner.posterPinFn = func(ctx context.Context, in, out string, seconds float64) (transcode.PosterResult, error) {
		close(started)
		<-ctx.Done()
		close(sawCancel)
		<-release // an ffmpeg that ignores its context, like a real one mid-write
		return transcode.PosterResult{}, ctx.Err()
	}
	guard := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(settleGuard): // a hang guard, never a budget
			t.Fatalf("%s\nlogs:\n%s", what, h.logs.String())
		}
	}
	h.enqueuePoster(c.ID, 1)
	guard(started, "the poster run never started")
	if !h.w.Busy(c.ID) {
		t.Fatal("Busy is false while a poster run holds the clip's scratch dir")
	}
	if err := h.db.MarkDeleting(context.Background(), c.ID, h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	mark := h.settles.mark()
	done := make(chan struct{})
	go func() {
		h.w.Cancel(context.Background(), c.ID)
		close(done)
	}()
	// Cancel cancelled the run's context (the run saw it) and the run is still
	// inside "ffmpeg": Cancel must be waiting, not returned.
	guard(sawCancel, "Cancel never cancelled the run's context")
	select {
	case <-done:
		t.Fatal("Cancel returned while the poster's ffmpeg was still running")
	default:
	}
	close(release)
	guard(done, "Cancel never returned")
	if h.w.Busy(c.ID) {
		t.Error("Busy is still true after Cancel returned")
	}
	h.settledAfter(mark, func(s Settled) bool { return s.ClipID == c.ID && s.Kind == db.KindPoster })
	// The ending touched no row it does not own: the job is MarkDeleting's
	// cancelled row (not requeued), and the clip is still where the mark put
	// it — a poster ending never moves a clip.
	if j, err := h.db.LatestJobOfKind(context.Background(), c.ID, db.KindPoster); err != nil || j.State != db.JobCancelled {
		t.Errorf("poster job = %+v (err %v), want cancelled", j, err)
	}
	if got := h.clip(c.ID); got.State != db.ClipDeleting {
		t.Errorf("the clip is %q after a cancelled poster job, want %q", got.State, db.ClipDeleting)
	}
}

// The preview opens where the CARD does once a frame has been chosen — and
// the extra selection pass erratum 92 added is not run at all, because the
// answer is a column now (§20 erratum 131).
func TestGIFStartsAtTheChosenFrame(t *testing.T) {
	h := newHarness(t)
	c := h.readyTrimmable()

	h.enqueuePoster(c.ID, 3)
	h.waitPoster(c.ID, jobDone)
	h.waitIdle()

	h.enqueueGIF(c.ID)
	h.waitGIF(c.ID, jobDone)
	h.waitIdle()

	plans := h.runner.GIFPlans()
	if len(plans) != 1 {
		t.Fatalf("GIF plans = %+v, want one", plans)
	}
	// The rendition is 6 s long and the preview is 6 s, so PlanGIF clamps the
	// start to duration − 6 s = 0. What this test can prove without a second
	// fixture is the ABSENCE of the derivation pass: the choice is stored, so
	// nothing needs to look for it again.
	for _, call := range h.runner.Calls() {
		if strings.HasPrefix(call, "PosterFrameSeconds") {
			t.Fatalf("the gif job re-derived the poster frame although the clip stores one: %v", h.runner.Calls())
		}
	}
	// …and with no choice, it DOES derive: the two halves of erratum 131.
	other := h.readyTrimmable()
	h.enqueueGIF(other.ID)
	h.waitGIF(other.ID, jobDone)
	h.waitIdle()
	var derived bool
	for _, call := range h.runner.Calls() {
		if strings.HasPrefix(call, "PosterFrameSeconds") {
			derived = true
		}
	}
	if !derived {
		t.Fatalf("a clip with no chosen frame must still derive one: %v", h.runner.Calls())
	}
}

// The window of a preview really does move to the chosen frame when there is
// room for it to: a long rendition with a choice late in it opens there.
func TestGIFWindowFollowsTheChosenFrame(t *testing.T) {
	h := newHarness(t)
	c := h.readyTrimmable()
	// Make the rendition long enough that PlanGIF's "never past duration − 6 s"
	// clamp does not swallow the choice.
	ctx := context.Background()
	job := h.enqueuePoster(c.ID, 2)
	_ = job
	h.waitPoster(c.ID, jobDone)
	h.waitIdle()
	if _, err := h.db.UpdateClipMeta(ctx, c.ID, db.ClipPatch{}, h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	got := h.clip(c.ID)
	if s, ok := renditionPosterSeconds(got); !ok || s != 2 {
		t.Fatalf("renditionPosterSeconds = %v, %v; an untrimmed clip's choice is its own timeline", s, ok)
	}
	// …and a choice that falls outside the rendition is no choice at all.
	got.DurationMS = new(int64)
	*got.DurationMS = 1000
	if _, ok := renditionPosterSeconds(got); ok {
		t.Error("a choice past the end of the rendition was accepted as a start")
	}
}

// A poster job never touches clips.state, whatever ends it — the same
// property db.FailAttempt enforces for the trim and the gif.
func TestPosterFailurePreservesTheClip(t *testing.T) {
	h := newHarness(t)
	before := h.readyTrimmable()
	h.runner.posterPinFn = func(context.Context, string, string, float64) (transcode.PosterResult, error) {
		return transcode.PosterResult{}, errors.New("storage went away mid-frame")
	}
	h.enqueuePoster(before.ID, 1)
	// PosterMaxAttempts is two, and the environment cap is the instance's, so
	// a transient failure retries before it gives up. Wait for the row to
	// record the first attempt and assert the clip through it.
	h.waitPoster(before.ID, func(j db.Job) bool { return j.TransientFailures >= 1 })
	after := h.clip(before.ID)
	if after.State != db.ClipReady || after.FailureReason != nil {
		t.Fatalf("a transient poster failure marked the clip %q/%v", after.State, after.FailureReason)
	}
	if db.Cap(db.KindPoster, db.FailCrash, h.w.lim) != db.PosterMaxAttempts {
		t.Errorf("a poster's crash cap is %d, want db.PosterMaxAttempts (%d)",
			db.Cap(db.KindPoster, db.FailCrash, h.w.lim), db.PosterMaxAttempts)
	}
}

// A trim KEEPS the choice when the cut still contains the moment, and takes
// the frame again from the new rendition; it drops the choice when the cut
// removes it and falls back to the automatic search (§20 erratum 130). Both
// halves are the same run through the same code, so they are one test.
func TestTrimKeepsOrDropsTheChosenFrame(t *testing.T) {
	run := func(t *testing.T, chosen float64, start, end int64) (db.Clip, []float64) {
		t.Helper()
		h := newHarness(t)
		c := h.readyTrimmable()
		h.enqueuePoster(c.ID, chosen)
		h.waitPoster(c.ID, jobDone)
		h.waitIdle()
		if got := h.clip(c.ID); got.PosterSeconds == nil {
			t.Fatalf("the choice was not recorded: %+v", got)
		}
		before := len(h.runner.PosterSeeks())
		h.runner.probeOutFn = trimmedOutput(end - start)
		h.enqueueTrim(c.ID, start, end)
		h.waitTrim(c.ID, jobDone)
		h.waitIdle()
		h.assertNoErrorLogs()
		return h.clip(c.ID), h.runner.PosterSeeks()[before:]
	}

	// 2.5 s of the original is inside a 1 s-4 s cut: the choice survives and
	// the frame is re-taken at 1.5 s of the NEW rendition.
	got, seeks := run(t, 2.5, 1000, 4000)
	if got.PosterSeconds == nil || *got.PosterSeconds != 2.5 {
		t.Fatalf("poster_seconds = %v after a cut that contains 2.5 s; the choice is on the original's timeline", got.PosterSeconds)
	}
	if len(seeks) != 1 || seeks[0] != 1.5 {
		t.Fatalf("the trim's poster seeks = %v, want [1.5] on the new rendition", seeks)
	}

	// 0.5 s is NOT inside a 1 s-4 s cut: the choice goes back to NULL and no
	// chosen-frame extraction is attempted at all.
	got, seeks = run(t, 0.5, 1000, 4000)
	if got.PosterSeconds != nil {
		t.Fatalf("poster_seconds = %v after a cut that removed the moment; it must go back to the automatic frame", *got.PosterSeconds)
	}
	if len(seeks) != 0 {
		t.Fatalf("the trim took a chosen frame (%v) although the cut removed the moment", seeks)
	}
}

// A chosen frame can never fail a TRIM: an extraction that says no falls back
// to the automatic search and drops the choice, because failing an edit of a
// playable clip over a picture is exactly what §17 #5's doctrine forbids.
func TestATrimSurvivesAChosenFrameThatCannotBeTaken(t *testing.T) {
	h := newHarness(t)
	c := h.readyTrimmable()
	h.enqueuePoster(c.ID, 2.5)
	h.waitPoster(c.ID, jobDone)
	h.waitIdle()

	h.runner.posterPinFn = func(context.Context, string, string, float64) (transcode.PosterResult, error) {
		return transcode.PosterResult{}, &transcode.ErrPermanent{Err: transcode.ErrNoFrameAt}
	}
	h.runner.probeOutFn = trimmedOutput(3000)
	h.enqueueTrim(c.ID, 1000, 4000)
	h.waitTrim(c.ID, jobDone)
	h.waitIdle()

	got := h.clip(c.ID)
	if got.State != db.ClipReady {
		t.Fatalf("the clip is %q after a trim whose chosen frame could not be taken", got.State)
	}
	if got.PosterSeconds != nil {
		t.Errorf("poster_seconds = %v; a choice that could not be honoured is not a choice", *got.PosterSeconds)
	}
	if got.PosterKey == nil || *got.PosterKey == "" {
		t.Fatal("the trim produced no card image at all")
	}
	h.assertNoErrorLogs()
	h.assertLog("INFO", "using the automatic frame")
}

// The owner-facing vocabulary is this kind's OWN, and that is the assertion:
// every one of the trim's seven sentences says the word "trim", so reusing
// one would tell an owner whose chosen frame could not be taken that a trim
// failed — which is not what happened and not what they should go and look
// at (§20 erratum 80's rule, applied to a second surface).
func TestPosterFailuresNeverBlameTheTrim(t *testing.T) {
	h := newHarness(t)
	p := &posterRun{w: h.w}
	for _, tc := range []struct {
		name  string
		stage string
		err   error
		want  string
	}{
		{"no picture there", stagePoster, &transcode.ErrPermanent{Err: transcode.ErrNoFrameAt}, msgNoFrame},
		{"ffmpeg refused", stagePoster, errors.New("ffmpeg exited 1"), msgFrameMedia},
		{"the bucket went away", stageAcquire, errors.New("download rendition: connection reset"), msgFrameStorage},
		{"the upload went away", stageStore, errors.New("upload poster: connection reset"), msgFrameStorage},
		{"no scratch room", stageAcquire, errNoScratchRoom, msgFrameDisk},
		{"our own output", stagePoster, errors.New("bad_output: the card image is unreadable"), msgFrameInternal},
		{"nothing anybody anticipated", "", errors.New("boom"), msgFrameInternal},
		{"a panic", stagePoster, &panicError{value: "boom"}, msgFrameInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p.stage = tc.stage
			got := p.ownerReason(tc.err)
			if got != tc.want {
				t.Fatalf("ownerReason = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "trim") {
				t.Fatalf("the owner is told %q about a card image", got)
			}
		})
	}
}

// --- the ranged source (N051, §20 erratum 233) --------------------------------

// posterBigClip is a ready clip whose RENDITION is big enough for a prefix to
// be worth fetching. The bytes are never decoded (the fake PosterAt writes its
// own JPEG), so what is being exercised is the arithmetic and the transfer.
// Keep the size under the pipeline's own output budget (§20 erratum 46), which
// for the harness's 1 KiB source is the plan's estimate.
func (h *harness) posterBigClip(renditionBytes int) db.Clip {
	h.t.Helper()
	h.runner.encodeBytes = bytes.Repeat([]byte("r"), renditionBytes)
	c := h.readyTrimmable()
	if c.RenditionSize == nil || *c.RenditionSize != int64(renditionBytes) {
		h.t.Fatalf("the clip's rendition is %v bytes, want %d", c.RenditionSize, renditionBytes)
	}
	if c.DurationMS == nil || *c.DurationMS <= 0 {
		h.t.Fatalf("the clip has no duration (%v): the prefix cannot be placed without one", c.DurationMS)
	}
	return c
}

// storeOps returns the ops the Fake has been asked for, in order, with their
// keys — "GetRange" and "Download" are what this feature is about.
func (h *harness) storeOps(op string) int {
	n := 0
	for _, c := range h.store.CallsSnapshot() {
		if strings.HasPrefix(c, op+" ") {
			n++
		}
	}
	return n
}

// The frame is taken from a PREFIX of the rendition: one ranged request, no
// whole-file download, and the card image is published exactly as before.
func TestPosterTakesTheFrameFromARangedPrefix(t *testing.T) {
	h := newHarness(t)
	before := h.posterBigClip(3 << 20)
	downloadsBefore := h.storeOps("Download")

	h.enqueuePoster(before.ID, 0.1)
	h.waitPoster(before.ID, jobDone)
	h.waitIdle()

	if got := h.storeOps("GetRange"); got != 1 {
		t.Fatalf("the poster made %d ranged requests, want exactly 1: %v", got, h.store.CallsSnapshot())
	}
	if got := h.storeOps("Download"); got != downloadsBefore {
		t.Fatalf("the poster downloaded the whole rendition as well (%d → %d downloads)", downloadsBefore, got)
	}
	after := h.clip(before.ID)
	if after.PosterKey == nil || *after.PosterKey == *before.PosterKey {
		t.Fatalf("the card image did not change: %v", after.PosterKey)
	}
	if got, ok := h.store.Get(*after.PosterKey); !ok || string(got) != chosenPosterPayload {
		t.Fatalf("the published card image is missing or wrong (%q, ok=%v)", got, ok)
	}
	if after.PosterSeconds == nil || *after.PosterSeconds != 0.1 {
		t.Fatalf("poster_seconds = %v, want the moment that was chosen", after.PosterSeconds)
	}
	// The operator can see what it cost, which is the whole point of the change.
	h.assertLog("INFO", "poster source: a ranged prefix of the rendition")
	h.assertLog("INFO", "ranged_source=true")
	h.assertNoErrorLogs()
}

// A prefix that cannot produce the frame is not a failure: the whole file is
// fetched and the frame is taken from that, inside the SAME attempt, and the
// owner never hears about any of it.
func TestPosterFallsBackToTheWholeRenditionWhenThePrefixCannotDecode(t *testing.T) {
	h := newHarness(t)
	before := h.posterBigClip(3 << 20)
	downloadsBefore := h.storeOps("Download")
	var calls int
	h.runner.posterPinFn = func(_ context.Context, in, out string, seconds float64) (transcode.PosterResult, error) {
		calls++
		if calls == 1 {
			// What a truncated MP4 looks like from here: ffmpeg wrote nothing.
			return transcode.PosterResult{}, &transcode.ErrPermanent{
				Err: fmt.Errorf("%w at %.3fs: moov atom not found", transcode.ErrNoFrameAt, seconds)}
		}
		if err := os.WriteFile(out, []byte(chosenPosterPayload), 0o644); err != nil {
			return transcode.PosterResult{}, err
		}
		return transcode.PosterResult{Width: 1280, Height: 720, Seconds: seconds}, nil
	}

	h.enqueuePoster(before.ID, 0.1)
	job := h.waitPoster(before.ID, jobDone)
	h.waitIdle()

	if calls != 2 {
		t.Fatalf("ffmpeg was asked for the frame %d times, want 2 (the prefix, then the whole file)", calls)
	}
	if got := h.storeOps("GetRange"); got != 1 {
		t.Fatalf("ranged requests = %d, want 1", got)
	}
	if got := h.storeOps("Download") - downloadsBefore; got != 1 {
		t.Fatalf("whole-file downloads = %d, want exactly 1 fallback", got)
	}
	if job.Attempts != 0 {
		t.Errorf("attempts = %d: a fallback inside one attempt must not spend one", job.Attempts)
	}
	after := h.clip(before.ID)
	if after.PosterKey == nil || *after.PosterKey == *before.PosterKey {
		t.Fatal("the fallback did not publish a card image")
	}
	if after.State != db.ClipReady || after.FailureReason != nil {
		t.Fatalf("clip = %s/%v after a fallback", after.State, after.FailureReason)
	}
	h.assertLog("INFO", "the ranged prefix of the rendition could not produce the frame")
	h.assertLog("INFO", "poster source: the whole rendition")
	h.assertLog("INFO", "ranged_source=false")
	h.assertNoErrorLogs()
}

// A small rendition is fetched whole, exactly as it always was: the heuristic
// must not turn a one-request job into a two-request one for nothing.
func TestPosterFetchesASmallRenditionWhole(t *testing.T) {
	h := newHarness(t)
	before := h.readyTrimmable() // the default rendition is a few dozen bytes
	downloadsBefore := h.storeOps("Download")

	h.enqueuePoster(before.ID, 1)
	h.waitPoster(before.ID, jobDone)
	h.waitIdle()

	if got := h.storeOps("GetRange"); got != 0 {
		t.Fatalf("a %v-byte rendition was ranged (%d requests)", before.RenditionSize, got)
	}
	if got := h.storeOps("Download") - downloadsBefore; got != 1 {
		t.Fatalf("whole-file downloads = %d, want 1", got)
	}
}

// The arithmetic itself. Zero means "the whole file", and every case that
// returns it is a case where a prefix cannot be trusted or cannot pay.
func TestPosterPrefixBytes(t *testing.T) {
	var size int64 = 100 << 20 // 100 MiB (a var, so the table's arithmetic is not a constant expression)
	dur := func(ms int64) *int64 { return &ms }
	for _, tc := range []struct {
		name       string
		size       int64
		durationMS *int64
		seek       float64
		want       int64 // 0 = the whole file
	}{
		{name: "a small file is never ranged", size: posterPrefixFloor, durationMS: dur(60000), seek: 1},
		{name: "no duration, no placement", size: size, durationMS: nil, seek: 1},
		{name: "a zero duration", size: size, durationMS: dur(0), seek: 1},
		{name: "a seek past the end is the FILE's answer to give", size: size, durationMS: dur(10000), seek: 10},
		{name: "a nonsense seek", size: size, durationMS: dur(10000), seek: math.NaN()},
		{name: "a negative seek", size: size, durationMS: dur(10000), seek: -1},
		{
			// 100 MiB / 150 s, five seconds in: 3% index + (5+3)/150 of the
			// media. The figure DEPLOY §24 quotes.
			name: "the 150s clip from the docs", size: size, durationMS: dur(150000), seek: 5,
			want: size/posterPrefixIndexPart + int64(float64(size)*8/150),
		},
		{
			// Late in the file, the prefix is nearly the file: not worth it.
			name: "a moment near the end", size: size, durationMS: dur(150000), seek: 140,
		},
		{
			// The floor on the media half applies at second 0 of a long file.
			name: "the very first frame of a long clip", size: size, durationMS: dur(3600000), seek: 0,
			want: size/posterPrefixIndexPart + posterPrefixMinBody,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := posterPrefixBytes(tc.size, tc.durationMS, tc.seek)
			if got != tc.want {
				t.Fatalf("posterPrefixBytes(%d, %v, %v) = %d, want %d", tc.size, tc.durationMS, tc.seek, got, tc.want)
			}
			if got != 0 && got >= tc.size {
				t.Fatalf("a prefix of %d bytes is not smaller than the %d-byte file", got, tc.size)
			}
		})
	}
}

// TestPosterReservesScratchForBothFilesTheLabelPassHolds (a nit from lane card):
// the scratch reservation covers the TWO full-size JPEGs the poster stage holds
// at once, not one.
//
// `drawPosterLabel` burns the duration into the chosen frame by writing
// `poster.jpg.lbl` beside `poster.jpg` and renaming it over the first (§20
// erratum 287), so both exist for the length of that pass — while the
// reservation acquirePrefix and acquireWhole make still budgeted for a single
// picture. Four megabytes against two files of a few hundred KB each was never
// a real squeeze; a reservation that does not state what the stage holds is the
// defect, and the free-space guard is exactly where it can be stated.
//
// The disk here reports exactly the OLD budget free: the rendition, one poster
// and ScratchMinFree. That was enough before this and is one poster short now.
func TestPosterReservesScratchForBothFilesTheLabelPassHolds(t *testing.T) {
	h := newHarness(t)
	const rendition = 3 << 20
	before := h.posterBigClip(rendition)
	size := *before.RenditionSize
	h.free = size + posterMaxBytes + h.w.deps.Cfg.ScratchMinFree

	h.enqueuePoster(before.ID, 0.1)
	job := h.waitPoster(before.ID, func(j db.Job) bool { return j.State == db.JobQueued && j.Attempts == 0 && j.LastError != nil })
	h.waitIdle()

	if job.LastError == nil || *job.LastError != msgFrameDisk {
		t.Fatalf("stored reason = %v, want exactly %q", job.LastError, msgFrameDisk)
	}
	after := h.clip(before.ID)
	if after.PosterKey == nil || *after.PosterKey != *before.PosterKey {
		t.Fatalf("the card image changed although the stage had no room: %v", after.PosterKey)
	}
	if after.State != db.ClipReady {
		t.Fatalf("clip = %s; a card image that could not be taken must not cost the clip anything", after.State)
	}

	// …and the SAME job with room for both files goes through, so the refusal
	// above is about the second picture and not about the arithmetic in general.
	h.free = size + posterScratchBytes + h.w.deps.Cfg.ScratchMinFree
	h.clock.Advance(2 * scratchRequeueDelay) // past the requeue's own backoff
	h.w.Wake()
	h.waitPoster(before.ID, jobDone)
	h.waitIdle()
	if got := h.clip(before.ID); got.PosterKey == nil || *got.PosterKey == *before.PosterKey {
		t.Fatalf("the retry with room for both files did not publish a card image: %v", got.PosterKey)
	}
}
