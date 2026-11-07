package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
)

// The automatic half of ROADMAP #12 (§20 errata 230-231): a job left `running`
// by a worker that is no longer there is repaired by the app itself, on a
// timer, without waiting for a restart.
//
// §11 promises a job is "always claimable or retryable, never stuck". Until
// now that promise held at BOOT (`ReconcileOnStartup`) and by HAND
// (`clippet admin jobs unstick`, erratum 146). Between the two, a row left
// running by a crash inside a job's own exit path — the case ROADMAP #12
// names — sat there, and for a transcode it sat there with its clip
// `processing`, i.e. a clip that never becomes playable and, with the default
// TTL of `never`, is never swept either.
//
// # The whole design is the answer to one question: how do we know nobody is
// on this job?
//
// There are exactly two kinds of evidence, and this loop uses BOTH:
//
//  1. **This process's own claims.** Every job this worker has claimed and not
//     yet finished is in `live` (see trackJob), keyed by job id and recorded
//     for EVERY kind — `runJob` is the one door all of them go through, so a
//     kind added later is covered without anybody remembering to say so. A job
//     in that set is never repaired, however old it is. This is the evidence
//     `admin jobs unstick` cannot have (it is a second process, erratum 146)
//     and it is why the automatic repair can be trusted where a cron job
//     running the CLI could not be.
//
//     It is deliberately NOT `Worker.Busy`, which ROADMAP #12's sketch named:
//     `Busy` answers about a CLIP and only for the kinds that register a run,
//     so it says nothing about a live `notify` job (unregistered by design,
//     §10.1) and it would refuse to repair a genuinely stuck OLD job of a clip
//     that has a healthy NEW run — two wrong answers in opposite directions.
//
//  2. **Age**: `started_at` at or before `now − JOBS_UNSTICK_AFTER`
//     (config.UnstickPlan, floored at a 30s sanity bound). Age is what covers
//     the process that is NOT this one — the one that died, and a second
//     instance somebody pointed at the same database. It is deliberately NOT
//     floored at `TRANSCODE_HARD_TIMEOUT` the way the CLI's `--older-than` is:
//     that floor exists because the CLI has no evidence but age, and (1) is
//     evidence. An age under the hard timeout is obeyed with one boot WARN
//     naming the only thing it risks, because six hours is a long time for a
//     clip to sit unplayable and an operator running one container should be
//     able to say so (§20 erratum 230).
//
// The repair itself is `db.UnstickJob`, per row: the same predicate (its age
// test is inside the transaction, so a job claimed a moment ago cannot be
// repaired out from under the ffmpeg running it), the same
// `repairRunningJobTx` the boot reconcile and the CLI use, the same
// `db.UnstickReason` in `last_error`, the same §5 clip coupling and the same
// attempt accounting — a checkpointed job is released with no counter spent, a
// job without one spends one crash attempt and fails for good at the cap, per
// §11. Nothing here is a second implementation of any of that.
//
// Row-at-a-time rather than `UnstickStaleJobs`' single transaction (which the
// CLI's `--older-than` form uses) because the live filter is per JOB: an
// all-or-nothing sweep cannot skip one row. Each repair is still atomic on its
// own, which is what the single-job CLI form has always been.

// repairBatch bounds one pass. A pass is a scan of the `running` rows, which on
// a healthy instance is at most TRANSCODE_CONCURRENCY + a media job + a notify
// job; a crash loop can leave more, and this is the number of them one pass
// will repair (the next pass takes the rest). It is generous on purpose: the
// point of the feature is that a queue does not stay stuck.
const repairBatch = 100

// Repairs counts what the automatic repair has done since boot. serve.go owns
// the instance, the worker writes it and /healthz reads it — the same shape as
// the sweeper's storage-budget holder, and for the same reason: no route may
// run a query of its own to answer this (§20 erratum 231).
//
// The counters are lifetime totals and never reset, so an alert reads them as
// a rate. Nothing here names a clip, a user or a configured value: /healthz is
// anonymous (§15).
type Repairs struct {
	repaired atomic.Int64
	released atomic.Int64
	requeued atomic.Int64
	failed   atomic.Int64
	errs     atomic.Int64
}

// RepairStatus is one reading of those counters.
type RepairStatus struct {
	// Repaired is every job this process put back on the queue or released —
	// the headline number, and the sum of Released, Requeued and Failed.
	Repaired int64
	// Released is the checkpointed jobs (no attempt spent, finalize-only
	// retry); Requeued the ones that spent a crash attempt; Failed the ones
	// whose attempts ran out, which for a transcode is a clip its owner is now
	// told about (§11).
	Released, Requeued, Failed int64
	// Errors counts repair passes that could not read or write the queue.
	Errors int64
}

// Get returns the current totals.
func (r *Repairs) Get() RepairStatus {
	return RepairStatus{
		Repaired: r.repaired.Load(),
		Released: r.released.Load(),
		Requeued: r.requeued.Load(),
		Failed:   r.failed.Load(),
		Errors:   r.errs.Load(),
	}
}

// Record folds one repair into the totals — the writer half of the holder, the
// way httpx.StorageBudget.Set is the writer half of that one. `action` is a
// db.Unstick* constant; anything else counts as a requeue, because the one
// thing that must never be wrong is the headline total.
func (r *Repairs) Record(action string) {
	if r == nil {
		return
	}
	r.repaired.Add(1)
	switch action {
	case db.UnstickReleased:
		r.released.Add(1)
	case db.UnstickFailed:
		r.failed.Add(1)
	default:
		r.requeued.Add(1)
	}
}

// RecordError counts a repair pass that could not read or write the queue.
func (r *Repairs) RecordError() {
	if r != nil {
		r.errs.Add(1)
	}
}

// trackJob records that this process has claimed a job and is running it.
// Called for EVERY claimed job, whatever its kind, at the one point every kind
// passes through (runJob) — never from a kind list, which is how `admin jobs`
// once came to know four of seven kinds.
func (w *worker) trackJob(id int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.live == nil {
		w.live = map[int64]struct{}{}
	}
	w.live[id] = struct{}{}
}

// untrackJob forgets a job whose run has fully returned — after its exit path,
// so the window in which a repair could see it as idle opens only once there is
// genuinely nothing of ours left on it.
func (w *worker) untrackJob(id int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.live, id)
}

// holdsJob reports whether THIS process is running the job right now.
func (w *worker) holdsJob(id int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.live[id]
	return ok
}

// repairLoop runs one pass every plan.Every until ctx is done or Stop is
// called. It is a loop of its own rather than a branch of the sweeper's tick
// (which ROADMAP #12 suggested) for two reasons: the evidence it needs — the
// set of jobs this process has claimed — lives here, and the sweeper's tick is
// bounded by SWEEP_INTERVAL, so an operator lengthening the sweep would
// silently lengthen the repair too.
func (w *worker) repairLoop(ctx context.Context, plan config.UnstickPlan) {
	defer w.wg.Done()
	log := w.log.With("loop", "repair")
	t := time.NewTicker(plan.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopCh:
			return
		case <-t.C:
			w.repairOnce(ctx, log, plan.After)
		}
	}
}

// repairOnce is one pass: the candidates the age test names, minus the ones
// this process is running, repaired one row at a time.
//
// It returns how many rows it repaired, for the tests. Every outcome is
// reported through the log at the level §12's doctrine gives it: a repair is an
// expected event (INFO), a candidate that moved on its own between the scan and
// the repair is not news at all (DEBUG), and a database error the repair could
// not do anything about is ERROR — the same call in `admin jobs unstick` exits
// non-zero, and here there is nobody to tell but the operator's log.
func (w *worker) repairOnce(ctx context.Context, log *slog.Logger, after time.Duration) int {
	now := w.now()
	notAfter := now.Add(-after).Unix()
	stale, err := w.deps.DB.StaleRunningJobs(ctx, notAfter, repairBatch)
	if err != nil {
		if ctx.Err() != nil {
			return 0
		}
		w.deps.Repairs.RecordError()
		log.Error("automatic repair: read the running jobs", "err", err)
		return 0
	}
	repaired, alive := 0, 0
	// The rows this pass settled, told to Deps.OnSettled once the pass's own
	// summary line is written — on every return below (§20 erratum 331).
	var settled []Settled
	defer func() {
		for _, s := range settled {
			w.settled(s)
		}
	}()
	// A full batch means the scan hit repairBatch and there may be more stale
	// rows behind it, waiting for the next tick; the summary says so rather
	// than printing `candidates=100` and letting an operator read it as "all
	// of them".
	capped := len(stale) == repairBatch
	for _, j := range stale {
		if ctx.Err() != nil || w.stopped(ctx) {
			break
		}
		// The one thing age cannot tell us, and the reason this loop lives in
		// the worker: a job THIS process holds is being worked on, whatever its
		// age says. A slow acquire of a multi-gigabyte original (bounded by
		// STORAGE_TRANSFER_MAX, not by the encode's hard timeout) is the
		// ordinary way a live job outgrows the floor.
		//
		// There is one window this cannot cover, and it is worth naming: between
		// the commit that CLAIMS a job (which is what sets `started_at`) and
		// `trackJob` in runJob, the row is `running` and `live` does not know it.
		// The only thing protecting that window is `UnstickJob`'s age test, which
		// runs INSIDE the repair transaction — and the clock it compares against
		// is a wall clock that can step BACKWARDS, so "claimed a moment ago" is
		// not arithmetically safe on its own. The 30 s floor under
		// JOBS_UNSTICK_AFTER (config.UnstickPlan) is the margin that makes it
		// safe: the window is a handful of statements wide, and no plausible
		// backwards step spans half a minute of it.
		if w.holdsJob(j.ID) {
			alive++
			continue
		}
		res, err := w.deps.DB.UnstickJob(ctx, j.ID, notAfter, w.lim, w.now().Unix())
		switch {
		case err == nil:
			repaired++
			w.deps.Repairs.Record(res.Action)
			log.Info("repaired a job that was running with no worker", "job", res.JobID, "clip", res.ClipID,
				"kind", res.Kind, "action", res.Action, "clip_state", res.ClipStateAfter,
				"running_for_s", runningFor(res.StartedAt, now), "reason", db.UnstickReason)
			if cardInput(res.Kind) {
				// A repair that fails a card copy at its cap settles `light_state`
				// (erratum 236), which may be what makes the card ready (§20
				// erratum 330).
				w.cardReady(ctx, res.ClipID)
			}
			settled = append(settled, Settled{JobID: res.JobID, ClipID: res.ClipID, Kind: res.Kind, Repair: true})
		case errors.Is(err, db.ErrNotStale), errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
			// It finished, was claimed again, or its clip was destroyed between
			// the scan and now. Nothing happened and nothing was wrong.
			log.Debug("automatic repair: the job settled on its own", "job", j.ID, "kind", j.Kind, "err", err)
		case ctx.Err() != nil:
			return repaired
		default:
			w.deps.Repairs.RecordError()
			log.Error("automatic repair: unstick a job", "job", j.ID, "kind", j.Kind, "err", err)
		}
	}
	if repaired > 0 {
		// A requeued job is claimable NOW: nudge every queue rather than let
		// the loops find it on their next 5s poll (the kind is not this
		// function's business, which is the whole point of Wake).
		w.Wake()
		log.Info("automatic repair", "repaired", repaired, "alive_here", alive,
			"candidates", len(stale), "capped", capped)
	} else {
		log.Debug("automatic repair", "repaired", 0, "alive_here", alive,
			"candidates", len(stale), "capped", capped)
	}
	return repaired
}

// runningFor renders how long a repaired job had been running, in seconds. A
// NULL started_at (a `running` row ClaimJob never stamped, which is itself part
// of what the repair is for) and the container clock stepping BACKWARDS both
// come out as 0 rather than as a negative number or an epoch-sized one.
func runningFor(startedAt int64, now time.Time) int64 {
	if startedAt <= 0 || now.Unix() <= startedAt {
		return 0
	}
	return now.Unix() - startedAt
}
