package notify

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
)

// The hosted client's transport (§20 erratum 334, the 2026-10-12 amendment,
// audit F4 and F10). Nothing here leaves loopback.

func hostedFor(t *testing.T, srv *httptest.Server, allowPrivate bool) *HostedClient {
	t.Helper()
	hc, err := NewHostedClient(HostedDeps{PlaneURL: srv.URL + "/claim", PlaneSecret: "plane.SECRET", AllowHTTP: true,
		AllowPrivate: allowPrivate})
	if err != nil {
		t.Fatal(err)
	}
	return hc
}

// Pin (audit F4): the hosted transport never dials a loopback address (nor
// private, link-local, ULA, CGNAT …) — for the plane and a box alike.
func TestHostedTransportRefusesNonPublic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(201) }))
	defer srv.Close()
	hc := hostedFor(t, srv, false)
	if _, _, err := hc.Plane(context.Background(), http.MethodGet, "/auto/session", nil); err == nil {
		t.Fatal("the plane call dialled a loopback address")
	}
	if st, _ := hc.Box(context.Background(), srv.URL+"/in/auto/x", []byte(`{}`), 0); st != 0 {
		t.Fatalf("the box call dialled a loopback address: %d", st)
	}
	if st, _, err := hostedFor(t, srv, true).Plane(context.Background(), http.MethodGet, "/auto/session", nil); err != nil || st != 201 {
		t.Fatalf("with AllowPrivate (tests): %d %v", st, err)
	}
	for addr, public := range map[string]bool{
		"127.0.0.1": false, "10.1.2.3": false, "172.16.0.9": false, "192.168.1.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "0.0.0.0": false, "0.1.2.3": false, "224.0.0.1": false, "::1": false, "fe80::1": false,
		"fdaa:0:1::3": false, "::ffff:127.0.0.1": false, "::": false,
		"64:ff9b::a00:1": false, "64:ff9b::808:808": false, "2001:db8::1": false, "2001:db8:ffff::1": false,
		"1.1.1.1": true, "104.16.0.1": true, "2606:4700::1111": true,
	} {
		if got := publicAddr(netip.MustParseAddr(addr)); got != public {
			t.Fatalf("publicAddr(%s) = %v, want %v", addr, got, public)
		}
	}
}

// Pin (audit F4): an answer whose headers pass 64 KiB is refused, not read
// into memory (net/http's default is 10 MiB).
func TestHostedTransportCapsHeaders(t *testing.T) {
	big := strings.Repeat("x", 4<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i < 32; i++ { // 128 KiB of headers
			w.Header().Set("X-Pad-"+strconv.Itoa(i), big)
		}
		w.WriteHeader(201)
	}))
	defer srv.Close()
	hc := hostedFor(t, srv, true)
	if _, _, err := hc.Plane(context.Background(), http.MethodGet, "/auto/session", nil); err == nil {
		t.Fatal("128 KiB of headers was read")
	}
	if st, _ := hc.Box(context.Background(), srv.URL+"/in/auto/x", []byte(`{}`), 0); st != 0 {
		t.Fatalf("a box's 128 KiB of headers was read: %d", st)
	}
}

// Pin (audit F10): GET /auto/guilds is read whole past 64 KiB — 5,000 servers
// (≈ 110 KiB) decode, where the 64 KiB cap cut the JSON at ≈ 2,900.
func TestPlaneGuildListPastSixtyFourKiB(t *testing.T) {
	ids := make([]string, 5000)
	for i := range ids {
		ids[i] = strconv.Itoa(1419000000000000000 + i)
	}
	body, _ := json.Marshal(map[string]any{"guilds": ids})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/claim/auto/guilds" || r.URL.Path == "/claim/auto/session" {
			_, _ = w.Write(body)
		}
	}))
	defer srv.Close()
	hc := hostedFor(t, srv, true)
	st, got, err := hc.Plane(context.Background(), http.MethodGet, "/auto/guilds", nil)
	var r struct {
		Guilds []string `json:"guilds"`
	}
	if err != nil || st != 200 || json.Unmarshal(got, &r) != nil || len(r.Guilds) != 5000 {
		t.Fatalf("the list read %d bytes of %d (%d %v)", len(got), len(body), st, err)
	}
	// Every other plane answer keeps its 64 KiB cap.
	if _, got, _ := hc.Plane(context.Background(), http.MethodGet, "/auto/session", nil); len(got) != planeAnswerMax {
		t.Fatalf("the session read %d bytes, want the 64 KiB cap", len(got))
	}
}

// Pin (§20 erratum 347): AllowTestNames opens the address check for a name
// under the reserved .test domain and for nothing else — an IP literal, a
// public-looking name, a lookalike (`test`, `x.test.example`, `xtest`) all
// still go through the guarded dial.
func TestHostedTransportTestNames(t *testing.T) {
	for host, want := range map[string]bool{
		"plane.clippet.test": true, "box.clippet.test": true, "BOX.Clippet.TEST": true, "box.clippet.test.": true,
		"test": false, ".test": false, "x..test": false, "x.test.example": false, "xtest": false, "clippet.gg": false,
		"bot.clippet.gg": false, "127.0.0.1": false, "::1": false, "": false,
	} {
		if got := TestName(host); got != want {
			t.Fatalf("TestName(%q) = %v, want %v", host, got, want)
		}
	}
	var took []string
	rec := func(name string) dialFunc {
		return func(_ context.Context, _, addr string) (net.Conn, error) {
			took = append(took, name+" "+addr)
			return nil, errNotPublic
		}
	}
	dial := testNameDial(rec("open"), rec("guarded"))
	for _, addr := range []string{"plane.clippet.test:443", "bot.clippet.gg:443", "127.0.0.1:443", "[::1]:443", "box.clippet.test:443"} {
		_, _ = dial(context.Background(), "tcp", addr)
	}
	want := "open plane.clippet.test:443|guarded bot.clippet.gg:443|guarded 127.0.0.1:443|guarded [::1]:443|open box.clippet.test:443"
	if got := strings.Join(took, "|"); got != want {
		t.Fatalf("dials = %s, want %s", got, want)
	}
	// Without the knob the transport is the one it always was: a loopback
	// httptest server stays refused (TestHostedTransportRefusesNonPublic), and
	// WITH it a loopback IP literal is still refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(201) }))
	defer srv.Close()
	hc, err := NewHostedClient(HostedDeps{PlaneURL: srv.URL + "/claim", PlaneSecret: "plane.SECRET", AllowHTTP: true, AllowTestNames: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := hc.Plane(context.Background(), http.MethodGet, "/auto/session", nil); err == nil {
		t.Fatal("AllowTestNames let an IP literal on loopback through")
	}
	if st, _ := hc.Box(context.Background(), srv.URL+"/in/auto/x", []byte(`{}`), 0); st != 0 {
		t.Fatalf("AllowTestNames let the box call dial loopback by IP: %d", st)
	}
}
