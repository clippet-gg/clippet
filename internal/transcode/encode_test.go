package transcode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var stdOpts = EncodeOptions{Preset: "veryfast", CRF: 23, StallTimeout: 5 * time.Minute, HardTimeout: 6 * time.Hour}

func basePlan(mode AudioMode, audio []int) Plan {
	return Plan{W: 1280, H: 720, FPS: Rational{30, 1}, GOP: 60, KeyintMin: 30, AudioMode: mode,
		VideoIndex: 0, AudioIndexes: audio, HasDuration: true, DurationUS: 6_000_000}
}

// TestEncodeArgsGoldens pins the §10.3 argv for none/single/merge. The
// merge golden contains no amerge (§20) and no argv contains -level:v.
func TestEncodeArgsGoldens(t *testing.T) {
	video := "[0:0]scale=1280:720:flags=lanczos,setsar=1,format=yuv420p" + colourStamp + "[v]"
	head := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-nostats", "-progress", "pipe:1", "-y", "-protocol_whitelist", "file", "-i", "in.mp4", "-filter_complex"}
	mid := []string{"-sn", "-dn", "-map_metadata", "-1", "-map_chapters", "-1", "-r", "30/1", "-fps_mode", "cfr",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-profile:v", "high", "-pix_fmt", "yuv420p",
		"-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709", "-g", "60", "-keyint_min", "30"}
	audio := []string{"-c:a", "aac", "-b:a", "128k", "-ar", "48000", "-ac", "2"}
	tail := []string{"-max_muxing_queue_size", "4096", "-movflags", "+faststart", "-f", "mp4", "out.mp4"}
	join := func(parts ...[]string) []string {
		var out []string
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	cases := []struct {
		name string
		plan Plan
		want []string
	}{
		{"none", basePlan(AudioNone, nil),
			join(head, []string{video, "-map", "[v]", "-an"}, mid, tail)},
		{"single", basePlan(AudioSingle, []int{1}),
			join(head, []string{video + ";[0:1]aresample=48000[a]", "-map", "[v]", "-map", "[a]"}, mid, audio, tail)},
		{"merge", basePlan(AudioMerge, []int{1, 2}),
			join(head, []string{video +
				";[0:1]aresample=48000,aformat=channel_layouts=stereo[a0]" +
				";[0:2]aresample=48000,aformat=channel_layouts=stereo[a1]" +
				";[a0][a1]amix=inputs=2:duration=longest:normalize=0,alimiter=limit=0.95:level=false:latency=true[a]",
				"-map", "[v]", "-map", "[a]"}, mid, audio, tail)},
		// Five tracks merge exactly like two: one aresample+aformat per
		// input, one amix, one alimiter (§20 erratum 93 raised the cap to 8).
		{"merge-5", basePlan(AudioMerge, []int{1, 2, 3, 4, 5}),
			join(head, []string{video +
				";[0:1]aresample=48000,aformat=channel_layouts=stereo[a0]" +
				";[0:2]aresample=48000,aformat=channel_layouts=stereo[a1]" +
				";[0:3]aresample=48000,aformat=channel_layouts=stereo[a2]" +
				";[0:4]aresample=48000,aformat=channel_layouts=stereo[a3]" +
				";[0:5]aresample=48000,aformat=channel_layouts=stereo[a4]" +
				";[a0][a1][a2][a3][a4]amix=inputs=5:duration=longest:normalize=0,alimiter=limit=0.95:level=false:latency=true[a]",
				"-map", "[v]", "-map", "[a]"}, mid, audio, tail)},
		// A tag-rotated source is turned explicitly, in front of the scale.
		{"tag-rotated", tagRotated(basePlan(AudioNone, nil)),
			join(head, []string{"[0:0]transpose=1,scale=720:1280:flags=lanczos,setsar=1,format=yuv420p" + colourStamp + "[v]", "-map", "[v]", "-an"}, mid, tail)},
		// An HDR source is tone mapped in front of everything else — and the
		// output is tagged bt709 either way, which `mid` already pins.
		{"tonemap", tonemapped(basePlan(AudioNone, nil)),
			join(head, []string{"[0:0]" + tonemapChain + "scale=1280:720:flags=lanczos,setsar=1,format=yuv420p" + colourStamp + "[v]", "-map", "[v]", "-an"}, mid, tail)},
		// Tone map AND a legacy rotate tag: the map runs on the full-size
		// linear signal first, then the turn, then the scale.
		{"tonemap+tag-rotated", tonemapped(tagRotated(basePlan(AudioNone, nil))),
			join(head, []string{"[0:0]" + tonemapChain + "transpose=1,scale=720:1280:flags=lanczos,setsar=1,format=yuv420p" + colourStamp + "[v]", "-map", "[v]", "-an"}, mid, tail)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.plan.EncodeArgs("in.mp4", "out.mp4", stdOpts)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("argv mismatch\n got: %q\nwant: %q", got, c.want)
			}
			all := strings.Join(got, " ")
			if strings.Contains(all, "amerge") || strings.Contains(all, "-level") {
				t.Fatalf("argv must contain neither amerge nor -level: %s", all)
			}
			if got[1] != "-nostdin" {
				t.Fatal("-nostdin is mandatory")
			}
		})
	}
	// Four merge inputs, fractional fps, attached-pic video index and a
	// non-default preset/crf all render.
	p := Plan{W: 720, H: 1280, FPS: Rational{30000, 1001}, GOP: 60, KeyintMin: 30, AudioMode: AudioMerge, VideoIndex: 1, AudioIndexes: []int{2, 3, 4, 5}}
	fc := p.FilterComplex()
	if !strings.HasPrefix(fc, "[0:1]scale=720:1280:") || !strings.Contains(fc, "[a0][a1][a2][a3]amix=inputs=4:") {
		t.Fatalf("FilterComplex = %s", fc)
	}
	args := strings.Join(p.EncodeArgs("i", "o", EncodeOptions{Preset: "slow", CRF: 18}), " ")
	if !strings.Contains(args, "-r 30000/1001 ") || !strings.Contains(args, "-preset slow -crf 18 ") {
		t.Fatalf("args = %s", args)
	}
	// The turn is in the legacy tag's clockwise convention; anything but a
	// quarter turn renders nothing.
	for turn, want := range map[int]string{0: "", 90: "transpose=1,", 180: "hflip,vflip,", 270: "transpose=2,", 45: "", -90: "", 360: ""} {
		if got := rotateFilter(turn); got != want {
			t.Errorf("rotateFilter(%d) = %q, want %q", turn, got, want)
		}
	}
}

// tagRotated is a 720x1280 plan for a tag-rotated (rotate=90) landscape source.
func tagRotated(p Plan) Plan {
	p.W, p.H, p.PreRotate = 720, 1280, 90
	return p
}

// tonemapped is the same plan for an HDR source.
func tonemapped(p Plan) Plan {
	p.Tonemap = true
	return p
}

// TestTonemapChainShape pins the tone map's ORDER and its exact filters: the
// map is a curve on light, so the source's transfer function is undone into
// linear light BEFORE the curve and Rec.709 is imposed AFTER it, and the
// whole thing runs at full resolution in front of the scale. Getting the
// order wrong still produces a picture — a wrong one, quietly.
func TestTonemapChainShape(t *testing.T) {
	if tonemapFilter(false) != "" {
		t.Fatalf("an SDR plan must add nothing: %q", tonemapFilter(false))
	}
	got := tonemapFilter(true)
	if got != tonemapChain || !strings.HasSuffix(got, ",") {
		t.Fatalf("tonemapFilter(true) = %q", got)
	}
	want := []string{
		"zscale=t=linear:npl=100",
		"format=gbrpf32le",
		"tonemap=tonemap=mobius:desat=0",
		"zscale=p=bt709:t=bt709:m=bt709:r=tv",
		"format=yuv420p",
	}
	if parts := strings.Split(strings.TrimSuffix(got, ","), ","); !reflect.DeepEqual(parts, want) {
		t.Fatalf("chain = %q, want %q", parts, want)
	}
	// And it is a PREFIX of the video chain, ahead of rotate and scale.
	fc := tonemapped(tagRotated(basePlan(AudioNone, nil))).FilterComplex()
	map0, rot := strings.Index(fc, "tonemap="), strings.Index(fc, "transpose=1")
	scale := strings.Index(fc, "scale=720:1280")
	if map0 < 0 || rot < 0 || scale < 0 || !(map0 < rot && rot < scale) {
		t.Fatalf("tone map must precede the turn and the scale: %s", fc)
	}
}

func TestParseProgressLine(t *testing.T) {
	cases := []struct {
		line string
		want ProgressLine
		ok   bool
	}{
		{"out_time_us=1500000", ProgressLine{OutTimeUS: 1_500_000, HasOutTime: true}, true},
		{"out_time_ms=1500000", ProgressLine{OutTimeUS: 1_500_000, HasOutTime: true}, true},
		{"out_time=00:00:01.500000", ProgressLine{OutTimeUS: 1_500_000, HasOutTime: true}, true},
		{"out_time=01:02:03.250000", ProgressLine{OutTimeUS: 3_723_250_000, HasOutTime: true}, true},
		{"out_time=-00:00:00.033000", ProgressLine{OutTimeUS: 0, HasOutTime: true}, true},
		{"out_time_us=-9223372036854775808", ProgressLine{OutTimeUS: 0, HasOutTime: true}, true},
		{"out_time_us=N/A", ProgressLine{}, false},
		{"out_time=garbage", ProgressLine{}, false},
		{"frame=42", ProgressLine{Frame: 42}, true},
		{"frame=-1", ProgressLine{}, false},
		{"total_size=1048624", ProgressLine{TotalSize: 1_048_624, HasTotalSize: true}, true},
		{"total_size=N/A", ProgressLine{}, false},
		{"total_size=-1", ProgressLine{}, false},
		{"progress=continue", ProgressLine{}, true},
		{"progress=end", ProgressLine{End: true}, true},
		{"progress=end\r", ProgressLine{End: true}, true},
		{"fps=29.97", ProgressLine{}, false},
		{"speed=1.5x", ProgressLine{}, false},
		{"", ProgressLine{}, false},
		{"novalue", ProgressLine{}, false},
	}
	for _, c := range cases {
		got, ok := ParseProgressLine(c.line)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseProgressLine(%q) = %+v, %v; want %+v, %v", c.line, got, ok, c.want, c.ok)
		}
	}
}

// feed pushes lines through a tracker and returns every reported fraction.
func feed(p Plan, lines ...string) (fracs []float64, indet []bool) {
	tr := newProgressTracker(p, func(f float64, i bool) { fracs = append(fracs, f); indet = append(indet, i) }, nil)
	for _, l := range lines {
		tr.line(l)
	}
	return fracs, indet
}

func TestProgressTracker(t *testing.T) {
	p := basePlan(AudioNone, nil) // 6s at 30 fps
	t.Run("out_time_us preferred over later keys", func(t *testing.T) {
		fr, _ := feed(p, "frame=1", "out_time_us=3000000", "out_time_ms=6000000", "out_time=00:00:06.000000", "progress=continue")
		if len(fr) != 1 || fr[0] != 0.5 {
			t.Fatalf("fracs = %v", fr)
		}
	})
	t.Run("out_time_ms and out_time fallbacks", func(t *testing.T) {
		fr, _ := feed(p, "out_time_ms=1500000", "progress=continue", "out_time=00:00:03.000000", "progress=continue")
		if !reflect.DeepEqual(fr, []float64{0.25, 0.5}) {
			t.Fatalf("fracs = %v", fr)
		}
	})
	t.Run("frame fallback uses the OUTPUT rate", func(t *testing.T) {
		// 60 fps output plan: 90 frames of a 6s clip = 25%, not 50%.
		p60 := p
		p60.FPS = Rational{60, 1}
		fr, _ := feed(p60, "frame=90", "progress=continue")
		if len(fr) != 1 || fr[0] != 0.25 {
			t.Fatalf("fracs = %v", fr)
		}
	})
	t.Run("clamp and end", func(t *testing.T) {
		fr, indet := feed(p, "out_time_us=9000000", "progress=continue", "out_time_us=0", "progress=continue", "progress=end")
		if !reflect.DeepEqual(fr, []float64{1, 1, 1}) || indet[0] || indet[2] {
			t.Fatalf("fracs = %v indet = %v", fr, indet)
		}
	})
	t.Run("monotone within a run", func(t *testing.T) {
		fr, _ := feed(p, "out_time_us=3000000", "progress=continue", "out_time_us=1000000", "progress=continue", "out_time_us=4500000", "progress=continue")
		if !reflect.DeepEqual(fr, []float64{0.5, 0.5, 0.75}) {
			t.Fatalf("fracs = %v", fr)
		}
	})
	t.Run("unknown duration is indeterminate", func(t *testing.T) {
		pu := p
		pu.HasDuration, pu.DurationUS = false, 0
		fr, indet := feed(pu, "out_time_us=3000000", "progress=continue", "progress=end")
		if !reflect.DeepEqual(fr, []float64{0, 1}) || !indet[0] || !indet[1] {
			t.Fatalf("fracs = %v indet = %v", fr, indet)
		}
	})
	t.Run("advancement detection drives the stall watchdog", func(t *testing.T) {
		var got []bool
		tr := newProgressTracker(p, func(float64, bool) {}, func(adv bool, _ float64, _ bool) { got = append(got, adv) })
		for _, l := range []string{"out_time_us=100", "progress=continue", "out_time_us=100", "progress=continue", "out_time_us=200", "progress=continue", "frame=5", "progress=continue"} {
			tr.line(l)
		}
		if !reflect.DeepEqual(got, []bool{true, false, true, true}) {
			t.Fatalf("advanced = %v", got)
		}
	})
	t.Run("growing total_size advances while the video clock stands still", func(t *testing.T) {
		// An audio tail past the last video frame: ffmpeg 7 repeats
		// frame/out_time_us and only total_size grows (measured).
		var got []bool
		tr := newProgressTracker(p, func(float64, bool) {}, func(adv bool, _ float64, _ bool) { got = append(got, adv) })
		for _, l := range []string{
			"frame=21", "out_time_us=633333", "total_size=48", "progress=continue",
			"frame=21", "out_time_us=633333", "total_size=524336", "progress=continue",
			"frame=21", "out_time_us=633333", "total_size=524336", "progress=continue",
			"frame=21", "out_time_us=633333", "total_size=1048624", "progress=continue",
		} {
			tr.line(l)
		}
		if !reflect.DeepEqual(got, []bool{true, true, false, true}) {
			t.Fatalf("advanced = %v", got)
		}
	})
}

func TestStallRule(t *testing.T) {
	for _, c := range []struct {
		has  bool
		frac float64
		end  bool
		want bool
	}{
		{true, 0, false, true}, {true, 0.5, false, true}, {true, 0.989, false, true},
		{true, 0.99, false, false}, {true, 1, false, false}, {true, 0.5, true, false},
		{false, 0, false, false}, {false, 0.5, false, false},
	} {
		if got := stallArmed(c.has, c.frac, c.end); got != c.want {
			t.Errorf("stallArmed(%v, %v, %v) = %v, want %v", c.has, c.frac, c.end, got, c.want)
		}
	}
	// The watchdog itself, on the fake clock (§20 erratum 331): every "silence"
	// below is an Advance, so the verdicts are exact and no load can move them.
	const timeout = time.Minute
	waitFired := func(fired <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-fired:
		case <-time.After(fakeBound):
			t.Fatalf("%s within %v of wall time — %s", what, fakeBound, ignoredClock)
		}
	}
	// Fires once when armed and starved: one timeout of silence from start.
	fc := newFakeClock()
	fired := make(chan struct{}, 1)
	w := newStallWatchdog(timeout, true, func() { fired <- struct{}{} }, fc)
	w.start()
	if n := fc.Advance(timeout - time.Nanosecond); n != 0 {
		t.Fatalf("the timer fired %d time(s) a nanosecond early", n)
	}
	if n := fc.Advance(time.Nanosecond); n != 1 {
		t.Fatalf("the timer fired %d time(s) at the timeout, want 1", n)
	}
	waitFired(fired, "watchdog did not fire")
	w.stop()
	// Disarms past 99%: the block stops the timer, and an hour of silence fires
	// nothing.
	fc = newFakeClock()
	w = newStallWatchdog(timeout, true, func() { t.Error("fired after 99%") }, fc)
	w.start()
	w.observe(true, 0.995, false)
	if s := fc.state(); s.armed != 0 || s.stops != 1 {
		t.Fatalf("a 99.5%% block left the timer armed: %+v", s)
	}
	if n := fc.Advance(time.Hour); n != 0 {
		t.Fatalf("%d timer(s) fired after 99%%", n)
	}
	w.stop()
	// A zero timeout never arms at all.
	fc = newFakeClock()
	w = newStallWatchdog(0, true, func() { t.Error("fired with timeout 0") }, fc)
	w.start()
	if s := fc.state(); s.timers != 0 {
		t.Fatalf("timeout 0 made a timer: %+v", s)
	}
	w.stop()
	// Keep-alive: advancing blocks inside the timeout hold it, however many of
	// them; once they stop it fires exactly one timeout after the last.
	fc = newFakeClock()
	fired = make(chan struct{}, 1)
	w = newStallWatchdog(timeout, true, func() { fired <- struct{}{} }, fc)
	w.start()
	for i := 0; i < 10; i++ {
		if n := fc.Advance(timeout / 2); n != 0 {
			t.Fatalf("block %d: the timer fired while blocks were advancing", i)
		}
		w.observe(true, 0.1, false)
	}
	select {
	case <-fired:
		t.Fatal("watchdog fired while blocks were advancing")
	default:
	}
	if n := fc.Advance(timeout); n != 1 {
		t.Fatalf("the timer fired %d time(s) one timeout after the last advance, want 1", n)
	}
	waitFired(fired, "watchdog did not fire after the advances stopped")
	w.stop()
}

// The loop honours an advance that landed while it was consuming a fire: the
// timer fired, but `last` moved, so it waits out the remainder instead of
// firing (the re-check in stallWatchdog.loop). Driven by the clock alone.
func TestStallWatchdogHonoursALateAdvance(t *testing.T) {
	const timeout = time.Minute
	fc := newFakeClock()
	fired := make(chan struct{}, 1)
	w := newStallWatchdog(timeout, true, func() { fired <- struct{}{} }, fc)
	w.start()
	defer w.stop()
	fc.Advance(timeout / 2)
	// Move `last` forward WITHOUT the Reset an observe would do: the timer still
	// fires at the original deadline, and the loop must see that the silence is
	// only half a timeout long.
	w.mu.Lock()
	w.last = fc.Now()
	w.mu.Unlock()
	before := fc.state().resets
	if n := fc.Advance(timeout / 2); n != 1 {
		t.Fatalf("fired %d timer(s) at the first deadline, want 1", n)
	}
	fc.await(t, "the loop re-arms for the remainder", func(s fakeState) bool { return s.resets > before })
	select {
	case <-fired:
		t.Fatal("fired on a stale deadline")
	default:
	}
	if n := fc.Advance(timeout / 2); n != 1 {
		t.Fatalf("fired %d timer(s) at the remainder, want 1", n)
	}
	select {
	case <-fired:
	case <-time.After(fakeBound):
		t.Fatalf("did not fire once the remainder was silent too within %v of wall time — %s", fakeBound, ignoredClock)
	}
}

func TestRingAndLineWriter(t *testing.T) {
	r := newRing(8)
	_, _ = r.Write([]byte("abc"))
	if r.String() != "abc" {
		t.Fatalf("ring = %q", r.String())
	}
	_, _ = r.Write([]byte("defgh")) // exactly full
	if r.String() != "abcdefgh" {
		t.Fatalf("ring full = %q", r.String())
	}
	_, _ = r.Write([]byte("ij"))
	if r.String() != "cdefghij" || r.Tail(3) != "hij" {
		t.Fatalf("ring wrapped = %q tail=%q", r.String(), r.Tail(3))
	}
	_, _ = r.Write([]byte("0123456789ABCDEF"))
	if r.String() != "89ABCDEF" {
		t.Fatalf("ring oversized write = %q", r.String())
	}
	r = newRing(8)
	_, _ = r.Write([]byte("xé"))
	if r.Tail(2) != "é" && r.Tail(2) != "" { // never a torn rune
		t.Fatalf("Tail on a rune boundary = %q", r.Tail(2))
	}

	var lines []string
	lw := newLineWriter(func(s string) { lines = append(lines, s) })
	_, _ = lw.Write([]byte("a=1\r\nb="))
	_, _ = lw.Write([]byte("2\nc=3"))
	if !reflect.DeepEqual(lines, []string{"a=1", "b=2"}) {
		t.Fatalf("lines = %q", lines)
	}
	_, _ = lw.Write([]byte("\n"))
	if len(lines) != 3 || lines[2] != "c=3" {
		t.Fatalf("lines = %q", lines)
	}
}

// fakeFFmpeg writes a shell script that stands in for ffmpeg; the script's
// stdout is parsed as -progress output and its exit status classified.
func fakeFFmpeg(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ffmpeg needs /bin/sh")
	}
	path := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// blocks prints n progress blocks 1..n out of n seconds, then progress=end.
const progressScript = `
i=1; while [ $i -le 6 ]; do
  echo "frame=$((i*30))"; echo "fps=30.0"; echo "out_time_us=$((i*1000000))"; echo "out_time_ms=$((i*1000000))"; echo "progress=continue"
  i=$((i+1))
done
echo "progress=end"
exit 0
`

func TestEncodeSuccessAndProgress(t *testing.T) {
	ff := fakeFFmpeg(t, progressScript)
	var mu sync.Mutex
	var fracs []float64
	err := Encode(context.Background(), ff, basePlan(AudioNone, nil), "in", "out", stdOpts, func(f float64, ind bool) {
		mu.Lock()
		defer mu.Unlock()
		if ind {
			t.Error("indeterminate with a known duration")
		}
		fracs = append(fracs, f)
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	want := []float64{1.0 / 6, 2.0 / 6, 0.5, 4.0 / 6, 5.0 / 6, 1, 1}
	if len(fracs) != len(want) {
		t.Fatalf("fracs = %v", fracs)
	}
	for i := range want {
		if d := fracs[i] - want[i]; d > 1e-9 || d < -1e-9 {
			t.Fatalf("fracs = %v, want %v", fracs, want)
		}
	}
}

func TestEncodeNonZeroExit(t *testing.T) {
	ff := fakeFFmpeg(t, "echo 'progress=continue'; echo 'boring line' >&2; echo 'Error: conversion failed!' >&2; exit 3\n")
	err := Encode(context.Background(), ff, basePlan(AudioNone, nil), "in", "out", stdOpts, nil)
	if !IsPermanent(err) {
		t.Fatalf("non-zero exit must be permanent: %v", err)
	}
	if !strings.Contains(err.Error(), "ffmpeg exited 3") || !strings.Contains(err.Error(), "conversion failed") {
		t.Fatalf("reason = %v", err)
	}
}

// TestEncodeStall, TestEncodeStallDisabledPast99Percent and
// TestEncodeUnknownDurationNeverStalls live in stall_unix_test.go since §20
// erratum 331: they feed the progress lines through a FIFO the test writes and
// drive the stall timeout on a fake clock, so no "silence" is a sleep.

func TestEncodeHardTimeout(t *testing.T) {
	ff := fakeFFmpeg(t, "echo 'progress=continue'; exec sleep 30\n")
	o := stdOpts
	o.StallTimeout = 0
	o.HardTimeout = 200 * time.Millisecond
	err := Encode(context.Background(), ff, basePlan(AudioNone, nil), "in", "out", o, nil)
	if !IsPermanent(err) || !strings.Contains(err.Error(), "encode_timeout") {
		t.Fatalf("hard timeout: err = %v", err)
	}
}

// The output budget kills a run that writes past it (§20 erratum 46): the
// CFR fill of a source whose timestamps run far past its bytes is bounded by
// nothing else once the duration is unknown (no stall watchdog) or a lie.
func TestEncodeOutputBudget(t *testing.T) {
	ff := fakeFFmpeg(t, "echo 'total_size=1000'; echo 'progress=continue'; echo 'total_size=100000'; echo 'progress=continue'; exec sleep 30\n")
	o := stdOpts
	o.MaxOutputBytes = 50_000
	p := basePlan(AudioNone, nil)
	p.HasDuration, p.DurationUS = false, 0
	start := time.Now()
	err := Encode(context.Background(), ff, p, "in", "out", o, nil)
	if !IsPermanent(err) || !strings.Contains(err.Error(), "output_too_large") {
		t.Fatalf("budget: err = %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("runaway ffmpeg not reaped promptly: %s", time.Since(start))
	}
}

// The cap is a ceiling, not a target: writing exactly the budget succeeds,
// and a zero budget disables the check altogether.
func TestEncodeOutputBudgetBoundary(t *testing.T) {
	ff := fakeFFmpeg(t, "echo 'total_size=50000'; echo 'progress=end'; exit 0\n")
	o := stdOpts
	o.MaxOutputBytes = 50_000
	if err := Encode(context.Background(), ff, basePlan(AudioNone, nil), "in", "out", o, nil); err != nil {
		t.Fatalf("exactly the budget must pass: %v", err)
	}
	o.MaxOutputBytes = 0
	if err := Encode(context.Background(), ff, basePlan(AudioNone, nil), "in", "out", o, nil); err != nil {
		t.Fatalf("no budget must not cap: %v", err)
	}
}

func TestEncodeCancelledIsNotAFailure(t *testing.T) {
	ff := fakeFFmpeg(t, "echo 'progress=continue'; exec sleep 30\n")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	err := Encode(ctx, ff, basePlan(AudioNone, nil), "in", "out", stdOpts, nil)
	if !errors.Is(err, context.Canceled) || IsPermanent(err) {
		t.Fatalf("cancel: err = %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("cancelled ffmpeg not reaped: %s", time.Since(start))
	}
}

func TestEncodeMissingBinary(t *testing.T) {
	err := Encode(context.Background(), filepath.Join(t.TempDir(), "nope"), basePlan(AudioNone, nil), "in", "out", stdOpts, nil)
	if err == nil || IsPermanent(err) {
		t.Fatalf("missing binary must be a plain error: %v", err)
	}
}
