package db

// sweepfail.go — the sweeper's per-clip Destroy-failure backoff, persisted
// (N004; DESIGN.md §12, §20 erratum 232 amending 101(a); migration 0013).
//
// The sweeper owns the POLICY (how many failures, how long the backoff, how
// many retries a tick may spend); this file is only its memory. Three calls,
// all of them cheap and none of them on a hot path: one read per PROCESS at
// the first tick, one write per failed Destroy, one delete per Destroy that
// finally went through.
//
// Everything here is best-effort from the sweeper's point of view — a write
// that fails leaves the in-memory schedule intact and costs nothing but the
// memory of it across a restart, which is the state this table improved on.

import "context"

// SweepFailure is one failing clip's schedule.
type SweepFailure struct {
	ClipID string
	// Failures is the consecutive-failure count the backoff is derived from.
	Failures int
	// LastAttemptAt is when the failure that wrote this row happened, and
	// NextAttemptAt the earliest moment the clip may be attempted again (both
	// unix seconds).
	LastAttemptAt, NextAttemptAt int64
}

// LoadSweepFailures returns at most limit failing-clip schedules, the ones
// whose next attempt is FURTHEST away first.
//
// That order is the in-memory cache's eviction policy read backwards, on
// purpose: `evictLocked` drops the entry due soonest, because its backoff is
// nearly over anyway, so the rows worth remembering across a restart are the
// most suppressed ones. A schedule this cap leaves behind is simply forgotten,
// which is the behaviour every version before this table had.
func (d *DB) LoadSweepFailures(ctx context.Context, limit int) ([]SweepFailure, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := d.ro.QueryContext(ctx,
		"SELECT clip_id, failures, last_attempt_at, next_attempt_at FROM sweep_failures ORDER BY next_attempt_at DESC, clip_id LIMIT ?",
		limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SweepFailure
	for rows.Next() {
		var f SweepFailure
		if err := rows.Scan(&f.ClipID, &f.Failures, &f.LastAttemptAt, &f.NextAttemptAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetSweepFailure records (or replaces) one clip's schedule. The COUNT is the
// caller's, not an increment here: the sweeper holds it in memory for the
// process's lifetime and this row exists so the NEXT process starts from the
// same number, so a read-modify-write in the database would be a second source
// of truth that could disagree with the first.
//
// A clip that is already GONE writes nothing and is not an error: the row is
// keyed by a foreign key, and an owner DELETE that lands while the sweeper's
// own Destroy of the same prefix is failing is a real (if rare) race whose
// outcome — no schedule for a clip that no longer needs one — is exactly what
// was wanted. The existence test is in the statement rather than in a
// driver-specific error check, so nothing here depends on how modernc spells a
// constraint violation.
func (d *DB) SetSweepFailure(ctx context.Context, f SweepFailure) error {
	_, err := d.rw.ExecContext(ctx, `
INSERT INTO sweep_failures (clip_id, failures, last_attempt_at, next_attempt_at)
SELECT ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM clips WHERE id = ?)
ON CONFLICT(clip_id) DO UPDATE SET failures = excluded.failures,
  last_attempt_at = excluded.last_attempt_at, next_attempt_at = excluded.next_attempt_at`,
		f.ClipID, f.Failures, f.LastAttemptAt, f.NextAttemptAt, f.ClipID)
	return err
}

// ClearSweepFailure forgets a clip's schedule (a Destroy that succeeded, or a
// candidate that turned out not to be due after all). Missing row = nothing to
// do, which is the ordinary case for every clip that never failed.
func (d *DB) ClearSweepFailure(ctx context.Context, clipID string) error {
	_, err := d.rw.ExecContext(ctx, "DELETE FROM sweep_failures WHERE clip_id = ?", clipID)
	return err
}

// CountSweepFailures is the size of the table: `admin stats`' `backoff:` line
// and the tests. Nothing in the TICK reads it — the schedule the sweeper obeys
// is the in-memory map, and this is only how many clips are on it.
func (d *DB) CountSweepFailures(ctx context.Context) (int, error) {
	var n int
	err := d.ro.QueryRowContext(ctx, "SELECT COUNT(*) FROM sweep_failures").Scan(&n)
	return n, err
}
