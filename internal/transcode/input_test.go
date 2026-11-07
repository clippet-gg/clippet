package transcode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// countOf returns how many times flag appears in args.
func countOf(args []string, flag string) int {
	n := 0
	for _, a := range args {
		if a == flag {
			n++
		}
	}
	return n
}

// assertWhitelistedInputs checks the ffmpeg pairing: every `-i` in the argv
// is preceded, immediately, by `-protocol_whitelist file`. Position is the
// whole point — an input option governs the input that FOLLOWS it, so a
// flag parked at the front of the argv or after the last `-i` protects
// nothing.
func assertWhitelistedInputs(t *testing.T, name string, args []string) {
	t.Helper()
	inputs := countOf(args, "-i")
	if inputs == 0 {
		t.Fatalf("%s: argv has no input at all: %s", name, strings.Join(args, " "))
	}
	if got := countOf(args, "-protocol_whitelist"); got != inputs {
		t.Errorf("%s: %d inputs but %d whitelists: %s", name, inputs, got, strings.Join(args, " "))
	}
	for i, a := range args {
		if a != "-i" {
			continue
		}
		if i < 2 || args[i-2] != "-protocol_whitelist" || args[i-1] != "file" {
			t.Errorf("%s: input %d is not preceded by `-protocol_whitelist file`: %s",
				name, i, strings.Join(args, " "))
		}
	}
}

// assertWhitelistedProbeInput checks the ffprobe pairing: the input path is
// positional, and the whitelist is the option immediately in front of it.
func assertWhitelistedProbeInput(t *testing.T, name string, args []string, path string) {
	t.Helper()
	if n := len(args); n < 3 || args[n-1] != path || args[n-2] != "file" || args[n-3] != "-protocol_whitelist" {
		t.Errorf("%s: argv must end in `-protocol_whitelist file %s`: %s", name, path, strings.Join(args, " "))
	}
}

// TestEveryFFmpegInputIsProtocolWhitelisted covers all six ffmpeg argv
// builders. Without the flag a demuxer opens whatever its input names —
// an HLS playlist's http:// segment, an ffconcat script's file:// sibling —
// and ffmpeg's built-in default (`file,crypto,data`) is the vendor's to
// change on the next bump, so the guarantee has to be ours (§20 erratum 91).
func TestEveryFFmpegInputIsProtocolWhitelisted(t *testing.T) {
	plan := basePlan(AudioSingle, []int{1})
	gif := PlanGIF(20_000, 1280, 3)
	probe := h264mp4()
	trim := DecideTrim(probe, planFor(probe), sixSecondKeys, 1000, 3000)
	trimOpts := stdOpts
	trimOpts.Trim = &Window{Start: 1, Duration: 2}
	for _, c := range []struct {
		name string
		args []string
	}{
		{"EncodeArgs", plan.EncodeArgs("in.mp4", "out.mp4", stdOpts)},
		{"EncodeArgs (trim window)", plan.EncodeArgs("in.mp4", "out.mp4", trimOpts)},
		{"PosterArgs", PosterArgs("in.mp4", "poster.jpg", 0.9, PosterBox{})},
		{"PosterArgs (boxed)", PosterArgs("in.mp4", "poster.jpg", 0.9, PosterBox{W: 1280, H: 720})},
		{"PosterThumbArgs", PosterThumbArgs("in.mp4", "poster.jpg", 0.9, 20, PosterBox{})},
		{"PosterThumbArgs (boxed)", PosterThumbArgs("in.mp4", "poster.jpg", 0.9, 20, PosterBox{W: 1280, H: 720})},
		{"GIFPlan.PaletteArgs", gif.PaletteArgs("in.mp4", "pal.png", 0)},
		{"GIFPlan.EncodeArgs", gif.EncodeArgs("in.mp4", "pal.png", "out.gif", 0)},
		{"TrimPlan.CopyArgs", trim.CopyArgs(probe, "in.mp4", "out.mp4")},
	} {
		assertWhitelistedInputs(t, c.name, c.args)
	}
	// The GIF's second pass reads the palette as a second input, and an
	// input option covers exactly one input.
	if got := countOf(gif.EncodeArgs("in.mp4", "pal.png", "out.gif", 0), "-i"); got != 2 {
		t.Fatalf("the GIF encode must have two inputs, got %d", got)
	}
}

// TestEveryFFprobeInputIsProtocolWhitelisted covers the three ffprobe
// invocations. Two of them build their argv inline, so they are exercised
// through a stand-in binary that records what it was called with.
func TestEveryFFprobeInputIsProtocolWhitelisted(t *testing.T) {
	assertWhitelistedProbeInput(t, "KeyframeArgs", KeyframeArgs("/s/in.mp4", 2), "/s/in.mp4")

	log := filepath.Join(t.TempDir(), "argv")
	ffprobe := fakeFFmpeg(t, `for a in "$@"; do echo "$a" >> `+log+`; done
cat <<'JSON'
{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":640,"height":360,
"avg_frame_rate":"30/1","duration":"2.0","nb_read_frames":"30"}],
"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"2.0"}}
JSON
`)
	recorded := func(t *testing.T) []string {
		t.Helper()
		raw, err := os.ReadFile(log)
		if err != nil {
			t.Fatalf("the stand-in ffprobe recorded nothing: %v", err)
		}
		if err := os.Remove(log); err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	}

	if _, err := Probe(ffprobe)(context.Background(), "/s/source.mp4"); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	assertWhitelistedProbeInput(t, "runFFprobe", recorded(t), "/s/source.mp4")

	if _, err := ProbeGIF(context.Background(), ffprobe, "/s/preview.gif"); err != nil {
		t.Fatalf("ProbeGIF: %v", err)
	}
	assertWhitelistedProbeInput(t, "ProbeGIF", recorded(t), "/s/preview.gif")
}
