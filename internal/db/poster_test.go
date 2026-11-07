package db

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"sync"
	"testing"
)

func posterFields(seconds float64) PosterFields {
	return PosterFields{Key: "clips/x/poster/poster-p8p8p8p8.jpg", Size: 2048, ETag: "chosen-etag",
		Width: 1280, Height: 720, Seconds: seconds}
}

func TestPosterParamsRoundTrip(t *testing.T) {
	s := MarshalPosterParams(PosterParams{Seconds: 2.5})
	if s != `{"seconds":2.5}` {
		t.Errorf("params column = %s", s)
	}
	p, err := ParsePosterParams(s)
	if err != nil || p.Seconds != 2.5 {
		t.Fatalf("ParsePosterParams = %+v, %v", p, err)
	}
	// Zero is the first frame and a legal choice; everything below is a row
	// no attempt could honour, so it is an explicit error rather than a
	// silent seek to the start — which would publish a frame nobody chose.
	if p, err := ParsePosterParams(`{"seconds":0}`); err != nil || p.Seconds != 0 {
		t.Errorf("zero seconds = %+v, %v; the first frame is a choice", p, err)
	}
	for _, bad := range []string{"", "{", `{"seconds":-1}`, `{"seconds":"2"}`} {
		if _, err := ParsePosterParams(bad); err == nil {
			t.Errorf("ParsePosterParams(%q) accepted an unusable row", bad)
		}
	}
	// JSON cannot carry NaN or ±Inf, so Marshal FAILS on one — and what it
	// writes then must not parse. `{}` would parse as zero, publishing the
	// first frame under the answer "your frame was used", which is the one
	// outcome this whole feature exists to rule out.
	for _, impossible := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := ParsePosterParams(MarshalPosterParams(PosterParams{Seconds: impossible})); err == nil {
			t.Errorf("a %v choice round-tripped into a runnable job", impossible)
		}
	}
}

// TestMigrate0008UpgradesAnOlderDatabase is the upgrade path an install
// actually takes: a database that has never seen 0008 — with rows in it —
// gains poster_seconds, every existing clip reads back with it NULL (nobody
// has chosen a frame), and the feature then works on that upgraded row.
func TestMigrate0008UpgradesAnOlderDatabase(t *testing.T) {
	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "clippet.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	files, err := migrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	// Everything BEFORE 0008, found by version rather than by position: a
	// later migration lands in this directory every time a feature needs a
	// column, and "0008 is the last file" was a fact about the tree on the day
	// it was written, not about the upgrade this test is checking.
	before := 0
	for _, m := range files {
		if m.version == 8 {
			break
		}
		before++
	}
	if before == len(files) {
		t.Fatalf("0008 is missing from the embedded migrations: %d files", len(files))
	}
	if _, err := d.rw.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	for _, m := range files[:before] {
		if err := d.applyMigration(ctx, m); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}
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
  duration_ms, width, height, fps_num, fps_den, has_audio, source_duration_ms,
  created_at, updated_at, ready_at)
VALUES (?, 'u1', 'old clip', '', 0, 'ready', 'clips/x/original/source.mp4', 'clip.mp4', 123, 'video/mp4', 'se',
  'r', 10, 're', 'p', 5, 'pe', 1280, 720, 6000, 1280, 720, 30, 1, 1, 6000, 1000, 1000, 1001)`, clipID); err != nil {
		t.Fatal(err)
	}
	// The Go API cannot read that row yet — every scan uses clipCols, which
	// now names poster_seconds. That failure IS the proof the column is new.
	if _, err := d.GetClip(ctx, clipID); err == nil {
		t.Fatal("GetClip succeeded before 0008; poster_seconds is not in clipCols")
	}

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate to 0008: %v", err)
	}
	c, err := d.GetClip(ctx, clipID)
	if err != nil {
		t.Fatalf("GetClip after the upgrade: %v", err)
	}
	if c.PosterSeconds != nil {
		t.Errorf("an upgraded row already has a chosen frame (%v); NULL means the automatic rule", *c.PosterSeconds)
	}
	if c.State != ClipReady || *c.PosterKey != "p" || c.Views != 0 {
		t.Errorf("the upgrade disturbed the row: %+v", c)
	}
	// And the feature works on it.
	job, err := d.EnqueuePoster(ctx, clipID, MarshalPosterParams(PosterParams{Seconds: 1.25}), 1002)
	if err != nil {
		t.Fatalf("EnqueuePoster on an upgraded row: %v", err)
	}
	if _, _, _, err := d.ClaimMediaJob(ctx, 1003); err != nil {
		t.Fatal(err)
	}
	swap, err := d.SetPoster(ctx, job.ID, clipID, posterFields(1.25), "re", 1004)
	if err != nil || !swap.OK {
		t.Fatalf("SetPoster on an upgraded row: %+v %v", swap, err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate again: %v", err)
	}
}

func TestEnqueuePosterAdmitsOneAtATime(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, _ := readyClip(t, d)

	job, err := d.EnqueuePoster(ctx, c.ID, MarshalPosterParams(PosterParams{Seconds: 1}), 2000)
	if err != nil {
		t.Fatalf("EnqueuePoster: %v", err)
	}
	if job.Kind != KindPoster || job.State != JobQueued || job.Stage != JobStageQueued || job.ParamsJSON != `{"seconds":1}` {
		t.Fatalf("queued job = %+v", job)
	}
	if _, err := d.EnqueuePoster(ctx, c.ID, MarshalPosterParams(PosterParams{Seconds: 2}), 2001); !errors.Is(err, ErrConflict) {
		t.Errorf("a second EnqueuePoster = %v, want ErrConflict", err)
	}
	if _, _, _, err := d.ClaimMediaJob(ctx, 2002); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnqueuePoster(ctx, c.ID, MarshalPosterParams(PosterParams{Seconds: 2}), 2003); !errors.Is(err, ErrConflict) {
		t.Errorf("EnqueuePoster while one runs = %v, want ErrConflict", err)
	}
	// A finished one does not block: an owner may change their mind.
	if _, err := d.SetPoster(ctx, job.ID, c.ID, posterFields(1), ready().RenditionETag, 2004); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnqueuePoster(ctx, c.ID, MarshalPosterParams(PosterParams{Seconds: 3}), 2005); err != nil {
		t.Errorf("EnqueuePoster after the first finished: %v", err)
	}
}

func TestEnqueuePosterNeedsAReadyClip(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	owner := mkUser(t, d, "posterowner")
	c, _ := mkClip(t, d, owner, 1000, nil, false)
	if _, err := d.EnqueuePoster(ctx, c.ID, "{}", 1001); !errors.Is(err, ErrStateMismatch) {
		t.Errorf("EnqueuePoster on a queued clip = %v, want ErrStateMismatch", err)
	}
	if _, err := d.EnqueuePoster(ctx, "aZ3kQ9xB2mLp", "{}", 1001); !errors.Is(err, ErrNotFound) {
		t.Errorf("EnqueuePoster on an unknown clip = %v, want ErrNotFound", err)
	}
}

// Two tabs, one job: the guard and the INSERT are one transaction on the
// single-writer pool, so eight simultaneous clicks cannot make eight jobs.
func TestEnqueuePosterIsSerialisedUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, _ := readyClip(t, d)

	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := d.EnqueuePoster(ctx, c.ID, MarshalPosterParams(PosterParams{Seconds: 1}), 3000)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrConflict):
				conflicts++
			default:
				t.Errorf("EnqueuePoster: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || conflicts != 7 {
		t.Fatalf("%d winners and %d conflicts, want exactly one of each kind", wins, conflicts)
	}
}

// TestSetPosterGuards is the commit's predicate table. Every false answer
// must change NOTHING and must say why.
func TestSetPosterGuards(t *testing.T) {
	ctx := context.Background()

	t.Run("the rendition changed under the job", func(t *testing.T) {
		d := openTest(t)
		c, _ := readyClip(t, d)
		job, err := d.EnqueuePoster(ctx, c.ID, MarshalPosterParams(PosterParams{Seconds: 1}), 2000)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := d.ClaimMediaJob(ctx, 2001); err != nil {
			t.Fatal(err)
		}
		swap, err := d.SetPoster(ctx, job.ID, c.ID, posterFields(1), "a-rendition-this-clip-never-had", 2002)
		if err != nil {
			t.Fatal(err)
		}
		if swap.OK || swap.Reason != "the rendition changed" {
			t.Fatalf("swap = %+v, want a refusal naming the rendition", swap)
		}
		after, err := d.GetClip(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if *after.PosterKey != ready().PosterKey || after.PosterSeconds != nil {
			t.Fatalf("a refused swap changed the clip: %+v", after)
		}
	})

	t.Run("the clip is no longer ready", func(t *testing.T) {
		d := openTest(t)
		c, _ := readyClip(t, d)
		job, err := d.EnqueuePoster(ctx, c.ID, MarshalPosterParams(PosterParams{Seconds: 1}), 2000)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := d.ClaimMediaJob(ctx, 2001); err != nil {
			t.Fatal(err)
		}
		if err := d.MarkDeleting(ctx, c.ID, 2002); err != nil {
			t.Fatal(err)
		}
		swap, err := d.SetPoster(ctx, job.ID, c.ID, posterFields(1), ready().RenditionETag, 2003)
		if err != nil {
			t.Fatal(err)
		}
		if swap.OK {
			t.Fatalf("a tombstoned clip was given a new card image: %+v", swap)
		}
	})

	t.Run("a transcode job is refused outright", func(t *testing.T) {
		d := openTest(t)
		c, j := readyClip(t, d)
		if _, err := d.SetPoster(ctx, j.ID, c.ID, posterFields(1), ready().RenditionETag, 2000); !errors.Is(err, ErrStateMismatch) {
			t.Errorf("SetPoster on a transcode job = %v, want ErrStateMismatch: only FinalizeTranscode may write that coupling", err)
		}
	})
}

// The happy path, and the two things it must and must not do: the poster
// columns and the choice are written together, and the CLIP's state, its
// rendition and its PREVIEW are untouched. A chosen frame removes nothing —
// unlike a trim, which replaces the frames the preview was cut from.
func TestSetPosterWritesTheChoiceAndLeavesEverythingElse(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, _ := readyClip(t, d)

	// Give the clip a preview first, so "the preview survives" is a real
	// assertion rather than a vacuous one.
	gjob, err := d.EnqueueGIF(ctx, c.ID, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := d.ClaimMediaJob(ctx, 1501); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SetGIF(ctx, gjob.ID, c.ID, gifFields(), ready().RenditionETag, 1502); err != nil {
		t.Fatal(err)
	}

	job, err := d.EnqueuePoster(ctx, c.ID, MarshalPosterParams(PosterParams{Seconds: 2.5}), 2000)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := d.ClaimMediaJob(ctx, 2001); err != nil {
		t.Fatal(err)
	}
	swap, err := d.SetPoster(ctx, job.ID, c.ID, posterFields(2.5), ready().RenditionETag, 2002)
	if err != nil || !swap.OK {
		t.Fatalf("SetPoster = %+v, %v", swap, err)
	}
	if len(swap.Superseded) != 1 || swap.Superseded[0] != ready().PosterKey {
		t.Errorf("superseded = %v, want the poster the clip stopped referencing", swap.Superseded)
	}
	after, err := d.GetClip(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.PosterSeconds == nil || *after.PosterSeconds != 2.5 {
		t.Fatalf("poster_seconds = %v, want the choice that was committed", after.PosterSeconds)
	}
	if *after.PosterKey != posterFields(2.5).Key || *after.PosterETag != "chosen-etag" || *after.PosterWidth != 1280 {
		t.Errorf("the poster columns were not swapped: %+v", after)
	}
	if after.State != ClipReady || after.FailureReason != nil {
		t.Fatalf("a poster job marked its clip %q/%v", after.State, after.FailureReason)
	}
	if *after.RenditionKey != ready().RenditionKey || *after.RenditionETag != ready().RenditionETag {
		t.Error("a poster job moved the rendition")
	}
	if after.GIFKey == nil || *after.GIFKey != gifFields().Key {
		t.Error("choosing a card image threw the preview away; the frames it was cut from are all still there")
	}
	if after.UpdatedAt != 2002 {
		t.Errorf("updated_at = %d; the derived routes date from it, so a cache must be told", after.UpdatedAt)
	}
	done, err := getJob(ctx, d.rw, job.ID)
	if err != nil || done.State != JobDone || done.Stage != "done" {
		t.Fatalf("the job did not resolve: %+v %v", done, err)
	}
}

// A trim CARRIES the choice when the cut still contains the moment, and
// clears it when it does not — one column, written by the same UPDATE that
// swaps the rendition (§20 erratum 130).
func TestSwapRenditionCarriesOrClearsTheChosenFrame(t *testing.T) {
	ctx := context.Background()

	swapWith := func(t *testing.T, pin *float64) Clip {
		t.Helper()
		d := openTest(t)
		c, _ := readyClip(t, d)
		// Choose a frame first, so the row really carries one.
		pjob, err := d.EnqueuePoster(ctx, c.ID, MarshalPosterParams(PosterParams{Seconds: 4}), 2000)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := d.ClaimMediaJob(ctx, 2001); err != nil {
			t.Fatal(err)
		}
		if _, err := d.SetPoster(ctx, pjob.ID, c.ID, posterFields(4), ready().RenditionETag, 2002); err != nil {
			t.Fatal(err)
		}
		tjob, err := d.EnqueueTrim(ctx, c.ID, MarshalTrimParams(TrimParams{StartMS: 3000, EndMS: 5000}), 3000)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := d.ClaimMediaJob(ctx, 3001); err != nil {
			t.Fatal(err)
		}
		f := ready()
		f.RenditionKey, f.RenditionETag, f.PosterKey, f.PosterETag = "r2", "re2", "p2", "pe2"
		f.DurationMS = 2000
		f.PosterSeconds = pin
		_, ok, _, err := d.SwapRendition(ctx, tjob.ID, c.ID, f, TrimParams{StartMS: 3000, EndMS: 5000}, 3002)
		if err != nil || !ok {
			t.Fatalf("SwapRendition: ok=%v err=%v", ok, err)
		}
		got, err := d.GetClip(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	kept := 4.0
	if got := swapWith(t, &kept); got.PosterSeconds == nil || *got.PosterSeconds != 4 {
		t.Errorf("poster_seconds = %v after a cut that CONTAINS 4 s; the choice is on the original's timeline and survives", got.PosterSeconds)
	}
	if got := swapWith(t, nil); got.PosterSeconds != nil {
		t.Errorf("poster_seconds = %v after a cut that removed the chosen moment; it must go back to the automatic frame", *got.PosterSeconds)
	}
}
