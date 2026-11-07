package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"clippet/internal/auth"
	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/ids"
	"clippet/internal/lifecycle"
	"clippet/internal/store"
	"clippet/internal/transcode"
	"clippet/internal/urls"
	"clippet/internal/worker"
)

const testOrigin = "http://localhost:8481"

// testHost is testOrigin's hostname — the site line every card and every oEmbed
// document on this fixture's instance carries since §20 erratum 287.
const testHost = "localhost"

// fakeWorker records Wake calls and serves a scripted progress registry.
type fakeWorker struct {
	worker.Worker
	woken    atomic.Int32
	progress map[string]worker.Progress
	// trim is the SECOND registry (§17 #5): what GET …/trim reads, and
	// deliberately not what GET …/progress does.
	trim map[string]worker.Progress

	// The capture ready-wait's wake-up (§20 erratum 97). `awaited` counts
	// AwaitTerminal calls, so a test can assert a handler did NOT wait;
	// `onAwait` runs on its own goroutine the moment the handler starts
	// waiting, which is where a test scripts what the "worker" then does.
	mu       sync.Mutex
	awaited  atomic.Int32
	terminal map[string]chan struct{}
	onAwait  func(id string)
}

func newFakeWorker() *fakeWorker {
	return &fakeWorker{Worker: worker.NewNoop(), progress: map[string]worker.Progress{},
		trim: map[string]worker.Progress{}, terminal: map[string]chan struct{}{}}
}

// AwaitTerminal hands out (and remembers) one channel per clip, as the real
// registry does, and fires the test's script.
func (f *fakeWorker) AwaitTerminal(id string) (<-chan struct{}, func()) {
	f.awaited.Add(1)
	f.mu.Lock()
	ch := f.chanFor(id)
	hook := f.onAwait
	f.mu.Unlock()
	if hook != nil {
		go hook(id)
	}
	return ch, func() {}
}

// finish ends the wait for id (idempotent, like a closed registry entry).
func (f *fakeWorker) finish(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.chanFor(id):
	default:
		close(f.terminal[id])
	}
}

// chanFor returns the clip's channel, creating it. Call under f.mu.
func (f *fakeWorker) chanFor(id string) chan struct{} {
	ch, ok := f.terminal[id]
	if !ok {
		ch = make(chan struct{})
		f.terminal[id] = ch
	}
	return ch
}

// script sets what the fake worker does once a handler starts waiting.
func (f *fakeWorker) script(fn func(id string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onAwait = fn
}

func (f *fakeWorker) TrimProgress(id string) (worker.Progress, bool) {
	p, ok := f.trim[id]
	return p, ok
}

func (f *fakeWorker) Wake() { f.woken.Add(1) }

func (f *fakeWorker) Progress(id string) (worker.Progress, bool) {
	p, ok := f.progress[id]
	return p, ok
}

// Forget evicts the scripted registry entry, like the real worker's.
func (f *fakeWorker) Forget(id string) { delete(f.progress, id) }

// env is one API under test: temp DB, Fake store, scratch dir, fake worker,
// swappable Probe/Destroy/FreeBytes and a captured log.
type env struct {
	t       *testing.T
	h       http.Handler
	db      *db.DB
	dbPath  string
	fake    *store.Fake
	worker  *fakeWorker
	cfg     config.Config
	scratch string
	logBuf  *bytes.Buffer

	probe func(ctx context.Context, path string) (*transcode.ProbeResult, error)
	// strip is the audio strip (§20 erratum 328); nil is fakeStrip.
	strip   func(ctx context.Context, p *transcode.ProbeResult, in, out string) (transcode.StripTarget, error)
	free    func(dir string) (int64, error)
	destroy func(ctx context.Context, clipID string) (lifecycle.Result, error)

	// lastNow makes now() non-decreasing. The app container's clock steps
	// BACKWARDS by whole seconds under load (Docker Desktop resyncing the VM;
	// a trap this project recorded), so a clip seeded at one wall-clock read and claimed at the
	// next could carry a run_after in the claim's future — ClaimJob then
	// answers ok=false and the test dies for a reason no code path explains.
	lastNow atomic.Int64
}

// now is time.Now().Unix() that never goes backwards within one env.
func (e *env) now() int64 {
	for {
		n := time.Now().Unix()
		last := e.lastNow.Load()
		if n < last {
			return last
		}
		if e.lastNow.CompareAndSwap(last, n) {
			return n
		}
	}
}

// wallClockSites is the tripwire under now(): a test of this package that
// stamps a row or claims a job from the WALL clock reintroduces the failure
// e.now() exists to prevent, and it fails once in fifty runs at a merge gate
// rather than on the machine that wrote it. Only two files may read the wall
// clock in whole seconds, and both say why here.
var wallClockSites = map[string]string{
	"harness_test.go":      "now() itself is defined here",
	"health_queue_test.go": "it drives the database and the handler directly; there is no env to ask",
}

func TestTestsOfThisPackageClaimOnTheHarnessClock(t *testing.T) {
	paths, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) < 10 {
		t.Fatalf("globbed %d test files; the tripwire is looking in the wrong place", len(paths))
	}
	for _, p := range paths {
		if _, allowed := wallClockSites[filepath.Base(p)]; allowed {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("time.Now()"+".Unix()")) {
			t.Errorf("%s reads the wall clock in whole seconds; use e.now(), which cannot step backwards (the container clock steps back under load)", p)
		}
	}
}

// openTestDB opens and migrates a fresh database at path.
func openTestDB(t *testing.T, path string) *db.DB {
	t.Helper()
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d
}

func newEnv(t *testing.T, mutate ...func(*Deps)) *env {
	t.Helper()
	base, _ := url.Parse(testOrigin)
	dbPath := filepath.Join(t.TempDir(), "clippet.db")
	e := &env{
		t:       t,
		db:      openTestDB(t, dbPath),
		dbPath:  dbPath,
		fake:    store.NewFake(),
		worker:  newFakeWorker(),
		scratch: t.TempDir(),
		logBuf:  &bytes.Buffer{},
	}
	e.cfg = config.Config{
		PublicBaseURL:     base,
		ScratchDir:        e.scratch,
		S3:                config.S3Config{KeyPrefix: "p/"},
		MaxUploadBytes:    1 << 20,
		ScratchMinFree:    1 << 10,
		SessionTTL:        time.Hour,
		SessionCookie:     "sess",
		Registration:      config.RegistrationOpen,
		RateAuthPerMin:    100,
		RateUploadPerHour: 100,
		RateMaxKeys:       100,
		ViewDedupeTTL:     30 * time.Minute,
		ViewDedupeMax:     100,
		Transcode:         config.TranscodeConfig{FFmpegVersion: "7.1.1"},
	}
	e.probe = func(context.Context, string) (*transcode.ProbeResult, error) {
		return &transcode.ProbeResult{VideoIndex: 0}, nil
	}
	e.free = func(string) (int64, error) { return 1 << 40, nil }
	e.destroy = e.lifecycleDestroy
	d := Deps{
		Cfg: e.cfg, DB: e.db, Store: e.fake, Worker: e.worker,
		URLs:    urls.New(base, "test"),
		Log:     slog.New(slog.NewTextHandler(e.logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Version: "test", Started: time.Now(),
		Probe:     func(ctx context.Context, p string) (*transcode.ProbeResult, error) { return e.probe(ctx, p) },
		Destroy:   func(ctx context.Context, id string) (lifecycle.Result, error) { return e.destroy(ctx, id) },
		FreeBytes: func(dir string) (int64, error) { return e.free(dir) },
		Strip: func(ctx context.Context, p *transcode.ProbeResult, in, out string) (transcode.StripTarget, error) {
			if e.strip != nil {
				return e.strip(ctx, p, in, out)
			}
			return fakeStrip(ctx, p, in, out)
		},
	}
	for _, m := range mutate {
		m(&d)
	}
	e.cfg = d.Cfg
	e.h = NewHandler(d)
	return e
}

// lifecycleDestroy mirrors lifecycle.Destroy's §12 sequence on the fakes.
func (e *env) lifecycleDestroy(ctx context.Context, id string) (lifecycle.Result, error) {
	if err := e.db.MarkDeleting(ctx, id, e.now()); err != nil {
		if err == db.ErrNotFound {
			return lifecycle.Result{RowDeleted: true}, nil
		}
		return lifecycle.Result{}, err
	}
	n, err := e.fake.DeletePrefix(ctx, store.ClipPrefix(e.cfg.S3.KeyPrefix, id))
	if err != nil {
		return lifecycle.Result{Objects: n}, err
	}
	if err := e.db.DeleteClipRow(ctx, id); err != nil {
		return lifecycle.Result{Objects: n}, err
	}
	_ = os.RemoveAll(filepath.Join(e.scratch, id))
	return lifecycle.Result{Objects: n, RowDeleted: true}, nil
}

// claimTrim claims the clip's queued trim job through the real
// non-transcode claim, so a status test sees a genuinely running row.
func (e *env) claimTrim(clipID string) db.Job {
	e.t.Helper()
	j, _, ok, err := e.db.ClaimMediaJob(context.Background(), e.now())
	if err != nil || !ok || j.ClipID != clipID || j.Kind != db.KindTrim {
		e.t.Fatalf("ClaimMediaJob = %+v, %v, %v", j, ok, err)
	}
	return j
}

// --- requests ---

type resp struct {
	code int
	hdr  http.Header
	body []byte
	json map[string]any
}

func (r resp) str(key string) string {
	s, _ := r.json[key].(string)
	return s
}

func (r resp) errCode() string { return r.str("error") }

func (r resp) has(key string) bool {
	_, ok := r.json[key]
	return ok
}

type reqOpt func(*http.Request)

func withCookie(c *http.Cookie) reqOpt {
	return func(r *http.Request) {
		if c != nil {
			r.AddCookie(c)
		}
	}
}

func withBearer(tok string) reqOpt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func withHeader(k, v string) reqOpt {
	return func(r *http.Request) { r.Header.Set(k, v) }
}

func withJSON() reqOpt { return withHeader("Content-Type", "application/json") }

// withRemote overrides the client address `do` stamps on every request, which
// is what the per-IP limiters key on.
func withRemote(addr string) reqOpt {
	return func(r *http.Request) { r.RemoteAddr = addr }
}

func (e *env) do(method, path string, body io.Reader, opts ...reqOpt) resp {
	e.t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.RemoteAddr = "192.0.2.10:5555"
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	out := resp{code: rec.Code, hdr: rec.Header(), body: rec.Body.Bytes()}
	_ = json.Unmarshal(out.body, &out.json)
	return out
}

// logged runs one request through httpx.AccessLog so the test can read the
// line an operator would see — its level, and the annotations the handler
// made (SetLogField is a no-op without that middleware).
func (e *env) logged(method, path string, body io.Reader, opts ...reqOpt) (resp, string) {
	e.t.Helper()
	var logs bytes.Buffer
	h := httpx.AccessLog(slog.New(slog.NewTextHandler(&logs, nil)))(e.h)
	req := httptest.NewRequest(method, path, body)
	req.RemoteAddr = "192.0.2.10:5555"
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := resp{code: rec.Code, hdr: rec.Header(), body: rec.Body.Bytes()}
	_ = json.Unmarshal(out.body, &out.json)
	return out, logs.String()
}

func (e *env) doJSON(method, path string, v any, opts ...reqOpt) resp {
	e.t.Helper()
	var body io.Reader
	switch b := v.(type) {
	case nil:
	case string:
		body = strings.NewReader(b)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			e.t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	}
	return e.do(method, path, body, append([]reqOpt{withJSON()}, opts...)...)
}

// --- users, sessions, tokens ---

// newUser inserts a user directly (fast: no bcrypt) with a session cookie.
func (e *env) newUser(name string) (db.User, *http.Cookie) {
	e.t.Helper()
	ctx := context.Background()
	u, err := e.db.CreateUser(ctx, ids.New(), name, "$2a$12$notahash", e.now())
	if err != nil {
		e.t.Fatal(err)
	}
	raw, _, err := auth.CreateSession(ctx, e.db, e.cfg, u.ID, "test-agent", time.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	return u, &http.Cookie{Name: e.cfg.SessionCookie, Value: raw}
}

// newToken inserts an API token for u with the given scopes and returns the
// raw value.
func (e *env) newToken(u db.User, scopes ...string) string {
	e.t.Helper()
	raw := ids.Token()
	err := e.db.CreateToken(context.Background(), db.APIToken{ID: ids.New(), UserID: u.ID, Name: "t",
		TokenHash: auth.HashToken(raw), Prefix: auth.TokenPrefixOf(raw), Scopes: scopes, CreatedAt: e.now()})
	if err != nil {
		e.t.Fatal(err)
	}
	return raw
}

// sessionCookie extracts the session cookie from a response.
func (e *env) sessionCookie(r resp) *http.Cookie {
	e.t.Helper()
	res := http.Response{Header: r.hdr}
	for _, c := range res.Cookies() {
		if c.Name == e.cfg.SessionCookie {
			return c
		}
	}
	return nil
}

// --- clips ---

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func testVideo(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i*7 + 3)
	}
	return data
}

type clipOpts struct {
	title     string
	unlisted  bool
	ttl       *int64
	source    []byte
	createdAt int64  // 0 → now
	id        string // "" → ids.New()
	ext       string // "" → mp4
}

// seedClip inserts a queued clip whose original is present in the Fake.
func (e *env) seedClip(owner db.User, o clipOpts) db.Clip {
	e.t.Helper()
	id := o.id
	if id == "" {
		id = ids.New()
	}
	ext := o.ext
	if ext == "" {
		ext = "mp4"
	}
	src := o.source
	if src == nil {
		src = testVideo(1000)
	}
	keys := store.KeysFor(e.cfg.S3.KeyPrefix, id, ext)
	e.fake.Put(keys.Original, src, "video/mp4")
	title := o.title
	if title == "" {
		title = "clip " + id
	}
	now := o.createdAt
	if now == 0 {
		now = e.now()
	}
	c, _, err := e.db.CreateClipWithJob(context.Background(), db.NewClip{
		ID: id, OwnerID: owner.ID, Title: title, Description: "d", Unlisted: o.unlisted,
		SourceKey: keys.Original, SourceFilename: "orig.mp4", SourceSize: int64(len(src)),
		SourceContentType: "video/mp4", SourceETag: sha256hex(src), TTLSeconds: o.ttl, Now: now,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

// claim claims the next job and asserts it belongs to id (tests create and
// claim in order, so this never claims a stranger).
func (e *env) claim(id string) db.Job {
	e.t.Helper()
	job, clip, ok, err := e.db.ClaimJob(context.Background(), e.now())
	if err != nil || !ok {
		e.t.Fatalf("ClaimJob: ok=%v err=%v", ok, err)
	}
	if clip.ID != id {
		e.t.Fatalf("ClaimJob claimed %s, want %s", clip.ID, id)
	}
	return job
}

// finishTranscode claims the clip's queued transcode job and finalizes it,
// exactly as the worker's happy path does. Errors are RETURNED rather than
// fataled: the capture ready-wait's tests call this from another goroutine,
// where t.Fatal is not allowed.
func (e *env) finishTranscode(id string) error {
	ctx := context.Background()
	job, clip, ok, err := e.db.ClaimJob(ctx, e.now())
	if err != nil || !ok {
		return fmt.Errorf("ClaimJob: ok=%v err=%v", ok, err)
	}
	if clip.ID != id {
		return fmt.Errorf("ClaimJob claimed %s, want %s", clip.ID, id)
	}
	rendition, poster := testVideo(2048), []byte("\xff\xd8\xff\xe0jpeg")
	rk := store.RenditionKey(e.cfg.S3.KeyPrefix, id, "r8r8r8r8")
	pk := store.PosterKey(e.cfg.S3.KeyPrefix, id, "r8r8r8r8")
	e.fake.Put(rk, rendition, "video/mp4")
	e.fake.Put(pk, poster, "image/jpeg")
	ok, _, err = e.db.FinalizeTranscode(ctx, job.ID, id, db.ReadyFields{
		RenditionKey: rk, RenditionSize: int64(len(rendition)), RenditionETag: sha256hex(rendition),
		PosterKey: pk, PosterSize: int64(len(poster)), PosterETag: sha256hex(poster),
		PosterWidth: 640, PosterHeight: 360,
		Width: 1280, Height: 720, DurationMS: 6000, FPSNum: 30, FPSDen: 1, HasAudio: true,
	}, false, e.now())
	if err != nil || !ok {
		return fmt.Errorf("FinalizeTranscode: ok=%v err=%v", ok, err)
	}
	return nil
}

func (e *env) processingClip(owner db.User, o clipOpts) db.Clip {
	e.t.Helper()
	c := e.seedClip(owner, o)
	e.claim(c.ID)
	return e.getClip(c.ID)
}

// readyClip seeds a clip and finalizes it with rendition and poster objects
// (distinct bytes so the routes can be told apart).
func (e *env) readyClip(owner db.User, o clipOpts) (db.Clip, []byte, []byte) {
	e.t.Helper()
	c := e.seedClip(owner, o)
	job := e.claim(c.ID)
	rendition := testVideo(4096)
	poster := bytes.Repeat([]byte{0xff, 0xd8, 0xff, 0xe0}, 300)
	rk := store.RenditionKey(e.cfg.S3.KeyPrefix, c.ID, "r8r8r8r8")
	pk := store.PosterKey(e.cfg.S3.KeyPrefix, c.ID, "r8r8r8r8")
	e.fake.Put(rk, rendition, "video/mp4")
	e.fake.Put(pk, poster, "image/jpeg")
	sourceMS := int64(6000) // a transcode renders the whole original, so this is its length
	ok, _, err := e.db.FinalizeTranscode(context.Background(), job.ID, c.ID, db.ReadyFields{
		RenditionKey: rk, RenditionSize: int64(len(rendition)), RenditionETag: sha256hex(rendition),
		PosterKey: pk, PosterSize: int64(len(poster)), PosterETag: sha256hex(poster), PosterWidth: 640, PosterHeight: 360,
		Width: 1280, Height: 720, DurationMS: 6000, FPSNum: 30000, FPSDen: 1001, HasAudio: true,
		SourceDurationMS: &sourceMS,
	}, false, e.now())
	if err != nil || !ok {
		e.t.Fatalf("FinalizeTranscode: ok=%v err=%v", ok, err)
	}
	return e.getClip(c.ID), rendition, poster
}

func (e *env) failedClip(owner db.User, o clipOpts) db.Clip {
	e.t.Helper()
	c := e.seedClip(owner, o)
	job := e.claim(c.ID)
	if _, err := e.db.FailAttempt(context.Background(), job.ID, db.ClaimStamp(job), c.ID, "bad file", db.FailPermanent, db.Limits{}, e.now()); err != nil {
		e.t.Fatal(err)
	}
	return e.getClip(c.ID)
}

func (e *env) deletingClip(owner db.User) db.Clip {
	e.t.Helper()
	c := e.seedClip(owner, clipOpts{})
	if err := e.db.MarkDeleting(context.Background(), c.ID, e.now()); err != nil {
		e.t.Fatal(err)
	}
	return e.getClip(c.ID)
}

func (e *env) getClip(id string) db.Clip {
	e.t.Helper()
	c, err := e.db.GetClip(context.Background(), id)
	if err != nil {
		e.t.Fatalf("GetClip(%s): %v", id, err)
	}
	return c
}

// nullClipColumns sets columns of one clip row to NULL through a second
// connection to the same file (the way `admin` writes it): the only way to
// fabricate the invariant violation the media routes must report, since
// the db package exposes no such write.
func (e *env) nullClipColumns(id string, cols ...string) {
	e.t.Helper()
	raw, err := sql.Open("sqlite", "file:"+e.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		e.t.Fatal(err)
	}
	defer raw.Close()
	sets := make([]string, len(cols))
	for i, c := range cols {
		sets[i] = c + " = NULL"
	}
	if _, err := raw.ExecContext(context.Background(), "UPDATE clips SET "+strings.Join(sets, ", ")+" WHERE id = ?", id); err != nil {
		e.t.Fatal(err)
	}
}

// setClipColumn writes one column of a clip row through a second connection
// (the db package exposes no such write): used to simulate a concurrent retry
// re-keying the row mid-stream, so confirmDrift sees a live row that no longer
// references the key being streamed.
func (e *env) setClipColumn(id, col string, val any) {
	e.t.Helper()
	raw, err := sql.Open("sqlite", "file:"+e.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		e.t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(context.Background(), "UPDATE clips SET "+col+" = ? WHERE id = ?", val, id); err != nil {
		e.t.Fatal(err)
	}
}

// gets counts GetRange calls recorded by the Fake.
func (e *env) gets() int {
	n := 0
	for _, c := range e.fake.CallsSnapshot() {
		if strings.HasPrefix(c, "GetRange ") {
			n++
		}
	}
	return n
}

// --- multipart ---

type part struct {
	name, filename, contentType string
	data                        []byte
}

// dedupeOn, given to multipartBody as a part, writes nothing and tells it NOT
// to add the duplicate check's opt-out (§20 erratum 326).
//
// Every other multipart body this package's tests build carries
// `duplicate=allow` just before its first file part — the E2E harness does the
// same at every upload site — because the tests upload the same few bytes over
// and over under one account and mean a new clip each time. The tests OF the
// check pass dedupeOn.
var dedupeOn = part{name: "\x00dedupe-on"}

func multipartBody(t *testing.T, parts ...part) (io.Reader, string) {
	t.Helper()
	optOut := true
	for _, p := range parts {
		if p.name == dedupeOn.name || p.name == duplicateField {
			optOut = false
		}
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		if p.name == dedupeOn.name {
			continue
		}
		if optOut && p.filename != "" {
			optOut = false
			w, err := mw.CreateFormField(duplicateField)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte(duplicateAllow)); err != nil {
				t.Fatal(err)
			}
		}
		h := textproto.MIMEHeader{}
		if p.filename != "" {
			h.Set("Content-Disposition", `form-data; name="`+p.name+`"; filename="`+p.filename+`"`)
		} else {
			h.Set("Content-Disposition", `form-data; name="`+p.name+`"`)
		}
		if p.contentType != "" {
			h.Set("Content-Type", p.contentType)
		}
		w, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(p.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

// upload posts metadata (JSON text; "" for an empty part) then the video.
func (e *env) upload(meta string, filename string, video []byte, opts ...reqOpt) resp {
	e.t.Helper()
	body, ct := multipartBody(e.t,
		part{name: "metadata", contentType: "application/json", data: []byte(meta)},
		part{name: "video", filename: filename, contentType: "video/mp4", data: video})
	return e.do(http.MethodPost, "/api/clips", body, append([]reqOpt{withHeader("Content-Type", ct)}, opts...)...)
}
