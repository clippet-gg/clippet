package transcode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Server-side trim (DESIGN.md §17 #5). The original is retained forever, so
// a trim never touches the rendition it replaces: it re-derives one from the
// SOURCE, either by copying the coded packets of the requested window or by
// running the ordinary plan over it.
//
// The whole of the decision lives in DecideTrim, and its bias is deliberate:
// a copy is chosen only when it is provably indistinguishable from what the
// re-encode would have produced. Everything else re-encodes, which is slower
// and always right.

// Reason* name why a trim could not be a stream copy. They are operator text
// (a log field), never a failure. ReasonNoKeyframes is the one a CALLER acts
// on: it is what DecideTrim answers when every cheap condition has passed and
// the only fact still missing is the keyframe list, which costs a demux of
// the whole file to obtain.
const (
	ReasonNone          = ""
	ReasonNotMP4        = "container is not mp4/mov"
	ReasonNotH264       = "video codec is not h264"
	ReasonBadPixFmt     = "pixel format is not yuv420p"
	ReasonHDR           = "the source declares an HDR transfer function"
	ReasonInterlaced    = "the source is interlaced"
	ReasonAudioShape    = "audio is not a single aac track"
	ReasonNeedsScale    = "the plan rescales the frame"
	ReasonNeedsFPS      = "the plan changes the frame rate"
	ReasonRotated       = "the source carries a rotation"
	ReasonAnamorphic    = "the source has a non-square pixel aspect"
	ReasonNoDuration    = "the source declares no duration"
	ReasonStartNotKey   = "the start does not land on a keyframe"
	ReasonEndNotKey     = "the end lands neither on a keyframe nor at the end of the source"
	ReasonNoKeyframes   = "no keyframes were found"
	ReasonWindowInvalid = "the trim window is empty"
)

// TrimPlan is DecideTrim's verdict: which of the two paths to take, and the
// exact ffmpeg window to take it with.
type TrimPlan struct {
	// Copy is true for the keyframe stream copy.
	Copy bool
	// Reason is why Copy is false (ReasonNone when it is true).
	Reason string
	// Window is what both paths hand ffmpeg. For a copy Start is the
	// KEYFRAME timestamp, not the requested one — see Window.
	Window Window
	// DropAudio drops the source's audio from this cut: the window sits
	// outside what the audio stream holds, so keeping the map would ask
	// ffmpeg to copy or encode a track with nothing in it (see CoverTrim).
	DropAudio bool
}

// Path names the trim path for logs, stages and the operator.
func (t TrimPlan) Path() string {
	if t.Copy {
		return "copy"
	}
	return "reencode"
}

// --- what the source actually holds over a window (§20 erratum 78) ---

// ErrWindowEmpty is the CLIENT condition of a trim: the requested window
// holds no video. It is the file and the request disagreeing — a window past
// the end of a video stream that stops before its container does — and no
// retry, and no different machine, can make it succeed. The worker fails the
// attempt permanently on it, at INFO, with this plain sentence: it is the one
// trim failure an owner can cause on purpose, so it must never look like the
// server breaking.
var ErrWindowEmpty = errors.New("there is no video in the part of the original you asked for")

// minCoveredSec is the least video a window may overlap and still be worth
// cutting. Below it there is no clip to make — a single frame or two, whose
// poster seek and GOP make it worse the smaller it gets — and the honest
// answer is the same as for no video at all.
const minCoveredSec = 0.1

// minAudioSec is the least audio a window may overlap and still keep its
// audio track. Below it the map would carry an empty or near-empty stream;
// dropping it produces a silent cut, which is what the source is.
const minAudioSec = 0.05

// TrimCoverage is what the source holds over a requested window: the window
// CLAMPED to the video stream, whether that clamp was measured or assumed,
// and whether the audio must be dropped.
type TrimCoverage struct {
	// StartMS/EndMS are the window to actually cut, clamped to the video
	// stream's own extent. They equal the request whenever the video covers
	// it, which is the ordinary case.
	StartMS, EndMS int64
	// Verified is true when the video extent was MEASURED and covers the
	// clamped window. Only then is a later `bad_output` this server's fault
	// rather than the file's — which is what decides the log level (§12).
	Verified bool
	// DropAudio: the window falls outside the audio stream.
	DropAudio bool
	// Clamped is true when the requested end was shortened to the video's.
	Clamped bool
}

// CoverTrim compares one requested window (milliseconds, on the ORIGINAL's
// timeline) against what p's streams hold, and returns the window to cut.
//
// Three outcomes, in the order they matter:
//
//  1. no video in the window at all (or less than a tenth of a second of it) —
//     ErrWindowEmpty, the CLIENT condition above;
//  2. video that stops inside the window — the window is CLAMPED to it, so the
//     owner gets the footage that exists instead of a file whose duration
//     check then calls our own output bad. The applied window is what gets
//     persisted, so nothing downstream is left believing the request;
//  3. audio that does not reach the window — DropAudio, and the cut is silent.
//
// A stream with no measured extent constrains nothing: Verified stays false,
// the window is used as asked, and a later mismatch is not blamed on us.
func CoverTrim(p *ProbeResult, startMS, endMS int64) (TrimCoverage, error) {
	if p == nil || endMS <= startMS || startMS < 0 {
		return TrimCoverage{}, ErrWindowEmpty
	}
	start, end := float64(startMS)/1000, float64(endMS)/1000
	cov := TrimCoverage{StartMS: startMS, EndMS: endMS}
	if p.Video.Known {
		if p.Video.Overlap(start, end) < minCoveredSec {
			return TrimCoverage{}, ErrWindowEmpty
		}
		if end > p.Video.EndSec {
			end = p.Video.EndSec
			cov.EndMS = int64(math.Floor(end * 1000))
			cov.Clamped = true
		}
		if cov.EndMS-cov.StartMS < int64(minCoveredSec*1000) {
			return TrimCoverage{}, ErrWindowEmpty
		}
		cov.Verified = true
	}
	if len(p.AudioIndexes) > 0 && p.Audio.Known && p.Audio.Overlap(start, end) < minAudioSec {
		cov.DropAudio = true
	}
	return cov, nil
}

// KeyframeArgs is the ffprobe argv listing one video stream's packets with
// their keyframe flags. Packets, not frames: `-skip_frame nokey` decodes the
// whole file to answer the same question, and a demux is enough — a packet
// flagged K is exactly a point ffmpeg can start a copy at.
func KeyframeArgs(path string, videoIndex int) []string {
	return append([]string{
		"-v", "error", "-select_streams", strconv.Itoa(videoIndex),
		"-show_entries", "packet=pts_time,flags", "-print_format", "json",
	}, probeInputArgs(path)...)
}

// Keyframes returns the ascending keyframe timestamps (seconds) of the
// given video stream. A non-zero ffprobe exit is the file's fault
// (ErrPermanent); a cancelled ctx is ctx.Err().
func Keyframes(ctx context.Context, ffprobe, path string, videoIndex int) ([]float64, error) {
	out, se, err := runCapture(ctx, ffprobe, KeyframeArgs(path, videoIndex)...)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		reason := exitReason("ffprobe", err, se)
		if isExit(err) {
			return nil, permanent("%s", reason)
		}
		return nil, fmt.Errorf("%s", reason)
	}
	return ParseKeyframes(out)
}

// packetsDoc is the `-show_entries packet=pts_time,flags` document.
type packetsDoc struct {
	Packets []struct {
		PTSTime string `json:"pts_time"`
		Flags   string `json:"flags"`
	} `json:"packets"`
}

// ParseKeyframes extracts the ascending timestamps of the packets flagged as
// keyframes ("K" in the flags string). Packets with no usable pts_time
// ("N/A") are skipped: a cut point cannot be matched against a timestamp
// that does not exist. Exported so fixtures exercise the rule without a
// binary.
func ParseKeyframes(raw []byte) ([]float64, error) {
	var doc packetsDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, permanent("ffprobe packet output unparsable: %v", err)
	}
	out := make([]float64, 0, 16)
	for _, p := range doc.Packets {
		if !strings.Contains(p.Flags, "K") {
			continue
		}
		t, err := strconv.ParseFloat(strings.TrimSpace(p.PTSTime), 64)
		if err != nil || math.IsNaN(t) || math.IsInf(t, 0) || t < 0 {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// keyframeEpsilon absorbs the rounding of a printed timestamp (ffprobe emits
// six decimals) on top of the one-frame tolerance.
const keyframeEpsilon = 1e-4

// nearestKeyframe returns the keyframe within tol of t, preferring the
// closest one. ok is false when there is none.
func nearestKeyframe(keys []float64, t, tol float64) (float64, bool) {
	best, found := 0.0, false
	for _, k := range keys {
		d := math.Abs(k - t)
		if d > tol {
			continue
		}
		if !found || d < math.Abs(best-t) {
			best, found = k, true
		}
	}
	return best, found
}

// DecideTrim chooses the path for one trim of a source described by p, whose
// ordinary encode plan is plan, given its keyframe timestamps and the
// requested window in milliseconds.
//
// A stream copy is allowed only when EVERY one of these holds, because each
// of them is something the re-encode would otherwise have changed:
//
//  1. the container is mp4/mov and the video is h264 in yuv420p, and the
//     source does not declare an HDR transfer function — the shape
//     `CheckOutput` demands, that `+faststart` can remux, and that carries
//     the Rec.709 the rendition claims (§20 erratum 93);
//  2. there is no audio, or exactly one aac track — two tracks are MERGED by
//     the plan (§10.3), and a copy would silently drop one;
//  3. the plan neither rescales nor caps the frame rate, the pixel aspect is
//     square and the source carries no rotation — i.e. the coded frame the
//     copy would emit is the frame the encode would have emitted;
//  4. both cut points land within one frame of a keyframe (the end may also
//     sit at the end of the source, which is the same clean cut).
//
// Rule 4 is one-sided in effect: the returned window starts at the KEYFRAME,
// never at the requested millisecond, because a copy cannot start anywhere
// else and `-ss` would otherwise silently rewind to the previous one. The
// duration is measured from that keyframe to the requested end, so the
// window a copy writes is within one frame of the window that was asked for.
func DecideTrim(p *ProbeResult, plan Plan, keys []float64, startMS, endMS int64) TrimPlan {
	startSec, endSec := float64(startMS)/1000, float64(endMS)/1000
	reencode := func(reason string) TrimPlan {
		return TrimPlan{Reason: reason, Window: Window{Start: startSec, Duration: endSec - startSec}}
	}
	if p == nil || endMS <= startMS || startMS < 0 {
		return reencode(ReasonWindowInvalid)
	}
	switch {
	case !isMP4Container(p.FormatName):
		return reencode(ReasonNotMP4)
	case p.VideoCodec != "h264":
		return reencode(ReasonNotH264)
	case p.PixFmt != "yuv420p":
		return reencode(ReasonBadPixFmt)
	case isHDRTransfer(p.ColorTransfer):
		// A copy keeps the source's colour tags, and an HDR rendition is a
		// thing this pipeline promises never to produce (§20 erratum 93).
		// Nearly every HDR source is 10-bit and already refused above; this
		// catches the 8-bit yuv420p H.264 stream that still claims PQ, which
		// some capture hardware really does write.
		return reencode(ReasonHDR)
	case plan.Deinterlace:
		// A copy keeps the coded fields, and the pipeline's rendition is
		// progressive (§20 erratum 125). Copying here would hand the owner a
		// combed cut of a clip whose own rendition had been deinterlaced —
		// the trim's promise is "the shape the pipeline would have produced",
		// and after this erratum that shape has no fields in it.
		return reencode(ReasonInterlaced)
	case len(p.AudioIndexes) > 1, len(p.AudioIndexes) == 1 && audioCodec(p, 0) != "aac":
		return reencode(ReasonAudioShape)
	case p.Rotation != 0:
		return reencode(ReasonRotated)
	case p.SARNum != p.SARDen:
		return reencode(ReasonAnamorphic)
	case plan.W != p.CodedW || plan.H != p.CodedH:
		return reencode(ReasonNeedsScale)
	case plan.FPS != p.FPS:
		return reencode(ReasonNeedsFPS)
	case !p.HasDuration || p.DurationUS <= 0:
		return reencode(ReasonNoDuration)
	case len(keys) == 0:
		return reencode(ReasonNoKeyframes)
	}
	tol := keyframeEpsilon
	if p.FPS.Valid() {
		tol += 1 / p.FPS.Float()
	}
	ks, ok := nearestKeyframe(keys, startSec, tol)
	if !ok {
		return reencode(ReasonStartNotKey)
	}
	srcEnd := float64(p.DurationUS) / 1e6
	if _, ok := nearestKeyframe(keys, endSec, tol); !ok && math.Abs(srcEnd-endSec) > tol {
		return reencode(ReasonEndNotKey)
	}
	if endSec-ks <= 0 {
		return reencode(ReasonWindowInvalid)
	}
	return TrimPlan{Copy: true, Window: Window{Start: ks, Duration: endSec - ks}}
}

// audioCodec is the codec of the i-th audio stream ("" when unknown, e.g. a
// probe from an older fixture).
func audioCodec(p *ProbeResult, i int) string {
	if i < len(p.AudioCodecs) {
		return p.AudioCodecs[i]
	}
	return ""
}

// isMP4Container reports whether an ffprobe format_name names the ISO-BMFF
// family ffprobe demuxes as one ("mov,mp4,m4a,3gp,3g2,mj2"). Only these can
// be copied into an mp4 whose moov `+faststart` then moves to the front
// without touching a single coded byte.
func isMP4Container(formatName string) bool {
	for _, name := range strings.Split(formatName, ",") {
		switch strings.TrimSpace(name) {
		case "mp4", "mov", "m4v":
			return true
		}
	}
	return false
}

// CopyArgs renders the stream-copy argv for one trim window: the same output
// SHAPE the encoder produces (one video stream, at most one audio stream, no
// subtitles, no metadata, faststart mp4) reached without re-encoding a
// frame. -progress is not requested: a copy has no encode to report on, and
// the pipeline drives its progress from the stage instead.
func (t TrimPlan) CopyArgs(p *ProbeResult, in, out string) []string {
	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-nostats", "-y",
		"-ss", secArg(t.Window.Start),
	}
	args = append(args, inputArgs(in)...)
	args = append(args, "-map", "0:"+strconv.Itoa(p.VideoIndex))
	if len(p.AudioIndexes) == 1 && !t.DropAudio {
		args = append(args, "-map", "0:"+strconv.Itoa(p.AudioIndexes[0]))
	}
	return append(args,
		"-sn", "-dn", "-map_metadata", "-1", "-map_chapters", "-1",
		"-t", secArg(t.Window.Duration),
		"-c", "copy",
		"-movflags", "+faststart", "-f", "mp4", out,
	)
}

// Copy runs one stream-copy trim. Outcomes match Encode's: nil on exit 0,
// ctx.Err() when the caller cancelled, ErrPermanent for a non-zero exit
// (with the stderr tail) — a remux that ffmpeg refuses is the file's fault.
func Copy(ctx context.Context, ffmpeg string, t TrimPlan, p *ProbeResult, in, out string) error {
	_, se, err := runCapture(ctx, ffmpeg, t.CopyArgs(p, in, out)...)
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
