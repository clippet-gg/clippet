// Package ratelimit: bounded per-key token buckets and the 429 middleware
// (DESIGN.md §14). One Limiter holds one golang.org/x/time/rate bucket per
// key in an LRU list: keys idle for more than idleTTL are evicted by a
// janitor pass that runs at most once per janitorInterval, and a hard cap
// on distinct keys evicts the least-recently-seen key before every insert
// at the cap, so a scan can never grow memory.
package ratelimit

import (
	"container/list"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"clippet/internal/httpx"
)

const (
	janitorInterval = time.Minute
	idleTTL         = 10 * time.Minute
	defaultMaxKeys  = 10000
)

// Limiter holds one token bucket per key with a hard cap on keys (LRU
// eviction at the cap, idle eviction by a janitor).
type Limiter struct {
	mu        sync.Mutex
	perSecond float64
	burst     int
	maxKeys   int
	entries   map[string]*list.Element
	lru       *list.List // front = most recently seen
	lastSweep time.Time
	now       func() time.Time
}

type entry struct {
	key      string
	bucket   *rate.Limiter
	lastSeen time.Time
}

// New returns a limiter allowing perSecond sustained with burst, bounded to
// maxKeys distinct keys (≤ 0 → 10,000). A burst below 1 is raised to 1 so
// the first request of a key is never refused outright.
func New(perSecond float64, burst, maxKeys int) *Limiter {
	if burst < 1 {
		burst = 1
	}
	if maxKeys < 1 {
		maxKeys = defaultMaxKeys
	}
	return &Limiter{
		perSecond: perSecond,
		burst:     burst,
		maxKeys:   maxKeys,
		entries:   map[string]*list.Element{},
		lru:       list.New(),
		now:       time.Now,
	}
}

// Allow reports whether key may proceed now; when not, retryAfter is the
// wait to advertise in Retry-After (always > 0 on refusal).
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	e := l.touchLocked(key, now)
	res := e.bucket.ReserveN(now, 1)
	if !res.OK() {
		return false, time.Second
	}
	delay := res.DelayFrom(now)
	if delay <= 0 {
		return true, 0
	}
	res.CancelAt(now)
	return false, delay
}

// Len reports the number of tracked keys (tests and metrics).
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// touchLocked returns key's entry, creating it (evicting the LRU key at the
// cap) and moving it to the front.
func (l *Limiter) touchLocked(key string, now time.Time) *entry {
	if el, ok := l.entries[key]; ok {
		e := el.Value.(*entry)
		e.lastSeen = now
		l.lru.MoveToFront(el)
		return e
	}
	for len(l.entries) >= l.maxKeys {
		l.removeLocked(l.lru.Back())
	}
	e := &entry{key: key, bucket: rate.NewLimiter(rate.Limit(l.perSecond), l.burst), lastSeen: now}
	l.entries[key] = l.lru.PushFront(e)
	return e
}

// sweepLocked evicts keys idle longer than idleTTL, at most once per
// janitorInterval. The list is ordered by recency, so it stops at the first
// live entry from the back.
func (l *Limiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < janitorInterval {
		return
	}
	l.lastSweep = now
	for el := l.lru.Back(); el != nil; el = l.lru.Back() {
		if now.Sub(el.Value.(*entry).lastSeen) <= idleTTL {
			return
		}
		l.removeLocked(el)
	}
}

func (l *Limiter) removeLocked(el *list.Element) {
	if el == nil {
		return
	}
	delete(l.entries, el.Value.(*entry).key)
	l.lru.Remove(el)
}

// Middleware answers 429 rate_limited + Retry-After when keyFn(r) is over
// its budget.
func Middleware(l *Limiter, keyFn func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ok, retryAfter := l.Allow(keyFn(r))
			if !ok {
				secs := int64(math.Ceil(retryAfter.Seconds()))
				if secs < 1 {
					secs = 1
				}
				w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
				w.Header().Set("Cache-Control", "no-store")
				httpx.SetLogField(r, "rate_limited", true)
				httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many requests; retry later")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
