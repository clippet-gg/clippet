package transcode

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
)

// OutputInfo is the measured description of an encoded output.
type OutputInfo struct {
	Width, Height              int
	FPS                        Rational
	DurationMS                 int64
	HasAudio                   bool
	VideoCodec, AudioCodec     string
	VideoStreams, AudioStreams int
	// VideoEndSec is where the PICTURE stops, in seconds, when the stream
	// declares it (0 = unmeasured). It is not DurationMS/1000: a source
	// whose capture died mid-recording keeps writing audio, and the
	// rendition inherits the shape — 2 s of video inside a 40 s file. The
	// poster seek is clamped against this rather than the container's
	// duration (§20 erratum 92).
	VideoEndSec float64
}

// ProbeOutput ffprobes an output file. A failing ffprobe on our own output
// is a permanent bad_output; a cancelled ctx is ctx.Err().
func ProbeOutput(ctx context.Context, ffprobe, path string) (*OutputInfo, error) {
	raw, err := runFFprobe(ctx, ffprobe, path)
	if err != nil {
		var pe *ErrPermanent
		switch {
		case ctx.Err() != nil:
			return nil, err
		case errors.As(err, &pe):
			return nil, permanent("bad_output: %v", pe.Err)
		}
		return nil, fmt.Errorf("probe output: %w", err)
	}
	return ParseOutput(raw)
}

// ParseOutput interprets an ffprobe JSON document describing an output:
// stream counts and codecs, the first video stream's dims and r_frame_rate,
// and the container duration in milliseconds.
func ParseOutput(raw []byte) (*OutputInfo, error) {
	var doc ffprobeDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, permanent("bad_output: ffprobe output unparsable: %v", err)
	}
	info := &OutputInfo{}
	var video *ffprobeStream
	for i := range doc.Streams {
		s := &doc.Streams[i]
		switch s.CodecType {
		case "video":
			info.VideoStreams++
			if video == nil {
				video = s
			}
		case "audio":
			info.AudioStreams++
			if info.AudioCodec == "" {
				info.AudioCodec = s.CodecName
			}
		}
	}
	info.HasAudio = info.AudioStreams > 0
	if video != nil {
		info.Width, info.Height, info.VideoCodec = video.Width, video.Height, video.CodecName
		if f, ok := parseRational(video.RFrameRate); ok {
			info.FPS = f
		} else if f, ok := parseRational(video.AvgFrameRate); ok {
			info.FPS = f
		}
		if e := extentOf(*video); e.Known {
			info.VideoEndSec = e.EndSec
		}
	}
	if d, ok := parseSeconds(doc.Format.Duration); ok {
		info.DurationMS = int64(math.Round(d * 1000))
	} else if video != nil {
		if d, ok := parseSeconds(video.Duration); ok {
			info.DurationMS = int64(math.Round(d * 1000))
		}
	}
	return info, nil
}

// CheckOutput applies the §10.3 step-5 assertions to a measured output:
// exactly one h264 video stream, at most one aac audio stream, even dims
// that fit the box in either orientation, a positive duration and a valid
// frame rate. Any mismatch is a permanent bad_output with detail.
func CheckOutput(info *OutputInfo, box PlanOptions) error {
	box = box.withDefaults()
	if info == nil {
		return permanent("bad_output: no probe result")
	}
	if info.VideoStreams != 1 {
		return permanent("bad_output: want exactly one video stream, got %d", info.VideoStreams)
	}
	if info.VideoCodec != "h264" {
		return permanent("bad_output: video codec %q, want h264", info.VideoCodec)
	}
	if info.AudioStreams > 1 {
		return permanent("bad_output: want at most one audio stream, got %d", info.AudioStreams)
	}
	if info.AudioStreams == 1 && info.AudioCodec != "aac" {
		return permanent("bad_output: audio codec %q, want aac", info.AudioCodec)
	}
	if info.Width <= 0 || info.Height <= 0 || info.Width%2 != 0 || info.Height%2 != 0 {
		return permanent("bad_output: dimensions %dx%d not even", info.Width, info.Height)
	}
	long, short := max(info.Width, info.Height), min(info.Width, info.Height)
	if long > box.MaxW || short > box.MaxH {
		return permanent("bad_output: dimensions %dx%d exceed the %dx%d box", info.Width, info.Height, box.MaxW, box.MaxH)
	}
	if info.DurationMS <= 0 {
		return permanent("bad_output: duration %dms not positive", info.DurationMS)
	}
	if !info.FPS.Valid() {
		return permanent("bad_output: frame rate %s invalid", info.FPS)
	}
	return nil
}

// TierCheck is what a produced TIER must be, for CheckTierOutput (§20 erratum
// 205). It exists beside CheckOutput because that function's box is the
// instance's 1280×720 frame ceiling, which since the quality tiers bounds the
// LIGHT copy alone: a Full is the source's own size and a remuxed Original is
// whatever was uploaded, so checking either against that box would fail exactly
// the clips this feature exists for.
type TierCheck struct {
	// Codecs is the closed set of acceptable video codec names ("h264",
	// "av1"). Empty means h264 alone, which is what every caller before these
	// tiers meant.
	Codecs []string
	// Width and Height, when positive, are the EXACT dimensions expected — the
	// plan's, or the source's for a remux. A tier is never "somewhere inside a
	// box": it is one known size, and a mismatch means the filter graph did
	// something nobody asked for.
	Width, Height int
	// AllowOdd drops the even-dimension assertion, and exactly one caller sets
	// it: the REMUX of a playable original (§20 erratum 200). Every encode this
	// pipeline runs produces even dimensions because MakePlan floors them to
	// even — so an odd one there is a bug and stays a failure — but a `-c copy`
	// inherits the frame it was handed, and H.264 permits an odd cropped size.
	// Refusing a file we are not re-encoding would be refusing the upload.
	AllowOdd bool
}

// CheckTierOutput applies the §10.3 step-5 assertions to a produced tier: one
// video stream in one of the accepted codecs, at most one aac audio stream,
// even dimensions that are EXACTLY the ones expected, a positive duration and a
// valid frame rate. Any mismatch is a permanent bad_output with detail.
//
// The one assertion it drops next to CheckOutput is the frame box, and the one
// it adds is exact dimensions — which is strictly stronger: a tier that came
// out at the wrong size is caught here whether that size is inside the old box
// or not.
func CheckTierOutput(info *OutputInfo, want TierCheck) error {
	if info == nil {
		return permanent("bad_output: no probe result")
	}
	if info.VideoStreams != 1 {
		return permanent("bad_output: want exactly one video stream, got %d", info.VideoStreams)
	}
	if !codecAllowed(info.VideoCodec, want.Codecs) {
		return permanent("bad_output: video codec %q, want one of %s", info.VideoCodec, codecList(want.Codecs))
	}
	if info.AudioStreams > 1 {
		return permanent("bad_output: want at most one audio stream, got %d", info.AudioStreams)
	}
	if info.AudioStreams == 1 && info.AudioCodec != "aac" {
		return permanent("bad_output: audio codec %q, want aac", info.AudioCodec)
	}
	if info.Width <= 0 || info.Height <= 0 {
		return permanent("bad_output: dimensions %dx%d not positive", info.Width, info.Height)
	}
	if !want.AllowOdd && (info.Width%2 != 0 || info.Height%2 != 0) {
		return permanent("bad_output: dimensions %dx%d not even", info.Width, info.Height)
	}
	if want.Width > 0 && want.Height > 0 && (info.Width != want.Width || info.Height != want.Height) {
		return permanent("bad_output: dimensions %dx%d, want %dx%d", info.Width, info.Height, want.Width, want.Height)
	}
	if info.DurationMS <= 0 {
		return permanent("bad_output: duration %dms not positive", info.DurationMS)
	}
	if !info.FPS.Valid() {
		return permanent("bad_output: frame rate %s invalid", info.FPS)
	}
	return nil
}

// codecAllowed reports whether got is in the accepted set (empty = h264 only).
func codecAllowed(got string, allowed []string) bool {
	if len(allowed) == 0 {
		return got == "h264"
	}
	for _, c := range allowed {
		if got == c {
			return true
		}
	}
	return false
}

// codecList renders the accepted set for a failure message.
func codecList(allowed []string) string {
	if len(allowed) == 0 {
		return "[h264]"
	}
	return "[" + strings.Join(allowed, " ") + "]"
}

// MP4 box types the walk cares about.
const (
	boxFtyp = "ftyp"
	boxMoov = "moov"
	boxMdat = "mdat"
)

// VerifyMP4 walks the top-level boxes and asserts ftyp first and moov
// before mdat (size==1 → 64-bit largesize; size==0 → to EOF). Any other
// shape is a permanent bad_output.
func VerifyMP4(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	return verifyBoxes(f, st.Size())
}

// FaststartMP4 reports whether a file is ALREADY in the shape a progressive
// download needs: ftyp first and moov before mdat (§20 erratum 200). It is
// VerifyMP4 asked as a question rather than as an assertion, and it is how the
// tier plan decides whether a playable original can be served as it stands or
// has to be remuxed.
//
// Anything that is not a walkable mp4 answers false, which is the safe
// direction: the caller's response to false is `-c copy` into a fresh mp4, and
// a file whose box layout this walk cannot follow is exactly the file that
// should be rewritten rather than streamed. (ffmpeg writes `moov` LAST unless
// told otherwise, so false is the common answer for an ordinary upload.)
func FaststartMP4(path string) bool { return VerifyMP4(path) == nil }

// verifyBoxes is the pure walk over an io.ReaderAt of the given size.
func verifyBoxes(r io.ReaderAt, size int64) error {
	var hdr [16]byte
	moovAt, mdatAt := int64(-1), int64(-1)
	for off, first := int64(0), true; off < size; first = false {
		if _, err := r.ReadAt(hdr[:8], off); err != nil {
			return permanent("bad_output: truncated box header at %d", off)
		}
		boxLen, hdrLen := int64(binary.BigEndian.Uint32(hdr[:4])), int64(8)
		typ := string(hdr[4:8])
		switch boxLen {
		case 0:
			boxLen = size - off
		case 1:
			if _, err := r.ReadAt(hdr[8:16], off+8); err != nil {
				return permanent("bad_output: truncated largesize at %d", off)
			}
			large := binary.BigEndian.Uint64(hdr[8:16])
			if large > uint64(math.MaxInt64) {
				return permanent("bad_output: absurd largesize at %d", off)
			}
			boxLen, hdrLen = int64(large), 16
		}
		if boxLen < hdrLen || !printableType(typ) {
			return permanent("bad_output: malformed box %q (size %d) at %d", typ, boxLen, off)
		}
		if first && typ != boxFtyp {
			return permanent("bad_output: first box is %q, want ftyp", typ)
		}
		switch typ {
		case boxMoov:
			if moovAt < 0 {
				moovAt = off
			}
		case boxMdat:
			if mdatAt < 0 {
				mdatAt = off
			}
		}
		off += boxLen
		if off > size {
			return permanent("bad_output: box %q overruns the file (%d > %d)", typ, off, size)
		}
	}
	switch {
	case moovAt < 0:
		return permanent("bad_output: no moov box")
	case mdatAt < 0:
		return permanent("bad_output: no mdat box")
	case moovAt > mdatAt:
		return permanent("bad_output: moov (%d) after mdat (%d); faststart missing", moovAt, mdatAt)
	}
	return nil
}

func printableType(t string) bool {
	if len(t) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		if t[i] < 0x20 || t[i] > 0x7e {
			return false
		}
	}
	return true
}
