package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/store"
	"clippet/internal/transcode"
)

// The owner's chosen card image (ROADMAP #5, §20 errata 127-130).
//
// A poster job is the smallest run in the system that still touches ffmpeg:
// one seek, one frame, one small object. It is deliberately built out of the
// gif job's parts, because the two have the same shape and the same three
// rules:
//
//  1. It reads the RENDITION, never the original. The card image describes
//     the clip people are watching, and the rendition is a file we produced
//     — normalised, bounded and already on the box's terms.
//  2. It writes the poster columns and nothing else. The clip is `ready`
//     before, during and after; db.SetPoster enforces that on its side.
//  3. Its commit is guarded by the rendition's ETag. A trim that lands while
//     this job runs replaces the very frames the picture was taken from, so
//     the output is thrown away rather than published.
//
// It is REGISTERED in the run registry (erratum 71's rule): it runs ffmpeg,
// writes scratch and uploads an object, which is precisely the list §10.1's
// Cancel promises to have finished when it returns.
//
// It also holds one of the TRANSCODE_CONCURRENCY ffmpeg slots around its one
// pass (erratum 79). A single-frame extraction is cheap, but "cheap" is not
// the promise DEPLOY §7 makes: the promise is that a default install runs
// ONE ffmpeg, and a third media kind that ignored the semaphore would make
// that false the same way a trim once did.

// The owner-facing failure vocabulary of a card image: short, plain, fixed
// and complete, exactly as §20 erratum 80 requires of the trim's. It is a
// vocabulary of its OWN rather than the trim's reused, because every one of
// the trim's seven sentences says the word "trim" — telling an owner whose
// chosen frame could not be taken that "the trim failed" would send them
// after the wrong thing, and the whole point of a fixed vocabulary is that
// the sentence is true.
const (
	msgNoFrame       = "there is no picture at that moment in the clip"
	msgFrameMedia    = "that frame could not be taken from this clip"
	msgFrameDisk     = "the server had no room to take the frame; try again later"
	msgFrameStorage  = "storage was unavailable while the frame was taken"
	msgFrameInternal = "the card image could not be changed"
)

// posterRun is the state of one poster attempt.
type posterRun struct {
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

	// renditionKey/renditionSize/renditionETag are the PLAYABLE FILE this
	// frame is taken from (db.PlayObject), read at the
	// start and handed to SetPoster as the commit's predicate.
	renditionKey  string
	renditionSize int64
	renditionETag string
	// ranged records that the file under srcPath is a PREFIX of the rendition
	// and not the whole of it (N051), so the log line the operator reads says
	// which of the two the frame came from.
	ranged bool
	// params.Seconds is on the ORIGINAL's timeline; seek is the same moment
	// on the rendition's, which is the file ffmpeg is pointed at.
	params db.PosterParams
	seek   float64
	fields db.PosterFields
}

func newPosterRun(w *worker, ctx context.Context, r *run, job db.Job, clip db.Clip) *posterRun {
	p := &posterRun{w: w, ctx: ctx, run: r, job: job, clip: clip}
	p.log = w.log.With("clip", clip.ID, "job", job.ID)
	p.r8 = ids.Rand(8)
	p.dir = filepath.Join(w.deps.Cfg.ScratchDir, clip.ID)
	p.srcPath = filepath.Join(p.dir, "postersrc-"+p.r8+".mp4")
	p.outPath = filepath.Join(p.dir, "cardimage-"+p.r8+".jpg")
	p.key = store.PosterKey(w.deps.Cfg.S3.KeyPrefix, clip.ID, p.r8)
	return p
}

// execute runs one attempt under recover() and settles its outcome.
func (p *posterRun) execute() {
	p.log.Info("poster started", "attempt", p.job.Attempts+1, "transient_failures", p.job.TransientFailures,
		"params", p.job.ParamsJSON)
	err := p.guarded()
	defer func() {
		if r := recover(); r != nil {
			p.log.Error("poster exit path panicked", "stage", p.stage, "err", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
	}()
	p.finish(err)
}

func (p *posterRun) guarded() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &panicError{value: r, stack: debug.Stack()}
		}
	}()
	return p.pipeline()
}

func (p *posterRun) pipeline() error {
	params, err := db.ParsePosterParams(p.job.ParamsJSON)
	if err != nil {
		// The row cannot describe a frame at all: no retry can fix it.
		return &transcode.ErrPermanent{Err: err}
	}
	p.params = params
	if err := p.recheckClip(); err != nil {
		return err
	}
	p.setStage(stageAcquire)
	if err := p.acquireAndRender(); err != nil {
		return err
	}
	p.setStage(stageStore)
	if err := p.store(); err != nil {
		return err
	}
	return p.commit()
}

// recheckClip closes the claim→registration window, reads the rendition this
// picture will describe, and turns the chosen moment into a seek on that
// rendition's own timeline. A clip that is no longer ready — or that has
// somehow no rendition — is skipped, not failed: nothing is wrong with it.
func (p *posterRun) recheckClip() error {
	c, err := p.w.deps.DB.GetClip(p.ctx, p.clip.ID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return skip("poster", "clip deleted")
	case err != nil:
		return fmt.Errorf("re-read clip: %w", err)
	case c.State == db.ClipDeleting:
		return skip("poster", "clip is being deleted")
	case c.State != db.ClipReady:
		return skip("poster", "clip is "+c.State)
	}
	// The PLAYABLE FILE this clip is, resolved from the row: the rendition when
	// there is one, and the UPLOAD itself on the remux fast path, where a
	// playable original is served as it stands and there is no rendition object
	// at all (db.PlayObject, §20 erratum 201).
	key, size, etag, ok := db.PlayObject(c)
	if !ok {
		return skip("poster", "clip has no playable file to take a frame from")
	}
	p.clip = c
	p.renditionKey, p.renditionSize, p.renditionETag = key, size, etag
	p.seek = p.params.Seconds - trimStartSeconds(c)
	if p.seek < 0 {
		// The clip was trimmed between the request and this run, and the
		// chosen moment is now before the cut begins. Nothing to publish and
		// nothing to retry — the frame is gone.
		return &transcode.ErrPermanent{Err: fmt.Errorf("%w: %.3fs is before this clip's cut", transcode.ErrNoFrameAt, p.params.Seconds)}
	}
	p.log.Info("poster source", "seconds", p.params.Seconds, "rendition_seconds", p.seek,
		"key", key, "bytes", size)
	return nil
}

// acquireAndRender puts enough of the rendition on scratch and takes the frame
// out of it — the two stages are one step because of what makes the download
// cheap (N051, §20 erratum 233).
//
// A card image is ONE frame near a known moment, and a faststart MP4 puts its
// `moov` — the index of every frame in the file — before the media data (§10.3
// step 5 asserts it). So a PREFIX of the rendition, big enough for the index
// plus the media up to the chosen moment, is enough to take the frame from: a
// 200 MiB rendition is a 17 MiB download to photograph its fifth second.
//
// Nothing about ffmpeg's inputs changes, which is the owner's constraint on this
// (erratum 91): the prefix is a FILE on scratch, written by the same store the
// whole-file download uses, and the protocol whitelist is untouched — no
// `http` input, no seekable HTTP reader, nothing new that ffmpeg may open.
//
// The fallback is the whole design's safety: a prefix that does not decode —
// an index bigger than the allowance, a non-faststart file some earlier version
// wrote, a fragmented MP4, a duration the row is wrong about — costs one
// wasted range request and is then retried against the complete file, ONCE,
// inside the same attempt. Three conditions skip the retry, all because
// repeating the work cannot help: a context that is done (Cancel or Stop), a
// scratch refusal (there is less room for the whole file than for the prefix,
// by definition), and a transfer that ran out of STORAGE_TRANSFER_MAX — that
// budget is per transfer (transferCtx derives a fresh deadline for each one),
// so the whole file, which is strictly bigger than the prefix that just missed
// it, would be given the same clock for more bytes. The attempt fails instead,
// and the job's own retry is what tries again.
func (p *posterRun) acquireAndRender() error {
	if err := os.MkdirAll(p.dir, 0o755); err != nil {
		return fmt.Errorf("scratch dir: %w", err)
	}
	size := p.renditionSize
	if prefix := posterPrefixBytes(size, p.clip.DurationMS, p.seek); prefix > 0 {
		err := p.acquirePrefix(prefix, size)
		if err == nil {
			p.ranged = true
			err = p.renderVerify()
			if err == nil {
				return nil
			}
		}
		p.ranged = false
		switch {
		case p.ctx.Err() != nil, errors.Is(err, errStopping), errors.Is(err, errNoScratchRoom),
			errors.Is(err, context.DeadlineExceeded):
			return err
		}
		// INFO, not WARN: for a file this heuristic cannot serve, the whole
		// file is the ANSWER and not a degradation. The detail is redacted the
		// same way a failure reason is — it can quote a scratch path.
		p.log.Info("the ranged prefix of the rendition could not produce the frame; taking it from the whole file",
			"prefix_bytes", prefix, "rendition_bytes", size, "err", p.reason(err))
		if err := os.Remove(p.srcPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove the ranged prefix: %w", err)
		}
		// Back to the DOWNLOAD stage for the retry. renderVerify left the row
		// reading `poster`, so without this the whole-file download runs one
		// stage ahead of itself and ownerReason would call a storage failure
		// there a media one.
		p.setStage(stageAcquire)
	}
	if err := p.acquireWhole(size); err != nil {
		return err
	}
	return p.renderVerify()
}

// acquirePrefix writes the first `prefix` bytes of the rendition to scratch.
func (p *posterRun) acquirePrefix(prefix, size int64) error {
	if err := requireScratch(p.w, p.dir, satAdd(satAdd(prefix, posterScratchBytes), p.w.deps.Cfg.ScratchMinFree)); err != nil {
		return err
	}
	ctx, cancel := p.transferCtx()
	defer cancel()
	body, err := p.w.deps.Store.GetRange(ctx, p.renditionKey, 0, prefix-1)
	if err != nil {
		return p.acquireErr("range the playable file", err)
	}
	defer body.Close()
	f, err := os.Create(p.srcPath)
	if err != nil {
		return fmt.Errorf("scratch prefix: %w", err)
	}
	n, err := io.Copy(f, body)
	cerr := f.Close()
	switch {
	case err != nil:
		return fmt.Errorf("range the rendition: %w", err)
	case cerr != nil:
		return fmt.Errorf("scratch prefix: %w", cerr)
	case n != prefix:
		// A short body is not this feature's business to interpret: the frame
		// either comes out of what arrived or the whole file is fetched.
		return fmt.Errorf("ranged rendition prefix is %d bytes, asked for %d", n, prefix)
	}
	p.log.Info("poster source: a ranged prefix of the rendition", "prefix_bytes", n, "rendition_bytes", size,
		"rendition_seconds", p.seek)
	return nil
}

// acquireWhole downloads the entire rendition to scratch under this attempt's
// name. It is never cached between attempts, for the gif job's reason exactly:
// a stale copy of the WRONG rendition is the one failure this feature must not
// have.
func (p *posterRun) acquireWhole(size int64) error {
	if err := requireScratch(p.w, p.dir, satAdd(satAdd(size, posterScratchBytes), p.w.deps.Cfg.ScratchMinFree)); err != nil {
		return err
	}
	ctx, cancel := p.transferCtx()
	err := p.w.deps.Store.Download(ctx, p.renditionKey, p.srcPath, nil)
	cancel()
	if err != nil {
		return p.acquireErr("download the playable file", err)
	}
	st, err := os.Stat(p.srcPath)
	if err != nil {
		return fmt.Errorf("downloaded rendition: %w", err)
	}
	if st.Size() != size {
		return fmt.Errorf("downloaded rendition is %d bytes, row says %d", st.Size(), size)
	}
	p.log.Info("poster source: the whole rendition", "rendition_bytes", size, "rendition_seconds", p.seek)
	return nil
}

// acquireErr wraps a transfer failure, logging the one case §12 calls drift.
func (p *posterRun) acquireErr(what string, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		// A ready row pointing at an object the bucket does not hold is the
		// drift §12 says cannot happen (ERROR); the attempt is still
		// transient, because a lagging bucket looks the same.
		p.log.Error("playable file missing from the bucket (row/object drift)", "key", p.renditionKey)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// The prefix heuristic (N051, §20 erratum 233), in the units the row carries.
const (
	// posterPrefixFloor is the rendition size under which a prefix is not worth
	// a thought: one request for a whole small file is cheaper than the risk of
	// two requests for parts of it.
	posterPrefixFloor = 1 << 20
	// posterPrefixIndex is the allowance for `ftyp` + `moov`, i.e. the sample
	// tables of the WHOLE file, which is what a prefix must contain to be
	// decodable at all. A fixed floor plus a share of the file, because the
	// tables grow with the frame count: 3% is comfortably above the ~1% a
	// 720p h264 rendition measures, and the fallback covers the rest.
	posterPrefixIndex     = 512 << 10
	posterPrefixIndexPart = 32 // size/32 ≈ 3%
	// posterPrefixPad is how many seconds PAST the chosen moment are fetched,
	// so an index that points a little further than the arithmetic predicts
	// (B-frames, a big GOP, a variable bitrate whose local rate is above the
	// average) still lands inside what arrived.
	posterPrefixPad = 3.0
	// posterPrefixMinBody is the floor on the media half: a moment at second 0
	// of a long file still needs some frames.
	posterPrefixMinBody = 256 << 10
	// posterPrefixWorthIt is the share of the file above which a prefix is not
	// worth fetching: saving an eighth does not pay for a possible second
	// request, and asking for 99% of a file is how a heuristic looks silly.
	posterPrefixWorthItNum = 7
	posterPrefixWorthItDen = 8
)

// posterPrefixBytes returns how many LEADING bytes of the rendition are worth
// fetching to take the frame at `seek` seconds of it, or 0 for "fetch the whole
// file".
//
// Zero is returned whenever the arithmetic cannot be trusted or cannot pay:
// a small file, a row with no duration (nothing places the seek), a seek at or
// past the end (the frame is not in the file at all — PosterAt's own refusal,
// which must be reached with the WHOLE file so the answer is about the file and
// not about a prefix), and any answer that is not meaningfully smaller than the
// file itself.
//
// durationMS is read as the CURRENT play object's own length, which is what
// every publisher writes with the key it publishes. A tier that ever stored a
// duration belonging to a different file would mis-place the seek here; the
// whole-file fallback still produces the right frame, so the symptom would be a
// silent loss of the saving, never a wrong picture.
func posterPrefixBytes(size int64, durationMS *int64, seek float64) int64 {
	if size <= posterPrefixFloor || durationMS == nil || *durationMS <= 0 {
		return 0
	}
	durSec := float64(*durationMS) / 1000
	if math.IsNaN(seek) || math.IsInf(seek, 0) || seek < 0 || seek >= durSec {
		return 0
	}
	body := int64(float64(size) * (seek + posterPrefixPad) / durSec)
	if body < posterPrefixMinBody {
		body = posterPrefixMinBody
	}
	index := int64(posterPrefixIndex)
	if part := size / posterPrefixIndexPart; part > index {
		index = part
	}
	want := satAdd(index, body)
	if want >= size/posterPrefixWorthItDen*posterPrefixWorthItNum {
		return 0
	}
	return want
}

// posterMaxBytes is what ONE card image may occupy on scratch. A `-q:v 3`
// JPEG of a 720p frame is a couple of hundred kilobytes; four megabytes is
// the "this is not a poster" line, and it exists only so the scratch
// reservation has a number rather than a guess.
const posterMaxBytes = 4 << 20

// posterScratchBytes is what the STAGE holds, which is two of them (a nit
// from lane card). `drawPosterLabel` burns the duration into the chosen frame by
// writing a second full-size JPEG beside it (`poster.jpg.lbl`) and renaming it
// over the first, so for the length of that pass both files exist. Eight
// megabytes against two pictures of a few hundred KB each is not a real
// squeeze, and the label pass fails soft — but a reservation that does not
// state what the stage actually holds is a number that has stopped meaning
// anything, and the next thing added beside the poster would inherit the same
// silence. The `.dur` text file is a handful of bytes and is not counted.
const posterScratchBytes = 2 * posterMaxBytes

// ptrMS reads a nullable duration column as the label's milliseconds: nil or a
// negative value is "not measured", which draws nothing (§20 erratum 287).
func ptrMS(ms *int64) int64 {
	if ms == nil || *ms < 0 {
		return 0
	}
	return *ms
}

// renderVerify takes the frame holding ONE of the TRANSCODE_CONCURRENCY
// ffmpeg slots (§20 erratum 79). It is one seek and one frame, but it is
// still ffmpeg on the box an operator sized that limit for, and it runs on
// the media loop beside trims and previews.
func (p *posterRun) renderVerify() error {
	release, err := p.w.acquireFFmpeg(p.ctx, p.log)
	if err != nil {
		return err
	}
	defer release()
	p.setStage(stagePoster)
	// The label is the CLIP's own duration (§20 erratum 287), read off the row
	// this job is for -- not the moment the owner picked, and not the length of
	// the ranged prefix srcPath may be. A clip with no measured duration (which
	// a `ready` row does not have) draws no label.
	res, err := p.w.runner.PosterAt(p.ctx, p.srcPath, p.outPath, p.seek, ptrMS(p.clip.DurationMS))
	if err != nil {
		return err
	}
	st, err := os.Stat(p.outPath)
	if err != nil {
		return &transcode.ErrPermanent{Err: fmt.Errorf("bad_output: the card image is unreadable: %w", err)}
	}
	if res.NearBlack {
		// Not a refusal: the owner was looking at this frame when they chose
		// it. The line is here so an operator asked "why is the card black"
		// can answer it from the log.
		p.log.Info("the chosen card image is near-black; it is published anyway because it was chosen", "seconds", p.params.Seconds)
	}
	p.fields = db.PosterFields{Key: p.key, Size: st.Size(), Width: res.Width, Height: res.Height, Seconds: p.params.Seconds}
	p.log.Info("poster taken", "bytes", st.Size(), "dims", fmt.Sprintf("%dx%d", res.Width, res.Height),
		"seconds", p.params.Seconds, "near_black", res.NearBlack, "ranged_source", p.ranged)
	return nil
}

func (p *posterRun) store() error {
	p.attempted = append(p.attempted, p.key)
	ctx, cancel := p.transferCtx()
	defer cancel()
	res, err := p.w.deps.Store.PutFile(ctx, p.key, p.outPath, "image/jpeg", nil)
	if err != nil {
		return fmt.Errorf("upload poster: %w", err)
	}
	p.fields.Size, p.fields.ETag = res.Size, res.SHA256
	return nil
}

// commit is the one guarded UPDATE. A swap that does not happen is not a
// failure of anything: the attempt deletes its own object and resolves the
// job as done with the reason, because there is nothing to retry — the clip
// this frame came from is not the clip that exists now, and the owner can
// choose again on the clip that does.
func (p *posterRun) commit() error {
	p.setStage(stageFinalize)
	swap, err := p.w.deps.DB.SetPoster(p.ctx, p.job.ID, p.clip.ID, p.fields, p.renditionETag, p.w.now().Unix())
	if err != nil {
		if p.ctx.Err() == nil {
			p.log.Error("set poster failed", "err", err)
		}
		return fmt.Errorf("set poster: %w", err)
	}
	if !swap.OK {
		p.skipped(swap.Reason)
		return errLostRace
	}
	ctx, cancel := p.exitCtx()
	deleteKeys(ctx, p.w.deps.Store, p.log, swap.Superseded)
	cancel()
	p.removeScratch()
	p.log.Info("poster applied", "key", p.fields.Key, "bytes", p.fields.Size, "seconds", p.fields.Seconds,
		"dims", fmt.Sprintf("%dx%d", p.fields.Width, p.fields.Height), "superseded", len(swap.Superseded))
	return nil
}

// skipped resolves the job as done with the reason and removes this
// attempt's object — the §11 lost-race shape, at INFO because it is an
// expected outcome of a concurrent trim.
func (p *posterRun) skipped(reason string) {
	ctx, cancel := p.exitCtx()
	defer cancel()
	p.complete(db.JobStageSkipped, "skipped: "+reason)
	deleteKeys(ctx, p.w.deps.Store, p.log, p.attempted)
	p.removeScratch()
	p.log.Info("poster skipped; the card image was not replaced", "reason", reason)
}

// finish settles the attempt (§11 classification) with the media rule: the
// CLIP is untouched whatever happened — db.FailAttempt moves a clip only for
// `transcode`, and a poster job's crash cap is db.PosterMaxAttempts (its
// environment cap is the instance's, §20 erratum 89).
func (p *posterRun) finish(err error) {
	var pe *panicError
	switch {
	case err == nil, errors.Is(err, errLostRace):
		return
	case errors.Is(err, errSkipped):
		p.complete(db.JobStageSkipped, err.Error())
		p.removeScratch()
		p.log.Info("poster skipped", "reason", err.Error())
	case errors.As(err, &pe):
		p.log.Error("poster job panicked", "stage", p.stage, "err", pe.Error(), "cancelled", p.ctx.Err() != nil, "stack", string(pe.stack))
		if p.ctx.Err() != nil {
			p.release()
			return
		}
		p.fail(db.FailCrash, err)
	case p.ctx.Err() != nil, errors.Is(err, errStopping):
		p.release()
	case errors.Is(err, errNoScratchRoom):
		p.noRoom(err)
	case transcode.IsPermanent(err):
		p.fail(db.FailPermanent, err)
	default:
		p.fail(db.FailTransient, err)
	}
}

// noRoom is the §20 erratum 90 ending: requeued with a backoff, both
// counters untouched, this attempt's object removed.
func (p *posterRun) noRoom(err error) {
	ctx, cancel := p.exitCtx()
	defer cancel()
	if lost := requeueNoCount(ctx, p.w, p.log, p.job, p.clip.ID, p.stage, msgFrameDisk, p.reason(err)); lost != nil {
		p.lostAttempt(lost, p.reason(err))
		return
	}
	deleteKeys(ctx, p.w.deps.Store, p.log, p.attempted)
	p.removeScratch()
}

// lostAttempt is the §20 erratum 293 ending, this kind's copy of the one
// erratum 291 gave the transcode: the row stopped being this attempt's — a
// repair took it and a NEWER attempt may hold it, or MarkDeleting cancelled it
// with the clip — so the row is NOT touched (an attempt recorded there spends
// the current owner's counter, and the owner would be told about a failure
// that is not this clip's) and only what this attempt made is discarded. The
// clip keeps the card image it had, which is what it does on every other
// ending of this kind too.
func (p *posterRun) lostAttempt(err error, detail string) {
	ctx, cancel := p.exitCtx()
	defer cancel()
	p.log.Info("poster job row is no longer this attempt's; outputs discarded, row untouched",
		"stage", p.stage, "err", err, "detail", detail)
	deleteKeys(ctx, p.w.deps.Store, p.log, p.attempted)
	p.removeScratch()
}

// complete resolves a job that must not run as `done` with the reason in
// last_error (CompleteJob refuses a transcode job, which is the point).
func (p *posterRun) complete(stage, note string) {
	ctx, cancel := p.exitCtx()
	defer cancel()
	err := p.w.deps.DB.CompleteJob(ctx, p.job.ID, db.ClaimStamp(p.job), stage, note, p.w.now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		p.lostAttempt(err, note)
	default:
		p.log.Error("complete poster job", "note", note, "err", err)
	}
}

// release is the Cancel/Stop path: the job returns to queued with both
// counters untouched and this attempt's object is removed — a poster keeps
// no checkpoint, so a retry simply redoes the work under a fresh {r8}.
func (p *posterRun) release() {
	why := p.run.reason()
	if why == reasonNone {
		why = reasonShutdown
	}
	ctx, cancel := p.exitCtx()
	err := p.w.deps.DB.ReleaseJob(ctx, p.job.ID, db.ClaimStamp(p.job), p.clip.ID, p.w.now().Unix())
	switch {
	case err == nil:
		p.log.Info("poster job released", "reason", why.String(), "stage", p.stage)
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		cancel()
		p.lostAttempt(err, why.String())
		return
	default:
		p.log.Error("release poster job", "reason", why.String(), "err", err)
	}
	deleteKeys(ctx, p.w.deps.Store, p.log, p.attempted)
	cancel()
	p.removeScratch()
}

// fail records one bad ending. The clip keeps its rendition, its old card
// image and its readiness; only the owner's new choice did not land, and the
// route says why in the vocabulary of §20 erratum 80.
func (p *posterRun) fail(kind db.FailKind, cause error) {
	detail, owner := p.reason(cause), p.ownerReason(cause)
	ctx, cancel := p.exitCtx()
	defer cancel()
	outcome, err := p.w.deps.DB.FailAttempt(ctx, p.job.ID, db.ClaimStamp(p.job), p.clip.ID, owner, kind, p.w.lim, p.w.now().Unix())
	switch {
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		// Not this attempt's row to fail (§20 erratum 293).
		p.lostAttempt(err, detail)
		return
	case err != nil:
		p.log.Error("fail poster attempt", "kind", kindName(kind), "reason", owner, "detail", detail, "err", err)
	case outcome == db.OutcomeFailed && kind == db.FailPermanent && strings.Contains(detail, "bad_output"):
		// A JPEG we wrote and cannot stat is ours, not the request's (§12).
		p.log.Error("poster failed: bad_output from our own poster", "reason", owner, "detail", detail)
	case outcome == db.OutcomeFailed:
		// Everything a REQUEST or a FILE is responsible for — a moment with
		// no picture in it above all — is INFO: nothing is broken, and the
		// clip still shows the card image it had.
		p.log.Info("poster failed; the clip keeps the card image it had", "kind", kindName(kind),
			"reason", owner, "detail", detail, "cap", db.Cap(db.KindPoster, kind, p.w.lim))
	default:
		p.log.Warn("poster attempt failed; requeued", "kind", kindName(kind), "reason", owner, "detail", detail)
	}
	deleteKeys(ctx, p.w.deps.Store, p.log, p.attempted)
	p.removeScratch()
}

// reason renders the LOGGED failure detail, paths and bucket coordinates
// redacted. It is not what the owner sees — ownerReason is.
func (p *posterRun) reason(err error) string {
	msg := err.Error()
	var pe *transcode.ErrPermanent
	if errors.As(err, &pe) {
		msg = pe.Err.Error()
	}
	msg = redactPaths(msg, p.dir, p.w.deps.Cfg)
	if p.stage != "" {
		msg = p.stage + ": " + msg
	}
	if len(msg) > maxReasonLen {
		msg = msg[:maxReasonLen]
	}
	return msg
}

// ownerReason maps one failure onto exactly one of those sentences (§20
// erratum 80's rule, this kind's vocabulary). The owner reads it on the share
// page, so nothing an ffmpeg build happens to print on stderr belongs in it,
// and the STAGE is the fallback, so an error nobody anticipated still comes
// out as a sentence rather than as whatever it happened to say.
//
// `bad_output` is a JPEG we wrote and then could not read, which is ours and
// not the request's: the owner gets the generic sentence and the operator
// gets an ERROR line with the detail (§12).
func (p *posterRun) ownerReason(err error) string {
	var pe *panicError
	switch {
	case err == nil:
		return msgFrameInternal
	case errors.Is(err, transcode.ErrNoFrameAt):
		return msgNoFrame
	case errors.As(err, &pe):
		return msgFrameInternal
	}
	detail := err.Error()
	switch {
	case strings.Contains(detail, "insufficient scratch"):
		return msgFrameDisk
	case strings.Contains(detail, "bad_output"):
		return msgFrameInternal
	}
	switch p.stage {
	case stageAcquire, stageStore:
		return msgFrameStorage
	case stagePoster:
		return msgFrameMedia
	}
	return msgFrameInternal
}

func (p *posterRun) transferCtx() (context.Context, context.CancelFunc) {
	if m := p.w.deps.Cfg.S3.TransferMax; m > 0 {
		return context.WithTimeout(p.ctx, m)
	}
	return context.WithCancel(p.ctx)
}

func (p *posterRun) exitCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(p.ctx), exitBudget)
}

// removeScratch removes only THIS attempt's two files. Like a gif run and
// unlike a transcode or a trim, a poster run never owns the clip's scratch
// dir: it did not put the source there and a trim may be about to reuse it.
func (p *posterRun) removeScratch() {
	for _, path := range []string{p.srcPath, p.outPath} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			p.log.Warn("remove scratch output", "path", path, "err", err)
		}
	}
	if err := os.Remove(p.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		p.log.Debug("scratch dir kept", "dir", p.dir, "err", err)
	}
}

// setStage persists the stage. A poster job has NO progress registry, for
// the gif job's reason: its only public state is queued/running/done/failed
// on the route, and one seek has no meaningful percentage anyway.
func (p *posterRun) setStage(stage string) {
	p.stage = stage
	err := p.w.deps.DB.SetJobStage(p.ctx, p.job.ID, db.ClaimStamp(p.job), stage, p.w.now().Unix())
	switch {
	case err == nil, p.ctx.Err() != nil:
	case errors.Is(err, db.ErrNotFound):
		p.log.Info("poster job row gone while setting stage", "stage", stage)
	case errors.Is(err, db.ErrStateMismatch):
		// Repaired or cancelled under another hand (§20 erratum 291): the stage is
		// abandoned, the run goes on, and the lost row ends it at the next write
		// that matters — never as a failure of the row's new owner.
		p.log.Info("poster job row is no longer this attempt's; stage not recorded", "stage", stage)
	default:
		p.log.Error("set poster job stage", "stage", stage, "err", err)
	}
}

// --- the pin, read from a clip row ---

// trimStartSeconds is where the clip's CURRENT rendition begins on the
// original's timeline: the applied cut's start, or zero for a clip that has
// never been trimmed. It is the whole conversion between the two timelines
// this feature speaks (§20 erratum 127).
func trimStartSeconds(c db.Clip) float64 {
	if c.TrimStartMS != nil && *c.TrimStartMS > 0 {
		return float64(*c.TrimStartMS) / 1000
	}
	return 0
}

// renditionPosterSeconds is the owner's chosen frame expressed on the
// CURRENT rendition's timeline, and false when there is no usable choice —
// none was made, or the moment does not lie inside the rendition (a row
// whose cut moved under a choice that a trim should already have cleared).
// The gif job asks so the preview opens on the same frame the card shows
// (§20 erratum 131).
func renditionPosterSeconds(c db.Clip) (float64, bool) {
	if c.PosterSeconds == nil {
		return 0, false
	}
	s := *c.PosterSeconds - trimStartSeconds(c)
	if math.IsNaN(s) || math.IsInf(s, 0) || s < 0 {
		return 0, false
	}
	if c.DurationMS != nil && *c.DurationMS > 0 && s >= float64(*c.DurationMS)/1000 {
		return 0, false
	}
	return s, true
}
