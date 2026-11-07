package worker

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/transcode"
)

// An audio clip end to end through the worker (§20 erratum 328): which files
// a run produced, what the row says, and the proofs it asked for.

// audioProbe is one stored (already stripped) audio file of `format`/`codec`.
func audioProbe(format, codec, sampleFmt string, bits, channels int) *transcode.ProbeResult {
	return &transcode.ProbeResult{Kind: transcode.KindAudio, VideoIndex: -1, AudioIndexes: []int{0},
		AudioCodecs: []string{codec}, FormatName: format, AudioSampleFmt: sampleFmt, AudioBits: bits,
		AudioChannels: channels, AudioSampleRate: 48000, HasDuration: true, DurationUS: 4_000_000,
		Audio: transcode.StreamExtent{EndSec: 4, Known: true}}
}

// audioOutputs measures each produced file by its name, as ffprobe would: the
// Full is sound alone, the card copy is the audiogram (the card frame's
// 1280×720 at 30 fps) over one AAC stream.
func audioOutputs(full string) func(context.Context, string) (*transcode.OutputInfo, error) {
	return func(_ context.Context, path string) (*transcode.OutputInfo, error) {
		if strings.HasPrefix(filepath.Base(path), "light-") {
			return &transcode.OutputInfo{Width: 1280, Height: 720, FPS: transcode.Rational{Num: 30, Den: 1}, DurationMS: 4000,
				HasAudio: true, VideoCodec: "h264", AudioCodec: "aac", VideoStreams: 1, AudioStreams: 1}, nil
		}
		return &transcode.OutputInfo{DurationMS: 4000, HasAudio: true, AudioCodec: full, AudioStreams: 1}, nil
	}
}

func audioHarness(t *testing.T, p *transcode.ProbeResult, full string) *harness {
	t.Helper()
	h := tierHarness(t, p)
	h.runner.probeOutFn = audioOutputs(full)
	return h
}

func (h *harness) audioClip(ext string) db.Clip {
	h.t.Helper()
	c, _ := h.newClip(ext, 64, true)
	return c
}

// TestAudioWavGetsABitExactFLACFull: a 24-bit WAV (D1a) — no Original to play,
// a FLAC Full at an audio rendition key under audio/flac, PROVEN by the PCM
// hash, the card copy always, the poster, and no picture of its own.
func TestAudioWavGetsABitExactFLACFull(t *testing.T) {
	h := audioHarness(t, audioProbe("wav", "pcm_s24le", "s32", 24, 2), "flac")
	c := h.audioClip("wav")
	h.start()
	got := h.waitClip(c.ID, db.ClipReady)
	h.waitIdle()

	if got.RenditionKey == nil || !strings.HasSuffix(*got.RenditionKey, ".flac") || !strings.Contains(*got.RenditionKey, "/renditions/main-") {
		t.Fatalf("rendition key %v, want renditions/main-{r8}.flac", got.RenditionKey)
	}
	if ct, ok := h.store.ContentType(*got.RenditionKey); !ok || ct != "audio/flac" {
		t.Fatalf("the Full is stored as %q (%v), want audio/flac", ct, ok)
	}
	if got.OriginalPlayable || got.FullState != db.TierReady || got.FullCodec != transcode.AudioFullFLAC || got.LightState != db.TierReady {
		t.Fatalf("tiers: playable=%v full=%q/%q light=%q", got.OriginalPlayable, got.FullState, got.FullCodec, got.LightState)
	}
	if got.Width != nil || got.Height != nil || got.FPSNum != nil || got.OriginalWidth != nil {
		t.Fatalf("an audio clip has no picture of its own: %v %v %v %v", got.Width, got.Height, got.FPSNum, got.OriginalWidth)
	}
	if got.DurationMS == nil || *got.DurationMS != 4000 || got.HasAudio == nil || !*got.HasAudio {
		t.Fatalf("duration %v has_audio %v", got.DurationMS, got.HasAudio)
	}
	if got.SocialKey == nil || got.SocialWidth == nil || *got.SocialWidth != 1280 || got.PosterKey == nil {
		t.Fatalf("card copy %v (%v wide), poster %v", got.SocialKey, got.SocialWidth, got.PosterKey)
	}
	if got.PosterWidth == nil || *got.PosterWidth != 1280 || *got.PosterHeight != 720 {
		t.Fatalf("poster %vx%v, want the card frame's 1280x720", got.PosterWidth, got.PosterHeight)
	}
	calls := strings.Join(h.runner.Calls(), "\n")
	for _, want := range []string{"AudioFull flac source.wav", "SamePCM source.wav out-", "AudioLight out-"} {
		if !strings.Contains(calls, want) {
			t.Errorf("no %q among the calls:\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "Encode") || strings.Contains(calls, "Remux") || strings.Contains(calls, "\nPoster") {
		t.Errorf("a video stage ran for an audio clip:\n%s", calls)
	}
	h.assertNoErrorLogs()
}

// TestAudioLosslessProofFailureFailsTheClip: a FLAC whose samples differ is
// never published under a name that says lossless.
func TestAudioLosslessProofFailureFailsTheClip(t *testing.T) {
	h := audioHarness(t, audioProbe("aiff", "pcm_s16be", "s16", 16, 2), "flac")
	h.runner.samePCMFn = func(context.Context, string, string) error {
		return &transcode.ErrPermanent{Err: errors.New("bad_output: the lossless copy's samples differ from the source's")}
	}
	c := h.audioClip("aiff")
	h.start()
	got := h.waitClip(c.ID, db.ClipFailed)
	if got.FailureReason == nil || !strings.Contains(*got.FailureReason, "samples differ") {
		t.Fatalf("failure %v", got.FailureReason)
	}
}

// TestAudioPlayableOriginalHasNoFull: an MP3 (or an Opus voice note) plays as
// its stored original — no Full, no encoder but the card copy's.
func TestAudioPlayableOriginalHasNoFull(t *testing.T) {
	for _, p := range []*transcode.ProbeResult{
		audioProbe("mp3", "mp3", "fltp", 0, 2),
		audioProbe("ogg", "opus", "fltp", 0, 1),
		audioProbe("flac", "flac", "s32", 24, 2),
	} {
		h := audioHarness(t, p, "")
		c := h.audioClip("bin")
		h.start()
		got := h.waitClip(c.ID, db.ClipReady)
		h.waitIdle()
		if got.RenditionKey != nil || !got.OriginalPlayable || got.FullState != db.TierSkipped || got.OriginalCodec != p.AudioCodecs[0] {
			t.Fatalf("%s: rendition %v playable %v full %q codec %q", p.AudioCodecs[0], got.RenditionKey, got.OriginalPlayable, got.FullState, got.OriginalCodec)
		}
		if !db.OriginalIsTheClip(got) {
			t.Fatalf("%s: the stored original is the clip", p.AudioCodecs[0])
		}
		if key, _, _, ok := db.PlayObject(got); !ok || key != got.SourceKey {
			t.Fatalf("%s: PlayObject %q %v", p.AudioCodecs[0], key, ok)
		}
		calls := strings.Join(h.runner.Calls(), "\n")
		if strings.Contains(calls, "AudioFull") || !strings.Contains(calls, "AudioLight source.bin") {
			t.Fatalf("%s: calls\n%s", p.AudioCodecs[0], calls)
		}
	}
}

// TestAudioFloatGetsAFittedFLAC24: 32-bit float (D2a) — the peak is measured
// first and a source over full scale is written with the gain that fits it.
func TestAudioFloatGetsAFittedFLAC24(t *testing.T) {
	h := audioHarness(t, audioProbe("wav", "pcm_f32le", "flt", 32, 2), "flac")
	h.runner.peakFn = func(context.Context, string) (float64, error) { return 3, nil }
	c := h.audioClip("wav")
	h.start()
	got := h.waitClip(c.ID, db.ClipReady)
	h.waitIdle()
	if got.FullCodec != transcode.AudioFullFLAC24 {
		t.Fatalf("full codec %q, want flac24", got.FullCodec)
	}
	if g := h.runner.AudioGains(); len(g) != 1 || g[0] != transcode.FitGainDB(3) {
		t.Fatalf("gains %v, want [%v]", g, transcode.FitGainDB(3))
	}
	calls := strings.Join(h.runner.Calls(), "\n")
	if strings.Contains(calls, "SamePCM") || !strings.Contains(calls, "AudioPeak source.wav") {
		t.Fatalf("a 24-bit Full of a float source is never PROVEN lossless, and its peak is measured:\n%s", calls)
	}
}

// TestAudioLossyGetsAnAACFull: WMA (D8a) — AAC 256k in an m4a under audio/mp4.
func TestAudioLossyGetsAnAACFull(t *testing.T) {
	h := audioHarness(t, audioProbe("asf", "wmav2", "fltp", 0, 2), "aac")
	c := h.audioClip("wma")
	h.start()
	got := h.waitClip(c.ID, db.ClipReady)
	h.waitIdle()
	if got.RenditionKey == nil || !strings.HasSuffix(*got.RenditionKey, ".m4a") || got.FullCodec != transcode.AudioFullAAC {
		t.Fatalf("rendition %v codec %q", got.RenditionKey, got.FullCodec)
	}
	if ct, _ := h.store.ContentType(*got.RenditionKey); ct != "audio/mp4" {
		t.Fatalf("stored as %q", ct)
	}
}

// TestAudioFullOverBudgetFails: an audio encode runs for as long as the
// container CLAIMS, so it is held to outputBudget's ceiling (`-fs`) and a file
// that reached it fails rather than being published truncated.
func TestAudioFullOverBudgetFails(t *testing.T) {
	h := audioHarness(t, audioProbe("asf", "wmav2", "fltp", 0, 2), "aac")
	h.runner.audioFullBytes = make([]byte, 200) // the source is 64 bytes: the budget is 128
	c := h.audioClip("wma")
	h.start()
	got := h.waitClip(c.ID, db.ClipFailed)
	if got.FailureReason == nil || !strings.Contains(*got.FailureReason, "output_too_large") {
		t.Fatalf("failure %v", got.FailureReason)
	}
	if b := h.runner.Budgets(); len(b) == 0 || b[0] != 128 {
		t.Fatalf("budgets %v, want the encode held to 128", b)
	}
}

// TestAudioWaveformOnTheRow (§20 erratum 329): one run reads the peaks ONCE
// from the stored original, draws the stencil the audiogram is encoded under
// and the poster from the SAME bars, stores the poster as a PNG under
// image/png, and commits the bars to the row for the page.
func TestAudioWaveformOnTheRow(t *testing.T) {
	h := audioHarness(t, audioProbe("wav", "pcm_s16le", "s16", 16, 2), "flac")
	c := h.audioClip("wav")
	h.start()
	got := h.waitClip(c.ID, db.ClipReady)
	h.waitIdle()
	ramp := make([]uint8, transcode.WaveBars)
	for i := range ramp {
		ramp[i] = uint8(i * 255 / (transcode.WaveBars - 1))
	}
	if got.Peaks != transcode.EncodePeaks(ramp) {
		t.Fatalf("the row's peaks %q, want the bars the run read %q", got.Peaks, transcode.EncodePeaks(ramp))
	}
	if got.PosterKey == nil || !strings.HasSuffix(*got.PosterKey, ".png") {
		t.Fatalf("poster key %v, want a .png", got.PosterKey)
	}
	if ct, ok := h.store.ContentType(*got.PosterKey); !ok || ct != "image/png" {
		t.Fatalf("the poster is stored as %q (%v), want image/png", ct, ok)
	}
	calls := strings.Join(h.runner.Calls(), "\n")
	if strings.Count(calls, "WavePeaks source.wav") != 1 {
		t.Errorf("the peaks were not read once from the stored original:\n%s", calls)
	}
	if !strings.Contains(calls, "AudioPoster 70 bars 4000 ms mark=true icon-192.png") {
		t.Errorf("the poster was not drawn from the 70 bars with the mark and the duration:\n%s", calls)
	}
	if strings.Index(calls, "WavePeaks") > strings.Index(calls, "AudioLight") {
		t.Errorf("the stencil's peaks were read after the encode:\n%s", calls)
	}
}

// TestAudioPosterMarkFollowsTheBadge (erratum 285; run 10, lane nits10): on an
// instance with an INSTANCE_BADGE the poster carries the GREY-span mark, the
// one the Discord card's thumbnail wears there (static.MarkIcon192For), not
// production's green one.
func TestAudioPosterMarkFollowsTheBadge(t *testing.T) {
	probe := audioProbe("wav", "pcm_s16le", "s16", 16, 2)
	h := newHarnessCfg(t, func(c *config.Config) { c.SocialMaxBytes = 100 << 20; c.InstanceBadge = "dev" })
	h.runner.probeFn = func(context.Context, string) (*transcode.ProbeResult, error) { return probe, nil }
	h.runner.probeOutFn = audioOutputs("flac")
	c := h.audioClip("wav")
	h.start()
	h.waitClip(c.ID, db.ClipReady)
	h.waitIdle()
	if calls := strings.Join(h.runner.Calls(), "\n"); !strings.Contains(calls, "AudioPoster 70 bars 4000 ms mark=true icon-dev-192.png") {
		t.Errorf("a badged instance's poster does not carry the grey-span mark:\n%s", calls)
	}
}

// TestAudioCardCarriesAnAACAsItIs (D6a): an AAC that fits is copied into the
// audiogram — here a WMA's AAC Full, the sound the card is made from; an MP3 is
// re-encoded at the fitted bitrate (the card's verifier takes AAC alone). The
// rule itself, an AAC original included, is transcode's TestAudioLightCopies.
func TestAudioCardCarriesAnAACAsItIs(t *testing.T) {
	for _, c := range []struct {
		probe *transcode.ProbeResult
		full  string
		want  string
	}{
		{audioProbe("mp3", "mp3", "fltp", 0, 2), "", "AudioLight source.bin 256k"},
		{audioProbe("asf", "wmav2", "fltp", 0, 2), "aac", "AudioLight out-"},
	} {
		h := audioHarness(t, c.probe, c.full)
		cl := h.audioClip("bin")
		h.start()
		h.waitClip(cl.ID, db.ClipReady)
		h.waitIdle()
		calls := strings.Join(h.runner.Calls(), "\n")
		if !strings.Contains(calls, c.want) {
			t.Errorf("%s: no %q among the calls:\n%s", c.probe.AudioCodecs[0], c.want, calls)
		}
		if c.full == "aac" && !strings.Contains(calls, ".m4a copy") {
			t.Errorf("the AAC Full is not what the card copies:\n%s", calls)
		}
	}
}

// TestCheckpointCarriesThePeaks: a finalize-only retry commits the bars this
// attempt drew (§20 erratum 291's rule, applied to erratum 329's column).
func TestCheckpointCarriesThePeaks(t *testing.T) {
	f := db.ReadyFields{Peaks: "00ff"}
	if got := checkpointOf(f).ready(); got.Peaks != "00ff" {
		t.Fatalf("the checkpoint lost the peaks: %+v", got)
	}
}
