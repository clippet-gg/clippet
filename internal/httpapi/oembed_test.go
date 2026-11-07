package httpapi

import (
	"net/url"
	"strings"
	"testing"

	"clippet/internal/urls"
)

// oembedURL builds the request an oEmbed consumer makes for a page URL.
func oembedURL(page string, extra ...string) string {
	q := url.Values{"url": {page}, "format": {"json"}}
	for i := 0; i+1 < len(extra); i += 2 {
		q.Set(extra[i], extra[i+1])
	}
	return "/oembed?" + q.Encode()
}

// TestOEmbedReadyClipIsAPlayer: type video, the same iframe "Copy embed"
// hands out, the poster's MEASURED dimensions, and no owner-private field.
func TestOEmbedReadyClipIsAPlayer(t *testing.T) {
	e := newEnv(t)
	u, cookie := e.newUser("oe")
	c, _, _ := e.readyClip(u, clipOpts{title: "My clip"})

	r := e.do("GET", oembedURL(testOrigin+"/"+c.ID), nil)
	if r.code != 200 {
		t.Fatalf("GET /oembed = %d %s", r.code, r.body)
	}
	if r.str("version") != "1.0" || r.str("type") != "video" {
		t.Fatalf("document = %s", r.body)
	}
	if r.str("provider_name") != testHost || r.str("provider_url") != testOrigin {
		t.Errorf("provider = %s", r.body)
	}
	if r.str("title") != "My clip" {
		t.Errorf("title = %q", r.str("title"))
	}
	html := r.str("html")
	if !strings.Contains(html, testOrigin+"/embed/"+c.ID) || !strings.Contains(html, "allowfullscreen") {
		t.Errorf("html = %q", html)
	}
	if !strings.Contains(r.str("thumbnail_url"), "/poster.jpg") {
		t.Errorf("thumbnail_url = %q", r.str("thumbnail_url"))
	}
	// The document carries NOTHING owner-private: no views, no owner, no
	// source, no trim. It is read by whoever holds the link.
	for _, key := range []string{"views", "owner", "source", "trim", "source_duration_ms"} {
		if r.has(key) {
			t.Errorf("the oEmbed document carries %q: %s", key, r.body)
		}
	}
	// The owner's own session gets the same bytes — there is no owner variant.
	owned := e.do("GET", oembedURL(testOrigin+"/"+c.ID), nil, withCookie(cookie))
	if string(owned.body) != string(r.body) {
		t.Errorf("the owner got a different document:\n%s\n%s", owned.body, r.body)
	}
}

// TestOEmbedUnlistedAnswersLikeTheSharePage: the link is the auth (spec §2),
// and this route is no more selective than the page it describes.
func TestOEmbedUnlistedAnswersLikeTheSharePage(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser("oe")
	c, _, _ := e.readyClip(u, clipOpts{title: "quiet", unlisted: true})
	r := e.do("GET", oembedURL(testOrigin+"/"+c.ID), nil)
	if r.code != 200 || r.str("type") != "video" {
		t.Fatalf("unlisted oEmbed = %d %s", r.code, r.body)
	}
}

// TestOEmbedNotReadyIsALink: no html, and the placeholder card image.
func TestOEmbedNotReadyIsALink(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser("oe")
	c := e.seedClip(u, clipOpts{title: "encoding"})
	r := e.do("GET", oembedURL(testOrigin+"/"+c.ID), nil)
	if r.code != 200 || r.str("type") != "link" {
		t.Fatalf("queued oEmbed = %d %s", r.code, r.body)
	}
	if r.has("html") {
		t.Errorf("a clip that is not ready carried a player: %s", r.body)
	}
	if !strings.Contains(r.str("thumbnail_url"), "card-placeholder.png") {
		t.Errorf("thumbnail_url = %q", r.str("thumbnail_url"))
	}
}

// TestOEmbedHostCheck is the security property: only a URL on this instance's
// own origin is described, and every near miss is a plain 404 that says
// nothing about why.
func TestOEmbedHostCheck(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser("oe")
	c, _, _ := e.readyClip(u, clipOpts{title: "t"})
	id := c.ID

	ok := []string{
		testOrigin + "/" + id,
		testOrigin + "/embed/" + id,      // the embed page names the same clip
		"http://LOCALHOST:8481/" + id,    // hosts are case-insensitive
		testOrigin + "/" + id + "?utm=x", // a tracking parameter names the same clip
		testOrigin + "/" + id + "#t=3",   // …and so does a fragment
	}
	for _, in := range ok {
		if r := e.do("GET", oembedURL(in), nil); r.code != 200 {
			t.Errorf("oEmbed(%q) = %d %s", in, r.code, r.body)
		}
	}
	bad := []string{
		"https://evil.example/" + id,
		"http://evil.example/" + id,
		"http://localhost:8481@evil.example/" + id, // userinfo that reads as ours
		"http://user:pass@localhost:8481/" + id,    // userinfo on the real host
		"http://localhost.:8481/" + id,             // a trailing dot is a different string
		"http://localhost:9999/" + id,              // a different port is a different origin
		"http://localhost/" + id,                   // …including the default one
		"https://localhost:8481/" + id,             // a different scheme is a different origin
		"//localhost:8481/" + id,                   // scheme-relative
		"http://xn--localhost-1s3d.example/" + id,  // punycode lookalike
		testOrigin + "/CLIPS/" + id,                // paths are case-SENSITIVE
		testOrigin + "/" + id + "/extra",
		testOrigin + "/%2e%2e", // the id must be an id
		testOrigin + "/not-an-id",
		testOrigin + "/c/" + id, // a collection has no oEmbed
		testOrigin + "/",
		"not a url at all",
		"javascript:alert(1)",
	}
	for _, in := range bad {
		r := e.do("GET", oembedURL(in), nil)
		if r.code != 404 {
			t.Errorf("oEmbed(%q) = %d %s, want 404", in, r.code, r.body)
		}
		// The 404 must not report on the URL it was handed: a message that
		// told a caller "that host is not ours" versus "that clip is gone"
		// would make the route an oracle about both.
		if strings.Contains(string(r.body), "evil.example") {
			t.Errorf("the 404 echoed the URL: %s", r.body)
		}
	}
}

// TestOEmbedMissingClipIs404AndBackendFailureIs503-shaped: an unknown but
// well-formed id is 404, and a tombstone is 404 too.
func TestOEmbedMissingClip(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser("oe")
	if r := e.do("GET", oembedURL(testOrigin+"/AaBbCcDdEeFf"), nil); r.code != 404 {
		t.Errorf("unknown clip = %d", r.code)
	}
	gone := e.deletingClip(u)
	if r := e.do("GET", oembedURL(testOrigin+"/"+gone.ID), nil); r.code != 404 {
		t.Errorf("tombstone = %d", r.code)
	}
}

// TestOEmbedFormatAndBounds: the parameter grammar.
func TestOEmbedFormatAndBounds(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser("oe")
	c, _, _ := e.readyClip(u, clipOpts{title: "t"})
	page := testOrigin + "/" + c.ID

	if r := e.do("GET", "/oembed?url="+url.QueryEscape(page), nil); r.code != 200 {
		t.Errorf("an absent format is json: %d %s", r.code, r.body)
	}
	if r := e.do("GET", "/oembed?url="+url.QueryEscape(page)+"&format=XML", nil); r.code != 501 {
		t.Errorf("format=xml = %d, want 501", r.code)
	}
	if r := e.do("GET", "/oembed?url="+url.QueryEscape(page)+"&format=yaml", nil); r.code != 400 {
		t.Errorf("format=yaml = %d, want 400", r.code)
	}
	if r := e.do("GET", "/oembed", nil); r.code != 400 || r.errCode() != "invalid_request" {
		t.Errorf("no url = %d %s", r.code, r.body)
	}
	for _, bad := range []string{"0", "-1", "big", "10001"} {
		if r := e.do("GET", oembedURL(page, "maxwidth", bad), nil); r.code != 400 {
			t.Errorf("maxwidth=%s = %d", bad, r.code)
		}
	}
	if r := e.do("GET", "/oembed?url="+url.QueryEscape(page), nil, withHeader("Origin", "https://evil.example")); r.code != 200 {
		t.Errorf("oEmbed is anonymous and cross-origin readable: %d", r.code)
	}
	if r := e.do("POST", oembedURL(page), nil); r.code != 405 {
		t.Errorf("POST /oembed = %d", r.code)
	}
}

// TestOEmbedMaxDimensionsScaleDownOnly.
func TestOEmbedMaxDimensionsScaleDownOnly(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser("oe")
	c, _, _ := e.readyClip(u, clipOpts{title: "t"})
	page := testOrigin + "/" + c.ID

	full := e.do("GET", oembedURL(page), nil)
	w, h := full.json["width"].(float64), full.json["height"].(float64)
	if w <= 0 || h <= 0 {
		t.Fatalf("no dimensions: %s", full.body)
	}
	r := e.do("GET", oembedURL(page, "maxwidth", "320"), nil)
	gw, gh := r.json["width"].(float64), r.json["height"].(float64)
	if gw != 320 {
		t.Errorf("maxwidth=320 → width %v", gw)
	}
	// The aspect survives, to within the rounding of one pixel.
	if diff := gw/gh - w/h; diff > 0.01 || diff < -0.01 {
		t.Errorf("aspect changed: %vx%v from %vx%v", gw, gh, w, h)
	}
	if !strings.Contains(r.str("html"), `width="320"`) {
		t.Errorf("the iframe was not resized: %q", r.str("html"))
	}
	// A ceiling ABOVE the clip is not an instruction to grow.
	big := e.do("GET", oembedURL(page, "maxwidth", "4000", "maxheight", "4000"), nil)
	if big.json["width"] != w || big.json["height"] != h {
		t.Errorf("a large maxwidth scaled the player UP: %s", big.body)
	}
}

// TestFitBox is the arithmetic on its own, including the degenerate inputs a
// consumer can send.
func TestFitBox(t *testing.T) {
	cases := []struct{ w, h, maxW, maxH, wantW, wantH int }{
		{1280, 720, 0, 0, 1280, 720},
		{1280, 720, 640, 0, 640, 360},
		{1280, 720, 0, 360, 640, 360},
		{1280, 720, 640, 100, 177, 100},
		{1280, 720, 4000, 4000, 1280, 720},
		{1280, 720, 1, 1, 1, 1},
		{720, 1280, 0, 640, 360, 640},
	}
	for _, c := range cases {
		w, h := fitBox(c.w, c.h, c.maxW, c.maxH)
		if w != c.wantW || h != c.wantH {
			t.Errorf("fitBox(%d,%d,%d,%d) = %d,%d; want %d,%d", c.w, c.h, c.maxW, c.maxH, w, h, c.wantW, c.wantH)
		}
	}
}

// The link-out document a consumer gets for a clip that is not ready: the
// picture and its dimensions now come from internal/urls, which internal/web
// reads for the share page's own og:image and whose static test decodes the
// real PNG against those same two numbers. Before that they were four private
// literals in this file, so renaming web/static/card-placeholder.png would
// have left every test green while oEmbed shipped a thumbnail_url that 404s —
// nothing here has ever fetched it (N063).
func TestOEmbedThumbnailFactsComeFromOneHome(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser("oe")
	c := e.seedClip(u, clipOpts{title: "encoding"})
	r := e.do("GET", oembedURL(testOrigin+"/"+c.ID), nil)
	if r.code != 200 || r.str("type") != "link" {
		t.Fatalf("GET /oembed = %d %s", r.code, r.body)
	}
	if got := r.str("thumbnail_url"); !strings.Contains(got, urls.PlaceholderCard) {
		t.Fatalf("thumbnail_url = %q, want the placeholder internal/urls names", got)
	}
	if r.json["thumbnail_width"] != float64(urls.PlaceholderWidth) || r.json["thumbnail_height"] != float64(urls.PlaceholderHeight) {
		t.Fatalf("thumbnail dimensions = %s, want internal/urls' %dx%d",
			r.body, urls.PlaceholderWidth, urls.PlaceholderHeight)
	}
}

// TestOEmbedProviderNameIsThePublicHost (§20 erratum 287, superseding 285's
// badge on this line): a consumer that renders the provider line — Slack does,
// Discord does — sees the instance's own HOST, which names the box exactly and
// is the one thing on a card a reader can type. The badge no longer reaches it:
// `dev.clippet.gg` already says which instance that is, and an operator whose
// dev box is on a host of its own gets the same answer with no variable set.
func TestOEmbedProviderNameIsThePublicHost(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"clippet.gg", "clippet.gg"},
		{"dev.clippet.gg", "dev.clippet.gg"},
		{"clips.example.com", "clips.example.com"},
		{"", oembedProvider}, // a builder with no host at all: never a blank line
	} {
		if got := providerNameOf(tc.host); got != tc.want {
			t.Errorf("providerNameOf(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
	// A BADGED instance is proof that the badge does not reach this line.
	e := newEnv(t, func(d *Deps) { d.Cfg.InstanceBadge = "dev" })
	u, _ := e.newUser("oeb")
	c, _, _ := e.readyClip(u, clipOpts{title: "Badged clip"})
	// A ready clip (type video) and a processing one (type link) carry it alike:
	// the provider is the instance, not the state of one of its clips.
	pending := e.processingClip(u, clipOpts{title: "Badged pending"})
	for _, id := range []string{c.ID, pending.ID} {
		r := e.do("GET", oembedURL(testOrigin+"/"+id), nil)
		if r.code != 200 {
			t.Fatalf("GET /oembed for %s = %d %s", id, r.code, r.body)
		}
		if r.str("provider_name") != testHost {
			t.Errorf("provider_name = %q, want the public host %q (%s)", r.str("provider_name"), testHost, r.body)
		}
		// Nothing else in the document moved: the provider URL is still the
		// whole origin, scheme and port included.
		if r.str("provider_url") != testOrigin {
			t.Errorf("provider_url = %q, want %q", r.str("provider_url"), testOrigin)
		}
	}
}
