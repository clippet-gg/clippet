package ratelimit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func clock(start time.Time) (func() time.Time, func(time.Duration)) {
	now := start
	return func() time.Time { return now }, func(d time.Duration) { now = now.Add(d) }
}

func TestAllowDeniesAtBurst(t *testing.T) {
	l := New(1, 3, 100)
	now, advance := clock(time.Unix(1_700_000_000, 0))
	l.now = now
	for i := 0; i < 3; i++ {
		if ok, ra := l.Allow("k"); !ok || ra != 0 {
			t.Fatalf("request %d refused (retry %s)", i, ra)
		}
	}
	ok, ra := l.Allow("k")
	if ok || ra <= 0 || ra > time.Second {
		t.Fatalf("4th request: ok=%v retryAfter=%s", ok, ra)
	}
	// Other keys are independent.
	if ok, _ := l.Allow("other"); !ok {
		t.Fatal("independent key refused")
	}
	// After the advertised wait the key is allowed again, and only once.
	advance(ra)
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("refused after Retry-After elapsed")
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("token bucket refilled more than one token")
	}
}

func TestHardCapEvictsLRU(t *testing.T) {
	l := New(1, 1, 3)
	now, advance := clock(time.Unix(1_700_000_000, 0))
	l.now = now
	l.Allow("a")
	advance(time.Millisecond)
	l.Allow("b")
	advance(time.Millisecond)
	l.Allow("c")
	advance(time.Millisecond)
	l.Allow("a") // a is now the most recently seen; b is the LRU
	advance(time.Millisecond)
	l.Allow("d") // at cap: evicts b
	if l.Len() != 3 {
		t.Fatalf("Len = %d, want 3", l.Len())
	}
	if _, ok := l.entries["b"]; ok {
		t.Fatal("b (least recently seen) survived eviction")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := l.entries[k]; !ok {
			t.Fatalf("%s was evicted", k)
		}
	}
	// A fresh bucket after eviction: b is allowed again (burst 1) without
	// growing past the cap.
	advance(time.Millisecond)
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("re-inserted key refused")
	}
	if l.Len() != 3 {
		t.Fatalf("Len after reinsert = %d", l.Len())
	}
}

func TestIdleEviction(t *testing.T) {
	l := New(1, 1, 100)
	now, advance := clock(time.Unix(1_700_000_000, 0))
	l.now = now
	l.Allow("old")
	advance(5 * time.Minute)
	l.Allow("fresh")
	if l.Len() != 2 {
		t.Fatalf("Len = %d", l.Len())
	}
	// 11 minutes after "old", 6 after "fresh": only "old" is idle > 10m.
	advance(6 * time.Minute)
	l.Allow("trigger")
	if _, ok := l.entries["old"]; ok {
		t.Fatal("idle key not evicted")
	}
	if _, ok := l.entries["fresh"]; !ok {
		t.Fatal("live key evicted")
	}
	// The janitor runs at most once per minute.
	sweptAt := l.lastSweep
	advance(30 * time.Second)
	l.Allow("trigger")
	if !l.lastSweep.Equal(sweptAt) {
		t.Fatal("janitor ran again within a minute")
	}
	advance(31 * time.Second)
	l.Allow("trigger")
	if l.lastSweep.Equal(sweptAt) {
		t.Fatal("janitor did not run after a minute")
	}
}

func TestMiddleware(t *testing.T) {
	l := New(0.5, 1, 10)
	now, _ := clock(time.Unix(1_700_000_000, 0))
	l.now = now
	calls := 0
	h := Middleware(l, func(r *http.Request) string { return r.RemoteAddr })(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(http.StatusNoContent)
		}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "1.2.3.4:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 204 || calls != 1 {
		t.Fatalf("first call: %d calls=%d", rec.Code, calls)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 429 || calls != 1 {
		t.Fatalf("second call: %d calls=%d", rec.Code, calls)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "2" {
		t.Fatalf("Retry-After = %q, want 2 (ceil of 2s at 0.5/s)", ra)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] != "rate_limited" {
		t.Fatalf("body = %s (%v)", rec.Body.String(), err)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	// A different key is unaffected.
	req2 := httptest.NewRequest(http.MethodPost, "/", nil)
	req2.RemoteAddr = "5.6.7.8:1"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req2)
	if rec.Code != 204 {
		t.Fatalf("other key: %d", rec.Code)
	}
}

func TestNewClampsArguments(t *testing.T) {
	l := New(1, 0, 0)
	if l.burst != 1 || l.maxKeys != defaultMaxKeys {
		t.Fatalf("burst=%d maxKeys=%d", l.burst, l.maxKeys)
	}
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("burst 0 refused the very first request")
	}
}
