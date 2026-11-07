package web

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"clippet/internal/config"
	"clippet/internal/db"
)

// TestIndexSearchBox: the search is a plain GET form, so it works with no
// JavaScript at all, and the box is re-filled with what the server actually
// searched for (it normalises, so the page must not show something else).
func TestIndexSearchBox(t *testing.T) {
	f := newFixture(t)
	h := f.handler(session(f.owner))
	body := get(h, "/").Body.String()
	mustContain(t, body, `<form class="search" method="get" action="/"`, `name="q"`, `name="mine"`)
	mustNotContain(t, body, `data-filters`)
	assertNoInline(t, "index search form", body)
	// The box's own bound IS the server's. A control that let the user type a
	// needle CleanSearch then discards would answer the search with an empty
	// box and no reason why (§20 erratum 120).
	mustContain(t, body, `name="q" maxlength="`+strconv.Itoa(db.MaxSearchRunes)+`"`)

	// The server lower-cases and trims; the box shows that, not the typing.
	rec := get(h, "/?q=%20E2E%20STD%20")
	assertPageHeaders(t, rec, 200, "no-store")
	body = rec.Body.String()
	mustContain(t, body, `value="e2e std"`, `data-q-filter="e2e std"`,
		`matching`, `data-clip-id="`+f.ready.ID+`"`, `<a href="/">show all</a>`)
	// A search NARROWS: the other clips of the same library are gone, and a
	// stranger's unlisted clip was never reachable to begin with.
	mustNotContain(t, body, f.failed.ID, f.otherUnlisted.ID)
	assertNoInline(t, "index searched", body)
}

// TestIndexMineFilter: "mine only" is the viewer's own clips, the checkbox
// comes back ticked, and the friend's listed clip is gone.
func TestIndexMineFilter(t *testing.T) {
	f := newFixture(t)
	h := f.handler(session(f.owner))
	body := get(h, "/?mine=1").Body.String()
	mustContain(t, body, `data-mine-filter="1"`, `Showing your clips`,
		`name="mine" value="1" checked`, `data-clip-id="`+f.ready.ID+`"`)
	mustNotContain(t, body, f.otherListed.ID, f.otherUnlisted.ID)
	assertNoInline(t, "index mine", body)
	// The unfiltered library still holds the friend's listed clip.
	mustContain(t, get(h, "/").Body.String(), `data-clip-id="`+f.otherListed.ID+`"`)
}

// TestIndexIllegalFiltersBounce: a filter that cannot be a filter is a
// mistyped URL, and the library is a better answer to one than an error page
// — exactly what an illegal `?tag=` already does (§20 erratum 81).
func TestIndexIllegalFiltersBounce(t *testing.T) {
	f := newFixture(t)
	h := f.handler(session(f.owner))
	for _, u := range []string{
		"/?q=" + url.QueryEscape(strings.Repeat("a", db.MaxSearchRunes+1)),
		"/?q=" + url.QueryEscape(strings.Repeat("x", 10<<10)),
		"/?q=" + url.QueryEscape("a\nb"),
		"/?mine=0",
		"/?mine=true",
		"/?mine=" + f.other.ID,
	} {
		rec := get(h, u)
		if rec.Code != 303 || rec.Header().Get("Location") != "/" {
			t.Errorf("%s -> %d %q, want 303 /", u, rec.Code, rec.Header().Get("Location"))
		}
		assertPrivateRedirect(t, rec)
	}
}

// TestIndexFilterLinksCarryEveryFilter: the "older clips" link is minted
// under the filters in force, and a stale cursor lands on the FIRST page of
// those same filters rather than on the whole library.
func TestIndexFilterLinksCarryEveryFilter(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, c := range []db.Clip{f.ready, f.failed, f.queued} {
		if err := f.db.SetClipTags(ctx, c.ID, []string{"gaming"}); err != nil {
			t.Fatal(err)
		}
	}
	restore := libraryPageSize
	libraryPageSize = 1
	t.Cleanup(func() { libraryPageSize = restore })

	h := f.handler(session(f.owner))
	body := get(h, "/?tag=gaming&mine=1").Body.String()
	i := strings.Index(body, `class="more"`)
	if i < 0 {
		t.Fatalf("no pagination link on a filtered page: %s", body)
	}
	link := body[i:]
	if j := strings.Index(link, `</p>`); j >= 0 {
		link = link[:j]
	}
	for _, want := range []string{"cursor=", "tag=gaming", "mine=1"} {
		if !strings.Contains(link, want) {
			t.Fatalf("the older-clips link %q is missing %q", link, want)
		}
	}
	href := link[strings.Index(link, `href="`)+len(`href="`):]
	href = strings.ReplaceAll(href[:strings.Index(href, `"`)], "&amp;", "&")
	if rec := get(h, href); rec.Code != 200 {
		t.Fatalf("following %q = %d", href, rec.Code)
	}
	// A stale token keeps the filters it was spent under.
	for _, tc := range []struct{ url, want string }{
		{"/?tag=gaming&mine=1&cursor=garbage", "/?mine=1&tag=gaming"},
		{"/?q=std&cursor=garbage", "/?q=std"},
		{"/?mine=1&cursor=garbage", "/?mine=1"},
	} {
		rec := get(h, tc.url)
		if rec.Code != 303 || rec.Header().Get("Location") != tc.want {
			t.Errorf("%s -> %d %q, want 303 %q", tc.url, rec.Code, rec.Header().Get("Location"), tc.want)
		}
	}
}

// nextLink pulls the "Older clips" href out of a rendered library page and
// un-escapes it into something a request can be made with; newestLink is the
// same for the way back (§20 erratum 266). "" when that link is not on the
// page. Both anchor on the link's own data attribute rather than on "the first
// href in `.more`", because since erratum 266 the paragraph can hold two.
func nextLink(body string) string { return pageLink(body, "data-older") }

func newestLink(body string) string { return pageLink(body, "data-newest") }

// pageLink returns the href of the `.more` anchor carrying attr.
func pageLink(body, attr string) string {
	i := strings.Index(body, `class="more"`)
	if i < 0 {
		return ""
	}
	rest := body[i:]
	if j := strings.Index(rest, `</p>`); j >= 0 {
		rest = rest[:j]
	}
	j := strings.Index(rest, attr)
	if j < 0 {
		return ""
	}
	// The href is written before the marker attribute on the same anchor.
	a := strings.LastIndex(rest[:j], `href="`)
	if a < 0 {
		return ""
	}
	rest = rest[a+len(`href="`):]
	k := strings.Index(rest, `"`)
	if k < 0 {
		return ""
	}
	return strings.ReplaceAll(rest[:k], "&amp;", "&")
}

// TestIndexNewestClipsLink: a page reached WITH a cursor offers a way back
// (§20 erratum 266) — the un-cursored library under the same filters — and
// page one does not, because there is nothing behind it. It is not a reverse
// cursor: the link carries the filters and no cursor at all.
func TestIndexNewestClipsLink(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, c := range []db.Clip{f.ready, f.failed, f.queued} {
		if err := f.db.SetClipTags(ctx, c.ID, []string{"gaming"}); err != nil {
			t.Fatal(err)
		}
	}
	restore := libraryPageSize
	libraryPageSize = 1
	t.Cleanup(func() { libraryPageSize = restore })

	h := f.handler(session(f.owner))
	first := get(h, "/?tag=gaming&mine=1")
	if first.Code != 200 {
		t.Fatalf("page one = %d", first.Code)
	}
	one := first.Body.String()
	mustNotContain(t, one, "Newest clips", "data-newest")
	href := nextLink(one)
	if href == "" {
		t.Fatalf("page one rendered no next-page link:\n%s", one)
	}

	second := get(h, href)
	if second.Code != 200 {
		t.Fatalf("following %q = %d", href, second.Code)
	}
	two := second.Body.String()
	mustContain(t, two, "Newest clips", "data-newest")
	back := newestLink(two)
	if back == "" {
		t.Fatalf("a cursored page rendered no way back:\n%s", two)
	}
	u, err := url.Parse(back)
	if err != nil {
		t.Fatal(err)
	}
	if qs := u.Query(); qs.Get("tag") != "gaming" || qs.Get("mine") != "1" || qs.Has("cursor") {
		t.Fatalf("the newest-clips link %q must carry every filter and no cursor", back)
	}
	// And it lands on the first page of exactly that filter: the same rows page
	// one showed, and no way back offered there either.
	rec := get(h, back)
	if rec.Code != 200 {
		t.Fatalf("following %q = %d", back, rec.Code)
	}
	body := rec.Body.String()
	mustNotContain(t, body, "data-newest")
	got, want := clipIDsOn(body), clipIDsOn(one)
	if len(got) == 0 || len(want) == 0 || got[0] != want[0] {
		t.Fatalf("the way back listed %v; page one listed %v", got, want)
	}
	assertNoInline(t, "index page two", two)
}

// clipIDsOn returns the clip ids a rendered library page lists, in order.
func clipIDsOn(body string) []string {
	var out []string
	const marker = `data-clip-id="`
	for i := strings.Index(body, marker); i >= 0; i = strings.Index(body, marker) {
		body = body[i+len(marker):]
		j := strings.Index(body, `"`)
		if j < 0 {
			break
		}
		out = append(out, body[:j])
		body = body[j:]
	}
	return out
}

// TestLibraryPageLinksRoundTrip is the pin the two encoders never had (N044).
//
// `filterQuery` (pages.go) mints a page link with the keys `cursor`/`tag`/`q`/
// `mine`; `listFilter.encode` (internal/db/library.go) mints the cursor's own
// filter field with `t`/`q`/`mine`. The two are deliberately NOT shared — one
// escapes for a URL a human sees, the other rides inside an opaque token that
// decodeFilter re-validates — but ListClips REFUSES a cursor whose filter does
// not equal the query's (§20 errata 81, 115). So if either side drops a filter,
// "Older clips" under that filter answers ErrInvalidCursor and bounces to the
// first page, and nothing failed. Nothing round-tripped a rendered link before
// this test: NextURL is produced in pages.go and consumed only by index.html.
//
// Each case renders page one, follows the link it rendered, and demands three
// things: 200 rather than the 303 bounce, ids that CONTINUE rather than restart
// (a bounce would look like a pass otherwise), and the filter still shown.
func TestLibraryPageLinksRoundTrip(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// Three of the owner's own clips share a tag and a searchable word, so
	// every filter below matches more than one row and can therefore page.
	for _, c := range []db.Clip{f.ready, f.failed, f.queued} {
		if err := f.db.SetClipTags(ctx, c.ID, []string{"gaming"}); err != nil {
			t.Fatal(err)
		}
	}
	restore := libraryPageSize
	libraryPageSize = 1
	t.Cleanup(func() { libraryPageSize = restore })

	h := f.handler(session(f.owner))
	for _, tc := range []struct {
		name, url string
		reflected []string
	}{
		{"tag", "/?tag=gaming", []string{`data-tag-filter="gaming"`}},
		{"q", "/?q=e", []string{`data-q-filter="e"`, `value="e"`}},
		{"mine", "/?mine=1", []string{`data-mine-filter="1"`, `name="mine" value="1" checked`}},
		{"tag+q+mine", "/?tag=gaming&q=e&mine=1",
			[]string{`data-tag-filter="gaming"`, `data-q-filter="e"`, `data-mine-filter="1"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := get(h, tc.url)
			if first.Code != 200 {
				t.Fatalf("page one of %s = %d", tc.url, first.Code)
			}
			body := first.Body.String()
			href := nextLink(body)
			if href == "" {
				t.Fatalf("%s rendered no next-page link:\n%s", tc.url, body)
			}
			second := get(h, href)
			if second.Code != 200 {
				t.Fatalf("following %q = %d (a 303 here is the cursor being refused because a filter was dropped)",
					href, second.Code)
			}
			one, two := clipIDsOn(body), clipIDsOn(second.Body.String())
			if len(one) == 0 || len(two) == 0 {
				t.Fatalf("%s: page one listed %v, page two listed %v", tc.url, one, two)
			}
			for _, id := range two {
				for _, seen := range one {
					if id == seen {
						t.Fatalf("%s: page two restarted the list (%s appears on both pages)", tc.url, id)
					}
				}
			}
			// …and the filter is still in force on the page the link led to,
			// which is the other half of "the link carried it".
			for _, want := range tc.reflected {
				if !strings.Contains(second.Body.String(), want) {
					t.Errorf("%s: page two does not reflect the filter (%q missing)", tc.url, want)
				}
			}
		})
	}
}

// TestIndexLibraryScopeOwn: the page renders the caller's own clips, says so,
// and offers no "mine only" toggle it could not turn off.
func TestIndexLibraryScopeOwn(t *testing.T) {
	f := newFixture(t)
	f.libraryScope = config.LibraryScopeOwn
	h := f.handler(session(f.owner))
	body := get(h, "/").Body.String()
	mustContain(t, body, `data-library-scope="own"`, `data-clip-id="`+f.ready.ID+`"`)
	mustNotContain(t, body, f.otherListed.ID, f.otherUnlisted.ID, `name="mine"`)
	assertNoInline(t, "index scope=own", body)

	// The friend sees theirs and only theirs.
	friend := get(f.handler(session(f.other)), "/").Body.String()
	mustContain(t, friend, `data-clip-id="`+f.otherListed.ID+`"`, `data-clip-id="`+f.otherUnlisted.ID+`"`)
	mustNotContain(t, friend, f.ready.ID, f.failed.ID)

	// The default is shared: nothing changes for a deployment that never set
	// the variable.
	f.libraryScope = ""
	mustContain(t, get(f.handler(session(f.owner)), "/").Body.String(), `data-clip-id="`+f.otherListed.ID+`"`)
}

// TestIndexRowBadges: the duration and the resolution a ready row already
// knows (ROADMAP #30) — and nothing at all on a row that has not been
// measured yet, because a "0:00" that later becomes "0:06" is a wrong fact
// rather than a missing one.
func TestIndexRowBadges(t *testing.T) {
	f := newFixture(t)
	body := get(f.handler(session(f.owner)), "/").Body.String()
	ready := rowOf(t, body, f.ready.ID)
	mustContain(t, ready, `<span class="duration" data-duration>0:06</span>`, `<span data-resolution>1280×720</span>`)
	queued := rowOf(t, body, f.queued.ID)
	mustNotContain(t, queued, `data-duration`, `data-resolution`)
	assertNoInline(t, "index badges", body)
}

// TestIndexBulkBarIsHiddenWithoutJS: the controls are server-rendered so the
// markup is one thing, but they are inert until app.js reveals them — every
// action they offer is still available one clip at a time on the clip's own
// page.
func TestIndexBulkBarIsHiddenWithoutJS(t *testing.T) {
	f := newFixture(t)
	body := get(f.handler(session(f.owner)), "/").Body.String()
	mustContain(t, body,
		`<div class="bulk" role="group" aria-label="Bulk actions" data-bulk hidden>`,
		`data-bulk-action="delete"`, `data-bulk-action="ttl"`, `data-bulk-action="tag"`)
	// A checkbox per OWN row and none on a stranger's: the API answers 403 for
	// somebody else's clip, so offering the box would be offering an error.
	if !strings.Contains(rowOf(t, body, f.ready.ID), `data-pick`) {
		t.Fatal("an own row has no bulk checkbox")
	}
	if strings.Contains(rowOf(t, body, f.otherListed.ID), `data-pick`) {
		t.Fatal("a stranger's row offers a bulk checkbox")
	}
	// …but it holds the same cell open, empty and out of the accessibility
	// tree, so a mixed library keeps ONE left edge for its thumbnails once
	// app.js reveals the column.
	if !strings.Contains(rowOf(t, body, f.otherListed.ID), `<span class="pick" aria-hidden="true"></span>`) {
		t.Fatal("a stranger's row does not reserve the tick column")
	}
	// It is NOT a <form>: POST /api/clips/bulk takes JSON, so a form that
	// could be submitted without JavaScript could only ever be a 415.
	if strings.Contains(body, `action="/api/clips/bulk"`) {
		t.Fatal("the bulk bar is a form that could be submitted without a client")
	}
	assertNoInline(t, "index bulk bar", body)
}

// TestIndexEmptyStatesSayWhy: three filtered empties and one unfiltered one.
func TestIndexEmptyStatesSayWhy(t *testing.T) {
	f := newFixture(t)
	h := f.handler(session(f.owner))
	mustContain(t, get(h, "/?tag=zzz").Body.String(), `data-empty>No clips tagged "zzz".`)
	mustContain(t, get(h, "/?q=zzzznothing").Body.String(), `data-empty>No clips match that.`)
	mustContain(t, get(h, "/?tag=zzz&q=zzz").Body.String(), `data-empty>No clips match that.`)
	lonely := f.handler(session(f.user("lonely")))
	mustContain(t, get(lonely, "/?mine=1").Body.String(), `data-empty>No clips match that.`)
	mustContain(t, get(lonely, "/").Body.String(), `No clips yet — upload one above.`)
}

// rowOf slices one library row out of a rendered page.
func rowOf(t *testing.T, body, clipID string) string {
	t.Helper()
	i := strings.Index(body, `data-clip-id="`+clipID+`"`)
	if i < 0 {
		t.Fatalf("no row for %s", clipID)
	}
	start := strings.LastIndex(body[:i], "<li")
	rest := body[start:]
	j := strings.Index(rest, "</li>")
	if j < 0 {
		t.Fatalf("unterminated row for %s", clipID)
	}
	return rest[:j]
}
