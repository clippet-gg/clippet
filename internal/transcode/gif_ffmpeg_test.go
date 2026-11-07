//go:build ffmpeg

package transcode

// The real thing (§16.2, §17 #6): the two palette passes run against a clip
// this test generates, and the assertions are the ones a unit test cannot
// make — that ffmpeg produces a file the GIF format recognises, that it
// animates, that it loops forever, and that the byte cap actually kills a
// run rather than merely being computed.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Threads is set for the reason the worker sets it (§20 erratum 215), and so
// that a REAL ffmpeg proves both passes accept the pair: an argv assertion
// cannot tell a valid option from a typo.
func gifOpts() GIFOptions {
	return GIFOptions{StallTimeout: time.Minute, HardTimeout: GIFHardTimeout,
		MaxOutputBytes: GIFMaxBytes, Threads: 2}
}

// renditionClip renders a 6s 640x360 clip in the shape the pipeline produces,
// which is what a gif job is always pointed at.
func renditionClip(t *testing.T, ffmpeg, dir string) string {
	t.Helper()
	return gen(t, ffmpeg, dir, "rendition.mp4", join(
		[]string{"-f", "lavfi", "-i", src("640x360", "30"), "-f", "lavfi", "-i", sine("440")},
		v, a, []string{"-shortest"},
	)...)
}

func TestMakeGIFProducesABoundedLoopingPreview(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	in := renditionClip(t, ffmpeg, dir)
	palette := filepath.Join(dir, "palette.png")
	out := filepath.Join(dir, "preview.gif")

	p := PlanGIF(6000, 640, 0)
	if err := MakeGIF(context.Background(), ffmpeg, p, in, palette, out, gifOpts(), nil); err != nil {
		t.Fatalf("MakeGIF: %v", err)
	}

	info, err := ProbeGIF(context.Background(), ffprobe, out)
	if err != nil {
		t.Fatalf("ProbeGIF: %v", err)
	}
	if err := CheckGIF(info, p); err != nil {
		t.Fatalf("our own preview failed the verify: %v", err)
	}
	if info.Width != 480 {
		t.Errorf("width = %d, want the planned 480", info.Width)
	}
	if info.Height != 270 { // 640x360 scaled to 480 wide keeps 16:9
		t.Errorf("height = %d, want 270 (the aspect ratio is kept)", info.Height)
	}
	// 6 s at 12 fps: exactly the frames the plan schedules, give or take the
	// one a muxer may round.
	if info.Frames < p.Frames()-2 || info.Frames > p.Frames()+2 {
		t.Errorf("frames = %d, want ≈%d", info.Frames, p.Frames())
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(raw)) > GIFMaxBytes {
		t.Errorf("the preview is %d bytes, past the %d cap", len(raw), GIFMaxBytes)
	}
	if !bytes.HasPrefix(raw, []byte("GIF89a")) {
		t.Errorf("the file does not start with the GIF89a signature: % x", raw[:min(6, len(raw))])
	}
	// `-loop 0` writes the NETSCAPE2.0 application extension, which is the
	// only thing in the format that says "loop forever". A preview that plays
	// once is not what a share page shows.
	if !bytes.Contains(raw, []byte("NETSCAPE2.0")) {
		t.Error("the preview carries no NETSCAPE loop block; it would play once")
	}
	// The palette really was built (pass 1 ran) and is a PNG, not the file
	// the second pass wrote.
	pal, err := os.ReadFile(palette)
	if err != nil || !bytes.HasPrefix(pal, []byte{0x89, 'P', 'N', 'G'}) {
		t.Errorf("the palette is not a PNG (%v)", err)
	}
	t.Logf("preview: %dx%d, %d frames, %d bytes", info.Width, info.Height, info.Frames, len(raw))
}

// firstFrameLuma measures the mean luma of a file's FIRST frame on the
// 0-255 scale signalstats prints — the same number E2E step 9 uses on a
// poster, applied here to the frame a preview OPENS on, which is the one an
// <img> shows before anything animates.
func firstFrameLuma(t *testing.T, ffmpeg, path string) float64 {
	t.Helper()
	raw, err := exec.Command(ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "info", "-i", path,
		"-vf", "signalstats,metadata=print:key=lavfi.signalstats.YAVG", "-frames:v", "1", "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("signalstats on %s: %v\n%s", filepath.Base(path), err, raw)
	}
	m := yavgRE.FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatalf("no YAVG for %s:\n%s", filepath.Base(path), raw)
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestMakeGIFStartsAtThePosterFrame (§20 erratum 92(c), amended). The
// preview's window is supposed to begin where the poster does — that was the
// whole of (c) — but the poster's timestamp is CHOSEN by a thumbnail pass and
// what the gif job re-derived was the blind SEEK the pass exists to overrule.
// On the clip the poster fix was written for (3 s of black, then picture) the
// card image was fine and the preview still opened on a black rectangle for a
// second and a half.
//
// Both halves are asserted on the same fixture: the seek-planned preview
// opens black (the defect, and the proof the fixture reproduces it) and the
// poster-frame-planned one opens on the picture.
func TestMakeGIFStartsAtThePosterFrame(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	in := blackIntroClip(t, ffmpeg, dir)

	info, err := ProbeOutput(context.Background(), ffprobe, in)
	if err != nil {
		t.Fatalf("ProbeOutput: %v", err)
	}
	durMS := int64(info.DurationMS)
	seek := PosterSeek(durMS*1000, true, 0)
	picked := PosterFrameSeconds(context.Background(), ffmpeg, in, durMS*1000, true, 0)
	if picked <= 3 || picked <= seek {
		t.Fatalf("the poster frame is at %.3fs (seek %.3fs): the fixture's intro is not being skipped", picked, seek)
	}

	render := func(name string, p GIFPlan) string {
		out := filepath.Join(dir, name)
		if err := MakeGIF(context.Background(), ffmpeg, p, in, filepath.Join(dir, name+".png"), out, gifOpts(), nil); err != nil {
			t.Fatalf("MakeGIF(%s): %v", name, err)
		}
		return out
	}
	blindPlan, pickedPlan := PlanGIF(durMS, info.Width, seek), PlanGIF(durMS, info.Width, picked)
	if blindPlan.StartSec >= pickedPlan.StartSec {
		t.Fatalf("the two plans start at %.3fs and %.3fs: the fix changes nothing here", blindPlan.StartSec, pickedPlan.StartSec)
	}
	// The premise, measured on the SOURCE rather than on a preview: the frame
	// the old window opened on is black by the poster's own test. (A GIF is
	// palettized and dithered, so a black frame reads ~16 rather than ~0
	// through signalstats — which is exactly why the premise is not asserted
	// on the preview.)
	at := filepath.Join(dir, "at-seek.jpg")
	if _, _, err := runCapture(context.Background(), ffmpeg, PosterArgs(in, at, seek, PosterBox{})...); err != nil {
		t.Fatalf("the frame at the seek: %v", err)
	}
	if f, err := jpegStats(at); err != nil || !f.nearBlack() {
		t.Fatalf("the frame at %.3fs is not black (%+v, %v): the fixture no longer reproduces the defect", seek, f, err)
	}

	blind := firstFrameLuma(t, ffmpeg, render("blind.gif", blindPlan))
	good := firstFrameLuma(t, ffmpeg, render("picked.gif", pickedPlan))
	t.Logf("first-frame luma: seek-planned %.2f (start %.3fs), poster-planned %.2f (start %.3fs)",
		blind, blindPlan.StartSec, good, pickedPlan.StartSec)
	// The fix: planned at the poster's frame, the preview opens on the
	// picture. The threshold is E2E step 9's rule for a poster, applied to a
	// GIF — and the ratio is what keeps dither noise from clearing it.
	if good <= posterMinMeanLuma {
		t.Fatalf("the preview opens at luma %.2f, want a lit frame (> %.0f)", good, posterMinMeanLuma)
	}
	if good < 4*blind {
		t.Fatalf("the poster-planned preview opens at %.2f against the seek-planned %.2f: not the picture, just noise", good, blind)
	}
}

// TestMakeGIFCapsTheOutput: the byte cap is enforced by killing the run, not
// by measuring afterwards, so a preview that would run away never finishes
// writing. A 4 KiB cap on a real preview is unreachable by construction.
func TestMakeGIFCapsTheOutput(t *testing.T) {
	ffmpeg, _ := binaries(t)
	dir := t.TempDir()
	in := renditionClip(t, ffmpeg, dir)
	o := gifOpts()
	o.MaxOutputBytes = 4 << 10

	err := MakeGIF(context.Background(), ffmpeg, PlanGIF(6000, 640, 0), in,
		filepath.Join(dir, "p.png"), filepath.Join(dir, "big.gif"), o, nil)
	if !IsPermanent(err) {
		t.Fatalf("MakeGIF = %v, want a permanent failure", err)
	}
	if got := err.Error(); !bytes.Contains([]byte(got), []byte("output_too_large")) ||
		!bytes.Contains([]byte(got), []byte("the gif wrote past")) {
		t.Errorf("error = %q, want the gif's own output_too_large", got)
	}
}

// TestMakeGIFHonoursCancellation: a cancelled context carries the
// cancellation out, and is NEVER dressed up as a permanent failure — the
// worker classifies on exactly that difference (a cancelled run is released
// with its counters untouched; a permanent one is failed).
func TestMakeGIFHonoursCancellation(t *testing.T) {
	ffmpeg, _ := binaries(t)
	dir := t.TempDir()
	in := renditionClip(t, ffmpeg, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := MakeGIF(ctx, ffmpeg, PlanGIF(6000, 640, 0), in, filepath.Join(dir, "p.png"), filepath.Join(dir, "o.gif"), gifOpts(), nil)
	if !errors.Is(err, context.Canceled) || IsPermanent(err) {
		t.Fatalf("MakeGIF on a cancelled ctx = %v, want a plain cancellation", err)
	}
}

// TestProbeGIFRejectsWhatIsNotAGIF: the verify's first question, asked of a
// file that is not one.
func TestProbeGIFRejectsWhatIsNotAGIF(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	mp4 := renditionClip(t, ffmpeg, dir)

	info, err := ProbeGIF(context.Background(), ffprobe, mp4)
	if err != nil {
		t.Fatalf("ProbeGIF on an mp4: %v", err)
	}
	if err := CheckGIF(info, PlanGIF(6000, 640, 0)); !IsPermanent(err) {
		t.Fatalf("CheckGIF on an h264 stream = %v, want a permanent bad_output", err)
	}
	// A file that is not media at all: ffprobe exits non-zero → permanent.
	junk := filepath.Join(dir, "junk.gif")
	if err := os.WriteFile(junk, []byte("not a gif at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ProbeGIF(context.Background(), ffprobe, junk); !IsPermanent(err) {
		t.Fatalf("ProbeGIF on junk = %v, want a permanent bad_output", err)
	}
}
