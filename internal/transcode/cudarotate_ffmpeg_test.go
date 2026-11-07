//go:build ffmpeg

package transcode

// §20 erratum 352 on real ffmpeg. The CUDA graph of a matrix-turned source
// turns the frames itself; this pins that its turn is the SAME turn ffmpeg's own
// autorotate makes, by running the explicit graph with the card's two filters
// swapped for their CPU equivalents (`scale_cuda` → `scale`, no hwdownload, no
// `-hwaccel`, libx264 for NVENC) and comparing its pixels with the software
// path's. What only a GPU box can prove — `scale_cuda` itself and the NVDEC
// frames — is the E2E's `113-rotated` bundle on the PC's gate.

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cudaOnCPU rewrites a CUDA encode argv into the same graph on the CPU: every
// option and filter the card adds is replaced by its CPU twin, and nothing else
// moves — in particular `-display_rotation:v 0` and the turn stay as rendered.
func cudaOnCPU(t *testing.T, argv []string) []string {
	t.Helper()
	scale := regexp.MustCompile(`scale_cuda=(\d+):(\d+):interp_algo=lanczos:format=yuv420p,hwdownload,format=yuv420p`)
	var out []string
	sawScale := false
	for i := 0; i < len(argv); i++ {
		switch a := argv[i]; {
		case a == "-hwaccel" || a == "-hwaccel_output_format" || a == "-bsf:v":
			i++ // drop the option and its value
		case a == "-c:v" && i+1 < len(argv) && argv[i+1] == NVENCH264:
			out = append(out, "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p")
			// skip the NVENC encoder options through `-b:v 0`
			for i++; i < len(argv) && argv[i] != "-b:v"; i++ {
			}
			i++
		case strings.Contains(a, "scale_cuda="):
			sawScale = sawScale || scale.MatchString(a)
			out = append(out, scale.ReplaceAllString(a, "scale=$1:$2:flags=lanczos,format=yuv420p"))
		default:
			out = append(out, a)
		}
	}
	if !sawScale {
		t.Fatalf("not a turned CUDA argv (no scale_cuda…hwdownload): %q", argv)
	}
	return out
}

// sideData is the file's video side data as ffprobe prints it; a Display
// Matrix in an output means players would turn the picture a second time.
func sideData(t *testing.T, ffprobe, file string) string {
	t.Helper()
	return strings.TrimSpace(run(t, ffprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream_side_data=side_data_type,rotation", "-of", "compact", file))
}

func TestMatrixTurnMatchesAutorotate(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	box := PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60}
	for _, c := range []struct {
		name       string
		rotation   string // -display_rotation (counter-clockwise, ffprobe's sign)
		audioFirst bool
		turn, w, h int
	}{
		// The phone clip's shape: audio stream 0, video stream 1, rotation 90.
		{name: "audiofirst-rot90", rotation: "90", audioFirst: true, turn: 270, w: 720, h: 1280},
		{name: "rot-90", rotation: "-90", turn: 90, w: 720, h: 1280},
		{name: "rot180", rotation: "180", turn: 180, w: 1280, h: 720},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx := context.Background()
			p := plain(t, ffmpeg, dir, "410")
			maps := []string{"-map", "0:v", "-map", "0:a"}
			if c.audioFirst {
				maps = []string{"-map", "0:a", "-map", "0:v"}
			}
			in := gen(t, ffmpeg, dir, "turned.mp4", join([]string{"-display_rotation", c.rotation, "-i", p}, maps, []string{"-c", "copy"})...)
			if sd := sideData(t, ffprobe, in); !strings.Contains(sd, "Display Matrix") {
				t.Fatalf("the fixture carries no display matrix: %q", sd)
			}
			pr, err := Probe(ffprobe)(ctx, in)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			plan := MakePlan(pr, box)
			if plan.MatrixTurn != c.turn || plan.PreRotate != 0 || plan.MatrixMirrored || plan.W != c.w || plan.H != c.h {
				t.Fatalf("plan turn %d pre %d mirrored %v %dx%d; want turn %d %dx%d", plan.MatrixTurn, plan.PreRotate,
					plan.MatrixMirrored, plan.W, plan.H, c.turn, c.w, c.h)
			}
			if c.audioFirst && (plan.VideoIndex != 1 || plan.AudioIndexes[0] != 0) {
				t.Fatalf("audio-first layout: video %d audio %v", plan.VideoIndex, plan.AudioIndexes)
			}

			// The software path: autorotate turns, and the output carries no matrix.
			sw := filepath.Join(dir, "sw.mp4")
			if err := Encode(ctx, ffmpeg, plan, in, sw, ffmpegOpts, nil); err != nil {
				t.Fatalf("Encode (cpu): %v", err)
			}
			// The explicit path: the CUDA argv this package renders, on the CPU.
			ex := filepath.Join(dir, "explicit.mp4")
			cuda := ffmpegOpts
			cuda.HWAccel, cuda.Codec = HWAccelCUDA, FullCodecH264
			argv := plan.EncodeArgs(in, ex, cuda)
			if !strings.Contains(strings.Join(argv, " "), "-display_rotation:v 0") {
				t.Fatalf("the CUDA argv does not reset the matrix: %q", argv)
			}
			run(t, ffmpeg, cudaOnCPU(t, argv)...)

			for name, f := range map[string]string{"software": sw, "explicit": ex} {
				info, err := ProbeOutput(ctx, ffprobe, f)
				if err != nil {
					t.Fatalf("ProbeOutput(%s): %v", name, err)
				}
				if info.Width != c.w || info.Height != c.h {
					t.Errorf("%s output %dx%d, want %dx%d", name, info.Width, info.Height, c.w, c.h)
				}
				if sd := sideData(t, ffprobe, f); strings.Contains(sd, "Display Matrix") {
					t.Errorf("%s output still carries side data (players would turn it again): %q", name, sd)
				}
			}
			// Same picture: the explicit turn is autorotate's turn. The control is
			// the explicit output against the software one turned by a half — which
			// is exactly what the OTHER quarter turn would have produced.
			if s := ssim(t, ffmpeg, ex, sw, "null"); s < ssimFaithful {
				t.Fatalf("SSIM(explicit, software) = %.3f, want ≥ %.2f: the explicit turn is not autorotate's", s, ssimFaithful)
			} else {
				t.Logf("SSIM(explicit, software) = %.3f", s)
			}
			if s := ssim(t, ffmpeg, ex, sw, "hflip,vflip"); s > ssimWrong {
				t.Fatalf("SSIM(explicit, software turned 180) = %.3f, want ≤ %.2f: the comparison cannot tell turns apart", s, ssimWrong)
			}
		})
	}
}
