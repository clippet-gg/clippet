package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"clippet/internal/auth"
	"clippet/internal/db"
	"clippet/internal/httpx"
)

// POST /in/{token} — the inbound URL (§20 erratum 322; ROADMAP #54 pick 5).
//
// A tool that can only POST a file to an address — a stream deck, an OBS
// script, anything with no place for an Authorization header — gets a URL
// whose last segment IS the credential: an api_tokens row holding the scope
// `inbound` alone, with a `ci_` secret. Past the credential it is POST
// /api/upload exactly: the same capture grammar, the same pair of upload
// limiters, the same quota and size cap, the same ingest and the same wait
// (h.capture). FILE BODIES ONLY: there is no field that makes the server
// fetch an address.
//
// What it can do is bounded three times over. The Resolver never sees the
// secret (bearerToken admits only `ck_`, and resolveToken refuses a row holding
// `inbound`); this route reads no cookie and no Authorization header (it is not
// under the Resolver at all); and the Principal it installs is a TOKEN
// principal holding only `inbound`, which every existing gate refuses
// (RequireSession, RequireScope, OwnerReader) — so it fails closed if it is
// ever composed with one by mistake.
//
// "A wrong token answers exactly like a missing route." Every miss — a value
// of the wrong shape, an unknown or revoked one, a `ck_` token, a row whose
// scopes are not exactly [inbound] — is answered by the SAME handler value the
// API's catch-all is registered with, and before closeUnlessRead has run, so
// the status, the headers and the body are those of POST /api/nope byte for
// byte (a Go test diffs them, with a body over the close threshold and a
// chunked one too). The limiters run AFTER the credential, so a 429 is only
// ever seen by the holder of a real URL.

// scopedCredential resolves the path's secret into a principal for next, or
// answers miss. It is the ONE credential check of the two path credentials
// (§20 errata 322, 324): `shape` is the secret's shape (ids.IsInboundToken or
// ids.IsAutoFetchToken) and `scope` the one scope the row must hold — exactly,
// `len == 1` — so neither route's secret opens the other, and a hand-written
// row holding either beside anything else is a miss on both. A database
// failure is the usual 503, which no token value can select: only a value of
// the right shape is ever looked up.
func (h *handler) scopedCredential(shape func(string) bool, scope string, miss, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.PathValue("token")
		if !shape(raw) {
			miss.ServeHTTP(w, r)
			return
		}
		tok, user, err := h.d.DB.GetTokenByHash(r.Context(), auth.HashToken(raw))
		switch {
		case errors.Is(err, db.ErrNotFound):
			miss.ServeHTTP(w, r)
			return
		case err != nil:
			writeDBError(w, r, err)
			return
		case !auth.IsOnly(tok.Scopes, scope):
			miss.ServeHTTP(w, r)
			return
		}
		// The row id is not a secret; the log's path is already /in/{token}.
		// The field is named by the scope: `inbound=` or `autofetch=`.
		httpx.SetLogField(r, scope, tok.ID)
		p := auth.Principal{User: user, Via: auth.ViaToken, TokenID: tok.ID, Scopes: []string{scope}}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
}

// inbound is the route's handler, past the credential and both limiters. The
// last_used_at write happens HERE rather than in the credential check, so a
// holder of a leaked URL who is already being answered 429 buys no database
// write per request. It is bookkeeping: a failed touch fails nothing.
//
// The answer is the share link as one line of text — what a tool that can only
// POST a file can do something with — or, when Accept names application/json,
// the UploadDTO POST /api/upload answers. A duplicate (§20 erratum 326) is the
// same two shapes with 200 instead of 201 — the existing clip's link, and in
// JSON `"existing": true`.
func (h *handler) inbound(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	_ = h.d.DB.TouchToken(r.Context(), p.TokenID, h.unixNow())
	clip, existing, ok := h.capture(w, r)
	if !ok {
		return
	}
	if acceptsJSON(r) {
		httpx.WriteJSON(w, uploadStatus(existing), uploadDTOFor(clip, existing, h.d.URLs))
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "text/plain; charset=utf-8")
	hd.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(uploadStatus(existing))
	_, _ = io.WriteString(w, h.d.URLs.Page(clip.ID)+"\n")
}

// acceptsJSON reports whether the Accept header NAMES application/json. A
// wildcard does not count: the text answer is this route's default. Nor does
// `application/json;q=0`, which is RFC 9110's "not acceptable" (run 8).
func acceptsJSON(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept") {
		for _, part := range strings.Split(v, ",") {
			mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err == nil && mt == "application/json" && !zeroQ(params["q"]) {
				return true
			}
		}
	}
	return false
}

// zeroQ reports whether a quality value is zero (`0`, `0.0`, `0.000`).
func zeroQ(q string) bool {
	f, err := strconv.ParseFloat(strings.TrimSpace(q), 64)
	return err == nil && f == 0
}
