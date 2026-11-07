package transcode

import (
	"strings"
	"testing"
)

// §20 erratum 352: a source turned by its Display Matrix on the CUDA graph.
// ffmpeg's autorotate inserts its `transpose` only on software frames, so on
// device frames the picture reached `scale_cuda` unturned (squeezed) and the
// output kept the matrix. These pin the argv; cudarotate_ffmpeg_test.go pins
// the pixels.

// phoneLight is the owner's phone clip of 2026-10-29 as the Light pass saw it: coded
// 626×1234, matrix rotation 90 (displayed 1234×626), audio stream 0 and video
// stream 1 in the upload, the Light encoded from the remux (§20 erratum 351).
func phoneLight() Plan {
	p := &ProbeResult{CodedW: 626, CodedH: 1234, DisplayW: 1234, DisplayH: 626, Rotation: 90,
		SARNum: 1, SARDen: 1, FPS: Rational{30, 1}, RFPS: Rational{30, 1}, HasDuration: true, DurationUS: 20_000_000,
		VideoIndex: 1, AudioIndexes: []int{0}, VideoCodec: "h264", PixFmt: "yuvj420p"}
	return MakePlan(p, LightPlanOptions(PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60})).ForProducedFile(true)
}

func cudaOpts() EncodeOptions {
	return EncodeOptions{Preset: "veryfast", CRF: 23, HWAccel: HWAccelCUDA, Codec: FullCodecH264}
}

func filterOf(t *testing.T, argv []string) string {
	t.Helper()
	for i, a := range argv {
		if a == "-filter_complex" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	t.Fatalf("no -filter_complex in %q", argv)
	return ""
}

func TestMakePlanMatrixTurn(t *testing.T) {
	for _, c := range []struct {
		rot       int
		fromTag   bool
		mirrored  bool
		turn, pre int
	}{
		{rot: 0},
		{rot: 90, turn: 270},  // ffprobe rotation 90 → autorotate's transpose=cclock
		{rot: 270, turn: 90},  // ffprobe rotation -90 → transpose=clock
		{rot: 180, turn: 180}, // hflip,vflip
		{rot: 90, fromTag: true, pre: 90},
		{rot: 90, mirrored: true, turn: 270},
	} {
		p := MakePlan(&ProbeResult{DisplayW: 720, DisplayH: 1280, Rotation: c.rot, RotationFromTag: c.fromTag,
			RotationMirrored: c.mirrored, FPS: Rational{30, 1}}, PlanOptions{})
		if p.MatrixTurn != c.turn || p.PreRotate != c.pre || p.MatrixMirrored != c.mirrored {
			t.Errorf("rotation %d (tag %v, mirrored %v): MatrixTurn %d PreRotate %d mirrored %v; want %d %d %v",
				c.rot, c.fromTag, c.mirrored, p.MatrixTurn, p.PreRotate, p.MatrixMirrored, c.turn, c.pre, c.mirrored)
		}
	}
}

// TestCUDAMatrixTurnGraph: that phone clip on the card — `-display_rotation:v 0`
// in front of the input, the scale to the CODED orientation, the download, the
// turn, then the tag in the corner the viewer sees.
func TestCUDAMatrixTurnGraph(t *testing.T) {
	p := phoneLight()
	if p.W != 1234 || p.H != 626 || p.MatrixTurn != 270 || p.VideoIndex != 0 {
		t.Fatalf("plan %dx%d turn %d video %d; want 1234x626 turn 270 video 0", p.W, p.H, p.MatrixTurn, p.VideoIndex)
	}
	o := SocialEncodeOptions(cudaOpts())
	o.Tag = LightTag{FontFile: "/f.ttf", TextFile: "/t.txt"}
	argv := p.EncodeArgs("/in.mp4", "/out.mp4", o)
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "-hwaccel cuda -hwaccel_output_format cuda -display_rotation:v 0 -protocol_whitelist file -i /in.mp4") {
		t.Fatalf("the input does not reset the display matrix right before -i:\n%s", joined)
	}
	fc := filterOf(t, argv)
	want := "[0:0]scale_cuda=626:1234:interp_algo=lanczos:format=yuv420p,hwdownload,format=yuv420p,transpose=2" +
		drawtextFilter(o.Tag, 626) + "[v];[0:1]aresample=48000[a]"
	if fc != want {
		t.Fatalf("graph\n got: %s\nwant: %s", fc, want)
	}

	// No tag: the turn closes the branch. 270 ffprobe (clockwise 90) is transpose=1.
	q := p
	q.MatrixTurn = 90
	if fc := filterOf(t, q.EncodeArgs("/in.mp4", "/out.mp4", cudaOpts())); !strings.HasPrefix(fc,
		"[0:0]scale_cuda=626:1234:interp_algo=lanczos:format=yuv420p,hwdownload,format=yuv420p,transpose=1[v]") {
		t.Fatalf("a clockwise quarter turn: %s", fc)
	}
	// A half turn keeps the scale's orientation.
	q.MatrixTurn = 180
	if fc := filterOf(t, q.EncodeArgs("/in.mp4", "/out.mp4", cudaOpts())); !strings.HasPrefix(fc,
		"[0:0]scale_cuda=1234:626:interp_algo=lanczos:format=yuv420p,hwdownload,format=yuv420p,hflip,vflip[v]") {
		t.Fatalf("a half turn: %s", fc)
	}
}

// TestCUDAUnturnedGraphUnchanged: a source with no matrix renders exactly the
// CUDA argv it always did — no input option, one filter, frames on the card.
func TestCUDAUnturnedGraphUnchanged(t *testing.T) {
	p := basePlan(AudioSingle, []int{1})
	argv := p.EncodeArgs("/in.mp4", "/out.mp4", cudaOpts())
	if strings.Contains(strings.Join(argv, " "), "display_rotation") {
		t.Fatalf("an unturned source carries -display_rotation: %q", argv)
	}
	if fc := filterOf(t, argv); fc != "[0:0]scale_cuda=1280:720:interp_algo=lanczos:format=yuv420p[v];[0:1]aresample=48000[a]" {
		t.Fatalf("the unturned CUDA graph changed: %s", fc)
	}
}

// TestCPUArgvIgnoresMatrixTurn: the CPU graph — and the NVENC-only fallback of
// §20 erratum 265, which decodes on the CPU — leave the turn to autorotate, so
// a matrix-turned plan renders byte for byte what it rendered before.
func TestCPUArgvIgnoresMatrixTurn(t *testing.T) {
	turned := phoneLight()
	plain := turned
	plain.MatrixTurn = 0
	cpu := EncodeOptions{Preset: "veryfast", CRF: 23}
	if a, b := strings.Join(turned.EncodeArgs("/i", "/o", cpu), " "), strings.Join(plain.EncodeArgs("/i", "/o", cpu), " "); a != b {
		t.Fatalf("the CPU argv moved:\n%s\n%s", a, b)
	}
	if a, b := strings.Join(turned.encodeArgs("/i", "/o", cudaOpts(), hwAccelNVENCOnly), " "),
		strings.Join(plain.encodeArgs("/i", "/o", cudaOpts(), hwAccelNVENCOnly), " "); a != b {
		t.Fatalf("the NVENC-only fallback's argv moved:\n%s\n%s", a, b)
	}
	m, l := TierOutput{Plan: turned, Path: "/f", Opts: cpu}, TierOutput{Plan: turned, Path: "/l", Opts: SocialEncodeOptions(cpu)}
	pm, pl := m, l
	pm.Plan, pl.Plan = plain, plain
	if a, b := strings.Join(DualEncodeArgs("/i", m, l), " "), strings.Join(DualEncodeArgs("/i", pm, pl), " "); a != b {
		t.Fatalf("the CPU dual argv moved:\n%s\n%s", a, b)
	}
	if strings.Contains(strings.Join(DualEncodeArgs("/i", m, l), " "), "display_rotation") {
		t.Fatal("the CPU dual argv carries -display_rotation")
	}
}

// TestCUDADualMatrixTurn: the Full of a turned source on the card had the same
// defect; both branches turn after their own scale, the input resets once.
func TestCUDADualMatrixTurn(t *testing.T) {
	p := &ProbeResult{CodedW: 1080, CodedH: 1920, DisplayW: 1920, DisplayH: 1080, Rotation: 90,
		SARNum: 1, SARDen: 1, FPS: Rational{60, 1}, RFPS: Rational{60, 1}, HasDuration: true, DurationUS: 10_000_000,
		VideoIndex: 0, AudioIndexes: []int{1}, VideoCodec: "h264", PixFmt: "yuv420p"}
	box := PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60}
	main, light := MakePlan(p, FullPlanOptions(p, box)), MakePlan(p, LightPlanOptions(box))
	lo := SocialEncodeOptions(cudaOpts())
	argv := DualEncodeArgs("/in.mp4", TierOutput{Plan: main, Path: "/f", Opts: cudaOpts()}, TierOutput{Plan: light, Path: "/l", Opts: lo})
	joined := strings.Join(argv, " ")
	if strings.Count(joined, "-display_rotation:v 0") != 1 || !strings.Contains(joined, "-display_rotation:v 0 -protocol_whitelist file -i /in.mp4") {
		t.Fatalf("the dual input does not reset the matrix once, before -i:\n%s", joined)
	}
	fc := filterOf(t, argv)
	for _, want := range []string{
		"[0:0]split=2[m][l]",
		";[m]scale_cuda=1080:1920:interp_algo=lanczos:format=yuv420p,hwdownload,format=yuv420p,transpose=2[v0]",
		";[l]scale_cuda=720:1280:interp_algo=lanczos:format=yuv420p,hwdownload,format=yuv420p,transpose=2[v1]",
	} {
		if !strings.Contains(fc, want) {
			t.Errorf("the dual graph lacks %q:\n%s", want, fc)
		}
	}
}

func TestHWAccelForMirroredMatrix(t *testing.T) {
	base := Plan{W: 1280, H: 720, FPS: Rational{30, 1}, MatrixTurn: 270}
	if accel, why := HWAccelFor(base, HWAccelCUDA, ""); accel != HWAccelCUDA || why != "" {
		t.Fatalf("a pure matrix turn must stay on the card: %q %q", accel, why)
	}
	base.MatrixMirrored = true
	if accel, why := HWAccelFor(base, HWAccelCUDA, ""); accel != HWAccelNone || why != hwWhyMirror {
		t.Fatalf("a mirrored matrix: %q %q, want the CPU graph with %q", accel, why, hwWhyMirror)
	}
}

// TestForProducedFileRotation: the remux carries the matrix, so the Light from
// it keeps the turn; an encoded Full is already turned and matrix-free, so a
// Light from it must not turn it again (nor redo any other head filter).
func TestForProducedFileRotation(t *testing.T) {
	p := phoneLight()
	if p.MatrixTurn != 270 {
		t.Fatalf("from the remux: MatrixTurn %d, want 270", p.MatrixTurn)
	}
	src := Plan{W: 1234, H: 626, VideoIndex: 1, AudioIndexes: []int{0}, AudioMode: AudioSingle,
		MatrixTurn: 270, MatrixMirrored: true, PreRotate: 90, Tonemap: true, Deinterlace: true, Anamorphic: true}
	got := src.ForProducedFile(false)
	if got.MatrixTurn != 0 || got.MatrixMirrored || got.PreRotate != 0 || got.Tonemap || got.Deinterlace || got.Anamorphic {
		t.Fatalf("from an encoded Full every head flag must be off: %+v", got)
	}
	if got.VideoIndex != 0 || got.AudioIndexes[0] != 1 || got.W != 1234 || got.H != 626 {
		t.Fatalf("from an encoded Full: %+v", got)
	}
}

func TestMatrixMirroredOf(t *testing.T) {
	sd := func(m string) ffprobeStream {
		return ffprobeStream{SideDataList: []ffprobeSideData{{Type: sideDataDisplayMatrix, DisplayMatrix: m}}}
	}
	phone := "\n00000000:            0      -65536           0\n00000001:        65536           0           0\n00000002:            0    41025536  1073741824\n"
	if matrixMirroredOf(sd(phone)) {
		t.Error("the phone's pure rotation read as mirrored")
	}
	if !matrixMirroredOf(sd("00000000: -65536 0 0\n00000001: 0 65536 0\n00000002: 0 0 1073741824")) {
		t.Error("an hflip matrix read as unmirrored")
	}
	if !matrixMirroredOf(sd("00000000: 0 65536 0\n00000001: 65536 0 0\n00000002: 0 0 1073741824")) {
		t.Error("a transpose (turn + flip) matrix read as unmirrored")
	}
	if matrixMirroredOf(ffprobeStream{}) || matrixMirroredOf(sd("garbage")) {
		t.Error("no matrix / an unreadable one must read as unmirrored")
	}
}
