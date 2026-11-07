package db

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

// readyClip creates a clip and takes it all the way to `ready` through the
// real transitions, so a trim test starts from a row the API would have
// answered 200 for.
func readyClip(t *testing.T, d *DB) (Clip, Job) {
	t.Helper()
	ctx := context.Background()
	owner := mkUser(t, d, "trimmer")
	c, j := mkClip(t, d, owner, 1000, nil, false)
	if _, _, _, err := d.ClaimJob(ctx, 1000); err != nil {
		t.Fatalf("ClaimJob: %v", err)
	}
	ok, _, err := d.FinalizeTranscode(ctx, j.ID, c.ID, ready(), false, 1001)
	if err != nil || !ok {
		t.Fatalf("FinalizeTranscode: %v %v", ok, err)
	}
	c, err = d.GetClip(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return c, j
}

func TestTrimParamsRoundTrip(t *testing.T) {
	s := MarshalTrimParams(TrimParams{StartMS: 1000, EndMS: 3500})
	if s != `{"start_ms":1000,"end_ms":3500}` {
		t.Errorf("params column = %s", s)
	}
	p, err := ParseTrimParams(s)
	if err != nil || p.StartMS != 1000 || p.EndMS != 3500 {
		t.Fatalf("ParseTrimParams = %+v, %v", p, err)
	}
	for _, bad := range []string{"{}", "", "nope", `{"start_ms":-1,"end_ms":10}`, `{"start_ms":10,"end_ms":10}`} {
		if _, err := ParseTrimParams(bad); err == nil {
			t.Errorf("ParseTrimParams(%q) = nil error; a job with no usable window must not run", bad)
		}
	}
}

func TestEnqueueTrimAdmitsOneAtATime(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, _ := readyClip(t, d)
	params := MarshalTrimParams(TrimParams{StartMS: 0, EndMS: 2000})

	job, err := d.EnqueueTrim(ctx, c.ID, params, 2000)
	if err != nil {
		t.Fatalf("EnqueueTrim: %v", err)
	}
	if job.Kind != KindTrim || job.State != JobQueued || job.Stage != JobStageQueued || job.ParamsJSON != params {
		t.Fatalf("queued job = %+v", job)
	}
	// A queued trim blocks a second one...
	if _, err := d.EnqueueTrim(ctx, c.ID, params, 2001); !errors.Is(err, ErrConflict) {
		t.Errorf("second EnqueueTrim = %v, want ErrConflict", err)
	}
	// ...and so does a running one.
	if _, _, _, err := d.ClaimMediaJob(ctx, 2002); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnqueueTrim(ctx, c.ID, params, 2003); !errors.Is(err, ErrConflict) {
		t.Errorf("EnqueueTrim while running = %v, want ErrConflict", err)
	}
	// A finished one does not.
	if _, _, _, err := d.SwapRendition(ctx, job.ID, c.ID, ready(), TrimParams{StartMS: 1000, EndMS: 3000}, 2004); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnqueueTrim(ctx, c.ID, params, 2005); err != nil {
		t.Errorf("EnqueueTrim after the first finished: %v", err)
	}
}

// TestEnqueueTrimIsSerialisedUnderConcurrency is the 409 the API promises,
// proven where it is actually decided: the guard and the INSERT are one
// transaction on the single-writer pool, so N simultaneous clicks produce
// exactly one job.
func TestEnqueueTrimIsSerialisedUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, _ := readyClip(t, d)
	params := MarshalTrimParams(TrimParams{StartMS: 0, EndMS: 2000})

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	won, conflicts := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := d.EnqueueTrim(ctx, c.ID, params, 2000)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, ErrConflict):
				conflicts++
			default:
				t.Errorf("EnqueueTrim: %v", err)
			}
		}()
	}
	wg.Wait()
	if won != 1 || conflicts != n-1 {
		t.Fatalf("%d winners and %d conflicts out of %d; want exactly one winner", won, conflicts, n)
	}
}

func TestEnqueueTrimRequiresAReadyClip(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	owner := mkUser(t, d, "queued-owner")
	c, _ := mkClip(t, d, owner, 1000, nil, false) // still `queued`
	params := MarshalTrimParams(TrimParams{StartMS: 0, EndMS: 2000})
	if _, err := d.EnqueueTrim(ctx, c.ID, params, 1001); !errors.Is(err, ErrStateMismatch) {
		t.Errorf("EnqueueTrim on a queued clip = %v, want ErrStateMismatch", err)
	}
	if _, err := d.EnqueueTrim(ctx, "nosuchclipid", params, 1001); !errors.Is(err, ErrNotFound) {
		t.Errorf("EnqueueTrim on an unknown clip = %v, want ErrNotFound", err)
	}
}

// swapFields is the measured result of a trim: new keys, half the duration.
func swapFields() ReadyFields {
	f := ready()
	f.RenditionKey, f.RenditionETag, f.RenditionSize = "r2", "re2", 20
	f.PosterKey, f.PosterETag, f.PosterSize = "p2", "pe2", 6
	f.DurationMS = 2000
	return f
}

func TestSwapRenditionIsOneCommitAndReportsTheOldKeys(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, _ := readyClip(t, d)
	job, err := d.EnqueueTrim(ctx, c.ID, MarshalTrimParams(TrimParams{StartMS: 1000, EndMS: 3000}), 2000)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := d.ClaimMediaJob(ctx, 2001); err != nil {
		t.Fatal(err)
	}
	// The clip must still be READY after the claim: only `transcode` moves it.
	if got, _ := d.GetClip(ctx, c.ID); got.State != ClipReady {
		t.Fatalf("claiming a trim moved the clip to %q", got.State)
	}

	superseded, ok, _, err := d.SwapRendition(ctx, job.ID, c.ID, swapFields(), TrimParams{StartMS: 1000, EndMS: 3000}, 2002)
	if err != nil || !ok {
		t.Fatalf("SwapRendition = %v, %v", ok, err)
	}
	if len(superseded) != 2 || superseded[0] != "r" || superseded[1] != "p" {
		t.Errorf("superseded = %v, want the previous rendition and poster", superseded)
	}
	got, err := d.GetClip(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ClipReady || got.FailureReason != nil {
		t.Errorf("clip is %q/%v after a trim; it must stay ready and unblemished", got.State, got.FailureReason)
	}
	if *got.RenditionKey != "r2" || *got.PosterKey != "p2" || *got.RenditionETag != "re2" || *got.PosterETag != "pe2" {
		t.Errorf("keys not swapped: %+v", got)
	}
	if *got.DurationMS != 2000 {
		t.Errorf("duration_ms = %d, want the trimmed 2000", *got.DurationMS)
	}
	if got.UpdatedAt != 2002 {
		t.Errorf("updated_at = %d, want the swap's now", got.UpdatedAt)
	}
	// ready_at is when the clip first became playable and is not re-stamped.
	if got.ReadyAt == nil || *got.ReadyAt != 1001 {
		t.Errorf("ready_at = %v, want the original 1001", got.ReadyAt)
	}
	j, err := getJob(ctx, d.ro, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != JobDone || j.Stage != "done" || j.FinishedAt == nil {
		t.Errorf("job after the swap = %+v, want done", j)
	}
}

func TestSwapRenditionLosesTheRaceCleanly(t *testing.T) {
	ctx := context.Background()
	params := MarshalTrimParams(TrimParams{StartMS: 1000, EndMS: 3000})

	t.Run("the clip was tombstoned meanwhile", func(t *testing.T) {
		d := openTest(t)
		c, _ := readyClip(t, d)
		job, err := d.EnqueueTrim(ctx, c.ID, params, 2000)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := d.ClaimMediaJob(ctx, 2001); err != nil {
			t.Fatal(err)
		}
		if err := d.MarkDeleting(ctx, c.ID, 2002); err != nil {
			t.Fatal(err)
		}
		superseded, ok, _, err := d.SwapRendition(ctx, job.ID, c.ID, swapFields(), TrimParams{StartMS: 1000, EndMS: 3000}, 2003)
		if err != nil || ok || superseded != nil {
			t.Fatalf("SwapRendition on a tombstoned clip = %v, %v, %v; want (nil, false, nil)", superseded, ok, err)
		}
		got, _ := d.GetClip(ctx, c.ID)
		if *got.RenditionKey != "r" {
			t.Errorf("the lost race still swapped the rendition to %q", *got.RenditionKey)
		}
	})

	t.Run("the job is no longer running", func(t *testing.T) {
		d := openTest(t)
		c, _ := readyClip(t, d)
		job, err := d.EnqueueTrim(ctx, c.ID, params, 2000)
		if err != nil {
			t.Fatal(err)
		}
		// Never claimed: still queued.
		_, ok, _, err := d.SwapRendition(ctx, job.ID, c.ID, swapFields(), TrimParams{StartMS: 1000, EndMS: 3000}, 2001)
		if err != nil || ok {
			t.Fatalf("SwapRendition on a queued job = %v, %v; want (false, nil)", ok, err)
		}
		got, _ := d.GetClip(ctx, c.ID)
		if *got.RenditionKey != "r" {
			t.Error("a job that was not running still swapped the rendition")
		}
	})

	t.Run("a transcode job is refused outright", func(t *testing.T) {
		d := openTest(t)
		c, tj := readyClip(t, d)
		_, _, _, err := d.SwapRendition(ctx, tj.ID, c.ID, swapFields(), TrimParams{StartMS: 1000, EndMS: 3000}, 2001)
		if !errors.Is(err, ErrStateMismatch) {
			t.Fatalf("SwapRendition on a transcode job = %v, want ErrStateMismatch (only FinalizeTranscode may)", err)
		}
	})
}

func TestLatestJobOfKind(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, tj := readyClip(t, d)
	if _, err := d.LatestJobOfKind(ctx, c.ID, KindTrim); !errors.Is(err, ErrNotFound) {
		t.Errorf("LatestJobOfKind with no trim = %v, want ErrNotFound", err)
	}
	first, err := d.EnqueueTrim(ctx, c.ID, MarshalTrimParams(TrimParams{StartMS: 0, EndMS: 1000}), 2000)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := d.ClaimMediaJob(ctx, 2001); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := d.SwapRendition(ctx, first.ID, c.ID, swapFields(), TrimParams{StartMS: 1000, EndMS: 3000}, 2002); err != nil {
		t.Fatal(err)
	}
	second, err := d.EnqueueTrim(ctx, c.ID, MarshalTrimParams(TrimParams{StartMS: 100, EndMS: 900}), 2003)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.LatestJobOfKind(ctx, c.ID, KindTrim)
	if err != nil || got.ID != second.ID {
		t.Errorf("LatestJobOfKind = %d (%v), want the newest trim %d", got.ID, err, second.ID)
	}
	// And the transcode job is still findable under its own kind.
	if got, err := d.LatestJobOfKind(ctx, c.ID, KindTranscode); err != nil || got.ID != tj.ID {
		t.Errorf("LatestJobOfKind(transcode) = %d (%v), want %d", got.ID, err, tj.ID)
	}
}

// TestTrimFailureNeverMarksTheClip is the §5 rule at the level a trim can
// break it: two failed attempts must exhaust the job and leave the clip
// exactly as ready as it was.
func TestTrimFailureNeverMarksTheClip(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, _ := readyClip(t, d)
	job, err := d.EnqueueTrim(ctx, c.ID, MarshalTrimParams(TrimParams{StartMS: 0, EndMS: 2000}), 2000)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately hand it the TRANSCODE limits a caller might be holding:
	// limitsFor must pin the trim's own cap regardless.
	generous := Limits{MaxAttempts: 99, MaxTransient: 99}
	for i := 1; i <= TrimMaxAttempts; i++ {
		job, _, ok, err := d.ClaimMediaJob(ctx, int64(2000+i*100))
		if err != nil || !ok {
			t.Fatalf("ClaimMediaJob: %v %v", ok, err)
		}
		out, err := d.FailAttempt(ctx, job.ID, ClaimStamp(job), c.ID, "ffmpeg said no", FailCrash, generous, int64(2000+i*100))
		if err != nil {
			t.Fatal(err)
		}
		want := OutcomeRequeued
		if i == TrimMaxAttempts {
			want = OutcomeFailed
		}
		if out != want {
			t.Fatalf("attempt %d outcome = %v, want %v (cap is %d)", i, out, want, TrimMaxAttempts)
		}
		got, _ := d.GetClip(ctx, c.ID)
		if got.State != ClipReady || got.FailureReason != nil {
			t.Fatalf("after attempt %d the clip is %q/%v; a failed trim is not a failed clip", i, got.State, got.FailureReason)
		}
	}
	j, _ := getJob(ctx, d.ro, job.ID)
	if j.State != JobFailed || j.LastError == nil || *j.LastError != "ffmpeg said no" {
		t.Errorf("job = %+v, want failed with the reason", j)
	}
	got, _ := d.GetClip(ctx, c.ID)
	if *got.RenditionKey != "r" {
		t.Errorf("the clip lost its rendition to a failed trim: %q", *got.RenditionKey)
	}
}

// TestMigrate0003UpgradesA0002Database is the live upgrade path for the trim
// window contract: a database that has only ever seen 0001+0002 — with rows
// in it, one of them ALREADY TRIMMED — gains the three columns, and the
// backfill is conservative in exactly the place it has to be.
func TestMigrate0003UpgradesA0002Database(t *testing.T) {
	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "clippet.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if _, err := d.rw.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	files, err := migrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 3 || files[2].version != 3 {
		t.Fatalf("migration set = %+v, want 0003 third", files)
	}
	for _, m := range files[:2] {
		if err := d.applyMigration(ctx, m); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}
	if _, err := d.rw.ExecContext(ctx,
		"INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1', 'upgrader', 'h', 1000)"); err != nil {
		t.Fatal(err)
	}
	// Two ready clips of the same measured length: one has never been
	// trimmed, one has (its duration_ms therefore describes a CUT, not the
	// upload — which is the whole reason the backfill has to tell them apart).
	insert := func(id string) {
		t.Helper()
		if _, err := d.rw.ExecContext(ctx, `
INSERT INTO clips (id, owner_id, title, description, unlisted, state,
  source_key, source_filename, source_size, source_content_type, source_etag,
  rendition_key, rendition_size, rendition_etag,
  poster_key, poster_size, poster_etag, poster_width, poster_height,
  duration_ms, width, height, fps_num, fps_den, has_audio, created_at, updated_at, ready_at)
VALUES (?, 'u1', 'old clip', '', 0, 'ready', 'clips/x/original/source.mp4', 'clip.mp4', 123, 'video/mp4', 'se',
  'r', 10, 're', 'p', 5, 'pe', 1280, 720, 2000, 1280, 720, 30, 1, 1, 1000, 1000, 1001)`, id); err != nil {
			t.Fatal(err)
		}
	}
	const untrimmed, trimmed = "aZ3kQ9xB2mLp", "bY4lR8yC3nMq"
	insert(untrimmed)
	insert(trimmed)
	if _, err := d.rw.ExecContext(ctx, `
INSERT INTO jobs (clip_id, kind, state, stage, params_json, run_after, created_at, updated_at)
VALUES (?, 'trim', 'done', 'done', '{"start_ms":1000,"end_ms":3000}', 0, 1000, 1000)`, trimmed); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetClip(ctx, untrimmed); err == nil {
		t.Fatal("GetClip succeeded before 0003; the three columns are not in clipCols")
	}

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate to 0003: %v", err)
	}
	// The clip that was never cut is its own original, to the millisecond the
	// encoder measured; the one that WAS cut keeps NULL, because its
	// duration_ms is a lie about the upload and a guess here would be the
	// very bug this migration exists to fix.
	got, err := d.GetClip(ctx, untrimmed)
	if err != nil {
		t.Fatalf("GetClip after the upgrade: %v", err)
	}
	if got.SourceDurationMS == nil || *got.SourceDurationMS != 2000 {
		t.Errorf("an untrimmed clip's source_duration_ms = %v, want its own 2000", got.SourceDurationMS)
	}
	if got.TrimStartMS != nil || got.TrimEndMS != nil {
		t.Errorf("an untrimmed clip has a window: %v..%v", got.TrimStartMS, got.TrimEndMS)
	}
	cut, err := d.GetClip(ctx, trimmed)
	if err != nil {
		t.Fatal(err)
	}
	if cut.SourceDurationMS != nil {
		t.Errorf("an already-trimmed clip was backfilled to %v; its duration describes the cut, not the upload", *cut.SourceDurationMS)
	}
	// The rest of the row is untouched, and the feature works on it: a trim
	// of the ambiguous clip fills the column from its own probe.
	if cut.State != ClipReady || *cut.RenditionKey != "r" || cut.Views != 0 {
		t.Errorf("the upgrade disturbed the row: %+v", cut)
	}
	job, err := d.EnqueueTrim(ctx, cut.ID, MarshalTrimParams(TrimParams{StartMS: 4000, EndMS: 6000}), 2000)
	if err != nil {
		t.Fatalf("EnqueueTrim on an upgraded row: %v", err)
	}
	if _, _, _, err := d.ClaimMediaJob(ctx, 2001); err != nil {
		t.Fatal(err)
	}
	f := swapFields()
	src := int64(9000)
	f.SourceDurationMS = &src
	if _, ok, _, err := d.SwapRendition(ctx, job.ID, cut.ID, f, TrimParams{StartMS: 4000, EndMS: 6000}, 2002); err != nil || !ok {
		t.Fatalf("SwapRendition on an upgraded row: %v %v", ok, err)
	}
	after, _ := d.GetClip(ctx, cut.ID)
	if after.SourceDurationMS == nil || *after.SourceDurationMS != 9000 {
		t.Errorf("source_duration_ms after the trim = %v, want the probe's 9000", after.SourceDurationMS)
	}
	if after.TrimStartMS == nil || *after.TrimStartMS != 4000 || *after.TrimEndMS != 6000 {
		t.Errorf("window = %v..%v, want 4000..6000", after.TrimStartMS, after.TrimEndMS)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate again: %v", err)
	}
}

// TestSwapRenditionPersistsTheWindow: the swap is also what records where the
// new rendition came from, and a later transcode (a retry) resets it — that
// clip is the whole upload again.
func TestSwapRenditionPersistsTheWindow(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, _ := readyClip(t, d)
	job, err := d.EnqueueTrim(ctx, c.ID, MarshalTrimParams(TrimParams{StartMS: 1000, EndMS: 3000}), 2000)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := d.ClaimMediaJob(ctx, 2001); err != nil {
		t.Fatal(err)
	}
	f := swapFields()
	// A copy path snapped the start back to its keyframe: what is persisted
	// is what was CUT, never what was asked for.
	if _, ok, _, err := d.SwapRendition(ctx, job.ID, c.ID, f, TrimParams{StartMS: 960, EndMS: 3000}, 2002); err != nil || !ok {
		t.Fatalf("SwapRendition: %v %v", ok, err)
	}
	got, _ := d.GetClip(ctx, c.ID)
	if got.TrimStartMS == nil || *got.TrimStartMS != 960 || *got.TrimEndMS != 3000 {
		t.Fatalf("window = %v..%v, want the applied 960..3000", got.TrimStartMS, got.TrimEndMS)
	}
	if got.SourceDurationMS == nil || *got.SourceDurationMS != 6000 {
		t.Errorf("source_duration_ms = %v; a trim with no measurement of its own must not erase it", got.SourceDurationMS)
	}
	// A retry re-encodes the WHOLE original, so the window goes away with it.
	// (The clip has to be `failed` for a retry to be legal; nothing else about
	// this test needs the failure, so it is forged rather than provoked.)
	if _, err := d.rw.ExecContext(ctx, "UPDATE clips SET state = 'failed' WHERE id = ?", c.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.RetryClip(ctx, c.ID, 3000); err != nil {
		t.Fatal(err)
	}
	tj, _, ok, err := d.ClaimJob(ctx, 3001)
	if err != nil || !ok {
		t.Fatalf("ClaimJob: %v %v", ok, err)
	}
	if ok, _, err := d.FinalizeTranscode(ctx, tj.ID, c.ID, ready(), false, 3002); err != nil || !ok {
		t.Fatalf("FinalizeTranscode: %v %v", ok, err)
	}
	got, _ = d.GetClip(ctx, c.ID)
	if got.TrimStartMS != nil || got.TrimEndMS != nil {
		t.Errorf("a re-encoded clip still claims a window: %v..%v", got.TrimStartMS, got.TrimEndMS)
	}
}

// TestMediaKindsSplitTheirTwoCaps pins §20 erratum 89. For a trim and a gif
// the CRASH cap is the kind's own constant — two, because the file may be
// what is crashing us and asking again from the share page is a better retry
// than a machine guessing — while the ENVIRONMENT cap is the instance's
// configured TRANSCODE_MAX_TRANSIENT: a storage outage is not an argument
// about this file and does not become one because the job is an edit.
func TestMediaKindsSplitTheirTwoCaps(t *testing.T) {
	cfg := Limits{MaxAttempts: 9, MaxTransient: 7}
	for _, tc := range []struct {
		kind     string
		attempts int
	}{
		{KindTrim, TrimMaxAttempts},
		{KindGIF, GIFMaxAttempts},
	} {
		got := limitsFor(tc.kind, cfg)
		if got.MaxAttempts != tc.attempts || got.MaxAttempts != 2 {
			t.Errorf("limitsFor(%s).MaxAttempts = %d, want the kind's own %d", tc.kind, got.MaxAttempts, tc.attempts)
		}
		if got.MaxTransient != cfg.MaxTransient {
			t.Errorf("limitsFor(%s).MaxTransient = %d, want the configured %d", tc.kind, got.MaxTransient, cfg.MaxTransient)
		}
		// A caller holding no config at all still gets the §4 defaults, not
		// zero (which would fail a job on its first bad ending).
		if got := limitsFor(tc.kind, Limits{}); got.MaxTransient != defaultMaxTransient {
			t.Errorf("limitsFor(%s, zero).MaxTransient = %d, want the default %d", tc.kind, got.MaxTransient, defaultMaxTransient)
		}
	}
	// The two kinds that are NOT media are untouched: a transcode gets
	// exactly what the operator configured, a notify its own constant twice.
	if got := limitsFor(KindTranscode, cfg); got != cfg {
		t.Errorf("limitsFor(transcode) = %+v, want the config's own", got)
	}
	if got := limitsFor(KindNotify, cfg); got.MaxAttempts != NotifyMaxAttempts || got.MaxTransient != NotifyMaxAttempts {
		t.Errorf("limitsFor(notify) = %+v, want %d/%d", got, NotifyMaxAttempts, NotifyMaxAttempts)
	}
	// Cap names the number the ending in hand is measured against.
	if got := Cap(KindTrim, FailCrash, cfg); got != TrimMaxAttempts {
		t.Errorf("Cap(trim, crash) = %d, want %d", got, TrimMaxAttempts)
	}
	if got := Cap(KindTrim, FailTransient, cfg); got != cfg.MaxTransient {
		t.Errorf("Cap(trim, transient) = %d, want the configured %d", got, cfg.MaxTransient)
	}
}

// TestTrimOutlastsTheEnvironmentButNotACrashLoop is that same split seen from
// the job table: three storage failures leave the trim QUEUED (nothing about
// the edit is at fault, and the next attempt costs the owner nothing), while
// two crash-class endings fail it for good.
func TestTrimOutlastsTheEnvironmentButNotACrashLoop(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	cfg := Limits{MaxAttempts: defaultMaxAttempts, MaxTransient: defaultMaxTransient}

	// Three transient endings, each after the previous backoff has expired.
	transient, _ := readyClip(t, d)
	_, err := d.EnqueueTrim(ctx, transient.ID, MarshalTrimParams(TrimParams{StartMS: 0, EndMS: 2000}), 2000)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		now := int64(2000 + i*1000)
		job, _, ok, err := d.ClaimMediaJob(ctx, now)
		if err != nil || !ok {
			t.Fatalf("claim %d: ok=%v err=%v", i, ok, err)
		}
		out, err := d.FailAttempt(ctx, job.ID, ClaimStamp(job), transient.ID, "storage was unavailable", FailTransient, cfg, now)
		if err != nil {
			t.Fatal(err)
		}
		if out != OutcomeRequeued {
			t.Fatalf("transient %d: outcome = %v, want a requeue — the environment failed, not the trim", i, out)
		}
	}
	got, err := d.LatestJobOfKind(ctx, transient.ID, KindTrim)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != JobQueued || got.TransientFailures != 3 || got.Attempts != 0 {
		t.Fatalf("after three outages the trim is %s (attempts=%d transient=%d), want queued 0/3",
			got.State, got.Attempts, got.TransientFailures)
	}
	if c, _ := d.GetClip(ctx, transient.ID); c.State != ClipReady || c.FailureReason != nil {
		t.Errorf("clip is %q/%v; a trim never marks its clip", c.State, c.FailureReason)
	}

	// Two crash-class endings. On a database of its own: the trim above is
	// still queued, and ClaimMediaJob takes the OLDEST claimable job.
	d = openTest(t)
	crash, _ := readyClip(t, d)
	_, err = d.EnqueueTrim(ctx, crash.ID, MarshalTrimParams(TrimParams{StartMS: 0, EndMS: 2000}), 6000)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= TrimMaxAttempts; i++ {
		now := int64(6000 + i*1000)
		cj, _, ok, err := d.ClaimMediaJob(ctx, now)
		if err != nil || !ok {
			t.Fatalf("claim %d: ok=%v err=%v", i, ok, err)
		}
		out, err := d.FailAttempt(ctx, cj.ID, ClaimStamp(cj), crash.ID, "panic: kaboom", FailCrash, cfg, now)
		if err != nil {
			t.Fatal(err)
		}
		want := OutcomeRequeued
		if i == TrimMaxAttempts {
			want = OutcomeFailed
		}
		if out != want {
			t.Fatalf("crash %d: outcome = %v, want %v (the crash cap is %d)", i, out, want, TrimMaxAttempts)
		}
	}
	if c, _ := d.GetClip(ctx, crash.ID); c.State != ClipReady || c.FailureReason != nil {
		t.Errorf("clip is %q/%v after a crash-looping trim; the clip is never marked", c.State, c.FailureReason)
	}
}
