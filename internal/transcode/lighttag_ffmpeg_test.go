//go:build ffmpeg

package transcode

// The LIGHT tag and the two-output encode on real ffmpeg (§20 errata 214, 217).
//
// Both claims here are about PIXELS and FILES, not about argv strings: the argv
// is pinned by the plain tests, and what those cannot say is whether ffmpeg
// draws anything, whether it draws it in the corner asked for, and whether one
// process really writes two complete mp4s.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// tagFont resolves the font this image ships, or skips. An image without one has
// no tag by design (FindTagFont → serve.go clears the text), so a build without
// a font is a different deployment and not a broken one.
func tagFont(t *testing.T, ffmpeg string) string {
	t.Helper()
	if !strings.Contains(run(t, ffmpeg, "-hide_banner", "-filters"), "drawtext") {
		t.Skip("this ffmpeg has no drawtext filter; the tag is off on such a build")
	}
	font := FindTagFont()
	if font == "" {
		t.Skip("no font in this image (" + strings.Join(tagFontCandidates, ", ") + "); the tag is off on such a build")
	}
	return font
}

// cornerLuma is the mean luminance of one corner of one extracted frame, as
// ffmpeg's own signalstats reports it. It is how the tag is PROVEN: a
// semi-transparent white line in the bottom-right corner raises the mean there
// and leaves every other corner alone.
func cornerLuma(t *testing.T, ffmpeg, file, crop string) float64 {
	t.Helper()
	out := run(t, ffmpeg, "-hide_banner", "-nostdin", "-y", "-i", file,
		"-vf", "crop="+crop+",signalstats,metadata=print:key=lavfi.signalstats.YAVG",
		"-frames:v", "1", "-f", "null", "-")
	for _, line := range strings.Split(out, "\n") {
		if _, v, ok := strings.Cut(line, "lavfi.signalstats.YAVG="); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				t.Fatalf("YAVG %q: %v", v, err)
			}
			return f
		}
	}
	t.Fatalf("no YAVG in signalstats output for %s crop=%s:\n%s", file, crop, out)
	return 0
}

// TestLightTagIsBurnedIntoTheCorner: the same source encoded twice, once with
// the tag and once without, and the DIFFERENCE measured per corner. Nothing else
// proves the line is there, in the corner meant, and nowhere else.
func TestLightTagIsBurnedIntoTheCorner(t *testing.T) {
	ffmpeg, _ := binaries(t)
	font := tagFont(t, ffmpeg)
	dir := t.TempDir()
	ctx := context.Background()

	// A DARK, flat source, so a semi-transparent white line is the only thing
	// that can move a corner's mean luminance. testsrc2 is far too busy for that.
	in := gen(t, ffmpeg, dir, "flat.mp4",
		"-f", "lavfi", "-i", "color=c=black:s=1280x720:rate=30:duration=1",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-an")
	probe, err := Probe(mustFFprobe(t))(ctx, in)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	plan := MakePlan(probe, LightPlanOptions(PlanOptions{}))

	textFile, err := WriteLightTagFile(dir, "tag.txt", "720p30 preview · 1440p60 on Clippet")
	if err != nil {
		t.Fatal(err)
	}
	base := EncodeOptions{Preset: "ultrafast", CRF: 26}
	plainOut := filepath.Join(dir, "plain.mp4")
	if err := Encode(ctx, ffmpeg, plan, in, plainOut, SocialEncodeOptions(base), nil); err != nil {
		t.Fatalf("untagged encode: %v", err)
	}
	tagged := SocialEncodeOptions(base)
	tagged.Tag = LightTag{FontFile: font, TextFile: textFile}
	taggedOut := filepath.Join(dir, "tagged.mp4")
	if err := Encode(ctx, ffmpeg, plan, in, taggedOut, tagged, nil); err != nil {
		t.Fatalf("tagged encode: %v", err)
	}

	// The corner the tag is in, and the three it is not. 420×40 at 720p is the
	// bottom-right region the line occupies at fontsize 16 with an 11 px inset.
	const w, h = 420, 40
	corners := map[string]string{
		"bottom-right": strconv.Itoa(w) + ":" + strconv.Itoa(h) + ":in_w-" + strconv.Itoa(w) + ":in_h-" + strconv.Itoa(h),
		"bottom-left":  strconv.Itoa(w) + ":" + strconv.Itoa(h) + ":0:in_h-" + strconv.Itoa(h),
		"top-right":    strconv.Itoa(w) + ":" + strconv.Itoa(h) + ":in_w-" + strconv.Itoa(w) + ":0",
		"top-left":     strconv.Itoa(w) + ":" + strconv.Itoa(h) + ":0:0",
	}
	for name, crop := range corners {
		before := cornerLuma(t, ffmpeg, plainOut, crop)
		after := cornerLuma(t, ffmpeg, taggedOut, crop)
		delta := after - before
		t.Logf("%s: YAVG %.3f → %.3f (%+.3f)", name, before, after, delta)
		if name == "bottom-right" {
			// A 16 px line of ~35 characters at 55 % white over a 420×40 black
			// region: a small mean, and unmistakably not zero.
			if delta < 0.5 {
				t.Errorf("the tag did not brighten the bottom-right corner: %+.3f", delta)
			}
		} else if delta > 0.05 {
			t.Errorf("the tag touched the %s corner: %+.3f", name, delta)
		}
	}

	// And the tagged file is still a card file: same dims, same rate, faststart.
	info, err := ProbeOutput(ctx, mustFFprobe(t), taggedOut)
	if err != nil {
		t.Fatalf("probe the tagged copy: %v", err)
	}
	if info.Width != 1280 || info.Height != 720 || info.FPS.Num != 30 {
		t.Errorf("the tagged copy is %dx%d @ %s", info.Width, info.Height, info.FPS.String())
	}
	if err := VerifyMP4(taggedOut); err != nil {
		t.Errorf("the tagged copy is not a faststart mp4: %v", err)
	}
}

// TestThePosterIsCutFromAnUntaggedFrame is the pixel half of §20 erratum 219:
// the card IMAGE carries no tag, and it is still card-sized.
//
// It is written as the same measurement twice over on ONE tagged card copy and
// one untagged source, because the failure it guards against is subtle in
// exactly one direction: a poster cut from the Light copy looks perfectly
// correct — right dimensions, right frame — and simply has a line of text
// burned into it that no document allows. So the test cuts a poster BOTH ways
// and asserts the corners disagree.
func TestThePosterIsCutFromAnUntaggedFrame(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	font := tagFont(t, ffmpeg)
	dir := t.TempDir()
	ctx := context.Background()

	// A black 1920×1080 source, as in the test above and for the same reason: a
	// semi-transparent white line is then the only thing that can move a
	// corner's mean luminance. 1080p is what makes the box mean something — the
	// poster is cut from THIS file and has to come out at the card's 1280×720.
	in := gen(t, ffmpeg, dir, "flat1080.mp4",
		"-f", "lavfi", "-i", "color=c=black:s=1920x1080:rate=30:duration=1",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-an")
	probe, err := Probe(ffprobe)(ctx, in)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	plan := MakePlan(probe, LightPlanOptions(PlanOptions{}))
	textFile, err := WriteLightTagFile(dir, "tag.txt", "720p30 preview · 1080p30 on Clippet")
	if err != nil {
		t.Fatal(err)
	}
	opts := SocialEncodeOptions(EncodeOptions{Preset: "ultrafast", CRF: 26})
	opts.Tag = LightTag{FontFile: font, TextFile: textFile}
	light := filepath.Join(dir, "light.mp4")
	if err := Encode(ctx, ffmpeg, plan, in, light, opts, nil); err != nil {
		t.Fatalf("tagged card copy: %v", err)
	}

	box := PosterBox{W: plan.W, H: plan.H}
	good := filepath.Join(dir, "good.jpg")
	res, err := Poster(ctx, ffmpeg, in, good, probe.DurationUS, probe.HasDuration, probe.Video.EndSec, box, PosterLabel{})
	if err != nil {
		t.Fatalf("poster from the untagged source: %v", err)
	}
	// THE BOX HELD: the card image is the card's size, cut from a 1080p file.
	if res.Width != plan.W || res.Height != plan.H {
		t.Errorf("the poster is %dx%d, want the card's %dx%d", res.Width, res.Height, plan.W, plan.H)
	}
	// The control: the same poster pass over the TAGGED copy, which is what the
	// pipeline used to do. Its box is the zero one — the file is already 720p.
	bad := filepath.Join(dir, "bad.jpg")
	if _, err := Poster(ctx, ffmpeg, light, bad, probe.DurationUS, probe.HasDuration, probe.Video.EndSec, PosterBox{}, PosterLabel{}); err != nil {
		t.Fatalf("poster from the tagged copy: %v", err)
	}

	// 420×40 of the bottom-right corner at 720p is the region the line occupies.
	const crop = "420:40:in_w-420:in_h-40"
	goodBR, goodBL := cornerLuma(t, ffmpeg, good, crop), cornerLuma(t, ffmpeg, good, "420:40:0:in_h-40")
	badBR := cornerLuma(t, ffmpeg, bad, crop)
	t.Logf("poster corners: untagged source BR %.3f / BL %.3f; tagged copy BR %.3f", goodBR, goodBL, badBR)
	if badBR-goodBL < 0.5 {
		t.Fatalf("the control is not tagged either (BR %.3f vs BL %.3f): this test proves nothing as written", badBR, goodBL)
	}
	if goodBR-goodBL > 0.05 {
		t.Errorf("the card image carries the tag: bottom-right %.3f vs bottom-left %.3f", goodBR, goodBL)
	}

	// AND THE BOXED THUMBNAIL PASS PARSES. The ladder above hides a failure of its
	// first candidate — a black source is near-black whatever pass took it, so a
	// scale link this filtergraph refused would fall through in silence and the
	// kept frame would still be the right size. So it is run on its own, and its
	// exit status and its picture are both read.
	thumb := filepath.Join(dir, "thumb.jpg")
	if _, se, err := runCapture(ctx, ffmpeg, PosterThumbArgs(in, thumb, 0, 1, box)...); err != nil {
		t.Fatalf("the boxed thumbnail pass failed: %v: %s", err, se.All())
	}
	frame, err := jpegStats(thumb)
	if err != nil {
		t.Fatalf("the boxed thumbnail pass wrote no readable JPEG: %v", err)
	}
	if frame.Width != plan.W || frame.Height != plan.H {
		t.Errorf("the boxed thumbnail pass wrote %dx%d, want %dx%d", frame.Width, frame.Height, plan.W, plan.H)
	}
}

// TestEncodeTiersWritesBothFiles: ONE ffmpeg, one decode, two complete files —
// the Full at the source's own size and the Light copy at the card's, each with
// its own rate and its own audio bitrate, and the tag on the Light one only.
func TestEncodeTiersWritesBothFiles(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	ctx := context.Background()

	// 1920×1080 at 60 fps with a tone: a Full is wanted at 1080p60 and a Light
	// copy at 720p30, which is the pair the erratum is about.
	in := gen(t, ffmpeg, dir, "src.mp4", join(
		[]string{"-f", "lavfi", "-i", src("1920x1080", "60"), "-f", "lavfi", "-i", sine("440")},
		v, a, []string{"-shortest"})...)
	probe, err := Probe(ffprobe)(ctx, in)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	box := PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60}
	fullPlan := MakePlan(probe, FullPlanOptions(probe, box))
	lightPlan := MakePlan(probe, LightPlanOptions(box))

	base := EncodeOptions{Preset: "ultrafast", CRF: FullCRF, Codec: FullCodecH264, Threads: 2}
	lightOpts := SocialEncodeOptions(base)
	if font := FindTagFont(); font != "" && strings.Contains(run(t, ffmpeg, "-hide_banner", "-filters"), "drawtext") {
		textFile, err := WriteLightTagFile(dir, "tag.txt", "720p30 preview · 1080p60 on Clippet")
		if err != nil {
			t.Fatal(err)
		}
		lightOpts.Tag = LightTag{FontFile: font, TextFile: textFile}
	}
	fullOut, lightOut := filepath.Join(dir, "full.mp4"), filepath.Join(dir, "light.mp4")
	if err := EncodeTiers(ctx, ffmpeg, in,
		TierOutput{Plan: fullPlan, Path: fullOut, Opts: base},
		TierOutput{Plan: lightPlan, Path: lightOut, Opts: lightOpts}, nil); err != nil {
		t.Fatalf("EncodeTiers: %v", err)
	}

	for _, tc := range []struct {
		name, path string
		w, h, fps  int
	}{
		{name: "full", path: fullOut, w: 1920, h: 1080, fps: 60},
		{name: "light", path: lightOut, w: 1280, h: 720, fps: 30},
	} {
		st, err := os.Stat(tc.path)
		if err != nil || st.Size() == 0 {
			t.Fatalf("%s: %v (size %v)", tc.name, err, st)
		}
		info, err := ProbeOutput(ctx, ffprobe, tc.path)
		if err != nil {
			t.Fatalf("probe %s: %v", tc.name, err)
		}
		if info.Width != tc.w || info.Height != tc.h {
			t.Errorf("%s is %dx%d, want %dx%d", tc.name, info.Width, info.Height, tc.w, tc.h)
		}
		if info.FPS.Den == 0 || info.FPS.Num/info.FPS.Den != tc.fps {
			t.Errorf("%s runs at %s, want %d fps", tc.name, info.FPS.String(), tc.fps)
		}
		if !info.HasAudio {
			t.Errorf("%s lost its audio", tc.name)
		}
		if err := VerifyMP4(tc.path); err != nil {
			t.Errorf("%s is not a faststart mp4: %v", tc.name, err)
		}
		if err := CheckTierOutput(info, TierCheck{Codecs: []string{FullCodecH264}, Width: tc.w, Height: tc.h}); err != nil {
			t.Errorf("%s fails its own tier check: %v", tc.name, err)
		}
	}
	// The Light copy is the smaller file: the whole reason the card fetches it.
	full, _ := os.Stat(fullOut)
	light, _ := os.Stat(lightOut)
	if light.Size() >= full.Size() {
		t.Errorf("the Light copy (%d bytes) is not smaller than the Full (%d)", light.Size(), full.Size())
	}
}

func mustFFprobe(t *testing.T) string {
	t.Helper()
	_, ffprobe := binaries(t)
	return ffprobe
}
