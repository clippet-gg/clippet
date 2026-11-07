package transcode

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

// h264mp4 is a source that satisfies every CHEAP copy condition, so a table
// case only has to state the one thing it means to break.
func h264mp4() *ProbeResult {
	return &ProbeResult{
		VideoIndex: 0, AudioIndexes: []int{1}, AudioCodecs: []string{"aac"},
		CodedW: 1280, CodedH: 720, DisplayW: 1280, DisplayH: 720, SARNum: 1, SARDen: 1,
		FPS: Rational{30, 1}, HasDuration: true, DurationUS: 6_000_000,
		VideoCodec: "h264", PixFmt: "yuv420p", FormatName: "mov,mp4,m4a,3gp,3g2,mj2",
	}
}

func planFor(p *ProbeResult) Plan { return MakePlan(p, PlanOptions{}) }

var sixSecondKeys = []float64{0, 1, 2, 3, 4, 5}

func TestParseKeyframes(t *testing.T) {
	raw := []byte(`{"packets":[
	  {"pts_time":"0.000000","flags":"K__"},
	  {"pts_time":"0.033333","flags":"___"},
	  {"pts_time":"N/A","flags":"K__"},
	  {"pts_time":"1.000000","flags":"K_ "},
	  {"pts_time":"-1.5","flags":"K__"},
	  {"pts_time":"2.500000","flags":"K__"}]}`)
	got, err := ParseKeyframes(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{0, 1, 2.5}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("keyframes = %v, want %v (N/A, non-key and negative timestamps are dropped)", got, want)
	}
	if _, err := ParseKeyframes([]byte("not json")); !IsPermanent(err) {
		t.Errorf("unparsable packet output → %v, want a permanent error", err)
	}
}

func TestKeyframeArgsSelectTheVideoStreamByIndex(t *testing.T) {
	args := strings.Join(KeyframeArgs("/s/in.mp4", 2), " ")
	for _, want := range []string{"-select_streams 2", "-show_entries packet=pts_time,flags", "-print_format json", "/s/in.mp4"} {
		if !strings.Contains(args, want) {
			t.Errorf("KeyframeArgs missing %q: %s", want, args)
		}
	}
	if strings.Contains(args, "-skip_frame") {
		t.Error("KeyframeArgs decodes the file; a demux of the packet flags is enough")
	}
}

func TestDecideTrim(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*ProbeResult)
		keys       []float64
		start, end int64
		wantCopy   bool
		wantReason string
		wantWindow Window
	}{
		{
			name: "keyframe aligned copy", keys: sixSecondKeys, start: 1000, end: 3000,
			wantCopy: true, wantWindow: Window{Start: 1, Duration: 2},
		},
		{
			// The end of the file is as clean a cut as a keyframe is.
			name: "copy to the end of the source", keys: sixSecondKeys, start: 2000, end: 6000,
			wantCopy: true, wantWindow: Window{Start: 2, Duration: 4},
		},
		{
			// Within one frame: the WINDOW must snap to the keyframe, never
			// to the request — `-ss` on a copy rewinds, it does not seek.
			name: "within one frame of a keyframe snaps to it", keys: sixSecondKeys, start: 1020, end: 3000,
			wantCopy: true, wantWindow: Window{Start: 1, Duration: 2},
		},
		{
			name: "start off the keyframe grid", keys: sixSecondKeys, start: 1500, end: 3000,
			wantReason: ReasonStartNotKey, wantWindow: Window{Start: 1.5, Duration: 1.5},
		},
		{
			name: "end off the keyframe grid", keys: sixSecondKeys, start: 1000, end: 3500,
			wantReason: ReasonEndNotKey, wantWindow: Window{Start: 1, Duration: 2.5},
		},
		{
			name: "no keyframes probed yet", keys: nil, start: 1000, end: 3000,
			wantReason: ReasonNoKeyframes, wantWindow: Window{Start: 1, Duration: 2},
		},
		{
			name: "webm", mutate: func(p *ProbeResult) { p.FormatName = "matroska,webm" },
			keys: sixSecondKeys, start: 1000, end: 3000, wantReason: ReasonNotMP4,
		},
		{
			name: "vp9", mutate: func(p *ProbeResult) { p.VideoCodec = "vp9" },
			keys: sixSecondKeys, start: 1000, end: 3000, wantReason: ReasonNotH264,
		},
		{
			name: "10-bit", mutate: func(p *ProbeResult) { p.PixFmt = "yuv420p10le" },
			keys: sixSecondKeys, start: 1000, end: 3000, wantReason: ReasonBadPixFmt,
		},
		{
			// 8-bit yuv420p H.264 that still claims PQ — some capture
			// hardware writes exactly this. A copy would carry the HDR tag
			// into a rendition that promises Rec.709 (§20 erratum 93).
			name: "8-bit but tagged PQ", mutate: func(p *ProbeResult) { p.ColorTransfer = "smpte2084" },
			keys: sixSecondKeys, start: 1000, end: 3000, wantReason: ReasonHDR,
		},
		{
			name: "8-bit but tagged HLG", mutate: func(p *ProbeResult) { p.ColorTransfer = "arib-std-b67" },
			keys: sixSecondKeys, start: 1000, end: 3000, wantReason: ReasonHDR,
		},
		{
			// An ordinary SDR tag is no obstacle at all.
			name: "bt709 tagged source still copies", mutate: func(p *ProbeResult) { p.ColorTransfer = "bt709" },
			keys: sixSecondKeys, start: 1000, end: 3000, wantCopy: true, wantWindow: Window{Start: 1, Duration: 2},
		},
		{
			name: "two audio tracks are merged by the plan, never copied",
			mutate: func(p *ProbeResult) {
				p.AudioIndexes = []int{1, 2}
				p.AudioCodecs = []string{"aac", "aac"}
			},
			keys: sixSecondKeys, start: 1000, end: 3000, wantReason: ReasonAudioShape,
		},
		{
			name:   "mp3 audio",
			mutate: func(p *ProbeResult) { p.AudioCodecs = []string{"mp3"} },
			keys:   sixSecondKeys, start: 1000, end: 3000, wantReason: ReasonAudioShape,
		},
		{
			name: "no audio at all is copyable",
			mutate: func(p *ProbeResult) {
				p.AudioIndexes = nil
				p.AudioCodecs = nil
			},
			keys: sixSecondKeys, start: 1000, end: 3000, wantCopy: true, wantWindow: Window{Start: 1, Duration: 2},
		},
		{
			name: "1080p needs a rescale",
			mutate: func(p *ProbeResult) {
				p.CodedW, p.CodedH, p.DisplayW, p.DisplayH = 1920, 1080, 1920, 1080
			},
			keys: sixSecondKeys, start: 1000, end: 3000, wantReason: ReasonNeedsScale,
		},
		{
			name:   "120fps is capped by the plan",
			mutate: func(p *ProbeResult) { p.FPS = Rational{120, 1} },
			keys:   sixSecondKeys, start: 1000, end: 3000, wantReason: ReasonNeedsFPS,
		},
		{
			name:   "a rotated source",
			mutate: func(p *ProbeResult) { p.Rotation = 90 },
			keys:   sixSecondKeys, start: 1000, end: 3000, wantReason: ReasonRotated,
		},
		{
			name:   "anamorphic",
			mutate: func(p *ProbeResult) { p.SARNum, p.SARDen = 16, 11 },
			keys:   sixSecondKeys, start: 1000, end: 3000, wantReason: ReasonAnamorphic,
		},
		{
			name:   "no declared duration",
			mutate: func(p *ProbeResult) { p.HasDuration, p.DurationUS = false, 0 },
			keys:   sixSecondKeys, start: 1000, end: 3000, wantReason: ReasonNoDuration,
		},
		{
			name: "an empty window is never a copy", keys: sixSecondKeys, start: 2000, end: 2000,
			wantReason: ReasonWindowInvalid,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := h264mp4()
			if c.mutate != nil {
				c.mutate(p)
			}
			// A mutated probe must be planned AFTER the mutation: the plan is
			// what "needs a rescale" and "changes the frame rate" compare to.
			got := DecideTrim(p, planFor(p), c.keys, c.start, c.end)
			if got.Copy != c.wantCopy {
				t.Fatalf("Copy = %v (reason %q), want %v", got.Copy, got.Reason, c.wantCopy)
			}
			if !c.wantCopy && got.Reason != c.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, c.wantReason)
			}
			if c.wantWindow != (Window{}) && !sameWindow(got.Window, c.wantWindow) {
				t.Errorf("window = %+v, want %+v", got.Window, c.wantWindow)
			}
			wantPath := "reencode"
			if c.wantCopy {
				wantPath = "copy"
			}
			if got.Path() != wantPath {
				t.Errorf("Path() = %q, want %q", got.Path(), wantPath)
			}
		})
	}
}

func sameWindow(a, b Window) bool {
	return math.Abs(a.Start-b.Start) < 1e-6 && math.Abs(a.Duration-b.Duration) < 1e-6
}

func TestCopyArgsProduceTheTranscodeShape(t *testing.T) {
	p := h264mp4()
	tp := DecideTrim(p, planFor(p), sixSecondKeys, 1000, 3000)
	if !tp.Copy {
		t.Fatalf("expected the copy path, got %q", tp.Reason)
	}
	args := tp.CopyArgs(p, "/s/in.mp4", "/s/out.mp4")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-ss 1.000000 -protocol_whitelist file -i /s/in.mp4", // the seek is IN FRONT of the input
		"-map 0:0 -map 0:1", // exactly one video and one audio stream
		"-sn -dn -map_metadata -1 -map_chapters -1",
		"-t 2.000000",
		"-c copy",
		"-movflags +faststart -f mp4 /s/out.mp4",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("CopyArgs missing %q:\n%s", want, joined)
		}
	}
	// The audio map must disappear with the audio, not become "0:-1".
	p.AudioIndexes, p.AudioCodecs = nil, nil
	if got := strings.Join(tp.CopyArgs(p, "in", "out"), " "); strings.Count(got, "-map ") != 1 {
		t.Errorf("a silent source must be mapped once: %s", got)
	}
}

func TestEncodeArgsCarryTheTrimWindow(t *testing.T) {
	p := MakePlan(h264mp4(), PlanOptions{})
	o := EncodeOptions{Preset: "veryfast", CRF: 23}
	plain := strings.Join(p.EncodeArgs("in.mp4", "out.mp4", o), " ")
	if strings.Contains(plain, "-ss") || strings.Contains(plain, " -t ") {
		t.Fatalf("a transcode's argv must be untouched by the trim option:\n%s", plain)
	}
	o.Trim = &Window{Start: 1.25, Duration: 2}
	args := p.EncodeArgs("in.mp4", "out.mp4", o)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-ss 1.250000 -protocol_whitelist file -i in.mp4") {
		t.Errorf("the seek must sit in front of -i (accurate AND fast):\n%s", joined)
	}
	if !strings.Contains(joined, "-t 2.000000") {
		t.Errorf("the window's length must be an output -t:\n%s", joined)
	}
	// -t must be an OUTPUT option: after the input, never before it.
	if idxOf(args, "-t") < idxOf(args, "-i") {
		t.Errorf("-t landed in front of -i, where it would mean something else:\n%s", joined)
	}
}

func idxOf(args []string, flag string) int {
	for i, a := range args {
		if a == flag {
			return i
		}
	}
	return -1
}
