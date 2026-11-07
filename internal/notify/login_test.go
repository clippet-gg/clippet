package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Discord sign-in exchange (ROADMAP #44, §20 erratum 302), against
// httptest servers that play Discord — the half the E2E cannot reach.

const (
	loginTestClient = "876543210987654321"
	loginTestSecret = "s3cr3t-ValueThatMustNeverPrint-0123"
	loginTestCode   = "the-authorization-code"
	loginTestVerify = "the-pkce-verifier-0123456789abcdefghijklmn"
	loginTestRedir  = "https://clips.example/api/auth/discord/callback"
)

// fakeDiscord records the two requests and answers them from a script.
type fakeDiscord struct {
	mu       sync.Mutex
	tokenReq *http.Request
	tokenBdy url.Values
	meReq    *http.Request
	token    func(w http.ResponseWriter, r *http.Request) // nil = the documented 200
	me       func(w http.ResponseWriter, r *http.Request) // nil = the documented 200
}

func (f *fakeDiscord) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case botAPIPath + "/oauth2/token":
			_ = r.ParseForm()
			f.mu.Lock()
			f.tokenReq, f.tokenBdy = r, r.PostForm
			f.mu.Unlock()
			if f.token != nil {
				f.token(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"acc-tok-xyz","token_type":"Bearer","expires_in":604800,"refresh_token":"ref-tok","scope":"identify"}`))
		case botAPIPath + "/users/@me":
			f.mu.Lock()
			f.meReq = r
			f.mu.Unlock()
			if f.me != nil {
				f.me(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"111111111111111111","username":"carol","global_name":"Carol","avatar":null}`))
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	})
}

func newLoginFixture(t *testing.T) (*Login, *fakeDiscord) {
	t.Helper()
	f := &fakeDiscord{}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	l := NewLogin(LoginDeps{ClientID: loginTestClient, Secret: loginTestSecret, APIHost: u.Host, APIScheme: "http",
		Timeout: 2 * time.Second})
	if l == nil {
		t.Fatal("NewLogin returned nil for a full configuration")
	}
	return l, f
}

func TestNewLoginIsNilWhenUnconfigured(t *testing.T) {
	for name, d := range map[string]LoginDeps{
		"nothing":   {},
		"no secret": {ClientID: loginTestClient, APIHost: "h"},
		"no id":     {Secret: loginTestSecret, APIHost: "h"},
		"no host":   {ClientID: loginTestClient, Secret: loginTestSecret},
	} {
		if NewLogin(d) != nil {
			t.Fatalf("%s: NewLogin built a client", name)
		}
	}
}

// The happy path: the token POST is the documented form, on the documented
// path, with the redirect URI and the PKCE verifier; the identity GET carries
// the access token as a Bearer and nothing else; the answer is the id and the
// two names.
func TestLoginIdentify(t *testing.T) {
	l, f := newLoginFixture(t)
	id, err := l.Identify(context.Background(), loginTestCode, loginTestVerify, loginTestRedir)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if id.ID != "111111111111111111" || id.Username != "carol" || id.GlobalName != "Carol" {
		t.Fatalf("identity = %+v", id)
	}
	if f.tokenReq == nil || f.tokenReq.Method != http.MethodPost ||
		!strings.HasPrefix(f.tokenReq.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		t.Fatalf("token request: %+v", f.tokenReq)
	}
	for k, want := range map[string]string{
		"client_id": loginTestClient, "client_secret": loginTestSecret, "grant_type": "authorization_code",
		"code": loginTestCode, "redirect_uri": loginTestRedir, "code_verifier": loginTestVerify,
	} {
		if got := f.tokenBdy.Get(k); got != want {
			t.Fatalf("token form %s = %q, want %q", k, got, want)
		}
	}
	if f.tokenReq.Header.Get("Authorization") != "" {
		t.Fatal("the token call carried an Authorization header")
	}
	if f.meReq == nil || f.meReq.Method != http.MethodGet || f.meReq.Header.Get("Authorization") != "Bearer acc-tok-xyz" {
		t.Fatalf("identity request: %+v", f.meReq)
	}
	for _, r := range []*http.Request{f.tokenReq, f.meReq} {
		if r.Header.Get("User-Agent") != userAgent {
			t.Fatalf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
	}
}

// Every failure is one error wrapping ErrLoginExchange, and none of them names
// the secret, the code, the verifier or a token.
func TestLoginIdentifyFailures(t *testing.T) {
	leaks := []string{loginTestSecret, url.QueryEscape(loginTestSecret), loginTestCode, loginTestVerify, "acc-tok-xyz"}
	check := func(t *testing.T, err error, want string) {
		t.Helper()
		if err == nil {
			t.Fatal("no error")
		}
		if !errors.Is(err, ErrLoginExchange) {
			t.Fatalf("%v does not wrap ErrLoginExchange", err)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err.Error(), want)
		}
		for _, leak := range leaks {
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("error leaks %q: %s", leak, err.Error())
			}
		}
	}
	t.Run("a refused code echoes the secret and is scrubbed", func(t *testing.T) {
		l, f := newLoginFixture(t)
		f.token = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"bad secret ` + loginTestSecret + ` for ` + loginTestClient + `"}`))
		}
		_, err := l.Identify(context.Background(), loginTestCode, loginTestVerify, loginTestRedir)
		check(t, err, "HTTP 400")
		if !strings.Contains(err.Error(), "invalid_client") || !strings.Contains(err.Error(), redacted) {
			t.Fatalf("the body was not kept and scrubbed: %s", err.Error())
		}
		if f.meReq != nil {
			t.Fatal("a refused exchange still asked for the identity")
		}
	})
	t.Run("a redirect is not followed", func(t *testing.T) {
		l, f := newLoginFixture(t)
		f.token = func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/somewhere-else", http.StatusFound)
		}
		_, err := l.Identify(context.Background(), loginTestCode, loginTestVerify, loginTestRedir)
		check(t, err, "HTTP 302")
	})
	t.Run("a 5xx on the identity read", func(t *testing.T) {
		l, f := newLoginFixture(t)
		f.me = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) }
		_, err := l.Identify(context.Background(), loginTestCode, loginTestVerify, loginTestRedir)
		check(t, err, "HTTP 502")
		if !strings.Contains(err.Error(), "identity") {
			t.Fatalf("the error does not say which call: %s", err.Error())
		}
	})
	t.Run("a hung server is one timeout", func(t *testing.T) {
		l, f := newLoginFixture(t)
		l.timeout = 150 * time.Millisecond
		l.client.Timeout = l.timeout
		release := make(chan struct{})
		defer close(release)
		f.token = func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		start := time.Now()
		_, err := l.Identify(context.Background(), loginTestCode, loginTestVerify, loginTestRedir)
		check(t, err, "token")
		if time.Since(start) > 2*time.Second {
			t.Fatalf("the hung server held Identify for %s", time.Since(start))
		}
	})
	t.Run("a bearerless answer", func(t *testing.T) {
		l, f := newLoginFixture(t)
		f.token = func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"token_type":"Bearer"}`)) }
		_, err := l.Identify(context.Background(), loginTestCode, loginTestVerify, loginTestRedir)
		check(t, err, "no bearer token")
	})
	t.Run("an identity with no snowflake", func(t *testing.T) {
		l, f := newLoginFixture(t)
		f.me = func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"id":"not-a-snowflake","username":"x"}`))
		}
		_, err := l.Identify(context.Background(), loginTestCode, loginTestVerify, loginTestRedir)
		check(t, err, "no user id")
	})
	t.Run("a body that is not JSON", func(t *testing.T) {
		l, f := newLoginFixture(t)
		f.me = func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>maintenance</html>")) }
		_, err := l.Identify(context.Background(), loginTestCode, loginTestVerify, loginTestRedir)
		check(t, err, "not the JSON")
	})
	t.Run("the rejection body is windowed and cut", func(t *testing.T) {
		l, f := newLoginFixture(t)
		f.token = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(strings.Repeat("x", 3*bodyWindow)))
		}
		_, err := l.Identify(context.Background(), loginTestCode, loginTestVerify, loginTestRedir)
		check(t, err, "HTTP 400")
		if len(err.Error()) > bodySnippet+100 {
			t.Fatalf("the error carries %d bytes of body", len(err.Error()))
		}
	})
}
