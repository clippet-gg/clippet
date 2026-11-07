package httpapi

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"clippet/internal/chunks"
)

// The chunked web upload (§20 erratum 344): a file as a run of requests, one
// ingest. These pin the routes; internal/chunks pins the store itself.

const testChunk = 1000

func chunkedEnv(t *testing.T) *env {
	t.Helper()
	return newEnv(t, func(d *Deps) { d.Cfg.UploadChunkBytes = testChunk })
}

// openSession is POST /api/uploads with extra fields merged in.
func (e *env) openSession(size int, extra string, opts ...reqOpt) resp {
	e.t.Helper()
	body := fmt.Sprintf(`{"size":%d,"name":"clip.mp4","type":"video/mp4"%s}`, size, extra)
	return e.doJSON(http.MethodPost, "/api/uploads", body, opts...)
}

func (e *env) putChunkBytes(id string, index int, b []byte, opts ...reqOpt) resp {
	e.t.Helper()
	return e.do(http.MethodPut, "/api/uploads/"+id+"/"+strconv.Itoa(index), bytes.NewReader(b),
		append([]reqOpt{withHeader("Content-Type", "application/octet-stream")}, opts...)...)
}

func received(r resp) int {
	n, _ := r.json["received"].(float64)
	return int(n)
}

// TestChunkedUploadIsTheOneShotsIngest: open, three chunks (one repeated, one
// out of order), finish → 201 with the clip; the stored original is the file
// byte for byte, and a one-shot upload of the same file answers that clip with
// `existing` — the duplicate check of the one ingest, reached by the other door.
func TestChunkedUploadIsTheOneShotsIngest(t *testing.T) {
	e := chunkedEnv(t)
	_, cookie := e.newUser("carol")
	video := testVideo(2500)

	op := e.openSession(len(video), `,"title":"Chunked","tags":["big"],"ttl_days":7`, withCookie(cookie))
	if op.code != 201 || op.json["chunk_bytes"] != float64(testChunk) || received(op) != 0 {
		t.Fatalf("open: %d %s", op.code, op.body)
	}
	id := op.str("id")
	if !chunks.ValidID(id) {
		t.Fatalf("open's id %q", id)
	}
	if r := e.putChunkBytes(id, 1, video[1000:2000], withCookie(cookie)); r.code != 409 || r.errCode() != "out_of_order" || received(r) != 0 {
		t.Fatalf("chunk 1 first: %d %s", r.code, r.body)
	}
	if r := e.putChunkBytes(id, 0, video[:1000], withCookie(cookie)); r.code != 200 || received(r) != 1 {
		t.Fatalf("chunk 0: %d %s", r.code, r.body)
	}
	if r := e.putChunkBytes(id, 0, video[:1000], withCookie(cookie)); r.code != 200 || received(r) != 1 {
		t.Fatalf("chunk 0 again (idempotent): %d %s", r.code, r.body)
	}
	if r := e.do(http.MethodPost, "/api/uploads/"+id+"/finish", nil, withCookie(cookie)); r.code != 409 || r.errCode() != "incomplete" {
		t.Fatalf("finish early: %d %s", r.code, r.body)
	}
	if r := e.putChunkBytes(id, 1, video[1000:1999], withCookie(cookie)); r.code != 400 || r.errCode() != "invalid_request" {
		t.Fatalf("a chunk of the wrong length: %d %s", r.code, r.body)
	}
	for i := 1; i < 3; i++ {
		end := (i + 1) * testChunk
		if end > len(video) {
			end = len(video)
		}
		if r := e.putChunkBytes(id, i, video[i*testChunk:end], withCookie(cookie)); r.code != 200 || received(r) != i+1 {
			t.Fatalf("chunk %d: %d %s", i, r.code, r.body)
		}
	}
	if r := e.do(http.MethodGet, "/api/uploads/"+id, nil, withCookie(cookie)); r.code != 200 || received(r) != 3 || r.has("clip") {
		t.Fatalf("GET before finish: %d %s", r.code, r.body)
	}
	fin := e.do(http.MethodPost, "/api/uploads/"+id+"/finish", nil, withCookie(cookie))
	if fin.code != 201 || fin.str("title") != "Chunked" || fin.has("existing") {
		t.Fatalf("finish: %d %s", fin.code, fin.body)
	}
	clipID := fin.str("id")
	c := e.getClip(clipID)
	if got, ok := e.fake.Get(c.SourceKey); !ok || !bytes.Equal(got, video) {
		t.Fatalf("the stored original is not the file (%v, %d bytes)", ok, len(got))
	}
	if c.SourceETag != sha256hex(video) || c.SourceSize != int64(len(video)) || c.SourceFilename != "clip.mp4" {
		t.Fatalf("the row: etag %s size %d name %q", c.SourceETag, c.SourceSize, c.SourceFilename)
	}
	if e.worker.woken.Load() == 0 {
		t.Fatal("the worker was not woken")
	}
	// The finish answer again (a client that lost it), and GET carries the clip.
	if r := e.do(http.MethodPost, "/api/uploads/"+id+"/finish", nil, withCookie(cookie)); r.code != 201 || r.str("id") != clipID {
		t.Fatalf("finish again: %d %s", r.code, r.body)
	}
	if r := e.do(http.MethodGet, "/api/uploads/"+id, nil, withCookie(cookie)); r.code != 200 {
		t.Fatalf("GET after finish: %d %s", r.code, r.body)
	} else if clip, _ := r.json["clip"].(map[string]any); clip == nil || clip["id"] != clipID {
		t.Fatalf("GET after finish carries no clip: %s", r.body)
	}
	// The other door, the same bytes: the existing clip.
	one := e.uploadChecked(`{"title":"One-shot"}`, "clip.mp4", video, nil, withCookie(cookie))
	if one.code != 200 || one.str("id") != clipID || one.json["existing"] != true {
		t.Fatalf("the one-shot upload of the same file: %d %s", one.code, one.body)
	}
	// And the other way round: a chunked upload of bytes the one-shot already
	// stored is the existing clip too, 200 + existing.
	other := testVideo(1500)
	other[0] ^= 0xff
	first := e.uploadChecked(`{}`, "b.mp4", other, nil, withCookie(cookie))
	if first.code != 201 {
		t.Fatalf("the one-shot first: %d %s", first.code, first.body)
	}
	op2 := e.openSession(len(other), "", withCookie(cookie))
	id2 := op2.str("id")
	e.putChunkBytes(id2, 0, other[:1000], withCookie(cookie))
	e.putChunkBytes(id2, 1, other[1000:], withCookie(cookie))
	if r := e.do(http.MethodPost, "/api/uploads/"+id2+"/finish", nil, withCookie(cookie)); r.code != 200 || r.str("id") != first.str("id") || r.json["existing"] != true {
		t.Fatalf("the chunked duplicate: %d %s", r.code, r.body)
	}
	// duplicate=allow at the open is the opt-out, as on the one-shot.
	op3 := e.openSession(len(other), `,"duplicate":"allow"`, withCookie(cookie))
	id3 := op3.str("id")
	e.putChunkBytes(id3, 0, other[:1000], withCookie(cookie))
	e.putChunkBytes(id3, 1, other[1000:], withCookie(cookie))
	if r := e.do(http.MethodPost, "/api/uploads/"+id3+"/finish", nil, withCookie(cookie)); r.code != 201 || r.str("id") == first.str("id") {
		t.Fatalf("duplicate=allow: %d %s", r.code, r.body)
	}
}

// TestChunkedOpenGates: the open asks every gate that can be asked before the
// bytes — the size against MAX_UPLOAD_BYTES, the metadata document, the
// duplicate field, the scratch room, the quota — and the routes are the
// owner's alone.
func TestChunkedOpenGates(t *testing.T) {
	e := chunkedEnv(t)
	owner, cookie := e.newUser("carol")
	_, otherCookie := e.newUser("other")

	for _, c := range []struct {
		body string
		code int
		err  string
	}{
		{`{"name":"a.mp4"}`, 400, "invalid_request"},
		{`{"size":0}`, 400, "invalid_request"},
		{`{"size":-5}`, 400, "invalid_request"},
		{`{"size":"10"}`, 400, "invalid_request"},
		{fmt.Sprintf(`{"size":%d}`, e.cfg.MaxUploadBytes+1), 413, "too_large"},
		{`{"size":10,"duplicate":"maybe"}`, 400, "invalid_request"},
		{`{"size":10,"ttl_days":3}`, 400, "invalid_ttl"},
		{`{"size":10,"title":"` + strings.Repeat("x", 201) + `"}`, 400, "invalid_request"},
	} {
		if r := e.doJSON(http.MethodPost, "/api/uploads", c.body, withCookie(cookie)); r.code != c.code || r.errCode() != c.err {
			t.Errorf("open %s: %d %s, want %d %s", c.body, r.code, r.body, c.code, c.err)
		}
	}
	if r := e.do(http.MethodPost, "/api/uploads", strings.NewReader(`{"size":10}`), withCookie(cookie)); r.code != 415 {
		t.Errorf("open without application/json: %d", r.code)
	}
	if r := e.openSession(10, ""); r.code != 401 {
		t.Errorf("open anonymous: %d", r.code)
	}
	if r := e.openSession(int(e.cfg.MaxUploadBytes), "", withCookie(cookie)); r.code != 201 {
		t.Errorf("open at exactly MAX_UPLOAD_BYTES: %d %s", r.code, r.body)
	}
	// The scratch precheck for the declared size.
	e.free = func(string) (int64, error) { return 100, nil }
	if r := e.openSession(10, "", withCookie(cookie)); r.code != 507 || r.errCode() != "insufficient_scratch" {
		t.Errorf("open with no scratch room: %d %s", r.code, r.body)
	}
	e.free = func(string) (int64, error) { return 1 << 40, nil }

	// Another account cannot see, write, finish or abandon the session.
	op := e.openSession(10, "", withCookie(cookie))
	id := op.str("id")
	for _, r := range []resp{
		e.do(http.MethodGet, "/api/uploads/"+id, nil, withCookie(otherCookie)),
		e.putChunkBytes(id, 0, make([]byte, 10), withCookie(otherCookie)),
		e.do(http.MethodPost, "/api/uploads/"+id+"/finish", nil, withCookie(otherCookie)),
		e.do(http.MethodDelete, "/api/uploads/"+id, nil, withCookie(otherCookie)),
		e.do(http.MethodGet, "/api/uploads/0123456789abcdef0123456789abcdef", nil, withCookie(cookie)),
		e.do(http.MethodGet, "/api/uploads/not-an-id", nil, withCookie(cookie)),
	} {
		if r.code != 404 || r.errCode() != "not_found" {
			t.Errorf("a session not the caller's: %d %s", r.code, r.body)
		}
	}
	// A bad index, a missing length, a capture token with the upload scope.
	for _, ix := range []string{"-1", "01", "x", "1000000"} {
		if r := e.do(http.MethodPut, "/api/uploads/"+id+"/"+ix, bytes.NewReader(nil), withCookie(cookie)); r.code != 400 {
			t.Errorf("index %q: %d %s", ix, r.code, r.body)
		}
	}
	if r := e.do(http.MethodPut, "/api/uploads/"+id+"/0", chunkedReader{bytes.NewReader(make([]byte, 10))}, withCookie(cookie),
		func(r *http.Request) { r.ContentLength = -1 }); r.code != 411 || r.errCode() != "length_required" {
		t.Errorf("a chunk with no length: %d %s", r.code, r.body)
	}
	tok := e.newToken(owner, "upload")
	if r := e.putChunkBytes(id, 0, make([]byte, 10), withBearer(tok)); r.code != 200 || received(r) != 1 {
		t.Errorf("a chunk with an upload token: %d %s", r.code, r.body)
	}
	// Abandon: 204, then gone.
	if r := e.do(http.MethodDelete, "/api/uploads/"+id, nil, withCookie(cookie)); r.code != 204 {
		t.Errorf("DELETE: %d %s", r.code, r.body)
	}
	if r := e.do(http.MethodGet, "/api/uploads/"+id, nil, withCookie(cookie)); r.code != 404 {
		t.Errorf("GET after DELETE: %d", r.code)
	}
	if _, err := os.Stat(filepath.Join(e.scratch, chunks.DirName, id)); !os.IsNotExist(err) {
		t.Errorf("the abandoned session's directory: %v", err)
	}
}

// TestChunkedOpenQuota: a hard quota refuses at the open, with the one
// sentence every surface of the cap says, before a byte is sent.
func TestChunkedOpenQuota(t *testing.T) {
	e := newEnv(t, func(d *Deps) {
		d.Cfg.UploadChunkBytes = testChunk
		d.Cfg.StorageBudgetHard = true
		d.Cfg.StorageBudgetBytes = 1
	})
	_, cookie := e.newUser("carol")
	if r := e.upload(`{}`, "a.mp4", testVideo(10), withCookie(cookie)); r.code != 201 {
		t.Fatalf("the upload that fills the line: %d %s", r.code, r.body)
	}
	if r := e.openSession(10, "", withCookie(cookie)); r.code != 507 || r.errCode() != "storage_quota" || r.str("message") != quotaRefusal {
		t.Fatalf("open over the quota: %d %s", r.code, r.body)
	}
}

// TestChunkedFinishFailureKeepsTheSession: an ingest the probe refuses answers
// the one-shot's own 415, and the session (its bytes) is still there; a
// transient storage failure answers 503 and the same finish then succeeds.
func TestChunkedFinishFailureKeepsTheSession(t *testing.T) {
	e := chunkedEnv(t)
	_, cookie := e.newUser("carol")
	video := testVideo(1200)
	op := e.openSession(len(video), "", withCookie(cookie))
	id := op.str("id")
	e.putChunkBytes(id, 0, video[:1000], withCookie(cookie))
	e.putChunkBytes(id, 1, video[1000:], withCookie(cookie))

	e.fake.FailNext("PutFile", errors.New("bucket unreachable"))
	if r := e.do(http.MethodPost, "/api/uploads/"+id+"/finish", nil, withCookie(cookie)); r.code != 503 || r.errCode() != "storage_unavailable" {
		t.Fatalf("finish with the store down: %d %s", r.code, r.body)
	}
	if r := e.do(http.MethodGet, "/api/uploads/"+id, nil, withCookie(cookie)); r.code != 200 || received(r) != 2 {
		t.Fatalf("the session after a failed finish: %d %s", r.code, r.body)
	}
	if r := e.do(http.MethodPost, "/api/uploads/"+id+"/finish", nil, withCookie(cookie)); r.code != 201 {
		t.Fatalf("the finish retried: %d %s", r.code, r.body)
	}
	// A spool the store never holds outlives nothing: no `.upload-` dir is left.
	entries, _ := os.ReadDir(e.scratch)
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), ".upload-") {
			t.Fatalf("a spool dir outlived the finish: %s", en.Name())
		}
	}
}

// chunkedReader hides the reader's length from httptest, so the request goes
// out with no Content-Length.
type chunkedReader struct{ r *bytes.Reader }

func (c chunkedReader) Read(p []byte) (int, error) { return c.r.Read(p) }
