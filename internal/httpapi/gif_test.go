package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/store"
)

// The §17 #6 routes. The public GET's answer table is the whole feature:
// 200 when there is a preview, 202 while one is being made, 409 when there
// is nothing to make one from, 503 when the last attempt failed, 404 for a
// clip that is not there.

const gifBytes = "GIF89a-fake-preview-bytes"

// withGIF gives a ready clip a published preview, through the real
// enqueue → claim → SetGIF path (there is no other writer of gif_*).
func (e *env) withGIF(c db.Clip, key string) db.Clip {
	e.t.Helper()
	ctx := context.Background()
	job, err := e.db.EnqueueGIF(ctx, c.ID, e.now())
	if errors.Is(err, db.ErrConflict) {
		// The route under test already queued one: finish THAT job rather
		// than a second, which the guard would refuse anyway.
		job, err = e.db.LatestJobOfKind(ctx, c.ID, db.KindGIF)
	}
	if err != nil {
		e.t.Fatal(err)
	}
	if job.State == db.JobQueued {
		if _, _, ok, err := e.db.ClaimMediaJob(ctx, e.now()); err != nil || !ok {
			e.t.Fatalf("ClaimMediaJob: ok=%v err=%v", ok, err)
		}
	}
	e.fake.Put(key, []byte(gifBytes), "image/gif")
	swap, err := e.db.SetGIF(ctx, job.ID, c.ID, db.GIFFields{
		Key: key, Size: int64(len(gifBytes)), ETag: sha256hex([]byte(gifBytes)), Width: 480, Height: 270,
	}, *c.RenditionETag, e.now())
	if err != nil || !swap.OK {
		e.t.Fatalf("SetGIF: %+v %v", swap, err)
	}
	return e.getClip(c.ID)
}

func gifKey(e *env, id string) string { return store.GIFKey(e.cfg.S3.KeyPrefix, id, "g8g8g8g8") }

// TestPreviewGIFEnqueuesThenServes walks the whole route contract in the
// order a real caller meets it.
func TestPreviewGIFEnqueuesThenServes(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})

	// 1. ANONYMOUS first request: 202, Retry-After, and one job queued.
	r := e.do("GET", "/api/clips/"+c.ID+"/preview.gif", nil)
	if r.code != 202 {
		t.Fatalf("first GET = %d %s, want 202", r.code, r.body)
	}
	if r.str("state") != "queued" {
		t.Errorf("202 body = %s, want state=queued", r.body)
	}
	if got := r.hdr.Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q", got)
	}
	if got := r.hdr.Get("Cache-Control"); got != "no-store" {
		t.Errorf("202 Cache-Control = %q, want no-store", got)
	}
	if e.worker.woken.Load() == 0 {
		t.Error("the worker was not woken")
	}
	job, err := e.db.LatestJobOfKind(context.Background(), c.ID, db.KindGIF)
	if err != nil || job.Kind != db.KindGIF || job.State != db.JobQueued {
		t.Fatalf("gif job = %+v, %v", job, err)
	}

	// 2. A second caller finds the SAME job: a public route may not be a way
	//    to pile work onto the queue.
	if r = e.do("GET", "/api/clips/"+c.ID+"/preview.gif", nil); r.code != 202 {
		t.Fatalf("second GET = %d %s, want 202", r.code, r.body)
	}
	second, _ := e.db.LatestJobOfKind(context.Background(), c.ID, db.KindGIF)
	if second.ID != job.ID {
		t.Fatalf("a second job was queued (%d then %d)", job.ID, second.ID)
	}

	// 3. Once the job publishes one, the same URL streams it.
	c = e.withGIF(c, gifKey(e, c.ID))
	r = e.do("GET", "/api/clips/"+c.ID+"/preview.gif", nil)
	if r.code != 200 {
		t.Fatalf("GET after the job = %d %s", r.code, r.body)
	}
	if ct := r.hdr.Get("Content-Type"); ct != "image/gif" {
		t.Errorf("Content-Type = %q", ct)
	}
	if string(r.body) != gifBytes {
		t.Errorf("body = %q", r.body)
	}
	etag := r.hdr.Get("ETag")
	if etag != `"`+sha256hex([]byte(gifBytes))+`"` {
		t.Errorf("ETag = %q, want the hex sha256 from the row", etag)
	}
	if cc := r.hdr.Get("Cache-Control"); cc != mediaCacheControl {
		t.Errorf("Cache-Control = %q, want the shared media policy %q", cc, mediaCacheControl)
	}
	if ar := r.hdr.Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("Accept-Ranges = %q", ar)
	}
	if r.hdr.Get("Retry-After") != "" {
		t.Error("a served preview must not carry Retry-After")
	}

	// 4. Conditional and ranged requests go through serveObject like every
	//    other media route.
	r = e.do("GET", "/api/clips/"+c.ID+"/preview.gif", nil, withHeader("If-None-Match", etag))
	if r.code != 304 || len(r.body) != 0 {
		t.Fatalf("If-None-Match = %d (%d bytes), want a bodiless 304", r.code, len(r.body))
	}
	r = e.do("GET", "/api/clips/"+c.ID+"/preview.gif", nil, withHeader("Range", "bytes=0-3"))
	if r.code != 206 || string(r.body) != gifBytes[:4] {
		t.Fatalf("Range = %d %q", r.code, r.body)
	}
	r = e.do("HEAD", "/api/clips/"+c.ID+"/preview.gif", nil)
	if r.code != 200 || len(r.body) != 0 {
		t.Fatalf("HEAD = %d (%d bytes)", r.code, len(r.body))
	}
}

// TestPreviewGIFNotFoundAndNotReady: the two answers that are NOT about the
// preview at all.
func TestPreviewGIFNotFoundAndNotReady(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")

	for _, path := range []string{
		"/api/clips/" + ids.New() + "/preview.gif", // unknown
		"/api/clips/nope/preview.gif",              // malformed id: 404 before any DB read
		"/api/clips/" + e.deletingClip(owner).ID + "/preview.gif",
	} {
		if r := e.do("GET", path, nil); r.code != 404 || r.errCode() != "not_found" {
			t.Errorf("GET %s = %d %s, want 404 not_found", path, r.code, r.body)
		}
	}
	// A clip that is not ready has nothing to preview and nothing to make one
	// from — and asking must not queue a job.
	failed := e.failedClip(owner, clipOpts{}) // claims its own job: create it first
	queued := e.seedClip(owner, clipOpts{})
	for _, c := range []db.Clip{queued, failed} {
		r := e.do("GET", "/api/clips/"+c.ID+"/preview.gif", nil)
		if r.code != 409 || r.errCode() != "not_ready" {
			t.Fatalf("GET on a %s clip = %d %s, want 409 not_ready", c.State, r.code, r.body)
		}
		if _, err := e.db.LatestJobOfKind(context.Background(), c.ID, db.KindGIF); err == nil {
			t.Errorf("a %s clip got a gif job", c.State)
		}
	}
}

// TestPreviewGIFReportsAFailedJob: the route stops asking for work that has
// already failed and says why, once, at 503.
func TestPreviewGIFReportsAFailedJob(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})

	if r := e.do("GET", "/api/clips/"+c.ID+"/preview.gif", nil); r.code != 202 {
		t.Fatalf("first GET = %d", r.code)
	}
	job := e.claimGIF(c.ID)
	if _, err := e.db.FailAttempt(context.Background(), job.ID, db.ClaimStamp(job), c.ID, "gif: encode exited 1", db.FailPermanent, db.Limits{}, e.now()); err != nil {
		t.Fatal(err)
	}

	r := e.do("GET", "/api/clips/"+c.ID+"/preview.gif", nil)
	if r.code != 503 || r.errCode() != "gif_failed" {
		t.Fatalf("GET after a failed job = %d %s, want 503 gif_failed", r.code, r.body)
	}
	if msg, _ := r.json["message"].(string); !strings.Contains(msg, "encode exited 1") {
		t.Errorf("message = %q, want the job's reason", msg)
	}
	if r.hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("503 Cache-Control = %q", r.hdr.Get("Cache-Control"))
	}
	// And it did NOT re-enqueue: a doomed encode must not be re-run for every
	// visitor of a page that links the preview.
	latest, _ := e.db.LatestJobOfKind(context.Background(), c.ID, db.KindGIF)
	if latest.ID != job.ID || latest.State != db.JobFailed {
		t.Fatalf("the failed job was replaced: %+v", latest)
	}
	// The OWNER's explicit retry is what starts a new one.
	if r := e.do("POST", "/api/clips/"+c.ID+"/gif", nil, withCookie(cookie)); r.code != 202 {
		t.Fatalf("owner POST after a failure = %d %s, want 202", r.code, r.body)
	}
	retried, _ := e.db.LatestJobOfKind(context.Background(), c.ID, db.KindGIF)
	if retried.ID == job.ID || retried.State != db.JobQueued {
		t.Fatalf("the retry did not queue a new job: %+v", retried)
	}
	// …and while THAT one is live the GET is a 202 again, not a 503.
	if r := e.do("GET", "/api/clips/"+c.ID+"/preview.gif", nil); r.code != 202 {
		t.Fatalf("GET while the retry is queued = %d %s", r.code, r.body)
	}
}

// TestStartGIFAuthMatrix is the §14 owner-route contract: the POST is the
// owner's alone, while the GET stays public (the link is the auth, §8.5).
func TestStartGIFAuthMatrix(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	_, otherCookie := e.newUser("other")
	c, _, _ := e.readyClip(owner, clipOpts{})
	tok := e.newToken(owner, "upload")

	for _, tc := range []struct {
		name   string
		opts   []reqOpt
		code   int
		errStr string
	}{
		{"anonymous", nil, 401, "unauthorized"},
		{"a stranger", []reqOpt{withCookie(otherCookie)}, 403, "forbidden"},
		{"a capture token", []reqOpt{withBearer(tok)}, 403, "token_not_allowed"},
		{"a cross-origin form", []reqOpt{withCookie(cookie), withHeader("Origin", "https://evil.example")}, 403, "csrf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := e.do("POST", "/api/clips/"+c.ID+"/gif", nil, tc.opts...)
			if r.code != tc.code || r.errCode() != tc.errStr {
				t.Fatalf("POST = %d %s, want %d %s", r.code, r.body, tc.code, tc.errStr)
			}
		})
	}
	// The anonymous GET is unaffected by all of that.
	if r := e.do("GET", "/api/clips/"+c.ID+"/preview.gif", nil); r.code != 202 {
		t.Fatalf("anonymous GET = %d %s, want 202", r.code, r.body)
	}
	// The owner's POST while that job is live is the one 409 of the route.
	r := e.do("POST", "/api/clips/"+c.ID+"/gif", nil, withCookie(cookie))
	if r.code != 409 || r.errCode() != "gif_in_progress" {
		t.Fatalf("POST while one is queued = %d %s, want 409 gif_in_progress", r.code, r.body)
	}
	// An unknown clip is a 404 for the owner too, and a not-ready clip a 409.
	if r := e.do("POST", "/api/clips/"+ids.New()+"/gif", nil, withCookie(cookie)); r.code != 404 {
		t.Errorf("POST on an unknown clip = %d", r.code)
	}
	queued := e.seedClip(owner, clipOpts{})
	if r := e.do("POST", "/api/clips/"+queued.ID+"/gif", nil, withCookie(cookie)); r.code != 409 || r.errCode() != "not_ready" {
		t.Errorf("POST on a queued clip = %d %s, want 409 not_ready", r.code, r.body)
	}
	// Method mismatches keep the §8.1 shape.
	if r := e.do("GET", "/api/clips/"+c.ID+"/gif", nil, withCookie(cookie)); r.code != 405 {
		t.Errorf("GET on the POST route = %d, want 405", r.code)
	}
	if r := e.do("POST", "/api/clips/"+c.ID+"/preview.gif", nil, withCookie(cookie)); r.code != 405 {
		t.Errorf("POST on the media route = %d, want 405", r.code)
	}
}

// TestClipDTOCarriesTheGIFURL: null until there is one, absolute afterwards,
// and the same for every caller (the preview is as public as the poster).
func TestClipDTOCarriesTheGIFURL(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})

	r := e.do("GET", "/api/clips/"+c.ID, nil, withCookie(cookie))
	if r.code != 200 {
		t.Fatalf("GET clip = %d", r.code)
	}
	if !r.has("gif_url") {
		t.Fatal("gif_url is absent; §8.2 says null, not missing")
	}
	if r.json["gif_url"] != nil {
		t.Fatalf("gif_url = %v before a preview exists, want null", r.json["gif_url"])
	}

	e.withGIF(c, gifKey(e, c.ID))
	want := testOrigin + "/api/clips/" + c.ID + "/preview.gif"
	for _, tc := range []struct {
		name string
		opts []reqOpt
	}{{"owner", []reqOpt{withCookie(cookie)}}, {"anonymous", nil}} {
		r := e.do("GET", "/api/clips/"+c.ID, nil, tc.opts...)
		if got, _ := r.json["gif_url"].(string); got != want {
			t.Errorf("%s gif_url = %q, want %q", tc.name, got, want)
		}
	}
}

// claimGIF claims the clip's queued gif job through the real non-transcode
// claim, so a test that fails it acts on a genuinely running row.
func (e *env) claimGIF(clipID string) db.Job {
	e.t.Helper()
	j, _, ok, err := e.db.ClaimMediaJob(context.Background(), e.now())
	if err != nil || !ok || j.ClipID != clipID || j.Kind != db.KindGIF {
		e.t.Fatalf("ClaimMediaJob = %+v, %v, %v", j, ok, err)
	}
	return j
}

// TestPreviewGIFHeadIsTheClientsPoll: the share page asks `preview.gif` with
// HEAD rather than GET — both on the owner's first click and on every poll
// after it — because a ready preview answers a GET with the whole GIF and
// neither ask wants the bytes: the <img> is the one download (§20 erratum
// 87(f), and the click since the Phase 5 sweep). That makes the HEAD path
// load-bearing, so it is pinned here: a HEAD before any preview exists
// answers 202 with Retry-After and ENQUEUES the job exactly as the GET does —
// byMethod maps HEAD onto the GET handler — and a second HEAD does not queue
// a second job. (The 202's body is not asserted here: this harness is an
// httptest.Recorder, and it is net/http's own server that drops a HEAD body,
// not the handler.)
func TestPreviewGIFHeadIsTheClientsPoll(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})

	r := e.do("HEAD", "/api/clips/"+c.ID+"/preview.gif", nil)
	if r.code != 202 {
		t.Fatalf("first HEAD = %d %s, want 202", r.code, r.body)
	}
	if r.hdr.Get("Retry-After") != gifRetryAfter || r.hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("202 headers = %v", r.hdr)
	}
	job, err := e.db.LatestJobOfKind(context.Background(), c.ID, db.KindGIF)
	if err != nil {
		t.Fatalf("the HEAD queued no gif job: %v", err)
	}
	// A second HEAD is the poll, and it must not queue a second job.
	if r = e.do("HEAD", "/api/clips/"+c.ID+"/preview.gif", nil); r.code != 202 {
		t.Fatalf("second HEAD = %d", r.code)
	}
	again, err := e.db.LatestJobOfKind(context.Background(), c.ID, db.KindGIF)
	if err != nil || again.ID != job.ID {
		t.Fatalf("the poll queued a second job: %v %+v", err, again)
	}
	// It reports the WINNER's state, not a flat "running" (§20 erratum 87(e)).
	body := e.do("GET", "/api/clips/"+c.ID+"/preview.gif", nil)
	if body.code != 202 || body.str("state") != db.JobQueued {
		t.Fatalf("the losing enqueue reported %q, want %q: %d %s", body.str("state"), db.JobQueued, body.code, body.body)
	}
}
