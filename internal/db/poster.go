package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
)

// The owner's chosen card image, database half (ROADMAP #5, §20 errata
// 127-130, §5's job-kind rule).
//
// Two statements again, and both are guarded transactions rather than plain
// writes — the shape the trim and the GIF already use, for the same reasons:
//
//   - EnqueuePoster admits at most ONE live poster job per clip, so two
//     clicks (or two tabs, or a retrying client) produce exactly one job and
//     the loser is told so;
//   - SetPoster writes the poster columns of a clip that is STILL ready and
//     STILL on the rendition the job read at its start. That second
//     predicate is what makes the picture honest: a trim that lands mid-run
//     replaces the very frames the poster was taken from, so a poster of the
//     old cut describes a clip that no longer exists and must be thrown away
//     rather than published.
//
// Neither ever writes clips.state, clips.failure_reason or either failure
// counter: a card image is a derived extra, and no failure of it may cost a
// clip its readiness.

// PosterMaxAttempts is the CRASH-class cap of a poster job, for the reason
// TrimMaxAttempts and GIFMaxAttempts are constants too: TRANSCODE_MAX_
// ATTEMPTS is a knob about the encode a clip cannot exist without, and a
// card image is one frame of a clip that is already ready and already
// playable. Two — one retry — is the budget, and the fallback is better than
// a third machine attempt: the route says what went wrong and the owner
// picks a frame again.
//
// Like the GIF's, it does NOT bound the ENVIRONMENT counter (§20 erratum
// 89): a storage blip is not an argument about this frame.
const PosterMaxAttempts = 2

// PosterParams is the `params_json` of a poster job: the frame the owner
// chose, in seconds on the ORIGINAL's timeline (§20 erratum 68's coordinate
// system — the one that does not move under the owner, which is what lets a
// later trim keep the choice).
//
// It lives here because it IS the shape of a column: the handler that writes
// it and the worker that reads it must agree, and the schema is where that
// agreement belongs.
type PosterParams struct {
	Seconds float64 `json:"seconds"`
}

// MarshalPosterParams renders the params column for EnqueuePoster.
//
// Its failure branch writes something ParsePosterParams REFUSES rather than
// an empty object, and that is the whole reason it is written out:
// encoding/json cannot represent NaN or ±Inf, so a value that got past the
// handler would come back as `{}` — which parses as zero, the first frame.
// A job that publishes a frame nobody chose is worse than a job that cannot
// run at all.
func MarshalPosterParams(p PosterParams) string {
	b, err := json.Marshal(p)
	if err != nil {
		return "not a poster choice: " + err.Error()
	}
	return string(b)
}

// ParsePosterParams reads a poster job's params column. A row whose params
// are unparsable, negative or not a real number is a job the worker cannot
// run at all, so the error is explicit rather than a silent seek to zero —
// which would publish a frame nobody asked for.
func ParsePosterParams(s string) (PosterParams, error) {
	var p PosterParams
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return PosterParams{}, fmt.Errorf("poster params unparsable: %w", err)
	}
	if math.IsNaN(p.Seconds) || math.IsInf(p.Seconds, 0) || p.Seconds < 0 {
		return PosterParams{}, fmt.Errorf("poster params out of range: seconds=%v", p.Seconds)
	}
	return p, nil
}

// PosterFields is everything SetPoster writes onto the clip. Width/Height
// are MEASURED from the produced JPEG — that is what og:image:width/height
// read — and Seconds is the choice being recorded, on the ORIGINAL's
// timeline.
type PosterFields struct {
	Key           string
	Size          int64
	ETag          string
	Width, Height int
	Seconds       float64
}

// PosterSwap reports what SetPoster did. Reason is set only when OK is false
// and is the operator-facing sentence for the job's `skipped` note: the run
// that produced the JPEG is the only caller, and what it needs to know is
// whether anything now points at its output (OK) or nothing does (Reason).
type PosterSwap struct {
	OK         bool
	Superseded []string
	Reason     string
}

// EnqueuePoster inserts the `poster` job for one clip and returns it.
//
// The guard is the point and it is the whole conflict story of the route:
// the clip must be `ready` (ErrStateMismatch otherwise) and it must have no
// queued or running poster job (ErrConflict). Both checks and the INSERT are
// ONE transaction on the single-writer `rw` pool, so two simultaneous clicks
// produce exactly one job. A missing clip is ErrNotFound.
func (d *DB) EnqueuePoster(ctx context.Context, clipID, paramsJSON string, now int64) (Job, error) {
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
			clipID, KindPoster).Scan(&live); err != nil {
			return err
		}
		if live > 0 {
			return ErrConflict
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO jobs (clip_id, kind, state, stage, params_json, run_after, created_at, updated_at)
VALUES (?, ?, 'queued', ?, ?, ?, ?, ?)`, clipID, KindPoster, JobStageQueued, paramsJSON, now, now, now)
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

// SetPoster points a READY clip at a freshly taken card image and marks the
// job done — one UPDATE, one transaction, and the clip is `ready` before it,
// during it and after it.
//
// playETag is the PLAY OBJECT the frame was taken from, read when the job
// started — `db.PlayObject`'s etag since the quality tiers, which is the
// rendition's whenever there is one and the UPLOAD's on the remux fast path
// (§20 erratum 201). It is part of the UPDATE's predicate, so a trim that committed
// while this job ran makes the swap a no-op with Reason "the rendition
// changed": the caller then deletes its own object. Nothing downstream could
// ever tell the difference — a perfectly valid JPEG of a frame the clip no
// longer contains is the exact failure this predicate exists for (the
// argument of §20 erratum 76, one artefact along).
//
// The GIF is deliberately NOT invalidated here. A trim NULLs gif_* because
// the frames the preview was cut from are gone; choosing a card image
// removes nothing — the preview is still frames of the rendition people are
// watching. It simply no longer opens where the poster does, and the owner
// regenerates it if they care (§20 erratum 131).
//
// It returns the key the clip stopped referencing (the previous poster) so
// the caller can delete it afterwards, and refuses a `transcode` job
// outright, keeping the §5 coupling rule where it belongs.
func (d *DB) SetPoster(ctx context.Context, jobID int64, clipID string, f PosterFields, playETag string, now int64) (PosterSwap, error) {
	var out PosterSwap
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
UPDATE clips SET poster_key = ?, poster_size = ?, poster_etag = ?, poster_width = ?, poster_height = ?,
  poster_seconds = ?, updated_at = ?
WHERE id = ? AND state = 'ready' AND `+playETagSQL+` = ?`,
			f.Key, f.Size, f.ETag, f.Width, f.Height, f.Seconds, now, clipID, playETag))
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
		if c.PosterKey != nil && *c.PosterKey != "" && *c.PosterKey != f.Key {
			out.Superseded = append(out.Superseded, *c.PosterKey)
		}
		out.OK = true
		return nil
	})
	if err != nil {
		return PosterSwap{}, err
	}
	return out, nil
}
