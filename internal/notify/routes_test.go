package notify

import (
	"context"
	"strings"
	"testing"
)

// routeToken is the second endpoint's secret. Like `token` it must never
// appear in anything this package returns.
const routeToken = "R0UTE-TOKEN-must-never-appear"

// TestRoutedDeliveryPicksTheNamedWebhook: two webhooks, one client, and the
// message goes to the one the route names — not to the default.
func TestRoutedDeliveryPicksTheNamedWebhook(t *testing.T) {
	def := newRecorder(t, 204, "")
	clutch := newRecorder(t, 204, "")
	n := New(Deps{
		Endpoint: def.srv.URL + "/api/webhooks/1/" + token,
		Routes:   []Route{{Name: "clutch", Endpoint: clutch.srv.URL + "/api/webhooks/2/" + routeToken}},
		URLs:     base(t),
	})
	if n == nil {
		t.Fatal("New returned nil with two webhooks configured")
	}
	if res := n.Deliver(context.Background(), "clutch", "aZ3kQ9xB2mLp", "t"); !res.OK {
		t.Fatalf("routed delivery: %+v", res)
	}
	if len(clutch.sent()) != 1 || len(def.sent()) != 0 {
		t.Fatalf("routed post went to default=%d clutch=%d", len(def.sent()), len(clutch.sent()))
	}
	// …and an unrouted one still goes to the default.
	if res := n.Deliver(context.Background(), "", "aZ3kQ9xB2mLp", "t"); !res.OK {
		t.Fatalf("default delivery: %+v", res)
	}
	if len(def.sent()) != 1 || len(clutch.sent()) != 1 {
		t.Fatalf("default post went to default=%d clutch=%d", len(def.sent()), len(clutch.sent()))
	}
	// The explicit spelling of the default is the same channel.
	if res := n.Deliver(context.Background(), DefaultRoute, "aZ3kQ9xB2mLp", "t"); !res.OK {
		t.Fatalf("named default delivery: %+v", res)
	}
	if len(def.sent()) != 2 {
		t.Fatalf("%q did not reach the default webhook", DefaultRoute)
	}
}

// TestRoutesAloneAreEnoughToConstructANotifier: an instance that announces
// only tagged clips has no DISCORD_WEBHOOK_URL, and that must not be read as
// "the feature is off".
func TestRoutesAloneConstructANotifier(t *testing.T) {
	rec := newRecorder(t, 204, "")
	n := New(Deps{Routes: []Route{{Name: "clutch", Endpoint: rec.srv.URL + "/api/webhooks/2/" + routeToken}}, URLs: base(t)})
	if n == nil {
		t.Fatal("New returned nil with a route configured")
	}
	if n.HasRoute("") || n.HasRoute(DefaultRoute) {
		t.Error("a default route exists although DISCORD_WEBHOOK_URL is unset")
	}
	if !n.HasRoute("clutch") {
		t.Error("the configured route is missing")
	}
	if res := n.Deliver(context.Background(), "clutch", "aZ3kQ9xB2mLp", "t"); !res.OK {
		t.Fatalf("routed delivery: %+v", res)
	}
}

// TestNoWebhookAtAllConstructsNothing: the §15 promise, restated for routing.
func TestNoWebhookAtAllConstructsNothing(t *testing.T) {
	if n := New(Deps{Routes: nil, URLs: base(t)}); n != nil {
		t.Fatal("New built a client with nothing configured")
	}
	if n := New(Deps{Routes: []Route{{Name: "clutch"}}, URLs: base(t)}); n != nil {
		t.Fatal("New built a client from a route with an empty endpoint")
	}
}

// TestUnknownRouteNeverFallsBackToTheDefault. HasRoute is what the worker
// asks; Deliver refuses too, and it must not post to a channel nobody chose.
func TestUnknownRouteNeverFallsBackToTheDefault(t *testing.T) {
	def := newRecorder(t, 204, "")
	n := New(Deps{Endpoint: def.srv.URL + "/api/webhooks/1/" + token, URLs: base(t)})
	if n.HasRoute("clutch") {
		t.Error("HasRoute said yes to a route that is not configured")
	}
	res := n.Deliver(context.Background(), "clutch", "aZ3kQ9xB2mLp", "t")
	if res.OK || res.Retry {
		t.Fatalf("an unknown route delivered or asked to retry: %+v", res)
	}
	if len(def.sent()) != 0 {
		t.Fatal("an unknown route posted to the DEFAULT webhook")
	}
	if !strings.Contains(res.Detail, "clutch") {
		t.Errorf("the detail does not name the route: %q", res.Detail)
	}
}

// TestEveryRoutesSecretIsScrubbed: the scrub list covers EVERY endpoint, not
// just the default one. A rejection body that echoes a routed webhook's token
// must come back with the token gone.
func TestEveryRoutesSecretIsScrubbed(t *testing.T) {
	def := newRecorder(t, 204, "")
	rec := newRecorder(t, 404, `{"message":"Unknown Webhook for `+routeToken+` and `+token+`","code":10015}`)
	n := New(Deps{
		Endpoint: def.srv.URL + "/api/webhooks/1/" + token,
		Routes:   []Route{{Name: "clutch", Endpoint: rec.srv.URL + "/api/webhooks/2/" + routeToken}},
		URLs:     base(t),
	})
	res := n.Deliver(context.Background(), "clutch", "aZ3kQ9xB2mLp", "t")
	if res.OK || res.Retry {
		t.Fatalf("a 404 is permanent: %+v", res)
	}
	for _, secret := range []string{token, routeToken, rec.srv.URL, def.srv.URL} {
		if strings.Contains(res.Detail, secret) {
			t.Errorf("Detail leaked %q: %s", secret, res.Detail)
		}
	}
	// The diagnosable half survives: an operator still sees Discord's code.
	if !strings.Contains(res.Detail, "10015") {
		t.Errorf("the rejection body was scrubbed away entirely: %s", res.Detail)
	}
}

// TestTheDefaultWebhookKeepsItsName is the belt to config's braces. Since §20
// erratum 160 config REFUSES a rule tagged `default` at boot, so this pair can
// no longer be assembled from the environment at all; the property is still
// pinned here because New is a constructor other code may call, and "the first
// writer of a name wins, and DISCORD_WEBHOOK_URL is written first" is what
// makes a silent displacement impossible however the Deps were built.
func TestTheDefaultWebhookKeepsItsName(t *testing.T) {
	def := newRecorder(t, 204, "")
	other := newRecorder(t, 204, "")
	n := New(Deps{
		Endpoint: def.srv.URL + "/api/webhooks/1/" + token,
		Routes:   []Route{{Name: DefaultRoute, Endpoint: other.srv.URL + "/api/webhooks/2/" + routeToken}},
		URLs:     base(t),
	})
	if res := n.Deliver(context.Background(), "", "aZ3kQ9xB2mLp", "t"); !res.OK {
		t.Fatalf("delivery: %+v", res)
	}
	if len(def.sent()) != 1 || len(other.sent()) != 0 {
		t.Fatalf("a route named %q displaced DISCORD_WEBHOOK_URL", DefaultRoute)
	}
}
