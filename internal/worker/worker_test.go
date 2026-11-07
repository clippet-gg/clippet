package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/store"
	"clippet/internal/transcode"
	"clippet/web/static"
)

// TestNoopNeverBlocks runs every method under a deadline; a hang fails.
func TestNoopNeverBlocks(t *testing.T) {
	var w Worker = NewNoop()
	steps := map[string]func(){
		"Start":    func() { w.Start(context.Background()) },
		"Wake":     func() { w.Wake() },
		"Cancel":   func() { w.Cancel(context.Background(), "aZ3kQ9xB2mLp") },
		"Forget":   func() { w.Forget("aZ3kQ9xB2mLp") },
		"Progress": func() { _, _ = w.Progress("aZ3kQ9xB2mLp") },
		"Stop": func() {
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // even a dead ctx must not block or error
			if err := w.Stop(ctx); err != nil {
				t.Errorf("Stop = %v", err)
			}
		},
	}
	for name, fn := range steps {
		done := make(chan struct{})
		go func() { fn(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s blocked", name)
		}
	}
	if p, ok := w.Progress("x"); ok || p != (Progress{}) {
		t.Fatalf("Progress = %+v, %v", p, ok)
	}
	// Start with a cancelled ctx must also return immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Start(ctx)
	// The real New before Start: Stop returns nil, twice.
	real := New(Deps{})
	if err := real.Stop(context.Background()); err != nil {
		t.Fatalf("New().Stop = %v", err)
	}
	if err := real.Stop(context.Background()); err != nil {
		t.Fatalf("New().Stop again = %v", err)
	}
	real.Cancel(context.Background(), "nothing-running") // returns at once
}

// ---- harness ----

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *logSink) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Reset()
}

// fakeRunner records calls and lets a test override any stage; defaults
// produce a plausible happy path with files on disk for the Fake to upload.
type fakeRunner struct {
	mu    sync.Mutex
	calls []string

	probeFn     func(ctx context.Context, path string) (*transcode.ProbeResult, error)
	encodeFn    func(ctx context.Context, p transcode.Plan, in, out string, onProgress func(float64, bool)) error
	probeOutFn  func(ctx context.Context, path string) (*transcode.OutputInfo, error)
	verifyFn    func(path string) error
	posterFn    func(ctx context.Context, in, out string, durUS int64, has bool) (transcode.PosterResult, error)
	posterAtFn  func(ctx context.Context, in string, durUS int64, has bool, videoEndSec float64) float64
	posterPinFn func(ctx context.Context, in, out string, seconds float64) (transcode.PosterResult, error)
	keyframesFn func(ctx context.Context, path string, videoIndex int) ([]float64, error)
	copyFn      func(ctx context.Context, t transcode.TrimPlan, p *transcode.ProbeResult, in, out string) error
	gifFn       func(ctx context.Context, p transcode.GIFPlan, in, palette, out string) error
	probeGIFFn  func(ctx context.Context, path string) (*transcode.GIFInfo, error)
	socialFn    func(ctx context.Context, p transcode.Plan, in, out string) error
	filmstripFn func(ctx context.Context, p transcode.FilmstripPlan, in, out string) (transcode.FilmstripResult, error)
	remuxFn     func(ctx context.Context, p *transcode.ProbeResult, in, out string) error
	encodeBytes []byte
	gifBytes    []byte
	socialBytes []byte
	// filmstripBytes / filmstripPlans are the §20 erratum 220 pass's fixtures:
	// what the fake sheet contains, and the plan of every call, so a test can
	// assert the tile geometry a rendition was sampled with.
	filmstripBytes []byte
	filmstripPlans []transcode.FilmstripPlan
	// gifPlans records the plan of every GIF call (§17 #6).
	gifPlans []transcode.GIFPlan
	// socialPlans records the plan of every EncodeSocial call (§17 #3): the
	// capped frame rate is the whole feature, so a test asserts on it.
	socialPlans []transcode.Plan
	// windows records the trim window of every EncodeTrim/CopyTrim call
	// (§17 #5), so a test can assert what ffmpeg would have been asked for.
	windows []transcode.Window
	// budgets records the maxOutputBytes of every Encode call (§20
	// erratum 46).
	budgets []int64
	// posterSeeks records the seek of every PosterAt call (ROADMAP #5), so a
	// test can assert WHICH frame the card image was taken from.
	posterSeeks []float64
	// posterLabelMS records the duration every PosterAt call was told to burn
	// into the frame's corner (§20 erratum 287), so a test can assert that a
	// chosen frame carries the CLIP's length and not the moment it was taken at.
	posterLabelMS []int64
	// posterCuts records the file and the frame box of every Poster call (§20
	// erratum 219), so a test can assert the card image was NOT cut from the
	// tagged Light copy and was still written card-sized.
	posterCuts []posterCut
	// fullPlans records the plan of every EncodeFull call (§20 errata 200-206).
	// EncodeFull DELEGATES to Encode, so `Calls()` still says "Encode …" and
	// every test written before the tiers keeps reading what it read; this slice
	// is what a tier test asserts the uncapped dimensions on.
	fullPlans []transcode.Plan
	// lightTags records the tag handed to every producer of the LIGHT copy (§20
	// erratum 214), so a test can assert the card copy carries one and the Full
	// never does.
	lightTags []transcode.LightTag
	// tierPairs records the (full, light) plans of every EncodeTiers call (§20
	// erratum 217): that ONE call writes both files, which is the feature.
	tierPairs [][2]transcode.Plan
	// tiersFn replaces the two-output pass; nil writes both payloads.
	tiersFn func(ctx context.Context, full, light transcode.Plan, in, fullOut, lightOut string) error
	// tagText, when set, is what LightTagFor expands and writes (a test's stand-in
	// for LIGHT_TAG_TEXT plus a font). Empty means no tag, which is what a build
	// with no font does. tagLines records every line it produced, because the
	// file itself lives in scratch a successful run then removes.
	tagText  string
	tagLines []string
	// The audio four (§20 erratum 328): the peak a float source measures, the
	// lossless proof's answer, and the gain each AudioFull was handed.
	peakFn    func(ctx context.Context, in string) (float64, error)
	peaksFn   func(ctx context.Context, in string, n int) ([]uint8, error)
	samePCMFn func(ctx context.Context, a, b string) error
	// audioFullBytes, when set, is what the fake audio Full writes.
	audioFullBytes []byte
	audioGains     []float64
}

const renditionPayload = "rendition-bytes-of-this-attempt"

// gifPayload is what the fake GIF pass writes; the bytes are never parsed
// (ProbeGIF is faked too), only stored and compared.
const gifPayload = "GIF89a-preview-bytes"

// socialPayload is what the fake social encode writes. It differs from the
// rendition's bytes on purpose: the whole point of the feature is that
// /social.mp4 stops being the rendition, and the fake bucket is where a test
// can see that (§17 #3).
const socialPayload = "social-rendition-bytes"

// chosenPosterPayload is what the fake chosen-frame pass writes; like the
// gif's, the bytes are never decoded, only stored and compared.
const chosenPosterPayload = "chosen-card-image-bytes"

// filmstripPayload is what the fake filmstrip pass writes (§20 erratum 220).
// The bytes are never decoded — the fake runner measures the sheet itself —
// only stored and compared.
const filmstripPayload = "filmstrip-sheet-bytes"

func (f *fakeRunner) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeRunner) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeRunner) Budgets() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.budgets...)
}

func (f *fakeRunner) Probe(ctx context.Context, path string) (*transcode.ProbeResult, error) {
	f.record("Probe " + filepath.Base(path))
	if f.probeFn != nil {
		return f.probeFn(ctx, path)
	}
	return defaultProbeResult(), nil
}

// defaultProbeResult is the plausible 720p30 source every stage-level test
// starts from (a probeFn that only wants to observe the call returns it).
func defaultProbeResult() *transcode.ProbeResult {
	return &transcode.ProbeResult{VideoIndex: 0, AudioIndexes: []int{1}, CodedW: 1280, CodedH: 720, SARNum: 1, SARDen: 1,
		DisplayW: 1280, DisplayH: 720, FPS: transcode.Rational{Num: 30, Den: 1}, HasDuration: true, DurationUS: 6_000_000,
		VideoCodec: "h264", Raw: json.RawMessage(`{"streams":[{"index":0}]}`),
		// Both streams span the whole six seconds, which is what an ordinary
		// file looks like: the trim's coverage check (§20 erratum 78) then
		// verifies every window these tests ask for instead of assuming it.
		Video: transcode.StreamExtent{EndSec: 6, Known: true},
		Audio: transcode.StreamExtent{EndSec: 6, Known: true},
	}
}

func (f *fakeRunner) Encode(ctx context.Context, p transcode.Plan, in, out string, maxOutputBytes int64, onProgress func(float64, bool)) error {
	f.record("Encode " + filepath.Base(in))
	f.mu.Lock()
	f.budgets = append(f.budgets, maxOutputBytes)
	f.mu.Unlock()
	if onProgress == nil {
		// transcode.Encode tolerates a nil callback (a caller with no progress
		// registry to write to — the `full` job, §20 erratum 206), so the fake
		// standing in for it has to as well.
		onProgress = func(float64, bool) {}
	}
	if f.encodeFn != nil {
		return f.encodeFn(ctx, p, in, out, onProgress)
	}
	payload := f.encodeBytes
	if payload == nil {
		payload = []byte(renditionPayload)
	}
	if err := os.WriteFile(out, payload, 0o644); err != nil {
		return err
	}
	onProgress(0.5, false)
	onProgress(1, false)
	return nil
}

// EncodeSocial is the §17 #3 half of the Runner. The default writes a
// payload of its own — so a test can tell the card file from the rendition in
// the fake bucket — and records the PLAN it was handed, because the capped
// frame rate is the feature.
func (f *fakeRunner) EncodeSocial(ctx context.Context, p transcode.Plan, in, out string, tag transcode.LightTag,
	maxOutputBytes int64, onProgress func(float64, bool)) error {
	f.record("EncodeSocial " + filepath.Base(in))
	f.mu.Lock()
	f.socialPlans = append(f.socialPlans, p)
	f.budgets = append(f.budgets, maxOutputBytes)
	f.lightTags = append(f.lightTags, tag)
	f.mu.Unlock()
	if f.socialFn != nil {
		return f.socialFn(ctx, p, in, out)
	}
	payload := f.socialBytes
	if payload == nil {
		payload = []byte(socialPayload)
	}
	return os.WriteFile(out, payload, 0o644)
}

// EncodeTiers is the two-output pass (§20 erratum 217): ONE call, both files.
// The default writes the rendition payload to the Full and the social payload to
// the Light copy, so every existing assertion about which bytes are in which
// object keeps reading what it read.
func (f *fakeRunner) EncodeTiers(ctx context.Context, full, light transcode.Plan, in, fullOut, lightOut string,
	tag transcode.LightTag, maxOutputBytes int64, onProgress func(float64, bool)) error {
	f.record("EncodeTiers " + filepath.Base(in))
	f.mu.Lock()
	f.tierPairs = append(f.tierPairs, [2]transcode.Plan{full, light})
	f.fullPlans = append(f.fullPlans, full)
	f.socialPlans = append(f.socialPlans, light)
	f.budgets = append(f.budgets, maxOutputBytes)
	f.lightTags = append(f.lightTags, tag)
	fn := f.tiersFn
	f.mu.Unlock()
	if onProgress != nil {
		onProgress(0.5, false)
		onProgress(1, false)
	}
	if fn != nil {
		return fn(ctx, full, light, in, fullOut, lightOut)
	}
	main := f.encodeBytes
	if main == nil {
		main = []byte(renditionPayload)
	}
	if err := os.WriteFile(fullOut, main, 0o644); err != nil {
		return err
	}
	social := f.socialBytes
	if social == nil {
		social = []byte(socialPayload)
	}
	return os.WriteFile(lightOut, social, 0o644)
}

// LightTagFor is the tag resolver (§20 erratum 214). With no tagText it returns
// the zero LightTag, which is a build with no font — the default in tests.
func (f *fakeRunner) LightTagFor(dir, name, lightQuality, bestQuality string) (transcode.LightTag, error) {
	if f.tagText == "" {
		return transcode.LightTag{}, nil
	}
	line := transcode.ExpandLightTag(f.tagText, lightQuality, bestQuality)
	f.mu.Lock()
	f.tagLines = append(f.tagLines, line)
	f.mu.Unlock()
	path, err := transcode.WriteLightTagFile(dir, name, line)
	if err != nil || path == "" {
		return transcode.LightTag{}, err
	}
	return transcode.LightTag{FontFile: "/fake/font.ttf", TextFile: path}, nil
}

// The audio four (§20 erratum 328). The defaults write payloads of their own
// and record the plan and the gain, so a test reads which branch ran; the
// *Fn fields override.
func (f *fakeRunner) AudioFull(ctx context.Context, p *transcode.ProbeResult, plan transcode.AudioPlan, gainDB float64, in, out string, maxOutputBytes int64) error {
	f.record("AudioFull " + plan.Full + " " + filepath.Base(in))
	f.mu.Lock()
	f.audioGains = append(f.audioGains, gainDB)
	f.budgets = append(f.budgets, maxOutputBytes)
	payload := f.audioFullBytes
	f.mu.Unlock()
	if payload == nil {
		payload = []byte(audioFullPayload)
	}
	return os.WriteFile(out, payload, 0o644)
}

func (f *fakeRunner) AudioLight(ctx context.Context, p *transcode.ProbeResult, stencil, in, out string, bitrateK int, copyAudio bool, maxOutputBytes int64) error {
	how := fmt.Sprintf("%dk", bitrateK)
	if copyAudio {
		how = "copy"
	}
	f.record(fmt.Sprintf("AudioLight %s %s", filepath.Base(in), how))
	if _, err := os.Stat(stencil); err != nil {
		return fmt.Errorf("fake AudioLight: the stencil is not on disk: %w", err)
	}
	return os.WriteFile(out, []byte(socialPayload), 0o644)
}

// WavePeaks answers a fixed ramp (or peaksFn's), so a test can read the bars
// the row and the poster were given.
func (f *fakeRunner) WavePeaks(ctx context.Context, in string, p *transcode.ProbeResult, n int) ([]uint8, error) {
	f.record("WavePeaks " + filepath.Base(in))
	if f.peaksFn != nil {
		return f.peaksFn(ctx, in, n)
	}
	h := make([]uint8, n)
	for i := range h {
		h[i] = uint8(i * 255 / max(n-1, 1))
	}
	return h, nil
}

// AudioPoster records the duration it was told to burn and the bars it was
// given, and writes a payload of its own.
func (f *fakeRunner) AudioPoster(ctx context.Context, heights []uint8, mark []byte, out string, durMS int64) (transcode.PosterResult, error) {
	which := "an unknown mark"
	for _, name := range []string{static.MarkIcon192, static.MarkIcon192Badged} {
		if b, err := static.FS.ReadFile(name); err == nil && bytes.Equal(b, mark) {
			which = name
		}
	}
	f.record(fmt.Sprintf("AudioPoster %d bars %d ms mark=%v %s", len(heights), durMS, len(mark) > 0, which))
	if err := os.WriteFile(out, []byte("png-poster"), 0o644); err != nil {
		return transcode.PosterResult{}, err
	}
	return transcode.PosterResult{Width: transcode.WaveFrameW, Height: transcode.WaveFrameH}, nil
}

func (f *fakeRunner) AudioPeak(ctx context.Context, in string) (float64, error) {
	f.record("AudioPeak " + filepath.Base(in))
	if f.peakFn != nil {
		return f.peakFn(ctx, in)
	}
	return -6, nil
}

func (f *fakeRunner) SamePCM(ctx context.Context, a, b string) error {
	f.record("SamePCM " + filepath.Base(a) + " " + filepath.Base(b))
	if f.samePCMFn != nil {
		return f.samePCMFn(ctx, a, b)
	}
	return nil
}

// AudioGains returns the gain every AudioFull call was handed.
func (f *fakeRunner) AudioGains() []float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]float64(nil), f.audioGains...)
}

// audioFullPayload is what the fake audio Full writes.
const audioFullPayload = "audio-full-bytes"

// EncodeFull is the FULL tier's encode (§20 erratum 204). It records the plan —
// the uncapped dimensions are the feature — and then DELEGATES to Encode, so
// every assertion written against `Calls()` before the tiers still matches.
func (f *fakeRunner) EncodeFull(ctx context.Context, p transcode.Plan, in, out string, maxOutputBytes int64, onProgress func(float64, bool)) error {
	f.mu.Lock()
	f.fullPlans = append(f.fullPlans, p)
	f.mu.Unlock()
	return f.Encode(ctx, p, in, out, maxOutputBytes, onProgress)
}

// FullPlans returns the plans handed to the full encode so far.
func (f *fakeRunner) FullPlans() []transcode.Plan {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]transcode.Plan(nil), f.fullPlans...)
}

// Remux is the `-c copy` container rewrite of a playable original (§20 erratum
// 200). It writes the rendition payload, because a remux IS the rendition
// object: what it contains is the source's own coded bytes.
func (f *fakeRunner) Remux(ctx context.Context, p *transcode.ProbeResult, in, out string) error {
	f.record("Remux " + filepath.Base(in))
	if f.remuxFn != nil {
		return f.remuxFn(ctx, p, in, out)
	}
	payload := f.encodeBytes
	if payload == nil {
		payload = []byte(renditionPayload)
	}
	return os.WriteFile(out, payload, 0o644)
}

// SocialPlans returns the plans handed to the social encode so far.
func (f *fakeRunner) SocialPlans() []transcode.Plan {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]transcode.Plan(nil), f.socialPlans...)
}

// TierPairs is the (full, light) plan pair of every EncodeTiers call (§20
// erratum 217), and LightTags the tag every producer of the card copy was
// handed (erratum 214).
func (f *fakeRunner) TierPairs() [][2]transcode.Plan {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]transcode.Plan(nil), f.tierPairs...)
}

func (f *fakeRunner) LightTags() []transcode.LightTag {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]transcode.LightTag(nil), f.lightTags...)
}

// TagLines is every line LightTagFor expanded. The file it wrote lives in the
// run's scratch directory, which a successful run removes, so the LINE is what a
// test can assert on afterwards.
func (f *fakeRunner) TagLines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tagLines...)
}

// Keyframes/EncodeTrim/CopyTrim are the §17 #5 half of the Runner. The
// defaults keep a trim on its happy path: one keyframe per second, an
// EncodeTrim that behaves exactly like Encode, and a CopyTrim that writes
// the same payload without pretending to decode anything.
func (f *fakeRunner) Keyframes(ctx context.Context, path string, videoIndex int) ([]float64, error) {
	f.record("Keyframes " + filepath.Base(path))
	if f.keyframesFn != nil {
		return f.keyframesFn(ctx, path, videoIndex)
	}
	return []float64{0, 1, 2, 3, 4, 5}, nil
}

func (f *fakeRunner) EncodeTrim(ctx context.Context, p transcode.Plan, win transcode.Window, in, out string, maxOutputBytes int64, onProgress func(float64, bool)) error {
	f.mu.Lock()
	f.windows = append(f.windows, win)
	f.mu.Unlock()
	return f.Encode(ctx, p, in, out, maxOutputBytes, onProgress)
}

func (f *fakeRunner) CopyTrim(ctx context.Context, t transcode.TrimPlan, p *transcode.ProbeResult, in, out string) error {
	f.record("CopyTrim " + filepath.Base(in))
	f.mu.Lock()
	f.windows = append(f.windows, t.Window)
	f.mu.Unlock()
	if f.copyFn != nil {
		return f.copyFn(ctx, t, p, in, out)
	}
	payload := f.encodeBytes
	if payload == nil {
		payload = []byte(renditionPayload)
	}
	return os.WriteFile(out, payload, 0o644)
}

// GIF/ProbeGIF are the §17 #6 half of the Runner. The defaults keep a gif
// job on its happy path: a palette and an output on disk, and a probe that
// reports a plausible animated preview.
func (f *fakeRunner) GIF(ctx context.Context, p transcode.GIFPlan, in, palette, out string) error {
	f.record("GIF " + filepath.Base(in))
	f.mu.Lock()
	f.gifPlans = append(f.gifPlans, p)
	f.mu.Unlock()
	if f.gifFn != nil {
		return f.gifFn(ctx, p, in, palette, out)
	}
	payload := f.gifBytes
	if payload == nil {
		payload = []byte(gifPayload)
	}
	if err := os.WriteFile(palette, []byte("palette"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(out, payload, 0o644)
}

func (f *fakeRunner) ProbeGIF(ctx context.Context, path string) (*transcode.GIFInfo, error) {
	f.record("ProbeGIF")
	if f.probeGIFFn != nil {
		return f.probeGIFFn(ctx, path)
	}
	return &transcode.GIFInfo{Width: 480, Height: 270, Frames: 72, Codec: "gif"}, nil
}

// Filmstrip is the §20 erratum 220 pass. The default writes a plausible sheet
// and answers the geometry the PLAN asked for, so a test can assert what a clip
// ends up carrying with no ffmpeg anywhere near it; the tile height is 16:9,
// which is what a 1280×720 rendition produces.
func (f *fakeRunner) Filmstrip(ctx context.Context, p transcode.FilmstripPlan, in, out string) (transcode.FilmstripResult, error) {
	f.record("Filmstrip " + filepath.Base(in))
	f.mu.Lock()
	f.filmstripPlans = append(f.filmstripPlans, p)
	f.mu.Unlock()
	if f.filmstripFn != nil {
		return f.filmstripFn(ctx, p, in, out)
	}
	payload := f.filmstripBytes
	if payload == nil {
		payload = []byte(filmstripPayload)
	}
	if err := os.WriteFile(out, payload, 0o644); err != nil {
		return transcode.FilmstripResult{}, err
	}
	return transcode.FilmstripResult{Tiles: p.Tiles, TileW: p.TileWidth, TileH: p.TileWidth * 9 / 16,
		IntervalMS: p.IntervalMS, Bytes: int64(len(payload))}, nil
}

// FilmstripPlans returns the plans handed to ffmpeg so far.
func (f *fakeRunner) FilmstripPlans() []transcode.FilmstripPlan {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]transcode.FilmstripPlan(nil), f.filmstripPlans...)
}

// GIFPlans returns the plans handed to ffmpeg so far.
func (f *fakeRunner) GIFPlans() []transcode.GIFPlan {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]transcode.GIFPlan(nil), f.gifPlans...)
}

// Windows returns the trim windows handed to ffmpeg so far.
func (f *fakeRunner) Windows() []transcode.Window {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]transcode.Window(nil), f.windows...)
}

func (f *fakeRunner) ProbeOutput(ctx context.Context, path string) (*transcode.OutputInfo, error) {
	f.record("ProbeOutput")
	if f.probeOutFn != nil {
		return f.probeOutFn(ctx, path)
	}
	// The dims MATCH the default plan (1280x720). Since the quality tiers a
	// produced tier is checked against its plan exactly rather than against a
	// frame box (transcode.CheckTierOutput), because a Full has no box — so a
	// fixture that measured something else would be asserting a bad_output.
	return &transcode.OutputInfo{Width: 1280, Height: 720, FPS: transcode.Rational{Num: 30, Den: 1}, DurationMS: 6000,
		HasAudio: true, VideoCodec: "h264", AudioCodec: "aac", VideoStreams: 1, AudioStreams: 1}, nil
}

func (f *fakeRunner) Verify(path string) error {
	f.record("Verify")
	if f.verifyFn != nil {
		return f.verifyFn(path)
	}
	return nil
}

func (f *fakeRunner) Poster(ctx context.Context, in, out string, durUS int64, has bool, videoEndSec float64,
	box transcode.PosterBox) (transcode.PosterResult, error) {
	f.record("Poster")
	// WHICH file the card image was cut from, and inside which frame, with no
	// ffmpeg anywhere near it: that pair is the whole subject of §20 erratum 219
	// (the tag is burned into the Light copy, so the poster is never taken from
	// it), and a fake that only counted the call could not say it.
	f.mu.Lock()
	f.posterCuts = append(f.posterCuts, posterCut{In: in, Box: box})
	f.mu.Unlock()
	if f.posterFn != nil {
		return f.posterFn(ctx, in, out, durUS, has)
	}
	if err := os.WriteFile(out, []byte("poster-bytes"), 0o644); err != nil {
		return transcode.PosterResult{}, err
	}
	return transcode.PosterResult{Width: 640, Height: 360}, nil
}

// PosterAt is the chosen-frame extraction (ROADMAP #5, §20 erratum 129). The
// default writes a plausible JPEG and records the seek, so a test can assert
// WHERE a card image was taken from with no ffmpeg anywhere near it.
func (f *fakeRunner) PosterAt(ctx context.Context, in, out string, seconds float64, durMS int64) (transcode.PosterResult, error) {
	f.record("PosterAt " + filepath.Base(in))
	f.mu.Lock()
	f.posterSeeks = append(f.posterSeeks, seconds)
	f.posterLabelMS = append(f.posterLabelMS, durMS)
	f.mu.Unlock()
	if f.posterPinFn != nil {
		return f.posterPinFn(ctx, in, out, seconds)
	}
	if err := os.WriteFile(out, []byte(chosenPosterPayload), 0o644); err != nil {
		return transcode.PosterResult{}, err
	}
	return transcode.PosterResult{Width: 1280, Height: 720, Seconds: seconds}, nil
}

// PosterSeeks returns every chosen-frame seek handed to ffmpeg so far.
func (f *fakeRunner) PosterSeeks() []float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]float64(nil), f.posterSeeks...)
}

// PosterLabelMS is the duration each PosterAt call was told to burn into the
// frame's corner (§20 erratum 287).
func (f *fakeRunner) PosterLabelMS() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.posterLabelMS...)
}

// posterCut is one card-image extraction: the file it was cut from and the frame
// it was written inside (§20 erratum 219).
type posterCut struct {
	In  string
	Box transcode.PosterBox
}

// PosterCuts returns every card-image extraction the poster search has run.
func (f *fakeRunner) PosterCuts() []posterCut {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]posterCut(nil), f.posterCuts...)
}

// PosterFrameSeconds is the gif job's re-derivation of the poster's chosen
// frame (§20 erratum 92(c), amended). The default answers the blind seek,
// which is the fallback the real one uses when ffmpeg says nothing, so every
// existing gif-plan assertion keeps its old numbers; posterAtFn overrides it
// for the tests that are about the window.
func (f *fakeRunner) PosterFrameSeconds(ctx context.Context, in string, durUS int64, has bool, videoEndSec float64) float64 {
	f.record("PosterFrameSeconds")
	if f.posterAtFn != nil {
		return f.posterAtFn(ctx, in, durUS, has, videoEndSec)
	}
	return transcode.PosterSeek(durUS, has, videoEndSec)
}

type harness struct {
	t       *testing.T
	db      *db.DB
	store   *store.Fake
	runner  *fakeRunner
	clock   *clock
	logs    *logSink
	cfg     config.Config
	w       *worker
	user    db.User
	ready   chan db.Clip
	free    int64
	settles *settleLog
}

// settleGuard bounds every wait on the settle hook. It is a HANG guard, not a
// budget: a wait ends at the settle that satisfies it, and only a job that never
// settles at all reaches this (R1 of run 9's test-toil proposal, §4).
const settleGuard = time.Minute

// settleLog is the harness's end of Deps.OnSettled (§20 erratum 331): every job
// this worker has settled, in order, and a channel closed and replaced on each
// one so a waiter blocks instead of polling.
type settleLog struct {
	mu   sync.Mutex
	got  []Settled
	next chan struct{}
}

func newSettleLog() *settleLog { return &settleLog{next: make(chan struct{})} }

func (s *settleLog) add(e Settled) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, e)
	close(s.next)
	s.next = make(chan struct{})
}

// watch returns what has settled so far and the channel the NEXT settle
// closes, read together under the lock so a settle between a check and the
// wait that follows it is never lost.
func (s *settleLog) watch() ([]Settled, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Settled(nil), s.got...), s.next
}

// until re-evaluates check once now and again after every settle, and fails the
// test only if the guard runs out first. It never sleeps: a condition that only a
// settled job can make true is seen at that job's settle.
func (h *harness) until(what string, check func() bool) {
	h.t.Helper()
	guard := time.NewTimer(settleGuard)
	defer guard.Stop()
	for {
		_, next := h.settles.watch()
		if check() {
			return
		}
		select {
		case <-next:
		case <-guard.C:
			h.t.Fatalf("%s: never true at any settle within %s\nlogs:\n%s", what, settleGuard, h.logs.String())
		}
	}
}

// settledAfter waits for a settle at an index ≥ mark (h.settles.mark() taken
// before the action) that matches pred, and returns it.
func (h *harness) settledAfter(mark int, pred func(Settled) bool) Settled {
	h.t.Helper()
	var hit Settled
	h.until("a matching job settle", func() bool {
		got, _ := h.settles.watch()
		for _, e := range got[min(mark, len(got)):] {
			if pred(e) {
				hit = e
				return true
			}
		}
		return false
	})
	return hit
}

// mark is the index the next settle will take.
func (s *settleLog) mark() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

// awaitJob is the settle-driven wait every job waiter below shares: lookup is
// re-read at the start and after every settle, and the wait ends when it returns
// a job that matches pred AND that this process is no longer running. runJob
// untracks a job after every effect of its run but the settle call itself (the
// card-ready call, the log lines, the counters), so a job that matches and is not
// held has nothing left to write. A row the run committed while its exit path is
// still going does not end the wait; that run's settle does.
func (h *harness) awaitJob(what string, lookup func() (db.Job, bool), pred func(db.Job) bool) db.Job {
	h.t.Helper()
	guard := time.NewTimer(settleGuard)
	defer guard.Stop()
	for {
		_, next := h.settles.watch()
		if j, ok := lookup(); ok && pred(j) && !h.w.holdsJob(j.ID) {
			return j
		}
		select {
		case <-next:
		case <-guard.C:
			j, _ := lookup()
			h.t.Fatalf("%s never matched at any settle within %s (state=%s stage=%s attempts=%d transient=%d err=%v)\nlogs:\n%s",
				what, settleGuard, j.State, j.Stage, j.Attempts, j.TransientFailures, j.LastError, h.logs.String())
		}
	}
}

// latestOfKind is awaitJob's lookup for the clip's newest job of one kind.
func (h *harness) latestOfKind(clipID, kind string) func() (db.Job, bool) {
	return func() (db.Job, bool) {
		j, err := h.db.LatestJobOfKind(context.Background(), clipID, kind)
		return j, err == nil
	}
}

func newHarness(t *testing.T) *harness { return newHarnessCfg(t, nil) }

// newHarnessCfg is newHarness with a last look at the config before the
// worker is built (the bucket coordinates a leak test needs, say).
func newHarnessCfg(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "clippet.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	user, err := d.CreateUser(context.Background(), ids.New(), "carol", "$2a$12$hash", 1000)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	h := &harness{t: t, db: d, store: store.NewFake(), runner: &fakeRunner{}, logs: &logSink{}, user: user,
		clock: &clock{t: time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)}, ready: make(chan db.Clip, 8), free: 1 << 40,
		settles: newSettleLog()}
	h.cfg = config.Config{
		ScratchDir:     t.TempDir(),
		ScratchMinFree: 1 << 20,
		S3:             config.S3Config{KeyPrefix: "", TransferMax: time.Minute},
		Transcode: config.TranscodeConfig{Concurrency: 1, Preset: "veryfast", CRF: 23, MaxFPS: 60, MaxW: 1280, MaxH: 720,
			StallTimeout: time.Minute, HardTimeout: time.Hour, MaxAttempts: 3, MaxTransient: 10},
	}
	if mutate != nil {
		mutate(&h.cfg)
	}
	log := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.w = New(Deps{
		DB: d, Store: h.store, Cfg: h.cfg, Log: log, Now: h.clock.Now, Runner: h.runner,
		Hooks:     Hooks{OnReady: func(_ context.Context, c db.Clip) { h.ready <- c }},
		FreeBytes: func(string) (int64, error) { return h.free, nil },
		OnSettled: h.settles.add,
	}).(*worker)
	return h
}

// newClip inserts a queued clip + job whose original lives under ext and,
// when seed is true, exists in the Fake so acquire can download it.
func (h *harness) newClip(ext string, size int64, seed bool) (db.Clip, db.Job) {
	h.t.Helper()
	id := ids.New()
	key := store.OriginalKey(h.cfg.S3.KeyPrefix, id, ext)
	if seed {
		h.store.Put(key, bytes.Repeat([]byte("s"), int(size)), "video/mp4")
	}
	c, j, err := h.db.CreateClipWithJob(context.Background(), db.NewClip{
		ID: id, OwnerID: h.user.ID, Title: "t", SourceKey: key, SourceFilename: "clip.mp4",
		SourceSize: size, SourceContentType: "video/mp4", SourceETag: "etag", Now: h.clock.Now().Unix(),
	})
	if err != nil {
		h.t.Fatalf("CreateClipWithJob: %v", err)
	}
	return c, j
}

// start runs the worker until the test ends.
func (h *harness) start() context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	h.w.Start(ctx)
	h.w.Wake()
	h.t.Cleanup(func() {
		cancel()
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		if err := h.w.Stop(sctx); err != nil {
			h.t.Errorf("Stop: %v", err)
		}
	})
	return cancel
}

func (h *harness) waitClip(id, state string) db.Clip {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := h.db.GetClip(context.Background(), id)
		if err == nil && c.State == state {
			return c
		}
		time.Sleep(10 * time.Millisecond) // sleep: a clip row can be the subject while its run is still exiting (no settle yet)
	}
	c, _ := h.db.GetClip(context.Background(), id)
	h.t.Fatalf("clip %s never reached %s (state=%s)\nlogs:\n%s", id, state, c.State, h.logs.String())
	return db.Clip{}
}

// waitJob waits, on the settle hook, for the clip's latest job to match pred
// with its run settled (awaitJob).
func (h *harness) waitJob(clipID string, pred func(db.Job) bool) db.Job {
	h.t.Helper()
	return h.awaitJob("job for "+clipID, func() (db.Job, bool) {
		j, err := h.db.GetLatestJob(context.Background(), clipID)
		return j, err == nil
	}, pred)
}

// waitIdle waits for the run registry to empty: each run that leaves it
// settles its job afterwards, so the settle hook is the wake-up.
func (h *harness) waitIdle() {
	h.t.Helper()
	h.until("the worker is idle", func() bool { return h.w.active() == 0 })
}

func (h *harness) assertNoErrorLogs() {
	h.t.Helper()
	if logs := h.logs.String(); strings.Contains(logs, "level=ERROR") {
		h.t.Fatalf("ERROR line in logs:\n%s", logs)
	}
}

func (h *harness) assertLog(level, substr string) {
	h.t.Helper()
	for _, line := range strings.Split(h.logs.String(), "\n") {
		if strings.Contains(line, "level="+level) && strings.Contains(line, substr) {
			return
		}
	}
	h.t.Fatalf("no %s log line containing %q:\n%s", level, substr, h.logs.String())
}

// waitLog is assertLog's patient twin, for a line the run writes AFTER the
// database observable the test waited on. `jobRun.fail` commits the attempt
// FIRST (FailAttempt) and writes its WARN line LAST, behind the key deletes,
// the scratch removal and the progress write — so a test that reads h.logs the
// instant waitJob returns is racing the logger across several statements, and
// lost that race once under `-race`. Polling the same locked buffer can never
// see half a line.
//
// Since the settle hook (§20 erratum 331) it re-reads the buffer at every job
// settle instead of on a timer: a line a run writes is written before that run
// settles.
func (h *harness) waitLog(substr string) {
	h.t.Helper()
	h.until(fmt.Sprintf("a log line containing %q", substr), func() bool { return strings.Contains(h.logs.String(), substr) })
}

func (h *harness) assertOriginalIntact(c db.Clip) {
	h.t.Helper()
	for _, k := range h.store.Deletes() {
		if strings.Contains(k, "/original/") {
			h.t.Fatalf("original deleted: %s", k)
		}
	}
	if _, ok := h.store.Get(c.SourceKey); !ok {
		h.t.Fatalf("original %s missing from the bucket", c.SourceKey)
	}
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

var (
	renditionRE = regexp.MustCompile(`^clips/[A-Za-z0-9]{12}/renditions/main-[A-Za-z0-9]{8}\.mp4$`)
	posterRE    = regexp.MustCompile(`^clips/[A-Za-z0-9]{12}/poster/poster-[A-Za-z0-9]{8}\.jpg$`)
)

// ---- tests ----

func TestHappyPath(t *testing.T) {
	h := newHarness(t)
	c, j := h.newClip("mp4", 100, true)
	h.start()
	got := h.waitClip(c.ID, db.ClipReady)

	if got.RenditionKey == nil || !renditionRE.MatchString(*got.RenditionKey) {
		t.Fatalf("rendition_key = %s", str(got.RenditionKey))
	}
	if got.PosterKey == nil || !posterRE.MatchString(*got.PosterKey) {
		t.Fatalf("poster_key = %s", str(got.PosterKey))
	}
	if got.RenditionSize == nil || *got.RenditionSize != int64(len(renditionPayload)) || str(got.RenditionETag) != sha(renditionPayload) {
		t.Fatalf("rendition size/etag = %v/%s", got.RenditionSize, str(got.RenditionETag))
	}
	if str(got.PosterETag) != sha("poster-bytes") || *got.PosterSize != int64(len("poster-bytes")) {
		t.Fatalf("poster size/etag = %v/%s", got.PosterSize, str(got.PosterETag))
	}
	if *got.PosterWidth != 640 || *got.PosterHeight != 360 {
		t.Fatalf("poster dims must be the MEASURED jpeg dims: %dx%d", *got.PosterWidth, *got.PosterHeight)
	}
	if *got.Width != 1280 || *got.Height != 720 || *got.DurationMS != 6000 || *got.FPSNum != 30 || *got.FPSDen != 1 || !*got.HasAudio {
		t.Fatalf("measured fields: %dx%d %dms %d/%d audio=%v", *got.Width, *got.Height, *got.DurationMS, *got.FPSNum, *got.FPSDen, *got.HasAudio)
	}
	if got.ReadyAt == nil || got.FailureReason != nil {
		t.Fatalf("ready_at=%v failure_reason=%v", got.ReadyAt, got.FailureReason)
	}

	// Bucket: original + rendition + poster + probe.json; probe.json carries
	// source/plan/output; nothing deleted.
	keys := h.store.Keys()
	if len(keys) != 4 {
		t.Fatalf("bucket keys = %v", keys)
	}
	raw, ok := h.store.Get(store.ProbeKey("", c.ID))
	if !ok {
		t.Fatal("probe.json not uploaded")
	}
	var doc probeDoc
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Output == nil || doc.Output.Width != 1280 || doc.Plan.W != 1280 || string(doc.Source) != `{"streams":[{"index":0}]}` {
		t.Fatalf("probe.json = %s (%v)", raw, err)
	}
	if d := h.store.Deletes(); len(d) != 0 {
		t.Fatalf("deletes = %v", d)
	}
	h.assertOriginalIntact(got)
	calls := h.store.CallsSnapshot()
	want := []string{"Download " + c.SourceKey, "PutFile " + *got.RenditionKey, "PutFile " + *got.PosterKey, "PutFile " + store.ProbeKey("", c.ID)}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("store calls = %v, want %v", calls, want)
	}
	if rc := h.runner.Calls(); strings.Join(rc, "|") != "Probe source.mp4|Encode source.mp4|ProbeOutput|Verify|Poster" {
		t.Fatalf("runner calls = %v", rc)
	}

	// Job done with a checkpoint; scratch gone; registry terminal; hook ran.
	job := h.waitJob(c.ID, func(j db.Job) bool { return j.State == db.JobDone })
	if job.ID != j.ID || job.Stage != stageDone || job.CheckpointJSON == nil || job.FinishedAt == nil {
		t.Fatalf("job = %+v", job)
	}
	h.waitIdle() // the row is committed before the run's deferred scratch cleanup
	if _, err := os.Stat(filepath.Join(h.cfg.ScratchDir, c.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch dir not removed: %v", err)
	}
	if p, ok := h.w.Progress(c.ID); !ok || p.State != db.ClipReady || p.Percent != 100 || p.Stage != stageDone {
		t.Fatalf("progress = %+v, %v", p, ok)
	}
	select {
	case rc := <-h.ready:
		if rc.ID != c.ID || rc.State != db.ClipReady {
			t.Fatalf("OnReady got %+v", rc)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnReady not called")
	}
	h.assertNoErrorLogs()
	h.assertLog("INFO", "clip ready")
}

func TestScratchSourceFastPathAndExt(t *testing.T) {
	h := newHarness(t)
	// ext comes from source_key (.mov), never from source_filename (.mp4).
	c, _ := h.newClip("mov", 64, true)
	dir := filepath.Join(h.cfg.ScratchDir, c.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source.mov"), bytes.Repeat([]byte("x"), 64), 0o644); err != nil {
		t.Fatal(err)
	}
	h.start()
	h.waitClip(c.ID, db.ClipReady)
	for _, call := range h.store.CallsSnapshot() {
		if strings.HasPrefix(call, "Download") {
			t.Fatalf("scratch source of the right size must not be re-downloaded: %v", h.store.CallsSnapshot())
		}
	}
	if rc := h.runner.Calls(); rc[0] != "Probe source.mov" {
		t.Fatalf("runner calls = %v", rc)
	}
	h.assertNoErrorLogs()

	// A stale source with the wrong size is replaced by a download.
	h2 := newHarness(t)
	c2, _ := h2.newClip("mkv", 64, true)
	dir2 := filepath.Join(h2.cfg.ScratchDir, c2.ID)
	if err := os.MkdirAll(dir2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir2, "source.mkv"), []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	h2.start()
	h2.waitClip(c2.ID, db.ClipReady)
	if calls := h2.store.CallsSnapshot(); len(calls) == 0 || calls[0] != "Download "+c2.SourceKey {
		t.Fatalf("stale source must be re-downloaded: %v", calls)
	}
	h2.assertNoErrorLogs()
}

// downloadSpy wraps the Fake to observe the progress callback acquire hands
// to Store.Download. before/after, when set, run on the download goroutine
// around each forwarded callback – a deterministic point to drive the fake
// clock and read the registry while acquire is still in flight.
type downloadSpy struct {
	store.Store
	mu     sync.Mutex
	nils   int
	fracs  []float64
	before func()
	after  func()
}

func (s *downloadSpy) Download(ctx context.Context, key, path string, progress func(done, total int64)) error {
	if progress == nil {
		s.mu.Lock()
		s.nils++
		s.mu.Unlock()
		return s.Store.Download(ctx, key, path, nil)
	}
	return s.Store.Download(ctx, key, path, func(d, tot int64) {
		if s.before != nil {
			s.before()
		}
		s.mu.Lock()
		s.fracs = append(s.fracs, float64(d)/float64(tot))
		s.mu.Unlock()
		progress(d, tot)
		if s.after != nil {
			s.after()
		}
	})
}

// TestAcquireReportsDownloadProgress: §10.3 step 1 – a re-downloaded source
// feeds frac = bytes/source_size through the progress chain instead of
// sitting at a frozen 0% for the whole transfer, and the worker's own
// registry (what GET /progress serves) shows the acquire band advancing
// while the download is still in flight – before encode begins.
func TestAcquireReportsDownloadProgress(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 6<<16, true) // 6 chunks of the Fake's 64 KiB step
	spy := &downloadSpy{Store: h.store}
	// Progress lines are rate-limited on the registry clock (250ms): each
	// step advances the fake clock so its line lands, then reads what
	// Worker.Progress reports at that instant (same goroutine: no race).
	var seen []Progress
	spy.before = func() { h.clock.Advance(time.Second) }
	spy.after = func() {
		p, ok := h.w.Progress(c.ID)
		if !ok {
			t.Error("no registry entry during acquire")
			return
		}
		spy.mu.Lock()
		seen = append(seen, p)
		spy.mu.Unlock()
	}
	// At encode start every acquire line has been observed and the registry
	// has moved on to the encode floor.
	var atEncode Progress
	var acquireSeen int
	h.runner.encodeFn = func(_ context.Context, _ transcode.Plan, _, out string, onProgress func(float64, bool)) error {
		p, _ := h.w.Progress(c.ID)
		spy.mu.Lock()
		atEncode, acquireSeen = p, len(seen)
		spy.mu.Unlock()
		if err := os.WriteFile(out, []byte(renditionPayload), 0o644); err != nil {
			return err
		}
		onProgress(1, false)
		return nil
	}
	h.w.deps.Store = spy
	h.start()
	h.waitClip(c.ID, db.ClipReady)

	spy.mu.Lock()
	defer spy.mu.Unlock()
	if spy.nils > 0 {
		t.Fatal("acquire called Download with a nil progress callback")
	}
	if len(spy.fracs) < 2 {
		t.Fatalf("download progress fired %d times, want intermediate fractions", len(spy.fracs))
	}
	for i, f := range spy.fracs {
		if f < 0 || f > 1 || (i > 0 && f < spy.fracs[i-1]) {
			t.Fatalf("fractions not monotone in [0,1]: %v", spy.fracs)
		}
	}
	if last := spy.fracs[len(spy.fracs)-1]; last != 1 {
		t.Fatalf("download progress ended at %v, want 1", last)
	}

	// The registry as GET /progress would have seen it mid-download: stage
	// acquire throughout, percent monotone within the band and past its
	// floor before the download ends, at the ceiling when it does.
	if len(seen) != len(spy.fracs) {
		t.Fatalf("registry read %d times, callbacks fired %d times", len(seen), len(spy.fracs))
	}
	lo, hi := bands[stageAcquire][0], bands[stageAcquire][1]
	for i, p := range seen {
		if p.State != db.ClipProcessing || p.Stage != stageAcquire || p.Indeterminate {
			t.Fatalf("registry during acquire #%d = %+v", i, p)
		}
		if p.Percent < lo || p.Percent > hi || (i > 0 && p.Percent < seen[i-1].Percent) {
			t.Fatalf("acquire percent not monotone within [%d,%d]: %+v", lo, hi, seen)
		}
	}
	if mid := seen[len(seen)/2]; mid.Percent <= lo {
		t.Fatalf("acquire band still at its floor halfway through the download: %+v", seen)
	}
	if last := seen[len(seen)-1]; last.Percent != hi {
		t.Fatalf("acquire ended at %d%%, want the band ceiling %d: %+v", last.Percent, hi, seen)
	}
	// Encode began only after every acquire line, at its own band floor.
	if acquireSeen != len(seen) || atEncode.Stage != stageEncode || atEncode.Percent != bands[stageEncode][0] {
		t.Fatalf("at encode start: %d/%d acquire lines seen, registry = %+v", acquireSeen, len(seen), atEncode)
	}
	h.assertNoErrorLogs()
}

func TestPermanentFailure(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 100, true)
	h.runner.encodeFn = func(context.Context, transcode.Plan, string, string, func(float64, bool)) error {
		return &transcode.ErrPermanent{Err: errors.New("ffmpeg exited 1: Invalid data found")}
	}
	h.start()
	got := h.waitClip(c.ID, db.ClipFailed)
	if str(got.FailureReason) != "encode: ffmpeg exited 1: Invalid data found" {
		t.Fatalf("failure_reason = %s", str(got.FailureReason))
	}
	job := h.waitJob(c.ID, func(j db.Job) bool { return j.State == db.JobFailed })
	if job.Attempts != 0 || job.TransientFailures != 0 || str(job.LastError) != str(got.FailureReason) {
		t.Fatalf("job = %+v", job)
	}
	h.assertOriginalIntact(got)
	if d := h.store.Deletes(); len(d) != 0 {
		t.Fatalf("deletes = %v", d)
	}
	h.waitIdle() // the failed row is committed before the run's deferred scratch cleanup
	if _, err := os.Stat(filepath.Join(h.cfg.ScratchDir, c.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("scratch dir must be removed on permanent failure")
	}
	if p, ok := h.w.Progress(c.ID); !ok || p.State != db.ClipFailed || p.Error == "" || p.Stage != stageEncode {
		t.Fatalf("progress = %+v", p)
	}
	h.assertNoErrorLogs() // a bad file is a normal input: INFO
	h.assertLog("INFO", "clip failed")
}

func TestVerifyMismatchIsPermanentAndLogsError(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 100, true)
	h.runner.probeOutFn = func(context.Context, string) (*transcode.OutputInfo, error) {
		return &transcode.OutputInfo{Width: 1281, Height: 720, FPS: transcode.Rational{Num: 30, Den: 1}, DurationMS: 6000,
			VideoCodec: "h264", VideoStreams: 1}, nil
	}
	mark := h.settles.mark()
	h.start()
	// jobRun.fail commits FailAttempt FIRST and writes the ERROR line after the
	// key deletes, the scratch removal and the progress write, so a wait on the
	// clip row could return before the line existed — the race this test lost on
	// a box shared with concurrent check runs (a nit sweep, run 6). The job's SETTLE
	// is after the line (§20 erratum 331): wait for it, then read once.
	h.settledAfter(mark, func(s Settled) bool { return s.ClipID == c.ID && s.Kind == db.KindTranscode })
	got := h.clip(c.ID)
	if got.State != db.ClipFailed || !strings.HasPrefix(str(got.FailureReason), "verify: bad_output:") {
		t.Fatalf("clip = %s, failure_reason = %s", got.State, str(got.FailureReason))
	}
	h.assertLog("ERROR", "bad_output")
	h.assertOriginalIntact(got)
}

func TestTransientRequeueThenRetry(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 100, true)
	var once sync.Once
	h.store.Hook = func(_ context.Context, op, key string) error {
		var err error
		if op == "PutFile" && strings.Contains(key, "/poster/") {
			once.Do(func() { err = errors.New("s3: 503 slow down") })
		}
		return err
	}
	h.start()
	job := h.waitJob(c.ID, func(j db.Job) bool { return j.State == db.JobQueued && j.TransientFailures == 1 })
	now := h.clock.Now().Unix()
	if job.Attempts != 0 || job.RunAfter != now+60 || job.Stage != db.JobStageQueued || !strings.Contains(str(job.LastError), "store: upload poster: s3: 503") {
		t.Fatalf("job = %+v (now=%d)", job, now)
	}
	h.waitIdle()
	clip, _ := h.db.GetClip(context.Background(), c.ID)
	if clip.State != db.ClipQueued {
		t.Fatalf("clip state = %s", clip.State)
	}
	// The rendition uploaded by the failed attempt was deleted; the original was not.
	d := h.store.Deletes()
	if len(d) != 1 || !renditionRE.MatchString(d[0]) {
		t.Fatalf("deletes = %v", d)
	}
	h.assertOriginalIntact(clip)
	// The key of the FAILED upload was deleted too — that is the one that can
	// hold an incomplete multipart upload (§11). By KEY since §20 erratum 305:
	// a listing cannot be trusted to show what was written a moment ago.
	calls := h.store.CallsSnapshot()
	var posterKey string
	for _, call := range calls {
		if strings.HasPrefix(call, "PutFile ") && strings.Contains(call, "/poster/") {
			posterKey = strings.TrimPrefix(call, "PutFile ")
			break
		}
	}
	if posterKey == "" || !slices.Contains(calls, "DeleteKey "+posterKey) {
		t.Fatalf("the failed poster upload's key must be cleaned up: %v", calls)
	}
	// Scratch keeps the source, drops the attempt's outputs.
	dir := filepath.Join(h.cfg.ScratchDir, c.ID)
	if _, err := os.Stat(filepath.Join(dir, "source.mp4")); err != nil {
		t.Fatalf("source must survive a transient failure: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("scratch outputs not removed: %v", entries)
	}
	if p, ok := h.w.Progress(c.ID); !ok || p.State != db.ClipQueued {
		t.Fatalf("progress = %+v", p)
	}
	h.assertLog("WARN", "requeued")
	h.assertNoErrorLogs()

	// Backoff elapses → the retry runs with FRESH r8 keys and succeeds.
	h.clock.Advance(61 * time.Second)
	h.w.Wake()
	got := h.waitClip(c.ID, db.ClipReady)
	if *got.RenditionKey == d[0] {
		t.Fatal("retry must use a new r8 rendition key")
	}
	keys := h.store.Keys()
	if len(keys) != 4 {
		t.Fatalf("bucket after retry = %v", keys)
	}
	job = h.waitJob(c.ID, func(j db.Job) bool { return j.State == db.JobDone })
	if job.Attempts != 0 || job.TransientFailures != 1 {
		t.Fatalf("counters after retry: %+v", job)
	}
	h.assertNoErrorLogs()
}

// A scratch shortage is transient in the ordinary sense — a disk fills and
// empties again — but it must NOT spend TRANSCODE_MAX_TRANSIENT (§20 erratum
// 90). Counting it let a disk that stayed full for the ten backoffs fail every
// queued clip PERMANENTLY, and the reason a viewer then read on the share page
// was a sentence about the server's free space. The job goes back to `queued`
// with a backoff, both counters untouched, and the stored reason is the
// bounded sentence with no numbers in it.
func TestInsufficientScratchRequeuesWithoutSpendingTheOutageBudget(t *testing.T) {
	h := newHarness(t)
	h.free = 10
	c, _ := h.newClip("mp4", 100, true)
	h.start()
	job := h.waitJob(c.ID, func(j db.Job) bool { return j.State == db.JobQueued && j.LastError != nil })
	if job.Attempts != 0 || job.TransientFailures != 0 {
		t.Fatalf("a scratch shortage spent a retry budget: %+v", job)
	}
	assertScratchReason(t, job)
	if want := h.clock.Now().Unix(); job.RunAfter <= want {
		t.Errorf("run_after = %d, want a backoff past %d: against a full disk an immediate retry is a spin", job.RunAfter, want)
	}
	clip := h.clip(c.ID)
	if clip.State != db.ClipQueued || clip.FailureReason != nil {
		t.Fatalf("clip = %q/%v, want queued with no public failure reason", clip.State, clip.FailureReason)
	}
	h.waitIdle()
	if rc := h.runner.Calls(); len(rc) != 0 {
		t.Fatalf("runner must not run without scratch space: %v", rc)
	}
	h.assertNoErrorLogs()
}

// The DETAIL a disk refusal logs is redacted by every run, not by some of
// them (N046/N057). `detail` is operator-only — requeueNoCount writes it on a
// WARN line and it never reaches a clip column or a DTO — but the transcode,
// the gif and the social runs passed the raw err.Error() where the trim, the
// poster and the replace passed reason(err), so three of the six wrote the
// same event two different ways and only three had the redaction in front of
// them. Nothing leaked today (errNoScratchRoom's two constructors carry no
// path) which is exactly why it had to be pinned before something did.
func TestEveryDiskRefusalLogsARedactedDetail(t *testing.T) {
	h := newHarnessCfg(t, func(c *config.Config) {
		c.S3.Endpoint, c.S3.Bucket, c.S3.KeyPrefix = "http://minio:9000", "clippet", "p/"
	})
	c, job := h.newClip("mp4", 100, false)
	dir := filepath.Join(h.cfg.ScratchDir, c.ID)
	// The shape a stage can hand noRoom once anything wraps the sentinel: the
	// sentinel itself, plus a path and the bucket coordinates the six runs all
	// promise never to print.
	err := fmt.Errorf("%w: no room under %s for %s", errNoScratchRoom,
		dir, h.cfg.S3.Endpoint+"/"+h.cfg.S3.Bucket+"/"+h.cfg.S3.KeyPrefix+"clips/"+c.ID)

	// Built through each kind's own constructor, not field by field (nit sweep
	// S07): the six newXRun functions are pure — they take (w, ctx, *run, job,
	// clip), need no registry entry and no live claim, and set log/r8/dir/paths
	// themselves — so a constructor that grows required state is caught here
	// instead of this test passing against a shape the worker no longer builds.
	// Only what a constructor cannot know is set afterwards: the stage, and
	// replace's dir (newReplaceRun derives it from params.Spool in setPaths, so
	// without this the redaction assertion would lose one of its four secrets).
	ctx := context.Background()
	runs := map[string]func(){
		"transcode": func() {
			j := newJobRun(h.w, ctx, &run{}, job, c)
			j.stage = stageAcquire
			j.noRoom(err)
		},
		"gif": func() {
			g := newGIFRun(h.w, ctx, &run{}, job, c)
			g.stage = stageAcquire
			g.noRoom(err)
		},
		"social": func() {
			s := newSocialRun(h.w, ctx, &run{}, job, c)
			s.stage = stageAcquire
			s.noRoom(err)
		},
		"poster": func() {
			p := newPosterRun(h.w, ctx, &run{}, job, c)
			p.stage = stageAcquire
			p.noRoom(err)
		},
		"trim": func() {
			tr := newTrimRun(h.w, ctx, &run{}, job, c)
			tr.stage = stageAcquire
			tr.noRoom(err)
		},
		"replace": func() {
			rp := newReplaceRun(h.w, ctx, &run{}, job, c)
			rp.dir, rp.stage = dir, stageAcquire
			rp.noRoom(err)
		},
	}
	for name, call := range runs {
		t.Run(name, func(t *testing.T) {
			h.logs.Reset()
			call()
			line := h.logs.String()
			if !strings.Contains(line, "no scratch room; requeued without spending the outage budget") {
				t.Fatalf("%s did not log the requeue: %s", name, line)
			}
			for _, secret := range []string{dir, h.cfg.ScratchDir, h.cfg.S3.Endpoint, h.cfg.S3.Bucket} {
				if secret != "" && strings.Contains(line, secret) {
					t.Fatalf("%s logged %q in its disk refusal: %s", name, secret, line)
				}
			}
			if !strings.Contains(line, "detail=") {
				t.Fatalf("%s logged no detail at all: %s", name, line)
			}
		})
	}
}

// assertScratchReason pins the whole of the PUBLIC text for that class: the
// bounded sentence, and not one digit of the operator's free-byte count.
func assertScratchReason(t *testing.T, job db.Job) {
	t.Helper()
	if got := str(job.LastError); got != msgNoScratchRoom {
		t.Fatalf("stored reason = %q, want exactly %q", got, msgNoScratchRoom)
	}
	if strings.ContainsAny(str(job.LastError), "0123456789") {
		t.Fatalf("the public reason names a byte count: %q", str(job.LastError))
	}
}

// The pre-encode free-space guard is sized from the PLAN, not only from the
// source (§20 erratum 42). The encode is CFR, so a container may declare a
// duration unrelated to its bytes and make ffmpeg duplicate frames to fill
// it: acquire's 2×source_size sees a tiny upload and waves through an encode
// that writes gigabytes into the shared scratch volume.
func TestPlannedOutputScratchGuard(t *testing.T) {
	h := newHarness(t)
	h.free = 1 << 30 // far more than 2× the source, far less than the plan
	h.runner.probeFn = func(context.Context, string) (*transcode.ProbeResult, error) {
		p := defaultProbeResult()
		p.DurationUS = 10 * 3600 * 1_000_000 // ten hours out of a 100-byte file
		return p, nil
	}
	c, _ := h.newClip("mp4", 100, true)
	h.start()
	job := h.waitJob(c.ID, func(j db.Job) bool { return j.State == db.JobQueued && j.LastError != nil })
	if job.Attempts != 0 || job.TransientFailures != 0 {
		t.Fatalf("job = %+v", job)
	}
	// The refusal came from the encode stage, and it spends no budget there
	// either (§20 erratum 90).
	assertScratchReason(t, job)
	if !strings.Contains(h.logs.String(), "stage=encode") {
		t.Fatalf("the refusal did not come from the encode stage:\n%s", h.logs.String())
	}
	h.waitIdle()
	for _, call := range h.runner.Calls() {
		if strings.HasPrefix(call, "Encode") {
			t.Fatalf("the encode ran without room for its output: %v", h.runner.Calls())
		}
	}
	h.assertNoErrorLogs()
}

// A backlog of clips whose sources ingest already spooled to scratch, on a
// disk with room for the operator's floor and for a rendition but not for a
// second copy of a source. Both drain (§20 erratum 90).
//
// The old rule asked for 2×source_size on top of the floor AFTER the source
// was confirmed on the very disk it was measuring, so it counted those bytes
// twice: ingest admits an upload at 1.5×size + floor and spools size, leaving
// 0.5×size + floor, and the worker then refused what the API had just taken.
// With a backlog the refusal is self-sustaining — every queued clip keeps its
// source on scratch across its failures, so nothing finishes and nothing frees
// anything, and the whole batch runs out of transient budget together.
func TestBacklogDrainsWithTheSourcesAlreadyOnScratch(t *testing.T) {
	h := newHarness(t)
	const size = 1 << 20
	// One second of 720p30 plans a 691,200-byte rendition; with the harness's
	// 1 MiB floor the encode needs 1,739,776 free. 2 MiB clears that and every
	// other real requirement, and is far under the 3,145,728 the double count
	// used to demand.
	h.free = 2 << 20
	h.runner.probeFn = func(context.Context, string) (*transcode.ProbeResult, error) {
		p := defaultProbeResult()
		p.DurationUS = 1_000_000
		return p, nil
	}
	var ids []string
	for i := 0; i < 2; i++ {
		// seed=false: the ONLY copy of this source is the one on scratch, so a
		// re-download would be an ERROR line (row/object drift) and the
		// assertion below would catch it.
		c, _ := h.newClip("mp4", size, false)
		dir := filepath.Join(h.cfg.ScratchDir, c.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "source.mp4"), bytes.Repeat([]byte("s"), size), 0o644); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	h.start()
	for _, id := range ids {
		c := h.waitClip(id, db.ClipReady)
		if c.FailureReason != nil {
			t.Fatalf("clip %s is ready with a failure reason %q", id, *c.FailureReason)
		}
	}
	h.waitIdle()
	h.assertNoErrorLogs()
}

// A storage failure's own text is a net/http *url.Error carrying the whole
// request URL — S3_ENDPOINT (for R2 the account id), the bucket and the key
// layout — and the reason a run stores is PUBLIC: the anonymous GET
// /api/clips/{id}, the unlimited GET …/progress and the share page all render
// `clips.failure_reason`, and the gif route prints a preview job's last_error
// to any caller. Errata 20 and 31 closed this class for scratch paths and for
// the health probes; erratum 90 closes the storage half, and closes it by
// having the store classify itself rather than by recognising error shapes
// here.
func TestStorageFailuresNeverReachThePublicReason(t *testing.T) {
	// What a refused connection to the compose MinIO actually says.
	const leak = `Get "http://minio:9000/clippet/p/clips/abc/original/source.mp4": ` +
		`dial tcp 172.19.0.3:9000: connect: connection refused`
	withBucket := func(c *config.Config) {
		c.S3.Endpoint, c.S3.Bucket, c.S3.KeyPrefix = "http://minio:9000", "clippet", "p/"
	}

	t.Run("a classified store error becomes its class and nothing else", func(t *testing.T) {
		h := newHarnessCfg(t, withBucket)
		h.store.FailNext("Download", &store.StorageError{Class: store.PublicUnavailable, Err: errors.New(leak)})
		c, _ := h.newClip("mp4", 100, true)
		h.start()
		job := h.waitJob(c.ID, func(j db.Job) bool { return j.State == db.JobQueued && j.TransientFailures == 1 })
		if got, want := str(job.LastError), stageAcquire+": "+store.PublicUnavailable; got != want {
			t.Fatalf("stored reason = %q, want exactly %q", got, want)
		}
		// The operator loses nothing: the whole error is on the WARN line — which
		// the run writes after the attempt this test just waited for, so it is
		// WAITED for rather than read the instant waitJob returns (§20 erratum
		// 226(e): that read failed once under `-race`).
		h.waitLog("connection refused")
		h.assertNoErrorLogs()
	})

	t.Run("an unclassified error keeps the redacted text", func(t *testing.T) {
		h := newHarnessCfg(t, withBucket)
		h.store.FailNext("Download", errors.New(leak))
		c, _ := h.newClip("mp4", 100, true)
		h.start()
		job := h.waitJob(c.ID, func(j db.Job) bool { return j.State == db.JobQueued && j.TransientFailures == 1 })
		assertNoBucketCoordinates(t, str(job.LastError), h.cfg)
		h.assertNoErrorLogs()
	})

	t.Run("a preview's 503 text is bounded the same way", func(t *testing.T) {
		h := newHarnessCfg(t, withBucket)
		c := h.gifReady()
		h.store.FailNext("Download", &store.StorageError{Class: store.PublicNotFound, Err: errors.New(leak)})
		h.enqueueGIF(c.ID)
		h.start()
		job := h.waitGIF(c.ID, func(j db.Job) bool { return j.State == db.JobQueued && j.TransientFailures == 1 })
		if got, want := str(job.LastError), stageAcquire+": "+store.PublicNotFound; got != want {
			t.Fatalf("gif reason = %q, want exactly %q", got, want)
		}
		if after := h.clip(c.ID); after.State != db.ClipReady || after.FailureReason != nil {
			t.Fatalf("clip is %q/%v after a failed preview", after.State, after.FailureReason)
		}
	})
}

// assertNoBucketCoordinates pins the second belt: whatever an unclassified
// error says, the endpoint, the bucket and the key prefix are struck out.
func assertNoBucketCoordinates(t *testing.T, reason string, cfg config.Config) {
	t.Helper()
	for _, needle := range []string{cfg.S3.Endpoint, cfg.S3.Bucket, cfg.S3.KeyPrefix} {
		if needle != "" && strings.Contains(reason, needle) {
			t.Fatalf("the public reason names %q: %q", needle, reason)
		}
	}
}

func TestPlannedOutputBytes(t *testing.T) {
	plan := func(mut func(*transcode.Plan)) transcode.Plan {
		p := transcode.Plan{W: 1280, H: 720, FPS: transcode.Rational{Num: 30, Den: 1}, HasDuration: true, DurationUS: 1_000_000}
		mut(&p)
		return p
	}
	// 1280·720·30·1s·0.2/8 = 691,200 bytes for one second of 720p30.
	if got := plannedOutputBytes(plan(func(*transcode.Plan) {})); got != 691_200 {
		t.Errorf("one second of 720p30 = %d", got)
	}
	// Linear in the declared duration: an hour is 3600×.
	if got := plannedOutputBytes(plan(func(p *transcode.Plan) { p.DurationUS = 3600 * 1_000_000 })); got != 691_200*3600 {
		t.Errorf("one hour = %d", got)
	}
	// Not sizeable → 0 (the source-based guard is all there is).
	for name, p := range map[string]transcode.Plan{
		"no duration": plan(func(p *transcode.Plan) { p.HasDuration = false }),
		"zero":        plan(func(p *transcode.Plan) { p.DurationUS = 0 }),
		"negative":    plan(func(p *transcode.Plan) { p.DurationUS = -1 }),
		"no fps":      plan(func(p *transcode.Plan) { p.FPS = transcode.Rational{} }),
		"no dims":     plan(func(p *transcode.Plan) { p.W = 0 }),
	} {
		if got := plannedOutputBytes(p); got != 0 {
			t.Errorf("%s = %d, want 0", name, got)
		}
	}
	// An absurd declared duration stays positive, and saturates rather than
	// wrapping when the product runs past int64.
	if got := plannedOutputBytes(plan(func(p *transcode.Plan) { p.DurationUS = math.MaxInt64 })); got <= 0 {
		t.Errorf("huge duration = %d", got)
	}
	huge := plan(func(p *transcode.Plan) {
		p.DurationUS, p.FPS = math.MaxInt64, transcode.Rational{Num: 60, Den: 1}
	})
	if got := plannedOutputBytes(huge); got != math.MaxInt64 {
		t.Errorf("saturation = %d", got)
	}
}

// The runtime output budget is armed even when the plan cannot be sized
// (§20 erratum 46). `format.duration` is uploader-written metadata: a
// container muxed to a non-seekable output (MediaRecorder, ffmpeg or OBS
// piping) carries none at all, so the pre-encode guard reserves nothing and
// the CFR gap fill — which follows the frame timestamps, not the declared
// duration — would otherwise run to TRANSCODE_HARD_TIMEOUT.
func TestOutputBudgetCapsAnUnsizedPlan(t *testing.T) {
	h := newHarness(t)
	h.runner.probeFn = func(context.Context, string) (*transcode.ProbeResult, error) {
		p := defaultProbeResult()
		p.HasDuration, p.DurationUS = false, 0
		return p, nil
	}
	// 2×source is all the budget there is here, and the run writes past it.
	h.runner.encodeBytes = bytes.Repeat([]byte("x"), 300)
	c, _ := h.newClip("mp4", 100, true)
	h.start()
	got := h.waitClip(c.ID, db.ClipFailed)
	if !strings.HasPrefix(str(got.FailureReason), "encode: output_too_large:") {
		t.Fatalf("failure_reason = %s", str(got.FailureReason))
	}
	if b := h.runner.Budgets(); len(b) != 1 || b[0] != 200 {
		t.Fatalf("budgets = %v, want one 200-byte cap (an unsized plan must still be capped)", b)
	}
	// Nothing of the runaway reached the bucket, and it is not retried.
	if keys := h.store.Keys(); len(keys) != 1 || keys[0] != c.SourceKey {
		t.Fatalf("bucket = %v, want the original alone", keys)
	}
	job := h.waitJob(c.ID, func(j db.Job) bool { return j.State == db.JobFailed })
	if job.TransientFailures != 0 {
		t.Fatalf("a runaway output is permanent, not requeued: %+v", job)
	}
	h.assertOriginalIntact(got)
	h.assertNoErrorLogs() // a lying container is a normal input: INFO
}

// A sized plan is capped at what checkOutputRoom reserved for it, and the
// budget never drops below the bounds acquire and the operator's own
// MAX_UPLOAD_BYTES already granted this clip.
func TestOutputBudget(t *testing.T) {
	job := func(size int64, maxUpload int64, mut func(*transcode.Plan)) *jobRun {
		p := transcode.Plan{W: 1280, H: 720, FPS: transcode.Rational{Num: 30, Den: 1}, HasDuration: true, DurationUS: 1_000_000}
		mut(&p)
		return &jobRun{w: newWorker(Deps{Cfg: config.Config{MaxUploadBytes: maxUpload}}),
			clip: db.Clip{SourceSize: size}, plan: p}
	}
	// The plan dominates: one second of 720p30 is 691,200 bytes.
	if got := job(100, 1000, func(*transcode.Plan) {}).outputBudget(); got != 691_200 {
		t.Errorf("plan term = %d", got)
	}
	// An unsized plan falls to the larger of 2×source and MAX_UPLOAD_BYTES.
	if got := job(100, 1000, func(p *transcode.Plan) { p.HasDuration = false }).outputBudget(); got != 1000 {
		t.Errorf("upload-cap term = %d", got)
	}
	if got := job(9000, 1000, func(p *transcode.Plan) { p.HasDuration = false }).outputBudget(); got != 18_000 {
		t.Errorf("source term = %d", got)
	}
	// 2×source saturates rather than wrapping negative on an absurd source.
	if got := job(math.MaxInt64, 1, func(p *transcode.Plan) { p.HasDuration = false }).outputBudget(); got <= 0 {
		t.Errorf("saturation = %d", got)
	}
}

// An absurd DECLARED duration must not disable the two guards written to
// bound it (§20 erratum 48). `format.duration` is uploader-written metadata
// ffprobe reports verbatim, and past ~6.7e12 s at the plan's ceiling dims
// the byte estimate saturates at math.MaxInt64: the pre-encode reservation
// then wrapped NEGATIVE (nothing is ever less than a negative need, so the
// encode started with no room reserved), and the runtime cap was handed
// math.MaxInt64, which no counter can exceed. A moderate lie was refused
// while the extreme one sailed through — the guards inverted exactly where
// they mattered.
func TestSaturatingPlanDoesNotDisableTheOutputGuards(t *testing.T) {
	const minFree = 512 << 20
	newJob := func(durationUS int64, fps int, free int64) *jobRun {
		w := newWorker(Deps{Cfg: config.Config{MaxUploadBytes: 1 << 20, ScratchMinFree: minFree},
			FreeBytes: func(string) (int64, error) { return free, nil }})
		return &jobRun{w: w, clip: db.Clip{SourceSize: 113_000},
			plan: transcode.Plan{W: 1280, H: 720, FPS: transcode.Rational{Num: fps, Den: 1}, HasDuration: true, DurationUS: durationUS}}
	}
	const tenGiB = 10 << 30
	// 7e12 s of 1280x720@60 saturates the estimate.
	sat := newJob(7e12*1_000_000, 60, tenGiB)
	if got := plannedOutputBytes(sat.plan); got != math.MaxInt64 {
		t.Fatalf("premise: plannedOutputBytes = %d, want saturation", got)
	}
	if err := sat.checkOutputRoom(); err == nil {
		t.Errorf("a saturating plan was let through with %d bytes free", int64(tenGiB))
	}
	if got := sat.outputBudget(); got != 1<<20 {
		t.Errorf("budget = %d, want %d (max of 2×source and MAX_UPLOAD_BYTES): a saturated estimate grants nothing", got, 1<<20)
	}
	// Control: a big-but-representable lie is refused the same way, and its
	// estimate IS a grant.
	big := newJob(35_990*1_000_000, 60, tenGiB)
	if err := big.checkOutputRoom(); err == nil {
		t.Errorf("35990 s of 720p60 fits in 10 GiB?")
	}
	if got, want := big.outputBudget(), plannedOutputBytes(big.plan); got != want || want <= 0 {
		t.Errorf("budget = %d, want the planned %d", got, want)
	}
	// Control: an honest plan still passes and is still budgeted by its plan.
	ok := newJob(10*1_000_000, 60, tenGiB)
	if err := ok.checkOutputRoom(); err != nil {
		t.Errorf("10 s of 720p60 refused: %v", err)
	}
	if got := ok.outputBudget(); got != plannedOutputBytes(ok.plan) {
		t.Errorf("budget = %d, want the planned %d", got, plannedOutputBytes(ok.plan))
	}
	// A filesystem that reports no free space still cannot arm an infinite
	// cap: the reservation is skipped, the budget stays finite.
	if got := newJob(7e12*1_000_000, 60, -1).outputBudget(); got == math.MaxInt64 {
		t.Errorf("budget = %d with unknown free space", got)
	}
}

func TestSatAdd(t *testing.T) {
	for _, c := range []struct{ a, b, want int64 }{
		{1, 2, 3}, {0, 0, 0}, {-5, 2, -3},
		{math.MaxInt64, 1, math.MaxInt64}, {math.MaxInt64, math.MaxInt64, math.MaxInt64},
		{math.MaxInt64 - 1, 1, math.MaxInt64}, {math.MaxInt64, 0, math.MaxInt64},
		{math.MinInt64, -1, math.MinInt64}, {math.MinInt64, math.MinInt64, math.MinInt64},
		{math.MaxInt64, math.MinInt64, -1},
	} {
		if got := satAdd(c.a, c.b); got != c.want {
			t.Errorf("satAdd(%d, %d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestDownloadErrorIsTransient: a missing original is retried as any store
// error (a lagging bucket looks the same) but, being row/object drift the
// design says cannot happen, it is also an ERROR line (§12).
func TestDownloadErrorIsTransient(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 100, false) // not seeded → Download fails with ErrNotFound
	h.start()
	job := h.waitJob(c.ID, func(j db.Job) bool { return j.State == db.JobQueued && j.TransientFailures == 1 })
	if !strings.Contains(str(job.LastError), "acquire: download source") || job.Attempts != 0 {
		t.Fatalf("job = %+v", job)
	}
	h.assertLog("ERROR", "drift")
	h.assertLog("WARN", "requeued")

	// Any other download error is WARN only.
	h2 := newHarness(t)
	c2, _ := h2.newClip("mp4", 100, true)
	h2.store.FailNext("Download", errors.New("s3: connection reset"))
	h2.start()
	h2.waitJob(c2.ID, func(j db.Job) bool { return j.State == db.JobQueued && j.TransientFailures == 1 })
	h2.assertNoErrorLogs()
}

func TestCheckpointFinalizeOnly(t *testing.T) {
	h := newHarness(t)
	c, j := h.newClip("mp4", 100, true)
	rk, pk := store.RenditionKey("", c.ID, "AAAAAAAA"), store.PosterKey("", c.ID, "AAAAAAAA")
	h.store.Put(rk, []byte("r"), "video/mp4")
	h.store.Put(pk, []byte("p"), "image/jpeg")
	cp := checkpoint{RenditionKey: rk, RenditionSize: 1, RenditionETag: "re", PosterKey: pk, PosterSize: 1, PosterETag: "pe",
		PosterWidth: 320, PosterHeight: 180, Width: 640, Height: 360, DurationMS: 4000, FPSNum: 30000, FPSDen: 1001, HasAudio: false}
	b, _ := json.Marshal(cp)
	// The shape ReleaseJob leaves behind: a claim that checkpointed its outputs
	// and was released with the checkpoint kept. SaveCheckpoint is pinned to
	// the claim (§20 erratum 291), so the checkpoint is written by a RUNNING
	// attempt and the queued row inherits it.
	cj, _, ok, err := h.db.ClaimJob(context.Background(), h.clock.Now().Unix())
	if err != nil || !ok || cj.ID != j.ID {
		t.Fatalf("ClaimJob: ok=%v err=%v", ok, err)
	}
	if err := h.db.SaveCheckpoint(context.Background(), cj.ID, *cj.StartedAt, string(b), h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := h.db.ReleaseJob(context.Background(), cj.ID, db.ClaimStamp(cj), c.ID, h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	h.start()
	got := h.waitClip(c.ID, db.ClipReady)
	if str(got.RenditionKey) != rk || str(got.PosterKey) != pk || *got.Width != 640 || *got.FPSNum != 30000 || *got.FPSDen != 1001 || *got.HasAudio || *got.PosterWidth != 320 {
		t.Fatalf("finalize-only fields: %+v", got)
	}
	if rc := h.runner.Calls(); len(rc) != 0 {
		t.Fatalf("runner must not run for a checkpointed job: %v", rc)
	}
	if calls := h.store.CallsSnapshot(); len(calls) != 0 {
		t.Fatalf("no storage traffic expected: %v", calls)
	}
	h.assertLog("INFO", "finalize only")
	h.assertNoErrorLogs()
}

// blockingEncode returns an encodeFn that signals started, blocks until
// ctx is done, then simulates reaping for reap before returning.
func blockingEncode(started chan<- struct{}, exited chan<- time.Time, reap time.Duration) func(context.Context, transcode.Plan, string, string, func(float64, bool)) error {
	var once sync.Once
	return func(ctx context.Context, _ transcode.Plan, _, _ string, on func(float64, bool)) error {
		on(0.25, false)
		once.Do(func() { close(started) })
		<-ctx.Done()
		time.Sleep(reap) // sleep: a slow reap IS the subject (Cancel must wait it out)
		exited <- time.Now()
		return ctx.Err()
	}
}

func TestCancelBlocksUntilRunExits(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 100, true)
	started := make(chan struct{})
	exited := make(chan time.Time, 1)
	h.runner.encodeFn = blockingEncode(started, exited, 200*time.Millisecond)
	h.start()
	<-started
	if p, ok := h.w.Progress(c.ID); !ok || p.Stage != stageEncode || p.State != db.ClipProcessing {
		t.Fatalf("progress mid-encode = %+v", p)
	}
	// lifecycle.Destroy order: MarkDeleting → Cancel.
	if err := h.db.MarkDeleting(context.Background(), c.ID, h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	h.w.Cancel(context.Background(), c.ID)
	t1 := time.Now()
	select {
	case ex := <-exited:
		if ex.After(t1) {
			t.Fatal("Cancel returned before the run exited")
		}
	default:
		t.Fatal("Cancel returned but the encode goroutine has not exited")
	}
	if t1.Sub(t0) < 200*time.Millisecond {
		t.Fatalf("Cancel returned after %s; must block for the reap", t1.Sub(t0))
	}
	if h.w.active() != 0 {
		t.Fatal("run still registered after Cancel")
	}
	if _, ok := h.w.Progress(c.ID); ok {
		t.Fatal("Cancel must evict the progress entry")
	}
	job, _ := h.db.GetLatestJob(context.Background(), c.ID)
	if job.State != db.JobCancelled || job.Attempts != 0 || job.TransientFailures != 0 {
		t.Fatalf("job = %+v", job)
	}
	if d := h.store.Deletes(); len(d) != 0 {
		t.Fatalf("cancelled run uploaded nothing, so it deletes nothing: %v", d)
	}
	// MarkDeleting cancelled the row, so the release finds a row that is no
	// longer this attempt's and takes the lost-attempt ending (§20 erratum 293).
	h.assertLog("INFO", "job row is no longer this attempt's")
	h.assertNoErrorLogs()
	// A second Cancel and an unknown id return at once.
	done := make(chan struct{})
	go func() { h.w.Cancel(context.Background(), c.ID); h.w.Cancel(context.Background(), "nope"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Cancel without a run blocked")
	}
}

func TestCancelWithoutTombstoneReleases(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 100, true)
	started := make(chan struct{})
	exited := make(chan time.Time, 1)
	h.runner.encodeFn = blockingEncode(started, exited, 0)
	h.start()
	<-started
	// A released job is claimable again at once; freeze claiming so the
	// assertions below see the released state, not the next attempt.
	h.w.mu.Lock()
	h.w.stopping = true
	h.w.mu.Unlock()
	h.w.Cancel(context.Background(), c.ID)
	job, _ := h.db.GetLatestJob(context.Background(), c.ID)
	clip, _ := h.db.GetClip(context.Background(), c.ID)
	if job.State != db.JobQueued || job.Attempts != 0 || job.TransientFailures != 0 || clip.State != db.ClipQueued {
		t.Fatalf("job = %+v clip = %s", job, clip.State)
	}
	h.assertLog("INFO", "reason=cancelled")
	h.assertNoErrorLogs()
}

func TestLostRace(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 100, true)
	// The sweeper wins after registration: MarkDeleting lands mid-run
	// without a Cancel (as when Destroy looked before the run registered).
	h.runner.posterFn = func(_ context.Context, _, out string, _ int64, _ bool) (transcode.PosterResult, error) {
		if err := os.WriteFile(out, []byte("poster-bytes"), 0o644); err != nil {
			return transcode.PosterResult{}, err
		}
		if err := h.db.MarkDeleting(context.Background(), c.ID, h.clock.Now().Unix()); err != nil {
			return transcode.PosterResult{}, err
		}
		return transcode.PosterResult{Width: 640, Height: 360}, nil
	}
	h.start()
	h.waitJob(c.ID, func(j db.Job) bool { return j.State == db.JobCancelled })
	h.waitIdle()
	d := h.store.Deletes()
	if len(d) != 3 || !renditionRE.MatchString(d[0]) || !posterRE.MatchString(d[1]) || d[2] != store.ProbeKey("", c.ID) {
		t.Fatalf("lost race must delete exactly its three keys: %v", d)
	}
	if keys := h.store.Keys(); len(keys) != 1 || keys[0] != c.SourceKey {
		t.Fatalf("bucket after lost race = %v", keys)
	}
	clip, _ := h.db.GetClip(context.Background(), c.ID)
	if clip.State != db.ClipDeleting || clip.RenditionKey != nil {
		t.Fatalf("clip = %s rendition=%v", clip.State, clip.RenditionKey)
	}
	if _, ok := h.w.Progress(c.ID); ok {
		t.Fatal("progress entry must be evicted after a lost race")
	}
	h.assertLog("INFO", "lost_race")
	h.assertNoErrorLogs()
	select {
	case rc := <-h.ready:
		t.Fatalf("OnReady must not fire after a lost race: %+v", rc)
	default:
	}
}

func TestStopReleasesWithoutCounting(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 100, true)
	entered := make(chan struct{})
	var once sync.Once
	h.store.Hook = func(ctx context.Context, op, key string) error {
		if op == "PutFile" && strings.HasSuffix(key, "meta/probe.json") {
			once.Do(func() { close(entered) })
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	h.start()
	<-entered
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	if err := h.w.Stop(sctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	job, _ := h.db.GetLatestJob(context.Background(), c.ID)
	clip, _ := h.db.GetClip(context.Background(), c.ID)
	if job.State != db.JobQueued || job.Stage != db.JobStageQueued || job.Attempts != 0 || job.TransientFailures != 0 || job.CheckpointJSON != nil {
		t.Fatalf("job = %+v", job)
	}
	if clip.State != db.ClipQueued {
		t.Fatalf("clip = %s", clip.State)
	}
	d := h.store.Deletes()
	if len(d) != 2 || !renditionRE.MatchString(d[0]) || !posterRE.MatchString(d[1]) {
		t.Fatalf("shutdown must delete this attempt's uploads (no checkpoint): %v", d)
	}
	// … and the key of the ABORTED upload (an incomplete multipart on a
	// real bucket) is cleaned up as well.
	if calls := h.store.CallsSnapshot(); !slices.Contains(calls, "DeleteKey "+store.ProbeKey("", c.ID)) {
		t.Fatalf("the aborted probe.json upload's key must be cleaned up: %v", calls)
	}
	h.assertOriginalIntact(clip)
	if _, err := os.Stat(filepath.Join(h.cfg.ScratchDir, c.ID, "source.mp4")); err != nil {
		t.Fatalf("source must survive a release: %v", err)
	}
	h.assertLog("INFO", "reason=shutdown")
	h.assertNoErrorLogs()
	// Stop is idempotent.
	if err := h.w.Stop(sctx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// TestReleaseWithCheckpointDeletesNothing drives the exit path directly: a
// checkpointed run interrupted by shutdown releases the job and never
// touches its outputs (§11 stage-aware cleanup).
func TestReleaseWithCheckpointDeletesNothing(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 100, true)
	job, clip, ok, err := h.db.ClaimJob(context.Background(), h.clock.Now().Unix())
	if err != nil || !ok || clip.ID != c.ID {
		t.Fatalf("ClaimJob: %v %v", ok, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &run{jobID: job.ID, clipID: c.ID, cancel: cancel, done: make(chan struct{})}
	j := newJobRun(h.w, ctx, r, job, clip)
	j.checkpointed = true
	j.attempted = []string{j.renditionKey, j.posterKey, j.probeKey}
	j.stage = stageFinalize
	r.setReason(reasonShutdown)
	cancel()
	j.finish(context.Canceled)
	if d := h.store.Deletes(); len(d) != 0 {
		t.Fatalf("checkpointed release deleted %v", d)
	}
	got, _ := h.db.GetLatestJob(context.Background(), c.ID)
	if got.State != db.JobQueued || got.Attempts != 0 || got.TransientFailures != 0 {
		t.Fatalf("job = %+v", got)
	}
	h.assertLog("INFO", "checkpointed=true")
	h.assertNoErrorLogs()

	// The same exit with reason=cancelled deletes nothing either even
	// without a checkpoint (lifecycle deletes the whole prefix next).
	job2, clip2, _, _ := h.db.ClaimJob(context.Background(), h.clock.Now().Unix())
	if job2.ID != job.ID {
		t.Fatalf("re-claim got job %d, want %d", job2.ID, job.ID)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	r2 := &run{jobID: job2.ID, clipID: c.ID, cancel: cancel2, done: make(chan struct{})}
	j2 := newJobRun(h.w, ctx2, r2, job2, clip2)
	j2.attempted = []string{j2.renditionKey}
	r2.setReason(reasonCancelled)
	cancel2()
	j2.finish(context.Canceled)
	if d := h.store.Deletes(); len(d) != 0 {
		t.Fatalf("cancelled release deleted %v", d)
	}
}

func TestPanicIsCrashClass(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 100, true)
	var calls int
	var mu sync.Mutex
	h.runner.encodeFn = func(_ context.Context, _ transcode.Plan, _, out string, on func(float64, bool)) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			panic("kaboom in the encoder")
		}
		if err := os.WriteFile(out, []byte(renditionPayload), 0o644); err != nil {
			return err
		}
		on(1, false)
		return nil
	}
	h.start()
	job := h.waitJob(c.ID, func(j db.Job) bool { return j.State == db.JobQueued && j.Attempts == 1 })
	now := h.clock.Now().Unix()
	if job.TransientFailures != 0 || job.RunAfter != now+30 || !strings.Contains(str(job.LastError), "panic: kaboom") {
		t.Fatalf("job = %+v", job)
	}
	h.waitIdle()
	clip, _ := h.db.GetClip(context.Background(), c.ID)
	if clip.State != db.ClipQueued {
		t.Fatalf("clip = %s", clip.State)
	}
	h.assertLog("ERROR", "job panicked")
	h.assertOriginalIntact(clip)
	// The loop survived the panic: after the crash backoff the retry succeeds.
	h.clock.Advance(31 * time.Second)
	h.w.Wake()
	h.waitClip(c.ID, db.ClipReady)
}

// TestPanicDuringShutdownIsLoggedAndReleased: a panic that coincides with
// Stop is never silent (ERROR with the stack) and releases the job rather
// than counting an attempt — the cancellation ended the attempt.
func TestPanicDuringShutdownIsLoggedAndReleased(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 100, true)
	started := make(chan struct{})
	h.runner.encodeFn = func(ctx context.Context, _ transcode.Plan, _, _ string, _ func(float64, bool)) error {
		close(started)
		<-ctx.Done()
		panic("encoder blew up on cancel")
	}
	h.start()
	<-started
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	if err := h.w.Stop(sctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	h.assertLog("ERROR", "job panicked")
	h.assertLog("INFO", "reason=shutdown")
	job, _ := h.db.GetLatestJob(context.Background(), c.ID)
	clip, _ := h.db.GetClip(context.Background(), c.ID)
	if job.State != db.JobQueued || job.Attempts != 0 || job.TransientFailures != 0 || clip.State != db.ClipQueued {
		t.Fatalf("a panic under shutdown must release, not count: job=%+v clip=%s", job, clip.State)
	}
}

// TestExitPathPanicIsContained: a panic on the exit path itself (finish →
// FailAttempt on a worker built without a DB, after the pipeline's own
// panic) is logged at ERROR and contained inside execute; the run reports
// not-ready instead of taking the process down.
func TestExitPathPanicIsContained(t *testing.T) {
	logs := &logSink{}
	w := New(Deps{Log: slog.New(slog.NewTextHandler(logs, nil)), Runner: &fakeRunner{}}).(*worker)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clip := db.Clip{ID: "aZ3kQ9xB2mLp", SourceKey: "clips/aZ3kQ9xB2mLp/original/source.mp4"}
	r := &run{jobID: 1, clipID: clip.ID, cancel: cancel, done: make(chan struct{})}
	ready := newJobRun(w, ctx, r, db.Job{ID: 1, ClipID: clip.ID, Kind: db.KindTranscode}, clip).execute()
	if ready {
		t.Fatal("ready after a panic")
	}
	if got := logs.String(); !strings.Contains(got, "job panicked") || !strings.Contains(got, "exit path panicked") {
		t.Fatalf("logs = %s", got)
	}
}

// TestHookPanicIsContained: a panicking OnReady hook is logged and the loop
// survives to run the next job.
func TestHookPanicIsContained(t *testing.T) {
	h := newHarness(t)
	c1, _ := h.newClip("mp4", 100, true)
	c2, _ := h.newClip("mp4", 100, true)
	h.w.deps.Hooks.OnReady = func(_ context.Context, c db.Clip) {
		if c.ID == c1.ID {
			panic("webhook exploded")
		}
		h.ready <- c
	}
	h.start()
	h.waitClip(c1.ID, db.ClipReady)
	h.waitClip(c2.ID, db.ClipReady)
	h.assertLog("ERROR", "OnReady hook panicked")
	select {
	case rc := <-h.ready:
		if rc.ID != c2.ID {
			t.Fatalf("OnReady got %s", rc.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not survive the hook panic")
	}
}

// TestHookRunsOutsideTheRun: Hooks.OnReady fires after the run has left the
// registry, so a slow hook holds neither Cancel nor Stop, and Stop aborts it.
func TestHookRunsOutsideTheRun(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 100, true)
	entered := make(chan struct{})
	hookErr := make(chan error, 1)
	h.w.deps.Hooks.OnReady = func(ctx context.Context, got db.Clip) {
		if got.ID != c.ID || got.State != db.ClipReady {
			t.Errorf("OnReady got %+v", got)
		}
		close(entered)
		<-ctx.Done()
		hookErr <- ctx.Err()
	}
	h.start()
	<-entered
	if h.w.active() != 0 {
		t.Fatal("the run must have left the registry before the hook runs")
	}
	done := make(chan struct{})
	go func() { h.w.Cancel(context.Background(), c.ID); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Cancel blocked on the OnReady hook")
	}
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	t0 := time.Now()
	if err := h.w.Stop(sctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if el := time.Since(t0); el > 2*time.Second {
		t.Fatalf("Stop waited %s on the hook", el)
	}
	select {
	case err := <-hookErr:
		if err == nil {
			t.Fatal("hook ctx must be cancelled by Stop")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not abort the hook")
	}
	h.assertNoErrorLogs()
}

// TestUnsupportedJobKindIsRefused: only transcode jobs run; any other kind
// is failed permanently with an ERROR line and never reaches the runner.
func TestUnsupportedJobKindIsRefused(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 100, true)
	job, clip, ok, err := h.db.ClaimJob(context.Background(), h.clock.Now().Unix())
	if err != nil || !ok {
		t.Fatalf("ClaimJob: %v", err)
	}
	job.Kind = "from-the-future" // a kind this worker has never been taught
	h.w.runJob(context.Background(), job, clip)
	got, _ := h.db.GetLatestJob(context.Background(), c.ID)
	if got.State != db.JobFailed || !strings.Contains(str(got.LastError), "unsupported job kind: from-the-future") {
		t.Fatalf("job = %+v", got)
	}
	if rc := h.runner.Calls(); len(rc) != 0 {
		t.Fatalf("runner must not run for an unknown kind: %v", rc)
	}
	if h.w.active() != 0 {
		t.Fatal("a refused job must not be registered")
	}
	h.assertLog("ERROR", "unsupported job kind")
}

// TestRunnerAuxTimeoutIsPermanent: ffprobe (source and output) and the
// poster ffmpeg run under a budget; a binary that never returns is a
// permanent *_timeout (the file is at fault), while a caller's cancellation
// is still returned as ctx.Err().
func TestRunnerAuxTimeoutIsPermanent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary needs /bin/sh")
	}
	bin := filepath.Join(t.TempDir(), "hang")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := ffmpegRunner{ffmpeg: bin, ffprobe: bin, auxTimeout: 200 * time.Millisecond}
	ctx := context.Background()
	start := time.Now()
	if _, err := r.Probe(ctx, "src.mp4"); !transcode.IsPermanent(err) || !strings.Contains(err.Error(), "probe_timeout") {
		t.Fatalf("Probe = %v", err)
	}
	if _, err := r.ProbeOutput(ctx, "out.mp4"); !transcode.IsPermanent(err) || !strings.Contains(err.Error(), "probe_timeout") {
		t.Fatalf("ProbeOutput = %v", err)
	}
	if _, err := r.Poster(ctx, "out.mp4", filepath.Join(t.TempDir(), "p.jpg"), 0, false, 0, transcode.PosterBox{}); !transcode.IsPermanent(err) || !strings.Contains(err.Error(), "poster_timeout") {
		t.Fatalf("Poster = %v", err)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("hung binaries not reaped promptly: %s", el)
	}
	cctx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(50 * time.Millisecond) // sleep: cancels a hung probe mid-flight; load only delays the cancel
		cancel()
	}()
	if _, err := r.Probe(cctx, "src.mp4"); !errors.Is(err, context.Canceled) || transcode.IsPermanent(err) {
		t.Fatalf("cancelled Probe = %v, want context.Canceled", err)
	}
	// NewFFmpegRunner carries the production budget.
	if got := NewFFmpegRunner(config.TranscodeConfig{}, config.LightTagConfig{}).(ffmpegRunner).auxTimeout; got != auxTimeout {
		t.Fatalf("auxTimeout = %s", got)
	}
}

func TestCrashCapFailsTheClip(t *testing.T) {
	h := newHarness(t)
	h.cfg.Transcode.MaxAttempts = 1
	h.w.lim = db.Limits{MaxAttempts: 1, MaxTransient: 10}
	c, _ := h.newClip("mp4", 100, true)
	h.runner.encodeFn = func(context.Context, transcode.Plan, string, string, func(float64, bool)) error {
		panic("always")
	}
	h.start()
	got := h.waitClip(c.ID, db.ClipFailed)
	if !strings.Contains(str(got.FailureReason), "panic: always") {
		t.Fatalf("failure_reason = %s", str(got.FailureReason))
	}
	h.waitIdle() // the failed row is committed before the run's deferred scratch cleanup
	if _, err := os.Stat(filepath.Join(h.cfg.ScratchDir, c.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("scratch dir must be removed when the clip fails")
	}
	h.assertLog("WARN", "retry cap reached")
}

func TestClaimSkipsDeletingAndProgressBands(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 100, true)
	job, clip, ok, err := h.db.ClaimJob(context.Background(), h.clock.Now().Unix())
	if err != nil || !ok {
		t.Fatalf("ClaimJob: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	j := newJobRun(h.w, ctx, &run{jobID: job.ID, clipID: c.ID, cancel: cancel, done: make(chan struct{})}, job, clip)
	if j.ext != "mp4" || filepath.Base(j.srcPath) != "source.mp4" || !renditionRE.MatchString(j.renditionKey) || !posterRE.MatchString(j.posterKey) {
		t.Fatalf("jobRun paths/keys: ext=%s src=%s r=%s p=%s", j.ext, j.srcPath, j.renditionKey, j.posterKey)
	}
	get := func() Progress { p, _ := h.w.Progress(c.ID); return p }

	// The plan is unsized here (probeSource has not run), so the encode floor
	// must be published INDETERMINATE straight away rather than at the first
	// -progress block (§10.3): a poller that catches the window between the
	// stage write and ffmpeg's first line must not see a determinate 5 %.
	j.setStage(stageEncode)
	if p := get(); p.Percent != 5 || p.Stage != stageEncode || p.State != db.ClipProcessing || !p.Indeterminate {
		t.Fatalf("encode floor: %+v", p)
	}
	h.clock.Advance(time.Second)
	j.progress(0.5, false)
	if p := get(); p.Percent != 45 {
		t.Fatalf("encode 50%%: %+v", p)
	}
	h.clock.Advance(time.Second)
	j.progress(0.25, false) // never decreases
	if p := get(); p.Percent != 45 {
		t.Fatalf("monotone: %+v", p)
	}
	h.clock.Advance(time.Second)
	j.progress(0.9, true) // indeterminate holds the floor, never regresses
	if p := get(); p.Percent != 45 || !p.Indeterminate {
		t.Fatalf("indeterminate: %+v", p)
	}
	j.setStage(stageVerify)
	if p := get(); p.Percent != 85 {
		t.Fatalf("verify floor: %+v", p)
	}
	h.clock.Advance(time.Second)
	j.progress(5, false) // clamp
	if p := get(); p.Percent != 87 {
		t.Fatalf("clamp: %+v", p)
	}
	j.setStage(stageStore)
	h.clock.Advance(time.Second)
	j.progress(1, false)
	if p := get(); p.Percent != 98 {
		t.Fatalf("store end: %+v", p)
	}
	j.setStage(stageFinalize)
	if p := get(); p.Percent != 98 {
		t.Fatalf("finalize floor: %+v", p)
	}
	j.progressTerminal(db.ClipReady, "")
	if p := get(); p.Percent != 100 || p.State != db.ClipReady || p.Stage != stageDone {
		t.Fatalf("ready: %+v", p)
	}
	// The DB saw the stage transitions (≤ 8 writes per job, none per frame).
	got, _ := h.db.GetLatestJob(context.Background(), c.ID)
	if got.Stage != stageFinalize {
		t.Fatalf("job stage = %s", got.Stage)
	}
	if err := h.db.ReleaseJob(context.Background(), job.ID, db.ClaimStamp(job), c.ID, h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	// A tombstoned clip is never claimed.
	if err := h.db.MarkDeleting(context.Background(), c.ID, h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := h.db.ClaimJob(context.Background(), h.clock.Now().Unix()); ok {
		t.Fatal("ClaimJob must skip deleting clips")
	}
}

func TestStartWithoutDepsDoesNotRun(t *testing.T) {
	logs := &logSink{}
	w := New(Deps{Log: slog.New(slog.NewTextHandler(logs, nil))})
	w.Start(context.Background())
	if err := w.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "not started") {
		t.Fatalf("logs = %s", logs.String())
	}
}

func TestReasonAndKindNames(t *testing.T) {
	h := newHarness(t)
	j := &jobRun{w: h.w, stage: stageEncode}
	if r := j.reason(&transcode.ErrPermanent{Err: errors.New("encode_stalled: x")}); r != "encode: encode_stalled: x" {
		t.Fatalf("reason = %q", r)
	}
	if r := j.reason(errors.New(strings.Repeat("y", 2000))); len(r) != maxReasonLen {
		t.Fatalf("reason length = %d", len(r))
	}
	// Scratch paths are stripped before the message becomes the public
	// failure_reason (§8.2 errata 20): ffprobe/ffmpeg stderr echoes the input
	// path, which no clip-id holder needs.
	j.dir = "/scratch/aZ3kQ9xB2mLp"
	got := j.reason(&transcode.ErrPermanent{Err: errors.New("ffprobe: /scratch/aZ3kQ9xB2mLp/source.mp4: Invalid data found")})
	if strings.Contains(got, "/scratch/") {
		t.Fatalf("reason leaks a scratch path: %q", got)
	}
	if got != "encode: ffprobe: …/source.mp4: Invalid data found" {
		t.Fatalf("reason redaction = %q", got)
	}
	for k, want := range map[db.FailKind]string{db.FailPermanent: "permanent", db.FailCrash: "crash", db.FailTransient: "transient"} {
		if kindName(k) != want {
			t.Fatalf("kindName(%v) = %s", k, kindName(k))
		}
	}
	if reasonCancelled.String() != "cancelled" || reasonShutdown.String() != "shutdown" || reasonNone.String() != "none" {
		t.Fatal("reason names")
	}
}

// TestSupersededRunNeverRacesItsSuccessor: fail() commits clip=failed (so
// POST /retry is accepted and another loop may re-claim the job) BEFORE its
// exit path deletes this attempt's objects, removes $SCRATCH/{id} and writes
// the terminal progress entry — all keyed by the CLIP id, not the attempt.
// With TRANSCODE_CONCURRENCY ≥ 2 the retry's run used to start under that
// cleanup: its scratch dir wiped mid-encode and its progress overwritten
// with the stale `failed`. A successor now waits for its predecessor to
// leave the registry (§20 erratum 32).
func TestSupersededRunNeverRacesItsSuccessor(t *testing.T) {
	h := newHarness(t)
	h.w.deps.Cfg.Transcode.Concurrency = 2
	h.w.lim = db.Limits{MaxAttempts: 1, MaxTransient: 1} // one bad ending → failed
	c, _ := h.newClip("mp4", 100, true)

	var mu sync.Mutex
	posterFails := true
	deleting := make(chan struct{}, 1)
	release := make(chan struct{})
	h.store.Hook = func(ctx context.Context, op, key string) error {
		switch op {
		case "PutFile":
			mu.Lock()
			fail := posterFails && strings.Contains(key, "/poster/")
			posterFails = posterFails && !fail
			mu.Unlock()
			if fail {
				return errors.New("bucket refused the poster")
			}
		case "DeleteKey", "DeletePrefix":
			// The superseded run is now inside its post-commit exit path. Its
			// key deletes are DeleteKey since §20 erratum 305; DeletePrefix is
			// kept here so this checkpoint survives either spelling.
			select {
			case deleting <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return nil
	}
	started := make(chan string, 4)
	h.runner.probeFn = func(_ context.Context, path string) (*transcode.ProbeResult, error) {
		started <- path
		return defaultProbeResult(), nil
	}

	h.start()
	<-started // attempt 1
	h.waitClip(c.ID, db.ClipFailed)
	<-deleting

	// Exactly what POST /api/clips/{id}/retry does once the row says failed.
	if err := h.db.RetryClip(context.Background(), c.ID, h.clock.Now().Unix()); err != nil {
		t.Fatalf("RetryClip: %v", err)
	}
	h.w.Forget(c.ID)
	h.w.Wake()

	// The free loop claims the job immediately, but the run must not begin
	// while the superseded one is still deleting keys, about to RemoveAll
	// $SCRATCH/{id} and to force-write its terminal registry entry.
	select {
	case path := <-started:
		t.Fatalf("the retry ran (probed %s) while the superseded run was still exiting", path)
	case <-time.After(500 * time.Millisecond):
	}

	close(release) // the superseded run finishes: deletes, RemoveAll, `failed`
	select {
	case <-started: // attempt 2, on a scratch dir nobody else owns
	case <-time.After(10 * time.Second):
		t.Fatalf("the retry never started\nlogs:\n%s", h.logs.String())
	}
	h.waitClip(c.ID, db.ClipReady)
	h.waitIdle()
	if p, ok := h.w.Progress(c.ID); !ok || p.State != db.ClipReady || p.Error != "" {
		t.Fatalf("progress after the retry = %+v (ok=%v); the superseded run must not own the entry", p, ok)
	}
	if _, err := os.Stat(filepath.Join(h.cfg.ScratchDir, c.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch dir after a successful retry: %v", err)
	}
}

// A retry evicts the failed run's progress entry (§20 erratum 18) — and the
// run that failed is still ALIVE when it does: fail() commits FailAttempt,
// which is the instant the clip is publicly `failed` and retryable, and only
// then walks its exit path (bucket deletes over the network, a RemoveAll of
// a multi-GB scratch dir), ending with the forced terminal registry write.
// That write used to land after the eviction and resurrect the entry, so
// GET /progress answered `failed` with the PREVIOUS run's error for a clip
// the row said was queued — and §9.3's poller stops on `failed`, so the user
// saw the retry instantly re-fail with a stale message. Erratum 32 makes the
// window strictly wider on the successor's side: it cannot register, and so
// cannot overwrite the entry, until the predecessor has fully unregistered.
func TestRetryEvictionSurvivesTheFailedRunsExitPath(t *testing.T) {
	h := newHarness(t)
	h.w.lim = db.Limits{MaxAttempts: 1, MaxTransient: 1} // one bad ending → failed
	c, _ := h.newClip("mp4", 100, true)

	deleting := make(chan struct{}, 1)
	release := make(chan struct{})
	h.store.Hook = func(ctx context.Context, op, key string) error {
		switch op {
		case "PutFile":
			if strings.Contains(key, "/poster/") {
				return errors.New("bucket refused the poster")
			}
		case "DeleteKey", "DeletePrefix":
			// The failed run is now inside its post-commit exit path, with
			// its terminal registry write still to come (its key deletes are
			// DeleteKey since §20 erratum 305).
			select {
			case deleting <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return nil
	}

	h.start()
	h.waitClip(c.ID, db.ClipFailed)
	<-deleting

	// Exactly what POST /api/clips/{id}/retry does — except that run_after is
	// dated forward so the free loop cannot claim the requeued job, which
	// keeps the assertion about the failed run alone.
	if err := h.db.RetryClip(context.Background(), c.ID, h.clock.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatalf("RetryClip: %v", err)
	}
	h.w.Forget(c.ID)
	if p, ok := h.w.Progress(c.ID); ok {
		t.Fatalf("Forget left %+v behind", p)
	}

	close(release) // deletes, RemoveAll, then the terminal write
	h.waitIdle()

	if p, ok := h.w.Progress(c.ID); ok {
		t.Fatalf("the failed run resurrected its entry after the retry evicted it: %+v", p)
	}
	if got := h.waitClip(c.ID, db.ClipQueued); got.FailureReason != nil {
		t.Fatalf("row = %+v", got)
	}
}

// Forget with no run in flight is the plain eviction lifecycle step 5 relies
// on, and the void is per RUN, not per clip: the next run's writes land.
func TestForgetVoidsOnlyTheRunItInterrupts(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 100, true)
	p := Progress{State: db.ClipProcessing, Stage: stageStore, Percent: 90}

	h.w.prog.set(c.ID, p, true)
	h.w.Forget(c.ID) // no live run
	if got, ok := h.w.Progress(c.ID); ok {
		t.Fatalf("Forget with no run left %+v", got)
	}

	// A run registered AFTER the Forget is not voided by it.
	r := &run{jobID: 1, clipID: c.ID, done: make(chan struct{})}
	h.w.mu.Lock()
	h.w.runs[c.ID] = r
	h.w.mu.Unlock()
	j := &jobRun{w: h.w, run: r, clip: db.Clip{ID: c.ID}, stage: stageStore, percent: 90}
	j.setProgress(p, true)
	if got, ok := h.w.Progress(c.ID); !ok || got.State != db.ClipProcessing {
		t.Fatalf("a fresh run's write = %+v (ok=%v)", got, ok)
	}
	// …and once Forget voids it, nothing it says lands again.
	h.w.Forget(c.ID)
	j.progressTerminal(db.ClipFailed, "store: upload poster: bucket refused the poster")
	j.setProgress(p, true)
	if got, ok := h.w.Progress(c.ID); ok {
		t.Fatalf("a voided run still wrote %+v", got)
	}
}

// TestCancelCoversARunParkedBehindTheOneItCancels pins §20 erratum 317(b)
// with the interleaving forced, not slept for: a run of the clip is in
// flight, a SECOND run of the same clip has been claimed and is parked in
// withRun behind it, and Cancel arrives. Before 317 the parked run took the
// slot the instant the first one closed `done` — after Cancel had stopped
// looking — and ran on, uncancelled, while Cancel's caller removed the
// prefix and the scratch dir. Now it registers cancelled and Cancel returns
// only once it, too, has exited.
func TestCancelCoversARunParkedBehindTheOneItCancels(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 100, true)
	wait := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
	}

	// The run in flight, registered by hand: it exits only when the test
	// says so, the way withRun's own exit does — unregister, then done.
	prevCancelled := make(chan struct{})
	prev := &run{jobID: 1, clipID: c.ID, done: make(chan struct{}), cancel: sync.OnceFunc(func() { close(prevCancelled) })}
	h.w.mu.Lock()
	h.w.runs[c.ID] = prev
	h.w.mu.Unlock()

	parked := make(chan struct{})
	var parkOnce sync.Once
	h.w.parkedHook = func(string) { parkOnce.Do(func() { close(parked) }) }

	// The successor lives until something cancels its context.
	exited := make(chan struct{})
	succReturned := make(chan struct{})
	go func() {
		defer close(succReturned)
		h.w.withRun(context.Background(), db.Job{ID: 2, ClipID: c.ID}, db.Clip{ID: c.ID}, func(rctx context.Context, _ *run) bool {
			<-rctx.Done()
			close(exited)
			return false
		})
	}()
	wait(parked, "the successor to park behind the run in flight")
	t.Cleanup(func() {
		// Releases the successor if the pin failed (the reverted code leaves
		// it running), so the goroutine never outlives the test.
		h.w.Cancel(context.Background(), c.ID)
		<-succReturned
	})

	cancelled := make(chan bool, 1)
	go func() { cancelled <- h.w.Cancel(context.Background(), c.ID) }()
	wait(prevCancelled, "Cancel to cancel the run in flight")
	h.w.mu.Lock()
	delete(h.w.runs, c.ID)
	h.w.mu.Unlock()
	close(prev.done)

	select {
	case ok := <-cancelled:
		if !ok {
			t.Fatal("Cancel reported its ctx done; it has none")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Cancel never returned")
	}
	select {
	case <-exited:
	default:
		t.Fatal("Cancel returned while a run of the clip was live: the run parked behind the cancelled one registered uncancelled")
	}
	if h.w.Busy(c.ID) {
		t.Fatal("Busy is true after Cancel returned")
	}
}

// TestEncodeFloorIsIndeterminateWithoutADuration is the other half of the
// band test: WHICH stage floor carries `indeterminate`, and on what evidence.
// §10.3 makes the encode indeterminate for a source with no duration, and it
// is indeterminate from the stage write rather than from ffmpeg's first
// -progress block -- the plan is already known, probeSource having run one
// step earlier, so publishing the zero value first would show a determinate
// 5 % that the next update takes away. No other stage is ever indeterminate:
// they are fixed floors.
func TestEncodeFloorIsIndeterminateWithoutADuration(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 100, true)
	job, clip, ok, err := h.db.ClaimJob(context.Background(), h.clock.Now().Unix())
	if err != nil || !ok {
		t.Fatalf("ClaimJob: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	get := func() Progress { p, _ := h.w.Progress(c.ID); return p }

	for _, tc := range []struct {
		name        string
		hasDuration bool
		stage       string
		want        bool
	}{
		{"unsized encode", false, stageEncode, true},
		{"sized encode", true, stageEncode, false},
		{"unsized verify", false, stageVerify, false},
		{"unsized store", false, stageStore, false},
		{"unsized finalize", false, stageFinalize, false},
	} {
		j := newJobRun(h.w, ctx, &run{jobID: job.ID, clipID: c.ID, cancel: cancel, done: make(chan struct{})}, job, clip)
		j.plan.HasDuration = tc.hasDuration
		if tc.hasDuration {
			j.plan.DurationUS = 5_000_000
		}
		h.clock.Advance(time.Second)
		j.setStage(tc.stage)
		if p := get(); p.Indeterminate != tc.want || p.Stage != tc.stage {
			t.Errorf("%s: indeterminate = %v, want %v (%+v)", tc.name, p.Indeterminate, tc.want, p)
		}
	}
}

// TestHWFallbackWarnNamesTheClip pins the WIRING of §20 erratum 265's one WARN.
//
// The fallback itself is discovered in internal/transcode, four layers down,
// which knows the ffmpeg line and nothing about clips; the sink that lets it say
// so is installed on every run's context by withRun. Nothing else would notice
// if that line were dropped in a refactor — the encode would still succeed and
// the operator would simply never learn that the card had stopped decoding.
func TestHWFallbackWarnNamesTheClip(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 100, true)
	const detail = "[h264 @ 0x7f10] Failed setup for format cuda: hwaccel initialisation returned error."
	h.runner.encodeFn = func(ctx context.Context, _ transcode.Plan, _, out string, onProgress func(float64, bool)) error {
		transcode.ReportHWFallback(ctx, "encode", detail)
		if err := os.WriteFile(out, []byte(renditionPayload), 0o644); err != nil {
			return err
		}
		onProgress(1, false)
		return nil
	}
	h.start()
	h.waitClip(c.ID, db.ClipReady)
	logs := h.logs.String()
	for _, want := range []string{"level=WARN", "the hardware decoder refused this source", c.ID, detail, "stage=encode"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("the fallback WARN does not carry %q:\n%s", want, logs)
		}
	}
	h.assertNoErrorLogs()
}

// TestCancelReturnsFalseWhenTheContextEndsFirst (AUDIT #13): the wait for the
// run's goroutine is bounded by the caller's context, and a Cancel that gives
// up says so instead of returning as though the run were gone.
//
// The run here takes two seconds to finish exiting — which is what a hung
// scratch RemoveAll looks like from outside — and the caller allows 150 ms.
// Before this, Cancel waited on `done` alone: lifecycle.Destroy, the sweeper
// tick that called it and a DELETE's own goroutine all waited with it, for as
// long as the filesystem took. A false answer is not "the run is gone", so the
// progress entry stays too: a viewer polling the clip still sees the run.
func TestCancelReturnsFalseWhenTheContextEndsFirst(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 100, true)
	started := make(chan struct{})
	exited := make(chan time.Time, 1)
	h.runner.encodeFn = blockingEncode(started, exited, 2*time.Second)
	h.start()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	got := make(chan bool, 1)
	t0 := time.Now()
	go func() { got <- h.w.Cancel(ctx, c.ID) }()
	select {
	case ok := <-got:
		if ok {
			t.Fatal("Cancel reported the run as reaped, but the run had not exited")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel ignored the caller's context and waited for the run")
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("Cancel returned after %s; the context allowed 150ms", d)
	}
	// The run is still this process's: neither the registry nor the progress
	// entry may be cleared while a goroutine can still write.
	if h.w.active() == 0 {
		t.Fatal("a Cancel that gave up deregistered the run anyway")
	}
	if _, ok := h.w.Progress(c.ID); !ok {
		t.Fatal("a Cancel that gave up evicted the progress entry")
	}

	// …and the same Cancel with room to wait reaps it, exactly as before.
	if ok := h.w.Cancel(context.Background(), c.ID); !ok {
		t.Fatal("Cancel with an unbounded context did not reap the run")
	}
	select {
	case <-exited:
	default:
		t.Fatal("Cancel returned true but the encode goroutine has not exited")
	}
	if h.w.active() != 0 {
		t.Fatal("run still registered after a completed Cancel")
	}
	if _, ok := h.w.Progress(c.ID); ok {
		t.Fatal("a completed Cancel must evict the progress entry")
	}
}

// Busy answers for a REGISTERED run only; a run parked behind a predecessor
// is not counted (see Busy's doc: its one caller, the scratch janitor, has
// nothing of a parked run's to protect, and the job table's half of its guard
// already sees the claimed job). Cancel, which must stop that run before a
// clip is destroyed, DOES count it — the contrast is the design, so both are
// pinned here on the same state.
func TestBusyDoesNotCountAParkedRun(t *testing.T) {
	w := newWorker(Deps{})
	const clip = "parkedclip01"
	w.mu.Lock()
	w.waiting[clip] = 1 // claimed, parked in withRun, not yet in `runs`
	w.mu.Unlock()
	if w.Busy(clip) {
		t.Fatal("Busy counted a parked run; it answers for a registered run only")
	}
	// Cancel waits for the parked run to register (cancelled) before it
	// returns true; with a ctx that is already over it must say false.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if w.Cancel(ctx, clip) {
		t.Fatal("Cancel returned true while a parked run of the clip was still to register")
	}
	// Control: a registered run is Busy.
	w.mu.Lock()
	delete(w.waiting, clip)
	w.runs[clip] = &run{clipID: clip, cancel: func() {}, done: make(chan struct{})}
	w.mu.Unlock()
	if !w.Busy(clip) {
		t.Fatal("Busy did not count a registered run")
	}
}
