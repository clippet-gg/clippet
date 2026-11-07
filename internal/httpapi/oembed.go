package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/ids"
	"clippet/internal/urls"
)

// oEmbed (ROADMAP #18, §20 erratum 152). Slack's documented unfurl order is
// oEmbed first and the OG/Twitter tags second, discovered through a
// `<link rel="alternate" type="application/json+oembed">` on the page. Discord
// and X read the tags and are COMPLETELY UNAFFECTED by this file — nothing
// about §9.1's inventory changes — so the whole feature is additive reach: a
// consumer that shows a title-and-thumbnail card today shows the player.
//
// The discipline is §9.1's, unchanged:
//
//   - the id must be `ids.Valid` before any database access (the cheapest scan
//     defence), a missing or `deleting` clip is 404, and a backend failure is
//     503 and never a 404 — a consumer must not cache "no embed" for a clip
//     that hit a hiccup;
//   - an UNLISTED clip answers exactly as a listed one does, because the share
//     page does: "unlisted" hides a clip from the library, and the link is the
//     authentication (spec §2);
//   - it is anonymous and under the `view` limiter, like every other route a
//     stranger holding a link can reach.
//
// The one rule that is this route's own is the HOST CHECK. An oEmbed endpoint
// takes a URL from the caller and describes it; without a check it would
// describe anything, which makes it a small open proxy for metadata and a
// place to point a consumer at somebody else's site under this instance's
// name. Only a URL on PUBLIC_BASE_URL's own origin is answered.

const (
	// oembedProvider is the FALLBACK for provider_name. Since §20 erratum 287
	// the line a document CARRIES is the instance's own public HOST, exactly as
	// `og:site_name` does on the share page — Slack renders the provider line,
	// Discord renders it above the title, and on both it now reads `clippet.gg`
	// or `dev.clippet.gg` or a self-hoster's own name. The host has ONE home,
	// urls.Builder.SiteName, which this package already holds; only the fallback
	// is duplicated here.
	//
	// It is the same string as internal/web/meta.go's `siteName`, and THAT
	// duplicate is DELIBERATE: a provider name is a wire value here and a page
	// tag there, DESIGN §3 forbids an import in either direction between these
	// two packages, so there is no home that is not one of them. Nothing but
	// check.sh pins the two together — change one and change the other. (The
	// placeholder card, which a drift would turn into a 404, does have a shared
	// home: internal/urls.)
	oembedProvider      = "clippet"
	oembedDefaultWidth  = 640
	oembedDefaultHeight = 360
	oembedMaxDimension  = 10000
	// The three path shapes a `url=` may carry. oembedRootPrefix is the share
	// page since §20 erratum 248 (the id at the root); oembedClipPrefix is the
	// address it replaced, still accepted because a consumer's stored URL — and
	// every link already pasted — names it; oembedEmbedPrefix is the player
	// page a consumer may have discovered us from instead.
	oembedRootPrefix              = "/"
	oembedClipPrefix              = "/clips/"
	oembedEmbedPrefix             = "/embed/"
	oembedUnsupportedFormatDetail = "only format=json is supported"
)

// oembedDTO is the oEmbed 1.0 document. `video` (a ready clip) carries `html`
// — the same iframe the share page's "Copy embed" hands out — and `link`
// (anything else) carries no html at all, which is the spec's own way of
// saying "there is nothing to play here yet".
type oembedDTO struct {
	Version      string `json:"version"`
	Type         string `json:"type"`
	ProviderName string `json:"provider_name"`
	ProviderURL  string `json:"provider_url"`
	Title        string `json:"title"`
	HTML         string `json:"html,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
	ThumbnailURL string `json:"thumbnail_url,omitempty"`
	ThumbnailW   int    `json:"thumbnail_width,omitempty"`
	ThumbnailH   int    `json:"thumbnail_height,omitempty"`
}

// oembed is GET /oembed?url=…&format=json[&maxwidth=&maxheight=].
func (h *handler) oembed(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch f := strings.ToLower(strings.TrimSpace(q.Get("format"))); f {
	case "", "json":
	case "xml":
		// The spec's own answer for a format a provider does not implement.
		httpx.WriteError(w, http.StatusNotImplemented, "unsupported_format", oembedUnsupportedFormatDetail)
		return
	default:
		writeInvalid(w, oembedUnsupportedFormatDetail)
		return
	}
	raw := q.Get("url")
	if raw == "" {
		writeInvalid(w, "url is required")
		return
	}
	id, ok := h.oembedClipID(raw)
	if !ok {
		// A URL that is not one of ours is NOT told why. 404 is also what a
		// consumer gets for a clip that has gone, and the two being one answer
		// is what stops the route from reporting on other people's hosts.
		writeNotFound(w)
		return
	}
	maxW, ok := oembedDimension(w, q.Get("maxwidth"), "maxwidth")
	if !ok {
		return
	}
	maxH, ok := oembedDimension(w, q.Get("maxheight"), "maxheight")
	if !ok {
		return
	}
	if h.d.DB == nil {
		writeDBError(w, r, errNoDB)
		return
	}
	c, err := h.d.DB.GetClip(r.Context(), id)
	switch {
	case errors.Is(err, db.ErrNotFound):
		writeNotFound(w)
		return
	case err != nil:
		writeDBError(w, r, err)
		return
	case c.State == db.ClipDeleting:
		writeNotFound(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h.oembedDoc(c, maxW, maxH))
}

// providerNameOf is `provider_name`: the instance's own public host (§20
// erratum 287), which is what a consumer that renders the provider line — Slack
// does, Discord does — shows above the title. It is internal/web's `siteNameOf`
// over the same rule for the oEmbed document, duplicated for the reason the two
// constants are (DESIGN §3). An empty host — the URL builder's zero value, which
// no running server has — falls back to the product's own spelling rather than
// leaving the line blank.
func providerNameOf(host string) string {
	if host == "" {
		return oembedProvider
	}
	return host
}

// oembedDoc renders the document for one clip.
func (h *handler) oembedDoc(c db.Clip, maxW, maxH int) oembedDTO {
	u := h.d.URLs
	doc := oembedDTO{
		Version:      "1.0",
		Type:         "link",
		ProviderName: providerNameOf(u.SiteName()),
		ProviderURL:  u.Origin(),
		Title:        c.Title,
	}
	if c.State != db.ClipReady {
		// The link-out card's own picture, and no `html`: there is nothing to
		// embed for a clip that is still encoding or has failed, and inventing
		// a player for it would give a consumer a black box to cache.
		// The three facts about that picture live in internal/urls, beside
		// the Static() that builds its URL, so this document and the share
		// page's own card cannot disagree about which file it is or how big
		// it is (the drift would be a thumbnail_url that 404s).
		doc.ThumbnailURL = u.Static(urls.PlaceholderCard)
		doc.ThumbnailW, doc.ThumbnailH = urls.PlaceholderWidth, urls.PlaceholderHeight
		return doc
	}
	width, height := oembedDefaultWidth, oembedDefaultHeight
	if c.Width != nil && c.Height != nil && *c.Width > 0 && *c.Height > 0 {
		width, height = *c.Width, *c.Height
	}
	width, height = fitBox(width, height, maxW, maxH)
	doc.Type = "video"
	doc.Width, doc.Height = width, height
	// The SAME snippet the share page's "Copy embed" hands out, from the same
	// function: an oEmbed consumer and a person pasting into a forum must not
	// end up with two different players (§20 erratum 152).
	doc.HTML = urls.EmbedCode(u.Embed(c.ID), &width, &height)
	doc.ThumbnailURL = u.Poster(c.ID)
	if c.PosterWidth != nil && c.PosterHeight != nil {
		// The JPEG's MEASURED dimensions, never the video's (§9.1's rule for
		// og:image, and the bug it was written for).
		doc.ThumbnailW, doc.ThumbnailH = *c.PosterWidth, *c.PosterHeight
	}
	return doc
}

// oembedClipID validates the `url` parameter and returns the clip id it names.
//
// The host check is the point of the function. A URL is ours when, after
// dropping a default port, its scheme and its host are byte-equal to
// PUBLIC_BASE_URL's (host lowercased — hosts are case-insensitive; nothing
// else normalised, so `example.com.`, `xn--…` against a unicode base and
// `example.com:8080` against a bare one are all simply NOT this origin).
// Userinfo is refused outright: a value spelling our own host, an `@`, and
// then somebody else's has THEIR host and reads, to a person skimming, as
// ours.
//
// The path must be exactly `/{id}`, `/clips/{id}` or `/embed/{id}` — a
// consumer that discovered us from any of those pages sends back the URL it
// was on — with a valid id and nothing after it. `/clips/{id}` is kept
// alongside the root form (§20 erratum 248) because a link somebody pasted
// last year is exactly the input this parameter carries, and answering 404 for
// it would break a card that still works everywhere else. A query string and a
// fragment are IGNORED rather than refused: consumers routinely carry tracking
// parameters, and they name the same clip.
//
// The prefixes cannot shadow one another: `ids.Valid` admits neither a slash
// nor a dot, so `/clips/x` never survives the root prefix and no page path
// survives any of them.
func (h *handler) oembedClipID(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil {
		return "", false
	}
	base, err := url.Parse(h.d.URLs.Origin())
	if err != nil {
		return "", false
	}
	if !strings.EqualFold(u.Scheme, base.Scheme) {
		return "", false
	}
	if hostKey(u.Host, u.Scheme) != hostKey(base.Host, base.Scheme) {
		return "", false
	}
	for _, prefix := range []string{oembedClipPrefix, oembedEmbedPrefix, oembedRootPrefix} {
		if id, found := strings.CutPrefix(u.Path, prefix); found && ids.Valid(id) {
			return id, true
		}
	}
	return "", false
}

// hostKey lowercases a host and drops the scheme's default port, so a plain
// http origin written with an explicit `:80` and the same one without it are
// one origin — the only normalisation applied, and one that cannot widen the
// set of accepted hosts.
func hostKey(host, scheme string) string {
	host = strings.ToLower(host)
	switch strings.ToLower(scheme) {
	case "http":
		return strings.TrimSuffix(host, ":80")
	case "https":
		return strings.TrimSuffix(host, ":443")
	}
	return host
}

// oembedDimension parses maxwidth/maxheight: absent or empty = no bound; a
// positive integer up to oembedMaxDimension = that bound; anything else is a
// 400 naming the parameter, because a consumer sending `maxwidth=big` has a
// bug it should hear about rather than silently get an unbounded player.
func oembedDimension(w http.ResponseWriter, raw, name string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 || n > oembedMaxDimension {
		writeInvalid(w, name+" must be a positive integer up to "+strconv.Itoa(oembedMaxDimension))
		return 0, false
	}
	return n, true
}

// fitBox scales w×h down into maxW×maxH, keeping the aspect ratio. It never
// scales UP: `maxwidth` is a ceiling the consumer can accommodate, not a size
// it is asking for, and a 640×360 clip blown up to 1920 would be a blurry
// player nobody asked for. Rounding can never produce 0.
func fitBox(w, h, maxW, maxH int) (int, int) {
	scale := 1.0
	if maxW > 0 && w > maxW {
		scale = float64(maxW) / float64(w)
	}
	if maxH > 0 && h > maxH {
		if s := float64(maxH) / float64(h); s < scale {
			scale = s
		}
	}
	if scale >= 1 {
		return w, h
	}
	nw, nh := int(float64(w)*scale), int(float64(h)*scale)
	return max(nw, 1), max(nh, 1)
}
