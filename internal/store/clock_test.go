package store

import (
	"sync"
	"time"
)

// fakeClock is the idle watchdog's clock for a test (minio.go's clock seam):
// time moves only on Advance, and an AfterFunc runs, on the caller's
// goroutine, only when an Advance reaches it — so a touch cadence is exact
// and no scheduler stall can move it.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)}
}

type fakeTimer struct {
	c      *fakeClock
	at     time.Time
	f      func()
	active bool
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) clockTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, at: c.now.Add(d), f: f, active: true}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.active = false
	return was
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.at, t.active = t.c.now.Add(d), true
	return was
}

// Advance moves the clock, runs every armed func it reaches (outside the
// clock's lock: the watchdog's expire takes its own), and returns how many ran.
func (c *fakeClock) Advance(d time.Duration) int {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []func()
	for _, t := range c.timers {
		if t.active && !t.at.After(c.now) {
			t.active = false
			due = append(due, t.f)
		}
	}
	c.mu.Unlock()
	for _, f := range due {
		f()
	}
	return len(due)
}
