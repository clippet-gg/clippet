// Package sweeper ticks every SWEEP_INTERVAL, destroying SweepCandidates,
// pruning sessions and running the scratch janitor (DESIGN.md §12, §18.4).
//
// Lifecycle contract (§20 item 2): Start(ctx) BLOCKS running the tick loop
// until ctx is cancelled and the in-flight tick has finished, then returns.
// serve.go runs it in its own goroutine and, on shutdown, waits up to 5s
// for it to return (§18.1). New starts no goroutine.
package sweeper

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"clippet/internal/config"
	"clippet/internal/cost"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/ids"
	"clippet/internal/lifecycle"
	"clippet/internal/scratch"
)

const (
	// bootDelay is how long after Start the first (immediate) run happens.
	bootDelay = 10 * time.Second
	// tickMargin is subtracted from the interval for the per-tick ctx so a
	// slow tick ends before the next one is due (60s → 55s).
	tickMargin = 5 * time.Second
	// janitorEvery: the scratch janitor runs on every N-th tick.
	janitorEvery = 10
	// janitorStaleAge is the §12 mtime clause: a scratch dir untouched for
	// this long is removed unless its clip is processing.
	janitorStaleAge = 24 * time.Hour

	// Backoff for a candidate whose Destroy failed (§20 erratum 101). A
	// blackholing prefix costs up to STORAGE_OP_TIMEOUT (30s) per call against
	// a 55s tick budget, and SweepCandidates returns the same ids every tick,
	// so without a backoff two of them consume every tick forever and TTL --
	// one of the two headline promises -- silently stops. The delay is
	// failureBackoffBase*2^(n-1), capped, counted per clip id.
	failureBackoffBase = 2 * time.Minute
	failureBackoffCap  = time.Hour
	// failingPerTick is how many REPEATEDLY failing candidates may be
	// attempted in one tick. A candidate is charged to this budget only from
	// its second consecutive failure on (failingBudgetFrom): the first retry
	// after a failure is free, so a whole batch that failed because storage
	// was down is retried in ONE tick when storage comes back, while a prefix
	// that keeps failing is down to two attempts a tick by its third round.
	// Fresh candidates are never charged at all.
	failingPerTick    = 2
	failingBudgetFrom = 2
	// failingMax bounds the retry map. Beyond it the entry due soonest (the
	// one closest to being retried anyway) is dropped, so the map is
	// memory-bounded like every other cache here. It also bounds the rows
	// `sweep_failures` is read back in at boot (§20 erratum 232).
	failingMax = 1024
	// failWriteBudget bounds the two one-row statements that keep
	// `sweep_failures` in step with the map. They run on a context DETACHED
	// from the tick's, because the tick's budget running out is exactly when
	// remembering a backoff matters most.
	failWriteBudget = 5 * time.Second
)

// Stats summarises one tick. Deferred counts candidates the tick did not
// get to (or was interrupted in) because its ctx ended; they are neither
// failures nor storage trouble — the next tick picks them up. Skipped
// counts candidates that were no longer due when the tick reached them (a
// TTL extension PATCHed after candidate selection wins, §8.2).
type Stats struct {
	Checked, Destroyed, Failed int
	Dur                        time.Duration
	Deferred                   int
	Skipped                    int
	// Backoff counts candidates this tick did not attempt because a previous
	// Destroy of them failed and their next attempt is not due yet, or
	// because the tick had already retried failingPerTick of them (§20
	// erratum 101). They are neither failures nor deferrals: the tick chose
	// them, on purpose, in favour of work that can succeed.
	Backoff int
}

// Sweeper is the background expiry loop.
type Sweeper struct {
	deps lifecycle.Deps
	cfg  config.Config

	now          func() time.Time
	bootDelay    time.Duration
	janitorEvery int64

	ticks     atomic.Int64
	destroyed atomic.Int64
	errs      atomic.Int64

	// budget is the shared holder /healthz and the library page read
	// (ROADMAP #24, §20 erratum 157); nil = nobody is listening, which is
	// every test that does not care. over is this process's memory of the
	// last verdict, so the WARN is EDGE-triggered: crossing the line is news
	// once, not once a minute for as long as it stays crossed.
	budget *httpx.StorageBudget
	over   atomic.Bool
	// extra is the bytes the bucket holds that are not a clip's — the
	// database snapshot under `_clippet/` (§20 erratum 343) — added to the
	// DISPLAYED reading like any stored byte. nil is none. The hard cap
	// (STORAGE_BUDGET_HARD) reads clip rows at the upload and never this, so
	// the snapshot can never be the reason a clip is refused.
	extra func() int64
	// uploads expires the chunked web upload's sessions (§20 erratum 344): 24
	// hours after each was opened, finished or not. nil is none — every test
	// that does not care.
	uploads func(now time.Time) int

	// failing is the per-clip retry schedule for candidates whose Destroy
	// failed (§20 erratum 101). It is a WRITE-THROUGH cache of the
	// `sweep_failures` table (migration 0013, §20 erratum 232 amending 101(a)):
	// erratum 101 kept it in memory only, on the argument that a restart is the
	// one event that plausibly fixes storage — true of a restart an operator
	// performs, and exactly wrong for the one nobody performed, where
	// `restart: unless-stopped` plus a process dying every few minutes reset
	// the schedule on every boot and re-attempted the same black-holing prefix
	// at full cost for as long as the loop lasted. Touched only from the tick,
	// which never overlaps itself, but guarded anyway so a test may read it.
	failMu  sync.Mutex
	failing map[string]failState
	// loaded records that the table has been read into `failing` — done at the
	// first tick, not in New (which takes no context and starts nothing) and
	// not on every tick, because after the first read this process's own memory
	// is the newer of the two. Guarded by failMu.
	loaded bool
}

// failState is one candidate's failure history: how many consecutive
// Destroys have failed, and the earliest time the next one may run.
type failState struct {
	count int
	next  time.Time
}

// New builds a Sweeper (no goroutines yet).
func New(d lifecycle.Deps, cfg config.Config) *Sweeper {
	return &Sweeper{deps: d, cfg: cfg, now: time.Now, bootDelay: bootDelay, janitorEvery: janitorEvery,
		failing: map[string]failState{}}
}

// WithBudget attaches the holder the storage-budget reading is published to.
// It is a separate call rather than a New parameter so every existing caller
// — serve.go aside — keeps compiling and keeps behaving identically.
func (s *Sweeper) WithBudget(b *httpx.StorageBudget) *Sweeper {
	s.budget = b
	return s
}

// WithExtraBytes adds fn's answer to every storage reading (§20 erratum 343).
func (s *Sweeper) WithExtraBytes(fn func() int64) *Sweeper {
	s.extra = fn
	return s
}

// WithUploadSweep attaches the chunked upload's expiry (§20 erratum 344),
// run beside the scratch janitor on its own every-Nth tick.
func (s *Sweeper) WithUploadSweep(fn func(now time.Time) int) *Sweeper {
	s.uploads = fn
	return s
}

// Start runs the loop until ctx is done: one run bootDelay after the call,
// then one per SweepInterval. It returns only after the in-flight tick has
// finished. Ticks never overlap; a tick longer than the interval simply
// delays the next one.
func (s *Sweeper) Start(ctx context.Context) {
	// The budget reading is taken IMMEDIATELY, before the boot delay: an
	// operator who has just set STORAGE_BUDGET_BYTES and reloaded /healthz
	// should see an answer, not a gap, and it is one aggregate query.
	s.checkBudget(ctx)
	if !sleepCtx(ctx, s.bootDelay) {
		return
	}
	s.tick(ctx)
	ticker := time.NewTicker(s.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Sweeper) interval() time.Duration {
	if s.cfg.SweepInterval > 0 {
		return s.cfg.SweepInterval
	}
	return time.Minute
}

// tick runs one RunOnce under the per-tick budget.
func (s *Sweeper) tick(ctx context.Context) {
	tctx, cancel := context.WithTimeout(ctx, tickBudget(s.interval()))
	defer cancel()
	s.RunOnce(tctx)
}

// tickBudget is the per-tick ctx: interval − 5s (55s for the default 60s),
// never below half the interval for short intervals.
func tickBudget(interval time.Duration) time.Duration {
	if interval <= 0 {
		interval = time.Minute
	}
	b := interval - tickMargin
	if b < interval/2 {
		b = interval / 2
	}
	return b
}

// RunOnce performs a single tick: SweepCandidates → Destroy each (stopping
// early, deferring the rest, when ctx runs out — a Destroy the ctx cut short
// counts as deferred too, since its tombstone resumes next tick) → prune
// expired sessions → every janitorEvery-th tick, the scratch janitor. It
// logs "sweep checked=N destroyed=N failed=N dur=…" at INFO when
// checked > 0, DEBUG otherwise (§12).
func (s *Sweeper) RunOnce(ctx context.Context) Stats {
	start := s.now()
	tick := s.ticks.Add(1)
	log := s.logger()
	var st Stats
	s.loadFailures(ctx)

	now := start.Unix()
	grace := int64(s.cfg.SweepGrace / time.Second)
	cands, err := s.deps.DB.SweepCandidates(ctx, now, grace, s.cfg.SweepBatch)
	if err != nil {
		log.Log(ctx, dbLevel(err), "sweep: candidates", "err", err)
		st.Dur = s.now().Sub(start)
		return st
	}
	st.Checked = len(cands)
	retried := 0
	for i, id := range cands {
		if ctx.Err() != nil {
			st.Deferred = len(cands) - i
			break
		}
		// A candidate whose Destroy already failed waits out its backoff, and
		// a REPEATEDLY failing one is additionally capped at failingPerTick
		// attempts per tick (§20 erratum 101). Everything else in the
		// batch is tried exactly as before.
		due, fails := s.retryDue(id, start)
		if !due || (fails >= failingBudgetFrom && retried >= failingPerTick) {
			st.Backoff++
			continue
		}
		if fails >= failingBudgetFrom {
			retried++
		}
		// DestroyExpired re-verifies the candidate inside its tombstone
		// write: a clip whose TTL was extended after candidate selection is
		// skipped, so a committed PATCH is never inverted by a stale list.
		res, err := lifecycle.DestroyExpired(ctx, s.deps, id, now, grace)
		if err != nil && ctx.Err() != nil {
			// Shutdown or the tick budget ended this Destroy, not storage:
			// it is deferred like the rest, not a sweep error, and the clip
			// keeps its backoff state so the next tick retries it at once.
			// The test is the CONTEXT, never the error's chain (§20 erratum
			// 101): a cancelled Destroy surfaces whatever the layer that
			// noticed first produced -- database/sql answers a cut commit
			// with "sql: transaction has already been committed or rolled
			// back", which wraps no context error at all. Charging that as a
			// storage failure suppressed the clip for 2m→1h and starved the
			// tail of every slow batch, which is the exact starvation
			// erratum 101 exists to prevent. err == nil is deliberately NOT
			// deferred: a Destroy that finished counts, and the loop head
			// defers what is left on the next pass.
			st.Deferred = len(cands) - i
			break
		}
		switch {
		case err != nil:
			st.Failed++
			s.errs.Add(1)
			n := s.recordFailure(ctx, id, start)
			log.Warn("sweep: destroy failed; backing this clip off", "clip", id, "failures", n,
				"retry_in", backoffFor(n), "err", err)
		case res.Skipped:
			st.Skipped++
			s.recordSuccess(ctx, id)
		default:
			st.Destroyed++
			s.destroyed.Add(1)
			s.recordSuccess(ctx, id)
		}
	}

	if ctx.Err() == nil {
		if n, err := s.deps.DB.DeleteExpiredSessions(ctx, now); err != nil {
			log.Log(ctx, dbLevel(err), "sweep: prune sessions", "err", err)
		} else if n > 0 {
			log.Info("sweep: pruned sessions", "n", n)
		}
		if s.janitorEvery > 0 && tick%s.janitorEvery == 0 {
			s.janitor(ctx, start)
			if s.uploads != nil {
				if n := s.uploads(start); n > 0 {
					log.Info("sweep: expired upload sessions", "n", n)
				}
			}
		}
		// After the destroys, so the number reflects what this tick removed.
		s.checkBudget(ctx)
		s.pruneServed(ctx, start)
	}

	st.Dur = s.now().Sub(start)
	attrs := []any{"checked", st.Checked, "destroyed", st.Destroyed, "failed", st.Failed, "dur", st.Dur}
	if st.Deferred > 0 {
		attrs = append(attrs, "deferred", st.Deferred)
	}
	if st.Skipped > 0 {
		attrs = append(attrs, "skipped", st.Skipped)
	}
	if st.Backoff > 0 {
		attrs = append(attrs, "backoff", st.Backoff)
	}
	if st.Checked > 0 {
		log.Info("sweep", attrs...)
	} else {
		log.Debug("sweep", attrs...)
	}
	return st
}

// Totals returns the process-lifetime counters exposed by /healthz.
func (s *Sweeper) Totals() (destroyed, errors int64) {
	return s.destroyed.Load(), s.errs.Load()
}

// checkBudget measures the instance's known object total and publishes it,
// warning ONCE each time the advisory line is crossed (ROADMAP #24, §20
// erratum 157).
//
// Three properties are the whole design:
//
//   - **Nothing here refuses work.** There is no path from this function to an
//     upload, a transcode or a route. Since §20 erratum 277 an operator CAN
//     turn the same line into a cap with STORAGE_BUDGET_HARD=1 — but that cap
//     reads the database live at the one place a clip is created
//     (httpapi.storageQuota) and never this cached reading, which would refuse
//     for a tick after somebody deleted a clip to make room. This function
//     stays what it was: a measurement and a warning. README's "no tiers, no
//     per-user quotas" is untouched either way — one line for one instance,
//     set by whoever runs it, is not a tier.
//   - **It is edge-triggered.** A WARN on the crossing, an INFO on the way
//     back under, and silence in between. A line that fired every tick would
//     be a line an operator filters out, which is the same as not having it.
//   - **It never fails a tick.** A read error is logged at the DB level and
//     the previous reading stands: the sweeper's job is destroying expired
//     clips, and a number for a dashboard must not be able to interrupt it.
//
// Since ROADMAP #39 (§20 erratum 270) the MEASUREMENT is taken whenever a
// holder is wired, whether or not STORAGE_BUDGET_BYTES is set: the stored
// figure is now the storage-cost line's first number, on /healthz and on the
// settings page, and both read this cached reading rather than growing a scan
// of `clips` of their own. Only the WARNING still depends on a budget existing
// — with none set, `budget` is published as 0, `BudgetStatus.Over` is false by
// construction and the `storage_budget` object stays absent exactly as before.
func (s *Sweeper) checkBudget(ctx context.Context) {
	limit := s.cfg.StorageBudgetBytes
	if s.budget == nil || ctx.Err() != nil {
		return
	}
	used, err := s.deps.DB.StorageUsed(ctx)
	if err != nil {
		s.logger().Log(ctx, dbLevel(err), "sweep: storage budget", "err", err)
		return
	}
	if s.extra != nil {
		used += s.extra()
	}
	s.budget.Set(used, limit)
	if limit <= 0 {
		return // nothing to compare it against; the reading is published and that is all
	}
	over := used > limit
	if s.over.Swap(over) == over {
		return
	}
	if over {
		// The sentence names the mode, because the two mean opposite things
		// to whoever reads the log: one is a number to watch, the other says
		// uploads are being refused right now (§20 erratum 277).
		msg := "storage budget exceeded (advisory: no upload is ever refused because of it, unless STORAGE_BUDGET_HARD=1)"
		if s.cfg.StorageBudgetHard {
			msg = "storage budget exceeded (STORAGE_BUDGET_HARD=1: new clips are being refused with 507)"
		}
		s.logger().Warn(msg, "used_bytes", used, "budget_bytes", limit, "over_bytes", used-limit)
		return
	}
	s.logger().Info("storage back under budget", "used_bytes", used, "budget_bytes", limit)
}

// pruneServed applies the served-bytes counter's retention: 400 days, one
// DELETE, on the tick that already prunes sessions (ROADMAP #39, §20 erratum
// 270; migration 0017).
//
// It is the whole of that table's lifecycle. On a healthy instance it deletes
// nothing — the table holds one row per day and cannot pass 400 of them — so
// the statement is an index probe against the primary key's own ordering, in
// the same class as the `DeleteExpiredSessions` above it. It cannot fail a
// tick: a read or write error is logged at the DB level and the next tick tries
// again, exactly like every other bookkeeping call here.
func (s *Sweeper) pruneServed(ctx context.Context, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	n, err := s.deps.DB.PruneBytesServed(ctx, cost.PruneBefore(now))
	if err != nil {
		s.logger().Log(ctx, dbLevel(err), "sweep: prune the served-bytes history", "err", err)
		return
	}
	if n > 0 {
		s.logger().Info("sweep: pruned served-bytes days", "days", n, "retention_days", cost.RetentionDays)
	}
}

// loadFailures reads the persisted backoff schedule ONCE per process, before
// the first candidate of the first tick is looked at (N004, §20 erratum 232).
//
// Three properties are the whole of it:
//
//   - **It cannot fail a tick.** A read error is logged at the DB level and the
//     process simply starts with no memory, which is what every version before
//     migration 0013 did. Destroying expired clips is the sweeper's job; the
//     schedule is an optimisation over how it spends a tick.
//   - **It never resurrects a row the tick would ignore.** The table is keyed
//     by a foreign key onto `clips`, so a destroyed clip has no schedule to
//     load, and a row whose next attempt is already in the past loads as
//     "due now" — the same answer a fresh candidate gets.
//   - **It is bounded by the same failingMax the map is**, taking the rows
//     whose next attempt is FURTHEST away: the ones the eviction policy keeps.
func (s *Sweeper) loadFailures(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	s.failMu.Lock()
	done := s.loaded
	s.failMu.Unlock()
	if done {
		return
	}
	rows, err := s.deps.DB.LoadSweepFailures(ctx, failingMax)
	if err != nil {
		// NOT marked loaded: a tick whose read failed leaves the next one to
		// try again, which is the difference between "this process has no
		// memory yet" and "this process will never have one".
		s.logger().Log(ctx, dbLevel(err), "sweep: read the destroy-failure backoff", "err", err)
		return
	}
	s.failMu.Lock()
	restored := 0
	for _, r := range rows {
		// THIS process's own memory wins: a load retried after a failed read
		// must not overwrite a failure this process has since recorded.
		if _, ok := s.failing[r.ClipID]; ok {
			continue
		}
		s.failing[r.ClipID] = failState{count: r.Failures, next: time.Unix(r.NextAttemptAt, 0)}
		restored++
	}
	s.loaded = true
	s.failMu.Unlock()
	if restored > 0 {
		s.logger().Info("sweep: restored the destroy-failure backoff a previous process left behind", "clips", restored)
	}
}

// retryDue reports whether id may be attempted now, and how many consecutive
// Destroys of it have already failed (0 for a candidate with no history).
func (s *Sweeper) retryDue(id string, now time.Time) (bool, int) {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	f, ok := s.failing[id]
	if !ok {
		return true, 0
	}
	return !now.Before(f.next), f.count
}

// recordFailure counts a failed Destroy and schedules the next attempt,
// returning the new consecutive-failure count. The schedule is written through
// to `sweep_failures` so the NEXT process honours it (N004, §20 erratum 232);
// a write that fails costs nothing but that memory, so it is logged and
// otherwise ignored — the in-memory schedule this tick and the next ones read
// is already correct.
func (s *Sweeper) recordFailure(ctx context.Context, id string, now time.Time) int {
	s.failMu.Lock()
	f := s.failing[id]
	f.count++
	f.next = now.Add(backoffFor(f.count))
	s.failing[id] = f
	evicted := s.evictLocked()
	s.failMu.Unlock()
	// A detached context: the tick's own budget is what just ran out on some
	// candidate, and forgetting a backoff because the write was cut short is
	// the failure mode the table exists to prevent. It is one indexed UPSERT.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failWriteBudget)
	defer cancel()
	err := s.deps.DB.SetSweepFailure(wctx, db.SweepFailure{
		ClipID: id, Failures: f.count, LastAttemptAt: now.Unix(), NextAttemptAt: f.next.Unix()})
	if err != nil {
		s.logger().Log(ctx, dbLevel(err), "sweep: remember the destroy-failure backoff", "clip", id, "err", err)
	}
	// An eviction FORGETS a backoff, and it has to forget it in both places:
	// the row is what the next process reads (loadFailures), so a row left
	// behind by an evicted entry would come back at the next boot as a backoff
	// nothing in memory can clear. Best-effort, on the same detached budget
	// recordSuccess uses, for the same reason.
	for _, gone := range evicted {
		if err := s.deps.DB.ClearSweepFailure(wctx, gone); err != nil {
			s.logger().Log(ctx, dbLevel(err), "sweep: forget an evicted destroy-failure backoff", "clip", gone, "err", err)
		}
	}
	return f.count
}

// recordSuccess forgets a candidate that finally went through (or turned out
// not to be due after all), in memory and on disk. A clip that was DESTROYED
// took its row with it through the foreign key, so the delete is usually a
// no-op; it is still issued, because the other caller is the `Skipped` case,
// whose clip is very much still there.
func (s *Sweeper) recordSuccess(ctx context.Context, id string) {
	s.failMu.Lock()
	_, had := s.failing[id]
	delete(s.failing, id)
	s.failMu.Unlock()
	if !had {
		// The overwhelmingly common case: a candidate that never failed. No
		// statement at all, so a healthy sweep costs the database nothing new.
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failWriteBudget)
	defer cancel()
	if err := s.deps.DB.ClearSweepFailure(wctx, id); err != nil {
		s.logger().Log(ctx, dbLevel(err), "sweep: forget the destroy-failure backoff", "clip", id, "err", err)
	}
}

// evictLocked keeps the retry map bounded: the entry due soonest is dropped,
// because its backoff is nearly over anyway. It returns what it dropped, so
// the caller can take those rows out of `sweep_failures` too — the map and the
// table say the same thing or neither is trustworthy.
func (s *Sweeper) evictLocked() []string {
	var evicted []string
	for len(s.failing) > failingMax {
		var oldestID string
		var oldest time.Time
		first := true
		for id, f := range s.failing {
			if first || f.next.Before(oldest) {
				oldestID, oldest, first = id, f.next, false
			}
		}
		delete(s.failing, oldestID)
		evicted = append(evicted, oldestID)
	}
	return evicted
}

// backoffFor is failureBackoffBase*2^(n-1), capped -- the same shape §11
// gives a transient job failure, for the same reason.
func backoffFor(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	shift := n - 1
	if shift > 20 {
		shift = 20
	}
	d := failureBackoffBase << uint(shift)
	if d <= 0 || d > failureBackoffCap {
		return failureBackoffCap
	}
	return d
}

// janitor applies the full §12 rule (mirrors the boot janitor in
// cmd/clippet/serve.go): remove $SCRATCH_DIR/{id} when the name is a valid
// id AND NOTHING IS RUNNING FOR THAT CLIP (no registered run here, no
// queued/running job row — §20 erratum 89) AND (the clip row is missing or
// in ready|failed|deleting, OR the dir's mtime is older than
// janitorStaleAge and the clip is not processing).
// Row-less upload spools ($SCRATCH_DIR/.upload-{id}, §8.3) are removed only
// when untouched for janitorStaleAge — an in-flight upload is sacred (§11).
func (s *Sweeper) janitor(ctx context.Context, now time.Time) {
	dir := s.deps.ScratchDir
	if dir == "" {
		return
	}
	log := s.logger()
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Warn("janitor: read scratch", "dir", dir, "err", err)
		return
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if scratch.IsUploadTmp(e.Name()) {
			// A row-less spool (§8.3): live while an upload is in flight —
			// only crash residue older than the stale bound is removed.
			if !uploadTmpStale(filepath.Join(dir, e.Name()), now) {
				continue
			}
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				log.Warn("janitor: remove", "dir", e.Name(), "err", err)
				continue
			}
			removed++
			continue
		}
		if !ids.Valid(e.Name()) {
			continue
		}
		remove, err := s.janitorWants(ctx, e, now)
		if err != nil {
			log.Warn("janitor: inspect", "clip", e.Name(), "err", err)
			continue
		}
		if !remove {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			log.Warn("janitor: remove", "clip", e.Name(), "err", err)
			continue
		}
		removed++
	}
	if removed > 0 {
		log.Info("janitor", "scanned", len(entries), "removed", removed)
	} else {
		log.Debug("janitor", "scanned", len(entries), "removed", 0)
	}
}

func (s *Sweeper) janitorWants(ctx context.Context, e os.DirEntry, now time.Time) (bool, error) {
	// This process's own answer first: every kind that writes into
	// $SCRATCH_DIR/{id} registers a run, so a registered run owns the dir
	// whatever the clip row says.
	if s.busy(e.Name()) {
		return false, nil
	}
	c, err := s.deps.DB.GetClip(ctx, e.Name())
	switch {
	case errors.Is(err, db.ErrNotFound):
		return true, nil
	case err != nil:
		return false, err
	case c.State == db.ClipReady || c.State == db.ClipFailed || c.State == db.ClipDeleting:
		// `ready` is not "nobody is working on this": a trim and a gif run
		// against a clip that is ready for the whole run and use this very
		// dir (§17 #5, #6). The job table is asked instead of the state
		// being read as a proxy, and it is the half of the guard that
		// survives a crash — a job left `queued` or `running` by a restart
		// is about to be claimed again (§20 erratum 89).
		live, lerr := s.deps.DB.HasLiveJob(ctx, e.Name())
		if lerr != nil {
			return false, lerr
		}
		return !live, nil
	case c.State == db.ClipProcessing:
		return false, nil // the worker owns it, however old
	}
	info, err := e.Info()
	if err != nil {
		return false, err
	}
	return now.Sub(info.ModTime()) > janitorStaleAge, nil
}

// uploadTmpStale reports whether a row-less upload spool is crash residue:
// nothing inside it (nor the dir itself) was written for janitorStaleAge.
// An upload actively spooling keeps its source file's mtime fresh. Any stat
// trouble keeps the dir — the next pass retries.
func uploadTmpStale(path string, now time.Time) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	newest := info.ModTime()
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			return false
		}
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	return now.Sub(newest) > janitorStaleAge
}

// busy is Deps.Busy with the nil case spelled out: a Sweeper wired without a
// worker (every test that does not need one) is told nothing is running here,
// and the job-table half of the guard still applies.
func (s *Sweeper) busy(clipID string) bool {
	return s.deps.Busy != nil && s.deps.Busy(clipID)
}

func (s *Sweeper) logger() *slog.Logger {
	if s.deps.Log != nil {
		return s.deps.Log
	}
	return slog.Default()
}

// dbLevel: a cancelled tick is expected (INFO), a deadline is WARN, any
// other database error outside busy_timeout is ERROR (§12).
func dbLevel(err error) slog.Level {
	switch {
	case errors.Is(err, context.Canceled):
		return slog.LevelInfo
	case errors.Is(err, context.DeadlineExceeded):
		return slog.LevelWarn
	}
	return slog.LevelError
}

// sleepCtx waits d or until ctx is done; it reports whether the full delay
// elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
