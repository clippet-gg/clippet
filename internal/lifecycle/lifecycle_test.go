package lifecycle

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/store"
	"clippet/internal/worker"
)

// --- fixtures -------------------------------------------------------------

func openDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "clippet.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return d
}

func mkUser(t *testing.T, d *db.DB) db.User {
	t.Helper()
	u, err := d.CreateUser(context.Background(), ids.New(), "carol", "$2a$12$hash", 1000)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// clipKeys are the four objects a ready clip owns.
func clipKeys(id string) []string {
	k := store.KeysFor("", id, "mp4")
	return []string{k.Original, store.RenditionKey("", id, "r8r8r8r8"), store.PosterKey("", id, "r8r8r8r8"), k.Probe}
}

// seedClip inserts a queued clip, seeds its four objects in the Fake and a
// scratch dir with a source file.
func seedClip(t *testing.T, d *db.DB, f *store.Fake, owner db.User, scratch string) db.Clip {
	t.Helper()
	id := ids.New()
	keys := clipKeys(id)
	c, _, err := d.CreateClipWithJob(context.Background(), db.NewClip{
		ID: id, OwnerID: owner.ID, Title: "t " + id, SourceKey: keys[0], SourceFilename: "a.mp4",
		SourceSize: 3, SourceContentType: "video/mp4", SourceETag: "e", Now: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		f.Put(k, []byte("data-"+k), "application/octet-stream")
	}
	if scratch != "" {
		dir := filepath.Join(scratch, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "source.mp4"), []byte("src"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// fakeWorker records Cancel/Forget calls and appends to a shared ordering
// log; Cancel optionally blocks until unblock is closed.
type fakeWorker struct {
	mu      sync.Mutex
	events  func(string)
	cancels []string
	forgets []string
	unblock chan struct{}
	// entered, when set, is closed by the first Cancel as it begins to wait, so a
	// test knows Destroy is inside Cancel without sleeping for it.
	entered     chan struct{}
	enteredOnce sync.Once
}

func (w *fakeWorker) Start(context.Context)                       {}
func (w *fakeWorker) Stop(context.Context) error                  { return nil }
func (w *fakeWorker) Wake()                                       {}
func (w *fakeWorker) Progress(string) (worker.Progress, bool)     { return worker.Progress{}, false }
func (w *fakeWorker) TrimProgress(string) (worker.Progress, bool) { return worker.Progress{}, false }
func (w *fakeWorker) Busy(string) bool                            { return false }
func (w *fakeWorker) AwaitTerminal(string) (<-chan struct{}, func()) {
	return make(chan struct{}), func() {}
}
func (w *fakeWorker) Cancel(ctx context.Context, id string) bool {
	w.mu.Lock()
	w.cancels = append(w.cancels, id)
	w.mu.Unlock()
	if w.events != nil {
		w.events("Cancel " + id)
	}
	if w.entered != nil {
		w.enteredOnce.Do(func() { close(w.entered) })
	}
	if w.unblock != nil {
		// AUDIT #13's shape: the wait is bounded by the caller's context, and a
		// ctx that ends first is answered `false` — the run is still exiting.
		select {
		case <-w.unblock:
		case <-ctx.Done():
			return false
		}
	}
	return true
}
func (w *fakeWorker) Forget(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.forgets = append(w.forgets, id)
	if w.events != nil {
		w.events("Forget " + id)
	}
}
func (w *fakeWorker) snapshot() (cancels, forgets []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.cancels...), append([]string(nil), w.forgets...)
}

var _ worker.Worker = (*fakeWorker)(nil)

// recHandler captures every log record (levels + messages) for assertions.
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
func (h *recHandler) messages(level slog.Level) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.recs {
		if r.Level == level {
			out = append(out, r.Message)
		}
	}
	return out
}

type fixture struct {
	db      *db.DB
	fake    *store.Fake
	worker  *fakeWorker
	logs    *recHandler
	scratch string
	owner   db.User
	events  []string
	mu      sync.Mutex
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	fx := &fixture{db: openDB(t), fake: store.NewFake(), worker: &fakeWorker{}, logs: &recHandler{}, scratch: filepath.Join(t.TempDir(), "scratch")}
	fx.owner = mkUser(t, fx.db)
	record := func(ev string) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		fx.events = append(fx.events, ev)
	}
	fx.worker.events = record
	fx.fake.Hook = func(_ context.Context, op, key string) error {
		record(op + " " + key)
		return nil
	}
	return fx
}

func (fx *fixture) deps() Deps {
	return Deps{DB: fx.db, Store: fx.fake, Worker: fx.worker, ScratchDir: fx.scratch, Log: slog.New(fx.logs), OpTimeout: 5 * time.Second}
}

func (fx *fixture) state(t *testing.T, id string) string {
	t.Helper()
	c, err := fx.db.GetClip(context.Background(), id)
	if errors.Is(err, db.ErrNotFound) {
		return "gone"
	}
	if err != nil {
		t.Fatal(err)
	}
	return c.State
}

// --- tests ----------------------------------------------------------------

// TestDestroyOrderAndOutcome: tombstone → Cancel → DeletePrefix (row still
// deleting at that moment) → row gone → scratch gone → Forget; only this
// clip's objects are removed; the Result is exact.
func TestDestroyOrderAndOutcome(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	victim := seedClip(t, fx.db, fx.fake, fx.owner, fx.scratch)
	other := seedClip(t, fx.db, fx.fake, fx.owner, fx.scratch)

	// The store hook doubles as the "row still deleting while objects go"
	// probe: it runs at the start of DeletePrefix.
	var stateAtDelete string
	prev := fx.fake.Hook
	fx.fake.Hook = func(ctx context.Context, op, key string) error {
		if op == "DeletePrefix" {
			stateAtDelete = fx.state(t, victim.ID)
		}
		return prev(ctx, op, key)
	}

	res, err := Destroy(ctx, fx.deps(), victim.ID)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if res != (Result{Objects: 4, RowDeleted: true}) {
		t.Fatalf("Result = %+v", res)
	}
	if stateAtDelete != db.ClipDeleting {
		t.Fatalf("state during DeletePrefix = %q, want deleting (tombstone before objects)", stateAtDelete)
	}
	wantEvents := []string{"Cancel " + victim.ID, "DeletePrefix clips/" + victim.ID + "/", "Forget " + victim.ID}
	if !reflect.DeepEqual(fx.events, wantEvents) {
		t.Fatalf("events = %v, want %v", fx.events, wantEvents)
	}
	if got := fx.state(t, victim.ID); got != "gone" {
		t.Fatalf("row state after Destroy = %q", got)
	}
	if _, err := fx.db.GetLatestJob(ctx, victim.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("jobs did not cascade: %v", err)
	}
	wantDeletes := append([]string(nil), clipKeys(victim.ID)...)
	sort.Strings(wantDeletes) // the Fake deletes (and records) in key order
	if got := fx.fake.Deletes(); !reflect.DeepEqual(got, wantDeletes) {
		t.Fatalf("Deletes = %v, want %v", got, wantDeletes)
	}
	for _, k := range clipKeys(other.ID) {
		if _, ok := fx.fake.Get(k); !ok {
			t.Fatalf("other clip's object %s was deleted", k)
		}
	}
	if fx.state(t, other.ID) != db.ClipQueued {
		t.Fatal("other clip's row was touched")
	}
	if _, err := os.Stat(filepath.Join(fx.scratch, victim.ID)); !os.IsNotExist(err) {
		t.Fatalf("scratch dir survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fx.scratch, other.ID)); err != nil {
		t.Fatalf("other clip's scratch dir removed: %v", err)
	}
	if got := fx.logs.maxLevel(); got > slog.LevelInfo {
		t.Fatalf("a successful destroy logged at %v", got)
	}
	if msgs := fx.logs.messages(slog.LevelInfo); len(msgs) != 1 || msgs[0] != "destroy" {
		t.Fatalf("INFO messages = %v", msgs)
	}
}

// TestDestroyStorageErrorLeavesTombstoneThenResumes: a DeletePrefix failure
// keeps the row in deleting with its objects intact, logs at WARN (never
// ERROR), and a later Destroy finishes the job.
func TestDestroyStorageErrorLeavesTombstoneThenResumes(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	c := seedClip(t, fx.db, fx.fake, fx.owner, fx.scratch)
	boom := errors.New("s3 is down")
	fx.fake.FailNext("DeletePrefix", boom)

	res, err := Destroy(ctx, fx.deps(), c.ID)
	if !errors.Is(err, boom) {
		t.Fatalf("Destroy = %+v, %v; want boom", res, err)
	}
	if res.RowDeleted {
		t.Fatal("RowDeleted reported despite the storage error")
	}
	if got := fx.state(t, c.ID); got != db.ClipDeleting {
		t.Fatalf("state = %q, want deleting", got)
	}
	if len(fx.fake.Deletes()) != 0 {
		t.Fatalf("objects deleted despite the error: %v", fx.fake.Deletes())
	}
	if _, forgets := fx.worker.snapshot(); len(forgets) != 0 {
		t.Fatal("Forget called on a failed destroy")
	}
	if got := fx.logs.maxLevel(); got != slog.LevelWarn {
		t.Fatalf("max log level = %v, want WARN", got)
	}
	if _, err := os.Stat(filepath.Join(fx.scratch, c.ID)); err != nil {
		t.Fatalf("scratch removed before the row: %v", err)
	}

	// Resume: MarkDeleting is idempotent, Cancel runs again, objects go, row goes.
	res, err = Destroy(ctx, fx.deps(), c.ID)
	if err != nil || res != (Result{Objects: 4, RowDeleted: true}) {
		t.Fatalf("resume: %+v, %v", res, err)
	}
	if fx.state(t, c.ID) != "gone" {
		t.Fatal("row survived the resumed destroy")
	}
	cancels, forgets := fx.worker.snapshot()
	if len(cancels) != 2 || len(forgets) != 1 {
		t.Fatalf("cancels=%v forgets=%v", cancels, forgets)
	}
}

// TestDestroyTimeoutHonoured: a black-holed DeletePrefix (Fake.Hook blocking
// on ctx.Done()) returns within OpTimeout + margin with DeadlineExceeded;
// the row stays deleting.
func TestDestroyTimeoutHonoured(t *testing.T) {
	fx := newFixture(t)
	c := seedClip(t, fx.db, fx.fake, fx.owner, fx.scratch)
	fx.fake.Hook = func(ctx context.Context, op, key string) error {
		if op != "DeletePrefix" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
			return nil
		}
	}
	deps := fx.deps()
	deps.OpTimeout = 150 * time.Millisecond
	start := time.Now()
	_, err := Destroy(context.Background(), deps, c.ID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Destroy = %v, want DeadlineExceeded", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("Destroy took %s; OpTimeout was 150ms", el)
	}
	if got := fx.state(t, c.ID); got != db.ClipDeleting {
		t.Fatalf("state = %q, want deleting", got)
	}
	if got := fx.logs.maxLevel(); got != slog.LevelWarn {
		t.Fatalf("max log level = %v, want WARN", got)
	}
}

// TestDestroyCallerCancelIsInfo: a cancelled caller ctx (client gone,
// shutdown) is an expected outcome — INFO, tombstone kept.
func TestDestroyCallerCancelIsInfo(t *testing.T) {
	fx := newFixture(t)
	c := seedClip(t, fx.db, fx.fake, fx.owner, fx.scratch)
	ctx, cancel := context.WithCancel(context.Background())
	fx.fake.Hook = func(hctx context.Context, op, key string) error {
		if op == "DeletePrefix" {
			cancel()
			<-hctx.Done()
			return hctx.Err()
		}
		return nil
	}
	_, err := Destroy(ctx, fx.deps(), c.ID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Destroy = %v, want Canceled", err)
	}
	if got := fx.logs.maxLevel(); got != slog.LevelInfo {
		t.Fatalf("max log level = %v, want INFO", got)
	}
	if fx.state(t, c.ID) != db.ClipDeleting {
		t.Fatal("tombstone lost")
	}
}

// TestDestroyAlreadyGone: an unknown id is not an error and touches nothing.
func TestDestroyAlreadyGone(t *testing.T) {
	fx := newFixture(t)
	res, err := Destroy(context.Background(), fx.deps(), "aZ3kQ9xB2mLp")
	if err != nil || res != (Result{RowDeleted: true}) {
		t.Fatalf("Destroy = %+v, %v", res, err)
	}
	if len(fx.fake.CallsSnapshot()) != 0 {
		t.Fatalf("store touched: %v", fx.fake.CallsSnapshot())
	}
	if cancels, _ := fx.worker.snapshot(); len(cancels) != 0 {
		t.Fatal("Cancel called for an unknown clip")
	}
}

// TestDestroyRowRace: if the row disappears while objects are being deleted
// (a concurrent Destroy finished), the outcome is still success.
func TestDestroyRowRace(t *testing.T) {
	fx := newFixture(t)
	c := seedClip(t, fx.db, fx.fake, fx.owner, "")
	fx.fake.Hook = func(ctx context.Context, op, key string) error {
		if op == "DeletePrefix" {
			if err := fx.db.DeleteClipRow(ctx, c.ID); err != nil {
				t.Errorf("row delete inside hook: %v", err)
			}
		}
		return nil
	}
	res, err := Destroy(context.Background(), fx.deps(), c.ID)
	if err != nil || !res.RowDeleted || res.Objects != 4 {
		t.Fatalf("Destroy = %+v, %v", res, err)
	}
}

// TestDestroyCancelBlocksBeforeDelete: DeletePrefix is not issued until the
// (blocking) Cancel has returned.
func TestDestroyCancelBlocksBeforeDelete(t *testing.T) {
	fx := newFixture(t)
	c := seedClip(t, fx.db, fx.fake, fx.owner, "")
	fx.worker.unblock = make(chan struct{})
	fx.worker.entered = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := Destroy(context.Background(), fx.deps(), c.ID)
		done <- err
	}()
	// Destroy is inside Cancel (it signalled), not merely 100 ms older: a
	// loaded box could still have it short of MarkDeleting when the test looked.
	select {
	case <-fx.worker.entered:
	case <-time.After(time.Minute): // a hang guard, never a budget
		t.Fatal("Destroy never reached Cancel")
	}
	if calls := fx.fake.CallsSnapshot(); len(calls) != 0 {
		t.Fatalf("store called while Cancel was blocking: %v", calls)
	}
	if got := fx.state(t, c.ID); got != db.ClipDeleting {
		t.Fatalf("state while cancelling = %q, want deleting", got)
	}
	close(fx.worker.unblock)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Destroy did not finish after Cancel returned")
	}
	if calls := fx.fake.CallsSnapshot(); len(calls) != 1 || !strings.HasPrefix(calls[0], "DeletePrefix ") {
		t.Fatalf("calls = %v", calls)
	}
}

// TestDestroyKeyPrefixAndNilWorker: the prefix honours S3_KEY_PREFIX and a
// nil Worker / empty ScratchDir are tolerated.
func TestDestroyKeyPrefixAndNilWorker(t *testing.T) {
	fx := newFixture(t)
	id := ids.New()
	if _, _, err := fx.db.CreateClipWithJob(context.Background(), db.NewClip{
		ID: id, OwnerID: fx.owner.ID, Title: "t", SourceKey: "team/clips/" + id + "/original/source.mp4",
		SourceFilename: "a.mp4", SourceSize: 1, SourceContentType: "video/mp4", SourceETag: "e", Now: 1,
	}); err != nil {
		t.Fatal(err)
	}
	fx.fake.Put("team/clips/"+id+"/original/source.mp4", []byte("x"), "video/mp4")
	fx.fake.Put("clips/"+id+"/original/source.mp4", []byte("y"), "video/mp4") // a different bucket layout: untouched
	deps := Deps{DB: fx.db, Store: fx.fake, KeyPrefix: "team/", Log: slog.New(fx.logs)}
	res, err := Destroy(context.Background(), deps, id)
	if err != nil || res != (Result{Objects: 1, RowDeleted: true}) {
		t.Fatalf("Destroy = %+v, %v", res, err)
	}
	if got := fx.fake.Deletes(); !reflect.DeepEqual(got, []string{"team/clips/" + id + "/original/source.mp4"}) {
		t.Fatalf("Deletes = %v", got)
	}
}

// TestDestroyLeavesTheTombstoneWhenTheRunIsStillExiting (AUDIT #13): a Cancel
// the Destroy's own context outlives removes NOTHING and leaves the clip
// `deleting` for the next sweep tick.
//
// Cancel used to wait on the run's `done` channel alone, so a run whose exit
// path was stuck — the one unbounded step in it is the scratch RemoveAll, on a
// filesystem that is not answering — held the whole sequence: the sweeper's
// tick, or a DELETE's DestroyBudget goroutine, for as long as the kernel took.
// The rule is unchanged and is the one §12 already gives every failure: while
// anything can still write into the prefix, nothing in it may be removed, so
// the tombstone stays and rule (a) picks the clip up next tick.
func TestDestroyLeavesTheTombstoneWhenTheRunIsStillExiting(t *testing.T) {
	fx := newFixture(t)
	c := seedClip(t, fx.db, fx.fake, fx.owner, fx.scratch)
	fx.worker.unblock = make(chan struct{}) // never closed: the run never exits
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	var res Result
	var err error
	go func() { res, err = Destroy(ctx, fx.deps(), c.ID); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Destroy blocked past its own context: Cancel is not bounded by it")
	}

	if !errors.Is(err, ErrStillRunning) {
		t.Fatalf("Destroy = %+v, %v; want ErrStillRunning", res, err)
	}
	if res.RowDeleted || res.Objects != 0 {
		t.Fatalf("Destroy removed something: %+v", res)
	}
	if calls := fx.fake.CallsSnapshot(); len(calls) != 0 {
		t.Fatalf("the store was called while the run could still write: %v", calls)
	}
	if got := fx.state(t, c.ID); got != db.ClipDeleting {
		t.Fatalf("state = %q, want the deleting tombstone the next tick resumes from", got)
	}
	// The scratch dir is the run's until the run is gone.
	if _, serr := os.Stat(filepath.Join(fx.scratch, c.ID)); serr != nil {
		t.Fatalf("the scratch dir of a still-running clip was removed: %v", serr)
	}
	// INFO, not WARN: an expected outcome, the level a cancellation gets.
	if lvl := fx.logs.maxLevel(); lvl > slog.LevelInfo {
		t.Fatalf("a still-exiting run logged at %v; it is an expected outcome and stays INFO", lvl)
	}
	if msgs := fx.logs.messages(slog.LevelInfo); len(msgs) == 0 {
		t.Fatal("no INFO destroy line at all")
	}
}
