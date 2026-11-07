package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// TrimParams is the `params_json` of a trim job: the window, in
// milliseconds, measured on the ORIGINAL's timeline — 0 is the first frame of
// the file that was uploaded, never of the rendition the owner happens to be
// looking at (§20 erratum 68). The original is retained for the life of the
// clip, so it is the one coordinate system that does not move underneath the
// owner: a clip cut to 5s–9s can be re-extended to 2s–4s, because both windows
// mean the same thing on the same file.
//
// It lives here because it IS the shape of a column — the handler that writes
// it and the worker that reads it must agree, and the schema is the place that
// agreement belongs.
type TrimParams struct {
	StartMS int64 `json:"start_ms"`
	EndMS   int64 `json:"end_ms"`
}

// MarshalTrimParams renders the params column for EnqueueTrim.
func MarshalTrimParams(p TrimParams) string {
	b, err := json.Marshal(p)
	if err != nil { // unreachable: two int64 fields
		return "{}"
	}
	return string(b)
}

// ParseTrimParams reads a trim job's params column. A row whose params are
// unparsable or empty is a job the worker cannot run at all, so the error is
// explicit rather than a zero window.
func ParseTrimParams(s string) (TrimParams, error) {
	var p TrimParams
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return TrimParams{}, fmt.Errorf("trim params unparsable: %w", err)
	}
	if p.StartMS < 0 || p.EndMS <= p.StartMS {
		return TrimParams{}, fmt.Errorf("trim params out of range: start_ms=%d end_ms=%d", p.StartMS, p.EndMS)
	}
	return p, nil
}

// Server-side trim, database half (DESIGN.md §17 #5, §5's Phase 4 rule).
//
// Two statements carry the whole feature, and both are guarded transactions
// rather than plain writes:
//
//   - EnqueueTrim admits at most one live trim per clip, so two clicks (or
//     two tabs) cannot both start one;
//   - SwapRendition moves the clip from the old rendition to the new one in
//     a SINGLE UPDATE, with the clip's `ready` state and the job's `running`
//     state as its predicate — the §11 lost-race pattern, in the one place
//     where losing the race must leave the OLD rendition untouched.
//
// Neither ever writes clips.state, clips.failure_reason or either failure
// counter of the clip: a trim runs against a ready clip and leaves it ready
// however it ends.

// EnqueueTrim inserts the `trim` job for one clip and returns it.
//
// The guard is the point: the clip must be `ready` (ErrStateMismatch
// otherwise) and it must have no queued or running trim job (ErrConflict).
// Both checks and the INSERT are ONE transaction on the single-writer `rw`
// pool, so two concurrent requests are serialised and the second sees the
// first's row — the API's 409 `trim_in_progress` is decided here, not by a
// racy pre-read in the handler. A missing clip is ErrNotFound.
func (d *DB) EnqueueTrim(ctx context.Context, clipID, paramsJSON string, now int64) (Job, error) {
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
			clipID, KindTrim).Scan(&live); err != nil {
			return err
		}
		if live > 0 {
			return ErrConflict
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO jobs (clip_id, kind, state, stage, params_json, run_after, created_at, updated_at)
VALUES (?, ?, 'queued', ?, ?, ?, ?, ?)`, clipID, KindTrim, JobStageQueued, paramsJSON, now, now, now)
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

// LatestJobOfKind returns the newest job of one kind for a clip, or
// ErrNotFound. GET /api/clips/{id}/trim reports on exactly this row.
func (d *DB) LatestJobOfKind(ctx context.Context, clipID, kind string) (Job, error) {
	return scanJob(d.ro.QueryRowContext(ctx,
		"SELECT "+jobCols+" FROM jobs WHERE clip_id = ? AND kind = ? ORDER BY id DESC LIMIT 1", clipID, kind))
}

// SwapRendition points a READY clip at a freshly produced rendition and
// poster and marks the job done — one UPDATE per row, one transaction, and
// the clip is `ready` before it, during it and after it.
//
// It returns the keys the clip stopped referencing (the previous rendition
// and poster, when they differ from the new ones) so the caller can delete
// them afterwards, and ok=false — changing NOTHING — when the swap must not
// happen:
//
//   - the clip is no longer `ready` (tombstoned by an owner DELETE or the
//     sweeper, or dragged back to `queued` by a retry) or is gone;
//   - the job is no longer `running` (MarkDeleting cancels every live job of
//     the clip, so this is the same race seen from the job's side).
//
// The caller then deletes ITS OWN outputs, exactly as the transcode does
// after a lost FinalizeTranscode (§11). Refusing a transcode job outright
// keeps the §5 coupling rule where it belongs: only FinalizeTranscode may
// ever write clips.state.
//
// `applied` is the window the run ACTUALLY cut, on the original's timeline —
// for the stream-copy path that is the keyframe-snapped start, not the
// millisecond that was asked for — and it is persisted with the swap so the
// next trim, the DTO and the share page all speak about the same file. The
// same statement fills `source_duration_ms` from this run's probe of the
// original (COALESCE: a probe that measured nothing leaves the old value
// alone), which is how a row that predates migration 0003 learns its own
// source length.
//
// It also invalidates BOTH derived extras of the rendition it replaces — the
// preview GIF and the social rendition (§17 #6, #3) — returning their keys
// among the superseded ones and, when `f.QueueSocial` says so, queuing the
// social encode again in this same commit.
//
// `poster_seconds` is written from `f.PosterSeconds` and is therefore the
// one poster column a trim can also CLEAR: the owner's chosen card-image
// frame is on the original's timeline, so a cut that still contains that
// moment keeps the choice (the run re-took the frame from the new
// rendition), and one that does not sets it back to NULL — the automatic
// frame — because the moment no longer exists in this clip (§20 erratum
// 130).
//
// alreadyApplied is the third answer, and it means exactly what it does for
// FinalizeTranscode (§20 erratum 102): this attempt's OWN earlier commit
// landed and was reported as an error, so the row already references the
// rendition and poster this run produced. The check runs on both refusal
// paths, because a landed swap moves the job to `done` as well as the clip:
// the second call is turned away by the job-state guard before the clip
// guard is ever reached. Deleting this run's outputs in that state would
// delete the objects the ready row now points at. The derived extras this
// statement NULLs out are no obstacle to recognising such a commit: the
// signature `attemptAlreadyApplied` reads is the attempt-scoped rendition
// and poster pair, both of which the swap writes.
func (d *DB) SwapRendition(ctx context.Context, jobID int64, clipID string, f ReadyFields, applied TrimParams, now int64) (superseded []string, ok, alreadyApplied bool, err error) {
	err = d.tx(ctx, func(tx *sql.Tx) error {
		j, err := getJob(ctx, tx, jobID)
		switch {
		case err == ErrNotFound:
			return nil
		case err != nil:
			return err
		case j.Kind == KindTranscode:
			return ErrStateMismatch
		case j.State != JobRunning:
			mine, err := attemptAlreadyApplied(ctx, tx, clipID, f)
			alreadyApplied = mine
			return err
		}
		c, err := getClip(ctx, tx, clipID)
		switch {
		case err == ErrNotFound:
			return nil
		case err != nil:
			return err
		case c.State != ClipReady:
			return nil
		}
		n, err := affected(tx.ExecContext(ctx, `
UPDATE clips SET
  rendition_key = ?, rendition_size = ?, rendition_etag = ?,
  poster_key = ?, poster_size = ?, poster_etag = ?, poster_width = ?, poster_height = ?,
  poster_seconds = ?,
  width = ?, height = ?, duration_ms = ?, fps_num = ?, fps_den = ?, has_audio = ?,
  source_duration_ms = COALESCE(?, source_duration_ms), trim_start_ms = ?, trim_end_ms = ?,
  gif_key = NULL, gif_size = NULL, gif_etag = NULL, gif_width = NULL, gif_height = NULL,
  filmstrip_key = NULL, filmstrip_size = NULL, filmstrip_etag = NULL,
  filmstrip_tiles = NULL, filmstrip_tile_w = NULL, filmstrip_tile_h = NULL, filmstrip_interval_ms = NULL,
  social_key = NULL, social_size = NULL, social_etag = NULL, social_width = NULL, social_height = NULL,
  social_fps_num = NULL, social_fps_den = NULL,
  original_playable = ?, original_remuxed = ?, original_width = ?, original_height = ?,
  original_fps_num = ?, original_fps_den = ?, original_codec = ?,
  full_state = ?, full_codec = ?, light_state = ?,
  updated_at = ?
WHERE id = ? AND state = 'ready'`,
			f.RenditionKey, f.RenditionSize, f.RenditionETag,
			f.PosterKey, f.PosterSize, f.PosterETag, f.PosterWidth, f.PosterHeight,
			nullFloat64(f.PosterSeconds),
			f.Width, f.Height, f.DurationMS, f.FPSNum, f.FPSDen, boolInt(f.HasAudio),
			nullInt64(f.SourceDurationMS), applied.StartMS, applied.EndMS,
			boolInt(f.Tiers.OriginalPlayable), boolInt(f.Tiers.OriginalRemuxed),
			zeroNull(int64(f.Tiers.OriginalWidth)), zeroNull(int64(f.Tiers.OriginalHeight)),
			zeroNull(int64(f.Tiers.OriginalFPSNum)), zeroNull(int64(f.Tiers.OriginalFPSDen)),
			f.Tiers.OriginalCodec,
			f.Tiers.FullState, f.Tiers.FullCodec, f.Tiers.LightState,
			now, clipID))
		if err != nil {
			return err
		}
		if n == 0 {
			mine, err := attemptAlreadyApplied(ctx, tx, clipID, f)
			alreadyApplied = mine
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE jobs SET state = 'done', stage = 'done', finished_at = ?, updated_at = ? WHERE id = ?",
			now, now, jobID); err != nil {
			return err
		}
		// The preview GIF and the social rendition go with them: both were
		// derived from the rendition this statement just replaced, so both
		// now describe a clip that no longer exists. NULLing the columns is
		// what makes the GIF route generate a new preview on demand and what
		// puts `/social.mp4` back on the (new) main rendition until the job
		// queued below produces its replacement; listing the keys here is
		// what stops the old objects from outliving the row that referenced
		// them (§17 #6, #3 — erratum 70 reserved this).
		for _, old := range []*string{c.RenditionKey, c.PosterKey, c.GIFKey, c.SocialKey, c.FilmstripKey} {
			if old != nil && *old != "" && *old != f.RenditionKey && *old != f.PosterKey {
				superseded = append(superseded, *old)
			}
		}
		// A trimmed clip needs its card file re-derived, on exactly the terms
		// a fresh encode does: the caller decides from the NEW rendition's
		// size and frame rate whether one is wanted at all (§20 erratum 121).
		// An already-QUEUED job is left alone — it will read whatever rendition
		// exists when it runs. A RUNNING one is encoding the rendition this
		// statement just replaced, so it is superseded and a fresh job queued
		// behind it (§20 erratum 237 (a)).
		if f.QueueSocial {
			if err := supersedeRunningTx(ctx, tx, clipID, KindSocial, now); err != nil {
				return err
			}
			if _, _, err := enqueueSocialTx(ctx, tx, clipID, now); err != nil {
				return err
			}
		}
		ok = true
		return nil
	})
	if err != nil {
		return nil, false, false, err
	}
	return superseded, ok, alreadyApplied, nil
}

// TrimTierFields is what a trim's commit says about the tiers (§20 erratum 208).
//
// A cut is always a re-encode or a keyframe stream copy of a WINDOW, so the
// clip it produces is the FULL tier whatever the original was: `full_state` is
// ready and the codec is H.264 (a trim's copy path preserves the source's
// stream, which the copy rule only chooses for H.264 in the first place). The
// ORIGINAL's own measurements are carried through from the trim's re-probe of
// the retained upload, because they are facts about a file the trim did not
// touch — and `original_remuxed` goes back to false, because the remux this
// cut replaced is one of the objects it just superseded.
//
// The Original TIER stops being offered for a trimmed clip, and nothing here
// does that: the trim WINDOW does, through db.OriginalIsTheClip. Blanking
// `original_playable` would have thrown away a probe result that is still true
// and that a later trim back out to the whole file would need again.
func TrimTierFields(playable bool, w, h, fpsNum, fpsDen int, codec string, queuedLight bool) TierFields {
	light := TierSkipped
	if queuedLight {
		light = TierEncoding
	}
	return TierFields{
		OriginalPlayable: playable,
		OriginalWidth:    w, OriginalHeight: h,
		OriginalFPSNum: fpsNum, OriginalFPSDen: fpsDen,
		OriginalCodec: codec,
		FullState:     TierReady, FullCodec: TierCodecH264,
		LightState: light,
	}
}

// TierCodecH264 is the `full_codec` value of every Full this pipeline produces
// with libx264 — including a trim's cut, whose stream-copy path only ever
// copies H.264 (transcode.DecideTrim refuses anything else).
const TierCodecH264 = "h264"
