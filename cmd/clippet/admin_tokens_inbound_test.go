package main

import (
	"net/http"
	"strings"
	"testing"
)

// `clippet admin tokens mint <user> --scopes inbound` (§20 erratum 322): stdout
// is the inbound URL on this instance's own PUBLIC_BASE_URL and nothing else,
// stderr names the row by its `ci_` display prefix and never carries the
// secret, `tokens list` prints `ci_`, the value is no Bearer credential, and a
// mixed set or --sxcu is refused before anything is minted.
func TestTokensMintInbound(t *testing.T) {
	fx := newAdminFixture(t)

	code, out, errOut := fx.mint(t, "carol", "--scopes", "inbound", "--name", "Stream deck")
	if code != 0 {
		t.Fatalf("mint --scopes inbound: %d\n%s\n%s", code, out, errOut)
	}
	url := strings.TrimSuffix(out, "\n")
	prefix := fx.cfg.PublicBaseURL.String() + "/in/"
	if strings.Contains(url, "\n") || !strings.HasPrefix(url, prefix) {
		t.Fatalf("stdout must be the inbound URL and nothing else, got %q", out)
	}
	raw := strings.TrimPrefix(url, prefix)
	if !strings.HasPrefix(raw, "ci_") || len(raw) != 46 {
		t.Fatalf("the URL's secret is %q", raw)
	}
	if strings.Contains(errOut, raw) || strings.Contains(errOut, raw[3:]) {
		t.Fatalf("the secret reached stderr: %q", errOut)
	}
	wantLine := `: ci_` + raw[3:11] + "…" + ` name="Stream deck" scopes=inbound`
	if !strings.Contains(errOut, wantLine) {
		t.Errorf("stderr line 1 must read %q: %q", wantLine, errOut)
	}
	if !strings.Contains(errOut, "the URL on stdout is shown ONCE; anyone who has it can upload as carol. Revoke it with: clippet admin tokens revoke carol ") {
		t.Errorf("stderr line 2: %q", errOut)
	}
	list := fx.tokenCount(t)
	if !strings.Contains(list, "ci_"+raw[3:11]+"…") || !strings.Contains(list, "scopes=inbound") || strings.Contains(list, raw) {
		t.Fatalf("tokens list must show the row as ci_… and never the secret:\n%s", list)
	}
	// Not a Bearer credential: the upload route's own chain is anonymous for it.
	if got := fx.uploadAuth(t, raw); got != http.StatusUnauthorized {
		t.Fatalf("Bearer ci_… on POST /api/upload: %d, want 401", got)
	}

	before := fx.tokenCount(t)
	for _, tc := range []struct {
		args []string
		says string
	}{
		{[]string{"carol", "--scopes", "inbound,upload"}, "admin tokens mint: --scopes must be a non-empty subset of upload, read; or inbound alone"},
		{[]string{"carol", "--scopes", "read,inbound"}, "admin tokens mint: --scopes must be a non-empty subset of upload, read; or inbound alone"},
		{[]string{"carol", "--scopes", "inbound", "--sxcu"}, "admin tokens mint: --sxcu needs the upload scope; a ShareX destination that cannot upload answers 403 on the first capture"},
	} {
		code, out, errOut := fx.mint(t, tc.args...)
		if code != 2 || out != "" || !strings.Contains(errOut, tc.says) {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", tc.args, code, out, errOut)
		}
	}
	if after := fx.tokenCount(t); countLine(after) != countLine(before) {
		t.Errorf("a refused mint created a row:\n%s\n%s", before, after)
	}
	// Duplicates collapse to the one inbound set.
	if code, out, errOut := fx.mint(t, "carol", "--scopes", "inbound,inbound"); code != 0 || !strings.Contains(out, "/in/ci_") {
		t.Errorf("--scopes inbound,inbound: %d %q %q", code, out, errOut)
	}
}
