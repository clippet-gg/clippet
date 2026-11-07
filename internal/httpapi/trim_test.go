package httpapi

import (
	"context"
	"testing"

	"clippet/internal/db"
	"clippet/internal/worker"
)

// trimBody is the request shape; a map so a test can omit a field.
func trimBody(start, end int64) map[string]any {
	return map[string]any{"start_ms": start, "end_ms": end}
}

func TestStartTrimQueuesTheJob(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})

	r := e.doJSON("POST", "/api/clips/"+c.ID+"/trim", trimBody(1000, 3000), withCookie(cookie))
	if r.code != 202 {
		t.Fatalf("POST trim: %d %s", r.code, r.body)
	}
	if r.str("state") != "queued" || r.json["job_id"] == nil {
		t.Fatalf("202 body = %s, want a job id and state=queued", r.body)
	}
	if e.worker.woken.Load() == 0 {
		t.Error("the worker was not woken; the trim would wait out the 5s poll")
	}
	job, err := e.db.LatestJobOfKind(context.Background(), c.ID, db.KindTrim)
	if err != nil {
		t.Fatal(err)
	}
	if job.ParamsJSON != `{"start_ms":1000,"end_ms":3000}` {
		t.Errorf("params = %s", job.ParamsJSON)
	}
	// The CLIP is untouched by the request: still ready, still on its old
	// rendition, still the same duration.
	got, _ := e.db.GetClip(context.Background(), c.ID)
	if got.State != db.ClipReady || *got.DurationMS != 6000 {
		t.Errorf("clip changed at enqueue time: %q %d", got.State, *got.DurationMS)
	}
	// A second POST while the first is live is the 409 the DB decides.
	r = e.doJSON("POST", "/api/clips/"+c.ID+"/trim", trimBody(0, 2000), withCookie(cookie))
	if r.code != 409 || r.errCode() != "trim_in_progress" {
		t.Fatalf("second POST = %d %s, want 409 trim_in_progress", r.code, r.body)
	}
}

func TestStartTrimValidation(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{}) // duration_ms = 6000

	cases := []struct {
		name string
		body any
	}{
		{"start after end", trimBody(3000, 1000)},
		{"start equals end", trimBody(1000, 1000)},
		{"negative start", trimBody(-1, 3000)},
		{"past the ORIGINAL's duration", trimBody(1000, 6001)},
		{"shorter than half a second", trimBody(1000, 1499)},
		{"the current window is a no-op", trimBody(0, 6000)},
		{"start_ms missing", map[string]any{"end_ms": 3000}},
		{"end_ms missing", map[string]any{"start_ms": 0}},
		{"both missing", map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := e.doJSON("POST", "/api/clips/"+c.ID+"/trim", tc.body, withCookie(cookie))
			if r.code != 400 || r.errCode() != "invalid_trim" {
				t.Fatalf("%d %s, want 400 invalid_trim", r.code, r.body)
			}
		})
	}
	// The edges that ARE valid: exactly 500 ms, and ending exactly at the
	// duration as long as the start moved.
	if r := e.doJSON("POST", "/api/clips/"+c.ID+"/trim", trimBody(1000, 1500), withCookie(cookie)); r.code != 202 {
		t.Fatalf("a 500 ms window must be accepted: %d %s", r.code, r.body)
	}
	// And no job was created by any of the rejections: exactly one exists.
	job, err := e.db.LatestJobOfKind(context.Background(), c.ID, db.KindTrim)
	if err != nil || job.ParamsJSON != `{"start_ms":1000,"end_ms":1500}` {
		t.Fatalf("latest trim job = %+v, %v", job, err)
	}
}

// TestTrimWindowsAreOnTheOriginalsTimeline is the contract of §20 erratum 68,
// and the regression test for the bug it replaced: a window used to be
// validated against the RENDITION's duration while being CUT from the
// original at the same offsets, so one trim shrank the reachable region for
// ever and the second cut came out of the wrong seconds entirely.
func TestTrimWindowsAreOnTheOriginalsTimeline(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	stranger, strangerCookie := e.newUser("stranger")
	_ = stranger
	c, _, _ := e.readyClip(owner, clipOpts{}) // a 6000 ms original, untrimmed

	// The untrimmed clip tells its owner both facts; nobody else gets either.
	r := e.do("GET", "/api/clips/"+c.ID, nil, withCookie(cookie))
	if r.code != 200 || r.json["source_duration_ms"] != float64(6000) {
		t.Fatalf("owner DTO = %s, want source_duration_ms 6000", r.body)
	}
	if _, ok := r.json["trim"]; ok {
		t.Errorf("an untrimmed clip has a trim window: %s", r.body)
	}
	r = e.do("GET", "/api/clips/"+c.ID, nil, withCookie(strangerCookie))
	if _, ok := r.json["source_duration_ms"]; ok {
		t.Errorf("source_duration_ms leaked to a stranger: %s", r.body)
	}

	// Cut it to 4000–6000 the way the worker does: the rendition is now 2 s.
	if r := e.doJSON("POST", "/api/clips/"+c.ID+"/trim", trimBody(4000, 6000), withCookie(cookie)); r.code != 202 {
		t.Fatalf("first trim: %d %s", r.code, r.body)
	}
	job := e.claimTrim(c.ID)
	if _, _, _, err := e.db.SwapRendition(context.Background(), job.ID, c.ID, db.ReadyFields{
		RenditionKey: "p/r2", RenditionETag: "e2", PosterKey: "p/p2", PosterETag: "pe2",
		Width: 1280, Height: 720, DurationMS: 2000, FPSNum: 30, FPSDen: 1,
	}, db.TrimParams{StartMS: 4000, EndMS: 6000}, e.now()); err != nil {
		t.Fatal(err)
	}
	got := e.getClip(c.ID)
	if *got.DurationMS != 2000 || got.TrimStartMS == nil || *got.TrimStartMS != 4000 || *got.TrimEndMS != 6000 {
		t.Fatalf("after the swap: duration=%v trim=%v..%v", *got.DurationMS, got.TrimStartMS, got.TrimEndMS)
	}
	r = e.do("GET", "/api/clips/"+c.ID, nil, withCookie(cookie))
	trim, _ := r.json["trim"].(map[string]any)
	if trim == nil || trim["start_ms"] != float64(4000) || trim["end_ms"] != float64(6000) {
		t.Fatalf("owner DTO after the trim = %s, want trim 4000..6000", r.body)
	}
	if r.json["source_duration_ms"] != float64(6000) {
		t.Errorf("source_duration_ms moved with the cut: %s", r.body)
	}

	// The whole original is still reachable — including footage this cut threw
	// away, which is the entire point of retaining the upload.
	for _, w := range []struct {
		name       string
		start, end int64
	}{
		{"re-extending past the current cut", 1000, 3000},
		{"a window spanning it", 0, 6000 - 1},
		{"the tail of the original", 5000, 6000},
	} {
		r := e.doJSON("POST", "/api/clips/"+c.ID+"/trim", trimBody(w.start, w.end), withCookie(cookie))
		if r.code != 202 {
			t.Fatalf("%s (%d..%d) = %d %s, want 202", w.name, w.start, w.end, r.code, r.body)
		}
		// Claim it and cancel it as its own run would (CancelJob is pinned to the
		// claim, §20 erratum 291), so the next case is not a 409.
		tj := e.claimTrim(c.ID)
		if err := e.db.CancelJob(context.Background(), tj.ID, *tj.StartedAt, e.now()); err != nil {
			t.Fatal(err)
		}
	}

	// What is refused is the CURRENT window (a no-op) and anything past the
	// original — never a window merely outside the current cut.
	for _, tc := range []struct {
		name       string
		start, end int64
	}{
		{"the cut it already has", 4000, 6000},
		{"past the original", 1000, 6001},
	} {
		r := e.doJSON("POST", "/api/clips/"+c.ID+"/trim", trimBody(tc.start, tc.end), withCookie(cookie))
		if r.code != 400 || r.errCode() != "invalid_trim" {
			t.Fatalf("%s = %d %s, want 400 invalid_trim", tc.name, r.code, r.body)
		}
	}
}

// TestTrimWindowIsUnboundedWhenTheOriginalIsUnmeasured: a row that predates
// migration 0003 and has already been trimmed has no source_duration_ms, and
// then the handler stops guessing — the worker's probe of the original is
// what rejects a window the file does not cover (§20 erratum 78).
func TestTrimWindowIsUnboundedWhenTheOriginalIsUnmeasured(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	e.nullClipColumns(c.ID, "source_duration_ms")
	e.setClipColumn(c.ID, "duration_ms", 2000)
	// Far past the rendition's own 2 s, and accepted: nothing here knows how
	// long the original is, and pretending otherwise is what broke re-extending.
	if r := e.doJSON("POST", "/api/clips/"+c.ID+"/trim", trimBody(9000, 30000), withCookie(cookie)); r.code != 202 {
		t.Fatalf("unbounded trim = %d %s, want 202", r.code, r.body)
	}
	// The absurd is still refused, and so is a clip DTO field nobody can fill.
	tj := e.claimTrim(c.ID)
	if err := e.db.CancelJob(context.Background(), tj.ID, *tj.StartedAt, e.now()); err != nil {
		t.Fatal(err)
	}
	if r := e.doJSON("POST", "/api/clips/"+c.ID+"/trim", trimBody(0, 24*60*60*1000+1), withCookie(cookie)); r.code != 400 {
		t.Fatalf("a day-long window = %d %s, want 400", r.code, r.body)
	}
	r := e.do("GET", "/api/clips/"+c.ID, nil, withCookie(cookie))
	if _, ok := r.json["source_duration_ms"]; ok {
		t.Errorf("source_duration_ms is present on an unmeasured clip: %s", r.body)
	}
}

func TestStartTrimRequiresAReadyClip(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	queued := e.seedClip(owner, clipOpts{}) // still `queued`
	r := e.doJSON("POST", "/api/clips/"+queued.ID+"/trim", trimBody(0, 2000), withCookie(cookie))
	if r.code != 409 || r.errCode() != "not_ready" {
		t.Fatalf("trim of a queued clip = %d %s, want 409 not_ready", r.code, r.body)
	}
}

// TestTrimAuthMatrix is the §14 owner-route contract, identical to PATCH and
// touch: anonymous 401, a stranger 403, a capture token 403 token_not_allowed
// (§20 erratum 19), an unknown clip 404 for the owner.
func TestTrimAuthMatrix(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	_, otherCookie := e.newUser("other")
	c, _, _ := e.readyClip(owner, clipOpts{})
	tok := e.newToken(owner, "upload")

	for _, tc := range []struct {
		name   string
		method string
		body   any
		opts   []reqOpt
		code   int
		errStr string
	}{
		{"anonymous POST", "POST", trimBody(0, 2000), nil, 401, "unauthorized"},
		{"anonymous GET", "GET", nil, nil, 401, "unauthorized"},
		{"stranger POST", "POST", trimBody(0, 2000), []reqOpt{withCookie(otherCookie)}, 403, "forbidden"},
		{"stranger GET", "GET", nil, []reqOpt{withCookie(otherCookie)}, 403, "forbidden"},
		{"token POST", "POST", trimBody(0, 2000), []reqOpt{withBearer(tok)}, 403, "token_not_allowed"},
		{"token GET", "GET", nil, []reqOpt{withBearer(tok)}, 403, "token_not_allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r resp
			if tc.method == "GET" {
				r = e.do("GET", "/api/clips/"+c.ID+"/trim", nil, tc.opts...)
			} else {
				r = e.doJSON("POST", "/api/clips/"+c.ID+"/trim", tc.body, tc.opts...)
			}
			if r.code != tc.code || r.errCode() != tc.errStr {
				t.Fatalf("%d %s, want %d %s", r.code, r.body, tc.code, tc.errStr)
			}
		})
	}
	// Unknown and malformed ids are 404 for the owner, before any work.
	for _, id := range []string{"nosuchclipid", "short"} {
		if r := e.doJSON("POST", "/api/clips/"+id+"/trim", trimBody(0, 2000), withCookie(cookie)); r.code != 404 {
			t.Errorf("POST trim %s = %d %s, want 404", id, r.code, r.body)
		}
		if r := e.do("GET", "/api/clips/"+id+"/trim", nil, withCookie(cookie)); r.code != 404 {
			t.Errorf("GET trim %s = %d %s, want 404", id, r.code, r.body)
		}
	}
	// A cross-origin form post is still CSRF-blocked.
	if r := e.doJSON("POST", "/api/clips/"+c.ID+"/trim", trimBody(0, 2000), withCookie(cookie),
		withHeader("Origin", "https://evil.example")); r.code != 403 {
		t.Errorf("cross-origin trim = %d %s, want 403", r.code, r.body)
	}
	// And a non-JSON body is refused before it is read.
	if r := e.do("POST", "/api/clips/"+c.ID+"/trim", nil, withCookie(cookie),
		withHeader("Content-Type", "text/plain")); r.code != 415 {
		t.Errorf("non-JSON trim = %d %s, want 415", r.code, r.body)
	}
}

func TestTrimStatus(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})

	// No trim yet: 404, which a poller must never read as "finished".
	r := e.do("GET", "/api/clips/"+c.ID+"/trim", nil, withCookie(cookie))
	if r.code != 404 || r.errCode() != "not_found" {
		t.Fatalf("GET before any trim = %d %s, want 404", r.code, r.body)
	}

	if r := e.doJSON("POST", "/api/clips/"+c.ID+"/trim", trimBody(1000, 3000), withCookie(cookie)); r.code != 202 {
		t.Fatalf("POST: %d %s", r.code, r.body)
	}
	// Queued: the row is the whole answer.
	r = e.do("GET", "/api/clips/"+c.ID+"/trim", nil, withCookie(cookie))
	if r.code != 200 || r.str("state") != "queued" || r.json["percent"] != float64(0) ||
		r.json["start_ms"] != float64(1000) || r.json["end_ms"] != float64(3000) || r.json["error"] != nil {
		t.Fatalf("queued status = %s", r.body)
	}
	if r.hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("trim status must not be cached: %q", r.hdr.Get("Cache-Control"))
	}

	// Running with a live registry entry: the registry wins.
	job := e.claimTrim(c.ID)
	e.worker.trim[c.ID] = worker.Progress{State: db.JobRunning, Stage: "encode", Percent: 42}
	r = e.do("GET", "/api/clips/"+c.ID+"/trim", nil, withCookie(cookie))
	if r.code != 200 || r.str("state") != "running" || r.str("stage") != "encode" || r.json["percent"] != float64(42) {
		t.Fatalf("running status = %s", r.body)
	}

	// A restart drops the registry; the persisted stage still gives a band.
	delete(e.worker.trim, c.ID)
	if err := e.db.SetJobStage(context.Background(), job.ID, *job.StartedAt, "store", e.now()); err != nil {
		t.Fatal(err)
	}
	r = e.do("GET", "/api/clips/"+c.ID+"/trim", nil, withCookie(cookie))
	if r.code != 200 || r.str("stage") != "store" || r.json["percent"] != float64(worker.StagePercent("store")) {
		t.Fatalf("after-restart status = %s", r.body)
	}

	// Done: 100, no error, and the CLIP's own progress is untouched by all
	// of this — §8.4 still reports a ready clip.
	if _, _, _, err := e.db.SwapRendition(context.Background(), job.ID, c.ID, db.ReadyFields{
		RenditionKey: "p/r2", RenditionETag: "e2", PosterKey: "p/p2", PosterETag: "pe2",
		Width: 1280, Height: 720, DurationMS: 2000, FPSNum: 30, FPSDen: 1,
	}, db.TrimParams{StartMS: 1000, EndMS: 3000}, e.now()); err != nil {
		t.Fatal(err)
	}
	r = e.do("GET", "/api/clips/"+c.ID+"/trim", nil, withCookie(cookie))
	if r.code != 200 || r.str("state") != "done" || r.json["percent"] != float64(100) || r.json["error"] != nil {
		t.Fatalf("done status = %s", r.body)
	}
	// …and it reports what was APPLIED, read from the clip the swap wrote.
	if r.json["applied_start_ms"] != float64(1000) || r.json["applied_end_ms"] != float64(3000) {
		t.Errorf("done status has no applied window: %s", r.body)
	}
	if p := e.do("GET", "/api/clips/"+c.ID+"/progress", nil); p.code != 200 || p.str("state") != "ready" {
		t.Fatalf("the clip's own progress = %s; a trim never changes it", p.body)
	}
}

func TestTrimStatusReportsAFailure(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	if r := e.doJSON("POST", "/api/clips/"+c.ID+"/trim", trimBody(1000, 3000), withCookie(cookie)); r.code != 202 {
		t.Fatalf("POST: %d %s", r.code, r.body)
	}
	job := e.claimTrim(c.ID)
	// The text a trim run actually stores is one of erratum 80's seven
	// sentences — the route re-checks that, and answers with the kind's
	// generic sentence for anything else (§20 erratum 165), so seeding an
	// ffmpeg stderr tail here would be testing a row the worker cannot write.
	if _, err := e.db.FailAttempt(context.Background(), job.ID, db.ClaimStamp(job), c.ID, "this clip's original could not be trimmed",
		db.FailPermanent, db.Limits{}, e.now()); err != nil {
		t.Fatal(err)
	}
	r := e.do("GET", "/api/clips/"+c.ID+"/trim", nil, withCookie(cookie))
	if r.code != 200 || r.str("state") != "failed" || r.str("error") != "this clip's original could not be trimmed" {
		t.Fatalf("failed status = %s", r.body)
	}
	// The clip is untouched: still ready, still playable.
	got, _ := e.db.GetClip(context.Background(), c.ID)
	if got.State != db.ClipReady || got.FailureReason != nil {
		t.Fatalf("clip = %q/%v after a failed trim", got.State, got.FailureReason)
	}
	if v := e.do("GET", "/api/clips/"+c.ID+"/video.mp4", nil); v.code != 200 {
		t.Errorf("video.mp4 after a failed trim = %d", v.code)
	}
}
