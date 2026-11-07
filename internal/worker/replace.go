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
	"clippet/internal/scratch"
	"clippet/internal/store"
	"clippet/internal/transcode"
)

// Clip replacement (ROADMAP #31, DESIGN.md §20 errata 135–136).
//
// A replacement is the transcode pipeline pointed at a DIFFERENT original —
// the one the owner has just uploaded under the same clip id — with the two
// properties every media kind has:
//
//  1. The clip is `ready` before, during and after. Nothing here writes
//     clips.state, clips.failure_reason or either failure counter (§5); the
//     db layer refuses to anyway. So the share page, the embed and any card
//     already posted keep playing the OLD file for the whole run, and start
//     playing the new one the instant it commits — a viewer never sees the
//     clip processing, and never sees it fail.
//  2. The result is committed by ONE UPDATE (db.SwapSource) guarded by
//     `state='ready'` AND the job still being `running`. Every object this
//     run writes carries a fresh {r8} (§6) — the replacement original
//     included — so an attempt that never reaches that UPDATE has published
//     nothing a reader could reach.
//
// The superseded objects are deleted AFTER the commit, best effort, and that
// list starts with the PREVIOUS ORIGINAL. "Upload is sacred" (spec §3) is not
// suspended by a replacement, it is scheduled by it: the file being replaced
// is what the clip still serves until the file replacing it is ready, so the
// only safe moment to delete it is after the row stopped pointing at it.
//
// The working directory is the handler's spool, $SCRATCH_DIR/.upload-{id}-{r8},
// never $SCRATCH_DIR/{id} (§20 erratum 136): the clip's own directory belongs
// to whatever run is registered for it, and a trim keeps the CURRENT original
// there under the same `source.{ext}` name.

// replaceRun is the state of one replacement attempt.
type replaceRun struct {
	w    *worker
	ctx  context.Context
	run  *run
	job  db.Job
	clip db.Clip
	log  *slog.Logger

	params db.ReplaceParams
	dir    string
	ext    string
	r8     string

	srcPath, outPath, posterPath string
	renditionKey, posterKey      string
	// The quality tiers, exactly as the transcode pipeline carries them (§20
	// errata 200-206): a replacement IS a first upload of a file that happens to
	// keep somebody else's id, so it reaches the same verdict through the same
	// function and produces the same tiers in the same order - card first.
	tiers               transcode.TierPlan
	lightPath, lightKey string
	lightOut            *transcode.OutputInfo
	posterSrc           string
	posterDurUS         int64
	posterHasDur        bool
	posterVideoEnd      float64
	posterBox           transcode.PosterBox
	// attempted lists the keys this attempt is responsible for: the two it
	// writes here, plus the replacement ORIGINAL the handler uploaded, which
	// nothing references until the commit. A run that ends any other way
	// removes all three (§11).
	attempted []string

	stage    string
	percent  int
	plan     transcode.Plan
	srcProbe *transcode.ProbeResult
	out      *transcode.OutputInfo
	ready    db.ReadyFields

	panicOnce sync.Once
}

func newReplaceRun(w *worker, ctx context.Context, r *run, job db.Job, clip db.Clip) *replaceRun {
	p := &replaceRun{w: w, ctx: ctx, run: r, job: job, clip: clip}
	p.log = w.log.With("clip", clip.ID, "job", job.ID)
	p.r8 = ids.Rand(8)
	pfx := w.deps.Cfg.S3.KeyPrefix
	p.renditionKey = store.RenditionKey(pfx, clip.ID, p.r8)
	p.lightKey = store.SocialKey(pfx, clip.ID, p.r8)
	p.posterKey = store.PosterKey(pfx, clip.ID, p.r8)
	return p
}

// setPaths resolves the scratch layout from the job's params. It runs after
// they are parsed, because the spool directory and the source extension are
// both facts of THIS attempt's upload rather than of the clip.
func (p *replaceRun) setPaths() error {
	if !scratch.ValidTmpName(p.params.Spool) {
		return &transcode.ErrPermanent{Err: fmt.Errorf("replace params name a spool that is not one: %q", p.params.Spool)}
	}
	p.ext = strings.TrimPrefix(path.Ext(p.params.SourceKey), ".")
	if p.ext == "" {
		p.ext = store.DefaultExt
	}
	p.dir = filepath.Join(p.w.deps.Cfg.ScratchDir, p.params.Spool)
	p.srcPath = filepath.Join(p.dir, "source."+p.ext)
	p.outPath = filepath.Join(p.dir, "main-"+p.r8+".mp4")
	p.lightPath = filepath.Join(p.dir, "light-"+p.r8+".mp4")
	p.posterPath = filepath.Join(p.dir, "poster-"+p.r8+".jpg")
	return nil
}

// execute runs one attempt under recover() and settles its outcome.
func (p *replaceRun) execute() {
	p.log.Info("replace started", "attempt", p.job.Attempts+1, "transient_failures", p.job.TransientFailures)
	err := p.guarded()
	defer func() {
		if r := recover(); r != nil {
			p.log.Error("replace exit path panicked", "stage", p.stage, "err", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
	}()
	p.finish(err)
}

func (p *replaceRun) guarded() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &panicError{value: r, stack: debug.Stack()}
		}
	}()
	return p.pipeline()
}

func (p *replaceRun) pipeline() error {
	params, err := db.ParseReplaceParams(p.job.ParamsJSON)
	if err != nil {
		// The row cannot describe a replacement at all: no retry can fix it.
		return &transcode.ErrPermanent{Err: err}
	}
	p.params = params
	if err := p.setPaths(); err != nil {
		return err
	}
	// The uploaded original is this attempt's to clean up from here on: it is
	// referenced by nothing until the commit, and every ending but the commit
	// must remove it.
	p.attempted = append(p.attempted, p.params.SourceKey)
	if err := p.recheckClip(); err != nil {
		return err
	}
	if err := p.step(stageAcquire, p.acquire); err != nil {
		return err
	}
	if err := p.step(stageProbe, p.probeSource); err != nil {
		return err
	}
	if err := p.produceVerifyPoster(); err != nil {
		return err
	}
	if err := p.step(stageStore, p.store); err != nil {
		return err
	}
	return p.swap()
}

func (p *replaceRun) step(stage string, fn func() error) error {
	p.setStage(stage)
	return fn()
}

// produceVerifyPoster runs every ffmpeg stage holding ONE of the
// TRANSCODE_CONCURRENCY slots (§20 erratum 79), exactly as a trim does: the
// slot is taken here rather than around the whole run because a replacement
// spends most of its life moving bytes, and it is held through `verify` and
// `poster` because those read and write the very files the encode produced.
//
// WHICH stages run is the tier plan's decision, and it is the transcode
// pipeline's decision reached by the same function (§20 erratum 206): a playable
// new original is remuxed or served as it stands, an unplayable one gets a Full
// - or, when a Light copy is wanted, gets the LIGHT copy now and the Full as a
// job of its own, so the card is right the moment the swap commits.
func (p *replaceRun) produceVerifyPoster() error {
	release, err := p.w.acquireFFmpeg(p.ctx, p.log)
	if err != nil {
		return err
	}
	defer release()
	if p.tiers.TooLarge {
		return &transcode.ErrPermanent{Err: fmt.Errorf("source_too_large: %s (%dx%d)",
			p.tiers.Reason, p.srcProbe.DisplayW, p.srcProbe.DisplayH)}
	}
	switch {
	case p.tiers.FullNeeded && p.tiers.LightNeeded:
		// BOTH TIERS, ONE DECODE (§20 erratum 217) - the transcode pipeline's own
		// branch, for the same measured reason: a replacement of an unplayable
		// 1440p60 file is the same encode with somebody else's clip id on it.
		if err := p.step(stageEncode, p.encodeTiers); err != nil {
			return err
		}
		if err := p.step(stageVerify, p.verifyTiers); err != nil {
			return err
		}
	case p.tiers.FullNeeded:
		if err := p.step(stageEncode, p.encode); err != nil {
			return err
		}
		if err := p.step(stageVerify, p.verifyFull); err != nil {
			return err
		}
	case p.tiers.Remux:
		if err := p.step(stageRemux, p.remux); err != nil {
			return err
		}
		if err := p.step(stageVerify, p.verifyRemux); err != nil {
			return err
		}
		if p.tiers.LightNeeded {
			if err := p.step(stageLight, p.light); err != nil {
				return err
			}
		}
	default:
		p.measureFromSource()
		p.posterSrc, p.posterDurUS, p.posterHasDur, p.posterVideoEnd =
			p.srcPath, p.srcProbe.DurationUS, p.srcProbe.HasDuration, p.srcProbe.Video.EndSec
		if p.tiers.LightNeeded {
			if err := p.step(stageLight, p.light); err != nil {
				return err
			}
		}
	}
	return p.step(stagePoster, p.poster)
}

// mainProduced reports whether this attempt wrote and MEASURED a rendition
// object - a Full or a remux (pipeline.go's jobRun.mainProduced, same rule).
func (p *replaceRun) mainProduced() bool { return p.out != nil }

// measure and measureFromSource are the transcode pipeline's, applied to the NEW
// original: a clip's five measurements are the best playable tier's, which on
// the remux and direct paths is the upload itself - its DISPLAY dims, so a phone
// capture is 1080x1920 rather than the coded 1920x1080.
func (p *replaceRun) measure(w, h int, durMS int64, fps transcode.Rational, hasAudio bool) {
	p.ready.Width, p.ready.Height = w, h
	p.ready.DurationMS = durMS
	p.ready.FPSNum, p.ready.FPSDen = fps.Num, fps.Den
	p.ready.HasAudio = hasAudio
}

func (p *replaceRun) measureFromSource() {
	pr := p.srcProbe
	durMS := int64(0)
	if pr.HasDuration && pr.DurationUS > 0 {
		durMS = int64(math.Round(float64(pr.DurationUS) / 1000))
	}
	p.measure(pr.DisplayW, pr.DisplayH, durMS, pr.FPS, len(pr.AudioIndexes) > 0)
}

// remux rewrites a playable new original's container with `-c copy`.
func (p *replaceRun) remux() error {
	if err := requireScratch(p.w, p.dir, satAdd(p.params.SourceSize, p.w.deps.Cfg.ScratchMinFree)); err != nil {
		return err
	}
	if err := p.w.runner.Remux(p.ctx, p.srcProbe, p.srcPath, p.outPath); err != nil {
		return err
	}
	p.progress(1, false)
	return nil
}

// light encodes the card's copy, from the produced rendition when there is one
// and from the new original otherwise - pipeline.go's light(), same argument.
func (p *replaceRun) light() error {
	in := p.srcPath
	if p.mainProduced() {
		in = p.outPath
	}
	if err := checkOutputRoom(p.w, p.dir, p.tiers.Light); err != nil {
		return err
	}
	tag, err := p.lightTag()
	if err != nil {
		return err
	}
	if err := p.w.runner.EncodeSocial(p.ctx, p.tiers.Light, in, p.lightPath, tag, p.lightBudget(), p.safeProgress); err != nil {
		return err
	}
	return p.measureLight()
}

// verifyTiers measures both files of the two-output pass: the Full first, so the
// clip's own five measurements are the faithful copy's, then the card's copy —
// which leaves `posterSrc` on the FULL, an untagged file (§20 erratum 219), and
// takes only the card's BOX from it so the card image is still card-sized.
func (p *replaceRun) verifyTiers() error {
	if err := p.verifyFull(); err != nil {
		return err
	}
	return p.measureLight()
}

// encodeTiers is the two-output pass (§20 erratum 217), pipeline.go's own.
func (p *replaceRun) encodeTiers() error {
	if err := checkOutputRoomFor(p.w, p.dir, p.tiers.Full, p.tiers.Light); err != nil {
		return err
	}
	tag, err := p.lightTag()
	if err != nil {
		return err
	}
	if err := p.w.runner.EncodeTiers(p.ctx, p.tiers.Full, p.tiers.Light, p.srcPath, p.outPath, p.lightPath,
		tag, satAdd(p.fullBudget(), p.lightBudget()), p.safeProgress); err != nil {
		return err
	}
	// Each file against its OWN budget; the runtime cap could only be the pair's.
	st, err := os.Stat(p.outPath)
	if err != nil || st.Size() <= p.fullBudget() {
		return nil
	}
	return &transcode.ErrPermanent{Err: fmt.Errorf(
		"output_too_large: the replacement rendition is %d bytes, past the %d budgeted for it", st.Size(), p.fullBudget())}
}

func (p *replaceRun) fullBudget() int64 {
	return outputBudget(p.params.SourceSize, p.w.deps.Cfg.MaxUploadBytes, p.tiers.Full)
}

func (p *replaceRun) lightBudget() int64 {
	return outputBudget(p.params.SourceSize, p.w.deps.Cfg.MaxUploadBytes, p.tiers.Light)
}

// lightTag is the corner line for this replacement's card copy (§20 erratum 214):
// the copy's own quality and the best tier's, both from the NEW file's plans.
func (p *replaceRun) lightTag() (transcode.LightTag, error) {
	light := transcode.QualityShort(p.tiers.Light.W, p.tiers.Light.H, p.tiers.Light.FPS)
	best := transcode.QualityShort(p.srcProbe.DisplayW, p.srcProbe.DisplayH, p.srcProbe.FPS)
	if p.tiers.FullNeeded {
		best = transcode.QualityShort(p.tiers.Full.W, p.tiers.Full.H, p.tiers.Full.FPS)
	}
	return p.w.runner.LightTagFor(p.dir, "tag-"+p.r8+".txt", light, best)
}

// measureLight measures the card's copy and records it on the commit
// (pipeline.go's own, same three checks and the same poster rule).
func (p *replaceRun) measureLight() error {
	budget := p.lightBudget()
	info, err := p.w.runner.ProbeOutput(p.ctx, p.lightPath)
	if err != nil {
		return err
	}
	tc := p.w.deps.Cfg.Transcode
	if err := transcode.CheckOutput(info, transcode.PlanOptions{MaxW: tc.MaxW, MaxH: tc.MaxH, MaxFPS: tc.MaxFPS}); err != nil {
		return err
	}
	if err := p.w.runner.Verify(p.lightPath); err != nil {
		return err
	}
	st, err := os.Stat(p.lightPath)
	if err != nil {
		return &transcode.ErrPermanent{Err: fmt.Errorf("bad_output: the light copy is unreadable: %w", err)}
	}
	if budget > 0 && st.Size() > budget {
		return &transcode.ErrPermanent{Err: fmt.Errorf(
			"output_too_large: the light copy is %d bytes, past the %d budgeted for it", st.Size(), budget)}
	}
	p.lightOut = info
	p.ready.Light = db.SocialFields{Key: p.lightKey, Size: st.Size(),
		Width: info.Width, Height: info.Height, FPSNum: info.FPS.Num, FPSDen: info.FPS.Den}
	// The card image is not cut from the tagged file, and takes its size from it
	// instead — pipeline.go's measureLight, same erratum 219.
	p.posterBox = transcode.PosterBox{W: p.tiers.Light.W, H: p.tiers.Light.H}
	p.log.Info("replacement light copy encoded", "bytes", st.Size(),
		"dims", fmt.Sprintf("%dx%d", info.Width, info.Height), "fps", info.FPS.String())
	return nil
}

// tierFields is what the swap says about the tiers, and it is jobRun.tierFields
// with the NEW original's probe in place of the first upload's.
func (p *replaceRun) tierFields() db.TierFields {
	pr := p.srcProbe
	f := db.TierFields{
		OriginalPlayable: p.tiers.Playable,
		OriginalRemuxed:  p.tiers.Remux,
		OriginalWidth:    pr.DisplayW, OriginalHeight: pr.DisplayH,
		OriginalFPSNum: pr.FPS.Num, OriginalFPSDen: pr.FPS.Den,
		OriginalCodec: pr.VideoCodec,
		FullState:     db.TierSkipped,
		LightState:    db.TierSkipped,
	}
	if p.ready.Light.Key != "" {
		f.LightState = db.TierReady
	}
	// The second arm is pipeline.go tierFields' arm: retained but unreached
	// since errata 217/235, because both tiers now publish at the same flip.
	switch {
	case p.tiers.FullNeeded && p.mainProduced():
		f.FullState, f.FullCodec = db.TierReady, transcode.NormalizeFullCodec(p.w.deps.Cfg.Transcode.FullCodec)
	case p.tiers.FullNeeded:
		f.FullState, f.QueueFull = db.TierEncoding, true
	}
	return f
}

// playSize is the size of the file a card would fetch if no Light copy existed
// (pipeline.go's playSize, applied to the replacement's own original).
func (p *replaceRun) playSize() int64 {
	if p.mainProduced() {
		return p.ready.RenditionSize
	}
	if p.tiers.Playable && !p.tiers.Remux {
		return p.params.SourceSize
	}
	return 0
}

// recheckClip closes the claim→registration window: a replacement only ever
// runs against a clip that is STILL ready.
func (p *replaceRun) recheckClip() error {
	c, err := p.w.deps.DB.GetClip(p.ctx, p.clip.ID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return skip("replace", "clip deleted")
	case err != nil:
		return fmt.Errorf("re-read clip: %w", err)
	case c.State == db.ClipDeleting:
		return skip("replace", "clip is being deleted")
	case c.State != db.ClipReady:
		return skip("replace", "clip is "+c.State)
	}
	p.clip = c
	return nil
}

// acquire finds the NEW original: the handler's spool if it survived, else a
// download of the object the handler stored. `acquireSource` takes a clip and
// reads only its source triple, so the replacement is described to it as the
// clip it is about to become — the one place in this package where those two
// differ, and the reason the synthetic value is built here rather than passed
// around.
func (p *replaceRun) acquire() error {
	return acquireSource(p.ctx, p.w, p.sourceClip(), p.dir, p.srcPath, p.log, p.transferCtx, p.safeProgress)
}

// sourceClip is the clip as it will be AFTER the commit, for the two fields
// acquireSource reads.
func (p *replaceRun) sourceClip() db.Clip {
	c := p.clip
	c.SourceKey, c.SourceSize = p.params.SourceKey, p.params.SourceSize
	return c
}

// probeSource probes the replacement original and plans it exactly as a
// transcode plans a first upload: a replacement is a first upload, of a file
// that happens to keep somebody else's id.
func (p *replaceRun) probeSource() error {
	res, err := p.w.runner.Probe(p.ctx, p.srcPath)
	if err != nil {
		return err
	}
	p.srcProbe = res
	p.ready.SourceDurationMS = sourceDurationMS(res)
	p.tiers = tierPlanFor(res, p.sourceReadyClip(), p.w.deps.Cfg, transcode.FaststartMP4(p.srcPath))
	// The plan of the encode THIS RUN performs, in the transcode pipeline's own
	// order (§20 erratum 217): the FULL one whenever a Full is wanted, because a
	// run that wants both tiers writes them in ONE pass whose output verifyFull
	// then measures against this plan. Taking the Light plan here made that check
	// compare a 1080p60 Full against a 720p30 box.
	switch {
	case p.tiers.FullNeeded:
		p.plan = p.tiers.Full
	case p.tiers.LightNeeded:
		p.plan = p.tiers.Light
	}
	p.log.Info("replace planned", "display", fmt.Sprintf("%dx%d", res.DisplayW, res.DisplayH), "rotation", res.Rotation,
		"original_playable", p.tiers.Playable, "remux", p.tiers.Remux, "why_not", p.tiers.Reason,
		"full", p.tiers.FullNeeded, "light", p.tiers.LightNeeded, "card_tier", p.tiers.CardTier,
		"out", fmt.Sprintf("%dx%d", p.plan.W, p.plan.H), "fps", p.plan.FPS.String(), "audio", p.plan.AudioMode.String(),
		"duration_us", res.DurationUS, "has_duration", res.HasDuration)
	return nil
}

// sourceReadyClip is the clip as the TIER PLAN must see it: the new original's
// size and media type, which are two of the three facts the playability table
// needs and ffprobe does not report. sourceClip above is its sibling for
// acquireSource, which reads the key and the size.
func (p *replaceRun) sourceReadyClip() db.Clip {
	c := p.sourceClip()
	c.SourceContentType = p.params.SourceContentType
	return c
}

func (p *replaceRun) encode() error {
	if err := checkOutputRoom(p.w, p.dir, p.plan); err != nil {
		return err
	}
	budget := outputBudget(p.params.SourceSize, p.w.deps.Cfg.MaxUploadBytes, p.plan)
	if err := p.w.runner.EncodeFull(p.ctx, p.plan, p.srcPath, p.outPath, budget, p.safeProgress); err != nil {
		return err
	}
	st, err := os.Stat(p.outPath)
	if err != nil || budget <= 0 || st.Size() <= budget {
		return nil
	}
	return &transcode.ErrPermanent{Err: fmt.Errorf(
		"output_too_large: the replacement rendition is %d bytes, past the %d budgeted for it", st.Size(), budget)}
}

// verifyFull measures the FULL tier against the plan EXACTLY - this tier has no
// box - and verifyRemux measures the container rewrite against the source's
// CODED frame, allowing an odd size it did not choose (pipeline.go's pair, the
// same two reasons).
func (p *replaceRun) verifyFull() error {
	info, err := p.w.runner.ProbeOutput(p.ctx, p.outPath)
	if err != nil {
		return err
	}
	if err := transcode.CheckTierOutput(info, transcode.TierCheck{
		Codecs: []string{transcode.NormalizeFullCodec(p.w.deps.Cfg.Transcode.FullCodec)},
		Width:  p.plan.W, Height: p.plan.H,
	}); err != nil {
		return err
	}
	if err := p.w.runner.Verify(p.outPath); err != nil {
		return err
	}
	p.out = info
	p.measure(info.Width, info.Height, info.DurationMS, info.FPS, info.HasAudio)
	p.posterSrc, p.posterDurUS, p.posterHasDur, p.posterVideoEnd =
		p.outPath, info.DurationMS*1000, info.DurationMS > 0, info.VideoEndSec
	return nil
}

func (p *replaceRun) verifyRemux() error {
	info, err := p.w.runner.ProbeOutput(p.ctx, p.outPath)
	if err != nil {
		return err
	}
	if err := transcode.CheckTierOutput(info, transcode.TierCheck{
		Codecs: []string{transcode.FullCodecH264, transcode.FullCodecAV1},
		Width:  p.srcProbe.CodedW, Height: p.srcProbe.CodedH, AllowOdd: true,
	}); err != nil {
		return err
	}
	if err := p.w.runner.Verify(p.outPath); err != nil {
		return err
	}
	p.out = info
	p.measureFromSource()
	p.posterSrc, p.posterDurUS, p.posterHasDur, p.posterVideoEnd =
		p.outPath, info.DurationMS*1000, info.DurationMS > 0, info.VideoEndSec
	return nil
}

func (p *replaceRun) poster() error {
	res, err := p.w.runner.Poster(p.ctx, p.posterSrc, p.posterPath,
		p.posterDurUS, p.posterHasDur, p.posterVideoEnd, p.posterBox)
	if err != nil {
		return err
	}
	if res.NearBlack {
		p.log.Info("the replacement's poster is near-black; no lit frame was found", "seconds", res.Seconds)
	}
	p.ready.PosterWidth, p.ready.PosterHeight = res.Width, res.Height
	return nil
}

// store uploads the new rendition and poster. meta/probe.json is NOT
// rewritten, for the reason a trim does not rewrite it (§20 erratum 70): it
// is a fixed key, so it cannot be written per attempt, and an attempt that
// never commits must publish nothing. It stays the record of how this clip
// was FIRST encoded.
func (p *replaceRun) store() error {
	if p.mainProduced() {
		res, err := p.put(p.renditionKey, p.outPath, "video/mp4", func(done, total int64) {
			if total > 0 {
				p.safeProgress(storeShareRendition*float64(done)/float64(total), false)
			}
		})
		if err != nil {
			return fmt.Errorf("upload rendition: %w", err)
		}
		p.ready.RenditionKey, p.ready.RenditionSize, p.ready.RenditionETag = p.renditionKey, res.Size, res.SHA256
	}
	if p.ready.Light.Key != "" {
		res, err := p.put(p.lightKey, p.lightPath, "video/mp4", nil)
		if err != nil {
			return fmt.Errorf("upload light copy: %w", err)
		}
		p.ready.Light.Size, p.ready.Light.ETag = res.Size, res.SHA256
	}
	p.progress(storeShareRendition, false)
	res, err := p.put(p.posterKey, p.posterPath, "image/jpeg", nil)
	if err != nil {
		return fmt.Errorf("upload poster: %w", err)
	}
	p.ready.PosterKey, p.ready.PosterSize, p.ready.PosterETag = p.posterKey, res.Size, res.SHA256
	p.progress(1, false)
	return nil
}

func (p *replaceRun) put(key, filePath, ct string, progress func(done, total int64)) (store.PutResult, error) {
	p.attempted = append(p.attempted, key)
	ctx, cancel := p.transferCtx()
	defer cancel()
	return p.w.deps.Store.PutFile(ctx, key, filePath, ct, progress)
}

func (p *replaceRun) transferCtx() (context.Context, context.CancelFunc) {
	if m := p.w.deps.Cfg.S3.TransferMax; m > 0 {
		return context.WithTimeout(p.ctx, m)
	}
	return context.WithCancel(p.ctx)
}

// swap is the commit: the conditional one-UPDATE source swap, with the same
// bounded retries the transcode's finalize gets. ok=false is the §11 lost
// race — the clip stopped being ready, or the job stopped being running — and
// this attempt then removes everything it uploaded, the replacement original
// included, because nothing references any of it.
func (p *replaceRun) swap() error {
	p.setStage(stageFinalize)
	p.ready.QueueSocial = lightWanted(p.ready, p.playSize(), p.w.deps.Cfg)
	p.ready.Tiers = p.tierFields()
	if p.ready.QueueSocial {
		p.ready.Tiers.LightState = db.TierEncoding
	}
	src := db.NewSource{
		Key: p.params.SourceKey, Filename: p.params.SourceFilename, Size: p.params.SourceSize,
		ContentType: p.params.SourceContentType, ETag: p.params.SourceETag,
	}
	var superseded []string
	var ok bool
	var err error
	for attempt := 0; ; attempt++ {
		superseded, ok, err = p.w.deps.DB.SwapSource(p.ctx, p.job.ID, p.clip.ID, p.ready, src, p.w.now().Unix())
		if err == nil || p.ctx.Err() != nil || attempt >= len(finalizeBackoff) {
			break
		}
		p.log.Warn("replace swap failed; retrying", "attempt", attempt+1, "err", err)
		select {
		case <-p.ctx.Done():
		case <-time.After(finalizeBackoff[attempt]):
		}
	}
	if err != nil {
		if p.ctx.Err() == nil {
			p.log.Error("replace swap gave up", "err", err)
		}
		return fmt.Errorf("swap: %w", err)
	}
	if !ok {
		p.lostRace()
		return errLostRace
	}
	// From here the clip references the new objects, so the OLD ones are what
	// must go — the previous original among them, and only now. Best effort,
	// never retried, and never at the cost of the outcome (§20 erratum 70's
	// known orphan class).
	ctx, cancel := p.exitCtx()
	deleteReplacedKeys(ctx, p.w.deps.Store, p.log, superseded, p.prefix(), p.params.SourceKey)
	cancel()
	p.removeScratch(true)
	p.w.trimProg.forget(p.clip.ID)
	p.log.Info("replace applied", "duration_ms", p.ready.DurationMS,
		"dims", fmt.Sprintf("%dx%d", p.ready.Width, p.ready.Height),
		"source_key", p.params.SourceKey, "source_bytes", p.params.SourceSize,
		"rendition_key", p.ready.RenditionKey, "superseded", len(superseded))
	return nil
}

// lostRace: the clip stopped being ready during the run. Remove every key this
// attempt owns and mark the job cancelled (INFO; an expected outcome).
func (p *replaceRun) lostRace() {
	ctx, cancel := p.exitCtx()
	defer cancel()
	switch err := p.w.deps.DB.CancelJob(ctx, p.job.ID, db.ClaimStamp(p.job), p.w.now().Unix()); {
	case err == nil:
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		// The row already ended under another hand (§20 erratum 291).
		p.log.Info("replace job already ended under another hand; nothing to cancel", "err", err)
	case errors.Is(err, context.DeadlineExceeded):
		p.log.Warn("cancel replace job after lost race: DB slow; boot reconciliation will repair the row", "err", err)
	default:
		p.log.Error("cancel replace job after lost race", "err", err)
	}
	p.dropAttempt(ctx, p.attempted)
	p.removeScratch(true)
	p.w.trimProg.forget(p.clip.ID)
	p.log.Info("lost_race: the clip stopped being ready during the replacement; outputs removed")
}

// finish settles the attempt (§11 classification) with the media kinds' rule:
// whatever happened, the CLIP is untouched — it keeps the original, the
// rendition and the poster it had.
func (p *replaceRun) finish(err error) {
	var pe *panicError
	switch {
	case err == nil, errors.Is(err, errLostRace):
		return
	case errors.Is(err, errSkipped):
		p.complete(db.JobStageSkipped, err.Error())
		p.cleanupAttempt(true)
		p.log.Info("replace skipped", "reason", err.Error())
		return
	case errors.As(err, &pe):
		p.log.Error("replace job panicked", "stage", p.stage, "err", pe.Error(), "cancelled", p.ctx.Err() != nil, "stack", string(pe.stack))
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

// cleanupAttempt removes this attempt's objects and, when the job is over for
// good, its scratch directory. A skipped or failed replacement must not leave
// the uploaded original in the bucket: nothing references it, and `admin
// orphans` cannot see an extra object inside a live clip's prefix.
func (p *replaceRun) cleanupAttempt(final bool) {
	ctx, cancel := p.exitCtx()
	defer cancel()
	p.dropAttempt(ctx, p.attempted)
	p.removeScratch(final)
	p.w.trimProg.forget(p.clip.ID)
}

// noRoom is the §20 erratum 90 ending: requeued with a backoff, both counters
// untouched, this attempt's ENCODE outputs removed and the uploaded original
// kept — the retry will need it, and re-downloading it is the only other way
// to get it back.
func (p *replaceRun) noRoom(err error) {
	ctx, cancel := p.exitCtx()
	defer cancel()
	if lost := requeueNoCount(ctx, p.w, p.log, p.job, p.clip.ID, p.stage, msgReplaceDisk, p.reason(err)); lost != nil {
		p.lostAttempt(lost, p.reason(err))
		return
	}
	p.dropAttempt(ctx, p.outputKeys())
	p.removeScratch(false)
	p.w.trimProg.forget(p.clip.ID)
}

// lostAttempt is the §20 erratum 293 ending, this kind's copy of the one
// erratum 291 gave the transcode: the row stopped being this attempt's — a
// repair took it and a NEWER attempt may hold it, or MarkDeleting cancelled it
// with the clip — so the row is NOT touched (an attempt recorded there spends
// the current owner's counter) and this attempt's ENCODE outputs are
// discarded. The uploaded ORIGINAL stays, exactly as it does on the requeue
// above and for the same reason: the job's params name it, so whichever
// attempt runs the row next reads that file and re-downloading it is the only
// other way to get it back. An attempt that truly ends for good — the row's
// own owner failing at its cap — is what takes it, not this one.
func (p *replaceRun) lostAttempt(err error, detail string) {
	ctx, cancel := p.exitCtx()
	defer cancel()
	p.log.Info("replace job row is no longer this attempt's; outputs discarded, row untouched",
		"stage", p.stage, "err", err, "detail", detail)
	p.dropAttempt(ctx, p.outputKeys())
	p.removeScratch(false)
	p.w.trimProg.forget(p.clip.ID)
}

// outputKeys is `attempted` without the uploaded original: what a retryable
// ending removes, because the source is what the retry starts from.
func (p *replaceRun) outputKeys() []string {
	keys := make([]string, 0, len(p.attempted))
	for _, k := range p.attempted {
		if k != p.params.SourceKey {
			keys = append(keys, k)
		}
	}
	return keys
}

// prefix is the clip's own key prefix: the second interlock on every delete
// this run makes (deleteReplacedKeys).
func (p *replaceRun) prefix() string {
	return store.ClipPrefix(p.w.deps.Cfg.S3.KeyPrefix, p.clip.ID)
}

// dropAttempt removes objects this attempt owns, with the clip's CURRENT
// source as the key that may never be deleted. Every ending but the commit
// goes through it, which is what stops a replacement that did not land from
// taking the upload the clip still is with it.
func (p *replaceRun) dropAttempt(ctx context.Context, keys []string) {
	deleteReplacedKeys(ctx, p.w.deps.Store, p.log, keys, p.prefix(), p.clip.SourceKey)
}

// complete resolves a job that must not run as `done` with the reason in
// last_error (CompleteJob refuses a transcode job, which is the point).
func (p *replaceRun) complete(stage, note string) {
	ctx, cancel := p.exitCtx()
	defer cancel()
	err := p.w.deps.DB.CompleteJob(ctx, p.job.ID, db.ClaimStamp(p.job), stage, note, p.w.now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		p.lostAttempt(err, note)
	default:
		p.log.Error("complete replace job", "note", note, "err", err)
	}
}

// reason renders the LOGGED failure detail, paths and bucket coordinates
// redacted. It is not what the owner sees — ownerReason is.
func (p *replaceRun) reason(err error) string {
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

func (p *replaceRun) exitCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(p.ctx), exitBudget)
}

// release is the Cancel/Stop path: the job goes back to queued with both
// counters untouched and this attempt's ENCODE outputs removed. The uploaded
// original stays — it is what the requeued attempt reads.
func (p *replaceRun) release() {
	why := p.run.reason()
	if why == reasonNone {
		why = reasonShutdown
	}
	ctx, cancel := p.exitCtx()
	err := p.w.deps.DB.ReleaseJob(ctx, p.job.ID, db.ClaimStamp(p.job), p.clip.ID, p.w.now().Unix())
	cancel()
	switch {
	case err == nil:
		p.log.Info("replace job released", "reason", why.String(), "stage", p.stage)
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		p.lostAttempt(err, why.String())
		return
	default:
		p.log.Error("release replace job", "reason", why.String(), "err", err)
	}
	dctx, dcancel := p.exitCtx()
	p.dropAttempt(dctx, p.outputKeys())
	dcancel()
	p.removeScratch(false)
	p.w.trimProg.forget(p.clip.ID)
}

// fail records one bad ending. The clip is never marked: db.FailAttempt
// couples a clip only for `transcode`, and a replacement's crash cap is
// pinned to db.ReplaceMaxAttempts there.
//
// Two texts come out of one error (§20 erratum 80's split): `owner` — a short
// fixed sentence — is what goes in the row and reaches the owner through GET
// …/replace, while `detail`, which may be an ffmpeg stderr tail, stays in the
// log where an operator can read it and a visitor cannot.
func (p *replaceRun) fail(kind db.FailKind, cause error) {
	detail, owner := p.reason(cause), p.ownerReason(cause)
	ctx, cancel := p.exitCtx()
	defer cancel()
	outcome, err := p.w.deps.DB.FailAttempt(ctx, p.job.ID, db.ClaimStamp(p.job), p.clip.ID, owner, kind, p.w.lim, p.w.now().Unix())
	defer p.w.trimProg.forget(p.clip.ID)
	switch {
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		// The row is not this attempt's to fail (§20 erratum 293): the clip is
		// being deleted and lifecycle removes the whole prefix, or a repair
		// took the row and a newer attempt may hold it. This attempt's ENCODE
		// outputs are ours to clean up either way, and the uploaded original is
		// the next attempt's to read (lostAttempt), which is why this ending no
		// longer takes `attempted` whole.
		p.lostAttempt(err, detail)
		return
	case err != nil:
		p.log.Error("fail replace attempt", "kind", kindName(kind), "detail", detail, "err", err)
		p.dropAttempt(ctx, p.attempted)
		p.removeScratch(false)
		return
	}
	if outcome == db.OutcomeFailed {
		// Terminal: the uploaded original goes too. Nothing will ever read it
		// again, and it is invisible to `admin orphans` where it sits.
		p.dropAttempt(ctx, p.attempted)
		p.removeScratch(true)
		switch kind {
		case db.FailPermanent:
			// The FILE the owner chose is what this is about: INFO, because
			// nothing here is broken — the clip still plays the file it had.
			p.log.Info("replace failed; the clip keeps the file it had", "reason", owner, "detail", detail)
		default:
			p.log.Warn("replace failed: retry cap reached; the clip keeps the file it had", "kind", kindName(kind),
				"reason", owner, "detail", detail, "cap", db.Cap(db.KindReplace, kind, p.w.lim))
		}
		return
	}
	p.dropAttempt(ctx, p.outputKeys())
	p.removeScratch(false)
	p.log.Warn("replace attempt failed; requeued", "kind", kindName(kind), "reason", owner, "detail", detail)
}

// The owner-facing failure vocabulary for a replacement. Short, plain, fixed
// and complete, for the reason the trim's is (§20 erratum 80): the owner reads
// one of these, and nothing an ffmpeg build happens to print on stderr — nor a
// path, a byte count or a pointer — belongs in an API answer.
const (
	msgReplaceMedia     = "the file you uploaded could not be transcoded"
	msgReplaceTooLarge  = "the replacement came out larger than this server allows"
	msgReplaceBadOutput = "the replacement did not produce a playable clip"
	msgReplaceDisk      = "the server had no room to transcode the replacement; try again later"
	msgReplaceStorage   = "storage was unavailable while the replacement was transcoded"
	msgReplaceInternal  = "the replacement failed"
)

// ownerReason maps one failure onto exactly one of those sentences, on the
// markers that are ours.
func (p *replaceRun) ownerReason(err error) string {
	var pe *panicError
	switch {
	case err == nil:
		return msgReplaceInternal
	case errors.As(err, &pe):
		return msgReplaceInternal
	}
	detail := err.Error()
	switch {
	case strings.Contains(detail, "output_too_large"):
		return msgReplaceTooLarge
	case strings.Contains(detail, msgNoScratchRoom):
		return msgReplaceDisk
	case strings.Contains(detail, "bad_output"):
		return msgReplaceBadOutput
	}
	switch p.stage {
	case stageAcquire, stageStore:
		return msgReplaceStorage
	case stageProbe, stageEncode, stageVerify, stagePoster:
		return msgReplaceMedia
	}
	return msgReplaceInternal
}

// removeScratch removes the whole spool dir (a committed swap, a permanent
// failure) or only this attempt's outputs (the uploaded source stays for the
// retry).
func (p *replaceRun) removeScratch(all bool) {
	if p.dir == "" {
		return
	}
	if all {
		if err := os.RemoveAll(p.dir); err != nil {
			p.log.Warn("remove replace scratch dir", "dir", p.dir, "err", err)
		}
		return
	}
	for _, f := range []string{p.outPath, p.posterPath} {
		if f == "" {
			continue
		}
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			p.log.Warn("remove replace scratch output", "path", f, "err", err)
		}
	}
}

// --- progress (the §17 #5 edit registry; never the §8.4 one) ---
//
// A replacement writes the same second registry a trim does (§20 erratum 71):
// its subject is the JOB, and the clip it belongs to is `ready` throughout, so
// an encode percentage in the §8.4 registry would make a ready clip poll as
// 40%-processing. At most one run per clip is ever live, so the two kinds can
// never be in it at once.

func (p *replaceRun) setStage(stage string) {
	p.stage = stage
	if lo := bands[stage][0]; lo > p.percent {
		p.percent = lo
	}
	p.setProgress(Progress{State: db.JobRunning, Stage: stage, Percent: p.percent}, true)
	err := p.w.deps.DB.SetJobStage(p.ctx, p.job.ID, db.ClaimStamp(p.job), stage, p.w.now().Unix())
	switch {
	case err == nil, p.ctx.Err() != nil:
	case errors.Is(err, db.ErrNotFound):
		p.log.Info("replace job row gone while setting stage", "stage", stage)
	case errors.Is(err, db.ErrStateMismatch):
		// Repaired or cancelled under another hand (§20 erratum 291): the stage is
		// abandoned, the run goes on, and the lost row ends it at the next write
		// that matters — never as a failure of the row's new owner.
		p.log.Info("replace job row is no longer this attempt's; stage not recorded", "stage", stage)
	default:
		p.log.Error("set replace job stage", "stage", stage, "err", err)
	}
}

func (p *replaceRun) progress(frac float64, indeterminate bool) {
	b := bands[p.stage]
	pct := b[0]
	if !indeterminate {
		frac = math.Min(math.Max(frac, 0), 1)
		pct = int(math.Floor(float64(b[0]) + frac*float64(b[1]-b[0])))
	}
	pct = min(max(pct, 0), 100)
	if pct > p.percent {
		p.percent = pct
	}
	p.setProgress(Progress{State: db.JobRunning, Stage: p.stage, Percent: p.percent, Indeterminate: indeterminate}, false)
}

// safeProgress is the callback handed to the runner and the store; it runs on
// their goroutines, outside this run's recover.
func (p *replaceRun) safeProgress(frac float64, indeterminate bool) {
	defer func() {
		if r := recover(); r != nil {
			p.panicOnce.Do(func() {
				p.log.Error("replace progress callback panicked", "stage", p.stage, "err", fmt.Sprint(r), "stack", string(debug.Stack()))
			})
		}
	}()
	p.progress(frac, indeterminate)
}

// setProgress writes the edit registry under worker.mu, the same lock Forget
// takes, so a voided run resurrects nothing (§20 erratum 49).
func (p *replaceRun) setProgress(pr Progress, force bool) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	if p.run.progressVoid {
		return
	}
	p.w.trimProg.set(p.clip.ID, pr, force)
}
