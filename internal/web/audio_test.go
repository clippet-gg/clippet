package web

import (
	"testing"

	"clippet/internal/db"
	"clippet/internal/transcode"
)

// An audio clip's watch page (§20 erratum 328): an <audio> where a video's
// <video> is, the card picture above it, the tier in the owner's words, and no
// control that is about a picture.

func audioClip(f *fixture, playable bool) db.Clip {
	c := f.ready
	c.Kind = db.ClipKindAudio
	c.AudioCodec = "pcm_s24le"
	c.Width, c.Height, c.FPSNum, c.FPSDen = nil, nil, nil, nil
	c.SocialKey, c.SocialSize, c.SocialETag = s("sk"), i64v(1<<20), s("se")
	c.SocialWidth, c.SocialHeight = i(1200), i(630)
	c.LightState = db.TierReady
	if playable {
		c.RenditionKey, c.RenditionSize, c.RenditionETag = nil, nil, nil
		c.OriginalPlayable, c.OriginalCodec = true, "flac"
		c.FullState = db.TierSkipped
	} else {
		c.OriginalCodec = "pcm_f32le"
		c.FullState, c.FullCodec = db.TierReady, transcode.AudioFullFLAC24
		c.RenditionSize = i64v(4 << 20)
	}
	return c
}

func TestAudioWatchPage(t *testing.T) {
	f := newFixture(t)
	srv := serverFor(t, f)

	t.Run("a float WAV plays its 24-bit FLAC at /audio", func(t *testing.T) {
		c := audioClip(f, false)
		m := srv.clipModel(ClipView{Clip: c, URLs: builder(t, testBase), IsOwner: true})
		body := renderClip(t, srv, m)
		mustContain(t, body, `<audio controls preload="metadata" src="/api/clips/`+c.ID+`/audio"`)
		mustContain(t, body, `<img class="audio-picture" src="/api/clips/`+c.ID+`/poster.jpg"`)
		mustNotContain(t, body, `<video`)
		mustContain(t, body, `Full · FLAC 24-bit · 4.0 MiB`)
		mustContain(t, body, `href="/api/clips/`+c.ID+`/audio?download=1"`)
		// No picture control: their routes answer not_for_audio.
		for _, gone := range []string{`data-trim`, `data-gif-go`, `data-poster-go`, `data-replace`} {
			mustNotContain(t, body, gone)
		}
		mustContain(t, body, `<dt>Audio</dt>`)
	})

	t.Run("a FLAC plays as its Original", func(t *testing.T) {
		c := audioClip(f, true)
		m := srv.clipModel(ClipView{Clip: c, URLs: builder(t, testBase)})
		if len(m.Tiers) != 1 || m.Tiers[0].Label == "" || m.VideoURL != "/api/clips/"+c.ID+"/audio" {
			t.Fatalf("tiers %+v src %q", m.Tiers, m.VideoURL)
		}
		body := renderClip(t, srv, m)
		mustContain(t, body, `Original · FLAC · `)
	})

	t.Run("the library row says audio where a video says its size", func(t *testing.T) {
		if got := resolutionText(audioClip(f, true)); got != "audio" {
			t.Fatalf("resolutionText = %q", got)
		}
		if got := videoFacts(audioClip(f, true)); got != formatDuration(*f.ready.DurationMS) {
			t.Fatalf("videoFacts = %q", got)
		}
	})
}

// waveClip is an audio clip as the waveform's run commits it (§20 erratum
// 329): a PNG poster at the card frame, the audiogram as its card copy, and
// the bars on the row.
func waveClip(f *fixture) db.Clip {
	c := audioClip(f, true)
	c.PosterKey = s("c/" + c.ID + "/poster/poster-abcd1234.png")
	c.PosterWidth, c.PosterHeight = i(1280), i(720)
	c.SocialWidth, c.SocialHeight = i(1280), i(720)
	c.SourceKey, c.SourceContentType = "c/"+c.ID+"/original/source.flac", "audio/flac"
	h := make([]uint8, transcode.WaveBars)
	for k := range h {
		h[k] = uint8(k * 3)
	}
	c.Peaks = transcode.EncodePeaks(h)
	return c
}

// TestAudioCardTags pins the audio card (§20 erratum 329): og:video is the
// audiogram (the card copy, as for any clip), og:image the PNG poster under
// image/png at its measured 1280×720 with an alt that says it is a waveform,
// and og:audio + og:audio:type name /audio under its real type. A video's
// card carries no og:audio and its image stays a JPEG.
func TestAudioCardTags(t *testing.T) {
	f := newFixture(t)
	c := waveClip(f)
	u := builder(t, "https://clips.example.com")
	m := metaMap(t, BuildMeta(ClipView{Clip: c, URLs: u}))
	for k, want := range map[string]string{
		"property og:type":         "video.other",
		"property og:video":        u.Social(c.ID),
		"property og:video:type":   "video/mp4",
		"property og:video:width":  "1280",
		"property og:image":        u.Poster(c.ID),
		"property og:image:type":   "image/png",
		"property og:image:width":  "1280",
		"property og:image:height": "720",
		"property og:image:alt":    "The waveform of the audio clip: " + c.Title,
		"name twitter:image:alt":   "The waveform of the audio clip: " + c.Title,
		"property og:audio":        u.Audio(c.ID),
		"property og:audio:type":   "audio/flac",
		"name twitter:player":      u.Embed(c.ID),
	} {
		if got := m[k]; got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	v := metaMap(t, BuildMeta(ClipView{Clip: readyClip(), URLs: u}))
	if _, ok := v["property og:audio"]; ok {
		t.Error("a video's card carries og:audio")
	}
	if v["property og:image:type"] != "image/jpeg" {
		t.Errorf("a video's og:image:type is %q", v["property og:image:type"])
	}
	// The Full plays where the original does not: og:audio:type follows it.
	full := audioClip(f, false)
	full.RenditionKey = s("c/" + full.ID + "/renditions/main-abcd1234.flac")
	if got := metaMap(t, BuildMeta(ClipView{Clip: full, URLs: u}))["property og:audio:type"]; got != "audio/flac" {
		t.Errorf("a Full's og:audio:type = %q", got)
	}
}

// TestAudioComponentEmbedPlaysTheAudiogram: the component embed's Media
// Gallery item for a ready audio clip is the card copy — the audiogram —
// exactly as a video's is its card file.
func TestAudioComponentEmbedPlaysTheAudiogram(t *testing.T) {
	f := newFixture(t)
	c := waveClip(f)
	u := builder(t, "https://clips.example.com")
	p := walkCE(t, string(BuildComponentEmbed(ClipView{Clip: c, URLs: u}, "")))
	if p.media != u.Social(c.ID) {
		t.Errorf("the gallery item is %q, want the audiogram %q", p.media, u.Social(c.ID))
	}
}

// TestAudioPageCarriesItsWaveform pins the page's own progress: the picture's
// frame carries the row's bars and the server's geometry for app.js, and the
// <audio> carries the title and the poster for the Media Session; a row with no
// readable peaks gets no attributes (and so no layer).
func TestAudioPageCarriesItsWaveform(t *testing.T) {
	f := newFixture(t)
	srv := serverFor(t, f)
	c := waveClip(f)
	body := renderClip(t, srv, srv.clipModel(ClipView{Clip: c, URLs: builder(t, testBase)}))
	mustContain(t, body, `<div class="audio-wave" data-wave="1280,720,80,160,1120,400,10,6,10" data-peaks="`+c.Peaks+`">`)
	mustContain(t, body, `data-media-title="`+c.Title+`" data-artwork="/api/clips/`+c.ID+`/poster.jpg"`)
	for _, bad := range []string{"", "zz", "0"} {
		c.Peaks = bad
		body = renderClip(t, srv, srv.clipModel(ClipView{Clip: c, URLs: builder(t, testBase)}))
		mustContain(t, body, `<div class="audio-wave">`)
		mustNotContain(t, body, `data-peaks`)
	}
}

// TestAudioEmbedPlaysTheAudiogram: the embed's <video> plays an audio clip's
// card copy — the audiogram at /video.mp4 — not the /audio the share page's
// <audio> plays; a video's embed plays what its page plays.
func TestAudioEmbedPlaysTheAudiogram(t *testing.T) {
	f := newFixture(t)
	srv := serverFor(t, f)
	u := builder(t, testBase)
	c := waveClip(f)
	m := srv.clipModel(ClipView{Clip: c, URLs: u})
	if got, want := embedVideoURL(c, m, u), "/api/clips/"+c.ID+"/video.mp4"; got != want {
		t.Errorf("the audio embed plays %q, want the audiogram %q", got, want)
	}
	v := srv.clipModel(ClipView{Clip: f.ready, URLs: u})
	if got := embedVideoURL(f.ready, v, u); got != v.VideoURL {
		t.Errorf("a video's embed plays %q, its page %q", got, v.VideoURL)
	}
}
