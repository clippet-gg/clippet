package db

import (
	"context"
	"database/sql"
)

// The dedicated social rendition, database half (DESIGN.md §17 #3, §5's
// Phase 4 job-kind rule, §20 errata 121–124).
//
// The shape is the GIF's, deliberately, because the two jobs have the same
// standing: both derive an extra file from a clip that is already ready and
// already playable, and neither may cost that clip anything if it fails.
//
//   - EnqueueSocialTx admits at most ONE live social job per clip, and it is
//     called from INSIDE the transaction that made the clip ready (or that
//     swapped its rendition), so the file the card will want is queued in the
//     same commit that created the thing it describes.
//   - SetSocial writes the five social_* columns of a clip that is STILL
//     ready and STILL on the rendition the job read at its start. That second
//     predicate is what keeps a social file honest: a trim landing mid-run
//     replaces the very frames it was encoded from, and a card pointing at
//     the old ones is worse than a card pointing at the rendition.
//
// Neither ever writes clips.state, clips.failure_reason or either failure
// counter.

// SocialMaxAttempts is the CRASH-class cap of a social job, a constant for
// the reason NotifyMaxAttempts, TrimMaxAttempts and GIFMaxAttempts are:
// TRANSCODE_MAX_ATTEMPTS is a knob about the encode a clip cannot exist
// without, and a social rendition is an extra on a clip that is already
// ready. Two — one retry — is the budget, and the fallback is not a failure
// anybody sees: `/social.mp4` keeps serving the main rendition, which is
// exactly what it served before this feature existed.
//
// It does NOT bound the ENVIRONMENT counter (§20 erratum 89): a storage blip
// is not an argument about this file, so a social job gets the instance's
// configured TRANSCODE_MAX_TRANSIENT like every other kind.
const SocialMaxAttempts = 2

// SocialFields is everything SetSocial writes onto the clip. Width/Height are
// MEASURED from the produced file, never planned — they become
// `og:video:width/height` and the `twitter:player` dims (§20 erratum 123).
// FPSNum/FPSDen are measured too (migration 0012): the quality selector labels
// this tier "Light 720p30", and a label stating a rate nobody measured is the
// bug erratum 123 closed for the dimensions.
type SocialFields struct {
	Key            string
	Size           int64
	ETag           string
	Width, Height  int
	FPSNum, FPSDen int
}

// SocialSwap reports what SetSocial did. Reason is set only when OK is false
// and is the operator-facing sentence for the job's `skipped` note.
type SocialSwap struct {
	OK         bool
	Superseded []string
	Reason     string
}

// EnqueueSocial inserts the `social` job for one clip and returns it. It is
// the exported form of enqueueSocialTx, used by tests and by any caller that
// is not already inside a transaction; the two commit points that matter
// (FinalizeTranscode, SwapRendition) use the Tx form so the ready flip and
// the intent to encode a card file are ONE commit.
//
// The clip must be `ready` (ErrStateMismatch otherwise) and must have no
// queued or running social job (ErrConflict). A missing clip is ErrNotFound.
func (d *DB) EnqueueSocial(ctx context.Context, clipID string, now int64) (Job, error) {
	var job Job
	err := d.tx(ctx, func(tx *sql.Tx) error {
		var state string
		if err := tx.QueryRowContext(ctx, "SELECT state FROM clips WHERE id = ?", clipID).Scan(&state); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		if state != ClipReady {
			return ErrStateMismatch
		}
		id, queued, err := enqueueSocialTx(ctx, tx, clipID, now)
		if err != nil {
			return err
		}
		if !queued {
			return ErrConflict
		}
		job, err = getJob(ctx, tx, id)
		return err
	})
	if err != nil {
		return Job{}, err
	}
	return job, nil
}

// enqueueSocialTx queues the §17 #3 encode for a clip whose rendition has
// just been written, INSIDE that same transaction.
//
// queued=false means one was already live — the at-most-one guard, the whole
// rate story of a kind that two different commit points can ask for. It is
// not an error at either call site: FinalizeTranscode cannot reach it (a clip
// becoming ready has no live media job), and SwapRendition legitimately can
// (an owner trimming twice in a minute), where the right answer is that the
// job already queued will read the rendition that exists when it runs. A
// RUNNING job is not that answer — it read the old rendition — so the two
// swaps call supersedeRunningTx first (§20 erratum 237 (a)) and only a queued
// job is ever left standing here.
//
// Unlike enqueueNotifyTx there is NO unlisted rule: `unlisted` means "do not
// surface this" and a social rendition surfaces nothing — it is the file
// behind a card the holder of the link asked for, and an unlisted clip's card
// is exactly as playable as any other (§9.1 renders them identically).
func enqueueSocialTx(ctx context.Context, tx *sql.Tx, clipID string, now int64) (id int64, queued bool, err error) {
	var live int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM jobs WHERE clip_id = ? AND kind = ? AND state IN ('queued','running')",
		clipID, KindSocial).Scan(&live); err != nil {
		return 0, false, err
	}
	if live > 0 {
		return 0, false, nil
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO jobs (clip_id, kind, state, stage, run_after, created_at, updated_at)
VALUES (?, ?, 'queued', ?, ?, ?, ?)`, clipID, KindSocial, JobStageQueued, now, now, now)
	if err != nil {
		return 0, false, err
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// supersedeRunningTx ends a RUNNING tier job of kind for the clip, INSIDE the
// commit that has just published the file that job was not encoding (§20
// erratum 237 (a)). It is called by SwapRendition and SwapSource immediately
// before they queue a fresh job of the same kind, and only when they do.
//
// A running `social` or `full` job read its source when it started, so after
// a trim or a replacement it is encoding a file the clip no longer plays: its
// guarded commit can only lose, and before this the at-most-one guard in
// enqueueSocialTx / enqueueFullTx read it as the job that would serve the new
// rendition and queued nothing — the new rendition kept no card copy (or no
// Full) of its own. A QUEUED job is different and is kept: it has not read
// anything yet, and it will read what exists when it runs.
//
// The row goes to `cancelled`, the hand MarkDeleting already uses on a live
// job, with the reason in last_error. The worker that holds it is not told;
// every one of its endings then meets the row as "not this attempt's" (§20
// erratum 293): its commit is refused (the job is no longer running), its
// CompleteJob / FailAttempt / ReleaseJob answer ErrStateMismatch, and its
// tier footnote finds the fresh job live and writes nothing.
func supersedeRunningTx(ctx context.Context, tx *sql.Tx, clipID, kind string, now int64) error {
	_, err := tx.ExecContext(ctx, `
UPDATE jobs SET state = 'cancelled', last_error = ?, finished_at = ?, updated_at = ?
WHERE clip_id = ? AND kind = ? AND state = 'running'`,
		"superseded: a newer file was published while this job ran", now, now, clipID, kind)
	return err
}

// SetSocial points a READY clip at a freshly produced social rendition and
// marks the job done — one UPDATE, one transaction, and the clip is `ready`
// before it, during it and after it.
//
// playETag is the PLAYABLE FILE the job encoded from, read when it
// started — `db.PlayObject`'s etag since the quality tiers, which is the
// rendition whenever there is one and the upload itself on the remux fast path
// (§20 erratum 201). It is part of the UPDATE's predicate, so a trim or a
// replacement that committed while this job ran makes the swap a no-op with
// Reason "the rendition changed": the caller deletes its own output, and that
// commit has already queued the replacement.
//
// It returns the key the clip stopped referencing (a previous social file, if
// somehow one existed) so the caller can delete it, and refuses a `transcode`
// job outright, keeping the §5 coupling rule where it belongs.
func (d *DB) SetSocial(ctx context.Context, jobID int64, clipID string, f SocialFields, playETag string, now int64) (SocialSwap, error) {
	var out SocialSwap
	err := d.tx(ctx, func(tx *sql.Tx) error {
		j, err := getJob(ctx, tx, jobID)
		switch {
		case err == ErrNotFound:
			out.Reason = "the job row is gone"
			return nil
		case err != nil:
			return err
		case j.Kind == KindTranscode:
			return ErrStateMismatch
		case j.State != JobRunning:
			out.Reason = "the job is no longer running"
			return nil
		}
		c, err := getClip(ctx, tx, clipID)
		switch {
		case err == ErrNotFound:
			out.Reason = "the clip is gone"
			return nil
		case err != nil:
			return err
		case c.State != ClipReady:
			out.Reason = "the clip is " + c.State
			return nil
		case !playETagMatches(c, playETag):
			out.Reason = "the rendition changed"
			return nil
		}
		n, err := affected(tx.ExecContext(ctx, `
UPDATE clips SET social_key = ?, social_size = ?, social_etag = ?, social_width = ?, social_height = ?,
  social_fps_num = ?, social_fps_den = ?, light_state = 'ready', updated_at = ?
WHERE id = ? AND state = 'ready' AND `+playETagSQL+` = ?`,
			f.Key, f.Size, f.ETag, f.Width, f.Height,
			zeroNull(int64(f.FPSNum)), zeroNull(int64(f.FPSDen)), now, clipID, playETag))
		if err != nil {
			return err
		}
		if n == 0 {
			out.Reason = "the rendition changed"
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE jobs SET state = 'done', stage = 'done', finished_at = ?, updated_at = ? WHERE id = ?",
			now, now, jobID); err != nil {
			return err
		}
		if c.SocialKey != nil && *c.SocialKey != "" && *c.SocialKey != f.Key {
			out.Superseded = append(out.Superseded, *c.SocialKey)
		}
		out.OK = true
		return nil
	})
	if err != nil {
		return SocialSwap{}, err
	}
	return out, nil
}
