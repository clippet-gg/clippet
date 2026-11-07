package transcode

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"strconv"
	"strings"
)

// The waveform an audio clip's card is drawn with (DESIGN.md §20 erratum 329;
// the owner's pick D3a, a drawn waveform of the clip, and the round-2
// research's verdict: draw the picture yourself and let ffmpeg do the motion).
//
// Go reads the clip's LOUDNESS from one streamed ffmpeg pipe (WavePeaks: s16le
// at the native rate, both channels of a stereo source, the energy per bucket,
// constant memory; §20 erratum 332 — the bars were the peaks before; ffmpeg's own
// showwavespic holds the whole decoded file in RAM, which is why it is not
// used), and draws TWO pictures with the standard library alone:
//
//   - the POSTER (WavePoster): rounded vertical bars mirrored about a centre
//     line, in the unplayed colour on the ink tile — `og:image`, the page's
//     picture, the library's tile;
//   - the STENCIL (WaveStencil): the same ink tile with the same bars cut out
//     of it as HOLES (alpha 0 inside a bar, 255 on the ground, the 1 px
//     antialiased edge in between).
//
// The audiogram (AudioLightArgs) then slides a flat amber field under the
// stencil from left to right over the clip's length, so the played bars turn
// amber while the rest stay grey. Frame 0 of it is the poster's bars exactly:
// the poster is the stencil composited over the unplayed colour
// (TestWavePosterIsTheStencilOverTheRestColour pins that to one level).
//
// The same quantised heights are what the page draws its own progress from
// (internal/web, `data-peaks`), and the geometry it needs is exported here so
// there is ONE source for where a bar is.

// The card frame and the band the bars live in. 1280×720 is the card copy's
// frame; the bars sit in a 1120×400 band centred in it, which leaves the
// top-right clear for the poster's duration plate (erratum 287) and the
// bottom-left for the mark (erratum 329).
const (
	WaveFrameW = 1280
	WaveFrameH = 720
	WaveBandX  = 80
	WaveBandY  = 160
	WaveBandW  = 1120
	WaveBandH  = 400
	// WaveBarW is one bar's width and WaveBarGap the space between two: chunky
	// enough to read at a card's own 400 px (≈3 px a bar).
	WaveBarW   = 10
	WaveBarGap = 6
	// WaveBars is how many bars fit the band at that pitch: 70.
	WaveBars = (WaveBandW + WaveBarGap) / (WaveBarW + WaveBarGap)
	// WaveMinBarH is the shortest bar: a bar as tall as it is wide is a DOT,
	// so silence reads as a dotted baseline rather than as nothing.
	WaveMinBarH = WaveBarW
)

// The palette (web/STYLE.md's colour law, the research's mapping): the ground
// is the ink tile the banners and the link card stand on; the unplayed bars are
// the dark palette's --muted; the PLAYED bars are the dark palette's --time —
// "amber = happening". Not green: green means "you can paste this", and the
// card's own edge already says that. A colour change after the owner's look is one
// constant here.
const (
	WaveGroundHex = "#17181C"
	WaveRestHex   = "#A9A59C"
	WavePlayedHex = "#F0B24A"
)

// waveBarsOffset centres the run of bars inside the band: 70 bars at a pitch of
// 16 less the last gap is 1114 px, 3 px short of each side of 1120.
const waveBarsOffset = (WaveBandW - (WaveBars*(WaveBarW+WaveBarGap) - WaveBarGap)) / 2

// WaveBar is the rectangle bar i of a waveform is drawn in, for a height h
// quantised to 0..255: its left edge, top, width and height in frame pixels.
// The bar is mirrored about the band's centre line and never shorter than
// WaveMinBarH. The poster, the stencil and the page all ask this.
func WaveBar(i int, h uint8) (x, y, w, hh float64) {
	x = float64(WaveBandX + waveBarsOffset + i*(WaveBarW+WaveBarGap))
	hh = math.Max(WaveMinBarH, float64(h)/255*WaveBandH)
	y = WaveBandY + (WaveBandH-hh)/2
	return x, y, WaveBarW, hh
}

// ---- the peaks -----------------------------------------------------------

// PeaksArgs renders the peaks pipe: the clip's one audio stream decoded at its
// NATIVE rate (a resample's lowpass would change the reading) as raw signed
// 16-bit little-endian samples on stdout — STEREO, interleaved, when the source
// has two channels or more, mono otherwise. Never `-ac 1` for a stereo source:
// a downmix sums L and R, so a file whose channels are in antiphase (L = −R:
// karaoke tricks, "8D" mixes, some vinyl rips) mixes to silence and its card
// would draw a dotted line under an audible clip (§20 erratum 332). The integer
// conversion saturates a float sample past full scale, which is the right
// reading of a +3 dBFS file: its bars are full.
func PeaksArgs(in string, channels int) []string {
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-nostats"}
	args = append(args, inputArgs(in)...)
	return append(args, "-map", "0:a:0", "-ac", strconv.Itoa(peaksChannels(channels)), "-c:a", "pcm_s16le", "-f", "s16le", "-")
}

// peaksChannels is how many channels the peaks pipe decodes for a source of
// `channels`: two for stereo and more (a 5.1 source is downmixed to its stereo
// pair), one for mono or an unknown layout.
func peaksChannels(channels int) int {
	if channels >= 2 {
		return 2
	}
	return 1
}

// peakFineBuckets is how many fine buckets the reader keeps before it halves
// them. The memory is 2 × this many peakBuckets whatever the clip's length.
const peakFineBuckets = 4096

// rmsFloor is the divisor's floor (§20 erratum 332): −30 dBFS RMS, 1036 of
// 32768. A clip whose loudest bar's RMS is quieter than that — hiss, a room
// tone, a voice memo of nothing — is drawn at its real share of the floor, not
// blown up into a full-height waveform by the normalisation. It is the peak
// reading's old floor, −20 dBFS, moved down by the ≈10 dB a loud bar's RMS
// sits under its peak (the brick fixture measures 6–11 dB, a sine 3 dB): the
// same material is floored at the same loudness. Through the curve
// a −40 dBFS tone is 41 % of the band and a hiss at −60 dBFS RMS 13 %.
const rmsFloor = 1036

// waveGamma is the perceptual curve the bars are drawn through: a bar's height
// is (rms / the loudest bar's rms)^0.6, so a passage 20 dB under the loudest is
// a quarter of the band rather than a tenth, and the quiet intro of a loud
// track reads as quiet, not as silence. A 40 dB decibel scale was the
// alternative, rendered beside it on the brick fixture: it lifts the quiet
// parts further (the intro at a third of the band) but flattens the loud ones
// (a breakdown 4 dB under the drop drew 223 against 247 — two bars a reader
// cannot tell apart; through the gamma 180 against 234), and a rising sound's
// top bars land within a level of each other.
const waveGamma = 0.6

// peakBucket is one fine bucket: max |s| over every sample of every channel in
// it, the sum of their squares and how many samples that sum holds. The max is
// not drawn (the bars are loudness); it is kept as the reading a clipping
// indicator or a peak-based near-silence test would want. The sum is an exact
// uint64: one square is at most 32768² = 2^30, and a bucket holds at most
// the stream's samples ÷ 4096 (a merge adds its two halves' sums and counts,
// and leaves 4096 buckets of the new size behind it), so it cannot overflow
// below 2^34 samples a bucket — 2^46 samples a clip, years of sound. A bar's sum, which spans many buckets, is taken in
// float64.
type peakBucket struct {
	max int32
	sq  uint64
	n   int64
}

func (b *peakBucket) merge(o peakBucket) {
	b.max = max(b.max, o.max)
	b.sq += o.sq
	b.n += o.n
}

// PeakWriter is the streaming half of the peaks reader: an io.Writer over
// s16le (mono, or interleaved stereo) that keeps, per bucket of FRAMES, the
// peak and the energy of every sample of every channel, in constant memory.
// The bucket starts at ONE frame and DOUBLES (pairs merged) whenever the
// buckets fill, so the container's claimed length — which can understate or
// overstate — is never read: every clip of more than 4096 frames ends with
// 4096–8192 buckets, after ⌈log2(frames/8192)⌉ merges of 4096 buckets each (15
// for an hour at 48 kHz). §20 erratum 332: sizing the bucket from the claim
// drew a 10-second file whose header said an hour from twelve buckets, six
// bars a step.
type PeakWriter struct {
	ch     int          // interleaved channels in the stream: 1 or 2
	per    int64        // frames per fine bucket
	fine   []peakBucket // the closed buckets
	cur    peakBucket   // the open bucket
	frames int64        // frames in the open bucket
	at     int          // the channel the next sample belongs to
	carry  []byte       // a sample split across two writes
}

// NewPeakWriter reads a stream of `channels` interleaved channels, as
// PeaksArgs decodes it for a source of that many (anything under 2 is mono).
func NewPeakWriter(channels int) *PeakWriter {
	return &PeakWriter{ch: peaksChannels(channels), per: 1, fine: make([]peakBucket, 0, 2*peakFineBuckets)}
}

// Write consumes raw samples.
func (w *PeakWriter) Write(p []byte) (int, error) {
	n := len(p)
	if len(w.carry) == 1 && len(p) > 0 {
		w.sample(int16(uint16(w.carry[0]) | uint16(p[0])<<8))
		w.carry, p = w.carry[:0], p[1:]
	}
	for len(p) >= 2 {
		w.sample(int16(uint16(p[0]) | uint16(p[1])<<8))
		p = p[2:]
	}
	if len(p) == 1 {
		w.carry = append(w.carry[:0], p[0])
	}
	return n, nil
}

func (w *PeakWriter) sample(s int16) {
	a := int32(s)
	if a < 0 {
		a = -a // -32768 becomes 32768 in an int32: no overflow
	}
	if a > w.cur.max {
		w.cur.max = a
	}
	w.cur.sq += uint64(a) * uint64(a)
	w.cur.n++
	if w.at++; w.at < w.ch {
		return
	}
	w.at = 0
	if w.frames++; w.frames == w.per {
		w.close()
	}
}

func (w *PeakWriter) close() {
	w.fine = append(w.fine, w.cur)
	w.cur, w.frames = peakBucket{}, 0
	if len(w.fine) == 2*peakFineBuckets {
		for i := 0; i < peakFineBuckets; i++ {
			b := w.fine[2*i]
			b.merge(w.fine[2*i+1])
			w.fine[i] = b
		}
		w.fine = w.fine[:peakFineBuckets]
		w.per *= 2
	}
}

// Heights reduces what was read to n bars quantised to 0..255: each bar's
// LOUDNESS — the RMS of every sample of every channel it covers — over the
// loudest bar's (never over less than rmsFloor), through waveGamma. A bar of
// pure silence is 0, which the drawing shows as a dot (§20 erratum 329); the
// loudest bar of a clip is a full bar.
func (w *PeakWriter) Heights(n int) []uint8 {
	fine := w.fine
	if w.cur.n > 0 {
		fine = append(fine[:len(fine):len(fine)], w.cur)
	}
	if n <= 0 {
		return []uint8{}
	}
	out := make([]uint8, n)
	if len(fine) == 0 {
		return out
	}
	rms := make([]float64, n)
	var top float64
	for j := range rms {
		lo, hi := j*len(fine)/n, (j+1)*len(fine)/n
		if hi <= lo {
			hi = lo + 1 // fewer buckets than bars (a clip under 70 frames): a bar repeats its bucket
		}
		var sq, cnt float64
		for _, b := range fine[lo:min(hi, len(fine))] {
			sq += float64(b.sq)
			cnt += float64(b.n)
		}
		if cnt > 0 {
			rms[j] = math.Sqrt(sq / cnt)
		}
		top = math.Max(top, rms[j])
	}
	div := math.Max(top, rmsFloor)
	for j, v := range rms {
		out[j] = uint8(math.Min(255, math.Round(255*waveCurve(v/div))))
	}
	return out
}

// waveCurve maps a bar's share of the loudest bar's RMS (0..1) to its share of
// the band's height.
func waveCurve(v float64) float64 {
	if v <= 0 {
		return 0
	}
	return math.Pow(v, waveGamma)
}

// WavePeaks runs PeaksArgs and streams the samples into a PeakWriter: n bars,
// quantised. Outcomes match RunAudio's (ErrPermanent for the file's fault).
func WavePeaks(ctx context.Context, ffmpeg, in string, p *ProbeResult, n int) ([]uint8, error) {
	channels := 1
	if p != nil {
		channels = p.AudioChannels
	}
	pw := NewPeakWriter(channels)
	cmd := newCommand(ctx, ffmpeg, PeaksArgs(in, channels)...)
	r := newRing(stderrRingSize)
	cmd.Stdout, cmd.Stderr = pw, r
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		reason := exitReason("ffmpeg", err, captureOf(r))
		if isExit(err) {
			return nil, permanent("%s", reason)
		}
		return nil, fmt.Errorf("%s", reason)
	}
	return pw.Heights(n), nil
}

// ---- the drawing ---------------------------------------------------------

// hexRGB parses one of the palette constants above.
func hexRGB(h string) color.NRGBA {
	v, _ := strconv.ParseUint(h[1:], 16, 32)
	return color.NRGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 255}
}

// barCoverage is how much of the pixel whose top-left corner is (px, py) a
// rounded bar covers: the bar's signed distance at the pixel's centre, mapped
// to a 1 px ramp — an analytic antialias with no supersampling. The radius is
// half the bar's width, so both ends are round.
func barCoverage(px, py int, x, y, w, h float64) float64 {
	r := w / 2
	cx, cy := x+w/2, y+h/2
	qx := math.Abs(float64(px)+0.5-cx) - (w/2 - r)
	qy := math.Abs(float64(py)+0.5-cy) - (h/2 - r)
	d := math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) + math.Min(math.Max(qx, qy), 0) - r
	return math.Min(1, math.Max(0, 0.5-d))
}

// eachBarPixel calls fn for every pixel a bar of `heights` touches, with its
// coverage. Bars never overlap (the gap is wider than the antialias ramp), so
// each pixel is visited at most once.
func eachBarPixel(heights []uint8, fn func(px, py int, c float64)) {
	for i, h := range heights {
		if i >= WaveBars {
			break
		}
		x, y, w, hh := WaveBar(i, h)
		for py := int(math.Floor(y)) - 1; py <= int(math.Ceil(y+hh)); py++ {
			for px := int(math.Floor(x)) - 1; px <= int(math.Ceil(x+w)); px++ {
				if c := barCoverage(px, py, x, y, w, hh); c > 0 {
					fn(px, py, c)
				}
			}
		}
	}
}

func lerp8(a, b uint8, t float64) uint8 {
	return uint8(math.Round(float64(a) + (float64(b)-float64(a))*t))
}

// WavePoster draws the poster: the bars in the unplayed colour on the ink
// tile, then the mark (the 192 px manifest icon, `mark`, its tile the same ink
// as the ground) in the bottom-left corner when one is given.
func WavePoster(heights []uint8, mark []byte) (*image.RGBA, error) {
	ground, rest := hexRGB(WaveGroundHex), hexRGB(WaveRestHex)
	img := image.NewRGBA(image.Rect(0, 0, WaveFrameW, WaveFrameH))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: ground}, image.Point{}, draw.Src)
	eachBarPixel(heights, func(px, py int, c float64) {
		img.SetRGBA(px, py, color.RGBA{R: lerp8(ground.R, rest.R, c), G: lerp8(ground.G, rest.G, c),
			B: lerp8(ground.B, rest.B, c), A: 255})
	})
	if len(mark) > 0 {
		if err := drawMark(img, mark); err != nil {
			return nil, err
		}
	}
	return img, nil
}

// WaveStencil draws the stencil: the ink tile, opaque, with every bar cut out
// as a hole (alpha 1 − coverage).
func WaveStencil(heights []uint8) *image.NRGBA {
	ground := hexRGB(WaveGroundHex)
	img := image.NewNRGBA(image.Rect(0, 0, WaveFrameW, WaveFrameH))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: ground}, image.Point{}, draw.Src)
	eachBarPixel(heights, func(px, py int, c float64) {
		img.SetNRGBA(px, py, color.NRGBA{R: ground.R, G: ground.G, B: ground.B, A: uint8(math.Round(255 * (1 - c)))})
	})
	return img
}

// The mark on the poster (erratum 329): the companion mark from its ONE source,
// the manifest icon scripts/lib/mkicons.py draws (web/static/icon-192.png),
// never redrawn — halved by a 2×2 box filter so its clapper stands ≈55 px tall,
// the height of the duration plate in the opposite corner, and placed with its
// INK's left edge on the first bar's and its ink centred in the band's bottom
// margin. Its rounded tile is the same ink as the ground and disappears into
// it. It is on the POSTER only — the card picture a reader sees before pressing
// play, as the placeholder card carries it — never in the audiogram itself,
// which is a picture of the clip and carries no brand (no watermarks).
func drawMark(dst *image.RGBA, src []byte) error {
	m, err := png.Decode(bytes.NewReader(src))
	if err != nil {
		return fmt.Errorf("the mark: %w", err)
	}
	half := halve(m)
	ground := hexRGB(WaveGroundHex)
	ink := inkBox(half, ground)
	if ink.Empty() {
		return nil
	}
	left := WaveBandX + waveBarsOffset
	top := (WaveBandY+WaveBandH+WaveFrameH)/2 - ink.Dy()/2
	at := image.Pt(left-ink.Min.X, top-ink.Min.Y)
	// Only the INK is laid on: the tile's own pixels — and the antialiased rim
	// of its rounded corners, whose premultiplied rounding is a level off the
	// ground — are left as the ground they already are.
	for y := ink.Min.Y; y < ink.Max.Y; y++ {
		for x := ink.Min.X; x < ink.Max.X; x++ {
			c := half.RGBAAt(x, y)
			if c.A == 0 || isGround(c, ground) {
				continue
			}
			d := dst.RGBAAt(x+at.X, y+at.Y)
			inv := 255 - uint32(c.A)
			dst.SetRGBA(x+at.X, y+at.Y, color.RGBA{
				R: uint8(uint32(c.R) + uint32(d.R)*inv/255), G: uint8(uint32(c.G) + uint32(d.G)*inv/255),
				B: uint8(uint32(c.B) + uint32(d.B)*inv/255), A: 255})
		}
	}
	return nil
}

// isGround reports whether a premultiplied pixel is the ground colour at its
// own coverage, to within the rounding a premultiplied, halved antialiased
// edge carries (a few levels per channel; the mark's own colours are a hundred
// and more away).
func isGround(c color.RGBA, ground color.NRGBA) bool {
	near := func(v, g uint8) bool { return abs(int(v)*255-int(g)*int(c.A)) <= 5*255 }
	return near(c.R, ground.R) && near(c.G, ground.G) && near(c.B, ground.B)
}

// halve is a 2×2 box filter over premultiplied colour: exact for a mark the
// generator already antialiased at 4×4.
func halve(m image.Image) *image.RGBA {
	b := m.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx()/2, b.Dy()/2))
	for y := 0; y < out.Rect.Dy(); y++ {
		for x := 0; x < out.Rect.Dx(); x++ {
			var r, g, bl, a uint32
			for dy := 0; dy < 2; dy++ {
				for dx := 0; dx < 2; dx++ {
					cr, cg, cb, ca := m.At(b.Min.X+2*x+dx, b.Min.Y+2*y+dy).RGBA()
					r, g, bl, a = r+cr, g+cg, bl+cb, a+ca
				}
			}
			out.SetRGBA(x, y, color.RGBA{R: uint8(r / 4 >> 8), G: uint8(g / 4 >> 8), B: uint8(bl / 4 >> 8), A: uint8(a / 4 >> 8)})
		}
	}
	return out
}

// inkBox is the bounds of the pixels of m that are not the ground colour (or
// transparent): the clapper and its span, without the tile.
func inkBox(m *image.RGBA, ground color.NRGBA) image.Rectangle {
	var box image.Rectangle
	for y := m.Rect.Min.Y; y < m.Rect.Max.Y; y++ {
		for x := m.Rect.Min.X; x < m.Rect.Max.X; x++ {
			c := m.RGBAAt(x, y)
			if c.A < 128 || isGround(c, ground) {
				continue
			}
			box = box.Union(image.Rect(x, y, x+1, y+1))
		}
	}
	return box
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// WritePNG writes one of the drawings above. The standard library's encoder
// writes no text chunk — no software name, no time — so the file carries
// nothing but its pixels.
func WritePNG(img image.Image, path string) error {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return fmt.Errorf("waveform png: %w", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("waveform png: %w", err)
	}
	return nil
}

// EncodePeaks is how the quantised heights are kept on the clip's row and in
// the page (§20 erratum 329): two lowercase hex digits per bar, 140 characters
// for WaveBars. DecodePeaks is its inverse, and answers nil for anything that is
// not exactly that shape — a row the page cannot read draws no bars of its own.
func EncodePeaks(h []uint8) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(h))
	for _, v := range h {
		out = append(out, digits[v>>4], digits[v&15])
	}
	return string(out)
}

// DecodePeaks reads EncodePeaks' form back: exactly WaveBars bars of two
// lowercase hex digits, the one shape the worker writes, so the page draws the
// poster's bars or none (§20 erratum 329(a)). Anything else — shorter, longer,
// uppercase (app.js's reader refuses it too), not hex — reads as nothing.
func DecodePeaks(s string) []uint8 {
	if len(s) != 2*WaveBars {
		return nil
	}
	out := make([]uint8, WaveBars)
	for i := range out {
		hi, okHi := lowerHex(s[2*i])
		lo, okLo := lowerHex(s[2*i+1])
		if !okHi || !okLo {
			return nil
		}
		out[i] = hi<<4 | lo
	}
	return out
}

// lowerHex is one lowercase hex digit's value.
func lowerHex(b byte) (uint8, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	}
	return 0, false
}

// PosterContentType is the media type a poster object is stored and served
// under, from its key: image/png for an audio clip's drawn waveform (§20
// erratum 329), image/jpeg for every frame cut out of a video. The key's
// extension decides, so the bucket, the route and the card's og:image:type
// all agree.
func PosterContentType(key string) string {
	if strings.HasSuffix(key, ".png") {
		return "image/png"
	}
	return "image/jpeg"
}
