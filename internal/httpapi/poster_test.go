package httpapi

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"clippet/internal/db"
)

// posterBody is the request shape; a map so a test can omit or mistype the
// one field.
func posterBody(seconds float64) map[string]any { return map[string]any{"seconds": seconds} }

// claimPoster claims the clip's queued poster job through the real media
// claim, so a status test sees a genuinely running row.
func (e *env) claimPoster(clipID string) db.Job {
	e.t.Helper()
	j, _, ok, err := e.db.ClaimMediaJob(context.Background(), e.now())
	if err != nil || !ok || j.ClipID != clipID || j.Kind != db.KindPoster {
		e.t.Fatalf("ClaimMediaJob = %+v, %v, %v", j, ok, err)
	}
	return j
}

func TestStartPosterQueuesTheJob(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{}) // duration_ms = 6000, untrimmed

	r := e.doJSON("POST", "/api/clips/"+c.ID+"/poster", posterBody(2.5), withCookie(cookie))
	if r.code != 202 {
		t.Fatalf("POST poster: %d %s", r.code, r.body)
	}
	if r.str("state") != "queued" || r.json["job_id"] == nil {
		t.Fatalf("202 body = %s, want a job id and state=queued", r.body)
	}
	if got, ok := r.json["seconds"].(float64); !ok || got != 2.5 {
		t.Fatalf("202 seconds = %v, want the moment that was queued", r.json["seconds"])
	}
	if e.worker.woken.Load() == 0 {
		t.Error("the worker was not woken; the job would wait out the 5s poll")
	}
	job, err := e.db.LatestJobOfKind(context.Background(), c.ID, db.KindPoster)
	if err != nil {
		t.Fatal(err)
	}
	if job.ParamsJSON != `{"seconds":2.5}` {
		t.Errorf("params = %s", job.ParamsJSON)
	}
	// The CLIP is untouched by the request: the 202 says a job exists and
	// nothing more, so every fact in the ClipDTO is still true.
	got, _ := e.db.GetClip(context.Background(), c.ID)
	if got.State != db.ClipReady || got.PosterSeconds != nil || *got.PosterKey != *c.PosterKey {
		t.Errorf("the clip changed at enqueue time: %+v", got)
	}
	// A second POST while the first is live is the 409 the DB decides.
	r = e.doJSON("POST", "/api/clips/"+c.ID+"/poster", posterBody(1), withCookie(cookie))
	if r.code != 409 || r.errCode() != "poster_in_progress" {
		t.Fatalf("second POST = %d %s, want 409 poster_in_progress", r.code, r.body)
	}
}

// The bounds are the part of the ORIGINAL's timeline this clip's rendition
// can actually show, which for an untrimmed 6 s clip is [0, 5.9] — the last
// tenth is the edge guard, because a seek at or past the last frame makes
// ffmpeg write nothing at all.
func TestStartPosterValidation(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{}) // duration_ms = 6000

	for _, tc := range []struct {
		name string
		body any
	}{
		{"before the start", posterBody(-0.001)},
		{"far before the start", posterBody(-10)},
		{"inside the edge guard", posterBody(5.901)},
		{"past the end", posterBody(6)},
		{"far past the end", posterBody(600)},
		{"seconds missing", map[string]any{}},
		{"seconds null", map[string]any{"seconds": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := e.doJSON("POST", "/api/clips/"+c.ID+"/poster", tc.body, withCookie(cookie))
			if r.code != 400 || r.errCode() != "invalid_poster" {
				t.Fatalf("%d %s, want 400 invalid_poster", r.code, r.body)
			}
		})
	}
	// A `seconds` that is not a number at all is the generic shape error,
	// not this route's rule: nothing was measured against anything.
	if r := e.doJSON("POST", "/api/clips/"+c.ID+"/poster", map[string]any{"seconds": "2.5"}, withCookie(cookie)); r.code != 400 {
		t.Errorf("a string seconds = %d %s, want 400", r.code, r.body)
	}
	// The edges that ARE valid: the very first frame, and the last moment
	// inside the guard.
	for _, ok := range []float64{0, 5.9} {
		r := e.doJSON("POST", "/api/clips/"+c.ID+"/poster", posterBody(ok), withCookie(cookie))
		if r.code != 202 {
			t.Fatalf("POST %.3f = %d %s, want 202", ok, r.code, r.body)
		}
		e.settlePoster(c.ID)
	}
	// `-0` is one of those valid edges — IEEE says -0 == 0, so it passes the
	// range check and math.Round keeps its sign bit — and it must come back
	// as plain 0 (§20 erratum 166). The echoed number, the params_json and
	// the ffmpeg argv are one value, so `-0` in the answer would be `-0.000`
	// in the seek for the frame everybody else calls 0.
	r := e.doJSON("POST", "/api/clips/"+c.ID+"/poster", map[string]any{"seconds": math.Copysign(0, -1)}, withCookie(cookie))
	if r.code != 202 {
		t.Fatalf("POST -0 = %d %s, want 202", r.code, r.body)
	}
	if strings.Contains(string(r.body), `"seconds":-0`) {
		t.Fatalf("the 202 echoed a negative zero: %s", r.body)
	}
	if !strings.Contains(string(r.body), `"seconds":0`) {
		t.Fatalf("the 202 does not echo a plain 0: %s", r.body)
	}
	job := e.claimPoster(c.ID)
	if strings.Contains(job.ParamsJSON, "-0") {
		t.Fatalf("params_json stored a negative zero: %s", job.ParamsJSON)
	}
	if p, err := db.ParsePosterParams(job.ParamsJSON); err != nil || math.Signbit(p.Seconds) {
		t.Fatalf("stored seconds = %v (%v), want a positive zero", p.Seconds, err)
	}
}

// A poster job whose params_json cannot be parsed leaves `seconds` at 0,
// which a client cannot tell from a genuine choice of the first frame. The
// row is unreachable from the write path, so the wire is left alone and the
// operator is told instead (N049): a nullable `seconds` would change a public
// shape for a case only a hand-corrupted row can produce.
func TestPosterStatusWarnsAboutUnreadableParams(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	// Enqueued through the db API with params no parser can read — the shape
	// only a hand-corrupted row (or a restore from a damaged file) produces.
	if _, err := e.db.EnqueuePoster(context.Background(), c.ID, "not a poster choice at all", e.now()); err != nil {
		t.Fatal(err)
	}
	r := e.do("GET", "/api/clips/"+c.ID+"/poster", nil, withCookie(cookie))
	if r.code != 200 || r.str("state") != "queued" {
		t.Fatalf("status = %d %s", r.code, r.body)
	}
	if r.json["seconds"] != float64(0) {
		t.Fatalf("seconds = %v, want the zero value the wire has always carried", r.json["seconds"])
	}
	if !strings.Contains(e.logBuf.String(), "poster job parameters are unreadable") {
		t.Fatalf("the operator was not told about the corrupt row:\n%s", e.logBuf.String())
	}
}

// settlePoster resolves the clip's live poster job so the next POST is not a
// 409. Tests that are about the ROUTE do not need a worker to have run — the
// job is claimed through the real media claim and then completed, which is
// the shortest legal path from `queued` to a terminal row.
func (e *env) settlePoster(clipID string) {
	e.t.Helper()
	ctx := context.Background()
	job := e.claimPoster(clipID)
	if err := e.db.CompleteJob(ctx, job.ID, db.ClaimStamp(job), "done", "settled by the test", e.now()); err != nil {
		e.t.Fatal(err)
	}
}

// A trimmed clip's window has MOVED on the original's timeline, and the
// route moves with it: the reachable range starts where the cut starts.
// Anything before it is a moment this clip no longer shows.
func TestStartPosterBoundsFollowTheCut(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	// A trim really lands: the rendition becomes the original's 3 s-5 s,
	// through the same SwapRendition the worker commits with.
	ctx := context.Background()
	win := db.TrimParams{StartMS: 3000, EndMS: 5000}
	tjob, err := e.db.EnqueueTrim(ctx, c.ID, db.MarshalTrimParams(win), e.now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.db.ClaimMediaJob(ctx, e.now()); err != nil {
		t.Fatal(err)
	}
	src := int64(6000)
	_, ok, _, err := e.db.SwapRendition(ctx, tjob.ID, c.ID, db.ReadyFields{
		RenditionKey: "r2", RenditionSize: 9, RenditionETag: "re2",
		PosterKey: "p2", PosterSize: 9, PosterETag: "pe2", PosterWidth: 1280, PosterHeight: 720,
		Width: 1280, Height: 720, DurationMS: 2000, FPSNum: 30, FPSDen: 1, HasAudio: true,
		SourceDurationMS: &src,
	}, win, e.now())
	if err != nil || !ok {
		t.Fatalf("SwapRendition: ok=%v err=%v", ok, err)
	}
	for _, tc := range []struct {
		seconds float64
		code    int
	}{
		{2.999, 400}, // before the cut: the rendition does not hold it
		{3.0, 202},   // the cut's first frame
		{4.8, 202},   // inside
		{4.901, 400}, // inside the edge guard at the cut's end
		{5.5, 400},   // past the cut entirely
		// The LAST choosable moment, written with the same operations in the
		// same order the handler and app.js's clamp both use — so this row
		// asserts the exact double the share page sends when the player has
		// run to its end is accepted, not refused by a rounding hair.
		{float64(3000)/1000 + float64(2000-posterEdgeGuardMS)/1000, 202},
	} {
		r := e.doJSON("POST", "/api/clips/"+c.ID+"/poster", posterBody(tc.seconds), withCookie(cookie))
		if r.code != tc.code {
			t.Errorf("POST %.3f on a 3-5 s cut = %d %s, want %d", tc.seconds, r.code, r.body, tc.code)
		}
		if r.code == 202 {
			e.settlePoster(c.ID)
		}
	}
}

// TestPosterEdgeGuardIsMirroredInTheClient keeps the share page's "Use this
// frame" button inside the range this handler accepts. The button reads the
// player's clock, and a player left to run to its end reports EXACTLY the
// moment the edge guard forbids — with that frame on screen — so app.js
// clamps to the guard rather than sending a request it knows will 400. A
// constant kept in two languages is a bug waiting for the day somebody moves
// one of them, so the test reads both.
func TestPosterEdgeGuardIsMirroredInTheClient(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "static", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)
	m := regexp.MustCompile(`const posterEdgeGuardMS = (\d+);`).FindStringSubmatch(js)
	if m == nil {
		t.Fatal("app.js declares no posterEdgeGuardMS: the button can offer a moment this handler refuses")
	}
	got, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || got != posterEdgeGuardMS {
		t.Errorf("app.js posterEdgeGuardMS = %q, handler = %d", m[1], int64(posterEdgeGuardMS))
	}
	if !strings.Contains(js, "(durationMs - posterEdgeGuardMS) / 1000") {
		t.Error("app.js declares posterEdgeGuardMS but never clamps the chosen moment with it")
	}
}

// TestTheClientReadsTheSkippedStage: since §20 erratum 228 the share page decides
// "the card image was not changed" from the job's STAGE — the one field that
// tells a lost race (`skipped`, internal/worker/poster.go) from a real swap
// (`done`) — and this route's DTO is where that stage reaches it. Another string
// kept in two languages, so the test reads both ends of it.
func TestTheClientReadsTheSkippedStage(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "static", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)
	if want := `stage === "` + db.JobStageSkipped + `"`; !strings.Contains(js, want) {
		t.Errorf("app.js never tests %s: the not-changed message is back to guessing from the seconds", want)
	}
	// …and the stage it reads is the one this route puts on the wire.
	if !strings.Contains(js, "applyChosen(asked, p.applied_seconds != null ? Number(p.applied_seconds) : NaN, p.stage)") {
		t.Error("app.js does not hand the poster status's stage to applyChosen")
	}
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	if r := e.doJSON("POST", "/api/clips/"+c.ID+"/poster", posterBody(0.5), withCookie(cookie)); r.code != 202 {
		t.Fatalf("POST poster = %d %s", r.code, r.body)
	}
	if r := e.do("GET", "/api/clips/"+c.ID+"/poster", nil, withCookie(cookie)); r.code != 200 || !r.has("stage") {
		t.Errorf("GET poster = %d %s, want 200 carrying a stage field", r.code, r.body)
	}
}

// TestTheClientCorrectsTheNoteFromTheWindowToo pins the other half of erratum
// 228(a) (amended by bundle W2's fix pass): after a trim, `refreshChosen` asks
// this route first AND keeps erratum 130's window test as the fallback for every
// answer this route cannot give.
//
// The state that makes the fallback load-bearing is the one asserted below. When
// the LATEST poster job is `failed`, posterStatus fills `applied_seconds` only for
// a `done` job — so the wire says nothing about what the clip carries — while a
// failed poster job never touched the clip (internal/worker/poster.go's finish:
// the CLIP is untouched whatever happened). A trim that then drops the choice
// leaves the page asserting a card image the clip no longer has, and only the
// window can see it: the chosen moment is outside the new cut.
func TestTheClientCorrectsTheNoteFromTheWindowToo(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "static", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)
	for _, want := range []string{
		// the applied window reaches the function…
		"async function refreshChosen(pbox, page, win)",
		"await refreshChosen(pbox, page, win)",
		// …it is read on the ORIGINAL's timeline, as the choice is stored…
		"const ms = Number(pbox.dataset.posterSeconds) * 1000;",
		// …and the inconclusive branch acts on it instead of returning silently.
		"if (outsideCut()) automatic();",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js does not carry %q: a false card-image note now survives a trim whose "+
				"latest poster job is failed", want)
		}
	}

	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	ctx := context.Background()
	// Job A lands: the clip carries the frame at 1 s and the page's note is true.
	if r := e.doJSON("POST", "/api/clips/"+c.ID+"/poster", posterBody(1), withCookie(cookie)); r.code != 202 {
		t.Fatalf("POST poster A = %d %s", r.code, r.body)
	}
	jobA := e.claimPoster(c.ID)
	if swap, err := e.db.SetPoster(ctx, jobA.ID, c.ID, db.PosterFields{
		Key: "pA", Size: 9, ETag: "peA", Width: 1280, Height: 720, Seconds: 1,
	}, *c.RenditionETag, e.now()); err != nil || !swap.OK {
		t.Fatalf("SetPoster: %+v %v", swap, err)
	}
	// Job B fails: the LATEST job is now the failed one.
	if r := e.doJSON("POST", "/api/clips/"+c.ID+"/poster", posterBody(2), withCookie(cookie)); r.code != 202 {
		t.Fatalf("POST poster B = %d %s", r.code, r.body)
	}
	jobB := e.claimPoster(c.ID)
	if _, err := e.db.FailAttempt(ctx, jobB.ID, db.ClaimStamp(jobB), c.ID, "there is no picture at that moment in the clip",
		db.FailPermanent, db.Limits{}, e.now()); err != nil {
		t.Fatal(err)
	}
	r := e.do("GET", "/api/clips/"+c.ID+"/poster", nil, withCookie(cookie))
	if r.code != 200 || r.str("state") != "failed" {
		t.Fatalf("status after the failure = %d %s, want 200 state=failed", r.code, r.body)
	}
	if r.json["applied_seconds"] != nil {
		t.Errorf("a failed latest job reported applied_seconds %v: the client's status read would then be "+
			"conclusive and the window fallback pointless — check which half this test is pinning", r.json["applied_seconds"])
	}
	// …while the clip still carries job A's choice, which is what the note on the
	// page is rendered from and what a trim can silently retire.
	got, _ := e.db.GetClip(ctx, c.ID)
	if got.PosterSeconds == nil || *got.PosterSeconds != 1 {
		t.Fatalf("a failed poster job changed the clip's chosen moment: %v", got.PosterSeconds)
	}
}

func TestStartPosterNeedsAReadyClip(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	// The failed clip first: failedClip claims "the next job", so a queued
	// clip seeded before it would be the one it claimed.
	f := e.failedClip(owner, clipOpts{})
	if r := e.doJSON("POST", "/api/clips/"+f.ID+"/poster", posterBody(1), withCookie(cookie)); r.code != 409 || r.errCode() != "not_ready" {
		t.Fatalf("poster of a failed clip = %d %s, want 409 not_ready", r.code, r.body)
	}
	q := e.seedClip(owner, clipOpts{})
	if r := e.doJSON("POST", "/api/clips/"+q.ID+"/poster", posterBody(1), withCookie(cookie)); r.code != 409 || r.errCode() != "not_ready" {
		t.Fatalf("poster of a queued clip = %d %s, want 409 not_ready", r.code, r.body)
	}
}

// TestPosterAuthMatrix is the §14 owner-route contract, identical to the
// trim's: anonymous 401, a stranger 403, a capture token 403
// token_not_allowed (§20 erratum 19), an unknown clip 404 for the owner, a
// cross-origin post 403 and a non-JSON body 415.
func TestPosterAuthMatrix(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	_, otherCookie := e.newUser("other")
	c, _, _ := e.readyClip(owner, clipOpts{})
	tok := e.newToken(owner, "upload")

	for _, tc := range []struct {
		name   string
		method string
		opts   []reqOpt
		code   int
		errStr string
	}{
		{"anonymous POST", "POST", nil, 401, "unauthorized"},
		{"anonymous GET", "GET", nil, 401, "unauthorized"},
		{"stranger POST", "POST", []reqOpt{withCookie(otherCookie)}, 403, "forbidden"},
		{"stranger GET", "GET", []reqOpt{withCookie(otherCookie)}, 403, "forbidden"},
		{"token POST", "POST", []reqOpt{withBearer(tok)}, 403, "token_not_allowed"},
		{"token GET", "GET", []reqOpt{withBearer(tok)}, 403, "token_not_allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r resp
			if tc.method == "GET" {
				r = e.do("GET", "/api/clips/"+c.ID+"/poster", nil, tc.opts...)
			} else {
				r = e.doJSON("POST", "/api/clips/"+c.ID+"/poster", posterBody(1), tc.opts...)
			}
			if r.code != tc.code || r.errCode() != tc.errStr {
				t.Fatalf("%d %s, want %d %s", r.code, r.body, tc.code, tc.errStr)
			}
		})
	}
	for _, id := range []string{"nosuchclipid", "short"} {
		if r := e.doJSON("POST", "/api/clips/"+id+"/poster", posterBody(1), withCookie(cookie)); r.code != 404 {
			t.Errorf("POST poster %s = %d %s, want 404", id, r.code, r.body)
		}
		if r := e.do("GET", "/api/clips/"+id+"/poster", nil, withCookie(cookie)); r.code != 404 {
			t.Errorf("GET poster %s = %d %s, want 404", id, r.code, r.body)
		}
	}
	if r := e.doJSON("POST", "/api/clips/"+c.ID+"/poster", posterBody(1), withCookie(cookie),
		withHeader("Origin", "https://evil.example")); r.code != 403 {
		t.Errorf("cross-origin poster = %d %s, want 403", r.code, r.body)
	}
	if r := e.do("POST", "/api/clips/"+c.ID+"/poster", nil, withCookie(cookie),
		withHeader("Content-Type", "text/plain")); r.code != 415 {
		t.Errorf("non-JSON poster = %d %s, want 415", r.code, r.body)
	}
	// The PUBLIC media route beside it is untouched by any of this: a
	// stranger still gets the picture, because the link is the auth.
	if r := e.do("GET", "/api/clips/"+c.ID+"/poster.jpg", nil); r.code != 200 {
		t.Errorf("anonymous GET poster.jpg = %d, want 200: the card image is public", r.code)
	}
}

func TestPosterStatus(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})

	// Nobody has chosen a frame: 404, which a poller must never read as
	// "finished".
	r := e.do("GET", "/api/clips/"+c.ID+"/poster", nil, withCookie(cookie))
	if r.code != 404 || r.errCode() != "not_found" {
		t.Fatalf("GET before any choice = %d %s, want 404", r.code, r.body)
	}
	if r := e.doJSON("POST", "/api/clips/"+c.ID+"/poster", posterBody(2.5), withCookie(cookie)); r.code != 202 {
		t.Fatalf("POST: %d %s", r.code, r.body)
	}

	// Queued: the request is echoed, nothing is applied yet.
	r = e.do("GET", "/api/clips/"+c.ID+"/poster", nil, withCookie(cookie))
	if r.code != 200 || r.str("state") != "queued" {
		t.Fatalf("queued status = %d %s", r.code, r.body)
	}
	if r.json["seconds"] != 2.5 || r.json["applied_seconds"] != nil {
		t.Errorf("status = %s, want seconds=2.5 and applied_seconds null before the job lands", r.body)
	}
	if r.hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", r.hdr.Get("Cache-Control"))
	}

	job := e.claimPoster(c.ID)
	r = e.do("GET", "/api/clips/"+c.ID+"/poster", nil, withCookie(cookie))
	if r.code != 200 || r.str("state") != "running" {
		t.Fatalf("running status = %d %s", r.code, r.body)
	}

	// Done: applied_seconds comes from the CLIP, which is what actually
	// happened rather than what was asked for.
	ctx := context.Background()
	if _, err := e.db.SetPoster(ctx, job.ID, c.ID, db.PosterFields{
		Key: "p2", Size: 9, ETag: "pe2", Width: 1280, Height: 720, Seconds: 2.5,
	}, *c.RenditionETag, e.now()); err != nil {
		t.Fatal(err)
	}
	r = e.do("GET", "/api/clips/"+c.ID+"/poster", nil, withCookie(cookie))
	if r.code != 200 || r.str("state") != "done" || r.json["applied_seconds"] != 2.5 {
		t.Fatalf("done status = %d %s, want applied_seconds 2.5", r.code, r.body)
	}
	if r.json["error"] != nil {
		t.Errorf("a done job carries an error: %s", r.body)
	}
	// GET …/progress still reports the CLIP, which was `ready` throughout.
	// The two routes answer different questions and must not be confused.
	p := e.do("GET", "/api/clips/"+c.ID+"/progress", nil)
	if p.code != 200 || p.str("state") != "ready" {
		t.Fatalf("progress during a poster job = %d %s, want the clip's own ready", p.code, p.body)
	}
}

// A failed job reports the worker's owner-facing sentence, and only then.
func TestPosterStatusReportsAFailure(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	if r := e.doJSON("POST", "/api/clips/"+c.ID+"/poster", posterBody(1), withCookie(cookie)); r.code != 202 {
		t.Fatalf("POST: %d %s", r.code, r.body)
	}
	job := e.claimPoster(c.ID)
	ctx := context.Background()
	if _, err := e.db.FailAttempt(ctx, job.ID, db.ClaimStamp(job), c.ID, "there is no picture at that moment in the clip",
		db.FailPermanent, db.Limits{}, e.now()); err != nil {
		t.Fatal(err)
	}
	r := e.do("GET", "/api/clips/"+c.ID+"/poster", nil, withCookie(cookie))
	if r.code != 200 || r.str("state") != "failed" {
		t.Fatalf("failed status = %d %s", r.code, r.body)
	}
	if r.str("error") != "there is no picture at that moment in the clip" {
		t.Errorf("error = %q, want the owner-facing sentence", r.str("error"))
	}
	// …and the clip is still ready, still on its own poster: a card image
	// nobody could take costs the clip nothing.
	got, _ := e.db.GetClip(ctx, c.ID)
	if got.State != db.ClipReady || got.PosterSeconds != nil || *got.PosterKey != *c.PosterKey {
		t.Fatalf("a failed poster job cost the clip something: %+v", got)
	}
}
