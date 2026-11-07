//go:build ffmpeg

package transcode

// §17 #3 on real ffmpeg: the card file is the rendition at half the frame
// rate and under a bitrate ceiling, and it is still a file a crawler will
// play — one h264 stream, one aac stream, moov before mdat.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSocialRenditionIsSmallerAndSlower encodes a 720p60 rendition the way
// the pipeline does, then the social file FROM it the way the social job
// does, and measures both.
func TestSocialRenditionIsSmallerAndSlower(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	ctx := context.Background()

	// A 1080p60 upload, encoded exactly as the pipeline would: the main
	// rendition, 1280×720 at 60 fps.
	in := gen(t, ffmpeg, dir, "hd-1080p60.mp4", join(
		[]string{"-f", "lavfi", "-i", src("1920x1080", "60"), "-f", "lavfi", "-i", sine("600")}, v, a, []string{"-shortest"})...)
	sp, err := Probe(ffprobe)(ctx, in)
	if err != nil {
		t.Fatalf("Probe(source): %v", err)
	}
	box := PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60}
	rendition := filepath.Join(dir, "main.mp4")
	if err := Encode(ctx, ffmpeg, MakePlan(sp, box), in, rendition, ffmpegOpts, nil); err != nil {
		t.Fatalf("Encode(rendition): %v", err)
	}

	// The social job's input is that rendition, probed on its own terms.
	rp, err := Probe(ffprobe)(ctx, rendition)
	if err != nil {
		t.Fatalf("Probe(rendition): %v", err)
	}
	plan := MakePlan(rp, SocialPlanOptions(box))
	if (plan.FPS != Rational{30, 1}) {
		t.Fatalf("social plan fps = %s, want 30/1", plan.FPS)
	}
	out := filepath.Join(dir, "social.mp4")
	if err := Encode(ctx, ffmpeg, plan, rendition, out, SocialEncodeOptions(ffmpegOpts), nil); err != nil {
		t.Fatalf("Encode(social): %v", err)
	}

	info, err := ProbeOutput(ctx, ffprobe, out)
	if err != nil {
		t.Fatalf("ProbeOutput: %v", err)
	}
	if err := CheckOutput(info, box); err != nil {
		t.Fatalf("CheckOutput: %v (%+v)", err, *info)
	}
	if (info.FPS != Rational{30, 1}) {
		t.Errorf("social output fps = %s, want 30/1", info.FPS)
	}
	// Same picture, same geometry: only the rate and the bits change, which
	// is why the card's dims agree with the rendition's today (erratum 123).
	if info.Width != 1280 || info.Height != 720 {
		t.Errorf("social output %dx%d, want the rendition's 1280x720", info.Width, info.Height)
	}
	if !info.HasAudio || info.AudioCodec != "aac" {
		t.Errorf("social audio = %q (has=%v), want aac", info.AudioCodec, info.HasAudio)
	}
	if err := VerifyMP4(out); err != nil {
		t.Errorf("VerifyMP4: %v", err)
	}
	// The point of the whole feature: fewer bytes than the file a player
	// streams. (The absolute numbers are content-dependent; the ORDER is
	// not — half the frames at a softer CRF under a 2.5 Mbps ceiling.)
	rst, err1 := os.Stat(rendition)
	sst, err2 := os.Stat(out)
	if err1 != nil || err2 != nil {
		t.Fatalf("stat: %v %v", err1, err2)
	}
	if sst.Size() >= rst.Size() {
		t.Errorf("social file is %d bytes, rendition %d — the card file must be the smaller one", sst.Size(), rst.Size())
	}
	t.Logf("rendition %d bytes, social %d bytes (%.0f%%)", rst.Size(), sst.Size(), 100*float64(sst.Size())/float64(rst.Size()))

	// The audio really is the smaller bitrate, and the stream is still the
	// stereo 48 kHz shape every rendition promises.
	audio := strings.TrimSpace(strings.ReplaceAll(run(t, ffprobe, "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=channels,sample_rate", "-of", "csv=p=0", out), "\r", ""))
	if audio != "48000,2" {
		t.Errorf("social audio stream = %q, want 48000,2", audio)
	}
}
