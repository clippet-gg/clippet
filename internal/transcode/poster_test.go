package transcode

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// near reports float equality at the precision a seek is spelled with.
func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

func TestPosterSeek(t *testing.T) {
	cases := []struct {
		name     string
		durUS    int64
		has      bool
		videoEnd float64
		want     float64
	}{
		{"15% in", 6_000_000, true, 0, 0.9},
		{"a minute", 60_000_000, true, 0, 9},
		{"two seconds", 2_000_000, true, 0, 0.3},
		{"one second", 1_000_000, true, 0, 0.15},
		{"the tail guard wins", 400_000, true, 0, 0}, // 0.06 > d-0.5 → clamped to 0
		{"no duration at all", 0, true, 0, 1},
		{"duration not measured", 6_000_000, false, 0, 1},
		// The capture whose device died: 40 s of container, 2 s of picture.
		// The container's answer (6 s) is 4 s past the last frame.
		{"the video stream stops early", 40_000_000, true, 2, 0.3},
		// A video extent that is not shorter changes nothing...
		{"a longer video extent is ignored", 10_000_000, true, 10, 1.5},
		{"an unknown extent keeps the old rule", 10_000_000, true, 0, 1.5},
		// ...and an extent is enough on its own when the container says nothing.
		{"the extent alone", 0, false, 4, 0.6},
		{"a nonsense extent is ignored", 10_000_000, true, -3, 1.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PosterSeek(c.durUS, c.has, c.videoEnd); !near(got, c.want) {
				t.Errorf("PosterSeek(%d, %v, %v) = %v, want %v", c.durUS, c.has, c.videoEnd, got, c.want)
			}
		})
	}
	args := PosterArgs("in.mp4", "p.jpg", 0.9, PosterBox{})
	want := "-hide_banner -nostdin -loglevel error -y -ss 0.900 -protocol_whitelist file -i in.mp4 -map 0:v:0 -frames:v 1 -update 1 -q:v 3 -f image2 p.jpg"
	if got := strings.Join(args, " "); got != want {
		t.Fatalf("PosterArgs = %s", got)
	}
	// THE BOX (§20 erratum 219): one scale link, and nothing else about the pass
	// changes. A box is what a run passes when it reads an UNTAGGED file bigger
	// than the card wants — which is every run that also made a Light copy.
	boxed := strings.Join(PosterArgs("in.mp4", "p.jpg", 0.9, PosterBox{W: 1280, H: 720}), " ")
	wantBoxed := "-hide_banner -nostdin -loglevel error -y -ss 0.900 -protocol_whitelist file -i in.mp4 " +
		"-map 0:v:0 -vf scale=1280:720:flags=lanczos -frames:v 1 -update 1 -q:v 3 -f image2 p.jpg"
	if boxed != wantBoxed {
		t.Fatalf("PosterArgs boxed = %s", boxed)
	}
	// A half-stated box is no box: nothing is scaled to a zero.
	for _, half := range []PosterBox{{W: 1280}, {H: 720}, {W: -2, H: 720}} {
		if got := strings.Join(PosterArgs("in.mp4", "p.jpg", 0.9, half), " "); got != want {
			t.Errorf("PosterArgs(%+v) scaled: %s", half, got)
		}
	}
	// The thumbnail pass: the window is bounded in FRONT of `-i`, so a
	// two-hour rendition is demuxed for as long as the window lasts — and the
	// pass REPORTS the frame it chose on stdout, which is the only way the
	// poster's timestamp (and the GIF window that follows it) can be the
	// chosen frame's rather than the window's start (§20 erratum 92(c)).
	const pick = "thumbnail=n=100,metadata=mode=add:key=clippet.poster:value=1,metadata=mode=print:file=pipe\\\\:1"
	thumb := strings.Join(PosterThumbArgs("in.mp4", "p.jpg", 1.5, 8.5, PosterBox{}), " ")
	wantThumb := "-hide_banner -nostdin -loglevel error -y -ss 1.500 -t 8.500 -protocol_whitelist file -i in.mp4 " +
		"-map 0:v:0 -vf " + pick + " -frames:v 1 -update 1 -q:v 3 -f image2 p.jpg"
	if thumb != wantThumb {
		t.Fatalf("PosterThumbArgs = %s", thumb)
	}
	// The box's scale is the LAST link of the thumbnail chain: the frames are
	// scored at the size they were decoded at, and only the chosen one is resized.
	boxedThumb := strings.Join(PosterThumbArgs("in.mp4", "p.jpg", 1.5, 8.5, PosterBox{W: 406, H: 720}), " ")
	wantBoxedThumb := "-hide_banner -nostdin -loglevel error -y -ss 1.500 -t 8.500 -protocol_whitelist file -i in.mp4 " +
		"-map 0:v:0 -vf " + pick + ",scale=406:720:flags=lanczos -frames:v 1 -update 1 -q:v 3 -f image2 p.jpg"
	if boxedThumb != wantBoxedThumb {
		t.Fatalf("PosterThumbArgs boxed = %s", boxedThumb)
	}
	// The same selection with no picture: the pick pass a caller runs when it
	// only needs to know WHERE the poster came from.
	gotPick := strings.Join(PosterPickArgs("in.mp4", 1.5, 8.5), " ")
	wantPick := "-hide_banner -nostdin -loglevel error -y -ss 1.500 -t 8.500 -protocol_whitelist file -i in.mp4 " +
		"-map 0:v:0 -vf " + pick + " -frames:v 1 -f null -"
	if gotPick != wantPick {
		t.Fatalf("PosterPickArgs = %s", gotPick)
	}
}

// TestPosterPickedSeconds: the pass prints a time RELATIVE to the window it
// read (`-ss` in front of `-i` shifts the output timeline to zero), so the
// frame's own timestamp is the window's start plus that. Anything the parser
// cannot trust falls back to the start, which is what the poster reported for
// every frame before it could describe itself.
func TestPosterPickedSeconds(t *testing.T) {
	const line = "frame:0    pts:45056   pts_time:2.933333\nclippet.poster=1\n"
	for _, c := range []struct {
		name   string
		stdout string
		start  float64
		want   float64
	}{
		{"the chosen frame", line, 1.5, 4.433333},
		{"from zero", line, 0, 2.933333},
		{"nothing printed", "", 1.5, 1.5},
		{"only the added key", "clippet.poster=1\n", 1.5, 1.5},
		{"a later frame is ignored", line + "frame:1    pts:1 pts_time:9.5\n", 1.5, 4.433333},
		{"unparseable", "frame:0    pts:0   pts_time:later\n", 1.5, 1.5},
		{"negative", "frame:0    pts:0   pts_time:-2\n", 1.5, 1.5},
		{"not a number", "frame:0    pts:0   pts_time:NaN\n", 1.5, 1.5},
	} {
		if got := posterPickedSeconds([]byte(c.stdout), c.start); !near(got, c.want) {
			t.Errorf("%s: posterPickedSeconds = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestPosterWindowIsBounded: the thumbnail pass never reads past the picture,
// and never more than posterThumbWindow seconds of it.
func TestPosterWindowIsBounded(t *testing.T) {
	for _, c := range []struct {
		name              string
		durUS             int64
		has               bool
		videoEnd          float64
		start, wantWindow float64
	}{
		{"a short clip: to its own end", 10_000_000, true, 0, 1.5, 8.5},
		{"a long clip: the 20 s bound", 600_000_000, true, 0, 90, posterThumbWindow},
		{"the video stream's end wins", 40_000_000, true, 2, 0.3, 1.7},
		{"no measurement: the bound", 0, false, 0, posterUnknownSeek, posterThumbWindow},
	} {
		t.Run(c.name, func(t *testing.T) {
			start, window := posterPlan(c.durUS, c.has, c.videoEnd)
			if !near(start, c.start) {
				t.Errorf("start = %v, want %v", start, c.start)
			}
			if !near(window, c.wantWindow) {
				t.Errorf("window = %v, want %v", window, c.wantWindow)
			}
			if end := start + window; c.has && c.durUS > 0 && end > float64(c.durUS)/1e6+1e-9 && window != posterThumbWindow {
				t.Errorf("the window ends at %v, past the clip", end)
			}
		})
	}
}

// TestNearBlackRejection is the rule the card image turns on: a frame with no
// light in it is not a poster, a dark one that HAS light is.
func TestNearBlackRejection(t *testing.T) {
	flat := func(v uint8) image.Image {
		img := image.NewGray(image.Rect(0, 0, 64, 64))
		for i := range img.Pix {
			img.Pix[i] = v
		}
		return img
	}
	// A black frame with one lit corner: mean ≈ 0, but there IS a picture.
	speck := image.NewGray(image.Rect(0, 0, 64, 64))
	speck.SetGray(0, 0, color.Gray{Y: 255})

	for _, c := range []struct {
		name string
		img  image.Image
		want bool // near-black → rejected
	}{
		{"a wholly black frame is rejected", flat(0), true},
		{"a fade-in's first frames are rejected", flat(5), true},
		{"a dark but real frame is kept", flat(30), false},
		{"a bright frame is kept", flat(200), false},
		{"a black frame with one lit pixel is kept", speck, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := measureLuma(c.img)
			if got := f.nearBlack(); got != c.want {
				t.Errorf("nearBlack = %v, want %v (mean %.1f, max %.1f)", got, c.want, f.MeanLuma, f.MaxLuma)
			}
		})
	}
	// The measurement is the 0-255 scale signalstats prints, so a failing
	// poster can be checked by hand with the same number.
	if f := measureLuma(flat(128)); f.MeanLuma < 127 || f.MeanLuma > 129 {
		t.Errorf("mean luma of a mid-grey frame = %.2f, want ≈128", f.MeanLuma)
	}
}

// jpegFile writes a real w×h JPEG for the fake ffmpeg to "extract". luma is
// the flat grey it is filled with.
func jpegFile(t *testing.T, w, h int, luma uint8) string {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = luma
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "frame.jpg") // a fresh dir per call
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// posterScript copies a prepared JPEG to the last argv entry, but only
// when the -ss value is in okSeeks (a space-separated list); otherwise it
// exits 0 without writing anything (ffmpeg past EOF behaves the same). A
// `-vf thumbnail` argv is the FIRST candidate, so a script that answers only
// a plain seek must refuse it: thumbSeeks lists the seeks it accepts there.
func posterScript(jpg, okSeeks string) string {
	return posterScriptTwo(jpg, "", jpg, okSeeks)
}

// posterScriptTwo answers the thumbnail candidate with thumbJPG at the seeks
// in thumbSeeks, and the plain-seek candidates with seekJPG at seekSeeks.
func posterScriptTwo(thumbJPG, thumbSeeks, seekJPG, seekSeeks string) string {
	return `
seek=""; prev=""; thumb=0
for a in "$@"; do
  if [ "$prev" = "-ss" ]; then seek="$a"; fi
  case "$a" in thumbnail=*) thumb=1;; esac
  prev="$a"
done
eval out=\${$#}
if [ "$thumb" = 1 ]; then oks="` + thumbSeeks + `"; src="` + thumbJPG + `"; else oks="` + seekSeeks + `"; src="` + seekJPG + `"; fi
for ok in $oks; do if [ "$seek" = "$ok" ]; then cp "$src" "$out"; exit 0; fi; done
exit 0
`
}

func TestPoster(t *testing.T) {
	lit := jpegFile(t, 640, 360, 200)
	black := jpegFile(t, 320, 240, 0)
	out := filepath.Join(t.TempDir(), "poster.jpg")
	t.Run("the thumbnail pass is the first candidate", func(t *testing.T) {
		ff := fakeFFmpeg(t, posterScriptTwo(lit, "0.900", black, "0.900 0.000"))
		res, err := Poster(context.Background(), ff, "in.mp4", out, 6_000_000, true, 0, PosterBox{}, PosterLabel{})
		if err != nil || res.Width != 640 || res.Height != 360 || res.NearBlack {
			t.Fatalf("Poster = %+v, %v", res, err)
		}
		if !near(res.Seconds, 0.9) {
			t.Errorf("seconds = %v, want the 15%% seek", res.Seconds)
		}
	})
	t.Run("a near-black thumbnail falls through to the ladder", func(t *testing.T) {
		// The thumbnail pass finds only black; the plain seek finds a
		// picture. The lit frame wins, and its dims are the poster's.
		ff := fakeFFmpeg(t, posterScriptTwo(black, "0.900", lit, "0.900 0.000"))
		res, err := Poster(context.Background(), ff, "in.mp4", out, 6_000_000, true, 0, PosterBox{}, PosterLabel{})
		if err != nil || res.Width != 640 || res.Height != 360 || res.NearBlack {
			t.Fatalf("Poster = %+v, %v", res, err)
		}
	})
	t.Run("a wholly black clip still gets a poster", func(t *testing.T) {
		ff := fakeFFmpeg(t, posterScriptTwo(black, "0.900", black, "0.900 0.000"))
		res, err := Poster(context.Background(), ff, "in.mp4", out, 6_000_000, true, 0, PosterBox{}, PosterLabel{})
		if err != nil {
			t.Fatalf("a black clip must not fail: %v", err)
		}
		if !res.NearBlack || res.Width != 320 || res.Height != 240 {
			t.Fatalf("Poster = %+v, want the kept near-black frame", res)
		}
		if st, err := os.Stat(out); err != nil || st.Size() == 0 {
			t.Fatalf("the kept frame must be on disk: %v", err)
		}
	})
	t.Run("fallback to -ss 0", func(t *testing.T) {
		ff := fakeFFmpeg(t, posterScript(lit, "0.000"))
		res, err := Poster(context.Background(), ff, "in.mp4", out, 6_000_000, true, 0, PosterBox{}, PosterLabel{})
		if err != nil || res.Width != 640 || res.Height != 360 {
			t.Fatalf("Poster fallback = %+v, %v", res, err)
		}
		if res.Seconds != 0 {
			t.Errorf("seconds = %v, want 0", res.Seconds)
		}
	})
	t.Run("unknown duration seeks 1.0 then 0", func(t *testing.T) {
		ff := fakeFFmpeg(t, posterScript(lit, "1.000"))
		if _, err := Poster(context.Background(), ff, "in.mp4", out, 0, false, 0, PosterBox{}, PosterLabel{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the seek is clamped to the video stream's extent", func(t *testing.T) {
		// Container 40 s, picture 2 s: 6.000 is past the last frame and
		// writes nothing; 0.300 is inside the picture.
		ff := fakeFFmpeg(t, posterScript(lit, "0.300"))
		res, err := Poster(context.Background(), ff, "in.mp4", out, 40_000_000, true, 2, PosterBox{}, PosterLabel{})
		if err != nil {
			t.Fatalf("Poster = %v", err)
		}
		if !near(res.Seconds, 0.3) {
			t.Errorf("seconds = %v, want the extent-clamped seek", res.Seconds)
		}
	})
	t.Run("nothing extracted is permanent poster_failed", func(t *testing.T) {
		ff := fakeFFmpeg(t, posterScript(lit, "9.999"))
		_, err := Poster(context.Background(), ff, "in.mp4", out, 6_000_000, true, 0, PosterBox{}, PosterLabel{})
		if !IsPermanent(err) || !strings.Contains(err.Error(), "poster_failed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("non-zero exit then success", func(t *testing.T) {
		ff := fakeFFmpeg(t, `
prev=""; for a in "$@"; do if [ "$prev" = "-ss" ] && [ "$a" != "0.000" ]; then echo "seek failed" >&2; exit 1; fi; prev="$a"; done
eval out=\${$#}; cp "`+lit+`" "$out"
`)
		if _, err := Poster(context.Background(), ff, "in.mp4", out, 6_000_000, true, 0, PosterBox{}, PosterLabel{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("stale file is not trusted", func(t *testing.T) {
		if err := os.WriteFile(out, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
		ff := fakeFFmpeg(t, "exit 0\n")
		if _, err := Poster(context.Background(), ff, "in.mp4", out, 6_000_000, true, 0, PosterBox{}, PosterLabel{}); !IsPermanent(err) {
			t.Fatalf("err = %v", err)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatalf("the stale poster must be gone: %v", err)
		}
	})
	t.Run("a candidate that is not a jpeg is refused", func(t *testing.T) {
		ff := fakeFFmpeg(t, "eval out=\\${$#}; printf 'not a jpeg' > \"$out\"\n")
		_, err := Poster(context.Background(), ff, "in.mp4", out, 6_000_000, true, 0, PosterBox{}, PosterLabel{})
		if !IsPermanent(err) || !strings.Contains(err.Error(), "undecodable") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ff := fakeFFmpeg(t, "exec sleep 30\n")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := Poster(ctx, ff, "in.mp4", out, 6_000_000, true, 0, PosterBox{}, PosterLabel{}); err != context.Canceled {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no candidate file is left behind", func(t *testing.T) {
		ff := fakeFFmpeg(t, posterScript(lit, "0.900"))
		if _, err := Poster(context.Background(), ff, "in.mp4", out, 6_000_000, true, 0, PosterBox{}, PosterLabel{}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(out + ".cand"); !os.IsNotExist(err) {
			t.Fatalf("the candidate scratch file must not survive: %v", err)
		}
	})
}
