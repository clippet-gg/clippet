package db

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
)

// Collection limits (§17 #9, §20 erratum 82). Both are guards, not product
// decisions: a personal instance will never approach either, and without them
// one request could ask for an unbounded transaction.
const (
	// MaxCollectionsPerUser caps how many collections one user may own.
	MaxCollectionsPerUser = 500
	// MaxClipsPerCollection caps one collection's membership.
	MaxClipsPerCollection = 200
)

// ErrLimitReached is the cap failure of a create: the row was NOT written.
// httpapi maps it to 409 too_many_collections. It is distinct from
// ErrConflict, which means "this id is taken" and asks the caller to retry.
var ErrLimitReached = errors.New("db: limit reached")

// NotYourClipError names the first clip in a membership request that the
// caller does not own (or that does not exist — the two are the same answer
// on purpose: a collection PUT must not become an oracle for which random
// slugs are live clips). httpapi maps it to 400 not_your_clip.
type NotYourClipError struct{ ClipID string }

func (e *NotYourClipError) Error() string {
	return "clip " + strconv.Quote(e.ClipID) + " is not yours"
}

// Collection is a row of collections plus the derived membership count.
//
// There is no Unlisted field and no column behind one: a collection is never
// listed to anyone but its owner, so the id is the whole access control
// (§20 erratum 82).
type Collection struct {
	ID, OwnerID, Title   string
	CreatedAt, UpdatedAt int64
	// ClipCount counts the members that are still visible — a clip in the
	// `deleting` tombstone state is already gone everywhere else, so it is not
	// counted here either. Filled by ListCollections and GetCollection.
	ClipCount int
}

const collectionCols = `id, owner_id, title, created_at, updated_at`

// collectionCountSQL counts a collection's live members. It is a correlated
// subquery rather than a stored counter for the reason every derived number in
// this codebase is derived: a clip can leave a collection without anything
// asking it to (the FK cascade of an owner DELETE or a TTL sweep, §12), and a
// counter nobody updates on that path is a counter that lies.
const collectionCountSQL = `(SELECT COUNT(*) FROM collection_clips cc
  JOIN clips c ON c.id = cc.clip_id AND c.state <> 'deleting'
  WHERE cc.collection_id = collections.id)`

func scanCollection(s scanner) (Collection, error) {
	var c Collection
	if err := s.Scan(&c.ID, &c.OwnerID, &c.Title, &c.CreatedAt, &c.UpdatedAt, &c.ClipCount); err != nil {
		if err == sql.ErrNoRows {
			return Collection{}, ErrNotFound
		}
		return Collection{}, err
	}
	return c, nil
}

// CreateCollection inserts an empty collection owned by ownerID. A duplicate
// id → ErrConflict (the caller draws another, §7); more than
// MaxCollectionsPerUser → ErrLimitReached with nothing written.
//
// The count and the insert share one transaction, which is what makes the cap
// a cap: the writer pool is a single connection under BEGIN IMMEDIATE, so two
// concurrent creates cannot both read 499.
func (d *DB) CreateCollection(ctx context.Context, id, ownerID, title string, now int64) (Collection, error) {
	var out Collection
	err := d.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM collections WHERE owner_id = ?", ownerID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxCollectionsPerUser {
			return ErrLimitReached
		}
		_, err := tx.ExecContext(ctx,
			"INSERT INTO collections (id, owner_id, title, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
			id, ownerID, title, now, now)
		if err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
		out = Collection{ID: id, OwnerID: ownerID, Title: title, CreatedAt: now, UpdatedAt: now}
		return nil
	})
	if err != nil {
		return Collection{}, err
	}
	return out, nil
}

// GetCollection reads one collection by id, for ANY caller: the link is the
// auth, exactly as it is for a clip (§7). Missing → ErrNotFound.
func (d *DB) GetCollection(ctx context.Context, id string) (Collection, error) {
	return scanCollection(d.ro.QueryRowContext(ctx,
		"SELECT "+collectionCols+", "+collectionCountSQL+" FROM collections WHERE id = ?", id))
}

// ListCollections returns a user's own collections, newest first. There is no
// pagination and none is needed: MaxCollectionsPerUser bounds the result at
// 500 rows of five short columns.
func (d *DB) ListCollections(ctx context.Context, ownerID string) ([]Collection, error) {
	rows, err := d.ro.QueryContext(ctx,
		"SELECT "+collectionCols+", "+collectionCountSQL+" FROM collections WHERE owner_id = ? ORDER BY created_at DESC, id ASC",
		ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Collection{}
	for rows.Next() {
		c, err := scanCollection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CollectionSummary is one collection plus its owner's NAME — the shape the
// operator CLI lists (`clippet admin collections ls`, §20 erratum 275). The name
// rather than the owner id, because an operator types and reads names: every
// other admin listing that mentions an account (`users`, `ls --owner`) does the
// same, and the id is in the row beside it for anything that needs it.
type CollectionSummary struct {
	Collection
	OwnerName string
}

// collectionSummarySQL joins the owner in. The columns are QUALIFIED: `users`
// carries an `id` of its own, so the unqualified list every other collection
// query uses would be ambiguous here.
var collectionSummarySQL = `
SELECT ` + prefixCols(collectionCols, "collections.") + `, ` + collectionCountSQL + `, u.username
FROM collections JOIN users u ON u.id = collections.owner_id`

// ListCollectionSummaries returns collections with their owners' names, newest
// first. ownerID "" is EVERY owner — the operator inventory; a non-empty one
// narrows it to that account, which is what the E2E's own sweep asks for.
//
// It is not paginated for ListCollections' reason exactly: MaxCollectionsPerUser
// bounds one account at 500 rows of six short columns, and an instance this
// design targets has a handful of accounts.
func (d *DB) ListCollectionSummaries(ctx context.Context, ownerID string) ([]CollectionSummary, error) {
	q := collectionSummarySQL
	args := []any{}
	if ownerID != "" {
		q += " WHERE collections.owner_id = ?"
		args = append(args, ownerID)
	}
	q += " ORDER BY collections.created_at DESC, collections.id ASC"
	rows, err := d.ro.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CollectionSummary{}
	for rows.Next() {
		var s CollectionSummary
		if err := rows.Scan(&s.ID, &s.OwnerID, &s.Title, &s.CreatedAt, &s.UpdatedAt, &s.ClipCount, &s.OwnerName); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UpdateCollectionTitle renames a collection the caller owns. A collection
// that is missing OR owned by somebody else → ErrNotFound, both from the same
// predicate: the mutating routes never need the public row, so they scope by
// owner and the two cases are one query and one answer (§20 erratum 82).
func (d *DB) UpdateCollectionTitle(ctx context.Context, id, ownerID, title string, now int64) (Collection, error) {
	var out Collection
	err := d.tx(ctx, func(tx *sql.Tx) error {
		n, err := affected(tx.ExecContext(ctx,
			"UPDATE collections SET title = ?, updated_at = ? WHERE id = ? AND owner_id = ?", title, now, id, ownerID))
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		out, err = scanCollection(tx.QueryRowContext(ctx,
			"SELECT "+collectionCols+", "+collectionCountSQL+" FROM collections WHERE id = ?", id))
		return err
	})
	if err != nil {
		return Collection{}, err
	}
	return out, nil
}

// DeleteCollection removes a collection the caller owns. Its membership rows
// go with it through the FK cascade and NOT ONE CLIP IS TOUCHED — a collection
// is a list of links, so deleting the list deletes links and nothing else.
// Missing or not the caller's → ErrNotFound.
func (d *DB) DeleteCollection(ctx context.Context, id, ownerID string) error {
	n, err := d.exec(ctx, "DELETE FROM collections WHERE id = ? AND owner_id = ?", id, ownerID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetCollectionClips replaces a collection's whole ordered membership in one
// transaction. Rules, all enforced here rather than in the handler so no
// second caller can skip one:
//
//   - the collection must exist and be the caller's, else ErrNotFound;
//   - at most MaxClipsPerCollection ids, else ErrLimitReached;
//   - every id must be a clip the SAME user owns and that is not a `deleting`
//     tombstone, else NotYourClipError naming the FIRST offender — an unknown
//     id and somebody else's clip give the identical answer, so the route is
//     not an existence oracle;
//   - duplicates collapse keeping the first position, so a client that sends
//     the same clip twice gets the list it meant rather than a constraint
//     failure.
//
// Positions are assigned 0..n-1 in the order given: the list the caller sent
// IS the order the page renders.
func (d *DB) SetCollectionClips(ctx context.Context, id, ownerID string, clipIDs []string, now int64) error {
	if len(clipIDs) > MaxClipsPerCollection {
		return ErrLimitReached
	}
	// Collapse duplicates before the transaction: it is pure string work and
	// it is what decides how many ownership checks the transaction runs.
	ordered := make([]string, 0, len(clipIDs))
	seen := make(map[string]bool, len(clipIDs))
	for _, cid := range clipIDs {
		if seen[cid] {
			continue
		}
		seen[cid] = true
		ordered = append(ordered, cid)
	}
	return d.tx(ctx, func(tx *sql.Tx) error {
		var got string
		err := tx.QueryRowContext(ctx, "SELECT id FROM collections WHERE id = ? AND owner_id = ?", id, ownerID).Scan(&got)
		if err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		for _, cid := range ordered {
			if err := ownedClipTx(ctx, tx, cid, ownerID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM collection_clips WHERE collection_id = ?", id); err != nil {
			return err
		}
		for i, cid := range ordered {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO collection_clips (collection_id, clip_id, position) VALUES (?, ?, ?)", id, cid, i); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, "UPDATE collections SET updated_at = ? WHERE id = ?", now, id)
		return err
	})
}

// CollectionClips returns a collection's members in position order, every
// state except `deleting` — so a clip that is still encoding appears as
// `processing`, exactly as it does in the library, rather than vanishing from
// the list its owner put it in and reappearing minutes later.
func (d *DB) CollectionClips(ctx context.Context, collectionID string) ([]Clip, error) {
	rows, err := d.ro.QueryContext(ctx, `
SELECT `+clipColsPrefixed+` FROM collection_clips cc
JOIN clips ON clips.id = cc.clip_id
WHERE cc.collection_id = ? AND clips.state <> 'deleting'
ORDER BY cc.position ASC`, collectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Clip{}
	for rows.Next() {
		c, err := scanClip(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListOwnerClips returns every clip a user owns, newest first, in every state
// but `deleting`. It is the membership picker's source: the owner is choosing
// from their OWN clips, so the library's visibility rule (everybody's listed
// clips) is the wrong set here — a collection may only hold clips the caller
// owns, and offering any other clip would offer a 400.
func (d *DB) ListOwnerClips(ctx context.Context, ownerID string, limit int) ([]Clip, error) {
	if limit <= 0 {
		limit = MaxClipsPerCollection
	}
	rows, err := d.ro.QueryContext(ctx, `
SELECT `+clipCols+` FROM clips
WHERE owner_id = ? AND state <> 'deleting'
ORDER BY created_at DESC, id ASC
LIMIT ?`, ownerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Clip{}
	for rows.Next() {
		c, err := scanClip(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// --- one clip at a time (ROADMAP #17, §20 erratum 151) ----------------------
//
// SetCollectionClips is the whole membership in one request, which is the
// right shape for a picker and the wrong one for "file the clip I am looking
// at": from a clip page the client would have to GET the collection, splice
// itself in and PUT the result back, and two of those racing lose one of the
// two edits with no error anywhere. The two statements below are that edit
// expressed once, server-side, so the clip page needs no read-modify-write.
//
// Both take the same three answers as every other collection mutation: a
// collection that is missing OR not the caller's is ErrNotFound (they scope
// by owner, so the two cases are literally one query), a clip that is not one
// the caller owns is NotYourClipError, and the cap is ErrLimitReached.

// The ADD half is AddClipToCollection, which the library's bulk "add to
// collection" action already needed and already implements (internal/db/bulk.go,
// §20 erratum 119): the same statement, the same three answers, the same
// MAX(position)+1 append. POST …/clips/{id} calls it rather than growing a
// second one, so the single-clip route and the bulk action can never disagree
// about what "add" means.

// RemoveClipFromCollection removes one membership row. It is IDEMPOTENT: a
// clip that is not in the collection is success, because the caller asked for
// a state ("not in this collection") and that state holds. What is never
// idempotent is the collection itself — a collection that is missing or not
// the caller's is still ErrNotFound, so the route can never be used to probe
// somebody else's id space.
//
// NOT ONE CLIP IS TOUCHED, exactly as DeleteCollection touches none: a
// collection is a list of links, and removing a link removes a link.
func (d *DB) RemoveClipFromCollection(ctx context.Context, collectionID, ownerID, clipID string, now int64) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		if err := collectionOfOwnerTx(ctx, tx, collectionID, ownerID); err != nil {
			return err
		}
		n, err := affected(tx.ExecContext(ctx,
			"DELETE FROM collection_clips WHERE collection_id = ? AND clip_id = ?", collectionID, clipID))
		if err != nil || n == 0 {
			// Nothing was a member, so nothing changed: the collection's
			// updated_at is not stamped either, exactly as AddClipToCollection
			// leaves it alone for a clip that was already in.
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE collections SET updated_at = ? WHERE id = ?", now, collectionID)
		return err
	})
}

// collectionOfOwnerTx is the "yours or 404" lookup both statements start with.
func collectionOfOwnerTx(ctx context.Context, tx *sql.Tx, collectionID, ownerID string) error {
	var got string
	err := tx.QueryRowContext(ctx,
		"SELECT id FROM collections WHERE id = ? AND owner_id = ?", collectionID, ownerID).Scan(&got)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	return err
}

// ownedClipTx is SetCollectionClips' membership rule for one id: the clip must
// exist, be the caller's, and not be a tombstone — and an unknown id gets the
// identical answer somebody else's does, so the route is not an existence
// oracle for the clip id space.
func ownedClipTx(ctx context.Context, tx *sql.Tx, clipID, ownerID string) error {
	var owner, state string
	err := tx.QueryRowContext(ctx, "SELECT owner_id, state FROM clips WHERE id = ?", clipID).Scan(&owner, &state)
	if err == sql.ErrNoRows || (err == nil && (owner != ownerID || state == ClipDeleting)) {
		return &NotYourClipError{ClipID: clipID}
	}
	return err
}

// CollectionsOfClip returns the ids of the caller's collections that hold the
// clip, as a set. It is what lets the clip page render its "Collections"
// checkboxes already ticked, in ONE query instead of one per collection.
func (d *DB) CollectionsOfClip(ctx context.Context, clipID, ownerID string) (map[string]bool, error) {
	rows, err := d.ro.QueryContext(ctx, `
SELECT cc.collection_id FROM collection_clips cc
JOIN collections c ON c.id = cc.collection_id
WHERE cc.clip_id = ? AND c.owner_id = ?`, clipID, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// clipColsPrefixed is clipCols qualified with the table name, for the one
// query that joins clips to another table and would otherwise be ambiguous.
var clipColsPrefixed = prefixCols(clipCols, "clips.")

func prefixCols(cols, prefix string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = prefix + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}
