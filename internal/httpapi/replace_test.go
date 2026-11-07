package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"clippet/internal/db"
	"clippet/internal/store"
	"clippet/internal/transcode"
	"clippet/internal/worker"
)

// replaceReq posts one file part to POST /api/clips/{id}/replace.
func (e *env) replaceReq(id, partName, filename string, video []byte, opts ...reqOpt) resp {
	e.t.Helper()
	body, ct := multipartBody(e.t, part{name: partName, filename: filename, contentType: "video/mp4", data: video})
	return e.do(http.MethodPost, "/api/clips/"+id+"/replace", body,
		append([]reqOpt{withHeader("Content-Type", ct)}, opts...)...)
}

// replaceJob is the clip's latest replacement job.
func (e *env) replaceJob(id string) db.Job {
	e.t.Helper()
	j, err := e.db.LatestJobOfKind(context.Background(), id, db.KindReplace)
	if err != nil {
		e.t.Fatalf("LatestJobOfKind(replace, %s): %v", id, err)
	}
	return j
}

// TestReplaceQueuesTheJobAndLeavesTheClipAlone is the feature's core promise:
// the POST stores a NEW original beside the old one and queues a job, and
// changes nothing a viewer can see.
func TestReplaceQueuesTheJobAndLeavesTheClipAlone(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	before := e.getClip(c.ID)

	newSource := testVideo(2222)
	r := e.replaceReq(c.ID, "file", "better.mp4", newSource, withCookie(cookie))
	if r.code != 202 {
		t.Fatalf("POST replace: %d %s", r.code, r.body)
	}
	if r.str("state") != "queued" || r.json["job_id"] == nil {
		t.Fatalf("202 body = %s, want a job id and state=queued", r.body)
	}
	if e.worker.woken.Load() == 0 {
		t.Error("the worker was not woken; the replacement would wait out the 5s poll")
	}

	job := e.replaceJob(c.ID)
	p, err := db.ParseReplaceParams(job.ParamsJSON)
	if err != nil {
		t.Fatalf("params %s: %v", job.ParamsJSON, err)
	}
	// The new original is under the clip's own prefix, under a per-attempt key
	// that is NOT the key the row still points at (§20 erratum 135).
	if !strings.HasPrefix(p.SourceKey, store.ClipPrefix(e.cfg.S3.KeyPrefix, c.ID)+"original/source-") {
		t.Errorf("source key %q is not the §6 replacement shape", p.SourceKey)
	}
	if p.SourceKey == before.SourceKey {
		t.Errorf("the replacement overwrote the live original key %q", p.SourceKey)
	}
	if p.SourceSize != int64(len(newSource)) || p.SourceETag != sha256hex(newSource) {
		t.Errorf("params describe the wrong bytes: size=%d etag=%s", p.SourceSize, p.SourceETag)
	}
	if p.SourceFilename != "better.mp4" {
		t.Errorf("filename = %q", p.SourceFilename)
	}
	got, ok := e.fake.Get(p.SourceKey)
	if !ok || string(got) != string(newSource) {
		t.Fatalf("the replacement original is not in the bucket under %q", p.SourceKey)
	}
	// …and the OLD original is still there, still referenced. "Upload is
	// sacred": nothing is deleted until the new rendition is ready.
	if _, ok := e.fake.Get(before.SourceKey); !ok {
		t.Fatalf("the previous original %q was deleted before the replacement was ready", before.SourceKey)
	}

	after := e.getClip(c.ID)
	switch {
	case after.State != db.ClipReady:
		t.Errorf("clip state = %q, want ready throughout", after.State)
	case after.SourceKey != before.SourceKey || after.SourceSize != before.SourceSize:
		t.Errorf("the clip's source moved at enqueue time")
	case *after.RenditionKey != *before.RenditionKey || *after.DurationMS != *before.DurationMS:
		t.Errorf("the clip's rendition moved at enqueue time")
	case after.UpdatedAt != before.UpdatedAt:
		t.Errorf("updated_at moved at enqueue time: %d → %d", before.UpdatedAt, after.UpdatedAt)
	}
}

// TestReplaceIsSerialised: a second replacement while one is live is the 409
// the database decides, and the loser's upload does not stay in the bucket.
func TestReplaceIsSerialised(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})

	if r := e.replaceReq(c.ID, "file", "a.mp4", testVideo(300), withCookie(cookie)); r.code != 202 {
		t.Fatalf("first replace: %d %s", r.code, r.body)
	}
	first := e.replaceJob(c.ID)

	r := e.replaceReq(c.ID, "video", "b.mp4", testVideo(400), withCookie(cookie))
	if r.code != 409 || r.errCode() != "replace_in_progress" {
		t.Fatalf("second replace = %d %s, want 409 replace_in_progress", r.code, r.body)
	}
	// Exactly one job, and exactly one replacement object in the bucket: the
	// refused upload cleaned up after itself.
	if got := e.replaceJob(c.ID); got.ID != first.ID {
		t.Errorf("a second job was inserted: %d then %d", first.ID, got.ID)
	}
	n := 0
	for _, k := range e.fake.Keys() {
		if strings.Contains(k, "/original/source-") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d replacement originals in the bucket, want 1 (the loser's must be removed)", n)
	}
}

// TestReplaceAuthMatrix is the §8.1 auth column for both routes, erratum 19
// included: a capture token is an upload credential and must not be able to
// overwrite the clips it once created.
func TestReplaceAuthMatrix(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	stranger, strangerCookie := e.newUser("stranger")
	_ = stranger
	token := e.newToken(owner, "upload")
	c, _, _ := e.readyClip(owner, clipOpts{})

	cases := []struct {
		name string
		id   string
		opts []reqOpt
		code int
		err  string
	}{
		{"anonymous", c.ID, nil, 401, "unauthorized"},
		{"a capture token", c.ID, []reqOpt{withBearer(token)}, 403, "token_not_allowed"},
		{"another user", c.ID, []reqOpt{withCookie(strangerCookie)}, 403, "forbidden"},
		{"an unknown clip", "aaaaaaaaaaaa", []reqOpt{withCookie(cookie)}, 404, "not_found"},
		{"a malformed id", "nope", []reqOpt{withCookie(cookie)}, 404, "not_found"},
		{"a cross-site POST", c.ID, []reqOpt{withCookie(cookie), withHeader("Origin", "https://evil.example")}, 403, "csrf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := e.replaceReq(tc.id, "file", "x.mp4", testVideo(100), tc.opts...)
			if r.code != tc.code || r.errCode() != tc.err {
				t.Fatalf("POST = %d %s, want %d %s", r.code, r.body, tc.code, tc.err)
			}
			rr := e.do(http.MethodGet, "/api/clips/"+tc.id+"/replace", nil, tc.opts...)
			if rr.code != tc.code && !(tc.name == "a cross-site POST" && rr.code == 404) {
				t.Fatalf("GET = %d %s, want %d", rr.code, rr.body, tc.code)
			}
		})
	}
	// Nothing above got as far as the bucket.
	for _, k := range e.fake.Keys() {
		if strings.Contains(k, "/original/source-") {
			t.Fatalf("a refused request stored %q", k)
		}
	}
}

// TestReplaceRefusesAClipThatIsNotReady: queued, processing and failed clips
// have nothing to swap, and the answer names that rather than the body.
func TestReplaceRefusesAClipThatIsNotReady(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	// The failed one first: failedClip claims the oldest queued transcode job,
	// and seeding the other one first would hand it that clip's.
	failed := e.failedClip(owner, clipOpts{})
	queued := e.seedClip(owner, clipOpts{})

	for _, id := range []string{queued.ID, failed.ID} {
		r := e.replaceReq(id, "file", "x.mp4", testVideo(100), withCookie(cookie))
		if r.code != 409 || r.errCode() != "not_ready" {
			t.Fatalf("replace %s = %d %s, want 409 not_ready", id, r.code, r.body)
		}
	}
}

// TestReplaceRunsTheSameGatesAsAnUpload: the multipart grammar, the ffprobe
// gate and the scratch precheck are the upload's, not a second copy.
func TestReplaceRunsTheSameGatesAsAnUpload(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})

	t.Run("not multipart", func(t *testing.T) {
		r := e.do(http.MethodPost, "/api/clips/"+c.ID+"/replace", strings.NewReader("{}"),
			withCookie(cookie), withJSON())
		if r.code != 415 || r.errCode() != "not_multipart" {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	t.Run("no file part", func(t *testing.T) {
		body, ct := multipartBody(t, part{name: "title", data: []byte("nope")})
		r := e.do(http.MethodPost, "/api/clips/"+c.ID+"/replace", body,
			withCookie(cookie), withHeader("Content-Type", ct))
		if r.code != 400 || r.errCode() != "missing_video" {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	t.Run("the file part under the wrong name", func(t *testing.T) {
		r := e.replaceReq(c.ID, "clip", "x.mp4", testVideo(100), withCookie(cookie))
		if r.code != 400 || r.errCode() != "missing_video" {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	// An oversized text field is refused the way the capture form refuses one,
	// but the message must NOT carry the field's name. This route reads no
	// field, so the name is whatever the caller invented, and mime/multipart
	// carries megabytes of part headers happily: reflecting it would put
	// unbounded caller-chosen text in an owner-facing error, which is erratum
	// 26's rule for filenames applied to field names.
	t.Run("an oversized text field under a caller-chosen name", func(t *testing.T) {
		long := strings.Repeat("N", 4000)
		body, ct := multipartBody(t,
			part{name: long, data: []byte(strings.Repeat("x", maxCaptureFieldByte+1))},
			part{name: "file", filename: "x.mp4", contentType: "video/mp4", data: testVideo(100)})
		r := e.do(http.MethodPost, "/api/clips/"+c.ID+"/replace", body,
			withCookie(cookie), withHeader("Content-Type", ct))
		if r.code != 400 || r.errCode() != "invalid_request" || !strings.Contains(r.str("message"), "16 KiB") {
			t.Fatalf("%d %s", r.code, r.body)
		}
		if strings.Contains(string(r.body), long[:64]) {
			t.Fatalf("the field name was reflected into the error: %.200s", r.body)
		}
		if len(r.body) > 512 {
			t.Fatalf("the error is %d bytes: it must not grow with the caller's field name", len(r.body))
		}
	})
	t.Run("more text fields than the capture form takes", func(t *testing.T) {
		parts := make([]part, 0, maxCaptureFields+2)
		for i := 0; i <= maxCaptureFields; i++ {
			parts = append(parts, part{name: strings.Repeat("x", i+1), data: []byte("v")})
		}
		parts = append(parts, part{name: "file", filename: "x.mp4", contentType: "video/mp4", data: testVideo(100)})
		body, ct := multipartBody(t, parts...)
		r := e.do(http.MethodPost, "/api/clips/"+c.ID+"/replace", body,
			withCookie(cookie), withHeader("Content-Type", ct))
		if r.code != 400 || r.errCode() != "invalid_request" || !strings.Contains(r.str("message"), "too many form fields") {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	t.Run("an empty file", func(t *testing.T) {
		r := e.replaceReq(c.ID, "file", "x.mp4", nil, withCookie(cookie))
		if r.code != 400 || r.errCode() != "empty_video" {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	t.Run("not a video", func(t *testing.T) {
		e.probe = func(context.Context, string) (*transcode.ProbeResult, error) {
			return nil, &transcode.ErrPermanent{Err: errors.New("no video stream")}
		}
		defer func() {
			e.probe = func(context.Context, string) (*transcode.ProbeResult, error) {
				return &transcode.ProbeResult{VideoIndex: 0}, nil
			}
		}()
		r := e.replaceReq(c.ID, "file", "x.mp4", testVideo(100), withCookie(cookie))
		if r.code != 415 || r.errCode() != "not_a_video" {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	t.Run("too large for the scratch disk", func(t *testing.T) {
		e.free = func(string) (int64, error) { return 1, nil }
		defer func() { e.free = func(string) (int64, error) { return 1 << 40, nil } }()
		r := e.replaceReq(c.ID, "file", "x.mp4", testVideo(100), withCookie(cookie))
		if r.code != 507 || r.errCode() != "insufficient_scratch" {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	t.Run("past MAX_UPLOAD_BYTES", func(t *testing.T) {
		r := e.replaceReq(c.ID, "file", "x.mp4", testVideo(int(e.cfg.MaxUploadBytes)+(2<<20)), withCookie(cookie))
		if r.code != 413 || r.errCode() != "too_large" {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	// Every refusal above left the bucket and the job table untouched.
	for _, k := range e.fake.Keys() {
		if strings.Contains(k, "/original/source-") {
			t.Fatalf("a refused replacement stored %q", k)
		}
	}
	if _, err := e.db.LatestJobOfKind(context.Background(), c.ID, db.KindReplace); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("a refused replacement queued a job: %v", err)
	}
}

// TestReplaceStatus is the poller's contract: 404 until there is a job (so
// "no job" can never be read as "finished"), then the job's own state, with
// the live registry only while the row says running.
func TestReplaceStatus(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})

	r := e.do(http.MethodGet, "/api/clips/"+c.ID+"/replace", nil, withCookie(cookie))
	if r.code != 404 || r.errCode() != "not_found" {
		t.Fatalf("status before any replacement = %d %s, want 404", r.code, r.body)
	}

	if rr := e.replaceReq(c.ID, "file", "new.mp4", testVideo(300), withCookie(cookie)); rr.code != 202 {
		t.Fatalf("replace: %d %s", rr.code, rr.body)
	}
	r = e.do(http.MethodGet, "/api/clips/"+c.ID+"/replace", nil, withCookie(cookie))
	if r.code != 200 || r.str("state") != "queued" || r.str("filename") != "new.mp4" {
		t.Fatalf("queued status = %d %s", r.code, r.body)
	}
	if r.hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", r.hdr.Get("Cache-Control"))
	}

	// Running: the row says so, and the live EDIT registry is what the
	// percentage comes from (§20 erratum 71). The claim reads e.now(), never
	// time.Now(): the handler queued the row at one wall-clock read and the
	// container clock can step BACKWARDS before the next (harness_test.go's
	// lastNow; it failed check.sh once exactly so, 2026-09-10).
	job, clip, ok, err := e.db.ClaimMediaJob(context.Background(), e.now())
	if err != nil || !ok || clip.ID != c.ID || job.Kind != db.KindReplace {
		t.Fatalf("ClaimMediaJob = %+v %v %v", job, ok, err)
	}
	e.worker.trim[c.ID] = worker.Progress{State: db.JobRunning, Stage: "encode", Percent: 42}
	r = e.do(http.MethodGet, "/api/clips/"+c.ID+"/replace", nil, withCookie(cookie))
	if r.code != 200 || r.str("state") != "running" || r.str("stage") != "encode" || r.json["percent"] != float64(42) {
		t.Fatalf("running status = %d %s", r.code, r.body)
	}

	// Failed: the owner-facing sentence the run stored, and nothing else.
	if _, err := e.db.FailAttempt(context.Background(), job.ID, db.ClaimStamp(job), c.ID, "the file you uploaded could not be transcoded",
		db.FailPermanent, db.Limits{MaxAttempts: 1}, e.now()); err != nil {
		t.Fatal(err)
	}
	delete(e.worker.trim, c.ID)
	r = e.do(http.MethodGet, "/api/clips/"+c.ID+"/replace", nil, withCookie(cookie))
	if r.code != 200 || r.str("state") != "failed" || r.str("error") != "the file you uploaded could not be transcoded" {
		t.Fatalf("failed status = %d %s", r.code, r.body)
	}
	// …and the clip is still ready, on the file it had.
	if got := e.getClip(c.ID); got.State != db.ClipReady {
		t.Fatalf("a failed replacement moved the clip to %q", got.State)
	}
}

// The precondition is `ready` + PLAYABLE BYTES, and never `rendition_key`
// (§20 erratum 201, amended). Both rows below are ready clips whose
// `rendition_key` is legitimately NULL — the remux fast path publishes no second
// object, and card-first ordering publishes the LIGHT copy while the Full encodes
// — and both used to be refused 409 `not_ready` on a clip that plainly was ready:
// ROADMAP #31 was unreachable for exactly the upload DEPLOY §27 calls the ideal
// case, and the watch page rendered the Replace form for it anyway.
func TestReplaceAcceptsAReadyClipWithNoRenditionObject(t *testing.T) {
	for _, tc := range []struct {
		name string
		clip func(*env, db.User) db.Clip
	}{
		{"the remux fast path", func(e *env, o db.User) db.Clip { c, _ := e.playableClip(o); return c }},
		{"card-first, while the Full encodes", func(e *env, o db.User) db.Clip { return e.cardFirstClip(o) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			owner, cookie := e.newUser("owner")
			c := tc.clip(e, owner)
			if c.State != db.ClipReady || c.RenditionKey != nil {
				t.Fatalf("this fixture is not the row under test: state=%q rendition=%v", c.State, c.RenditionKey)
			}
			r := e.replaceReq(c.ID, "file", "better.mp4", testVideo(2222), withCookie(cookie))
			if r.code != 202 {
				t.Fatalf("POST replace on a ready clip with no rendition = %d %s, want 202", r.code, r.body)
			}
			if job := e.replaceJob(c.ID); job.State != db.JobQueued {
				t.Fatalf("the replacement job is %q", job.State)
			}
		})
	}
}

// A `ready` row with NO playable bytes at all is the §12 invariant violation
// every media route answers 503 for, and this route answers it the same way: it
// needs a human, not a `not_ready` its owner can do nothing about.
func TestReplaceOnAReadyClipWithNoPlayableBytesIs503(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c := e.seedClip(owner, clipOpts{})
	job := e.claim(c.ID)
	poster := []byte("poster-bytes")
	pk := store.PosterKey(e.cfg.S3.KeyPrefix, c.ID, "r8r8r8r8")
	e.fake.Put(pk, poster, "image/jpeg")
	// Ready, and nothing to play: no rendition, no light copy, an upload the
	// browser cannot stream. Only a bug can produce this row; the route still has
	// to have an answer for it.
	ok, _, err := e.db.FinalizeTranscode(context.Background(), job.ID, c.ID, db.ReadyFields{
		PosterKey: pk, PosterSize: int64(len(poster)), PosterETag: sha256hex(poster),
		PosterWidth: 640, PosterHeight: 360,
		Width: 1280, Height: 720, DurationMS: 6000, FPSNum: 30, FPSDen: 1,
		Tiers: db.TierFields{FullState: db.TierEncoding, LightState: db.TierSkipped},
	}, false, e.now())
	if err != nil || !ok {
		t.Fatalf("FinalizeTranscode: ok=%v err=%v", ok, err)
	}
	r := e.replaceReq(c.ID, "file", "better.mp4", testVideo(300), withCookie(cookie))
	if r.code != 503 || r.errCode() != "unavailable" {
		t.Fatalf("replace on a ready clip with no playable bytes = %d %s, want 503 unavailable", r.code, r.body)
	}
}
