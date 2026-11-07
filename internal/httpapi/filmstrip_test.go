package httpapi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/store"
)

// countFilmstripJobs is how many filmstrip jobs one clip has, ever — the number
// that proves the enqueue guard is what bounds this public route.
func (e *env) countFilmstripJobs(clipID string) int {
	e.t.Helper()
	jobs, err := e.db.ListJobs(context.Background(), db.JobQuery{ClipID: clipID, Kind: db.KindFilmstrip})
	if err != nil {
		e.t.Fatal(err)
	}
	return len(jobs)
}

// The §20 errata 220 and 227 route. Its answer table is the whole feature, and
// it has two halves. SERVING a sheet needs no credential at all — 200 for
// anybody, because a scrubber's thumbnails of a clip the caller may already
// watch frame by frame leak nothing, and because the URL sits inside pages that
// get cached and shared. MAKING one is the owner's session and nothing else: 202
// while one is being made, 409 when there is nothing to make one from, 503 when
// the last attempt failed — and, for every other caller, 404 with nothing
// queued.

const filmstripBytes = "\xff\xd8\xff-fake-sheet-bytes"

func filmstripKeyFor(e *env, id string) string {
	return store.FilmstripKey(e.cfg.S3.KeyPrefix, id, "f8f8f8f8")
}

// withFilmstrip gives a ready clip a published sheet through the real
// enqueue → claim → SetFilmstrip path (there is no other writer of filmstrip_*).
func (e *env) withFilmstrip(c db.Clip, key string) db.Clip {
	e.t.Helper()
	ctx := context.Background()
	job, err := e.db.EnqueueFilmstrip(ctx, c.ID, e.now())
	if errors.Is(err, db.ErrConflict) {
		// The route under test already queued one: finish THAT job rather than a
		// second, which the guard would refuse anyway.
		job, err = e.db.LatestJobOfKind(ctx, c.ID, db.KindFilmstrip)
	}
	if err != nil {
		e.t.Fatal(err)
	}
	if job.State == db.JobQueued {
		if _, _, ok, err := e.db.ClaimMediaJob(ctx, e.now()); err != nil || !ok {
			e.t.Fatalf("ClaimMediaJob: ok=%v err=%v", ok, err)
		}
	}
	e.fake.Put(key, []byte(filmstripBytes), "image/jpeg")
	swap, err := e.db.SetFilmstrip(ctx, job.ID, c.ID, db.FilmstripFields{
		Key: key, Size: int64(len(filmstripBytes)), ETag: sha256hex([]byte(filmstripBytes)),
		Tiles: 24, TileW: 160, TileH: 90, IntervalMS: 250,
	}, *c.RenditionETag, e.now())
	if err != nil || !swap.OK {
		e.t.Fatalf("SetFilmstrip: %+v %v", swap, err)
	}
	return e.getClip(c.ID)
}

// TestFilmstripEnqueuesThenServes walks the whole contract in the order a real
// caller meets it.
func TestFilmstripEnqueuesThenServes(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})

	// 1. The OWNER's first request: 202, Retry-After, and ONE job queued. The
	//    answer is credential-dependent, so it carries `private, no-store` and
	//    the Vary that says so (§20 erratum 227).
	r := e.do("GET", "/api/clips/"+c.ID+"/filmstrip.jpg", nil, withCookie(cookie))
	if r.code != 202 {
		t.Fatalf("first GET = %d %s, want 202", r.code, r.body)
	}
	if r.str("state") != "queued" {
		t.Errorf("202 body = %s, want state=queued", r.body)
	}
	if got := r.hdr.Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want 5", got)
	}
	if got := r.hdr.Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("202 Cache-Control = %q, want private, no-store", got)
	}
	if got := r.hdr.Get("Vary"); got != credentialVary {
		t.Errorf("202 Vary = %q, want %q", got, credentialVary)
	}
	if e.worker.woken.Load() == 0 {
		t.Error("the worker was not woken")
	}
	job, err := e.db.LatestJobOfKind(context.Background(), c.ID, db.KindFilmstrip)
	if err != nil || job.Kind != db.KindFilmstrip || job.State != db.JobQueued {
		t.Fatalf("filmstrip job = %+v, %v", job, err)
	}

	// 2. A SECOND request finds that job rather than making another (§20 erratum
	//    87's rule: report the winner's real state, never a flat "running").
	if r = e.do("GET", "/api/clips/"+c.ID+"/filmstrip.jpg", nil, withCookie(cookie)); r.code != 202 || r.str("state") != "queued" {
		t.Fatalf("second GET = %d %s, want 202 queued", r.code, r.body)
	}
	if n := e.countFilmstripJobs(c.ID); n != 1 {
		t.Fatalf("%d filmstrip jobs after two requests, want 1", n)
	}

	// 3. Once the sheet exists the route serves it to ANYBODY — here anonymously,
	//    with the media route's own headers and a shared-cacheable directive (the
	//    object is as public as a poster).
	key := filmstripKeyFor(e, c.ID)
	c = e.withFilmstrip(c, key)
	r = e.do("GET", "/api/clips/"+c.ID+"/filmstrip.jpg", nil)
	if r.code != 200 || string(r.body) != filmstripBytes {
		t.Fatalf("GET after the sheet landed = %d %q", r.code, r.body)
	}
	if ct := r.hdr.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	etag := r.hdr.Get("ETag")
	if etag == "" || etag != `"`+sha256hex([]byte(filmstripBytes))+`"` {
		t.Errorf("ETag = %q, want the object's sha256", etag)
	}
	if got := r.hdr.Get("Cache-Control"); got != mediaCacheControl {
		t.Errorf("Cache-Control = %q, want %q", got, mediaCacheControl)
	}
	// The served sheet is the same shared-cacheable answer WITH a session on the
	// request as without one, which is erratum 227's whole reason for resolving
	// the credential on the other arm alone: no Vary, no `private`, and nothing
	// about this response that a cache could key on a cookie.
	if o := e.do("GET", "/api/clips/"+c.ID+"/filmstrip.jpg", nil, withCookie(cookie)); o.code != 200 ||
		o.hdr.Get("Cache-Control") != mediaCacheControl || o.hdr.Get("Vary") != "" ||
		o.hdr.Get("Set-Cookie") != "" {
		t.Errorf("the owner's own GET of an existing sheet = %d %q Vary=%q Set-Cookie=%q, want the public answer",
			o.code, o.hdr.Get("Cache-Control"), o.hdr.Get("Vary"), o.hdr.Get("Set-Cookie"))
	}
	if got := r.hdr.Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q", got)
	}
	if got := r.hdr.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	// …and it goes through the same serveObject as every other media route:
	// conditional GET, ranges, HEAD.
	if r = e.do("GET", "/api/clips/"+c.ID+"/filmstrip.jpg", nil, withHeader("If-None-Match", etag)); r.code != 304 {
		t.Errorf("If-None-Match = %d, want 304", r.code)
	}
	if r = e.do("GET", "/api/clips/"+c.ID+"/filmstrip.jpg", nil, withHeader("Range", "bytes=0-3")); r.code != 206 {
		t.Errorf("Range = %d, want 206", r.code)
	}
	if r = e.do("HEAD", "/api/clips/"+c.ID+"/filmstrip.jpg", nil); r.code != 200 || len(r.body) != 0 {
		t.Errorf("HEAD = %d with %d bytes, want 200 and none", r.code, len(r.body))
	}
}

// TestFilmstripNotFoundAndNotReady: the two answers that are NOT about a sheet.
func TestFilmstripNotFoundAndNotReady(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	for _, path := range []string{
		"/api/clips/" + ids.New() + "/filmstrip.jpg", // unknown
		"/api/clips/nope/filmstrip.jpg",              // malformed id: 404 before any DB read
		"/api/clips/" + e.deletingClip(owner).ID + "/filmstrip.jpg",
	} {
		if r := e.do("GET", path, nil, withCookie(cookie)); r.code != 404 {
			t.Errorf("GET %s = %d, want 404", path, r.code)
		}
	}
	// A clip that is still encoding has nothing to sample: for its owner 409,
	// uncacheable, and no job queued for it.
	c := e.seedClip(owner, clipOpts{})
	r := e.do("GET", "/api/clips/"+c.ID+"/filmstrip.jpg", nil, withCookie(cookie))
	if r.code != 409 || r.str("error") != "not_ready" {
		t.Fatalf("a queued clip = %d %s, want 409 not_ready", r.code, r.body)
	}
	if got := r.hdr.Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("409 Cache-Control = %q, want private, no-store", got)
	}
	// …and for anyone else the same 404 every sheetless clip gives: the state of
	// a clip is nobody's business on THIS route, which is about a sheet (§20
	// erratum 227 — it used to answer 409 here to any caller).
	if a := e.do("GET", "/api/clips/"+c.ID+"/filmstrip.jpg", nil); a.code != 404 || a.str("error") != "not_found" {
		t.Errorf("an anonymous GET of a not-ready clip's sheet = %d %s, want 404 not_found", a.code, a.body)
	}
	if _, err := e.db.LatestJobOfKind(context.Background(), c.ID, db.KindFilmstrip); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("a filmstrip job was queued for a clip that is not ready: %v", err)
	}
}

// TestFilmstripIsMadeForTheOwnerAlone is §20 erratum 227's gate, the matrix in
// one place: on a ready clip with NO sheet, only the owner's own browser session
// is answered 202 and only that request enqueues a pass. Everybody else gets one
// answer — 404 — and the queue stays empty, which is the point: the pass decodes
// a whole rendition and nothing anonymous ever reads the result.
func TestFilmstripIsMadeForTheOwnerAlone(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	stranger, strangerCookie := e.newUser("stranger")
	ctx := context.Background()

	for _, listed := range []bool{true, false} {
		c, _, _ := e.readyClip(owner, clipOpts{unlisted: !listed})
		path := "/api/clips/" + c.ID + "/filmstrip.jpg"
		wokenBefore := e.worker.woken.Load()
		for _, tc := range []struct {
			name string
			opts []reqOpt
		}{
			{"anonymous", nil},
			{"another account's session", []reqOpt{withCookie(strangerCookie)}},
			{"the owner's upload token", []reqOpt{withBearer(e.newToken(owner, "upload"))}},
			{"the owner's read token", []reqOpt{withBearer(e.newToken(owner, "read"))}},
			{"a token of both scopes", []reqOpt{withBearer(e.newToken(owner, "upload", "read"))}},
			{"a stranger's read token", []reqOpt{withBearer(e.newToken(stranger, "read"))}},
			// A dead credential is not a special case: it resolves to nobody.
			{"a made-up bearer token", []reqOpt{withBearer("ck_" + strings.Repeat("z", 43))}},
		} {
			r := e.do("GET", path, nil, tc.opts...)
			if r.code != 404 || r.str("error") != "not_found" {
				t.Errorf("unlisted=%v: %s = %d %s, want 404 not_found", !listed, tc.name, r.code, r.body)
			}
			if got := r.hdr.Get("Cache-Control"); got != "private, no-store" {
				t.Errorf("unlisted=%v: %s 404 Cache-Control = %q", !listed, tc.name, got)
			}
			if got := r.hdr.Get("Vary"); got != credentialVary {
				t.Errorf("unlisted=%v: %s 404 Vary = %q, want %q", !listed, tc.name, got, credentialVary)
			}
			if n := e.countFilmstripJobs(c.ID); n != 0 {
				t.Fatalf("unlisted=%v: %s queued %d filmstrip job(s)", !listed, tc.name, n)
			}
		}
		// The refusals cost nothing at all: no job row, and not even a wake.
		if e.worker.woken.Load() != wokenBefore {
			t.Errorf("unlisted=%v: the worker was woken by a caller who may not make a sheet", !listed)
		}
		// The owner, and only now, gets the 202 and the one job.
		if r := e.do("GET", path, nil, withCookie(cookie)); r.code != 202 || r.str("state") != "queued" {
			t.Fatalf("unlisted=%v: the owner's GET = %d %s, want 202 queued", !listed, r.code, r.body)
		}
		if n := e.countFilmstripJobs(c.ID); n != 1 {
			t.Fatalf("unlisted=%v: %d filmstrip jobs after the owner asked, want 1", !listed, n)
		}
		// …and a stranger asking again while that job is live still learns
		// nothing and adds nothing.
		if r := e.do("GET", path, nil); r.code != 404 {
			t.Errorf("unlisted=%v: an anonymous GET while a job is live = %d, want 404", !listed, r.code)
		}
		if n := e.countFilmstripJobs(c.ID); n != 1 {
			t.Errorf("unlisted=%v: %d filmstrip jobs, want 1", !listed, n)
		}
		// Once the sheet exists every one of those callers is served it: the URL
		// may be in a page a cache handed them, and the gate is about MAKING.
		c = e.withFilmstrip(c, filmstripKeyFor(e, c.ID))
		for _, opts := range [][]reqOpt{nil, {withCookie(strangerCookie)}, {withBearer(e.newToken(stranger, "read"))}} {
			if r := e.do("GET", path, nil, opts...); r.code != 200 {
				t.Errorf("unlisted=%v: a sheet that exists = %d, want 200 for everyone", !listed, r.code)
			}
		}
		if _, err := e.db.LatestJobOfKind(ctx, c.ID, db.KindFilmstrip); err != nil {
			t.Errorf("unlisted=%v: the filmstrip job went missing: %v", !listed, err)
		}
	}
}

// TestFilmstripFailureIsReportedNotRetried: a doomed pass is not re-run every
// time the control is opened. The 503 names the reason the WORKER wrote —
// redacted, no path, no bucket — and never an operator's sentence (§20 erratum
// 165).
func TestFilmstripFailureIsReportedNotRetried(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	ctx := context.Background()

	if r := e.do("GET", "/api/clips/"+c.ID+"/filmstrip.jpg", nil, withCookie(cookie)); r.code != 202 {
		t.Fatalf("first GET = %d", r.code)
	}
	job, _, ok, err := e.db.ClaimMediaJob(ctx, e.now())
	if err != nil || !ok || job.Kind != db.KindFilmstrip {
		t.Fatalf("ClaimMediaJob: %+v ok=%v err=%v", job, ok, err)
	}
	if _, err := e.db.FailAttempt(ctx, job.ID, db.ClaimStamp(job), c.ID, "filmstrip: no thumbnails could be taken from this clip",
		db.FailPermanent, db.Limits{MaxAttempts: 1, MaxTransient: 1}, e.now()); err != nil {
		t.Fatal(err)
	}
	r := e.do("GET", "/api/clips/"+c.ID+"/filmstrip.jpg", nil, withCookie(cookie))
	if r.code != 503 || r.str("error") != "filmstrip_failed" {
		t.Fatalf("after a failed job = %d %s, want 503 filmstrip_failed", r.code, r.body)
	}
	if !strings.Contains(r.str("message"), "no thumbnails") {
		t.Errorf("503 message = %q, want the worker's own reason", r.str("message"))
	}
	if got := r.hdr.Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("503 Cache-Control = %q, want private, no-store", got)
	}
	// A caller who is not the owner is told nothing about the failure — the 404
	// every sheetless clip gives them (§20 erratum 227).
	if a := e.do("GET", "/api/clips/"+c.ID+"/filmstrip.jpg", nil); a.code != 404 {
		t.Errorf("an anonymous GET after a failed job = %d, want 404", a.code)
	}
	// No second job: the route reports, it does not retry.
	if n := e.countFilmstripJobs(c.ID); n != 1 {
		t.Errorf("%d filmstrip jobs after a failure, want 1", n)
	}
}

// TestFilmstripFailureNeverSpeaksTheOperatorsSentence is §20 erratum 165 applied
// to one more 503: the two texts internal/db writes when it repairs a job nobody
// is running are an OPERATOR's words, and this route hands last_error to the
// clip's owner.
func TestFilmstripFailureNeverSpeaksTheOperatorsSentence(t *testing.T) {
	for _, operator := range []string{db.MsgInterruptedByRestart, db.UnstickReason} {
		got := filmstripFailureMessage(db.Job{Kind: db.KindFilmstrip, LastError: &operator})
		if strings.Contains(got, operator) {
			t.Errorf("the 503 for %q says %q", operator, got)
		}
		if got == "" {
			t.Errorf("the 503 for %q says nothing at all", operator)
		}
	}
	// A run's OWN reason survives, because that is what the reader can act on.
	mine := "filmstrip: no thumbnails could be taken from this clip"
	if got := filmstripFailureMessage(db.Job{Kind: db.KindFilmstrip, LastError: &mine}); !strings.Contains(got, "no thumbnails") {
		t.Errorf("the 503 dropped the run's own reason: %q", got)
	}
	// And a job with nothing to say still gets a sentence.
	if got := filmstripFailureMessage(db.Job{Kind: db.KindFilmstrip}); got == "" {
		t.Error("a job with no last_error produced an empty message")
	}
}

// TestFilmstripIsPublicAndTakesNoOtherMethod: a sheet that EXISTS is
// `poster.jpg`'s neighbour in every respect that matters — no session, no CSRF,
// GET and HEAD only, and never a route that mutates anything but its own job
// queue. (What the gate of §20 erratum 227 covers is the making of one, which
// this clip has already had: TestFilmstripIsMadeForTheOwnerAlone.)
func TestFilmstripIsPublicAndTakesNoOtherMethod(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	key := filmstripKeyFor(e, c.ID)
	c = e.withFilmstrip(c, key)

	// Anonymous, another user's session and a capture token all get the sheet:
	// the link is the auth.
	_, otherCookie := e.newUser("stranger")
	for _, tc := range []struct {
		name string
		opts []reqOpt
	}{
		{"anonymous", nil},
		{"another user's session", []reqOpt{withCookie(otherCookie)}},
		{"the owner's own session", []reqOpt{withCookie(cookie)}},
	} {
		if r := e.do("GET", "/api/clips/"+c.ID+"/filmstrip.jpg", nil, tc.opts...); r.code != 200 {
			t.Errorf("%s = %d, want 200", tc.name, r.code)
		}
	}
	// An UNLISTED clip is no different: unlisted is about the library listing,
	// not about the link (§8.2).
	u, _, _ := e.readyClip(owner, clipOpts{unlisted: true})
	u = e.withFilmstrip(u, filmstripKeyFor(e, u.ID))
	if r := e.do("GET", "/api/clips/"+u.ID+"/filmstrip.jpg", nil); r.code != 200 {
		t.Errorf("an unlisted clip's sheet = %d, want 200", r.code)
	}
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if r := e.do(m, "/api/clips/"+c.ID+"/filmstrip.jpg", nil, withCookie(cookie)); r.code != 405 {
			t.Errorf("%s = %d, want 405", m, r.code)
		}
	}
}

// TestTheClientBustsTheSheetAfterATrim is the pair of facts that only bite each
// other: the ADDRESS of a sheet is stable for the life of the clip while the BYTES
// behind it are per rendition, and a served sheet is `public, max-age=3600`. So a
// trim that lands under an open page would otherwise be followed by the browser
// repainting the retired rendition's frames — the strip and both handle tiles —
// out of its own cache for up to an hour, which is exactly what `resetFilm` exists
// to prevent. `bustPlayer` and `bustPoster` already do this to the video and the
// poster; the sheet is the third artefact with the same shape (§20 erratum 220's
// client paragraph, amended by bundle W2's fix pass).
//
// Both halves are asserted here so a later change to either one fails: the
// directive on the wire, and the bust in the shipped client.
func TestTheClientBustsTheSheetAfterATrim(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	c = e.withFilmstrip(c, filmstripKeyFor(e, c.ID))
	r := e.do("GET", "/api/clips/"+c.ID+"/filmstrip.jpg", nil)
	if r.code != 200 || r.hdr.Get("Cache-Control") != mediaCacheControl {
		t.Fatalf("served sheet = %d %q, want 200 %q", r.code, r.hdr.Get("Cache-Control"), mediaCacheControl)
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "static", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)
	for _, want := range []string{
		// one nonce, so the strip and both tiles ask for ONE address…
		"return { url: d.filmstripUrl + filmBust,",
		// …refreshed by the only event that retires a sheet (bustQuery is the
		// client's one spelling of that nonce)…
		`filmBust = bustQuery();`,
		// …and empty until then, so an ordinary page load of a clip that already
		// has a sheet still comes out of the cache, which is what the directive
		// above is for.
		`let filmBust = "";`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js does not carry %q: after a trim the owner repaints the superseded sheet "+
				"out of the HTTP cache", want)
		}
	}
	// The bust belongs to resetFilm and nowhere else: a nonce minted on every
	// paint would defeat the caching this route is built around.
	reset := strings.Index(js, "function resetFilm()")
	bust := strings.Index(js, `filmBust = bustQuery();`)
	if reset < 0 || bust < reset || bust > reset+400 {
		t.Errorf("the filmstrip bust is not inside resetFilm (resetFilm at %d, the bust at %d): a nonce "+
			"minted anywhere else would either be stale or defeat the cache", reset, bust)
	}
}

// TestClipDTOCarriesTheFilmstripGeometry: the DTO says a sheet exists and how to
// read it, or says nothing at all — a client cannot place one tile from a URL
// alone, and half a layout would place them wrongly.
func TestClipDTOCarriesTheFilmstripGeometry(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})

	r := e.do("GET", "/api/clips/"+c.ID, nil, withCookie(cookie))
	if r.code != 200 {
		t.Fatalf("GET clip = %d", r.code)
	}
	if got := r.json["filmstrip"]; got != nil {
		t.Fatalf("filmstrip = %v before one exists, want null", got)
	}
	c = e.withFilmstrip(c, filmstripKeyFor(e, c.ID))
	r = e.do("GET", "/api/clips/"+c.ID, nil, withCookie(cookie))
	fs, ok := r.json["filmstrip"].(map[string]any)
	if !ok {
		t.Fatalf("filmstrip = %v, want an object", r.json["filmstrip"])
	}
	if url, _ := fs["url"].(string); !strings.HasSuffix(url, "/api/clips/"+c.ID+"/filmstrip.jpg") {
		t.Errorf("filmstrip.url = %v", fs["url"])
	}
	for key, want := range map[string]float64{"tiles": 24, "tile_width": 160, "tile_height": 90, "interval_ms": 250} {
		if got, _ := fs[key].(float64); got != want {
			t.Errorf("filmstrip.%s = %v, want %v", key, fs[key], want)
		}
	}
	// …and it is NOT owner-private: the route it describes is public, and the
	// geometry says nothing the video does not.
	r = e.do("GET", "/api/clips/"+c.ID, nil)
	if _, ok := r.json["filmstrip"].(map[string]any); !ok {
		t.Errorf("an anonymous caller sees no filmstrip: %s", r.body)
	}
}
