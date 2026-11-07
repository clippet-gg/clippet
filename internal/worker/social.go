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

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/store"
	"clippet/internal/transcode"
)

// The dedicated social rendition (DESIGN.md §17 #3, §20 errata 121–124).
//
// `/social.mp4` is the file every card points at, and until now it was the
// main rendition. That rendition is sized for a player somebody opened on
// purpose: 60 fps, CRF 23, no ceiling. A card is fetched by Discord itself
// and stops playing inline somewhere past ~100 MB, so a four-minute
// high-motion 720p60 clip — the exact clip this product exists to share — is
// the one that degrades to a link.
//
// The job is the GIF job's twin, and deliberately so:
//
//  1. It reads the RENDITION, never the original. That is not a shortcut, it
//     is the only correct input: the rendition IS the clip as everyone sees
//     it, trim window and all, and re-deriving from the upload would mean
//     re-deciding the trim, the rotation and the tone map in a second place
//     and getting a card that shows something the player does not. The cost
//     argument agrees — the rendition is bounded, the original is whatever
//     somebody uploaded — but it is the second reason, not the first.
//  2. It writes five columns and nothing else. The clip is `ready` before,
//     during and after; db.SetSocial enforces that on its side.
//  3. Its commit is guarded by the rendition's ETag. A trim that lands while
//     this job runs replaces the frames the card file was encoded from, and
//     a card pointing at the old ones is worse than a card pointing at the
//     rendition — which is exactly what the route falls back to.
//
// A failure of any of it costs the clip nothing: `/social.mp4` keeps serving
// the main rendition, which is what it served before this feature existed.

// lightWanted is the SKIP RULE at COMMIT time (§20 erratum 121, restated by
// erratum 203), applied to the file a card would fetch if this commit published
// nothing more.
//
// A Light copy is built when that file is at or above SOCIAL_MAX_BYTES, or runs
// above 30 fps, or is larger than 1280×720; a file that is small, calm and
// card-sized already IS what a card wants, and encoding a second copy of it
// would spend an ffmpeg slot to produce something indistinguishable. The three
// terms are not redundant: a 6-second 60 fps clip is far under any byte
// threshold, and 60 fps is precisely what makes a longer one of the same footage
// too big — the rule catches the SHAPE, not just the instance.
//
// Two things changed with the tiers. `playSize` is the size of the card's file
// — the rendition when there is one, and the UPLOAD itself on the remux fast
// path, where there is no rendition object at all — because the file a crawler
// fetches is no longer always a rendition. And a commit that has ALREADY made
// the Light copy (card-first ordering, §20 erratum 206) says no here: the work
// is done, and queueing a job to redo it would spend a slot on a duplicate.
//
// SOCIAL_MAX_BYTES = 0 turns the whole tier off, whatever the shape.
func lightWanted(f db.ReadyFields, playSize int64, cfg config.Config) bool {
	if cfg.SocialMaxBytes <= 0 || f.Light.Key != "" {
		return false
	}
	return transcode.LightWanted(cfg.SocialMaxBytes, f.Width, f.Height,
		transcode.Rational{Num: f.FPSNum, Den: f.FPSDen}, playSize, playSize > 0)
}

// socialRun is the state of one social attempt.
type socialRun struct {
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

	// renditionETag is the PLAYABLE FILE this copy is encoded from — the
	// rendition when there is one, the upload itself on the remux fast path
	// (db.PlayObject) — read at the start and handed to SetSocial as the
	// commit's predicate. renditionKey/renditionSize are that same object.
	renditionKey  string
	renditionETag string
	renditionSize int64

	plan   transcode.Plan
	out    *transcode.OutputInfo
	fields db.SocialFields
}

func newSocialRun(w *worker, ctx context.Context, r *run, job db.Job, clip db.Clip) *socialRun {
	s := &socialRun{w: w, ctx: ctx, run: r, job: job, clip: clip}
	s.log = w.log.With("clip", clip.ID, "job", job.ID)
	s.r8 = ids.Rand(8)
	s.dir = filepath.Join(w.deps.Cfg.ScratchDir, clip.ID)
	s.srcPath = filepath.Join(s.dir, "socialsrc-"+s.r8+".mp4")
	s.outPath = filepath.Join(s.dir, "social-"+s.r8+".mp4")
	s.key = store.SocialKey(w.deps.Cfg.S3.KeyPrefix, clip.ID, s.r8)
	return s
}

// execute runs one attempt under recover() and settles its outcome.
func (s *socialRun) execute() {
	s.log.Info("social started", "attempt", s.job.Attempts+1, "transient_failures", s.job.TransientFailures)
	err := s.guarded()
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("social exit path panicked", "stage", s.stage, "err", fmt.Sprint(p), "stack", string(debug.Stack()))
		}
	}()
	s.finish(err)
}

func (s *socialRun) guarded() (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = &panicError{value: p, stack: debug.Stack()}
		}
	}()
	return s.pipeline()
}

func (s *socialRun) pipeline() error {
	if err := s.recheckClip(); err != nil {
		return err
	}
	s.setStage(stageAcquire)
	if err := s.acquire(); err != nil {
		return err
	}
	if err := s.encodeVerify(); err != nil {
		return err
	}
	s.setStage(stageStore)
	if err := s.store(); err != nil {
		return err
	}
	return s.commit()
}

// encodeVerify holds ONE of the TRANSCODE_CONCURRENCY ffmpeg slots (§20
// erratum 79) across the probe, the encode and the two verify passes — every
// stage of this job that spawns ffmpeg. Without it a default install would
// run two encoders the moment a clip became ready, which is precisely the
// promise DEPLOY §7 makes and this feature's own worst case: a social encode
// is queued by the same commit that finished a transcode, so the next one is
// already waiting.
func (s *socialRun) encodeVerify() error {
	release, err := s.w.acquireFFmpeg(s.ctx, s.log)
	if err != nil {
		return err
	}
	defer release()
	s.setStage(stageProbe)
	if err := s.planEncode(); err != nil {
		return err
	}
	s.setStage(stageEncode)
	if err := s.encode(); err != nil {
		return err
	}
	s.setStage(stageVerify)
	return s.verify()
}

// recheckClip closes the claim→registration window and reads the rendition
// this file will describe. A clip that is no longer ready — or that has
// somehow no rendition — is skipped, not failed: nothing is wrong with it.
//
// It also RE-APPLIES the skip rule to the rendition it just read (§20 erratum
// 169). The rule is decided at commit time by whoever publishes a rendition
// (FinalizeTranscode, SwapRendition, SwapSource) and turned into `QueueSocial`
// — but `enqueueSocialTx` leaves an already-queued job alone rather than
// re-deciding (a RUNNING one is superseded, erratum 237 (a)). The ETag
// predicate only catches a job that had already STARTED and read the old
// ETag; one still `queued`
// when a trim commits reads the NEW rendition here and encodes it. So a clip
// that qualified by SIZE alone and was then trimmed small got one social
// encode the rule would have refused — an ffmpeg slot and an object spent on
// a file the card does not need. Re-deciding from the row in hand costs
// nothing and is the same function the commit used.
func (s *socialRun) recheckClip() error {
	c, err := s.w.deps.DB.GetClip(s.ctx, s.clip.ID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return skip("social", "clip deleted")
	case err != nil:
		return fmt.Errorf("re-read clip: %w", err)
	case c.State == db.ClipDeleting:
		return skip("social", "clip is being deleted")
	case c.State != db.ClipReady:
		return skip("social", "clip is "+c.State)
	}
	// The file the card would otherwise fetch, resolved from the row: the
	// rendition when there is one, the upload itself on the remux fast path
	// (§20 erratum 201). A clip whose only playable bytes ARE the Light copy —
	// one still waiting for its Full — is skipped rather than re-encoded from
	// its own output: that would be a copy of a copy.
	key, size, etag, ok := db.PlayObject(c)
	if !ok {
		return skip("light", "clip has no playable file to encode from")
	}
	if c.SocialKey != nil && *c.SocialKey == key {
		return skip("light", "the only playable file IS the light copy")
	}
	f := db.ReadyFields{}
	if c.Width != nil && c.Height != nil {
		f.Width, f.Height = *c.Width, *c.Height
	}
	if c.FPSNum != nil && c.FPSDen != nil {
		f.FPSNum, f.FPSDen = *c.FPSNum, *c.FPSDen
	}
	if !lightWanted(f, size, s.w.deps.Cfg) {
		return skip("light", "the playable file no longer qualifies")
	}
	s.clip = c
	s.renditionKey, s.renditionETag, s.renditionSize = key, etag, size
	s.log.Info("light source", "key", key, "bytes", size)
	return nil
}

// acquire downloads the rendition to scratch under a per-attempt name. It is
// never cached between attempts, for the gif job's reason: a rendition is
// small and bounded, and a stale copy of the WRONG rendition is exactly the
// failure this feature must not have.
func (s *socialRun) acquire() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("scratch dir: %w", err)
	}
	// The bytes are about to land, so they are counted once, here — the same
	// rule acquireSource applies to a source it is about to fetch (§20
	// erratum 90). The OUTPUT's room is reserved after the plan knows how
	// big it will be (checkOutputRoom, below).
	if err := requireScratch(s.w, s.dir, satAdd(s.renditionSize, s.w.deps.Cfg.ScratchMinFree)); err != nil {
		return err
	}
	ctx, cancel := s.transferCtx()
	err := s.w.deps.Store.Download(ctx, s.renditionKey, s.srcPath, nil)
	cancel()
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// A ready row pointing at an object the bucket does not hold is
			// the drift §12 says cannot happen (ERROR); the attempt is still
			// transient, because a lagging bucket looks the same.
			s.log.Error("playable file missing from the bucket (row/object drift)", "key", s.renditionKey)
		}
		return fmt.Errorf("download rendition: %w", err)
	}
	st, err := os.Stat(s.srcPath)
	if err != nil {
		return fmt.Errorf("downloaded rendition: %w", err)
	}
	if st.Size() != s.renditionSize {
		return fmt.Errorf("downloaded rendition is %d bytes, row says %d", st.Size(), s.renditionSize)
	}
	return nil
}

// planEncode probes the rendition and plans the social encode from it.
//
// The probe is a real one rather than a read of the clip's own columns: the
// plan needs the video stream index, the audio stream indexes and the frame
// rate, and only two of those are on the row. The box is the instance's own
// with the frame rate capped at 30 (transcode.SocialPlanOptions), so a
// rendition that already fits the box is not rescaled — the social file has
// the rendition's dimensions and half its frame rate.
func (s *socialRun) planEncode() error {
	p, err := s.w.runner.Probe(s.ctx, s.srcPath)
	if err != nil {
		return err
	}
	tc := s.w.deps.Cfg.Transcode
	// The LIGHT tier's own box — 1280×720 at 30 fps, narrowed but never widened
	// by the instance's (§20 erratum 203). Before the tiers this was the
	// instance's box alone, which was right while the input was always a
	// rendition that already fitted it; the input can now be a 4K upload.
	s.plan = transcode.MakePlan(p, transcode.LightPlanOptions(transcode.PlanOptions{MaxW: tc.MaxW, MaxH: tc.MaxH, MaxFPS: tc.MaxFPS}))
	s.log.Info("social planned", "out", fmt.Sprintf("%dx%d", s.plan.W, s.plan.H), "fps", s.plan.FPS.String(),
		"audio", s.plan.AudioMode.String(), "duration_us", s.plan.DurationUS)
	return nil
}

// encode runs the one instrumented pass with the card settings, under the
// same output budget the trim gets: the bounds this clip has already been
// granted, and no new ones.
func (s *socialRun) encode() error {
	if err := checkOutputRoom(s.w, s.dir, s.plan); err != nil {
		return err
	}
	budget := outputBudget(s.renditionSize, s.w.deps.Cfg.MaxUploadBytes, s.plan)
	// The tag (§20 erratum 214). This job's "best quality" is the clip's own
	// five measurements — the shape of the file `/video.mp4` serves, which is
	// exactly what a reader following the link will get.
	tag, err := s.w.runner.LightTagFor(s.dir, "tag-"+s.r8+".txt",
		transcode.QualityShort(s.plan.W, s.plan.H, s.plan.FPS), clipQualityShort(s.clip))
	if err != nil {
		return err
	}
	if err := s.w.runner.EncodeSocial(s.ctx, s.plan, s.srcPath, s.outPath, tag, budget, nil); err != nil {
		return err
	}
	st, err := os.Stat(s.outPath)
	if err == nil && budget > 0 && st.Size() > budget {
		return &transcode.ErrPermanent{Err: fmt.Errorf(
			"output_too_large: the social rendition is %d bytes, past the %d budgeted for it", st.Size(), budget)}
	}
	return nil
}

// verify measures what we produced with the same two checks the rendition
// gets — one h264 video stream, at most one aac audio stream, even dims
// inside the box, a positive duration, and moov before mdat — because the
// card points a CRAWLER at this file and a crawler is the least forgiving
// player there is. The dims it reports are what the card then states.
func (s *socialRun) verify() error {
	info, err := s.w.runner.ProbeOutput(s.ctx, s.outPath)
	if err != nil {
		return err
	}
	tc := s.w.deps.Cfg.Transcode
	if err := transcode.CheckOutput(info, transcode.PlanOptions{MaxW: tc.MaxW, MaxH: tc.MaxH, MaxFPS: tc.MaxFPS}); err != nil {
		return err
	}
	if err := s.w.runner.Verify(s.outPath); err != nil {
		return err
	}
	st, err := os.Stat(s.outPath)
	if err != nil {
		return &transcode.ErrPermanent{Err: fmt.Errorf("bad_output: the social rendition is unreadable: %w", err)}
	}
	s.out = info
	s.fields = db.SocialFields{Key: s.key, Size: st.Size(), Width: info.Width, Height: info.Height,
		FPSNum: info.FPS.Num, FPSDen: info.FPS.Den}
	s.log.Info("light copy encoded", "bytes", st.Size(), "dims", fmt.Sprintf("%dx%d", info.Width, info.Height),
		"fps", info.FPS.String(), "source_bytes", s.renditionSize)
	return nil
}

func (s *socialRun) store() error {
	s.attempted = append(s.attempted, s.key)
	ctx, cancel := s.transferCtx()
	defer cancel()
	res, err := s.w.deps.Store.PutFile(ctx, s.key, s.outPath, "video/mp4", nil)
	if err != nil {
		return fmt.Errorf("upload social rendition: %w", err)
	}
	s.fields.Size, s.fields.ETag = res.Size, res.SHA256
	return nil
}

// commit is the one guarded UPDATE. A swap that does not happen is not a
// failure of anything: the attempt deletes its own object and resolves the
// job as done with the reason, because there is nothing to retry — the
// rendition this file was encoded from is not the one that exists now, and
// the commit that replaced it queued a fresh job of its own.
func (s *socialRun) commit() error {
	s.setStage(stageFinalize)
	swap, err := s.w.deps.DB.SetSocial(s.ctx, s.job.ID, s.clip.ID, s.fields, s.renditionETag, s.w.now().Unix())
	if err != nil {
		if s.ctx.Err() == nil {
			s.log.Error("set social failed", "err", err)
		}
		return fmt.Errorf("set social: %w", err)
	}
	if !swap.OK {
		s.skipped(swap.Reason)
		return errLostRace
	}
	ctx, cancel := s.exitCtx()
	deleteKeys(ctx, s.w.deps.Store, s.log, swap.Superseded)
	cancel()
	s.removeScratch()
	s.log.Info("social applied", "key", s.fields.Key, "bytes", s.fields.Size,
		"dims", fmt.Sprintf("%dx%d", s.fields.Width, s.fields.Height), "superseded", len(swap.Superseded))
	return nil
}

// skipped resolves the job as done with the reason and removes this
// attempt's object — the §11 lost-race shape, at INFO because it is an
// expected outcome of a concurrent trim.
func (s *socialRun) skipped(reason string) {
	ctx, cancel := s.exitCtx()
	defer cancel()
	s.complete(db.JobStageSkipped, "skipped: "+reason)
	// A lost race is a finished answer about the tier too (§20 erratum 237). A
	// commit that queued a replacement card copy superseded this row first (237
	// (a)), so that ending never reaches here; this one is a race no commit queued
	// a job behind. The guard is what makes the line safe: when a job is live,
	// this writes nothing and the promise stands.
	s.markSkipped()
	deleteKeys(ctx, s.w.deps.Store, s.log, s.attempted)
	s.removeScratch()
	s.log.Info("social skipped; the card file was not published", "reason", reason)
}

// markSkipped records `light_state = skipped` for a job that will not run, but
// ONLY while the row still promises `encoding` and no `social` job is left to keep
// that promise — db.SettleTierEncoding's guard (§20 erratum 237). It is
// full.markSkipped's twin, and it matters MORE than its twin: an `encoding` card
// copy is the one tier state erratum 235's `CardReady` belt waits on, so a
// `light_state` left at `encoding` with no job behind it holds a crawler off the
// clip with `503 Retry-After: 30` for the life of the clip, tells `POST
// /api/upload` and the watch page the card is not ready, and has `tiersLive` poll
// /progress on every view. Nothing an operator can do clears it.
func (s *socialRun) markSkipped() {
	ctx, cancel := s.exitCtx()
	defer cancel()
	settleTier(ctx, s.w, s.log, s.clip.ID, db.KindSocial, db.TierSkipped)
}

// finish settles the attempt (§11 classification) with the media kinds' one
// extra rule: whatever happened, the CLIP is untouched — db.FailAttempt moves
// a clip only for `transcode`, and a social job's crash cap is
// db.SocialMaxAttempts (its environment cap is the instance's, erratum 89).
func (s *socialRun) finish(err error) {
	var pe *panicError
	switch {
	case err == nil, errors.Is(err, errLostRace):
		return
	case errors.Is(err, errSkipped):
		s.complete(db.JobStageSkipped, err.Error())
		// A skip is a finished answer about the tier too: nothing is coming, and
		// neither the selector nor the card's own readiness may go on saying
		// `encoding` for a job that stopped (§20 erratum 237).
		s.markSkipped()
		s.removeScratch()
		s.log.Info("social skipped", "reason", err.Error())
	case errors.As(err, &pe):
		s.log.Error("social job panicked", "stage", s.stage, "err", pe.Error(), "cancelled", s.ctx.Err() != nil, "stack", string(pe.stack))
		if s.ctx.Err() != nil {
			s.release()
			return
		}
		s.fail(db.FailCrash, err)
	case s.ctx.Err() != nil, errors.Is(err, errStopping):
		s.release()
	case errors.Is(err, errNoScratchRoom):
		s.noRoom(err)
	case transcode.IsPermanent(err):
		s.fail(db.FailPermanent, err)
	default:
		s.fail(db.FailTransient, err)
	}
}

// noRoom is the §20 erratum 90 ending: requeued with a backoff, both counters
// untouched, this attempt's object removed.
func (s *socialRun) noRoom(err error) {
	ctx, cancel := s.exitCtx()
	defer cancel()
	if lost := requeueNoCount(ctx, s.w, s.log, s.job, s.clip.ID, s.stage, msgNoScratchRoom, s.reason(err)); lost != nil {
		s.lostAttempt(lost, s.reason(err))
		return
	}
	deleteKeys(ctx, s.w.deps.Store, s.log, s.attempted)
	s.removeScratch()
}

// lostAttempt is the §20 erratum 293 ending, this kind's copy of the one
// erratum 291 gave the transcode: the row stopped being this attempt's — a
// repair took it and a NEWER attempt may hold it, or MarkDeleting cancelled it
// with the clip — so the row is NOT touched (an attempt recorded there spends
// the current owner's counter) and only what this attempt made is discarded.
// The TIER column is left alone for the same reason: `light_state` still says
// `encoding`, and it is the attempt that holds the row that will settle it.
func (s *socialRun) lostAttempt(err error, detail string) {
	ctx, cancel := s.exitCtx()
	defer cancel()
	s.log.Info("social job row is no longer this attempt's; outputs discarded, row untouched",
		"stage", s.stage, "err", err, "detail", detail)
	deleteKeys(ctx, s.w.deps.Store, s.log, s.attempted)
	s.removeScratch()
}

// complete resolves a job that must not run as `done` with the reason in
// last_error (CompleteJob refuses a transcode job, which is the point).
func (s *socialRun) complete(stage, note string) {
	ctx, cancel := s.exitCtx()
	defer cancel()
	err := s.w.deps.DB.CompleteJob(ctx, s.job.ID, db.ClaimStamp(s.job), stage, note, s.w.now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		s.lostAttempt(err, note)
	default:
		s.log.Error("complete social job", "note", note, "err", err)
	}
}

// release is the Cancel/Stop path: the job returns to queued with both
// counters untouched and this attempt's object is removed — a social job
// keeps no checkpoint, so a retry redoes the work under a fresh {r8}.
func (s *socialRun) release() {
	why := s.run.reason()
	if why == reasonNone {
		why = reasonShutdown
	}
	ctx, cancel := s.exitCtx()
	err := s.w.deps.DB.ReleaseJob(ctx, s.job.ID, db.ClaimStamp(s.job), s.clip.ID, s.w.now().Unix())
	switch {
	case err == nil:
		s.log.Info("social job released", "reason", why.String(), "stage", s.stage)
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		cancel()
		s.lostAttempt(err, why.String())
		return
	default:
		s.log.Error("release social job", "reason", why.String(), "err", err)
	}
	deleteKeys(ctx, s.w.deps.Store, s.log, s.attempted)
	cancel()
	s.removeScratch()
}

// fail records one bad ending. The clip keeps its rendition, its poster and
// its readiness; only the card's dedicated file is missing, and `/social.mp4`
// answers with the rendition exactly as it did before this feature.
func (s *socialRun) fail(kind db.FailKind, cause error) {
	reason, detail := s.reason(cause), cause.Error()
	ctx, cancel := s.exitCtx()
	defer cancel()
	outcome, err := s.w.deps.DB.FailAttempt(ctx, s.job.ID, db.ClaimStamp(s.job), s.clip.ID, reason, kind, s.w.lim, s.w.now().Unix())
	switch {
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		// Not this attempt's row to fail (§20 erratum 293).
		s.lostAttempt(err, detail)
		return
	case err != nil:
		s.log.Error("fail social attempt", "kind", kindName(kind), "reason", reason, "detail", detail, "err", err)
	case outcome == db.OutcomeFailed && kind == db.FailPermanent && strings.Contains(reason, "bad_output"):
		// A verify mismatch on our own output cannot happen by design (§12).
		s.log.Error("social failed: bad_output from our own encoder", "reason", reason, "detail", detail)
	case outcome == db.OutcomeFailed:
		s.log.Info("light copy failed; the clip is unaffected and the card points at the playable file",
			"kind", kindName(kind), "reason", reason, "detail", detail, "cap", db.Cap(db.KindSocial, kind, s.w.lim))
	default:
		s.log.Warn("light attempt failed; requeued", "kind", kindName(kind), "reason", reason, "detail", detail)
	}
	if outcome == db.OutcomeFailed {
		// The tier state, so the quality selector stops promising a copy that is
		// not coming (§20 erratum 205). It is a footnote on the job's ending and
		// never a reason to fail one: an error here is logged and dropped.
		settleTier(ctx, s.w, s.log, s.clip.ID, db.KindSocial, db.TierFailed)
	}
	deleteKeys(ctx, s.w.deps.Store, s.log, s.attempted)
	s.removeScratch()
}

// reason renders the stored failure text. A social job's last_error reaches
// no API route at all, but it is written by the same helper as every other
// kind's so a bucket endpoint can never start leaking through a new one.
func (s *socialRun) reason(err error) string {
	return publicReason(err, s.stage, s.dir, s.w.deps.Cfg)
}

func (s *socialRun) transferCtx() (context.Context, context.CancelFunc) {
	if m := s.w.deps.Cfg.S3.TransferMax; m > 0 {
		return context.WithTimeout(s.ctx, m)
	}
	return context.WithCancel(s.ctx)
}

func (s *socialRun) exitCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(s.ctx), exitBudget)
}

// removeScratch removes only THIS attempt's two files. Like the gif run and
// unlike the transcode, a social run never owns the clip's scratch dir: it
// did not put the source there and a trim may be about to reuse it.
func (s *socialRun) removeScratch() {
	for _, p := range []string{s.srcPath, s.outPath} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("remove scratch output", "path", p, "err", err)
		}
	}
	if err := os.Remove(s.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Debug("scratch dir kept", "dir", s.dir, "err", err)
	}
}

// setStage persists the stage. Like the gif job, a social job has NO progress
// registry: nothing polls it, the clip it belongs to is `ready` throughout,
// and its only public trace is the file appearing at a URL that already
// worked.
func (s *socialRun) setStage(stage string) {
	s.stage = stage
	err := s.w.deps.DB.SetJobStage(s.ctx, s.job.ID, db.ClaimStamp(s.job), stage, s.w.now().Unix())
	switch {
	case err == nil, s.ctx.Err() != nil:
	case errors.Is(err, db.ErrNotFound):
		s.log.Info("social job row gone while setting stage", "stage", stage)
	case errors.Is(err, db.ErrStateMismatch):
		// Repaired or cancelled under another hand (§20 erratum 291): the stage is
		// abandoned, the run goes on, and the lost row ends it at the next write
		// that matters — never as a failure of the row's new owner.
		s.log.Info("social job row is no longer this attempt's; stage not recorded", "stage", stage)
	default:
		s.log.Error("set social job stage", "stage", stage, "err", err)
	}
}
