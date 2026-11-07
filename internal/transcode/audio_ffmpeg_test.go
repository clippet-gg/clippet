//go:build ffmpeg

package transcode

// §20 erratum 328 on real ffmpeg: the strip leaves no tag, no picture and no
// encoder string and is bit-exact; the FLAC Full is bit-exact by the PCM hash;
// a +3 dBFS float source's 24-bit Full is not clipped and is not lossless; the
// card copy is an H.264 + AAC MP4 of the picture's size with no tag either.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// tagsOf is every tag key ffprobe reports anywhere in the file, and whether
// any stream is an attached picture.
func tagsOf(t *testing.T, ffprobe, path string) (keys []string, picture bool) {
	t.Helper()
	out := run(t, ffprobe, "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path)
	var doc ffprobeDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	for k := range doc.Format.Tags {
		keys = append(keys, "format:"+k)
	}
	for _, s := range doc.Streams {
		for k := range s.Tags {
			keys = append(keys, "stream:"+k)
		}
		if s.Disposition["attached_pic"] == 1 {
			picture = true
		}
	}
	return keys, picture
}

// assertNoTags is the pin "format.tags empty, no attached_pic, no encoder
// anywhere" — with the one allowance an MP4 needs: its structural boxes
// (major_brand and friends, a track's language/handler/vendor), which name
// nothing about a person, a place or a program.
func assertNoTags(t *testing.T, ffprobe, path string) {
	t.Helper()
	keys, picture := tagsOf(t, ffprobe, path)
	if picture {
		t.Errorf("%s: carries an attached picture", filepath.Base(path))
	}
	for _, k := range keys {
		name := k[strings.Index(k, ":")+1:]
		if name == "encoder" {
			t.Errorf("%s: names an encoder (%s)", filepath.Base(path), k)
			continue
		}
		if _, ok := structuralTags[name]; !ok {
			t.Errorf("%s: carries the tag %s", filepath.Base(path), k)
		}
	}
}

func TestAudioStripAndFullsOnFFmpeg(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()
	ctx := context.Background()
	strip := StripVerified(ffmpeg, ffprobe, 1)
	tags := []string{"-metadata", "title=Secret", "-metadata", "artist=Someone", "-metadata", "comment=home address"}

	t.Run("mp3 with ID3 and a cover", func(t *testing.T) {
		in := gen(t, ffmpeg, dir, "song.mp3", join(
			[]string{"-f", "lavfi", "-i", sine("440"), "-f", "lavfi", "-i", "color=c=red:s=64x64:d=1",
				"-map", "0:a", "-map", "1:v", "-c:a", "libmp3lame", "-b:a", "320k", "-c:v", "mjpeg",
				"-disposition:v", "attached_pic", "-id3v2_version", "3"}, tags)...)
		if keys, pic := tagsOf(t, ffprobe, in); len(keys) == 0 || !pic {
			t.Fatalf("the fixture must carry tags and a cover (keys %v, picture %v)", keys, pic)
		}
		p, err := Probe(ffprobe)(ctx, in)
		if err != nil || p.Kind != KindAudio || !p.CoverArt {
			t.Fatalf("probe: %+v %v", p, err)
		}
		out := filepath.Join(dir, "s.mp3")
		if _, err := strip(ctx, p, in, out); err != nil {
			t.Fatalf("strip: %v", err)
		}
		assertNoTags(t, ffprobe, out)
		// The strip is reproducible, which is what lets a second upload of one
		// file dedupe after its strip (erratum 326).
		again := filepath.Join(dir, "s2.mp3")
		if _, err := strip(ctx, p, in, again); err != nil {
			t.Fatalf("strip again: %v", err)
		}
		a, _ := os.ReadFile(out)
		b, _ := os.ReadFile(again)
		if string(a) != string(b) {
			t.Error("two strips of one file differ")
		}
		q, _ := Probe(ffprobe)(ctx, out)
		if plan := MakeAudioPlan(q, FaststartMP4(out)); !plan.Playable {
			t.Errorf("a stripped mp3 must play as its Original: %+v", plan)
		}
	})

	t.Run("wav 24/96 strips bit-exact and its FLAC Full is bit-exact", func(t *testing.T) {
		in := gen(t, ffmpeg, dir, "w24.wav", join([]string{"-f", "lavfi", "-i", sine("440"), "-c:a", "pcm_s24le", "-ar", "96000"}, tags)...)
		p, err := Probe(ffprobe)(ctx, in)
		if err != nil || p.AudioBits != 24 || p.AudioSampleRate != 96000 {
			t.Fatalf("probe: %+v %v", p, err)
		}
		out := filepath.Join(dir, "s.wav")
		if _, err := strip(ctx, p, in, out); err != nil {
			t.Fatalf("strip: %v", err)
		}
		assertNoTags(t, ffprobe, out)
		if err := SamePCM(ctx, ffmpeg, in, out); err != nil {
			t.Fatalf("the strip of a WAV changed its samples: %v", err)
		}
		q, _ := Probe(ffprobe)(ctx, out)
		plan := MakeAudioPlan(q, false)
		if plan.Full != AudioFullFLAC || !plan.Lossless {
			t.Fatalf("plan: %+v", plan)
		}
		full := filepath.Join(dir, "full.flac")
		if err := RunAudio(ctx, ffmpeg, AudioFullArgs(q, plan, 0, out, full, 0, 1)); err != nil {
			t.Fatalf("full: %v", err)
		}
		assertNoTags(t, ffprobe, full)
		if err := SamePCM(ctx, ffmpeg, in, full); err != nil {
			t.Fatalf("the FLAC Full is not bit-exact: %v", err)
		}
		info, err := ProbeOutput(ctx, ffprobe, full)
		if err != nil || CheckAudioOutput(info, AudioCheck{Codec: "flac"}) != nil {
			t.Fatalf("full verdict: %+v %v", info, err)
		}
	})

	t.Run("a +3 dBFS float wav: the 24-bit Full is fitted, not clipped, not lossless", func(t *testing.T) {
		in := gen(t, ffmpeg, dir, "hot.wav", "-f", "lavfi", "-i", "aevalsrc=1.4125*sin(2*PI*440*t):s=48000:d=2", "-c:a", "pcm_f32le")
		p, err := Probe(ffprobe)(ctx, in)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		plan := MakeAudioPlan(p, false)
		if plan.Full != AudioFullFLAC24 || !plan.MeasurePeak || plan.Lossless {
			t.Fatalf("plan: %+v", plan)
		}
		peak, err := PeakDB(ctx, ffmpeg, in)
		if err != nil || peak < 2.9 || peak > 3.1 {
			t.Fatalf("the fixture's peak is %v dBFS (%v), want +3", peak, err)
		}
		fitted := filepath.Join(dir, "fitted.flac")
		if err := RunAudio(ctx, ffmpeg, AudioFullArgs(p, plan, FitGainDB(peak), in, fitted, 0, 0)); err != nil {
			t.Fatalf("full: %v", err)
		}
		if got, _ := PeakDB(ctx, ffmpeg, fitted); got >= 0 || got < -0.1 {
			t.Errorf("the fitted Full peaks at %v dBFS; want just under 0 (no clipping)", got)
		}
		// The control: with no gain the integer conversion saturates — the peak
		// is EXACTLY full scale, which is what clipping measures as.
		clipped := filepath.Join(dir, "clipped.flac")
		if err := RunAudio(ctx, ffmpeg, AudioFullArgs(p, plan, 0, in, clipped, 0, 0)); err != nil {
			t.Fatalf("control: %v", err)
		}
		if got, _ := PeakDB(ctx, ffmpeg, clipped); got != 0 {
			t.Errorf("the unfitted control peaks at %v dBFS; the fixture no longer exercises clipping", got)
		}
		if err := SamePCMAs(ctx, ffmpeg, in, fitted, true); err == nil {
			t.Error("a 24-bit FLAC of a float source decodes to the same samples: it would be called lossless")
		}
	})

	// Matroska/WebM (a browser's MediaRecorder writes `audio/webm;codecs=opus`):
	// its own muxers keep a DURATION tag and `encoder=Lavf` under every strip
	// flag, so the strip moves each codec into a clean box — and the Opus one
	// then PLAYS as its Original.
	for _, c := range []struct {
		name, file, codec, format string
		extra                     []string
		ext                       string
		playable, lossless        bool
	}{
		{"webm opus", "rec.webm", "libopus", "webm", []string{"-ac", "1", "-ar", "48000"}, "ogg", true, false},
		{"webm vorbis", "v.webm", "libvorbis", "webm", nil, "ogg", true, false},
		{"mka flac", "f.mka", "flac", "matroska", []string{"-ar", "48000"}, "flac", true, true},
		{"mka pcm", "p.mka", "pcm_s16le", "matroska", nil, "wav", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := gen(t, ffmpeg, dir, c.file, join(join([]string{"-f", "lavfi", "-i", sine("440"), "-c:a", c.codec}, c.extra), join(tags, []string{"-f", c.format}))...)
			p, err := Probe(ffprobe)(ctx, in)
			if err != nil || p.Kind != KindAudio {
				t.Fatalf("probe: %+v %v", p, err)
			}
			out := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "-")+"."+c.ext)
			tgt, err := strip(ctx, p, in, out)
			if err != nil {
				t.Fatalf("strip: %v", err)
			}
			if tgt.Ext != c.ext {
				t.Fatalf("stored as .%s, want .%s", tgt.Ext, c.ext)
			}
			assertNoTags(t, ffprobe, out)
			if c.lossless {
				if err := SamePCM(ctx, ffmpeg, in, out); err != nil {
					t.Fatalf("the strip changed the samples: %v", err)
				}
			}
			q, err := Probe(ffprobe)(ctx, out)
			if err != nil {
				t.Fatalf("probe the strip: %v", err)
			}
			if plan := MakeAudioPlan(q, FaststartMP4(out)); plan.Playable != c.playable {
				t.Errorf("plan of the stripped %s: %+v, want playable=%v", c.ext, plan, c.playable)
			}
		})
	}

	// A STEREO MP3 (§20 erratum 329, found by its E2E): the muxer's own Xing
	// frame names "Lavf" on the stream, and the strip's proof refused every one.
	t.Run("a stereo mp3 strips", func(t *testing.T) {
		in := gen(t, ffmpeg, dir, "st.mp3", join([]string{"-f", "lavfi", "-i", sine("440"), "-ac", "2",
			"-c:a", "libmp3lame", "-b:a", "192k"}, tags)...)
		p, err := Probe(ffprobe)(ctx, in)
		if err != nil || p.AudioChannels != 2 {
			t.Fatalf("probe: %+v %v", p, err)
		}
		out := filepath.Join(dir, "st-strip.mp3")
		if _, err := strip(ctx, p, in, out); err != nil {
			t.Fatalf("a stereo mp3 does not strip: %v", err)
		}
	})

	t.Run("the card copy", func(t *testing.T) {
		audiogramOnFFmpeg(t, ffmpeg, ffprobe, dir)
	})
}

// frameRGB is the colour of one pixel of the frame at `sec` of a video, as
// ffmpeg decodes and converts it (rgb24, BT.709 as the file is stamped).
func frameRGB(t *testing.T, ffmpeg, in string, sec float64, x, y int) [3]int {
	t.Helper()
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-ss", strconv.FormatFloat(sec, 'f', 3, 64), "-i", in,
		"-frames:v", "1", "-vf", "scale=in_color_matrix=bt709:in_range=tv,crop=1:1:"+strconv.Itoa(x)+":"+strconv.Itoa(y)+",format=rgb24",
		"-f", "rawvideo", "-")
	out, err := cmd.Output()
	if err != nil || len(out) < 3 {
		t.Fatalf("frame at %v: %v (%d bytes)", sec, err, len(out))
	}
	return [3]int{int(out[0]), int(out[1]), int(out[2])}
}

func nearRGB(a, b [3]int, tol int) bool {
	for i := range a {
		if d := a[i] - b[i]; d > tol || d < -tol {
			return false
		}
	}
	return true
}

// audiogramOnFFmpeg is the audiogram (§20 erratum 329) end to end on real
// ffmpeg: the peaks of a rising sound, the stencil drawn from them, the
// research's filtergraph 2, the clean pass — and the pins: it passes the card
// verdict (exactly one H.264 stream at 1280×720 and one AAC stream, faststart,
// no tag, no picture), its duration is the sound's within one frame, its
// frame 0 is the poster (the unplayed colour inside a bar) and its played bars
// turn amber left to right; an AAC source is carried as it is; the poster PNG
// takes its duration label and stays a PNG.
func audiogramOnFFmpeg(t *testing.T, ffmpeg, ffprobe, dir string) {
	ctx := context.Background()
	// Six seconds that grow from nothing to full scale: the bars must rise.
	in := gen(t, ffmpeg, dir, "c.flac", "-f", "lavfi", "-i", "aevalsrc=sin(2*PI*440*t)*t/6:s=48000:d=6", "-ac", "2", "-c:a", "flac")
	p, err := Probe(ffprobe)(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	heights, err := WavePeaks(ctx, ffmpeg, in, p, WaveBars)
	if err != nil {
		t.Fatalf("peaks: %v", err)
	}
	// The loudness of a linear ramp through the curve (§20 erratum 332): the
	// middle bar at half the loudest RMS is 255 × 0.5^0.6 ≈ 168.
	if heights[0] > 20 || heights[WaveBars-1] != 255 || heights[WaveBars/2] < 150 || heights[WaveBars/2] > 190 {
		t.Errorf("the loudness of a linear ramp: first %d, middle %d, last %d", heights[0], heights[WaveBars/2], heights[WaveBars-1])
	}
	for i := 1; i < WaveBars; i++ {
		if int(heights[i])+3 < int(heights[i-1]) {
			t.Errorf("the ramp's bars fall at %d: %v", i, heights)
			break
		}
	}
	stencil := filepath.Join(dir, "stencil.png")
	if err := WritePNG(WaveStencil(heights), stencil); err != nil {
		t.Fatal(err)
	}
	enc, out := filepath.Join(dir, "l.enc.mp4"), filepath.Join(dir, "l.mp4")
	if err := RunAudio(ctx, ffmpeg, AudioLightArgs(p, stencil, in, enc, 256, false, 0, 0)); err != nil {
		t.Fatalf("audiogram: %v", err)
	}
	if err := RunAudio(ctx, ffmpeg, CleanMP4Args(enc, out)); err != nil {
		t.Fatalf("clean: %v", err)
	}
	assertNoTags(t, ffprobe, out)
	info, err := ProbeOutput(ctx, ffprobe, out)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckTierOutput(info, TierCheck{Codecs: []string{"h264"}, Width: 1280, Height: 720}); err != nil {
		t.Fatalf("audiogram: %v", err)
	}
	if err := CheckOutput(info, PlanOptions{}); err != nil {
		t.Fatalf("the card verdict: %v", err)
	}
	if info.VideoStreams != 1 || info.AudioStreams != 1 || info.AudioCodec != "aac" || VerifyMP4(out) != nil {
		t.Fatalf("audiogram: %+v", info)
	}
	if info.FPS.Num != 30 || info.FPS.Den != 1 {
		t.Errorf("audiogram at %v fps, want 30", info.FPS)
	}
	if d := info.DurationMS - 6000; d < -34 || d > 34 {
		t.Errorf("the audiogram lasts %d ms, the sound 6000 (more than a frame apart)", info.DurationMS)
	}
	// Frame 0 IS the poster: inside the last (full) bar, the unplayed colour.
	x, _, _, _ := WaveBar(WaveBars-1, 255)
	rest, played, ground := hexTriple(WaveRestHex), hexTriple(WavePlayedHex), hexTriple(WaveGroundHex)
	if got := frameRGB(t, ffmpeg, out, 0, int(x)+5, 360); !nearRGB(got, rest, 8) {
		t.Errorf("frame 0 inside a bar: %v, want the poster's %v", got, rest)
	}
	if got := frameRGB(t, ffmpeg, out, 0, 40, 40); !nearRGB(got, ground, 6) {
		t.Errorf("frame 0's margin: %v, want the ground %v", got, ground)
	}
	// Half way, the first bars are played and the last are not; at the end, all are.
	x0, _, _, _ := WaveBar(5, 255)
	if got := frameRGB(t, ffmpeg, out, 3, int(x0)+5, 360); !nearRGB(got, played, 10) {
		t.Errorf("half way, an early bar: %v, want amber %v", got, played)
	}
	if got := frameRGB(t, ffmpeg, out, 3, int(x)+5, 360); !nearRGB(got, rest, 8) {
		t.Errorf("half way, the last bar: %v, want still %v", got, rest)
	}
	x55, _, _, _ := WaveBar(55, 255)
	if got := frameRGB(t, ffmpeg, out, 5.9, int(x55)+5, 360); !nearRGB(got, played, 10) {
		t.Errorf("near the end, a late bar: %v, want amber %v", got, played)
	}

	// D6a: an AAC source that fits is carried as it is (its own rate, 44.1 kHz).
	m4a := gen(t, ffmpeg, dir, "c.m4a", "-f", "lavfi", "-i", sine("440"), "-ar", "44100", "-c:a", "aac", "-b:a", "128k")
	pa, err := Probe(ffprobe)(ctx, m4a)
	if err != nil {
		t.Fatal(err)
	}
	cp := filepath.Join(dir, "cp.mp4")
	if err := RunAudio(ctx, ffmpeg, AudioLightArgs(pa, stencil, m4a, cp, 256, true, 0, 0)); err != nil {
		t.Fatalf("audiogram with a copied AAC: %v", err)
	}
	if rate := strings.TrimSpace(run(t, ffprobe, "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=sample_rate,codec_name",
		"-of", "csv=p=0", cp)); rate != "aac,44100" {
		t.Errorf("the copied AAC is %q, want aac,44100 (as it was)", rate)
	}

	// The poster: a PNG of the frame, its duration plate in the top-right.
	font := posterFont(t, ffmpeg)
	poster := filepath.Join(dir, "poster.png")
	res, err := AudioPoster(ctx, ffmpeg, heights, nil, poster, PosterLabel{FontFile: font, DurationMS: 6000})
	if err != nil || res.Width != 1280 || res.Height != 720 {
		t.Fatalf("poster: %+v %v", res, err)
	}
	b, err := os.ReadFile(poster)
	if err != nil || len(b) < 4 || string(b[1:4]) != "PNG" || b[0] != 0x89 {
		t.Fatalf("the labelled poster is not a PNG (%v)", err)
	}
	assertNoTags(t, ffprobe, poster)
	if got := frameRGB(t, ffmpeg, poster, 0, 1280-30, 30); nearRGB(got, ground, 2) {
		t.Errorf("no duration plate in the poster's top-right (%v)", got)
	}
}
