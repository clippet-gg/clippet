package web

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"clippet/internal/db"
)

// The component embed (§20 erratum 316). The format is Discord's unmerged
// Link Previews documentation (discord-api-docs #8606): one Container, a link
// button carrying only the documented keys, at most 3,000 bytes. These tests
// pin what a Clippet page declares inside that format.

// decodeCE parses a rendered payload into generic JSON, failing the test when
// it is not one JSON object.
func decodeCE(t *testing.T, raw string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("payload is not JSON: %v\n%s", err, raw)
	}
	return v
}

func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// ceParts walks the fixed layout and returns its pieces, failing on any shape
// that is not exactly Container{Section{Text; Thumbnail}, MediaGallery{1 item},
// ActionRow{1 link Button}}.
type ceParts struct {
	accent    int
	text      string
	thumbnail string
	media     string
	button    map[string]any
}

func walkCE(t *testing.T, raw string) ceParts {
	t.Helper()
	top := decodeCE(t, raw)
	if got := keysOf(top); !reflect.DeepEqual(got, []string{"component"}) {
		t.Fatalf("top-level keys %v, want [component]", got)
	}
	c := top["component"].(map[string]any)
	if c["type"] != float64(17) {
		t.Fatalf("component type %v, want 17 (Container)", c["type"])
	}
	if got := keysOf(c); !reflect.DeepEqual(got, []string{"accent_color", "components", "type"}) {
		t.Errorf("container keys %v", got)
	}
	kids := c["components"].([]any)
	if len(kids) != 3 {
		t.Fatalf("%d container children, want 3", len(kids))
	}
	var p ceParts
	p.accent = int(c["accent_color"].(float64))

	sec := kids[0].(map[string]any)
	if sec["type"] != float64(9) {
		t.Fatalf("first child type %v, want 9 (Section)", sec["type"])
	}
	texts := sec["components"].([]any)
	if len(texts) != 1 || texts[0].(map[string]any)["type"] != float64(10) {
		t.Fatalf("section children %v, want one Text Display", texts)
	}
	p.text = texts[0].(map[string]any)["content"].(string)
	acc := sec["accessory"].(map[string]any)
	if acc["type"] != float64(11) {
		t.Fatalf("accessory type %v, want 11 (Thumbnail)", acc["type"])
	}
	p.thumbnail = acc["media"].(map[string]any)["url"].(string)

	gal := kids[1].(map[string]any)
	if gal["type"] != float64(12) {
		t.Fatalf("second child type %v, want 12 (Media Gallery)", gal["type"])
	}
	items := gal["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("%d gallery items, want 1", len(items))
	}
	p.media = items[0].(map[string]any)["media"].(map[string]any)["url"].(string)

	row := kids[2].(map[string]any)
	if row["type"] != float64(1) {
		t.Fatalf("third child type %v, want 1 (Action Row)", row["type"])
	}
	btns := row["components"].([]any)
	if len(btns) != 1 {
		t.Fatalf("%d buttons, want 1", len(btns))
	}
	p.button = btns[0].(map[string]any)
	// "may contain id, type, url, style, label, emoji, and disabled … Any other
	// key invalidates the entire payload."
	allowed := map[string]bool{"id": true, "type": true, "url": true, "style": true, "label": true, "emoji": true, "disabled": true}
	for k := range p.button {
		if !allowed[k] {
			t.Errorf("button carries %q, which invalidates the whole payload", k)
		}
	}
	return p
}

func TestComponentEmbedReady(t *testing.T) {
	for _, base := range []string{"http://example.test:8481", "https://clips.example.com"} {
		u := builder(t, base)
		c := readyClip()
		raw := string(BuildComponentEmbed(ClipView{Clip: c, URLs: u}, ""))
		p := walkCE(t, raw)
		if p.accent != 0x0E7C4F {
			t.Errorf("%s: accent %#x, want the ready green 0x0E7C4F", base, p.accent)
		}
		page := u.Page(c.ID)
		want := "-# " + builderHost(base) + "\n### [E2E std](" + page + ")\n0:06 · a \\<b\\>desc\\</b\\>"
		if p.text != want {
			t.Errorf("%s: text\n got %q\nwant %q", base, p.text, want)
		}
		if p.thumbnail != base+"/static/icon-192.png" {
			t.Errorf("%s: thumbnail %q, want the mark's PNG", base, p.thumbnail)
		}
		// The SAME file og:video names: the card plays what the fallback plays.
		if og := metaMap(t, BuildMeta(ClipView{Clip: c, URLs: u}))["property og:video"]; p.media != og {
			t.Errorf("%s: gallery %q, og:video %q", base, p.media, og)
		}
		if p.button["type"] != float64(2) || p.button["style"] != float64(5) || p.button["url"] != page || p.button["label"] != "Open" {
			t.Errorf("%s: button %v", base, p.button)
		}
	}
}

func TestComponentEmbedLinkOut(t *testing.T) {
	u := builder(t, "https://clips.example.com")
	for _, tc := range []struct {
		state  string
		accent int
		line   string
	}{
		{db.ClipQueued, 0xD98E12, "Processing…"},
		{db.ClipProcessing, 0xD98E12, "Processing…"},
		{db.ClipFailed, 0xF0665C, "Processing failed"},
	} {
		c := db.Clip{ID: "aZ3kQ9xB2mLp", Title: "Mid encode", State: tc.state}
		p := walkCE(t, string(BuildComponentEmbed(ClipView{Clip: c, URLs: u}, "")))
		if p.accent != tc.accent {
			t.Errorf("%s: accent %#x, want %#x", tc.state, p.accent, tc.accent)
		}
		if !strings.HasSuffix(p.text, "\n"+tc.line) {
			t.Errorf("%s: text %q does not end with %q", tc.state, p.text, tc.line)
		}
		if !strings.HasPrefix(p.media, u.Abs("/static/card-placeholder.png")) {
			t.Errorf("%s: media %q, want the §9.1 placeholder", tc.state, p.media)
		}
	}
}

// The accent is web.CardColor's value for every state, so the bar, the bot's
// embed and this card can never disagree.
func TestComponentEmbedAccentIsTheCardColour(t *testing.T) {
	for _, s := range []string{db.ClipQueued, db.ClipProcessing, db.ClipReady, db.ClipFailed, "anything"} {
		if got, want := "#"+hex6(cardColorInt(s)), strings.ToUpper(CardColor(s)); got != want {
			t.Errorf("%s: %s, want %s", s, got, want)
		}
	}
}

func hex6(n int) string {
	const digits = "0123456789ABCDEF"
	b := make([]byte, 6)
	for i := 5; i >= 0; i-- {
		b[i] = digits[n&0xF]
		n >>= 4
	}
	return string(b)
}

// A title is the uploader's text: it cannot close the <script> element, cannot
// smuggle markdown (a mention chip, a masked link) and, reading as a link, is
// dropped from the line rather than printed as a second link.
func TestComponentEmbedHostileText(t *testing.T) {
	u := builder(t, "https://clips.example.com")
	c := readyClip()
	c.Title = `</script><script>alert(1)</script> <@123> [x](y)`
	c.Description = "see https://evil.example now"
	raw := string(BuildComponentEmbed(ClipView{Clip: c, URLs: u}, ""))
	if strings.ContainsAny(raw, "<>&") {
		t.Errorf("raw payload carries < > or &: %s", raw)
	}
	p := walkCE(t, raw)
	if !strings.Contains(p.text, `\</script\>`) || !strings.Contains(p.text, `\<@123\>`) || !strings.Contains(p.text, `\[x\]\(y\)`) {
		t.Errorf("title not escaped: %q", p.text)
	}
	if strings.Contains(p.text, "evil") {
		t.Errorf("a description carrying a link reached the card: %q", p.text)
	}
	if !strings.HasSuffix(p.text, "\n0:06") {
		t.Errorf("the line lost its length with the link: %q", p.text)
	}

	c = readyClip()
	c.Title = "www.example.com/watch"
	p = walkCE(t, string(BuildComponentEmbed(ClipView{Clip: c, URLs: u}, "")))
	if strings.Contains(p.text, "example.com/watch") || strings.Contains(p.text, "###") {
		t.Errorf("a link-shaped title was printed: %q", p.text)
	}
}

// The worst case a row can reach stays inside Discord's 3,000 bytes: a
// 200-rune title of the character that escapes worst, the 200-rune card
// description of four-byte runes and a quality clause, on a long https base.
func TestComponentEmbedWorstCaseFits(t *testing.T) {
	u := builder(t, "https://a-rather-long-subdomain.of-a-self-hosters-domain.example.com")
	c := readyClip()
	c.Title = strings.Repeat("<", 200)
	c.Description = strings.Repeat("𝄞", 2000)
	sk := "s"
	c.SocialKey = &sk
	c.SocialWidth, c.SocialHeight, c.SocialFPSNum, c.SocialFPSDen = ip(1280), ip(720), ip(30), ip(1)
	c.Width, c.Height, c.FPSNum, c.FPSDen = ip(3840), ip(2160), ip(60000), ip(1001)
	raw := string(BuildComponentEmbed(ClipView{Clip: c, URLs: u}, ""))
	if raw == "" {
		t.Fatal("the worst case was not emitted at all")
	}
	if len(raw) > ComponentEmbedMaxBytes {
		t.Fatalf("%d bytes, over the format's %d", len(raw), ComponentEmbedMaxBytes)
	}
	walkCE(t, raw)
	t.Logf("worst case: %d bytes", len(raw))
}

// discordText is erratum 268's escape rule, now only the component card's
// (§20 erratum 316 took the title out of the bot's message): every markdown
// active backslash-escaped, whitespace collapsed. Outside a masked link's label
// Discord consumes the escapes, so the reader sees the title's own characters.
func TestDiscordTextEscapes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"arc", "arc"},
		{"clip_2024_final", `clip\_2024\_final`},
		{"*not bold*", `\*not bold\*`},
		{"a ~ b ` c | d", "a \\~ b \\` c \\| d"},
		{`back\slash`, `back\\slash`},
		{"two\nlines\tand   spaces", "two lines and spaces"},
		{"  padded  ", "padded"},
		{" \n\t ", ""},
		{"día · ok", "día · ok"},
		{"<@1234>", `\<@1234\>`},
		{"<#1234>", `\<\#1234\>`},
		{"[Watch](x)", `\[Watch\]\(x\)`},
		{"# heading", `\# heading`},
		{"- item", `\- item`},
	} {
		if got := discordText(tc.in); got != tc.want {
			t.Errorf("discordText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The rendered page: off is the shipped default and carries no element; on
// carries exactly one, in <head>, holding exactly BuildComponentEmbed's bytes,
// beside the WHOLE §9.1 inventory — the fallback — and only on the share page.
func TestComponentEmbedOnThePage(t *testing.T) {
	f := newFixture(t)
	off := get(f.handler(passAuth{}), "/"+f.ready.ID).Body.String()
	mustNotContain(t, off, "discord:component-embed")

	f.cardComponents = true
	h := f.handlerBase(passAuth{}, "https://clips.example.com")
	for _, c := range []db.Clip{f.ready, f.failed, f.processing} {
		body := get(h, "/"+c.ID).Body.String()
		head, _, _ := strings.Cut(body, "</head>")
		if n := strings.Count(head, componentEmbedOpenTag); n != 1 {
			t.Fatalf("%s: %d component-embed elements in <head>, want 1", c.State, n)
		}
		_, rest, _ := strings.Cut(head, componentEmbedOpenTag)
		inner, _, _ := strings.Cut(rest, "</script>")
		want := string(BuildComponentEmbed(ClipView{Clip: f.reload(c.ID), URLs: builder(t, "https://clips.example.com")}, ""))
		if inner != want {
			t.Errorf("%s: element body\n got %s\nwant %s", c.State, inner, want)
		}
		walkCE(t, inner)
		assertNoInline(t, "share page with a component embed ("+c.State+")", body)
	}
	head, _, _ := strings.Cut(get(h, "/"+f.ready.ID).Body.String(), "</head>")
	for _, tag := range unfurlTags("https://clips.example.com", f.ready.ID, true) {
		if !strings.Contains(head, tag) {
			t.Errorf("with the switch on, head lacks %s", tag)
		}
	}
	for _, path := range []string{"/embed/" + f.ready.ID, "/", "/login"} {
		mustNotContain(t, get(h, path).Body.String(), "discord:component-embed")
	}
}

// TestRedrawQueryLeavesTheCardBare (§20 erratum 325): the bot redraws a card by
// editing its link to `{page}?v=2`. That URL serves the same page, and every
// address the page declares — the canonical, og:url, the component embed — is
// the BARE link, so the query never spreads past the one message.
func TestRedrawQueryLeavesTheCardBare(t *testing.T) {
	const base = "https://clips.example.com"
	f := newFixture(t)
	f.cardComponents = true
	h := f.handlerBase(passAuth{}, base)
	// `?v=3` too: a link redrawn twice — the late card and the late title (§20
	// erratum 330) — steps the version.
	for _, v := range []string{"v=2", "v=3"} {
		rec := get(h, "/"+f.ready.ID+"?"+v)
		if rec.Code != 200 {
			t.Fatalf("GET /{id}?%s = %d", v, rec.Code)
		}
		head, _, _ := strings.Cut(rec.Body.String(), "</head>")
		page := base + "/" + f.ready.ID
		mustContain(t, head, `<link rel="canonical" href="`+page+`">`, `<meta property="og:url" content="`+page+`">`)
		_, rest, _ := strings.Cut(head, componentEmbedOpenTag)
		inner, _, _ := strings.Cut(rest, "</script>")
		if want := string(BuildComponentEmbed(ClipView{Clip: f.reload(f.ready.ID), URLs: builder(t, base)}, "")); inner != want {
			t.Errorf("the component embed at ?%s\n got %s\nwant %s", v, inner, want)
		}
		if strings.Contains(head, v) {
			t.Errorf("the head carries the redraw query %s somewhere:\n%s", v, head)
		}
	}
}

// The owner's real-channel finding (erratum 316): Discord does not consume a
// backslash escape inside a masked link's label, so a title that needs one is a
// plain escaped heading and NOT a link; a title that needs none stays the link.
func TestComponentEmbedTitleLine(t *testing.T) {
	page := "https://dev.clippet.gg/KMKE3S0Sqhsp"
	for _, tc := range []struct{ title, want string }{
		{"Arc_Raiders_2026.01.06_-_00.15.37.07.DVR-00.02.14.016-00.02.23.033",
			`### Arc\_Raiders\_2026.01.06\_\-\_00.15.37.07.DVR\-00.02.14.016\-00.02.23.033`},
		{"My clutch play", "### [My clutch play](" + page + ")"},
		{"  two\n words ", "### [two words](" + page + ")"},
		{"a ] b", `### a \] b`},
		{"<@123> hi", `### \<@123\> hi`},
		{"see https://x.example", ""},
		{"", ""},
		// §20 erratum 317: the timestamp titles are LINKS, their own characters
		// in the label — `:` is no markdown active, and `-` and `#` act only at a
		// line's start, which a label never is.
		{"2025-06-24 00:23", "### [2025-06-24 00:23](" + page + ")"},
		{"Arc Raiders 2026-01-06 00:15", "### [Arc Raiders 2026-01-06 00:15](" + page + ")"},
		{"Clip #3 - the ace", "### [Clip #3 - the ace](" + page + ")"},
		// …and any character that does act inside a label still makes it the
		// plain escaped heading, exactly as 316 built it.
		{"2025-06-24 00:23 ]", `### 2025\-06\-24 00:23 \]`},
		{"*2025-06-24 00:23*", `### \*2025\-06\-24 00:23\*`},
		{"@everyone 2025-06-24 00:23", `### @everyone 2025\-06\-24 00:23`},
	} {
		if got := titleLine(tc.title, page); got != tc.want {
			t.Errorf("%q:\n got %q\nwant %q", tc.title, got, tc.want)
		}
		// A label never carries a backslash: the one thing Discord prints literally.
		if i := strings.Index(titleLine(tc.title, page), "["); i >= 0 && strings.Contains(titleLine(tc.title, page)[i:], `\`) {
			t.Errorf("%q: a backslash inside a link label", tc.title)
		}
	}
}

// One whole card for a capture tool's file (§20 erratum 317): the date-and-time
// title is the heading's link, its characters exactly as og:title carries them.
func TestComponentEmbedTimestampTitleIsALink(t *testing.T) {
	u := builder(t, "https://clips.example.com")
	c := readyClip()
	c.Title = "Arc Raiders 2026-01-06 00:15"
	p := walkCE(t, string(BuildComponentEmbed(ClipView{Clip: c, URLs: u}, "")))
	if want := "\n### [Arc Raiders 2026-01-06 00:15](" + u.Page(c.ID) + ")\n"; !strings.Contains(p.text, want) {
		t.Errorf("text %q lacks %q", p.text, want)
	}
	if og := metaMap(t, BuildMeta(ClipView{Clip: c, URLs: u}))["property og:title"]; og != c.Title {
		t.Errorf("og:title = %q, want %q", og, c.Title)
	}
}

// The owner's second finding: the quality clause's own words "at this link" ARE the
// link, on the component card only; the og:description is unchanged.
func TestComponentEmbedAtThisLinkIsALink(t *testing.T) {
	u := builder(t, "https://dev.clippet.gg")
	c := readyClip()
	c.Description = ""
	sk := "s"
	c.SocialKey = &sk
	c.SocialWidth, c.SocialHeight, c.SocialFPSNum, c.SocialFPSDen = ip(1280), ip(720), ip(30), ip(1)
	c.Width, c.Height, c.FPSNum, c.FPSDen = ip(2560), ip(1440), ip(60), ip(1)
	c.OriginalPlayable = true
	p := walkCE(t, string(BuildComponentEmbed(ClipView{Clip: c, URLs: u}, "")))
	want := "0:06 · Preview 720p30 · Original 1440p60 [at this link](" + u.Page(c.ID) + ")"
	if !strings.HasSuffix(p.text, "\n"+want) {
		t.Errorf("text %q does not end with %q", p.text, want)
	}
	if og := metaMap(t, BuildMeta(ClipView{Clip: c, URLs: u}))["property og:description"]; og != "0:06 · Preview 720p30 · Original 1440p60 at this link" {
		t.Errorf("og:description moved: %q", og)
	}
}

// The card's mark is the INSTANCE's (the owner's pick (b), erratum 316): the
// grey-span icon on a badged instance, the green one otherwise — both served,
// both drawn by mkicons from one geometry.
func TestComponentEmbedMarkFollowsTheBadge(t *testing.T) {
	u := builder(t, "https://dev.clippet.gg")
	for badge, want := range map[string]string{"": "/static/icon-192.png", "dev": "/static/icon-dev-192.png"} {
		p := walkCE(t, string(BuildComponentEmbed(ClipView{Clip: readyClip(), URLs: u}, badge)))
		if p.thumbnail != "https://dev.clippet.gg"+want {
			t.Errorf("badge %q: thumbnail %q, want %s", badge, p.thumbnail, want)
		}
	}
	f := newFixture(t)
	f.cardComponents, f.instanceBadge = true, "dev"
	body := get(f.handler(passAuth{}), "/"+f.ready.ID).Body.String()
	mustContain(t, body, `/static/icon-dev-192.png"`)
	if rec := get(f.handler(passAuth{}), "/static/icon-dev-192.png"); rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" {
		t.Errorf("GET /static/icon-dev-192.png = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}
