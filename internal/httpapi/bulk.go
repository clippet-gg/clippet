package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"clippet/internal/auth"
	"clippet/internal/db"
	"clippet/internal/httpx"
)

// POST /api/clips/bulk — the library's multi-select actions (ROADMAP #16,
// §20 errata 117-119).
//
// One request, one action, up to MaxBulkIDs clips, and a PER-ID result. The
// alternative — a loop over the existing single-clip routes in the browser —
// was rejected for two reasons that only show up at twenty rows: it is twenty
// round trips over one connection, each with its own CSRF check and session
// lookup, and a tab closed halfway through leaves the set half-applied with
// nothing that says which half. Here the half that applied is IN the answer.
//
// What it is NOT is a transaction over the set. Each id is applied on its own,
// in its own database transaction, and reported on its own: "delete these
// twenty" where one belongs to somebody else deletes nineteen and says 403
// about the twentieth. Rolling the other nineteen back would be a worse
// answer to a typo, and an all-or-nothing delete of twenty clips' objects is
// not something SQLite could promise anyway (the bucket is not in the
// transaction).

// MaxBulkIDs bounds one bulk request. It is the library's page size: the
// action is "the rows I ticked on this page", so a request naming more clips
// than a page can hold is not the feature asking.
const MaxBulkIDs = db.LibraryPageSize

// The actions. Each is a verb the library's action bar offers; there is no
// generic "PATCH these fields" because a bulk edit of free text (a title, a
// description) would overwrite twenty different values with one, which is the
// accident this whole surface has to be incapable of.
const (
	bulkDelete     = "delete"
	bulkTTL        = "ttl"
	bulkTag        = "tag"
	bulkCollection = "collection"
)

type bulkRequest struct {
	Action string   `json:"action"`
	IDs    []string `json:"ids"`
	// TTLDays is required by, and only read for, the `ttl` action; it takes
	// the §8.2 wire values (0 = never, 1|7|30|90).
	TTLDays ttlDays `json:"ttl_days"`
	// Tag is required by, and only read for, the `tag` action: ONE tag, ADDED
	// to each clip's set (see db.AddClipTag for why this is the one add-one
	// verb in the codebase).
	Tag string `json:"tag"`
	// CollectionID is required by, and only read for, the `collection`
	// action: the clips are APPENDED to it in the order they were sent.
	CollectionID string `json:"collection_id"`
}

// bulkResultDTO is one clip's outcome. Status is the status the equivalent
// single-clip request would have answered with, so a client that already
// knows this API needs no second vocabulary: 204/202 for a delete, 200 for an
// edit, 403 for somebody else's clip, 404 for one that is not there.
type bulkResultDTO struct {
	ID     string  `json:"id"`
	Status int     `json:"status"`
	Error  *string `json:"error"` // the snake_case code, null on success
}

// bulkResponse is always a 200 when the REQUEST was valid: the per-id results
// are the answer, and a transport status cannot say "nineteen of twenty".
type bulkResponse struct {
	Action  string          `json:"action"`
	OK      int             `json:"ok"`
	Failed  int             `json:"failed"`
	Results []bulkResultDTO `json:"results"`
}

// bulkClips applies one action to a bounded set of clips.
//
// Session-only and CSRF-guarded like every other owner mutation (§20 erratum
// 19: a capture token is an upload credential and gets 403 token_not_allowed),
// and OUTSIDE the TimeoutHandler for the same reason DELETE is — a delete
// destroys objects in the bucket inline, and a handler that streams that work
// under a 15 s guillotine would abandon it half-done.
func (h *handler) bulkClips(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	if h.d.DB == nil {
		writeDBError(w, r, errNoDB)
		return
	}
	var req bulkRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ids, ok := bulkIDs(w, req.IDs)
	if !ok {
		return
	}
	apply, ok := h.bulkAction(w, r, p, req)
	if !ok {
		return
	}
	out := bulkResponse{Action: req.Action, Results: make([]bulkResultDTO, 0, len(ids))}
	for _, id := range ids {
		res := apply(id)
		if res.Error == nil {
			out.OK++
		} else {
			out.Failed++
		}
		out.Results = append(out.Results, res)
	}
	h.d.Log.Info("bulk action", "action", req.Action, "user", p.User.ID, "ids", len(ids), "ok", out.OK, "failed", out.Failed)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// bulkIDs validates and de-duplicates the id list. Duplicates COLLAPSE rather
// than being applied twice: the results are one row per clip, and a client
// that ticked the same row twice meant one clip.
func bulkIDs(w http.ResponseWriter, in []string) ([]string, bool) {
	if len(in) == 0 {
		writeInvalid(w, "ids must name at least one clip")
		return nil, false
	}
	if len(in) > MaxBulkIDs {
		writeInvalid(w, "ids must name at most "+strconv.Itoa(MaxBulkIDs)+" clips")
		return nil, false
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, id := range in {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, true
}

// bulkAction validates the REQUEST-level half of an action — the TTL, the
// tag, the collection — and returns the per-id closure. A malformed action
// fails the whole request with the same code the single-clip route would use,
// because nothing about it is per-id: sending an illegal tag is one mistake,
// not twenty.
func (h *handler) bulkAction(w http.ResponseWriter, r *http.Request, p auth.Principal, req bulkRequest) (func(string) bulkResultDTO, bool) {
	switch req.Action {
	case bulkDelete:
		if h.d.Destroy == nil {
			writeDBError(w, r, errors.New("destroy not configured"))
			return nil, false
		}
		// ONE budget for the whole request, not one per clip: N × 60 s is not
		// a request, it is an outage. Ids the budget does not reach are
		// tombstoned instead — which is the 202 the single route already
		// documents, and is what makes them 404 to every reader immediately
		// while the sweeper finishes the objects (§12).
		deadline := h.now().Add(h.d.DestroyBudget)
		return func(id string) bulkResultDTO { return h.bulkDeleteOne(r, p, id, deadline) }, true
	case bulkTTL:
		if !req.TTLDays.present() {
			writeInvalidTTL(w)
			return nil, false
		}
		secs, err := req.TTLDays.resolve(nil)
		if err != nil {
			writeInvalidTTL(w)
			return nil, false
		}
		patch := db.ClipPatch{TTLSeconds: &secs}
		return func(id string) bulkResultDTO { return h.bulkPatchOne(r, p, id, patch) }, true
	case bulkTag:
		tag, err := db.CleanTag(req.Tag)
		if err != nil {
			writeInvalidTags(w, err)
			return nil, false
		}
		return func(id string) bulkResultDTO { return h.bulkTagOne(r, p, id, tag) }, true
	case bulkCollection:
		// The collection is checked ONCE, before any clip is touched: it is a
		// parameter of the request, not of an id, and "not yours" is 404 for
		// the same reason every other collection mutation answers 404 —
		// missing and not-yours are literally the same query result (§20
		// erratum 82). A MISSING or malformed collection_id is the same 404,
		// message included: the parameter that is not there is the collection,
		// and answering "no such clip" would send the caller after a clip that
		// is perfectly fine (§20 errata 30 and 117).
		cid := req.CollectionID
		if !validID(cid) {
			writeCollectionNotFound(w)
			return nil, false
		}
		col, err := h.d.DB.GetCollection(r.Context(), cid)
		if err != nil || col.OwnerID != p.User.ID {
			if err != nil && !errors.Is(err, db.ErrNotFound) {
				writeDBError(w, r, err)
				return nil, false
			}
			writeCollectionNotFound(w)
			return nil, false
		}
		return func(id string) bulkResultDTO { return h.bulkCollectionOne(r, p, cid, id) }, true
	default:
		writeInvalid(w, "action must be one of delete, ttl, tag, collection")
		return nil, false
	}
}

// bulkOwned is the per-id ownership gate: the same 404-before-403 order the
// single-clip routes use (§8), so a bulk result can never say more about a
// stranger's clip than a single request would.
func (h *handler) bulkOwned(ctx context.Context, p auth.Principal, id string) (db.Clip, *bulkResultDTO) {
	if !validID(id) {
		return db.Clip{}, bulkFail(id, http.StatusNotFound, "not_found")
	}
	c, err := h.d.DB.GetClip(ctx, id)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return db.Clip{}, bulkFail(id, http.StatusNotFound, "not_found")
	case err != nil:
		return db.Clip{}, bulkFail(id, http.StatusServiceUnavailable, "unavailable")
	}
	if c.State == db.ClipDeleting {
		return db.Clip{}, bulkFail(id, http.StatusNotFound, "not_found")
	}
	if c.OwnerID != p.User.ID {
		return db.Clip{}, bulkFail(id, http.StatusForbidden, "forbidden")
	}
	return c, nil
}

func bulkOK(id string, status int) bulkResultDTO {
	return bulkResultDTO{ID: id, Status: status}
}

func bulkFail(id string, status int, code string) *bulkResultDTO {
	c := code
	return &bulkResultDTO{ID: id, Status: status, Error: &c}
}

// bulkDeleteOne destroys one clip, or tombstones it when the request's shared
// budget is spent. Either way the clip is gone to every reader when this
// returns; 202 says the objects are the sweeper's now.
func (h *handler) bulkDeleteOne(r *http.Request, p auth.Principal, id string, deadline time.Time) bulkResultDTO {
	if _, bad := h.bulkOwned(r.Context(), p, id); bad != nil {
		return *bad
	}
	left := time.Until(deadline)
	if left > 0 {
		// Detached from the client for the same reason the single DELETE is:
		// once the tombstone is written the work must finish whatever the
		// browser does (§12).
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), left)
		res, err := h.d.Destroy(ctx, id)
		cancel()
		if err == nil && res.RowDeleted {
			return bulkOK(id, http.StatusNoContent)
		}
	}
	// Destroy did not finish (or was never started, the budget being spent):
	// make sure the tombstone is set, which is what 202 promises.
	// MarkDeleting is idempotent on an already-deleting row, and ErrNotFound
	// means the row is already gone — the 204 outcome by another route.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), tombstoneCheckTimeout)
	defer cancel()
	switch err := h.d.DB.MarkDeleting(ctx, id, h.unixNow()); {
	case errors.Is(err, db.ErrNotFound):
		return bulkOK(id, http.StatusNoContent)
	case err == nil:
		h.d.Log.Warn("bulk delete deferred to sweeper", "clip", id)
		return bulkOK(id, http.StatusAccepted)
	default:
		httpx.SetLogField(r, "db_error", err.Error())
		return *bulkFail(id, http.StatusServiceUnavailable, "unavailable")
	}
}

func (h *handler) bulkPatchOne(r *http.Request, p auth.Principal, id string, patch db.ClipPatch) bulkResultDTO {
	if _, bad := h.bulkOwned(r.Context(), p, id); bad != nil {
		return *bad
	}
	_, err := h.d.DB.UpdateClipMeta(r.Context(), id, patch, h.unixNow())
	switch {
	case err == nil:
		return bulkOK(id, http.StatusOK)
	case errors.Is(err, db.ErrNotFound):
		return *bulkFail(id, http.StatusNotFound, "not_found")
	default:
		httpx.SetLogField(r, "db_error", err.Error())
		return *bulkFail(id, http.StatusServiceUnavailable, "unavailable")
	}
}

func (h *handler) bulkTagOne(r *http.Request, p auth.Principal, id, tag string) bulkResultDTO {
	if _, bad := h.bulkOwned(r.Context(), p, id); bad != nil {
		return *bad
	}
	err := h.d.DB.AddClipTag(r.Context(), id, tag, h.unixNow())
	switch {
	case err == nil:
		return bulkOK(id, http.StatusOK)
	case errors.Is(err, db.ErrNotFound):
		return *bulkFail(id, http.StatusNotFound, "not_found")
	case errors.Is(err, db.ErrLimitReached):
		// The cap belongs to the CLIP (§20 erratum 81: at most ten), so it is
		// this clip's answer and not the request's.
		return *bulkFail(id, http.StatusBadRequest, "invalid_tags")
	default:
		httpx.SetLogField(r, "db_error", err.Error())
		return *bulkFail(id, http.StatusServiceUnavailable, "unavailable")
	}
}

func (h *handler) bulkCollectionOne(r *http.Request, p auth.Principal, collectionID, id string) bulkResultDTO {
	if _, bad := h.bulkOwned(r.Context(), p, id); bad != nil {
		return *bad
	}
	err := h.d.DB.AddClipToCollection(r.Context(), collectionID, p.User.ID, id, h.unixNow())
	var notYours *db.NotYourClipError
	switch {
	case err == nil:
		return bulkOK(id, http.StatusOK)
	case errors.As(err, &notYours):
		// bulkOwned already answered for a stranger's clip, so reaching here
		// means the clip was deleted between the two reads.
		return *bulkFail(id, http.StatusNotFound, "not_found")
	case errors.Is(err, db.ErrNotFound):
		// The collection went away mid-request.
		return *bulkFail(id, http.StatusNotFound, "not_found")
	case errors.Is(err, db.ErrLimitReached):
		return *bulkFail(id, http.StatusConflict, "too_many_clips")
	default:
		httpx.SetLogField(r, "db_error", err.Error())
		return *bulkFail(id, http.StatusServiceUnavailable, "unavailable")
	}
}
