//go:build ffmpeg

package transcode

// The real thing (DESIGN.md §16.2, §17 #5): both trim paths run against
// clips this test generates, and the assertion is the one the whole feature
// stands on — the output is the WINDOW that was asked for. A unit test can
// pin the argv; only ffmpeg can prove that `-ss` in front of `-i` means what
// this package believes it means, which is exactly where a stream copy
// differs from a re-encode.

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"strconv"
	"testing"
)

// gopClip renders a 6s 640x360 clip whose keyframes sit on whole seconds
// (-g 30 -sc_threshold 0 over 30 fps), so a cut at 1s and 3s is exactly
// keyframe-aligned. It is already the shape a plan would produce, so nothing
// but the keyframe grid decides the path.
func gopClip(t *testing.T, ffmpeg, dir string) string {
	t.Helper()
	return gen(t, ffmpeg, dir, "gop30.mp4", join(
		[]string{"-f", "lavfi", "-i", src("640x360", "30"), "-f", "lavfi", "-i", sine("440")},
		v, a,
		[]string{"-g", "30", "-keyint_min", "30", "-sc_threshold", "0", "-shortest"},
	)...)
}

// longGOPClip renders the same 6s clip with x264's default 250-frame
// keyframe interval: over six seconds that is ONE keyframe, at 0, so every
// interior cut point forces the re-encode path.
func longGOPClip(t *testing.T, ffmpeg, dir string) string {
	t.Helper()
	return gen(t, ffmpeg, dir, "longgop.mp4", join(
		[]string{"-f", "lavfi", "-i", src("640x360", "30"), "-f", "lavfi", "-i", sine("440")},
		v, a, []string{"-sc_threshold", "0", "-shortest"},
	)...)
}

// mismatchedClip renders a file whose video and audio have DIFFERENT lengths
// — the shape that made a perfectly valid trim window produce an output with
// no video stream in it, because the CONTAINER outlives the video track. Both
// orders exist in the wild (a screen capture whose encoder stops early, a
// still with a soundtrack), so both are generated.
func mismatchedClip(t *testing.T, ffmpeg, dir, file string, videoSec, audioSec int) string {
	t.Helper()
	return gen(t, ffmpeg, dir, file,
		"-f", "lavfi", "-i", "testsrc2=duration="+strconv.Itoa(videoSec)+":size=320x180:rate=30",
		"-f", "lavfi", "-i", "sine=frequency=440:duration="+strconv.Itoa(audioSec),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "64k",
	)
}

// TestCoverTrimAgainstRealStreamExtents is F2's regression, measured on real
// files: the per-stream extents come out of ffprobe, and CoverTrim's three
// verdicts follow from them.
func TestCoverTrimAgainstRealStreamExtents(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()

	shortVideo := probeFile(t, ffprobe, mismatchedClip(t, ffmpeg, dir, "v2a8.mp4", 2, 8))
	shortAudio := probeFile(t, ffprobe, mismatchedClip(t, ffmpeg, dir, "v8a2.mp4", 8, 2))

	if !shortVideo.Video.Known || shortVideo.Video.EndSec > 3 {
		t.Fatalf("the 2s video of an 8s file measured %+v", shortVideo.Video)
	}
	if !shortVideo.Audio.Known || shortVideo.Audio.EndSec < 7 {
		t.Fatalf("the 8s audio measured %+v", shortVideo.Audio)
	}
	if shortVideo.HasDuration && shortVideo.DurationUS < 7_000_000 {
		t.Fatalf("the container of the 2s-video file declares %d us; the point is that it outlives the video", shortVideo.DurationUS)
	}

	// 5000–9000 of the file whose VIDEO stops at 2s: nothing to cut. This is
	// the window that used to reach ffmpeg and come back as `bad_output` at
	// ERROR, from a request the API had every reason to accept.
	if _, err := CoverTrim(shortVideo, 5000, 9000); !errors.Is(err, ErrWindowEmpty) {
		t.Errorf("CoverTrim past the video's end = %v, want ErrWindowEmpty", err)
	}
	// The same window on the file whose AUDIO stops at 2s: the cut is made,
	// and it is silent.
	cov, err := CoverTrim(shortAudio, 5000, 9000)
	if err != nil {
		t.Fatalf("CoverTrim within the video = %v", err)
	}
	if !cov.DropAudio {
		t.Error("a window past the audio must drop the audio, not carry an empty track")
	}
	if !cov.Verified || cov.StartMS != 5000 {
		t.Errorf("coverage = %+v, want a verified 5000..~8000 window", cov)
	}
	if !cov.Clamped || cov.EndMS > 8100 || cov.EndMS < 7500 {
		t.Errorf("coverage = %+v, want the end clamped to the video's ~8000", cov)
	}
	// A window inside both streams keeps its audio and is not clamped.
	cov, err = CoverTrim(shortAudio, 0, 1500)
	if err != nil || cov.DropAudio || cov.Clamped || cov.EndMS != 1500 {
		t.Errorf("CoverTrim(0..1500) = %+v, %v; want the window untouched with audio", cov, err)
	}
}

func probeFile(t *testing.T, ffprobe, path string) *ProbeResult {
	t.Helper()
	p, err := Probe(ffprobe)(context.Background(), path)
	if err != nil {
		t.Fatalf("probe %s: %v", filepath.Base(path), err)
	}
	return p
}

// checkTrimmed runs the output through the same gates the worker does and
// returns its measured duration in ms.
func checkTrimmed(t *testing.T, ffprobe, out string) int64 {
	t.Helper()
	info, err := ProbeOutput(context.Background(), ffprobe, out)
	if err != nil {
		t.Fatalf("probe the trimmed output: %v", err)
	}
	if err := CheckOutput(info, PlanOptions{}); err != nil {
		t.Fatalf("the trimmed output is not the shape the pipeline promises: %v", err)
	}
	if err := VerifyMP4(out); err != nil {
		t.Fatalf("the trimmed output is not a faststart mp4: %v", err)
	}
	if info.VideoCodec != "h264" || !info.HasAudio || info.AudioCodec != "aac" {
		t.Errorf("codecs = %s/%s (audio %v), want h264/aac", info.VideoCodec, info.AudioCodec, info.HasAudio)
	}
	return info.DurationMS
}

func TestTrimStreamCopyOnKeyframes(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	in := gopClip(t, ffmpeg, dir)

	p := probeFile(t, ffprobe, in)
	keys, err := Keyframes(context.Background(), ffprobe, in, p.VideoIndex)
	if err != nil {
		t.Fatalf("keyframes: %v", err)
	}
	if len(keys) < 6 {
		t.Fatalf("keyframes = %v, want one per second", keys)
	}
	tp := DecideTrim(p, MakePlan(p, PlanOptions{}), keys, 1000, 3000)
	if !tp.Copy {
		t.Fatalf("a keyframe-aligned cut of an already-conforming mp4 must copy; got %q", tp.Reason)
	}
	out := filepath.Join(dir, "copy.mp4")
	if err := Copy(context.Background(), ffmpeg, tp, p, in, out); err != nil {
		t.Fatalf("copy trim: %v", err)
	}
	// The copy ends on a packet boundary, so it may overshoot by a frame or
	// two — never by the whole GOP an unsnapped `-ss` would have rewound.
	if got := checkTrimmed(t, ffprobe, out); math.Abs(float64(got-2000)) > 150 {
		t.Errorf("the copied window is %d ms, want 2000 ±150", got)
	}
}

func TestTrimReencodeOffKeyframes(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	in := longGOPClip(t, ffmpeg, dir)

	p := probeFile(t, ffprobe, in)
	keys, err := Keyframes(context.Background(), ffprobe, in, p.VideoIndex)
	if err != nil {
		t.Fatalf("keyframes: %v", err)
	}
	plan := MakePlan(p, PlanOptions{})
	tp := DecideTrim(p, plan, keys, 1234, 3234)
	if tp.Copy {
		t.Fatalf("a cut between keyframes (%v) must re-encode", keys)
	}
	if tp.Reason != ReasonStartNotKey {
		t.Errorf("reason = %q, want %q", tp.Reason, ReasonStartNotKey)
	}
	plan.HasDuration, plan.DurationUS = true, int64(tp.Window.Duration*1e6)
	out := filepath.Join(dir, "reencode.mp4")
	o := ffmpegOpts
	o.Trim = &tp.Window
	if err := Encode(context.Background(), ffmpeg, plan, in, out, o, nil); err != nil {
		t.Fatalf("re-encode trim: %v", err)
	}
	// Input seeking is ACCURATE for a re-encode: the frames between the
	// preceding keyframe and 1.234s are decoded and discarded, so the window
	// is exact to the frame.
	if got := checkTrimmed(t, ffprobe, out); math.Abs(float64(got-2000)) > 100 {
		t.Errorf("the re-encoded window is %d ms, want 2000 ±100", got)
	}
}

// TestTrimCopySeekRewindsToTheKeyframe is the regression this whole decision
// exists for, and it is nastier than it looks: `-ss` on a stream copy seeks
// BACKWARD to a keyframe, and because the output timestamps are then shifted
// to zero, `-t` still yields a clip of exactly the requested LENGTH. A window
// that was not snapped to a keyframe therefore produces a plausible file that
// starts up to a whole GOP too early — a wrong cut no duration check can
// see. Refusing such a window in DecideTrim is the only defence, so this test
// measures the rewind directly (with -copyts, the one way to see where the
// seek landed) and then asserts the refusal.
func TestTrimCopySeekRewindsToTheKeyframe(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	in := gopClip(t, ffmpeg, dir)
	p := probeFile(t, ffprobe, in)
	keys, err := Keyframes(context.Background(), ffprobe, in, p.VideoIndex)
	if err != nil {
		t.Fatal(err)
	}

	// Where does a copy asked to start at 1.5s actually start? -copyts keeps
	// the source timestamps, so the first packet's pts is the answer.
	probe := filepath.Join(dir, "rewound.mp4")
	run(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-copyts",
		"-ss", "1.5", "-i", in, "-c", "copy", "-f", "mp4", probe)
	firstKeys, err := Keyframes(context.Background(), ffprobe, probe, 0)
	if err != nil || len(firstKeys) == 0 {
		t.Fatalf("keyframes of the copied file: %v %v", firstKeys, err)
	}
	if start := firstKeys[0]; math.Abs(start-1.5) < 0.01 {
		t.Logf("this ffmpeg starts an unsnapped copy at %.3fs; other builds rewind, and the snap covers both", start)
	} else if math.Abs(start-1.0) > 0.01 {
		t.Errorf("an unsnapped copy started at %.3fs, neither the request (1.5) nor the keyframe (1.0)", start)
	}

	// Whatever this build does, the decision refuses the window: a cut
	// between keyframes is a re-encode.
	if tp := DecideTrim(p, MakePlan(p, PlanOptions{}), keys, 1500, 3500); tp.Copy {
		t.Error("DecideTrim allowed a copy at 1.5s, which ffmpeg is free to rewind to 1.0s")
	}
	// And the window it DOES allow starts on the keyframe, never on the
	// millisecond that was asked for.
	tp := DecideTrim(p, MakePlan(p, PlanOptions{}), keys, 1020, 3000)
	if !tp.Copy || math.Abs(tp.Window.Start-1.0) > 1e-6 {
		t.Errorf("a request 20 ms past a keyframe gave %+v (copy=%v); want a copy snapped to 1.000", tp.Window, tp.Copy)
	}
}
