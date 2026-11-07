// Package transcode wraps ffprobe/ffmpeg: probe, plan, encode with progress
// and stall detection, verify, poster (DESIGN.md §10, §18.3).
//
// Every external invocation is argv-based (no shell), reads stdin from the
// null device (ffmpeg additionally gets -nostdin), is bound to a context,
// receives SIGTERM on cancellation and is SIGKILLed after killDelay if it
// ignores that. Failures the file is responsible for — a non-zero ffprobe or
// ffmpeg exit, a stall, a hard timeout, a verify mismatch, a missing poster
// — are wrapped in ErrPermanent (§11); a cancelled context is returned as
// ctx.Err() and is not a failure at all; everything else (binary missing,
// I/O trouble) is a plain error the worker treats as transient.
package transcode

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Rational is an exact frame rate (30000/1001 stays {30000, 1001}).
type Rational struct{ Num, Den int }

// String renders the rate as "num/den", the form ffmpeg's -r accepts.
func (r Rational) String() string {
	return strconv.Itoa(r.Num) + "/" + strconv.Itoa(r.Den)
}

// Float is the rate as a float64 (0 when the denominator is 0).
func (r Rational) Float() float64 {
	if r.Den == 0 {
		return 0
	}
	return float64(r.Num) / float64(r.Den)
}

// Ceil is ⌈num/den⌉ in integer arithmetic (GOP math; never a float).
func (r Rational) Ceil() int {
	if r.Den <= 0 {
		return 0
	}
	return (r.Num + r.Den - 1) / r.Den
}

// Valid reports whether both terms are positive.
func (r Rational) Valid() bool { return r.Num > 0 && r.Den > 0 }

// ProbeResult is the rotation- and SAR-corrected view of a source.
//
// Rotation is the normalized quarter turn {0, 90, 180, 270}. It comes from
// the Display Matrix side data when present (ffprobe's counter-clockwise
// convention) and otherwise from the legacy rotate tag (Android's clockwise
// convention); only the swap of the display dims depends on it, and
// RotationFromTag tells the plan which source it came from: ffmpeg
// autorotates from the matrix alone, so a tag-only source must be turned
// explicitly in the filter graph (Plan.PreRotate). An angle that is not a
// quarter turn (within a degree) is reported as 0: ffmpeg does not turn such
// a frame into a swapped one either (§20 erratum 39).
type ProbeResult struct {
	// Kind is KindVideo or KindAudio (§20 erratum 328). An audio result has
	// VideoIndex -1, every picture field zero, exactly one entry in
	// AudioIndexes, and its shape in the Audio* fields below.
	Kind                                                         string
	VideoIndex                                                   int
	AudioIndexes                                                 []int
	CodedW, CodedH, SARNum, SARDen, Rotation, DisplayW, DisplayH int
	FPS                                                          Rational
	HasDuration                                                  bool
	DurationUS                                                   int64
	VideoCodec                                                   string
	Raw                                                          json.RawMessage

	// RotationFromTag is true when Rotation came from tags.rotate rather
	// than a Display Matrix entry.
	RotationFromTag bool

	// RotationMirrored is true when the Display Matrix also mirrors the picture
	// (a negative determinant). It changes no CPU argv — ffmpeg's autorotate
	// handles a mirror itself — and exists so the CUDA graph, which turns the
	// frames explicitly, can refuse what a quarter turn would get wrong (§20
	// erratum 352).
	RotationMirrored bool

	// PrivateTags is true when the container or one of its streams carries
	// metadata beyond the structural keys every ISO-BMFF file has
	// (hasPrivateTags): a phone's GPS location, a camera's make and model, an
	// editor's artist or comment, a creation time. Every path that RENDERS a
	// file strips them (`-map_metadata -1`), so exactly one decision reads it —
	// whether a playable upload may be served as its own bytes, or has to take
	// the `-c copy` remux that strips them (§20 erratum 200, amended).
	PrivateTags bool

	// Video and Audio are where those streams actually hold data, which is
	// NOT the same question as HasDuration/DurationUS: that pair describes the
	// CONTAINER, and a container routinely outlives its streams (two seconds
	// of video muxed with eight of audio is a legal, and common, file). A trim
	// window is checked against these before ffmpeg is asked to cut it (§20
	// erratum 78) — a window past the video's end would otherwise produce an
	// output with no video stream at all, i.e. a failure the OWNER can trigger
	// with a window the API had every reason to accept.
	Video StreamExtent
	Audio StreamExtent

	// PixFmt, FormatName and AudioCodecs describe the source exactly as it
	// is stored rather than as it will be displayed. Nothing in the encode
	// path needs them — every plan re-encodes to yuv420p/h264/aac whatever
	// they say — but the trim of §17 #5 does: a stream copy is only allowed
	// when the source ALREADY has the shape the pipeline would have
	// produced, and these three are the parts of that shape the display
	// geometry cannot answer. AudioCodecs is parallel to AudioIndexes.
	PixFmt      string
	FormatName  string
	AudioCodecs []string

	// RFPS is the source's r_frame_rate — ffprobe's guess at the rate the
	// timeline is a multiple of — and is invalid (both terms zero) when the
	// source declared none. FPS above is the AVERAGE rate; on a constant-rate
	// source the two agree, and on a variable-rate one (a phone screen
	// recorder, a MediaRecorder capture, an OBS replay buffer that dropped
	// frames) the average is far below the rate the action was captured at.
	// pickFPS plans from both (§20 erratum 93).
	RFPS Rational

	// ColorPrimaries, ColorTransfer and ColorSpace are the source's colour
	// tags exactly as ffprobe printed them ("bt709", "smpte2084",
	// "bt2020nc", "" when the stream declares none). Only the transfer
	// function is acted on — an HDR one (PQ or HLG) makes the plan tone map
	// — but all three are read because a rendition is tagged bt709
	// unconditionally and the three together are the claim that was being
	// made falsely before (§20 erratum 93).
	ColorPrimaries, ColorTransfer, ColorSpace string

	// FieldOrder is the video stream's `field_order`, lowercased ("tt",
	// "bb", "tb", "bt", "progressive", "unknown", or "" when the stream
	// declares none). Anything naming a FIELD order rather than a
	// progressive frame makes the plan deinterlace (§20 erratum 125): the
	// comb is in the pixels, the rendition is a progressive H.264 file, and
	// a player handed a woven frame with no interlace flag cannot undo it.
	FieldOrder string

	// The ONE audio stream of an audio clip (§20 erratum 328): its sample
	// rate in Hz, its channel count, ffmpeg's sample format ("s16", "s32",
	// "flt", "fltp", …) and its bit depth — bits_per_raw_sample for a codec
	// that declares one (FLAC, ALAC, a 24-bit WAV), bits_per_sample for raw
	// PCM, 0 for a lossy codec, which has none. CoverArt says the file also
	// carried an attached picture, which is never a video and never served.
	AudioSampleRate, AudioChannels, AudioBits int
	AudioSampleFmt                            string
	CoverArt                                  bool
	// EncoderTag says an audio file names a program anywhere — a format or
	// stream tag `encoder` with a value. The video remux's fixed point allows
	// that key (structuralTags); an audio clip's strip does not (D4a: "no
	// encoder anywhere"), and CheckStripped refuses an output that carries one.
	EncoderTag bool
}

// Clip kinds (§20 erratum 328), spelled as db stores them.
const (
	KindVideo = "video"
	KindAudio = "audio"
)

// interlacedFieldOrders are the ffprobe `field_order` values that mean the
// coded picture is two fields per frame: top/bottom field first, plus the
// two "coded one way, displayed the other" spellings. Everything else —
// "progressive", "unknown", an absent field — is left alone: inferring
// interlace from silence would put every untagged upload through a
// deinterlacer.
var interlacedFieldOrders = map[string]struct{}{
	"tt": {}, "bb": {}, "tb": {}, "bt": {},
}

// IsInterlacedFieldOrder reports whether an ffprobe `field_order` names an
// interlaced stream. It is one function because the plan and the trim's
// stream-copy decision must agree about one string.
func IsInterlacedFieldOrder(s string) bool {
	_, ok := interlacedFieldOrders[strings.ToLower(strings.TrimSpace(s))]
	return ok
}

// StreamExtent is one stream's own span on the source's timeline, in seconds.
// Known is false when the stream declared no usable duration, and then the
// span means nothing: callers must treat an unknown extent as "everything may
// be there" rather than as an empty one.
type StreamExtent struct {
	StartSec, EndSec float64
	Known            bool
}

// Overlap is how many seconds of this stream fall inside [start, end). An
// unknown extent overlaps the whole window: nothing was measured, so nothing
// is refused on its account.
func (e StreamExtent) Overlap(start, end float64) float64 {
	if !e.Known {
		return math.Max(0, end-start)
	}
	lo, hi := math.Max(start, e.StartSec), math.Min(end, e.EndSec)
	return math.Max(0, hi-lo)
}

// ErrPermanent wraps file-at-fault failures (§11 classification).
type ErrPermanent struct{ Err error }

func (e *ErrPermanent) Error() string { return "permanent: " + e.Err.Error() }
func (e *ErrPermanent) Unwrap() error { return e.Err }

// IsPermanent reports whether err is (wraps) an ErrPermanent.
func IsPermanent(err error) bool {
	var p *ErrPermanent
	return errors.As(err, &p)
}

// permanent builds an ErrPermanent from a format string.
func permanent(format string, args ...any) error {
	return &ErrPermanent{Err: fmt.Errorf(format, args...)}
}
