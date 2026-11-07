package worker

import (
	"context"
	"encoding/json"
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

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/store"
	"clippet/internal/transcode"
)

// Stage vocabulary (§8.4) and percent bands (§10.3): percent =
// clamp(floor(lo + frac·(hi−lo)), 0, 100), monotone within a run.
const (
	stageAcquire  = "acquire"
	stageProbe    = "probe"
	stageEncode   = "encode"
	stageVerify   = "verify"
	stagePoster   = "poster"
	stageStore    = "store"
	stageFinalize = "finalize"
	stageDone     = "done"
	// stageCopy is the trim's stream-copy stage (§17 #5) — `encode`'s twin,
	// on the same band, told apart so the stage alone says which of the two
	// trim paths ran. No transcode ever reports it.
	stageCopy = "copy"
	// stageGIF is the gif job's two palette passes (§17 #6), again on the
	// encode band. It exists so `admin ls` and the job row say which stage a
	// preview died in; nothing polls it.
	stageGIF = "gif"
	// stageFilmstrip is the filmstrip job's one sampling pass (§20 erratum
	// 220), on the encode band for stageGIF's reason exactly: nothing polls it,
	// and it is here so `admin ls` and the job row say which stage a sheet died
	// in.
	stageFilmstrip = "filmstrip"
	// stageRemux is the `-c copy` container rewrite of a playable original (§20
	// erratum 200) — `encode`'s cheapest twin, on the same band, told apart so
	// the stage alone says the remux fast path was taken. It is where a clip
	// that needs no encoding at all spends most of its couple of seconds.
	stageRemux = "remux"
	// stageLight is the LIGHT copy's encode (§20 erratum 203). It is on the
	// encode band with the rest, and since card-first ordering it is the stage a
	// clip whose original the browser cannot play is IN while its `ready` flip
	// waits: the card's own file is what `ready` now means (erratum 206).
	stageLight = "light"
)

var bands = map[string][2]int{
	stageAcquire:   {0, 3},
	stageProbe:     {3, 5},
	stageEncode:    {5, 85},
	stageCopy:      {5, 85},
	stageGIF:       {5, 85},
	stageFilmstrip: {5, 85},
	stageRemux:     {5, 40},
	stageLight:     {40, 85},
	stageVerify:    {85, 87},
	stagePoster:    {87, 90},
	stageStore:     {90, 98},
	stageFinalize:  {98, 100},
	stageDone:      {100, 100},
}

// Budgets for the exit path and hooks.
const (
	exitBudget   = 10 * time.Second // DB + best-effort deletes after a run ends
	hookBudget   = 30 * time.Second
	maxReasonLen = 1000
	// scratchSpaceMul sizes outputBudget's source term (2×source_size). It is
	// NOT an acquire reservation: acquire counts the source ONCE, because by
	// the time it looks the bytes are already on the disk it is measuring
	// (§20 erratum 90).
	scratchSpaceMul = 2
	// storeShareRendition is the part of the store band the rendition
	// upload spans; the poster and probe.json each bump by a fixed step.
	storeShareRendition = 0.9
	storeSharePoster    = 0.95
)

// finalizeBackoff paces the FinalizeTranscode retries (§10.3 step 9).
var finalizeBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// Sentinel outcomes that are not failures.
var (
	errClipGone = errors.New("clip deleted before the run started")
	errLostRace = errors.New("lost race: clip tombstoned during the run")
	// errLostAttempt is the ending of a run whose JOB ROW stopped being its own
	// before the checkpoint (§20 erratum 291): the attempt-pinned SaveCheckpoint
	// answered ErrStateMismatch, so the row was repaired (an operator's unstick,
	// a second process's automatic repair) or cancelled (MarkDeleting) meanwhile,
	// and may already be a newer attempt's. It is not a failure of THIS attempt
	// and it must never be recorded as one: FailAttempt on that row would spend
	// the newer attempt's counter.
	errLostAttempt = errors.New("lost attempt: the job row is no longer this attempt's")
	// errNoScratchRoom marks a refusal for want of free scratch space. It is
	// transient in the ordinary sense — a disk fills and empties again — but
	// it is deliberately NOT one of the outages TRANSCODE_MAX_TRANSIENT
	// budgets (§20 erratum 90): a disk that stays full for the ten backoffs
	// would otherwise fail every queued clip PERMANENTLY, with a public
	// sentence naming the server's free-byte count, for a condition the
	// operator fixes by freeing bytes rather than by anyone re-uploading. The
	// run is requeued with a backoff and both counters untouched instead.
	errNoScratchRoom = errors.New(msgNoScratchRoom)
)

const (
	// msgNoScratchRoom is the WHOLE of the stored reason for that class: a
	// bounded sentence with no numbers in it. The free-byte count and what
	// was being reserved go to the WARN line, which only an operator reads.
	msgNoScratchRoom = "insufficient scratch space"
	// scratchRequeueDelay paces the retry. It is the transient backoff's
	// first step and it never grows, because the counter it would grow with
	// is the one this class deliberately does not touch.
	scratchRequeueDelay = time.Minute
)

// panicError carries a recovered panic to the FailCrash path.
type panicError struct {
	value any
	stack []byte
}

func (p *panicError) Error() string { return fmt.Sprintf("panic: %v", p.value) }

// checkpoint is the §10.3 step-8 JSON: everything finalize needs, so a
// retry after this point is finalize-only.
// Every field is what a commit needs, and the two blocks the quality tiers
// added are there for the same reason as the first: a retry off a checkpoint
// must commit the SAME decision, not re-derive it. The tier plan was decided
// from a probe this attempt no longer holds, and re-deciding it on the retry
// would let a clip whose Light copy is already in the bucket be committed as if
// it had none (§20 erratum 206).
type checkpoint struct {
	RenditionKey  string `json:"rendition_key"`
	RenditionSize int64  `json:"rendition_size"`
	RenditionETag string `json:"rendition_etag"`
	PosterKey     string `json:"poster_key"`
	PosterSize    int64  `json:"poster_size"`
	PosterETag    string `json:"poster_etag"`
	PosterWidth   int    `json:"poster_width"`
	PosterHeight  int    `json:"poster_height"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	DurationMS    int64  `json:"duration_ms"`
	FPSNum        int    `json:"fps_num"`
	FPSDen        int    `json:"fps_den"`
	HasAudio      bool   `json:"has_audio"`

	Light db.SocialFields `json:"light"`
	Tiers db.TierFields   `json:"tiers"`
	// SourceDurationMS rides along because finalize's own skip rule reads the
	// clip's measurements and the ORIGINAL's length is one of them; it was
	// previously re-derived from a probe a finalize-only retry does not have,
	// which left `source_duration_ms` unwritten on such a retry.
	SourceDurationMS *int64 `json:"source_duration_ms,omitempty"`
	// Peaks is an audio clip's waveform (§20 erratum 329), carried so a
	// finalize-only retry commits the same bars this attempt drew.
	Peaks string `json:"peaks,omitempty"`
}

func (c checkpoint) ready() db.ReadyFields {
	return db.ReadyFields{
		RenditionKey: c.RenditionKey, RenditionSize: c.RenditionSize, RenditionETag: c.RenditionETag,
		PosterKey: c.PosterKey, PosterSize: c.PosterSize, PosterETag: c.PosterETag,
		PosterWidth: c.PosterWidth, PosterHeight: c.PosterHeight,
		Width: c.Width, Height: c.Height, DurationMS: c.DurationMS, FPSNum: c.FPSNum, FPSDen: c.FPSDen, HasAudio: c.HasAudio,
		Light: c.Light, Tiers: c.Tiers, SourceDurationMS: c.SourceDurationMS, Peaks: c.Peaks,
	}
}

func checkpointOf(f db.ReadyFields) checkpoint {
	return checkpoint{
		RenditionKey: f.RenditionKey, RenditionSize: f.RenditionSize, RenditionETag: f.RenditionETag,
		PosterKey: f.PosterKey, PosterSize: f.PosterSize, PosterETag: f.PosterETag,
		PosterWidth: f.PosterWidth, PosterHeight: f.PosterHeight,
		Width: f.Width, Height: f.Height, DurationMS: f.DurationMS, FPSNum: f.FPSNum, FPSDen: f.FPSDen, HasAudio: f.HasAudio,
		Light: f.Light, Tiers: f.Tiers, SourceDurationMS: f.SourceDurationMS, Peaks: f.Peaks,
	}
}

// probeDoc is the meta/probe.json object (§6): source ffprobe JSON, the
// plan and the measured output.
type probeDoc struct {
	Source json.RawMessage       `json:"source"`
	Plan   transcode.Plan        `json:"plan"`
	Output *transcode.OutputInfo `json:"output"`
	// Tiers is the tier verdict this run acted on — playability, the reason it
	// was refused when it was, and the two plans (§20 erratum 200). It is the
	// only durable record of WHY a clip did or did not get a Full, which is the
	// first question an operator asks about a tier.
	Tiers *transcode.TierPlan `json:"tiers,omitempty"`
	// Light is the measured LIGHT copy, when this run made one.
	Light *transcode.OutputInfo `json:"light,omitempty"`
	// Audio is an audio clip's tier decision (§20 erratum 328).
	Audio *transcode.AudioPlan `json:"audio,omitempty"`
}

// jobRun is the state of one attempt.
type jobRun struct {
	w    *worker
	ctx  context.Context
	run  *run
	job  db.Job
	clip db.Clip
	log  *slog.Logger

	dir, ext, r8                            string
	srcPath, outPath, posterPath, probePath string
	renditionKey, posterKey, probeKey       string
	// The quality tiers (§20 errata 200–206). `tiers` is the verdict probeSource
	// reached; `lightPath`/`lightKey` are the card copy this run produces when
	// one is planned; `lightOut` is what it measured. `posterSrc` is the file the
	// card image is taken from — always an UNTAGGED one, the best this run has
	// (§20 erratum 219) — with the duration and picture-end of that same file
	// beside it; `posterBox` is the frame the picture is written inside, so the
	// card image is card-sized whatever it was cut from.
	// audio is the tier decision of an AUDIO clip (§20 erratum 328) and
	// renditionCT the media type its Full is stored as; both are zero for a
	// video, whose rendition is always video/mp4.
	audio       transcode.AudioPlan
	renditionCT string
	// peaks is an audio clip's waveform as this run read it (§20 erratum 329):
	// the stencil is drawn from it in produceAudio, the poster in posterAudio.
	peaks               []uint8
	tiers               transcode.TierPlan
	lightPath, lightKey string
	lightOut            *transcode.OutputInfo
	posterSrc           string
	posterDurUS         int64
	posterHasDur        bool
	posterVideoEnd      float64
	posterBox           transcode.PosterBox
	// attempted lists the keys this attempt STARTED writing, recorded
	// before each PutFile: the key whose upload failed or was aborted is
	// exactly the one that can hold an incomplete multipart upload or an
	// unreferenced object, and it is cleaned up with the rest (§11).
	attempted    []string
	checkpointed bool
	finalized    bool
	stage        string
	percent      int
	probe        *transcode.ProbeResult
	plan         transcode.Plan
	out          *transcode.OutputInfo
	ready        db.ReadyFields
	panicOnce    sync.Once
}

func newJobRun(w *worker, ctx context.Context, r *run, job db.Job, clip db.Clip) *jobRun {
	j := &jobRun{w: w, ctx: ctx, run: r, job: job, clip: clip}
	j.log = w.log.With("clip", clip.ID, "job", job.ID)
	j.ext = strings.TrimPrefix(path.Ext(clip.SourceKey), ".")
	if j.ext == "" {
		j.ext = store.DefaultExt
	}
	j.r8 = ids.Rand(8)
	j.dir = filepath.Join(w.deps.Cfg.ScratchDir, clip.ID)
	j.srcPath = filepath.Join(j.dir, "source."+j.ext)
	j.outPath = filepath.Join(j.dir, "out-"+j.r8+".mp4")
	j.lightPath = filepath.Join(j.dir, "light-"+j.r8+".mp4")
	j.posterPath = filepath.Join(j.dir, "poster-"+j.r8+".jpg")
	j.probePath = filepath.Join(j.dir, "probe.json")
	p := w.deps.Cfg.S3.KeyPrefix
	j.renditionKey = store.RenditionKey(p, clip.ID, j.r8)
	// The LIGHT copy keeps the `renditions/social-{r8}.mp4` key it has had since
	// erratum 121: the tier was renamed in the API and the docs, where people
	// read it, and NOT in the bucket, where a rename would have been a migration
	// of every object for a word (§20 erratum 203).
	j.lightKey = store.SocialKey(p, clip.ID, j.r8)
	j.posterKey = store.PosterKey(p, clip.ID, j.r8)
	j.probeKey = store.ProbeKey(p, clip.ID)
	return j
}

// execute runs the pipeline under recover() and settles the outcome. It
// reports whether the clip became ready, so the caller can fire the hook
// once the run has left the registry.
func (j *jobRun) execute() (ready bool) {
	j.log.Info("transcode started", "attempt", j.job.Attempts+1, "transient_failures", j.job.TransientFailures,
		"checkpointed", j.job.CheckpointJSON != nil, "source_size", j.clip.SourceSize)
	err := j.guarded()
	defer func() {
		// The exit path talks to the DB and the bucket; a panic there must
		// not take the process (HTTP included) down with it. ready stays
		// false: the return below was never reached.
		if p := recover(); p != nil {
			j.log.Error("exit path panicked", "stage", j.stage, "err", fmt.Sprint(p), "stack", string(debug.Stack()))
		}
	}()
	j.finish(err)
	return j.finalized
}

func (j *jobRun) guarded() (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = &panicError{value: p, stack: debug.Stack()}
		}
	}()
	return j.pipeline()
}

// pipeline is §10.3 steps 1–9; a checkpointed job skips straight to 9.
func (j *jobRun) pipeline() error {
	if err := j.recheckClip(); err != nil {
		return err
	}
	if j.job.CheckpointJSON != nil {
		var cp checkpoint
		if err := json.Unmarshal([]byte(*j.job.CheckpointJSON), &cp); err != nil {
			return fmt.Errorf("checkpoint unparsable: %w", err)
		}
		j.ready, j.checkpointed = cp.ready(), true
		j.log.Info("checkpoint found; finalize only")
		return j.finalize()
	}
	j.setStage(stageAcquire)
	if err := j.acquire(); err != nil {
		return err
	}
	j.setStage(stageProbe)
	if err := j.probeSource(); err != nil {
		return err
	}
	// produce sets its OWN stages — `encode`, `remux`, `light`, `verify` — because
	// which of them run is the tier plan's decision and not this list's (§20
	// erratum 206). An audio clip has its own (erratum 328).
	produce := j.produce
	if j.isAudio() {
		produce = j.produceAudio
	}
	if err := produce(); err != nil {
		return err
	}
	for _, s := range []struct {
		stage string
		fn    func() error
	}{
		{stagePoster, j.poster},
		{stageStore, j.store},
	} {
		j.setStage(s.stage)
		if err := s.fn(); err != nil {
			return err
		}
	}
	// The tier decision is settled BEFORE the checkpoint, because the checkpoint
	// is what a finalize-only retry commits from and a retry has no probe to
	// re-derive it (§20 erratum 291).
	j.settleTiers()
	if err := j.checkpoint(); err != nil {
		return err
	}
	return j.finalize()
}

// settleTiers decides what this run's commit says about the clip's tiers, and
// whether the card still needs its own copy, from the probe and the files this
// run produced. It runs once, between `store` and the checkpoint (§20 erratum
// 291). Until then `tierFields()` ran inside finalize — AFTER checkpoint() had
// marshalled j.ready — so every checkpoint ever written carried the zero
// TierFields, and a finalize-only retry off one (a released job, ReleaseJob
// keeps the checkpoint) committed blank tier columns: `original_playable`
// false, `original_codec` and both tier states empty, on a clip whose files
// were all in the bucket. The order inside is load-bearing: playSize() reads
// Tiers.OriginalPlayable, so the columns come first and the instruction second.
func (j *jobRun) settleTiers() {
	j.ready.Tiers = j.tierFields()
	j.settleLight()
}

// settleLight is the commit-time half of that decision: whether the card still
// needs its own copy (§17 #3, §20 errata 121, 203). A run that already made one
// says no; every other run asks the rule about the file the card would fetch,
// whose size is now known. It reads j.ready and nothing else, so a finalize-only
// retry — which holds the checkpoint's fields and no probe — reaches the answer
// this attempt reached, from the same numbers.
func (j *jobRun) settleLight() {
	j.ready.QueueSocial = lightWanted(j.ready, j.playSize(), j.w.deps.Cfg)
	if j.ready.QueueSocial {
		j.ready.Tiers.LightState = db.TierEncoding
	}
}

// produce makes the tiers this upload needs, CARD FIRST (§20 erratum 206).
//
// Exactly one of three things happens to the video:
//
//   - the original is not web-playable → the FULL tier is encoded at the
//     source's own resolution and frame rate;
//   - it is playable but its container is not servable as it stands → it is
//     REMUXED with `-c copy`, which rewrites no coded byte;
//   - it is playable and servable → NOTHING. There is no second object, the
//     upload itself is what the watch page streams, and the clip is ready as
//     soon as a poster exists. That is the remux fast path.
//
// Then, when the file a crawler would fetch is too big, too smooth or too large
// a picture, the LIGHT copy is encoded — and it is encoded HERE, inside the run
// that flips `ready`, rather than queued behind it. That is the whole of
// card-first ordering: `ready` now means CARD-ready, so the moment a viewer can
// watch the clip is also the moment Discord can inline it, and the faithful copy
// of an unplayable source (which is the slow one, at 1440p60) follows as its own
// job afterwards.
//
// The ffmpeg slot is already held for the whole run (worker.runRegistered), so
// nothing here takes a second one.
func (j *jobRun) produce() error {
	if j.tiers.TooLarge {
		// No tier this server can produce, and no downscale either: a picture
		// past the Full ceiling fails the clip with a plain reason (§20 erratum
		// 202). The alternative — quietly shrinking it — is what this whole
		// feature exists to stop.
		return &transcode.ErrPermanent{Err: fmt.Errorf("source_too_large: %s (%dx%d)",
			j.tiers.Reason, j.probe.DisplayW, j.probe.DisplayH)}
	}
	switch {
	case j.tiers.FullNeeded && j.tiers.LightNeeded:
		// BOTH TIERS, ONE DECODE (§20 erratum 217, superseding erratum 206's
		// ordering for exactly this case). A source no browser can play needs a
		// faithful copy AND a card copy, and every other way of getting them was
		// measured and is worse: two processes at once put the card's file 2.2×
		// LATER than encoding it alone, and back to back decodes the same source
		// twice. So one ffmpeg writes both, and the clip becomes ready with the
		// Full AND the card file already in the bucket — which also closes the
		// window in which `/video.mp4` served the 720p30 card copy to viewers.
		j.setStage(stageEncode)
		if err := j.encodeTiers(); err != nil {
			return err
		}
		j.setStage(stageVerify)
		if err := j.verifyFull(); err != nil {
			return err
		}
		// The card's copy is measured LAST, and it leaves `posterSrc` where
		// verifyFull put it — on the FULL, which is untagged (§20 erratum 219) —
		// while lending the poster its own box, so the card image is card-sized
		// without being cut from the one file that carries the tag.
		return j.measureLight(j.srcPath)
	case j.tiers.FullNeeded:
		// A source ≤1280×720 and ≤30 fps that still cannot be played: the Full
		// IS the card's file, so there is nothing to order and one encode does
		// everything.
		j.setStage(stageEncode)
		if err := j.encode(); err != nil {
			return err
		}
		j.setStage(stageVerify)
		if err := j.verifyFull(); err != nil {
			return err
		}
	case j.tiers.Remux:
		j.setStage(stageRemux)
		if err := j.remux(); err != nil {
			return err
		}
		j.setStage(stageVerify)
		if err := j.verifyRemux(); err != nil {
			return err
		}
		if j.tiers.LightNeeded {
			j.setStage(stageLight)
			return j.light()
		}
	default:
		// The remux fast path with nothing at all to do: the upload plays, it is
		// already a faststart mp4, and the clip's own numbers are the probe's.
		j.measureFromSource()
		j.posterSrc, j.posterDurUS, j.posterHasDur, j.posterVideoEnd =
			j.srcPath, j.probe.DurationUS, j.probe.HasDuration, j.probe.Video.EndSec
		if j.tiers.LightNeeded {
			j.setStage(stageLight)
			return j.light()
		}
	}
	return nil
}

// recheckClip closes the claim→registration window (§10.1): a clip that
// was tombstoned or deleted meanwhile is abandoned quietly.
func (j *jobRun) recheckClip() error {
	c, err := j.w.deps.DB.GetClip(j.ctx, j.clip.ID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return errClipGone
	case err != nil:
		return fmt.Errorf("re-read clip: %w", err)
	case c.State == db.ClipDeleting:
		return errClipGone
	}
	j.clip = c
	return nil
}

// acquire (§10.2): use the scratch source if present with the right size,
// else download it; then the free-space rule.
func (j *jobRun) acquire() error {
	return acquireSource(j.ctx, j.w, j.clip, j.dir, j.srcPath, j.log, j.transferCtx, j.safeProgress)
}

// acquireSource is the acquire stage as a free function, shared by the
// transcode pipeline and the trim run (§17 #5) — both need exactly the same
// original on exactly the same terms, and a second copy of the drift log,
// the size re-check and the §10.2 free-space formula would be a second
// chance to get one of them wrong.
//
// transferCtx bounds one download by STORAGE_TRANSFER_MAX; progress reports
// the download fraction (§10.3 step 1) so a multi-GB re-acquire never shows
// a frozen 0%.
//
// The free-space rule is split around the download, because the source is
// counted ONCE (§20 erratum 90). Before a download the bytes are still in the
// bucket, so the room for them must be found: source_size +
// SCRATCH_MIN_FREE_BYTES. Once the source is local — the common case, since
// ingest spooled it there, and the case after a successful download — those
// bytes are already IN the free figure being measured, and demanding room for
// a second copy of them refused exactly the clip ingest had just admitted:
// ingest lets an upload in at 1.5×size + min-free, spools size, and left
// 0.5×size + min-free where the old rule wanted 2×size + min-free. All that is
// required of a local source is the operator's floor.
func acquireSource(ctx context.Context, w *worker, clip db.Clip, dir, srcPath string, log *slog.Logger,
	transferCtx func() (context.Context, context.CancelFunc), progress func(float64, bool)) error {
	if progress == nil {
		// A caller with no progress registry to write to — the `full` job, whose
		// clip is `ready` throughout and therefore has no §8.4 entry (§20 erratum
		// 206). Nil used to be a panic inside the download callback.
		progress = func(float64, bool) {}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("scratch dir: %w", err)
	}
	if st, err := os.Stat(srcPath); err != nil || st.Size() != clip.SourceSize {
		// Removed first: a wrong-sized leftover is about to stop occupying
		// the bytes the check below weighs.
		_ = os.Remove(srcPath)
		if err := requireScratch(w, dir, AcquireScratchNeed(clip.SourceSize, w.deps.Cfg.ScratchMinFree, false)); err != nil {
			return err
		}
		tctx, cancel := transferCtx()
		err := w.deps.Store.Download(tctx, clip.SourceKey, srcPath, func(done, total int64) {
			if total <= 0 {
				total = clip.SourceSize
			}
			if total > 0 {
				progress(float64(done)/float64(total), false)
			}
		})
		cancel()
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// The row references a key the bucket does not hold: drift
				// the design says cannot happen (§12 ERROR). The attempt is
				// still transient — a lagging bucket looks the same.
				log.Error("original missing from the bucket (row/object drift)", "key", clip.SourceKey)
			}
			return fmt.Errorf("download source: %w", err)
		}
		st, err := os.Stat(srcPath)
		if err != nil {
			return fmt.Errorf("downloaded source: %w", err)
		}
		if st.Size() != clip.SourceSize {
			return fmt.Errorf("downloaded source is %d bytes, row says %d", st.Size(), clip.SourceSize)
		}
		log.Info("source downloaded", "bytes", st.Size())
	}
	progress(1, false)
	return requireScratch(w, dir, AcquireScratchNeed(clip.SourceSize, w.deps.Cfg.ScratchMinFree, true))
}

// AcquireScratchNeed is the free-byte figure acquireSource weighs, exported so
// the INGEST precheck can be tested against the very arithmetic the worker
// applies rather than against a copy of it: an upload the API admits must not
// be one the worker then refuses (§20 erratum 90).
//
// sourceIsLocal=false — the source has still to be downloaded — asks for the
// bytes about to land plus the operator's floor; sourceIsLocal=true asks for
// the floor alone, because the source is already part of the free figure.
func AcquireScratchNeed(sourceSize, minFree int64, sourceIsLocal bool) int64 {
	if sourceIsLocal {
		return minFree
	}
	return satAdd(sourceSize, minFree)
}

// requireScratch is the one place a free-space refusal is worded. A statfs
// failure is a real error (the caller's stage names it); a negative free
// figure means "unknown" and skips the check.
func requireScratch(w *worker, dir string, need int64) error {
	free, err := w.free(dir)
	if err != nil {
		return fmt.Errorf("statfs scratch: %w", err)
	}
	if free >= 0 && free < need {
		return fmt.Errorf("%w: %d bytes free, need %d", errNoScratchRoom, free, need)
	}
	return nil
}

func (j *jobRun) probeSource() error {
	p, err := j.w.runner.Probe(j.ctx, j.srcPath)
	if err != nil {
		return err
	}
	j.probe = p
	// The ORIGINAL's own length, recorded on the clip because it is the
	// timeline every trim window is measured on (§20 erratum 68). It is a
	// fact about the upload, so the encode that first describes the upload is
	// where it belongs; a trim re-probes and refreshes it.
	j.ready.SourceDurationMS = sourceDurationMS(p)
	if j.isAudio() {
		// Sound has its own table (§20 erratum 328); the video plan would be
		// asked about a picture this file does not have.
		j.planAudio()
		return nil
	}
	j.tiers = tierPlanFor(p, j.clip, j.w.deps.Cfg, transcode.FaststartMP4(j.srcPath))
	// `plan` is the plan of the encode THIS RUN performs, which is what every
	// consumer of it asks about: the progress arithmetic, the free-space
	// prediction and the output budget. Since §20 erratum 217 a run that needs
	// BOTH tiers produces both in one pass, and the plan of that pass is the FULL
	// one — the bigger output, the longer encode, and the file the progress
	// fraction should be about. A run that needs only the Light copy plans that
	// one; a remux with no Light encodes nothing and leaves the zero plan, which
	// no consumer then reaches.
	switch {
	case j.tiers.FullNeeded:
		j.plan = j.tiers.Full
	case j.tiers.LightNeeded:
		j.plan = j.tiers.Light
	}
	accel, why := transcode.HWAccelFor(j.plan, j.w.deps.Cfg.Transcode.HWAccel,
		transcode.NormalizeFullCodec(j.w.deps.Cfg.Transcode.FullCodec))
	j.log.Info("planned", "display", fmt.Sprintf("%dx%d", p.DisplayW, p.DisplayH), "rotation", p.Rotation,
		"codec", p.VideoCodec, "pix_fmt", p.PixFmt,
		"original_playable", j.tiers.Playable, "remux", j.tiers.Remux, "why_not", j.tiers.Reason,
		"full", j.tiers.FullNeeded, "light", j.tiers.LightNeeded, "card_tier", j.tiers.CardTier,
		"out", fmt.Sprintf("%dx%d", j.plan.W, j.plan.H), "fps", j.plan.FPS.String(), "audio", j.plan.AudioMode.String(),
		"duration_us", p.DurationUS, "has_duration", p.HasDuration,
		// Which argv this clip's encode renders, and why when it is not the
		// hardware one (§20 erratum 216). `hwaccel_why` is empty exactly when the
		// CUDA path was taken and reads "not configured" on a default install.
		"hwaccel", accel, "hwaccel_why", why)
	return nil
}

// tierPlanFor is the tier decision, as one call with everything it needs
// (§20 errata 200–203). It is a free function so the trim and the replacement
// reach the same verdict from the same inputs.
func tierPlanFor(p *transcode.ProbeResult, clip db.Clip, cfg config.Config, faststart bool) transcode.TierPlan {
	tc := cfg.Transcode
	return transcode.MakeTierPlan(p, transcode.TierPlanOptions{
		Box:             transcode.PlanOptions{MaxW: tc.MaxW, MaxH: tc.MaxH, MaxFPS: tc.MaxFPS},
		FullCodec:       tc.FullCodec,
		LightMaxBytes:   cfg.SocialMaxBytes,
		SourceSize:      clip.SourceSize,
		SourceMediaType: clip.SourceContentType,
		Faststart:       faststart,
	})
}

// encode runs the FULL tier's pass: the source's own resolution and frame rate,
// CRF 18 (or AV1's 32) in FULL_CODEC's encoder.
func (j *jobRun) encode() error {
	if err := j.checkOutputRoom(); err != nil {
		return err
	}
	budget := j.outputBudget()
	if err := j.w.runner.EncodeFull(j.ctx, j.plan, j.srcPath, j.outPath, budget, j.safeProgress); err != nil {
		return err
	}
	return j.checkOutputSize(budget)
}

// remux rewrites the container of a playable original: `-c copy` into a
// faststart mp4 (§20 erratum 200). It is not an encode — no CRF, no filter
// graph, no progress to report — so it borrows nothing from the encode's guards
// except the free-space rule, sized from the source rather than from a plan
// (a stream copy's output is the source's coded bytes plus a container).
func (j *jobRun) remux() error {
	if err := requireScratch(j.w, j.dir, satAdd(j.clip.SourceSize, j.w.deps.Cfg.ScratchMinFree)); err != nil {
		return err
	}
	if err := j.w.runner.Remux(j.ctx, j.probe, j.srcPath, j.outPath); err != nil {
		return err
	}
	j.progress(1, false)
	return nil
}

// light encodes the card's own copy, from the best file this run has: the Full
// or the remux when one was produced, and the source itself on the direct path.
// Reading the produced file rather than the source is not a shortcut — it is
// the same argument the social job has always made (§17 #3): that file IS the
// clip as everyone sees it, so a card derived from it can never show something
// the player does not.
func (j *jobRun) light() error {
	in := j.srcPath
	plan := j.tiers.Light
	if j.mainProduced() {
		// The produced file's streams are the video, then its one audio track,
		// whatever order the upload had (§20 erratum 351): the plan's indices
		// are the upload's and must not be used against it. The remux keeps the
		// upload's Display Matrix and an encoded Full does not, so the rotation
		// rides only with the remux (§20 erratum 352).
		in = j.outPath
		plan = plan.ForProducedFile(j.tiers.Remux)
	}
	if err := checkOutputRoom(j.w, j.dir, plan); err != nil {
		return err
	}
	tag, err := j.lightTag()
	if err != nil {
		return err
	}
	if err := j.w.runner.EncodeSocial(j.ctx, plan, in, j.lightPath, tag, j.lightBudget(), j.safeProgress); err != nil {
		return err
	}
	return j.measureLight(in)
}

// encodeTiers is the ONE pass that writes the FULL tier and the LIGHT copy
// together (§20 erratum 217). See the erratum for the measurement: two processes
// at once put the card's file 2.2× later than encoding it alone, and back to back
// pays for the decode twice.
//
// The room and the budget are the PAIR's. checkOutputRoom is asked once, about
// the sum, because two calls in a row would each weigh the same free bytes; the
// runtime cap is the sum for the reason EncodeTiers documents (ffmpeg's
// `total_size` is not per-output), and each file is then weighed against its own
// budget by the verify steps that follow.
func (j *jobRun) encodeTiers() error {
	if err := checkOutputRoomFor(j.w, j.dir, j.tiers.Full, j.tiers.Light); err != nil {
		return err
	}
	tag, err := j.lightTag()
	if err != nil {
		return err
	}
	if err := j.w.runner.EncodeTiers(j.ctx, j.tiers.Full, j.tiers.Light, j.srcPath, j.outPath, j.lightPath,
		tag, satAdd(j.outputBudget(), j.lightBudget()), j.safeProgress); err != nil {
		return err
	}
	// Each file against its OWN budget: the runtime cap could only be the pair's,
	// so a Full that ran away inside a sum the card's copy left room in is caught
	// here (measureLight does the same for the card's copy).
	return j.checkOutputSize(j.outputBudget())
}

// lightBudget is the ceiling on what the LIGHT copy may write.
func (j *jobRun) lightBudget() int64 {
	return outputBudget(j.clip.SourceSize, j.w.deps.Cfg.MaxUploadBytes, j.tiers.Light)
}

// lightTag is the corner line to burn into this clip's LIGHT copy (§20 erratum
// 214): the card copy's own quality, and the best tier's — the Full's planned
// numbers when there will be one, the upload's own otherwise.
func (j *jobRun) lightTag() (transcode.LightTag, error) {
	light := transcode.QualityShort(j.tiers.Light.W, j.tiers.Light.H, j.tiers.Light.FPS)
	best := transcode.QualityShort(j.probe.DisplayW, j.probe.DisplayH, j.probe.FPS)
	if j.tiers.FullNeeded {
		best = transcode.QualityShort(j.tiers.Full.W, j.tiers.Full.H, j.tiers.Full.FPS)
	}
	return j.w.runner.LightTagFor(j.dir, "tag-"+j.r8+".txt", light, best)
}

// measureLight measures the card's copy and records it on the commit: the file
// must be what the tier promised, it must be a faststart mp4, and it must be
// inside the budget it was granted. `from` names the file it was derived from,
// for the log line alone.
func (j *jobRun) measureLight(from string) error {
	budget := j.lightBudget()
	info, err := j.w.runner.ProbeOutput(j.ctx, j.lightPath)
	if err != nil {
		return err
	}
	tc := j.w.deps.Cfg.Transcode
	if err := transcode.CheckOutput(info, transcode.PlanOptions{MaxW: tc.MaxW, MaxH: tc.MaxH, MaxFPS: tc.MaxFPS}); err != nil {
		return err
	}
	if err := j.w.runner.Verify(j.lightPath); err != nil {
		return err
	}
	st, err := os.Stat(j.lightPath)
	if err != nil {
		return &transcode.ErrPermanent{Err: fmt.Errorf("bad_output: the light copy is unreadable: %w", err)}
	}
	if budget > 0 && st.Size() > budget {
		return &transcode.ErrPermanent{Err: fmt.Errorf(
			"output_too_large: the light copy is %d bytes, past the %d budgeted for it", st.Size(), budget)}
	}
	j.lightOut = info
	j.ready.Light = db.SocialFields{Key: j.lightKey, Size: st.Size(),
		Width: info.Width, Height: info.Height, FPSNum: info.FPS.Num, FPSDen: info.FPS.Den}
	// The card image is NOT cut from this file (§20 erratum 219). It carries
	// erratum 214's burned-in line, and a poster is exactly what that amendment
	// does not cover: it is the og:image, the still the site's own player shows
	// before play, and a JPEG anybody can download. `posterSrc` therefore stays on
	// the untagged file this copy was derived from — the one the branch above
	// already chose — and what the card copy lends the poster is its SIZE, as the
	// box the picture is written inside.
	j.posterBox = transcode.PosterBox{W: j.tiers.Light.W, H: j.tiers.Light.H}
	j.log.Info("light copy encoded", "bytes", st.Size(), "dims", fmt.Sprintf("%dx%d", info.Width, info.Height),
		"fps", info.FPS.String(), "from", filepath.Base(from))
	return nil
}

// mainProduced reports whether this run wrote and MEASURED a rendition object —
// a Full or a remux. `j.out` is set by exactly those two verify steps, so it is
// the fact itself rather than a second reading of the plan: false covers both
// the direct path (the upload is the playable file, `rendition_key` stays NULL,
// §20 erratum 201) and the card-first path (the Full is a job of its own, and
// this run wrote only the Light copy, erratum 206).
func (j *jobRun) mainProduced() bool { return j.out != nil }

// checkOutputRoom applies §10.2's free-space rule to what the PLAN schedules,
// which acquire's 2×source_size cannot see (§20 erratum 42): the encode is
// CFR, so a container that declares a long duration over a couple of frames
// makes ffmpeg duplicate frames to fill it, and the output grows with that
// declared duration rather than with the uploaded bytes MAX_UPLOAD_BYTES
// bounds — 113 KB in, 81 MB out at one hour, linear from there. Same class,
// no hostility needed: a long screen recording with dropped frames encodes
// far past twice its source. Sizing the guard from the plan keeps the
// promise the formula was written for — the encode never starts without room
// for its own output, so it can neither fill the shared scratch volume under
// unrelated in-flight jobs nor put more in the bucket than the operator's
// disk allows. A plan with no usable duration has nothing to size from: the
// prediction falls back to the source-based guard, and outputBudget's
// runtime cap — not this check — is what bounds it (§20 erratum 46).
func (j *jobRun) checkOutputRoom() error { return checkOutputRoom(j.w, j.dir, j.plan) }

// sourceDurationMS is the original's container duration in milliseconds, or
// nil when the source declared none (a raw stream, a broken index). nil is
// written as "leave it alone" rather than as a zero, because a clip whose
// source length is unknown is exactly the clip whose trim windows must not be
// bounded by a guess.
func sourceDurationMS(p *transcode.ProbeResult) *int64 {
	if p == nil || !p.HasDuration || p.DurationUS <= 0 {
		return nil
	}
	ms := int64(math.Round(float64(p.DurationUS) / 1000))
	return &ms
}

// checkOutputRoom as a free function; the trim reserves room for its own
// output the same way, from a plan whose duration is the TRIM window rather
// than the whole source.
func checkOutputRoom(w *worker, dir string, plan transcode.Plan) error {
	return checkOutputRoomFor(w, dir, plan)
}

// checkOutputRoomFor is that rule for a run that writes SEVERAL outputs at once
// (§20 erratum 217): the room asked for is the SUM, because the two files land
// on the same disk at the same time. Asking twice in a row would weigh the same
// free bytes twice and reserve nothing for the second file. A plan that cannot
// be sized contributes nothing, exactly as it always did; a sum that overflows
// saturates, so an absurd estimate cannot wrap into a negative need (§20
// erratum 48).
func checkOutputRoomFor(w *worker, dir string, plans ...transcode.Plan) error {
	var need int64
	for _, plan := range plans {
		if n := plannedOutputBytes(plan); n > 0 {
			need = satAdd(need, n)
		}
	}
	if need <= 0 {
		return nil
	}
	plan := plans[0]
	need = satAdd(need, w.deps.Cfg.ScratchMinFree)
	free, err := w.free(dir)
	if err != nil {
		return fmt.Errorf("statfs scratch: %w", err)
	}
	if free >= 0 && free < need {
		return fmt.Errorf("%w: %d bytes free, the planned output (%dx%d @ %s for %d ms) needs %d",
			errNoScratchRoom, free, plan.W, plan.H, plan.FPS.String(), plan.DurationUS/1000, need)
	}
	return nil
}

// satAdd adds without wrapping: a sum past the int64 range saturates at the
// bound it ran into. The scratch guards weigh a "need" built from
// uploader-influenced numbers against the free space, and a WRAPPED need is
// a negative need — a guard that lets everything through, exactly where the
// number that overflowed it is the most absurd one (§20 erratum 48).
func satAdd(a, b int64) int64 {
	switch {
	case b > 0 && a > math.MaxInt64-b:
		return math.MaxInt64
	case b < 0 && a < math.MinInt64-b:
		return math.MinInt64
	}
	return a + b
}

// plannedOutputBytes bounds the rendition a plan schedules:
// W·H·fps·duration·transcode.OutputBitsPerPixel / 8. 0 means "not sizeable" — no
// duration, no frame rate or no dimensions to work from. math.MaxInt64 means
// the estimate itself overflowed, which is not a bound but the ABSENCE of
// one: callers must refuse such a plan rather than size anything from it
// (§20 erratum 48).
//
// The arithmetic moved into internal/transcode with §20 erratum 235, because
// MakeTierPlan asks the same question about the Full it has not encoded yet and
// two estimators of one number is how they come to disagree. This is the
// worker's name for it, kept so the §10.2 guard and the output budget read as
// they always have.
func plannedOutputBytes(p transcode.Plan) int64 { return transcode.PlannedOutputBytes(p) }

// outputBudget is the hard ceiling on what one encode may WRITE, enforced
// while it runs (§20 erratum 46). checkOutputRoom only predicts, and it
// predicts from `format.duration` — metadata the uploader controls, which a
// container may omit entirely (any mkv/webm muxed to a non-seekable output:
// a browser MediaRecorder, ffmpeg or OBS piping) or state as a lie. The
// frame timestamps that actually drive `-r fps -fps_mode cfr` are never
// measured, so an unsized plan reserved nothing and a short-lying one
// reserved too little, while the CFR gap fill runs on until the six-hour
// hard timeout. The budget is the largest of the three bounds this clip has
// already been granted — what the plan schedules (checkOutputRoom found the
// room for it), twice the source, and MAX_UPLOAD_BYTES, the operator's own
// statement of what one clip may cost —
// so nothing an honest source encodes to can reach it, and a runaway is
// killed at a number the operator has accepted instead of at the disk.
//
// The three terms are ceilings this clip has been GRANTED, not room anybody
// went and found: the plan's estimate is the one checkOutputRoom really
// reserved, MAX_UPLOAD_BYTES is the operator's own statement of what one clip
// may cost, and 2×source_size is a proportionality rule — an honest encode of
// a file the operator accepted does not come out at twice its size — and NOT,
// since §20 erratum 90, a reservation acquire made (acquire counts the source
// once and reserves only SCRATCH_MIN_FREE_BYTES on top of it).
func (j *jobRun) outputBudget() int64 {
	return outputBudget(j.clip.SourceSize, j.w.deps.Cfg.MaxUploadBytes, j.plan)
}

// outputBudget is that ceiling as a free function (the trim shares it: it
// writes an mp4 from the same source under the same plan, so it is granted
// the same bounds and no others).
func outputBudget(sourceSize, maxUpload int64, plan transcode.Plan) int64 {
	src := min(sourceSize, math.MaxInt64/scratchSpaceMul) * scratchSpaceMul
	granted := max(src, maxUpload)
	// A plan whose own estimate overflowed was granted nothing by
	// checkOutputRoom — it refuses it — so it must not be budgeted from
	// either: math.MaxInt64 as the ceiling is the cap switched OFF, and the
	// only bounds left standing are the two that are real (§20 erratum 48).
	if planned := plannedOutputBytes(plan); planned != math.MaxInt64 {
		granted = max(granted, planned)
	}
	return granted
}

// checkOutputSize is the belt-and-braces half of the cap: Encode kills the
// run on the byte counter ffmpeg reports, this measures the file that
// survived it — a muxer that reports no total_size, and a run that exited 0
// in the same instant the cap fired, still cannot hand a runaway rendition
// to verify and store.
func (j *jobRun) checkOutputSize(budget int64) error {
	st, err := os.Stat(j.outPath)
	if err != nil || budget <= 0 || st.Size() <= budget {
		// An output that cannot even be stat'ed is verify's to reject, in
		// verify's own words; this check only ever adds a verdict.
		return nil
	}
	return &transcode.ErrPermanent{Err: fmt.Errorf("output_too_large: the rendition is %d bytes, past the %d budgeted for it", st.Size(), budget)}
}

// verifyFull measures the FULL tier: exactly the plan's dimensions (not "inside
// a box" — this tier has no box), one video stream in the configured codec, at
// most one aac track, and moov before mdat.
func (j *jobRun) verifyFull() error {
	info, err := j.w.runner.ProbeOutput(j.ctx, j.outPath)
	if err != nil {
		return err
	}
	if err := transcode.CheckTierOutput(info, transcode.TierCheck{
		Codecs: []string{transcode.NormalizeFullCodec(j.w.deps.Cfg.Transcode.FullCodec)},
		Width:  j.plan.W, Height: j.plan.H,
	}); err != nil {
		return err
	}
	if err := j.w.runner.Verify(j.outPath); err != nil {
		return err
	}
	j.out = info
	j.measure(info.Width, info.Height, info.DurationMS, info.FPS, info.HasAudio)
	j.posterSrc, j.posterDurUS, j.posterHasDur, j.posterVideoEnd =
		j.outPath, info.DurationMS*1000, info.DurationMS > 0, info.VideoEndSec
	return nil
}

// verifyRemux measures the container rewrite. Its expected dimensions are the
// source's CODED ones — a copy inherits the coded frame and the display matrix
// untouched, so the clip's own width and height come from the probe's DISPLAY
// dims instead (a phone capture is coded 1920×1080 and displayed 1080×1920).
// Odd dimensions are allowed here and nowhere else, for the same reason: we did
// not choose them, and refusing a file we are not re-encoding would be refusing
// the upload.
func (j *jobRun) verifyRemux() error {
	info, err := j.w.runner.ProbeOutput(j.ctx, j.outPath)
	if err != nil {
		return err
	}
	if err := transcode.CheckTierOutput(info, transcode.TierCheck{
		Codecs: []string{transcode.FullCodecH264, transcode.FullCodecAV1},
		Width:  j.probe.CodedW, Height: j.probe.CodedH, AllowOdd: true,
	}); err != nil {
		return err
	}
	if err := j.w.runner.Verify(j.outPath); err != nil {
		return err
	}
	j.out = info
	j.measureFromSource()
	j.posterSrc, j.posterDurUS, j.posterHasDur, j.posterVideoEnd =
		j.outPath, info.DurationMS*1000, info.DurationMS > 0, info.VideoEndSec
	return nil
}

// measureFromSource records the clip's own facts from the SOURCE probe, which is
// what the Original tier's numbers are: the display dims (SAR and rotation
// applied), the container duration, the source's average rate, and whether it
// has audio at all. It is the direct path's whole "verify" — there is no output
// to measure, because the upload is the output — and it is also what the remux
// path uses for the row, because a copy's coded dims are not its display dims.
func (j *jobRun) measureFromSource() {
	p := j.probe
	durMS := int64(0)
	if p.HasDuration && p.DurationUS > 0 {
		durMS = int64(math.Round(float64(p.DurationUS) / 1000))
	}
	j.measure(p.DisplayW, p.DisplayH, durMS, p.FPS, len(p.AudioIndexes) > 0)
}

// measure records the measurements of the BEST PLAYABLE TIER, which is what
// clips.width/height/duration_ms/fps_*/has_audio have always meant — the shape
// of the file `/video.mp4` serves. On the remux and direct paths that is the
// original; with a Full it is the Full; and while a Full is still encoding it is
// the LIGHT copy, until SetFull replaces these five numbers with the Full's own.
func (j *jobRun) measure(w, h int, durMS int64, fps transcode.Rational, hasAudio bool) {
	j.ready.Width, j.ready.Height = w, h
	j.ready.DurationMS = durMS
	j.ready.FPSNum, j.ready.FPSDen = fps.Num, fps.Den
	j.ready.HasAudio = hasAudio
}

func (j *jobRun) poster() error {
	if j.isAudio() {
		return j.posterAudio()
	}
	res, err := j.w.runner.Poster(j.ctx, j.posterSrc, j.posterPath,
		j.posterDurUS, j.posterHasDur, j.posterVideoEnd, j.posterBox)
	if err != nil {
		return err
	}
	if res.NearBlack {
		// Every candidate was black (§20 erratum 92). The clip keeps a
		// poster — a card with no image is worse — but the card will look
		// empty, and this is the only place that says why.
		j.log.Info("the poster is near-black; no lit frame was found", "seconds", res.Seconds)
	}
	j.ready.PosterWidth, j.ready.PosterHeight = res.Width, res.Height
	return nil
}

// store uploads rendition, poster and probe.json sequentially (§10.3
// step 7). Every key written this attempt is recorded for cleanup.
func (j *jobRun) store() error {
	if err := j.writeProbeJSON(); err != nil {
		return err
	}
	// The rendition, when this run produced one at all. The direct path (a
	// playable, faststart upload) has no rendition object and the card-first path
	// has not made its Full yet — in both cases `rendition_key` stays NULL and
	// the media routes resolve the playable bytes from the row instead (§20
	// erratum 201).
	if j.mainProduced() {
		ct := "video/mp4"
		if j.renditionCT != "" {
			ct = j.renditionCT // an audio Full: audio/flac or audio/mp4 (§20 erratum 328)
		}
		res, err := j.put(j.renditionKey, j.outPath, ct, func(done, total int64) {
			if total > 0 {
				j.safeProgress(storeShareRendition*float64(done)/float64(total), false)
			}
		})
		if err != nil {
			return fmt.Errorf("upload rendition: %w", err)
		}
		j.ready.RenditionKey, j.ready.RenditionSize, j.ready.RenditionETag = j.renditionKey, res.Size, res.SHA256
	}
	// The LIGHT copy, when one was made. It is uploaded in the SAME step as the
	// rendition and committed by the same transaction: the card's file existing
	// is what `ready` means now, so it cannot be a later upload (§20 erratum 206).
	if j.ready.Light.Key != "" {
		res, err := j.put(j.lightKey, j.lightPath, "video/mp4", nil)
		if err != nil {
			return fmt.Errorf("upload light copy: %w", err)
		}
		j.ready.Light.Size, j.ready.Light.ETag = res.Size, res.SHA256
	}
	j.progress(storeShareRendition, false)
	res, err := j.put(j.posterKey, j.posterPath, transcode.PosterContentType(j.posterKey), nil)
	if err != nil {
		return fmt.Errorf("upload poster: %w", err)
	}
	j.ready.PosterKey, j.ready.PosterSize, j.ready.PosterETag = j.posterKey, res.Size, res.SHA256
	j.progress(storeSharePoster, false)
	if _, err := j.put(j.probeKey, j.probePath, "application/json", nil); err != nil {
		return fmt.Errorf("upload probe.json: %w", err)
	}
	j.progress(1, false)
	return nil
}

// put uploads one file. The key is recorded as attempted BEFORE the
// upload so a failed or aborted PutFile is cleaned up (DeletePrefix is
// what aborts an incomplete multipart upload, §20.6).
func (j *jobRun) put(key, path, ct string, progress func(done, total int64)) (store.PutResult, error) {
	j.attempted = append(j.attempted, key)
	ctx, cancel := j.transferCtx()
	defer cancel()
	return j.w.deps.Store.PutFile(ctx, key, path, ct, progress)
}

// transferCtx bounds one transfer by STORAGE_TRANSFER_MAX on top of the
// run ctx (the store's idle watchdog handles stalls).
func (j *jobRun) transferCtx() (context.Context, context.CancelFunc) {
	if m := j.w.deps.Cfg.S3.TransferMax; m > 0 {
		return context.WithTimeout(j.ctx, m)
	}
	return context.WithCancel(j.ctx)
}

func (j *jobRun) writeProbeJSON() error {
	tiers := j.tiers
	doc := probeDoc{Source: j.probe.Raw, Plan: j.plan, Output: j.out, Tiers: &tiers, Light: j.lightOut}
	if j.isAudio() {
		audio := j.audio
		doc.Audio = &audio
	}
	if len(doc.Source) == 0 {
		doc.Source = json.RawMessage("null")
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("probe.json: %w", err)
	}
	if err := os.WriteFile(j.probePath, b, 0o644); err != nil {
		return fmt.Errorf("probe.json: %w", err)
	}
	return nil
}

// checkpoint (§10.3 step 8): from this write on no failure path deletes
// the outputs; a retry is finalize-only.
func (j *jobRun) checkpoint() error {
	b, err := json.Marshal(checkpointOf(j.ready))
	if err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	// Pinned to this attempt's claim stamp (§20 erratum 291): a row that is no
	// longer ours — repaired, cancelled, perhaps a newer attempt's by now — is
	// not written, and the run ends as a lost attempt rather than as a failure
	// of the row's current owner.
	switch err := j.w.deps.DB.SaveCheckpoint(j.ctx, j.job.ID, db.ClaimStamp(j.job), string(b), j.w.now().Unix()); {
	case err == nil:
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		// Two hands can have taken the row. MarkDeleting cancels a clip's jobs
		// as it tombstones the clip, so a tombstone that landed during the run
		// is met HERE now rather than at finalize's conditional flip — and it is
		// the same lost race it always was, ended the same way. Anything else
		// is a repair, and the row may already be a newer attempt's.
		if c, cerr := j.w.deps.DB.GetClip(j.ctx, j.clip.ID); errors.Is(cerr, db.ErrNotFound) || (cerr == nil && c.State == db.ClipDeleting) {
			j.lostRace()
			return errLostRace
		}
		return fmt.Errorf("%w: %v", errLostAttempt, err)
	default:
		return fmt.Errorf("save checkpoint: %w", err)
	}
	j.checkpointed = true
	return nil
}

// finalize (§10.3 step 9): conditional ready flip with bounded retries;
// false → lost race (delete own outputs, cancel the job, INFO).
func (j *jobRun) finalize() error {
	j.setStage(stageFinalize)
	// The tier columns were settled before the checkpoint (settleTiers), and a
	// finalize-only retry takes them from the checkpoint, which is where this
	// very struct wrote them — it has NO PROBE, it never ran one, and nothing
	// here re-derives anything from one (§20 erratum 291; erratum 206 for why the
	// checkpoint carries the decision at all). QueueSocial is the one exception:
	// it is an INSTRUCTION rather than a column and rides on no checkpoint, so it
	// is derived here on both paths — on a fresh run it repeats settleTiers'
	// answer from the same numbers, on a retry it is the checkpoint's own fields
	// asked the same question.
	j.settleLight()
	// The clip webhook's instruction (§20 erratum 319), derived on both paths
	// for QueueSocial's reason: it rides on no checkpoint.
	j.ready.QueueHook = j.w.deps.Hook != nil
	var ok, mine bool
	var err error
	for attempt := 0; ; attempt++ {
		// The notify bit and the ready flip are ONE commit (§17 #4): with a
		// webhook configured, a listed clip cannot become ready without its
		// announcement being durably queued — and, since ROADMAP #26, without
		// the webhook it is routed to being decided in that same commit.
		ok, mine, err = j.w.deps.DB.FinalizeTranscode(j.ctx, j.job.ID, j.clip.ID, j.ready,
			j.w.deps.Notifier != nil, j.w.now().Unix(), j.w.notifyRouting())
		if err == nil || j.ctx.Err() != nil || attempt >= len(finalizeBackoff) {
			break
		}
		j.log.Warn("finalize failed; retrying", "attempt", attempt+1, "err", err)
		select {
		case <-j.ctx.Done():
		case <-time.After(finalizeBackoff[attempt]):
		}
	}
	if err != nil {
		if j.ctx.Err() == nil {
			j.log.Error("finalize gave up", "err", err)
		}
		return fmt.Errorf("finalize: %w", err)
	}
	// A retry whose PREDECESSOR actually committed (§20 erratum 102). The clip
	// is ready on this attempt's own keys, so this is not a lost race and the
	// outputs must be kept — but the retry loop above only ran because a commit
	// reported an error it had not suffered, which is worth a line.
	if !ok && mine {
		j.log.Warn("finalize had already been applied by this attempt; keeping its objects (an earlier call committed and reported an error)")
		ok = true
	}
	if !ok {
		j.lostRace()
		return errLostRace
	}
	j.removeScratch(true)
	j.progressTerminal(db.ClipReady, "")
	j.log.Info("clip ready", "dims", fmt.Sprintf("%dx%d", j.ready.Width, j.ready.Height), "duration_ms", j.ready.DurationMS,
		"fps", fmt.Sprintf("%d/%d", j.ready.FPSNum, j.ready.FPSDen), "has_audio", j.ready.HasAudio,
		"rendition_bytes", j.ready.RenditionSize, "rendition_key", j.ready.RenditionKey)
	j.finalized = true // Hooks.OnReady fires once the run has left the registry (worker.onReady)
	return nil
}

// tierFields is what this run's commit says about the clip's tiers (§20 errata
// 200–206). Every value is a fact about what just happened; the one instruction
// — QueueFull — is set in exactly one case, the Full tier wanted and this run
// having made the LIGHT copy instead (card-first ordering), and that case is
// RETAINED BUT UNREACHED since errata 217 and 235: a plan that wants both tiers
// runs one ffmpeg and publishes both files at the flip, so a run that wants the
// Full tier has always produced it. The arm stays, and stays correct, because it
// is the right answer the day a plan splits the two encodes again — which is why
// erratum 217 kept the `full` job kind too.
func (j *jobRun) tierFields() db.TierFields {
	if j.isAudio() {
		return j.tierFieldsAudio()
	}
	p := j.probe
	f := db.TierFields{
		OriginalPlayable: j.tiers.Playable,
		OriginalRemuxed:  j.tiers.Remux,
		OriginalWidth:    p.DisplayW, OriginalHeight: p.DisplayH,
		OriginalFPSNum: p.FPS.Num, OriginalFPSDen: p.FPS.Den,
		OriginalCodec: p.VideoCodec,
		FullState:     db.TierSkipped,
		LightState:    db.TierSkipped,
	}
	if j.ready.Light.Key != "" {
		f.LightState = db.TierReady
	}
	switch {
	case j.tiers.FullNeeded && j.mainProduced():
		f.FullState, f.FullCodec = db.TierReady, transcode.NormalizeFullCodec(j.w.deps.Cfg.Transcode.FullCodec)
	case j.tiers.FullNeeded:
		// The faithful copy is queued behind the card's. `full_codec` is left
		// empty on purpose: the job that produces it records which encoder it
		// actually used, and an operator who changes FULL_CODEC while a job is
		// queued should read the truth rather than the intention.
		f.FullState, f.QueueFull = db.TierEncoding, true
	}
	return f
}

// playSize is the size of the file a card would fetch if no Light copy existed:
// the rendition this run produced, or the upload itself on the direct path. It
// is what the commit-time skip rule weighs, and it is 0 — "unknown, decide on
// shape alone" — for a run that produced neither (the card-first path, whose
// Light copy already exists).
// It reads j.ready and j.clip rather than j.tiers, so a finalize-only retry off
// a checkpoint — which has no probe and no tier plan, only the committed fields —
// weighs the same numbers this attempt did.
func (j *jobRun) playSize() int64 {
	if j.ready.RenditionKey != "" {
		return j.ready.RenditionSize
	}
	if j.ready.Tiers.OriginalPlayable && !j.ready.Tiers.OriginalRemuxed {
		return j.clip.SourceSize
	}
	return 0
}

// lostRace: the clip was tombstoned during the run — remove the three keys
// this run owns and mark the job cancelled (INFO; an expected outcome).
func (j *jobRun) lostRace() {
	ctx, cancel := j.exitCtx()
	defer cancel()
	switch err := j.w.deps.DB.CancelJob(ctx, j.job.ID, db.ClaimStamp(j.job), j.w.now().Unix()); {
	case err == nil:
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		// The ordinary shape of a lost race: MarkDeleting cancelled the job
		// itself, so the row already ended under another hand (§20 erratum 291).
		j.log.Info("job already ended under another hand; nothing to cancel", "err", err)
	case errors.Is(err, context.DeadlineExceeded):
		j.log.Warn("cancel job after lost race: DB slow; boot reconciliation will repair the row", "err", err)
	default:
		j.log.Error("cancel job after lost race", "err", err)
	}
	j.deleteKeys(ctx, j.outputKeys())
	j.removeScratch(true)
	j.w.prog.forget(j.clip.ID)
	j.log.Info("lost_race: clip deleted during transcode; outputs removed")
}

// outputKeys are the keys a completed attempt wrote (from the ready fields
// so a finalize-only retry names the right ones).
func (j *jobRun) outputKeys() []string {
	var keys []string
	for _, k := range []string{j.ready.RenditionKey, j.ready.Light.Key, j.ready.PosterKey} {
		if k != "" {
			keys = append(keys, k)
		}
	}
	return append(keys, j.probeKey)
}

// finish settles a run's outcome (§11 classification). A panic is looked
// at first: it is ERROR whatever else is going on (§12), and a panic that
// coincides with Cancel/Stop releases rather than counts — the cancellation
// ended the attempt, and a file-triggered panic recurs (and counts) on the
// uncancelled retry.
func (j *jobRun) finish(err error) {
	var pe *panicError
	switch {
	case err == nil, errors.Is(err, errLostRace):
		return
	case errors.Is(err, errClipGone):
		j.log.Info("clip gone before the run started; nothing to do")
		return
	case errors.Is(err, errLostAttempt):
		j.lostAttempt(err)
		return
	case errors.As(err, &pe):
		j.log.Error("job panicked", "stage", j.stage, "err", pe.Error(), "cancelled", j.ctx.Err() != nil, "stack", string(pe.stack))
		if j.ctx.Err() != nil {
			j.release()
			return
		}
		j.fail(db.FailCrash, err)
	case j.ctx.Err() != nil:
		j.release()
	case errors.Is(err, errNoScratchRoom):
		j.noRoom(err)
	case transcode.IsPermanent(err):
		j.fail(db.FailPermanent, err)
	default:
		j.fail(db.FailTransient, err)
	}
}

// lostAttempt is the errLostAttempt ending (§20 erratum 291). The row is NOT
// touched — it is not ours, and FailAttempt would spend its current owner's
// counter — and nothing this attempt uploaded is referenced by anything, since
// the checkpoint is what would have referenced it: the uploads go the way a
// failed attempt's do, this attempt's scratch outputs go, the source stays for
// whoever runs the row next. INFO, because it is an expected ending of the
// repair the operator (or a second process) chose to run.
//
// Since §20 erratum 293 the other endings reach it too — release(), fail() and
// noRoom() are pinned now and answer the same refusal — and one of them can
// arrive with a checkpoint already written, which the ending from the
// checkpoint itself never could. That checkpoint NAMES these objects, the
// repair that took the row kept it (a checkpointed row is released, never
// failed), and the retry finalizes onto them: so a checkpointed lost attempt
// leaves the bucket alone. Objects a still newer checkpoint has since
// superseded are bounded orphans under the clip's own prefix, which is the
// safe direction and the one §10.1 already takes.
func (j *jobRun) lostAttempt(err error) {
	ctx, cancel := j.exitCtx()
	defer cancel()
	if j.checkpointed {
		j.log.Info("job row is no longer this attempt's; its checkpoint keeps the outputs, row untouched",
			"stage", j.stage, "err", err)
	} else {
		j.log.Info("job row is no longer this attempt's; outputs discarded, row untouched", "stage", j.stage, "err", err)
		j.deleteKeys(ctx, j.attempted)
	}
	j.removeScratch(false)
	// This attempt's view of the clip is obsolete; the row (queued, or a newer
	// attempt's) is the truth GET …/progress falls back to once it is gone.
	j.w.prog.forget(j.clip.ID)
}

// noRoom is the §20 erratum 90 ending: the attempt is requeued with a backoff
// and BOTH counters untouched, this attempt's outputs are cleared and the
// SOURCE is kept for the retry. Nothing about the disk reaches the clip.
func (j *jobRun) noRoom(err error) {
	ctx, cancel := j.exitCtx()
	defer cancel()
	if lost := requeueNoCount(ctx, j.w, j.log, j.job, j.clip.ID, j.stage, msgNoScratchRoom, j.reason(err)); lost != nil {
		// The row stopped being ours while we were deciding the disk was full
		// (§20 erratum 293). A checkpointed run cannot get here — the pinned
		// checkpoint precedes every ending on this path — so the uploads go.
		j.lostAttempt(lost)
		return
	}
	if !j.checkpointed {
		j.deleteKeys(ctx, j.attempted)
	}
	j.removeScratch(false)
	j.setProgress(Progress{State: db.ClipQueued, Stage: db.JobStageQueued}, true)
}

// requeueNoCount is the shared body of that ending for all three run kinds.
// The free-byte count is in `detail`, which is a log field; `reason` is what
// the row keeps, and every kind passes a bounded sentence with no numbers in
// it, because every kind's last_error is read by somebody who is not the
// operator.
// requeueNoCount is the erratum 90 requeue: `reason` is the bounded sentence
// stored on the job, `detail` the operator's fuller wording on the WARN line
// beside it. Every caller passes its run's reason(err) rather than
// err.Error(): the detail is log-only, but the redaction is the same one
// defence the public text has, and wording six runs' disk refusals two
// different ways for no reason was how three of them came to be missing it.
//
// It takes the whole Job because the requeue is PINNED to the attempt (§20
// erratum 293), and it returns the refusal when the row turned out not to be
// this attempt's any more, so the caller can take its kind's lost-attempt
// ending instead of reporting a requeue that did not happen. Anything else is
// logged here and is not the caller's business.
func requeueNoCount(ctx context.Context, w *worker, log *slog.Logger, job db.Job, clipID, stage, reason, detail string) error {
	log.Warn("no scratch room; requeued without spending the outage budget", "stage", stage, "detail", detail)
	err := w.deps.DB.RequeueWithBackoff(ctx, job.ID, db.ClaimStamp(job), clipID, reason, scratchRequeueDelay, w.now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		return err
	default:
		log.Error("requeue after a scratch shortage", "err", err)
	}
	return nil
}

// reason renders the stored failure text (§20 erratum 90 for the storage
// half; erratum 20 for the scratch half).
func (j *jobRun) reason(err error) string {
	return publicReason(err, j.stage, j.dir, j.w.deps.Cfg)
}

// publicReason renders a failure as the text an ANONYMOUS reader may see:
// `clips.failure_reason` is returned by the public GET /api/clips/{id} and
// GET …/progress and rendered on the share page, and the gif route's 503
// prints a preview job's last_error. It is "stage: message" with the
// permanent wrapper stripped and a length bound, and the message is:
//
//   - a storage failure's bounded CLASS and nothing else. minio-go builds a
//     transport failure as a *url.Error whose text carries the whole request
//     URL — S3_ENDPOINT, the bucket, the key layout — and the store classifies
//     itself so this package never has to recognise those shapes;
//   - otherwise the message with paths and bucket coordinates redacted.
//
// Anything that does not classify itself therefore keeps the redacted text:
// the leak is closed by default, never by enumerating what would have leaked.
// The full error goes to the WARN/ERROR line, where only an operator reads it.
func publicReason(err error, stage, dir string, cfg config.Config) string {
	msg := err.Error()
	var pe *transcode.ErrPermanent
	if errors.As(err, &pe) {
		msg = pe.Err.Error()
	}
	if class := store.PublicClass(err); class != "" {
		msg = class
	} else {
		msg = redactPaths(msg, dir, cfg)
	}
	if stage != "" {
		msg = stage + ": " + msg
	}
	if len(msg) > maxReasonLen {
		msg = msg[:maxReasonLen]
	}
	return msg
}

// redactPaths strips absolute scratch paths from a failure message before it
// becomes the public failure_reason (§8.2, exposed on GET /api/clips/{id} to
// any clip-id holder): ffmpeg/ffprobe stderr routinely echoes the input path
// ($SCRATCH_DIR/{id}/source.ext) and the scratch layout, which no viewer
// needs. The clip's dir (and, defensively, the scratch root) is replaced by a
// placeholder; the full stderr stays in the server logs for the operator. An
// empty needle (unset in a bare config) is skipped so it can never blank the
// whole message.
//
// The bucket coordinates — S3_ENDPOINT, S3_BUCKET, S3_KEY_PREFIX — are struck
// out for the same reason and as the SECOND belt behind publicReason's class
// (§20 erratum 90): the class closes the leak for every error the store
// builds, and this closes it for a message that names them some other way.
func redactPaths(msg, dir string, cfg config.Config) string {
	for _, s := range []string{dir, cfg.ScratchDir, cfg.S3.Endpoint, cfg.S3.Bucket, cfg.S3.KeyPrefix} {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "…")
		}
	}
	return msg
}

// exitCtx is the fresh, bounded context for the exit path (the run ctx is
// typically already cancelled).
func (j *jobRun) exitCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(j.ctx), exitBudget)
}

// release is the Cancel/Stop path: ReleaseJob (counters untouched), this
// attempt's uploads deleted when there is no checkpoint, stale scratch outputs
// removed, the source kept.
//
// The deletes used to happen only on reasonShutdown, on the argument that a
// CANCELLED clip's prefix is deleted by lifecycle.Destroy a moment later
// anyway. Two things are wrong with that (§20 erratum 305): it makes this run's
// own uploads somebody else's problem when the promise §10.1 makes is that a
// finished run holds nothing, and the somebody else deletes by LISTING, which
// is exactly the thing that cannot be trusted to see an object this run wrote a
// millisecond ago. Every other kind's release() has always deleted whatever the
// reason (trim, gif, social, poster, full, filmstrip); this one now agrees with
// them.
func (j *jobRun) release() {
	why := j.run.reason()
	if why == reasonNone {
		why = reasonShutdown
	}
	// The local DB write first, so a slow bucket can never leave the job
	// running (which the next boot would count as a crash).
	ctx, cancel := j.exitCtx()
	err := j.w.deps.DB.ReleaseJob(ctx, j.job.ID, db.ClaimStamp(j.job), j.clip.ID, j.w.now().Unix())
	cancel()
	switch {
	case err == nil:
		j.log.Info("job released", "reason", why.String(), "stage", j.stage, "checkpointed", j.checkpointed)
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		// The row is not this attempt's any more (§20 erratum 293): cancelled
		// under us, or repaired and perhaps re-claimed. Releasing it would put
		// a running attempt's job back on the queue, so nothing is written and
		// this attempt discards what it made instead.
		j.lostAttempt(err)
		return
	default:
		j.log.Error("release job", "reason", why.String(), "err", err)
	}
	if !j.checkpointed {
		dctx, dcancel := j.exitCtx()
		j.deleteKeys(dctx, j.attempted)
		dcancel()
	}
	j.removeScratch(false)
	j.setProgress(Progress{State: db.ClipQueued, Stage: db.JobStageQueued}, true)
}

// fail is the FailAttempt path for the three bad-ending classes. It takes the
// CAUSE, not a rendered string: `reason` is the bounded public text and
// `detail` is the whole error, and the split is the point — a storage failure
// tells a viewer only its class while the operator's log keeps the endpoint,
// the key and minio-go's own words (§20 erratum 90).
func (j *jobRun) fail(kind db.FailKind, cause error) {
	reason, detail := j.reason(cause), cause.Error()
	ctx, cancel := j.exitCtx()
	defer cancel()
	outcome, err := j.w.deps.DB.FailAttempt(ctx, j.job.ID, db.ClaimStamp(j.job), j.clip.ID, reason, kind, j.w.lim, j.w.now().Unix())
	switch {
	case errors.Is(err, db.ErrStateMismatch), errors.Is(err, db.ErrNotFound):
		// The row is not this attempt's to fail (§20 erratum 293): the clip was
		// tombstoned and MarkDeleting cancelled the job, or a repair took the
		// row and a newer attempt may hold it — and an attempt recorded there
		// spends that attempt's counter for this attempt's ending.
		j.log.Info("attempt not recorded; the job row is no longer this attempt's", "reason", reason, "detail", detail)
		j.lostAttempt(err)
		return
	case err != nil:
		j.log.Error("fail attempt", "kind", kindName(kind), "reason", reason, "detail", detail, "err", err)
		if !j.checkpointed {
			j.deleteKeys(ctx, j.attempted)
		}
		j.removeScratch(false)
		return
	}
	if !j.checkpointed {
		j.deleteKeys(ctx, j.attempted)
	}
	if outcome == db.OutcomeFailed {
		j.removeScratch(true)
		j.progressTerminal(db.ClipFailed, reason)
		switch {
		case kind == db.FailPermanent && strings.Contains(reason, "bad_output"):
			// A verify mismatch on our own encoder output cannot happen by
			// design (§12) — it needs a human.
			j.log.Error("clip failed: bad_output from our own encoder", "reason", reason, "detail", detail)
		case kind == db.FailPermanent:
			j.log.Info("clip failed", "reason", reason, "detail", detail)
		default:
			j.log.Warn("clip failed: retry cap reached", "kind", kindName(kind), "reason", reason, "detail", detail)
		}
		return
	}
	j.removeScratch(false)
	j.setProgress(Progress{State: db.ClipQueued, Stage: db.JobStageQueued}, true)
	j.log.Warn("attempt failed; requeued", "kind", kindName(kind), "reason", reason, "detail", detail)
}

func kindName(k db.FailKind) string {
	switch k {
	case db.FailPermanent:
		return "permanent"
	case db.FailCrash:
		return "crash"
	case db.FailTransient:
		return "transient"
	}
	return "unknown"
}

// deleteKeys removes the given object keys best-effort. Never called with a
// key under original/.
func (j *jobRun) deleteKeys(ctx context.Context, keys []string) {
	deleteKeys(ctx, j.w.deps.Store, j.log, keys)
}

// deleteKeys is the free function the transcode pipeline, the trim, the gif
// and the social runs use. The `original/` refusal is a hard interlock, not a
// comment: the upload is sacred (§11), and the only other code path allowed to
// remove one is lifecycle.Destroy deleting the whole clip prefix — plus, since
// ROADMAP #31, deleteReplacedKeys below, under interlocks of its own.
//
// It removes each key BY KEY (store.DeleteKey), which is the whole of §20
// erratum 305. It used to say DeletePrefix(k) — a key is its own prefix — and
// that made every cleanup a run does of its own uploads depend on the bucket's
// LISTING having caught up with a PutFile that had returned milliseconds
// earlier. R2 lists eventually; the object stayed, the delete reported success,
// and lifecycle.Destroy — which lists too — then deleted the clip's row over
// the top of it. A key-addressed delete asks the object instead of the index,
// so what this logs is what happened.
func deleteKeys(ctx context.Context, st store.Store, log *slog.Logger, keys []string) {
	for _, k := range keys {
		if strings.Contains(k, "/original/") {
			log.Error("refusing to delete an original", "key", k)
			continue
		}
		if err := st.DeleteKey(ctx, k); err != nil {
			log.Warn("delete attempt output (orphan until clip deletion)", "key", k, "err", err)
		}
	}
}

// deleteReplacedKeys is the one other path allowed to remove an object under
// `original/` (ROADMAP #31, §20 erratum 135), and it earns that by replacing
// the blanket refusal above with two interlocks a replacement can satisfy and
// a bug cannot:
//
//   - `live` — the key the clip's row names RIGHT NOW — is never deleted,
//     whatever the list says. After the swap that is the NEW original; before
//     it, the old one. Either way, the file the clip currently IS survives.
//   - a key outside `prefix`, the clip's own, is never deleted, so a corrupt
//     job row cannot reach another clip's objects.
//
// Both refusals log at ERROR: they describe states this program is not
// supposed to be able to reach. The upload is still sacred — it is the
// CURRENT upload that is sacred, and a replacement is how that one changes.
func deleteReplacedKeys(ctx context.Context, st store.Store, log *slog.Logger, keys []string, prefix, live string) {
	for _, k := range keys {
		switch {
		case k == "":
			continue
		case k == live:
			log.Error("refusing to delete the object the clip references", "key", k)
			continue
		case prefix != "" && !strings.HasPrefix(k, prefix):
			log.Error("refusing to delete a key outside the clip's prefix", "key", k, "prefix", prefix)
			continue
		}
		if err := st.DeleteKey(ctx, k); err != nil {
			log.Warn("delete superseded object (orphan until clip deletion)", "key", k, "err", err)
		}
	}
}

// removeScratch removes the whole clip dir (all=true: finalize success,
// permanent failure) or only this attempt's outputs (the source stays for
// the retry). Errors are WARN and never affect state.
func (j *jobRun) removeScratch(all bool) {
	if all {
		if err := os.RemoveAll(j.dir); err != nil {
			j.log.Warn("remove scratch dir", "dir", j.dir, "err", err)
		}
		return
	}
	for _, p := range []string{j.outPath, j.lightPath, j.posterPath, j.probePath} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			j.log.Warn("remove scratch output", "path", p, "err", err)
		}
	}
}

// setStage moves to a new stage: percent to the band floor (monotone), a
// forced registry write and the one DB write per stage.
func (j *jobRun) setStage(stage string) {
	j.stage = stage
	if lo := bands[stage][0]; lo > j.percent {
		j.percent = lo
	}
	// `encode` is the one instrumented stage, and §10.3 makes it
	// indeterminate for a source with no duration — FROM THE START, not from
	// the first -progress block. j.plan is written by probeSource, which is
	// the step before this one, so the answer is already known here; leaving
	// Indeterminate at its zero value published an unsized clip as a
	// DETERMINATE band floor (5 %) for the width of an ffmpeg spawn, and a
	// poller that caught that window drew a progress bar it then had to take
	// away. Every other stage is a fixed floor and has nothing to be
	// indeterminate about. (A trim's own setStage needs no equivalent: its
	// plan duration is the requested window, which is positive by the time a
	// job exists — erratum 78 fails an unhonourable window at the client.)
	j.setProgress(Progress{State: db.ClipProcessing, Stage: stage, Percent: j.percent,
		Indeterminate: stage == stageEncode && !j.plan.HasDuration}, true)
	err := j.w.deps.DB.SetJobStage(j.ctx, j.job.ID, db.ClaimStamp(j.job), stage, j.w.now().Unix())
	switch {
	case err == nil, j.ctx.Err() != nil:
	case errors.Is(err, db.ErrNotFound):
		j.log.Info("job row gone while setting stage", "stage", stage)
	case errors.Is(err, db.ErrStateMismatch):
		// The row is no longer this attempt's — repaired or cancelled under
		// another hand (§20 erratum 291). The stage is abandoned, the run goes
		// on: the next write that matters (the checkpoint, the commit) is where
		// the lost row ends it, and never as a failure of the row's new owner.
		j.log.Info("job row is no longer this attempt's; stage not recorded", "stage", stage)
	default:
		j.log.Error("set job stage", "stage", stage, "err", err)
	}
	j.log.Debug("stage", "stage", stage, "percent", j.percent)
}

// progress maps a stage fraction (clamped to [0, 1]) onto the band
// (rate-limited registry write). Indeterminate holds the band floor.
func (j *jobRun) progress(frac float64, indeterminate bool) {
	b := bands[j.stage]
	pct := b[0]
	if !indeterminate {
		frac = math.Min(math.Max(frac, 0), 1)
		pct = int(math.Floor(float64(b[0]) + frac*float64(b[1]-b[0])))
	}
	pct = min(max(pct, 0), 100)
	if pct > j.percent {
		j.percent = pct
	}
	j.setProgress(Progress{State: db.ClipProcessing, Stage: j.stage, Percent: j.percent, Indeterminate: indeterminate}, false)
}

// safeProgress is the progress callback handed to the runner and the
// store. It runs on their goroutines (ffmpeg's stdout copier, the upload
// loop), outside the job's recover, so it shields the process itself.
func (j *jobRun) safeProgress(frac float64, indeterminate bool) {
	defer func() {
		if p := recover(); p != nil {
			j.panicOnce.Do(func() {
				j.log.Error("progress callback panicked", "stage", j.stage, "err", fmt.Sprint(p), "stack", string(debug.Stack()))
			})
		}
	}()
	j.progress(frac, indeterminate)
}

// setProgress is every registry write a run makes. It runs under worker.mu,
// which Forget also holds while it evicts the entry and voids this run, so a
// write can never straddle a Forget: once a retry has declared this run's
// view of the clip obsolete, the run resurrects nothing (§20 erratum 49).
func (j *jobRun) setProgress(p Progress, force bool) {
	j.w.mu.Lock()
	defer j.w.mu.Unlock()
	if j.run.progressVoid {
		return
	}
	j.w.prog.set(j.clip.ID, p, force)
}

// progressTerminal records ready/100 or failed with its error (forced).
func (j *jobRun) progressTerminal(state, errText string) {
	p := Progress{State: state, Stage: stageDone, Percent: 100}
	if state == db.ClipFailed {
		p.Stage, p.Percent, p.Error = j.stage, j.percent, errText
	}
	j.setProgress(p, true)
}
