package main

import (
	"bytes"
	"log/slog"
	"os/exec"
	"strings"
	"testing"

	"clippet/internal/config"
)

// The boot capability checks (§20 errata 214–216). Two of them are FATAL and one
// is a warning, and which is which is the whole point: an encoder the operator
// asked for and this binary does not have means every encode fails, while a
// missing font means one cosmetic line is missing.

// cpuImage is what the app's own static ffmpeg lists: libx264 and libsvtav1, no
// nvenc at all, and drawtext.
var cpuImage = ffCaps{
	encoders: " V..... libx264   libx264 H.264\n V..... libsvtav1  SVT-AV1\n A..... aac  AAC\n",
	filters:  " T.C drawtext  Draw text\n ..C scale  Scale\n",
}

// gpuImage is jrottenberg/ffmpeg:7.1-nvidia: nvenc and the CUDA scaler, and no
// libsvtav1 (which is why FULL_CODEC=av1 on the GPU image is a boot error).
var gpuImage = ffCaps{
	encoders: " V..... libx264  libx264 H.264\n V..... h264_nvenc  NVIDIA NVENC H.264\n",
	filters:  " ..C scale_cuda  GPU accelerated video resizer\n T.C drawtext  Draw text\n",
}

func testLog() (*slog.Logger, *bytes.Buffer) {
	var b bytes.Buffer
	return slog.New(slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelDebug})), &b
}

func TestCheckFFCaps(t *testing.T) {
	const font = "/usr/share/fonts/dejavu/DejaVuSans.ttf"
	for _, tc := range []struct {
		name     string
		cfg      config.Config
		caps     ffCaps
		font     string
		wantFont string
		errHas   string
		warnHas  string
	}{
		{name: "the default install", cfg: config.Config{
			Transcode: config.TranscodeConfig{FullCodec: "h264"},
			LightTag:  config.LightTagConfig{Text: config.DefaultLightTagText}},
			caps: cpuImage, font: font, wantFont: font},
		{name: "av1 on the cpu image", cfg: config.Config{
			Transcode: config.TranscodeConfig{FullCodec: "av1"}},
			caps: cpuImage},
		{name: "av1 on an ffmpeg without it", cfg: config.Config{
			Transcode: config.TranscodeConfig{FullCodec: "av1"}},
			caps: gpuImage, errHas: "FULL_CODEC=av1"},
		{name: "cuda on the cpu image", cfg: config.Config{
			Transcode: config.TranscodeConfig{FullCodec: "h264", HWAccel: config.HWAccelCUDA}},
			caps: cpuImage, errHas: "TRANSCODE_HWACCEL=cuda"},
		{name: "cuda on the gpu image", cfg: config.Config{
			Transcode: config.TranscodeConfig{FullCodec: "h264", HWAccel: config.HWAccelCUDA},
			LightTag:  config.LightTagConfig{Text: config.DefaultLightTagText}},
			caps: gpuImage, font: font, wantFont: font},
		{name: "the tag with no font is a WARNING", cfg: config.Config{
			Transcode: config.TranscodeConfig{FullCodec: "h264"},
			LightTag:  config.LightTagConfig{Text: config.DefaultLightTagText}},
			caps: cpuImage, warnHas: "no font"},
		{name: "the tag with no drawtext is a WARNING", cfg: config.Config{
			Transcode: config.TranscodeConfig{FullCodec: "h264"},
			LightTag:  config.LightTagConfig{Text: config.DefaultLightTagText}},
			caps: ffCaps{encoders: cpuImage.encoders, filters: " ..C scale  Scale\n"},
			font: font, warnHas: "drawtext"},
		{name: "no tag asked for, no font wanted", cfg: config.Config{
			Transcode: config.TranscodeConfig{FullCodec: "h264"}},
			caps: cpuImage, font: font},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, buf := testLog()
			got, err := checkFFCaps(tc.cfg, tc.caps, tc.font, log)
			switch {
			case tc.errHas != "" && err == nil:
				t.Fatalf("want a fatal error naming %q, got none", tc.errHas)
			case tc.errHas != "" && !strings.Contains(err.Error(), tc.errHas):
				t.Fatalf("error %q does not name %q", err, tc.errHas)
			case tc.errHas == "" && err != nil:
				t.Fatalf("unexpected fatal error: %v", err)
			}
			if got != tc.wantFont {
				t.Errorf("font = %q, want %q", got, tc.wantFont)
			}
			if tc.warnHas != "" && !strings.Contains(buf.String(), tc.warnHas) {
				t.Errorf("no warning naming %q:\n%s", tc.warnHas, buf.String())
			}
			if tc.warnHas != "" && !strings.Contains(buf.String(), "level=WARN") {
				t.Errorf("the missing tag was not a WARNING:\n%s", buf.String())
			}
			if tc.warnHas == "" && strings.Contains(buf.String(), "level=WARN") {
				t.Errorf("unexpected warning:\n%s", buf.String())
			}
		})
	}
}

// TestReadFFCapsAgainstTheRealBinary is the belt on the fixtures above: whatever
// this image ships, the two listings are readable and name libx264 and drawtext,
// so the strings the checks look for are the strings ffmpeg prints.
func TestReadFFCapsAgainstTheRealBinary(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg on PATH")
	}
	caps, err := readFFCaps(t.Context(), ffmpeg)
	if err != nil {
		t.Fatalf("readFFCaps: %v", err)
	}
	if !caps.hasEncoder("libx264") {
		t.Errorf("this ffmpeg lists no libx264:\n%s", caps.encoders)
	}
	if !caps.hasFilter("scale") {
		t.Errorf("this ffmpeg lists no scale filter:\n%s", caps.filters)
	}
}
