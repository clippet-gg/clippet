package httpapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"clippet/internal/auth"
	"clippet/internal/db"
	"clippet/internal/ids"
)

// POST /in/{token}, the inbound URL (§20 erratum 322).

// newInbound inserts an inbound row for u straight into the database and
// returns its raw secret. Other scope sets build the rows the route must
// refuse.
func (e *env) newInbound(u db.User, scopes ...string) string {
	e.t.Helper()
	if len(scopes) == 0 {
		scopes = []string{auth.ScopeInbound}
	}
	raw := ids.InboundToken()
	err := e.db.CreateToken(context.Background(), db.APIToken{ID: ids.New(), UserID: u.ID, Name: "in",
		TokenHash: auth.HashToken(raw), Prefix: auth.TokenPrefixOf(raw), Scopes: scopes, CreatedAt: e.now()})
	if err != nil {
		e.t.Fatal(err)
	}
	return raw
}

// inbound posts a capture body to /in/<raw>.
func (e *env) inbound(raw string, parts []part, opts ...reqOpt) resp {
	e.t.Helper()
	body, ct := multipartBody(e.t, parts...)
	return e.do(http.MethodPost, "/in/"+raw, body, append([]reqOpt{withHeader("Content-Type", ct)}, opts...)...)
}

func TestCleanScopesInbound(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want []string
	}{
		{[]string{"inbound"}, []string{"inbound"}},
		{[]string{"inbound", "inbound"}, []string{"inbound"}},
		{[]string{"read", "upload"}, []string{"upload", "read"}},
		{nil, []string{"upload"}},
	} {
		got, err := CleanScopes(tc.in)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("CleanScopes(%v) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range [][]string{{"inbound", "upload"}, {"read", "inbound"}, {"inbound", ""}, {}, {"Inbound"}} {
		if got, err := CleanScopes(bad); err == nil {
			t.Errorf("CleanScopes(%v) = %v, want an error", bad, got)
		}
	}
	if auth.ValidScope(auth.ScopeInbound) || slices.Contains(auth.Scopes, auth.ScopeInbound) {
		t.Error("inbound leaked into the Bearer scope set")
	}
}

// The browser mint: `["inbound"]` alone is an inbound URL (token ci_…, url the
// whole address on PUBLIC_BASE_URL — never the request's Host — and sxcu
// null); `url` is absent for every other token; a mixed set is the 400 with the
// settled sentence.
func TestCreateTokenInbound(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")
	r := e.doJSON(http.MethodPost, "/api/auth/tokens", map[string]any{"name": "Stream deck", "scopes": []string{"inbound"}},
		withCookie(cookie), func(r *http.Request) { r.Host = "evil.example" })
	if r.code != 201 {
		t.Fatalf("mint: %d %s", r.code, r.body)
	}
	raw := r.str("token")
	if !ids.IsInboundToken(raw) || r.str("url") != testOrigin+"/in/"+raw || r.json["sxcu"] != nil || !r.has("sxcu") {
		t.Fatalf("inbound mint: %s", r.body)
	}
	if r.str("prefix") != raw[3:11] || !reflect.DeepEqual(r.json["scopes"], []any{"inbound"}) {
		t.Fatalf("row: %s", r.body)
	}

	r = e.doJSON(http.MethodPost, "/api/auth/tokens", map[string]any{"name": "ShareX"}, withCookie(cookie))
	if r.code != 201 || r.has("url") || !strings.HasPrefix(r.str("token"), "ck_") {
		t.Fatalf("a capture token must carry no url: %d %s", r.code, r.body)
	}
	for _, mixed := range [][]string{{"inbound", "upload"}, {"read", "inbound"}} {
		r = e.doJSON(http.MethodPost, "/api/auth/tokens", map[string]any{"name": "x", "scopes": mixed}, withCookie(cookie))
		if r.code != 400 || r.str("message") != ScopesRule || ScopesRule != "scopes must be a non-empty subset of upload, read; or inbound alone; or autofetch alone" {
			t.Fatalf("mixed %v: %d %s", mixed, r.code, r.body)
		}
	}
}

// Every miss is POST /api/nope byte for byte: the status, every header and
// the body — for a small body, a declared length past the close threshold (the
// real case: a file posted to a revoked URL) and a chunked one. A session
// cookie and a Bearer token riding along change nothing.
func TestInboundMissIsAMissingRoute(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	u, cookie := e.newUser("carol")
	revoked := e.newInbound(u)
	toks, _ := e.db.ListTokens(ctx, u.ID)
	if err := e.db.RevokeToken(ctx, u.ID, toks[0].ID, e.now()); err != nil {
		t.Fatal(err)
	}
	doomed, _ := e.newUser("doomed")
	orphan := e.newInbound(doomed)
	if _, err := e.db.PruneUsers(ctx, "doomed", e.now()+10); err != nil {
		t.Fatal(err)
	}
	bearer := e.newToken(u, "upload")
	misses := map[string]string{
		"bad shape":         "ci_short",
		"too long":          ids.InboundToken() + "A",
		"unknown":           ids.InboundToken(),
		"revoked":           revoked,
		"capture token":     bearer,
		"read token":        e.newToken(u, "read"),
		"deleted user":      orphan,
		"mixed row":         e.newInbound(u, "inbound", "upload"),
		"inbound body, ck_": "ck_" + ids.InboundToken()[3:],
		"percent-encoded":   "ci_%41" + strings.Repeat("A", 42),
	}
	bodies := map[string]func() (io.Reader, reqOpt){
		"small": func() (io.Reader, reqOpt) {
			return strings.NewReader("--x--"), func(*http.Request) {}
		},
		"large": func() (io.Reader, reqOpt) {
			return bytes.NewReader(make([]byte, 300<<10)), func(*http.Request) {}
		},
		"chunked": func() (io.Reader, reqOpt) {
			return io.NopCloser(strings.NewReader("--x--")), func(r *http.Request) {
				r.ContentLength = -1
				r.TransferEncoding = []string{"chunked"}
			}
		},
	}
	for bname, mk := range bodies {
		body, opt := mk()
		want := e.do(http.MethodPost, "/api/nope", body, withHeader("Content-Type", "multipart/form-data; boundary=x"), opt)
		if want.code != 404 || want.str("message") != "no such route" {
			t.Fatalf("the catch-all itself: %d %s", want.code, want.body)
		}
		for name, raw := range misses {
			body, opt := mk()
			got := e.do(http.MethodPost, "/in/"+raw, body, withHeader("Content-Type", "multipart/form-data; boundary=x"),
				withCookie(cookie), withBearer(bearer), opt)
			if got.code != want.code || !bytes.Equal(got.body, want.body) || !reflect.DeepEqual(got.hdr, want.hdr) {
				t.Errorf("%s / %s body: got %d %v %s; want %d %v %s", name, bname, got.code, got.hdr, got.body,
					want.code, want.hdr, want.body)
			}
			if got.hdr.Get("Connection") != "" {
				t.Errorf("%s / %s body: a miss says Connection: %q", name, bname, got.hdr.Get("Connection"))
			}
		}
	}
	// No clip came of any of it.
	if used, _ := e.db.StorageUsed(ctx); used != 0 || e.worker.woken.Load() != 0 {
		t.Fatalf("a miss stored %d bytes / woke the worker", used)
	}
	// Other methods answer the route's 405 alike, whatever the value.
	for _, raw := range []string{ids.InboundToken(), "nope"} {
		r := e.do(http.MethodGet, "/in/"+raw, nil)
		if raw != "nope" && (r.code != 405 || r.hdr.Get("Allow") != "POST") {
			t.Errorf("GET /in/%s: %d %v", raw, r.code, r.hdr)
		}
		if raw == "nope" && r.code != 405 {
			t.Errorf("GET /in/nope: %d, want the same 405", r.code)
		}
	}
}

// A hit is POST /api/upload's ingest: 201 and the share link as one line of
// text by default, the UploadDTO with Accept naming application/json, the clip
// owned by the URL's account whatever cookie or Bearer token rides along, and
// last_used_at written.
func TestInboundHit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	u, _ := e.newUser("carol")
	other, otherCookie := e.newUser("other")
	raw := e.newInbound(u)
	video := testVideo(4000)

	r := e.inbound(raw, []part{filePart("file", "clip.mp4", video)},
		withCookie(otherCookie), withBearer(e.newToken(other, "upload")), withHeader("Accept", "*/*"))
	if r.code != 201 || r.hdr.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("text hit: %d %v %s", r.code, r.hdr, r.body)
	}
	line := string(r.body)
	if !strings.HasPrefix(line, testOrigin+"/") || !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
		t.Fatalf("the body must be the share URL and a newline: %q", line)
	}
	id := strings.TrimSuffix(strings.TrimPrefix(line, testOrigin+"/"), "\n")
	if c := e.getClip(id); c.OwnerID != u.ID {
		t.Fatalf("the clip belongs to the URL's account: %+v", c)
	}

	r = e.inbound(raw, []part{textPart("title", "From the deck"), filePart("file", "b.mp4", video)},
		withHeader("Accept", "text/html, application/json;q=0.9"))
	if r.code != 201 || r.hdr.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("json hit: %d %s", r.code, r.body)
	}
	want := map[string]bool{"id": true, "url": true, "embed_url": true, "kind": true, "state": true, "card_ready": true, "expires_at": true, "tiers": true}
	for k := range r.json {
		if !want[k] {
			t.Errorf("unexpected key %q: %s", k, r.body)
		}
	}
	if len(r.json) != len(want) || r.str("url") != testOrigin+"/"+r.str("id") {
		t.Errorf("the JSON answer is not /api/upload's: %s", r.body)
	}
	if c := e.getClip(r.str("id")); c.Title != "From the deck" || c.OwnerID != u.ID {
		t.Fatalf("title/owner: %+v", c)
	}
	toks, _ := e.db.ListTokens(ctx, u.ID)
	if len(toks) != 1 || toks[0].LastUsedAt == nil {
		t.Fatalf("last_used_at not written: %+v", toks)
	}
	// Refusals past the credential are /api/upload's own JSON envelope.
	r = e.inbound(raw, []part{textPart("title", "no file")})
	if r.code != 400 || r.errCode() != "missing_video" {
		t.Fatalf("no file part: %d %s", r.code, r.body)
	}
}

// The limiters run AFTER the credential, and so does the last_used_at write:
// the account's bucket refuses its own URL's second upload without a database
// write, and a miss from the same address is still the plain 404.
func TestInboundRateLimitIsAfterTheCredential(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Cfg.RateUploadPerHour = 1 })
	ctx := context.Background()
	u, _ := e.newUser("carol")
	raw := e.newInbound(u)
	video := testVideo(1000)
	if r := e.inbound(raw, []part{filePart("file", "a.mp4", video)}); r.code != 201 {
		t.Fatalf("first: %d %s", r.code, r.body)
	}
	toks, _ := e.db.ListTokens(ctx, u.ID)
	if err := e.db.TouchToken(ctx, toks[0].ID, 1); err != nil {
		t.Fatal(err)
	}
	if r := e.inbound(raw, []part{filePart("file", "b.mp4", video)}); r.code != 429 {
		t.Fatalf("second: %d %s", r.code, r.body)
	}
	toks, _ = e.db.ListTokens(ctx, u.ID)
	if toks[0].LastUsedAt == nil || *toks[0].LastUsedAt != 1 {
		t.Errorf("a 429 wrote last_used_at: %v", toks[0].LastUsedAt)
	}
	if r := e.inbound(ids.InboundToken(), []part{filePart("file", "c.mp4", video)}); r.code != 404 || r.str("message") != "no such route" {
		t.Fatalf("a miss behind a tripped bucket: %d %s", r.code, r.body)
	}
}

// The quota's 507 is answered past the credential, exactly as on /api/upload.
func TestInboundQuota(t *testing.T) {
	e := quotaEnv(t, 4000)
	u, _ := e.newUser("carol")
	raw := e.newInbound(u)
	video := testVideo(4000)
	if r := e.inbound(raw, []part{filePart("file", "a.mp4", video)}); r.code != 201 {
		t.Fatalf("first: %d %s", r.code, r.body)
	}
	if r := e.inbound(raw, []part{filePart("file", "b.mp4", video)}); r.code != 507 || r.errCode() != "storage_quota" {
		t.Fatalf("past the line: %d %s", r.code, r.body)
	}
}

// The access log carries the row id, never the secret.
func TestInboundAccessLog(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser("carol")
	raw := e.newInbound(u)
	body, ct := multipartBody(t, filePart("file", "a.mp4", testVideo(1000)))
	r, logs := e.logged(http.MethodPost, "/in/"+raw, body, withHeader("Content-Type", ct))
	if r.code != 201 || strings.Contains(logs, raw) || strings.Contains(logs, raw[3:]) ||
		!strings.Contains(logs, "/in/{token}") || !strings.Contains(logs, "inbound=") {
		t.Fatalf("%d\n%s", r.code, logs)
	}
}

// acceptsJSON: a header that NAMES application/json, at any quality but zero
// (run 8: `q=0` is RFC 9110's "not acceptable").
func TestAcceptsJSON(t *testing.T) {
	for accept, want := range map[string]bool{
		"application/json":                       true,
		"text/plain, application/json;q=0.5":     true,
		"application/json; q=1":                  true,
		"application/json;q=0":                   false,
		"application/json; q=0.000":              false,
		"text/plain, application/json;q=0.0":     false,
		"*/*":                                    false,
		"application/*":                          false,
		"":                                       false,
		"application/json;q=0, application/json": true,
	} {
		r := httptest.NewRequest(http.MethodPost, "/in/x", nil)
		if accept != "" {
			r.Header.Set("Accept", accept)
		}
		if got := acceptsJSON(r); got != want {
			t.Errorf("Accept %q: %v, want %v", accept, got, want)
		}
	}
}

// ScopesRule is the reviewed sentence (the review's COPY 5), built from
// auth.Scopes (run 8).
func TestScopesRuleWording(t *testing.T) {
	if want := "scopes must be a non-empty subset of upload, read; or inbound alone; or autofetch alone"; ScopesRule != want {
		t.Fatalf("ScopesRule = %q, want %q", ScopesRule, want)
	}
}
