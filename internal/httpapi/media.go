package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"clippet/internal/auth"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/store"
	"clippet/internal/transcode"
)

// Media caching is short and revalidated by ETag — never immutable, because
// bytes may change under the stable routes (§8.5). The owner-only original
// is private: a shared cache must never hand it to the next caller of the
// same URL (RFC 9111 §3.5 treats "public" as the opt-in for that).
const (
	mediaCacheControl   = "public, max-age=3600"
	privateCacheControl = "private, max-age=3600"
	credentialVary      = "Cookie, Authorization"
)

// object is the (key, size, sha256) triple plus presentation of one media
// route, resolved entirely from the clip row.
type object struct {
	key         string
	size        int64
	etag        string
	contentType string
	modTime     time.Time
	filename    string // non-empty → Content-Disposition: attachment
	private     bool   // owner-only: private cache directive + Vary
	// redirectable: this object MAY be answered as a 302 to a presigned URL
	// on the bucket's own origin when MEDIA_DELIVERY=redirect (§20 erratum
	// 278). The zero value is the safe one — a kind nobody has thought about
	// keeps being proxied rather than silently acquiring a public URL.
	//
	// THREE kinds set it: the two video routes and the poster, which are the
	// card's and the player's payload and the whole of what the Cloudflare
	// rule is about. Three do not:
	//
	//   - `/original` is the one object with a real identity gate (erratum
	//     207), and a presigned URL cannot carry one.
	//   - `preview.gif` and `filmstrip.jpg` are the two media routes whose
	//     STATUS this app's own JavaScript polls with `fetch` (errata 226(b)
	//     and 227: a HEAD, because the answer wanted is the status and the
	//     picture is fetched by the <img> that wants it anyway). A `fetch`
	//     follows a cross-origin 302 into a CORS check, so its response would
	//     be unreadable — the GIF button would poll out and the trim scrubber
	//     would never get its sheet — unless `connect-src` were widened AND
	//     the bucket served CORS headers, which is exactly the "no CORS
	//     configuration" a BYO bucket is promised (DEPLOY §7). They are also
	//     small, on-demand and owner-facing, so keeping them app-served costs
	//     nothing against a rule that is about video.
	redirectable bool
}

type mediaKind int

const (
	mediaVideo mediaKind = iota
	mediaSocial
	mediaPoster
	mediaGIF
	mediaFilmstrip
	mediaOriginal
	// mediaAudio is an audio clip's best playable tier (§20 erratum 328).
	mediaAudio
)

func (k mediaKind) String() string {
	switch k {
	case mediaVideo:
		return "video.mp4"
	case mediaSocial:
		return "social.mp4"
	case mediaPoster:
		return "poster.jpg"
	case mediaGIF:
		return "preview.gif"
	case mediaFilmstrip:
		return "filmstrip.jpg"
	case mediaAudio:
		return "audio"
	default:
		return "original"
	}
}

func (h *handler) video(w http.ResponseWriter, r *http.Request)    { h.serveMedia(w, r, mediaVideo) }
func (h *handler) social(w http.ResponseWriter, r *http.Request)   { h.serveMedia(w, r, mediaSocial) }
func (h *handler) poster(w http.ResponseWriter, r *http.Request)   { h.serveMedia(w, r, mediaPoster) }
func (h *handler) original(w http.ResponseWriter, r *http.Request) { h.serveMedia(w, r, mediaOriginal) }

// serveMedia applies the §8.5 step-1 rules then hands the triple to
// serveObject. No storage call happens before the single ranged GET.
func (h *handler) serveMedia(w http.ResponseWriter, r *http.Request, kind mediaKind) {
	c, ok := h.lookupClip(w, r)
	if !ok {
		return
	}
	if kind == mediaOriginal {
		// The uploaded bytes are anonymous-readable in exactly one case: they
		// ARE the clip, and the watch page is streaming them (§20 erratum 207,
		// db.OriginalIsTheClip — playable, not remuxed, untrimmed, ready).
		// Everything else keeps the owner-only gate this route has always had,
		// including a failed clip's upload, which is the owner's rescue copy.
		//
		// The three refusals below reproduce, in this order, exactly what the
		// route answered before: 401 for an anonymous caller (auth.RequireSession
		// used to write it from the chain), 403 `token_not_allowed` for a Bearer
		// token — erratum 19's rule, and the reason this check is here rather
		// than a bare ownership test — and 403 `forbidden` for a session that is
		// not the owner's. The route no longer carries RequireSession because the
		// answer now depends on the ROW, which a middleware cannot see.
		if !db.OriginalIsTheClip(c) {
			p, ok := auth.FromContext(r.Context())
			switch {
			case !ok:
				httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
				return
			case p.Via == auth.ViaToken:
				httpx.WriteError(w, http.StatusForbidden, "token_not_allowed", "this endpoint requires a browser session")
				return
			case p.User.ID != c.OwnerID:
				httpx.WriteError(w, http.StatusForbidden, "forbidden", "only the owner may download the original")
				return
			}
		}
	} else if c.State != db.ClipReady {
		w.Header().Set("Cache-Control", "no-store")
		httpx.WriteError(w, http.StatusConflict, "not_ready", "the clip is still "+c.State)
		return
	}
	if kind == mediaAudio && !c.IsAudio() {
		// A video has no audio object; the route is not for it (§20 erratum 328).
		w.Header().Set("Cache-Control", "no-store")
		httpx.WriteError(w, http.StatusNotFound, "not_found", "this clip is not audio")
		return
	}
	obj, ok := mediaObject(c, kind)
	if !ok {
		// A ready row with NULL keys cannot happen (finalize writes the
		// whole triple): an invariant violation that needs a human (§12),
		// answered 503 — never 404, never a misleading not_ready.
		httpx.SetLogField(r, "invariant", "ready_null_keys")
		h.d.Log.Error("ready clip with NULL media keys", "clip", c.ID, "route", kind.String())
		w.Header().Set("Cache-Control", "no-store")
		httpx.WriteError(w, http.StatusServiceUnavailable, "unavailable", "clip media is unavailable")
		return
	}
	// `?download=1` is the only thing a viewer may ask of a media route, and
	// it asks for PRESENTATION, not for different bytes (§8.1, §20 erratum
	// 98): the same object, through the same serveObject, with a
	// Content-Disposition that makes a browser save it instead of navigating
	// to it. Everything a cache or a player keys on is unchanged — ETag,
	// Last-Modified, Cache-Control, Accept-Ranges, and every 206/304/412/416
	// answer — so the query string, which is already part of the cache key,
	// carries the whole difference and no Vary is needed. Any other value is
	// IGNORED rather than refused: this parameter is reached from a link on
	// the share page, and a link must never be a way to make the route fail.
	if kind == mediaVideo && r.URL.Query().Get(downloadParam) == "1" {
		obj.filename = renditionFilename(c)
	}
	if kind == mediaAudio && r.URL.Query().Get(downloadParam) == "1" {
		obj.filename = audioFilename(c, obj.key)
	}
	// `/original` is a PUBLIC PLAYER URL when the upload is the clip, so it is
	// served inline there and saved only when asked — the presentation rule
	// `/video.mp4` has had since erratum 98, applied to the route that has just
	// become a second player URL (§20 erratum 207). A `<video src>` ignores
	// Content-Disposition, but a browser NAVIGATED to that URL does not, and the
	// owner's own "Download original" link therefore carries `?download=1`.
	// Nothing changes for a clip whose upload stays owner-only: obj already has
	// the attachment name, and this leaves it alone.
	if kind == mediaOriginal && obj.filename != "" && db.OriginalIsTheClip(c) &&
		r.URL.Query().Get(downloadParam) != "1" {
		obj.filename = ""
		obj.private = false
	}
	if h.d.Store == nil {
		writeStorageError(w, r, errors.New("storage not configured"))
		return
	}
	h.serveObject(w, r, c.ID, obj)
}

// downloadParam is the query parameter that turns a plain `video.mp4` GET
// into a save. Its one accepted value is "1".
const downloadParam = "download"

// maxSlugRunes caps a download filename's stem. titleSlug emits nothing but
// [a-z0-9-], so a rune there is a byte and the cap is exact.
const maxSlugRunes = 60

// renditionFilename is the name a `?download=1` save lands as: the clip's
// title slugged, plus the extension the rendition always has. A title with
// nothing sluggable in it falls back to the clip id, which is never empty.
func renditionFilename(c db.Clip) string { return titleSlug(c.Title, c.ID) + ".mp4" }

// titleSlug lowercases title and reduces it to [a-z0-9-]: every run of
// anything else collapses to ONE '-', and a literal '-' counts as part of
// such a run (so "a — b" cannot come out as "a---b"). A run at either end is
// dropped, which is why the maxSlugRunes cap needs no second trim — the
// builder never holds a trailing dash. fallback is returned when nothing
// survives: a title of pure emoji is a filename problem, not an error.
//
// The result is deliberately plain ASCII rather than the source title, so the
// Content-Disposition it feeds is a bare RFC 2045 token — one `filename=`
// parameter every client understands, no quoting and no RFC 5987 extended
// form to fall back from. (contentDisposition still adds that form when it is
// needed; here it never is.)
func titleSlug(title, fallback string) string {
	var b strings.Builder
	b.Grow(len(title))
	sep := false
	for _, r := range strings.ToLower(title) {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			sep = true
			continue
		}
		need := 1
		if sep && b.Len() > 0 {
			need = 2 // the separator run collapses into one '-' in front of it
		}
		if b.Len()+need > maxSlugRunes {
			break
		}
		if need == 2 {
			b.WriteByte('-')
		}
		sep = false
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return fallback
	}
	return b.String()
}

// mediaObject resolves the triple for kind from the row. The rendition,
// poster and social routes need their key column set (social falls back to
// the rendition, §8.5 step 4); the original's triple is NOT NULL from insert.
// ok=false means the row lacks the keys the route needs.
//
// Every DERIVED object dates from `updated_at`, not from `ready_at`: a trim
// (`SwapRendition`) and a GIF regenerate (`SetGIF`) replace bytes under these
// stable URLs and both stamp `updated_at`, while `ready_at` stays where the
// transcode put it. Dating them from `ready_at` let a cache that revalidates
// with `If-Modified-Since` ALONE — no `If-None-Match` — be answered 304 for
// bytes that HAD changed (§20 errata 70 and 76). The other writers of
// `updated_at` on a ready row (a PATCH of the metadata, a TTL touch, an admin
// expire) move the date without moving the bytes, which is the harmless
// direction: a date-only revalidation then costs a fresh 200 instead of a
// 304, and the strong ETag — the exact validator, unchanged — still settles
// it in one round trip. A view never stamps `updated_at`, so the busiest
// thing a clip does cannot churn the date at all.
//
// The original keeps `created_at`: its bytes are written once at ingest and
// nothing in the system ever rewrites them.
func mediaObject(c db.Clip, kind mediaKind) (object, bool) {
	derivedAt := c.UpdatedAt
	switch kind {
	case mediaAudio:
		o, _, ok := audioObject(c)
		return o, ok
	case mediaVideo:
		if c.IsAudio() {
			// An audio clip's `/video.mp4` is its CARD COPY (§20 erratum 328): the
			// one MP4 it has. Its best playable tier is `/audio`, under its real
			// media type, never an `.mp4` name over a FLAC. No card copy is the
			// ready-row invariant violation (ok=false, a 503), never a fall back
			// to bytes that are not an MP4.
			if c.SocialKey == nil || c.SocialSize == nil || c.SocialETag == nil {
				return object{}, false
			}
			return object{key: *c.SocialKey, size: *c.SocialSize, etag: *c.SocialETag,
				contentType: "video/mp4", modTime: time.Unix(derivedAt, 0), redirectable: true}, true
		}
		// `/video.mp4` is "the BEST PLAYABLE TIER of this clip", and since the
		// quality tiers that is not always a rendition object (§20 erratum 201):
		// a playable, faststart upload is streamed as it stands, and a clip
		// waiting for its Full plays the LIGHT copy the transcode made first.
		// db.PlayObject is the one definition of that order, shared with every
		// job that derives something from a clip.
		key, size, etag, ok := db.PlayObject(c)
		if !ok {
			return object{}, false
		}
		ct := "video/mp4"
		mod := derivedAt
		if key == c.SourceKey {
			// The upload itself. Its bytes are written once at ingest and nothing
			// rewrites them, so it dates from `created_at` like `/original` does —
			// and it carries its stored media type, which the playability table
			// has already established is video/mp4 for anything served here.
			ct, mod = c.SourceContentType, c.CreatedAt
		}
		return object{key: key, size: size, etag: etag,
			contentType: ct, modTime: time.Unix(mod, 0), redirectable: true}, true
	case mediaSocial:
		if c.SocialKey != nil && c.SocialSize != nil && c.SocialETag != nil {
			return object{key: *c.SocialKey, size: *c.SocialSize, etag: *c.SocialETag,
				contentType: "video/mp4", modTime: time.Unix(derivedAt, 0), redirectable: true}, true
		}
		return mediaObject(c, mediaVideo)
	case mediaPoster:
		if c.PosterKey == nil || c.PosterSize == nil || c.PosterETag == nil {
			return object{}, false
		}
		// An audio clip's poster is a PNG (§20 erratum 329); the key says which.
		return object{key: *c.PosterKey, size: *c.PosterSize, etag: *c.PosterETag,
			contentType: transcode.PosterContentType(*c.PosterKey), modTime: time.Unix(derivedAt, 0), redirectable: true}, true
	case mediaGIF:
		// ok=false here is NOT the invariant violation the other kinds mean
		// by it: a ready clip legitimately has no preview until somebody asks
		// for one. previewGIF handles that case itself and never routes it
		// through serveMedia (§17 #6).
		if c.GIFKey == nil || c.GIFSize == nil || c.GIFETag == nil {
			return object{}, false
		}
		return object{key: *c.GIFKey, size: *c.GIFSize, etag: *c.GIFETag,
			contentType: "image/gif", modTime: time.Unix(derivedAt, 0)}, true
	case mediaFilmstrip:
		// Same rule as the GIF's, for the same reason: a ready clip
		// legitimately has no filmstrip until somebody opens the trim control,
		// and `filmstrip` handles that case itself rather than routing it
		// through serveMedia (§20 erratum 220).
		if c.FilmstripKey == nil || c.FilmstripSize == nil || c.FilmstripETag == nil {
			return object{}, false
		}
		return object{key: *c.FilmstripKey, size: *c.FilmstripSize, etag: *c.FilmstripETag,
			contentType: "image/jpeg", modTime: time.Unix(derivedAt, 0)}, true
	default:
		name := attachmentName(c.SourceFilename, c.SourceKey)
		if c.IsAudio() {
			name = audioAttachmentName(c)
		}
		return object{key: c.SourceKey, size: c.SourceSize, etag: c.SourceETag,
			contentType: c.SourceContentType, modTime: time.Unix(c.CreatedAt, 0),
			filename: name, private: true}, true
	}
}

// maxRanges caps the range-specs one Range header may list (§20 erratum
// 23). http.ServeContent honours any count and the seeker opens one GetRange
// per non-adjacent range, so without a cap a kilobyte of header on the
// anonymous, un-rate-limited media routes bought thousands of sequential
// storage GETs and held a handler goroutine for their whole walk. RFC 9110
// §14.2 lets a server reject "many small ranges" outright; real clients send
// one range (a seek) or two (a moov-at-the-end probe), never eight.
const maxRanges = 8

// bytesUnit is the one range unit this server implements.
const bytesUnit = "bytes"

// normalizeRangeUnit settles the Range header's UNIT before anything reads
// its specs, and returns the value ServeContent will see (§20 erratum 44).
// RFC 9110 §14.2 makes ignoring a range unit the server does not understand
// a MUST — `Range: items=0-1` is answered with the full representation, not
// 416 — and §14.1 makes unit names case-insensitive. `http.ServeContent`'s
// parser tests for a literal `bytes=` prefix and rejects everything else as
// an invalid range, which `objectErrorWriter` then dresses up as this
// package's own considered 416; and this package already owns the range
// surface (errata 23, 38, 43). So: a `bytes` unit in any case is
// canonicalised so its ranges are honoured, an unrecognised unit is dropped
// so the request is served in full, and a value with no `=` at all is left
// alone — an invalid ranges-specifier a server MAY reject (§14.1.1), which
// is what ServeContent does with it today.
func normalizeRangeUnit(r *http.Request) string {
	v := r.Header.Get("Range")
	unit, specs, ok := strings.Cut(v, "=")
	switch {
	case v == "", !ok, unit == bytesUnit:
		return v
	case strings.EqualFold(unit, bytesUnit):
		v = bytesUnit + "=" + specs
		r.Header.Set("Range", v)
		return v
	default:
		r.Header.Del("Range")
		return ""
	}
}

// rangeCount counts the non-empty range-specs after the unit of a Range
// header value — what ServeContent would try to serve (it skips empty
// specs too; a header with no unit it rejects itself). A range-spec never
// contains a comma.
func rangeCount(rangeHeader string) int {
	_, specs, ok := strings.Cut(rangeHeader, "=")
	if !ok {
		return 0
	}
	n := 0
	for _, spec := range strings.Split(specs, ",") {
		if strings.TrimSpace(spec) != "" {
			n++
		}
	}
	return n
}

// zeroSuffixRange reports whether the header lists a suffix range of zero
// length — "bytes=-0" and the shapes net/http parses identically ("-00",
// "- 0", trimmed on both sides of the dash). RFC 9110 §14.1.2 makes such a
// spec unsatisfiable (it selects zero bytes), but http.ServeContent computes
// start = size, length = 0 and answers 206 with an INVALID Content-Range —
// "bytes {size}-{size-1}/{size}", last-byte-pos below first-byte-pos — under
// a cacheable Cache-Control. §8.5's rule is "unsatisfiable → 416 with
// Content-Range: bytes */size, zero GETs", so the gate is ours to apply
// (§20 erratum 38).
func zeroSuffixRange(rangeHeader string) bool {
	_, specs, ok := strings.Cut(rangeHeader, "=")
	if !ok {
		return false
	}
	for _, spec := range strings.Split(specs, ",") {
		start, end, ok := strings.Cut(strings.TrimSpace(spec), "-")
		if !ok || strings.TrimSpace(start) != "" {
			continue // not a suffix range (or malformed: ServeContent 416s it)
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(end), 10, 64); err == nil && n == 0 {
			return true
		}
	}
	return false
}

// rangeBinding reports whether http.ServeContent will actually apply the
// Range header, i.e. whether the RFC 9110 §13.2.2 preconditions leave it
// standing. Both gates above pre-empt ServeContent, so they must stand aside
// exactly where the stdlib would not honour the Range at all: an If-Range
// whose validator does not match makes the server serve the FULL
// representation (§13.1.5), and a matching If-None-Match or a failing
// If-Match resolves to 304 / 412 before Range is looked at. Standing aside
// is free — those three answers cost one GET, one and zero respectively,
// never the per-range walk the gates exist to prevent — while a 416 there
// would be a hard error where the client is owed the representation. The
// judgement errs toward binding: anything unparsable or unknown leaves the
// gates in force (§20 erratum 43).
func rangeBinding(r *http.Request, etag string, modTime time.Time) bool {
	if im := r.Header.Get("If-Match"); im != "" {
		if !etagListMatches(im, etag, true) {
			return false // 412
		}
	} else if modified, ok := modifiedSince(r.Header.Get("If-Unmodified-Since"), modTime); ok && modified {
		return false // 412
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		if etagListMatches(inm, etag, false) {
			return false // 304 (GET/HEAD; the media routes are nothing else)
		}
	} else if modified, ok := modifiedSince(r.Header.Get("If-Modified-Since"), modTime); ok && !modified {
		return false // 304
	}
	if ir := r.Header.Get("If-Range"); ir != "" {
		if tag, _ := scanETag(ir); tag != "" {
			return tag == etag // strong comparison, the only one If-Range allows
		}
		if noModTime(modTime) {
			return false
		}
		t, err := http.ParseTime(ir)
		return err == nil && t.Unix() == modTime.Unix()
	}
	return true
}

// modifiedSince reports whether modTime is strictly after the HTTP-date v
// (compared at second resolution, as the wire format is), and whether the
// pair was usable at all — an absent, unparsable or unknown date decides
// nothing, exactly like net/http's own check.
func modifiedSince(v string, modTime time.Time) (modified, ok bool) {
	if v == "" || noModTime(modTime) {
		return false, false
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return false, false
	}
	return modTime.Truncate(time.Second).After(t), true
}

// noModTime mirrors net/http's "no modification time" test: the zero Time
// and the Unix epoch both mean unknown there.
func noModTime(t time.Time) bool { return t.IsZero() || t.Unix() == 0 }

// scanETag pulls the leading entity-tag off s (RFC 9110 §8.8.3:
// `[ "W/" ] DQUOTE *etagc DQUOTE`) and returns it with the rest of the list.
// "" means s does not begin with a well-formed one.
func scanETag(s string) (tag, rest string) {
	s = strings.TrimLeft(s, " \t")
	start := 0
	if strings.HasPrefix(s, "W/") {
		start = 2
	}
	if start >= len(s) || s[start] != '"' {
		return "", ""
	}
	for i := start + 1; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			return s[:i+1], s[i+1:]
		case c == 0x21, c >= 0x23 && c <= 0x7e, c >= 0x80: // etagc / obs-text
		default:
			return "", ""
		}
	}
	return "", ""
}

// etagListMatches reports whether an If-Match / If-None-Match list matches
// tag: `*` matches any representation, a strong comparison is equality, a
// weak one ignores both `W/` prefixes (RFC 9110 §8.8.3.2). A malformed entry
// ends the walk, as it does in net/http.
func etagListMatches(list, tag string, strong bool) bool {
	for {
		list = strings.TrimLeft(list, " \t")
		switch {
		case list == "":
			return false
		case list[0] == ',':
			list = list[1:]
			continue
		case list[0] == '*':
			return true
		}
		got, rest := scanETag(list)
		if got == "" {
			return false
		}
		if (strong && got == tag) || (!strong && strings.TrimPrefix(got, "W/") == strings.TrimPrefix(tag, "W/")) {
			return true
		}
		list = rest
	}
}

// serveObject sets every header before http.ServeContent (so it neither
// sniffs nor stats) and streams through the lazy seeker: Seek is offline,
// 304/HEAD/416 make zero storage calls, a body needs one GetRange per
// non-adjacent range, of which a request may list at most maxRanges.
func (h *handler) serveObject(w http.ResponseWriter, r *http.Request, clipID string, o object) {
	// MEDIA_DELIVERY=redirect answers here, and only here: every caller of
	// serveObject has already run every gate its route has (§20 erratum 278).
	if o.redirectable && h.d.Cfg.RedirectsMedia() {
		h.redirectToObject(w, r, o)
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", o.contentType)
	hdr.Set("ETag", `"`+o.etag+`"`)
	hdr.Set("Accept-Ranges", "bytes")
	hdr.Set("X-Content-Type-Options", "nosniff")
	if o.private || hdr.Get("Set-Cookie") != "" {
		// A CREDENTIAL-BEARING response is never publicly cacheable, whatever the
		// object is (§20 erratum 207, amended). The one route this second term is
		// about is `/original` on a playable upload: it is a PUBLIC player URL that
		// still runs the credential resolver, whose once-a-minute sliding touch
		// re-issues the session cookie on whatever response it lands on
		// (auth.Resolver.Middleware). `public` is RFC 9111 §3.5's opt-in for a
		// shared cache to hand one caller's response to the next, and a stored
		// Set-Cookie is one owner's session handed to a stranger. Most shared
		// caches special-case Set-Cookie; nothing in the RFC makes them, and this
		// codebase's own rule — a response a cache must not reuse is `private` with
		// the credential Vary — is not a thing to leave to a CDN's defaults. The
		// anonymous request, which is the one a CDN is there for, carries no cookie
		// and keeps `public, max-age=3600`.
		hdr.Set("Cache-Control", privateCacheControl)
		hdr.Set("Vary", credentialVary)
	} else {
		hdr.Set("Cache-Control", mediaCacheControl)
		// A PUBLIC object varies on nothing: the same bytes for every caller.
		// The `/original` route is wrapped in privateNoStore, which sets the
		// credential Vary for the owner-only case — and since erratum 207 that
		// route also serves a public playable upload, where the Vary would stop
		// a shared cache reusing one anonymous response for the next anonymous
		// caller. Every other media route sets none, so this deletes nothing.
		hdr.Del("Vary")
	}
	if o.filename != "" {
		hdr.Set("Content-Disposition", contentDisposition(o.filename))
	}
	rangeHeader := normalizeRangeUnit(r)
	if rangeHeader != "" && rangeBinding(r, hdr.Get("ETag"), o.modTime) {
		if n := rangeCount(rangeHeader); n > maxRanges {
			httpx.SetLogField(r, "reject", "too_many_ranges")
			httpx.SetLogField(r, "ranges", n)
			writeRangeError(w, o.size, "at most "+strconv.Itoa(maxRanges)+" ranges per request")
			return
		}
		if zeroSuffixRange(rangeHeader) {
			httpx.SetLogField(r, "reject", "zero_suffix_range")
			writeRangeError(w, o.size, "a suffix range of zero length is not satisfiable")
			return
		}
	}
	rs := &readRecorder{ReadSeekCloser: store.NewSeeker(r.Context(), h.d.Store, o.key, o.size)}
	// Closed before the error is read, and again on the panic path: a
	// multi-range response is written by a goroutine http.ServeContent starts
	// and never joins, which is still reading when an aborted copy returns
	// here (§20 erratum 41). Close stops it from opening another range.
	defer rs.Close()
	// The served-bytes counter's one measuring point (ROADMAP #39, §20 erratum
	// 270). It wraps the writer INSIDE objectErrorWriter, so a 416 or a 412
	// envelope — an error, not the object — is written through it and is not
	// counted; and it counts what was WRITTEN, so a viewer who aborts a ranged
	// read is charged what they received.
	sw := &servedWriter{ResponseWriter: w}
	http.ServeContent(&objectErrorWriter{ResponseWriter: sw, size: o.size}, r, "", o.modTime, rs)
	h.d.Served.Add(sw.n)
	_ = rs.Close()
	h.noteReadError(r, clipID, o.key, rs.readError())
}

// inlineDisposition is what a presigned media URL asks the bucket to answer
// with when the request is not a download: the object is a thing to PLAY, not
// a thing to save, and an object stored with a disposition of its own (or
// none) must not be able to change that.
const inlineDisposition = "inline"

// redirectToObject is MEDIA_DELIVERY=redirect's whole answer: 302 to a URL
// minted for THIS request against the bucket's own origin (§20 erratum 278).
//
// What it deliberately does NOT do:
//
//   - It writes no representation headers. ETag, Last-Modified, Accept-Ranges
//     and the object's Content-Type all describe bytes this response does not
//     carry, and a cache that stored them against this URL would be caching a
//     redirect as if it were the object (erratum 85's rule, applied to a third
//     status). `no-store` for the same reason and one more: the Location holds
//     a signature with an expiry, so a stored 302 is a link that stops working
//     while a shared cache goes on handing it out.
//   - It does not look at Range, If-None-Match or If-Range. A 302 has no
//     representation to range over or revalidate; the TARGET answers all of
//     that, and the live Discord run behind this design measured the media
//     proxy forwarding `Range: bytes=0-1023` through the hop and getting its
//     206 back from the target.
//   - It makes no distinction between GET and HEAD. Both get the same 302 with
//     no body. (Discord never sent a HEAD in the measurement; answering one is
//     insurance, not a requirement.)
//
// The served-bytes counter (ROADMAP #39, §20 erratum 270) is charged the
// object's full size here. That is an UPPER bound — a viewer who watches four
// seconds pays for the file, and Discord's double unfurl pays twice — and the
// over-count is the safe direction for a cost line. It is never silently
// wrong: Settings' Storage panel prints `estimated (redirect)` beside the
// figure and DEPLOY §31.3 says why, because erratum 270's law is that a number
// nobody measured is ABSENT rather than a zero somebody could read as
// "nothing has been served".
func (h *handler) redirectToObject(w http.ResponseWriter, r *http.Request, o object) {
	disposition := inlineDisposition
	if o.filename != "" {
		disposition = contentDisposition(o.filename)
	}
	loc, err := h.d.Store.PresignGet(r.Context(), o.key, h.d.Cfg.MediaPresignTTL, o.contentType, disposition)
	if err != nil {
		h.d.Log.Error("media presign failed", "key", o.key, "err", err)
		writeStorageError(w, r, err)
		return
	}
	// `mediaHeaders` (router.go) has already set X-Content-Type-Options and the
	// referrer policy on every media response, this one included. Setting them
	// again here would be a second place to keep in step.
	hdr := w.Header()
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Location", loc)
	httpx.SetLogField(r, "media", "redirect")
	// No body. net/http's own http.Redirect writes an HTML page for a GET,
	// which would put the signed URL into a document a browser can render and
	// a proxy can log as content; a bare 302 with Content-Length: 0 is what
	// the Discord measurement saw and what every client here wants.
	w.WriteHeader(http.StatusFound)
	// A HEAD is charged NOTHING, exactly as in `proxy` mode, where it writes no
	// body and servedWriter counts none. The first reading of this — "whatever
	// follows it fetches the object anyway" — is true of a browser and false of
	// every request that is ONLY a HEAD: an uptime monitor, `curl -I`, a link
	// checker, this project's own probes. Charging those would put bytes nobody
	// received into a cost line, which is the one direction an estimate that
	// already over-counts must not err in further (§20 erratum 278(i)).
	if r.Method != http.MethodHead {
		h.d.Served.Add(o.size)
	}
}

// writeRangeError is the 416 of the media routes (§8 conventions): the JSON
// envelope with Content-Range: bytes */size and no-store. It costs zero
// storage calls wherever it is written from.
//
// It also UNDOES the representation headers serveObject set before the range
// gates ran, because this body is an error and not the object (§20 erratum
// 85). Content-Disposition is the one that matters: /original is served
// `attachment; filename="clip.mp4"`, so a download client that hit a 416
// would otherwise save the JSON error envelope to disk under the clip's own
// filename and the user would find a broken video. ETag and Last-Modified go
// with it for the same reason — they are validators for the OBJECT, and a
// cache must never be able to revalidate the object against an error body.
// One call covers both entry points: the gates above and the ServeContent
// 416 that objectErrorWriter intercepts; writePreconditionFailed is its 412
// twin (erratum 103).
func writeRangeError(w http.ResponseWriter, size int64, msg string) {
	dropObjectHeaders(w.Header())
	w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
	httpx.WriteError(w, http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable", msg)
}

// writePreconditionFailed is the 412 of the media routes, and it is erratum
// 85 applied to the OTHER status net/http can write out of serveObject
// (§20 erratum 103). `If-Match: "nope"` or a failing
// `If-Unmodified-Since` used to answer 412 with the whole representation
// header set serveObject had already written -- the object ETag, its
// Content-Type, `Cache-Control: public, max-age=3600` and, on /original, its
// `Content-Disposition: attachment` -- because objectErrorWriter intercepted
// 416 only. Every word of erratum 85 applies here: the body is an ERROR and
// not the object, a download client must not save it under the clip filename,
// and "a cache must never be able to revalidate the object against an error
// body" -- which the explicit max-age made worse, since an empty 412 is
// storable. No Content-Range: 412 is a precondition failure, not a range one.
func writePreconditionFailed(w http.ResponseWriter) {
	dropObjectHeaders(w.Header())
	httpx.WriteError(w, http.StatusPreconditionFailed, "precondition_failed",
		"the If-Match or If-Unmodified-Since precondition failed")
}

// dropObjectHeaders undoes the representation headers serveObject set before
// the precondition and range gates ran, and makes the response
// unstoreable. It is the shared half of the two errors above.
func dropObjectHeaders(h http.Header) {
	h.Del("Content-Disposition")
	h.Del("ETag")
	h.Del("Last-Modified")
	h.Set("Cache-Control", "no-store")
}

// objectErrorWriter intercepts the TWO statuses net/http can write out of
// serveObject on its own and re-answers each in this package's envelope: the
// text/plain 416 ServeContent writes for a malformed or unsatisfiable Range
// becomes writeRangeError's, and the bodyless 412 checkPreconditions writes
// becomes writePreconditionFailed's (§20 erratum 103). Both replacements exist
// to strip the representation headers serveObject had already set, which is
// why the name is the OBJECT's and not the range's. Every other status passes
// through untouched. ServeContent settles both before its first Read, so
// neither costs a storage call.
type objectErrorWriter struct {
	http.ResponseWriter
	size        int64
	intercepted bool
}

func (w *objectErrorWriter) WriteHeader(code int) {
	switch code {
	case http.StatusRequestedRangeNotSatisfiable:
		w.intercepted = true
		writeRangeError(w.ResponseWriter, w.size, "requested range is not satisfiable")
	case http.StatusPreconditionFailed:
		// net/http's checkPreconditions writes this one with no body and no
		// header cleanup of its own, so the object headers would escape with
		// it (§20 erratum 103).
		w.intercepted = true
		writePreconditionFailed(w.ResponseWriter)
	default:
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *objectErrorWriter) Write(p []byte) (int, error) {
	if w.intercepted {
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}

func (w *objectErrorWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// servedWriter counts the body bytes a media response actually wrote, for the
// per-day served-bytes counter (ROADMAP #39, §20 erratum 270). It is the ONLY
// place in the app that counts anything a viewer received.
//
// What it counts and what it does not:
//
//   - Only a 200 or a 206 body. An error envelope this package writes through
//     the same writer (the 416 and 412 objectErrorWriter re-answers) is not the
//     object and is not serving; a 304 and a HEAD write no body at all.
//   - What was WRITTEN, not what was asked for. A viewer who seeks away or
//     closes the tab mid-stream is counted for the bytes that reached the
//     socket, which is what a provider meters and what the cost line is for.
//   - Nothing about the viewer. There is no clip id, no address and no user
//     agent here or anywhere downstream: one integer per UTC day is the whole
//     of what is kept (migration 0017).
//
// No mutex, deliberately. Every Write to a ResponseWriter happens on the
// handler's own goroutine — including a multi-range response, where the
// goroutine `http.ServeContent` starts writes into a PIPE and the handler
// copies that pipe to the response. (The un-joined goroutine is why
// readRecorder below needs a lock; it reads the seeker, and it never touches
// this.)
type servedWriter struct {
	http.ResponseWriter
	status int
	n      int64
}

func (w *servedWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *servedWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK // an implicit 200, as net/http does it
	}
	n, err := w.ResponseWriter.Write(p)
	if n > 0 && (w.status == http.StatusOK || w.status == http.StatusPartialContent) {
		w.n += int64(n)
	}
	return n, err
}

// Unwrap keeps the ResponseController chain (and every wrapper's own Unwrap)
// intact through this layer.
func (w *servedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// readRecorder captures the first read failure so the handler can classify
// it after ServeContent returns (the status is already committed by then).
// The reads may come from ServeContent's un-joined multi-range goroutine
// while the handler is already closing and classifying, so the record is
// taken under a mutex and closed to late writers: an error a read hits after
// Close belongs to the teardown, not to the response (§20 erratum 41).
type readRecorder struct {
	io.ReadSeekCloser
	mu     sync.Mutex
	err    error
	closed bool
}

func (rr *readRecorder) Read(p []byte) (int, error) {
	n, err := rr.ReadSeekCloser.Read(p)
	if err != nil && err != io.EOF {
		rr.mu.Lock()
		if rr.err == nil && !rr.closed {
			rr.err = err
		}
		rr.mu.Unlock()
	}
	return n, err
}

// readError is the recorded failure, safe to call once the response is done.
func (rr *readRecorder) readError() error {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	return rr.err
}

// Close stops recording and closes the seeker (idempotent).
func (rr *readRecorder) Close() error {
	rr.mu.Lock()
	rr.closed = true
	rr.mu.Unlock()
	return rr.ReadSeekCloser.Close()
}

// noteReadError applies the §8.5 log doctrine: a client disconnect is INFO
// (an access-log field only); a key a LIVE row still references but the
// bucket lacks is row/object drift, the one ERROR on this path; anything
// else — including a transfer that ended short, which the seeker reports as
// io.ErrUnexpectedEOF whether the object is short or the link dropped — is
// WARN. `admin orphans` confirms drift; a flaky link must not page anyone.
//
// The clip snapshot predates the GetRange (multi-range responses stream for
// minutes), so a NoSuchKey may simply be a lost race with owner DELETE or
// sweeper expiry — §12 classifies that as INFO. The row is re-read on a
// short detached ctx to tell the two apart before the one ERROR is emitted.
func (h *handler) noteReadError(r *http.Request, clipID, key string, err error) {
	switch {
	case err == nil:
	case r.Context().Err() != nil || errors.Is(err, context.Canceled):
		httpx.SetLogField(r, "client_disconnected", true)
	case errors.Is(err, store.ErrNotFound):
		switch drift, verr := h.confirmDrift(clipID, key); {
		case verr != nil:
			httpx.SetLogField(r, "storage_read_error", "object_missing")
			h.d.Log.Warn("media object missing; row re-read failed", "clip", clipID, "key", key, "err", err, "recheck_err", verr)
		case drift:
			httpx.SetLogField(r, "storage_read_error", "object_missing")
			h.d.Log.Error("media object missing: row/object drift", "clip", clipID, "key", key, "err", err)
		default:
			httpx.SetLogField(r, "lost_race", "clip_deleted")
			h.d.Log.Info("media stream lost race with clip deletion", "clip", clipID, "key", key)
		}
	case errors.Is(err, io.ErrUnexpectedEOF):
		httpx.SetLogField(r, "storage_read_error", "short_object_or_truncated")
		h.d.Log.Warn("media object short or transfer truncated", "clip", clipID, "key", key, "err", err)
	default:
		httpx.SetLogField(r, "storage_read_error", err.Error())
		h.d.Log.Warn("media storage read error", "clip", clipID, "key", key, "err", err)
	}
}

// driftCheckTimeout bounds noteReadError's post-hoc row re-read; the
// request's own ctx may already be spent, so the read is detached.
const driftCheckTimeout = 5 * time.Second

// confirmDrift re-reads the clip row: drift is confirmed only when a live
// (non-deleting) row still references the missing key. A row that is gone,
// tombstoned, or re-keyed (a retry mints fresh per-attempt keys) means the
// object was removed legitimately — a lost race, not drift.
func (h *handler) confirmDrift(clipID, key string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), driftCheckTimeout)
	defer cancel()
	c, err := h.d.DB.GetClip(ctx, clipID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	case c.State == db.ClipDeleting:
		return false, nil
	}
	if c.SourceKey == key {
		return true, nil
	}
	for _, k := range []*string{c.RenditionKey, c.PosterKey, c.SocialKey, c.GIFKey, c.FilmstripKey} {
		if k != nil && *k == key {
			return true, nil
		}
	}
	return false, nil
}

// attachmentName picks the download filename for an original: the stored
// filename stripped of path and unsafe characters, else source.{ext}.
func attachmentName(filename, key string) string {
	name := stripPath(filename)
	name = strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		name = sourceFilePrefix + store.DefaultExt
		if i := strings.LastIndex(key, "/"); i >= 0 {
			name = key[i+1:]
		}
	}
	return name
}

// contentDisposition renders the §8.1 `attachment; filename="<sanitized>"`.
//
// A name carrying anything outside printable ASCII gets BOTH parameters, as
// RFC 6266 §4.3 recommends: an ASCII fallback under `filename=` and the exact
// name under RFC 5987's `filename*=`. Handed the name alone,
// mime.FormatMediaType emits the extended form ONLY, and a client that does
// not implement it — `curl -OJ`, `wget --content-disposition`, download
// managers — is then left with no name at all and saves the URL's last
// segment: a file called `original`, without even the extension (§20
// erratum 50). ASCII names keep exactly the header they had.
func contentDisposition(name string) string {
	params := map[string]string{"filename": name}
	if ascii, ok := asciiFilename(name); !ok {
		// mime.FormatMediaType renders both: the extended value is all RFC
		// 2045 token characters, so it is emitted unquoted, and `filename`
		// sorts before `filename*`.
		params["filename"], params["filename*"] = ascii, "utf-8''"+encodeRFC5987(name)
	}
	if cd := mime.FormatMediaType("attachment", params); cd != "" {
		return cd
	}
	return `attachment; filename="` + sourceFilePrefix + store.DefaultExt + `"`
}

// asciiFilename replaces every rune a plain `filename=` cannot carry with
// '_', which keeps an ASCII extension where the name has one, and reports
// whether the name was already pure printable ASCII (nothing replaced —
// attachmentName has already dropped the C0 controls, DEL, quote and
// backslash, so only the high runes are left to map).
func asciiFilename(name string) (string, bool) {
	ok := true
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			ok = false
			return '_'
		}
		return r
	}, name), ok
}

// encodeRFC5987 percent-encodes the UTF-8 bytes of s down to RFC 5987's
// attr-char set (which deliberately excludes '%', '\” and '*'), the value
// half of `filename*=utf-8”…`.
func encodeRFC5987(s string) string {
	const safe = "!#$&+-.^_`|~"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.IndexByte(safe, c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
