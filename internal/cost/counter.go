package cost

// counter.go — the served-bytes counter (ROADMAP #39, §20 erratum 270).
//
// Every byte this app hands a viewer is written by `http.ServeContent` through
// one wrapper in internal/httpapi, which adds the size of the finished response
// here. This file holds that number in memory and writes it to one row per UTC
// day on a window.
//
// Three properties are the whole design:
//
//   - **The media path never touches the database.** Add takes a mutex, adds to
//     a map with at most two entries in it, and returns. A ranged video stream
//     must not grow a write.
//   - **A crash loses at most one window.** The flush is every FlushEvery, and
//     sooner if FlushBytes have accumulated, and again at shutdown. What is lost
//     is bytes that were already served — the counter is an accounting estimate,
//     never a ledger, and the erratum says so in those words.
//   - **The unflushed window is still COUNTED.** Totals adds what is in memory
//     to what is on disk, so /healthz and the settings page are exact to the
//     byte at every instant and never lag a window behind the clip somebody just
//     watched. The flush is about durability, not about the number.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// The flush window (§20 erratum 270). Thirty seconds, or 64 MiB of serving,
// whichever comes first.
//
// The pair is chosen from what is LOST when a process dies between flushes: at
// a typical egress price of 0.01 per GB, 64 MiB is 0.00064 of a currency unit —
// four orders of magnitude below the hundredth any surface prints — and thirty
// seconds of an idle instance is nothing at all, because a window with no bytes
// in it costs no statement. The byte trigger is what keeps the loss bounded
// while a big file streams: a 4 GiB download on a fast link can be over inside
// one interval, and it flushes sixty-four times instead of once.
const (
	FlushEvery = 30 * time.Second
	FlushBytes = 64 << 20
)

// flushBudget bounds one flush's write. It is DETACHED from the loop's context
// at shutdown for the reason the sweeper detaches its backoff write: the moment
// the process is going away is exactly when the last window matters.
const flushBudget = 5 * time.Second

// Store is what the counter needs of the database (internal/db implements it).
// An interface so this package's tests need no SQLite file and so nothing here
// can reach a statement it was not given.
type Store interface {
	AddBytesServed(ctx context.Context, day string, n int64) error
	BytesServed(ctx context.Context, day, monthStart, monthEnd string) (today, month int64, err error)
}

// Counter accumulates served bytes and flushes them per UTC day.
//
// A nil *Counter is usable and does nothing: Add on it is a no-op, so a build
// that wires none (a test handler) serves media exactly as it always did.
type Counter struct {
	db  Store
	log *slog.Logger

	// now, every and atBytes are fields rather than constants so a test can
	// drive the day boundary and the triggers without a real clock or 64 MiB.
	now     func() time.Time
	every   time.Duration
	atBytes int64

	// mu guards pending and total. pending is keyed by UTC day and holds at
	// most two entries in practice — today's, and yesterday's for the seconds
	// between a day boundary and the next flush. That is exactly why the map is
	// keyed at all: a counter holding one running total would post the bytes a
	// viewer received at 23:59:59Z to the following day.
	mu      sync.Mutex
	pending map[string]int64
	total   int64

	// wake is the byte trigger, buffered and never blocking: Add must return at
	// once whatever the flush loop is doing.
	wake chan struct{}
}

// New builds a counter over d. Start its loop with Run.
func New(d Store, log *slog.Logger) *Counter {
	if log == nil {
		log = slog.Default()
	}
	return &Counter{db: d, log: log, now: time.Now, every: FlushEvery, atBytes: FlushBytes,
		pending: map[string]int64{}, wake: make(chan struct{}, 1)}
}

// Add counts n bytes served at this instant. It is called once per finished
// media response, from the handler's own goroutine, and never blocks.
func (c *Counter) Add(n int64) {
	if c == nil || n <= 0 {
		return
	}
	day := Day(c.now())
	c.mu.Lock()
	c.pending[day] += n
	c.total += n
	trigger := c.total >= c.atBytes
	c.mu.Unlock()
	if trigger {
		select {
		case c.wake <- struct{}{}:
		default: // a flush is already pending; one signal is enough
		}
	}
}

// Pending is the unflushed bytes whose day starts with prefix: a whole day
// (`2026-09-11`) or a whole month (`2026-09`). Zero for a nil counter.
func (c *Counter) Pending(prefix string) int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var n int64
	for day, b := range c.pending {
		if strings.HasPrefix(day, prefix) {
			n += b
		}
	}
	return n
}

// Totals is what the surfaces read: bytes served today and this calendar month,
// UTC, INCLUDING the window that has not been flushed yet.
//
// A nil counter answers (0, 0) with no error — there is nothing counting, and
// the caller renders no served figure at all rather than a zero (the
// "absent is not zero" rule this whole feature is built on).
func (c *Counter) Totals(ctx context.Context, now time.Time) (today, month int64, err error) {
	if c == nil {
		return 0, 0, nil
	}
	start, end := MonthRange(now)
	day := Day(now)
	today, month, err = c.db.BytesServed(ctx, day, start, end)
	if err != nil {
		return 0, 0, err
	}
	return today + c.Pending(day), month + c.Pending(Month(now)), nil
}

// Flush writes every unflushed day and empties the window. One UPSERT per day,
// which is one statement on all but the few seconds a day boundary spans.
//
// Bytes whose write FAILED are put back, so the next flush retries them: the
// counter's only failure mode is being late, never being short. A flush with
// nothing in it makes no call at all.
func (c *Counter) Flush(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if len(c.pending) == 0 {
		c.mu.Unlock()
		return nil
	}
	batch := c.pending
	c.pending = map[string]int64{}
	c.total = 0
	c.mu.Unlock()

	var firstErr error
	for day, n := range batch {
		if err := c.db.AddBytesServed(ctx, day, n); err != nil {
			c.mu.Lock()
			c.pending[day] += n
			c.total += n
			c.mu.Unlock()
			if firstErr == nil {
				firstErr = err
			}
			c.log.Warn("served-bytes counter: the day's total could not be written; the bytes are kept for the next flush",
				"day", day, "bytes", n, "err", err)
		}
	}
	return firstErr
}

// Run flushes on the window until ctx is done, then flushes once more on a
// DETACHED context and returns. serve.go flushes again after the HTTP drain,
// which is what covers the bytes served while the server was draining; both
// calls are the same idempotent operation over whatever is left in memory.
func (c *Counter) Run(ctx context.Context) {
	if c == nil {
		return
	}
	t := time.NewTicker(c.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushBudget)
			_ = c.Flush(fctx)
			cancel()
			return
		case <-t.C:
			_ = c.flushWithBudget(ctx)
		case <-c.wake:
			_ = c.flushWithBudget(ctx)
		}
	}
}

func (c *Counter) flushWithBudget(ctx context.Context) error {
	fctx, cancel := context.WithTimeout(ctx, flushBudget)
	defer cancel()
	return c.Flush(fctx)
}
