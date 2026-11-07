package httpapi

import (
	"net/http"
	"regexp"

	"clippet/internal/config"
	"clippet/internal/httpx"
)

// The instance's own self-description: GET /api/instance (DESIGN.md §20
// erratum 303).
//
// Four facts about THIS build and THIS configuration, in one anonymous JSON
// object: which release it is, which commit that release was built from, and
// which of the two media-delivery shapes it serves clips in. Nothing about a
// clip, an account, a bucket, a credential or a path — the same rule /healthz
// keeps (§15), with the one amendment §15 already carries for the storage
// backend's name.
//
// Who reads it, and why it exists at all. A Clippet Pass name (a subdomain of
// the Pass operator's zone pointed at somebody's box; `DEPLOY.md` §36) is verified from
// OUTSIDE by a control plane that holds the release artefacts: it needs to know
// which release is answering, so an old one gets a banner and a revoked one is
// blocked, and it needs to know whether the box is serving media bytes through
// the tunnel, which a Pass name may not do. Both are questions about the
// PROCESS, and a process is the only thing that can answer them.
//
// This is not a "pass mode" and it is not a call home. The route is registered
// on every instance whether or not a Pass exists, its body is a function of the
// build and `.env` alone, and nothing in this binary branches on who asked or
// on whether anybody is asking — the free product has exactly the route, and
// exactly the behaviour, that a Pass name has. That is the whole of the
// product's side of the verification plane.
//
// Anonymous on purpose. A verifier reaching a box across the internet holds no
// credential on it (minting one would put a secret of ours on every customer's
// disk), and nothing here is private: the version is already in the anonymous
// `/healthz`, the delivery mode is observable in one request to any media route
// (a 200 or a 302), and the media origin is in the `Location` of that 302 and in
// the CSP of every page. The route publishes what the origin already shows,
// in one place and in one shape.

// instancePath is the route. `api` is a reserved first segment
// (internal/ids/reserved.go), so no clip id can collide with it.
const instancePath = "/api/instance"

// InstancePath is that path, for the docs and the CLI.
func InstancePath() string { return instancePath }

// instanceDTO is the body, and its FIELD ORDER is the wire order: four keys,
// always all four, never a fifth. `build` and `media_origin` are pointers
// because both have a meaningful JSON `null` — "this build carries no commit"
// and "media comes from this instance itself" — which an omitted key would not
// say (the same rule /healthz's optional objects keep, in the other direction).
type instanceDTO struct {
	Version       string  `json:"version"`
	Build         *string `json:"build"`
	MediaDelivery string  `json:"media_delivery"`
	MediaOrigin   *string `json:"media_origin"`
}

// commitRE is what may be published as `build`: a full git object name, lower
// case, and nothing else.
//
// The check is on the way OUT rather than at boot because the value arrives as
// a build argument — `docker compose build` interpolates `CLIPPET_COMMIT` from
// `.env`, and an operator who put `$(git rev-parse HEAD)` in the wrong quotes,
// or a CI that passed a tag, gets a string that is not a commit. The verifier
// that reads this field treats a non-hex value as DRIFT and disables the name
// (`provision/src/verify.ts acceptRelease`: 7-40 hex, compared on its first
// twelve), so publishing something shaped wrong is worse than publishing
// nothing. Anything but forty hex characters is therefore `null`, which every
// reader already handles because it is what an unstamped build answers.
var commitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// instance is GET /api/instance.
func (h *handler) instance(w http.ResponseWriter, r *http.Request) {
	// The delivery word comes from `RedirectsMedia()` and not from the raw
	// `MEDIA_DELIVERY` string, for the reason that predicate exists: it is the
	// ONE place the mode is decided, so the handler, the CSP and the cost line
	// cannot disagree about which shape is in force — and it makes this field
	// TOTAL. The plane accepts exactly the two words and reads anything else as
	// drift, and a zero-valued Config (a hand-built Deps; config.Load always
	// fills the field) behaves as proxy, so proxy is what it must say.
	delivery := config.DeliveryProxy
	if h.d.Cfg.RedirectsMedia() {
		delivery = config.DeliveryRedirect
	}
	body := instanceDTO{
		Version:       h.version(),
		MediaDelivery: delivery,
	}
	if commitRE.MatchString(h.d.Commit) {
		c := h.d.Commit
		body.Build = &c
	}
	// MediaOrigin() answers "" in proxy mode — every proxy instance, and a
	// redirect one before the store resolved its origin — and that is `null`
	// here rather than an empty string: the field names an origin or says
	// there is not one.
	if origin := h.d.Cfg.MediaOrigin(); origin != "" {
		body.MediaOrigin = &origin
	}
	// no-store for the reason /api/bot/claim has it: the answer changes with a
	// rebuild or an `.env` edit, and a verifier that read a cached one would
	// judge a box by the release it used to run.
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, body)
}

// version is what this build calls itself: `main.version`, which is already
// the string `dev` on an unstamped build (`cmd/clippet/main.go`), so the
// fallback below is for a hand-built Deps alone — and it is here because this
// field is a CONTRACT: a verifier that read an empty string would have nothing
// to look up, while `dev` is a release name it can refuse. /healthz reports the
// same field and prints it raw, which it may: nothing reads /healthz for a
// verdict.
func (h *handler) version() string {
	if h.d.Version == "" {
		return "dev"
	}
	return h.d.Version
}
