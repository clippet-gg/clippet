package web

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"clippet/internal/db"
	"clippet/internal/ids"
)

// mkCollection creates a collection owned by owner and sets its membership.
func (f *fixture) mkCollection(owner db.User, title string, members ...db.Clip) db.Collection {
	f.t.Helper()
	ctx := context.Background()
	col, err := f.db.CreateCollection(ctx, ids.New(), owner.ID, title, f.now)
	if err != nil {
		f.t.Fatalf("CreateCollection: %v", err)
	}
	f.now++
	if len(members) > 0 {
		clipIDs := make([]string, len(members))
		for i, c := range members {
			clipIDs[i] = c.ID
		}
		if err := f.db.SetCollectionClips(ctx, col.ID, owner.ID, clipIDs, f.now); err != nil {
			f.t.Fatalf("SetCollectionClips: %v", err)
		}
	}
	return col
}

// TestCollectionPageAnonymous: the list, its links, the LINK-OUT card built
// from the first ready member's poster, and nothing else — no view count, no
// owner name, no controls, no player tags.
func TestCollectionPageAnonymous(t *testing.T) {
	f := newFixture(t)
	col := f.mkCollection(f.owner, "Best of 2026", f.processing, f.ready)

	rec := get(f.handler(passAuth{}), "/c/"+col.ID)
	assertPageHeaders(t, rec, 200, "private, no-store")
	assertNonEmbedHeaders(t, rec) // a collection page is framed by nobody
	body := rec.Body.String()
	mustContain(t, body,
		`<title>Best of 2026</title>`,
		`<h1 data-collection-title>Best of 2026</h1>`,
		`<link rel="canonical" href="`+testBase+`/c/`+col.ID+`">`,
		// the members, in POSITION order: the processing one first
		`data-clip-id="`+f.processing.ID+`" data-state="processing"`,
		`data-clip-id="`+f.ready.ID+`" data-state="ready"`,
		`href="/`+f.ready.ID+`"`,
		`<img src="/api/clips/`+f.ready.ID+`/poster.jpg"`,
		`<span class="badge badge-processing">processing</span>`,
		// the card: link-out, the first READY member's poster, no player
		`<meta property="og:type" content="website">`,
		`<meta property="og:title" content="Best of 2026">`,
		`<meta property="og:url" content="`+testBase+`/c/`+col.ID+`">`,
		`<meta property="og:site_name" content="example.test">`,
		`<meta property="og:image" content="`+testBase+`/api/clips/`+f.ready.ID+`/poster.jpg">`,
		`<meta property="og:image:width" content="640">`,
		`<meta property="og:image:height" content="360">`,
		`<meta name="twitter:card" content="summary_large_image">`,
		`<meta name="twitter:image" content="`+testBase+`/api/clips/`+f.ready.ID+`/poster.jpg">`,
	)
	// A collection is several clips: there is no single video to unfurl, so
	// there is no player and no video tag anywhere.
	mustNotContain(t, body, `og:video`, `twitter:player`, `<video`)
	// Anti-features: no counter, no author, no controls for a visitor.
	mustNotContain(t, body, `data-views`, `carol`, `data-collection-controls`,
		`data-collection-rename`, `data-collection-members`, `data-collection-delete`, `data-copy-link`)
	// The wordmark alone, linking home, like the share page (§20 erratum 244).
	mustContain(t, body, `<header class="top">`,
		`<a class="wordmark" href="/" aria-label="clippet">cli<span class="pp">pp</span>et</a>`)
	mustNotContain(t, body, `<nav>`, `data-logout`)
	assertNoInline(t, "collection anon", body)
}

// TestCollectionPageOwner: the same page plus the controls, and a membership
// picker whose CURRENT members come first in their current order — document
// order is the order the PUT sends, which is what makes saving an unchanged
// set a no-op rather than a reshuffle.
func TestCollectionPageOwner(t *testing.T) {
	f := newFixture(t)
	col := f.mkCollection(f.owner, "Best of 2026", f.ready)

	rec := get(f.handler(session(f.owner)), "/c/"+col.ID)
	assertPageHeaders(t, rec, 200, "private, no-store")
	body := rec.Body.String()
	mustContain(t, body,
		`data-collection-controls`,
		`data-collection-id="`+col.ID+`"`,
		`data-page-url="`+testBase+`/c/`+col.ID+`"`,
		`data-api-url="/api/collections/`+col.ID+`"`,
		`data-clips-url="/api/collections/`+col.ID+`/clips"`,
		`data-collection-rename`, `data-collection-members`, `data-copy-link`,
		`<div class="picker" data-members-picker>`,
		// the current member, ticked — and carrying its card image, which is what
		// makes the list pickable by sight (§20 erratum 225)
		`<label class="check choice"><input type="checkbox" name="clip_ids" value="`+f.ready.ID+`" checked="checked">`+
			`<img class="choice-thumb" src="/api/clips/`+f.ready.ID+`/poster.jpg" alt="" loading="lazy" width="80" height="45">`+
			`<span class="choice-title">E2E std</span></label>`,
		// a non-member, offered with its state so an owner can see what it is —
		// and with NO thumbnail: a clip that is still encoding has no poster, and
		// `poster.jpg` would answer 409 for it
		`<label class="check choice"><input type="checkbox" name="clip_ids" value="`+f.processing.ID+`">`+
			`<span class="choice-title">Mid encode (processing)</span></label>`,
		// removing members takes a second click, like the delete below
		`data-members-hint hidden`,
		// delete is TWO CLICKS, never a confirm() dialog
		`data-collection-delete data-confirm-text="Really delete"`,
		`data-delete-hint hidden`,
	)
	// The picker offers the owner's OWN clips and nobody else's: the API
	// refuses anything else with 400 not_your_clip.
	mustNotContain(t, body, `value="`+f.otherListed.ID+`"`, `value="`+f.otherUnlisted.ID+`"`, `value="`+f.deleting.ID+`"`)
	// Still no counter anywhere, owner or not.
	mustNotContain(t, body, `data-views`)
	assertNoInline(t, "collection owner", body)

	// Another logged-in user gets the visitor's page.
	body = get(f.handler(session(f.other)), "/c/"+col.ID).Body.String()
	mustNotContain(t, body, `data-collection-controls`, `data-collection-rename`)
	// …and so does the owner's own capture TOKEN (§20 erratum 19).
	body = get(f.handler(bearer(f.owner)), "/c/"+col.ID).Body.String()
	mustNotContain(t, body, `data-collection-controls`)
}

// TestCollectionPickerIsACheckboxList pins the mechanism, not just the markup
// (§20 erratum 95). The picker was a <select multiple>, where an unmodified
// click REPLACES the selection: one stray click on a ten-clip collection sent
// a PUT holding a single id and silently dropped the other nine, with no diff
// and no undo, and the reload that followed showed the damage as done. A
// checkbox toggles only itself. What must not change with it is the ORDER —
// document order is what the client PUTs — so the current members still come
// first, in their position order, and the offer is still the owner's own clips
// alone.
func TestCollectionPickerIsACheckboxList(t *testing.T) {
	f := newFixture(t)
	// Members in a deliberately NON-library order: queued was created last.
	col := f.mkCollection(f.owner, "Ordered", f.queued, f.ready)
	body := get(f.handler(session(f.owner)), "/c/"+col.ID).Body.String()

	// Not one <select> anywhere on the page: the collection page has no other.
	if strings.Contains(body, "<select") {
		t.Error("the collection page still renders a <select>")
	}
	// One checkbox per choice, in Choices order: the two members first, in
	// position order, then the rest of the owner's clips.
	boxes := checkboxRE.FindAllStringSubmatch(body, -1)
	got := make([]string, 0, len(boxes))
	checked := map[string]bool{}
	for _, m := range boxes {
		got = append(got, m[1])
		checked[m[1]] = strings.Contains(m[0], `checked="checked"`)
	}
	if len(got) < 2 || got[0] != f.queued.ID || got[1] != f.ready.ID {
		t.Fatalf("members are not first in position order: %v", got)
	}
	for _, c := range []db.Clip{f.queued, f.ready} {
		if !checked[c.ID] {
			t.Errorf("member %s is not checked", c.ID)
		}
	}
	for _, c := range []db.Clip{f.processing, f.failed} {
		if !contains(got, c.ID) {
			t.Errorf("the owner's clip %s is not offered", c.ID)
		}
		if checked[c.ID] {
			t.Errorf("non-member %s is checked", c.ID)
		}
	}
	// Somebody else's clips, and a clip on its way out, are never offered:
	// the API answers 400 not_your_clip, so offering one is offering an error.
	for _, c := range []db.Clip{f.otherListed, f.otherUnlisted, f.deleting} {
		if contains(got, c.ID) {
			t.Errorf("%s must not be in the picker", c.ID)
		}
	}
	assertNoInline(t, "collection picker", body)
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// checkboxRE finds each membership checkbox and captures the clip id it
// carries, in document order — which IS the order the client PUTs.
var checkboxRE = regexp.MustCompile(`<input type="checkbox" name="clip_ids" value="([^"]+)"[^>]*>`)

// TestCollectionPageWithoutAReadyClip: no poster exists, so the card carries
// no image at all and degrades to `summary` — the processing placeholder
// belongs to a CLIP that is processing and would say the wrong thing here.
func TestCollectionPageWithoutAReadyClip(t *testing.T) {
	f := newFixture(t)
	col := f.mkCollection(f.owner, "Nothing yet", f.processing)
	body := get(f.handler(passAuth{}), "/c/"+col.ID).Body.String()
	mustContain(t, body, `<meta name="twitter:card" content="summary">`, `<meta property="og:type" content="website">`)
	mustNotContain(t, body, `og:image`, `twitter:image`, `card-placeholder`)

	empty := f.mkCollection(f.owner, "Empty")
	body = get(f.handler(passAuth{}), "/c/"+empty.ID).Body.String()
	mustContain(t, body, `<meta name="twitter:card" content="summary">`, `This collection is empty.`)
	mustNotContain(t, body, `og:image`)
	assertNoInline(t, "empty collection", body)
}

// TestCollectionPageTrue404: the same discipline as the share page — an
// invalid id and an unknown id are 404s, and nothing else is.
func TestCollectionPageTrue404(t *testing.T) {
	f := newFixture(t)
	h := f.handler(session(f.owner))
	for _, path := range []string{"/c/nope", "/c/AAAAAAAAAAAA", "/c/" + f.ready.ID + "x"} {
		rec := get(h, path)
		if rec.Code != 404 {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
		body := rec.Body.String()
		mustContain(t, body, `<meta name="robots" content="noindex">`)
		mustNotContain(t, body, `og:title`, `og:image`)
	}
}

func TestCollectionPage503OnClosedDB(t *testing.T) {
	f := newFixture(t)
	col := f.mkCollection(f.owner, "Doomed", f.ready)
	h := f.handler(session(f.owner))
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	rec := get(h, "/c/"+col.ID)
	if rec.Code != 503 || rec.Header().Get("Retry-After") != retryAfterSeconds {
		t.Fatalf("closed DB = %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	// A backend failure must NEVER render as a 404 with no card: Discord
	// caches "no embed" per URL (§9.1).
	mustNotContain(t, rec.Body.String(), `og:title`)
}

func TestCollectionPageMethodNotAllowed(t *testing.T) {
	f := newFixture(t)
	col := f.mkCollection(f.owner, "Mine")
	rec := do(f.handler(session(f.owner)), http.MethodPost, "/c/"+col.ID)
	if rec.Code != 405 || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST /c/{id} = %d, Allow %q", rec.Code, rec.Header().Get("Allow"))
	}
}

// TestIndexCollectionsSection: the library lists the OWNER's collections and
// offers the one-field create form — the only place a collection is ever
// listed (§20 erratum 82).
func TestIndexCollectionsSection(t *testing.T) {
	f := newFixture(t)
	mine := f.mkCollection(f.owner, "Best of 2026", f.ready)
	theirs := f.mkCollection(f.other, "Friend's list")

	body := get(f.handler(session(f.owner)), "/").Body.String()
	mustContain(t, body,
		`data-collection-list`,
		`data-collection-id="`+mine.ID+`"`,
		`href="/c/`+mine.ID+`"`, `Best of 2026`,
		`<span class="sub">1 clip</span>`, // one clip is not "1 clips"
		`data-collection-new action="/api/collections"`,
	)
	mustNotContain(t, body, theirs.ID, `Friend's list`)
	assertNoInline(t, "index with collections", body)

	// A user with none sees the hint and the form.
	lonely := f.user("lonely")
	body = get(f.handler(session(lonely)), "/").Body.String()
	mustContain(t, body, `data-collections-empty>No collections yet.`, `data-collection-new`)
	mustNotContain(t, body, mine.ID)
}

// TestCollectionPageEscapesHostileText: a title is user text and lands in the
// <title>, the <h1>, the rename input's value and every card tag.
func TestCollectionPageEscapesHostileText(t *testing.T) {
	f := newFixture(t)
	col := f.mkCollection(f.owner, `"><script>alert(1)</script>`)
	body := get(f.handler(session(f.owner)), "/c/"+col.ID).Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("the collection title was not escaped")
	}
	mustContain(t, body, `alert(1)`) // present, escaped
	assertNoInline(t, "hostile collection title", body)
}
