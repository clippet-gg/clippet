// Package templates embeds the html/template sources rendered by
// internal/web (DESIGN.md §9). It lives next to the files because the
// pattern of an embed directive cannot reach outside its own directory, and
// the templates belong at web/templates by design (§3). It has no logic and
// no imports beyond embed.
package templates

import "embed"

// FS holds every *.html template.
//
//go:embed *.html
var FS embed.FS
