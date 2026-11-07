//go:build ffmpeg

package transcode

// The real thing (§16.2, §20 erratum 92): the poster is taken from clips
// shaped like the ones that broke it — a game clip that opens on a loading
// screen, and a capture whose device died mid-recording — and the assertion
// is the one a unit test cannot make: there is a PICTURE in the JPEG that
// Discord, X and the <video> element are handed.

import (
	"context"
	"path/filepath"
	"testing"
)

// blackIntroClip: 3 s of black followed by 7 s of colour bars, the shape of
// every clip that starts on a loading screen or a fade-in. The blind 15%
// seek lands at 1.5 s — inside the black.
func blackIntroClip(t *testing.T, ffmpeg, dir string) string {
	t.Helper()
	return gen(t, ffmpeg, dir, "blackintro.mp4", join(
		[]string{
			"-f", "lavfi", "-i", "color=c=black:s=640x360:r=30:d=3",
			"-f", "lavfi", "-i", "testsrc2=s=640x360:r=30:d=7",
			"-filter_complex", "[0:v][1:v]concat=n=2:v=1:a=0[v]", "-map", "[v]",
		}, v,
	)...)
}

// shortVideoLongAudioClip: 2 s of picture (the first half second black) in a
// 40 s file, because the audio keeps going. The container's duration sends a
// 15% seek to 6 s — four seconds past the last frame — where ffmpeg writes
// nothing at all and the ladder falls back to frame 1, which here is black.
func shortVideoLongAudioClip(t *testing.T, ffmpeg, dir string) string {
	t.Helper()
	return gen(t, ffmpeg, dir, "shortvideo.mp4", join(
		[]string{
			"-f", "lavfi", "-i", "color=c=black:s=320x240:r=30:d=0.5",
			"-f", "lavfi", "-i", "testsrc2=s=320x240:r=30:d=1.5",
			"-f", "lavfi", "-i", "sine=frequency=440:duration=40",
			"-filter_complex", "[0:v][1:v]concat=n=2:v=1:a=0[v]", "-map", "[v]", "-map", "2:a",
		}, v, a,
	)...)
}

func TestPosterSkipsABlackIntro(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	in := blackIntroClip(t, ffmpeg, dir)

	info, err := ProbeOutput(context.Background(), ffprobe, in)
	if err != nil {
		t.Fatalf("ProbeOutput: %v", err)
	}
	// The premise: the old rule's frame really is black. If this ever stops
	// being true the test below proves nothing.
	seek := PosterSeek(int64(info.DurationMS)*1000, true, info.VideoEndSec)
	blind := filepath.Join(dir, "blind.jpg")
	if _, _, err := runCapture(context.Background(), ffmpeg, PosterArgs(in, blind, seek, PosterBox{})...); err != nil {
		t.Fatalf("the blind seek at %.3fs: %v", seek, err)
	}
	before, err := jpegStats(blind)
	if err != nil {
		t.Fatalf("the blind seek produced no readable frame: %v", err)
	}
	if !before.nearBlack() {
		t.Fatalf("the fixture is wrong: the frame at %.3fs is not black (mean %.1f, max %.1f)", seek, before.MeanLuma, before.MaxLuma)
	}

	out := filepath.Join(dir, "poster.jpg")
	res, err := Poster(context.Background(), ffmpeg, in, out, int64(info.DurationMS)*1000, true, info.VideoEndSec, PosterBox{}, PosterLabel{})
	if err != nil {
		t.Fatalf("Poster: %v", err)
	}
	if res.NearBlack {
		t.Fatal("the poster is near-black: the card image is a black rectangle")
	}
	if res.Width != 640 || res.Height != 360 {
		t.Fatalf("poster %dx%d, want the clip's 640x360", res.Width, res.Height)
	}
	frame, err := jpegStats(out)
	if err != nil {
		t.Fatalf("jpegStats: %v", err)
	}
	if frame.MeanLuma <= posterMinMeanLuma {
		t.Fatalf("poster mean luma %.2f, want a lit frame (> %.0f)", frame.MeanLuma, posterMinMeanLuma)
	}
	// And the result says WHERE that frame is, not merely where the window
	// began (§20 erratum 92(c), amended). The window began at 1.5 s, inside
	// the black; the frame is past 3 s, and anything downstream that starts
	// "where the poster is" — the GIF — needs the second number.
	if res.Seconds <= 3 {
		t.Fatalf("the poster reports %.3fs, still inside the 3 s black intro: the chosen frame was not reported", res.Seconds)
	}
	// PosterFrameSeconds is the same answer without the picture, which is
	// what a caller with no poster to write asks for.
	if at := PosterFrameSeconds(context.Background(), ffmpeg, in, int64(info.DurationMS)*1000, true, info.VideoEndSec); at != res.Seconds {
		t.Fatalf("PosterFrameSeconds = %.3fs, Poster chose %.3fs: the two must select the same frame", at, res.Seconds)
	}
}

func TestPosterOfAClipWhoseVideoStopsEarly(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	in := shortVideoLongAudioClip(t, ffmpeg, dir)

	info, err := ProbeOutput(context.Background(), ffprobe, in)
	if err != nil {
		t.Fatalf("ProbeOutput: %v", err)
	}
	if info.DurationMS < 39_000 || info.VideoEndSec < 1.9 || info.VideoEndSec > 2.1 {
		t.Fatalf("the fixture is wrong: container %d ms, picture %.3f s", info.DurationMS, info.VideoEndSec)
	}

	out := filepath.Join(dir, "poster.jpg")
	res, err := Poster(context.Background(), ffmpeg, in, out, int64(info.DurationMS)*1000, true, info.VideoEndSec, PosterBox{}, PosterLabel{})
	if err != nil {
		t.Fatalf("Poster: %v", err)
	}
	// The seek is inside the picture, not 15% of the audio.
	if res.Seconds <= 0 || res.Seconds >= info.VideoEndSec {
		t.Fatalf("the poster was taken at %.3fs, outside the %.3fs of picture", res.Seconds, info.VideoEndSec)
	}
	if res.NearBlack {
		t.Fatal("the poster is near-black: the seek fell back to the black first frame")
	}
	if res.Width != 320 || res.Height != 240 {
		t.Fatalf("poster %dx%d, want the clip's 320x240", res.Width, res.Height)
	}
	frame, err := jpegStats(out)
	if err != nil {
		t.Fatalf("jpegStats: %v", err)
	}
	if frame.MeanLuma <= posterMinMeanLuma {
		t.Fatalf("poster mean luma %.2f, want a lit frame (> %.0f)", frame.MeanLuma, posterMinMeanLuma)
	}
}
