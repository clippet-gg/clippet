package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"clippet/internal/config"
	"clippet/internal/transcode"
)

// auxTimeout bounds the runs that carry no timeout of their own — ffprobe
// on the source and on the output, and the poster extraction — so a source
// that makes a demuxer spin cannot park the worker's only goroutine (and
// the whole queue behind it) forever. Encode has its stall/hard timeouts.
// Ten minutes is far beyond any honest probe (seconds, even for a multi-GB
// file) and far below the 6h encode cap.
const auxTimeout = 10 * time.Minute

// errAuxTimeout is the cause of an auxiliary context that expired.
var errAuxTimeout = errors.New("auxiliary run timeout")

// ffmpegRunner is the production Runner: the transcode package bound to
// the resolved binaries and the operator's encoder knobs.
type ffmpegRunner struct {
	ffmpeg, ffprobe string
	opts            transcode.EncodeOptions
	auxTimeout      time.Duration
	// fullCodec is FULL_CODEC, kept beside opts rather than in it: opts is the
	// baseline every encode starts from and the codec belongs to exactly one of
	// them (EncodeFull). Putting it in opts would have made the LIGHT copy and a
	// trim's re-encode inherit it, which is the one thing erratum 121 forbids.
	fullCodec string
	// tagText is LIGHT_TAG_TEXT and tagFont the font resolved for it at boot
	// (§20 erratum 214). Either one empty means no tag, on every encode.
	tagText, tagFont string
	// posterFont is the face the poster's duration label is drawn with (§20
	// erratum 287), resolved from the image once, here. There is no variable
	// behind it and nothing to configure: unlike the LIGHT tag, whose text is
	// the operator's sentence, this label is one measurement of the file in the
	// product's own mono face. Empty — an image that ships neither this project's
	// font nor any of the LIGHT tag's — draws no label and costs a clip nothing.
	posterFont string
}

// NewFFmpegRunner builds the real Runner from config (paths are absolute
// after serve.go's LookPath, §20.1).
//
// `tag` is config.Config.LightTag, whose font serve.go resolved from the image
// the same way it resolved the binaries. It is passed separately rather than
// read off TranscodeConfig because it is not an ffmpeg knob: it is a product
// decision about one artefact (§20 erratum 214).
func NewFFmpegRunner(tc config.TranscodeConfig, tag config.LightTagConfig) Runner {
	return ffmpegRunner{
		ffmpeg:  tc.FFmpeg,
		ffprobe: tc.FFprobe,
		// MaxRateBits is TRANSCODE_MAX_BITRATE, 0 = off (§20 erratum 124);
		// `-bufsize` is left to EncodeOptions' own doubling.
		// Threads is the CPU quota (§20 erratum 215) and HWAccel the operator's
		// TRANSCODE_HWACCEL request (erratum 216) — both are baseline properties
		// of EVERY encode this runner performs, so they belong in opts.
		opts: transcode.EncodeOptions{Preset: tc.Preset, CRF: tc.CRF, StallTimeout: tc.StallTimeout,
			HardTimeout: tc.HardTimeout, MaxRateBits: tc.MaxBitrate,
			Threads: transcode.ResolveThreads(tc.Threads), HWAccel: tc.HWAccel},
		auxTimeout: auxTimeout,
		fullCodec:  tc.FullCodec,
		tagText:    tag.Text,
		tagFont:    tag.Font,
		// Resolved here and not in config, because there is no variable to
		// validate and nothing for an operator to decide: it is a filesystem
		// question about the image this binary is running in, asked once (§20
		// erratum 287).
		posterFont: transcode.FindPosterFont(),
	}
}

func (r ffmpegRunner) Probe(ctx context.Context, path string) (*transcode.ProbeResult, error) {
	actx, cancel := r.aux(ctx)
	defer cancel()
	p, err := transcode.Probe(r.ffprobe)(actx, path)
	return p, r.auxErr(actx, "probe", err)
}

func (r ffmpegRunner) Encode(ctx context.Context, p transcode.Plan, in, out string, maxOutputBytes int64, onProgress func(float64, bool)) error {
	o := r.opts
	o.MaxOutputBytes = maxOutputBytes
	return transcode.Encode(ctx, r.ffmpeg, p, in, out, o, onProgress)
}

// EncodeSocial is Encode with the §17 #3 card settings substituted for the
// operator's: the social CRF, the social VBV ceiling and the smaller audio
// bitrate, over the same preset and the same stall/hard timeouts. The plan
// caps the frame rate; this caps the bits.
//
// `tag` is the LIGHT tier's corner line (§20 erratum 214) — a zero LightTag,
// which is what every other caller in this package passes, renders the argv this
// method always rendered.
func (r ffmpegRunner) EncodeSocial(ctx context.Context, p transcode.Plan, in, out string,
	tag transcode.LightTag, maxOutputBytes int64, onProgress func(float64, bool)) error {
	o := transcode.SocialEncodeOptions(r.opts)
	o.MaxOutputBytes = maxOutputBytes
	o.Tag = tag
	return transcode.Encode(ctx, r.ffmpeg, p, in, out, o, onProgress)
}

// EncodeTiers is the two-output pass (§20 erratum 217): one decode, the FULL
// tier and the LIGHT copy, each with its own tier's settings — which is exactly
// what EncodeFull and EncodeSocial would each have used alone.
func (r ffmpegRunner) EncodeTiers(ctx context.Context, full, light transcode.Plan, in, fullOut, lightOut string,
	tag transcode.LightTag, maxOutputBytes int64, onProgress func(float64, bool)) error {
	main := r.opts
	main.Codec = transcode.NormalizeFullCodec(r.fullCodec)
	main.CRF = transcode.FullCRFFor(main.Codec)
	main.CQOffset = transcode.NVENCCQOffsetFull
	// The budget is the pair's, and it is enforced on the ONE process that
	// writes both of them.
	main.MaxOutputBytes = maxOutputBytes
	lightOpts := transcode.SocialEncodeOptions(r.opts)
	lightOpts.Tag = tag
	return transcode.EncodeTiers(ctx, r.ffmpeg, in,
		transcode.TierOutput{Plan: full, Path: fullOut, Opts: main},
		transcode.TierOutput{Plan: light, Path: lightOut, Opts: lightOpts}, onProgress)
}

// LightTagFor writes this clip's corner line and returns the pair drawtext
// reads. Nothing configured, no font in the image, or a template that asks for a
// quality nobody measured → a zero LightTag, which draws nothing.
func (r ffmpegRunner) LightTagFor(dir, name, lightQuality, bestQuality string) (transcode.LightTag, error) {
	if r.tagText == "" || r.tagFont == "" {
		return transcode.LightTag{}, nil
	}
	line := transcode.ExpandLightTag(r.tagText, lightQuality, bestQuality)
	path, err := transcode.WriteLightTagFile(dir, name, line)
	if err != nil || path == "" {
		return transcode.LightTag{}, err
	}
	return transcode.LightTag{FontFile: r.tagFont, TextFile: path}, nil
}

// EncodeFull is Encode with the FULL tier's settings substituted for the
// operator's CRF: the tier's own quality floor (CRF 18 for libx264, 32 for
// libsvtav1) in the encoder FULL_CODEC names, over the same preset and the same
// stall/hard timeouts (§20 erratum 204).
//
// TRANSCODE_CRF is deliberately NOT used here. "Full" is a promise about
// fidelity, and a knob that could set it to 30 would be shipping a tier whose
// name lies; TRANSCODE_MAX_BITRATE, which r.opts still carries, is the one
// operator ceiling that applies — and DEPLOY says out loud that setting it is
// how a Full stops being faithful.
func (r ffmpegRunner) EncodeFull(ctx context.Context, p transcode.Plan, in, out string, maxOutputBytes int64, onProgress func(float64, bool)) error {
	o := r.opts
	o.Codec = transcode.NormalizeFullCodec(r.fullCodec)
	o.CRF = transcode.FullCRFFor(o.Codec)
	o.CQOffset = transcode.NVENCCQOffsetFull
	o.MaxOutputBytes = maxOutputBytes
	return transcode.Encode(ctx, r.ffmpeg, p, in, out, o, onProgress)
}

// Remux rewrites a playable original's container with `-c copy` (§20 erratum
// 200). It carries no stall watchdog — a copy emits no progress to watch — so
// the auxiliary budget bounds it, exactly as it bounds CopyTrim, which is the
// same ffmpeg invocation over a window.
func (r ffmpegRunner) Remux(ctx context.Context, p *transcode.ProbeResult, in, out string) error {
	actx, cancel := r.aux(ctx)
	defer cancel()
	return r.auxErr(actx, "remux", transcode.Remux(actx, r.ffmpeg, p, in, out))
}

func (r ffmpegRunner) ProbeOutput(ctx context.Context, path string) (*transcode.OutputInfo, error) {
	actx, cancel := r.aux(ctx)
	defer cancel()
	info, err := transcode.ProbeOutput(actx, r.ffprobe, path)
	return info, r.auxErr(actx, "probe", err)
}

func (r ffmpegRunner) Verify(path string) error { return transcode.VerifyMP4(path) }

// Keyframes lists the source's keyframe timestamps (§17 #5). It is an
// auxiliary run under the same 10-minute budget as the probes: a demux of a
// large file is seconds, and a demuxer that spins must not park the loop.
func (r ffmpegRunner) Keyframes(ctx context.Context, path string, videoIndex int) ([]float64, error) {
	actx, cancel := r.aux(ctx)
	defer cancel()
	keys, err := transcode.Keyframes(actx, r.ffprobe, path, videoIndex)
	return keys, r.auxErr(actx, "probe", err)
}

// EncodeTrim is Encode over one window of the input: the same argv, the same
// stall and hard timeouts, the same output budget, with `-ss`/`-t` around it.
func (r ffmpegRunner) EncodeTrim(ctx context.Context, p transcode.Plan, win transcode.Window, in, out string, maxOutputBytes int64, onProgress func(float64, bool)) error {
	o := r.opts
	o.MaxOutputBytes = maxOutputBytes
	o.Trim = &win
	return transcode.Encode(ctx, r.ffmpeg, p, in, out, o, onProgress)
}

// CopyTrim remuxes one window without re-encoding. It carries no stall
// watchdog — a copy emits no progress to watch — so the auxiliary budget is
// its bound, as it is for the poster.
func (r ffmpegRunner) CopyTrim(ctx context.Context, t transcode.TrimPlan, p *transcode.ProbeResult, in, out string) error {
	actx, cancel := r.aux(ctx)
	defer cancel()
	return r.auxErr(actx, "trim_copy", transcode.Copy(actx, r.ffmpeg, t, p, in, out))
}

// GIF renders the preview's two palette passes (§17 #6). Its bounds are the
// package's own — GIFMaxBytes and GIFHardTimeout — plus the operator's stall
// timeout, which means the same thing here as it does for an encode: no
// progress for that long is a run that will never finish. The caller sizes
// nothing: a preview that needed a bigger budget would not be a preview.
func (r ffmpegRunner) GIF(ctx context.Context, p transcode.GIFPlan, in, palette, out string) error {
	return transcode.MakeGIF(ctx, r.ffmpeg, p, in, palette, out, transcode.GIFOptions{
		StallTimeout:   r.opts.StallTimeout,
		HardTimeout:    transcode.GIFHardTimeout,
		MaxOutputBytes: transcode.GIFMaxBytes,
		// The CPU quota, exactly as every encode gets it (§20 erratum 215):
		// two filter passes are not free, and this runner's opts already carry
		// the resolved number.
		Threads: r.opts.Threads,
	}, nil)
}

// Filmstrip renders the trim scrubber's sprite sheet (§20 erratum 220) under a
// bound of its OWN rather than the auxiliary one: the pass decodes a whole
// rendition, which is heavier than a probe and lighter than an encode, and
// FilmstripHardTimeout is the number that describes it. Everything else about
// the run — the byte budget, the tile geometry, the JPEG quality — belongs to
// the transcode package, because a filmstrip that needed a caller's opinion
// about any of them would not be a filmstrip.
func (r ffmpegRunner) Filmstrip(ctx context.Context, p transcode.FilmstripPlan, in, out string) (transcode.FilmstripResult, error) {
	actx, cancel := context.WithTimeoutCause(ctx, transcode.FilmstripHardTimeout, errAuxTimeout)
	defer cancel()
	res, err := transcode.MakeFilmstrip(actx, r.ffmpeg, p, in, out)
	if err != nil && errors.Is(err, context.DeadlineExceeded) && errors.Is(context.Cause(actx), errAuxTimeout) {
		// OUR budget expired, not the caller's: the file is at fault (§11), so
		// the ending is permanent and the scrubber keeps its plain track.
		return res, &transcode.ErrPermanent{Err: fmt.Errorf("filmstrip_timeout: not finished within %s", transcode.FilmstripHardTimeout)}
	}
	return res, err
}

// ProbeGIF counts the produced GIF's frames under the auxiliary budget.
func (r ffmpegRunner) ProbeGIF(ctx context.Context, path string) (*transcode.GIFInfo, error) {
	actx, cancel := r.aux(ctx)
	defer cancel()
	info, err := transcode.ProbeGIF(actx, r.ffprobe, path)
	return info, r.auxErr(actx, "probe", err)
}

// Poster takes the card image, and since §20 erratum 287 burns the clip's
// duration into its top-right corner. The label's duration is the one this pass
// was already given — the measured length of the file the frame comes from,
// which IS the clip's — so nothing new has to be plumbed for it; `has` false is
// an unmeasured file and draws no label rather than claiming 0:00.
func (r ffmpegRunner) Poster(ctx context.Context, in, out string, durUS int64, has bool, videoEndSec float64,
	box transcode.PosterBox) (transcode.PosterResult, error) {
	actx, cancel := r.aux(ctx)
	defer cancel()
	res, err := transcode.Poster(actx, r.ffmpeg, in, out, durUS, has, videoEndSec, box, r.posterLabel(durationMSOf(durUS, has)))
	return res, r.auxErr(actx, "poster", err)
}

// posterLabel pairs the image's font with a duration in milliseconds. Either
// half missing is a zero label, which draws nothing.
func (r ffmpegRunner) posterLabel(durMS int64) transcode.PosterLabel {
	return transcode.PosterLabel{FontFile: r.posterFont, DurationMS: durMS}
}

// durationMSOf converts the poster pass's microseconds to the label's
// milliseconds. An unmeasured duration is 0, which the label reads as "say
// nothing".
func durationMSOf(durUS int64, has bool) int64 {
	if !has || durUS <= 0 {
		return 0
	}
	return durUS / 1000
}

// PosterAt takes the ONE frame an owner chose, at `seconds` on the file's
// own timeline, under the same auxiliary budget the poster search gets. It
// has no ladder and rejects nothing: somebody asked for that frame (ROADMAP
// #5, §20 erratum 129).
func (r ffmpegRunner) PosterAt(ctx context.Context, in, out string, seconds float64, durMS int64) (transcode.PosterResult, error) {
	actx, cancel := r.aux(ctx)
	defer cancel()
	res, err := transcode.PosterAt(actx, r.ffmpeg, in, out, seconds, r.posterLabel(durMS))
	return res, r.auxErr(actx, "poster", err)
}

// PosterFrameSeconds runs the poster's selection pass alone, under the same
// auxiliary budget the poster itself gets. It returns a timestamp whatever
// happens — the plain seek when the run fails or the budget expires — so the
// caller has nothing to classify: a preview that starts a second and a half
// early is a worse preview, not a failed job.
func (r ffmpegRunner) PosterFrameSeconds(ctx context.Context, in string, durUS int64, has bool, videoEndSec float64) float64 {
	actx, cancel := r.aux(ctx)
	defer cancel()
	return transcode.PosterFrameSeconds(actx, r.ffmpeg, in, durUS, has, videoEndSec)
}

// AudioFull writes an audio clip's Full (§20 erratum 328) under the encode's
// own hard timeout: a FLAC or AAC of an hour of sound is a minute of work, and
// there is no progress stream to watch, so the stall watchdog has nothing to
// read — the hard bound is what stops a demuxer that spins.
func (r ffmpegRunner) AudioFull(ctx context.Context, p *transcode.ProbeResult, plan transcode.AudioPlan, gainDB float64, in, out string, maxOutputBytes int64) error {
	hctx, cancel := r.hard(ctx)
	defer cancel()
	return r.hardErr(hctx, "encode", transcode.RunAudio(hctx, r.ffmpeg, transcode.AudioFullArgs(p, plan, gainDB, in, out, maxOutputBytes, r.opts.Threads)))
}

// AudioLight writes the audio card copy — the audiogram (§20 erratum 329) — in
// two passes: the encode into a sibling file, then the copy that drops the one
// tag the encode cannot (transcode.CleanMP4Args). The sibling is removed
// whatever happens.
func (r ffmpegRunner) AudioLight(ctx context.Context, p *transcode.ProbeResult, stencil, in, out string, bitrateK int, copyAudio bool, maxOutputBytes int64) error {
	hctx, cancel := r.hard(ctx)
	defer cancel()
	enc := out + ".enc.mp4"
	defer os.Remove(enc)
	if err := transcode.RunAudio(hctx, r.ffmpeg, transcode.AudioLightArgs(p, stencil, in, enc, bitrateK, copyAudio, maxOutputBytes, r.opts.Threads)); err != nil {
		return r.hardErr(hctx, "encode", err)
	}
	return r.hardErr(hctx, "encode", transcode.RunAudio(hctx, r.ffmpeg, transcode.CleanMP4Args(enc, out)))
}

// WavePeaks is one decode of the whole file into the peaks reader, under the
// auxiliary budget, as a probe is: seconds for anything a person uploads.
func (r ffmpegRunner) WavePeaks(ctx context.Context, in string, p *transcode.ProbeResult, n int) ([]uint8, error) {
	actx, cancel := r.aux(ctx)
	defer cancel()
	h, err := transcode.WavePeaks(actx, r.ffmpeg, in, p, n)
	return h, r.auxErr(actx, "peaks", err)
}

// AudioPoster draws the waveform poster and labels it with the image's font,
// under the auxiliary budget (the label is one frame of drawtext).
func (r ffmpegRunner) AudioPoster(ctx context.Context, heights []uint8, mark []byte, out string, durMS int64) (transcode.PosterResult, error) {
	actx, cancel := r.aux(ctx)
	defer cancel()
	res, err := transcode.AudioPoster(actx, r.ffmpeg, heights, mark, out, r.posterLabel(durMS))
	return res, r.auxErr(actx, "poster", err)
}

// AudioPeak and SamePCM are decodes of the whole file under the auxiliary
// budget, as a probe is: seconds for anything a person uploads.
func (r ffmpegRunner) AudioPeak(ctx context.Context, in string) (float64, error) {
	actx, cancel := r.aux(ctx)
	defer cancel()
	peak, err := transcode.PeakDB(actx, r.ffmpeg, in)
	return peak, r.auxErr(actx, "peak", err)
}

func (r ffmpegRunner) SamePCM(ctx context.Context, a, b string) error {
	actx, cancel := r.aux(ctx)
	defer cancel()
	return r.auxErr(actx, "pcm_hash", transcode.SamePCM(actx, r.ffmpeg, a, b))
}

// hard derives an audio encode's context: the operator's hard timeout.
func (r ffmpegRunner) hard(ctx context.Context) (context.Context, context.CancelFunc) {
	if r.opts.HardTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeoutCause(ctx, r.opts.HardTimeout, errAuxTimeout)
}

// hardErr is auxErr for the hard bound: OUR timeout is the file's fault.
func (r ffmpegRunner) hardErr(ctx context.Context, what string, err error) error {
	if err != nil && errors.Is(err, context.DeadlineExceeded) && errors.Is(context.Cause(ctx), errAuxTimeout) {
		return &transcode.ErrPermanent{Err: fmt.Errorf("%s_timeout: not finished within %s", what, r.opts.HardTimeout)}
	}
	return err
}

// aux derives the bounded context of an auxiliary run (zero = unbounded).
func (r ffmpegRunner) aux(ctx context.Context) (context.Context, context.CancelFunc) {
	if r.auxTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeoutCause(ctx, r.auxTimeout, errAuxTimeout)
}

// auxErr maps our own expired budget onto a permanent failure (the file is
// at fault, §11) and returns a caller's cancellation unchanged.
func (r ffmpegRunner) auxErr(ctx context.Context, what string, err error) error {
	if err != nil && errors.Is(err, context.DeadlineExceeded) && errors.Is(context.Cause(ctx), errAuxTimeout) {
		return &transcode.ErrPermanent{Err: fmt.Errorf("%s_timeout: not finished within %s", what, r.auxTimeout)}
	}
	return err
}
