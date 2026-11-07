package db

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
)

// newSource is the replacement original a swap points a clip at.
func newSource() NewSource {
	return NewSource{Key: "clips/x/original/source-r8r8r8r8.mov", Filename: "better.mov",
		Size: 4242, ContentType: "video/quicktime", ETag: "new-source-etag"}
}

// replaceFields is the measured result of transcoding that file: different
// keys, different dimensions, a different length.
func replaceFields() ReadyFields {
	f := ready()
	f.RenditionKey, f.RenditionETag, f.RenditionSize = "r-new", "re-new", 99
	f.PosterKey, f.PosterETag, f.PosterSize = "p-new", "pe-new", 9
	f.Width, f.Height, f.DurationMS = 640, 360, 12000
	src := int64(12000)
	f.SourceDurationMS = &src
	return f
}

func TestReplaceParamsRoundTrip(t *testing.T) {
	p := ReplaceParams{SourceKey: "k", SourceFilename: "f.mp4", SourceSize: 10,
		SourceContentType: "video/mp4", SourceETag: "e", Spool: ".upload-abc-r8"}
	got, err := ParseReplaceParams(MarshalReplaceParams(p))
	if err != nil || got != p {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	// A row that cannot describe a replacement must not run one: an empty key
	// or ETag, a zero size or a missing spool would each make the worker
	// publish something nobody uploaded.
	for _, bad := range []string{
		"", "nope", "{}",
		`{"source_key":"","source_etag":"e","source_size":1,"spool":"s"}`,
		`{"source_key":"k","source_etag":"","source_size":1,"spool":"s"}`,
		`{"source_key":"k","source_etag":"e","source_size":0,"spool":"s"}`,
		`{"source_key":"k","source_etag":"e","source_size":-1,"spool":"s"}`,
		`{"source_key":"k","source_etag":"e","source_size":1,"spool":""}`,
	} {
		if _, err := ParseReplaceParams(bad); err == nil {
			t.Errorf("ParseReplaceParams(%q) = nil error", bad)
		}
	}
}

func TestEnqueueReplaceAdmitsOneAtATime(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, _ := readyClip(t, d)
	params := MarshalReplaceParams(ReplaceParams{SourceKey: "k", SourceETag: "e", SourceSize: 1, Spool: ".upload-x-1"})

	job, err := d.EnqueueReplace(ctx, c.ID, params, 2000)
	if err != nil {
		t.Fatalf("EnqueueReplace: %v", err)
	}
	if job.Kind != KindReplace || job.State != JobQueued || job.Stage != JobStageQueued || job.ParamsJSON != params {
		t.Fatalf("queued job = %+v", job)
	}
	if _, err := d.EnqueueReplace(ctx, c.ID, params, 2001); !errors.Is(err, ErrConflict) {
		t.Errorf("second EnqueueReplace = %v, want ErrConflict", err)
	}
	if _, _, _, err := d.ClaimMediaJob(ctx, 2002); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnqueueReplace(ctx, c.ID, params, 2003); !errors.Is(err, ErrConflict) {
		t.Errorf("EnqueueReplace while running = %v, want ErrConflict", err)
	}
	// A finished one does not block the next.
	if _, _, err := d.SwapSource(ctx, job.ID, c.ID, replaceFields(), newSource(), 2004); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnqueueReplace(ctx, c.ID, params, 2005); err != nil {
		t.Errorf("EnqueueReplace after the first finished: %v", err)
	}
}

// TestEnqueueReplaceIsSerialisedUnderConcurrency: the 409 the API promises,
// proven where it is decided — one transaction on the single-writer pool.
func TestEnqueueReplaceIsSerialisedUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, _ := readyClip(t, d)
	params := MarshalReplaceParams(ReplaceParams{SourceKey: "k", SourceETag: "e", SourceSize: 1, Spool: ".upload-x-1"})

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	won, conflicts := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := d.EnqueueReplace(ctx, c.ID, params, 2000)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, ErrConflict):
				conflicts++
			default:
				t.Errorf("EnqueueReplace: %v", err)
			}
		}()
	}
	wg.Wait()
	if won != 1 || conflicts != n-1 {
		t.Fatalf("%d winners and %d conflicts out of %d; want exactly one winner", won, conflicts, n)
	}
}

func TestEnqueueReplaceRequiresAReadyClip(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	owner := mkUser(t, d, "queued-owner")
	c, _ := mkClip(t, d, owner, 1000, nil, false) // still `queued`
	params := MarshalReplaceParams(ReplaceParams{SourceKey: "k", SourceETag: "e", SourceSize: 1, Spool: ".upload-x-1"})
	if _, err := d.EnqueueReplace(ctx, c.ID, params, 1001); !errors.Is(err, ErrStateMismatch) {
		t.Errorf("EnqueueReplace on a queued clip = %v, want ErrStateMismatch", err)
	}
	if _, err := d.EnqueueReplace(ctx, "nosuchclipid", params, 1001); !errors.Is(err, ErrNotFound) {
		t.Errorf("EnqueueReplace on an unknown clip = %v, want ErrNotFound", err)
	}
}

// TestSwapSourceIsOneCommit is the whole contract of the commit: what moves,
// what is kept, what is reset, and which keys the caller must then delete —
// the PREVIOUS ORIGINAL among them, which is what makes "upload is sacred"
// an ordering rather than an exception.
func TestSwapSourceIsOneCommit(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, _ := readyClip(t, d)
	// Give the clip everything a replacement must reset: a trim window, a GIF
	// and a social rendition, plus a view and a tag it must KEEP.
	if _, _, _, err := d.SwapRendition(ctx, mustEnqueueTrim(t, d, c.ID, 2000), c.ID, trimmedFields(), TrimParams{StartMS: 1000, EndMS: 3000}, 2001); err != nil {
		t.Fatal(err)
	}
	if err := d.SetClipTags(ctx, c.ID, []string{"gaming"}); err != nil {
		t.Fatal(err)
	}
	if err := d.IncrementViews(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	gifJob, err := d.EnqueueGIF(ctx, c.ID, 2002)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := d.ClaimMediaJob(ctx, 2002); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SetGIF(ctx, gifJob.ID, c.ID, GIFFields{Key: "g", Size: 3, ETag: "ge", Width: 320, Height: 180},
		"re2", 2002); err != nil {
		t.Fatal(err)
	}
	socialJob, err := d.EnqueueSocial(ctx, c.ID, 2002)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := d.ClaimMediaJob(ctx, 2002); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SetSocial(ctx, socialJob.ID, c.ID, SocialFields{Key: "s", Size: 4, ETag: "se", Width: 640, Height: 360},
		"re2", 2002); err != nil {
		t.Fatal(err)
	}
	// …and a CHOSEN card frame, which is a moment on the OLD original's
	// timeline and therefore cannot survive a new file being put underneath
	// the clip (§20 errata 127, 130).
	posterJob, err := d.EnqueuePoster(ctx, c.ID, MarshalPosterParams(PosterParams{Seconds: 1.5}), 2002)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := d.ClaimMediaJob(ctx, 2002); err != nil {
		t.Fatal(err)
	}
	if sw, err := d.SetPoster(ctx, posterJob.ID, c.ID,
		PosterFields{Key: "chosen-p", Size: 5, ETag: "pe2", Width: 640, Height: 360, Seconds: 1.5},
		"re2", 2002); err != nil || !sw.OK {
		t.Fatalf("SetPoster = %+v, %v", sw, err)
	}
	before, err := d.GetClip(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}

	job, err := d.EnqueueReplace(ctx, c.ID, MarshalReplaceParams(ReplaceParams{
		SourceKey: newSource().Key, SourceETag: newSource().ETag, SourceSize: newSource().Size, Spool: ".upload-x-1"}), 2003)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := d.ClaimMediaJob(ctx, 2004); err != nil {
		t.Fatal(err)
	}
	// Claiming a replacement must not move the clip: only `transcode` does.
	if got, _ := d.GetClip(ctx, c.ID); got.State != ClipReady {
		t.Fatalf("claiming a replacement moved the clip to %q", got.State)
	}

	superseded, ok, err := d.SwapSource(ctx, job.ID, c.ID, replaceFields(), newSource(), 2005)
	if err != nil || !ok {
		t.Fatalf("SwapSource = %v, %v", ok, err)
	}
	got, err := d.GetClip(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The source moved, in all five columns.
	src := newSource()
	if got.SourceKey != src.Key || got.SourceFilename != src.Filename || got.SourceSize != src.Size ||
		got.SourceContentType != src.ContentType || got.SourceETag != src.ETag {
		t.Fatalf("source = %+v, want %+v", got, src)
	}
	// The rendition and the measurements moved with it.
	if *got.RenditionKey != "r-new" || *got.PosterKey != "p-new" || *got.DurationMS != 12000 ||
		*got.Width != 640 || *got.Height != 360 {
		t.Fatalf("rendition/measurements not swapped: %+v", got)
	}
	// The trim window is RESET, and source_duration_ms is the new file's own
	// (assigned, not COALESCEd: it describes a different file now).
	if got.TrimStartMS != nil || got.TrimEndMS != nil {
		t.Errorf("trim window survived the replacement: %v..%v", got.TrimStartMS, got.TrimEndMS)
	}
	// The chosen card frame is reset with it, and for the same reason: it was
	// a moment on a timeline this clip no longer has. Leaving it behind would
	// point the share page's "chosen frame" — and the preview's opening frame
	// (erratum 131) — at an arbitrary instant of the NEW video.
	if got.PosterSeconds != nil {
		t.Errorf("poster_seconds = %v survived the replacement; the frame it names is in the old original", *got.PosterSeconds)
	}
	if got.SourceDurationMS == nil || *got.SourceDurationMS != 12000 {
		t.Errorf("source_duration_ms = %v, want the NEW original's 12000", got.SourceDurationMS)
	}
	// The GIF and the social rendition are gone: both described a rendition
	// that no longer exists.
	if got.GIFKey != nil || got.SocialKey != nil {
		t.Errorf("gif/social survived the replacement: %v %v", got.GIFKey, got.SocialKey)
	}
	// Everything that is the CLIP rather than the file is kept.
	switch {
	case got.State != ClipReady:
		t.Errorf("state = %q, want ready throughout", got.State)
	case got.Title != before.Title || got.Description != before.Description || got.Unlisted != before.Unlisted:
		t.Errorf("metadata changed")
	case got.Views != before.Views:
		t.Errorf("views = %d, want %d", got.Views, before.Views)
	case got.CreatedAt != before.CreatedAt:
		t.Errorf("created_at moved")
	case !equalPtr(got.ReadyAt, before.ReadyAt):
		t.Errorf("ready_at moved: the clip has been ready the whole time")
	case !equalPtr(got.ExpiresAt, before.ExpiresAt) || !equalPtr(got.TTLSeconds, before.TTLSeconds):
		t.Errorf("the lifetime changed")
	case got.UpdatedAt != 2005:
		t.Errorf("updated_at = %d, want the swap's clock (the media routes date from it)", got.UpdatedAt)
	}
	tags, err := d.TagsOf(ctx, c.ID)
	if err != nil || !slices.Equal(tags, []string{"gaming"}) {
		t.Errorf("tags = %v, %v; a replacement must keep them", tags, err)
	}
	// The superseded list: the PREVIOUS ORIGINAL first, then everything the
	// row stopped referencing.
	want := []string{before.SourceKey, *before.RenditionKey, *before.PosterKey, "g", "s"}
	if !slices.Equal(superseded, want) {
		t.Fatalf("superseded = %v, want %v", superseded, want)
	}
	// The job is done in the same commit.
	j, err := getJob(ctx, d.rw, job.ID)
	if err != nil || j.State != JobDone || j.Stage != "done" {
		t.Fatalf("job = %+v, %v", j, err)
	}
}

// TestSwapSourceLosesTheRaceQuietly: a clip that stopped being ready, or a job
// that stopped being running, changes NOTHING — the caller then removes its
// own objects, the uploaded original included.
func TestSwapSourceLosesTheRaceQuietly(t *testing.T) {
	ctx := context.Background()

	t.Run("the clip was tombstoned", func(t *testing.T) {
		d := openTest(t)
		c, _ := readyClip(t, d)
		job, err := d.EnqueueReplace(ctx, c.ID, MarshalReplaceParams(ReplaceParams{
			SourceKey: "k", SourceETag: "e", SourceSize: 1, Spool: ".upload-x-1"}), 2000)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := d.ClaimMediaJob(ctx, 2001); err != nil {
			t.Fatal(err)
		}
		if err := d.MarkDeleting(ctx, c.ID, 2002); err != nil {
			t.Fatal(err)
		}
		superseded, ok, err := d.SwapSource(ctx, job.ID, c.ID, replaceFields(), newSource(), 2003)
		if err != nil || ok || superseded != nil {
			t.Fatalf("SwapSource on a tombstoned clip = %v %v %v", superseded, ok, err)
		}
	})

	t.Run("a transcode job is refused outright", func(t *testing.T) {
		d := openTest(t)
		c, j := readyClip(t, d)
		_, ok, err := d.SwapSource(ctx, j.ID, c.ID, replaceFields(), newSource(), 2100)
		if !errors.Is(err, ErrStateMismatch) || ok {
			t.Fatalf("SwapSource with a transcode job = %v %v, want ErrStateMismatch", ok, err)
		}
		// FinalizeTranscode stays the only writer of clips.state (§5).
		got, _ := d.GetClip(ctx, c.ID)
		if got.SourceKey == newSource().Key {
			t.Fatal("a transcode job swapped the source")
		}
	})
}

// TestReplaceCapsSplitTheirTwoCounters: the crash cap is this kind's own
// constant, the environment cap is the instance's (§20 erratum 89's rule,
// applied to the fourth media kind).
func TestReplaceCapsSplitTheirTwoCounters(t *testing.T) {
	lim := Limits{MaxAttempts: 9, MaxTransient: 7}
	if got := Cap(KindReplace, FailCrash, lim); got != ReplaceMaxAttempts {
		t.Errorf("crash cap = %d, want %d", got, ReplaceMaxAttempts)
	}
	if got := Cap(KindReplace, FailPermanent, lim); got != ReplaceMaxAttempts {
		t.Errorf("permanent cap = %d, want %d", got, ReplaceMaxAttempts)
	}
	if got := Cap(KindReplace, FailTransient, lim); got != 7 {
		t.Errorf("transient cap = %d, want the instance's 7", got)
	}
	// And the media loop claims it: a replacement runs ffmpeg against a ready
	// clip, so it belongs on that loop and NOT on the notify one.
	if !slices.Contains(MediaKinds(), KindReplace) {
		t.Errorf("MediaKinds() = %v, want it to carry %q", MediaKinds(), KindReplace)
	}
}

// mustEnqueueTrim queues a trim, claims it and returns its id, so a test can
// give a clip a real trim window before replacing it.
func mustEnqueueTrim(t *testing.T, d *DB, clipID string, now int64) int64 {
	t.Helper()
	ctx := context.Background()
	j, err := d.EnqueueTrim(ctx, clipID, MarshalTrimParams(TrimParams{StartMS: 1000, EndMS: 3000}), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := d.ClaimMediaJob(ctx, now); err != nil {
		t.Fatal(err)
	}
	return j.ID
}

// trimmedFields is a cut rendition: new keys so the swap has old ones to
// report.
func trimmedFields() ReadyFields {
	f := ready()
	f.RenditionKey, f.RenditionETag = "r2", "re2"
	f.PosterKey, f.PosterETag = "p2", "pe2"
	f.DurationMS = 2000
	return f
}

func equalPtr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
