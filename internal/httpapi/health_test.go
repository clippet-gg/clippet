package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/store"
)

func testDB(t *testing.T) *db.DB {
	t.Helper()
	return openTestDB(t, filepath.Join(t.TempDir(), "clippet.db"))
}

func get(h http.Handler, path string) (int, map[string]any) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestHealthAndReady(t *testing.T) {
	d := testDB(t)
	fake := store.NewFake()
	health := &httpx.StorageHealth{}
	cfg := config.Config{Transcode: config.TranscodeConfig{FFmpegVersion: "7.1.1"}}
	h := NewHandler(Deps{Cfg: cfg, DB: d, Store: fake, Version: "test", Started: time.Now().Add(-3 * time.Second), Storage: health})

	// healthz before any ping: storage pending, never a live call.
	code, body := get(h, "/healthz")
	if code != 200 || body["ok"] != true || body["storage"] != "pending" || body["db"] != "ok" ||
		body["version"] != "test" || body["ffmpeg"] != "7.1.1" {
		t.Fatalf("healthz: %d %v", code, body)
	}
	if q := body["queue"].(map[string]any); q["queued"] != float64(0) || q["running"] != float64(0) {
		t.Fatalf("queue: %v", q)
	}
	if up, _ := body["uptime_s"].(float64); up < 2 {
		t.Fatalf("uptime_s = %v", up)
	}
	if len(fake.CallsSnapshot()) != 0 {
		t.Fatalf("healthz touched storage: %v", fake.Calls)
	}

	// readyz with a failing Ping → 503 and the cache flips to error. The
	// public probes print the CLASS, never the message (§20 erratum 31).
	fake.FailNext("Ping", errors.New(`store: ping: Get "https://acc0unt.r2.cloudflarestorage.com/myclips/?location=": dial tcp: refused`))
	code, body = get(h, "/readyz")
	if code != 503 || body["ok"] != false || body["db"] != "ok" || body["storage"] != "error: unavailable" {
		t.Fatalf("readyz failing: %d %v", code, body)
	}
	code, body = get(h, "/healthz")
	if code != 200 || body["storage"] != "error: unavailable" {
		t.Fatalf("healthz cached error: %d %v", code, body)
	}

	// readyz succeeding → 200 {"ok":true} and the cache is ok.
	code, body = get(h, "/readyz")
	if code != 200 || body["ok"] != true || len(body) != 1 {
		t.Fatalf("readyz ok: %d %v", code, body)
	}
	if _, b := get(h, "/healthz"); b["storage"] != "ok" {
		t.Fatalf("healthz after ok ping: %v", b)
	}
	if calls := fake.CallsSnapshot(); len(calls) != 2 || calls[0] != "Ping " {
		t.Fatalf("readyz should ping exactly once per call: %v", calls)
	}

	// Unknown API route → 404 envelope; method mismatch → 405.
	code, body = get(h, "/api/nope")
	if code != 404 || body["error"] != "not_found" {
		t.Fatalf("unknown route: %d %v", code, body)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/healthz", nil))
	var envelope map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &envelope)
	if rec.Code != 405 || envelope["error"] != "method_not_allowed" {
		t.Fatalf("POST /healthz = %d %s", rec.Code, rec.Body.String())
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Fatalf("405 Allow = %q, want \"GET, HEAD\"", allow)
	}
	// HEAD is implied by GET.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("HEAD", "/readyz", nil))
	if rec.Code != 200 {
		t.Fatalf("HEAD /readyz = %d", rec.Code)
	}
}

func TestReadyzPendingWithoutStore(t *testing.T) {
	h := NewHandler(Deps{DB: testDB(t)})
	code, body := get(h, "/readyz")
	if code != 503 || body["storage"] != "pending" || body["db"] != "ok" {
		t.Fatalf("readyz no store: %d %v", code, body)
	}
}

// dbClasses is the closed set §20 erratum 99 allows in the `db` field of an
// ANONYMOUS probe. The field is always exactly "error: " + one of these.
var dbClasses = map[string]bool{
	"timeout": true, "locked": true, "readonly": true,
	"io": true, "full": true, "corrupt": true, "unavailable": true,
}

// wantDBClass asserts the wire format erratum 99 fixed: the field names a
// class and nothing else. The driver's message for a closed pool is
// "sql: database is closed", and modernc's for a bad statement names both
// DATA_DIR's path and the failing SQL -- so the substrings below are the
// leak, not decoration.
func wantDBClass(t *testing.T, where string, body map[string]any) {
	t.Helper()
	s, ok := body["db"].(string)
	if !ok {
		t.Fatalf("%s: db field is %T (%v), want a string", where, body["db"], body["db"])
	}
	class, found := strings.CutPrefix(s, "error: ")
	if !found || !dbClasses[class] {
		t.Fatalf("%s: db = %q, want \"error: \" + one of the closed set", where, s)
	}
	for _, leak := range []string{"sql:", "sqlite", "SQL", "/", "\\", "SELECT", "PRAGMA", "clippet.db", "no such"} {
		if strings.Contains(s, leak) {
			t.Fatalf("%s: db = %q leaks %q", where, s, leak)
		}
	}
}

func TestHealthzDBDown(t *testing.T) {
	d := testDB(t)
	_ = d.Close()
	h := NewHandler(Deps{DB: d, Store: store.NewFake()})
	code, body := get(h, "/healthz")
	if code != 500 || body["ok"] != false || body["db"] == "ok" {
		t.Fatalf("healthz closed db: %d %v", code, body)
	}
	wantDBClass(t, "healthz closed db", body)
	code, body = get(h, "/readyz")
	if code != 503 || body["ok"] != false {
		t.Fatalf("readyz closed db: %d %v", code, body)
	}
	wantDBClass(t, "readyz closed db", body)
	h = NewHandler(Deps{})
	code, body = get(h, "/healthz")
	if code != 500 {
		t.Fatalf("healthz nil db: %d", code)
	}
	wantDBClass(t, "healthz nil db", body)
}

func TestReadyzSlowStoreIsBounded(t *testing.T) {
	fake := store.NewFake()
	fake.Hook = func(ctx context.Context, op, key string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	h := NewHandler(Deps{DB: testDB(t), Store: fake})
	start := time.Now()
	code, body := get(h, "/readyz")
	if code != 503 || body["ok"] != false {
		t.Fatalf("slow readyz: %d %v", code, body)
	}
	if time.Since(start) > healthTimeout+2*time.Second {
		t.Fatalf("readyz not bounded: %s", time.Since(start))
	}
}
