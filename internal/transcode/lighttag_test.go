package transcode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestQualityShort: the compact quality name the tag and the card description
// both state. It is the SHORT side of the picture, so a vertical capture reads
// the same as the landscape clip of the same footage, and a fractional rate
// stays fractional.
func TestQualityShort(t *testing.T) {
	for _, tc := range []struct {
		name string
		w, h int
		fps  Rational
		want string
	}{
		{name: "the card copy", w: 1280, h: 720, fps: Rational{30, 1}, want: "720p30"},
		{name: "the capture this product is for", w: 2560, h: 1440, fps: Rational{60, 1}, want: "1440p60"},
		{name: "ntsc rate stays fractional", w: 1920, h: 1080, fps: Rational{30000, 1001}, want: "1080p29.97"},
		{name: "film rate", w: 3840, h: 2160, fps: Rational{24000, 1001}, want: "2160p23.98"},
		{name: "vertical reads by its short side", w: 1080, h: 1920, fps: Rational{30, 1}, want: "1080p30"},
		{name: "pal", w: 720, h: 576, fps: Rational{25, 1}, want: "576p25"},
		{name: "no rate at all", w: 1280, h: 720, fps: Rational{}, want: "720p"},
		{name: "no picture at all", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := QualityShort(tc.w, tc.h, tc.fps); got != tc.want {
				t.Fatalf("QualityShort(%d, %d, %s) = %q, want %q", tc.w, tc.h, tc.fps.String(), got, tc.want)
			}
		})
	}
}

// TestExpandLightTag: the template, and what happens when a quality is unknown.
// A gap must never render as a dangling separator — the line is burned into the
// picture, so there is no fixing it afterwards.
func TestExpandLightTag(t *testing.T) {
	const shipped = "{light} preview · {best} on Clippet"
	for _, tc := range []struct {
		name, tmpl, light, best, want string
	}{
		{name: "the shipped template", tmpl: shipped, light: "720p30", best: "1440p60",
			want: "720p30 preview · 1440p60 on Clippet"},
		// A template that ASKS for a quality nobody knows renders nothing: the line
		// is burned into the frame, so a hole in it is permanent.
		{name: "no best quality known", tmpl: shipped, light: "720p30", want: ""},
		{name: "no light quality known", tmpl: shipped, best: "1440p60", want: ""},
		{name: "neither known", tmpl: shipped, want: ""},
		{name: "a template that names only the light quality", tmpl: "{light} preview", light: "720p30", want: "720p30 preview"},
		{name: "…and does not need the other one", tmpl: "{light} preview", light: "720p30", best: "", want: "720p30 preview"},
		{name: "an operator's own words", tmpl: "preview only — full quality at the link", want: "preview only — full quality at the link"},
		{name: "the other order", tmpl: "{best} original, {light} here", light: "720p30", best: "1440p60",
			want: "1440p60 original, 720p30 here"},
		{name: "double spaces collapse", tmpl: "{light}  preview", light: "720p30", want: "720p30 preview"},
		{name: "empty template", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ExpandLightTag(tc.tmpl, tc.light, tc.best)
			if got != tc.want {
				t.Fatalf("ExpandLightTag(%q, %q, %q) = %q, want %q", tc.tmpl, tc.light, tc.best, got, tc.want)
			}
			if strings.Contains(got, "  ") {
				t.Errorf("the expanded line has a double space: %q", got)
			}
		})
	}
}

// TestWriteLightTagFile: the text reaches ffmpeg through a FILE, never through
// the argv, so nothing an operator can type has to survive two levels of
// filtergraph escaping.
func TestWriteLightTagFile(t *testing.T) {
	dir := t.TempDir()
	hostile := `a:b,c[d];e'f\g %{pts}`
	path, err := WriteLightTagFile(dir, "tag.txt", hostile)
	if err != nil {
		t.Fatalf("WriteLightTagFile: %v", err)
	}
	if path != filepath.Join(dir, "tag.txt") {
		t.Fatalf("path = %q", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != hostile {
		t.Fatalf("the file holds %q, want the line verbatim", b)
	}
	// No tag: no file, no error.
	empty, err := WriteLightTagFile(dir, "none.txt", "   ")
	if err != nil || empty != "" {
		t.Fatalf("a blank line wrote %q, %v; want no file at all", empty, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "none.txt")); !os.IsNotExist(err) {
		t.Error("a blank line still created the file")
	}
}

// TestDrawtextFilter: the geometry, the opacity, and the two facts that matter
// most — a zero LightTag renders NOTHING (so every argv in this package that is
// not the Light copy's is byte-for-byte what it always was) and the size scales
// with the frame.
func TestDrawtextFilter(t *testing.T) {
	if got := drawtextFilter(LightTag{}, 720); got != "" {
		t.Fatalf("a zero tag rendered %q, want nothing", got)
	}
	if got := drawtextFilter(LightTag{FontFile: "/f.ttf"}, 720); got != "" {
		t.Fatalf("a tag with no text file rendered %q, want nothing", got)
	}
	tag := LightTag{FontFile: "/usr/share/fonts/dejavu/DejaVuSans.ttf", TextFile: "/scratch/x/tag.txt"}
	got := drawtextFilter(tag, 720)
	for _, want := range []string{
		",drawtext=", "expansion=none",
		// 720 · 0.022 = 15.84 → 16; 720 · 0.015 = 10.8 → 11.
		"fontsize=16", "x=w-tw-11", "y=h-th-11",
		"fontcolor=white@0.55", "shadowcolor=black@0.45", "shadowx=1:shadowy=1",
		// The paths are QUOTED for the graph level (escapeFilterValue; a Linux
		// path has nothing for the option level to escape).
		"fontfile='/usr/share/fonts/dejavu/DejaVuSans.ttf'", "textfile='/scratch/x/tag.txt'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the drawtext filter lacks %q:\n%s", want, got)
		}
	}
	// A 4K Light copy cannot happen (the tier is ≤720p) but a narrowed
	// TRANSCODE_MAX_HEIGHT can make a tiny one, and the floor holds.
	if small := drawtextFilter(tag, 240); !strings.Contains(small, "fontsize=12") {
		t.Errorf("a 240-line frame got %q, want the %d px floor", small, LightTagMinSize)
	}
	if big := drawtextFilter(tag, 1440); !strings.Contains(big, "fontsize=32") {
		t.Errorf("a 1440-line frame got %q, want 32 px", big)
	}
}

// TestEscapeFilterValue: the value is escaped for BOTH levels of the graph parser
// (§20 erratum 312) — `\`, `'` and `:` backslash-escaped for the option level,
// then single-quoted for the graph level with a `'` written as close, escaped
// quote, reopen. Every rendering below with a path in it was measured to open
// its file on ffmpeg 7.1.5 / 7.1.1 (Windows) and 7.1.1 (Linux).
func TestEscapeFilterValue(t *testing.T) {
	for in, want := range map[string]string{
		// Windows: the drive letter's colon is escaped INSIDE the quotes, the
		// backslashes doubled. The old `'C:\\Tools\\…'` failed every Light copy.
		`C:\Tools\x\fonts\DejaVuSans.ttf`: `'C\:\\Tools\\x\\fonts\\DejaVuSans.ttf'`,
		`C:/Windows/Fonts/x.ttf`:          `'C\:/Windows/Fonts/x.ttf'`,
		// Linux: nothing to escape.
		`/usr/share/fonts/x.ttf`: `'/usr/share/fonts/x.ttf'`,
		// A quote and a space: the quote closes, is escaped twice, and reopens.
		`/tmp/it's a tag.txt`: `'/tmp/it\'\''s a tag.txt'`,
		`a,b`:                 `'a,b'`,
		`a;b`:                 `'a;b'`,
		`a[b]`:                `'a[b]'`,
		`a\b`:                 `'a\\b'`,
		`a:b`:                 `'a\:b'`,
		``:                    `''`,
	} {
		if got := escapeFilterValue(in); got != want {
			t.Errorf("escapeFilterValue(%q) = %q, want %q", in, got, want)
		}
	}
}
