package transcode

import (
	"strings"
	"testing"
)

// The §17 #3 encode (the card file) and §20 erratum 125 (interlaced
// sources), as pure functions: what the plan decides and what the argv says.

func TestSocialPlanOptionsCapTheFrameRate(t *testing.T) {
	for _, tc := range []struct {
		name string
		box  PlanOptions
		want PlanOptions
	}{
		{"defaults", PlanOptions{}, PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 30}},
		{"the instance box", PlanOptions{MaxW: 1920, MaxH: 1080, MaxFPS: 60}, PlanOptions{MaxW: 1920, MaxH: 1080, MaxFPS: 30}},
		// An operator who asked for a SLOWER rendition than the card's cap
		// keeps theirs: the card file may never be smoother than the file it
		// is derived from.
		{"a slower instance", PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 24}, PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 24}},
	} {
		if got := SocialPlanOptions(tc.box); got != tc.want {
			t.Errorf("%s: SocialPlanOptions(%+v) = %+v, want %+v", tc.name, tc.box, got, tc.want)
		}
	}
}

// TestSocialPlanKeepsTheRenditionsShape: the input is the main rendition, so
// nothing is rescaled — only the frame rate moves. That is what makes the
// social dims equal the rendition's today and why they are still MEASURED
// rather than assumed (erratum 123).
func TestSocialPlanKeepsTheRenditionsShape(t *testing.T) {
	rendition := &ProbeResult{
		VideoIndex: 0, AudioIndexes: []int{1}, CodedW: 1280, CodedH: 720, SARNum: 1, SARDen: 1,
		DisplayW: 1280, DisplayH: 720, FPS: Rational{60, 1}, RFPS: Rational{60, 1},
		HasDuration: true, DurationUS: 6_000_000, VideoCodec: "h264",
	}
	p := MakePlan(rendition, SocialPlanOptions(PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60}))
	if p.W != 1280 || p.H != 720 {
		t.Fatalf("social plan = %dx%d, want the rendition's 1280x720", p.W, p.H)
	}
	if (p.FPS != Rational{30, 1}) {
		t.Fatalf("social fps = %s, want 30/1", p.FPS)
	}
	if p.GOP != 60 || p.KeyintMin != 30 {
		t.Fatalf("GOP/keyint = %d/%d, want 60/30", p.GOP, p.KeyintMin)
	}
	// 29.97 is NOT above 30 and must survive exactly.
	rendition.FPS, rendition.RFPS = Rational{30000, 1001}, Rational{30000, 1001}
	if p := MakePlan(rendition, SocialPlanOptions(PlanOptions{})); (p.FPS != Rational{30000, 1001}) {
		t.Fatalf("29.97 planned as %s, want it unchanged", p.FPS)
	}
}

func TestSocialEncodeOptionsOverrideTheOperators(t *testing.T) {
	base := EncodeOptions{Preset: "slow", CRF: 18, MaxRateBits: 9_000_000, BufSizeBits: 1, AudioBitrate: "320k",
		MaxOutputBytes: 4096, Trim: &Window{Start: 1, Duration: 2}}
	got := SocialEncodeOptions(base)
	switch {
	case got.Preset != "slow":
		t.Errorf("preset = %q; the operator's preset is kept", got.Preset)
	case got.CRF != SocialCRF:
		t.Errorf("CRF = %d, want %d", got.CRF, SocialCRF)
	case got.MaxRateBits != SocialMaxRateBits || got.BufSizeBits != SocialBufSizeBits:
		t.Errorf("VBV = %d/%d, want %d/%d", got.MaxRateBits, got.BufSizeBits, SocialMaxRateBits, SocialBufSizeBits)
	case got.AudioBitrate != SocialAudioBitrate:
		t.Errorf("audio = %q, want %q", got.AudioBitrate, SocialAudioBitrate)
	case got.MaxOutputBytes != 4096:
		t.Errorf("the caller's output budget was dropped: %d", got.MaxOutputBytes)
	case got.Trim != nil:
		t.Errorf("a social encode is never a trim: %+v", got.Trim)
	}
}

// TestEncodeArgsRenderTheVBVPair: TRANSCODE_MAX_BITRATE and the social
// ceiling are the same mechanism (§20 erratum 124), and OFF renders nothing
// at all — every argv this package emitted before this erratum is unchanged.
func TestEncodeArgsRenderTheVBVPair(t *testing.T) {
	p := Plan{W: 1280, H: 720, FPS: Rational{30, 1}, GOP: 60, KeyintMin: 30, AudioMode: AudioSingle, AudioIndexes: []int{1}}
	for _, tc := range []struct {
		name string
		o    EncodeOptions
		want []string // consecutive argv fragments, or nil for "absent"
		none []string
	}{
		{name: "off", o: EncodeOptions{Preset: "veryfast", CRF: 23}, none: []string{"-maxrate", "-bufsize"}},
		{name: "ceiling only, bufsize doubles", o: EncodeOptions{Preset: "veryfast", CRF: 23, MaxRateBits: 2_500_000},
			want: []string{"-maxrate", "2500000", "-bufsize", "5000000"}},
		{name: "both stated", o: EncodeOptions{Preset: "veryfast", CRF: 23, MaxRateBits: 3_000_000, BufSizeBits: 3_000_000},
			want: []string{"-maxrate", "3000000", "-bufsize", "3000000"}},
	} {
		args := p.EncodeArgs("in.mp4", "out.mp4", tc.o)
		joined := " " + strings.Join(args, " ") + " "
		for _, absent := range tc.none {
			if strings.Contains(joined, " "+absent+" ") {
				t.Errorf("%s: %s present in %v", tc.name, absent, args)
			}
		}
		if tc.want != nil && !strings.Contains(joined, " "+strings.Join(tc.want, " ")+" ") {
			t.Errorf("%s: want %v in %v", tc.name, tc.want, args)
		}
		// The VBV pair sits in front of the GOP options, where x264 reads it
		// with the rest of the rate control.
		if tc.want != nil {
			if i, j := indexOf(args, "-maxrate"), indexOf(args, "-g"); i < 0 || j < 0 || i > j {
				t.Errorf("%s: -maxrate at %d, -g at %d", tc.name, i, j)
			}
		}
	}
}

func TestEncodeArgsAudioBitrate(t *testing.T) {
	p := Plan{W: 640, H: 360, FPS: Rational{30, 1}, GOP: 60, KeyintMin: 30, AudioMode: AudioSingle, AudioIndexes: []int{1}}
	dflt := strings.Join(p.EncodeArgs("in", "out", EncodeOptions{Preset: "veryfast", CRF: 23}), " ")
	if !strings.Contains(dflt, "-b:a 128k") {
		t.Fatalf("an unset AudioBitrate must render the §10.3 default: %s", dflt)
	}
	social := strings.Join(p.EncodeArgs("in", "out", SocialEncodeOptions(EncodeOptions{Preset: "veryfast", CRF: 23})), " ")
	if !strings.Contains(social, "-b:a 96k") {
		t.Fatalf("the social encode must render 96k audio: %s", social)
	}
	// No audio, no -b:a, whatever the option says.
	silent := Plan{W: 640, H: 360, FPS: Rational{30, 1}, GOP: 60, KeyintMin: 30, AudioMode: AudioNone}
	if got := strings.Join(silent.EncodeArgs("in", "out", SocialEncodeOptions(EncodeOptions{})), " "); strings.Contains(got, "-b:a") {
		t.Fatalf("a silent plan rendered an audio bitrate: %s", got)
	}
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// --- §20 erratum 125: interlaced sources ------------------------------------

func TestIsInterlacedFieldOrder(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"tt", true}, {"bb", true}, {"tb", true}, {"bt", true}, {"TT", true}, {" tt ", true},
		{"progressive", false}, {"unknown", false}, {"", false}, {"top", false},
	} {
		if got := IsInterlacedFieldOrder(tc.in); got != tc.want {
			t.Errorf("IsInterlacedFieldOrder(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestPlanDeinterlacesOnlyAnInterlacedSource(t *testing.T) {
	base := ProbeResult{VideoIndex: 0, CodedW: 720, CodedH: 480, SARNum: 1, SARDen: 1, DisplayW: 720, DisplayH: 480,
		FPS: Rational{30000, 1001}, HasDuration: true, DurationUS: 6_000_000}
	for _, tc := range []struct {
		order string
		want  bool
	}{{"tt", true}, {"bb", true}, {"progressive", false}, {"", false}} {
		p := base
		p.FieldOrder = tc.order
		plan := MakePlan(&p, PlanOptions{})
		if plan.Deinterlace != tc.want {
			t.Fatalf("field_order=%q → Deinterlace=%v, want %v", tc.order, plan.Deinterlace, tc.want)
		}
		fc := plan.FilterComplex()
		if has := strings.Contains(fc, "yadif=deint=interlaced,"); has != tc.want {
			t.Fatalf("field_order=%q → filter %q", tc.order, fc)
		}
		if tc.want && !strings.HasPrefix(fc, "[0:0]yadif=deint=interlaced,") {
			t.Fatalf("the deinterlacer must be the FIRST filter (before any scale): %q", fc)
		}
	}
}

// TestDeinterlaceGoesInFrontOfTheToneMap: an interlaced HDR source is a
// nonsense file, and the order still has to be defined — fields first,
// because every later filter mixes two moments of time together.
func TestDeinterlaceGoesInFrontOfTheToneMap(t *testing.T) {
	p := &ProbeResult{VideoIndex: 0, CodedW: 1920, CodedH: 1080, SARNum: 1, SARDen: 1, DisplayW: 1920, DisplayH: 1080,
		FPS: Rational{30, 1}, HasDuration: true, DurationUS: 6_000_000,
		FieldOrder: "tt", ColorTransfer: "smpte2084", ColorPrimaries: "bt2020", ColorSpace: "bt2020nc"}
	fc := MakePlan(p, PlanOptions{}).FilterComplex()
	yadif, zscale := strings.Index(fc, "yadif"), strings.Index(fc, "zscale")
	if yadif < 0 || zscale < 0 || yadif > zscale {
		t.Fatalf("yadif at %d, zscale at %d in %q", yadif, zscale, fc)
	}
	if scale := strings.Index(fc, "scale=1280:720"); scale < 0 || yadif > scale {
		t.Fatalf("the deinterlacer must precede the scale: %q", fc)
	}
}

// TestTrimWillNotCopyAnInterlacedSource: a stream copy keeps the coded
// fields, and the rendition this pipeline produces is progressive — so the
// copy path, whose whole premise is "already the shape we would produce", has
// to refuse.
func TestTrimWillNotCopyAnInterlacedSource(t *testing.T) {
	p := &ProbeResult{
		VideoIndex: 0, CodedW: 1280, CodedH: 720, SARNum: 1, SARDen: 1, DisplayW: 1280, DisplayH: 720,
		FPS: Rational{30, 1}, RFPS: Rational{30, 1}, HasDuration: true, DurationUS: 6_000_000,
		VideoCodec: "h264", PixFmt: "yuv420p", FormatName: "mov,mp4,m4a,3gp,3g2,mj2",
		Video: StreamExtent{EndSec: 6, Known: true},
	}
	keys := []float64{0, 1, 2, 3, 4, 5}
	if tp := DecideTrim(p, MakePlan(p, PlanOptions{}), keys, 1000, 3000); !tp.Copy {
		t.Fatalf("the progressive control must copy: %+v", tp)
	}
	p.FieldOrder = "tt"
	tp := DecideTrim(p, MakePlan(p, PlanOptions{}), keys, 1000, 3000)
	if tp.Copy || tp.Reason != ReasonInterlaced {
		t.Fatalf("interlaced trim = %+v, want a re-encode with %q", tp, ReasonInterlaced)
	}
}
