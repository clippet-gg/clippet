package db

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"clippet/internal/ids"
)

// NewClip is the ingest payload for CreateClipWithJob. TTLSeconds nil = never.
type NewClip struct {
	ID, OwnerID, Title, Description string
	Unlisted                        bool
	SourceKey, SourceFilename       string
	SourceSize                      int64
	SourceContentType, SourceETag   string
	TTLSeconds                      *int64
	// Tags is the clip's initial tag set (§17 #9). It is written inside the
	// SAME transaction as the row, so a clip is never briefly visible without
	// the tags it was uploaded with. Already-normalised values are expected;
	// CreateClipWithJob re-cleans them anyway.
	Tags []string
	Now  int64
	// Kind and Audio describe what the ingest probe found (migration 0021, §20
	// erratum 328): an empty Kind is a video, and Audio is read only for an
	// audio clip.
	Kind  string
	Audio AudioFacts
}

// AudioFacts are an audio clip's stream facts as the strip stored them: zero
// fields are written as NULL.
type AudioFacts struct {
	Codec                          string
	SampleRate, BitDepth, Channels int
}

// CreateClipWithJob inserts the clip (state queued, expires_at = Now+TTL) and
// its transcode job (stage "queued") in one transaction. A duplicate id →
// ErrConflict; any other constraint failure (e.g. an unknown owner) is a
// plain DB error so httpapi answers 503, not id_collision.
func (d *DB) CreateClipWithJob(ctx context.Context, c NewClip) (Clip, Job, error) {
	var expires *int64
	if c.TTLSeconds != nil {
		e := c.Now + *c.TTLSeconds
		expires = &e
	}
	tags, err := CleanTags(c.Tags)
	if err != nil {
		return Clip{}, Job{}, err
	}
	kind, audio := ClipKindVideo, AudioFacts{}
	if c.Kind == ClipKindAudio {
		kind, audio = ClipKindAudio, c.Audio
	}
	var clip Clip
	var job Job
	err = d.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO clips (id, owner_id, title, description, unlisted, state,
  source_key, source_filename, source_size, source_content_type, source_etag,
  ttl_seconds, expires_at, created_at, updated_at,
  kind, audio_codec, sample_rate, bit_depth, channels)
VALUES (?, ?, ?, ?, ?, 'queued', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.ID, c.OwnerID, c.Title, c.Description, boolInt(c.Unlisted),
			c.SourceKey, c.SourceFilename, c.SourceSize, c.SourceContentType, c.SourceETag,
			nullInt64(c.TTLSeconds), nullInt64(expires), c.Now, c.Now,
			kind, emptyNull(audio.Codec), zeroNull(int64(audio.SampleRate)),
			zeroNull(int64(audio.BitDepth)), zeroNull(int64(audio.Channels)))
		if err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO jobs (clip_id, kind, state, stage, run_after, created_at, updated_at)
VALUES (?, 'transcode', 'queued', ?, ?, ?, ?)`, c.ID, JobStageQueued, c.Now, c.Now, c.Now)
		if err != nil {
			return err
		}
		jobID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		for _, tag := range tags {
			if _, err := tx.ExecContext(ctx, "INSERT INTO clip_tags (clip_id, tag) VALUES (?, ?)", c.ID, tag); err != nil {
				return err
			}
		}
		if clip, err = getClip(ctx, tx, c.ID); err != nil {
			return err
		}
		job, err = getJob(ctx, tx, jobID)
		return err
	})
	if err != nil {
		return Clip{}, Job{}, err
	}
	return clip, job, nil
}

func getClip(ctx context.Context, q querier, id string) (Clip, error) {
	return scanClip(q.QueryRowContext(ctx, "SELECT "+clipCols+" FROM clips WHERE id = ?", id))
}

// ClipExists reports whether a row with the id exists (any state).
func (d *DB) ClipExists(ctx context.Context, id string) (bool, error) {
	var n int
	if err := d.ro.QueryRowContext(ctx, "SELECT COUNT(*) FROM clips WHERE id = ?", id).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// GetClip returns the clip in any state (including deleting — callers check
// State), or ErrNotFound.
func (d *DB) GetClip(ctx context.Context, id string) (Clip, error) {
	return getClip(ctx, d.ro, id)
}

// ListQuery parameterises ListClips. Limit is clamped to 1..100 (default 50).
//
// Tag, when set, narrows the SAME visibility set (every listed clip plus the
// viewer's own unlisted ones) to the clips carrying that tag — it never widens
// it, so a tag can no more reveal somebody else's unlisted clip than an empty
// filter can. It must already be normalised (db.CleanTag); ListClips does not
// re-clean it, because the caller has to reject an invalid tag with a 400
// naming it long before it reaches SQL.
//
// Q and Mine are the other two narrowing filters (§20 errata 114–115) and
// obey the same rule. Q must already be normalised (db.CleanSearch); Mine
// restricts the list to the VIEWER's own clips and can never name another
// user, because there is no user page and no social layer to have one for.
//
// ScopeOwn is LIBRARY_SCOPE=own (§20 erratum 113): the same predicate Mine
// applies, decided by the instance's configuration instead of by the request,
// and therefore NOT part of the cursor's filter binding.
type ListQuery struct {
	ViewerID string
	Limit    int
	Cursor   string
	Tag      string
	Q        string
	Mine     bool
	ScopeOwn bool
}

// filter is the part of the query a cursor is minted under.
func (q ListQuery) filter() listFilter {
	return listFilter{Tag: q.Tag, Q: q.Q, Mine: q.Mine}
}

const (
	listDefaultLimit = 50
	listMaxLimit     = 100
)

// ListClips returns every listed clip plus the viewer's own unlisted ones,
// newest first (created_at DESC, id ASC as tie-break), excluding deleting
// rows. nextCursor is "" on the last page. An undecodable cursor returns
// ErrInvalidCursor.
//
// The tie-break is the ID, and the id is random, so "newest first" is not
// INSERTION order for rows created inside the same second — created_at is
// stored in whole seconds. That is a known limitation, not an oversight: the
// order is total and stable, which is what the cursor's
// `created_at < ? OR (created_at = ? AND id > ?)` predicate needs to page
// without skipping or repeating a row. Making it insertion order costs
// either a millisecond column (a migration that changes the unit every
// timestamp consumer reads) or a rowid in the cursor (a page-token format
// change), and neither is worth it for a sub-second display order nobody can
// observe except a test uploading in a loop — E2E step 23 sleeps 1.2 s
// between the two clips it orders.
func (d *DB) ListClips(ctx context.Context, q ListQuery) ([]Clip, string, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = listDefaultLimit
	}
	if limit > listMaxLimit {
		limit = listMaxLimit
	}
	var (
		hasCursor int64
		curAt     int64
		curID     string
	)
	filter := q.filter()
	if q.Cursor != "" {
		at, id, got, err := decodeCursor(q.Cursor)
		if err != nil {
			return nil, "", err
		}
		// The cursor carries the filter it was minted under, and a cursor
		// from a different filter is not a cursor for THIS list: continuing
		// with it would page through positions that mean nothing here and
		// silently skip rows. Answering ErrInvalidCursor (→ 400) is the only
		// honest reading (§20 errata 81, 115).
		if got != filter {
			return nil, "", ErrInvalidCursor
		}
		hasCursor, curAt, curID = 1, at, id
	}
	var args []any
	if q.ViewerID != "" {
		args = append(args, q.ViewerID)
	}
	if filter.Tag != "" {
		args = append(args, filter.Tag)
	}
	if filter.Mine || q.ScopeOwn {
		args = append(args, q.ViewerID)
	}
	if filter.Q != "" {
		pattern := searchPattern(filter.Q)
		args = append(args, pattern, pattern)
	}
	args = append(args, hasCursor, curAt, curAt, curID, limit+1)
	rows, err := d.ro.QueryContext(ctx, listClipsSQL(q.ViewerID != "", filter, q.ScopeOwn), args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []Clip
	for rows.Next() {
		c, err := scanClip(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[limit-1]
		next = encodeCursor(last.CreatedAt, last.ID, filter)
	}
	return out, next, nil
}

// listClipsSQL builds ListClips' statement. It exists as a function rather
// than a string literal inside ListClips so a test can hand the EXACT query
// the list route runs to EXPLAIN QUERY PLAN; the placeholders are, in order,
// the viewer id (only when there IS one), the tag (only when filtered by
// one), the owner id (only under `?mine=1` or LIBRARY_SCOPE=own), the search
// pattern TWICE (only when searching), then the four cursor parameters and
// the limit.
//
// Every filter is appended INSIDE the visibility clause, so none of them can
// widen the set: a tag, a word in a title or "mine only" can no more reveal
// somebody else's unlisted clip than an empty filter can (§20 errata 81,
// 113-115).
//
// The anonymous shape drops the disjunction entirely (§20 erratum 100).
// Migration 0001 creates `clips_listed ON clips(created_at DESC, id) WHERE
// unlisted = 0` for exactly this query, and `(unlisted = 0 OR owner_id = ?)`
// is a predicate no partial index can drive: EXPLAIN QUERY PLAN over it
// reads `SCAN clips` + `USE TEMP B-TREE FOR ORDER BY`, i.e. a full table
// scan and a sort of the whole table for every anonymous page — which is
// the HOT case, since it is what every crawler, every unfurl fetch and
// every logged-out library page runs. With no viewer there is nothing for
// the disjunction to admit (`owner_id` is a user id and is never ""), so
// emitting `AND unlisted = 0` alone is the same result set reached through
// the index, in index order, with no sort. `TestListClipsAnonymousUsesTheListedIndex`
// pins the plan the way erratum 84 pinned the tag plan; the signed-in shape
// is unchanged and still scans. An anonymous caller that somehow carries an
// owner or "mine" filter still binds "" for the owner id and so still
// selects nothing, exactly as the disjunction spelling did.
func listClipsSQL(hasViewer bool, f listFilter, scopeOwn bool) string {
	where := "WHERE state <> 'deleting' AND unlisted = 0"
	if hasViewer {
		where = "WHERE state <> 'deleting' AND (unlisted = 0 OR owner_id = ?)"
	}
	if f.Tag != "" {
		where += tagFilterSQL
	}
	if f.Mine || scopeOwn {
		where += ownerFilterSQL
	}
	if f.Q != "" {
		where += searchSQL
	}
	return `
SELECT ` + clipCols + ` FROM clips
` + where + `
  AND (? = 0 OR created_at < ? OR (created_at = ? AND id > ?))
ORDER BY created_at DESC, id ASC
LIMIT ?`
}

// encodeCursor mints the opaque page token. It carries the filter as a third
// field so a page of a FILTERED list can only be continued as that same
// filtered list. The shapes, in the order they were introduced:
//
//	"<at>:<id>"              — no filter (Phase 1; still minted, still decoded)
//	"<at>:<id>:<tag>"        — a tag alone (§17 #9; still minted, still decoded)
//	"<at>:<id>:mine=1&q=…&t=…" — any other combination (§20 erratum 115)
//
// The first two are kept EXACTLY as they were rather than folded into the
// third: erratum 81's "tokens minted before this feature still decode" is a
// standing promise, and a tag-only list is the common filtered case, so it
// keeps the short token it has always had. A bare tag can never be mistaken
// for the key=value form — the tag grammar has no '=' in it.
func encodeCursor(createdAt int64, id string, f listFilter) string {
	raw := strconv.FormatInt(createdAt, 10) + ":" + id
	switch {
	case f.tagOnly():
		raw += ":" + f.Tag
	case !f.empty():
		raw += ":" + f.encode()
	}
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (int64, string, listFilter, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, "", listFilter{}, ErrInvalidCursor
	}
	at, rest, ok := strings.Cut(string(raw), ":")
	if !ok {
		return 0, "", listFilter{}, ErrInvalidCursor
	}
	// The filter field is everything after the SECOND colon, so a value that
	// contains one cannot truncate it — url.Values.Encode percent-escapes ':'
	// anyway, which is the belt to this braces.
	id, filterField, hasFilter := strings.Cut(rest, ":")
	if !ids.Valid(id) {
		return 0, "", listFilter{}, ErrInvalidCursor
	}
	var f listFilter
	if hasFilter {
		if f, err = decodeFilter(filterField); err != nil {
			return 0, "", listFilter{}, err
		}
	}
	n, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		return 0, "", listFilter{}, ErrInvalidCursor
	}
	return n, id, f, nil
}

// ClipPatch is the PATCH payload. nil = unchanged. TTLSeconds is a double
// pointer: nil = unchanged; pointer to nil = never; pointer to value = that
// many seconds, with expires_at recomputed from now.
type ClipPatch struct {
	Title, Description *string
	Unlisted           *bool
	TTLSeconds         **int64
	// Tags replaces the WHOLE set when non-nil (§17 #9): a pointer to an
	// empty slice clears it, nil leaves it alone. There is no add-one or
	// remove-one verb, because the editor that drives this is a text field
	// holding the whole list and a partial verb would need a merge rule
	// nobody could see in the box they are typing into.
	Tags *[]string
}

// UpdateClipMeta applies the patch and returns the updated row. A missing or
// deleting clip → ErrNotFound.
func (d *DB) UpdateClipMeta(ctx context.Context, id string, p ClipPatch, now int64) (Clip, error) {
	sets := []string{"updated_at = ?"}
	args := []any{now}
	if p.Title != nil {
		sets = append(sets, "title = ?")
		args = append(args, *p.Title)
	}
	if p.Description != nil {
		sets = append(sets, "description = ?")
		args = append(args, *p.Description)
	}
	if p.Unlisted != nil {
		sets = append(sets, "unlisted = ?")
		args = append(args, boolInt(*p.Unlisted))
	}
	if p.TTLSeconds != nil {
		if *p.TTLSeconds == nil {
			sets = append(sets, "ttl_seconds = NULL", "expires_at = NULL")
		} else {
			ttl := **p.TTLSeconds
			sets = append(sets, "ttl_seconds = ?", "expires_at = ?")
			args = append(args, ttl, now+ttl)
		}
	}
	args = append(args, id)
	var tags []string
	if p.Tags != nil {
		var err error
		if tags, err = CleanTags(*p.Tags); err != nil {
			return Clip{}, err
		}
	}
	var clip Clip
	err := d.tx(ctx, func(tx *sql.Tx) error {
		n, err := affected(tx.ExecContext(ctx,
			"UPDATE clips SET "+strings.Join(sets, ", ")+" WHERE id = ? AND state <> 'deleting'", args...))
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		if p.Tags != nil {
			if _, err := tx.ExecContext(ctx, "DELETE FROM clip_tags WHERE clip_id = ?", id); err != nil {
				return err
			}
			for _, tag := range tags {
				if _, err := tx.ExecContext(ctx, "INSERT INTO clip_tags (clip_id, tag) VALUES (?, ?)", id, tag); err != nil {
					return err
				}
			}
		}
		clip, err = getClip(ctx, tx, id)
		return err
	})
	if err != nil {
		return Clip{}, err
	}
	return clip, nil
}

// TouchClip is the §8.1 keep-alive: ONE update restarting the clip's TTL
// from now (expires_at = now + ttl_seconds), in every state but deleting —
// a queued or processing clip may be touched, and a clip already past its
// expiry is rescued from the sweeper (erratum 15 re-verifies expiry inside
// the tombstone write, so a touch that lands before DestroyExpired wins).
//
// A clip kept forever (ttl_seconds NULL) has no timer to reset: the UPDATE's
// own predicate excludes it, so expires_at stays NULL and updated_at is not
// stamped — a no-op, with the current row returned exactly as for a real
// touch. A missing or deleting clip → ErrNotFound.
func (d *DB) TouchClip(ctx context.Context, id string, now int64) (Clip, error) {
	var clip Clip
	err := d.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
UPDATE clips SET expires_at = ? + ttl_seconds, updated_at = ?
WHERE id = ? AND state <> 'deleting' AND ttl_seconds IS NOT NULL`, now, now, id); err != nil {
			return err
		}
		c, err := getClip(ctx, tx, id)
		if err != nil {
			return err
		}
		if c.State == ClipDeleting {
			return ErrNotFound
		}
		clip = c
		return nil
	})
	if err != nil {
		return Clip{}, err
	}
	return clip, nil
}

// IncrementViews adds one to the persisted counter. A missing or deleting
// clip → ErrNotFound (a view never lands on a tombstone).
func (d *DB) IncrementViews(ctx context.Context, id string) error {
	n, err := d.exec(ctx, "UPDATE clips SET views = views + 1 WHERE id = ? AND state <> 'deleting'", id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkDeleting sets the tombstone and cancels every queued/running job of the
// clip in one transaction. Idempotent on an already-deleting row; a missing
// row → ErrNotFound (already gone).
func (d *DB) MarkDeleting(ctx context.Context, id string, now int64) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		n, err := affected(tx.ExecContext(ctx,
			"UPDATE clips SET state = 'deleting', updated_at = ? WHERE id = ?", now, id))
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		_, err = tx.ExecContext(ctx, `
UPDATE jobs SET state = 'cancelled', finished_at = ?, updated_at = ?
WHERE clip_id = ? AND state IN ('queued', 'running')`, now, now, id)
		return err
	})
}

// MarkDeletingIfDue is the sweeper's conditional tombstone: MarkDeleting
// applied only while the clip is still a §12 sweep candidate under the same
// (now, grace) the candidate list used — already deleting, or still past
// expiry. A clip whose TTL was extended (or cleared) after candidate
// selection returns ErrStateMismatch with nothing changed, so an
// acknowledged PATCH is never inverted by a stale candidate list. Missing →
// ErrNotFound.
func (d *DB) MarkDeletingIfDue(ctx context.Context, id string, now, grace int64) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		n, err := affected(tx.ExecContext(ctx, `
UPDATE clips SET state = 'deleting', updated_at = ?
WHERE id = ? AND (state = 'deleting'
   OR (expires_at IS NOT NULL AND (
        (state IN ('queued','ready','failed') AND expires_at <= ?)
     OR (expires_at <= ? - ?))))`, now, id, now, now, grace))
		if err != nil {
			return err
		}
		if n == 0 {
			var exists int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM clips WHERE id = ?", id).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				return ErrNotFound
			}
			return ErrStateMismatch
		}
		_, err = tx.ExecContext(ctx, `
UPDATE jobs SET state = 'cancelled', finished_at = ?, updated_at = ?
WHERE clip_id = ? AND state IN ('queued', 'running')`, now, now, id)
		return err
	})
}

// DeleteClipRow removes a tombstoned row (jobs cascade). Missing → ErrNotFound;
// present but not deleting → ErrStateMismatch.
func (d *DB) DeleteClipRow(ctx context.Context, id string) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		n, err := affected(tx.ExecContext(ctx, "DELETE FROM clips WHERE id = ? AND state = 'deleting'", id))
		if err != nil {
			return err
		}
		if n == 1 {
			return nil
		}
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM clips WHERE id = ?", id).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrNotFound
		}
		return ErrStateMismatch
	})
}

// sweepCandidatesSQL is the statement SweepCandidates runs, as a constant so a
// test can hand the EXACT query to EXPLAIN QUERY PLAN (the way erratum 84 and
// erratum 100 pinned the list route's plan). Placeholders, in order: now, now,
// grace, limit.
//
// BOTH arms of the OR reach an index (AUDIT #29). The expiry arm is written
// `expires_at <= ? AND (state IN (…) OR expires_at <= ? - ?)`, so `expires_at
// <= ?` is a TOP-LEVEL term of that disjunct and drives `clips_expires`; the
// tombstone arm drives `clips_state`. The spelling this replaced put the state
// test at the top level of its own sub-disjunct —
// `(state IN (…) AND expires_at <= ?) OR (expires_at <= ? - ?)` — and the
// planner took `clips_state` for it, walking every `ready` row on the instance
// on every tick to look at a column the index does not carry. The two forms
// select the same rows: `expires_at <= ?` is false for NULL, which is what the
// `expires_at IS NOT NULL` guard it replaces said, and `expires_at <= ? - ?`
// implies `expires_at <= ?` for any grace ≥ 0 (SWEEP_GRACE is a duration and
// cannot be negative). `clips_expires` is partial (`WHERE expires_at IS NOT
// NULL`) and SQLite proves `expires_at <= ?` implies it.
//
// TestSweepCandidatesUsesTheExpiryIndex pins the plan; the rows are pinned by
// TestSweepCandidates and the two order tests, which are unchanged.
const sweepCandidatesSQL = `
SELECT id FROM clips
WHERE state = 'deleting'
   OR (expires_at <= ? AND (state IN ('queued','ready','failed') OR expires_at <= ? - ?))
ORDER BY expires_at IS NULL, expires_at, id LIMIT ?`

// SweepCandidates returns up to limit clip ids to destroy (§12): (a) deleting
// tombstones; (b) queued/ready/failed past expiry; (c) anything (i.e.
// processing) past expiry + grace.
//
// The order is `expires_at IS NULL` LAST, then `expires_at` ascending (§20
// erratum 101). SQLite sorts NULL first, so the plain `ORDER BY expires_at`
// this used to carry put every `deleting` tombstone with no TTL — a manual
// DELETE, which is the one candidate class that can pile up — at position 0
// forever, ahead of the clips whose TTL has actually run out. RunOnce walks
// the list in order and synchronously, so two blackholing prefixes at the
// head could consume a whole tick and starve every expiring clip behind
// them. Putting the TTL-driven work first makes the promise the product
// makes (a clip expires when it said it would) the one the tick keeps when
// it cannot keep both.
func (d *DB) SweepCandidates(ctx context.Context, now, grace int64, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.ro.QueryContext(ctx, sweepCandidatesSQL, now, now, grace, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ForceExpire sets expires_at directly (admin expire, §8.8). updated_at is
// stamped with the wall clock because the signature carries no now.
func (d *DB) ForceExpire(ctx context.Context, id string, at int64) error {
	n, err := d.exec(ctx, "UPDATE clips SET expires_at = ?, updated_at = ? WHERE id = ?", at, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RetryClip moves a failed clip back to queued, clears failure_reason and
// resets its latest transcode job (queued, stage "queued", both counters 0,
// checkpoint NULL, run_after = now). Missing → ErrNotFound; not failed →
// ErrStateMismatch.
func (d *DB) RetryClip(ctx context.Context, id string, now int64) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		n, err := affected(tx.ExecContext(ctx,
			"UPDATE clips SET state = 'queued', failure_reason = NULL, updated_at = ? WHERE id = ? AND state = 'failed'", now, id))
		if err != nil {
			return err
		}
		if n == 0 {
			var exists int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM clips WHERE id = ?", id).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				return ErrNotFound
			}
			return ErrStateMismatch
		}
		n, err = affected(tx.ExecContext(ctx, `
UPDATE jobs SET state = 'queued', stage = ?, attempts = 0, transient_failures = 0, checkpoint_json = NULL,
  last_error = NULL, run_after = ?, updated_at = ?, started_at = NULL, finished_at = NULL
WHERE id = (SELECT id FROM jobs WHERE clip_id = ? AND kind = 'transcode' ORDER BY id DESC LIMIT 1)`,
			JobStageQueued, now, now, id))
		if err != nil {
			return err
		}
		if n == 0 {
			// No transcode job exists (should not happen): create one so the
			// clip does not sit in queued forever.
			_, err = tx.ExecContext(ctx, `
INSERT INTO jobs (clip_id, kind, state, stage, run_after, created_at, updated_at)
VALUES (?, 'transcode', 'queued', ?, ?, ?, ?)`, id, JobStageQueued, now, now, now)
		}
		return err
	})
}

// ClipKeysRow is the cheap projection of a clip's object keys (admin ls,
// admin stats, admin orphans). It carries EVERY key a row can reference, and
// Keys is the single definition of that list — a subcommand that builds its
// own would drift the moment a Phase 4 column starts being filled, which is
// exactly what happened to `orphans` and social_key.
type ClipKeysRow struct {
	ID, State, SourceKey                    string
	RenditionKey, PosterKey, SocialKey, GIF *string
	// Filmstrip is the trim scrubber's sprite sheet (§20 erratum 220). It is
	// listed here for the reason the comment above gives: a Phase 4 column that
	// starts being filled and is not in this struct is an object `orphans`
	// reports as unreferenced while a live row points straight at it.
	Filmstrip *string
}

// Keys lists the object keys this row references: the source always (NOT
// NULL from the insert), then each derived object that has been produced.
func (r ClipKeysRow) Keys() []string {
	keys := []string{r.SourceKey}
	for _, k := range []*string{r.RenditionKey, r.PosterKey, r.SocialKey, r.GIF, r.Filmstrip} {
		if k != nil && *k != "" {
			keys = append(keys, *k)
		}
	}
	return keys
}

// KeysOf projects a full Clip onto the same row, so a caller holding one
// (admin ls) and a caller holding the cheap listing (admin orphans) ask the
// same function for the same list.
func KeysOf(c Clip) ClipKeysRow {
	return ClipKeysRow{ID: c.ID, State: c.State, SourceKey: c.SourceKey,
		RenditionKey: c.RenditionKey, PosterKey: c.PosterKey, SocialKey: c.SocialKey, GIF: c.GIFKey,
		Filmstrip: c.FilmstripKey}
}

// ListAllClipIDsAndKeys returns every clip's id, state and object keys.
func (d *DB) ListAllClipIDsAndKeys(ctx context.Context) ([]ClipKeysRow, error) {
	rows, err := d.ro.QueryContext(ctx,
		"SELECT id, state, source_key, rendition_key, poster_key, social_key, gif_key, filmstrip_key FROM clips ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClipKeysRow
	for rows.Next() {
		var r ClipKeysRow
		var rk, pk, sk, gk, fk sql.NullString
		if err := rows.Scan(&r.ID, &r.State, &r.SourceKey, &rk, &pk, &sk, &gk, &fk); err != nil {
			return nil, err
		}
		r.RenditionKey, r.PosterKey = ptrStr(rk), ptrStr(pk)
		r.SocialKey, r.GIF, r.Filmstrip = ptrStr(sk), ptrStr(gk), ptrStr(fk)
		out = append(out, r)
	}
	return out, rows.Err()
}

// String renders a patch for logs (no user text).
func (p ClipPatch) String() string {
	var parts []string
	if p.Title != nil {
		parts = append(parts, "title")
	}
	if p.Description != nil {
		parts = append(parts, "description")
	}
	if p.Unlisted != nil {
		parts = append(parts, "unlisted")
	}
	if p.TTLSeconds != nil {
		parts = append(parts, "ttl")
	}
	if p.Tags != nil {
		parts = append(parts, "tags")
	}
	return fmt.Sprintf("patch{%s}", strings.Join(parts, ","))
}
