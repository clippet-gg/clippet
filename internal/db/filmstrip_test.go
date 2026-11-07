package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// The filmstrip's database half (§20 erratum 220). Every test here asserts the
// same background property from a different angle: whatever happens to a sheet,
// the CLIP is ready, playable and otherwise untouched.

func filmstripFields() FilmstripFields {
	return FilmstripFields{Key: "clips/x/filmstrip/strip-f8f8f8f8.jpg", Size: 200_000, ETag: "strip-etag",
		Tiles: 24, TileW: 160, TileH: 90, IntervalMS: 250}
}

// filmstripReady is readyClip under another name: a clip taken to `ready`
// through the real transitions, so these tests start from a row the API would
// answer 200 for.
func filmstripReady(t *testing.T, d *DB) Clip {
	t.Helper()
	c, _ := readyClip(t, d)
	return c
}

// hasKey reports whether ss holds s. internal/db's tests carry no such helper
// and the three key reports below all need one.
func hasKey(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// claimOne claims the next media job and asserts its kind — with no worker
// running, the order is the insertion order.
func claimOne(t *testing.T, d *DB, kind string, now int64) Job {
	t.Helper()
	j, _, ok, err := d.ClaimMediaJob(context.Background(), now)
	if err != nil || !ok || j.Kind != kind {
		t.Fatalf("ClaimMediaJob = %+v ok=%v err=%v, want a %s job", j, ok, err, kind)
	}
	return j
}

// TestEnqueueFilmstripAdmitsOneLiveJob is the whole rate story of the public
// route: the guard, not a limiter, is what keeps a hundred simultaneous first
// requests from becoming a hundred jobs.
func TestEnqueueFilmstripAdmitsOneLiveJob(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	c := filmstripReady(t, d)

	job, err := d.EnqueueFilmstrip(ctx, c.ID, 2000)
	if err != nil || job.Kind != KindFilmstrip || job.State != JobQueued || job.Stage != JobStageQueued {
		t.Fatalf("EnqueueFilmstrip = %+v, %v", job, err)
	}
	if _, err := d.EnqueueFilmstrip(ctx, c.ID, 2001); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second live filmstrip job: err = %v, want ErrConflict", err)
	}
	// …and a job that is RUNNING is just as live as a queued one.
	claimOne(t, d, KindFilmstrip, 2002)
	if _, err := d.EnqueueFilmstrip(ctx, c.ID, 2003); !errors.Is(err, ErrConflict) {
		t.Fatalf("a filmstrip job while one runs: err = %v, want ErrConflict", err)
	}
	// Once it has committed, the next request may queue another — which is what
	// makes "remake it" the GET's own business and not a route of its own.
	swap, err := d.SetFilmstrip(ctx, job.ID, c.ID, filmstripFields(), ready().RenditionETag, 2004)
	if err != nil || !swap.OK {
		t.Fatalf("SetFilmstrip = %+v, %v", swap, err)
	}
	if _, err := d.EnqueueFilmstrip(ctx, c.ID, 2005); err != nil {
		t.Fatalf("EnqueueFilmstrip after one landed: %v", err)
	}
}

// TestEnqueueFilmstripNeedsAReadyClip: the two refusals the route turns into a
// 409 and a 404.
func TestEnqueueFilmstripNeedsAReadyClip(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	owner := mkUser(t, d, "notready")
	c, _ := mkClip(t, d, owner, 1000, nil, false)
	if _, err := d.EnqueueFilmstrip(ctx, c.ID, 1001); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("a queued clip: err = %v, want ErrStateMismatch", err)
	}
	if _, err := d.EnqueueFilmstrip(ctx, "aZ3kQ9xB2mLp", 1002); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown clip: err = %v, want ErrNotFound", err)
	}
}

// TestSetFilmstripWritesOnlyItsOwnColumns: the commit is the seven columns and
// the job row, and nothing else on the clip moves — not the state, not the
// rendition, not the poster, not a counter.
func TestSetFilmstripWritesOnlyItsOwnColumns(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	c := filmstripReady(t, d)
	before := c

	job, err := d.EnqueueFilmstrip(ctx, c.ID, 2000)
	if err != nil {
		t.Fatal(err)
	}
	claimOne(t, d, KindFilmstrip, 2001)
	f := filmstripFields()
	swap, err := d.SetFilmstrip(ctx, job.ID, c.ID, f, ready().RenditionETag, 2002)
	if err != nil || !swap.OK || len(swap.Superseded) != 0 {
		t.Fatalf("SetFilmstrip = %+v, %v", swap, err)
	}
	got, err := d.GetClip(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FilmstripKey == nil || *got.FilmstripKey != f.Key ||
		got.FilmstripSize == nil || *got.FilmstripSize != f.Size ||
		got.FilmstripETag == nil || *got.FilmstripETag != f.ETag ||
		got.FilmstripTiles == nil || *got.FilmstripTiles != f.Tiles ||
		got.FilmstripTileW == nil || *got.FilmstripTileW != f.TileW ||
		got.FilmstripTileH == nil || *got.FilmstripTileH != f.TileH ||
		got.FilmstripIntervalMS == nil || *got.FilmstripIntervalMS != f.IntervalMS {
		t.Fatalf("the filmstrip columns did not land: %+v", got)
	}
	if got.State != ClipReady || *got.RenditionKey != *before.RenditionKey ||
		*got.PosterKey != *before.PosterKey || got.Views != before.Views ||
		got.FailureReason != nil {
		t.Errorf("SetFilmstrip disturbed the clip: %+v", got)
	}
	if got.UpdatedAt != 2002 {
		t.Errorf("updated_at = %d, want the commit's clock (a cache revalidates on it)", got.UpdatedAt)
	}
	if j, err := d.LatestJobOfKind(ctx, c.ID, KindFilmstrip); err != nil || j.ID != job.ID ||
		j.State != JobDone || j.Stage != "done" {
		t.Errorf("the job is %+v, %v", j, err)
	}

	// A SECOND sheet supersedes the first, and the old key comes back so the
	// caller can delete the object nothing references any more.
	job2, err := d.EnqueueFilmstrip(ctx, c.ID, 2003)
	if err != nil {
		t.Fatal(err)
	}
	claimOne(t, d, KindFilmstrip, 2004)
	f2 := filmstripFields()
	f2.Key = "clips/x/filmstrip/strip-99999999.jpg"
	swap, err = d.SetFilmstrip(ctx, job2.ID, c.ID, f2, ready().RenditionETag, 2005)
	if err != nil || !swap.OK {
		t.Fatalf("second SetFilmstrip = %+v, %v", swap, err)
	}
	if len(swap.Superseded) != 1 || swap.Superseded[0] != f.Key {
		t.Errorf("superseded = %v, want the first sheet's key", swap.Superseded)
	}
}

// TestSetFilmstripIsGuardedByTheRendition is the honesty predicate: a sheet cut
// from a rendition a trim has since replaced describes a clip that no longer
// exists, and its tiles would sit under handles naming moments it does not show.
func TestSetFilmstripIsGuardedByTheRendition(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	c := filmstripReady(t, d)
	job, err := d.EnqueueFilmstrip(ctx, c.ID, 2000)
	if err != nil {
		t.Fatal(err)
	}
	job = claimOne(t, d, KindFilmstrip, 2001)
	swap, err := d.SetFilmstrip(ctx, job.ID, c.ID, filmstripFields(), "a-rendition-this-clip-never-had", 2002)
	if err != nil {
		t.Fatalf("SetFilmstrip: %v", err)
	}
	if swap.OK || swap.Reason != "the rendition changed" {
		t.Fatalf("swap = %+v, want a refusal naming the rendition", swap)
	}
	got, err := d.GetClip(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FilmstripKey != nil {
		t.Errorf("a stale sheet was published: %v", *got.FilmstripKey)
	}
	// The other three refusals a run can meet, each reported and never an error.
	for _, tc := range []struct {
		name, want string
		jobID      int64
	}{
		{"a job row that is gone", "the job row is gone", 99999},
		{"a job that is no longer running", "the job is no longer running", job.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The first SetFilmstrip above left this job `running`; resolving it
			// is what makes the second case true.
			if tc.jobID == job.ID {
				if err := d.CompleteJob(ctx, job.ID, ClaimStamp(job), JobStageSkipped, "test", 2003); err != nil {
					t.Fatal(err)
				}
			}
			swap, err := d.SetFilmstrip(ctx, tc.jobID, c.ID, filmstripFields(), ready().RenditionETag, 2004)
			if err != nil || swap.OK || swap.Reason != tc.want {
				t.Fatalf("swap = %+v, %v, want Reason %q", swap, err, tc.want)
			}
		})
	}
	// And a transcode job is refused outright, keeping the §5 coupling rule
	// where it belongs: nothing but a filmstrip job may write these columns.
	tj, err := d.LatestJobOfKind(ctx, c.ID, KindTranscode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SetFilmstrip(ctx, tj.ID, c.ID, filmstripFields(), ready().RenditionETag, 2005); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("SetFilmstrip on a transcode job: err = %v, want ErrStateMismatch", err)
	}
}

// TestSetFilmstripCommitsOnAClipWithNoRendition is the lane-4 merge's regression
// guard (§20 errata 201, 220 as amended). A clip on the REMUX FAST PATH is ready
// with `rendition_key` NULL — the upload IS the playable file — and that is now
// the commonest shape of all. The commit's predicate is therefore db.PlayObject's
// etag (`playETagSQL`), not `rendition_etag`: guarded on the column, every such
// clip's sheet would have been produced and then silently thrown away.
func TestSetFilmstripCommitsOnAClipWithNoRendition(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	owner := mkUser(t, d, "directplay")
	c, j := mkClip(t, d, owner, 1000, nil, false)
	if _, _, _, err := d.ClaimJob(ctx, 1000); err != nil {
		t.Fatalf("ClaimJob: %v", err)
	}
	// The direct path: no rendition object, no card copy, the upload played as
	// it stands.
	f := ReadyFields{
		PosterKey: "p", PosterSize: 5, PosterETag: "pe", PosterWidth: 1280, PosterHeight: 720,
		Width: 1280, Height: 720, DurationMS: 6000, FPSNum: 30, FPSDen: 1, HasAudio: true,
		Tiers: TierFields{OriginalPlayable: true, OriginalWidth: 1280, OriginalHeight: 720,
			OriginalFPSNum: 30, OriginalFPSDen: 1, OriginalCodec: "h264",
			FullState: TierSkipped, LightState: TierSkipped},
	}
	if ok, _, err := d.FinalizeTranscode(ctx, j.ID, c.ID, f, false, 1001); err != nil || !ok {
		t.Fatalf("FinalizeTranscode: ok=%v err=%v", ok, err)
	}
	c, err := d.GetClip(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.RenditionKey != nil {
		t.Fatalf("rendition_key = %q; this test is about the clip that has none", *c.RenditionKey)
	}
	play, ok := PlayETagOf(c)
	if !ok || play != c.SourceETag {
		t.Fatalf("PlayETagOf = %q ok=%v, want the source's %q", play, ok, c.SourceETag)
	}

	job, err := d.EnqueueFilmstrip(ctx, c.ID, 2000)
	if err != nil {
		t.Fatal(err)
	}
	claimOne(t, d, KindFilmstrip, 2001)
	// The guard still bites on this row: an etag the clip never played is
	// refused, so the predicate is a predicate and not a NULL that matches
	// anything. (It comes first because a successful commit resolves the job.)
	if swap, err := d.SetFilmstrip(ctx, job.ID, c.ID, filmstripFields(), "never-played", 2002); err != nil ||
		swap.OK || swap.Reason != "the rendition changed" {
		t.Fatalf("swap = %+v, %v, want a refusal", swap, err)
	}
	swap, err := d.SetFilmstrip(ctx, job.ID, c.ID, filmstripFields(), play, 2003)
	if err != nil || !swap.OK {
		t.Fatalf("SetFilmstrip on a clip with no rendition = %+v, %v; the sheet must commit", swap, err)
	}
	got, err := d.GetClip(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FilmstripKey == nil || *got.FilmstripKey != filmstripFields().Key {
		t.Fatalf("the filmstrip columns did not land: %+v", got.FilmstripKey)
	}
}

// TestTrimDropsTheFilmstrip and its replacement twin are the invalidation half
// of erratum 220: the columns are NULLed and the object is listed as superseded
// by the very statement that swaps the rendition, so the route makes a fresh
// sheet on the next request and no object outlives the row that referenced it.
func TestTrimDropsTheFilmstrip(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	c := filmstripReady(t, d)
	f := filmstripFields()

	job, err := d.EnqueueFilmstrip(ctx, c.ID, 2000)
	if err != nil {
		t.Fatal(err)
	}
	claimOne(t, d, KindFilmstrip, 2001)
	if swap, err := d.SetFilmstrip(ctx, job.ID, c.ID, f, ready().RenditionETag, 2002); err != nil || !swap.OK {
		t.Fatalf("SetFilmstrip = %+v, %v", swap, err)
	}

	tjob, err := d.EnqueueTrim(ctx, c.ID, MarshalTrimParams(TrimParams{StartMS: 1000, EndMS: 4000}), 2003)
	if err != nil {
		t.Fatal(err)
	}
	claimOne(t, d, KindTrim, 2004)
	rf := ready()
	rf.RenditionKey, rf.RenditionETag = "r2", "re2"
	rf.PosterKey, rf.PosterETag = "p2", "pe2"
	rf.DurationMS = 3000
	superseded, ok, _, err := d.SwapRendition(ctx, tjob.ID, c.ID, rf, TrimParams{StartMS: 1000, EndMS: 4000}, 2005)
	if err != nil || !ok {
		t.Fatalf("SwapRendition: ok=%v err=%v", ok, err)
	}
	if !hasKey(superseded, f.Key) {
		t.Errorf("superseded = %v, want the filmstrip key %q", superseded, f.Key)
	}
	got, err := d.GetClip(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FilmstripKey != nil || got.FilmstripSize != nil || got.FilmstripETag != nil ||
		got.FilmstripTiles != nil || got.FilmstripTileW != nil || got.FilmstripTileH != nil ||
		got.FilmstripIntervalMS != nil {
		t.Fatalf("a trim kept the old filmstrip: %+v", got)
	}
	// The row no longer references the object, so `orphans` must not be able to
	// read it as unreferenced-but-live either way round: KeysOf is the single
	// definition both the report and `ls --stat` use.
	if hasKey(KeysOf(got).Keys(), f.Key) {
		t.Errorf("KeysOf still lists the dropped filmstrip: %v", KeysOf(got).Keys())
	}
}

func TestReplaceDropsTheFilmstrip(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	c := filmstripReady(t, d)
	f := filmstripFields()

	job, err := d.EnqueueFilmstrip(ctx, c.ID, 2000)
	if err != nil {
		t.Fatal(err)
	}
	claimOne(t, d, KindFilmstrip, 2001)
	if swap, err := d.SetFilmstrip(ctx, job.ID, c.ID, f, ready().RenditionETag, 2002); err != nil || !swap.OK {
		t.Fatalf("SetFilmstrip = %+v, %v", swap, err)
	}

	rjob, err := d.EnqueueReplace(ctx, c.ID, MarshalReplaceParams(ReplaceParams{
		SourceKey: "clips/x/original/source-r8r8r8r8.mp4", SourceFilename: "new.mp4", SourceSize: 456,
		SourceContentType: "video/mp4", SourceETag: "new-source-etag",
	}), 2003)
	if err != nil {
		t.Fatal(err)
	}
	claimOne(t, d, KindReplace, 2004)
	rf := ready()
	rf.RenditionKey, rf.RenditionETag = "r3", "re3"
	rf.PosterKey, rf.PosterETag = "p3", "pe3"
	src := NewSource{Key: "clips/x/original/source-r8r8r8r8.mp4", Filename: "new.mp4", Size: 456,
		ContentType: "video/mp4", ETag: "new-source-etag"}
	superseded, ok, err := d.SwapSource(ctx, rjob.ID, c.ID, rf, src, 2005)
	if err != nil || !ok {
		t.Fatalf("SwapSource: ok=%v err=%v", ok, err)
	}
	if !hasKey(superseded, f.Key) {
		t.Errorf("superseded = %v, want the filmstrip key %q", superseded, f.Key)
	}
	got, err := d.GetClip(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FilmstripKey != nil || got.FilmstripTiles != nil {
		t.Fatalf("a replacement kept the old filmstrip: %+v", got)
	}
}

// TestFilmstripKeyIsAccountedEverywhere pins the one thing a new object column
// has always got wrong in this project: the reports that walk keys. `orphans`
// once forgot social_key, and the fix was to make ONE list — so the filmstrip
// belongs to it, or a live sheet is reported as an orphan and `--delete` removes
// a file the clip is serving.
func TestFilmstripKeyIsAccountedEverywhere(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	c := filmstripReady(t, d)
	f := filmstripFields()
	job, err := d.EnqueueFilmstrip(ctx, c.ID, 2000)
	if err != nil {
		t.Fatal(err)
	}
	claimOne(t, d, KindFilmstrip, 2001)
	if swap, err := d.SetFilmstrip(ctx, job.ID, c.ID, f, ready().RenditionETag, 2002); err != nil || !swap.OK {
		t.Fatalf("SetFilmstrip = %+v, %v", swap, err)
	}
	got, err := d.GetClip(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !hasKey(KeysOf(got).Keys(), f.Key) {
		t.Errorf("KeysOf (admin ls --stat) does not list the filmstrip: %v", KeysOf(got).Keys())
	}
	rows, err := d.ListAllClipIDsAndKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.ID == c.ID && hasKey(r.Keys(), f.Key) {
			found = true
		}
	}
	if !found {
		t.Errorf("ListAllClipIDsAndKeys (admin orphans) does not list the filmstrip: %+v", rows)
	}
	// …and its bytes are part of what the instance says it is storing, or the
	// storage budget under-reports every clip whose scrubber has been opened.
	used, err := d.StorageUsed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := got.SourceSize + *got.RenditionSize + *got.PosterSize + f.Size; used != want {
		t.Errorf("StorageUsed = %d, want %d (source + rendition + poster + filmstrip)", used, want)
	}
	// …and of what the OWNER is told they are storing, which is the OTHER sum
	// this bundle widened and the one nothing was asserting (N113).
	st, err := d.AccountStatsOf(ctx, got.OwnerID, 2003)
	if err != nil {
		t.Fatal(err)
	}
	if want := *got.RenditionSize + *got.PosterSize + f.Size; st.BytesRenditions != want {
		t.Errorf("AccountStatsOf.BytesRenditions = %d, want %d (rendition + poster + filmstrip)",
			st.BytesRenditions, want)
	}
	if st.BytesSource != got.SourceSize {
		t.Errorf("AccountStatsOf.BytesSource = %d, want %d", st.BytesSource, got.SourceSize)
	}
}

// TestFilmstripCapsAreItsOwn: a sheet's CRASH budget is this package's constant
// and its ENVIRONMENT budget is the operator's, which is §20 erratum 89 applied
// to one more kind — a storage blip must not permanently cost a clip its
// filmstrip, and a pass that keeps crashing must not be retried for ever.
func TestFilmstripCapsAreItsOwn(t *testing.T) {
	lim := Limits{MaxAttempts: 9, MaxTransient: 7}
	if got := Cap(KindFilmstrip, FailCrash, lim); got != FilmstripMaxAttempts {
		t.Errorf("crash cap = %d, want FilmstripMaxAttempts (%d)", got, FilmstripMaxAttempts)
	}
	if got := Cap(KindFilmstrip, FailTransient, lim); got != 7 {
		t.Errorf("environment cap = %d, want the instance's 7", got)
	}
	if !hasKey(MediaKinds(), KindFilmstrip) {
		t.Errorf("MediaKinds() = %v, want the filmstrip on the media loop", MediaKinds())
	}
}

// TestMigrate0014UpgradesAPopulatedDatabase is the upgrade path a running
// install actually takes: a database that has never seen 0014 — with rows in it
// — gains the seven columns, every existing clip reads back with them NULL, and
// the feature works on that upgraded row.
func TestMigrate0014UpgradesAPopulatedDatabase(t *testing.T) {
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
	// Everything EXCEPT 0014, found by VERSION rather than by position: a later
	// migration lands in that directory every time a feature needs a table or a
	// column, and "0014 is the last file" was a fact about the tree on the day
	// this was written, not about the upgrade it is checking (§20 errata 252
	// and 255 — 0015 and 0016 both landed the week it was corrected; the same
	// correction sweepfail_test.go had already made for 0013).
	const filmstripVersion = 14
	skipped := 0
	for _, m := range files {
		if m.version == filmstripVersion {
			skipped++
			continue
		}
		if err := d.applyMigration(ctx, m); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}
	if skipped != 1 {
		t.Fatalf("skipped %d migrations, want exactly 00%d (of %d embedded files)", skipped, filmstripVersion, len(files))
	}
	// A ready clip written the way an installation on 0001-0013 holds one. The
	// Go API cannot write it: every read scans the new clipCols, which is itself
	// the proof below that this migration is what adds them.
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
  source_duration_ms, created_at, updated_at, ready_at)
VALUES (?, 'u1', 'old clip', '', 0, 'ready', 'clips/x/original/source.mp4', 'clip.mp4', 123, 'video/mp4', 'se',
  'r', 10, 're', 'p', 5, 'pe', 1280, 720, 6000, 1280, 720, 30, 1, 1, 6000, 1000, 1000, 1001)`,
		clipID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetClip(ctx, clipID); err == nil {
		t.Fatal("GetClip succeeded before 0014; the filmstrip columns are not in clipCols")
	}

	applySkipped(t, d, 14)
	got, err := d.GetClip(ctx, clipID)
	if err != nil {
		t.Fatalf("GetClip after the upgrade: %v", err)
	}
	if got.FilmstripKey != nil || got.FilmstripSize != nil || got.FilmstripETag != nil ||
		got.FilmstripTiles != nil || got.FilmstripTileW != nil || got.FilmstripTileH != nil ||
		got.FilmstripIntervalMS != nil {
		t.Errorf("an upgraded row has a filmstrip: %+v", got)
	}
	if got.State != ClipReady || *got.RenditionKey != "r" || got.Views != 0 {
		t.Errorf("the upgrade disturbed the row: %+v", got)
	}
	job, err := d.EnqueueFilmstrip(ctx, clipID, 1002)
	if err != nil {
		t.Fatalf("EnqueueFilmstrip on an upgraded row: %v", err)
	}
	if _, _, ok, err := d.ClaimMediaJob(ctx, 1003); err != nil || !ok {
		t.Fatalf("ClaimMediaJob: ok=%v err=%v", ok, err)
	}
	swap, err := d.SetFilmstrip(ctx, job.ID, clipID, filmstripFields(), "re", 1004)
	if err != nil || !swap.OK {
		t.Fatalf("SetFilmstrip on an upgraded row = %+v, %v", swap, err)
	}
}
