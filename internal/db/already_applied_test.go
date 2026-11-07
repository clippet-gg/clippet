package db

import (
	"context"
	"testing"
)

// The deterministic reproduction of the §20 erratum 102 defect. Both
// finalize and swap retry on `err != nil`, and both used to read `ok == false`
// as "the clip was tombstoned" — the state whose remedy is to DELETE this
// attempt's objects. But `ok == false` is also what the SECOND call returns
// when the FIRST one landed, which is exactly the state a retry loop reaches
// when a commit succeeds and is still reported as an error. The consequence
// was a `ready` row pointing at objects the run had just deleted.
//
// These tests need no fault injection: calling the function twice with the
// same arguments IS the second commit, and it is the only thing the caller
// can observe about it.

func TestFinalizeTranscodeTellsItsOwnSecondCommitFromALostRace(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "a")
	mkClip(t, d, u, 1, nil, false)
	j, c := claim(t, d, 10)

	ok, mine, err := d.FinalizeTranscode(ctx, j.ID, c.ID, ready(), false, 30)
	if err != nil || !ok || mine {
		t.Fatalf("first finalize: ok=%v alreadyApplied=%v err=%v; want true,false,nil", ok, mine, err)
	}
	// The retry. It changes nothing (ok=false) but the row holds THIS
	// attempt's keys, so the caller must keep its objects.
	ok, mine, err = d.FinalizeTranscode(ctx, j.ID, c.ID, ready(), false, 31)
	if err != nil || ok || !mine {
		t.Fatalf("second finalize: ok=%v alreadyApplied=%v err=%v; want false,true,nil", ok, mine, err)
	}
	gc, err := d.GetClip(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gc.State != ClipReady || gc.RenditionKey == nil || *gc.RenditionKey != ready().RenditionKey {
		t.Fatalf("the row must still reference this attempt's rendition: %+v", gc)
	}
	// …and the second call did not stamp the row again.
	if gc.ReadyAt == nil || *gc.ReadyAt != 30 {
		t.Fatalf("ready_at = %v, want the FIRST commit's 30", gc.ReadyAt)
	}
}

// A genuine lost race is still a lost race: the tombstone answers
// (false, false), which is what makes the caller delete its outputs.
func TestFinalizeTranscodeStillReportsATombstone(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "a")
	mkClip(t, d, u, 1, nil, false)
	j, c := claim(t, d, 10)
	if err := d.MarkDeleting(ctx, c.ID, 20); err != nil {
		t.Fatal(err)
	}
	ok, mine, err := d.FinalizeTranscode(ctx, j.ID, c.ID, ready(), false, 30)
	if err != nil || ok || mine {
		t.Fatalf("finalize on a tombstone: ok=%v alreadyApplied=%v err=%v; want false,false,nil", ok, mine, err)
	}
}

// A row holding SOMEBODY ELSE's keys is not this attempt's commit. The keys
// are attempt-scoped, so a clip re-transcoded by a later attempt reads as a
// lost race — correctly: this attempt's objects are unreferenced and must go.
func TestFinalizeTranscodeOtherKeysAreNotThisAttempt(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "a")
	mkClip(t, d, u, 1, nil, false)
	j, c := claim(t, d, 10)
	if ok, _, err := d.FinalizeTranscode(ctx, j.ID, c.ID, ready(), false, 30); err != nil || !ok {
		t.Fatalf("first finalize: %v %v", ok, err)
	}
	other := ready()
	other.RenditionKey, other.PosterKey = "clips/x/other/rendition.mp4", "clips/x/other/poster.jpg"
	ok, mine, err := d.FinalizeTranscode(ctx, j.ID, c.ID, other, false, 31)
	if err != nil || ok || mine {
		t.Fatalf("finalize with foreign keys: ok=%v alreadyApplied=%v err=%v; want false,false,nil", ok, mine, err)
	}
}

func TestSwapRenditionTellsItsOwnSecondCommitFromALostRace(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, _ := readyClip(t, d)
	job := runningTrim(t, d, c.ID, 2000)

	f := swapFields()
	superseded, ok, mine, err := d.SwapRendition(ctx, job.ID, c.ID, f, TrimParams{StartMS: 1000, EndMS: 3000}, 2002)
	if err != nil || !ok || mine {
		t.Fatalf("first swap: ok=%v alreadyApplied=%v err=%v", ok, mine, err)
	}
	if len(superseded) == 0 {
		t.Fatal("the first swap must report the keys it superseded")
	}
	// The retry. The job is `done` by now, so the JOB guard turns this call
	// away before the clip guard is ever reached — which is precisely why the
	// check has to run on that path too.
	superseded, ok, mine, err = d.SwapRendition(ctx, job.ID, c.ID, f, TrimParams{StartMS: 1000, EndMS: 3000}, 2003)
	if err != nil || ok || !mine {
		t.Fatalf("second swap: ok=%v alreadyApplied=%v err=%v; want false,true,nil", ok, mine, err)
	}
	if superseded != nil {
		t.Fatalf("the second swap reported superseded keys: %v", superseded)
	}
	gc, err := d.GetClip(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gc.RenditionKey == nil || *gc.RenditionKey != f.RenditionKey || gc.PosterKey == nil || *gc.PosterKey != f.PosterKey {
		t.Fatalf("the row must still reference this attempt's objects: %+v", gc)
	}
}

// A trim that lost its clip is still a lost race: nothing references this
// attempt's objects, so (false, false) is the honest answer.
func TestSwapRenditionStillReportsATombstone(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, _ := readyClip(t, d)
	job := runningTrim(t, d, c.ID, 2000)
	if err := d.MarkDeleting(ctx, c.ID, 2001); err != nil {
		t.Fatal(err)
	}
	_, ok, mine, err := d.SwapRendition(ctx, job.ID, c.ID, swapFields(), TrimParams{StartMS: 1000, EndMS: 3000}, 2002)
	if err != nil || ok || mine {
		t.Fatalf("swap on a tombstone: ok=%v alreadyApplied=%v err=%v; want false,false,nil", ok, mine, err)
	}
}

// runningTrim enqueues a trim for clipID and claims it, so the swap under
// test starts from the only state SwapRendition accepts.
func runningTrim(t *testing.T, d *DB, clipID string, now int64) Job {
	t.Helper()
	ctx := context.Background()
	job, err := d.EnqueueTrim(ctx, clipID, MarshalTrimParams(TrimParams{StartMS: 1000, EndMS: 3000}), now)
	if err != nil {
		t.Fatalf("EnqueueTrim: %v", err)
	}
	if _, _, ok, err := d.ClaimMediaJob(ctx, now+1); err != nil || !ok {
		t.Fatalf("ClaimMediaJob: ok=%v err=%v", ok, err)
	}
	return job
}

// The same defect, in the two commit shapes the quality tiers added (§20 erratum
// 201, amended). Both leave `rendition_key` NULL on purpose — the remux fast path
// publishes no second object, and a card-first commit publishes the LIGHT copy and
// queues the Full behind it — so a classifier that required a rendition key to
// match read BOTH of them as a lost race. The caller's answer to a lost race is to
// delete the objects it uploaded, which here are the poster and the card copy the
// just-committed `ready` row points at: erratum 102's own defect, on a clip that
// is live and shareable.
func TestFinalizeTranscodeKnowsItsOwnCommitWithNoRenditionKey(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields func() ReadyFields
	}{
		{"the remux fast path (no rendition object at all)", func() ReadyFields {
			f := ready()
			f.RenditionKey, f.RenditionSize, f.RenditionETag = "", 0, ""
			f.Tiers = TierFields{OriginalPlayable: true, OriginalWidth: 1280, OriginalHeight: 720,
				OriginalFPSNum: 30, OriginalFPSDen: 1, OriginalCodec: "h264",
				FullState: TierSkipped, LightState: TierSkipped}
			return f
		}},
		{"card-first (the light copy is published, the Full is queued)", func() ReadyFields {
			f := ready()
			f.RenditionKey, f.RenditionSize, f.RenditionETag = "", 0, ""
			f.Light = SocialFields{Key: "lk", Size: 20, ETag: "le", Width: 1280, Height: 720, FPSNum: 30, FPSDen: 1}
			f.Tiers = TierFields{OriginalWidth: 2560, OriginalHeight: 1440, OriginalFPSNum: 60, OriginalFPSDen: 1,
				OriginalCodec: "hevc", FullState: TierEncoding, LightState: TierReady, QueueFull: true}
			return f
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			d := openTest(t)
			u := mkUser(t, d, "a")
			mkClip(t, d, u, 1, nil, false)
			j, c := claim(t, d, 10)
			f := tc.fields()

			ok, mine, err := d.FinalizeTranscode(ctx, j.ID, c.ID, f, false, 30)
			if err != nil || !ok || mine {
				t.Fatalf("first finalize: ok=%v alreadyApplied=%v err=%v; want true,false,nil", ok, mine, err)
			}
			ok, mine, err = d.FinalizeTranscode(ctx, j.ID, c.ID, f, false, 31)
			if err != nil || ok || !mine {
				t.Fatalf("second finalize: ok=%v alreadyApplied=%v err=%v; want false,true,nil "+
					"(the caller must KEEP the objects the ready row points at)", ok, mine, err)
			}
			gc, err := d.GetClip(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gc.State != ClipReady || gc.PosterKey == nil || *gc.PosterKey != f.PosterKey {
				t.Fatalf("the row must still be ready on this attempt's poster: %+v", gc)
			}
			if gc.RenditionKey != nil {
				t.Fatalf("rendition_key = %q, want NULL on this shape", *gc.RenditionKey)
			}
		})
	}
}

// …and the classifier is still not fooled by a row it did not write: a foreign
// POSTER key is a genuine lost race whatever the rendition column says, because
// the poster key is attempt-scoped and every ready commit writes one.
func TestFinalizeTranscodeForeignPosterIsALostRaceWithNoRendition(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "a")
	mkClip(t, d, u, 1, nil, false)
	j, c := claim(t, d, 10)
	f := ready()
	f.RenditionKey, f.RenditionSize, f.RenditionETag = "", 0, ""
	f.Tiers = TierFields{OriginalPlayable: true, FullState: TierSkipped, LightState: TierSkipped}
	if ok, _, err := d.FinalizeTranscode(ctx, j.ID, c.ID, f, false, 30); err != nil || !ok {
		t.Fatalf("first finalize: %v %v", ok, err)
	}
	other := f
	other.PosterKey = "clips/x/other/poster.jpg"
	ok, mine, err := d.FinalizeTranscode(ctx, j.ID, c.ID, other, false, 31)
	if err != nil || ok || mine {
		t.Fatalf("finalize with a foreign poster: ok=%v alreadyApplied=%v err=%v; want false,false,nil", ok, mine, err)
	}
}
