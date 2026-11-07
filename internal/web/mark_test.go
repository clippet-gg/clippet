package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The companion mark (ROADMAP #43, §20 errata 301 and 306; docs/brand/README.md): the
// striped clapper on the surfaces the wordmark cannot reach, at every size. Two pins
// here. The favicon is read
// back out of the rendered pages and DECODED — the data: URI is a percent-encoded SVG,
// and what the tab draws is its element count, its geometry and its two colour rules,
// so those are what is asserted, not a byte string. The raster assets are pinned by a
// golden over their decoded PIXELS (never the compressed bytes, which depend on the
// encoder's zlib) plus a handful of probe pixels that say WHAT the golden is of: a hole
// in the lid is the tile showing through, the span is green, the corner is transparent.
// scripts/lib/mkicons.py --check and mkcard.py --check (check.sh stages) prove the files
// match their generator; this proves the files the BINARY embeds are the mark.

var faviconLink = regexp.MustCompile(`<link rel="icon" href="([^"]+)">`)

type svgShape struct {
	XMLName   xml.Name
	Class     string `xml:"class,attr"`
	D         string `xml:"d,attr"`
	Transform string `xml:"transform,attr"`
	FillRule  string `xml:"fill-rule,attr"`
	X         string `xml:"x,attr"`
	Y         string `xml:"y,attr"`
	Width     string `xml:"width,attr"`
	Height    string `xml:"height,attr"`
	RX        string `xml:"rx,attr"`
}

type svgDoc struct {
	ViewBox string     `xml:"viewBox,attr"`
	Style   string     `xml:"style"`
	Shapes  []svgShape `xml:",any"`
}

// faviconOf pulls the one icon link out of a page and decodes it.
func faviconOf(t *testing.T, page, body string) (string, svgDoc) {
	t.Helper()
	m := faviconLink.FindAllStringSubmatch(body, -1)
	if len(m) != 1 {
		t.Fatalf("%s: %d icon links, want 1", page, len(m))
	}
	href := m[0][1]
	const prefix = "data:image/svg+xml,"
	if !strings.HasPrefix(href, prefix) {
		t.Fatalf("%s: the favicon is not an inline SVG data: URI: %.40s…", page, href)
	}
	enc := href[len(prefix):]
	// Percent-encoded IN FULL: no scheme literal, no raw markup, reaches the template — the
	// outbound-URL tripwire in check.sh reads base.html, and `xmlns` names an origin.
	for _, raw := range []string{"://", "<", ">", "\"", " "} {
		if strings.Contains(enc, raw) {
			t.Fatalf("%s: the favicon URI carries %q unencoded", page, raw)
		}
	}
	svg, err := url.PathUnescape(enc)
	if err != nil {
		t.Fatalf("%s: unescape: %v", page, err)
	}
	var doc svgDoc
	if err := xml.Unmarshal([]byte(svg), &doc); err != nil {
		t.Fatalf("%s: the favicon does not parse as XML: %v\n%s", page, err, svg)
	}
	return href, doc
}

// TestFaviconIsTheStripedMark: the favicon a PAGE declares is the striped clapper itself,
// the UNIT geometry docs/brand/mark.svg carries, at every size a client draws it (§20
// erratum 306 — the owner revoked 301's size rule and the plain mitre is gone from the repo).
// Four pieces in font units, the lid one even-odd path whose three cuts are holes, the
// span the wordmark's own — under the two colour rules the pp favicon carried (light ink
// and green; the dark theme's under prefers-color-scheme). The share page and the embed
// carry the identical URI.
func TestFaviconIsTheStripedMark(t *testing.T) {
	f := newFixture(t)
	h := f.handler(passAuth{})
	var hrefs []string
	for _, page := range []string{"/" + f.ready.ID, "/embed/" + f.ready.ID} {
		body := get(h, page).Body.String()
		href, doc := faviconOf(t, page, body)
		hrefs = append(hrefs, href)
		// The mark's own box, square, with the same 1/16 inset the ICO's entries have —
		// not a 16 px grid: there is one drawing now and the client scales it.
		if doc.ViewBox != "-104 -306 1624 1624" {
			t.Errorf("%s: viewBox %q, want the mark's own box in font units", page, doc.ViewBox)
		}
		const style = ".p{fill:#17181C}.s{fill:#0E7C4F}@media(prefers-color-scheme:dark){.p{fill:#ECE9E2}.s{fill:#4CD69A}}"
		if doc.Style != style {
			t.Errorf("%s: the colour rules are\n  %s\nwant\n  %s", page, doc.Style, style)
		}
		if len(doc.Shapes) != 4 {
			t.Fatalf("%s: %d pieces, want 4 (two stems, the lid, the span): %+v", page, len(doc.Shapes), doc.Shapes)
		}
		var ink, span, lids int
		for _, s := range doc.Shapes {
			switch s.Class {
			case "p":
				ink++
			case "s":
				span++
				if s.XMLName.Local != "rect" || s.X != "0" || s.Y != "1000" || s.Width != "1418" || s.Height != "200" || s.RX != "100" {
					t.Errorf("%s: the span is %+v, want the wordmark's 1418 × 200 capsule at y = 1000", page, s)
				}
			default:
				t.Errorf("%s: a piece with class %q", page, s.Class)
			}
			if strings.HasPrefix(s.Transform, "rotate(-9 0 230)") {
				lids++
				// THE STRIPES: the lid's outline and its three cuts are four subpaths of
				// one even-odd path, so the cuts are holes and never paper over ink.
				if s.FillRule != "evenodd" {
					t.Errorf("%s: the lid's fill-rule is %q, want evenodd — its cuts are holes", page, s.FillRule)
				}
				if got := strings.Count(s.D, "M"); got != 4 {
					t.Errorf("%s: the lid has %d subpaths, want 4 (the outline and three cuts): %q", page, got, s.D)
				}
				if strings.Count(s.D, "A230 230") != 2 {
					t.Errorf("%s: the lid is %q, want the two corner arcs of the unit geometry", page, s.D)
				}
			}
		}
		if ink != 3 || span != 1 || lids != 1 {
			t.Errorf("%s: %d ink pieces, %d span, %d lid turned 9° at the hinge; want 3, 1, 1", page, ink, span, lids)
		}
		// The stems in font units: the left one 272 × 570 at x = 0, the right one grown up
		// to the lid with its top cut on the lid's angle.
		var left, right bool
		for _, s := range doc.Shapes {
			if s.XMLName.Local == "rect" && s.Class == "p" && s.X == "0" && s.Y == "230" && s.Width == "272" && s.Height == "570" {
				left = true
			}
			if s.XMLName.Local == "path" && s.Class == "p" && s.Transform == "" && strings.HasPrefix(s.D, "M1146 18L1418 5V800H1146Z") {
				right = true
			}
		}
		if !left || !right {
			t.Errorf("%s: stems left=%v right=%v; the mitre's stems are not there", page, left, right)
		}
		// Nothing of the pp glyph: no quadratic in any piece.
		for _, s := range doc.Shapes {
			if strings.Contains(s.D, "Q") {
				t.Errorf("%s: a piece carries a glyph curve: %q", page, s.D)
			}
		}
	}
	if hrefs[0] != hrefs[1] {
		t.Errorf("the share page and the embed carry different favicons")
	}
}

// pixels decodes a PNG into rows of RGBA bytes and returns the image, its sha256 over
// those rows and the rows' width and height.
func pixels(t *testing.T, name string, r io.Reader) (*image.NRGBA, string) {
	t.Helper()
	img, err := png.Decode(r)
	if err != nil {
		t.Fatalf("%s: decode: %v", name, err)
	}
	b := img.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	h := sha256.New()
	row := make([]byte, 4*b.Dx())
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			c := color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			out.SetNRGBA(x, y, c)
			row[4*x], row[4*x+1], row[4*x+2], row[4*x+3] = c.R, c.G, c.B, c.A
		}
		h.Write(row)
	}
	return out, hex.EncodeToString(h.Sum(nil))
}

type probe struct {
	x, y int
	want color.NRGBA
}

var (
	tileInk   = color.NRGBA{0x17, 0x18, 0x1C, 255} // the ink tile
	tilePaper = color.NRGBA{0xEC, 0xED, 0xEF, 255} // the mark on it
	tileGreen = color.NRGBA{0x4C, 0xD6, 0x9A, 255} // the span on ink
	devGrey   = color.NRGBA{0x83, 0x7F, 0x77, 255} // the DEV application's span
	autoBlue  = color.NRGBA{0x2F, 0xA8, 0xE8, 255} // the AUTO application's span (STYLE.md: never a UI colour)
	cardInk   = color.NRGBA{0x19, 0x18, 0x16, 255} // the card's ground (--bg, dark)
	cardText  = color.NRGBA{0xEC, 0xE9, 0xE2, 255} // the mark and the letters (--text, dark)
	clear     = color.NRGBA{0, 0, 0, 0}
)

// TestMarkAssetsMatchTheGolden pins the rasters the binary embeds and the six the
// operator uploads to Discord by hand (docs/brand/discord). The probe points are font-unit
// positions on the mark mapped through the generator's own box (2400 units, the mark's box
// at (491, 713)): the span's centre (709, 1100), the first cut's centre (526, 115) and the
// lid's flat run before it (250, 115) — both turned 9° about the hinge — a point in the
// left stem (136, 500), the open window (709, 500) and the tile's corner.
func TestMarkAssetsMatchTheGolden(t *testing.T) {
	f := newFixture(t)
	h := f.handler(passAuth{})
	brand := filepath.Join("..", "..", "docs", "brand", "discord")
	open := func(name string) io.Reader {
		t.Helper()
		if strings.HasPrefix(name, "docs/") {
			b, err := os.ReadFile(filepath.Join(brand, filepath.Base(name)))
			if err != nil {
				t.Fatalf("%s: %v (the Discord icons live beside the SVG in docs/brand)", name, err)
			}
			return strings.NewReader(string(b))
		}
		rec := get(h, "/static/"+name)
		if rec.Code != 200 {
			t.Fatalf("GET /static/%s = %d", name, rec.Code)
		}
		return rec.Body
	}
	cases := []struct {
		name   string
		w, h   int
		sha    string
		probes []probe
	}{
		{"icon-192.png", 192, 192, "b247507c59928239d64b33fae327613c2947cb029dc6eece002d249eac96cecc", []probe{
			{96, 145, tileGreen}, {79, 59, tileInk}, {57, 63, tilePaper}, {50, 97, tilePaper}, {96, 97, tileInk}, {0, 0, clear}}},
		// The dev instance's card thumbnail (§20 erratum 316, the owner's pick (b)): icon-192 with the dev
		// application's GREY span and nothing else changed — every other probe is icon-192's own.
		{"icon-dev-192.png", 192, 192, "97bdfe45c0dc16d0509607b642bb7cc1e8bd92360ceb661e1b30f61a7c7e650f", []probe{
			{96, 145, devGrey}, {79, 59, tileInk}, {57, 63, tilePaper}, {50, 97, tilePaper}, {96, 97, tileInk}, {0, 0, clear}}},
		{"icon-512.png", 512, 512, "a3f49d22fd0aedd05f67bc260ee53cd482a27849e0a32122a65879df8977ff5a", []probe{
			{256, 386, tileGreen}, {211, 159, tileInk}, {153, 168, tilePaper}, {133, 258, tilePaper}, {256, 258, tileInk}, {0, 0, clear}}},
		// The card (mkcard.py): the mark 340 px tall on the left margin, the word 64 px under it.
		// The mark's probes are the same font-unit points through the card's own transform; the
		// word's are the centre of its span (the pp's, 200 units under the descenders) and a point
		// in the l's stem. The far corners are the flat ground.
		{"card-placeholder.png", 1200, 630, "8ca97105ed7da66d5a32541617becbb90f9a13408bd306c598a3ad018ae7a9ae", []probe{
			{294, 400, tileGreen}, {243, 139, cardInk}, {176, 149, cardText}, {153, 253, cardText}, {294, 253, cardInk},
			{210, 542, tileGreen}, {159, 497, cardText}, {5, 5, cardInk}, {1190, 620, cardInk}}},
		{"docs/clippet-512.png", 512, 512, "3db076ce56c4e2deeb819e43b2f198be0856fd13b46a04b0072eac9923a1e128", []probe{
			{256, 386, tileGreen}, {211, 159, tileInk}, {153, 168, tilePaper}, {133, 258, tilePaper}, {256, 258, tileInk}, {0, 0, tileInk}}},
		{"docs/clippet-1024.png", 1024, 1024, "dc8bb422491e38339a29b4efdba1e120c51a7c9179a72bd2918dfa9df89ce269", []probe{
			{512, 773, tileGreen}, {423, 318, tileInk}, {307, 337, tilePaper}, {267, 517, tilePaper}, {512, 517, tileInk}, {0, 0, tileInk}}},
		{"docs/clippet-dev-512.png", 512, 512, "bb0f90612f2e2e823f5248a459f23dbbf2e210f4f273034181d0c7b4b2aebcea", []probe{
			{256, 386, devGrey}, {211, 159, tileInk}, {153, 168, tilePaper}, {133, 258, tilePaper}, {256, 258, tileInk}, {0, 0, tileInk}}},
		{"docs/clippet-dev-1024.png", 1024, 1024, "b18e1f50cd58df8373df49b5ac54c8bd230340d91e8a71472873d055b5210df8", []probe{
			{512, 773, devGrey}, {423, 318, tileInk}, {307, 337, tilePaper}, {267, 517, tilePaper}, {512, 517, tileInk}, {0, 0, tileInk}}},
		// The AUTO application's icon (run 8): the same drawing with the BLUE span.
		{"docs/clippet-auto-512.png", 512, 512, "262dbd9627ae5782bf34746af7aaab98f656530fe6f87a0b8645c3b757668cf3", []probe{
			{256, 386, autoBlue}, {211, 159, tileInk}, {153, 168, tilePaper}, {133, 258, tilePaper}, {256, 258, tileInk}, {0, 0, tileInk}}},
		{"docs/clippet-auto-1024.png", 1024, 1024, "dd5ad0bbac348b4616e1d68e3fceb56124162d5789befc1cedf0f184a69a503f", []probe{
			{512, 773, autoBlue}, {423, 318, tileInk}, {307, 337, tilePaper}, {267, 517, tilePaper}, {512, 517, tileInk}, {0, 0, tileInk}}},
	}
	imgs := map[string]*image.NRGBA{}
	for _, tc := range cases {
		img, sha := pixels(t, tc.name, open(tc.name))
		imgs[tc.name] = img
		if b := img.Bounds(); b.Dx() != tc.w || b.Dy() != tc.h {
			t.Errorf("%s: %dx%d, want %dx%d", tc.name, b.Dx(), b.Dy(), tc.w, tc.h)
			continue
		}
		for _, p := range tc.probes {
			if got := img.NRGBAAt(p.x, p.y); got != p.want {
				t.Errorf("%s: pixel (%d,%d) = %v, want %v", tc.name, p.x, p.y, got, p.want)
			}
		}
		if sha != tc.sha {
			t.Errorf("%s: the decoded pixels hash to %s, not the golden %s — if the mark moved on purpose, "+
				"re-run scripts/lib/mkicons.py and mkcard.py, look at the files, and update the golden", tc.name, sha, tc.sha)
		}
	}
	// The DEV application's icon differs from production in the colour of the span and in
	// nothing else (docs/brand/README.md; the rule INSTANCE_BADGE repeats on the wordmark, §20
	// erratum 285). Every pixel that differs is a blend of the span's colour over the ink at
	// the same coverage in both files: read the coverage off the production pixel's green
	// channel and the dev pixel must be the grey at that coverage, to a rounding step.
	// The AUTO application's icon is the same rule with the blue span (run 8).
	for _, v := range []struct {
		name string
		span color.NRGBA
	}{{"dev", devGrey}, {"auto", autoBlue}} {
		for _, size := range []string{"512", "1024"} {
			prod, alt := imgs["docs/clippet-"+size+".png"], imgs["docs/clippet-"+v.name+"-"+size+".png"]
			if prod == nil || alt == nil {
				continue
			}
			b := prod.Bounds()
			differ := 0
			for y := 0; y < b.Dy(); y++ {
				for x := 0; x < b.Dx(); x++ {
					p, d := prod.NRGBAAt(x, y), alt.NRGBAAt(x, y)
					if p == d {
						continue
					}
					differ++
					cov := float64(int(p.G)-int(tileInk.G)) / float64(int(tileGreen.G)-int(tileInk.G))
					want := color.NRGBA{blend(tileInk.R, v.span.R, cov), blend(tileInk.G, v.span.G, cov), blend(tileInk.B, v.span.B, cov), 255}
					if abs(int(d.R)-int(want.R)) > 2 || abs(int(d.G)-int(want.G)) > 2 || abs(int(d.B)-int(want.B)) > 2 || d.A != 255 {
						t.Fatalf("clippet-%s vs clippet-%s-%s at (%d,%d): production %v, %s %v — not the %s span at the same coverage (%v)",
							size, v.name, size, x, y, p, v.name, d, v.name, want)
					}
				}
			}
			if differ == 0 {
				t.Errorf("clippet-%s and clippet-%s-%s are identical; the %s icon wears no span of its own", size, v.name, size, v.name)
			}
		}
	}
}

func blend(a, b uint8, t float64) uint8 {
	v := float64(a) + (float64(b)-float64(a))*t
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v + 0.5)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
