package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// dtoTags reads the `tags` array of a ClipDTO body. It fails the test when the
// key is missing or null: `tags` is ALWAYS present and `[]` is how "none" is
// spelled (§20 erratum 81).
func dtoTags(t *testing.T, r resp) []string {
	t.Helper()
	raw, ok := r.json["tags"]
	if !ok {
		t.Fatalf("the DTO has no tags key: %s", r.body)
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("tags is %T, want an array (never null): %s", raw, r.body)
	}
	out := make([]string, len(list))
	for i, v := range list {
		out[i], _ = v.(string)
	}
	return out
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPatchTags is the write path: replace, clear, absent-means-unchanged,
// and every rejection naming the offending word.
func TestPatchTags(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c := e.seedClip(owner, clipOpts{})

	// A fresh clip carries `[]`, not null and not a missing key.
	r := e.do("GET", "/api/clips/"+c.ID, nil)
	if got := dtoTags(t, r); len(got) != 0 {
		t.Fatalf("fresh clip tags = %v", got)
	}
	if !strings.Contains(string(r.body), `"tags":[]`) {
		t.Fatalf("an empty tag set must serialise as [], got: %s", r.body)
	}

	// Set: normalised, deduped, sorted — by the server, whatever was sent.
	r = e.doJSON("PATCH", "/api/clips/"+c.ID, map[string]any{"tags": []string{"Gaming", "clutch", "GAMING"}}, withCookie(cookie))
	if r.code != 200 {
		t.Fatalf("PATCH tags: %d %s", r.code, r.body)
	}
	if got := dtoTags(t, r); !eqStrings(got, []string{"clutch", "gaming"}) {
		t.Fatalf("after PATCH = %v", got)
	}

	// Absent = unchanged: a title-only edit must not clear the tags.
	r = e.doJSON("PATCH", "/api/clips/"+c.ID, map[string]any{"title": "renamed"}, withCookie(cookie))
	if r.code != 200 || !eqStrings(dtoTags(t, r), []string{"clutch", "gaming"}) {
		t.Fatalf("title-only PATCH changed the tags: %d %s", r.code, r.body)
	}

	// [] clears.
	r = e.doJSON("PATCH", "/api/clips/"+c.ID, map[string]any{"tags": []string{}}, withCookie(cookie))
	if r.code != 200 || len(dtoTags(t, r)) != 0 {
		t.Fatalf("PATCH [] did not clear: %d %s", r.code, r.body)
	}

	// A rejection names the tag and changes nothing.
	if r = e.doJSON("PATCH", "/api/clips/"+c.ID, map[string]any{"tags": []string{"ok", "NOT OK"}}, withCookie(cookie)); r.code != 400 ||
		r.errCode() != "invalid_tags" || !strings.Contains(r.str("message"), "NOT OK") {
		t.Fatalf("bad tag: %d %s", r.code, r.body)
	}
	// Eleven distinct tags is the cap, not a bad word.
	eleven := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}
	if r = e.doJSON("PATCH", "/api/clips/"+c.ID, map[string]any{"tags": eleven}, withCookie(cookie)); r.code != 400 ||
		r.errCode() != "invalid_tags" || !strings.Contains(r.str("message"), "10") {
		t.Fatalf("eleven tags: %d %s", r.code, r.body)
	}
	if r = e.doJSON("PATCH", "/api/clips/"+c.ID, map[string]any{"tags": eleven[:10]}, withCookie(cookie)); r.code != 200 {
		t.Fatalf("ten tags: %d %s", r.code, r.body)
	}
	// A 33-character tag.
	long := strings.Repeat("a", 33)
	if r = e.doJSON("PATCH", "/api/clips/"+c.ID, map[string]any{"tags": []string{long}}, withCookie(cookie)); r.code != 400 || r.errCode() != "invalid_tags" {
		t.Fatalf("33-char tag: %d %s", r.code, r.body)
	}
	// `tags` must be an array, and saying so is more useful than "bad body".
	if r = e.doJSON("PATCH", "/api/clips/"+c.ID, `{"tags":"gaming"}`, withCookie(cookie)); r.code != 400 || r.errCode() != "invalid_request" {
		t.Fatalf("tags as a string: %d %s", r.code, r.body)
	}
	// And tags are an OWNER mutation: a token gets erratum 19's answer.
	tok := e.newToken(owner, "upload")
	if r = e.doJSON("PATCH", "/api/clips/"+c.ID, map[string]any{"tags": []string{"x"}}, withBearer(tok)); r.code != 403 ||
		r.errCode() != "token_not_allowed" {
		t.Fatalf("token PATCH tags: %d %s", r.code, r.body)
	}
}

// TestTagsAreNotOwnerPrivate: unlike `views`, a clip's tags reach every caller
// its DTO reaches — they are part of what the clip is, and the DTO is already
// owner-or-link gated.
func TestTagsAreNotOwnerPrivate(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	_, otherCookie := e.newUser("other")
	c, _, _ := e.readyClip(owner, clipOpts{})
	if r := e.doJSON("PATCH", "/api/clips/"+c.ID, map[string]any{"tags": []string{"gaming"}}, withCookie(cookie)); r.code != 200 {
		t.Fatalf("PATCH: %d %s", r.code, r.body)
	}
	for _, who := range []struct {
		name string
		opts []reqOpt
	}{
		{"anonymous", nil},
		{"other user", []reqOpt{withCookie(otherCookie)}},
		{"owner", []reqOpt{withCookie(cookie)}},
	} {
		r := e.do("GET", "/api/clips/"+c.ID, nil, who.opts...)
		if r.code != 200 || !eqStrings(dtoTags(t, r), []string{"gaming"}) {
			t.Errorf("%s: %d %s", who.name, r.code, r.body)
		}
	}
}

// TestUploadCarriesTags: the metadata part accepts `tags`, they are written
// with the row and the 201 already reports them.
func TestUploadCarriesTags(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	r := e.upload(`{"title":"t","tags":["Gaming","clutch","gaming"]}`, "a.mp4", testVideo(64), withCookie(cookie))
	if r.code != 201 {
		t.Fatalf("upload: %d %s", r.code, r.body)
	}
	if got := dtoTags(t, r); !eqStrings(got, []string{"clutch", "gaming"}) {
		t.Fatalf("201 tags = %v", got)
	}
	id := r.str("id")
	tags, err := e.db.TagsOf(context.Background(), id)
	if err != nil || !eqStrings(tags, []string{"clutch", "gaming"}) {
		t.Fatalf("stored tags = %v, %v", tags, err)
	}
	_ = owner

	// A bad tag is a 400 before anything is written.
	r = e.upload(`{"title":"t","tags":["NOT OK"]}`, "b.mp4", testVideo(64), withCookie(cookie))
	if r.code != 400 || r.errCode() != "invalid_tags" || !strings.Contains(r.str("message"), "NOT OK") {
		t.Fatalf("bad upload tag: %d %s", r.code, r.body)
	}
	list := e.do("GET", "/api/clips", nil, withCookie(cookie))
	var body struct {
		Clips []ClipDTO `json:"clips"`
	}
	if err := json.Unmarshal(list.body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Clips) != 1 {
		t.Fatalf("a rejected upload created %d clips", len(body.Clips)-1)
	}
}

// TestCaptureUploadTags: the capture form spells a tag set comma-separated,
// an empty field means "not sent", and the grammar is the same one.
func TestCaptureUploadTags(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	tok := e.newToken(owner, "upload")

	post := func(tags string) resp {
		body, ct := multipartBody(t,
			part{name: "title", data: []byte("captured")},
			part{name: "tags", data: []byte(tags)},
			part{name: "file", filename: "clip.mp4", contentType: "video/mp4", data: testVideo(64)})
		return e.do(http.MethodPost, "/api/upload", body, withHeader("Content-Type", ct), withBearer(tok))
	}

	r := post(" Gaming , clutch ,")
	if r.code != 201 {
		t.Fatalf("capture upload: %d %s", r.code, r.body)
	}
	// The 201 is erratum 58's five fields and nothing else — no tags in it.
	if r.has("tags") {
		t.Fatalf("the capture 201 must stay the five fields: %s", r.body)
	}
	tags, err := e.db.TagsOf(context.Background(), r.str("id"))
	if err != nil || !eqStrings(tags, []string{"clutch", "gaming"}) {
		t.Fatalf("stored = %v, %v", tags, err)
	}

	if r = post(""); r.code != 201 {
		t.Fatalf("empty tags field: %d %s", r.code, r.body)
	}
	if tags, _ := e.db.TagsOf(context.Background(), r.str("id")); len(tags) != 0 {
		t.Fatalf("an empty field stored %v", tags)
	}

	if r = post("gaming, NOT OK"); r.code != 400 || r.errCode() != "invalid_tags" ||
		!strings.Contains(r.str("message"), "NOT OK") {
		t.Fatalf("bad capture tag: %d %s", r.code, r.body)
	}
}

// TestListClipsTagFilter is the read path: the filter narrows the visibility
// set, paginates, and refuses a tag that cannot exist.
func TestListClipsTagFilter(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	other, otherCookie := e.newUser("other")

	a := e.seedClip(owner, clipOpts{createdAt: 1000})
	b := e.seedClip(owner, clipOpts{createdAt: 1001})
	hidden := e.seedClip(other, clipOpts{createdAt: 1002, unlisted: true})
	theirs := e.seedClip(other, clipOpts{createdAt: 1003})
	plain := e.seedClip(owner, clipOpts{createdAt: 1004})
	ctx := context.Background()
	for _, id := range []string{a.ID, b.ID, hidden.ID, theirs.ID} {
		if err := e.db.SetClipTags(ctx, id, []string{"pick"}); err != nil {
			t.Fatal(err)
		}
	}

	ids := func(r resp) []string {
		t.Helper()
		var body struct {
			Clips      []ClipDTO `json:"clips"`
			NextCursor *string   `json:"next_cursor"`
		}
		if err := json.Unmarshal(r.body, &body); err != nil {
			t.Fatalf("%v: %s", err, r.body)
		}
		out := make([]string, len(body.Clips))
		for i, c := range body.Clips {
			out[i] = c.ID
		}
		return out
	}

	// The owner sees their own two; the stranger's unlisted clip is not in it.
	r := e.do("GET", "/api/clips?tag=pick", nil, withCookie(cookie))
	if r.code != 200 || !eqStrings(ids(r), []string{theirs.ID, b.ID, a.ID}) {
		t.Fatalf("owner filtered list = %v (%s)", ids(r), r.body)
	}
	// The other user sees their own unlisted one instead.
	r = e.do("GET", "/api/clips?tag=pick", nil, withCookie(otherCookie))
	if !eqStrings(ids(r), []string{theirs.ID, hidden.ID, b.ID, a.ID}) {
		t.Fatalf("other filtered list = %v", ids(r))
	}
	// The untagged clip is never in a filtered page.
	for _, id := range ids(r) {
		if id == plain.ID {
			t.Fatal("an untagged clip appeared in a filtered page")
		}
	}
	// An uppercase filter is the same filter: the grammar normalises first.
	r = e.do("GET", "/api/clips?tag=PICK", nil, withCookie(cookie))
	if !eqStrings(ids(r), []string{theirs.ID, b.ID, a.ID}) {
		t.Fatalf("uppercase filter = %v", ids(r))
	}
	// A tag nobody carries is an empty page, not an error.
	r = e.do("GET", "/api/clips?tag=nobody", nil, withCookie(cookie))
	if r.code != 200 || len(ids(r)) != 0 {
		t.Fatalf("unknown tag: %d %v", r.code, ids(r))
	}
	// A tag that cannot exist is a 400 that names it.
	r = e.do("GET", "/api/clips?tag=BAD!", nil, withCookie(cookie))
	if r.code != 400 || r.errCode() != "invalid_tags" || !strings.Contains(r.str("message"), "BAD!") {
		t.Fatalf("illegal tag: %d %s", r.code, r.body)
	}

	// Pagination: one row per page, and the cursor keeps the filter.
	var seen []string
	url := "/api/clips?tag=pick&limit=1"
	for i := 0; i < 5; i++ {
		r = e.do("GET", url, nil, withCookie(cookie))
		if r.code != 200 {
			t.Fatalf("page %d: %d %s", i, r.code, r.body)
		}
		seen = append(seen, ids(r)...)
		next, _ := r.json["next_cursor"].(string)
		if next == "" {
			break
		}
		url = "/api/clips?tag=pick&limit=1&cursor=" + next
	}
	if !eqStrings(seen, []string{theirs.ID, b.ID, a.ID}) {
		t.Fatalf("paged = %v", seen)
	}
	// A cursor minted under the filter is not a cursor for the whole library.
	r = e.do("GET", "/api/clips?tag=pick&limit=1", nil, withCookie(cookie))
	cursor, _ := r.json["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("no cursor to test with")
	}
	if r = e.do("GET", "/api/clips?limit=1&cursor="+cursor, nil, withCookie(cookie)); r.code != 400 || r.errCode() != "invalid_cursor" {
		t.Fatalf("a filtered cursor spent unfiltered: %d %s", r.code, r.body)
	}
}

// TestListClipsCarriesTags: the library page renders each row's tags, from
// ONE batch query rather than one per row.
func TestListClipsCarriesTags(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	a := e.seedClip(owner, clipOpts{createdAt: 1000})
	e.seedClip(owner, clipOpts{createdAt: 1001})
	if err := e.db.SetClipTags(context.Background(), a.ID, []string{"gaming"}); err != nil {
		t.Fatal(err)
	}
	r := e.do("GET", "/api/clips", nil, withCookie(cookie))
	var body struct {
		Clips []ClipDTO `json:"clips"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Clips) != 2 {
		t.Fatalf("clips = %d", len(body.Clips))
	}
	for _, c := range body.Clips {
		if c.Tags == nil {
			t.Fatalf("clip %s has nil tags; every DTO carries [] at worst", c.ID)
		}
		want := []string{}
		if c.ID == a.ID {
			want = []string{"gaming"}
		}
		if !eqStrings(c.Tags, want) {
			t.Errorf("clip %s tags = %v, want %v", c.ID, c.Tags, want)
		}
	}
}

// TestTouchAndRetryCarryTags: every route that answers with a ClipDTO answers
// with the tags too — a client that re-renders from the reply must not see a
// clip lose its tags because it pressed "keep forever".
func TestTouchAndRetryCarryTags(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	ttl := int64(86400)
	c, _, _ := e.readyClip(owner, clipOpts{ttl: &ttl})
	if err := e.db.SetClipTags(context.Background(), c.ID, []string{"gaming"}); err != nil {
		t.Fatal(err)
	}
	r := e.doJSON("POST", "/api/clips/"+c.ID+"/touch", nil, withCookie(cookie))
	if r.code != 200 || !eqStrings(dtoTags(t, r), []string{"gaming"}) {
		t.Fatalf("touch: %d %s", r.code, r.body)
	}
	f := e.failedClip(owner, clipOpts{})
	if err := e.db.SetClipTags(context.Background(), f.ID, []string{"broken"}); err != nil {
		t.Fatal(err)
	}
	r = e.doJSON("POST", "/api/clips/"+f.ID+"/retry", nil, withCookie(cookie))
	if r.code != 202 || !eqStrings(dtoTags(t, r), []string{"broken"}) {
		t.Fatalf("retry: %d %s", r.code, r.body)
	}
}

// TestTagsSurviveADestroyedClip is the API-side view of the FK cascade: the
// tag rows of a deleted clip are gone, so a filter can never surface a ghost.
func TestTagsSurviveNothing(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	if err := e.db.SetClipTags(context.Background(), c.ID, []string{"ghost"}); err != nil {
		t.Fatal(err)
	}
	if r := e.doJSON("DELETE", "/api/clips/"+c.ID, nil, withCookie(cookie)); r.code != 204 {
		t.Fatalf("DELETE: %d %s", r.code, r.body)
	}
	r := e.do("GET", "/api/clips?tag=ghost", nil, withCookie(cookie))
	var body struct {
		Clips []ClipDTO `json:"clips"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Clips) != 0 {
		t.Fatalf("a destroyed clip is still tagged: %s", r.body)
	}
	tags, err := e.db.TagsOf(context.Background(), c.ID)
	if err != nil || len(tags) != 0 {
		t.Fatalf("orphan tag rows: %v, %v", tags, err)
	}
}
