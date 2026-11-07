package db

import (
	"context"
	"database/sql"
)

// The write halves of the library's bulk actions (ROADMAP #16, §20 errata
// 118–119). Each function is ONE transaction over ONE clip: a bulk request
// applies them in a loop and reports per id, so nothing here ever half-applies
// a clip and no clip's outcome depends on another's.

// AddClipTag adds ONE tag to a clip, leaving the rest of the set alone.
//
// This is the only add-one tag verb in the codebase, and erratum 81 explains
// why there is otherwise none: the editor driving PATCH is a text field
// holding the whole list, so a partial verb there would need a merge rule
// nobody can see in the box they are typing into. A bulk action is the
// opposite shape — one word, twenty clips, each with a different existing
// set — and REPLACING those sets is exactly the accident (§20 erratum 95's
// lesson about the multi-select) this must not be able to cause.
//
// Already present → success, nothing written and updated_at not stamped: an
// action that says "these twenty carry `clutch`" is done when they do.
// A clip that already carries MaxTagsPerClip other tags → ErrLimitReached,
// because the cap is the clip's, not the request's. Missing or deleting →
// ErrNotFound. The tag must already be normalised (CleanTag); the caller
// rejects an illegal one with a 400 naming it long before it reaches SQL.
func (d *DB) AddClipTag(ctx context.Context, clipID, tag string, now int64) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		var state string
		if err := tx.QueryRowContext(ctx, "SELECT state FROM clips WHERE id = ?", clipID).Scan(&state); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		if state == ClipDeleting {
			return ErrNotFound
		}
		var n int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM clip_tags WHERE clip_id = ?", clipID).Scan(&n); err != nil {
			return err
		}
		var have int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM clip_tags WHERE clip_id = ? AND tag = ?", clipID, tag).Scan(&have); err != nil {
			return err
		}
		if have > 0 {
			return nil
		}
		if n >= MaxTagsPerClip {
			return ErrLimitReached
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO clip_tags (clip_id, tag) VALUES (?, ?)", clipID, tag); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			"UPDATE clips SET updated_at = ? WHERE id = ?", now, clipID)
		return err
	})
}

// AddClipToCollection appends ONE clip to the END of a collection the caller
// owns, without disturbing the order of the members already in it.
//
// SetCollectionClips replaces the whole ordered membership, which is right for
// the picker that sends the whole list and wrong for "file these five as
// well": a client that had to read-modify-write the membership to add a clip
// would race every other tab doing the same and would need the whole list in
// hand to add one row (§20 erratum 95's rule about not losing work).
//
//   - collection missing or not the caller's → ErrNotFound (erratum 82: the
//     mutating routes scope by owner, so the two cases are one query);
//   - clip missing, deleting, or somebody else's → NotYourClipError, the same
//     answer for all three so the route is not an existence oracle;
//   - already a member → success, position unchanged, updated_at unstamped;
//   - the collection is already at MaxClipsPerCollection → ErrLimitReached.
//
// The position is MAX(position)+1 inside the same transaction, so two
// concurrent appends cannot collide (the rw pool is a single connection under
// BEGIN IMMEDIATE) and the sequence stays dense.
func (d *DB) AddClipToCollection(ctx context.Context, collectionID, ownerID, clipID string, now int64) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		if err := collectionOfOwnerTx(ctx, tx, collectionID, ownerID); err != nil {
			return err
		}
		if err := ownedClipTx(ctx, tx, clipID, ownerID); err != nil {
			return err
		}
		var member int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM collection_clips WHERE collection_id = ? AND clip_id = ?",
			collectionID, clipID).Scan(&member); err != nil {
			return err
		}
		if member > 0 {
			return nil
		}
		var n int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM collection_clips WHERE collection_id = ?", collectionID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxClipsPerCollection {
			return ErrLimitReached
		}
		var next int64
		if err := tx.QueryRowContext(ctx,
			"SELECT COALESCE(MAX(position), -1) + 1 FROM collection_clips WHERE collection_id = ?",
			collectionID).Scan(&next); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO collection_clips (collection_id, clip_id, position) VALUES (?, ?, ?)",
			collectionID, clipID, next); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE collections SET updated_at = ? WHERE id = ?", now, collectionID)
		return err
	})
}
