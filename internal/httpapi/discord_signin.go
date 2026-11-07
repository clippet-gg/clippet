package httpapi

import (
	"errors"
	"net/http"
	"slices"

	"clippet/internal/auth"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/ids"
)

// `/signin` (DESIGN.md §20 erratum 341; the owner's decision of 2026-10-19:
// the bot is the door). A member of a bound server runs it and is answered, privately,
// with a one-time link that signs their browser in to this instance.
//
// What it proves is Discord's own signature over the interaction — verified
// HERE, against the application's public key, exactly as `/upload` is (on a
// managed instance the forwarder relays the bytes and can forge none of them).
// It never asks the plane, and it never runs OAuth: the interaction already
// carries the member's id, name and roles, signed. Erratum 302's "a second key,
// never a door" is amended accordingly — the door is the signed interaction.
//
// The command only MINTS. The link's page (internal/web) renders on GET and
// consumes nothing, because Discord's crawler fetches what a bot says (erratum
// 316); its form's POST consumes the token and opens the account.
//
// The mode is REGISTRATION: `members` lets any member of a bound server in,
// `role:<id>` only a member carrying that role; every other value refuses the
// command before a token exists, so the link's route only ever sees tokens a
// mode that allows them minted.

// The `/signin` refusals, in full (the owner's sentences of 2026-10-19).
const (
	msgSigninOff  = "Discord sign-in is off on this Clippet."
	msgSigninRole = "This server limits sign-in to a role you do not have."
)

// suppressEmbedsFlag (1 << 2) keeps Discord from drawing a card under the
// link: the page is a sign-in button, not something to preview.
const suppressEmbedsFlag = 4

// signinReply is the reply's first line; the link follows on its own line,
// inside Discord's no-unfurl brackets.
func signinReply(host string) string {
	return "Sign in to " + host + " · this link works once, for five minutes."
}

// discordSignin is `/signin`: the server, the binding, the mode, the role,
// then one token and the private reply. Nothing about the member reaches the
// log — the access line says `discord=signin` and the verdict.
func (h *handler) discordSignin(w http.ResponseWriter, r *http.Request, in discordInteraction) {
	// A server, and one somebody connected: the other commands' order and
	// their refusals, word for word.
	if _, ok := h.guildBinding(w, r, in, "signin"); !ok {
		return
	}
	mode := h.registrationMode()
	if !mode.DiscordSignIn() {
		httpx.SetLogField(r, "signin", "off")
		writeEphemeral(w, msgSigninOff)
		return
	}
	m := in.Member
	if m == nil || m.User == nil || !isDiscordID(m.User.ID) {
		// A guild interaction always carries its member; a body without one
		// is not Discord's, and gets the generic line.
		httpx.SetLogField(r, "signin", "no_member")
		writeEphemeral(w, msgUnavailable)
		return
	}
	if role := mode.SignInRole(); role != "" && !slices.Contains(m.Roles, role) {
		httpx.SetLogField(r, "signin", "no_role")
		writeEphemeral(w, msgSigninRole)
		return
	}
	display := m.User.GlobalName
	if display == "" {
		display = m.User.Username
	}
	raw := ids.SigninToken()
	now := h.unixNow()
	err := h.d.DB.CreateSigninToken(r.Context(), db.SigninToken{
		Hash: auth.HashToken(raw), GuildID: in.GuildID, DiscordID: m.User.ID,
		Username: m.User.Username, DisplayName: display,
		CreatedAt: now, ExpiresAt: now + db.SigninTTLSeconds,
	})
	if err != nil {
		if !errors.Is(err, db.ErrConflict) {
			h.d.Log.Warn("discord bot: the sign-in link could not be stored", "err", err)
		}
		writeEphemeral(w, msgUnavailable)
		return
	}
	httpx.SetLogField(r, "signin", "link")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"type": callbackMessage,
		"data": map[string]any{
			"content":          signinReply(h.d.URLs.Host()) + "\n<" + h.d.URLs.Abs(auth.SigninLinkPrefix+raw) + ">",
			"flags":            ephemeralFlag | suppressEmbedsFlag,
			"allowed_mentions": map[string]any{"parse": []string{}},
		},
	})
}

// isDiscordID is the snowflake shape (15-25 digits), the rule `admin users
// link` holds an id to: the key of an account is never a value of another shape.
func isDiscordID(s string) bool {
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
