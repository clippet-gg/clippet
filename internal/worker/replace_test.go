package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/scratch"
	"clippet/internal/store"
	"clippet/internal/transcode"
)

const replacementPayload = "the-replacement-original-bytes"

// readyReplaceable drives a clip to `ready` through the real pipeline, so a
// replacement test starts where a replacement can start.
func (h *harness) readyReplaceable() db.Clip {
	h.t.Helper()
	c, _ := h.newClip("mp4", 1024, true)
	h.start()
	h.waitClip(c.ID, db.ClipReady)
	h.waitIdle()
	got, err := h.db.GetClip(context.Background(), c.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return got
}

// enqueueReplace does what the HTTP handler does before the worker sees
// anything: it stores the new original under a per-attempt key, spools it
// under the attempt's own scratch directory, and queues the job.
func (h *harness) enqueueReplace(clipID string, size int) (db.Job, db.ReplaceParams) {
	h.t.Helper()
	r8 := ids.Rand(8)
	key := store.ReplacementKey(h.cfg.S3.KeyPrefix, clipID, r8, "mp4")
	payload := []byte(replacementPayload)
	for len(payload) < size {
		payload = append(payload, 'x')
	}
	payload = payload[:size]
	h.store.Put(key, payload, "video/mp4")
	spool := scratch.ReplaceTmpName(clipID, r8)
	dir := filepath.Join(h.cfg.ScratchDir, spool)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source.mp4"), payload, 0o644); err != nil {
		h.t.Fatal(err)
	}
	p := db.ReplaceParams{SourceKey: key, SourceFilename: "better.mp4", SourceSize: int64(len(payload)),
		SourceContentType: "video/mp4", SourceETag: "replacement-etag", Spool: spool}
	j, err := h.db.EnqueueReplace(context.Background(), clipID, db.MarshalReplaceParams(p), h.clock.Now().Unix())
	if err != nil {
		h.t.Fatalf("EnqueueReplace: %v", err)
	}
	h.w.Wake()
	return j, p
}

// waitReplace waits, on the settle hook, for the clip's latest replacement job to match pred with its run settled (awaitJob).
func (h *harness) waitReplace(clipID string, pred func(db.Job) bool) db.Job {
	h.t.Helper()
	return h.awaitJob(db.KindReplace+" job for "+clipID, h.latestOfKind(clipID, db.KindReplace), pred)
}

// TestReplaceSwapsEverythingAndKeepsTheClipReady is the happy path: the media
// loop claims the kind, the clip never leaves `ready`, one UPDATE swaps the
// source AND the rendition, and the OLD original is deleted only afterwards.
func TestReplaceSwapsEverythingAndKeepsTheClipReady(t *testing.T) {
	h := newHarness(t)
	before := h.readyReplaceable()
	oldSource, oldRendition, oldPoster := before.SourceKey, *before.RenditionKey, *before.PosterKey

	_, params := h.enqueueReplace(before.ID, 2048)
	h.waitReplace(before.ID, jobDone)
	h.waitIdle()

	after, err := h.db.GetClip(context.Background(), before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != db.ClipReady || after.FailureReason != nil {
		t.Fatalf("clip is %q/%v after a replacement; it never leaves ready", after.State, after.FailureReason)
	}
	if after.SourceKey != params.SourceKey || after.SourceETag != params.SourceETag ||
		after.SourceSize != params.SourceSize || after.SourceFilename != "better.mp4" {
		t.Fatalf("the source was not swapped: %+v", after)
	}
	if *after.RenditionKey == oldRendition || *after.PosterKey == oldPoster {
		t.Fatalf("the rendition was not re-derived (rendition %q, poster %q)", *after.RenditionKey, *after.PosterKey)
	}
	if after.TrimStartMS != nil || after.TrimEndMS != nil {
		t.Errorf("the trim window survived: %v..%v", after.TrimStartMS, after.TrimEndMS)
	}
	// Everything the row stopped referencing is gone from the bucket — the
	// PREVIOUS ORIGINAL among them, and only after the commit.
	for name, key := range map[string]string{"original": oldSource, "rendition": oldRendition, "poster": oldPoster} {
		if _, ok := h.store.Get(key); ok {
			t.Errorf("the superseded %s %q is still in the bucket", name, key)
		}
	}
	// The NEW original is still there: it is the clip's now.
	if _, ok := h.store.Get(params.SourceKey); !ok {
		t.Error("the replacement original is gone; it is the upload now")
	}
	// The attempt's scratch directory is cleaned up, and the clip's own dir
	// was never touched.
	if _, err := os.Stat(filepath.Join(h.cfg.ScratchDir, params.Spool)); !os.IsNotExist(err) {
		t.Errorf("the replacement spool survived the commit: %v", err)
	}
	h.assertNoErrorLogs()
}

// TestReplaceOfAnUnplayableFileWritesBothTiers is the replacement half of §20
// erratum 217, and it exists because the bug it catches was silent: the
// replacement run planned the LIGHT tier whenever one was wanted, so `verifyFull`
// measured a 1080p60 Full against a 720p30 box and every such replacement failed
// `bad_output`. One decode, two files, the clip's own measurements the FAITHFUL
// copy's, and no `full` job behind it.
func TestReplaceOfAnUnplayableFileWritesBothTiers(t *testing.T) {
	p := defaultProbeResult()
	p.CodedW, p.CodedH, p.DisplayW, p.DisplayH = 1920, 1080, 1920, 1080
	p.FPS, p.RFPS = transcode.Rational{Num: 60, Den: 1}, transcode.Rational{Num: 60, Den: 1}
	h := tierHarness(t, p)
	h.runner.probeOutFn = func(_ context.Context, path string) (*transcode.OutputInfo, error) {
		if strings.Contains(path, "light-") {
			return &transcode.OutputInfo{Width: 1280, Height: 720, FPS: transcode.Rational{Num: 30, Den: 1},
				DurationMS: 6000, HasAudio: true, VideoCodec: "h264", AudioCodec: "aac", VideoStreams: 1, AudioStreams: 1}, nil
		}
		return &transcode.OutputInfo{Width: 1920, Height: 1080, FPS: transcode.Rational{Num: 60, Den: 1},
			DurationMS: 6000, HasAudio: true, VideoCodec: "h264", AudioCodec: "aac", VideoStreams: 1, AudioStreams: 1}, nil
	}
	before := h.readyReplaceable()
	// The first upload took this branch too, so the DELTA is what this test is about.
	tiersBefore := countCalls(h, "EncodeTiers ")
	h.enqueueReplace(before.ID, 4096)
	h.waitReplace(before.ID, jobDone)
	h.waitIdle()

	after := h.clip(before.ID)
	switch {
	case after.State != db.ClipReady:
		t.Fatalf("the clip is %q; a replacement never moves it", after.State)
	case after.RenditionKey == nil || after.SocialKey == nil:
		t.Fatalf("want both tiers after the swap; rendition=%v light=%v", after.RenditionKey, after.SocialKey)
	case after.FullState != db.TierReady || after.LightState != db.TierReady:
		t.Fatalf("tier states full=%q light=%q, want ready/ready", after.FullState, after.LightState)
	case after.Width == nil || *after.Width != 1920:
		t.Fatalf("width = %v after the swap, want the Full's 1920", after.Width)
	}
	if _, err := h.db.LatestJobOfKind(context.Background(), before.ID, db.KindFull); err == nil {
		t.Fatal("a full job was queued although the replacement produced the Full itself")
	}
	// ONE ffmpeg for the replacement, and no single-output card encode anywhere.
	if n := countCalls(h, "EncodeTiers ") - tiersBefore; n != 1 {
		t.Fatalf("%d EncodeTiers calls in the replacement, want one: %v", n, h.runner.Calls())
	}
	if n := countCalls(h, "EncodeSocial "); n != 0 {
		t.Fatalf("%d EncodeSocial calls, want none — one decode writes both tiers: %v", n, h.runner.Calls())
	}
	h.assertNoErrorLogs()
}

// countCalls is how many of the fake runner's recorded calls start with prefix.
func countCalls(h *harness, prefix string) int {
	n := 0
	for _, call := range h.runner.Calls() {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

// TestReplaceFailurePreservesTheClip: a permanent failure costs the ATTEMPT,
// never the clip — which keeps the original, the rendition and the poster it
// had — and the uploaded replacement is removed, because nothing references
// it and `admin orphans` cannot see an extra object in a live clip's prefix.
func TestReplaceFailurePreservesTheClip(t *testing.T) {
	h := newHarness(t)
	before := h.readyReplaceable()
	h.runner.probeFn = func(context.Context, string) (*transcode.ProbeResult, error) {
		return nil, &transcode.ErrPermanent{Err: errors.New("moov atom not found")}
	}

	_, params := h.enqueueReplace(before.ID, 2048)
	job := h.waitReplace(before.ID, jobFailed)
	h.waitIdle()

	after, err := h.db.GetClip(context.Background(), before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != db.ClipReady || after.FailureReason != nil {
		t.Fatalf("a failed replacement marked the clip: %q/%v", after.State, after.FailureReason)
	}
	if after.SourceKey != before.SourceKey || *after.RenditionKey != *before.RenditionKey {
		t.Fatalf("a failed replacement moved the clip's files")
	}
	if _, ok := h.store.Get(before.SourceKey); !ok {
		t.Error("the original was deleted by a replacement that never landed")
	}
	if _, ok := h.store.Get(params.SourceKey); ok {
		t.Error("the failed replacement's upload is still in the bucket")
	}
	// The owner is told one of the fixed sentences, not an ffmpeg tail.
	if job.LastError == nil || *job.LastError != msgReplaceMedia {
		t.Fatalf("last_error = %v, want %q", job.LastError, msgReplaceMedia)
	}
	// A permanent failure of a file the owner chose is INFO: nothing here is
	// broken (§12).
	h.assertNoErrorLogs()
}

// TestReplaceLosesTheRaceQuietly: the clip was destroyed while the encode ran,
// so nothing is committed and every object this attempt wrote is removed.
func TestReplaceLosesTheRaceQuietly(t *testing.T) {
	h := newHarness(t)
	before := h.readyReplaceable()
	started := make(chan struct{})
	release := make(chan struct{})
	h.runner.encodeFn = func(ctx context.Context, p transcode.Plan, in, out string, onProgress func(float64, bool)) error {
		close(started)
		<-release
		return os.WriteFile(out, []byte(renditionPayload), 0o644)
	}

	_, params := h.enqueueReplace(before.ID, 2048)
	<-started
	// Tombstone the clip under the running job, exactly as an owner DELETE
	// does. MarkDeleting cancels every live job of the clip.
	if err := h.db.MarkDeleting(context.Background(), before.ID, h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	close(release)
	h.waitReplace(before.ID, func(j db.Job) bool { return j.State == db.JobCancelled })
	h.waitIdle()

	if _, ok := h.store.Get(params.SourceKey); ok {
		t.Error("the replacement original survived a lost race")
	}
	for _, k := range h.store.Keys() {
		if strings.Contains(k, "/renditions/main-") && k != *before.RenditionKey {
			t.Errorf("a lost race left its own rendition behind: %q", k)
		}
	}
	h.assertNoErrorLogs()
}

// TestReplaceRefusesASpoolItDidNotMint: the job row carries a directory NAME,
// and a row is not a place to take a filesystem path on trust.
func TestReplaceRefusesASpoolItDidNotMint(t *testing.T) {
	h := newHarness(t)
	before := h.readyReplaceable()
	bad := db.MarshalReplaceParams(db.ReplaceParams{SourceKey: "k", SourceETag: "e", SourceSize: 1,
		Spool: "../../etc"})
	if _, err := h.db.EnqueueReplace(context.Background(), before.ID, bad, h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	h.w.Wake()
	job := h.waitReplace(before.ID, jobFailed)
	h.waitIdle()
	if job.LastError == nil || *job.LastError != msgReplaceInternal {
		t.Fatalf("last_error = %v, want %q", job.LastError, msgReplaceInternal)
	}
	if after, _ := h.db.GetClip(context.Background(), before.ID); after.SourceKey != before.SourceKey {
		t.Fatal("a job with an unusable spool changed the clip")
	}
}
