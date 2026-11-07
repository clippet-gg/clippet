package cost

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// testLogger keeps the counter's WARN about a failed write out of the test
// output; the assertion is the kept bytes, not the line.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeStore is the database the counter writes to: a map of day → bytes, with a
// switch that makes the next write fail.
type fakeStore struct {
	mu     sync.Mutex
	days   map[string]int64
	writes int
	fail   error
}

func newFakeStore() *fakeStore { return &fakeStore{days: map[string]int64{}} }

func (f *fakeStore) AddBytesServed(_ context.Context, day string, n int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if f.fail != nil {
		return f.fail
	}
	f.days[day] += n
	return nil
}

func (f *fakeStore) BytesServed(_ context.Context, day, start, end string) (int64, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return 0, 0, f.fail
	}
	var today, month int64
	for d, n := range f.days {
		if d == day {
			today += n
		}
		if d >= start && d < end {
			month += n
		}
	}
	return today, month, nil
}

func (f *fakeStore) get(day string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.days[day]
}

// clock is a test clock the counter reads through. It never moves on its own,
// which is the point: the day boundary below is a decision this test makes and
// not a race with midnight.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func newTestCounter(t *testing.T, at time.Time) (*Counter, *fakeStore, *clock) {
	t.Helper()
	f, ck := newFakeStore(), &clock{t: at}
	c := New(f, testLogger(t))
	c.now = ck.now
	return c, f, ck
}

// A day boundary is the one thing a single running total would get wrong: a
// write at 23:59:59Z and one at 00:00:00Z belong to two rows, whatever the
// flush window does between them (§20 erratum 270).
func TestTheDayBoundaryMakesTwoRows(t *testing.T) {
	ctx := context.Background()
	eve := time.Date(2026, 9, 11, 23, 59, 59, 0, time.UTC)
	c, f, ck := newTestCounter(t, eve)

	c.Add(1_000)
	ck.set(time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC))
	c.Add(700)
	// Both are still unflushed, and each is already attributed to its own day.
	if got := c.Pending("2026-09-11"); got != 1_000 {
		t.Errorf("pending on the 11th = %d, want 1000", got)
	}
	if got := c.Pending("2026-09-12"); got != 700 {
		t.Errorf("pending on the 12th = %d, want 700", got)
	}
	if err := c.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := f.get("2026-09-11"); got != 1_000 {
		t.Errorf("the 11th's row = %d, want 1000", got)
	}
	if got := f.get("2026-09-12"); got != 700 {
		t.Errorf("the 12th's row = %d, want 700", got)
	}
	if c.Pending("2026-09") != 0 {
		t.Error("a flush must empty the window")
	}
	// A month boundary is the same fact one level up: the 11th's bytes are in
	// September and cannot reach October's total.
	ck.set(time.Date(2026, 10, 1, 0, 0, 1, 0, time.UTC))
	c.Add(5)
	today, month, err := c.Totals(ctx, ck.now())
	if err != nil {
		t.Fatal(err)
	}
	if today != 5 || month != 5 {
		t.Errorf("on the 1st of October: today=%d month=%d, want 5 and 5", today, month)
	}
}

// Totals is DISK PLUS the unflushed window, so the number a surface prints is
// exact at every instant rather than one flush behind the clip somebody just
// watched.
func TestTotalsIncludeTheUnflushedWindow(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	c, f, _ := newTestCounter(t, now)

	c.Add(400)
	if err := c.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	c.Add(600) // not flushed
	today, month, err := c.Totals(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if today != 1_000 || month != 1_000 {
		t.Errorf("Totals = %d, %d; want 1000 both (400 on disk + 600 in memory)", today, month)
	}
	if f.get("2026-09-11") != 400 {
		t.Errorf("the row itself must still hold only what was flushed: %d", f.get("2026-09-11"))
	}
	// An earlier month's row is not this month's business.
	f.mu.Lock()
	f.days["2026-08-31"] = 9_999
	f.mu.Unlock()
	_, month, err = c.Totals(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if month != 1_000 {
		t.Errorf("last month's bytes leaked into this month: %d", month)
	}
}

// The byte trigger: FlushBytes of serving wakes the loop before the interval
// does, which is what bounds the loss while a big file streams.
func TestTheByteTriggerWakesTheLoop(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	c, _, _ := newTestCounter(t, now)
	c.atBytes = 1_000

	c.Add(999)
	select {
	case <-c.wake:
		t.Fatal("the trigger fired under the threshold")
	default:
	}
	c.Add(1)
	select {
	case <-c.wake:
	default:
		t.Fatal("the trigger did not fire at the threshold")
	}
}

// A failed write keeps its bytes: the counter's only failure mode is being
// late, never being short.
func TestAFailedFlushKeepsTheBytes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	c, f, _ := newTestCounter(t, now)

	c.Add(1_234)
	f.fail = errors.New("database is locked")
	if err := c.Flush(ctx); err == nil {
		t.Fatal("a failed write must be reported")
	}
	if got := c.Pending("2026-09-11"); got != 1_234 {
		t.Fatalf("pending after a failed flush = %d, want the bytes kept (1234)", got)
	}
	f.mu.Lock()
	f.fail = nil
	f.mu.Unlock()
	c.Add(1) // and the next window's bytes ride along
	if err := c.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.get("2026-09-11"); got != 1_235 {
		t.Errorf("the retried flush wrote %d, want 1235", got)
	}
}

// Two shapes that must cost nothing: an empty window makes no statement at all,
// and a nil counter is a working counter that counts nothing (what a handler
// wired without one gets).
func TestAnEmptyWindowAndANilCounterCostNothing(t *testing.T) {
	ctx := context.Background()
	c, f, _ := newTestCounter(t, time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC))
	if err := c.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	c.Add(0)
	c.Add(-5)
	if err := c.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if f.writes != 0 {
		t.Errorf("an empty window wrote %d statement(s)", f.writes)
	}

	var nilCounter *Counter
	nilCounter.Add(1 << 20)
	if got := nilCounter.Pending("2026-09"); got != 0 {
		t.Errorf("a nil counter reported %d pending bytes", got)
	}
	today, month, err := nilCounter.Totals(ctx, time.Now())
	if err != nil || today != 0 || month != 0 {
		t.Errorf("a nil counter's Totals = %d, %d, %v", today, month, err)
	}
	if err := nilCounter.Flush(ctx); err != nil {
		t.Errorf("a nil counter's Flush: %v", err)
	}
	nilCounter.Run(ctx) // returns at once
}

// Run flushes on its own ticker and once more when its context ends — the
// "and on shutdown" half of the window (§20 erratum 270).
func TestRunFlushesOnTheTickAndOnTheWayOut(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	c, f, _ := newTestCounter(t, now)
	c.every = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	c.Add(11)
	deadline := time.After(2 * time.Second)
	for f.get("2026-09-11") == 0 {
		select {
		case <-deadline:
			t.Fatal("the ticker never flushed")
		case <-time.After(time.Millisecond):
		}
	}
	c.Add(22)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return when its context ended")
	}
	if got := f.get("2026-09-11"); got != 33 {
		t.Errorf("after the shutdown flush the row holds %d, want 33", got)
	}
}
