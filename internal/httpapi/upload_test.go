package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/store"
	"clippet/internal/transcode"
	"clippet/internal/worker"
)

func TestUploadHappyPath(t *testing.T) {
	e := newEnv(t)
	u, cookie := e.newUser("carol")
	video := testVideo(50_000)

	r := e.upload(`{"title":"My clip","description":"desc","unlisted":true,"ttl_days":7}`, "C:\\Videos\\game clip.mp4", video, withCookie(cookie))
	if r.code != 201 {
		t.Fatalf("upload: %d %s", r.code, r.body)
	}
	id := r.str("id")
	if r.str("state") != "queued" || r.str("title") != "My clip" || r.str("description") != "desc" || r.json["unlisted"] != true {
		t.Fatalf("dto: %s", r.body)
	}
	// The uploader is the owner: views is present (0).
	if v, ok := r.json["views"]; !ok || v != float64(0) {
		t.Fatalf("views missing on the owner's 201: %s", r.body)
	}
	if r.json["ttl_days"] != float64(7) || r.json["expires_at"] == nil || r.json["ready_at"] != nil || r.json["poster"] != nil {
		t.Fatalf("ttl/expiry: %s", r.body)
	}
	src := r.json["source"].(map[string]any)
	if src["filename"] != "game clip.mp4" || src["size"] != float64(len(video)) || src["content_type"] != "video/mp4" {
		t.Fatalf("source: %v", src)
	}
	owner := r.json["owner"].(map[string]any)
	if owner["id"] != u.ID || owner["username"] != "carol" {
		t.Fatalf("owner: %v", owner)
	}
	us := r.json["urls"].(map[string]any)
	if us["page"] != testOrigin+"/"+id || us["video"] != testOrigin+"/api/clips/"+id+"/video.mp4" ||
		us["social"] != testOrigin+"/api/clips/"+id+"/social.mp4" || us["poster"] != testOrigin+"/api/clips/"+id+"/poster.jpg" ||
		us["progress"] != testOrigin+"/api/clips/"+id+"/progress" || us["embed"] != testOrigin+"/embed/"+id {
		t.Fatalf("urls: %v", us)
	}
	// The original is in the bucket under the right key; source_etag is the
	// Fake's sha256; the scratch source is retained for the worker.
	key := store.KeysFor("p/", id, "mp4").Original
	if got, ok := e.fake.Get(key); !ok || string(got) != string(video) {
		t.Fatalf("original not stored under %s", key)
	}
	c := e.getClip(id)
	if c.SourceETag != sha256hex(video) || c.SourceKey != key || c.SourceSize != int64(len(video)) {
		t.Fatalf("row: %+v", c)
	}
	if data, err := os.ReadFile(filepath.Join(e.scratch, id, "source.mp4")); err != nil || string(data) != string(video) {
		t.Fatalf("scratch source: %v", err)
	}
	if e.worker.woken.Load() != 1 {
		t.Fatalf("worker woken %d times", e.worker.woken.Load())
	}
	job, err := e.db.GetLatestJob(context.Background(), id)
	if err != nil || job.State != db.JobQueued || job.Kind != db.KindTranscode {
		t.Fatalf("job: %+v %v", job, err)
	}
	// Expiry is created_at + 7 days.
	if c.ExpiresAt == nil || *c.ExpiresAt != c.CreatedAt+7*86400 {
		t.Fatalf("expires_at = %v", c.ExpiresAt)
	}
	// Bearer token with the upload scope works too; title defaults to the
	// filename sans extension.
	tok := e.newToken(u, "upload")
	r = e.upload(``, "holiday.MOV", testVideo(10), withBearer(tok))
	if r.code != 201 || r.str("title") != "holiday" || r.json["source"].(map[string]any)["content_type"] != "video/mp4" {
		t.Fatalf("token upload: %d %s", r.code, r.body)
	}
	if c := e.getClip(r.str("id")); !strings.HasSuffix(c.SourceKey, "/original/source.mov") {
		t.Fatalf("ext: %s", c.SourceKey)
	}
}

func TestUploadTTLWireTable(t *testing.T) {
	def := int64(30 * 86400)
	e := newEnv(t, func(d *Deps) { d.Cfg.DefaultTTLSeconds = &def })
	_, cookie := e.newUser("carol")
	tests := []struct {
		name string
		meta string
		code int
		days float64
		err  string
	}{
		{"absent → default", `{"title":"t"}`, 201, 30, ""},
		{"empty metadata → default", ``, 201, 30, ""},
		{"null → default", `{"ttl_days":null}`, 201, 30, ""},
		{"0 → never", `{"ttl_days":0}`, 201, 0, ""},
		{"7", `{"ttl_days":7}`, 201, 7, ""},
		{"5 → 400", `{"ttl_days":5}`, 400, 0, "invalid_ttl"},
		{"negative → 400", `{"ttl_days":-1}`, 400, 0, "invalid_ttl"},
		{"3.5 → 400", `{"ttl_days":3.5}`, 400, 0, "invalid_ttl"},
		{"string → 400", `{"ttl_days":"7"}`, 400, 0, "invalid_ttl"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := e.upload(tc.meta, "a.mp4", testVideo(10), withCookie(cookie))
			if r.code != tc.code || r.errCode() != tc.err {
				t.Fatalf("got %d %s, want %d %s", r.code, r.body, tc.code, tc.err)
			}
			if tc.code != 201 {
				return
			}
			if r.json["ttl_days"] != tc.days {
				t.Fatalf("ttl_days = %v, want %v", r.json["ttl_days"], tc.days)
			}
			if (tc.days == 0) != (r.json["expires_at"] == nil) {
				t.Fatalf("expires_at = %v for ttl %v", r.json["expires_at"], tc.days)
			}
		})
	}
}

func TestUploadRejections(t *testing.T) {
	e := newEnv(t)
	u, cookie := e.newUser("carol")
	video := testVideo(1000)

	t.Run("anonymous", func(t *testing.T) {
		if r := e.upload(`{}`, "a.mp4", video); r.code != 401 {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	t.Run("token without upload scope", func(t *testing.T) {
		tok := e.newToken(u, "read")
		if r := e.upload(`{}`, "a.mp4", video, withBearer(tok)); r.code != 403 || r.errCode() != "insufficient_scope" {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	t.Run("csrf", func(t *testing.T) {
		if r := e.upload(`{}`, "a.mp4", video, withCookie(cookie), withHeader("Origin", "https://evil.example")); r.code != 403 || r.errCode() != "csrf" {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	t.Run("not multipart", func(t *testing.T) {
		r := e.do("POST", "/api/clips", strings.NewReader("{}"), withCookie(cookie), withJSON())
		if r.code != 415 || r.errCode() != "not_multipart" {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	t.Run("file first", func(t *testing.T) {
		body, ct := multipartBody(t,
			part{name: "video", filename: "a.mp4", contentType: "video/mp4", data: video},
			part{name: "metadata", contentType: "application/json", data: []byte(`{}`)})
		r := e.do("POST", "/api/clips", body, withCookie(cookie), withHeader("Content-Type", ct))
		if r.code != 400 || r.errCode() != "metadata_first" {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	t.Run("no parts", func(t *testing.T) {
		body, ct := multipartBody(t)
		r := e.do("POST", "/api/clips", body, withCookie(cookie), withHeader("Content-Type", ct))
		if r.code != 400 || r.errCode() != "metadata_first" {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	t.Run("missing video", func(t *testing.T) {
		body, ct := multipartBody(t, part{name: "metadata", contentType: "application/json", data: []byte(`{}`)})
		r := e.do("POST", "/api/clips", body, withCookie(cookie), withHeader("Content-Type", ct))
		if r.code != 400 || r.errCode() != "missing_video" {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	t.Run("video without filename", func(t *testing.T) {
		body, ct := multipartBody(t,
			part{name: "metadata", contentType: "application/json", data: []byte(`{}`)},
			part{name: "video", contentType: "video/mp4", data: video})
		r := e.do("POST", "/api/clips", body, withCookie(cookie), withHeader("Content-Type", ct))
		if r.code != 400 || r.errCode() != "missing_video" {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	t.Run("empty video", func(t *testing.T) {
		if r := e.upload(`{}`, "a.mp4", nil, withCookie(cookie)); r.code != 400 || r.errCode() != "empty_video" {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	t.Run("bad metadata json", func(t *testing.T) {
		if r := e.upload(`{not json`, "a.mp4", video, withCookie(cookie)); r.code != 400 || r.errCode() != "invalid_request" {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	t.Run("title too long", func(t *testing.T) {
		if r := e.upload(`{"title":"`+strings.Repeat("x", 201)+`"}`, "a.mp4", video, withCookie(cookie)); r.code != 400 {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	t.Run("metadata too large", func(t *testing.T) {
		if r := e.upload(`{"description":"`+strings.Repeat("x", 17<<10)+`"}`, "a.mp4", video, withCookie(cookie)); r.code != 400 {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	t.Run("not a video", func(t *testing.T) {
		e.probe = func(context.Context, string) (*transcode.ProbeResult, error) {
			return nil, &transcode.ErrPermanent{Err: errors.New("no video stream")}
		}
		defer func() {
			e.probe = func(context.Context, string) (*transcode.ProbeResult, error) { return &transcode.ProbeResult{}, nil }
		}()
		before := len(e.fake.CallsSnapshot())
		dirs := scratchDirs(t, e.scratch)
		r := e.upload(`{}`, "a.txt", video, withCookie(cookie))
		if r.code != 415 || r.errCode() != "not_a_video" {
			t.Fatalf("%d %s", r.code, r.body)
		}
		if len(e.fake.CallsSnapshot()) != before {
			t.Fatalf("storage touched for a rejected file: %v", e.fake.CallsSnapshot()[before:])
		}
		assertScratchUnchanged(t, e.scratch, dirs)
	})
	t.Run("insufficient scratch", func(t *testing.T) {
		e.free = func(string) (int64, error) { return 500, nil }
		defer func() { e.free = func(string) (int64, error) { return 1 << 40, nil } }()
		r := e.upload(`{}`, "a.mp4", video, withCookie(cookie))
		if r.code != 507 || r.errCode() != "insufficient_scratch" {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	t.Run("statfs failure does not block", func(t *testing.T) {
		e.free = func(string) (int64, error) { return 0, errors.New("statfs: boom") }
		defer func() { e.free = func(string) (int64, error) { return 1 << 40, nil } }()
		if r := e.upload(`{}`, "a.mp4", video, withCookie(cookie)); r.code != 201 {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	t.Run("too large streamed", func(t *testing.T) {
		dirs := scratchDirs(t, e.scratch)
		big := testVideo(int(e.cfg.MaxUploadBytes) + 2<<20)
		r := e.upload(`{}`, "a.mp4", big, withCookie(cookie), func(req *http.Request) { req.ContentLength = -1 })
		if r.code != 413 || r.errCode() != "too_large" {
			t.Fatalf("%d %s", r.code, r.body)
		}
		assertScratchUnchanged(t, e.scratch, dirs)
	})
	t.Run("too large by content length", func(t *testing.T) {
		big := testVideo(int(e.cfg.MaxUploadBytes) + 2<<20)
		if r := e.upload(`{}`, "a.mp4", big, withCookie(cookie)); r.code != 413 || r.errCode() != "too_large" {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	t.Run("storage failure", func(t *testing.T) {
		dirs := scratchDirs(t, e.scratch)
		e.fake.FailNext("PutFile", errors.New("dial tcp: refused"))
		r := e.upload(`{}`, "a.mp4", video, withCookie(cookie))
		if r.code != 503 || r.errCode() != "storage_unavailable" || r.hdr.Get("Retry-After") == "" {
			t.Fatalf("%d %s", r.code, r.body)
		}
		calls := e.fake.CallsSnapshot()
		if last := calls[len(calls)-1]; !strings.HasPrefix(last, "DeletePrefix p/clips/") {
			t.Fatalf("prefix not cleaned up: %v", calls)
		}
		assertScratchUnchanged(t, e.scratch, dirs)
	})
	t.Run("closed db is 503 unavailable", func(t *testing.T) {
		e2 := newEnv(t)
		_, cookie2 := e2.newUser("carol")
		_ = e2.db.Close()
		r := e2.upload(`{}`, "a.mp4", video, withCookie(cookie2))
		if r.code != 503 || r.errCode() != "unavailable" || r.hdr.Get("Retry-After") != "5" {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	// Clean state afterwards: the rejected uploads left no clips.
	clips, _, err := e.db.ListClips(context.Background(), db.ListQuery{ViewerID: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(clips) != 1 { // only the statfs-failure upload succeeded
		t.Fatalf("clips after rejections: %d", len(clips))
	}
}

// scratchDirs lists the per-clip scratch directories (successful uploads
// keep theirs for the worker; rejected ones must leave none behind).
func scratchDirs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// assertScratchUnchanged fails when a rejected upload left a scratch dir.
func assertScratchUnchanged(t *testing.T, dir string, before []string) {
	t.Helper()
	after := scratchDirs(t, dir)
	if strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("scratch changed by a rejected upload: %v → %v", before, after)
	}
}

// Ingest and the worker must agree about scratch (§20 erratum 90): an upload
// the API admits must not be one the worker then refuses. The two sides are
// asked here as FUNCTIONS, so the test fails if either formula drifts.
//
// Ingest admits at free ≥ 1.5×Content-Length + SCRATCH_MIN_FREE_BYTES and then
// spools the body, so the worker sees free − Content-Length; the source is on
// scratch by then, and all acquire may require of it is the floor. The
// property is monotone in free — more free space never turns an admission into
// a refusal — so the boundary free figure is the only one worth checking.
func TestIngestAndWorkerAgreeAboutScratch(t *testing.T) {
	for _, minFree := range []int64{0, 1 << 10, 512 << 20} {
		for _, contentLength := range []int64{1, 1 << 10, 1 << 20, 100 << 20, 4 << 30} {
			free := declaredScratchNeed(contentLength, minFree)
			afterSpool := free - contentLength
			// The video part is at most the whole body and at least nothing;
			// both extremes must clear the worker's rule.
			for _, sourceSize := range []int64{0, contentLength / 2, contentLength} {
				need := worker.AcquireScratchNeed(sourceSize, minFree, true)
				if afterSpool < need {
					t.Fatalf("ingest admits Content-Length %d at %d bytes free, leaving %d, but the worker demands %d for a %d-byte source already on scratch",
						contentLength, free, afterSpool, need, sourceSize)
				}
			}
		}
	}
}

// The same parity, end to end: an upload admitted at exactly the free figure
// the precheck asks for leaves the worker's requirement satisfied, and one
// byte less is refused. This half fails if beginUpload stops consulting the
// precheck at all.
func TestUploadAdmittedAtTheBoundaryLeavesTheWorkerRoom(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")
	video := testVideo(40_000)
	body, ct := multipartBody(t,
		part{name: "metadata", contentType: "application/json", data: []byte(`{}`)},
		part{name: "video", filename: "a.mp4", contentType: "video/mp4", data: video})
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	n := int64(len(raw))
	post := func(free int64) resp {
		e.free = func(string) (int64, error) { return free, nil }
		return e.do(http.MethodPost, "/api/clips", bytes.NewReader(raw),
			withHeader("Content-Type", ct), withCookie(cookie))
	}
	need := declaredScratchNeed(n, e.cfg.ScratchMinFree)
	if r := post(need - 1); r.code != 507 || r.errCode() != "insufficient_scratch" {
		t.Fatalf("one byte under the precheck: %d %s", r.code, r.body)
	}
	r := post(need)
	if r.code != 201 {
		t.Fatalf("at the precheck's own figure: %d %s", r.code, r.body)
	}
	// What the worker will find when it picks this clip up.
	left := need - n
	if want := worker.AcquireScratchNeed(int64(len(video)), e.cfg.ScratchMinFree, true); left < want {
		t.Fatalf("the admitted upload leaves %d bytes free; acquire wants %d", left, want)
	}
}

// Both pre-flight gates used to be inside `if r.ContentLength > 0`, and
// net/http reports ContentLength = −1 for `Transfer-Encoding: chunked` — so a
// client that simply omitted Content-Length skipped SCRATCH_MIN_FREE_BYTES,
// the operator's one lever over the transcode disk, entirely (§20 erratum 90).
// A body with no declared length is now weighed against a conservative need,
// and the working path is untouched.
func TestChunkedUploadIsGatedLikeAnyOther(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")
	video := testVideo(4096)
	chunked := func(r *http.Request) { r.ContentLength = -1 }
	need := chunkedScratchNeed(e.cfg.MaxUploadBytes, e.cfg.ScratchMinFree)

	dirs := scratchDirs(t, e.scratch)
	e.free = func(string) (int64, error) { return need - 1, nil }
	r, logs := e.logged(http.MethodPost, "/api/clips", nil, withCookie(cookie), chunked,
		withHeader("Content-Type", "multipart/form-data; boundary=x"))
	if r.code != 507 || r.errCode() != "insufficient_scratch" {
		t.Fatalf("chunked body under the floor: %d %s", r.code, r.body)
	}
	if !strings.Contains(logs, "reject=insufficient_scratch") {
		t.Fatalf("the rejection is not in the access log: %s", logs)
	}
	assertScratchUnchanged(t, e.scratch, dirs)

	e.free = func(string) (int64, error) { return need, nil }
	if r := e.upload(`{}`, "a.mp4", video, withCookie(cookie), chunked); r.code != 201 {
		t.Fatalf("a chunked upload with room must still work: %d %s", r.code, r.body)
	}
}

// A playlist is not a video (§8.3 step 6, §20 erratum 91). The uploader
// picks the spool extension, so a part named evil.m3u8 is spooled as
// source.m3u8 and ffprobe demuxes it as HLS — a text file naming further
// resources, whose "video stream" is somebody else's. The container
// allowlist in transcode.ParseProbe refuses it with the ordinary permanent
// verdict, so the wire answer is the 415 not_a_video the gate already
// speaks and nothing reaches the bucket.
func TestUploadPlaylistIsNotAVideo(t *testing.T) {
	e := newEnv(t)
	u, cookie := e.newUser("carol")
	playlist := []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:3\n#EXTINF:2.000000,\n" +
		"file:../victim/source.mp4\n#EXT-X-ENDLIST\n")

	var probed string
	// The real ParseProbe over the document ffprobe answers with for a
	// playlist that resolved: streams and all, format_name "hls".
	e.probe = func(_ context.Context, path string) (*transcode.ProbeResult, error) {
		probed = path
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if !bytes.HasPrefix(raw, []byte("#EXTM3U")) {
			return &transcode.ProbeResult{}, nil
		}
		return transcode.ParseProbe([]byte(`{"streams":[{"index":0,"codec_type":"video","codec_name":"h264",
			"width":1280,"height":720,"avg_frame_rate":"30/1","duration":"2.000000"}],
			"format":{"format_name":"hls","duration":"2.000000"}}`))
	}

	before := len(e.fake.CallsSnapshot())
	dirs := scratchDirs(t, e.scratch)
	r := e.upload(`{}`, "evil.m3u8", playlist, withCookie(cookie))
	if r.code != 415 || r.errCode() != "not_a_video" {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if !strings.HasSuffix(probed, ".m3u8") {
		t.Fatalf("the spool extension is the uploader's to choose, so the gate must be what refuses it: probed %q", probed)
	}
	if len(e.fake.CallsSnapshot()) != before {
		t.Fatalf("storage touched for a playlist: %v", e.fake.CallsSnapshot()[before:])
	}
	assertScratchUnchanged(t, e.scratch, dirs)
	clips, _, err := e.db.ListClips(context.Background(), db.ListQuery{ViewerID: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(clips) != 0 {
		t.Fatalf("a playlist became %d clip(s)", len(clips))
	}
}

// probeGate applies the §11 taxonomy like its neighbouring steps: only a
// file-at-fault error (transcode.ErrPermanent) is 415 not_a_video; a
// transient/environmental probe failure (ffprobe exec trouble, the probe
// budget) is 503 with a WARN — never a permanent verdict on a good file.
func TestUploadProbeTransientFailureIs503(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")
	e.probe = func(context.Context, string) (*transcode.ProbeResult, error) {
		return nil, errors.New("ffprobe: fork/exec: resource temporarily unavailable")
	}
	before := len(e.fake.CallsSnapshot())
	dirs := scratchDirs(t, e.scratch)
	r := e.upload(`{}`, "a.mp4", testVideo(100), withCookie(cookie))
	if r.code != 503 || r.errCode() != "unavailable" || r.hdr.Get("Retry-After") == "" {
		t.Fatalf("transient probe failure: %d %s Retry-After=%q", r.code, r.body, r.hdr.Get("Retry-After"))
	}
	if len(e.fake.CallsSnapshot()) != before {
		t.Fatalf("storage touched: %v", e.fake.CallsSnapshot()[before:])
	}
	assertScratchUnchanged(t, e.scratch, dirs)
	if !strings.Contains(e.logBuf.String(), "level=WARN") {
		t.Fatalf("environmental probe failure not WARN: %s", e.logBuf.String())
	}
}

func TestUploadRateLimit(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Cfg.RateUploadPerHour = 1 })
	_, cookie := e.newUser("carol")
	if r := e.upload(`{}`, "a.mp4", testVideo(10), withCookie(cookie)); r.code != 201 {
		t.Fatalf("first: %d %s", r.code, r.body)
	}
	r := e.upload(`{}`, "a.mp4", testVideo(10), withCookie(cookie))
	if r.code != 429 || r.errCode() != "rate_limited" || r.hdr.Get("Retry-After") == "" {
		t.Fatalf("second: %d %s Retry-After=%q", r.code, r.body, r.hdr.Get("Retry-After"))
	}
	// Keyed on the user: a second account from the SAME address still gets
	// its own budget (the IP bucket beside it is far looser and untouched).
	_, other := e.newUser("other")
	if r := e.upload(`{}`, "a.mp4", testVideo(10), withCookie(other)); r.code != 201 {
		t.Fatalf("other user: %d %s", r.code, r.body)
	}
}

// TestUploadIPRateLimit is the other half of the pair (§20 erratum 97). The
// per-user budget bounds an ACCOUNT and accounts are free — register is held
// only by the auth limiter at 10/min/IP, so one address can mint hundreds an
// hour, each with its own 60 uploads. An upload must satisfy BOTH buckets,
// and this walks the whole default shape: 60 per user, 120 per address.
func TestUploadIPRateLimit(t *testing.T) {
	e := newEnv(t, func(d *Deps) {
		d.Cfg.RateUploadPerHour = defaultRateUploadHour    // 60
		d.Cfg.RateUploadPerHourPerIP = defaultRateUploadIP // 120
		d.Cfg.RateAuthPerMin = 100
	})
	const attacker = "198.51.100.7:4444"
	post := func(cookie *http.Cookie, addr string) resp {
		return e.upload(`{}`, "a.mp4", testVideo(10), withCookie(cookie), withRemote(addr))
	}
	spend := func(name string, n int) *http.Cookie {
		t.Helper()
		_, cookie := e.newUser(name)
		for i := 0; i < n; i++ {
			if r := post(cookie, attacker); r.code != 201 {
				t.Fatalf("%s upload %d: %d %s", name, i+1, r.code, r.body)
			}
		}
		return cookie
	}
	// One account exhausts its own 60 and is refused on the 61st.
	a := spend("acct_a", defaultRateUploadHour)
	if r := post(a, attacker); r.code != 429 || r.errCode() != "rate_limited" || r.hdr.Get("Retry-After") == "" {
		t.Fatalf("61st upload from one user: %d %s Retry-After=%q", r.code, r.body, r.hdr.Get("Retry-After"))
	}
	// A fresh account from the same address gets a fresh USER budget — that
	// is the amplification — but spends the address's remaining 60.
	spend("acct_b", defaultRateUploadIP-defaultRateUploadHour)
	// The 121st upload from that address is refused however new the account.
	_, c := e.newUser("acct_c")
	if r := post(c, attacker); r.code != 429 || r.errCode() != "rate_limited" || r.hdr.Get("Retry-After") == "" {
		t.Fatalf("121st upload from one IP: %d %s Retry-After=%q", r.code, r.body, r.hdr.Get("Retry-After"))
	}
	// And a household behind a different NAT is untouched by any of it.
	if r := post(c, "203.0.113.9:5555"); r.code != 201 {
		t.Fatalf("same account from another address: %d %s", r.code, r.body)
	}
}

// The 413 speaks to whoever is uploading: app.js renders `message` verbatim,
// so it must carry the actual limit and no environment-variable name
// (§20 erratum 97). The code and the status are unchanged.
func TestUploadTooLargeMessageIsHuman(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Cfg.MaxUploadBytes = 4 << 30 })
	_, cookie := e.newUser("carol")
	r := e.upload(`{}`, "a.mp4", testVideo(10), withCookie(cookie),
		func(req *http.Request) { req.ContentLength = (4 << 30) + (8 << 20) })
	if r.code != 413 || r.errCode() != "too_large" {
		t.Fatalf("%d %s", r.code, r.body)
	}
	msg := r.str("message")
	if strings.Contains(msg, "MAX_UPLOAD_BYTES") {
		t.Errorf("the 413 still names an environment variable: %q", msg)
	}
	if !strings.Contains(msg, "4.0 GiB") {
		t.Errorf("the 413 does not say what the limit is: %q", msg)
	}
	// The same wording on the capture endpoint, which shares the helper.
	r2 := e.capture([]part{filePart("file", "a.mp4", testVideo(10))}, withCookie(cookie),
		func(req *http.Request) { req.ContentLength = (4 << 30) + (8 << 20) })
	if r2.code != 413 || r2.str("message") != msg {
		t.Fatalf("POST /api/upload 413: %d %s", r2.code, r2.body)
	}
}

// The same golden table internal/web and cmd/clippet read (nit sweep S11): this
// copy and internal/web's must agree on every row, which is what the shared file
// makes checkable.
func TestHumanBytes(t *testing.T) {
	for _, tc := range humanBytesGolden(t) {
		if got := humanBytes(tc.N); got != tc.Page {
			t.Errorf("humanBytes(%d) = %q, want %q (testdata/humanbytes.tsv)", tc.N, got, tc.Page)
		}
	}
}

// humanBytesGolden reads the ONE table all three humanBytes copies are pinned
// against (../../testdata/humanbytes.tsv, nit sweep S11) and returns
// (bytes, page spelling, cli spelling) triples. The file's own header explains
// the two deliberate page/CLI differences; a THIRD difference is a bug in
// whichever copy moved.
func humanBytesGolden(t *testing.T) []struct {
	N         int64
	Page, CLI string
} {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "humanbytes.tsv"))
	if err != nil {
		t.Fatalf("read the humanBytes golden: %v", err)
	}
	var out []struct {
		N         int64
		Page, CLI string
	}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			t.Fatalf("humanbytes.tsv:%d: %d tab-separated fields, want 3 (%q)", i+1, len(f), line)
		}
		n, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			t.Fatalf("humanbytes.tsv:%d: %v", i+1, err)
		}
		out = append(out, struct {
			N         int64
			Page, CLI string
		}{n, f[1], f[2]})
	}
	if len(out) < 8 {
		t.Fatalf("the humanBytes golden holds %d rows; it is meant to be the whole table", len(out))
	}
	return out
}

// source_filename is bounded (§20 erratum 26): a multipart filename may be
// megabytes long and would otherwise be stored verbatim, bloat every
// ClipDTO (the shared library included) and break the owner's own original
// download with a Content-Disposition no client accepts.
func TestUploadLongFilenameIsCapped(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")
	video := testVideo(1000)
	r := e.upload(`{}`, strings.Repeat("A", 300_000)+".mp4", video, withCookie(cookie))
	if r.code != 201 {
		t.Fatalf("upload: %d %.200s", r.code, r.body)
	}
	name, _ := r.json["source"].(map[string]any)["filename"].(string)
	if utf8.RuneCountInString(name) != maxFilenameRunes || !strings.HasSuffix(name, ".mp4") || !strings.HasPrefix(name, "AAAA") {
		t.Fatalf("stored filename: %d runes, %.20q…%q", utf8.RuneCountInString(name), name, name[len(name)-8:])
	}
	if utf8.RuneCountInString(r.str("title")) != maxTitleRunes {
		t.Fatalf("default title: %d runes", utf8.RuneCountInString(r.str("title")))
	}
	if len(r.body) > 4096 {
		t.Fatalf("DTO is %d bytes", len(r.body))
	}
	// The owner's download carries the capped name.
	cd := e.do("HEAD", "/api/clips/"+r.str("id")+"/original", nil, withCookie(cookie)).hdr.Get("Content-Disposition")
	if len(cd) > 400 || !strings.HasSuffix(strings.TrimSuffix(cd, `"`), ".mp4") {
		t.Fatalf("Content-Disposition (%d bytes): %.60s…", len(cd), cd)
	}
}

// source_content_type is bounded the same way (§20 erratum 40): the video
// part's Content-Type is as client-controlled as its filename, and an
// oversized one was stored, echoed in every ClipDTO — the shared library
// listing included — sent as the object's Content-Type metadata (which the
// provider rejects past a few KiB, turning a malformed request into a
// 503 storage_unavailable with a WARN naming the operator's bucket) and
// written verbatim as the response header of GET /original.
func TestUploadLongContentTypeIsCapped(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")
	body, ct := multipartBody(t,
		part{name: "metadata", contentType: "application/json", data: []byte(`{}`)},
		part{name: "video", filename: "clip.mp4", contentType: "video/" + strings.Repeat("z", 300_000), data: testVideo(1000)})
	r := e.do(http.MethodPost, "/api/clips", body, withHeader("Content-Type", ct), withCookie(cookie))
	if r.code != 201 {
		t.Fatalf("upload: %d %.200s", r.code, r.body)
	}
	// The part type is not a media type any client means: the stored type is
	// derived from the extension instead (§6).
	got, _ := r.json["source"].(map[string]any)["content_type"].(string)
	if got != "video/mp4" {
		t.Fatalf("stored content_type: %d bytes, %.40q…", len(got), got)
	}
	if len(r.body) > 4096 {
		t.Fatalf("DTO is %d bytes", len(r.body))
	}
	// The owner's download carries the bounded type, not the part's.
	if hdr := e.do("HEAD", "/api/clips/"+r.str("id")+"/original", nil, withCookie(cookie)).hdr.Get("Content-Type"); hdr != "video/mp4" {
		t.Fatalf("original Content-Type (%d bytes): %.60q…", len(hdr), hdr)
	}
	// A plausible video type still survives untouched.
	r = e.upload(`{}`, "clip.mkv", testVideo(1000), withCookie(cookie))
	if got, _ := r.json["source"].(map[string]any)["content_type"].(string); r.code != 201 || got != "video/mp4" {
		t.Fatalf("normal upload: %d %q", r.code, got)
	}
}

func TestTruncateName(t *testing.T) {
	tests := []struct {
		in      string
		max     int
		want    string
		wantLen int
	}{
		{"clip.mp4", 255, "clip.mp4", 8},
		{strings.Repeat("中", 300) + ".mp4", 255, strings.Repeat("中", 251) + ".mp4", 255},
		{strings.Repeat("a", 300), 255, strings.Repeat("a", 255), 255},
		{strings.Repeat("a", 9) + " " + strings.Repeat("b", 300) + ".mov", 12, "aaaaaaaa.mov", 12}, // cut then trimmed, ext kept
		{"a." + strings.Repeat("x", 300), 20, "a." + strings.Repeat("x", 18), 20},                  // an "extension" of 300 runes is not one
	}
	for _, tc := range tests {
		got := truncateName(tc.in, tc.max)
		if got != tc.want || utf8.RuneCountInString(got) != tc.wantLen {
			t.Errorf("truncateName(%.20q…, %d) = %.30q… (%d runes), want %.30q… (%d)", tc.in, tc.max, got, utf8.RuneCountInString(got), tc.want, tc.wantLen)
		}
	}
}

func TestDefaultTitle(t *testing.T) {
	tests := map[string]string{
		"clip.mp4":                        "clip",
		"C:\\Videos\\My Clip.MOV":         "My Clip",
		"/tmp/x/.hidden":                  "Untitled",
		"":                                "Untitled",
		"   ":                             "Untitled",
		"noext":                           "noext",
		"weird\x00name.mp4":               "weirdname",
		strings.Repeat("a", 250) + ".mp4": strings.Repeat("a", 200),
	}
	for in, want := range tests {
		if got := defaultTitle(in); got != want {
			t.Errorf("defaultTitle(%q) = %q, want %q", in, got, want)
		}
	}
	if got := baseName("..\\..\\evil.mp4"); got != "evil.mp4" {
		t.Errorf("baseName = %q", got)
	}
}

// TestDefaultTitleFromACaptureTimestamp pins §20 erratum 317(a): a capture
// tool's timestamp in the file's name becomes `YYYY-MM-DD HH:MM`, with any
// text before it kept, and a name without one is the stem exactly as before.
func TestDefaultTitleFromACaptureTimestamp(t *testing.T) {
	tests := []struct{ in, want string }{
		// The brief's examples, each tool's own shape.
		{"2025-06-24 00-23-54.mkv", "2025-06-24 00:23"},                                                            // OBS
		{"2025-06-24_00-23-54-00.00.22.520-00.00.40.483.mp4", "2025-06-24 00:23"},                                  // OBS, trimmed
		{"2025-06-24_00-23-54.mp4", "2025-06-24 00:23"},                                                            // ShareX
		{"Arc Raiders 2026.01.06 - 00.15.37.07.DVR.mp4", "Arc Raiders 2026-01-06 00:15"},                           // Game Bar
		{"Arc_Raiders_2026.01.06_-_00.15.37.07.DVR-00.02.14.016-00.02.23.033.mp4", "Arc Raiders 2026-01-06 00:15"}, // its trim
		{"Arc_Raiders_2026.01.06_-_00.15.37.07.DVR-00.02.14.016-00.02.23.033", "Arc Raiders 2026-01-06 00:15"},     // as the brief printed it
		// The brief listed this among the negatives; the rule it states keeps
		// the text before the date and drops the `(1)`, so it is a positive
		// (the ruling of 2026-10-06).
		{"final 2025-06-24 00-23-54 (1).mp4", "final 2025-06-24 00:23"},
		// Each separator the rule names, and the bare HH-MM clock.
		{"2025_06_24_00_23_54.mp4", "2025-06-24 00:23"},
		{"2025.06.24 00.23.54.mp4", "2025-06-24 00:23"},
		{"2025-06-24-00-23-54.mp4", "2025-06-24 00:23"},
		{"2025-06-24 00-23.mp4", "2025-06-24 00:23"},
		{"Game__Name   2025-06-24 23-59-59.mp4", "Game Name 2025-06-24 23:59"},
		// The negatives: the stem, unchanged, byte for byte.
		{"2025.mp4", "2025"},
		{"v1.2.3.mp4", "v1.2.3"},
		{"192.168.0.1.mp4", "192.168.0.1"},
		{"2025-06-24.mp4", "2025-06-24"},
		{"2025-06-24 clip.mp4", "2025-06-24 clip"},
		{"2025-06-24 00.23.mp4", "2025-06-24 00.23"},       // HH.MM alone is not a clock the rule names
		{"2025-06.24 00-23-54.mp4", "2025-06.24 00-23-54"}, // the date's separators disagree
		{"2025-13-24 00-23-54.mp4", "2025-13-24 00-23-54"}, // no thirteenth month
		{"2025-06-24 24-00-00.mp4", "2025-06-24 24-00-00"}, // no hour 24
		{"2025-06-24 00-23-541.mp4", "2025-06-24 00-23-541"},
		{"12025-06-24 00-23-54.mp4", "12025-06-24 00-23-54"}, // the date starts inside a run of digits
	}
	for _, tc := range tests {
		if got := defaultTitle(tc.in); got != tc.want {
			t.Errorf("defaultTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestUploadIDCollision: §8.3 step 8 – CreateClipWithJob reports a UNIQUE
// violation on the id as db.ErrConflict, which the handler answers with 500
// id_collision after undoing its own side effects. The colliding row appears
// inside the window between pickClipID's ClipExists check and the insert
// (the probe hook runs there) – exactly where a concurrent upload that drew
// the same id would land; newClipID pins the id so the test can plant it.
func TestUploadIDCollision(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")
	winnerOwner, _ := e.newUser("other")
	id := ids.New()
	restore := newClipID
	newClipID = func(string, bool) string { return id }
	t.Cleanup(func() { newClipID = restore })

	var winner db.Clip
	winnerSource := testVideo(777)
	e.probe = func(context.Context, string) (*transcode.ProbeResult, error) {
		// Someone else took the id after our ClipExists check. Their
		// original is a .mov, so its key differs from the one we write.
		winner = e.seedClip(winnerOwner, clipOpts{id: id, ext: "mov", source: winnerSource})
		return &transcode.ProbeResult{VideoIndex: 0}, nil
	}
	dirs := scratchDirs(t, e.scratch)
	r := e.upload(`{"title":"loser"}`, "a.mp4", testVideo(1000), withCookie(cookie))
	if r.code != 500 || r.errCode() != "id_collision" || r.str("message") == "" {
		t.Fatalf("collision: %d %s", r.code, r.body)
	}
	if ct := r.hdr.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	// The object this request wrote is gone (abortUpload), and only it.
	ours := store.KeysFor(e.cfg.S3.KeyPrefix, id, "mp4").Original
	if _, ok := e.fake.Get(ours); ok {
		t.Fatalf("loser's original %s survived the collision", ours)
	}
	if deletes := e.fake.Deletes(); len(deletes) != 1 || deletes[0] != ours {
		t.Fatalf("deletes = %v, want exactly [%s]", deletes, ours)
	}
	calls := e.fake.CallsSnapshot()
	if last := calls[len(calls)-1]; last != "DeletePrefix "+ours {
		t.Fatalf("last store call = %q, want the loser's key deleted", last)
	}
	// The winner is untouched: row, state and original.
	got := e.getClip(id)
	if got.OwnerID != winner.OwnerID || got.State != db.ClipQueued || got.SourceKey != winner.SourceKey ||
		got.SourceETag != winner.SourceETag || got.Title != winner.Title || got.SourceSize != winner.SourceSize {
		t.Fatalf("winner row changed:\n%+v\n%+v", winner, got)
	}
	if data, ok := e.fake.Get(winner.SourceKey); !ok || string(data) != string(winnerSource) {
		t.Fatalf("winner's original %s missing or altered", winner.SourceKey)
	}
	// No spool left behind, no wake, and only the winner's clip exists.
	assertScratchUnchanged(t, e.scratch, dirs)
	if n := e.worker.woken.Load(); n != 0 {
		t.Fatalf("worker woken %d times for a failed upload", n)
	}
	clips, _, err := e.db.ListClips(context.Background(), db.ListQuery{ViewerID: winnerOwner.ID})
	if err != nil || len(clips) != 1 || clips[0].ID != id {
		t.Fatalf("clips = %v (%v)", clips, err)
	}
}

// TestUploadLimitSaturates: MAX_UPLOAD_BYTES near MaxInt64 plus the 1 MiB
// multipart slack must not wrap — http.MaxBytesReader clamps a negative n to
// 0 and would answer 413 to the very first metadata part of every upload.
// config.Load bounds the variable (§20 erratum 33); this is the arithmetic's
// own guard.
func TestUploadLimitSaturates(t *testing.T) {
	for _, c := range []struct{ in, want int64 }{
		{1, 1 + uploadSlackBytes},
		{4 << 30, 4<<30 + uploadSlackBytes},
		{1 << 62, 1<<62 + uploadSlackBytes},
		{math.MaxInt64 - uploadSlackBytes, math.MaxInt64},
		{math.MaxInt64 - uploadSlackBytes + 1, math.MaxInt64},
		{math.MaxInt64, math.MaxInt64},
	} {
		if got := uploadLimit(c.in); got != c.want || got <= 0 {
			t.Errorf("uploadLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// errAfterReader delivers data, then fails. It stands in for a body that
// stops arriving mid-part (a dropped connection) as opposed to one that
// arrives whole but does not parse.
type errAfterReader struct {
	data []byte
	err  error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// rawUpload posts a hand-built multipart body through AccessLog so the test
// can read the annotations the handler makes (SetLogField is a no-op without
// that middleware, and the false field is half of what this pins).
func rawUpload(t *testing.T, e *env, cookie *http.Cookie, boundary string, body io.Reader) (resp, string) {
	t.Helper()
	return e.logged(http.MethodPost, "/api/clips", body,
		withHeader("Content-Type", "multipart/form-data; boundary="+boundary), withCookie(cookie))
}

// unterminatedBody is a complete, Content-Length-framed request whose video
// part never reaches a closing boundary: mime/multipart answers
// io.ErrUnexpectedEOF from Part.Read while the client sits on the connection
// waiting for the reply.
func unterminatedBody(boundary string) string {
	return "--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"metadata\"\r\n" +
		"Content-Type: application/json\r\n\r\n" +
		`{"title":"repro"}` + "\r\n" +
		"--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"video\"; filename=\"a.mp4\"\r\n" +
		"Content-Type: video/mp4\r\n\r\n" +
		"AAAA"
}

// A malformed body is a body-validation failure, not a disconnect (§20
// erratum 53). spoolVideo's catch-all used to assert client_disconnected on
// every io.Copy error that was neither MaxBytes nor disk, so a truncating
// proxy or a hand-rolled uploader was told — in the reply it demonstrably
// read, and in the access log an operator later trusts — that it had gone
// away. The real disconnect must keep its own answer, so both are pinned.
func TestUploadMalformedMultipartIsNotAClientDisconnect(t *testing.T) {
	const boundary = "zz9"

	t.Run("unterminated part", func(t *testing.T) {
		e := newEnv(t)
		_, cookie := e.newUser("carol")
		r, logs := rawUpload(t, e, cookie, boundary, strings.NewReader(unterminatedBody(boundary)))
		if r.code != 400 || r.errCode() != "invalid_request" {
			t.Fatalf("%d %s", r.code, r.body)
		}
		if msg := r.str("message"); strings.Contains(msg, "disconnect") {
			t.Errorf("message claims a disconnect that did not happen: %q", msg)
		}
		if strings.Contains(logs, "client_disconnected") {
			t.Errorf("access log claims a disconnect: %s", logs)
		}
		if !strings.Contains(logs, "reject=malformed_multipart") {
			t.Errorf("access log lacks the reject class: %s", logs)
		}
		if left := scratchDirs(t, e.scratch); len(left) != 0 {
			t.Errorf("scratch not cleaned: %v", left)
		}
	})

	t.Run("real disconnect", func(t *testing.T) {
		e := newEnv(t)
		_, cookie := e.newUser("carol")
		body := &errAfterReader{data: []byte(unterminatedBody(boundary)), err: context.Canceled}
		r, logs := rawUpload(t, e, cookie, boundary, body)
		if r.code != 400 || r.errCode() != "upload_interrupted" {
			t.Fatalf("%d %s", r.code, r.body)
		}
		if !strings.Contains(logs, "client_disconnected=true") {
			t.Errorf("access log lost the disconnect: %s", logs)
		}
		if left := scratchDirs(t, e.scratch); len(left) != 0 {
			t.Errorf("scratch not cleaned: %v", left)
		}
	})
}
