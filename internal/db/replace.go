package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Clip replacement, database half (ROADMAP #31, DESIGN.md §20 erratum 135;
// §5's Phase 4 job-kind rule).
//
// A replacement is a second upload under an id that already has one. Two
// statements carry it, and both are guarded transactions rather than plain
// writes:
//
//   - EnqueueReplace admits at most one live replacement per clip, so two
//     taps (or two tabs) cannot both start one;
//   - SwapSource points the clip at the NEW original, the rendition derived
//     from it and its poster in a SINGLE UPDATE, with the clip's `ready`
//     state and the job's `running` state as its predicate — the §11
//     lost-race pattern, in the one place where losing the race must leave
//     the clip on the file it already had.
//
// Neither ever writes clips.state, clips.failure_reason or either failure
// counter: a replacement runs against a ready clip and leaves it ready
// however it ends, which is what keeps the share page and the posted card
// playable for the whole run (a viewer never sees the clip leave `ready`).
//
// What SwapSource PRESERVES is as much of the contract as what it writes:
// the id and therefore the share URL, the title, description, unlisted flag,
// tags, collection membership, TTL and expiry, the view count, created_at and
// ready_at. What it RESETS is everything that described the file that is
// being replaced: the trim window (the new original has its own timeline), the
// preview GIF and the social rendition (both were derived from a rendition
// that no longer exists).

// ReplaceMaxAttempts is the CRASH-class cap of a replacement job — a panic, or
// a job found `running` at boot with no checkpoint — and it is a constant for
// the reason TrimMaxAttempts is: TRANSCODE_MAX_ATTEMPTS is a knob about the
// encode a clip cannot exist without, and a replacement is an edit of a clip
// that is already ready and already playable. Two is the budget, because a
// replacement that fails leaves NOTHING broken: the clip keeps the original,
// the rendition and the poster it had, and the owner can upload again.
//
// It does NOT bound the ENVIRONMENT counter (§20 erratum 89): a storage blip
// is not an argument about this file, so a replacement gets the instance's
// configured TRANSCODE_MAX_TRANSIENT like every other kind.
const ReplaceMaxAttempts = 2

// ReplaceParams is the `params_json` of a replacement job: everything the
// worker needs to find the file the handler already spooled and stored, and
// everything the commit will write onto the clip as its new source.
//
// It lives here because it IS the shape of a column — the handler that writes
// it and the worker that reads it must agree, and the schema is the place
// that agreement belongs. It is also why this feature needs no migration: the
// job row carries the attempt, exactly as the trim's window does.
//
// Spool is a directory NAME under SCRATCH_DIR, never a path: a job row is not
// a place to take a filesystem path on trust (scratch.ValidTmpName).
type ReplaceParams struct {
	SourceKey         string `json:"source_key"`
	SourceFilename    string `json:"source_filename"`
	SourceSize        int64  `json:"source_size"`
	SourceContentType string `json:"source_content_type"`
	SourceETag        string `json:"source_etag"`
	Spool             string `json:"spool"`
}

// MarshalReplaceParams renders the params column for EnqueueReplace.
func MarshalReplaceParams(p ReplaceParams) string {
	b, err := json.Marshal(p)
	if err != nil { // unreachable: five strings and an int64
		return "{}"
	}
	return string(b)
}

// ParseReplaceParams reads a replacement job's params column. A row whose
// params are unparsable, keyless or negative is a job the worker cannot run
// at all, so the error is explicit rather than a zero-valued attempt that
// would upload an empty original over a live clip.
func ParseReplaceParams(s string) (ReplaceParams, error) {
	var p ReplaceParams
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return ReplaceParams{}, fmt.Errorf("replace params unparsable: %w", err)
	}
	if p.SourceKey == "" || p.SourceETag == "" || p.SourceSize <= 0 || p.Spool == "" {
		return ReplaceParams{}, fmt.Errorf("replace params incomplete: key=%q size=%d spool=%q",
			p.SourceKey, p.SourceSize, p.Spool)
	}
	return p, nil
}

// NewSource is the replacement original SwapSource points the clip at. Every
// field is a clips.source_* column, and all five move together: a row that
// named the new key beside the old ETag would be a row no reader could
// validate.
type NewSource struct {
	Key, Filename, ContentType, ETag string
	Size                             int64
}

// EnqueueReplace inserts the `replace` job for one clip and returns it.
//
// The guard is the point: the clip must be `ready` (ErrStateMismatch
// otherwise) and it must have no queued or running replacement job
// (ErrConflict). Both checks and the INSERT are ONE transaction on the
// single-writer `rw` pool, so two concurrent requests are serialised and the
// second sees the first's row — the API's 409 `replace_in_progress` is
// decided here, not by a racy pre-read in the handler. A missing clip is
// ErrNotFound.
//
// It deliberately does not refuse a clip that has a live TRIM or GIF job. Two
// runs of one clip never overlap (the worker registry serialises them), the
// media loop claims them in order, and each commit is guarded by its own
// predicate — so the worst case is a trim whose window is then superseded by
// a replacement, which is precisely what the owner asked for by uploading a
// different file.
func (d *DB) EnqueueReplace(ctx context.Context, clipID, paramsJSON string, now int64) (Job, error) {
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
			clipID, KindReplace).Scan(&live); err != nil {
			return err
		}
		if live > 0 {
			return ErrConflict
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO jobs (clip_id, kind, state, stage, params_json, run_after, created_at, updated_at)
VALUES (?, ?, 'queued', ?, ?, ?, ?, ?)`, clipID, KindReplace, JobStageQueued, paramsJSON, now, now, now)
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

// SwapSource points a READY clip at a new original and the rendition and
// poster derived from it, and marks the job done — one UPDATE per row, one
// transaction, and the clip is `ready` before it, during it and after it.
//
// It returns the keys the clip stopped referencing — the PREVIOUS original
// among them, which is the ordering the whole feature turns on: "upload is
// sacred" means the file that is being replaced survives until the file that
// replaces it is committed and playable, so the delete happens after this
// statement and never before it. ok=false — changing NOTHING — when the swap
// must not happen:
//
//   - the clip is no longer `ready` (tombstoned by an owner DELETE or the
//     sweeper, or dragged back to `queued` by a retry) or is gone;
//   - the job is no longer `running` (MarkDeleting cancels every live job of
//     the clip, so this is the same race seen from the job's side).
//
// The caller then deletes ITS OWN outputs — the uploaded replacement original
// included — exactly as the trim does after a lost SwapRendition (§11).
// Refusing a transcode job outright keeps the §5 coupling rule where it
// belongs: only FinalizeTranscode may ever write clips.state.
//
// Two differences from SwapRendition, both deliberate:
//
//   - `source_duration_ms` is ASSIGNED, not COALESCEd. A trim re-measures the
//     same original, so a probe that measured nothing must leave the old
//     value alone; a replacement measures a DIFFERENT file, and keeping the
//     previous length would leave every future trim window validated against
//     a file nobody can reach any more. NULL — an unmeasurable new original —
//     is the honest answer there, and the trim handler already knows what to
//     do with it (§20 erratum 68).
//   - `trim_start_ms`/`trim_end_ms` are NULLed rather than written: the new
//     rendition is the whole of the new original, and the window that
//     described the old one means nothing on the new timeline. `poster_seconds`
//     goes with them for the same reason and not a different one: the owner's
//     chosen card frame is a moment on the ORIGINAL's timeline (§20 errata 127,
//     130), so a replacement retires it exactly as it retires the trim window.
//     A trim can CARRY the choice because it re-cuts the same file; a
//     replacement cannot, because the second the moment named is in a file the
//     clip no longer references — and leaving the number behind would point the
//     share page's "chosen frame" and the GIF's opening frame (erratum 131) at
//     an arbitrary instant of a video nobody chose it from.
func (d *DB) SwapSource(ctx context.Context, jobID int64, clipID string, f ReadyFields, src NewSource, now int64) (superseded []string, ok bool, err error) {
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
			return nil
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
  source_key = ?, source_filename = ?, source_size = ?, source_content_type = ?, source_etag = ?,
  rendition_key = ?, rendition_size = ?, rendition_etag = ?,
  poster_key = ?, poster_size = ?, poster_etag = ?, poster_width = ?, poster_height = ?,
  width = ?, height = ?, duration_ms = ?, fps_num = ?, fps_den = ?, has_audio = ?,
  source_duration_ms = ?, trim_start_ms = NULL, trim_end_ms = NULL, poster_seconds = NULL,
  gif_key = NULL, gif_size = NULL, gif_etag = NULL, gif_width = NULL, gif_height = NULL,
  filmstrip_key = NULL, filmstrip_size = NULL, filmstrip_etag = NULL,
  filmstrip_tiles = NULL, filmstrip_tile_w = NULL, filmstrip_tile_h = NULL, filmstrip_interval_ms = NULL,
  social_key = ?, social_size = ?, social_etag = ?, social_width = ?, social_height = ?,
  social_fps_num = ?, social_fps_den = ?,
  original_playable = ?, original_remuxed = ?, original_width = ?, original_height = ?,
  original_fps_num = ?, original_fps_den = ?, original_codec = ?,
  full_state = ?, full_codec = ?, light_state = ?,
  updated_at = ?
WHERE id = ? AND state = 'ready'`,
			src.Key, src.Filename, src.Size, src.ContentType, src.ETag,
			emptyNull(f.RenditionKey), zeroNull(f.RenditionSize), emptyNull(f.RenditionETag),
			f.PosterKey, f.PosterSize, f.PosterETag, f.PosterWidth, f.PosterHeight,
			f.Width, f.Height, f.DurationMS, f.FPSNum, f.FPSDen, boolInt(f.HasAudio),
			nullInt64(f.SourceDurationMS),
			emptyNull(f.Light.Key), zeroNull(f.Light.Size), emptyNull(f.Light.ETag),
			zeroNull(int64(f.Light.Width)), zeroNull(int64(f.Light.Height)),
			zeroNull(int64(f.Light.FPSNum)), zeroNull(int64(f.Light.FPSDen)),
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
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE jobs SET state = 'done', stage = 'done', finished_at = ?, updated_at = ? WHERE id = ?",
			now, now, jobID); err != nil {
			return err
		}
		// Everything the row has stopped referencing, the previous ORIGINAL
		// first: the preview GIF and the social rendition were derived from
		// the rendition this statement just replaced, and the old original is
		// the file the new one replaces. The NULLed columns are what make the
		// GIF route generate a new preview on demand and put `/social.mp4`
		// back on the (new) main rendition until the job queued below produces
		// its replacement; listing the keys here is what stops the old objects
		// from outliving the row that referenced them.
		if c.SourceKey != "" && c.SourceKey != src.Key {
			superseded = append(superseded, c.SourceKey)
		}
		for _, old := range []*string{c.RenditionKey, c.PosterKey, c.GIFKey, c.SocialKey, c.FilmstripKey} {
			if old != nil && *old != "" && *old != f.RenditionKey && *old != f.PosterKey {
				superseded = append(superseded, *old)
			}
		}
		// A replaced clip needs its card file re-derived, on exactly the terms
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
		// …and the faithful copy of a NEW original the browser cannot play, on
		// exactly the terms FinalizeTranscode queues one (§20 erratum 206): the
		// replacement published the LIGHT copy so the card is right immediately,
		// and the Full follows behind it.
		// A running Full is encoding the original this statement replaced:
		// superseded the same way (§20 erratum 237 (a)).
		if f.Tiers.QueueFull {
			if err := supersedeRunningTx(ctx, tx, clipID, KindFull, now); err != nil {
				return err
			}
			if _, _, err := enqueueFullTx(ctx, tx, clipID, now); err != nil {
				return err
			}
		}
		ok = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return superseded, ok, nil
}
