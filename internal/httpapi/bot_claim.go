package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/httpx"
)

// The origin proof for the OFFICIAL Clippet bot: GET /api/bot/claim?guild_id=…
// (ROADMAP #34 lane A, DESIGN.md §20 erratum 279, DEPLOY §30.8).
//
// The official bot's forwarder will POST a signed interaction to whatever
// origin a server's admin names when they connect that server — so before it
// writes that route, its claim page asks the named origin ONE question: "did
// the operator of this box consent to this guild?" This route is the answer.
// It is 200 only after `clippet admin bot bind <guild-id> <username>` was run
// on this box, which needs a shell here — the one thing a stranger with a
// throwaway Discord server cannot do. Without it, the forwarder would be a
// request relay aimed at any address a claimant typed.
//
// What it discloses: ONE boolean about a guild id the asker must already
// know, on an origin the asker must already know — a fact anyone in that
// server learns by typing `/clip` and reading the link's host — plus the
// application id this box is configured for, which is public (it is in
// every interaction) and lets the claim page refuse "your box is set up for
// a different application" at claim time rather than at the first `/clip`.
// Never the bound username, never the user id, never a count, never a list.
// Since §20 erratum 342 it also says which consent it is, `"scope"`: `server`
// (every member) or `person` (one member, below).
// Since 2026-10-12 (§20 erratum 318 amended) a bound answer also carries the
// bound account's three link defaults, for the official bot's `/settings`
// panel (claimDefaults below) — preferences, never content or identity.
//
// It is INBOUND only: no new egress, no new call site, nothing for the §15
// tripwire. It lives inside the same `if h.discordConfigured()` block as the
// interactions route, so with the bot off it does not exist. Anonymous, under
// its OWN `claim` rate bucket (30/min per IP — never the interactions route's
// `bot` bucket, which the official bot's `/clip`s share behind a fixed proxy
// sentinel and which a stranger's GETs must not be able to drain), and it
// never caches: the answer changes the moment the operator binds or unbinds.
//
// A PERSON binding (§20 erratum 342, migration 0024) is the box's consent for
// ONE member of the server, the bound account's linked Discord user: the plane
// asks with `&user_id=<the member>` before it routes that member's commands
// here, and the box answers 200 only when the id is that account's. A guild-only
// ask about a person binding is a 404 — so a server's admin who names a friend's
// box for the whole server is refused by the box itself, whatever the plane
// does. The answer says which consent it is (`"scope"`), so the plane can refuse
// a box too old to know the difference. A person answer carries no `defaults`:
// the server's `/settings` panel reads the server route's box, never this one.
// The asked id is compared and dropped — never logged, never echoed.

// botClaimPath is the route. `api` is already a reserved first segment
// (internal/ids/reserved.go), so no id can ever collide with it.
const botClaimPath = "/api/bot/claim"

// BotClaimPath is that path, for the CLI and the docs.
func BotClaimPath() string { return botClaimPath }

// botClaim is GET /api/bot/claim?guild_id=<snowflake>[&user_id=<snowflake>].
func (h *handler) botClaim(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	guildID := strings.TrimSpace(q.Get("guild_id"))
	userID := strings.TrimSpace(q.Get("user_id"))
	w.Header().Set("Cache-Control", "no-store")
	if !isSnowflake(guildID) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_guild_id", "guild_id must be the server's id: 15-25 digits")
		return
	}
	// Present means asked: an empty or malformed user_id is a typo to name,
	// not a guild-only ask to answer.
	if q.Has("user_id") && !isSnowflake(userID) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_user_id", "user_id must be the member's id: 15-25 digits")
		return
	}
	if h.d.DB == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "unavailable", "not available right now")
		return
	}
	binding, err := h.d.DB.GuildBinding(r.Context(), guildID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// The honest "nobody consented here": a 404 with an EMPTY body,
			// so the answer is exactly one bit.
			httpx.SetLogField(r, "bot_claim", "unbound")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		h.d.Log.Warn("bot claim: guild lookup failed", "err", err)
		httpx.WriteError(w, http.StatusServiceUnavailable, "unavailable", "not available right now")
		return
	}
	if binding.Person() {
		// One member's consent: 200 only for the account's own linked id. No
		// id asked, another id, or an account unlinked since — the same empty
		// 404 an unbound guild answers, so the asker learns one bit.
		if userID == "" || binding.DiscordID == "" || userID != binding.DiscordID {
			httpx.SetLogField(r, "bot_claim", "not_person")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		httpx.SetLogField(r, "bot_claim", "person")
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"bound":  true,
			"app_id": h.d.Cfg.DiscordBot.AppID(),
			"scope":  db.GuildScopePerson,
		})
		return
	}
	httpx.SetLogField(r, "bot_claim", "bound")
	out := map[string]any{
		"bound":  true,
		"app_id": h.d.Cfg.DiscordBot.AppID(),
		"scope":  db.GuildScopeServer,
	}
	// The bound account's link defaults (§20 erratum 318, amended 2026-10-12):
	// the official bot's `/settings` panel prints them under "Link defaults".
	// A failed read costs the line, never the proof.
	if def, err := h.d.DB.GetUserDefaults(r.Context(), binding.UserID); err == nil {
		out["defaults"] = claimDefaults(def, h.d.Cfg.DefaultTTLSeconds)
	} else {
		h.d.Log.Warn("bot claim: read the bound account's defaults", "err", err)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// claimDefaults is the `defaults` object: the EFFECTIVE days a new clip is kept
// (the account's choice, or the instance's DEFAULT_CLIP_TTL when it has none;
// 0 = never), whether new clips are unlisted, and the link grammar. Three
// preferences, no content: the grammar shows in every `/clip` link the server
// sees, the term on every clip page; the unlisted default is the one value
// this adds. Never the account's name or id. A stored term no upload would
// apply (a hand-edited row outside the legal set) answers the instance's days,
// exactly as applyDefaultsCtx falls back — the line states the term in force.
func claimDefaults(def db.UserDefaults, instanceTTL *int64) map[string]any {
	days := instanceDefaultTTLDays(instanceTTL)
	if def.TTLDays != nil {
		if _, err := config.ParseTTLDays(def.TTLDays, nil); err == nil {
			days = *def.TTLDays
		}
	}
	return map[string]any{
		"ttl_days": days,
		"unlisted": def.Unlisted,
		"link":     def.LinkStyleOr(),
	}
}

// isSnowflake is the shape every Discord id has: 15-25 digits. A typo guard,
// not a security control — an id that passes and does not exist is simply
// unbound.
func isSnowflake(s string) bool {
	if len(s) < 15 || len(s) > 25 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
