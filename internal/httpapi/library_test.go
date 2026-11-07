package httpapi

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"clippet/internal/config"
	"clippet/internal/db"
)

// listIDs reads the ids of a GET /api/clips page, in order.
func listIDs(t *testing.T, r resp) []string {
	t.Helper()
	var body struct {
		Clips []ClipDTO `json:"clips"`
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

// retitle renames a seeded clip so the search tests have something to find.
func (e *env) retitle(id, title string) {
	e.t.Helper()
	if _, err := e.db.UpdateClipMeta(context.Background(), id, db.ClipPatch{Title: &title}, 1000); err != nil {
		e.t.Fatal(err)
	}
}

// TestListClipsSearchFilter: `?q=` narrows the SAME visibility set — a
// stranger's unlisted clip carrying the needle is still invisible — and an
// illegal query is a 400 rather than a silently wider list.
func TestListClipsSearchFilter(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	other, otherCookie := e.newUser("other")

	mine := e.seedClip(owner, clipOpts{createdAt: 1000})
	e.retitle(mine.ID, "The clutch round")
	theirs := e.seedClip(other, clipOpts{createdAt: 1001})
	e.retitle(theirs.ID, "Their clutch too")
	secret := e.seedClip(other, clipOpts{createdAt: 1002, unlisted: true})
	e.retitle(secret.ID, "Secret clutch")
	miss := e.seedClip(owner, clipOpts{createdAt: 1003})
	e.retitle(miss.ID, "nothing here")

	r := e.do("GET", "/api/clips?q=clutch", nil, withCookie(cookie))
	if r.code != 200 || !eqStrings(listIDs(t, r), []string{theirs.ID, mine.ID}) {
		t.Fatalf("search = %v (%s)", listIDs(t, r), r.body)
	}
	// Case does not matter, and the untitled miss is never in it.
	if got := listIDs(t, e.do("GET", "/api/clips?q=CLUTCH", nil, withCookie(cookie))); !eqStrings(got, []string{theirs.ID, mine.ID}) {
		t.Fatalf("an uppercase search = %v", got)
	}
	// The owner of the unlisted clip sees it; nobody else does, under any
	// filter. This is the whole "narrows, never widens" rule.
	if got := listIDs(t, e.do("GET", "/api/clips?q=clutch", nil, withCookie(otherCookie))); !eqStrings(got, []string{secret.ID, theirs.ID, mine.ID}) {
		t.Fatalf("the other user's search = %v", got)
	}
	// Hostile needles are text, not syntax.
	for _, q := range []string{"%", "_", `\`, "%_%"} {
		r = e.do("GET", "/api/clips?q="+url.QueryEscape(q), nil, withCookie(cookie))
		if r.code != 200 {
			t.Fatalf("q=%q: %d %s", q, r.code, r.body)
		}
		if got := listIDs(t, r); len(got) != 0 {
			t.Fatalf("q=%q matched %v; a LIKE metacharacter must be a character", q, got)
		}
	}
	// Too long, and not text at all.
	for _, q := range []string{strings.Repeat("a", db.MaxSearchRunes+1), strings.Repeat("x", 10<<10), "a\nb"} {
		r = e.do("GET", "/api/clips?q="+url.QueryEscape(q), nil, withCookie(cookie))
		if r.code != 400 || r.errCode() != "invalid_request" {
			t.Fatalf("q of %d chars: %d %s", len(q), r.code, r.body)
		}
	}
	// An empty q is the absence of a filter, not an error.
	if r = e.do("GET", "/api/clips?q=", nil, withCookie(cookie)); r.code != 200 ||
		!eqStrings(listIDs(t, r), []string{miss.ID, theirs.ID, mine.ID}) {
		t.Fatalf("q= : %d %v", r.code, listIDs(t, r))
	}
}

// TestListClipsMineFilter: `?mine=1` is the viewer's own clips, `?mine=` in
// any other spelling is a 400, and there is no way to name another user.
func TestListClipsMineFilter(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	other, _ := e.newUser("other")
	mine := e.seedClip(owner, clipOpts{createdAt: 1000})
	mineHidden := e.seedClip(owner, clipOpts{createdAt: 1001, unlisted: true})
	theirs := e.seedClip(other, clipOpts{createdAt: 1002})

	r := e.do("GET", "/api/clips", nil, withCookie(cookie))
	if got := listIDs(t, r); !eqStrings(got, []string{theirs.ID, mineHidden.ID, mine.ID}) {
		t.Fatalf("the shared library = %v", got)
	}
	r = e.do("GET", "/api/clips?mine=1", nil, withCookie(cookie))
	if r.code != 200 || !eqStrings(listIDs(t, r), []string{mineHidden.ID, mine.ID}) {
		t.Fatalf("?mine=1 = %v (%s)", listIDs(t, r), r.body)
	}
	// Every other spelling is a 400: a filter that silently did nothing would
	// show a WIDER list than the caller asked for.
	for _, v := range []string{"", "0", "true", "yes", "me", other.ID} {
		r = e.do("GET", "/api/clips?mine="+url.QueryEscape(v), nil, withCookie(cookie))
		if r.code != 400 || r.errCode() != "invalid_request" {
			t.Fatalf("?mine=%q: %d %s", v, r.code, r.body)
		}
	}
}

// TestListClipsFiltersCombineAndBindTheCursor: the three filters compose, and
// the page token minted under them is refused under any other combination.
func TestListClipsFiltersCombineAndBindTheCursor(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	other, _ := e.newUser("other")
	ctx := context.Background()

	a := e.seedClip(owner, clipOpts{createdAt: 1000})
	e.retitle(a.ID, "clutch one")
	b := e.seedClip(owner, clipOpts{createdAt: 1001})
	e.retitle(b.ID, "clutch two")
	theirs := e.seedClip(other, clipOpts{createdAt: 1002})
	e.retitle(theirs.ID, "clutch theirs")
	for _, id := range []string{a.ID, b.ID, theirs.ID} {
		if err := e.db.SetClipTags(ctx, id, []string{"pick"}); err != nil {
			t.Fatal(err)
		}
	}
	r := e.do("GET", "/api/clips?tag=pick&q=clutch&mine=1", nil, withCookie(cookie))
	if r.code != 200 || !eqStrings(listIDs(t, r), []string{b.ID, a.ID}) {
		t.Fatalf("the three filters together = %v (%s)", listIDs(t, r), r.body)
	}
	// Page it one row at a time; the cursor keeps every filter.
	r = e.do("GET", "/api/clips?tag=pick&q=clutch&mine=1&limit=1", nil, withCookie(cookie))
	next, _ := r.json["next_cursor"].(string)
	if next == "" {
		t.Fatalf("no cursor on a one-row page: %s", r.body)
	}
	if r = e.do("GET", "/api/clips?tag=pick&q=clutch&mine=1&limit=1&cursor="+url.QueryEscape(next), nil, withCookie(cookie)); r.code != 200 ||
		!eqStrings(listIDs(t, r), []string{a.ID}) {
		t.Fatalf("continuing the filtered page: %d %s", r.code, r.body)
	}
	for _, q := range []string{
		"/api/clips?limit=1&cursor=",
		"/api/clips?tag=pick&limit=1&cursor=",
		"/api/clips?q=clutch&mine=1&limit=1&cursor=",
		"/api/clips?tag=pick&q=other&mine=1&limit=1&cursor=",
	} {
		r = e.do("GET", q+url.QueryEscape(next), nil, withCookie(cookie))
		if r.code != 400 || r.errCode() != "invalid_cursor" {
			t.Fatalf("%s: %d %s", q, r.code, r.body)
		}
	}
}

// TestLibraryScopeOwn is the two-user visibility matrix under
// LIBRARY_SCOPE=own: each account's list is its own clips and nothing else,
// while every other route is exactly as it was — the link is still the auth.
func TestLibraryScopeOwn(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Cfg.LibraryScope = config.LibraryScopeOwn })
	owner, cookie := e.newUser("owner")
	other, otherCookie := e.newUser("other")
	mine := e.seedClip(owner, clipOpts{createdAt: 1000})
	mineHidden := e.seedClip(owner, clipOpts{createdAt: 1001, unlisted: true})
	theirs := e.seedClip(other, clipOpts{createdAt: 1002})
	theirHidden := e.seedClip(other, clipOpts{createdAt: 1003, unlisted: true})

	if got := listIDs(t, e.do("GET", "/api/clips", nil, withCookie(cookie))); !eqStrings(got, []string{mineHidden.ID, mine.ID}) {
		t.Fatalf("owner's scoped library = %v", got)
	}
	if got := listIDs(t, e.do("GET", "/api/clips", nil, withCookie(otherCookie))); !eqStrings(got, []string{theirHidden.ID, theirs.ID}) {
		t.Fatalf("other's scoped library = %v", got)
	}
	// ?mine=1 is a no-op under the scope rather than an error.
	if got := listIDs(t, e.do("GET", "/api/clips?mine=1", nil, withCookie(cookie))); !eqStrings(got, []string{mineHidden.ID, mine.ID}) {
		t.Fatalf("?mine=1 under scope=own = %v", got)
	}
	// The scope changes the LIST and nothing else: the direct route still
	// answers for anyone holding the id (§8.1), because the link is the auth.
	if r := e.do("GET", "/api/clips/"+theirs.ID, nil, withCookie(cookie)); r.code != 200 {
		t.Fatalf("GET a friend's clip under scope=own = %d %s", r.code, r.body)
	}
	if r := e.do("GET", "/api/clips/"+theirHidden.ID, nil); r.code != 200 {
		t.Fatalf("an anonymous GET of an unlisted clip under scope=own = %d", r.code)
	}
	// And it never widens: an owner still cannot edit a stranger's clip.
	if r := e.doJSON("PATCH", "/api/clips/"+theirs.ID, map[string]any{"title": "mine now"}, withCookie(cookie)); r.code != 403 {
		t.Fatalf("PATCH a stranger's clip = %d", r.code)
	}
}

// TestLibraryScopeSharedIsTheDefault: an unset LIBRARY_SCOPE is the behaviour
// every deployment had before the variable existed.
func TestLibraryScopeSharedIsTheDefault(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	other, _ := e.newUser("other")
	mine := e.seedClip(owner, clipOpts{createdAt: 1000})
	theirs := e.seedClip(other, clipOpts{createdAt: 1001})
	if got := listIDs(t, e.do("GET", "/api/clips", nil, withCookie(cookie))); !eqStrings(got, []string{theirs.ID, mine.ID}) {
		t.Fatalf("the default library = %v, want both users' listed clips", got)
	}
}
