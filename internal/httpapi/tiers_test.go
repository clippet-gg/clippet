package httpapi

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"clippet/internal/auth"
	"clippet/internal/db"
	"clippet/internal/store"
)

// The quality tiers on the wire (DESIGN.md §8.1, §8.2, §20 errata 200–210).
//
// The two things this file is really about are the AUTH change and the tier
// list. Everything else — the ranges, the ETags, the 416s — is TestMediaRoutes'
// and is unchanged by these tiers, deliberately: /original became a second
// player URL by widening one gate, not by growing a second implementation.

// playableClip is a ready clip whose UPLOAD is what it plays: no rendition
// object at all, `original_playable` set, untrimmed (§20 errata 201, 207). It is
// the remux fast path's row, which is the row the auth change is about.
func (e *env) playableClip(owner db.User) (db.Clip, []byte) {
	e.t.Helper()
	source := testVideo(2048)
	c := e.seedClip(owner, clipOpts{source: source})
	job := e.claim(c.ID)
	poster := []byte("poster-bytes")
	pk := store.PosterKey(e.cfg.S3.KeyPrefix, c.ID, "r8r8r8r8")
	e.fake.Put(pk, poster, "image/jpeg")
	sourceMS := int64(6000)
	ok, _, err := e.db.FinalizeTranscode(context.Background(), job.ID, c.ID, db.ReadyFields{
		PosterKey: pk, PosterSize: int64(len(poster)), PosterETag: sha256hex(poster), PosterWidth: 640, PosterHeight: 360,
		Width: 1280, Height: 720, DurationMS: 6000, FPSNum: 30, FPSDen: 1, HasAudio: true,
		SourceDurationMS: &sourceMS,
		Tiers: db.TierFields{OriginalPlayable: true, OriginalWidth: 1280, OriginalHeight: 720,
			OriginalFPSNum: 30, OriginalFPSDen: 1, OriginalCodec: "h264",
			FullState: db.TierSkipped, LightState: db.TierSkipped},
	}, false, e.now())
	if err != nil || !ok {
		e.t.Fatalf("FinalizeTranscode: ok=%v err=%v", ok, err)
	}
	return e.getClip(c.ID), source
}

func TestOriginalIsAnonymousWhenItIsTheClip(t *testing.T) {
	e := newEnv(t)
	owner, ownerCookie := e.newUser("owner")
	_, strangerCookie := e.newUser("stranger")
	ownerSession, strangerSession := withCookie(ownerCookie), withCookie(strangerCookie)
	c, source := e.playableClip(owner)
	url := "/api/clips/" + c.ID + "/original"

	// ANONYMOUS: 200, the whole upload, inline (no attachment), publicly
	// cacheable and varying on nothing — it is the file the watch page streams.
	r := e.do("GET", url, nil)
	if r.code != 200 || string(r.body) != string(source) {
		t.Fatalf("anonymous /original: %d len=%d", r.code, len(r.body))
	}
	if cd := r.hdr.Get("Content-Disposition"); cd != "" {
		t.Fatalf("a public player URL must not be an attachment: %q", cd)
	}
	if cc, vary := r.hdr.Get("Cache-Control"), r.hdr.Get("Vary"); cc != "public, max-age=3600" || vary != "" {
		t.Fatalf("Cache-Control=%q Vary=%q, want the public media pair", cc, vary)
	}
	if r.hdr.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("Accept-Ranges = %q", r.hdr.Get("Accept-Ranges"))
	}
	// …and it ranges, because that is how a browser seeks in it.
	r = e.do("GET", url, nil, withHeader("Range", "bytes=0-99"))
	if r.code != 206 || len(r.body) != 100 || r.hdr.Get("Content-Range") != "bytes 0-99/"+strconv.Itoa(len(source)) {
		t.Fatalf("ranged anonymous /original: %d %q len=%d", r.code, r.hdr.Get("Content-Range"), len(r.body))
	}
	// A STRANGER's session gets the same public answer: the link is the auth, as
	// it is for every other public route.
	if r := e.do("GET", url, nil, strangerSession); r.code != 200 {
		t.Fatalf("a stranger's session on a public original: %d", r.code)
	}
	// `?download=1` is the SAVE, for the owner and for anybody else: same bytes,
	// same validators, a Content-Disposition (§20 errata 98, 207).
	r = e.do("GET", url+"?download=1", nil, ownerSession)
	if r.code != 200 || !strings.HasPrefix(r.hdr.Get("Content-Disposition"), "attachment") {
		t.Fatalf("?download=1: %d %q", r.code, r.hdr.Get("Content-Disposition"))
	}
	// /video.mp4 serves the SAME object — it is the best playable tier — so a
	// client written before these tiers keeps working unchanged.
	v := e.do("GET", "/api/clips/"+c.ID+"/video.mp4", nil)
	if v.code != 200 || string(v.body) != string(source) {
		t.Fatalf("/video.mp4 on a playable original: %d len=%d", v.code, len(v.body))
	}
	if v.hdr.Get("ETag") != `"`+sha256hex(source)+`"` {
		t.Fatalf("/video.mp4 ETag = %q, want the upload's", v.hdr.Get("ETag"))
	}
	if cd := v.hdr.Get("Content-Disposition"); cd != "" {
		t.Fatalf("/video.mp4 must be inline: %q", cd)
	}
}

func TestOriginalStaysOwnerOnlyForEveryOtherShape(t *testing.T) {
	e := newEnv(t)
	owner, ownerCookie := e.newUser("owner")
	_, strangerCookie := e.newUser("stranger")
	ownerSession, strangerSession := withCookie(ownerCookie), withCookie(strangerCookie)
	tok := e.newToken(owner, auth.ScopeUpload)

	// Each of these is a clip whose upload is NOT what it plays, and each must
	// answer exactly what /original answered before the tiers.
	// They are built ONE AT A TIME: `claim` asserts it took the clip it was asked
	// for, so a second queued clip must not exist while another is being driven to
	// ready.
	ordinary, _, _ := e.readyClip(owner, clipOpts{}) // encoded: a Full tier
	failed := e.failedClip(owner, clipOpts{})
	// A playable upload that was TRIMMED: the clip is a window of it, so handing
	// the whole file to a link-holder would hand out the footage the owner cut.
	trimmed, _ := e.playableClip(owner)
	e.trimRow(trimmed.ID, 500, 3000)
	queued := e.seedClip(owner, clipOpts{})

	for _, c := range []db.Clip{ordinary, queued, failed, trimmed} {
		url := "/api/clips/" + c.ID + "/original"
		if r := e.do("GET", url, nil); r.code != 401 {
			t.Errorf("anonymous /original on a %s clip = %d, want 401", c.State, r.code)
		}
		if r := e.do("GET", url, nil, strangerSession); r.code != 403 || r.errCode() != "forbidden" {
			t.Errorf("a stranger on a %s clip = %d %s, want 403 forbidden", c.State, r.code, r.body)
		}
		// A capture token is an upload credential and never a download one
		// (erratum 19), even for its own owner's clip.
		if r := e.do("GET", url, nil, withBearer(tok)); r.code != 403 || r.errCode() != "token_not_allowed" {
			t.Errorf("a Bearer token on a %s clip = %d %s, want 403 token_not_allowed", c.State, r.code, r.body)
		}
		// The OWNER still gets it, as an attachment, in every one of these states.
		r := e.do("GET", url, nil, ownerSession)
		if r.code != 200 || !strings.HasPrefix(r.hdr.Get("Content-Disposition"), "attachment") {
			t.Errorf("the owner on a %s clip = %d %q", c.State, r.code, r.hdr.Get("Content-Disposition"))
		}
		if cc := r.hdr.Get("Cache-Control"); !strings.HasPrefix(cc, "private") {
			t.Errorf("an owner-only original must not be publicly cacheable: %q", cc)
		}
	}
	// A tombstone is a 404 for everybody, owner included.
	del := e.deletingClip(owner)
	for name, opts := range map[string][]reqOpt{"anonymous": nil, "the owner": {ownerSession}} {
		if r := e.do("GET", "/api/clips/"+del.ID+"/original", nil, opts...); r.code != 404 {
			t.Errorf("a tombstoned clip's original to %s = %d, want 404", name, r.code)
		}
	}
}

func TestLightRouteIsTheSocialRouteUnderItsCanonicalName(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	c, rendition, _ := e.readyClip(owner, clipOpts{})
	// With no card file of its own, both names fall back to the playable file —
	// the behaviour /social.mp4 has had since erratum 121.
	for _, name := range []string{"social.mp4", "light.mp4"} {
		r := e.do("GET", "/api/clips/"+c.ID+"/"+name, nil)
		if r.code != 200 || string(r.body) != string(rendition) {
			t.Fatalf("/%s with no card file: %d len=%d", name, r.code, len(r.body))
		}
	}
	// With one, both names serve IT.
	light := []byte("light-copy-bytes-for-the-card")
	lk := store.SocialKey(e.cfg.S3.KeyPrefix, c.ID, "lightr8r")
	e.fake.Put(lk, light, "video/mp4")
	if _, err := e.db.SetSocial(context.Background(), e.enqueueLight(c.ID), c.ID,
		db.SocialFields{Key: lk, Size: int64(len(light)), ETag: sha256hex(light),
			Width: 1280, Height: 720, FPSNum: 30, FPSDen: 1}, sha256hex(rendition), e.now()); err != nil {
		t.Fatalf("SetSocial: %v", err)
	}
	for _, name := range []string{"social.mp4", "light.mp4"} {
		r := e.do("GET", "/api/clips/"+c.ID+"/"+name, nil)
		if r.code != 200 || string(r.body) != string(light) {
			t.Fatalf("/%s with a card file: %d %q", name, r.code, r.body)
		}
		if r.hdr.Get("ETag") != `"`+sha256hex(light)+`"` {
			t.Fatalf("/%s ETag = %q, want the light copy's", name, r.hdr.Get("ETag"))
		}
	}
	// HEAD works on the new name too (the media routes serve HEAD from GET).
	if r := e.do("HEAD", "/api/clips/"+c.ID+"/light.mp4", nil); r.code != 200 || len(r.body) != 0 {
		t.Fatalf("HEAD /light.mp4: %d len=%d", r.code, len(r.body))
	}
	// …and the method gate is the media routes' own.
	if r := e.do("POST", "/api/clips/"+c.ID+"/light.mp4", nil); r.code != 405 {
		t.Fatalf("POST /light.mp4 = %d, want 405", r.code)
	}
}

func TestClipDTOTiers(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")

	t.Run("a playable original", func(t *testing.T) {
		c, source := e.playableClip(owner)
		dto := e.clipDTO(t, c.ID)
		if dto.CardTier != db.TierNameOriginal {
			t.Fatalf("card_tier = %q, want original", dto.CardTier)
		}
		if len(dto.Tiers) != 3 {
			t.Fatalf("tiers = %d entries, want 3 (one per tier, always)", len(dto.Tiers))
		}
		orig := dto.Tiers[0]
		switch {
		case orig.Tier != db.TierNameOriginal || orig.State != db.TierReady || !orig.Playable:
			t.Fatalf("original tier = %+v", orig)
		case orig.URL == nil || !strings.HasSuffix(*orig.URL, "/original"):
			t.Fatalf("the original tier's url = %v, want /original", orig.URL)
		case orig.Width == nil || *orig.Width != 1280 || orig.FPS == nil || *orig.FPS != "30/1":
			t.Fatalf("the original tier's numbers = %v x %v @ %v", orig.Width, orig.Height, orig.FPS)
		case orig.Bytes == nil || *orig.Bytes != int64(len(source)):
			t.Fatalf("the original tier's bytes = %v, want %d", orig.Bytes, len(source))
		case orig.Codec != "h264":
			t.Fatalf("the original tier's codec = %q", orig.Codec)
		}
		// The two tiers that were never needed are `skipped` with no URL: a client
		// must never be handed an address that answers 404.
		for _, tier := range dto.Tiers[1:] {
			if tier.State != db.TierSkipped || tier.URL != nil {
				t.Fatalf("%s tier = %+v, want skipped with no url", tier.Tier, tier)
			}
		}
	})

	t.Run("an encoded clip", func(t *testing.T) {
		c, rendition, _ := e.readyClip(owner, clipOpts{})
		dto := e.clipDTO(t, c.ID)
		if dto.CardTier != db.TierNameFull {
			t.Fatalf("card_tier = %q, want full", dto.CardTier)
		}
		full := dto.Tiers[1]
		switch {
		case full.State != db.TierReady || !full.Playable:
			t.Fatalf("full tier = %+v", full)
		case full.URL == nil || !strings.HasSuffix(*full.URL, "/video.mp4"):
			t.Fatalf("the full tier's url = %v", full.URL)
		case full.Bytes == nil || *full.Bytes != int64(len(rendition)):
			t.Fatalf("the full tier's bytes = %v, want %d", full.Bytes, len(rendition))
		case full.Width == nil || *full.Width != 1280 || full.FPS == nil || *full.FPS != "30000/1001":
			t.Fatalf("the full tier's numbers = %v x %v @ %v", full.Width, full.Height, full.FPS)
		}
		// The upload is present but NOT playable: nobody probed it (this row has no
		// tier columns of its own), so the honest answer is a download.
		if orig := dto.Tiers[0]; orig.Playable || orig.URL != nil {
			t.Fatalf("original tier = %+v, want present but not playable", orig)
		}
		// `urls` carries both names of the card route plus the upload's.
		if !strings.HasSuffix(dto.URLs.Light, "/light.mp4") || !strings.HasSuffix(dto.URLs.Social, "/social.mp4") ||
			!strings.HasSuffix(dto.URLs.Original, "/original") {
			t.Fatalf("urls = %+v", dto.URLs)
		}
	})

	t.Run("a clip waiting for its full tier", func(t *testing.T) {
		c := e.cardFirstClip(owner)
		dto := e.clipDTO(t, c.ID)
		if dto.CardTier != db.TierNameLight {
			t.Fatalf("card_tier = %q, want light", dto.CardTier)
		}
		full := dto.Tiers[1]
		if full.State != db.TierEncoding || full.URL != nil {
			t.Fatalf("full tier = %+v, want encoding with no url", full)
		}
		// It is labelled with the ORIGINAL's numbers, because a Full is encoded at
		// the source's own size: the selector can say what is coming.
		if full.Width == nil || *full.Width != 2560 || full.FPS == nil || *full.FPS != "60/1" {
			t.Fatalf("an encoding full tier's numbers = %v x %v @ %v", full.Width, full.Height, full.FPS)
		}
		light := dto.Tiers[2]
		if light.State != db.TierReady || light.URL == nil || !strings.HasSuffix(*light.URL, "/light.mp4") {
			t.Fatalf("light tier = %+v", light)
		}
		if light.Width == nil || *light.Width != 1280 || light.FPS == nil || *light.FPS != "30/1" {
			t.Fatalf("the light tier's numbers = %v x %v @ %v", light.Width, light.Height, light.FPS)
		}
		// N079 is closed by this entry: the DTO now says whether a card file exists.
		if light.Bytes == nil || *light.Bytes <= 0 {
			t.Fatalf("the light tier's bytes = %v", light.Bytes)
		}
	})

	t.Run("a clip that is not ready", func(t *testing.T) {
		c := e.seedClip(owner, clipOpts{})
		dto := e.clipDTO(t, c.ID)
		// Present and never null, so a client renders the list without a check.
		if dto.Tiers == nil {
			t.Fatal("tiers is null on a queued clip; it must be a list")
		}
		if dto.CardTier != "" {
			t.Fatalf("card_tier = %q on a queued clip, want empty", dto.CardTier)
		}
		for _, tier := range dto.Tiers {
			if tier.URL != nil {
				t.Fatalf("%s tier of a queued clip has a url: %v", tier.Tier, tier.URL)
			}
		}
		// …and the two derived tiers are PENDING and not `skipped` (§20 erratum 205,
		// amended). Nothing has probed this upload yet, so "not needed — a finished
		// answer" is the one thing that is certainly untrue: a client that believed
		// it would stop waiting for a Full that may be on its way.
		if full := dto.Tiers[1]; full.State != db.TierPending {
			t.Fatalf("the full tier of a queued clip = %q, want pending", full.State)
		}
		if light := dto.Tiers[2]; light.State != db.TierPending {
			t.Fatalf("the light tier of a queued clip = %q, want pending", light.State)
		}
		// The Original is `ready` throughout: its bytes were in the bucket before
		// the 201 answered.
		if orig := dto.Tiers[0]; orig.State != db.TierReady {
			t.Fatalf("the original tier of a queued clip = %q, want ready", orig.State)
		}
		// The polling route says the same word, which is erratum 116's promise
		// applied to the tiers.
		r := e.do("GET", "/api/clips/"+c.ID+"/progress", nil)
		var p ProgressDTO
		if r.code != 200 {
			t.Fatalf("progress: %d %s", r.code, r.body)
		}
		if err := json.Unmarshal(r.body, &p); err != nil {
			t.Fatal(err)
		}
		if len(p.Tiers) != 3 || p.Tiers[1].State != db.TierPending || p.Tiers[2].State != db.TierPending {
			t.Fatalf("progress tiers of a queued clip = %+v, want the derived two pending", p.Tiers)
		}
	})
}

func TestProgressReportsTierStates(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	c := e.cardFirstClip(owner)

	r := e.do("GET", "/api/clips/"+c.ID+"/progress", nil)
	if r.code != 200 {
		t.Fatalf("progress: %d %s", r.code, r.body)
	}
	var single ProgressDTO
	if err := json.Unmarshal(r.body, &single); err != nil {
		t.Fatal(err)
	}
	if single.State != db.ClipReady {
		t.Fatalf("state = %q, want ready — a clip waiting for a better tier is playable", single.State)
	}
	want := map[string]string{db.TierNameOriginal: db.TierReady, db.TierNameFull: db.TierEncoding,
		db.TierNameLight: db.TierReady}
	if len(single.Tiers) != 3 {
		t.Fatalf("progress tiers = %+v", single.Tiers)
	}
	for _, tier := range single.Tiers {
		if want[tier.Tier] != tier.State {
			t.Fatalf("progress tier %s = %q, want %q", tier.Tier, tier.State, want[tier.Tier])
		}
	}
	// The BATCHED route answers identically (§20 errata 116, 205).
	rb := e.do("GET", "/api/clips/progress?ids="+c.ID, nil)
	if rb.code != 200 {
		t.Fatalf("batch progress: %d %s", rb.code, rb.body)
	}
	var batch struct {
		Progress map[string]json.RawMessage `json:"progress"`
	}
	if err := json.Unmarshal(rb.body, &batch); err != nil {
		t.Fatal(err)
	}
	// The bodies are compared after trimming: WriteJSON ends a document with a
	// newline, and the batch's copy of this one is nested inside another.
	if strings.TrimSpace(string(batch.Progress[c.ID])) != strings.TrimSpace(string(r.body)) {
		t.Fatalf("batch != single:\nbatch  %s\nsingle %s", batch.Progress[c.ID], r.body)
	}
}

// ---- harness helpers ------------------------------------------------------

// clipDTO fetches and decodes GET /api/clips/{id}.
func (e *env) clipDTO(t *testing.T, id string) ClipDTO {
	t.Helper()
	r := e.do("GET", "/api/clips/"+id, nil)
	if r.code != 200 {
		t.Fatalf("GET /api/clips/%s: %d %s", id, r.code, r.body)
	}
	var dto ClipDTO
	if err := json.Unmarshal(r.body, &dto); err != nil {
		t.Fatalf("decode clip DTO: %v", err)
	}
	return dto
}

// cardFirstClip is the row card-first ordering produces: ready on its LIGHT copy,
// no rendition object, a `full` job encoding behind it (§20 erratum 206).
func (e *env) cardFirstClip(owner db.User) db.Clip {
	e.t.Helper()
	c := e.seedClip(owner, clipOpts{})
	job := e.claim(c.ID)
	light := []byte("light-copy-bytes")
	poster := []byte("poster-bytes")
	lk := store.SocialKey(e.cfg.S3.KeyPrefix, c.ID, "lightr8r")
	pk := store.PosterKey(e.cfg.S3.KeyPrefix, c.ID, "r8r8r8r8")
	e.fake.Put(lk, light, "video/mp4")
	e.fake.Put(pk, poster, "image/jpeg")
	ok, _, err := e.db.FinalizeTranscode(context.Background(), job.ID, c.ID, db.ReadyFields{
		PosterKey: pk, PosterSize: int64(len(poster)), PosterETag: sha256hex(poster), PosterWidth: 640, PosterHeight: 360,
		Width: 1280, Height: 720, DurationMS: 6000, FPSNum: 30, FPSDen: 1, HasAudio: true,
		Light: db.SocialFields{Key: lk, Size: int64(len(light)), ETag: sha256hex(light),
			Width: 1280, Height: 720, FPSNum: 30, FPSDen: 1},
		Tiers: db.TierFields{OriginalWidth: 2560, OriginalHeight: 1440, OriginalFPSNum: 60, OriginalFPSDen: 1,
			OriginalCodec: "hevc", FullState: db.TierEncoding, LightState: db.TierReady, QueueFull: true},
	}, false, e.now())
	if err != nil || !ok {
		e.t.Fatalf("FinalizeTranscode: ok=%v err=%v", ok, err)
	}
	return e.getClip(c.ID)
}

// enqueueLight queues and claims a light (`social`) job, returning its id, so a
// test can drive db.SetSocial directly.
func (e *env) enqueueLight(clipID string) int64 {
	e.t.Helper()
	job, err := e.db.EnqueueSocial(context.Background(), clipID, e.now())
	if err != nil {
		e.t.Fatalf("EnqueueSocial: %v", err)
	}
	if _, _, ok, err := e.db.ClaimJobOfKinds(context.Background(), []string{db.KindSocial}, e.now()); err != nil || !ok {
		e.t.Fatalf("claim the light job: ok=%v err=%v", ok, err)
	}
	return job.ID
}

// trimRow writes a trim window straight onto a clip: what the API refuses to
// fabricate and a trim's own commit would take a whole run to produce.
func (e *env) trimRow(id string, startMS, endMS int64) {
	e.t.Helper()
	e.setClipColumn(id, "trim_start_ms", startMS)
	e.setClipColumn(id, "trim_end_ms", endMS)
}

// A publicly cacheable response must never carry a CREDENTIAL (§20 erratum 207,
// amended). `/original` on a playable upload is a public player URL that still
// runs the credential resolver, and that resolver's once-a-minute sliding touch
// re-issues the session cookie on whatever response it lands on — which is the
// FIRST request a session makes, the one this test makes. `public` is RFC 9111
// §3.5's opt-in for a shared cache to hand one caller's response to the next, and
// a stored Set-Cookie is one account's session handed to a stranger.
func TestPublicOriginalIsPrivateWhileItCarriesACookie(t *testing.T) {
	e := newEnv(t)
	owner, ownerCookie := e.newUser("owner")
	c, source := e.playableClip(owner)
	url := "/api/clips/" + c.ID + "/original"

	r := e.do("GET", url, nil, withCookie(ownerCookie))
	if r.code != 200 || len(r.body) != len(source) {
		t.Fatalf("owner GET of the public original: %d len=%d", r.code, len(r.body))
	}
	if r.hdr.Get("Set-Cookie") == "" {
		t.Fatal("the resolver re-issued no cookie on this response; the case under test cannot be observed here any more")
	}
	if cc := r.hdr.Get("Cache-Control"); cc != privateCacheControl {
		t.Fatalf("Cache-Control = %q on a response carrying Set-Cookie, want %q", cc, privateCacheControl)
	}
	if vary := r.hdr.Get("Vary"); vary != credentialVary {
		t.Fatalf("Vary = %q on a credential-bearing response, want %q", vary, credentialVary)
	}
	// The ANONYMOUS answer — the one a CDN exists for — is unchanged: no cookie,
	// so `public` and no Vary.
	r = e.do("GET", url, nil)
	switch {
	case r.code != 200:
		t.Fatalf("anonymous GET: %d", r.code)
	case r.hdr.Get("Set-Cookie") != "":
		t.Fatalf("an anonymous request was handed a cookie: %q", r.hdr.Get("Set-Cookie"))
	case r.hdr.Get("Cache-Control") != mediaCacheControl || r.hdr.Get("Vary") != "":
		t.Fatalf("anonymous Cache-Control=%q Vary=%q, want the public pair",
			r.hdr.Get("Cache-Control"), r.hdr.Get("Vary"))
	}
	// A SECOND request on the same session is inside the touch interval, so it
	// carries no cookie — and is publicly cacheable again, which is the whole
	// point of erratum 207 and is not what this fix takes away.
	r = e.do("GET", url, nil, withCookie(ownerCookie))
	switch {
	case r.code != 200:
		t.Fatalf("second owner GET: %d", r.code)
	case r.hdr.Get("Set-Cookie") != "":
		t.Fatalf("the touch fired twice inside its interval: %q", r.hdr.Get("Set-Cookie"))
	case r.hdr.Get("Cache-Control") != mediaCacheControl:
		t.Fatalf("Cache-Control = %q on a cookie-free owner request, want %q",
			r.hdr.Get("Cache-Control"), mediaCacheControl)
	}
}
