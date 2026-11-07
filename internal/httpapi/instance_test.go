package httpapi

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"clippet/internal/config"
	"clippet/internal/urls"
)

// GET /api/instance — the instance's own self-description (§20 erratum 303).
// Four keys, in one order, for ever; anonymous; never cached; registered on
// every instance whatever else is configured.

const testCommit = "0123456789abcdef0123456789abcdef01234567"

// instanceKeys is the key list, IN ORDER, read out of the raw bytes rather than
// out of a decoded map — a map has no order, and the order is part of the
// contract: the plane's verifier reads this body field by field and a release
// manifest is diffed against it, so a reordering is a wire change and belongs
// in a diff. It also catches a fifth key, which a length check on a map would
// catch and a `Contains` on each name would not.
func instanceKeys(t *testing.T, body []byte) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(body)))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		t.Fatalf("the body does not open with an object: %q (%v)", body, err)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatalf("reading a key out of %q: %v", body, err)
		}
		name, ok := k.(string)
		if !ok {
			t.Fatalf("a non-string key in %q", body)
		}
		keys = append(keys, name)
		// Skip the value, whatever it is.
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("reading %s's value out of %q: %v", name, body, err)
		}
	}
	return keys
}

func TestInstanceAnswersFourKeysInOneOrder(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Commit = testCommit })
	r := e.do(http.MethodGet, instancePath, nil)
	if r.code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", instancePath, r.code, r.body)
	}
	if ct := r.hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	// The exact key set, in the exact order, out of the bytes on the wire.
	want := []string{"version", "build", "media_delivery", "media_origin"}
	got := instanceKeys(t, r.body)
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keys = %v, want %v (order is part of the contract)", got, want)
		}
	}
	// …and decoded, the same four and nothing else.
	if len(r.json) != 4 {
		t.Fatalf("the decoded body carries %d keys: %s", len(r.json), r.body)
	}
	if r.json["version"] != "test" || r.json["build"] != testCommit {
		t.Fatalf("body = %s", r.body)
	}
	// This fixture is a proxy instance, which is what `null` means here.
	if r.json["media_delivery"] != config.DeliveryProxy {
		t.Fatalf("media_delivery = %v, want %q", r.json["media_delivery"], config.DeliveryProxy)
	}
	if v, ok := r.json["media_origin"]; !ok || v != nil {
		t.Fatalf("media_origin = %v (present=%v), want the JSON literal null", v, ok)
	}
	// Nothing about the store, the bucket, the queue or this box beyond those
	// four facts: the body is short enough to say so as an assertion.
	for _, leak := range []string{"p/", e.scratch, "queue", "db", "storage", "uptime"} {
		if leak != "" && strings.Contains(string(r.body), leak) {
			t.Errorf("the body carries %q: %s", leak, r.body)
		}
	}
}

// The router SPELLS this path (router.go says why: the plane's route diff
// resolves a constant only for three named files, so a named one would be
// invisible to it). This is the pin that keeps the two spellings one string:
// the pattern the mux recorded is `instancePath`, exactly.
func TestInstanceIsRegisteredAtItsOwnPath(t *testing.T) {
	e := newEnv(t)
	h := newHandler(Deps{Cfg: e.cfg, DB: e.db, URLs: urls.New(mustParseOrigin(t), "test")})
	h.routes()
	if !slices.Contains(h.patterns, instancePath) {
		t.Fatalf("the mux did not register %q: %v", instancePath, h.patterns)
	}
	// …and once. A second registration of one pattern is a panic from the mux,
	// so this is really about a NEAR miss — a trailing slash, a second spelling
	// — which would leave two patterns sharing the prefix.
	n := 0
	for _, p := range h.patterns {
		if strings.HasPrefix(p, instancePath) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d patterns begin with %q: %v", n, instancePath, h.patterns)
	}
}

func TestInstanceIsNoStore(t *testing.T) {
	e := newEnv(t)
	r := e.do(http.MethodGet, instancePath, nil)
	if cc := r.hdr.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store: the answer changes with a rebuild or an .env edit", cc)
	}
}

// Anonymous, and it sets no cookie: a verifier holds no credential on the box
// it is asking, and the answer must not depend on one or hand one out.
func TestInstanceIsAnonymousAndSetsNoCookie(t *testing.T) {
	e := newEnv(t)
	r := e.do(http.MethodGet, instancePath, nil)
	if r.code != http.StatusOK {
		t.Fatalf("an anonymous GET = %d %s", r.code, r.body)
	}
	if sc := r.hdr.Values("Set-Cookie"); len(sc) != 0 {
		t.Fatalf("the route set a cookie: %v", sc)
	}
	if v := r.hdr.Get("Vary"); strings.Contains(v, "Cookie") {
		t.Errorf("Vary = %q: the answer does not depend on a credential", v)
	}
}

// GET only. It reports and changes nothing, so every other method is the
// router's own 405 with an Allow header.
func TestInstanceIsGetOnly(t *testing.T) {
	e := newEnv(t)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		r := e.do(m, instancePath, strings.NewReader("{}"), withJSON())
		if r.code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s = %d %s, want 405", m, instancePath, r.code, r.body)
		}
		if a := r.hdr.Get("Allow"); a != "GET, HEAD" {
			t.Errorf("%s: Allow = %q, want \"GET, HEAD\"", m, a)
		}
	}
	// HEAD is the GET handler, as it is on every GET route here.
	if r := e.do(http.MethodHead, instancePath, nil); r.code != http.StatusOK {
		t.Errorf("HEAD = %d", r.code)
	}
}

// It exists with the bot OFF. Unlike /api/bot/claim — which lives inside
// `discordConfigured()` and is a 404 without it — this route is registered
// unconditionally, because there is no configuration behind it: `newEnv`
// configures no Discord anything, and the route still answers.
func TestInstanceIsRegisteredWithTheBotOff(t *testing.T) {
	e := newEnv(t)
	if e.cfg.DiscordBot.AppID() != "" {
		t.Fatalf("this fixture has the bot configured; the test asserts the opposite")
	}
	if r := e.do(http.MethodGet, instancePath, nil); r.code != http.StatusOK {
		t.Fatalf("with the bot off = %d %s: the route must not be conditional", r.code, r.body)
	}
	// …and the claim route, on the same bucket, is the 404 it has always been
	// in this state — so registering one unconditionally did not register the
	// other.
	if r := e.do(http.MethodGet, botClaimPath+"?guild_id=222222222222222222", nil); r.code != http.StatusNotFound {
		t.Fatalf("GET %s with the bot off = %d", botClaimPath, r.code)
	}
}

// Both delivery shapes, and the two states of `build`.
func TestInstanceReportsBothDeliveryShapes(t *testing.T) {
	const origin = "https://clips.example.test"
	e := newEnv(t, func(d *Deps) {
		d.Cfg.MediaDelivery = config.DeliveryRedirect
		d.Cfg.ResolvedMediaOrigin = origin
		d.Commit = testCommit
	})
	r := e.do(http.MethodGet, instancePath, nil)
	if r.json["media_delivery"] != config.DeliveryRedirect || r.json["media_origin"] != origin {
		t.Fatalf("redirect mode: %s", r.body)
	}
	// A resolved origin is IGNORED in proxy mode, exactly as MediaOrigin()
	// ignores it: the field names where a viewer fetches media from, and in
	// proxy mode that is this instance.
	e = newEnv(t, func(d *Deps) {
		d.Cfg.MediaDelivery = config.DeliveryProxy
		d.Cfg.ResolvedMediaOrigin = origin
	})
	r = e.do(http.MethodGet, instancePath, nil)
	if r.json["media_delivery"] != config.DeliveryProxy {
		t.Fatalf("proxy mode: %s", r.body)
	}
	if v, ok := r.json["media_origin"]; !ok || v != nil {
		t.Fatalf("proxy mode published a media origin: %s", r.body)
	}
}

// `build` is the COMMIT and is validated on the way out (§20 erratum 303): the
// plane's verifier reads 7-40 hex characters and treats anything else as drift,
// so a value that is not a commit is published as `null` rather than as itself.
func TestInstanceBuildIsAValidatedCommitOrNull(t *testing.T) {
	for _, bad := range []string{
		"",    // the ordinary local build
		"dev", // the VERSION word, in the wrong field
		"0123456789abcdef0123456789abcdef0123456",   // 39
		"0123456789abcdef0123456789abcdef012345678", // 41
		"0123456789ABCDEF0123456789abcdef01234567",  // upper case
		"0123456789abcdef0123456789abcdef0123456g",  // not hex
		"0123456789abcdef0123456789abcdef01234567\n",
		" 0123456789abcdef0123456789abcdef01234567",
		"$(git rev-parse HEAD)", // the shell that was not run
		"v2026.09.17",
	} {
		e := newEnv(t, func(d *Deps) { d.Commit = bad })
		r := e.do(http.MethodGet, instancePath, nil)
		if v, ok := r.json["build"]; !ok || v != nil {
			t.Errorf("Commit=%q published build=%v, want null", bad, v)
		}
		if !strings.Contains(string(r.body), `"build":null`) {
			t.Errorf("Commit=%q: %s", bad, r.body)
		}
	}
	// …and the one shape that IS published, byte for byte.
	e := newEnv(t, func(d *Deps) { d.Commit = testCommit })
	r := e.do(http.MethodGet, instancePath, nil)
	if !strings.Contains(string(r.body), `"build":"`+testCommit+`"`) {
		t.Fatalf("a 40-hex commit is not published as itself: %s", r.body)
	}
}

// An unstamped VERSION answers the word `dev` rather than an empty string: the
// field is a release NAME a verifier looks up, and "" is not one.
func TestInstanceVersionIsNeverEmpty(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Version = "" })
	r := e.do(http.MethodGet, instancePath, nil)
	if r.json["version"] != "dev" {
		t.Fatalf("version = %v, want dev: %s", r.json["version"], r.body)
	}
}

// It is on the `claim` bucket and NOT on a new one (the no-new-limiter rule), which means
// two things a test can see: draining this route refuses the claim route too,
// and neither of them spends the interactions route's tokens.
func TestInstanceSharesTheClaimBucket(t *testing.T) {
	b := newBotEnv(t)
	for i := 0; i < claimsPerMinute; i++ {
		if r := b.do(http.MethodGet, instancePath, nil); r.code != http.StatusOK {
			t.Fatalf("instance GET %d: %d %s", i, r.code, r.body)
		}
	}
	r := b.do(http.MethodGet, instancePath, nil)
	if r.code != http.StatusTooManyRequests || r.errCode() != "rate_limited" || r.hdr.Get("Retry-After") == "" {
		t.Fatalf("instance GET %d: %d %s, want 429 rate_limited with Retry-After", claimsPerMinute+1, r.code, r.body)
	}
	// The SAME bucket, not a second one of its own: the claim endpoint is out
	// of tokens too.
	if c := b.do(http.MethodGet, botClaimPath+"?guild_id=222222222222222222", nil); c.code != http.StatusTooManyRequests {
		t.Fatalf("GET %s after the instance route drained the claim bucket: %d %s (it has a limiter of its own)", botClaimPath, c.code, c.body)
	}
	// …and the official bot's `/clip`s are untouched.
	if p := b.post(`{"type":1}`); p.code != http.StatusOK {
		t.Fatalf("a signed PING after the claim bucket drained: %d %s", p.code, p.body)
	}
}

// /healthz is unchanged by any of this: the two routes report the same version
// and nothing moved between them.
func TestHealthzIsUnchangedByTheInstanceRoute(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Commit = testCommit })
	h := e.do(http.MethodGet, "/healthz", nil)
	if h.code != http.StatusOK {
		t.Fatalf("/healthz = %d %s", h.code, h.body)
	}
	if h.json["version"] != "test" {
		t.Fatalf("/healthz version = %v", h.json["version"])
	}
	for _, name := range []string{"build", "media_delivery", "media_origin"} {
		if _, ok := h.json[name]; ok {
			t.Errorf("/healthz grew a %q key: %s", name, h.body)
		}
	}
}
