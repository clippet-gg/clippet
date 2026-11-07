package web

import (
	"strconv"
	"strings"

	"clippet/internal/db"
	"clippet/internal/transcode"
	"clippet/internal/urls"
)

// The watch page's quality selector (DESIGN.md §9, §17 #10, §20 errata 205, 211).
//
// The owner's ask of 2026-09-08: an option to stream in Original quality, which
// means a quality selector with at least the source, the faithful re-encode
// (Full) and the light shareable copy. This is that control, and the two rules
// it follows are the owner's:
//
//   - it is labelled with REAL NUMBERS — "Original 1440p60 · 38 MB" — because
//     "high/medium/low" tells a viewer nothing about a clip they can already see
//     the dimensions of;
//   - it is SHORT. The whole control is a label, a <select> and at most one
//     sentence, because the existing tooltips were found too verbose for what
//     they actually are.
//
// It is server-rendered: the options are in the HTML, so a browser with no
// JavaScript shows which tier it is playing and a crawler sees a page with one
// <video> and one src. app.js makes it switch (progressive enhancement, §9), and
// the CSP forbids any other way of doing it.

// tierLabel names one tier for a person: the tier's own word, the picture, and
// the size when it is known. Every part is optional except the word, because a
// legacy row knows nothing about its upload except that it exists.
//
//	"Original 2560x1440 60fps · 38.2 MB"
//	"Full 1920x1080 60fps"
//	"Light 1280x720 30fps · 2.1 MB"
func tierLabel(tier string, w, h *int, fpsNum, fpsDen *int, bytes *int64) string {
	parts := []string{tierWord(tier)}
	if dims := dimsText(w, h); dims != "" {
		parts = append(parts, dims)
	}
	if fps := fpsText(fpsNum, fpsDen); fps != "" {
		parts = append(parts, fps)
	}
	label := strings.Join(parts, " ")
	if bytes != nil && *bytes > 0 {
		label += " · " + humanBytes(*bytes)
	}
	return label
}

// tierWord is the tier's name as a reader sees it. "Original" is the owner's word
// for the uploaded file; "Full" is the owner's word for a faithful re-encode (one
// that keeps the quality and fidelity without a cap);
// "Light" is the card copy.
func tierWord(tier string) string {
	switch tier {
	case db.TierNameOriginal:
		return "Original"
	case db.TierNameFull:
		return "Full"
	case db.TierNameLight:
		return "Light"
	}
	return tier
}

func dimsText(w, h *int) string {
	if w == nil || h == nil || *w <= 0 || *h <= 0 {
		return ""
	}
	return strconv.Itoa(*w) + "×" + strconv.Itoa(*h)
}

// fpsText renders a rate the way a person says it: a whole number where the rate
// is one ("60fps"), and one decimal where it is not ("29.97fps"), so a 30000/1001
// clip is not labelled "30fps" and a 60/1 one is not labelled "60.0fps".
func fpsText(num, den *int) string {
	if num == nil || den == nil || *num <= 0 || *den <= 0 {
		return ""
	}
	if *num%*den == 0 {
		return strconv.Itoa(*num / *den) + "fps"
	}
	return strconv.FormatFloat(float64(*num)/float64(*den), 'f', 2, 64) + "fps"
}

// fillTiers builds the selector from the clip row (§20 erratum 211).
//
// Only tiers with a FILE are offered: a viewer cannot stream something that is
// still encoding, and an option that 409s is worse than no option. What a
// missing tier gets instead is TierNote — one sentence saying it is on its way,
// or that it will not be.
//
// The order is the quality order, and the SELECTED one is the first playable
// entry, which is also what the <video src> is set to. A viewer's own choice is
// remembered by app.js in localStorage and applied on top of this; it is
// deliberately not a cookie or a column — what somebody watched at last night is
// not a fact about the clip.
func (m *clipPage) fillTiers(c db.Clip, u urls.Builder) {
	if c.State != db.ClipReady {
		return
	}
	add := func(tier, url string, w, h, fpsNum, fpsDen *int, bytes *int64) {
		m.Tiers = append(m.Tiers, clipTier{
			Tier: tier, URL: samePath(url),
			Label:    tierLabel(tier, w, h, fpsNum, fpsDen, bytes),
			Selected: len(m.Tiers) == 0,
		})
	}
	if c.IsAudio() {
		m.fillAudioTiers(c, u)
		return
	}
	// ORIGINAL, when the upload is what this clip plays. `/original` for a direct
	// upload and `/video.mp4` for a remuxed one — the same coded bytes either
	// way, which is why both are the Original tier (§20 errata 200, 207).
	switch {
	case db.OriginalIsTheClip(c):
		add(db.TierNameOriginal, u.Original(c.ID), c.OriginalWidth, c.OriginalHeight,
			c.OriginalFPSNum, c.OriginalFPSDen, &c.SourceSize)
	case c.OriginalPlayable && c.OriginalRemuxed && c.RenditionKey != nil &&
		c.TrimStartMS == nil && c.TrimEndMS == nil:
		add(db.TierNameOriginal, u.Video(c.ID), c.OriginalWidth, c.OriginalHeight,
			c.OriginalFPSNum, c.OriginalFPSDen, &c.SourceSize)
	}
	// FULL, when a faithful re-encode exists. A remuxed original is not one — the
	// bytes were never re-encoded — and a trimmed clip's cut is.
	if c.RenditionKey != nil && !c.OriginalRemuxed {
		add(db.TierNameFull, u.Video(c.ID), c.Width, c.Height, c.FPSNum, c.FPSDen, c.RenditionSize)
	}
	// LIGHT, when the card's own copy exists. It is offered to a viewer because a
	// viewer on a phone connection may want it, which is the whole argument for a
	// selector rather than a server-side guess.
	if c.SocialKey != nil {
		add(db.TierNameLight, u.Light(c.ID), c.SocialWidth, c.SocialHeight,
			c.SocialFPSNum, c.SocialFPSDen, c.SocialSize)
	}
	m.HasTierChoice = len(m.Tiers) > 1
	m.TierNote = tierNote(c, m.IsOwner)
	// The row is rendered when there is a control, a note, or a tier still coming.
	// The third case is the mount point: a clip with ONE tier and a Light copy
	// still encoding has nothing to show yet and will have an option to add, and
	// app.js needs somewhere to put it (an empty row renders nothing visible).
	m.TiersLive = tiersLive(c)
	if len(m.Tiers) > 0 {
		// The player starts on the best tier, which is also what `/video.mp4`
		// resolves to on the server — so a page with no JavaScript and a page with
		// it play the same file.
		m.VideoURL = m.Tiers[0].URL
	}
}

// tierNote is the ONE short line about a tier that is not here yet. The
// wording rule applies: it says what is happening and stops.
//
// A FAILED Full is worth saying out loud, because the clip is permanently
// playing a 720p copy of something better and the owner may want to re-upload.
// A failed LIGHT is the OWNER's line only. Nothing a viewer can see is missing —
// db.CardObject falls back to the play object, so the link still unfurls — but
// the file a crawler then fetches is the big one, which is the whole thing the
// card copy exists to avoid, and only the owner can do anything about it
// (a replacement). A stranger's page stays free of it, as erratum 211 intends.
//
// The FIRST arm — "Full quality still encoding." — is retained and unreached,
// deliberately, and a later reader should not mistake that for dead code. Since
// §20 erratum 217 (amended by 235) both tiers come out of ONE ffmpeg and a clip
// flips ready with both files in the bucket, so the `full` job kind, though it
// still exists end to end, is no longer enqueued by either producer and nothing
// writes `full_state = encoding` today. The arm is kept for the same reason the
// job kind is: it is this page's answer the moment a producer queues a Full
// again, and a state the schema still holds is better rendered than ignored.
func tierNote(c db.Clip, owner bool) string {
	switch {
	case c.FullState == db.TierEncoding:
		return "Full quality still encoding."
	case c.FullState == db.TierFailed:
		return "Full quality could not be encoded."
	case owner && c.LightState == db.TierFailed:
		return "Card copy could not be made."
	}
	return ""
}

// tiersLive reports whether this page should keep polling: a tier is on its way,
// so the selector will grow. It is what puts a ProgressURL on a READY clip,
// which no other feature does (§20 erratum 211).
//
// It fires on ORDINARY first uploads, not only on a replacement or a post-hoc
// card copy: `finalize` writes LightState = encoding whenever the clip still
// needs a card copy of its own (worker/pipeline.go, `QueueSocial`; §20 errata
// 121, 203), which is the common capture path. The Full arm is the retained one
// tierNote's doc describes.
func tiersLive(c db.Clip) bool {
	return c.State == db.ClipReady && (c.FullState == db.TierEncoding || c.LightState == db.TierEncoding)
}

// fillAudioTiers is fillTiers for an AUDIO clip (§20 erratum 328). It offers
// the tiers that PLAY, each under its own words — `Original · FLAC`, `Full ·
// FLAC 24-bit` — and the size, and both play at `/audio`: the Original when the
// stored file plays as it stands, the Full otherwise. The card copy is not
// offered: it is the same sound in AAC under a still picture, and the page's
// player is an <audio>.
func (m *clipPage) fillAudioTiers(c db.Clip, u urls.Builder) {
	add := func(label string, bytes *int64) {
		if bytes != nil && *bytes > 0 {
			label += " · " + humanBytes(*bytes)
		}
		m.Tiers = append(m.Tiers, clipTier{Tier: tierOf(label), URL: samePath(u.Audio(c.ID)), Label: label,
			Selected: len(m.Tiers) == 0})
	}
	switch {
	case db.OriginalIsTheClip(c):
		add(transcode.AudioTierLabel(transcode.TierOriginal, c.OriginalCodec, ""), &c.SourceSize)
	case c.RenditionKey != nil:
		add(transcode.AudioTierLabel(transcode.TierFull, "", c.FullCodec), c.RenditionSize)
	}
	// One tier is the whole story of an audio clip, so there is no choice to
	// offer — but the label is still what the row says, which is the point of
	// erratum 211's "real numbers": a viewer is told what they are hearing.
	m.HasTierChoice = len(m.Tiers) > 1
	m.AudioTierLabel = ""
	if len(m.Tiers) > 0 {
		m.AudioTierLabel = m.Tiers[0].Label
		m.VideoURL = m.Tiers[0].URL
	}
}

// tierOf is the tier word an audio label names, for the option's data-tier.
func tierOf(label string) string {
	if strings.HasPrefix(label, "Full") {
		return db.TierNameFull
	}
	return db.TierNameOriginal
}
