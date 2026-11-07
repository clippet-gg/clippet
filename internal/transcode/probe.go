package transcode

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	// maxAudioInputs caps the audio streams a plan merges (§10.3 step 2).
	// Eight, not four (§20 erratum 93): an OBS scene with game + mic +
	// Discord + music + browser audio is already five tracks, and every one
	// past the cap is SILENTLY dropped — there is no surfacing for it. The
	// merge itself is free next to the video encode (one more `aresample`
	// and one more `amix` input), so the bound exists only to keep the
	// filter graph finite, not to save work.
	maxAudioInputs = 8
	// sideDataDisplayMatrix is the ffprobe side_data_type carrying rotation.
	sideDataDisplayMatrix = "Display Matrix"
)

// defaultFPS is assumed when the source reports no usable frame rate.
var defaultFPS = Rational{30, 1}

// ffprobeDoc mirrors the parts of `ffprobe -print_format json -show_format
// -show_streams` that Probe and ProbeOutput read. Unknown fields are ignored.
type ffprobeDoc struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

type ffprobeStream struct {
	Index             int               `json:"index"`
	CodecName         string            `json:"codec_name"`
	CodecType         string            `json:"codec_type"`
	Width             int               `json:"width"`
	Height            int               `json:"height"`
	PixFmt            string            `json:"pix_fmt"`
	SampleAspectRatio string            `json:"sample_aspect_ratio"`
	RFrameRate        string            `json:"r_frame_rate"`
	AvgFrameRate      string            `json:"avg_frame_rate"`
	ColorPrimaries    string            `json:"color_primaries"`
	ColorTransfer     string            `json:"color_transfer"`
	ColorSpace        string            `json:"color_space"`
	FieldOrder        string            `json:"field_order"`
	Duration          string            `json:"duration"`
	StartTime         string            `json:"start_time"`
	Disposition       map[string]int    `json:"disposition"`
	Tags              map[string]string `json:"tags"`
	SideDataList      []ffprobeSideData `json:"side_data_list"`
	// The audio stream's shape (§20 erratum 328): what an audio clip is
	// labelled with and what its Full is planned from. ffprobe prints
	// sample_rate and bits_per_raw_sample as STRINGS ("48000", "24", "N/A")
	// and bits_per_sample as a number; each is read the way it comes.
	SampleRate       string `json:"sample_rate"`
	Channels         int    `json:"channels"`
	SampleFmt        string `json:"sample_fmt"`
	BitsPerSample    int    `json:"bits_per_sample"`
	BitsPerRawSample string `json:"bits_per_raw_sample"`
}

type ffprobeSideData struct {
	Type     string   `json:"side_data_type"`
	Rotation *float64 `json:"rotation"`
	// DisplayMatrix is ffprobe's dump of the nine 16.16 fixed-point
	// coefficients ("00000000: a b u\n00000001: c d v\n..."), the ONLY
	// place the sub-degree angle survives: the sibling "rotation" key is
	// printed with a C cast and is therefore truncated (§20 erratum 47).
	DisplayMatrix string `json:"displaymatrix"`
}

type ffprobeFormat struct {
	FormatName string            `json:"format_name"`
	Duration   string            `json:"duration"`
	Tags       map[string]string `json:"tags"`
}

// isStillImage reports whether an ffprobe format_name (comma-separated
// demuxer aliases) names a single-picture demuxer: image2/image2pipe or
// one of the *_pipe image demuxers (png_pipe, jpeg_pipe, webp_pipe, ...;
// the suffix is used by nothing else). ffmpeg would happily "transcode" a
// picture into a one-frame clip, so such an upload is rejected as no video
// (§8.3 not_a_video). Animated formats (gif, apng in a container) are
// demuxed under their own names and stay accepted.
func isStillImage(formatName string) bool {
	for _, name := range strings.Split(formatName, ",") {
		name = strings.TrimSpace(name)
		if name == "image2" || name == "image2pipe" || strings.HasSuffix(name, "_pipe") {
			return true
		}
	}
	return false
}

// allowedFormats is the closed set of demuxer aliases the ingest gate
// accepts (§8.3 step 6). ffprobe names a demuxer by every alias it answers
// to, comma-separated — an MP4 is reported as the whole string
// "mov,mp4,m4a,3gp,3g2,mj2" — so a document is judged alias by alias and
// one hit is enough.
//
// It is an ALLOWLIST, not a list of bad formats, because the formats that
// must never reach a demuxer of ours are exactly the ones that are not
// video at all: hls/applehttp, dash, concat, sdp, rtsp and ffmetadata are
// text files naming further resources, and ffmpeg would go and read them.
// The uploader picks the spool extension (store.SanitizeExt), so `evil.m3u8`
// arrives as source.m3u8 and is demuxed as HLS unless something says no;
// this is that something. A raw elementary stream (h264, hevc) is refused
// with them: it is not a container, and nothing produces one as a clip.
var allowedFormats = map[string]struct{}{
	// ISO-BMFF, as ffprobe spells the family.
	"mov": {}, "mp4": {}, "m4a": {}, "3gp": {}, "3g2": {}, "mj2": {},
	// Matroska/WebM, AVI, the MPEG family, Flash, ASF/WMV, Ogg, MXF, GIF.
	"matroska": {}, "webm": {}, "avi": {}, "mpegts": {}, "mpeg": {},
	"mpegvideo": {}, "flv": {}, "asf": {}, "wmv": {}, "ogg": {},
	"gif": {}, "mxf": {},
}

// isAllowedContainer reports whether an ffprobe format_name names a
// container this pipeline transcodes. An empty format_name — a document
// with no format object at all — is not allowed: unknown is not video.
func isAllowedContainer(formatName string) bool {
	for _, name := range strings.Split(formatName, ",") {
		if _, ok := allowedFormats[strings.TrimSpace(name)]; ok {
			return true
		}
	}
	return false
}

// Probe returns a prober bound to the ffprobe binary (curried so httpapi can
// inject it). The prober runs `ffprobe -v error -print_format json
// -show_format -show_streams -protocol_whitelist file PATH` and interprets
// it per §10.3 step 2.
func Probe(ffprobe string) func(ctx context.Context, path string) (*ProbeResult, error) {
	return func(ctx context.Context, path string) (*ProbeResult, error) {
		raw, err := runFFprobe(ctx, ffprobe, path)
		if err != nil {
			return nil, err
		}
		return ParseProbe(raw)
	}
}

// runFFprobe executes the §10.3 ffprobe command. A non-zero exit is the
// file's fault (ErrPermanent); a cancelled ctx is ctx.Err().
func runFFprobe(ctx context.Context, ffprobe, path string) ([]byte, error) {
	args := append([]string{"-v", "error", "-print_format", "json", "-show_format", "-show_streams"}, probeInputArgs(path)...)
	out, se, err := runCapture(ctx, ffprobe, args...)
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
	return out, nil
}

// ParseProbe interprets an ffprobe JSON document (source side). It is
// exported so fixtures under testdata/ exercise every rule without a binary.
func ParseProbe(raw []byte) (*ProbeResult, error) {
	var doc ffprobeDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, permanent("ffprobe output unparsable: %v", err)
	}
	if isStillImage(doc.Format.FormatName) {
		return nil, permanent("no_video_stream: still image (%s)", doc.Format.FormatName)
	}
	video, ok := pickVideo(doc.Streams)
	if !ok {
		// No picture at all — a SECOND branch, not a weaker one (§20 erratum
		// 328): a file with no non-cover video stream may still be sound, and
		// it is judged by a closed allowlist of its own (parseAudio). A file
		// that HAS a video stream never reaches it, whatever is wrong with that
		// stream, so nothing the video gate refuses can be let in here.
		return parseAudio(doc, raw)
	}
	if !isAllowedContainer(doc.Format.FormatName) {
		return nil, permanent("not_a_video: unsupported container (%s)", doc.Format.FormatName)
	}
	if video.Width <= 0 || video.Height <= 0 {
		return nil, permanent("no_video_stream: video stream %d has no dimensions", video.Index)
	}
	p := &ProbeResult{
		Kind:           KindVideo,
		VideoIndex:     video.Index,
		CodedW:         video.Width,
		CodedH:         video.Height,
		VideoCodec:     video.CodecName,
		PixFmt:         video.PixFmt,
		FormatName:     doc.Format.FormatName,
		ColorPrimaries: strings.TrimSpace(video.ColorPrimaries),
		ColorTransfer:  strings.TrimSpace(video.ColorTransfer),
		ColorSpace:     strings.TrimSpace(video.ColorSpace),
		FieldOrder:     strings.ToLower(strings.TrimSpace(video.FieldOrder)),
		Raw:            json.RawMessage(append([]byte(nil), raw...)),
	}
	for _, s := range doc.Streams {
		if s.CodecType == "audio" && len(p.AudioIndexes) < maxAudioInputs {
			p.AudioIndexes = append(p.AudioIndexes, s.Index)
			p.AudioCodecs = append(p.AudioCodecs, s.CodecName)
		}
	}
	if d, ok := parseSeconds(doc.Format.Duration); ok {
		p.HasDuration, p.DurationUS = true, secondsToUS(d)
	} else if d, ok := parseSeconds(video.Duration); ok {
		p.HasDuration, p.DurationUS = true, secondsToUS(d)
	}
	p.Video = extentOf(video)
	p.Audio = audioExtent(doc.Streams, p.AudioIndexes)
	p.FPS = sourceFPS(video.AvgFrameRate, video.RFrameRate)
	p.RFPS, _ = parseRational(video.RFrameRate)
	p.Rotation, p.RotationFromTag = rotationOf(video)
	p.RotationMirrored = matrixMirroredOf(video)
	p.PrivateTags = hasPrivateTags(doc)
	p.SARNum, p.SARDen = parseSAR(video.SampleAspectRatio)
	p.DisplayW, p.DisplayH = displayDims(p.CodedW, p.CodedH, p.SARNum, p.SARDen, p.Rotation)
	return p, nil
}

// extentOf reads one stream's own [start, start+duration) span in seconds.
// Known is false unless the stream declares a usable duration: a container
// duration says how long the FILE claims to be, and a stream that stops early
// (a video track two seconds into an eight-second file — a real shape, and the
// one that made a perfectly valid trim window produce an output with no video
// stream at all) is only visible here.
func extentOf(s ffprobeStream) StreamExtent {
	start, ok := parseSeconds(s.StartTime)
	if !ok {
		start = 0 // "N/A", or the ordinary 0 that parseSeconds calls absent
	}
	d, ok := parseSeconds(s.Duration)
	if !ok {
		return StreamExtent{StartSec: start}
	}
	return StreamExtent{StartSec: start, EndSec: start + d, Known: true}
}

// audioExtent is the UNION of the extents of the audio streams the plan would
// use: the merge of §10.3 step 2 makes one track out of them, so the question
// "does this window hold any audio" is asked of all of them at once. Known is
// false unless every one of them was measured — a stream we could not measure
// might hold the audio the window needs, and the safe answer is "assume it
// does" (the cut keeps its audio and ffmpeg produces whatever exists).
func audioExtent(streams []ffprobeStream, indexes []int) StreamExtent {
	out := StreamExtent{}
	for _, idx := range indexes {
		for _, s := range streams {
			if s.Index != idx {
				continue
			}
			e := extentOf(s)
			if !e.Known {
				return StreamExtent{}
			}
			if !out.Known {
				out = e
				continue
			}
			out.StartSec = math.Min(out.StartSec, e.StartSec)
			out.EndSec = math.Max(out.EndSec, e.EndSec)
		}
	}
	return out
}

// structuralTags are the metadata keys an ISO-BMFF file carries because it IS
// one: the brand boxes every muxer writes, a track's language and handler name,
// the muxer's vendor id and its own name. None of them says anything about a
// person, a place, a device or a time.
//
// They are also EXACTLY the set the pipeline's own `-c copy -map_metadata -1
// -map_chapters -1` output carries (measured, ffmpeg 7.1.1), which makes the
// rule below a fixed point: a file this pipeline remuxed never needs remuxing
// again for its metadata.
var structuralTags = map[string]struct{}{
	"major_brand": {}, "minor_version": {}, "compatible_brands": {},
	"encoder": {}, "language": {}, "handler_name": {}, "vendor_id": {},
}

// hasPrivateTags reports whether the file carries container metadata BEYOND
// those keys — a phone's `location` / `location-eng` /
// `com.apple.quicktime.location.ISO6709`, a camera's `make` and `model`, an
// editor's `artist` or `comment`, any `creation_time`.
//
// It exists for the one branch that publishes a file's own bytes: the direct
// remux fast path (§20 erratum 200, amended). Every other path renders through
// `-map_metadata -1` and cannot carry a tag out, so this question is only ever
// asked of an upload that would otherwise be served as it stands — where such a
// tag would become public on `/original`, `/video.mp4` and the file a crawler
// fetches. Answering yes makes the file take the `-c copy` branch that strips
// them, which costs the sub-second copy the common (moov-last) case already
// pays and keeps the tier Original: not one coded byte is rewritten.
//
// It is an ALLOWLIST and not a list of bad keys, because the direction that must
// be safe is the unknown one: a vendor tag nobody here has heard of is exactly
// the kind that turns out to hold a serial number or a place.
func hasPrivateTags(doc ffprobeDoc) bool {
	if privateTagIn(doc.Format.Tags) {
		return true
	}
	// Every stream, not merely the ones a plan would map: the direct branch
	// publishes the WHOLE file, so a tag anywhere in it is a public tag.
	for _, s := range doc.Streams {
		if privateTagIn(s.Tags) {
			return true
		}
	}
	return false
}

// privateTagIn reports whether one tag map holds a non-structural key with a
// value. An EMPTY value is not metadata — ffprobe prints those for containers
// that reserve a field they never filled in — and a key is compared lowercased
// and trimmed, because a container's spelling is not a promise.
func privateTagIn(tags map[string]string) bool {
	for k, v := range tags {
		if strings.TrimSpace(v) == "" {
			continue
		}
		if _, ok := structuralTags[strings.ToLower(strings.TrimSpace(k))]; !ok {
			return true
		}
	}
	return false
}

// pickVideo returns the first video stream that is not cover art.
func pickVideo(streams []ffprobeStream) (ffprobeStream, bool) {
	for _, s := range streams {
		if s.CodecType == "video" && s.Disposition["attached_pic"] != 1 {
			return s, true
		}
	}
	return ffprobeStream{}, false
}

// sourceFPS is the source's AVERAGE frame rate: avg_frame_rate, falling back
// to r_frame_rate, then 30/1. 0/0, 0/1, a zero denominator or garbage never
// survive. It is not necessarily the rate the rendition is encoded at — a
// variable-rate source is planned from both rates, by pickFPS (§20 erratum
// 93) — but it is what the source itself averaged, which is what a trim
// compares against when it asks whether a stream copy would change anything.
func sourceFPS(avg, r string) Rational {
	if f, ok := parseRational(avg); ok {
		return f
	}
	if f, ok := parseRational(r); ok {
		return f
	}
	return defaultFPS
}

// isHDRTransfer reports whether a color_transfer names a high-dynamic-range
// transfer function: PQ (`smpte2084`, HDR10/Dolby Vision base layer) or HLG
// (`arib-std-b67`). Those are the two an SDR rendition must tone map rather
// than merely truncate to 8 bits (§10.3 step 4). Everything else — bt709,
// bt470bg, smpte170m, an empty/`unknown` field — is already a display-light
// signal and is left alone.
func isHDRTransfer(transfer string) bool {
	switch strings.ToLower(strings.TrimSpace(transfer)) {
	case "smpte2084", "arib-std-b67":
		return true
	}
	return false
}

// isUnknownColour reports whether a probed colour field says nothing. ffprobe
// omits an unspecified field from its JSON entirely (so the parsed value is
// ""), but a container can also carry the enum's own placeholders, and a
// filter graph must treat all of them alike: a frame that does not describe
// its gamut is a frame zscale cannot convert (§20 erratum 93, amended).
func isUnknownColour(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "unknown", "unspecified", "reserved":
		return true
	}
	return false
}

// parseRational reads "num/den" (or a bare integer) into positive ints.
func parseRational(s string) (Rational, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Rational{}, false
	}
	numS, denS, hasSlash := strings.Cut(s, "/")
	num, err := strconv.Atoi(numS)
	if err != nil {
		return Rational{}, false
	}
	den := 1
	if hasSlash {
		if den, err = strconv.Atoi(denS); err != nil {
			return Rational{}, false
		}
	}
	if num <= 0 || den <= 0 {
		return Rational{}, false
	}
	return Rational{num, den}, true
}

// rotationOf scans EVERY side_data_list entry for the Display Matrix (HDR
// metadata may occupy index 0), then falls back to the legacy rotate tag
// (any case: MKV reports ROTATE), and normalizes to {0, 90, 180, 270}.
// fromTag reports which source supplied the value (see ProbeResult).
//
// The matrix coefficients are preferred over the sibling "rotation" key
// because only they carry the angle ffmpeg's autorotate actually decides on
// (§20 erratum 47); "rotation" is the fallback for a side data entry that
// carries no coefficients (an older ffprobe, a hand-written fixture).
func rotationOf(s ffprobeStream) (rotation int, fromTag bool) {
	for _, sd := range s.SideDataList {
		if sd.Type != sideDataDisplayMatrix {
			continue
		}
		if raw, ok := matrixRotation(sd.DisplayMatrix); ok {
			return autorotateQuarter(raw), false
		}
		if sd.Rotation != nil {
			return normalizeRotation(*sd.Rotation), false
		}
	}
	for k, v := range s.Tags {
		if strings.EqualFold(k, "rotate") {
			if r, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				return normalizeRotation(r), true
			}
		}
	}
	return 0, false
}

// rotationEpsilon is how far from a quarter turn a rotation ANGLE STATED AS
// A NUMBER may sit and still count as that turn: encoders write the matrix
// as 16.16 fixed point, so a "90" can arrive as 89.9998, but nothing
// legitimate arrives as 45. It governs the legacy rotate tag (whose turn
// this package emits itself, so any tolerance is self-consistent) and the
// Display Matrix fallback for a side data entry with no coefficients; the
// coefficients themselves are classified exactly, by autorotateQuarter.
const rotationEpsilon = 1.0

// displayMatrixDen is the 16.16 fixed-point denominator of the matrix
// coefficients. Dividing by it is exact (a power of two), so this mirrors
// libavutil's CONV_FP without changing a single rounding.
const displayMatrixDen = 65536.0

// matrixRotation recovers the exact Display Matrix angle from the nine
// coefficients ffprobe prints under "displaymatrix", reproducing
// av_display_rotation_get: the scale-normalized atan2 of the first two
// coefficients of the top row, negated, in degrees. Row labels ("00000001:")
// are skipped; anything but exactly nine integers, or a degenerate matrix
// with a zero scale, reports false so the caller falls back.
func matrixRotation(s string) (float64, bool) {
	m, ok := parseDisplayMatrix(s)
	if !ok {
		return 0, false
	}
	sx, sy := math.Hypot(m[0], m[3]), math.Hypot(m[1], m[4])
	if sx == 0 || sy == 0 {
		return 0, false
	}
	return -math.Atan2(m[1]/sy, m[0]/sx) * 180 / math.Pi, true
}

// parseDisplayMatrix reads the nine coefficients ffprobe prints under
// "displaymatrix" (row labels skipped), scaled out of 16.16 fixed point.
// Anything but exactly nine integers reports false.
func parseDisplayMatrix(s string) ([9]float64, bool) {
	var m [9]float64
	n := 0
	for _, f := range strings.Fields(s) {
		if strings.HasSuffix(f, ":") {
			continue // row label
		}
		v, err := strconv.ParseInt(f, 10, 64)
		if err != nil || n == len(m) {
			return m, false
		}
		m[n], n = float64(v)/displayMatrixDen, n+1
	}
	return m, n == len(m)
}

// matrixMirroredOf reports whether the video stream's Display Matrix MIRRORS
// the picture as well as turning it: the 2×2 linear part has a negative
// determinant. ffmpeg's autorotate then inserts a flipping filter
// (`cclock_flip`, `clock_flip`, a lone `hflip`/`vflip`) that a plain quarter
// turn does not reproduce, so the CUDA graph — which must turn the frames
// itself (§20 erratum 352) — refuses such a source and leaves it on the CPU
// graph, where autorotate does the whole job. A stream with no coefficients
// (only the "rotation" key) is read as unmirrored: the key cannot say.
func matrixMirroredOf(s ffprobeStream) bool {
	for _, sd := range s.SideDataList {
		if sd.Type != sideDataDisplayMatrix {
			continue
		}
		if m, ok := parseDisplayMatrix(sd.DisplayMatrix); ok {
			return m[0]*m[4]-m[1]*m[3] < 0
		}
		return false
	}
	return false
}

// autorotateQuarter classifies a raw Display Matrix angle the way ffmpeg's
// own autorotate does, so the swapped dims this package plans and the frame
// ffmpeg hands it can never disagree (§20 erratum 47): ffmpeg ROUNDS the raw
// angle to whole degrees (get_rotation: -round(av_display_rotation_get)),
// normalizes into [0, 360), and only then inserts a transpose on an EXACT
// quarter — |θ−90k| < 1 over an integer means equality. So 89.4° is turned
// in place by a rotate filter on the original canvas (dims unchanged) while
// 89.6° is transposed (dims swapped), and the two must be classified apart
// even though ffprobe prints "rotation": 89 for both. The result is returned
// in ffprobe's sign convention (the negation of ffmpeg's), which is what
// displayDims and ProbeResult.Rotation speak.
func autorotateQuarter(raw float64) int {
	if math.IsNaN(raw) || math.IsInf(raw, 0) {
		return 0
	}
	theta := math.Mod(-math.Round(raw), 360)
	if theta < 0 {
		theta += 360
	}
	switch theta {
	case 90, 180, 270:
		return (360 - int(theta)) % 360
	}
	return 0
}

// normalizeRotation maps an angle within rotationEpsilon of a quarter turn
// onto that turn in [0, 360), and reports every other angle as 0 (§20
// erratum 39). Rotation exists here ONLY to swap the display dims, and a
// non-quarter angle must not swap them: ffmpeg's autorotate does not turn an
// odd angle into a swapped frame — it rotates in place on the original
// canvas — so snapping 45° to 90° made the plan scale an unrotated landscape
// into a portrait box.
func normalizeRotation(r float64) int {
	if math.IsNaN(r) || math.IsInf(r, 0) {
		return 0
	}
	r = math.Mod(r, 360) // (-360, 360): the quarter count cannot overflow
	q := math.Round(r / 90)
	if math.Abs(r-q*90) > rotationEpsilon {
		return 0
	}
	return ((int(q) % 4) + 4) % 4 * 90
}

// parseSAR reads "N:M"; missing, "0:1", "N/A", "1:1" or garbage are square.
func parseSAR(s string) (int, int) {
	s = strings.TrimSpace(s)
	n, m, ok := strings.Cut(s, ":")
	if !ok {
		return 1, 1
	}
	num, err1 := strconv.Atoi(n)
	den, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || num <= 0 || den <= 0 {
		return 1, 1
	}
	return num, den
}

// displayDims applies SAR to the coded width, then swaps for 90/270.
func displayDims(codedW, codedH, sarNum, sarDen, rotation int) (int, int) {
	w := int(math.Round(float64(codedW) * float64(sarNum) / float64(sarDen)))
	if w < 1 {
		w = 1
	}
	h := codedH
	if rotation == 90 || rotation == 270 {
		return h, w
	}
	return w, h
}

// parseSeconds reads ffprobe's decimal seconds; "N/A", empty or ≤0 is absent.
func parseSeconds(s string) (float64, bool) {
	d, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(d) || math.IsInf(d, 0) || d <= 0 {
		return 0, false
	}
	return d, true
}

func secondsToUS(d float64) int64 { return int64(math.Round(d * 1e6)) }
