package db

import (
	"context"
	"database/sql"
)

// The trim scrubber's filmstrip, database half (§20 erratum 220; ROADMAP #6's
// open half).
//
// It is the gif's shape, statement for statement, because it is the gif's
// problem: a derived artefact of the CURRENT rendition that nobody has asked
// for until somebody opens the control, produced by one bounded ffmpeg pass,
// and worthless the moment the rendition under it changes.
//
//   - EnqueueFilmstrip admits at most ONE live filmstrip job per clip, so the
//     route may enqueue on demand without its caller being able to pile work
//     onto the queue: every request past the first finds the job already there.
//     (Since §20 erratum 227 that caller is the clip's owner; the guard was
//     written when it was anybody, and is unchanged.)
//   - SetFilmstrip writes the seven filmstrip_* columns of a clip that is
//     STILL ready and STILL on the rendition the job read at its start. That
//     second predicate is the whole honesty of a filmstrip: a trim that lands
//     mid-run replaces the very frames the sheet was cut from, so its tiles
//     would sit under handles naming moments the sheet does not show.
//
// Neither ever writes clips.state, clips.failure_reason or either failure
// counter: a filmstrip is a derived extra on a clip that is already ready and
// already playable, and no failure of it may cost a clip its readiness.

// FilmstripMaxAttempts is the CRASH-class cap of a filmstrip job, for
// GIFMaxAttempts' reason exactly: TRANSCODE_MAX_ATTEMPTS is a knob about the
// encode a clip cannot exist without, and a scrubber's thumbnail sheet is an
// extra on a clip that already plays. Two — one retry — is the budget, and the
// fallback is better than a third machine attempt: the scrubber keeps its
// plain track and the next owner who opens the control asks again.
//
// It does NOT bound the ENVIRONMENT counter (§20 erratum 89): a storage blip
// is not an argument about this file, so a filmstrip gets the instance's
// configured TRANSCODE_MAX_TRANSIENT budget like everything else.
const FilmstripMaxAttempts = 2

// FilmstripFields is everything SetFilmstrip writes onto the clip. Every
// number is MEASURED from the produced JPEG (Tiles is derived from its width),
// never planned: the client positions tiles from these, and a plan the encoder
// did not follow would put the wrong frame under a handle.
type FilmstripFields struct {
	Key          string
	Size         int64
	ETag         string
	Tiles        int
	TileW, TileH int
	IntervalMS   int64
}

// FilmstripSwap reports what SetFilmstrip did. Reason is set only when OK is
// false and is the operator-facing sentence for the job's `skipped` note — the
// run that produced the sheet is the only caller, and what it needs to know is
// whether anything now points at its output (OK) or nothing does (Reason).
type FilmstripSwap struct {
	OK         bool
	Superseded []string
	Reason     string
}

// EnqueueFilmstrip inserts the `filmstrip` job for one clip and returns it.
//
// The guard is the whole rate story of the route (which since §20 erratum 227
// enqueues for the clip's OWNER only): the clip must be `ready`
// (ErrStateMismatch otherwise) and must have no queued or running filmstrip job
// (ErrConflict). Both checks and the INSERT are ONE transaction on the
// single-writer `rw` pool, so a hundred simultaneous first requests for the same
// clip produce exactly one job. A missing clip is ErrNotFound.
func (d *DB) EnqueueFilmstrip(ctx context.Context, clipID string, now int64) (Job, error) {
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
			clipID, KindFilmstrip).Scan(&live); err != nil {
			return err
		}
		if live > 0 {
			return ErrConflict
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO jobs (clip_id, kind, state, stage, run_after, created_at, updated_at)
VALUES (?, ?, 'queued', ?, ?, ?, ?)`, clipID, KindFilmstrip, JobStageQueued, now, now, now)
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

// SetFilmstrip points a READY clip at a freshly produced filmstrip and marks
// the job done — one UPDATE, one transaction, and the clip is `ready` before
// it, during it and after it.
//
// playETag is the PLAYABLE FILE the job cut the sheet from, read when it
// started — `db.PlayObject`'s etag since the quality tiers, which is the
// rendition's when there is one and the UPLOAD's on the remux fast path (§20
// erratum 201), asked here through `playETagSQL` exactly as SetGIF, SetSocial
// and SetPoster ask it. It is part of the UPDATE's predicate, so a trim that
// committed while this job ran makes the swap a no-op with Reason "the rendition
// changed": the caller then deletes its own object. Nothing downstream could
// ever tell — a filmstrip of a six-second clip trimmed to two is a perfectly
// valid sheet of a clip nobody has any more, and its tiles would land under
// the wrong handles.
//
// It returns the key the clip stopped referencing (a previous filmstrip, when
// one was remade) so the caller can delete it afterwards, and refuses a
// `transcode` job outright, keeping the §5 coupling rule where it belongs.
func (d *DB) SetFilmstrip(ctx context.Context, jobID int64, clipID string, f FilmstripFields, playETag string, now int64) (FilmstripSwap, error) {
	var out FilmstripSwap
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
UPDATE clips SET filmstrip_key = ?, filmstrip_size = ?, filmstrip_etag = ?,
  filmstrip_tiles = ?, filmstrip_tile_w = ?, filmstrip_tile_h = ?, filmstrip_interval_ms = ?, updated_at = ?
WHERE id = ? AND state = 'ready' AND `+playETagSQL+` = ?`,
			f.Key, f.Size, f.ETag, f.Tiles, f.TileW, f.TileH, f.IntervalMS, now, clipID, playETag))
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
		if c.FilmstripKey != nil && *c.FilmstripKey != "" && *c.FilmstripKey != f.Key {
			out.Superseded = append(out.Superseded, *c.FilmstripKey)
		}
		out.OK = true
		return nil
	})
	if err != nil {
		return FilmstripSwap{}, err
	}
	return out, nil
}
