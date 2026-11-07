package httpapi

import (
	"errors"
	"net/http"

	"clippet/internal/auth"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/urls"
	"clippet/internal/worker"
)

// filmstripDTO renders a clip's sheet for the wire, and reports false when
// there is none to render. All five columns are written by one UPDATE and read
// back together, so a row carrying some of them and not the others cannot come
// from this server — and rather than half-answer such a row this says "no
// sheet", which sends the client to the route that makes a whole one.
func filmstripDTO(c db.Clip, u urls.Builder) (FilmstripDTO, bool) {
	if c.FilmstripKey == nil || c.FilmstripTiles == nil || c.FilmstripTileW == nil ||
		c.FilmstripTileH == nil || c.FilmstripIntervalMS == nil {
		return FilmstripDTO{}, false
	}
	if *c.FilmstripTiles <= 0 || *c.FilmstripTileW <= 0 || *c.FilmstripTileH <= 0 || *c.FilmstripIntervalMS <= 0 {
		return FilmstripDTO{}, false
	}
	return FilmstripDTO{URL: u.Filmstrip(c.ID), Tiles: *c.FilmstripTiles,
		TileWidth: *c.FilmstripTileW, TileHeight: *c.FilmstripTileH, IntervalMS: *c.FilmstripIntervalMS}, true
}

// The trim scrubber's filmstrip (§8.1, §20 errata 220 and 227): ONE route over
// one job, with TWO arms that answer to different rules.
//
//	GET /api/clips/{id}/filmstrip.jpg → 200 image/jpeg (anybody)
//	                                  → 202 | 409 | 503 (the owner's session)
//	                                  → 404 (everybody else, and nothing queued)
//
// SERVING a sheet that already exists is PUBLIC exactly like `/poster.jpg`: the
// link is the auth, two dozen 160-pixel thumbnails of a clip anyone holding the
// link may already watch frame by frame leak nothing, and the URL travels inside
// pages that get cached and shared. That arm resolves NO credential at all,
// which is what keeps `serveObject`'s `public, max-age=3600` free of a session
// cookie (erratum 227 explains why the two facts are one decision).
//
// MAKING one is the owner's, from a browser session, and nobody else's. The
// pass decodes the whole current rendition and holds one of the
// TRANSCODE_CONCURRENCY ffmpeg slots while it does, and the only consumer of the
// finished sheet is the owner's own trim control — so a stranger's request is
// answered 404 and buys nothing. The guards that bound the OWNER's own asking
// are unchanged: db.EnqueueFilmstrip admits at most one live filmstrip job per
// clip, so the hundredth request finds the first one's job rather than making a
// hundredth; the job is bounded by construction (24 tiles of 160 px, one pass,
// 4 MiB, a 180 s kill); and the `view` limiter still counts every call.
//
// There is no owner POST beside it, and that is the one place this route is
// SMALLER than the GIF's pair. A GIF is an artefact a person asks for, keeps and
// downloads, so "make it again" is a decision somebody takes; a filmstrip is a
// tool the trim control needs whenever it is on screen, and it is invalidated by
// exactly the events that invalidate the rendition — a trim and a replacement
// both NULL the columns and delete the object. So the GET's own 202-and-enqueue
// covers every case a POST could: after a cut the next time the owner opens the
// control it asks for a sheet, does not find one, and one is made.
//
// The 202 is the whole contract: "there is no sheet yet, one is being made, come
// back". It is never a 404 for the owner (the clip exists), never a 500 (it is
// the normal first answer), and it carries Retry-After so a polite client paces
// itself.

// filmstripRetryAfter is the 202's Retry-After, in seconds. Five is about the
// time one bounded sampling pass takes end to end on a small machine — the same
// number the GIF's 202 gives, and the same number app.js waits.
const filmstripRetryAfter = "5"

// FilmstripStatusDTO is the 202 body: the job's state and nothing else. Every
// other fact about the clip is unchanged and already in hand.
type FilmstripStatusDTO struct {
	State string `json:"state"`
}

// filmstrip is the serving arm, and the gate in front of the other one (§8.5
// rules otherwise: the same 404 for an unknown or deleting clip, the same
// ETag/Range/cache handling through serveObject).
func (h *handler) filmstrip(w http.ResponseWriter, r *http.Request) {
	c, ok := h.lookupClip(w, r)
	if !ok {
		return
	}
	if refuseAudio(w, c) {
		return
	}
	if obj, ok := mediaObject(c, mediaFilmstrip); ok {
		if h.d.Store == nil {
			writeStorageError(w, r, errors.New("storage not configured"))
			return
		}
		h.serveObject(w, r, c.ID, obj)
		return
	}
	// There is no sheet, so this request may be asking for WORK — and only here
	// does the route look at who is asking (§20 erratum 227). The credential
	// resolution is composed HERE, per request, rather than mounted on the route
	// beside the `view` limiter, and that placement is the whole point: a
	// request the arm above answered has already returned, carrying
	// serveObject's shared-cacheable directive and no Set-Cookie of its own.
	h.filmstripMakeArm(c).ServeHTTP(w, r)
}

// filmstripMakeArm composes the no-sheet arm: the Resolver, the 503 gate that
// follows it on every credentialed chain (authGate, §14), and privateNoStore —
// because every answer below depends on WHO asked, so none of them may be stored
// by a shared cache or handed to the next caller of this URL. Both headers
// privateNoStore sets are left standing by the writers under it; a sheet that
// lands is served by the other arm, at the other cache directive.
func (h *handler) filmstripMakeArm(c db.Clip) http.Handler {
	return h.auth.Middleware(authGate(privateNoStore(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) { h.makeFilmstrip(w, r, c) }))))
}

// filmstripMaker reports whether this request may spend an ffmpeg slot on a
// sheet: the clip's OWNER, through a browser session, and nothing else.
//
// A Bearer token of either scope is refused, which is erratum 19's rule applied
// where it was written for: a token is an upload credential, and errata 111/112
// widened it by exactly two READS of rows the owner already has (GET /api/clips,
// GET /api/collections). Enqueueing is not a read — it is the one verb on this
// route that costs the operator something — so a `read` token, which cannot
// upload a byte, must not be able to spend a transcode slot per clip in a
// library it can list. Nothing needs it: the only consumer of a sheet is the
// owner's own trim control, in a browser, with a session.
//
// No CSRF check goes with this, and that is not an omission. The session cookie
// is SameSite=Lax (§4), so a cross-site subresource — the `<img src>` an
// attacker's page would use — carries no credential at all and lands on the 404
// below; what Lax does let through is a top-level GET navigation, and the most
// that can buy is the one sheet the owner's own control would have made on their
// next visit, once per rendition, under the `view` limiter.
func filmstripMaker(r *http.Request, c db.Clip) bool {
	p, ok := auth.FromContext(r.Context())
	return ok && p.Via == auth.ViaSession && p.User.ID == c.OwnerID
}

// makeFilmstrip answers the caller who found no sheet.
func (h *handler) makeFilmstrip(w http.ResponseWriter, r *http.Request, c db.Clip) {
	if !filmstripMaker(r, c) {
		// 404, and the SAME 404 for every one of them — an anonymous visitor,
		// another account's session, a token of either scope. It is the truth
		// this URL has to tell (there is no sheet at it), it is what the caller
		// would get for a clip that legitimately has none, and a distinct
		// refusal would turn the route into an oracle for "is this id mine".
		// Nothing is queued and no job row is even read.
		//
		// The SENTENCE differs from the unknown-clip 404's "no such clip", and
		// that is not a leak to plug: whether a clip exists is already public on
		// GET /api/clips/{id}, which answers 200 for any id anybody holds. What
		// this route must not say is whose clip it is.
		httpx.WriteError(w, http.StatusNotFound, "not_found", "no filmstrip for this clip")
		return
	}
	if c.State != db.ClipReady {
		// Nothing to show and nothing to make one from. Same code and same
		// shape as the other media routes' not-ready answer.
		httpx.WriteError(w, http.StatusConflict, "not_ready", "the clip is still "+c.State)
		return
	}
	// A filmstrip job that has already FAILED is reported, not retried: the
	// route would otherwise re-run a doomed pass every time the control is
	// opened. A trim or a replacement is what gives the clip a fresh rendition
	// and therefore a fresh reason to try (both NULL the columns), so nothing is
	// stuck for ever on one bad file.
	job, err := h.d.DB.LatestJobOfKind(r.Context(), c.ID, db.KindFilmstrip)
	switch {
	case errors.Is(err, db.ErrNotFound): // never asked for: fall through and enqueue
	case err != nil:
		writeDBError(w, r, err)
		return
	case job.State == db.JobFailed:
		httpx.WriteError(w, http.StatusServiceUnavailable, "filmstrip_failed", filmstripFailureMessage(job))
		return
	case job.State == db.JobQueued, job.State == db.JobRunning:
		writeFilmstripAccepted(w, job.State)
		return
	}
	job, err = h.d.DB.EnqueueFilmstrip(r.Context(), c.ID, h.unixNow())
	switch {
	case errors.Is(err, db.ErrConflict):
		// The insert lost to a job that already exists — so READ that job and
		// report ITS state, exactly as the GIF route does (§20 erratum 87): a
		// flat "running" would claim work had started when the winner was still
		// queued, which is the common case when two callers ask in the same
		// second. A job that finished in between is gone from the queue, and
		// "running" is the honest fallback there — the caller is about to poll
		// and will get the sheet.
		state := db.JobRunning
		if live, jerr := h.d.DB.LatestJobOfKind(r.Context(), c.ID, db.KindFilmstrip); jerr == nil &&
			(live.State == db.JobQueued || live.State == db.JobRunning) {
			state = live.State
		}
		writeFilmstripAccepted(w, state)
		return
	case errors.Is(err, db.ErrStateMismatch):
		httpx.WriteError(w, http.StatusConflict, "not_ready", "only a ready clip has a filmstrip")
		return
	case errors.Is(err, db.ErrNotFound):
		writeNotFound(w) // destroyed between the lookup and the insert
		return
	case err != nil:
		writeDBError(w, r, err)
		return
	}
	h.d.Worker.Wake()
	h.d.Log.Info("filmstrip queued", "clip", c.ID, "job", job.ID)
	writeFilmstripAccepted(w, job.State)
}

// writeFilmstripAccepted is the 202 the route answers with. Its Cache-Control
// comes from privateNoStore above it — `private, no-store`, which is stronger
// than the bare `no-store` this used to set for itself, and it arrives with the
// `Vary` that says the answer depended on a credential (§20 erratum 227).
func writeFilmstripAccepted(w http.ResponseWriter, state string) {
	w.Header().Set("Retry-After", filmstripRetryAfter)
	httpx.WriteJSON(w, http.StatusAccepted, FilmstripStatusDTO{State: state})
}

// filmstripFailureMessage renders the 503's text. Since erratum 227 the only
// reader of it is the clip's owner, but the filter stays exactly as it was: the
// detail appended has to be one the WORKER wrote, because those carry
// publicReason's redaction and a shape a reader can do something with.
// worker.OwnerError drops the texts that reach this column from anywhere else —
// the two sentences internal/db writes when it repairs a job nobody is running,
// which are an operator's words and not an owner's (§20 erratum 165). A job with
// nothing left to say still gets a sentence, never an empty message.
func filmstripFailureMessage(job db.Job) string {
	if job.LastError == nil {
		return "the filmstrip could not be made"
	}
	if detail := worker.OwnerError(db.KindFilmstrip, *job.LastError); detail != "" {
		return "the filmstrip could not be made: " + detail
	}
	return "the filmstrip could not be made"
}
