package web

import "strings"

// The embed overlay's lockup (§20 erratum 287; web/STYLE.md).
//
// The owner's ask of 2026-09-15, on the small mark burned into the corner of the
// player a stranger meets inside somebody else's page: spell the host
// (clippet.gg) in the baked-in part, in the clippet design language, readable on
// any background, without making it much more obtrusive.
//
// So the overlay says the instance's own public HOST rather than the product's
// name, for the same reason the card's site line does (siteNameOf): a viewer who
// likes this player can type what they just read. What the overlay may NOT do is
// stop being the logo — erratum 246 put the wordmark there, web/STYLE.md says
// the wordmark IS the logo, and 21 px of live text with a green span under the
// double p is the whole of it.
//
// The two survive together because `clippet.gg` CONTAINS the wordmark. On this
// product's own domain, and on any subdomain of it, the overlay is the lockup
// with a prefix and a suffix in the same face and size — `dev.` + `cli<span
// class="pp">pp</span>et` + `.gg` — so the green span still runs under the two
// stems and the mark is untouched. On ANY other host it is the host as plain
// text in the same style and there is no span at all: a self-hoster's
// `clips.example.com` is not this product's wordmark, and drawing the brand's
// green under two letters of somebody else's domain would be the logo claiming a
// name it does not have.
//
// Consequences of the plain form, both accepted: it carries no `.pp`, so the
// living mark's overlay is never injected there (app.js's setupEyes selects
// `.wordmark .pp`) and that instance's player wears no face; and the instance
// badge's one embed signal — the grey span of erratum 285 — has nothing to paint,
// which is no loss, because on such an instance the overlay already says which
// box this is by name.
const (
	// markWord is the wordmark, and the three pieces the template draws it from.
	// markSpan is the pair of stems the green span joins.
	markWord = markStem + markSpan + markTail
	markStem = "cli"
	markSpan = "pp"
	markTail = "et"
	// markDomain is the product's own domain. It is a literal, deliberately:
	// this is the one rule in the codebase that is about THIS product's name and
	// not about the instance it is running on, and a configurable "which host
	// counts as the brand" knob would let any operator paint the brand's green
	// under their own domain.
	markDomain = markWord + markTLD
	markTLD    = ".gg"
)

// hostMark is one rendering of the embed overlay: the whole host (the link's
// accessible name, and the text of the plain form), and — when the host is the
// product's own — what the lockup puts before `cli` and after `et`.
type hostMark struct {
	Host   string
	Prefix string
	Suffix string
	// Lockup is true when the host is `clippet.gg` or a subdomain of it, i.e.
	// when the wordmark's own letters are IN the host and the span can sit under
	// them. False renders the host as plain text.
	Lockup bool
}

// markOf resolves the overlay for a public host.
//
// The subdomain test is on `"." + markDomain` and not on a suffix of the bare
// domain: `notclippet.gg` and `evilclippet.gg` end with `clippet.gg` as a
// STRING and are somebody else's hosts. An empty host — the URL builder's zero
// value, which no running server has — renders as the bare wordmark, which is
// what the overlay was before this erratum.
func markOf(host string) hostMark {
	m := hostMark{Host: host}
	switch {
	case host == "":
		m.Host, m.Lockup = markWord, true
	case host == markDomain:
		m.Lockup, m.Suffix = true, markTLD
	case strings.HasSuffix(host, "."+markDomain):
		m.Lockup = true
		m.Prefix = strings.TrimSuffix(host, markDomain)
		m.Suffix = markTLD
	}
	return m
}
