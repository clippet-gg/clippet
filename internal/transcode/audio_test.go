package transcode

import (
	"math"
	"slices"
	"strings"
	"testing"
)

// audioDoc is an ffprobe document of one audio stream (plus `extra` streams)
// in `format`, the shape every audio pin below starts from.
func audioDoc(format, codec, extra string) []byte {
	s := `{"streams":[{"index":0,"codec_type":"audio","codec_name":"` + codec + `","sample_rate":"48000","channels":2,` +
		`"sample_fmt":"s32","bits_per_sample":24,"bits_per_raw_sample":"24","duration":"4.0"}` + extra + `],` +
		`"format":{"format_name":"` + format + `","duration":"4.0"}}`
	return []byte(s)
}

// TestParseProbeAudio pins ParseProbe's second branch (§20 erratum 328): a
// file with no non-cover video stream and ONE audio stream in an allowlisted
// container is sound, and its shape is read.
func TestParseProbeAudio(t *testing.T) {
	p, err := ParseProbe(audioDoc("wav", "pcm_s24le", ""))
	if err != nil {
		t.Fatalf("wav: %v", err)
	}
	if p.Kind != KindAudio || p.VideoIndex != -1 || !slices.Equal(p.AudioIndexes, []int{0}) {
		t.Fatalf("wav: kind %q video %d audio %v", p.Kind, p.VideoIndex, p.AudioIndexes)
	}
	if p.AudioSampleRate != 48000 || p.AudioChannels != 2 || p.AudioBits != 24 || p.DurationUS != 4_000_000 {
		t.Fatalf("wav shape: %+v", p)
	}
	// The cover-only "video" the video gate always refused is an MP3 with a
	// picture: it is SOUND, and the picture is noted, never a video.
	c, err := ParseProbe(fixture(t, "cover-only.json"))
	if err != nil || c.Kind != KindAudio || !c.CoverArt || c.VideoIndex != -1 {
		t.Fatalf("cover-only: %+v, %v", c, err)
	}
	// A video is still a video: the second branch is never reached by a file
	// with a picture, so a broken picture cannot be waved through as sound.
	_, err = ParseProbe([]byte(`{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":0,"height":0},
		{"index":1,"codec_type":"audio","codec_name":"aac","channels":2,"sample_rate":"48000"}],
		"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"2.0"}}`))
	if !IsPermanent(err) || !strings.Contains(err.Error(), "no_video_stream") {
		t.Fatalf("a zero-dim video with sound: err = %v, want the video gate's refusal", err)
	}
}

// TestParseProbeAudioRefusals pins every refusal of the audio branch, each in
// the permanent not_a_video shape the gate answers 415 with.
func TestParseProbeAudioRefusals(t *testing.T) {
	second := `,{"index":1,"codec_type":"audio","codec_name":"pcm_s24le","channels":2,"sample_rate":"48000"}`
	for _, c := range []struct {
		name string
		doc  []byte
		want string
	}{
		{"two audio tracks", audioDoc("wav", "pcm_s24le", second), "2 audio tracks"},
		{"dsd in an allowed box", audioDoc("matroska,webm", "dsd_lsbf_planar", ""), "DSD audio"},
		{"dsf container", audioDoc("dsf", "dsd_lsbf_planar", ""), "unsupported container (dsf)"},
		{"dsdiff container", audioDoc("iff", "dsd_msbf", ""), "unsupported container (iff)"},
		{"monkey's audio", audioDoc("ape", "ape", ""), "unsupported container (ape)"},
		{"a playlist with sound", audioDoc("hls", "aac", ""), "unsupported container (hls)"},
		{"nine channels", []byte(strings.Replace(string(audioDoc("wav", "pcm_s16le", "")), `"channels":2`, `"channels":9`, 1)), "9 audio channels"},
		{"no duration", []byte(strings.ReplaceAll(string(audioDoc("flac", "flac", "")), `"duration":"4.0"`, `"duration":"N/A"`)), "no duration"},
		{"zero duration", []byte(strings.ReplaceAll(string(audioDoc("flac", "flac", "")), `"duration":"4.0"`, `"duration":"0.000000"`)), "no duration"},
	} {
		_, err := ParseProbe(c.doc)
		if !IsPermanent(err) || !strings.Contains(err.Error(), "not_a_video") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want permanent not_a_video %q", c.name, err, c.want)
		}
	}
}

func audioProbe(format, codec, sampleFmt string, bits, channels int) *ProbeResult {
	return &ProbeResult{Kind: KindAudio, VideoIndex: -1, AudioIndexes: []int{0}, AudioCodecs: []string{codec},
		FormatName: format, AudioSampleFmt: sampleFmt, AudioBits: bits, AudioChannels: channels,
		AudioSampleRate: 48000, HasDuration: true, DurationUS: 4_000_000}
}

// TestMakeAudioPlan pins the decision table of the erratum, row by row.
func TestMakeAudioPlan(t *testing.T) {
	for _, c := range []struct {
		name      string
		p         *ProbeResult
		faststart bool
		playable  bool
		full      string
		lossless  bool
		peak      bool
	}{
		{"mp3", audioProbe("mp3", "mp3", "fltp", 0, 2), false, true, "", false, false},
		{"aac in a faststart m4a", audioProbe("mov,mp4,m4a,3gp,3g2,mj2", "aac", "fltp", 0, 2), true, true, "", false, false},
		{"aac in a moov-last m4a", audioProbe("mov,mp4,m4a,3gp,3g2,mj2", "aac", "fltp", 0, 2), false, false, AudioFullAAC, false, false},
		{"opus in ogg", audioProbe("ogg", "opus", "fltp", 0, 1), false, true, "", false, false},
		{"vorbis in ogg", audioProbe("ogg", "vorbis", "fltp", 0, 2), false, true, "", false, false},
		{"flac 24", audioProbe("flac", "flac", "s32", 24, 2), false, true, "", false, false},
		{"flac 32 int", audioProbe("flac", "flac", "s32", 32, 2), false, false, AudioFullFLAC24, false, false},
		{"wav 16", audioProbe("wav", "pcm_s16le", "s16", 16, 2), false, false, AudioFullFLAC, true, false},
		{"wav 24", audioProbe("wav", "pcm_s24le", "s32", 24, 2), false, false, AudioFullFLAC, true, false},
		{"aiff", audioProbe("aiff", "pcm_s16be", "s16", 16, 2), false, false, AudioFullFLAC, true, false},
		{"alac", audioProbe("mov,mp4,m4a,3gp,3g2,mj2", "alac", "s32p", 24, 2), true, false, AudioFullFLAC, true, false},
		{"wavpack", audioProbe("wv", "wavpack", "s16p", 16, 2), false, false, AudioFullFLAC, true, false},
		{"flac in matroska", audioProbe("matroska,webm", "flac", "s16", 16, 2), false, false, AudioFullFLAC, true, false},
		{"wav 32f", audioProbe("wav", "pcm_f32le", "flt", 32, 2), false, false, AudioFullFLAC24, false, true},
		{"wav 32 int", audioProbe("wav", "pcm_s32le", "s32", 32, 2), false, false, AudioFullFLAC24, false, false},
		{"wma", audioProbe("asf", "wmav2", "fltp", 0, 2), false, false, AudioFullAAC, false, false},
		{"ac3", audioProbe("ac3", "ac3", "fltp", 0, 6), false, false, AudioFullAAC, false, false},
		{"amr", audioProbe("amr", "amr_nb", "flt", 0, 1), false, false, AudioFullAAC, false, false},
		{"opus in webm", audioProbe("matroska,webm", "opus", "fltp", 0, 2), false, false, AudioFullAAC, false, false},
		{"mp3 in mp4", audioProbe("mov,mp4,m4a,3gp,3g2,mj2", "mp3", "fltp", 0, 2), true, false, AudioFullAAC, false, false},
	} {
		got := MakeAudioPlan(c.p, c.faststart)
		if got.Playable != c.playable || got.Full != c.full || got.Lossless != c.lossless || got.MeasurePeak != c.peak {
			t.Errorf("%s: %+v, want playable=%v full=%q lossless=%v peak=%v", c.name, got, c.playable, c.full, c.lossless, c.peak)
		}
		if (got.Full == AudioFullAAC) != (got.FullExt == "m4a") || (got.Full != "" && got.Full != AudioFullAAC && got.FullExt != "flac") {
			t.Errorf("%s: ext %q for %q", c.name, got.FullExt, got.Full)
		}
	}
}

// TestStripAudioArgs pins the strip's argv (§20 erratum 328, D4a, the round-2
// research §5.2): the ONE audio stream mapped explicitly — never `-map 0`,
// which would carry the cover art, a STREAM — with the metadata, the chapters
// and the encoder strings dropped, the mp3 muxer's ID3 off, and a FILE output.
func TestStripAudioArgs(t *testing.T) {
	p := audioProbe("mp3", "mp3", "fltp", 0, 2)
	p.AudioIndexes = []int{1} // the picture is stream 0
	tgt, ok := StripTargetFor(p)
	if !ok || tgt.Muxer != "mp3" || tgt.ContentType != "audio/mpeg" || tgt.Ext != "mp3" {
		t.Fatalf("mp3 target: %+v %v", tgt, ok)
	}
	args := StripAudioArgs(p, tgt, "/s/in.mp3", "/s/out.mp3", 0)
	got := strings.Join(args, " ")
	for _, want := range []string{"-map 0:1 -c copy", "-map_metadata -1", "-map_chapters -1",
		"-fflags +bitexact", "-flags +bitexact", "-id3v2_version 0", "-write_id3v1 0", "-f mp3 /s/out.mp3"} {
		if !strings.Contains(got, want) {
			t.Errorf("strip argv lacks %q: %s", want, got)
		}
	}
	if slices.Contains(args, "0") && strings.Contains(got, "-map 0 ") {
		t.Errorf("strip maps the whole input: %s", got)
	}
	if args[len(args)-1] == "-" || strings.HasPrefix(args[len(args)-1], "pipe:") {
		t.Errorf("strip writes to a pipe; the mp3 muxer's Xing frame needs a seekable FILE: %s", got)
	}
	for _, c := range []struct {
		format, codec, muxer, ext, ct string
	}{
		{"wav", "pcm_s24le", "wav", "wav", "audio/wav"},
		{"w64", "pcm_s24le", "wav", "wav", "audio/wav"},
		{"flac", "flac", "flac", "flac", "audio/flac"},
		{"ogg", "opus", "ogg", "ogg", "audio/ogg"},
		{"mov,mp4,m4a,3gp,3g2,mj2", "aac", "mp4", "m4a", "audio/mp4"},
		{"mov,mp4,m4a,3gp,3g2,mj2", "pcm_s16le", "mov", "mov", "audio/quicktime"},
		{"aac", "aac", "mp4", "m4a", "audio/mp4"},
		// Matroska/WebM is never written back (its muxers keep DURATION and
		// `encoder` under every strip flag): each codec moves into its clean box.
		{"matroska,webm", "opus", "ogg", "ogg", "audio/ogg"},
		{"matroska,webm", "vorbis", "ogg", "ogg", "audio/ogg"},
		{"matroska,webm", "flac", "flac", "flac", "audio/flac"},
		{"matroska,webm", "alac", "mp4", "m4a", "audio/mp4"},
		{"matroska,webm", "pcm_s16le", "wav", "wav", "audio/wav"},
		{"matroska,webm", "pcm_s16be", "aiff", "aiff", "audio/aiff"},
		{"aiff", "pcm_s16be", "aiff", "aiff", "audio/aiff"},
	} {
		tgt, ok := StripTargetFor(audioProbe(c.format, c.codec, "s16", 16, 2))
		if !ok || tgt.Muxer != c.muxer || tgt.Ext != c.ext || tgt.ContentType != c.ct {
			t.Errorf("%s/%s: %+v %v, want %s .%s %s", c.format, c.codec, tgt, ok, c.muxer, c.ext, c.ct)
		}
	}
	wav, _ := StripTargetFor(audioProbe("wav", "pcm_s16le", "s16", 16, 2))
	if !strings.Contains(strings.Join(wav.Opts, " "), "-rf64 auto") {
		t.Errorf("a WAV past 4 GB needs -rf64 auto: %v", wav.Opts)
	}
	if _, ok := StripTargetFor(&ProbeResult{Kind: KindVideo}); ok {
		t.Error("a video has no strip target")
	}
	for _, codec := range []string{"aac", "mp3", "ac3", "wavpack"} {
		if tgt, ok := StripTargetFor(audioProbe("matroska,webm", codec, "fltp", 0, 2)); ok {
			t.Errorf("%s in Matroska: %+v; any codec without a clean box is refused, never an .mka", codec, tgt)
		}
	}
}

// TestAudioFullArgs pins the Full's argv: the codec per plan, the gain only on
// the 24-bit path and only when negative, every write stripped and bitexact.
func TestAudioFullArgs(t *testing.T) {
	p := audioProbe("wav", "pcm_f32le", "flt", 32, 2)
	f24 := strings.Join(AudioFullArgs(p, AudioPlan{Full: AudioFullFLAC24}, -3.01, "/s/in.wav", "/s/o.flac", 0, 0), " ")
	for _, want := range []string{"-af volume=-3.0100dB", "-c:a flac -sample_fmt s32 -bits_per_raw_sample 24", "-f flac /s/o.flac", "-map_metadata -1", "-fflags +bitexact"} {
		if !strings.Contains(f24, want) {
			t.Errorf("flac24 argv lacks %q: %s", want, f24)
		}
	}
	if s := strings.Join(AudioFullArgs(p, AudioPlan{Full: AudioFullFLAC24}, 0, "i", "o", 0, 0), " "); strings.Contains(s, "volume=") {
		t.Errorf("no gain asked, a gain applied: %s", s)
	}
	exact := strings.Join(AudioFullArgs(p, AudioPlan{Full: AudioFullFLAC}, -3, "i", "o", 0, 0), " ")
	if strings.Contains(exact, "volume=") || strings.Contains(exact, "-sample_fmt") {
		t.Errorf("the bit-exact FLAC must not touch a sample: %s", exact)
	}
	aac := strings.Join(AudioFullArgs(p, AudioPlan{Full: AudioFullAAC}, 0, "i", "o", 0, 0), " ")
	if !strings.Contains(aac, "-c:a aac -b:a 256k") || !strings.Contains(aac, "-movflags +faststart -f mp4 o") {
		t.Errorf("aac argv: %s", aac)
	}
}

// TestAudioRunsPinTheAudiogramsThreads: the Full and the strip carry the
// audiogram's thread pin (erratum 329; run 10, lane nits10) — the filter pools
// before the input, `-threads` on the output — and threads 0 pins nothing.
func TestAudioRunsPinTheAudiogramsThreads(t *testing.T) {
	p := audioProbe("wav", "pcm_s16le", "s16", 16, 2)
	mp3 := audioProbe("mp3", "mp3", "fltp", 0, 2)
	tgt, _ := StripTargetFor(mp3)
	for name, argv := range map[string][2][]string{
		"the FLAC Full":   {AudioFullArgs(p, AudioPlan{Full: AudioFullFLAC}, 0, "i", "o", 0, 3), AudioFullArgs(p, AudioPlan{Full: AudioFullFLAC}, 0, "i", "o", 0, 0)},
		"the FLAC24 Full": {AudioFullArgs(p, AudioPlan{Full: AudioFullFLAC24}, -1, "i", "o", 0, 3), AudioFullArgs(p, AudioPlan{Full: AudioFullFLAC24}, -1, "i", "o", 0, 0)},
		"the AAC Full":    {AudioFullArgs(p, AudioPlan{Full: AudioFullAAC}, 0, "i", "o", 0, 3), AudioFullArgs(p, AudioPlan{Full: AudioFullAAC}, 0, "i", "o", 0, 0)},
		"the strip":       {StripAudioArgs(mp3, tgt, "i", "o", 3), StripAudioArgs(mp3, tgt, "i", "o", 0)},
	} {
		pinned, free := strings.Join(argv[0], " "), strings.Join(argv[1], " ")
		in := strings.Index(pinned, "-i i")
		pre := strings.Index(pinned, "-filter_threads 3 -filter_complex_threads 3")
		post := strings.Index(pinned, " -threads 3 ")
		if in < 0 || pre < 0 || pre > in || post < in || strings.Count(pinned, " -threads 3 ") != 1 {
			t.Errorf("%s is not pinned like the audiogram (the filter pools before the input, -threads on the output): %s", name, pinned)
		}
		if strings.Contains(free, "threads") {
			t.Errorf("%s with threads 0 pinned anyway: %s", name, free)
		}
	}
}

// TestFitGainDB pins the float headroom: none at or under full scale, just
// under it above (§5.3 of the round-2 research).
func TestFitGainDB(t *testing.T) {
	for _, c := range []struct{ peak, want float64 }{
		{-6, 0}, {0, 0}, {math.Inf(-1), 0}, {math.NaN(), 0}, {3, -3.01}, {0.5, -0.51},
	} {
		if got := FitGainDB(c.peak); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("FitGainDB(%v) = %v, want %v", c.peak, got, c.want)
		}
	}
}

// TestAudioLightBitrateK pins the card's fitting: 256k when it fits, the fit
// in between (95 % of the cap over the length, less the audiogram's 160k), never
// under 96k, 256k when the cap is off.
func TestAudioLightBitrateK(t *testing.T) {
	const mib = 1 << 20
	for _, c := range []struct {
		max  int64
		durS float64
		want int
	}{
		{25 * mib, 60, 256}, {25 * mib, 10 * 60, 172}, {25 * mib, 20 * 60, 96}, {25 * mib, 3 * 3600, 96},
		{0, 3600, 256}, {25 * mib, 0, 256},
	} {
		if got := AudioLightBitrateK(c.max, int64(c.durS*1e6)); got != c.want {
			t.Errorf("AudioLightBitrateK(%d, %vs) = %d, want %d", c.max, c.durS, got, c.want)
		}
	}
}

// TestAudioLightCopies pins D6a's copy: an AAC source that fits is carried as
// it is; an MP3 never (the card's verifier takes AAC alone), nor more than two
// channels, nor an AAC whose bytes would pass the cap.
func TestAudioLightCopies(t *testing.T) {
	const mib = 1 << 20
	min := int64(60_000_000)
	for _, c := range []struct {
		codec    string
		ch       int
		bytes    int64
		durUS    int64
		max      int64
		want     bool
		describe string
	}{
		{"aac", 2, 2 * mib, min, 25 * mib, true, "a minute of 256k AAC"},
		{"aac", 1, 2 * mib, min, 0, true, "no cap"},
		{"mp3", 2, 2 * mib, min, 25 * mib, false, "an MP3"},
		{"aac", 6, 2 * mib, min, 25 * mib, false, "5.1 AAC"},
		{"aac", 2, 24 * mib, min, 25 * mib, false, "an AAC past 95 % of the cap once the picture is counted"},
		{"flac", 2, mib, min, 25 * mib, false, "a FLAC"},
	} {
		if got := AudioLightCopies(c.codec, c.ch, c.bytes, c.durUS, c.max); got != c.want {
			t.Errorf("%s: AudioLightCopies = %v, want %v", c.describe, got, c.want)
		}
	}
}

// TestAudioLightArgs pins the audiogram's argv (§20 erratum 329): the sound
// first and the stencil looped at 30 fps, the research's filtergraph 2 with
// the duration a LITERAL in both fields and the overlay's x, -t beside
// -shortest (a container's claimed length is the uploader's), a keyframe every
// 5 s, BT.709 stamped, -map 0:a:0 (never -map 0: cover art is a stream), AAC
// 256k at 48 kHz or a copy, the output capped by -fs, stereo only past two
// channels, stripped and bitexact.
func TestAudioLightArgs(t *testing.T) {
	p := audioProbe("flac", "flac", "s32", 24, 6)
	args := AudioLightArgs(p, "/s/st.png", "/s/in.flac", "/s/l.mp4", 192, false, 1<<30, 0)
	got := strings.Join(args, " ")
	for _, want := range []string{
		"-i /s/in.flac -loop 1 -framerate 30 -protocol_whitelist file -i /s/st.png",
		"color=c=0xA9A59C:s=1280x720:r=30:d=4.000000,format=gbrp[rest]",
		"color=c=0xF0B24A:s=1280x720:r=30:d=4.000000,format=gbrp[done]",
		"[rest][done]overlay=x='80+1120*t/4.000000-w':y=0:eval=frame:format=gbrp[field]",
		"[1:v]format=gbrap[st];[field][st]overlay=0:0:shortest=1:format=gbrp,scale=out_color_matrix=bt709:out_range=tv,format=yuv420p,setparams=",
		"-map [v] -map 0:a:0", "-t 4.000000 -shortest",
		"-c:v libx264 -preset veryfast -crf 18 -g 150 -keyint_min 150 -sc_threshold 0 -r 30",
		"-colorspace bt709", "-c:a aac -b:a 192k -ar 48000", "-ac 2", "-map_metadata -1", "-map_chapters -1",
		"-fflags +bitexact", "-fs 1073741824", "-movflags +faststart -f mp4 /s/l.mp4"} {
		if !strings.Contains(got, want) {
			t.Errorf("audiogram argv lacks %q: %s", want, got)
		}
	}
	if strings.Contains(got, "-map 0 ") || strings.Contains(got, "loudnorm") {
		t.Errorf("audiogram argv maps the whole input or normalises: %s", got)
	}
	if full := strings.Join(AudioFullArgs(p, AudioPlan{Full: AudioFullAAC}, 0, "i", "o", 1<<30, 0), " "); !strings.Contains(full, "-fs 1073741824") {
		t.Errorf("the Full is not capped: %s", full)
	}
	if none := strings.Join(AudioFullArgs(p, AudioPlan{Full: AudioFullAAC}, 0, "i", "o", 0, 0), " "); strings.Contains(none, "-fs") {
		t.Errorf("no budget, a cap: %s", none)
	}
	if s := strings.Join(AudioLightArgs(audioProbe("flac", "flac", "s32", 24, 2), "p", "i", "o", 256, false, 0, 0), " "); strings.Contains(s, "-ac ") {
		t.Errorf("a stereo source is downmixed: %s", s)
	}
	cp := strings.Join(AudioLightArgs(audioProbe("mov,mp4,m4a,3gp,3g2,mj2", "aac", "fltp", 0, 2), "p", "i", "o", 256, true, 0, 0), " ")
	if !strings.Contains(cp, "-c:a copy") || strings.Contains(cp, "-c:a aac") || strings.Contains(cp, "-ar 48000") {
		t.Errorf("a copied AAC is re-encoded or resampled: %s", cp)
	}
	if pinned := strings.Join(AudioLightArgs(p, "p", "i", "o", 256, false, 0, 2), " "); !strings.Contains(pinned, "-y -filter_threads 2 -filter_complex_threads 2 -protocol_whitelist") ||
		!strings.Contains(pinned, "-colorspace bt709 -threads 2") {
		t.Errorf("the audiogram is not pinned to the quota's threads: %s", pinned)
	}
	if strings.Contains(got, "-threads") {
		t.Errorf("threads 0 pinned anyway: %s", got)
	}
	clean := strings.Join(CleanMP4Args("/s/e.mp4", "/s/l.mp4"), " ")
	if !strings.Contains(clean, "-map 0 -c copy") || !strings.Contains(clean, "-map_metadata -1") || !strings.Contains(clean, "-f mp4 /s/l.mp4") {
		t.Errorf("clean argv: %s", clean)
	}
}

// TestCheckAudioOutput pins the audio verdict: no picture, one sound, the
// codec the plan named, a duration.
func TestCheckAudioOutput(t *testing.T) {
	ok := &OutputInfo{AudioStreams: 1, AudioCodec: "flac", DurationMS: 4000}
	if err := CheckAudioOutput(ok, AudioCheck{Codec: "flac"}); err != nil {
		t.Fatalf("good: %v", err)
	}
	for name, info := range map[string]*OutputInfo{
		"a picture":   {VideoStreams: 1, AudioStreams: 1, AudioCodec: "flac", DurationMS: 4000},
		"two sounds":  {AudioStreams: 2, AudioCodec: "flac", DurationMS: 4000},
		"wrong codec": {AudioStreams: 1, AudioCodec: "aac", DurationMS: 4000},
		"no duration": {AudioStreams: 1, AudioCodec: "flac"},
		"nothing":     nil,
	} {
		if err := CheckAudioOutput(info, AudioCheck{Codec: "flac"}); !IsPermanent(err) {
			t.Errorf("%s: err = %v, want permanent bad_output", name, err)
		}
	}
}

// TestCheckStripped pins the strip's verdict on its own output.
func TestCheckStripped(t *testing.T) {
	src := audioProbe("mp3", "mp3", "fltp", 0, 2)
	clean := audioProbe("mp3", "mp3", "fltp", 0, 2)
	if err := CheckStripped(src, clean); err != nil {
		t.Fatalf("clean: %v", err)
	}
	tagged, pic, other := *clean, *clean, *clean
	tagged.PrivateTags, pic.CoverArt, other.AudioCodecs = true, true, []string{"aac"}
	// An output naming only its encoder — structural to the video remux, so
	// PrivateTags stays false — is still refused: the pin is "no encoder
	// anywhere", read from the probe document itself.
	enc, err := ParseProbe([]byte(`{"streams":[{"index":0,"codec_type":"audio","codec_name":"mp3","sample_rate":"48000",` +
		`"channels":2,"duration":"4.0"}],"format":{"format_name":"mp3","duration":"4.0","tags":{"encoder":"Lavf61.7.100"}}}`))
	if err != nil || enc.PrivateTags || !enc.EncoderTag {
		t.Fatalf("format.tags.encoder: %+v %v; want EncoderTag and no PrivateTags", enc, err)
	}
	streamEnc, err := ParseProbe([]byte(`{"streams":[{"index":0,"codec_type":"audio","codec_name":"mp3","sample_rate":"48000",` +
		`"channels":2,"duration":"4.0","tags":{"ENCODER":"Lavc libmp3lame"}}],"format":{"format_name":"mp3","duration":"4.0"}}`))
	if err != nil || !streamEnc.EncoderTag {
		t.Fatalf("stream ENCODER: %+v %v", streamEnc, err)
	}
	for name, out := range map[string]*ProbeResult{"tags": &tagged, "picture": &pic, "codec": &other,
		"a video": {Kind: KindVideo}, "format encoder": enc, "stream encoder": streamEnc} {
		if err := CheckStripped(src, out); !IsPermanent(err) {
			t.Errorf("%s: err = %v, want a permanent refusal", name, err)
		}
	}
}

// TestAudioTierLabel pins the owner's words.
func TestAudioTierLabel(t *testing.T) {
	for _, c := range []struct{ tier, codec, full, want string }{
		{TierOriginal, "flac", "", "Original · FLAC"}, {TierOriginal, "mp3", "", "Original · MP3"},
		{TierOriginal, "aac", "", "Original · AAC"}, {TierOriginal, "opus", "", "Original · Opus"},
		{TierFull, "", AudioFullFLAC, "Full · FLAC"}, {TierFull, "", AudioFullFLAC24, "Full · FLAC 24-bit"},
		{TierFull, "", AudioFullAAC, "Full · AAC"}, {TierLight, "", "", ""},
	} {
		if got := AudioTierLabel(c.tier, c.codec, c.full); got != c.want {
			t.Errorf("AudioTierLabel(%s,%s,%s) = %q, want %q", c.tier, c.codec, c.full, got, c.want)
		}
	}
}

// TestStrippedStereoMP3IsNotAName (§20 erratum 329): the bare `Lavf` the mp3
// muxer writes into a stereo stream's Xing/LAME field under +bitexact is not an
// encoder name the strip refuses; any other value, a versioned one, or `Lavf`
// anywhere else still is.
func TestStrippedStereoMP3IsNotAName(t *testing.T) {
	doc := func(format, codec, enc string, onFormat bool) []byte {
		st, ft := `{"encoder":"`+enc+`"}`, `{}`
		if onFormat {
			st, ft = `{}`, `{"encoder":"`+enc+`"}`
		}
		return []byte(`{"streams":[{"index":0,"codec_type":"audio","codec_name":"` + codec + `","channels":2,"sample_rate":"44100","tags":` + st +
			`}],"format":{"format_name":"` + format + `","duration":"6.0","tags":` + ft + `}}`)
	}
	for _, c := range []struct {
		format, codec, enc string
		onFormat, want     bool
	}{
		{"mp3", "mp3", "Lavf", false, false},
		{"mp3", "mp3", "Lavf61.7.100", false, true},
		{"mp3", "mp3", "LAME3.100", false, true},
		{"mp3", "mp3", "Lavf", true, true},
		{"flac", "flac", "Lavf", false, true},
	} {
		p, err := ParseProbe(doc(c.format, c.codec, c.enc, c.onFormat))
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		if p.EncoderTag != c.want {
			t.Errorf("%s %q (format tag %v): EncoderTag %v, want %v", c.format, c.enc, c.onFormat, p.EncoderTag, c.want)
		}
	}
}
