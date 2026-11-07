package httpapi

import (
	"errors"
	"math"
	"net/http"

	"clippet/internal/db"
	"clippet/internal/httpx"
)

// The owner's chosen card image (§8.1, ROADMAP #5, §20 errata 127-130). Two
// owner routes over one job, shaped exactly like the trim's:
//
//	POST /api/clips/{id}/poster  {"seconds":n} → 202 {"job_id","state","seconds"}
//	GET  /api/clips/{id}/poster                → 200 PosterStatusDTO
//
// Both are session-only with the CSRF rule of every other owner mutation
// (§20 erratum 19: a capture token is an upload credential and gets 403
// `token_not_allowed`), and the POST is on the `upload` limiter because it
// queues an ffmpeg run that takes a slot from the very semaphore
// TRANSCODE_CONCURRENCY sizes (erratum 79).
//
// The clip is `ready` throughout, so — again — this is NOT the §8.4 progress
// route: GET …/progress keeps reporting the CLIP (ready, 100%) while a card
// image is taken, and GET …/poster reports the JOB.
//
// `seconds` is on the ORIGINAL's timeline, the coordinate system erratum 68
// established for the trim and for the same reason: it is the one that does
// not move under the owner, which is what lets a later cut KEEP the choice
// instead of silently pointing it at a different frame.

// posterEdgeGuardMS is how much of the end of a clip cannot be chosen. A
// seek at or past the last picture makes ffmpeg write no file at all, and a
// request the API had every reason to accept would then come back as a
// failed job (erratum 78's lesson, one artefact along). A tenth of a second
// is at least one frame at every rate this server produces, and it is the
// only part of the timeline the control cannot reach.
const posterEdgeGuardMS = 100

// posterRequest is the POST body. A pointer so "absent" and "0" are
// different things: zero is the first frame, and it is a legal choice.
type posterRequest struct {
	Seconds *float64 `json:"seconds"`
}

// PosterStartDTO is the 202 of POST …/poster: the job that was queued and
// the moment it was queued for (rounded to the millisecond the server will
// actually seek to), and nothing else — every fact about the clip is still
// true, and the caller polls GET …/poster for the rest.
type PosterStartDTO struct {
	JobID   int64   `json:"job_id"`
	State   string  `json:"state"`
	Seconds float64 `json:"seconds"`
}

// PosterStatusDTO is the 200 of GET …/poster: the clip's latest poster job.
// `state` is the JOB's (queued|running|done|failed|cancelled), never the
// clip's — the clip is `ready` for the whole of it. `cancelled` is in the
// enum for the trim's reason: the row can hold it, and a poller that does
// not stop on it polls for ever.
//
// `seconds` is what was ASKED for; `applied_seconds` is the choice the CLIP
// now carries, and it appears once the job is done. They are normally the
// same number — a chosen frame has no keyframe snap to move it — and
// `applied_seconds` is null for a clip whose card image is automatic, which
// is what a swap that lost its race leaves behind.
//
// A `done` job with `applied_seconds: null` is not always a lost race: the
// clip's `poster_seconds` also goes back to NULL when a later trim drops the
// chosen moment from its cut, or a later replacement retires it outright
// (`db.SwapSource`) — either way this route still reports the job that
// finished, with `seconds` possibly naming a moment on a clip's retired
// original, rather than answering 404 (N110, roadmap-run nit sweep: a 404
// here would read as "gone" to a poller and swallow the erratum-130
// lost-race message on the one path that message actually applies to).
type PosterStatusDTO struct {
	JobID          int64    `json:"job_id"`
	State          string   `json:"state"`
	Stage          string   `json:"stage"`
	Error          *string  `json:"error"`
	Seconds        float64  `json:"seconds"`
	AppliedSeconds *float64 `json:"applied_seconds"`
}

// writeInvalidPoster is the single 400 of the feature: one code, one
// sentence naming the rule that failed.
func writeInvalidPoster(w http.ResponseWriter, msg string) {
	httpx.WriteError(w, http.StatusBadRequest, "invalid_poster", msg)
}

// posterChoiceRange is the part of the ORIGINAL's timeline this clip's
// CURRENT rendition can show: it begins where the applied cut begins (zero
// for an untrimmed clip) and runs for the rendition's own measured length,
// less the edge guard.
//
// It is deliberately built from `duration_ms` rather than from
// `trim_end_ms`: the rendition's measured length is what the file HOLDS,
// while the window is what was asked of the original, and a stream copy ends
// on a packet boundary. Choosing the shorter, measured one means the API
// never accepts a moment the worker then cannot find.
func posterChoiceRange(c db.Clip) (first, last float64, ok bool) {
	if c.DurationMS == nil || *c.DurationMS <= 0 {
		return 0, 0, false
	}
	if c.TrimStartMS != nil && *c.TrimStartMS > 0 {
		first = float64(*c.TrimStartMS) / 1000
	}
	span := float64(*c.DurationMS-posterEdgeGuardMS) / 1000
	if span < 0 {
		span = 0
	}
	return first, first + span, true
}

// startPoster validates the chosen moment and enqueues the job.
//
// The clip is never touched here: the 202 says a job exists, nothing more,
// and every fact in the ClipDTO — the poster's URL, its dimensions, its ETag
// — is still true when it lands. That is the same promise the trim's POST
// makes, and it is what lets the card image be replaced without any link,
// any card or any player changing address.
func (h *handler) startPoster(w http.ResponseWriter, r *http.Request) {
	c, _, ok := h.ownedClip(w, r)
	if !ok {
		return
	}
	if refuseAudio(w, c) {
		return
	}
	var req posterRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Seconds == nil {
		writeInvalidPoster(w, "seconds is required")
		return
	}
	if c.State != db.ClipReady {
		httpx.WriteError(w, http.StatusConflict, "not_ready", "only a ready clip has a card image to choose")
		return
	}
	first, last, measured := posterChoiceRange(c)
	if !measured {
		// A ready clip always has a duration (FinalizeTranscode writes the
		// whole set), so this is a row that cannot be true rather than a
		// caller's mistake — and there is nothing to validate against.
		httpx.WriteError(w, http.StatusConflict, "not_ready", "this clip has not been measured yet")
		return
	}
	secs := *req.Seconds
	switch {
	// NaN/Inf are not legal JSON literals, so decodeJSON already turns them
	// into invalid_request before this handler ever sees them (N054) — this
	// guard exists for any future non-wire caller of startPoster, not the API.
	case math.IsNaN(secs), math.IsInf(secs, 0):
		writeInvalidPoster(w, "seconds must be a number of seconds on the original upload's timeline")
		return
	case secs < first || secs > last:
		writeInvalidPoster(w, "seconds must be a moment this clip actually shows")
		return
	}
	// The stored value and the seek ffmpeg is given must be the same number,
	// and the argv carries three decimals. Rounding here rather than in the
	// worker is what makes the 202's `seconds` the truth about what will
	// happen.
	secs = math.Round(secs*1000) / 1000
	// …and the number is normalised to ONE spelling of zero. `-0` passes the
	// range check (IEEE says -0 == 0) and math.Round keeps the sign bit, so
	// without this the echoed `seconds`, the params_json and the ffmpeg argv
	// all read `-0` / `-0.000` for the frame everybody else calls 0 (§20
	// erratum 166). The frame taken is the same one; the answer is not.
	if secs == 0 {
		secs = 0
	}
	job, err := h.d.DB.EnqueuePoster(r.Context(), c.ID, db.MarshalPosterParams(db.PosterParams{Seconds: secs}), h.unixNow())
	switch {
	case errors.Is(err, db.ErrConflict):
		httpx.WriteError(w, http.StatusConflict, "poster_in_progress", "a card image for this clip is already being taken")
		return
	case errors.Is(err, db.ErrStateMismatch):
		httpx.WriteError(w, http.StatusConflict, "not_ready", "only a ready clip has a card image to choose")
		return
	case errors.Is(err, db.ErrNotFound):
		writeNotFound(w) // destroyed between the lookup and the insert
		return
	case err != nil:
		writeDBError(w, r, err)
		return
	}
	h.d.Worker.Wake()
	h.d.Log.Info("poster queued", "clip", c.ID, "job", job.ID, "seconds", secs)
	httpx.WriteJSON(w, http.StatusAccepted, PosterStartDTO{JobID: job.ID, State: job.State, Seconds: secs})
}

// posterStatus reports the clip's LATEST poster job — the row itself, which
// is the whole of it: a poster job keeps no progress registry, because one
// seek has no meaningful percentage and a number nobody can ask for is a
// registry nobody can evict (§20 erratum 129).
//
// A clip whose card image has never been chosen is a 404, so a poller cannot
// read "no job" as "finished", exactly as §8.4 forbids for progress.
func (h *handler) posterStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	c, _, ok := h.ownedClip(w, r)
	if !ok {
		return
	}
	job, err := h.d.DB.LatestJobOfKind(r.Context(), c.ID, db.KindPoster)
	switch {
	case errors.Is(err, db.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "nobody has chosen a card image for this clip")
		return
	case err != nil:
		writeDBError(w, r, err)
		return
	}
	dto := PosterStatusDTO{JobID: job.ID, State: job.State, Stage: job.Stage}
	if p, perr := db.ParsePosterParams(job.ParamsJSON); perr == nil {
		dto.Seconds = p.Seconds
	} else {
		// `seconds` then reads 0, which is indistinguishable from a genuine
		// choice of the first frame — and the wire has no way to say
		// "unknown" without a nullable field this route does not need. The
		// row is unreachable from the write path (startPoster refuses NaN and
		// Infinity, and MarshalPosterParams writes a self-describing string
		// rather than `{}` when it cannot marshal), so what is left is a
		// hand-corrupted row, and the honest fix is to tell the operator
		// instead of the client. Such a job fails at the worker anyway, so a
		// client sees state=failed beside the 0 and never a success (N049).
		h.d.Log.Warn("poster job parameters are unreadable", "clip", c.ID, "job", job.ID, "err", perr)
	}
	// What was ASKED for is in the job row; what the clip CARRIES is on the
	// clip, and a `done` job is the one that wrote it. A swap that lost its
	// race resolves `done` too and wrote nothing, so reading the clip rather
	// than echoing the request is what keeps this answer honest.
	if job.State == db.JobDone {
		dto.AppliedSeconds = c.PosterSeconds
	}
	if job.State == db.JobFailed {
		dto.Error = ownerJobError(db.KindPoster, job.LastError)
	}
	httpx.WriteJSON(w, http.StatusOK, dto)
}
