//go:build ffmpeg

package transcode

// §20 erratum 332 on real ffmpeg: the card's bars are LOUDNESS (RMS per bar,
// both channels, through the curve), read from the decode PeaksArgs renders, on
// three fixtures made here the way scripts/testclips.sh makes its own (lavfi,
// deterministic, small):
//
//   - edm.wav: the brick an unmastered track drew (2026-10-08) — a quiet
//     intro, then a full-scale burst every 0.25 s, so EVERY bar of the 20 s
//     file holds a full-scale peak, over a bed that drops for a breakdown;
//   - antiphase.wav: stereo, R = −L, which a mono downmix cancels to silence;
//   - longheader.flac: 10 s of sound whose header claims an hour. A FLAC,
//     not a WAV: ffmpeg 7.1's WAV demuxer measures a data chunk that
//     outruns the file from the file's size ("Estimating duration from
//     bitrate"), so a WAV cannot overstate; FLAC's STREAMINFO total is
//     believed, as an MP3's Xing count is.

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// The fixtures' lavfi recipes. The bursts come every 0.25 s because a bar of a
// 20 s file is 0.29 s: at the brief's 0.5 s some bars would hold no burst and
// the peak reading would already show a shape. Each burst is 20 ms of a
// full-scale 50 Hz square (±1.0, saturating to ±32767/−32768), the bed a sine
// at −6 dBFS that falls to −18 dBFS from 10 to 14 s (a breakdown with the
// kick still at full scale); the intro is 2 s of a sine at −30 dBFS.
const (
	waveEDMRecipe = "aevalsrc='if(lt(t,2),0.0316*sin(2*PI*440*t)," +
		"if(lt(mod(t-2,0.25),0.02),1-2*lt(mod(t,0.02),0.01)," +
		"if(between(t,10,14),0.125,0.5)*sin(2*PI*110*t)))':s=44100:d=20"
	waveAntiphaseRecipe = "aevalsrc='0.5*sin(2*PI*440*t)|-0.5*sin(2*PI*440*t)':s=44100:d=4"
	// A sine that rises from nothing to −1 dBFS over 10 s: every bar a different
	// loudness.
	waveLongRecipe = "aevalsrc=0.9*sin(2*PI*440*t)*t/10:s=48000:d=10"
)

// waveFixture makes one fixture, 16-bit, with no tag: a WAV unless the name
// says .flac.
func waveFixture(t *testing.T, ffmpeg, dir, file, recipe string) string {
	t.Helper()
	codec := "pcm_s16le"
	if filepath.Ext(file) == ".flac" {
		codec = "flac"
	}
	return gen(t, ffmpeg, dir, file, "-f", "lavfi", "-i", recipe, "-c:a", codec, "-sample_fmt", "s16",
		"-fflags", "+bitexact", "-flags", "+bitexact")
}

// claimAnHour rewrites a FLAC's STREAMINFO total — 36 bits at bytes 21–25,
// after `fLaC`, the block header and STREAMINFO's first ten bytes, the sample
// rate and the layout — to an hour at its own rate, leaving its frames as they
// are: the container overstates.
func claimAnHour(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(b, []byte("fLaC")) || b[4]&0x7f != 0 || len(b) < 26 {
		t.Fatalf("%s is not the FLAC this test writes", path)
	}
	rate := uint32(b[18])<<12 | uint32(b[19])<<4 | uint32(b[20])>>4
	b[21] &= 0xf0
	binary.BigEndian.PutUint32(b[22:], 3600*rate)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func wavePeaksOf(t *testing.T, ffmpeg, ffprobe, in string) ([]uint8, *ProbeResult) {
	t.Helper()
	ctx := context.Background()
	p, err := Probe(ffprobe)(ctx, in)
	if err != nil {
		t.Fatalf("probe %s: %v", filepath.Base(in), err)
	}
	h, err := WavePeaks(ctx, ffmpeg, in, p, WaveBars)
	if err != nil {
		t.Fatalf("peaks %s: %v", filepath.Base(in), err)
	}
	return h, p
}

// TestWaveLoudnessOnFFmpeg pins erratum 332's three readings.
func TestWaveLoudnessOnFFmpeg(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()

	t.Run("the brick is loudness, not peaks", func(t *testing.T) {
		h, _ := wavePeaksOf(t, ffmpeg, ffprobe, waveFixture(t, ffmpeg, dir, "edm.wav", waveEDMRecipe))
		// Bars 0–5 are the intro (bar 6 straddles 2 s), 8–34 the first drop,
		// 36–47 the breakdown (10–14 s), 50–69 the second drop.
		for j := 0; j <= 5; j++ {
			if h[j] >= 102 {
				t.Errorf("intro bar %d is %d, want under 40%% of the band: %v", j, h[j], h)
			}
		}
		full := true
		for _, v := range h[8:] {
			full = full && v >= 250
		}
		if full {
			t.Errorf("every bar after the intro is full — the peak reading's brick: %v", h)
		}
		lowestDrop := uint8(255)
		for _, v := range append(append([]uint8{}, h[8:35]...), h[50:]...) {
			lowestDrop = min(lowestDrop, v)
		}
		for j := 36; j <= 47; j++ {
			if h[j] >= lowestDrop {
				t.Errorf("breakdown bar %d is %d, not under the drops' lowest %d: %v", j, h[j], lowestDrop, h)
			}
		}
	})

	t.Run("an antiphase stereo file is not silence", func(t *testing.T) {
		h, p := wavePeaksOf(t, ffmpeg, ffprobe, waveFixture(t, ffmpeg, dir, "antiphase.wav", waveAntiphaseRecipe))
		if p.AudioChannels != 2 {
			t.Fatalf("the fixture has %d channels, want 2", p.AudioChannels)
		}
		for j, v := range h {
			if v <= 200 {
				t.Fatalf("bar %d of a steady antiphase tone is %d, want > 200: %v", j, v, h)
			}
		}
	})

	t.Run("a header that claims an hour", func(t *testing.T) {
		in := waveFixture(t, ffmpeg, dir, "longheader.flac", waveLongRecipe)
		claimAnHour(t, in)
		h, p := wavePeaksOf(t, ffmpeg, ffprobe, in)
		if p.DurationUS < 3000e6 {
			t.Fatalf("the fixture claims %d µs; the test needs a header that overstates", p.DurationUS)
		}
		seen := map[uint8]bool{}
		for _, v := range h {
			seen[v] = true
		}
		if len(seen) != WaveBars {
			t.Errorf("a rising 10 s sound draws %d distinct bars, want %d: %v", len(seen), WaveBars, h)
		}
	})
}
