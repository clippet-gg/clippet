package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strings"

	"clippet/internal/auth"
	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/notify"
)

// "Sign in with Discord" on an instance (ROADMAP #44, DESIGN.md §20 erratum
// 302): three paths, four handlers, registered ONLY when the instance's own
// application is configured (DISCORD_LOGIN_CLIENT_ID + _SECRET). With it off
// the paths are the router's 404 like any other unknown route, /login shows no
// button, and nothing about the feature exists.
//
// The rule the whole file keeps: **a Discord login is a second key to an
// existing door, never a door.** The callback looks the proven Discord user id
// up in `users.discord_user_id` and issues a session for the account it finds
// — the SAME session `POST /api/auth/login` issues, same row, same cookie —
// and creates nothing when it finds none, under any REGISTRATION value. The
// link is written by exactly two explicit hands: the signed-in owner, from
// Settings, with their password (erratum 108's reason: a session says "this
// browser was logged in once", a permanent login key needs "this is still the
// owner"); and the operator, with `clippet admin users link`. No global users,
// nothing cross-instance, no guild anything.
//
// The flow's safety is `state` + PKCE carried in ONE browser-side cookie (the
// flow cookie, internal/auth): a 32-byte nonce that IS the PKCE code_verifier,
// from which both the challenge and the state are derived by domain-separated
// hashes. Both are public in the authorize URL; both bind to a secret only this
// browser holds. The cookie is single-use — the callback queues its clearing
// before anything else — so a replayed callback URL is refused at the cookie
// step with no outbound call.
//
// The cookie is also AUTHENTICATED (erratum 302(h)): its payload carries an
// HMAC-SHA256 under a key derived from the client secret, and decodeFlow
// verifies it before reading a byte of the payload. Without that, the cookie
// was a string the caller sends — a thief holding a session cookie could
// hand-build a link-mode cookie for that account and, with a code minted on
// their OWN Discord consent, link their Discord to the victim's account
// without ever supplying the password: a second key that survives the password
// change and "Sign out everywhere else", the two things a stolen laptop is
// answered with. The key is the client secret's derivative rather than a
// per-process random so that a restart keeps a flow in flight, and so that a
// second replica with the same configuration answers the callback (the
// harness's own probe boots one). There is still no server-side table.

// The three paths. `api` is a reserved first segment (erratum 248).
const (
	discordLoginStartPath    = "/api/auth/discord/start"
	discordLoginPath         = "/api/auth/discord"
	discordLoginCallbackPath = "/api/auth/discord/callback"
)

// DiscordLoginCallbackPath is the redirect URI's path — what the operator
// pastes into the Developer Portal, appended to PUBLIC_BASE_URL. For the CLI,
// the boot line and the docs.
func DiscordLoginCallbackPath() string { return discordLoginCallbackPath }

// DiscordLoginStartPath is the anchor /login carries, for the pages and the
// docs.
func DiscordLoginStartPath() string { return discordLoginStartPath }

// IdentityProvider is what the callback hands a code to (internal/notify's
// *Login in production, a fake in tests): the token exchange and the one
// identity read, or one error.
type IdentityProvider interface {
	Identify(ctx context.Context, code, verifier, redirectURI string) (notify.DiscordIdentity, error)
}

// The flow cookie's two modes.
const (
	flowLogin = "login" // anonymous → a session for the linked account
	flowLink  = "link"  // a signed-in owner → this Discord user may open their account
)

// flowStatePrefix domain-separates the state from the PKCE challenge: both
// are hashes of the same nonce, and a state that equalled the challenge would
// let the authorize URL's own query answer the callback's state check.
const flowStatePrefix = "clippet-discord-state."

// maxCodeBytes bounds the `code` the callback accepts. Discord's are 30
// characters; the ceiling exists so a hostile query cannot make the exchange
// carry a megabyte.
const maxCodeBytes = 512

// flow is the decoded flow cookie.
type flow struct {
	nonce  string // 43 base64url characters: the PKCE code_verifier
	mode   string // flowLogin | flowLink
	userID string // link mode: the account the password was checked for
	next   string // login mode: where to land, held to safeNext
}

// newNonce mints the flow's secret: 32 random bytes as 43 base64url
// characters, which is exactly RFC 7636's code_verifier charset and inside its
// 43-128 length.
func newNonce() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("httpapi: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// flowState derives the OAuth `state` from the nonce.
func flowState(nonce string) string {
	sum := sha256.Sum256([]byte(flowStatePrefix + nonce))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// flowChallenge derives the PKCE S256 code_challenge from the verifier.
func flowChallenge(nonce string) string {
	sum := sha256.Sum256([]byte(nonce))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// flowKeyPrefix domain-separates the cookie's MAC key from the client secret
// it is derived from: the secret itself goes to exactly one place (the token
// endpoint), and a key that is a hash of it under a label never does.
const flowKeyPrefix = "clippet-discord-flow-mac."

// flowKey derives the cookie's MAC key from the instance's client secret. It
// is the one value a session thief does not hold (a session cookie says
// nothing about `.env`), it is the same on every replica of one configuration
// and across a restart, and it is never written anywhere. With the login off
// no cookie is ever minted or read, so the empty-secret key is never used.
func flowKey(login config.DiscordLogin) []byte {
	sum := sha256.Sum256([]byte(flowKeyPrefix + login.Secret()))
	return sum[:]
}

// flowMAC is the tag over `nonce "." payload` — the nonce is inside it, so a
// payload cannot be moved under another nonce's state.
func flowMAC(key []byte, signed string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(signed))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// encodeFlow renders the cookie value: `nonce "." base64url(mode NUL userID
// NUL next) "." base64url(mac)`. Every byte is a cookie-value byte, and the
// three segments carry no dot of their own.
func encodeFlow(key []byte, f flow) string {
	payload := f.mode + "\x00" + f.userID + "\x00" + f.next
	signed := f.nonce + "." + base64.RawURLEncoding.EncodeToString([]byte(payload))
	return signed + "." + flowMAC(key, signed)
}

// decodeFlow parses a cookie value, refusing anything that is not exactly the
// shape encodeFlow writes. The MAC is verified FIRST, in constant time, before
// any field is read: a value this instance did not mint is no flow at all,
// whatever it says.
func decodeFlow(key []byte, v string) (flow, bool) {
	segs := strings.Split(v, ".")
	if len(segs) != 3 {
		return flow{}, false
	}
	nonce, rest, mac := segs[0], segs[1], segs[2]
	if len(nonce) != 43 || !isBase64URL(nonce) {
		return flow{}, false
	}
	if !hmac.Equal([]byte(mac), []byte(flowMAC(key, nonce+"."+rest))) {
		return flow{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return flow{}, false
	}
	parts := strings.Split(string(raw), "\x00")
	if len(parts) != 3 {
		return flow{}, false
	}
	f := flow{nonce: nonce, mode: parts[0], userID: parts[1], next: safeNext(parts[2])}
	switch f.mode {
	case flowLogin:
		if f.userID != "" {
			return flow{}, false
		}
	case flowLink:
		if f.userID == "" {
			return flow{}, false
		}
	default:
		return flow{}, false
	}
	return f, true
}

func isBase64URL(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// safeNext keeps a post-login redirect target on this origin — internal/web's
// rule (§20 erratum 52), spelled here because httpapi cannot import web; a
// table test pins the two against each other. A single-slash-rooted path, no
// control byte, no backslash, and its own path.Clean normal form.
func safeNext(v string) string {
	if !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") {
		return "/"
	}
	for _, c := range v {
		if c < 0x20 || c == 0x7f || c == '\\' {
			return "/"
		}
	}
	p := v
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	clean := path.Clean(p)
	if strings.HasSuffix(p, "/") && !strings.HasSuffix(clean, "/") {
		clean += "/"
	}
	if clean != p {
		return "/"
	}
	return v
}

// discordLoginConfigured is the routing switch: the instance's own
// application, both variables.
func (h *handler) discordLoginConfigured() bool {
	return h.d.Cfg.DiscordLogin.Configured()
}

// discordRedirectURI is the ONE redirect URI, fixed: PUBLIC_BASE_URL plus the
// callback path, compared byte-exact by Discord.
func (h *handler) discordRedirectURI() string {
	return h.d.URLs.Abs(discordLoginCallbackPath)
}

// discordAuthorizeURL builds Discord's authorize URL for a flow: the code
// grant, `identify` and nothing else (no email, no guilds — ever), the state
// and the S256 challenge. Built from the allowlisted host, never a literal.
func (h *handler) discordAuthorizeURL(nonce string) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {h.d.Cfg.DiscordLogin.ClientID()},
		"redirect_uri":          {h.discordRedirectURI()},
		"scope":                 {"identify"},
		"state":                 {flowState(nonce)},
		"code_challenge":        {flowChallenge(nonce)},
		"code_challenge_method": {"S256"},
	}
	u := url.URL{Scheme: "https", Host: config.DiscordAPIHost(), Path: "/oauth2/authorize", RawQuery: q.Encode()}
	return u.String()
}

// mintFlow writes the flow cookie and returns the authorize URL to send the
// browser to.
func (h *handler) mintFlow(w http.ResponseWriter, f flow) string {
	f.nonce = newNonce()
	auth.SetFlowCookie(w, h.d.Cfg, h.d.URLs.IsHTTPS(), encodeFlow(flowKey(h.d.Cfg.DiscordLogin), f))
	return h.discordAuthorizeURL(f.nonce)
}

// noStore marks a navigation answer uncacheable: every one of these depends
// on a cookie.
func noStore(w http.ResponseWriter) { w.Header().Set("Cache-Control", "no-store") }

// discordStart is GET /api/auth/discord/start?next= — the anchor on /login.
// Anonymous by design: a plain link, so it is reachable without a form (CSP
// `form-action 'self'` is enforced on a form's redirect chain). Signed in →
// straight to next. A cross-site navigation may not launch a flow: the same
// header the CSRF rule reads, and a third party gains nothing from a flow in
// somebody else's browser anyway (the state cookie is theirs, not the
// attacker's), but the refusal costs nothing.
func (h *handler) discordStart(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	next := safeNext(r.URL.Query().Get("next"))
	if p, ok := auth.FromContext(r.Context()); ok && p.Via == auth.ViaSession {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		httpx.SetLogField(r, "discord_login", "cross_site")
		h.logClientIP(r)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	target := h.mintFlow(w, flow{mode: flowLogin, next: next})
	http.Redirect(w, r, target, http.StatusFound)
}

type discordLinkRequest struct {
	CurrentPassword string `json:"current_password"`
}

// discordLinkResponse is where app.js navigates: a script navigation, not a
// form submission, for the CSP reason above.
type discordLinkResponse struct {
	URL string `json:"url"`
}

// discordLink is POST /api/auth/discord: the signed-in owner re-enters their
// password and gets the authorize URL for a LINK flow bound to their account.
// Session-only, CSRF-guarded, under the `auth` limiter (a bcrypt compare —
// erratum 108's argument). The password is checked HERE, when the cookie is
// minted; the cookie's MAC (which only this instance's secret can produce, and
// which the callback verifies before reading the bound account) is then the
// proof it was — a hand-built link cookie is refused as no flow.
func (h *handler) discordLink(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req discordLinkRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.CurrentPassword == "" {
		writeInvalid(w, "current_password is required")
		return
	}
	// The row by id, not the Principal's copy: the session join does not
	// carry the link fields, and the answer must be about the row as it is
	// now.
	u, err := h.d.DB.GetUserByID(r.Context(), p.User.ID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	case err != nil:
		writeDBError(w, r, err)
		return
	}
	if u.DiscordID != nil {
		httpx.WriteError(w, http.StatusConflict, "discord_linked", "this account is already linked to a Discord account; unlink it first")
		return
	}
	if !auth.CheckPassword(u.PasswordHash, req.CurrentPassword) {
		httpx.SetLogField(r, "discord_link", "bad_password")
		h.logClientIP(r)
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_credentials", "the current password is wrong")
		return
	}
	target := h.mintFlow(w, flow{mode: flowLink, userID: u.ID, next: "/settings"})
	httpx.WriteJSON(w, http.StatusOK, discordLinkResponse{URL: target})
}

// discordCallback is GET /api/auth/discord/callback?code&state[&error] — a
// browser navigation from Discord, never JSON. The checks run in the order the
// file comment gives; each refusal logs a one-word verdict and never a value,
// and lands on the page of the flow's mode with one closed-map reason.
func (h *handler) discordCallback(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	// 1. The flow is single-use: its clearing is queued before anything else,
	//    whatever follows.
	auth.ClearFlowCookie(w, h.d.Cfg, h.d.URLs.IsHTTPS())
	h.logClientIP(r)
	refuse := func(verdict, target string) {
		httpx.SetLogField(r, "discord_login", verdict)
		http.Redirect(w, r, target, http.StatusSeeOther)
	}
	// 2. No cookie, or not the shape this binary writes, or a MAC this
	//    instance's secret did not produce (302(h)): a forged or tampered
	//    cookie is no flow, whichever mode it claims.
	c, err := r.Cookie(auth.FlowCookieName(h.d.Cfg))
	if err != nil || c.Value == "" {
		refuse("no_flow", "/login?discord=expired")
		return
	}
	f, ok := decodeFlow(flowKey(h.d.Cfg.DiscordLogin), c.Value)
	if !ok {
		refuse("no_flow", "/login?discord=expired")
		return
	}
	page := "/login"
	if f.mode == flowLink {
		page = "/settings"
	}
	q := r.URL.Query()
	// 3. The state must be THIS cookie's, in constant time. The page does not
	//    distinguish a bad state from an expired flow — a stranger learns
	//    nothing from the difference.
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(flowState(f.nonce))) != 1 {
		refuse("bad_state", page+"?discord=expired")
		return
	}
	// 4. Discord's own refusal (the person pressed Cancel).
	if q.Get("error") != "" {
		refuse("denied", page+"?discord=denied")
		return
	}
	// 5. A code, of a plausible size.
	code := q.Get("code")
	if code == "" || len(code) > maxCodeBytes {
		refuse("bad_state", page+"?discord=expired")
		return
	}
	// 6. Link mode needs the SAME session the password was checked in: a
	//    callback in another browser, or after a logout, links nothing.
	if f.mode == flowLink {
		p, ok := auth.FromContext(r.Context())
		if !ok || p.Via != auth.ViaSession || p.User.ID != f.userID {
			refuse("link_session", "/settings")
			return
		}
	}
	// 7. The exchange. The nonce IS the verifier; the redirect URI is the
	//    fixed one the code was minted for.
	if h.d.Login == nil {
		refuse("exchange_failed", page+"?discord=failed")
		return
	}
	id, err := h.d.Login.Identify(r.Context(), code, f.nonce, h.discordRedirectURI())
	if err != nil {
		// Already scrubbed of the secret by internal/notify, and it names no
		// URL, code or token. The app log gets the cause; the access line the
		// verdict.
		h.d.Log.Warn("discord login: the exchange failed", "err", err.Error())
		refuse("exchange_failed", page+"?discord=failed")
		return
	}
	// 8. The account rule.
	switch f.mode {
	case flowLogin:
		u, err := h.d.DB.GetUserByDiscordID(r.Context(), id.ID)
		switch {
		case errors.Is(err, db.ErrNotFound):
			// No row written, ever: a Discord account is not an account here.
			refuse("unlinked", "/login?discord=unlinked")
			return
		case err != nil:
			writeDBError(w, r, err)
			return
		}
		if !h.issueSession(w, r, u) {
			return
		}
		// Display only; a failed touch must not fail a login.
		_ = h.d.DB.TouchDiscordUsername(r.Context(), u.ID, id.Username)
		httpx.SetLogField(r, "login", "discord")
		httpx.SetLogField(r, "user", u.Username)
		http.Redirect(w, r, f.next, http.StatusSeeOther)
	case flowLink:
		p, _ := auth.FromContext(r.Context())
		err := h.d.DB.LinkDiscord(r.Context(), f.userID, id.ID, id.Username, h.unixNow())
		switch {
		case errors.Is(err, db.ErrConflict):
			// Already linked elsewhere, or this account gained a link since
			// the cookie was minted — one answer, naming no other account.
			refuse("taken", "/settings?discord=taken")
			return
		case err != nil:
			writeDBError(w, r, err)
			return
		}
		// The operator's record of the mapping; `admin users` prints it too.
		httpx.SetLogField(r, "discord_linked", p.User.Username)
		httpx.SetLogField(r, "discord_id", id.ID)
		http.Redirect(w, r, "/settings?discord=linked", http.StatusSeeOther)
	}
}

// discordUnlink is DELETE /api/auth/discord: session + CSRF, no password — it
// removes a capability rather than adding one. 204, or 404 not_linked.
// Sessions are untouched: unlinking removes a way in, not the browsers in.
func (h *handler) discordUnlink(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	had, err := h.d.DB.UnlinkDiscord(r.Context(), p.User.ID)
	if err != nil {
		writeDBError(w, r, err)
		return
	}
	if !had {
		httpx.WriteError(w, http.StatusNotFound, "not_linked", "this account is not linked to a Discord account")
		return
	}
	httpx.SetLogField(r, "discord_unlinked", p.User.Username)
	w.WriteHeader(http.StatusNoContent)
}
