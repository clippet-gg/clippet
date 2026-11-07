package transcode

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"

	"clippet/web/static"
)

// The waveform (§20 erratum 329) on synthetic PCM: the peaks reader, the bar
// geometry, the rasteriser, the poster/stencil identity and the mark.

// pcm renders samples as s16le, the peaks pipe's format.
func pcm(samples []int16) []byte {
	b := make([]byte, 2*len(samples))
	for i, s := range samples {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(s))
	}
	return b
}

// sineSamples is n samples of a 441 Hz sine at amplitude amp (of 32767) at 44.1 kHz.
func sineSamples(n int, amp float64) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(math.Round(amp * math.Sin(2*math.Pi*441*float64(i)/44100)))
	}
	return out
}

// heightsOf reads mono samples through the peaks reader.
func heightsOf(t *testing.T, samples []int16) []uint8 {
	t.Helper()
	w := NewPeakWriter(1)
	if _, err := w.Write(pcm(samples)); err != nil {
		t.Fatal(err)
	}
	return w.Heights(WaveBars)
}

func allEqual(h []uint8, v uint8) bool {
	for _, x := range h {
		if x != v {
			return false
		}
	}
	return true
}

// TestWavePeaksPinnedHeights pins the bar heights the reader gives each kind of
// sound: a steady sine is a row of full bars (the clip's own loudest bar is a
// full bar), silence is all zero (drawn as dots), a sine then silence is full
// bars then zero ones, a +3 dBFS float — which the s16 decode SATURATES, the
// most negative sample included — is full and does not overflow, and a sine at
// −40 dBFS is NOT blown up to full height by the normalisation. n is 70 × 4096
// samples, so every bar is exactly 64 fine buckets and the halfway cut falls on
// a bar's edge.
func TestWavePeaksPinnedHeights(t *testing.T) {
	const n = WaveBars * 4096
	if h := heightsOf(t, sineSamples(n, 16384)); !allEqual(h, 255) {
		t.Errorf("a steady sine: %v, want every bar 255", h)
	}
	if h := heightsOf(t, make([]int16, n)); !allEqual(h, 0) {
		t.Errorf("silence: %v, want every bar 0", h)
	}
	half := append(sineSamples(n/2, 16384), make([]int16, n/2)...)
	h := heightsOf(t, half)
	if !allEqual(h[:WaveBars/2], 255) || !allEqual(h[WaveBars/2:], 0) {
		t.Errorf("a sine then silence: %v, want 35 full bars then 35 empty", h)
	}
	hot := make([]int16, n)
	for i := range hot {
		if i%2 == 0 {
			hot[i] = 32767
		} else {
			hot[i] = -32768
		}
	}
	if h := heightsOf(t, hot); !allEqual(h, 255) {
		t.Errorf("a saturated +3 dBFS float: %v, want every bar 255", h)
	}
	neg := make([]int16, n)
	for i := range neg {
		neg[i] = -32768
	}
	if h := heightsOf(t, neg); !allEqual(h, 255) {
		t.Errorf("the most negative sample alone: %v, want every bar 255 (|−32768| must not overflow)", h)
	}
	// −40 dBFS is a peak of 328 of 32768, an RMS of 232: under the −30 dBFS
	// RMS floor (1036), so it is drawn at its share of the floor through the
	// curve — round(255 × (232/1036)^0.6) = 104 — not stretched to 255.
	if h := heightsOf(t, sineSamples(n, 328)); !allEqual(h, 104) {
		t.Errorf("a −40 dBFS sine: %v, want every bar 104", h)
	}
}

// TestPeakWriterStreams pins the reader's streaming: a sample split across
// two writes is read once and whole, and a long source is read in bounded
// memory (the buckets, one frame each at first, double) with the same shape.
func TestPeakWriterStreams(t *testing.T) {
	const n = 44100 * 2
	samples := append(make([]int16, n/2), sineSamples(n/2, 20000)...)
	whole := heightsOf(t, samples)
	w := NewPeakWriter(1)
	for _, b := range pcm(samples) {
		_, _ = w.Write([]byte{b})
	}
	if got := w.Heights(WaveBars); !bytes.Equal(got, whole) {
		t.Errorf("byte-at-a-time writes: %v, want %v", got, whole)
	}
	// 4 million samples arrive: the buckets double from one frame each.
	long := NewPeakWriter(1)
	big := append(make([]int16, 2_000_000), sineSamples(2_000_000, 20000)...)
	_, _ = long.Write(pcm(big))
	if len(long.fine) > 2*peakFineBuckets {
		t.Fatalf("the fine buckets grew to %d (bounded at %d)", len(long.fine), 2*peakFineBuckets)
	}
	h := long.Heights(WaveBars)
	if !allEqual(h[:WaveBars/2-1], 0) || !allEqual(h[WaveBars/2+1:], 255) {
		t.Errorf("an understated length: %v, want silence then full bars", h)
	}
}

// TestPeakWriterHeightsNoBars pins the guard's order (the run 10 nit sweep, W4): a
// bar count of zero or below is an empty answer, with or without sound read —
// never make's "len out of range" panic on a negative n.
func TestPeakWriterHeightsNoBars(t *testing.T) {
	read := NewPeakWriter(1)
	_, _ = read.Write(pcm(sineSamples(44100, 16384)))
	for _, w := range []*PeakWriter{NewPeakWriter(1), read} {
		for _, n := range []int{-1, -70, 0} {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("Heights(%d) panicked: %v", n, r)
					}
				}()
				if h := w.Heights(n); h == nil || len(h) != 0 {
					t.Errorf("Heights(%d) = %#v, want an empty, non-nil slice", n, h)
				}
			}()
		}
	}
}

// TestPeakWriterBucketBound pins the arithmetic the peakBucket comment and
// §20 erratum 332(a)/(c) state (the run 10 nit sweep, W2/W3): after any number of
// merges a bucket holds at most the stream's samples ÷ 4096, and Heights reads
// 4096–8192 buckets (the closed ones and the open one).
func TestPeakWriterBucketBound(t *testing.T) {
	w := NewPeakWriter(2)
	var samples int64
	chunk := pcm(sineSamples(2*12345, 20000)) // 12,345 stereo frames a write
	for i := 0; i < 100; i++ {
		_, _ = w.Write(chunk)
		samples += 2 * 12345
		var most int64
		for _, b := range w.fine {
			most = max(most, b.n)
		}
		if most > max(samples/peakFineBuckets, 2) {
			t.Fatalf("after %d samples a bucket holds %d, over samples ÷ %d", samples, most, peakFineBuckets)
		}
		read := len(w.fine)
		if w.cur.n > 0 {
			read++
		}
		if samples >= 2*peakFineBuckets && (read < peakFineBuckets || read > 2*peakFineBuckets) {
			t.Fatalf("after %d samples Heights reads %d buckets, want %d–%d", samples, read, peakFineBuckets, 2*peakFineBuckets)
		}
	}
}

// TestPeakWriterStereo pins the stereo reading (§20 erratum 332): the pipe
// decodes two channels for a source of two or more, one otherwise, and the
// writer reads every sample of both — a tone whose right channel is its left
// negated (which a mono downmix cancels to nothing) is a row of full bars, and
// its loudness is the same as the same tone in phase. A frame is never split
// between two buckets: the doubling counts frames, not samples.
func TestPeakWriterStereo(t *testing.T) {
	for ch, want := range map[int]string{0: "-ac 1", 1: "-ac 1", 2: "-ac 2", 6: "-ac 2"} {
		if got := joinArgs(PeaksArgs("/s/in", ch)); !bytes.Contains([]byte(got), []byte(want)) {
			t.Errorf("PeaksArgs for %d channels: %s, want %q", ch, got, want)
		}
	}
	const frames = WaveBars * 4096
	tone := sineSamples(frames, 16384)
	inter := func(sign int16) []int16 {
		out := make([]int16, 0, 2*frames)
		for _, s := range tone {
			out = append(out, s, sign*s)
		}
		return out
	}
	anti, same := NewPeakWriter(2), NewPeakWriter(2)
	_, _ = anti.Write(pcm(inter(-1)))
	_, _ = same.Write(pcm(inter(1)))
	ha, hs := anti.Heights(WaveBars), same.Heights(WaveBars)
	if !allEqual(ha, 255) || !bytes.Equal(ha, hs) {
		t.Errorf("antiphase %v, in phase %v: want both every bar 255", ha, hs)
	}
	if anti.per*int64(len(anti.fine)) != frames || anti.fine[0].n != 2*anti.per {
		t.Errorf("%d buckets of %d frames holding %d samples each, for %d frames", len(anti.fine), anti.per, anti.fine[0].n, frames)
	}
	// Half a stereo file loud and half silent splits on the bar's edge.
	half := append(inter(1)[:frames], make([]int16, frames)...)
	w := NewPeakWriter(2)
	_, _ = w.Write(pcm(half))
	if h := w.Heights(WaveBars); !allEqual(h[:WaveBars/2], 255) || !allEqual(h[WaveBars/2:], 0) {
		t.Errorf("a stereo tone then silence: %v", h)
	}
}

// TestWaveBarGeometry pins where the bars are: 70 of them, 10 px wide at a
// pitch of 16, centred in the 1120-px band, mirrored about y = 360, never
// shorter than a dot and never taller than the band.
func TestWaveBarGeometry(t *testing.T) {
	if WaveBars != 70 {
		t.Fatalf("WaveBars = %d, want 70", WaveBars)
	}
	x, y, w, h := WaveBar(0, 0)
	if x != 83 || w != 10 || h != WaveMinBarH || y+h/2 != 360 {
		t.Errorf("bar 0 at silence: x=%v y=%v w=%v h=%v", x, y, w, h)
	}
	x, y, _, h = WaveBar(WaveBars-1, 255)
	if x+10 != 1197 || h != 400 || y != 160 {
		t.Errorf("the last bar at full: x=%v y=%v h=%v", x, y, h)
	}
}

func rgbAt(img image.Image, x, y int) [3]int {
	r, g, b, _ := img.At(x, y).RGBA()
	return [3]int{int(r >> 8), int(g >> 8), int(b >> 8)}
}

func hexTriple(h string) [3]int {
	c := hexRGB(h)
	return [3]int{int(c.R), int(c.G), int(c.B)}
}

// TestWaveRasteriser pins the drawing: a bar's centre is the unplayed colour,
// the gap between two bars and the margins are the ground, a silent bar is a
// dot (its centre lit, a pixel past its radius not), and an edge pixel is
// ANTIALIASED — strictly between the two colours.
func TestWaveRasteriser(t *testing.T) {
	h := make([]uint8, WaveBars)
	h[1] = 255
	img, err := WavePoster(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 1280 || b.Dy() != 720 {
		t.Fatalf("poster %v, want 1280x720", b)
	}
	ground, rest := hexTriple(WaveGroundHex), hexTriple(WaveRestHex)
	x1, _, _, _ := WaveBar(1, 255)
	if got := rgbAt(img, int(x1)+5, 300); got != rest {
		t.Errorf("inside the full bar: %v, want %v", got, rest)
	}
	if got := rgbAt(img, int(x1)+12, 300); got != ground {
		t.Errorf("in the gap: %v, want the ground %v", got, ground)
	}
	if got := rgbAt(img, 40, 40); got != ground {
		t.Errorf("the margin: %v, want the ground", got)
	}
	x0, _, _, _ := WaveBar(0, 0)
	if got := rgbAt(img, int(x0)+5, 359); got != rest {
		t.Errorf("a silent bar's centre: %v, want a lit dot", got)
	}
	if got := rgbAt(img, int(x0)+5, 352); got != ground {
		t.Errorf("past a silent bar's radius: %v, want the ground (a dot, not a line)", got)
	}
	// The full bar's top cap: the pixel straddling its top edge at the bar's
	// centre column is partly covered.
	edge := rgbAt(img, int(x1)+1, 161)
	if edge == ground || edge == rest {
		t.Errorf("the rounded cap's edge pixel %v is not antialiased", edge)
	}
}

// TestWavePosterIsTheStencilOverTheRestColour pins "the poster IS frame 0 of
// the audiogram": compositing the stencil over a flat field of the unplayed
// colour — which is what the audiogram's first frame is — gives the poster's
// pixels to within one level per channel.
func TestWavePosterIsTheStencilOverTheRestColour(t *testing.T) {
	h := make([]uint8, WaveBars)
	for i := range h {
		h[i] = uint8((i * 37) % 256)
	}
	poster, err := WavePoster(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := WaveStencil(h)
	rest := hexRGB(WaveRestHex)
	worst := 0
	for y := 0; y < WaveFrameH; y++ {
		for x := 0; x < WaveFrameW; x++ {
			s := st.NRGBAAt(x, y)
			a := float64(s.A) / 255
			comp := [3]float64{
				float64(s.R)*a + float64(rest.R)*(1-a),
				float64(s.G)*a + float64(rest.G)*(1-a),
				float64(s.B)*a + float64(rest.B)*(1-a)}
			p := poster.RGBAAt(x, y)
			for i, v := range []uint8{p.R, p.G, p.B} {
				if d := int(math.Abs(math.Round(comp[i]) - float64(v))); d > worst {
					worst = d
				}
			}
		}
	}
	if worst > 1 {
		t.Errorf("the stencil over the rest colour differs from the poster by %d levels", worst)
	}
	if st.NRGBAAt(40, 40).A != 255 {
		t.Error("the stencil's ground is not opaque")
	}
	x, _, _, _ := WaveBar(3, h[3])
	if st.NRGBAAt(int(x)+5, 360).A != 0 {
		t.Error("a bar is not a hole in the stencil")
	}
}

// TestWavePosterCarriesTheMark pins the mark: drawn from the manifest icon (its
// one source), in the band's bottom margin with its ink's left edge on the
// first bar's, and nowhere in the band; the stencil never carries it.
func TestWavePosterCarriesTheMark(t *testing.T) {
	mark, err := static.FS.ReadFile("icon-192.png")
	if err != nil {
		t.Fatal(err)
	}
	h := make([]uint8, WaveBars)
	with, err := WavePoster(h, mark)
	if err != nil {
		t.Fatal(err)
	}
	without, _ := WavePoster(h, nil)
	var box image.Rectangle
	for y := 0; y < WaveFrameH; y++ {
		for x := 0; x < WaveFrameW; x++ {
			if with.RGBAAt(x, y) != without.RGBAAt(x, y) {
				box = box.Union(image.Rect(x, y, x+1, y+1))
			}
		}
	}
	if box.Empty() {
		t.Fatal("the poster carries no mark")
	}
	if box.Min.Y < WaveBandY+WaveBandH || box.Max.Y > WaveFrameH || box.Min.X < 83 || box.Max.X > 200 {
		t.Errorf("the mark's ink is at %v; want it in the bottom-left margin, ink left at x=83", box)
	}
	if d := box.Min.X - 83; d < 0 || d > 1 {
		t.Errorf("the mark's ink starts at x=%d, want the first bar's left edge (83)", box.Min.X)
	}
	if h := box.Dy(); h < 45 || h > 65 {
		t.Errorf("the mark stands %d px tall, want ≈55 (the icon halved)", h)
	}
	// The green span is there: the mark is the brand's, not a grey shape.
	green := false
	for y := box.Min.Y; y < box.Max.Y; y++ {
		for x := box.Min.X; x < box.Max.X; x++ {
			c := with.RGBAAt(x, y)
			if c.G > 180 && c.R < 120 {
				green = true
			}
		}
	}
	if !green {
		t.Error("the mark has no green span")
	}
}

// TestAudioPosterIsAPNG pins the poster file: a PNG of the card frame, with no
// text chunk (nothing names a program or a time), drawn from the heights.
func TestAudioPosterIsAPNG(t *testing.T) {
	out := filepath.Join(t.TempDir(), "poster.png")
	h := make([]uint8, WaveBars)
	res, err := AudioPoster(context.Background(), "ffmpeg-not-called", h, nil, out, PosterLabel{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Width != 1280 || res.Height != 720 {
		t.Errorf("result %+v", res)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatal("the poster is not a PNG")
	}
	for _, chunk := range []string{"tEXt", "zTXt", "iTXt", "tIME"} {
		if bytes.Contains(b, []byte(chunk)) {
			t.Errorf("the poster carries a %s chunk", chunk)
		}
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := img.(*image.RGBA); !ok {
		if _, ok := img.ColorModel().(color.Palette); ok {
			t.Error("the poster was palettised")
		}
	}
}

// TestPeaksEncoding pins the row's form: two lowercase hex digits a bar,
// exactly WaveBars bars, round-tripped; any other shape reads as nothing, so
// the page draws the poster's 70 bars or none (§20 erratum 329(a)).
func TestPeaksEncoding(t *testing.T) {
	if s := EncodePeaks([]uint8{0, 15, 16, 255}); s != "000f10ff" {
		t.Fatalf("EncodePeaks = %q", s)
	}
	h := make([]uint8, WaveBars)
	for i := range h {
		h[i] = uint8(i * 7)
	}
	h[0], h[1] = 0x0f, 0xff
	s := EncodePeaks(h)
	if len(s) != 2*WaveBars {
		t.Fatalf("EncodePeaks length %d, want %d", len(s), 2*WaveBars)
	}
	if got := DecodePeaks(s); !bytes.Equal(got, h) {
		t.Errorf("DecodePeaks = %v", got)
	}
	upper := "0F" + s[2:] // hex, but not the worker's lowercase form
	for _, bad := range []string{"", "0", "00", "FF", "zz", "000f10ff", s[:len(s)-2], s + "00",
		s[:len(s)-1] + "g", upper} {
		if got := DecodePeaks(bad); got != nil {
			t.Errorf("DecodePeaks(%q) = %v, want nil", bad, got)
		}
	}
}

// TestPosterContentType pins the poster's media type by its key.
func TestPosterContentType(t *testing.T) {
	if got := PosterContentType("c/x/poster/poster-abcd1234.png"); got != "image/png" {
		t.Errorf("png key: %q", got)
	}
	if got := PosterContentType("c/x/poster/poster-abcd1234.jpg"); got != "image/jpeg" {
		t.Errorf("jpg key: %q", got)
	}
}

// TestPosterLabelArgsForAPNG pins the label pass on a PNG poster: it writes a
// PNG back, losslessly and bitexact; a JPEG poster's argv is unchanged.
func TestPosterLabelArgsForAPNG(t *testing.T) {
	p := PosterLabelArgs("/s/poster.png", "/s/poster.png.lbl", "/f.ttf", "/s/d", 720)
	got := joinArgs(p)
	for _, want := range []string{"-c:v png", "-map_metadata -1", "-fflags +bitexact", "-f image2 /s/poster.png.lbl"} {
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Errorf("png label argv lacks %q: %s", want, got)
		}
	}
	j := joinArgs(PosterLabelArgs("/s/poster.jpg", "/s/poster.jpg.lbl", "/f.ttf", "/s/d", 720))
	if bytes.Contains([]byte(j), []byte("-c:v png")) || !bytes.Contains([]byte(j), []byte("-q:v 2 -f image2")) {
		t.Errorf("the jpeg label argv changed: %s", j)
	}
}

func joinArgs(a []string) string {
	var b bytes.Buffer
	for i, s := range a {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(s)
	}
	return b.String()
}
