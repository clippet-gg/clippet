package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"clippet/internal/db"
	"clippet/internal/transcode"
)

// bothTiersHarness is the §20 erratum 217 shape — a 1440p60 source no browser
// plays, so one ffmpeg writes the Full AND the card copy — which makes every
// tier column non-trivial: original_playable false, original_codec h264,
// full_state ready, light_state ready.
func bothTiersHarness(t *testing.T) *harness {
	t.Helper()
	p := defaultProbeResult()
	p.CodedW, p.CodedH, p.DisplayW, p.DisplayH = 2560, 1440, 2560, 1440
	p.FPS, p.RFPS = transcode.Rational{Num: 60, Den: 1}, transcode.Rational{Num: 60, Den: 1}
	h := tierHarness(t, p)
	h.runner.probeOutFn = func(_ context.Context, path string) (*transcode.OutputInfo, error) {
		if strings.Contains(path, "light-") {
			return &transcode.OutputInfo{Width: 1280, Height: 720, FPS: transcode.Rational{Num: 30, Den: 1},
				DurationMS: 6000, HasAudio: true, VideoCodec: "h264", AudioCodec: "aac", VideoStreams: 1, AudioStreams: 1}, nil
		}
		return &transcode.OutputInfo{Width: 2560, Height: 1440, FPS: transcode.Rational{Num: 60, Den: 1},
			DurationMS: 6000, HasAudio: true, VideoCodec: "h264", AudioCodec: "aac", VideoStreams: 1, AudioStreams: 1}, nil
	}
	return h
}

// TestCheckpointCarriesTheTierDecision is AUDIT #1 (§20 erratum 291). The
// checkpoint is what a finalize-only retry commits from, and until this pass
// `tierFields()` ran inside finalize — AFTER checkpoint() had marshalled
// j.ready — so every checkpoint carried the zero TierFields and a retry off one
// (a released job keeps its checkpoint) committed blank tier columns.
// TestTierFieldsSurviveAFinalizeOnlyRetry round-trips a hand-built struct and
// cannot see this; this test reads the checkpoint the RUN wrote, then commits a
// second clip from exactly that checkpoint and expects the same four columns.
func TestCheckpointCarriesTheTierDecision(t *testing.T) {
	h := bothTiersHarness(t)
	// Clip B is created FIRST and claimed by the test before the worker starts,
	// so the worker cannot take it: it is the row the second run will be made
	// from, once there is a checkpoint to hand it. ClaimJob takes the oldest
	// queued job, which is why B precedes A.
	b, bj := h.newClip("mkv", 4096, true)
	cj, _, ok, err := h.db.ClaimJob(context.Background(), h.clock.Now().Unix())
	if err != nil || !ok || cj.ID != bj.ID {
		t.Fatalf("ClaimJob: ok=%v err=%v", ok, err)
	}
	a, _ := h.newClip("mkv", 4096, true)
	h.start()
	first := h.waitClip(a.ID, db.ClipReady)
	h.waitIdle()
	if first.OriginalPlayable || first.OriginalCodec != "h264" || first.FullState != db.TierReady || first.LightState != db.TierReady {
		t.Fatalf("the first run's columns: playable %v codec %q full %q light %q, want false/h264/ready/ready",
			first.OriginalPlayable, first.OriginalCodec, first.FullState, first.LightState)
	}

	// The checkpoint the run wrote (the row is `done`; the checkpoint stays).
	job := h.jobOf(a.ID)
	if job.State != db.JobDone || job.CheckpointJSON == nil {
		t.Fatalf("job after the run = state %s checkpoint %v, want done with its checkpoint kept", job.State, job.CheckpointJSON != nil)
	}
	var cp checkpoint
	if err := json.Unmarshal([]byte(*job.CheckpointJSON), &cp); err != nil {
		t.Fatalf("checkpoint unparsable: %v", err)
	}
	if cp.Tiers == (db.TierFields{}) {
		t.Fatalf("the checkpoint carries the zero TierFields; a finalize-only retry off it commits blank tier columns:\n%s", *job.CheckpointJSON)
	}
	if cp.Tiers.OriginalCodec != "h264" || cp.Tiers.FullState != db.TierReady || cp.Tiers.LightState != db.TierReady ||
		cp.Tiers.OriginalWidth != 2560 || cp.Tiers.OriginalHeight != 1440 || cp.Tiers.OriginalPlayable {
		t.Fatalf("checkpoint tiers = %+v, want the run's own verdict (h264, 2560x1440, unplayable, full ready, light ready)", cp.Tiers)
	}

	// A second run off that very checkpoint: clip B's claim checkpoints these
	// bytes and is released (ReleaseJob keeps the checkpoint), the worker claims
	// it again — finalize-only, no probe — and must commit the same four columns
	// the first run did.
	if err := h.db.SaveCheckpoint(context.Background(), cj.ID, *cj.StartedAt, *job.CheckpointJSON, h.clock.Now().Unix()); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := h.db.ReleaseJob(context.Background(), cj.ID, db.ClaimStamp(cj), b.ID, h.clock.Now().Unix()); err != nil {
		t.Fatalf("ReleaseJob: %v", err)
	}
	h.w.Wake()
	second := h.waitClip(b.ID, db.ClipReady)
	h.waitIdle()
	h.assertLog("INFO", "finalize only")
	if second.OriginalPlayable != first.OriginalPlayable || second.OriginalCodec != first.OriginalCodec ||
		second.FullState != first.FullState || second.LightState != first.LightState {
		t.Fatalf("the finalize-only retry committed different tier columns:\n first  playable=%v codec=%q full=%q light=%q\n second playable=%v codec=%q full=%q light=%q",
			first.OriginalPlayable, first.OriginalCodec, first.FullState, first.LightState,
			second.OriginalPlayable, second.OriginalCodec, second.FullState, second.LightState)
	}
	if second.OriginalWidth == nil || *second.OriginalWidth != 2560 || second.FullCodec != "h264" {
		t.Fatalf("the retry's other tier columns: width %v full_codec %q", second.OriginalWidth, second.FullCodec)
	}
	h.assertNoErrorLogs()
}

// TestARunWhoseRowWasRepairedEndsAsALostAttempt is AUDIT #12 at the pipeline
// (§20 erratum 291). An operator's `admin jobs unstick` (a second process on
// the database, which is why the worker's own live set cannot refuse it) takes
// the row from under a run mid-encode. The run must not write its stage, its
// checkpoint or a failure over the row — the repair already spent the one
// crash attempt the row owes, and a newer attempt may be on it — and it must
// discard what it uploaded, because nothing references those objects.
func TestARunWhoseRowWasRepairedEndsAsALostAttempt(t *testing.T) {
	h := bothTiersHarness(t)
	c, _ := h.newClip("mkv", 4096, true)
	h.runner.tiersFn = func(_ context.Context, full, light transcode.Plan, in, fullOut, lightOut string) error {
		// The operator's unstick, from inside the encode: the row is old enough
		// by its own stamp (started_at <= notAfter), so the repair takes it.
		j := h.jobOf(c.ID)
		if _, err := h.db.UnstickJob(context.Background(), j.ID, *j.StartedAt, h.w.lim, h.clock.Now().Unix()); err != nil {
			return err
		}
		return (&fakeRunner{}).EncodeTiers(context.Background(), full, light, in, fullOut, lightOut, transcode.LightTag{}, 0, nil)
	}
	h.start()
	h.waitLog("job row is no longer this attempt's; outputs discarded, row untouched")
	h.waitIdle()

	job := h.jobOf(c.ID)
	if job.State != db.JobQueued || job.Attempts != 1 || job.TransientFailures != 0 || job.CheckpointJSON != nil ||
		job.LastError == nil || *job.LastError != db.UnstickReason {
		t.Fatalf("the repaired row after the lost attempt = %+v, want queued, ONE crash attempt (the repair's), no transient failure, no checkpoint, the unstick reason", job)
	}
	if got := h.clipRow(c.ID); got.State != db.ClipQueued {
		t.Fatalf("clip = %s, want queued (the repair's coupling, untouched by the lost attempt)", got.State)
	}
	// The attempt's uploads are gone and the original is not.
	for _, k := range h.store.Keys() {
		if k != c.SourceKey {
			t.Fatalf("an object of the lost attempt survived: %s", k)
		}
	}
	h.assertOriginalIntact(c)
	h.assertLog("INFO", "stage not recorded")
	h.assertNoErrorLogs()
}
