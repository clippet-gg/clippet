package worker

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/transcode"
)

// The quality tiers, end to end through the worker (DESIGN.md §17 #10, §20
// errata 200–206). Every test here asks the same question from a different
// angle: WHICH files did this run produce, and does the row say what happened?
//
// The four shapes, and what each proves:
//
//	playable + faststart          → nothing at all is encoded (the fast path)
//	playable + moov at the end    → one `-c copy` remux, no encoder
//	unplayable + card-shaped      → one Full encode, which IS the card file
//	unplayable + big or smooth    → ONE encode, both tiers (§20 erratum 217)

// playableProbe is a 1280×720 30 fps H.264 mp4 with one aac track — the source
// every row of the playability table says yes to.
func playableProbe() *transcode.ProbeResult {
	p := defaultProbeResult()
	p.FormatName = "mov,mp4,m4a,3gp,3g2,mj2"
	p.PixFmt = "yuv420p"
	p.AudioCodecs = []string{"aac"}
	p.FieldOrder = "progressive"
	return p
}

// tierHarness is newHarness with the Light tier ON at a threshold no fake output
// can reach (100 MB), so a Light copy is planned by SHAPE alone and the tests
// below read as statements about dimensions and frame rates rather than about
// byte counts.
func tierHarness(t *testing.T, probe *transcode.ProbeResult) *harness {
	t.Helper()
	h := newHarnessCfg(t, func(c *config.Config) { c.SocialMaxBytes = 100 << 20 })
	h.runner.probeFn = func(context.Context, string) (*transcode.ProbeResult, error) { return probe, nil }
	return h
}

// mp4Boxes renders a minimal top-level box sequence — the ONLY thing
// transcode.VerifyMP4 and FaststartMP4 look at. Their sizes are fixed so a test
// can hand the same byte count to newClip's row.
func mp4Boxes(order ...string) []byte {
	sizes := map[string]int{"ftyp": 8, "moov": 16, "mdat": 32}
	var out []byte
	for _, typ := range order {
		payload := sizes[typ]
		size := 8 + payload
		out = append(out, byte(size>>24), byte(size>>16), byte(size>>8), byte(size))
		out = append(out, typ...)
		out = append(out, make([]byte, payload)...)
	}
	return out
}

// faststartLen is the length of a faststart-shaped fixture, so a clip row can be
// created with the right source size before the object is overwritten with one.
var faststartMP4Bytes = mp4Boxes("ftyp", "moov", "mdat")

// playableClip creates a clip whose stored object really is a faststart mp4, so
// the tier plan's `Faststart` term is decided by walking the file rather than by
// a test asserting it.
func (h *harness) playableClip() db.Clip {
	h.t.Helper()
	c, _ := h.newClip("mp4", int64(len(faststartMP4Bytes)), false)
	h.store.Put(c.SourceKey, faststartMP4Bytes, "video/mp4")
	return c
}

func TestPlayableOriginalIsServedAsItStands(t *testing.T) {
	h := tierHarness(t, playableProbe())
	c := h.playableClip()
	h.start()
	got := h.waitClip(c.ID, db.ClipReady)
	h.waitIdle()

	if got.RenditionKey != nil {
		t.Fatalf("a playable faststart upload must produce NO rendition object; got %q", *got.RenditionKey)
	}
	if !got.OriginalPlayable || got.OriginalRemuxed {
		t.Fatalf("original_playable=%v original_remuxed=%v, want true/false", got.OriginalPlayable, got.OriginalRemuxed)
	}
	if got.FullState != db.TierSkipped || got.LightState != db.TierSkipped {
		t.Fatalf("tier states full=%q light=%q, want skipped/skipped", got.FullState, got.LightState)
	}
	// The clip's own measurements are the UPLOAD's.
	if got.Width == nil || *got.Width != 1280 || got.Height == nil || *got.Height != 720 {
		t.Fatalf("dims are not the upload's: %v x %v", got.Width, got.Height)
	}
	if got.OriginalWidth == nil || *got.OriginalWidth != 1280 || got.OriginalCodec != "h264" {
		t.Fatalf("original columns not filled: %v codec=%q", got.OriginalWidth, got.OriginalCodec)
	}
	if got.PosterKey == nil {
		t.Fatal("no poster: the card image is the one thing this path still has to make")
	}
	// Not one encoder ran. This is the whole claim of the fast path.
	for _, call := range h.runner.Calls() {
		if strings.HasPrefix(call, "Encode") || strings.HasPrefix(call, "Remux") {
			t.Fatalf("the fast path spawned %q; it must encode nothing at all: %v", call, h.runner.Calls())
		}
	}
	// db.PlayObject resolves the upload, which is what /video.mp4 then serves.
	key, size, etag, ok := db.PlayObject(got)
	if !ok || key != got.SourceKey || size != got.SourceSize || etag != got.SourceETag {
		t.Fatalf("PlayObject = %q/%d/%q ok=%v, want the source triple", key, size, etag, ok)
	}
	if !db.OriginalIsTheClip(got) {
		t.Fatal("OriginalIsTheClip is false for the one shape it exists to describe")
	}
	h.assertNoErrorLogs()
}

func TestPlayableOriginalWithMoovAtTheEndIsRemuxed(t *testing.T) {
	h := tierHarness(t, playableProbe())
	// `mdat` before `moov` — what ffmpeg writes unless told otherwise, and
	// therefore what an ordinary upload looks like.
	body := mp4Boxes("ftyp", "mdat", "moov")
	c, _ := h.newClip("mp4", int64(len(body)), false)
	h.store.Put(c.SourceKey, body, "video/mp4")
	h.start()
	got := h.waitClip(c.ID, db.ClipReady)
	h.waitIdle()

	if got.RenditionKey == nil {
		t.Fatal("a remux must produce a rendition object")
	}
	if !got.OriginalPlayable || !got.OriginalRemuxed {
		t.Fatalf("original_playable=%v original_remuxed=%v, want true/true", got.OriginalPlayable, got.OriginalRemuxed)
	}
	if got.FullState != db.TierSkipped {
		t.Fatalf("full_state = %q; a remux is the ORIGINAL tier, not a Full", got.FullState)
	}
	// The dims are the upload's DISPLAY dims and not the copy's coded ones.
	if got.Width == nil || *got.Width != 1280 {
		t.Fatalf("width = %v, want the upload's 1280", got.Width)
	}
	var remuxed bool
	for _, call := range h.runner.Calls() {
		if strings.HasPrefix(call, "Remux ") {
			remuxed = true
		}
		if strings.HasPrefix(call, "Encode ") {
			t.Fatalf("a remux must not encode: %v", h.runner.Calls())
		}
	}
	if !remuxed {
		t.Fatalf("Remux was never called: %v", h.runner.Calls())
	}
	// A remuxed original is NOT anonymous-readable: the public playable bytes are
	// the remux, at /video.mp4 (§20 erratum 207).
	if db.OriginalIsTheClip(got) {
		t.Fatal("a remuxed clip must not open /original to anonymous readers")
	}
	h.assertNoErrorLogs()
}

func TestUnplayableCardShapedSourceGetsOneFullAndNoLight(t *testing.T) {
	// defaultProbeResult declares no container at all, so it is not playable —
	// and it is 720p30, so the Full IS the card's file and nothing is ordered.
	h := tierHarness(t, defaultProbeResult())
	c, _ := h.newClip("mkv", 1024, true)
	h.start()
	got := h.waitClip(c.ID, db.ClipReady)
	h.waitIdle()

	if got.RenditionKey == nil || got.SocialKey != nil {
		t.Fatalf("want a rendition and no light copy; rendition=%v light=%v", got.RenditionKey, got.SocialKey)
	}
	if got.FullState != db.TierReady || got.FullCodec != "h264" {
		t.Fatalf("full_state=%q full_codec=%q, want ready/h264", got.FullState, got.FullCodec)
	}
	if got.LightState != db.TierSkipped {
		t.Fatalf("light_state = %q, want skipped", got.LightState)
	}
	if got.OriginalPlayable {
		t.Fatal("original_playable is true for a source with no recognised container")
	}
	// No `full` job: this run made the Full itself.
	if _, err := h.db.LatestJobOfKind(context.Background(), c.ID, db.KindFull); err == nil {
		t.Fatal("a full job was queued although the transcode produced the Full itself")
	}
	h.assertNoErrorLogs()
}

// TestUnplayableCardShapedSourceOverTheCapGetsBothTiersAtTheFlip is erratum
// 235's regression, end to end through the worker.
//
// The source is card-SHAPED — 1280x720 at 30 fps, so neither the picture term nor
// the frame-rate term of the skip rule fires — and no browser plays it, so the
// FULL is the file a crawler would fetch. It is also 80 seconds long, which at
// CRF 18 is well past the shipped 25 MiB SOCIAL_MAX_BYTES.
//
// Before erratum 235 the plan had no size for a file it had not encoded yet, so
// it decided CardTier=full and LightNeeded=false: the run made ONE encode, flipped
// `ready`, and only in finalize discovered the Full was over the cap and queued a
// `social` job. In that window db.CardReady was TRUE — CardObject falls back to
// PlayObject, which is the Full — so POST /api/upload answered card_ready: true,
// the crawler hold let a Discordbot through, and og:video pointed at a file above
// the very threshold the Light tier exists to stay under.
//
// The assertion is therefore about the FLIP and not about the eventual state: at
// the moment the clip becomes ready it must have BOTH files and be card-ready.
func TestUnplayableCardShapedSourceOverTheCapGetsBothTiersAtTheFlip(t *testing.T) {
	p := defaultProbeResult()
	p.DurationUS = 80_000_000
	p.Video = transcode.StreamExtent{EndSec: 80, Known: true}
	p.Audio = transcode.StreamExtent{EndSec: 80, Known: true}
	h := newHarnessCfg(t, func(c *config.Config) { c.SocialMaxBytes = 25 << 20 })
	h.runner.probeFn = func(context.Context, string) (*transcode.ProbeResult, error) { return p, nil }
	c, _ := h.newClip("mkv", 1024, true)
	h.start()
	ready := h.waitClip(c.ID, db.ClipReady)

	// BOTH FILES, at the flip.
	switch {
	case ready.RenditionKey == nil:
		t.Fatal("the clip became ready with no Full")
	case ready.SocialKey == nil:
		t.Fatalf("the clip became ready with no card copy although its Full is planned at %d bytes, past the %d cap",
			transcode.PlannedOutputBytes(transcode.MakeTierPlan(p, transcode.TierPlanOptions{
				Box: transcode.PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60}, LightMaxBytes: 25 << 20,
				SourceSize: 1024, SourceMediaType: "video/x-matroska"}).Full), int64(25<<20))
	case ready.FullState != db.TierReady || ready.LightState != db.TierReady:
		t.Fatalf("tier states full=%q light=%q at the ready flip, want ready/ready", ready.FullState, ready.LightState)
	}
	// Which is what the ONE predicate the capture reply, the crawler hold and the
	// watch page's line all read says too.
	if !db.CardReady(ready) {
		t.Fatal("db.CardReady is false at the ready flip")
	}
	if key, _, _, _ := db.CardObject(ready); key != *ready.SocialKey {
		t.Fatalf("the card resolves to %q, want the light copy %q", key, *ready.SocialKey)
	}
	h.waitIdle()

	// ONE ffmpeg wrote both (§20 erratum 217), and no `social` job was queued
	// behind the flip to do the work the plan should have asked for.
	tiers := 0
	for _, call := range h.runner.Calls() {
		switch {
		case strings.HasPrefix(call, "EncodeTiers "):
			tiers++
		case strings.HasPrefix(call, "EncodeSocial "):
			t.Fatalf("a second encode ran for the card copy: %v", h.runner.Calls())
		}
	}
	if tiers != 1 {
		t.Fatalf("%d EncodeTiers calls, want one: %v", tiers, h.runner.Calls())
	}
	if j, err := h.db.LatestJobOfKind(context.Background(), c.ID, db.KindSocial); err == nil {
		t.Fatalf("a social job was queued after the flip: %+v", j)
	}
	// The Full is still the source's own frame, and it is what /video.mp4 plays:
	// over-planning a card copy must not cost the clip its quality.
	if ready.Width == nil || *ready.Width != 1280 || ready.Height == nil || *ready.Height != 720 {
		t.Fatalf("dims at the flip = %v x %v", ready.Width, ready.Height)
	}
	if k, _, _, ok := db.PlayObject(ready); !ok || k != *ready.RenditionKey {
		t.Fatalf("PlayObject = %q ok=%v, want the Full", k, ok)
	}
	h.assertNoErrorLogs()
}

func TestBothTiersComeOutOfOneDecode(t *testing.T) {
	// A 1440p60 source the browser cannot play needs a FAITHFUL copy and a CARD
	// copy, and since §20 erratum 217 one ffmpeg writes both: at the ready flip
	// the clip has the Full AND the card file, no `full` job is queued behind it,
	// and nothing ever serves the 720p30 copy as the clip's own playable bytes.
	// This supersedes erratum 206's ordering for this case, and the reason is
	// measured — two processes put the card's file 2.2x LATER than encoding it
	// alone, and back to back decodes the same source twice.
	p := defaultProbeResult()
	p.CodedW, p.CodedH, p.DisplayW, p.DisplayH = 2560, 1440, 2560, 1440
	p.FPS, p.RFPS = transcode.Rational{Num: 60, Den: 1}, transcode.Rational{Num: 60, Den: 1}
	h := tierHarness(t, p)
	// One fake answers for both files, from the path it was handed.
	h.runner.probeOutFn = func(_ context.Context, path string) (*transcode.OutputInfo, error) {
		if strings.Contains(path, "light-") {
			return &transcode.OutputInfo{Width: 1280, Height: 720, FPS: transcode.Rational{Num: 30, Den: 1},
				DurationMS: 6000, HasAudio: true, VideoCodec: "h264", AudioCodec: "aac", VideoStreams: 1, AudioStreams: 1}, nil
		}
		return &transcode.OutputInfo{Width: 2560, Height: 1440, FPS: transcode.Rational{Num: 60, Den: 1},
			DurationMS: 6000, HasAudio: true, VideoCodec: "h264", AudioCodec: "aac", VideoStreams: 1, AudioStreams: 1}, nil
	}
	h.runner.tagText = "{light} preview · {best} on Clippet"
	c, _ := h.newClip("mkv", 4096, true)
	h.start()
	ready := h.waitClip(c.ID, db.ClipReady)
	h.waitIdle()

	// AT THE READY FLIP: both files exist.
	switch {
	case ready.SocialKey == nil:
		t.Fatal("the clip became ready with no light copy; `ready` must mean card-ready")
	case ready.RenditionKey == nil:
		t.Fatal("the clip became ready with no Full; erratum 217 publishes both at once")
	case ready.FullState != db.TierReady || ready.LightState != db.TierReady:
		t.Fatalf("tier states full=%q light=%q at the ready flip, want ready/ready", ready.FullState, ready.LightState)
	case ready.FullCodec != "h264":
		t.Fatalf("full_codec = %q, want h264", ready.FullCodec)
	}
	// The clip's five measurements are the FAITHFUL copy's, never the card's, and
	// that is also what /video.mp4 resolves to: nothing serves the 720p30 file.
	if ready.Width == nil || *ready.Width != 2560 || ready.Height == nil || *ready.Height != 1440 {
		t.Fatalf("dims at the ready flip = %v x %v, want the source's 2560x1440", ready.Width, ready.Height)
	}
	if k, _, _, ok := db.PlayObject(ready); !ok || k != *ready.RenditionKey {
		t.Fatalf("PlayObject = %q ok=%v, want the Full", k, ok)
	}
	if ready.SocialFPSNum == nil || *ready.SocialFPSNum != 30 {
		t.Fatalf("the light copy's measured rate was not stored: %v", ready.SocialFPSNum)
	}
	// No `full` job at all: this run made the Full itself.
	if _, err := h.db.LatestJobOfKind(context.Background(), c.ID, db.KindFull); err == nil {
		t.Fatal("a full job was queued although the transcode produced the Full itself")
	}
	// ONE ffmpeg, not two: EncodeTiers, and neither of the single-output passes.
	tiers := 0
	for _, call := range h.runner.Calls() {
		switch {
		case strings.HasPrefix(call, "EncodeTiers "):
			tiers++
		case strings.HasPrefix(call, "Encode "), strings.HasPrefix(call, "EncodeSocial "):
			t.Fatalf("a second encode ran: %v", h.runner.Calls())
		}
	}
	if tiers != 1 {
		t.Fatalf("%d EncodeTiers calls, want one: %v", tiers, h.runner.Calls())
	}
	// The pair of plans that one call was handed: the Full at the SOURCE's size
	// (the whole feature) and the Light at the card's box.
	pairs := h.runner.TierPairs()
	if len(pairs) != 1 {
		t.Fatalf("tier pairs = %+v, want one", pairs)
	}
	full, light := pairs[0][0], pairs[0][1]
	if full.W != 2560 || full.H != 1440 || full.FPS != (transcode.Rational{Num: 60, Den: 1}) {
		t.Fatalf("the Full plan is %dx%d @ %s, want 2560x1440 @ 60/1", full.W, full.H, full.FPS.String())
	}
	if light.W != 1280 || light.H != 720 || light.FPS != (transcode.Rational{Num: 30, Den: 1}) {
		t.Fatalf("the Light plan is %dx%d @ %s, want 1280x720 @ 30/1", light.W, light.H, light.FPS.String())
	}
	// The tag went with the LIGHT copy and names both real qualities.
	tags := h.runner.LightTags()
	if len(tags) != 1 || tags[0].TextFile == "" {
		t.Fatalf("light tags = %+v, want one with a text file", tags)
	}
	lines := h.runner.TagLines()
	if want := "720p30 preview · 1440p60 on Clippet"; len(lines) != 1 || lines[0] != want {
		t.Fatalf("the tag reads %q, want one line %q", lines, want)
	}
	// And both objects hold their own bytes.
	if got, ok := h.store.Get(*ready.RenditionKey); !ok || string(got) != renditionPayload {
		t.Fatalf("the Full object is missing or wrong (%q, ok=%v)", got, ok)
	}
	if got, ok := h.store.Get(*ready.SocialKey); !ok || string(got) != socialPayload {
		t.Fatalf("the card object is missing or wrong (%q, ok=%v)", got, ok)
	}
	h.assertNoErrorLogs()
}

// TestTwoTierEncodeFailureFailsTheClip is the OTHER side of erratum 217's trade,
// recorded as a test because it is a behaviour change: one process writes both
// files, so a permanent failure is the CLIP's and not merely a lost tier.
// Erratum 206's card-first ordering bought "a failed Full costs a clip nothing
// but quality" by encoding the two separately; that is what paying for the
// decode twice was buying. The recovery is every other upload's — the attempt
// budget, then the owner's retry.
func TestTwoTierEncodeFailureFailsTheClip(t *testing.T) {
	p := defaultProbeResult()
	p.CodedW, p.CodedH, p.DisplayW, p.DisplayH = 1920, 1080, 1920, 1080
	h := tierHarness(t, p)
	h.runner.tiersFn = func(context.Context, transcode.Plan, transcode.Plan, string, string, string) error {
		return &transcode.ErrPermanent{Err: errFullBoom}
	}
	c, _ := h.newClip("mkv", 4096, true)
	h.start()
	failed := h.waitClip(c.ID, db.ClipFailed)
	h.waitIdle()
	switch {
	case failed.FailureReason == nil || !strings.Contains(*failed.FailureReason, "boom"):
		t.Fatalf("failure_reason = %v, want the encoder's own reason", failed.FailureReason)
	case failed.RenditionKey != nil || failed.SocialKey != nil:
		t.Fatalf("a failed run left objects behind: rendition=%v light=%v", failed.RenditionKey, failed.SocialKey)
	}
	// Nothing was queued to finish the job later: there is no half-published clip.
	if _, err := h.db.LatestJobOfKind(context.Background(), c.ID, db.KindFull); err == nil {
		t.Fatal("a full job was queued for a clip that never became ready")
	}
}
func TestTierFieldsSurviveAFinalizeOnlyRetry(t *testing.T) {
	// The checkpoint carries the tier decision, because a retry has no probe to
	// re-derive it from (§20 erratum 206). Before it did, that retry panicked.
	f := db.ReadyFields{
		RenditionKey: "k", RenditionSize: 12, RenditionETag: "e",
		PosterKey: "p", PosterSize: 3, PosterETag: "pe", Width: 1920, Height: 1080,
		DurationMS: 6000, FPSNum: 60, FPSDen: 1, HasAudio: true,
		Light: db.SocialFields{Key: "l", Size: 4, ETag: "le", Width: 1280, Height: 720, FPSNum: 30, FPSDen: 1},
		Tiers: db.TierFields{OriginalPlayable: true, OriginalRemuxed: true, OriginalWidth: 1920, OriginalHeight: 1080,
			OriginalFPSNum: 60, OriginalFPSDen: 1, OriginalCodec: "h264",
			FullState: db.TierSkipped, LightState: db.TierReady},
	}
	raw, err := json.Marshal(checkpointOf(f))
	if err != nil {
		t.Fatalf("marshal checkpoint: %v", err)
	}
	var cp checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		t.Fatalf("unmarshal checkpoint: %v", err)
	}
	got := cp.ready()
	if got.Tiers != f.Tiers {
		t.Fatalf("tiers did not round-trip:\n got %+v\nwant %+v", got.Tiers, f.Tiers)
	}
	if got.Light != f.Light {
		t.Fatalf("the light copy did not round-trip:\n got %+v\nwant %+v", got.Light, f.Light)
	}
}

func TestTierPlanIsUncappedAndCodecAware(t *testing.T) {
	cfg := config.Config{SocialMaxBytes: 25 << 20}
	cfg.Transcode.MaxW, cfg.Transcode.MaxH, cfg.Transcode.MaxFPS = 1280, 720, 60
	cfg.Transcode.FullCodec = config.FullCodecAV1
	p := defaultProbeResult()
	p.CodedW, p.CodedH, p.DisplayW, p.DisplayH = 3840, 2160, 3840, 2160
	clip := db.Clip{SourceSize: 1 << 20, SourceContentType: "video/x-matroska"}
	tp := tierPlanFor(p, clip, cfg, false)
	if !tp.FullNeeded || tp.Full.W != 3840 || tp.Full.H != 2160 {
		t.Fatalf("full plan = %dx%d, want the source's own 3840x2160 — the instance box must not bind", tp.Full.W, tp.Full.H)
	}
	if !tp.LightNeeded || tp.Light.W != 1280 || tp.Light.H != 720 {
		t.Fatalf("light plan = %dx%d, want the tier's 1280x720", tp.Light.W, tp.Light.H)
	}
	// The CODEC belongs to the runner (the plan is geometry), and the runner must
	// read the configured value.
	if got := transcode.NormalizeFullCodec(cfg.Transcode.FullCodec); got != transcode.FullCodecAV1 {
		t.Fatalf("NormalizeFullCodec = %q, want av1", got)
	}
}

// TestTheCardImageIsNeverCutFromTheTaggedCopy is §20 erratum 219, and it guards
// exactly what shipped broken: the poster used to be cut from the LIGHT copy, and
// since erratum 214 that file carries a burned-in line — so the card IMAGE (the
// og:image, the still the site's own player shows before play, a JPEG anybody can
// download) carried a watermark the constraint's amendment does not cover.
//
// The claim is a PAIR per run: which file the frame was taken from, and the box it
// was written inside. Both are read off the fake, so this is an assertion about
// the pipeline's CHOICE with no ffmpeg in it; the pixels are the -tags ffmpeg
// test's (transcode.TestThePosterIsCutFromAnUntaggedFrame).
func TestTheCardImageIsNeverCutFromTheTaggedCopy(t *testing.T) {
	// cardBox is the card's own frame, which is what a poster cut from a bigger
	// untagged file is scaled into: a still cut from a 1440p frame is a card image
	// no crawler inlines, and that size is all the Light copy ever lent the poster.
	cardBox := transcode.PosterBox{W: 1280, H: 720}
	// oneCut returns the run's single card-image extraction and refuses the tagged
	// file outright, which is the sentence this test exists to write.
	oneCut := func(t *testing.T, h *harness) posterCut {
		t.Helper()
		cuts := h.runner.PosterCuts()
		if len(cuts) != 1 {
			t.Fatalf("poster cuts = %+v, want exactly one", cuts)
		}
		if strings.Contains(cuts[0].In, "light-") {
			t.Fatalf("the card image was cut from the TAGGED card copy (%s): erratum 219", cuts[0].In)
		}
		return cuts[0]
	}

	t.Run("both tiers from one decode: the FULL, scaled into the card's box", func(t *testing.T) {
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
		h.runner.tagText = "{light} preview · {best} on Clippet"
		c, _ := h.newClip("mkv", 4096, true)
		h.start()
		ready := h.waitClip(c.ID, db.ClipReady)
		h.waitIdle()
		if ready.SocialKey == nil {
			t.Fatal("no card copy was made, so this run says nothing about the tagged file")
		}
		cut := oneCut(t, h)
		if !strings.Contains(filepath.Base(cut.In), "out-") {
			t.Errorf("the card image was cut from %q, want the Full", cut.In)
		}
		if cut.Box != cardBox {
			t.Errorf("poster box = %+v, want the card's %+v", cut.Box, cardBox)
		}
		h.assertNoErrorLogs()
	})

	t.Run("a playable upload that still needs a card copy: the UPLOAD", func(t *testing.T) {
		// 1080p60 h264/aac in a faststart mp4: nothing is re-encoded (the upload
		// IS the clip), and a card copy is still wanted because 1080p60 is past
		// the card's box. The poster's only untagged file is then the upload.
		p := playableProbe()
		p.CodedW, p.CodedH, p.DisplayW, p.DisplayH = 1920, 1080, 1920, 1080
		p.FPS, p.RFPS = transcode.Rational{Num: 60, Den: 1}, transcode.Rational{Num: 60, Den: 1}
		h := tierHarness(t, p)
		c := h.playableClip()
		h.start()
		ready := h.waitClip(c.ID, db.ClipReady)
		h.waitIdle()
		switch {
		case ready.SocialKey == nil:
			t.Fatal("a 1080p60 playable upload must still get a card copy")
		case ready.RenditionKey != nil:
			t.Fatalf("a playable faststart upload produced a rendition (%q)", *ready.RenditionKey)
		}
		cut := oneCut(t, h)
		if !strings.Contains(filepath.Base(cut.In), "source.") {
			t.Errorf("the card image was cut from %q, want the upload itself", cut.In)
		}
		if cut.Box != cardBox {
			t.Errorf("poster box = %+v, want the card's %+v", cut.Box, cardBox)
		}
		h.assertNoErrorLogs()
	})

	t.Run("no card copy: no box either", func(t *testing.T) {
		// 720p30 and unplayable: the Full IS the card's file, nothing carries a
		// tag, and the poster is written at the size it was decoded at — the argv
		// this pass has always used.
		h := tierHarness(t, defaultProbeResult())
		c, _ := h.newClip("mkv", 1024, true)
		h.start()
		ready := h.waitClip(c.ID, db.ClipReady)
		h.waitIdle()
		if ready.SocialKey != nil {
			t.Fatalf("this shape must get no card copy; got %q", *ready.SocialKey)
		}
		cut := oneCut(t, h)
		if !strings.Contains(filepath.Base(cut.In), "out-") {
			t.Errorf("the card image was cut from %q, want the Full", cut.In)
		}
		if cut.Box != (transcode.PosterBox{}) {
			t.Errorf("poster box = %+v, want the zero box (nothing to scale into)", cut.Box)
		}
		h.assertNoErrorLogs()
	})
}

// TestSkippedTierJobSettlesItsTierState is §20 erratum 237: a tier job that ENDS
// WITHOUT PRODUCING ITS FILE and without failing — a skip, or a lost commit race —
// settles its tier column, which until now only a `failed` ending did.
//
// The trigger is ordinary and its consequence was permanent. A card copy is queued
// (or in a transient backoff) with `light_state = encoding` — after a trim, a
// replacement, or the commit that found its Full past the cap — and the operator
// restarts with SOCIAL_MAX_BYTES=0, or raised above the rendition. recheckClip then
// skips "the playable file no longer qualifies" and resolves the job as done,
// writing no tier state at all: the column read `encoding` for the life of the
// clip, so db.CardReady stayed false, a crawler was held with 503 Retry-After: 30
// for ever, POST /api/upload and the watch page said the card was not ready, and
// tiersLive polled /progress on every single view. Nothing an operator ran cleared
// it.
func TestSkippedTierJobSettlesItsTierState(t *testing.T) {
	ctx := context.Background()

	// The trigger above, through the loop: the feature is turned OFF under a job
	// that was queued while it was on.
	t.Run("a skip the loop ran", func(t *testing.T) {
		h := newHarnessCfg(t, func(c *config.Config) { c.SocialMaxBytes = 0 })
		c := h.gifReady()
		if err := h.db.SetTierState(ctx, c.ID, db.TierNameLight, db.TierEncoding, h.clock.Now().Unix()); err != nil {
			t.Fatal(err)
		}
		if db.CardReady(h.clip(c.ID)) {
			t.Fatal("premise: a clip whose card copy is `encoding` is not card-ready (erratum 235)")
		}
		if _, err := h.db.EnqueueSocial(ctx, c.ID, h.clock.Now().Unix()); err != nil {
			t.Fatal(err)
		}
		h.start()
		job := h.waitSocial(c.ID, jobDone)
		h.waitIdle()

		if job.Stage != db.JobStageSkipped {
			t.Fatalf("the job resolved at stage %q, want %q", job.Stage, db.JobStageSkipped)
		}
		after := h.clip(c.ID)
		if after.LightState != db.TierSkipped {
			t.Fatalf("light_state = %q after the only card job skipped, want %q", after.LightState, db.TierSkipped)
		}
		if !db.CardReady(after) {
			t.Fatal("the card is still not ready although no copy is coming: a crawler is held for ever")
		}
		if after.State != db.ClipReady || after.SocialKey != nil {
			t.Fatalf("the skip changed the clip: state=%q social=%v", after.State, after.SocialKey)
		}
		h.assertNoErrorLogs()
	})

	// The other ending, driven by hand because no scheduling of the loop produces
	// it: the commit lost the ETag race. The winning commit queues a replacement
	// only if none was live — and THIS job was — so this is an ending where
	// nothing else answers for the tier either.
	t.Run("a lost commit race with no replacement behind it", func(t *testing.T) {
		h := socialByHandHarness(t)
		c := h.gifReady()
		if err := h.db.SetTierState(ctx, c.ID, db.TierNameLight, db.TierEncoding, h.clock.Now().Unix()); err != nil {
			t.Fatal(err)
		}
		if _, err := h.db.EnqueueSocial(ctx, c.ID, h.clock.Now().Unix()); err != nil {
			t.Fatal(err)
		}
		claimed := h.claimKind(db.KindSocial)
		newSocialRun(h.w, ctx, &run{}, claimed, c).skipped("the rendition changed")

		after := h.clip(c.ID)
		if after.LightState != db.TierSkipped {
			t.Fatalf("light_state = %q after a lost race with no job behind it, want %q",
				after.LightState, db.TierSkipped)
		}
		if !db.CardReady(after) {
			t.Fatal("a lost race left the card unreadable for ever")
		}
		job, err := h.db.LatestJobOfKind(ctx, c.ID, db.KindSocial)
		if err != nil || job.State != db.JobDone {
			t.Fatalf("the job is %+v (%v), want done — a lost race is not a failure", job, err)
		}
	})
}

// errFullBoom is what the fake encoder refuses with in
// TestTwoTierEncodeFailureFailsTheClip.
var errFullBoom = errors.New("ffmpeg exited 1: boom")
