package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestWriteJSONAndError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, 201, map[string]any{"a": 1})
	if rec.Code != 201 || rec.Header().Get("Content-Type") != "application/json; charset=utf-8" ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Body.String() != "{\"a\":1}\n" {
		t.Fatalf("WriteJSON: %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
	rec = httptest.NewRecorder()
	WriteError(rec, 404, "not_found", "no such clip")
	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 404 || body.Error != "not_found" || body.Message != "no such clip" {
		t.Fatalf("WriteError: %d %+v", rec.Code, body)
	}
	// Unencodable value → clean 500 envelope.
	rec = httptest.NewRecorder()
	WriteJSON(rec, 200, map[string]any{"f": func() {}})
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), `"error":"internal"`) {
		t.Fatalf("unencodable: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRequestID(t *testing.T) {
	var seen string
	h := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}))
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Request-ID", "client-supplied")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	got := rec.Header().Get("X-Request-ID")
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(got) || got != seen || got == "client-supplied" {
		t.Fatalf("request id header %q ctx %q", got, seen)
	}
	if RequestIDFrom(httptest.NewRequest("GET", "/", nil).Context()) != "" {
		t.Fatal("RequestIDFrom without middleware should be empty")
	}
}

func jsonLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func TestAccessLogFields(t *testing.T) {
	log, buf := jsonLogger()
	h := RequestID()(AccessLog(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetLogField(r, "storage_read_error", "boom")
		SetLogField(r, "n", 1)
		SetLogField(r, "n", 2) // overwrite, not duplicate
		w.WriteHeader(418)
		_, _ = w.Write([]byte("hello"))
	})))
	req := httptest.NewRequest("GET", "/api/clips/x/video.mp4", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log line %q: %v", buf.String(), err)
	}
	checks := map[string]any{"method": "GET", "path": "/api/clips/x/video.mp4", "status": float64(418), "bytes": float64(5),
		"storage_read_error": "boom", "n": float64(2), "level": "INFO", "msg": "http"}
	for k, want := range checks {
		if line[k] != want {
			t.Errorf("%s = %v, want %v (line %s)", k, line[k], want, buf.String())
		}
	}
	if _, ok := line["request_id"]; !ok {
		t.Errorf("request_id missing: %s", buf.String())
	}
	if _, ok := line["dur"]; !ok {
		t.Errorf("dur missing: %s", buf.String())
	}
	if strings.Count(buf.String(), `"n":`) != 1 {
		t.Errorf("field duplicated: %s", buf.String())
	}
}

func TestAccessLogLevels(t *testing.T) {
	log, buf := jsonLogger()
	status := 200
	h := AccessLog(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	level := func(path string, code int) string {
		buf.Reset()
		status = code
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
		var line map[string]any
		_ = json.Unmarshal(buf.Bytes(), &line)
		l, _ := line["level"].(string)
		return l
	}
	if got := level("/healthz", 200); got != "DEBUG" {
		t.Errorf("healthz 200 → %s, want DEBUG", got)
	}
	if got := level("/healthz", 500); got != "WARN" {
		t.Errorf("healthz 500 → %s, want WARN", got)
	}
	if got := level("/api/x", 503); got != "WARN" {
		t.Errorf("api 503 → %s, want WARN", got)
	}
	if got := level("/api/x", 404); got != "INFO" {
		t.Errorf("api 404 → %s, want INFO", got)
	}
	// Implicit 200 via Write without WriteHeader.
	buf.Reset()
	AccessLog(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("x")) })).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(buf.String(), `"status":200`) {
		t.Errorf("implicit status: %s", buf.String())
	}
}

func TestSetLogFieldNoopWithoutAccessLog(t *testing.T) {
	// Must not panic.
	SetLogField(httptest.NewRequest("GET", "/", nil), "k", "v")
}

func TestRecover(t *testing.T) {
	log, buf := jsonLogger()
	h := AccessLog(log)(Recover(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("kaboom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/boom", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), `"error":"internal"`) {
		t.Fatalf("recover: %d %s", rec.Code, rec.Body.String())
	}
	out := buf.String()
	if !strings.Contains(out, `"level":"ERROR"`) || !strings.Contains(out, "kaboom") || !strings.Contains(out, `"panic":"kaboom"`) {
		t.Fatalf("log: %s", out)
	}
	// Panic after headers were sent: no second WriteHeader, status preserved.
	rec = httptest.NewRecorder()
	AccessLog(log)(Recover(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
		panic(errors.New("late"))
	}))).ServeHTTP(rec, httptest.NewRequest("GET", "/late", nil))
	if rec.Code != 204 {
		t.Fatalf("late panic changed status to %d", rec.Code)
	}
	// ErrAbortHandler propagates.
	defer func() {
		if p := recover(); p != http.ErrAbortHandler {
			t.Fatalf("ErrAbortHandler not re-panicked: %v", p)
		}
	}()
	Recover(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) })).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}

func TestSecurityHeaders(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	rec := httptest.NewRecorder()
	SecurityHeaders(false, false, "")(ok).ServeHTTP(rec, httptest.NewRequest("GET", "/clips/x", nil))
	h := rec.Header()
	if h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("Referrer-Policy") != "strict-origin-when-cross-origin" ||
		h.Get("Content-Security-Policy") != "default-src 'self'; img-src 'self' data:; media-src 'self'; script-src 'self'; style-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'" {
		t.Fatalf("page headers: %v", h)
	}
	rec = httptest.NewRecorder()
	rec.Header().Set("X-Frame-Options", "DENY") // set by an outer layer; embed must drop it
	SecurityHeaders(true, false, "")(ok).ServeHTTP(rec, httptest.NewRequest("GET", "/embed/x", nil))
	h = rec.Header()
	if _, has := h["X-Frame-Options"]; has {
		t.Fatalf("embed has XFO: %v", h)
	}
	if h.Get("Content-Security-Policy") != "default-src 'self'; img-src 'self' data:; media-src 'self'; script-src 'self'; style-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors *" ||
		h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("embed headers: %v", h)
	}
	for _, csp := range []string{CSPPage(""), CSPEmbed("")} {
		if strings.Contains(csp, "unsafe-inline") {
			t.Fatalf("CSP allows inline: %s", csp)
		}
		// The three directives default-src does not cover. base-uri and
		// form-action have NO fallback in CSP3, so omitting them left an
		// injected <base> free to repoint every relative URL on the page and a
		// rewritten form action free to post the login off-origin.
		for _, want := range []string{"object-src 'none'", "base-uri 'none'", "form-action 'self'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("CSP lacks %q: %s", want, csp)
			}
		}
	}
}

// TestCSPNamesTheMediaOriginOnlyInRedirectMode (§20 erratum 278): CSP
// re-checks a redirect TARGET by host, so a media route that 302s to the
// bucket is blocked on our own watch and embed pages — silently, with nothing
// in the response to explain it — unless the bucket's origin is named in
// media-src and img-src. In `proxy` mode (the empty origin) the two directives
// must be byte-for-byte what they always were, because that is every
// deployment that existed before this.
func TestCSPNamesTheMediaOriginOnlyInRedirectMode(t *testing.T) {
	const origin = "https" + "://bucket.example.test"
	for _, tc := range []struct {
		name string
		csp  func(string) string
	}{{"page", CSPPage}, {"embed", CSPEmbed}} {
		proxy := tc.csp("")
		if strings.Contains(proxy, "bucket.example.test") {
			t.Errorf("%s: proxy mode names a media origin: %s", tc.name, proxy)
		}
		if !strings.Contains(proxy, "img-src 'self' data:;") || !strings.Contains(proxy, "media-src 'self';") {
			t.Errorf("%s: proxy mode changed the shipped directives: %s", tc.name, proxy)
		}
		redirect := tc.csp(origin)
		if n := strings.Count(redirect, origin); n != 2 {
			t.Errorf("%s: the origin appears %d times, want exactly 2 (img-src and media-src): %s", tc.name, n, redirect)
		}
		if !strings.Contains(redirect, "img-src 'self' data: "+origin+";") ||
			!strings.Contains(redirect, "media-src 'self' "+origin+";") {
			t.Errorf("%s: the origin did not land in both directives: %s", tc.name, redirect)
		}
		// Widening the MEDIA is not widening anything else.
		if !strings.Contains(redirect, "script-src 'self';") || strings.Contains(redirect, "unsafe-inline") {
			t.Errorf("%s: redirect mode widened more than the media: %s", tc.name, redirect)
		}
	}
	// …and the header the middleware actually writes carries it.
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	rec := httptest.NewRecorder()
	SecurityHeaders(false, false, origin)(ok).ServeHTTP(rec, httptest.NewRequest("GET", "/clips/x", nil))
	if got := rec.Header().Get("Content-Security-Policy"); got != CSPPage(origin) {
		t.Errorf("middleware header: %q", got)
	}
}

// TestHSTSFollowsTheOrigin: the header rides on the PUBLIC scheme — the same
// fact that decides the Secure cookie flag — not on the request, because TLS
// is terminated upstream and the listener never sees a certificate. A local
// http dev stack must send none: pinning https on localhost would lock a
// developer out of their own machine for a year.
func TestHSTSFollowsTheOrigin(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	for _, embed := range []bool{false, true} {
		rec := httptest.NewRecorder()
		SecurityHeaders(embed, true, "")(ok).ServeHTTP(rec, httptest.NewRequest("GET", "/clips/x", nil))
		if got := rec.Header().Get("Strict-Transport-Security"); got != "max-age=31536000; includeSubDomains" {
			t.Errorf("https embed=%v: HSTS %q", embed, got)
		}
		rec = httptest.NewRecorder()
		SecurityHeaders(embed, false, "")(ok).ServeHTTP(rec, httptest.NewRequest("GET", "/clips/x", nil))
		if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
			t.Errorf("http embed=%v: HSTS %q, want absent", embed, got)
		}
	}
	// No preload: that is a hostname-wide, hard-to-undo commitment belonging
	// to whoever owns the domain, not to an app deployed under it.
	if strings.Contains(HSTS, "preload") {
		t.Errorf("HSTS asks for preload: %q", HSTS)
	}
}

func TestStorageHealth(t *testing.T) {
	var h StorageHealth
	if h.Get() != "pending" || h.OK() || !h.LastChecked().IsZero() {
		t.Fatalf("initial: %q ok=%v", h.Get(), h.OK())
	}
	h.Set(errors.New("dial tcp: refused"))
	if h.Get() != "error: unavailable" || h.OK() {
		t.Fatalf("after error: %q", h.Get())
	}
	h.Set(nil)
	if h.Get() != "ok" || !h.OK() || h.LastChecked().IsZero() {
		t.Fatalf("after ok: %q", h.Get())
	}
	h.Set(errors.New("again"))
	if h.Get() != "error: unavailable" {
		t.Fatalf("flap back: %q", h.Get())
	}
}

// The public probes print the failure's class, never its message (§20
// erratum 31): a real ping failure is a *url.Error carrying S3_ENDPOINT
// (for R2 the account id) and the bucket, and /healthz is anonymous,
// cached and unmetered.
func TestStorageHealthNeverEchoesTheError(t *testing.T) {
	raw := `store: ping: Get "https://acc0unt.r2.cloudflarestorage.com/myclips/?location=": dial tcp 1.2.3.4:443: connect: connection refused`
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"unclassified", errors.New(raw), "error: unavailable"},
		{"classified", &classifiedErr{class: "unreachable", msg: raw}, "error: unreachable"},
		{"wrapped", fmt.Errorf("boot ping: %w", &classifiedErr{class: "bucket missing", msg: raw}), "error: bucket missing"},
		{"empty class", &classifiedErr{msg: raw}, "error: unavailable"},
	} {
		var h StorageHealth
		h.Set(c.err)
		got := h.Get()
		if got != c.want {
			t.Errorf("%s: Get() = %q, want %q", c.name, got, c.want)
		}
		for _, leak := range []string{"acc0unt", "r2.cloudflarestorage.com", "myclips", "1.2.3.4"} {
			if strings.Contains(got, leak) {
				t.Errorf("%s: Get() = %q leaks %q", c.name, got, leak)
			}
		}
	}
}

// classifiedErr stands in for store.PingError (httpx must not import store).
type classifiedErr struct {
	class, msg string
}

func (e *classifiedErr) Error() string       { return e.msg }
func (e *classifiedErr) HealthClass() string { return e.class }

func TestRecorderFlushAndUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	r := &recorder{ResponseWriter: rec, status: 200}
	r.Flush()
	if !r.Written() || rec.Code != 200 || !rec.Flushed {
		t.Fatalf("flush: written=%v code=%d flushed=%v", r.Written(), rec.Code, rec.Flushed)
	}
	if r.Unwrap() != rec {
		t.Fatal("Unwrap")
	}
	r.WriteHeader(500) // ignored after first write
	if r.status != 200 {
		t.Fatal("second WriteHeader changed status")
	}
}
