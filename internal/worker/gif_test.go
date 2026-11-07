package worker

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"clippet/internal/db"
	"clippet/internal/store"
	"clippet/internal/transcode"
)

// The §17 #6 job, from the loop that claims it to the UPDATE that commits
// it. Every test here asserts the same background property from a different
// angle: whatever happens to a preview, the CLIP is ready, playable and
// unchanged afterwards.

func (h *harness) enqueueGIF(clipID string) db.Job {
	h.t.Helper()
	j, err := h.db.EnqueueGIF(context.Background(), clipID, h.clock.Now().Unix())
	if err != nil {
		h.t.Fatalf("EnqueueGIF: %v", err)
	}
	h.w.Wake()
	return j
}

// gifReady prepares a `ready` clip WITHOUT starting the worker, so a test can
// claim and run jobs itself in a deterministic order (the rendition-race test
// needs two jobs claimed and one of them driven by hand).
func (h *harness) gifReady() db.Clip {
	h.t.Helper()
	c, _ := h.newClip("mp4", 1024, true)
	job, _, ok, err := h.db.ClaimJob(context.Background(), h.clock.Now().Unix())
	if err != nil || !ok {
		h.t.Fatalf("ClaimJob: ok=%v err=%v", ok, err)
	}
	rk := store.RenditionKey(h.cfg.S3.KeyPrefix, c.ID, "rendr8r8")
	pk := store.PosterKey(h.cfg.S3.KeyPrefix, c.ID, "rendr8r8")
	h.store.Put(rk, []byte(renditionPayload), "video/mp4")
	h.store.Put(pk, []byte("poster-bytes"), "image/jpeg")
	done, _, err := h.db.FinalizeTranscode(context.Background(), job.ID, c.ID, db.ReadyFields{
		RenditionKey: rk, RenditionSize: int64(len(renditionPayload)), RenditionETag: "the-rendition-etag",
		PosterKey: pk, PosterSize: 12, PosterETag: "poster-etag", PosterWidth: 640, PosterHeight: 360,
		Width: 1280, Height: 720, DurationMS: 6000, FPSNum: 30, FPSDen: 1, HasAudio: true,
	}, false, h.clock.Now().Unix())
	if err != nil || !done {
		h.t.Fatalf("FinalizeTranscode: ok=%v err=%v", done, err)
	}
	return h.clip(c.ID)
}

// claimKind claims the next non-transcode job and asserts its kind (with no
// loop running, the order is the insertion order).
func (h *harness) claimKind(kind string) db.Job {
	h.t.Helper()
	j, _, ok, err := h.db.ClaimMediaJob(context.Background(), h.clock.Now().Unix())
	if err != nil || !ok || j.Kind != kind {
		h.t.Fatalf("ClaimMediaJob = %+v ok=%v err=%v, want a %s job", j, ok, err, kind)
	}
	return j
}

func (h *harness) clip(id string) db.Clip {
	h.t.Helper()
	c, err := h.db.GetClip(context.Background(), id)
	if err != nil {
		h.t.Fatalf("GetClip(%s): %v", id, err)
	}
	return c
}

// waitGIF waits, on the settle hook, for the clip's latest GIF job to match pred with its run settled (awaitJob).
func (h *harness) waitGIF(clipID string, pred func(db.Job) bool) db.Job {
	h.t.Helper()
	return h.awaitJob(db.KindGIF+" job for "+clipID, h.latestOfKind(clipID, db.KindGIF), pred)
}

// TestGIFIsRenderedFromTheRenditionAndCommittedOnce is the happy path: the
// non-transcode loop claims the `gif` kind, ffmpeg is pointed at the
// RENDITION (not the original), the object lands in the bucket and one
// UPDATE fills the five columns while the clip stays ready.
func TestGIFIsRenderedFromTheRenditionAndCommittedOnce(t *testing.T) {
	h := newHarness(t)
	before := h.readyTrimmable()

	h.enqueueGIF(before.ID)
	h.waitGIF(before.ID, jobDone)
	h.waitIdle()

	after, err := h.db.GetClip(context.Background(), before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != db.ClipReady || after.FailureReason != nil {
		t.Fatalf("clip is %q/%v after a gif; a gif never marks its clip", after.State, after.FailureReason)
	}
	if after.GIFKey == nil || after.GIFSize == nil || after.GIFETag == nil || after.GIFWidth == nil || after.GIFHeight == nil {
		t.Fatalf("gif columns not filled: %+v", after)
	}
	if !strings.Contains(*after.GIFKey, "/renditions/preview-") || !strings.HasSuffix(*after.GIFKey, ".gif") {
		t.Errorf("gif key %q is not the §6 shape", *after.GIFKey)
	}
	if got, ok := h.store.Get(*after.GIFKey); !ok || string(got) != gifPayload {
		t.Errorf("the gif object is missing or wrong (%q, ok=%v)", got, ok)
	}
	if *after.GIFWidth != 480 || *after.GIFHeight != 270 {
		t.Errorf("gif dims %dx%d, want the MEASURED 480x270", *after.GIFWidth, *after.GIFHeight)
	}
	if *after.GIFSize != int64(len(gifPayload)) {
		t.Errorf("gif size %d, want %d", *after.GIFSize, len(gifPayload))
	}
	// The rendition, the poster and the ORIGINAL are all untouched.
	if *after.RenditionKey != *before.RenditionKey || *after.PosterKey != *before.PosterKey {
		t.Error("a gif job changed the rendition or the poster")
	}
	if _, ok := h.store.Get(before.SourceKey); !ok {
		t.Error("the original is gone; the upload is sacred")
	}
	// ffmpeg was handed the rendition, never the original.
	var sawGIF bool
	for _, call := range h.runner.Calls() {
		if strings.HasPrefix(call, "GIF ") {
			sawGIF = true
			if !strings.HasPrefix(call, "GIF gifsrc-") {
				t.Errorf("the gif pass read %q, want the downloaded rendition", call)
			}
		}
	}
	if !sawGIF {
		t.Fatal("the gif pass never ran")
	}
	// And the plan is the bounded one: the clip is 6000 ms of 1280 px.
	plans := h.runner.GIFPlans()
	if len(plans) != 1 || plans[0].Width != transcode.GIFMaxWidth || plans[0].FPS != transcode.GIFFPS {
		t.Errorf("gif plans = %+v, want one bounded plan", plans)
	}
	h.assertNoErrorLogs()
	h.assertLog("INFO", "gif applied")
	// Nothing was written to the clip's §8.4 progress registry: its subject
	// is the clip, which was ready throughout.
	if p, ok := h.w.Progress(before.ID); ok && p.Stage == stageGIF {
		t.Errorf("the gif job wrote the clip's progress registry: %+v", p)
	}
}

// TestGIFSkippedWhenTheRenditionChanged is the stale-preview guard: a trim
// that commits while the gif renders replaces the frames the preview was cut
// from, so the attempt publishes nothing and resolves as done/skipped.
//
// It drives the run by hand: both jobs must be claimed in a known order and
// the swap must land BETWEEN the run's first read of the clip and its
// commit, which no scheduling of the loop can be relied on to produce.
func TestGIFSkippedWhenTheRenditionChanged(t *testing.T) {
	h := newHarness(t)
	c := h.gifReady()
	h.enqueueGIF(c.ID)
	if _, err := h.db.EnqueueTrim(context.Background(), c.ID,
		db.MarshalTrimParams(db.TrimParams{StartMS: 0, EndMS: 2000}), h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	gifJob := h.claimKind(db.KindGIF)
	trimJob := h.claimKind(db.KindTrim)

	// The gif pass "runs" while that trim commits underneath it.
	h.runner.gifFn = func(_ context.Context, _ transcode.GIFPlan, _, palette, out string) error {
		_, ok, _, err := h.db.SwapRendition(context.Background(), trimJob.ID, c.ID, db.ReadyFields{
			RenditionKey: "renditions/main-newnewnew.mp4", RenditionSize: 10, RenditionETag: "a-different-etag",
			PosterKey: "poster/poster-newnewnew.jpg", PosterSize: 5, PosterETag: "p", PosterWidth: 2, PosterHeight: 2,
			Width: 1280, Height: 720, DurationMS: 2000, FPSNum: 30, FPSDen: 1, HasAudio: true,
		}, db.TrimParams{StartMS: 1000, EndMS: 3000}, h.clock.Now().Unix())
		if err != nil || !ok {
			t.Errorf("the trim did not commit: ok=%v err=%v", ok, err)
		}
		if err := os.WriteFile(palette, []byte("p"), 0o644); err != nil {
			return err
		}
		return os.WriteFile(out, []byte(gifPayload), 0o644)
	}

	newGIFRun(h.w, context.Background(), &run{}, gifJob, c).execute()

	job, err := h.db.LatestJobOfKind(context.Background(), c.ID, db.KindGIF)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != db.JobDone || job.Stage != db.JobStageSkipped {
		t.Errorf("job is %s/%s, want done/%s", job.State, job.Stage, db.JobStageSkipped)
	}
	if job.LastError == nil || !strings.Contains(*job.LastError, "rendition changed") {
		t.Errorf("last_error = %v, want the rendition-changed reason", job.LastError)
	}
	after := h.clip(c.ID)
	if after.GIFKey != nil {
		t.Errorf("a stale preview was published: %q", *after.GIFKey)
	}
	if after.State != db.ClipReady {
		t.Errorf("clip is %q", after.State)
	}
	// The attempt removed its own object rather than leaving it orphaned.
	for _, k := range h.store.Keys() {
		if strings.Contains(k, "/renditions/preview-") {
			t.Errorf("the unpublished preview %q is still in the bucket", k)
		}
	}
	h.assertLog("INFO", "gif skipped")
	h.assertNoErrorLogs()
}

// TestGIFFailurePreservesTheClip: a permanent ffmpeg failure fails the JOB,
// names the reason, and leaves the clip ready on the rendition it had.
func TestGIFFailurePreservesTheClip(t *testing.T) {
	h := newHarness(t)
	c := h.readyTrimmable()
	h.runner.gifFn = func(context.Context, transcode.GIFPlan, string, string, string) error {
		return &transcode.ErrPermanent{Err: errors.New("gif_timeout: not finished within 1m0s")}
	}

	h.enqueueGIF(c.ID)
	job := h.waitGIF(c.ID, jobFailed)
	h.waitIdle()

	if job.LastError == nil || !strings.Contains(*job.LastError, "gif_timeout") {
		t.Errorf("last_error = %v", job.LastError)
	}
	if !strings.HasPrefix(*job.LastError, stageGIF+": ") {
		t.Errorf("last_error %q does not name the stage it died in", *job.LastError)
	}
	after := h.clip(c.ID)
	if after.State != db.ClipReady || after.FailureReason != nil || after.GIFKey != nil {
		t.Fatalf("a failed preview changed the clip: state=%q reason=%v gif=%v", after.State, after.FailureReason, after.GIFKey)
	}
	if _, ok := h.store.Get(*after.RenditionKey); !ok {
		t.Error("the clip lost its rendition")
	}
	h.assertLog("INFO", "gif failed")
}

// TestGIFTransientFailureStopsAtTheConfiguredBudget: a preview's ENVIRONMENT
// cap is the instance's TRANSCODE_MAX_TRANSIENT, not db.GIFMaxAttempts (§20
// erratum 89) — an outage costs a preview what it costs an encode. Two here,
// because that is what this instance is configured with.
func TestGIFTransientFailureStopsAtTheConfiguredBudget(t *testing.T) {
	h := newHarness(t)
	// Set before the worker starts, like every other cap this suite bends.
	h.w.lim = db.Limits{MaxAttempts: 3, MaxTransient: 2}
	c := h.readyTrimmable()
	h.runner.gifFn = func(context.Context, transcode.GIFPlan, string, string, string) error {
		return errors.New("scratch is on fire")
	}

	h.enqueueGIF(c.ID)
	h.waitGIF(c.ID, func(j db.Job) bool { return j.State == db.JobQueued && j.TransientFailures == 1 })
	// The transient backoff holds the retry for a minute; the harness clock is
	// what the claim compares run_after against.
	h.clock.Advance(2 * time.Minute)
	h.w.Wake()
	final := h.waitGIF(c.ID, jobFailed)
	h.waitIdle()

	if final.TransientFailures != h.w.lim.MaxTransient {
		t.Errorf("transient_failures = %d, want the configured %d", final.TransientFailures, h.w.lim.MaxTransient)
	}
	if after := h.clip(c.ID); after.State != db.ClipReady {
		t.Errorf("clip is %q after two failed previews", after.State)
	}
}

// TestGIFSkipsAClipThatIsNoLongerReady: the claim→run window. A clip
// tombstoned between the enqueue and the run is skipped, not failed.
func TestGIFSkipsAClipThatIsNoLongerReady(t *testing.T) {
	h := newHarness(t)
	c := h.readyTrimmable()
	gr := newGIFRun(h.w, context.Background(), &run{}, db.Job{ID: 1, ClipID: c.ID, Kind: db.KindGIF}, c)
	if err := h.db.MarkDeleting(context.Background(), c.ID, h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	err := gr.recheckClip()
	if !errors.Is(err, errSkipped) || !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("recheckClip = %v, want a skip", err)
	}
}

// TestGIFRefusesAnOversizeOutput: the byte cap is checked on the FILE too,
// not only on what ffmpeg reported while it ran.
func TestGIFRefusesAnOversizeOutput(t *testing.T) {
	h := newHarness(t)
	c := h.readyTrimmable()
	h.runner.gifFn = func(_ context.Context, _ transcode.GIFPlan, _, palette, out string) error {
		if err := os.WriteFile(palette, []byte("p"), 0o644); err != nil {
			return err
		}
		return os.WriteFile(out, make([]byte, transcode.GIFMaxBytes+1), 0o644)
	}

	h.enqueueGIF(c.ID)
	job := h.waitGIF(c.ID, jobFailed)
	h.waitIdle()

	if job.LastError == nil || !strings.Contains(*job.LastError, "output_too_large") {
		t.Fatalf("last_error = %v, want output_too_large", job.LastError)
	}
	if after := h.clip(c.ID); after.GIFKey != nil {
		t.Error("an oversize preview was published")
	}
	for _, k := range h.store.Keys() {
		if strings.Contains(k, "/renditions/preview-") {
			t.Errorf("the oversize preview %q reached the bucket", k)
		}
	}
}

// TestGIFDriftOnTheRenditionIsTransient: a ready row pointing at an object
// the bucket does not hold is an ERROR log and a retry, never a clip that
// stops being ready.
func TestGIFDriftOnTheRenditionIsTransient(t *testing.T) {
	h := newHarness(t)
	c := h.readyTrimmable()
	if _, err := h.store.DeletePrefix(context.Background(), *c.RenditionKey); err != nil {
		t.Fatal(err)
	}

	h.enqueueGIF(c.ID)
	h.waitGIF(c.ID, func(j db.Job) bool { return j.TransientFailures >= 1 })
	h.waitIdle()

	if after := h.clip(c.ID); after.State != db.ClipReady {
		t.Errorf("clip is %q after a missing rendition", after.State)
	}
	h.assertLog("ERROR", "row/object drift")
}

// TestGIFIsCancelledSynchronously pins the erratum-71 property for the new
// kind: Cancel returns only after the run has finished, so lifecycle.Destroy
// can never delete a prefix an ffmpeg of ours is still writing into.
func TestGIFIsCancelledSynchronously(t *testing.T) {
	h := newHarness(t)
	c := h.readyTrimmable()
	started := make(chan struct{})
	release := make(chan struct{})
	h.runner.gifFn = func(_ context.Context, _ transcode.GIFPlan, _, palette, out string) error {
		close(started)
		<-release // deliberately ignores its context, like a wedged ffmpeg
		if err := os.WriteFile(palette, []byte("p"), 0o644); err != nil {
			return err
		}
		return os.WriteFile(out, []byte(gifPayload), 0o644)
	}

	h.enqueueGIF(c.ID)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatalf("the gif pass never started\nlogs:\n%s", h.logs.String())
	}
	done := make(chan struct{})
	go func() { h.w.Cancel(context.Background(), c.ID); close(done) }()
	select {
	case <-done:
		t.Fatal("Cancel returned while the gif pass was still running")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Cancel never returned")
	}
}

// TestGIFPreviewKeyIsUnderTheClipPrefix keeps the key inside the prefix that
// clip deletion removes — the reason nothing extra is needed in Destroy.
func TestGIFPreviewKeyIsUnderTheClipPrefix(t *testing.T) {
	key := store.GIFKey("p/", "aZ3kQ9xB2mLp", "r8r8r8r8")
	if prefix := store.ClipPrefix("p/", "aZ3kQ9xB2mLp"); !strings.HasPrefix(key, prefix) {
		t.Fatalf("%q is not under %q", key, prefix)
	}
}
