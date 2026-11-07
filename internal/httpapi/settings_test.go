package httpapi

import (
	"strings"
	"testing"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/httpx"
)

// --- GET/PATCH /api/auth/me/settings (ROADMAP #22) --------------------------

// TestSettingsRoundTrip: the three TTL states survive the wire, and `null` is
// a different request from `0`.
func TestSettingsRoundTrip(t *testing.T) {
	e := newEnv(t, func(d *Deps) {
		secs := int64(7 * 86400)
		d.Cfg.DefaultTTLSeconds = &secs
	})
	_, cookie := e.newUser("prefs")

	r := e.do("GET", "/api/auth/me/settings", nil, withCookie(cookie))
	if r.code != 200 {
		t.Fatalf("GET settings = %d %s", r.code, r.body)
	}
	if r.json["default_ttl_days"] != nil {
		t.Errorf("a fresh account already has a TTL preference: %v", r.json["default_ttl_days"])
	}
	if r.json["default_unlisted"] != false {
		t.Errorf("default_unlisted = %v", r.json["default_unlisted"])
	}
	if got := r.json["instance_default_ttl_days"]; got != float64(7) {
		t.Errorf("instance_default_ttl_days = %v, want 7", got)
	}

	// "keep my clips forever" is 0 and is REMEMBERED as a choice.
	r = e.doJSON("PATCH", "/api/auth/me/settings", map[string]any{"default_ttl_days": 0, "default_unlisted": true}, withCookie(cookie))
	if r.code != 200 {
		t.Fatalf("PATCH = %d %s", r.code, r.body)
	}
	if r.json["default_ttl_days"] != float64(0) || r.json["default_unlisted"] != true {
		t.Fatalf("PATCH answered %s", r.body)
	}
	r = e.do("GET", "/api/auth/me/settings", nil, withCookie(cookie))
	if r.json["default_ttl_days"] != float64(0) {
		t.Errorf("0 did not survive a round trip: %s", r.body)
	}

	// An ABSENT key leaves the other field alone.
	r = e.doJSON("PATCH", "/api/auth/me/settings", map[string]any{"default_unlisted": false}, withCookie(cookie))
	if r.json["default_ttl_days"] != float64(0) || r.json["default_unlisted"] != false {
		t.Fatalf("an absent key changed the TTL: %s", r.body)
	}

	// …and `null` CLEARS it, back to following the instance.
	r = e.doJSON("PATCH", "/api/auth/me/settings", `{"default_ttl_days":null}`, withCookie(cookie))
	if r.code != 200 || r.json["default_ttl_days"] != nil {
		t.Fatalf("null did not clear the preference: %d %s", r.code, r.body)
	}
}

// TestSettingsRejectIllegalTTL: the same closed set an upload is held to, and
// a non-numeric value is an invalid_ttl rather than a decode error.
func TestSettingsRejectIllegalTTL(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("prefs")
	for _, body := range []string{`{"default_ttl_days":5}`, `{"default_ttl_days":-1}`, `{"default_ttl_days":"7"}`, `{"default_ttl_days":{}}`} {
		r := e.doJSON("PATCH", "/api/auth/me/settings", body, withCookie(cookie))
		if r.code != 400 || r.errCode() != "invalid_ttl" {
			t.Errorf("PATCH %s = %d %s", body, r.code, r.body)
		}
	}
	// …and nothing was written.
	r := e.do("GET", "/api/auth/me/settings", nil, withCookie(cookie))
	if r.json["default_ttl_days"] != nil {
		t.Errorf("a rejected PATCH still wrote: %s", r.body)
	}
}

// TestSettingsPosture: session only, CSRF-guarded, JSON only. A capture token
// must not be able to read or change an account's preferences (erratum 19).
func TestSettingsPosture(t *testing.T) {
	e := newEnv(t)
	u, cookie := e.newUser("prefs")
	tok := e.newToken(u)

	if r := e.do("GET", "/api/auth/me/settings", nil); r.code != 401 {
		t.Errorf("anonymous GET = %d", r.code)
	}
	if r := e.do("GET", "/api/auth/me/settings", nil, withBearer(tok)); r.code != 403 || r.errCode() != "token_not_allowed" {
		t.Errorf("token GET = %d %s", r.code, r.body)
	}
	if r := e.doJSON("PATCH", "/api/auth/me/settings", `{}`, withBearer(tok)); r.code != 403 {
		t.Errorf("token PATCH = %d %s", r.code, r.body)
	}
	if r := e.doJSON("PATCH", "/api/auth/me/settings", `{}`, withCookie(cookie), withHeader("Origin", "https://evil.example")); r.code != 403 {
		t.Errorf("cross-origin PATCH = %d %s", r.code, r.body)
	}
	if r := e.do("PATCH", "/api/auth/me/settings", strings.NewReader("default_ttl_days=1"),
		withCookie(cookie), withHeader("Content-Type", "application/x-www-form-urlencoded")); r.code != 415 {
		t.Errorf("form-encoded PATCH = %d %s", r.code, r.body)
	}
	if r := e.do("POST", "/api/auth/me/settings", nil, withCookie(cookie)); r.code != 405 {
		t.Errorf("POST = %d", r.code)
	}
	// Owner-private answers are never cacheable by a shared cache.
	r := e.do("GET", "/api/auth/me/settings", nil, withCookie(cookie))
	if cc := r.hdr.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q", cc)
	}
}

// --- the defaults an upload actually gets (ROADMAP #22) ---------------------

// TestUploadUsesTheAccountDefaults: the resolution order, on BOTH upload
// routes, with the field always winning.
func TestUploadUsesTheAccountDefaults(t *testing.T) {
	e := newEnv(t, func(d *Deps) {
		secs := int64(86400) // the instance says one day
		d.Cfg.DefaultTTLSeconds = &secs
	})
	u, cookie := e.newUser("prefs")
	if err := e.db.SetUserDefaults(t.Context(), u.ID, db.UserDefaults{TTLDays: intp(30), Unlisted: true}); err != nil {
		t.Fatal(err)
	}

	// (a) The metadata part says nothing → the ACCOUNT's 30 days and unlisted.
	r := e.upload(`{"title":"silent"}`, "clip.mp4", testVideo(64), withCookie(cookie))
	if r.code != 201 {
		t.Fatalf("upload = %d %s", r.code, r.body)
	}
	if r.json["ttl_days"] != float64(30) || r.json["unlisted"] != true {
		t.Fatalf("the account defaults were not applied: %s", r.body)
	}

	// (b) The metadata part says something → the FIELD wins, both ways.
	r = e.upload(`{"title":"loud","ttl_days":1,"unlisted":false}`, "clip.mp4", testVideo(64), withCookie(cookie))
	if r.code != 201 || r.json["ttl_days"] != float64(1) || r.json["unlisted"] != false {
		t.Fatalf("an explicit field lost to the account default: %d %s", r.code, r.body)
	}

	// (c) The capture form agrees, in both directions.
	r = e.capture([]part{filePart("file", "clip.mp4", testVideo(64))}, withCookie(cookie))
	if r.code != 201 {
		t.Fatalf("capture upload = %d %s", r.code, r.body)
	}
	got := e.getClip(r.str("id"))
	if got.TTLSeconds == nil || *got.TTLSeconds != 30*86400 || !got.Unlisted {
		t.Fatalf("the capture route ignored the account defaults: ttl=%v unlisted=%v", got.TTLSeconds, got.Unlisted)
	}
	r = e.capture([]part{
		textPart("ttl_days", "7"), textPart("unlisted", "false"),
		filePart("file", "clip.mp4", testVideo(64)),
	}, withCookie(cookie))
	if r.code != 201 {
		t.Fatalf("capture upload = %d %s", r.code, r.body)
	}
	got = e.getClip(r.str("id"))
	if got.TTLSeconds == nil || *got.TTLSeconds != 7*86400 || got.Unlisted {
		t.Fatalf("an explicit capture field lost: ttl=%v unlisted=%v", got.TTLSeconds, got.Unlisted)
	}

	// (d) An account that has expressed nothing still gets the INSTANCE's.
	_, other := e.newUser("plain")
	r = e.upload(`{"title":"instance"}`, "clip.mp4", testVideo(64), withCookie(other))
	if r.code != 201 || r.json["ttl_days"] != float64(1) || r.json["unlisted"] != false {
		t.Fatalf("the instance default was not applied: %d %s", r.code, r.body)
	}
}

// TestAccountDefaultZeroIsKeepForever: the state migration 0011 exists for —
// an account that chose "never" on an instance whose own default is 7 days.
func TestAccountDefaultZeroIsKeepForever(t *testing.T) {
	e := newEnv(t, func(d *Deps) {
		secs := int64(7 * 86400)
		d.Cfg.DefaultTTLSeconds = &secs
	})
	u, cookie := e.newUser("forever")
	if err := e.db.SetUserDefaults(t.Context(), u.ID, db.UserDefaults{TTLDays: intp(0)}); err != nil {
		t.Fatal(err)
	}
	r := e.upload(`{"title":"forever"}`, "clip.mp4", testVideo(64), withCookie(cookie))
	if r.code != 201 || r.json["ttl_days"] != float64(0) || r.json["expires_at"] != nil {
		t.Fatalf("a chosen keep-forever did not beat DEFAULT_CLIP_TTL: %d %s", r.code, r.body)
	}
}

func intp(n int) *int { return &n }

// --- GET /api/auth/me/stats (ROADMAP #23) ----------------------------------

// TestAccountStatsRoute: the caller's own numbers, session only, and there is
// no spelling of the route that answers for anybody else.
func TestAccountStatsRoute(t *testing.T) {
	e := newEnv(t)
	u, cookie := e.newUser("counter")
	other, otherCookie := e.newUser("other")
	e.readyClip(u, clipOpts{title: "mine"})
	e.seedClip(other, clipOpts{title: "theirs"})

	r := e.do("GET", "/api/auth/me/stats", nil, withCookie(cookie))
	if r.code != 200 {
		t.Fatalf("GET stats = %d %s", r.code, r.body)
	}
	if r.json["clips"] != float64(1) || r.json["ready"] != float64(1) {
		t.Fatalf("stats = %s", r.body)
	}
	if r.json["bytes_total"] == float64(0) {
		t.Errorf("bytes_total is zero although a clip exists: %s", r.body)
	}
	r = e.do("GET", "/api/auth/me/stats", nil, withCookie(otherCookie))
	if r.code != 200 || r.json["clips"] != float64(1) || r.json["ready"] != float64(0) {
		t.Fatalf("the other account's stats = %d %s", r.code, r.body)
	}
	// Posture.
	if r := e.do("GET", "/api/auth/me/stats", nil); r.code != 401 {
		t.Errorf("anonymous stats = %d", r.code)
	}
	if r := e.do("GET", "/api/auth/me/stats", nil, withBearer(e.newToken(u))); r.code != 403 {
		t.Errorf("token stats = %d %s", r.code, r.body)
	}
	// There is no per-user form of this route, and asking for one is a 404.
	if r := e.do("GET", "/api/auth/me/stats/"+other.ID, nil, withCookie(cookie)); r.code != 404 {
		t.Errorf("a per-user stats route answered %d", r.code)
	}
}

// TestInstanceDefaultTTLDaysRendering pins the wire unit of the field the
// settings page labels its first option with.
func TestInstanceDefaultTTLDaysRendering(t *testing.T) {
	if got := instanceDefaultTTLDays(nil); got != 0 {
		t.Errorf("never = %d, want 0", got)
	}
	secs := int64(90 * 86400)
	if got := instanceDefaultTTLDays(&secs); got != 90 {
		t.Errorf("90d = %d", got)
	}
	// …and the value it renders is one config.ParseTTLDays accepts back.
	for _, days := range []int{0, 1, 7, 30, 90} {
		if _, err := config.ParseTTLDays(&days, nil); err != nil {
			t.Errorf("ParseTTLDays(%d) = %v", days, err)
		}
	}
}

// --- /healthz's advisory storage line (ROADMAP #24, §20 erratum 157) -------

// TestHealthzStorageBudget: the object appears only when a budget is
// configured AND a reading exists, and it is a REPORT — nothing about the
// instance's behaviour changes when `over` is true.
func TestHealthzStorageBudget(t *testing.T) {
	holder := &httpx.StorageBudget{}
	e := newEnv(t, func(d *Deps) { d.Budget = holder })

	r := e.do("GET", "/healthz", nil)
	if r.code != 200 || r.has("storage_budget") {
		t.Fatalf("a budget with no reading: %d %s", r.code, r.body)
	}
	holder.Set(50, 0) // measured, but STORAGE_BUDGET_BYTES is off
	if r := e.do("GET", "/healthz", nil); r.has("storage_budget") {
		t.Fatalf("a budget object with the feature off: %s", r.body)
	}
	holder.Set(50, 100)
	r = e.do("GET", "/healthz", nil)
	got, _ := r.json["storage_budget"].(map[string]any)
	if got == nil || got["used"] != float64(50) || got["budget"] != float64(100) || got["over"] != false {
		t.Fatalf("storage_budget = %s", r.body)
	}
	holder.Set(500, 100)
	r = e.do("GET", "/healthz", nil)
	got, _ = r.json["storage_budget"].(map[string]any)
	if got == nil || got["over"] != true {
		t.Fatalf("storage_budget over the line = %s", r.body)
	}
	// It is advisory: an upload still works, over budget, every time.
	u, cookie := e.newUser("overspender")
	_ = u
	if up := e.upload(`{"title":"still fine"}`, "clip.mp4", testVideo(64), withCookie(cookie)); up.code != 201 {
		t.Fatalf("an upload was refused while over the storage budget: %d %s", up.code, up.body)
	}
	// …and /healthz is still ok:true. A budget is not a health problem.
	if r := e.do("GET", "/healthz", nil); r.code != 200 || r.json["ok"] != true {
		t.Errorf("/healthz over budget = %d %s", r.code, r.body)
	}
}
