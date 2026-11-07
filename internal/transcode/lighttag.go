package transcode

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The LIGHT tag (DESIGN.md §20 erratum 214).
//
// The product's hard constraints forbid watermarks on anything the site plays
// or the user downloads, and that stands. The owner amended it on 2026-09-08 for
// exactly ONE artefact: the LIGHT copy — the file a card fetches, which is
// never what the watch page streams once a better tier exists — carries one
// small semi-transparent corner line naming the preview quality and the real
// one. His words: "watermark the LQ to not make our product look bad … clear
// yet unobtrusive". The reason is the complaint that started this run: a card
// plays a 720p30 copy of a 1440p60 capture, and a reader who does not know that
// concludes the product is a 720p product.
//
// The Original, the Full and every poster, GIF and download are untouched.

const (
	// LightTagHeightFrac is the cap height of the line as a share of the frame:
	// ~2.2 %, which is 16 px on 720p. The brief: clear yet unobtrusive.
	LightTagHeightFrac = 0.022
	// LightTagMarginFrac is the inset from the bottom-right corner, as a share
	// of the frame height.
	LightTagMarginFrac = 0.015
	// LightTagMinSize is the floor in pixels, for the small frames a narrowed
	// TRANSCODE_MAX_HEIGHT produces: below this the line is not a line, it is
	// texture.
	LightTagMinSize = 12
	// LightTagOpacity is 55 %: legible over both a bright and a dark frame
	// while never being the thing the eye lands on.
	LightTagOpacity = "0.55"
	// LightTagShadowOpacity keeps the line readable over a light frame without
	// drawing a box around it.
	LightTagShadowOpacity = "0.45"
)

// tagFontPaths are the paths a permissively licensed sans is INSTALLED at in
// the images this project ships, best first.
//
// DejaVu Sans is the font, on both images: it is under the DejaVu Fonts License
// (a Bitstream Vera derivative — free to use, redistribute and modify, with no
// attribution requirement in the rendered output), it is one `apk add
// font-dejavu` / `apt-get install fonts-dejavu-core` away on the two bases, and
// it has the numerals and the middle dot the shipped template needs. DEPLOY §27
// records the licence; nothing is vendored into the repository.
//
// Liberation Sans and Noto Sans are here for an operator who built their own
// image on a base that ships one of those instead. The list is searched, never
// configured: a `LIGHT_TAG_FONT` variable would be one more thing to get wrong
// for a cosmetic line, and an image with none of these simply has no tag.
//
// It is a FUNCTION returning a fresh slice because two candidate lists are
// built from it (the poster label's is the other) and an `append` onto a shared
// backing array would let one list's growth rewrite the other's.
func tagFontPaths() []string {
	return []string{
		"/usr/share/fonts/dejavu/DejaVuSans.ttf",          // alpine: font-dejavu
		"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", // debian/ubuntu: fonts-dejavu-core
		"/usr/share/fonts/TTF/DejaVuSans.ttf",             // arch
		"/usr/share/fonts/liberation/LiberationSans-Regular.ttf",
		"/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf",
		"/usr/share/fonts/noto/NotoSans-Regular.ttf",
	}
}

// exeFontsDir is `fonts` beside the binary — `<the directory os.Executable
// reports>/fonts` — or "" when the executable's path cannot be resolved
// (§20 erratum 308).
//
// It exists for the operator this program's POSIX paths have nothing to offer:
// somebody running `clippet.exe` on Windows, where /usr/share/fonts is not a
// place. It is a SEARCHED PATH and not a variable, in the spirit of the
// paragraph above — a `LIGHT_TAG_FONT` would be one more thing to get wrong for
// a cosmetic line — and it goes LAST, so an image that installed a font is
// never overruled by a file somebody dropped next to the binary.
//
// Resolved ONCE, at package start-up, because it cannot change while the
// process runs and because os.Executable is a syscall that should not be made
// per encode. A failure just omits the entry: no candidate, no tag, never an
// error — the same degradation an image with no font already gets.
var exeFontsDir = executableFontsDir()

func executableFontsDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "fonts")
}

// tagFontList is the LIGHT tag's candidate list for a given `fonts` directory
// beside the binary: the installed paths, then DejaVuSans.ttf there. An empty
// exeFonts (os.Executable failed) simply omits the last entry. Taking the
// directory as an ARGUMENT is what lets a test plant a font in one.
func tagFontList(exeFonts string) []string {
	c := tagFontPaths()
	if exeFonts != "" {
		c = append(c, filepath.Join(exeFonts, "DejaVuSans.ttf"))
	}
	return c
}

// tagFontCandidates is that list for THIS binary, built once.
var tagFontCandidates = tagFontList(exeFontsDir)

// FindTagFont returns the first font of tagFontCandidates that exists, or "".
// serve.go calls it once at boot, the same way it resolves the ffmpeg binaries,
// and clears LIGHT_TAG_TEXT when it comes back empty: a missing font costs a
// clip its tag and must never cost it its encode.
func FindTagFont() string { return findFont(tagFontCandidates) }

func findFont(candidates []string) string {
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// LightTag is the resolved tag of one encode: the font the image ships and the
// file this run wrote the expanded line into.
//
// The TEXT is passed through a FILE and never through the argv. `drawtext`'s
// `text=` value goes through two levels of filtergraph escaping, and the value
// is operator-supplied — a colon, a comma, a bracket, a backslash or a `%`
// would each mean something to a different level of that parser. A file has no
// levels: whatever bytes are in it are the line.
type LightTag struct {
	FontFile string
	TextFile string
}

// Valid reports whether this tag can be rendered at all.
func (t LightTag) Valid() bool { return t.FontFile != "" && t.TextFile != "" }

// QualityShort is the compact quality name a person reads on a card: the short
// side of the picture, a `p`, and the frame rate.
//
//	1280×720 @ 30/1        → "720p30"
//	2560×1440 @ 60/1       → "1440p60"
//	1920×1080 @ 30000/1001 → "1080p29.97"
//
// The SHORT side is the number, whichever way up the picture is, so a vertical
// phone capture reads "1080p30" like the landscape clip of the same footage —
// that is what "1080p" has always meant, and a viewer comparing two cards is
// comparing pictures and not orientations. A fractional rate stays fractional:
// 29.97 is not 30, and rounding it here would be the card telling a small lie
// about the very number it exists to state.
func QualityShort(w, h int, fps Rational) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	s := strconv.Itoa(min(w, h)) + "p"
	if !fps.Valid() {
		return s
	}
	return s + fpsShort(fps)
}

// fpsShort renders a rate with no unit: whole where the rate is whole, two
// decimals where it is not, trailing zeros trimmed ("29.97", "23.98", "50").
func fpsShort(fps Rational) string {
	if fps.Num%fps.Den == 0 {
		return strconv.Itoa(fps.Num / fps.Den)
	}
	v := strconv.FormatFloat(float64(fps.Num)/float64(fps.Den), 'f', 2, 64)
	v = strings.TrimRight(v, "0")
	return strings.TrimSuffix(v, ".")
}

// Tag template placeholders (§20 erratum 214). They are braces rather than
// `%s` so an operator can drop either one, use them in the other order, or
// write a line with neither.
const (
	LightTagLightPlaceholder = "{light}"
	LightTagBestPlaceholder  = "{best}"
)

// ExpandLightTag renders LIGHT_TAG_TEXT for one clip: `light` is the card copy's
// own quality and `best` the best tier's.
//
// A template that ASKS for a quality nobody knows renders NOTHING at all, and
// the clip's Light copy carries no tag. That is deliberate and it is the only
// honest option: the line is burned into the picture, so "720p30 preview · on
// Clippet" — or any other attempt to tidy the hole a dropped placeholder leaves
// in an operator's own sentence — is a permanent defect in every card. An
// operator whose template names only one quality is unaffected, because the
// placeholder they left out cannot be missing.
//
// In practice neither is ever unknown: the Light plan is what this run is about
// to encode and the best tier's numbers come from the same probe. This is the
// belt on that.
func ExpandLightTag(template, light, best string) string {
	if strings.Contains(template, LightTagLightPlaceholder) && light == "" {
		return ""
	}
	if strings.Contains(template, LightTagBestPlaceholder) && best == "" {
		return ""
	}
	s := strings.ReplaceAll(template, LightTagLightPlaceholder, light)
	s = strings.ReplaceAll(s, LightTagBestPlaceholder, best)
	// One line, one space between words: a template with a stray double space
	// would otherwise render it into the frame.
	return strings.Join(strings.Fields(s), " ")
}

// WriteLightTagFile writes the expanded line into the run's scratch directory
// and returns the path drawtext will read. An empty line writes nothing and
// returns "": there is no tag, which is what LIGHT_TAG_TEXT=off asks for.
func WriteLightTagFile(dir, name, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", nil
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return "", fmt.Errorf("light tag text: %w", err)
	}
	return path, nil
}

// drawtextFilter renders the tag as the last link of the LIGHT copy's video
// chain, sized from the frame it is drawn on.
//
// Bottom-right, because bottom-LEFT is where a phone's own recording indicator
// and most games' killfeeds live, and because a card is cropped from the top in
// some clients. `x`/`y` are drawtext's own expressions over the text metrics
// (`tw`/`th`), so the line sits against the corner whatever the font does with
// the characters.
func drawtextFilter(t LightTag, frameH int) string {
	if !t.Valid() {
		return ""
	}
	size := int(math.Round(float64(frameH) * LightTagHeightFrac))
	if size < LightTagMinSize {
		size = LightTagMinSize
	}
	margin := int(math.Round(float64(frameH) * LightTagMarginFrac))
	if margin < 2 {
		margin = 2
	}
	var b strings.Builder
	b.WriteString(",drawtext=fontfile=")
	b.WriteString(escapeFilterValue(t.FontFile))
	b.WriteString(":textfile=")
	b.WriteString(escapeFilterValue(t.TextFile))
	// expansion=none: the line is text, and `%{…}` in it is text too. Without
	// this an operator's `%` would be read as a drawtext expression and the
	// encode would fail on a line nobody could see was wrong.
	b.WriteString(":expansion=none:fontsize=")
	b.WriteString(strconv.Itoa(size))
	b.WriteString(":fontcolor=white@")
	b.WriteString(LightTagOpacity)
	b.WriteString(":shadowcolor=black@")
	b.WriteString(LightTagShadowOpacity)
	b.WriteString(":shadowx=1:shadowy=1:x=w-tw-")
	b.WriteString(strconv.Itoa(margin))
	b.WriteString(":y=h-th-")
	b.WriteString(strconv.Itoa(margin))
	return b.String()
}

// escapeFilterValue renders one filter option VALUE for the graph description,
// escaped once for EACH of the two levels ffmpeg parses a filtergraph at
// (ffmpeg-filters "Notes on filtergraph escaping"; §20 erratum 312).
//
// The GRAPH level reads a filter's whole argument string and strips one layer of
// quoting and escaping; the OPTION level then splits what is left on `:` and
// strips a second. So a value is escaped inside-out:
//
//  1. option level: `\`, `'` and `:` each get a backslash — the escaped colon is
//     what lets a Windows drive letter survive the option split;
//  2. graph level: the result is wrapped in single quotes, inside which `,`,
//     `;`, `[`, `]` and every backslash are literal; a `'` cannot sit inside
//     quotes, so it is written as three pieces: a closing `'`, an escaped
//     `\'`, and a reopening `'`.
//
// Quoting ALONE does not protect a colon. The quotes are gone by the time the
// option level splits, so the old rendering `'C:\\Tools\\…'` became the option
// `fontfile=C` and every Light copy failed on Windows ("No option name near
// '\\Tools\\…'"); the earlier claim that a colon is safe inside quotes was
// measured on a Linux path, which has none. Measured on ffmpeg 7.1.5 and 7.1.1
// (Windows) and 7.1.1 (Linux): this rendering opens a Windows path, a Linux
// path, a path with a quote and a space, one with a colon and one with a
// backslash; the old rendering and the forward-slash spellings each fail at
// least one of them.
//
// Backslashes are KEPT rather than rewritten to forward slashes: Windows takes
// either, but on Linux a backslash is a legal filename character, and rewriting
// it opens a different file (measured).
//
// Both values it is used on are paths this process chose, so this is the belt on a
// rule rather than a parser for hostile input: the OPERATOR's own text never comes
// through here, it comes through a file (LightTag.TextFile).
func escapeFilterValue(v string) string {
	opt := filterOptionEscaper.Replace(v)
	return "'" + strings.ReplaceAll(opt, `'`, `'\''`) + "'"
}

// filterOptionEscaper is the option level's escape: a backslash before each of
// the three characters that level treats as special.
var filterOptionEscaper = strings.NewReplacer(`\`, `\\`, `'`, `\'`, `:`, `\:`)
