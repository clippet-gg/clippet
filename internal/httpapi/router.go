package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"clippet/internal/auth"
	"clippet/internal/clientip"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/ids"
	"clippet/internal/ratelimit"
)

// jsonTimeout bounds the short JSON routes (§8). Upload, media and owner
// DELETE are never wrapped: they stream or run a bounded destroy inline.
// A variable only so tests can shrink it before NewHandler.
var jsonTimeout = 15 * time.Second

// timeoutBody is what http.TimeoutHandler writes on expiry — the envelope,
// labelled JSON by the headers timeout() pre-sets on the real writer.
const timeoutBody = `{"error":"timeout","message":"request timed out"}` + "\n"

// methodOrder fixes the Allow header order for byMethod routes.
var methodOrder = []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete}

// byMethod maps a method to its fully wrapped handler for one path. HEAD is
// served by the GET handler; anything unlisted is a 405 from Methods.
type byMethod map[string]http.Handler

func (m byMethod) handler() http.Handler {
	var methods []string
	for _, name := range methodOrder {
		if _, ok := m[name]; ok {
			methods = append(methods, name)
		}
	}
	return Methods(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, ok := m[r.Method]
		if !ok && r.Method == http.MethodHead {
			h, ok = m[http.MethodGet]
		}
		if !ok { // unreachable: Methods filtered the method already
			httpx.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method "+r.Method+" not allowed")
			return
		}
		h.ServeHTTP(w, r)
	}), methods...)
}

// chain wraps final with mws, the first listed being outermost.
func chain(final http.Handler, mws ...httpx.Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		final = mws[i](final)
	}
	return final
}

// clientCtxKey carries the request's own, client-cancellable context past
// http.TimeoutHandler, which derives the handler's context from the one it
// is handed and has no other way to be given a deadline alone.
type clientCtxKey struct{}

// timeout wraps next in http.TimeoutHandler. The JSON content type and
// no-store are set on the real writer first, so the stock 503 written on
// expiry is a labelled, uncacheable envelope (the route-level security
// headers are already on that writer); on the normal path the inner
// handler's own headers are merged over them. Every short JSON route is
// therefore no-store — §15 demands it for auth/token responses and none of
// the others is cacheable JSON either.
//
// TimeoutHandler is given a context that can only EXPIRE (§20 erratum 54):
// its "context done for any other reason" arm writes a bodyless 503 and
// throws away whatever the handler had already answered, so a client that
// goes away mid-request — net/http cancels the request context the moment
// its body comes up short or the socket closes — would turn every short
// JSON route into a spurious 5xx against the §12 doctrine. cancelable hands
// that cancellation back to the handler below.
func timeout(next http.Handler) http.Handler {
	th := http.TimeoutHandler(cancelable(next), jsonTimeout, timeoutBody)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "application/json; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		client := r.Context()
		deadlineOnly := context.WithValue(context.WithoutCancel(client), clientCtxKey{}, client)
		th.ServeHTTP(&bodylessWriter{ResponseWriter: w}, r.WithContext(deadlineOnly))
	})
}

// cancelable restores, below http.TimeoutHandler, the cancellation timeout()
// withheld from it: the handler's context still dies when the client goes
// away — so a database or storage call stops, and writeClientGone can tell —
// while it keeps TimeoutHandler's deadline, which is what still produces the
// timeout envelope. The handler's own answer then wins the race it used to
// lose (§20 erratum 54).
func cancelable(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client, ok := r.Context().Value(clientCtxKey{}).(context.Context)
		if !ok { // unreachable: timeout() always attaches it
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		defer context.AfterFunc(client, cancel)()
		if client.Err() != nil {
			cancel() // already gone: never start work under a live context
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bodylessWriter drops the pre-set Content-Type when the inner handler
// answered 204 (logout, token revoke, view): there is no body to label.
type bodylessWriter struct{ http.ResponseWriter }

func (w *bodylessWriter) WriteHeader(code int) {
	if code == http.StatusNoContent {
		w.Header().Del("Content-Type")
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *bodylessWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// authGate answers 503 for a Resolver database error (the Resolver itself
// only records it, §14) so no API route runs with a credential it could
// not check. It sits directly after the Resolver on every credentialed chain.
func authGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := auth.ResolveError(r.Context()); err != nil {
			writeDBError(w, r, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// privateNoStore marks a principal-dependent answer — clip JSON, whose
// views field is the owner's alone (§8.2), and the owner-only `/original`,
// which is the one route under the Resolver that `timeout` does not preset
// (§20 erratum 55) — as uncacheable by any shared cache, and keyed on the
// credential for a misconfigured cache that stores anyway. `serveObject`
// overwrites both headers on the success path with the media pair
// (`private, max-age=3600` + the same Vary), exactly as an inner handler
// overwrites `timeout`'s preset; the 403/404/416 exits keep these.
func privateNoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "private, no-store")
		h.Set("Vary", "Cookie, Authorization")
		next.ServeHTTP(w, r)
	})
}

// mediaHeaders is the header set for media responses (§15): nosniff
// everywhere plus the referrer policy, but no X-Frame-Options and no CSP —
// those guard pages, and would only block a consumer that frames the MP4.
func mediaHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

// routes builds the §8.1 route table.
func (h *handler) routes() http.Handler {
	sec := httpx.SecurityHeaders(false, h.d.URLs.IsHTTPS(), h.d.Cfg.MediaOrigin())
	authn := h.auth.Middleware
	csrf := auth.CSRF(h.d.URLs.Origin())
	authRate := ratelimit.Middleware(h.authLimit, h.ipKey)
	viewRate := ratelimit.Middleware(h.viewLimit, h.ipKey)
	// The single progress route's own bucket (§20 erratum 211, amended): a poll is
	// polled by design, and it must not spend the same tokens a VIEW does.
	pollRate := ratelimit.Middleware(h.pollLimit, h.ipKey)
	// The Discord endpoint's own bucket (§20 erratum 256), per IP.
	botRate := ratelimit.Middleware(h.botLimit, h.ipKey)
	// The official bot's claim endpoint's own bucket (§20 erratum 279(h)),
	// per IP: anonymous and public, it must never spend the tokens the
	// official bot's `/clip`s need. Since §20 erratum 303 the instance's
	// self-description shares it: the two are the same shape of anonymous
	// machine GET, and a second limiter for a second such route is the thing
	// no-new-limiter rule exists to prevent.
	claimRate := ratelimit.Middleware(h.claimLimit, h.ipKey)
	uploadRate := ratelimit.Middleware(h.uploadLimit, h.userKey)
	// The upload routes' second bucket (§20 erratum 97). The user bucket is
	// listed first so a caller who is over their OWN budget is refused
	// without spending an IP token — the ordering does not change the
	// aggregate ceiling either way.
	uploadIPRate := ratelimit.Middleware(h.uploadIPLimit, h.ipKey)
	upload := auth.RequireScope(auth.ScopeUpload)
	// The owner-read gate (§20 erratum 111): a browser session, or a token
	// minted with the `read` scope, and nothing else.
	readable := auth.RequireSessionOrScope(auth.ScopeRead)

	// authed: credential resolution, the 503 gate for a resolver DB error,
	// then the route's own middleware.
	authed := func(final http.HandlerFunc, mws ...httpx.Middleware) http.Handler {
		return authn(authGate(chain(final, mws...)))
	}
	// json: a short JSON route — TimeoutHandler outermost.
	json := func(final http.HandlerFunc, mws ...httpx.Middleware) http.Handler {
		return timeout(authed(final, mws...))
	}
	// long: no TimeoutHandler (upload, DELETE, original).
	long := authed

	mux := http.NewServeMux()
	h.patterns = nil
	// handle records the pattern and then registers it (§20 erratum 248 — see
	// handler.patterns). Every registration in this function goes through it,
	// directly or through `route`.
	handle := func(path string, final http.Handler) {
		h.patterns = append(h.patterns, path)
		mux.Handle(path, final)
	}
	// The header middleware wraps each path outermost so 405s and timeout
	// 503s carry it too: the page set on JSON paths, the lighter set on
	// media paths (public streams need no credential lookup at all).
	route := func(path string, headers httpx.Middleware, m byMethod) {
		handle(path, headers(m.handler()))
	}
	// noRoute is the catch-all's handler VALUE, registered for "/" at the end
	// of this function — and called, as the very same value, by the inbound
	// URL's credential check for every miss (§20 erratum 322), so a wrong
	// token's answer is a missing route's by construction rather than by copy.
	noRoute := sec(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "no such route")
	}))

	route("/healthz", sec, byMethod{http.MethodGet: timeout(http.HandlerFunc(h.healthz))})
	route("/readyz", sec, byMethod{http.MethodGet: timeout(http.HandlerFunc(h.readyz))})
	// The instance's own self-description (§20 erratum 303, instance.go): the
	// release, the commit it was built from and which of the two media-delivery
	// shapes this box serves. Beside the health probes because it is the third
	// thing about the PROCESS that an anonymous caller may ask, and registered
	// UNCONDITIONALLY — there is no configuration that turns it on, because
	// there is no feature behind it: the answer is a function of the build and
	// `.env`, and it is the same on an instance nobody is verifying.
	//
	// It carries the `claim` bucket rather than one of its own (the rule: no
	// new limiter for a new anonymous route). The two are the same shape of
	// caller — an anonymous GET from a machine that already knows this origin,
	// a handful a day per name — and the ceiling they share, 30/min per client
	// address, is far above what a verifier spends and far below what makes
	// either route worth pointing at something.
	//
	// The path is SPELLED here, not named: `guard/test/routes.test.ts` in the
	// verification plane diffs every registration in this file against the
	// guard's own allowlist, and it resolves a path CONSTANT only for the
	// three Discord files it lists by name. A route registered by a constant
	// it cannot resolve is invisible to that diff, which would then pass
	// vacuously on the one route the plane exists to read. `instance.go` keeps
	// the same string as `instancePath` for the docs and the tests, and a pin
	// there asserts the two agree. (That reader balances parentheses from the
	// registration helper's own name, so the name is not written here either:
	// spelled inside a comment it reads as a call and swallows the rest of the
	// file, which is how this note came to be written — the diff went from 45
	// routes to none and said so.)
	route("/api/instance", sec, byMethod{
		http.MethodGet: json(h.instance, claimRate),
	})

	route("/api/auth/register", sec, byMethod{
		http.MethodPost: json(h.register, authRate, requireJSON),
	})
	route("/api/auth/login", sec, byMethod{
		http.MethodPost: json(h.login, authRate, requireJSON),
	})
	route("/api/auth/logout", sec, byMethod{
		http.MethodPost: json(h.logout, auth.RequireSession, csrf),
	})
	route("/api/auth/me", sec, byMethod{
		http.MethodGet: json(h.me, auth.RequireUser),
	})
	// Account security (ROADMAP #1; §20 errata 107–108). Both are session-only
	// and CSRF-guarded — a capture token must never be able to rotate the
	// password that would revoke it. PATCH carries the AUTH limiter, not the
	// upload one: it takes a password and runs a bcrypt compare, so it
	// belongs in the same per-IP bucket as login and register rather than in
	// a per-user budget that a thief holding the cookie already owns. The
	// session LIST is principal-dependent JSON, so it gets privateNoStore
	// exactly as GET /api/clips does.
	route("/api/auth/password", sec, byMethod{
		http.MethodPatch: json(h.changePassword, auth.RequireSession, csrf, requireJSON, authRate),
	})
	route("/api/auth/sessions", sec, byMethod{
		http.MethodGet:    json(h.listSessions, privateNoStore, auth.RequireSession),
		http.MethodDelete: json(h.deleteSessions, auth.RequireSession, csrf),
	})
	route("/api/auth/tokens", sec, byMethod{
		http.MethodGet:  json(h.listTokens, auth.RequireSession),
		http.MethodPost: json(h.createToken, auth.RequireSession, csrf, requireJSON),
	})
	route("/api/auth/tokens/{tid}", sec, byMethod{
		http.MethodDelete: json(h.revokeToken, auth.RequireSession, csrf),
	})
	// The account's own settings and its own tally (ROADMAP #22, #23; §20
	// errata 155-156). SESSION only, both of them — a leaked capture token
	// must not be able to read how much an account has stored, and certainly
	// not to change what its future uploads default to (erratum 19). The GET
	// pair carries privateNoStore for the reason GET /api/clips does: the body
	// depends entirely on who is asking.
	route("/api/auth/me/settings", sec, byMethod{
		http.MethodGet:   json(h.getSettings, privateNoStore, auth.RequireSession),
		http.MethodPatch: json(h.patchSettings, auth.RequireSession, csrf, requireJSON),
	})
	route("/api/auth/me/stats", sec, byMethod{
		http.MethodGet: json(h.getAccountStats, privateNoStore, auth.RequireSession),
	})
	// The setup page's `Check now` (ROADMAP #45, §20 erratum 297). Session
	// only and CSRF-guarded, like every other browser-driven POST here: a
	// capture token must not be able to spend a storage operation, and the
	// button lives on a page only a session (or a brand-new instance) reaches.
	// It takes NO body, so there is no requireJSON, and it carries NO limiter
	// of its own — the single-flight + 10 s cache in setup.go is what makes a
	// hundred presses one probe, which is a cache rather than a refusal
	// (the no-new-rate-limit rule, ROADMAP #35).
	route("/api/setup/check", sec, byMethod{
		http.MethodPost: json(h.setupCheck, auth.RequireSession, csrf),
	})
	// The Discord bot's interactions endpoint (ROADMAP #33, §20 errata
	// 253-256), registered ONLY when the three DISCORD_* variables are set:
	// with the bot off the path is a 404 from the catch-all, exactly like any
	// other unknown route, and nothing about the feature exists.
	//
	// Anonymous (Discord is the caller and has no session), NOT CSRF-guarded
	// (there is no browser), and no requireJSON — the endpoint validation
	// Discord performs when the URL is saved must reach the signature check,
	// not a 415 from a middleware. The credential is the Ed25519 signature
	// over the body, and nothing below it runs until that verifies.
	//
	// Its own limiter, keyed per IP like the auth one and with a burst of its
	// own (`interactionsPerMinute`): a slash command must not spend the tokens
	// that let people log in, and Discord's egress is a small set of addresses
	// that would otherwise share one bucket with every browser behind the same
	// proxy. The TimeoutHandler is on for the reason every short JSON route
	// has it — and this one has three seconds of Discord's patience to answer
	// inside, which is well under it.
	if h.discordConfigured() {
		route(interactionsPath, sec, byMethod{
			http.MethodPost: json(h.discordInteractions, botRate),
		})
		// The official bot's origin proof (§20 erratum 279, bot_claim.go):
		// one anonymous boolean per guild id, in its OWN bucket (279(h): a
		// stranger's GETs must not drain the bucket the official bot's
		// `/clip`s share), and it exists exactly when the interactions route
		// does.
		route(botClaimPath, sec, byMethod{
			http.MethodGet: json(h.botClaim, claimRate),
		})
	}
	// Sign in with Discord (ROADMAP #44, §20 erratum 302), registered ONLY when
	// the instance's own application is configured: with it off the three
	// paths are 404s from the catch-all, and /login shows no button.
	//
	// The two GETs are browser NAVIGATIONS — anonymous, no requireJSON, no
	// TimeoutHandler (the callback performs a two-call exchange with its own
	// budgets, and neither answers JSON) — and both carry the `auth` limiter,
	// because the callback performs an outbound exchange and a database write
	// (erratum 108's argument). The start is a plain anchor on purpose: a
	// `<form>` would meet CSP's form-action on its redirect to Discord. The
	// POST (link) runs a bcrypt compare, so it is session-only, CSRF-guarded,
	// JSON and in the `auth` bucket like PATCH /api/auth/password; the DELETE
	// (unlink) removes a capability and needs session + CSRF alone.
	if h.discordLoginConfigured() {
		route(discordLoginStartPath, sec, byMethod{
			http.MethodGet: long(h.discordStart, authRate),
		})
		route(discordLoginCallbackPath, sec, byMethod{
			http.MethodGet: long(h.discordCallback, authRate),
		})
		route(discordLoginPath, sec, byMethod{
			http.MethodPost:   json(h.discordLink, auth.RequireSession, csrf, requireJSON, authRate),
			http.MethodDelete: json(h.discordUnlink, auth.RequireSession, csrf),
		})
	}
	// oEmbed discovery (ROADMAP #18, §20 erratum 152). It is NOT under /api/
	// — the `<link rel="alternate">` a consumer follows names /oembed, which
	// is where every oEmbed provider puts it — so serve.go mounts this path on
	// the API handler explicitly. Anonymous like the share page it describes,
	// and under the `view` limiter for GET /api/collections/{cid}'s reason
	// (erratum 87): it is reachable by anyone holding a link and it costs a
	// row read per call.
	route("/oembed", sec, byMethod{
		http.MethodGet: json(h.oembed, viewRate),
	})

	// §14 (errata 19): in Phase 1 a Bearer token is an upload credential —
	// accepted only on GET /api/auth/me and POST /api/clips (with the upload
	// scope). Every other user/owner route requires a browser session, so a
	// leaked ShareX token cannot list, read-private, edit, delete, retry, or
	// download originals. RequireSession answers 403 token_not_allowed.
	//
	// §20 erratum 111 widens exactly two of those reads by exactly one scope:
	// GET /api/clips and GET /api/collections also accept a token minted with
	// `read`, which is what lets the owner's own backup script list the
	// owner's own library. An upload-only token still gets the same 403
	// token_not_allowed it always did.
	//
	// Every route that takes an upload body is wrapped OUTERMOST by
	// closeUnlessRead (§20 erratum 138): a refusal — no credential, the wrong
	// one, a tripped limiter, a body past MAX_UPLOAD_BYTES — is written
	// before a byte of that body has been read, and says `Connection: close`
	// so net/http does not then drain a gigabyte to keep alive a connection
	// whose request it just refused. beginUpload takes the header off again
	// for a request that IS going to be read.
	route("/api/clips", sec, byMethod{
		http.MethodGet:  json(h.listClips, privateNoStore, readable),
		http.MethodPost: closeUnlessRead(long(h.upload, auth.RequireUser, upload, csrf, uploadRate, uploadIPRate)),
	})
	// The chunked web upload (§20 erratum 344, uploads.go): one file as a run of
	// requests, each under the edge's per-request cap. The credential model,
	// the CSRF rule and the scope are POST /api/clips' exactly. The OPEN carries
	// both upload limiters — one upload is one token, however many chunks it
	// takes — and the requireJSON of a JSON route; the chunks, the finish, the
	// read and the abandon carry none (the no-new-limiter rule: every one of
	// them is bounded by a session the open already paid for). A chunk is an
	// upload body, so it is `long` and wrapped by closeUnlessRead like every
	// other one; the finish is `long` because the ingest it runs is the
	// one-shot's, which no TimeoutHandler wraps. Spelled, not named, for the
	// verification plane's route diff (see /api/instance above).
	route("/api/uploads", sec, byMethod{
		http.MethodPost: json(h.openUpload, auth.RequireUser, upload, csrf, requireJSON, uploadRate, uploadIPRate),
	})
	route("/api/uploads/{uid}", sec, byMethod{
		http.MethodGet:    json(h.getUpload, privateNoStore, auth.RequireUser, upload),
		http.MethodDelete: json(h.deleteUpload, auth.RequireUser, upload, csrf),
	})
	route("/api/uploads/{uid}/finish", sec, byMethod{
		http.MethodPost: long(h.finishUpload, auth.RequireUser, upload, csrf),
	})
	route("/api/uploads/{uid}/{index}", sec, byMethod{
		http.MethodPut: closeUnlessRead(long(h.putChunk, auth.RequireUser, upload, csrf)),
	})
	// The capture-tool endpoint (§8.1 Phase 4 row; §20 erratum 58). Same
	// credential model, same CSRF rule, the same PAIR of upload limiters and
	// the same ingest core as POST /api/clips — only the multipart grammar
	// and the response body differ, so a token cannot buy looser limits here.
	route("/api/upload", sec, byMethod{
		http.MethodPost: closeUnlessRead(long(h.captureUpload, auth.RequireUser, upload, csrf, uploadRate, uploadIPRate)),
	})
	// The inbound URL (§20 erratum 322, inbound.go): the capture endpoint for a
	// tool that cannot send a header, with the credential as the path's last
	// segment. NOT under the Resolver (no cookie or Authorization header is
	// read) and no CSRF check (no browser credential is in play). The credential
	// check is OUTERMOST, outside closeUnlessRead, so every miss is noRoute's
	// answer with nothing added; the account's upload bucket and the address
	// bucket run after it, so a 429 is only ever seen by a real URL's holder.
	// Spelled, not named, for the verification plane's route diff (see
	// /api/instance above).
	route("/in/{token}", sec, byMethod{
		http.MethodPost: h.scopedCredential(ids.IsInboundToken, auth.ScopeInbound, noRoute,
			closeUnlessRead(chain(http.HandlerFunc(h.inbound), uploadRate, uploadIPRate))),
	})
	// The Clippet Auto URL (§20 erratum 324, autofetch.go): the same credential
	// check with its own shape and scope, the same miss, the same pair of
	// limiters after it. It takes a small JSON instruction naming one Discord
	// attachment and never a file body. Two segments under /in/, so it can
	// collide neither with /in/{token} nor with a share id.
	//
	// Its second bucket is keyed by the URL (the token row), not by the
	// address (§20 erratum 327): every press reaches a box from Clippet Auto's
	// egress, so an address bucket here was one cap for every server's
	// presses at once.
	route("/in/auto/{token}", sec, byMethod{
		http.MethodPost: h.scopedCredential(ids.IsAutoFetchToken, auth.ScopeAutoFetch, noRoute,
			closeUnlessRead(chain(http.HandlerFunc(h.autofetch), uploadRate, ratelimit.Middleware(h.uploadIPLimit, h.tokenKey)))),
	})
	// The phone share-sheet target (ROADMAP #27, §20 erratum 137). The
	// capture endpoint's core with a browser's conventions: session ONLY (a
	// share sheet has no place to put a Bearer token, and a token must not be
	// able to spend an account's uploads through a route that answers with a
	// redirect), the same CSRF rule — which a share-sheet launch passes
	// because it carries no Origin and is not `cross-site` — the same pair of
	// upload limiters, and a 303 to the clip instead of a JSON body.
	// shareInterstitial sits above RequireSession so a signed-out phone gets
	// a page it can act on rather than an envelope it cannot read.
	route("/api/share", sec, byMethod{
		http.MethodPost: closeUnlessRead(long(h.shareTarget, shareInterstitial, auth.RequireSession, csrf, uploadRate, uploadIPRate)),
	})
	// The library's multi-select actions (§20 errata 117-119). A LITERAL
	// segment, so the Go 1.22 mux prefers it over /api/clips/{id} without
	// either pattern having to know about the other — and "bulk" could never
	// have been a clip id anyway: `bulk` is neither twelve base62 characters
	// nor three capitalised words, which is the whole of ids.Valid.
	// It is `long` for DELETE's reason: it destroys objects inline.
	route("/api/clips/bulk", sec, byMethod{
		http.MethodPost: long(h.bulkClips, auth.RequireSession, csrf, requireJSON),
	})
	// One poll for a whole page of the library instead of one per row (§20
	// erratum 116). Public like the single-clip progress route it replaces —
	// same DTO, same facts, same three terminal signals — and, unlike it,
	// under the `view` limiter, because one call can do a hundred times the
	// work (erratum 87's reasoning for the collection GET).
	route("/api/clips/progress", sec, byMethod{
		http.MethodGet: json(h.progressBatch, viewRate),
	})
	route("/api/clips/{id}", sec, byMethod{
		http.MethodGet:    json(h.getClip, privateNoStore),
		http.MethodPatch:  json(h.patchClip, auth.RequireSession, csrf, requireJSON),
		http.MethodDelete: long(h.deleteClip, auth.RequireSession, csrf),
	})
	route("/api/clips/{id}/retry", sec, byMethod{
		http.MethodPost: json(h.retryClip, auth.RequireSession, csrf),
	})
	// The TTL keep-alive (§8.1 Phase 4 row; §20 erratum 61). An owner route
	// shaped exactly like PATCH and retry: session only (a token gets 403
	// token_not_allowed), CSRF-guarded, bodiless, short enough for the
	// TimeoutHandler — whose preset no-store already covers the
	// owner-private DTO it answers with, as it does for PATCH.
	route("/api/clips/{id}/touch", sec, byMethod{
		http.MethodPost: json(h.touchClip, auth.RequireSession, csrf),
	})
	// Server-side trim (§8.1 Phase 4 rows; §20 errata 68+). The owner-route
	// conventions of PATCH/touch — session only, CSRF, the TimeoutHandler —
	// plus the `upload` limiter on the POST, because a trim spends the same
	// transcode slot an upload does — literally, since erratum 79: its ffmpeg
	// stages take a token from the TRANSCODE_CONCURRENCY semaphore. The GET is
	// session-only too: its `error` field names why a job failed, which is the
	// owner's to see and nobody else's.
	route("/api/clips/{id}/trim", sec, byMethod{
		http.MethodGet:  json(h.trimStatus, privateNoStore, auth.RequireSession),
		http.MethodPost: json(h.startTrim, auth.RequireSession, csrf, requireJSON, uploadRate),
	})
	// Clip replacement (ROADMAP #31, §20 errata 135–136). The owner-route
	// conventions again — session only (a capture token gets 403
	// token_not_allowed: a leaked ShareX credential must not be able to
	// overwrite the clips it once created), CSRF-guarded — plus BOTH upload
	// limiters and no TimeoutHandler, because the POST carries an upload body
	// and IS an upload in every way that costs the server something. The GET
	// is session-only for the trim status's reason: its `error` field is the
	// operator-facing detail of a failed job.
	route("/api/clips/{id}/replace", sec, byMethod{
		http.MethodGet:  json(h.replaceStatus, privateNoStore, auth.RequireSession),
		http.MethodPost: closeUnlessRead(long(h.replaceClip, auth.RequireSession, csrf, uploadRate, uploadIPRate)),
	})
	// The owner's explicit (re)generate of the preview GIF (§17 #6). Same
	// owner-route conventions as trim — session only, CSRF, the
	// TimeoutHandler — and the same `upload` limiter, because it queues an
	// ffmpeg run. It takes no body, so there is no requireJSON.
	route("/api/clips/{id}/gif", sec, byMethod{
		http.MethodPost: json(h.startGIF, auth.RequireSession, csrf, uploadRate),
	})
	// The owner's chosen card image (ROADMAP #5; §20 errata 127-130). The
	// trim's conventions exactly — session only, CSRF, JSON on the POST, the
	// TimeoutHandler, and the `upload` limiter because it queues an ffmpeg run
	// that takes a slot from the TRANSCODE_CONCURRENCY semaphore. The GET is
	// session-only for the trim's reason too: its `error` names why a job
	// failed, which is the owner's to see and nobody else's. The path is a
	// LITERAL segment beside `/poster.jpg`, which is the public media route
	// and is untouched by any of this.
	route("/api/clips/{id}/poster", sec, byMethod{
		http.MethodGet:  json(h.posterStatus, privateNoStore, auth.RequireSession),
		http.MethodPost: json(h.startPoster, auth.RequireSession, csrf, requireJSON, uploadRate),
	})
	// Collections (§8.1 Phase 4 rows; §20 erratum 82). The owner routes follow
	// the PATCH/touch/trim conventions exactly — session only (a token gets
	// 403 token_not_allowed, erratum 19), CSRF-guarded, under the
	// TimeoutHandler — and carry no limiter of their own: every one of them is
	// a handful of indexed statements, and the §14 hooks stay where the work
	// is (auth, upload, view). The single GET is PUBLIC, like GET
	// /api/clips/{id}: the unguessable id is the auth, and the body it returns
	// carries no owner-private field. It DOES carry the `view` limiter
	// (60/min/IP), which GET /api/clips/{id} does not: one call renders up to
	// 200 clip DTOs plus a tag query — around a hundred times the work and the
	// bytes of a single clip — and it is reachable by anyone holding the link
	// (§20 erratum 87). Sixty a minute is far above what curating or reading a
	// collection costs and far below what makes the route worth pointing at
	// something.
	route("/api/collections", sec, byMethod{
		http.MethodGet:  json(h.listCollections, privateNoStore, readable),
		http.MethodPost: json(h.createCollection, auth.RequireSession, csrf, requireJSON),
	})
	route("/api/collections/{cid}", sec, byMethod{
		http.MethodGet:    json(h.getCollection, viewRate),
		http.MethodPatch:  json(h.patchCollection, auth.RequireSession, csrf, requireJSON),
		http.MethodDelete: json(h.deleteCollection, auth.RequireSession, csrf),
	})
	route("/api/collections/{cid}/clips", sec, byMethod{
		http.MethodPut: json(h.putCollectionClips, auth.RequireSession, csrf, requireJSON),
	})
	// One clip at a time, beside the whole-membership PUT (ROADMAP #17, §20
	// erratum 151). Same conventions as its neighbour — session only, CSRF,
	// the TimeoutHandler, no limiter of its own — and no requireJSON, because
	// neither takes a body: the two ids in the path are the whole request.
	route("/api/collections/{cid}/clips/{id}", sec, byMethod{
		http.MethodPost:   json(h.addCollectionClip, auth.RequireSession, csrf),
		http.MethodDelete: json(h.removeCollectionClip, auth.RequireSession, csrf),
	})
	// The single-clip poll. It carries `pollRate` (120/min/IP) since the quality
	// tiers made it reachable by every viewer of a READY public clip whose better
	// tier is still encoding (§20 erratum 211, amended); before them only a queued
	// or processing clip polled, and such a clip is nobody's share link. Its own
	// bucket, not the `view` one its batched sibling shares — a poll must not
	// spend the tokens that count views and serve preview GIFs (httpapi.go's
	// pollsPerMinute).
	route("/api/clips/{id}/progress", sec, byMethod{
		http.MethodGet: json(h.progress, pollRate),
	})
	route("/api/clips/{id}/view", sec, byMethod{
		http.MethodPost: json(h.view, viewRate),
	})
	route("/api/clips/{id}/video.mp4", mediaHeaders, byMethod{http.MethodGet: http.HandlerFunc(h.video)})
	// An audio clip's best playable tier under its real media type (§20 erratum
	// 328): the stored original when it plays, the FLAC or AAC Full otherwise.
	// Public like `/video.mp4`, and like it outside the TimeoutHandler: it
	// streams. A video clip answers 404 here.
	route("/api/clips/{id}/audio", mediaHeaders, byMethod{http.MethodGet: http.HandlerFunc(h.audio)})
	// The LIGHT tier, under both its names (§20 erratum 210). `/light.mp4` is the
	// canonical one — it is what `tiers[]` lists and what the docs call it — and
	// `/social.mp4` is the name every card tag, every Discord message already
	// posted and every crawler cache keys on, so it keeps working and keeps being
	// what the tags point at. One handler, one object, two paths.
	route("/api/clips/{id}/light.mp4", mediaHeaders, byMethod{http.MethodGet: http.HandlerFunc(h.social)})
	route("/api/clips/{id}/social.mp4", mediaHeaders, byMethod{http.MethodGet: http.HandlerFunc(h.social)})
	route("/api/clips/{id}/poster.jpg", mediaHeaders, byMethod{http.MethodGet: http.HandlerFunc(h.poster)})
	// preview.gif is public like poster.jpg, but it is the ONE media route
	// that can start work, so it carries the `view` limiter (60/min/IP): an
	// anonymous caller may ask for a preview, and may not walk the id space
	// queueing them. It stays outside the TimeoutHandler with its siblings —
	// it streams a file when there is one.
	route("/api/clips/{id}/preview.gif", mediaHeaders, byMethod{
		http.MethodGet: chain(http.HandlerFunc(h.previewGIF), viewRate),
	})
	// filmstrip.jpg is preview.gif's twin (§20 erratum 220), with one arm of its
	// own (erratum 227): SERVING a sheet that exists is public like poster.jpg,
	// and MAKING one is the OWNER's — so the handler mounts the Resolver itself,
	// on the no-sheet path alone, instead of taking it from this chain. Resolving
	// it there and not here is what keeps a session cookie off the cacheable 200.
	// It keeps the `view` limiter for preview.gif's reason, which now bounds the
	// served bytes and the strangers' 404s as well as the owner's asking, and
	// stays outside the TimeoutHandler with its siblings: it streams a file when
	// there is one.
	route("/api/clips/{id}/filmstrip.jpg", mediaHeaders, byMethod{
		http.MethodGet: chain(http.HandlerFunc(h.filmstrip), viewRate),
	})
	// The uploaded bytes. It carries the credential RESOLVER and privateNoStore
	// exactly as before, but no longer auth.RequireSession: since the quality
	// tiers the answer depends on the ROW — a web-playable, untrimmed upload IS
	// what the watch page streams, so it is anonymous-readable, and everything
	// else stays owner-only (§20 erratum 207). A middleware cannot see a row, so
	// serveMedia writes all three refusals itself, in the same order and with the
	// same codes RequireSession wrote them: 401 anonymous, 403 token_not_allowed
	// for a Bearer token, 403 forbidden for somebody else's session.
	route("/api/clips/{id}/original", mediaHeaders, byMethod{
		http.MethodGet: long(h.original, privateNoStore),
	})

	handle("/", noRoute)
	return mux
}

// ipKey keys the per-IP limiters (auth, view) by the resolved client address.
func (h *handler) ipKey(r *http.Request) string {
	return clientip.Resolve(r, h.d.Cfg.TrustedProxies).String()
}

// userKey keys the per-user upload limiter by user id (RequireUser runs
// first, so the IP fallback only guards an unexpected composition). It is
// half the hook: the two upload routes also carry an ipKey'd bucket, because
// a per-account budget bounds nothing when accounts are free (§20 erratum 97).
func (h *handler) userKey(r *http.Request) string {
	if p, ok := auth.FromContext(r.Context()); ok {
		return "u:" + p.User.ID
	}
	return "ip:" + h.ipKey(r)
}

// tokenKey keys the Clippet Auto URL's second upload bucket by the credential's
// row (§20 erratum 327). scopedCredential runs first, so the address fallback
// (uploadIPRate's own key) only guards an unexpected composition.
func (h *handler) tokenKey(r *http.Request) string {
	if p, ok := auth.FromContext(r.Context()); ok && p.TokenID != "" {
		return "t:" + p.TokenID
	}
	return h.ipKey(r)
}

// --- shared error responses ---

// writeNotFound also fills in a MISSING Cache-Control, which is the media
// chain's case alone: mediaHeaders sets nosniff + the referrer policy and no
// directive at all, so an unknown clip's 404 on /poster.jpg, /video.mp4,
// /light.mp4, /preview.gif or /filmstrip.jpg used to leave it empty for a
// heuristic shared cache to store — while every SUCCESSFUL exit on those routes
// names one. Every other chain presets a directive (`timeout`'s `no-store`,
// `privateNoStore`'s `private, no-store`) and keeps it on its 404, so this
// never overwrites a decision somebody made.
func writeNotFound(w http.ResponseWriter) {
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	httpx.WriteError(w, http.StatusNotFound, "not_found", "no such clip")
}

func writeInvalid(w http.ResponseWriter, msg string) {
	httpx.WriteError(w, http.StatusBadRequest, "invalid_request", msg)
}

// writeDBError answers 503 unavailable + Retry-After for any database
// failure that is not a sentinel (§8: never 404 for a backend failure).
func writeDBError(w http.ResponseWriter, r *http.Request, err error) {
	if writeClientGone(w, r) {
		return
	}
	httpx.SetLogField(r, "db_error", err.Error())
	w.Header().Set("Retry-After", "5")
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteError(w, http.StatusServiceUnavailable, "unavailable", "database unavailable; retry shortly")
}

// writeStorageError answers 503 storage_unavailable.
func writeStorageError(w http.ResponseWriter, r *http.Request, err error) {
	if writeClientGone(w, r) {
		return
	}
	httpx.SetLogField(r, "storage_error", err.Error())
	w.Header().Set("Retry-After", "5")
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteError(w, http.StatusServiceUnavailable, "storage_unavailable", "storage unavailable; retry shortly")
}

// writeClientGone turns a backend call that failed only because the client
// disconnected into a 4xx (nobody reads it) so the access log stays at INFO
// (§12 doctrine) instead of recording a spurious 5xx.
func writeClientGone(w http.ResponseWriter, r *http.Request) bool {
	if r.Context().Err() == nil {
		return false
	}
	httpx.SetLogField(r, "client_disconnected", true)
	httpx.WriteError(w, http.StatusBadRequest, "client_disconnected", "client went away")
	return true
}

// lookupClip resolves {id}: malformed → 404 before any DB access; missing
// or deleting → 404; any other DB error → 503.
func (h *handler) lookupClip(w http.ResponseWriter, r *http.Request) (db.Clip, bool) {
	id := r.PathValue("id")
	if !validID(id) {
		writeNotFound(w)
		return db.Clip{}, false
	}
	if h.d.DB == nil {
		writeDBError(w, r, errNoDB)
		return db.Clip{}, false
	}
	c, err := h.d.DB.GetClip(r.Context(), id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeNotFound(w)
		} else {
			writeDBError(w, r, err)
		}
		return db.Clip{}, false
	}
	if c.State == db.ClipDeleting {
		writeNotFound(w)
		return db.Clip{}, false
	}
	return c, true
}

// ownedClip is lookupClip plus the owner check (403 after the 404, §8).
func (h *handler) ownedClip(w http.ResponseWriter, r *http.Request) (db.Clip, auth.Principal, bool) {
	c, ok := h.lookupClip(w, r)
	if !ok {
		return db.Clip{}, auth.Principal{}, false
	}
	p, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return db.Clip{}, auth.Principal{}, false
	}
	if p.User.ID != c.OwnerID {
		httpx.WriteError(w, http.StatusForbidden, "forbidden", "not the owner of this clip")
		return db.Clip{}, auth.Principal{}, false
	}
	return c, p, true
}

// unixNow returns the handler clock as unix seconds.
func (h *handler) unixNow() int64 { return h.now().Unix() }
