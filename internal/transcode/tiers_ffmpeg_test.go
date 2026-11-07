//go:build ffmpeg

package transcode

// The quality tiers on real ffmpeg (§20 errata 200–204): the playability verdict
// against files this build actually produces, the `-c copy` remux, the Full at
// the source's own size, and AV1 when the operator asks for it.
//
// Everything here is MEASURED. A tier that merely exists proves nothing — the
// question is always what is in it, and whether the picture survived.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hasEncoder reports whether this ffmpeg build carries the named encoder. AV1 is
// an OPERATOR's choice (FULL_CODEC), so a build without libsvtav1 is a different
// deployment and not a broken one — the test skips rather than fails.
func hasEncoder(t *testing.T, ffmpeg, name string) bool {
	t.Helper()
	return strings.Contains(run(t, ffmpeg, "-hide_banner", "-encoders"), name)
}

// TestPlayabilityAgainstRealFiles walks the playability table over files ffmpeg
// writes, so the verdict is about real containers and real stream metadata rather
// than about a hand-written probe.
func TestPlayabilityAgainstRealFiles(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	ctx := context.Background()

	// The ordinary upload: H.264 + AAC in mp4, `moov` at the END, because that is
	// what ffmpeg writes unless it is told otherwise.
	plain := gen(t, ffmpeg, dir, "plain.mp4", join(
		[]string{"-f", "lavfi", "-i", src("1280x720", "30"), "-f", "lavfi", "-i", sine("440")}, v, a, []string{"-shortest"})...)
	// The same thing with the moov moved to the front.
	fast := gen(t, ffmpeg, dir, "fast.mp4", join(
		[]string{"-f", "lavfi", "-i", src("1280x720", "30"), "-f", "lavfi", "-i", sine("440")}, v, a,
		[]string{"-shortest", "-movflags", "+faststart"})...)
	// Matroska: a browser plays it, and this pipeline still refuses it, because
	// every media URL promises an mp4.
	mkv := gen(t, ffmpeg, dir, "plain.mkv", join(
		[]string{"-f", "lavfi", "-i", src("1280x720", "30")}, v, []string{"-an"})...)
	// 10-bit 4:2:0 — the shape a modern capture card writes and no browser
	// decodes in hardware.
	tenBit := gen(t, ffmpeg, dir, "tenbit.mp4",
		"-f", "lavfi", "-i", src("1280x720", "30"), "-c:v", "libx264", "-preset", "ultrafast",
		"-pix_fmt", "yuv420p10le", "-profile:v", "high10", "-an", "-movflags", "+faststart")
	// Two audio tracks: merging them is a FEATURE of this pipeline, and a browser
	// would play only the first.
	dual := gen(t, ffmpeg, dir, "dual.mp4", join(
		[]string{"-f", "lavfi", "-i", src("1280x720", "30"), "-f", "lavfi", "-i", sine("440"),
			"-f", "lavfi", "-i", sine("880"), "-map", "0:v", "-map", "1:a", "-map", "2:a"}, v, a,
		[]string{"-shortest", "-movflags", "+faststart"})...)

	for _, tc := range []struct {
		name      string
		file      string
		mediaType string
		want      Playability
	}{
		{"faststart mp4 plays directly", fast, "video/mp4", Playability{Playable: true}},
		{"moov at the end needs a remux", plain, "video/mp4", Playability{Playable: true, Remux: true}},
		{"a quicktime media type needs a remux", fast, "video/quicktime", Playability{Playable: true, Remux: true}},
		{"matroska", mkv, "video/x-matroska", Playability{Reason: NotPlayableContainer}},
		{"10-bit", tenBit, "video/mp4", Playability{Reason: NotPlayablePixFmt}},
		{"two audio tracks", dual, "video/mp4", Playability{Reason: NotPlayableAudio}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Probe(ffprobe)(ctx, tc.file)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			got := OriginalPlayability(p, tc.mediaType, FaststartMP4(tc.file))
			if got != tc.want {
				t.Fatalf("OriginalPlayability(%s) = %+v, want %+v (codec=%s pix_fmt=%s audio=%v)",
					filepath.Base(tc.file), got, tc.want, p.VideoCodec, p.PixFmt, p.AudioCodecs)
			}
		})
	}
	// FaststartMP4 is the whole of the remux decision, and it reads the file.
	if FaststartMP4(plain) {
		t.Error("FaststartMP4 says an ordinary ffmpeg mp4 needs no remux")
	}
	if !FaststartMP4(fast) {
		t.Error("FaststartMP4 says a +faststart mp4 needs one")
	}
	if FaststartMP4(mkv) {
		t.Error("FaststartMP4 accepted a matroska file")
	}
}

// TestRemuxKeepsTheCodedBytes is the fast path's central claim: a remux is a
// container rewrite and NOT an encode — the picture is bit-identical, the moov
// moves to the front, and the whole thing is far cheaper than an encode.
func TestRemuxKeepsTheCodedBytes(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	ctx := context.Background()

	in := gen(t, ffmpeg, dir, "in.mp4", join(
		[]string{"-f", "lavfi", "-i", src("1920x1080", "60"), "-f", "lavfi", "-i", sine("600")}, v, a, []string{"-shortest"})...)
	p, err := Probe(ffprobe)(ctx, in)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if play := OriginalPlayability(p, "video/mp4", FaststartMP4(in)); !play.Playable || !play.Remux {
		t.Fatalf("the fixture is not the remux case: %+v", play)
	}
	out := filepath.Join(dir, "remuxed.mp4")
	if err := Remux(ctx, ffmpeg, p, in, out); err != nil {
		t.Fatalf("Remux: %v", err)
	}
	// The result is a faststart mp4 the pipeline's own verifier accepts…
	if err := VerifyMP4(out); err != nil {
		t.Fatalf("VerifyMP4(remux): %v", err)
	}
	info, err := ProbeOutput(ctx, ffprobe, out)
	if err != nil {
		t.Fatalf("ProbeOutput: %v", err)
	}
	if err := CheckTierOutput(info, TierCheck{Codecs: []string{FullCodecH264, FullCodecAV1},
		Width: p.CodedW, Height: p.CodedH, AllowOdd: true}); err != nil {
		t.Fatalf("CheckTierOutput(remux): %v (%+v)", err, *info)
	}
	// …at the source's own resolution and frame rate. This is what "not
	// downscaled" means, measured.
	if info.Width != 1920 || info.Height != 1080 {
		t.Fatalf("the remux is %dx%d, want the source's 1920x1080", info.Width, info.Height)
	}
	if (info.FPS != Rational{60, 1}) {
		t.Fatalf("the remux runs at %s, want 60/1", info.FPS)
	}
	// The CODED BYTES are identical: extract the elementary stream from both and
	// compare. Not "about the same size" — the same bytes.
	inES, outES := filepath.Join(dir, "in.h264"), filepath.Join(dir, "out.h264")
	run(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", in, "-an", "-c", "copy", "-f", "h264", inES)
	run(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", out, "-an", "-c", "copy", "-f", "h264", outES)
	a, err := os.ReadFile(inES)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(outES)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("the remux changed the coded video: %d bytes in, %d out", len(a), len(b))
	}
}

// TestFullTierIsNotDownscaled is the bundle's headline, measured: a 1440p60
// source that the browser cannot play comes back at 1440p60 and not at 720p.
func TestFullTierIsNotDownscaled(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	ctx := context.Background()

	// An HEVC source in mp4: playable nowhere this pipeline promises, so it gets
	// a Full — and a 1440p60 one, which the OLD box would have made 720p60.
	in := gen(t, ffmpeg, dir, "hevc-1440p60.mp4",
		"-f", "lavfi", "-i", src("2560x1440", "60"), "-c:v", "libx265", "-preset", "ultrafast",
		"-pix_fmt", "yuv420p", "-tag:v", "hvc1", "-an", "-movflags", "+faststart")
	p, err := Probe(ffprobe)(ctx, in)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	box := PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60}
	tp := MakeTierPlan(p, TierPlanOptions{Box: box, FullCodec: FullCodecH264,
		LightMaxBytes: 25 << 20, SourceSize: 1 << 20, SourceMediaType: "video/mp4", Faststart: true})
	if tp.Playable || !tp.FullNeeded || tp.Reason != NotPlayableCodec {
		t.Fatalf("tier plan = %+v, want a Full because of the codec", tp)
	}
	if tp.Full.W != 2560 || tp.Full.H != 1440 {
		t.Fatalf("the Full is planned at %dx%d, want the source's 2560x1440 — the instance box must not bind",
			tp.Full.W, tp.Full.H)
	}
	// The card still gets its own copy, inside the tier's own box.
	if !tp.LightNeeded || tp.Light.W != 1280 || tp.Light.H != 720 || (tp.Light.FPS != Rational{30, 1}) {
		t.Fatalf("the Light copy = %dx%d @ %s, want 1280x720 @ 30/1", tp.Light.W, tp.Light.H, tp.Light.FPS)
	}

	// Encode the Full for real and measure it.
	out := filepath.Join(dir, "full.mp4")
	o := ffmpegOpts
	o.CRF = FullCRF
	if err := Encode(ctx, ffmpeg, tp.Full, in, out, o, nil); err != nil {
		t.Fatalf("Encode(full): %v", err)
	}
	info, err := ProbeOutput(ctx, ffprobe, out)
	if err != nil {
		t.Fatalf("ProbeOutput: %v", err)
	}
	if err := CheckTierOutput(info, TierCheck{Codecs: []string{FullCodecH264},
		Width: tp.Full.W, Height: tp.Full.H}); err != nil {
		t.Fatalf("CheckTierOutput(full): %v (%+v)", err, *info)
	}
	if info.Width != 2560 || info.Height != 1440 || (info.FPS != Rational{60, 1}) {
		t.Fatalf("the Full came out %dx%d @ %s", info.Width, info.Height, info.FPS)
	}
	// The old boxed check would have refused this file, which is the point.
	if err := CheckOutput(info, box); err == nil {
		t.Fatal("CheckOutput accepted a 1440p tier; the box check is the Light tier's alone")
	}
}

// TestFullTierInAV1 is FULL_CODEC=av1 on the encoder this image ships. It is
// skipped rather than failed where libsvtav1 is absent: the tier is offered
// because the shipped ffmpeg has it, and a build without it is a different
// deployment rather than a broken one.
func TestFullTierInAV1(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	if !hasEncoder(t, ffmpeg, "libsvtav1") {
		t.Skip("this ffmpeg has no libsvtav1")
	}
	dir := t.TempDir()
	ctx := context.Background()

	in := gen(t, ffmpeg, dir, "in.mkv", join(
		[]string{"-f", "lavfi", "-i", src("640x360", "30")}, v, []string{"-an"})...)
	p, err := Probe(ffprobe)(ctx, in)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	plan := MakePlan(p, FullPlanOptions(p, PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60}))
	out := filepath.Join(dir, "full-av1.mp4")
	o := ffmpegOpts
	o.Codec, o.CRF = FullCodecAV1, FullCRFFor(FullCodecAV1)
	if err := Encode(ctx, ffmpeg, plan, in, out, o, nil); err != nil {
		t.Fatalf("Encode(av1): %v", err)
	}
	if err := VerifyMP4(out); err != nil {
		t.Fatalf("VerifyMP4(av1): %v", err)
	}
	info, err := ProbeOutput(ctx, ffprobe, out)
	if err != nil {
		t.Fatalf("ProbeOutput: %v", err)
	}
	if err := CheckTierOutput(info, TierCheck{Codecs: []string{FullCodecAV1},
		Width: plan.W, Height: plan.H}); err != nil {
		t.Fatalf("CheckTierOutput(av1): %v (%+v)", err, *info)
	}
	if info.VideoCodec != "av1" {
		t.Fatalf("the av1 Full is %q", info.VideoCodec)
	}
	// …and such a file IS playable as an original: AV1 in mp4 is a tier this
	// pipeline serves directly, which is why a source already in it is never
	// re-encoded.
	ap, err := Probe(ffprobe)(ctx, out)
	if err != nil {
		t.Fatalf("Probe(av1 output): %v", err)
	}
	if play := OriginalPlayability(ap, "video/mp4", FaststartMP4(out)); !play.Playable {
		t.Fatalf("an av1 mp4 is not playable: %+v", play)
	}
}

// The privacy half of the fast path, on real files (§20 erratum 200, amended).
// The direct branch is the ONE place this pipeline publishes bytes it did not
// render, so a file's own GPS/device/creation tags would go public on it. They
// make the file a REMUX case instead, and the remux — the same `-c copy` the
// moov-last case takes — strips every one of them.
func TestPrivateMetadataIsRemuxedAway(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	ctx := context.Background()

	// A faststart mp4 that would otherwise be served exactly as it stands, with
	// the tags a phone writes.
	in := gen(t, ffmpeg, dir, "tagged.mp4", join(
		[]string{"-f", "lavfi", "-i", src("1280x720", "30"), "-f", "lavfi", "-i", sine("440")}, v, a,
		[]string{"-shortest",
			"-metadata", "location=+51.5074-000.1278/",
			"-metadata", "artist=a phone",
			"-metadata", "comment=PRIVATE-COMMENT",
			"-metadata", "creation_time=2019-04-05T13:14:15Z",
			"-movflags", "+faststart"})...)
	if !FaststartMP4(in) {
		t.Fatal("the fixture is not faststart; it would take the remux branch anyway and prove nothing")
	}
	p, err := Probe(ffprobe)(ctx, in)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !p.PrivateTags {
		t.Fatalf("the probe saw no private metadata in %s", filepath.Base(in))
	}
	play := OriginalPlayability(p, "video/mp4", FaststartMP4(in))
	if !play.Playable || !play.Remux {
		t.Fatalf("OriginalPlayability = %+v, want playable AND remux (the tags are the trigger)", play)
	}

	out := filepath.Join(dir, "remuxed.mp4")
	if err := Remux(ctx, ffmpeg, p, in, out); err != nil {
		t.Fatalf("Remux: %v", err)
	}
	// What a viewer would be served carries none of them — and is a fixed point of
	// the rule, so it would never be remuxed again for its metadata.
	rp, err := Probe(ffprobe)(ctx, out)
	if err != nil {
		t.Fatalf("Probe(remux): %v", err)
	}
	if rp.PrivateTags {
		t.Fatalf("the remux still carries private metadata: %s", string(rp.Raw))
	}
	// The VALUES and not the keys: `chroma_location` is an ffprobe field of every
	// video stream and says nothing about anybody's whereabouts.
	for _, word := range []string{"+51.5074", "PRIVATE-COMMENT", "a phone", "2019-04-05"} {
		if strings.Contains(string(rp.Raw), word) {
			t.Fatalf("%q survived the remux: %s", word, string(rp.Raw))
		}
	}
}
