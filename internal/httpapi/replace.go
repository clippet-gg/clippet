package httpapi

import (
	"errors"
	"mime/multipart"
	"net/http"
	"path/filepath"

	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/ids"
	"clippet/internal/scratch"
	"clippet/internal/store"
	"clippet/internal/transcode"
	"clippet/internal/worker"
)

// Clip replacement (ROADMAP #31, §8.1, §20 errata 135–136). Two owner routes
// over one job, shaped exactly like the trim's:
//
//	POST /api/clips/{id}/replace  multipart, one file part → 202 {"job_id","state"}
//	GET  /api/clips/{id}/replace                           → 200 ReplaceStatusDTO
//
// Both are session-only with the CSRF rule of every other owner mutation
// (§20 erratum 19: a capture token is an upload credential and gets 403
// `token_not_allowed` — a leaked ShareX token must not be able to overwrite
// the clips it once created), and the POST carries BOTH upload limiters,
// because a replacement is an upload: it spends the same bytes, the same
// scratch and the same ffmpeg slot.
//
// The clip is `ready` throughout. The POST does not touch it at all — it
// stores a new original beside the old one and queues the job — and the swap
// is one guarded UPDATE at the end. Everything a viewer can see (the share
// URL, the page, the card, `/video.mp4`) keeps working for the whole run and
// starts serving the new file at that instant.

// ReplaceStartDTO is the 202 of POST /api/clips/{id}/replace: the job that was
// queued, and nothing else — every fact about the clip is still true, and the
// caller polls GET …/replace for the rest.
type ReplaceStartDTO struct {
	JobID int64  `json:"job_id"`
	State string `json:"state"`
}

// ReplaceStatusDTO is the 200 of GET /api/clips/{id}/replace: the clip's
// latest replacement job. `state` is the JOB's
// (queued|running|done|failed|cancelled), never the clip's — the clip is
// `ready` for the whole of it. `cancelled` is in that enum because the row can
// hold it (§11's lost race: the clip stopped being ready under a running job),
// and a poller that does not stop on it polls for ever.
//
// `filename` is what was uploaded, so a client can say WHICH replacement it is
// watching. It is the owner's own text coming back to the owner; the route is
// session-owner, like the trim's status.
type ReplaceStatusDTO struct {
	JobID    int64   `json:"job_id"`
	State    string  `json:"state"`
	Stage    string  `json:"stage"`
	Percent  int     `json:"percent"`
	Error    *string `json:"error"`
	Filename string  `json:"filename"`
}

// replaceClip ingests a new original for an existing clip and queues the job
// that will swap the whole clip onto it.
//
// The order is the feature: spool → ffprobe gate → PutFile under a FRESH key →
// one guarded INSERT. Nothing before the INSERT touches the clip row, so a
// body that turns out not to be a video, a full disk, a storage outage or a
// second concurrent attempt all leave the clip exactly as it was — still
// serving the file it had.
func (h *handler) replaceClip(w http.ResponseWriter, r *http.Request) {
	c, p, ok := h.ownedClip(w, r)
	if !ok {
		return
	}
	// Only a ready clip has something to replace. A queued or processing one
	// has a transcode in flight that would race this, and a failed one is what
	// POST …/retry and DELETE are for.
	if refuseAudio(w, c) {
		return
	}
	if c.State != db.ClipReady {
		httpx.WriteError(w, http.StatusConflict, "not_ready", "only a ready clip can be replaced")
		return
	}
	// The rest of the precondition is PLAYABLE BYTES, not `rendition_key` — which
	// is what this test used to read (§20 erratum 201, amended). Since the quality
	// tiers that column is legitimately NULL on a `ready` clip: the remux fast path
	// serves the upload itself, and card-first ordering publishes the LIGHT copy
	// while the Full encodes. So the old test answered 409 `not_ready` on a plainly
	// ready clip — on exactly the shape DEPLOY §27 calls the ideal case — with a
	// message its owner could do nothing about. `db.PlayObject` is the one
	// definition of "which bytes does this clip play", and ok=false on a `ready`
	// row is the §12 invariant violation the media routes answer 503 for; it is
	// answered the same way here, because it needs a human and not a retry.
	if _, _, _, ok := db.PlayObject(c); !ok {
		httpx.SetLogField(r, "invariant", "ready_null_keys")
		h.d.Log.Error("ready clip with no playable bytes", "clip", c.ID, "route", "replace")
		httpx.WriteError(w, http.StatusServiceUnavailable, "unavailable", "clip media is unavailable")
		return
	}
	mr, ok := h.beginUpload(w, r)
	if !ok {
		return
	}
	part, ok := h.replaceFilePart(w, r, mr)
	if !ok {
		return
	}
	job, ok := h.ingestReplacement(w, r, c, part)
	if !ok {
		return
	}
	h.d.Worker.Wake()
	h.d.Log.Info("replace queued", "clip", c.ID, "job", job.ID, "user", p.User.ID)
	httpx.WriteJSON(w, http.StatusAccepted, ReplaceStartDTO{JobID: job.ID, State: job.State})
}

// replaceFilePart reads the multipart body up to the file part, which must be
// named `file` or `video` — the same two names POST /api/upload takes, so a
// script that can post one can post the other (§20 erratum 58).
//
// Text fields before it are bounded exactly as the capture form bounds them
// and then IGNORED, every one of them — and no error names one, because the
// name would be the caller's own text: a replacement changes the FILE and
// nothing else. Title, description, tags, TTL and unlisted are the clip's, not
// the upload's, and PATCH is where they are changed — silently applying them
// here would make "I uploaded the wrong file" also mean "and lost my tags".
func (h *handler) replaceFilePart(w http.ResponseWriter, r *http.Request, mr *multipart.Reader) (*multipart.Part, bool) {
	fields := 0
	for {
		part, err := mr.NextPart()
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				httpx.SetLogField(r, "reject", "too_large")
				h.writeTooLarge(w)
				return nil, false
			}
			writeMissingFile(w, r)
			return nil, false
		}
		if part.FileName() != "" {
			if !captureFilePart(part.FormName()) {
				writeMissingFile(w, r)
				return nil, false
			}
			return part, true
		}
		fields++
		if fields > maxCaptureFields {
			httpx.SetLogField(r, "reject", "too_many_fields")
			writeInvalid(w, "too many form fields before the file part")
			return nil, false
		}
		// The label is the FIXED word "form", never part.FormName(); the
		// reason is readCaptureField's own doc comment.
		if _, ok := readCaptureField(w, part, "form"); !ok {
			return nil, false
		}
	}
}

// ingestReplacement spools the new original, gates it through ffprobe, stores
// it under a per-attempt key and queues the job.
//
// It deliberately does NOT reuse `ingest`: that one mints a clip id and INSERTs
// a row, and the whole point here is that the row already exists and must not
// change until the new file is playable. What the two do share is every gate
// that matters — beginUpload's limits, spoolVideo's failure classes and
// probeGate — which is why those are functions rather than inline code.
func (h *handler) ingestReplacement(w http.ResponseWriter, r *http.Request, c db.Clip, part *multipart.Part) (db.Job, bool) {
	filename := part.FileName()
	ext := store.SanitizeExt(filename)
	contentType := store.ContentTypeFor(part.Header.Get("Content-Type"), ext)
	r8 := ids.Rand(8)

	// The spool is the attempt's own directory, never $SCRATCH_DIR/{id}: that
	// one belongs to whatever run is registered for this clip, and a trim
	// keeps the CURRENT original in it under this very name (§20 erratum 136).
	dir := scratch.ReplaceTmpDir(h.d.Cfg.ScratchDir, c.ID, r8)
	src := filepath.Join(dir, sourceFilePrefix+ext)
	size, ok := h.spoolVideo(w, r, part, dir, src)
	if !ok {
		h.removeScratch(dir)
		return db.Job{}, false
	}
	probe, ok := h.probeGate(w, r, src)
	if !ok {
		h.removeScratch(dir)
		return db.Job{}, false
	}
	if probe != nil && probe.Kind == transcode.KindAudio {
		// A video is replaced by a video (§20 erratum 328: same kind only). An
		// audio clip never gets this far — replaceClip answers not_for_audio
		// before the body is read.
		h.removeScratch(dir)
		httpx.SetLogField(r, "reject", "kind_mismatch")
		h.writeIngestError(w, r, failIngest(ingestFailNotVideo,
			&transcode.ErrPermanent{Err: errors.New("a video clip is replaced by a video")}, ""))
		return db.Job{}, false
	}
	// The probe said VIDEO: the part's `audio/…` opinion is not what it is
	// stored (and later served direct) as — ingestCore's rule.
	contentType = videoContentType(ext, contentType)
	if h.d.Store == nil {
		h.removeScratch(dir)
		writeStorageError(w, r, errors.New("storage not configured"))
		return db.Job{}, false
	}
	key := store.ReplacementKey(h.d.Cfg.S3.KeyPrefix, c.ID, r8, ext)
	res, err := h.d.Store.PutFile(r.Context(), key, src, contentType, nil)
	if err != nil {
		// The KEY, never the clip prefix: a key is its own prefix (§20.6), and
		// DeletePrefix on the clip's would destroy the live clip this request
		// was only trying to change.
		h.abortUpload(key, dir)
		if clientGone(r, err) {
			httpx.SetLogField(r, "client_disconnected", true)
			httpx.WriteError(w, http.StatusBadRequest, "upload_interrupted", "client disconnected during upload")
			return db.Job{}, false
		}
		h.d.Log.Warn("replace: store original", "clip", c.ID, "err", err)
		writeStorageError(w, r, err)
		return db.Job{}, false
	}
	params := db.MarshalReplaceParams(db.ReplaceParams{
		SourceKey: key, SourceFilename: baseName(filename), SourceSize: size,
		SourceContentType: contentType, SourceETag: res.SHA256,
		Spool: scratch.ReplaceTmpName(c.ID, r8),
	})
	job, err := h.d.DB.EnqueueReplace(r.Context(), c.ID, params, h.unixNow())
	if err != nil {
		h.abortUpload(key, dir)
		switch {
		case errors.Is(err, db.ErrConflict):
			httpx.WriteError(w, http.StatusConflict, "replace_in_progress", "a replacement of this clip is already running")
		case errors.Is(err, db.ErrStateMismatch):
			httpx.WriteError(w, http.StatusConflict, "not_ready", "only a ready clip can be replaced")
		case errors.Is(err, db.ErrNotFound):
			writeNotFound(w) // destroyed between the lookup and the insert
		default:
			writeDBError(w, r, err)
		}
		return db.Job{}, false
	}
	httpx.SetLogField(r, "clip", c.ID)
	h.d.Log.Info("replacement accepted", "clip", c.ID, "job", job.ID, "bytes", size,
		"content_type", contentType, "key", key)
	return job, true
}

// replaceStatus reports the clip's LATEST replacement job — the live registry
// entry while it runs, the row itself otherwise, which is what makes the
// answer survive a restart. A clip that has never been replaced is a 404, so a
// poller cannot read "no job" as "finished" (§8.4's rule, and the trim's).
func (h *handler) replaceStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	c, _, ok := h.ownedClip(w, r)
	if !ok {
		return
	}
	job, err := h.d.DB.LatestJobOfKind(r.Context(), c.ID, db.KindReplace)
	switch {
	case errors.Is(err, db.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "this clip has never been replaced")
		return
	case err != nil:
		writeDBError(w, r, err)
		return
	}
	dto := ReplaceStatusDTO{JobID: job.ID, State: job.State, Stage: job.Stage}
	if p, perr := db.ParseReplaceParams(job.ParamsJSON); perr == nil {
		dto.Filename = p.SourceFilename
	}
	// The registry is authoritative only while the row still says running:
	// every terminal outcome forgets its entry, and the row is what a restart
	// leaves behind. It is the EDIT registry (§20 erratum 71), shared with the
	// trim, which is safe because at most one run per clip is ever live.
	if pr, found := h.d.Worker.TrimProgress(c.ID); found && job.State == db.JobRunning {
		if pr.Stage != "" {
			dto.Stage = pr.Stage
		}
		dto.Percent = clampPercent(pr.Percent)
		httpx.WriteJSON(w, http.StatusOK, dto)
		return
	}
	switch job.State {
	case db.JobDone:
		dto.Stage, dto.Percent = "done", percentReady
	case db.JobFailed:
		dto.Percent = worker.StagePercent(job.Stage)
		dto.Error = ownerJobError(db.KindReplace, job.LastError)
	default:
		dto.Percent = worker.StagePercent(job.Stage)
	}
	httpx.WriteJSON(w, http.StatusOK, dto)
}
