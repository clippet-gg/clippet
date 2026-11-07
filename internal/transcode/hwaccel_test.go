package transcode

import (
	"strings"
	"testing"
)

// TestHWAccelFor: the CUDA path is a FAST PATH with a CPU fallback, never a
// replacement (§20 erratum 216). Every plan that needs a CPU-only filter —
// yadif, the tone map's zscales, transpose, setsar — falls back, and the reason
// names the filter so an operator can check it against probe.json.
func TestHWAccelFor(t *testing.T) {
	base := Plan{W: 1920, H: 1080, FPS: Rational{60, 1}, VideoIndex: 0}
	for _, tc := range []struct {
		name       string
		plan       Plan
		configured string
		codec      string
		want       string
		why        string
	}{
		{name: "off by default", plan: base, configured: "", want: HWAccelNone, why: hwWhyOff},
		{name: "the word none", plan: base, configured: "none", want: HWAccelNone, why: hwWhyOff},
		{name: "an ordinary capture", plan: base, configured: "cuda", want: HWAccelCUDA},
		{name: "case does not matter", plan: base, configured: "CUDA", want: HWAccelCUDA},
		{name: "the light copy carries no codec", plan: base, configured: "cuda", codec: "", want: HWAccelCUDA},
		{name: "an h264 full", plan: base, configured: "cuda", codec: FullCodecH264, want: HWAccelCUDA},
		{name: "an av1 full stays on the cpu", plan: base, configured: "cuda", codec: FullCodecAV1,
			want: HWAccelNone, why: hwWhyCodec},
		{name: "hdr", plan: withPlan(base, func(p *Plan) { p.Tonemap = true }), configured: "cuda",
			want: HWAccelNone, why: hwWhyTonemap},
		{name: "interlaced", plan: withPlan(base, func(p *Plan) { p.Deinterlace = true }), configured: "cuda",
			want: HWAccelNone, why: hwWhyDeinterlace},
		{name: "tag-rotated", plan: withPlan(base, func(p *Plan) { p.PreRotate = 90 }), configured: "cuda",
			want: HWAccelNone, why: hwWhyRotate},
		{name: "anamorphic", plan: withPlan(base, func(p *Plan) { p.Anamorphic = true }), configured: "cuda",
			want: HWAccelNone, why: hwWhyAnamorphic},
		// §20 erratum 247: what NVDEC cannot decode stays on the CPU argv, which
		// handles it — `-hwaccel cuda` on such a source silently decodes in
		// software and scale_cuda then dies with a zero-byte file.
		{name: "an 8-bit h264 source", plan: withPlan(base, func(p *Plan) { p.SourceCodec, p.SourcePixFmt = "h264", "yuv420p" }),
			configured: "cuda", want: HWAccelCUDA},
		{name: "a full-range jpeg-yuv source", plan: withPlan(base, func(p *Plan) { p.SourceCodec, p.SourcePixFmt = "hevc", "yuvj420p" }),
			configured: "cuda", want: HWAccelCUDA},
		{name: "a 10-bit h264 source (a capture card's High 10)", plan: withPlan(base, func(p *Plan) { p.SourceCodec, p.SourcePixFmt = "h264", "yuv420p10le" }),
			configured: "cuda", want: HWAccelNone, why: hwWhyDecoder},
		{name: "a 4:4:4 source", plan: withPlan(base, func(p *Plan) { p.SourceCodec, p.SourcePixFmt = "h264", "yuv444p" }),
			configured: "cuda", want: HWAccelNone, why: hwWhyDecoder},
		{name: "prores has no hardware decoder", plan: withPlan(base, func(p *Plan) { p.SourceCodec, p.SourcePixFmt = "prores", "yuv422p10le" }),
			configured: "cuda", want: HWAccelNone, why: hwWhyDecoder},
		{name: "codec case does not matter", plan: withPlan(base, func(p *Plan) { p.SourceCodec, p.SourcePixFmt = "H264", "YUV420P" }),
			configured: "cuda", want: HWAccelCUDA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accel, why := HWAccelFor(tc.plan, tc.configured, tc.codec)
			if accel != tc.want || why != tc.why {
				t.Fatalf("HWAccelFor = %q, %q; want %q, %q", accel, why, tc.want, tc.why)
			}
			if (accel == HWAccelCUDA) != (why == "") {
				t.Fatalf("accel %q and why %q disagree: a reason is exactly what a fallback has", accel, why)
			}
		})
	}
}

func withPlan(p Plan, fn func(*Plan)) Plan {
	fn(&p)
	return p
}

// TestNVENCCQ: NVENC's quantizer is not x264's — at the same number it wrote
// 1.8–4× the bytes — so the CRF is offset, and the encoder's own ceiling holds.
func TestNVENCCQ(t *testing.T) {
	// The two MEASURED points: the Full's CRF 18 lands on cq 22 and the Light
	// copy's CRF 26 on cq 34, each within ±15 % of the x264 file it replaces.
	if got := NVENCCQ(FullCRF, NVENCCQOffsetFull); got != 22 {
		t.Errorf("the Full's cq = %d, want 22", got)
	}
	if got := NVENCCQ(SocialCRF, NVENCCQOffsetLight); got != 34 {
		t.Errorf("the Light copy's cq = %d, want 34", got)
	}
	// An encode that named no tier (a trim at TRANSCODE_CRF) gets the middle.
	if got := NVENCCQ(23, 0); got != 29 {
		t.Errorf("the default offset gave cq %d, want 29", got)
	}
	// The encoder's own ceiling holds, and nothing goes negative.
	for _, tc := range []struct{ crf, offset, want int }{
		{51, 4, 51}, {48, 8, 51}, {0, 4, 4}, {0, -8, 0},
	} {
		if got := NVENCCQ(tc.crf, tc.offset); got != tc.want {
			t.Errorf("NVENCCQ(%d, %d) = %d, want %d", tc.crf, tc.offset, got, tc.want)
		}
	}
}

// TestCUDAArgv pins the whole CUDA argv, because two of its properties are
// invisible until a run dies:
//
//   - the colour tags MUST be a bitstream filter and NOT `-colorspace`, which
//     forces buffersink negotiation, inserts a CPU auto_scale a CUDA graph
//     cannot connect to, and exits 218 with a zero-byte file;
//   - not one CPU-only filter may appear in the graph.
func TestCUDAArgv(t *testing.T) {
	p := Plan{W: 1280, H: 720, FPS: Rational{30, 1}, GOP: 60, KeyintMin: 30,
		AudioMode: AudioSingle, AudioIndexes: []int{1}, VideoIndex: 0}
	args := p.EncodeArgs("/in.mp4", "/out.mp4", EncodeOptions{
		Preset: "veryfast", CRF: 26, HWAccel: HWAccelCUDA, Threads: 4})
	argv := strings.Join(args, " ")

	for _, want := range []string{
		"-hwaccel cuda -hwaccel_output_format cuda",
		"scale_cuda=1280:720:interp_algo=lanczos:format=yuv420p",
		"-c:v " + NVENCH264, "-preset " + NVENCPreset, "-tune " + NVENCTune,
		// CRF 26 with no tier offset named: the default +6.
		"-rc vbr -cq 32 -b:v 0",
		"-bsf:v " + h264MetadataBSF,
		"-threads 4", "-filter_threads 4", "-filter_complex_threads 4",
		"-r 30/1 -fps_mode cfr", "-movflags +faststart -f mp4 /out.mp4",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("the CUDA argv lacks %q:\n%s", want, argv)
		}
	}
	for _, forbidden := range []string{
		"-colorspace", "-color_primaries", "-color_trc", // the §5 trap
		"setparams", "setsar", "yadif", "zscale", "tonemap", "transpose",
		"scale=", // the CPU scaler, whose name is a prefix of nothing here
		"libx264", "-pix_fmt",
	} {
		if strings.Contains(argv, forbidden) {
			t.Errorf("the CUDA argv still carries %q:\n%s", forbidden, argv)
		}
	}
	// The audio half is untouched: audio was never on the card.
	if !strings.Contains(argv, "-c:a aac") || !strings.Contains(argv, "aresample=48000") {
		t.Errorf("the CUDA argv lost its audio chain:\n%s", argv)
	}

	// And the fallback renders the argv this package has always rendered: an
	// interlaced plan asked for cuda is byte-for-byte the CPU one.
	inter := withPlan(p, func(p *Plan) { p.Deinterlace = true })
	cpu := strings.Join(inter.EncodeArgs("/in.mp4", "/out.mp4", EncodeOptions{Preset: "veryfast", CRF: 26}), " ")
	asked := strings.Join(inter.EncodeArgs("/in.mp4", "/out.mp4",
		EncodeOptions{Preset: "veryfast", CRF: 26, HWAccel: HWAccelCUDA}), " ")
	if cpu != asked {
		t.Errorf("a plan that cannot take the CUDA path did not render the CPU argv:\n%s\n%s", cpu, asked)
	}
}
