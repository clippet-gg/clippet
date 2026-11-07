package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"clippet/internal/db"
	"clippet/internal/lifecycle"
	"clippet/internal/worker"
)

func TestGetClipViewsPrivacy(t *testing.T) {
	e := newEnv(t)
	owner, ownerCookie := e.newUser("owner")
	_, otherCookie := e.newUser("other")
	c, _, _ := e.readyClip(owner, clipOpts{unlisted: true})
	if err := e.db.IncrementViews(context.Background(), c.ID); err != nil {
		t.Fatal(err)
	}

	// Anonymous: 200 (unlisted is still 200 — the link is the auth), no views key.
	r := e.do("GET", "/api/clips/"+c.ID, nil)
	if r.code != 200 || r.has("views") || r.str("state") != "ready" {
		t.Fatalf("anonymous: %d %s", r.code, r.body)
	}
	// Principal-dependent JSON is never stored by a shared cache and is
	// keyed on the credential for a misconfigured one that stores anyway.
	if r.hdr.Get("Cache-Control") != "private, no-store" || r.hdr.Get("Vary") != "Cookie, Authorization" {
		t.Fatalf("clip JSON cache headers: %v", r.hdr)
	}
	if r.json["unlisted"] != true || r.json["poster"].(map[string]any)["width"] != float64(640) || r.str("fps") != "30000/1001" ||
		r.json["duration_ms"] != float64(6000) || r.json["has_audio"] != true || r.json["ready_at"] == nil || r.json["ttl_days"] != float64(0) {
		t.Fatalf("ready dto: %s", r.body)
	}
	// Other user: no views.
	if r = e.do("GET", "/api/clips/"+c.ID, nil, withCookie(otherCookie)); r.code != 200 || r.has("views") {
		t.Fatalf("other user: %d %s", r.code, r.body)
	}
	// Owner: views present.
	if r = e.do("GET", "/api/clips/"+c.ID, nil, withCookie(ownerCookie)); r.code != 200 || r.json["views"] != float64(1) {
		t.Fatalf("owner: %d %s", r.code, r.body)
	}
	if r.hdr.Get("Cache-Control") != "private, no-store" || r.hdr.Get("Vary") != "Cookie, Authorization" {
		t.Fatalf("owner clip JSON cache headers: %v", r.hdr)
	}
	// A Bearer token is an upload credential (§14 errata 19): it identifies
	// the user but never unlocks owner-private data, so the view count stays
	// hidden even on the owner's own clip.
	tok := e.newToken(owner, "upload")
	if r = e.do("GET", "/api/clips/"+c.ID, nil, withBearer(tok)); r.code != 200 || r.has("views") {
		t.Fatalf("owner token must not see private views: %d %s", r.code, r.body)
	}
	// 404s: malformed, unknown, deleting.
	for _, p := range []string{"/api/clips/short", "/api/clips/" + strings.Repeat("Z", 12), "/api/clips/" + e.deletingClip(owner).ID} {
		if r = e.do("GET", p, nil, withCookie(ownerCookie)); r.code != 404 || r.errCode() != "not_found" {
			t.Fatalf("%s: %d %s", p, r.code, r.body)
		}
	}
}

// A Bearer token is an upload-only credential in Phase 1 (§14 errata 19):
// accepted on /me and POST /api/clips, rejected 403 token_not_allowed on
// every other user/owner route so a leaked ShareX token cannot list, edit,
// delete, retry, or download an original.
func TestTokenIsUploadOnlyInPhase1(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	f := e.failedClip(owner, clipOpts{})
	tok := e.newToken(owner, "upload")

	// Accepted: the token identifies the user on /me.
	if r := e.do("GET", "/api/auth/me", nil, withBearer(tok)); r.code != 200 || r.str("via") != "token" {
		t.Fatalf("me via token: %d %s", r.code, r.body)
	}
	// Rejected: every session-only route answers 403 token_not_allowed.
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/clips"},
		{"DELETE", "/api/clips/" + c.ID},
		{"POST", "/api/clips/" + f.ID + "/retry"},
		{"POST", "/api/clips/" + c.ID + "/touch"},
		{"GET", "/api/clips/" + c.ID + "/original"},
	} {
		if r := e.do(tc.method, tc.path, nil, withBearer(tok)); r.code != 403 || r.errCode() != "token_not_allowed" {
			t.Fatalf("%s %s via token: %d %s", tc.method, tc.path, r.code, r.body)
		}
	}
	if r := e.doJSON("PATCH", "/api/clips/"+c.ID, map[string]string{"title": "x"}, withBearer(tok)); r.code != 403 || r.errCode() != "token_not_allowed" {
		t.Fatalf("PATCH via token: %d %s", r.code, r.body)
	}
	// The clip and its data survived the token's destructive attempts.
	if r := e.do("GET", "/api/clips/"+c.ID, nil); r.code != 200 || r.str("title") == "x" {
		t.Fatalf("clip mutated or destroyed by a token request: %d %s", r.code, r.body)
	}
}

func TestListClipsVisibilityAndPagination(t *testing.T) {
	e := newEnv(t)
	a, aCookie := e.newUser("a")
	b, bCookie := e.newUser("b")
	t0 := e.now() - 100
	aListed := e.seedClip(a, clipOpts{createdAt: t0 + 1})
	aUnlisted := e.seedClip(a, clipOpts{unlisted: true, createdAt: t0 + 2})
	bListed := e.seedClip(b, clipOpts{createdAt: t0 + 3})
	bUnlisted := e.seedClip(b, clipOpts{unlisted: true, createdAt: t0 + 4})
	if err := e.db.IncrementViews(context.Background(), aListed.ID); err != nil {
		t.Fatal(err)
	}

	r := e.do("GET", "/api/clips", nil, withCookie(aCookie))
	if r.code != 200 {
		t.Fatalf("list: %d %s", r.code, r.body)
	}
	if r.hdr.Get("Cache-Control") != "private, no-store" || r.hdr.Get("Vary") != "Cookie, Authorization" {
		t.Fatalf("list cache headers: %v", r.hdr)
	}
	clips, _ := r.json["clips"].([]any)
	seen := map[string]map[string]any{}
	for _, c := range clips {
		m := c.(map[string]any)
		seen[m["id"].(string)] = m
	}
	if len(seen) != 3 || seen[bUnlisted.ID] != nil || seen[aUnlisted.ID] == nil || seen[bListed.ID] == nil {
		t.Fatalf("visibility for a: %v", r.body)
	}
	// views only on the caller's own clips.
	if _, ok := seen[aListed.ID]["views"]; !ok || seen[aListed.ID]["views"] != float64(1) {
		t.Fatalf("own clip lacks views: %v", seen[aListed.ID])
	}
	if _, ok := seen[bListed.ID]["views"]; ok {
		t.Fatalf("other's clip shows views: %v", seen[bListed.ID])
	}
	if seen[bListed.ID]["owner"].(map[string]any)["username"] != "b" {
		t.Fatalf("owner: %v", seen[bListed.ID]["owner"])
	}
	if r.json["next_cursor"] != nil {
		t.Fatalf("next_cursor = %v", r.json["next_cursor"])
	}

	// Pagination with limit=1: newest first, cursor chains, ends with null.
	var ids []string
	cursor := ""
	for i := 0; i < 5; i++ {
		p := "/api/clips?limit=1"
		if cursor != "" {
			p += "&cursor=" + cursor
		}
		r = e.do("GET", p, nil, withCookie(bCookie))
		if r.code != 200 {
			t.Fatalf("page %d: %d %s", i, r.code, r.body)
		}
		page := r.json["clips"].([]any)
		if len(page) != 1 {
			t.Fatalf("page %d size %d", i, len(page))
		}
		ids = append(ids, page[0].(map[string]any)["id"].(string))
		if r.json["next_cursor"] == nil {
			break
		}
		cursor = r.str("next_cursor")
	}
	if len(ids) != 3 || ids[0] != bUnlisted.ID || ids[1] != bListed.ID || ids[2] != aListed.ID {
		t.Fatalf("pages for b: %v (want newest first: %s %s %s)", ids, bUnlisted.ID, bListed.ID, aListed.ID)
	}

	// Errors: bad cursor → 400 invalid_cursor; bad limit → 400; anonymous 401.
	if r = e.do("GET", "/api/clips?cursor=%21%21garbage", nil, withCookie(aCookie)); r.code != 400 || r.errCode() != "invalid_cursor" {
		t.Fatalf("garbage cursor: %d %s", r.code, r.body)
	}
	if r = e.do("GET", "/api/clips?limit=x", nil, withCookie(aCookie)); r.code != 400 || r.errCode() != "invalid_request" {
		t.Fatalf("bad limit: %d %s", r.code, r.body)
	}
	if r = e.do("GET", "/api/clips", nil); r.code != 401 {
		t.Fatalf("anonymous list: %d", r.code)
	}
	// Empty library renders [] not null.
	e2 := newEnv(t)
	_, c2 := e2.newUser("lonely")
	if r = e2.do("GET", "/api/clips", nil, withCookie(c2)); r.code != 200 || !strings.Contains(string(r.body), `"clips":[]`) {
		t.Fatalf("empty list: %d %s", r.code, r.body)
	}
}

func TestListClipsClosedDBIs503(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("a")
	_ = e.db.Close()
	r := e.do("GET", "/api/clips", nil, withCookie(cookie))
	if r.code != 503 || r.errCode() != "unavailable" || r.hdr.Get("Retry-After") != "5" {
		t.Fatalf("closed db: %d %s Retry-After=%q", r.code, r.body, r.hdr.Get("Retry-After"))
	}
	// Public GET of a clip with the DB down is 503, never 404.
	r = e.do("GET", "/api/clips/"+strings.Repeat("a", 12), nil)
	if r.code != 503 || r.errCode() != "unavailable" {
		t.Fatalf("get with closed db: %d %s", r.code, r.body)
	}
}

func TestPatchClip(t *testing.T) {
	e := newEnv(t)
	owner, ownerCookie := e.newUser("owner")
	_, otherCookie := e.newUser("other")
	ttl := int64(7 * 86400)
	c := e.seedClip(owner, clipOpts{ttl: &ttl})
	path := "/api/clips/" + c.ID

	// Metadata edits; TTL absent → unchanged.
	r := e.doJSON("PATCH", path, map[string]any{"title": "  New title ", "description": "d2", "unlisted": true}, withCookie(ownerCookie))
	if r.code != 200 || r.str("title") != "New title" || r.str("description") != "d2" || r.json["unlisted"] != true || r.json["ttl_days"] != float64(7) {
		t.Fatalf("patch: %d %s", r.code, r.body)
	}
	if _, ok := r.json["views"]; !ok {
		t.Fatal("owner PATCH response lacks views")
	}
	// TTL null → unchanged; 0 → never; 30 → expires_at ≈ now + 30d; 5 → 400.
	if r = e.doJSON("PATCH", path, `{"ttl_days":null}`, withCookie(ownerCookie)); r.code != 200 || r.json["ttl_days"] != float64(7) {
		t.Fatalf("ttl null: %d %s", r.code, r.body)
	}
	if r = e.doJSON("PATCH", path, `{"ttl_days":0}`, withCookie(ownerCookie)); r.code != 200 || r.json["ttl_days"] != float64(0) || r.json["expires_at"] != nil {
		t.Fatalf("ttl 0: %d %s", r.code, r.body)
	}
	r = e.doJSON("PATCH", path, `{"ttl_days":30}`, withCookie(ownerCookie))
	if r.code != 200 || r.json["ttl_days"] != float64(30) {
		t.Fatalf("ttl 30: %d %s", r.code, r.body)
	}
	exp, _ := time.Parse(time.RFC3339, r.str("expires_at"))
	if d := time.Until(exp) - 30*24*time.Hour; d < -time.Minute || d > time.Minute {
		t.Fatalf("expires_at = %s, want ≈ now+30d", exp)
	}
	for _, body := range []string{`{"ttl_days":5}`, `{"ttl_days":"7"}`, `{"ttl_days":1.5}`} {
		if r = e.doJSON("PATCH", path, body, withCookie(ownerCookie)); r.code != 400 || r.errCode() != "invalid_ttl" {
			t.Fatalf("%s: %d %s", body, r.code, r.body)
		}
	}
	// Validation.
	if r = e.doJSON("PATCH", path, `{"title":""}`, withCookie(ownerCookie)); r.code != 400 || r.errCode() != "invalid_request" {
		t.Fatalf("empty title: %d %s", r.code, r.body)
	}
	if r = e.doJSON("PATCH", path, `{"description":"`+strings.Repeat("x", 2001)+`"}`, withCookie(ownerCookie)); r.code != 400 {
		t.Fatalf("long description: %d", r.code)
	}
	if r = e.do("PATCH", path, strings.NewReader(`{"title":"x"}`), withCookie(ownerCookie)); r.code != 415 {
		t.Fatalf("non-json: %d", r.code)
	}
	// Ownership and existence.
	if r = e.doJSON("PATCH", path, `{"title":"x"}`, withCookie(otherCookie)); r.code != 403 || r.errCode() != "forbidden" {
		t.Fatalf("other user: %d %s", r.code, r.body)
	}
	if r = e.doJSON("PATCH", path, `{"title":"x"}`); r.code != 401 {
		t.Fatalf("anonymous: %d", r.code)
	}
	if r = e.doJSON("PATCH", "/api/clips/"+strings.Repeat("Q", 12), `{"title":"x"}`, withCookie(ownerCookie)); r.code != 404 {
		t.Fatalf("unknown: %d", r.code)
	}
	// CSRF.
	if r = e.doJSON("PATCH", path, `{"title":"x"}`, withCookie(ownerCookie), withHeader("Origin", "https://evil.example")); r.code != 403 || r.errCode() != "csrf" {
		t.Fatalf("csrf: %d %s", r.code, r.body)
	}
}

func TestDeleteClip(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.DestroyBudget = 200 * time.Millisecond })
	owner, ownerCookie := e.newUser("owner")
	_, otherCookie := e.newUser("other")
	c, _, _ := e.readyClip(owner, clipOpts{})
	path := "/api/clips/" + c.ID

	if r := e.do("DELETE", path, nil, withCookie(otherCookie)); r.code != 403 {
		t.Fatalf("other: %d %s", r.code, r.body)
	}
	if r := e.do("DELETE", path, nil); r.code != 401 {
		t.Fatalf("anonymous: %d", r.code)
	}
	if r := e.do("DELETE", path, nil, withCookie(ownerCookie), withHeader("Sec-Fetch-Site", "cross-site")); r.code != 403 || r.errCode() != "csrf" {
		t.Fatalf("csrf: %d %s", r.code, r.body)
	}
	// 204: objects and row gone.
	r := e.do("DELETE", path, nil, withCookie(ownerCookie))
	if r.code != 204 {
		t.Fatalf("delete: %d %s", r.code, r.body)
	}
	if _, err := e.db.GetClip(context.Background(), c.ID); err != db.ErrNotFound {
		t.Fatalf("row after delete: %v", err)
	}
	if got := e.fake.Deletes(); len(got) != 3 {
		t.Fatalf("deleted objects: %v", got)
	}
	for _, k := range e.fake.Keys() {
		if strings.Contains(k, c.ID) {
			t.Fatalf("object survived: %s", k)
		}
	}
	if r = e.do("DELETE", path, nil, withCookie(ownerCookie)); r.code != 404 {
		t.Fatalf("delete again: %d", r.code)
	}

	// 202: the storage step blocks past the budget; the tombstone is set and
	// every public route answers 404 from now on.
	c2, _, _ := e.readyClip(owner, clipOpts{})
	e.fake.Hook = func(ctx context.Context, op, key string) error {
		if op != "DeletePrefix" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	}
	start := time.Now()
	r = e.do("DELETE", "/api/clips/"+c2.ID, nil, withCookie(ownerCookie))
	e.fake.Hook = nil
	if r.code != 202 || r.str("state") != "deleting" {
		t.Fatalf("deferred delete: %d %s", r.code, r.body)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("DELETE exceeded its budget: %s", took)
	}
	if got := e.getClip(c2.ID); got.State != db.ClipDeleting {
		t.Fatalf("state after deferred delete = %s", got.State)
	}
	for _, p := range []string{"", "/video.mp4", "/progress"} {
		if r = e.do("GET", "/api/clips/"+c2.ID+p, nil, withCookie(ownerCookie)); r.code != 404 {
			t.Fatalf("GET %s after tombstone: %d", p, r.code)
		}
	}
	if !strings.Contains(e.logBuf.String(), "level=WARN") || !strings.Contains(e.logBuf.String(), "deferred") {
		t.Fatalf("deferred delete should log at WARN: %s", e.logBuf.String())
	}
	if strings.Contains(e.logBuf.String(), "level=ERROR") {
		t.Fatalf("unexpected ERROR line: %s", e.logBuf.String())
	}
}

func TestRetryClip(t *testing.T) {
	e := newEnv(t)
	owner, ownerCookie := e.newUser("owner")
	_, otherCookie := e.newUser("other")
	failed := e.failedClip(owner, clipOpts{})
	ready, _, _ := e.readyClip(owner, clipOpts{})

	if r := e.do("POST", "/api/clips/"+ready.ID+"/retry", nil, withCookie(ownerCookie)); r.code != 409 || r.errCode() != "not_failed" {
		t.Fatalf("retry ready: %d %s", r.code, r.body)
	}
	if r := e.do("POST", "/api/clips/"+failed.ID+"/retry", nil, withCookie(otherCookie)); r.code != 403 {
		t.Fatalf("retry other: %d", r.code)
	}
	// The failed run left a terminal registry entry (kept 10 minutes): a
	// successful retry must evict it, or /progress keeps answering the old
	// failure while the clip is queued (§8.4 — the poller stops on failed).
	e.worker.progress[failed.ID] = worker.Progress{State: db.ClipFailed, Stage: "encode", Percent: 42, Error: "boom", UpdatedAt: time.Now()}
	woken := e.worker.woken.Load()
	r := e.do("POST", "/api/clips/"+failed.ID+"/retry", nil, withCookie(ownerCookie))
	if r.code != 202 || r.str("state") != "queued" || r.json["failure_reason"] != nil {
		t.Fatalf("retry: %d %s", r.code, r.body)
	}
	if e.worker.woken.Load() != woken+1 {
		t.Fatal("retry did not wake the worker")
	}
	if _, ok := e.worker.progress[failed.ID]; ok {
		t.Fatal("retry left the stale terminal progress entry in the registry")
	}
	if pr := e.do("GET", "/api/clips/"+failed.ID+"/progress", nil); pr.code != 200 || pr.str("state") != db.ClipQueued || pr.json["error"] != nil {
		t.Fatalf("progress after retry: %d %s", pr.code, pr.body)
	}
	job, err := e.db.GetLatestJob(context.Background(), failed.ID)
	if err != nil || job.State != db.JobQueued || job.Attempts != 0 {
		t.Fatalf("job after retry: %+v %v", job, err)
	}
	if r = e.do("POST", "/api/clips/"+strings.Repeat("R", 12)+"/retry", nil, withCookie(ownerCookie)); r.code != 404 {
		t.Fatalf("retry unknown: %d", r.code)
	}
}

// POST /api/clips/{id}/touch (§8.1, §20 erratum 61): the owner's keep-alive.
// One UPDATE, no body, 200 ClipDTO with expires_at = now + ttl_seconds; a
// no-op for a clip kept forever; an owner-session-only route like PATCH.
func TestTouchClip(t *testing.T) {
	e := newEnv(t)
	owner, ownerCookie := e.newUser("owner")
	_, otherCookie := e.newUser("other")
	day := int64(86400)
	week := 7 * day
	// Uploaded four days ago with a 7-day TTL: three days of it are left.
	old := e.now() - 4*day
	c, _, _ := e.readyClip(owner, clipOpts{ttl: &week, createdAt: old})
	before := *e.getClip(c.ID).ExpiresAt

	r := e.do("POST", "/api/clips/"+c.ID+"/touch", nil, withCookie(ownerCookie))
	if r.code != 200 || r.str("id") != c.ID || r.json["ttl_days"] != float64(7) {
		t.Fatalf("touch: %d %s", r.code, r.body)
	}
	after := *e.getClip(c.ID).ExpiresAt
	// NOTHING HERE IS PINNED AT SECONDS SCALE. The row is stamped from the
	// HANDLER's wall clock and e.now() is the harness's latched one, which never
	// goes backwards — and the container clock DOES, by tens of seconds under load
	// (a trap this project recorded: the container clock steps back), so the two can legitimately disagree by more than any tight
	// tolerance. The old `now+week-5` bound flaked once under exactly that. Ten
	// minutes is the same slack the E2E's expect_ttl_ahead carries, for the same
	// reason, and it is three orders of magnitude short of anything this asserts.
	const slack = 600
	if after <= before {
		t.Fatalf("expires_at %d → %d: the touch did not move it", before, after)
	}
	if off := after - (e.now() + week); off > slack || off < -slack {
		t.Fatalf("expires_at %d is %ds off now+7d (%d)", after, off, e.now()+week)
	}
	// …and RECOMPUTED from now rather than EXTENDED, which is the whole promise of
	// the route: the clip is four days old, so a recompute moves the expiry by
	// those four days while an extension would move it by a whole week.
	// It is NOT clock-proof, and saying so was an overstatement: `before` is
	// stamped from the harness's latched clock and `after` from the handler's wall
	// clock, so the jump is 4 days PLUS the same offset between the two that the
	// bound above carries, and it needs the same slack for the same reason. What it
	// adds is the DISTANCE between the two behaviours — four days against seven —
	// which is three orders of magnitude past any jitter, so the failure message is
	// the value: a break here says which of the two the route did (§20 erratum 275).
	if jump := after - before; jump < 4*day-slack || jump > 4*day+slack {
		t.Fatalf("expires_at moved by %ds, want ≈ 4 days (%d): the touch extended the timer instead of recomputing it from now", jump, 4*day)
	}
	if r.str("expires_at") != time.Unix(after, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("expires_at in the DTO = %q, want the row's %d", r.str("expires_at"), after)
	}
	// The response is the owner's DTO, so the private view count is in it.
	if !r.has("views") {
		t.Fatalf("touch answered the owner without the private views key: %s", r.body)
	}
	// Nothing else about the clip moved.
	if got := e.getClip(c.ID); got.State != db.ClipReady || got.Title != c.Title || *got.TTLSeconds != week {
		t.Fatalf("touch changed more than the expiry: %+v", got)
	}

	// A clip kept forever: 200, and expires_at stays null.
	never, _, _ := e.readyClip(owner, clipOpts{})
	r = e.do("POST", "/api/clips/"+never.ID+"/touch", nil, withCookie(ownerCookie))
	if r.code != 200 || r.json["expires_at"] != nil || r.json["ttl_days"] != float64(0) {
		t.Fatalf("touch a never-expiring clip: %d %s", r.code, r.body)
	}
	if got := e.getClip(never.ID); got.ExpiresAt != nil {
		t.Fatalf("a no-op touch set expires_at: %v", got.ExpiresAt)
	}

	// Every non-deleting state is touchable: the TTL of a clip still in the
	// queue is as real as a ready one's.
	queued := e.seedClip(owner, clipOpts{ttl: &week, createdAt: old})
	if r := e.do("POST", "/api/clips/"+queued.ID+"/touch", nil, withCookie(ownerCookie)); r.code != 200 || r.str("state") != db.ClipQueued {
		t.Fatalf("touch a queued clip: %d %s", r.code, r.body)
	}

	// Auth: anonymous 401, a stranger 403, an unknown or tombstoned clip 404
	// (before the ownership check, §8).
	if r := e.do("POST", "/api/clips/"+c.ID+"/touch", nil); r.code != 401 || r.errCode() != "unauthorized" {
		t.Fatalf("anonymous touch: %d %s", r.code, r.body)
	}
	if r := e.do("POST", "/api/clips/"+c.ID+"/touch", nil, withCookie(otherCookie)); r.code != 403 || r.errCode() != "forbidden" {
		t.Fatalf("stranger touch: %d %s", r.code, r.body)
	}
	for _, id := range []string{"short", strings.Repeat("Q", 12), e.deletingClip(owner).ID} {
		if r := e.do("POST", "/api/clips/"+id+"/touch", nil, withCookie(ownerCookie)); r.code != 404 || r.errCode() != "not_found" {
			t.Fatalf("touch %s: %d %s", id, r.code, r.body)
		}
	}
	// Wrong method on a known path is the usual 405 + Allow.
	if r := e.do("GET", "/api/clips/"+c.ID+"/touch", nil, withCookie(ownerCookie)); r.code != 405 || r.hdr.Get("Allow") != "POST" {
		t.Fatalf("GET touch: %d Allow=%q", r.code, r.hdr.Get("Allow"))
	}
	// A cross-site cookie POST is CSRF-rejected like every other mutation.
	if r := e.do("POST", "/api/clips/"+c.ID+"/touch", nil, withCookie(ownerCookie), withHeader("Origin", "https://evil.example")); r.code != 403 || r.errCode() != "csrf" {
		t.Fatalf("cross-origin touch: %d %s", r.code, r.body)
	}
	// A clip rescued from its own expiry: the tombstone has not been written
	// yet, so the touch lands and the sweeper's next look finds it healthy
	// (erratum 15 re-verifies expiry inside the tombstone UPDATE).
	if err := e.db.ForceExpire(context.Background(), c.ID, e.now()-7200); err != nil {
		t.Fatal(err)
	}
	if r := e.do("POST", "/api/clips/"+c.ID+"/touch", nil, withCookie(ownerCookie)); r.code != 200 {
		t.Fatalf("touch an expired clip: %d %s", r.code, r.body)
	}
	if got := *e.getClip(c.ID).ExpiresAt; got <= e.now() {
		t.Fatalf("a rescued clip is still expired: %d", got)
	}
}

func TestDeleteClipFailsBeforeTombstone(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	// Destroy fails before MarkDeleting (a DB error): no tombstone, so a 202
	// would be a lie; the answer is 503 and the clip is untouched.
	e.destroy = func(context.Context, string) (lifecycle.Result, error) {
		return lifecycle.Result{}, errors.New("db: begin: locked")
	}
	r := e.do("DELETE", "/api/clips/"+c.ID, nil, withCookie(cookie))
	if r.code != 503 || r.errCode() != "unavailable" {
		t.Fatalf("delete without tombstone: %d %s", r.code, r.body)
	}
	if got := e.getClip(c.ID); got.State != db.ClipReady {
		t.Fatalf("state = %s", got.State)
	}
	// Destroy errors after the row is already gone (say a concurrent sweep
	// finished it): the post-check finds no row, so the answer is 204.
	e.destroy = func(ctx context.Context, id string) (lifecycle.Result, error) {
		if err := e.db.MarkDeleting(ctx, id, e.now()); err != nil {
			return lifecycle.Result{}, err
		}
		if err := e.db.DeleteClipRow(ctx, id); err != nil {
			return lifecycle.Result{}, err
		}
		return lifecycle.Result{}, errors.New("scratch: remove failed")
	}
	if r = e.do("DELETE", "/api/clips/"+c.ID, nil, withCookie(cookie)); r.code != 204 {
		t.Fatalf("delete with row gone: %d %s", r.code, r.body)
	}
	// And a clip that no longer exists is 404 before Destroy is consulted.
	if r = e.do("DELETE", "/api/clips/"+c.ID, nil, withCookie(cookie)); r.code != 404 {
		t.Fatalf("delete gone clip: %d %s", r.code, r.body)
	}
}
