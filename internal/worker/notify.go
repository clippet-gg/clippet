package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/notify"
)

// notifyRouting turns the configured DISCORD_WEBHOOK_ROUTES into the rule set
// db.FinalizeTranscode decides a clip's channel with (ROADMAP #26, §20
// erratum 159). It carries TAGS and a bool and never a URL: db stores a tag,
// so a webhook secret has no path into the database at all.
func (w *worker) notifyRouting() db.NotifyRouting {
	cfg := w.deps.Cfg
	out := db.NotifyRouting{
		// "There is somewhere to post an unmatched clip": a default webhook the
		// NOTIFIER actually holds, and an operator who has not asked for
		// tagged-clips-only. The first half is asked of the Notifier rather
		// than of the config because the Notifier is what would deliver it —
		// the two are built from the same config in serve.go, and deriving the
		// bit from the deliverer is what keeps them from ever disagreeing.
		DefaultConfigured: w.deps.Notifier != nil &&
			w.deps.Notifier.HasRoute(notify.DefaultRoute) &&
			cfg.DiscordUnmatched != config.DiscordUnmatchedSkip,
	}
	for _, r := range cfg.DiscordRoutes {
		out.Rules = append(out.Rules, db.NotifyRule{Tag: r.Tag, Name: r.Tag})
	}
	return out
}

// notifyLimits is a notify job's budget: three delivery attempts, never the
// transcode caps. db.limitsFor enforces the same thing on its side, so a
// reconciled job cannot pick up TRANSCODE_MAX_ATTEMPTS either.
var notifyLimits = db.Limits{MaxAttempts: db.NotifyMaxAttempts, MaxTransient: db.NotifyMaxAttempts}

// runNotify delivers one `notify` job: the Discord auto-post of §17 #4. It
// runs on the worker's non-transcode loop (§20 erratum 67), so the 10s an
// attempt may take is never taken from the encode queue.
//
// What it does NOT do is the load-bearing half. A notify job never writes
// clips.state, clips.failure_reason or either failure counter of the clip
// (§5) — the clip is `ready` before the job exists and stays `ready` however
// the delivery ends, because a chat message that did not arrive is not a
// broken clip. Every DB call below is kind-aware on the db side: ClaimJob
// moves no clip for this kind, FailAttempt and ReleaseJob move no clip for
// this kind, and CompleteJob refuses a transcode job outright.
//
// The run is deliberately NOT put in the run registry. Everything §10.1's
// Cancel waits for — ffmpeg reaped, no PutFile in flight, no scratch I/O —
// is absent from a notify job, and registering it would make an unrelated
// retry of the same clip queue behind it (§20 erratum 32). The one thing that
// could go wrong instead is a post landing microseconds after a delete, and
// the re-read below plus the `deleting` tombstone (which 404s every public
// route immediately) already cover that.
func (w *worker) runNotify(ctx context.Context, job db.Job, clip db.Clip) {
	// The webhook this job was ROUTED to when it was queued (ROADMAP #26). It
	// is a NAME — a tag, or `default` — and every line below carries it, so an
	// operator running several channels can see which one a post went to. The
	// URL is never in scope here: only internal/notify holds one.
	route := db.NotifyRouteOf(job.ParamsJSON)
	if route == "" {
		route = notify.DefaultRoute
	}
	log := w.log.With("clip", clip.ID, "job", job.ID, "webhook", route)

	// Stop() must not wait out a whole attempt: shutdown cancels this run the
	// same way it cancels a registered one.
	nctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-w.stopCh:
			cancel()
		case <-nctx.Done():
		}
	}()

	if w.deps.Notifier == nil {
		// The job outlived its configuration: enqueued while a webhook was
		// set, claimed after the operator removed it. There is nothing to
		// deliver and — crucially — no client to deliver it with, so the
		// "unset ⇒ nothing posted to Discord" rule holds for old rows too.
		w.skipNotify(nctx, job, log, "webhook not configured")
		return
	}
	if !w.deps.Notifier.HasRoute(route) {
		// The same case, one level finer: the job named a route the operator
		// has since removed from DISCORD_WEBHOOK_ROUTES. Posting it to the
		// default channel instead would put the clip somewhere nobody chose
		// for it, which is the one thing routing exists to prevent — so this
		// is a skip, like every other "the configuration moved" outcome.
		w.skipNotify(nctx, job, log, "webhook route not configured")
		return
	}

	c, err := w.deps.DB.GetClip(nctx, clip.ID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		w.skipNotify(nctx, job, log, "clip deleted")
		return
	case err != nil:
		if nctx.Err() != nil {
			w.releaseNotify(nctx, job, log)
			return
		}
		w.failNotify(nctx, job, clip.ID, log, "re-read clip: "+err.Error(), true, 0)
		return
	case c.State == db.ClipDeleting:
		w.skipNotify(nctx, job, log, "clip is being deleted")
		return
	case c.State != db.ClipReady:
		w.skipNotify(nctx, job, log, "clip is "+c.State)
		return
	case c.Unlisted:
		// The per-upload opt-out (§8.1): unlisted means "do not surface
		// this". It is checked again here, not only at enqueue, because the
		// owner may have flipped it while the job waited in the queue.
		w.skipNotify(nctx, job, log, "clip is unlisted")
		return
	}

	res := w.deps.Notifier.Deliver(nctx, route, c.ID, c.Title)
	switch {
	case res.OK:
		w.completeNotify(nctx, job, log, db.JobStageDelivered, res.Detail)
		log.Info(notifyDelivery.subject+" delivered", "status", res.Status)
	case nctx.Err() != nil:
		// Cancellation is no failure (§11): release, counters untouched.
		w.releaseNotify(nctx, job, log)
	case res.Retry:
		w.failNotify(nctx, job, c.ID, log, res.Detail, true, res.RetryAfter)
	default:
		w.failNotify(nctx, job, c.ID, log, res.Detail, false, 0)
	}
}

// delivery names one of the two outbound jobs the notify loop runs — the
// Discord auto-post and the clip webhook (§20 erratum 319) — for the helpers
// below, which are the same for both but for the words in their log lines and
// the attempt budget. The words are the operator's grep targets: Discord's
// are byte-for-byte what they were before the webhook existed (E2E step 20
// and DEPLOY §11 quote them), and no hook line ever says `discord webhook`.
type delivery struct {
	// noun names the job in the bookkeeping lines: `notify job released`.
	noun string
	// subject names the integration in the outcome lines: `discord webhook
	// delivered`.
	subject string
	limits  db.Limits
}

var notifyDelivery = delivery{noun: "notify", subject: "discord webhook", limits: notifyLimits}

// skipNotify resolves a job that must not post: the clip is gone, not ready
// any more, or unlisted.
func (w *worker) skipNotify(ctx context.Context, job db.Job, log *slog.Logger, reason string) {
	w.skipDelivery(ctx, notifyDelivery, job, log, reason)
}

func (w *worker) completeNotify(ctx context.Context, job db.Job, log *slog.Logger, stage, note string) {
	w.completeDelivery(ctx, notifyDelivery, job, log, stage, note)
}

func (w *worker) releaseNotify(ctx context.Context, job db.Job, log *slog.Logger) {
	w.releaseDelivery(ctx, notifyDelivery, job, log)
}

func (w *worker) failNotify(ctx context.Context, job db.Job, clipID string, log *slog.Logger, reason string, retryable bool, after time.Duration) {
	w.failDelivery(ctx, notifyDelivery, job, clipID, log, reason, retryable, after)
}

// skipDelivery resolves a job that must not deliver. It is a normal outcome,
// so the job is `done` (with the reason in last_error) at INFO — not a
// failure to retry.
func (w *worker) skipDelivery(ctx context.Context, d delivery, job db.Job, log *slog.Logger, reason string) {
	w.completeDelivery(ctx, d, job, log, db.JobStageSkipped, "skipped: "+reason)
	log.Info(d.subject+" skipped", "reason", reason)
}

func (w *worker) completeDelivery(ctx context.Context, d delivery, job db.Job, log *slog.Logger, stage, note string) {
	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), exitBudget)
	defer cancel()
	err := w.deps.DB.CompleteJob(ectx, job.ID, db.ClaimStamp(job), stage, note, w.now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		// The row is no longer this attempt's (§20 erratum 293): cancelled with
		// the clip, or repaired and perhaps re-claimed. A delivery job writes
		// no object, keeps no scratch and registers no progress, so the whole
		// lost-attempt ending is this one line — there is nothing to discard.
		log.Info(d.noun+" job row is no longer this attempt's; nothing recorded, row untouched", "note", note, "err", err)
	default:
		log.Error("complete "+d.noun+" job", "note", note, "err", err)
	}
}

// releaseDelivery is the shutdown path: back to queued, both counters
// untouched, retried after the restart.
func (w *worker) releaseDelivery(ctx context.Context, d delivery, job db.Job, log *slog.Logger) {
	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), exitBudget)
	defer cancel()
	err := w.deps.DB.ReleaseJob(ectx, job.ID, db.ClaimStamp(job), job.ClipID, w.now().Unix())
	switch {
	case err == nil:
		log.Info(d.noun+" job released", "reason", reasonShutdown.String())
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		log.Info(d.noun+" job row is no longer this attempt's; nothing released, row untouched", "err", err)
	default:
		log.Error("release "+d.noun+" job", "err", err)
	}
}

// failDelivery records one bad delivery. Retryable failures (network,
// timeout, 429, 5xx) use the CRASH class — one finite counter, capped at the
// delivery's own budget, with the 30s·2ⁿ backoff already in the schema — and
// a 429's Retry-After becomes a floor under that backoff. Everything else is
// permanent: a 400/401/403/404 or a redirect is a configuration problem no
// number of retries fixes, so the job is failed with the status and the
// response body an operator needs to see.
//
// Neither branch is ERROR: the E2E log gate treats ERROR as "a human is
// needed", and an integration the far end is rate-limiting or rejecting is
// not that (§12 log-level doctrine). WARN is exactly what it is.
func (w *worker) failDelivery(ctx context.Context, d delivery, job db.Job, clipID string, log *slog.Logger, reason string, retryable bool, after time.Duration) {
	kind := db.FailPermanent
	if retryable {
		kind = db.FailCrash
	}
	if len(reason) > maxReasonLen {
		reason = reason[:maxReasonLen]
	}
	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), exitBudget)
	defer cancel()
	outcome, err := w.deps.DB.FailAttemptAfter(ectx, job.ID, db.ClaimStamp(job), clipID, reason, kind, d.limits, w.now().Unix(), after)
	switch {
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		// Not this attempt's row to fail (§20 erratum 293): recording a
		// delivery attempt there would spend the current owner's budget.
		log.Info(d.noun+" job row is no longer this attempt's; attempt not recorded, row untouched", "reason", reason, "err", err)
	case err != nil:
		log.Error("fail "+d.noun+" attempt", "reason", reason, "err", err)
	case outcome == db.OutcomeFailed && !retryable:
		log.Warn(d.subject+" rejected; not retrying", "reason", reason)
	case outcome == db.OutcomeFailed:
		log.Warn(d.subject+" not delivered; giving up", "reason", reason,
			"attempt", job.Attempts+1, "max_attempts", d.limits.MaxAttempts)
	default:
		log.Warn(d.subject+" delivery failed; requeued", "reason", reason,
			"attempt", job.Attempts+1, "retry_after", max(after, db.Backoff(db.FailCrash, job.Attempts+1)))
	}
}
