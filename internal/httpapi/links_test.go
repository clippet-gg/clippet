package httpapi

import (
	"crypto/ed25519"
	"net/url"
	"slices"
	"strings"
	"testing"

	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/urls"
)

// The two link grammars on the wire (ROADMAP #35, §20 errata 248-251): the
// per-upload field, the account default, what the capture routes do when
// neither is sent, and the refusals.

// wordsIn counts the capitalised words of a memorable id.
func wordsIn(id string) int {
	n := 0
	for i := 0; i < len(id); i++ {
		if id[i] >= 'A' && id[i] <= 'Z' {
			n++
		}
	}
	return n
}

// TestUploadLinkStyleField: `link_style` on POST /api/clips picks the grammar,
// and the id it produces is what the DTO reports back.
func TestUploadLinkStyleField(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")

	r := e.upload(`{"title":"words","link_style":"memorable"}`, "clip.mp4", testVideo(2000), withCookie(cookie))
	if r.code != 201 {
		t.Fatalf("upload: %d %s", r.code, r.body)
	}
	id := r.str("id")
	if ids.Kind(id) != ids.KindMemorable || wordsIn(id) != ids.MemorableMinWords {
		t.Fatalf("id %q is not a three-word memorable id", id)
	}
	if r.str("link_style") != db.LinkStyleMemorable {
		t.Errorf("link_style = %q, want %q", r.str("link_style"), db.LinkStyleMemorable)
	}
	// The share URL is the id at the root, whatever grammar it is in.
	us := r.json["urls"].(map[string]any)
	if us["page"] != testOrigin+"/"+id {
		t.Errorf("page URL = %v", us["page"])
	}

	// Unlisted at upload time draws FOUR words (§20 erratum 250): the link is
	// the only authentication on an unlisted clip, so it is worth more bits.
	r = e.upload(`{"link_style":"memorable","unlisted":true}`, "clip.mp4", testVideo(2000), withCookie(cookie))
	if r.code != 201 {
		t.Fatalf("unlisted upload: %d %s", r.code, r.body)
	}
	if id := r.str("id"); wordsIn(id) != ids.MemorableMaxWords {
		t.Fatalf("an unlisted memorable id is %q (%d words), want %d", id, wordsIn(id), ids.MemorableMaxWords)
	}

	// The default is compact, and an absent field changes nothing about it.
	r = e.upload(`{"title":"plain"}`, "clip.mp4", testVideo(2000), withCookie(cookie))
	if r.code != 201 || ids.Kind(r.str("id")) != ids.KindCompact || r.str("link_style") != db.LinkStyleCompact {
		t.Fatalf("the default upload is not compact: %s", r.body)
	}
}

// TestUploadLinkStyleRejections: one code, on both upload routes, before the
// file is read.
func TestUploadLinkStyleRejections(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")

	for _, bad := range []string{"weird", "Memorable", "MEMORABLE", "words"} {
		r := e.upload(`{"link_style":"`+bad+`"}`, "clip.mp4", testVideo(1000), withCookie(cookie))
		if r.code != 400 || r.errCode() != "invalid_link_style" {
			t.Errorf("POST /api/clips link_style=%q: %d %s", bad, r.code, r.body)
		}
		r = e.capture([]part{
			textPart("link_style", bad),
			filePart("file", "clip.mp4", testVideo(1000)),
		}, withCookie(cookie))
		if r.code != 400 || r.errCode() != "invalid_link_style" {
			t.Errorf("POST /api/upload link_style=%q: %d %s", bad, r.code, r.body)
		}
	}
	// An EMPTY capture field means "not sent", like every other one: a tool that
	// always emits its arguments must not be told off for a value it did not
	// choose (§20 erratum 58).
	r := e.capture([]part{
		textPart("link_style", ""),
		filePart("file", "clip.mp4", testVideo(1000)),
	}, withCookie(cookie))
	if r.code != 201 || ids.Kind(r.str("id")) != ids.KindCompact {
		t.Fatalf("an empty link_style is not 'not sent': %d %s", r.code, r.body)
	}
	// JSON is exact and a form field is typed: the capture grammar TRIMS every
	// value it reads (§20 erratum 58), so `" compact"` is a legal FORM field and
	// an illegal JSON one. That asymmetry is the one `ttl_days` has had since the
	// capture route existed; it is pinned here so a later tidy-up cannot quietly
	// make one of the two stricter than the other by accident.
	if r := e.upload(`{"link_style":" compact"}`, "clip.mp4", testVideo(1000), withCookie(cookie)); r.code != 400 ||
		r.errCode() != "invalid_link_style" {
		t.Errorf(`JSON link_style=" compact": %d %s`, r.code, r.body)
	}
	if r := e.capture([]part{
		textPart("link_style", " memorable "),
		filePart("file", "clip.mp4", testVideo(1000)),
	}, withCookie(cookie)); r.code != 201 || ids.Kind(r.str("id")) != ids.KindMemorable {
		t.Errorf("a capture field with spaces round it: %d %s", r.code, r.body)
	}
}

// TestCaptureFollowsTheAccountDefault: the capture endpoint sends no field —
// the shipped clippet.sxcu has no argument for it — so it follows the ACCOUNT,
// not the instance (§20 erratum 250). So does the phone share target.
func TestCaptureFollowsTheAccountDefault(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")

	r := e.doJSON("PATCH", "/api/auth/me/settings",
		map[string]any{"default_link_style": "memorable"}, withCookie(cookie))
	if r.code != 200 || r.str("default_link_style") != db.LinkStyleMemorable {
		t.Fatalf("PATCH settings: %d %s", r.code, r.body)
	}

	captured := e.capture([]part{filePart("file", "clip.mp4", testVideo(1500))}, withCookie(cookie))
	if captured.code != 201 {
		t.Fatalf("capture: %d %s", captured.code, captured.body)
	}
	if id := captured.str("id"); ids.Kind(id) != ids.KindMemorable || wordsIn(id) != ids.MemorableMinWords {
		t.Fatalf("the capture upload did not follow the account default: %q", captured.str("id"))
	}
	// A JSON upload with no field follows it too, and an explicit field still
	// wins over it — a default is never a permission (§20 erratum 155).
	if r := e.upload(`{}`, "clip.mp4", testVideo(1500), withCookie(cookie)); ids.Kind(r.str("id")) != ids.KindMemorable {
		t.Errorf("POST /api/clips did not follow the account default: %s", r.body)
	}
	if r := e.upload(`{"link_style":"compact"}`, "clip.mp4", testVideo(1500), withCookie(cookie)); ids.Kind(r.str("id")) != ids.KindCompact {
		t.Errorf("an explicit compact field lost to the account default: %s", r.body)
	}
}

// TestSettingsLinkStyle: the account default round-trips, an absent key leaves
// it alone, and anything outside the closed set is one 400.
func TestSettingsLinkStyle(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")

	r := e.do("GET", "/api/auth/me/settings", nil, withCookie(cookie))
	if r.code != 200 || r.str("default_link_style") != db.LinkStyleCompact {
		t.Fatalf("a fresh account: %d %s", r.code, r.body)
	}
	r = e.doJSON("PATCH", "/api/auth/me/settings",
		map[string]any{"default_link_style": "memorable"}, withCookie(cookie))
	if r.code != 200 || r.str("default_link_style") != db.LinkStyleMemorable {
		t.Fatalf("PATCH: %d %s", r.code, r.body)
	}
	// An absent key leaves it alone, exactly as it does for the other two.
	r = e.doJSON("PATCH", "/api/auth/me/settings",
		map[string]any{"default_unlisted": true}, withCookie(cookie))
	if r.code != 200 || r.str("default_link_style") != db.LinkStyleMemorable {
		t.Fatalf("an unrelated PATCH moved the link style: %d %s", r.code, r.body)
	}
	for _, bad := range []any{"weird", "", "Compact", "memorable "} {
		r = e.doJSON("PATCH", "/api/auth/me/settings",
			map[string]any{"default_link_style": bad}, withCookie(cookie))
		if r.code != 400 || r.errCode() != "invalid_link_style" {
			t.Errorf("PATCH default_link_style=%v: %d %s", bad, r.code, r.body)
		}
	}
	// …and the refusal changed nothing.
	r = e.do("GET", "/api/auth/me/settings", nil, withCookie(cookie))
	if r.str("default_link_style") != db.LinkStyleMemorable {
		t.Errorf("a refused PATCH moved the preference: %s", r.body)
	}
}

// TestClipDTOReportsTheGrammar: `link_style` is derived from the id on every
// route that renders a ClipDTO, and flipping a clip to unlisted LATER leaves
// its id — and therefore its links — alone (§20 erratum 251).
func TestClipDTOReportsTheGrammar(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")

	r := e.upload(`{"link_style":"memorable"}`, "clip.mp4", testVideo(1200), withCookie(cookie))
	if r.code != 201 {
		t.Fatalf("upload: %d %s", r.code, r.body)
	}
	id := r.str("id")
	if wordsIn(id) != ids.MemorableMinWords {
		t.Fatalf("a listed memorable upload drew %d words", wordsIn(id))
	}
	got := e.do("GET", "/api/clips/"+id, nil)
	if got.code != 200 || got.str("link_style") != db.LinkStyleMemorable {
		t.Fatalf("GET: %d %s", got.code, got.body)
	}
	patched := e.doJSON("PATCH", "/api/clips/"+id, map[string]any{"unlisted": true}, withCookie(cookie))
	if patched.code != 200 {
		t.Fatalf("PATCH: %d %s", patched.code, patched.body)
	}
	if patched.str("id") != id {
		t.Fatalf("the id changed when the clip was hidden: %q → %q", id, patched.str("id"))
	}
	if patched.json["unlisted"] != true || patched.str("link_style") != db.LinkStyleMemorable {
		t.Fatalf("after the PATCH: %s", patched.body)
	}
	if wordsIn(patched.str("id")) != ids.MemorableMinWords {
		t.Error("hiding a clip re-minted its id with a fourth word")
	}
	// A compact clip says so, on the same field, so a client needs no length
	// arithmetic of its own.
	plain := e.upload(`{}`, "clip.mp4", testVideo(1200), withCookie(cookie))
	if plain.str("link_style") != db.LinkStyleCompact {
		t.Errorf("a compact clip reports %q", plain.str("link_style"))
	}
}

// TestOEmbedAcceptsBothShareURLs: the root form is what the page advertises
// now, and `/clips/{id}` is what a consumer stored (or somebody pasted) before
// §20 erratum 248. Both name the same clip; nothing else does.
func TestOEmbedAcceptsBothShareURLs(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser("carol")
	c, _, _ := e.readyClip(u, clipOpts{title: "Both spellings"})

	for _, raw := range []string{
		testOrigin + "/" + c.ID,
		testOrigin + "/clips/" + c.ID,
		testOrigin + "/embed/" + c.ID,
		testOrigin + "/" + c.ID + "?utm_source=x#t=3",
	} {
		r := e.do("GET", "/oembed?format=json&url="+url.QueryEscape(raw), nil)
		if r.code != 200 {
			t.Errorf("oEmbed for %q: %d %s", raw, r.code, r.body)
			continue
		}
		if r.str("type") != "video" || !strings.Contains(r.str("html"), testOrigin+"/embed/"+c.ID) {
			t.Errorf("oEmbed for %q: %s", raw, r.body)
		}
	}
	// And the rejections still reject, in BOTH spellings.
	for _, raw := range []string{
		testOrigin + "/" + c.ID + "/extra",
		testOrigin + "/clips/" + c.ID + "/extra",
		testOrigin + "/not-an-id",
		testOrigin + "/clips/not-an-id",
		testOrigin + "/settings",
		"https://evil.example/" + c.ID,
	} {
		if r := e.do("GET", "/oembed?format=json&url="+url.QueryEscape(raw), nil); r.code != 404 {
			t.Errorf("oEmbed for %q = %d, want 404", raw, r.code)
		}
	}
}

// TestEveryAPISegmentIsReserved is internal/web's twin (§20 erratum 248): the
// API mux's own first segments must be in ids.Reserved too, because serve.go
// mounts /api/, /healthz, /readyz and /oembed ABOVE the page handler — a clip
// id equal to one of them would be unreachable at the root.
func TestEveryAPISegmentIsReserved(t *testing.T) {
	e := newEnv(t)
	cfg := e.cfg
	// CONFIGURED, so the Discord interactions route is registered and walked
	// too: it is the only route in this package registered conditionally, and
	// a conditional route is exactly the one a reserved-word test would miss
	// (ROADMAP #33 arrived in the lane beside #35's; §20 errata 248, 254).
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DiscordBot = testBotConfig(t, pub, "")
	// …and the Discord sign-in's three paths, conditional for the same reason
	// (ROADMAP #44, §20 erratum 302).
	cfg.DiscordLogin = testLoginConfig(t)
	h := newHandler(Deps{Cfg: cfg, DB: e.db, URLs: urls.New(mustParseOrigin(t), "test"), Bot: &recorderBot{}, Login: &fakeLogin{}})
	h.routes()
	if len(h.patterns) == 0 {
		t.Fatal("the API mux recorded no patterns")
	}
	if !slices.Contains(h.patterns, interactionsPath) {
		t.Fatalf("the conditional interactions route was not registered: %v", h.patterns)
	}
	for _, p := range []string{discordLoginStartPath, discordLoginPath, discordLoginCallbackPath} {
		if !slices.Contains(h.patterns, p) {
			t.Fatalf("the conditional login route %s was not registered: %v", p, h.patterns)
		}
	}
	for _, p := range h.patterns {
		seg := strings.TrimPrefix(p, "/")
		if i := strings.IndexByte(seg, '/'); i >= 0 {
			seg = seg[:i]
		}
		if seg == "" || strings.HasPrefix(seg, "{") {
			continue // "/" is the catch-all, not a literal
		}
		if !ids.IsReserved(seg) {
			t.Errorf("API route segment %q is not in ids.Reserved: an id could be minted that shadows it", seg)
		}
	}
}

func mustParseOrigin(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse(testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
