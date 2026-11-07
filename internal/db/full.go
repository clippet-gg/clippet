package db

import (
	"context"
	"database/sql"
	"path"
	"strings"
)

// The FULL tier, database half (DESIGN.md §17 #10, §5's job-kind rule, §20
// errata 200–206).
//
// A `full` job exists for one shape of clip: an upload no browser can play as
// it stands (HEVC, ProRes, 10-bit, Matroska, an interlaced capture…) whose
// LIGHT copy the transcode run already made, so the clip is card-ready and
// playable while the faithful copy is still encoding. Its standing is the
// social job's exactly:
//
//   - EnqueueFullTx admits at most ONE live full job per clip and is called
//     from INSIDE the commit that made the clip ready, so a clip cannot become
//     ready without the work its better tier needs being durably queued.
//   - SetFull points `rendition_key` at the faithful copy and writes the two
//     tier words, on a clip that is STILL ready, STILL on the same original and
//     STILL untrimmed. It never writes clips.state, clips.failure_reason or
//     either failure counter.
//
// The three predicates are the whole of its honesty. `state='ready'` is the §5
// rule. The SOURCE etag is the guard a replacement trips: a Full is encoded
// from the original, and a replacement puts a different original under the same
// clip id (SwapSource), so a Full of the retired file must not be published.
// And an untrimmed window is what a TRIM trips: a trim produces a Full of the
// CUT — from the same original, so the etag still matches — and the whole-file
// Full this job is carrying is then a copy of footage the clip no longer is.
//
// A failure costs the clip nothing but quality: the Light copy goes on playing,
// `full_state` becomes `failed`, and the quality selector says so.

// FullMaxAttempts is the CRASH-class cap of a full job. It is TWO more than a
// social job's, and the reason is what the file IS: a Light copy is a
// convenience whose absence nobody can see, while a Full is the only faithful
// copy of a source the browser cannot play — the file the owner will download
// in a year. Three attempts is TRANSCODE_MAX_ATTEMPTS' own default, and this
// tier deserves the same budget the encode a clip cannot exist without gets.
//
// Like every other kind it does NOT bound the ENVIRONMENT counter (§20 erratum
// 89): a storage blip is not an argument about this file.
const FullMaxAttempts = 3

// FullFields is everything SetFull writes. Every dimension and rate here is
// MEASURED from the produced file, never planned — they become the clip's own
// width/height/fps the moment this commits, and the quality selector's "Full
// 1440p60" label.
type FullFields struct {
	Key            string
	Size           int64
	ETag           string
	Width, Height  int
	DurationMS     int64
	FPSNum, FPSDen int
	HasAudio       bool
	// Codec is which encoder produced it ("h264" | "av1"), stored so the
	// selector and `admin ls` can say, and so an operator who changed
	// FULL_CODEC can tell which clips predate the change.
	Codec string
}

// FullSwap reports what SetFull did. Reason is set only when OK is false and is
// the operator-facing sentence for the job's `skipped` note.
type FullSwap struct {
	OK         bool
	Superseded []string
	Reason     string
}

// EnqueueFull inserts the `full` job for one clip and returns it. It is the
// exported form of enqueueFullTx, used by tests and by any caller that is not
// already inside a transaction; the commits that matter use the Tx form.
//
// The clip must be `ready` (ErrStateMismatch otherwise) and must have no queued
// or running full job (ErrConflict). A missing clip is ErrNotFound.
func (d *DB) EnqueueFull(ctx context.Context, clipID string, now int64) (Job, error) {
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
		id, queued, err := enqueueFullTx(ctx, tx, clipID, now)
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

// enqueueFullTx queues the faithful copy of a clip whose LIGHT copy has just
// been published, INSIDE that same transaction.
//
// queued=false means one was already live. FinalizeTranscode cannot reach it (a
// clip becoming ready has no live media job); SwapSource legitimately can (an
// owner replacing a file twice in a minute), where the right answer is that the
// job already queued reads the original that exists when it runs. A RUNNING
// one read the old original, so SwapSource supersedes it first
// (supersedeRunningTx, §20 erratum 237 (a)).
//
// Like the social job there is no `unlisted` rule: `unlisted` means "do not
// surface this", and a stored quality tier surfaces nothing.
func enqueueFullTx(ctx context.Context, tx *sql.Tx, clipID string, now int64) (id int64, queued bool, err error) {
	var live int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM jobs WHERE clip_id = ? AND kind = ? AND state IN ('queued','running')",
		clipID, KindFull).Scan(&live); err != nil {
		return 0, false, err
	}
	if live > 0 {
		return 0, false, nil
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO jobs (clip_id, kind, state, stage, run_after, created_at, updated_at)
VALUES (?, ?, 'queued', ?, ?, ?, ?)`, clipID, KindFull, JobStageQueued, now, now, now)
	if err != nil {
		return 0, false, err
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// SetFull points a READY clip at its faithful copy and marks the job done — one
// UPDATE, one transaction, and the clip is `ready` before it, during it and
// after it.
//
// sourceETag is the original the job encoded from, read when it started. It is
// part of the UPDATE's predicate, so a replacement that committed while this
// job ran makes the swap a no-op with Reason "the original changed": the caller
// deletes its own output, and the replacement's own commit has already queued
// whatever the new file needs.
//
// The trim guard is the second predicate, and it is not a refinement of the
// first: a trim re-derives the rendition from the SAME original, so the etag
// still matches, and the window it wrote is the only trace that the clip is no
// longer the whole of that file. Publishing a whole-file Full over a trimmed
// clip would silently un-cut it.
//
// It returns the key the clip stopped referencing so the caller can delete it —
// the LIGHT-as-rendition case cannot produce one (a clip waiting for its Full
// has `rendition_key` NULL), but a retry that raced its own predecessor can —
// and refuses a `transcode` job outright, keeping the §5 coupling rule where it
// belongs.
func (d *DB) SetFull(ctx context.Context, jobID int64, clipID string, f FullFields, sourceETag string, now int64) (FullSwap, error) {
	var out FullSwap
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
		case c.SourceETag != sourceETag:
			out.Reason = "the original changed"
			return nil
		case c.TrimStartMS != nil || c.TrimEndMS != nil:
			out.Reason = "the clip was trimmed"
			return nil
		}
		n, err := affected(tx.ExecContext(ctx, `
UPDATE clips SET rendition_key = ?, rendition_size = ?, rendition_etag = ?,
  width = ?, height = ?, duration_ms = ?, fps_num = ?, fps_den = ?, has_audio = ?,
  full_state = 'ready', full_codec = ?, updated_at = ?
WHERE id = ? AND state = 'ready' AND source_etag = ? AND trim_start_ms IS NULL AND trim_end_ms IS NULL`,
			f.Key, f.Size, f.ETag,
			f.Width, f.Height, f.DurationMS, f.FPSNum, f.FPSDen, boolInt(f.HasAudio),
			f.Codec, now, clipID, sourceETag))
		if err != nil {
			return err
		}
		if n == 0 {
			out.Reason = "the original changed"
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE jobs SET state = 'done', stage = 'done', finished_at = ?, updated_at = ? WHERE id = ?",
			now, now, jobID); err != nil {
			return err
		}
		if c.RenditionKey != nil && *c.RenditionKey != "" && *c.RenditionKey != f.Key {
			out.Superseded = append(out.Superseded, *c.RenditionKey)
		}
		out.OK = true
		return nil
	})
	if err != nil {
		return FullSwap{}, err
	}
	return out, nil
}

// SetTierState records the terminal state of ONE tier on a clip that stays
// exactly as it is otherwise (§20 erratum 205).
//
// It exists for one caller and one moment: a `full` or `social` job that has
// run out of attempts on the WORKER's own exit path, where the clip is perfectly
// fine — it plays, its card works — and the only new fact is that a better or
// smaller copy will not be arriving. The quality selector reads it and says so
// instead of offering a tier that is never coming, and GET …/progress stops
// reporting `encoding` for a job that has stopped.
//
// Since §20 erratum 236 it is no longer the ONLY writer: failAttemptTx writes the
// same column in the same transaction as the job's own `failed`, so an ending
// with no worker to run an exit path — the automatic repair, the boot reconcile,
// `admin jobs unstick` — settles the tier too.
//
// It is the UNGUARDED write, and since erratum 237 no worker takes it: a job
// settling its own ending calls SettleTierEncoding, which writes the same word
// only while the column still promises `encoding`. What is left here is the
// primitive that STATES a tier unconditionally — the four states, both tier
// names, `pending` refused because it is derived — which is what the tests that
// plant a tier word need and what a future caller with a fact rather than a
// footnote would want.
//
// It refuses anything but the two tier names and the four states, writes
// nothing else, and is a no-op on a clip that is gone or being deleted: this is
// a footnote on a job's ending, never a reason to fail one.
func (d *DB) SetTierState(ctx context.Context, clipID, tier, state string, now int64) error {
	column, ok := tierStateColumn(tier)
	if !ok {
		return ErrStateMismatch
	}
	switch state {
	case TierSkipped, TierEncoding, TierReady, TierFailed:
	default:
		return ErrStateMismatch
	}
	_, err := d.exec(ctx,
		"UPDATE clips SET "+column+" = ?, updated_at = ? WHERE id = ? AND state <> 'deleting'",
		state, now, clipID)
	return err
}

// SettleTierEncoding is SetTierState's GUARDED sibling, and the one write a job's
// own ending makes: it records that this tier is not coming — `skipped` or
// `failed` — but ONLY while the column still says `encoding` AND no job of that
// kind is live for the clip (§20 erratum 237).
//
// The two conditions are one sentence. `encoding` is a PROMISE that a job of this
// kind is coming, so the only thing entitled to overwrite it is the ending that
// makes that promise false, and an ending is only that ending if nothing of the
// kind is left. Unguarded, the same write races every commit that publishes or
// re-queues a tier: a trim publishes a Full (`full_state = ready`, and its key
// with it), a replacement queues a fresh card copy (`light_state = encoding`) —
// and a late footnote from a job that stopped would bury the fact, tell the
// quality selector a file in the bucket is not coming, or release erratum 235's
// `CardReady` belt while a card copy really was still being encoded.
//
// It is keyed on the job KIND rather than the tier name because both halves of
// the predicate are about the job: tierStateColumnOfKind is the mapping erratum
// 236 introduced, and "is anything of this kind live" is the second half.
//
// Like SetTierState it never touches a clip that is being deleted, and it is a
// footnote on a job's ending rather than a reason to fail one: a caller logs the
// error and carries on.
func (d *DB) SettleTierEncoding(ctx context.Context, clipID, kind, state string, now int64) error {
	column, ok := tierStateColumnOfKind(kind)
	if !ok {
		return ErrStateMismatch
	}
	switch state {
	case TierSkipped, TierFailed:
	default:
		return ErrStateMismatch
	}
	_, err := d.exec(ctx, settleTierSQL(column), state, now, clipID, TierEncoding, clipID, kind)
	return err
}

// settleTierSQL is the statement both settle paths run — SettleTierEncoding for a
// worker that ran its own exit path, and failAttemptTx for the three endings with
// no worker on the job (erratum 236) — so the guard has ONE definition and the
// two cannot drift apart. Its parameters are, in order: the state to write, now,
// the clip id, the state being overwritten (always `encoding`), the clip id
// again and the job kind.
//
// The `NOT EXISTS` term is only correct because the job in hand is resolved
// BEFORE it runs: CompleteJob has written `done` on the skip paths and
// failAttemptTx's own statement has written `failed` above this one, so a job
// never reads itself as the reason not to settle. A job that could not be
// resolved (the row was cancelled underneath it) settles nothing, which is the
// conservative half of the same rule.
func settleTierSQL(column string) string {
	return "UPDATE clips SET " + column + " = ?, updated_at = ?" +
		" WHERE id = ? AND state <> 'deleting' AND " + column + " = ?" +
		" AND NOT EXISTS (SELECT 1 FROM jobs WHERE clip_id = ? AND kind = ? AND state IN ('queued','running'))"
}

// tierStateColumn maps a tier NAME to the clips column that holds its state.
// The Original has none — its bytes are in the bucket before the 201, so the
// tier exists for every row that exists — and anything else is not a tier.
func tierStateColumn(tier string) (string, bool) {
	switch tier {
	case TierNameFull:
		return "full_state", true
	case TierNameLight:
		return "light_state", true
	}
	return "", false
}

// tierOfKind maps a job KIND to the tier it produces, and is the whole of that
// mapping (§20 erratum 236). Every other kind returns "": a transcode settles
// every tier through TierFields, and a trim, a replacement, a poster, a
// filmstrip, a GIF and a notify produce no tier at all.
//
// It is deliberately a mapping from the kind rather than a list of kinds to
// treat specially, because "the kinds this function knows about" is exactly the
// thing that goes stale when a new kind is added.
func tierOfKind(kind string) string {
	switch kind {
	case KindSocial:
		return TierNameLight
	case KindFull:
		return TierNameFull
	}
	return ""
}

// tierStateColumnOfKind composes the two: the column a job of this kind settles
// when it ends for good, or ok=false for a kind that owns no tier.
func tierStateColumnOfKind(kind string) (string, bool) {
	tier := tierOfKind(kind)
	if tier == "" {
		return "", false
	}
	return tierStateColumn(tier)
}

// The tier NAMES, as SetTierState and the DTO speak them. They mirror
// transcode.Tier* for the reason TierReady and its siblings mirror
// transcode.TierState*: internal/db imports nothing of internal/transcode.
const (
	TierNameOriginal = "original"
	TierNameFull     = "full"
	TierNameLight    = "light"
)

// TierStateRow is one tier's name and its reconciled state. It exists so the
// clip DTO, the single progress route and the BATCHED progress route all get
// their tier states from one function: erratum 116's rule is that a batched poll
// and a single poll of one clip can never answer differently, and two
// reconciliations of the same three columns is exactly how they would.
type TierStateRow struct{ Tier, State string }

// ReconcileTierState settles a stored tier word against the OBJECT.
//
// The object wins when it exists — a file in the bucket is not "encoding"
// whatever a column says, which is what makes a row migrated from before 0012
// read correctly — and an unknown state (that same row, or a clip whose tiers
// have not been decided yet) reads as `skipped`: the honest answer to "is a
// better copy coming" is no, nobody has queued one.
func ReconcileTierState(stored string, present bool) string {
	if present {
		return TierReady
	}
	switch stored {
	case TierEncoding, TierFailed, TierSkipped:
		return stored
	}
	return TierSkipped
}

// ReconcileClipTier is ReconcileTierState with the CLIP's own state in hand,
// and it is what every reporter of a tier state uses (§20 erratum 205, amended).
//
// The one thing it adds is the difference between "not needed" and "not decided
// yet". A queued or processing clip has an unknown word in both columns because
// nothing has looked at the upload yet, and reporting `skipped` for it told a
// client that a Full it may be about to get is never coming — the exact opposite
// of the truth, in the state a client is most likely to be polling. That case is
// `pending`; it is never stored, and every other case is unchanged. A FAILED
// clip is deliberately not pending: nothing is coming for it either.
func ReconcileClipTier(clipState, stored string, present bool) string {
	if !present && stored == TierUnknown && (clipState == ClipQueued || clipState == ClipProcessing) {
		return TierPending
	}
	return ReconcileTierState(stored, present)
}

// TierStatesOf is the three tiers' states in QUALITY order. The Original is
// always `ready`: its bytes were fsynced to the bucket before the 201, so the
// tier exists for every row that exists — whether a browser can PLAY it is a
// different question, and the clip DTO's `playable` is where that is answered.
func TierStatesOf(clipState, fullStored, lightStored string, hasFull, hasLight bool) []TierStateRow {
	return []TierStateRow{
		{Tier: TierNameOriginal, State: TierReady},
		{Tier: TierNameFull, State: ReconcileClipTier(clipState, fullStored, hasFull)},
		{Tier: TierNameLight, State: ReconcileClipTier(clipState, lightStored, hasLight)},
	}
}

// TierStatesOfClip is TierStatesOf for a whole clip row. A remuxed original is
// NOT a Full — the coded bytes were never re-encoded — so the rendition object
// it points at does not make one exist.
func TierStatesOfClip(c Clip) []TierStateRow {
	return TierStatesOf(c.State, c.FullState, c.LightState,
		c.RenditionKey != nil && !c.OriginalRemuxed, c.SocialKey != nil)
}

// PlayObject is the object a clip's PLAYABLE bytes live in, resolved from the
// row (§20 erratum 201). It is the single answer to "which file do I read to
// derive something from this clip", and every derived-artefact job asks it: the
// GIF, the chosen card frame and the Light copy.
//
// The order is the tier order, best first:
//
//  1. `rendition_key` — the FULL re-encode, or the `-c copy` remux of a
//     playable original, or a trim's cut. Whenever it is set it is the file
//     `/video.mp4` serves.
//  2. the SOURCE, when `original_playable` says the upload is streamable as it
//     stands and no remux was needed. This is the remux fast path: there is no
//     second object, and the upload IS the playable file.
//  3. the LIGHT copy, while a Full is still encoding. A clip in that state is
//     card-ready and playable, and the card's own file is the only playable
//     thing it has (§20 erratum 206).
//
// Case 3 is DORMANT, and it is worth knowing which one is: since erratum 217 a
// plan that wants both tiers runs ONE ffmpeg and publishes both files at the
// same flip, so `EnqueueFull` has no producer left and no current path can
// leave a clip holding only a Light copy. The case stays because it is the
// right answer the day a plan splits the two encodes again, and because
// reaching it is not a failure: the tagged Light copy would simply be what
// `/video.mp4` serves and what the GIF, the card frame and the filmstrip are
// derived from until the Full lands.
//
// ok=false means the clip has no playable bytes at all, which on a `ready` row
// is an invariant violation (§12) and on any other state is simply "not yet".
func PlayObject(c Clip) (key string, size int64, etag string, ok bool) {
	switch {
	case c.RenditionKey != nil && c.RenditionSize != nil && c.RenditionETag != nil:
		return *c.RenditionKey, *c.RenditionSize, *c.RenditionETag, true
	case c.OriginalPlayable && !c.OriginalRemuxed:
		return c.SourceKey, c.SourceSize, c.SourceETag, true
	case c.SocialKey != nil && c.SocialSize != nil && c.SocialETag != nil:
		return *c.SocialKey, *c.SocialSize, *c.SocialETag, true
	}
	return "", 0, "", false
}

// CardObject is the object a CRAWLER fetches for this clip: the LIGHT copy when
// there is one, and otherwise whatever `/video.mp4` would serve (§20 erratum
// 212). It mirrors the `/social.mp4` route's own resolution — which is what every
// card tag points at — so "does this clip have a card file" has one answer.
func CardObject(c Clip) (key string, size int64, etag string, ok bool) {
	if c.SocialKey != nil && c.SocialSize != nil && c.SocialETag != nil {
		return *c.SocialKey, *c.SocialSize, *c.SocialETag, true
	}
	return PlayObject(c)
}

// CardReady reports whether this clip's Discord card WORKS: a `ready` clip with a
// poster and with the file its `og:video` points at (§20 erratum 212).
//
// The owner's decision of 2026-09-08: a link is given only when it is ready for
// sharing, which means the Discord card is ready. This is that predicate, and it has three readers — deliberately one predicate,
// because three answers to "is the card ready" is how they drift apart:
//
//   - POST /api/upload holds its 201 on it, and reports it as `card_ready`;
//   - GET /clips/{id} answers a link-preview CRAWLER 503 while it is false, so no
//     crawler can cache a card for a clip that has none yet (erratum 213);
//   - the watch page tells the HUMAN, in one short line, while it is false.
//
// Since erratum 206 `ready` already means card-ready, so the conjuncts after the
// state are the belt: a `ready` row with no poster or no playable bytes is the
// §12 invariant violation those readers answer 503 for, and "the card is ready" is
// the one answer that must not be given for it.
//
// `light_state = encoding` is the third of them, and it is the one that says
// something the others cannot (§20 erratum 235). A clip in that state has a card
// file COMING that is not the one CardObject resolves to: the tier exists exactly
// because the fallback — the Full, or the cut a trim just made — is too big, too
// smooth or too large a picture for a crawler, so answering "ready" here hands
// over precisely the file the tier was planned to replace, and Discord remembers
// it per URL. It is safe to wait on because a tier state can no longer stick at
// `encoding`: since errata 236 and 237 EVERY ending of a `social` or `full` job
// writes its column — the automatic repair and the boot reconcile (236), and the
// two endings that resolve the job as `done` without producing a file, a skip and
// a lost commit race (237). Erratum 237's table lists them all, which is what
// makes this wait bounded rather than hopeful.
func CardReady(c Clip) bool {
	if c.State != ClipReady || c.PosterKey == nil || c.LightState == TierEncoding {
		return false
	}
	_, _, _, ok := CardObject(c)
	return ok
}

// CardPending reports whether this clip's card is still COMING: not ready, and
// not failed either.
//
// A FAILED clip is deliberately excluded. Its link-out card (§9.1) is the card it
// is going to have for ever, so holding a crawler off it would withhold the only
// card it will ever get — and the owner's retry, if it comes, publishes under the
// same URL and the crawler's own cache lifetime is the whole reason erratum 213
// exists (§20 erratum 213).
func CardPending(c Clip) bool { return !CardReady(c) && c.State != ClipFailed }

// playETagSQL is PlayObject's etag as an SQL expression, mirror for mirror. It
// is the predicate every DERIVED-artefact commit is guarded by — SetGIF,
// SetSocial, SetPoster — so a preview, a card file or a chosen frame is only
// published while the clip is still the file it was derived from (§20 erratum
// 201). Before the quality tiers it read `rendition_etag` and nothing else,
// which is still the first branch; the two that follow are the two ways a ready
// clip can have playable bytes with no rendition object at all.
//
// The Go side is PlayObject: one definition asked twice, in the two languages
// this package speaks, in the same order.
const playETagSQL = `COALESCE(rendition_etag,
  CASE WHEN original_playable = 1 AND original_remuxed = 0 THEN NULLIF(source_etag, '') END,
  social_etag)`

// PlayETagOf is the Go side of that expression, for the read half of the same
// commits (they check the clip they just read before they run the UPDATE, so
// the reason can be worded).
func PlayETagOf(c Clip) (string, bool) {
	_, _, etag, ok := PlayObject(c)
	if !ok || etag == "" {
		return "", false
	}
	return etag, true
}

// playETagMatches reports whether the clip's playable bytes are still the ones
// a derived-artefact job read when it started. An empty candidate never
// matches: "I did not record what I read" must not pass a predicate.
func playETagMatches(c Clip, etag string) bool {
	got, ok := PlayETagOf(c)
	return ok && etag != "" && got == etag
}

// OriginalIsTheClip reports whether the uploaded bytes are still the whole of
// what this clip IS — and are therefore safe to hand an anonymous viewer
// (§20 erratum 207).
//
// It is `original_playable` plus three conjuncts, each of which is a way for
// the upload to stop being the clip:
//
//   - REMUXED. The bytes a viewer plays are then the faststart rewrite, which
//     `/video.mp4` already serves anonymously; nothing is gained by opening a
//     second door onto the raw upload, so it stays the owner's.
//   - TRIMMED. A cut clip is a WINDOW of the upload. Serving the upload to
//     anybody holding the link would hand out the footage the owner removed,
//     which is the one thing a trim is for.
//   - NOT READY. A queued, processing or failed clip has published nothing.
//     Its upload is the owner's alone, exactly as it was before these tiers.
//
// The owner's own access to `/original` is unchanged in every one of those
// states: this predicate only ever ADDS anonymous read, and only for the file
// the watch page is streaming anyway.
func OriginalIsTheClip(c Clip) bool {
	return c.OriginalPlayable && !c.OriginalRemuxed &&
		c.TrimStartMS == nil && c.TrimEndMS == nil &&
		c.State == ClipReady
}

// AudioPlayType is the media type an AUDIO clip's playable file is served
// under at GET /api/clips/{id}/audio (§20 erratum 328), and named by the card's
// og:audio:type (erratum 329): the stored original's own type when it plays,
// otherwise the Full's by its extension — the two the worker stores one as.
// "" when the clip has nothing to play yet.
func AudioPlayType(c Clip) string {
	key, _, _, ok := PlayObject(c)
	if !ok {
		return ""
	}
	if key == c.SourceKey {
		return c.SourceContentType
	}
	switch strings.ToLower(path.Ext(key)) {
	case ".flac":
		return "audio/flac"
	case ".m4a":
		return "audio/mp4"
	}
	return "video/mp4" // the card copy
}
