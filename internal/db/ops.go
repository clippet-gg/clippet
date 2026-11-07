package db

// ops.go — the queries the OPERATOR reads and the one repair they can run by
// hand (ROADMAP #10–#12, DESIGN.md §20 errata 144–146).
//
// Everything here is additive: no column, no index and no migration. The job
// table already carries every fact `admin jobs` prints and every fact
// /healthz's queue block reports, and the repair `admin jobs unstick` performs
// is `ReconcileOnStartup`'s per-job rule applied to one row instead of to all
// of them at boot — which is why it is spelled with the SAME function
// (repairRunningJobTx) rather than a second implementation that could drift
// from the §5 coupling rule.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// ErrNotStale is `unstick`'s refusal: the job is running, but it has not been
// running long enough to be called stuck. It is a distinct sentinel because
// the CLI is a SECOND PROCESS and cannot see the worker's in-memory run
// registry (§12): age is the only evidence it has that nobody is holding the
// job, so "too young" must be a refusal an operator can tell apart from "not
// running at all" (ErrStateMismatch) and "no such job" (ErrNotFound).
var ErrNotStale = errors.New("db: job has not been running long enough")

// JobQuery filters ListJobs. A zero value lists the newest jobs of every kind
// and state. Every field is an EXACT match except Since, which is a lower
// bound on updated_at.
type JobQuery struct {
	Kind   string
	State  string
	ClipID string
	// Since bounds updated_at from below (unix seconds); 0 = no bound.
	Since int64
	// Limit caps the result (0 = DefaultJobListLimit). Rows come back newest
	// first, so a limit truncates the OLDEST rows, never the interesting ones.
	Limit int
}

// DefaultJobListLimit bounds `admin jobs` when the caller names no limit. A
// job row is one line of output and a busy instance keeps every job it has
// ever run, so an unbounded default would page a terminal for no reason.
const DefaultJobListLimit = 100

// MaxJobListLimit is the ceiling on JobQuery.Limit.
const MaxJobListLimit = 10000

// ListJobs returns jobs matching q, newest first (by id, which is the insert
// order). It reads on the ro pool and takes no lock the worker cares about.
func (d *DB) ListJobs(ctx context.Context, q JobQuery) ([]Job, error) {
	var where []string
	var args []any
	if q.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, q.Kind)
	}
	if q.State != "" {
		where = append(where, "state = ?")
		args = append(args, q.State)
	}
	if q.ClipID != "" {
		where = append(where, "clip_id = ?")
		args = append(args, q.ClipID)
	}
	if q.Since > 0 {
		where = append(where, "updated_at >= ?")
		args = append(args, q.Since)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultJobListLimit
	}
	if limit > MaxJobListLimit {
		limit = MaxJobListLimit
	}
	sqlText := "SELECT " + jobCols + " FROM jobs"
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	sqlText += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := d.ro.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// JobKindStateCounts counts every job row by (kind, state). `admin jobs`
// prints it as its summary so an operator sees the shape of the queue without
// paging the rows; it is deliberately NOT in the /healthz DTO, which stays the
// five anonymous numbers of QueueHealth (§20 erratum 145).
func (d *DB) JobKindStateCounts(ctx context.Context) (map[string]map[string]int, error) {
	rows, err := d.ro.QueryContext(ctx, "SELECT kind, state, COUNT(*) FROM jobs GROUP BY kind, state")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int{}
	for rows.Next() {
		var kind, state string
		var n int
		if err := rows.Scan(&kind, &state, &n); err != nil {
			return nil, err
		}
		if out[kind] == nil {
			out[kind] = map[string]int{}
		}
		out[kind][state] = n
	}
	return out, rows.Err()
}

// ClipIDsWithLiveJobs returns the set of clips carrying at least one job in a
// NON-TERMINAL state (`queued` or `running`) that could still write an object
// under the clip's prefix — the clips somebody may still be finishing.
//
// It exists for `admin orphans`' third report (§20 erratum 150). A transcode
// uploads its rendition and poster and then CHECKPOINTS their keys (§10.3
// step 8) BEFORE `FinalizeTranscode` writes `rendition_key`/`poster_key`, and
// the retry of a checkpointed job is finalize-only. Between those two writes
// the objects are referenced by `jobs.checkpoint_json` and by no column at
// all, so a sweep that removes "every object no column names" would remove
// exactly the outputs the retry is about to publish — and age cannot bound
// that window, because the population that needs repairing at all is jobs
// that have been stuck for hours. Asking whether anybody may still be on the
// clip is the bound that does hold.
//
// `notify` and `hook` are EXCLUDED, and they are the only exclusions. The
// interlock is about objects, and a notify or hook job posts to a webhook and
// writes nothing to the bucket at all — so a queued announcement, which on a busy instance is the
// longest-lived non-terminal row there is (`NOTIFY_MAX_ATTEMPTS` retries
// against a Discord that is down), would otherwise suppress the whole report
// for a clip whose leftovers are exactly what an operator is looking for. It
// is written as an EXCLUSION rather than as a list of the kinds that do write
// objects on purpose: a kind added later starts out protected, and the day
// somebody teaches this system a new media job the interlock covers it
// without anybody remembering to say so.
//
// It reads on the ro pool and takes no lock the worker cares about.
func (d *DB) ClipIDsWithLiveJobs(ctx context.Context) (map[string]bool, error) {
	rows, err := d.ro.QueryContext(ctx,
		"SELECT DISTINCT clip_id FROM jobs WHERE state IN (?, ?) AND kind NOT IN (?, ?)",
		JobQueued, JobRunning, KindNotify, KindHook)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// QueueHealth is the five numbers /healthz reports about the queue (§20
// erratum 145) and `admin stats` prints. Every one of them is a COUNT or an
// AGE: nothing here names a clip, a user or a key, because /healthz is
// anonymous and unauthenticated.
type QueueHealth struct {
	// Queued and Running are the counters /healthz has always carried.
	Queued, Running int
	// Failed24h is jobs that reached `failed` in the last 24 hours. A backlog
	// of failures is invisible in Queued/Running, which is the gap this closes.
	Failed24h int
	// OldestQueuedAgeS is how long the oldest CLAIMABLE queued job has been
	// claimable (now − min(run_after) over jobs with run_after <= now), 0 when
	// none is. A job waiting out its backoff is deliberately not counted: it is
	// not claimable yet, so counting it would report a stuck queue every time a
	// storage blip requeued something.
	OldestQueuedAgeS int64
	// LongestRunningAgeS is now − min(started_at) over running jobs, 0 when
	// none. Past TRANSCODE_HARD_TIMEOUT it means a row nobody is finishing —
	// the condition `admin jobs unstick` repairs.
	LongestRunningAgeS int64
}

// QueueHealth computes the block in one statement.
func (d *DB) QueueHealth(ctx context.Context, now int64) (QueueHealth, error) {
	var q QueueHealth
	var oldestQueued, oldestRunning int64
	err := d.ro.QueryRowContext(ctx, `
SELECT COALESCE(SUM(state = 'queued'), 0),
       COALESCE(SUM(state = 'running'), 0),
       COALESCE(SUM(state = 'failed' AND finished_at IS NOT NULL AND finished_at >= ?), 0),
       COALESCE(MIN(CASE WHEN state = 'queued' AND run_after <= ? THEN run_after END), 0),
       COALESCE(MIN(CASE WHEN state = 'running' THEN started_at END), 0)
FROM jobs`, now-86400, now).
		Scan(&q.Queued, &q.Running, &q.Failed24h, &oldestQueued, &oldestRunning)
	if err != nil {
		return QueueHealth{}, err
	}
	q.OldestQueuedAgeS = ageSince(oldestQueued, now)
	q.LongestRunningAgeS = ageSince(oldestRunning, now)
	return q, nil
}

// ageSince renders now−ts as a non-negative age, and 0 for the "no such row"
// sentinel. The container clock steps backwards (DEPLOY §8), so a negative age
// is a real possibility and is reported as 0 rather than as a negative number
// nobody's alerting expects.
func ageSince(ts, now int64) int64 {
	if ts <= 0 || now <= ts {
		return 0
	}
	return now - ts
}

// UnstickResult is what one repaired job became.
type UnstickResult struct {
	JobID          int64
	ClipID, Kind   string
	StartedAt      int64
	Action         string // "released" | "requeued" | "failed"
	ClipStateAfter string
}

// The three actions repairRunningJobTx can take, named so the CLI and the
// tests spell them the same way.
const (
	UnstickReleased = "released"
	UnstickRequeued = "requeued"
	UnstickFailed   = "failed"
)

// UnstickReason is written into the repaired job's last_error and, for a
// transcode job that has now exhausted its attempts, into the clip's
// failure_reason — which the share page renders to ANY viewer. It therefore
// says what happened and nothing about this instance: no path, no host, no
// job id, no operator.
const UnstickReason = "interrupted: the job was found running with no worker"

// IsOperatorReason reports whether s is one of the sentences THIS package
// writes into jobs.last_error — a job found running with no worker, and a job
// the process did not survive. They are an OPERATOR's words: every other value
// in that column was written by internal/worker, which words each media kind's
// endings from that kind's own fixed vocabulary (§20 erratum 80).
//
// It exists for the routes that show last_error to somebody who is not an
// operator (§20 erratum 165). A kind with a closed vocabulary needs no list —
// anything outside its own sentences is already mapped onto its generic one —
// but the gif's reason is deliberately open (publicReason's redacted
// "stage: message", printed by a PUBLIC route an operator also reads), so for
// that kind this is the whole test. Keeping the list beside the constants is
// the point: a third sentence added here is covered without anybody having to
// remember the other end.
func IsOperatorReason(s string) bool {
	return s == MsgInterruptedByRestart || s == UnstickReason
}

// UnstickJob repairs ONE running job whose started_at is at or before
// notAfter, applying exactly the rule ReconcileOnStartup applies at boot: a
// job WITH a checkpoint is released (no counter spent, the retry is
// finalize-only), a job WITHOUT one takes a FailCrash attempt. The §5
// coupling is untouched — only a transcode job moves its clip, and it moves it
// through the same failAttemptTx/releaseJobTx every other caller uses.
//
// The age guard is inside the transaction on purpose. The CLI cannot see the
// worker's run registry, so "started long enough ago that no live worker can
// still be on it" is the whole safety argument; checking it in the caller
// would leave a window in which a job claimed a moment ago is repaired out
// from under the ffmpeg that is running it.
//
// Errors: ErrNotFound (no such job), ErrStateMismatch (not running),
// ErrNotStale (running, but started after notAfter).
func (d *DB) UnstickJob(ctx context.Context, jobID, notAfter int64, lim Limits, now int64) (UnstickResult, error) {
	var res UnstickResult
	err := d.tx(ctx, func(tx *sql.Tx) error {
		j, err := getJob(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if j.State != JobRunning {
			return ErrStateMismatch
		}
		if !staleEnough(j, notAfter) {
			return ErrNotStale
		}
		res, err = repairRunningJobTx(ctx, tx, j, UnstickReason, lim, now)
		return err
	})
	if err != nil {
		return UnstickResult{}, err
	}
	return res, nil
}

// UnstickStaleJobs repairs every running job whose started_at is at or before
// notAfter, in id order, in ONE transaction — so an operator either gets the
// whole repair or none of it, exactly as the boot reconcile does.
func (d *DB) UnstickStaleJobs(ctx context.Context, notAfter int64, lim Limits, now int64) ([]UnstickResult, error) {
	var out []UnstickResult
	err := d.tx(ctx, func(tx *sql.Tx) error {
		out = nil
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
			if !staleEnough(j, notAfter) {
				continue
			}
			res, err := repairRunningJobTx(ctx, tx, j, UnstickReason, lim, now)
			if err != nil {
				return err
			}
			out = append(out, res)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// StaleRunningJobs returns the running jobs that `unstick`'s age test would
// repair — started_at at or before notAfter, of EVERY kind — in id order,
// capped at limit (0 = DefaultJobListLimit, ceiling MaxJobListLimit).
//
// It is the CANDIDATE list of the automatic repair (§20 erratum 231), which
// then drops the jobs its own process is running and repairs what is left one
// row at a time through UnstickJob. The predicate is `staleEnough` itself and
// not a second copy of it in SQL, for the reason the whole of this file exists:
// the boot reconcile, the operator's `unstick` and the automatic loop must be
// unable to disagree about which rows they are talking about. The `running`
// population is bounded by what a healthy instance can claim at once, so the
// filter runs in Go over the rows rather than in an index nobody else needs.
//
// It reads on the ro pool and takes no lock the worker cares about.
func (d *DB) StaleRunningJobs(ctx context.Context, notAfter int64, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = DefaultJobListLimit
	}
	if limit > MaxJobListLimit {
		limit = MaxJobListLimit
	}
	rows, err := d.ro.QueryContext(ctx, "SELECT "+jobCols+" FROM jobs WHERE state = ? ORDER BY id", JobRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		if !staleEnough(j, notAfter) {
			continue
		}
		if out = append(out, j); len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// staleEnough reports whether a running job may be repaired by hand. A NULL
// started_at counts as stale: ClaimJob always sets it, so a running row
// without one is already inconsistent and the repair is the point.
func staleEnough(j Job, notAfter int64) bool {
	return j.StartedAt == nil || *j.StartedAt <= notAfter
}

// repairRunningJobTx is the per-job half of ReconcileOnStartup, extracted so
// the boot repair and the operator's `unstick` cannot drift apart. It assumes
// the caller has already established that j is `running`.
//
// It is the one caller of these two endings that passes `unpinned` (§20
// erratum 293): a repair exists precisely to end a row that no run of this
// process is holding, and its evidence is age plus the live set rather than a
// claim stamp it would have to hold itself. It is also what must still reach a
// row whose `started_at` is NULL — which staleEnough above deliberately counts
// as stale, and which no pinned write can ever match.
func repairRunningJobTx(ctx context.Context, tx *sql.Tx, j Job, reason string, lim Limits, now int64) (UnstickResult, error) {
	res := UnstickResult{JobID: j.ID, ClipID: j.ClipID, Kind: j.Kind}
	if j.StartedAt != nil {
		res.StartedAt = *j.StartedAt
	}
	if j.CheckpointJSON != nil {
		if err := releaseJobTx(ctx, tx, j.ID, unpinned, j.ClipID, now); err != nil {
			return UnstickResult{}, err
		}
		res.Action = UnstickReleased
	} else {
		out, err := failAttemptTx(ctx, tx, j.ID, unpinned, j.ClipID, reason, FailCrash, lim, now, 0)
		if err != nil {
			return UnstickResult{}, err
		}
		res.Action = UnstickRequeued
		if out == OutcomeFailed {
			res.Action = UnstickFailed
		}
	}
	c, err := getClip(ctx, tx, j.ClipID)
	switch {
	case errors.Is(err, ErrNotFound):
		res.ClipStateAfter = "(gone)"
	case err != nil:
		return UnstickResult{}, err
	default:
		res.ClipStateAfter = c.State
	}
	return res, nil
}
