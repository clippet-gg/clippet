package transcode

import "time"

// Clock is the stall watchdog's time source (§20 erratum 331): the instant of
// the last advance and the timer that fires when the silence after it reaches
// the stall timeout. nil — every production caller — is the wall clock. A test
// supplies one it advances by hand, so "silence longer than the timeout" is an
// Advance and never a sleep against a real process (rule R2: no test asserts a
// wall-clock duration under 10 s against a real process).
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer is the part of *time.Timer the watchdog uses.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

// wallClock is the production Clock.
type wallClock struct{}

func (wallClock) Now() time.Time                 { return time.Now() }
func (wallClock) NewTimer(d time.Duration) Timer { return wallTimer{time.NewTimer(d)} }

type wallTimer struct{ t *time.Timer }

func (w wallTimer) C() <-chan time.Time        { return w.t.C }
func (w wallTimer) Stop() bool                 { return w.t.Stop() }
func (w wallTimer) Reset(d time.Duration) bool { return w.t.Reset(d) }

// clockOr is c, or the wall clock when c is nil.
func clockOr(c Clock) Clock {
	if c == nil {
		return wallClock{}
	}
	return c
}
