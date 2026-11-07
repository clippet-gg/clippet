package httpapi

import (
	"strconv"

	"clippet/internal/db"
	"clippet/internal/transcode"
	"clippet/internal/urls"
)

// The quality tiers on the wire (DESIGN.md §8.2, §17 #10, §20 errata 205, 210).
//
// One function builds the list, and every renderer of a clip uses it: the clip
// DTO on six routes, the progress DTO, and the watch page's quality selector
// (internal/web reads the same three predicates through db). That is deliberate
// — a selector offering a tier the DTO does not list, or a card pointing at a
// tier the page cannot play, would be two answers to one question.
//
// Everything here is derived from the clip ROW alone. No job lookup, no storage
// call: the tier STATES are columns (migration 0012) precisely so a library page
// of fifty clips costs fifty rows and not two hundred queries.

// tiersOf renders a clip's tiers, best first, and names the one a crawler
// fetches.
//
// The ORDER is the quality order and not the state order: Original, Full, Light.
// A client that wants "the best tier that exists" walks the list and takes the
// first `ready` one with a `url`, which is exactly what the watch page does and
// exactly what `/video.mp4` resolves to on the server (db.PlayObject).
func tiersOf(c db.Clip, u urls.Builder) ([]TierDTO, string) {
	tiers := []TierDTO{originalTier(c, u), fullTier(c, u), lightTier(c, u)}
	if c.IsAudio() {
		audioTiers(c, u, tiers)
	}
	return tiers, cardTierOf(c)
}

// audioTiers rewrites the Original and Full entries of an AUDIO clip (§20
// erratum 328): both play at `/audio` — the route that serves the best playable
// tier under its real media type — never at `/video.mp4`, which for an audio
// clip is the card copy; both carry their label; and neither has a picture, so
// the three picture fields stay null. The Light entry is the card copy as for
// every clip.
func audioTiers(c db.Clip, u urls.Builder, tiers []TierDTO) {
	orig, full := &tiers[0], &tiers[1]
	orig.Width, orig.Height, orig.FPS = nil, nil, nil
	orig.Label = transcode.AudioTierLabel(transcode.TierOriginal, c.OriginalCodec, "")
	if orig.Playable {
		orig.URL = strPtr(u.Audio(c.ID))
	}
	full.Width, full.Height, full.FPS = nil, nil, nil
	if full.State == db.TierReady {
		full.URL = strPtr(u.Audio(c.ID))
		full.Label = transcode.AudioTierLabel(transcode.TierFull, "", c.FullCodec)
	}
}

// originalTier describes the upload. Its `state` is `ready` whenever the row
// exists at all — the bytes were fsynced to the bucket before the 201 — and its
// `playable` is the probe's verdict, narrowed to the cases where those bytes are
// still the whole of what the clip IS (db.OriginalIsTheClip: not remuxed, not
// trimmed, ready). A trimmed clip therefore reports its Original as present and
// NOT playable, which is the truth: the file is there, the owner can download
// it, and it is not what this clip plays.
//
// `url` is the tier's playback address when there is one — `/original` for a
// direct upload, `/video.mp4` for a remuxed one, since the remux IS the original
// bits in a servable container — and null when the upload is a download only.
// The owner's download link is `urls.original` in the DTO either way.
func originalTier(c db.Clip, u urls.Builder) TierDTO {
	t := TierDTO{Tier: db.TierNameOriginal, Codec: c.OriginalCodec,
		Width: c.OriginalWidth, Height: c.OriginalHeight,
		Bytes: int64Ptr(c.SourceSize), State: db.TierReady}
	t.FPS = fpsPtr(c.OriginalFPSNum, c.OriginalFPSDen)
	switch {
	case db.OriginalIsTheClip(c):
		t.Playable = true
		t.URL = strPtr(u.Original(c.ID))
	case c.OriginalPlayable && c.OriginalRemuxed && c.State == db.ClipReady &&
		c.TrimStartMS == nil && c.TrimEndMS == nil && c.RenditionKey != nil:
		// A remuxed original: the same coded bytes in a faststart mp4, which is
		// what `/video.mp4` serves. It is still the Original tier — no frame was
		// re-encoded — and it is what the selector labels with the upload's own
		// dimensions.
		t.Playable = true
		t.URL = strPtr(u.Video(c.ID))
	}
	return t
}

// fullTier describes the faithful re-encode. It exists only for a clip whose
// upload the browser cannot play; for every other READY clip its state is
// `skipped`, which is a finished answer and not a gap. On a clip that is still
// queued or processing nothing has decided yet and the state is `pending`
// (db.ReconcileClipTier) — `skipped` there would tell a poller to stop waiting
// for a tier that is on its way (§20 erratum 205, amended).
//
// While it is `encoding` the DIMENSIONS come from the ORIGINAL's columns rather
// than from nothing, because a Full is encoded at the source's own size: the
// selector can honestly say "Full 1440p60 — still encoding" before the file
// exists. Once it is `ready` the clip's own width/height/fps ARE the Full's
// (db.SetFull writes them together), so they are read from there.
func fullTier(c db.Clip, u urls.Builder) TierDTO {
	t := TierDTO{Tier: db.TierNameFull, Codec: c.FullCodec, Playable: true,
		State: db.ReconcileClipTier(c.State, c.FullState, c.RenditionKey != nil && !c.OriginalRemuxed)}
	switch t.State {
	case db.TierReady:
		t.Width, t.Height = c.Width, c.Height
		t.FPS = fpsPtr(c.FPSNum, c.FPSDen)
		t.Bytes = c.RenditionSize
		t.URL = strPtr(u.Video(c.ID))
	case db.TierEncoding:
		t.Width, t.Height = c.OriginalWidth, c.OriginalHeight
		t.FPS = fpsPtr(c.OriginalFPSNum, c.OriginalFPSDen)
	}
	return t
}

// lightTier describes the card's own copy. `ready` is the presence of the
// object, whatever the state column says: a row migrated from before 0012 has a
// `social_key` and a backfilled state, and a row written since has both.
func lightTier(c db.Clip, u urls.Builder) TierDTO {
	t := TierDTO{Tier: db.TierNameLight, Codec: db.TierCodecH264, Playable: true,
		State: db.ReconcileClipTier(c.State, c.LightState, c.SocialKey != nil)}
	if t.State == db.TierReady && c.SocialKey != nil {
		t.Width, t.Height = c.SocialWidth, c.SocialHeight
		t.FPS = fpsPtr(c.SocialFPSNum, c.SocialFPSDen)
		t.Bytes = c.SocialSize
		t.URL = strPtr(u.Light(c.ID))
	}
	return t
}

// cardTierOf names the tier a crawler actually fetches, which is what
// `/social.mp4` and `/light.mp4` resolve to (mediaObject's fallback chain, in
// the same order): the Light copy when one exists, else the best playable tier
// the clip has.
func cardTierOf(c db.Clip) string {
	switch {
	case c.SocialKey != nil:
		return db.TierNameLight
	case c.RenditionKey != nil && !c.OriginalRemuxed:
		// A rendition object that is not a remux IS the Full tier, whatever the
		// state column says — which is how a row written before migration 0012
		// reads correctly (db.ReconcileTierState's rule, applied here).
		return db.TierNameFull
	case c.RenditionKey != nil:
		return db.TierNameOriginal // the remux: the original's bits, rewrapped
	case c.OriginalPlayable:
		return db.TierNameOriginal // the upload itself
	}
	return ""
}

// fpsPtr renders an exact rate as "num/den" — the form every other fps field in
// this API uses, so 30000/1001 stays itself — or nil when either term is absent.
func fpsPtr(num, den *int) *string {
	if num == nil || den == nil || *num <= 0 || *den <= 0 {
		return nil
	}
	s := strconv.Itoa(*num) + "/" + strconv.Itoa(*den)
	return &s
}

func strPtr(s string) *string { return &s }

func int64Ptr(n int64) *int64 { return &n }
