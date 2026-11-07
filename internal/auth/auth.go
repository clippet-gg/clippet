// Package auth: bcrypt passwords, sessions, API tokens, the request
// Principal and the Require* / CSRF middleware (DESIGN.md §14, §18.2).
//
// Sessions are 256-bit random opaque cookie values stored as hex sha256;
// API tokens are "ck_" + 43 base64url chars stored the same way. Resolver
// turns either credential into a Principal on the request context and never
// rejects by itself; the Require* middleware and CSRF do the rejecting.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/ids"
)

// Principal is the authenticated caller. Population per Via (§14):
// Via="session" → SessionHash set, TokenID "", Scopes nil (all scopes);
// Via="token" → TokenID set, SessionHash "", Scopes = the token's list.
type Principal struct {
	User        db.User
	Via         string // "session" | "token"
	SessionHash string
	TokenID     string
	Scopes      []string
}

// Via values.
const (
	ViaSession = "session"
	ViaToken   = "token"
)

// The API-token scopes (§20 erratum 111). `upload` is the capture-tool
// credential of erratum 19; `read` lets automation GET the owner's own clips
// and collections — a backup script, a "sync my library to a NAS" cron — and
// nothing else. They are independent: a token may hold either, or both.
const (
	ScopeUpload = "upload"
	ScopeRead   = "read"
)

// Scopes is the closed set a token may be minted with, in the order the mint
// form and every DTO render them.
var Scopes = []string{ScopeUpload, ScopeRead}

// ScopeInbound marks an api_tokens row as an INBOUND URL (§20 erratum 322): a
// `ci_` secret that travels in the path of POST /in/{token} and is held alone.
// It is deliberately NOT in Scopes, so ValidScope refuses it and no Bearer
// token can be minted holding it beside another scope. No route requires it:
// the one route that reads such a row checks the row itself, and the
// Principal it installs is a token principal (Via = ViaToken) holding only
// this scope — so every existing gate refuses it, failing closed.
const ScopeInbound = "inbound"

// ScopeAutoFetch marks an api_tokens row as a CLIPPET AUTO URL (§20 erratum
// 324): a `ca_` secret in the path of POST /in/auto/{token}, held alone, that
// makes this instance fetch one Discord attachment into the row's account.
// Outside Scopes for ScopeInbound's reasons, and failing closed the same way.
const ScopeAutoFetch = "autofetch"

// IsInboundSet reports whether a CLEANED scope set is an inbound URL's: the
// one scope ScopeInbound, alone. IsAutoFetchSet is the same for a Clippet Auto
// URL. The ONE spelling of both set tests in Go (run 8: the mint door, the
// admin verb and the settings page each had their own); web/static/app.js
// keeps its own, `tokenLead`, named as internal/web's is.
func IsInboundSet(scopes []string) bool { return IsOnly(scopes, ScopeInbound) }

// IsAutoFetchSet: see IsInboundSet.
func IsAutoFetchSet(scopes []string) bool { return IsOnly(scopes, ScopeAutoFetch) }

// IsOnly is the exact set test the two path credentials are read with: one
// element, and it is s (§20 erratum 324's review: `len == 1`, never "holds").
func IsOnly(scopes []string, s string) bool { return len(scopes) == 1 && scopes[0] == s }

// pathScoped reports whether a row is one of the two kinds whose secret
// travels in a URL path — an inbound URL or a Clippet Auto URL. Such a row is
// never a Bearer credential, whatever else it holds.
func pathScoped(t db.APIToken) bool {
	return t.HasScope(ScopeInbound) || t.HasScope(ScopeAutoFetch)
}

// ValidScope reports whether s is one of the scopes this build knows. An
// unknown scope is refused at mint time rather than stored and ignored: a row
// claiming a permission the binary does not implement is a lie a later
// release could accidentally make true.
func ValidScope(s string) bool {
	for _, k := range Scopes {
		if k == s {
			return true
		}
	}
	return false
}

// HasScope reports whether the principal may use scope: sessions hold every
// scope implicitly; tokens only what they were minted with.
func (p Principal) HasScope(scope string) bool {
	if p.Via != ViaToken {
		return true
	}
	for _, s := range p.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// OwnerReader reports whether this principal may see the OWNER-private
// fields of rows it owns — the `views` count, `source_duration_ms`, `trim`
// (§8.2). Erratum 19 restricted that to a browser session because a capture
// token is an upload credential; erratum 112 widens it by exactly the `read`
// scope, which exists to let the owner's own automation read the owner's own
// library. An upload-only token still sees the public DTO of its own clips.
//
// It says nothing about which ROW is the caller's: every call site pairs it
// with `p.User.ID == ownerID`.
func (p Principal) OwnerReader() bool {
	return p.Via != ViaToken || p.HasScope(ScopeRead)
}

type ctxKey struct{}

// WithPrincipal stores p in ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the Principal installed by WithPrincipal, if any.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

type errKey struct{}

// WithResolveError records err as the request's credential-resolution
// failure (see ResolveError). Exported so a stub Authenticator can inject
// one in tests.
func WithResolveError(ctx context.Context, err error) context.Context {
	return context.WithValue(ctx, errKey{}, err)
}

// ResolveError returns the database error the Resolver hit while checking
// the request's credential, or nil. The Resolver never rejects by itself
// (§14): on such an error it passes the request through anonymously and
// records the error here so each route family answers in its own shape —
// httpapi with the 503 envelope, the pages with their 503 page. Callers
// that serve credentialed content must check it before trusting "anonymous".
func ResolveError(ctx context.Context) error {
	err, _ := ctx.Value(errKey{}).(error)
	return err
}

// Authenticator resolves credentials into a Principal on the request
// context without rejecting anything itself.
type Authenticator interface {
	Middleware(next http.Handler) http.Handler
}

// Defaults applied when the config carries zero values (tests build a bare
// config.Config; config.Load always fills these).
const (
	DefaultCookieName = "clippet_session"
	DefaultSessionTTL = 720 * time.Hour
	// DefaultSessionMaxTTL mirrors config.DefaultSessionMaxTTL for a bare
	// config.Config (§20 erratum 105).
	DefaultSessionMaxTTL = 2160 * time.Hour

	// touchInterval bounds the sliding-expiry and last_used_at writes to one
	// per credential per minute (§14).
	touchInterval = time.Minute
	// touchGateMax bounds the in-memory last-touch map (§14).
	touchGateMax = 10000
	// bearerPrefix is the Authorization scheme accepted for API tokens.
	bearerPrefix = "Bearer "
	// maxUserAgent caps what a session row stores.
	maxUserAgent = 256
)

// CookieName returns the configured session cookie name (or the default).
func CookieName(cfg config.Config) string {
	if cfg.SessionCookie == "" {
		return DefaultCookieName
	}
	return cfg.SessionCookie
}

// SessionTTL returns the configured session lifetime (or the default).
func SessionTTL(cfg config.Config) time.Duration {
	if cfg.SessionTTL <= 0 {
		return DefaultSessionTTL
	}
	return cfg.SessionTTL
}

// SessionMaxTTL returns the configured ceiling on the sliding window (or the
// default), measured from a session's creation (§20 erratum 105).
func SessionMaxTTL(cfg config.Config) time.Duration {
	if cfg.SessionMaxTTL <= 0 {
		return DefaultSessionMaxTTL
	}
	return cfg.SessionMaxTTL
}

// slidTo is the expiry a touch at `now` produces for a session created at
// `createdAt`: `now + SESSION_TTL`, never past `createdAt + SESSION_MAX_TTL`
// (§20 erratum 105). It is also what the re-issued cookie's Max-Age is built
// from, so the browser and the row stop agreeing to disagree — a cookie that
// outlived its row was the shape erratum 25 fixed in the other direction.
func slidTo(cfg config.Config, createdAt, now time.Time) time.Time {
	want := now.Add(SessionTTL(cfg))
	if ceiling := createdAt.Add(SessionMaxTTL(cfg)); want.After(ceiling) {
		return ceiling
	}
	return want
}

// Resolver is the real Authenticator: Bearer token first, then the cookie.
// The zero fields are lazily initialised so the literal
// &Resolver{DB: …, Cfg: …, Now: …} serve.go builds is complete.
type Resolver struct {
	DB  *db.DB
	Cfg config.Config
	Now func() time.Time

	once  sync.Once
	touch *touchGate
}

var _ Authenticator = (*Resolver)(nil)

func (a *Resolver) init() {
	a.once.Do(func() { a.touch = newTouchGate(touchGateMax) })
}

func (a *Resolver) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Middleware resolves the Bearer token (if present and valid), else the
// session cookie, into a Principal on the context. It never rejects (§14):
// a database failure while resolving leaves the request anonymous and is
// recorded for ResolveError, so the JSON API can answer its 503 envelope
// and the pages their 503 page. With a nil DB it is a pure pass-through.
//
// The session is sliding on both sides (§4, §20 erratum 25): whenever the
// once-a-minute touch extends the row's expires_at, the same response
// re-issues the cookie — same value, Max-Age = SESSION_TTL again — so a
// browser that honours Max-Age keeps the session exactly as long as the
// database does instead of dropping it SESSION_TTL after login.
func (a *Resolver) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.DB == nil {
			next.ServeHTTP(w, r)
			return
		}
		a.init()
		p, ok, refresh, err := a.resolve(r)
		switch {
		case err != nil:
			httpx.SetLogField(r, "auth_db_error", err.Error())
			r = r.WithContext(WithResolveError(r.Context(), err))
		case ok:
			if refresh.raw != "" {
				SetSessionCookie(w, a.Cfg, cfgHTTPS(a.Cfg), refresh.raw, refresh.ttl)
			}
			r = r.WithContext(WithPrincipal(r.Context(), p))
		}
		next.ServeHTTP(w, r)
	})
}

// refreshCookie is the cookie a sliding touch re-issues on this response:
// the same value with the lifetime the ROW now has, which the §20 erratum 105
// ceiling can make shorter than SESSION_TTL. The zero value means "nothing to
// re-issue".
type refreshCookie struct {
	raw string
	ttl time.Duration
}

// resolve turns the request's credential into a Principal. refresh carries
// the raw session cookie to re-issue when this request slid the row's expiry
// (zero otherwise, and always for a token).
func (a *Resolver) resolve(r *http.Request) (Principal, bool, refreshCookie, error) {
	now := a.now()
	if raw, ok := bearerToken(r); ok {
		p, found, err := a.resolveToken(r.Context(), raw, now)
		if err != nil || found {
			return p, found, refreshCookie{}, err
		}
	}
	c, err := r.Cookie(CookieName(a.Cfg))
	if err != nil || c.Value == "" {
		return Principal{}, false, refreshCookie{}, nil
	}
	p, found, until, err := a.resolveSession(r.Context(), c.Value, now)
	if !until.IsZero() {
		return p, found, refreshCookie{raw: c.Value, ttl: until.Sub(now)}, err
	}
	return p, found, refreshCookie{}, err
}

// cfgHTTPS mirrors urls.Builder.IsHTTPS for the cookie's Secure flag; the
// Resolver holds only the config.
func cfgHTTPS(cfg config.Config) bool {
	return cfg.PublicBaseURL != nil && cfg.PublicBaseURL.Scheme == "https"
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) <= len(bearerPrefix) || !strings.EqualFold(h[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	raw := strings.TrimSpace(h[len(bearerPrefix):])
	if !strings.HasPrefix(raw, ids.TokenPrefix) {
		return "", false
	}
	return raw, true
}

func (a *Resolver) resolveToken(ctx context.Context, raw string, now time.Time) (Principal, bool, error) {
	hash := HashToken(raw)
	tok, user, err := a.DB.GetTokenByHash(ctx, hash)
	if errors.Is(err, db.ErrNotFound) {
		return Principal{}, false, nil
	}
	if err != nil {
		return Principal{}, false, err
	}
	// An inbound URL's row is never a Bearer credential (§20 erratum 322), and
	// neither is a Clippet Auto URL's (§20 erratum 324). bearerToken admits
	// only `ck_` and the hash covers the prefix, so this is belt and braces: a
	// row holding `inbound` or `autofetch` is simply not found here.
	if pathScoped(tok) {
		return Principal{}, false, nil
	}
	if a.touch.due(hash, now) {
		// Bookkeeping only: a failed touch must not fail the request.
		_ = a.DB.TouchToken(ctx, tok.ID, now.Unix())
	}
	return Principal{User: user, Via: ViaToken, TokenID: tok.ID, Scopes: tok.Scopes}, true, nil
}

// resolveSession returns a non-zero `until` when this request extended the
// row's expiry (the once-a-minute touch), so the caller re-issues the cookie
// with exactly the lifetime the row now has. That is the §20 erratum 105
// clamp: `now + SESSION_TTL`, never past `created_at + SESSION_MAX_TTL`, and
// the browser is told the same thing the database was.
func (a *Resolver) resolveSession(ctx context.Context, raw string, now time.Time) (p Principal, found bool, until time.Time, err error) {
	hash := HashToken(raw)
	s, user, err := a.DB.GetSession(ctx, hash, now.Unix())
	if errors.Is(err, db.ErrNotFound) {
		return Principal{}, false, time.Time{}, nil
	}
	if err != nil {
		return Principal{}, false, time.Time{}, err
	}
	if a.touch.due(hash, now) {
		// Bookkeeping: a failed touch must not fail the request. The cookie
		// slides regardless — the next touch retries the row.
		expires := slidTo(a.Cfg, time.Unix(s.CreatedAt, 0), now)
		_ = a.DB.TouchSession(ctx, hash, now.Unix(), expires.Unix())
		until = expires
	}
	return Principal{User: user, Via: ViaSession, SessionHash: hash}, true, until, nil
}

// touchGate remembers when each credential was last touched so the sliding
// session expiry and token last_used_at are written at most once a minute.
// At the cap the map is reset — the cost is one extra write per live
// credential, the benefit is a hard memory bound.
type touchGate struct {
	mu   sync.Mutex
	max  int
	last map[string]time.Time
}

func newTouchGate(max int) *touchGate {
	return &touchGate{max: max, last: make(map[string]time.Time)}
}

func (g *touchGate) due(key string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if t, ok := g.last[key]; ok && now.Sub(t) < touchInterval {
		return false
	}
	if len(g.last) >= g.max {
		g.last = make(map[string]time.Time)
	}
	g.last[key] = now
	return true
}

// CreateSession issues a new session for userID: it inserts the row keyed by
// the cookie's hash and returns the raw cookie value and the lifetime the
// cookie should carry.
//
// The row's ceiling (§20 erratum 105) is written HERE and never moved again,
// so it is a fact about this session and not about how hard it is used. The
// returned duration is the clamped one: with `SESSION_MAX_TTL` below
// `SESSION_TTL` even a brand-new session is short, and the cookie says so
// rather than outliving the row it names.
func CreateSession(ctx context.Context, d *db.DB, cfg config.Config, userID, userAgent string, now time.Time) (string, time.Duration, error) {
	raw, hash := NewSessionToken()
	if len(userAgent) > maxUserAgent {
		userAgent = userAgent[:maxUserAgent]
	}
	expires := slidTo(cfg, now, now)
	maxExpires := now.Add(SessionMaxTTL(cfg))
	if err := d.CreateSession(ctx, hash, userID, now.Unix(), expires.Unix(), maxExpires.Unix(), userAgent); err != nil {
		return "", 0, err
	}
	return raw, expires.Sub(now), nil
}

// BcryptCost is the fixed work factor (§14).
const BcryptCost = 12

// HashPassword hashes pw with bcrypt cost 12. bcrypt itself rejects inputs
// longer than 72 bytes; the 8-byte minimum is validated by the handler.
func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), BcryptCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// CheckPassword reports whether pw matches hash.
func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

func reject(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteError(w, status, code, msg)
}

// RequireUser rejects unauthenticated requests with 401 unauthorized.
func RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := FromContext(r.Context()); !ok {
			reject(w, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireSession rejects unauthenticated (401) and token-authenticated
// (403 token_not_allowed) requests.
func RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := FromContext(r.Context())
		if !ok {
			reject(w, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}
		if p.Via == ViaToken {
			reject(w, http.StatusForbidden, "token_not_allowed", "this endpoint requires a browser session")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireSessionOrScope is RequireSession widened by exactly one scope (§20
// erratum 111): a browser session passes as it always did, and so does a
// token that was minted with `scope`. Everything else answers what it
// answered before — 401 for anonymous, 403 `token_not_allowed` for a token
// without the scope — so an upload-only capture token still cannot read the
// library, and no existing rejection is weakened.
//
// The refusal keeps `token_not_allowed` rather than gaining a more precise
// `insufficient_scope`: erratum 19's wire answer for a capture token on a
// session-only route is part of the contract clients were written against,
// and only the human-readable message names the way out.
func RequireSessionOrScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := FromContext(r.Context())
			if !ok {
				reject(w, http.StatusUnauthorized, "unauthorized", "authentication required")
				return
			}
			if p.Via == ViaToken && !p.HasScope(scope) {
				reject(w, http.StatusForbidden, "token_not_allowed",
					"this endpoint requires a browser session or a token with the "+scope+" scope")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireScope rejects token principals lacking scope (403
// insufficient_scope). Sessions hold every scope; an anonymous request is
// 401 so the middleware is safe to compose on its own.
func RequireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := FromContext(r.Context())
			if !ok {
				reject(w, http.StatusUnauthorized, "unauthorized", "authentication required")
				return
			}
			if !p.HasScope(scope) {
				reject(w, http.StatusForbidden, "insufficient_scope", "token lacks the "+scope+" scope")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CSRF applies the §14 rule to cookie-authenticated mutating requests
// (POST, PATCH, DELETE, PUT): reject 403 csrf only when an Origin header is
// present and differs from origin (urls.Builder.Origin()), or Sec-Fetch-Site
// is present and equals "cross-site". Requests with neither header (curl,
// cookie jars) pass; Bearer-authenticated and anonymous requests skip the
// check entirely.
func CSRF(origin string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if mutating(r.Method) {
				if p, ok := FromContext(r.Context()); ok && p.Via == ViaSession && crossSite(r, origin) {
					httpx.SetLogField(r, "csrf", "rejected")
					reject(w, http.StatusForbidden, "csrf", "cross-site request rejected")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CrossSite reports whether a request came from another site, by the same two
// headers the CSRF rule reads. It is exported for the one form POST that must
// refuse a cross-site caller WITHOUT a session to compare against: the
// one-time login link (§20 erratum 341), where a forged POST would sign a
// victim's browser into somebody else's account.
func CrossSite(r *http.Request, origin string) bool { return crossSite(r, origin) }

// SigninLinkPrefix is the path of the bot's one-time login links, before the
// token (§20 erratum 341): `/signin` replies with PUBLIC_BASE_URL + this +
// the raw `cs_…` value, and internal/web serves the page there. One spelling
// for both packages, which may not import each other.
const SigninLinkPrefix = "/login/discord/"

func mutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPatch, http.MethodDelete, http.MethodPut:
		return true
	}
	return false
}

func crossSite(r *http.Request, origin string) bool {
	if o := r.Header.Get("Origin"); o != "" && !sameOrigin(o, origin) {
		return true
	}
	return strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site")
}

// sameOrigin compares scheme, host and effective port of two origins; a
// browser omits default ports, config may not.
func sameOrigin(a, b string) bool {
	ua, err := url.Parse(strings.TrimSpace(a))
	if err != nil {
		return false
	}
	ub, err := url.Parse(strings.TrimSpace(b))
	if err != nil {
		return false
	}
	return originTuple(ua) == originTuple(ub)
}

func originTuple(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	if scheme == "" || host == "" {
		return ""
	}
	return scheme + "://" + host + ":" + port
}

// NewSessionToken returns a fresh raw cookie value (32 random bytes as 43
// base64url characters) and its hex sha256.
func NewSessionToken() (raw, hash string) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("auth: crypto/rand failed: " + err.Error())
	}
	raw = base64.RawURLEncoding.EncodeToString(b[:])
	return raw, HashToken(raw)
}

// HashToken returns the hex sha256 of a raw session or API token.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// TokenPrefixOf returns the display prefix of a raw API token: the first 8
// characters after "ck_", after "ci_" for an inbound URL's secret (§20
// erratum 322), or after "ca_" for a Clippet Auto URL's (§20 erratum 324).
func TokenPrefixOf(raw string) string {
	s := raw
	for _, p := range []string{ids.TokenPrefix, ids.InboundPrefix, ids.AutoFetchPrefix} {
		if rest, ok := strings.CutPrefix(raw, p); ok {
			s = rest
			break
		}
	}
	if len(s) > 8 {
		s = s[:8]
	}
	return s
}

func sessionCookie(cfg config.Config, https bool) *http.Cookie {
	return &http.Cookie{
		Name:     CookieName(cfg),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   https,
	}
}

// SetSessionCookie writes the session cookie (§14 flags): Path=/, HttpOnly,
// SameSite=Lax, Secure iff https, and `Max-Age = ttl` — the lifetime the ROW
// has, which the §20 erratum 105 ceiling can make shorter than SESSION_TTL. A
// non-positive ttl would spell "delete this cookie" to a browser, so it is
// floored at one second: a session in its last moment is still a session, and
// the next request resolves it or does not.
func SetSessionCookie(w http.ResponseWriter, cfg config.Config, https bool, raw string, ttl time.Duration) {
	c := sessionCookie(cfg, https)
	c.Value = raw
	secs := int(ttl / time.Second)
	if secs < 1 {
		secs = 1
	}
	c.MaxAge = secs
	http.SetCookie(w, c)
}

// ClearSessionCookie expires the session cookie. A refresh the Resolver
// queued on this same response (Middleware's sliding Max-Age) is dropped
// first: logout must never re-issue what it clears.
func ClearSessionCookie(w http.ResponseWriter, cfg config.Config, https bool) {
	c := sessionCookie(cfg, https)
	dropSetCookie(w.Header(), c.Name)
	c.MaxAge = -1
	c.Expires = time.Unix(0, 0)
	http.SetCookie(w, c)
}

// dropSetCookie removes every pending Set-Cookie for name from h.
func dropSetCookie(h http.Header, name string) {
	pending := h.Values("Set-Cookie")
	if len(pending) == 0 {
		return
	}
	kept := make([]string, 0, len(pending))
	for _, v := range pending {
		if !strings.HasPrefix(v, name+"=") {
			kept = append(kept, v)
		}
	}
	h.Del("Set-Cookie")
	for _, v := range kept {
		h.Add("Set-Cookie", v)
	}
}

// --- the "Sign in with Discord" flow cookie (ROADMAP #44, §20 erratum 302) ---

// FlowCookieSuffix is appended to the session cookie's name for the flow
// cookie, so it INHERITS the `__Host-` prefix on an https origin that uses one
// (the subdomain sign-in path): a flow cookie planted by a subdomain is the one way to
// fix a Discord login, and the prefix is what forbids it.
const FlowCookieSuffix = "_discord"

// FlowCookieTTL bounds one round trip to Discord and back. Ten minutes is
// generous for a consent screen and far too short for a planted cookie to
// wait for a victim.
const FlowCookieTTL = 10 * time.Minute

// FlowCookieName is the flow cookie's name: the session cookie's plus the
// suffix.
func FlowCookieName(cfg config.Config) string { return CookieName(cfg) + FlowCookieSuffix }

func flowCookie(cfg config.Config, https bool) *http.Cookie {
	return &http.Cookie{
		Name:     FlowCookieName(cfg),
		Path:     "/",
		HttpOnly: true,
		// Lax and not Strict: the callback is a top-level GET navigation from
		// Discord's site, which Lax sends the cookie on and Strict would not.
		SameSite: http.SameSiteLaxMode,
		Secure:   https,
	}
}

// SetFlowCookie writes the flow cookie with the session cookie's flags —
// Path=/, HttpOnly, SameSite=Lax, Secure iff https — and FlowCookieTTL.
func SetFlowCookie(w http.ResponseWriter, cfg config.Config, https bool, value string) {
	c := flowCookie(cfg, https)
	c.Value = value
	c.MaxAge = int(FlowCookieTTL / time.Second)
	http.SetCookie(w, c)
}

// ClearFlowCookie expires the flow cookie. It drops any pending Set-Cookie of
// the same name first, as ClearSessionCookie does: a flow is single-use, and
// the clearing is queued before anything else on the callback so a replayed
// callback URL finds no cookie.
func ClearFlowCookie(w http.ResponseWriter, cfg config.Config, https bool) {
	c := flowCookie(cfg, https)
	dropSetCookie(w.Header(), c.Name)
	c.MaxAge = -1
	c.Expires = time.Unix(0, 0)
	http.SetCookie(w, c)
}
