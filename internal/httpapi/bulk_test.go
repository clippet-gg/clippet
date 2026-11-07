package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/lifecycle"
	"clippet/internal/store"
)

// bulkOutcome reads the per-id results of POST /api/clips/bulk.
func bulkOutcome(t *testing.T, r resp) map[string]bulkResultDTO {
	t.Helper()
	var body bulkResponse
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatalf("%v: %s", err, r.body)
	}
	out := make(map[string]bulkResultDTO, len(body.Results))
	for _, res := range body.Results {
		out[res.ID] = res
	}
	return out
}

func wantResult(t *testing.T, got map[string]bulkResultDTO, id string, status int, code string) {
	t.Helper()
	res, ok := got[id]
	if !ok {
		t.Fatalf("no result for %s (got %+v)", id, got)
	}
	if res.Status != status {
		t.Fatalf("%s status = %d, want %d", id, res.Status, status)
	}
	switch {
	case code == "" && res.Error != nil:
		t.Fatalf("%s error = %q, want none", id, *res.Error)
	case code != "" && (res.Error == nil || *res.Error != code):
		t.Fatalf("%s error = %v, want %q", id, res.Error, code)
	}
}

// TestBulkDeleteReportsPerID is the headline contract: one foreign id gets its
// own 403 while every other id is destroyed, and the batch itself is a 200.
func TestBulkDeleteReportsPerID(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	other, _ := e.newUser("other")
	a := e.seedClip(owner, clipOpts{createdAt: 1000})
	b := e.seedClip(owner, clipOpts{createdAt: 1001})
	theirs := e.seedClip(other, clipOpts{createdAt: 1002})
	missing := ids.New()

	r := e.doJSON("POST", "/api/clips/bulk", map[string]any{
		"action": "delete",
		"ids":    []string{a.ID, theirs.ID, b.ID, missing, "not-an-id"},
	}, withCookie(cookie))
	if r.code != 200 {
		t.Fatalf("bulk delete = %d %s", r.code, r.body)
	}
	got := bulkOutcome(t, r)
	wantResult(t, got, a.ID, http.StatusNoContent, "")
	wantResult(t, got, b.ID, http.StatusNoContent, "")
	wantResult(t, got, theirs.ID, http.StatusForbidden, "forbidden")
	wantResult(t, got, missing, http.StatusNotFound, "not_found")
	wantResult(t, got, "not-an-id", http.StatusNotFound, "not_found")
	if ok, _ := r.json["ok"].(float64); ok != 2 {
		t.Fatalf("ok = %v, want 2 (%s)", r.json["ok"], r.body)
	}
	// The nineteen that worked are GONE and the one that did not is untouched.
	for _, id := range []string{a.ID, b.ID} {
		if _, err := e.db.GetClip(context.Background(), id); !errors.Is(err, db.ErrNotFound) {
			t.Fatalf("%s survived a successful bulk delete: %v", id, err)
		}
	}
	if _, err := e.db.GetClip(context.Background(), theirs.ID); err != nil {
		t.Fatalf("a refused id was deleted anyway: %v", err)
	}
	// …and its objects went with it, exactly as the single DELETE's do.
	if n, _ := e.fake.List(context.Background(), store.ClipPrefix(e.cfg.S3.KeyPrefix, a.ID)); len(n) != 0 {
		t.Fatalf("objects left behind: %v", n)
	}
}

// TestBulkDeleteTombstonesWhenTheBudgetIsSpent: one budget for the whole
// request, and an id it does not reach is still gone to every reader — 202,
// the answer the single DELETE already documents.
func TestBulkDeleteTombstonesWhenTheBudgetIsSpent(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.DestroyBudget = time.Nanosecond })
	owner, cookie := e.newUser("owner")
	c := e.seedClip(owner, clipOpts{createdAt: 1000})

	r := e.doJSON("POST", "/api/clips/bulk", map[string]any{
		"action": "delete", "ids": []string{c.ID},
	}, withCookie(cookie))
	if r.code != 200 {
		t.Fatalf("bulk delete = %d %s", r.code, r.body)
	}
	wantResult(t, bulkOutcome(t, r), c.ID, http.StatusAccepted, "")
	after, err := e.db.GetClip(context.Background(), c.ID)
	if err != nil || after.State != db.ClipDeleting {
		t.Fatalf("a deferred bulk delete left the clip at %q (%v); it must be tombstoned", after.State, err)
	}
	// And a tombstone is a 404 to every reader immediately.
	if got := e.do("GET", "/api/clips/"+c.ID, nil); got.code != 404 {
		t.Fatalf("a tombstoned clip answers %d", got.code)
	}
}

// TestBulkDeleteStorageFailureIsStillATombstone: a Destroy that cannot reach
// the bucket must not report success, and must not leave a live row either.
func TestBulkDeleteStorageFailureIsStillATombstone(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c := e.seedClip(owner, clipOpts{createdAt: 1000})
	e.destroy = func(ctx context.Context, id string) (lifecycle.Result, error) {
		if err := e.db.MarkDeleting(ctx, id, e.now()); err != nil {
			return lifecycle.Result{}, err
		}
		return lifecycle.Result{}, errors.New("bucket unreachable")
	}
	r := e.doJSON("POST", "/api/clips/bulk", map[string]any{
		"action": "delete", "ids": []string{c.ID},
	}, withCookie(cookie))
	wantResult(t, bulkOutcome(t, r), c.ID, http.StatusAccepted, "")
	after, err := e.db.GetClip(context.Background(), c.ID)
	if err != nil || after.State != db.ClipDeleting {
		t.Fatalf("state = %q (%v), want deleting", after.State, err)
	}
}

// TestBulkTTL: one request sets the lifetime of a whole selection, and the
// wire values are §8.2's (0 = never).
func TestBulkTTL(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	other, _ := e.newUser("other")
	a := e.seedClip(owner, clipOpts{createdAt: 1000})
	b := e.seedClip(owner, clipOpts{createdAt: 1001})
	theirs := e.seedClip(other, clipOpts{createdAt: 1002})

	r := e.doJSON("POST", "/api/clips/bulk", map[string]any{
		"action": "ttl", "ttl_days": 7, "ids": []string{a.ID, b.ID, theirs.ID},
	}, withCookie(cookie))
	if r.code != 200 {
		t.Fatalf("bulk ttl = %d %s", r.code, r.body)
	}
	got := bulkOutcome(t, r)
	wantResult(t, got, a.ID, http.StatusOK, "")
	wantResult(t, got, b.ID, http.StatusOK, "")
	wantResult(t, got, theirs.ID, http.StatusForbidden, "forbidden")
	for _, id := range []string{a.ID, b.ID} {
		c, err := e.db.GetClip(context.Background(), id)
		if err != nil || c.TTLSeconds == nil || *c.TTLSeconds != 7*86400 || c.ExpiresAt == nil {
			t.Fatalf("%s ttl = %v expires = %v (%v)", id, c.TTLSeconds, c.ExpiresAt, err)
		}
	}
	if c, _ := e.db.GetClip(context.Background(), theirs.ID); c.TTLSeconds != nil {
		t.Fatal("a refused id had its lifetime changed anyway")
	}
	// 0 is "keep forever", and it clears the expiry.
	r = e.doJSON("POST", "/api/clips/bulk", map[string]any{
		"action": "ttl", "ttl_days": 0, "ids": []string{a.ID},
	}, withCookie(cookie))
	wantResult(t, bulkOutcome(t, r), a.ID, http.StatusOK, "")
	if c, _ := e.db.GetClip(context.Background(), a.ID); c.TTLSeconds != nil || c.ExpiresAt != nil {
		t.Fatalf("ttl_days=0 left ttl=%v expires=%v", c.TTLSeconds, c.ExpiresAt)
	}
	// A TTL that is not one of the five, and a missing one, fail the REQUEST:
	// nothing about them is per-id.
	for _, body := range []map[string]any{
		{"action": "ttl", "ttl_days": 3, "ids": []string{a.ID}},
		{"action": "ttl", "ids": []string{a.ID}},
	} {
		r = e.doJSON("POST", "/api/clips/bulk", body, withCookie(cookie))
		if r.code != 400 || r.errCode() != "invalid_ttl" {
			t.Fatalf("%v: %d %s", body, r.code, r.body)
		}
	}
}

// TestBulkTag: one tag ADDED to each clip's set, never replacing it, with the
// clip's own ten-tag cap answered per id.
func TestBulkTag(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner, cookie := e.newUser("owner")
	other, _ := e.newUser("other")
	a := e.seedClip(owner, clipOpts{createdAt: 1000})
	full := e.seedClip(owner, clipOpts{createdAt: 1001})
	theirs := e.seedClip(other, clipOpts{createdAt: 1002})
	if err := e.db.SetClipTags(ctx, a.ID, []string{"gaming"}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetClipTags(ctx, full.ID, []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}); err != nil {
		t.Fatal(err)
	}
	r := e.doJSON("POST", "/api/clips/bulk", map[string]any{
		"action": "tag", "tag": " CLUTCH ", "ids": []string{a.ID, full.ID, theirs.ID},
	}, withCookie(cookie))
	if r.code != 200 {
		t.Fatalf("bulk tag = %d %s", r.code, r.body)
	}
	got := bulkOutcome(t, r)
	wantResult(t, got, a.ID, http.StatusOK, "")
	wantResult(t, got, full.ID, http.StatusBadRequest, "invalid_tags")
	wantResult(t, got, theirs.ID, http.StatusForbidden, "forbidden")
	// The tag it already had is STILL there — this is an add, not a replace.
	tags, err := e.db.TagsOf(ctx, a.ID)
	if err != nil || strings.Join(tags, ",") != "clutch,gaming" {
		t.Fatalf("tags = %v (%v), want the old one and the new one", tags, err)
	}
	// A tag that cannot exist fails the REQUEST and names itself.
	r = e.doJSON("POST", "/api/clips/bulk", map[string]any{
		"action": "tag", "tag": "NOT OK", "ids": []string{a.ID},
	}, withCookie(cookie))
	if r.code != 400 || r.errCode() != "invalid_tags" || !strings.Contains(r.str("message"), "NOT OK") {
		t.Fatalf("an illegal bulk tag: %d %s", r.code, r.body)
	}
}

// TestBulkCollection: an append to a collection the caller owns, refusing a
// stranger's collection with the same 404 every other collection mutation
// gives (erratum 82).
func TestBulkCollection(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner, cookie := e.newUser("owner")
	other, otherCookie := e.newUser("other")
	a := e.seedClip(owner, clipOpts{createdAt: 1000})
	b := e.seedClip(owner, clipOpts{createdAt: 1001})
	theirs := e.seedClip(other, clipOpts{createdAt: 1002})

	r := e.doJSON("POST", "/api/collections", map[string]any{"title": "best of"}, withCookie(cookie))
	if r.code != 201 {
		t.Fatalf("create collection: %d %s", r.code, r.body)
	}
	col := r.str("id")

	r = e.doJSON("POST", "/api/clips/bulk", map[string]any{
		"action": "collection", "collection_id": col, "ids": []string{a.ID, theirs.ID, b.ID},
	}, withCookie(cookie))
	if r.code != 200 {
		t.Fatalf("bulk collection = %d %s", r.code, r.body)
	}
	got := bulkOutcome(t, r)
	wantResult(t, got, a.ID, http.StatusOK, "")
	wantResult(t, got, b.ID, http.StatusOK, "")
	wantResult(t, got, theirs.ID, http.StatusForbidden, "forbidden")
	members, err := e.db.CollectionClips(ctx, col)
	if err != nil || len(members) != 2 || members[0].ID != a.ID || members[1].ID != b.ID {
		t.Fatalf("membership = %+v (%v), want a then b in the order sent", members, err)
	}
	// Re-adding is a no-op, not a duplicate.
	r = e.doJSON("POST", "/api/clips/bulk", map[string]any{
		"action": "collection", "collection_id": col, "ids": []string{a.ID},
	}, withCookie(cookie))
	wantResult(t, bulkOutcome(t, r), a.ID, http.StatusOK, "")
	if members, _ = e.db.CollectionClips(ctx, col); len(members) != 2 {
		t.Fatalf("a re-add changed the membership: %+v", members)
	}
	// A stranger's collection, one that is not there, one that cannot be an
	// id, and one that was never sent are ONE answer — and nothing is written
	// before that answer. The message names the COLLECTION every time: the
	// caller's clips are fine, and "no such clip" would send them after the
	// wrong parameter (§20 erratum 30).
	for _, cid := range []string{col, ids.New(), "not-an-id", ""} {
		cookieToUse := otherCookie
		if cid != col {
			cookieToUse = cookie
		}
		body := map[string]any{"action": "collection", "ids": []string{a.ID}}
		if cid != "" {
			body["collection_id"] = cid
		}
		r = e.doJSON("POST", "/api/clips/bulk", body, withCookie(cookieToUse))
		if r.code != 404 || r.errCode() != "not_found" || r.str("message") != "no such collection" {
			t.Fatalf("collection %q: %d %s", cid, r.code, r.body)
		}
	}
	// Nothing above touched the membership.
	if members, _ = e.db.CollectionClips(ctx, col); len(members) != 2 {
		t.Fatalf("a rejected request changed the membership: %+v", members)
	}
}

// TestBulkRequestShape: the bounds, the vocabulary and the de-duplication.
func TestBulkRequestShape(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c := e.seedClip(owner, clipOpts{createdAt: 1000})

	tooMany := make([]string, MaxBulkIDs+1)
	for i := range tooMany {
		tooMany[i] = ids.New()
	}
	for _, tc := range []struct {
		name string
		body map[string]any
		code string
	}{
		{"no ids", map[string]any{"action": "ttl", "ttl_days": 1, "ids": []string{}}, "invalid_request"},
		{"over the cap", map[string]any{"action": "delete", "ids": tooMany}, "invalid_request"},
		{"unknown action", map[string]any{"action": "burn", "ids": []string{c.ID}}, "invalid_request"},
		{"no action", map[string]any{"ids": []string{c.ID}}, "invalid_request"},
	} {
		r := e.doJSON("POST", "/api/clips/bulk", tc.body, withCookie(cookie))
		if r.code != 400 || r.errCode() != tc.code {
			t.Fatalf("%s: %d %s", tc.name, r.code, r.body)
		}
	}
	// A duplicate id is ONE clip and ONE result.
	r := e.doJSON("POST", "/api/clips/bulk", map[string]any{
		"action": "ttl", "ttl_days": 1, "ids": []string{c.ID, c.ID, c.ID},
	}, withCookie(cookie))
	var body bulkResponse
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Results) != 1 || body.OK != 1 {
		t.Fatalf("duplicates were not collapsed: %s", r.body)
	}
	// Exactly the cap is fine (the last id is real so it does real work).
	atCap := make([]string, MaxBulkIDs)
	for i := range atCap {
		atCap[i] = ids.New()
	}
	atCap[MaxBulkIDs-1] = c.ID
	if r = e.doJSON("POST", "/api/clips/bulk", map[string]any{
		"action": "ttl", "ttl_days": 1, "ids": atCap,
	}, withCookie(cookie)); r.code != 200 {
		t.Fatalf("a request AT the cap: %d %s", r.code, r.body)
	}
}

// TestBulkIsSessionOnly: the owner-mutation posture of every other write
// route — a session, a CSRF-checkable origin, and JSON (§20 erratum 19).
func TestBulkIsSessionOnly(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c := e.seedClip(owner, clipOpts{createdAt: 1000})
	body := map[string]any{"action": "ttl", "ttl_days": 1, "ids": []string{c.ID}}

	if r := e.doJSON("POST", "/api/clips/bulk", body); r.code != 401 {
		t.Fatalf("anonymous bulk = %d %s", r.code, r.body)
	}
	tok := e.newToken(owner, "upload")
	if r := e.doJSON("POST", "/api/clips/bulk", body, withBearer(tok)); r.code != 403 || r.errCode() != "token_not_allowed" {
		t.Fatalf("a capture token bulk = %d %s", r.code, r.body)
	}
	// A cross-origin form post is refused by the CSRF guard.
	if r := e.doJSON("POST", "/api/clips/bulk", body, withCookie(cookie), withHeader("Origin", "https://evil.example")); r.code != 403 {
		t.Fatalf("a cross-origin bulk = %d %s", r.code, r.body)
	}
	// And a non-JSON body cannot reach it at all.
	if r := e.do("POST", "/api/clips/bulk", strings.NewReader("action=delete"),
		withCookie(cookie), withHeader("Content-Type", "application/x-www-form-urlencoded")); r.code != 415 {
		t.Fatalf("a form-encoded bulk = %d %s", r.code, r.body)
	}
	// The path is a POST-only literal: GET is a 405 naming the method.
	if r := e.do("GET", "/api/clips/bulk", nil, withCookie(cookie)); r.code != 405 {
		t.Fatalf("GET /api/clips/bulk = %d", r.code)
	}
}
