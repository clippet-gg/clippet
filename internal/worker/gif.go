package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/store"
	"clippet/internal/transcode"
)

// GIF export (DESIGN.md §17 #6, §20 errata 73+).
//
// A gif job is the smallest thing on the non-transcode loop that still
// touches ffmpeg, and it is deliberately the least privileged run in the
// system:
//
//  1. It reads the RENDITION, never the original. The rendition is already
//     normalised and bounded (720p, ≤60 fps, faststart mp4), so the preview's
//     cost is bounded by a file we produced rather than by one an uploader
//     chose — and a GIF of the clip people are watching is the only honest
//     preview of it. This is the one derived artefact that is NOT re-derived
//     from the upload, and the reason is exactly that.
//  2. It writes five columns and nothing else. The clip is `ready` before,
//     during and after; db.SetGIF enforces that on its side.
//  3. Its commit is guarded by the rendition's ETag. A trim that lands while
//     this job runs replaces the very frames the preview was cut from, so the
//     output is thrown away rather than published — a stale preview is worse
//     than none, because nothing downstream could ever tell.
//
// It is REGISTERED in the run registry (erratum 71's rule, unchanged): it
// runs ffmpeg, writes scratch and uploads an object, which is precisely the
// list §10.1's Cancel promises to have finished when it returns.

// gifRun is the state of one gif attempt.
type gifRun struct {
	w    *worker
	ctx  context.Context
	run  *run
	job  db.Job
	clip db.Clip
	log  *slog.Logger

	dir   string
	r8    string
	key   string
	stage string

	srcPath, palettePath, outPath string
	// attempted lists the keys this attempt STARTED writing, recorded before
	// the PutFile so a failed or aborted upload is cleaned up (§11).
	attempted []string

	// renditionKey/renditionSize/renditionETag are the PLAYABLE FILE this
	// preview is cut from (db.PlayObject), read at the
	// start and handed to SetGIF as the commit's predicate.
	renditionKey  string
	renditionSize int64
	renditionETag string
	// durationMS/width are the rendition's own numbers, read from the row at
	// the start and turned into the plan once the file is on disk.
	durationMS int64
	width      int
	plan       transcode.GIFPlan
	fields     db.GIFFields
}

func newGIFRun(w *worker, ctx context.Context, r *run, job db.Job, clip db.Clip) *gifRun {
	g := &gifRun{w: w, ctx: ctx, run: r, job: job, clip: clip}
	g.log = w.log.With("clip", clip.ID, "job", job.ID)
	g.r8 = ids.Rand(8)
	g.dir = filepath.Join(w.deps.Cfg.ScratchDir, clip.ID)
	g.srcPath = filepath.Join(g.dir, "gifsrc-"+g.r8+".mp4")
	g.palettePath = filepath.Join(g.dir, "palette-"+g.r8+".png")
	g.outPath = filepath.Join(g.dir, "preview-"+g.r8+".gif")
	g.key = store.GIFKey(w.deps.Cfg.S3.KeyPrefix, clip.ID, g.r8)
	return g
}

// execute runs one attempt under recover() and settles its outcome.
func (g *gifRun) execute() {
	g.log.Info("gif started", "attempt", g.job.Attempts+1, "transient_failures", g.job.TransientFailures)
	err := g.guarded()
	defer func() {
		if p := recover(); p != nil {
			g.log.Error("gif exit path panicked", "stage", g.stage, "err", fmt.Sprint(p), "stack", string(debug.Stack()))
		}
	}()
	g.finish(err)
}

func (g *gifRun) guarded() (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = &panicError{value: p, stack: debug.Stack()}
		}
	}()
	return g.pipeline()
}

func (g *gifRun) pipeline() error {
	if err := g.recheckClip(); err != nil {
		return err
	}
	g.setStage(stageAcquire)
	if err := g.acquire(); err != nil {
		return err
	}
	if err := g.renderVerify(); err != nil {
		return err
	}
	g.setStage(stageStore)
	if err := g.store(); err != nil {
		return err
	}
	return g.commit()
}

// renderVerify runs the poster-frame pick, the two palette passes and the
// probe of their result holding ONE of the TRANSCODE_CONCURRENCY ffmpeg slots
// (§20 erratum 79). A preview is small and bounded, but it is still ffmpeg on
// the same box as the encodes an operator sized that limit for — and it now
// runs on a loop of its own, so without the slot a default install could have
// two. The pick pass is inside the slot for the same reason and because it is
// what decides the window the render then uses.
func (g *gifRun) renderVerify() error {
	release, err := g.w.acquireFFmpeg(g.ctx, g.log)
	if err != nil {
		return err
	}
	defer release()
	g.setStage(stageGIF)
	g.planGIF()
	if err := g.render(); err != nil {
		return err
	}
	g.setStage(stageVerify)
	return g.verify()
}

// recheckClip closes the claim→registration window and reads the rendition
// this preview will describe. A clip that is no longer ready — or that has
// somehow no rendition — is skipped, not failed: nothing is wrong with it.
func (g *gifRun) recheckClip() error {
	c, err := g.w.deps.DB.GetClip(g.ctx, g.clip.ID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return skip("gif", "clip deleted")
	case err != nil:
		return fmt.Errorf("re-read clip: %w", err)
	case c.State == db.ClipDeleting:
		return skip("gif", "clip is being deleted")
	case c.State != db.ClipReady:
		return skip("gif", "clip is "+c.State)
	}
	// The PLAYABLE FILE this clip is, resolved from the row: the rendition when
	// there is one, and the UPLOAD itself on the remux fast path, where a
	// playable original is served as it stands and there is no rendition object
	// at all (db.PlayObject, §20 erratum 201).
	key, size, etag, ok := db.PlayObject(c)
	if !ok {
		return skip("gif", "clip has no playable file to preview")
	}
	g.clip = c
	g.renditionKey, g.renditionSize, g.renditionETag = key, size, etag
	if c.Width != nil {
		g.width = *c.Width
	}
	if c.DurationMS != nil {
		g.durationMS = *c.DurationMS
	}
	g.log.Info("gif source", "duration_ms", g.durationMS, "width", g.width,
		"key", key, "bytes", size)
	return nil
}

// planGIF decides the window: the preview starts where the poster was taken
// (§20 erratum 92(c), amended), and that is now one of two facts.
//
// When the OWNER chose the card image, the clip STORES that moment and this
// is a subtraction — the preview opens on the very frame the card shows,
// which is most of the reason for having chosen one, and the extra ffmpeg
// pass below is not run at all (§20 erratum 131).
//
// Otherwise the poster's timestamp was chosen by a thumbnail pass and
// nothing stores it — an automatic poster writes five columns and holds no
// time — so the same selection is re-run here, on the rendition already on
// disk, under the ffmpeg slot this stage holds. It cannot fail: a pass that
// says nothing yields the blind seek, which is what this job used before and
// is still a legal window.
//
// The rendition's video extent is not measured (it would cost a probe of a
// file we already trust), so a rendition whose picture stops early is
// clamped only by its container duration, exactly as before.
func (g *gifRun) planGIF() {
	poster, from := g.posterSeconds()
	g.plan = transcode.PlanGIF(g.durationMS, g.width, poster)
	g.log.Info("gif planned", "poster_at", poster, "poster_from", from, "start", g.plan.StartSec,
		"seconds", g.plan.Seconds, "fps", g.plan.FPS, "width", g.plan.Width)
}

// posterSeconds answers "where is the poster's frame on this rendition", and
// says which of the two ways it knows.
func (g *gifRun) posterSeconds() (float64, string) {
	if s, ok := renditionPosterSeconds(g.clip); ok {
		return s, "chosen"
	}
	return g.w.runner.PosterFrameSeconds(g.ctx, g.srcPath, g.durationMS*1000, g.durationMS > 0, 0), "derived"
}

// acquire downloads the rendition to scratch under a per-attempt name. It is
// never cached between attempts: a rendition is already small and bounded,
// and a stale copy of the WRONG rendition is exactly the failure this
// feature must not have.
func (g *gifRun) acquire() error {
	if err := os.MkdirAll(g.dir, 0o755); err != nil {
		return fmt.Errorf("scratch dir: %w", err)
	}
	size := g.renditionSize
	// The file is downloaded per attempt (never cached), so its bytes
	// ARE about to land: they are counted once, here, exactly as
	// acquireSource counts a source it is about to fetch.
	if err := requireScratch(g.w, g.dir, satAdd(satAdd(size, transcode.GIFMaxBytes), g.w.deps.Cfg.ScratchMinFree)); err != nil {
		return err
	}
	ctx, cancel := g.transferCtx()
	err := g.w.deps.Store.Download(ctx, g.renditionKey, g.srcPath, nil)
	cancel()
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// A ready row pointing at an object the bucket does not hold is
			// the drift §12 says cannot happen (ERROR); the attempt is still
			// transient, because a lagging bucket looks the same.
			g.log.Error("playable file missing from the bucket (row/object drift)", "key", g.renditionKey)
		}
		return fmt.Errorf("download rendition: %w", err)
	}
	st, err := os.Stat(g.srcPath)
	if err != nil {
		return fmt.Errorf("downloaded rendition: %w", err)
	}
	if st.Size() != size {
		return fmt.Errorf("downloaded rendition is %d bytes, row says %d", st.Size(), size)
	}
	return nil
}

// render runs the two palette passes under the byte cap and the hard timeout.
func (g *gifRun) render() error {
	return g.w.runner.GIF(g.ctx, g.plan, g.srcPath, g.palettePath, g.outPath)
}

// verify measures what we produced: a gif, with frames, no wider than the
// plan, and inside the byte cap even if ffmpeg reported nothing while it ran.
func (g *gifRun) verify() error {
	info, err := g.w.runner.ProbeGIF(g.ctx, g.outPath)
	if err != nil {
		return err
	}
	if err := transcode.CheckGIF(info, g.plan); err != nil {
		return err
	}
	st, err := os.Stat(g.outPath)
	if err != nil {
		return &transcode.ErrPermanent{Err: fmt.Errorf("bad_output: the gif is unreadable: %w", err)}
	}
	if st.Size() > transcode.GIFMaxBytes {
		return &transcode.ErrPermanent{Err: fmt.Errorf(
			"output_too_large: the gif is %d bytes, past the %d budgeted for it", st.Size(), transcode.GIFMaxBytes)}
	}
	g.fields = db.GIFFields{Key: g.key, Size: st.Size(), Width: info.Width, Height: info.Height}
	g.log.Info("gif rendered", "bytes", st.Size(), "dims", fmt.Sprintf("%dx%d", info.Width, info.Height), "frames", info.Frames)
	return nil
}

func (g *gifRun) store() error {
	g.attempted = append(g.attempted, g.key)
	ctx, cancel := g.transferCtx()
	defer cancel()
	res, err := g.w.deps.Store.PutFile(ctx, g.key, g.outPath, "image/gif", nil)
	if err != nil {
		return fmt.Errorf("upload gif: %w", err)
	}
	g.fields.Size, g.fields.ETag = res.Size, res.SHA256
	return nil
}

// commit is the one guarded UPDATE. A swap that does not happen is not a
// failure of anything: the attempt deletes its own object and resolves the
// job as done with the reason, because there is nothing to retry — the clip
// this preview described is not the clip that exists now, and the route will
// queue a fresh job the next time somebody asks.
func (g *gifRun) commit() error {
	g.setStage(stageFinalize)
	swap, err := g.w.deps.DB.SetGIF(g.ctx, g.job.ID, g.clip.ID, g.fields, g.renditionETag, g.w.now().Unix())
	if err != nil {
		if g.ctx.Err() == nil {
			g.log.Error("set gif failed", "err", err)
		}
		return fmt.Errorf("set gif: %w", err)
	}
	if !swap.OK {
		g.skipped(swap.Reason)
		return errLostRace
	}
	ctx, cancel := g.exitCtx()
	deleteKeys(ctx, g.w.deps.Store, g.log, swap.Superseded)
	cancel()
	g.removeScratch()
	g.log.Info("gif applied", "key", g.fields.Key, "bytes", g.fields.Size,
		"dims", fmt.Sprintf("%dx%d", g.fields.Width, g.fields.Height), "superseded", len(swap.Superseded))
	return nil
}

// skipped resolves the job as done with the reason and removes this
// attempt's object — the §11 lost-race shape, at INFO because it is an
// expected outcome of a concurrent trim.
func (g *gifRun) skipped(reason string) {
	ctx, cancel := g.exitCtx()
	defer cancel()
	g.complete(db.JobStageSkipped, "skipped: "+reason)
	deleteKeys(ctx, g.w.deps.Store, g.log, g.attempted)
	g.removeScratch()
	g.log.Info("gif skipped; the preview was not published", "reason", reason)
}

// finish settles the attempt (§11 classification) with the trim's one extra
// rule: whatever happened, the CLIP is untouched — db.FailAttempt moves a
// clip only for `transcode`, and a gif job's crash cap is db.GIFMaxAttempts
// (its environment cap is the instance's, §20 erratum 89).
func (g *gifRun) finish(err error) {
	var pe *panicError
	switch {
	case err == nil, errors.Is(err, errLostRace):
		return
	case errors.Is(err, errSkipped):
		g.complete(db.JobStageSkipped, err.Error())
		g.removeScratch()
		g.log.Info("gif skipped", "reason", err.Error())
	case errors.As(err, &pe):
		g.log.Error("gif job panicked", "stage", g.stage, "err", pe.Error(), "cancelled", g.ctx.Err() != nil, "stack", string(pe.stack))
		if g.ctx.Err() != nil {
			g.release()
			return
		}
		g.fail(db.FailCrash, err)
	case g.ctx.Err() != nil, errors.Is(err, errStopping):
		g.release()
	case errors.Is(err, errNoScratchRoom):
		g.noRoom(err)
	case transcode.IsPermanent(err):
		g.fail(db.FailPermanent, err)
	default:
		g.fail(db.FailTransient, err)
	}
}

// noRoom is the §20 erratum 90 ending: requeued with a backoff, both counters
// untouched, this attempt's object removed. A preview that cannot find disk
// space is not a preview that will never exist.
func (g *gifRun) noRoom(err error) {
	ctx, cancel := g.exitCtx()
	defer cancel()
	if lost := requeueNoCount(ctx, g.w, g.log, g.job, g.clip.ID, g.stage, msgNoScratchRoom, g.reason(err)); lost != nil {
		g.lostAttempt(lost, g.reason(err))
		return
	}
	deleteKeys(ctx, g.w.deps.Store, g.log, g.attempted)
	g.removeScratch()
}

// lostAttempt is the §20 erratum 293 ending, this kind's copy of the one
// erratum 291 gave the transcode: the row stopped being this attempt's — a
// repair took it and a NEWER attempt may hold it, or MarkDeleting cancelled it
// with the clip — so the row is NOT touched (an attempt recorded there spends
// the current owner's counter) and only what this attempt made is discarded. A
// gif keeps no checkpoint, so nothing references it; the clip, which was ready
// throughout, is untouched exactly as it always is.
func (g *gifRun) lostAttempt(err error, detail string) {
	ctx, cancel := g.exitCtx()
	defer cancel()
	g.log.Info("gif job row is no longer this attempt's; outputs discarded, row untouched",
		"stage", g.stage, "err", err, "detail", detail)
	deleteKeys(ctx, g.w.deps.Store, g.log, g.attempted)
	g.removeScratch()
}

// complete resolves a job that must not run as `done` with the reason in
// last_error (CompleteJob refuses a transcode job, which is the point).
func (g *gifRun) complete(stage, note string) {
	ctx, cancel := g.exitCtx()
	defer cancel()
	err := g.w.deps.DB.CompleteJob(ctx, g.job.ID, db.ClaimStamp(g.job), stage, note, g.w.now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		g.lostAttempt(err, note)
	default:
		g.log.Error("complete gif job", "note", note, "err", err)
	}
}

// release is the Cancel/Stop path: the job returns to queued with both
// counters untouched and this attempt's object is removed — a gif keeps no
// checkpoint, so a retry simply redoes the work under a fresh {r8}.
func (g *gifRun) release() {
	why := g.run.reason()
	if why == reasonNone {
		why = reasonShutdown
	}
	ctx, cancel := g.exitCtx()
	err := g.w.deps.DB.ReleaseJob(ctx, g.job.ID, db.ClaimStamp(g.job), g.clip.ID, g.w.now().Unix())
	switch {
	case err == nil:
		g.log.Info("gif job released", "reason", why.String(), "stage", g.stage)
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		cancel()
		g.lostAttempt(err, why.String())
		return
	default:
		g.log.Error("release gif job", "reason", why.String(), "err", err)
	}
	deleteKeys(ctx, g.w.deps.Store, g.log, g.attempted)
	cancel()
	g.removeScratch()
}

// fail records one bad ending. The clip keeps its rendition, its poster and
// its readiness; only the preview is missing, and the route says so. Like the
// transcode's, it takes the CAUSE: `reason` is the bounded text the anonymous
// 503 prints, `detail` the whole error for the log (§20 erratum 90).
func (g *gifRun) fail(kind db.FailKind, cause error) {
	reason, detail := g.reason(cause), cause.Error()
	ctx, cancel := g.exitCtx()
	defer cancel()
	outcome, err := g.w.deps.DB.FailAttempt(ctx, g.job.ID, db.ClaimStamp(g.job), g.clip.ID, reason, kind, g.w.lim, g.w.now().Unix())
	switch {
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		// Not this attempt's row to fail (§20 erratum 293).
		g.lostAttempt(err, detail)
		return
	case err != nil:
		g.log.Error("fail gif attempt", "kind", kindName(kind), "reason", reason, "detail", detail, "err", err)
	case outcome == db.OutcomeFailed && kind == db.FailPermanent && strings.Contains(reason, "bad_output"):
		// A verify mismatch on our own output cannot happen by design (§12).
		g.log.Error("gif failed: bad_output from our own gif", "reason", reason, "detail", detail)
	case outcome == db.OutcomeFailed:
		// The cap named is the one this ending exhausted: the crash cap is
		// db.GIFMaxAttempts, the environment cap is the instance's
		// TRANSCODE_MAX_TRANSIENT (§20 erratum 89).
		g.log.Info("gif failed; the clip is unaffected", "kind", kindName(kind), "reason", reason, "detail", detail,
			"cap", db.Cap(db.KindGIF, kind, g.w.lim))
	default:
		g.log.Warn("gif attempt failed; requeued", "kind", kindName(kind), "reason", reason, "detail", detail)
	}
	deleteKeys(ctx, g.w.deps.Store, g.log, g.attempted)
	g.removeScratch()
}

// reason renders the stored failure text. It reaches an ANONYMOUS caller (the
// route's 503 names why the preview is missing), so publicReason's storage
// class and its redaction are load-bearing here rather than merely tidy.
func (g *gifRun) reason(err error) string {
	return publicReason(err, g.stage, g.dir, g.w.deps.Cfg)
}

func (g *gifRun) transferCtx() (context.Context, context.CancelFunc) {
	if m := g.w.deps.Cfg.S3.TransferMax; m > 0 {
		return context.WithTimeout(g.ctx, m)
	}
	return context.WithCancel(g.ctx)
}

func (g *gifRun) exitCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(g.ctx), exitBudget)
}

// removeScratch removes only THIS attempt's three files. Unlike the
// transcode and the trim, a gif run never owns the clip's scratch dir: it
// did not put the source there and a trim may be about to reuse it.
func (g *gifRun) removeScratch() {
	for _, p := range []string{g.srcPath, g.palettePath, g.outPath} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			g.log.Warn("remove scratch output", "path", p, "err", err)
		}
	}
	// The dir is ours to drop only when nothing else is in it (the ordinary
	// case: a ready clip's source was removed at finalize).
	if err := os.Remove(g.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		g.log.Debug("scratch dir kept", "dir", g.dir, "err", err)
	}
}

// setStage persists the stage. A gif job has NO progress registry — neither
// §8.4's, whose subject is the clip (ready throughout), nor the trim's: its
// only public state is `queued`/`running`/`failed` on the route, and a
// percentage nobody can ask for is a registry nobody can evict.
func (g *gifRun) setStage(stage string) {
	g.stage = stage
	err := g.w.deps.DB.SetJobStage(g.ctx, g.job.ID, db.ClaimStamp(g.job), stage, g.w.now().Unix())
	switch {
	case err == nil, g.ctx.Err() != nil:
	case errors.Is(err, db.ErrNotFound):
		g.log.Info("gif job row gone while setting stage", "stage", stage)
	case errors.Is(err, db.ErrStateMismatch):
		// Repaired or cancelled under another hand (§20 erratum 291): the stage is
		// abandoned, the run goes on, and the lost row ends it at the next write
		// that matters — never as a failure of the row's new owner.
		g.log.Info("gif job row is no longer this attempt's; stage not recorded", "stage", stage)
	default:
		g.log.Error("set gif job stage", "stage", stage, "err", err)
	}
}
