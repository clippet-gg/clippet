//go:build ffmpeg

package transcode

// The real thing (§16.2, ROADMAP #5): the chosen frame really IS the chosen
// frame. The assertion a unit test cannot make is a colour one — the fixture
// is a different, known colour every second, so the picture in the JPEG says
// which second it came from and nothing else could.

import (
	"context"
	"errors"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

// colourPerSecondClip is five seconds of flat colour, one second each:
// red, green, blue, white, black. A frame taken at t=1.5 s must be GREEN
// and at t=2.5 s BLUE, which is a fact no rounding, no keyframe and no
// fallback ladder can fake.
func colourPerSecondClip(t *testing.T, ffmpeg, dir string) string {
	t.Helper()
	return gen(t, ffmpeg, dir, "colours.mp4", join(
		[]string{
			"-f", "lavfi", "-i", "color=c=red:s=320x180:r=30:d=1",
			"-f", "lavfi", "-i", "color=c=lime:s=320x180:r=30:d=1",
			"-f", "lavfi", "-i", "color=c=blue:s=320x180:r=30:d=1",
			"-f", "lavfi", "-i", "color=c=white:s=320x180:r=30:d=1",
			"-f", "lavfi", "-i", "color=c=black:s=320x180:r=30:d=1",
			"-filter_complex", "[0:v][1:v][2:v][3:v][4:v]concat=n=5:v=1:a=0[v]", "-map", "[v]",
		}, v,
	)...)
}

// meanRGB is the average colour of a JPEG, on the 0-255 scale.
func meanRGB(t *testing.T, path string) (r, g, b float64) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	img, err := jpeg.Decode(f)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	bounds := img.Bounds()
	var sr, sg, sb float64
	n := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y += 4 {
		for x := bounds.Min.X; x < bounds.Max.X; x += 4 {
			cr, cg, cb, _ := img.At(x, y).RGBA()
			sr += float64(cr) / 257
			sg += float64(cg) / 257
			sb += float64(cb) / 257
			n++
		}
	}
	if n == 0 {
		t.Fatalf("%s decoded to an empty picture", path)
	}
	return sr / float64(n), sg / float64(n), sb / float64(n)
}

// TestPosterAtTakesTheFrameThatWasAskedFor is the whole feature in one
// assertion: three different seconds, three different colours, in the order
// the fixture puts them.
func TestPosterAtTakesTheFrameThatWasAskedFor(t *testing.T) {
	ffmpeg, _ := binaries(t)
	dir := t.TempDir()
	in := colourPerSecondClip(t, ffmpeg, dir)

	cases := []struct {
		seconds  float64
		name     string
		dominant int // 0=red, 1=green, 2=blue
	}{
		{0.5, "red", 0},
		{1.5, "green", 1},
		{2.5, "blue", 2},
	}
	for _, c := range cases {
		out := filepath.Join(dir, c.name+".jpg")
		res, err := PosterAt(context.Background(), ffmpeg, in, out, c.seconds, PosterLabel{})
		if err != nil {
			t.Fatalf("PosterAt(%.1fs): %v", c.seconds, err)
		}
		if res.Seconds != c.seconds {
			t.Errorf("PosterAt(%.1fs) reports %.3fs; a chosen frame has no ladder to move it", c.seconds, res.Seconds)
		}
		if res.Width != 320 || res.Height != 180 {
			t.Errorf("PosterAt(%.1fs) measured %dx%d, want the clip's 320x180", c.seconds, res.Width, res.Height)
		}
		r, g, b := meanRGB(t, out)
		channels := []float64{r, g, b}
		want := channels[c.dominant]
		for i, got := range channels {
			if i == c.dominant {
				continue
			}
			if got+40 > want {
				t.Fatalf("the frame at %.1fs is not %s (r=%.0f g=%.0f b=%.0f): a chosen frame must be THAT frame",
					c.seconds, c.name, r, g, b)
			}
		}
	}
	// The last second is black, and a chosen black frame is still published:
	// the owner was looking at it. The result says so rather than refusing.
	out := filepath.Join(dir, "black.jpg")
	res, err := PosterAt(context.Background(), ffmpeg, in, out, 4.5, PosterLabel{})
	if err != nil {
		t.Fatalf("PosterAt(4.5s): %v", err)
	}
	if !res.NearBlack {
		t.Errorf("the frame at 4.5s is the fixture's black second; NearBlack must say so")
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("a near-black CHOSEN frame must still be written: %v", err)
	}
}

// A seek past the last picture is the CLIENT's failure, with a sentence of
// its own and no ffmpeg text in it: the owner reads that sentence, and
// erratum 78's doctrine is that a request and a file that disagree are never
// an error of this server's.
func TestPosterAtPastTheEndIsTheRequestsFault(t *testing.T) {
	ffmpeg, _ := binaries(t)
	dir := t.TempDir()
	in := colourPerSecondClip(t, ffmpeg, dir)

	out := filepath.Join(dir, "past.jpg")
	_, err := PosterAt(context.Background(), ffmpeg, in, out, 30, PosterLabel{})
	if err == nil {
		t.Fatal("PosterAt 25 s past the end succeeded; there is no frame there")
	}
	if !errors.Is(err, ErrNoFrameAt) {
		t.Errorf("PosterAt past the end = %v, want ErrNoFrameAt", err)
	}
	if !IsPermanent(err) {
		t.Errorf("PosterAt past the end = %v, want a permanent error: no retry puts a frame there", err)
	}
	if _, serr := os.Stat(out); serr == nil {
		t.Error("a failed extraction left a file where the poster goes")
	}
	if _, serr := os.Stat(out + ".cand"); serr == nil {
		t.Error("a failed extraction left its candidate behind in scratch")
	}
}

// A second choice replaces the first, in place, with no candidate left over.
func TestPosterAtOverwritesThePreviousChoice(t *testing.T) {
	ffmpeg, _ := binaries(t)
	dir := t.TempDir()
	in := colourPerSecondClip(t, ffmpeg, dir)
	out := filepath.Join(dir, "poster.jpg")

	if _, err := PosterAt(context.Background(), ffmpeg, in, out, 0.5, PosterLabel{}); err != nil {
		t.Fatalf("first PosterAt: %v", err)
	}
	if r, _, _ := meanRGB(t, out); r < 100 {
		t.Fatalf("the first frame is not the red second (r=%.0f)", r)
	}
	if _, err := PosterAt(context.Background(), ffmpeg, in, out, 2.5, PosterLabel{}); err != nil {
		t.Fatalf("second PosterAt: %v", err)
	}
	if _, _, b := meanRGB(t, out); b < 100 {
		t.Fatalf("the second choice did not replace the first (b=%.0f)", b)
	}
	if _, err := os.Stat(out + ".cand"); err == nil {
		t.Error("the candidate file was left behind")
	}
}
