package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/store"
	"clippet/internal/transcode"
)

// Server-side trim (DESIGN.md §17 #5, §20 errata 68+).
//
// A trim is the transcode pipeline pointed at a WINDOW of the retained
// original, with two differences that are the whole design:
//
//  1. The clip is `ready` before, during and after. Nothing here writes
//     clips.state, clips.failure_reason or either failure counter of the
//     clip (§5); the db layer refuses to anyway. A trim that fails leaves a
//     playable clip on its old rendition and a `failed` job row saying why.
//  2. The result is committed by ONE UPDATE (db.SwapRendition) guarded by
//     `state='ready'` AND the job still being `running`. Every output key
//     carries a fresh {r8} (§6), so an attempt that never reaches that
//     UPDATE has published nothing a reader could reach, and the swap is
//     therefore atomic from every viewer's point of view: the old rendition
//     until the commit, the new one after it, never a half of either.
//
// The superseded rendition and poster are deleted AFTER the commit, best
// effort. A failure there is an orphan under a live clip prefix — bounded,
// invisible to readers, removed with the clip — and is logged, never
// retried: retrying a delete is how you delete the wrong thing.

// maxTrimDurationDriftMS is how far the measured output may sit from the
// window that was asked for before it counts as bad_output. It is generous
// on purpose: a stream copy ends on a packet boundary and a re-encode rounds
// to a whole frame, so a legitimate output is off by milliseconds, while the
// failures it exists for — a `-t` that was never applied, a source whose
// declared duration lied, a window the muxer could not honour — are off by
// whole seconds.
//
// It is NOT what guards the copy path's start. `-ss` on a stream copy seeks
// backward to a keyframe and the output timestamps are then shifted to zero,
// so a window that started between keyframes yields a file of exactly the
// right LENGTH beginning up to a whole GOP too early. Nothing downstream can
// see that; only transcode.DecideTrim's refusal to copy such a window can,
// which is why the window it returns is snapped to the keyframe itself.
const maxTrimDurationDriftMS = 1500

// trimRun is the state of one trim attempt.
type trimRun struct {
	w    *worker
	ctx  context.Context
	run  *run
	job  db.Job
	clip db.Clip
	log  *slog.Logger

	params db.TrimParams
	dir    string
	ext    string
	r8     string

	srcPath, outPath, posterPath string
	renditionKey, posterKey      string
	// attempted lists the keys this attempt STARTED writing, recorded
	// before each PutFile so a failed or aborted upload is cleaned up (§11).
	attempted []string

	stage    string
	percent  int
	plan     transcode.Plan
	srcProbe *transcode.ProbeResult
	tp       transcode.TrimPlan
	out      *transcode.OutputInfo
	ready    db.ReadyFields

	// applied is the window this attempt actually cut, on the ORIGINAL's
	// timeline: the request, clamped to what the video stream holds and — on
	// the copy path — snapped back to the keyframe ffmpeg can start at. It is
	// what SwapRendition persists, so the next trim and the share page speak
	// about the same file rather than about what was asked for (erratum 68).
	applied db.TrimParams
	// covered records that the video extent was MEASURED and covers the
	// window. Only then can a later bad_output be this server's fault, which
	// is what decides whether it is logged at ERROR (§12).
	covered bool

	panicOnce sync.Once
}

func newTrimRun(w *worker, ctx context.Context, r *run, job db.Job, clip db.Clip) *trimRun {
	t := &trimRun{w: w, ctx: ctx, run: r, job: job, clip: clip}
	t.log = w.log.With("clip", clip.ID, "job", job.ID)
	t.ext = strings.TrimPrefix(path.Ext(clip.SourceKey), ".")
	if t.ext == "" {
		t.ext = store.DefaultExt
	}
	t.r8 = ids.Rand(8)
	t.dir = filepath.Join(w.deps.Cfg.ScratchDir, clip.ID)
	t.srcPath = filepath.Join(t.dir, "source."+t.ext)
	t.outPath = filepath.Join(t.dir, "trim-"+t.r8+".mp4")
	t.posterPath = filepath.Join(t.dir, "poster-"+t.r8+".jpg")
	p := w.deps.Cfg.S3.KeyPrefix
	t.renditionKey = store.RenditionKey(p, clip.ID, t.r8)
	t.posterKey = store.PosterKey(p, clip.ID, t.r8)
	return t
}

// errSkipped resolves a job that must not run: the clip is gone, being
// deleted, or no longer ready. It is a normal outcome, not a failure, and it
// is shared by every kind that runs against a `ready` clip (trim, gif) —
// they all mean the same thing by it.
var errSkipped = errors.New("job skipped")

type skipError struct{ what, reason string }

func (s *skipError) Error() string { return s.what + " skipped: " + s.reason }
func (s *skipError) Unwrap() error { return errSkipped }

func skip(what, reason string) error { return &skipError{what: what, reason: reason} }

// execute runs one attempt under recover() and settles its outcome.
func (t *trimRun) execute() {
	t.log.Info("trim started", "attempt", t.job.Attempts+1, "transient_failures", t.job.TransientFailures,
		"params", t.job.ParamsJSON, "source_size", t.clip.SourceSize)
	err := t.guarded()
	defer func() {
		if p := recover(); p != nil {
			t.log.Error("trim exit path panicked", "stage", t.stage, "err", fmt.Sprint(p), "stack", string(debug.Stack()))
		}
	}()
	t.finish(err)
}

func (t *trimRun) guarded() (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = &panicError{value: p, stack: debug.Stack()}
		}
	}()
	return t.pipeline()
}

func (t *trimRun) pipeline() error {
	params, err := db.ParseTrimParams(t.job.ParamsJSON)
	if err != nil {
		// The row cannot describe a trim at all: no retry can fix it.
		return &transcode.ErrPermanent{Err: err}
	}
	t.params = params
	if err := t.recheckClip(); err != nil {
		return err
	}
	if err := t.step(stageAcquire, t.acquire); err != nil {
		return err
	}
	if err := t.step(stageProbe, t.probeSource); err != nil {
		return err
	}
	if err := t.cutVerifyPoster(); err != nil {
		return err
	}
	if err := t.step(stageStore, t.store); err != nil {
		return err
	}
	return t.swap()
}

func (t *trimRun) step(stage string, fn func() error) error {
	t.setStage(stage)
	return fn()
}

// cutVerifyPoster runs the three ffmpeg stages holding ONE of the
// TRANSCODE_CONCURRENCY slots (§20 erratum 79). The slot is taken here rather
// than around the whole run because a trim spends most of its life moving
// bytes — downloading the original, uploading two objects — and none of that
// competes for the CPU an operator sized the limit for. It is held through
// `verify` and `poster` on purpose: those read and write the very files the
// cut produced, so releasing earlier would let a second ffmpeg start against
// the same scratch volume this attempt has not finished with.
func (t *trimRun) cutVerifyPoster() error {
	release, err := t.w.acquireFFmpeg(t.ctx, t.log)
	if err != nil {
		return err
	}
	defer release()
	if err := t.step(t.cutStage(), t.cut); err != nil {
		return err
	}
	if err := t.step(stageVerify, t.verify); err != nil {
		return err
	}
	return t.step(stagePoster, t.poster)
}

// recheckClip closes the claim→registration window: the trim only ever runs
// against a clip that is STILL ready.
func (t *trimRun) recheckClip() error {
	c, err := t.w.deps.DB.GetClip(t.ctx, t.clip.ID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return skip("trim", "clip deleted")
	case err != nil:
		return fmt.Errorf("re-read clip: %w", err)
	case c.State == db.ClipDeleting:
		return skip("trim", "clip is being deleted")
	case c.State != db.ClipReady:
		return skip("trim", "clip is "+c.State)
	}
	t.clip = c
	return nil
}

func (t *trimRun) acquire() error {
	return acquireSource(t.ctx, t.w, t.clip, t.dir, t.srcPath, t.log, t.transferCtx, t.safeProgress)
}

// probeSource probes the ORIGINAL — never the rendition it is replacing, so
// a trim is always one generation from the source however often it is run —
// checks the window against what that original actually holds, plans it
// exactly as a transcode would, then decides the path.
//
// The coverage check comes first because it is the one that can end the
// attempt: a window with no video in it is a CLIENT condition (a request and
// a file that disagree), not a server failure, and every branch below assumes
// there is footage to cut.
func (t *trimRun) probeSource() error {
	p, err := t.w.runner.Probe(t.ctx, t.srcPath)
	if err != nil {
		return err
	}
	t.srcProbe = p
	t.ready.SourceDurationMS = sourceDurationMS(p)

	cov, err := transcode.CoverTrim(p, t.params.StartMS, t.params.EndMS)
	if err != nil {
		// Permanent, and deliberately NOT an error of ours: no retry and no
		// other machine can put video where the file has none.
		return &transcode.ErrPermanent{Err: err}
	}
	t.applied = db.TrimParams{StartMS: cov.StartMS, EndMS: cov.EndMS}
	t.covered = cov.Verified
	if cov.Clamped {
		t.log.Info("trim window clamped to the video stream", "asked_end_ms", t.params.EndMS,
			"end_ms", cov.EndMS, "video_end_s", p.Video.EndSec)
	}

	// The cut is planned in the FULL tier's box — the source's own long and
	// short sides — and not in the instance's 1280×720 one (§20 erratum 208).
	// A trim must never be the operation that costs a clip its resolution: an
	// owner shaving two seconds off a 1440p60 capture would otherwise get a
	// 720p one back, which is exactly the silent downscale these tiers exist to
	// remove. The frame-rate ceiling is raised the same way, so the cut keeps
	// the rate the footage was captured at — and, as a bonus the copy rule can
	// now reach, a cut that changes neither is a candidate for a stream copy.
	tc := t.w.deps.Cfg.Transcode
	t.plan = transcode.MakePlan(p, transcode.FullPlanOptions(p,
		transcode.PlanOptions{MaxW: tc.MaxW, MaxH: tc.MaxH, MaxFPS: tc.MaxFPS}))
	if cov.DropAudio {
		// There is audio in the file but none in this window. Mapping it would
		// ask ffmpeg to carry an empty track; the honest cut is a silent one.
		t.plan.AudioIndexes, t.plan.AudioMode = nil, transcode.AudioNone
		t.log.Info("trim drops the audio: the window is outside the audio stream",
			"audio_start_s", p.Audio.StartSec, "audio_end_s", p.Audio.EndSec)
	}

	// A copy needs the keyframe list, and listing it costs a demux of the
	// whole file — so ask the decision first with no keyframes at all. Every
	// cheap disqualifier (wrong codec, a rescale, two audio tracks) answers
	// then and there; only a source that has passed all of them comes back
	// asking for the one expensive fact.
	t.tp = transcode.DecideTrim(p, t.plan, nil, t.applied.StartMS, t.applied.EndMS)
	if !t.tp.Copy && t.tp.Reason == transcode.ReasonNoKeyframes {
		keys, err := t.w.runner.Keyframes(t.ctx, t.srcPath, p.VideoIndex)
		if err != nil {
			return err
		}
		t.tp = transcode.DecideTrim(p, t.plan, keys, t.applied.StartMS, t.applied.EndMS)
		t.log.Debug("keyframes probed", "count", len(keys))
	}
	t.tp.DropAudio = cov.DropAudio

	// The window ffmpeg is given IS the applied window: the copy path snaps
	// the start back to the keyframe it can begin at, and that snapped start
	// is what the clip will really begin with.
	t.applied.StartMS = int64(math.Round(t.tp.Window.Start * 1000))
	t.applied.EndMS = int64(math.Round((t.tp.Window.Start + t.tp.Window.Duration) * 1000))

	// The plan's duration is the SOURCE's; every consumer of it here — the
	// scratch reservation, the encode budget, the progress denominator —
	// wants the window's.
	t.plan.HasDuration = t.tp.Window.Duration > 0
	t.plan.DurationUS = int64(math.Round(t.tp.Window.Duration * 1e6))

	t.log.Info("trim planned", "path", t.tp.Path(), "reason", t.tp.Reason,
		"start_ms", t.params.StartMS, "end_ms", t.params.EndMS,
		"applied_start_ms", t.applied.StartMS, "applied_end_ms", t.applied.EndMS,
		"drop_audio", t.tp.DropAudio, "covered", t.covered,
		"ss", t.tp.Window.Start, "t", t.tp.Window.Duration,
		"out", fmt.Sprintf("%dx%d", t.plan.W, t.plan.H), "fps", t.plan.FPS.String())
	return nil
}

// cutStage names the stage after the path, so the stage alone — in the job
// row, in the API and in the log — says which of the two ran.
func (t *trimRun) cutStage() string {
	if t.tp.Copy {
		return stageCopy
	}
	return stageEncode
}

func (t *trimRun) cut() error {
	if err := checkOutputRoom(t.w, t.dir, t.plan); err != nil {
		return err
	}
	budget := outputBudget(t.clip.SourceSize, t.w.deps.Cfg.MaxUploadBytes, t.plan)
	if t.tp.Copy {
		// A remux emits no -progress and finishes in the time it takes to
		// copy the bytes; the stage band alone is the honest report.
		if err := t.w.runner.CopyTrim(t.ctx, t.tp, t.srcProbe, t.srcPath, t.outPath); err != nil {
			return err
		}
	} else if err := t.w.runner.EncodeTrim(t.ctx, t.plan, t.tp.Window, t.srcPath, t.outPath, budget, t.safeProgress); err != nil {
		return err
	}
	return t.checkOutputSize(budget)
}

func (t *trimRun) checkOutputSize(budget int64) error {
	st, err := os.Stat(t.outPath)
	if err != nil || budget <= 0 || st.Size() <= budget {
		return nil
	}
	return &transcode.ErrPermanent{Err: fmt.Errorf("output_too_large: the trimmed rendition is %d bytes, past the %d budgeted for it", st.Size(), budget)}
}

// verify applies the transcode's own output assertions plus the one a trim
// adds: the output must actually BE the window that was requested. That
// check is what stands between a stream copy's `-ss` quietly rewinding to
// the previous keyframe and a clip whose owner asked for four seconds and
// silently got nine.
func (t *trimRun) verify() error {
	info, err := t.w.runner.ProbeOutput(t.ctx, t.outPath)
	if err != nil {
		return err
	}
	// The cut is checked against the PLAN and not against the instance's frame
	// box (§20 erratum 208): since a trim is planned at the source's own size, a
	// box check would fail every cut of a clip above 1280×720 — which is exactly
	// the clip this feature exists for. Exact dimensions are strictly stronger
	// anyway: the copy path is only chosen when the plan already equals the
	// source's coded frame, and the encode path scales to the plan.
	if err := transcode.CheckTierOutput(info, transcode.TierCheck{
		Codecs: []string{transcode.FullCodecH264}, Width: t.plan.W, Height: t.plan.H,
	}); err != nil {
		return err
	}
	if err := t.w.runner.Verify(t.outPath); err != nil {
		return err
	}
	want := int64(math.Round(t.tp.Window.Duration * 1000))
	if drift := info.DurationMS - want; drift > maxTrimDurationDriftMS || drift < -maxTrimDurationDriftMS {
		return &transcode.ErrPermanent{Err: fmt.Errorf(
			"bad_output: the %s trim is %d ms long, %d ms from the %d ms asked for", t.tp.Path(), info.DurationMS, drift, want)}
	}
	t.out = info
	t.ready.Width, t.ready.Height = info.Width, info.Height
	t.ready.DurationMS = info.DurationMS
	t.ready.FPSNum, t.ready.FPSDen = info.FPS.Num, info.FPS.Den
	t.ready.HasAudio = info.HasAudio
	return nil
}

// poster re-takes the card image from the NEW rendition, and the owner's
// choice survives the cut when the cut still contains it (§20 erratum 130).
//
// The choice is a moment on the ORIGINAL's timeline — the coordinate system
// erratum 68 made this feature speak — so "does it survive" is one
// comparison against the window this run actually applied, and "where is it
// now" is one subtraction. A choice the cut removed goes back to NULL and
// the automatic rule picks the frame, which is what a clip that never had
// one gets.
//
// A chosen frame can never fail a trim. If the single-frame extraction says
// anything but yes — the moment is a hair past the last picture of a cut
// that came out a frame short, say — the run falls back to the automatic
// search and drops the choice. The alternative is failing an edit of a
// playable clip over a picture, and §17 #5's whole doctrine is that a trim
// costs the ATTEMPT and never the clip.
func (t *trimRun) poster() error {
	if seek, ok := t.chosenPosterSeek(); ok {
		// The label is the CUT's duration (§20 erratum 287): the clip this
		// poster belongs to is the trimmed one, and t.out is what it measured.
		res, err := t.w.runner.PosterAt(t.ctx, t.outPath, t.posterPath, seek, t.out.DurationMS)
		if err == nil {
			t.ready.PosterWidth, t.ready.PosterHeight = res.Width, res.Height
			t.ready.PosterSeconds = t.clip.PosterSeconds
			t.log.Info("the cut kept the chosen card image", "seconds", *t.clip.PosterSeconds,
				"rendition_seconds", seek, "near_black", res.NearBlack)
			return nil
		}
		t.log.Info("the chosen card image could not be re-taken from the cut; using the automatic frame",
			"seconds", *t.clip.PosterSeconds, "rendition_seconds", seek, "detail", t.reason(err))
	}
	// The cut's own rendition, and no PosterBox: a trim writes the file
	// `/video.mp4` serves, which carries no burned-in line for erratum 219's box
	// to be about, and the card copy that follows is re-derived by a `light` job
	// of its own.
	res, err := t.w.runner.Poster(t.ctx, t.outPath, t.posterPath,
		t.out.DurationMS*1000, t.out.DurationMS > 0, t.out.VideoEndSec, transcode.PosterBox{})
	if err != nil {
		return err
	}
	if res.NearBlack {
		t.log.Info("the trim's poster is near-black; no lit frame was found in the cut", "seconds", res.Seconds)
	}
	t.ready.PosterWidth, t.ready.PosterHeight = res.Width, res.Height
	t.ready.PosterSeconds = nil
	return nil
}

// chosenPosterSeek is the owner's chosen moment expressed on the NEW
// rendition's timeline, and false when this cut does not contain it. Both
// bounds matter: the APPLIED window says what was cut from the original, and
// the MEASURED output length says what the file really holds — a stream copy
// ends on a packet boundary, so the two are not the same sentence.
func (t *trimRun) chosenPosterSeek() (float64, bool) {
	if t.clip.PosterSeconds == nil || t.out == nil {
		return 0, false
	}
	pin := *t.clip.PosterSeconds
	start := float64(t.applied.StartMS) / 1000
	if math.IsNaN(pin) || math.IsInf(pin, 0) || pin < start || pin >= float64(t.applied.EndMS)/1000 {
		return 0, false
	}
	seek := pin - start
	if t.out.DurationMS > 0 && seek >= float64(t.out.DurationMS)/1000 {
		return 0, false
	}
	return seek, true
}

// store uploads the new rendition and poster. meta/probe.json is NOT
// rewritten: it is the record of how the clip was first encoded, it is never
// served, and overwriting it would replace that record with a description of
// an edit while leaving nothing to say what the upload itself was.
func (t *trimRun) store() error {
	res, err := t.put(t.renditionKey, t.outPath, "video/mp4", func(done, total int64) {
		if total > 0 {
			t.safeProgress(storeShareRendition*float64(done)/float64(total), false)
		}
	})
	if err != nil {
		return fmt.Errorf("upload rendition: %w", err)
	}
	t.ready.RenditionKey, t.ready.RenditionSize, t.ready.RenditionETag = t.renditionKey, res.Size, res.SHA256
	t.progress(storeShareRendition, false)
	res, err = t.put(t.posterKey, t.posterPath, "image/jpeg", nil)
	if err != nil {
		return fmt.Errorf("upload poster: %w", err)
	}
	t.ready.PosterKey, t.ready.PosterSize, t.ready.PosterETag = t.posterKey, res.Size, res.SHA256
	t.progress(1, false)
	return nil
}

func (t *trimRun) put(key, path, ct string, progress func(done, total int64)) (store.PutResult, error) {
	t.attempted = append(t.attempted, key)
	ctx, cancel := t.transferCtx()
	defer cancel()
	return t.w.deps.Store.PutFile(ctx, key, path, ct, progress)
}

func (t *trimRun) transferCtx() (context.Context, context.CancelFunc) {
	if m := t.w.deps.Cfg.S3.TransferMax; m > 0 {
		return context.WithTimeout(t.ctx, m)
	}
	return context.WithCancel(t.ctx)
}

// swap is the commit: the conditional one-UPDATE rendition swap, with the
// same bounded retries the transcode's finalize gets. ok=false is the §11
// lost race — the clip stopped being ready (an owner DELETE, the sweeper) or
// the job stopped being running — and this attempt then removes what it
// uploaded, because nothing references it.
func (t *trimRun) swap() error {
	t.setStage(stageFinalize)
	// The new rendition needs its own card file on exactly the terms a fresh
	// encode does — the skip rule applied to what this cut actually produced
	// (§17 #3, §20 erratum 121). The same UPDATE invalidates the old social
	// file and queues its replacement, so the two can never disagree, not
	// even for the width of a commit.
	t.ready.QueueSocial = lightWanted(t.ready, t.ready.RenditionSize, t.w.deps.Cfg)
	// A cut is the FULL tier whatever the upload was: it is a re-encode, or a
	// keyframe copy the rule only ever chooses for H.264 (§20 erratum 208). The
	// ORIGINAL's own measurements come from this run's re-probe of the retained
	// upload, which is a file the cut did not touch — and `original_remuxed`
	// goes back to false, because the remux this cut replaces is one of the
	// objects the same statement supersedes.
	play := transcode.OriginalPlayability(t.srcProbe, t.clip.SourceContentType, transcode.FaststartMP4(t.srcPath))
	t.ready.Tiers = db.TrimTierFields(play.Playable, t.srcProbe.DisplayW, t.srcProbe.DisplayH,
		t.srcProbe.FPS.Num, t.srcProbe.FPS.Den, t.srcProbe.VideoCodec, t.ready.QueueSocial)
	var superseded []string
	var ok, mine bool
	var err error
	for attempt := 0; ; attempt++ {
		superseded, ok, mine, err = t.w.deps.DB.SwapRendition(t.ctx, t.job.ID, t.clip.ID, t.ready, t.applied, t.w.now().Unix())
		if err == nil || t.ctx.Err() != nil || attempt >= len(finalizeBackoff) {
			break
		}
		t.log.Warn("trim swap failed; retrying", "attempt", attempt+1, "err", err)
		select {
		case <-t.ctx.Done():
		case <-time.After(finalizeBackoff[attempt]):
		}
	}
	if err != nil {
		if t.ctx.Err() == nil {
			t.log.Error("trim swap gave up", "err", err)
		}
		return fmt.Errorf("swap: %w", err)
	}
	// This attempt's own earlier commit landed (§20 erratum 102). The clip
	// already points at the objects this run uploaded, so lostRace() would
	// delete them; the keys the swap SUPERSEDED are unknown here — the first
	// call carried them off with the error — and are left to `admin orphans`,
	// which is the safe direction.
	if !ok && mine {
		t.log.Warn("trim swap had already been applied by this attempt; keeping its objects (an earlier call committed and reported an error)")
		ok = true
	}
	if !ok {
		t.lostRace()
		return errLostRace
	}
	// From here the clip references the new objects, so the OLD ones are
	// what must go — best effort, and never at the cost of the outcome.
	ctx, cancel := t.exitCtx()
	deleteKeys(ctx, t.w.deps.Store, t.log, superseded)
	cancel()
	t.removeScratch(true)
	t.w.trimProg.forget(t.clip.ID)
	t.log.Info("trim applied", "path", t.tp.Path(), "start_ms", t.applied.StartMS, "end_ms", t.applied.EndMS,
		"asked_start_ms", t.params.StartMS, "asked_end_ms", t.params.EndMS,
		"duration_ms", t.ready.DurationMS, "dims", fmt.Sprintf("%dx%d", t.ready.Width, t.ready.Height),
		"rendition_key", t.ready.RenditionKey, "superseded", len(superseded))
	return nil
}

// lostRace: the clip stopped being ready during the run. Remove the two keys
// this attempt owns and mark the job cancelled (INFO; an expected outcome).
func (t *trimRun) lostRace() {
	ctx, cancel := t.exitCtx()
	defer cancel()
	switch err := t.w.deps.DB.CancelJob(ctx, t.job.ID, db.ClaimStamp(t.job), t.w.now().Unix()); {
	case err == nil:
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		// The row already ended under another hand (§20 erratum 291).
		t.log.Info("trim job already ended under another hand; nothing to cancel", "err", err)
	case errors.Is(err, context.DeadlineExceeded):
		t.log.Warn("cancel trim job after lost race: DB slow; boot reconciliation will repair the row", "err", err)
	default:
		t.log.Error("cancel trim job after lost race", "err", err)
	}
	deleteKeys(ctx, t.w.deps.Store, t.log, t.attempted)
	t.removeScratch(true)
	t.w.trimProg.forget(t.clip.ID)
	t.log.Info("lost_race: the clip stopped being ready during the trim; outputs removed")
}

// finish settles the attempt (§11 classification), with one rule of its own:
// whatever happened, the CLIP is untouched. db.FailAttempt enforces that on
// its side (it moves a clip only for `transcode`), and the trim's caps are
// pinned to db.TrimMaxAttempts there too.
func (t *trimRun) finish(err error) {
	var pe *panicError
	switch {
	case err == nil, errors.Is(err, errLostRace):
		return
	case errors.Is(err, errSkipped):
		t.complete(db.JobStageSkipped, err.Error())
		t.removeScratch(false)
		t.w.trimProg.forget(t.clip.ID)
		t.log.Info("trim skipped", "reason", err.Error())
		return
	case errors.As(err, &pe):
		t.log.Error("trim job panicked", "stage", t.stage, "err", pe.Error(), "cancelled", t.ctx.Err() != nil, "stack", string(pe.stack))
		if t.ctx.Err() != nil {
			t.release()
			return
		}
		t.fail(db.FailCrash, err)
	case t.ctx.Err() != nil, errors.Is(err, errStopping):
		t.release()
	case errors.Is(err, errNoScratchRoom):
		t.noRoom(err)
	case transcode.IsPermanent(err):
		t.fail(db.FailPermanent, err)
	default:
		t.fail(db.FailTransient, err)
	}
}

// noRoom is the §20 erratum 90 ending: requeued with a backoff, both counters
// untouched, this attempt's outputs removed and the source kept. The clip
// still plays on the rendition it has, and the edit is still coming — so what
// the owner reads through GET …/trim is this kind's own sentence for a full
// disk, msgDisk, rather than the transcode's wording.
func (t *trimRun) noRoom(err error) {
	ctx, cancel := t.exitCtx()
	defer cancel()
	if lost := requeueNoCount(ctx, t.w, t.log, t.job, t.clip.ID, t.stage, msgDisk, t.reason(err)); lost != nil {
		t.lostAttempt(lost, t.reason(err))
		return
	}
	deleteKeys(ctx, t.w.deps.Store, t.log, t.attempted)
	t.removeScratch(false)
	t.w.trimProg.forget(t.clip.ID)
}

// lostAttempt is the §20 erratum 293 ending, this kind's copy of the one
// erratum 291 gave the transcode: the job row stopped being this attempt's
// while the attempt was running — a repair took it (`admin jobs unstick` on a
// live job, a second process's automatic repair, the boot reconcile) and a
// NEWER attempt may hold it by now, or MarkDeleting cancelled it as the clip
// was tombstoned. The row is NOT touched: an attempt recorded there would
// spend the current owner's counter, and a release or a `done` would end that
// owner's run on this one's word. What this attempt made is discarded (a trim
// keeps no checkpoint, so nothing references it), its scratch outputs go, the
// source stays for whoever runs the row next, and the progress entry is
// forgotten so the route falls back to the row. INFO: it is the expected
// ending of a repair somebody chose to run.
func (t *trimRun) lostAttempt(err error, detail string) {
	ctx, cancel := t.exitCtx()
	defer cancel()
	t.log.Info("trim job row is no longer this attempt's; outputs discarded, row untouched",
		"stage", t.stage, "err", err, "detail", detail)
	deleteKeys(ctx, t.w.deps.Store, t.log, t.attempted)
	t.removeScratch(false)
	t.w.trimProg.forget(t.clip.ID)
}

// complete resolves a job that must not run as `done` with the reason in
// last_error (CompleteJob refuses a transcode job, which is the point).
func (t *trimRun) complete(stage, note string) {
	ctx, cancel := t.exitCtx()
	defer cancel()
	err := t.w.deps.DB.CompleteJob(ctx, t.job.ID, db.ClaimStamp(t.job), stage, note, t.w.now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		t.lostAttempt(err, note)
	default:
		t.log.Error("complete trim job", "note", note, "err", err)
	}
}

// reason renders the LOGGED failure detail, paths and bucket coordinates
// redacted. It is not what the owner sees — ownerReason is — so it may carry
// an ffmpeg stderr tail; the redaction is kept anyway, because the server's
// directory layout is nobody's business and a log line is quoted into issues.
func (t *trimRun) reason(err error) string {
	msg := err.Error()
	var pe *transcode.ErrPermanent
	if errors.As(err, &pe) {
		msg = pe.Err.Error()
	}
	msg = redactPaths(msg, t.dir, t.w.deps.Cfg)
	if t.stage != "" {
		msg = t.stage + ": " + msg
	}
	if len(msg) > maxReasonLen {
		msg = msg[:maxReasonLen]
	}
	return msg
}

func (t *trimRun) exitCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(t.ctx), exitBudget)
}

// release is the Cancel/Stop path: the job goes back to queued with both
// counters untouched, and this attempt's uploads are removed — a trim keeps
// no checkpoint, so a retry simply redoes the work under a fresh {r8}.
func (t *trimRun) release() {
	why := t.run.reason()
	if why == reasonNone {
		why = reasonShutdown
	}
	ctx, cancel := t.exitCtx()
	err := t.w.deps.DB.ReleaseJob(ctx, t.job.ID, db.ClaimStamp(t.job), t.clip.ID, t.w.now().Unix())
	cancel()
	switch {
	case err == nil:
		t.log.Info("trim job released", "reason", why.String(), "stage", t.stage)
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		t.lostAttempt(err, why.String())
		return
	default:
		t.log.Error("release trim job", "reason", why.String(), "err", err)
	}
	dctx, dcancel := t.exitCtx()
	deleteKeys(dctx, t.w.deps.Store, t.log, t.attempted)
	dcancel()
	t.removeScratch(false)
	t.w.trimProg.forget(t.clip.ID)
}

// fail records one bad ending. The clip is never marked: db.FailAttempt
// couples a clip only for `transcode`, and a trim's crash cap is pinned to
// db.TrimMaxAttempts there (its environment cap is the instance's own,
// §20 erratum 89).
//
// Two texts come out of one error, and the split is the point (§20 erratum
// 80): `owner` — a short fixed sentence — is what goes in the row and reaches
// the owner through GET …/trim, while `detail`, which may be an ffmpeg stderr
// tail, stays in the log where an operator can read it and a visitor cannot.
func (t *trimRun) fail(kind db.FailKind, cause error) {
	detail, owner := t.reason(cause), t.ownerReason(cause)
	ctx, cancel := t.exitCtx()
	defer cancel()
	outcome, err := t.w.deps.DB.FailAttempt(ctx, t.job.ID, db.ClaimStamp(t.job), t.clip.ID, owner, kind, t.w.lim, t.w.now().Unix())
	defer t.w.trimProg.forget(t.clip.ID)
	switch {
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		// The row is not this attempt's to fail (§20 erratum 293): the clip is
		// being deleted and lifecycle removes the whole prefix, or a repair
		// took the row and a newer attempt may hold it. Either way this
		// attempt's keys are ours to clean up and the row is nobody's to spend.
		t.lostAttempt(err, detail)
		return
	case err != nil:
		t.log.Error("fail trim attempt", "kind", kindName(kind), "detail", detail, "err", err)
		deleteKeys(ctx, t.w.deps.Store, t.log, t.attempted)
		t.removeScratch(false)
		return
	}
	deleteKeys(ctx, t.w.deps.Store, t.log, t.attempted)
	if outcome == db.OutcomeFailed {
		t.removeScratch(true)
		switch {
		case kind == db.FailPermanent && t.covered && strings.Contains(detail, "bad_output"):
			// A verify mismatch on our own output, over a window the source
			// was MEASURED to cover, cannot happen by design (§12) — it needs
			// a human. The clip is unharmed either way.
			t.log.Error("trim failed: bad_output from our own trim", "detail", detail, "reason", owner)
		case kind == db.FailPermanent:
			// Everything the FILE (or the request) is responsible for, the
			// uncovered-window bad_output included: INFO, because nothing here
			// is broken — the clip still plays on the rendition it had.
			t.log.Info("trim failed; the clip keeps its rendition", "reason", owner, "detail", detail, "covered", t.covered)
		default:
			// The cap named is the one this ending actually exhausted: the
			// crash cap is db.TrimMaxAttempts, the environment cap is the
			// instance's TRANSCODE_MAX_TRANSIENT (§20 erratum 89).
			t.log.Warn("trim failed: retry cap reached; the clip keeps its rendition", "kind", kindName(kind),
				"reason", owner, "detail", detail, "cap", db.Cap(db.KindTrim, kind, t.w.lim))
		}
		return
	}
	t.removeScratch(false)
	t.log.Warn("trim attempt failed; requeued", "kind", kindName(kind), "reason", owner, "detail", detail)
}

// The owner-facing failure vocabulary. Short, plain, fixed, and complete: an
// owner reads one of these on the share page, and nothing an ffmpeg build
// happens to print on stderr — nor a path, a byte count or a pointer — belongs
// on a page a link can be shared to.
const (
	msgNoWindow  = "there is no video in the part of the original you asked for"
	msgTooLarge  = "the trimmed clip came out larger than this server allows"
	msgBadOutput = "the trim did not produce the clip that was asked for"
	msgMedia     = "this clip's original could not be trimmed"
	msgDisk      = "the server had no room to make the cut; try again later"
	msgStorage   = "storage was unavailable while the trim ran"
	msgInternal  = "the trim failed"
)

// ownerReason maps one failure onto exactly one of those sentences. The
// markers it matches on are ours — `bad_output`, `output_too_large`,
// `insufficient scratch`, transcode.ErrWindowEmpty — and the stage is the
// fallback, so an error nobody anticipated still comes out as a sentence
// rather than as whatever it happened to say.
//
// The scratch marker is kept although `finish` now routes errNoScratchRoom to
// noRoom (which writes msgDisk directly) before any fail path can see it: it
// costs a line, and a future refusal worded that way by some other code must
// not fall through to the stage guess (§20 erratum 90).
func (t *trimRun) ownerReason(err error) string {
	var pe *panicError
	switch {
	case err == nil:
		return msgInternal
	case errors.Is(err, transcode.ErrWindowEmpty):
		return msgNoWindow
	case errors.As(err, &pe):
		return msgInternal
	}
	detail := err.Error()
	switch {
	case strings.Contains(detail, "output_too_large"):
		return msgTooLarge
	case strings.Contains(detail, "insufficient scratch"):
		return msgDisk
	case strings.Contains(detail, "bad_output"):
		return msgBadOutput
	}
	switch t.stage {
	case stageAcquire, stageStore:
		return msgStorage
	case stageProbe, stageCopy, stageEncode, stageVerify, stagePoster:
		return msgMedia
	}
	return msgInternal
}

// removeScratch removes the whole clip dir (a committed swap, a permanent
// failure) or only this attempt's outputs (the source stays for the retry).
func (t *trimRun) removeScratch(all bool) {
	if all {
		if err := os.RemoveAll(t.dir); err != nil {
			t.log.Warn("remove scratch dir", "dir", t.dir, "err", err)
		}
		return
	}
	for _, p := range []string{t.outPath, t.posterPath} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.log.Warn("remove scratch output", "path", p, "err", err)
		}
	}
}

// --- progress (the §17 #5 registry; never the §8.4 one) ---

func (t *trimRun) setStage(stage string) {
	t.stage = stage
	if lo := bands[stage][0]; lo > t.percent {
		t.percent = lo
	}
	t.setProgress(Progress{State: db.JobRunning, Stage: stage, Percent: t.percent}, true)
	err := t.w.deps.DB.SetJobStage(t.ctx, t.job.ID, db.ClaimStamp(t.job), stage, t.w.now().Unix())
	switch {
	case err == nil, t.ctx.Err() != nil:
	case errors.Is(err, db.ErrNotFound):
		t.log.Info("trim job row gone while setting stage", "stage", stage)
	case errors.Is(err, db.ErrStateMismatch):
		// Repaired or cancelled under another hand (§20 erratum 291): the stage is
		// abandoned, the run goes on, and the lost row ends it at the next write
		// that matters — never as a failure of the row's new owner.
		t.log.Info("trim job row is no longer this attempt's; stage not recorded", "stage", stage)
	default:
		t.log.Error("set trim job stage", "stage", stage, "err", err)
	}
}

func (t *trimRun) progress(frac float64, indeterminate bool) {
	b := bands[t.stage]
	pct := b[0]
	if !indeterminate {
		frac = math.Min(math.Max(frac, 0), 1)
		pct = int(math.Floor(float64(b[0]) + frac*float64(b[1]-b[0])))
	}
	pct = min(max(pct, 0), 100)
	if pct > t.percent {
		t.percent = pct
	}
	t.setProgress(Progress{State: db.JobRunning, Stage: t.stage, Percent: t.percent, Indeterminate: indeterminate}, false)
}

// safeProgress is the callback handed to the runner and the store; it runs
// on their goroutines, outside this run's recover.
func (t *trimRun) safeProgress(frac float64, indeterminate bool) {
	defer func() {
		if p := recover(); p != nil {
			t.panicOnce.Do(func() {
				t.log.Error("trim progress callback panicked", "stage", t.stage, "err", fmt.Sprint(p), "stack", string(debug.Stack()))
			})
		}
	}()
	t.progress(frac, indeterminate)
}

// setProgress writes the trim registry under worker.mu, the same lock
// Forget takes, so a voided run resurrects nothing (§20 erratum 49).
func (t *trimRun) setProgress(p Progress, force bool) {
	t.w.mu.Lock()
	defer t.w.mu.Unlock()
	if t.run.progressVoid {
		return
	}
	t.w.trimProg.set(t.clip.ID, p, force)
}
