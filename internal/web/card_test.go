package web

import (
	"net/http"
	"strings"
	"testing"
)

// The card, the owner's six calls (§20 erratum 287): the site line, the description
// budget, the bar Discord paints from the LAST theme-color tag, and the embed
// overlay's host lockup. The poster's burned-in duration lives in
// internal/transcode; the placeholder PNG is drawn by scripts/lib/mkcard.py.

// ---- the bar ------------------------------------------------------------

// TestCardColorFollowsTheState: green when the clip can be pasted, amber while
// something is happening, red when it is lost — web/STYLE.md's colour law, on
// the one surface it had never reached.
func TestCardColorFollowsTheState(t *testing.T) {
	for _, tc := range []struct{ state, want string }{
		{"ready", cardColorReady},
		{"queued", cardColorWorking},
		{"processing", cardColorWorking},
		{"failed", cardColorFailed},
		{"deleting", cardColorWorking},
		{"", cardColorWorking},
	} {
		if got := CardColor(tc.state); got != tc.want {
			t.Errorf("CardColor(%q) = %q, want %q", tc.state, got, tc.want)
		}
	}
}

// TestCardBarIsTheLastThemeColorTag is the whole mechanism, pinned.
//
// MEASURED 2026-09-15 against a real Discord channel, eight variants: Discord
// takes the LAST `theme-color` on the page and ignores `media` entirely, while a
// browser takes the FIRST candidate whose media matches (HTML, "obtain a page's
// theme color"). So the card tag must be last AND unqualified, and the browser's
// pair must still come first — if either ever stops being true, the phone's
// chrome goes green and Discord goes back to painting the dark ground.
func TestCardBarIsTheLastThemeColorTag(t *testing.T) {
	f := newFixture(t)
	h := f.handler(passAuth{})
	for _, tc := range []struct{ id, want string }{
		{f.ready.ID, cardColorReady},
		{f.processing.ID, cardColorWorking},
		{f.failed.ID, cardColorFailed},
	} {
		head := headOf(t, get(h, "/"+tc.id).Body.String())
		tags := themeColorTags(head)
		if len(tags) != 3 {
			t.Fatalf("%s: %d theme-color tags, want the browser's pair plus the card's one: %q", tc.id, len(tags), tags)
		}
		// The pair first, media-qualified, unchanged.
		if !strings.Contains(tags[0], `media="(prefers-color-scheme: light)"`) ||
			!strings.Contains(tags[1], `media="(prefers-color-scheme: dark)"`) {
			t.Errorf("%s: the browser's pair is not first and qualified: %q", tc.id, tags)
		}
		// …then the card's, LAST and with no media attribute at all.
		last := tags[len(tags)-1]
		if strings.Contains(last, "media=") {
			t.Errorf("%s: the card's tag carries a media attribute (%q) — Discord ignores it and a browser would then take it", tc.id, last)
		}
		if want := `<meta name="theme-color" content="` + tc.want + `">`; last != want {
			t.Errorf("%s: the last theme-color tag is %q, want %q", tc.id, last, want)
		}
	}
}

// TestCardBarIsOnlyOnPagesThatCarryACard: the front door, the library, Settings,
// the auth pages, the collection page and the crawler hold emit the browser's
// pair ALONE. A collection has no state to colour (it is a list of several
// clips), and a page with no card has nothing for Discord to paint beside.
func TestCardBarIsOnlyOnPagesThatCarryACard(t *testing.T) {
	f := newFixture(t)
	anon, owner := f.handler(passAuth{}), f.handler(session(f.owner))
	col := f.mkCollection(f.owner, "Best of 2026", f.ready)
	for _, path := range []string{"/login", "/register"} {
		assertPairAlone(t, path, headOf(t, get(anon, path).Body.String()))
	}
	for _, path := range []string{"/", "/settings"} {
		assertPairAlone(t, path, headOf(t, get(owner, path).Body.String()))
	}
	assertPairAlone(t, "/c/"+col.ID, headOf(t, get(anon, "/c/"+col.ID).Body.String()))
	assertPairAlone(t, "a 404", headOf(t, get(anon, "/ZZZZZZZZZZZZ").Body.String()))
	// …and the crawler hold (§20 erratum 213), which is the one page a card
	// consumer reaches that is NOT a card: an unfurler asking for a clip with no
	// card yet gets `unavailable` under a 503, and a bar colour on a page whose
	// whole point is that there is nothing to unfurl yet would be a colour for
	// no card at all.
	hold := do(anon, http.MethodGet, "/"+f.queued.ID, "User-Agent", "Discordbot/2.0")
	if hold.Code != http.StatusServiceUnavailable {
		t.Fatalf("the crawler hold answered %d, want 503 — this case is no longer the hold", hold.Code)
	}
	assertPairAlone(t, "the crawler hold", headOf(t, hold.Body.String()))
}

// assertPairAlone: exactly the two media-qualified tags, and nothing after them.
func assertPairAlone(t *testing.T, what, head string) {
	t.Helper()
	tags := themeColorTags(head)
	if len(tags) != 2 {
		t.Errorf("%s: %d theme-color tags, want the browser's pair alone: %q", what, len(tags), tags)
		return
	}
	for _, tag := range tags {
		if !strings.Contains(tag, "media=") {
			t.Errorf("%s: an unqualified theme-color tag on a page that carries no card: %q", what, tag)
		}
	}
}

// themeColorTags returns every <meta name="theme-color"…> in the head, in
// DOCUMENT ORDER — which is the only thing this feature is about.
func themeColorTags(head string) []string {
	var out []string
	for _, line := range strings.Split(head, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if strings.HasPrefix(line, `<meta name="theme-color"`) {
			out = append(out, line)
		}
	}
	return out
}

func headOf(t *testing.T, body string) string {
	t.Helper()
	head, _, ok := strings.Cut(body, "</head>")
	if !ok {
		t.Fatal("no </head> in the rendered page")
	}
	return head
}

// ---- the description budget ---------------------------------------------

// TestCardDescriptionClampsTheOwnerAndKeepsTheClause is the amendment to erratum
// 214: the quality clause is the only thing standing between a Light card and a
// reader concluding the product is a 720p product, and on a wordy clip it used
// to be the first thing Discord cut off.
func TestCardDescriptionClampsTheOwnerAndKeepsTheClause(t *testing.T) {
	c := readyClip()
	c.Width, c.Height, c.FPSNum, c.FPSDen = ip(2560), ip(1440), ip(60), ip(1)
	c.OriginalPlayable = true
	c.SocialKey, c.SocialSize, c.SocialETag = sp("clips/x/renditions/social-abcd1234.mp4"), i64p(99), sp("se")
	c.SocialWidth, c.SocialHeight, c.SocialFPSNum, c.SocialFPSDen = ip(1280), ip(720), ip(30), ip(1)
	c.Description = strings.TrimSpace(strings.Repeat("wordy ", 400)) // ~2400 runes, the DTO's own ceiling is 2000

	got := cardDescription(c)
	clause := "Preview 720p30 · Original 1440p60 at this link"
	if !strings.HasPrefix(got, "0:06 · ") {
		t.Errorf("the duration does not lead: %q", got)
	}
	if !strings.HasSuffix(got, clause) {
		t.Errorf("the quality clause did not survive the clamp: %q", got)
	}
	if n := len([]rune(got)); n > cardDescriptionBudget {
		t.Errorf("%d runes, want at most the %d-rune budget: %q", n, cardDescriptionBudget, got)
	}
	if strings.Count(got, cardDescriptionCut) != 1 {
		t.Errorf("a clamped description carries exactly one ellipsis: %q", got)
	}
	if strings.Contains(got, cardDescriptionCut+cardDescriptionSep+clause) == false {
		t.Errorf("the ellipsis does not sit at the end of the owner's half: %q", got)
	}
}

// TestCardDescriptionClampIsRuneSafeAndDoesNotCutMidWord.
func TestCardDescriptionClampIsRuneSafe(t *testing.T) {
	// Multi-byte runes: a byte-wise cut would leave a replacement character on
	// every card in a language that does not use the latin alphabet.
	c := readyClip()
	c.Description = strings.Repeat("あ", 500)
	got := cardDescription(c)
	if n := len([]rune(got)); n > cardDescriptionBudget {
		t.Errorf("%d runes, want at most %d", n, cardDescriptionBudget)
	}
	if strings.ContainsRune(got, '�') {
		t.Errorf("a replacement character survived the clamp: %q", got)
	}
	// …and a spaced sentence ends on a word, not mid-syllable.
	c.Description = strings.TrimSpace(strings.Repeat("alpha bravo charlie delta ", 40))
	got = cardDescription(c)
	body := strings.TrimSuffix(strings.TrimPrefix(got, "0:06 · "), cardDescriptionCut)
	if last := body[strings.LastIndex(body, " ")+1:]; last != "alpha" && last != "bravo" && last != "charlie" && last != "delta" {
		t.Errorf("the clamp cut mid-word (%q): %q", last, got)
	}
}

// TestCardDescriptionFloorIsABelt: the fixed parts cannot eat the budget, so the
// floor that would drop the owner's words entirely never fires on a real clip.
// If a longer clause or an hour-long duration ever changed that, this fails.
func TestCardDescriptionFloorIsABelt(t *testing.T) {
	// The widest fixed pair this product can produce: an hour-plus duration and
	// a two-fractional-rate clause.
	head := "10:00:00"
	clause := "Preview 1080p29.97 · Original 2160p59.94 at this link"
	if got := ownerBudget(head, clause); got < cardDescriptionFloor {
		t.Errorf("the worst-case owner allowance is %d runes, under the %d-rune floor: the floor is no longer a belt",
			got, cardDescriptionFloor)
	}
	// The floor itself still works when it is reached.
	if got := clampRunes("some words", cardDescriptionFloor-1); got != "" {
		t.Errorf("clampRunes under the floor = %q, want the words dropped", got)
	}
}

// TestCardDescriptionAtExactlyTheBudgetIsNotCut is the boundary either side of
// the clamp: a description that fits EXACTLY is printed whole, with no ellipsis
// spent on a cut that did not happen, and one rune more is cut. It is the case a
// reader hits when they trim their own words to fit, and the one an
// off-by-one in clampRunes would silently spoil.
func TestCardDescriptionAtExactlyTheBudgetIsNotCut(t *testing.T) {
	c := readyClip()
	head := "0:06"
	n := ownerBudget(head, "") // no Light copy on readyClip(), so no clause
	c.Description = strings.Repeat("x", n)

	got := cardDescription(c)
	if want := head + cardDescriptionSep + c.Description; got != want {
		t.Errorf("a description of exactly the budget was changed:\n got %q\nwant %q", got, want)
	}
	if n := len([]rune(got)); n != cardDescriptionBudget {
		t.Errorf("the whole description is %d runes, want exactly the %d-rune budget", n, cardDescriptionBudget)
	}
	if strings.Contains(got, cardDescriptionCut) {
		t.Errorf("an ellipsis on a description that fits: %q", got)
	}
	// One rune more, and it is cut — to the budget, not past it.
	c.Description += "x"
	got = cardDescription(c)
	if !strings.HasSuffix(got, cardDescriptionCut) {
		t.Errorf("one rune over the budget was not cut: %q", got)
	}
	if n := len([]rune(got)); n > cardDescriptionBudget {
		t.Errorf("the cut description is %d runes, over the %d-rune budget", n, cardDescriptionBudget)
	}
}

// ---- the embed overlay's host lockup ------------------------------------

// TestMarkOfKeepsTheWordmarkOnOurOwnDomain (§20 erratum 287): clippet.gg and any
// subdomain of it render the lockup, because the host CONTAINS the wordmark;
// anything else renders as plain text, because a self-hoster's domain is not
// this product's logo.
func TestMarkOfKeepsTheWordmarkOnOurOwnDomain(t *testing.T) {
	for _, tc := range []struct {
		host, prefix, suffix string
		lockup               bool
	}{
		{"clippet.gg", "", ".gg", true},
		{"dev.clippet.gg", "dev.", ".gg", true},
		{"lab.test.clippet.gg", "lab.test.", ".gg", true},
		{"clips.example.com", "", "", false},
		{"localhost", "", "", false},
		{"clippet.tv", "", "", false},
		// The suffix test is on ".clippet.gg" and not on the bare domain, so a
		// host that merely ENDS with those bytes is somebody else's.
		{"notclippet.gg", "", "", false},
		{"evilclippet.gg", "", "", false},
	} {
		m := markOf(tc.host)
		if m.Lockup != tc.lockup || m.Prefix != tc.prefix || m.Suffix != tc.suffix {
			t.Errorf("markOf(%q) = %+v, want lockup=%v prefix=%q suffix=%q", tc.host, m, tc.lockup, tc.prefix, tc.suffix)
		}
		if m.Host != tc.host {
			t.Errorf("markOf(%q) lost the host: %q", tc.host, m.Host)
		}
		if m.Lockup && m.Prefix+markWord+m.Suffix != tc.host {
			t.Errorf("markOf(%q): the lockup does not spell the host back (%q + %q + %q)", tc.host, m.Prefix, markWord, m.Suffix)
		}
	}
	// A builder with no host at all is the bare wordmark: what the overlay was
	// before this erratum, never an empty link.
	if m := markOf(""); !m.Lockup || m.Host != markWord || m.Prefix != "" || m.Suffix != "" {
		t.Errorf("markOf(\"\") = %+v, want the bare wordmark", m)
	}
}

// TestEmbedOverlayRendersTheHost: both shapes, through the real template.
func TestEmbedOverlayRendersTheHost(t *testing.T) {
	f := newFixture(t)
	// The fixture's own base is example.test → the plain form, with no span (so
	// no living mark and no badge grey) and no aria-label (the text IS the name).
	body := get(f.handler(passAuth{}), "/embed/"+f.ready.ID).Body.String()
	mustContain(t, body, embedMark(f.ready.ID))
	mustNotContain(t, body, `aria-label="clippet"`, `<span class="pp">`)

	// …and on the product's own domain the lockup is intact.
	for _, tc := range []struct{ base, prefix string }{
		{"https://clippet.gg", ""},
		{"https://dev.clippet.gg", "dev."},
	} {
		body := get(f.handlerBase(passAuth{}, tc.base), "/embed/"+f.ready.ID).Body.String()
		mustContain(t, body, embedLockup(f.ready.ID, tc.prefix))
		if n := strings.Count(body, "<a "); n != 1 {
			t.Errorf("%s: the embed carries %d links, want exactly the overlay", tc.base, n)
		}
	}
}
