package httpapi

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// countingBody is a request body that records every Read. It never returns
// data: a handler that reads it at all is a handler that would have spooled a
// gigabyte before refusing the request.
type countingBody struct {
	reads atomic.Int32
}

func (b *countingBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	return 0, errors.New("the body must not be read")
}

func (b *countingBody) Close() error { return nil }

// refuseCases are every upload route and the credential each one refuses.
func refuseCases(e *env, clipID string) []struct {
	name, method, path string
	opts               []reqOpt
	code               int
} {
	return []struct {
		name, method, path string
		opts               []reqOpt
		code               int
	}{
		{"POST /api/clips anonymous", "POST", "/api/clips", nil, 401},
		{"POST /api/upload anonymous", "POST", "/api/upload", nil, 401},
		{"POST /api/upload with a revoked token", "POST", "/api/upload", []reqOpt{withBearer("ck_nope")}, 401},
		{"POST /api/share anonymous", "POST", "/api/share", nil, 401},
		{"POST …/replace anonymous", "POST", "/api/clips/" + clipID + "/replace", nil, 401},
	}
}

// TestRefusedUploadNeverReadsTheBody is ROADMAP #32 (§20 erratum 138): a
// request with no usable credential is answered before a single Read, and the
// answer closes the connection when the body is too big to drain.
func TestRefusedUploadNeverReadsTheBody(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})

	for _, tc := range refuseCases(e, c.ID) {
		t.Run(tc.name, func(t *testing.T) {
			body := &countingBody{}
			req := httptest.NewRequest(tc.method, tc.path, body)
			req.RemoteAddr = "192.0.2.10:5555"
			req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
			req.ContentLength = 1 << 30 // a declared gibibyte
			for _, o := range tc.opts {
				o(req)
			}
			rec := httptest.NewRecorder()
			e.h.ServeHTTP(rec, req)
			if rec.Code != tc.code {
				t.Fatalf("status = %d %s, want %d", rec.Code, rec.Body.String(), tc.code)
			}
			if n := body.reads.Load(); n != 0 {
				t.Fatalf("the body was read %d times before the %d", n, tc.code)
			}
			if got := rec.Header().Get("Connection"); got != "close" {
				t.Fatalf("Connection = %q, want close for a 1 GiB body nobody is going to read", got)
			}
		})
	}
}

// TestRefusedUploadWithNoDeclaredLengthCloses: a chunked body has no number to
// compare, which is the case with no upper bound at all, so it is treated as
// the large one.
func TestRefusedUploadWithNoDeclaredLengthCloses(t *testing.T) {
	e := newEnv(t)
	body := &countingBody{}
	req := httptest.NewRequest("POST", "/api/upload", body)
	req.RemoteAddr = "192.0.2.10:5555"
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 401 || rec.Header().Get("Connection") != "close" || body.reads.Load() != 0 {
		t.Fatalf("chunked refusal: %d Connection=%q reads=%d", rec.Code,
			rec.Header().Get("Connection"), body.reads.Load())
	}
}

// TestSmallRefusedUploadKeepsTheConnection: below the threshold net/http
// drains the remainder in one read, and a capture tool about to retry with a
// good credential keeps its connection.
func TestSmallRefusedUploadKeepsTheConnection(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequest("POST", "/api/upload", strings.NewReader("small"))
	req.RemoteAddr = "192.0.2.10:5555"
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Connection"); got != "" {
		t.Fatalf("Connection = %q, want unset for a body small enough to drain", got)
	}
}

// TestAcceptedUploadKeepsTheConnection: the mark is taken off again for a
// request that IS going to be read, however big it is — otherwise every real
// upload would close its connection.
func TestAcceptedUploadKeepsTheConnection(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	_ = owner
	body, ct := multipartBody(t,
		part{name: "metadata", contentType: "application/json", data: []byte(`{"title":"t"}`)},
		part{name: "video", filename: "v.mp4", contentType: "video/mp4", data: testVideo(2000)})
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/clips", strings.NewReader(string(raw)))
	req.RemoteAddr = "192.0.2.10:5555"
	req.Header.Set("Content-Type", ct)
	// Declared over the 256 KiB mark — so closeUnlessRead marked it — and under
	// MAX_UPLOAD_BYTES, so it is accepted: the gate is the declaration.
	req.ContentLength = 512 << 10
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Connection"); got != "" {
		t.Fatalf("Connection = %q on an accepted upload, want unset", got)
	}
}

// TestRefusedUploadOverTheLimiterAlsoCloses: the rule is about the ANSWER, not
// about the credential — a caller over the rate limit is refused with the body
// unread too.
func TestRefusedUploadOverTheLimiterAlsoCloses(t *testing.T) {
	e := newEnv(t, func(d *Deps) {
		d.Cfg.RateUploadPerHour = 1
		d.Cfg.RateUploadPerHourPerIP = 1000
	})
	_, cookie := e.newUser("owner")
	// Spend the one token.
	if r := e.upload(`{"title":"one"}`, "a.mp4", testVideo(500), withCookie(cookie)); r.code != 201 {
		t.Fatalf("first upload: %d %s", r.code, r.body)
	}
	body := &countingBody{}
	req := httptest.NewRequest("POST", "/api/clips", body)
	req.RemoteAddr = "192.0.2.10:5555"
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
	req.ContentLength = 1 << 30
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d %s, want 429", rec.Code, rec.Body.String())
	}
	if body.reads.Load() != 0 || rec.Header().Get("Connection") != "close" {
		t.Fatalf("429: reads=%d Connection=%q", body.reads.Load(), rec.Header().Get("Connection"))
	}
}
