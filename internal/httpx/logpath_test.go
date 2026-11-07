package httpx

import (
	"bufio"
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clippet/internal/ids"
)

// LogPath (§20 erratum 322): the inbound URL's secret never reaches a log line,
// whichever handler the path ended up at.
func TestLogPath(t *testing.T) {
	sec := ids.InboundToken()
	cases := map[string]string{
		"/in/" + sec:              "/in/{token}",
		"//in/" + sec:             "/in/{token}",
		"/in/" + sec + "/x":       "/in/{token}",
		"/in/":                    "/in/{token}",
		"/in":                     "/in/{token}",
		"/in/whatever":            "/in/{token}",
		"/./in/" + sec:            "/in/{token}",
		"/IN/" + sec:              "/IN/{token}",
		"/api/in/" + sec:          "/api/in/{token}",
		"/x/" + sec:               "/x/{token}",
		"/x/" + sec + sec:         "/x/{token}",
		"/api/clips/AbcDefGhiJkl": "/api/clips/AbcDefGhiJkl",
		"/":                       "/",
		"":                        "",
		"/index":                  "/index",
		"/ci_short":               "/ci_short",

		// A crafted near-secret before a real one: matched from the first
		// `ci_` to the end of the run, never leaving the real body (run 8).
		"/x/ci_" + strings.Repeat("A", 40) + sec: "/x/{token}",
		"/x/" + sec + "/y":                       "/x/{token}/y",
	}
	for in, want := range cases {
		if got := LogPath(in); got != want {
			t.Errorf("LogPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// The two middleware log lines write LogPath, and the secret is absent from the
// whole captured log — for a path the ROOT mux answers itself (a redirect for the
// doubled slash), one past the route, one with the wrong case and one under
// another prefix, and for a panic on the route.
func TestAccessLogAndRecoverRedactTheInboundSecret(t *testing.T) {
	sec := ids.InboundToken()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	mux := http.NewServeMux()
	mux.Handle("/in/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Panic") != "" {
			panic("boom")
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	mux.Handle("/", http.NotFoundHandler())
	h := RequestID()(AccessLog(log)(Recover(log)(mux)))
	for _, p := range []string{"/in/" + sec, "//in/" + sec, "/in/" + sec + "/x", "/IN/" + sec, "/x/" + sec} {
		buf.Reset()
		req := httptest.NewRequest(http.MethodPost, "http://box"+p, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		out := buf.String()
		if strings.Contains(out, sec) || strings.Contains(out, sec[3:]) {
			t.Errorf("%s: the secret reached the log: %s", p, out)
		}
		if !strings.Contains(out, "{token}") {
			t.Errorf("%s: no redacted path in the log: %s", p, out)
		}
	}
	buf.Reset()
	req := httptest.NewRequest(http.MethodPost, "/in/"+sec, nil)
	req.Header.Set("X-Panic", "1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	out := buf.String()
	if !strings.Contains(out, "msg=panic") || strings.Contains(out, sec) || strings.Count(out, "/in/{token}") != 2 {
		t.Errorf("panic line: %s", out)
	}
}

// A pin: no non-test Go in internal/ or cmd/ writes a request path to a log
// without LogPath. A line that names r.URL.Path beside a "path" key is a log
// call, and it must go through LogPath.
func TestEveryLoggedPathGoesThroughLogPath(t *testing.T) {
	var bad []string
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 1<<20), 1<<20)
			for n := 1; sc.Scan(); n++ {
				line := sc.Text()
				if strings.Contains(line, "URL.Path") && strings.Contains(line, `"path"`) && !strings.Contains(line, "LogPath(") {
					bad = append(bad, p+":"+itoa(n)+": "+strings.TrimSpace(line))
				}
			}
			return sc.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(bad) > 0 {
		t.Fatalf("a request path is logged without LogPath:\n%s", strings.Join(bad, "\n"))
	}
}

func itoa(n int) string {
	var b [20]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + n%10)
		if n /= 10; n == 0 {
			return string(b[i:])
		}
	}
}

// The Clippet Auto URL's secret (§20 erratum 324) is redacted by both rules:
// under /in/ (whatever the case of the route below it) by rule 1, and anywhere
// else by rule 2, which must test BOTH prefixes before skipping its regular
// expression.
func TestLogPathAutoFetch(t *testing.T) {
	sec := ids.AutoFetchToken()
	if ids.AutoFetchTokenLen != ids.InboundTokenLen {
		t.Fatal("the two path secrets differ in length; the one expression no longer covers both")
	}
	cases := map[string]string{
		"/in/auto/" + sec:                "/in/{token}",
		"//in/auto/" + sec:               "/in/{token}",
		"/in/auto/" + sec + "/x":         "/in/{token}",
		"/IN/AUTO/" + sec:                "/IN/AUTO/{token}",
		"/x/" + sec:                      "/x/{token}",
		"/api/in/auto/" + sec:            "/api/in/auto/{token}",
		"/x/" + sec + ids.InboundToken(): "/x/{token}", // one run since run 8 ({43,})
		"/ca_short":                      "/ca_short",
	}
	for in, want := range cases {
		if got := LogPath(in); got != want {
			t.Errorf("LogPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// The one-time login link's secret (§20 erratum 341): under /login/discord/ by
// the path rule, and a `cs_` run anywhere else by the expression.
func TestLogPathSignin(t *testing.T) {
	sec := ids.SigninToken()
	if ids.SigninTokenLen != ids.InboundTokenLen || !ids.IsSigninToken(sec) {
		t.Fatal("the login-link secret is not the shape the one expression covers")
	}
	cases := map[string]string{
		"/login/discord/" + sec:        "/login/discord/{token}",
		"//login/discord/" + sec:       "/login/discord/{token}",
		"/login/discord/" + sec + "/x": "/login/discord/{token}",
		"/login/discord/not-a-token":   "/login/discord/{token}",
		"/LOGIN/DISCORD/" + sec:        "/LOGIN/DISCORD/{token}",
		"/x/" + sec:                    "/x/{token}",
		"/login":                       "/login",
		"/cs_short":                    "/cs_short",
	}
	for in, want := range cases {
		if got := LogPath(in); got != want {
			t.Errorf("LogPath(%q) = %q, want %q", in, got, want)
		}
	}
}
