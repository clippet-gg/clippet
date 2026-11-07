package transcode

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestNVENCOnlyArgv pins the fallback shape of §20 erratum 265: the CPU argv
// with the ENCODER swapped, and nothing else.
//
// The property that matters is not which options are present but that the graph
// is the CPU graph — a fallback that quietly rendered a third filter chain would
// be a second pipeline nobody tests, and the whole reason the decode can move at
// all is that the chain feeding a CPU decode already exists.
func TestNVENCOnlyArgv(t *testing.T) {
	p := basePlan(AudioSingle, []int{1})
	o := EncodeOptions{Preset: "veryfast", CRF: 26, Threads: 4}
	cpu := p.encodeArgs("/in.mp4", "/out.mp4", o, HWAccelNone)
	nv := p.encodeArgs("/in.mp4", "/out.mp4", o, hwAccelNVENCOnly)
	cuda := p.encodeArgs("/in.mp4", "/out.mp4", o, HWAccelCUDA)

	// The graph is the CPU one, character for character — and it is NOT the CUDA
	// one, which is the whole point of re-rendering rather than patching an argv.
	if graphOf(t, nv) != graphOf(t, cpu) {
		t.Errorf("the fallback graph is not the CPU graph:\n%s\n%s", graphOf(t, nv), graphOf(t, cpu))
	}
	if graphOf(t, nv) == graphOf(t, cuda) {
		t.Error("the fallback graph is still the CUDA one")
	}

	argv := strings.Join(nv, " ")
	for _, want := range []string{
		// The software decode: no `-hwaccel` of any kind in front of the input.
		"-protocol_whitelist file -i /in.mp4 -filter_complex",
		"[0:0]scale=1280:720:flags=lanczos,setsar=1,format=yuv420p" + colourStamp + "[v]",
		// The encoder is still the card's, with the same calibrated quantizer the
		// CUDA path would have used (CRF 26 + the default offset).
		"-c:v " + NVENCH264, "-preset " + NVENCPreset, "-tune " + NVENCTune, "-rc vbr -cq 32 -b:v 0",
		"-bsf:v " + h264MetadataBSF,
		"-r 30/1 -fps_mode cfr", "-c:a aac", "-movflags +faststart -f mp4 /out.mp4",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("the fallback argv lacks %q:\n%s", want, argv)
		}
	}
	// The decode is the thing that moved, and with it the whole device graph
	// (`-hwaccel`, `scale_cuda`, `hwdownload`); the encoder did not move
	// (`libx264`); and it is still an NVENC output, so erratum 216's colour trap
	// still applies to it.
	for _, forbidden := range []string{
		"-hwaccel", "scale_cuda", "hwdownload", "libx264",
		"-colorspace", "-color_primaries", "-color_trc", "-pix_fmt",
	} {
		if strings.Contains(argv, forbidden) {
			t.Errorf("the fallback argv still carries %q:\n%s", forbidden, argv)
		}
	}
}

// TestNVENCOnlyArgvTagAndTiers: the two argv shapes that are not a plain
// single-output encode render the fallback too — the Light copy's burned-in tag
// (a CPU filter that the CUDA path has to `hwdownload` for, and this one does
// not) and the two-output pass of erratum 217.
func TestNVENCOnlyArgvTagAndTiers(t *testing.T) {
	mainPlan, lightPlan := samplePlans()
	tag := LightTag{FontFile: "/f.ttf", TextFile: "/scratch/c/tag.txt"}
	main := TierOutput{Plan: mainPlan, Path: "/full.mp4",
		Opts: EncodeOptions{Preset: "veryfast", CRF: FullCRF, CQOffset: NVENCCQOffsetFull}}
	light := TierOutput{Plan: lightPlan, Path: "/light.mp4",
		Opts: EncodeOptions{Preset: "veryfast", CRF: SocialCRF, CQOffset: NVENCCQOffsetLight, Tag: tag}}
	argv := strings.Join(dualEncodeArgs("/in.mp4", main, light, hwAccelNVENCOnly), " ")

	// The tag is an ordinary link of a CPU chain here, and each tier keeps its
	// own measured quantizer.
	for _, want := range []string{
		"split=2[m][l]",
		";[m]scale=2560:1440:flags=lanczos,setsar=1,format=yuv420p" + colourStamp + "[v0]",
		"drawtext=", "-cq 22", "-cq 34",
		"-f mp4 /full.mp4", "-f mp4 /light.mp4",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("the two-output fallback argv lacks %q:\n%s", want, argv)
		}
	}
	for _, forbidden := range []string{"-hwaccel", "scale_cuda", "hwdownload", "libx264"} {
		if strings.Contains(argv, forbidden) {
			t.Errorf("the two-output fallback argv still carries %q:\n%s", forbidden, argv)
		}
	}
	if n := strings.Count(argv, "-c:v "+NVENCH264); n != 2 {
		t.Errorf("both outputs must be NVENC ones; got %d:\n%s", n, argv)
	}
}

// graphOf returns the -filter_complex value of an argv.
func graphOf(t *testing.T, args []string) string {
	t.Helper()
	for i, a := range args {
		if a == "-filter_complex" && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("no -filter_complex in %v", args)
	return ""
}

// hwInitStderr is the failure this whole mechanism exists for, as the reference
// stack's ffmpeg 7.1.1 printed it on a 1440p60 ShadowPlay capture (§20 erratum
// 265; the pointer values are elided, nothing else is).
//
// The `-progress` block comes first because that is what a refused run really
// emits (measured in the gpu image): ffmpeg prints its final block whatever
// happened, so a run that wrote nothing still announces `total_size=0` and
// `progress=end`. Both halves are load-bearing — the zero size must not read as
// output, and the end must not reach a monotone progress bar.
const hwInitStderr = `echo 'frame=0'
echo 'total_size=0'
echo 'out_time_us=N/A'
echo 'progress=end'
echo "[h264 @ 0x7f10] decoder->cvdl->cuvidCreateDecoder(&decoder->decoder, params) failed -> CUDA_ERROR_INVALID_VALUE: invalid argument" >&2
echo "[h264 @ 0x7f10] Failed setup for format cuda: hwaccel initialisation returned error." >&2
echo "Impossible to convert between the formats supported by the filter 'graph 0 input from stream 0:0' and the filter 'auto_scale_0'" >&2
echo "Error reinitializing filters!" >&2
echo "[out#0/mp4 @ 0x7f10] Nothing was written into output file, because at least one of its streams received no packets." >&2
exit 218`

// The other endings a run can have, as sh fragments.
const (
	ffOK = `echo 'out_time_us=6000000'
echo 'progress=end'
exit 0`
	// A decode that got going and then hit bad bytes: a CUDA run, a permanent
	// failure, and none of it the decoder refusing the stream.
	ffMidStream = `echo 'out_time_us=3000000'
echo 'progress=continue'
echo "[h264 @ 0x7f10] Error while decoding stream #0:0: Invalid data found when processing input" >&2
exit 1`
	// The refusal signature AFTER the output file has bytes in it. It cannot
	// happen — a decoder refuses a stream before the first frame — and if it ever
	// does, the run is not re-run.
	ffInitAfterOutput = `echo 'total_size=65536'
echo 'progress=continue'
` + hwInitStderr
	ffBroken = `echo "[libx264 @ 0x7f10] the fallback failed too" >&2
exit 1`
)

// hwInitStderrLongTrailer — the fixture `shadowplay-1440p30-long-trailer` — is
// the same NVDEC refusal as above with everything ffmpeg really prints on its
// way out after one. It is the stderr of the FIRST `/clip` ever to reach a
// Clippet through the official bot (clip NCrggyXXNUpT, 2026-09-16; §20 erratum
// 289): a 2560×1440 30 fps H.264 yuv420p ShadowPlay-class capture, 37.9 MB,
// 19.7 s — erratum 265's proof shape, one size and one frame rate apart.
//
// The trailer is the whole point of the fixture. 973 bytes of it follow the last
// signature line, so the last `failureTail` bytes — the failure REASON, and until
// erratum 289 what the CLASSIFIER was handed as well — open inside the word
// `initialisation` and carry not one of the three signatures. That clip was
// `failed` permanently on a card that would have decoded it on the CPU, and its
// recorded reason began mid-word: `ed error.` and then `Impossible to convert…`.
//
// The shutdown lines are not invented. They are ffmpeg 7.1.1's own, captured on
// this host by failing a filter negotiation under the same
// `-hide_banner -nostdin -loglevel error -nostats` every encode argv in this
// package carries: `Error reinitializing filters!`, a `Task finished` / `Terminating
// thread` pair per component, a `Could not open encoder before EOF` per output
// stream, and the muxer's last word. Pointer values are elided, nothing else is.
const hwInitStderrLongTrailer = `[h264 @ 0x7f2b4c0a2e40] decoder->cvdl->cuvidCreateDecoder(&decoder->decoder, params) failed -> CUDA_ERROR_INVALID_VALUE: invalid argument
[h264 @ 0x7f2b4c0a2e40] Failed setup for format cuda: hwaccel initialisation returned error.
Impossible to convert between the formats supported by the filter 'graph 0 input from stream 0:0' and the filter 'auto_scale_0'
[fc#0 @ 0x7f2b4c0b1200] Error reinitializing filters!
[fc#0 @ 0x7f2b4c0b1200] Task finished with error code: -22 (Invalid argument)
[fc#0 @ 0x7f2b4c0b1200] Terminating thread with return code -22 (Invalid argument)
[aost#0:1/aac @ 0x7f2b4c0c3940] Could not open encoder before EOF
[aost#0:1/aac @ 0x7f2b4c0c3940] Task finished with error code: -22 (Invalid argument)
[aost#0:1/aac @ 0x7f2b4c0c3940] Terminating thread with return code -22 (Invalid argument)
[vost#0:0/h264_nvenc @ 0x7f2b4c0c1680] Could not open encoder before EOF
[vost#0:0/h264_nvenc @ 0x7f2b4c0c1680] Task finished with error code: -22 (Invalid argument)
[vost#0:0/h264_nvenc @ 0x7f2b4c0c1680] Terminating thread with return code -22 (Invalid argument)
[out#0/mp4 @ 0x7f2b4c0c0100] Nothing was written into output file, because at least one of its streams received no packets.`

// hwInitRefusalLine is the line that names the cause in that capture — what the
// operator's WARN must carry, and what a classifier reading only the tail lost.
//
// It is NVDEC's own refusal rather than ffmpeg's summary of it on the next line,
// because the rule is the FIRST line naming a signature and that is this one.
// (The `Failed setup for format cuda` line is what an install with no device at
// all leads with, which is the example DEPLOY §27 prints.)
const hwInitRefusalLine = "[h264 @ 0x7f2b4c0a2e40] decoder->cvdl->cuvidCreateDecoder(&decoder->decoder, params) failed -> CUDA_ERROR_INVALID_VALUE: invalid argument"

// asStderrScript renders a captured stderr as the sh that prints it, one echo per
// line. It refuses a line sh would not carry through double quotes rather than
// mangling it silently — a fixture that half-prints proves the wrong thing.
func asStderrScript(t *testing.T, stderr string) string {
	t.Helper()
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
		if strings.ContainsAny(line, "\"$`\\") {
			t.Fatalf("this fixture line needs sh quoting this helper does not do: %q", line)
		}
		b.WriteString(`echo "` + line + `" >&2` + "\n")
	}
	return b.String()
}

// hwInitLongTrailerRun is that capture as a refused run of the fake ffmpeg: the
// terminal `-progress` block a refused run really prints first, then the stderr,
// then exit 218 with nothing written.
func hwInitLongTrailerRun(t *testing.T) string {
	t.Helper()
	return `echo 'frame=0'
echo 'total_size=0'
echo 'out_time_us=N/A'
echo 'progress=end'
` + asStderrScript(t, hwInitStderrLongTrailer) + `exit 218`
}

// hwFakeFFmpeg writes a fake ffmpeg that logs its argv and then behaves one way
// for a CUDA argv and another for everything else.
func hwFakeFFmpeg(t *testing.T, log, onCUDA, onOther string) string {
	t.Helper()
	return fakeFFmpeg(t, `printf '%s\n' "$*" >> `+log+`
case " $* " in
  *" -hwaccel cuda "*)
`+onCUDA+`
;;
esac
`+onOther+`
`)
}

// runs returns the argv of every run the fake performed, in order.
func runs(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read the run log: %v", err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// TestEncodeHWFallback: what a refused hardware decode does to one encode.
//
// The rule has two halves and both are tested here, because getting either wrong
// is a clip that fails: a CUDA run refused at INITIALISATION is re-run on the
// spot with the decode on the CPU, and every other failure — including a CUDA one
// that got as far as writing something — is the ending it always was.
func TestEncodeHWFallback(t *testing.T) {
	for _, tc := range []struct {
		name            string
		hwaccel         string
		onCUDA, onOther string
		wantErr         string // "" = the encode must succeed
		wantRuns        int
		wantReport      string // "" = the sink must not be called
		// wantDone is how many times 100 % is published. The refused run's own
		// `progress=end` must NOT be one of them: the worker's percent is
		// monotone, and a bar pinned at the top of the encode band for the whole
		// fallback encode is the one way this re-run could show from outside.
		wantDone int
	}{
		{
			name: "nvdec refuses the source", hwaccel: HWAccelCUDA,
			onCUDA: hwInitStderr, onOther: ffOK,
			wantRuns: 2, wantReport: "cuvidCreateDecoder", wantDone: 1,
		},
		{
			// One fallback, never a cascade: the second run's ending is the
			// clip's, reported exactly as it would have been anyway.
			name: "the fallback fails too", hwaccel: HWAccelCUDA,
			onCUDA: hwInitStderr, onOther: ffBroken,
			wantErr: "the fallback failed too", wantRuns: 2, wantReport: "cuvidCreateDecoder",
		},
		{
			// A CUDA run that works is not gated in any way: its completion is
			// published as it always was, before Encode returns.
			name: "a CUDA run that finishes reports finishing", hwaccel: HWAccelCUDA,
			onCUDA: ffOK, onOther: ffBroken,
			wantRuns: 1, wantDone: 1,
		},
		{
			name: "a mid-stream CUDA failure is the failure it always was", hwaccel: HWAccelCUDA,
			onCUDA: ffMidStream, onOther: ffOK,
			wantErr: "Invalid data found", wantRuns: 1,
		},
		{
			// Whatever wrote those bytes, the run's ending is its own: the
			// failure AND the completion report are what they were before.
			name: "a refusal after output was written is not re-run", hwaccel: HWAccelCUDA,
			onCUDA: ffInitAfterOutput, onOther: ffOK,
			wantErr: "ffmpeg exited 218", wantRuns: 1, wantDone: 1,
		},
		{
			// Nothing to fall back FROM: the CPU argv is already the fallback.
			name: "a CPU run that somehow prints it is not re-run", hwaccel: HWAccelNone,
			onCUDA: ffOK, onOther: hwInitStderr,
			wantErr: "ffmpeg exited 218", wantRuns: 1, wantDone: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := t.TempDir() + "/argv.log"
			ff := hwFakeFFmpeg(t, log, tc.onCUDA, tc.onOther)
			var reports []string
			ctx := WithHWFallbackSink(context.Background(), func(stage, detail string) {
				reports = append(reports, stage+": "+detail)
			})
			o := stdOpts
			o.HWAccel = tc.hwaccel
			done := 0
			err := Encode(ctx, ff, basePlan(AudioNone, nil), "in.mp4", "out.mp4", o,
				func(frac float64, _ bool) {
					if frac >= 1 {
						done++
					}
				})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("the clip failed after a fallback: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("want a failure naming %q, got none", tc.wantErr)
			case tc.wantErr != "":
				if !IsPermanent(err) || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want a permanent one naming %q", err, tc.wantErr)
				}
			}
			if done != tc.wantDone {
				t.Fatalf("100%% was published %d times, want %d", done, tc.wantDone)
			}
			got := runs(t, log)
			if len(got) != tc.wantRuns {
				t.Fatalf("ffmpeg ran %d times, want %d:\n%s", len(got), tc.wantRuns, strings.Join(got, "\n"))
			}
			if tc.wantRuns == 2 {
				if !strings.Contains(got[0], "-hwaccel cuda") {
					t.Errorf("the first run was not the CUDA one: %s", got[0])
				}
				// The re-run is the fallback shape, on the same input and output.
				if strings.Contains(got[1], "-hwaccel") || !strings.Contains(got[1], NVENCH264) {
					t.Errorf("the re-run is not the decode-on-the-CPU argv: %s", got[1])
				}
				if !strings.Contains(got[1], "out.mp4") {
					t.Errorf("the re-run wrote somewhere else: %s", got[1])
				}
			}
			switch {
			case tc.wantReport == "" && len(reports) != 0:
				t.Fatalf("the sink was called for a run that did not fall back: %v", reports)
			case tc.wantReport != "" && len(reports) != 1:
				t.Fatalf("want ONE warning, got %d: %v", len(reports), reports)
			case tc.wantReport != "":
				if !strings.HasPrefix(reports[0], "encode: ") || !strings.Contains(reports[0], tc.wantReport) {
					t.Fatalf("the warning does not name the stage and the ffmpeg line: %q", reports[0])
				}
			}
		})
	}
}

// TestEncodeTiersHWFallback: the two-output pass falls back through the same one
// place, so the Full and the Light copy of a refused source are still produced by
// ONE ffmpeg (erratum 217) — on the CPU decode.
func TestEncodeTiersHWFallback(t *testing.T) {
	log := t.TempDir() + "/argv.log"
	ff := hwFakeFFmpeg(t, log, hwInitStderr, ffOK)
	mainPlan, lightPlan := samplePlans()
	opts := stdOpts
	opts.HWAccel = HWAccelCUDA
	var reports int
	ctx := WithHWFallbackSink(context.Background(), func(string, string) { reports++ })
	err := EncodeTiers(ctx, ff, "in.mp4",
		TierOutput{Plan: mainPlan, Path: "full.mp4", Opts: opts},
		TierOutput{Plan: lightPlan, Path: "light.mp4", Opts: opts}, nil)
	if err != nil {
		t.Fatalf("the two-output pass failed after a fallback: %v", err)
	}
	got := runs(t, log)
	if len(got) != 2 {
		t.Fatalf("ffmpeg ran %d times, want 2:\n%s", len(got), strings.Join(got, "\n"))
	}
	if reports != 1 {
		t.Fatalf("want ONE warning, got %d", reports)
	}
	if strings.Contains(got[1], "-hwaccel") || !strings.Contains(got[1], "full.mp4") || !strings.Contains(got[1], "light.mp4") {
		t.Fatalf("the re-run is not the two-output CPU-decode argv: %s", got[1])
	}
}

// TestHWInitRefusal: which stderr is a hardware decoder refusing a source, and
// which line of it an operator is shown.
func TestHWInitRefusal(t *testing.T) {
	for _, tc := range []struct {
		name, stderr, want string
	}{
		{"nothing at all", "", ""},
		{"an ordinary decode error", "[h264] Error while decoding stream #0:0: Invalid data found", ""},
		{"nvdec itself", "[h264] decoder->cvdl->cuvidCreateDecoder(&decoder->decoder, params) failed -> CUDA_ERROR_INVALID_VALUE",
			"[h264] decoder->cvdl->cuvidCreateDecoder(&decoder->decoder, params) failed -> CUDA_ERROR_INVALID_VALUE"},
		{"ffmpeg's own words", "noise\n[h264] Failed setup for format cuda: hwaccel initialisation returned error.\nmore noise",
			"[h264] Failed setup for format cuda: hwaccel initialisation returned error."},
		// No device at all prints the same words. The fallback then fails at the
		// encoder, which is the deployment's business and not this test's.
		{"no device", "[h264] No device available for decoder: device type cuda needed for codec h264.\n" +
			"[h264] Failed setup for format cuda: hwaccel initialisation returned error.",
			"[h264] Failed setup for format cuda: hwaccel initialisation returned error."},
		// The tail opens mid-line; the line that names the cause is still whole.
		{"a tail that opens mid-line", "...received no packets\r\n[h264] Failed setup for format CUDA: HWACCEL INITIALISATION RETURNED ERROR",
			"[h264] Failed setup for format CUDA: HWACCEL INITIALISATION RETURNED ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hwInitRefusal(tc.stderr); got != tc.want {
				t.Fatalf("hwInitRefusal = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHWInitRefusalReadsTheWholeRing is §20 erratum 289, pinned from both ends.
//
// The bug was not in hwInitRefusal: it was in what hwInitRefusal was HANDED. The
// captured stderr lives in an 8 KiB ring, and two different readings of it are
// wanted — the last kilobyte as the failure REASON, because a reason is a log
// line, and the whole thing for the CLASSIFICATION, because the signature is
// printed at set-up and the refusal's own shutdown prints past a kilobyte after
// it. Handing the classifier the tail is silent, plausible, and fails a clip
// permanently on hardware that would have transcoded it on the CPU.
//
// So the first half of this test asserts the BUG — that the tail really does lose
// the cause on this capture — because a fixture that stopped reproducing it would
// leave the second half proving nothing.
func TestHWInitRefusalReadsTheWholeRing(t *testing.T) {
	// 8 KiB is enough for this, which is the other half of the argument: every
	// encode argv here runs `-loglevel error -nostats -hide_banner`, so nothing
	// below an error is written at all, and the longest refusal measured is this one.
	if n := len(hwInitStderrLongTrailer); n >= stderrRingSize {
		t.Fatalf("the capture is %d bytes and no longer fits the %d-byte ring: the ring must grow", n, stderrRingSize)
	}
	r := newRing(stderrRingSize)
	if _, err := r.Write([]byte(hwInitStderrLongTrailer)); err != nil {
		t.Fatal(err)
	}

	// THE BUG. The failure tail names the consequence and not the cause.
	tail := r.Tail(failureTail)
	if got := hwInitRefusal(tail); got != "" {
		t.Fatalf("this fixture no longer reproduces erratum 289 — the %d-byte tail still names %q, "+
			"so the whole-ring assertion below proves nothing. Lengthen the trailer.", failureTail, got)
	}
	if !strings.Contains(tail, "Impossible to convert") {
		t.Fatalf("the tail should still carry the CONSEQUENCE of the refusal:\n%s", tail)
	}
	// And it opens mid-word, which is how this was noticed at all: the reason on
	// the failed production clip began `ed error.`.
	first, _, _ := strings.Cut(tail, "\n")
	for _, line := range strings.Split(hwInitStderrLongTrailer, "\n") {
		if line == first {
			t.Fatalf("the tail opens on a WHOLE line (%q); erratum 289's opened mid-word", first)
		}
	}

	// THE FIX. The classification reads the whole ring and finds the line.
	got := hwInitRefusal(r.Tail(stderrRingSize))
	if got != hwInitRefusalLine {
		t.Fatalf("hwInitRefusal(the whole ring) = %q, want %q", got, hwInitRefusalLine)
	}
	// It is a line the tail never had — which is the difference between a clip
	// that lands and a clip that fails, stated as an assertion.
	if strings.Contains(tail, got) {
		t.Fatalf("the tail carried that line after all; this fixture proves nothing:\n%s", tail)
	}
	// The reason an operator is shown is unchanged: still the tail, still a
	// kilobyte. Widening the classification must not widen the log line.
	if len(tail) > failureTail {
		t.Fatalf("the failure reason grew to %d bytes; it is a log line and stays %d", len(tail), failureTail)
	}
}

// TestEncodeHWFallbackLongTrailer is the run-level half of erratum 289: the very
// capture above, through a real Encode, on the argv this package really renders.
//
// Before the fix this is the failure seen in production — one run, exit 218, "Processing
// failed" on the card. After it the decode moves to the CPU exactly once and the
// clip lands.
func TestEncodeHWFallbackLongTrailer(t *testing.T) {
	log := t.TempDir() + "/argv.log"
	ff := hwFakeFFmpeg(t, log, hwInitLongTrailerRun(t), ffOK)
	var reports []string
	ctx := WithHWFallbackSink(context.Background(), func(stage, detail string) {
		reports = append(reports, stage+": "+detail)
	})
	o := stdOpts
	o.HWAccel = HWAccelCUDA
	done := 0
	err := Encode(ctx, ff, basePlan(AudioSingle, []int{1}), "in.mp4", "out.mp4", o,
		func(frac float64, _ bool) {
			if frac >= 1 {
				done++
			}
		})
	if err != nil {
		t.Fatalf("the clip failed although the decode could have moved to the CPU: %v", err)
	}
	got := runs(t, log)
	if len(got) != 2 {
		t.Fatalf("ffmpeg ran %d times, want 2 (the refused one and the fallback):\n%s", len(got), strings.Join(got, "\n"))
	}
	if !strings.Contains(got[0], "-hwaccel cuda") {
		t.Errorf("the first run was not the CUDA one: %s", got[0])
	}
	// Exactly hwAccelNVENCOnly: no `-hwaccel` anywhere, the encoder still the
	// card's, the same output file.
	if strings.Contains(got[1], "-hwaccel") || !strings.Contains(got[1], NVENCH264) || !strings.Contains(got[1], "out.mp4") {
		t.Errorf("the re-run is not the decode-on-the-CPU argv: %s", got[1])
	}
	if len(reports) != 1 {
		t.Fatalf("want ONE warning, got %d: %v", len(reports), reports)
	}
	// The operator is told the CAUSE, not the consequence the tail ends on.
	if reports[0] != "encode: "+hwInitRefusalLine {
		t.Fatalf("the warning does not name the line that refused:\n got %q\nwant %q", reports[0], "encode: "+hwInitRefusalLine)
	}
	if done != 1 {
		t.Fatalf("100%% was published %d times, want 1 (the refused run's own end is held)", done)
	}
}
