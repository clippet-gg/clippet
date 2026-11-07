package web

import (
	"context"
	"strings"
	"testing"

	"clippet/internal/db"
	"clippet/internal/httpx"
)

// --- the share page's collection control (ROADMAP #17) ---------------------

// TestSharePageCollectionsAreOwnerOnly: the checkboxes carry the single-clip
// membership routes, they are ticked from the row and not from a request, and
// a visitor's page has no collection markup at all.
func TestSharePageCollectionsAreOwnerOnly(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	in, err := f.db.CreateCollection(ctx, "CoLLeCtIoN12", f.owner.ID, "Best of", f.now)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.db.CreateCollection(ctx, "CoLLeCtIoN23", f.owner.ID, "Later", f.now)
	if err != nil {
		t.Fatal(err)
	}
	// A collection of somebody ELSE's must never appear on this page.
	if _, err := f.db.CreateCollection(ctx, "CoLLeCtIoN34", f.other.ID, "Not yours", f.now); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddClipToCollection(ctx, in.ID, f.owner.ID, f.ready.ID, f.now); err != nil {
		t.Fatal(err)
	}

	rec := get(f.handler(session(f.owner)), "/"+f.ready.ID)
	body := rec.Body.String()
	mustContain(t, body,
		"data-clip-collections",
		`data-member-url="/api/collections/`+in.ID+`/clips/`+f.ready.ID+`"`,
		`data-member-url="/api/collections/`+out.ID+`/clips/`+f.ready.ID+`"`,
		"Best of", "Later")
	mustNotContain(t, body, "Not yours", "CoLLeCtIoN34")
	// The member is ticked and the non-member is not — from the row, with no
	// request from the client.
	inBox := `data-member-url="/api/collections/` + in.ID + `/clips/` + f.ready.ID + `" data-collection-id="` + in.ID + `" checked="checked"`
	if !strings.Contains(body, inBox) {
		t.Errorf("the member checkbox is not ticked:\n%s", sliceAround(body, in.ID))
	}
	if strings.Contains(body, `data-collection-id="`+out.ID+`" checked`) {
		t.Errorf("a non-member checkbox is ticked:\n%s", sliceAround(body, out.ID))
	}
	assertNoInline(t, "clip", body)

	// A visitor, another user, and a Bearer principal — which is never an
	// owner (§20 errata 19, 22) — see none of it.
	for _, body := range []string{
		get(f.handler(passAuth{}), "/"+f.ready.ID).Body.String(),
		get(f.handler(session(f.other)), "/"+f.ready.ID).Body.String(),
		get(f.handler(bearer(f.owner)), "/"+f.ready.ID).Body.String(),
	} {
		mustNotContain(t, body, "data-clip-collections", "Best of", "/api/collections/")
	}
}

// sliceAround returns a window of the body around a needle, for a message.
func sliceAround(body, needle string) string {
	i := strings.Index(body, needle)
	if i < 0 {
		return "(needle absent)"
	}
	lo, hi := max(0, i-200), min(len(body), i+200)
	return body[lo:hi]
}

// --- oEmbed discovery (ROADMAP #18) ----------------------------------------

// TestSharePageCarriesTheOEmbedLink, and nothing else does.
func TestSharePageCarriesTheOEmbedLink(t *testing.T) {
	f := newFixture(t)
	h := f.handler(passAuth{})
	body := get(h, "/"+f.ready.ID).Body.String()
	mustContain(t, body,
		`<link rel="alternate" type="application/json+oembed"`,
		`href="`+testBase+"/oembed?format=json&amp;url=",
		// The page URL is percent-encoded as a QUERY VALUE: it carries a scheme
		// and slashes, and an unencoded one would end at the first `&`.
		"%3A%2F%2F", "%2F"+f.ready.ID+`"`)
	// A clip that is not ready still advertises it: the document is a `link`
	// card, which is exactly what a consumer should get for one.
	if !strings.Contains(get(h, "/"+f.processing.ID).Body.String(), "json+oembed") {
		t.Error("a processing clip advertises no oEmbed document")
	}
	// No other page does.
	for _, path := range []string{"/", "/login", "/c/" + f.ready.ID, "/embed/" + f.ready.ID} {
		if strings.Contains(get(f.handler(session(f.owner)), path).Body.String(), "json+oembed") {
			t.Errorf("%s advertises an oEmbed document", path)
		}
	}
}

// --- the library's stats strip and budget notice (ROADMAP #23, #24) --------

// TestLibraryStatsAreTheCallersOwn.
func TestLibraryStatsAreTheCallersOwn(t *testing.T) {
	f := newFixture(t)
	body := get(f.handler(session(f.owner)), "/").Body.String()
	mustContain(t, body, "data-account-stats", "data-stats-views=")
	// The owner has five rows in the fixture, one of which is a `deleting`
	// tombstone — already gone everywhere else, so it is not counted here
	// either — and one of the remaining four is ready. The friend's two are
	// not theirs, in either direction.
	mustContain(t, body, "4 clips", "1 ready")
	other := get(f.handler(session(f.other)), "/").Body.String()
	mustContain(t, other, "2 clips", "0 ready")
	assertNoInline(t, "index", body)
}

// TestLibraryBudgetNoticeOnlyWhenOverIt.
func TestLibraryBudgetNoticeOnlyWhenOverIt(t *testing.T) {
	f := newFixture(t)

	// (a) No holder at all: nothing is rendered, and no page fails.
	if body := get(f.handler(session(f.owner)), "/").Body.String(); strings.Contains(body, "data-storage-budget") {
		t.Error("a budget notice with no holder wired")
	}
	// (b) A holder with no measurement, and one under the line.
	f.budget = &httpx.StorageBudget{}
	if body := get(f.handler(session(f.owner)), "/").Body.String(); strings.Contains(body, "data-storage-budget") {
		t.Error("a budget notice before any measurement")
	}
	f.budget.Set(100, 1000)
	if body := get(f.handler(session(f.owner)), "/").Body.String(); strings.Contains(body, "data-storage-budget") {
		t.Error("a budget notice while under the budget")
	}
	// (c) Over it: the warning, with both numbers and the sentence that says
	//     nothing has stopped working.
	f.budget.Set(3000, 1000)
	body := get(f.handler(session(f.owner)), "/").Body.String()
	mustContain(t, body, "data-storage-budget", "2.9 KiB", "1000 bytes", "uploads are never refused")
	assertNoInline(t, "index", body)
	// (c') The same line armed as a cap (STORAGE_BUDGET_HARD=1, §20 erratum
	//      277): the notice says which mode it is in, and not the advisory sentence.
	f.budgetHard = true
	hard := get(f.handler(session(f.owner)), "/").Body.String()
	mustContain(t, hard, "data-storage-budget", "New uploads are being refused", "STORAGE_BUDGET_HARD=1")
	if strings.Contains(hard, "uploads are never refused") {
		t.Error("the capped notice still carries the advisory sentence")
	}
	assertNoInline(t, "index", hard)
	f.budgetHard = false
	// (d) A budget of 0 is OFF, whatever has been measured.
	f.budget.Set(3000, 0)
	if body := get(f.handler(session(f.owner)), "/").Body.String(); strings.Contains(body, "data-storage-budget") {
		t.Error("a budget notice with STORAGE_BUDGET_BYTES off")
	}
}

// --- the settings page's upload defaults (ROADMAP #22) ---------------------

// TestSettingsPageRendersTheAccountDefaults: the stored preference is what is
// selected, the "Server default" option names the instance's own answer, and
// the section is session-only like the rest of the page.
func TestSettingsPageRendersTheAccountDefaults(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	secs := int64(7 * 86400)
	f.defaultTTL = &secs

	body := get(f.handler(session(f.owner)), "/settings").Body.String()
	mustContain(t, body,
		`data-defaults-url="/api/auth/me/settings"`,
		"server default · 7 days",
		"data-defaults-ttl", "data-defaults-unlisted", "data-defaults-save")
	if !strings.Contains(body, `<option value=""selected="selected">server default`) &&
		!strings.Contains(body, `<option value="" selected="selected">server default`) {
		t.Errorf("the untouched account does not select the server default:\n%s", sliceAround(body, "server default"))
	}
	assertNoInline(t, "settings", body)

	// A stored preference selects its own option instead.
	if err := f.db.SetUserDefaults(ctx, f.owner.ID, db.UserDefaults{TTLDays: intp(30), Unlisted: true}); err != nil {
		t.Fatal(err)
	}
	body = get(f.handler(session(f.owner)), "/settings").Body.String()
	if !strings.Contains(body, `value="30" selected="selected"`) {
		t.Errorf("the stored TTL is not selected:\n%s", sliceAround(body, `value="30"`))
	}
	if !strings.Contains(body, `data-defaults-unlisted checked="checked"`) {
		t.Errorf("the stored unlisted default is not ticked:\n%s", sliceAround(body, "data-defaults-unlisted"))
	}
	// Session only: an anonymous caller is bounced to the login page, which is
	// what the whole page already does.
	rec := get(f.handler(passAuth{}), "/settings")
	if rec.Code != 303 || rec.Header().Get("Location") != "/login?next=/settings" {
		t.Errorf("anonymous /settings = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func intp(n int) *int { return &n }

// --- the collection page's per-row controls (ROADMAP #17) ------------------

// TestCollectionPageRowControlsAreOwnerOnlyAndInert.
func TestCollectionPageRowControlsAreOwnerOnlyAndInert(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	col, err := f.db.CreateCollection(ctx, "CoLLeCtIoN12", f.owner.ID, "Best of", f.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddClipToCollection(ctx, col.ID, f.owner.ID, f.ready.ID, f.now); err != nil {
		t.Fatal(err)
	}

	body := get(f.handler(session(f.owner)), "/c/"+col.ID).Body.String()
	mustContain(t, body,
		"data-row-actions hidden", // inert until a client reveals it
		`data-member-remove data-member-url="/api/collections/`+col.ID+`/clips/`+f.ready.ID+`"`,
		`data-member-move="up"`, `data-member-move="down"`)
	assertNoInline(t, "collection", body)

	// A visitor gets the list and none of the controls.
	visitor := get(f.handler(passAuth{}), "/c/"+col.ID).Body.String()
	mustContain(t, visitor, f.ready.Title)
	// (The page's own data-api-url / data-clips-url have always been rendered
	// for everybody — they address the collection, which is public — so what
	// must be absent is the CONTROLS and the per-clip membership route.)
	mustNotContain(t, visitor, "data-row-actions", "data-member-remove",
		"/api/collections/"+col.ID+"/clips/"+f.ready.ID)
}

// --- the owner's per-clip stored size (ROADMAP #23) ------------------------

// TestSharePageStoredSizeIsOwnerOnly.
func TestSharePageStoredSizeIsOwnerOnly(t *testing.T) {
	f := newFixture(t)
	owner := get(f.handler(session(f.owner)), "/"+f.ready.ID).Body.String()
	mustContain(t, owner, "data-stored", "<dt>Stored</dt>")
	for _, body := range []string{
		get(f.handler(passAuth{}), "/"+f.ready.ID).Body.String(),
		get(f.handler(session(f.other)), "/"+f.ready.ID).Body.String(),
		get(f.handler(bearer(f.owner)), "/"+f.ready.ID).Body.String(),
	} {
		mustNotContain(t, body, "data-stored", "<dt>Stored</dt>")
	}
}
