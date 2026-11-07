package transcode

import (
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"os"
	"regexp"
	"strconv"
)

// Poster selection (§10.3 step 6, §20 erratum 92).
//
// The poster is not a thumbnail of a video player: it is the picture Discord
// and X put in the card, the `poster` of the <video> element, and the only
// frame most people ever see of a clip. "Seek 15% in" was the whole rule, and
// 15% of a game clip is very often still the loading screen — the same black
// frame the spec forbade at t=0, moved along the timeline.
//
// So the frame is CHOSEN rather than guessed: one `thumbnail` pass over a
// bounded window picks the most representative frame of the frames it sees,
// the old seek ladder stands behind it, and every candidate is decoded and
// measured before it is accepted. A near-black candidate is rejected in
// favour of the next one — but a clip that really is black still gets a
// poster (the last candidate is kept), because a missing card image is worse
// than a dark one and the poster must never fail a clip.
const (
	posterSeekFrac    = 0.15
	posterTailGuard   = 0.5 // seconds kept clear of the end
	posterUnknownSeek = 1.0 // seconds, when the duration is unknown
	// posterThumbFrames is how many consecutive frames `thumbnail` compares
	// before it emits the most representative one. It buffers them, so the
	// number is also the memory this pass costs: 100 frames of 720p yuv420p
	// is ≈140 MB, held for the moment one bounded auxiliary run takes, under
	// the same ffmpeg semaphore the encodes hold.
	posterThumbFrames = 100
	// posterThumbWindow bounds what that pass READS. `-ss`/`-t` in front of
	// `-i` mean a two-hour rendition is demuxed for twenty seconds, and a
	// window this size holds the whole of a normal intro at any frame rate.
	posterThumbWindow = 20.0
	// posterMinMeanLuma / posterMinMaxLuma are the near-black test, on the
	// 0-255 scale ffmpeg's signalstats prints. BOTH must fail for a frame to
	// be rejected: a dark night scene has a mean near zero and highlights
	// well past 24, while a fade-in or a loading screen has neither.
	posterMinMeanLuma = 8.0
	posterMinMaxLuma  = 24.0
	// posterLumaStride decimates the luma measurement: every 8th pixel in
	// each direction is 1/64th of the work and the same verdict.
	posterLumaStride = 8
)

// PosterResult is the poster that was written: the JPEG's MEASURED
// dimensions (og:image:width/height read them), the timestamp of the FRAME
// it holds, and whether it survived only because everything was black.
//
// Seconds is the chosen frame's own time, not the window's start: the
// thumbnail pass reports which frame it picked and this is that frame
// (§20 erratum 92(c), amended). A fallback candidate is a single-frame seek,
// so for those two the frame's time and the seek are the same thing.
type PosterResult struct {
	Width, Height int
	Seconds       float64
	NearBlack     bool
}

// PosterSeek returns the poster timestamp in seconds:
// clamp(0.15·duration, 0, duration − 0.5); unknown duration → 1.0.
//
// The duration it clamps against is the SHORTER of the container's and the
// video stream's own end (videoEndSec, 0 when unmeasured). A capture whose
// device died mid-recording is a 2 s picture in a 40 s file, and the
// container's answer sends the seek 38 s past the last frame — ffmpeg writes
// nothing and the ladder falls back to exactly the frame this rule exists to
// avoid.
func PosterSeek(durationUS int64, hasDuration bool, videoEndSec float64) float64 {
	start, _ := posterPlan(durationUS, hasDuration, videoEndSec)
	return start
}

// posterPlan is where the poster is looked for: the seek, and how many
// seconds of picture the thumbnail pass may read from there.
func posterPlan(durationUS int64, hasDuration bool, videoEndSec float64) (start, window float64) {
	d, ok := posterExtent(durationUS, hasDuration, videoEndSec)
	if !ok {
		return posterUnknownSeek, posterThumbWindow
	}
	start = posterSeekFrac * d
	if limit := d - posterTailGuard; start > limit {
		start = limit
	}
	if start < 0 {
		start = 0
	}
	window = d - start
	if window <= 0 || window > posterThumbWindow {
		window = posterThumbWindow
	}
	return start, window
}

// posterExtent is the span the poster may seek inside: the container's
// duration and the video stream's end, whichever is shorter, and whichever
// of the two was measured at all.
func posterExtent(durationUS int64, hasDuration bool, videoEndSec float64) (float64, bool) {
	if math.IsNaN(videoEndSec) || math.IsInf(videoEndSec, 0) {
		videoEndSec = 0
	}
	d, ok := 0.0, false
	if hasDuration && durationUS > 0 {
		d, ok = float64(durationUS)/1e6, true
	}
	if videoEndSec > 0 && (!ok || videoEndSec < d) {
		d, ok = videoEndSec, true
	}
	return d, ok
}

// posterSecArg renders a poster timestamp for the argv (milliseconds are as
// fine as a seek needs to be).
func posterSecArg(s float64) string { return strconv.FormatFloat(s, 'f', 3, 64) }

// PosterBox is the frame a poster pass writes its picture INSIDE (§20 erratum
// 219).
//
// It exists because of what the card image must NOT be cut from. The LIGHT copy
// carries erratum 214's burned-in line, and a poster is not one of the artefacts
// that amendment covers — it is an og:image, the still the site itself shows
// over its player, and a JPEG anybody can download — so the poster is taken from
// an UNTAGGED file: the Full, the remuxed original, or the upload itself. What
// the Light copy did give the poster was its SIZE: a still cut from a 4K frame is
// a card image no crawler inlines. So the pass scales instead, into the box the
// clip's LIGHT plan computed.
//
// The zero value is "no scaling", and it is what every caller passes whose input
// is already the size the card wants (a trim's own rendition, an owner's chosen
// frame): the argv is then exactly what it always was.
type PosterBox struct{ W, H int }

// scaling reports whether this box says anything at all.
func (b PosterBox) scaling() bool { return b.W > 0 && b.H > 0 }

// filter is the box's one filter link, and it is the LIGHT encode's own scale
// without the square-pixel and pixel-format steps a JPEG has no use for.
//
// `scale=W:H` rather than a fit-inside expression, deliberately: W and H were
// computed from THIS source's display dimensions (MakePlan), so they already
// carry its aspect ratio, and the frame this pass reads cannot arrive with a
// non-square pixel aspect — an anamorphic upload is never playable
// (NotPlayableAnamorphic), so it goes down the Full path and what is read here
// is that encode's output at SAR 1. The result is therefore geometrically the
// card copy's own frame, which is the point of taking it inside this box.
func (b PosterBox) filter() string {
	return "scale=" + strconv.Itoa(b.W) + ":" + strconv.Itoa(b.H) + ":flags=lanczos"
}

// PosterArgs renders the ffmpeg argv for one frame at seek seconds, scaled into
// `box` when it says anything.
func PosterArgs(in, out string, seek float64, box PosterBox) []string {
	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-ss", posterSecArg(seek),
	}
	args = append(args, inputArgs(in)...)
	args = append(args, "-map", "0:v:0")
	if box.scaling() {
		args = append(args, "-vf", box.filter())
	}
	return append(args,
		"-frames:v", "1", "-update", "1", "-q:v", "3", "-f", "image2", out,
	)
}

// posterPickKey is the frame metadata the thumbnail pass adds purely so that
// the next filter has something to print. `metadata=print` emits its
// `frame:N pts:… pts_time:…` header line only when the frame CARRIES
// metadata, and a decoded video frame usually carries none — measured on
// 7.1.1, the print is silent without the add.
const posterPickKey = "clippet.poster"

// posterPickSink is where that print goes: ffmpeg's own stdout, which is
// free in both poster passes (the JPEG goes to a file and the pick pass
// muxes to null) and which runCapture already collects. It is a CONSTANT on
// purpose — a real path would have to be escaped for the filter parser,
// twice, and a scratch path this package does not otherwise need is a
// scratch path it can leave behind. The doubled backslash is that escaping:
// the filter parser reads `\\:` as a literal colon inside an option value.
const posterPickSink = `pipe\\:1`

// posterThumbFilter is the thumbnail pass's -vf: pick the most
// representative frame of the window, then SAY which one it was.
func posterThumbFilter() string {
	return "thumbnail=n=" + strconv.Itoa(posterThumbFrames) +
		",metadata=mode=add:key=" + posterPickKey + ":value=1" +
		",metadata=mode=print:file=" + posterPickSink
}

// PosterThumbArgs renders the first candidate: `thumbnail` scores
// posterThumbFrames consecutive frames from `start` against their own
// average and emits the one that stands out, so a fade-in is skipped for the
// frame the clip is actually about. `-ss` and `-t` sit in front of `-i`, so
// the window is what is READ.
//
// The box's scale goes at the END of the chain, after the selection and after
// the print: the frames are scored at the size they were decoded at, and the one
// frame that survives is the only one resized.
func PosterThumbArgs(in, out string, start, window float64, box PosterBox) []string {
	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-ss", posterSecArg(start), "-t", posterSecArg(window),
	}
	args = append(args, inputArgs(in)...)
	vf := posterThumbFilter()
	if box.scaling() {
		vf += "," + box.filter()
	}
	return append(args,
		"-map", "0:v:0", "-vf", vf,
		"-frames:v", "1", "-update", "1", "-q:v", "3", "-f", "image2", out,
	)
}

// PosterPickArgs is PosterThumbArgs without the picture: the same window and
// the same selection, muxed to null, so the only thing it produces is the
// chosen frame's timestamp on stdout. It is what a caller runs when it wants
// to know WHERE the poster came from on a file it is not writing a poster
// for (§20 erratum 92(c), amended).
func PosterPickArgs(in string, start, window float64) []string {
	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-ss", posterSecArg(start), "-t", posterSecArg(window),
	}
	args = append(args, inputArgs(in)...)
	return append(args, "-map", "0:v:0", "-vf", posterThumbFilter(), "-frames:v", "1", "-f", "null", "-")
}

// posterPickLine matches the header `metadata=print` writes for a frame:
// `frame:0    pts:45056   pts_time:2.933333`. The time is relative to the
// window's start, because `-ss` in front of `-i` shifts the output timeline
// to zero.
var posterPickLine = regexp.MustCompile(`(?m)^frame:\S*\s+pts:\S*\s+pts_time:(\S+)`)

// posterPickedSeconds turns that print into an absolute timestamp. Anything
// unreadable falls back to the window's start, which is what the poster used
// to report for every frame: a pass that chose well but described itself
// badly must not be worse than one that never described itself at all.
func posterPickedSeconds(stdout []byte, start float64) float64 {
	m := posterPickLine.FindSubmatch(stdout)
	if m == nil {
		return start
	}
	pts, err := strconv.ParseFloat(string(m[1]), 64)
	if err != nil || math.IsNaN(pts) || math.IsInf(pts, 0) || pts < 0 {
		return start
	}
	return start + pts
}

// PosterFrameSeconds reports the timestamp the poster's own rule would land
// on for this file, without writing a poster: the thumbnail pass's chosen
// frame, or the plain seek when the pass says nothing (an unreadable print,
// a failed run, a cancelled context — none of which is worth failing over,
// because the seek is still a legal answer).
//
// It exists because the poster's timestamp is CHOSEN and nothing stores it:
// the clip row holds five poster columns and none of them is a time. A
// caller that has to start where the poster started re-derives it here
// (§20 erratum 92(c), amended).
func PosterFrameSeconds(ctx context.Context, ffmpeg, in string, durationUS int64, hasDuration bool, videoEndSec float64) float64 {
	start, window := posterPlan(durationUS, hasDuration, videoEndSec)
	out, _, err := runCapture(ctx, ffmpeg, PosterPickArgs(in, start, window)...)
	if err != nil || ctx.Err() != nil {
		return start
	}
	return posterPickedSeconds(out, start)
}

// Poster extracts the poster JPEG from the OUTPUT rendition. It tries, in
// order: the thumbnail pass over the window at PosterSeek, one frame at
// PosterSeek, one frame at 0 — and takes the first candidate that decodes as
// a JPEG and is not near-black. If every candidate is near-black the LAST one
// is kept (NearBlack in the result says so); only a candidate list that
// produced no readable JPEG at all is a permanent poster_failed.
//
// `box` bounds the picture every candidate is written at (PosterBox); its zero
// value means the frame is written at the size it was decoded at.
//
// `label` is the duration burned into the accepted frame's top-right corner (§20
// erratum 287, posterlabel.go). It is drawn ONCE, on whatever this pass settled
// on, and after the near-black measurement — a plate burned into a candidate
// before it was measured would lift its maximum luma past the test and the
// near-black rule would stop rejecting anything. Its zero value draws nothing.
func Poster(ctx context.Context, ffmpeg, in, out string, durationUS int64, hasDuration bool,
	videoEndSec float64, box PosterBox, label PosterLabel) (PosterResult, error) {
	start, window := posterPlan(durationUS, hasDuration, videoEndSec)
	// Candidates are rendered beside the poster and moved onto it only once
	// they are measured, so a rejected frame never becomes the poster and a
	// stale file from a previous attempt is never mistaken for this one's.
	tmp := out + ".cand"
	_ = os.Remove(out)
	defer os.Remove(tmp)

	type candidate struct {
		args []string
		// seconds is where this candidate looked; picked says the run
		// REPORTS which frame it took, so the result carries the frame's own
		// time rather than the window's start.
		seconds float64
		picked  bool
	}
	candidates := []candidate{
		{PosterThumbArgs(in, tmp, start, window, box), start, true},
		{PosterArgs(in, tmp, start, box), start, false},
	}
	if start != 0 {
		candidates = append(candidates, candidate{PosterArgs(in, tmp, 0, box), 0, false})
	}

	var (
		last string
		kept PosterResult
		have bool
	)
	for _, c := range candidates {
		_ = os.Remove(tmp)
		stdout, se, runErr := runCapture(ctx, ffmpeg, c.args...)
		if ctx.Err() != nil {
			return PosterResult{}, ctx.Err()
		}
		if runErr != nil {
			last = exitReason("ffmpeg", runErr, se)
			continue
		}
		frame, err := jpegStats(tmp)
		if err != nil {
			last = err.Error()
			continue
		}
		if err := os.Rename(tmp, out); err != nil {
			last = fmt.Sprintf("poster unmovable: %v", err)
			continue
		}
		seconds := c.seconds
		if c.picked {
			seconds = posterPickedSeconds(stdout, c.seconds)
		}
		res := PosterResult{Width: frame.Width, Height: frame.Height, Seconds: seconds, NearBlack: frame.nearBlack()}
		kept, have = res, true
		if !res.NearBlack {
			break
		}
	}
	if !have {
		return PosterResult{}, permanent("poster_failed: %s", last)
	}
	// The label goes on the frame that WON — once, whichever candidate that was,
	// and after every measurement in the loop above (§20 erratum 287). It cannot
	// fail this pass: a poster with no number in the corner is the poster this
	// function returned before the erratum existed.
	drawPosterLabel(ctx, ffmpeg, out, label, kept.Height)
	return kept, nil
}

// posterFrame is one measured candidate: the JPEG's real dimensions and how
// lit it is.
type posterFrame struct {
	Width, Height     int
	MeanLuma, MaxLuma float64
}

// nearBlack reports the frame this rule refuses to publish as a card image:
// dark on average AND with nothing bright anywhere in it.
func (f posterFrame) nearBlack() bool {
	return f.MeanLuma < posterMinMeanLuma && f.MaxLuma < posterMinMaxLuma
}

// jpegStats decodes a poster candidate and measures it. Decoding (rather
// than reading the header, as the dimensions alone would need) is the point:
// the width and height come from the same pass that says whether there is a
// picture in the frame at all.
func jpegStats(path string) (posterFrame, error) {
	f, err := os.Open(path)
	if err != nil {
		return posterFrame{}, fmt.Errorf("poster missing: %w", err)
	}
	defer f.Close()
	// jpeg.Decode, not image.Decode: the muxer is image2 with a JPEG
	// encoder, so anything else here is not our output.
	img, err := jpeg.Decode(f)
	if err != nil {
		return posterFrame{}, fmt.Errorf("poster undecodable: %w", err)
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return posterFrame{}, fmt.Errorf("poster is %dx%d, want a real picture", b.Dx(), b.Dy())
	}
	frame := measureLuma(img)
	frame.Width, frame.Height = b.Dx(), b.Dy()
	return frame, nil
}

// measureLuma samples every posterLumaStride-th pixel in each direction and
// returns the mean and maximum Rec. 601 luma on the 0-255 scale — the scale
// ffmpeg's signalstats prints, so a failing poster can be measured by hand
// with the same numbers.
func measureLuma(img image.Image) posterFrame {
	b := img.Bounds()
	var sum, top float64
	n := 0
	for y := b.Min.Y; y < b.Max.Y; y += posterLumaStride {
		for x := b.Min.X; x < b.Max.X; x += posterLumaStride {
			// RGBA() is 16-bit alpha-premultiplied; 257 maps it onto 0-255.
			r, g, bl, _ := img.At(x, y).RGBA()
			l := (0.299*float64(r) + 0.587*float64(g) + 0.114*float64(bl)) / 257
			sum += l
			if l > top {
				top = l
			}
			n++
		}
	}
	if n == 0 {
		return posterFrame{}
	}
	return posterFrame{MeanLuma: sum / float64(n), MaxLuma: top}
}
