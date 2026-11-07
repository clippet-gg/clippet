package transcode

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// TestRunCaptureHandsOverTheWholeRing (a nit from lane hwtail): runCapture's
// stderr comes back as BOTH readings, so the next thing that wants to ask a
// question of an ffprobe or ffmpeg failure is not handed a kilobyte.
//
// Erratum 289 is the reason this matters and the reason it is not a bug report:
// the classification of a refused hardware decode has to read the WHOLE capture,
// because ffmpeg's shutdown after one runs past the failure tail and the tail can
// open past the line that named the cause. runInstrumentedRefusal already reads
// the ring; runCapture — every ffprobe, every poster, every remux — returned the
// tail alone. Nothing classifies those today, so nothing was broken; what is
// closed here is the SHAPE, so the next classifier cannot be handed the short
// reading by accident.
func TestRunCaptureHandsOverTheWholeRing(t *testing.T) {
	// A trailer longer than the failure tail, whose FIRST line is the one a
	// classifier would want and whose tail therefore cannot carry it — the
	// shape of erratum 289's own capture, in miniature.
	const marker = "cuvidCreateDecoder failed: CUDA_ERROR_INVALID_VALUE"
	var b strings.Builder
	b.WriteString(marker + "\n")
	for b.Len() < failureTail*2 {
		b.WriteString("[h264 @ 0x1] a line of shutdown noise that nothing needs to read\n")
	}
	capture := b.String()

	r := newRing(stderrRingSize)
	if _, err := r.Write([]byte(capture)); err != nil {
		t.Fatal(err)
	}
	se := captureOf(r)

	if strings.Contains(se.Reason(), marker) {
		t.Fatalf("this fixture no longer reproduces the trap: the %d-byte reason still carries the cause", failureTail)
	}
	if !strings.Contains(se.All(), marker) {
		t.Fatalf("the whole ring does not carry the cause:\n%s", se.All())
	}
	if len(se.Reason()) > failureTail {
		t.Fatalf("the reason is %d bytes; it is a log line and stays %d", len(se.Reason()), failureTail)
	}
	if se.Reason() != r.Tail(failureTail) || se.All() != r.Tail(stderrRingSize) {
		t.Fatal("captureOf does not take the two readings erratum 289 defined")
	}
}

// TestRunCaptureOfARealProcess: the capture a real non-zero exit produces holds
// both readings, and exitReason spends the SHORT one — the log line an operator
// is shown must not grow because the classification can now see more.
func TestRunCaptureOfARealProcess(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh on this image: %v", err)
	}
	long := strings.Repeat("y", failureTail+200)
	_, se, err := runCapture(context.Background(), sh, "-c", "printf 'HEAD-MARKER\n%s\n' "+long+" 1>&2; exit 3")
	if err == nil {
		t.Fatal("a command that exits 3 returned no error")
	}
	if !strings.Contains(se.All(), "HEAD-MARKER") {
		t.Fatalf("the whole capture lost the first line:\n%s", se.All())
	}
	if strings.Contains(se.Reason(), "HEAD-MARKER") {
		t.Fatalf("this fixture no longer reproduces the trap; the reason carries the first line:\n%s", se.Reason())
	}
	reason := exitReason("sh", err, se)
	if !strings.HasPrefix(reason, "sh exited 3: ") {
		t.Fatalf("exitReason = %q", reason)
	}
	if strings.Contains(reason, "HEAD-MARKER") {
		t.Fatalf("the failure REASON grew to the whole ring: it is a log line (§20 erratum 289)")
	}
}
