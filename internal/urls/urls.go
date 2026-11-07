// Package urls builds every absolute URL Clippet emits (share pages, embed
// pages, media routes, static assets) from the configured PUBLIC_BASE_URL
// (DESIGN.md §9, §18.1). A Builder is constructed once at boot and used at
// request time; all methods are pure.
package urls

import (
	"fmt"
	"net/url"
	"strings"
)

// Builder renders absolute URLs under one origin.
type Builder struct {
	base    url.URL
	buildID string
}

// New returns a Builder for base. A trailing slash on base.Path is trimmed;
// config.Load already rejects any other path, so Builder only ever sees an
// origin. buildID (the binary version) is appended to Static URLs as ?v=…
// for cache busting.
func New(base *url.URL, buildID string) Builder {
	var b url.URL
	if base != nil {
		b = *base
	}
	b.Path = strings.TrimRight(b.Path, "/")
	b.RawPath = ""
	b.RawQuery = ""
	b.Fragment = ""
	b.User = nil
	return Builder{base: b, buildID: buildID}
}

// Origin returns scheme://host[:port] with no trailing slash — the value the
// CSRF check compares the Origin header against.
func (b Builder) Origin() string {
	return b.base.Scheme + "://" + b.base.Host
}

// IsHTTPS reports whether the public base uses https (drives the cookie
// Secure flag and og:video:secure_url).
func (b Builder) IsHTTPS() bool {
	return b.base.Scheme == "https"
}

// Abs returns the absolute URL for an app-relative path such as "/embed/x".
// A missing leading slash is added.
func (b Builder) Abs(path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return b.Origin() + b.base.Path + path
}

// Page is the share page URL, and since §20 erratum 248 the id sits at the
// ROOT of the origin — `{PUBLIC_BASE_URL}/{id}`, with no `/clips/` in front of
// it. It is the shortest a link on a domain of one's own can be, which is most
// of the point of having bought one, and this is the ONE place it is rendered:
// the DTOs, `og:url`, the copy control, the library rows, the collection rows,
// the embed overlay, the Discord post, the sxcu reply and the phone share
// target's 303 all go through it.
func (b Builder) Page(id string) string { return b.Abs("/" + id) }

// PageShort is the share page as a person READS it — the configured host and
// the id, with the scheme left off (§20 erratum 264). It is not an address to
// fetch and nothing links to it: its one caller is the Link control's hint,
// which shows one example of each id grammar on THIS instance's own host, where
// the scheme is noise and the width of a row is real. It is built from the same
// base every other method uses, so an operator who reads the hint reads the
// host their links will actually carry.
func (b Builder) PageShort(id string) string { return b.Host() + "/" + id }

// Host is this instance as a person NAMES it — the configured host and any base
// path, with the scheme left off. PageShort is this plus an id; Host alone
// exists for the one caller that puts the instance inside a SENTENCE rather
// than into a link: the Discord command's description, which has a hundred
// characters to say what `/clip` does and where a file Discord refuses can go
// instead (§20 erratum 268). Built from the same base every other method uses,
// so the name in that sentence is the host the links will actually carry.
func (b Builder) Host() string { return b.base.Host + b.base.Path }

// ParseShare reads a share link OF THIS INSTANCE back into the path segment it
// names — the reverse of Page, LegacyPage and PageShort, and the only thing in
// the binary that turns a URL somebody typed into a clip. Written for the
// Discord bot's `link` option (§20 erratum 268), widened by erratum 275(b),
// removed with that option by erratum 286 and RESTORED unchanged by erratum
// 315 for `/clip link:`, the command that posts a clip already here as a card.
//
// It is a PARSER and never a fetcher. Nothing in this package opens a socket,
// and answering "is this one of ours, and which clip" HERE is precisely what
// makes a `link` option not a fetch option: a URL naming any other host is a
// `false` in a pure function, before any code that could act on it runs.
//
// The spellings it accepts are the ones this instance itself emits, and no
// others:
//
//	{origin}/{id}          Page — the address since erratum 248
//	{origin}/clips/{id}    LegacyPage — what every link sent before it names
//	{host}/{id}            PageShort — the scheme-less form erratum 264 SHOWS
//	{origin}/embed/{id}    Embed — the player somebody copied out of an iframe
//
// plus the wrappers Discord itself puts round a pasted link: `<…>` (suppress the
// unfurl), `||…||` (spoiler), and either inside the other, because a person
// pasting into a chat box has been trained to type both and the brackets belong
// to Discord rather than to the address. The trim runs until nothing more comes
// off, which is what makes `<<…>>` — Discord's own doubled form — one link and
// not a refusal (§20 erratum 275).
//
// `/embed/{id}` is ACCEPTED as of erratum 275 and names the same clip: it is
// this instance's own player URL for exactly that clip, the reply re-issues the
// canonical share link as it does for every other accepted spelling, and
// refusing it made the bot say "that is not a clip link from this Clippet" about
// an address this Clippet had emitted. `/c/{id}` stays refused: a collection is
// not a clip, and "post this clip" has no answer for a list of them.
//
// It returns the SEGMENT the link names, not a validated id: `ids.Valid` is the
// caller's check, made in the package where every `{id}` route already makes it
// (§8), which is what keeps this package's dependency set at net/url.
func (b Builder) ParseShare(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	// Discord's wrappers, until nothing more comes off: `<…>`, `||…||`, and
	// either nested in the other. Bounded (four rounds is two of each, which is
	// past anything a chat box produces) so that this stays a pure function over
	// a string with no loop a caller can lengthen.
	for i := 0; i < 4; i++ {
		t := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(s, "<"), ">"))
		t = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(t, "||"), "||"))
		if t == s {
			break
		}
		s = t
	}
	if s == "" {
		return "", false
	}
	if !strings.Contains(s, "//") {
		// The scheme-less form. It is given the base's own scheme so that
		// net/url parses the host as a HOST: without one, "clippet.gg/x" is a
		// relative reference whose whole path is "clippet.gg/x".
		s = b.base.Scheme + "://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.User != nil || u.Opaque != "" {
		return "", false
	}
	// Either scheme names the same clip — a link pasted out of a browser that
	// was on http reaches the same page — but nothing else does: a mailto:,
	// a file: or a javascript: is not a share link of anything.
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return "", false
	}
	// The host decides, and it is matched WHOLE, port included: a host is
	// case-insensitive, `clippet.gg.evil.test` is not `clippet.gg`, and a
	// different port is a different origin. `u.Host` is the AUTHORITY minus any
	// userinfo, which is what makes `clippet.gg@evil.example` — where the host
	// is what follows the `@` — a mismatch rather than a match; the userinfo
	// check above refuses that spelling outright anyway, and both are tested.
	if !strings.EqualFold(u.Host, b.base.Host) {
		return "", false
	}
	// EscapedPath rather than Path: an id is [A-Za-z0-9] or capitalised words,
	// so no real link carries an escape, and comparing the raw form means a
	// percent-encoded separator cannot decode its way into a second segment.
	p, ok := strings.CutPrefix(strings.TrimSuffix(u.EscapedPath(), "/"), b.base.Path)
	if !ok {
		return "", false
	}
	if p, ok = strings.CutPrefix(p, "/"); !ok {
		return "", false
	}
	if rest, legacy := strings.CutPrefix(p, "clips/"); legacy {
		p = rest
	} else if rest, embed := strings.CutPrefix(p, "embed/"); embed {
		// The player URL of the same clip (erratum 275). One prefix, one clip.
		p = rest
	} else if p == "clips" || p == "embed" {
		// `/clips` and `/clips/` name the LEGACY PREFIX and no clip at all, and
		// `/embed` likewise. Trimmed rather than cut, this used to come back as
		// the segment "clips" — which `ids.Valid` refuses, so nothing ever acted
		// on it, but "this link names a clip" would have been true of a link
		// that does not.
		return "", false
	}
	if p == "" || strings.Contains(p, "/") {
		return "", false
	}
	return p, true
}

// SiteName is the instance's own PUBLIC HOST, with no scheme, no port and no
// base path: `clippet.gg`, `dev.clippet.gg`, a self-hoster's
// `clips.example.com` (§20 erratum 287; 2026-09-15: the small grey line Discord
// draws above a card's title should read the host, `clippet.gg`).
//
// It is the one home for that string, which is why it lives here and not in
// internal/web beside `og:site_name` or in internal/httpapi beside
// `provider_name`: DESIGN §3 forbids an import in either direction between
// those two packages, both of them already hold a Builder, and both must say
// the same thing on the same card (Discord reads `og:site_name` for the
// provider line of a pasted link and `provider_name` for the same line of an
// oEmbed-discovered one).
//
// Port and path are dropped deliberately. A card's provider line is READ, not
// fetched — `:8481` and `/clippet` are addressing detail that says nothing
// about whose clip this is — and Hostname() is also what unbracket an IPv6
// literal, so a bare-IP instance reads as its address rather than as `[::1]`.
//
// It can only be empty for a Builder with no base at all (the zero value, which
// no running server has); the two callers fall back to the product's own
// spelling rather than emit a blank provider line.
func (b Builder) SiteName() string { return b.base.Hostname() }

// LegacyPage is the address Page replaced, `/clips/{id}`, and it is built here
// because it is still a REAL address rather than history: every link already
// sent names it and gets a permanent redirect to Page (internal/web), and the
// oEmbed endpoint still accepts it as a `url=` parameter. One spelling, in the
// package that owns what a URL of this instance looks like (§20 erratum 248).
func (b Builder) LegacyPage(id string) string { return b.Abs("/clips/" + id) }

// Inbound is an inbound URL (§20 erratum 322): the address a tool POSTs a file
// to, with the raw `ci_` secret as its last segment. It is built from
// PUBLIC_BASE_URL and never from a request's Host header, and it exists only
// in the one mint response (or the shell's stdout) that shows it once.
func (b Builder) Inbound(raw string) string { return b.Abs("/in/" + raw) }

// AutoFetch is a Clippet Auto URL (§20 erratum 324): the address Clippet Auto
// POSTs one Discord attachment's description to, with the raw `ca_` secret as
// its last segment. Built from PUBLIC_BASE_URL, as Inbound is.
func (b Builder) AutoFetch(raw string) string { return b.Abs("/in/auto/" + raw) }

// Embed is the chrome-less player URL (twitter:player).
func (b Builder) Embed(id string) string { return b.Abs("/embed/" + id) }

// Video is the progressive MP4 the <video> element loads.
func (b Builder) Video(id string) string { return b.Abs("/api/clips/" + id + "/video.mp4") }

// Audio is an audio clip's best playable tier under its real media type (§20
// erratum 328): what the watch page's <audio> plays.
func (b Builder) Audio(id string) string { return b.Abs("/api/clips/" + id + "/audio") }

// Social is the stable card-file route used by crawler tags. It is the LIGHT
// tier's ORIGINAL name and stays the URL the card points at (§20 erratum 210):
// every Discord message already posted, and every crawler cache keyed on it,
// names this path, and a card tag that moved would re-fetch nothing while the
// old URL kept working anyway. `Light` below is the same object's canonical
// name for a client that is reading the tier list.
func (b Builder) Social(id string) string { return b.Abs("/api/clips/" + id + "/social.mp4") }

// Light is the canonical route of the LIGHT tier — the same object `Social`
// serves, at the name the DTO, the docs and the quality selector use. Two names
// for one route is deliberate and bounded: the tier was renamed where people
// read it, and the old path is kept working for the cards that already exist
// (§20 erratum 210).
func (b Builder) Light(id string) string { return b.Abs("/api/clips/" + id + "/light.mp4") }

// Original is the uploaded bytes. It has always been the owner's download route
// and is ALSO, since the quality tiers, what the watch page streams when the
// upload is web-playable as it stands (§20 erratum 207) — the same URL, with
// `?download=1` for the save.
func (b Builder) Original(id string) string { return b.Abs("/api/clips/" + id + "/original") }

// Poster is the poster JPEG URL (og:image).
func (b Builder) Poster(id string) string { return b.Abs("/api/clips/" + id + "/poster.jpg") }

// GIF is the on-demand preview GIF route (§17 #6). Like every other media
// URL it is stable for the life of the clip: the object behind it changes
// when the clip is trimmed and the preview is remade, the address does not.
func (b Builder) GIF(id string) string { return b.Abs("/api/clips/" + id + "/preview.gif") }

// Filmstrip is the on-demand trim-scrubber sprite sheet (§20 erratum 220), and
// it is stable for the life of the clip for the GIF's reason exactly: the sheet
// behind it is remade after a cut, the address is not.
func (b Builder) Filmstrip(id string) string {
	return b.Abs("/api/clips/" + id + "/filmstrip.jpg")
}

// Collection is the public page of a collection (§17 #9). It is short on
// purpose — a collection link is pasted beside clip links, and one letter is
// as little as a prefix can cost now that a CLIP link has none at all (§20
// erratum 248 left `/c/` alone deliberately: a collection is not a clip, and
// the two spaces stay separate) — and it is unguessable for exactly the same
// reason a clip link is: the id is 12 crypto-random characters and there is no
// listing anywhere that enumerates them.
func (b Builder) Collection(id string) string { return b.Abs("/c/" + id) }

// Progress is the polling endpoint the JS client uses.
func (b Builder) Progress(id string) string { return b.Abs("/api/clips/" + id + "/progress") }

// Static returns the URL of an embedded static asset with a ?v=buildID cache
// buster (omitted when the Builder has no build id).
func (b Builder) Static(name string) string {
	u := b.Abs("/static/" + strings.TrimPrefix(name, "/"))
	if b.buildID == "" {
		return u
	}
	return u + "?v=" + url.QueryEscape(b.buildID)
}

// The committed link-out card image and its dimensions: the picture a clip
// that is not ready shows on a Discord/X card and in an oEmbed document
// (§9.1). It lives HERE, in the package both renderers already use to build
// its URL, because internal/web (the share page's meta tags) and
// internal/httpapi (the oEmbed thumbnail) had a private copy each and neither
// imports the other — so renaming the file left every test green while oEmbed
// shipped a `thumbnail_url` that 404s. The file itself is web/static/,
// and internal/web's static test decodes it and pins these two numbers
// against the real PNG.
const (
	PlaceholderCard   = "card-placeholder.png"
	PlaceholderWidth  = 1200
	PlaceholderHeight = 630
)

// defaultEmbedWidth/Height size a snippet for a clip that carries no
// measurements. A ready clip always has them (FinalizeTranscode writes the
// whole set), so this is a floor and never the normal case.
const defaultEmbedWidth, defaultEmbedHeight = 640, 360

// EmbedCode is the exact <iframe> that embeds a clip — what the share page's
// "Copy embed" puts on the clipboard AND what the oEmbed document's `html`
// carries (§20 erratum 152). It lives here, in the package that owns what a
// URL of this instance looks like, so those two can never drift into two
// different players.
//
// The box is sized from the RENDITION's own dimensions so it matches the
// clip's aspect — a 9:16 phone capture pasted into somebody's 16:9 hole is two
// black bars and a strip. The src is ABSOLUTE: the snippet is going onto a
// page on another origin, where a same-origin path means nothing.
// `allow="autoplay"` is there for the reason DEPLOY §9 tells integrators to
// add it — a cross-origin iframe cannot autoplay without it — and costs
// nothing when the URL asks for no autoplay, which is the default.
func EmbedCode(embedURL string, width, height *int) string {
	w, h := defaultEmbedWidth, defaultEmbedHeight
	if width != nil && height != nil && *width > 0 && *height > 0 {
		w, h = *width, *height
	}
	return fmt.Sprintf(`<iframe src="%s" width="%d" height="%d" frameborder="0" allowfullscreen allow="autoplay"></iframe>`,
		embedURL, w, h)
}
