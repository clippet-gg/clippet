// Package web renders the SSR pages from embedded templates and serves the
// static assets (DESIGN.md §9, §18.5).
//
// Every page is html/template rendered in-process from the local SQLite row
// under an SSR_TIMEOUT context. The share and embed pages follow the
// true-404 discipline of §9.1: only an invalid id, db.ErrNotFound or a
// `deleting` tombstone produce a 404; every other failure (including the
// deadline) is a 503 with Retry-After and no card tags, so a crawler never
// caches "no embed" for a clip that merely hit a backend hiccup.
//
// Card tags and the canonical link are absolute URLs from urls.Builder
// (crawlers need them); in-page resources (video/poster sources, static
// assets, the API paths app.js calls) are same-origin paths derived from the
// same Builder, so a page browsed under a host other than PUBLIC_BASE_URL
// still satisfies the CSP 'self' sources.
//
// No template contains an inline style, an inline script or an on* handler
// (CSP style-src 'self'; script-src 'self'); tests assert it on every
// rendered page.
package web

import (
	"log/slog"
	"net/http"
	"time"

	"clippet/internal/auth"
	"clippet/internal/config"
	"clippet/internal/cost"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/urls"
)

// Deps wires the page handlers. Auth is the *auth.Resolver in production
// and a stub Authenticator in tests.
type Deps struct {
	DB      *db.DB
	Auth    auth.Authenticator
	URLs    urls.Builder
	Cfg     config.Config
	BuildID string
	Log     *slog.Logger
	Now     func() time.Time
	// Budget is the shared storage-budget reading the sweeper publishes
	// (ROADMAP #24, §20 errata 157 and 277). The library renders one line from
	// it — saying which mode the budget is in — and the settings page's Storage
	// panel leads with the quota row; nil, no STORAGE_BUDGET_BYTES, or no
	// measurement yet → neither.
	Budget *httpx.StorageBudget
	// Served is the per-day served-bytes counter (ROADMAP #39, §20 erratum
	// 270). The settings page's "Storage" panel reads the same totals /healthz
	// does, through the same counter, so the two can never disagree. nil → the
	// panel is not rendered at all: with nothing counting, there is nothing
	// true to say.
	Served *cost.Counter
	// Health is the shared last-Ping cache serve.go's boot loop, /readyz and
	// /healthz already write and read (ROADMAP #45, §20 erratum 297). The
	// setup page's first panel renders its State() — `pending`, `ok` or
	// `error: <class>` — so the page, the probe and `admin doctor` cannot
	// disagree about whether the store answered. nil → the row reads
	// `pending`, which is what a page with nothing measuring has to say.
	Health *httpx.StorageHealth
}

// New mounts /, /login, /register, /settings (+ the /settings/tokens shim),
// /{id} — the share page, the id at the ROOT since §20 erratum 248 —
// /clips/{id} (that page's old address, now a 308 to it), /c/{id},
// /embed/{id}, /robots.txt, /manifest.webmanifest and /static/, and returns
// the handler serve.go installs at "/".
// The templates are parsed once here; a broken template is a boot error.
func New(d Deps) (http.Handler, error) {
	s, err := newServer(d)
	if err != nil {
		return nil, err
	}
	return s.handler(), nil
}

// Meta is one <meta> tag: Attr ∈ {"property", "name"}.
type Meta struct{ Attr, Key, Content string }

// ClipView is the input to BuildMeta and the clip templates.
//
// Tags is the clip's tag set. It reaches the SHARE page only for the owner
// (§9: an anonymous watch page is the clip and nothing else), so the field
// being populated is itself the owner decision — the template renders what it
// is given.
type ClipView struct {
	Clip    db.Clip
	URLs    urls.Builder
	Viewer  *db.User
	IsOwner bool
	Tags    []string
	// TrimLive says a trim job of this clip is queued or running right now.
	// The page carries it as `data-trim-live` so the owner's browser knows
	// whether there is anything to rejoin WITHOUT asking: GET …/trim answers
	// 404 for a clip that has never been trimmed, and asking on every load
	// put one 404 in the access log per view (§20 erratum 87). Read only for
	// the owner — the control it belongs to is owner-only.
	TrimLive bool
	// PosterLive says a poster job of this clip is queued or running right
	// now, and is read for the owner alone for TrimLive's reasons exactly:
	// the page carries it as `data-poster-live` so a reload rejoins a card
	// image being taken, and a clip nobody has chosen a frame for costs no
	// request at all (GET …/poster is a 404 for every one of them).
	PosterLive bool
	// ReplaceLive says a replacement of this clip is queued or running right
	// now — the same fact as the two above, for the same reason (§20 erratum
	// 240 closed the one owner poll that still asked the route: GET …/replace
	// is a 404 for every clip nobody has replaced, and a console error on
	// every owner view).
	ReplaceLive bool
	// Collections and InCollections are the owner's "file this clip" control
	// (ROADMAP #17): every collection the caller owns, and the set of those
	// that already hold this clip. Read for the owner alone — the control is
	// owner-only, and an anonymous view costs neither query.
	Collections   []db.Collection
	InCollections map[string]bool
}

// CollectionView is the input to BuildCollectionMeta and the collection
// template: the row, its members in position order, and who is asking.
type CollectionView struct {
	Collection db.Collection
	Clips      []db.Clip
	URLs       urls.Builder
	Viewer     *db.User
	IsOwner    bool
}
