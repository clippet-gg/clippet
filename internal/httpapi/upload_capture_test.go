package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/transcode"
)

// --- helpers ---------------------------------------------------------------

func textPart(name, value string) part { return part{name: name, data: []byte(value)} }

func filePart(name, filename string, data []byte) part {
	return part{name: name, filename: filename, contentType: "video/mp4", data: data}
}

// capture posts a capture-tool body: the parts exactly in the given order.
func (e *env) capture(parts []part, opts ...reqOpt) resp {
	e.t.Helper()
	body, ct := multipartBody(e.t, parts...)
	return e.do(http.MethodPost, "/api/upload", body, append([]reqOpt{withHeader("Content-Type", ct)}, opts...)...)
}

// --- the §8.1 response -----------------------------------------------------

// The capture 201 is exactly the five §8.1 fields, `url` being the share page
// (what ShareX pastes) and `embed_url` the player page, both absolute.
func TestCaptureUploadResponse(t *testing.T) {
	e := newEnv(t)
	u, cookie := e.newUser("carol")
	video := testVideo(4000)

	r := e.capture([]part{
		textPart("title", "Ace clutch"),
		textPart("ttl_days", "7"),
		filePart("file", "clip.mp4", video),
	}, withCookie(cookie))
	if r.code != 201 {
		t.Fatalf("upload: %d %s", r.code, r.body)
	}
	id := r.str("id")
	if id == "" || r.str("url") != testOrigin+"/"+id || r.str("embed_url") != testOrigin+"/embed/"+id {
		t.Fatalf("urls: %s", r.body)
	}
	if r.str("state") != "queued" || r.json["expires_at"] == nil {
		t.Fatalf("state/expiry: %s", r.body)
	}
	// A queued clip has no card yet, and the reply says so rather than implying
	// it from `state` (§20 erratum 212).
	if r.json["card_ready"] != false {
		t.Errorf("card_ready = %v on a queued clip: %s", r.json["card_ready"], r.body)
	}
	want := map[string]bool{"id": true, "url": true, "embed_url": true, "kind": true, "state": true,
		"card_ready": true, "expires_at": true, "tiers": true}
	for k := range r.json {
		if !want[k] {
			t.Errorf("unexpected key %q in the capture 201: %s", k, r.body)
		}
	}
	for k := range want {
		if !r.has(k) {
			t.Errorf("missing key %q: %s", k, r.body)
		}
	}
	// The clip really landed: owner, title and the original in the bucket.
	c, err := e.db.GetClip(context.Background(), id)
	if err != nil || c.OwnerID != u.ID || c.Title != "Ace clutch" || c.SourceFilename != "clip.mp4" {
		t.Fatalf("row: %+v err=%v", c, err)
	}
	if got, ok := e.fake.Get(c.SourceKey); !ok || string(got) != string(video) {
		t.Fatalf("original not stored under %s", c.SourceKey)
	}
	if e.worker.woken.Load() == 0 {
		t.Fatal("worker was not woken")
	}
}

// --- the multipart grammar (§20 erratum 58) --------------------------------

// Text fields are accepted in ANY order before the file part, under either
// accepted file-part name.
func TestCaptureUploadFieldOrderAndPartNames(t *testing.T) {
	video := testVideo(1000)
	cases := []struct {
		name  string
		parts []part
	}{
		{"title first", []part{
			textPart("title", "T"), textPart("description", "D"),
			textPart("unlisted", "true"), textPart("ttl_days", "30"),
			filePart("file", "a.mp4", video)}},
		{"scrambled", []part{
			textPart("ttl_days", "30"), textPart("unlisted", "true"),
			textPart("description", "D"), textPart("title", "T"),
			filePart("file", "a.mp4", video)}},
		{"part named video", []part{
			textPart("title", "T"), textPart("description", "D"),
			textPart("unlisted", "true"), textPart("ttl_days", "30"),
			filePart("video", "a.mp4", video)}},
		{"unknown fields ignored", []part{
			textPart("shareX", "17"), textPart("title", "T"), textPart("name", "whatever"),
			textPart("description", "D"), textPart("unlisted", "true"),
			textPart("ttl_days", "30"), textPart("metadata", `{"title":"ignored"}`),
			filePart("file", "a.mp4", video)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			_, cookie := e.newUser("carol")
			r := e.capture(tc.parts, withCookie(cookie))
			if r.code != 201 {
				t.Fatalf("%d %s", r.code, r.body)
			}
			c, err := e.db.GetClip(context.Background(), r.str("id"))
			if err != nil {
				t.Fatal(err)
			}
			if c.Title != "T" || c.Description != "D" || !c.Unlisted {
				t.Fatalf("fields: %+v", c)
			}
			if c.TTLSeconds == nil || *c.TTLSeconds != 30*86400 {
				t.Fatalf("ttl_seconds: %v", c.TTLSeconds)
			}
		})
	}
}

// A text field AFTER the file part is never read: by then the stream is
// spooled and the request is answered, so the title falls back to the
// filename (§20 erratum 58).
func TestCaptureUploadFieldAfterFileIsIgnored(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")
	r := e.capture([]part{
		filePart("file", "Replay 2026-09-04 18-22-13.mp4", testVideo(1000)),
		textPart("title", "too late"),
		textPart("ttl_days", "90"),
	}, withCookie(cookie))
	if r.code != 201 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	c, err := e.db.GetClip(context.Background(), r.str("id"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Title != "Replay 2026-09-04 18:22" { // the fallback's timestamp rule, §20 erratum 317(a)
		t.Fatalf("title = %q, want the filename's fallback title", c.Title)
	}
	if c.TTLSeconds != nil || r.json["expires_at"] != nil {
		t.Fatalf("a ttl_days after the file part was applied: %v %s", c.TTLSeconds, r.body)
	}
}

// The title defaults to the filename without its extension (§8.1), and an
// empty or blank title field means "not sent" rather than "Untitled".
func TestCaptureUploadTitleDefaults(t *testing.T) {
	cases := []struct{ name, title, filename, want string }{
		{"no title field", "", "my game clip.mp4", "my game clip"},
		{"empty title field", "", "Highlight.mkv", "Highlight"},
		{"blank title field", "   ", "Highlight.mkv", "Highlight"},
		{"path stripped", "", `D:\Videos\raid boss.mp4`, "raid boss"},
		{"no extension", "", "clip", "clip"},
		{"unusable filename", "", " ", untitled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			_, cookie := e.newUser("carol")
			parts := []part{}
			if tc.name != "no title field" {
				parts = append(parts, textPart("title", tc.title))
			}
			parts = append(parts, filePart("file", tc.filename, testVideo(1000)))
			r := e.capture(parts, withCookie(cookie))
			if r.code != 201 {
				t.Fatalf("%d %s", r.code, r.body)
			}
			c, err := e.db.GetClip(context.Background(), r.str("id"))
			if err != nil {
				t.Fatal(err)
			}
			if c.Title != tc.want {
				t.Fatalf("title = %q, want %q", c.Title, tc.want)
			}
		})
	}
}

// A title that is exactly the file's own name is treated as absent, so the
// shared defaultTitle fallback strips the extension (§20 erratum 60). This is
// the RePlays/OBS-script case: a tool whose title argument is filled from the
// filename would otherwise title every clip "….mp4". Any other title is a
// real choice and survives verbatim, extension or not.
func TestCaptureUploadTitleEqualToTheFilename(t *testing.T) {
	cases := []struct{ name, title, filename, want string }{
		{"title is the filename", "2026-09-04_18-22-13.mp4", "2026-09-04_18-22-13.mp4", "2026-09-04 18:22"}, // the fallback's timestamp rule, §20 erratum 317(a)
		{"title is the filename, path sent too", "raid boss.mkv", `C:\clips\raid boss.mkv`, "raid boss"},
		{"title is the filename with spaces around it", " clip.mp4 ", "clip.mp4", "clip"},
		{"a real title that ends in .mp4 is kept", "My clip.mp4", "raw-000123.mp4", "My clip.mp4"},
		{"a real title is kept", "Ace clutch", "2026-09-04_18-22-13.mp4", "Ace clutch"},
		{"title equal to a filename with no extension", "highlight", "highlight", "highlight"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			_, cookie := e.newUser("carol")
			r := e.capture([]part{
				textPart("title", tc.title),
				filePart("file", tc.filename, testVideo(1000)),
			}, withCookie(cookie))
			if r.code != 201 {
				t.Fatalf("%d %s", r.code, r.body)
			}
			c, err := e.db.GetClip(context.Background(), r.str("id"))
			if err != nil {
				t.Fatal(err)
			}
			if c.Title != tc.want {
				t.Fatalf("title = %q, want %q", c.Title, tc.want)
			}
		})
	}
	// The rule is the capture path's alone: POST /api/clips carries an
	// explicit metadata document from the browser form, where a title equal
	// to the filename is what the user typed.
	e := newEnv(t)
	_, cookie := e.newUser("carol")
	r := e.upload(`{"title":"clip.mp4"}`, "clip.mp4", testVideo(1000), withCookie(cookie))
	if r.code != 201 || r.str("title") != "clip.mp4" {
		t.Fatalf("POST /api/clips must keep an explicit title verbatim: %d %s", r.code, r.body)
	}
}

// The TTL wire rule of §8.2 holds on the form too, including 0-vs-missing.
func TestCaptureUploadTTL(t *testing.T) {
	day := int64(86400)
	cases := []struct {
		name    string
		value   string // "" → the field is not sent at all
		def     *int64
		code    int
		errCode string
		ttl     *int64
	}{
		{name: "absent uses the server default", def: &day, code: 201, ttl: &day},
		{name: "absent with no default is never", code: 201},
		{name: "empty value is absent", value: "", def: &day, code: 201, ttl: &day},
		{name: "explicit 0 is never", value: "0", def: &day, code: 201},
		{name: "7 days", value: "7", code: 201, ttl: ptr(7 * day)},
		{name: "spaces tolerated", value: " 30 ", code: 201, ttl: ptr(30 * day)},
		{name: "5 is invalid", value: "5", code: 400, errCode: "invalid_ttl"},
		{name: "not a number", value: "seven", code: 400, errCode: "invalid_ttl"},
		{name: "negative", value: "-1", code: 400, errCode: "invalid_ttl"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, func(d *Deps) { d.Cfg.DefaultTTLSeconds = tc.def })
			_, cookie := e.newUser("carol")
			parts := []part{}
			if !strings.HasPrefix(tc.name, "absent") {
				parts = append(parts, textPart("ttl_days", tc.value))
			}
			parts = append(parts, filePart("file", "a.mp4", testVideo(1000)))
			r := e.capture(parts, withCookie(cookie))
			if r.code != tc.code || (tc.errCode != "" && r.errCode() != tc.errCode) {
				t.Fatalf("%d %s", r.code, r.body)
			}
			if tc.code != 201 {
				return
			}
			c, err := e.db.GetClip(context.Background(), r.str("id"))
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.ttl == nil && c.TTLSeconds != nil:
				t.Fatalf("ttl_seconds = %v, want never", *c.TTLSeconds)
			case tc.ttl != nil && (c.TTLSeconds == nil || *c.TTLSeconds != *tc.ttl):
				t.Fatalf("ttl_seconds = %v, want %v", c.TTLSeconds, *tc.ttl)
			}
			if (tc.ttl == nil) != (r.json["expires_at"] == nil) {
				t.Fatalf("expires_at = %v for ttl %v", r.json["expires_at"], tc.ttl)
			}
		})
	}
}

func ptr(v int64) *int64 { return &v }

// `unlisted` takes the spellings a form or a capture tool sends.
func TestCaptureUploadUnlisted(t *testing.T) {
	cases := []struct {
		value string
		code  int
		want  bool
	}{
		{"true", 201, true}, {"TRUE", 201, true}, {"1", 201, true}, {"on", 201, true}, {"yes", 201, true},
		{"false", 201, false}, {"0", 201, false}, {"no", 201, false}, {"", 201, false},
		{"maybe", 400, false},
	}
	for _, tc := range cases {
		t.Run("unlisted="+tc.value, func(t *testing.T) {
			e := newEnv(t)
			_, cookie := e.newUser("carol")
			r := e.capture([]part{
				textPart("unlisted", tc.value),
				filePart("file", "a.mp4", testVideo(1000)),
			}, withCookie(cookie))
			if r.code != tc.code {
				t.Fatalf("%d %s", r.code, r.body)
			}
			if tc.code != 201 {
				if r.errCode() != "invalid_request" || r.str("message") != "unlisted must be true or false" {
					t.Fatalf("message: %s", r.body)
				}
				return
			}
			c, err := e.db.GetClip(context.Background(), r.str("id"))
			if err != nil {
				t.Fatal(err)
			}
			if c.Unlisted != tc.want {
				t.Fatalf("unlisted = %v, want %v", c.Unlisted, tc.want)
			}
		})
	}
}

// Text validation is the same code as the metadata part's (§8.2, §20
// errata 26/29): same codes, same messages.
func TestCaptureUploadTextValidation(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")
	video := testVideo(1000)
	cases := []struct{ name, field, value, want string }{
		{"title control char", "title", "nul\x00x", "title must not contain control characters"},
		{"title too long", "title", strings.Repeat("中", maxTitleRunes+1), "title must be at most 200 characters"},
		{"description control char", "description", "d\x07", "description must not contain control characters"},
		{"description too long", "description", strings.Repeat("x", maxDescriptionRunes+1), "description must be at most 2000 characters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := e.capture([]part{textPart(tc.field, tc.value), filePart("file", "a.mp4", video)}, withCookie(cookie))
			if r.code != 400 || r.errCode() != "invalid_request" || r.str("message") != tc.want {
				t.Fatalf("%d %s (want %q)", r.code, r.body, tc.want)
			}
		})
	}
	// A field larger than the per-field bound is rejected before it is parsed.
	r := e.capture([]part{textPart("description", strings.Repeat("x", maxCaptureFieldByte+1)), filePart("file", "a.mp4", video)}, withCookie(cookie))
	if r.code != 400 || r.errCode() != "invalid_request" || !strings.Contains(r.str("message"), "16 KiB") {
		t.Fatalf("oversized field: %d %s", r.code, r.body)
	}
	// And a body that is only fields cannot make the handler read forever.
	parts := make([]part, 0, maxCaptureFields+2)
	for i := 0; i <= maxCaptureFields; i++ {
		parts = append(parts, textPart("pad", "x"))
	}
	parts = append(parts, filePart("file", "a.mp4", video))
	if r := e.capture(parts, withCookie(cookie)); r.code != 400 || r.errCode() != "invalid_request" {
		t.Fatalf("field flood: %d %s", r.code, r.body)
	}
}

// A body with no file part — or with the file under an unexpected name — is
// the §8.3 missing_video rejection.
func TestCaptureUploadMissingFile(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")
	video := testVideo(1000)
	cases := []struct {
		name  string
		parts []part
	}{
		{"no parts", nil},
		{"fields only", []part{textPart("title", "T"), textPart("ttl_days", "7")}},
		{"file part under another name", []part{filePart("clip", "a.mp4", video)}},
		{"file name but no filename", []part{textPart("file", "not a file")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := e.capture(tc.parts, withCookie(cookie))
			if r.code != 400 || r.errCode() != "missing_video" {
				t.Fatalf("%d %s", r.code, r.body)
			}
		})
	}
}

// --- the shared pipeline (§17: one ingest core, one set of limits) ---------

// Every failure class answers identically on both upload routes: the limits,
// the auth model, the CSRF rule and the scratch/probe/storage gates live in
// beginUpload and ingest, not in either handler.
func TestCaptureUploadSharesTheClipsPipeline(t *testing.T) {
	video := testVideo(4000)
	cases := []struct {
		name    string
		mutate  func(*env)
		code    int
		errCode string
		parts   []part // capture parts; the /api/clips twin is built from meta+video
		meta    string
	}{
		{name: "too large", code: 413, errCode: "too_large"},
		{name: "insufficient scratch", code: 507, errCode: "insufficient_scratch",
			mutate: func(e *env) { e.free = func(string) (int64, error) { return 0, nil } }},
		{name: "not a video", code: 415, errCode: "not_a_video",
			mutate: func(e *env) {
				e.probe = func(context.Context, string) (*transcode.ProbeResult, error) {
					return nil, &transcode.ErrPermanent{Err: errors.New("no video stream")}
				}
			}},
		{name: "probe unavailable", code: 503, errCode: "unavailable",
			mutate: func(e *env) {
				e.probe = func(context.Context, string) (*transcode.ProbeResult, error) {
					return nil, errors.New("ffprobe exec failed")
				}
			}},
		{name: "empty video", code: 400, errCode: "empty_video"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := video
			switch tc.name {
			case "empty video":
				body = nil
			case "too large":
				body = testVideo(1<<20 + 2<<20) // the harness MAX_UPLOAD_BYTES plus the framing slack
			}
			run := func(capture bool) resp {
				e := newEnv(t)
				if tc.mutate != nil {
					tc.mutate(e)
				}
				_, cookie := e.newUser("carol")
				if capture {
					return e.capture([]part{textPart("title", "T"), filePart("file", "a.mp4", body)}, withCookie(cookie))
				}
				return e.upload(`{"title":"T"}`, "a.mp4", body, withCookie(cookie))
			}
			clips, cap := run(false), run(true)
			if clips.code != tc.code || clips.errCode() != tc.errCode {
				t.Fatalf("POST /api/clips: %d %s", clips.code, clips.body)
			}
			if cap.code != clips.code || cap.errCode() != clips.errCode() {
				t.Fatalf("POST /api/upload answered %d %s; /api/clips answered %d %s",
					cap.code, cap.body, clips.code, clips.body)
			}
		})
	}
}

// The credential model is §8.1's: a session, or a token with the upload
// scope. Nothing else, and no CSRF exemption a browser could abuse.
func TestCaptureUploadAuth(t *testing.T) {
	e := newEnv(t)
	u, cookie := e.newUser("carol")
	video := testVideo(1000)
	parts := []part{textPart("title", "T"), filePart("file", "a.mp4", video)}

	if r := e.capture(parts); r.code != 401 || r.errCode() != "unauthorized" {
		t.Fatalf("anonymous: %d %s", r.code, r.body)
	}
	if r := e.capture(parts, withBearer(e.newToken(u, "read"))); r.code != 403 || r.errCode() != "insufficient_scope" {
		t.Fatalf("token without the upload scope: %d %s", r.code, r.body)
	}
	if r := e.capture(parts, withBearer("ck_nope")); r.code != 401 {
		t.Fatalf("bad token: %d %s", r.code, r.body)
	}
	if r := e.capture(parts, withCookie(cookie), withHeader("Origin", "https://evil.example")); r.code != 403 || r.errCode() != "csrf" {
		t.Fatalf("cross-origin cookie POST: %d %s", r.code, r.body)
	}
	// A Bearer request skips the CSRF check (§14) — capture tools send no Origin.
	if r := e.capture(parts, withBearer(e.newToken(u, "upload"))); r.code != 201 {
		t.Fatalf("upload-scoped token: %d %s", r.code, r.body)
	}
	if r := e.do("POST", "/api/upload", strings.NewReader("{}"), withCookie(cookie), withJSON()); r.code != 415 || r.errCode() != "not_multipart" {
		t.Fatalf("JSON body: %d %s", r.code, r.body)
	}
	if r := e.do("GET", "/api/upload", nil, withCookie(cookie)); r.code != 405 || r.hdr.Get("Allow") != "POST" {
		t.Fatalf("GET /api/upload: %d Allow=%q", r.code, r.hdr.Get("Allow"))
	}
}

// Both upload routes share ONE uploadRate limiter keyed by user id: a capture
// tool cannot spend a second budget by switching endpoints.
func TestCaptureUploadSharesTheRateLimiter(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Cfg.RateUploadPerHour = 1 })
	_, cookie := e.newUser("carol")
	if r := e.upload(`{}`, "a.mp4", testVideo(10), withCookie(cookie)); r.code != 201 {
		t.Fatalf("POST /api/clips: %d %s", r.code, r.body)
	}
	r := e.capture([]part{filePart("file", "a.mp4", testVideo(10))}, withCookie(cookie))
	if r.code != 429 || r.errCode() != "rate_limited" || r.hdr.Get("Retry-After") == "" {
		t.Fatalf("POST /api/upload after the budget was spent: %d %s", r.code, r.body)
	}
	// Keyed on the user, exactly as POST /api/clips: a second account from
	// the same address still has its own budget (the IP bucket beside it is
	// looser and untouched — TestUploadIPRateLimit walks that one).
	_, other := e.newUser("other")
	if r := e.capture([]part{filePart("file", "a.mp4", testVideo(10))}, withCookie(other)); r.code != 201 {
		t.Fatalf("other user: %d %s", r.code, r.body)
	}
}

// A failed capture upload leaves nothing behind — same cleanup as §8.3.
func TestCaptureUploadCleansUpOnProbeReject(t *testing.T) {
	e := newEnv(t)
	u, cookie := e.newUser("carol")
	e.probe = func(context.Context, string) (*transcode.ProbeResult, error) {
		return nil, &transcode.ErrPermanent{Err: errors.New("no video stream")}
	}
	if r := e.capture([]part{filePart("file", "a.mp4", testVideo(1000))}, withCookie(cookie)); r.code != 415 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if keys := e.fake.Keys(); len(keys) != 0 {
		t.Fatalf("objects written for a rejected upload: %v", keys)
	}
	assertScratchUnchanged(t, e.scratch, nil)
	clips, _, err := e.db.ListClips(context.Background(), db.ListQuery{ViewerID: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(clips) != 0 {
		t.Fatalf("clips after a rejected upload: %d", len(clips))
	}
}

// --- clippet.sxcu at mint time (§17; §20 erratum 59) ------------------------

// The mint response carries the finished ShareX file: it is the only moment
// the raw token exists, so this is the only place it can be built.
func TestMintTokenCarriesSXCU(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")
	r := e.doJSON(http.MethodPost, "/api/auth/tokens", `{"name":"ShareX on desktop"}`, withCookie(cookie))
	if r.code != 201 {
		t.Fatalf("mint: %d %s", r.code, r.body)
	}
	raw := r.str("token")
	if !strings.HasPrefix(raw, "ck_") {
		t.Fatalf("raw token: %q", raw)
	}
	sx, ok := r.json["sxcu"].(map[string]any)
	if !ok {
		t.Fatalf("no sxcu object: %s", r.body)
	}
	want := map[string]string{
		"Version":         sxcuVersion,
		"Name":            "Clippet",
		"DestinationType": "FileUploader",
		"RequestMethod":   "POST",
		"RequestURL":      testOrigin + "/api/upload",
		"Body":            "MultipartFormData",
		"FileFormName":    "file",
		"URL":             "{json:url}",
		"ErrorMessage":    "{json:message}",
	}
	for k, v := range want {
		if sx[k] != v {
			t.Errorf("sxcu.%s = %v, want %q", k, sx[k], v)
		}
	}
	if _, extra := sx["ThumbnailURL"]; extra {
		t.Error("sxcu.ThumbnailURL is set: ShareX expects an image there and this API returns HTML pages")
	}
	// ImageUploader would make Clippet the screenshot destination on import,
	// and every screenshot would come back 415 not_a_video (§20 erratum 59).
	if strings.Contains(sx["DestinationType"].(string), "Image") {
		t.Errorf("sxcu.DestinationType = %v claims an image destination", sx["DestinationType"])
	}
	hdrs, _ := sx["Headers"].(map[string]any)
	if hdrs["Authorization"] != "Bearer "+raw {
		t.Errorf("sxcu.Headers.Authorization = %v, want the raw token", hdrs["Authorization"])
	}
	// No Arguments at all: a title={filename} argument would send the name
	// WITH its extension and every card would read "….mp4" (§20 erratum 60).
	if args, present := sx["Arguments"]; present {
		if m, _ := args.(map[string]any); len(m) != 0 {
			t.Errorf("sxcu.Arguments = %v, want absent or empty", args)
		}
	}
	if strings.Contains(string(r.body), "{filename}") {
		t.Errorf("the sxcu still sends a {filename} argument: %s", r.body)
	}
	// FileFormName must be a name POST /api/upload actually accepts.
	if !captureFilePart(sx["FileFormName"].(string)) {
		t.Errorf("sxcu.FileFormName = %v is not accepted by the endpoint", sx["FileFormName"])
	}
	// The file is a working credential end to end, and the request it
	// describes — the file part alone, no title — names the clip from the
	// filename: ShareX's timestamp, as a date and a time (§20 erratum 317(a)).
	r2 := e.capture([]part{
		filePart(sx["FileFormName"].(string), "2026-09-04_18-22-13.mp4", testVideo(1000)),
	}, withHeader("Authorization", hdrs["Authorization"].(string)))
	if r2.code != 201 {
		t.Fatalf("upload with the minted sxcu credential: %d %s", r2.code, r2.body)
	}
	c, err := e.db.GetClip(context.Background(), r2.str("id"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Title != "2026-09-04 18:22" {
		t.Errorf("a ShareX upload is titled %q — the sxcu must not put the extension in the title", c.Title)
	}
	if r2.str("url") != testOrigin+"/"+r2.str("id") {
		t.Fatalf("sxcu URL mapping {json:url} would paste %q", r2.str("url"))
	}
}

// The listing never leaks the raw token or its uploader file.
func TestListTokensHasNoSXCU(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")
	if r := e.doJSON(http.MethodPost, "/api/auth/tokens", `{"name":"t"}`, withCookie(cookie)); r.code != 201 {
		t.Fatalf("mint: %d %s", r.code, r.body)
	}
	r := e.do("GET", "/api/auth/tokens", nil, withCookie(cookie))
	if r.code != 200 {
		t.Fatalf("list: %d %s", r.code, r.body)
	}
	if strings.Contains(string(r.body), "ck_") && strings.Contains(string(r.body), "sxcu") {
		t.Fatalf("listing leaks the raw token or the sxcu: %s", r.body)
	}
	if strings.Contains(string(r.body), `"sxcu"`) || strings.Contains(string(r.body), `"token"`) {
		t.Fatalf("listing carries token/sxcu keys: %s", r.body)
	}
}

// --- the ready wait (§20 errata 97, 212) -----------------------------------

// waitEnv is a capture-upload environment with a ready-wait budget short
// enough that a test never actually spends it.
func waitEnv(t *testing.T, budget time.Duration) *env {
	t.Helper()
	return newEnv(t, func(d *Deps) { d.Cfg.CaptureReadyWait = budget })
}

// With the wait switched off — by the operator or by the request — the 201 is
// the one this route has always written, and the handler does not so much as
// subscribe to the worker.
func TestCaptureUploadNoWait(t *testing.T) {
	e := waitEnv(t, 0)
	_, cookie := e.newUser("carol")
	// Nothing will ever finish this clip, so a handler that waited would hang for
	// the whole budget rather than answer.
	r := e.capture([]part{filePart("file", "a.mp4", testVideo(1000))}, withCookie(cookie))
	if r.code != 201 || r.str("state") != "queued" {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if r.json["card_ready"] != false {
		t.Errorf("card_ready = %v on a queued clip, want false: %s", r.json["card_ready"], r.body)
	}
	if n := e.worker.awaited.Load(); n != 0 {
		t.Errorf("the handler subscribed to the worker %d times with no budget", n)
	}
}

// The feature itself: the wait holds the 201 until the clip's CARD really works,
// and says so — that is what decides whether the pasted link unfurls as a player
// card or as a link-out Discord then caches (§20 erratum 212).
func TestCaptureUploadWaitsForReady(t *testing.T) {
	e := waitEnv(t, 10*time.Second)
	_, cookie := e.newUser("carol")
	failed := make(chan error, 1)
	e.worker.script(func(id string) {
		if err := e.finishTranscode(id); err != nil {
			failed <- err
		}
		e.worker.finish(id)
	})
	r := e.capture([]part{filePart("file", "a.mp4", testVideo(1000))}, withCookie(cookie))
	select {
	case err := <-failed:
		t.Fatalf("the scripted worker failed: %v", err)
	default:
	}
	if r.code != 201 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if r.str("state") != "ready" {
		t.Fatalf("state = %q, want ready — the wait is the whole feature: %s", r.str("state"), r.body)
	}
	// …and the card, which is what erratum 212 made the wait about. `state` is
	// still the CLIP's state and is documented as such.
	if r.json["card_ready"] != true {
		t.Fatalf("card_ready = %v after the wait, want true: %s", r.json["card_ready"], r.body)
	}
	// Nothing else about the body moved (§8.1): the same keys, same URLs.
	id := r.str("id")
	if r.str("url") != testOrigin+"/"+id || r.str("embed_url") != testOrigin+"/embed/"+id {
		t.Fatalf("urls: %s", r.body)
	}
	want := map[string]bool{"id": true, "url": true, "embed_url": true, "kind": true, "state": true,
		"card_ready": true, "expires_at": true, "tiers": true}
	for k := range r.json {
		if !want[k] {
			t.Errorf("unexpected key %q after a wait: %s", k, r.body)
		}
	}
	// `tiers` is the ClipDTO's own list: always three entries, best first, so a
	// capture tool can say "1440p60, card at 720p30" without a second request.
	tiers, ok := r.json["tiers"].([]any)
	if !ok || len(tiers) != 3 {
		t.Fatalf("tiers = %v, want three entries: %s", r.json["tiers"], r.body)
	}
	for i, name := range []string{"original", "full", "light"} {
		row, ok := tiers[i].(map[string]any)
		if !ok || row["tier"] != name {
			t.Fatalf("tiers[%d] = %v, want the %s tier: %s", i, tiers[i], name, r.body)
		}
	}
}

// A budget that runs out is not an error: still 201, still the share link,
// just the state the route always used to report.
func TestCaptureUploadWaitExpires(t *testing.T) {
	e := waitEnv(t, 40*time.Millisecond)
	_, cookie := e.newUser("carol")
	start := time.Now()
	r := e.capture([]part{filePart("file", "a.mp4", testVideo(1000))}, withCookie(cookie))
	elapsed := time.Since(start)
	if r.code != 201 || r.str("state") != "queued" {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if e.worker.awaited.Load() != 1 {
		t.Fatalf("the handler did not wait at all (awaited=%d)", e.worker.awaited.Load())
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the wait ran %s past a 40ms budget", elapsed)
	}
	if r.str("id") == "" || r.json["expires_at"] == nil && !r.has("expires_at") {
		t.Fatalf("body after an expired wait: %s", r.body)
	}
}

// A client that goes away ends the wait at once — it is holding a connection
// outside the TimeoutHandler, and nobody is left to read the answer.
func TestCaptureUploadWaitEndsOnDisconnect(t *testing.T) {
	e := waitEnv(t, time.Minute)
	_, cookie := e.newUser("carol")
	ctx, cancel := context.WithCancel(context.Background())
	e.worker.script(func(string) { cancel() }) // the client vanishes mid-wait
	start := time.Now()
	r := e.capture([]part{filePart("file", "a.mp4", testVideo(1000))},
		withCookie(cookie), func(req *http.Request) { *req = *req.WithContext(ctx) })
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("a cancelled request waited %s of its minute", elapsed)
	}
	cancel()
	if r.code != 201 {
		t.Fatalf("%d %s", r.code, r.body)
	}
}

// The `wait` field is GONE (§20 erratum 212): the owner's decision of 2026-09-08
// is that a user may not set up a breaking behaviour at all. Every value is
// refused, including the one that used to be useful, and the message names the
// operator setting that replaced it — a silent ignore is what would let a tool
// carrying `wait=0` go on believing it had the old behaviour.
func TestCaptureUploadWaitFieldIsRefused(t *testing.T) {
	for _, v := range []string{"0", "8", "9999", "abc", "-1", "8s", "1.5", "١٢"} {
		t.Run("wait="+v, func(t *testing.T) {
			e := waitEnv(t, 30*time.Second)
			_, cookie := e.newUser("carol")
			r := e.capture([]part{
				textPart("wait", v), filePart("file", "a.mp4", testVideo(1000)),
			}, withCookie(cookie))
			if r.code != 400 || r.errCode() != "invalid_request" {
				t.Fatalf("wait=%q → %d %s", v, r.code, r.body)
			}
			if !strings.Contains(r.str("message"), "wait") ||
				!strings.Contains(r.str("message"), "CAPTURE_READY_WAIT") {
				t.Errorf("the message must name the field and its replacement: %s", r.body)
			}
			if n := e.worker.awaited.Load(); n != 0 {
				t.Errorf("a refused request still subscribed to the worker %d times", n)
			}
		})
	}
	// An EMPTY value still means "not sent", like every other capture field: a
	// tool that always emits its arguments must not be told off for a value it
	// did not choose.
	t.Run("empty is not sent", func(t *testing.T) {
		e := waitEnv(t, 0)
		_, cookie := e.newUser("carol")
		r := e.capture([]part{
			textPart("wait", ""), filePart("file", "a.mp4", testVideo(1000)),
		}, withCookie(cookie))
		if r.code != 201 {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})
	// The budget is the operator's alone, clamped at the unit the handler works
	// in: the ceiling still holds for a Config a test assembled by hand.
	t.Run("captureWait", func(t *testing.T) {
		for _, tc := range []struct{ cfg, want time.Duration }{
			{30 * time.Second, 30 * time.Second},
			{0, 0},
			{time.Hour, config.CaptureReadyWaitMax},
			{-time.Second, 0},
		} {
			h := &handler{d: Deps{Cfg: config.Config{CaptureReadyWait: tc.cfg}}}
			if got := h.captureWait(); got != tc.want {
				t.Errorf("CAPTURE_READY_WAIT=%s → %s, want %s", tc.cfg, got, tc.want)
			}
		}
	})
}
