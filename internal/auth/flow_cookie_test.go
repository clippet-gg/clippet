package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"clippet/internal/config"
)

// The Discord sign-in flow cookie (ROADMAP #44, §20 erratum 302): the session
// cookie's flags, the session cookie's name plus a suffix (so a `__Host-`
// prefix is inherited), ten minutes, Lax, and a clear that drops a pending set.
func TestFlowCookieFlags(t *testing.T) {
	for _, tc := range []struct {
		session, want string
		https         bool
	}{
		{"sess", "sess_discord", false},
		{"__Host-clippet_session", "__Host-clippet_session_discord", true},
		{"", DefaultCookieName + "_discord", false},
	} {
		cfg := config.Config{SessionCookie: tc.session}
		if got := FlowCookieName(cfg); got != tc.want {
			t.Fatalf("FlowCookieName(%q) = %q, want %q", tc.session, got, tc.want)
		}
		rec := httptest.NewRecorder()
		SetFlowCookie(rec, cfg, tc.https, "nonce.payload")
		cs := rec.Result().Cookies()
		if len(cs) != 1 {
			t.Fatalf("%d cookies", len(cs))
		}
		c := cs[0]
		if c.Name != tc.want || c.Value != "nonce.payload" || c.Path != "/" || !c.HttpOnly ||
			c.SameSite != http.SameSiteLaxMode || c.MaxAge != 600 || c.Secure != tc.https || c.Domain != "" {
			t.Fatalf("flow cookie %+v", c)
		}
		// A clear on the same response drops the set and leaves ONE expiring
		// cookie (a fresh recorder: Result() is a snapshot the recorder caches).
		rec = httptest.NewRecorder()
		SetFlowCookie(rec, cfg, tc.https, "nonce.payload")
		ClearFlowCookie(rec, cfg, tc.https)
		cs = rec.Result().Cookies()
		if len(cs) != 1 {
			t.Fatalf("after the clear: %d cookies (%v)", len(cs), cs)
		}
		c = cs[0]
		if c.Name != tc.want || c.Value != "" || c.MaxAge != -1 || c.Secure != tc.https || !c.HttpOnly || c.Path != "/" {
			t.Fatalf("cleared flow cookie %+v", c)
		}
	}
	// The clear never touches the SESSION cookie queued on the same response.
	rec := httptest.NewRecorder()
	cfg := config.Config{SessionCookie: "sess"}
	SetSessionCookie(rec, cfg, false, "raw", DefaultSessionTTL)
	ClearFlowCookie(rec, cfg, false)
	names := map[string]int{}
	for _, c := range rec.Result().Cookies() {
		names[c.Name]++
	}
	if names["sess"] != 1 || names["sess_discord"] != 1 {
		t.Fatalf("cookies after a clear beside a session set: %v", names)
	}
}
