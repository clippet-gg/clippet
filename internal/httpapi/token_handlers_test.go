package httpapi

import (
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"clippet/internal/urls"
)

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func urlsFor(s string) urls.Builder { return urls.New(mustURL(s), "test") }

func TestTokenLifecycle(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")

	// Mint: raw token shown once, prefix and scopes reported.
	r := e.doJSON("POST", "/api/auth/tokens", map[string]string{"name": "ShareX on desktop"}, withCookie(cookie))
	if r.code != 201 {
		t.Fatalf("create: %d %s", r.code, r.body)
	}
	raw := r.str("token")
	if !strings.HasPrefix(raw, "ck_") || len(raw) != 46 || r.str("prefix") != raw[3:11] || r.str("name") != "ShareX on desktop" ||
		r.str("id") == "" || r.str("created_at") == "" || r.json["last_used_at"] != nil {
		t.Fatalf("create body: %s", r.body)
	}
	if scopes, _ := r.json["scopes"].([]any); len(scopes) != 1 || scopes[0] != "upload" {
		t.Fatalf("scopes: %v", r.json["scopes"])
	}
	if r.hdr.Get("Cache-Control") != "no-store" {
		t.Fatal("token responses must be no-store")
	}
	// The `sxcu` field IS NewSXCU's document for this token, said out loud so
	// the ONE generator is pinned from this side too: `clippet admin tokens
	// mint --sxcu` writes the same call's output to stdout (§20 erratum 308,
	// cmd/clippet/admin_tokens_mint_test.go), and nothing but a test on each
	// side keeps the two files one file.
	wantSXCU, err := json.Marshal(NewSXCU(urlsFor(testOrigin), raw))
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(wantSXCU, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.json["sxcu"], want) {
		t.Fatalf("the mint's sxcu is not NewSXCU's file:\n got %v\nwant %v", r.json["sxcu"], want)
	}
	id := r.str("id")

	// The token authenticates /me; /me is via token.
	if r = e.do("GET", "/api/auth/me", nil, withBearer(raw)); r.code != 200 || r.str("via") != "token" {
		t.Fatalf("me with token: %d %s", r.code, r.body)
	}
	// Tokens cannot mint or list tokens.
	if r = e.doJSON("POST", "/api/auth/tokens", map[string]string{"name": "x"}, withBearer(raw)); r.code != 403 || r.errCode() != "token_not_allowed" {
		t.Fatalf("token minting token: %d %s", r.code, r.body)
	}
	if r = e.do("GET", "/api/auth/tokens", nil, withBearer(raw)); r.code != 403 {
		t.Fatalf("token listing tokens: %d", r.code)
	}

	// List: the raw token is absent; last_used_at was bumped by the /me call.
	r = e.do("GET", "/api/auth/tokens", nil, withCookie(cookie))
	if r.code != 200 {
		t.Fatalf("list: %d %s", r.code, r.body)
	}
	toks, _ := r.json["tokens"].([]any)
	if len(toks) != 1 {
		t.Fatalf("list: %s", r.body)
	}
	tok := toks[0].(map[string]any)
	if tok["id"] != id || tok["last_used_at"] == nil || strings.Contains(string(r.body), raw) {
		t.Fatalf("list entry: %v", tok)
	}

	// Revoke: 204, then the token no longer authenticates and is not listed.
	if r = e.do("DELETE", "/api/auth/tokens/"+id, nil, withCookie(cookie)); r.code != 204 {
		t.Fatalf("revoke: %d %s", r.code, r.body)
	}
	if r = e.do("DELETE", "/api/auth/tokens/"+id, nil, withCookie(cookie)); r.code != 404 || r.errCode() != "not_found" {
		t.Fatalf("revoke again: %d %s", r.code, r.body)
	}
	if r = e.do("GET", "/api/auth/me", nil, withBearer(raw)); r.code != 401 {
		t.Fatalf("revoked token: %d", r.code)
	}
	r = e.do("GET", "/api/auth/tokens", nil, withCookie(cookie))
	if toks, _ := r.json["tokens"].([]any); len(toks) != 0 || !strings.Contains(string(r.body), `"tokens":[]`) {
		t.Fatalf("list after revoke: %s", r.body)
	}

	// Validation and ownership.
	if r = e.doJSON("POST", "/api/auth/tokens", map[string]string{"name": ""}, withCookie(cookie)); r.code != 400 {
		t.Fatalf("empty name: %d", r.code)
	}
	if r = e.doJSON("POST", "/api/auth/tokens", map[string]string{"name": strings.Repeat("n", 65)}, withCookie(cookie)); r.code != 400 {
		t.Fatalf("long name: %d", r.code)
	}
	if r = e.do("POST", "/api/auth/tokens", strings.NewReader(`{"name":"x"}`), withCookie(cookie)); r.code != 415 {
		t.Fatalf("non-json: %d", r.code)
	}
	if r = e.do("DELETE", "/api/auth/tokens/not-an-id", nil, withCookie(cookie)); r.code != 404 {
		t.Fatalf("bad id: %d", r.code)
	}
	r = e.doJSON("POST", "/api/auth/tokens", map[string]string{"name": "second"}, withCookie(cookie))
	otherID := r.str("id")
	_, otherCookie := e.newUser("other")
	if r = e.do("DELETE", "/api/auth/tokens/"+otherID, nil, withCookie(otherCookie)); r.code != 404 {
		t.Fatalf("revoking someone else's token: %d", r.code)
	}
	// Anonymous.
	if r = e.do("GET", "/api/auth/tokens", nil); r.code != 401 {
		t.Fatalf("anonymous list: %d", r.code)
	}
	// CSRF on minting.
	r = e.doJSON("POST", "/api/auth/tokens", map[string]string{"name": "x"}, withCookie(cookie), withHeader("Sec-Fetch-Site", "cross-site"))
	if r.code != 403 || r.errCode() != "csrf" {
		t.Fatalf("csrf mint: %d %s", r.code, r.body)
	}
}
