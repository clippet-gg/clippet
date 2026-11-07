package transcode

import "math"

// AudioMode selects the audio filter chain.
type AudioMode int

const (
	AudioNone AudioMode = iota
	AudioSingle
	AudioMerge
)

// String names the mode for logs and probe.json.
func (m AudioMode) String() string {
	switch m {
	case AudioNone:
		return "none"
	case AudioSingle:
		return "single"
	case AudioMerge:
		return "merge"
	}
	return "unknown"
}

// PlanOptions is the fit box and fps cap (from config.TranscodeConfig).
// Zero values fall back to the §4 defaults (1280×720, 60).
type PlanOptions struct{ MaxW, MaxH, MaxFPS int }

// Default box and cap (§4 TRANSCODE_MAX_WIDTH/HEIGHT/FPS).
const (
	DefaultMaxW   = 1280
	DefaultMaxH   = 720
	DefaultMaxFPS = 60
)

func (o PlanOptions) withDefaults() PlanOptions {
	if o.MaxW <= 0 {
		o.MaxW = DefaultMaxW
	}
	if o.MaxH <= 0 {
		o.MaxH = DefaultMaxH
	}
	if o.MaxFPS <= 0 {
		o.MaxFPS = DefaultMaxFPS
	}
	return o
}

// Plan is the pure output of MakePlan.
type Plan struct {
	W, H           int
	FPS            Rational
	GOP, KeyintMin int
	AudioMode      AudioMode
	VideoIndex     int
	AudioIndexes   []int
	HasDuration    bool
	DurationUS     int64

	// PreRotate is the clockwise quarter turn (0, 90, 180, 270) the filter
	// graph applies before scaling. Non-zero only when the source's rotation
	// came from a legacy rotate tag: ffmpeg autorotates from the Display
	// Matrix side data alone, so a tag-only source would otherwise be
	// squeezed unrotated into the swapped W×H.
	PreRotate int

	// MatrixTurn is the clockwise quarter turn (0, 90, 180, 270) ffmpeg's own
	// autorotate gives a source whose rotation comes from a Display Matrix:
	// (360 − ProbeResult.Rotation) mod 360, since the probe speaks ffprobe's
	// counter-clockwise convention (rotation 90 → 270 clockwise → `transpose=2`,
	// pinned against autorotate by TestMatrixTurnMatchesAutorotate). The CPU
	// graph never reads it — autorotate turns those frames at the graph's input
	// and drops the matrix from the output — so no CPU argv changes. The CUDA
	// graph does (§20 erratum 352): autorotate cannot insert a `transpose` on
	// device frames, so the frames reached `scale_cuda` unturned (a 626×1234
	// phone frame squeezed into 1234×626) and the output kept the matrix for
	// players to turn a second time. With MatrixTurn set, the CUDA input gets
	// `-display_rotation:v 0`, `scale_cuda` scales to the CODED orientation, and
	// the turn runs on the CPU after `hwdownload`.
	MatrixTurn int

	// MatrixMirrored rides with MatrixTurn: the matrix mirrors the picture too,
	// which a quarter turn cannot reproduce, so HWAccelFor keeps the source on
	// the CPU graph (§20 erratum 352).
	MatrixMirrored bool

	// Tonemap is set when the source declares an HDR transfer function (PQ
	// or HLG) and the filter graph must therefore convert it to SDR before
	// scaling. False renders exactly the graph this package has always
	// rendered (§20 erratum 93).
	Tonemap bool

	// AssumePrimaries / AssumeMatrix ride with Tonemap: they say the source
	// named an HDR transfer but left its primaries / matrix UNKNOWN, so the
	// chain must state BT.2100's before the map runs. zscale has no path out
	// of an undescribed colorspace and fails the encode outright, so this is
	// not a refinement of the picture — it is whether the clip transcodes at
	// all (§20 erratum 93, amended). Meaningless when Tonemap is false.
	AssumePrimaries bool
	AssumeMatrix    bool

	// Anamorphic is set when the source's pixel aspect is not square, so
	// `setsar=1` in the video chain is doing real work rather than restating a
	// fact. It changes no CPU argv — that filter has always been emitted
	// unconditionally — and exists because `setsar` is a CPU filter and a CUDA
	// graph therefore cannot serve such a source (§20 erratum 216).
	Anamorphic bool

	// SourceCodec and SourcePixFmt are the probe's `codec_name` and `pix_fmt`
	// of the video stream, carried so HWAccelFor can refuse the CUDA path for a
	// source the hardware decoder cannot take (§20 erratum 247): `-hwaccel cuda`
	// on such a stream silently decodes in software, the frames then sit in
	// system memory, and `scale_cuda` fails the run with a zero-byte file. They
	// change no CPU argv. Empty (a plan built without a probe, as tests do)
	// means "unknown" and is not refused.
	SourceCodec  string
	SourcePixFmt string

	// Deinterlace is set when the source's `field_order` names an interlaced
	// stream, and it puts `yadif` at the head of the video chain (§20
	// erratum 125). Encoded as-is, such a source comes out probing
	// `progressive` with the comb still in the pixels — the metadata denies
	// what the picture is, so no player can correct it. False renders
	// exactly the graph this package has always rendered.
	Deinterlace bool
}

// MakePlan applies the §10.3 fit/even/fps rules:
//
//	long, short := max(Dw,Dh), min(Dw,Dh)
//	s := min(1, MaxW/long, MaxH/short)      // fit in EITHER orientation; never upscale
//	W := even(round(Dw·s)); H := even(round(Dh·s))   // even() FLOORS to even, min 2
//	fps := pickFPS(avg, r, MaxFPS), then capped at MaxFPS/1
//	GOP = 2·ceil(fps); KeyintMin = ceil(fps)
//
// It also decides the colour handling: a source whose transfer function is
// HDR (PQ or HLG) is tone mapped by the filter graph, because the rendition
// is 8-bit yuv420p tagged bt709 whatever the source said (§20 erratum 93);
// and a source whose `field_order` names fields rather than frames is
// deinterlaced, because the rendition is a progressive file (§20 erratum 125).
func MakePlan(p *ProbeResult, o PlanOptions) Plan {
	o = o.withDefaults()
	dw, dh := float64(p.DisplayW), float64(p.DisplayH)
	long, short := math.Max(dw, dh), math.Min(dw, dh)
	s := 1.0
	if long > 0 {
		s = math.Min(s, float64(o.MaxW)/long)
	}
	if short > 0 {
		s = math.Min(s, float64(o.MaxH)/short)
	}
	fps := pickFPS(p.FPS, p.RFPS, o.MaxFPS)
	if fps.Num > o.MaxFPS*fps.Den { // fps > MaxFPS, compared exactly
		fps = Rational{o.MaxFPS, 1}
	}
	plan := Plan{
		W:            even(int(math.Round(dw * s))),
		H:            even(int(math.Round(dh * s))),
		FPS:          fps,
		GOP:          2 * fps.Ceil(),
		KeyintMin:    fps.Ceil(),
		VideoIndex:   p.VideoIndex,
		AudioIndexes: append([]int(nil), p.AudioIndexes...),
		HasDuration:  p.HasDuration,
		DurationUS:   p.DurationUS,
		SourceCodec:  p.VideoCodec,
		SourcePixFmt: p.PixFmt,
	}
	if p.RotationFromTag {
		plan.PreRotate = p.Rotation
	} else if p.Rotation != 0 {
		plan.MatrixTurn = (360 - p.Rotation) % 360
	}
	plan.MatrixMirrored = p.RotationMirrored
	plan.Deinterlace = IsInterlacedFieldOrder(p.FieldOrder)
	plan.Anamorphic = p.SARNum != p.SARDen
	if plan.Tonemap = isHDRTransfer(p.ColorTransfer); plan.Tonemap {
		plan.AssumePrimaries = isUnknownColour(p.ColorPrimaries)
		plan.AssumeMatrix = isUnknownColour(p.ColorSpace)
	}
	if len(plan.AudioIndexes) > maxAudioInputs {
		plan.AudioIndexes = plan.AudioIndexes[:maxAudioInputs]
	}
	switch n := len(plan.AudioIndexes); {
	case n == 0:
		plan.AudioMode = AudioNone
	case n == 1:
		plan.AudioMode = AudioSingle
	default:
		plan.AudioMode = AudioMerge
	}
	return plan
}

// ForProducedFile is this plan for a PRODUCED file as its input — the remux
// (remux true) or a Full this package encoded (remux false) — rather than the
// upload the probe read. Both lay their streams out the same way, whatever the
// upload's order was: the video first, then at most one audio track (encode
// maps `[v]` then `[a]`; RemuxArgs maps the video index, then the one audio
// index). A Merge of two upload tracks is one track in the produced file, so it
// is read as Single.
//
// The picture's decisions differ between the two. The remux is the upload's own
// coded packets and carries its Display Matrix, so W, H, FPS and the rotation
// (MatrixTurn) hold for it unchanged. An encoded Full has already been through
// the whole head of the chain — turned, tone mapped, deinterlaced, square
// pixels — and carries no matrix (§20 erratum 352: the CUDA graph strips it as
// the CPU graph always did), so every one of those flags is OFF for it: a Light
// encoded from it must not turn it a second time.
//
// §20 erratum 351: before this, the Light copy encoded from the remux on the
// fast path carried the UPLOAD's indices, and a phone recording with its audio
// track first (audio 0, video 1) put `[0:1]` on the video pad of a file whose
// stream 1 is the audio — ffmpeg: "Stream specifier ':1' … matches no streams".
// Every such clip failed `permanent` on the Light pass; the Original played.
func (p Plan) ForProducedFile(remux bool) Plan {
	out := p
	out.VideoIndex = 0
	if !remux {
		out.PreRotate, out.MatrixTurn, out.MatrixMirrored = 0, 0, false
		out.Tonemap, out.AssumePrimaries, out.AssumeMatrix = false, false, false
		out.Deinterlace, out.Anamorphic = false, false
	}
	if p.AudioMode == AudioNone || len(p.AudioIndexes) == 0 {
		out.AudioIndexes = nil
		out.AudioMode = AudioNone
		return out
	}
	out.AudioIndexes = []int{1}
	out.AudioMode = AudioSingle
	return out
}

// vfrPreferNum/vfrPreferDen is how much higher r_frame_rate must be than
// avg_frame_rate before the plan believes the source is VARIABLE rate rather
// than merely rounded: 5/4, i.e. 25%. Below it the two rates are describing
// the same constant timeline (an mp4's r_frame_rate is a guess from the
// timebase and lands a percent or two off a fractional avg), and the average
// — the exact one — is the honest answer.
const (
	vfrPreferNum = 5
	vfrPreferDen = 4
)

// pickFPS chooses the rate the rendition is encoded at, from the source's
// average rate and its r_frame_rate (§20 erratum 93).
//
// `-fps_mode cfr` at the chosen `-r` resamples the whole timeline to it, so
// on a VARIABLE-rate source the average is the wrong rate to pick: a phone
// screen recording whose action runs at 30 fps and whose still menus run at
// ~10 reports `r_frame_rate=30/1, avg_frame_rate=3600/293` (12.29), and
// planning 12.29 throws away roughly three of every five frames of the part
// anyone wants to watch while duplicating frames of the part nobody does.
// r_frame_rate is what the fast section was actually captured at.
//
// It is only preferred when it is itself SANE — at or below the box's fps cap
// — because r_frame_rate is also where containers put nonsense: Matroska and
// WebM routinely report 1000/1 or 90000/1 (the timebase, not a rate), which
// would otherwise be capped straight to MaxFPS and make every mkv upload a
// 60 fps encode of a 30 fps source. Those fall through to the average, which
// is why the cap participates in the decision rather than only bounding it
// afterwards.
//
// A rate that is missing on either side is not a decision at all: whichever
// one exists is used, and 30/1 when neither does.
func pickFPS(avg, r Rational, maxFPS int) Rational {
	switch {
	case !avg.Valid() && !r.Valid():
		return defaultFPS
	case !avg.Valid():
		return r
	case !r.Valid():
		return avg
	}
	// r ≤ maxFPS && r > 1.25·avg, both in exact integer arithmetic.
	sane := int64(r.Num) <= int64(maxFPS)*int64(r.Den)
	higher := int64(vfrPreferDen)*int64(r.Num)*int64(avg.Den) > int64(vfrPreferNum)*int64(avg.Num)*int64(r.Den)
	if sane && higher {
		return r
	}
	return avg
}

// even floors x to an even number, never below 2.
func even(x int) int {
	x -= x % 2
	if x < 2 {
		return 2
	}
	return x
}

// OutputBitsPerPixel sizes the file a plan schedules. Measured libx264 output
// runs ~0.02 bits per encoded pixel (low motion) to ~0.15 (high motion); 0.2 is
// the ceiling with headroom, because both readers of the estimate want to err
// the SAME way — the scratch guard refuses to start rather than hit ENOSPC
// mid-encode (§20 erratum 42), and the tier plan over-plans a Light copy rather
// than flip `ready` on a card file that is too big (§20 erratum 235).
const OutputBitsPerPixel = 0.2

// PlannedOutputBytes bounds the file a plan schedules:
// W·H·fps·duration·OutputBitsPerPixel / 8.
//
// 0 means "not sizeable" — no duration, no frame rate or no dimensions to work
// from. math.MaxInt64 means the estimate itself overflowed, which is not a bound
// but the ABSENCE of one: a caller that sizes something from it must refuse such
// a plan instead (§20 erratum 48).
//
// It lives here rather than in the worker because two callers in two packages
// ask it, and one estimator asked twice cannot disagree with itself: the
// worker's scratch guard and output budget, and MakeTierPlan's question about
// the Full it has not encoded yet.
func PlannedOutputBytes(p Plan) int64 {
	if !p.HasDuration || p.DurationUS <= 0 || p.W <= 0 || p.H <= 0 || !p.FPS.Valid() {
		return 0
	}
	bytes := float64(p.W) * float64(p.H) * p.FPS.Float() * (float64(p.DurationUS) / 1e6) * OutputBitsPerPixel / 8
	if bytes >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(bytes)
}
