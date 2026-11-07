package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"clippet/internal/auth"
	"clippet/internal/config"
	"clippet/internal/httpapi"
	"clippet/internal/store"
	"clippet/internal/urls"
)

// `clippet admin tokens mint` — the capture-token mint with no browser behind
// it (§20 erratum 308). Every test here reads stdout and stderr SEPARATELY,
// because which stream a thing is written to is the verb's contract: stdout is
// the artefact (the raw token, or the ShareX file), stderr is for people, and
// the raw value appears on stdout exactly once and nowhere else.

// mint runs the verb and returns the exit code, stdout and stderr apart.
func (fx *adminFixture) mint(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	fx.out.Reset()
	fx.errOut.Reset()
	code := runAdminWith(append([]string{"tokens", "mint"}, args...), fx.cfg, adminEnv{
		stdout:    &fx.out,
		stderr:    &fx.errOut,
		stdin:     strings.NewReader(""),
		openStore: func(config.Config) (store.Store, error) { return fx.fake, nil },
		now:       func() time.Time { return fx.now },
	})
	return code, fx.out.String(), fx.errOut.String()
}

// uploadAuth drives the exact middleware chain POST /api/upload is wrapped in
// (router.go: auth.RequireUser then auth.RequireScope(auth.ScopeUpload), under
// the Resolver) against one bearer token, and returns the status. It is the
// only honest way to say "this mint produces a token the upload route accepts"
// from this package: the answer must come from internal/auth's own resolver
// reading the row the CLI wrote, not from a lookup this test invents.
func (fx *adminFixture) uploadAuth(t *testing.T, raw string) int {
	t.Helper()
	d, err := openForTest(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	res := &auth.Resolver{DB: d, Cfg: fx.cfg, Now: func() time.Time { return fx.now }}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.FromContext(r.Context())
		if !ok || p.Via != auth.ViaToken || p.User.Username != "carol" {
			t.Errorf("the principal on the request is %+v, want carol via token", p)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	h := res.Middleware(auth.RequireUser(auth.RequireScope(auth.ScopeUpload)(inner)))
	req := httptest.NewRequest(http.MethodPost, "/api/upload", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// tokenCount is how many live tokens `carol` has, read through the same verb an
// operator would use.
func (fx *adminFixture) tokenCount(t *testing.T) string {
	t.Helper()
	code, out := fx.run(t, "tokens", "list", "carol")
	if code != 0 {
		t.Fatalf("tokens list: %d %s", code, out)
	}
	return out
}

// TestTokensMintIsACaptureToken: the default mint, end to end — the raw value
// on stdout and only there, the row `tokens list` and `tokens revoke` see, and
// the credential POST /api/upload accepts and then stops accepting.
func TestTokensMintIsACaptureToken(t *testing.T) {
	fx := newAdminFixture(t)

	code, out, errOut := fx.mint(t, "carol")
	if code != 0 {
		t.Fatalf("mint: %d\n%s\n%s", code, out, errOut)
	}
	raw := strings.TrimSuffix(out, "\n")
	if strings.Contains(raw, "\n") || !strings.HasPrefix(raw, "ck_") || len(raw) != 46 {
		t.Fatalf("stdout must be the token and nothing else, got %q", out)
	}
	// The whole point of one stream per purpose: nothing a person reads may
	// carry the credential, here or in any log this program could grow.
	if strings.Contains(errOut, raw) {
		t.Fatalf("the raw token reached stderr: %q", errOut)
	}
	if !strings.Contains(errOut, "ck_"+raw[3:11]+"\u2026") {
		t.Errorf("stderr must name the token by its display prefix: %q", errOut)
	}
	if !strings.Contains(errOut, "scopes=upload") {
		t.Errorf("the default scope set must be upload, as the route's absent `scopes` array is: %q", errOut)
	}

	// The route's own auth chain accepts it.
	if got := fx.uploadAuth(t, raw); got != http.StatusNoContent {
		t.Fatalf("POST /api/upload with the minted token: %d, want 204", got)
	}

	// It is an ORDINARY token: listed like any other, revocable like any other.
	list := fx.tokenCount(t)
	if !strings.Contains(list, "ck_"+raw[3:11]+"\u2026") || !strings.Contains(list, `name="shell"`) {
		t.Fatalf("tokens list does not show the minted token:\n%s", list)
	}
	if strings.Contains(list, raw) {
		t.Fatalf("tokens list printed the raw token:\n%s", list)
	}
	var id string
	for _, line := range strings.Split(list, "\n") {
		if strings.Contains(line, "ck_"+raw[3:11]+"\u2026") {
			id = strings.Fields(line)[0]
		}
	}
	if id == "" {
		t.Fatalf("no id for the minted token in:\n%s", list)
	}
	if code, out := fx.run(t, "tokens", "revoke", "carol", id); code != 0 || !strings.Contains(out, "revoked 1 token") {
		t.Fatalf("revoke: %d %s", code, out)
	}
	if got := fx.uploadAuth(t, raw); got != http.StatusUnauthorized {
		t.Fatalf("POST /api/upload with the revoked token: %d, want 401", got)
	}
}

// TestTokensMintScopes: --scopes is the route's `scopes` array, with the route's
// rules — the canonical order, duplicates collapsed, and a set outside the
// closed vocabulary refused rather than stored.
func TestTokensMintScopes(t *testing.T) {
	fx := newAdminFixture(t)

	code, out, errOut := fx.mint(t, "carol", "--scopes", "read,upload,read")
	if code != 0 {
		t.Fatalf("mint: %d %s %s", code, out, errOut)
	}
	if !strings.Contains(errOut, "scopes=upload+read") {
		t.Errorf("scopes must render in the canonical order with duplicates gone: %q", errOut)
	}
	raw := strings.TrimSpace(out)
	if got := fx.uploadAuth(t, raw); got != http.StatusNoContent {
		t.Fatalf("a token with both scopes on POST /api/upload: %d", got)
	}

	// A read-only token is refused by the upload route, which is the whole
	// reason the scope exists.
	if _, out, _ := fx.mint(t, "carol", "--scopes", "read"); true {
		if got := fx.uploadAuth(t, strings.TrimSpace(out)); got != http.StatusForbidden {
			t.Fatalf("a read-only token on POST /api/upload: %d, want 403", got)
		}
	}

	before := fx.tokenCount(t)
	for _, bad := range []string{"admin", "", "upload,root", "upload,", ","} {
		code, out, errOut := fx.mint(t, "carol", "--scopes", bad)
		if code != 2 {
			t.Errorf("--scopes %q: %d, want 2", bad, code)
		}
		if out != "" {
			t.Errorf("--scopes %q wrote to stdout: %q", bad, out)
		}
		if !strings.Contains(errOut, "non-empty subset") {
			t.Errorf("--scopes %q: %q", bad, errOut)
		}
	}
	if after := fx.tokenCount(t); countLine(after) != countLine(before) {
		t.Errorf("a refused mint created a token:\n%s\n%s", before, after)
	}
}

// TestTokensMintSXCUIsTheRouteSFile: `--sxcu` writes the byte-for-byte document
// `POST /api/auth/tokens` puts in its `sxcu` field for the same token, built by
// the same generator (§20 errata 59, 308). Two generators of one file is how a
// capture tool comes to work from one door and not the other.
func TestTokensMintSXCUIsTheRouteSFile(t *testing.T) {
	fx := newAdminFixture(t)

	code, out, errOut := fx.mint(t, "carol", "--name", "the gaming pc", "--sxcu")
	if code != 0 {
		t.Fatalf("mint --sxcu: %d\n%s\n%s", code, out, errOut)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not the JSON file ShareX imports: %v\n%s", err, out)
	}
	hdrs, _ := got["Headers"].(map[string]any)
	bearer, _ := hdrs["Authorization"].(string)
	raw := strings.TrimPrefix(bearer, "Bearer ")
	if !strings.HasPrefix(raw, "ck_") || len(raw) != 46 {
		t.Fatalf("the file carries no credential: %s", out)
	}

	// The expectation is the ROUTE's expression — newSXCU(h.d.URLs, raw), where
	// h.d.URLs is urls.New(cfg.PublicBaseURL, buildID) — for this same token. A
	// DIFFERENT build id is passed on purpose: the `?v=` cache-buster must not
	// reach this file, so the two documents have to agree without it.
	b, err := json.Marshal(httpapi.NewSXCU(urls.New(fx.cfg.PublicBaseURL, "test"), raw))
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the shell's .sxcu is not the route's:\n got %v\nwant %v", got, want)
	}
	if got["RequestURL"] != fx.cfg.PublicBaseURL.String()+"/api/upload" {
		t.Errorf("RequestURL = %v, want this instance's own upload route", got["RequestURL"])
	}
	// The token is on stdout inside the file, and nowhere else.
	if strings.Contains(errOut, raw) {
		t.Fatalf("the raw token reached stderr: %q", errOut)
	}
	if !strings.Contains(errOut, `name="the gaming pc"`) {
		t.Errorf("stderr must name the token: %q", errOut)
	}
	// It is a working credential, not just a well-shaped file.
	if code := fx.uploadAuth(t, raw); code != http.StatusNoContent {
		t.Fatalf("the token in the .sxcu on POST /api/upload: %d", code)
	}
}

// TestTokensMintRefusals: everything this verb says no to, and the fact that
// each no leaves the database exactly as it was.
func TestTokensMintRefusals(t *testing.T) {
	fx := newAdminFixture(t)
	before := fx.tokenCount(t)

	for _, tc := range []struct {
		name string
		args []string
		code int
		says string
	}{
		{"no username", nil, 2, "usage:"},
		{"a blank username", []string{"   "}, 2, "usage:"},
		{"two usernames", []string{"carol", "friend"}, 2, "usage:"},
		{"an unknown user", []string{"nobody"}, 1, "no such user"},
		// A ShareX destination without the upload scope imports cleanly and
		// answers 403 on the first capture, so it is refused BEFORE a token
		// exists rather than written as a file nobody can use.
		{"--sxcu on a read-only token", []string{"carol", "--scopes", "read", "--sxcu"}, 2, "answers 403"},
		// One message for the whole name rule: an operator fixing a name does
		// not need it split into three.
		{"an empty --name", []string{"carol", "--name", "  "}, 2, "--name must be"},
		{"a --name with a control character", []string{"carol", "--name", "a\x1bb"}, 2, "--name must be"},
		{"a --name of 65 runes", []string{"carol", "--name", strings.Repeat("n", 65)}, 2, "--name must be"},
		{"a --name that is not UTF-8", []string{"carol", "--name", "a\xffb"}, 2, "--name must be"},
	} {
		code, out, errOut := fx.mint(t, tc.args...)
		if code != tc.code {
			t.Errorf("%s: exit %d, want %d (%s)", tc.name, code, tc.code, errOut)
		}
		if out != "" {
			t.Errorf("%s wrote to stdout: %q", tc.name, out)
		}
		if !strings.Contains(errOut, tc.says) {
			t.Errorf("%s: %q does not say %q", tc.name, errOut, tc.says)
		}
	}
	if after := fx.tokenCount(t); countLine(after) != countLine(before) {
		t.Errorf("a refusal minted a token:\n%s\n%s", before, after)
	}
	// A 64-rune name is the boundary on the other side: accepted.
	if code, _, errOut := fx.mint(t, "carol", "--name", strings.Repeat("n", 64)); code != 0 {
		t.Errorf("a 64-rune name: %d %s", code, errOut)
	}
	// And the rule is the ROUTE's, no stricter and no looser: cleanText lets
	// \n, \t and \r through (a description goes through it too), so a name
	// carrying one is a name POST /api/auth/tokens would have stored as well.
	// What keeps it from breaking a listing is that both `tokens list` and
	// this verb print a name through %q: one token is one line whatever is in
	// it, and no escape sequence a name carries reaches a terminal raw.
	if code, _, errOut := fx.mint(t, "carol", "--name", "two\nlines"); code != 0 {
		t.Errorf("a name with a newline is the route's rule too: %d %s", code, errOut)
	} else if !strings.Contains(errOut, `name="two\nlines"`) {
		t.Errorf("the confirmation must quote the name: %q", errOut)
	}
	if list := fx.tokenCount(t); !strings.Contains(list, `name="two\nlines"`) {
		t.Errorf("tokens list must quote the name: %q", list)
	}
}

// countLine is the "N live token(s) for carol" line of `tokens list`, which is
// what a refusal must not move.
func countLine(listing string) string {
	for _, l := range strings.Split(listing, "\n") {
		if strings.Contains(l, "live token(s) for") {
			return l
		}
	}
	return ""
}
