package transcode

import (
	"sync"
	"testing"
	"time"
)

// fakeClock is the stall watchdog's clock for a test (§20 erratum 331): time
// moves only on Advance, and a timer fires only when an Advance reaches it, so
// "silence longer than the stall timeout" is an Advance and never a sleep.
//
// Its timers follow the Go 1.23 contract the watchdog is written against: Stop
// and Reset discard a value that fired and was not received.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	// resets and stops count the watchdog's Reset and Stop calls: a block it
	// observed shows up as one of the two, which is how a test knows the block
	// was read without sleeping for it.
	resets, stops int
	// moved is closed and replaced on every timer operation and every fire.
	moved chan struct{}
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), moved: make(chan struct{})}
}

type fakeTimer struct {
	c      *fakeClock
	ch     chan time.Time
	at     time.Time
	active bool
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, ch: make(chan time.Time, 1), at: c.now.Add(d), active: true}
	c.timers = append(c.timers, t)
	c.bumpLocked()
	return t
}

// bumpLocked wakes every await; c.mu is held.
func (c *fakeClock) bumpLocked() {
	close(c.moved)
	c.moved = make(chan struct{})
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) drainLocked() {
	select {
	case <-t.ch:
	default:
	}
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.active = false
	t.drainLocked()
	t.c.stops++
	t.c.bumpLocked()
	return was
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.drainLocked()
	t.at, t.active = t.c.now.Add(d), true
	t.c.resets++
	t.c.bumpLocked()
	return was
}

// Advance moves the clock and fires every armed timer it reaches; it returns
// how many fired.
func (c *fakeClock) Advance(d time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	n := 0
	for _, t := range c.timers {
		if t.active && !t.at.After(c.now) {
			t.active = false
			select {
			case t.ch <- c.now:
			default:
			}
			n++
		}
	}
	c.bumpLocked()
	return n
}

// fakeState is one reading of the clock's bookkeeping.
type fakeState struct {
	timers, armed, resets, stops int
}

func (c *fakeClock) state() fakeState {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := fakeState{timers: len(c.timers), resets: c.resets, stops: c.stops}
	for _, t := range c.timers {
		if t.active {
			s.armed++
		}
	}
	return s
}

// fakeBound is the wall-time bound of every wait a fake-clock test makes (a
// hang guard, never a budget: nothing it waits for needs wall time beyond a
// process spawn). A watchdog that reads the wall clock instead of the injected
// one never arms, resets or fires on the fake clock, so the wait would
// otherwise hang to the package timeout; it fails here in 10 s with
// ignoredClock instead.
const fakeBound = 10 * time.Second

// ignoredClock is the sentence a fake-clock test fails with when its bound runs out.
const ignoredClock = "the watchdog ignored the injected clock"

// await blocks until cond holds over the clock's state, re-reading it at every
// timer operation, for at most fakeBound of wall time.
func (c *fakeClock) await(t *testing.T, what string, cond func(fakeState) bool) {
	t.Helper()
	guard := time.NewTimer(fakeBound)
	defer guard.Stop()
	for {
		c.mu.Lock()
		next := c.moved
		c.mu.Unlock()
		if cond(c.state()) {
			return
		}
		select {
		case <-next:
		case <-guard.C:
			t.Fatalf("%s: never true within %v of wall time — %s (clock state %+v)", what, fakeBound, ignoredClock, c.state())
		}
	}
}
