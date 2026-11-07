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

// The trim scrubber's filmstrip (§20 erratum 220; ROADMAP #6's open half).
//
// It is the gif job's shape, and deliberately so: the two are the same kind of
// thing — a bounded, derived, re-derivable artefact of the CURRENT rendition
// that nobody has asked for until somebody opens a control — and they obey the
// same three rules:
//
//  1. It reads the PLAYABLE FILE — `db.PlayObject`, the same file `/video.mp4`
//     serves — and never the upload behind it. The tiles a scrubber shows must
//     be the frames the player can seek to, and after a cut the upload holds
//     footage the player does not. Since the quality tiers that file is the
//     rendition when there is one and the UPLOAD ITSELF on the remux fast path,
//     where a playable original is served as it stands and there is no rendition
//     object at all (§20 erratum 201) — which is the common case now, so reading
//     `rendition_key` alone would leave the commonest clips with no sheet.
//  2. It writes seven columns and nothing else. The clip is `ready` before,
//     during and after; db.SetFilmstrip enforces that on its side.
//  3. Its commit is guarded by that file's ETag. A trim that lands while
//     this job runs replaces the very frames the sheet was cut from, so the
//     output is thrown away rather than published — a stale filmstrip is worse
//     than none, because its tiles would sit under handles naming moments it
//     does not show, and nothing downstream could tell.
//
// It is REGISTERED in the run registry (erratum 71's rule): it runs ffmpeg,
// writes scratch and uploads an object, which is precisely the list §10.1's
// Cancel promises to have finished when it returns. And it holds one of the
// TRANSCODE_CONCURRENCY ffmpeg slots (erratum 79) — its pass decodes a whole
// rendition, which is exactly the kind of work that limit exists to bound.

// filmstripRun is the state of one filmstrip attempt.
type filmstripRun struct {
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

	srcPath, outPath string
	// attempted lists the keys this attempt STARTED writing, recorded before
	// the PutFile so a failed or aborted upload is cleaned up (§11).
	attempted []string

	// renditionKey/renditionSize/renditionETag are the PLAYABLE FILE this sheet
	// is sampled from (db.PlayObject), read at the start; the etag is handed to
	// SetFilmstrip as the commit's predicate.
	renditionKey  string
	renditionSize int64
	renditionETag string
	// durationMS/width are the rendition's own measurements, read from the row
	// and turned into the plan.
	durationMS int64
	width      int
	plan       transcode.FilmstripPlan
	fields     db.FilmstripFields
}

func newFilmstripRun(w *worker, ctx context.Context, r *run, job db.Job, clip db.Clip) *filmstripRun {
	f := &filmstripRun{w: w, ctx: ctx, run: r, job: job, clip: clip}
	f.log = w.log.With("clip", clip.ID, "job", job.ID)
	f.r8 = ids.Rand(8)
	f.dir = filepath.Join(w.deps.Cfg.ScratchDir, clip.ID)
	f.srcPath = filepath.Join(f.dir, "stripsrc-"+f.r8+".mp4")
	f.outPath = filepath.Join(f.dir, "strip-"+f.r8+".jpg")
	f.key = store.FilmstripKey(w.deps.Cfg.S3.KeyPrefix, clip.ID, f.r8)
	return f
}

// execute runs one attempt under recover() and settles its outcome.
func (f *filmstripRun) execute() {
	f.log.Info("filmstrip started", "attempt", f.job.Attempts+1, "transient_failures", f.job.TransientFailures)
	err := f.guarded()
	defer func() {
		if p := recover(); p != nil {
			f.log.Error("filmstrip exit path panicked", "stage", f.stage, "err", fmt.Sprint(p), "stack", string(debug.Stack()))
		}
	}()
	f.finish(err)
}

func (f *filmstripRun) guarded() (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = &panicError{value: p, stack: debug.Stack()}
		}
	}()
	return f.pipeline()
}

func (f *filmstripRun) pipeline() error {
	if err := f.recheckClip(); err != nil {
		return err
	}
	f.setStage(stageAcquire)
	if err := f.acquire(); err != nil {
		return err
	}
	if err := f.renderVerify(); err != nil {
		return err
	}
	f.setStage(stageStore)
	if err := f.store(); err != nil {
		return err
	}
	return f.commit()
}

// recheckClip closes the claim→registration window and reads the playable file
// this sheet will describe. A clip that is no longer ready — or that has
// somehow no playable bytes — is skipped, not failed: nothing is wrong with it.
func (f *filmstripRun) recheckClip() error {
	c, err := f.w.deps.DB.GetClip(f.ctx, f.clip.ID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return skip("filmstrip", "clip deleted")
	case err != nil:
		return fmt.Errorf("re-read clip: %w", err)
	case c.State == db.ClipDeleting:
		return skip("filmstrip", "clip is being deleted")
	case c.State != db.ClipReady:
		return skip("filmstrip", "clip is "+c.State)
	}
	// The PLAYABLE FILE this clip is, resolved from the row: the rendition when
	// there is one, and the UPLOAD itself on the remux fast path, where a
	// playable original is served as it stands and there is no rendition object
	// at all (db.PlayObject, §20 erratum 201).
	key, size, etag, ok := db.PlayObject(c)
	if !ok {
		return skip("filmstrip", "clip has no playable file to sample")
	}
	f.clip = c
	f.renditionKey, f.renditionSize, f.renditionETag = key, size, etag
	if c.Width != nil {
		f.width = *c.Width
	}
	if c.DurationMS != nil {
		f.durationMS = *c.DurationMS
	}
	f.log.Info("filmstrip source", "duration_ms", f.durationMS, "width", f.width,
		"key", key, "bytes", size)
	return nil
}

// acquire downloads the playable file to scratch under a per-attempt name. It is
// never cached between attempts, for the gif job's reason: a stale copy of the
// WRONG file is exactly the failure this feature must not have.
func (f *filmstripRun) acquire() error {
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		return fmt.Errorf("scratch dir: %w", err)
	}
	size := f.renditionSize
	if err := requireScratch(f.w, f.dir, satAdd(satAdd(size, transcode.FilmstripMaxBytes), f.w.deps.Cfg.ScratchMinFree)); err != nil {
		return err
	}
	ctx, cancel := f.transferCtx()
	err := f.w.deps.Store.Download(ctx, f.renditionKey, f.srcPath, nil)
	cancel()
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// A ready row pointing at an object the bucket does not hold is the
			// drift §12 says cannot happen (ERROR); the attempt is still
			// transient, because a lagging bucket looks the same.
			f.log.Error("playable file missing from the bucket (row/object drift)", "key", f.renditionKey)
		}
		return fmt.Errorf("download the playable file: %w", err)
	}
	st, err := os.Stat(f.srcPath)
	if err != nil {
		return fmt.Errorf("downloaded playable file: %w", err)
	}
	if st.Size() != size {
		return fmt.Errorf("downloaded playable file is %d bytes, row says %d", st.Size(), size)
	}
	return nil
}

// renderVerify plans the sheet and takes it holding ONE of the
// TRANSCODE_CONCURRENCY ffmpeg slots (§20 erratum 79). The pass decodes the
// rendition, so it is precisely the sort of work that limit is about; and the
// measurement is inside the same call, because an unmeasured sheet is of no use
// to anybody (the client positions tiles from the numbers).
func (f *filmstripRun) renderVerify() error {
	release, err := f.w.acquireFFmpeg(f.ctx, f.log)
	if err != nil {
		return err
	}
	defer release()
	f.setStage(stageFilmstrip)
	f.plan = transcode.PlanFilmstrip(f.durationMS, f.width)
	f.log.Info("filmstrip planned", "tiles", f.plan.Tiles, "tile_width", f.plan.TileWidth, "interval_ms", f.plan.IntervalMS)
	res, err := f.w.runner.Filmstrip(f.ctx, f.plan, f.srcPath, f.outPath)
	if err != nil {
		return err
	}
	f.fields = db.FilmstripFields{Key: f.key, Size: res.Bytes, Tiles: res.Tiles,
		TileW: res.TileW, TileH: res.TileH, IntervalMS: res.IntervalMS}
	f.log.Info("filmstrip rendered", "bytes", res.Bytes, "tiles", res.Tiles,
		"tile", fmt.Sprintf("%dx%d", res.TileW, res.TileH), "interval_ms", res.IntervalMS)
	return nil
}

func (f *filmstripRun) store() error {
	f.attempted = append(f.attempted, f.key)
	ctx, cancel := f.transferCtx()
	defer cancel()
	res, err := f.w.deps.Store.PutFile(ctx, f.key, f.outPath, "image/jpeg", nil)
	if err != nil {
		return fmt.Errorf("upload filmstrip: %w", err)
	}
	f.fields.Size, f.fields.ETag = res.Size, res.SHA256
	return nil
}

// commit is the one guarded UPDATE. A swap that does not happen is not a
// failure of anything: the attempt deletes its own object and resolves the job
// as done with the reason, because there is nothing to retry — the rendition
// this sheet described is not the one that exists now, and the route queues a
// fresh job the next time somebody opens the control.
func (f *filmstripRun) commit() error {
	f.setStage(stageFinalize)
	swap, err := f.w.deps.DB.SetFilmstrip(f.ctx, f.job.ID, f.clip.ID, f.fields, f.renditionETag, f.w.now().Unix())
	if err != nil {
		if f.ctx.Err() == nil {
			f.log.Error("set filmstrip failed", "err", err)
		}
		return fmt.Errorf("set filmstrip: %w", err)
	}
	if !swap.OK {
		f.skipped(swap.Reason)
		return errLostRace
	}
	ctx, cancel := f.exitCtx()
	deleteKeys(ctx, f.w.deps.Store, f.log, swap.Superseded)
	cancel()
	f.removeScratch()
	f.log.Info("filmstrip applied", "key", f.fields.Key, "bytes", f.fields.Size,
		"tiles", f.fields.Tiles, "superseded", len(swap.Superseded))
	return nil
}

// skipped resolves the job as done with the reason and removes this attempt's
// object — the §11 lost-race shape, at INFO because it is an expected outcome
// of a concurrent trim.
func (f *filmstripRun) skipped(reason string) {
	ctx, cancel := f.exitCtx()
	defer cancel()
	f.complete(db.JobStageSkipped, "skipped: "+reason)
	deleteKeys(ctx, f.w.deps.Store, f.log, f.attempted)
	f.removeScratch()
	f.log.Info("filmstrip skipped; the sheet was not published", "reason", reason)
}

// finish settles the attempt (§11 classification) with the media rule: whatever
// happened, the CLIP is untouched — db.FailAttempt moves a clip only for
// `transcode`, and a filmstrip job's crash cap is db.FilmstripMaxAttempts (its
// environment cap is the instance's, §20 erratum 89).
func (f *filmstripRun) finish(err error) {
	var pe *panicError
	switch {
	case err == nil, errors.Is(err, errLostRace):
		return
	case errors.Is(err, errSkipped):
		f.complete(db.JobStageSkipped, err.Error())
		f.removeScratch()
		f.log.Info("filmstrip skipped", "reason", err.Error())
	case errors.As(err, &pe):
		f.log.Error("filmstrip job panicked", "stage", f.stage, "err", pe.Error(), "cancelled", f.ctx.Err() != nil, "stack", string(pe.stack))
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

// noRoom is the §20 erratum 90 ending: requeued with a backoff, both counters
// untouched, this attempt's object removed. A sheet that cannot find disk space
// is not a sheet that will never exist.
func (f *filmstripRun) noRoom(err error) {
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
// the current owner's counter) and only what this attempt made is discarded. A
// filmstrip keeps no checkpoint, so nothing references it.
func (f *filmstripRun) lostAttempt(err error, detail string) {
	ctx, cancel := f.exitCtx()
	defer cancel()
	f.log.Info("filmstrip job row is no longer this attempt's; outputs discarded, row untouched",
		"stage", f.stage, "err", err, "detail", detail)
	deleteKeys(ctx, f.w.deps.Store, f.log, f.attempted)
	f.removeScratch()
}

// complete resolves a job that must not run as `done` with the reason in
// last_error (CompleteJob refuses a transcode job, which is the point).
func (f *filmstripRun) complete(stage, note string) {
	ctx, cancel := f.exitCtx()
	defer cancel()
	err := f.w.deps.DB.CompleteJob(ctx, f.job.ID, db.ClaimStamp(f.job), stage, note, f.w.now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		f.lostAttempt(err, note)
	default:
		f.log.Error("complete filmstrip job", "note", note, "err", err)
	}
}

// release is the Cancel/Stop path: the job returns to queued with both counters
// untouched and this attempt's object is removed — a filmstrip keeps no
// checkpoint, so a retry simply redoes the work under a fresh {r8}.
func (f *filmstripRun) release() {
	why := f.run.reason()
	if why == reasonNone {
		why = reasonShutdown
	}
	ctx, cancel := f.exitCtx()
	err := f.w.deps.DB.ReleaseJob(ctx, f.job.ID, db.ClaimStamp(f.job), f.clip.ID, f.w.now().Unix())
	switch {
	case err == nil:
		f.log.Info("filmstrip job released", "reason", why.String(), "stage", f.stage)
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		cancel()
		f.lostAttempt(err, why.String())
		return
	default:
		f.log.Error("release filmstrip job", "reason", why.String(), "err", err)
	}
	deleteKeys(ctx, f.w.deps.Store, f.log, f.attempted)
	cancel()
	f.removeScratch()
}

// fail records one bad ending. The clip keeps its rendition, its card image and
// its readiness; only the scrubber's sheet is missing, and the route says so.
func (f *filmstripRun) fail(kind db.FailKind, cause error) {
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
		f.log.Error("fail filmstrip attempt", "kind", kindName(kind), "reason", reason, "detail", detail, "err", err)
	case outcome == db.OutcomeFailed && kind == db.FailPermanent && strings.Contains(reason, "bad_output"):
		// A JPEG we wrote and cannot measure is ours, not the request's (§12).
		f.log.Error("filmstrip failed: bad_output from our own sheet", "reason", reason, "detail", detail)
	case outcome == db.OutcomeFailed:
		f.log.Info("filmstrip failed; the clip is unaffected", "kind", kindName(kind), "reason", reason, "detail", detail,
			"cap", db.Cap(db.KindFilmstrip, kind, f.w.lim))
	default:
		f.log.Warn("filmstrip attempt failed; requeued", "kind", kindName(kind), "reason", reason, "detail", detail)
	}
	deleteKeys(ctx, f.w.deps.Store, f.log, f.attempted)
	f.removeScratch()
}

// reason renders the stored failure text. It reaches the clip's OWNER through
// the route's 503 (§20 erratum 227 narrowed that from any caller to them), so
// publicReason's storage class and its redaction are load-bearing here rather
// than merely tidy — the gif's rule exactly, and it is not relaxed for the
// smaller audience: a path or a bucket name is nobody's business.
func (f *filmstripRun) reason(err error) string {
	return publicReason(err, f.stage, f.dir, f.w.deps.Cfg)
}

func (f *filmstripRun) transferCtx() (context.Context, context.CancelFunc) {
	if m := f.w.deps.Cfg.S3.TransferMax; m > 0 {
		return context.WithTimeout(f.ctx, m)
	}
	return context.WithCancel(f.ctx)
}

func (f *filmstripRun) exitCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(f.ctx), exitBudget)
}

// removeScratch removes only THIS attempt's two files. Like a gif or a poster
// run, a filmstrip run never owns the clip's scratch dir: it did not put the
// source there and a trim may be about to reuse it.
func (f *filmstripRun) removeScratch() {
	for _, p := range []string{f.srcPath, f.outPath} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			f.log.Warn("remove scratch output", "path", p, "err", err)
		}
	}
	if err := os.Remove(f.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		f.log.Debug("scratch dir kept", "dir", f.dir, "err", err)
	}
}

// setStage persists the stage. A filmstrip job has NO progress registry, for
// the gif job's reason: its only public state is queued/running/done/failed on
// the route, and a percentage nobody can ask for is a registry nobody can
// evict.
func (f *filmstripRun) setStage(stage string) {
	f.stage = stage
	err := f.w.deps.DB.SetJobStage(f.ctx, f.job.ID, db.ClaimStamp(f.job), stage, f.w.now().Unix())
	switch {
	case err == nil, f.ctx.Err() != nil:
	case errors.Is(err, db.ErrNotFound):
		f.log.Info("filmstrip job row gone while setting stage", "stage", stage)
	case errors.Is(err, db.ErrStateMismatch):
		// Repaired or cancelled under another hand (§20 erratum 291): the stage is
		// abandoned, the run goes on, and the lost row ends it at the next write
		// that matters — never as a failure of the row's new owner.
		f.log.Info("filmstrip job row is no longer this attempt's; stage not recorded", "stage", stage)
	default:
		f.log.Error("set filmstrip job stage", "stage", stage, "err", err)
	}
}
