package transcode

import (
	"math"
	"strings"
	"testing"
)

// playableProbe is a source that passes every row of the playability table:
// 1920×1080 h264/yuv420p in mp4 with one aac track, square pixels, SDR,
// progressive, 60 fps, a declared duration.
func playableProbe() *ProbeResult {
	return &ProbeResult{
		VideoIndex: 0, AudioIndexes: []int{1}, AudioCodecs: []string{"aac"},
		CodedW: 1920, CodedH: 1080, DisplayW: 1920, DisplayH: 1080,
		SARNum: 1, SARDen: 1,
		FPS:        Rational{60, 1},
		RFPS:       Rational{60, 1},
		VideoCodec: "h264", PixFmt: "yuv420p", FormatName: "mov,mp4,m4a,3gp,3g2,mj2",
		FieldOrder:  "progressive",
		HasDuration: true, DurationUS: 6_000_000,
	}
}

func TestOriginalPlayability(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*ProbeResult)
		mediaType string
		faststart bool
		want      Playability
	}{
		{"faststart mp4 plays directly", nil, "video/mp4", true,
			Playability{Playable: true}},
		{"moov at the end needs a remux", nil, "video/mp4", false,
			Playability{Playable: true, Remux: true}},
		{"a quicktime media type needs a remux", nil, "video/quicktime", true,
			Playability{Playable: true, Remux: true}},
		{"an unstated media type needs a remux", nil, "", true,
			Playability{Playable: true, Remux: true}},
		{"av1 in mp4 plays", func(p *ProbeResult) { p.VideoCodec = "av1" }, "video/mp4", true,
			Playability{Playable: true}},
		{"no audio at all plays", func(p *ProbeResult) { p.AudioIndexes, p.AudioCodecs = nil, nil }, "video/mp4", true,
			Playability{Playable: true}},
		{"matroska", func(p *ProbeResult) { p.FormatName = "matroska,webm" }, "video/x-matroska", true,
			Playability{Reason: NotPlayableContainer}},
		{"hevc", func(p *ProbeResult) { p.VideoCodec = "hevc" }, "video/mp4", true,
			Playability{Reason: NotPlayableCodec}},
		{"10-bit", func(p *ProbeResult) { p.PixFmt = "yuv420p10le" }, "video/mp4", true,
			Playability{Reason: NotPlayablePixFmt}},
		{"4:4:4", func(p *ProbeResult) { p.PixFmt = "yuv444p" }, "video/mp4", true,
			Playability{Reason: NotPlayablePixFmt}},
		{"full-range 4:2:0 still plays", func(p *ProbeResult) { p.PixFmt = "yuvj420p" }, "video/mp4", true,
			Playability{Playable: true}},
		{"HDR PQ", func(p *ProbeResult) { p.ColorTransfer = "smpte2084" }, "video/mp4", true,
			Playability{Reason: NotPlayableHDR}},
		{"HLG", func(p *ProbeResult) { p.ColorTransfer = "arib-std-b67" }, "video/mp4", true,
			Playability{Reason: NotPlayableHDR}},
		{"interlaced", func(p *ProbeResult) { p.FieldOrder = "tt" }, "video/mp4", true,
			Playability{Reason: NotPlayableInterlaced}},
		{"two audio tracks", func(p *ProbeResult) {
			p.AudioIndexes, p.AudioCodecs = []int{1, 2}, []string{"aac", "aac"}
		}, "video/mp4", true, Playability{Reason: NotPlayableAudio}},
		{"one non-aac track", func(p *ProbeResult) { p.AudioCodecs = []string{"opus"} }, "video/mp4", true,
			Playability{Reason: NotPlayableAudio}},
		{"rotation from a tag", func(p *ProbeResult) {
			p.Rotation, p.RotationFromTag = 90, true
			p.DisplayW, p.DisplayH = 1080, 1920
		}, "video/mp4", true, Playability{Reason: NotPlayableRotateTag}},
		{"rotation from the display matrix plays", func(p *ProbeResult) {
			p.Rotation, p.RotationFromTag = 90, false
			p.DisplayW, p.DisplayH = 1080, 1920
		}, "video/mp4", true, Playability{Playable: true}},
		{"anamorphic", func(p *ProbeResult) { p.SARNum, p.SARDen = 16, 11 }, "video/mp4", true,
			Playability{Reason: NotPlayableAnamorphic}},
		{"no duration", func(p *ProbeResult) { p.HasDuration, p.DurationUS = false, 0 }, "video/mp4", true,
			Playability{Reason: NotPlayableNoDuration}},
		{"no frame rate", func(p *ProbeResult) { p.FPS = Rational{} }, "video/mp4", true,
			Playability{Reason: NotPlayableNoFPS}},
		{"past the Full ceiling", func(p *ProbeResult) { p.DisplayW, p.DisplayH = 8192, 4320 }, "video/mp4", true,
			Playability{Reason: NotPlayableTooLarge}},
		{"exactly 8K plays", func(p *ProbeResult) { p.DisplayW, p.DisplayH = FullMaxW, FullMaxH }, "video/mp4", true,
			Playability{Playable: true}},
		// The third remux trigger (§20 erratum 200, amended): this is the one branch
		// that publishes a file WITHOUT rendering it, so a phone's GPS tag or a
		// camera's model would become public on it. `-c copy` is what strips them.
		{"a file carrying private metadata is remuxed to strip it",
			func(p *ProbeResult) { p.PrivateTags = true }, "video/mp4", true,
			Playability{Playable: true, Remux: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := playableProbe()
			if tc.mutate != nil {
				tc.mutate(p)
			}
			got := OriginalPlayability(p, tc.mediaType, tc.faststart)
			if got != tc.want {
				t.Fatalf("OriginalPlayability = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestOriginalPlayabilityNilProbe(t *testing.T) {
	if got := OriginalPlayability(nil, "video/mp4", true); got.Playable {
		t.Fatalf("a nil probe must not be playable: %+v", got)
	}
}

func TestLightWanted(t *testing.T) {
	const cap25 = 25 << 20
	tests := []struct {
		name     string
		maxBytes int64
		w, h     int
		fps      Rational
		size     int64
		known    bool
		want     bool
	}{
		{"off by configuration", 0, 3840, 2160, Rational{60, 1}, 1 << 30, true, false},
		{"720p30 and small: no light", cap25, 1280, 720, Rational{30, 1}, 1 << 20, true, false},
		{"1080p: light", cap25, 1920, 1080, Rational{30, 1}, 1 << 20, true, true},
		{"vertical 720x1280 fits the box", cap25, 720, 1280, Rational{30, 1}, 1 << 20, true, false},
		{"vertical 1080x1920 does not", cap25, 1080, 1920, Rational{30, 1}, 1 << 20, true, true},
		{"60 fps at 720p: light", cap25, 1280, 720, Rational{60, 1}, 1 << 20, true, true},
		{"29.97 is not above 30", cap25, 1280, 720, Rational{30000, 1001}, 1 << 20, true, false},
		{"big and calm: light", cap25, 1280, 720, Rational{30, 1}, cap25, true, true},
		{"big but the size is unknown: no light", cap25, 1280, 720, Rational{30, 1}, cap25, false, false},
		{"no dims decides nothing", cap25, 0, 0, Rational{60, 1}, 1 << 30, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := LightWanted(tc.maxBytes, tc.w, tc.h, tc.fps, tc.size, tc.known); got != tc.want {
				t.Fatalf("LightWanted = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFullPlanOptionsNeverBinds(t *testing.T) {
	box := PlanOptions{MaxW: DefaultMaxW, MaxH: DefaultMaxH, MaxFPS: DefaultMaxFPS}
	p := playableProbe()
	p.DisplayW, p.DisplayH = 2560, 1440
	p.FPS, p.RFPS = Rational{60, 1}, Rational{60, 1}
	plan := MakePlan(p, FullPlanOptions(p, box))
	if plan.W != 2560 || plan.H != 1440 {
		t.Fatalf("Full plan is %dx%d, want the source's 2560x1440", plan.W, plan.H)
	}
	if plan.FPS != (Rational{60, 1}) {
		t.Fatalf("Full plan fps = %s, want 60/1", plan.FPS)
	}
	// A rate above the operator's ceiling is kept: the ceiling can no longer
	// bind below what the source itself ran at.
	p.FPS, p.RFPS = Rational{240, 1}, Rational{240, 1}
	if plan := MakePlan(p, FullPlanOptions(p, box)); plan.FPS != (Rational{240, 1}) {
		t.Fatalf("240 fps source planned at %s, want 240/1", plan.FPS)
	}
	// …but a container reporting its TIMEBASE as a rate still falls through to
	// the average it measured (pickFPS's sanity test, erratum 93).
	p.FPS, p.RFPS = Rational{30, 1}, Rational{1000, 1}
	if plan := MakePlan(p, FullPlanOptions(p, box)); plan.FPS != (Rational{30, 1}) {
		t.Fatalf("mkv nonsense r_frame_rate planned at %s, want 30/1", plan.FPS)
	}
	// A vertical source keeps its orientation: the box is (long, short).
	p.DisplayW, p.DisplayH = 1080, 1920
	p.FPS, p.RFPS = Rational{30, 1}, Rational{30, 1}
	if plan := MakePlan(p, FullPlanOptions(p, box)); plan.W != 1080 || plan.H != 1920 {
		t.Fatalf("vertical Full plan is %dx%d, want 1080x1920", plan.W, plan.H)
	}
}

func TestLightPlanOptions(t *testing.T) {
	got := LightPlanOptions(PlanOptions{MaxW: 1920, MaxH: 1080, MaxFPS: 60})
	if got != (PlanOptions{MaxW: LightMaxW, MaxH: LightMaxH, MaxFPS: SocialMaxFPS}) {
		t.Fatalf("LightPlanOptions widened past the tier's own box: %+v", got)
	}
	// An operator who asked for something smaller keeps it, in every dimension.
	got = LightPlanOptions(PlanOptions{MaxW: 854, MaxH: 480, MaxFPS: 24})
	if got != (PlanOptions{MaxW: 854, MaxH: 480, MaxFPS: 24}) {
		t.Fatalf("LightPlanOptions overrode a smaller instance box: %+v", got)
	}
}

func TestMakeTierPlan(t *testing.T) {
	box := PlanOptions{MaxW: DefaultMaxW, MaxH: DefaultMaxH, MaxFPS: DefaultMaxFPS}
	opts := func(mt string, fast bool, size int64) TierPlanOptions {
		return TierPlanOptions{Box: box, FullCodec: FullCodecH264, LightMaxBytes: 25 << 20,
			SourceSize: size, SourceMediaType: mt, Faststart: fast}
	}

	t.Run("playable 1080p60: no full, light for the card", func(t *testing.T) {
		p := playableProbe()
		tp := MakeTierPlan(p, opts("video/mp4", true, 8<<20))
		if !tp.Playable || tp.Remux || tp.FullNeeded {
			t.Fatalf("want a direct playable original: %+v", tp)
		}
		if !tp.LightNeeded || tp.CardTier != TierLight {
			t.Fatalf("1080p60 must plan a Light copy: %+v", tp)
		}
		if tp.Light.W != 1280 || tp.Light.H != 720 || tp.Light.FPS != (Rational{30, 1}) {
			t.Fatalf("light plan %dx%d @ %s, want 1280x720 @ 30/1", tp.Light.W, tp.Light.H, tp.Light.FPS)
		}
	})

	t.Run("playable 720p30 and small: nothing to make", func(t *testing.T) {
		p := playableProbe()
		p.DisplayW, p.DisplayH, p.CodedW, p.CodedH = 1280, 720, 1280, 720
		p.FPS, p.RFPS = Rational{30, 1}, Rational{30, 1}
		tp := MakeTierPlan(p, opts("video/mp4", true, 2<<20))
		if !tp.Playable || tp.FullNeeded || tp.LightNeeded {
			t.Fatalf("want the remux fast path with no encode at all: %+v", tp)
		}
		if tp.CardTier != TierOriginal {
			t.Fatalf("card tier = %q, want original", tp.CardTier)
		}
	})

	t.Run("playable 720p30 but large: light only", func(t *testing.T) {
		p := playableProbe()
		p.DisplayW, p.DisplayH, p.CodedW, p.CodedH = 1280, 720, 1280, 720
		p.FPS, p.RFPS = Rational{30, 1}, Rational{30, 1}
		tp := MakeTierPlan(p, opts("video/mp4", false, 400<<20))
		if !tp.Playable || !tp.Remux || tp.FullNeeded {
			t.Fatalf("want a remuxed original: %+v", tp)
		}
		if !tp.LightNeeded || tp.CardTier != TierLight {
			t.Fatalf("a 400 MB upload must plan a Light copy: %+v", tp)
		}
	})

	t.Run("not playable: full at the source's own size", func(t *testing.T) {
		p := playableProbe()
		p.VideoCodec = "hevc"
		p.DisplayW, p.DisplayH, p.CodedW, p.CodedH = 2560, 1440, 2560, 1440
		tp := MakeTierPlan(p, opts("video/mp4", true, 8<<20))
		if tp.Playable || !tp.FullNeeded {
			t.Fatalf("hevc must need a Full: %+v", tp)
		}
		if tp.Full.W != 2560 || tp.Full.H != 1440 {
			t.Fatalf("full plan %dx%d, want the source's 2560x1440 (never downscaled)", tp.Full.W, tp.Full.H)
		}
		if !tp.LightNeeded || tp.CardTier != TierLight {
			t.Fatalf("1440p60 must plan a Light copy: %+v", tp)
		}
	})

	t.Run("not playable, small: the full is the card", func(t *testing.T) {
		p := playableProbe()
		p.FormatName = "matroska,webm"
		p.DisplayW, p.DisplayH, p.CodedW, p.CodedH = 640, 360, 640, 360
		p.FPS, p.RFPS = Rational{30, 1}, Rational{30, 1}
		tp := MakeTierPlan(p, opts("video/x-matroska", false, 1<<20))
		if tp.Playable || !tp.FullNeeded || tp.LightNeeded {
			t.Fatalf("want a Full and no Light: %+v", tp)
		}
		if tp.CardTier != TierFull {
			t.Fatalf("card tier = %q, want full", tp.CardTier)
		}
	})

	// The bug erratum 235 closed. This source is card-SHAPED — 1280x720 at 30 fps,
	// so neither the picture term nor the frame-rate term of the skip rule fires —
	// and no browser plays it, so the Full is what a crawler would fetch. The only
	// term left is the SIZE, and before erratum 235 the plan had none for a file it
	// had not encoded yet: it decided CardTier=full, the run flipped `ready` on one
	// CRF 18 encode, and only THEN discovered the Full was past SOCIAL_MAX_BYTES
	// and queued the card copy behind it. In that window POST /api/upload answered
	// card_ready: true and og:video pointed at a file above the very threshold the
	// Light tier exists to stay under.
	//
	// 80 seconds of 1280x720 at 30 fps estimates 1280*720*30*80*0.2/8 = 52.7 MiB,
	// past the 25 MiB cap, so the plan now carries this clip into the dual-output
	// branch and both files are in the bucket at the flip.
	t.Run("not playable, card-shaped, but too long to fit the cap: light as well", func(t *testing.T) {
		p := playableProbe()
		p.FormatName = "matroska,webm"
		p.DisplayW, p.DisplayH, p.CodedW, p.CodedH = 1280, 720, 1280, 720
		p.FPS, p.RFPS = Rational{30, 1}, Rational{30, 1}
		p.DurationUS = 80_000_000
		tp := MakeTierPlan(p, opts("video/x-matroska", false, 4<<20))
		if tp.Playable || !tp.FullNeeded {
			t.Fatalf("matroska must need a Full: %+v", tp)
		}
		if tp.Full.W != 1280 || tp.Full.H != 720 || tp.Full.FPS != (Rational{30, 1}) {
			t.Fatalf("full plan %dx%d @ %s, want the source's own 1280x720 @ 30/1", tp.Full.W, tp.Full.H, tp.Full.FPS)
		}
		if !tp.LightNeeded || tp.CardTier != TierLight {
			t.Fatalf("a Full estimated at %d bytes past a %d cap must plan a Light copy: %+v",
				PlannedOutputBytes(tp.Full), int64(25<<20), tp)
		}
		if tp.Light.W != 1280 || tp.Light.H != 720 || tp.Light.FPS != (Rational{30, 1}) {
			t.Fatalf("light plan %dx%d @ %s", tp.Light.W, tp.Light.H, tp.Light.FPS)
		}
		// The SAME source, ten seconds long, is under the cap and keeps the single
		// encode: the fix is a size test and not a new blanket rule.
		p.DurationUS = 10_000_000
		if short := MakeTierPlan(p, opts("video/x-matroska", false, 4<<20)); short.LightNeeded || short.CardTier != TierFull {
			t.Fatalf("a card-shaped Full that fits the cap must stay the card: %+v", short)
		}
	})

	// A plan nobody can size — a matroska muxed to a pipe carries no duration —
	// falls back to the shape alone, which is what it did before erratum 235. The
	// commit-time rule (worker's lightWanted) is what covers this clip.
	t.Run("not playable and unsizeable: the shape alone decides", func(t *testing.T) {
		p := playableProbe()
		p.FormatName = "matroska,webm"
		p.DisplayW, p.DisplayH, p.CodedW, p.CodedH = 1280, 720, 1280, 720
		p.FPS, p.RFPS = Rational{30, 1}, Rational{30, 1}
		p.HasDuration, p.DurationUS = false, 0
		tp := MakeTierPlan(p, opts("video/x-matroska", false, 4<<20))
		if PlannedOutputBytes(tp.Full) != 0 {
			t.Fatalf("premise: the Full is sizeable after all (%d)", PlannedOutputBytes(tp.Full))
		}
		if tp.LightNeeded || tp.CardTier != TierFull {
			t.Fatalf("an unsizeable Full must decide on shape alone: %+v", tp)
		}
	})

	t.Run("past the ceiling and unplayable: no tier at all", func(t *testing.T) {
		p := playableProbe()
		p.VideoCodec = "prores"
		p.DisplayW, p.DisplayH, p.CodedW, p.CodedH = 8192, 4320, 8192, 4320
		tp := MakeTierPlan(p, opts("video/quicktime", false, 1<<30))
		if !tp.TooLarge || tp.Reason != NotPlayableTooLarge {
			t.Fatalf("want TooLarge with the plain reason: %+v", tp)
		}
	})

	t.Run("SOCIAL_MAX_BYTES=0 turns the Light tier off", func(t *testing.T) {
		o := opts("video/mp4", true, 8<<20)
		o.LightMaxBytes = 0
		tp := MakeTierPlan(playableProbe(), o)
		if tp.LightNeeded || tp.CardTier != TierOriginal {
			t.Fatalf("light must be off: %+v", tp)
		}
	})

	t.Run("a nil probe decides nothing", func(t *testing.T) {
		tp := MakeTierPlan(nil, opts("video/mp4", true, 1))
		if tp.Playable || tp.LightNeeded || tp.TooLarge {
			t.Fatalf("a nil probe must plan nothing: %+v", tp)
		}
	})
}

func TestNormalizeFullCodecAndCRF(t *testing.T) {
	for _, in := range []string{"", "h264", "H264", "nonsense"} {
		if got := NormalizeFullCodec(in); got != FullCodecH264 {
			t.Fatalf("NormalizeFullCodec(%q) = %q, want h264", in, got)
		}
	}
	for _, in := range []string{"av1", "AV1", " av1 "} {
		if got := NormalizeFullCodec(in); got != FullCodecAV1 {
			t.Fatalf("NormalizeFullCodec(%q) = %q, want av1", in, got)
		}
	}
	if FullCRFFor(FullCodecH264) != FullCRF || FullCRFFor(FullCodecAV1) != FullAV1CRF {
		t.Fatal("FullCRFFor does not follow the codec")
	}
}

func TestRemuxArgs(t *testing.T) {
	p := playableProbe()
	got := strings.Join(RemuxArgs(p, "/s/in.mp4", "/s/out.mp4"), " ")
	for _, want := range []string{
		"-c copy", "-movflags +faststart", "-f mp4 /s/out.mp4",
		"-map 0:0", "-map 0:1", "-sn -dn", "-map_metadata -1", "-map_chapters -1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remux argv lacks %q:\n%s", want, got)
		}
	}
	// Not one encoder option, and no window: a remux is a container rewrite.
	for _, never := range []string{"libx264", "libsvtav1", "-crf", "-ss ", "-t ", "-filter_complex"} {
		if strings.Contains(got, never) {
			t.Fatalf("remux argv must not contain %q:\n%s", never, got)
		}
	}
	// A silent source maps no audio at all.
	p.AudioIndexes, p.AudioCodecs = nil, nil
	if got := strings.Join(RemuxArgs(p, "in.mp4", "out.mp4"), " "); strings.Contains(got, "-map 0:1") {
		t.Fatalf("a source with no audio must map none:\n%s", got)
	}
}

func TestEncodeArgsCodecSelection(t *testing.T) {
	plan := MakePlan(playableProbe(), FullPlanOptions(playableProbe(), PlanOptions{}))
	h264 := strings.Join(plan.EncodeArgs("in.mp4", "out.mp4", EncodeOptions{Preset: "veryfast", CRF: FullCRF}), " ")
	if !strings.Contains(h264, "-c:v libx264 -preset veryfast -crf 18 -profile:v high") {
		t.Fatalf("h264 argv changed shape:\n%s", h264)
	}
	av1 := strings.Join(plan.EncodeArgs("in.mp4", "out.mp4",
		EncodeOptions{Preset: "veryfast", CRF: FullAV1CRF, Codec: FullCodecAV1}), " ")
	if !strings.Contains(av1, "-c:v libsvtav1 -preset 8 -crf 32 -svtav1-params tune=0") {
		t.Fatalf("av1 argv:\n%s", av1)
	}
	for _, never := range []string{"libx264", "-profile:v high"} {
		if strings.Contains(av1, never) {
			t.Fatalf("av1 argv must not contain %q:\n%s", never, av1)
		}
	}
	// The Light tier pins H.264 whatever the instance's FULL_CODEC says.
	light := strings.Join(plan.EncodeArgs("in.mp4", "out.mp4",
		SocialEncodeOptions(EncodeOptions{Preset: "veryfast", CRF: FullAV1CRF, Codec: FullCodecAV1})), " ")
	if !strings.Contains(light, "-c:v libx264") || strings.Contains(light, "libsvtav1") {
		t.Fatalf("the light encode must always be h264:\n%s", light)
	}
}

func TestCheckTierOutput(t *testing.T) {
	ok := &OutputInfo{Width: 1920, Height: 1080, FPS: Rational{60, 1}, DurationMS: 6000,
		VideoStreams: 1, VideoCodec: "h264", AudioStreams: 1, AudioCodec: "aac", HasAudio: true}
	if err := CheckTierOutput(ok, TierCheck{Width: 1920, Height: 1080}); err != nil {
		t.Fatalf("a 1080p h264 tier must pass: %v", err)
	}
	// The old box no longer applies — that is the whole point of the tier check.
	if err := CheckOutput(ok, PlanOptions{}); err == nil {
		t.Fatal("CheckOutput still has to enforce the instance box (it guards Light)")
	}
	av1 := *ok
	av1.VideoCodec = "av1"
	if err := CheckTierOutput(&av1, TierCheck{Codecs: []string{"h264", "av1"}, Width: 1920, Height: 1080}); err != nil {
		t.Fatalf("av1 in the accepted set: %v", err)
	}
	if err := CheckTierOutput(&av1, TierCheck{Width: 1920, Height: 1080}); err == nil {
		t.Fatal("av1 must be refused when only h264 was asked for")
	}
	wrong := *ok
	wrong.Width = 1280
	if err := CheckTierOutput(&wrong, TierCheck{Width: 1920, Height: 1080}); err == nil {
		t.Fatal("a tier that came out at the wrong size must be bad_output")
	} else if !IsPermanent(err) {
		t.Fatalf("a dimension mismatch is the file's fault: %v", err)
	}
	if err := CheckTierOutput(nil, TierCheck{}); err == nil {
		t.Fatal("no probe result is bad_output")
	}
}

// hasPrivateTags is an ALLOWLIST of the keys an ISO-BMFF file carries because it
// IS one. The direction that has to be safe is the unknown one, so anything else
// — including a vendor key nobody here has heard of — makes the upload take the
// `-c copy` branch that strips it (§20 erratum 200, amended).
func TestHasPrivateTags(t *testing.T) {
	tests := []struct {
		name   string
		format map[string]string
		stream map[string]string
		want   bool
	}{
		{"nothing at all", nil, nil, false},
		{"what ffmpeg writes by default (measured, 7.1.1)",
			map[string]string{"major_brand": "isom", "minor_version": "512",
				"compatible_brands": "isomiso2avc1mp41", "encoder": "Lavf61.7.100"},
			map[string]string{"language": "und", "handler_name": "VideoHandler",
				"vendor_id": "[0][0][0][0]", "encoder": "Lavc61.19.101 libx264"},
			false},
		{"an empty value is not metadata", map[string]string{"comment": "  "}, nil, false},
		{"a phone's GPS location", map[string]string{"location": "+51.5074-000.1278/"}, nil, true},
		{"Apple's own spelling of it",
			map[string]string{"com.apple.quicktime.location.ISO6709": "+51.5074-000.1278/"}, nil, true},
		{"a creation time", map[string]string{"creation_time": "2019-04-05T13:14:15.000000Z"}, nil, true},
		{"a device", map[string]string{"make": "Acme", "model": "Phone 3"}, nil, true},
		{"an editor's attribution", map[string]string{"artist": "somebody"}, nil, true},
		{"a case the container spelled its own way", map[string]string{"LOCATION": "+0+0/"}, nil, true},
		{"on a STREAM rather than the container", nil,
			map[string]string{"creation_time": "2019-04-05T13:14:15.000000Z"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := ffprobeDoc{
				Format:  ffprobeFormat{FormatName: "mov,mp4,m4a,3gp,3g2,mj2", Tags: tc.format},
				Streams: []ffprobeStream{{Index: 0, CodecType: "video", Tags: tc.stream}},
			}
			if got := hasPrivateTags(doc); got != tc.want {
				t.Fatalf("hasPrivateTags = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPlannedOutputBytes pins the estimator both readers share (§20 erratum 235):
// the worker's scratch guard and output budget, and MakeTierPlan's question about
// the Full it has not encoded yet.
func TestPlannedOutputBytes(t *testing.T) {
	base := Plan{W: 1280, H: 720, FPS: Rational{30, 1}, HasDuration: true, DurationUS: 1_000_000}
	// 1280*720*30*1s*0.2/8
	if got := PlannedOutputBytes(base); got != 691_200 {
		t.Fatalf("PlannedOutputBytes = %d, want 691200", got)
	}
	// It scales linearly in the duration, which is the property the tier plan
	// leans on: the same picture for longer crosses the cap.
	long := base
	long.DurationUS = 80_000_000
	if got, want := PlannedOutputBytes(long), int64(691_200*80); got != want {
		t.Fatalf("PlannedOutputBytes(80s) = %d, want %d", got, want)
	}
	if got := PlannedOutputBytes(long); got <= 25<<20 {
		t.Fatalf("80 s of 720p30 estimates %d, which does not cross a 25 MiB cap", got)
	}
	// "Not sizeable" is 0 and never a guess.
	for name, mutate := range map[string]func(*Plan){
		"no duration":  func(p *Plan) { p.HasDuration = false },
		"zero seconds": func(p *Plan) { p.DurationUS = 0 },
		"no width":     func(p *Plan) { p.W = 0 },
		"no height":    func(p *Plan) { p.H = 0 },
		"no rate":      func(p *Plan) { p.FPS = Rational{} },
	} {
		p := base
		mutate(&p)
		if got := PlannedOutputBytes(p); got != 0 {
			t.Errorf("%s: PlannedOutputBytes = %d, want 0", name, got)
		}
	}
	// An estimate that overflows saturates, which is the ABSENCE of a bound.
	huge := base
	huge.W, huge.H, huge.DurationUS = 1<<20, 1<<20, math.MaxInt64
	if got := PlannedOutputBytes(huge); got != math.MaxInt64 {
		t.Fatalf("PlannedOutputBytes(absurd) = %d, want saturation", got)
	}
}
