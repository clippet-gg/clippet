package httpapi

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"clippet/internal/db"
	"clippet/internal/store"
	"clippet/internal/transcode"
)

// fakeStrip stands in for transcode.StripVerified: it writes "stripped:" plus
// the upload's bytes — deterministic, as the real strip is (`+bitexact`), and
// never equal to the upload, so a test can tell which of the two was stored.
func fakeStrip(_ context.Context, p *transcode.ProbeResult, in, out string) (transcode.StripTarget, error) {
	t, ok := transcode.StripTargetFor(p)
	if !ok {
		return t, &transcode.ErrPermanent{Err: errors.New("no target")}
	}
	b, err := os.ReadFile(in)
	if err != nil {
		return t, err
	}
	return t, os.WriteFile(out, append([]byte("stripped:"), b...), 0o644)
}

// audioProbeOf answers every probe with one WAV stream, the shape a 24/96
// recording probes as.
func audioProbeOf(format, codec string) func(context.Context, string) (*transcode.ProbeResult, error) {
	return func(context.Context, string) (*transcode.ProbeResult, error) {
		return &transcode.ProbeResult{Kind: transcode.KindAudio, VideoIndex: -1, AudioIndexes: []int{0},
			AudioCodecs: []string{codec}, FormatName: format, AudioSampleRate: 96000, AudioChannels: 2,
			AudioBits: 24, AudioSampleFmt: "s32", HasDuration: true, DurationUS: 4_000_000}, nil
	}
}

// uploadAs posts one file with a part type of the caller's choosing.
func (e *env) uploadAs(filename, contentType string, data []byte, opts ...reqOpt) resp {
	e.t.Helper()
	body, ct := multipartBody(e.t,
		part{name: "metadata", contentType: "application/json", data: []byte(`{}`)},
		part{name: "video", filename: filename, contentType: contentType, data: data})
	return e.do(http.MethodPost, "/api/clips", body, append([]reqOpt{withHeader("Content-Type", ct)}, opts...)...)
}

// TestAudioUploadStoresTheStrip pins the ingest's audio branch (§20 erratum
// 328): the object that reaches the bucket is the STRIP, never the upload; the
// key and the stored media type are the strip's container's; the row is kind
// audio with the stream's facts; the DTO says so.
func TestAudioUploadStoresTheStrip(t *testing.T) {
	e := newEnv(t)
	e.probe = audioProbeOf("wav", "pcm_s24le")
	_, cookie := e.newUser("carol")
	raw := []byte("RIFF....WAVEbext originator=Carol's recorder iXML scene 4 take 2")
	r := e.uploadAs("take 2.wav", "audio/x-wav", raw, withCookie(cookie))
	if r.code != http.StatusCreated {
		t.Fatalf("upload: %d %s", r.code, r.body)
	}
	if r.str("kind") != "audio" {
		t.Fatalf("kind = %q: %s", r.str("kind"), r.body)
	}
	c := e.getClip(r.str("id"))
	if c.Kind != db.ClipKindAudio || c.AudioCodec != "pcm_s24le" || c.SampleRate == nil || *c.SampleRate != 96000 ||
		c.BitDepth == nil || *c.BitDepth != 24 || c.AudioChannels == nil || *c.AudioChannels != 2 {
		t.Fatalf("row: kind %q codec %q rate %v bits %v channels %v", c.Kind, c.AudioCodec, c.SampleRate, c.BitDepth, c.AudioChannels)
	}
	if !strings.HasSuffix(c.SourceKey, "/original/source.wav") || c.SourceContentType != "audio/wav" {
		t.Fatalf("stored as %q %q, want the strip's .wav under audio/wav", c.SourceKey, c.SourceContentType)
	}
	stored, ok := e.fake.Get(c.SourceKey)
	if !ok {
		t.Fatal("no original in the bucket")
	}
	if bytes.Equal(stored, raw) || !bytes.Equal(stored, append([]byte("stripped:"), raw...)) {
		t.Fatalf("the bucket holds %q — the raw upload, not its strip", stored)
	}
	if c.SourceSize != int64(len(stored)) || c.SourceETag != sha256hex(stored) {
		t.Fatalf("size/etag describe the upload, not the stored strip: %d %s", c.SourceSize, c.SourceETag)
	}
}

// TestAudioUploadContainerFromTheProbe pins "the probe decides": an audio-only
// `.mov` voice memo, sent as video/quicktime, is stored as the MP4 the strip
// wrote — `.m4a`, audio/mp4.
func TestAudioUploadContainerFromTheProbe(t *testing.T) {
	e := newEnv(t)
	e.probe = audioProbeOf("mov,mp4,m4a,3gp,3g2,mj2", "aac")
	_, cookie := e.newUser("carol")
	r := e.uploadAs("memo.mov", "video/quicktime", []byte("a phone's voice memo"), withCookie(cookie))
	if r.code != http.StatusCreated {
		t.Fatalf("upload: %d %s", r.code, r.body)
	}
	c := e.getClip(r.str("id"))
	if !strings.HasSuffix(c.SourceKey, "/original/source.m4a") || c.SourceContentType != "audio/mp4" {
		t.Fatalf("stored as %q %q", c.SourceKey, c.SourceContentType)
	}
}

// TestAudioDedupe pins erratum 326 on audio: the same file twice is ONE clip,
// matched on the stored strip's hash.
func TestAudioDedupe(t *testing.T) {
	e := newEnv(t)
	e.probe = audioProbeOf("mp3", "mp3")
	_, cookie := e.newUser("carol")
	raw := []byte("ID3 an mp3 with a cover and a title")
	up := func() resp {
		body, ct := multipartBody(t, dedupeOn, part{name: "metadata", contentType: "application/json", data: []byte(`{}`)},
			part{name: "video", filename: "song.mp3", contentType: "audio/mpeg", data: raw})
		return e.do(http.MethodPost, "/api/clips", body, withCookie(cookie), withHeader("Content-Type", ct))
	}
	first, second := up(), up()
	if first.code != http.StatusCreated || second.code != http.StatusOK || second.str("id") != first.str("id") ||
		!second.has("existing") {
		t.Fatalf("first %d %s, second %d %s\n%s", first.code, first.str("id"), second.code, second.body, e.logBuf.String())
	}
}

// TestAudioUploadWithNoStripIsRefused pins the fail-closed direction: no strip
// wired is a 503, never a silent unstripped store.
func TestAudioUploadWithNoStripIsRefused(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Strip = nil })
	e.probe = audioProbeOf("wav", "pcm_s16le")
	_, cookie := e.newUser("carol")
	r := e.uploadAs("a.wav", "audio/wav", []byte("RIFF"), withCookie(cookie))
	if r.code != http.StatusServiceUnavailable || len(e.fake.Keys()) != 0 {
		t.Fatalf("no strip: %d %s, objects %v", r.code, r.body, e.fake.Keys())
	}
}

// TestAudioStripRefusalIs415 pins the strip's own refusal (a file it cannot
// strip provably) as the not_a_video answer, with nothing stored.
func TestAudioStripRefusalIs415(t *testing.T) {
	e := newEnv(t)
	e.probe = audioProbeOf("flac", "flac")
	e.strip = func(context.Context, *transcode.ProbeResult, string, string) (transcode.StripTarget, error) {
		return transcode.StripTarget{}, &transcode.ErrPermanent{Err: errors.New("the stripped copy still carries tags")}
	}
	_, cookie := e.newUser("carol")
	r := e.uploadAs("a.flac", "audio/flac", []byte("fLaC"), withCookie(cookie))
	if r.code != http.StatusUnsupportedMediaType || r.errCode() != "not_a_video" || len(e.fake.Keys()) != 0 {
		t.Fatalf("strip refusal: %d %s, objects %v", r.code, r.body, e.fake.Keys())
	}
	if !strings.Contains(r.str("message"), "video or audio") {
		t.Errorf("the 415 says %q", r.str("message"))
	}
}

// audioReady seeds an audio clip finalized the way the worker finalizes one:
// a FLAC Full at an audio rendition key, the audiogram, the waveform poster as
// a PNG, the bars on the row (§20 erratum 329), no picture of its own.
func (e *env) audioReady(owner db.User, playable bool) db.Clip {
	e.t.Helper()
	ctx := context.Background()
	id := "AudioClip123"
	keys := store.KeysFor(e.cfg.S3.KeyPrefix, id, "wav")
	src := []byte("stripped wav bytes")
	e.fake.Put(keys.Original, src, "audio/wav")
	c, _, err := e.db.CreateClipWithJob(ctx, db.NewClip{ID: id, OwnerID: owner.ID, Title: "Take two",
		SourceKey: keys.Original, SourceFilename: "take 2.wav", SourceSize: int64(len(src)),
		SourceContentType: "audio/wav", SourceETag: sha256hex(src), Now: e.now(),
		Kind: db.ClipKindAudio, Audio: db.AudioFacts{Codec: "pcm_s24le", SampleRate: 96000, BitDepth: 24, Channels: 2}})
	if err != nil {
		e.t.Fatal(err)
	}
	job := e.claim(c.ID)
	full, light, poster := []byte("fLaC full bytes"), []byte("card mp4 bytes"), []byte("\x89PNG waveform")
	pk := store.PNGPosterKey(e.cfg.S3.KeyPrefix, id, "r8r8r8r8")
	lk := store.SocialKey(e.cfg.S3.KeyPrefix, id, "r8r8r8r8")
	e.fake.Put(pk, poster, "image/png")
	e.fake.Put(lk, light, "video/mp4")
	f := db.ReadyFields{PosterKey: pk, PosterSize: int64(len(poster)), PosterETag: sha256hex(poster),
		PosterWidth: 1280, PosterHeight: 720, DurationMS: 4000, HasAudio: true, Peaks: "00ff80",
		Light: db.SocialFields{Key: lk, Size: int64(len(light)), ETag: sha256hex(light), Width: 1280, Height: 720, FPSNum: 30, FPSDen: 1},
		Tiers: db.TierFields{OriginalPlayable: playable, OriginalCodec: "pcm_s24le", FullState: db.TierSkipped, LightState: db.TierReady}}
	if !playable {
		rk := store.AudioRenditionKey(e.cfg.S3.KeyPrefix, id, "r8r8r8r8", "flac")
		e.fake.Put(rk, full, "audio/flac")
		f.RenditionKey, f.RenditionSize, f.RenditionETag = rk, int64(len(full)), sha256hex(full)
		f.Tiers.FullState, f.Tiers.FullCodec = db.TierReady, transcode.AudioFullFLAC
	}
	if ok, _, err := e.db.FinalizeTranscode(ctx, job.ID, id, f, false, e.now()); err != nil || !ok {
		e.t.Fatalf("FinalizeTranscode: %v %v", ok, err)
	}
	return e.getClip(id)
}

// TestAudioRoute pins GET /api/clips/{id}/audio: the best playable tier under
// its REAL media type with ranges, the card copy on /video.mp4, a 404 for a
// video, and a download named with the served file's own extension.
func TestAudioRoute(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("carol")
	c := e.audioReady(owner, false)
	if c.Width != nil || c.Height != nil || c.FPSNum != nil {
		t.Fatalf("an audio clip has no picture of its own: %v %v %v", c.Width, c.Height, c.FPSNum)
	}
	r := e.do(http.MethodGet, "/api/clips/"+c.ID+"/audio", nil)
	if r.code != 200 || r.hdr.Get("Content-Type") != "audio/flac" || string(r.body) != "fLaC full bytes" ||
		r.hdr.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("/audio: %d %q %q", r.code, r.hdr.Get("Content-Type"), r.body)
	}
	if r := e.do(http.MethodGet, "/api/clips/"+c.ID+"/audio", nil, withHeader("Range", "bytes=0-3")); r.code != 206 || string(r.body) != "fLaC" {
		t.Fatalf("/audio range: %d %q", r.code, r.body)
	}
	if r := e.do(http.MethodGet, "/api/clips/"+c.ID+"/audio?download=1", nil); !strings.Contains(r.hdr.Get("Content-Disposition"), `filename=take-two.flac`) {
		t.Fatalf("/audio download: %q", r.hdr.Get("Content-Disposition"))
	}
	if r := e.do(http.MethodGet, "/api/clips/"+c.ID+"/video.mp4", nil); r.code != 200 || string(r.body) != "card mp4 bytes" || r.hdr.Get("Content-Type") != "video/mp4" {
		t.Fatalf("/video.mp4 of an audio clip: %d %q %q", r.code, r.hdr.Get("Content-Type"), r.body)
	}
	v, _, _ := e.readyClip(owner, clipOpts{})
	if r := e.do(http.MethodGet, "/api/clips/"+v.ID+"/audio", nil); r.code != 404 {
		t.Fatalf("/audio of a video: %d %s", r.code, r.body)
	}
	// The DTO: kind, the audio block, both tiers playing at /audio with their words.
	d := e.do(http.MethodGet, "/api/clips/"+c.ID, nil)
	if d.str("kind") != "audio" || !strings.Contains(string(d.body), `"audio":{"codec":"pcm_s24le","sample_rate":96000,"bit_depth":24,"channels":2}`) {
		t.Fatalf("dto: %s", d.body)
	}
	if !strings.Contains(string(d.body), `"url":"`+testOrigin+`/api/clips/`+c.ID+`/audio"`) || !strings.Contains(string(d.body), `"label":"Full · FLAC"`) {
		t.Fatalf("dto tiers: %s", d.body)
	}
}

// TestAudioPosterIsServedAsAPNG (§20 erratum 329): the waveform poster is a
// PNG, and poster.jpg — the one poster route every card already names — serves
// it under image/png (the key decides), while the row keeps the bars.
func TestAudioPosterIsServedAsAPNG(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("carol")
	c := e.audioReady(owner, true)
	if c.Peaks != "00ff80" {
		t.Fatalf("the row's peaks %q", c.Peaks)
	}
	r := e.do(http.MethodGet, "/api/clips/"+c.ID+"/poster.jpg", nil)
	if r.code != 200 || r.hdr.Get("Content-Type") != "image/png" || !strings.HasPrefix(string(r.body), "\x89PNG") {
		t.Fatalf("poster: %d %q %q", r.code, r.hdr.Get("Content-Type"), r.body)
	}
	v, _, _ := e.readyClip(owner, clipOpts{})
	if r := e.do(http.MethodGet, "/api/clips/"+v.ID+"/poster.jpg", nil); r.hdr.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("a video's poster: %q", r.hdr.Get("Content-Type"))
	}
}

// TestAudioPlayableOriginal pins the other branch: an Original that plays is
// what /audio serves, under the stored media type, and /original is public.
func TestAudioPlayableOriginal(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("carol")
	c := e.audioReady(owner, true)
	r := e.do(http.MethodGet, "/api/clips/"+c.ID+"/audio", nil)
	if r.code != 200 || r.hdr.Get("Content-Type") != "audio/wav" || string(r.body) != "stripped wav bytes" {
		t.Fatalf("/audio: %d %q %q", r.code, r.hdr.Get("Content-Type"), r.body)
	}
}

// TestPictureRoutesRefuseAudio pins `not_for_audio`: trim, both GIF routes, the
// filmstrip, the chosen frame and the replacement answer 409 before anything
// is queued.
func TestPictureRoutesRefuseAudio(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("carol")
	c := e.audioReady(owner, false)
	for _, rq := range []struct{ method, path, body string }{
		{http.MethodPost, "/trim", `{"start_ms":0,"end_ms":1000}`},
		{http.MethodPost, "/gif", ""},
		{http.MethodGet, "/preview.gif", ""},
		{http.MethodGet, "/filmstrip.jpg", ""},
		{http.MethodPost, "/poster", `{"seconds":1}`},
		{http.MethodPost, "/replace", ""},
	} {
		var body *strings.Reader
		opts := []reqOpt{withCookie(cookie), withHeader("Origin", testOrigin)}
		if rq.body != "" {
			body = strings.NewReader(rq.body)
			opts = append(opts, withJSON())
		}
		var r resp
		if body != nil {
			r = e.do(rq.method, "/api/clips/"+c.ID+rq.path, body, opts...)
		} else {
			r = e.do(rq.method, "/api/clips/"+c.ID+rq.path, nil, opts...)
		}
		if r.code != http.StatusConflict || r.errCode() != "not_for_audio" || r.str("message") != "Not for audio." {
			t.Errorf("%s %s: %d %s", rq.method, rq.path, r.code, r.body)
		}
	}
	if jobs, _ := e.db.ListJobs(context.Background(), db.JobQuery{ClipID: c.ID}); len(jobs) != 1 {
		t.Errorf("a refused route queued something: %d jobs", len(jobs))
	}
}

// TestReplaceVideoWithAudioIsRefused pins "same kind only": a video clip's
// replacement must be a video.
func TestReplaceVideoWithAudioIsRefused(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("carol")
	v, _, _ := e.readyClip(owner, clipOpts{})
	e.probe = audioProbeOf("mp3", "mp3")
	body, ct := multipartBody(t, part{name: "file", filename: "song.mp3", contentType: "audio/mpeg", data: []byte("ID3")})
	r := e.do(http.MethodPost, "/api/clips/"+v.ID+"/replace", body, withCookie(cookie), withHeader("Origin", testOrigin), withHeader("Content-Type", ct))
	if r.code != http.StatusUnsupportedMediaType {
		t.Fatalf("replace a video with audio: %d %s", r.code, r.body)
	}
}

// TestIsMediaTakesAVoiceMessage pins the bot's first opinion: Discord's voice
// message is one `audio/ogg` attachment, and an audio file is media.
func TestIsMediaTakesAVoiceMessage(t *testing.T) {
	for _, a := range []discordAttachment{
		{Filename: "voice-message.ogg", ContentType: "audio/ogg"},
		{Filename: "take.wav", ContentType: "audio/x-wav"},
		{Filename: "song.flac"},
		{Filename: "clip.mp4", ContentType: "video/mp4"},
	} {
		if !isMedia(a) {
			t.Errorf("%+v is not media", a)
		}
	}
	for _, a := range []discordAttachment{{Filename: "notes.txt", ContentType: "text/plain"}, {Filename: "pic.png", ContentType: "image/png"}} {
		if isMedia(a) {
			t.Errorf("%+v is media", a)
		}
	}
	if got, ok := firstVideo([]discordAttachment{{Filename: "pic.png", ContentType: "image/png", URL: "x"},
		{Filename: "voice-message.ogg", ContentType: "audio/ogg", URL: "y"}}); !ok || got.URL != "y" {
		t.Errorf("Enrich skips a voice message: %+v %v", got, ok)
	}
}

// TestVideoWithAnAudioFirstOpinionIsStoredAsAVideo: the probe decides in both
// directions — a VIDEO sent as `audio/mp4` under a `.m4a` name is stored under
// a video type (here, none known: octet-stream), never an audio one.
func TestVideoWithAnAudioFirstOpinionIsStoredAsAVideo(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")
	r := e.uploadAs("clip.m4a", "audio/mp4", testVideo(500), withCookie(cookie))
	if r.code != http.StatusCreated || r.str("kind") != "video" {
		t.Fatalf("upload: %d %s", r.code, r.body)
	}
	if c := e.getClip(r.str("id")); c.SourceContentType != "application/octet-stream" || c.Kind != db.ClipKindVideo {
		t.Fatalf("stored as %q kind %q", c.SourceContentType, c.Kind)
	}
}

// TestVideoReplacementWithAnAudioFirstOpinionIsStoredAsAVideo is the replace
// path's twin of the pin above: a video sent to /replace with an `audio/…`
// part type is stored (and, after the swap, served direct) under a VIDEO type.
func TestVideoReplacementWithAnAudioFirstOpinionIsStoredAsAVideo(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	for _, tc := range []struct{ filename, partType, want string }{
		{"better.mp4", "audio/mpeg", "video/mp4"},
		{"better.m4a", "audio/mp4", "application/octet-stream"},
	} {
		c, _, _ := e.readyClip(owner, clipOpts{})
		body, ct := multipartBody(t, part{name: "file", filename: tc.filename, contentType: tc.partType, data: testVideo(2222)})
		r := e.do(http.MethodPost, "/api/clips/"+c.ID+"/replace", body, withHeader("Content-Type", ct), withCookie(cookie))
		if r.code != http.StatusAccepted {
			t.Fatalf("%s as %s: %d %s", tc.filename, tc.partType, r.code, r.body)
		}
		p, err := db.ParseReplaceParams(e.replaceJob(c.ID).ParamsJSON)
		if err != nil {
			t.Fatal(err)
		}
		if p.SourceContentType != tc.want {
			t.Errorf("%s as %s: stored as %q, want %q", tc.filename, tc.partType, p.SourceContentType, tc.want)
		}
	}
}
