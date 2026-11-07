package transcode

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Quality tiers (DESIGN.md §17 #10, §20 errata 200–211).
//
// Until now every upload was re-encoded once, into a rendition capped at
// TRANSCODE_MAX_WIDTH × TRANSCODE_MAX_HEIGHT (1280×720 by default) and
// TRANSCODE_MAX_FPS. A 1440p60 capture — the shape this product exists to
// share — was therefore silently downscaled, and the file the owner got back
// was worse than the file they uploaded. There are now three tiers, and none
// of them is ever downscaled:
//
//   - ORIGINAL is the uploaded bytes. When the container and the streams are
//     something every current browser plays (OriginalPlayability below), it is
//     what the watch page streams: no encode at all, and the clip is ready as
//     fast as a poster can be taken. A container that only needs its `moov`
//     moved to the front — or an .mov that needs to be an .mp4 — is REMUXED
//     with `-c copy`, which rewrites no coded byte and is still the original.
//   - FULL exists only when the original is NOT web-playable. It is a faithful
//     re-encode at the source's own resolution and frame rate.
//   - LIGHT is the card copy: ≤1280×720, ≤30 fps, CRF 26 with a 2.5 Mbps
//     ceiling, H.264 always (erratum 121's measured fact). It is made when the
//     file a card would otherwise fetch is too big, too smooth or too large a
//     picture for a crawler to inline.
//
// The playability verdict is deliberately the SAME set of predicates
// DecideTrim uses to choose a stream copy (§17 #5): both questions are "does
// this file already have the shape the pipeline would have produced", and two
// independent answers to one question is how they drift apart.

// Tier names, as they appear in the DTO, the docs and the quality selector.
const (
	TierOriginal = "original"
	TierFull     = "full"
	TierLight    = "light"
)

// Tier states, as the DTO and GET …/progress report them. Skipped is "this
// tier is not needed", which is a finished answer and not a failure: an
// original that plays needs no Full, and a small calm file needs no Light.
const (
	TierStateReady    = "ready"
	TierStateEncoding = "encoding"
	TierStateFailed   = "failed"
	TierStateSkipped  = "skipped"
)

// Full-tier codecs (FULL_CODEC). H.264 is the default because it is the only
// codec every browser, every Discord client and every embedded player decodes;
// AV1 is offered because an operator with a modern audience halves the bytes.
const (
	FullCodecH264 = "h264"
	FullCodecAV1  = "av1"
)

// The Full tier's encoder settings (§20 erratum 204). They are constants and
// not knobs on purpose: "Full" is a promise about FIDELITY, and an operator who
// could set CRF 30 on it would be shipping a tier whose name lies. The instance's
// TRANSCODE_PRESET still chooses how much CPU an H.264 Full spends, and
// TRANSCODE_MAX_BITRATE is still available as a hard ceiling (off by default).
const (
	// FullCRF is five points tighter than the old rendition default of 23.
	// This file is the ONLY copy of a source the browser cannot play, so it is
	// the file the owner will download in a year; 18 is where libx264 stops
	// being distinguishable from its input on game footage.
	FullCRF = 18
	// FullAV1CRF is AV1's own scale, which is not H.264's: libsvtav1 at CRF 32
	// measures about as close to the source as libx264 at 18, at roughly half
	// the bytes.
	FullAV1CRF = 32
	// FullAV1Preset is libsvtav1's INTEGER effort (0 slowest … 13 fastest).
	// Eight is the encoder's own default balance and the fastest preset that
	// still beats x264 on bytes-per-quality; anything slower turns a two-minute
	// capture into a ten-minute job on the one CPU compose gives the app.
	FullAV1Preset = 8
)

// NormalizeFullCodec maps a configured FULL_CODEC value onto one this package
// implements, defaulting to H.264. config.Load refuses anything else outright;
// this is the belt for a row or a job that predates the variable.
func NormalizeFullCodec(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), FullCodecAV1) {
		return FullCodecAV1
	}
	return FullCodecH264
}

// FullCRFFor is the CRF of a Full encode in the given codec's own scale.
func FullCRFFor(codec string) int {
	if NormalizeFullCodec(codec) == FullCodecAV1 {
		return FullAV1CRF
	}
	return FullCRF
}

// The Light tier's own box and encoder settings (§20 erratum 121, unchanged
// numbers; the names follow the tier). SocialMaxFPS/SocialCRF and their
// siblings in social.go remain the spelling the code has used since that
// erratum — these are the tier's box.
const (
	// LightMaxW / LightMaxH are the frame a card is worth. A crawler renders
	// the file in a timeline at a few hundred pixels wide; 720p is already
	// more than it shows, and it is what erratum 121 measured.
	LightMaxW = 1280
	LightMaxH = 720
)

// FullMaxW / FullMaxH bound what the Full encode will attempt. It is a sanity
// ceiling and NOT a box the plan fits into: a source inside it is encoded at
// its own dimensions, and a source above it fails the clip with a plain reason
// rather than being downscaled (§20 erratum 202). 8K is far past anything a
// capture card or a phone writes, and it is where libx264's own level tables
// stop being meaningful.
const (
	FullMaxW = 7680
	FullMaxH = 4320
)

// Playability reasons. They are operator text — a log field and a line in
// probe.json — never a public failure, and they name the FILE's property
// rather than a browser: which browser refuses what changes, and the file
// does not.
const (
	PlayableYes           = ""
	NotPlayableContainer  = "container is not mp4/mov"
	NotPlayableCodec      = "video codec is neither h264 nor av1"
	NotPlayablePixFmt     = "pixel format is not 8-bit yuv420p"
	NotPlayableHDR        = "the source declares an HDR transfer function"
	NotPlayableInterlaced = "the source is interlaced"
	NotPlayableAudio      = "audio is not a single aac track"
	NotPlayableRotateTag  = "the rotation comes from a legacy rotate tag"
	NotPlayableAnamorphic = "the source has a non-square pixel aspect"
	NotPlayableNoDuration = "the source declares no duration"
	NotPlayableNoFPS      = "the source declares no usable frame rate"
	NotPlayableTooLarge   = "the picture is larger than this server encodes"
)

// Playability is the verdict on one upload.
//
// Playable=true says the coded bytes are already something a browser plays.
// Remux=true says the CONTAINER still has to be rewritten before they can be
// served — `moov` after `mdat` (which is what ffmpeg writes by default, so it
// is the common case), or a QuickTime media type Firefox will not touch — and
// that rewrite is `-c copy`: not one coded byte changes, so the result is
// still the Original tier and not a Full (§20 erratum 200).
type Playability struct {
	Playable bool
	Remux    bool
	Reason   string
}

// OriginalPlayability decides whether the uploaded file can be streamed as it
// stands (§20 erratum 200). `mediaType` is the clip's stored
// source_content_type and `faststart` is whether the file's own top-level
// boxes put `moov` before `mdat` (transcode.VerifyMP4 answers that without
// spawning anything).
//
// The table, and why each row is where it is. Every "no" here means the clip
// gets a Full tier instead, which is the honest outcome — never a worse
// picture, only a re-encode:
//
//		| property        | playable                | refused                          |
//		|-----------------|-------------------------|----------------------------------|
//		| container       | mp4 / mov / m4v         | matroska, webm, avi, mpegts, …   |
//		| media type      | video/mp4 → direct      | anything else → remux to mp4     |
//		| video codec     | h264, av1               | hevc, vp9, prores, mpeg4, …      |
//		| pixel format    | yuv420p, yuvj420p       | 10-bit, 4:2:2, 4:4:4, rgb        |
//		| colour transfer | SDR                     | PQ / HLG (needs a tone map)      |
//		| field order     | progressive / unknown   | tt, bb, tb, bt (needs yadif)     |
//		| audio           | none, or one aac track  | 2+ tracks (the merge is ours)    |
//		| rotation        | Display Matrix, or none | a legacy `rotate` tag            |
//		| pixel aspect    | square                  | anamorphic (needs setsar=1)      |
//		| duration        | declared and positive   | absent (a broken index)          |
//		| picture         | ≤ 7680×4320             | larger                           |
//		| faststart       | direct                  | remux (`-c copy +faststart`)     |
//		| metadata        | structural keys only    | remux (GPS/device/creation tags) |
//
//	  - Matroska and WebM are refused although a browser plays them: the file a
//	    Discord crawler fetches, the file `?download=1` hands over and the file
//	    an embed plays are all one URL, and that URL promises an mp4. Remuxing
//	    a webm's VP9 into mp4 is not a copy anybody should rely on.
//	  - HEVC is refused although Safari plays it: Chrome and Firefox on the
//	    desktop do not, so "playable" would mean "playable for some viewers",
//	    which is worse than a Full copy everyone can watch.
//	  - AV1 is accepted: Chrome 70+, Firefox 67+, Edge and Safari 17+ decode it
//	    from an mp4, and a source already in it is exactly the source nobody
//	    wants re-encoded.
//	  - Two audio tracks are refused because merging them is a FEATURE (§10.3
//	    step 2, maxAudioInputs): a browser plays the first track only, so
//	    "direct play" would silently drop the microphone.
//	  - A legacy `rotate` tag is refused for the reason Plan.PreRotate exists:
//	    ffmpeg autorotates from the Display Matrix alone, and so do browsers, so
//	    a tag-only source would play on its side.
func OriginalPlayability(p *ProbeResult, mediaType string, faststart bool) Playability {
	no := func(reason string) Playability { return Playability{Reason: reason} }
	switch {
	case p == nil:
		return no(NotPlayableContainer)
	case !isMP4Container(p.FormatName):
		return no(NotPlayableContainer)
	case p.VideoCodec != "h264" && p.VideoCodec != "av1":
		return no(NotPlayableCodec)
	case !isPlayablePixFmt(p.PixFmt):
		return no(NotPlayablePixFmt)
	case isHDRTransfer(p.ColorTransfer):
		return no(NotPlayableHDR)
	case IsInterlacedFieldOrder(p.FieldOrder):
		return no(NotPlayableInterlaced)
	case len(p.AudioIndexes) > 1, len(p.AudioIndexes) == 1 && audioCodec(p, 0) != "aac":
		return no(NotPlayableAudio)
	case p.Rotation != 0 && p.RotationFromTag:
		return no(NotPlayableRotateTag)
	case p.SARNum != p.SARDen:
		return no(NotPlayableAnamorphic)
	case !p.HasDuration || p.DurationUS <= 0:
		return no(NotPlayableNoDuration)
	case !p.FPS.Valid():
		return no(NotPlayableNoFPS)
	case p.DisplayW <= 0 || p.DisplayH <= 0:
		return no(NotPlayableContainer)
	case max(p.DisplayW, p.DisplayH) > FullMaxW || min(p.DisplayW, p.DisplayH) > FullMaxH:
		return no(NotPlayableTooLarge)
	}
	// The bytes play. What is left is whether the FILE can be served as it
	// stands — three questions about the container and none about the picture,
	// every one of them answered by rewriting the container around the same coded
	// packets:
	//
	//   - a `moov` at the end costs a player an extra round trip and some embedded
	//     ones give up (and it is what ffmpeg writes unless told otherwise);
	//   - a `video/quicktime` media type is one Firefox declines outright;
	//   - METADATA beyond the structural keys — a phone's GPS location, a camera's
	//     make and model, an editor's `artist` or `comment`, a `creation_time` —
	//     because this is the one branch that publishes a file WITHOUT rendering
	//     it, so those tags would become public on `/original`, on `/video.mp4`
	//     and in the file a crawler fetches. `-map_metadata -1 -map_chapters -1`
	//     is in RemuxArgs exactly so they never travel into a public object;
	//     making them a remux trigger is what stops the direct branch from being
	//     the hole in that rule (§20 erratum 200, amended).
	return Playability{Playable: true, Remux: !faststart ||
		strings.ToLower(strings.TrimSpace(mediaType)) != "video/mp4" || p.PrivateTags}
}

// isPlayablePixFmt reports whether a pixel format is 8-bit 4:2:0 — the one
// every browser decodes in hardware. `yuvj420p` is the same samples with a
// full-range flag, which ffmpeg still reports for some capture hardware.
func isPlayablePixFmt(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yuv420p", "yuvj420p":
		return true
	}
	return false
}

// TierPlanOptions is everything MakeTierPlan needs that is not in the probe.
type TierPlanOptions struct {
	// Box is the instance's own frame box (TRANSCODE_MAX_*). Since these tiers
	// it bounds the LIGHT copy only — the Full tier is the source's own size —
	// and it can only make Light smaller, never larger (LightPlanOptions).
	Box PlanOptions
	// FullCodec is FULL_CODEC: FullCodecH264 (the default) or FullCodecAV1.
	FullCodec string
	// LightMaxBytes is SOCIAL_MAX_BYTES: the size at or above which the card
	// gets its own copy. 0 turns the Light tier off entirely.
	LightMaxBytes int64
	// SourceSize and SourceMediaType describe the stored upload; Faststart is
	// its box order. They are the three facts the playability table needs that
	// ffprobe does not report.
	SourceSize      int64
	SourceMediaType string
	Faststart       bool
}

// TierPlan is what one upload needs: which tiers will exist, and the encode
// plan for each that has to be produced.
type TierPlan struct {
	Playability

	// FullNeeded is the negation of Playable, kept as its own field because it
	// is what every caller actually asks.
	FullNeeded bool
	// LightNeeded is the skip rule (§20 erratum 203) applied to the tier that
	// will serve the card.
	LightNeeded bool
	// CardTier is which tier a crawler ends up fetching: Light when one is
	// planned, otherwise the Original (playable) or the Full.
	CardTier string
	// Full and Light are the plans for those tiers, meaningful only when the
	// matching *Needed is true.
	Full, Light Plan
	// TooLarge is set when the source is past FullMaxW×FullMaxH and cannot be
	// played either: there is no tier this server can produce for it, and the
	// clip fails with NotPlayableTooLarge rather than being downscaled.
	TooLarge bool
}

// MakeTierPlan decides the tiers of one upload (§20 errata 200–203).
func MakeTierPlan(p *ProbeResult, o TierPlanOptions) TierPlan {
	play := OriginalPlayability(p, o.SourceMediaType, o.Faststart)
	t := TierPlan{Playability: play, FullNeeded: !play.Playable}
	if p == nil {
		return t
	}
	if t.FullNeeded {
		if max(p.DisplayW, p.DisplayH) > FullMaxW || min(p.DisplayW, p.DisplayH) > FullMaxH {
			t.TooLarge, t.Reason = true, NotPlayableTooLarge
			return t
		}
		t.Full = MakePlan(p, FullPlanOptions(p, o.Box))
	}
	// The shape AND the size of the file the card would be served if nothing
	// else were made: the Original's own picture and byte count when it plays,
	// the Full's otherwise. The pictures are the same numbers — the Full is not
	// downscaled — which is why one branch does for both.
	//
	// The Full's SIZE is the plan's own estimate (§20 erratum 235). It has not
	// been encoded yet, so the alternative was to leave the size term out and
	// re-ask it at the commit — which flipped `ready` and answered
	// `card_ready: true` on a Full that then turned out to be past the cap, with
	// the Light copy the tier exists for only queued behind it. The estimate is
	// deliberately an over-estimate (transcode.OutputBitsPerPixel is the measured
	// CEILING), because over-planning a Light copy costs one encode of a file
	// nobody watches while under-planning it hands a crawler the thing the tier
	// exists to prevent. 0 — a plan with no duration or no rate to work from — is
	// "unknown", and the shape alone decides, exactly as before.
	cardW, cardH, cardFPS := p.DisplayW, p.DisplayH, p.FPS
	cardSize, cardSizeKnown := o.SourceSize, play.Playable
	if t.FullNeeded {
		cardW, cardH, cardFPS = t.Full.W, t.Full.H, t.Full.FPS
		cardSize = PlannedOutputBytes(t.Full)
		cardSizeKnown = cardSize > 0
	}
	t.LightNeeded = LightWanted(o.LightMaxBytes, cardW, cardH, cardFPS, cardSize, cardSizeKnown)
	switch {
	case t.LightNeeded:
		t.Light = MakePlan(p, LightPlanOptions(o.Box))
		t.CardTier = TierLight
	case t.FullNeeded:
		t.CardTier = TierFull
	default:
		t.CardTier = TierOriginal
	}
	return t
}

// LightWanted is the skip rule, restated as a function of the tier that would
// serve the card (§20 erratum 203; erratum 121 wrote it about the rendition,
// which was then the only tier there was).
//
// A Light copy is made when the file a crawler would otherwise fetch is
//
//   - larger than 1280×720 — a card is rendered a few hundred pixels wide and
//     a crawler that has to download 4K to show it usually gives up;
//   - above 30 fps — a card autoplays muted in a timeline, and 60 fps is
//     precisely what makes a longer clip of the same footage too big;
//   - at or above SOCIAL_MAX_BYTES — the threshold that has always been here.
//
// The size term only applies when the size is KNOWN. For a playable original it
// is the upload's own; for a Full that has yet to be encoded it is the plan's
// estimate (§20 erratum 235) — an over-estimate on purpose — and it is unknown
// only when the plan has no duration or no frame rate to work from. The commit
// still re-asks the question about the file it actually produced (worker's
// lightWanted, the erratum-121 rule, unchanged): that is the belt, this is the
// braces.
//
// SOCIAL_MAX_BYTES = 0 turns the tier off whatever the shape.
func LightWanted(maxBytes int64, w, h int, fps Rational, sizeBytes int64, sizeKnown bool) bool {
	if maxBytes <= 0 {
		return false
	}
	switch {
	case w <= 0 || h <= 0:
		return false
	case max(w, h) > LightMaxW || min(w, h) > LightMaxH:
		return true
	// fps > SocialMaxFPS in exact integer arithmetic (30000/1001 is 29.97, and
	// is NOT above 30).
	case fps.Den > 0 && int64(fps.Num) > int64(SocialMaxFPS)*int64(fps.Den):
		return true
	}
	return sizeKnown && sizeBytes >= maxBytes
}

// FullPlanOptions is the box of a Full encode: one that cannot bind.
//
// MakePlan fits the display dims into (MaxW long, MaxH short) and never
// upscales, so handing it the source's own long and short sides makes the
// scale factor exactly 1 — the frame is the source's frame, which is the whole
// point of this tier.
//
// The frame-rate ceiling is the one number that is not simply removed, and the
// reason is pickFPS: `r_frame_rate` is where containers put nonsense (Matroska
// routinely reports 1000/1 or 90000/1, the timebase rather than a rate), and
// pickFPS uses the ceiling to tell a genuine high rate from that. So the
// ceiling is raised to the source's OWN average rate whenever that is higher
// than the operator's — it can no longer bind below what the source actually
// ran at, and a 240 fps phone capture is encoded at 240 — while a container
// claiming 1000 fps still falls back to the average it measured.
func FullPlanOptions(p *ProbeResult, box PlanOptions) PlanOptions {
	box = box.withDefaults()
	if p == nil {
		return box
	}
	long, short := max(p.DisplayW, p.DisplayH), min(p.DisplayW, p.DisplayH)
	if long > 0 && short > 0 {
		box.MaxW, box.MaxH = long, short
	}
	if ceil := p.FPS.Ceil(); ceil > box.MaxFPS {
		box.MaxFPS = ceil
	}
	return box
}

// LightPlanOptions is the box of the Light copy: the tier's own 1280×720/30,
// never widened by the instance's box and always narrowed by it.
//
// The minimum in each dimension is what makes an operator's TRANSCODE_MAX_*
// still mean something: somebody who asked for 854×480 renditions asked for a
// smaller card file too, and the Light copy has no business being larger or
// smoother than the instance's own ceiling.
func LightPlanOptions(box PlanOptions) PlanOptions {
	box = box.withDefaults()
	box.MaxW = min(box.MaxW, LightMaxW)
	box.MaxH = min(box.MaxH, LightMaxH)
	box.MaxFPS = min(box.MaxFPS, SocialMaxFPS)
	return box
}

// RemuxArgs renders the `-c copy` container rewrite of a playable original
// (§20 erratum 200): the same coded packets, `moov` moved to the front, in an
// mp4.
//
// It is CopyArgs without a window, and the differences from it are only the
// ones a whole-file copy implies: no `-ss`, no `-t`. Everything else is
// deliberately identical — one video stream and at most one audio stream
// mapped explicitly (the playability table has already refused anything else),
// subtitles and data streams dropped, and `-map_metadata -1` so the creation
// time, the device name and the GPS tags a phone writes do not travel into a
// public object.
func RemuxArgs(p *ProbeResult, in, out string) []string {
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-nostats", "-y"}
	args = append(args, inputArgs(in)...)
	args = append(args, "-map", "0:"+strconv.Itoa(p.VideoIndex))
	if len(p.AudioIndexes) == 1 {
		args = append(args, "-map", "0:"+strconv.Itoa(p.AudioIndexes[0]))
	}
	return append(args,
		"-sn", "-dn", "-map_metadata", "-1", "-map_chapters", "-1",
		"-c", "copy",
		"-movflags", "+faststart", "-f", "mp4", out,
	)
}

// Remux runs that rewrite. Outcomes match Copy's: nil on exit 0, ctx.Err()
// when the caller cancelled, ErrPermanent for a non-zero exit with the stderr
// tail — a container ffmpeg cannot rewrap is the file's fault.
func Remux(ctx context.Context, ffmpeg string, p *ProbeResult, in, out string) error {
	_, se, err := runCapture(ctx, ffmpeg, RemuxArgs(p, in, out)...)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		reason := exitReason("ffmpeg", err, se)
		if isExit(err) {
			return permanent("%s", reason)
		}
		return fmt.Errorf("%s", reason)
	}
	return nil
}
