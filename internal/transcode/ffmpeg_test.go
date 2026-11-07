//go:build ffmpeg

package transcode

// The -tags ffmpeg matrix (DESIGN.md §16.2, §16.4): generate the test
// clips with lavfi inside the container, run Probe→Plan→Encode→Verify→
// Poster end to end and assert the measured facts — including the
// dual-audio both-ears check that "one stereo stream" alone cannot see, and
// an SSIM geometry check that dimensions alone cannot see (a 1280×720 frame
// squeezed unrotated into 720×1280 has the right size and the wrong picture).

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var ffmpegOpts = EncodeOptions{Preset: "veryfast", CRF: 23, StallTimeout: 5 * time.Minute, HardTimeout: 6 * time.Hour}

// SSIM thresholds (measured on 7.1.1: a faithful rendition scores ≥ 0.95,
// a squeezed or wrongly turned one 0.51–0.61).
const (
	ssimFaithful = 0.9
	ssimWrong    = 0.7
)

func binaries(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	var err error
	if ffmpeg, err = exec.LookPath("ffmpeg"); err != nil {
		t.Fatalf("ffmpeg tag set but no ffmpeg on PATH: %v", err)
	}
	if ffprobe, err = exec.LookPath("ffprobe"); err != nil {
		t.Fatalf("ffmpeg tag set but no ffprobe on PATH: %v", err)
	}
	return ffmpeg, ffprobe
}

func run(t *testing.T, bin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", filepath.Base(bin), strings.Join(args, " "), err, out)
	}
	return string(out)
}

// gen renders one clip (file is the output name with its extension); every
// lavfi recipe mirrors scripts/testclips.sh.
func gen(t *testing.T, ffmpeg, dir, file string, args ...string) string {
	t.Helper()
	out := filepath.Join(dir, file)
	full := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
	run(t, ffmpeg, append(full, out)...)
	return out
}

type matrixCase struct {
	name string
	// make returns the clip to transcode and the PLAIN (unrotated) source
	// the SSIM reference is rendered from — the same file unless the case
	// is a rotation remux.
	make               func(t *testing.T, ffmpeg, dir string) (in, src string)
	w, h               int
	fps                Rational
	audio              AudioMode
	displayW, displayH int
	preRotate          int
	// ref turns the plain source into the expected orientation (before the
	// scale); squish adds the negative control: the plain source merely
	// scaled to W×H must NOT resemble the output.
	ref    string
	squish bool
}

const dur = "6"

var v = []string{"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p"}
var a = []string{"-c:a", "aac", "-b:a", "128k"}

func src(size, rate string) string {
	return "testsrc2=duration=" + dur + ":size=" + size + ":rate=" + rate
}

func sine(freq string) string { return "sine=frequency=" + freq + ":duration=" + dur }

func join(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// plain renders a 1280x720 30 fps clip with a tone, the base of every
// rotation remux.
func plain(t *testing.T, ff, dir, freq string) string {
	return gen(t, ff, dir, "plain-"+freq+".mp4", join([]string{"-f", "lavfi", "-i", src("1280x720", "30"), "-f", "lavfi", "-i", sine(freq)}, v, a, []string{"-shortest"})...)
}

// same is the make of a case whose reference source is the clip itself.
func same(fn func(t *testing.T, ff, dir string) string) func(*testing.T, string, string) (string, string) {
	return func(t *testing.T, ff, dir string) (string, string) {
		p := fn(t, ff, dir)
		return p, p
	}
}

func matrix() []matrixCase {
	return []matrixCase{
		{name: "std-720p30", w: 1280, h: 720, fps: Rational{30, 1}, audio: AudioSingle, displayW: 1280, displayH: 720,
			make: same(func(t *testing.T, ff, dir string) string {
				return gen(t, ff, dir, "std-720p30.mp4", join([]string{"-f", "lavfi", "-i", src("1280x720", "30"), "-f", "lavfi", "-i", sine("440")}, v, a, []string{"-shortest"})...)
			})},
		{name: "noaudio-720p30", w: 1280, h: 720, fps: Rational{30, 1}, audio: AudioNone, displayW: 1280, displayH: 720,
			make: same(func(t *testing.T, ff, dir string) string {
				return gen(t, ff, dir, "noaudio-720p30.mp4", join([]string{"-f", "lavfi", "-i", src("1280x720", "30")}, v, []string{"-an"})...)
			})},
		{name: "vertical-720x1280", w: 720, h: 1280, fps: Rational{30, 1}, audio: AudioSingle, displayW: 720, displayH: 1280,
			make: same(func(t *testing.T, ff, dir string) string {
				return gen(t, ff, dir, "vertical-720x1280.mp4", join([]string{"-f", "lavfi", "-i", src("720x1280", "30"), "-f", "lavfi", "-i", sine("520")}, v, a, []string{"-shortest"})...)
			})},
		{name: "hd-1080p60", w: 1280, h: 720, fps: Rational{60, 1}, audio: AudioSingle, displayW: 1920, displayH: 1080,
			make: same(func(t *testing.T, ff, dir string) string {
				return gen(t, ff, dir, "hd-1080p60.mp4", join([]string{"-f", "lavfi", "-i", src("1920x1080", "60"), "-f", "lavfi", "-i", sine("600")}, v, a, []string{"-shortest"})...)
			})},
		{name: "dualaudio-720p30", w: 1280, h: 720, fps: Rational{30, 1}, audio: AudioMerge, displayW: 1280, displayH: 720,
			make: same(func(t *testing.T, ff, dir string) string {
				return gen(t, ff, dir, "dualaudio-720p30.mp4", join([]string{"-f", "lavfi", "-i", src("1280x720", "30"), "-f", "lavfi", "-i", sine("440"), "-f", "lavfi", "-i", sine("880"),
					"-map", "0:v", "-map", "1:a", "-map", "2:a"}, v, a, []string{"-shortest", "-metadata:s:a:0", "title=game", "-metadata:s:a:1", "title=mic"})...)
			})},
		// Display Matrix rotations: ffmpeg autorotates them, the plan only
		// swaps the dims. -display_rotation is counter-clockwise, so +90 is
		// transpose=2 and -90 transpose=1.
		{name: "rotated90-sidedata", w: 720, h: 1280, fps: Rational{30, 1}, audio: AudioSingle, displayW: 720, displayH: 1280, ref: "transpose=2,", squish: true,
			make: func(t *testing.T, ff, dir string) (string, string) {
				p := plain(t, ff, dir, "330")
				return gen(t, ff, dir, "rotated90-sidedata.mp4", "-display_rotation", "90", "-i", p, "-c", "copy", "-movflags", "+faststart"), p
			}},
		{name: "rotated-neg90", w: 720, h: 1280, fps: Rational{30, 1}, audio: AudioSingle, displayW: 720, displayH: 1280, ref: "transpose=1,", squish: true,
			make: func(t *testing.T, ff, dir string) (string, string) {
				p := plain(t, ff, dir, "340")
				return gen(t, ff, dir, "rotated-neg90.mp4", "-display_rotation", "-90", "-i", p, "-c", "copy"), p
			}},
		{name: "rotated-180", w: 1280, h: 720, fps: Rational{30, 1}, audio: AudioSingle, displayW: 1280, displayH: 720, ref: "hflip,vflip,", squish: true,
			make: func(t *testing.T, ff, dir string) (string, string) {
				p := plain(t, ff, dir, "350")
				return gen(t, ff, dir, "rotated-180.mp4", "-display_rotation", "180", "-i", p, "-c", "copy"), p
			}},
		// Sub-degree matrix angles straddling ffmpeg's own autorotate
		// boundary (§20 erratum 47). ffprobe prints "rotation": 89 for BOTH
		// — the key is a C cast — while ffmpeg rounds the raw angle: 89.6
		// is transposed (dims swap) and 89.4 is turned in place by a rotate
		// filter on the untouched 1280×720 canvas (dims must NOT swap).
		{name: "rotated-89.6-sidedata", w: 720, h: 1280, fps: Rational{30, 1}, audio: AudioSingle, displayW: 720, displayH: 1280, ref: "transpose=2,", squish: true,
			make: func(t *testing.T, ff, dir string) (string, string) {
				p := plain(t, ff, dir, "380")
				return gen(t, ff, dir, "rotated-89.6-sidedata.mp4", "-display_rotation", "89.6", "-i", p, "-c", "copy"), p
			}},
		{name: "rotated-89.4-sidedata", w: 1280, h: 720, fps: Rational{30, 1}, audio: AudioSingle, displayW: 1280, displayH: 720, ref: "rotate=271*PI/180,", squish: true,
			make: func(t *testing.T, ff, dir string) (string, string) {
				p := plain(t, ff, dir, "390")
				return gen(t, ff, dir, "rotated-89.4-sidedata.mp4", "-display_rotation", "89.4", "-i", p, "-c", "copy"), p
			}},
		// Legacy rotate tags (matroska keeps them as ROTATE, no matrix):
		// ffmpeg does NOT autorotate these, so the plan turns them itself.
		// The tag is clockwise: rotate=90 is transpose=1.
		{name: "tags-rotate", w: 720, h: 1280, fps: Rational{30, 1}, audio: AudioSingle, displayW: 720, displayH: 1280, preRotate: 90, ref: "transpose=1,", squish: true,
			make: func(t *testing.T, ff, dir string) (string, string) {
				p := plain(t, ff, dir, "360")
				return gen(t, ff, dir, "tags-rotate.mkv", "-i", p, "-c", "copy", "-metadata:s:v:0", "rotate=90"), p
			}},
		{name: "tags-rotate-neg90", w: 720, h: 1280, fps: Rational{30, 1}, audio: AudioSingle, displayW: 720, displayH: 1280, preRotate: 270, ref: "transpose=2,", squish: true,
			make: func(t *testing.T, ff, dir string) (string, string) {
				p := plain(t, ff, dir, "370")
				return gen(t, ff, dir, "tags-rotate-neg90.mkv", "-i", p, "-c", "copy", "-metadata:s:v:0", "rotate=-90"), p
			}},
		{name: "anamorphic-sar", w: 1046, h: 576, fps: Rational{25, 1}, audio: AudioSingle, displayW: 1047, displayH: 576,
			make: same(func(t *testing.T, ff, dir string) string {
				return gen(t, ff, dir, "anamorphic-sar.mp4", join([]string{"-f", "lavfi", "-i", src("720x576", "25"), "-f", "lavfi", "-i", sine("300"), "-vf", "setsar=16/11"}, v, a, []string{"-shortest"})...)
			})},
		{name: "fractional-2997", w: 1280, h: 720, fps: Rational{30000, 1001}, audio: AudioSingle, displayW: 1280, displayH: 720,
			make: same(func(t *testing.T, ff, dir string) string {
				return gen(t, ff, dir, "fractional-2997.mp4", join([]string{"-f", "lavfi", "-i", src("1280x720", "30000/1001"), "-f", "lavfi", "-i", sine("400")}, v, a, []string{"-shortest"})...)
			})},
		{name: "small-640x360", w: 640, h: 360, fps: Rational{30, 1}, audio: AudioSingle, displayW: 640, displayH: 360,
			make: same(func(t *testing.T, ff, dir string) string {
				return gen(t, ff, dir, "small-640x360.mp4", join([]string{"-f", "lavfi", "-i", src("640x360", "30"), "-f", "lavfi", "-i", sine("450")}, v, a, []string{"-shortest"})...)
			})},
	}
}

func TestFFmpegMatrix(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	box := PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60}
	for _, c := range matrix() {
		c := c
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			in, srcPath := c.make(t, ffmpeg, dir)
			ctx := context.Background()

			p, err := Probe(ffprobe)(ctx, in)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if p.DisplayW != c.displayW || p.DisplayH != c.displayH {
				t.Fatalf("display dims %dx%d, want %dx%d (rotation=%d sar=%d:%d)", p.DisplayW, p.DisplayH, c.displayW, c.displayH, p.Rotation, p.SARNum, p.SARDen)
			}
			if !p.HasDuration || p.DurationUS < 5_500_000 || p.DurationUS > 6_500_000 {
				t.Fatalf("duration = %d us, has=%v", p.DurationUS, p.HasDuration)
			}
			plan := MakePlan(p, box)
			if plan.W != c.w || plan.H != c.h || plan.FPS != c.fps || plan.AudioMode != c.audio {
				t.Fatalf("plan = %dx%d %v %v, want %dx%d %v %v", plan.W, plan.H, plan.FPS, plan.AudioMode, c.w, c.h, c.fps, c.audio)
			}
			if plan.PreRotate != c.preRotate || p.RotationFromTag != (c.preRotate != 0) {
				t.Fatalf("PreRotate = %d (from tag %v), want %d", plan.PreRotate, p.RotationFromTag, c.preRotate)
			}

			out := filepath.Join(dir, "out.mp4")
			var mu sync.Mutex
			var fracs []float64
			err = Encode(ctx, ffmpeg, plan, in, out, ffmpegOpts, func(f float64, ind bool) {
				mu.Lock()
				defer mu.Unlock()
				if ind {
					t.Error("indeterminate progress with a known duration")
				}
				fracs = append(fracs, f)
			})
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if len(fracs) == 0 || fracs[len(fracs)-1] != 1 {
				t.Fatalf("progress must end at 1: %v", fracs)
			}
			for i := 1; i < len(fracs); i++ {
				if fracs[i] < fracs[i-1] || fracs[i] < 0 || fracs[i] > 1 {
					t.Fatalf("progress not monotone in [0,1]: %v", fracs)
				}
			}

			info, err := ProbeOutput(ctx, ffprobe, out)
			if err != nil {
				t.Fatalf("ProbeOutput: %v", err)
			}
			if err := CheckOutput(info, box); err != nil {
				t.Fatalf("CheckOutput: %v (%+v)", err, *info)
			}
			if info.Width != plan.W || info.Height != plan.H {
				t.Fatalf("output %dx%d, plan %dx%d", info.Width, info.Height, plan.W, plan.H)
			}
			if info.FPS != plan.FPS {
				t.Fatalf("output fps %v, plan %v", info.FPS, plan.FPS)
			}
			if info.HasAudio != (c.audio != AudioNone) || info.VideoStreams != 1 || info.AudioStreams > 1 {
				t.Fatalf("streams: %+v", *info)
			}
			if info.DurationMS < 5500 || info.DurationMS > 6500 {
				t.Fatalf("output duration %d ms", info.DurationMS)
			}
			// Every rendition declares Rec.709 — 8-bit yuv420p H.264 is
			// Rec.709 and now says so unconditionally (§20 erratum 93).
			colours := run(t, ffprobe, "-v", "error", "-select_streams", "v:0",
				"-show_entries", "stream=color_primaries,color_transfer,color_space", "-of", "csv=p=0", out)
			if got := strings.TrimSpace(strings.ReplaceAll(colours, "\r", "")); got != "bt709,bt709,bt709" {
				t.Fatalf("output colour tags = %q, want bt709,bt709,bt709", got)
			}
			if err := VerifyMP4(out); err != nil {
				t.Fatalf("VerifyMP4: %v", err)
			}

			// Geometry by content, not by size.
			scale := "scale=" + strconv.Itoa(plan.W) + ":" + strconv.Itoa(plan.H)
			if s := ssim(t, ffmpeg, out, srcPath, c.ref+scale); s < ssimFaithful {
				t.Fatalf("SSIM against %q = %.3f, want ≥ %.2f: geometry wrong", c.ref+scale, s, ssimFaithful)
			}
			if c.squish {
				if s := ssim(t, ffmpeg, out, srcPath, scale); s > ssimWrong {
					t.Fatalf("SSIM against the unrotated %q = %.3f, want ≤ %.2f: output is squeezed, not turned", scale, s, ssimWrong)
				}
			}

			poster := filepath.Join(dir, "poster.jpg")
			res, err := Poster(ctx, ffmpeg, out, poster, int64(info.DurationMS)*1000, true, info.VideoEndSec, PosterBox{}, PosterLabel{})
			if err != nil {
				t.Fatalf("Poster: %v", err)
			}
			if res.Width != info.Width || res.Height != info.Height {
				t.Fatalf("poster %dx%d, output %dx%d", res.Width, res.Height, info.Width, info.Height)
			}
			if res.NearBlack {
				t.Fatalf("the poster of a testsrc2 clip is near-black")
			}
			if st, err := os.Stat(poster); err != nil || st.Size() == 0 {
				t.Fatalf("poster file: %v", err)
			}

			if c.audio == AudioMerge {
				assertBothEars(t, ffmpeg, out)
			}
			if c.audio != AudioNone {
				channels := run(t, ffprobe, "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=channels,sample_rate", "-of", "csv=p=0", out)
				if strings.TrimSpace(channels) != "48000,2" {
					t.Fatalf("audio stream = %q, want 48000,2", strings.TrimSpace(channels))
				}
			}
		})
	}
}

var ssimRE = regexp.MustCompile(`All:([0-9.]+)`)

// ssim scores the output's first frames against a reference rendered from
// the plain source by refFilter, with autorotate OFF on that input so the
// source's own metadata cannot shape the reference.
func ssim(t *testing.T, ffmpeg, out, src, refFilter string) float64 {
	t.Helper()
	cmd := exec.Command(ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "info", "-i", out, "-noautorotate", "-i", src,
		"-filter_complex", "[1:v]"+refFilter+"[ref];[0:v][ref]ssim", "-frames:v", "5", "-f", "null", "-")
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ssim: %v\n%s", err, raw)
	}
	m := ssimRE.FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatalf("no SSIM in ffmpeg output:\n%s", raw)
	}
	s, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("SSIM %q: %v", m[1], err)
	}
	t.Logf("SSIM vs %q = %.4f", refFilter, s)
	return s
}

var rmsRE = regexp.MustCompile(`RMS level dB:\s*(-?[0-9.]+|-inf)`)

// assertBothEars runs the §16.2 bandpass+astats recipe: every source tone
// must be present in BOTH output channels above −40 dB and balanced within
// 1 dB (a dropped track or a hard pan fails; the fixture measures ≈ −24 dB).
func assertBothEars(t *testing.T, ffmpeg, out string) {
	t.Helper()
	levels := map[string]map[int]float64{}
	for _, tone := range []string{"440", "880"} {
		levels[tone] = map[int]float64{}
		for ch := 0; ch < 2; ch++ {
			af := "pan=mono|c0=c" + strconv.Itoa(ch) + ",bandpass=f=" + tone + ":width_type=h:w=40,astats=measure_perchannel=RMS_level:measure_overall=none"
			cmd := exec.Command(ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "info", "-i", out, "-map", "0:a:0", "-af", af, "-f", "null", "-")
			raw, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("astats: %v\n%s", err, raw)
			}
			m := rmsRE.FindStringSubmatch(string(raw))
			if m == nil {
				t.Fatalf("no RMS level in astats output:\n%s", raw)
			}
			db, err := strconv.ParseFloat(m[1], 64)
			if err != nil {
				db = math.Inf(-1)
			}
			levels[tone][ch] = db
		}
	}
	t.Logf("both-ears levels dB: %v", levels)
	for tone, chs := range levels {
		for ch, db := range chs {
			if !(db > -40) {
				t.Errorf("tone %s Hz in channel %d is %.1f dB (want > -40): track dropped or panned", tone, ch, db)
			}
		}
		if d := math.Abs(chs[0] - chs[1]); !(d < 1) {
			t.Errorf("tone %s Hz |L-R| = %.2f dB (want < 1): hard pan", tone, d)
		}
	}
}

// TestFFmpegHDRIsTonemappedAndTaggedSDR (§20 erratum 93). Xbox, PS5, Game
// Bar, the NVIDIA App and every recent iPhone capture HDR by DEFAULT, so an
// HDR10 upload is the ordinary case, not an exotic one. Two separate things
// were wrong with what came out:
//
//  1. the rendition is 8-bit yuv420p H.264 and ffmpeg propagated the SOURCE's
//     colour tags onto it, so the file claimed to be PQ/BT.2020 — a claim no
//     8-bit H.264 stream can honour. Players trusted it and applied a second
//     HDR→SDR transform to an already-truncated signal;
//  2. `format=yuv420p` converts bit depth and chroma, not the transfer
//     function, so the highlights were never mapped at all.
//
// The assertions are therefore in two halves: the output must SAY bt709 on
// all three fields, and it must LOOK like the SDR original rather than like
// the dark thing today's pipeline produces. The fixture is built here rather
// than checked in — a 10-bit HEVC clip is large and the recipe is the point:
// SMPTE bars taken to LINEAR LIGHT and re-encoded through the PQ transfer
// over BT.2020 primaries, which is a real HDR10 signal rather than SDR code
// values wearing an HDR label (mislabelling them would not reproduce the
// defect, and measuring it would prove nothing).
func TestFFmpegHDRIsTonemappedAndTaggedSDR(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	if !strings.Contains(run(t, ffmpeg, "-hide_banner", "-encoders"), "libx265") {
		t.Skip("no libx265 in this ffmpeg: cannot build a 10-bit HDR10 fixture (the tone map itself needs only zscale+tonemap)")
	}
	if f := run(t, ffmpeg, "-hide_banner", "-filters"); !strings.Contains(f, "zscale") || !strings.Contains(f, "tonemap") {
		t.Skip("this ffmpeg has no zscale/tonemap (libzimg): the shipped image does")
	}
	dir := t.TempDir()
	const bars = "smptehdbars=size=1280x720:rate=30:duration=3"
	// zscale refuses to linearize a frame that declares nothing, hence the
	// setparams in front: the bars ARE bt709/tv, they just do not say so.
	in := gen(t, ffmpeg, dir, "hdr10.mp4", "-f", "lavfi", "-i", bars, "-vf",
		"setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=tv,"+
			"zscale=t=linear:npl=100,format=gbrpf32le,"+
			"zscale=p=bt2020:t=smpte2084:m=bt2020nc:r=tv,format=yuv420p10le",
		"-c:v", "libx265", "-x265-params", "colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc",
		"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc", "-an")
	// The same bars through the ordinary SDR pipeline: what the clip is
	// SUPPOSED to look like.
	sdrSrc := gen(t, ffmpeg, dir, "sdr.mp4", "-f", "lavfi", "-i", bars,
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-an")

	ctx := context.Background()
	p, err := Probe(ffprobe)(ctx, in)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if p.ColorTransfer != "smpte2084" || p.ColorPrimaries != "bt2020" || p.ColorSpace != "bt2020nc" {
		t.Fatalf("the fixture is not HDR10 as probed: %q/%q/%q", p.ColorPrimaries, p.ColorTransfer, p.ColorSpace)
	}
	box := PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60}
	plan := MakePlan(p, box)
	if !plan.Tonemap {
		t.Fatal("an smpte2084 source must be planned with a tone map")
	}
	out := filepath.Join(dir, "out.mp4")
	if err := Encode(ctx, ffmpeg, plan, in, out, ffmpegOpts, nil); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	// Half one: the file no longer lies. yuv420p AND bt709 on all three.
	fields := run(t, ffprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=pix_fmt,color_primaries,color_transfer,color_space", "-of", "csv=p=0", out)
	if got := strings.TrimSpace(strings.ReplaceAll(fields, "\r", "")); got != "yuv420p,bt709,bt709,bt709" {
		t.Fatalf("output colour = %q, want %q", got, "yuv420p,bt709,bt709,bt709")
	}
	if err := VerifyMP4(out); err != nil {
		t.Fatalf("VerifyMP4: %v", err)
	}

	// Half two: it LOOKS right. Three measurements, on the same bars:
	// the SDR reference, this pipeline's HDR output, and the control — the
	// same plan with the map switched off, i.e. exactly what shipped before.
	control := filepath.Join(dir, "control.mp4")
	flat := plan
	flat.Tonemap = false
	if err := Encode(ctx, ffmpeg, flat, in, control, ffmpegOpts, nil); err != nil {
		t.Fatalf("Encode (control): %v", err)
	}
	sdrOut := filepath.Join(dir, "sdrout.mp4")
	sp, err := Probe(ffprobe)(ctx, sdrSrc)
	if err != nil {
		t.Fatal(err)
	}
	if err := Encode(ctx, ffmpeg, MakePlan(sp, box), sdrSrc, sdrOut, ffmpegOpts, nil); err != nil {
		t.Fatalf("Encode (SDR reference): %v", err)
	}
	reference, mapped, unmapped := meanLuma(t, ffmpeg, sdrOut), meanLuma(t, ffmpeg, out), meanLuma(t, ffmpeg, control)
	t.Logf("mean luma: SDR reference %.2f, tone mapped %.2f, untone-mapped %.2f", reference, mapped, unmapped)
	// The defect: without the map the HDR clip is far darker than the SDR
	// original. If this ever stops holding the fixture has stopped being HDR
	// and the rest of the test would pass for the wrong reason.
	if unmapped >= reference*0.95 {
		t.Fatalf("the untone-mapped control (%.2f) is not darker than the SDR reference (%.2f): the fixture is not HDR", unmapped, reference)
	}
	if mapped <= unmapped {
		t.Fatalf("tone mapped mean luma %.2f is no brighter than untone-mapped %.2f: the map did nothing", mapped, unmapped)
	}
	// And it lands near the SDR original rather than merely somewhere above
	// the control — the operator was chosen on exactly this measurement.
	if d := math.Abs(mapped-reference) / reference; d > 0.15 {
		t.Fatalf("tone mapped mean luma %.2f is %.0f%% off the SDR reference %.2f", mapped, d*100, reference)
	}
}

// TestFFmpegHDRTaggedOnlyOnTheTransferStillTranscodes (§20 erratum 93,
// amended). The fixture above is a WELL-FORMED HDR10 file, and a great many
// real ones are not: a capture that names its transfer and leaves primaries
// and matrix unspecified is the ordinary output of several screen recorders
// and of any remux that dropped the container-level tags. zscale converts
// between colorspaces it can NAME, so on such a source both zscales in
// tonemapChain answer "code 3074: no path between colorspaces", ffmpeg exits
// non-zero, and the clip fails permanently — a file that transcoded fine (as
// SDR, untouched) before the tone map shipped.
//
// The test proves its own premise first: the same plan with the assumed tags
// removed is exactly what shipped, and it must FAIL. Then the real plan must
// produce a Rec.709 rendition like any other.
func TestFFmpegHDRTaggedOnlyOnTheTransferStillTranscodes(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	if f := run(t, ffmpeg, "-hide_banner", "-filters"); !strings.Contains(f, "zscale") || !strings.Contains(f, "tonemap") {
		t.Skip("this ffmpeg has no zscale/tonemap (libzimg): the shipped image does")
	}
	dir := t.TempDir()
	// 8-bit yuv420p H.264 — no libx265 needed, because the defect is about
	// what the file SAYS, not about its depth. `setparams` writes the frame
	// properties x264 then copies into the stream; the two explicit
	// `unknown`s keep a lavfi source's own guesses out of the fixture.
	in := gen(t, ffmpeg, dir, "pq_transfer_only.mp4", "-f", "lavfi",
		"-i", "smptehdbars=size=640x360:rate=30:duration=2",
		"-vf", "format=yuv420p,setparams=color_primaries=unknown:colorspace=unknown:color_trc=smpte2084",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-an")

	ctx := context.Background()
	p, err := Probe(ffprobe)(ctx, in)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if p.ColorTransfer != "smpte2084" || !isUnknownColour(p.ColorPrimaries) || !isUnknownColour(p.ColorSpace) {
		t.Fatalf("the fixture must be tagged on the transfer ONLY, probed %q/%q/%q",
			p.ColorPrimaries, p.ColorTransfer, p.ColorSpace)
	}
	plan := MakePlan(p, PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60})
	if !plan.Tonemap || !plan.AssumePrimaries || !plan.AssumeMatrix {
		t.Fatalf("plan: tonemap=%v assume primaries/matrix=%v/%v, want all three",
			plan.Tonemap, plan.AssumePrimaries, plan.AssumeMatrix)
	}

	// The premise: without the assumed tags this file does not transcode.
	blind := plan
	blind.AssumePrimaries, blind.AssumeMatrix = false, false
	if err := Encode(ctx, ffmpeg, blind, in, filepath.Join(dir, "blind.mp4"), ffmpegOpts, nil); err == nil {
		t.Fatal("the untagged chain encoded this fixture: it no longer reproduces the defect")
	} else {
		t.Logf("premise holds — the untagged chain fails: %v", err)
	}

	out := filepath.Join(dir, "out.mp4")
	if err := Encode(ctx, ffmpeg, plan, in, out, ffmpegOpts, nil); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	fields := run(t, ffprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=pix_fmt,color_primaries,color_transfer,color_space", "-of", "csv=p=0", out)
	if got := strings.TrimSpace(strings.ReplaceAll(fields, "\r", "")); got != "yuv420p,bt709,bt709,bt709" {
		t.Fatalf("output colour = %q, want %q", got, "yuv420p,bt709,bt709,bt709")
	}
	if err := VerifyMP4(out); err != nil {
		t.Fatalf("VerifyMP4: %v", err)
	}
}

// TestFFmpegVFRIsPlannedAtTheCaptureRate (§20 erratum 93) proves the PREMISE
// of the frame-rate rule against real ffprobe output, not against a hand-made
// fixture: a clip whose action runs at 30 fps and whose still section runs at
// 10 reports r_frame_rate=30/1 with an average far below it, and `-fps_mode
// cfr` at the average would throw away most of the frames anyone wants.
func TestFFmpegVFRIsPlannedAtTheCaptureRate(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	// Two seconds of every frame, then eight seconds of every third — the
	// shape a phone screen recorder or an OBS replay buffer with dropped
	// frames writes. -fps_mode passthrough keeps the original timestamps,
	// which is what makes the result variable rate rather than a slow CFR.
	in := gen(t, ffmpeg, dir, "vfr.mp4", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30:duration=10",
		"-vf", "select='if(lt(t,2),1,eq(mod(n\\,3),0))'", "-fps_mode", "passthrough",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-an")

	p, err := Probe(ffprobe)(context.Background(), in)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if p.RFPS != (Rational{30, 1}) {
		t.Fatalf("r_frame_rate = %v, want 30/1 (the fixture is not the VFR shape any more)", p.RFPS)
	}
	if p.FPS.Float() >= 24 {
		t.Fatalf("avg_frame_rate = %v (%.2f): the fixture is not variable enough to be a test", p.FPS, p.FPS.Float())
	}
	plan := MakePlan(p, PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60})
	if plan.FPS != (Rational{30, 1}) {
		t.Fatalf("plan fps = %v, want 30/1: the action's frames are being decimated to the average", plan.FPS)
	}
	out := filepath.Join(dir, "out.mp4")
	if err := Encode(context.Background(), ffmpeg, plan, in, out, ffmpegOpts, nil); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	info, err := ProbeOutput(context.Background(), ffprobe, out)
	if err != nil {
		t.Fatalf("ProbeOutput: %v", err)
	}
	if info.FPS != (Rational{30, 1}) {
		t.Fatalf("output fps = %v, want 30/1", info.FPS)
	}
	// The CFR output holds a frame for every 1/30 s of the source's span, so
	// it carries MORE frames than the variable-rate source did — the fast
	// section is intact and the still section is filled, which is the whole
	// point of the choice.
	frames := strings.TrimSpace(run(t, ffprobe, "-v", "error", "-select_streams", "v:0",
		"-count_frames", "-show_entries", "stream=nb_read_frames", "-of", "csv=p=0", out))
	n, err := strconv.Atoi(strings.TrimSuffix(frames, ","))
	if err != nil {
		t.Fatalf("nb_read_frames = %q: %v", frames, err)
	}
	if n < 280 {
		t.Fatalf("output has %d frames for ~10 s at 30 fps: the timeline was decimated", n)
	}
}

var yavgRE = regexp.MustCompile(`lavfi\.signalstats\.YAVG=([0-9.]+)`)

// meanLuma is the average of the first frames' Y planes, via signalstats.
func meanLuma(t *testing.T, ffmpeg, path string) float64 {
	t.Helper()
	cmd := exec.Command(ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "info", "-i", path,
		"-vf", "signalstats,metadata=print:key=lavfi.signalstats.YAVG", "-frames:v", "10", "-f", "null", "-")
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("signalstats: %v\n%s", err, raw)
	}
	ms := yavgRE.FindAllStringSubmatch(string(raw), -1)
	if len(ms) == 0 {
		t.Fatalf("no YAVG in ffmpeg output:\n%s", raw)
	}
	sum := 0.0
	for _, m := range ms {
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			t.Fatalf("YAVG %q: %v", m[1], err)
		}
		sum += v
	}
	return sum / float64(len(ms))
}

// TestFFmpegCancelReaps: cancelling mid-encode returns ctx.Err() quickly and
// leaves no ffmpeg behind (Wait returned, so the process is reaped).
func TestFFmpegCancelReaps(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	in := gen(t, ffmpeg, dir, "long.mp4", join([]string{"-f", "lavfi", "-i", "testsrc2=duration=10:size=1920x1080:rate=60"}, v)...)
	p, err := Probe(ffprobe)(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	plan := MakePlan(p, PlanOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 1)
	go func() {
		<-started
		cancel()
	}()
	start := time.Now()
	err = Encode(ctx, ffmpeg, plan, in, filepath.Join(dir, "out.mp4"), EncodeOptions{Preset: "slow", CRF: 23}, func(float64, bool) {
		select {
		case started <- struct{}{}:
		default:
		}
	})
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if el := time.Since(start); el > 15*time.Second {
		t.Fatalf("cancel took %s", el)
	}
}

// TestFFmpegHardTimeout: the hard cap is a permanent encode_timeout.
func TestFFmpegHardTimeout(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	in := gen(t, ffmpeg, dir, "long.mp4", join([]string{"-f", "lavfi", "-i", "testsrc2=duration=10:size=1920x1080:rate=60"}, v)...)
	p, err := Probe(ffprobe)(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	plan := MakePlan(p, PlanOptions{})
	err = Encode(context.Background(), ffmpeg, plan, in, filepath.Join(dir, "out.mp4"), EncodeOptions{Preset: "slow", CRF: 23, HardTimeout: 700 * time.Millisecond}, nil)
	if !IsPermanent(err) || !strings.Contains(err.Error(), "encode_timeout") {
		t.Fatalf("err = %v", err)
	}
}

// TestFFmpegNoDurationGapFillIsCapped (§20 erratum 46): a Matroska muxed to
// a NON-SEEKABLE output carries no Duration element — the shape a browser
// MediaRecorder, or ffmpeg/OBS piping, writes — so ffprobe reports neither
// format.duration nor a stream duration and the plan cannot be sized. The
// frame timestamps still can be anything: two frames an hour apart make
// `-r fps -fps_mode cfr` duplicate frames to fill the gap, and the output
// grows with the timestamps, not with the 1,377 uploaded bytes (measured on
// 7.1.1: 215,942 frames, 3599 s, 6.78 MB, in 28 s — with nothing but the 6 h
// hard timeout above it). The output budget is the bound.
func TestFFmpegNoDurationGapFillIsCapped(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "gap.mkv")
	f, err := os.Create(in)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=red:s=640x360:r=30",
		"-frames:v", "2", "-vf", "setpts=N*107970", "-c:v", "libx264", "-f", "matroska", "pipe:1")
	cmd.Stdout = f
	err = cmd.Run()
	_ = f.Close()
	if err != nil {
		t.Fatalf("mux the gap clip to a pipe: %v", err)
	}
	p, err := Probe(ffprobe)(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if p.HasDuration {
		t.Fatalf("a pipe-muxed mkv is expected to carry no duration; got %d us", p.DurationUS)
	}
	out := filepath.Join(dir, "out.mp4")
	o := ffmpegOpts
	o.MaxOutputBytes = 200 << 10
	start := time.Now()
	err = Encode(context.Background(), ffmpeg, MakePlan(p, PlanOptions{}), in, out, o, nil)
	if !IsPermanent(err) || !strings.Contains(err.Error(), "output_too_large") {
		t.Fatalf("err = %v", err)
	}
	if el := time.Since(start); el > 60*time.Second {
		t.Fatalf("the capped run took %s", el)
	}
	if st, err := os.Stat(out); err == nil && st.Size() > 4<<20 {
		t.Fatalf("the killed run still wrote %d bytes", st.Size())
	}
}

// TestFFmpegBadInput: garbage in → ffprobe exit ≠ 0 → permanent.
func TestFFmpegBadInput(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.mp4")
	if err := os.WriteFile(junk, []byte("this is not a video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Probe(ffprobe)(context.Background(), junk); !IsPermanent(err) {
		t.Fatalf("Probe(junk) = %v", err)
	}
	plan := basePlan(AudioNone, nil)
	err := Encode(context.Background(), ffmpeg, plan, junk, filepath.Join(dir, "out.mp4"), ffmpegOpts, nil)
	if !IsPermanent(err) || !strings.Contains(err.Error(), "ffmpeg exited") {
		t.Fatalf("Encode(junk) = %v", err)
	}
	if _, err := ProbeOutput(context.Background(), ffprobe, junk); !IsPermanent(err) || !strings.Contains(err.Error(), "bad_output") {
		t.Fatalf("ProbeOutput(junk) = %v", err)
	}
	if _, err := Poster(context.Background(), ffmpeg, junk, filepath.Join(dir, "p.jpg"), 0, false, 0, PosterBox{}, PosterLabel{}); !IsPermanent(err) {
		t.Fatalf("Poster(junk) = %v", err)
	}
}

// TestFFmpegNoVideoInput: a file with no picture is SOUND since §20 erratum
// 328 — an audio clip, never a video with VideoIndex set.
func TestFFmpegNoVideoInput(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	audio := filepath.Join(dir, "audio.m4a")
	run(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", sine("440"), "-c:a", "aac", audio)
	p, err := Probe(ffprobe)(context.Background(), audio)
	if err != nil || p.Kind != KindAudio || p.VideoIndex != -1 || p.AudioCodecs[0] != "aac" {
		t.Fatalf("Probe(audio only) = %+v, %v; want an audio result", p, err)
	}
}

// TestFFmpegStillImageRejected: a picture (png_pipe/jpeg_pipe) is not a
// video even though it carries a video stream; an animated GIF is.
func TestFFmpegStillImageRejected(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	for _, file := range []string{"pic.png", "pic.jpg"} {
		out := gen(t, ffmpeg, dir, file, "-f", "lavfi", "-i", "color=c=red:size=160x90:duration=0.1", "-frames:v", "1", "-update", "1")
		_, err := Probe(ffprobe)(context.Background(), out)
		if !IsPermanent(err) || !strings.Contains(err.Error(), "no_video_stream: still image") {
			t.Fatalf("Probe(%s) = %v, want permanent still image", file, err)
		}
	}
	gif := gen(t, ffmpeg, dir, "anim.gif", "-f", "lavfi", "-i", "testsrc2=duration=1:size=160x90:rate=10")
	p, err := Probe(ffprobe)(context.Background(), gif)
	if err != nil || p.DisplayW != 160 || p.DisplayH != 90 {
		t.Fatalf("Probe(gif) = %+v, %v; an animated gif is a video", p, err)
	}
}
