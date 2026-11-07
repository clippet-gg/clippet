package worker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/transcode"
)

// The §17 #3 job, from the commit that queues it to the UPDATE that commits
// it. Every test here asserts the same background property from a different
// angle: whatever happens to the card file, the CLIP is ready, playable and
// unchanged, and `/social.mp4` has an answer either way.

// socialHarness is newHarness set up for the one thing that still makes the
// TRANSCODE's own commit queue a `social` job: an encode whose real output beat
// the PLAN's estimate of it (§20 erratum 235).
//
// It used to be SOCIAL_MAX_BYTES = 1, so that the 31 bytes the fake writes were
// "big" and no test had to invent megabytes. That stopped queueing a job at all
// when erratum 235 gave the plan a SIZE for the Full: a cap of one byte is
// crossed by any estimate, so the transcode now plans the card copy itself and
// the commit has nothing left to ask for. The shape that still reaches this job
// from a transcode is the opposite one — the estimate UNDER the cap and the file
// OVER it — which is a real source (0.2 bits per pixel is the measured ceiling,
// and a high-motion CRF 18 encode can pass it), and it is what these numbers are:
//
//	the plan's estimate  1280·720·30·6 s·0.2/8 = 3.95 MiB   (under the cap)
//	SOCIAL_MAX_BYTES     5 MiB
//	the encode's output  6 MiB                              (at or over the cap)
//
// MAX_UPLOAD_BYTES has to be raised with them, because it is one of the three
// bounds outputBudget grants an encode (§20 erratum 46) and the plan's estimate
// is the only one a 1 KiB source would otherwise reach — the run would be killed
// for writing 6 MiB instead of committing it.
func socialHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarnessCfg(t, func(c *config.Config) {
		c.SocialMaxBytes = socialTestCap
		c.MaxUploadBytes = 64 << 20
	})
	h.runner.encodeBytes = bytes.Repeat([]byte("R"), socialTestRendition)
	return h
}

const (
	// socialTestCap is above the harness probe's planned-output estimate
	// (3.95 MiB) — so the PLAN never asks for a card copy — and at or below
	// socialTestRendition, so the COMMIT does. It is also the cap the tests that
	// need the rule to say NO use, against the fake's default 31-byte rendition.
	socialTestCap = 5 << 20
	// socialTestRendition is what the fake encoder writes under that cap.
	socialTestRendition = 6 << 20
)

// socialByHandHarness is socialHarness for a test that drives a socialRun ITSELF
// against a hand-made ready clip (h.gifReady), where no transcode and therefore
// no tier PLAN is involved at all: SOCIAL_MAX_BYTES = 1 makes the 31 bytes such a
// clip's rendition weighs qualify, which is the whole of what those tests need
// from the rule.
func socialByHandHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessCfg(t, func(c *config.Config) { c.SocialMaxBytes = 1 })
}

// waitSocial waits, on the settle hook, for the clip's latest social job to match pred with its run settled (awaitJob).
func (h *harness) waitSocial(clipID string, pred func(db.Job) bool) db.Job {
	h.t.Helper()
	return h.awaitJob(db.KindSocial+" job for "+clipID, h.latestOfKind(clipID, db.KindSocial), pred)
}

// TestSocialSkipRule is the whole of erratum 121's decision, as a table. The
// two halves are independent on purpose: a six-second 60 fps clip is far
// under any byte threshold, and 60 fps is exactly what makes a longer one of
// the same footage too big to inline.
func TestSocialSkipRule(t *testing.T) {
	const cap25 = 25 << 20
	for _, tc := range []struct {
		name     string
		max      int64
		size     int64
		num, den int
		want     bool
	}{
		{"small and 30 fps: the rendition is already the card file", cap25, 4 << 20, 30, 1, false},
		{"small and 29.97: not above 30", cap25, 4 << 20, 30000, 1001, false},
		{"small but 60 fps", cap25, 4 << 20, 60, 1, true},
		{"small but 50 fps", cap25, 4 << 20, 50, 1, true},
		{"exactly at the cap", cap25, cap25, 30, 1, true},
		{"one byte under the cap", cap25, cap25 - 1, 30, 1, false},
		{"big and calm", cap25, 90 << 20, 30, 1, true},
		{"SOCIAL_MAX_BYTES=0 turns it off, 60 fps included", 0, 900 << 20, 60, 1, false},
		{"a zero frame rate cannot be above 30", cap25, 4 << 20, 30, 0, false},
	} {
		// The dimensions are the card-sized 1280x720 throughout, so this table
		// stays a test of the SIZE and FRAME-RATE halves alone; the third term
		// erratum 203 added (a picture larger than 1280x720) is TestLightWanted's
		// in internal/transcode.
		f := db.ReadyFields{RenditionKey: "r", RenditionSize: tc.size, Width: 1280, Height: 720,
			FPSNum: tc.num, FPSDen: tc.den}
		got := lightWanted(f, tc.size, config.Config{SocialMaxBytes: tc.max})
		if got != tc.want {
			t.Errorf("%s: lightWanted = %v, want %v", tc.name, got, tc.want)
		}
	}
	// A commit that has ALREADY produced the Light copy says no: the work is
	// done, and a job to redo it would spend an ffmpeg slot on a duplicate
	// (§20 erratum 206).
	made := db.ReadyFields{RenditionKey: "r", RenditionSize: 90 << 20, Width: 1280, Height: 720,
		FPSNum: 60, FPSDen: 1, Light: db.SocialFields{Key: "clips/x/renditions/social-abc.mp4"}}
	if lightWanted(made, 90<<20, config.Config{SocialMaxBytes: cap25}) {
		t.Error("lightWanted although this commit already made the light copy")
	}
	// An unknown size decides on SHAPE alone: a 720p30 file whose bytes nobody
	// has counted is not assumed to be over the threshold.
	calm := db.ReadyFields{Width: 1280, Height: 720, FPSNum: 30, FPSDen: 1}
	if lightWanted(calm, 0, config.Config{SocialMaxBytes: 1}) {
		t.Error("lightWanted with no measured size and a card-shaped file")
	}
}

// TestSocialRenditionIsEncodedFromTheRenditionAndCommittedOnce is the happy
// path end to end: the ready flip queues the job, the media loop claims it,
// ffmpeg is pointed at the RENDITION (never the original), the object lands
// in the bucket under the clip's own prefix and one UPDATE fills the five
// columns while the clip stays ready throughout.
func TestSocialRenditionIsEncodedFromTheRenditionAndCommittedOnce(t *testing.T) {
	h := socialHarness(t)
	c, _ := h.newClip("mp4", 1024, true)
	h.start()
	h.waitClip(c.ID, db.ClipReady)
	h.waitSocial(c.ID, jobDone)
	h.waitIdle()

	after := h.clip(c.ID)
	switch {
	case after.State != db.ClipReady || after.FailureReason != nil:
		t.Fatalf("clip is %q/%v after a social encode; it never touches the clip", after.State, after.FailureReason)
	case after.SocialKey == nil || after.SocialSize == nil || after.SocialETag == nil ||
		after.SocialWidth == nil || after.SocialHeight == nil:
		t.Fatalf("social columns not filled: %+v", after)
	}
	if !strings.Contains(*after.SocialKey, "/renditions/social-") || !strings.HasSuffix(*after.SocialKey, ".mp4") {
		t.Errorf("social key %q is not the §6 shape", *after.SocialKey)
	}
	if !strings.HasPrefix(*after.SocialKey, "clips/"+c.ID+"/") {
		t.Errorf("social key %q is outside the clip prefix a delete removes", *after.SocialKey)
	}
	if got, ok := h.store.Get(*after.SocialKey); !ok || string(got) != socialPayload {
		t.Errorf("the social object is missing or wrong (%q, ok=%v)", got, ok)
	}
	// It is a DIFFERENT file from the rendition — which is the whole feature,
	// and what the route's ETag then tells a crawler.
	if *after.SocialETag == *after.RenditionETag {
		t.Errorf("the social file has the rendition's ETag %q; nothing changed", *after.SocialETag)
	}
	// The dims are MEASURED from the produced file (the fake probe says
	// 1280×720), never copied off the clip row — db.SetSocial writes what the
	// job measured, which TestSetSocial pins independently.
	if *after.SocialWidth != 1280 || *after.SocialHeight != 720 {
		t.Errorf("social dims = %dx%d, want the measured 1280x720", *after.SocialWidth, *after.SocialHeight)
	}
	// The encode read the rendition, and the frame rate it planned is capped.
	var sawEncode bool
	for _, call := range h.runner.Calls() {
		if strings.HasPrefix(call, "EncodeSocial ") {
			sawEncode = true
			if !strings.HasPrefix(strings.TrimPrefix(call, "EncodeSocial "), "socialsrc-") {
				t.Errorf("the social encode read %q, want the downloaded rendition", call)
			}
		}
	}
	if !sawEncode {
		t.Fatalf("EncodeSocial was never called: %v", h.runner.Calls())
	}
	plans := h.runner.SocialPlans()
	if len(plans) != 1 {
		t.Fatalf("%d social plans, want 1", len(plans))
	}
	if plans[0].FPS.Float() > transcode.SocialMaxFPS {
		t.Errorf("social plan fps = %s, want ≤ %d", plans[0].FPS, transcode.SocialMaxFPS)
	}
	h.assertOriginalIntact(after)
	h.assertNoErrorLogs()
	// The scratch copy is gone; a card file is not worth a permanent disk.
	if entries, err := os.ReadDir(h.cfg.ScratchDir); err == nil {
		for _, e := range entries {
			if e.Name() == c.ID {
				t.Errorf("the social run left %s behind in scratch", e.Name())
			}
		}
	}
}

// TestSocialIsSkippedForASmallCalmRendition: the other half of the rule, at
// the level that matters — no job is ever queued, so no ffmpeg slot is spent
// and /social.mp4 keeps serving the rendition through its COALESCE.
func TestSocialIsSkippedForASmallCalmRendition(t *testing.T) {
	h := newHarnessCfg(t, func(c *config.Config) { c.SocialMaxBytes = 25 << 20 })
	c, _ := h.newClip("mp4", 1024, true)
	h.start()
	h.waitClip(c.ID, db.ClipReady)
	h.waitIdle()

	if j, err := h.db.LatestJobOfKind(context.Background(), c.ID, db.KindSocial); err == nil {
		t.Fatalf("a 31-byte 30 fps rendition queued a social job: %+v", j)
	}
	after := h.clip(c.ID)
	if after.SocialKey != nil {
		t.Fatalf("social_key = %q, want NULL so the route falls back", *after.SocialKey)
	}
	for _, call := range h.runner.Calls() {
		if strings.HasPrefix(call, "EncodeSocial") {
			t.Fatalf("the skip rule still ran ffmpeg: %v", h.runner.Calls())
		}
	}
}

// TestSocialFailurePreservesTheClip: a permanent ffmpeg failure fails the
// JOB, names the reason, and leaves the clip ready on the rendition it had —
// which is what /social.mp4 then serves, exactly as before the feature.
func TestSocialFailurePreservesTheClip(t *testing.T) {
	h := socialHarness(t)
	h.runner.socialFn = func(context.Context, transcode.Plan, string, string) error {
		return &transcode.ErrPermanent{Err: errors.New("encode_timeout: not finished within 1m0s")}
	}
	c, _ := h.newClip("mp4", 1024, true)
	h.start()
	h.waitClip(c.ID, db.ClipReady)
	job := h.waitSocial(c.ID, jobFailed)
	h.waitIdle()

	if job.LastError == nil || !strings.Contains(*job.LastError, "encode_timeout") {
		t.Errorf("last_error = %v", job.LastError)
	}
	if !strings.HasPrefix(*job.LastError, stageEncode+": ") {
		t.Errorf("last_error %q does not name the stage it died in", *job.LastError)
	}
	after := h.clip(c.ID)
	if after.State != db.ClipReady || after.FailureReason != nil || after.SocialKey != nil {
		t.Fatalf("a failed card file changed the clip: state=%q reason=%v social=%v",
			after.State, after.FailureReason, after.SocialKey)
	}
	if _, ok := h.store.Get(*after.RenditionKey); !ok {
		t.Error("the clip lost its rendition")
	}
	h.assertLog("INFO", "light copy failed")
	h.assertNoErrorLogs()
}

// TestSocialSkippedWhenTheRenditionChanged is the stale-card guard: a trim
// that commits while the social encode runs replaces the very frames it was
// made from, so the attempt publishes nothing, removes its own object and
// resolves as done/skipped. The trim's own commit has already queued the
// replacement.
//
// It drives the run by hand for the gif test's reason: the swap must land
// between the run's first read of the clip and its commit, which no
// scheduling of the loop can be relied on to produce.
func TestSocialSkippedWhenTheRenditionChanged(t *testing.T) {
	h := socialByHandHarness(t)
	c := h.gifReady() // a ready clip with no worker running
	sj, err := h.db.EnqueueSocial(context.Background(), c.ID, h.clock.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.EnqueueTrim(context.Background(), c.ID,
		db.MarshalTrimParams(db.TrimParams{StartMS: 0, EndMS: 2000}), h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	claimed := h.claimKind(db.KindSocial)
	trimJob := h.claimKind(db.KindTrim)
	if claimed.ID != sj.ID {
		t.Fatalf("claimed job %d, want the social job %d", claimed.ID, sj.ID)
	}

	h.runner.socialFn = func(_ context.Context, _ transcode.Plan, _, out string) error {
		_, ok, _, err := h.db.SwapRendition(context.Background(), trimJob.ID, c.ID, db.ReadyFields{
			RenditionKey: "renditions/main-newnewnew.mp4", RenditionSize: 10, RenditionETag: "a-different-etag",
			PosterKey: "poster/poster-newnewnew.jpg", PosterSize: 5, PosterETag: "p", PosterWidth: 2, PosterHeight: 2,
			Width: 1280, Height: 720, DurationMS: 2000, FPSNum: 30, FPSDen: 1, HasAudio: true,
		}, db.TrimParams{StartMS: 1000, EndMS: 3000}, h.clock.Now().Unix())
		if err != nil || !ok {
			t.Errorf("the trim did not commit: ok=%v err=%v", ok, err)
		}
		return os.WriteFile(out, []byte(socialPayload), 0o644)
	}

	newSocialRun(h.w, context.Background(), &run{}, claimed, c).execute()

	job, err := h.db.LatestJobOfKind(context.Background(), c.ID, db.KindSocial)
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
	if after.SocialKey != nil {
		t.Errorf("a stale card file was published: %q", *after.SocialKey)
	}
	if after.State != db.ClipReady {
		t.Errorf("clip is %q", after.State)
	}
	for _, k := range h.store.Keys() {
		if strings.Contains(k, "/renditions/social-") {
			t.Errorf("the unpublished card file %q is still in the bucket", k)
		}
	}
	h.assertLog("INFO", "social skipped")
	h.assertNoErrorLogs()
}

// TestTrimReplacesTheSocialRendition is erratum 70's reservation end to end:
// the swap NULLs the columns, deletes the superseded object and queues a
// fresh encode, so a trimmed clip's card describes the trimmed clip.
func TestTrimReplacesTheSocialRendition(t *testing.T) {
	h := socialHarness(t)
	c, _ := h.newClip("mp4", 1024, true)
	h.start()
	h.waitClip(c.ID, db.ClipReady)
	h.waitSocial(c.ID, jobDone)
	h.waitIdle()
	before := h.clip(c.ID)
	if before.SocialKey == nil {
		t.Fatal("no social rendition to replace")
	}
	oldKey := *before.SocialKey

	h.runner.probeOutFn = trimmedOutput(2000)
	h.enqueueTrim(c.ID, 0, 2000)
	h.waitTrim(c.ID, jobDone)
	// A second social job now exists, queued by the swap itself.
	h.waitSocial(c.ID, func(j db.Job) bool { return j.State == db.JobDone && j.ID != 0 && j.CreatedAt >= before.UpdatedAt })
	h.waitIdle()

	after := h.clip(c.ID)
	if after.SocialKey == nil || *after.SocialKey == oldKey {
		t.Fatalf("social key after the trim = %v, want a fresh one (was %q)", after.SocialKey, oldKey)
	}
	if _, ok := h.store.Get(oldKey); ok {
		t.Errorf("the superseded card file %q is still in the bucket", oldKey)
	}
	if after.State != db.ClipReady {
		t.Errorf("clip is %q after a trim + social encode", after.State)
	}
	h.assertNoErrorLogs()
}

// TestSocialSkipsAClipThatIsNoLongerReady: the claim→run window. A clip
// tombstoned between the enqueue and the run is skipped, not failed.
func TestSocialSkipsAClipThatIsNoLongerReady(t *testing.T) {
	h := socialHarness(t)
	c := h.readyTrimmable()
	sr := newSocialRun(h.w, context.Background(), &run{}, db.Job{ID: 1, ClipID: c.ID, Kind: db.KindSocial}, c)
	if err := h.db.MarkDeleting(context.Background(), c.ID, h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	err := sr.recheckClip()
	if !errors.Is(err, errSkipped) || !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("recheckClip = %v, want a skip", err)
	}
}

// TestSocialHoldsAnFFmpegSlot pins erratum 79 for the new kind: the encode
// runs inside the semaphore every other ffmpeg stage takes a token from, so a
// default install still runs ONE ffmpeg however much media work is queued.
func TestSocialHoldsAnFFmpegSlot(t *testing.T) {
	h := socialHarness(t)
	c := h.readyTrimmable()
	// Fill the single slot and hold it.
	release, err := h.w.acquireFFmpeg(context.Background(), h.w.log)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	// Closed ONCE: under -race with the box busy the harness's own worker loop
	// has reached a queued social job for this clip and called the fake a
	// second time (check.sh 2026-09-10: "close of closed channel"). The test
	// is about the slot, not about how many callers the fake sees.
	var startedOnce sync.Once
	h.runner.socialFn = func(_ context.Context, _ transcode.Plan, _, out string) error {
		startedOnce.Do(func() { close(started) })
		return os.WriteFile(out, []byte(socialPayload), 0o644)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sr := newSocialRun(h.w, context.Background(), &run{}, db.Job{ID: 999, ClipID: c.ID, Kind: db.KindSocial}, c)
		_ = sr.recheckClip()
		_ = sr.acquire()
		_ = sr.encodeVerify()
	}()
	select {
	case <-started:
		t.Fatal("the social encode ran while the only ffmpeg slot was held")
	case <-time.After(150 * time.Millisecond):
	}
	release()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the social encode never got the slot back")
	}
	<-done
	h.assertLog("INFO", "waiting for an ffmpeg slot")
}

// A social job that was queued when the clip qualified is NOT cancelled by a
// trim that de-qualifies it: SwapRendition leaves an already-live job alone,
// and the ETag predicate that throws away stale work only catches a job that
// had already STARTED and read the old ETag. One still `queued` when the trim
// commits reads the NEW rendition in recheckClip — so the skip rule has to be
// re-applied there, or a clip that qualified by SIZE alone and was then
// trimmed small gets one social encode the rule would have refused: an ffmpeg
// slot and an object spent on a file the card does not need (§20 erratum 169).
func TestSocialRechecksTheSkipRuleAgainstTheRenditionItFound(t *testing.T) {
	t.Run("the rendition no longer qualifies", func(t *testing.T) {
		// SOCIAL_MAX_BYTES far above what this rendition weighs, and the
		// clip is 30 fps: exactly the shape a trim leaves behind when the
		// clip qualified on size before the cut. The cap is also above the
		// PLAN's estimate of the Full, so the transcode that made this clip
		// planned no card copy of its own (§20 erratum 235) and every social
		// plan the fake recorded is this job's.
		h := newHarnessCfg(t, func(c *config.Config) { c.SocialMaxBytes = socialTestCap })
		c := h.readyTrimmable()
		if c.RenditionSize == nil || *c.RenditionSize >= h.cfg.SocialMaxBytes {
			t.Fatalf("premise broken -- the rendition is not under SOCIAL_MAX_BYTES: %v", c.RenditionSize)
		}
		sr := newSocialRun(h.w, context.Background(), &run{}, db.Job{ID: 1, ClipID: c.ID, Kind: db.KindSocial}, c)
		err := sr.recheckClip()
		if !errors.Is(err, errSkipped) || !strings.Contains(err.Error(), "no longer qualifies") {
			t.Fatalf("recheckClip = %v, want a skip naming the rule", err)
		}
		if len(h.runner.SocialPlans()) != 0 {
			t.Fatalf("a skipped job planned an encode: %v", h.runner.SocialPlans())
		}
	})

	t.Run("a rendition that still qualifies runs", func(t *testing.T) {
		h := socialHarness(t) // the rendition is over SOCIAL_MAX_BYTES
		c := h.readyTrimmable()
		sr := newSocialRun(h.w, context.Background(), &run{}, db.Job{ID: 1, ClipID: c.ID, Kind: db.KindSocial}, c)
		if err := sr.recheckClip(); err != nil {
			t.Fatalf("recheckClip = %v, want the run to go ahead", err)
		}
		if sr.renditionETag == "" || sr.renditionSize == 0 {
			t.Fatalf("recheckClip did not read the rendition it approved: %q/%d", sr.renditionETag, sr.renditionSize)
		}
	})

	// The off switch outranks everything, on this path as on the commit's.
	t.Run("SOCIAL_MAX_BYTES=0 skips whatever the rendition looks like", func(t *testing.T) {
		h := newHarnessCfg(t, func(c *config.Config) { c.SocialMaxBytes = 0 })
		c := h.readyTrimmable()
		sr := newSocialRun(h.w, context.Background(), &run{}, db.Job{ID: 1, ClipID: c.ID, Kind: db.KindSocial}, c)
		if err := sr.recheckClip(); !errors.Is(err, errSkipped) {
			t.Fatalf("recheckClip with the feature off = %v, want a skip", err)
		}
	})
}
