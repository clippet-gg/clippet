package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Outcome is FailAttempt's verdict.
type Outcome int

const (
	// OutcomeRequeued: the job is queued again with a backoff.
	OutcomeRequeued Outcome = iota
	// OutcomeFailed: the job (and, for transcode, the clip) is failed.
	OutcomeFailed
)

// FailKind classifies a bad ending (§5, §11).
type FailKind int

const (
	// FailPermanent: the file is at fault → failed immediately, no counter.
	FailPermanent FailKind = iota
	// FailCrash: panic, or found running at boot without a checkpoint →
	// attempts++, backoff min(30s·2^(n−1), 10m), failed at MaxAttempts.
	FailCrash
	// FailTransient: storage/scratch/DB/io → transient_failures++, backoff
	// min(1m·2^(n−1), 30m), failed at MaxTransient.
	FailTransient
)

// Limits carries the two caps from config.TranscodeConfig. Zero values fall
// back to the §4 defaults (3 and 10) so a zero Limits never fails a job on
// its first bad ending.
type Limits struct {
	MaxAttempts, MaxTransient int
}

// NotifyMaxAttempts is the delivery-attempt cap of a notify job. It is a
// CONSTANT, not config: TRANSCODE_MAX_ATTEMPTS is a knob about how often an
// operator is willing to re-run an ENCODE on a file that may be what crashes
// the worker, and it has no business also deciding how many times a chat
// message is retried. Three attempts — immediate, +30s, +60s — is the budget;
// after that the job is `failed` with the reason in its row and the clip,
// which was never touched, stays exactly as ready as it was.
const NotifyMaxAttempts = 3

// HookMaxAttempts is the delivery-attempt cap of a `hook` job (§20 erratum
// 319), a constant for NotifyMaxAttempts' reason. Five — immediately, then
// +30s, +60s, +120s, +240s on the crash backoff — because the receiver is the
// operator's own automation, which is restarted more often than Discord is
// down, and a delivery that waits out a seven-minute restart is worth more
// than one that does not. After that the job is `failed`, logged at WARN, and
// the clip is exactly as ready as it was.
const HookMaxAttempts = 5

// TrimMaxAttempts is the CRASH-class cap of a trim job (§17 #5) — a panic, or
// a job found `running` at boot with no checkpoint — and it is a constant for
// the same reason NotifyMaxAttempts is: TRANSCODE_MAX_ATTEMPTS is a knob
// about the encode a clip cannot exist without, and a trim is an edit of a
// clip that is already ready and already playable. Two is the budget — one
// retry — because a trim that fails leaves NOTHING broken: the clip keeps the
// rendition it had, and the owner can ask again from the share page, which is
// a better retry than a machine guessing.
//
// It does NOT bound the ENVIRONMENT counter (§20 erratum 89). That argument
// is about the FILE and the owner, and §11 defines a transient failure as
// storage/scratch/DB/io — by construction not the file's fault, and not
// something asking again fixes any better than waiting does. A trim therefore
// gets the same TRANSCODE_MAX_TRANSIENT budget every other kind gets, so the
// MinIO restart that costs an encode nothing does not permanently fail an
// edit sixty seconds in.
const TrimMaxAttempts = 2

// limitsFor pins the caps for a job of this kind, so no caller can hand a
// non-transcode job the transcode budget by passing the config Limits it
// happens to be holding (ReconcileOnStartup does exactly that).
//
// The media kinds split the two counters (§20 erratum 89): the crash cap is
// their own constant — an edit that keeps crashing is what "ask again" is the
// right answer to — while the environment cap is the instance's configured
// TRANSCODE_MAX_TRANSIENT, because an outage is not an argument about the
// file and does not become one because the job is a trim.
func limitsFor(kind string, lim Limits) Limits {
	lim = withDefaults(lim)
	switch kind {
	case KindTranscode:
		return lim
	case KindTrim:
		return Limits{MaxAttempts: TrimMaxAttempts, MaxTransient: lim.MaxTransient}
	case KindGIF:
		return Limits{MaxAttempts: GIFMaxAttempts, MaxTransient: lim.MaxTransient}
	case KindSocial:
		return Limits{MaxAttempts: SocialMaxAttempts, MaxTransient: lim.MaxTransient}
	case KindReplace:
		return Limits{MaxAttempts: ReplaceMaxAttempts, MaxTransient: lim.MaxTransient}
	case KindPoster:
		return Limits{MaxAttempts: PosterMaxAttempts, MaxTransient: lim.MaxTransient}
	case KindFilmstrip:
		return Limits{MaxAttempts: FilmstripMaxAttempts, MaxTransient: lim.MaxTransient}
	case KindFull:
		return Limits{MaxAttempts: FullMaxAttempts, MaxTransient: lim.MaxTransient}
	case KindHook:
		return Limits{MaxAttempts: HookMaxAttempts, MaxTransient: HookMaxAttempts}
	default:
		return Limits{MaxAttempts: NotifyMaxAttempts, MaxTransient: NotifyMaxAttempts}
	}
}

// withDefaults fills the §4 defaults in for a zero cap, so a zero Limits
// never fails a job on its first bad ending.
func withDefaults(lim Limits) Limits {
	if lim.MaxAttempts <= 0 {
		lim.MaxAttempts = defaultMaxAttempts
	}
	if lim.MaxTransient <= 0 {
		lim.MaxTransient = defaultMaxTransient
	}
	return lim
}

// Cap reports the counter cap a job of this kind runs under for one class of
// bad ending — the very number FailAttempt compares against — so a log line
// can name the budget that was actually exhausted instead of the other one.
func Cap(kind string, fk FailKind, lim Limits) int {
	l := limitsFor(kind, lim)
	if fk == FailTransient {
		return l.MaxTransient
	}
	return l.MaxAttempts
}

const (
	defaultMaxAttempts  = 3
	defaultMaxTransient = 10

	crashBackoffBase     = 30 * time.Second
	crashBackoffCap      = 10 * time.Minute
	transientBackoffBase = time.Minute
	transientBackoffCap  = 30 * time.Minute
)

// Backoff returns the delay before the n-th retry (n ≥ 1) for kind:
// min(base·2^(n−1), cap). FailPermanent → 0.
func Backoff(kind FailKind, n int) time.Duration {
	var base, limit time.Duration
	switch kind {
	case FailCrash:
		base, limit = crashBackoffBase, crashBackoffCap
	case FailTransient:
		base, limit = transientBackoffBase, transientBackoffCap
	default:
		return 0
	}
	if n < 1 {
		n = 1
	}
	shift := n - 1
	if shift > 20 {
		shift = 20
	}
	d := base << uint(shift)
	if d <= 0 || d > limit {
		return limit
	}
	return d
}

// ReadyFields is everything FinalizeTranscode writes onto the clip.
type ReadyFields struct {
	RenditionKey              string
	RenditionSize             int64
	RenditionETag             string
	PosterKey                 string
	PosterSize                int64
	PosterETag                string
	PosterWidth, PosterHeight int
	Width, Height             int
	DurationMS                int64
	FPSNum, FPSDen            int
	HasAudio                  bool

	// SourceDurationMS is the ORIGINAL's container duration as this run
	// probed it, or nil when the source declared none. It is the timeline
	// every trim window is measured on (§20 erratum 68), so both writers fill
	// it: a transcode from its source probe, a trim from its own — the trim
	// re-probes the original on every run, which is what fills the column in
	// for rows that predate migration 0003.
	SourceDurationMS *int64

	// QueueSocial is the one field here that is an INSTRUCTION rather than a
	// column: "this rendition needs a dedicated social file, so queue the job
	// for it in this same commit" (§17 #3, §20 erratum 121). It rides on
	// ReadyFields because both writers of a rendition already carry one —
	// FinalizeTranscode and SwapRendition — and the skip rule it encodes is
	// decided from this very struct (the rendition's size and frame rate)
	// plus SOCIAL_MAX_BYTES, which is the worker's to know and not db's.
	// Adding a parameter instead would have changed two signatures and every
	// test that calls them to say something one field already says.
	QueueSocial bool

	// QueueHook is the clip webhook's instruction (§20 erratum 319): "queue
	// one `hook` job in this same commit". ONLY FinalizeTranscode reads it —
	// the webhook fires once per clip, when it FIRST becomes ready, so the
	// rendition swaps of a trim or a replacement (SwapRendition, SwapSource)
	// ignore it. It is independent of the notify bool and of the clip's
	// unlisted flag: an unlisted clip is sent, marked as such.
	QueueHook bool

	// PosterSeconds is the owner's chosen card-image frame, on the ORIGINAL's
	// timeline, carried through a rendition swap (§20 erratum 130). ONLY
	// SwapRendition reads it: a transcode renders the whole upload of a clip
	// that has never been ready, so it has no choice to carry, and
	// FinalizeTranscode does not write the column at all. nil means "no
	// choice survives this run" — the automatic frame — which is what a trim
	// records when the chosen moment falls outside the new cut.
	PosterSeconds *float64

	// Tiers is the quality-tier half of a commit (§20 errata 200–206). Every
	// writer of a rendition fills it: a transcode and a replacement from their
	// own source probe, a trim from its re-probe of the retained original. It
	// carries columns AND one instruction, exactly as QueueSocial does.
	Tiers TierFields

	// Peaks is an audio clip's waveform as transcode.EncodePeaks writes it
	// (migration 0022, §20 erratum 329): the bars its poster and its audiogram
	// were drawn from, which the share page draws its own progress over. Only
	// FinalizeTranscode reads it, and "" is NULL — every video, and an audio row
	// committed before the waveform existed.
	Peaks string

	// Light is the LIGHT copy this run produced ITSELF, or the zero value when
	// it produced none. Since card-first ordering the transcode run makes the
	// card's file before it flips `ready` (§20 erratum 206), so the social_*
	// columns are written by the same commit as the rendition — and a zero
	// value NULLs them, which is exactly right for a run whose playable
	// original or Full is the card file. Only FinalizeTranscode and SwapSource
	// read it; a trim's Light is still a job of its own, because a trim commits
	// a rendition somebody is already watching and must not wait on one.
	Light SocialFields
}

// TierFields is what a rendition commit says about the clip's tiers.
//
// It is a struct of its own rather than nine more fields on ReadyFields
// because the three commits that write it (FinalizeTranscode, SwapRendition,
// SwapSource) write it as a unit while the fourth (SetFull) writes only its own
// two words: a Full landing later must not be able to overwrite the ORIGINAL's
// measurements with nothing.
type TierFields struct {
	// The upload's own facts, from the probe of THIS run's source.
	OriginalPlayable, OriginalRemuxed bool
	OriginalWidth, OriginalHeight     int
	OriginalFPSNum, OriginalFPSDen    int
	OriginalCodec                     string

	// FullState/FullCodec and LightState are the two tier states the commit
	// settles. A commit that produced the Full itself writes TierReady; one
	// that queued a `full` job writes TierEncoding; one whose original plays
	// writes TierSkipped.
	FullState, FullCodec string
	LightState           string

	// QueueFull is the instruction: "the original cannot be played, and the
	// faithful copy has NOT been made in this run — queue the `full` job in
	// this same commit". It rides here for QueueSocial's reason exactly: the
	// clip cannot become ready without the work its better tier needs being
	// durably queued, and one field says what a second parameter on three
	// signatures would have said.
	QueueFull bool
}

func getJob(ctx context.Context, q querier, id int64) (Job, error) {
	return scanJob(q.QueryRowContext(ctx, "SELECT "+jobCols+" FROM jobs WHERE id = ?", id))
}

// ClaimJob atomically takes the oldest claimable TRANSCODE job (queued,
// run_after <= now, clip not deleting), marks it running and moves the clip
// queued → processing. ok=false when nothing is claimable.
//
// Kind-filtered since §20 erratum 67 (amended: three classes, not two). The
// queue is one queue per CLASS of work, one loop per class. A single FIFO over
// every kind put a blocked delivery — three 10s attempts against a blackholed
// Discord — in front of the encode of a clip somebody is waiting for, on the
// only loop TRANSCODE_CONCURRENCY=1 has; and one shared "everything else" loop
// then let a multi-minute trim do the same to every notify behind it.
func (d *DB) ClaimJob(ctx context.Context, now int64) (Job, Clip, bool, error) {
	return d.claimJob(ctx, now, "j.kind = ?", KindTranscode)
}

// MediaKinds are the kinds that run ffmpeg against a clip that is already
// `ready` (§17 #5, #6, #3, ROADMAP #31, #5). They share ONE loop of their own
// and the transcode concurrency semaphore, so a trim, a gif, a social
// rendition, a replacement and a poster never race each other for the scratch
// disk and never add an ffmpeg beyond TRANSCODE_CONCURRENCY.
func MediaKinds() []string {
	return []string{KindTrim, KindGIF, KindSocial, KindReplace, KindPoster, KindFilmstrip, KindFull}
}

// nonNotifyKinds are the kinds the notify loop must NOT claim: the media ones
// plus transcode. Everything else — `notify` today, an unrecognised kind
// tomorrow — belongs to it.
func nonNotifyKinds() []string { return append(MediaKinds(), KindTranscode) }

// ClaimJobOfKinds takes the oldest claimable job whose kind is one of kinds.
// It is the media loop's claim (MediaKinds), and it is an INCLUSION filter on
// purpose: those kinds are the ones bounded by the ffmpeg semaphore, and a
// kind nobody has taught the worker about must not silently join them.
func (d *DB) ClaimJobOfKinds(ctx context.Context, kinds []string, now int64) (Job, Clip, bool, error) {
	return d.claimJob(ctx, now, "j.kind IN "+placeholders(len(kinds)), toArgs(kinds)...)
}

// ClaimJobOtherThanKinds is the complement, and it is what the notify loop
// claims with (kinds = transcode + MediaKinds). Exclusion rather than
// `kind='notify'` is load-bearing: every kind is claimed by exactly one loop,
// so an unrecognised kind is still claimed — and then failed loudly by
// `refuseKind` — instead of sitting queued for ever.
func (d *DB) ClaimJobOtherThanKinds(ctx context.Context, kinds []string, now int64) (Job, Clip, bool, error) {
	return d.claimJob(ctx, now, "j.kind NOT IN "+placeholders(len(kinds)), toArgs(kinds)...)
}

// ClaimNotifyJob is the notify loop's claim: everything that is neither a
// transcode nor media work.
func (d *DB) ClaimNotifyJob(ctx context.Context, now int64) (Job, Clip, bool, error) {
	return d.ClaimJobOtherThanKinds(ctx, nonNotifyKinds(), now)
}

// ClaimMediaJob is the media loop's claim.
func (d *DB) ClaimMediaJob(ctx context.Context, now int64) (Job, Clip, bool, error) {
	return d.ClaimJobOfKinds(ctx, MediaKinds(), now)
}

// placeholders renders "(?, ?, ?)" — parentheses included — for an IN / NOT IN
// list. It is the package's ONE spelling of that idea; every caller passes a
// length it already bounded (a package-local kind list, or a slice the route
// capped at db.LibraryPageSize), and a zero-length list would be a
// programming error rather than a query — which is what the empty-string guard
// turns into a query that matches nothing instead of a syntax error.
func placeholders(n int) string {
	if n <= 0 {
		return "('')"
	}
	return "(?" + strings.Repeat(", ?", n-1) + ")"
}

func toArgs(kinds []string) []any {
	args := make([]any, len(kinds))
	for i, k := range kinds {
		args[i] = k
	}
	return args
}

func (d *DB) claimJob(ctx context.Context, now int64, kindClause string, kindArgs ...any) (Job, Clip, bool, error) {
	var job Job
	var clip Clip
	found := false
	err := d.tx(ctx, func(tx *sql.Tx) error {
		j, err := scanJob(tx.QueryRowContext(ctx, `
SELECT `+prefixed("j", jobCols)+`
FROM jobs j JOIN clips c ON c.id = j.clip_id
WHERE j.state = 'queued' AND `+kindClause+` AND j.run_after <= ? AND c.state <> 'deleting'
ORDER BY j.id LIMIT 1`, append(kindArgs, now)...))
		if err != nil {
			if err == ErrNotFound {
				return nil
			}
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE jobs SET state = 'running', started_at = ?, updated_at = ? WHERE id = ?", now, now, j.ID); err != nil {
			return err
		}
		if j.Kind == KindTranscode {
			if _, err := tx.ExecContext(ctx,
				"UPDATE clips SET state = 'processing', updated_at = ? WHERE id = ? AND state = 'queued'", now, j.ClipID); err != nil {
				return err
			}
		}
		if job, err = getJob(ctx, tx, j.ID); err != nil {
			return err
		}
		if clip, err = getClip(ctx, tx, j.ClipID); err != nil {
			return err
		}
		found = true
		return nil
	})
	if err != nil {
		return Job{}, Clip{}, false, err
	}
	return job, clip, found, nil
}

// prefixed rewrites "a, b, c" into "p.a, p.b, p.c" for joined selects.
func prefixed(p, cols string) string {
	out := make([]byte, 0, len(cols)+len(cols)/4)
	start := true
	for i := 0; i < len(cols); i++ {
		c := cols[i]
		switch {
		case c == ' ', c == '\n', c == '\t', c == ',':
			out = append(out, c)
			if c == ',' {
				start = true
			}
		default:
			if start {
				out = append(out, p...)
				out = append(out, '.')
				start = false
			}
			out = append(out, c)
		}
	}
	return string(out)
}

// SetJobStage persists a stage transition (progress fallback after restart).
//
// It is PINNED TO THE ATTEMPT (§20 erratum 291): startedAt is the claim stamp
// the run holds in its Job.StartedAt, and the write lands only while the row
// is still `running` under that very stamp. A row repaired meanwhile — an
// operator's `admin jobs unstick`, a second process's automatic repair, the
// boot reconcile — or cancelled by MarkDeleting, and perhaps claimed again by
// a NEWER attempt, is left alone and the caller gets ErrStateMismatch: the old
// attempt has lost the row and must not write its stage over the new
// attempt's. Two claims can never share a stamp, because an unstick needs at
// least 30 s of age and the stamp is in seconds. An unknown id is ErrNotFound.
func (d *DB) SetJobStage(ctx context.Context, jobID, startedAt int64, stage string, now int64) error {
	return d.attemptWrite(ctx, jobID,
		"UPDATE jobs SET stage = ?, updated_at = ? WHERE id = ? AND state = 'running' AND started_at = ?",
		stage, now, jobID, startedAt)
}

// SaveCheckpoint records that this attempt's outputs are uploaded and
// verified; from here on no code path deletes them (§10 step 8). Pinned to
// the attempt exactly as SetJobStage is (§20 erratum 291), and here the pin is
// what keeps a checkpoint honest: a checkpoint names the keys THIS attempt
// uploaded, and writing it onto a row a newer attempt now owns would hand that
// attempt's finalize somebody else's objects.
func (d *DB) SaveCheckpoint(ctx context.Context, jobID, startedAt int64, checkpointJSON string, now int64) error {
	return d.attemptWrite(ctx, jobID,
		"UPDATE jobs SET checkpoint_json = ?, updated_at = ? WHERE id = ? AND state = 'running' AND started_at = ?",
		checkpointJSON, now, jobID, startedAt)
}

// ClaimStamp is the claim's `started_at` as a holder of the Job reads it —
// the one value every attempt-pinned write is keyed on (§20 errata 291 and
// 293). ClaimJob always sets it, so a `running` row without one is already
// inconsistent: 0 comes back for that case and matches no row, which leaves
// such a row to the repair, and that is the point.
func ClaimStamp(j Job) int64 {
	if j.StartedAt == nil {
		return 0
	}
	return *j.StartedAt
}

// unpinned is the claim stamp the REPAIR passes (§20 erratum 293). The boot
// reconcile, the automatic repair and `admin jobs unstick` are the second hand
// by design — they end a row precisely because they have established that no
// run of theirs is holding it — so they are the one caller of these endings
// that is not pinned to an attempt, and a row whose `started_at` is NULL
// (already inconsistent, and exactly what a repair is for) stays repairable.
// It can never collide with a real stamp: ClaimJob stamps with a Unix second,
// which is never negative.
const unpinned = int64(-1)

// heldByAttempt reports whether the row a transaction has just read is still
// this attempt's to end. It is the Go half of attemptWrite's WHERE — `state =
// 'running' AND started_at = ?` — for the three endings that cannot be one
// statement, because they read the kind, the counters and the clip coupling
// out of the row before they decide what to write. A NULL `started_at` matches
// nothing, exactly as the SQL comparison would.
func heldByAttempt(j Job, startedAt int64) bool {
	if j.State != JobRunning {
		return false
	}
	if startedAt == unpinned {
		return true
	}
	return j.StartedAt != nil && *j.StartedAt == startedAt
}

// attemptWrite runs one UPDATE whose WHERE pins the row to a running attempt
// (`id = ? AND state = 'running' AND started_at = ?`) and, when no row
// matched, says why: ErrNotFound when the id is gone, ErrStateMismatch when
// the row exists but is no longer that attempt's. The lookup runs only on the
// miss, so the common path is still the one statement it was.
func (d *DB) attemptWrite(ctx context.Context, jobID int64, query string, args ...any) error {
	n, err := d.exec(ctx, query, args...)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	var one int
	switch err := d.ro.QueryRowContext(ctx, "SELECT 1 FROM jobs WHERE id = ?", jobID).Scan(&one); {
	case err == sql.ErrNoRows:
		return ErrNotFound
	case err != nil:
		return err
	}
	return ErrStateMismatch
}

// FinalizeTranscode flips the clip processing → ready with every measured
// field and marks the job done, in one transaction. It returns false (and
// changes nothing) when the clip is no longer processing — tombstoned or
// deleted meanwhile — so the caller deletes its own outputs (§11).
//
// A transcode renders the WHOLE original, so it also settles the trim
// contract for the row: `source_duration_ms` is filled from the source probe
// (COALESCE, so a probe that measured nothing leaves an older value alone) and
// `trim_start_ms`/`trim_end_ms` go back to NULL — "this clip is the whole
// upload", which is exactly what a fresh or retried encode produces.
//
// notify is "the operator configured a Discord webhook" (§17 #4). When it is
// true and the clip is LISTED, the notify job is inserted inside this same
// transaction: the ready flip and the intent to announce it are one commit,
// so a crash between them is impossible — either the clip is ready and the
// announcement is queued, or neither happened. db knows nothing about the
// webhook itself; the caller passes the one bit, and the UNLISTED half of the
// rule is read from the row here (the owner may have flipped it during the
// encode, and the row at commit time is the one that counts).
// alreadyApplied is the THIRD answer (§20 erratum 102). `ok == false` used to
// carry two incompatible meanings: "the clip was tombstoned" and "this very
// attempt already committed, and you are looking at your own second call".
// The caller's response to the first is to delete the objects it uploaded;
// doing that for the second deletes the objects the ready row now points at.
// The two are told apart by re-reading the clip in the SAME transaction: a row
// that is `ready` and references exactly this attempt's rendition and poster
// keys can only have been written by this attempt, so alreadyApplied is true,
// ok is false (this call changed nothing) and the caller must keep its
// outputs. The retry loop that makes this reachable is real — a commit that
// lands and is still reported as an error — even though it could not be
// provoked against modernc.
//
// routing is the optional per-tag rule set (ROADMAP #26, §20 erratum 159), at
// most one value: WHICH webhook this clip's announcement goes to is decided
// here, inside the commit, from the tags the clip has AT THAT MOMENT, and the
// answer is stored in the job's params_json. A later tag edit therefore never
// re-routes a post that is already queued, and the worker never has to guess.
// It is variadic so that every existing caller — and every test — keeps
// compiling unchanged; a caller that passes nothing gets the single-webhook
// behaviour byte for byte.
func (d *DB) FinalizeTranscode(ctx context.Context, jobID int64, clipID string, f ReadyFields, notify bool, now int64, routing ...NotifyRouting) (ok, alreadyApplied bool, err error) {
	err = d.tx(ctx, func(tx *sql.Tx) error {
		n, err := affected(tx.ExecContext(ctx, `
UPDATE clips SET state = 'ready', failure_reason = NULL,
  rendition_key = ?, rendition_size = ?, rendition_etag = ?,
  poster_key = ?, poster_size = ?, poster_etag = ?, poster_width = ?, poster_height = ?,
  width = ?, height = ?, duration_ms = ?, fps_num = ?, fps_den = ?, has_audio = ?,
  social_key = ?, social_size = ?, social_etag = ?, social_width = ?, social_height = ?,
  social_fps_num = ?, social_fps_den = ?,
  original_playable = ?, original_remuxed = ?, original_width = ?, original_height = ?,
  original_fps_num = ?, original_fps_den = ?, original_codec = ?,
  full_state = ?, full_codec = ?, light_state = ?,
  source_duration_ms = COALESCE(?, source_duration_ms), trim_start_ms = NULL, trim_end_ms = NULL,
  peaks = ?,
  ready_at = ?, updated_at = ?
WHERE id = ? AND state = 'processing'`,
			emptyNull(f.RenditionKey), zeroNull(f.RenditionSize), emptyNull(f.RenditionETag),
			f.PosterKey, f.PosterSize, f.PosterETag, f.PosterWidth, f.PosterHeight,
			// zeroNull: an audio clip has no picture of its own, and its four are
			// NULL rather than a zero anybody could read as a size (erratum 328).
			// A video's are always positive, so nothing about one changes.
			zeroNull(int64(f.Width)), zeroNull(int64(f.Height)), f.DurationMS,
			zeroNull(int64(f.FPSNum)), zeroNull(int64(f.FPSDen)), boolInt(f.HasAudio),
			emptyNull(f.Light.Key), zeroNull(f.Light.Size), emptyNull(f.Light.ETag),
			zeroNull(int64(f.Light.Width)), zeroNull(int64(f.Light.Height)),
			zeroNull(int64(f.Light.FPSNum)), zeroNull(int64(f.Light.FPSDen)),
			boolInt(f.Tiers.OriginalPlayable), boolInt(f.Tiers.OriginalRemuxed),
			zeroNull(int64(f.Tiers.OriginalWidth)), zeroNull(int64(f.Tiers.OriginalHeight)),
			zeroNull(int64(f.Tiers.OriginalFPSNum)), zeroNull(int64(f.Tiers.OriginalFPSDen)),
			f.Tiers.OriginalCodec,
			f.Tiers.FullState, f.Tiers.FullCodec, f.Tiers.LightState,
			nullInt64(f.SourceDurationMS),
			emptyNull(f.Peaks),
			now, now, clipID))
		if err != nil {
			return err
		}
		if n == 0 {
			mine, err := attemptAlreadyApplied(ctx, tx, clipID, f)
			if err != nil {
				return err
			}
			alreadyApplied = mine
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE jobs SET state = 'done', stage = 'done', finished_at = ?, updated_at = ? WHERE id = ?",
			now, now, jobID); err != nil {
			return err
		}
		if notify {
			// `routed` is "the caller expressed a routing policy at all", which
			// is not the same as "there are rules": a caller that passes nothing
			// — every caller before ROADMAP #26, and every test — gets the
			// single-webhook behaviour, while one that passes an empty policy
			// with DefaultConfigured false is saying "there is nowhere to post
			// an unmatched clip", and is obeyed.
			var route NotifyRouting
			routed := len(routing) > 0
			if routed {
				route = routing[0]
			}
			if err := enqueueNotifyTx(ctx, tx, clipID, now, route, routed); err != nil {
				return err
			}
		}
		// The clip webhook (§20 erratum 319), in the same commit for the
		// notify job's reason: with the webhook configured, a clip cannot
		// become ready without its delivery being durably queued. Nothing of
		// the URL or the key is stored — params_json is `{}`.
		if f.QueueHook {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO jobs (clip_id, kind, state, stage, params_json, run_after, created_at, updated_at)
VALUES (?, ?, 'queued', ?, '{}', ?, ?, ?)`, clipID, KindHook, JobStageQueued, now, now, now); err != nil {
				return err
			}
		}
		// The social rendition is queued AFTER the ready flip and in the same
		// commit, for the notify job's reason: a clip cannot become ready
		// without the work its card needs being durably queued. The job
		// touches no clip column, so a crash before it runs — or a failure
		// once it does — leaves a perfectly ordinary ready clip whose
		// /social.mp4 serves the rendition (§17 #3, §20 erratum 121).
		if f.QueueSocial {
			if _, _, err := enqueueSocialTx(ctx, tx, clipID, now); err != nil {
				return err
			}
		}
		// …and the FULL tier, on exactly the same terms and for the same reason
		// (§20 erratum 206). The card-first ordering is what makes this a
		// separate job at all: the transcode run produced the LIGHT copy, so the
		// clip is card-ready NOW, and the faithful copy of a source no browser
		// can play is queued behind it rather than in front of it.
		if f.Tiers.QueueFull {
			if _, _, err := enqueueFullTx(ctx, tx, clipID, now); err != nil {
				return err
			}
		}
		ok = true
		return nil
	})
	if err != nil {
		return false, false, err
	}
	return ok, alreadyApplied, nil
}

// attemptAlreadyApplied reports whether the clip is READY and already
// references the objects of this attempt — the signature of a commit this same
// run made and was told had failed (§20 erratum 102). A missing clip, a
// tombstone and a row holding somebody else's keys all answer false, which is
// the genuine lost race.
//
// The POSTER key decides it. It is attempt-scoped (`clips/{id}/{attempt}/…`,
// §7), it is never empty — every commit that flips `ready` writes one — and no
// other run and no other attempt of this run can produce it, so a `ready` row
// carrying it can only have been written by this attempt's own commit.
//
// The RENDITION key is compared too, but only when this attempt HAS one (§20
// erratum 201, amended). Since the quality tiers a commit legitimately leaves
// that column NULL — the remux fast path publishes no second object, and a
// card-first commit publishes the LIGHT copy and queues the Full behind it —
// and requiring `c.RenditionKey != nil` there classified both of those as lost
// races: the caller then deleted the poster (and the Light copy) the row it had
// just written was pointing at, which is precisely the §12 drift erratum 102
// exists to prevent. An absent want is therefore not evidence in either
// direction, and it must not be read as one: the column can also hold a LATER
// job's key by the time the retry runs, because `db.SetFull` writes exactly
// this column on a clip that is already ready.
func attemptAlreadyApplied(ctx context.Context, tx *sql.Tx, clipID string, f ReadyFields) (bool, error) {
	c, err := getClip(ctx, tx, clipID)
	switch {
	case errors.Is(err, ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return c.State == ClipReady &&
		c.PosterKey != nil && *c.PosterKey == f.PosterKey &&
		sameAttemptKey(c.RenditionKey, f.RenditionKey) &&
		sameAttemptKey(c.SocialKey, f.Light.Key), nil
}

// sameAttemptKey compares one of an attempt's optional keys against the column
// that would hold it. An empty want is a tier this commit did not write, and the
// column then says nothing about whose commit this row is; see
// attemptAlreadyApplied, whose poster comparison is what actually decides.
func sameAttemptKey(got *string, want string) bool {
	if want == "" {
		return true
	}
	return got != nil && *got == want
}

// NotifyRouting is the per-tag rule set at the moment a clip becomes ready
// (ROADMAP #26, §20 erratum 159). Rules are walked IN ORDER and the first one
// whose tag the clip carries wins, so priority is the operator's configured
// order and not the alphabet the tags happen to sort in.
//
// DefaultConfigured is "…and if nothing matched, there is still somewhere to
// post it": false means an unmatched clip is announced nowhere and no job is
// queued at all (DISCORD_WEBHOOK_URL unset, or DISCORD_WEBHOOK_UNMATCHED=skip).
// db knows nothing about a webhook URL; it is handed rules made of tags and a
// bool, and it stores a tag.
type NotifyRouting struct {
	Rules             []NotifyRule
	DefaultConfigured bool
}

// NotifyRule is one tag→route-name rule. Name is what goes in params_json and
// in a log line; it is never a URL.
type NotifyRule struct{ Tag, Name string }

// notifyParams is a notify job's params_json. An EMPTY params (`{}`) means the
// default webhook, which is what every job written before routing existed
// carries and what an unrouted clip still gets — so no migration and no
// backfill were needed for this feature.
type notifyParams struct {
	Route string `json:"route,omitempty"`
}

// NotifyRouteOf reads the route a notify job was queued for out of its
// params_json. An empty params, an absent key or an unparseable value all
// mean the default webhook: a job row is not a place to fail over.
func NotifyRouteOf(paramsJSON string) string {
	var p notifyParams
	if err := json.Unmarshal([]byte(paramsJSON), &p); err != nil {
		return ""
	}
	return p.Route
}

// enqueueNotifyTx queues the §17 #4 announcement for a clip that has just
// become ready — unless the clip is UNLISTED, which is the per-upload opt-out:
// unlisted means "do not surface this" (§8.1 hides it from the shared
// library), and auto-posting it to a channel would be exactly the surfacing
// the uploader declined. That is why the feature needs no new API surface.
//
// The ROUTE is chosen here, in the same commit, from the tags the clip has at
// this instant (ROADMAP #26). With no rules — the single-webhook deployment —
// the query below is not run at all and the row is written exactly as it
// always was.
func enqueueNotifyTx(ctx context.Context, tx *sql.Tx, clipID string, now int64, routing NotifyRouting, routed bool) error {
	var unlisted int64
	if err := tx.QueryRowContext(ctx, "SELECT unlisted FROM clips WHERE id = ?", clipID).Scan(&unlisted); err != nil {
		return err
	}
	if unlisted != 0 {
		return nil
	}
	params := "{}"
	if routed {
		name := ""
		if len(routing.Rules) > 0 {
			var err error
			if name, err = matchNotifyRouteTx(ctx, tx, clipID, routing.Rules); err != nil {
				return err
			}
		}
		switch {
		case name != "":
			b, err := json.Marshal(notifyParams{Route: name})
			if err != nil { // unreachable: one short string
				return err
			}
			params = string(b)
		case !routing.DefaultConfigured:
			// Nothing matched and there is nowhere to fall back to — no
			// DISCORD_WEBHOOK_URL, or DISCORD_WEBHOOK_UNMATCHED=skip. The
			// operator asked for tagged clips only (and, with no rules at all,
			// for nothing at all). No job, so no client is ever asked to
			// deliver one.
			return nil
		}
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO jobs (clip_id, kind, state, stage, params_json, run_after, created_at, updated_at)
VALUES (?, 'notify', 'queued', ?, ?, ?, ?, ?)`, clipID, JobStageQueued, params, now, now, now)
	return err
}

// matchNotifyRouteTx returns the name of the first rule whose tag the clip
// carries, or "" for no match. It is ONE query — the whole tag set of one clip
// is at most MaxTagsPerClip rows off the primary key — walked in RULE order,
// because the operator's order is the priority they wrote down.
func matchNotifyRouteTx(ctx context.Context, tx *sql.Tx, clipID string, rules []NotifyRule) (string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT tag FROM clip_tags WHERE clip_id = ?", clipID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	has := map[string]bool{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return "", err
		}
		has[t] = true
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	for _, r := range rules {
		if has[r.Tag] {
			return r.Name, nil
		}
	}
	return "", nil
}

// CompleteJob marks a running job done WITHOUT touching its clip — the §5
// rule that only `transcode` moves clips.state — and refuses a transcode job
// outright so that rule cannot be broken by a mistaken call. note (a delivery
// result or a skip reason) is stored in last_error, which for a non-transcode
// kind is an operator-facing record and never reaches the API.
//
// It is PINNED TO THE ATTEMPT (§20 erratum 293), exactly as SetJobStage is:
// startedAt is the claim stamp the run holds in its Job.StartedAt, and the
// write lands only while the row is still `running` under that very stamp. A
// row a repair took meanwhile — and perhaps a NEWER attempt claimed — is left
// alone with ErrStateMismatch, because marking it `done` here would end the
// new attempt's row on the old attempt's word. A transcode job, a row that is
// not running and a row that is another attempt's are all ErrStateMismatch
// (they are all "not this call's row to end"); a gone id is ErrNotFound.
func (d *DB) CompleteJob(ctx context.Context, jobID, startedAt int64, stage, note string, now int64) error {
	return d.attemptWrite(ctx, jobID, `
UPDATE jobs SET state = 'done', stage = ?, last_error = ?, finished_at = ?, updated_at = ?
WHERE id = ? AND kind <> ? AND state = 'running' AND started_at = ?`,
		stage, sql.NullString{String: note, Valid: note != ""}, now, now, jobID, KindTranscode, startedAt)
}

// JobKindCounts counts every job row by kind. `clippet admin stats` prints it
// so an operator (and E2E step 20) can see at a glance whether any notify job
// exists at all — the observable that proves DISCORD_WEBHOOK_URL is off.
func (d *DB) JobKindCounts(ctx context.Context) (map[string]int, error) {
	rows, err := d.ro.QueryContext(ctx, "SELECT kind, COUNT(*) FROM jobs GROUP BY kind")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, err
		}
		out[kind] = n
	}
	return out, rows.Err()
}

// FailAttempt is THE ONLY place attempts / transient_failures are incremented.
// It applies the §5 rule for kind, computes run_after, and couples the clip
// state for transcode jobs (processing → queued or failed).
//
// The job must be running AND STILL THIS ATTEMPT'S (§20 erratum 293):
// startedAt is the claim stamp the run holds, and a row that a repair took
// meanwhile — perhaps already re-claimed by a newer attempt — is left
// untouched with ErrStateMismatch. Spending a counter there would spend the
// NEW attempt's budget for the old attempt's ending, and at the cap it would
// fail a clip whose encode is running. A missing id is ErrNotFound, and any
// other state (e.g. cancelled by MarkDeleting while the worker was finishing)
// is ErrStateMismatch with nothing changed — every caller treats both as an
// ending that is not its to record, never as an error.
func (d *DB) FailAttempt(ctx context.Context, jobID, startedAt int64, clipID, reason string, kind FailKind, lim Limits, now int64) (Outcome, error) {
	return d.FailAttemptAfter(ctx, jobID, startedAt, clipID, reason, kind, lim, now, 0)
}

// FailAttemptAfter is FailAttempt with a FLOOR under the requeue delay: the
// job is scheduled no earlier than now+minDelay. It exists for the one case
// where the far end tells us when to come back — a Discord 429's Retry-After
// (§17 #4) — and is ignored when the outcome is `failed` (nothing is
// rescheduled) or when the computed backoff is already longer.
func (d *DB) FailAttemptAfter(ctx context.Context, jobID, startedAt int64, clipID, reason string, kind FailKind, lim Limits, now int64, minDelay time.Duration) (Outcome, error) {
	var out Outcome
	err := d.tx(ctx, func(tx *sql.Tx) (err error) {
		out, err = failAttemptTx(ctx, tx, jobID, startedAt, clipID, reason, kind, lim, now, minDelay)
		return err
	})
	return out, err
}

func failAttemptTx(ctx context.Context, tx *sql.Tx, jobID, startedAt int64, clipID, reason string, kind FailKind, lim Limits, now int64, minDelay time.Duration) (Outcome, error) {
	j, err := getJob(ctx, tx, jobID)
	if err != nil {
		return 0, err
	}
	if !heldByAttempt(j, startedAt) {
		return 0, ErrStateMismatch
	}
	// limitsFor fills the §4 defaults in and then pins the kind's own caps.
	lim = limitsFor(j.Kind, lim)
	attempts, transient := j.Attempts, j.TransientFailures
	final := false
	var backoff time.Duration
	switch kind {
	case FailPermanent:
		final = true
	case FailCrash:
		attempts++
		final = attempts >= lim.MaxAttempts
		backoff = Backoff(FailCrash, attempts)
	case FailTransient:
		transient++
		final = transient >= lim.MaxTransient
		backoff = Backoff(FailTransient, transient)
	}
	if final {
		if _, err := tx.ExecContext(ctx, `
UPDATE jobs SET state = 'failed', attempts = ?, transient_failures = ?, last_error = ?, finished_at = ?, updated_at = ?
WHERE id = ?`, attempts, transient, reason, now, now, jobID); err != nil {
			return 0, err
		}
		if j.Kind == KindTranscode {
			if _, err := tx.ExecContext(ctx,
				"UPDATE clips SET state = 'failed', failure_reason = ?, updated_at = ? WHERE id = ? AND state = 'processing'",
				reason, now, clipID); err != nil {
				return 0, err
			}
		}
		// THE TIER THIS JOB WAS PRODUCING, settled in the same transaction as its
		// `failed` (§20 erratum 236). It used to be written only by the worker's
		// own exit path (worker/social.go, worker/full.go), and there are three
		// endings with no worker to run one: the automatic repair
		// (JOBS_UNSTICK_AFTER), the boot reconcile, and `admin jobs unstick`. Each
		// of them ends the job through repairRunningJobTx → here, so a `social` or
		// `full` job repaired to its cap left `light_state`/`full_state` reading
		// `encoding` FOR EVER — which the DTO, both progress routes and the watch
		// page all repeat, and which put a `data-progress-url` on a ready page that
		// then polled /progress for twenty minutes on every single view.
		//
		// It is written here rather than left to the caller because this is the one
		// function that knows a job has ended for good, and because the tier column
		// and the job row have to move together: a crash between two statements is
		// exactly the stuck state this fixes.
		//
		// It is GUARDED, by settleTierSQL: the column must still say `encoding` and
		// nothing of this kind may be live (§20 erratum 237). Unguarded it could
		// write `failed` over a tier that EXISTS — a stale `full` job repaired after
		// a trim published a Full of its own would bury `full_state = ready` with the
		// key still set, and the quality selector would then refuse to offer a file
		// the bucket holds. The job UPDATE above has already written `failed`, so
		// this job is never the live job the term looks for.
		if column, ok := tierStateColumnOfKind(j.Kind); ok {
			if _, err := tx.ExecContext(ctx, settleTierSQL(column),
				TierFailed, now, clipID, TierEncoding, clipID, j.Kind); err != nil {
				return 0, err
			}
		}
		return OutcomeFailed, nil
	}
	if minDelay > backoff {
		backoff = minDelay
	}
	runAfter := now + int64(backoff/time.Second)
	if _, err := tx.ExecContext(ctx, `
UPDATE jobs SET state = 'queued', stage = ?, attempts = ?, transient_failures = ?, last_error = ?, run_after = ?, updated_at = ?, started_at = NULL
WHERE id = ?`, JobStageQueued, attempts, transient, reason, runAfter, now, jobID); err != nil {
		return 0, err
	}
	if j.Kind == KindTranscode {
		if _, err := tx.ExecContext(ctx,
			"UPDATE clips SET state = 'queued', updated_at = ? WHERE id = ? AND state = 'processing'", now, clipID); err != nil {
			return 0, err
		}
	}
	return OutcomeRequeued, nil
}

// RequeueWithBackoff returns a running job to `queued` after delay with BOTH
// counters untouched and reason in last_error. It is FailAttempt's sibling for
// the one bad ending that must not spend a budget: a scratch shortage (§20
// erratum 90). That ending is transient by nature, but COUNTING it lets a disk
// which stays full for TRANSCODE_MAX_TRANSIENT backoffs turn every queued clip
// into a PERMANENT failure — for a condition an operator fixes by freeing
// bytes, never by re-uploading, and whose reason would then be a public
// sentence about the server's free space.
//
// ReleaseJob cannot serve here: it schedules the retry at `now`, which against
// a full disk is a spin. The clip goes processing → queued for a transcode
// job, exactly as FailAttempt's requeue does. Pinned to the attempt exactly as
// FailAttempt is (§20 erratum 293): not this attempt's row → ErrStateMismatch,
// nothing changed; gone → ErrNotFound. Requeueing a row a newer attempt holds
// would take that attempt's encode off the queue while it is still running.
func (d *DB) RequeueWithBackoff(ctx context.Context, jobID, startedAt int64, clipID, reason string, delay time.Duration, now int64) error {
	if delay < 0 {
		delay = 0
	}
	return d.tx(ctx, func(tx *sql.Tx) error {
		j, err := getJob(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if !heldByAttempt(j, startedAt) {
			return ErrStateMismatch
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE jobs SET state = 'queued', stage = ?, last_error = ?, run_after = ?, updated_at = ?, started_at = NULL
WHERE id = ?`, JobStageQueued, reason, now+int64(delay/time.Second), now, jobID); err != nil {
			return err
		}
		if j.Kind == KindTranscode {
			if _, err := tx.ExecContext(ctx,
				"UPDATE clips SET state = 'queued', updated_at = ? WHERE id = ? AND state = 'processing'", now, clipID); err != nil {
				return err
			}
		}
		return nil
	})
}

// ReleaseJob returns a running job to queued (stage "queued") with run_after
// = now and BOTH counters untouched (graceful shutdown, checkpointed
// restart); checkpoint_json is kept so the retry is finalize-only. The clip
// goes processing → queued for transcode jobs. Pinned to the attempt exactly
// as FailAttempt is (§20 erratum 293): a row that is not this attempt's is
// ErrStateMismatch with nothing changed, because releasing a row a newer
// attempt holds would queue a second claim of a job that is running — and
// would keep a checkpoint naming the OLD attempt's objects alive for it. A
// gone id is ErrNotFound.
//
// The repair passes `unpinned`: it is the second hand by design and has
// already established that the row is running and stale (§24.3, §24.6).
func (d *DB) ReleaseJob(ctx context.Context, jobID, startedAt int64, clipID string, now int64) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		return releaseJobTx(ctx, tx, jobID, startedAt, clipID, now)
	})
}

func releaseJobTx(ctx context.Context, tx *sql.Tx, jobID, startedAt int64, clipID string, now int64) error {
	j, err := getJob(ctx, tx, jobID)
	if err != nil {
		return err
	}
	if !heldByAttempt(j, startedAt) {
		return ErrStateMismatch
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE jobs SET state = 'queued', stage = ?, run_after = ?, updated_at = ?, started_at = NULL WHERE id = ?",
		JobStageQueued, now, now, jobID); err != nil {
		return err
	}
	if j.Kind == KindTranscode {
		if _, err := tx.ExecContext(ctx,
			"UPDATE clips SET state = 'queued', updated_at = ? WHERE id = ? AND state = 'processing'", now, clipID); err != nil {
			return err
		}
	}
	return nil
}

// CancelJob marks a queued/running job cancelled (the worker's lost-race
// path, §10.3 step 9), pinned to the attempt exactly as SetJobStage is (§20
// erratum 291): only a RUNNING row under the caller's own claim stamp is
// cancelled. It used to take `queued` too and to be silent over a row that had
// already ended; the pin makes it neither. Its one production caller is a run
// cancelling the claim it holds, so a row that ended under another hand —
// MarkDeleting cancelled it, a repair requeued it, a newer attempt claimed it —
// answers ErrStateMismatch, which the run logs as exactly that, and an unknown
// id ErrNotFound.
func (d *DB) CancelJob(ctx context.Context, jobID, startedAt int64, now int64) error {
	return d.attemptWrite(ctx, jobID,
		"UPDATE jobs SET state = 'cancelled', finished_at = ?, updated_at = ? WHERE id = ? AND state = 'running' AND started_at = ?",
		now, now, jobID, startedAt)
}

// HasLiveJob reports whether a clip has a job the worker may be running right
// now — `queued` or `running`, of ANY kind. It is the scratch janitor's answer
// to "does something own $SCRATCH_DIR/{id}" (§12, §20 erratum 89): a trim and
// a gif run against a clip that is `ready` for the whole run, so the clip's
// own state cannot say, and after a crash the job table is the only record
// left. Covered by the jobs_clip index.
func (d *DB) HasLiveJob(ctx context.Context, clipID string) (bool, error) {
	var one int
	err := d.ro.QueryRowContext(ctx,
		"SELECT 1 FROM jobs WHERE clip_id = ? AND state IN ('queued', 'running') LIMIT 1", clipID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// GetLatestJob returns the newest job of a clip, or ErrNotFound.
func (d *DB) GetLatestJob(ctx context.Context, clipID string) (Job, error) {
	return scanJob(d.ro.QueryRowContext(ctx,
		"SELECT "+jobCols+" FROM jobs WHERE clip_id = ? ORDER BY id DESC LIMIT 1", clipID))
}

// ReconcileOnStartup repairs state left by a crash, in one transaction:
// running jobs WITH a checkpoint are released (no counter); running jobs
// WITHOUT one get FailAttempt(MsgInterruptedByRestart, FailCrash); clips left
// processing with no running job are repaired from their latest transcode
// job so the result is always claimable or retryable, never stuck (§12):
// job queued → clip queued (requeued); job failed → clip failed with the
// job's last_error (failed); job done/cancelled → job and clip queued, the
// checkpoint kept so a checkpointed job retries finalize-only (requeued); no
// transcode job at all → clip failed "orphaned at boot" so the owner's retry
// creates one (failed).
func (d *DB) ReconcileOnStartup(ctx context.Context, lim Limits, now int64) (released, requeued, failed int, err error) {
	err = d.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT "+jobCols+" FROM jobs WHERE state = 'running' ORDER BY id")
		if err != nil {
			return err
		}
		var running []Job
		for rows.Next() {
			j, err := scanJob(rows)
			if err != nil {
				rows.Close()
				return err
			}
			running = append(running, j)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, j := range running {
			// The same per-job rule `admin jobs unstick` applies to one row by
			// hand (§20 erratum 146); one implementation, so the §5 coupling
			// cannot be written two different ways.
			res, err := repairRunningJobTx(ctx, tx, j, MsgInterruptedByRestart, lim, now)
			if err != nil {
				return err
			}
			switch res.Action {
			case UnstickReleased:
				released++
			case UnstickFailed:
				failed++
			default:
				requeued++
			}
		}
		orphans, err := orphanedProcessingClips(ctx, tx)
		if err != nil {
			return err
		}
		for _, clipID := range orphans {
			outcome, err := repairOrphanTx(ctx, tx, clipID, now)
			if err != nil {
				return err
			}
			if outcome == OutcomeFailed {
				failed++
			} else {
				requeued++
			}
		}
		return nil
	})
	if err != nil {
		return 0, 0, 0, err
	}
	return released, requeued, failed, nil
}

// orphanedProcessingClips lists clips in processing that no running TRANSCODE
// job backs. Every live transition couples job and clip in one transaction, so
// this is only reachable through hand edits or a future bug — the repair
// exists so boot never leaves such a clip stuck.
//
// The `kind` clause matters (§5): only a transcode job moves clips.state, so
// only a transcode job can be the thing that is supposed to finish a
// `processing` clip. A clip whose only running job is a `notify` left over
// from an earlier ready would otherwise be excluded from the repair and stay
// stuck in processing forever.
func orphanedProcessingClips(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT id FROM clips
WHERE state = 'processing' AND id NOT IN (SELECT clip_id FROM jobs WHERE state = 'running' AND kind = 'transcode')
ORDER BY id`)
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

// orphanReason is the failure_reason of a processing clip that has no
// transcode job to finish it.
const orphanReason = "orphaned at boot: no transcode job"

// repairOrphanTx completes the half-applied transition of one orphaned
// processing clip from its latest transcode job (see ReconcileOnStartup).
func repairOrphanTx(ctx context.Context, tx *sql.Tx, clipID string, now int64) (Outcome, error) {
	j, err := scanJob(tx.QueryRowContext(ctx,
		"SELECT "+jobCols+" FROM jobs WHERE clip_id = ? AND kind = 'transcode' ORDER BY id DESC LIMIT 1", clipID))
	if err != nil && err != ErrNotFound {
		return 0, err
	}
	setClip := func(state string, reason *string) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE clips SET state = ?, failure_reason = ?, updated_at = ? WHERE id = ? AND state = 'processing'",
			state, nullStr(reason), now, clipID)
		return err
	}
	switch {
	case err == ErrNotFound:
		reason := orphanReason
		return OutcomeFailed, setClip(ClipFailed, &reason)
	case j.State == JobFailed:
		reason := orphanReason
		if j.LastError != nil && *j.LastError != "" {
			reason = *j.LastError
		}
		return OutcomeFailed, setClip(ClipFailed, &reason)
	case j.State == JobQueued:
		return OutcomeRequeued, setClip(ClipQueued, nil)
	default: // done or cancelled: run it again (finalize-only when checkpointed)
		if _, err := tx.ExecContext(ctx, `
UPDATE jobs SET state = 'queued', stage = ?, run_after = ?, updated_at = ?, started_at = NULL, finished_at = NULL
WHERE id = ?`, JobStageQueued, now, now, j.ID); err != nil {
			return 0, err
		}
		return OutcomeRequeued, setClip(ClipQueued, nil)
	}
}

// QueueStats counts queued and running jobs (for /healthz).
func (d *DB) QueueStats(ctx context.Context) (queued, running int, err error) {
	err = d.ro.QueryRowContext(ctx,
		"SELECT COALESCE(SUM(state = 'queued'), 0), COALESCE(SUM(state = 'running'), 0) FROM jobs").
		Scan(&queued, &running)
	return queued, running, err
}
