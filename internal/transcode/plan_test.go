package transcode

import (
	"strings"
	"testing"
)

// TestMakePlanGoldens is the §10.3 table verbatim (even() floors; never
// upscale; fit in either orientation).
func TestMakePlanGoldens(t *testing.T) {
	std := PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60}
	cases := []struct {
		name   string
		dw, dh int
		w, h   int
	}{
		{"1920x1080", 1920, 1080, 1280, 720},
		{"1080x1920", 1080, 1920, 720, 1280},
		{"3840x2160", 3840, 2160, 1280, 720},
		{"3840x1600", 3840, 1600, 1280, 532},
		{"640x360", 640, 360, 640, 360},
		{"1366x768", 1366, 768, 1280, 720},
		{"720x576 SAR 16:11 (1047x576)", 1047, 576, 1046, 576},
		{"1280x720 rotation 90 (720x1280)", 720, 1280, 720, 1280},
		{"1281x721", 1281, 721, 1278, 720},
		{"2560x1080", 2560, 1080, 1280, 540},
		{"641x361", 641, 361, 640, 360},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := MakePlan(&ProbeResult{DisplayW: c.dw, DisplayH: c.dh, FPS: Rational{30, 1}}, std)
			if p.W != c.w || p.H != c.h {
				t.Fatalf("MakePlan(%dx%d) = %dx%d, want %dx%d", c.dw, c.dh, p.W, p.H, c.w, c.h)
			}
			if p.W%2 != 0 || p.H%2 != 0 || max(p.W, p.H) > 1280 || min(p.W, p.H) > 720 {
				t.Fatalf("plan %dx%d violates even/box rules", p.W, p.H)
			}
		})
	}
}

// TestMakePlanRotatedDisplayDimsFromProbe runs the SAR/rotation fixtures'
// display dims through the same rule via ParseProbe-shaped inputs.
func TestMakePlanFPSAndGOP(t *testing.T) {
	std := PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60}
	cases := []struct {
		name           string
		in             Rational
		fps            Rational
		gop, keyintMin int
	}{
		{"30/1", Rational{30, 1}, Rational{30, 1}, 60, 30},
		{"30000/1001 stays exact", Rational{30000, 1001}, Rational{30000, 1001}, 60, 30},
		{"60000/1001 is under the cap", Rational{60000, 1001}, Rational{60000, 1001}, 120, 60},
		{"60/1 at the cap", Rational{60, 1}, Rational{60, 1}, 120, 60},
		{"120/1 capped", Rational{120, 1}, Rational{60, 1}, 120, 60},
		{"121/2 (60.5) capped exactly", Rational{121, 2}, Rational{60, 1}, 120, 60},
		{"25/1", Rational{25, 1}, Rational{25, 1}, 50, 25},
		{"invalid falls back to 30", Rational{0, 0}, Rational{30, 1}, 60, 30},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := MakePlan(&ProbeResult{DisplayW: 1280, DisplayH: 720, FPS: c.in}, std)
			if p.FPS != c.fps || p.GOP != c.gop || p.KeyintMin != c.keyintMin {
				t.Fatalf("fps=%v gop=%d keyint=%d, want %v/%d/%d", p.FPS, p.GOP, p.KeyintMin, c.fps, c.gop, c.keyintMin)
			}
		})
	}
}

func TestMakePlanAudioModeAndPassThrough(t *testing.T) {
	cases := []struct {
		audio []int
		mode  AudioMode
		n     int
	}{
		{nil, AudioNone, 0},
		{[]int{1}, AudioSingle, 1},
		{[]int{1, 2}, AudioMerge, 2},
		{[]int{1, 2, 3, 4}, AudioMerge, 4},
		// An OBS scene with game + mic + Discord + music + browser audio is
		// five; the cap is eight (§20 erratum 93), and only a ninth is lost.
		{[]int{1, 2, 3, 4, 5}, AudioMerge, 5},
		{[]int{1, 2, 3, 4, 5, 6, 7, 8}, AudioMerge, 8},
		{[]int{1, 2, 3, 4, 5, 6, 7, 8, 9}, AudioMerge, 8}, // capped
	}
	for _, c := range cases {
		pr := &ProbeResult{VideoIndex: 3, AudioIndexes: c.audio, DisplayW: 640, DisplayH: 360, FPS: Rational{30, 1},
			HasDuration: true, DurationUS: 6_000_000}
		p := MakePlan(pr, PlanOptions{})
		if p.AudioMode != c.mode || len(p.AudioIndexes) != c.n {
			t.Fatalf("audio %v: mode=%v n=%d, want %v/%d", c.audio, p.AudioMode, len(p.AudioIndexes), c.mode, c.n)
		}
		if p.VideoIndex != 3 || !p.HasDuration || p.DurationUS != 6_000_000 {
			t.Fatalf("pass-through fields lost: %+v", p)
		}
		if p.W != 640 || p.H != 360 {
			t.Fatalf("zero PlanOptions should default to 1280x720 box: got %dx%d", p.W, p.H)
		}
	}
	// Mutating the plan's slice must not touch the probe result.
	pr := &ProbeResult{AudioIndexes: []int{1, 2}, DisplayW: 640, DisplayH: 360, FPS: Rational{30, 1}}
	p := MakePlan(pr, PlanOptions{})
	p.AudioIndexes[0] = 99
	if pr.AudioIndexes[0] != 1 {
		t.Fatal("MakePlan aliased the probe's AudioIndexes")
	}
}

// TestMakePlanPreRotate: only a tag-derived rotation becomes an explicit
// turn in the filter graph; a matrix rotation is left to ffmpeg's autorotate.
func TestMakePlanPreRotate(t *testing.T) {
	std := PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60}
	for _, c := range []struct {
		rot     int
		fromTag bool
		want    int
	}{{90, true, 90}, {270, true, 270}, {180, true, 180}, {0, true, 0}, {90, false, 0}, {270, false, 0}} {
		p := MakePlan(&ProbeResult{DisplayW: 720, DisplayH: 1280, Rotation: c.rot, RotationFromTag: c.fromTag, FPS: Rational{30, 1}}, std)
		if p.PreRotate != c.want || p.W != 720 || p.H != 1280 {
			t.Errorf("rotation %d from tag %v: PreRotate=%d %dx%d, want %d 720x1280", c.rot, c.fromTag, p.PreRotate, p.W, p.H, c.want)
		}
	}
}

func TestMakePlanLargerBox(t *testing.T) {
	// A raised box (1920x1080) keeps a 1080p60 source as is and scales 4K.
	box := PlanOptions{MaxW: 1920, MaxH: 1080, MaxFPS: 60}
	if p := MakePlan(&ProbeResult{DisplayW: 1920, DisplayH: 1080, FPS: Rational{60, 1}}, box); p.W != 1920 || p.H != 1080 {
		t.Fatalf("1080p in a 1080p box = %dx%d", p.W, p.H)
	}
	if p := MakePlan(&ProbeResult{DisplayW: 3840, DisplayH: 2160, FPS: Rational{30, 1}}, box); p.W != 1920 || p.H != 1080 {
		t.Fatalf("4K in a 1080p box = %dx%d", p.W, p.H)
	}
}

// TestPickFPS: a VARIABLE-rate source is planned at the rate its action was
// captured at, not at the average of action and stillness (§20 erratum 93) —
// but only when r_frame_rate is itself a plausible rate, because Matroska and
// WebM put the timebase there.
func TestPickFPS(t *testing.T) {
	for _, c := range []struct {
		name   string
		avg, r Rational
		maxFPS int
		want   Rational
	}{
		// The measured shape: a phone screen recording, fast section 30 fps,
		// menus ~10. Planning the average threw away 3 of every 5 frames.
		{"VFR mp4", Rational{3600, 293}, Rational{30, 1}, 60, Rational{30, 1}},
		{"VFR measured on 7.1.1", Rational{2100, 149}, Rational{30, 1}, 60, Rational{30, 1}},
		// Matroska/WebM report the timebase as r_frame_rate: far above the
		// cap, so the average survives instead of being capped to MaxFPS.
		{"mkv timebase", Rational{30000, 1001}, Rational{1000, 1}, 60, Rational{30000, 1001}},
		{"webm timebase", Rational{30, 1}, Rational{90000, 1}, 60, Rational{30, 1}},
		// Constant rate: the two agree, or r is a rounded guess at a
		// fractional average and must not displace the exact one.
		{"CFR 30", Rational{30, 1}, Rational{30, 1}, 60, Rational{30, 1}},
		{"CFR 29.97 with a rounded r", Rational{30000, 1001}, Rational{30, 1}, 60, Rational{30000, 1001}},
		{"CFR 60", Rational{60, 1}, Rational{60, 1}, 60, Rational{60, 1}},
		// Exactly at and just past the 25% threshold.
		{"r = 1.25·avg is not enough", Rational{24, 1}, Rational{30, 1}, 60, Rational{24, 1}},
		{"r just past 1.25·avg", Rational{24, 1}, Rational{31, 1}, 60, Rational{31, 1}},
		// r above the cap is refused even when the average is tiny.
		{"r past the cap", Rational{5, 1}, Rational{120, 1}, 60, Rational{5, 1}},
		{"r exactly at the cap", Rational{5, 1}, Rational{60, 1}, 60, Rational{60, 1}},
		{"a lower cap refuses more", Rational{5, 1}, Rational{60, 1}, 30, Rational{5, 1}},
		// Missing rates.
		{"missing avg", Rational{}, Rational{25, 1}, 60, Rational{25, 1}},
		{"missing r", Rational{30, 1}, Rational{}, 60, Rational{30, 1}},
		{"both missing", Rational{}, Rational{}, 60, Rational{30, 1}},
		{"invalid avg", Rational{0, 0}, Rational{0, 0}, 60, Rational{30, 1}},
	} {
		if got := pickFPS(c.avg, c.r, c.maxFPS); got != c.want {
			t.Errorf("%s: pickFPS(%v, %v, %d) = %v, want %v", c.name, c.avg, c.r, c.maxFPS, got, c.want)
		}
	}
	// End to end through MakePlan: the VFR source is encoded at 30, and the
	// mkv whose r_frame_rate is 1000/1 is NOT capped to 60.
	std := PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60}
	vfr := MakePlan(&ProbeResult{DisplayW: 1280, DisplayH: 720, FPS: Rational{3600, 293}, RFPS: Rational{30, 1}}, std)
	if vfr.FPS != (Rational{30, 1}) || vfr.GOP != 60 || vfr.KeyintMin != 30 {
		t.Errorf("VFR plan: fps=%v gop=%d keyint=%d", vfr.FPS, vfr.GOP, vfr.KeyintMin)
	}
	mkv := MakePlan(&ProbeResult{DisplayW: 1280, DisplayH: 720, FPS: Rational{30000, 1001}, RFPS: Rational{1000, 1}}, std)
	if mkv.FPS != (Rational{30000, 1001}) {
		t.Errorf("mkv plan: fps=%v, want 30000/1001", mkv.FPS)
	}
	// A picked r is within the cap by construction; the cap still governs the
	// average branch, which is where an above-cap r lands.
	capped := MakePlan(&ProbeResult{DisplayW: 1280, DisplayH: 720, FPS: Rational{100, 1}, RFPS: Rational{1000, 1}}, std)
	if capped.FPS != (Rational{60, 1}) {
		t.Errorf("100 fps average behind a timebase r: fps=%v, want 60/1", capped.FPS)
	}
}

// TestMakePlanTonemap: the plan tone maps a source that says it is HDR and
// nothing else, and an SDR plan's filter graph is the one this package has
// always rendered plus a metadata-only Rec.709 stamp (§20 erratum 93).
func TestMakePlanTonemap(t *testing.T) {
	std := PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60}
	// The SDR graph is the one this package has always rendered, plus the
	// Rec.709 stamp that closes EVERY chain. The stamp is metadata-only, so
	// not one pixel of an SDR encode changes.
	const stamp = ",setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709"
	const sdrGolden = "[0:0]scale=1280:720:flags=lanczos,setsar=1,format=yuv420p" + stamp + "[v]"
	const hdrGolden = "[0:0]zscale=t=linear:npl=100,format=gbrpf32le,tonemap=tonemap=mobius:desat=0," +
		"zscale=p=bt709:t=bt709:m=bt709:r=tv,format=yuv420p," +
		"scale=1280:720:flags=lanczos,setsar=1,format=yuv420p" + stamp + "[v]"
	for _, c := range []struct {
		name     string
		transfer string
		tonemap  bool
		fc       string
	}{
		{"HDR10 / PQ", "smpte2084", true, hdrGolden},
		{"HLG", "arib-std-b67", true, hdrGolden},
		{"PQ, upper case", "SMPTE2084", true, hdrGolden},
		{"HLG, padded", "  arib-std-b67 ", true, hdrGolden},
		{"SDR control: bt709", "bt709", false, sdrGolden},
		{"SDR control: unset", "", false, sdrGolden},
		{"SDR control: unknown", "unknown", false, sdrGolden},
		{"SDR control: SD bt470bg", "bt470bg", false, sdrGolden},
		// bt2020-10 is a WIDE GAMUT transfer, not an HDR one: 10-bit
		// Rec.2020 SDR is left alone rather than tone mapped.
		{"SDR control: bt2020-10", "bt2020-10", false, sdrGolden},
	} {
		p := MakePlan(&ProbeResult{DisplayW: 1280, DisplayH: 720, FPS: Rational{30, 1},
			ColorTransfer: c.transfer, ColorPrimaries: "bt2020", ColorSpace: "bt2020nc"}, std)
		if p.Tonemap != c.tonemap {
			t.Errorf("%s: Tonemap = %v, want %v", c.name, p.Tonemap, c.tonemap)
			continue
		}
		if got := p.FilterComplex(); got != c.fc {
			t.Errorf("%s: FilterComplex =\n %s\nwant\n %s", c.name, got, c.fc)
		}
	}
	// The stamp is the LAST link of the video chain in every shape — nothing
	// downstream can put the source's tags back (§20 erratum 93).
	for _, pl := range []Plan{
		MakePlan(&ProbeResult{DisplayW: 1280, DisplayH: 720, FPS: Rational{30, 1}}, std),
		MakePlan(&ProbeResult{DisplayW: 1280, DisplayH: 720, FPS: Rational{30, 1}, ColorTransfer: "smpte2084"}, std),
		MakePlan(&ProbeResult{DisplayW: 720, DisplayH: 1280, FPS: Rational{30, 1},
			Rotation: 90, RotationFromTag: true, AudioIndexes: []int{1, 2}}, std),
	} {
		video, _, _ := strings.Cut(pl.FilterComplex(), ";")
		if !strings.HasSuffix(video, stamp+"[v]") {
			t.Errorf("the Rec.709 stamp must close the video chain: %s", video)
		}
	}
}

// TestMakePlanTonemapAssumesMissingColourTags (§20 erratum 93, amended). A
// consumer HDR capture very often names its TRANSFER and leaves primaries and
// matrix unspecified — and zscale cannot convert a frame that has not said
// what it is, so before this the tone map did not degrade, it FAILED the clip
// permanently ("no path between colorspaces"). The chain therefore states
// BT.2100's own container on the frame first, per field, only for the fields
// the probe found empty: a fully tagged HDR10 source renders exactly the
// graph it did before.
func TestMakePlanTonemapAssumesMissingColourTags(t *testing.T) {
	std := PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60}
	for _, c := range []struct {
		name             string
		primaries, space string
		wantPri, wantMtx bool
		prefix           string
	}{
		{"fully tagged HDR10", "bt2020", "bt2020nc", false, false, ""},
		{"transfer only", "", "", true, true, "setparams=color_primaries=bt2020:colorspace=bt2020nc,"},
		{"matrix known, primaries not", "", "bt470bg", true, false, "setparams=color_primaries=bt2020,"},
		{"primaries known, matrix not", "bt709", "", false, true, "setparams=colorspace=bt2020nc,"},
		{"the enum's own placeholders", "unknown", "UNSPECIFIED", true, true,
			"setparams=color_primaries=bt2020:colorspace=bt2020nc,"},
		{"padded placeholder", " reserved ", "bt2020nc", true, false, "setparams=color_primaries=bt2020,"},
	} {
		p := MakePlan(&ProbeResult{DisplayW: 1280, DisplayH: 720, FPS: Rational{30, 1},
			ColorTransfer: "smpte2084", ColorPrimaries: c.primaries, ColorSpace: c.space}, std)
		if !p.Tonemap {
			t.Fatalf("%s: an smpte2084 source must be tone mapped", c.name)
		}
		if p.AssumePrimaries != c.wantPri || p.AssumeMatrix != c.wantMtx {
			t.Errorf("%s: assume primaries/matrix = %v/%v, want %v/%v",
				c.name, p.AssumePrimaries, p.AssumeMatrix, c.wantPri, c.wantMtx)
		}
		want := "[0:0]" + c.prefix + tonemapChain
		if got := p.FilterComplex(); !strings.HasPrefix(got, want) {
			t.Errorf("%s: FilterComplex =\n %s\nwant the prefix\n %s", c.name, got, want)
		}
		// The assumed tags are only ever the tone map's business: an SDR
		// source's graph is untouched whatever its colour fields say.
		sdr := MakePlan(&ProbeResult{DisplayW: 1280, DisplayH: 720, FPS: Rational{30, 1},
			ColorPrimaries: c.primaries, ColorSpace: c.space}, std)
		if sdr.AssumePrimaries || sdr.AssumeMatrix || strings.Contains(sdr.FilterComplex(), "bt2020") {
			t.Errorf("%s: an SDR plan must assume nothing: %s", c.name, sdr.FilterComplex())
		}
	}
}

func TestEvenAndRational(t *testing.T) {
	for _, c := range []struct{ in, want int }{{0, 2}, {1, 2}, {2, 2}, {3, 2}, {533, 532}, {1279, 1278}, {1280, 1280}} {
		if got := even(c.in); got != c.want {
			t.Errorf("even(%d) = %d, want %d", c.in, got, c.want)
		}
	}
	r := Rational{30000, 1001}
	if r.String() != "30000/1001" || r.Ceil() != 30 || !r.Valid() {
		t.Errorf("Rational %v: String=%s Ceil=%d Valid=%v", r, r.String(), r.Ceil(), r.Valid())
	}
	if (Rational{0, 1}).Valid() || (Rational{1, 0}).Valid() || (Rational{1, 0}).Float() != 0 {
		t.Error("invalid rationals must not be Valid / must Float to 0")
	}
	if AudioMerge.String() != "merge" || AudioNone.String() != "none" || AudioSingle.String() != "single" {
		t.Error("AudioMode.String")
	}
}
