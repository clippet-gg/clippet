//go:build ffmpeg && unix

package transcode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestFFmpegStallOnSilentInput: a real ffmpeg blocked on a FIFO that never
// delivers bytes emits no progress; the stall timer must kill it and the
// verdict is permanent encode_stalled (not a hang, not a hard timeout).
func TestFFmpegStallOnSilentInput(t *testing.T) {
	ffmpeg, _ := binaries(t)
	dir := t.TempDir()
	fifo := filepath.Join(dir, "in.mp4")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// Hold a writer open so ffmpeg's open() succeeds and its read() blocks.
	w, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	plan := basePlan(AudioNone, nil)
	start := time.Now()
	err = Encode(context.Background(), ffmpeg, plan, fifo, filepath.Join(dir, "out.mp4"),
		EncodeOptions{Preset: "veryfast", CRF: 23, StallTimeout: 500 * time.Millisecond, HardTimeout: time.Minute}, nil)
	if !IsPermanent(err) || !strings.Contains(err.Error(), "encode_stalled") {
		t.Fatalf("err = %v", err)
	}
	if el := time.Since(start); el > 15*time.Second {
		t.Fatalf("stalled ffmpeg took %s to reap", el)
	}
}
