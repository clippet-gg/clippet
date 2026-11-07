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

	"github.com/coder/websocket"
)

// Clippet Auto's network half (§20 erratum 333): the REST call puts the token
// only where asked, never returns a URL or a token, and the socket is opened
// only for a URL the gateway rule admits.

const autoTestToken = "AUTO.secret-token_0123456789abcdef"

func autoClientFor(t *testing.T, srv *httptest.Server, allow func(string) (*url.URL, error)) *AutoClient {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	if allow == nil {
		allow = func(s string) (*url.URL, error) { return nil, errors.New("no socket in this test") }
	}
	c := NewAutoClient(AutoDeps{Token: autoTestToken, APIHost: u.Host, APIScheme: "http", AllowGateway: allow})
	if c == nil {
		t.Fatal("NewAutoClient returned nil")
	}
	return c
}

func TestAutoClientCall(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("X-RateLimit-Bucket", "b1")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":"1"}`))
	}))
	defer srv.Close()
	c := autoClientFor(t, srv, nil)
	r, err := c.Call(context.Background(), "POST", "/channels/1/messages", []byte(`{}`), true)
	if err != nil || r.Status != 201 || string(r.Body) != `{"id":"1"}` || r.Header.Get("X-RateLimit-Bucket") != "b1" {
		t.Fatalf("r=%+v err=%v", r, err)
	}
	if _, err := c.Call(context.Background(), "POST", "/interactions/1/itok/callback", []byte(`{}`), false); err != nil {
		t.Fatal(err)
	}
	if auths[0] != "Bot "+autoTestToken || auths[1] != "" {
		t.Fatalf("authorization = %q", auths)
	}
	if paths[0] != "/api/v10/channels/1/messages" {
		t.Fatalf("path = %s", paths[0])
	}
	// A transport failure says nothing of the URL (an interaction token rides
	// in it) or the token.
	srv.Close()
	_, err = c.Call(context.Background(), "POST", "/interactions/1/SECRET-ITOKEN/callback", nil, true)
	if err == nil || strings.Contains(err.Error(), "SECRET-ITOKEN") || strings.Contains(err.Error(), autoTestToken) {
		t.Fatalf("err = %v", err)
	}
}

func TestAutoClientDialChecksTheGatewayRule(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = ws.Write(r.Context(), websocket.MessageText, []byte(`{"op":10}`))
		_ = ws.Close(4014, "x")
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/?v=10&encoding=json"
	refuse := func(string) (*url.URL, error) { return nil, errors.New("no") }
	if _, err := autoClientFor(t, srv, refuse).Dial(context.Background(), wsURL); err == nil {
		t.Fatal("a URL the rule refused was dialled")
	}
	var seen string
	allow := func(s string) (*url.URL, error) { seen = s; return url.Parse(s) }
	conn, err := autoClientFor(t, srv, allow).Dial(context.Background(), wsURL)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(seen, "?") {
		t.Fatalf("the rule was asked about %q, query and all", seen)
	}
	p, err := conn.Read(context.Background())
	if err != nil || string(p) != `{"op":10}` {
		t.Fatalf("read %q %v", p, err)
	}
	_, err = conn.Read(context.Background())
	var cc interface{ CloseCode() int }
	if !errors.As(err, &cc) || cc.CloseCode() != 4014 {
		t.Fatalf("close = %v", err)
	}
}

func TestAutoClientMe(t *testing.T) {
	for flags, want := range map[string]bool{`{"flags":262144}`: true, `{"flags":524288}`: true, `{"flags":0}`: false, `{}`: false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v10/users/@me" {
				_, _ = w.Write([]byte(`{"id":"1"}`))
				return
			}
			_, _ = w.Write([]byte(flags))
		}))
		tok, intent, err := autoClientFor(t, srv, nil).Me(context.Background())
		srv.Close()
		if err != nil || !tok || intent != want {
			t.Fatalf("%s: tok=%v intent=%v err=%v", flags, tok, intent, err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	if tok, _, err := autoClientFor(t, srv, nil).Me(context.Background()); tok || err != nil {
		t.Fatalf("a refused token: tok=%v err=%v", tok, err)
	}
}

// Pin (§20 erratum 347 (e)): a path's query reaches Discord as a query — the
// hosted backfill's GET /channels/{id}/messages?after=…&limit=50 — and is not
// escaped into the path (it was, as `%3F`, until the E2E bundle saw it).
func TestAutoClientCallQuery(t *testing.T) {
	var got *url.URL
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := *r.URL
		got = &u
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c := autoClientFor(t, srv, nil)
	if _, err := c.Call(context.Background(), "GET", "/channels/123/messages?after=456&limit=50", nil, true); err != nil {
		t.Fatal(err)
	}
	if got.Path != "/api/v10/channels/123/messages" || got.RawQuery != "after=456&limit=50" || strings.Contains(got.RawPath, "%3F") {
		t.Fatalf("path=%q query=%q raw=%q", got.Path, got.RawQuery, got.RawPath)
	}
	if _, err := c.Call(context.Background(), "GET", "/gateway/bot", nil, true); err != nil || got.RawQuery != "" {
		t.Fatalf("a path with no query gained one: %q %v", got.RawQuery, err)
	}
}
