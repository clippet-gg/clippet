// Package lifecycle destroys a clip: tombstone → worker cancel (bounded by the
// caller's context) → delete prefix → delete row → scratch rm (DESIGN.md §12,
// §18.4).
//
// Objects are always removed before the row: a surviving row is a surviving
// pointer, so the system converges to "no orphaned objects". Every failure
// leaves the deleting tombstone in place, and the sweeper's first candidate
// rule (state = 'deleting') resumes the sequence on its next tick.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"clippet/internal/db"
	"clippet/internal/store"
	"clippet/internal/worker"
)

// Deps wires Destroy. OpTimeout bounds the one storage call inside it
// (DeletePrefix, whose minio implementation bounds each of its own
// sub-calls by the same OpTimeout), so Destroy returns within OpTimeout plus
// a margin when storage hangs (§16.2).
type Deps struct {
	DB                    *db.DB
	Store                 store.Store
	Worker                worker.Worker
	KeyPrefix, ScratchDir string
	Log                   *slog.Logger
	OpTimeout             time.Duration

	// Busy answers worker.Worker.Busy: does THIS process have a run
	// registered for the clip, and therefore own $SCRATCH_DIR/{id} right
	// now. Destroy does not use it (it cancels the run and waits); the
	// sweeper's scratch janitor does, as its in-process half of the §20
	// erratum 89 guard. nil means "nothing is running here", which is the
	// right answer for every caller that has no worker.
	Busy func(clipID string) bool

	// Destroyed is told that a clip's row is gone (§20 erratum 343): the
	// database snapshot in the bucket marks itself due. It must not block —
	// it runs on the destroying goroutine, the sweeper's or a request's — and
	// nil (every test, and an instance with the snapshot off) reads nothing.
	Destroyed func(clipID string)
}

// Result reports what Destroy achieved. RowDeleted=false with a nil error
// happens only when Skipped is set; an error with the tombstone set means
// "sweeper finishes". Skipped means DestroyExpired found the clip no longer
// due on this tick and touched nothing — either rescued (its TTL was
// extended after candidate selection) or deferred (claimed by the worker
// after selection, so it is inside its SWEEP_GRACE window). The destroy line
// names which (§20 erratum 51).
type Result struct {
	Objects    int
	RowDeleted bool
	Skipped    bool
}

// defaultOpTimeout guards a zero Deps.OpTimeout (serve.go passes
// STORAGE_OP_TIMEOUT; tests may leave it unset).
const defaultOpTimeout = 30 * time.Second

func (d Deps) logger() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

func (d Deps) opTimeout() time.Duration {
	if d.OpTimeout > 0 {
		return d.OpTimeout
	}
	return defaultOpTimeout
}

// Destroy runs the §12 sequence for clipID:
//
//  1. db.MarkDeleting — the tombstone; public routes answer 404 from here on,
//     ClaimJob skips the clip, queued/running jobs are cancelled. A missing
//     row means the clip is already gone → ok.
//  2. worker.Cancel — waits, bounded by ctx, for any in-flight job goroutine
//     to exit (ffmpeg reaped, no PutFile in flight). A wait that runs out of
//     ctx ends the sequence here with ErrStillRunning: the tombstone is all
//     this call leaves, and the next tick starts again (AUDIT #13).
//  3. store.DeletePrefix under its own OpTimeout ctx — objects, then
//     incomplete multipart uploads, then the re-list must be empty (§6).
//     An error leaves the tombstone; the next sweep tick retries.
//     This step works off a LISTING, and a bucket's listing is an index that
//     can lag a write of a millisecond ago (§20 erratum 305). It is not, and
//     cannot be, what removes an object a run was writing as step 2 cancelled
//     it — the RUN removes those, by key, before its goroutine exits, which is
//     what makes step 2's "no PutFile in flight" a statement about the bucket
//     and not only about this process. Step 3 removes what the clip has HAD.
//  4. db.DeleteClipRow — jobs cascade. An error leaves the tombstone; the
//     next tick re-lists an empty prefix and deletes the row.
//  5. Scratch dir removal and worker.Forget, best-effort.
//
// Log levels follow the §12 doctrine: success and cancellation are INFO, a
// storage error or a deadline is WARN (it will be retried), and a database
// error outside busy_timeout is ERROR.
func Destroy(ctx context.Context, d Deps, clipID string) (Result, error) {
	return destroy(ctx, d, clipID, func(ctx context.Context, now int64) error {
		return d.DB.MarkDeleting(ctx, clipID, now)
	})
}

// DestroyExpired is the sweeper's expiry-driven Destroy: the tombstone is
// written conditionally (db.MarkDeletingIfDue with the tick's now and
// grace), so a TTL extension PATCHed between candidate selection and this
// call wins — the clip is skipped (Result.Skipped, nil error, INFO) instead
// of destroyed (§8.2, §12). Owner DELETE keeps the unconditional Destroy.
func DestroyExpired(ctx context.Context, d Deps, clipID string, now, grace int64) (Result, error) {
	return destroy(ctx, d, clipID, func(ctx context.Context, _ int64) error {
		err := d.DB.MarkDeletingIfDue(ctx, clipID, now, grace)
		if errors.Is(err, db.ErrStateMismatch) && stillExpired(ctx, d, clipID, now) {
			return errInGrace
		}
		return err
	})
}

// errInGrace marks the OTHER way a candidate stops matching the §12
// predicate: not a rescue but a deferral (§20 erratum 51).
var errInGrace = errors.New("still expired, inside the sweep grace window")

// ErrStillRunning ends a Destroy whose worker.Cancel ran out of context before
// the clip's run had exited (AUDIT #13). The tombstone is written and nothing
// else was touched, which is the state every other §12 failure leaves behind:
// the next sweep tick resumes from candidate rule (a). The sweeper reads the
// CONTEXT rather than this error to decide whether the tick deferred it —
// Cancel only ever returns false on a dead ctx — so it needs no case of its own
// there; it is a sentinel so a caller (and a test) can name the ending.
var ErrStillRunning = errors.New("the clip's run is still exiting")

// stillExpired re-reads the row to tell the two apart. Rule (b) covers only
// `queued`/`ready`/`failed`, so a candidate the worker claims between
// SweepCandidates and this call is `processing` and matches neither (b) nor
// rule (c), which needs `expires_at <= now - grace` — the clip is not
// rescued, it has entered the SWEEP_GRACE window §12 grants a clip that is
// mid-encode at its expiry, and the first tick past that window destroys it.
// A read that fails says nothing, so the generic note stands.
func stillExpired(ctx context.Context, d Deps, clipID string, now int64) bool {
	c, err := d.DB.GetClip(ctx, clipID)
	return err == nil && c.ExpiresAt != nil && *c.ExpiresAt <= now
}

func destroy(ctx context.Context, d Deps, clipID string, mark func(context.Context, int64) error) (Result, error) {
	start := time.Now()
	log := d.logger().With("clip", clipID)
	var res Result

	if err := mark(ctx, start.Unix()); err != nil {
		switch {
		case errors.Is(err, db.ErrNotFound):
			log.Info("destroy", "objects", 0, "ok", true, "note", "already gone", "dur", time.Since(start))
			return Result{RowDeleted: true}, nil
		case errors.Is(err, errInGrace):
			// Still expired, just not destroyable on this tick: the worker
			// claimed it between selection and now, so it is `processing`
			// inside its SWEEP_GRACE window (§20 erratum 51).
			log.Info("destroy", "objects", 0, "ok", true, "note", "in grace; deferred", "dur", time.Since(start))
			return Result{Skipped: true}, nil
		case errors.Is(err, db.ErrStateMismatch):
			// Only MarkDeletingIfDue returns this: the clip stopped being a
			// candidate between selection and now — an expected outcome.
			log.Info("destroy", "objects", 0, "ok", true, "note", "no longer expired; skipped", "dur", time.Since(start))
			return Result{Skipped: true}, nil
		}
		return res, fail(ctx, log, start, res, "mark deleting", err, dbLevel(err))
	}

	if d.Worker != nil && !d.Worker.Cancel(ctx, clipID) {
		// The run did not finish inside this Destroy's budget (AUDIT #13).
		// Nothing may be removed while a goroutine can still PutFile into the
		// prefix, so this ends where every other §12 failure ends: the
		// tombstone stays, candidate rule (a) picks the clip up on the next
		// tick, and an owner's DELETE answers 202 "deferred to sweeper". It is
		// an expected outcome rather than trouble, so it is INFO — the level a
		// cancellation already gets.
		return res, fail(ctx, log, start, res, "cancel the run", ErrStillRunning, slog.LevelInfo)
	}

	prefix := store.ClipPrefix(d.KeyPrefix, clipID)
	sctx, cancel := context.WithTimeout(ctx, d.opTimeout())
	n, err := d.Store.DeletePrefix(sctx, prefix)
	cancel()
	res.Objects = n
	if err != nil {
		return res, fail(ctx, log, start, res, "delete prefix "+prefix, err, storageLevel(err))
	}

	if err := d.DB.DeleteClipRow(ctx, clipID); err != nil && !errors.Is(err, db.ErrNotFound) {
		// ErrNotFound: another Destroy finished first — same end state.
		return res, fail(ctx, log, start, res, "delete row", err, dbLevel(err))
	}
	res.RowDeleted = true

	if d.ScratchDir != "" {
		if err := os.RemoveAll(filepath.Join(d.ScratchDir, clipID)); err != nil {
			log.Warn("destroy: scratch remove", "err", err)
		}
	}
	if d.Worker != nil {
		d.Worker.Forget(clipID)
	}
	if d.Destroyed != nil {
		d.Destroyed(clipID)
	}
	log.Info("destroy", "objects", n, "ok", true, "dur", time.Since(start))
	return res, nil
}

// fail logs the single destroy line for a failed run and wraps err with the
// step that failed.
func fail(ctx context.Context, log *slog.Logger, start time.Time, res Result, step string, err error, lvl slog.Level) error {
	log.Log(ctx, lvl, "destroy", "objects", res.Objects, "ok", false, "step", step, "err", err, "dur", time.Since(start))
	return fmt.Errorf("lifecycle: %s: %w", step, err)
}

// storageLevel: a storage error is transient trouble the sweeper retries
// (WARN); a cancellation is an expected outcome (INFO).
func storageLevel(err error) slog.Level {
	if errors.Is(err, context.Canceled) {
		return slog.LevelInfo
	}
	return slog.LevelWarn
}

// dbLevel: cancellation is INFO, a deadline is WARN, anything else from the
// database outside busy_timeout is a condition the design says cannot
// happen (ERROR) — including ErrStateMismatch, since nothing ever leaves
// the deleting state.
func dbLevel(err error) slog.Level {
	switch {
	case errors.Is(err, context.Canceled):
		return slog.LevelInfo
	case errors.Is(err, context.DeadlineExceeded):
		return slog.LevelWarn
	}
	return slog.LevelError
}
