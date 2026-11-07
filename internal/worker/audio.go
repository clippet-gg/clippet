package worker

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"time"

	"clippet/internal/db"
	"clippet/internal/store"
	"clippet/internal/transcode"
	"clippet/web/static"
)

// The audio half of a transcode (DESIGN.md §20 erratum 328).
//
// An audio clip's stored original has already been stripped at ingest — a
// `-c copy` of its one audio stream into a fresh container, every tag, chapter,
// picture and encoder string left behind (transcode.StripAudio) — so what this
// run does is the tier table of transcode.MakeAudioPlan:
//
//   - the original plays (MP3, AAC in a faststart MP4, Opus/Vorbis in Ogg,
//     FLAC) → there is no Full, and the stored original IS the clip;
//   - it does not → the Full: a bit-exact FLAC proven by the decoded-PCM hash,
//     a 24-bit FLAC of a float source with the gain that keeps its peak under
//     full scale, or AAC 256k;
//   - ALWAYS the card copy: the AUDIOGRAM (§20 erratum 329) — the clip's own
//     waveform, its played bars turning amber left to right over its length,
//     under the sound, an H.264 + AAC MP4: the one thing every card path
//     already knows how to play.
//
// The poster is the same waveform as a PNG, with the mark in one corner and
// the duration in the other. There is no filmstrip, no GIF, no trim and no
// chosen frame for a clip with no picture: their routes answer `not_for_audio`
// before anything is queued.

// audioWork is one run's audio production, shared in shape by the transcode
// pipeline: everything it needs is a field, and it writes nothing but the
// files it is told to.
type audioWork struct {
	w        *worker
	ctx      context.Context
	log      *slog.Logger
	probe    *transcode.ProbeResult
	plan     transcode.AudioPlan
	src      string
	dir      string
	fullPath string
	light    string
	picture  string
	srcSize  int64
	setStage func(string)
	// maxSocial is SOCIAL_MAX_BYTES, which the card's sound is fitted to.
	maxSocial int64
}

// audioResult is what an audioWork produced: the measured Full (nil when the
// original plays) and the measured card copy with its size.
type audioResult struct {
	full      *transcode.OutputInfo
	lightInfo *transcode.OutputInfo
	lightSize int64
	// peaks is the waveform the stencil was drawn from, quantised: the poster
	// is drawn from the same array, and the row keeps it for the page.
	peaks []uint8
}

// fullCodecName is the ffprobe codec name a Full of this plan must carry.
func fullCodecName(plan transcode.AudioPlan) string {
	if plan.Full == transcode.AudioFullAAC {
		return "aac"
	}
	return "flac"
}

// audioMarkPNG is the mark the poster carries, for an instance with this
// INSTANCE_BADGE: the manifest icon from its one source (scripts/lib/mkicons.py
// draws it; it is never redrawn here), with the span the Discord card's
// thumbnail wears on this instance — grey on a badged instance, green otherwise
// (erratum 285; static.MarkIcon192For is the one choice, run 10).
func audioMarkPNG(badge string) ([]byte, error) {
	b, err := static.FS.ReadFile(static.MarkIcon192For(badge))
	if err != nil {
		return nil, fmt.Errorf("the mark: %w", err)
	}
	return b, nil
}

// produce makes the Full when the plan names one and the card copy always.
func (a *audioWork) produce() (audioResult, error) {
	var res audioResult
	// The ceiling every audio encode is held to (outputBudget's rule, §20 erratum
	// 46, with no plan to add): twice the source or MAX_UPLOAD_BYTES. The
	// duration an encode runs for is the CONTAINER's claim, which the uploader
	// controls; `-fs` stops a lie at a number the operator already accepted, and
	// a file that reached it fails rather than being published truncated.
	budget := outputBudget(a.srcSize, a.w.deps.Cfg.MaxUploadBytes, transcode.Plan{})
	if a.plan.Full != "" {
		a.setStage(stageEncode)
		// The room asked for is the plan's estimate (a FLAC of PCM is at most the
		// PCM's size, an AAC far less), never more than the budget the encode is
		// held to.
		if err := requireScratch(a.w, a.dir, satAdd(min(audioPlannedBytes(a.probe, a.plan), budget), a.w.deps.Cfg.ScratchMinFree)); err != nil {
			return res, err
		}
		gain := 0.0
		if a.plan.MeasurePeak {
			peak, err := a.w.runner.AudioPeak(a.ctx, a.src)
			if err != nil {
				return res, err
			}
			gain = transcode.FitGainDB(peak)
			a.log.Info("float source measured", "peak_db", peak, "gain_db", gain)
		}
		if err := a.w.runner.AudioFull(a.ctx, a.probe, a.plan, gain, a.src, a.fullPath, budget); err != nil {
			return res, err
		}
		if err := underBudget(a.fullPath, budget, "the audio Full"); err != nil {
			return res, err
		}
		a.setStage(stageVerify)
		info, err := a.w.runner.ProbeOutput(a.ctx, a.fullPath)
		if err != nil {
			return res, err
		}
		if err := transcode.CheckAudioOutput(info, transcode.AudioCheck{Codec: fullCodecName(a.plan)}); err != nil {
			return res, err
		}
		if a.plan.Lossless {
			// D1a's proof: the FLAC decodes to exactly the source's samples, or it
			// is not published under a name that says lossless.
			if err := a.w.runner.SamePCM(a.ctx, a.src, a.fullPath); err != nil {
				return res, err
			}
		}
		res.full = info
	}
	a.setStage(stageLight)
	// The waveform, from the stored original: one streamed decode, a few
	// hundred bytes of peaks out (§20 erratum 329). The stencil is drawn from
	// them BEFORE the encode — a single process that drew it from the sound it
	// is also encoding would have to buffer the whole sound first.
	peaks, err := a.w.runner.WavePeaks(a.ctx, a.src, a.probe, transcode.WaveBars)
	if err != nil {
		return res, err
	}
	res.peaks = peaks
	if err := transcode.WritePNG(transcode.WaveStencil(peaks), a.picture); err != nil {
		return res, err
	}
	// The card's sound comes from the best file this run has: the Full when there
	// is one (a float source's carries the gain that keeps it from clipping), the
	// stored original otherwise. It is carried as it is when it is already AAC
	// and fits (D6a), and encoded to AAC otherwise.
	in, codec, inSize := a.src, firstCodec(a.probe), a.srcSize
	if res.full != nil {
		in, codec = a.fullPath, res.full.AudioCodec
		if st, err := os.Stat(a.fullPath); err == nil {
			inSize = st.Size()
		}
	}
	copyAudio := transcode.AudioLightCopies(codec, a.probe.AudioChannels, inSize, a.probe.DurationUS, a.maxSocial)
	bitrate := transcode.AudioLightBitrateK(a.maxSocial, a.probe.DurationUS)
	t0 := time.Now()
	if err := a.w.runner.AudioLight(a.ctx, a.probe, a.picture, in, a.light, bitrate, copyAudio, budget); err != nil {
		return res, err
	}
	encodeSec := time.Since(t0).Seconds()
	if err := underBudget(a.light, budget, "the audio card copy"); err != nil {
		return res, err
	}
	info, err := a.w.runner.ProbeOutput(a.ctx, a.light)
	if err != nil {
		return res, err
	}
	if err := transcode.CheckTierOutput(info, transcode.TierCheck{Codecs: []string{transcode.FullCodecH264},
		Width: transcode.WaveFrameW, Height: transcode.WaveFrameH}); err != nil {
		return res, err
	}
	if info.AudioStreams != 1 || info.AudioCodec != "aac" {
		return res, &transcode.ErrPermanent{Err: fmt.Errorf("bad_output: the audio card copy has %d audio streams (%q), want one aac",
			info.AudioStreams, info.AudioCodec)}
	}
	if err := a.w.runner.Verify(a.light); err != nil {
		return res, err
	}
	st, err := os.Stat(a.light)
	if err != nil {
		return res, &transcode.ErrPermanent{Err: fmt.Errorf("bad_output: the light copy is unreadable: %w", err)}
	}
	res.lightInfo, res.lightSize = info, st.Size()
	// The encode's seconds per minute of sound, logged so an operator (and the
	// lane that measured it) can read the audiogram's real cost off the box.
	perMin := 0.0
	if a.probe.DurationUS > 0 {
		perMin = encodeSec / (float64(a.probe.DurationUS) / 60e6)
	}
	a.log.Info("audio card copy encoded", "bytes", st.Size(), "audio_kbps", bitrate, "audio_copied", copyAudio,
		"from", filepath.Base(in), "encode_s", math.Round(encodeSec*100)/100, "encode_s_per_audio_min", math.Round(perMin*100)/100)
	return res, nil
}

// underBudget fails a file that reached the encode's `-fs` ceiling: ffmpeg stops
// THERE and exits 0, so the file is a truncation, and a truncation is never
// published. A file that cannot be stat'ed is left to the probe that follows.
func underBudget(path string, budget int64, what string) error {
	st, err := os.Stat(path)
	if err != nil || budget <= 0 || st.Size() < budget {
		return nil
	}
	return &transcode.ErrPermanent{Err: fmt.Errorf("output_too_large: %s reached the %d bytes budgeted for it", what, budget)}
}

// audioPlannedBytes is an over-estimate of a Full's size from the probe: AAC at
// its bitrate, FLAC at the PCM it encodes (never larger). 0 is "unknown".
func audioPlannedBytes(p *transcode.ProbeResult, plan transcode.AudioPlan) int64 {
	if p == nil || p.DurationUS <= 0 {
		return 0
	}
	sec := float64(p.DurationUS) / 1e6
	if plan.Full == transcode.AudioFullAAC {
		return int64(sec * float64(transcode.AudioFullBitrateK) * 1000 / 8 * 1.1)
	}
	bits := max(p.AudioBits, 16)
	if plan.Full == transcode.AudioFullFLAC24 {
		bits = 24
	}
	return int64(sec * float64(p.AudioSampleRate) * float64(max(p.AudioChannels, 1)) * float64(bits) / 8)
}

// ---- the transcode pipeline's audio branch -------------------------------

// isAudio reports whether this run's source is sound.
func (j *jobRun) isAudio() bool { return j.probe != nil && j.probe.Kind == transcode.KindAudio }

// planAudio is probeSource's audio half: the tier decision, and the Full's
// path, key and media type (the bucket never holds a FLAC under a name that
// says mp4).
func (j *jobRun) planAudio() {
	j.audio = transcode.MakeAudioPlan(j.probe, transcode.FaststartMP4(j.srcPath))
	// The poster is a drawing, kept as a PNG under image/png (§20 erratum 329),
	// at the key a video's JPEG would have but for its extension.
	j.posterPath = filepath.Join(j.dir, "poster-"+j.r8+".png")
	j.posterKey = store.PNGPosterKey(j.w.deps.Cfg.S3.KeyPrefix, j.clip.ID, j.r8)
	j.tiers = transcode.TierPlan{Playability: transcode.Playability{Playable: j.audio.Playable, Reason: j.audio.Reason},
		FullNeeded: j.audio.Full != "", LightNeeded: true, CardTier: transcode.TierLight}
	if j.audio.Full != "" {
		j.outPath = filepath.Join(j.dir, "out-"+j.r8+"."+j.audio.FullExt)
		j.renditionKey = store.AudioRenditionKey(j.w.deps.Cfg.S3.KeyPrefix, j.clip.ID, j.r8, j.audio.FullExt)
		j.renditionCT = audioRenditionType(j.audio.FullExt)
	}
	j.log.Info("planned audio", "codec", firstCodec(j.probe), "format", j.probe.FormatName,
		"sample_rate", j.probe.AudioSampleRate, "bits", j.probe.AudioBits, "channels", j.probe.AudioChannels,
		"original_playable", j.audio.Playable, "why_not", j.audio.Reason, "full", j.audio.Full,
		"duration_us", j.probe.DurationUS)
}

// audioRenditionType is the media type an audio Full is stored and served as.
func audioRenditionType(ext string) string {
	if ext == "flac" {
		return "audio/flac"
	}
	return "audio/mp4"
}

func firstCodec(p *transcode.ProbeResult) string {
	if p == nil || len(p.AudioCodecs) == 0 {
		return ""
	}
	return p.AudioCodecs[0]
}

// produceAudio is produce's audio twin: the Full when it is needed, the card
// copy always, and the clip's own measurements — the sound's length and NO
// picture (width, height and rate stay NULL; the card copy's dimensions are on
// the Light columns, where every card copy's are).
func (j *jobRun) produceAudio() error {
	a := &audioWork{w: j.w, ctx: j.ctx, log: j.log, probe: j.probe, plan: j.audio,
		src: j.srcPath, dir: j.dir, fullPath: j.outPath, light: j.lightPath,
		picture: filepath.Join(j.dir, "stencil-"+j.r8+".png"), srcSize: j.clip.SourceSize, setStage: j.setStage,
		maxSocial: j.w.deps.Cfg.SocialMaxBytes}
	defer os.Remove(a.picture)
	res, err := a.produce()
	if err != nil {
		return err
	}
	if res.full != nil {
		j.out = res.full
	}
	j.peaks = res.peaks
	j.ready.Peaks = transcode.EncodePeaks(res.peaks)
	durMS := int64(math.Round(float64(j.probe.DurationUS) / 1000))
	j.measure(0, 0, durMS, transcode.Rational{}, true)
	j.lightOut = res.lightInfo
	j.ready.Light = db.SocialFields{Key: j.lightKey, Size: res.lightSize,
		Width: res.lightInfo.Width, Height: res.lightInfo.Height, FPSNum: res.lightInfo.FPS.Num, FPSDen: res.lightInfo.FPS.Den}
	return nil
}

// posterAudio draws the poster from the peaks this run read (§20 erratum 329):
// the waveform as a PNG, the mark, and the duration label.
func (j *jobRun) posterAudio() error {
	mark, err := audioMarkPNG(j.w.deps.Cfg.InstanceBadge)
	if err != nil {
		return err
	}
	var durMS int64
	if j.probe != nil {
		durMS = int64(math.Round(float64(j.probe.DurationUS) / 1000))
	}
	res, err := j.w.runner.AudioPoster(j.ctx, j.peaks, mark, j.posterPath, durMS)
	if err != nil {
		return err
	}
	j.ready.PosterWidth, j.ready.PosterHeight = res.Width, res.Height
	return nil
}

// tierFieldsAudio is tierFields for an audio clip: the Original's verdict and
// codec, the Full's state and codec (`flac`, `flac24`, `aac`), the card copy
// ready, and no picture anywhere.
func (j *jobRun) tierFieldsAudio() db.TierFields {
	f := db.TierFields{
		OriginalPlayable: j.audio.Playable,
		OriginalCodec:    firstCodec(j.probe),
		FullState:        db.TierSkipped,
		LightState:       db.TierSkipped,
	}
	if j.ready.Light.Key != "" {
		f.LightState = db.TierReady
	}
	if j.audio.Full != "" && j.mainProduced() {
		f.FullState, f.FullCodec = db.TierReady, j.audio.Full
	}
	return f
}
