package transcode

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EncodeOptions are the libx264 knobs and timeouts. A zero StallTimeout or
// HardTimeout disables that guard.
type EncodeOptions struct {
	Preset                    string
	CRF                       int
	StallTimeout, HardTimeout time.Duration

	// StallClock is the stall watchdog's clock (§20 erratum 331). nil — every
	// production caller; nothing outside a test sets it — is the wall clock.
	StallClock Clock

	// MaxOutputBytes caps what one run may write (§20 erratum 46). The
	// caller budgets it; the run is killed with a permanent
	// output_too_large the first time ffmpeg reports having written past
	// it. Zero disables the cap.
	MaxOutputBytes int64

	// Trim, when non-nil, restricts the encode to one window of the input
	// (§17 #5). Nil — every transcode — renders exactly the argv this
	// package has always rendered.
	Trim *Window

	// MaxRateBits is the VBV ceiling in bits per second, rendered as
	// `-maxrate` with `-bufsize` = BufSizeBits (§20 errata 121, 124).
	// Zero — the default for the main rendition — renders neither option,
	// so CRF alone decides the bitrate exactly as it always has. It is the
	// one knob that bounds a high-motion clip's peak rate, which is what a
	// player buffering over a phone connection actually feels.
	MaxRateBits int64
	// BufSizeBits is the VBV buffer, meaningless without MaxRateBits and
	// defaulted to 2×MaxRateBits when left zero: a one-second ceiling with a
	// two-second buffer is the ordinary streaming shape.
	BufSizeBits int64

	// AudioBitrate is the `-b:a` value ("96k"). Empty is the §10.3 default
	// (128k), so every existing caller renders the argv it always did.
	AudioBitrate string

	// Codec is the VIDEO encoder: "" or FullCodecH264 renders `-c:v libx264`
	// exactly as this package always has, FullCodecAV1 renders `-c:v
	// libsvtav1` (§20 erratum 204, FULL_CODEC). It is meaningful only for the
	// Full tier — the main rendition of a source the browser cannot play —
	// because Light is H.264 always (erratum 121) and a remux encodes nothing.
	Codec string

	// Threads is the `-threads` / `-filter_threads` value (§20 erratum 215).
	// Zero renders neither option, which is ffmpeg's own "decide for
	// yourself" — and inside a CPU-quota'd container what it decides is wrong,
	// which is why the worker always sets this. transcode.ResolveThreads is
	// where the number comes from.
	Threads int

	// HWAccel is TRANSCODE_HWACCEL (§20 erratum 216): "" renders the CPU argv,
	// HWAccelCUDA asks for the NVENC path. It is a REQUEST, not a decision —
	// HWAccelFor refuses it for any plan a CUDA graph cannot serve, and the
	// argv falls back to the CPU one on the same binary.
	HWAccel string

	// Tag is the LIGHT copy's burned-in corner line (§20 erratum 214). It is
	// set by exactly one caller, the Light encode; a zero LightTag renders
	// nothing, so every other argv in this package is byte-for-byte what it
	// always was.
	Tag LightTag

	// CQOffset is how far above CRF this tier's NVENC quantizer has to sit to
	// land on the same BYTES (§20 erratum 216). It is meaningless without
	// HWAccel, and 0 means NVENCCQOffsetDefault — the two tiers set their own
	// measured values (NVENCCQOffsetFull / NVENCCQOffsetLight).
	CQOffset int
}

// videoCodecArgs renders the encoder selection and its two rate/effort knobs.
//
// libx264 keeps the argv it has always had, `-profile:v high` included: that
// profile is what every browser, every Discord client and every set-top
// decoder implements, and stating it stops x264 from picking `high10` off a
// pixel format we have already forced to 8-bit.
//
// libsvtav1 takes an INTEGER preset (0 slowest … 13 fastest), so the
// operator's TRANSCODE_PRESET — an x264 word — cannot be passed through and
// FullAV1Preset is used instead; there is no profile option worth stating
// (Main is the only one an 8-bit 4:2:0 stream can be). `-svtav1-params
// tune=0` asks the encoder to optimise for how the picture LOOKS rather than
// for PSNR, which is what a clip somebody watches wants.
func (o EncodeOptions) videoCodecArgs() []string {
	if o.Codec == FullCodecAV1 {
		return []string{"-c:v", "libsvtav1", "-preset", strconv.Itoa(FullAV1Preset),
			"-crf", strconv.Itoa(o.CRF), "-svtav1-params", "tune=0"}
	}
	return []string{"-c:v", "libx264", "-preset", o.Preset, "-crf", strconv.Itoa(o.CRF), "-profile:v", "high"}
}

// audioRateArg is the -b:a value of an encode: the caller's, or §10.3's.
func (o EncodeOptions) audioRateArg() string {
	if o.AudioBitrate != "" {
		return o.AudioBitrate
	}
	return audioBitrate
}

// vbv renders the -maxrate/-bufsize pair, or nothing at all when no ceiling
// was asked for. A bufsize the caller left unset is twice the ceiling.
func (o EncodeOptions) vbv() []string {
	if o.MaxRateBits <= 0 {
		return nil
	}
	buf := o.BufSizeBits
	if buf <= 0 {
		buf = min(o.MaxRateBits, math.MaxInt64/2) * 2
	}
	return []string{"-maxrate", strconv.FormatInt(o.MaxRateBits, 10), "-bufsize", strconv.FormatInt(buf, 10)}
}

// Window is the [Start, Start+Duration) slice of an input a trim keeps, in
// seconds. It is rendered as `-ss` in FRONT of `-i` and `-t` on the output:
//
//   - `-ss` before the input is the fast seek — ffmpeg jumps to the keyframe
//     at or before Start and decodes forward from there — and since ffmpeg
//     2.1 it is also the ACCURATE one for a re-encode: the frames between
//     that keyframe and Start are decoded and discarded, so the first frame
//     written is the one the caller asked for. Behind `-i` it would be exact
//     too, but only by decoding the whole file up to Start.
//   - `-t` on the OUTPUT, not `-to`: input `-ss` shifts the output timeline
//     to zero, so an output `-to` would mean the same thing as `-t` while
//     reading as if it were an absolute point in the source. A duration says
//     what it means.
//
// For a STREAM COPY the same `-ss` is not accurate at all — nothing is
// decoded, so ffmpeg starts at the keyframe at or BEFORE Start, up to a
// whole GOP early. That is why Start is snapped to the keyframe itself
// before a copy is chosen (see DecideTrim), never taken from the request.
type Window struct{ Start, Duration float64 }

// secArg renders a seconds value for ffmpeg: fixed point, microsecond
// resolution, never scientific notation.
func secArg(s float64) string { return strconv.FormatFloat(s, 'f', 6, 64) }

// stallDisableFrac: once out_time reaches this share of the known duration
// ffmpeg is in the faststart remux tail, which emits no progress lines; from
// there only the hard timeout applies (§10.3).
const stallDisableFrac = 0.99

// Audio encoding constants (§10.3 step 4).
const (
	audioRate    = "48000"
	audioBitrate = "128k"
)

// colourStamp closes every video chain by declaring, on the frame itself,
// what an 8-bit yuv420p H.264 rendition is: Rec.709 (§20 erratum 93).
//
// It has to be a FILTER. The output options `-color_primaries` and
// `-color_trc` were measured NOT to reach the encoder on ffmpeg 7.1.1 — the
// encoder takes those two from the frame the filter graph hands it, so a
// source's tags propagate through `scale` and out into the file — and an
// HDR10 upload therefore produced an 8-bit stream still labelled
// bt2020/smpte2084. `setparams` writes the frame properties the encoder then
// copies, and it is the LAST link in the chain so nothing can overwrite it.
// (Of the three, only `-colorspace` is honoured as an option, through the
// buffersink's colorspace negotiation; the argv keeps all three anyway.)
//
// This is a RELABEL, not a conversion, and for the HDR path the conversion
// has already happened in tonemapChain. The one case it papers over is a
// genuinely non-709 SDR source — an SD bt470bg/smpte170m capture — which is
// now tagged 709 without its matrix being converted, a small hue shift on a
// shape this product effectively never sees (game capture is 709 or
// untagged, and untagged was already being played as 709). Converting those
// properly would mean a zscale on every encode; the honest tag is the better
// trade for now.
const colourStamp = ",setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709"

// deinterlaceFilter is the head of the video chain of an interlaced source
// (§20 erratum 125).
//
// It is FIRST — before the tone map, the rotate and the scale — because it
// is the only filter in the chain that reads the source's own field
// structure: yadif interpolates the missing lines of each field from its
// neighbours, and every later filter would have averaged two moments of time
// together before it ever got the chance. Scaling a woven frame is how a
// comb becomes a permanent blur.
//
// `deint=interlaced` processes only frames FLAGGED interlaced and passes the
// rest through untouched, which matters for the mixed files real capture
// hardware writes (a progressive menu spliced into interlaced gameplay);
// combined with the plan's own `field_order` test it means a progressive
// upload is never touched by two independent conditions rather than one. The
// default mode (`send_frame`) emits one frame per input frame, so the
// timeline `-r`/`-fps_mode cfr` then resamples is the source's own.
const deinterlaceFilter = "yadif=deint=interlaced,"

// deinterlacePrefix is that filter when the plan says the source is
// interlaced, and nothing at all otherwise.
func (p Plan) deinterlacePrefix() string {
	if p.Deinterlace {
		return deinterlaceFilter
	}
	return ""
}

// The pad labels of a rendered graph. A single-output encode uses the two bare
// ones this package has always used; a two-output encode numbers them.
const (
	videoPad      = "v"
	audioPad      = "a"
	videoPadMain  = "v0"
	videoPadLight = "v1"
	audioPadMain  = "a0"
	audioPadLight = "a1"
)

// FilterComplex renders the -filter_complex graph for the plan's audio mode
// (§10.3): the video chain always (with the deinterlacer, then the HDR tone
// map, then the explicit turn of a tag-rotated source, in front, and the
// Rec.709 stamp last), then none | single | merge audio.
//
// It is the CPU, single-output, untagged graph — the one every caller but the
// LIGHT encode and the two-output transcode renders, byte for byte as it always
// has been.
func (p Plan) FilterComplex() string {
	return p.filterComplex(HWAccelNone, LightTag{})
}

// filterComplex is FilterComplex with the two things the tiers added: which
// accelerator the video chain runs on, and the tag to burn into it.
func (p Plan) filterComplex(accel string, tag LightTag) string {
	var b strings.Builder
	b.WriteString(p.videoChain(accel, tag, videoPad))
	b.WriteString(p.audioChain(""))
	return b.String()
}

// videoChain renders one video branch of a graph, ending in the named pad.
//
// The CPU chain is the one this package has always rendered — deinterlace, tone
// map, pre-rotate, lanczos scale, square pixels, 8-bit 4:2:0, the Rec.709 stamp
// — with the tag, when there is one, as the very last link so nothing can
// overwrite the pixels it drew.
//
// The CUDA chain is one filter (§20 erratum 216). Everything the CPU chain puts
// in front of the scale is a CPU-only filter, which is exactly why HWAccelFor
// refuses a plan that needs any of them; the colour stamp moves to a bitstream
// filter on the output; and a tag — `drawtext`, also CPU-only — brings the
// frames back to system memory with `hwdownload` after the scale has run on the
// card, so the hardware decode and scale are still what they were for. A source
// turned by its Display Matrix downloads the same way for its `transpose`
// (Plan.cudaBranch, §20 erratum 352).
//
// hwAccelNVENCOnly renders the CPU chain, byte for byte: that fallback moves the
// DECODE off the card and nothing else (§20 erratum 265), and the chain that
// feeds a CPU decode is the one this package has always rendered.
func (p Plan) videoChain(accel string, tag LightTag, pad string) string {
	if cudaGraph(accel) {
		return padOf(p.VideoIndex) + p.cudaBranch(tag) + "[" + pad + "]"
	}
	return fmt.Sprintf("[0:%d]%s%s%sscale=%d:%d:flags=lanczos,setsar=1,format=yuv420p%s%s[%s]",
		p.VideoIndex, p.deinterlacePrefix(), p.hdrPrefix(), rotateFilter(p.PreRotate),
		p.W, p.H, colourStamp, drawtextFilter(tag, p.H), pad)
}

// audioChain renders the audio half of a graph, ending in `[a]` — or, when tail
// is set, in whatever that appends (the two-output graph's asplit). It is
// empty for a plan with no audio at all, in which case both outputs are `-an`.
func (p Plan) audioChain(tail string) string {
	var b strings.Builder
	switch p.AudioMode {
	case AudioSingle:
		fmt.Fprintf(&b, ";[0:%d]aresample=%s%s", p.AudioIndexes[0], audioRate, audioTail(tail))
	case AudioMerge:
		for i, idx := range p.AudioIndexes {
			fmt.Fprintf(&b, ";[0:%d]aresample=%s,aformat=channel_layouts=stereo[a%d]", idx, audioRate, i)
		}
		b.WriteString(";")
		for i := range p.AudioIndexes {
			fmt.Fprintf(&b, "[a%d]", i)
		}
		fmt.Fprintf(&b, "amix=inputs=%d:duration=longest:normalize=0,alimiter=limit=0.95:level=false:latency=true%s",
			len(p.AudioIndexes), audioTail(tail))
	}
	return b.String()
}

// audioTail closes the audio chain: the single `[a]` pad, or the caller's own
// ending.
func audioTail(tail string) string {
	if tail == "" {
		return "[" + audioPad + "]"
	}
	return tail
}

// tonemapChain is the HDR→SDR prefix of the video chain (§20 erratum 93).
// Read left to right:
//
//   - `zscale=t=linear:npl=100` undoes the source's transfer function into
//     LINEAR light, normalized so that 100 nits — an SDR display's peak — is
//     1.0. Tone mapping is a curve on light, and PQ/HLG code values are not
//     light; without this step the "conversion" is arithmetic on the wrong
//     numbers.
//
//   - `format=gbrpf32le` gives the tonemap filter the 32-bit float RGB it
//     requires. It is not a colour decision, it is the filter's input format.
//
//   - `tonemap=tonemap=mobius:desat=0` compresses the highlights into the
//     range a 100-nit display has. Mobius is near-IDENTITY below its knee and
//     only rolls the top off, which is what game capture needs: an HDR frame
//     is mostly SDR-level midtones with a few bright highlights, and an
//     operator that compresses everything darkens the picture it was supposed
//     to rescue. `desat=0` because the default desaturation pass washes the
//     picture out, which is half of the complaint being fixed.
//
//     The operator was MEASURED, not assumed (mean/peak luma of SMPTE bars
//     through the real chain on the shipped 7.1.1 build; the SDR encode of
//     the same bars is YAVG 103.7 / YMAX 254, and the untone-mapped HDR is
//     the defect being fixed):
//
//     | chain            | 100-nit grade | 1000-nit grade |
//     |------------------|---------------|----------------|
//     | today (no map)   |  79.7 / 134   |  119.4 / 186   |
//     | hable, desat=0   |  74.0 / 161   |  126.7 / 239   |
//     | reinhard,desat=0 |  95.2 / 194   |  138.5 / 236   |
//     | mobius, desat=0  | 100.9 / 207   |  141.7 / 236   |
//
//     Hable — the operator this fix was first specified with — makes the
//     reference clip DARKER than shipping nothing at all, because ffmpeg's
//     tonemap cannot see the source's peak (zscale drops the mastering
//     display and MaxCLL side data on the way to linear light, so
//     `ff_determine_signal_peak` falls back to its fixed default) and Hable
//     then compresses a 100-nit signal as if it were twelve times brighter.
//     Mobius is the operator that survives that blindness, and it clips
//     nothing at either grade.
//
//   - `zscale=p=bt709:t=bt709:m=bt709:r=tv` converts primaries, transfer and
//     matrix to Rec.709 and back to limited (TV) range — the shape the
//     rendition claims.
//
//   - `format=yuv420p` returns to the encoder's pixel format.
//
// It goes in FRONT of the rotate and the scale on purpose: the map is a
// per-pixel curve on the full-resolution linear signal, and scaling first
// would average HDR code values as if they were light.
const tonemapChain = "zscale=t=linear:npl=100,format=gbrpf32le," +
	"tonemap=tonemap=mobius:desat=0," +
	"zscale=p=bt709:t=bt709:m=bt709:r=tv,format=yuv420p,"

// tonemapFilter is the chain when the plan says the source is HDR, and
// nothing at all otherwise — an SDR source renders the graph byte for byte
// as it always has.
func tonemapFilter(on bool) string {
	if on {
		return tonemapChain
	}
	return ""
}

// hdrAssumedPrimaries / hdrAssumedMatrix are what a PQ or HLG source that
// left those fields UNSET must be taken to have meant. Both transfers are
// defined by BT.2100, whose container is BT.2020 primaries with the
// non-constant-luminance matrix; there is no other gamut a consumer HDR
// capture is graded in.
const (
	hdrAssumedPrimaries = "bt2020"
	hdrAssumedMatrix    = "bt2020nc"
)

// hdrInputTags states, on the frame, the colour fields an HDR source left
// UNKNOWN — and without them the tone map does not merely guess, it REFUSES
// (§20 erratum 93, amended).
//
// zscale converts between fully described colorspaces. A source tagged only
// on the transfer — which is the ordinary shape of a screen or console
// capture that names its transfer and leaves the rest unspecified — makes
// both zscales in tonemapChain fail with "code 3074: no path between
// colorspaces", and ffmpeg exits non-zero, so the clip fails PERMANENTLY.
// Before the tone map shipped the same file transcoded (untagged, untouched),
// which makes this a regression that the erratum's own fixture — a fully
// tagged HDR10 file — could not see. Measured on 7.1.1: zscale's own
// `pin=`/`tin=`/`min=` input overrides do NOT fix it; `setparams` in front of
// the chain does, and on a fully tagged source the two outputs are byte
// identical, which is why this is emitted per FIELD and only for the fields
// the probe found empty.
func hdrInputTags(assumePrimaries, assumeMatrix bool) string {
	switch {
	case assumePrimaries && assumeMatrix:
		return "setparams=color_primaries=" + hdrAssumedPrimaries + ":colorspace=" + hdrAssumedMatrix + ","
	case assumePrimaries:
		return "setparams=color_primaries=" + hdrAssumedPrimaries + ","
	case assumeMatrix:
		return "setparams=colorspace=" + hdrAssumedMatrix + ","
	}
	return ""
}

// hdrPrefix is the whole HDR head of the video chain: the assumed input tags
// first (they describe the frame the map is about to read), then the map.
func (p Plan) hdrPrefix() string {
	if !p.Tonemap {
		return ""
	}
	return hdrInputTags(p.AssumePrimaries, p.AssumeMatrix) + tonemapFilter(true)
}

// rotateFilter is the filter prefix for a clockwise quarter turn, in the
// legacy rotate-tag convention (rotate=90 → turn 90° clockwise to display;
// transpose=1 is clockwise, transpose=2 counter-clockwise). Anything but a
// quarter turn renders nothing.
func rotateFilter(clockwise int) string {
	switch clockwise {
	case 90:
		return "transpose=1,"
	case 180:
		return "hflip,vflip,"
	case 270:
		return "transpose=2,"
	}
	return ""
}

// encodeHeader is the global half of every instrumented argv: the quiet flags,
// the progress pipe, and — since §20 erratum 215 — the thread count.
//
// Both `-filter_threads` and `-filter_complex_threads` are set because they are
// two options: the first sizes a simple `-vf` graph's pool, the second a
// `-filter_complex` one, and this package renders the second. Setting both
// costs nothing and means a future simple-graph caller is pinned too.
func encodeHeader(threads int) []string {
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-nostats", "-progress", "pipe:1", "-y"}
	if threads > 0 {
		n := strconv.Itoa(threads)
		args = append(args, "-filter_threads", n, "-filter_complex_threads", n)
	}
	return args
}

// outputArgs renders ONE output of an encode: from its mapped pads to the file
// name. It is what makes a two-output argv (DualEncodeArgs) and a one-output
// argv provably the same encode written twice — every option a tier is encoded
// with is here and nowhere else.
//
// vPad/aPad name the graph pads this output consumes.
func (p Plan) outputArgs(o EncodeOptions, accel, vPad, aPad, out string) []string {
	args := []string{"-map", "[" + vPad + "]"}
	if p.AudioMode == AudioNone {
		args = append(args, "-an")
	} else {
		args = append(args, "-map", "["+aPad+"]")
	}
	args = append(args, "-sn", "-dn", "-map_metadata", "-1", "-map_chapters", "-1")
	if o.Trim != nil {
		args = append(args, "-t", secArg(o.Trim.Duration))
	}
	args = append(args, "-r", p.FPS.String(), "-fps_mode", "cfr")
	if nvencOut(accel) {
		args = append(args, nvencCodecArgs(o.CRF, o.CQOffset)...)
		// The Rec.709 stamp of a CUDA encode. `-colorspace bt709` is the one of
		// the three colour OPTIONS ffmpeg 7 honours, and honouring it means
		// negotiating the buffersink's colorspace — which inserts a CPU
		// `auto_scale` a CUDA graph cannot connect to and kills the run (§20
		// erratum 216). The bitstream filter writes the same three code points
		// into the SPS VUI and touches no pixel.
		//
		// The NVENC-only fallback (§20 erratum 265) takes the same two lines: its
		// graph is the CPU one, whose `setparams` already states 709 on the frame,
		// and the bitstream filter then states it in the SPS as well — the two
		// agree (primaries 1, transfer 1, matrix 1), so the file is stamped once
		// however the frames reached the encoder. The three colour OPTIONS stay
		// off it: it is still an NVENC output, and `-colorspace` is negotiated
		// before anybody knows which decoder ran.
		args = append(args, "-bsf:v", h264MetadataBSF)
	} else {
		args = append(args, o.videoCodecArgs()...)
		args = append(args,
			"-pix_fmt", "yuv420p",
			// The rendition is 8-bit yuv420p H.264, so it is Rec.709 and says so
			// — UNCONDITIONALLY, whatever the source declared (§20 erratum 93).
			// An HDR10 upload used to produce an 8-bit stream still labelled
			// bt2020nc/smpte2084: players trusted the label and applied a PQ→SDR
			// transform to a signal that had already been truncated, and the clip
			// played dark. Of these three, ffmpeg 7.1.1 honours only
			// `-colorspace`; the other two are taken from the frame, which is why
			// `colourStamp` closes the filter graph. They are stated here as well
			// because the argv should say what the file is, and because the one
			// that IS honoured is the one that drives buffersink negotiation.
			"-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709",
		)
	}
	if o.Threads > 0 {
		args = append(args, "-threads", strconv.Itoa(o.Threads))
	}
	// The VBV pair, when one was asked for: TRANSCODE_MAX_BITRATE on the main
	// rendition, the social rendition's own ceiling on that one (§20 errata
	// 121, 124). Neither set renders neither option.
	args = append(args, o.vbv()...)
	args = append(args, "-g", strconv.Itoa(p.GOP), "-keyint_min", strconv.Itoa(p.KeyintMin))
	if p.AudioMode != AudioNone {
		args = append(args, "-c:a", "aac", "-b:a", o.audioRateArg(), "-ar", audioRate, "-ac", "2")
	}
	return append(args,
		"-max_muxing_queue_size", "4096",
		"-movflags", "+faststart", "-f", "mp4", out,
	)
}

// EncodeArgs renders the single instrumented ffmpeg argv (§10.3 step 4),
// without the binary itself. No -level:v: x264 derives it from dims/fps.
func (p Plan) EncodeArgs(in, out string, o EncodeOptions) []string {
	accel, _ := HWAccelFor(p, o.HWAccel, o.Codec)
	return p.encodeArgs(in, out, o, accel)
}

// encodeArgs is EncodeArgs with the accelerator already decided, which is the
// one thing a run can change under it: the fallback of §20 erratum 265 re-renders
// this very argv on hwAccelNVENCOnly after NVDEC has refused the source.
func (p Plan) encodeArgs(in, out string, o EncodeOptions, accel string) []string {
	args := encodeHeader(o.Threads)
	if o.Trim != nil {
		args = append(args, "-ss", secArg(o.Trim.Start))
	}
	if cudaGraph(accel) {
		args = append(args, cudaInputArgs()...)
		args = append(args, p.cudaRotationInputArgs()...)
	}
	args = append(args, inputArgs(in)...)
	args = append(args, "-filter_complex", p.filterComplex(accel, o.Tag))
	return append(args, p.outputArgs(o, accel, videoPad, audioPad, out)...)
}

// TierOutput is one output of a two-output encode: which tier's plan, where it
// is written, and the encoder configuration of that tier.
type TierOutput struct {
	Plan Plan
	Path string
	Opts EncodeOptions
}

// DualEncodeArgs renders ONE ffmpeg argv that decodes the input once and writes
// BOTH the Full and the Light copy (§20 erratum 217).
//
// The measurement that justifies it: running the two encodes as two PROCESSES
// is worse than running them back to back — the first file appears 2.2× later
// than it would alone, because both processes decode the same source — while one
// process with two outputs costs 1.05–1.42× the Full alone instead of 1.6–1.9×,
// and produces files byte-identical to the separate runs. The decode and, for a
// Full (whose scale factor is exactly 1), effectively the scale as well are paid
// once.
//
// The graph is the shared head, a `split`, then each tier's own scale — the head
// is where the expensive part is, and the two tiers are different sizes so the
// scales cannot be shared. Audio is built once and `asplit`ed, so the two
// bitrates come off one resample.
//
// `main` and `light` MUST come from the same probe (they do: MakeTierPlan builds
// both), because the head reads main.Plan's video index, audio indexes and
// CPU-only filter flags for both branches.
func DualEncodeArgs(in string, main, light TierOutput) []string {
	return dualEncodeArgs(in, main, light, dualAccel(main, light))
}

// dualAccel is the ONE accelerator a two-output pass can run on.
func dualAccel(main, light TierOutput) string {
	accel, _ := HWAccelFor(main.Plan, main.Opts.HWAccel, main.Opts.Codec)
	if lightAccel, _ := HWAccelFor(light.Plan, light.Opts.HWAccel, light.Opts.Codec); lightAccel != accel {
		// One decode, one accelerator. The two tiers of one clip can only
		// disagree through FULL_CODEC=av1, which has no NVENC encoder here, and
		// the honest answer for a shared graph is the CPU both can render.
		accel = HWAccelNone
	}
	return accel
}

// dualEncodeArgs is DualEncodeArgs with the accelerator already decided (§20
// erratum 265: a refused CUDA run re-renders it on hwAccelNVENCOnly).
func dualEncodeArgs(in string, main, light TierOutput, accel string) []string {
	args := encodeHeader(main.Opts.Threads)
	if cudaGraph(accel) {
		args = append(args, cudaInputArgs()...)
		args = append(args, main.Plan.cudaRotationInputArgs()...)
	}
	args = append(args, inputArgs(in)...)
	args = append(args, "-filter_complex", dualFilterComplex(accel, main, light))
	args = append(args, main.Plan.outputArgs(main.Opts, accel, videoPadMain, audioPadMain, main.Path)...)
	return append(args, light.Plan.outputArgs(light.Opts, accel, videoPadLight, audioPadLight, light.Path)...)
}

// dualFilterComplex renders the two-output graph.
//
// The CPU head — deinterlace, tone map, pre-rotate — runs BEFORE the split, so
// each is paid once; the split is on the source's own frames, which is the
// cheapest place to put it. On the CUDA path the head is empty by construction
// (HWAccelFor refuses every plan that needs it), so the split is on the decoded
// device frames; a Display-Matrix turn runs per branch, after each branch's own
// scale and download (§20 erratum 352).
func dualFilterComplex(accel string, main, light TierOutput) string {
	var b strings.Builder
	p := main.Plan
	if cudaGraph(accel) {
		fmt.Fprintf(&b, "%ssplit=2[m][l]", padOf(p.VideoIndex))
		fmt.Fprintf(&b, ";[m]%s[%s]", main.Plan.cudaBranch(LightTag{}), videoPadMain)
		fmt.Fprintf(&b, ";[l]%s[%s]", light.Plan.cudaBranch(light.Opts.Tag), videoPadLight)
	} else {
		fmt.Fprintf(&b, "[0:%d]%s%s%ssplit=2[m][l]",
			p.VideoIndex, p.deinterlacePrefix(), p.hdrPrefix(), rotateFilter(p.PreRotate))
		fmt.Fprintf(&b, ";[m]scale=%d:%d:flags=lanczos,setsar=1,format=yuv420p%s[%s]",
			main.Plan.W, main.Plan.H, colourStamp, videoPadMain)
		fmt.Fprintf(&b, ";[l]scale=%d:%d:flags=lanczos,setsar=1,format=yuv420p%s%s[%s]",
			light.Plan.W, light.Plan.H, colourStamp, drawtextFilter(light.Opts.Tag, light.Plan.H), videoPadLight)
	}
	b.WriteString(p.audioChain(",asplit=2[" + audioPadMain + "][" + audioPadLight + "]"))
	return b.String()
}

// padOf renders the `[0:N]` input pad of a video stream.
func padOf(videoIndex int) string { return "[0:" + strconv.Itoa(videoIndex) + "]" }

// cudaBranch is one CUDA video branch, between its input pad and its output
// pad: the scale on the card, then — only when needed — the frames brought back
// to system memory for the CPU filters that follow.
//
// A source turned by its Display Matrix (MatrixTurn ≠ 0) is the one case where
// the card cannot do the whole picture (§20 erratum 352): ffmpeg's autorotate
// inserts its `transpose` only on SOFTWARE frames, so on device frames it does
// nothing and the matrix travels to the output. The encode therefore reads the
// input with `-display_rotation:v 0` (cudaRotationInputArgs: no autorotate, no
// matrix on the output — measured on 7.1.1 and 8.0.1), scales on the card to
// the CODED orientation (W and H swapped for a quarter turn, since the frames
// are still unturned), downloads, and turns with the same filter rotateFilter
// renders for that clockwise angle — the one autorotate itself would have
// inserted for a pure rotation (a mirrored matrix never gets here: HWAccelFor).
// The tag comes after the turn, so it sits in the corner the viewer sees.
func (p Plan) cudaBranch(tag LightTag) string {
	if p.MatrixTurn == 0 {
		return cudaScaleFilter(p) + cudaTagSuffix(tag, p.H)
	}
	w, h := p.W, p.H
	if p.MatrixTurn == 90 || p.MatrixTurn == 270 {
		w, h = h, w
	}
	return cudaScaleTo(w, h) + ",hwdownload,format=yuv420p," +
		strings.TrimSuffix(rotateFilter(p.MatrixTurn), ",") + drawtextFilter(tag, p.H)
}

// cudaRotationInputArgs is the input option of a CUDA encode of a matrix-turned
// source: `-display_rotation:v 0` replaces the stream's Display Matrix with the
// identity, so ffmpeg neither tries to autorotate the device frames nor writes
// the matrix into the output — the frames the graph turns itself are then the
// only turn the file carries (§20 erratum 352). `-noautorotate` would stop the
// first and NOT the second (measured: the output kept rotation 90). Empty for
// every other plan, so their argv is unchanged.
func (p Plan) cudaRotationInputArgs() []string {
	if p.MatrixTurn == 0 {
		return nil
	}
	return []string{"-display_rotation:v", "0"}
}

// cudaTagSuffix is what a tag adds to a CUDA branch: `drawtext` is a CPU
// filter, so the frames come back to system memory AFTER the card has decoded
// and scaled them — which is where the 9–11× came from — and the encoder
// uploads them again. No tag renders nothing at all, and the frames never leave
// the card.
func cudaTagSuffix(tag LightTag, frameH int) string {
	if draw := drawtextFilter(tag, frameH); draw != "" {
		return ",hwdownload,format=yuv420p" + draw
	}
	return ""
}

// Sentinel causes for the run context (distinguish our own timeouts from a
// caller's cancellation).
var (
	errStalled     = errors.New("encode stalled")
	errHardTimeout = errors.New("encode hard timeout")
	errTooLarge    = errors.New("encode output past its budget")
)

// Encode runs ffmpeg with progress parsing, stall and hard timeouts.
// onProgress receives (frac 0–1, indeterminate) at every -progress block;
// indeterminate is true when the plan has no duration. Outcomes: nil on
// exit 0; ctx.Err() when the caller cancelled (not a failure); ErrPermanent
// for a stall, the hard timeout, the output budget or a non-zero exit (with
// the stderr tail).
//
// A CUDA run that NVDEC refuses at initialisation is re-run here with the decode
// on the CPU before any of that is reported (§20 erratum 265).
func Encode(ctx context.Context, ffmpeg string, p Plan, in, out string, o EncodeOptions, onProgress func(frac float64, indeterminate bool)) error {
	accel, _ := HWAccelFor(p, o.HWAccel, o.Codec)
	return runHWFallback(ctx, ffmpeg, accel,
		func(a string) []string { return p.encodeArgs(in, out, o, a) }, p, instrument{
			stallTimeout: o.StallTimeout, hardTimeout: o.HardTimeout, maxOutputBytes: o.MaxOutputBytes,
			clock: o.StallClock,
		}, onProgress)
}

// EncodeTiers runs the two-output encode of §20 erratum 217: one decode, the
// FULL tier and the LIGHT copy, both files complete when it returns.
//
// The guards come from `main.Opts` — one process, one stall watchdog, one hard
// timeout — and `main.Opts.MaxOutputBytes` must be the budget of BOTH files
// together: ffmpeg's `total_size` progress key is not a per-output figure, so
// the runtime cap can only be a sum, and the caller still stats each file
// against its own budget afterwards (checkOutputSize).
//
// The progress arithmetic reads main.Plan, which is the right one either way:
// both outputs cover the same source timeline, so out_time/duration is the same
// fraction for both, and the Full is the one that finishes last.
func EncodeTiers(ctx context.Context, ffmpeg string, in string, main, light TierOutput, onProgress func(frac float64, indeterminate bool)) error {
	return runHWFallback(ctx, ffmpeg, dualAccel(main, light),
		func(a string) []string { return dualEncodeArgs(in, main, light, a) }, main.Plan, instrument{
			stallTimeout: main.Opts.StallTimeout, hardTimeout: main.Opts.HardTimeout,
			maxOutputBytes: main.Opts.MaxOutputBytes, clock: main.Opts.StallClock,
		}, onProgress)
}

// instrument is the guard set of one instrumented ffmpeg run: the stall
// watchdog, the hard timeout and the output-byte cap of §20 erratum 46,
// plus the label the first three failures are reported under. It is a type
// of its own because the GIF export (§17 #6) needs exactly these guards
// around a completely different argv, and a second copy of this loop would
// be a second chance to get the kill paths wrong.
type instrument struct {
	label                     string // "" = "encode"
	stallTimeout, hardTimeout time.Duration
	maxOutputBytes            int64
	// clock is the stall watchdog's (EncodeOptions.StallClock); nil = the wall clock.
	clock Clock
}

func (i instrument) name() string {
	if i.label == "" {
		return "encode"
	}
	return i.label
}

// runInstrumented runs one ffmpeg argv under those guards. p is read ONLY
// for the progress arithmetic (duration and output frame rate), so a caller
// that is not encoding a rendition passes whichever plan describes what
// ffmpeg is about to emit.
func runInstrumented(ctx context.Context, ffmpeg string, args []string, p Plan, in instrument, onProgress func(frac float64, indeterminate bool)) error {
	_, err := runInstrumentedRefusal(ctx, ffmpeg, args, p, in, onProgress)
	return err
}

// runInstrumentedRefusal is that run plus the ONE extra fact the CUDA fallback
// needs: whether ffmpeg died with the hardware decoder refusing the source at
// initialisation, before a byte of output existed (§20 erratum 265). refusal is
// the ffmpeg line that said so, and is empty for every other ending — a clean
// exit, a stall, a timeout, the byte budget, a cancellation, and a failure that
// happened once frames were already being written.
func runInstrumentedRefusal(ctx context.Context, ffmpeg string, args []string, p Plan, in instrument,
	onProgress func(frac float64, indeterminate bool)) (refusal string, err error) {
	if onProgress == nil {
		onProgress = func(float64, bool) {}
	}
	runCtx, cancelRun := context.WithCancelCause(ctx)
	defer cancelRun(nil)
	if in.hardTimeout > 0 {
		hardCtx, cancelHard := context.WithTimeoutCause(runCtx, in.hardTimeout, errHardTimeout)
		defer cancelHard()
		runCtx = hardCtx
	}
	cmd := newCommand(runCtx, ffmpeg, args...)
	stderr := newRing(stderrRingSize)
	cmd.Stderr = stderr
	watchdog := newStallWatchdog(in.stallTimeout, p.HasDuration, func() { cancelRun(errStalled) }, in.clock)
	tracker := newProgressTracker(p, onProgress, watchdog.observe)
	if in.maxOutputBytes > 0 {
		tracker.onSize = func(n int64) {
			if n > in.maxOutputBytes {
				cancelRun(errTooLarge)
			}
		}
	}
	cmd.Stdout = newLineWriter(tracker.line)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start ffmpeg: %w", err)
	}
	watchdog.start()
	waitErr := cmd.Wait()
	watchdog.stop()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// The budget is checked before the exit status: a run killed for size
	// whose ffmpeg still managed to exit 0 wrote the same runaway file.
	if errors.Is(context.Cause(runCtx), errTooLarge) {
		return "", permanent("output_too_large: the %s wrote past the %d bytes budgeted for it", in.name(), in.maxOutputBytes)
	}
	if waitErr == nil {
		return "", nil
	}
	switch cause := context.Cause(runCtx); {
	case errors.Is(cause, errStalled):
		return "", permanent("%s_stalled: no progress for %s", in.name(), in.stallTimeout)
	case errors.Is(cause, errHardTimeout):
		return "", permanent("%s_timeout: not finished within %s", in.name(), in.hardTimeout)
	}
	// Wait() has returned and os/exec has closed both copiers, so the ring and
	// the tracker are settled and can be read from this goroutine.
	//
	// The two readings of that stderr are deliberately different sizes (§20
	// erratum 289). The REASON an operator is shown is the last failureTail
	// bytes, because a reason is a log line. The CLASSIFICATION reads the WHOLE
	// ring: what ffmpeg prints on its way out after a refused hardware decode is
	// longer than a kilobyte, so the tail can open PAST the line that named the
	// cause — and a refusal this test cannot see is a clip that fails permanently
	// on the operator's hardware, which is the one thing erratum 265 exists to
	// prevent. It was the first production /clip that proved it.
	se := captureOf(stderr)
	if isExit(waitErr) {
		if !tracker.wrote() {
			refusal = hwInitRefusal(se.All())
		}
		return refusal, permanent("%s", exitReason("ffmpeg", waitErr, se))
	}
	return "", fmt.Errorf("%s", exitReason("ffmpeg", waitErr, se))
}

// ProgressLine is one parsed -progress record. TotalSize (bytes written so
// far) is a liveness signal only: it keeps growing while ffmpeg muxes a
// tail of audio past the last video frame, when out_time/frame stand still.
type ProgressLine struct {
	OutTimeUS  int64
	Frame      int64
	End        bool
	HasOutTime bool

	TotalSize    int64
	HasTotalSize bool
}

// ParseProgressLine parses one key=value line of ffmpeg -progress output.
// ok is false for keys the pipeline does not use (fps=, speed=, …) and for
// malformed values ("out_time_us=N/A"). out_time_ms is microseconds too
// (ffmpeg has always emitted it that way); out_time is HH:MM:SS.ffffff.
func ParseProgressLine(line string) (ProgressLine, bool) {
	key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
	if !ok {
		return ProgressLine{}, false
	}
	key, val = strings.TrimSpace(key), strings.TrimSpace(val)
	switch key {
	case "out_time_us", "out_time_ms":
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return ProgressLine{}, false
		}
		return ProgressLine{OutTimeUS: max(n, 0), HasOutTime: true}, true
	case "out_time":
		us, ok := parseClock(val)
		if !ok {
			return ProgressLine{}, false
		}
		return ProgressLine{OutTimeUS: us, HasOutTime: true}, true
	case "frame":
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil || n < 0 {
			return ProgressLine{}, false
		}
		return ProgressLine{Frame: n}, true
	case "total_size":
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil || n < 0 {
			return ProgressLine{}, false
		}
		return ProgressLine{TotalSize: n, HasTotalSize: true}, true
	case "progress":
		return ProgressLine{End: val == "end"}, true
	}
	return ProgressLine{}, false
}

// parseClock reads [-]HH:MM:SS[.ffffff] into microseconds (negative → 0).
func parseClock(s string) (int64, bool) {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	sec, err3 := strconv.ParseFloat(parts[2], 64)
	if err1 != nil || err2 != nil || err3 != nil || h < 0 || m < 0 || sec < 0 {
		return 0, false
	}
	if neg {
		return 0, true
	}
	total := (float64(h)*3600+float64(m)*60)*1e6 + sec*1e6
	return int64(total + 0.5), true
}

// ProgressFrac converts the fields gathered from one -progress block into
// the §10.3 fraction: out_time / duration when an out_time was seen, else
// frame / (fps_out · duration) using the OUTPUT rate, clamped to [0, 1];
// end → 1. With no known duration the fraction is indeterminate (0, or 1 at
// end).
func ProgressFrac(p Plan, outUS int64, hasOut bool, frame int64, end bool) (frac float64, indeterminate bool) {
	if end {
		return 1, !p.HasDuration
	}
	if !p.HasDuration || p.DurationUS <= 0 {
		return 0, true
	}
	switch {
	case hasOut:
		frac = float64(outUS) / float64(p.DurationUS)
	case frame > 0 && p.FPS.Valid():
		outFrames := p.FPS.Float() * float64(p.DurationUS) / 1e6
		if outFrames > 0 {
			frac = float64(frame) / outFrames
		}
	}
	return clamp01(frac), false
}

func clamp01(f float64) float64 {
	if f < 0 || f != f { // NaN
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// progressTracker folds -progress lines into per-block progress reports.
// A block is terminated by its progress=continue|end line; within a block
// the first out_time* key wins (ffmpeg emits out_time_us first). A block
// "advances" (feeds the stall watchdog) when out_time, the frame count or
// the bytes written grew — the last one covers an audio-only tail during
// which the video clock stands still. onSize, when set, receives every
// total_size the run reports (the output budget watches it).
type progressTracker struct {
	p          Plan
	onProgress func(float64, bool)
	onBlock    func(advanced bool, frac float64, end bool)
	onSize     func(total int64)

	blockOutUS   int64
	blockHasOut  bool
	blockFrame   int64
	blockSize    int64
	blockHasSize bool

	lastOutUS, lastFrame, lastSize int64
	lastFrac                       float64
	blocks                         int
	end                            bool
}

func newProgressTracker(p Plan, onProgress func(float64, bool), onBlock func(bool, float64, bool)) *progressTracker {
	if onBlock == nil {
		onBlock = func(bool, float64, bool) {}
	}
	return &progressTracker{p: p, onProgress: onProgress, onBlock: onBlock}
}

func (t *progressTracker) line(s string) {
	if pl, ok := ParseProgressLine(s); ok {
		if pl.HasOutTime && !t.blockHasOut {
			t.blockOutUS, t.blockHasOut = pl.OutTimeUS, true
		}
		if pl.Frame > 0 {
			t.blockFrame = pl.Frame
		}
		if pl.HasTotalSize {
			t.blockSize, t.blockHasSize = pl.TotalSize, true
		}
		if pl.End {
			t.end = true
		}
	}
	if strings.HasPrefix(s, "progress=") {
		t.flush()
	}
}

// flush emits one report for the block just closed and resets it.
func (t *progressTracker) flush() {
	advanced := t.blocks == 0 || (t.blockHasOut && t.blockOutUS > t.lastOutUS) || t.blockFrame > t.lastFrame ||
		(t.blockHasSize && t.blockSize > t.lastSize)
	frac, indeterminate := ProgressFrac(t.p, t.blockOutUS, t.blockHasOut, t.blockFrame, t.end)
	if frac < t.lastFrac {
		frac = t.lastFrac // monotone within a run
	}
	if t.blockHasOut {
		t.lastOutUS = t.blockOutUS
	}
	if t.blockFrame > t.lastFrame {
		t.lastFrame = t.blockFrame
	}
	if t.blockHasSize && t.blockSize > t.lastSize {
		t.lastSize = t.blockSize
	}
	t.lastFrac = frac
	t.blocks++
	size, hasSize := t.blockSize, t.blockHasSize
	t.blockOutUS, t.blockHasOut, t.blockFrame, t.blockSize, t.blockHasSize = 0, false, 0, 0, false
	t.onProgress(frac, indeterminate)
	t.onBlock(advanced, frac, t.end)
	if hasSize && t.onSize != nil {
		t.onSize(size)
	}
}

// wrote reports whether this run has produced anything at all: a moment on the
// output timeline or a byte in the file. It is the second half of the fallback
// test of §20 erratum 265 — a hardware decoder that refuses a source refuses it
// before the first frame, so a failure that arrives once output exists is a
// different failure and is never re-run. It is read after Wait(), when the
// stdout copier has finished and nothing else writes these fields.
func (t *progressTracker) wrote() bool { return t.lastOutUS > 0 || t.lastSize > 0 }

// stallArmed is the §10.3 rule: the stall timer runs only while the
// duration is known, progress=end has not been seen and frac < 0.99.
func stallArmed(hasDuration bool, frac float64, end bool) bool {
	return hasDuration && !end && frac < stallDisableFrac
}

// stallWatchdog fires once when no ADVANCING progress block arrives for
// timeout while armed (a report with the same out_time does not count —
// ffmpeg 7 keeps printing blocks while stuck). last is the instant of the
// latest advance; the loop re-checks it when the timer fires so an advance
// that lands in the same instant cannot be lost.
type stallWatchdog struct {
	timeout     time.Duration
	hasDuration bool
	fire        func()
	clock       Clock

	mu    sync.Mutex
	armed bool
	last  time.Time
	timer Timer
	done  chan struct{}
	once  sync.Once
}

// clock nil is the wall clock (EncodeOptions.StallClock).
func newStallWatchdog(timeout time.Duration, hasDuration bool, fire func(), clock Clock) *stallWatchdog {
	return &stallWatchdog{timeout: timeout, hasDuration: hasDuration, fire: fire, clock: clockOr(clock), done: make(chan struct{})}
}

// start arms the timer from process start: a source that never yields a
// first progress block stalls too.
func (w *stallWatchdog) start() {
	if w.timeout <= 0 || !stallArmed(w.hasDuration, 0, false) {
		return
	}
	w.mu.Lock()
	w.armed = true
	w.last = w.clock.Now()
	w.timer = w.clock.NewTimer(w.timeout)
	w.mu.Unlock()
	go w.loop()
}

func (w *stallWatchdog) loop() {
	for {
		select {
		case <-w.timer.C():
			w.mu.Lock()
			if !w.armed {
				w.mu.Unlock()
				return
			}
			// An advance that reset the timer while we were consuming
			// its value is honoured: wait out the remainder instead.
			if remain := w.timeout - w.clock.Now().Sub(w.last); remain > 0 {
				w.timer.Reset(remain)
				w.mu.Unlock()
				continue
			}
			w.mu.Unlock()
			w.fire()
			return
		case <-w.done:
			return
		}
	}
}

// observe is called per progress block from the stdout copier.
func (w *stallWatchdog) observe(advanced bool, frac float64, end bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer == nil {
		return
	}
	if !stallArmed(w.hasDuration, frac, end) {
		w.armed = false
		w.timer.Stop()
		return
	}
	if advanced {
		w.last = w.clock.Now()
		w.timer.Reset(w.timeout)
	}
}

func (w *stallWatchdog) stop() {
	w.once.Do(func() { close(w.done) })
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
	}
}
