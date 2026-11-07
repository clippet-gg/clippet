package main

import (
	"net/http"
	"strings"
	"testing"
)

// `clippet admin tokens mint <user> --scopes autofetch` (§20 erratum 324):
// stdout is the Clippet Auto URL on this instance's own PUBLIC_BASE_URL and
// nothing else; stderr is inbound's line with `ca_<prefix>…` and
// `scopes=autofetch` and never the secret; `tokens list` prints `ca_`; the
// value is no Bearer credential; a mixed set and --sxcu are refused before
// anything is minted.
func TestTokensMintAutoFetch(t *testing.T) {
	fx := newAdminFixture(t)

	code, out, errOut := fx.mint(t, "carol", "--scopes", "autofetch", "--name", "Discord server")
	if code != 0 {
		t.Fatalf("mint --scopes autofetch: %d\n%s\n%s", code, out, errOut)
	}
	url := strings.TrimSuffix(out, "\n")
	prefix := fx.cfg.PublicBaseURL.String() + "/in/auto/"
	if strings.Contains(url, "\n") || !strings.HasPrefix(url, prefix) {
		t.Fatalf("stdout must be the Clippet Auto URL and nothing else, got %q", out)
	}
	raw := strings.TrimPrefix(url, prefix)
	if !strings.HasPrefix(raw, "ca_") || len(raw) != 46 {
		t.Fatalf("the URL's secret is %q", raw)
	}
	if strings.Contains(errOut, raw) || strings.Contains(errOut, raw[3:]) {
		t.Fatalf("the secret reached stderr: %q", errOut)
	}
	if want := `: ca_` + raw[3:11] + "…" + ` name="Discord server" scopes=autofetch`; !strings.Contains(errOut, want) {
		t.Errorf("stderr line 1 must read %q: %q", want, errOut)
	}
	if !strings.Contains(errOut, "the URL on stdout is shown ONCE; anyone who has it can upload as carol. Revoke it with: clippet admin tokens revoke carol ") {
		t.Errorf("stderr line 2: %q", errOut)
	}
	list := fx.tokenCount(t)
	if !strings.Contains(list, "ca_"+raw[3:11]+"…") || !strings.Contains(list, "scopes=autofetch") || strings.Contains(list, raw) {
		t.Fatalf("tokens list must show the row as ca_… and never the secret:\n%s", list)
	}
	if got := fx.uploadAuth(t, raw); got != http.StatusUnauthorized {
		t.Fatalf("Bearer ca_… on POST /api/upload: %d, want 401", got)
	}

	before := fx.tokenCount(t)
	rule := "admin tokens mint: --scopes must be a non-empty subset of upload, read; or inbound alone; or autofetch alone"
	for _, tc := range []struct {
		args []string
		says string
	}{
		{[]string{"carol", "--scopes", "autofetch,upload"}, rule},
		{[]string{"carol", "--scopes", "inbound,autofetch"}, rule},
		{[]string{"carol", "--scopes", "autofetch", "--sxcu"}, "admin tokens mint: --sxcu needs the upload scope; a ShareX destination that cannot upload answers 403 on the first capture"},
	} {
		code, out, errOut := fx.mint(t, tc.args...)
		if code != 2 || out != "" || !strings.Contains(errOut, tc.says) {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", tc.args, code, out, errOut)
		}
	}
	if after := fx.tokenCount(t); countLine(after) != countLine(before) {
		t.Errorf("a refused mint created a row:\n%s\n%s", before, after)
	}
}
