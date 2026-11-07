package httpapi

import (
	"errors"
	"net/http"
	"regexp"
	"sync"
	"sync/atomic"

	"clippet/internal/auth"
	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/ids"
)

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9_]{3,32}$`)

const (
	minPasswordBytes = 8
	maxPasswordBytes = 72 // bcrypt truncation guard (§14)
)

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type userResponse struct {
	User UserDTO `json:"user"`
	Via  string  `json:"via,omitempty"`
}

// dummy is the bcrypt hash compared against when the username is unknown,
// so login timing does not reveal whether a user exists (§14). It is built
// once, lazily, at the same cost as a real hash.
var dummy struct {
	once sync.Once
	hash string
}

// dummyCompares counts unknown-user logins (asserted by tests).
var dummyCompares atomic.Int64

func dummyCompare(password string) {
	dummy.once.Do(func() {
		h, err := auth.HashPassword("clippet-no-such-user-placeholder")
		if err != nil {
			// Unreachable for a short constant; keep a syntactically valid
			// hash so CompareHashAndPassword still does its work.
			h = "$2a$12$C6UzMDM.H6dfI/f/IKcEeO5mOSWdMK1L6VXhOm/ND3F4nyHXvWk1O"
		}
		dummy.hash = h
	})
	dummyCompares.Add(1)
	auth.CheckPassword(dummy.hash, password)
}

func validPassword(pw string) bool {
	return len(pw) >= minPasswordBytes && len(pw) <= maxPasswordBytes
}

// registrationMode resolves REGISTRATION for this process. The zero value is
// `first`, so a Config built by hand behaves like a shipped one rather than
// like `open` (§20 erratum 106).
func (h *handler) registrationMode() config.RegistrationMode {
	if mode := h.d.Cfg.Registration; mode != "" {
		return mode
	}
	return config.RegistrationFirst
}

// registrationOpen answers the CHEAP half of REGISTRATION for this request
// (§20 erratum 106). `open` and `closed` are constants; `first` is open only
// while the users table is empty, which is one COUNT(*) on the smallest table
// in the schema.
//
// It is a fast refusal, not the guarantee: under `first` the door is actually
// held by `db.CreateFirstUser`, whose INSERT carries the emptiness test with
// it. This read exists so a closed instance answers 403 without burning a
// bcrypt hash first, and so the anonymous pages can draw (or not draw) a form.
//
// The count is deliberately NOT cached: the whole point of `first` is that it
// closes the instant an account exists, and a cached "empty" would keep the
// door open after the bootstrap account walked through it. A DB failure is
// reported, never guessed at — "sure, go ahead" on an unreadable users table
// is the one answer this must not give.
func (h *handler) registrationOpen(r *http.Request) (bool, error) {
	mode := h.registrationMode()
	if !mode.NeedsFirstUser() {
		return mode.AlwaysOpen(), nil
	}
	n, err := h.d.DB.CountUsers(r.Context())
	if err != nil {
		return false, err
	}
	return n == 0, nil
}

// logClientIP puts the RESOLVED client address on the access-log line of an
// authentication outcome — every registration and every refusal of one, and
// both ways a login can be rejected (ROADMAP #21, extended).
//
// The access log's own `remote` field is the raw socket address, which in the
// deployment shape this project documents is the reverse proxy and nothing
// else: DEPLOY §4 has the app behind a tunnel with TRUSTED_PROXIES set, so
// every refusal line would otherwise name the proxy and a scanner probing
// /register or /login would be unattributable. clientip.Resolve is the same
// address the auth rate limiter buckets on, so the log and the 429 agree
// about who the caller was.
func (h *handler) logClientIP(r *http.Request) {
	httpx.SetLogField(r, "client_ip", h.ipKey(r))
}

// register creates a user and starts a session (§8.1).
func (h *handler) register(w http.ResponseWriter, r *http.Request) {
	mode := h.registrationMode()
	open, err := h.registrationOpen(r)
	if err != nil {
		writeDBError(w, r, err)
		return
	}
	if !open {
		httpx.SetLogField(r, "registration", mode.String())
		h.logClientIP(r)
		httpx.WriteError(w, http.StatusForbidden, "registration_closed", "registration is closed")
		return
	}
	var c credentials
	if !decodeJSON(w, r, &c) {
		return
	}
	if !usernameRE.MatchString(c.Username) {
		writeInvalid(w, "username must be 3-32 characters of letters, digits or underscore")
		return
	}
	if !validPassword(c.Password) {
		writeInvalid(w, "password must be 8-72 bytes")
		return
	}
	ctx := r.Context()
	if _, err := h.d.DB.GetUserByUsername(ctx, c.Username); err == nil {
		httpx.WriteError(w, http.StatusConflict, "username_taken", "that username is taken")
		return
	} else if !errors.Is(err, db.ErrNotFound) {
		writeDBError(w, r, err)
		return
	}
	hash, err := auth.HashPassword(c.Password)
	if err != nil {
		writeInvalid(w, "password rejected")
		return
	}
	var user db.User
	if mode.NeedsFirstUser() {
		// The emptiness test rides inside the INSERT (§20 erratum 106). The
		// COUNT(*) above is a fast refusal and nothing more: bcrypt has just
		// spent a couple of hundred milliseconds between that read and this
		// write, which is all the room a burst of concurrent registrations
		// against a fresh instance needs to walk through a door `first`
		// promises shuts behind the FIRST one. Losing the race is the same
		// 403 a late registration has always got.
		user, err = h.d.DB.CreateFirstUser(ctx, ids.New(), c.Username, hash, h.unixNow())
		switch {
		case errors.Is(err, db.ErrConflict):
			httpx.SetLogField(r, "registration", mode.String())
			h.logClientIP(r)
			httpx.WriteError(w, http.StatusForbidden, "registration_closed", "registration is closed")
			return
		case err != nil:
			writeDBError(w, r, err)
			return
		}
	} else {
		for attempt := 0; ; attempt++ {
			user, err = h.d.DB.CreateUser(ctx, ids.New(), c.Username, hash, h.unixNow())
			if err == nil {
				break
			}
			if !errors.Is(err, db.ErrConflict) {
				writeDBError(w, r, err)
				return
			}
			// UNIQUE: the username raced us, or (astronomically) the id collided.
			if _, gerr := h.d.DB.GetUserByUsername(ctx, c.Username); gerr == nil {
				httpx.WriteError(w, http.StatusConflict, "username_taken", "that username is taken")
				return
			}
			if attempt >= 1 {
				httpx.WriteError(w, http.StatusInternalServerError, "id_collision", "could not allocate a user id; retry")
				return
			}
		}
	}
	// Every account that comes into existence is on the record, with the
	// address that made it (ROADMAP #21): on a public origin `first`/`open` is
	// the one route that mints a credential out of nothing, and "who
	// registered, from where, when" is the first question an operator asks.
	// The username is data the caller chose, so it rides as a quoted slog
	// attribute and never as part of the message.
	httpx.SetLogField(r, "registered", user.Username)
	h.logClientIP(r)
	if !h.issueSession(w, r, user) {
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, userResponse{User: toUserDTO(user)})
}

// login verifies credentials with exactly one bcrypt compare on every path
// and starts a session.
func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if !decodeJSON(w, r, &c) {
		return
	}
	if c.Username == "" || c.Password == "" {
		writeInvalid(w, "username and password are required")
		return
	}
	user, err := h.d.DB.GetUserByUsername(r.Context(), c.Username)
	switch {
	case errors.Is(err, db.ErrNotFound):
		dummyCompare(c.Password)
		httpx.SetLogField(r, "login", "unknown_user")
		h.logClientIP(r)
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password")
		return
	case err != nil:
		writeDBError(w, r, err)
		return
	case !auth.CheckPassword(user.PasswordHash, c.Password):
		httpx.SetLogField(r, "login", "bad_password")
		h.logClientIP(r)
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password")
		return
	}
	if !h.issueSession(w, r, user) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, userResponse{User: toUserDTO(user)})
}

// issueSession creates the session row and sets the cookie; on a DB error
// it writes 503 and returns false.
func (h *handler) issueSession(w http.ResponseWriter, r *http.Request, user db.User) bool {
	raw, ttl, err := auth.CreateSession(r.Context(), h.d.DB, h.d.Cfg, user.ID, r.UserAgent(), h.now())
	if err != nil {
		writeDBError(w, r, err)
		return false
	}
	auth.SetSessionCookie(w, h.d.Cfg, h.d.URLs.IsHTTPS(), raw, ttl)
	return true
}

// logout deletes the session row and clears the cookie.
func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	if err := h.d.DB.DeleteSession(r.Context(), p.SessionHash); err != nil && !errors.Is(err, db.ErrNotFound) {
		writeDBError(w, r, err)
		return
	}
	auth.ClearSessionCookie(w, h.d.Cfg, h.d.URLs.IsHTTPS())
	w.WriteHeader(http.StatusNoContent)
}

// me reports the caller and how it authenticated.
func (h *handler) me(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	httpx.WriteJSON(w, http.StatusOK, userResponse{User: toUserDTO(p.User), Via: p.Via})
}

// --- account security (ROADMAP #1; §20 errata 107–108) ---------------------

type passwordChange struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// changePassword rotates the caller's bcrypt hash and, in the SAME
// transaction, deletes every OTHER session of that user (§20 erratum 108).
// That is the whole incident response the product had been missing: "my
// laptop was stolen" is answered by changing the password from any other
// browser, and the stolen cookie stops resolving on its next request.
//
// Session-only and CSRF-guarded like every other cookie mutation, held by the
// AUTH limiter (not the upload one): this route takes a password and runs a
// bcrypt compare, so it belongs in the same bucket as login and register,
// which is what stops it being an oracle a script can hammer.
//
// The current password is re-verified even though the caller already holds a
// live session, because a session is "this browser was logged in once" and a
// password change is "this is still the account's owner" — an unlocked laptop
// must not silently become a permanent takeover.
func (h *handler) changePassword(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req passwordChange
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.CurrentPassword == "" || req.NewPassword == "" {
		writeInvalid(w, "current_password and new_password are required")
		return
	}
	if !validPassword(req.NewPassword) {
		writeInvalid(w, "password must be 8-72 bytes")
		return
	}
	// The row from the Resolver is as fresh as this request, so the compare
	// is against the hash a concurrent change would already have replaced.
	if !auth.CheckPassword(p.User.PasswordHash, req.CurrentPassword) {
		httpx.SetLogField(r, "password_change", "bad_current")
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_credentials", "the current password is wrong")
		return
	}
	if req.NewPassword == req.CurrentPassword {
		// Not a security rule — a courtesy. Rotating to the same secret
		// would still log every other browser out, which is a surprising
		// amount of damage for a no-op the caller did not mean.
		writeInvalid(w, "the new password must differ from the current one")
		return
	}
	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeInvalid(w, "password rejected")
		return
	}
	removed, err := h.d.DB.SetPassword(r.Context(), p.User.ID, hash, p.SessionHash)
	switch {
	case errors.Is(err, db.ErrNotFound):
		// The account went away under the request (an admin prune). There is
		// nothing to change and the caller's own credential is already dead.
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	case err != nil:
		writeDBError(w, r, err)
		return
	}
	httpx.SetLogField(r, "password_change", "ok")
	httpx.SetLogField(r, "sessions_revoked", removed)
	httpx.WriteJSON(w, http.StatusOK, sessionsRevoked{SessionsRevoked: removed})
}

// sessionsRevoked is the body of both revocation answers: how many other
// sessions went. A count is the only honest receipt — the caller cannot see
// the sessions it just ended, and "3" is what tells them the stolen browser
// was among them.
type sessionsRevoked struct {
	SessionsRevoked int64 `json:"sessions_revoked"`
}

// listSessions renders the caller's live sessions (§20 erratum 107). The
// cookie value is not in the database at all and its hash never leaves this
// package: the list is what a session IS (when it started, when it was last
// seen, when it must end) plus which one is asking, and the only action it
// offers is the DELETE below.
func (h *handler) listSessions(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	rows, err := h.d.DB.ListSessions(r.Context(), p.User.ID, h.unixNow())
	if err != nil {
		writeDBError(w, r, err)
		return
	}
	out := sessionList{Sessions: make([]SessionDTO, 0, len(rows))}
	for _, s := range rows {
		out.Sessions = append(out.Sessions, toSessionDTO(s, s.TokenHash == p.SessionHash))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// deleteSessions is "log out everywhere but here" (§20 erratum 107): every
// session of this user except the one making the request. The caller's own
// cookie is untouched, which is the difference between this and logout and
// the reason it is safe to click from the phone you are holding.
func (h *handler) deleteSessions(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	removed, err := h.d.DB.DeleteOtherSessions(r.Context(), p.User.ID, p.SessionHash)
	if err != nil {
		writeDBError(w, r, err)
		return
	}
	httpx.SetLogField(r, "sessions_revoked", removed)
	httpx.WriteJSON(w, http.StatusOK, sessionsRevoked{SessionsRevoked: removed})
}
