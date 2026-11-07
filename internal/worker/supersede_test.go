package worker

import (
	"context"
	"errors"
	"testing"

	"clippet/internal/db"
	"clippet/internal/store"
)

// TestTrimDuringARunningCardCopyQueuesAFreshOne is §20 erratum 237's open ending,
// closed as 237 (a), through the worker: a trim commits while a `social` job is
// RUNNING — the job has read the old rendition and is encoding it — and then
// that job reaches its commit. Before (a) the trim's commit read the running job
// as the one that would serve the new rendition and queued nothing; the job then
// lost its ETag race, its skip settled `light_state = skipped`, and the trimmed
// clip never got a card copy of its own. Now the trim's commit supersedes the
// running row and queues a fresh job; the stale run's every ending is "not this
// attempt's row"; the loop picks the fresh job up, publishes the copy of the CUT,
// and the card-ready hook (erratum 330) fires for it.
func TestTrimDuringARunningCardCopyQueuesAFreshOne(t *testing.T) {
	ctx := context.Background()
	h := socialByHandHarness(t)
	hook := withCardHook(h)
	c := h.gifReady()
	now := h.clock.Now().Unix()
	if err := h.db.SetTierState(ctx, c.ID, db.TierNameLight, db.TierEncoding, now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.EnqueueSocial(ctx, c.ID, now); err != nil {
		t.Fatal(err)
	}
	staleJob := h.claimKind(db.KindSocial)

	// The stale run gets as far as its commit against the rendition it read.
	stale := newSocialRun(h.w, ctx, &run{}, staleJob, c)
	if err := stale.recheckClip(); err != nil {
		t.Fatalf("recheckClip: %v", err)
	}
	if err := stale.acquire(); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := stale.encodeVerify(); err != nil {
		t.Fatalf("encodeVerify: %v", err)
	}
	if err := stale.store(); err != nil {
		t.Fatalf("store: %v", err)
	}

	// …and the trim commits underneath it, asking for a card copy of the cut.
	trim, err := h.db.EnqueueTrim(ctx, c.ID, db.MarshalTrimParams(db.TrimParams{StartMS: 1000, EndMS: 3000}), now)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.claimKind(db.KindTrim); got.ID != trim.ID {
		t.Fatalf("claimed %+v, want the trim", got)
	}
	rk := store.RenditionKey(h.cfg.S3.KeyPrefix, c.ID, "cutr8cut")
	pk := store.PosterKey(h.cfg.S3.KeyPrefix, c.ID, "cutr8cut")
	h.store.Put(rk, []byte(renditionPayload), "video/mp4")
	h.store.Put(pk, []byte("poster-bytes"), "image/jpeg")
	f := db.ReadyFields{
		RenditionKey: rk, RenditionSize: int64(len(renditionPayload)), RenditionETag: "the-cut-etag",
		PosterKey: pk, PosterSize: 12, PosterETag: "cut-poster-etag", PosterWidth: 640, PosterHeight: 360,
		Width: 1280, Height: 720, DurationMS: 2000, FPSNum: 30, FPSDen: 1, HasAudio: true,
		QueueSocial: true,
		Tiers:       db.TrimTierFields(true, 1280, 720, 30, 1, db.TierCodecH264, true),
	}
	if _, ok, _, err := h.db.SwapRendition(ctx, trim.ID, c.ID, f, db.TrimParams{StartMS: 1000, EndMS: 3000}, now); err != nil || !ok {
		t.Fatalf("SwapRendition = %v, %v", ok, err)
	}

	// The stale run's ending: its commit is refused and its row is not its own.
	if err := stale.commit(); !errors.Is(err, errLostRace) {
		t.Fatalf("the stale commit = %v, want the lost race", err)
	}
	stale.finish(errLostRace)
	if got := h.clip(c.ID); got.LightState != db.TierEncoding {
		t.Fatalf("light_state = %q after the stale run ended, want %q: a fresh card copy is coming", got.LightState, db.TierEncoding)
	}

	h.start()
	fresh := h.waitSocial(c.ID, jobDone)
	hook.wait(t, h, c.ID)
	h.waitIdle()
	if fresh.ID == staleJob.ID {
		t.Fatal("no fresh card-copy job was queued behind the trim")
	}
	if fresh.Stage != "done" {
		t.Fatalf("the fresh job resolved at stage %q, want done (a published copy)", fresh.Stage)
	}
	after := h.clip(c.ID)
	if after.LightState != db.TierReady || after.SocialKey == nil || *after.SocialKey == "" {
		t.Fatalf("after the fresh job: light_state=%q social=%v, want a published card copy of the cut", after.LightState, after.SocialKey)
	}
	if !db.CardReady(after) {
		t.Fatal("the trimmed clip's card is not ready")
	}
	old, err := h.db.LatestJobOfKind(ctx, c.ID, db.KindSocial)
	if err != nil || old.ID != fresh.ID {
		t.Fatalf("latest social job = %+v (%v), want the fresh one", old, err)
	}
	h.assertNoErrorLogs()
}
