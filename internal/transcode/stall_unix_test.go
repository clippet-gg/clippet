//go:build unix

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

// The stall watchdog end to end, with neither a sleep nor a wall-clock budget
// (§20 erratum 331, rule R2): the "ffmpeg" is `cat` of a FIFO the test writes,
// so every progress line arrives exactly when the test says, and the stall
// timeout runs on a fakeClock, so "silence longer than the timeout" is an
// Advance. A slow spawn under sixteen shards moves nothing: the timer cannot
// fire until the test advances the clock past it.
//
// These replace three tests that used `sleep 3` / `sleep 30` scripts against a
// 150 ms–2 s timeout. TestEncodeStallDisabledPast99Percent's was widened three
// times (150 ms → 1 s → 2 s) and still failed gate 28a on a slow spawn.

// pipeFFmpeg is a fake ffmpeg whose stdout is whatever the test writes into a
// FIFO. The test holds the FIFO open read-write, so neither side's open()
// blocks, and closing it is the EOF that lets `cat` exit 0.
type pipeFFmpeg struct {
	bin string
	w   *os.File
}

func newPipeFFmpeg(t *testing.T) *pipeFFmpeg {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "progress")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	p := &pipeFFmpeg{bin: fakeFFmpeg(t, "exec cat '"+fifo+"'\n"), w: w}
	t.Cleanup(func() { _ = p.w.Close() })
	return p
}

// say writes one progress block's lines.
func (p *pipeFFmpeg) say(t *testing.T, lines ...string) {
	t.Helper()
	if _, err := p.w.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
}

// end closes the test's end of the FIFO: `cat` reads EOF and exits 0.
func (p *pipeFFmpeg) end(t *testing.T) {
	t.Helper()
	if err := p.w.Close(); err != nil {
		t.Fatal(err)
	}
}

// encodeAsync runs Encode on its own goroutine and returns its result channel.
func encodeAsync(p *pipeFFmpeg, plan Plan, o EncodeOptions, onProgress func(float64, bool)) <-chan error {
	errc := make(chan error, 1)
	go func() { errc <- Encode(context.Background(), p.bin, plan, "in", "out", o, onProgress) }()
	return errc
}

// result waits for Encode to return, for at most fakeBound of wall time.
func result(t *testing.T, errc <-chan error) error {
	t.Helper()
	select {
	case err := <-errc:
		return err
	case <-time.After(fakeBound):
		t.Fatalf("Encode never returned within %v of wall time — %s", fakeBound, ignoredClock)
		return nil
	}
}

// A block, then one stall timeout of silence: the run is killed and the verdict
// is a permanent encode_stalled.
func TestEncodeStall(t *testing.T) {
	ff := newPipeFFmpeg(t)
	fc := newFakeClock()
	o := stdOpts
	o.StallTimeout, o.StallClock = time.Minute, fc
	errc := encodeAsync(ff, basePlan(AudioNone, nil), o, nil)
	fc.await(t, "the watchdog armed at process start", func(s fakeState) bool { return s.armed == 1 })
	ff.say(t, "out_time_us=1000000", "progress=continue")
	fc.await(t, "the advancing block re-armed the timer", func(s fakeState) bool { return s.resets == 1 })
	if n := fc.Advance(o.StallTimeout - time.Nanosecond); n != 0 {
		t.Fatalf("the stall timer fired %d time(s) before the timeout", n)
	}
	if n := fc.Advance(time.Nanosecond); n != 1 {
		t.Fatalf("the stall timer fired %d time(s) at the timeout, want 1", n)
	}
	err := result(t, errc)
	if !IsPermanent(err) || !strings.Contains(err.Error(), "encode_stalled") {
		t.Fatalf("stall: err = %v", err)
	}
}

// A block at 99.5% then silence far longer than the stall timeout, then end:
// the faststart tail must not be mistaken for a stall.
func TestEncodeStallDisabledPast99Percent(t *testing.T) {
	ff := newPipeFFmpeg(t)
	fc := newFakeClock()
	o := stdOpts
	o.StallTimeout, o.StallClock = time.Minute, fc
	errc := encodeAsync(ff, basePlan(AudioNone, nil), o, nil)
	fc.await(t, "the watchdog armed at process start", func(s fakeState) bool { return s.armed == 1 })
	ff.say(t, "out_time_us=5970000", "progress=continue")
	fc.await(t, "the 99.5% block was observed", func(s fakeState) bool { return s.resets+s.stops >= 1 })
	if s := fc.state(); s.armed != 0 {
		t.Fatalf("the 99.5%% block left the stall timer armed: %+v", s)
	}
	if n := fc.Advance(time.Hour); n != 0 {
		t.Fatalf("an hour of tail fired %d stall timer(s)", n)
	}
	ff.say(t, "progress=end")
	ff.end(t)
	if err := result(t, errc); err != nil {
		t.Fatalf("remux tail treated as stall: %v", err)
	}
}

// With no known duration the watchdog never arms: an hour of silence after a
// block is still a run that ends cleanly, and every report is indeterminate.
func TestEncodeUnknownDurationNeverStalls(t *testing.T) {
	ff := newPipeFFmpeg(t)
	fc := newFakeClock()
	o := stdOpts
	o.StallTimeout, o.StallClock = time.Minute, fc
	p := basePlan(AudioNone, nil)
	p.HasDuration, p.DurationUS = false, 0
	reported := make(chan bool, 8)
	errc := encodeAsync(ff, p, o, func(_ float64, indeterminate bool) { reported <- indeterminate })
	ff.say(t, "out_time_us=1000000", "progress=continue")
	select {
	case indeterminate := <-reported:
		if !indeterminate {
			t.Fatal("unknown duration must report indeterminate")
		}
	case <-time.After(fakeBound):
		t.Fatalf("the block was never reported within %v of wall time — %s", fakeBound, ignoredClock)
	}
	if n := fc.Advance(time.Hour); n != 0 {
		t.Fatalf("%d stall timer(s) fired with no known duration", n)
	}
	if s := fc.state(); s.timers != 0 {
		t.Fatalf("a run with no duration armed a stall timer: %+v", s)
	}
	ff.say(t, "progress=end")
	ff.end(t)
	if err := result(t, errc); err != nil {
		t.Fatalf("unknown duration stalled: %v", err)
	}
	close(reported)
	for indeterminate := range reported {
		if !indeterminate {
			t.Fatal("unknown duration must report indeterminate")
		}
	}
}
