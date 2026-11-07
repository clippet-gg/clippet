package config

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// The clip webhook's configuration (ROADMAP #54, §20 erratum 319): both or
// none, the URL's rules, the key's rule, and that no message and no rendering
// carries any part of either value.

const (
	testHookPathToken = "pathtok-0123456789abcdef"
	testHookQuery     = "qtok-fedcba9876543210"
	testHookSecret    = "hook-secret-hook-secret-hook-secret-42"
)

func hookEnv(u string) map[string]string {
	return map[string]string{EnvClipWebhookURL: u, EnvClipWebhookSecret: testHookSecret}
}

func TestClipWebhookOffByDefault(t *testing.T) {
	c := mustLoad(t, nil)
	if c.ClipWebhook.Configured() || c.ClipWebhook.URL() != "" || c.ClipWebhook.Secret() != "" {
		t.Fatalf("the zero value carries something: %+v", c.ClipWebhook)
	}
	if s := c.ClipWebhook.String(); s != "(unset)" {
		t.Fatalf("String() = %q", s)
	}
}

func TestClipWebhookAccepts(t *testing.T) {
	for _, tc := range []struct{ in, scheme, host, redacted string }{
		{"https://automation.example.com/hooks/" + testHookPathToken, "https", "automation.example.com", "https://automation.example.com/(path redacted)"},
		{"http://automation.lan:5678/webhook?token=" + testHookQuery, "http", "automation.lan:5678", "http://automation.lan:5678/(path redacted)"},
		{"http://10.0.0.5:8123", "http", "10.0.0.5:8123", "http://10.0.0.5:8123"},
		{"https://Hooks.Example.com/", "https", "hooks.example.com", "https://hooks.example.com"},
		{"HTTP://h.example?x=" + testHookQuery, "http", "h.example", "http://h.example/(path redacted)"},
		// ASCII case alone (§20 erratum 327): a non-ASCII host is shown as it is
		// dialled, never folded into an ASCII one (U+0130 → "i" under ToLower).
		{"https://dİsc.Example/", "https", "dİsc.example", "https://dİsc.example"},
	} {
		c := mustLoad(t, hookEnv(tc.in))
		w := c.ClipWebhook
		if !w.Configured() || w.URL() != tc.in || w.Secret() != testHookSecret || w.Scheme() != tc.scheme || w.Host() != tc.host {
			t.Fatalf("%s: loaded %q %q %q", tc.in, w.Scheme(), w.Host(), w.URL())
		}
		if s := w.String(); s != tc.redacted {
			t.Fatalf("%s: String() = %q, want %q", tc.in, s, tc.redacted)
		}
	}
}

func TestClipWebhookRefusals(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://h.example/%zz" + testHookPathToken, "CLIP_WEBHOOK_URL: is not a valid URL (the value is a secret and is not echoed here)"},
		{"ftp://h.example/" + testHookPathToken, "CLIP_WEBHOOK_URL: scheme must be http or https"},
		// The two values swapped: the key's prefix would parse as a "scheme".
		{testHookSecret + ":" + testHookPathToken, "CLIP_WEBHOOK_URL: scheme must be http or https"},
		{"https:///" + testHookPathToken, "CLIP_WEBHOOK_URL: must name a host"},
		{"https://user:" + testHookPathToken + "@h.example/x", "CLIP_WEBHOOK_URL: must not contain userinfo (user:password@)"},
		{"https://h.example/" + testHookPathToken + "#frag", "CLIP_WEBHOOK_URL: must not carry a fragment"},
		{"https://h.example/" + testHookPathToken + "#", "CLIP_WEBHOOK_URL: must not carry a fragment"},
	} {
		err := loadErr(t, hookEnv(tc.in))
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error %v, want %q", tc.in, err, tc.want)
		}
		for _, leak := range []string{testHookPathToken, testHookSecret, "ftp", "zz"} {
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("%s: the error echoes %q: %v", tc.in, leak, err)
			}
		}
	}
}

func TestClipWebhookSecretRule(t *testing.T) {
	for _, bad := range []string{"short", strings.Repeat("a", 31), strings.Repeat("a", 20) + " " + strings.Repeat("b", 20)} {
		err := loadErr(t, map[string]string{EnvClipWebhookURL: "https://h.example/x", EnvClipWebhookSecret: bad})
		want := "CLIP_WEBHOOK_SECRET: must be at least 32 characters with no whitespace (the value is a secret and is not echoed here)"
		if !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), bad) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	c := mustLoad(t, map[string]string{EnvClipWebhookURL: "https://h.example/x", EnvClipWebhookSecret: strings.Repeat("a", 32)})
	if !c.ClipWebhook.Configured() {
		t.Fatal("a 32-character key was refused")
	}
}

func TestClipWebhookBothOrNeither(t *testing.T) {
	err := loadErr(t, map[string]string{EnvClipWebhookURL: "https://h.example/" + testHookPathToken})
	if want := "CLIP_WEBHOOK_SECRET: is unset but CLIP_WEBHOOK_URL is set: the clip webhook needs both, or neither"; !strings.Contains(err.Error(), want) {
		t.Fatalf("URL alone: %v", err)
	}
	if strings.Contains(err.Error(), testHookPathToken) {
		t.Fatalf("URL alone: the error echoes the URL: %v", err)
	}
	err = loadErr(t, map[string]string{EnvClipWebhookSecret: testHookSecret})
	if want := "CLIP_WEBHOOK_URL: is unset but CLIP_WEBHOOK_SECRET is set: the clip webhook needs both, or neither"; !strings.Contains(err.Error(), want) {
		t.Fatalf("secret alone: %v", err)
	}
	if strings.Contains(err.Error(), testHookSecret) {
		t.Fatalf("secret alone: the error echoes the key: %v", err)
	}
}

// TestClipWebhookNeverRenders: %v, %+v and slog of the whole Config show the
// redacted form — never the path, the query or the key.
func TestClipWebhookNeverRenders(t *testing.T) {
	c := mustLoad(t, hookEnv("https://h.example/hooks/"+testHookPathToken+"?k="+testHookQuery))
	var b strings.Builder
	slog.New(slog.NewTextHandler(&b, nil)).Info("cfg", "hook", c.ClipWebhook, "all", c)
	for _, s := range []string{fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprint(c.ClipWebhook), b.String()} {
		for _, leak := range []string{testHookPathToken, testHookQuery, testHookSecret, "/hooks"} {
			if strings.Contains(s, leak) {
				t.Fatalf("a rendering leaks %q: %s", leak, s)
			}
		}
	}
	if !strings.Contains(b.String(), "https://h.example/(path redacted)") {
		t.Fatalf("slog did not use the redacted form: %s", b.String())
	}
}
