package worker

import (
	"context"
	"errors"

	"clippet/internal/db"
	"clippet/internal/notify"
)

// hookLimits is a `hook` job's budget: five delivery attempts (§20 erratum
// 319), never the transcode caps. db.limitsFor pins the same numbers on its
// side, so a reconciled job cannot pick up TRANSCODE_MAX_ATTEMPTS either.
var hookLimits = db.Limits{MaxAttempts: db.HookMaxAttempts, MaxTransient: db.HookMaxAttempts}

var hookDelivery = delivery{noun: "hook", subject: "clip webhook", limits: hookLimits}

// runHook delivers one `hook` job: the clip webhook of ROADMAP #54 (§20
// erratum 319). It is runNotify's twin and keeps every one of its rules — the
// notify loop, no run registry, the clip never written, WARN and never ERROR
// — with two differences that are the feature: an UNLISTED clip is sent
// (marked `"unlisted": true`; the receiver is the operator's own system), and
// there is no route — one URL, or none.
//
// Every value in the document is read HERE, from a fresh read of the row, so
// a title edited or an unlisted flag flipped while the job waited is what is
// sent. The URL and the key are never in scope: only internal/notify holds
// them, and the job row holds `{}`.
func (w *worker) runHook(ctx context.Context, job db.Job, clip db.Clip) {
	log := w.log.With("clip", clip.ID, "job", job.ID)

	// Stop() must not wait out a whole attempt (runNotify's reason).
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-w.stopCh:
			cancel()
		case <-hctx.Done():
		}
	}()

	if w.deps.Hook == nil {
		// Enqueued while the webhook was set, claimed after the operator
		// removed it: nothing to deliver and no client to deliver it with.
		w.skipDelivery(hctx, hookDelivery, job, log, "webhook not configured")
		return
	}

	c, err := w.deps.DB.GetClip(hctx, clip.ID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		w.skipDelivery(hctx, hookDelivery, job, log, "clip deleted")
		return
	case err != nil:
		if hctx.Err() != nil {
			w.releaseDelivery(hctx, hookDelivery, job, log)
			return
		}
		w.failDelivery(hctx, hookDelivery, job, clip.ID, log, "re-read clip: "+err.Error(), true, 0)
		return
	case c.State == db.ClipDeleting:
		w.skipDelivery(hctx, hookDelivery, job, log, "clip is being deleted")
		return
	case c.State != db.ClipReady:
		w.skipDelivery(hctx, hookDelivery, job, log, "clip is "+c.State)
		return
	}

	// The owner's USERNAME — public through the clip API already — and
	// nothing else of the account. An owner row that is gone is an empty
	// name, not a reason to withhold the clip's event.
	owner := ""
	switch u, err := w.deps.DB.GetUserByID(hctx, c.OwnerID); {
	case err == nil:
		owner = u.Username
	case errors.Is(err, db.ErrNotFound):
	default:
		if hctx.Err() != nil {
			w.releaseDelivery(hctx, hookDelivery, job, log)
			return
		}
		w.failDelivery(hctx, hookDelivery, job, c.ID, log, "read owner: "+err.Error(), true, 0)
		return
	}

	res := w.deps.Hook.Deliver(hctx, notify.ClipEvent{
		ID: c.ID, Title: c.Title, Owner: owner,
		DurationMS: c.DurationMS, CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt,
		Unlisted: c.Unlisted,
	})
	switch {
	case res.OK:
		w.completeDelivery(hctx, hookDelivery, job, log, db.JobStageDelivered, res.Detail)
		log.Info(hookDelivery.subject+" delivered", "status", res.Status)
	case hctx.Err() != nil:
		// Cancellation is no failure (§11): release, counters untouched.
		w.releaseDelivery(hctx, hookDelivery, job, log)
	case res.Retry:
		w.failDelivery(hctx, hookDelivery, job, c.ID, log, res.Detail, true, res.RetryAfter)
	default:
		w.failDelivery(hctx, hookDelivery, job, c.ID, log, res.Detail, false, 0)
	}
}
