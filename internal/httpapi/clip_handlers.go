package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"clippet/internal/auth"
	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/httpx"
)

type clipList struct {
	Clips      []ClipDTO `json:"clips"`
	NextCursor *string   `json:"next_cursor"`
}

type clipPatchRequest struct {
	Title       *string   `json:"title"`
	Description *string   `json:"description"`
	Unlisted    *bool     `json:"unlisted"`
	TTLDays     ttlDays   `json:"ttl_days"`
	Tags        *[]string `json:"tags"`
}

type deletingDTO struct {
	State string `json:"state"`
}

// ownerCache memoises owner lookups across one list response.
type ownerCache map[string]db.User

// owner returns the clip's owner, using the principal when it is the owner
// (no lookup) and the cache otherwise.
func (h *handler) owner(ctx context.Context, c db.Clip, p *auth.Principal, cache ownerCache) (db.User, error) {
	if p != nil && p.User.ID == c.OwnerID {
		return p.User, nil
	}
	if u, ok := cache[c.OwnerID]; ok {
		return u, nil
	}
	u, err := h.d.DB.GetUserByID(ctx, c.OwnerID)
	if err != nil {
		return db.User{}, err
	}
	if cache != nil {
		cache[c.OwnerID] = u
	}
	return u, nil
}

// clipDTO renders c for the caller. A missing owner is an invariant
// violation (users cascade-delete their clips): the only ERROR on this path.
// tags is the clip's tag set (nil is fine and renders as `[]`); a list
// handler passes the batch it already fetched rather than one query per row.
func (h *handler) clipDTO(w http.ResponseWriter, r *http.Request, c db.Clip, p *auth.Principal, cache ownerCache, tags []string) (ClipDTO, bool) {
	u, err := h.owner(r.Context(), c, p, cache)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			h.d.Log.Error("clip owner missing", "clip", c.ID, "owner", c.OwnerID)
		}
		writeDBError(w, r, err)
		return ClipDTO{}, false
	}
	// Owner-private fields (the view count, §8.2) are unlocked by a browser
	// session or — since §20 erratum 112 — by the owner's own `read`-scoped
	// token, and by nothing else. A capture token is still an upload
	// credential (erratum 19): getClip is the one owner-reachable route it
	// hits, because that route is public, and it must keep seeing the public
	// DTO of the owner's own clip. The `read` scope exists precisely so the
	// owner's automation can read the owner's library, which is what these
	// fields are.
	isOwner := p != nil && p.OwnerReader() && p.User.ID == c.OwnerID
	return ToDTO(c, u, h.d.URLs, isOwner, tags), true
}

// tagsOf reads one clip's tags for a single-clip response. A failure is a
// backend failure like any other read (503) — a DTO that quietly dropped its
// tags would look like "the clip has none", which is a different fact.
func (h *handler) tagsOf(w http.ResponseWriter, r *http.Request, clipID string) ([]string, bool) {
	tags, err := h.d.DB.TagsOf(r.Context(), clipID)
	if err != nil {
		writeDBError(w, r, err)
		return nil, false
	}
	return tags, true
}

// principal returns the caller (nil when anonymous).
func principal(r *http.Request) *auth.Principal {
	if p, ok := auth.FromContext(r.Context()); ok {
		return &p
	}
	return nil
}

// listClips is the library: every listed clip plus the caller's own
// unlisted ones, views only on the caller's own (§8.1).
//
// `?tag=` narrows that same set (§17 #9). It is a FILTER over the library, not
// a tag page: there is no route that lists a tag to the world, no count beside
// it and nothing that ranks one tag against another — a tag is a word an owner
// wrote on their own clip, and this is where it is useful.
//
// `?q=` and `?mine=1` are the two other filters (§20 errata 114-115), and they
// narrow the same set the same way. `mine` is the VIEWER's own clips and can
// never name another user: there is no user page and no social layer for one
// to point at. What the whole set IS depends on LIBRARY_SCOPE (§20 erratum
// 113), which is the instance's configuration rather than the request's.
func (h *handler) listClips(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	query := r.URL.Query()
	q := db.ListQuery{
		ViewerID: p.User.ID,
		Cursor:   query.Get("cursor"),
		ScopeOwn: h.d.Cfg.LibraryScope == config.LibraryScopeOwn,
	}
	if s := query.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			writeInvalid(w, "limit must be a positive integer")
			return
		}
		q.Limit = n
	}
	if s := query.Get("tag"); s != "" {
		tag, err := db.CleanTag(s)
		if err != nil {
			writeInvalidTags(w, err)
			return
		}
		q.Tag = tag
	}
	if s := query.Get("q"); s != "" {
		search, err := db.CleanSearch(s)
		if err != nil {
			writeInvalid(w, "q must be at most "+strconv.Itoa(db.MaxSearchRunes)+" characters of text")
			return
		}
		q.Q = search
	}
	// `mine` is a switch, not a value: the only spelling that turns it on is
	// `1`, and anything else is a client that meant something this route does
	// not do. Answering 400 rather than quietly ignoring it is the same
	// choice `?tag=` makes for a word no clip can carry — a filter that
	// silently does nothing shows a wider list than the caller asked for.
	if query.Has("mine") {
		if query.Get("mine") != "1" {
			writeInvalid(w, "mine must be 1 (there is no way to list another user's clips)")
			return
		}
		q.Mine = true
	}
	clips, next, err := h.d.DB.ListClips(r.Context(), q)
	if err != nil {
		if errors.Is(err, db.ErrInvalidCursor) {
			httpx.WriteError(w, http.StatusBadRequest, "invalid_cursor", "cursor is not valid")
			return
		}
		writeDBError(w, r, err)
		return
	}
	tags, err := h.d.DB.TagsForClips(r.Context(), clipIDs(clips))
	if err != nil {
		writeDBError(w, r, err)
		return
	}
	out := clipList{Clips: make([]ClipDTO, 0, len(clips))}
	cache := ownerCache{}
	for _, c := range clips {
		dto, ok := h.clipDTO(w, r, c, p, cache, tags[c.ID])
		if !ok {
			return
		}
		out.Clips = append(out.Clips, dto)
	}
	if next != "" {
		out.NextCursor = &next
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// clipIDs projects a page of clips onto their ids (the batch tag lookup).
func clipIDs(clips []db.Clip) []string {
	out := make([]string, len(clips))
	for i, c := range clips {
		out[i] = c.ID
	}
	return out
}

// getClip is public: the unguessable link is the auth; views only for the owner.
func (h *handler) getClip(w http.ResponseWriter, r *http.Request) {
	c, ok := h.lookupClip(w, r)
	if !ok {
		return
	}
	tags, ok := h.tagsOf(w, r, c.ID)
	if !ok {
		return
	}
	dto, ok := h.clipDTO(w, r, c, principal(r), nil, tags)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, dto)
}

// patchClip edits title/description/unlisted/TTL (§8.2 TTL rule: absent or
// null leaves the TTL unchanged; 0 = never; PATCH recomputes expires_at).
func (h *handler) patchClip(w http.ResponseWriter, r *http.Request) {
	c, p, ok := h.ownedClip(w, r)
	if !ok {
		return
	}
	var req clipPatchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	var patch db.ClipPatch
	if req.Title != nil {
		title, err := cleanText(*req.Title, maxTitleRunes)
		if err != nil || title == "" {
			writeInvalidText(w, "title", "1-200 characters", err)
			return
		}
		patch.Title = &title
	}
	if req.Description != nil {
		desc, err := cleanText(*req.Description, maxDescriptionRunes)
		if err != nil {
			writeInvalidText(w, "description", "at most 2000 characters", err)
			return
		}
		patch.Description = &desc
	}
	patch.Unlisted = req.Unlisted
	if req.TTLDays.present() {
		secs, err := req.TTLDays.resolve(nil)
		if err != nil {
			writeInvalidTTL(w)
			return
		}
		patch.TTLSeconds = &secs
	}
	// `tags` replaces the WHOLE set: `[]` clears it, absent leaves it alone
	// (§20 erratum 81). Normalising here rather than in the DB layer is what
	// lets the 400 name the offending word.
	if req.Tags != nil {
		tags, ok := cleanTagList(w, *req.Tags)
		if !ok {
			return
		}
		patch.Tags = &tags
	}
	updated, err := h.d.DB.UpdateClipMeta(r.Context(), c.ID, patch, h.unixNow())
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeNotFound(w)
			return
		}
		writeDBError(w, r, err)
		return
	}
	tags, ok := h.tagsOf(w, r, c.ID)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ToDTO(updated, p.User, h.d.URLs, true, tags))
}

// touchClip is the §8.1 keep-alive: one UPDATE restarting the TTL from now,
// no request body, 200 ClipDTO. The share page's copy-link button calls it
// (§17 #2: re-sharing a link is the clearest signal the clip is still
// wanted), so a clip an owner keeps handing out never quietly expires.
//
// It answers in every state but deleting — a queued or processing clip has a
// TTL like any other — and is a no-op 200 for a clip kept forever, whose
// expires_at stays null. There is no rate limit of its own: it is an owner,
// session-only, CSRF-guarded route doing a single indexed UPDATE, the same
// shape as PATCH, and the §14 hooks stay where the routes that cost real
// work are (auth, upload, view).
func (h *handler) touchClip(w http.ResponseWriter, r *http.Request) {
	c, p, ok := h.ownedClip(w, r)
	if !ok {
		return
	}
	updated, err := h.d.DB.TouchClip(r.Context(), c.ID, h.unixNow())
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeNotFound(w) // destroyed between the lookup and the update
			return
		}
		writeDBError(w, r, err)
		return
	}
	tags, ok := h.tagsOf(w, r, c.ID)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ToDTO(updated, p.User, h.d.URLs, true, tags))
}

// deleteClip runs Destroy inline under DestroyBudget on a context detached
// from the client (§12): 204 when objects and row are gone, 202 when the
// tombstone is set but the sweeper must finish.
func (h *handler) deleteClip(w http.ResponseWriter, r *http.Request) {
	c, _, ok := h.ownedClip(w, r)
	if !ok {
		return
	}
	if h.d.Destroy == nil {
		writeDBError(w, r, errors.New("destroy not configured"))
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), h.d.DestroyBudget)
	defer cancel()
	start := time.Now()
	res, err := h.d.Destroy(ctx, c.ID)
	dur := time.Since(start)
	if err == nil && res.RowDeleted {
		h.d.Log.Info("clip deleted", "clip", c.ID, "objects", res.Objects, "dur", dur)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err == nil {
		err = errors.New("row not deleted")
	}
	// 202 promises the sweeper will finish, which is only true once the
	// tombstone is set; a failure before that (a DB error in MarkDeleting)
	// is a plain 503. The budget context may be spent, so re-read fresh.
	checkCtx, cancelCheck := context.WithTimeout(context.Background(), tombstoneCheckTimeout)
	defer cancelCheck()
	after, gerr := h.d.DB.GetClip(checkCtx, c.ID)
	switch {
	case errors.Is(gerr, db.ErrNotFound):
		h.d.Log.Info("clip deleted", "clip", c.ID, "objects", res.Objects, "dur", dur)
		w.WriteHeader(http.StatusNoContent)
		return
	case gerr != nil:
		writeDBError(w, r, gerr)
		return
	case after.State != db.ClipDeleting:
		h.d.Log.Warn("clip delete failed before the tombstone", "clip", c.ID, "err", err, "dur", dur)
		writeDBError(w, r, err)
		return
	}
	httpx.SetLogField(r, "destroy_deferred", err.Error())
	h.d.Log.Warn("clip delete deferred to sweeper", "clip", c.ID, "err", err, "dur", dur)
	httpx.WriteJSON(w, http.StatusAccepted, deletingDTO{State: db.ClipDeleting})
}

// tombstoneCheckTimeout bounds the post-Destroy state read of deleteClip.
const tombstoneCheckTimeout = 5 * time.Second

// retryClip requeues a failed clip (the source is still in the bucket).
func (h *handler) retryClip(w http.ResponseWriter, r *http.Request) {
	c, p, ok := h.ownedClip(w, r)
	if !ok {
		return
	}
	err := h.d.DB.RetryClip(r.Context(), c.ID, h.unixNow())
	switch {
	case errors.Is(err, db.ErrStateMismatch):
		httpx.WriteError(w, http.StatusConflict, "not_failed", "only failed clips can be retried")
		return
	case errors.Is(err, db.ErrNotFound):
		writeNotFound(w)
		return
	case err != nil:
		writeDBError(w, r, err)
		return
	}
	// The failed run's terminal registry entry is stale now: evict it so
	// /progress falls back to the DB and reports queued, not the old failure
	// (§8.4 — mirrors lifecycle.Destroy step 5).
	h.d.Worker.Forget(c.ID)
	h.d.Worker.Wake()
	updated, err := h.d.DB.GetClip(r.Context(), c.ID)
	if err != nil {
		writeDBError(w, r, err)
		return
	}
	tags, ok := h.tagsOf(w, r, c.ID)
	if !ok {
		return
	}
	h.d.Log.Info("clip retry queued", "clip", c.ID)
	httpx.WriteJSON(w, http.StatusAccepted, ToDTO(updated, p.User, h.d.URLs, true, tags))
}
