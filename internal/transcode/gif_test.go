package transcode

import (
	"strings"
	"testing"
)

// The GIF export's pure half (§17 #6): what the plan bounds, what the argv
// says, and what the verify accepts.

func TestPlanGIFBounds(t *testing.T) {
	for _, tc := range []struct {
		name       string
		durationMS int64
		width      int
		posterSec  float64
		want       GIFPlan
	}{
		{"a long wide clip is bounded on both axes", 45_000, 1280, 0, GIFPlan{Seconds: 6, FPS: 12, Width: 480}},
		{"a short clip yields its whole self", 2_500, 1280, 0, GIFPlan{Seconds: 2.5, FPS: 12, Width: 480}},
		{"a narrow rendition is never upscaled", 20_000, 320, 0, GIFPlan{Seconds: 6, FPS: 12, Width: 320}},
		{"exactly the bound", 6_000, 480, 0, GIFPlan{Seconds: 6, FPS: 12, Width: 480}},
		{"no measurements at all falls back to the bounds", 0, 0, 0, GIFPlan{Seconds: 6, FPS: 12, Width: 480}},
		{"an odd width is made even for scale=W:-2", 6_000, 321, 0, GIFPlan{Seconds: 6, FPS: 12, Width: 320}},
		{"an absurd row cannot produce a zero width", 6_000, 1, 0, GIFPlan{Seconds: 6, FPS: 12, Width: 2}},
		// The window starts where the poster was taken (§20 erratum 92)...
		{"the window starts at the poster", 45_000, 1280, 6.75, GIFPlan{StartSec: 6.75, Seconds: 6, FPS: 12, Width: 480}},
		// ...but never so late that it would run off the end...
		{"the window never runs past the end", 10_000, 1280, 9, GIFPlan{StartSec: 4, Seconds: 6, FPS: 12, Width: 480}},
		{"a clip shorter than the window starts at 0", 4_000, 1280, 0.6, GIFPlan{Seconds: 4, FPS: 12, Width: 480}},
		{"exactly the bound has nowhere to move", 6_000, 480, 0.9, GIFPlan{Seconds: 6, FPS: 12, Width: 480}},
		// ...and an absent or nonsensical timestamp is the old behaviour.
		{"no poster timestamp starts at 0", 45_000, 1280, 0, GIFPlan{Seconds: 6, FPS: 12, Width: 480}},
		{"a negative timestamp starts at 0", 45_000, 1280, -3, GIFPlan{Seconds: 6, FPS: 12, Width: 480}},
		{"an unmeasured duration starts at 0", 0, 1280, 9, GIFPlan{Seconds: 6, FPS: 12, Width: 480}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PlanGIF(tc.durationMS, tc.width, tc.posterSec); got != tc.want {
				t.Errorf("PlanGIF(%d, %d, %v) = %+v, want %+v", tc.durationMS, tc.width, tc.posterSec, got, tc.want)
			}
		})
	}
}

// TestPlanGIFStartsWhereThePosterDoes: the two rules are one rule. Whatever
// timestamp the poster stage would seek to, the preview begins there — so a
// clip whose first seconds are a loading screen no longer spends its whole
// preview on one.
func TestPlanGIFStartsWhereThePosterDoes(t *testing.T) {
	const durationMS = 45_000
	poster := PosterSeek(durationMS*1000, true, 0) // 6.75 s
	p := PlanGIF(durationMS, 1280, poster)
	if p.StartSec != poster {
		t.Fatalf("StartSec = %v, want the poster's %v", p.StartSec, poster)
	}
	if p.StartSec+p.Seconds > float64(durationMS)/1000 {
		t.Fatalf("the window %v..%v runs past the %v s rendition", p.StartSec, p.StartSec+p.Seconds, float64(durationMS)/1000)
	}
}

func TestGIFPlanFrames(t *testing.T) {
	if n := PlanGIF(6000, 1280, 0).Frames(); n != 72 {
		t.Errorf("frames = %d, want 6s × 12fps", n)
	}
	// A window too short to hold a frame still claims one: the verify's
	// lower bound must never be zero.
	if n := (GIFPlan{Seconds: 0.01, FPS: 12}).Frames(); n != 1 {
		t.Errorf("frames = %d, want 1", n)
	}
}

// TestGIFArgsAreTheTwoPassPaletteChain pins the argv of both passes. The
// chain IS the feature: fps → scale → palettegen(diff), then the same head
// mapped through paletteuse with ordered dithering, and `-t` in FRONT of
// `-i` so a two-hour rendition costs what a six-second one does.
func TestGIFArgsAreTheTwoPassPaletteChain(t *testing.T) {
	p := PlanGIF(20_000, 1280, 3)
	pal := strings.Join(p.PaletteArgs("in.mp4", "pal.png", 3), " ")
	for _, want := range []string{
		"-ss 3.000000 -t 6.000000 -protocol_whitelist file -i in.mp4",
		"-vf fps=12,scale=480:-2:flags=lanczos,palettegen=stats_mode=diff",
		"-frames:v 1 -update 1 -f image2 pal.png",
		"-progress pipe:1",
		"-an -sn -dn",
		// Both passes are filter work and carry the CPU quota (§20 erratum
		// 215), through the same encodeHeader every encode uses.
		"-filter_threads 3 -filter_complex_threads 3",
	} {
		if !strings.Contains(pal, want) {
			t.Errorf("palette argv lacks %q:\n%s", want, pal)
		}
	}

	out := strings.Join(p.EncodeArgs("in.mp4", "pal.png", "out.gif", 3), " ")
	for _, want := range []string{
		"-ss 3.000000 -t 6.000000 -protocol_whitelist file -i in.mp4 -protocol_whitelist file -i pal.png",
		"-lavfi [0:v]fps=12,scale=480:-2:flags=lanczos[x];[x][1:v]paletteuse=dither=bayer:bayer_scale=5",
		"-loop 0 -f gif out.gif",
		"-map_metadata -1",
		"-filter_threads 3 -filter_complex_threads 3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("gif argv lacks %q:\n%s", want, out)
		}
	}
	// No audio, no subtitles, no branding of any kind ever reaches the argv:
	// a watermark is an anti-feature, and a GIF has no sound to lose.
	for _, forbidden := range []string{"drawtext", "overlay", "-c:a", "watermark"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("gif argv contains %q", forbidden)
		}
	}
	// The window is an INPUT option in both passes: `-ss` then `-t`, both in
	// front of the `-i` they govern. Behind `-i` they would decode the whole
	// rendition and discard it, and a palette built from one set of frames
	// and used on another is not the palette those frames need — so the two
	// passes must also spell the SAME pair.
	for _, argv := range []struct{ what, args string }{{"palette", pal}, {"encode", out}} {
		ss, tt, i := strings.Index(argv.args, "-ss "), strings.Index(argv.args, "-t "), strings.Index(argv.args, "-i ")
		if ss < 0 || tt < 0 || i < 0 || !(ss < tt && tt < i) {
			t.Errorf("%s argv: want -ss before -t before -i, got positions %d/%d/%d:\n%s", argv.what, ss, tt, i, argv.args)
		}
	}
	if !strings.Contains(pal, "-ss 3.000000 -t 6.000000") || !strings.Contains(out, "-ss 3.000000 -t 6.000000") {
		t.Errorf("the two passes must read the identical window:\npalette: %s\nencode:  %s", pal, out)
	}
}

// TestGIFArgsFollowTheNarrowPlan: a narrow rendition scales to its own width,
// so the preview is never upscaled and never wider than the source.
func TestGIFArgsFollowTheNarrowPlan(t *testing.T) {
	args := strings.Join(PlanGIF(3_000, 320, 0.45).EncodeArgs("in.mp4", "p.png", "o.gif", 0), " ")
	if !strings.Contains(args, "scale=320:-2") || !strings.Contains(args, "-t 3.000000") {
		t.Errorf("argv does not follow the plan:\n%s", args)
	}
}

func TestParseGIFProbe(t *testing.T) {
	info, err := ParseGIFProbe([]byte(`{"streams":[{"codec_name":"gif","width":480,"height":270,"nb_read_frames":"72"}]}`))
	if err != nil {
		t.Fatalf("ParseGIFProbe: %v", err)
	}
	if info.Codec != "gif" || info.Width != 480 || info.Height != 270 || info.Frames != 72 {
		t.Fatalf("info = %+v", info)
	}
	// A document with no video stream at all is our own output being wrong.
	if _, err := ParseGIFProbe([]byte(`{"streams":[]}`)); !IsPermanent(err) {
		t.Errorf("empty streams = %v, want a permanent bad_output", err)
	}
	if _, err := ParseGIFProbe([]byte("not json")); !IsPermanent(err) {
		t.Errorf("unparsable = %v, want a permanent bad_output", err)
	}
	// ffprobe prints N/A for a count it could not make: no frames, not a crash.
	info, err = ParseGIFProbe([]byte(`{"streams":[{"codec_name":"gif","width":8,"height":8,"nb_read_frames":"N/A"}]}`))
	if err != nil || info.Frames != 0 {
		t.Fatalf("N/A frames = %+v, %v", info, err)
	}
}

func TestCheckGIF(t *testing.T) {
	p := PlanGIF(6000, 1280, 0)
	good := &GIFInfo{Codec: "gif", Width: 480, Height: 270, Frames: 72}
	if err := CheckGIF(good, p); err != nil {
		t.Fatalf("a good preview was rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		info *GIFInfo
	}{
		{"no probe result", nil},
		{"not a gif", &GIFInfo{Codec: "png", Width: 480, Height: 270, Frames: 72}},
		{"no dimensions", &GIFInfo{Codec: "gif", Width: 0, Height: 270, Frames: 72}},
		{"wider than planned", &GIFInfo{Codec: "gif", Width: 900, Height: 270, Frames: 72}},
		{"no frames", &GIFInfo{Codec: "gif", Width: 480, Height: 270, Frames: 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckGIF(tc.info, p)
			if !IsPermanent(err) {
				t.Fatalf("CheckGIF = %v, want a permanent bad_output", err)
			}
			if !strings.Contains(err.Error(), "bad_output") {
				t.Errorf("error %q does not name bad_output", err)
			}
		})
	}
}

// TestGIFProgressPlanDrivesTheInstrumentation: the synthetic plan handed to
// runInstrumented must describe the OUTPUT (window and rate), or the stall
// watchdog would be disarmed and the progress fraction meaningless.
func TestGIFProgressPlanDrivesTheInstrumentation(t *testing.T) {
	p := PlanGIF(4_000, 1280, 0).progressPlan()
	if !p.HasDuration || p.DurationUS != 4_000_000 {
		t.Fatalf("progress plan duration = %v/%d", p.HasDuration, p.DurationUS)
	}
	if p.FPS.Num != GIFFPS || p.FPS.Den != 1 {
		t.Fatalf("progress plan fps = %s", p.FPS)
	}
	frac, indeterminate := ProgressFrac(p, 2_000_000, true, 0, false)
	if indeterminate || frac < 0.49 || frac > 0.51 {
		t.Errorf("half way through the window = %v (indeterminate %v)", frac, indeterminate)
	}
}
