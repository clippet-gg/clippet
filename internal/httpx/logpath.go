package httpx

import (
	"path"
	"regexp"
	"strconv"
	"strings"

	"clippet/internal/ids"
)

// The inbound URL's secret travels in a PATH (§20 erratum 322), and so does
// the Clippet Auto URL's (§20 erratum 324); a path is what every log line of
// this process names. LogPath is the one spelling of a request path that may
// be written to a log: AccessLog, Recover and the page handler's
// render-failure line all go through it, and a pin test refuses a log call
// that names `r.URL.Path` without it.
//
// The rule does not depend on the path having reached the inbound handler.
// AccessLog wraps the ROOT mux, which answers `//in/ci_…` (a base URL with a
// trailing slash joined to `/in/…` — a realistic tool misconfiguration) with
// its own redirect before any handler runs; `/IN/ci_…` and `/api/in/ci_…` reach
// other handlers entirely. So:
//
//  1. the CLEANED path `/in` or anything under `/in/` — `/in/auto/…` included —
//     is written `/in/{token}`;
//  2. otherwise every run of `ci_` or `ca_` and AT LEAST 43 base64url
//     characters anywhere in the path is replaced with `{token}` — at least,
//     not exactly (run 8): an exact count let a crafted `ci_<40>ci_<43>` match
//     from the first `ci_` and leave the real secret's body after it. BOTH
//     prefixes are tested before the regular expression is skipped (§20
//     erratum 324's review: a short-cut on `ci_` alone would let
//     `/IN/AUTO/ca_…` through).
//
// Since §20 erratum 341 the one-time login link is the third such secret: the
// cleaned path under `/login/discord/` is written `/login/discord/{token}`, and
// a `cs_` run is replaced like the other two wherever it appears.
const inboundLogPath = "/in/{token}"

const signinLogPath = "/login/discord/{token}"

var pathSecret = regexp.MustCompile(`c[ias]_[A-Za-z0-9_-]{` +
	strconv.Itoa(ids.InboundTokenLen-len(ids.InboundPrefix)) + `,}`)

// LogPath returns p as it may appear in a log line.
func LogPath(p string) string {
	c := path.Clean(p)
	if c == "/in" || strings.HasPrefix(c, "/in/") {
		return inboundLogPath
	}
	if strings.HasPrefix(c, "/login/discord/") {
		return signinLogPath
	}
	if !strings.Contains(p, ids.InboundPrefix) && !strings.Contains(p, ids.AutoFetchPrefix) && !strings.Contains(p, ids.SigninPrefix) {
		return p
	}
	return pathSecret.ReplaceAllLiteralString(p, "{token}")
}
