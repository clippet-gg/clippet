package worker

import (
	"sync"
	"time"

	"clippet/internal/db"
)

// Registry policy (§10.1): zero DB writes per frame; at most 4 registry
// writes per second per clip for progress lines; terminal entries
// (ready/failed) kept 10 minutes; at most 1000 entries (oldest evicted).
const (
	registryMinInterval  = 250 * time.Millisecond
	registryTerminalKeep = 10 * time.Minute
	registryCap          = 1000
)

// registry is the in-memory progress map behind Worker.Progress.
type registry struct {
	now func() time.Time
	mu  sync.Mutex
	m   map[string]Progress
	// waiting holds the channels await handed out, per clip, closed the
	// moment that clip's entry turns terminal (§20 erratum 97).
	waiting map[string]map[chan struct{}]struct{}
}

func newRegistry(now func() time.Time) *registry {
	return &registry{now: now, m: map[string]Progress{}, waiting: map[string]map[chan struct{}]struct{}{}}
}

// await returns a channel closed when clipID's entry reaches a terminal state
// (ready or failed), and the function that deregisters it — which the caller
// MUST call, or a waiter for a clip that never finishes is leaked.
//
// An entry that is ALREADY terminal comes back as a closed channel, which is
// what closes the one race a caller cannot close itself: POST /api/upload
// enqueues the job, wakes the worker and only then asks to be told, and on a
// short clip the whole transcode can land in between (§20 erratum 97).
//
// The signal is deliberately "the registry turned terminal", not "the row
// says ready": a run writes its terminal registry entry AFTER the commit that
// published it, so a caller woken here and re-reading the database cannot see
// the state the wait was meant to skip past.
func (r *registry) await(clipID string) (<-chan struct{}, func()) {
	ch := make(chan struct{})
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.m[clipID]; ok && terminal(p) {
		close(ch)
		return ch, func() {}
	}
	w := r.waiting[clipID]
	if w == nil {
		w = map[chan struct{}]struct{}{}
		r.waiting[clipID] = w
	}
	w[ch] = struct{}{}
	return ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if w := r.waiting[clipID]; w != nil {
			delete(w, ch)
			if len(w) == 0 {
				delete(r.waiting, clipID)
			}
		}
	}
}

// wakeLocked closes and drops every waiter for clipID. Called under the lock,
// from the write that made the entry terminal.
func (r *registry) wakeLocked(clipID string) {
	for ch := range r.waiting[clipID] {
		close(ch)
	}
	delete(r.waiting, clipID)
}

// set stores p for clipID. Non-forced writes (progress lines) are dropped
// when the previous write is younger than registryMinInterval; forced
// writes (stage changes, terminal states) always land. UpdatedAt is stamped
// here. Expired terminal entries are purged and the cap enforced on every
// landing write.
func (r *registry) set(clipID string, p Progress, force bool) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.m[clipID]; ok && !force && now.Sub(old.UpdatedAt) < registryMinInterval {
		return
	}
	p.UpdatedAt = now
	r.m[clipID] = p
	if terminal(p) {
		r.wakeLocked(clipID)
	}
	r.purge(now)
}

// get returns the entry, evicting it first if it is an expired terminal.
func (r *registry) get(clipID string) (Progress, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.m[clipID]
	if !ok {
		return Progress{}, false
	}
	if terminal(p) && r.now().Sub(p.UpdatedAt) >= registryTerminalKeep {
		delete(r.m, clipID)
		return Progress{}, false
	}
	return p, true
}

func (r *registry) forget(clipID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, clipID)
}

func (r *registry) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.m)
}

// purge drops expired terminal entries, then the oldest entries beyond the
// cap. Called under the lock.
func (r *registry) purge(now time.Time) {
	for id, p := range r.m {
		if terminal(p) && now.Sub(p.UpdatedAt) >= registryTerminalKeep {
			delete(r.m, id)
		}
	}
	for len(r.m) > registryCap {
		oldestID, oldest := "", time.Time{}
		first := true
		for id, p := range r.m {
			if first || p.UpdatedAt.Before(oldest) {
				oldestID, oldest, first = id, p.UpdatedAt, false
			}
		}
		delete(r.m, oldestID)
	}
}

// StagePercent is the floor of a stage's progress band (§10.3). The API uses
// it for the DB fallback of GET …/trim, where the persisted `stage` is all a
// restarted process has left of a job's progress.
func StagePercent(stage string) int { return bands[stage][0] }

func terminal(p Progress) bool {
	return p.State == db.ClipReady || p.State == db.ClipFailed
}
