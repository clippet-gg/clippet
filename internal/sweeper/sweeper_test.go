package sweeper

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/lifecycle"
	"clippet/internal/scratch"
	"clippet/internal/store"
	"clippet/internal/worker"
)

// --- fixtures -------------------------------------------------------------

type fakeWorker struct {
	mu      sync.Mutex
	cancels []string
	// busy is the run registry as far as the janitor is concerned: the
	// clips this worker would say it is running right now.
	busy map[string]bool
}

func (w *fakeWorker) Start(context.Context)                       {}
func (w *fakeWorker) Stop(context.Context) error                  { return nil }
func (w *fakeWorker) Wake()                                       {}
func (w *fakeWorker) Progress(string) (worker.Progress, bool)     { return worker.Progress{}, false }
func (w *fakeWorker) TrimProgress(string) (worker.Progress, bool) { return worker.Progress{}, false }
func (w *fakeWorker) Forget(string)                               {}
func (w *fakeWorker) AwaitTerminal(string) (<-chan struct{}, func()) {
	return make(chan struct{}), func() {}
}
func (w *fakeWorker) Busy(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.busy[id]
}
func (w *fakeWorker) setBusy(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.busy == nil {
		w.busy = map[string]bool{}
	}
	w.busy[id] = true
}
func (w *fakeWorker) Cancel(_ context.Context, id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cancels = append(w.cancels, id)
	return true
}
func (w *fakeWorker) cancelled() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.cancels...)
}

type recHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *recHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r)
	return nil
}
func (h *recHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recHandler) WithGroup(string) slog.Handler      { return h }
func (h *recHandler) maxLevel() slog.Level {
	h.mu.Lock()
	defer h.mu.Unlock()
	lvl := slog.LevelDebug
	for _, r := range h.recs {
		if r.Level > lvl {
			lvl = r.Level
		}
	}
	return lvl
}

// notes collects the "note" attribute of every record with msg.
func (h *recHandler) notes(msg string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.recs {
		if r.Message != msg {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "note" {
				out = append(out, a.Value.String())
			}
			return true
		})
	}
	return out
}

// attrs returns the attribute set of the LAST record with msg, keyed by name
// and rendered as text, so a test can assert what one log line actually said.
func (h *recHandler) attrs(msg string) map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out map[string]string
	for _, r := range h.recs {
		if r.Message != msg {
			continue
		}
		out = map[string]string{}
		r.Attrs(func(a slog.Attr) bool {
			out[a.Key] = a.Value.String()
			return true
		})
	}
	return out
}

func (h *recHandler) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = nil
}
func (h *recHandler) count(msg string, level slog.Level) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.recs {
		if r.Message == msg && r.Level == level {
			n++
		}
	}
	return n
}

type fixture struct {
	t       *testing.T
	db      *db.DB
	fake    *store.Fake
	worker  *fakeWorker
	logs    *recHandler
	scratch string
	owner   db.User
	now     time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "clippet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	fx := &fixture{t: t, db: d, fake: store.NewFake(), worker: &fakeWorker{}, logs: &recHandler{},
		scratch: filepath.Join(t.TempDir(), "scratch"), now: time.Now()}
	if err := os.MkdirAll(fx.scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	fx.owner, err = d.CreateUser(context.Background(), ids.New(), "carol", "$2a$12$h", 1)
	if err != nil {
		t.Fatal(err)
	}
	return fx
}

func (fx *fixture) cfg() config.Config {
	return config.Config{SweepInterval: time.Minute, SweepGrace: time.Hour, SweepBatch: 50, ScratchDir: fx.scratch}
}

func (fx *fixture) sweeper(cfg config.Config) *Sweeper {
	deps := lifecycle.Deps{DB: fx.db, Store: fx.fake, Worker: fx.worker, ScratchDir: fx.scratch, Log: slog.New(fx.logs),
		OpTimeout: 5 * time.Second, Busy: fx.worker.Busy}
	s := New(deps, cfg)
	s.now = func() time.Time { return fx.now }
	s.bootDelay = 0
	return s
}

// clip inserts a queued clip with its four objects in the Fake.
func (fx *fixture) clip() db.Clip {
	fx.t.Helper()
	id := ids.New()
	k := store.KeysFor("", id, "mp4")
	c, _, err := fx.db.CreateClipWithJob(context.Background(), db.NewClip{
		ID: id, OwnerID: fx.owner.ID, Title: "t " + id, SourceKey: k.Original, SourceFilename: "a.mp4",
		SourceSize: 1, SourceContentType: "video/mp4", SourceETag: "e", Now: fx.now.Unix() - 3600,
	})
	if err != nil {
		fx.t.Fatal(err)
	}
	for _, key := range []string{k.Original, store.RenditionKey("", id, "r8r8r8r8"), store.PosterKey("", id, "r8r8r8r8"), k.Probe} {
		fx.fake.Put(key, []byte("x"), "")
	}
	return c
}

// claim moves the oldest queued job (which must be c's) to running.
func (fx *fixture) claim(c db.Clip) db.Job {
	fx.t.Helper()
	j, cc, ok, err := fx.db.ClaimJob(context.Background(), fx.now.Unix())
	if err != nil || !ok || cc.ID != c.ID {
		fx.t.Fatalf("claim %s: ok=%v got=%s err=%v", c.ID, ok, cc.ID, err)
	}
	return j
}

func (fx *fixture) processing() db.Clip {
	c := fx.clip()
	fx.claim(c)
	return c
}

func (fx *fixture) ready() db.Clip {
	fx.t.Helper()
	c := fx.clip()
	j := fx.claim(c)
	ok, _, err := fx.db.FinalizeTranscode(context.Background(), j.ID, c.ID, db.ReadyFields{RenditionKey: "r", PosterKey: "p"}, false, fx.now.Unix())
	if err != nil || !ok {
		fx.t.Fatalf("finalize: %v %v", ok, err)
	}
	return c
}

func (fx *fixture) failed() db.Clip {
	fx.t.Helper()
	c := fx.clip()
	j := fx.claim(c)
	if _, err := fx.db.FailAttempt(context.Background(), j.ID, db.ClaimStamp(j), c.ID, "bad", db.FailPermanent, db.Limits{}, fx.now.Unix()); err != nil {
		fx.t.Fatal(err)
	}
	return c
}

func (fx *fixture) expire(c db.Clip, ago time.Duration) {
	fx.t.Helper()
	if err := fx.db.ForceExpire(context.Background(), c.ID, fx.now.Add(-ago).Unix()); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *fixture) state(id string) string {
	fx.t.Helper()
	c, err := fx.db.GetClip(context.Background(), id)
	if errors.Is(err, db.ErrNotFound) {
		return "gone"
	}
	if err != nil {
		fx.t.Fatal(err)
	}
	return c.State
}

// --- tests ----------------------------------------------------------------

// TestRunOnceCandidateRules covers every §12 candidate branch on one tick:
// (a) deleting tombstones; (b) queued/ready/failed at expiry; (c) processing
// only after grace; never-expiring and future rows untouched.
func TestRunOnceCandidateRules(t *testing.T) {
	fx := newFixture(t)
	// ClaimJob takes the oldest queued job, so every clip that must be
	// claimed is created before the one that stays queued.
	readyExpired := fx.ready()
	fx.expire(readyExpired, time.Second)
	failedExpired := fx.failed()
	fx.expire(failedExpired, time.Second)
	processingInGrace := fx.processing()
	fx.expire(processingInGrace, 30*time.Minute)
	processingPastGrace := fx.processing()
	fx.expire(processingPastGrace, 2*time.Hour)
	readyFuture := fx.ready()
	fx.expire(readyFuture, -time.Hour)
	readyNever := fx.ready()
	tombstone := fx.ready()
	if err := fx.db.MarkDeleting(context.Background(), tombstone.ID, fx.now.Unix()); err != nil {
		t.Fatal(err)
	}
	queuedExpired := fx.clip()
	fx.expire(queuedExpired, 10*time.Second)

	s := fx.sweeper(fx.cfg())
	st := s.RunOnce(context.Background())
	if st.Checked != 5 || st.Destroyed != 5 || st.Failed != 0 {
		t.Fatalf("Stats = %+v, want checked=5 destroyed=5 failed=0", st)
	}
	if d, e := s.Totals(); d != 5 || e != 0 {
		t.Fatalf("Totals = %d, %d", d, e)
	}
	want := map[string]string{
		queuedExpired.ID: "gone", readyExpired.ID: "gone", failedExpired.ID: "gone",
		processingPastGrace.ID: "gone", tombstone.ID: "gone",
		processingInGrace.ID: db.ClipProcessing, readyFuture.ID: db.ClipReady, readyNever.ID: db.ClipReady,
	}
	for id, w := range want {
		if got := fx.state(id); got != w {
			t.Errorf("clip %s: state %q, want %q", id, got, w)
		}
	}
	// Objects of the survivors are intact; the destroyed clips' are gone.
	for id, w := range want {
		_, present := fx.fake.Get(store.OriginalKey("", id, "mp4"))
		if present != (w != "gone") {
			t.Errorf("clip %s: original present=%v, want %v", id, present, w != "gone")
		}
	}
	// Every candidate got a (blocking) Cancel before its objects went.
	cancelled := map[string]bool{}
	for _, id := range fx.worker.cancelled() {
		cancelled[id] = true
	}
	for _, id := range []string{queuedExpired.ID, processingPastGrace.ID, tombstone.ID} {
		if !cancelled[id] {
			t.Errorf("clip %s: Cancel not called", id)
		}
	}
	if cancelled[processingInGrace.ID] {
		t.Error("in-grace processing clip was cancelled")
	}
	if fx.logs.count("sweep", slog.LevelInfo) != 1 || fx.logs.count("destroy", slog.LevelInfo) != 5 {
		t.Fatalf("log lines: sweep=%d destroy=%d", fx.logs.count("sweep", slog.LevelInfo), fx.logs.count("destroy", slog.LevelInfo))
	}
	if fx.logs.maxLevel() > slog.LevelInfo {
		t.Fatalf("a green sweep logged at %v", fx.logs.maxLevel())
	}
}

// TestRunOnceResumesAfterStorageError: tick 1 fails (row stays deleting,
// counted as failed, WARN); the next tick leaves it alone -- a failed
// candidate is backed off, §20 erratum 101 -- and the tick after the
// backoff expires resumes the tombstone and finishes.
func TestRunOnceResumesAfterStorageError(t *testing.T) {
	fx := newFixture(t)
	c := fx.ready()
	fx.expire(c, time.Minute)
	fx.fake.FailNext("DeletePrefix", errors.New("bucket unreachable"))
	s := fx.sweeper(fx.cfg())

	st := s.RunOnce(context.Background())
	if st.Checked != 1 || st.Failed != 1 || st.Destroyed != 0 {
		t.Fatalf("tick 1 Stats = %+v", st)
	}
	if got := fx.state(c.ID); got != db.ClipDeleting {
		t.Fatalf("state after failed tick = %q", got)
	}
	if fx.logs.maxLevel() != slog.LevelWarn {
		t.Fatalf("max level after storage error = %v, want WARN", fx.logs.maxLevel())
	}

	// Immediately afterwards the candidate is still in its backoff: the tick
	// sees it, counts it as backed off and touches storage not at all.
	st = s.RunOnce(context.Background())
	if st.Checked != 1 || st.Backoff != 1 || st.Destroyed != 0 || st.Failed != 0 {
		t.Fatalf("tick 2 (inside the backoff) Stats = %+v", st)
	}
	if got := fx.state(c.ID); got != db.ClipDeleting {
		t.Fatalf("state during the backoff = %q", got)
	}

	fx.now = fx.now.Add(failureBackoffBase + time.Second)
	st = s.RunOnce(context.Background())
	if st.Checked != 1 || st.Destroyed != 1 || st.Failed != 0 || st.Backoff != 0 {
		t.Fatalf("tick 3 Stats = %+v", st)
	}
	if got := fx.state(c.ID); got != "gone" {
		t.Fatalf("state after resume = %q", got)
	}
	if d, e := s.Totals(); d != 1 || e != 1 {
		t.Fatalf("Totals = %d, %d", d, e)
	}
	// Quiet tick: nothing to check, DEBUG only.
	st = s.RunOnce(context.Background())
	if st.Checked != 0 || fx.logs.count("sweep", slog.LevelDebug) != 1 {
		t.Fatalf("quiet tick: %+v, debug sweeps=%d", st, fx.logs.count("sweep", slog.LevelDebug))
	}
}

// TestRunOnceBatchAndDeferral: SWEEP_BATCH bounds a tick; a ctx that ends
// mid-tick defers the rest — including the Destroy it interrupted, which is
// not a sweep error — to the next tick without losing anything.
func TestRunOnceBatchAndDeferral(t *testing.T) {
	fx := newFixture(t)
	for i := 0; i < 3; i++ {
		fx.expire(fx.ready(), time.Minute)
	}
	cfg := fx.cfg()
	cfg.SweepBatch = 2
	s := fx.sweeper(cfg)
	if st := s.RunOnce(context.Background()); st.Checked != 2 || st.Destroyed != 2 {
		t.Fatalf("batched tick = %+v", st)
	}
	if st := s.RunOnce(context.Background()); st.Checked != 1 || st.Destroyed != 1 {
		t.Fatalf("second tick = %+v", st)
	}

	// Deferral: cancel the tick ctx inside the first DeletePrefix.
	for i := 0; i < 3; i++ {
		fx.expire(fx.ready(), time.Minute)
	}
	cfg.SweepBatch = 50
	s = fx.sweeper(cfg)
	fx.logs.reset()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fx.fake.Hook = func(context.Context, string, string) error {
		cancel()
		return nil
	}
	st := s.RunOnce(ctx)
	if st.Checked != 3 || st.Deferred != 3 || st.Failed != 0 || st.Destroyed != 0 {
		t.Fatalf("deferred tick = %+v", st)
	}
	if _, e := s.Totals(); e != 0 {
		t.Fatalf("an interrupted tick counted %d sweep errors", e)
	}
	if fx.logs.count("destroy", slog.LevelInfo) != 1 || fx.logs.maxLevel() > slog.LevelInfo {
		t.Fatalf("interrupted destroy: INFO lines=%d max=%v", fx.logs.count("destroy", slog.LevelInfo), fx.logs.maxLevel())
	}
	fx.fake.Hook = nil
	if st := s.RunOnce(context.Background()); st.Checked != 3 || st.Destroyed != 3 || st.Deferred != 0 {
		t.Fatalf("catch-up tick = %+v", st)
	}
	// A storage failure while the tick is alive is still a sweep error.
	fx.expire(fx.ready(), time.Minute)
	fx.fake.FailNext("DeletePrefix", errors.New("bucket unreachable"))
	if st := s.RunOnce(context.Background()); st.Failed != 1 || st.Deferred != 0 {
		t.Fatalf("failed tick = %+v", st)
	}
	if _, e := s.Totals(); e != 1 {
		t.Fatalf("Totals errors = %d, want 1", e)
	}
}

// TestRunOnceDeferralDoesNotDependOnTheErrorChain (§20 erratum 101): the
// deferral test is ctx.Err(), not errors.Is(err, ctx.Err()). A Destroy the
// tick budget cuts short surfaces whatever layer noticed first -- for the
// real bug it was database/sql's "sql: transaction has already been
// committed or rolled back", which wraps no context error at all -- and the
// old chain test missed those, charged a storage failure and suppressed the
// clip for 2m→1h. Here the ctx is cancelled and the op answers with a plain
// error that is NOT a context error: the tick must still call it deferred,
// count no error, log no WARN, and retry the clip on the very next tick
// (Backoff == 0 proves no failure was recorded against it).
func TestRunOnceDeferralDoesNotDependOnTheErrorChain(t *testing.T) {
	fx := newFixture(t)
	for i := 0; i < 3; i++ {
		fx.expire(fx.ready(), time.Minute)
	}
	s := fx.sweeper(fx.cfg())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var once sync.Once
	fx.fake.Hook = func(_ context.Context, op, _ string) error {
		if op != "DeletePrefix" {
			return nil
		}
		var err error
		once.Do(func() {
			cancel()
			err = errors.New("sql: transaction has already been committed or rolled back")
		})
		return err
	}

	st := s.RunOnce(ctx)
	if st.Checked != 3 || st.Deferred != 3 || st.Failed != 0 || st.Destroyed != 0 {
		t.Fatalf("cut-short tick = %+v, want checked=3 deferred=3 failed=0 destroyed=0", st)
	}
	if _, e := s.Totals(); e != 0 {
		t.Fatalf("an interrupted tick counted %d sweep errors", e)
	}
	if n := fx.logs.count("sweep: destroy failed; backing this clip off", slog.LevelWarn); n != 0 {
		t.Fatalf("backoff WARNs on an interrupted tick = %d, want 0", n)
	}

	fx.fake.Hook = nil
	if st := s.RunOnce(context.Background()); st.Checked != 3 || st.Destroyed != 3 || st.Backoff != 0 || st.Failed != 0 {
		t.Fatalf("catch-up tick = %+v, want checked=3 destroyed=3 backoff=0 failed=0", st)
	}
}

// TestTickDefersOnItsOwnBudget closes the loop tick() -> tickBudget() ->
// RunOnce's deferral branch. tick() is the only place the per-tick budget is
// ever built, and it DISCARDS the Stats it gets back, so nothing about it was
// asserted anywhere: TestTickBudget covers the arithmetic alone, and
// TestStartBlocksAndTicks drove the path incidentally (which is why the -race
// flakes surfaced there) while asserting only that a clip was destroyed.
//
// Here the parent context is LIVE for the whole test and the store's
// DeletePrefix blocks until the ctx it was handed is done, so the only clock
// that can end the run is the budget tick() derived. What must follow is the
// erratum 101 deferral: every remaining candidate deferred, nothing failed,
// no sweep error counted, no backoff recorded against the clip -- and the
// parent still usable afterwards, which is what proves only the derived
// context expired.
//
// The budget is WALL-CLOCK, and it starts before SweepCandidates: on a box
// shared with other check runs under -race, the query (or loadFailures) once
// took longer than the 50 ms on its own, the tick logged no checked=2, and
// the test failed with nothing wrong (a nit sweep, run 6). So the tick is made
// on a doubling interval until the hook is ENTERED with the budget still
// live — the premise every assertion below rests on — reading the clock
// after the planting and polling the premise rather than sleeping (a trap
// this project recorded: fixed sleeps flake under load). On an idle box the first, 50 ms attempt is the one that counts; a
// fresh sweeper per attempt keeps an abandoned attempt's totals out of it.
func TestTickDefersOnItsOwnBudget(t *testing.T) {
	fx := newFixture(t)
	for i := 0; i < 2; i++ {
		fx.expire(fx.ready(), time.Minute)
	}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	var s *Sweeper
	for interval := 100 * time.Millisecond; ; interval *= 2 {
		cfg := fx.cfg()
		cfg.SweepInterval = interval // tickBudget -> interval/2 (50ms on the first attempt)
		s = fx.sweeper(cfg)
		if got := tickBudget(s.interval()); got != interval/2 {
			t.Fatalf("premise broken -- tickBudget(%v) = %v, want %v", interval, got, interval/2)
		}
		// Waiting on the ctx rather than sleeping a fixed time is what keeps
		// this test off the clock: it ends exactly when the budget does,
		// whatever the machine is doing.
		var entered atomic.Bool
		fx.fake.Hook = func(ctx context.Context, op, _ string) error {
			if op != "DeletePrefix" {
				return nil
			}
			if ctx.Err() == nil {
				entered.Store(true)
			}
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
				t.Error("the tick budget never expired")
			}
			return nil
		}
		fx.logs.reset()
		s.tick(parent)
		if entered.Load() {
			break
		}
		if interval >= 6400*time.Millisecond {
			t.Fatalf("the tick budget ran out before the first destroy at every interval up to %v: %v", interval, fx.logs.attrs("sweep"))
		}
	}

	if parent.Err() != nil {
		t.Fatalf("tick() ended its PARENT context: %v", parent.Err())
	}
	got := fx.logs.attrs("sweep")
	if got["checked"] != "2" || got["deferred"] != "2" || got["failed"] != "0" || got["destroyed"] != "0" {
		t.Fatalf("the tick's own log line = %v, want checked=2 deferred=2 failed=0 destroyed=0", got)
	}
	if _, e := s.Totals(); e != 0 {
		t.Fatalf("a tick that ran out of budget counted %d sweep errors", e)
	}
	if n := fx.logs.count("sweep: destroy failed; backing this clip off", slog.LevelWarn); n != 0 {
		t.Fatalf("backoff WARNs on a deferred tick = %d, want 0", n)
	}

	// The next tick, with room to work, catches up: no clip was suppressed.
	// "Room" is a sweeper on the default interval, not the one the loop above
	// settled on: that one has the SHORTEST budget the box could enter (50 ms
	// on a quiet box), and under -race beside sixteen other shards two
	// destroys can outrun it — the catch-up then defers one and the test
	// fails on load, not on the sweeper (a nit sweep, run 6; the 44th gate).
	// A deferral marks nothing in the database, so a fresh sweeper over the
	// same fixture sees the same two candidates.
	fx.fake.Hook = nil
	fx.logs.reset()
	roomy := fx.cfg()
	roomy.SweepInterval = time.Minute
	fx.sweeper(roomy).tick(parent)
	if got := fx.logs.attrs("sweep"); got["checked"] != "2" || got["destroyed"] != "2" || got["deferred"] != "" {
		t.Fatalf("the catch-up tick = %v, want checked=2 destroyed=2 and no deferral", got)
	}
}

// TestStartBlocksAndTicks: Start runs ticks on the interval, blocks until
// ctx is cancelled, then returns promptly.
func TestStartBlocksAndTicks(t *testing.T) {
	fx := newFixture(t)
	c := fx.ready()
	fx.expire(c, time.Minute)
	cfg := fx.cfg()
	cfg.SweepInterval = 20 * time.Millisecond
	s := fx.sweeper(cfg)
	s.now = time.Now

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Start(ctx)
		close(done)
	}()
	// The allowances here are WALL-CLOCK and are scaled for the build (a
	// harness fix after Gate 11): under -race, on a box carrying other work, five seconds
	// was not enough for a 20 ms-interval sweeper to destroy one clip, and the
	// gate failed on the harness rather than on the product.
	//
	// It waits for BOTH facts it asserts — the clip destroyed and a tick past the
	// boot run — instead of sleeping 60 ms and hoping a second tick fitted in
	// (a loaded box could stall the ticker past it).
	until := time.Now().Add(deadline(5 * time.Second))
	for {
		if d, _ := s.Totals(); d == 1 && s.ticks.Load() >= 2 {
			break
		}
		if time.Now().After(until) {
			d, _ := s.Totals()
			t.Fatalf("Start never destroyed the expired clip and ticked again (destroyed=%d ticks=%d)", d, s.ticks.Load())
		}
		time.Sleep(5 * time.Millisecond) // sleep: the loop reports no tick; Totals and ticks are what the test asserts
	}
	select {
	case <-done:
		t.Fatal("Start returned while ctx was live")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(deadline(2 * time.Second)):
		t.Fatal("Start did not return after cancel")
	}
	if fx.state(c.ID) != "gone" {
		t.Fatal("clip survived")
	}
	if s.ticks.Load() < 2 {
		t.Fatalf("ticks = %d, want the boot run plus interval ticks", s.ticks.Load())
	}

	// A ctx cancelled before the boot delay elapses: Start returns at once.
	s2 := fx.sweeper(fx.cfg())
	s2.bootDelay = time.Hour
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	start := time.Now()
	s2.Start(cctx)
	if time.Since(start) > deadline(time.Second) || s2.ticks.Load() != 0 {
		t.Fatal("Start with a dead ctx ran or blocked")
	}
}

// TestSessionsPrunedEachTick: expired sessions go on every tick.
func TestSessionsPrunedEachTick(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	now := fx.now.Unix()
	if err := fx.db.CreateSession(ctx, "expired", fx.owner.ID, now-7200, now-3600, 0, "ua"); err != nil {
		t.Fatal(err)
	}
	if err := fx.db.CreateSession(ctx, "live", fx.owner.ID, now, now+3600, 0, "ua"); err != nil {
		t.Fatal(err)
	}
	fx.sweeper(fx.cfg()).RunOnce(ctx)
	if n, err := fx.db.DeleteExpiredSessions(ctx, now); err != nil || n != 0 {
		t.Fatalf("expired session survived the tick: n=%d err=%v", n, err)
	}
	if _, _, err := fx.db.GetSession(ctx, "live", now); err != nil {
		t.Fatalf("live session pruned: %v", err)
	}
	if fx.logs.count("sweep: pruned sessions", slog.LevelInfo) != 1 {
		t.Fatal("prune not logged")
	}
}

// TestJanitorEveryTenthTick: the full §12 rule runs on the 10th tick only.
//
// `ready` is where the rule is subtle (§20 erratum 89): a trim and a gif run
// against a clip that is ready for the whole run and write into that clip's
// own scratch dir, so a ready clip's dir is removable only when nothing is
// running for it — no queued/running job row, and no run registered in this
// process.
func TestJanitorEveryTenthTick(t *testing.T) {
	fx := newFixture(t)
	ready := fx.ready()
	trimming := fx.ready()
	registry := fx.ready()
	failed := fx.failed()
	processing := fx.processing()
	deleting := fx.ready()
	if err := fx.db.MarkDeleting(context.Background(), deleting.ID, fx.now.Unix()); err != nil {
		t.Fatal(err)
	}
	queuedFresh := fx.clip() // queued clips last: ClaimJob takes the oldest queued job
	queuedStale := fx.clip()
	// Keep the tombstone from being swept so the janitor branch is what removes its dir.
	fx.fake.Hook = func(_ context.Context, op, _ string) error {
		if op == "DeletePrefix" {
			return errors.New("storage down")
		}
		return nil
	}
	// A live trim of a ready clip: the job row is the fact the janitor reads.
	if _, err := fx.db.EnqueueTrim(context.Background(), trimming.ID,
		db.MarshalTrimParams(db.TrimParams{StartMS: 0, EndMS: 2000}), fx.now.Unix()); err != nil {
		t.Fatal(err)
	}
	// And a run this process has registered but that has no job row at all
	// (the registry is the in-process half of the guard).
	fx.worker.setBusy(registry.ID)
	names := []string{ready.ID, trimming.ID, registry.ID, failed.ID, processing.ID, queuedFresh.ID, queuedStale.ID, deleting.ID, "aZ3kQ9xB2mLp", "notanid"}
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(fx.scratch, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(fx.scratch, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := fx.now.Add(-janitorStaleAge - time.Hour)
	for _, n := range []string{processing.ID, queuedStale.ID} {
		if err := os.Chtimes(filepath.Join(fx.scratch, n), stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	s := fx.sweeper(fx.cfg())
	exists := func(n string) bool {
		_, err := os.Stat(filepath.Join(fx.scratch, n))
		return err == nil
	}
	for i := 1; i <= 9; i++ {
		s.RunOnce(context.Background())
	}
	for _, n := range names {
		if !exists(n) {
			t.Fatalf("%s removed before the 10th tick", n)
		}
	}
	s.RunOnce(context.Background())
	gone := map[string]bool{
		ready.ID: true, failed.ID: true, deleting.ID: true, "aZ3kQ9xB2mLp": true, queuedStale.ID: true,
		processing.ID: false, queuedFresh.ID: false, "notanid": false, "stray.txt": false,
		// The two ready clips something is working on keep their dirs.
		trimming.ID: false, registry.ID: false,
	}
	for n, wantGone := range gone {
		if exists(n) == wantGone {
			t.Errorf("%s: exists=%v, want gone=%v", n, exists(n), wantGone)
		}
	}
	if fx.logs.count("janitor", slog.LevelInfo) != 1 {
		t.Fatalf("janitor INFO lines = %d", fx.logs.count("janitor", slog.LevelInfo))
	}
}

// TestJanitorSparesLiveUploadSpools: a row-less upload spool
// ($SCRATCH_DIR/.upload-{id}) belongs to an in-flight upload — the janitor
// must never remove a fresh one (an upload spanning a janitor tick used to
// lose its spool and fail), and removes only crash residue past the stale
// bound. A stale-looking spool whose FILE was just written (a slow upload
// still spooling) is live too.
func TestJanitorSparesLiveUploadSpools(t *testing.T) {
	fx := newFixture(t)
	mk := func(name string, dirAge, fileAge time.Duration) string {
		p := filepath.Join(fx.scratch, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(p, "source.mp4")
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		at := fx.now.Add(-fileAge)
		if err := os.Chtimes(f, at, at); err != nil {
			t.Fatal(err)
		}
		at = fx.now.Add(-dirAge)
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
		return name
	}
	tmpName := func() string { return filepath.Base(scratch.UploadTmpDir(fx.scratch, ids.New())) }
	fresh := mk(tmpName(), 0, 0)
	spooling := mk(tmpName(), janitorStaleAge+time.Hour, time.Minute)
	stale := mk(tmpName(), janitorStaleAge+time.Hour, janitorStaleAge+time.Hour)
	// A row-less dir under a plain clip id is still removed immediately: with
	// uploads spooling under .upload-*, such a dir is always residue.
	rowless := filepath.Join(fx.scratch, "aZ3kQ9xB2mLp")
	if err := os.MkdirAll(rowless, 0o755); err != nil {
		t.Fatal(err)
	}
	s := fx.sweeper(fx.cfg())
	for i := 0; i < 10; i++ {
		s.RunOnce(context.Background())
	}
	for name, wantGone := range map[string]bool{fresh: false, spooling: false, stale: true, "aZ3kQ9xB2mLp": true} {
		_, err := os.Stat(filepath.Join(fx.scratch, name))
		if gone := os.IsNotExist(err); gone != wantGone {
			t.Errorf("%s: gone=%v, want %v", name, gone, wantGone)
		}
	}
	if fx.logs.maxLevel() > slog.LevelInfo {
		t.Fatalf("janitor logged above INFO: %v", fx.logs.maxLevel())
	}
}

// TestDestroyExpiredSkipsRescuedClip: a TTL extension committed after
// candidate selection wins — DestroyExpired re-verifies inside the
// tombstone write and skips, so an acknowledged PATCH is never inverted.
func TestDestroyExpiredSkipsRescuedClip(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	rescued := fx.ready()
	fx.expire(rescued, time.Minute)
	doomed := fx.ready()
	fx.expire(doomed, time.Minute)
	now := fx.now.Unix()
	deps := lifecycle.Deps{DB: fx.db, Store: fx.fake, Worker: fx.worker, ScratchDir: fx.scratch, Log: slog.New(fx.logs), OpTimeout: 5 * time.Second}

	// Both were listed as candidates; then the owner rescues one (expiry
	// moved to the future — the PATCH ttl_days path recomputes expires_at).
	fx.expire(rescued, -time.Hour)
	res, err := lifecycle.DestroyExpired(ctx, deps, rescued.ID, now, 3600)
	if err != nil || !res.Skipped || res.RowDeleted {
		t.Fatalf("rescued: %+v, %v", res, err)
	}
	if got := fx.state(rescued.ID); got != db.ClipReady {
		t.Fatalf("rescued clip state = %q, want ready", got)
	}
	if _, ok := fx.fake.Get(store.OriginalKey("", rescued.ID, "mp4")); !ok {
		t.Fatal("rescued clip's objects were deleted")
	}
	if len(fx.worker.cancelled()) != 0 {
		t.Fatal("rescued clip's job was cancelled")
	}
	if fx.logs.maxLevel() > slog.LevelInfo {
		t.Fatalf("skip logged above INFO: %v", fx.logs.maxLevel())
	}

	// A clip still expired at the re-check is destroyed as before, and a
	// deleting tombstone is resumed whatever its expiry says.
	if res, err := lifecycle.DestroyExpired(ctx, deps, doomed.ID, now, 3600); err != nil || res.Skipped || !res.RowDeleted {
		t.Fatalf("doomed: %+v, %v", res, err)
	}
	tomb := fx.ready()
	if err := fx.db.MarkDeleting(ctx, tomb.ID, now); err != nil {
		t.Fatal(err)
	}
	if res, err := lifecycle.DestroyExpired(ctx, deps, tomb.ID, now, 3600); err != nil || res.Skipped || !res.RowDeleted {
		t.Fatalf("tombstone: %+v, %v", res, err)
	}
}

// The Phase 4 #2 keep-alive is only worth anything if it wins the race it
// exists for: POST /api/clips/{id}/touch landing after SweepCandidates has
// already listed the clip, but before DestroyExpired reaches it. Erratum 15
// is what makes that a rescue — the tombstone UPDATE re-checks the candidate
// predicate — so this drives the real db.TouchClip against a real tick and
// asserts the clip, its objects and its job all survive, while a clip left
// alone in the same tick is destroyed as usual.
func TestTouchedClipSurvivesTheTick(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	rescued := fx.ready()
	doomed := fx.ready()
	// Both are a day past a 7-day expiry, i.e. both are candidates.
	ttl := int64(7 * 86400)
	ttlp := &ttl
	for _, c := range []db.Clip{rescued, doomed} {
		if _, err := fx.db.UpdateClipMeta(ctx, c.ID, db.ClipPatch{TTLSeconds: &ttlp}, fx.now.Unix()); err != nil {
			t.Fatal(err)
		}
		fx.expire(c, 24*time.Hour)
	}
	now, grace := fx.now.Unix(), int64(3600)
	deps := lifecycle.Deps{DB: fx.db, Store: fx.fake, Worker: fx.worker, ScratchDir: fx.scratch,
		Log: slog.New(fx.logs), OpTimeout: 5 * time.Second}

	// The tick opens: both clips are on its candidate list.
	cands, err := fx.db.SweepCandidates(ctx, now, grace, 50)
	if err != nil || len(cands) != 2 {
		t.Fatalf("candidates = %v, %v", cands, err)
	}

	// The owner copies the link (the share page's copy button) while the
	// tick is still working through that list: one touch, and the countdown
	// restarts from now — the exact window erratum 15 covers.
	touched, err := fx.db.TouchClip(ctx, rescued.ID, now)
	if err != nil || touched.ExpiresAt == nil || *touched.ExpiresAt != now+ttl {
		t.Fatalf("touch: %v %v", touched.ExpiresAt, err)
	}

	// The tick now reaches its (stale) candidates and destroys them.
	res, err := lifecycle.DestroyExpired(ctx, deps, rescued.ID, now, grace)
	if err != nil || !res.Skipped || res.RowDeleted || res.Objects != 0 {
		t.Fatalf("touched candidate: %+v, %v", res, err)
	}
	if res, err := lifecycle.DestroyExpired(ctx, deps, doomed.ID, now, grace); err != nil || res.Skipped || !res.RowDeleted {
		t.Fatalf("untouched candidate: %+v, %v", res, err)
	}
	if got := fx.state(rescued.ID); got != db.ClipReady {
		t.Fatalf("touched clip state = %q, want ready", got)
	}
	if _, ok := fx.fake.Get(store.OriginalKey("", rescued.ID, "mp4")); !ok {
		t.Fatal("the touched clip's objects were deleted by the tick")
	}
	if got := fx.worker.cancelled(); len(got) != 1 || got[0] != doomed.ID {
		t.Fatalf("cancelled = %v, want only the untouched clip %s", got, doomed.ID)
	}
	if got := fx.state(doomed.ID); got != "gone" {
		t.Fatalf("untouched clip state = %q, want gone", got)
	}
	if got := fx.logs.notes("destroy"); len(got) != 1 || got[0] != "no longer expired; skipped" {
		t.Fatalf("destroy note = %q, want [no longer expired; skipped]", got)
	}
	if fx.logs.maxLevel() > slog.LevelInfo {
		t.Fatalf("a rescue logged above INFO: %v", fx.logs.maxLevel())
	}

	// A whole tick later the touched clip is not even a candidate any more.
	s := fx.sweeper(fx.cfg())
	if st := s.RunOnce(ctx); st.Checked != 0 {
		t.Fatalf("next tick = %+v, want nothing left to check", st)
	}
	if got := fx.state(rescued.ID); got != db.ClipReady {
		t.Fatalf("touched clip state after a full tick = %q, want ready", got)
	}

	// The rescue is a reprieve, not immunity: when the restarted TTL runs
	// out the clip is destroyed like any other.
	fx.expire(rescued, time.Minute)
	if st := s.RunOnce(ctx); st.Checked != 1 || st.Destroyed != 1 {
		t.Fatalf("tick after the restarted TTL ran out = %+v", st)
	}
	if got := fx.state(rescued.ID); got != "gone" {
		t.Fatalf("state after the restarted TTL ran out = %q, want gone", got)
	}
}

func TestTickBudget(t *testing.T) {
	cases := map[time.Duration]time.Duration{
		0:                55 * time.Second,
		time.Minute:      55 * time.Second,
		2 * time.Minute:  115 * time.Second,
		10 * time.Second: 5 * time.Second,
		4 * time.Second:  2 * time.Second,
	}
	for in, want := range cases {
		if got := tickBudget(in); got != want {
			t.Errorf("tickBudget(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestRunOnceDBErrorIsLogged(t *testing.T) {
	fx := newFixture(t)
	s := fx.sweeper(fx.cfg())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st := s.RunOnce(ctx)
	if st.Checked != 0 {
		t.Fatalf("Stats = %+v", st)
	}
	if fx.logs.count("sweep: candidates", slog.LevelInfo) != 1 {
		t.Fatal("a cancelled tick must log the candidates failure at INFO")
	}
}

// MarkDeletingIfDue answers the same ErrStateMismatch for two very different
// things, and the destroy line used to call both of them "no longer
// expired". Rule (b) covers only queued/ready/failed, so a candidate the
// worker CLAIMS between SweepCandidates and DestroyExpired is `processing`
// and matches neither (b) nor rule (c) — but it is still past its expiry: it
// has entered the SWEEP_GRACE window §12 grants a clip that is mid-encode at
// its expiry, and the first tick past that window destroys it. Telling the
// operator such a clip "is no longer expired", under the counter erratum 15
// introduced for clips an owner RESCUED, names the wrong cause (§20
// erratum 51).
func TestDestroyExpiredTellsAGraceDeferralFromARescue(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	now := fx.now.Unix()
	deps := lifecycle.Deps{DB: fx.db, Store: fx.fake, Worker: fx.worker, ScratchDir: fx.scratch, Log: slog.New(fx.logs), OpTimeout: 5 * time.Second}

	// Listed as a candidate while queued and one minute past expiry, then
	// claimed by the worker before its turn in the tick's loop.
	deferred := fx.clip()
	fx.expire(deferred, time.Minute)
	fx.claim(deferred)
	res, err := lifecycle.DestroyExpired(ctx, deps, deferred.ID, now, 3600)
	if err != nil || !res.Skipped || res.RowDeleted || res.Objects != 0 {
		t.Fatalf("deferred: %+v, %v", res, err)
	}
	if got := fx.logs.notes("destroy"); len(got) != 1 || got[0] != "in grace; deferred" {
		t.Fatalf("note = %q, want [in grace; deferred]", got)
	}
	if got := fx.state(deferred.ID); got != db.ClipProcessing {
		t.Fatalf("deferred clip state = %q", got)
	}
	if _, ok := fx.fake.Get(store.OriginalKey("", deferred.ID, "mp4")); !ok {
		t.Fatal("a deferred clip's objects were deleted")
	}
	// Past the grace window rule (c) matches whatever the state, and the
	// same clip is destroyed on the next tick.
	if res, err := lifecycle.DestroyExpired(ctx, deps, deferred.ID, now, 30); err != nil || res.Skipped || !res.RowDeleted {
		t.Fatalf("after grace: %+v, %v", res, err)
	}

	// A genuine rescue keeps the note erratum 15 wrote it for.
	fx.logs.reset()
	rescued := fx.ready()
	fx.expire(rescued, time.Minute)
	fx.expire(rescued, -time.Hour) // the owner PATCHes ttl_days
	if res, err := lifecycle.DestroyExpired(ctx, deps, rescued.ID, now, 3600); err != nil || !res.Skipped {
		t.Fatalf("rescued: %+v, %v", res, err)
	}
	if got := fx.logs.notes("destroy"); len(got) != 1 || got[0] != "no longer expired; skipped" {
		t.Fatalf("note = %q, want [no longer expired; skipped]", got)
	}
	if fx.logs.maxLevel() > slog.LevelInfo {
		t.Fatalf("skip logged above INFO: %v", fx.logs.maxLevel())
	}
}
