package httpapi

import (
	"errors"
	"net/http"

	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/worker"
)

// GIF export (§8.1 Phase 4 rows, §17 #6). Two routes over one job:
//
//	GET  /api/clips/{id}/preview.gif  → 200 image/gif | 202 | 409 | 503 | 404
//	POST /api/clips/{id}/gif          → 202 | 409 | 401 | 403 | 404
//
// The GET is PUBLIC, exactly like /poster.jpg: the link is the auth, and a
// preview of a clip anyone with the link may already watch frame by frame
// leaks nothing. What makes that safe to combine with "and it may start
// work" is the pair of guards under it — db.EnqueueGIF admits at most one
// live gif job per clip, so the hundredth caller finds the first one's job
// rather than making a hundredth, and the job itself is bounded by
// construction (six seconds, 12 fps, 480 px, 8 MiB, a 60 s kill). The route
// also carries the `view` limiter, so a stranger cannot spend an operator's
// queue by walking clip ids.
//
// The 202 is the whole contract: "there is no preview yet, one is being
// made, come back". It is never a 404 (the clip exists), never a 500 (it is
// the normal first answer), and it carries Retry-After so a polite client
// paces itself.

// gifRetryAfter is the 202's Retry-After, in seconds. Five is about the time
// a bounded preview takes end to end on a small machine.
const gifRetryAfter = "5"

// GIFStatusDTO is the 202 body of both routes: the job's state and nothing
// else. Every other fact about the clip is unchanged and already in hand.
type GIFStatusDTO struct {
	State string `json:"state"`
}

// previewGIF serves the preview, or starts making one (§8.5 rules otherwise:
// the same 404 for an unknown or deleting clip, the same ETag/Range/cache
// handling through serveObject).
func (h *handler) previewGIF(w http.ResponseWriter, r *http.Request) {
	c, ok := h.lookupClip(w, r)
	if !ok {
		return
	}
	if refuseAudio(w, c) {
		return
	}
	if obj, ok := mediaObject(c, mediaGIF); ok {
		if h.d.Store == nil {
			writeStorageError(w, r, errors.New("storage not configured"))
			return
		}
		h.serveObject(w, r, c.ID, obj)
		return
	}
	if c.State != db.ClipReady {
		// Nothing to preview and nothing to make one from. Same code and
		// same shape as the other media routes' not-ready answer.
		w.Header().Set("Cache-Control", "no-store")
		httpx.WriteError(w, http.StatusConflict, "not_ready", "the clip is still "+c.State)
		return
	}
	// A gif job that has already FAILED is reported, not retried: the route
	// would otherwise re-run a doomed encode for every visitor of a page that
	// links it. The owner's control re-POSTs when a person decides to.
	job, err := h.d.DB.LatestJobOfKind(r.Context(), c.ID, db.KindGIF)
	switch {
	case errors.Is(err, db.ErrNotFound): // never asked for: fall through and enqueue
	case err != nil:
		writeDBError(w, r, err)
		return
	case job.State == db.JobFailed:
		w.Header().Set("Cache-Control", "no-store")
		httpx.WriteError(w, http.StatusServiceUnavailable, "gif_failed", gifFailureMessage(job))
		return
	case job.State == db.JobQueued, job.State == db.JobRunning:
		writeGIFAccepted(w, job.State)
		return
	}
	h.enqueueGIF(w, r, c, false)
}

// startGIF is the owner's explicit (re)generate: after a failure, after a
// trim, or simply because they want the preview refreshed. It always
// enqueues — there is no "already have one" case, because asking for it IS
// the case — and a live job is the only 409.
func (h *handler) startGIF(w http.ResponseWriter, r *http.Request) {
	c, _, ok := h.ownedClip(w, r)
	if !ok {
		return
	}
	if refuseAudio(w, c) {
		return
	}
	h.enqueueGIF(w, r, c, true)
}

// enqueueGIF is the guarded INSERT both routes share. owner distinguishes
// the two answers to "one is already running": the GET says "come back"
// (202, because the caller wants the picture and one is coming), the POST
// says 409 (because the caller asked to START one and did not).
func (h *handler) enqueueGIF(w http.ResponseWriter, r *http.Request, c db.Clip, owner bool) {
	job, err := h.d.DB.EnqueueGIF(r.Context(), c.ID, h.unixNow())
	switch {
	case errors.Is(err, db.ErrConflict):
		if owner {
			httpx.WriteError(w, http.StatusConflict, "gif_in_progress", "a preview of this clip is already being made")
			return
		}
		// The insert lost to a job that already exists — so READ that job and
		// report ITS state. Answering a flat "running" was wrong whenever the
		// winner was still queued (nothing had claimed it yet), which is the
		// common case when two viewers ask within the same second: the body
		// said work had started when none had (§20 erratum 87). A job that
		// finished between the failed insert and this read is gone from the
		// queue, and "running" is the honest fallback there — the caller is
		// about to poll and will get the picture.
		state := db.JobRunning
		if job, jerr := h.d.DB.LatestJobOfKind(r.Context(), c.ID, db.KindGIF); jerr == nil &&
			(job.State == db.JobQueued || job.State == db.JobRunning) {
			state = job.State
		}
		writeGIFAccepted(w, state)
		return
	case errors.Is(err, db.ErrStateMismatch):
		w.Header().Set("Cache-Control", "no-store")
		httpx.WriteError(w, http.StatusConflict, "not_ready", "only a ready clip has a preview")
		return
	case errors.Is(err, db.ErrNotFound):
		writeNotFound(w) // destroyed between the lookup and the insert
		return
	case err != nil:
		writeDBError(w, r, err)
		return
	}
	h.d.Worker.Wake()
	h.d.Log.Info("gif queued", "clip", c.ID, "job", job.ID, "owner", owner)
	writeGIFAccepted(w, job.State)
}

// writeGIFAccepted is the 202 both routes answer with.
func writeGIFAccepted(w http.ResponseWriter, state string) {
	w.Header().Set("Retry-After", gifRetryAfter)
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusAccepted, GIFStatusDTO{State: state})
}

// gifFailureMessage renders the 503's text. This route is PUBLIC (§8.1), so
// the detail it appends has to be one the worker wrote: those carry the
// redaction publicReason applies and the shape a reader can do something
// with. worker.OwnerError drops the texts that reach this column from
// anywhere else — the two sentences internal/db writes when it repairs a job
// nobody is running, which are an operator's words and not a viewer's (§20
// erratum 165). A job with nothing left to say still gets a sentence, never
// an empty message.
func gifFailureMessage(job db.Job) string {
	if job.LastError == nil {
		return "the preview could not be made"
	}
	if detail := worker.OwnerError(db.KindGIF, *job.LastError); detail != "" {
		return "the preview could not be made: " + detail
	}
	return "the preview could not be made"
}
