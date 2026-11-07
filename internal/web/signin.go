package web

import (
	"errors"
	"net/http"
	"strings"

	"clippet/internal/auth"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/ids"
)

// The bot's one-time login link (DESIGN.md §20 erratum 341): the page the
// private `/signin` reply links to, at auth.SigninLinkPrefix + the raw token.
//
//   - GET (and HEAD) RENDERS and changes nothing. Discord's crawler fetches the
//     links a bot sends (erratum 316 relies on it), so a GET that signed in
//     would hand the session to the crawler and spend the link before the
//     member ever clicked it. The page names the instance and the Discord name
//     the link was minted for, and holds one button.
//   - POST (the button) CONSUMES the token — one transaction, so of two POSTs
//     exactly one wins — and opens the account whose `discord_user_id` is the
//     token's member: the same session row and the same cookie a password login
//     issues (auth.CreateSession + auth.SetSessionCookie, httpapi's
//     issueSession). An unlinked member gets a new account named from their
//     Discord username, linked by id in the same row, with no password.
//
// The route exists only while the door is open — the bot configured and
// REGISTRATION=members|role:<id> — and is the 404 page otherwise, so a token
// minted before a restart into another mode opens nothing. An unknown, used or
// expired token lands on /login with one line saying so. Nothing about the
// token reaches a log: httpx.LogPath writes this path as `{token}`.

// signinNotice is the loginNotices key a dead link lands on /login with.
const signinNotice = "link"

// signinPage is the page's data.
type signinPage struct {
	page
	Action string // the form's own path (this page's)
	Next   string
	Host   string
	Name   string // the Discord name the link was minted for
}

// signinDoor reports whether `/signin` may let anyone in on this process: the
// bot configured and a Discord REGISTRATION mode. It is the one switch for the
// route, the /login line and the dead-link notice.
func (s *server) signinDoor() bool {
	return s.d.Cfg.DiscordBot.Configured() && s.d.Cfg.Registration.DiscordSignIn()
}

// signinLink is GET|HEAD|POST /login/discord/{token}.
func (s *server) signinLink(w http.ResponseWriter, r *http.Request) {
	if !s.signinDoor() {
		s.notFound(w, r)
		return
	}
	if !s.resolved(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.signinShow(w, r)
	case http.MethodPost:
		s.signinTake(w, r)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// signinDead sends the browser to /login with the one line a dead link earns.
func signinDead(w http.ResponseWriter, r *http.Request, verdict string) {
	httpx.SetLogField(r, "signin", verdict)
	redirectPrivate(w, r, "/login?discord="+signinNotice)
}

// signinShow renders the page for a live link and reads nothing but its row.
func (s *server) signinShow(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("token")
	if !ids.IsSigninToken(raw) {
		signinDead(w, r, "bad_shape")
		return
	}
	ctx, cancel := s.ssrContext(r)
	defer cancel()
	t, err := s.d.DB.PeekSigninToken(ctx, auth.HashToken(raw), s.d.Now().Unix())
	switch {
	case errors.Is(err, db.ErrNotFound):
		signinDead(w, r, "dead")
		return
	case err != nil:
		s.backendFailure(w, r, err)
		return
	}
	p := s.basePage(r, "Sign in · "+productName)
	p.NoIndex = true
	// The theme glyph's hidden `next` would carry the token back through a
	// second form; the page sends it nowhere but to itself.
	p.Path = "/"
	data := signinPage{
		page:   p,
		Action: auth.SigninLinkPrefix + raw,
		Next:   safeNext(r.URL.Query().Get("next")),
		Host:   s.d.URLs.Host(),
		Name:   t.DisplayName,
	}
	setPrivate(w)
	// The token is in this page's own address: no Referer to any other origin.
	// `same-origin` and not `no-referrer`, which would make a browser send the
	// form's POST with `Origin: null` — and the cross-site check refuse it.
	w.Header().Set("Referrer-Policy", "same-origin")
	s.render(w, r, "signin", http.StatusOK, data)
}

// signinTake is the button: refuse another site's POST, consume the token,
// find or make the account, issue the session, land on `next`.
func (s *server) signinTake(w http.ResponseWriter, r *http.Request) {
	// A forged POST would sign the victim's browser into the forger's account
	// (the forger can mint a link for themselves): the CSRF rule's two headers,
	// applied with no session to compare, because this POST makes one.
	if auth.CrossSite(r, s.d.URLs.Origin()) {
		httpx.SetLogField(r, "csrf", "rejected")
		http.Error(w, "cross-site request rejected", http.StatusForbidden)
		return
	}
	raw := r.PathValue("token")
	if !ids.IsSigninToken(raw) {
		signinDead(w, r, "bad_shape")
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	now := s.d.Now()
	t, err := s.d.DB.ConsumeSigninToken(ctx, auth.HashToken(raw), now.Unix())
	switch {
	case errors.Is(err, db.ErrNotFound):
		signinDead(w, r, "dead")
		return
	case err != nil:
		s.backendFailure(w, r, err)
		return
	}
	// The server the link was minted in must still be connected: an operator
	// who unbinds it inside the five minutes has said no.
	if _, err := s.d.DB.GuildBinding(ctx, t.GuildID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			signinDead(w, r, "unbound")
			return
		}
		s.backendFailure(w, r, err)
		return
	}
	u, created, err := s.signinAccount(r, t, now.Unix())
	if err != nil {
		s.backendFailure(w, r, err)
		return
	}
	cookie, ttl, err := auth.CreateSession(ctx, s.d.DB, s.d.Cfg, u.ID, r.UserAgent(), now)
	if err != nil {
		s.backendFailure(w, r, err)
		return
	}
	auth.SetSessionCookie(w, s.d.Cfg, s.d.URLs.IsHTTPS(), cookie, ttl)
	// Display only; a failed touch must not fail a sign-in.
	_ = s.d.DB.TouchDiscordUsername(ctx, u.ID, t.Username)
	httpx.SetLogField(r, "login", "discord_signin")
	httpx.SetLogField(r, "user", u.Username)
	if created {
		httpx.SetLogField(r, "registered", u.Username)
	}
	redirectPrivate(w, r, safeNext(r.PostForm.Get("next")))
}

// signinAccount is the account rule: the one linked to the token's Discord id,
// or — the door being open, which signinLink already checked — a new one made
// for it. A username that happens to match is never a link.
func (s *server) signinAccount(r *http.Request, t db.SigninToken, now int64) (db.User, bool, error) {
	ctx := r.Context()
	u, err := s.d.DB.GetUserByDiscordID(ctx, t.DiscordID)
	if err == nil {
		return u, false, nil
	}
	if !errors.Is(err, db.ErrNotFound) {
		return db.User{}, false, err
	}
	u, err = s.d.DB.CreateDiscordUser(ctx, ids.New(), signinUsername(t.Username), t.DiscordID, t.Username, now)
	if errors.Is(err, db.ErrConflict) {
		// Another sign-in of the same member made the account a moment ago.
		u, err = s.d.DB.GetUserByDiscordID(ctx, t.DiscordID)
		return u, false, err
	}
	return u, err == nil, err
}

// signinUsername turns a Discord username into one §14 accepts: lowercase
// ASCII letters, digits and `_` — every other character (Discord allows `.`)
// becomes `_` — at most 32 bytes, at least 3 (padded with `_`). The collision
// suffix is the database's (db.CreateDiscordUser).
func signinUsername(discord string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(discord) {
		if b.Len() == 32 {
			break
		}
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	for b.Len() < 3 {
		b.WriteByte('_')
	}
	return b.String()
}
