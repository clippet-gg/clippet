package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func gifFields() GIFFields {
	return GIFFields{Key: "clips/x/renditions/preview-r8r8r8r8.gif", Size: 4096, ETag: "gif-etag", Width: 480, Height: 270}
}

// TestMigrate0002UpgradesA0001Database is the upgrade path, which is what a
// running install actually does: a database that has only ever seen 0001 —
// with rows in it — gains the five gif_* columns and every existing clip
// reads back with them NULL. The live stack's volume is exactly this
// database, so this test is the rehearsal for E2E step 22.
func TestMigrate0002UpgradesA0001Database(t *testing.T) {
	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "clippet.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	// Bring the file up to 0001 and NO further, the way an installation that
	// has been running since Phase 1 is.
	if _, err := d.rw.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	files, err := migrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 2 || files[0].version != 1 {
		t.Fatalf("migration set = %+v, want 0001 first and at least one more", files)
	}
	if err := d.applyMigration(ctx, files[0]); err != nil {
		t.Fatalf("apply 0001: %v", err)
	}
	// A ready clip that predates the columns, written the way 0001 wrote one
	// (the Go API cannot: every read of it scans the new clipCols, which is
	// itself the proof below that the migration is what adds them).
	const clipID = "aZ3kQ9xB2mLp"
	if _, err := d.rw.ExecContext(ctx,
		"INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1', 'upgrader', 'h', 1000)"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.rw.ExecContext(ctx, `
INSERT INTO clips (id, owner_id, title, description, unlisted, state,
  source_key, source_filename, source_size, source_content_type, source_etag,
  rendition_key, rendition_size, rendition_etag,
  poster_key, poster_size, poster_etag, poster_width, poster_height,
  duration_ms, width, height, fps_num, fps_den, has_audio,
  created_at, updated_at, ready_at)
VALUES (?, 'u1', 'old clip', '', 0, 'ready', 'clips/x/original/source.mp4', 'clip.mp4', 123, 'video/mp4', 'se',
  ?, 10, ?, ?, 5, 'pe', 1280, 720, 6000, 1280, 720, 30, 1, 1, 1000, 1000, 1001)`,
		clipID, ready().RenditionKey, ready().RenditionETag, ready().PosterKey); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetClip(ctx, clipID); err == nil {
		t.Fatal("GetClip succeeded on a 0001-only database; the gif columns are not in clipCols")
	}

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate to 0002: %v", err)
	}
	got, err := d.GetClip(ctx, clipID)
	if err != nil {
		t.Fatalf("GetClip after the upgrade: %v", err)
	}
	c := got
	if got.GIFKey != nil || got.GIFSize != nil || got.GIFETag != nil || got.GIFWidth != nil || got.GIFHeight != nil {
		t.Errorf("an upgraded row has a gif: %+v", got)
	}
	if got.State != ClipReady || *got.RenditionKey != ready().RenditionKey || got.Views != 0 {
		t.Errorf("the upgrade disturbed the row: %+v", got)
	}
	// And the feature works on that upgraded row.
	job, err := d.EnqueueGIF(ctx, c.ID, 1002)
	if err != nil {
		t.Fatalf("EnqueueGIF on an upgraded row: %v", err)
	}
	if _, _, _, err := d.ClaimMediaJob(ctx, 1003); err != nil {
		t.Fatal(err)
	}
	swap, err := d.SetGIF(ctx, job.ID, c.ID, gifFields(), ready().RenditionETag, 1004)
	if err != nil || !swap.OK {
		t.Fatalf("SetGIF on an upgraded row: %+v %v", swap, err)
	}
	// Migrate is still idempotent afterwards.
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate again: %v", err)
	}
}

func TestEnqueueGIFAdmitsOneAtATime(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, _ := readyClip(t, d)

	job, err := d.EnqueueGIF(ctx, c.ID, 2000)
	if err != nil {
		t.Fatalf("EnqueueGIF: %v", err)
	}
	if job.Kind != KindGIF || job.State != JobQueued || job.Stage != JobStageQueued || job.ParamsJSON != "{}" {
		t.Fatalf("queued job = %+v", job)
	}
	// A queued gif blocks a second one — this is what makes the PUBLIC route
	// safe: a hundred callers produce one job.
	if _, err := d.EnqueueGIF(ctx, c.ID, 2001); !errors.Is(err, ErrConflict) {
		t.Errorf("second EnqueueGIF = %v, want ErrConflict", err)
	}
	// ...and so does a running one.
	if _, _, _, err := d.ClaimMediaJob(ctx, 2002); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnqueueGIF(ctx, c.ID, 2003); !errors.Is(err, ErrConflict) {
		t.Errorf("EnqueueGIF while running = %v, want ErrConflict", err)
	}
	// A finished one does not: the owner may ask for a fresh preview.
	if _, err := d.SetGIF(ctx, job.ID, c.ID, gifFields(), ready().RenditionETag, 2004); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnqueueGIF(ctx, c.ID, 2005); err != nil {
		t.Errorf("EnqueueGIF after the first finished: %v", err)
	}
}

func TestEnqueueGIFNeedsAReadyClip(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	owner := mkUser(t, d, "gifowner")
	c, _ := mkClip(t, d, owner, 1000, nil, false)
	if _, err := d.EnqueueGIF(ctx, c.ID, 1001); !errors.Is(err, ErrStateMismatch) {
		t.Errorf("EnqueueGIF on a queued clip = %v, want ErrStateMismatch", err)
	}
	if _, err := d.EnqueueGIF(ctx, "aZ3kQ9xB2mLp", 1001); !errors.Is(err, ErrNotFound) {
		t.Errorf("EnqueueGIF on an unknown clip = %v, want ErrNotFound", err)
	}
}

// TestSetGIFGuards is the commit's predicate table. Every false answer must
// change NOTHING, and must say why.
func TestSetGIFGuards(t *testing.T) {
	ctx := context.Background()

	// (a) the rendition changed under the job — the trim race.
	d := openTest(t)
	c, _ := readyClip(t, d)
	job := mustEnqueueGIF(t, d, c.ID, 2000)
	swap, err := d.SetGIF(ctx, job.ID, c.ID, gifFields(), "an-etag-from-before-the-trim", 2001)
	if err != nil {
		t.Fatal(err)
	}
	if swap.OK || swap.Reason != "the rendition changed" {
		t.Fatalf("stale swap = %+v, want a refusal naming the rendition", swap)
	}
	if got, _ := d.GetClip(ctx, c.ID); got.GIFKey != nil {
		t.Error("a stale preview was written")
	}
	if j, _ := d.GetLatestJob(ctx, c.ID); j.State != JobRunning {
		t.Errorf("the job was resolved by a refused swap: %s", j.State)
	}

	// (b) the clip is no longer ready.
	d = openTest(t)
	c, _ = readyClip(t, d)
	job = mustEnqueueGIF(t, d, c.ID, 2000)
	if err := d.MarkDeleting(ctx, c.ID, 2001); err != nil {
		t.Fatal(err)
	}
	swap, err = d.SetGIF(ctx, job.ID, c.ID, gifFields(), ready().RenditionETag, 2002)
	if err != nil || swap.OK || swap.Reason == "" {
		t.Fatalf("swap onto a deleting clip = %+v, %v", swap, err)
	}

	// (c) the job is no longer running (MarkDeleting cancelled it, a restart
	// released it): nothing is published on its behalf.
	d = openTest(t)
	c, _ = readyClip(t, d)
	job, err = d.EnqueueGIF(ctx, c.ID, 2000) // queued, never claimed
	if err != nil {
		t.Fatal(err)
	}
	swap, err = d.SetGIF(ctx, job.ID, c.ID, gifFields(), ready().RenditionETag, 2001)
	if err != nil || swap.OK || swap.Reason != "the job is no longer running" {
		t.Fatalf("swap from a queued job = %+v, %v", swap, err)
	}

	// (d) a transcode job is refused outright: only FinalizeTranscode may
	// ever write the state coupling, and only this may write gif_*.
	d = openTest(t)
	c, tj := readyClip(t, d)
	if _, err := d.SetGIF(ctx, tj.ID, c.ID, gifFields(), ready().RenditionETag, 2001); !errors.Is(err, ErrStateMismatch) {
		t.Errorf("SetGIF on a transcode job = %v, want ErrStateMismatch", err)
	}
}

// TestSetGIFSupersedesThePreviousPreview: a regenerate returns the old key so
// the caller can delete it, and never returns the key it just wrote.
func TestSetGIFSupersedesThePreviousPreview(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, _ := readyClip(t, d)

	first := mustEnqueueGIF(t, d, c.ID, 2000)
	swap, err := d.SetGIF(ctx, first.ID, c.ID, gifFields(), ready().RenditionETag, 2001)
	if err != nil || !swap.OK || len(swap.Superseded) != 0 {
		t.Fatalf("first swap = %+v, %v", swap, err)
	}
	got, _ := d.GetClip(ctx, c.ID)
	if got.GIFKey == nil || *got.GIFKey != gifFields().Key || *got.GIFSize != 4096 || *got.GIFETag != "gif-etag" ||
		*got.GIFWidth != 480 || *got.GIFHeight != 270 {
		t.Fatalf("gif columns = %+v", got)
	}
	if got.State != ClipReady || got.UpdatedAt != 2001 {
		t.Errorf("the clip's state or stamp is wrong after a gif: %+v", got)
	}
	if j, _ := d.GetLatestJob(ctx, c.ID); j.State != JobDone || j.Stage != "done" {
		t.Errorf("the job was not marked done: %+v", j)
	}

	second := mustEnqueueGIF(t, d, c.ID, 2002)
	next := gifFields()
	next.Key = "clips/x/renditions/preview-88888888.gif"
	swap, err = d.SetGIF(ctx, second.ID, c.ID, next, ready().RenditionETag, 2003)
	if err != nil || !swap.OK {
		t.Fatalf("second swap = %+v, %v", swap, err)
	}
	if len(swap.Superseded) != 1 || swap.Superseded[0] != gifFields().Key {
		t.Fatalf("superseded = %v, want the first preview's key", swap.Superseded)
	}
}

// TestSwapRenditionInvalidatesTheGIF is the trim's half of the contract: a
// new cut makes the old preview a picture of a clip that no longer exists,
// so the columns are cleared and the key is handed back for deletion.
func TestSwapRenditionInvalidatesTheGIF(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, _ := readyClip(t, d)
	gifJob := mustEnqueueGIF(t, d, c.ID, 2000)
	if swap, err := d.SetGIF(ctx, gifJob.ID, c.ID, gifFields(), ready().RenditionETag, 2001); err != nil || !swap.OK {
		t.Fatalf("SetGIF: %+v %v", swap, err)
	}

	trim, err := d.EnqueueTrim(ctx, c.ID, MarshalTrimParams(TrimParams{StartMS: 0, EndMS: 2000}), 2002)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := d.ClaimMediaJob(ctx, 2003); err != nil {
		t.Fatal(err)
	}
	f := ready()
	f.RenditionKey, f.RenditionETag, f.PosterKey = "r2", "re2", "p2"
	f.DurationMS = 2000
	superseded, ok, _, err := d.SwapRendition(ctx, trim.ID, c.ID, f, TrimParams{StartMS: 1000, EndMS: 3000}, 2004)
	if err != nil || !ok {
		t.Fatalf("SwapRendition: %v %v", ok, err)
	}
	got, _ := d.GetClip(ctx, c.ID)
	if got.GIFKey != nil || got.GIFSize != nil || got.GIFETag != nil || got.GIFWidth != nil || got.GIFHeight != nil {
		t.Fatalf("the trimmed clip kept its stale preview: %+v", got)
	}
	var sawGIF bool
	for _, k := range superseded {
		if k == gifFields().Key {
			sawGIF = true
		}
	}
	if !sawGIF {
		t.Fatalf("superseded = %v, want the preview's key among them", superseded)
	}
	// And a fresh preview can be made of the new cut.
	if _, err := d.EnqueueGIF(ctx, c.ID, 2005); err != nil {
		t.Fatalf("EnqueueGIF after a trim: %v", err)
	}
}

func mustEnqueueGIF(t *testing.T, d *DB, clipID string, now int64) Job {
	t.Helper()
	if _, err := d.EnqueueGIF(context.Background(), clipID, now); err != nil {
		t.Fatalf("EnqueueGIF: %v", err)
	}
	job, _, ok, err := d.ClaimMediaJob(context.Background(), now)
	if err != nil || !ok {
		t.Fatalf("ClaimMediaJob: %v %v", ok, err)
	}
	return job
}

// TestGIFLimitsAreTheirOwn pins the crash cap: a preview is attempted twice,
// not TRANSCODE_MAX_ATTEMPTS times, whatever the config says. Its environment
// cap is the configured one — that half is TestMediaKindsSplitTheirTwoCaps.
func TestGIFLimitsAreTheirOwn(t *testing.T) {
	generous := Limits{MaxAttempts: 9, MaxTransient: 9}
	if got := limitsFor(KindGIF, generous); got.MaxAttempts != GIFMaxAttempts || got.MaxTransient != generous.MaxTransient {
		t.Fatalf("limitsFor(gif) = %+v, want %d/%d", got, GIFMaxAttempts, generous.MaxTransient)
	}
	if got := limitsFor(KindTranscode, generous); got != generous {
		t.Fatalf("limitsFor(transcode) = %+v, want the config's own", got)
	}
}

// TestGIFFailureNeverMarksTheClip: FailAttempt moves a clip only for
// `transcode`, so a preview that fails twice leaves a ready, playable clip.
func TestGIFFailureNeverMarksTheClip(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, _ := readyClip(t, d)
	job := mustEnqueueGIF(t, d, c.ID, 2000)

	out, err := d.FailAttempt(ctx, job.ID, ClaimStamp(job), c.ID, "gif_timeout", FailPermanent, Limits{MaxAttempts: 9, MaxTransient: 9}, 2001)
	if err != nil || out != OutcomeFailed {
		t.Fatalf("FailAttempt = %v, %v", out, err)
	}
	got, _ := d.GetClip(ctx, c.ID)
	if got.State != ClipReady || got.FailureReason != nil || got.GIFKey != nil {
		t.Fatalf("a failed preview marked the clip: %+v", got)
	}
	if got.RenditionKey == nil || *got.RenditionKey != ready().RenditionKey {
		t.Error("the clip lost its rendition")
	}
}
