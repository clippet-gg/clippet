package web

import (
	"math"
	"strconv"
	"strings"

	"clippet/internal/db"
	"clippet/internal/transcode"
	"clippet/internal/urls"
)

const (
	// siteName is the FALLBACK for `og:site_name`: the wordmark's own spelling,
	// lowercase.
	//
	// Since §20 erratum 287 the line itself is the instance's own public HOST
	// (`clippet.gg`, `dev.clippet.gg`, a self-hoster's `clips.example.com`) —
	// the owner's call of 2026-09-15 on the small grey line above a card's
	// title: it should read the host, `clippet.gg`. Erratum 241's "the wordmark's spelling" and
	// erratum 285's `clippet dev` are both superseded by that: a host names the
	// instance exactly, it is the thing a reader can type, and the dev box's card
	// is told apart from production's by the hostname it already carries.
	//
	// This constant survives as the one thing a card falls back to when the URL
	// builder has no host at all (its zero value, which no running server has):
	// a BLANK provider line would be a grey empty row above the title, which is
	// worse than the product's own name. internal/httpapi's oEmbed document
	// carries the same fallback as `provider_name`; DESIGN §3 forbids an import
	// between the two packages, so that duplicate is deliberate and pinned by
	// check.sh, not by an import — change one and change the other. The HOST
	// itself is NOT duplicated: it has one home, urls.Builder.SiteName, which
	// both packages already hold.
	siteName = "clippet"
	// productName is the product in prose — document titles, the sentence a
	// card falls back to — where the word is a noun and not the wordmark.
	productName        = "Clippet"
	defaultDescription = "Shared with Clippet"
	statusProcessing   = "Processing…"
	statusFailed       = "Processing failed"
	// The two sentences the LINK-OUT card's picture carries (§20 erratum 294).
	// See placeholderAlt for why they name the picture and not the clip.
	placeholderAltEncoding = "The " + productName + " wordmark: this clip is still encoding"
	placeholderAltFailed   = "The " + productName + " wordmark: this clip could not be encoded"
	// statusQueued is the LIBRARY row's word for a clip nothing has claimed yet
	// (§20 erratum 224). It is app.js's own first word for that state, so the
	// server-rendered line and the poll's first tick say the same thing. The
	// clip page, the embed and every card keep statusProcessing for `queued`:
	// there the line is one clip's whole status and the distinction earns
	// nothing.
	statusQueued = "Queued…"

	// The committed 1200×630 PNG used by link-out cards (§9.1). The three
	// facts live in internal/urls, which internal/httpapi's oEmbed document
	// reads too: they are one picture, and a drift between the two renderers
	// shows up as a card thumbnail that 404s.
	placeholderName = urls.PlaceholderCard
)

var (
	placeholderWidth  = strconv.Itoa(urls.PlaceholderWidth)
	placeholderHeight = strconv.Itoa(urls.PlaceholderHeight)
)

// BuildMeta produces the exact §9.1 tag inventory for the clip's state. It
// is pure: the output depends only on the clip row and the URL builder.
//
// ready → the full player card (og:video*, twitter:card=player, …), with
// og:video:secure_url only when the public base is https; every other
// state → the link-out card (summary_large_image, the placeholder PNG, zero
// og:video* / twitter:player* tags). Unlisted clips render identically.
func BuildMeta(v ClipView) []Meta {
	if v.Clip.State == db.ClipReady {
		return readyMeta(v)
	}
	return linkOutMeta(v)
}

func prop(key, content string) Meta { return Meta{Attr: "property", Key: key, Content: content} }
func name(key, content string) Meta { return Meta{Attr: "name", Key: key, Content: content} }

// siteNameOf is `og:site_name`: the instance's own public host (§20 erratum
// 287).
//
// Discord renders that line as TEXT, in its own face, above the title — it is
// the one line of a card nobody can click and everybody reads. Until erratum
// 287 it was the wordmark's spelling (241) plus the instance badge (285). Both
// are superseded by the host, for three reasons and they compose: the host is
// the SAME string on every instance's card and yet different on each, so a
// self-hoster's card says whose box served it without one line of
// configuration; it is the one thing on a card a reader can type; and it tells
// `dev.clippet.gg` from `clippet.gg` without the badge being involved at all.
// The badge keeps its `dev · ` title prefix and its `Clippet dev` manifest
// name — both of those are the BROWSER's surfaces and neither is a card.
//
// An empty host falls back to the product's spelling: see siteName.
// internal/httpapi's `providerNameOf` is the same function over the same rule
// for the oEmbed document, duplicated for the reason the two constants are
// (DESIGN §3).
func siteNameOf(host string) string {
	if host == "" {
		return siteName
	}
	return host
}

// appendDims appends the width/height pair only when both are known; a
// ready row always has them (FinalizeTranscode sets all of them at once).
func appendDims(m []Meta, mk func(string, string) Meta, wKey, hKey string, w, h *int) []Meta {
	if w == nil || h == nil {
		return m
	}
	return append(m, mk(wKey, strconv.Itoa(*w)), mk(hKey, strconv.Itoa(*h)))
}

// socialDims are the dimensions of the file `/social.mp4` actually serves,
// which is what `og:video:width/height` and the `twitter:player` dims must
// state (§17 #3, §20 erratum 123): the social rendition's own measurements
// when a clip has one, and otherwise the main rendition's — which is exactly
// what that route falls back to serving. A pair is never mixed: both come
// from the same file or neither is emitted.
//
// twitter:player itself is /embed/{id} (an iframe playing /video.mp4, the
// main rendition — not /social.mp4), so giving its width/height the social
// file's numbers is deliberate, not a mismatch: the player box only needs
// the aspect ratio, and the social encode preserves the rendition's aspect
// ratio by construction, so the two files agree on box shape even when a
// narrowed TRANSCODE_MAX_WIDTH/HEIGHT makes their pixel counts diverge.
func socialDims(c db.Clip) (*int, *int) {
	if c.SocialWidth != nil && c.SocialHeight != nil {
		return c.SocialWidth, c.SocialHeight
	}
	return c.Width, c.Height
}

// The three tags §20 erratum 103 added to the ready card, and why each is
// free:
//
//   - `og:video:url` is the SAME property as `og:video` in OGP (the bare name
//     is the shorthand for its `:url` structured field), so emitting both is a
//     duplicate by the specification's own reading -- and Discord's
//     self-hosting guidance names the explicit spelling. Nothing has to choose.
//   - `video:duration`, in whole seconds, is licensed by `og:type=video.other`
//     and comes off `duration_ms`, a column the page body already renders.
//   - `og:image:alt` is the one accessibility affordance a card has, and there
//     was none anywhere. The picture is a frame the transcode picked out of the
//     clip, so the alt says exactly that and names the clip.
//
// `twitter:image:alt` followed in §20 erratum 177, for the reason erratum 103
// added `og:image:alt` at all: the sentence existed for Discord and not for X,
// which is half an accessibility affordance rather than one. Erratum 294
// carried the same pair to the LINK-OUT card, which had neither: see
// placeholderAlt.
//
// The inventory is an EXACT set in three places (§9.1, the BuildMeta golden
// and the E2E card check), which is why a tag is never added without moving
// the count with it. The ready card now carries 27 tags on an http origin and
// 28 on https (the extra one is og:video:secure_url): 23 and 24 before erratum
// 103, 26 and 27 between the two errata. The link-out card carries 16 in every
// state and on either scheme: 14 before erratum 294.

// durationSeconds renders duration_ms as OGP's integer seconds, rounded to
// nearest so a 5.6 s clip does not advertise 5. A missing or non-positive
// duration emits nothing at all: a ready row always has one, and inventing a
// `0` would be a claim about the clip rather than a gap in the row.
func durationSeconds(ms *int64) string {
	if ms == nil || *ms <= 0 {
		return ""
	}
	return strconv.FormatInt((*ms+500)/1000, 10)
}

// placeholderAlt describes the LINK-OUT card's picture for a reader who cannot
// see it (§20 erratum 294). It is `og:image:alt` and `twitter:image:alt` on
// every state that is not `ready`, which took that card's inventory from 14
// tags to 16.
//
// It names what is DRAWN and not what the clip is, because the two are
// different things here and only on this card: since §20 erratum 287(f) the
// placeholder is the wordmark on the dark ground and carries no text at all, so
// a reader who cannot see it is told the one thing the picture says — this is
// Clippet — and then the state, which is why the card is a link-out card and is
// the fact the reader actually came for.
//
// It does NOT name the clip, which is the one way this sentence differs from
// imageAlt's: the picture is not of this clip and never was. `og:title` already
// carries the title, so nothing is lost and a false claim about a picture is
// avoided. Two states, because the card has two: `og:description` says
// "Processing…" for `queued` and `processing` alike, and this follows it rather
// than inventing a third line for a distinction the card does not draw.
func placeholderAlt(state string) string {
	if state == db.ClipFailed {
		return placeholderAltFailed
	}
	return placeholderAltEncoding
}

// posterType is the poster's media type: a PNG for an audio clip's drawn
// waveform (§20 erratum 329), a JPEG for every frame cut out of a video.
func posterType(c db.Clip) string {
	if c.PosterKey == nil {
		return "image/jpeg"
	}
	return transcode.PosterContentType(*c.PosterKey)
}

// waveformAlt is imageAlt for an audio clip, whose picture is not a frame but
// a drawing of its sound (§20 erratum 329).
func waveformAlt(title string) string {
	t := strings.Join(strings.Fields(title), " ")
	if t == "" {
		return "The waveform of an audio clip"
	}
	return "The waveform of the audio clip: " + t
}

// imageAlt describes the poster for a reader who cannot see it: what the
// picture IS (a frame the encoder chose out of this clip) plus which clip.
func imageAlt(title string) string {
	t := strings.Join(strings.Fields(title), " ")
	if t == "" {
		return "A still frame from a clip"
	}
	return "A still frame from the clip: " + t
}

// cardQualityLine states, in the card's own description, what the card PLAYS and
// what the link leads to (§20 erratum 214).
//
// It exists for one reason: when a LIGHT copy serves the card, the video a reader
// watches inside Discord is 720p30 and the clip is not, and a reader who does not
// know that concludes the product is a 720p product. The owner's call of 2026-09-08 — the same
// decision the burned-in tag comes from. Real numbers, both of them, in one short
// clause; nothing at all when the card plays the clip's own file, because then
// there is nothing to explain.
//
// "Preview 720p30 · Original 1440p60 at this link" — the second word is the TIER
// the link leads to, so a re-encoded source reads "Full" and a direct upload
// "Original", which is what the quality selector on the page will call it too.
func cardQualityLine(c db.Clip) string {
	if c.SocialKey == nil || c.State != db.ClipReady {
		return ""
	}
	preview := shortQuality(c.SocialWidth, c.SocialHeight, c.SocialFPSNum, c.SocialFPSDen)
	best := shortQuality(c.Width, c.Height, c.FPSNum, c.FPSDen)
	if preview == "" || best == "" || preview == best {
		// Nothing to explain: an unmeasured row, or a card copy that is the same
		// shape as the clip (a small calm file that got one for its SIZE alone).
		return ""
	}
	return "Preview " + preview + " · " + tierWord(bestTierName(c)) + " " + best + " " + qualityLinkWords
}

// qualityLinkWords close the quality clause. The component embed (§20 erratum
// 316) makes exactly these words the link to the page, so they are one constant.
const qualityLinkWords = "at this link"

// bestTierName is which tier the LINK leads to, in the words the quality selector
// uses: the Original when the upload is what this clip plays, the Full otherwise.
func bestTierName(c db.Clip) string {
	if c.OriginalPlayable && c.TrimStartMS == nil && c.TrimEndMS == nil {
		return db.TierNameOriginal
	}
	return db.TierNameFull
}

// shortQuality is the compact "1440p60" form of a measured tier, from the four
// nullable columns a clip row keeps it in. Empty when the row does not have it.
func shortQuality(w, h, fpsNum, fpsDen *int) string {
	if w == nil || h == nil || *w <= 0 || *h <= 0 {
		return ""
	}
	var fps transcode.Rational
	if fpsNum != nil && fpsDen != nil {
		fps = transcode.Rational{Num: *fpsNum, Den: *fpsDen}
		// A rate within 0.1 of a whole number prints the whole number (60.01 ->
		// 60, 29.97 -> 30, 23.976 -> 24): a card never shows a decimal rate.
		if fps.Valid() {
			if f := float64(fps.Num) / float64(fps.Den); math.Abs(f-math.Round(f)) < 0.1 {
				fps = transcode.Rational{Num: int(math.Round(f)), Den: 1}
			}
		}
	}
	return transcode.QualityShort(*w, *h, fps)
}

// The card description's own grammar (§20 erratum 287, amending 214).
//
// `{m:ss} · {the owner's words, clamped} · {the quality clause}` — three parts,
// in that order, each dropped when it has nothing to say. The owner's sign-off of
// 2026-09-15: the description, clamped so the clause always shows, and the
// clip's length there too.
//
//   - The DURATION goes first because it is the one fact a reader wants before
//     they click and the one Discord will not render from `video:duration` (it
//     is emitted, it is correct, and the client shows nothing at all for it —
//     measured 2026-09-14). It leads the component card's own line the same
//     way (§20 erratum 316).
//   - The owner's words are the flexible half. They may be 2000 runes
//     (internal/httpapi's DTO limit) and Discord truncates a long description in
//     the client at a length nobody has ever documented — which used to mean the
//     one clause that exists to stop a reader concluding Clippet is a 720p
//     product was the FIRST thing cut off a wordy clip.
//   - The CLAUSE always survives, which is the whole of this amendment.
//     Erratum 214 put the owner's words first on purpose and that ordering is
//     unchanged; what changes is that the owner's half is now clamped to what is
//     left of the budget instead of taking all of it.
const (
	// cardDescriptionBudget is the whole description's target length in RUNES.
	//
	// 200 is a safe target, not a measured limit: Discord's visible cut is
	// undocumented (discord-api-docs#1593 has been asking for it since 2020) and
	// the community numbers people quote for it range over a few hundred
	// characters with no first-party source. 200 is comfortably under the lowest
	// of those, it is about two lines of an embed at a normal client width, and
	// it leaves the clause — worst case about 50 runes, "Preview 1080p29.97 ·
	// Original 2160p59.94 at this link" — with room it cannot lose. A card is a
	// summary; an owner who wants 2000 runes read has the watch page, which
	// renders the description whole and always has.
	cardDescriptionBudget = 200
	// cardDescriptionFloor is the smallest slice of the owner's own words worth
	// putting on a card. Below it the words are DROPPED rather than cut to a
	// stub: "The moment where…" says less than nothing and spends the reader's
	// line on an ellipsis. It is a belt — the fixed parts cannot reach 176 runes
	// — and TestCardDescriptionFloor is what proves it stays one.
	cardDescriptionFloor = 24
	// cardDescriptionCut is the ONE character a clamped description ends with.
	cardDescriptionCut = "…"
	// cardDescriptionSep is the separator between the three parts, and it is the
	// same middle dot the bot reply, the library row and the quality clause use.
	cardDescriptionSep = " · "
)

// cardDescription is `og:description` / `twitter:description` / `description`
// for a READY clip — one string, emitted three times (the E2E's card check
// asserts the three are equal rather than re-deriving this).
//
// A clip with nothing to say in any of the three parts keeps §9.1's default
// sentence: an EMPTY description is a card with a blank row under the title.
func cardDescription(c db.Clip) string {
	var head string
	if c.DurationMS != nil && *c.DurationMS >= 0 {
		head = formatDuration(*c.DurationMS)
	}
	clause := cardQualityLine(c)
	owner := strings.Join(strings.Fields(c.Description), " ")
	if owner != "" {
		owner = clampRunes(owner, ownerBudget(head, clause))
	}
	parts := make([]string, 0, 3)
	for _, p := range []string{head, owner, clause} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return defaultDescription
	}
	return strings.Join(parts, cardDescriptionSep)
}

// ownerBudget is how many runes of the owner's own words fit beside the two
// parts that may not be cut: the budget, minus the duration, minus the clause,
// minus one separator for each part that is actually there.
func ownerBudget(head, clause string) int {
	n := cardDescriptionBudget
	for _, p := range []string{head, clause} {
		if p != "" {
			n -= len([]rune(p)) + len([]rune(cardDescriptionSep))
		}
	}
	return n
}

// clampRunes cuts s to at most n RUNES (never bytes — a multi-byte cut is a
// replacement character on the card) and marks the cut with one ellipsis.
//
// The cut backs off to the last space inside the final quarter of what is kept,
// when there is one, so the card ends on a word rather than mid-syllable; a run
// of text with no space in that window (a URL, a hash, a language that does not
// space its words) is cut where the budget falls, which is still a card and not
// a defect. A budget under cardDescriptionFloor drops the words entirely.
func clampRunes(s string, n int) string {
	if n < cardDescriptionFloor {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	keep := r[:n-1] // one rune for the ellipsis
	if i := lastSpace(keep, len(keep)-len(keep)/4); i > 0 {
		keep = keep[:i]
	}
	return strings.TrimRight(string(keep), " ") + cardDescriptionCut
}

// lastSpace is the index of the last space in r at or after `from`, or -1.
func lastSpace(r []rune, from int) int {
	if from < 0 {
		from = 0
	}
	for i := len(r) - 1; i >= from; i-- {
		if r[i] == ' ' {
			return i
		}
	}
	return -1
}

// Card bar colours (§20 erratum 287). Discord paints a ~4 px vertical rule down
// the left edge of an unfurl from the page's `theme-color`, and until this
// erratum every Clippet card got `#191816` — the dark ground, because Discord
// takes the LAST theme-color tag on the page and the shipped pair ends with the
// dark one. That is 1.29:1 against Discord's own dark embed body: the brand was
// being spent on an invisible stripe.
//
// The three values are the colour law exactly as web/STYLE.md writes it — green
// = you can paste this, amber = happening, red = loss — chosen from the
// palette's two columns on ONE criterion, the better worst-case contrast across
// Discord's light and dark grounds (the measurement's table, from the
// theme-colour test of the dev instance):
//
//	ready    #0E7C4F  light --ok       2.64 dark / 4.71 light   (Discord's own blurple: 2.99 / 4.15)
//	working  #D98E12  light --time     5.14 dark / 2.42 light
//	failed   #F0665C  dark  --danger   4.44 dark / 2.80 light   (light --danger #B3261E is 2.11 on dark, and Discord is mostly dark)
//
// No palette value clears 3:1 on both grounds — the window is a luminance band
// the brand green sits outside — so the bar clears the honest bar instead: as
// visible as Discord's own embeds. STYLE.md's palette stays closed; this adds no
// token.
const (
	cardColorReady   = "#0E7C4F"
	cardColorWorking = "#D98E12"
	cardColorFailed  = "#F0665C"
)

// CardColor is the colour Discord paints down the left edge of this clip's
// unfurl, from the ONE unqualified `theme-color` tag base.html renders after the
// browser's own light/dark pair.
//
// The ordering is the entire mechanism and it is MEASURED, not documented
// (2026-09-15, eight variants against a real channel): Discord takes the LAST
// `theme-color` on the page and does not evaluate `media` at all, while a
// browser takes the FIRST whose media matches (HTML, "obtain a page's theme
// color"). The two consumers read opposite ends of one list, so the phone's
// chrome keeps the page's ground and the card gets the state — with every client
// served byte-identical HTML and no UA sniffing anywhere. A media-less
// theme-color is precisely the specification's own fallback for a consumer that
// cannot evaluate media queries, and Discord is such a consumer: this declares a
// true fallback rather than serving a crawler something else.
//
// The risk, recorded: if Discord ever flips to first-wins the bar becomes
// `#ECEDEF`, a bright stripe — visible, wrong, harmless, and detectable by
// re-running the same probe. Neither rule breaks a card.
func CardColor(state string) string {
	switch state {
	case db.ClipReady:
		return cardColorReady
	case db.ClipFailed:
		return cardColorFailed
	default:
		return cardColorWorking
	}
}

func readyMeta(v ClipView) []Meta {
	c, u := v.Clip, v.URLs
	desc := cardDescription(c)
	social, poster := u.Social(c.ID), u.Poster(c.ID)
	// The card's video dims describe the SOCIAL file, not the rendition.
	videoW, videoH := socialDims(c)

	m := []Meta{
		prop("og:title", c.Title),
		prop("og:description", desc),
		prop("og:type", "video.other"),
		prop("og:url", u.Page(c.ID)),
		prop("og:site_name", siteNameOf(u.SiteName())),
		prop("og:image", poster),
		prop("og:image:type", posterType(c)),
	}
	// Poster dims are the JPEG's MEASURED dims — never the video's (§9.1).
	// A ready row without them (impossible via FinalizeTranscode) omits the
	// pair rather than faking it from the rendition: that was the shipped bug.
	m = appendDims(m, prop, "og:image:width", "og:image:height", c.PosterWidth, c.PosterHeight)
	alt := imageAlt(c.Title)
	if c.IsAudio() {
		alt = waveformAlt(c.Title)
	}
	m = append(m, prop("og:image:alt", alt))
	m = append(m, prop("og:video", social), prop("og:video:url", social))
	if u.IsHTTPS() {
		m = append(m, prop("og:video:secure_url", social))
	}
	m = append(m, prop("og:video:type", "video/mp4"))
	m = appendDims(m, prop, "og:video:width", "og:video:height", videoW, videoH)
	if secs := durationSeconds(c.DurationMS); secs != "" {
		m = append(m, prop("video:duration", secs))
	}
	if c.IsAudio() {
		// The SOUND itself, beside the audiogram (§20 erratum 329): Open Graph's
		// own audio property, naming the file the page's <audio> plays under its
		// real type. iMessage reads it; Discord ignores it (the round-2
		// research §3/§4) — harmless there, and the card it draws is the
		// audiogram og:video names. Two tags on an audio card only: a video's
		// inventory (§9.1) is unchanged.
		if t := db.AudioPlayType(c); t != "" {
			m = append(m, prop("og:audio", u.Audio(c.ID)), prop("og:audio:type", t))
		}
	}
	m = append(m,
		name("twitter:card", "player"),
		name("twitter:title", c.Title),
		name("twitter:description", desc),
		name("twitter:image", poster),
		// X's own spelling of the sentence Discord reads off og:image:alt
		// (§20 erratum 177). It describes the SAME picture, so it is the SAME
		// sentence: a card that told one reader what its poster shows and
		// left the other without it was half an accessibility affordance.
		name("twitter:image:alt", alt),
		name("twitter:player", u.Embed(c.ID)),
	)
	m = appendDims(m, name, "twitter:player:width", "twitter:player:height", videoW, videoH)
	m = append(m,
		name("twitter:player:stream", social),
		name("twitter:player:stream:content_type", "video/mp4"),
		name("description", desc),
	)
	return m
}

// BuildCollectionMeta is the card of a collection page (§9.4, §17 #9). It is
// a LINK-OUT card and only ever that: a collection is a list of several clips,
// so there is no single video to put behind `og:video` and no player to put
// behind `twitter:player` — an unfurler that tried would have to pick one clip
// for the reader, which is exactly the choice the list exists to leave open.
//
// The picture is the poster of the first READY member, which is the closest
// thing to "what this list is": a real frame from a real clip in it. A
// collection with no ready member yet has no picture at all — the placeholder
// PNG belongs to a clip that is still processing and says "processing", which
// is not what an empty collection is — so the card degrades to `summary`,
// Twitter's own card for "a link with no image".
func BuildCollectionMeta(v CollectionView) []Meta {
	c, u := v.Collection, v.URLs
	page := u.Collection(c.ID)
	m := []Meta{
		prop("og:title", c.Title),
		prop("og:description", defaultDescription),
		prop("og:type", "website"),
		prop("og:url", page),
		prop("og:site_name", siteNameOf(u.SiteName())),
	}
	poster := firstPoster(v)
	if poster != nil {
		m = append(m,
			prop("og:image", u.Poster(poster.ID)),
			prop("og:image:type", posterType(*poster)))
		m = appendDims(m, prop, "og:image:width", "og:image:height", poster.PosterWidth, poster.PosterHeight)
	}
	card := "summary"
	if poster != nil {
		card = "summary_large_image"
	}
	m = append(m,
		name("twitter:card", card),
		name("twitter:title", c.Title),
		name("twitter:description", defaultDescription))
	if poster != nil {
		m = append(m, name("twitter:image", u.Poster(poster.ID)))
	}
	return append(m, name("description", defaultDescription))
}

// firstPoster returns the first member that is ready AND has a poster, or nil.
func firstPoster(v CollectionView) *db.Clip {
	for i := range v.Clips {
		c := &v.Clips[i]
		if c.State == db.ClipReady && c.PosterKey != nil {
			return c
		}
	}
	return nil
}

func linkOutMeta(v ClipView) []Meta {
	c, u := v.Clip, v.URLs
	desc := statusProcessing
	if c.State == db.ClipFailed {
		desc = statusFailed
	}
	img := u.Static(placeholderName)
	// The alt sentence is the picture's, so it sits where the ready card keeps
	// its own: immediately after the image it describes, once per consumer.
	alt := placeholderAlt(c.State)
	return []Meta{
		prop("og:title", c.Title),
		prop("og:description", desc),
		prop("og:type", "website"),
		prop("og:url", u.Page(c.ID)),
		prop("og:site_name", siteNameOf(u.SiteName())),
		prop("og:image", img),
		prop("og:image:type", "image/png"),
		prop("og:image:width", placeholderWidth),
		prop("og:image:height", placeholderHeight),
		prop("og:image:alt", alt),
		name("twitter:card", "summary_large_image"),
		name("twitter:title", c.Title),
		name("twitter:description", desc),
		name("twitter:image", img),
		name("twitter:image:alt", alt),
		name("description", desc),
	}
}
