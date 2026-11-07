package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"clippet/internal/auth"
	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/httpx"
)

// Per-account settings and the owner's own tally (ROADMAP #22 and #23, §20
// errata 155-156). Three rules shape both routes:
//
//  1. **Session only.** They are owner routes like every other one (§14
//     erratum 19): a leaked capture token can upload, and that is all it can
//     do. It certainly cannot read how much an account has stored or change
//     what its future uploads default to.
//  2. **They answer for the CALLER and there is no route that answers for
//     anybody else.** `/api/auth/me/stats` has no `{user}` form and never
//     will: "private view counts are the ceiling" is the anti-feature line,
//     and an aggregate of somebody else's clips is over it.
//  3. **A default is not a permission.** Nothing here can make an upload do
//     something an explicit field did not ask for: the resolution order is
//     FIELD SENT > USER DEFAULT > instance default, in that order, in one
//     place (§20 erratum 155).

// settingsDTO is the account-settings wire shape.
//
// DefaultTTLDays is a POINTER with a meaningful null: `null` is "no
// preference — follow whatever DEFAULT_CLIP_TTL is", while `0` is "this
// account has chosen keep-forever". Those are different answers on an
// instance whose DEFAULT_CLIP_TTL is 7d, so they are different values on the
// wire. InstanceDefaultTTLDays says what the null case resolves to right now,
// so a settings page can label the option honestly ("server default · 7 days")
// without a second request or a guess.
//
// DefaultLinkStyle (ROADMAP #35, §20 erratum 250) is the opposite shape and
// deliberately so: two states, no null, no instance twin. Migration 0015's
// column is NOT NULL DEFAULT 'compact', there is no `DEFAULT_LINK_STYLE` for a
// null to defer to, and "no preference" and "compact" would be the same answer
// if there were — so a nullable field here would only invite a client to send
// a null that means nothing.
type settingsDTO struct {
	DefaultTTLDays         *int   `json:"default_ttl_days"`
	DefaultUnlisted        bool   `json:"default_unlisted"`
	DefaultLinkStyle       string `json:"default_link_style"`
	InstanceDefaultTTLDays int    `json:"instance_default_ttl_days"`
}

// settingsPatchRequest is PATCH's body: every field optional, an absent key
// meaning "leave it alone", exactly as PATCH /api/clips/{id} works.
type settingsPatchRequest struct {
	DefaultTTLDays   settingsTTL `json:"default_ttl_days"`
	DefaultUnlisted  *bool       `json:"default_unlisted"`
	DefaultLinkStyle *string     `json:"default_link_style"`
}

// settingsTTL decodes a field with THREE meanings, which is one more than
// dto.go's ttlDays can carry: absent ("leave the current preference alone"),
// null ("clear it — follow the instance") and an integer ("this many days").
// ttlDays folds null into absent because on an upload those two ARE the same
// request; here they are opposites, so this type exists rather than the other
// one being changed under its own callers.
type settingsTTL struct {
	set     bool
	null    bool
	invalid bool
	days    int
}

func (t *settingsTTL) UnmarshalJSON(b []byte) error {
	t.set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		t.null = true
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		// A string or an object is a bad TTL, not a bad body: it is answered
		// 400 invalid_ttl like every other illegal value of this field, rather
		// than as a decode error naming a Go type.
		t.invalid = true
		return nil
	}
	t.days = n
	return nil
}

// instanceDefaultTTLDays renders DEFAULT_CLIP_TTL in the wire unit (0 = never).
func instanceDefaultTTLDays(secs *int64) int {
	if secs == nil {
		return 0
	}
	return int(*secs / 86400)
}

func toSettingsDTO(d db.UserDefaults, cfg config.Config) settingsDTO {
	return settingsDTO{
		DefaultTTLDays:         d.TTLDays,
		DefaultUnlisted:        d.Unlisted,
		DefaultLinkStyle:       d.LinkStyleOr(),
		InstanceDefaultTTLDays: instanceDefaultTTLDays(cfg.DefaultTTLSeconds),
	}
}

// getSettings is GET /api/auth/me/settings.
func (h *handler) getSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := h.sessionPrincipal(w, r)
	if !ok {
		return
	}
	def, err := h.d.DB.GetUserDefaults(r.Context(), p.User.ID)
	if err != nil {
		writeDBError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toSettingsDTO(def, h.d.Cfg))
}

// patchSettings is PATCH /api/auth/me/settings: session, CSRF, JSON.
func (h *handler) patchSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := h.sessionPrincipal(w, r)
	if !ok {
		return
	}
	var req settingsPatchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	def, err := h.changeDefaults(r.Context(), p.User.ID, defaultsChange{
		TTL: req.DefaultTTLDays, Unlisted: req.DefaultUnlisted, LinkStyle: req.DefaultLinkStyle,
	})
	switch {
	case errors.Is(err, errDefaultsTTL):
		writeInvalidTTL(w)
		return
	case errors.Is(err, errDefaultsLinkStyle):
		writeInvalidLinkStyle(w)
		return
	case err != nil:
		writeDBError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toSettingsDTO(def, h.d.Cfg))
}

// defaultsChange is one change to an account's upload defaults, every field
// optional: the settings page's PATCH and the Discord `/settings` modal (§20
// erratum 318) both state theirs in this shape and both go through
// changeDefaults — ONE writer for the three columns, with ONE set of closed-set
// checks, so the two doors cannot drift into two rules.
type defaultsChange struct {
	TTL       settingsTTL
	Unlisted  *bool
	LinkStyle *string
}

// The two refusals changeDefaults can give that are the REQUEST's fault. Each
// door words them its own way: the PATCH as its 400s, the modal as one
// sentence.
var (
	errDefaultsTTL       = errors.New("default_ttl_days is not one of the legal values")
	errDefaultsLinkStyle = errors.New("default_link_style is not one of the legal values")
)

// changeDefaults reads the account's defaults, applies the change through the
// closed sets an upload is held to, and writes the result. Anything it returns
// that is not one of the two refusals above is the database's.
func (h *handler) changeDefaults(ctx context.Context, userID string, c defaultsChange) (db.UserDefaults, error) {
	def, err := h.d.DB.GetUserDefaults(ctx, userID)
	if err != nil {
		return db.UserDefaults{}, err
	}
	if c.TTL.set {
		switch {
		case c.TTL.invalid:
			return db.UserDefaults{}, errDefaultsTTL
		case c.TTL.null:
			def.TTLDays = nil
		default:
			// The SAME closed set an upload's ttl_days is held to, from the
			// same function: a preference that could hold 5 would be a
			// preference that made every upload a 400.
			n := c.TTL.days
			if _, terr := config.ParseTTLDays(&n, nil); terr != nil {
				return db.UserDefaults{}, errDefaultsTTL
			}
			def.TTLDays = &n
		}
	}
	if c.Unlisted != nil {
		def.Unlisted = *c.Unlisted
	}
	if c.LinkStyle != nil {
		// The SAME closed set an upload's `link_style` is held to, from the
		// same function (§20 erratum 250): a preference that could hold `words`
		// would be a preference that made every upload a 400 — or, worse, one
		// silently ignored at mint time. A JSON `null` reads as ABSENT here,
		// exactly as it does for `default_unlisted`: unlike the TTL there is no
		// third state for it to mean, so "leave it alone" is the only sense it
		// could have.
		if !db.ValidLinkStyle(*c.LinkStyle) {
			return db.UserDefaults{}, errDefaultsLinkStyle
		}
		def.LinkStyle = *c.LinkStyle
	}
	if err := h.d.DB.SetUserDefaults(ctx, userID, def); err != nil {
		return db.UserDefaults{}, err
	}
	return def, nil
}

// accountStatsDTO is GET /api/auth/me/stats: the caller's own tally.
//
// It is deliberately dull. There is no rank, no comparison, no "top clip", no
// history and no other account's number anywhere — those would be the social
// layer this product does not have. What it answers is the question an owner
// of a self-hosted box actually asks: how much is in here, how much of it is
// about to expire, and how much space am I paying for.
type accountStatsDTO struct {
	Clips           int   `json:"clips"`
	Ready           int   `json:"ready"`
	Collections     int   `json:"collections"`
	BytesSource     int64 `json:"bytes_source"`
	BytesRenditions int64 `json:"bytes_renditions"`
	BytesTotal      int64 `json:"bytes_total"`
	ViewsTotal      int64 `json:"views_total"`
	Expiring7d      int   `json:"expiring_7d"`
}

// getAccountStats is GET /api/auth/me/stats (session only).
func (h *handler) getAccountStats(w http.ResponseWriter, r *http.Request) {
	p, ok := h.sessionPrincipal(w, r)
	if !ok {
		return
	}
	st, err := h.d.DB.AccountStatsOf(r.Context(), p.User.ID, h.unixNow())
	if err != nil {
		writeDBError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, accountStatsDTO{
		Clips: st.Clips, Ready: st.Ready, Collections: st.CollectionsCount,
		BytesSource: st.BytesSource, BytesRenditions: st.BytesRenditions,
		BytesTotal: st.BytesSource + st.BytesRenditions,
		ViewsTotal: st.ViewsTotal, Expiring7d: st.Expiring7d,
	})
}

// sessionPrincipal is the 401 guard both routes share. auth.RequireSession
// has already refused a token and an anonymous caller, so this is the
// unreachable-composition case spelled out rather than assumed.
func (h *handler) sessionPrincipal(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return auth.Principal{}, false
	}
	if h.d.DB == nil {
		writeDBError(w, r, errNoDB)
		return auth.Principal{}, false
	}
	return p, true
}

// userDefaults resolves the caller's upload defaults, with the instance's own
// as the answer for a user whose row has gone (a deleted account whose
// request is still in flight). It never fails an upload: the defaults are a
// convenience, and a database that cannot answer this question is about to
// fail the upload's own transaction anyway with a message about the upload.
func (h *handler) userDefaults(ctx context.Context, userID string) db.UserDefaults {
	if h.d.DB == nil || userID == "" {
		return db.UserDefaults{}
	}
	def, err := h.d.DB.GetUserDefaults(ctx, userID)
	if err != nil {
		if !errors.Is(err, db.ErrNotFound) {
			h.d.Log.Warn("upload: read account defaults; using the instance defaults", "user", userID, "err", err)
		}
		return db.UserDefaults{}
	}
	return def
}
