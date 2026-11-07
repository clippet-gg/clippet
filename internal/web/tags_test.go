package web

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

// TestSharePageTagsAreOwnerOnly is the §9 rule applied to #9: the ANONYMOUS
// watch page is the clip and nothing else, so it renders no tag markup at all
// — not the editor, not a read-only chip, nothing — while the owner gets the
// editor pre-filled from the clip.
func TestSharePageTagsAreOwnerOnly(t *testing.T) {
	f := newFixture(t)
	if err := f.db.SetClipTags(context.Background(), f.ready.ID, []string{"clutch", "gaming"}); err != nil {
		t.Fatal(err)
	}

	anon := get(f.handler(passAuth{}), "/"+f.ready.ID).Body.String()
	mustNotContain(t, anon, `data-tags-edit`, `data-tags-input`, `data-tags-status`,
		`clutch`, `gaming`, `Tags`)
	assertNoInline(t, "clip anon with tags", anon)

	owner := get(f.handler(session(f.owner)), "/"+f.ready.ID).Body.String()
	mustContain(t, owner,
		`data-tags-edit data-api-url="/api/clips/`+f.ready.ID+`"`,
		`value="clutch, gaming" data-tags-input`,
		`data-tags-status`)
	assertNoInline(t, "clip owner with tags", owner)

	// Another logged-in user is not the owner, and neither is the owner's own
	// capture token (§20 erratum 19).
	other := get(f.handler(session(f.other)), "/"+f.ready.ID).Body.String()
	mustNotContain(t, other, `data-tags-edit`, `clutch`)
	tok := get(f.handler(bearer(f.owner)), "/"+f.ready.ID).Body.String()
	mustNotContain(t, tok, `data-tags-edit`, `clutch`)

	// An untagged clip still gets the editor — empty.
	empty := get(f.handler(session(f.owner)), "/"+f.failed.ID).Body.String()
	mustContain(t, empty, `value="" data-tags-input`)
}

// TestIndexTagChips: the library shows each row's tags as plain links back
// into the filter. No counts, no ranking, no /tags/x page — the filter IS the
// only tag surface (§17 #9 anti-features).
func TestIndexTagChips(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.db.SetClipTags(ctx, f.ready.ID, []string{"clutch", "gaming"}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetClipTags(ctx, f.otherListed.ID, []string{"gaming"}); err != nil {
		t.Fatal(err)
	}
	h := f.handler(session(f.owner))
	body := get(h, "/").Body.String()
	mustContain(t, body,
		`<div class="tags" data-tags>`,
		`<a class="tag" href="/?tag=clutch">clutch</a>`,
		`<a class="tag" href="/?tag=gaming">gaming</a>`)
	// A clip with no tags carries no tags div at all.
	if n := strings.Count(body, `data-tags>`); n != 2 {
		t.Errorf("tag blocks = %d, want 2 (only the two tagged clips)", n)
	}
	assertNoInline(t, "index with tag chips", body)
}

// TestIndexTagFilter: ?tag= narrows the same visibility set, says so, and
// carries the tag into the "older clips" cursor link — a cursor is minted
// under its filter and a page that dropped it would answer 400.
func TestIndexTagFilter(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.db.SetClipTags(ctx, f.ready.ID, []string{"gaming"}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetClipTags(ctx, f.otherUnlisted.ID, []string{"gaming"}); err != nil {
		t.Fatal(err)
	}
	h := f.handler(session(f.owner))
	rec := get(h, "/?tag=gaming")
	assertPageHeaders(t, rec, 200, "no-store")
	body := rec.Body.String()
	mustContain(t, body, `data-tag-filter="gaming"`, `Showing clips tagged`, `<a href="/">show all</a>`,
		`data-clip-id="`+f.ready.ID+`"`)
	// The filter never widens the set: the friend's UNLISTED clip carries the
	// same tag and is still invisible.
	mustNotContain(t, body, f.otherUnlisted.ID, f.processing.ID, f.queued.ID)
	assertNoInline(t, "index filtered", body)

	// An uppercase filter is the same filter.
	if !strings.Contains(get(h, "/?tag=GAMING").Body.String(), `data-tag-filter="gaming"`) {
		t.Error("an uppercase ?tag= did not normalise")
	}
	// A tag that cannot exist bounces to the whole library, exactly as a
	// garbage cursor does — there is nothing to render for a word no clip can
	// carry, and a page is not an API (§20 erratum 81).
	rec = get(h, "/?tag=NOT%20OK")
	if rec.Code != 303 || rec.Header().Get("Location") != "/" {
		t.Fatalf("illegal ?tag= = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	assertPrivateRedirect(t, rec)
	// The unfiltered library shows no filter line.
	mustNotContain(t, get(h, "/").Body.String(), `data-tag-filter`)
}

// TestIndexTagFilterEmptyStateSaysWhy: a LEGAL tag that matches nothing is the
// reachable case — a chip that has since been cleared from every clip, or a
// bookmarked filter URL. The library used to answer "No clips yet.", directly
// contradicting the filter line above it and telling a reader their library
// was empty when it is not.
func TestIndexTagFilterEmptyStateSaysWhy(t *testing.T) {
	f := newFixture(t)
	h := f.handler(session(f.owner))
	body := get(h, "/?tag=zzz").Body.String()
	mustContain(t, body, `data-tag-filter="zzz"`, `data-empty>No clips tagged "zzz".`)
	mustNotContain(t, body, `No clips yet`)
	assertNoInline(t, "index empty filter", body)

	// The unfiltered empty state still points at the form above it, and a
	// library with rows keeps its hint hidden whichever text it holds.
	lonely := get(f.handler(session(f.user("lonely"))), "/?tag=zzz").Body.String()
	mustContain(t, lonely, `No clips tagged "zzz".`)
	mustContain(t, get(h, "/").Body.String(), `data-empty hidden="hidden"`, `No clips yet — upload one above.`)
}

// TestIndexTagFilterCursorKeepsTheTag pins the pagination link: the cursor a
// filtered page mints is only valid under that filter, so the link must carry
// both.
func TestIndexTagFilterCursorKeepsTheTag(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, c := range []string{f.ready.ID, f.processing.ID, f.failed.ID} {
		if err := f.db.SetClipTags(ctx, c, []string{"gaming"}); err != nil {
			t.Fatal(err)
		}
	}
	// One row per page makes the "older clips" link appear.
	restore := libraryPageSize
	libraryPageSize = 1
	t.Cleanup(func() { libraryPageSize = restore })

	h := f.handler(session(f.owner))
	body := get(h, "/?tag=gaming").Body.String()
	i := strings.Index(body, `class="more"`)
	if i < 0 {
		t.Fatalf("no pagination link on a filtered page: %s", body)
	}
	link := body[i:]
	if j := strings.Index(link, `</p>`); j >= 0 {
		link = link[:j]
	}
	if !strings.Contains(link, "tag=gaming") || !strings.Contains(link, "cursor=") {
		t.Fatalf("the older-clips link = %q; it must carry both the cursor and the tag", link)
	}
	// And following it works: the tag survives the round trip.
	href := link[strings.Index(link, `href="`)+len(`href="`):]
	href = href[:strings.Index(href, `"`)]
	href = strings.ReplaceAll(href, "&amp;", "&")
	if _, err := url.Parse(href); err != nil {
		t.Fatal(err)
	}
	rec := get(h, href)
	if rec.Code != 200 {
		t.Fatalf("following %q = %d", href, rec.Code)
	}
	mustContain(t, rec.Body.String(), `data-tag-filter="gaming"`)
}

// TestIndexStaleCursorKeepsTheTag: a page token that no longer decodes under
// the current filter is a reason to go back to the FIRST page of that filter,
// not to throw the filter away. The reader is looking at `?tag=x` and asked
// for more of it (§20 erratum 87). An illegal TAG still bounces to `/`,
// because there is no filter left to keep.
func TestIndexStaleCursorKeepsTheTag(t *testing.T) {
	f := newFixture(t)
	h := f.handler(session(f.owner))
	for _, tc := range []struct{ url, want string }{
		{"/?tag=gaming&cursor=garbage", "/?tag=gaming"},
		{"/?cursor=garbage", "/"},
		{"/?tag=NOT%20OK&cursor=garbage", "/"},
	} {
		rec := get(h, tc.url)
		if rec.Code != 303 || rec.Header().Get("Location") != tc.want {
			t.Errorf("%s -> %d %q, want 303 %q", tc.url, rec.Code, rec.Header().Get("Location"), tc.want)
		}
		assertPrivateRedirect(t, rec)
	}
}
