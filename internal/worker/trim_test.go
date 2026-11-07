package worker

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"clippet/internal/db"
	"clippet/internal/transcode"
)

// trimmedOutput is the measurement a trim's ProbeOutput must return for the
// worker's own duration guard to accept it (the fake encoder writes the same
// bytes whatever the window, so the measurement has to be told).
func trimmedOutput(ms int64) func(context.Context, string) (*transcode.OutputInfo, error) {
	return func(context.Context, string) (*transcode.OutputInfo, error) {
		return &transcode.OutputInfo{Width: 1280, Height: 720, FPS: transcode.Rational{Num: 30, Den: 1},
			DurationMS: ms, HasAudio: true, VideoCodec: "h264", AudioCodec: "aac", VideoStreams: 1, AudioStreams: 1}, nil
	}
}

// readyTrimmable drives a clip all the way to `ready` through the real
// pipeline and returns it, so every trim test starts where a trim can start.
// From then on the runner measures a 2000 ms output — the window every test
// below asks for.
func (h *harness) readyTrimmable() db.Clip {
	h.t.Helper()
	c, _ := h.newClip("mp4", 1024, true)
	h.start()
	h.waitClip(c.ID, db.ClipReady)
	h.waitIdle()
	got, err := h.db.GetClip(context.Background(), c.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	h.runner.probeOutFn = trimmedOutput(2000)
	return got
}

func (h *harness) enqueueTrim(clipID string, start, end int64) db.Job {
	h.t.Helper()
	j, err := h.db.EnqueueTrim(context.Background(), clipID,
		db.MarshalTrimParams(db.TrimParams{StartMS: start, EndMS: end}), h.clock.Now().Unix())
	if err != nil {
		h.t.Fatalf("EnqueueTrim: %v", err)
	}
	h.w.Wake()
	return j
}

// waitTrim waits, on the settle hook, for the clip's latest TRIM job to match pred with its run settled (awaitJob).
func (h *harness) waitTrim(clipID string, pred func(db.Job) bool) db.Job {
	h.t.Helper()
	return h.awaitJob(db.KindTrim+" job for "+clipID, h.latestOfKind(clipID, db.KindTrim), pred)
}

func jobDone(j db.Job) bool   { return j.State == db.JobDone }
func jobFailed(j db.Job) bool { return j.State == db.JobFailed }

// copyableProbe is a source that satisfies every cheap stream-copy rule, so
// only the keyframe grid decides the path.
func copyableProbe() *transcode.ProbeResult {
	p := defaultProbeResult()
	p.PixFmt, p.FormatName = "yuv420p", "mov,mp4,m4a,3gp,3g2,mj2"
	p.AudioCodecs = []string{"aac"}
	return p
}

// TestTrimSwapsTheRenditionAndKeepsTheClipReady is the whole happy path: the
// non-transcode loop claims the `trim` kind, the clip never leaves `ready`,
// one UPDATE swaps the keys, and the superseded objects are gone afterwards.
func TestTrimSwapsTheRenditionAndKeepsTheClipReady(t *testing.T) {
	h := newHarness(t)
	before := h.readyTrimmable()
	oldRendition, oldPoster := *before.RenditionKey, *before.PosterKey

	job := h.enqueueTrim(before.ID, 1000, 3000)
	h.waitTrim(before.ID, jobDone)
	h.waitIdle()

	after, err := h.db.GetClip(context.Background(), before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != db.ClipReady || after.FailureReason != nil {
		t.Fatalf("clip is %q/%v after a trim; a trim never marks its clip", after.State, after.FailureReason)
	}
	if *after.RenditionKey == oldRendition || *after.PosterKey == oldPoster {
		t.Fatalf("keys were not swapped (rendition %q, poster %q)", *after.RenditionKey, *after.PosterKey)
	}
	if *after.DurationMS != 2000 {
		t.Errorf("duration_ms = %d, want the trimmed 2000", *after.DurationMS)
	}
	// The superseded objects are gone and the ORIGINAL is untouched.
	if _, ok := h.store.Get(oldRendition); ok {
		t.Error("the superseded rendition is still in the bucket")
	}
	if _, ok := h.store.Get(oldPoster); ok {
		t.Error("the superseded poster is still in the bucket")
	}
	if _, ok := h.store.Get(before.SourceKey); !ok {
		t.Error("the original is gone; the upload is sacred")
	}
	if _, ok := h.store.Get(*after.RenditionKey); !ok {
		t.Error("the new rendition is not in the bucket")
	}
	for _, d := range h.store.Deletes() {
		if strings.Contains(d, "/original/") {
			t.Errorf("a trim deleted an original: %s", d)
		}
	}
	got := h.waitTrim(before.ID, jobDone)
	if got.ID != job.ID || got.Stage != "done" {
		t.Errorf("job = %+v, want %d done", got, job.ID)
	}
	h.assertNoErrorLogs()
}

// TestTrimTakesTheCopyPathOnKeyframes pins the decision plumbing: a source
// that is already the pipeline's shape, cut on the fake runner's keyframe
// grid, is remuxed rather than re-encoded — and the window handed to ffmpeg
// is the snapped one.
func TestTrimTakesTheCopyPathOnKeyframes(t *testing.T) {
	h := newHarness(t)
	h.runner.probeFn = func(context.Context, string) (*transcode.ProbeResult, error) { return copyableProbe(), nil }
	c := h.readyTrimmable()
	h.enqueueTrim(c.ID, 1000, 3000)
	h.waitTrim(c.ID, jobDone)
	h.waitIdle()

	calls := strings.Join(h.runner.Calls(), " ")
	if !strings.Contains(calls, "CopyTrim") {
		t.Fatalf("expected a stream copy; calls were %s\nlogs:\n%s", calls, h.logs.String())
	}
	if !strings.Contains(calls, "Keyframes") {
		t.Error("the copy decision must have asked for the keyframe list")
	}
	wins := h.runner.Windows()
	if len(wins) != 1 || wins[0].Start != 1 || wins[0].Duration != 2 {
		t.Errorf("window = %+v, want {1 2}", wins)
	}
	if !strings.Contains(h.logs.String(), "path=copy") {
		t.Error("the log must name the path an operator would otherwise have to guess")
	}
	if j := h.waitTrim(c.ID, jobDone); j.State != db.JobDone {
		t.Errorf("job = %+v", j)
	}
	h.assertNoErrorLogs()
}

// TestTrimReencodesWhenItMust is the same run for a source the copy rules
// reject; the window is then the exact request, not a snapped one.
func TestTrimReencodesWhenItMust(t *testing.T) {
	h := newHarness(t)
	h.runner.probeFn = func(context.Context, string) (*transcode.ProbeResult, error) {
		p := copyableProbe()
		p.FormatName = "matroska,webm" // a webm can never be copied into an mp4
		return p, nil
	}
	c := h.readyTrimmable()
	h.enqueueTrim(c.ID, 1234, 3234)
	h.waitTrim(c.ID, jobDone)
	h.waitIdle()

	calls := strings.Join(h.runner.Calls(), " ")
	if strings.Contains(calls, "CopyTrim") {
		t.Fatalf("a webm source must be re-encoded: %s", calls)
	}
	if strings.Contains(calls, "Keyframes") {
		t.Error("a source disqualified on a cheap rule must not pay for a keyframe demux")
	}
	wins := h.runner.Windows()
	if len(wins) != 1 || wins[0].Start != 1.234 || wins[0].Duration != 2 {
		t.Errorf("window = %+v, want {1.234 2}", wins)
	}
	if !strings.Contains(h.logs.String(), "path=reencode") {
		t.Error("the log must name the path")
	}
	h.assertNoErrorLogs()
}

// TestTrimFailurePreservesTheClip: a permanent ending fails the job, leaves
// the clip playable on the rendition it had, and takes nothing with it.
func TestTrimFailurePreservesTheClip(t *testing.T) {
	h := newHarness(t)
	before := h.readyTrimmable()
	oldRendition := *before.RenditionKey
	h.runner.encodeFn = func(context.Context, transcode.Plan, string, string, func(float64, bool)) error {
		return &transcode.ErrPermanent{Err: errors.New("ffmpeg exited 1: no")}
	}
	h.enqueueTrim(before.ID, 1000, 3000)
	j := h.waitTrim(before.ID, jobFailed)
	// The OWNER's text is one of the fixed sentences, and carries no ffmpeg
	// output at all (§20 erratum 80); the detail is in the log instead.
	if j.LastError == nil || *j.LastError != msgMedia {
		t.Errorf("last_error = %v, want the plain %q", j.LastError, msgMedia)
	}
	if strings.Contains(*j.LastError, "ffmpeg") {
		t.Errorf("last_error carries ffmpeg's own words: %q", *j.LastError)
	}
	// AFTER waitIdle, never before: waitTrim polls the job ROW, and the run
	// commits that row (DB.FailAttempt) BEFORE it writes the log line carrying
	// ffmpeg's tail. Reading the buffer the instant the row settles is a race.
	h.waitIdle()
	if !strings.Contains(h.logs.String(), "ffmpeg exited 1") {
		t.Error("the ffmpeg detail is not in the log; it is now nowhere at all")
	}

	after, err := h.db.GetClip(context.Background(), before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != db.ClipReady || after.FailureReason != nil {
		t.Fatalf("clip = %q/%v; a failed trim is not a failed clip", after.State, after.FailureReason)
	}
	if *after.RenditionKey != oldRendition {
		t.Errorf("rendition changed to %q despite the failure", *after.RenditionKey)
	}
	if _, ok := h.store.Get(oldRendition); !ok {
		t.Error("the clip's rendition was deleted by a failed trim")
	}
	// A permanent failure is INFO, not ERROR: the clip is fine.
	h.assertNoErrorLogs()
}

// TestTrimCrashCapIsTwo pins db.TrimMaxAttempts through the worker: the
// config's generous TRANSCODE_MAX_ATTEMPTS must not reach a trim.
func TestTrimCrashCapIsTwo(t *testing.T) {
	h := newHarness(t)
	c := h.readyTrimmable()
	h.runner.encodeFn = func(context.Context, transcode.Plan, string, string, func(float64, bool)) error {
		panic("boom")
	}
	h.enqueueTrim(c.ID, 1000, 3000)
	h.waitTrim(c.ID, func(j db.Job) bool { return j.Attempts == 1 && j.State == db.JobQueued })
	// The crash-class backoff is 30s; move the clock past it.
	h.clock.Advance(time.Minute)
	h.w.Wake()
	j := h.waitTrim(c.ID, jobFailed)
	if j.Attempts != db.TrimMaxAttempts {
		t.Errorf("attempts = %d, want the trim cap %d (TRANSCODE_MAX_ATTEMPTS is %d)",
			j.Attempts, db.TrimMaxAttempts, h.cfg.Transcode.MaxAttempts)
	}
	h.waitIdle()
	if got, _ := h.db.GetClip(context.Background(), c.ID); got.State != db.ClipReady {
		t.Errorf("clip = %q after a panicking trim", got.State)
	}
}

// TestTrimLostRaceDeletesItsOwnOutputs: the clip is tombstoned mid-run, so
// the swap must not happen and the attempt must take its objects with it.
func TestTrimLostRaceDeletesItsOwnOutputs(t *testing.T) {
	h := newHarness(t)
	c := h.readyTrimmable()
	before, err := h.db.GetClip(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.runner.encodeFn = func(_ context.Context, _ transcode.Plan, _, out string, _ func(float64, bool)) error {
		// Tombstone the clip after the encode but before the upload + swap.
		if err := h.db.MarkDeleting(context.Background(), c.ID, h.clock.Now().Unix()); err != nil {
			return err
		}
		return os.WriteFile(out, []byte(renditionPayload), 0o644)
	}
	h.enqueueTrim(c.ID, 1000, 3000)
	h.waitTrim(c.ID, func(j db.Job) bool { return j.State == db.JobCancelled })
	h.waitIdle()

	if !strings.Contains(h.logs.String(), "lost_race") {
		t.Errorf("expected the lost-race path\nlogs:\n%s", h.logs.String())
	}
	// Only the PRE-trim objects (which lifecycle.Destroy will remove with the
	// prefix) may still be there; nothing the lost attempt uploaded.
	keep := map[string]bool{before.SourceKey: true, *before.RenditionKey: true, *before.PosterKey: true}
	for _, k := range h.store.Keys() {
		if !keep[k] && !strings.HasSuffix(k, "probe.json") {
			t.Errorf("the lost attempt left %s behind", k)
		}
	}
	h.assertNoErrorLogs()
}

// TestTrimIsCancelledSynchronously is the reason a trim is REGISTERED while
// a notify is not: Cancel must not return while our ffmpeg is still writing
// into a prefix lifecycle.Destroy is about to delete.
func TestTrimIsCancelledSynchronously(t *testing.T) {
	h := newHarness(t)
	c := h.readyTrimmable()
	inEncode := make(chan struct{})
	release := make(chan struct{})
	// Deliberately DEAF to ctx: a real ffmpeg takes until cmd.Wait returns,
	// and the promise Cancel makes is that it waits for exactly that.
	h.runner.encodeFn = func(_ context.Context, _ transcode.Plan, _, out string, _ func(float64, bool)) error {
		close(inEncode)
		<-release
		return os.WriteFile(out, []byte(renditionPayload), 0o644)
	}
	h.enqueueTrim(c.ID, 1000, 3000)
	<-inEncode
	done := make(chan struct{})
	go func() { h.w.Cancel(context.Background(), c.ID); close(done) }()
	select {
	case <-done:
		t.Fatal("Cancel returned while the trim's ffmpeg was still in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Cancel never returned")
	}
	h.waitIdle()
}

// TestTrimProgressIsItsOwnRegistry: a trim must never make a ready clip poll
// as a processing one (§8.4 stays exactly what it was).
func TestTrimProgressIsItsOwnRegistry(t *testing.T) {
	h := newHarness(t)
	c := h.readyTrimmable()
	inEncode := make(chan struct{})
	release := make(chan struct{})
	h.runner.encodeFn = func(_ context.Context, _ transcode.Plan, _, out string, onProgress func(float64, bool)) error {
		// Past the registry's 250 ms rate limit, so the line actually lands.
		h.clock.Advance(300 * time.Millisecond)
		onProgress(0.5, false)
		close(inEncode)
		<-release
		return os.WriteFile(out, []byte(renditionPayload), 0o644)
	}
	h.enqueueTrim(c.ID, 1000, 3000)
	<-inEncode

	tp, ok := h.w.TrimProgress(c.ID)
	if !ok || tp.State != db.JobRunning || tp.Percent <= bands[stageEncode][0] {
		t.Errorf("TrimProgress = %+v (%v), want a running entry past the encode band floor", tp, ok)
	}
	if p, ok := h.w.Progress(c.ID); !ok || p.State != db.ClipReady || p.Percent != 100 {
		t.Errorf("Progress = %+v (%v); the CLIP is ready and must keep reporting so", p, ok)
	}
	close(release)
	h.waitTrim(c.ID, jobDone)
	h.waitIdle()
	if _, ok := h.w.TrimProgress(c.ID); ok {
		t.Error("a finished trim must leave no registry entry; the job row is the truth from there")
	}
}

// TestTrimRecheckSkipsAClipThatLeftReady: a job that outlives the state it
// was queued for is RESOLVED, not left running forever.
func TestTrimRecheckSkipsAClipThatLeftReady(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 1024, true) // queued, never transcoded
	tr := newTrimRun(h.w, context.Background(), &run{clipID: c.ID}, db.Job{ID: 1, ClipID: c.ID}, c)
	err := tr.recheckClip()
	if !errors.Is(err, errSkipped) || !strings.Contains(err.Error(), "clip is queued") {
		t.Errorf("recheckClip on a queued clip = %v, want a skip naming the state", err)
	}
	if err := h.db.MarkDeleting(context.Background(), c.ID, h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := tr.recheckClip(); !errors.Is(err, errSkipped) {
		t.Errorf("recheckClip on a tombstone = %v, want a skip", err)
	}
}

// TestTrimClaimedByTheNonTranscodeLoop pins the queue partition for the new
// kind: the trim runs on the OTHER loop, never on the encode queue.
func TestTrimClaimedByTheNonTranscodeLoop(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	c := h.readyTrimmable()
	h.enqueueTrim(c.ID, 1000, 3000)
	h.waitTrim(c.ID, jobDone)
	h.waitIdle()

	// ClaimJob must never see a trim: enqueue another and prove the transcode
	// claim skips it.
	if _, err := h.db.EnqueueTrim(ctx, c.ID, db.MarshalTrimParams(db.TrimParams{StartMS: 0, EndMS: 1000}), h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := h.db.ClaimJob(ctx, h.clock.Now().Unix()+1); err != nil || ok {
		t.Errorf("ClaimJob took a trim job (ok=%v, err=%v)", ok, err)
	}
}

// TestTrimRefusesAWindowWithNoVideo is F2's regression at the worker level: a
// window past the end of a video stream that stops before its container is a
// CLIENT condition. It must fail permanently, at INFO, with a plain sentence
// in the row and no ffmpeg run at all — an owner must not be able to put an
// ERROR line in an operator's log by picking the wrong seconds.
func TestTrimRefusesAWindowWithNoVideo(t *testing.T) {
	h := newHarness(t)
	h.runner.probeFn = func(context.Context, string) (*transcode.ProbeResult, error) {
		p := defaultProbeResult()
		// Two seconds of video inside an eight-second container.
		p.HasDuration, p.DurationUS = true, 8_000_000
		p.Video = transcode.StreamExtent{EndSec: 2, Known: true}
		p.Audio = transcode.StreamExtent{EndSec: 8, Known: true}
		return p, nil
	}
	c := h.readyTrimmable()
	before, _ := h.db.GetClip(context.Background(), c.ID)
	seen := len(h.runner.Calls()) // the transcode that made the clip ready

	h.enqueueTrim(c.ID, 5000, 9000)
	j := h.waitTrim(c.ID, jobFailed)
	h.waitIdle()

	if j.LastError == nil || *j.LastError != msgNoWindow {
		t.Errorf("last_error = %v, want %q", j.LastError, msgNoWindow)
	}
	if j.Attempts != 0 || j.TransientFailures != 0 {
		t.Errorf("attempts=%d transient=%d; a client condition is permanent, not retried", j.Attempts, j.TransientFailures)
	}
	if calls := strings.Join(h.runner.Calls()[seen:], " "); strings.Contains(calls, "Encode") || strings.Contains(calls, "Copy") {
		t.Errorf("ffmpeg ran for a window with no video in it: %s", calls)
	}
	after, _ := h.db.GetClip(context.Background(), c.ID)
	if after.State != db.ClipReady || *after.RenditionKey != *before.RenditionKey {
		t.Errorf("the clip was disturbed: %q %v", after.State, after.RenditionKey)
	}
	// The whole point: INFO, not ERROR.
	h.assertNoErrorLogs()
	h.assertLog("INFO", "the clip keeps its rendition")
}

// TestTrimDropsAudioOutsideTheWindow: the other half of F2. A window inside
// the video but past the audio is CUT — silently — rather than failed, and
// the end is clamped to the video the file actually has.
func TestTrimDropsAudioOutsideTheWindow(t *testing.T) {
	h := newHarness(t)
	h.runner.probeFn = func(context.Context, string) (*transcode.ProbeResult, error) {
		p := defaultProbeResult()
		p.HasDuration, p.DurationUS = true, 8_000_000
		p.Video = transcode.StreamExtent{EndSec: 8, Known: true}
		p.Audio = transcode.StreamExtent{EndSec: 2, Known: true}
		return p, nil
	}
	var got transcode.Plan
	h.runner.encodeFn = func(_ context.Context, p transcode.Plan, _, out string, onProgress func(float64, bool)) error {
		got = p
		onProgress(1, false)
		return os.WriteFile(out, []byte(renditionPayload), 0o644)
	}
	c := h.readyTrimmable()
	h.runner.probeOutFn = trimmedOutput(3000) // 5000..8000 after the clamp

	h.enqueueTrim(c.ID, 5000, 9000)
	h.waitTrim(c.ID, jobDone)
	h.waitIdle()

	if got.AudioMode != transcode.AudioNone || len(got.AudioIndexes) != 0 {
		t.Errorf("plan audio = %v/%v, want none: the window is outside the audio stream", got.AudioMode, got.AudioIndexes)
	}
	wins := h.runner.Windows()
	if len(wins) != 1 || wins[0].Start != 5 || math.Abs(wins[0].Duration-3) > 0.01 {
		t.Errorf("window = %+v, want {5 3}: the end is clamped to the video's own end", wins)
	}
	after, _ := h.db.GetClip(context.Background(), c.ID)
	if after.TrimStartMS == nil || *after.TrimStartMS != 5000 || *after.TrimEndMS != 8000 {
		t.Errorf("applied window = %v..%v, want 5000..8000", after.TrimStartMS, after.TrimEndMS)
	}
	h.assertNoErrorLogs()
	h.assertLog("INFO", "drops the audio")
}

// TestTrimPersistsTheWindowOnTheOriginalsTimeline: the swap records what was
// CUT, and the source's own length, so a second trim measures itself against
// the upload rather than against the cut (§20 erratum 68).
func TestTrimPersistsTheWindowOnTheOriginalsTimeline(t *testing.T) {
	h := newHarness(t)
	h.runner.probeFn = func(context.Context, string) (*transcode.ProbeResult, error) { return copyableProbe(), nil }
	c := h.readyTrimmable()
	if c.SourceDurationMS == nil || *c.SourceDurationMS != 6000 {
		t.Fatalf("the transcode did not record the original's length: %v", c.SourceDurationMS)
	}
	h.enqueueTrim(c.ID, 1000, 3000)
	h.waitTrim(c.ID, jobDone)
	h.waitIdle()

	after, _ := h.db.GetClip(context.Background(), c.ID)
	if after.TrimStartMS == nil || *after.TrimStartMS != 1000 || *after.TrimEndMS != 3000 {
		t.Errorf("window = %v..%v, want 1000..3000", after.TrimStartMS, after.TrimEndMS)
	}
	if after.SourceDurationMS == nil || *after.SourceDurationMS != 6000 {
		t.Errorf("source_duration_ms = %v after a trim; it describes the UPLOAD and never moves", after.SourceDurationMS)
	}
	if *after.DurationMS != 2000 {
		t.Errorf("duration_ms = %d, want the cut's 2000", *after.DurationMS)
	}
}

// TestFFmpegSlotsBoundTrimAndTranscodeTogether is F4's regression: a trim
// used to run on a loop of its own with no relation to TRANSCODE_CONCURRENCY,
// so a default install — which DEPLOY promises runs ONE ffmpeg — ran two.
// The semaphore is what makes the promise true, and the assertion is the
// number of ffmpeg processes alive at once, not the number of loops.
func TestFFmpegSlotsBoundTrimAndTranscodeTogether(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	live, peak := 0, 0
	enter := func() func() {
		mu.Lock()
		live++
		if live > peak {
			peak = live
		}
		mu.Unlock()
		// Long enough that a second ffmpeg would have to overlap this one.
		time.Sleep(80 * time.Millisecond) // sleep: an overlap window; load can only hide an overlap, never fail the test falsely
		return func() {
			mu.Lock()
			live--
			mu.Unlock()
		}
	}
	h.runner.encodeFn = func(_ context.Context, _ transcode.Plan, _, out string, onProgress func(float64, bool)) error {
		defer enter()()
		onProgress(1, false)
		return os.WriteFile(out, []byte(renditionPayload), 0o644)
	}
	c := h.readyTrimmable() // ready, and the runner is now measuring overlap

	// A trim of the ready clip and a transcode of a fresh upload, racing.
	h.enqueueTrim(c.ID, 1000, 3000)
	other, _ := h.newClip("mp4", 1024, true)
	h.w.Wake()
	h.waitTrim(c.ID, jobDone)
	h.waitClip(other.ID, db.ClipReady)
	h.waitIdle()

	mu.Lock()
	got := peak
	mu.Unlock()
	if got > h.cfg.Transcode.Concurrency {
		t.Errorf("%d ffmpeg runs overlapped with TRANSCODE_CONCURRENCY=%d", got, h.cfg.Transcode.Concurrency)
	}
	h.assertNoErrorLogs()
}

// TestTrimDoesNotStarveTheNotifyLoop is the other half of F4: media work and
// deliveries are separate queues, so a trim that takes minutes cannot hold up
// an announcement that takes milliseconds. The trim here is pinned inside
// ffmpeg — holding the only ffmpeg slot — and the notify must still land.
func TestTrimDoesNotStarveTheNotifyLoop(t *testing.T) {
	h := newHarness(t)
	hk := newHook(t, reply{status: 200})
	h.withNotifier(t, hk.endpoint(), 30*time.Second)

	// Clip A becomes ready first, so its notify job is queued BEFORE the trim.
	a, _ := h.newClip("mp4", 100, true)
	claimed, clip, ok, err := h.db.ClaimJob(context.Background(), h.clock.Now().Unix())
	if err != nil || !ok {
		t.Fatalf("claim: %v", err)
	}
	h.w.runJob(context.Background(), claimed, clip)
	ready := h.clip(a.ID)
	if ready.State != db.ClipReady {
		t.Fatalf("clip A is %q", ready.State)
	}
	// The trim is pinned in ffmpeg until this test lets it go. The release is
	// deferred as well: a failing assertion below must not leave the encode —
	// and therefore Stop — waiting for ever.
	inTrim, release := make(chan struct{}), make(chan struct{})
	var entered, released sync.Once
	unpin := func() { released.Do(func() { close(release) }) }
	defer unpin()
	h.runner.encodeFn = func(_ context.Context, _ transcode.Plan, _, out string, onProgress func(float64, bool)) error {
		entered.Do(func() { close(inTrim) })
		<-release
		onProgress(1, false)
		return os.WriteFile(out, []byte(renditionPayload), 0o644)
	}
	h.runner.probeOutFn = trimmedOutput(2000)

	h.start()
	h.enqueueTrim(a.ID, 1000, 3000)
	<-inTrim

	// The delivery goes out while the trim sits in ffmpeg holding the slot.
	h.awaitJob("the announcement behind the trim", h.latestOfKind(a.ID, db.KindNotify), jobDone)
	if posts := hk.posts(); len(posts) != 1 {
		t.Fatalf("posts = %d, want the one announcement", len(posts))
	}
	unpin()
	h.waitTrim(a.ID, jobDone)
	h.waitIdle()
	h.assertNoErrorLogs()
	h.assertNoWebhookLeak(t, hk)
}

// TestTrimSurvivesLosingItsScratchDir is §20 erratum 89 from the worker's
// side. The sweeper's janitor used to remove a READY clip's scratch dir while
// a trim was using it, and the trim then ended on "storage was unavailable
// while the trim ran" — permanently, because both of a trim's counters were
// capped at two. The janitor no longer does that; what this pins is the other
// half of the fix, the one that matters whatever removed the dir: losing
// scratch is an ENVIRONMENT failure, it is bounded by the instance's
// TRANSCODE_MAX_TRANSIENT, and the run comes back and finishes the edit.
func TestTrimSurvivesLosingItsScratchDir(t *testing.T) {
	h := newHarness(t)
	c := h.readyTrimmable()
	dir := filepath.Join(h.cfg.ScratchDir, c.ID)
	var mu sync.Mutex
	// Twice: one more than the cap this used to run under, so a green test
	// cannot be the old behaviour getting away with a single retry.
	sabotage := 2
	h.runner.posterFn = func(_ context.Context, _, out string, _ int64, _ bool) (transcode.PosterResult, error) {
		if err := os.WriteFile(out, []byte("poster-bytes"), 0o644); err != nil {
			return transcode.PosterResult{}, err
		}
		mu.Lock()
		defer mu.Unlock()
		if sabotage > 0 {
			sabotage--
			// Exactly the janitor's own action, at the worst moment: the
			// whole dir, after the cut, before the upload reads it back.
			if err := os.RemoveAll(dir); err != nil {
				return transcode.PosterResult{}, err
			}
		}
		return transcode.PosterResult{Width: 640, Height: 360}, nil
	}

	h.enqueueTrim(c.ID, 1000, 3000)
	for i := 1; i <= 2; i++ {
		h.waitTrim(c.ID, func(j db.Job) bool { return j.State == db.JobQueued && j.TransientFailures == i })
		// Past the 1m·2ⁿ transient backoff; the claim compares run_after
		// against this clock, so nothing here sleeps.
		h.clock.Advance(time.Hour)
		h.w.Wake()
	}
	done := h.waitTrim(c.ID, jobDone)
	h.waitIdle()

	if done.TransientFailures != 2 || done.Attempts != 0 {
		t.Errorf("job ended attempts=%d transient=%d, want 0/2: losing scratch is the environment, not a crash",
			done.Attempts, done.TransientFailures)
	}
	after := h.clip(c.ID)
	if after.State != db.ClipReady || after.FailureReason != nil {
		t.Fatalf("clip is %q/%v; a trim never marks its clip", after.State, after.FailureReason)
	}
	if after.DurationMS == nil || *after.DurationMS != 2000 {
		t.Errorf("duration_ms = %v, want the trimmed 2000 — the edit was never applied", after.DurationMS)
	}
	// The owner is never LEFT with the storage sentence: there is no terminal
	// failure at all, and nothing here needed a human.
	if strings.Contains(h.logs.String(), "trim failed") {
		t.Errorf("the trim was failed for good:\n%s", h.logs.String())
	}
	h.assertNoErrorLogs()
}

// A trim refused for want of scratch space takes the §20 erratum 90 ending
// like every other kind: requeued with a backoff, BOTH counters untouched —
// so a full disk can never exhaust the budget and permanently fail an edit —
// and the sentence the owner reads through GET …/trim is this kind's own
// plain one, never a byte count from the server's disk.
func TestTrimScratchShortageSpendsNoBudget(t *testing.T) {
	h := newHarness(t)
	c := h.readyTrimmable()
	// The source is on scratch and the acquire floor is met; what is missing
	// is room for the cut this plan schedules.
	h.free = 2 << 20

	h.enqueueTrim(c.ID, 1000, 3000)
	job := h.waitTrim(c.ID, func(j db.Job) bool { return j.State == db.JobQueued && j.LastError != nil })
	h.waitIdle()

	if job.Attempts != 0 || job.TransientFailures != 0 {
		t.Fatalf("a scratch shortage spent a retry budget: attempts=%d transient=%d", job.Attempts, job.TransientFailures)
	}
	if got := str(job.LastError); got != msgDisk {
		t.Fatalf("owner-facing reason = %q, want %q", got, msgDisk)
	}
	if strings.ContainsAny(str(job.LastError), "0123456789") {
		t.Fatalf("the owner-facing reason names a byte count: %q", str(job.LastError))
	}
	if after := h.clip(c.ID); after.State != db.ClipReady || after.FailureReason != nil {
		t.Fatalf("clip is %q/%v; a trim never marks its clip", after.State, after.FailureReason)
	}
	h.assertNoErrorLogs()
}
