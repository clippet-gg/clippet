//go:build ffmpeg

package transcode

// The real thing (§16.2, §20 erratum 220): the sampling pass runs against a clip
// this test generates, and the assertions are the ones a unit test cannot make —
// that ffmpeg writes a JPEG of exactly the planned geometry, that the tiles are
// DIFFERENT frames rather than the same one N times, that a vertical rendition
// gets a tall tile, and that a file with no picture in it fails as the FILE's
// fault rather than as ours.

import (
	"context"
	"errors"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

// rampClip renders a 6 s 640x360 clip whose luma rises with the TIMESTAMP: dark
// grey at 0 s, near-white at 6 s. It is the fixture that makes the sheet's
// contents measurable — a filmstrip's whole job is that tile i shows an EARLIER
// frame than tile i+1, and on a clip of constant brightness (testsrc2) no
// measurement can tell a correct sheet from the same frame twenty-four times.
//
// The ramp starts at 32 rather than 0 because x264 writes LIMITED range: a luma
// of 10 and a luma of 0 are both below the black level and come back from the
// decoder as the same 0, which made the first two tiles indistinguishable and
// the assertion below a false alarm.
func rampClip(t *testing.T, ffmpeg, dir string) string {
	t.Helper()
	return gen(t, ffmpeg, dir, "ramp.mp4", join(
		[]string{"-f", "lavfi", "-i", "color=c=black:s=640x360:d=" + dur + ":r=30",
			"-vf", "geq=lum='32+T*34':cb=128:cr=128"},
		v,
	)...)
}

// TestMakeFilmstripProducesTheSheetThePlanDescribes is the whole contract in one
// run: the geometry measured back out of the JPEG equals the plan, and the tiles
// are the clip's frames IN ORDER.
func TestMakeFilmstripProducesTheSheetThePlanDescribes(t *testing.T) {
	ffmpeg, _ := binaries(t)
	dir := t.TempDir()
	in := rampClip(t, ffmpeg, dir)
	out := filepath.Join(dir, "strip.jpg")

	p := PlanFilmstrip(6000, 640)
	res, err := MakeFilmstrip(context.Background(), ffmpeg, p, in, out)
	if err != nil {
		t.Fatalf("MakeFilmstrip: %v", err)
	}
	if res.Tiles != FilmstripTiles || res.TileW != FilmstripTileWidth {
		t.Fatalf("result = %+v, want %d tiles of %d px", res, FilmstripTiles, FilmstripTileWidth)
	}
	if res.IntervalMS != 250 {
		t.Errorf("interval = %d ms, want 250 for a 6 s clip in 24 tiles", res.IntervalMS)
	}
	// 160 px of a 640x360 source is 160x90, and the sheet is one row of them.
	if res.TileH != 90 {
		t.Errorf("tile height = %d, want 90 for a 16:9 source", res.TileH)
	}
	img := decodeSheet(t, out)
	if img.Bounds().Dx() != p.SheetWidth() || img.Bounds().Dy() != res.TileH {
		t.Fatalf("sheet is %v, want %dx%d", img.Bounds(), p.SheetWidth(), res.TileH)
	}
	if res.Bytes <= 0 || res.Bytes > FilmstripMaxBytes {
		t.Errorf("sheet is %d bytes (cap %d)", res.Bytes, FilmstripMaxBytes)
	}
	st, err := os.Stat(out)
	if err != nil || st.Size() != res.Bytes {
		t.Errorf("Bytes = %d but the file is %v (%v)", res.Bytes, st, err)
	}
	// The candidate file is never left behind: a stale one would be mistaken for
	// the next attempt's output.
	if _, err := os.Stat(out + ".cand"); !os.IsNotExist(err) {
		t.Errorf("the candidate file survived: %v", err)
	}

	// The tiles are the clip's frames IN ORDER. The fixture's luma rises with
	// time, so each tile must be brighter than the one before it: that single
	// assertion rules out a sheet of one frame repeated (a wrong `fps`
	// expression), a sheet in the wrong order, and a sheet sampled from the
	// wrong part of the clip — none of which a geometry check can see.
	luma := make([]float64, res.Tiles)
	for i := range luma {
		luma[i] = tileLuma(img, i, res)
	}
	for i := 1; i < len(luma); i++ {
		// A hair of tolerance for JPEG quantisation, nothing more: the fixture
		// gains ~10 units of luma per tile.
		if luma[i] <= luma[i-1]+1 {
			t.Fatalf("tile %d is not later than tile %d (luma %.2f vs %.2f); all tiles: %.1f",
				i, i-1, luma[i], luma[i-1], luma)
		}
	}
	if luma[len(luma)-1]-luma[0] < 100 {
		t.Errorf("the sheet spans %.1f luma over %d tiles; the fixture ramps ~230 across the clip, so it is "+
			"sampling only part of it", luma[len(luma)-1]-luma[0], res.Tiles)
	}
}

// TestMakeFilmstripVerticalSource: a 9:16 phone capture gets a TALL tile, and the
// height is measured rather than assumed — which is the whole reason the geometry
// travels with the sheet instead of being computed by the client.
func TestMakeFilmstripVerticalSource(t *testing.T) {
	ffmpeg, _ := binaries(t)
	dir := t.TempDir()
	in := gen(t, ffmpeg, dir, "vertical.mp4", join(
		[]string{"-f", "lavfi", "-i", src("360x640", "30")}, v,
	)...)
	out := filepath.Join(dir, "strip.jpg")
	p := PlanFilmstrip(6000, 360)
	res, err := MakeFilmstrip(context.Background(), ffmpeg, p, in, out)
	if err != nil {
		t.Fatalf("MakeFilmstrip: %v", err)
	}
	// 160 px wide of a 360x640 source is 160x284 (even, as scale=W:-2 forces).
	if res.TileW != 160 || res.TileH != 284 {
		t.Fatalf("tile = %dx%d, want 160x284", res.TileW, res.TileH)
	}
	img := decodeSheet(t, out)
	if img.Bounds().Dx() != res.Tiles*res.TileW || img.Bounds().Dy() != res.TileH {
		t.Fatalf("sheet is %v, want %dx%d", img.Bounds(), res.Tiles*res.TileW, res.TileH)
	}
}

// TestMakeFilmstripShortClipGetsFewerTiles: a clip too short to space 24 tiles
// out gets as many as fit, and the JPEG is exactly that wide. This is the row of
// the plan table that most easily produces a file the client cannot read.
func TestMakeFilmstripShortClipGetsFewerTiles(t *testing.T) {
	ffmpeg, _ := binaries(t)
	dir := t.TempDir()
	in := gen(t, ffmpeg, dir, "short.mp4", join(
		[]string{"-f", "lavfi", "-i", "testsrc2=duration=1:size=320x180:rate=30"}, v,
	)...)
	out := filepath.Join(dir, "strip.jpg")
	p := PlanFilmstrip(1000, 320)
	if p.Tiles != 10 {
		t.Fatalf("plan = %+v, want 10 tiles for a 1 s clip", p)
	}
	res, err := MakeFilmstrip(context.Background(), ffmpeg, p, in, out)
	if err != nil {
		t.Fatalf("MakeFilmstrip: %v", err)
	}
	if res.Tiles != 10 {
		t.Fatalf("result = %+v, want 10 tiles", res)
	}
	if img := decodeSheet(t, out); img.Bounds().Dx() != 10*res.TileW {
		t.Fatalf("sheet is %v, want %d px wide", img.Bounds(), 10*res.TileW)
	}
}

// TestMakeFilmstripAudioOnlyFileIsTheFilesFault: a file with no video stream
// cannot yield a sheet, and it is a 4xx-shaped fact about the FILE — permanent,
// never retried, and the scrubber keeps its plain track.
func TestMakeFilmstripAudioOnlyFileIsTheFilesFault(t *testing.T) {
	ffmpeg, _ := binaries(t)
	dir := t.TempDir()
	in := gen(t, ffmpeg, dir, "audio.m4a", join(
		[]string{"-f", "lavfi", "-i", sine("440")}, a,
	)...)
	_, err := MakeFilmstrip(context.Background(), ffmpeg, PlanFilmstrip(6000, 640), in, filepath.Join(dir, "strip.jpg"))
	if !IsPermanent(err) {
		t.Fatalf("err = %v, want a permanent failure", err)
	}
	if !errorsIsNoTiles(err) {
		t.Errorf("err = %v, want it to wrap ErrNoTiles", err)
	}
}

// TestMakeFilmstripCancelledContext: the caller's cancellation comes back as the
// caller's error and never as a permanent failure of the file — the run is
// released and retried, exactly as §11 requires.
func TestMakeFilmstripCancelledContext(t *testing.T) {
	ffmpeg, _ := binaries(t)
	dir := t.TempDir()
	in := renditionClip(t, ffmpeg, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := MakeFilmstrip(ctx, ffmpeg, PlanFilmstrip(6000, 640), in, filepath.Join(dir, "strip.jpg"))
	if err == nil || IsPermanent(err) {
		t.Fatalf("err = %v, want the context's error and not a permanent one", err)
	}
}

func decodeSheet(t *testing.T, path string) image.Image {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open sheet: %v", err)
	}
	defer f.Close()
	img, err := jpeg.Decode(f)
	if err != nil {
		t.Fatalf("the sheet is not a JPEG: %v", err)
	}
	return img
}

// tileLuma is the mean Rec. 601 luma of one tile of the sheet, on the 0-255
// scale — the same measurement the poster's near-black test uses, applied to a
// slice of the row.
func tileLuma(img image.Image, idx int, res FilmstripResult) float64 {
	b := img.Bounds()
	x0 := b.Min.X + idx*res.TileW
	var sum float64
	n := 0
	for y := b.Min.Y; y < b.Max.Y; y += 4 {
		for x := x0; x < x0+res.TileW && x < b.Max.X; x += 4 {
			r, g, bl, _ := img.At(x, y).RGBA()
			sum += (0.299*float64(r) + 0.587*float64(g) + 0.114*float64(bl)) / 257
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// errorsIsNoTiles unwraps the permanent wrapper the run returns.
func errorsIsNoTiles(err error) bool {
	var pe *ErrPermanent
	if errors.As(err, &pe) {
		return errors.Is(pe.Err, ErrNoTiles)
	}
	return errors.Is(err, ErrNoTiles)
}
