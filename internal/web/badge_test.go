package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"clippet/internal/auth"
)

// The instance badge (§20 erratum 285; web/STYLE.md): `INSTANCE_BADGE=dev` makes
// an instance say so, in the places a person actually looks — the mark's colour,
// a word beside it, the tab's title and the manifest's app name — and an unset
// badge changes nothing at all, anywhere. Since §20 erratum 287 the line Discord
// draws as the logo is NOT one of them: that line is the instance's own public
// host, which already tells dev.clippet.gg from clippet.gg.

// badgeInsertions are the EXACT bytes a badge of `dev` adds to a rendered page.
// They are listed here, once, and TestBadgedPageIsTheUnbadgedPagePlusTheBadge
// uses the list to reconstruct the unbadged page out of the badged one: the
// feature is proven additive by SUBTRACTING it, rather than by an enumeration of
// the things somebody remembered to look for.
//
// The two display sequences are removed BEFORE the header one, because the
// display close carries the same `<span class="instance">` inside it: longest
// first, so the pair opened by `<span class="lockup">` is taken whole.
var badgeInsertions = [][2]string{
	{` data-badge="dev"`, ""},                                                  // on <html>, beside data-theme
	{`<span class="lockup">`, ""},                                              // the display lockup opens…
	{`<span class="instance">dev</span></span>`, ""},                           // …the tag, and the lockup closes
	{"\n  <span class=\"instance\">dev</span>", ""},                            // the header's own tag
	{"<title>dev · ", "<title>"},                                               // the tab
	{` title="dev · `, ` title="`},                                             // the oEmbed discovery link's own title=
	{`og:site_name" content="clippet dev"`, `og:site_name" content="clippet"`}, // the card's site line
}

// unbadge turns a badged page back into the page the same instance would have
// served with no badge set at all.
//
// It COUNTS rather than ReplaceAll's (§20 erratum 285, amended): the badge
// inserts each of these sequences at most once per page, so a page carrying two
// of one of them is a bug — a tag rendered twice, a title prefixed twice — and
// ReplaceAll subtracted the duplicate along with the original, which made the
// reconstruction succeed and the test pass. A subtraction that removes more
// than was added proves nothing about what was added. Not every page carries
// every sequence (the display lockup is on four pages, the header tag on the
// others), so the rule is "at most one", and t.Fatalf names which and how many.
func unbadge(t *testing.T, body string) string {
	t.Helper()
	for _, ins := range badgeInsertions {
		switch n := strings.Count(body, ins[0]); {
		case n == 0:
			continue
		case n > 1:
			t.Fatalf("the badged page carries %q %d times; the badge inserts it once", ins[0], n)
		}
		body = strings.Replace(body, ins[0], ins[1], 1)
	}
	return body
}

// badgedHandlers builds two handlers over ONE fixture — the same rows, the same
// ids, the same clock — differing only in INSTANCE_BADGE.
func badgedHandlers(f *fixture, a auth.Authenticator) (plain, badged http.Handler) {
	f.instanceBadge = ""
	plain = f.handler(a)
	f.instanceBadge = "dev"
	badged = f.handler(a)
	f.instanceBadge = ""
	return plain, badged
}

// TestBadgedPageIsTheUnbadgedPagePlusTheBadge is the pin the whole feature rests
// on: every page rendered with `INSTANCE_BADGE=dev`, with the badge's own bytes
// removed again, is BYTE-IDENTICAL to the page the same fixture renders with no
// badge. Nothing else moved — not a class, not an attribute, not a space.
//
// It is the strongest form of "an unbadged instance is unchanged" this package
// can state, and it is the one that matters: the feature exists to be invisible
// on the instance where it is off, which is production.
func TestBadgedPageIsTheUnbadgedPagePlusTheBadge(t *testing.T) {
	f := newFixture(t)
	col := f.mkCollection(f.owner, "Badge collection", f.ready)
	for _, c := range []struct {
		name, path string
		session    bool
	}{
		{"the front door", "/", false},
		{"login", "/login", false},
		{"register", "/register", false},
		{"the 404 page", "/AAAAAAAAAAAA", false},
		{"the library", "/", true},
		{"settings", "/settings", true},
		{"a stranger's share page", "/" + f.ready.ID, false},
		{"the owner's share page", "/" + f.ready.ID, true},
		{"a processing share page", "/" + f.processing.ID, false},
		{"the collection page", "/c/" + col.ID, true},
		{"the embed", "/embed/" + f.ready.ID, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var a auth.Authenticator = passAuth{}
			if c.session {
				a = session(f.owner)
			}
			plain, badged := badgedHandlers(f, a)
			want := get(plain, c.path).Body.String()
			got := get(badged, c.path).Body.String()
			if want == "" {
				t.Fatalf("%s rendered nothing on the unbadged instance", c.path)
			}
			if got == want {
				t.Fatalf("%s is identical on a badged instance: the badge reached nothing", c.path)
			}
			if u := unbadge(t, got); u != want {
				t.Errorf("%s: the badged page minus the badge is not the unbadged page.\n--- want (unbadged) ---\n%s\n--- got (badged, then unbadged) ---\n%s", c.path, want, u)
			}
		})
	}
}

// TestBadgeOnEveryMark: the attribute, the tag and the title, on every page that
// carries a mark — and the ONE page that gets the attribute and no tag.
func TestBadgeOnEveryMark(t *testing.T) {
	f := newFixture(t)
	f.instanceBadge = "dev"
	anon := f.handler(passAuth{})
	owner := f.handler(session(f.owner))

	// The display mark: the front door, the two auth pages, the 404. The tag
	// FOLLOWS THE WORD there too (the owner's call; §20 erratum 285(c)), inside a
	// lockup that exists only on a badged page and keeps the mark where the
	// container had centred it.
	for _, p := range []string{"/", "/login", "/register", "/AAAAAAAAAAAA"} {
		body := get(anon, p).Body.String()
		mustContain(t, body, `<html lang="en" data-badge="dev">`,
			`<span class="lockup">`, `<span class="instance">dev</span></span>`, "<title>dev · ")
		i, j, k := strings.Index(body, `class="lockup"`), strings.Index(body, "wordmark display"), strings.Index(body, `class="instance"`)
		if i < 0 || j < i || k < j {
			t.Errorf("%s: want lockup, then the display wordmark, then the tag (got %d, %d, %d)", p, i, j, k)
		}
	}
	// The header mark: a session's library, and a stranger's share page — which
	// carries the wordmark ALONE (§20 erratum 244), plus this one word.
	for _, h := range []http.Handler{owner, anon} {
		mustContain(t, get(h, "/"+f.ready.ID).Body.String(),
			`<a class="wordmark" href="/" aria-label="clippet">cli<span class="pp">pp</span>et</a>`+"\n"+`  <span class="instance">dev</span>`)
	}
	mustContain(t, get(owner, "/").Body.String(), `<span class="instance">dev</span>`)

	// The embed: the attribute and NOTHING else. The overlay's whole signal is
	// the grey span the attribute produces (web/STYLE.md: no text beside it).
	emb := get(anon, "/embed/"+f.ready.ID).Body.String()
	mustContain(t, emb, `data-badge="dev"`, "<title>dev · ")
	mustNotContain(t, emb, `class="instance"`, `class="lockup"`)

	// The theme-color pair is untouched: a badge is not a theme.
	mustContain(t, get(anon, "/login").Body.String(),
		`<meta name="theme-color" media="(prefers-color-scheme: light)" content="#ECEDEF">`)
}

// TestBadgeLeavesTheSiteLineAlone (§20 erratum 287, superseding 285's one card
// tag): the site line is the instance's own public HOST on a badged instance and
// on an unbadged one, because it is the same box either way. The badge keeps the
// two surfaces a BROWSER shows — the `dev · ` title prefix and the manifest's
// `Clippet dev` — and reaches no card tag at all.
func TestBadgeLeavesTheSiteLineAlone(t *testing.T) {
	f := newFixture(t)
	f.instanceBadge = "dev"
	h := f.handler(passAuth{})
	host := mustHost(t, testBase)
	for _, id := range []string{f.ready.ID, f.processing.ID, f.failed.ID} {
		body := get(h, "/"+id).Body.String()
		mustContain(t, body, `<meta property="og:site_name" content="`+host+`">`)
		mustNotContain(t, body, `og:site_name" content="clippet`)
	}
}

// TestSiteLineIsThePublicHost (§20 erratum 287; the owner's call of 2026-09-15:
// the small grey line Discord draws above a card's title reads the host).
// Every card shape, and the collection card, name the instance's own host — no
// scheme, no port — and an instance whose URL builder has no host at all falls
// back to the product's spelling rather than emitting a blank provider row.
func TestSiteLineIsThePublicHost(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{"https://clippet.gg", "clippet.gg"},
		{"https://dev.clippet.gg", "dev.clippet.gg"},
		{"http://clips.example.com:8481", "clips.example.com"},
		{"http://localhost:8518", "localhost"},
		{"https://example.test/clippet", "example.test"},
	} {
		u := builder(t, tc.base)
		if got := siteNameOf(u.SiteName()); got != tc.want {
			t.Errorf("siteNameOf(%q) = %q, want %q", tc.base, got, tc.want)
		}
		got := metaMap(t, BuildMeta(ClipView{Clip: readyClip(), URLs: u}))["property og:site_name"]
		if got != tc.want {
			t.Errorf("BuildMeta og:site_name on %q = %q, want %q", tc.base, got, tc.want)
		}
		got = metaMap(t, BuildCollectionMeta(CollectionView{URLs: u}))["property og:site_name"]
		if got != tc.want {
			t.Errorf("BuildCollectionMeta og:site_name on %q = %q, want %q", tc.base, got, tc.want)
		}
	}
	if got := siteNameOf(""); got != siteName {
		t.Errorf("siteNameOf(\"\") = %q, want the product's own spelling %q", got, siteName)
	}
}

// mustHost is testBase's hostname — what every card on the test fixture's own
// instance now carries as its site line.
func mustHost(t *testing.T, base string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %q: %v", base, err)
	}
	return u.Hostname()
}

// TestManifestCarriesTheBadge: an installed dev app and an installed production
// app are two icons on one home screen, and the name under the icon is the only
// thing that separates them there — the tile itself is the same drawing. The
// name is the product in PROSE, like the document titles beside it (`Clippet
// dev`), never the wordmark's own lowercase spelling.
func TestManifestCarriesTheBadge(t *testing.T) {
	for _, tc := range []struct{ badge, want string }{{"", "Clippet"}, {"dev", "Clippet dev"}} {
		f := newFixture(t)
		f.instanceBadge = tc.badge
		rec := get(f.handler(passAuth{}), "/manifest.webmanifest")
		if rec.Code != 200 {
			t.Fatalf("badge %q: manifest status %d", tc.badge, rec.Code)
		}
		var m struct {
			Name      string `json:"name"`
			ShortName string `json:"short_name"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("badge %q: the manifest is not JSON: %v\n%s", tc.badge, err, rec.Body.String())
		}
		if m.Name != tc.want || m.ShortName != tc.want {
			t.Errorf("badge %q: manifest name/short_name = %q/%q, want %q", tc.badge, m.Name, m.ShortName, tc.want)
		}
		// Content-Length is computed per badge; a stale constant would truncate.
		if n := rec.Header().Get("Content-Length"); n != "" {
			if want := rec.Body.Len(); n != itoaTest(want) {
				t.Errorf("badge %q: Content-Length %s, body %d bytes", tc.badge, n, want)
			}
		}
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestNoBadgeIsNoBadge: the production instance. Nothing, anywhere.
func TestNoBadgeIsNoBadge(t *testing.T) {
	f := newFixture(t)
	h := f.handler(passAuth{})
	for _, p := range []string{"/", "/login", "/register", "/AAAAAAAAAAAA", "/" + f.ready.ID, "/embed/" + f.ready.ID} {
		body := get(h, p).Body.String()
		mustNotContain(t, body, "data-badge", `class="instance"`, `class="lockup"`, "<title>dev · ")
	}
	mustContain(t, get(h, "/"+f.ready.ID).Body.String(), `<meta property="og:site_name" content="`+mustHost(t, testBase)+`">`)
	mustContain(t, get(h, "/").Body.String(), "<title>Clippet</title>")
}

// docTitle is the whole of the title rule, so it is asserted as a function too:
// the badge word, one separator, and the title the handler asked for.
func TestDocTitle(t *testing.T) {
	for _, tc := range []struct{ badge, title, want string }{
		{"", "Clippet", "Clippet"},
		{"dev", "Clippet", "dev · Clippet"},
		{"staging", "Settings · Clippet", "staging · Settings · Clippet"},
	} {
		if got := docTitle(tc.badge, tc.title); got != tc.want {
			t.Errorf("docTitle(%q, %q) = %q, want %q", tc.badge, tc.title, got, tc.want)
		}
	}
}

// embedMarkDescenderOverhangPx is how far the plain overlay's own GLYPHS hang
// below their content box, and it is a MEASUREMENT rather than a guess:
// `.wordmark` is `line-height: 1`, so at `.embed-mark`'s 16 px the content box is
// exactly 16 px tall while the line box is 18 — the tails of p, q, y, g and j.
// Read in Chrome off the served sheet at this face, weight and size
// (`a.scrollHeight` 18 against `a.clientHeight` 16 for
// `clips.pygmy-quay.example.jp`) and confirmed by scanning the rendered pixels:
// the lowest row of glyph ink is the 17th below the box's top edge.
//
// Nothing in Go can re-measure a font, so this number is held still. The part
// that CAN move without anyone noticing — the shadow — is read out of the
// stylesheet instead, by textShadowDropPx below.
const embedMarkDescenderOverhangPx = 2

// TestEmbedOverlayIsBoundedOnTheHostForm (a nit from lane card): the plain-host
// form of the embed overlay carries a max-width and ellipsizes, so a
// self-hoster's ~40-character domain cannot run into the duration plate burned
// into the top-right of the same poster (§20 erratum 287(e)).
//
// It is asserted as a CSS unit test rather than in the visual pass because
// VISUAL_DRY — the only half of that pass a lane may run — stops before a
// browser exists, and the measurement would need one. What the pass DOES assert
// is the half a stylesheet cannot: that the box never leaves the frame.
//
// The bound is a CLIP, so the second half of this test is that it clips the
// mark's width and not its ink: a box that ends at the text's own content box
// cuts off every descender and the whole shadow under the line, and on this mark
// the shadow IS the legibility (287(h), amended).
func TestEmbedOverlayIsBoundedOnTheHostForm(t *testing.T) {
	f := newFixture(t)
	css := get(f.handler(passAuth{}), "/static/app.css").Body.String()

	const rule = ".embed-mark:not(:has(.pp)) { display: inline-block; max-width: calc(80% - 12px); padding-bottom: 5px; overflow: hidden; text-overflow: ellipsis; }"
	if !strings.Contains(css, rule) {
		t.Fatalf("app.css does not bound the plain-host overlay; want the rule\n%s", rule)
	}
	// THE CLIP MUST NOT EAT THE MARK. `overflow: hidden` clips at the PADDING box,
	// so the bound above is a horizontal bound with a vertical COST unless there is
	// room under the text: the descenders hang below the content box and the shadow
	// hangs below THEM. That shadow is not decoration here — #ECE9E2 on white is
	// 1.21:1, so over a bright poster the letters are carried by the dark edge under
	// them, and a mark that loses it is a mark nobody can read (web/STYLE.md, the
	// overlay paragraph). The room is recomputed from the sheet rather than spelled
	// as a number, so DEEPENING the shadow fails this test instead of quietly
	// costing the mark its bottom.
	plain := cssRuleBody(t, css, ".embed-mark:not(:has(.pp))")
	drop := textShadowDropPx(t, cssRuleBody(t, css, ".embed-mark"))
	need := embedMarkDescenderOverhangPx + drop
	// A rule with no `padding-bottom` at all has no room, which is the shape this
	// check exists for — so it is read as zero and answered by the message below,
	// rather than by a missing-declaration one that says nothing about why.
	room := 0.0
	if _, ok := cssDecl(plain, "padding-bottom"); ok {
		room = cssLengthPx(t, plain, "padding-bottom")
	}
	if room < need {
		t.Fatalf("the plain overlay clips its own ink: the box leaves %g px under the text and the mark paints %g px below it "+
			"(%d of descender, %g of shadow, the latter read off `text-shadow` as served) — "+
			"raise `padding-bottom` to %g px, or make the shadow shallower",
			room, need, embedMarkDescenderOverhangPx, drop, need)
	}
	// ...and the room goes UNDER the mark only. Padding on any other side moves the
	// text off the 10 px / 12 px insets errata 246 and 287 put it on, and on a
	// border-box sheet a side padding also eats the `max-width` this rule exists for.
	for _, side := range []string{"padding", "padding-top", "padding-left", "padding-right", "padding-inline", "padding-block"} {
		if _, ok := cssDecl(plain, side); ok {
			t.Fatalf("the plain overlay's bound carries `%s`, which moves the mark off its inset; only `padding-bottom` belongs here: %s", side, strings.TrimSpace(plain))
		}
	}
	// The BRANDED form keeps its overflow, and that is not a detail: `.pp::after`
	// draws the green span BELOW the text box, so an overflow: hidden anywhere on
	// `.embed-mark` itself would cut the span off the mark.
	for _, line := range strings.Split(css, "\n") {
		if strings.HasPrefix(line, ".embed-mark {") && strings.Contains(line, "overflow") {
			t.Fatalf("the overlay's own rule clips its overflow, which cuts the green span off:\n%s", line)
		}
	}
	// The two rules 80-front.sh reads as served are untouched.
	for _, want := range []string{
		".embed-mark { position: absolute; top: 10px; left: 12px;",
		"body.embed[data-playing] .embed-mark { display: none; }",
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("app.css lacks %q", want)
		}
	}
}

// cssRuleBody returns the declaration block of the one-line rule in css whose
// selector is exactly sel. app.css is written a rule to a line, which is what
// lets a test read one without a parser; a rule that stopped being one line fails
// here, and is told so rather than silently skipped.
func cssRuleBody(t *testing.T, css, sel string) string {
	t.Helper()
	for _, line := range strings.Split(css, "\n") {
		line = strings.TrimSpace(line)
		open := strings.Index(line, "{")
		shut := strings.LastIndex(line, "}")
		if open < 0 || shut < open {
			continue
		}
		if strings.TrimSpace(line[:open]) == sel {
			return line[open+1 : shut]
		}
	}
	t.Fatalf("app.css carries no one-line rule for the selector %q", sel)
	return ""
}

// cssDecl reads one declaration out of a block, without its surrounding spaces.
func cssDecl(body, prop string) (string, bool) {
	for _, d := range strings.Split(body, ";") {
		name, value, ok := strings.Cut(d, ":")
		if ok && strings.TrimSpace(name) == prop {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

// cssLengthPx reads a declaration whose value is a plain pixel length.
func cssLengthPx(t *testing.T, body, prop string) float64 {
	t.Helper()
	v, ok := cssDecl(body, prop)
	if !ok {
		t.Fatalf("no `%s` in `%s`", prop, strings.TrimSpace(body))
	}
	n, err := strconv.ParseFloat(strings.TrimSuffix(v, "px"), 64)
	if !strings.HasSuffix(v, "px") || err != nil {
		t.Fatalf("`%s: %s` is not a plain pixel length", prop, v)
	}
	return n
}

// textShadowDropPx is how far BELOW a glyph the deepest layer of a `text-shadow`
// reaches: the largest (vertical offset + blur radius) over its layers. A blur
// radius is how far a CSS shadow spreads from its offset position, so offset plus
// radius is the bottom of the painted ink — which is the number a clip box has to
// contain.
//
// The colour functions are cut out first, because their commas are the only other
// commas in the value and the layer separator has to be unambiguous. What is left
// of a layer is its lengths, in order: x offset, y offset, blur.
func textShadowDropPx(t *testing.T, body string) float64 {
	t.Helper()
	raw, ok := cssDecl(body, "text-shadow")
	if !ok {
		t.Fatalf("no `text-shadow` in `%s`", strings.TrimSpace(body))
	}
	for {
		i := strings.Index(raw, "(")
		if i < 0 {
			break
		}
		j := strings.Index(raw[i:], ")")
		if j < 0 {
			t.Fatalf("unbalanced parentheses in `text-shadow: %s`", raw)
		}
		raw = raw[:i] + raw[i+j+1:]
	}
	deepest := 0.0
	for _, layer := range strings.Split(raw, ",") {
		var lens []float64
		for _, tok := range strings.Fields(layer) {
			if n, err := strconv.ParseFloat(strings.TrimSuffix(tok, "px"), 64); err == nil {
				lens = append(lens, n)
			}
		}
		if len(lens) < 2 {
			t.Fatalf("layer %q of `text-shadow` carries no offsets", strings.TrimSpace(layer))
		}
		down := lens[1]
		if len(lens) > 2 {
			down += lens[2]
		}
		if down > deepest {
			deepest = down
		}
	}
	return deepest
}
