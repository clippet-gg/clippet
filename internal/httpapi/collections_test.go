package httpapi

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"clippet/internal/db"
	"clippet/internal/ids"
)

func collectionDTO(t *testing.T, r resp) CollectionDTO {
	t.Helper()
	var out CollectionDTO
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("decode CollectionDTO: %v (%s)", err, r.body)
	}
	return out
}

// collectionMembers dereferences CollectionDTO.Clips, which GET
// /api/collections/{cid} always sets and the shell routes always leave nil
// (§20 erratum 82, amended). Reading it through the helper means a route that silently
// stopped answering with `clips` fails here rather than reading as "empty".
func collectionMembers(t *testing.T, dto CollectionDTO) []ClipDTO {
	t.Helper()
	if dto.Clips == nil {
		t.Fatalf("the body carried no `clips` key at all: %+v", dto)
	}
	return *dto.Clips
}

// newCollection creates one through the API and returns its id.
func (e *env) newCollection(title string, opts ...reqOpt) string {
	e.t.Helper()
	r := e.doJSON("POST", "/api/collections", map[string]any{"title": title}, opts...)
	if r.code != 201 {
		e.t.Fatalf("POST /api/collections: %d %s", r.code, r.body)
	}
	return r.str("id")
}

// TestCollectionCreateAndList: 201 with the public URL, the owner's own list,
// the title rules and the cap's own error code.
func TestCollectionCreateAndList(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("owner")
	_, otherCookie := e.newUser("other")

	r := e.doJSON("POST", "/api/collections", map[string]any{"title": "  Best of 2026  "}, withCookie(cookie))
	if r.code != 201 {
		t.Fatalf("create: %d %s", r.code, r.body)
	}
	dto := collectionDTO(t, r)
	if dto.Title != "Best of 2026" || dto.ClipCount != 0 || dto.Clips != nil {
		t.Fatalf("created dto = %+v", dto)
	}
	if dto.URL != testOrigin+"/c/"+dto.ID {
		t.Fatalf("url = %q, want %s/c/%s", dto.URL, testOrigin, dto.ID)
	}
	if !validID(dto.ID) {
		t.Fatalf("id %q is not a §7 slug", dto.ID)
	}

	// A blank or over-long title is a 400 naming the field.
	for _, bad := range []any{"", "   ", strings.Repeat("x", 201), nil} {
		if r := e.doJSON("POST", "/api/collections", map[string]any{"title": bad}, withCookie(cookie)); r.code != 400 ||
			r.errCode() != "invalid_request" {
			t.Errorf("title %v: %d %s", bad, r.code, r.body)
		}
	}

	// The list is the caller's own, newest first, and never a stranger's.
	e.newCollection("Second", withCookie(cookie))
	e.newCollection("Theirs", withCookie(otherCookie))
	r = e.do("GET", "/api/collections", nil, withCookie(cookie))
	if r.code != 200 {
		t.Fatalf("list: %d %s", r.code, r.body)
	}
	var list struct {
		Collections []CollectionDTO `json:"collections"`
	}
	if err := json.Unmarshal(r.body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Collections) != 2 {
		t.Fatalf("list = %d collections, want 2: %s", len(list.Collections), r.body)
	}
	for _, c := range list.Collections {
		if c.Title == "Theirs" {
			t.Fatal("the list leaked another user's collection")
		}
		if c.Clips != nil {
			t.Errorf("the list route must not embed clips: %+v", c)
		}
	}
	// Principal-dependent JSON is never stored by a shared cache.
	if r.hdr.Get("Cache-Control") != "private, no-store" || r.hdr.Get("Vary") != "Cookie, Authorization" {
		t.Errorf("list cache headers: %v", r.hdr)
	}
}

// TestCollectionRoutesAreSessionOnly is §14 erratum 19 applied to the new
// routes: a capture token uploads and does nothing else.
func TestCollectionRoutesAreSessionOnly(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	tok := e.newToken(owner, "upload")
	cid := e.newCollection("Mine", withCookie(cookie))

	cases := []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/collections", map[string]any{"title": "x"}},
		{"GET", "/api/collections", nil},
		{"PATCH", "/api/collections/" + cid, map[string]any{"title": "x"}},
		{"DELETE", "/api/collections/" + cid, nil},
		{"PUT", "/api/collections/" + cid + "/clips", map[string]any{"clip_ids": []string{}}},
	}
	for _, c := range cases {
		r := e.doJSON(c.method, c.path, c.body, withBearer(tok))
		if r.code != 403 || r.errCode() != "token_not_allowed" {
			t.Errorf("%s %s with a token: %d %s", c.method, c.path, r.code, r.body)
		}
		r = e.doJSON(c.method, c.path, c.body)
		if r.code != 401 || r.errCode() != "unauthorized" {
			t.Errorf("%s %s anonymous: %d %s", c.method, c.path, r.code, r.body)
		}
	}
	// The single GET is the exception: it is public, like GET /api/clips/{id}.
	if r := e.do("GET", "/api/collections/"+cid, nil); r.code != 200 {
		t.Errorf("anonymous GET one: %d %s", r.code, r.body)
	}
	if r := e.do("GET", "/api/collections/"+cid, nil, withBearer(tok)); r.code != 200 {
		t.Errorf("token GET one: %d %s", r.code, r.body)
	}
}

// TestCollectionOwnerVsNotFound: the mutating routes scope by owner, so "not
// there" and "not yours" are one answer — 404, from one query.
func TestCollectionOwnerVsNotFound(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("owner")
	_, otherCookie := e.newUser("other")
	cid := e.newCollection("Mine", withCookie(cookie))

	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"PATCH", "/api/collections/" + cid, map[string]any{"title": "stolen"}},
		{"DELETE", "/api/collections/" + cid, nil},
		{"PUT", "/api/collections/" + cid + "/clips", map[string]any{"clip_ids": []string{}}},
	} {
		r := e.doJSON(c.method, c.path, c.body, withCookie(otherCookie))
		if r.code != 404 || r.errCode() != "not_found" {
			t.Errorf("%s by a stranger: %d %s", c.method, r.code, r.body)
		}
	}
	// Unchanged.
	if r := e.do("GET", "/api/collections/"+cid, nil); collectionDTO(t, r).Title != "Mine" {
		t.Fatalf("a stranger's PATCH landed: %s", r.body)
	}
	// A malformed id is a 404 before any DB access (§7).
	for _, bad := range []string{"nope", "toolongtobeanid", "AAAAAAAAAAA!"} {
		if r := e.do("GET", "/api/collections/"+bad, nil); r.code != 404 {
			t.Errorf("GET /api/collections/%s: %d", bad, r.code)
		}
	}
	if r := e.do("GET", "/api/collections/AAAAAAAAAAAA", nil); r.code != 404 || r.errCode() != "not_found" {
		t.Errorf("GET an unknown collection: %d %s", r.code, r.body)
	}
}

// TestCollectionMembership is the PUT contract end to end: order, duplicates,
// ownership, the cap, and what the public GET then shows.
func TestCollectionMembership(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	other, otherCookie := e.newUser("other")
	cid := e.newCollection("Best of", withCookie(cookie))

	// The ready clips are built FIRST: readyClip claims the oldest queued job,
	// so a clip left queued must not exist yet when another is finalized.
	a, _, _ := e.readyClip(owner, clipOpts{title: "Alpha", createdAt: 1000})
	c, _, _ := e.readyClip(owner, clipOpts{title: "Charlie", createdAt: 1002})
	b := e.seedClip(owner, clipOpts{title: "Bravo", createdAt: 1001})
	theirs := e.seedClip(other, clipOpts{title: "Theirs", createdAt: 1003})

	put := func(ids []string, opts ...reqOpt) resp {
		return e.doJSON("PUT", "/api/collections/"+cid+"/clips", map[string]any{"clip_ids": ids},
			append([]reqOpt{withCookie(cookie)}, opts...)...)
	}

	// The order sent is the order stored — and it is not the library's.
	r := put([]string{c.ID, a.ID, b.ID})
	if r.code != 200 || collectionDTO(t, r).ClipCount != 3 {
		t.Fatalf("PUT: %d %s", r.code, r.body)
	}
	r = e.do("GET", "/api/collections/"+cid, nil)
	dto := collectionDTO(t, r)
	mem := collectionMembers(t, dto)
	if len(mem) != 3 || mem[0].ID != c.ID || mem[1].ID != a.ID || mem[2].ID != b.ID {
		t.Fatalf("order = %+v", mem)
	}
	// A processing clip stays in the list, as itself.
	if mem[2].State != db.ClipQueued {
		t.Errorf("the queued member reads %q", mem[2].State)
	}
	// The embedded clips are PUBLIC DTOs — no view count, even for the owner.
	r = e.do("GET", "/api/collections/"+cid, nil, withCookie(cookie))
	if strings.Contains(string(r.body), `"views"`) {
		t.Fatalf("the owner's own collection leaked a view count: %s", r.body)
	}

	// Duplicates collapse keeping the first position.
	r = put([]string{b.ID, a.ID, b.ID})
	if r.code != 200 || collectionDTO(t, r).ClipCount != 2 {
		t.Fatalf("dupes: %d %s", r.code, r.body)
	}
	dto = collectionDTO(t, e.do("GET", "/api/collections/"+cid, nil))
	mem = collectionMembers(t, dto)
	if len(mem) != 2 || mem[0].ID != b.ID || mem[1].ID != a.ID {
		t.Fatalf("after dupes = %+v", mem)
	}

	// A stranger's clip, an unknown id and a malformed one are ONE answer.
	for _, bad := range []string{theirs.ID, "AAAAAAAAAAAA", "not-an-id"} {
		r = put([]string{a.ID, bad})
		if r.code != 400 || r.errCode() != "not_your_clip" || !strings.Contains(r.str("message"), truncateID(bad)) {
			t.Errorf("PUT with %q: %d %s", bad, r.code, r.body)
		}
	}
	// …and none of them changed the membership.
	dto = collectionDTO(t, e.do("GET", "/api/collections/"+cid, nil))
	mem = collectionMembers(t, dto)
	if len(mem) != 2 || mem[0].ID != b.ID {
		t.Fatalf("a rejected PUT changed the membership: %+v", mem)
	}

	// A body with NO clip_ids is a 400, not a silent wipe: `{}` and
	// `{"clip_ids":null}` mean "I sent no membership", and PATCH on the
	// sibling route reads an absent key as "unchanged" (§20 erratum 82).
	for _, body := range []map[string]any{{}, {"clip_ids": nil}, {"title": "wrong route"}} {
		r = e.doJSON("PUT", "/api/collections/"+cid+"/clips", body, withCookie(cookie))
		if r.code != 400 || r.errCode() != "invalid_request" || !strings.Contains(r.str("message"), "clip_ids is required") {
			t.Errorf("PUT %v: %d %s", body, r.code, r.body)
		}
	}
	dto = collectionDTO(t, e.do("GET", "/api/collections/"+cid, nil))
	if mem = collectionMembers(t, dto); len(mem) != 2 {
		t.Fatalf("a keyless PUT cleared the membership: %+v", mem)
	}

	// The cap.
	tooMany := make([]string, db.MaxClipsPerCollection+1)
	for i := range tooMany {
		tooMany[i] = a.ID
	}
	if r = put(tooMany); r.code != 400 || r.errCode() != "invalid_request" {
		t.Fatalf("PUT past the cap: %d %s", r.code, r.body)
	}
	// The message NAMES the cap, and names the constant's value rather than a
	// literal: the two write paths used to spell the same sentence twice, so
	// moving db.MaxClipsPerCollection would have left one of them lying (N067).
	if want := "at most " + strconv.Itoa(db.MaxClipsPerCollection) + " clips"; !strings.Contains(string(r.body), want) {
		t.Fatalf("the PUT's refusal does not name the cap (%q): %s", want, r.body)
	}

	// Emptying is a PUT of [].
	if r = put([]string{}); r.code != 200 || collectionDTO(t, r).ClipCount != 0 {
		t.Fatalf("empty PUT: %d %s", r.code, r.body)
	}
	// An EMPTY collection still answers WITH the key: `"clips":[]`, never a
	// body that omits it the way a shell does (§8.1, §20 erratum 82).
	r = e.do("GET", "/api/collections/"+cid, nil)
	dto = collectionDTO(t, r)
	if mem = collectionMembers(t, dto); len(mem) != 0 {
		t.Fatalf("after emptying = %+v", mem)
	}
	if !strings.Contains(string(r.body), `"clips":[]`) {
		t.Fatalf("an empty collection's GET must spell it \"clips\":[]: %s", r.body)
	}
	_ = otherCookie
}

// truncateID mirrors quoteID's bound so the test can look for the echoed id.
func truncateID(id string) string {
	r := []rune(id)
	if len(r) > 32 {
		return string(r[:32])
	}
	return id
}

// TestCollectionDropsADeletedClip: an owner DELETE (or a TTL sweep) takes the
// clip out of every collection through the FK cascade — no membership step
// anywhere, and the collection itself is untouched.
func TestCollectionDropsADeletedClip(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	cid := e.newCollection("Best of", withCookie(cookie))
	a, _, _ := e.readyClip(owner, clipOpts{title: "Alpha", createdAt: 1000})
	b, _, _ := e.readyClip(owner, clipOpts{title: "Bravo", createdAt: 1001})
	if r := e.doJSON("PUT", "/api/collections/"+cid+"/clips",
		map[string]any{"clip_ids": []string{a.ID, b.ID}}, withCookie(cookie)); r.code != 200 {
		t.Fatalf("PUT: %d %s", r.code, r.body)
	}
	if r := e.doJSON("DELETE", "/api/clips/"+a.ID, nil, withCookie(cookie)); r.code != 204 {
		t.Fatalf("DELETE clip: %d %s", r.code, r.body)
	}
	dto := collectionDTO(t, e.do("GET", "/api/collections/"+cid, nil))
	if mem := collectionMembers(t, dto); dto.ClipCount != 1 || len(mem) != 1 || mem[0].ID != b.ID {
		t.Fatalf("after the clip delete = %+v", dto)
	}
}

// TestCollectionDeleteKeepsClips: 204, then 404, and every clip still there.
func TestCollectionDelete(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	cid := e.newCollection("Doomed", withCookie(cookie))
	a, _, _ := e.readyClip(owner, clipOpts{})
	if r := e.doJSON("PUT", "/api/collections/"+cid+"/clips",
		map[string]any{"clip_ids": []string{a.ID}}, withCookie(cookie)); r.code != 200 {
		t.Fatalf("PUT: %d %s", r.code, r.body)
	}
	r := e.doJSON("DELETE", "/api/collections/"+cid, nil, withCookie(cookie))
	if r.code != 204 || len(r.body) != 0 {
		t.Fatalf("DELETE: %d %q", r.code, r.body)
	}
	if r = e.do("GET", "/api/collections/"+cid, nil); r.code != 404 {
		t.Fatalf("GET after delete: %d %s", r.code, r.body)
	}
	if r = e.doJSON("DELETE", "/api/collections/"+cid, nil, withCookie(cookie)); r.code != 404 {
		t.Fatalf("second DELETE: %d %s", r.code, r.body)
	}
	if r = e.do("GET", "/api/clips/"+a.ID, nil); r.code != 200 {
		t.Fatalf("the clip went with the collection: %d %s", r.code, r.body)
	}
}

func TestCollectionRename(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("owner")
	cid := e.newCollection("Before", withCookie(cookie))
	r := e.doJSON("PATCH", "/api/collections/"+cid, map[string]any{"title": "After"}, withCookie(cookie))
	if r.code != 200 || collectionDTO(t, r).Title != "After" {
		t.Fatalf("PATCH: %d %s", r.code, r.body)
	}
	if r = e.doJSON("PATCH", "/api/collections/"+cid, map[string]any{"title": ""}, withCookie(cookie)); r.code != 400 {
		t.Fatalf("PATCH empty title: %d %s", r.code, r.body)
	}
	if r = e.do("GET", "/api/collections/"+cid, nil); collectionDTO(t, r).Title != "After" {
		t.Fatalf("title after the rejected rename: %s", r.body)
	}
	// PATCH and PUT are JSON routes: a form body is a 415 (§14).
	if r = e.do("PATCH", "/api/collections/"+cid, strings.NewReader("title=x"),
		withHeader("Content-Type", "application/x-www-form-urlencoded"), withCookie(cookie)); r.code != 415 {
		t.Fatalf("form PATCH: %d %s", r.code, r.body)
	}
}

func TestCollectionCapAnswers409(t *testing.T) {
	e := newEnv(t)
	u, cookie := e.newUser("owner")
	ctx := context.Background()
	for i := 0; i < db.MaxCollectionsPerUser; i++ {
		if _, err := e.db.CreateCollection(ctx, ids.New(), u.ID, "c", 1000); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	r := e.doJSON("POST", "/api/collections", map[string]any{"title": "one too many"}, withCookie(cookie))
	if r.code != 409 || r.errCode() != "too_many_collections" {
		t.Fatalf("past the cap: %d %s", r.code, r.body)
	}
}

func TestCollectionMethodNotAllowed(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("owner")
	cid := e.newCollection("Mine", withCookie(cookie))
	r := e.doJSON("DELETE", "/api/collections/"+cid+"/clips", nil, withCookie(cookie))
	if r.code != 405 || r.hdr.Get("Allow") != "PUT" {
		t.Fatalf("DELETE …/clips: %d Allow=%q", r.code, r.hdr.Get("Allow"))
	}
	r = e.doJSON("PUT", "/api/collections", nil, withCookie(cookie))
	if r.code != 405 {
		t.Fatalf("PUT /api/collections: %d", r.code)
	}
}
