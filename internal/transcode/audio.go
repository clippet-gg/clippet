package transcode

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Audio clips (DESIGN.md §20 erratum 328; the owner's picks D1a–D8a, 2026-10-07).
//
// A clip can be SOUND. It is found by ParseProbe's second branch — a file with
// no non-cover video stream at all — and judged by its own closed allowlist,
// exactly as a video is by allowedFormats. Everything an audio clip is served
// as is decided here, in one place, so the ingest gate, the worker and the
// tests read one table:
//
//	| stored original (after the strip)     | Original tier      | Full                         |
//	|---------------------------------------|--------------------|------------------------------|
//	| MP3 in mp3                            | plays (MP3)        | —                            |
//	| AAC in mp4 (faststart)                | plays (AAC)        | —                            |
//	| Opus / Vorbis in Ogg                  | plays (Opus/Vorbis)| —                            |
//	| FLAC in flac                          | plays (FLAC)       | —                            |
//	| integer PCM ≤ 24-bit, ALAC, WavPack,  | download only      | FLAC, bit-exact (PCM hash)   |
//	|   TTA, TrueHD, FLAC in another box    |                    |                              |
//	| 32-bit float PCM, integer PCM > 24-bit| download only      | FLAC 24-bit, gain if > 0 dBFS|
//	| anything else (WMA, AC-3, AMR, MP3/AAC| download only      | AAC 256k                     |
//	|   in another box)                     |                    |                              |
//
// A Matroska/WebM upload is never stored as one: the strip moves Opus/Vorbis
// into Ogg (and it then plays), FLAC into flac, ALAC into mp4, PCM into WAV or
// AIFF, and refuses every other codec (StripTargetFor).
//
// DSD is refused (D8a); so is a file with two or more audio tracks, more than
// eight channels, or no duration.

// allowedAudioFormats is the closed set of demuxer aliases an AUDIO clip may
// arrive in, each with the container its stripped original is written back
// into (stripMuxers). It is the audio twin of allowedFormats and the same kind
// of list: an ALLOWLIST, because the formats that must never reach a demuxer
// here are the ones that name further resources. Monkey's Audio (`ape`) is
// absent on purpose: ffmpeg has no muxer for it, so its tags could not be
// stripped by a copy, and a file this server cannot strip is a file it does not
// publish. DSD (`dsf`, the `iff` DSDIFF) is absent because D8a refuses it.
var allowedAudioFormats = map[string]struct{}{
	"mp3": {}, "wav": {}, "w64": {}, "flac": {}, "aiff": {}, "caf": {}, "ogg": {},
	"mov": {}, "mp4": {}, "m4a": {}, "3gp": {}, "3g2": {}, "mj2": {},
	"matroska": {}, "webm": {}, "wv": {}, "tta": {}, "aac": {}, "ac3": {}, "eac3": {},
	"amr": {}, "asf": {},
}

// maxAudioChannels is the most channels an audio clip may carry: 7.1. A file
// past it is a multitrack session export rather than something to listen to,
// and nothing downstream (the card's stereo downmix, a browser's mixer) has a
// sensible answer for it.
const maxAudioChannels = 8

// isAllowedAudioContainer reports whether a format_name names a container an
// audio clip may arrive in (alias by alias, one hit is enough).
func isAllowedAudioContainer(formatName string) bool {
	return audioFamily(formatName) != ""
}

// audioFamily is the first allowlisted alias of a format_name, normalised so
// the ISO-BMFF and Matroska families each answer one word.
func audioFamily(formatName string) string {
	for _, name := range strings.Split(formatName, ",") {
		name = strings.TrimSpace(name)
		if _, ok := allowedAudioFormats[name]; !ok {
			continue
		}
		switch name {
		case "mov", "mp4", "m4a", "3gp", "3g2", "mj2":
			return "mov"
		case "matroska", "webm":
			return "matroska"
		}
		return name
	}
	return ""
}

// parseAudio is ParseProbe's second branch: a document with no non-cover video
// stream (§20 erratum 328). Every refusal is the permanent not_a_video shape
// the ingest gate answers 415 with — the code stays, the sentence a person reads
// is notify.SentenceNotVideo, which says "video or audio".
func parseAudio(doc ffprobeDoc, raw []byte) (*ProbeResult, error) {
	var audio []ffprobeStream
	cover := false
	for _, s := range doc.Streams {
		switch {
		case s.CodecType == "audio":
			audio = append(audio, s)
		case s.CodecType == "video" && s.Disposition["attached_pic"] == 1:
			cover = true
		}
	}
	if len(audio) == 0 {
		// Neither a picture nor a sound: the shape the gate has always called
		// no_video_stream, and the file is refused before its container is
		// even weighed.
		return nil, permanent("no_video_stream")
	}
	if !isAllowedAudioContainer(doc.Format.FormatName) {
		return nil, permanent("not_a_video: unsupported container (%s)", doc.Format.FormatName)
	}
	a := audio[0]
	switch {
	case len(audio) > 1:
		// REFUSED, not "take the first" (the erratum's decision): a voice and a
		// backing track in one file are two things, and quietly publishing one
		// of them is publishing something the person did not choose.
		return nil, permanent("not_a_video: %d audio tracks; an audio clip has one", len(audio))
	case strings.HasPrefix(a.CodecName, "dsd_"):
		return nil, permanent("not_a_video: DSD audio (%s)", a.CodecName)
	case strings.TrimSpace(a.CodecName) == "" || a.CodecName == "none":
		return nil, permanent("not_a_video: audio stream %d has no codec", a.Index)
	case a.Channels <= 0 || a.Channels > maxAudioChannels:
		return nil, permanent("not_a_video: %d audio channels", a.Channels)
	}
	p := &ProbeResult{
		Kind:            KindAudio,
		VideoIndex:      -1,
		AudioIndexes:    []int{a.Index},
		AudioCodecs:     []string{a.CodecName},
		FormatName:      doc.Format.FormatName,
		Raw:             copyRaw(raw),
		AudioChannels:   a.Channels,
		AudioSampleFmt:  strings.TrimSpace(a.SampleFmt),
		AudioSampleRate: atoiOr0(a.SampleRate),
		AudioBits:       audioBits(a),
		CoverArt:        cover,
	}
	if d, ok := parseSeconds(doc.Format.Duration); ok {
		p.HasDuration, p.DurationUS = true, secondsToUS(d)
	} else if d, ok := parseSeconds(a.Duration); ok {
		p.HasDuration, p.DurationUS = true, secondsToUS(d)
	}
	if !p.HasDuration || p.DurationUS <= 0 {
		return nil, permanent("not_a_video: the audio declares no duration")
	}
	p.Audio = extentOf(a)
	p.PrivateTags = hasPrivateTags(doc)
	p.EncoderTag = hasEncoderTag(doc)
	return p, nil
}

// hasEncoderTag reports whether the format or any stream carries a non-empty
// `encoder` tag (key compared lowercased and trimmed, privateTagIn's rule).
//
// One value is not a name (§20 erratum 329, found by its E2E): in an MP3 the
// stream's `encoder` is read out of the Xing/Info frame's LAME field, and when
// ffmpeg's mp3 muxer writes that frame for a STEREO stream under `+bitexact`
// it fills the field with the bare string `Lavf` — the muxer's own, no
// version, nothing of the uploader's (whose `LAME3.100` or `Lavc61.19` is
// gone). The strip must keep that frame (it is what gives a VBR file its
// duration and its seeking), so `Lavf` alone, on an mp3 stream, is structural;
// before this every stereo MP3 was refused 415 by the strip's own proof.
func hasEncoderTag(doc ffprobeDoc) bool {
	named := func(tags map[string]string, mp3 bool) bool {
		for k, v := range tags {
			v = strings.TrimSpace(v)
			if strings.ToLower(strings.TrimSpace(k)) != "encoder" || v == "" {
				continue
			}
			if mp3 && v == mp3XingEncoder {
				continue
			}
			return true
		}
		return false
	}
	if named(doc.Format.Tags, false) {
		return true
	}
	for _, s := range doc.Streams {
		if named(s.Tags, s.CodecName == "mp3" && audioFamily(doc.Format.FormatName) == "mp3") {
			return true
		}
	}
	return false
}

// mp3XingEncoder is the LAME field ffmpeg's mp3 muxer writes under +bitexact.
const mp3XingEncoder = "Lavf"

// copyRaw copies the document so the result does not alias the caller's
// buffer (ParseProbe's own rule for Raw).
func copyRaw(raw []byte) []byte { return append([]byte(nil), raw...) }

func atoiOr0(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// audioBits is the stream's bit depth: bits_per_raw_sample where the codec
// declares one (FLAC, ALAC, a 24-bit WAV decoded into s32), else
// bits_per_sample (raw PCM), else 0 — a lossy codec has no bit depth.
func audioBits(s ffprobeStream) int {
	if n := atoiOr0(s.BitsPerRawSample); n > 0 {
		return n
	}
	if s.BitsPerSample > 0 && (strings.HasPrefix(s.CodecName, "pcm_") || losslessCodecs[s.CodecName]) {
		return s.BitsPerSample
	}
	return 0
}

// losslessCodecs decode to exactly the samples that were encoded, so a FLAC
// of their decode is bit-exact and provable by the PCM hash (D1a). Raw PCM is
// recognised by its `pcm_` prefix and is not listed.
var losslessCodecs = map[string]bool{
	"flac": true, "alac": true, "wavpack": true, "tta": true, "mlp": true, "truehd": true,
}

// isFloatPCM reports whether the stream's samples are floating point — PCM
// (`pcm_f32le`, `pcm_f64be`, …) or a lossless codec carrying float (WavPack
// can). FLAC cannot hold float, so such a source gets the 24-bit Full (D2a).
func isFloatPCM(p *ProbeResult) bool {
	c := audioCodec(p, 0)
	if strings.HasPrefix(c, "pcm_f") {
		return true
	}
	if !losslessCodecs[c] {
		return false
	}
	switch p.AudioSampleFmt {
	case "flt", "fltp", "dbl", "dblp":
		return true
	}
	return false
}

// isLosslessSource reports whether the stream is PCM or a lossless codec.
func isLosslessSource(p *ProbeResult) bool {
	c := audioCodec(p, 0)
	return strings.HasPrefix(c, "pcm_") || losslessCodecs[c]
}

// StripTarget is the container a stripped audio original is written into: the
// muxer, the extension the stored key gets and the media type it is served as.
type StripTarget struct {
	Muxer, Ext, ContentType string
	// Opts are the muxer's own options, written before `-f`.
	Opts []string
}

// StripTargetFor names the container the strip writes for this probe (§20
// erratum 328). It is the source's own container wherever ffmpeg can write one
// with a copy, with three deliberate exceptions:
//
//   - the ISO-BMFF family becomes an MP4 (`audio/mp4`, `.m4a`) with `moov` in
//     front — the voice memo's `.m4a`, a `.mov` with AAC — except raw PCM, which
//     the mp4 muxer will not take and which stays a QuickTime file;
//   - a raw ADTS `.aac` becomes an MP4 too, which is what makes it PLAY;
//   - Wave64 becomes a WAV with `-rf64 auto`, the same samples in the container
//     every tool reads (an RF64 past 4 GB).
//
// ok=false means the probe is not an audio result this table knows.
func StripTargetFor(p *ProbeResult) (StripTarget, bool) {
	if p == nil || p.Kind != KindAudio {
		return StripTarget{}, false
	}
	codec := audioCodec(p, 0)
	switch audioFamily(p.FormatName) {
	case "mp3":
		// No ID3v2 and no ID3v1 at all, and the Xing/Info frame left on: the
		// muxer writes it only when its output is seekable, which is why every
		// strip writes to a FILE (§4.4 of the round-2 research).
		return StripTarget{Muxer: "mp3", Ext: "mp3", ContentType: "audio/mpeg",
			Opts: []string{"-id3v2_version", "0", "-write_id3v1", "0"}}, true
	case "wav", "w64":
		return StripTarget{Muxer: "wav", Ext: "wav", ContentType: "audio/wav", Opts: []string{"-rf64", "auto"}}, true
	case "flac":
		return StripTarget{Muxer: "flac", Ext: "flac", ContentType: "audio/flac"}, true
	case "aiff":
		return StripTarget{Muxer: "aiff", Ext: "aiff", ContentType: "audio/aiff"}, true
	case "caf":
		return StripTarget{Muxer: "caf", Ext: "caf", ContentType: "audio/x-caf"}, true
	case "ogg":
		return StripTarget{Muxer: "ogg", Ext: "ogg", ContentType: "audio/ogg"}, true
	case "mov", "aac":
		if strings.HasPrefix(codec, "pcm_") {
			return StripTarget{Muxer: "mov", Ext: "mov", ContentType: "audio/quicktime",
				Opts: []string{"-movflags", "+faststart"}}, true
		}
		return StripTarget{Muxer: "mp4", Ext: "m4a", ContentType: "audio/mp4",
			Opts: []string{"-movflags", "+faststart"}}, true
	case "matroska":
		// NEVER the matroska/webm muxers: they write a per-stream DURATION tag
		// and the format's `encoder=Lavf` even under `-map_metadata -1` and
		// bitexact (measured, ffmpeg 7.1.1), so their output could never pass
		// the pin "no tag, no encoder anywhere". The packets move into the clean
		// container their codec has — which for Opus/Vorbis (a browser's
		// MediaRecorder) is Ogg, and then the Original PLAYS. Every other codec
		// in Matroska is refused rather than written back into an `.mka`.
		switch {
		case codec == "opus" || codec == "vorbis":
			return StripTarget{Muxer: "ogg", Ext: "ogg", ContentType: "audio/ogg"}, true
		case codec == "flac":
			return StripTarget{Muxer: "flac", Ext: "flac", ContentType: "audio/flac"}, true
		case codec == "alac":
			return StripTarget{Muxer: "mp4", Ext: "m4a", ContentType: "audio/mp4",
				Opts: []string{"-movflags", "+faststart"}}, true
		case strings.HasPrefix(codec, "pcm_") && strings.HasSuffix(codec, "be"):
			// The wav muxer takes no big-endian PCM; AIFF is big-endian PCM.
			return StripTarget{Muxer: "aiff", Ext: "aiff", ContentType: "audio/aiff"}, true
		case strings.HasPrefix(codec, "pcm_"):
			return StripTarget{Muxer: "wav", Ext: "wav", ContentType: "audio/wav", Opts: []string{"-rf64", "auto"}}, true
		}
		return StripTarget{}, false
	case "wv":
		return StripTarget{Muxer: "wv", Ext: "wv", ContentType: "audio/x-wavpack"}, true
	case "tta":
		return StripTarget{Muxer: "tta", Ext: "tta", ContentType: "audio/x-tta"}, true
	case "ac3":
		return StripTarget{Muxer: "ac3", Ext: "ac3", ContentType: "audio/ac3"}, true
	case "eac3":
		return StripTarget{Muxer: "eac3", Ext: "eac3", ContentType: "audio/eac3"}, true
	case "amr":
		return StripTarget{Muxer: "amr", Ext: "amr", ContentType: "audio/amr"}, true
	case "asf":
		return StripTarget{Muxer: "asf", Ext: "wma", ContentType: "audio/x-ms-wma"}, true
	}
	return StripTarget{}, false
}

// bitexactArgs are written on EVERY ffmpeg write of an audio clip's file: no
// `Lavf…` encoder string, no LAME/Xing encoder tag, no vendor string, no
// creation time — and therefore a reproducible output, which is what lets two
// uploads of one file be recognised as one after their strip (erratum 326).
var bitexactArgs = []string{"-fflags", "+bitexact", "-flags", "+bitexact"}

// CleanMP4Args renders the second pass every ENCODED audio card copy takes: a
// `-c copy` of the whole file with the tags dropped. It exists for one tag the
// first pass cannot drop — the ffmpeg CLI names the H.264 track's encoder
// ("Lavc libx264") in the mp4's compressor name whatever `-metadata` says, and
// it does so only for a stream it ENCODES; a copy writes none (measured, ffmpeg
// 7.1.1). The pass is the cost of the pin "no encoder anywhere".
func CleanMP4Args(in, out string) []string {
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-nostats", "-y"}
	args = append(args, inputArgs(in)...)
	args = append(args, "-map", "0", "-c", "copy")
	args = append(args, stripArgs...)
	args = append(args, bitexactArgs...)
	return append(args, "-movflags", "+faststart", "-f", "mp4", out)
}

// stripArgs is what every audio write drops: the container's tags, every
// stream's tags, the chapters. Cover art is not dropped here — it is a STREAM,
// and every audio argv maps the one audio stream explicitly, never `-map 0`.
var stripArgs = []string{"-map_metadata", "-1", "-map_chapters", "-1", "-sn", "-dn"}

// StripAudioArgs renders the strip: `-c copy` of the ONE audio stream into the
// target container with every tag, chapter, picture and encoder string left
// behind (§20 erratum 328, D4a). For PCM it is bit-exact (the decoded-PCM hash
// proves it); for everything else it is the same packets in a fresh box.
// `threads` > 0 pins it as threadArgs says.
func StripAudioArgs(p *ProbeResult, t StripTarget, in, out string, threads int) []string {
	pre, post := threadArgs(threads)
	args := append([]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-nostats", "-y"}, pre...)
	args = append(args, inputArgs(in)...)
	args = append(args, "-map", "0:"+strconv.Itoa(p.AudioIndexes[0]), "-c", "copy")
	args = append(args, post...)
	args = append(args, stripArgs...)
	args = append(args, bitexactArgs...)
	args = append(args, t.Opts...)
	return append(args, "-f", t.Muxer, out)
}

// StripAudio runs the strip into the FILE `out` (never a pipe: the mp3 muxer
// writes its Xing/Info frame only into a seekable output). Outcomes match
// Remux's.
func StripAudio(ctx context.Context, ffmpeg string, p *ProbeResult, in, out string, threads int) (StripTarget, error) {
	t, ok := StripTargetFor(p)
	if !ok {
		return StripTarget{}, permanent("not_a_video: no container to strip %q into", p.FormatName)
	}
	_, se, err := runCapture(ctx, ffmpeg, StripAudioArgs(p, t, in, out, threads)...)
	if err != nil {
		if ctx.Err() != nil {
			return t, err
		}
		reason := exitReason("ffmpeg", err, se)
		if isExit(err) {
			return t, permanent("%s", reason)
		}
		return t, fmt.Errorf("%s", reason)
	}
	return t, nil
}

// Audio Full codecs, as clips.full_codec stores them.
const (
	AudioFullFLAC   = "flac"   // bit-exact FLAC of an integer source ≤ 24-bit
	AudioFullFLAC24 = "flac24" // 24-bit FLAC of a float (or > 24-bit) source — never called lossless
	AudioFullAAC    = "aac"    // AAC 256k of a source no browser plays
)

// AudioFullBitrateK is the AAC Full's bitrate (D8a): transparent for every
// source this path takes, all of which were lossy to begin with.
const AudioFullBitrateK = 256

// Audio playability reasons (operator text, probe.json and the log).
const (
	NotPlayableAudioContainer = "the audio's codec and container are not one every browser plays"
	NotPlayableAudioLossless  = "lossless PCM plays as a FLAC Full"
)

// AudioPlan is the tier decision of one audio clip (§20 erratum 328).
type AudioPlan struct {
	// Playable says the stored original plays in every current browser as it
	// stands; it is then the Original tier and there is no Full.
	Playable bool
	Reason   string
	// Full is "" or one of AudioFull*; FullExt the rendition's extension.
	Full, FullExt string
	// Lossless says the Full must decode to EXACTLY the source's samples, and
	// the worker proves it with the PCM hash before the Full is published.
	Lossless bool
	// MeasurePeak says the source is float and the Full's gain is decided by
	// its measured peak (a float file may hold samples past full scale).
	MeasurePeak bool
}

// OriginalAudioLabel is the codec word an audio Original is labelled with.
func OriginalAudioLabel(codec string) string {
	switch codec {
	case "mp3":
		return "MP3"
	case "aac":
		return "AAC"
	case "opus":
		return "Opus"
	case "vorbis":
		return "Vorbis"
	case "flac":
		return "FLAC"
	}
	return strings.ToUpper(codec)
}

// MakeAudioPlan decides an audio clip's tiers from the probe of its STORED
// (already stripped) original. `faststart` is FaststartMP4 of that file, which
// matters only for AAC. A FLAC deeper than 24 bits is not played as it stands
// (run 10, lane nits10): no browser is promised to decode one, so a 32-bit
// integer FLAC gets the 24-bit Full a 32-bit WAV gets, and the page plays that
// (fillAudioTiers: the Full whenever the Original is not playable).
func MakeAudioPlan(p *ProbeResult, faststart bool) AudioPlan {
	if p == nil || p.Kind != KindAudio {
		return AudioPlan{Reason: NotPlayableAudioContainer}
	}
	codec, fam := audioCodec(p, 0), audioFamily(p.FormatName)
	switch {
	case codec == "mp3" && fam == "mp3",
		codec == "aac" && fam == "mov" && faststart,
		(codec == "opus" || codec == "vorbis") && fam == "ogg",
		codec == "flac" && fam == "flac" && !isFloatPCM(p) && p.AudioBits <= 24:
		return AudioPlan{Playable: true}
	case isLosslessSource(p) && (isFloatPCM(p) || p.AudioBits > 24):
		return AudioPlan{Reason: NotPlayableAudioLossless, Full: AudioFullFLAC24, FullExt: "flac", MeasurePeak: isFloatPCM(p)}
	case isLosslessSource(p):
		return AudioPlan{Reason: NotPlayableAudioLossless, Full: AudioFullFLAC, FullExt: "flac", Lossless: true}
	}
	return AudioPlan{Reason: NotPlayableAudioContainer, Full: AudioFullAAC, FullExt: "m4a"}
}

// AudioFullArgs renders the Full encode of an audio clip: the one audio stream,
// stripped and bitexact, as FLAC (bit-exact, or 24-bit with `gainDB` applied
// first when it is negative) or as AAC 256k in a faststart MP4. `maxBytes` > 0 is
// ffmpeg's `-fs`: the encode stops writing there (the caller measures the file
// and fails a run that reached it — outputBudget's rule, applied to sound).
// `threads` > 0 pins it as threadArgs says.
func AudioFullArgs(p *ProbeResult, plan AudioPlan, gainDB float64, in, out string, maxBytes int64, threads int) []string {
	pre, post := threadArgs(threads)
	args := append([]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-nostats", "-y"}, pre...)
	args = append(args, inputArgs(in)...)
	args = append(args, "-map", "0:"+strconv.Itoa(p.AudioIndexes[0]))
	switch plan.Full {
	case AudioFullFLAC24:
		if gainDB < 0 {
			args = append(args, "-af", "volume="+strconv.FormatFloat(gainDB, 'f', 4, 64)+"dB")
		}
		args = append(args, "-c:a", "flac", "-sample_fmt", "s32", "-bits_per_raw_sample", "24")
	case AudioFullFLAC:
		args = append(args, "-c:a", "flac")
	default:
		args = append(args, "-c:a", "aac", "-b:a", strconv.Itoa(AudioFullBitrateK)+"k")
	}
	args = append(args, post...)
	args = append(args, stripArgs...)
	args = append(args, bitexactArgs...)
	args = append(args, fsArgs(maxBytes)...)
	if plan.Full == AudioFullAAC {
		return append(args, "-movflags", "+faststart", "-f", "mp4", out)
	}
	return append(args, "-f", "flac", out)
}

// FitGainDB is the gain a float source's 24-bit Full is written with: zero when
// its peak is at or under full scale, otherwise the negative gain that brings
// the peak to just under it — a hundredth of a decibel of headroom, so the
// integer conversion never saturates a sample (round-2 research §5.3). The label
// already says "24-bit", not lossless; clipping would make it worse.
func FitGainDB(peakDB float64) float64 {
	if math.IsNaN(peakDB) || math.IsInf(peakDB, 0) || peakDB <= 0 {
		return 0
	}
	return -(peakDB + 0.01)
}

// The audio Light (the card copy): the AUDIOGRAM (§20 erratum 329) — the
// clip's waveform, its played bars turning amber left to right over the clip's
// length, under the clip's sound.
const (
	// AudioLightFPS is the audiogram's rate: a CONSTANT 30, taken off the list of
	// variables (the round-2 research §2 found no source on how Discord's phone
	// clients treat a 1–2 fps card, and a flat picture costs almost nothing per
	// frame), with a keyframe every AudioLightGOP frames — 5 s — so a seek is
	// cheap.
	AudioLightFPS = 30
	AudioLightGOP = 150
	// AudioLightCRF is the x264 quality. The research's 30 was measured here
	// (lane audiocard, ffmpeg 7.1.1, a minute of a busy waveform): 22 kbps, but a
	// 10 px bar's chroma (4:2:0) drifted up to ~20 levels off the poster's — the
	// amber went yellow. 18 holds the bars within ~6 levels of the poster at
	// ~120 kbps, which is what frame 0 IS the poster needs.
	AudioLightCRF = 18
	// AudioLightMaxK and AudioLightMinK bound the card's AAC (D6a): 256k when it
	// fits, never under 96k — a long clip whose 96k card is past
	// SOCIAL_MAX_BYTES keeps its card anyway, because an audio clip has no
	// other mp4 a card could point at.
	AudioLightMaxK = 256
	AudioLightMinK = 96
	// audioLightPictureK is the picture's allowance in the fitting: the
	// audiogram's H.264 at AudioLightCRF measured ~120 kbps over a minute of a
	// busy waveform at 1280×720/30 (lane audiocard, ffmpeg 7.1.1), with room.
	audioLightPictureK = 160
)

// AudioLightBitrateK fits the card's AAC to SOCIAL_MAX_BYTES the way the video
// Light is fitted: 95 % of the cap over the duration, less the picture, clamped
// to [96k, 256k]. A cap of 0 (the Light tier switched off for video) still
// gets the 256k card: an audio clip has nothing else to put on one.
func AudioLightBitrateK(maxBytes, durationUS int64) int {
	if maxBytes <= 0 || durationUS <= 0 {
		return AudioLightMaxK
	}
	sec := float64(durationUS) / 1e6
	k := int(float64(maxBytes)*0.95*8/sec/1000) - audioLightPictureK
	return min(max(k, AudioLightMinK), AudioLightMaxK)
}

// AudioLightCopies says the card may carry the sound as it is (`-c:a copy`,
// D6a): the file is already AAC in no more than two channels and its bytes,
// with the picture's allowance, fit 95 % of SOCIAL_MAX_BYTES (any size fits a
// cap of 0). An MP3 is never copied — the card's verifier takes AAC alone.
// `audioBytes` is the size of the file the sound comes from.
func AudioLightCopies(codec string, channels int, audioBytes, durationUS, maxBytes int64) bool {
	if codec != "aac" || channels < 1 || channels > 2 || audioBytes <= 0 || durationUS <= 0 {
		return false
	}
	if maxBytes <= 0 {
		return true
	}
	picture := float64(audioLightPictureK) * 1000 / 8 * float64(durationUS) / 1e6
	return float64(audioBytes)+picture <= float64(maxBytes)*0.95
}

// audioLightFilter is the audiogram's filtergraph (the round-2 research's
// filtergraph 2, as built): two flat fields — the unplayed colour, and the
// played colour sliding in from the left so its right edge reaches the band's
// right edge exactly at DUR — composited in planar RGB, the stencil (input 1,
// the ink tile with the bars cut out) laid over them, then ONE conversion to
// BT.709 limited-range yuv420p, stamped as such. No expression is evaluated per
// pixel: the only per-frame expression is the overlay's x.
func audioLightFilter(dur string) string {
	size := strconv.Itoa(WaveFrameW) + "x" + strconv.Itoa(WaveFrameH)
	fps := strconv.Itoa(AudioLightFPS)
	field := func(hex string) string {
		return "color=c=0x" + hex[1:] + ":s=" + size + ":r=" + fps + ":d=" + dur + ",format=gbrp"
	}
	return field(WaveRestHex) + "[rest];" +
		field(WavePlayedHex) + "[done];" +
		"[rest][done]overlay=x='" + strconv.Itoa(WaveBandX) + "+" + strconv.Itoa(WaveBandW) + "*t/" + dur +
		"-w':y=0:eval=frame:format=gbrp[field];" +
		"[1:v]format=gbrap[st];" +
		"[field][st]overlay=0:0:shortest=1:format=gbrp," +
		"scale=out_color_matrix=bt709:out_range=tv,format=yuv420p" + colourStamp + "[v]"
}

// AudioLightArgs renders the audio card copy, the audiogram: the sound (input
// 0, its one audio stream) under audioLightFilter's picture, the `stencil` PNG
// looped as input 1. H.264 at AudioLightFPS with a keyframe every
// AudioLightGOP frames, and AAC-LC 48 kHz at `bitrateK` — or the sound as it
// is when `copyAudio` (AudioLightCopies) — in a faststart MP4. The duration is
// the probed seconds written in as a LITERAL, in the picture's fields and as
// `-t`, AND `-shortest` beside it, so the output ends at whichever comes first:
// the declared length, or the sound itself — a container that claims a
// thousand hours over a minute of audio must not draw the picture for a
// thousand hours. Stereo only when the source has more than two channels;
// `maxBytes` is `-fs`, as above. No loudness normalisation (erratum 329).
//
// `threads` > 0 pins the filter pools and the encoder to that many threads,
// the video encodes' rule (§20 erratum 215): under a CPU quota an unpinned
// ffmpeg spawns a thread per HOST core and is throttled — measured here, 38 s of
// encoding per minute of sound at APP_CPUS=1 unpinned.
func AudioLightArgs(p *ProbeResult, stencil, in, out string, bitrateK int, copyAudio bool, maxBytes int64, threads int) []string {
	fps := strconv.Itoa(AudioLightFPS)
	dur := strconv.FormatFloat(float64(p.DurationUS)/1e6, 'f', 6, 64)
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-nostats", "-y"}
	if threads > 0 {
		n := strconv.Itoa(threads)
		args = append(args, "-filter_threads", n, "-filter_complex_threads", n)
	}
	args = append(args, inputArgs(in)...)
	args = append(args, "-loop", "1", "-framerate", fps)
	args = append(args, inputArgs(stencil)...)
	args = append(args, "-filter_complex", audioLightFilter(dur), "-map", "[v]", "-map", "0:a:0",
		"-t", dur, "-shortest",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", strconv.Itoa(AudioLightCRF),
		"-g", strconv.Itoa(AudioLightGOP), "-keyint_min", strconv.Itoa(AudioLightGOP), "-sc_threshold", "0", "-r", fps,
		"-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709")
	if threads > 0 {
		args = append(args, "-threads", strconv.Itoa(threads))
	}
	if copyAudio {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-b:a", strconv.Itoa(bitrateK)+"k", "-ar", "48000")
		if p.AudioChannels > 2 {
			args = append(args, "-ac", "2")
		}
	}
	args = append(args, stripArgs...)
	args = append(args, bitexactArgs...)
	args = append(args, fsArgs(maxBytes)...)
	return append(args, "-movflags", "+faststart", "-f", "mp4", out)
}

// threadArgs is the audiogram's thread pin (AudioLightArgs, erratum 329) for the
// other audio runs, the Full and the strip (run 10, lane nits10): `pre` goes
// before the first input (the filter pools), `post` on the output (the codecs).
// Under a CPU quota an unpinned ffmpeg sizes its pools by the HOST's cores and
// is throttled. The codecs these two runs use are mostly single-threaded and
// the strip is a copy, so here the pin is a cap more than a speed-up: one rule
// for every audio run, so none of them oversubscribes a quota. threads <= 0 is
// nothing (ffmpeg's own choice), as in AudioLightArgs.
func threadArgs(threads int) (pre, post []string) {
	if threads <= 0 {
		return nil, nil
	}
	n := strconv.Itoa(threads)
	return []string{"-filter_threads", n, "-filter_complex_threads", n}, []string{"-threads", n}
}

// fsArgs is ffmpeg's output size limit, or nothing.
func fsArgs(maxBytes int64) []string {
	if maxBytes <= 0 {
		return nil
	}
	return []string{"-fs", strconv.FormatInt(maxBytes, 10)}
}

// RunAudio runs one of the argv builders above. Outcomes match Remux's: nil on
// exit 0, ctx.Err() when the caller cancelled, ErrPermanent for a non-zero exit
// (the file is at fault).
func RunAudio(ctx context.Context, ffmpeg string, args []string) error {
	_, se, err := runCapture(ctx, ffmpeg, args...)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		reason := exitReason("ffmpeg", err, se)
		if isExit(err) {
			return permanent("%s", reason)
		}
		return fmt.Errorf("%s", reason)
	}
	return nil
}

// PCMHashArgs renders the decoded-PCM hash of a file's first audio stream:
// sha256 over its samples as 32-bit integers (or 32-bit floats). The depth is
// deliberately ≥ every integer source's — the hash muxer's default is 16-bit,
// which would hide a 24-bit loss (round-1 research, the proof recipe).
func PCMHashArgs(in string, float bool) []string {
	codec := "pcm_s32le"
	if float {
		codec = "pcm_f32le"
	}
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-nostats"}
	args = append(args, inputArgs(in)...)
	return append(args, "-map", "0:a:0", "-c:a", codec, "-f", "hash", "-hash", "sha256", "-")
}

var pcmHashLine = regexp.MustCompile(`SHA256=([0-9a-f]{64})`)

// PCMHash runs PCMHashArgs and returns the hex digest.
func PCMHash(ctx context.Context, ffmpeg, in string, float bool) (string, error) {
	out, se, err := runCapture(ctx, ffmpeg, PCMHashArgs(in, float)...)
	if err != nil {
		if ctx.Err() != nil {
			return "", err
		}
		reason := exitReason("ffmpeg", err, se)
		if isExit(err) {
			return "", permanent("%s", reason)
		}
		return "", fmt.Errorf("%s", reason)
	}
	m := pcmHashLine.FindSubmatch(out)
	if m == nil {
		return "", permanent("bad_output: no PCM hash in %q", strings.TrimSpace(string(out)))
	}
	return string(m[1]), nil
}

// SamePCM is the lossless proof (D1a): the decoded samples of a and b hash
// equal. Any difference is a permanent bad_output — a FLAC that is not
// bit-exact is a Full that lies about its name.
func SamePCM(ctx context.Context, ffmpeg, a, b string) error {
	ha, err := PCMHash(ctx, ffmpeg, a, false)
	if err != nil {
		return err
	}
	hb, err := PCMHash(ctx, ffmpeg, b, false)
	if err != nil {
		return err
	}
	if ha != hb {
		return permanent("bad_output: the lossless copy's samples differ from the source's")
	}
	return nil
}

// PeakArgs renders astats' overall peak measurement of the first audio stream.
func PeakArgs(in string) []string {
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "info", "-nostats"}
	args = append(args, inputArgs(in)...)
	return append(args, "-map", "0:a:0", "-af", "astats=measure_overall=Peak_level:measure_perchannel=none",
		"-f", "null", "-")
}

var peakLine = regexp.MustCompile(`Peak level dB:\s*(-?inf|-?[0-9.]+)`)

// PeakDB measures the file's overall sample peak in dBFS (0 = full scale).
// `-inf` (digital silence) is reported as math.Inf(-1).
func PeakDB(ctx context.Context, ffmpeg, in string) (float64, error) {
	_, se, err := runCapture(ctx, ffmpeg, PeakArgs(in)...)
	if err != nil {
		if ctx.Err() != nil {
			return 0, err
		}
		reason := exitReason("ffmpeg", err, se)
		if isExit(err) {
			return 0, permanent("%s", reason)
		}
		return 0, fmt.Errorf("%s", reason)
	}
	ms := peakLine.FindAllStringSubmatch(se.All(), -1)
	if len(ms) == 0 {
		return 0, permanent("bad_output: no peak level in the astats output")
	}
	v := ms[len(ms)-1][1]
	if strings.HasSuffix(v, "inf") {
		return math.Inf(-1), nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, permanent("bad_output: peak level %q", v)
	}
	return f, nil
}

// AudioCheck is what a produced audio Full must be.
type AudioCheck struct {
	// Codec is the one acceptable audio codec name ("flac", "aac").
	Codec string
}

// CheckAudioOutput applies the audio verdict to a measured Full: NO video
// stream, exactly one audio stream, the expected codec and a positive duration.
func CheckAudioOutput(info *OutputInfo, want AudioCheck) error {
	switch {
	case info == nil:
		return permanent("bad_output: no probe result")
	case info.VideoStreams != 0:
		return permanent("bad_output: want no video stream in an audio tier, got %d", info.VideoStreams)
	case info.AudioStreams != 1:
		return permanent("bad_output: want exactly one audio stream, got %d", info.AudioStreams)
	case info.AudioCodec != want.Codec:
		return permanent("bad_output: audio codec %q, want %s", info.AudioCodec, want.Codec)
	case info.DurationMS <= 0:
		return permanent("bad_output: duration %dms not positive", info.DurationMS)
	}
	return nil
}

// AudioPoster writes an audio clip's poster (§20 erratum 329): the waveform of
// `heights` with the mark, as a PNG — flat colour and hard edges, which a PNG
// keeps exactly and a JPEG rings around — and then burns the clip's duration
// into its top-right corner exactly as every video poster's (erratum 287,
// drawPosterLabel; a label that cannot be drawn leaves the picture as it was).
// The size is the card frame's.
func AudioPoster(ctx context.Context, ffmpeg string, heights []uint8, mark []byte, out string, label PosterLabel) (PosterResult, error) {
	img, err := WavePoster(heights, mark)
	if err != nil {
		return PosterResult{}, fmt.Errorf("audio poster: %w", err)
	}
	if err := WritePNG(img, out); err != nil {
		return PosterResult{}, fmt.Errorf("audio poster: %w", err)
	}
	drawPosterLabel(ctx, ffmpeg, out, label, WaveFrameH)
	return PosterResult{Width: WaveFrameW, Height: WaveFrameH}, nil
}

// SamePCMAs is SamePCM with the hash depth chosen: float sources are hashed as
// 32-bit floats, so a copy of a float WAV is compared sample for sample rather
// than through an integer conversion that would saturate past full scale.
func SamePCMAs(ctx context.Context, ffmpeg, a, b string, float bool) error {
	ha, err := PCMHash(ctx, ffmpeg, a, float)
	if err != nil {
		return err
	}
	hb, err := PCMHash(ctx, ffmpeg, b, float)
	if err != nil {
		return err
	}
	if ha != hb {
		return permanent("bad_output: the stripped copy's samples differ from the upload's")
	}
	return nil
}

// StripVerified is the ingest's strip (§20 erratum 328), as a function the
// HTTP layer is handed: StripAudio, then the proof that what it wrote is what
// it should be — it probes as the SAME one audio stream in the same codec,
// carries no tag beyond a container's structural keys and no picture, and, on
// every lossless path, decodes to exactly the upload's samples. Any miss is a
// permanent refusal: an upload this server cannot strip provably is one it does
// not publish.
//
// `threads` is ResolveThreads' value, the worker's own: the strip runs in the
// API process, under the same CPU quota as every encode.
func StripVerified(ffmpeg, ffprobe string, threads int) func(ctx context.Context, p *ProbeResult, in, out string) (StripTarget, error) {
	return func(ctx context.Context, p *ProbeResult, in, out string) (StripTarget, error) {
		t, err := StripAudio(ctx, ffmpeg, p, in, out, threads)
		if err != nil {
			return t, err
		}
		raw, err := runFFprobe(ctx, ffprobe, out)
		if err != nil {
			return t, err
		}
		q, err := ParseProbe(raw)
		if err != nil {
			return t, permanent("not_a_video: the stripped copy does not probe: %v", err)
		}
		if err := CheckStripped(p, q); err != nil {
			return t, err
		}
		if isLosslessSource(p) {
			if err := SamePCMAs(ctx, ffmpeg, in, out, isFloatPCM(p)); err != nil {
				return t, err
			}
		}
		return t, nil
	}
}

// CheckStripped is the strip's verdict on its own output, from the two probes:
// one audio stream, the upload's codec, no private tag, no `encoder` tag, no
// picture.
func CheckStripped(src, out *ProbeResult) error {
	switch {
	case out == nil || out.Kind != KindAudio:
		return permanent("not_a_video: the stripped copy is not audio")
	case audioCodec(out, 0) != audioCodec(src, 0):
		return permanent("not_a_video: the stripped copy is %q, the upload %q", audioCodec(out, 0), audioCodec(src, 0))
	case out.PrivateTags:
		return permanent("not_a_video: the stripped copy still carries tags")
	case out.EncoderTag:
		// Stricter than the video remux's fixed point, which allows `encoder`:
		// a served audio copy names no program (D4a).
		return permanent("not_a_video: the stripped copy still names an encoder")
	case out.CoverArt:
		return permanent("not_a_video: the stripped copy still carries a picture")
	}
	return nil
}

// AudioTierLabel is the quality control's label for an audio tier (§20
// erratum 328; the owner's words): `Original · FLAC`, `Original · MP3`, `Original ·
// AAC`, `Original · Opus`, `Full · FLAC`, `Full · FLAC 24-bit`, `Full · AAC`.
// `tier` is "original" or "full"; `codec` is the original's codec name and
// `fullCodec` the stored AudioFull* word. Anything else is "".
func AudioTierLabel(tier, codec, fullCodec string) string {
	switch tier {
	case TierOriginal:
		return "Original · " + OriginalAudioLabel(codec)
	case TierFull:
		switch fullCodec {
		case AudioFullFLAC:
			return "Full · FLAC"
		case AudioFullFLAC24:
			return "Full · FLAC 24-bit"
		case AudioFullAAC:
			return "Full · AAC"
		}
	}
	return ""
}
