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

// The §20 erratum 220 job, from the loop that claims it to the UPDATE that
// commits it. Every test here asserts the same background property from a
// different angle: whatever happens to a filmstrip, the CLIP is ready, playable
// and unchanged afterwards.

func (h *harness) enqueueFilmstrip(clipID string) db.Job {
	h.t.Helper()
	j, err := h.db.EnqueueFilmstrip(context.Background(), clipID, h.clock.Now().Unix())
	if err != nil {
		h.t.Fatalf("EnqueueFilmstrip: %v", err)
	}
	h.w.Wake()
	return j
}

// waitFilmstrip waits, on the settle hook, for the clip's latest filmstrip job to match pred with its run settled (awaitJob).
func (h *harness) waitFilmstrip(clipID string, pred func(db.Job) bool) db.Job {
	h.t.Helper()
	return h.awaitJob(db.KindFilmstrip+" job for "+clipID, h.latestOfKind(clipID, db.KindFilmstrip), pred)
}

// TestFilmstripIsSampledFromTheRenditionAndCommittedOnce is the happy path: the
// media loop claims the `filmstrip` kind, ffmpeg is pointed at the RENDITION (not
// the original), the sheet lands in the bucket under the clip's own prefix, and
// ONE update fills the seven columns while the clip stays ready.
func TestFilmstripIsSampledFromTheRenditionAndCommittedOnce(t *testing.T) {
	h := newHarness(t)
	before := h.readyTrimmable()

	h.enqueueFilmstrip(before.ID)
	h.waitFilmstrip(before.ID, jobDone)
	h.waitIdle()

	after, err := h.db.GetClip(context.Background(), before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != db.ClipReady || after.FailureReason != nil {
		t.Fatalf("clip is %q/%v after a filmstrip; a filmstrip never marks its clip", after.State, after.FailureReason)
	}
	if after.FilmstripKey == nil || after.FilmstripSize == nil || after.FilmstripETag == nil ||
		after.FilmstripTiles == nil || after.FilmstripTileW == nil || after.FilmstripTileH == nil ||
		after.FilmstripIntervalMS == nil {
		t.Fatalf("filmstrip columns not filled: %+v", after)
	}
	if !strings.Contains(*after.FilmstripKey, "/filmstrip/strip-") || !strings.HasSuffix(*after.FilmstripKey, ".jpg") {
		t.Errorf("filmstrip key %q is not the §6 shape", *after.FilmstripKey)
	}
	if got, ok := h.store.Get(*after.FilmstripKey); !ok || string(got) != filmstripPayload {
		t.Errorf("the filmstrip object is missing or wrong (%q, ok=%v)", got, ok)
	}
	if *after.FilmstripTiles != transcode.FilmstripTiles || *after.FilmstripTileW != transcode.FilmstripTileWidth {
		t.Errorf("tiles = %d of %d px, want %d of %d", *after.FilmstripTiles, *after.FilmstripTileW,
			transcode.FilmstripTiles, transcode.FilmstripTileWidth)
	}
	// The rendition, the poster and the ORIGINAL are all untouched.
	if *after.RenditionKey != *before.RenditionKey || *after.PosterKey != *before.PosterKey {
		t.Error("a filmstrip job changed the rendition or the poster")
	}
	if _, ok := h.store.Get(before.SourceKey); !ok {
		t.Error("the original is gone; the upload is sacred")
	}
	// ffmpeg was handed the downloaded rendition, never the original.
	var saw bool
	for _, call := range h.runner.Calls() {
		if strings.HasPrefix(call, "Filmstrip ") {
			saw = true
			if !strings.HasPrefix(call, "Filmstrip stripsrc-") {
				t.Errorf("the filmstrip pass read %q, want the downloaded rendition", call)
			}
		}
	}
	if !saw {
		t.Fatal("the filmstrip pass never ran")
	}
	// The plan is the bounded one for a 6000 ms, 1280 px rendition.
	plans := h.runner.FilmstripPlans()
	if len(plans) != 1 || plans[0].Tiles != transcode.FilmstripTiles || plans[0].IntervalMS != 250 {
		t.Errorf("filmstrip plans = %+v, want one bounded plan of 24 tiles 250 ms apart", plans)
	}
	h.assertNoErrorLogs()
	h.assertLog("INFO", "filmstrip applied")
	// Nothing was written to the clip's §8.4 progress registry: its subject is
	// the clip, which was ready throughout.
	if p, ok := h.w.Progress(before.ID); ok && p.Stage == stageFilmstrip {
		t.Errorf("the filmstrip job wrote the clip's progress registry: %+v", p)
	}
	// And the attempt left no scratch behind.
	if entries, err := os.ReadDir(h.cfg.ScratchDir); err == nil {
		for _, e := range entries {
			if e.Name() == before.ID {
				inner, _ := os.ReadDir(h.cfg.ScratchDir + "/" + e.Name())
				if len(inner) > 0 {
					t.Errorf("scratch left behind: %v", inner)
				}
			}
		}
	}
}

// TestFilmstripSkippedWhenTheRenditionChanged is the stale-sheet guard: a trim
// that commits while the sheet is being sampled replaces the very frames its
// tiles show, so the attempt publishes nothing, deletes its own object and
// resolves as done/skipped — a stale filmstrip is worse than none, because its
// tiles would sit under handles naming moments it does not show.
//
// It drives the run by hand: both jobs must be claimed in a known order and the
// swap must land BETWEEN the run's first read of the clip and its commit, which
// no scheduling of the loop can be relied on to produce.
func TestFilmstripSkippedWhenTheRenditionChanged(t *testing.T) {
	h := newHarness(t)
	c := h.gifReady()
	h.enqueueFilmstrip(c.ID)
	if _, err := h.db.EnqueueTrim(context.Background(), c.ID,
		db.MarshalTrimParams(db.TrimParams{StartMS: 0, EndMS: 2000}), h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	fsJob := h.claimKind(db.KindFilmstrip)
	trimJob := h.claimKind(db.KindTrim)

	var strip string
	h.runner.filmstripFn = func(_ context.Context, p transcode.FilmstripPlan, _, out string) (transcode.FilmstripResult, error) {
		_, ok, _, err := h.db.SwapRendition(context.Background(), trimJob.ID, c.ID, db.ReadyFields{
			RenditionKey: "renditions/main-newnewnew.mp4", RenditionSize: 10, RenditionETag: "a-different-etag",
			PosterKey: "poster/poster-newnewnew.jpg", PosterSize: 5, PosterETag: "p", PosterWidth: 2, PosterHeight: 2,
			Width: 1280, Height: 720, DurationMS: 2000, FPSNum: 30, FPSDen: 1, HasAudio: true,
		}, db.TrimParams{StartMS: 1000, EndMS: 3000}, h.clock.Now().Unix())
		if err != nil || !ok {
			t.Errorf("the trim did not commit: ok=%v err=%v", ok, err)
		}
		if err := os.WriteFile(out, []byte(filmstripPayload), 0o644); err != nil {
			return transcode.FilmstripResult{}, err
		}
		return transcode.FilmstripResult{Tiles: p.Tiles, TileW: p.TileWidth, TileH: 90,
			IntervalMS: p.IntervalMS, Bytes: int64(len(filmstripPayload))}, nil
	}

	r := newFilmstripRun(h.w, context.Background(), &run{}, fsJob, c)
	strip = r.key
	r.execute()

	job, err := h.db.LatestJobOfKind(context.Background(), c.ID, db.KindFilmstrip)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != db.JobDone || job.Stage != db.JobStageSkipped {
		t.Fatalf("job = %s/%s, want done/skipped", job.State, job.Stage)
	}
	if job.LastError == nil || !strings.Contains(*job.LastError, "the rendition changed") {
		t.Errorf("last_error = %v, want the rendition reason", job.LastError)
	}
	after := h.clip(c.ID)
	if after.FilmstripKey != nil {
		t.Errorf("a stale sheet was published: %v", *after.FilmstripKey)
	}
	if _, ok := h.store.Get(strip); ok {
		t.Errorf("the skipped attempt left its object %q in the bucket", strip)
	}
	if after.State != db.ClipReady {
		t.Errorf("clip is %q; a skipped filmstrip never moves it", after.State)
	}
	h.assertNoErrorLogs()
	h.assertLog("INFO", "filmstrip skipped")
}

// TestFilmstripSkippedWhenTheClipIsGone: a clip deleted between the claim and
// the run is not a failure of anything — nothing is wrong with it, and there is
// nothing to retry.
func TestFilmstripSkippedWhenTheClipIsGone(t *testing.T) {
	h := newHarness(t)
	c := h.gifReady()
	h.enqueueFilmstrip(c.ID)
	job := h.claimKind(db.KindFilmstrip)
	if err := h.db.MarkDeleting(context.Background(), c.ID, h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	newFilmstripRun(h.w, context.Background(), &run{}, job, c).execute()

	got, err := h.db.LatestJobOfKind(context.Background(), c.ID, db.KindFilmstrip)
	if err != nil {
		t.Fatal(err)
	}
	// MarkDeleting cancels every queued and running job of the clip, so the row
	// the run tries to resolve is already `cancelled` — and the run must NOT
	// treat that as an error to report. What matters is that it did no work and
	// said so.
	if got.State != db.JobCancelled {
		t.Fatalf("job = %s/%s, want cancelled (MarkDeleting cancels the queue)", got.State, got.Stage)
	}
	if len(h.runner.FilmstripPlans()) != 0 {
		t.Error("ffmpeg ran for a clip that is being deleted")
	}
	h.assertLog("INFO", "filmstrip skipped")
	h.assertNoErrorLogs()
}

// TestFilmstripPermanentFailureCostsTheClipNothing: a file that yields no tiles
// is the FILE's business, so the job fails permanently at INFO and the clip keeps
// its rendition, its card image and its readiness — the scrubber simply keeps its
// plain track. The attempt's own object is removed either way.
func TestFilmstripPermanentFailureCostsTheClipNothing(t *testing.T) {
	h := newHarness(t)
	c := h.readyTrimmable()
	h.runner.filmstripFn = func(_ context.Context, _ transcode.FilmstripPlan, _, _ string) (transcode.FilmstripResult, error) {
		return transcode.FilmstripResult{}, &transcode.ErrPermanent{Err: transcode.ErrNoTiles}
	}
	h.enqueueFilmstrip(c.ID)
	job := h.waitFilmstrip(c.ID, func(j db.Job) bool { return j.State == db.JobFailed })
	h.waitIdle()

	// Never retried: exactly one pass ran, whatever the counters say about how a
	// permanent ending is recorded.
	if n := len(h.runner.FilmstripPlans()); n != 1 {
		t.Errorf("the pass ran %d times; a permanent ending is never retried", n)
	}
	after := h.clip(c.ID)
	if after.State != db.ClipReady || after.FailureReason != nil || after.FilmstripKey != nil {
		t.Fatalf("a failed filmstrip touched the clip: %+v", after)
	}
	if after.RenditionKey == nil || after.PosterKey == nil {
		t.Error("a failed filmstrip cost the clip its rendition or its card image")
	}
	// The failure text the PUBLIC 503 prints comes from the run and carries no
	// path and no bucket coordinate.
	if job.LastError == nil {
		t.Fatal("no failure text for the route to print")
	}
	if strings.Contains(*job.LastError, h.cfg.ScratchDir) ||
		(h.cfg.S3.Bucket != "" && strings.Contains(*job.LastError, h.cfg.S3.Bucket)) {
		t.Errorf("the public failure text leaks a path or a bucket: %q", *job.LastError)
	}
	h.assertLog("INFO", "filmstrip failed")
	h.assertNoErrorLogs()
}

// TestFilmstripTransientFailureRequeues: a storage blip is not an argument about
// the file, so the attempt is requeued under the instance's transient budget and
// the sheet still arrives.
func TestFilmstripTransientFailureRequeues(t *testing.T) {
	h := newHarness(t)
	c := h.readyTrimmable()
	var calls int
	h.runner.filmstripFn = func(_ context.Context, p transcode.FilmstripPlan, _, out string) (transcode.FilmstripResult, error) {
		calls++
		if calls == 1 {
			return transcode.FilmstripResult{}, errors.New("storage went away")
		}
		if err := os.WriteFile(out, []byte(filmstripPayload), 0o644); err != nil {
			return transcode.FilmstripResult{}, err
		}
		return transcode.FilmstripResult{Tiles: p.Tiles, TileW: p.TileWidth, TileH: 90,
			IntervalMS: p.IntervalMS, Bytes: int64(len(filmstripPayload))}, nil
	}
	h.enqueueFilmstrip(c.ID)
	// The backoff is on the job's run_after, so the clock has to move for the
	// loop to claim it again.
	h.waitFilmstrip(c.ID, func(j db.Job) bool { return j.TransientFailures > 0 })
	h.clock.Advance(10 * time.Minute)
	h.w.Wake()
	h.waitFilmstrip(c.ID, jobDone)
	h.waitIdle()

	after := h.clip(c.ID)
	if after.FilmstripKey == nil {
		t.Fatalf("the retry did not land a sheet: %+v", after)
	}
	if after.State != db.ClipReady {
		t.Errorf("clip is %q after a requeued filmstrip", after.State)
	}
}

// TestFilmstripKeyIsPerAttempt: every attempt writes a fresh {r8}, so a retry
// can never be confused with the attempt before it and a superseded sheet is
// always a distinct object to delete.
func TestFilmstripKeyIsPerAttempt(t *testing.T) {
	h := newHarness(t)
	c := h.gifReady()
	a := newFilmstripRun(h.w, context.Background(), &run{}, db.Job{ID: 1, ClipID: c.ID}, c)
	b := newFilmstripRun(h.w, context.Background(), &run{}, db.Job{ID: 1, ClipID: c.ID}, c)
	if a.key == b.key {
		t.Errorf("two attempts share the key %q", a.key)
	}
	if want := store.FilmstripKey(h.cfg.S3.KeyPrefix, c.ID, a.r8); a.key != want {
		t.Errorf("key = %q, want %q", a.key, want)
	}
}
