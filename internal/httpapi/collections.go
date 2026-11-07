package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"clippet/internal/auth"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/ids"
)

// Collections (§17 #9, §20 erratum 82). A collection is an ordered list of
// links to clips and NOTHING ELSE — no likes, no comments, no followers, no
// feed, no author byline on the page, no view count anywhere. Three shapes are
// binding and are what the rest of this file is:
//
//  1. Owner mutations are SESSION-only, exactly as clip mutations are (§14
//     erratum 19): a leaked capture token can upload, and that is all it can
//     do. RequireSession answers a Bearer with 403 token_not_allowed.
//  2. Reading one collection is PUBLIC: the id is 12 crypto-random characters
//     (§7), the link is the auth, and the page a collection exists to be is a
//     link somebody pastes. The clips inside it render as PUBLIC ClipDTOs
//     whoever asks — the owner's private view counts are not in a list.
//  3. The mutating routes answer 404 for "not there" AND for "not yours",
//     because they scope by owner: the lookup is one query with owner_id in
//     its predicate, so the two cases are literally the same result. This is
//     the ONE place the owner-vs-404 shape differs from clips, which answer
//     403 — a clip route has to read the public row first (its GET is public
//     and its DTO is the answer), so it knows the row exists before it checks
//     who is asking. Nothing is leaked either way: the id is public in both.

// collectionCreateRequest / collectionPatchRequest are the two title bodies.
type collectionCreateRequest struct {
	Title *string `json:"title"`
}

type collectionPatchRequest struct {
	Title *string `json:"title"`
}

// collectionClipsRequest is the whole ordered membership, replaced at once.
//
// The field is a POINTER so "absent" and "empty" are different requests.
// `{"clip_ids":[]}` is the deliberate "empty this collection"; `{}` and
// `{"clip_ids":null}` are a client that sent no membership at all, and are
// answered 400 rather than silently clearing a 200-clip list. PUT semantics
// would defend either reading, but PATCH on the sibling route treats an
// absent key as "unchanged", and a body that means "leave it alone" on one
// route and "delete everything" on its neighbour is a footgun, not a
// convention (§20 erratum 82, amended).
type collectionClipsRequest struct {
	ClipIDs *[]string `json:"clip_ids"`
}

// collectionTitle validates a title with the clip-title rule (cleanText, 200
// runes, non-empty after the trim): a collection title is rendered in exactly
// the places a clip title is, so it obeys exactly the same limits.
func collectionTitle(w http.ResponseWriter, raw *string) (string, bool) {
	if raw == nil {
		writeInvalid(w, "title must be 1-200 characters")
		return "", false
	}
	title, err := cleanText(*raw, maxTitleRunes)
	if err != nil || title == "" {
		writeInvalidText(w, "title", "1-200 characters", err)
		return "", false
	}
	return title, true
}

// createCollection is POST /api/collections → 201 CollectionDTO.
func (h *handler) createCollection(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	var req collectionCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	title, ok := collectionTitle(w, req.Title)
	if !ok {
		return
	}
	// The §7 collision loop, the same one ingest runs: draw an id, insert,
	// and on the vanishingly unlikely UNIQUE draw another.
	var col db.Collection
	var err error
	for i := 0; i < idPickAttempts; i++ {
		col, err = h.d.DB.CreateCollection(r.Context(), ids.New(), p.User.ID, title, h.unixNow())
		if !errors.Is(err, db.ErrConflict) {
			break
		}
	}
	switch {
	case errors.Is(err, db.ErrLimitReached):
		httpx.WriteError(w, http.StatusConflict, "too_many_collections",
			"you already have the maximum number of collections")
		return
	case errors.Is(err, db.ErrConflict):
		httpx.WriteError(w, http.StatusInternalServerError, "id_collision", "collection id collided; retry")
		return
	case err != nil:
		writeDBError(w, r, err)
		return
	}
	h.d.Log.Info("collection created", "collection", col.ID, "user", p.User.ID)
	httpx.WriteJSON(w, http.StatusCreated, toCollectionDTO(col, h.d.URLs))
}

// listCollections is GET /api/collections: the caller's OWN collections,
// newest first. There is no shared collection library and no route that lists
// somebody else's — that would be the browse surface a collection is
// deliberately not (§20 erratum 82).
func (h *handler) listCollections(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	cols, err := h.d.DB.ListCollections(r.Context(), p.User.ID)
	if err != nil {
		writeDBError(w, r, err)
		return
	}
	out := collectionList{Collections: make([]CollectionDTO, 0, len(cols))}
	for _, c := range cols {
		out.Collections = append(out.Collections, toCollectionDTO(c, h.d.URLs))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// getCollection is GET /api/collections/{cid}, public: the shell plus its
// clips as PUBLIC ClipDTOs in position order.
func (h *handler) getCollection(w http.ResponseWriter, r *http.Request) {
	col, ok := h.lookupCollection(w, r)
	if !ok {
		return
	}
	clips, err := h.d.DB.CollectionClips(r.Context(), col.ID)
	if err != nil {
		writeDBError(w, r, err)
		return
	}
	tags, err := h.d.DB.TagsForClips(r.Context(), clipIDs(clips))
	if err != nil {
		writeDBError(w, r, err)
		return
	}
	dto := toCollectionDTO(col, h.d.URLs)
	members := make([]ClipDTO, 0, len(clips))
	cache := ownerCache{}
	for _, c := range clips {
		owner, err := h.owner(r.Context(), c, nil, cache)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				h.d.Log.Error("clip owner missing", "clip", c.ID, "owner", c.OwnerID)
			}
			writeDBError(w, r, err)
			return
		}
		// isOwner is FALSE for every caller, including the collection's owner:
		// a list of links is not the place a private view count belongs, and
		// the same bytes are then safe for the anonymous visitor the link was
		// pasted to.
		members = append(members, ToDTO(c, owner, h.d.URLs, false, tags[c.ID]))
	}
	// Always non-nil, EMPTY COLLECTION INCLUDED: this route answers with
	// `clips`, and `[]` is how empty is spelled (§20 erratum 82, amended).
	dto.Clips = &members
	httpx.WriteJSON(w, http.StatusOK, dto)
}

// patchCollection is PATCH /api/collections/{cid} — rename, owner only.
func (h *handler) patchCollection(w http.ResponseWriter, r *http.Request) {
	p, ok := h.collectionPrincipal(w, r)
	if !ok {
		return
	}
	cid, ok := h.collectionID(w, r)
	if !ok {
		return
	}
	var req collectionPatchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	title, ok := collectionTitle(w, req.Title)
	if !ok {
		return
	}
	col, err := h.d.DB.UpdateCollectionTitle(r.Context(), cid, p.User.ID, title, h.unixNow())
	if err != nil {
		writeCollectionError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toCollectionDTO(col, h.d.URLs))
}

// deleteCollection is DELETE /api/collections/{cid} → 204. It removes rows and
// nothing else: no object is touched and no clip changes, because a collection
// never owned any of them.
func (h *handler) deleteCollection(w http.ResponseWriter, r *http.Request) {
	p, ok := h.collectionPrincipal(w, r)
	if !ok {
		return
	}
	cid, ok := h.collectionID(w, r)
	if !ok {
		return
	}
	if err := h.d.DB.DeleteCollection(r.Context(), cid, p.User.ID); err != nil {
		writeCollectionError(w, r, err)
		return
	}
	h.d.Log.Info("collection deleted", "collection", cid, "user", p.User.ID)
	w.WriteHeader(http.StatusNoContent)
}

// writeCollectionFull is the 400 both write paths answer when a collection is
// at its cap — the whole-list PUT and the single-clip POST. One spelling and
// one constant: the PUT used to carry the number as a literal, so changing
// db.MaxClipsPerCollection would have left one of the two routes lying about
// the cap and no test could have told them apart (N067).
func writeCollectionFull(w http.ResponseWriter) {
	writeInvalid(w, "a collection holds at most "+strconv.Itoa(db.MaxClipsPerCollection)+" clips")
}

// putCollectionClips is PUT /api/collections/{cid}/clips: the whole ordered
// membership, replaced in one transaction. The order of `clip_ids` IS the
// order the page renders.
func (h *handler) putCollectionClips(w http.ResponseWriter, r *http.Request) {
	p, ok := h.collectionPrincipal(w, r)
	if !ok {
		return
	}
	cid, ok := h.collectionID(w, r)
	if !ok {
		return
	}
	var req collectionClipsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ClipIDs == nil {
		writeInvalid(w, "clip_ids is required; send [] to empty the collection")
		return
	}
	ids := *req.ClipIDs
	for _, id := range ids {
		// A malformed id can never be a clip the caller owns, so it gets the
		// same answer a stranger's clip gets — and it is refused before the
		// transaction rather than inside it.
		if !validID(id) {
			writeNotYourClip(w, id)
			return
		}
	}
	err := h.d.DB.SetCollectionClips(r.Context(), cid, p.User.ID, ids, h.unixNow())
	var notYours *db.NotYourClipError
	switch {
	case errors.As(err, &notYours):
		writeNotYourClip(w, notYours.ClipID)
		return
	case errors.Is(err, db.ErrLimitReached):
		writeCollectionFull(w)
		return
	case err != nil:
		writeCollectionError(w, r, err)
		return
	}
	col, err := h.d.DB.GetCollection(r.Context(), cid)
	if err != nil {
		writeCollectionError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toCollectionDTO(col, h.d.URLs))
}

// addCollectionClip is POST /api/collections/{cid}/clips/{id}: file ONE clip,
// appended, without sending the whole membership (ROADMAP #17, §20 erratum
// 151). It is the route the clip page's "Collections" checkboxes tick.
//
// It answers with the CollectionDTO the PUT answers with — same shape, same
// `clip_count` — so a client that already knows how to read one reads both,
// and 200 rather than 201 because the thing named by the URL is the
// membership, which is a state and not a new resource with its own address.
// Adding a clip that is already in the collection is 200 too: the caller
// asked for a state, and that state holds.
func (h *handler) addCollectionClip(w http.ResponseWriter, r *http.Request) {
	h.collectionClipEdit(w, r, true)
}

// removeCollectionClip is DELETE /api/collections/{cid}/clips/{id} → 204. It
// is idempotent in the clip (a clip that is not a member is still 204: the
// requested state holds) and never idempotent in the collection (a collection
// that is missing or not the caller's is 404, so the route can no more be
// used to probe somebody else's ids than any other collection route can).
func (h *handler) removeCollectionClip(w http.ResponseWriter, r *http.Request) {
	h.collectionClipEdit(w, r, false)
}

// collectionClipEdit is the body both single-clip routes share: the same
// principal check, the same two id validations and the same error mapping,
// written once so an add and a remove cannot drift apart on any of them.
func (h *handler) collectionClipEdit(w http.ResponseWriter, r *http.Request, add bool) {
	p, ok := h.collectionPrincipal(w, r)
	if !ok {
		return
	}
	cid, ok := h.collectionID(w, r)
	if !ok {
		return
	}
	clipID := r.PathValue("id")
	if !validID(clipID) {
		// A malformed id can never be a clip the caller owns, so it gets the
		// answer a stranger's clip gets — before any database access, which is
		// also the cheapest scan defence (§7). A REMOVE gives the same answer
		// rather than a cheerful 204: "remove this thing that cannot exist" is
		// a client bug, and 204 would hide it.
		writeNotYourClip(w, clipID)
		return
	}
	var err error
	if add {
		err = h.d.DB.AddClipToCollection(r.Context(), cid, p.User.ID, clipID, h.unixNow())
	} else {
		err = h.d.DB.RemoveClipFromCollection(r.Context(), cid, p.User.ID, clipID, h.unixNow())
	}
	var notYours *db.NotYourClipError
	switch {
	case errors.As(err, &notYours):
		writeNotYourClip(w, notYours.ClipID)
		return
	case errors.Is(err, db.ErrLimitReached):
		writeCollectionFull(w)
		return
	case err != nil:
		writeCollectionError(w, r, err)
		return
	}
	if !add {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	col, err := h.d.DB.GetCollection(r.Context(), cid)
	if err != nil {
		writeCollectionError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toCollectionDTO(col, h.d.URLs))
}

// writeNotYourClip is the ONE answer for "this id is not a clip you own",
// whether the id is malformed, unknown, somebody else's or a tombstone: 400
// not_your_clip naming the first offender. Telling those cases apart would
// make the route an existence oracle for the clip id space.
func writeNotYourClip(w http.ResponseWriter, clipID string) {
	httpx.WriteError(w, http.StatusBadRequest, "not_your_clip",
		"clip "+quoteID(clipID)+" is not one of yours")
}

// quoteID renders a client-supplied id for a message: bounded, then Go-quoted
// so a control character or a quote in it cannot break out of the JSON string
// it lands in. Bounded first, because the value is whatever the caller sent.
func quoteID(id string) string {
	const max = 32
	r := []rune(id)
	if len(r) > max {
		id = string(r[:max]) + "…"
	}
	return strconv.Quote(id)
}

// collectionPrincipal returns the session principal of an owner route.
func (h *handler) collectionPrincipal(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return auth.Principal{}, false
	}
	return p, true
}

// collectionID validates {cid} (§7: a malformed id is a 404 before any DB
// access, which is also the cheapest scan defence).
func (h *handler) collectionID(w http.ResponseWriter, r *http.Request) (string, bool) {
	cid := r.PathValue("cid")
	if !validID(cid) {
		writeCollectionNotFound(w)
		return "", false
	}
	return cid, true
}

// lookupCollection resolves {cid} for the public GET.
func (h *handler) lookupCollection(w http.ResponseWriter, r *http.Request) (db.Collection, bool) {
	cid, ok := h.collectionID(w, r)
	if !ok {
		return db.Collection{}, false
	}
	if h.d.DB == nil {
		writeDBError(w, r, errNoDB)
		return db.Collection{}, false
	}
	col, err := h.d.DB.GetCollection(r.Context(), cid)
	if err != nil {
		writeCollectionError(w, r, err)
		return db.Collection{}, false
	}
	return col, true
}

func writeCollectionNotFound(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusNotFound, "not_found", "no such collection")
}

// writeCollectionError maps ErrNotFound to 404 (which is also "not yours" on
// the mutating routes) and anything else to the usual 503.
func writeCollectionError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, db.ErrNotFound) {
		writeCollectionNotFound(w)
		return
	}
	writeDBError(w, r, err)
}
