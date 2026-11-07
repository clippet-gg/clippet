// Package static embeds the client assets served under /static/ (DESIGN.md
// §9): app.js, app.css, the 1200×630 link-out card placeholder, the two
// manifest icons, the favicon a client asks for unprompted and the self-hosted
// fonts under fonts/ (§20 errata 240, 304).
package static

import "embed"

// FS holds the embedded assets. The patterns are explicit so this file is
// never served. `fonts` embeds the directory whole: the two woff2 files the
// pages load, the TTF the poster label is drawn with (§20 erratum 287 — ffmpeg
// reads the copy the image installs, not this one; it is here so the family has
// ONE home in the repository) and the OFL licence text beside them, which the
// licence requires to travel with the fonts. `*.ico` is favicon.ico, which
// `/favicon.ico` serves straight out of this FS (§20 erratum 304); it is a
// file of this directory like any other, so `/static/favicon.ico` answers it
// too, with the same ETag.
//
//go:embed *.js *.css *.png *.ico fonts
var FS embed.FS

// The 192-pixel mark, by span (§20 erratum 285's rule, the owner's pick (b),
// 2026-10-05): the GREEN span is production, the GREY span any instance that
// wears an INSTANCE_BADGE. Both are drawn by scripts/lib/mkicons.py from one
// geometry and are never redrawn. MarkIcon192 is the one choice between them:
// the Discord card's thumbnail (internal/web) and an audio clip's poster
// (internal/worker, run 10) both take it.
const (
	MarkIcon192       = "icon-192.png"
	MarkIcon192Badged = "icon-dev-192.png"
)

// MarkIcon192For is the mark file in FS for an instance with this badge.
func MarkIcon192For(badge string) string {
	if badge != "" {
		return MarkIcon192Badged
	}
	return MarkIcon192
}
