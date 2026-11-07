package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"clippet/internal/auth"
	"clippet/internal/chunks"
	"clippet/internal/db"
	"clippet/internal/httpx"
)

// The chunked web upload (DESIGN.md §20 erratum 344).
//
// The edge in front of a Pass name refuses any one request body over its plan's
// cap — 100 MB on Cloudflare's Free and Pro plans, answered with a 413 before
// the guard or the tunnel ever sees the request — so a one-shot POST of a 2 GB
// clip to a `*.clippet.gg` name never reaches the box at all. The remedy is the
// edge's own documented one: send the file in pieces, each a request under the
// cap. Four routes under one family:
//
//	POST   /api/uploads                 open: {size, name, type, + the metadata document} → 201 {id, chunk_bytes, received: 0}
//	PUT    /api/uploads/{uid}/{index}   one chunk, exactly its length; a chunk already in is 200 unread
//	POST   /api/uploads/{uid}/finish    the ingest core on the assembled file → the one-shot's 201 (or 200 + existing)
//	GET    /api/uploads/{uid}           {received, …} for a resume, and the clip once it is finished
//	DELETE /api/uploads/{uid}           abandon
//
// THE ONE-INGEST RULE: finish runs `ingestCore` — the function POST /api/clips
// runs, with the assembled file as its source instead of a multipart part — and
// writes its answer through the same `writeIngestError` and the same DTO. The
// duplicate check, the quota, the working-disk check, the probe, the audio
// strip, the store and the transaction are therefore not "the same as" the
// one-shot's: they ARE the one-shot's. Nothing here decides anything about a
// clip.
//
// The open is where the one-shot's gates that can be asked BEFORE the bytes are
// asked: the credential and CSRF chain, both upload limiters (one upload is one
// token, however many chunks it takes — the chunks carry no limiter), the
// metadata document by `checkMeta`, MAX_UPLOAD_BYTES against the declared size,
// the scratch precheck for that size, and the storage quota. So a friend's 2 GB
// file is refused in one small request rather than after it has been sent.

// uploadSessionDTO is every session route's answer but finish's.
type uploadSessionDTO struct {
	ID         string `json:"id"`
	Size       int64  `json:"size"`
	ChunkBytes int64  `json:"chunk_bytes"`
	// Received is how many chunks are in: the next one to send is this index.
	Received int `json:"received"`
	// Finishing says a finish is running in this process right now.
	Finishing bool `json:"finishing,omitempty"`
	// Clip is the finished upload's clip, on GET once finish has succeeded:
	// what a client whose finish answer was lost reads instead.
	Clip *ClipDTO `json:"clip,omitempty"`
}

// The open's own refusals.
const (
	msgUploadSize    = "size must be a positive integer"
	msgNoUpload      = "no such upload"
	msgChunkOrder    = "chunk out of order"
	msgChunkIndex    = "chunk index out of range"
	msgChunkLength   = "chunk length does not match"
	msgChunkNoLength = "Content-Length is required"
	msgIncomplete    = "upload is missing chunks"
)

// openRequest is the open's own half of the body; the rest of the same JSON
// object is the one-shot's metadata document, read into uploadMeta.
type openRequest struct {
	Size      *int64 `json:"size"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Duplicate string `json:"duplicate"`
}

func (h *handler) chunkBytes() int64 { return h.d.Cfg.UploadChunkBytes }

// openUpload is POST /api/uploads.
func (h *handler) openUpload(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMetadataBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeInvalid(w, "metadata part larger than 16 KiB")
			return
		}
		writeInvalid(w, "could not read the metadata part")
		return
	}
	var req openRequest
	if err := json.Unmarshal(data, &req); err != nil {
		writeInvalid(w, jsonShapeMessage(err, "body must be a JSON object"))
		return
	}
	var meta uploadMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		writeInvalid(w, jsonShapeMessage(err, "body must be a JSON object"))
		return
	}
	if req.Size == nil || *req.Size <= 0 {
		httpx.SetLogField(r, "reject", "invalid_size")
		writeInvalid(w, msgUploadSize)
		return
	}
	size := *req.Size
	if size > h.d.Cfg.MaxUploadBytes {
		httpx.SetLogField(r, "reject", "too_large")
		h.writeTooLarge(w)
		return
	}
	if _, ok := parseDuplicate(req.Duplicate); !ok {
		httpx.SetLogField(r, "reject", "invalid_duplicate")
		writeInvalid(w, msgInvalidDuplicate)
		return
	}
	if !h.checkMeta(w, r, &meta) {
		return
	}
	if free, ok := h.scratchHasRoom(declaredScratchNeed(size, h.d.Cfg.ScratchMinFree)); !ok {
		httpx.SetLogField(r, "scratch_free", free)
		httpx.SetLogField(r, "reject", "insufficient_scratch")
		httpx.WriteError(w, http.StatusInsufficientStorage, "insufficient_scratch", "not enough scratch space for this upload")
		return
	}
	if err := h.storageQuota(r.Context()); err != nil {
		h.writeIngestError(w, r, err)
		return
	}
	s, err := h.uploads.Open(chunks.Session{
		Owner: p.User.ID, Size: size, ChunkBytes: h.chunkBytes(),
		Name: req.Name, Type: req.Type, Meta: json.RawMessage(data),
	})
	if err != nil {
		h.writeSessionError(w, r, chunks.Session{}, err)
		return
	}
	httpx.SetLogField(r, "upload", s.ID)
	httpx.SetLogField(r, "bytes", size)
	httpx.WriteJSON(w, http.StatusCreated, sessionDTO(s, nil))
}

// getUpload is GET /api/uploads/{uid}.
func (h *handler) getUpload(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	s, err := h.uploads.Get(p.User.ID, r.PathValue("uid"))
	if err != nil {
		h.writeSessionError(w, r, s, err)
		return
	}
	var clip *ClipDTO
	if s.Done() {
		dto, ok := h.finishedDTO(w, r, p, s)
		if !ok {
			return
		}
		clip = &dto
	}
	httpx.WriteJSON(w, http.StatusOK, sessionDTO(s, clip))
}

// deleteUpload is DELETE /api/uploads/{uid}: 204, and the session is gone —
// or, while its finish runs, goes when that returns (the clip it makes is made).
func (h *handler) deleteUpload(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	if err := h.uploads.Delete(p.User.ID, r.PathValue("uid")); err != nil {
		h.writeSessionError(w, r, chunks.Session{}, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// putChunk is PUT /api/uploads/{uid}/{index}: ONE chunk, whose length is the
// server's to know (chunk_bytes, the last one the remainder) and must be
// declared — a chunk is never sent as a stream of unknown length. A chunk
// already received answers 200 without a byte of its body read; a chunk past
// the next one is 409 with `received`, which is where the client resumes.
func (h *handler) putChunk(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	id := r.PathValue("uid")
	index, ok := parseChunkIndex(r.PathValue("index"))
	if !ok {
		httpx.SetLogField(r, "reject", "chunk_index")
		writeInvalid(w, msgChunkIndex)
		return
	}
	st := h.uploads
	s, err := st.Get(p.User.ID, id)
	if err != nil {
		h.writeSessionError(w, r, s, err)
		return
	}
	httpx.SetLogField(r, "upload", s.ID)
	if index < s.Received || index >= s.Chunks() {
		// Answered by the store with no byte read: a chunk already in, or an
		// index the file does not have.
		s, _, err = st.Put(p.User.ID, id, index, 0, http.NoBody)
		if err != nil {
			h.writeSessionError(w, r, s, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, sessionDTO(s, nil))
		return
	}
	want := s.ChunkLen(index)
	if r.ContentLength < 0 {
		httpx.SetLogField(r, "reject", "length_required")
		httpx.WriteError(w, http.StatusLengthRequired, "length_required", msgChunkNoLength)
		return
	}
	if r.ContentLength != want {
		httpx.SetLogField(r, "reject", "chunk_length")
		writeInvalid(w, msgChunkLength)
		return
	}
	if free, ok := h.scratchHasRoom(want + h.d.Cfg.ScratchMinFree); !ok {
		httpx.SetLogField(r, "scratch_free", free)
		httpx.SetLogField(r, "reject", "insufficient_scratch")
		httpx.WriteError(w, http.StatusInsufficientStorage, "insufficient_scratch", "not enough scratch space for this upload")
		return
	}
	bodyWillBeRead(w)
	s, _, err = st.Put(p.User.ID, id, index, want, http.MaxBytesReader(w, r.Body, want))
	if err != nil {
		h.writeSessionError(w, r, s, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sessionDTO(s, nil))
}

// finishUpload is POST /api/uploads/{uid}/finish: the ingest core over the
// assembled file, and the one-shot upload's own answer — 201 with the clip, or
// 200 with the account's existing clip of these bytes and `"existing": true`.
//
// The core runs under a context the CLIENT cannot cancel. A 4 GiB file's probe
// and store can outlast the edge's wait for an answer (the edge then answers the
// browser itself and drops the connection), and a finish that died with the
// connection would throw the whole upload away at its last step. The client
// asks GET for the result instead; a second finish waits for the first and
// answers what it made.
func (h *handler) finishUpload(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	id := r.PathValue("uid")
	ctx := context.WithoutCancel(r.Context())
	var meta uploadMeta
	// ran: the store called the function (a finish already done does not);
	// answered: the function wrote the refusal itself.
	var ran, answered bool
	var ingestErr error
	s, err := h.uploads.Finish(p.User.ID, id, func(s chunks.Session, data string) (chunks.Result, error) {
		ran = true
		var req openRequest
		if err := json.Unmarshal(s.Meta, &meta); err != nil {
			answered = true
			writeInvalid(w, jsonShapeMessage(err, "body must be a JSON object"))
			return chunks.Result{}, err
		}
		_ = json.Unmarshal(s.Meta, &req)
		meta.allowDuplicate, _ = parseDuplicate(req.Duplicate)
		// The document passed checkMeta at the open; it is resolved again here
		// because the resolved half (the defaults, the id grammar) is the
		// account's NOW, exactly as a one-shot upload sent now would get.
		if !h.checkMeta(w, r, &meta) {
			answered = true
			return chunks.Result{}, errInvalidLinkStyle
		}
		clip, existing, err := h.ingestCore(ctx, p.User.ID, meta, ingestSource{
			Spooled: data, Filename: s.Name, ContentType: s.Type,
		})
		if err != nil {
			ingestErr = err
			return chunks.Result{}, err
		}
		return chunks.Result{Clip: clip.ID, Existing: existing}, nil
	})
	switch {
	case err == nil:
	case answered:
		return
	case ingestErr != nil:
		h.writeIngestError(w, r, ingestErr)
		return
	default:
		h.writeSessionError(w, r, s, err)
		return
	}
	httpx.SetLogField(r, "upload", s.ID)
	httpx.SetLogField(r, "clip", s.Clip)
	if s.Existing {
		httpx.SetLogField(r, "existing", true)
	}
	clip, err := h.d.DB.GetClip(r.Context(), s.Clip)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeNotFound(w)
			return
		}
		writeDBError(w, r, err)
		return
	}
	tags := meta.tags
	if !ran || s.Existing {
		var ok bool
		if tags, ok = h.tagsOf(w, r, clip.ID); !ok {
			return
		}
	}
	dto := ToDTO(clip, p.User, h.d.URLs, true, tags)
	if s.Existing {
		dto.Existing = true
		httpx.WriteJSON(w, http.StatusOK, dto)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, dto)
}

// finishedDTO is the clip a finished session made, as GET shows it.
func (h *handler) finishedDTO(w http.ResponseWriter, r *http.Request, p auth.Principal, s chunks.Session) (ClipDTO, bool) {
	clip, err := h.d.DB.GetClip(r.Context(), s.Clip)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeNotFound(w)
		} else {
			writeDBError(w, r, err)
		}
		return ClipDTO{}, false
	}
	tags, ok := h.tagsOf(w, r, clip.ID)
	if !ok {
		return ClipDTO{}, false
	}
	dto := ToDTO(clip, p.User, h.d.URLs, true, tags)
	dto.Existing = s.Existing
	return dto, true
}

func sessionDTO(s chunks.Session, clip *ClipDTO) uploadSessionDTO {
	return uploadSessionDTO{ID: s.ID, Size: s.Size, ChunkBytes: s.ChunkBytes, Received: s.Received,
		Finishing: s.Finishing, Clip: clip}
}

// writeSessionError maps the store's errors onto the routes' answers. The ones
// that tell a client where it stands (out of order, incomplete) carry the
// session, so its `received` is in the same body as the refusal.
func (h *handler) writeSessionError(w http.ResponseWriter, r *http.Request, s chunks.Session, err error) {
	var de *chunks.DiskError
	var mbe *http.MaxBytesError
	switch {
	case errors.Is(err, chunks.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", msgNoUpload)
	case errors.Is(err, chunks.ErrOutOfOrder):
		httpx.SetLogField(r, "reject", "chunk_order")
		writeSessionRefusal(w, http.StatusConflict, "out_of_order", msgChunkOrder, s)
	case errors.Is(err, chunks.ErrIncomplete):
		httpx.SetLogField(r, "reject", "incomplete")
		writeSessionRefusal(w, http.StatusConflict, "incomplete", msgIncomplete, s)
	case errors.Is(err, chunks.ErrIndex):
		httpx.SetLogField(r, "reject", "chunk_index")
		writeInvalid(w, msgChunkIndex)
	case errors.Is(err, chunks.ErrLength):
		httpx.SetLogField(r, "reject", "chunk_length")
		writeInvalid(w, msgChunkLength)
	case errors.As(err, &mbe):
		httpx.SetLogField(r, "reject", "chunk_length")
		writeInvalid(w, msgChunkLength)
	case errors.As(err, &de):
		h.writeIngestError(w, r, h.scratchFailed(de))
	case errors.Is(err, chunks.ErrShort), clientGone(r, err):
		httpx.SetLogField(r, "client_disconnected", true)
		httpx.WriteError(w, http.StatusBadRequest, "upload_interrupted", "client disconnected during upload")
	default:
		writeDBError(w, r, err)
	}
}

// writeSessionRefusal is the error envelope with the session's position in it.
func writeSessionRefusal(w http.ResponseWriter, status int, code, msg string, s chunks.Session) {
	httpx.WriteJSON(w, status, map[string]any{
		"error": code, "message": msg, "received": s.Received, "chunk_bytes": s.ChunkBytes, "size": s.Size,
	})
}

// parseChunkIndex reads {index}: a decimal integer with no sign, no leading
// zero and at most six digits (a 4 GiB file in 5 MiB chunks is 820 of them).
func parseChunkIndex(s string) (int, bool) {
	if s == "" || len(s) > 6 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}
