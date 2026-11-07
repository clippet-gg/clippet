// Package worker runs the DB-driven transcode job loop with an in-memory
// progress registry, blocking Cancel and graceful Stop (DESIGN.md §10.1,
// §18.3).
//
// Lifecycle contract: Start(ctx) spawns the loop goroutines and RETURNS
// immediately; the loop stops claiming when ctx is cancelled; Stop(ctx)
// cancels every run, waits for them (bounded by ctx) and returns. Cancel
// (ctx, clipID) is synchronous: it returns true only after that clip's job
// goroutine has exited — ffmpeg reaped, no PutFile in flight, no scratch I/O,
// and every object that attempt wrote removed by key (§20 erratum 305) — and
// false when ctx ended first, which is the caller's signal that the clip's
// objects are still somebody's (AUDIT #13).
//
// Log-level doctrine (§12): cancellation, shutdown, lost races and
// permanent transcode failures are INFO; storage/scratch/DB trouble that is
// retried is WARN; panics, DB errors, row/object drift and verify
// mismatches on our own output are ERROR.
//
// Hooks.OnReady runs after the run has left the registry, under its own
// budget and aborted by shutdown, so a slow hook never holds Cancel/Stop.
//
// Testing: a repeated race run of this package (`go test -race -count=N`)
// needs `-timeout 0` (or a generous one): the package's tests are many and
// the default ten-minute budget covers the WHOLE run, so a large N is killed
// by the test binary's own timer rather than by a failure.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/notify"
	"clippet/internal/store"
	"clippet/internal/transcode"
)

// Progress is one registry entry (§8.4 fields).
type Progress struct {
	State, Stage  string
	Percent       int
	Indeterminate bool
	Error         string
	UpdatedAt     time.Time
}

// Worker is what httpapi, lifecycle and serve.go depend on.
type Worker interface {
	Start(ctx context.Context)
	Stop(ctx context.Context) error
	Wake()
	Progress(clipID string) (Progress, bool)
	// TrimProgress is Progress for the clip's live EDIT — its `trim` job
	// (§17 #5), or its `replace` job (ROADMAP #31), which share this map
	// because at most one run per clip is ever live. It is a SECOND
	// registry, not an entry in the first, and deliberately so: §8.4's
	// registry is the source of truth for GET …/progress, whose subject is
	// the clip's own state, and a clip being edited is `ready` the whole
	// time. Writing an encode percentage there would make a ready clip poll
	// as 40%-processing.
	TrimProgress(clipID string) (Progress, bool)
	// AwaitTerminal returns a channel closed when the clip's transcode
	// reaches `ready` or `failed` — already closed if it is there
	// already — plus the deregister the caller must always run. It is the
	// wake-up behind CAPTURE_READY_WAIT (§20 erratum 97): POST /api/upload
	// holds its 201 on it so the link a capture tool pastes unfurls as a
	// player card instead of the link-out card Discord would then cache.
	// A clip this process is not running never signals; the caller's own
	// budget is what ends the wait.
	AwaitTerminal(clipID string) (<-chan struct{}, func())
	// Busy reports whether this process has a registered run for the clip —
	// a transcode, a trim, a gif or a poster — and therefore owns $SCRATCH_DIR/{id}
	// right now. The scratch janitor asks before removing a dir (§12, §20
	// erratum 89); nothing else may act on it, because it is a fact about
	// this instant and this process only.
	Busy(clipID string) bool
	// Cancel stops the clip's run and WAITS for its goroutine to exit, so a
	// caller that is about to delete the clip's objects knows nothing can
	// still write one. It returns false when ctx ended before the run did:
	// the caller must then delete NOTHING and leave the tombstone (AUDIT #13).
	Cancel(ctx context.Context, clipID string) bool
	Forget(clipID string)
}

// Hooks are the Phase 4 extension points (no-ops in Phase 1).
type Hooks struct {
	OnReady func(ctx context.Context, c db.Clip)
}

// Deps wires the worker. Runner nil = real ffmpeg; tests inject a fake.
// FreeBytes nil = the platform statfs (tests inject a small disk).
type Deps struct {
	DB     *db.DB
	Store  store.Store
	Cfg    config.Config
	Hooks  Hooks
	Log    *slog.Logger
	Now    func() time.Time
	Runner Runner

	// Notifier is the Discord auto-post client (§17 #4), or nil when
	// DISCORD_WEBHOOK_URL is unset. Nil is not just "do not deliver": it is
	// also what stops FinalizeTranscode from ever enqueuing a notify job, so
	// with the variable unset nothing is posted to Discord and no queue entry
	// implies otherwise (§15).
	Notifier *notify.Notifier

	// Hook is the clip webhook client (ROADMAP #54, §20 erratum 319), or nil
	// when CLIP_WEBHOOK_URL is unset. Nil is also what stops
	// FinalizeTranscode from ever enqueuing a `hook` job.
	Hook *notify.Hook

	// CardReady is told when a clip's card has just become ready (db.CardReady
	// turned true, §20 erratum 330): the Discord bot's Bot.CardReady in
	// production, so a link it posted before the card worked is redrawn. nil —
	// no bot — is a no-op that reads nothing. It must not block: the worker
	// calls it on its own goroutine (cardReady, below).
	CardReady func(clipID string)

	// FreeBytes reports the bytes available on the filesystem holding dir
	// for the §10.2 scratch check. A negative value means "unknown" and
	// skips the check.
	FreeBytes func(dir string) (int64, error)

	// Repairs is where the automatic stuck-job repair publishes its lifetime
	// counters for /healthz (§20 erratum 231). nil = nobody is listening,
	// which is every test that does not care; the repair itself still runs.
	Repairs *Repairs

	// OnSettled is told that a job has SETTLED: everything this process will
	// ever do for that one claim — the row, the counters, the log lines, the
	// card-ready call, the OnReady hook — is behind it (§20 erratum 331). It is
	// the seam a test waits on instead of polling the database, because a row is
	// committed FIRST and the effects a test then reads (a WARN line, a repair
	// counter) land after it. nil in production: serve.go never sets it, and
	// nil is a no-op that reads nothing.
	//
	// It is called once per run of a claimed job, of every kind (runJob is the
	// one door), and once per row the automatic repair settled, after that
	// pass's own summary line. It must not block: it runs on the worker's own
	// goroutine, like CardReady; a panic in it is one log line.
	OnSettled func(Settled)
}

// Settled names one job OnSettled reports.
type Settled struct {
	JobID  int64
	ClipID string
	Kind   string
	// Repair is true when the automatic stuck-job repair settled the row
	// (repairOnce) rather than a run of this process.
	Repair bool
}

// Runner abstracts the ffmpeg-touching stages so the pipeline is testable.
type Runner interface {
	Probe(ctx context.Context, path string) (*transcode.ProbeResult, error)
	// Encode writes at most maxOutputBytes (§20 erratum 46); 0 = uncapped.
	Encode(ctx context.Context, p transcode.Plan, in, out string, maxOutputBytes int64, onProgress func(float64, bool)) error
	ProbeOutput(ctx context.Context, path string) (*transcode.OutputInfo, error)
	Verify(path string) error
	// Poster takes the card image; videoEndSec is where the PICTURE stops
	// (0 = unmeasured), which is what the seek is clamped against. `box` is the
	// frame the picture is written inside (§20 erratum 219): its zero value is
	// "the size it was decoded at", and a run that reads an UNTAGGED file bigger
	// than the card wants passes the LIGHT plan's box instead.
	Poster(ctx context.Context, in, out string, durUS int64, has bool, videoEndSec float64,
		box transcode.PosterBox) (transcode.PosterResult, error)
	// PosterFrameSeconds reports where the poster's rule WOULD land on this
	// file without writing one. It is how a job that has no stored poster
	// timestamp — the gif job — starts its preview at the frame the card
	// image came from (§20 erratum 92(c), amended). It cannot fail: an
	// unreadable answer is the plain seek.
	PosterFrameSeconds(ctx context.Context, in string, durUS int64, has bool, videoEndSec float64) float64
	// PosterAt takes the ONE frame an owner CHOSE, at `seconds` on the given
	// file's own timeline. It is Poster without the search: no thumbnail
	// pass, no seek ladder and no near-black rejection, because a fallback
	// would publish a DIFFERENT frame under the answer "your frame was used"
	// (ROADMAP #5, §20 erratum 129). A seek past the last picture comes back
	// as transcode.ErrNoFrameAt — a request and a file that disagree, which
	// is the client's business and never this server's.
	//
	// `durMS` is the CLIP's duration, for the label burned into the frame's
	// top-right corner (§20 erratum 287) — the clip's and not this moment's,
	// because a frame chosen at 0:03 of a 0:42 clip is still a frame of a 0:42
	// clip. 0 means "not measured" and draws nothing.
	PosterAt(ctx context.Context, in, out string, seconds float64, durMS int64) (transcode.PosterResult, error)

	// The three §17 #5 additions. Keyframes is asked for only when a stream
	// copy is otherwise possible (it costs a demux of the whole file);
	// EncodeTrim is Encode restricted to one window of the input; CopyTrim
	// remuxes that window without re-encoding a frame.
	Keyframes(ctx context.Context, path string, videoIndex int) ([]float64, error)
	EncodeTrim(ctx context.Context, p transcode.Plan, win transcode.Window, in, out string, maxOutputBytes int64, onProgress func(float64, bool)) error
	CopyTrim(ctx context.Context, t transcode.TrimPlan, p *transcode.ProbeResult, in, out string) error

	// The §17 #6 pair: the two palette passes that render the preview GIF,
	// and the frame-counting probe that proves the result animates. Both
	// carry their own bounds (GIFMaxBytes, GIFHardTimeout) inside the runner
	// — a preview is never sized by the caller.
	GIF(ctx context.Context, p transcode.GIFPlan, in, palette, out string) error
	ProbeGIF(ctx context.Context, path string) (*transcode.GIFInfo, error)

	// Filmstrip is the trim scrubber's sprite sheet (§20 erratum 220): one
	// pass, one JPEG, and the MEASUREMENTS of what it produced. It measures
	// rather than merely writes because the client positions tiles from those
	// numbers, so the pass and the verify are the same call — there is nothing
	// a caller could do with an unmeasured sheet.
	Filmstrip(ctx context.Context, p transcode.FilmstripPlan, in, out string) (transcode.FilmstripResult, error)

	// EncodeSocial is the §17 #3 encode: the same instrumented pass as
	// Encode, over the plan the social job built (frame rate capped at 30),
	// with this package's card settings instead of the operator's — CRF 26, a
	// 2.5 Mbps VBV ceiling, 96k audio. It is a method of its own rather than
	// an option on Encode because those settings are the FEATURE and not a
	// caller's preference: nothing outside the social job may choose them.
	// `tag` is the LIGHT tier's burned-in corner line (§20 erratum 214): every
	// producer of that file passes it, because the tag is a property of the
	// TIER and not of the job that happened to make it.
	EncodeSocial(ctx context.Context, p transcode.Plan, in, out string, tag transcode.LightTag,
		maxOutputBytes int64, onProgress func(float64, bool)) error

	// The quality tiers' two additions (§20 errata 200–204). EncodeFull is
	// Encode with the FULL tier's own quality floor in FULL_CODEC's encoder —
	// its own method for EncodeSocial's reason exactly: the settings are the
	// tier, not a caller's preference. Remux rewrites a playable original's
	// container with `-c copy`, which is the cheapest thing this interface can
	// be asked to do and the reason a 1440p60 upload is ready in seconds.
	EncodeFull(ctx context.Context, p transcode.Plan, in, out string, maxOutputBytes int64, onProgress func(float64, bool)) error
	Remux(ctx context.Context, p *transcode.ProbeResult, in, out string) error

	// EncodeTiers is the two-output pass of §20 erratum 217: ONE ffmpeg that
	// decodes the source once and writes the FULL tier and the LIGHT copy
	// together. It exists because both alternatives were measured and both are
	// worse — two processes at once put the first file 2.2× LATER than encoding
	// it alone (they decode the same source twice), and back to back costs
	// 1.6–1.9× the Full alone where this costs 1.05–1.42×.
	//
	// maxOutputBytes bounds the two files TOGETHER (ffmpeg's progress
	// `total_size` is not a per-output figure); the caller still weighs each
	// file against its own budget afterwards. `tag` is burned into the LIGHT
	// copy alone.
	EncodeTiers(ctx context.Context, full, light transcode.Plan, in, fullOut, lightOut string,
		tag transcode.LightTag, maxOutputBytes int64, onProgress func(float64, bool)) error

	// LightTagFor is the corner line this instance would burn into one clip's
	// LIGHT copy, written into `dir` and returned as the pair drawtext reads
	// (§20 erratum 214). A zero LightTag — no LIGHT_TAG_TEXT, no font in the
	// image, no drawtext in the build — draws nothing and is never an error: the
	// line is cosmetic and an encode is not.
	LightTagFor(dir, name, lightQuality, bestQuality string) (transcode.LightTag, error)

	// The audio clip's four (§20 erratum 328). AudioFull writes the Full the
	// AudioPlan names (FLAC, 24-bit FLAC with `gainDB` applied, or AAC 256k);
	// AudioLight writes the card copy — the audiogram, the waveform `stencil`
	// over a sliding amber field under the sound, H.264 + AAC (or the AAC as it
	// is, `copyAudio`), then a tag-dropping copy pass; AudioPeak is the source's overall sample
	// peak in dBFS; SamePCM is the lossless proof, an ErrPermanent when the two
	// files' decoded samples differ.
	AudioFull(ctx context.Context, p *transcode.ProbeResult, plan transcode.AudioPlan, gainDB float64, in, out string, maxOutputBytes int64) error
	AudioLight(ctx context.Context, p *transcode.ProbeResult, stencil, in, out string, bitrateK int, copyAudio bool, maxOutputBytes int64) error
	AudioPeak(ctx context.Context, in string) (float64, error)
	// The waveform's two (§20 errata 329, 332). WavePeaks reads each bar's loudness
	// from one streamed decode, `n` bars quantised to 0..255; AudioPoster draws
	// the poster PNG from them (with `mark`, the manifest icon, in its corner)
	// and burns the duration into it as every poster's (erratum 287).
	WavePeaks(ctx context.Context, in string, p *transcode.ProbeResult, n int) ([]uint8, error)
	AudioPoster(ctx context.Context, heights []uint8, mark []byte, out string, durMS int64) (transcode.PosterResult, error)
	SamePCM(ctx context.Context, a, b string) error
}

// Loop timing.
const (
	// pollInterval is the fallback poll when nothing is claimable, so a
	// run_after backoff expiring is noticed without a Wake (§10.1).
	pollInterval = 5 * time.Second
	// claimErrorPause spaces retries after a failing ClaimJob.
	claimErrorPause = 5 * time.Second
)

// reason records why a run's context was cancelled (§10.1).
type reason int32

const (
	reasonNone      reason = iota
	reasonCancelled        // Cancel(clipID): the clip is being destroyed
	reasonShutdown         // Stop(ctx) or the Start ctx: release the job
)

func (r reason) String() string {
	switch r {
	case reasonCancelled:
		return "cancelled"
	case reasonShutdown:
		return "shutdown"
	}
	return "none"
}

// run is one entry of the run registry.
type run struct {
	jobID  int64
	clipID string
	cancel context.CancelFunc
	done   chan struct{}
	why    atomic.Int32

	// progressVoid is set by Forget while this run is still registered: the
	// caller has just declared everything this run has to say about the
	// clip's progress obsolete (a retry requeued it), so the run's remaining
	// registry writes are dropped instead of resurrecting the entry Forget
	// evicted (§20 erratum 49). Guarded by worker.mu, which is also held
	// across the eviction and across every one of those writes, so the
	// check and the write cannot straddle a Forget.
	progressVoid bool
}

func (r *run) setReason(why reason) { r.why.CompareAndSwap(int32(reasonNone), int32(why)) }
func (r *run) reason() reason       { return reason(r.why.Load()) }

type worker struct {
	deps   Deps
	log    *slog.Logger
	now    func() time.Time
	runner Runner
	free   func(string) (int64, error)
	lim    db.Limits
	prog   *registry
	// trimProg is the §17 #5 registry: same type, same policy, disjoint
	// keyspace-in-practice (a clip has at most one live trim), and never
	// read by GET …/progress.
	trimProg *registry

	// wake nudges the transcode loops, wakeMedia the media loop (trim, gif)
	// and wakeNotify the notify loop. They are separate channels because Wake
	// must reach EVERY class of loop and a buffered channel of one wakes
	// exactly one waiter (§20 erratum 67, amended).
	wake       chan struct{}
	wakeMedia  chan struct{}
	wakeNotify chan struct{}

	// sem is the ffmpeg slot pool: TRANSCODE_CONCURRENCY tokens, held by a
	// transcode for its whole run and by the media kinds around their ffmpeg
	// stages only. It is what makes the operator-facing promise true — a
	// default install runs ONE ffmpeg — now that a second class of work runs
	// ffmpeg on a loop of its own (§20 erratum 79).
	sem chan struct{}

	mu   sync.Mutex
	runs map[string]*run
	// live is every JOB this process has claimed and not yet finished, of
	// every kind (repair.go): the evidence the automatic stuck-job repair
	// needs and the one thing an age test cannot know. `runs` cannot serve —
	// it is keyed by clip and holds only the kinds that register a run.
	live map[int64]struct{}
	// waiting counts, per clip, the runs parked in withRun behind a
	// predecessor of the same clip: claimed, about to register, not yet in
	// `runs`. cancelling counts the Cancel calls in progress per clip, and a
	// run that registers while one is in progress starts cancelled. regCh is
	// closed (and replaced) on every registration, so a Cancel that finds a
	// parked run between its predecessor's exit and its own registration can
	// wait for it with its ctx still bounding the wait (§20 erratum 317(b)).
	waiting    map[string]int
	cancelling map[string]int
	regCh      chan struct{}
	// parkedHook, nil outside tests, is called (without worker.mu) each time a
	// run parks behind a predecessor: the one place a test can learn that a
	// successor is waiting without sleeping for it.
	parkedHook func(clipID string)
	stopping   bool
	stopCh     chan struct{}
	stopOnce   sync.Once
	wg         sync.WaitGroup
}

// New returns the real worker. Nothing runs until Start.
func New(d Deps) Worker {
	w := newWorker(d)
	if w.log == nil {
		w.log = slog.Default()
	}
	if w.now == nil {
		w.now = time.Now
	}
	if w.runner == nil {
		w.runner = NewFFmpegRunner(d.Cfg.Transcode, d.Cfg.LightTag)
	}
	if w.free == nil {
		w.free = freeBytes
	}
	w.prog = newRegistry(w.now)
	w.trimProg = newRegistry(w.now)
	return w
}

// newWorker is the worker with every map and channel made and NOTHING
// defaulted: the log, the clock, the runner and the disk probe stay whatever
// d says (nil included). New is it plus those defaults and the two progress
// registries; a test that drives one method of a bare worker builds it here,
// so a map added to the struct later is never nil in a test that forgot it.
func newWorker(d Deps) *worker {
	return &worker{
		deps:   d,
		log:    d.Log,
		now:    d.Now,
		runner: d.Runner,
		free:   d.FreeBytes,
		lim:    db.Limits{MaxAttempts: d.Cfg.Transcode.MaxAttempts, MaxTransient: d.Cfg.Transcode.MaxTransient},
		wake:   make(chan struct{}, 1),
		runs:   map[string]*run{},
		live:   map[int64]struct{}{},
		stopCh: make(chan struct{}),

		waiting:    map[string]int{},
		cancelling: map[string]int{},
		regCh:      make(chan struct{}),

		wakeMedia:  make(chan struct{}, 1),
		wakeNotify: make(chan struct{}, 1),
		sem:        make(chan struct{}, max(1, d.Cfg.Transcode.Concurrency)),
	}
}

// Start spawns TRANSCODE_CONCURRENCY transcode loops plus one MEDIA loop and
// one NOTIFY loop, and returns.
//
// The extra loops are not extra workers; they are extra QUEUES (§20 errata 67
// and 79), partitioned by what the work COSTS:
//
//   - `transcode` — the encode a clip cannot exist without. Its loops are the
//     ones TRANSCODE_CONCURRENCY names.
//   - `media` (trim, gif, poster) — ffmpeg against a clip that is already `ready`.
//     One loop, because these are edits nobody is blocked on, and their ffmpeg
//     stages take a slot from the SAME semaphore the transcodes hold: a
//     default install runs one ffmpeg, whatever mix of work is queued.
//   - `notify` — an HTTP call with a 10s per-attempt budget, and everything
//     else the kind list ever grows. Network only, so it needs no slot; it
//     needs to not be stuck behind three minutes of somebody's trim.
//
// Sharing one FIFO cost the encode: a blackholed Discord occupied 3 × 10s of
// the ONLY loop that TRANSCODE_CONCURRENCY=1 has, per ready clip. Sharing the
// leftover loop between trims and notifies then cost the announcements the
// same way. The three claims are disjoint by kind, so no loop can take
// another's work, and the notify loop claims by EXCLUSION so an unrecognised
// kind is still claimed and failed loudly rather than left queued for ever.
func (w *worker) Start(ctx context.Context) {
	if w.deps.DB == nil || w.deps.Store == nil {
		w.log.Error("worker not started: DB and Store are required")
		return
	}
	n := max(1, w.deps.Cfg.Transcode.Concurrency)
	for i := 0; i < n; i++ {
		w.wg.Add(1)
		go w.loop(ctx, i)
	}
	w.wg.Add(1)
	go w.mediaLoop(ctx)
	w.wg.Add(1)
	go w.notifyLoop(ctx)
	w.startRepair(ctx)
}

// startRepair spawns the automatic stuck-job repair loop, or says why it did
// not. Either way it logs ONE line naming JOBS_UNSTICK_AFTER: an operator
// reading a boot log should never have to work out whether the app will repair
// its own queue (ROADMAP #12, §20 erratum 230).
func (w *worker) startRepair(ctx context.Context) {
	plan := w.deps.Cfg.UnstickPlan()
	if !plan.On() {
		w.log.Info("automatic stuck-job repair is off (JOBS_UNSTICK_AFTER=0): a job left running with no worker waits for a restart or for `clippet admin jobs unstick`")
		return
	}
	attrs := []any{"after", plan.After, "every", plan.Every, "transcode_hard_timeout", plan.HardTimeout}
	if plan.Raised() {
		attrs = append(attrs, "configured", plan.Configured, "raised_to_floor", true)
	}
	w.log.Info("automatic stuck-job repair is on (JOBS_UNSTICK_AFTER)", attrs...)
	if plan.UnderHardTimeout() {
		// The one thing an operator can get wrong here, and it is not wrong for
		// THIS process: a job this worker has claimed is never repaired, at any
		// age. It is wrong for a SECOND process on the same database, whose live
		// job this one cannot see and would repair out from under it. One WARN,
		// at boot, on the COMBINATION — the same shape as the TRUSTED_PROXIES
		// line (§20 erratum 96).
		w.log.Warn("JOBS_UNSTICK_AFTER is shorter than TRANSCODE_HARD_TIMEOUT: a job THIS process runs is never repaired whatever its age, but a live job of a SECOND process sharing this database could be - set JOBS_UNSTICK_AFTER=0 there (DEPLOY.md §24.6)",
			"after", plan.After, "transcode_hard_timeout", plan.HardTimeout)
	}
	w.wg.Add(1)
	go w.repairLoop(ctx, plan)
}

// loop claims and runs transcode jobs until ctx is done or Stop is called.
func (w *worker) loop(ctx context.Context, id int) {
	w.claimLoop(ctx, w.log.With("loop", id), w.deps.DB.ClaimJob, w.wake)
}

// mediaLoop is the same loop over the ffmpeg kinds that run on a ready clip.
func (w *worker) mediaLoop(ctx context.Context) {
	w.claimLoop(ctx, w.log.With("loop", "media"), w.deps.DB.ClaimMediaJob, w.wakeMedia)
}

// notifyLoop is the same loop over everything else.
func (w *worker) notifyLoop(ctx context.Context) {
	w.claimLoop(ctx, w.log.With("loop", "notify"), w.deps.DB.ClaimNotifyJob, w.wakeNotify)
}

// acquireFFmpeg takes one of the TRANSCODE_CONCURRENCY ffmpeg slots, blocking
// until one is free or ctx is done, and returns the release. The release is
// idempotent, so a caller may both `defer` it and call it early to give the
// slot back before its uploads.
//
// Every path that spawns ffmpeg goes through here: the transcode holds a slot
// for its whole run (unchanged behaviour — there are exactly as many slots as
// transcode loops), and trim and gif hold one only around their ffmpeg stages,
// giving it back before they upload. That is what bounds the number of
// simultaneous ffmpeg processes to the number an operator configured, rather
// than to the number of loops the worker happens to have.
func (w *worker) acquireFFmpeg(ctx context.Context, log *slog.Logger) (func(), error) {
	select {
	case w.sem <- struct{}{}:
		return sync.OnceFunc(func() { <-w.sem }), nil
	default:
	}
	log.Info("waiting for an ffmpeg slot", "slots", cap(w.sem))
	select {
	case w.sem <- struct{}{}:
		return sync.OnceFunc(func() { <-w.sem }), nil
	case <-ctx.Done():
		return func() {}, ctx.Err()
	case <-w.stopCh:
		return func() {}, errStopping
	}
}

// errStopping ends a wait for an ffmpeg slot that Stop will never grant. It
// is not a failure of the job: every run that sees it takes the release path,
// exactly as it would for a cancelled context (the two arrive together, in an
// order Stop does not promise).
var errStopping = errors.New("worker is stopping")

// claimFunc is one half of the kind partition of the job queue.
type claimFunc func(ctx context.Context, now int64) (db.Job, db.Clip, bool, error)

// claimLoop claims and runs jobs until ctx is done or Stop is called.
func (w *worker) claimLoop(ctx context.Context, log *slog.Logger, claim claimFunc, wake <-chan struct{}) {
	defer w.wg.Done()
	for !w.stopped(ctx) {
		job, clip, ok, err := claim(ctx, w.now().Unix())
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			log.Error("claim job", "err", err)
			w.pause(ctx, claimErrorPause, wake)
		case !ok:
			w.pause(ctx, pollInterval, wake)
		default:
			w.runJob(ctx, job, clip)
		}
	}
}

// stopped reports whether claiming must cease.
func (w *worker) stopped(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stopping
}

// pause waits for a Wake on this loop's channel, the fallback poll interval,
// or shutdown.
func (w *worker) pause(ctx context.Context, d time.Duration, wake <-chan struct{}) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-w.stopCh:
	case <-wake:
	case <-t.C:
	}
}

// runJob dispatches one claimed job by kind. A transcode goes through the
// registered pipeline and, once the run has left the registry, fires
// Hooks.OnReady; a notify posts the ready clip to the operator's webhook
// (§17 #4) and touches no clip column at all (§5). Any other kind is refused
// — a kind this worker cannot run needs a human, not a retry.
func (w *worker) runJob(ctx context.Context, job db.Job, clip db.Clip) {
	// Deferred FIRST so it runs LAST: after the card-ready call and the untrack
	// below, after the OnReady hook, after every log line of the run (§20
	// erratum 331).
	defer w.settled(Settled{JobID: job.ID, ClipID: clip.ID, Kind: job.Kind})
	// Every claimed job, of every kind, is recorded as LIVE HERE for exactly as
	// long as this call runs — the automatic repair's proof that a row nobody
	// else can see into is being worked on (repair.go). It is here rather than
	// in each case below on purpose: a kind added later is covered by the
	// dispatch it must go through anyway.
	w.trackJob(job.ID)
	defer w.untrackJob(job.ID)
	if cardInput(job.Kind) {
		// Deferred so it runs once the job's own commit — and, for a transcode,
		// the run's exit from the registry — is behind it (§20 erratum 330).
		defer w.cardReady(ctx, clip.ID)
	}
	switch job.Kind {
	case db.KindTranscode:
		// The slot is taken INSIDE the run (runRegistered), not around it
		// (§20 erratum 101): registration first, then the semaphore, which is
		// the order a trim and a gif already use. Around it, a transcode held
		// the only slot while withRun blocked on `<-prev.done` for a
		// predecessor of the SAME clip -- a lock-ordering inversion that with
		// TRANSCODE_CONCURRENCY=1 is a wedge, held off today only by the
		// undocumented invariant that nothing moves a ready clip back to
		// queued (EnqueueTrim/EnqueueGIF require `state=ready`, so no clip is
		// ever transcode-claimable and media-claimable at once). It is still
		// held for the whole run: there are exactly as many slots as transcode
		// loops, so a transcode can only ever wait on media work.
		ready := w.runRegistered(ctx, job, clip)
		if ready {
			// The ready flip, the notify and hook enqueues and the social
			// enqueue are one commit (FinalizeTranscode), so a clip that just
			// became ready may have put work on either queue. Nudge both rather than
			// let them find the row on their next 5s poll.
			nudge(w.wakeNotify)
			nudge(w.wakeMedia)
			w.onReady(ctx, clip)
		}
	case db.KindNotify:
		w.runNotify(ctx, job, clip)
	case db.KindHook:
		// The clip webhook (§20 erratum 319): notify's twin, unregistered for
		// notify's reason — it writes no object and runs no ffmpeg.
		w.runHook(ctx, job, clip)
	case db.KindTrim:
		// REGISTERED, unlike notify: a trim runs ffmpeg, writes scratch and
		// uploads objects, which is precisely the set of things §10.1's
		// Cancel promises to have finished when it returns. An unregistered
		// trim would let lifecycle.Destroy delete the clip prefix while an
		// ffmpeg of ours was still writing into it, and the PutFile that
		// landed afterwards would be an object with no row (§11).
		w.withRun(ctx, job, clip, func(rctx context.Context, r *run) bool {
			newTrimRun(w, rctx, r, job, clip).execute()
			return false
		})
	case db.KindGIF:
		// Registered for the same reason a trim is (erratum 71): ffmpeg,
		// scratch and a PutFile are exactly what Cancel promises to have
		// finished when it returns.
		w.withRun(ctx, job, clip, func(rctx context.Context, r *run) bool {
			newGIFRun(w, rctx, r, job, clip).execute()
			return false
		})
	case db.KindSocial:
		// And once more for the §17 #3 encode, which is the same three
		// things again: ffmpeg, scratch, one PutFile.
		w.withRun(ctx, job, clip, func(rctx context.Context, r *run) bool {
			newSocialRun(w, rctx, r, job, clip).execute()
			return false
		})
	case db.KindReplace:
		// And the replacement (ROADMAP #31, erratum 135): ffmpeg, scratch and
		// two PutFiles over a clip that is `ready` throughout — the same three
		// reasons, plus one of its own. Registration is also what keeps a trim
		// of the same clip from running beside it, so the commit that lands
		// last is always the edit the owner asked for last.
		w.withRun(ctx, job, clip, func(rctx context.Context, r *run) bool {
			newReplaceRun(w, rctx, r, job, clip).execute()
			return false
		})
	case db.KindPoster:
		// The last media kind, registered for the same reason as the rest
		// (§20 erratum 129): one seek is still ffmpeg, scratch and a PutFile
		// against a prefix lifecycle.Destroy may be about to remove.
		w.withRun(ctx, job, clip, func(rctx context.Context, r *run) bool {
			newPosterRun(w, rctx, r, job, clip).execute()
			return false
		})
	case db.KindFilmstrip:
		// The scrubber's sprite sheet (§20 erratum 220), registered for the
		// same reason as every media kind before it: ffmpeg, scratch and a
		// PutFile against a prefix lifecycle.Destroy may be about to remove.
		w.withRun(ctx, job, clip, func(rctx context.Context, r *run) bool {
			newFilmstripRun(w, rctx, r, job, clip).execute()
			return false
		})
	case db.KindFull:
		// The FULL tier (§20 errata 200–206), registered for the same three
		// reasons as every media kind: ffmpeg, scratch, one PutFile. It is the
		// heaviest of them — a faithful encode of a source at its own resolution
		// — and it runs on the media loop precisely so it cannot get in front of
		// the next upload's card.
		w.withRun(ctx, job, clip, func(rctx context.Context, r *run) bool {
			newFullRun(w, rctx, r, job, clip).execute()
			return false
		})
	default:
		w.refuseKind(ctx, job, clip)
	}
}

// runRegistered registers the run, executes the pipeline and unregisters
// it, closing done (what Cancel and Stop wait on). A run registered after
// Stop began is cancelled before it starts, so it takes the release path
// immediately. Reports whether the clip became ready.
//
// At most ONE run per clip id is ever live (§20 erratum 32): fail() and
// release() commit the DB transition that lets another loop re-claim the
// job BEFORE they finish their exit path, and everything that exit path
// then touches — this attempt's objects, $SCRATCH/{id}, the progress entry
// — is keyed by the CLIP id, not by the attempt. So a successor waits here
// until its predecessor has left the registry (the unregister happens after
// execute() has fully returned), instead of having its scratch dir removed
// mid-encode and its progress overwritten with the predecessor's terminal
// entry. The wait is bounded by the predecessor's exit budget.
func (w *worker) runRegistered(ctx context.Context, job db.Job, clip db.Clip) bool {
	return w.withRun(ctx, job, clip, func(rctx context.Context, r *run) bool {
		// The ffmpeg slot, taken after registration (§20 erratum 101). An
		// error here is errStopping or a cancelled rctx, which are the same
		// thing seen twice: Stop cancels every registered run, and this run is
		// registered by now, so execute() finds its context done and takes the
		// release path -- the outcome the run itself must record. That is why
		// the error is not handled separately, exactly as in trimRun.
		release, _ := w.acquireFFmpeg(rctx, w.log.With("clip", clip.ID, "job", job.ID, "kind", job.Kind))
		defer release()
		return newJobRun(w, rctx, r, job, clip).execute()
	})
}

// withRun is that registration, without the pipeline: it publishes the run,
// waits out any predecessor for the same clip, hands body a cancellable
// context and the registry entry, and unregisters afterwards. Both kinds of
// run that touch ffmpeg go through it, so Cancel and Stop have exactly one
// definition of "this clip has a run in flight".
func (w *worker) withRun(ctx context.Context, job db.Job, clip db.Clip, body func(context.Context, *run) bool) bool {
	rctx, cancel := context.WithCancel(ctx)
	// The one WARN a run-time GPU fallback gets (§20 erratum 265). It is
	// installed HERE because this is the single context every ffmpeg-running
	// kind derives its own from, and because the fact is discovered four layers
	// down — in the transcode package, which knows the ffmpeg line and nothing
	// about clips — while only this layer can name the clip it happened to.
	rctx = transcode.WithHWFallbackSink(rctx, func(stage, detail string) {
		w.log.Warn("the hardware decoder refused this source; re-running the stage with the decode on the CPU "+
			"and the encode on the card (DEPLOY.md §27)",
			"clip", clip.ID, "job", job.ID, "kind", job.Kind, "stage", stage, "detail", detail)
	})
	r := &run{jobID: job.ID, clipID: clip.ID, cancel: cancel, done: make(chan struct{})}
	w.mu.Lock()
	parked := false
	for prev := w.runs[clip.ID]; prev != nil; prev = w.runs[clip.ID] {
		if !parked {
			// Counted until the registration below, under the same hold of
			// mu: a Cancel of this clip waits for a parked run as it waits
			// for a registered one (§20 erratum 317(b)).
			w.waiting[clip.ID]++
			parked = true
		}
		hook := w.parkedHook
		w.mu.Unlock()
		if hook != nil {
			hook(clip.ID)
		}
		w.log.Debug("previous run of this clip is still exiting; waiting", "clip", clip.ID, "job", job.ID, "prev_job", prev.jobID)
		<-prev.done
		w.mu.Lock()
	}
	if parked {
		if w.waiting[clip.ID]--; w.waiting[clip.ID] <= 0 {
			delete(w.waiting, clip.ID)
		}
	}
	switch {
	case w.stopping:
		r.setReason(reasonShutdown)
		cancel()
	case w.cancelling[clip.ID] > 0:
		// A Cancel of this clip is in progress: the run it is waiting for
		// is this one too, so it starts cancelled and takes its release
		// path, and Cancel returns only once it has.
		r.setReason(reasonCancelled)
		cancel()
	}
	w.runs[clip.ID] = r
	close(w.regCh)
	w.regCh = make(chan struct{})
	w.mu.Unlock()
	defer func() {
		cancel()
		w.mu.Lock()
		if w.runs[clip.ID] == r {
			delete(w.runs, clip.ID)
		}
		w.mu.Unlock()
		close(r.done)
	}()
	return body(rctx, r)
}

// refuseKind fails a job of a kind this worker cannot run: permanent, no
// counter, ERROR — a Phase-4 kind reaching a Phase-1 worker needs a human.
func (w *worker) refuseKind(ctx context.Context, job db.Job, clip db.Clip) {
	log := w.log.With("clip", clip.ID, "job", job.ID, "kind", job.Kind)
	log.Error("unsupported job kind; failing the job")
	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), exitBudget)
	defer cancel()
	// Pinned to the claim like every other ending (§20 erratum 293): this run
	// made nothing and holds nothing, so a row that is no longer its own is
	// simply left where the repair put it.
	_, err := w.deps.DB.FailAttempt(ectx, job.ID, db.ClaimStamp(job), clip.ID, "unsupported job kind: "+job.Kind, db.FailPermanent, w.lim, w.now().Unix())
	if err != nil && !errors.Is(err, db.ErrStateMismatch) && !errors.Is(err, db.ErrNotFound) {
		log.Error("fail attempt", "err", err)
	}
}

// cardInput says a job of this kind writes something db.CardReady reads (§20
// erratum 330): the clip's state (the transcode's ready flip, FinalizeTranscode),
// its poster (SetPoster; the ready flip), its card file and `light_state` (the
// Light copy's commit SetSocial, its skip and failure endings through
// SettleTierEncoding and FailAttempt — the post-hoc Light copy of erratum 211 is
// such a `social` job too), its playable file (the Full's SetFull, a trim's
// SwapRendition, a replacement's SwapSource, each of which may also re-promise a
// card copy). A gif, a filmstrip, a notify and a hook write none of them.
func cardInput(kind string) bool {
	switch kind {
	case db.KindTranscode, db.KindSocial, db.KindPoster, db.KindFull, db.KindTrim, db.KindReplace:
		return true
	}
	return false
}

// cardReady tells Deps.CardReady that clipID's card works NOW, when it does —
// after a job that can make it so has ended, and after the automatic repair has
// settled one (repairOnce). It is a re-read of the row and a call, never a poll:
// whether the card was ready BEFORE is the bot's to know (it holds only links
// posted while it was not), so a ready card reported twice redraws nothing
// twice. nil is a no-op that reads nothing. The read survives a cancelled ctx —
// a job's commit may be the last thing it did before a shutdown — under the
// exit budget, and a fault inside the hook is one log line.
func (w *worker) cardReady(ctx context.Context, clipID string) {
	fn := w.deps.CardReady
	if fn == nil || clipID == "" {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), exitBudget)
	c, err := w.deps.DB.GetClip(rctx, clipID)
	cancel()
	if err != nil || !db.CardReady(c) {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			w.log.Error("card-ready hook panicked", "clip", clipID, "err", fmt.Sprint(p))
		}
	}()
	fn(clipID)
}

// settled tells Deps.OnSettled that a job has settled (§20 erratum 331). nil
// — every production instance — is a no-op; a fault inside the hook is one
// log line and never the worker's.
func (w *worker) settled(s Settled) {
	fn := w.deps.OnSettled
	if fn == nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			w.log.Error("settle hook panicked", "job", s.JobID, "err", fmt.Sprint(p))
		}
	}()
	fn(s)
}

// onReady fires Hooks.OnReady for a clip that just became ready. It runs
// after the run has left the registry, so a slow hook can never hold
// Cancel or Stop (§10.1 defines what they wait for; a hook is not in that
// set), under its own budget, and is aborted by shutdown.
func (w *worker) onReady(ctx context.Context, clip db.Clip) {
	fn := w.deps.Hooks.OnReady
	if fn == nil {
		return
	}
	hctx, cancel := context.WithTimeout(ctx, hookBudget)
	defer cancel()
	go func() {
		select {
		case <-w.stopCh:
			cancel()
		case <-hctx.Done():
		}
	}()
	log := w.log.With("clip", clip.ID)
	c, err := w.deps.DB.GetClip(hctx, clip.ID)
	if err != nil {
		c = clip
		c.State = db.ClipReady
	}
	defer func() {
		if p := recover(); p != nil {
			log.Error("OnReady hook panicked", "err", fmt.Sprint(p), "stack", string(debug.Stack()))
		}
	}()
	fn(hctx, c)
}

// Stop stops claiming, cancels every run with reason=shutdown and waits for
// all loops (bounded by ctx). Safe before Start and when called twice.
func (w *worker) Stop(ctx context.Context) error {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.stopping = true
		close(w.stopCh)
		runs := make([]*run, 0, len(w.runs))
		for _, r := range w.runs {
			runs = append(runs, r)
		}
		w.mu.Unlock()
		for _, r := range runs {
			r.setReason(reasonShutdown)
			r.cancel()
		}
	})
	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("worker: %d run(s) still active: %w", w.active(), ctx.Err())
	}
}

func (w *worker) active() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.runs)
}

// Wake nudges an idle loop of EACH queue to claim immediately (non-blocking).
// A caller that enqueues work does not know which kind it enqueued — POST
// /retry queues a transcode, POST …/trim a trim, a finalize a notify — so all
// three are nudged.
func (w *worker) Wake() {
	nudge(w.wake)
	nudge(w.wakeMedia)
	nudge(w.wakeNotify)
}

func nudge(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// Progress returns the registry entry for a clip.
func (w *worker) Progress(clipID string) (Progress, bool) { return w.prog.get(clipID) }

// TrimProgress returns the trim registry entry for a clip.
func (w *worker) TrimProgress(clipID string) (Progress, bool) { return w.trimProg.get(clipID) }

// AwaitTerminal subscribes to the clip's next terminal progress entry.
func (w *worker) AwaitTerminal(clipID string) (<-chan struct{}, func()) { return w.prog.await(clipID) }

// Busy reports whether the run registry — the same map Cancel and Stop treat
// as the definition of "this clip has a run in flight" — holds an entry for
// the clip. Every kind that writes into $SCRATCH_DIR/{id} goes through
// withRun, so a true answer means the dir is in use, whatever the clip row
// says (a trim and a gif run against a `ready` clip). It is the sweeper's
// in-process guard; db.HasLiveJob is the one that also survives a crash.
//
// A run PARKED behind a predecessor (`waiting`, §20 erratum 317(b)) is not
// counted, by design, and unlike Cancel, which must wait for it. Busy's one
// caller is the scratch janitor, and its question is "does a run own the
// dir right now": a parked run has written nothing there yet (it makes its
// dir only after it registers) and its predecessor clears what it wrote there
// on the way out, so there is nothing of the parked run's to protect. Its job
// is already claimed — `running` in the job table — so HasLiveJob, the
// janitor's second half, keeps the dir of a ready, failed or deleting clip,
// and a processing clip's dir is never removed at all. Counting it here would
// change no outcome. TestBusyDoesNotCountAParkedRun pins this.
func (w *worker) Busy(clipID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.runs[clipID] != nil
}

// Cancel is synchronous (§10.1): if the clip has a run it sets
// reason=cancelled, cancels the run context and blocks until the job
// goroutine has exited, then evicts the progress entry. No run → returns
// true at once.
//
// The wait is BOUNDED by ctx (AUDIT #13). The run's own exit path is bounded
// by exitBudget for its database and bucket work, but the scratch RemoveAll at
// the end of it is not: a hung filesystem there used to hold Cancel — and with
// it lifecycle.Destroy, the sweeper tick that called it, and a DELETE's
// DestroyBudget goroutine — for as long as the kernel took. Returning false
// instead hands the decision back to the caller, whose answer is always the
// same one §12 already gives every other failure: delete nothing, leave the
// tombstone, let the next tick resume. Nothing is orphaned by that, because
// the row still points at the objects.
//
// The progress entry is evicted only when the run really is gone: a false
// return leaves the clip's last progress entry in place, which is what a
// caller polling /progress should still see while the run is on its way out.
//
// A true return means more than "the goroutine is gone": the run's exit path
// ran first, and it has removed — BY KEY — every object this attempt wrote,
// including one whose PutFile this very cancellation cut off (§20 erratum 305).
// Cancelling a transfer does not un-write it: the client stops waiting while
// the bucket may already have taken the last byte, and the object then appears
// under a prefix its caller is about to stop listing. So the run, which is the
// only thing that knows the key, deletes it; lifecycle.Destroy's own
// list-driven sweep is not asked to see a write of a millisecond ago.
//
// "Has a run" includes a run PARKED behind the one in flight (§20 erratum
// 317(b)): a job of the same clip already claimed and waiting in withRun for
// its predecessor to leave the registry. Such a run used to register the
// instant the cancelled one closed `done` — after Cancel had stopped looking —
// and then run uncancelled into a prefix and a scratch dir lifecycle.Destroy
// was about to remove. Now every run that registers while a Cancel of its
// clip is in progress starts cancelled, and Cancel waits for each in turn, so
// a true return means no run of the clip is registered or parked. A job the
// cancelled run handed BACK to the queue is not a run: a later claim of it is
// new work Cancel cannot see, and the only caller keeps it from existing —
// lifecycle.Destroy calls MarkDeleting first, which cancels every job of the
// clip, and no claim takes a job of a deleting clip.
func (w *worker) Cancel(ctx context.Context, clipID string) bool {
	w.mu.Lock()
	if w.runs[clipID] == nil && w.waiting[clipID] == 0 {
		w.mu.Unlock()
		return true
	}
	w.cancelling[clipID]++
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		if w.cancelling[clipID]--; w.cancelling[clipID] <= 0 {
			delete(w.cancelling, clipID)
		}
		w.mu.Unlock()
	}()
	for {
		w.mu.Lock()
		r, parked, registered := w.runs[clipID], w.waiting[clipID], w.regCh
		w.mu.Unlock()
		if r == nil {
			if parked == 0 {
				break
			}
			// A parked run saw its predecessor leave and has not taken the
			// slot yet; it will, cancelled, and announce it on regCh.
			select {
			case <-registered:
				continue
			case <-ctx.Done():
				return false
			}
		}
		r.setReason(reasonCancelled)
		r.cancel()
		select {
		case <-r.done:
		case <-ctx.Done():
			return false
		}
	}
	w.Forget(clipID)
	return true
}

// Forget evicts a clip's progress entry (lifecycle step 5; POST /retry).
//
// A run for this clip may still be inside its exit path: fail() commits
// FailAttempt — which is the instant the clip is publicly `failed` and can
// be retried — and only then walks its bucket deletes and its scratch
// RemoveAll, ending with the forced terminal write. Evicting alone would let
// that write land AFTER the eviction and report the previous run's failure
// for a clip the row now says is queued, which is the very staleness Forget
// exists to prevent (§20 erratum 49). So the eviction also voids the run: it
// takes worker.mu, which every registry write from a run also takes, so no
// write can straddle it.
func (w *worker) Forget(clipID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if r := w.runs[clipID]; r != nil {
		r.progressVoid = true
	}
	w.prog.forget(clipID)
}

// NewNoop returns a Worker that does nothing and never blocks: Start
// returns, Stop returns nil, Wake/Cancel/Forget are no-ops, Progress reports
// (Progress{}, false) and Busy is never true — it runs nothing, so it owns no
// scratch dir.
func NewNoop() Worker { return noop{} }

type noop struct{}

func (noop) Start(context.Context)                {}
func (noop) Stop(context.Context) error           { return nil }
func (noop) Wake()                                {}
func (noop) Progress(string) (Progress, bool)     { return Progress{}, false }
func (noop) TrimProgress(string) (Progress, bool) { return Progress{}, false }
func (noop) Busy(string) bool                     { return false }
func (noop) Cancel(context.Context, string) bool  { return true }
func (noop) Forget(string)                        {}

// AwaitTerminal never signals: a worker that runs nothing finishes nothing,
// so the caller's own budget is what ends the wait.
func (noop) AwaitTerminal(string) (<-chan struct{}, func()) { return make(chan struct{}), func() {} }
