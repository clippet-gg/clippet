package httpapi

import (
	"errors"
	"net/http"

	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/worker"
)

// Server-side trim (§8.1, §17 #5). Two owner routes over one job:
//
//	POST /api/clips/{id}/trim  {"start_ms","end_ms"} → 202 {"job_id","state"}
//	GET  /api/clips/{id}/trim                        → 200 TrimStatusDTO
//
// Both are session-only with the CSRF rule of every other owner mutation
// (§20 erratum 19: a capture token is an upload credential and gets 403
// `token_not_allowed`), and the POST is on the `upload` limiter because a
// trim spends the same transcode slot an upload does — literally, since
// erratum 79: its ffmpeg stages take a token from the very semaphore
// TRANSCODE_CONCURRENCY sizes.
//
// The clip is `ready` throughout, so this is NOT the §8.4 progress route and
// must never be confused with it: GET …/progress keeps reporting the CLIP
// (ready, 100%) while a trim runs, and GET …/trim reports the JOB.

// minTrimDurationMS is the shortest window a trim may produce. Half a second
// is already two thirds of a Discord card's autoplay loop; below it the
// result is not a clip, and the poster seek and the encoder's own GOP make it
// a worse one the shorter it gets.
const minTrimDurationMS = 500

// maxUnknownWindowMS bounds a window on a clip whose ORIGINAL has not been
// measured — a row that predates migration 0003 and has already been trimmed,
// which is the only way the column is NULL. There is nothing to validate
// against, so the handler stops being the thing that validates: it refuses
// only the absurd (a day, which no upload this server accepts can reach) and
// lets the WORKER, which is about to probe the original anyway, reject a
// window the file does not cover — as a client-class failure with a plain
// reason, not as a server error (§20 erratum 78).
const maxUnknownWindowMS = 24 * 60 * 60 * 1000

type trimRequest struct {
	StartMS *int64 `json:"start_ms"`
	EndMS   *int64 `json:"end_ms"`
}

// writeInvalidTrim is the single 400 of the feature: one code, one sentence
// naming the rule that failed.
func writeInvalidTrim(w http.ResponseWriter, msg string) {
	httpx.WriteError(w, http.StatusBadRequest, "invalid_trim", msg)
}

// currentWindow is the window the clip's CURRENT rendition was cut from, on
// the original's timeline: the persisted pair when the clip has been trimmed,
// and 0..source_duration_ms when it has not. ok is false when the original's
// length is unknown and the clip has never been trimmed — then there is no
// window to compare a request against, and no no-op to reject.
func currentWindow(c db.Clip) (start, end int64, ok bool) {
	if c.TrimStartMS != nil && c.TrimEndMS != nil {
		return *c.TrimStartMS, *c.TrimEndMS, true
	}
	if c.SourceDurationMS != nil && *c.SourceDurationMS > 0 {
		return 0, *c.SourceDurationMS, true
	}
	return 0, 0, false
}

// startTrim validates the window and enqueues the job.
//
// The window is measured on the ORIGINAL, always (§20 erratum 68). That is
// the whole contract: the upload is retained for the life of the clip, so its
// timeline is the one that never moves, and a clip cut to 5s–9s can be
// re-extended to 2s–4s because both windows name the same seconds of the same
// file. Validating against the RENDITION's duration — which is what this did
// first — silently made every cut permanent: after one trim the reachable
// region had shrunk to what was left, for ever.
//
// The "no-op" rejection is deliberate and is now measured against the CURRENT
// window rather than against "the whole clip": asking for exactly the cut the
// clip already has would re-encode it, swap in a new rendition and change
// every ETag for no change at all, which is the one outcome nobody asked for.
func (h *handler) startTrim(w http.ResponseWriter, r *http.Request) {
	c, _, ok := h.ownedClip(w, r)
	if !ok {
		return
	}
	if refuseAudio(w, c) {
		return
	}
	var req trimRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.StartMS == nil || req.EndMS == nil {
		writeInvalidTrim(w, "start_ms and end_ms are required")
		return
	}
	if c.State != db.ClipReady || c.DurationMS == nil {
		httpx.WriteError(w, http.StatusConflict, "not_ready", "only a ready clip can be trimmed")
		return
	}
	start, end := *req.StartMS, *req.EndMS
	limit := int64(maxUnknownWindowMS)
	if c.SourceDurationMS != nil && *c.SourceDurationMS > 0 {
		limit = *c.SourceDurationMS
	} else if *c.DurationMS > limit {
		limit = *c.DurationMS
	}
	curStart, curEnd, haveWindow := currentWindow(c)
	switch {
	case start < 0 || end <= start:
		writeInvalidTrim(w, "start_ms must be at least 0 and less than end_ms")
		return
	case end > limit:
		writeInvalidTrim(w, "end_ms must not be past the end of the original upload")
		return
	case end-start < minTrimDurationMS:
		writeInvalidTrim(w, "the trimmed clip must be at least 500 ms long")
		return
	case haveWindow && start == curStart && end == curEnd:
		writeInvalidTrim(w, "that is the cut this clip already has; there is nothing to change")
		return
	}
	job, err := h.d.DB.EnqueueTrim(r.Context(), c.ID, db.MarshalTrimParams(db.TrimParams{StartMS: start, EndMS: end}), h.unixNow())
	switch {
	case errors.Is(err, db.ErrConflict):
		httpx.WriteError(w, http.StatusConflict, "trim_in_progress", "a trim of this clip is already running")
		return
	case errors.Is(err, db.ErrStateMismatch):
		httpx.WriteError(w, http.StatusConflict, "not_ready", "only a ready clip can be trimmed")
		return
	case errors.Is(err, db.ErrNotFound):
		writeNotFound(w) // destroyed between the lookup and the insert
		return
	case err != nil:
		writeDBError(w, r, err)
		return
	}
	h.d.Worker.Wake()
	h.d.Log.Info("trim queued", "clip", c.ID, "job", job.ID, "start_ms", start, "end_ms", end,
		"source_duration_ms", limit)
	httpx.WriteJSON(w, http.StatusAccepted, TrimStartDTO{JobID: job.ID, State: job.State})
}

// trimStatus reports the clip's LATEST trim job — the live registry entry
// while it runs, the row itself otherwise, which is what makes the answer
// survive a restart. A clip that has never been trimmed is a 404, so a
// poller cannot read "no job" as "finished".
func (h *handler) trimStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	c, _, ok := h.ownedClip(w, r)
	if !ok {
		return
	}
	job, err := h.d.DB.LatestJobOfKind(r.Context(), c.ID, db.KindTrim)
	switch {
	case errors.Is(err, db.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "this clip has no trim")
		return
	case err != nil:
		writeDBError(w, r, err)
		return
	}
	dto := TrimStatusDTO{JobID: job.ID, State: job.State, Stage: job.Stage}
	if p, perr := db.ParseTrimParams(job.ParamsJSON); perr == nil {
		dto.StartMS, dto.EndMS = p.StartMS, p.EndMS
	}
	// What was ASKED for is in the job row; what was APPLIED is on the clip,
	// and the two differ by design — the copy path snaps the start back to a
	// keyframe, and a window running past the video stream is clamped to it.
	// A `done` job is the one that wrote those columns, so this is its result.
	if job.State == db.JobDone && c.TrimStartMS != nil && c.TrimEndMS != nil {
		dto.AppliedStartMS, dto.AppliedEndMS = c.TrimStartMS, c.TrimEndMS
	}
	// The registry is authoritative only while the row still says running:
	// every terminal outcome forgets its entry, and the row is what a
	// restart leaves behind.
	if p, found := h.d.Worker.TrimProgress(c.ID); found && job.State == db.JobRunning {
		if p.Stage != "" {
			dto.Stage = p.Stage
		}
		dto.Percent = clampPercent(p.Percent)
		httpx.WriteJSON(w, http.StatusOK, dto)
		return
	}
	switch job.State {
	case db.JobDone:
		dto.Stage, dto.Percent = "done", percentReady
	case db.JobFailed:
		dto.Percent = worker.StagePercent(job.Stage)
		dto.Error = ownerJobError(db.KindTrim, job.LastError)
	default:
		dto.Percent = worker.StagePercent(job.Stage)
	}
	httpx.WriteJSON(w, http.StatusOK, dto)
}
