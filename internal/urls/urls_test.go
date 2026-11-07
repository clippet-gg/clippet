package urls

import (
	"net/url"
	"strings"
	"testing"
)

func mustParse(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return u
}

func TestBuilderHTTP(t *testing.T) {
	b := New(mustParse(t, "http://localhost:8481"), "abc123")
	const id = "aZ3kQ9xB2mLp"
	cases := map[string]string{
		b.Page(id):         "http://localhost:8481/" + id,
		b.LegacyPage(id):   "http://localhost:8481/clips/" + id,
		b.Embed(id):        "http://localhost:8481/embed/" + id,
		b.Video(id):        "http://localhost:8481/api/clips/" + id + "/video.mp4",
		b.Social(id):       "http://localhost:8481/api/clips/" + id + "/social.mp4",
		b.Poster(id):       "http://localhost:8481/api/clips/" + id + "/poster.jpg",
		b.GIF(id):          "http://localhost:8481/api/clips/" + id + "/preview.gif",
		b.Progress(id):     "http://localhost:8481/api/clips/" + id + "/progress",
		b.Static("app.js"): "http://localhost:8481/static/app.js?v=abc123",
		b.Abs("/x"):        "http://localhost:8481/x",
		b.Abs("x"):         "http://localhost:8481/x",
		b.Origin():         "http://localhost:8481",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if b.IsHTTPS() {
		t.Error("IsHTTPS() = true for http base")
	}
}

func TestBuilderHTTPS(t *testing.T) {
	b := New(mustParse(t, "https://clips.example.com"), "v1")
	if !b.IsHTTPS() {
		t.Error("IsHTTPS() = false for https base")
	}
	if got, want := b.Origin(), "https://clips.example.com"; got != want {
		t.Errorf("Origin() = %q, want %q", got, want)
	}
	// The id is at the ROOT since §20 erratum 248; the address it replaced is
	// still built — and still answered, with a 308 — as LegacyPage.
	if got, want := b.Page("aZ3kQ9xB2mLp"), "https://clips.example.com/aZ3kQ9xB2mLp"; got != want {
		t.Errorf("Page() = %q, want %q", got, want)
	}
	if got, want := b.LegacyPage("aZ3kQ9xB2mLp"), "https://clips.example.com/clips/aZ3kQ9xB2mLp"; got != want {
		t.Errorf("LegacyPage() = %q, want %q", got, want)
	}
}

// TestPageShort (§20 erratum 264): the same link a person reads aloud — host
// and id, no scheme — on both schemes and on a base with a port.
func TestPageShort(t *testing.T) {
	cases := map[string]string{
		"https://clips.example.com": "clips.example.com/aZ3kQ9xB2mLp",
		"http://localhost:8481":     "localhost:8481/aZ3kQ9xB2mLp",
		"http://h:1/":               "h:1/aZ3kQ9xB2mLp",
	}
	for base, want := range cases {
		if got := New(mustParse(t, base), "v1").PageShort("aZ3kQ9xB2mLp"); got != want {
			t.Errorf("base %q: PageShort() = %q, want %q", base, got, want)
		}
	}
}

func TestTrailingSlashTrimmed(t *testing.T) {
	for _, base := range []string{"http://h:1/", "http://h:1///", "http://h:1"} {
		b := New(mustParse(t, base), "")
		if got, want := b.Abs("/a"), "http://h:1/a"; got != want {
			t.Errorf("base %q: Abs(/a) = %q, want %q", base, got, want)
		}
		if got, want := b.Origin(), "http://h:1"; got != want {
			t.Errorf("base %q: Origin() = %q, want %q", base, got, want)
		}
	}
}

func TestStaticWithoutBuildID(t *testing.T) {
	b := New(mustParse(t, "http://h"), "")
	if got, want := b.Static("app.css"), "http://h/static/app.css"; got != want {
		t.Errorf("Static() = %q, want %q", got, want)
	}
	b = New(mustParse(t, "http://h"), "a b&c")
	if got, want := b.Static("/app.css"), "http://h/static/app.css?v=a+b%26c"; got != want {
		t.Errorf("Static() = %q, want %q", got, want)
	}
}

func TestQueryAndUserinfoDropped(t *testing.T) {
	b := New(mustParse(t, "http://user:pw@h:1/?x=1#frag"), "")
	if got, want := b.Abs("/a"), "http://h:1/a"; got != want {
		t.Errorf("Abs(/a) = %q, want %q", got, want)
	}
}

func TestNilBase(t *testing.T) {
	// Defensive: a zero Builder must not panic.
	b := New(nil, "")
	_ = b.Abs("/a")
	_ = b.Origin()
}

// Host is the instance inside a SENTENCE — the Discord command's description
// (§20 erratum 268) — and PageShort is that plus an id, so the two can never
// name different hosts.
func TestHostAndPageShortAgree(t *testing.T) {
	for _, base := range []string{"https://clippet.gg", "http://localhost:8500", "https://ex.test/app"} {
		b := New(mustParse(t, base), "")
		if got, want := b.PageShort("aZ3kQ9xB2mLp"), b.Host()+"/aZ3kQ9xB2mLp"; got != want {
			t.Errorf("base %q: PageShort = %q, want %q", base, got, want)
		}
		if got := b.Host(); got == "" || got[0] == '/' {
			t.Errorf("base %q: Host() = %q, want a host", base, got)
		}
	}
}

// SiteName is the line Discord draws above a card's title and the oEmbed
// document's provider_name (§20 erratum 287): the HOST, and only the host. Host
// (above) is a different answer to a different question — it keeps the port and
// any base path, because it goes inside a sentence a person will type back.
func TestSiteNameIsTheBareHost(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{"https://clippet.gg", "clippet.gg"},
		{"https://dev.clippet.gg", "dev.clippet.gg"},
		{"http://localhost:8518", "localhost"},
		{"http://127.0.0.1:8080", "127.0.0.1"},
		{"https://ex.test/app", "ex.test"},
		{"https://EX.test", "EX.test"}, // the host is passed through as configured, never re-cased
		{"http://[::1]:8080", "::1"},   // an IPv6 literal without its brackets
	} {
		b := New(mustParse(t, tc.base), "")
		if got := b.SiteName(); got != tc.want {
			t.Errorf("base %q: SiteName() = %q, want %q", tc.base, got, tc.want)
		}
		// …and it is never the port, never the scheme and never the path.
		if got := b.SiteName(); strings.ContainsAny(got, ":/") && !strings.Contains(tc.base, "[") {
			t.Errorf("base %q: SiteName() = %q carries a port, a scheme or a path", tc.base, got)
		}
	}
	// The zero Builder has no host at all; its callers fall back to the
	// product's own spelling rather than emitting a blank provider line.
	if got := (Builder{}).SiteName(); got != "" {
		t.Errorf("the zero Builder's SiteName() = %q, want empty", got)
	}
}

// ParseShare is the ONLY thing in the binary that turns a URL somebody typed
// into a clip, and it is a pure function on purpose: a link naming any other
// host is a `false` here, before any code that could act on it runs
// (§20 errata 268, 315).
func TestParseShareAcceptsOurOwnSpellings(t *testing.T) {
	b := New(mustParse(t, "https://clippet.gg"), "")
	const id = "aZ3kQ9xB2mLp"
	for _, tc := range []struct{ raw, want string }{
		{b.Page(id), id},                 // the address since erratum 248
		{b.LegacyPage(id), id},           // what every link sent before it names
		{b.PageShort(id), id},            // the scheme-less form erratum 264 shows
		{"<" + b.Page(id) + ">", id},     // Discord's own no-unfurl wrapper
		{"<<" + b.Page(id) + ">>", id},   // …doubled, which is what a paste often carries (erratum 275)
		{"||" + b.Page(id) + "||", id},   // …and the spoiler wrap
		{"<||" + b.Page(id) + "||>", id}, // …and one inside the other
		{b.Embed(id), id},                // the player URL of the SAME clip (erratum 275)
		{"<" + b.Embed(id) + ">", id},    //
		{"  " + b.Page(id) + "  ", id},   // pasted with the whitespace around it
		{b.Page(id) + "/", id},           // a trailing slash is not a fourth grammar
		{b.Page(id) + "?t=12#x", id},     // a query and a fragment name the same clip
		{b.Page(id) + "?v=2", id},        // the upload form's redrawn link (erratum 325)
		{b.PageShort(id) + "?v=2", id},   //
		{b.Page(id) + "?v=3", id},        // its second redraw (erratum 330)
		{"http://clippet.gg/" + id, id},  // either scheme reaches the same page
		{"https://CLIPPET.GG/" + id, id}, // a host is case-insensitive; an id is not
		// The memorable grammar is just an id: the link shapes are the same two.
		{b.Page("PurpleUpdateCharacter"), "PurpleUpdateCharacter"},
		{b.LegacyPage("PurpleUpdateCharacter"), "PurpleUpdateCharacter"},
	} {
		got, ok := b.ParseShare(tc.raw)
		if !ok || got != tc.want {
			t.Errorf("ParseShare(%q) = %q, %v — want %q, true", tc.raw, got, ok, tc.want)
		}
	}
}

func TestParseShareRefusesEverythingElse(t *testing.T) {
	b := New(mustParse(t, "https://clippet.gg"), "")
	const id = "aZ3kQ9xB2mLp"
	for _, raw := range []string{
		"",
		"   ",
		"https://clippet.gg.evil.test/" + id,    // a lookalike is not the host
		"https://evil.test/clippet.gg/" + id,    // nor is the host in a path
		"https://clippet.gg@evil.example/" + id, // …nor in the USERINFO: the host is what follows the @
		"https://clippet.gg:8443/" + id,         // a different port is a different origin
		"https://user@clippet.gg/" + id,         // userinfo is never in one of ours
		"javascript:alert(1)",                   // not a link to anything
		"file:///etc/passwd",
		"https://clippet.gg/c/" + id, // a collection is not a clip
		"https://clippet.gg/embed",   // the embed PREFIX names no clip
		"https://clippet.gg/embed/",  // …with or without its trailing slash
		"//clippet.gg/" + id,         // protocol-relative: no scheme, so not a link of ours (erratum 275)
		"||||",                       // an empty spoiler is not a link
		"https://clippet.gg/",        // the front door names no clip
		"https://clippet.gg/clips",   // the legacy PREFIX names no clip either
		"https://clippet.gg/clips/",  // …with or without its trailing slash
		"https://clippet.gg/a/b/" + id,
		"https://clippet.gg/clips/" + id + "/video.mp4",
	} {
		if got, ok := b.ParseShare(raw); ok {
			t.Errorf("ParseShare(%q) = %q, true — it is not a share link of ours", raw, got)
		}
	}
}

// A base PATH is part of the address, so a link that omits it is not ours.
func TestParseShareHonoursTheBasePath(t *testing.T) {
	b := New(mustParse(t, "https://ex.test/app"), "")
	const id = "aZ3kQ9xB2mLp"
	if got, ok := b.ParseShare("https://ex.test/app/" + id); !ok || got != id {
		t.Errorf("ParseShare of our own Page = %q, %v", got, ok)
	}
	if got, ok := b.ParseShare("https://ex.test/app/clips/" + id); !ok || got != id {
		t.Errorf("ParseShare of our own LegacyPage = %q, %v", got, ok)
	}
	for _, raw := range []string{"https://ex.test/" + id, "https://ex.test/application/" + id} {
		if got, ok := b.ParseShare(raw); ok {
			t.Errorf("ParseShare(%q) = %q, true — the base path is part of the address", raw, got)
		}
	}
}
