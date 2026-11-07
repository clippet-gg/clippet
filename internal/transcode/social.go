package transcode

// The dedicated social rendition (DESIGN.md §17 #3, §20 errata 121–124).
//
// `/social.mp4` is the URL every card points at — `og:video`,
// `og:video:secure_url` and `twitter:player:stream` — and until now it served
// the main rendition byte for byte. That rendition is sized for a player the
// viewer chose to open: 60 fps, CRF 23, no bitrate ceiling. A card is the
// opposite case. Discord fetches the file itself and stops inlining somewhere
// around 100 MB (community-reported; no official figure was fetchable), and a
// four-minute high-motion 720p60 capture is past that on its own.
//
// So the social file is the same picture at the shape a CARD needs: half the
// frame rate, a hard VBV ceiling, a smaller audio bitrate. Nothing else about
// it changes — same dimensions, same faststart mp4, same Rec.709 stamp — so a
// player handed it plays exactly the clip, and the fallback when it does not
// exist (the main rendition, through the route's COALESCE) is still correct.

const (
	// SocialMaxFPS halves the frame rate of anything above it. Thirty is
	// where the size comes from: a card autoplays muted in a timeline, and
	// nobody has ever noticed the difference there.
	SocialMaxFPS = 30

	// SocialCRF is three points softer than the rendition's default 23 —
	// roughly a 30 % smaller file for a difference invisible at card size,
	// and the number the ceiling below has to work with.
	SocialCRF = 26

	// SocialMaxRateBits / SocialBufSizeBits are the VBV ceiling and buffer.
	// CRF alone has no ceiling at all: a high-motion minute runs to 14 Mbps
	// (measured, erratum 121), which is exactly the clip that then fails to
	// inline. 2.5 Mbps with a two-second buffer keeps a four-minute clip
	// around 75 MB, under the threshold with room to spare.
	SocialMaxRateBits = 2_500_000
	SocialBufSizeBits = 5_000_000

	// SocialAudioBitrate is 96k rather than the rendition's 128k: the card's
	// audio is heard through a phone speaker if it is heard at all, and the
	// difference is ~2 MB across four minutes.
	SocialAudioBitrate = "96k"
)

// SocialPlanOptions is the box a social rendition is planned in: the
// instance's own frame box (the input is the main rendition, which already
// fits it, so nothing is rescaled) with the frame rate capped at
// SocialMaxFPS.
//
// The cap is a MINIMUM of the two, never a raise: an operator who set
// TRANSCODE_MAX_FPS below 30 asked for a slower rendition, and the social
// file has no business being smoother than the file it is derived from.
func SocialPlanOptions(box PlanOptions) PlanOptions {
	box = box.withDefaults()
	box.MaxFPS = min(box.MaxFPS, SocialMaxFPS)
	return box
}

// SocialEncodeOptions is the encoder configuration of a social rendition:
// the instance's preset and timeouts, this package's CRF, ceiling and audio
// rate. Everything the caller set that is not one of those four — the preset,
// the stall and hard timeouts, the output budget — is kept, and
// TRANSCODE_MAX_BITRATE is deliberately OVERWRITTEN rather than combined: the
// social file has its own ceiling and two ceilings would mean the lower one
// silently wins.
// Since the quality tiers this is the LIGHT tier's encoder configuration, and
// `Codec` is pinned to H.264 here rather than merely left alone: FULL_CODEC may
// say `av1`, and the card copy must not follow it. Erratum 121's measured fact
// is that this file exists to be inlined by a crawler, and every crawler and
// every in-app player decodes H.264 (§20 erratum 203).
func SocialEncodeOptions(o EncodeOptions) EncodeOptions {
	o.CRF = SocialCRF
	o.CQOffset = NVENCCQOffsetLight
	o.MaxRateBits = SocialMaxRateBits
	o.BufSizeBits = SocialBufSizeBits
	o.AudioBitrate = SocialAudioBitrate
	o.Codec = FullCodecH264
	o.Trim = nil
	return o
}
