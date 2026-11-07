//go:build ffmpeg

package transcode

// The poster's burned-in duration on real ffmpeg (§20 erratum 287).
//
// The plain tests pin the argv and the arithmetic. What they cannot say is
// whether ffmpeg draws anything at all, whether it draws it in the corner asked
// for, whether the plate is dark enough to carry #ECE9E2 over a WHITE frame, and
// whether the picture comes out the size the card tags already claim. Those four
// are what this file is for, and it asks them of the shapes §16.4's test-clip
// matrix names — a landscape clip, a vertical one, and a tiny frame.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// posterFont resolves the face for these tests. In the image the Dockerfile
// installs it at /usr/local/share/clippet/fonts; in a plain `go test` the
// repository's own copy is two directories up, which is the SAME file.
func posterFont(t *testing.T, ffmpeg string) string {
	t.Helper()
	if !strings.Contains(run(t, ffmpeg, "-hide_banner", "-filters"), "drawtext") {
		t.Skip("this ffmpeg has no drawtext filter; the label is off on such a build")
	}
	if f := FindPosterFont(); f != "" {
		return f
	}
	repo := filepath.Join("..", "..", "web", "static", "fonts", "spline-sans-mono.ttf")
	if st, err := os.Stat(repo); err == nil && st.Mode().IsRegular() {
		return repo
	}
	t.Skip("no font for the poster label in this image or this tree")
	return ""
}

// regionStat is one signalstats number for one region of one frame, as ffmpeg
// reports it. cornerLuma (lighttag_ffmpeg_test.go) reads YAVG, which is the
// right measure for a semi-transparent LINE spread across a corner; a poster
// label is a small opaque PLATE, so the measure that says whether it is there is
// the region's MINIMUM luma — a number that does not dilute as the plate shrinks
// with the frame.
func regionStat(t *testing.T, ffmpeg, file, crop, key string) float64 {
	t.Helper()
	out := run(t, ffmpeg, "-hide_banner", "-nostdin", "-y", "-i", file,
		"-vf", "crop="+crop+",signalstats,metadata=print:key=lavfi.signalstats."+key,
		"-frames:v", "1", "-f", "null", "-")
	for _, line := range strings.Split(out, "\n") {
		if _, v, ok := strings.Cut(line, "lavfi.signalstats."+key+"="); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				t.Fatalf("%s %q: %v", key, v, err)
			}
			return f
		}
	}
	t.Fatalf("no %s in signalstats output for %s crop=%s:\n%s", key, file, crop, out)
	return 0
}

// TestPosterLabelIsBurnedIntoTheTopRight: one poster taken twice out of the same
// clip, once with a label and once without, and the DIFFERENCE measured per
// corner. Nothing else proves the plate is there, in the corner meant, and
// nowhere else.
func TestPosterLabelIsBurnedIntoTheTopRight(t *testing.T) {
	ffmpeg, _ := binaries(t)
	font := posterFont(t, ffmpeg)
	dir := t.TempDir()
	// A WHITE clip: the frame the label has to survive, and the one the bare
	// embed-overlay colour could not (1.21:1 before its shadow). If the plate
	// reads here it reads anywhere.
	in := gen(t, ffmpeg, dir, "white.mp4", join(
		[]string{"-f", "lavfi", "-i", "color=c=white:s=640x360:r=30:d=6"}, v,
	)...)

	plain := filepath.Join(dir, "plain.jpg")
	bare, err := Poster(context.Background(), ffmpeg, in, plain, 6_000_000, true, 0, PosterBox{}, PosterLabel{})
	if err != nil {
		t.Fatalf("the unlabelled poster: %v", err)
	}
	labelled := filepath.Join(dir, "labelled.jpg")
	res, err := Poster(context.Background(), ffmpeg, in, labelled, 6_000_000, true, 0, PosterBox{},
		PosterLabel{FontFile: font, DurationMS: 6000})
	if err != nil {
		t.Fatalf("the labelled poster: %v", err)
	}

	// (1) The dimensions the card tags already claim are untouched.
	if res.Width != bare.Width || res.Height != bare.Height {
		t.Errorf("the label pass resized the poster: %dx%d, want %dx%d", res.Width, res.Height, bare.Width, bare.Height)
	}
	if res.Width != 640 || res.Height != 360 {
		t.Errorf("poster %dx%d, want the clip's 640x360", res.Width, res.Height)
	}

	// (2) The top-right corner grew a DARK plate and no other corner moved at all.
	// The plate is the dark palette's ground at 72 %; composited over white that is
	// about #595957, i.e. a luma near 89 where the frame is 255 — which is also the
	// contrast the design asks for (#ECE9E2 on that ground reads 5.8:1).
	const q = "160:120:"
	corners := map[string]string{
		"top-right":    q + "480:0",
		"top-left":     q + "0:0",
		"bottom-right": q + "480:240",
		"bottom-left":  q + "0:240",
	}
	for name, crop := range corners {
		before := regionStat(t, ffmpeg, plain, crop, "YMIN")
		after := regionStat(t, ffmpeg, labelled, crop, "YMIN")
		t.Logf("%-13s darkest pixel %.0f → %.0f", name, before, after)
		if name == "top-right" {
			// (3) …and the plate really is DARK rather than a tint: nearer the
			// dark ground than to the picture it sits on.
			if after > 120 {
				t.Errorf("the top-right corner's darkest pixel is %.0f (was %.0f): no dark plate was drawn on a white frame", after, before)
			}
			continue
		}
		if before != after {
			t.Errorf("the %s corner's darkest pixel moved %.0f → %.0f: the label is not confined to the top-right", name, before, after)
		}
		if b, a := regionStat(t, ffmpeg, plain, crop, "YAVG"), regionStat(t, ffmpeg, labelled, crop, "YAVG"); b-a > 1 || a-b > 1 {
			t.Errorf("the %s corner's mean moved %.2f → %.2f", name, b, a)
		}
	}
}

// TestPosterLabelOnTheMatrixShapes: the vertical capture and the small frame.
// A vertical poster must carry the label in ITS top-right — the same corner of a
// different rectangle — and a frame narrowed by TRANSCODE_MAX_HEIGHT must still
// come out with a readable one rather than with texture.
func TestPosterLabelOnTheMatrixShapes(t *testing.T) {
	ffmpeg, _ := binaries(t)
	font := posterFont(t, ffmpeg)
	for _, tc := range []struct {
		name, size string
		w, h       int
		// crop is a quarter-width, third-height box in the frame's own top-right.
		crop string
	}{
		{"vertical", "360x640", 360, 640, "90:213:270:0"},
		{"small", "160x120", 160, 120, "40:40:120:0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			in := gen(t, ffmpeg, dir, tc.name+".mp4", join(
				[]string{"-f", "lavfi", "-i", "color=c=white:s=" + tc.size + ":r=30:d=4"}, v,
			)...)
			plain := filepath.Join(dir, "plain.jpg")
			if _, err := Poster(context.Background(), ffmpeg, in, plain, 4_000_000, true, 0, PosterBox{}, PosterLabel{}); err != nil {
				t.Fatalf("the unlabelled poster: %v", err)
			}
			out := filepath.Join(dir, "p.jpg")
			res, err := Poster(context.Background(), ffmpeg, in, out, 4_000_000, true, 0, PosterBox{},
				PosterLabel{FontFile: font, DurationMS: 4000})
			if err != nil {
				t.Fatalf("the labelled poster: %v", err)
			}
			if res.Width != tc.w || res.Height != tc.h {
				t.Errorf("poster %dx%d, want %dx%d", res.Width, res.Height, tc.w, tc.h)
			}
			before := regionStat(t, ffmpeg, plain, tc.crop, "YMIN")
			after := regionStat(t, ffmpeg, out, tc.crop, "YMIN")
			t.Logf("%s top-right darkest pixel %.0f → %.0f", tc.name, before, after)
			if after > 120 {
				t.Errorf("%s: the top-right corner's darkest pixel is %.0f (was %.0f): no label was drawn", tc.name, after, before)
			}
		})
	}
}

// TestPosterNearBlackIsMeasuredBeforeTheLabel: the ORDER is load-bearing. The
// near-black rule rejects a candidate by the maximum luma of the frame it
// decoded; a bright plate burned in before that measurement would lift every
// candidate past the test and the rule would stop rejecting anything.
func TestPosterNearBlackIsMeasuredBeforeTheLabel(t *testing.T) {
	ffmpeg, _ := binaries(t)
	font := posterFont(t, ffmpeg)
	dir := t.TempDir()
	// A clip that is black from end to end: every candidate is near-black, the
	// LAST one is kept, and the result has to SAY so — with the label on it.
	in := gen(t, ffmpeg, dir, "black.mp4", join(
		[]string{"-f", "lavfi", "-i", "color=c=black:s=320x180:r=30:d=4"}, v,
	)...)
	out := filepath.Join(dir, "p.jpg")
	res, err := Poster(context.Background(), ffmpeg, in, out, 4_000_000, true, 0, PosterBox{},
		PosterLabel{FontFile: font, DurationMS: 4000})
	if err != nil {
		t.Fatalf("poster: %v", err)
	}
	if !res.NearBlack {
		t.Error("an all-black clip's poster is not reported near-black: the label was measured as picture")
	}
	// …and the label is still on it: a dark clip keeps its duration. Here the
	// plate is the DARKEST thing on the frame already, so what proves the label is
	// the bright text on it — the corner's MAXIMUM luma, which on a black clip can
	// only come from #ECE9E2.
	if got := regionStat(t, ffmpeg, out, "80:60:240:0", "YMAX"); got < 120 {
		t.Errorf("the top-right corner's brightest pixel is %.0f on a black clip: the label did not draw", got)
	}
	// Nothing left beside the poster.
	for _, litter := range []string{out + ".dur", out + ".lbl", out + ".cand"} {
		if _, err := os.Stat(litter); err == nil {
			t.Errorf("%s survived the pass", filepath.Base(litter))
		}
	}
}

// TestPosterAtCarriesTheClipsDuration: the frame an owner CHOSE gets the clip's
// own length, not the moment they chose.
func TestPosterAtCarriesTheClipsDuration(t *testing.T) {
	ffmpeg, _ := binaries(t)
	font := posterFont(t, ffmpeg)
	dir := t.TempDir()
	in := gen(t, ffmpeg, dir, "chosen.mp4", join(
		[]string{"-f", "lavfi", "-i", "color=c=white:s=640x360:r=30:d=6"}, v,
	)...)
	plain := filepath.Join(dir, "plain.jpg")
	if _, err := PosterAt(context.Background(), ffmpeg, in, plain, 2.5, PosterLabel{}); err != nil {
		t.Fatalf("the unlabelled chosen frame: %v", err)
	}
	out := filepath.Join(dir, "p.jpg")
	res, err := PosterAt(context.Background(), ffmpeg, in, out, 2.5, PosterLabel{FontFile: font, DurationMS: 6000})
	if err != nil {
		t.Fatalf("the chosen frame: %v", err)
	}
	if res.Width != 640 || res.Height != 360 {
		t.Errorf("poster %dx%d, want 640x360", res.Width, res.Height)
	}
	before := regionStat(t, ffmpeg, plain, "160:120:480:0", "YMIN")
	after := regionStat(t, ffmpeg, out, "160:120:480:0", "YMIN")
	t.Logf("chosen frame top-right darkest pixel %.0f → %.0f", before, after)
	if after > 120 {
		t.Errorf("the chosen frame carries no label: darkest pixel %.0f (was %.0f)", after, before)
	}
}
