package httpapi

// setup_test.go — POST /api/setup/check (ROADMAP #45, §20 erratum 297): the
// refusal matrix, the bounded vocabulary, and the single flight that makes a
// hundred presses one probe.

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"clippet/internal/httpx"
	"clippet/internal/store"
)

// setupStates is the closed set the route may answer with and nothing else
// (C10; store.go's §20 erratum 31 classes plus `ok`). A page renders this
// word, so anything outside the set is a storage detail reaching a reader.
var setupStates = map[string]bool{
	"ok": true, store.PingUnreachable: true, store.PingTimeout: true,
	store.PingDenied: true, store.PingBucketMissing: true, store.PingUnavailable: true,
}

// TestSetupCheckRefusalMatrix: anonymous and a capture token are both refused,
// a session is not. A leaked ShareX token must not be able to spend a storage
// operation on somebody else's bucket.
func TestSetupCheckRefusalMatrix(t *testing.T) {
	e := newEnv(t)
	u, cookie := e.newUser("carol")
	tok := e.newToken(u, "upload")

	if r := e.do(http.MethodPost, "/api/setup/check", nil); r.code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401 (%s)", r.code, r.body)
	}
	if r := e.do(http.MethodPost, "/api/setup/check", nil, withBearer(tok)); r.code != http.StatusForbidden {
		t.Errorf("a capture token = %d, want 403 (%s)", r.code, r.body)
	}
	if r := e.do(http.MethodGet, "/api/setup/check", nil, withCookie(cookie)); r.code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", r.code)
	}
	r := e.do(http.MethodPost, "/api/setup/check", nil, withCookie(cookie))
	if r.code != http.StatusOK {
		t.Fatalf("a session = %d, want 200 (%s)", r.code, r.body)
	}
	if ok, _ := r.json["ok"].(bool); !ok {
		t.Errorf("a healthy store answered ok=false: %s", r.body)
	}
	if !setupStates[r.str("state")] {
		t.Errorf("state %q is outside the closed vocabulary", r.str("state"))
	}
	if r.str("checked_at") == "" {
		t.Errorf("no checked_at: %s", r.body)
	}
}

// TestSetupCheckProbesAndCleansUp: the probe is Ping + put + delete, with no
// read-back, and it leaves nothing behind — under `{S3_KEY_PREFIX}doctor/`,
// never under `clips/`, so `admin orphans` and the sweeper can never see it.
func TestSetupCheckProbesAndCleansUp(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")
	if r := e.do(http.MethodPost, "/api/setup/check", nil, withCookie(cookie)); r.code != 200 {
		t.Fatalf("check = %d", r.code)
	}
	var put, del, got bool
	for _, c := range e.fake.CallsSnapshot() {
		switch {
		case strings.HasPrefix(c, "PutFile"):
			put = true
		case strings.HasPrefix(c, "DeletePrefix"):
			del = true
		case strings.HasPrefix(c, "GetRange"), strings.HasPrefix(c, "Download"):
			got = true
		}
	}
	if !put || !del {
		t.Errorf("the probe did not put and delete: %v", e.fake.CallsSnapshot())
	}
	if got {
		t.Errorf("the probe read the object back: that is a Class B operation per press, and it is not what the doctor's row proves")
	}
	for _, k := range e.fake.Deletes() {
		if !strings.HasPrefix(k, "p/"+store.ProbeKeyFolder) {
			t.Errorf("the probe object was not under the key prefix's doctor folder: %q", k)
		}
	}
	if n := len(e.fake.Deletes()); n != 1 {
		t.Errorf("the probe left %d deletes, want 1", n)
	}
}

// TestSetupCheckIsSingleFlightAndCached is C7 as a test: no new rate limiter,
// a cache instead. Ten presses are ONE probe, and every one of them gets the
// same `checked_at` — which is how a caller can see that.
func TestSetupCheckIsSingleFlightAndCached(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")
	first := e.do(http.MethodPost, "/api/setup/check", nil, withCookie(cookie))
	at := first.str("checked_at")
	if at == "" {
		t.Fatalf("no checked_at: %s", first.body)
	}
	pings := 0
	for _, c := range e.fake.CallsSnapshot() {
		if c == "Ping" {
			pings++
		}
	}
	for i := 0; i < 9; i++ {
		r := e.do(http.MethodPost, "/api/setup/check", nil, withCookie(cookie))
		if r.code != 200 {
			t.Fatalf("press %d = %d", i, r.code)
		}
		if r.str("checked_at") != at {
			t.Errorf("press %d re-probed: checked_at %q, want %q", i, r.str("checked_at"), at)
		}
	}
	after := 0
	for _, c := range e.fake.CallsSnapshot() {
		if c == "Ping" {
			after++
		}
	}
	if after != pings {
		t.Errorf("ten presses made %d pings, want the first one's %d", after, pings)
	}
}

// TestSetupCheckSaysTheClassAndNothingElse: a store that refuses answers the
// bounded word, and the body carries no endpoint, no bucket, no credential and
// none of the library's own sentence — which is the whole difference between
// this route and `admin doctor`'s line.
func TestSetupCheckSaysTheClassAndNothingElse(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")
	e.fake.FailNext("Ping", &store.PingError{Class: store.PingDenied,
		Err: errors.New("403 Forbidden: GET http://secret-host.example:9000/secret-bucket?x=AKIASECRET")})
	r := e.do(http.MethodPost, "/api/setup/check", nil, withCookie(cookie))
	if r.code != http.StatusOK {
		t.Fatalf("a refused store = %d, want 200 with ok=false", r.code)
	}
	if ok, _ := r.json["ok"].(bool); ok {
		t.Errorf("a refused store answered ok=true: %s", r.body)
	}
	if r.str("state") != store.PingDenied {
		t.Errorf("state = %q, want %q", r.str("state"), store.PingDenied)
	}
	body := string(r.body)
	for _, leak := range []string{"secret-host.example", "secret-bucket", "AKIASECRET", "403 Forbidden"} {
		if strings.Contains(body, leak) {
			t.Errorf("the body carries %q: %s", leak, body)
		}
	}
}

// TestSetupCheckRecordsItsProbeInTheSharedCache is §20 erratum 297 (i): the
// one LIVE probe this page performs goes into the same `httpx.StorageHealth`
// the boot loop and /readyz write and that /healthz and the page's first panel
// read — so `last check` means the last check, and the page cannot say `ok`
// about a store the button has just watched fail.
//
// It matters because nothing else takes a second reading on a running
// instance: serve.go's ping loop RETURNS on its first success, and the compose
// healthcheck polls /healthz, which only Gets.
func TestSetupCheckRecordsItsProbeInTheSharedCache(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fail  error
		state string
		ok    bool
	}{
		{"a store that answers", nil, "ok", true},
		{"a store that refuses", &store.PingError{Class: store.PingDenied,
			Err: errors.New("403 Forbidden")}, "error: " + store.PingDenied, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			health := &httpx.StorageHealth{}
			e := newEnv(t, func(d *Deps) { d.Storage = health })
			_, cookie := e.newUser("carol")
			if got := health.State(); got != "pending" {
				t.Fatalf("before the press the shared cache reads %q, want pending", got)
			}
			if tc.fail != nil {
				e.fake.FailNext("Ping", tc.fail)
			}
			r := e.do(http.MethodPost, "/api/setup/check", nil, withCookie(cookie))
			if r.code != http.StatusOK {
				t.Fatalf("check = %d (%s)", r.code, r.body)
			}
			if ok, _ := r.json["ok"].(bool); ok != tc.ok {
				t.Errorf("the route answered ok=%v, want %v: %s", ok, tc.ok, r.body)
			}
			if got := health.State(); got != tc.state {
				t.Errorf("the shared cache reads %q, want %q", got, tc.state)
			}
			if health.OK() != tc.ok {
				t.Errorf("the shared cache's OK() is %v, want %v", health.OK(), tc.ok)
			}
			if health.LastChecked().IsZero() {
				t.Error("the shared cache has no `last check` instant, so the page's row would still read the boot ping")
			}
			// The class travels, and the store's own sentence does not: the
			// cache renders the bounded word, never the wrapped error's text.
			if strings.Contains(health.Get(), "403 Forbidden") {
				t.Errorf("the shared cache carries the store's own error: %q", health.Get())
			}
		})
	}
}

// TestSetupCheckIsTheOwners: `Check now` is the owner's button (§20 erratum
// 349). A second account's session is refused 403 `not_owner` with the
// owner-only sentence, and no probe runs for it — the store sees no call — while
// the first account (the owner) is answered.
func TestSetupCheckIsTheOwners(t *testing.T) {
	e := newEnv(t)
	_, owner := e.newUser("carol")
	_, member := e.newUser("friend")
	before := len(e.fake.CallsSnapshot())
	r := e.do(http.MethodPost, "/api/setup/check", nil, withCookie(member))
	if r.code != http.StatusForbidden || r.str("error") != "not_owner" || r.str("message") != msgOwnerOnly {
		t.Fatalf("a member = %d %s, want 403 not_owner", r.code, r.body)
	}
	if got := e.fake.CallsSnapshot()[before:]; len(got) != 0 {
		t.Fatalf("a member's press reached the store: %v", got)
	}
	if r := e.do(http.MethodPost, "/api/setup/check", nil, withCookie(owner)); r.code != http.StatusOK {
		t.Fatalf("the owner = %d %s, want 200", r.code, r.body)
	}
}
