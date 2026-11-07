package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"

	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/store"
	"clippet/internal/transcode"
)

// The FULL tier (DESIGN.md §17 #10, §20 errata 200–206).
//
// A `full` job exists for one shape of clip: an upload no browser can play as
// it stands, whose LIGHT copy the transcode run made FIRST so the share card
// exists in seconds rather than in minutes. This job is what then produces the
// faithful copy — the source's own resolution and frame rate, CRF 18 in
// libx264 or 32 in libsvtav1, no cap of any kind — and points `rendition_key`
// at it.
//
// It is the social job's twin in standing and its opposite in weight:
//
//  1. It reads the ORIGINAL, not a rendition. It is the only media kind that
//     does, and it has to: it is the tier that exists BECAUSE the original
//     cannot be served, so re-deriving it from anything else would be
//     re-encoding a downscale.
//  2. It writes the rendition columns, the five measurements and two tier
//     words, and nothing else. The clip is `ready` before, during and after;
//     db.SetFull enforces that on its side.
//  3. Its commit is guarded by the SOURCE's ETag and by the clip still being
//     untrimmed. A replacement changes the original under it; a trim makes its
//     own Full of a window. Either way the whole-file copy this job is carrying
//     describes a clip that no longer exists, and it is thrown away rather than
//     published.
//
// A failure of any of it costs the clip nothing but quality: the Light copy
// goes on playing at 720p30, `full_state` becomes `failed`, and the watch
// page's quality selector says so instead of offering a tier that is never
// coming.

// fullRun is the state of one full-tier attempt.
type fullRun struct {
	w    *worker
	ctx  context.Context
	run  *run
	job  db.Job
	clip db.Clip
	log  *slog.Logger

	dir   string
	ext   string
	r8    string
	key   string
	stage string

	srcPath, outPath string
	// attempted lists the keys this attempt STARTED writing, recorded before
	// the PutFile so a failed or aborted upload is cleaned up (§11).
	attempted []string

	// sourceETag is the original this copy is encoded from, read at the start
	// and handed to SetFull as the commit's predicate.
	sourceETag string

	probe  *transcode.ProbeResult
	plan   transcode.Plan
	out    *transcode.OutputInfo
	fields db.FullFields
}

func newFullRun(w *worker, ctx context.Context, r *run, job db.Job, clip db.Clip) *fullRun {
	f := &fullRun{w: w, ctx: ctx, run: r, job: job, clip: clip}
	f.log = w.log.With("clip", clip.ID, "job", job.ID)
	f.r8 = ids.Rand(8)
	f.ext = strings.TrimPrefix(path.Ext(clip.SourceKey), ".")
	if f.ext == "" {
		f.ext = store.DefaultExt
	}
	f.dir = filepath.Join(w.deps.Cfg.ScratchDir, clip.ID)
	// The SAME `source.{ext}` name the transcode and the trim use, deliberately:
	// this job runs minutes after the transcode that queued it, often on the
	// very copy that transcode left behind, and acquireSource's size re-check is
	// what makes reusing it safe (§10.2). A per-attempt name would re-download
	// a multi-gigabyte upload for nothing.
	f.srcPath = filepath.Join(f.dir, "source."+f.ext)
	f.outPath = filepath.Join(f.dir, "full-"+f.r8+".mp4")
	f.key = store.RenditionKey(w.deps.Cfg.S3.KeyPrefix, clip.ID, f.r8)
	return f
}

// execute runs one attempt under recover() and settles its outcome.
func (f *fullRun) execute() {
	f.log.Info("full started", "attempt", f.job.Attempts+1, "transient_failures", f.job.TransientFailures)
	err := f.guarded()
	defer func() {
		if p := recover(); p != nil {
			f.log.Error("full exit path panicked", "stage", f.stage, "err", fmt.Sprint(p), "stack", string(debug.Stack()))
		}
	}()
	f.finish(err)
}

func (f *fullRun) guarded() (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = &panicError{value: p, stack: debug.Stack()}
		}
	}()
	return f.pipeline()
}

func (f *fullRun) pipeline() error {
	if err := f.recheckClip(); err != nil {
		return err
	}
	f.setStage(stageAcquire)
	if err := f.acquire(); err != nil {
		return err
	}
	if err := f.encodeVerify(); err != nil {
		return err
	}
	f.setStage(stageStore)
	if err := f.store(); err != nil {
		return err
	}
	return f.commit()
}

// encodeVerify holds ONE of the TRANSCODE_CONCURRENCY ffmpeg slots (§20 erratum
// 79) across the probe, the encode and the two verify passes — every stage that
// spawns ffmpeg. It matters more here than for any other media kind: this is
// the longest encode in the system, and without the slot a default install
// would run it beside the next upload's transcode.
func (f *fullRun) encodeVerify() error {
	release, err := f.w.acquireFFmpeg(f.ctx, f.log)
	if err != nil {
		return err
	}
	defer release()
	f.setStage(stageProbe)
	if err := f.planEncode(); err != nil {
		return err
	}
	f.setStage(stageEncode)
	if err := f.encode(); err != nil {
		return err
	}
	f.setStage(stageVerify)
	return f.verify()
}

// recheckClip closes the claim→registration window and re-reads the clip this
// copy will describe. Every refusal here is a SKIP and not a failure: nothing
// is wrong with a clip that was deleted, replaced or trimmed while its Full sat
// in the queue, and the commit that did any of those has already queued
// whatever the new file needs.
//
// It also re-asks whether a Full is wanted at all, from the row in hand, for
// the reason erratum 169 gave the social job: the decision was made by a commit
// that has since been overtaken. `full_state = 'ready'` means a Full already
// exists — a trim landed, and its cut IS the Full — and re-encoding the whole
// file over it would silently un-cut the clip.
func (f *fullRun) recheckClip() error {
	c, err := f.w.deps.DB.GetClip(f.ctx, f.clip.ID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return skip("full", "clip deleted")
	case err != nil:
		return fmt.Errorf("re-read clip: %w", err)
	case c.State == db.ClipDeleting:
		return skip("full", "clip is being deleted")
	case c.State != db.ClipReady:
		return skip("full", "clip is "+c.State)
	case c.TrimStartMS != nil || c.TrimEndMS != nil:
		return skip("full", "the clip was trimmed; its cut is the full tier")
	case c.OriginalPlayable && !c.OriginalRemuxed:
		return skip("full", "the original plays as it stands")
	case c.FullState == db.TierReady:
		return skip("full", "a full tier already exists")
	case c.SourceETag == "":
		return skip("full", "the clip records no source etag to commit against")
	}
	f.clip = c
	f.sourceETag = c.SourceETag
	f.log.Info("full source", "key", c.SourceKey, "bytes", c.SourceSize, "codec", c.OriginalCodec)
	return nil
}

// acquire fetches the original on exactly the terms the transcode does — the
// scratch copy when it is there with the right size, a download otherwise, and
// the §10.2 free-space rule around it. It is literally acquireSource: an upload
// this job must not be able to admit under looser rules than the encode that
// produced its Light copy.
func (f *fullRun) acquire() error {
	return acquireSource(f.ctx, f.w, f.clip, f.dir, f.srcPath, f.log, f.transferCtx, nil)
}

// planEncode probes the original and plans the Full from it: the source's own
// long and short sides as the box, so the scale factor is exactly 1, and a frame
// rate ceiling that cannot bind below the rate the source itself ran at
// (transcode.FullPlanOptions).
//
// A source past the Full ceiling fails here rather than being downscaled — the
// transcode run refuses the same file for the same reason, so this is the belt
// for a row whose ceiling changed between the two.
func (f *fullRun) planEncode() error {
	p, err := f.w.runner.Probe(f.ctx, f.srcPath)
	if err != nil {
		return err
	}
	f.probe = p
	tiers := tierPlanFor(p, f.clip, f.w.deps.Cfg, false)
	if tiers.TooLarge {
		return &transcode.ErrPermanent{Err: fmt.Errorf("source_too_large: %s (%dx%d)",
			tiers.Reason, p.DisplayW, p.DisplayH)}
	}
	if tiers.Playable {
		// The original plays after all — the probe disagrees with the column the
		// transcode wrote, which can happen if the operator's ceiling moved. There
		// is nothing for this job to make.
		return skip("full", "the original plays as it stands")
	}
	f.plan = tiers.Full
	f.log.Info("full planned", "out", fmt.Sprintf("%dx%d", f.plan.W, f.plan.H), "fps", f.plan.FPS.String(),
		"audio", f.plan.AudioMode.String(), "codec", transcode.NormalizeFullCodec(f.w.deps.Cfg.Transcode.FullCodec),
		"duration_us", f.plan.DurationUS)
	return nil
}

// encode runs the one instrumented pass with the tier's own quality floor,
// under the same output budget every other encode gets: the bounds this clip has
// already been granted, and no new ones.
func (f *fullRun) encode() error {
	if err := checkOutputRoom(f.w, f.dir, f.plan); err != nil {
		return err
	}
	budget := outputBudget(f.clip.SourceSize, f.w.deps.Cfg.MaxUploadBytes, f.plan)
	if err := f.w.runner.EncodeFull(f.ctx, f.plan, f.srcPath, f.outPath, budget, nil); err != nil {
		return err
	}
	st, err := os.Stat(f.outPath)
	if err == nil && budget > 0 && st.Size() > budget {
		return &transcode.ErrPermanent{Err: fmt.Errorf(
			"output_too_large: the full tier is %d bytes, past the %d budgeted for it", st.Size(), budget)}
	}
	return nil
}

// verify measures what we produced against the plan EXACTLY — not against a box
// — because this tier has no box: one video stream in the configured codec, at
// most one aac track, the planned dimensions to the pixel, a positive duration
// and moov before mdat. The numbers it reports become the clip's own.
func (f *fullRun) verify() error {
	info, err := f.w.runner.ProbeOutput(f.ctx, f.outPath)
	if err != nil {
		return err
	}
	codec := transcode.NormalizeFullCodec(f.w.deps.Cfg.Transcode.FullCodec)
	if err := transcode.CheckTierOutput(info, transcode.TierCheck{
		Codecs: []string{codec}, Width: f.plan.W, Height: f.plan.H,
	}); err != nil {
		return err
	}
	if err := f.w.runner.Verify(f.outPath); err != nil {
		return err
	}
	st, err := os.Stat(f.outPath)
	if err != nil {
		return &transcode.ErrPermanent{Err: fmt.Errorf("bad_output: the full tier is unreadable: %w", err)}
	}
	f.out = info
	f.fields = db.FullFields{Key: f.key, Size: st.Size(), Width: info.Width, Height: info.Height,
		DurationMS: info.DurationMS, FPSNum: info.FPS.Num, FPSDen: info.FPS.Den,
		HasAudio: info.HasAudio, Codec: codec}
	f.log.Info("full encoded", "bytes", st.Size(), "dims", fmt.Sprintf("%dx%d", info.Width, info.Height),
		"fps", info.FPS.String(), "codec", codec, "source_bytes", f.clip.SourceSize)
	return nil
}

func (f *fullRun) store() error {
	f.attempted = append(f.attempted, f.key)
	ctx, cancel := f.transferCtx()
	defer cancel()
	res, err := f.w.deps.Store.PutFile(ctx, f.key, f.outPath, "video/mp4", nil)
	if err != nil {
		return fmt.Errorf("upload full tier: %w", err)
	}
	f.fields.Size, f.fields.ETag = res.Size, res.SHA256
	return nil
}

// commit is the one guarded UPDATE. A swap that does not happen is not a failure
// of anything: the attempt deletes its own object and resolves the job as done
// with the reason, because there is nothing to retry — the original this copy
// was encoded from is not the clip that exists now, and whatever replaced it has
// queued its own work.
func (f *fullRun) commit() error {
	f.setStage(stageFinalize)
	swap, err := f.w.deps.DB.SetFull(f.ctx, f.job.ID, f.clip.ID, f.fields, f.sourceETag, f.w.now().Unix())
	if err != nil {
		if f.ctx.Err() == nil {
			f.log.Error("set full failed", "err", err)
		}
		return fmt.Errorf("set full: %w", err)
	}
	if !swap.OK {
		f.skipped(swap.Reason)
		return errLostRace
	}
	ctx, cancel := f.exitCtx()
	deleteKeys(ctx, f.w.deps.Store, f.log, swap.Superseded)
	cancel()
	f.removeScratch()
	f.log.Info("full applied", "key", f.fields.Key, "bytes", f.fields.Size,
		"dims", fmt.Sprintf("%dx%d", f.fields.Width, f.fields.Height), "superseded", len(swap.Superseded))
	return nil
}

// skipped resolves the job as done with the reason and removes this attempt's
// object — the §11 lost-race shape, at INFO because it is an expected outcome of
// a concurrent trim or replacement.
func (f *fullRun) skipped(reason string) {
	ctx, cancel := f.exitCtx()
	defer cancel()
	f.complete(db.JobStageSkipped, "skipped: "+reason)
	// A lost race is a finished answer about the tier too (§20 erratum 237). A
	// commit that queued a replacement Full superseded this row first (237 (a)),
	// so that ending never reaches here; this one is a race no commit queued a
	// job behind. The guard is what makes the line safe: when a job is live, or
	// the winning commit published a Full of its own, it writes nothing.
	f.markSkipped()
	deleteKeys(ctx, f.w.deps.Store, f.log, f.attempted)
	f.removeScratch()
	f.log.Info("full skipped; the faithful copy was not published", "reason", reason)
}

// finish settles the attempt (§11 classification) with the media kinds' one
// extra rule: whatever happened, the CLIP is untouched — db.FailAttempt moves a
// clip only for `transcode`, and a full job's crash cap is db.FullMaxAttempts
// (its environment cap is the instance's, erratum 89).
func (f *fullRun) finish(err error) {
	var pe *panicError
	switch {
	case err == nil, errors.Is(err, errLostRace):
		return
	case errors.Is(err, errSkipped):
		f.complete(db.JobStageSkipped, err.Error())
		// A skip is a finished answer about the tier too: nothing is coming, and
		// the selector must not go on saying `encoding` for a job that stopped.
		f.markSkipped()
		f.removeScratch()
		f.log.Info("full skipped", "reason", err.Error())
	case errors.As(err, &pe):
		f.log.Error("full job panicked", "stage", f.stage, "err", pe.Error(), "cancelled", f.ctx.Err() != nil, "stack", string(pe.stack))
		if f.ctx.Err() != nil {
			f.release()
			return
		}
		f.fail(db.FailCrash, err)
	case f.ctx.Err() != nil, errors.Is(err, errStopping):
		f.release()
	case errors.Is(err, errNoScratchRoom):
		f.noRoom(err)
	case transcode.IsPermanent(err):
		f.fail(db.FailPermanent, err)
	default:
		f.fail(db.FailTransient, err)
	}
}

// markSkipped records `full_state = skipped` for a job that will not run, but
// ONLY while the row still promises `encoding` and no `full` job is left to keep
// that promise — db.SettleTierEncoding's guard (§20 erratum 237). A skip caused by
// a trim or a replacement arrives after that commit wrote its own answer, and
// overwriting it would replace a fact with a footnote; a skip that races the
// commit's REPLACEMENT job would tell the selector that a queued encode is not
// coming. Both are the same guard, and it lives in the statement rather than in a
// read here, which is what makes it a guard at all.
func (f *fullRun) markSkipped() {
	ctx, cancel := f.exitCtx()
	defer cancel()
	settleTier(ctx, f.w, f.log, f.clip.ID, db.KindFull, db.TierSkipped)
}

// noRoom is the §20 erratum 90 ending: requeued with a backoff, both counters
// untouched, this attempt's object removed. The SOURCE is kept for the retry.
func (f *fullRun) noRoom(err error) {
	ctx, cancel := f.exitCtx()
	defer cancel()
	if lost := requeueNoCount(ctx, f.w, f.log, f.job, f.clip.ID, f.stage, msgNoScratchRoom, f.reason(err)); lost != nil {
		f.lostAttempt(lost, f.reason(err))
		return
	}
	deleteKeys(ctx, f.w.deps.Store, f.log, f.attempted)
	f.removeScratch()
}

// lostAttempt is the §20 erratum 293 ending, this kind's copy of the one
// erratum 291 gave the transcode: the row stopped being this attempt's — a
// repair took it and a NEWER attempt may hold it, or MarkDeleting cancelled it
// with the clip — so the row is NOT touched (an attempt recorded there spends
// the current owner's counter) and only what this attempt made is discarded.
// The TIER column is left alone too, and for once that is the whole argument
// for it: `full_state` still says `encoding`, and it is the attempt that holds
// the row that will settle it — markSkipped and the failed ending are both
// answers about a job that has ended, which this one has not.
func (f *fullRun) lostAttempt(err error, detail string) {
	ctx, cancel := f.exitCtx()
	defer cancel()
	f.log.Info("full job row is no longer this attempt's; outputs discarded, row untouched",
		"stage", f.stage, "err", err, "detail", detail)
	deleteKeys(ctx, f.w.deps.Store, f.log, f.attempted)
	f.removeScratch()
}

// complete resolves a job that must not run as `done` with the reason in
// last_error (CompleteJob refuses a transcode job, which is the point).
func (f *fullRun) complete(stage, note string) {
	ctx, cancel := f.exitCtx()
	defer cancel()
	err := f.w.deps.DB.CompleteJob(ctx, f.job.ID, db.ClaimStamp(f.job), stage, note, f.w.now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		// The markSkipped() its two callers run next is safe over this ending
		// without a check of its own: SettleTierEncoding's guard refuses while
		// any job of the kind is queued or running, and a repaired row is one
		// or the other (§20 errata 237 and 293).
		f.lostAttempt(err, note)
	default:
		f.log.Error("complete full job", "note", note, "err", err)
	}
}

// release is the Cancel/Stop path: the job returns to queued with both counters
// untouched and this attempt's object is removed — a full job keeps no
// checkpoint, so a retry redoes the work under a fresh {r8}. The tier state is
// left at `encoding`, which is exactly what it still is.
func (f *fullRun) release() {
	why := f.run.reason()
	if why == reasonNone {
		why = reasonShutdown
	}
	ctx, cancel := f.exitCtx()
	err := f.w.deps.DB.ReleaseJob(ctx, f.job.ID, db.ClaimStamp(f.job), f.clip.ID, f.w.now().Unix())
	switch {
	case err == nil:
		f.log.Info("full job released", "reason", why.String(), "stage", f.stage)
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		cancel()
		f.lostAttempt(err, why.String())
		return
	default:
		f.log.Error("release full job", "reason", why.String(), "err", err)
	}
	deleteKeys(ctx, f.w.deps.Store, f.log, f.attempted)
	cancel()
	f.removeScratch()
}

// fail records one bad ending. The clip keeps its Light copy, its poster and its
// readiness; only the better tier is missing, and the selector says so.
func (f *fullRun) fail(kind db.FailKind, cause error) {
	reason, detail := f.reason(cause), cause.Error()
	ctx, cancel := f.exitCtx()
	defer cancel()
	outcome, err := f.w.deps.DB.FailAttempt(ctx, f.job.ID, db.ClaimStamp(f.job), f.clip.ID, reason, kind, f.w.lim, f.w.now().Unix())
	switch {
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		// Not this attempt's row to fail (§20 erratum 293).
		f.lostAttempt(err, detail)
		return
	case err != nil:
		f.log.Error("fail full attempt", "kind", kindName(kind), "reason", reason, "detail", detail, "err", err)
	case outcome == db.OutcomeFailed && kind == db.FailPermanent && strings.Contains(reason, "bad_output"):
		// A verify mismatch on our own output cannot happen by design (§12).
		f.log.Error("full failed: bad_output from our own encoder", "reason", reason, "detail", detail)
	case outcome == db.OutcomeFailed:
		f.log.Info("full failed; the clip keeps playing its light copy",
			"kind", kindName(kind), "reason", reason, "detail", detail, "cap", db.Cap(db.KindFull, kind, f.w.lim))
	default:
		f.log.Warn("full attempt failed; requeued", "kind", kindName(kind), "reason", reason, "detail", detail)
	}
	if outcome == db.OutcomeFailed {
		settleTier(ctx, f.w, f.log, f.clip.ID, db.KindFull, db.TierFailed)
	}
	deleteKeys(ctx, f.w.deps.Store, f.log, f.attempted)
	f.removeScratch()
}

// reason renders the stored failure text. A full job's last_error reaches no
// public route, but it is written by the same helper as every other kind's so a
// bucket endpoint can never start leaking through a new one.
func (f *fullRun) reason(err error) string {
	return publicReason(err, f.stage, f.dir, f.w.deps.Cfg)
}

func (f *fullRun) transferCtx() (context.Context, context.CancelFunc) {
	if m := f.w.deps.Cfg.S3.TransferMax; m > 0 {
		return context.WithTimeout(f.ctx, m)
	}
	return context.WithCancel(f.ctx)
}

func (f *fullRun) exitCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(f.ctx), exitBudget)
}

// removeScratch removes only THIS attempt's output and, like the social run,
// tries the directory afterwards in case nothing else is using it. The SOURCE
// stays: a retry, and any later trim, wants it.
func (f *fullRun) removeScratch() {
	if err := os.Remove(f.outPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		f.log.Warn("remove scratch output", "path", f.outPath, "err", err)
	}
	if err := os.Remove(f.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		f.log.Debug("scratch dir kept", "dir", f.dir, "err", err)
	}
}

// setStage persists the stage. Like the social and gif jobs a full job has NO
// progress registry entry: the clip it belongs to is `ready` throughout, so
// §8.4's registry — whose subject is the clip's own state — must not be told a
// percentage for it (§20 erratum 206). GET …/progress reports the TIER states
// instead, which is the honest shape of "this clip plays, and a better copy is
// on its way".
func (f *fullRun) setStage(stage string) {
	f.stage = stage
	err := f.w.deps.DB.SetJobStage(f.ctx, f.job.ID, db.ClaimStamp(f.job), stage, f.w.now().Unix())
	switch {
	case err == nil, f.ctx.Err() != nil:
	case errors.Is(err, db.ErrNotFound):
		f.log.Info("full job row gone while setting stage", "stage", stage)
	case errors.Is(err, db.ErrStateMismatch):
		// Repaired or cancelled under another hand (§20 erratum 291): the stage is
		// abandoned, the run goes on, and the lost row ends it at the next write
		// that matters — never as a failure of the row's new owner.
		f.log.Info("full job row is no longer this attempt's; stage not recorded", "stage", stage)
	default:
		f.log.Error("set full job stage", "stage", stage, "err", err)
	}
}

// settleTier is the shared, best-effort tier-state write of the two jobs that can
// end without producing their tier (§20 erratum 205, guarded by erratum 237). It
// is a footnote on a job's ending: a failure to record it is logged and dropped,
// because the job's own outcome is already settled and re-running it for a word
// would be worse.
//
// db.SettleTierEncoding carries the guard, so every caller here may be read as
// "if this tier is still promised and nothing of this kind is left, say what
// happened" — which is why the two kinds' settling endings share one line.
func settleTier(ctx context.Context, w *worker, log *slog.Logger, clipID, kind, state string) {
	if err := w.deps.DB.SettleTierEncoding(ctx, clipID, kind, state, w.now().Unix()); err != nil {
		log.Warn("settle tier state", "kind", kind, "state", state, "err", err)
	}
}
