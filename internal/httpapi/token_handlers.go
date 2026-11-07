package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"clippet/internal/auth"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/ids"
	"clippet/internal/urls"
)

type createTokenRequest struct {
	Name string `json:"name"`
	// Scopes is the §20 erratum 111 addition. ABSENT (or null) means
	// `["upload"]` — the shape every capture tool and every existing client
	// mints, so erratum 19's wire contract is unchanged for anyone who never
	// heard of scopes. An EMPTY array is an error, not "no permissions": a
	// token that can do nothing is a mistake, not a request.
	Scopes []string `json:"scopes"`
}

// The mint's three rules — the name, the scopes and the ShareX file — are
// EXPORTED from this package (§20 erratum 308).
//
// There are two doors into a mint and only one set of rules. `POST
// /api/auth/tokens` needs a browser session, and the two situations that have
// none are exactly the ones a capture token is wanted for: a machine being set
// up by somebody who is not the account's owner, and a shell on a box whose
// browser cannot reach the instance. `clippet admin tokens mint` is that second
// door (cmd/clippet/admin.go), and it calls CleanTokenName, CleanScopes and
// NewSXCU rather than carrying its own copy of any of them — a CLI whose
// default scope set, name rule or `.sxcu` drifted from the route's would be a
// token that behaves differently depending on which door minted it.
//
// This package stays their home: it owns erratum 111's scope contract and
// erratum 59's file, cmd/clippet already imports it (doctor.go, serve.go), and
// moving either rule out would split one contract across two packages to save
// an import that is already there.

// errScopes is what CleanScopes returns for a set the mint cannot honour; the
// handler turns it into one 400 with a message that lists what IS accepted.
var errScopes = errors.New("httpapi: bad scopes")

// CleanTokenName applies the mint's name rule: trimmed, valid UTF-8, no
// control characters, 1–64 runes. An empty result is errTextEmpty rather than
// a nil error, so a caller that cannot call writeInvalidText still knows which
// rule was broken.
func CleanTokenName(s string) (string, error) {
	name, err := cleanText(s, maxTokenNameRunes)
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", errTextEmpty
	}
	return name, nil
}

// CleanScopes validates the requested scope set: known values only, at least
// one, duplicates collapsed, and rendered in the canonical auth.Scopes order
// so the stored column and every DTO read the same however the client sent it.
//
// One set is recognised BEFORE that loop (§20 erratum 322): `inbound`, alone
// (duplicates collapsed), is an inbound URL and comes back as ["inbound"].
// `inbound` with anything else falls through to the loop, where ValidScope
// refuses it — auth.Scopes stays [upload, read], so a mixed row is never one
// loop away. `autofetch` (§20 erratum 324) is recognised the same way: alone,
// a Clippet Auto URL; beside anything, refused by the loop.
func CleanScopes(req []string) ([]string, error) {
	if req == nil {
		return []string{auth.ScopeUpload}, nil
	}
	for _, s := range []string{auth.ScopeInbound, auth.ScopeAutoFetch} {
		if onlyScope(req, s) {
			return []string{s}, nil
		}
	}
	seen := make(map[string]bool, len(req))
	for _, s := range req {
		if !auth.ValidScope(s) {
			return nil, errScopes
		}
		seen[s] = true
	}
	out := make([]string, 0, len(auth.Scopes))
	for _, s := range auth.Scopes {
		if seen[s] {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, errScopes
	}
	return out, nil
}

// onlyScope reports whether req is the set {s}: non-empty, and every element
// s (duplicates collapse).
func onlyScope(req []string, s string) bool {
	if len(req) == 0 {
		return false
	}
	for _, v := range req {
		if v != s {
			return false
		}
	}
	return true
}

// IsInboundSet reports whether a CLEANED scope set is an inbound URL's.
// It is auth.IsInboundSet, the one spelling (run 8).
func IsInboundSet(scopes []string) bool { return auth.IsInboundSet(scopes) }

// IsAutoFetchSet reports whether a CLEANED scope set is a Clippet Auto URL's
// (§20 erratum 324). It is auth.IsAutoFetchSet.
func IsAutoFetchSet(scopes []string) bool { return auth.IsAutoFetchSet(scopes) }

// ScopesRule is the sentence both mint doors refuse a scope set with. Its
// Bearer list is auth.Scopes itself (run 8), so a third Bearer scope cannot
// leave it stale.
var ScopesRule = "scopes must be a non-empty subset of " + strings.Join(auth.Scopes, ", ") +
	"; or " + auth.ScopeInbound + " alone; or " + auth.ScopeAutoFetch + " alone"

// NewTokenValue mints the raw value for a CLEANED scope set — the one place
// both mint doors (POST /api/auth/tokens and `clippet admin tokens mint`)
// decide which kind of secret a row gets (§20 erratum 322). The inbound set
// gets a `ci_` secret and, as pathURL, the whole address a tool POSTs to,
// built from PUBLIC_BASE_URL (u) and never from a request's Host header; the
// autofetch set gets a `ca_` secret and its Clippet Auto URL the same way (§20
// erratum 324); every other set gets a `ck_` token and an empty pathURL.
func NewTokenValue(scopes []string, u urls.Builder) (raw, pathURL string) {
	switch {
	case IsInboundSet(scopes):
		raw = ids.InboundToken()
		return raw, u.Inbound(raw)
	case IsAutoFetchSet(scopes):
		raw = ids.AutoFetchToken()
		return raw, u.AutoFetch(raw)
	}
	return ids.Token(), ""
}

// createdTokenDTO is the one response that carries the raw token — and, for
// the same reason, the only place the ready-to-import ShareX file can be
// built (§20 erratum 59). URL is the inbound URL, present only for a row
// minted with the inbound set (§20 erratum 322), or the Clippet Auto URL for
// a row minted with the autofetch set (§20 erratum 324).
type createdTokenDTO struct {
	TokenDTO
	Token string `json:"token"`
	URL   string `json:"url,omitempty"`
	SXCU  *SXCU  `json:"sxcu"`
}

type tokenList struct {
	Tokens []TokenDTO `json:"tokens"`
}

// createToken mints an API token carrying the requested scopes for the
// session's user (§20 erratum 111). An absent or null `scopes` array means
// ["upload"], which is what every capture tool asks for and what this route
// minted before the read scope existed; an unknown scope, or an empty array,
// is a 400. The raw value is shown exactly once (§14), and the ShareX
// `sxcu` block is null for a token without the upload scope, because there
// is nothing for a capture tool to do with one. `["inbound"]` alone mints an
// inbound URL instead (§20 erratum 322): `token` is its `ci_` secret and
// `url` the whole address, built from PUBLIC_BASE_URL.
func (h *handler) createToken(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req createTokenRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name, err := CleanTokenName(req.Name)
	if err != nil {
		writeInvalidText(w, "name", "1-64 characters", err)
		return
	}
	scopes, err := CleanScopes(req.Scopes)
	if err != nil {
		writeInvalid(w, ScopesRule)
		return
	}
	raw, pathURL := NewTokenValue(scopes, h.d.URLs)
	tok := db.APIToken{
		UserID:    p.User.ID,
		Name:      name,
		TokenHash: auth.HashToken(raw),
		Prefix:    auth.TokenPrefixOf(raw),
		Scopes:    scopes,
		CreatedAt: h.unixNow(),
	}
	for attempt := 0; ; attempt++ {
		tok.ID = ids.New()
		err := h.d.DB.CreateToken(r.Context(), tok)
		if err == nil {
			break
		}
		if !errors.Is(err, db.ErrConflict) {
			writeDBError(w, r, err)
			return
		}
		if attempt >= 1 {
			httpx.WriteError(w, http.StatusInternalServerError, "id_collision", "could not allocate a token id; retry")
			return
		}
	}
	// The ShareX file is an UPLOAD destination, so it is minted only for a
	// token that can upload (§20 erratum 111). Handing a read-only token a
	// `.sxcu` would produce a destination that answers 403 on the first
	// screenshot — the import succeeds, the capture fails, and nothing on
	// either side says why.
	out := createdTokenDTO{TokenDTO: toTokenDTO(tok), Token: raw, URL: pathURL}
	if tok.HasScope(auth.ScopeUpload) {
		out.SXCU = NewSXCU(h.d.URLs, raw)
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
}

// listTokens returns the session user's live tokens, newest first.
func (h *handler) listTokens(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	toks, err := h.d.DB.ListTokens(r.Context(), p.User.ID)
	if err != nil {
		writeDBError(w, r, err)
		return
	}
	out := tokenList{Tokens: make([]TokenDTO, 0, len(toks))}
	for _, t := range toks {
		out.Tokens = append(out.Tokens, toTokenDTO(t))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// revokeToken sets revoked_at on the caller's own token.
func (h *handler) revokeToken(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	tid := r.PathValue("tid")
	if !validID(tid) {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "no such token")
		return
	}
	err := h.d.DB.RevokeToken(r.Context(), p.User.ID, tid, h.unixNow())
	switch {
	case errors.Is(err, db.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "no such token")
	case err != nil:
		writeDBError(w, r, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
