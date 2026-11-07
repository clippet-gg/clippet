package transcode

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return raw
}

// TestParseProbeFixtures covers the §16.2 fixture list: rotation at
// side-data index 1 behind an HDR entry, negative rotation, tags.rotate,
// SAR 16:11, 30000/1001, no audio, dual audio, attached_pic first, missing
// duration — plus the plain baseline.
func TestParseProbeFixtures(t *testing.T) {
	type want struct {
		video, codedW, codedH, sarN, sarD, rot, dispW, dispH int
		audio                                                []int
		fps                                                  Rational
		hasDur                                               bool
		durUS                                                int64
		codec                                                string
		fromTag                                              bool
	}
	cases := []struct {
		file string
		want want
	}{
		{"std-720p30.json", want{0, 1280, 720, 1, 1, 0, 1280, 720, []int{1}, Rational{30, 1}, true, 2_000_000, "h264", false}},
		{"rotated90-sidedata.json", want{0, 1280, 720, 1, 1, 90, 720, 1280, []int{1}, Rational{30, 1}, true, 2_000_000, "h264", false}},
		{"rotated-neg90.json", want{0, 1280, 720, 1, 1, 270, 720, 1280, []int{1}, Rational{30, 1}, true, 2_000_000, "h264", false}},
		{"tags-rotate.json", want{0, 1280, 720, 1, 1, 90, 720, 1280, []int{1}, Rational{30, 1}, true, 2_023_000, "h264", true}},
		{"anamorphic-sar.json", want{0, 720, 576, 16, 11, 0, 1047, 576, []int{1}, Rational{25, 1}, true, 2_000_000, "h264", false}},
		{"fractional-2997.json", want{0, 1280, 720, 1, 1, 0, 1280, 720, []int{1}, Rational{30000, 1001}, true, 2_000_000, "h264", false}},
		{"noaudio.json", want{0, 1280, 720, 1, 1, 0, 1280, 720, nil, Rational{30, 1}, true, 2_000_000, "h264", false}},
		{"dualaudio.json", want{0, 1280, 720, 1, 1, 0, 1280, 720, []int{1, 2}, Rational{30, 1}, true, 2_000_000, "h264", false}},
		{"attached-pic-first.json", want{1, 1280, 720, 1, 1, 0, 1280, 720, []int{2}, Rational{30, 1}, true, 2_023_000, "h264", false}},
		// matroska muxed to a pipe: no duration at any level.
		{"no-duration.json", want{0, 1280, 720, 1, 1, 0, 1280, 720, []int{1}, Rational{30, 1}, false, 0, "h264", false}},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			raw := fixture(t, c.file)
			p, err := ParseProbe(raw)
			if err != nil {
				t.Fatalf("ParseProbe: %v", err)
			}
			got := want{p.VideoIndex, p.CodedW, p.CodedH, p.SARNum, p.SARDen, p.Rotation, p.DisplayW, p.DisplayH,
				p.AudioIndexes, p.FPS, p.HasDuration, p.DurationUS, p.VideoCodec, p.RotationFromTag}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got  %+v\nwant %+v", got, c.want)
			}
			if string(p.Raw) != string(raw) {
				t.Fatal("Raw must retain the ffprobe document verbatim")
			}
		})
	}
}

// TestParseProbeRotationBehindHDR pins the trap directly: the fixture's
// Display Matrix is NOT the first side-data entry.
func TestParseProbeRotationBehindHDR(t *testing.T) {
	raw := string(fixture(t, "rotated90-sidedata.json"))
	hdr := strings.Index(raw, `"Mastering display metadata"`)
	matrix := strings.Index(raw, `"Display Matrix"`)
	if hdr < 0 || matrix < 0 || hdr > matrix {
		t.Fatalf("fixture must list an HDR entry before the Display Matrix (hdr=%d matrix=%d)", hdr, matrix)
	}
}

func TestParseProbeNoVideo(t *testing.T) {
	// An accepted container carrying no picture at all is SOUND since §20
	// erratum 328 (TestParseProbeAudio pins that branch); one whose audio
	// stream declares no channels is still refused, in the not_a_video shape.
	audioOnlyMP4 := []byte(`{"streams":[{"index":0,"codec_type":"audio","codec_name":"aac"}],
		"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"2.0"}}`)
	if _, err := ParseProbe(audioOnlyMP4); !IsPermanent(err) || !strings.Contains(err.Error(), "not_a_video") {
		t.Fatalf("channel-less audio-only mp4: err = %v, want permanent not_a_video", err)
	}
	// A picture has a "video" stream (png) but is not a video. The still
	// image check runs BEFORE the container allowlist, so it keeps its own
	// verdict rather than being swallowed as an unsupported container.
	_, err := ParseProbe(fixture(t, "still-image.json"))
	if !IsPermanent(err) || !strings.Contains(err.Error(), "no_video_stream: still image (png_pipe)") {
		t.Fatalf("still-image: err = %v, want permanent still image", err)
	}
	if _, err := ParseProbe([]byte(`{"streams":[],"format":{}}`)); !IsPermanent(err) {
		t.Fatalf("empty streams: err = %v", err)
	}
	if _, err := ParseProbe([]byte(`not json`)); !IsPermanent(err) {
		t.Fatalf("garbage: err = %v", err)
	}
	if _, err := ParseProbe([]byte(`{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":0,"height":0}],
		"format":{"format_name":"matroska,webm"}}`)); !IsPermanent(err) {
		t.Fatalf("zero dims: err = %v", err)
	}
}

// TestParseProbeContainerAllowlist: the ingest gate accepts a closed set of
// container demuxers (§8.3 step 6, §20 erratum 91). The formats that matter
// are the LIST formats — a playlist, a manifest, an ffconcat script — which
// are text files naming further resources; the uploader picks the spool
// extension, so `evil.m3u8` is demuxed as HLS unless this rule refuses it.
// Every refusal carries the permanent not_a_video shape the upload gate
// answers 415 with.
func TestParseProbeContainerAllowlist(t *testing.T) {
	t.Run("refused fixtures", func(t *testing.T) {
		for _, c := range []struct{ file, format string }{
			{"hls-playlist.json", "hls"},
			{"dash-manifest.json", "dash"},
			{"concat-list.json", "concat"},
			{"elementary-h264.json", "h264"},
		} {
			_, err := ParseProbe(fixture(t, c.file))
			if !IsPermanent(err) || !strings.Contains(err.Error(), "not_a_video: unsupported container ("+c.format+")") {
				t.Errorf("%s: err = %v, want permanent not_a_video for %q", c.file, err, c.format)
			}
		}
	})
	// A document with a video stream and dimensions, so only the container
	// decides. The empty format_name is the "no format object at all" case.
	t.Run("format names", func(t *testing.T) {
		doc := func(formatName string) []byte {
			return []byte(`{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":640,"height":360,
				"avg_frame_rate":"30/1","duration":"2.0"}],"format":{"format_name":"` + formatName + `","duration":"2.0"}}`)
		}
		for _, c := range []struct {
			formatName string
			allowed    bool
		}{
			{"mov,mp4,m4a,3gp,3g2,mj2", true}, {"matroska,webm", true}, {"avi", true},
			{"mpegts", true}, {"mpeg", true}, {"mpegvideo", true}, {"flv", true},
			{"asf", true}, {"wmv", true}, {"ogg", true}, {"gif", true}, {"mxf", true},
			{"mp4", true}, {"webm", true},
			// The reason the allowlist exists.
			{"hls", false}, {"applehttp", false}, {"hls,applehttp", false},
			{"dash", false}, {"concat", false}, {"sdp", false}, {"rtsp", false},
			{"ffmetadata", false},
			// Not a container, or no answer at all.
			{"h264", false}, {"hevc", false}, {"mp3", false}, {"", false}, {" ", false},
		} {
			_, err := ParseProbe(doc(c.formatName))
			switch {
			case c.allowed && err != nil:
				t.Errorf("format_name %q: err = %v, want accepted", c.formatName, err)
			case !c.allowed && !IsPermanent(err):
				t.Errorf("format_name %q: err = %v, want permanent refusal", c.formatName, err)
			case !c.allowed && !strings.Contains(err.Error(), "not_a_video"):
				t.Errorf("format_name %q: err = %v, want the not_a_video shape", c.formatName, err)
			}
		}
	})
}

func TestProbeRules(t *testing.T) {
	t.Run("rotation normalization", func(t *testing.T) {
		for _, c := range []struct {
			in   float64
			want int
		}{
			{0, 0}, {90, 90}, {-90, 270}, {180, 180}, {-180, 180}, {270, 270}, {-270, 90}, {360, 0}, {450, 90},
			// Fixed-point slop around a quarter turn still snaps to it…
			{89.6, 90}, {-89.6, 270}, {90.4, 90}, {179.5, 180}, {270.999, 270}, {-0.5, 0},
			// …but an angle that is no quarter turn is NOT snapped: ffmpeg
			// does not swap such a frame either, so the dims must not swap
			// (§20 erratum 39).
			{45, 0}, {-45, 0}, {46, 0}, {44, 0}, {135, 0}, {225, 0}, {315, 0}, {30, 0}, {88, 0}, {1e300, 0},
		} {
			if got := normalizeRotation(c.in); got != c.want {
				t.Errorf("normalizeRotation(%v) = %d, want %d", c.in, got, c.want)
			}
		}
	})
	t.Run("side data scan takes the matrix at any index", func(t *testing.T) {
		r := float64(-90)
		s := ffprobeStream{SideDataList: []ffprobeSideData{{Type: "Content light level metadata"}, {Type: "Mastering display metadata"}, {Type: "Display Matrix", Rotation: &r}}}
		if got, fromTag := rotationOf(s); got != 270 || fromTag {
			t.Fatalf("rotationOf = %d (tag %v), want 270 from the matrix", got, fromTag)
		}
		// The matrix wins over a tag, and is never reported as a tag.
		s.Tags = map[string]string{"rotate": "90"}
		if got, fromTag := rotationOf(s); got != 270 || fromTag {
			t.Fatalf("rotationOf matrix+tag = %d (tag %v), want 270 from the matrix", got, fromTag)
		}
		// A matrix without a rotation key falls through to the tag.
		s = ffprobeStream{SideDataList: []ffprobeSideData{{Type: "Display Matrix"}}, Tags: map[string]string{"ROTATE": "180"}}
		if got, fromTag := rotationOf(s); got != 180 || !fromTag {
			t.Fatalf("rotationOf tag fallback = %d (tag %v), want 180 from the tag", got, fromTag)
		}
		if got, fromTag := rotationOf(ffprobeStream{Tags: map[string]string{"rotate": "junk"}}); got != 0 || fromTag {
			t.Fatalf("garbage tag = %d (tag %v)", got, fromTag)
		}
	})
	t.Run("still image formats", func(t *testing.T) {
		for _, c := range []struct {
			in   string
			want bool
		}{
			{"png_pipe", true}, {"jpeg_pipe", true}, {"webp_pipe", true}, {"bmp_pipe", true}, {"tiff_pipe", true},
			{"image2", true}, {"image2pipe", true},
			{"gif", false}, {"mov,mp4,m4a,3gp,3g2,mj2", false}, {"matroska,webm", false}, {"h264", false}, {"", false},
		} {
			if got := isStillImage(c.in); got != c.want {
				t.Errorf("isStillImage(%q) = %v, want %v", c.in, got, c.want)
			}
		}
	})
	t.Run("SAR", func(t *testing.T) {
		for _, c := range []struct {
			in   string
			n, d int
		}{{"", 1, 1}, {"N/A", 1, 1}, {"0:1", 1, 1}, {"1:1", 1, 1}, {"16:11", 16, 11}, {"4:3", 4, 3}, {"x:y", 1, 1}, {"-1:2", 1, 1}} {
			if n, d := parseSAR(c.in); n != c.n || d != c.d {
				t.Errorf("parseSAR(%q) = %d:%d, want %d:%d", c.in, n, d, c.n, c.d)
			}
		}
		if w, h := displayDims(720, 576, 16, 11, 0); w != 1047 || h != 576 {
			t.Errorf("displayDims SAR = %dx%d", w, h)
		}
		if w, h := displayDims(720, 576, 16, 11, 90); w != 576 || h != 1047 {
			t.Errorf("displayDims SAR+rot = %dx%d", w, h)
		}
	})
	t.Run("fps", func(t *testing.T) {
		for _, c := range []struct {
			avg, r string
			want   Rational
		}{
			{"30000/1001", "30000/1001", Rational{30000, 1001}},
			{"0/0", "60/1", Rational{60, 1}},
			{"0/1", "0/0", Rational{30, 1}},
			{"", "", Rational{30, 1}},
			{"25/0", "25/1", Rational{25, 1}},
			{"abc", "24", Rational{24, 1}},
			{"-30/1", "", Rational{30, 1}},
		} {
			if got := sourceFPS(c.avg, c.r); got != c.want {
				t.Errorf("sourceFPS(%q, %q) = %v, want %v", c.avg, c.r, got, c.want)
			}
		}
		// The r_frame_rate is kept alongside the average so the plan can see
		// a variable-rate source (§20 erratum 93); garbage leaves it invalid.
		for _, c := range []struct {
			r    string
			want Rational
		}{{"30/1", Rational{30, 1}}, {"1000/1", Rational{1000, 1}}, {"0/0", Rational{}}, {"", Rational{}}, {"junk", Rational{}}} {
			p, err := ParseProbe([]byte(`{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":640,"height":360,
				"avg_frame_rate":"3600/293","r_frame_rate":"` + c.r + `","duration":"2.0"}],
				"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"2.0"}}`))
			if err != nil {
				t.Fatalf("r_frame_rate %q: %v", c.r, err)
			}
			if p.RFPS != c.want || p.FPS != (Rational{3600, 293}) {
				t.Errorf("r_frame_rate %q: RFPS=%v FPS=%v, want RFPS=%v FPS=3600/293", c.r, p.RFPS, p.FPS, c.want)
			}
		}
	})
	t.Run("colour tags", func(t *testing.T) {
		// The plan cannot decide whether to tone map without them, and until
		// §20 erratum 93 ParseProbe did not read them at all.
		doc := func(prim, trc, space string) []byte {
			return []byte(`{"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","width":1920,"height":1080,
				"pix_fmt":"yuv420p10le","avg_frame_rate":"30/1","r_frame_rate":"30/1","duration":"2.0",
				"color_primaries":"` + prim + `","color_transfer":"` + trc + `","color_space":"` + space + `"}],
				"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"2.0"}}`)
		}
		p, err := ParseProbe(doc("bt2020", "smpte2084", "bt2020nc"))
		if err != nil {
			t.Fatal(err)
		}
		if p.ColorPrimaries != "bt2020" || p.ColorTransfer != "smpte2084" || p.ColorSpace != "bt2020nc" {
			t.Fatalf("HDR10 colour tags = %q/%q/%q", p.ColorPrimaries, p.ColorTransfer, p.ColorSpace)
		}
		if !MakePlan(p, PlanOptions{}).Tonemap {
			t.Fatal("an smpte2084 source must be planned with a tone map")
		}
		// A stream that declares nothing leaves them empty and SDR.
		bare, err := ParseProbe(fixture(t, "std-720p30.json"))
		if err != nil {
			t.Fatal(err)
		}
		if bare.ColorPrimaries != "" || bare.ColorTransfer != "" || bare.ColorSpace != "" {
			t.Fatalf("undeclared colour tags = %q/%q/%q", bare.ColorPrimaries, bare.ColorTransfer, bare.ColorSpace)
		}
		if MakePlan(bare, PlanOptions{}).Tonemap {
			t.Fatal("a source with no colour tags must not be tone mapped")
		}
		for _, trc := range []string{"smpte2084", "arib-std-b67", "SMPTE2084", " arib-std-b67 "} {
			if !isHDRTransfer(trc) {
				t.Errorf("isHDRTransfer(%q) = false", trc)
			}
		}
		for _, trc := range []string{"", "unknown", "bt709", "bt470bg", "smpte170m", "bt2020-10", "iec61966-2-1"} {
			if isHDRTransfer(trc) {
				t.Errorf("isHDRTransfer(%q) = true", trc)
			}
		}
	})
	t.Run("audio track cap", func(t *testing.T) {
		// An OBS scene is routinely five tracks (game, mic, Discord, music,
		// browser); the cap is eight and only a ninth is dropped, silently
		// and by design (§20 erratum 93).
		doc := func(n int) []byte {
			s := `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":1280,"height":720,
				"avg_frame_rate":"30/1","r_frame_rate":"30/1","duration":"2.0"}`
			for i := 1; i <= n; i++ {
				s += `,{"index":` + strconv.Itoa(i) + `,"codec_type":"audio","codec_name":"aac","duration":"2.0"}`
			}
			return []byte(s + `],"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"2.0"}}`)
		}
		for _, c := range []struct{ tracks, want int }{{1, 1}, {4, 4}, {5, 5}, {8, 8}, {9, 8}, {16, 8}} {
			p, err := ParseProbe(doc(c.tracks))
			if err != nil {
				t.Fatalf("%d tracks: %v", c.tracks, err)
			}
			if len(p.AudioIndexes) != c.want || len(p.AudioCodecs) != c.want {
				t.Errorf("%d tracks: %d indexes / %d codecs, want %d", c.tracks, len(p.AudioIndexes), len(p.AudioCodecs), c.want)
				continue
			}
			// They are the FIRST n, in file order, and the plan keeps them.
			for i, idx := range p.AudioIndexes {
				if idx != i+1 {
					t.Errorf("%d tracks: AudioIndexes = %v, want the first %d in file order", c.tracks, p.AudioIndexes, c.want)
					break
				}
			}
			plan := MakePlan(p, PlanOptions{})
			if len(plan.AudioIndexes) != c.want {
				t.Errorf("%d tracks: plan has %d inputs, want %d", c.tracks, len(plan.AudioIndexes), c.want)
			}
			if want := AudioMerge; c.want > 1 && plan.AudioMode != want {
				t.Errorf("%d tracks: mode = %v, want %v", c.tracks, plan.AudioMode, want)
			}
			if n := strings.Count(plan.FilterComplex(), "aresample=48000,aformat=channel_layouts=stereo"); c.want > 1 && n != c.want {
				t.Errorf("%d tracks: %d merge inputs in the graph, want %d", c.tracks, n, c.want)
			}
		}
	})
	t.Run("duration fallback to the video stream", func(t *testing.T) {
		p, err := ParseProbe([]byte(`{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":640,"height":360,"duration":"1.5"}],
			"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"N/A"}}`))
		if err != nil || !p.HasDuration || p.DurationUS != 1_500_000 {
			t.Fatalf("stream duration fallback: %+v, %v", p, err)
		}
		if _, ok := parseSeconds("0.000000"); ok {
			t.Fatal("zero duration must count as absent")
		}
	})
}

// TestProbeMissingBinary: a binary that cannot start is a plain (transient)
// error, never a permanent one; a cancelled ctx is ctx.Err().
func TestProbeMissingBinary(t *testing.T) {
	probe := Probe(filepath.Join(t.TempDir(), "no-such-ffprobe"))
	_, err := probe(context.Background(), "x.mp4")
	if err == nil || IsPermanent(err) {
		t.Fatalf("missing binary: err = %v, want plain error", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = probe(ctx, "x.mp4")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx: err = %v", err)
	}
}

// A Display Matrix angle that is not a quarter turn must not swap the
// display dims. ffmpeg's autorotate turns only quarter angles into a swapped
// frame — an odd angle is rotated in place on the original canvas — and the
// plan emits no rotate filter for a matrix source (PreRotate is for tag-only
// sources), so a swapped plan would scale an unrotated 640x360 frame into a
// 360x640 box and squish the rendition and its poster (§20 erratum 39).
func TestNonQuarterRotationDoesNotSwapDims(t *testing.T) {
	probeJSON := func(rotation string) []byte {
		return []byte(`{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":640,"height":360,
			"sample_aspect_ratio":"1:1","avg_frame_rate":"30/1","r_frame_rate":"30/1","duration":"2.0",
			"side_data_list":[{"side_data_type":"Display Matrix","rotation":` + rotation + `}]}],
			"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2"}}`)
	}
	for _, c := range []struct {
		rotation                string
		rot, dispW, dispH, w, h int
	}{
		{"45", 0, 640, 360, 640, 360},
		{"-45", 0, 640, 360, 640, 360},
		{"135", 0, 640, 360, 640, 360},
		{"90", 90, 360, 640, 360, 640},      // control: a real quarter turn still swaps
		{"89.9998", 90, 360, 640, 360, 640}, // control: fixed-point slop is a quarter turn
		{"180", 180, 640, 360, 640, 360},
	} {
		p, err := ParseProbe(probeJSON(c.rotation))
		if err != nil {
			t.Fatalf("rotation %s: ParseProbe: %v", c.rotation, err)
		}
		if p.Rotation != c.rot || p.DisplayW != c.dispW || p.DisplayH != c.dispH || p.RotationFromTag {
			t.Errorf("rotation %s: rot=%d display=%dx%d fromTag=%v, want rot=%d display=%dx%d",
				c.rotation, p.Rotation, p.DisplayW, p.DisplayH, p.RotationFromTag, c.rot, c.dispW, c.dispH)
			continue
		}
		plan := MakePlan(p, PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60})
		if plan.W != c.w || plan.H != c.h || plan.PreRotate != 0 {
			t.Errorf("rotation %s: plan %dx%d prerotate=%d, want %dx%d prerotate=0",
				c.rotation, plan.W, plan.H, plan.PreRotate, c.w, c.h)
		}
	}
}

// The Display Matrix angle ffprobe prints under "rotation" is a C cast of
// the real one — 89.4 and 89.6 both print 89 — while ffmpeg's autorotate
// rounds the raw angle and transposes only an exact quarter turn. Classify
// from the truncated integer and the two land on the same side of the
// boundary, so one of them plans a swapped W×H over a frame ffmpeg never
// transposed and the scale filter squeezes the rendition and its poster —
// the erratum-39 failure mode, from inside the tolerance. The matrices below
// are ffprobe 7.1.1's verbatim output for `-display_rotation 89.4 / 89.6`
// remuxes of a 640×360 source (§20 erratum 47).
func TestDisplayMatrixRotationMirrorsAutorotate(t *testing.T) {
	const (
		m894 = "\n00000000:          686      -65532           0\n00000001:        65532         686           0\n00000002:            0           0  1073741824\n"
		m896 = "\n00000000:          457      -65534           0\n00000001:        65534         457           0\n00000002:            0           0  1073741824\n"
		m90  = "\n00000000:            0      -65536           0\n00000001:        65536           0           0\n00000002:            0           0  1073741824\n"
		mn90 = "\n00000000:            0       65536           0\n00000001:       -65536           0           0\n00000002:            0           0  1073741824\n"
		m180 = "\n00000000:       -65536           0           0\n00000001:            0      -65536           0\n00000002:            0           0  1073741824\n"
		m45  = "\n00000000:        46340      -46340           0\n00000001:        46340       46340           0\n00000002:            0           0  1073741824\n"
	)
	t.Run("the raw angle survives the coefficients", func(t *testing.T) {
		for _, c := range []struct {
			matrix string
			want   float64
		}{{m894, 89.4}, {m896, 89.6}, {m90, 90}, {mn90, -90}, {m180, -180}, {m45, 45}} {
			got, ok := matrixRotation(c.matrix)
			if !ok || math.Abs(got-c.want) > 0.01 {
				t.Errorf("matrixRotation = %v (ok %v), want ≈%v", got, ok, c.want)
			}
		}
		for _, bad := range []string{"", "00000000: 1 2 3", "00000000: 1 2 3 4 5 6 7 8 9 10", "00000000: a b c\n00000001: d e f\n00000002: g h i",
			// A degenerate matrix (zero scale) has no angle to read.
			"00000000: 0 0 0\n00000001: 0 0 0\n00000002: 0 0 1073741824"} {
			if _, ok := matrixRotation(bad); ok {
				t.Errorf("matrixRotation(%q) accepted", bad)
			}
		}
	})
	t.Run("only ffmpeg's own quarter turns swap", func(t *testing.T) {
		for _, c := range []struct {
			raw  float64
			want int
		}{
			// Rounds to a quarter: transposed by ffmpeg, so the dims swap.
			{89.6, 90}, {90, 90}, {90.4, 90}, {89.9998, 90}, {-89.6, 270}, {-90, 270}, {180, 180}, {-180, 180}, {0, 0}, {0.4, 0},
			// Rounds to anything else: rotated in place on the original
			// canvas, so the dims must NOT swap.
			{89.4, 0}, {90.5, 0}, {91.4, 0}, {-89.4, 0}, {45, 0}, {135, 0}, {1, 0}, {179, 0},
			{math.NaN(), 0}, {math.Inf(1), 0}, {math.Inf(-1), 0},
		} {
			if got := autorotateQuarter(c.raw); got != c.want {
				t.Errorf("autorotateQuarter(%v) = %d, want %d", c.raw, got, c.want)
			}
		}
	})
	t.Run("plan dims follow the matrix, not the truncated key", func(t *testing.T) {
		probeJSON := func(matrix string) []byte {
			return []byte(`{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":640,"height":360,
				"sample_aspect_ratio":"1:1","avg_frame_rate":"30/1","r_frame_rate":"30/1","duration":"2.0",
				"side_data_list":[{"side_data_type":"Display Matrix","displaymatrix":"` +
				strings.ReplaceAll(matrix, "\n", `\n`) + `","rotation":89}]}],
				"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2"}}`)
		}
		for _, c := range []struct {
			name                    string
			matrix                  string
			rot, dispW, dispH, w, h int
		}{
			// Both entries carry the SAME "rotation": 89 that ffprobe prints.
			{"89.4 is not transposed by ffmpeg", m894, 0, 640, 360, 640, 360},
			{"89.6 is", m896, 90, 360, 640, 360, 640},
		} {
			p, err := ParseProbe(probeJSON(c.matrix))
			if err != nil {
				t.Fatalf("%s: ParseProbe: %v", c.name, err)
			}
			if p.Rotation != c.rot || p.DisplayW != c.dispW || p.DisplayH != c.dispH || p.RotationFromTag {
				t.Errorf("%s: rot=%d display=%dx%d fromTag=%v, want rot=%d display=%dx%d",
					c.name, p.Rotation, p.DisplayW, p.DisplayH, p.RotationFromTag, c.rot, c.dispW, c.dispH)
				continue
			}
			plan := MakePlan(p, PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60})
			if plan.W != c.w || plan.H != c.h || plan.PreRotate != 0 {
				t.Errorf("%s: plan %dx%d prerotate=%d, want %dx%d prerotate=0", c.name, plan.W, plan.H, plan.PreRotate, c.w, c.h)
			}
		}
	})
}
