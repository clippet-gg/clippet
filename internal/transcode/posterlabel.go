package transcode

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The duration burned into the poster (DESIGN.md §20 erratum 287).
//
// The owner's ask of 2026-09-15: bake a big length text into the top right of
// the thumbnail, because it is only useful before clicking play anyway. That
// last reason is the whole specification. The poster is the picture Discord and X put
// in the card and the `poster` attribute of the <video> element; it is what a
// reader looks at while deciding whether to click, and "how long is this" is the
// one fact that decision needs and the one Discord will not render (`video:
// duration` is emitted, is correct, and produces nothing in the client —
// measured 2026-09-14). The moment anybody presses play the poster is gone, so
// the label costs the clip nothing at all.
//
// IT IS NOT A WATERMARK, and the anti-features list (medal-feature-research.md,
// §15) is not bent for it. A watermark is a brand mark that says who made the
// artefact and exists so the artefact advertises its maker; this carries no
// brand, no name and no logo — it is one measurement of the file it is drawn on,
// in the same relationship to the picture as the "3:42" a video player prints
// over its own thumbnail. The LIGHT tag (erratum 214) is the one thing in this
// product that could be argued either way, and it is on the Light copy alone.
// Nothing else changes: the video, the Original, the Full, the GIF, the
// filmstrip, the Light copy and the frames an owner picks from are all untouched
// — only `poster.jpg` carries it, and only the served one, never the source.

const (
	// PosterLabelHeightFrac is the type size as a share of the poster's height:
	// 4.5 %, which is 32 px on a 720p card image. The owner asked for a big length
	// text, and at a card's rendered size in a Discord message (roughly
	// 400 px wide) that is still a ~18 px line — read at a glance, and a
	// fourteenth of the frame's height.
	PosterLabelHeightFrac = 0.045
	// PosterLabelMinSize is the floor in pixels, for the small frames a narrowed
	// TRANSCODE_MAX_HEIGHT produces. Below it the label is texture, not text.
	PosterLabelMinSize = 14
	// PosterLabelMarginFrac is the inset of the BOX from the top-right corner as
	// a share of the frame height: 12 px at 720p, scaled with the picture, so a
	// vertical 1080×1920 poster and a 1280×720 one both look inset by the same
	// proportion of what a viewer sees.
	PosterLabelMarginFrac = 12.0 / 720.0
	// PosterLabelMinMargin keeps the box off the very edge on a tiny frame.
	PosterLabelMinMargin = 4
	// PosterLabelPadFrac is the box's padding around the text, as a share of the
	// type size. 0.32 is ~10 px at 32 px type: enough that the ground reads as a
	// plate under the number rather than as a tight outline.
	PosterLabelPadFrac = 0.32
	// PosterLabelMinPad is that padding's floor.
	PosterLabelMinPad = 3
	// PosterLabelTextColor is the dark palette's --text and PosterLabelBoxColor
	// its --bg at 72 %. On a PICTURE the dark palette is spent outright whatever
	// the page theme (web/STYLE.md, the rule erratum 246 set for the embed
	// overlay): there is no page ground to agree with.
	//
	// The plate is what makes the label legible on any frame rather than on most
	// of them, and its numbers are why it is there at all: composited over WHITE
	// the ground becomes ≈#595957 and the text reads 5.8:1; over BLACK it is
	// ≈#121110 and the text reads 15.5:1. Both clear WCAG's 4.5:1 for body text,
	// on the two worst frames a clip can have. The alternative — bare text with a
	// shadow, which is what the embed overlay uses because a logo may not grow a
	// box — is 1.21:1 over white before the shadow does anything, and even with
	// the shadow it is the pixels UNDER each glyph that go dark while the top edge
	// stays near 1.1:1. Fine for a mark a reader only has to recognise; not fine
	// for a number they have to read.
	PosterLabelTextColor = "0xECE9E2"
	PosterLabelBoxColor  = "0x191816@0.72"
)

// The poster label's fonts: this project's own mono face where the images
// install it, followed by the LIGHT tag's fonts as a fallback for an operator
// who built their own image, followed by whatever is in `fonts` beside the
// binary.
//
// Spline Sans Mono is the face because web/STYLE.md gives it exactly this job:
// "Spline Sans Mono for things a machine measured or a person copies (timecodes,
// sizes, prefixes, the state cell)" — a duration is a timecode, and the page
// prints the same number in the same face two inches below the poster. It is
// under the SIL Open Font License 1.1; the licence text travels with it in
// web/static/fonts/OFL.txt, the file carries its own copyright and licence URL
// in its name table, and DEPLOY §27 records both. The repository holds ONE copy
// of it (web/static/fonts/spline-sans-mono.ttf, a 24 KB static instance at
// weight 500, the same family and weight the browser loads as woff2); the
// Dockerfile installs that copy into the image.
//
// The list is searched and never configured, for the reason tagFontPaths
// is: a POSTER_LABEL_FONT variable would be one more thing to get wrong for a
// cosmetic line. An image with none of these draws no label, exactly as an image
// with no DejaVu draws no Light tag.

// posterFontInstalled is where the image installs this project's own mono face.
const posterFontInstalled = "/usr/local/share/clippet/fonts/spline-sans-mono.ttf"

// posterFontList is that face, then the LIGHT tag's installed paths, then
// — last — the two faces `fonts` beside the binary may hold (§20 erratum 308,
// the same searched path lighttag.go documents; an empty exeFonts omits both).
//
// The project's OWN face leads each half: the installed copy leads the
// installed paths, and the copy beside the binary leads the fallbacks beside
// the binary. A Windows operator who put both files in `fonts/` gets the mono
// face this label is designed in, not the sans that is only there so an
// operator who built their own image still gets a label.
func posterFontList(exeFonts string) []string {
	c := append([]string{posterFontInstalled}, tagFontPaths()...)
	if exeFonts != "" {
		c = append(c,
			filepath.Join(exeFonts, "spline-sans-mono.ttf"),
			filepath.Join(exeFonts, "DejaVuSans.ttf"))
	}
	return c
}

// posterFontCandidates is that list for THIS binary, built once.
var posterFontCandidates = posterFontList(exeFontsDir)

// FindPosterFont returns the first font of posterFontCandidates that exists, or
// "". cmd/clippet resolves it once at boot, beside FindTagFont, and a missing
// font costs a poster its label and must never cost a clip its card image.
func FindPosterFont() string { return findFont(posterFontCandidates) }

// PosterLabel is the duration one poster pass burns into its top-right corner.
// The zero value draws nothing, which is what every caller that has no font, no
// measured duration, or no business labelling a frame passes.
type PosterLabel struct {
	FontFile   string
	DurationMS int64
}

// Valid reports whether this label can be drawn at all. A zero or negative
// duration is "not measured yet", not "0:00": a card that claimed a clip was
// zero seconds long would be worse than one that said nothing.
func (l PosterLabel) Valid() bool { return l.FontFile != "" && l.DurationMS > 0 }

// Text is what gets drawn: `m:ss`, or `h:mm:ss` past the hour.
func (l PosterLabel) Text() string { return FormatDuration(l.DurationMS) }

// FormatDuration renders milliseconds as m:ss or h:mm:ss.
//
// It is the THIRD spelling of twelve lines of arithmetic in this repository, and
// like the other two it is pinned against ONE table, testdata/duration.tsv.
// internal/notify's copy exists because DESIGN §3's import graph has no edge
// from the outbound-call package to anything that could hold it; this one exists
// because the graph has no edge OUT of internal/transcode at all — it is at the
// bottom of it. internal/web's `formatDuration` is no longer a copy: that
// package already imports this one (for QualityShort), so it delegates here, and
// what a viewer reads under the player, on the card and burned into the poster
// is now literally one function.
func FormatDuration(ms int64) string {
	total := (ms + 500) / 1000
	h, m, sec := total/3600, (total%3600)/60, total%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	}
	return fmt.Sprintf("%d:%02d", m, sec)
}

// posterLabelMetrics is the label's geometry on a frame of this height: the type
// size, the box's padding, and the inset of the box from the corner.
func posterLabelMetrics(frameH int) (size, pad, margin int) {
	size = int(math.Round(float64(frameH) * PosterLabelHeightFrac))
	if size < PosterLabelMinSize {
		size = PosterLabelMinSize
	}
	pad = int(math.Round(float64(size) * PosterLabelPadFrac))
	if pad < PosterLabelMinPad {
		pad = PosterLabelMinPad
	}
	margin = int(math.Round(float64(frameH) * PosterLabelMarginFrac))
	if margin < PosterLabelMinMargin {
		margin = PosterLabelMinMargin
	}
	return size, pad, margin
}

// posterLabelFilter renders the one drawtext link that draws the label.
//
// `x`/`y` are drawtext's own expressions over the text metrics, so the box sits
// against the corner whatever the font does with the characters. drawtext draws
// its box from `x-boxborderw` to `x+tw+boxborderw` (and the same vertically),
// which is why the padding is added to BOTH offsets: the inset is the box's, not
// the glyphs'.
//
// The corner is the TOP-right and not the bottom-right, which is where the LIGHT
// tag goes: the two must never collide, a poster is cut from an UNTAGGED file
// (erratum 219) but the Light copy's own frames are not, and the bottom band of
// a <video> is where the native controls live — a duration under them would be
// unreadable on the share page for the whole time it is visible.
//
// The TEXT goes through a FILE and never through the argv, the rule lighttag.go
// set: drawtext's `text=` value is parsed at two levels and `0:42` carries the
// option separator in the middle of it.
func posterLabelFilter(fontFile, textFile string, frameH int) string {
	size, pad, margin := posterLabelMetrics(frameH)
	off := strconv.Itoa(margin + pad)
	var b strings.Builder
	b.WriteString("drawtext=fontfile=")
	b.WriteString(escapeFilterValue(fontFile))
	b.WriteString(":textfile=")
	b.WriteString(escapeFilterValue(textFile))
	// expansion=none for the reason the Light tag has it: a `%` in the value
	// would otherwise be read as a drawtext expression. Nothing here can carry
	// one (Text is digits and colons), and the belt costs nothing.
	b.WriteString(":expansion=none:fontsize=")
	b.WriteString(strconv.Itoa(size))
	b.WriteString(":fontcolor=")
	b.WriteString(PosterLabelTextColor)
	b.WriteString(":box=1:boxcolor=")
	b.WriteString(PosterLabelBoxColor)
	b.WriteString(":boxborderw=")
	b.WriteString(strconv.Itoa(pad))
	b.WriteString(":x=w-tw-")
	b.WriteString(off)
	b.WriteString(":y=")
	b.WriteString(off)
	return b.String()
}

// PosterLabelArgs renders the ffmpeg argv of the label pass: one JPEG in, the
// same JPEG with the label in.
//
// `-q:v 2` and not the poster passes' 3: this frame is being JPEG-encoded a
// second time, and spending the one quality step keeps the re-encode off the
// picture. The pass cannot change the dimensions — drawtext draws, it does not
// scale — so the measured `og:image:width/height` the caller already holds stay
// true.
//
// An audio clip's poster is a PNG (§20 erratum 329): its label pass writes a
// PNG back, losslessly, with the bitexact flags so the encoder adds no text
// chunk naming itself.
func PosterLabelArgs(in, out, fontFile, textFile string, frameH int) []string {
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y"}
	args = append(args, inputArgs(in)...)
	args = append(args, "-map", "0:v:0", "-vf", posterLabelFilter(fontFile, textFile, frameH), "-frames:v", "1", "-update", "1")
	if strings.HasSuffix(strings.ToLower(in), ".png") {
		args = append(args, "-c:v", "png", "-map_metadata", "-1")
		args = append(args, bitexactArgs...)
		return append(args, "-f", "image2", out)
	}
	return append(args, "-q:v", "2", "-f", "image2", out)
}

// drawPosterLabel burns the label into the poster at `path`, in place.
//
// It NEVER fails a poster. Every way this can go wrong — no font in the image,
// an unmeasured duration, an unwritable scratch file, an ffmpeg that refuses the
// filter, a cancelled context — leaves the unlabelled picture exactly where it
// was, which is a perfectly good card image and is what every poster before this
// erratum was. The caller does not even get an error to classify: a clip must
// not fail over a number in a corner.
//
// It runs AFTER the poster has been chosen and measured, and that order is
// load-bearing. poster.go rejects a near-black candidate by the luma of the
// frame it decoded; a bright plate burned into the corner first would lift every
// candidate's maximum luma past the test and the near-black rule would stop
// rejecting anything.
func drawPosterLabel(ctx context.Context, ffmpeg, path string, l PosterLabel, frameH int) {
	if !l.Valid() || frameH <= 0 || ctx.Err() != nil {
		return
	}
	textFile := path + ".dur"
	if err := os.WriteFile(textFile, []byte(l.Text()), 0o644); err != nil {
		return
	}
	defer os.Remove(textFile)
	tmp := path + ".lbl"
	defer os.Remove(tmp)
	if _, _, err := runCapture(ctx, ffmpeg, PosterLabelArgs(path, tmp, l.FontFile, textFile, frameH)...); err != nil {
		return
	}
	if ctx.Err() != nil {
		return
	}
	if st, err := os.Stat(tmp); err != nil || st.Size() == 0 {
		return
	}
	_ = os.Rename(tmp, path)
}
