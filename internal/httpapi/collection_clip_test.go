package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// POST/DELETE /api/collections/{cid}/clips/{id} (ROADMAP #17, §20 erratum
// 151). The matrix below is the whole contract: who may call it, what each
// wrong id answers, and what the two verbs do to the membership.

// newCollectionID creates one collection and returns its id.
func (e *env) newCollectionID(cookie *http.Cookie, title string) string {
	e.t.Helper()
	r := e.doJSON("POST", "/api/collections", map[string]any{"title": title}, withCookie(cookie))
	if r.code != 201 {
		e.t.Fatalf("create collection = %d %s", r.code, r.body)
	}
	return r.str("id")
}

// TestCollectionClipAddAndRemove: one clip, two verbs, an append that does not
// disturb the order and a remove that is idempotent in the clip.
func TestCollectionClipAddAndRemove(t *testing.T) {
	e := newEnv(t)
	u, cookie := e.newUser("curator")
	a := e.seedClip(u, clipOpts{title: "a"})
	b := e.seedClip(u, clipOpts{title: "b"})
	cid := e.newCollectionID(cookie, "best of")

	// The first add answers with the collection, counted.
	r := e.do("POST", "/api/collections/"+cid+"/clips/"+a.ID, nil, withCookie(cookie))
	if r.code != 200 || r.json["clip_count"] != float64(1) {
		t.Fatalf("add a = %d %s", r.code, r.body)
	}
	// The second APPENDS: the order the owner already had is untouched.
	r = e.do("POST", "/api/collections/"+cid+"/clips/"+b.ID, nil, withCookie(cookie))
	if r.code != 200 || r.json["clip_count"] != float64(2) {
		t.Fatalf("add b = %d %s", r.code, r.body)
	}
	got := e.do("GET", "/api/collections/"+cid, nil)
	clips, _ := got.json["clips"].([]any)
	if len(clips) != 2 {
		t.Fatalf("members = %s", got.body)
	}
	if first, _ := clips[0].(map[string]any); first["id"] != a.ID {
		t.Errorf("the append reordered the collection: %s", got.body)
	}
	// Adding again is success and does NOT move the clip to the end.
	r = e.do("POST", "/api/collections/"+cid+"/clips/"+a.ID, nil, withCookie(cookie))
	if r.code != 200 || r.json["clip_count"] != float64(2) {
		t.Fatalf("re-add = %d %s", r.code, r.body)
	}
	got = e.do("GET", "/api/collections/"+cid, nil)
	clips, _ = got.json["clips"].([]any)
	if first, _ := clips[0].(map[string]any); first["id"] != a.ID {
		t.Errorf("a repeated add moved the clip: %s", got.body)
	}

	// Remove is 204, and removing again is 204 too.
	if r := e.do("DELETE", "/api/collections/"+cid+"/clips/"+a.ID, nil, withCookie(cookie)); r.code != 204 {
		t.Fatalf("remove = %d %s", r.code, r.body)
	}
	if r := e.do("DELETE", "/api/collections/"+cid+"/clips/"+a.ID, nil, withCookie(cookie)); r.code != 204 {
		t.Errorf("removing a non-member = %d", r.code)
	}
	// …and the CLIP is still there. A collection is a list of links.
	if r := e.do("GET", "/api/clips/"+a.ID, nil); r.code != 200 {
		t.Errorf("the clip was deleted with its membership: %d", r.code)
	}
	got = e.do("GET", "/api/collections/"+cid, nil)
	if got.json["clip_count"] != float64(1) {
		t.Errorf("clip_count after the remove = %s", got.body)
	}
}

// TestCollectionClipOwnershipMatrix: every wrong id, every wrong caller.
func TestCollectionClipOwnershipMatrix(t *testing.T) {
	e := newEnv(t)
	u, cookie := e.newUser("curator")
	other, otherCookie := e.newUser("stranger")
	mine := e.seedClip(u, clipOpts{title: "mine"})
	theirs := e.seedClip(other, clipOpts{title: "theirs"})
	cid := e.newCollectionID(cookie, "best of")
	theirCid := e.newCollectionID(otherCookie, "theirs")

	cases := []struct {
		name, method, path string
		cookie             *http.Cookie
		want               int
		code               string
	}{
		{"a stranger's collection", "POST", "/api/collections/" + theirCid + "/clips/" + mine.ID, cookie, 404, "not_found"},
		{"a stranger's collection, remove", "DELETE", "/api/collections/" + theirCid + "/clips/" + mine.ID, cookie, 404, "not_found"},
		{"an unknown collection", "POST", "/api/collections/AaBbCcDdEeFf/clips/" + mine.ID, cookie, 404, "not_found"},
		{"a malformed collection id", "POST", "/api/collections/nope/clips/" + mine.ID, cookie, 404, "not_found"},
		{"a stranger's clip", "POST", "/api/collections/" + cid + "/clips/" + theirs.ID, cookie, 400, "not_your_clip"},
		{"an unknown clip", "POST", "/api/collections/" + cid + "/clips/AaBbCcDdEeFf", cookie, 400, "not_your_clip"},
		{"a malformed clip id", "POST", "/api/collections/" + cid + "/clips/nope", cookie, 400, "not_your_clip"},
		{"a malformed clip id, remove", "DELETE", "/api/collections/" + cid + "/clips/nope", cookie, 400, "not_your_clip"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := e.do(c.method, c.path, nil, withCookie(c.cookie))
			if r.code != c.want || r.errCode() != c.code {
				t.Fatalf("%s %s = %d %s, want %d %s", c.method, c.path, r.code, r.body, c.want, c.code)
			}
		})
	}
	// A stranger's collection answers the SAME 404 whether the collection
	// exists or not — the route is no existence oracle.
	real := e.do("POST", "/api/collections/"+theirCid+"/clips/"+mine.ID, nil, withCookie(cookie))
	fake := e.do("POST", "/api/collections/AaBbCcDdEeFf/clips/"+mine.ID, nil, withCookie(cookie))
	if string(real.body) != string(fake.body) {
		t.Errorf("a real collection and an invented one answer differently:\n%s\n%s", real.body, fake.body)
	}
	// Nothing landed anywhere.
	if got := e.do("GET", "/api/collections/"+cid, nil); got.json["clip_count"] != float64(0) {
		t.Errorf("a refused request added a clip: %s", got.body)
	}
	if got := e.do("GET", "/api/collections/"+theirCid, nil); got.json["clip_count"] != float64(0) {
		t.Errorf("a refused request wrote to the stranger's collection: %s", got.body)
	}
}

// TestCollectionClipPosture: session only, CSRF-guarded, no body accepted or
// needed, and 405 for everything else.
func TestCollectionClipPosture(t *testing.T) {
	e := newEnv(t)
	u, cookie := e.newUser("curator")
	c := e.seedClip(u, clipOpts{title: "x"})
	cid := e.newCollectionID(cookie, "best of")
	path := "/api/collections/" + cid + "/clips/" + c.ID
	tok := e.newToken(u)

	if r := e.do("POST", path, nil); r.code != 401 {
		t.Errorf("anonymous add = %d", r.code)
	}
	if r := e.do("POST", path, nil, withBearer(tok)); r.code != 403 || r.errCode() != "token_not_allowed" {
		t.Errorf("token add = %d %s", r.code, r.body)
	}
	if r := e.do("DELETE", path, nil, withBearer(tok)); r.code != 403 {
		t.Errorf("token remove = %d %s", r.code, r.body)
	}
	if r := e.do("POST", path, nil, withCookie(cookie), withHeader("Origin", "https://evil.example")); r.code != 403 {
		t.Errorf("cross-origin add = %d %s", r.code, r.body)
	}
	if r := e.do("DELETE", path, nil, withCookie(cookie), withHeader("Origin", "https://evil.example")); r.code != 403 {
		t.Errorf("cross-origin remove = %d %s", r.code, r.body)
	}
	if r := e.do("GET", path, nil, withCookie(cookie)); r.code != 405 {
		t.Errorf("GET = %d", r.code)
	}
	if r := e.do("PUT", path, nil, withCookie(cookie)); r.code != 405 {
		t.Errorf("PUT = %d", r.code)
	}
	// The whole-list PUT on the sibling path is untouched by any of this.
	if r := e.doJSON("PUT", "/api/collections/"+cid+"/clips", map[string]any{"clip_ids": []string{c.ID}}, withCookie(cookie)); r.code != 200 {
		t.Errorf("PUT …/clips = %d %s", r.code, r.body)
	}
	// A body is ignored rather than required: neither verb takes one.
	if r := e.do("POST", path, strings.NewReader(`{"anything":true}`), withCookie(cookie), withJSON()); r.code != 200 {
		t.Errorf("add with a body = %d %s", r.code, r.body)
	}
}
