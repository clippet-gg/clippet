package db

import (
	"context"
	"database/sql"
)

// GIF export, database half (DESIGN.md §17 #6, §5's Phase 4 job-kind rule).
//
// Two statements again, and both are guarded transactions rather than plain
// writes — the same shape the trim uses, for the same reasons:
//
//   - EnqueueGIF admits at most ONE live gif job per clip, so the public
//     route can enqueue on demand without an anonymous caller being able to
//     pile work onto the queue: every request past the first finds the job
//     already there;
//   - SetGIF writes the five gif_* columns of a clip that is STILL ready and
//     STILL on the rendition the job read at its start. That second predicate
//     is what makes a GIF honest: a trim that lands mid-run replaces the
//     rendition the preview was cut from, so the preview describes a clip
//     that no longer exists and must be thrown away rather than published.
//
// Neither ever writes clips.state, clips.failure_reason or either failure
// counter: a GIF is a derived extra, and no failure of it may cost a clip
// its readiness.

// GIFMaxAttempts is the CRASH-class cap of a gif job, for the reason
// NotifyMaxAttempts and TrimMaxAttempts are constants too: TRANSCODE_MAX_
// ATTEMPTS is a knob about the encode a clip cannot exist without, and a
// preview GIF is an extra on a clip that is already ready and already
// playable. Two — one retry — is the budget, and the fallback is better than
// a third machine attempt: the route reports the failure and the owner asks
// again.
//
// It does NOT bound the ENVIRONMENT counter (§20 erratum 89): a storage blip
// is not an argument about this file, and here the terminal ending is worse
// than terminal — previewGIF answers every anonymous visitor 503 gif_failed
// and never retries by design — so a gif gets the instance's configured
// TRANSCODE_MAX_TRANSIENT budget like everything else.
const GIFMaxAttempts = 2

// GIFFields is everything SetGIF writes onto the clip. Width/Height are
// MEASURED from the produced GIF, never planned.
type GIFFields struct {
	Key           string
	Size          int64
	ETag          string
	Width, Height int
}

// GIFSwap reports what SetGIF did. Reason is set only when OK is false and
// is the operator-facing sentence for the job's `skipped` note — the run
// that produced the GIF is the only caller, and what it needs to know is
// whether anything is now pointing at its output (OK) or nothing is (Reason).
type GIFSwap struct {
	OK         bool
	Superseded []string
	Reason     string
}

// EnqueueGIF inserts the `gif` job for one clip and returns it.
//
// The guard is the point, and it is the whole rate story of the public route:
// the clip must be `ready` (ErrStateMismatch otherwise) and it must have no
// queued or running gif job (ErrConflict). Both checks and the INSERT are ONE
// transaction on the single-writer `rw` pool, so a hundred simultaneous first
// requests for the same clip produce exactly one job. A missing clip is
// ErrNotFound.
func (d *DB) EnqueueGIF(ctx context.Context, clipID string, now int64) (Job, error) {
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
		var live int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM jobs WHERE clip_id = ? AND kind = ? AND state IN ('queued','running')",
			clipID, KindGIF).Scan(&live); err != nil {
			return err
		}
		if live > 0 {
			return ErrConflict
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO jobs (clip_id, kind, state, stage, run_after, created_at, updated_at)
VALUES (?, ?, 'queued', ?, ?, ?, ?)`, clipID, KindGIF, JobStageQueued, now, now, now)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		job, err = getJob(ctx, tx, id)
		return err
	})
	if err != nil {
		return Job{}, err
	}
	return job, nil
}

// SetGIF points a READY clip at a freshly produced preview GIF and marks the
// job done — one UPDATE, one transaction, and the clip is `ready` before it,
// during it and after it.
//
// playETag is the PLAY OBJECT the job cut the GIF from, read when it started —
// `db.PlayObject`'s etag since the quality tiers, which is the rendition's
// whenever there is one and the UPLOAD's on the remux fast path (§20 erratum
// 201). It is part of the UPDATE's predicate, so a trim that committed
// while this job ran makes the swap a no-op with Reason "the rendition
// changed": the caller then deletes its own output. Nothing else can tell —
// the GIF of a six-second clip trimmed to two is a perfectly valid GIF of a
// clip nobody has any more.
//
// It returns the key the clip stopped referencing (a previous GIF, when the
// owner asked for a new one) so the caller can delete it afterwards, and
// refuses a `transcode` job outright, keeping the §5 coupling rule where it
// belongs.
func (d *DB) SetGIF(ctx context.Context, jobID int64, clipID string, f GIFFields, playETag string, now int64) (GIFSwap, error) {
	var out GIFSwap
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
UPDATE clips SET gif_key = ?, gif_size = ?, gif_etag = ?, gif_width = ?, gif_height = ?, updated_at = ?
WHERE id = ? AND state = 'ready' AND `+playETagSQL+` = ?`,
			f.Key, f.Size, f.ETag, f.Width, f.Height, now, clipID, playETag))
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
		if c.GIFKey != nil && *c.GIFKey != "" && *c.GIFKey != f.Key {
			out.Superseded = append(out.Superseded, *c.GIFKey)
		}
		out.OK = true
		return nil
	})
	if err != nil {
		return GIFSwap{}, err
	}
	return out, nil
}
