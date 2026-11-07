package transcode

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
)

// GIF export (DESIGN.md §17 #6): a bounded, silent, looping PREVIEW of a
// few seconds of a clip's RENDITION, starting where the poster was taken
// (§20 erratum 92).
//
// It is a preview, not a re-encode, and every bound below exists to keep it
// one. A GIF has no inter-frame compression worth the name and one palette
// of 256 colours per frame set, so the honest way to bound its cost is to
// bound its inputs: GIFMaxSeconds of window, GIFFPS frames a second, at most
// GIFMaxWidth across, never upscaled. Six seconds of 480×270 at 12 fps is a
// couple of megabytes; the same clip at source rate and size is fifty.
//
// Two passes, because one is visibly worse. `palettegen` walks the frames
// and builds the 256-colour table they actually need; `paletteuse` then maps
// each frame onto it. The single-pass alternative is ffmpeg's built-in 216
// colour web palette, which turns every gradient into bands. `stats_mode=diff`
// weighs the pixels that CHANGE between frames, so the palette is spent on
// what moves rather than on a static background, and `dither=bayer` with a
// small scale trades a faint ordered pattern for a much smaller file than
// error-diffusion dithering, whose noise is different in every frame and
// therefore compresses appallingly.
const (
	// GIFMaxSeconds bounds the window, wherever in the clip it starts.
	GIFMaxSeconds = 6.0
	// GIFFPS is the preview's frame rate: enough for motion to read, few
	// enough that six seconds is 72 frames.
	GIFFPS = 12
	// GIFMaxWidth is the widest preview produced; a narrower rendition keeps
	// its own width (a preview is never upscaled).
	GIFMaxWidth = 480
	// GIFMaxBytes is the hard ceiling on what one pass may write (§20
	// erratum 46's mechanism). Past it the run is killed and the job fails
	// permanently: a GIF this size is not a preview, and the only way to
	// reach it is an input the bounds above did not predict.
	GIFMaxBytes = 8 << 20
	// GIFHardTimeout bounds one pass. A bounded window at 12 fps is seconds
	// of work; a minute is the "this is not happening" line.
	GIFHardTimeout = 60 * time.Second
	// gifDither is the paletteuse dithering: ordered, cheap, stable frame to
	// frame. bayer_scale 5 is the finest pattern ffmpeg offers.
	gifDither = "bayer:bayer_scale=5"
	// gifPaletteStats spends the palette on what moves.
	gifPaletteStats = "diff"
	// gifScaleFlags matches the rendition's own scaler (§10.3).
	gifScaleFlags = "lanczos"
)

// GIFPlan is the bounded preview one clip gets: where the window starts, how
// long it is, the rate and the width, all derived from the rendition's
// measured facts. It is pure and table-tested; nothing about a GIF depends on
// anything the uploader says.
type GIFPlan struct {
	StartSec float64 // where the window begins: the poster's own timestamp
	Seconds  float64 // window length
	FPS      int
	Width    int
}

// PlanGIF derives the preview from the rendition's measured duration (ms) and
// width, and starts it where the poster was taken (posterSec; 0 when no
// timestamp is available). A rendition shorter than GIFMaxSeconds yields its
// whole self from the start; a rendition narrower than GIFMaxWidth keeps its
// width; a row with no usable measurement falls back to the bounds
// themselves, which are safe by construction (the byte cap and the hard
// timeout stand behind them).
//
// The start matters for the same reason the poster's does (§20 erratum 92): a
// clip that opens on a loading screen or a fade-in spent half its preview on
// a blank rectangle. The window still ends inside the rendition — the start
// is never past duration − GIFMaxSeconds — so no preview is shortened by it.
func PlanGIF(durationMS int64, width int, posterSec float64) GIFPlan {
	p := GIFPlan{Seconds: GIFMaxSeconds, FPS: GIFFPS, Width: GIFMaxWidth}
	if durationMS > 0 {
		if s := float64(durationMS) / 1000; s < p.Seconds {
			p.Seconds = s
		}
		if latest := float64(durationMS)/1000 - GIFMaxSeconds; latest > 0 && posterSec > 0 &&
			!math.IsNaN(posterSec) && !math.IsInf(posterSec, 0) {
			p.StartSec = math.Min(posterSec, latest)
		}
	}
	if width > 0 && width < p.Width {
		p.Width = width
	}
	// An odd width would make `scale=W:-2` pick an even height for an odd
	// width — legal, but the rendition is always even (§10.3), so this only
	// ever fires on a hand-written row.
	if p.Width%2 != 0 {
		p.Width--
	}
	if p.Width < 2 {
		p.Width = 2
	}
	return p
}

// Frames is how many frames the plan schedules (the verify step's lower
// bound: a preview with no frames is not a preview).
func (p GIFPlan) Frames() int {
	n := int(math.Round(p.Seconds * float64(p.FPS)))
	if n < 1 {
		return 1
	}
	return n
}

// filterChain is the shared head of both passes: sample at GIFFPS, scale to
// the planned width keeping the aspect ratio on an even height.
func (p GIFPlan) filterChain() string {
	return "fps=" + strconv.Itoa(p.FPS) +
		",scale=" + strconv.Itoa(p.Width) + ":-2:flags=" + gifScaleFlags
}

// PaletteArgs renders pass 1: the 256-colour table this window needs, as a
// single PNG frame.
//
// threads is the CPU quota (§20 erratum 215), rendered by the same
// encodeHeader every encode carries. Both GIF passes are real filter-graph
// work — a scale plus palettegen or paletteuse — and an unpinned filter pool
// inside a `cpus: N` container sizes itself from the HOST's core count, which
// is the exact mistake erratum 215 measured at 25 % on the encode.
func (p GIFPlan) PaletteArgs(in, palette string, threads int) []string {
	args := append(encodeHeader(threads),
		// `-ss` and `-t` in FRONT of `-i` bound what is READ, so a two-hour
		// rendition costs the same as a six-second one wherever the window
		// sits. Both passes MUST spell the identical pair: a palette built
		// from one set of frames and used on another is not the palette
		// those frames need.
		"-ss", secArg(p.StartSec), "-t", secArg(p.Seconds),
	)
	args = append(args, inputArgs(in)...)
	return append(args,
		"-map", "0:v:0", "-an", "-sn", "-dn",
		"-vf", p.filterChain()+",palettegen=stats_mode="+gifPaletteStats,
		"-frames:v", "1", "-update", "1", "-f", "image2", palette,
	)
}

// EncodeArgs renders pass 2: the same window and chain, mapped onto the
// palette, muxed as an infinitely looping GIF (`-loop 0`).
func (p GIFPlan) EncodeArgs(in, palette, out string, threads int) []string {
	args := append(encodeHeader(threads),
		"-ss", secArg(p.StartSec), "-t", secArg(p.Seconds),
	)
	// Two inputs, two whitelists: an input option governs the input that
	// follows it and nothing else. `-ss`/`-t` govern the FIRST input only —
	// the palette is a single PNG frame and is read whole.
	args = append(args, inputArgs(in)...)
	args = append(args, inputArgs(palette)...)
	return append(args,
		"-lavfi", "[0:v]"+p.filterChain()+"[x];[x][1:v]paletteuse=dither="+gifDither,
		"-an", "-sn", "-dn", "-map_metadata", "-1", "-map_chapters", "-1",
		"-loop", "0", "-f", "gif", out,
	)
}

// progressPlan is the arithmetic runInstrumented needs to turn ffmpeg's
// -progress lines into a fraction and to feed the stall watchdog: the
// window's duration and the output frame rate. Nothing else of Plan is read.
func (p GIFPlan) progressPlan() Plan {
	return Plan{
		HasDuration: p.Seconds > 0,
		DurationUS:  int64(math.Round(p.Seconds * 1e6)),
		FPS:         Rational{Num: p.FPS, Den: 1},
		W:           p.Width,
	}
}

// GIFOptions are the guards around the two passes. Zero values disable the
// guard they name, exactly as EncodeOptions does.
type GIFOptions struct {
	StallTimeout, HardTimeout time.Duration
	MaxOutputBytes            int64

	// Threads is EncodeOptions.Threads for the two GIF passes (§20 erratum
	// 215): zero renders neither option, which is ffmpeg's own "decide for
	// yourself", and the worker always sets it.
	Threads int
}

// MakeGIF runs both passes. Outcomes are Encode's: nil on success, ctx.Err()
// when the caller cancelled, ErrPermanent for a stall, a timeout, the byte
// cap or a non-zero exit. onProgress (may be nil) reports the SECOND pass —
// the first is a palette that finishes in a moment and would only make the
// bar jump back.
func MakeGIF(ctx context.Context, ffmpeg string, p GIFPlan, in, palette, out string, o GIFOptions, onProgress func(frac float64, indeterminate bool)) error {
	pp := p.progressPlan()
	guards := instrument{label: "gif", stallTimeout: o.StallTimeout, hardTimeout: o.HardTimeout, maxOutputBytes: o.MaxOutputBytes}
	if err := runInstrumented(ctx, ffmpeg, p.PaletteArgs(in, palette, o.Threads), pp, guards, nil); err != nil {
		return err
	}
	return runInstrumented(ctx, ffmpeg, p.EncodeArgs(in, palette, out, o.Threads), pp, guards, onProgress)
}

// GIFInfo is the measured description of a produced GIF.
type GIFInfo struct {
	Width, Height, Frames int
	Codec                 string
}

// gifProbeDoc is the shape of the ffprobe call ProbeGIF makes. nb_read_frames
// is printed as a string (ffprobe prints every stream field as one).
type gifProbeDoc struct {
	Streams []struct {
		CodecName  string `json:"codec_name"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		ReadFrames string `json:"nb_read_frames"`
	} `json:"streams"`
}

// ProbeGIF measures the produced GIF, COUNTING its frames rather than
// trusting a header: a GIF's stream carries no frame count, and "did this
// actually animate" is the one question a preview must answer. Counting is
// cheap here and only here, because the plan bounds the file to a few dozen
// frames.
func ProbeGIF(ctx context.Context, ffprobe, path string) (*GIFInfo, error) {
	args := append([]string{"-v", "error", "-select_streams", "v:0", "-count_frames",
		"-show_entries", "stream=codec_name,width,height,nb_read_frames", "-print_format", "json"}, probeInputArgs(path)...)
	out, se, err := runCapture(ctx, ffprobe, args...)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		reason := exitReason("ffprobe", err, se)
		if isExit(err) {
			// ffprobe ran and rejected the file: our own output is at fault.
			return nil, permanent("bad_output: %s", reason)
		}
		// ffprobe could not start at all: an environment failure, retryable.
		return nil, fmt.Errorf("%s", reason)
	}
	return ParseGIFProbe(out)
}

// ParseGIFProbe interprets ProbeGIF's document (exported so the parse is
// tested without a binary).
func ParseGIFProbe(raw []byte) (*GIFInfo, error) {
	var doc gifProbeDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, permanent("bad_output: ffprobe output unparsable: %v", err)
	}
	if len(doc.Streams) == 0 {
		return nil, permanent("bad_output: the gif has no video stream")
	}
	s := doc.Streams[0]
	info := &GIFInfo{Width: s.Width, Height: s.Height, Codec: s.CodecName}
	if n, err := strconv.Atoi(s.ReadFrames); err == nil {
		info.Frames = n
	}
	return info, nil
}

// CheckGIF applies the verify-stage assertions to a measured GIF: it must be
// a gif, it must have real dimensions no wider than the plan asked for, and
// it must have at least one frame. Any mismatch is a permanent bad_output —
// our own output, which cannot be wrong by design (§12).
func CheckGIF(info *GIFInfo, p GIFPlan) error {
	switch {
	case info == nil:
		return permanent("bad_output: no probe result")
	case info.Codec != "gif":
		return permanent("bad_output: codec %q, want gif", info.Codec)
	case info.Width <= 0 || info.Height <= 0:
		return permanent("bad_output: dimensions %dx%d", info.Width, info.Height)
	case info.Width > p.Width:
		return permanent("bad_output: width %d exceeds the %d planned", info.Width, p.Width)
	case info.Frames < 1:
		return permanent("bad_output: the gif has %d frames", info.Frames)
	}
	return nil
}
