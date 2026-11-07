package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"clippet/internal/autobot"
	"clippet/internal/notify"
)

// countPinger is the dead-man's far end: it counts the pings.
type countPinger struct{ n atomic.Int32 }

func (p *countPinger) Ping(context.Context, string) bool { p.n.Add(1); return true }

// TestDeadmanFollowsDiscordReads is §20 erratum 334 (i): the dead-man ping is
// conditional on BEHAVIOUR, not only on the session. The hosted process stayed
// healthy — and kept pinging — through fourteen days of a backfill whose every
// read answered 404 (erratum 347 (e)). Here a real notify.AutoClient reads from
// a stand-in for Discord's REST that answers the backfill's route 404, 200 or
// 429 on demand; the ping is withheld for a minute whose last read failed, one
// WARN says so, an idle minute still pings, and one INFO says the pings are
// back.
func TestDeadmanFollowsDiscordReads(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusNotFound)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusForbidden) // a write's refusal: one server's fact
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/v10/channels/") {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		if s := int(status.Load()); s == http.StatusTooManyRequests {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(s)
			_, _ = w.Write([]byte(`{"retry_after":0.01,"global":false}`))
			return
		} else if s != http.StatusOK {
			w.WriteHeader(s)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	dc := notify.NewAutoClient(notify.AutoDeps{Token: "test-token", APIHost: u.Host, APIScheme: "http",
		AllowGateway: func(string) (*url.URL, error) { return nil, errors.New("no gateway in this test") }})
	if dc == nil {
		t.Fatal("no client")
	}
	reads := &restReads{}
	call := reads.wrap(dc.Call)
	var broken bool
	failing := reads.wrap(func(ctx context.Context, method, path string, body []byte, auth bool) (autobot.Response, error) {
		if broken {
			return autobot.Response{}, errors.New("the Discord call did not complete")
		}
		return dc.Call(ctx, method, path, body, auth)
	})

	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	m := newMetrics(quiet())
	m.Gateway(autobot.GatewayEvent{Kind: "up"})
	p := &countPinger{}
	ctx := context.Background()
	read := func(c autobot.Caller) {
		t.Helper()
		if _, err := c(ctx, http.MethodGet, "/channels/1430000000000000011/messages?after=1&limit=50", nil, true); err != nil && !broken {
			t.Fatalf("read: %v", err)
		}
	}
	minute := func(wantPings int32, why string) {
		t.Helper()
		deadman(ctx, p, m, reads, "the-deadman-target", log)
		if got := p.n.Load(); got != wantPings {
			t.Fatalf("%s: %d pings, want %d", why, got, wantPings)
		}
	}
	count := func(outcome string) int { return strings.Count(logs.String(), `"outcome":"`+outcome+`"`) }

	minute(1, "an idle minute on a healthy session")

	read(call) // 404
	minute(1, "a minute whose only read answered 404")
	read(call)
	read(call)
	minute(1, "a second failing minute")
	if count("withheld") != 1 || !strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Fatalf("want ONE WARN for the withheld pings, logs:\n%s", logs.String())
	}

	minute(2, "an idle minute after the failures (idle is healthy)")
	if count("resumed") != 1 || !strings.Contains(logs.String(), `"level":"INFO"`) {
		t.Fatalf("want ONE INFO when the pings resume, logs:\n%s", logs.String())
	}

	read(call) // 404, then a 2xx: the LAST read of the minute decides
	status.Store(http.StatusOK)
	read(call)
	minute(3, "a minute whose last read answered 200")

	status.Store(http.StatusTooManyRequests) // the manners' own wait: neither
	read(call)
	minute(4, "a minute whose only read was a 429")

	if _, err := call(ctx, http.MethodPost, "/channels/1430000000000000011/messages", []byte(`{}`), true); err != nil {
		t.Fatal(err)
	}
	minute(5, "a minute whose only call was a refused WRITE")

	broken = true // a read that never completed
	read(failing)
	minute(5, "a minute whose read did not complete")
	broken = false
	status.Store(http.StatusOK)
	read(failing)
	minute(6, "the reads answer again")
	if count("withheld") != 2 || count("resumed") != 2 {
		t.Fatalf("want two withheld/resumed pairs, logs:\n%s", logs.String())
	}

	// An unhealthy session pings nothing, whatever the reads said.
	m.Gateway(autobot.GatewayEvent{Kind: "down"})
	minute(6, "a session that is down")
}
