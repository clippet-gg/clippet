package transcode

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The poster's burned-in duration (§20 erratum 287), in argv and arithmetic.
// posterlabel_ffmpeg_test.go is where the pixels are proven.

// TestFormatDurationMatchesTheGolden: the THIRD renderer of m:ss in this
// repository, pinned against the same one table as internal/web's and
// internal/notify's. A number burned into a picture that disagreed with the one
// under the player would be a permanent defect in the card.
func TestFormatDurationMatchesTheGolden(t *testing.T) {
	for _, tc := range durationGolden(t) {
		if got := FormatDuration(tc.MS); got != tc.Text {
			t.Errorf("FormatDuration(%d) = %q, want %q (testdata/duration.tsv)", tc.MS, got, tc.Text)
		}
	}
}

// durationGolden reads ../../testdata/duration.tsv into (ms, text) pairs — the
// same reader internal/web and internal/notify have, for the same table.
func durationGolden(t *testing.T) []struct {
	MS   int64
	Text string
} {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "duration.tsv"))
	if err != nil {
		t.Fatalf("duration.tsv: %v", err)
	}
	var out []struct {
		MS   int64
		Text string
	}
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for i := 0; sc.Scan(); i++ {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 2 {
			t.Fatalf("duration.tsv:%d: %d tab-separated fields, want 2 (%q)", i+1, len(f), line)
		}
		ms, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			t.Fatalf("duration.tsv:%d: %v", i+1, err)
		}
		out = append(out, struct {
			MS   int64
			Text string
		}{ms, f[1]})
	}
	if len(out) == 0 {
		t.Fatal("duration.tsv held no rows: the golden is the whole assertion")
	}
	return out
}

// TestPosterLabelValidity: a label draws only when there is a font AND a real
// measurement. A zero or negative duration is "not measured yet" and must not
// become a card that claims the clip is zero seconds long.
func TestPosterLabelValidity(t *testing.T) {
	for _, tc := range []struct {
		l    PosterLabel
		want bool
	}{
		{PosterLabel{}, false},
		{PosterLabel{FontFile: "/f.ttf"}, false},
		{PosterLabel{DurationMS: 6000}, false},
		{PosterLabel{FontFile: "/f.ttf", DurationMS: 0}, false},
		{PosterLabel{FontFile: "/f.ttf", DurationMS: -1}, false},
		{PosterLabel{FontFile: "/f.ttf", DurationMS: 1}, true},
	} {
		if got := tc.l.Valid(); got != tc.want {
			t.Errorf("PosterLabel%+v.Valid() = %v, want %v", tc.l, got, tc.want)
		}
	}
	if got := (PosterLabel{FontFile: "/f.ttf", DurationMS: 65400}).Text(); got != "1:05" {
		t.Errorf("Text() = %q, want 1:05", got)
	}
}

// TestPosterLabelMetricsScaleWithTheFrame: 4.5 % of the height, a floor for a
// narrowed TRANSCODE_MAX_HEIGHT, and a margin that is 12 px at 720p.
func TestPosterLabelMetricsScaleWithTheFrame(t *testing.T) {
	for _, tc := range []struct{ h, size, pad, margin int }{
		{720, 32, 10, 12},
		{1080, 49, 16, 18},
		{1920, 86, 28, 32}, // a vertical poster: the SHORT side is the width, so the type is still sized by height
		{360, 16, 5, 6},
		{240, 14, 4, 4}, // the size floor
		{120, 14, 4, 4}, // …and the margin floor under it
		{64, 14, 4, 4},  // a frame smaller than the label itself still renders one
	} {
		size, pad, margin := posterLabelMetrics(tc.h)
		if size != tc.size || pad != tc.pad || margin != tc.margin {
			t.Errorf("posterLabelMetrics(%d) = %d/%d/%d, want %d/%d/%d",
				tc.h, size, pad, margin, tc.size, tc.pad, tc.margin)
		}
	}
}

// TestPosterLabelFilterPinsTheCorner: the box's own inset is the margin, which
// means the offsets carry the padding too — drawtext grows its box outward from
// the text anchor, so an `x=w-tw-margin` would hang the plate over the edge.
func TestPosterLabelFilterPinsTheCorner(t *testing.T) {
	f := posterLabelFilter("/fonts/x.ttf", "/scratch/p.jpg.dur", 720)
	for _, want := range []string{
		"drawtext=fontfile='/fonts/x.ttf'",
		":textfile='/scratch/p.jpg.dur'",
		":expansion=none",
		":fontsize=32",
		":fontcolor=" + PosterLabelTextColor,
		":box=1:boxcolor=" + PosterLabelBoxColor,
		":boxborderw=10",
		":x=w-tw-22", // margin 12 + padding 10
		":y=22",
	} {
		if !strings.Contains(f, want) {
			t.Errorf("the filter lacks %q:\n%s", want, f)
		}
	}
	// TOP-right, never bottom: the LIGHT tag owns the bottom-right corner and a
	// <video>'s native controls own the bottom band.
	if strings.Contains(f, "y=h-") {
		t.Errorf("the label is drawn against the bottom edge: %s", f)
	}
}

// TestPosterLabelArgsAreOneFrameThroughTheWhitelist.
func TestPosterLabelArgsAreOneFrameThroughTheWhitelist(t *testing.T) {
	args := PosterLabelArgs("in.jpg", "out.jpg", "/f.ttf", "/t.txt", 720)
	joined := strings.Join(args, " ")
	for _, want := range []string{"-protocol_whitelist", "-i in.jpg", "-frames:v 1", "-update 1", "-f image2 out.jpg", "-q:v 2"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv lacks %q: %s", want, joined)
		}
	}
	// No scaling anywhere: the measured og:image dimensions the caller already
	// holds must stay true after the label pass.
	if strings.Contains(joined, "scale=") {
		t.Errorf("the label pass scales the picture: %s", joined)
	}
}

// TestPosterFontIsThisProjectsOwnFirst: the shipped face wins, and the LIGHT
// tag's fonts stand behind it for an operator who built their own image.
func TestPosterFontIsThisProjectsOwnFirst(t *testing.T) {
	installed := posterFontList("")
	tags := tagFontPaths()
	if len(installed) != len(tags)+1 {
		t.Fatalf("posterFontList(empty) = %v, want this project's own face plus the LIGHT tag's installed list", installed)
	}
	if !strings.Contains(installed[0], "spline-sans-mono") {
		t.Errorf("the first candidate is %q, want this project's own mono face", installed[0])
	}
	// Building one list from the other must not ALIAS it: a shared backing array
	// would let one list's growth rewrite the other's, which is why tagFontPaths
	// is a function returning a fresh slice.
	if &installed[1] == &tags[0] {
		t.Error("posterFontList shares its backing array with tagFontPaths")
	}
	if got := FindPosterFont(); got != "" {
		if st, err := os.Stat(got); err != nil || !st.Mode().IsRegular() {
			t.Errorf("FindPosterFont() = %q, which is not a readable file: %v", got, err)
		}
	}
}

// TestFontsBesideTheBinaryAreTheLastCandidates: a font planted in `fonts` next
// to the executable is found when every installed path is absent, and never
// before one that is present (§20 erratum 308).
//
// This is the whole of what a Windows operator running clippet.exe by hand has
// to go on: /usr/share/fonts is not a place on their machine, so without this
// entry every Light tag and every poster label on that instance is silently
// off. The container cannot run that binary, but a candidate list is a list
// either way - which is why the two builders take the directory as an argument.
func TestFontsBesideTheBinaryAreTheLastCandidates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fonts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tag := filepath.Join(dir, "DejaVuSans.ttf")
	mono := filepath.Join(dir, "spline-sans-mono.ttf")
	for _, f := range []string{tag, mono} {
		if err := os.WriteFile(f, []byte("not really a font"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	installed := tagFontPaths()

	// (a) Position. The installed paths come first, unchanged and in order, and
	// the entries beside the binary are the LAST ones: an image that installed a
	// font is never overruled by a file dropped next to the binary.
	tagList := tagFontList(dir)
	if len(tagList) != len(installed)+1 || tagList[len(tagList)-1] != tag {
		t.Fatalf("tagFontList(%q) = %v, want the installed list then %q", dir, tagList, tag)
	}
	posterList := posterFontList(dir)
	if n := len(posterList); n != len(installed)+3 || posterList[n-2] != mono || posterList[n-1] != tag {
		t.Fatalf("posterFontList(%q) = %v, want the installed list then %q then %q", dir, posterList, mono, tag)
	}
	if !strings.Contains(posterList[0], "spline-sans-mono") {
		t.Errorf("posterFontList[0] = %q, want the installed mono face", posterList[0])
	}
	for i, want := range installed {
		if tagList[i] != want || posterList[i+1] != want {
			t.Fatalf("the installed paths moved: %v / %v", tagList, posterList)
		}
	}

	// (b) An unresolvable executable simply omits the entries - no candidate, no
	// tag, never an error.
	if got := tagFontList(""); len(got) != len(installed) {
		t.Errorf("tagFontList(empty) = %v, want the installed list alone", got)
	}
	if got := posterFontList(""); len(got) != len(installed)+1 {
		t.Errorf("posterFontList(empty) = %v, want the installed list and the own face alone", got)
	}

	// (c) findFont reaches them. The installed paths are re-rooted under an EMPTY
	// directory so the answer is the same on a host that happens to have DejaVu
	// installed as on one that does not: "when every POSIX path is absent" has to
	// be a fact of the test, not of the machine.
	absent := t.TempDir()
	probeOf := func(list []string) []string {
		out := make([]string, len(list))
		for i, p := range list {
			if strings.HasPrefix(p, dir) {
				out[i] = p
				continue
			}
			out[i] = filepath.Join(absent, p)
		}
		return out
	}
	for _, tc := range []struct {
		name string
		list []string
		want string
	}{
		{"the tag falls through to the binary's own folder", tagList, tag},
		{"the poster label prefers this project's face there", posterList, mono},
	} {
		if got := findFont(probeOf(tc.list)); got != tc.want {
			t.Errorf("%s: findFont(%v) = %q, want %q", tc.name, probeOf(tc.list), got, tc.want)
		}
	}

	// (d) A present installed font still wins.
	real := filepath.Join(absent, installed[0])
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("not really a font"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := findFont(probeOf(tagList)); got != real {
		t.Errorf("findFont(%v) = %q, want the installed font %q to win", probeOf(tagList), got, real)
	}
}

// TestExecutableFontsDirIsResolvedOnce: the `fonts` directory beside the binary
// is derived from os.Executable and ends in that one name, and the two
// candidate lists this process actually uses are built from that one reading
// rather than from a fresh syscall per encode (§20 erratum 308).
func TestExecutableFontsDirIsResolvedOnce(t *testing.T) {
	if exeFontsDir == "" {
		t.Skip("os.Executable() could not resolve this test binary")
	}
	if filepath.Base(exeFontsDir) != "fonts" {
		t.Errorf("exeFontsDir = %q, want a directory called fonts", exeFontsDir)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(filepath.Dir(exe), "fonts"); exeFontsDir != want {
		t.Errorf("exeFontsDir = %q, want %q", exeFontsDir, want)
	}
	if got, want := tagFontCandidates, tagFontList(exeFontsDir); len(got) != len(want) || got[len(got)-1] != want[len(want)-1] {
		t.Errorf("tagFontCandidates = %v, want %v", got, want)
	}
	if got, want := posterFontCandidates, posterFontList(exeFontsDir); len(got) != len(want) || got[len(got)-1] != want[len(want)-1] {
		t.Errorf("posterFontCandidates = %v, want %v", got, want)
	}
}

// TestDrawPosterLabelNeverFailsThePoster: every way this can go wrong leaves the
// picture exactly as it was. A clip must not lose its card image over a number
// in a corner.
func TestDrawPosterLabelNeverFailsThePoster(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "poster.jpg")
	const original = "not-really-a-jpeg"
	for _, tc := range []struct {
		name  string
		label PosterLabel
		frame int
	}{
		{"no font", PosterLabel{DurationMS: 6000}, 720},
		{"no duration", PosterLabel{FontFile: "/f.ttf"}, 720},
		{"no frame height", PosterLabel{FontFile: "/f.ttf", DurationMS: 6000}, 0},
		{"an ffmpeg that is not there", PosterLabel{FontFile: "/f.ttf", DurationMS: 6000}, 720},
	} {
		if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
			t.Fatal(err)
		}
		drawPosterLabel(t.Context(), "ffmpeg-that-is-never-run", path, tc.label, tc.frame)
		got, err := os.ReadFile(path)
		if err != nil || string(got) != original {
			t.Errorf("%s: the poster was disturbed (%q, %v)", tc.name, got, err)
		}
		// …and nothing is left beside it.
		for _, litter := range []string{path + ".dur", path + ".lbl"} {
			if _, err := os.Stat(litter); err == nil {
				t.Errorf("%s: %s survived the pass", tc.name, filepath.Base(litter))
			}
		}
	}
}
