package transcode

import (
	"strings"
	"testing"
)

// samplePlans is a source no browser plays (so a Full is wanted) that is also
// big and fast (so a Light copy is wanted): the exact shape erratum 217's single
// encode exists for.
func samplePlans() (main, light Plan) {
	p := &ProbeResult{DisplayW: 2560, DisplayH: 1440, CodedW: 2560, CodedH: 1440,
		FPS: Rational{60, 1}, RFPS: Rational{60, 1}, SARNum: 1, SARDen: 1,
		HasDuration: true, DurationUS: 30_000_000, VideoIndex: 0, AudioIndexes: []int{1},
		VideoCodec: "hevc", PixFmt: "yuv420p"}
	box := PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60}
	return MakePlan(p, FullPlanOptions(p, box)), MakePlan(p, LightPlanOptions(box))
}

// TestDualEncodeArgs: ONE decode, ONE split, each tier's own scale, and each
// tier's own encoder settings — and the Light copy alone carries the tag.
func TestDualEncodeArgs(t *testing.T) {
	mainPlan, lightPlan := samplePlans()
	if mainPlan.W != 2560 || mainPlan.H != 1440 {
		t.Fatalf("the Full plan is %dx%d, want the source's own size", mainPlan.W, mainPlan.H)
	}
	if lightPlan.W != 1280 || lightPlan.H != 720 || lightPlan.FPS.Num != 30 {
		t.Fatalf("the Light plan is %dx%d @ %s", lightPlan.W, lightPlan.H, lightPlan.FPS.String())
	}
	tag := LightTag{FontFile: "/f.ttf", TextFile: "/scratch/c/tag.txt"}
	base := EncodeOptions{Preset: "veryfast", CRF: FullCRF, Threads: 2, Codec: FullCodecH264}
	lightOpts := SocialEncodeOptions(base)
	lightOpts.Tag = tag
	args := DualEncodeArgs("/in.mkv",
		TierOutput{Plan: mainPlan, Path: "/out/full.mp4", Opts: base},
		TierOutput{Plan: lightPlan, Path: "/out/light.mp4", Opts: lightOpts})
	argv := strings.Join(args, " ")

	// One input, one filter_complex, two output files.
	if n := strings.Count(argv, "-i /in.mkv"); n != 1 {
		t.Fatalf("the argv reads the input %d times, want once — the whole point of erratum 217:\n%s", n, argv)
	}
	if n := strings.Count(argv, " -filter_complex "); n != 1 {
		t.Fatalf("%d filter graphs, want one:\n%s", n, argv)
	}
	for _, want := range []string{
		"split=2[m][l]",
		"[m]scale=2560:1440:flags=lanczos,setsar=1,format=yuv420p",
		"[l]scale=1280:720:flags=lanczos,setsar=1,format=yuv420p",
		"asplit=2[a0][a1]",
		"-map [v0] -map [a0]", "-map [v1] -map [a1]",
		"/out/full.mp4", "/out/light.mp4",
		// Each tier's own rate, quality and audio bitrate.
		"-r 60/1 -fps_mode cfr", "-r 30/1 -fps_mode cfr",
		"-crf 18", "-crf 26", "-b:a 128k", "-b:a 96k",
		// The Light copy's VBV ceiling, and only it.
		"-maxrate 2500000 -bufsize 5000000",
		"-threads 2",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("the dual argv lacks %q:\n%s", want, argv)
		}
	}
	// The tag is on the LIGHT branch and nowhere else.
	if n := strings.Count(argv, "drawtext"); n != 1 {
		t.Fatalf("%d drawtext filters, want exactly one (the Light copy's):\n%s", n, argv)
	}
	gi := indexOf(args, "-filter_complex")
	if gi < 0 {
		t.Fatal("no -filter_complex in the dual argv")
	}
	graph := args[gi+1]
	mainBranch, lightBranch, ok := strings.Cut(graph, ";[l]")
	if !ok {
		t.Fatalf("the graph has no [l] branch: %s", graph)
	}
	if strings.Contains(mainBranch, "drawtext") {
		t.Errorf("the FULL branch carries the tag — it must never:\n%s", mainBranch)
	}
	if !strings.Contains(lightBranch, "drawtext") {
		t.Errorf("the LIGHT branch does not carry the tag:\n%s", lightBranch)
	}
	// The head runs once, before the split: the expensive filters are not paid twice.
	if strings.Count(argv, "scale=") != 2 {
		t.Errorf("want exactly two scales (one per tier):\n%s", argv)
	}
}

// TestDualEncodeArgsNoAudio: a silent source maps no audio pad on either output
// and renders no asplit at all.
func TestDualEncodeArgsNoAudio(t *testing.T) {
	mainPlan, lightPlan := samplePlans()
	for _, p := range []*Plan{&mainPlan, &lightPlan} {
		p.AudioMode, p.AudioIndexes = AudioNone, nil
	}
	argv := strings.Join(DualEncodeArgs("/in.mkv",
		TierOutput{Plan: mainPlan, Path: "/a.mp4", Opts: EncodeOptions{Preset: "veryfast", CRF: 18}},
		TierOutput{Plan: lightPlan, Path: "/b.mp4", Opts: EncodeOptions{Preset: "veryfast", CRF: 26}}), " ")
	if strings.Contains(argv, "asplit") || strings.Contains(argv, "-c:a") {
		t.Errorf("a silent source rendered an audio chain:\n%s", argv)
	}
	if n := strings.Count(argv, "-an"); n != 2 {
		t.Errorf("%d -an, want one per output:\n%s", n, argv)
	}
}

// TestDualEncodeArgsCUDA: the two-output graph on the card. `split` is on the
// decoded DEVICE frames, each branch scales on the card, and only the tagged
// branch comes back to system memory.
func TestDualEncodeArgsCUDA(t *testing.T) {
	mainPlan, lightPlan := samplePlans()
	tag := LightTag{FontFile: "/f.ttf", TextFile: "/t.txt"}
	main := EncodeOptions{Preset: "veryfast", CRF: FullCRF, HWAccel: HWAccelCUDA, Codec: FullCodecH264}
	lightOpts := SocialEncodeOptions(main)
	lightOpts.Tag = tag
	argv := strings.Join(DualEncodeArgs("/in.mkv",
		TierOutput{Plan: mainPlan, Path: "/full.mp4", Opts: main},
		TierOutput{Plan: lightPlan, Path: "/light.mp4", Opts: lightOpts}), " ")
	for _, want := range []string{
		"-hwaccel cuda -hwaccel_output_format cuda", "split=2[m][l]",
		"[m]scale_cuda=2560:1440", "[l]scale_cuda=1280:720",
		"hwdownload,format=yuv420p,drawtext=", "-bsf:v " + h264MetadataBSF,
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("the CUDA dual argv lacks %q:\n%s", want, argv)
		}
	}
	if strings.Contains(argv, "-colorspace") || strings.Contains(argv, "setparams") {
		t.Errorf("the CUDA dual argv carries a filter or option a CUDA graph cannot survive:\n%s", argv)
	}
	// An AV1 Full has no NVENC encoder, so the SHARED graph falls back whole:
	// one decode cannot be on the card for one output and off it for the other.
	av1 := main
	av1.Codec = FullCodecAV1
	fell := strings.Join(DualEncodeArgs("/in.mkv",
		TierOutput{Plan: mainPlan, Path: "/full.mp4", Opts: av1},
		TierOutput{Plan: lightPlan, Path: "/light.mp4", Opts: lightOpts}), " ")
	if strings.Contains(fell, "scale_cuda") {
		t.Errorf("an AV1 Full still rendered a CUDA graph:\n%s", fell)
	}
	if !strings.Contains(fell, "libsvtav1") || !strings.Contains(fell, "libx264") {
		t.Errorf("the fallback did not render both CPU encoders:\n%s", fell)
	}
}

// TestSingleEncodeArgvUnchanged is the regression guard on the refactor erratum
// 217 needed: the ordinary CPU encode's argv must be byte-for-byte what it was
// before there were two outputs. The literal below is the shipped argv.
func TestSingleEncodeArgvUnchanged(t *testing.T) {
	p := Plan{W: 1280, H: 720, FPS: Rational{60, 1}, GOP: 120, KeyintMin: 60,
		AudioMode: AudioSingle, AudioIndexes: []int{1}, VideoIndex: 0, HasDuration: true, DurationUS: 6_000_000}
	got := strings.Join(p.EncodeArgs("/in.mp4", "/out.mp4", EncodeOptions{Preset: "veryfast", CRF: 23}), " ")
	want := "-hide_banner -nostdin -loglevel error -nostats -progress pipe:1 -y " +
		"-protocol_whitelist file -i /in.mp4 " +
		"-filter_complex [0:0]scale=1280:720:flags=lanczos,setsar=1,format=yuv420p," +
		"setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709[v];[0:1]aresample=48000[a] " +
		"-map [v] -map [a] -sn -dn -map_metadata -1 -map_chapters -1 -r 60/1 -fps_mode cfr " +
		"-c:v libx264 -preset veryfast -crf 23 -profile:v high " +
		"-pix_fmt yuv420p -color_primaries bt709 -color_trc bt709 -colorspace bt709 " +
		"-g 120 -keyint_min 60 -c:a aac -b:a 128k -ar 48000 -ac 2 " +
		"-max_muxing_queue_size 4096 -movflags +faststart -f mp4 /out.mp4"
	if got != want {
		t.Fatalf("the ordinary encode's argv changed.\n got: %s\nwant: %s", got, want)
	}
}
