package transcode

import (
	"context"
	"strings"
)

// The RUN-TIME half of the GPU fast path (DESIGN.md §20 erratum 265).
//
// HWAccelFor decides at PLAN time whether a source may take the card, from the
// filters the plan needs and from what NVDEC's own table says it decodes
// (erratum 247). That decision is necessary and not sufficient: NVDEC also
// refuses individual streams for reasons no probe reports.
//
// The proof is a 2560×1440 60 fps H.264 High/8-bit/yuv420p ShadowPlay capture —
// level 5.1, refs 1, progressive, bt709, nothing any rule here can object to —
// whose decode dies in one second:
//
//	[h264] decoder->cvdl->cuvidCreateDecoder(&decoder->decoder, params) failed
//	       -> CUDA_ERROR_INVALID_VALUE: invalid argument
//	[h264] Failed setup for format cuda: hwaccel initialisation returned error.
//	Impossible to convert between the formats supported by the filter
//	       'graph 0 input from stream 0:0' and the filter 'auto_scale_0'
//	... Nothing was written into output file
//
// exit 218, zero bytes written. The same file decodes on the host's own ffmpeg
// with the same error and decodes fine through `h264_cuvid`, so it is the
// stream and the `nvdec` hwaccel path, not the container and not WSL2.
//
// A clip must never fail because of the operator's hardware, so the run itself
// is the last authority: when ffmpeg exits with that signature before it has
// written anything, the same stage is re-run at once with the decode moved off
// the card. It is ONE attempt from the scheduler's point of view — the internal
// re-run touches no counter and no classification, and only the ending of the
// second run is reported.
//
// The fallback keeps the ENCODER on the card (hwAccelNVENCOnly). What failed is
// the decoder; NVENC is a different engine and works, and erratum 216 measured
// CPU-decode+NVENC at 1.4–3.6× libx264 where the full CUDA path is 9–11×. The
// argv is then exactly the CPU argv with the encoder swapped — one filter graph,
// not two — which is why it is a fallback shape and not a second pipeline.
//
// Two things it deliberately does NOT do:
//
//   - cascade. A fallback that also fails is the clip's ending, reported as it
//     would have been reported anyway. The one case that costs is a box with
//     NVENC configured and no device at all — `-hwaccel cuda` then prints this
//     same signature ("No device available for decoder"), the fallback's encoder
//     cannot reach the card either, and the clip fails. That is a misconfigured
//     deployment, not a source, it is the same failure the run had before this
//     existed, and DEPLOY §27 names the line an operator sees.
//   - remember. The refusal is a fact about one stream, and the next clip asks
//     the card again; nothing is learned, cached or disabled process-wide.

// hwInitSignatures are the fragments an ffmpeg stderr carries when the hardware
// decoder refused the source while SETTING UP — `cuvidCreateDecoder` is NVDEC
// itself saying no, and the other two are ffmpeg's own words for the same event
// (they are printed for a missing device as well as for a refused stream). They
// are matched case-insensitively and none of them can be printed by a decode
// that has already started, which is what makes them a safe retry test.
//
// The British spelling is ffmpeg's, not ours.
var hwInitSignatures = []string{
	"hwaccel initialisation returned error",
	"cuvidcreatedecoder",
	"failed setup for format cuda",
}

// hwInitRefusal returns the ffmpeg line that refused the source, or "" when this
// stderr is not a hardware-decoder refusal.
//
// It must be given the WHOLE captured stderr and not the failure tail (§20
// erratum 289): those signatures are printed while the decoder is being SET UP,
// which is the earliest thing in a run, and ffmpeg's shutdown after such a
// refusal prints more than a kilobyte after them — every thread it terminates,
// every encoder it could not open, and the output it wrote nothing into. A
// classifier handed the last kilobyte therefore sees the consequences and not
// the cause, says "not a refusal", and the clip fails permanently on hardware
// that would have transcoded it on the CPU. That is exactly what happened to the
// first `/clip` that ever ran through the official bot.
//
// The line returned is the FIRST one naming a signature rather than the first
// line of what it was given: even the whole ring can open mid-line once stderr
// is longer than it, and the line that names the cause is the one an operator
// wants in the log anyway.
func hwInitRefusal(stderr string) string {
	if stderr == "" {
		return ""
	}
	for _, line := range strings.Split(stderr, "\n") {
		l := strings.ToLower(line)
		for _, sig := range hwInitSignatures {
			if strings.Contains(l, sig) {
				return strings.TrimSpace(line)
			}
		}
	}
	return ""
}

// runHWFallback runs one encode argv and, when a CUDA run is refused by the
// hardware decoder before it writes anything, re-runs it at once with the decode
// on the CPU and the encoder still on the card.
//
// build renders the argv for a given accelerator; it is called a second time
// rather than the argv patched, so the fallback run is exactly what this package
// would have rendered had the plan never asked for the card in the first place.
// Every ffmpeg argv in this package that can carry `-hwaccel cuda` is built by
// Encode or EncodeTiers, and both come through here, so the fallback covers
// every kind of encode (transcode, full, light, replace, trim) in one place.
//
// The second run gets its own full stall and hard-timeout budget. The refused
// run lasts about a second — it dies at graph configuration — so there is
// nothing worth subtracting, and an operator's timeout is a statement about how
// long an encode may take, not about how long a process may live.
func runHWFallback(ctx context.Context, ffmpeg, accel string, build func(accel string) []string,
	p Plan, in instrument, onProgress func(frac float64, indeterminate bool)) error {
	if !cudaGraph(accel) {
		_, err := runInstrumentedRefusal(ctx, ffmpeg, build(accel), p, in, onProgress)
		return err
	}
	gate := newHWProgressGate(onProgress)
	refusal, err := runInstrumentedRefusal(ctx, ffmpeg, build(accel), p, in, gate.report)
	if err == nil || refusal == "" {
		gate.flush()
		return err
	}
	ReportHWFallback(ctx, in.name(), refusal)
	_, err = runInstrumentedRefusal(ctx, ffmpeg, build(hwAccelNVENCOnly), p, in, onProgress)
	return err
}

// hwProgressGate forwards a run's progress except a report of COMPLETION, which
// it holds until that run's exit status is known.
//
// ffmpeg prints its final `-progress` block whatever happened, so a refused CUDA
// run ends with `progress=end` (and `total_size=0`, `out_time=N/A`) exactly like
// a finished one, which this package reads as 100 %. The worker's percent is
// MONOTONE — that is what keeps a bar from going backwards mid-stage — so
// publishing that would pin it at the top of the encode band for the whole
// fallback encode, which is the one way the internal re-run could be visible from
// outside. Holding the report costs nothing: it is released the moment the run
// turns out to have finished for real, before the call that reported it returns.
//
// report runs on ffmpeg's stdout copier and flush after Wait(), which os/exec
// does not return until that copier has stopped — the same ordering
// progressTracker.wrote() relies on.
type hwProgressGate struct {
	fn            func(frac float64, indeterminate bool)
	held          bool
	indeterminate bool
}

func newHWProgressGate(fn func(frac float64, indeterminate bool)) *hwProgressGate {
	if fn == nil {
		fn = func(float64, bool) {}
	}
	return &hwProgressGate{fn: fn}
}

func (g *hwProgressGate) report(frac float64, indeterminate bool) {
	if frac >= 1 {
		g.held, g.indeterminate = true, indeterminate
		return
	}
	g.fn(frac, indeterminate)
}

func (g *hwProgressGate) flush() {
	if g.held {
		g.held = false
		g.fn(1, g.indeterminate)
	}
}

// hwFallbackKey is the context key of the sink below.
type hwFallbackKey struct{}

// WithHWFallbackSink returns a context whose encodes report a hardware-decoder
// fallback to fn: stage is the run's own label ("encode"), detail the ffmpeg line
// that refused. It is the shape internal/httpx uses for the same problem — the
// code that KNOWS a fact is not the code that can name the clip it happened to —
// and it is on the context because the worker already derives one per job, so a
// sink can never outlive its run or be shared by two.
//
// fn is called at most once per run, from the goroutine running it, after the
// refused run has been reaped and before the fallback run starts. Nil installs
// nothing; a context without one is how every test and every non-CUDA install
// runs.
func WithHWFallbackSink(ctx context.Context, fn func(stage, detail string)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, hwFallbackKey{}, fn)
}

// ReportHWFallback hands one fallback to the context's sink, if there is one.
// runHWFallback above is its only caller in this package; it is exported as the
// other half of the pair so the WORKER's own wiring — that every run's context
// really carries a sink — can be tested without a card (internal/worker).
func ReportHWFallback(ctx context.Context, stage, detail string) {
	if fn, ok := ctx.Value(hwFallbackKey{}).(func(stage, detail string)); ok {
		fn(stage, detail)
	}
}
