//go:build ffmpeg

package transcode

// §20 erratum 125 (ROADMAP #28): an interlaced source encoded as-is comes out
// probing `field_order=progressive` with the comb still in the pixels — the
// metadata denies what the picture is, and no player can correct that. This
// file proves the fix on real ffmpeg, and proves the BUG at the same time:
// the same source through the same chain without the deinterlacer is the
// negative control, because "the output probes progressive" is true either
// way and is therefore not, on its own, evidence of anything.

import (
	"context"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// interlacedFixture renders a 720×576 TFF clip: testsrc2 at 50 fps woven into
// 25 interlaced frames (`tinterlace=interleave_top`), the field order stated
// (`fieldorder=tff`), and x264 told to code it as fields (`+ilme+ildct -top
// 1`) so the container declares `field_order=tt`. That combination is what a
// real SD capture card writes and what the probe has to notice.
func interlacedFixture(t *testing.T, ffmpeg, dir string) string {
	t.Helper()
	return gen(t, ffmpeg, dir, "interlaced-576i.mp4",
		"-f", "lavfi", "-i", "testsrc2=duration="+dur+":size=720x576:rate=50",
		"-vf", "tinterlace=interleave_top,fieldorder=tff",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-flags", "+ilme+ildct", "-top", "1")
}

var idetRE = regexp.MustCompile(`Multi frame detection: TFF:\s*(\d+) BFF:\s*(\d+) Progressive:\s*(\d+) Undetermined:\s*(\d+)`)

// idet counts what ffmpeg's own interlace detector sees in the first n frames
// of a file: (interlaced, progressive). It is the measurement that matters,
// because `field_order` is metadata a re-encode rewrites and the comb is not.
func idet(t *testing.T, ffmpeg, path string, n int) (interlaced, progressive int) {
	t.Helper()
	cmd := exec.Command(ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "info", "-i", path,
		"-vf", "idet", "-frames:v", strconv.Itoa(n), "-f", "null", "-")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("idet %s: %v\n%s", filepath.Base(path), err, out)
	}
	m := idetRE.FindAllStringSubmatch(string(out), -1)
	if len(m) == 0 {
		t.Fatalf("idet printed no multi-frame verdict for %s:\n%s", filepath.Base(path), out)
	}
	last := m[len(m)-1]
	tff, _ := strconv.Atoi(last[1])
	bff, _ := strconv.Atoi(last[2])
	prog, _ := strconv.Atoi(last[3])
	return tff + bff, prog
}

// encodeWith runs one encode of in through plan's own argv, with the plan's
// Deinterlace forced either way, so the two chains differ in exactly one
// filter.
func encodeWith(t *testing.T, ffmpeg, in, out string, p Plan, deinterlace bool) {
	t.Helper()
	p.Deinterlace = deinterlace
	if err := Encode(context.Background(), ffmpeg, p, in, out, ffmpegOpts, nil); err != nil {
		t.Fatalf("Encode(deinterlace=%v): %v", deinterlace, err)
	}
}

// TestInterlacedSourceIsDeinterlaced is the whole of erratum 125 on one
// fixture: the probe sees the field order, the plan says deinterlace, and the
// output is measurably progressive — while the same encode WITHOUT the filter
// is measurably not, though both files claim `field_order=progressive`.
func TestInterlacedSourceIsDeinterlaced(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	in := interlacedFixture(t, ffmpeg, dir)
	ctx := context.Background()

	p, err := Probe(ffprobe)(ctx, in)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if p.FieldOrder != "tt" {
		t.Fatalf("the fixture probes field_order=%q, want tt (it is not interlaced, so this test proves nothing)", p.FieldOrder)
	}
	plan := MakePlan(p, PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60})
	if !plan.Deinterlace {
		t.Fatal("MakePlan did not set Deinterlace for a tt source")
	}
	if plan.W != 720 || plan.H != 576 {
		t.Fatalf("plan = %dx%d, want the source's 720x576 (it fits the box)", plan.W, plan.H)
	}

	// The source itself: interlaced on every frame it was asked about.
	if il, prog := idet(t, ffmpeg, in, 100); il < 90 {
		t.Fatalf("the fixture is not actually combed (idet: %d interlaced, %d progressive)", il, prog)
	}

	good := filepath.Join(dir, "deinterlaced.mp4")
	encodeWith(t, ffmpeg, in, good, plan, true)
	bad := filepath.Join(dir, "as-is.mp4")
	encodeWith(t, ffmpeg, in, bad, plan, false)

	// Both files SAY progressive. That is the trap this erratum exists for.
	for _, f := range []string{good, bad} {
		order := strings.TrimSpace(strings.ReplaceAll(run(t, ffprobe, "-v", "error", "-select_streams", "v:0",
			"-show_entries", "stream=field_order", "-of", "csv=p=0", f), "\r", ""))
		if order != "progressive" {
			t.Fatalf("%s declares field_order=%q; the metadata half of the trap has changed", filepath.Base(f), order)
		}
	}
	// The pixels do not.
	if il, prog := idet(t, ffmpeg, bad, 100); il < 90 {
		t.Fatalf("the negative control is not combed (idet: %d interlaced, %d progressive) — "+
			"this fixture can no longer prove the fix does anything", il, prog)
	}
	il, prog := idet(t, ffmpeg, good, 100)
	if il > 10 || prog < 90 {
		t.Fatalf("the deinterlaced output is still combed: idet says %d interlaced, %d progressive", il, prog)
	}

	// And it is still a rendition: one h264 stream, the planned geometry,
	// faststart intact.
	info, err := ProbeOutput(ctx, ffprobe, good)
	if err != nil {
		t.Fatalf("ProbeOutput: %v", err)
	}
	if err := CheckOutput(info, PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60}); err != nil {
		t.Fatalf("CheckOutput: %v (%+v)", err, *info)
	}
	if info.Width != plan.W || info.Height != plan.H {
		t.Fatalf("output %dx%d, plan %dx%d", info.Width, info.Height, plan.W, plan.H)
	}
	if err := VerifyMP4(good); err != nil {
		t.Fatalf("VerifyMP4: %v", err)
	}
}

// TestProgressiveSourceIsUntouchedByTheDeinterlacer: the other half of the
// promise. `deint=interlaced` processes only FLAGGED frames, so a progressive
// source renders identically whether the filter is in the chain or not — the
// goldens of every other clip in the matrix are safe.
func TestProgressiveSourceIsUntouchedByTheDeinterlacer(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	in := gen(t, ffmpeg, dir, "progressive.mp4",
		"-f", "lavfi", "-i", src("1280x720", "30"), "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p")
	p, err := Probe(ffprobe)(context.Background(), in)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if IsInterlacedFieldOrder(p.FieldOrder) {
		t.Fatalf("a testsrc2 clip probes field_order=%q", p.FieldOrder)
	}
	plan := MakePlan(p, PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60})
	if plan.Deinterlace {
		t.Fatal("MakePlan wants to deinterlace a progressive source")
	}
	// Forced on anyway: the frames are not flagged, so nothing is filtered.
	off := filepath.Join(dir, "off.mp4")
	on := filepath.Join(dir, "on.mp4")
	encodeWith(t, ffmpeg, in, off, plan, false)
	encodeWith(t, ffmpeg, in, on, plan, true)
	if s := ssim(t, ffmpeg, on, off, "null"); s < 0.99 {
		t.Fatalf("yadif=deint=interlaced changed a progressive picture: SSIM %.4f", s)
	}
}
