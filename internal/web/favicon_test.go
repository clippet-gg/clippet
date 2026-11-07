package web

import (
	"bytes"
	"encoding/binary"
	"image/color"
	"net/http"
	"strings"
	"testing"
)

// GET /favicon.ico (§20 erratum 304). Before it, the path answered the router's
// 404 page: every page already declares its own `data:` icon, so a browser
// looking at a page never asks, but a caller that has only ever been handed a
// URL — a bookmark manager, a link previewer, Safari's pinned-tab fallback —
// asks for this exact path unprompted and got HTML.
//
// Three files' worth of behaviour meets here, so the pins below are in three
// groups: the ROUTE (status, type, cache, conditional, method, principal), the
// CONTAINER (an ICO directory of three PNG entries, which is what makes the
// answer readable at all), and the DRAWING (the mark, at each size, through the
// running handler rather than off the disk).

// TestFaviconRoute is the route's own contract.
func TestFaviconRoute(t *testing.T) {
	f := newFixture(t)
	h := f.handler(passAuth{})
	rec := get(h, "/favicon.ico")
	if rec.Code != 200 {
		t.Fatalf("GET /favicon.ico = %d", rec.Code)
	}
	// The IANA registration on the wire — and the ROW that decides it. Go's own
	// table agrees today (go1.27 `mime.builtinTypesLower` has `.ico`), so the
	// header assertion alone would hold with staticTypes' row deleted and would
	// be pinning the toolchain rather than this product. The second check is
	// what makes the choice ours: it is the string `guard/scripts/manifest.mjs`
	// mirrors for a Pass name, and the one a system mime database could
	// otherwise move (§20 erratum 304 (e)).
	if ct := rec.Header().Get("Content-Type"); ct != "image/vnd.microsoft.icon" {
		t.Errorf("Content-Type %q, want the registered icon type", ct)
	}
	if got, ok := staticTypes[".ico"]; !ok || got != "image/vnd.microsoft.icon" {
		t.Errorf("staticTypes[\".ico\"] = %q (present=%v), want image/vnd.microsoft.icon — the product "+
			"spells this type rather than inheriting it, and the guard's manifest records the same string", got, ok)
	}
	// A DAY, not staticCacheControl's hour: this path carries no ?v=<build id>
	// and nothing renders it into a page, so there is no busting to do.
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=86400" {
		t.Errorf("Cache-Control %q, want a day", cc)
	}
	etag := rec.Header().Get("ETag")
	if !strings.HasPrefix(etag, `"`) || strings.HasPrefix(etag, `W/`) || len(etag) < 8 {
		t.Errorf("ETag %q, want a strong one", etag)
	}
	// It is the SAME file /static/ serves, so it is the same validator: the
	// route is a second address for one embedded asset, not a second copy.
	if s := get(h, "/static/favicon.ico"); s.Code != 200 || s.Header().Get("ETag") != etag {
		t.Errorf("/static/favicon.ico = %d ETag=%q, want 200 and %q", s.Code, s.Header().Get("ETag"), etag)
	}
	if got := rec.Body.Len(); got == 0 {
		t.Fatal("the body is empty")
	}

	// A conditional request is a 304 with no body — the point of the strong
	// ETag, and what keeps a client that asks on every visit cheap.
	if nm := do(h, http.MethodGet, "/favicon.ico", "If-None-Match", etag); nm.Code != 304 || nm.Body.Len() != 0 {
		t.Errorf("If-None-Match: %d body=%d, want 304 and nothing", nm.Code, nm.Body.Len())
	}
	if nm := do(h, http.MethodGet, "/favicon.ico", "If-None-Match", `"not-this-one"`); nm.Code != 200 {
		t.Errorf("a stale If-None-Match: %d, want the file", nm.Code)
	}

	// getOnly's shape: HEAD answers, everything else is 405 with Allow.
	if hd := do(h, http.MethodHead, "/favicon.ico"); hd.Code != 200 || hd.Body.Len() != 0 || hd.Header().Get("Content-Length") == "" {
		t.Errorf("HEAD: %d body=%d len=%q", hd.Code, hd.Body.Len(), hd.Header().Get("Content-Length"))
	}
	for _, m := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch} {
		if r := do(h, m, "/favicon.ico"); r.Code != 405 || r.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s /favicon.ico: %d Allow=%q", m, r.Code, r.Header().Get("Allow"))
		}
	}
	// It carries the page header set like every other non-embed route.
	assertNonEmbedHeaders(t, rec)
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("/favicon.ico: nosniff missing")
	}
}

// TestFaviconIsAnonymous: the answer does not depend on the principal, so the
// route must not cost the Resolver a session lookup — the same rule /static/,
// /robots.txt and the manifest follow (§9). A crawler hitting this path on
// every page it visits would otherwise be a session lookup each time.
func TestFaviconIsAnonymous(t *testing.T) {
	f := newFixture(t)
	n := 0
	h := f.handler(countingAuth{&n})
	if rec := get(h, "/favicon.ico"); rec.Code != 200 || n != 0 {
		t.Errorf("GET /favicon.ico: status %d, authenticator ran %d times (want 0)", rec.Code, n)
	}
	// And it sets no cookie and does not vary on one.
	rec := get(f.handler(passAuth{}), "/favicon.ico")
	if c := rec.Header().Get("Set-Cookie"); c != "" {
		t.Errorf("Set-Cookie %q on a public asset", c)
	}
	if v := rec.Header().Get("Vary"); strings.Contains(v, "Cookie") {
		t.Errorf("Vary %q, want no Cookie on a response every caller shares", v)
	}
}

// TestFaviconIsAnICOOfThreePNGs reads the served bytes as a container. The
// magic (`00 00 01 00`) and the entry count are what a client looks at before
// it looks at anything else, and PNG-in-ICO — rather than a BMP/DIB entry — is
// the choice erratum 304 makes: every current browser and every Windows since
// Vista reads it, and it needs no encoder beside the PNG writer the generator
// already has.
func TestFaviconIsAnICOOfThreePNGs(t *testing.T) {
	f := newFixture(t)
	body := get(f.handler(passAuth{}), "/favicon.ico").Body.Bytes()
	if len(body) < 6 {
		t.Fatalf("%d bytes is not an ICONDIR", len(body))
	}
	if !bytes.Equal(body[:4], []byte{0x00, 0x00, 0x01, 0x00}) {
		t.Fatalf("the first bytes are %x, want the ICO header 00000100", body[:4])
	}
	count := int(binary.LittleEndian.Uint16(body[4:6]))
	if count != 3 {
		t.Fatalf("%d directory entries, want 3 (16, 32 and 48)", count)
	}
	if len(body) < 6+16*count {
		t.Fatalf("%d bytes is too short for %d directory entries", len(body), count)
	}
	want := []int{16, 32, 48}
	for i := 0; i < count; i++ {
		e := body[6+16*i : 22+16*i]
		w, h := int(e[0]), int(e[1])
		length := int(binary.LittleEndian.Uint32(e[8:12]))
		at := int(binary.LittleEndian.Uint32(e[12:16]))
		if w != want[i] || h != want[i] {
			t.Errorf("entry %d is %dx%d, want %d square", i, w, h, want[i])
			continue
		}
		if at < 6+16*count || length <= 0 || at+length > len(body) {
			t.Fatalf("entry %d claims %d bytes at %d, in a %d-byte file", i, length, at, len(body))
		}
		if !bytes.HasPrefix(body[at:at+length], []byte("\x89PNG\r\n\x1a\n")) {
			t.Errorf("entry %d is not a PNG; PNG-in-ICO is what this file is", i)
		}
	}
}

// TestFaviconEntriesAreTheMark decodes each entry and pins the drawing, the
// same way TestMarkAssetsMatchTheGolden pins the manifest icons: a golden over
// the DECODED pixels (never the compressed bytes, which depend on the encoder's
// zlib) plus probes that say what the golden is OF.
//
// Since §20 erratum 306 the three entries are ONE drawing at three sizes: the
// striped mark out of the rasteriser at each of them, framed by the same 1/16
// inset, the supersampling carrying it down to 16 — 16 and 32 were the plain
// mitre until the owner revoked erratum 301's size rule. All three are INK ON
// TRANSPARENT with the green span: the favicon slot has no tile, exactly as the
// `data:` URI in base.html has none.
func TestFaviconEntriesAreTheMark(t *testing.T) {
	f := newFixture(t)
	body := get(f.handler(passAuth{}), "/favicon.ico").Body.Bytes()
	count := int(binary.LittleEndian.Uint16(body[4:6]))
	cases := []struct {
		size   int
		sha    string
		probes []probe
	}{
		// 16: the left stem, the lid, the open window between the stems, the span,
		// the corner. No cut probe: at 16 a cut is a third of a pixel wide, so the
		// stripes are an antialiased lightening of the lid rather than holes any
		// single pixel is fully inside — the golden is what pins them there.
		{16, "88886c5eaa253fb74f22a595582d16785c5fa0a0eb2940eb0d73d68469ff8f79", []probe{
			{1, 6, faviconInk}, {4, 3, faviconInk}, {8, 6, clear}, {8, 13, tileGreen}, {0, 0, clear}}},
		// 32: the same five, and from here the CUTS are whole transparent pixels —
		// two of the lid's three, which is what tells the mark from a plain mitre.
		{32, "7d83ba193d69f21af3d5dfa0c5c7028b9be233a3a4a7dcdf2d09aa1ca2e48ea6", []probe{
			{3, 15, faviconInk}, {5, 6, faviconInk}, {16, 12, clear}, {16, 27, tileGreen}, {0, 0, clear},
			{16, 6, clear}, {21, 6, clear}}},
		{48, "b72037407682d93aa955c714c00bfb8ff27c245c8be866f8dee5a29fae0ebebe", []probe{
			{5, 24, faviconInk}, {30, 5, faviconInk}, {24, 24, clear}, {24, 41, tileGreen}, {0, 0, clear},
			{19, 7, clear}, {26, 7, clear}}},
	}
	if count != len(cases) {
		t.Fatalf("%d entries, and this test knows %d", count, len(cases))
	}
	for i, tc := range cases {
		e := body[6+16*i : 22+16*i]
		at := int(binary.LittleEndian.Uint32(e[12:16]))
		length := int(binary.LittleEndian.Uint32(e[8:12]))
		img, sha := pixels(t, "favicon.ico", bytes.NewReader(body[at:at+length]))
		if b := img.Bounds(); b.Dx() != tc.size || b.Dy() != tc.size {
			t.Errorf("the %d px entry decodes to %dx%d", tc.size, b.Dx(), b.Dy())
			continue
		}
		for _, p := range tc.probes {
			if got := img.NRGBAAt(p.x, p.y); got != p.want {
				t.Errorf("favicon %d px: pixel (%d,%d) = %v, want %v", tc.size, p.x, p.y, got, p.want)
			}
		}
		if sha != tc.sha {
			t.Errorf("favicon %d px: the decoded pixels hash to %s, not the golden %s — if the mark moved "+
				"on purpose, re-run scripts/lib/mkicons.py, look at the file, and update the golden",
				tc.size, sha, tc.sha)
		}
	}
}

// faviconInk is the mark on a favicon: the LIGHT theme's ink, opaque, on
// nothing. It is the same colour tileInk paints, and it is named apart because
// it means the opposite thing — there it is the tile UNDER the mark, here it is
// the mark itself (an ICO carries no colour-scheme rule; the `data:` URI, which
// every browser prefers, is the one that follows the theme).
var faviconInk = color.NRGBA{0x17, 0x18, 0x1C, 255}
