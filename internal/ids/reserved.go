package ids

// Reserved is every FIRST PATH SEGMENT this app routes, and it is the ONE
// place that list lives (§20 erratum 248).
//
// Since the share link is `{PUBLIC_BASE_URL}/{id}`, a clip id occupies the
// same space as a page path, so a minted id must never equal one of these —
// New and NewMemorable check it, and neither can return such an id.
//
// It is also read from the other end: internal/web's and internal/httpapi's
// router tests walk the patterns their mux actually registers and assert that
// every literal first segment appears here. Adding a route without adding its
// word therefore fails a test rather than shadowing a clip, which is the whole
// reason this is a slice and not a sentence in a comment.
//
// Three entries are here for the operator's sake rather than the router's:
// `robots.txt`, `manifest.webmanifest` and — since §20 erratum 304 —
// `favicon.ico` ARE all routes. The last was on this list before it was one,
// because it is what a client asks for unprompted; it is answered now, and the
// word does not move. All three carry a dot, which neither id grammar admits,
// so they can only ever be belt and braces; leaving them out would invite the
// next reader to work that out again.
var Reserved = []string{
	"api",
	"c",
	"clips",
	"embed",
	"favicon.ico",
	"healthz",
	"in", // POST /in/{token}, the inbound URL (§20 erratum 322)
	"login",
	"manifest.webmanifest",
	"oembed",
	"readyz",
	"register",
	"robots.txt",
	"settings",
	"setup",
	"static",
}

// IsReserved reports whether s is one of Reserved, compared
// case-insensitively over ASCII: paths are matched case-sensitively by the mux
// and ids are case-sensitive too, but `Login` reading as a route to a person
// is reason enough never to mint it. No allocation — this runs on every mint.
func IsReserved(s string) bool {
	for _, r := range Reserved {
		if equalFold(s, r) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if lowerASCII(a[i]) != lowerASCII(b[i]) {
			return false
		}
	}
	return true
}

func lowerASCII(c byte) byte {
	if isUpper(c) {
		return c + ('a' - 'A')
	}
	return c
}
