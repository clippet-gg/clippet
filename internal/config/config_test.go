package config

import (
	"errors"
	"fmt"
	"net/netip"
	"path"
	"strings"
	"testing"
	"time"
)

// env builds a getenv over the minimal required set plus overrides.
func env(overrides map[string]string) func(string) string {
	m := map[string]string{
		"PUBLIC_BASE_URL": "http://localhost:8481",
		"S3_ENDPOINT":     "http://minio:9000",
		"S3_BUCKET":       "clips",
		"S3_ACCESS_KEY":   "ak",
		"S3_SECRET_KEY":   "sk",
	}
	for k, v := range overrides {
		m[k] = v
	}
	return func(k string) string { return m[k] }
}

func mustLoad(t *testing.T, overrides map[string]string) Config {
	t.Helper()
	c, err := Load(env(overrides))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func loadErr(t *testing.T, overrides map[string]string) error {
	t.Helper()
	_, err := Load(env(overrides))
	if err == nil {
		t.Fatalf("Load(%v): expected error", overrides)
	}
	return err
}

func TestDefaults(t *testing.T) {
	c := mustLoad(t, nil)
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"Listen", c.Listen, ":8080"},
		{"PublicBaseURL", c.PublicBaseURL.String(), "http://localhost:8481"},
		{"DataDir", c.DataDir, "/data"},
		{"ScratchDir", c.ScratchDir, "/scratch"},
		{"S3.Endpoint", c.S3.Endpoint, "http://minio:9000"},
		{"S3.Region", c.S3.Region, "us-east-1"},
		{"S3.Bucket", c.S3.Bucket, "clips"},
		{"S3.AccessKey", c.S3.AccessKey, "ak"},
		{"S3.SecretKey", c.S3.SecretKey, "sk"},
		{"S3.ForcePathStyle", c.S3.ForcePathStyle, false},
		{"S3.KeyPrefix", c.S3.KeyPrefix, ""},
		{"S3.OpTimeout", c.S3.OpTimeout, 30 * time.Second},
		{"S3.IdleTimeout", c.S3.IdleTimeout, 2 * time.Minute},
		{"S3.TransferMax", c.S3.TransferMax, 6 * time.Hour},
		{"S3.UploadThreads", c.S3.UploadThreads, 6},
		{"StoragePingAttempts", c.StoragePingAttempts, 10},
		{"StoragePingInterval", c.StoragePingInterval, 3 * time.Second},
		{"DefaultTTLSeconds", c.DefaultTTLSeconds, (*int64)(nil)},
		{"SweepInterval", c.SweepInterval, 60 * time.Second},
		{"SweepGrace", c.SweepGrace, time.Hour},
		{"SweepBatch", c.SweepBatch, 50},
		{"MaxUploadBytes", c.MaxUploadBytes, int64(4294967296)},
		{"ScratchMinFree", c.ScratchMinFree, int64(536870912)},
		{"UploadChunkBytes", c.UploadChunkBytes, int64(52428800)},
		{"CaptureReadyWait", c.CaptureReadyWait, 30 * time.Second},
		{"Transcode.Concurrency", c.Transcode.Concurrency, 1},
		{"Transcode.Preset", c.Transcode.Preset, "veryfast"},
		{"Transcode.CRF", c.Transcode.CRF, 23},
		{"Transcode.MaxFPS", c.Transcode.MaxFPS, 60},
		{"Transcode.MaxW", c.Transcode.MaxW, 1280},
		{"Transcode.MaxH", c.Transcode.MaxH, 720},
		{"Transcode.StallTimeout", c.Transcode.StallTimeout, 5 * time.Minute},
		{"Transcode.HardTimeout", c.Transcode.HardTimeout, 6 * time.Hour},
		{"Transcode.MaxAttempts", c.Transcode.MaxAttempts, 3},
		{"Transcode.MaxTransient", c.Transcode.MaxTransient, 10},
		{"Transcode.FFmpeg", c.Transcode.FFmpeg, "ffmpeg"},
		{"Transcode.FFprobe", c.Transcode.FFprobe, "ffprobe"},
		{"SessionTTL", c.SessionTTL, 720 * time.Hour},
		{"SessionCookie", c.SessionCookie, "clippet_session"},
		{"SessionMaxTTL", c.SessionMaxTTL, 2160 * time.Hour},
		{"Registration", c.Registration, RegistrationFirst},
		{"TrustedProxies", len(c.TrustedProxies), 0},
		{"ViewDedupeTTL", c.ViewDedupeTTL, 30 * time.Minute},
		{"ViewDedupeMax", c.ViewDedupeMax, 50000},
		{"RateAuthPerMin", c.RateAuthPerMin, 10},
		{"RateUploadPerHour", c.RateUploadPerHour, 60},
		{"RateUploadPerHourPerIP", c.RateUploadPerHourPerIP, 120},
		{"RateMaxKeys", c.RateMaxKeys, 10000},
		{"SSRTimeout", c.SSRTimeout, 5 * time.Second},
		{"LogLevel", c.LogLevel, "info"},
		{"LogFormat", c.LogFormat, "text"},
		{"Transcode.Threads", c.Transcode.Threads, 0},
		{"Transcode.HWAccel", c.Transcode.HWAccel, HWAccelNone},
		{"LightTag.Text", c.LightTag.Text, DefaultLightTagText},
		{"LightTag.Font", c.LightTag.Font, ""},
	}
	for _, ck := range checks {
		if ck.got != ck.want {
			t.Errorf("%s = %v, want %v", ck.name, ck.got, ck.want)
		}
	}
}

// TestStoragePingLoop is §20 erratum 283: the boot ping loop's shape became
// configuration so the E2E's step 75 — which boots a probe against a port it
// CLOSED on purpose, twice — stops paying two full 60s retry loops. The
// defaults are pinned in TestDefaults above, which is what stops the knob
// from quietly changing what every operator gets; this pins the rest of the
// contract: both are read, and an attempt count under one is REFUSED at boot
// rather than guessed at (§4's rule) — a process that pinged zero times would
// log "storage unreachable" without ever having asked.
func TestStoragePingLoop(t *testing.T) {
	c := mustLoad(t, map[string]string{"STORAGE_PING_ATTEMPTS": "2", "STORAGE_PING_INTERVAL": "1s"})
	if c.StoragePingAttempts != 2 {
		t.Errorf("StoragePingAttempts = %d, want 2", c.StoragePingAttempts)
	}
	if c.StoragePingInterval != time.Second {
		t.Errorf("StoragePingInterval = %v, want 1s", c.StoragePingInterval)
	}
	for _, in := range []string{"0", "-3", "nine"} {
		if err := loadErr(t, map[string]string{"STORAGE_PING_ATTEMPTS": in}); !strings.Contains(err.Error(), "STORAGE_PING_ATTEMPTS") {
			t.Errorf("STORAGE_PING_ATTEMPTS=%s: error %q does not name the variable", in, err)
		}
	}
	if err := loadErr(t, map[string]string{"STORAGE_PING_INTERVAL": "3 seconds"}); !strings.Contains(err.Error(), "STORAGE_PING_INTERVAL") {
		t.Errorf("an unparsable interval: error %q does not name the variable", err)
	}
}

func TestRequiredVars(t *testing.T) {
	for _, name := range []string{"PUBLIC_BASE_URL", "S3_ENDPOINT", "S3_BUCKET", "S3_ACCESS_KEY", "S3_SECRET_KEY"} {
		err := loadErr(t, map[string]string{name: ""})
		if !strings.Contains(err.Error(), name) {
			t.Errorf("missing %s: error %q does not name the variable", name, err)
		}
	}
}

func TestDefaultTTL(t *testing.T) {
	secs := func(d int64) *int64 { s := d * 86400; return &s }
	cases := []struct {
		in   string
		want *int64
		err  bool
	}{
		{"", nil, false},
		{"never", nil, false},
		{"NEVER", nil, false},
		{"0", nil, false},
		{"1d", secs(1), false},
		{"7d", secs(7), false},
		{"30d", secs(30), false},
		{"90d", secs(90), false},
		{"1", secs(1), false},
		{"7", secs(7), false},
		{"30", secs(30), false},
		{"90", secs(90), false},
		{"5d", nil, true},
		{"2", nil, true},
		{"-7", nil, true},
		{"7x", nil, true},
		{"forever", nil, true},
	}
	for _, c := range cases {
		cfg, err := Load(env(map[string]string{"DEFAULT_CLIP_TTL": c.in}))
		if c.err {
			if err == nil {
				t.Errorf("DEFAULT_CLIP_TTL=%q: expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("DEFAULT_CLIP_TTL=%q: %v", c.in, err)
			continue
		}
		switch {
		case c.want == nil && cfg.DefaultTTLSeconds != nil:
			t.Errorf("DEFAULT_CLIP_TTL=%q: got %d, want never", c.in, *cfg.DefaultTTLSeconds)
		case c.want != nil && cfg.DefaultTTLSeconds == nil:
			t.Errorf("DEFAULT_CLIP_TTL=%q: got never, want %d", c.in, *c.want)
		case c.want != nil && *cfg.DefaultTTLSeconds != *c.want:
			t.Errorf("DEFAULT_CLIP_TTL=%q: got %d, want %d", c.in, *cfg.DefaultTTLSeconds, *c.want)
		}
	}
}

func TestPublicBaseURL(t *testing.T) {
	ok := map[string]string{
		"http://localhost:8481":       "http://localhost:8481",
		"http://localhost:8481/":      "http://localhost:8481",
		"https://clips.example.com":   "https://clips.example.com",
		"https://clips.example.com/":  "https://clips.example.com",
		"http://10.0.0.5":             "http://10.0.0.5",
		"https://clips.example.com//": "https://clips.example.com",
	}
	for in, want := range ok {
		c := mustLoad(t, map[string]string{"PUBLIC_BASE_URL": in})
		if got := c.PublicBaseURL.String(); got != want {
			t.Errorf("PUBLIC_BASE_URL=%q → %q, want %q", in, got, want)
		}
		if c.PublicBaseURL.Path != "" {
			t.Errorf("PUBLIC_BASE_URL=%q: path %q not cleared", in, c.PublicBaseURL.Path)
		}
	}
	bad := []string{
		"localhost:8481",           // no scheme
		"ftp://localhost:8481",     // wrong scheme
		"http://localhost:8481/x",  // path
		"http://localhost:8481/x/", // path
		"http://",                  // no host
		"http://host?x=1",          // query
		"http://host#f",            // fragment
		"http://u:p@host",          // userinfo
		"not a url at all",
	}
	for _, in := range bad {
		loadErr(t, map[string]string{"PUBLIC_BASE_URL": in})
	}
}

func TestS3Endpoint(t *testing.T) {
	bad := []string{
		"minio:9000", "//minio", "http://", "http://:9000", "ftp://minio:9000",
		"https://s3.example.com/prefix", // path would be dropped by minio.New(host)
		"http://minio:9000/clips/",
		"http://minio:9000?x=1",
		"http://minio:9000#frag",
		"http://ak:sk@minio:9000",
	}
	for _, in := range bad {
		loadErr(t, map[string]string{"S3_ENDPOINT": in})
	}
	good := map[string]string{
		"https://acct.r2.cloudflarestorage.com":  "https://acct.r2.cloudflarestorage.com",
		"https://s3.us-west-004.backblazeb2.com": "https://s3.us-west-004.backblazeb2.com",
		"http://minio:9000":                      "http://minio:9000",
		"http://minio:9000/":                     "http://minio:9000", // trailing slash trimmed
	}
	for in, want := range good {
		c := mustLoad(t, map[string]string{"S3_ENDPOINT": in})
		if c.S3.Endpoint != want {
			t.Errorf("endpoint(%q) = %q, want %q", in, c.S3.Endpoint, want)
		}
	}
	if c := mustLoad(t, nil); c.Transcode.FFmpegVersion != "" {
		t.Errorf("FFmpegVersion must not come from the environment, got %q", c.Transcode.FFmpegVersion)
	}
}

// SESSION_COOKIE_NAME must be an RFC 6265 token (§20 erratum 28): net/http
// silently drops a Set-Cookie with any other name, which would boot a server
// nobody can stay logged in to.
func TestSessionCookieName(t *testing.T) {
	bad := []string{
		"clippet session", "clippet:session", "clippet/session", "clippet;s", "clippet=s", "clippet,s",
		`"clippet_session"`, // a quoted .env value handed over verbatim
		"clippet\tsession", "clippet(s)", "clippet[1]", "clippet@host", "clippét",
	}
	for _, in := range bad {
		err := loadErr(t, map[string]string{"SESSION_COOKIE_NAME": in})
		if !strings.Contains(err.Error(), "SESSION_COOKIE_NAME") || !strings.Contains(err.Error(), "RFC 6265") {
			t.Errorf("cookie name %q: error %q does not name the variable and the rule", in, err)
		}
	}
	for _, in := range []string{"clippet_session", "clippet-session", "clippet.sid", "s3ss10n!", "a"} {
		if c := mustLoad(t, map[string]string{"SESSION_COOKIE_NAME": in}); c.SessionCookie != in {
			t.Errorf("cookie name %q: got %q", in, c.SessionCookie)
		}
	}
	if c := mustLoad(t, map[string]string{"SESSION_COOKIE_NAME": "  "}); c.SessionCookie != "clippet_session" {
		t.Errorf("blank cookie name: got %q, want the default", c.SessionCookie)
	}
}

// A __Host-/__Secure- name is a valid RFC 6265 token, but §14 sets Secure
// from PUBLIC_BASE_URL's scheme alone and a browser discards a prefixed
// cookie that arrives without it — erratum 28's "nobody can stay logged in,
// nothing is logged" through the browser's rule (§20 erratum 35).
func TestSessionCookiePrefixNeedsHTTPS(t *testing.T) {
	for _, in := range []string{"__Host-clippet", "__Secure-clippet", "__Host-clippet_session"} {
		err := loadErr(t, map[string]string{"SESSION_COOKIE_NAME": in}) // the fixture base is http://
		if !strings.Contains(err.Error(), "SESSION_COOKIE_NAME") || !strings.Contains(err.Error(), "https") {
			t.Errorf("cookie name %q on http: error %q does not name the variable and the rule", in, err)
		}
		c := mustLoad(t, map[string]string{"SESSION_COOKIE_NAME": in, "PUBLIC_BASE_URL": "https://clips.example.com"})
		if c.SessionCookie != in {
			t.Errorf("cookie name %q on https: got %q", in, c.SessionCookie)
		}
	}
	// The prefix rule only fires on the prefix itself.
	if c := mustLoad(t, map[string]string{"SESSION_COOKIE_NAME": "__host-clippet"}); c.SessionCookie != "__host-clippet" {
		t.Errorf("lowercase __host- must be an ordinary name: %q", c.SessionCookie)
	}
}

func TestKeyPrefix(t *testing.T) {
	cases := map[string]string{
		"":          "",
		"clippet":   "clippet/",
		"clippet/":  "clippet/",
		"a/b":       "a/b/",
		"/clippet/": "clippet/",
	}
	for in, want := range cases {
		c := mustLoad(t, map[string]string{"S3_KEY_PREFIX": in})
		if c.S3.KeyPrefix != want {
			t.Errorf("S3_KEY_PREFIX=%q → %q, want %q", in, c.S3.KeyPrefix, want)
		}
	}
	for _, bad := range []string{"../x", "a b", "clips?x"} {
		loadErr(t, map[string]string{"S3_KEY_PREFIX": bad})
	}
}

func TestTrustedProxies(t *testing.T) {
	c := mustLoad(t, map[string]string{"TRUSTED_PROXIES": "127.0.0.1/32, 172.16.0.0/12,::1,10.1.2.3"})
	want := []string{"127.0.0.1/32", "172.16.0.0/12", "::1/128", "10.1.2.3/32"}
	if len(c.TrustedProxies) != len(want) {
		t.Fatalf("got %v, want %v", c.TrustedProxies, want)
	}
	for i, w := range want {
		if c.TrustedProxies[i].String() != w {
			t.Errorf("prefix[%d] = %s, want %s", i, c.TrustedProxies[i], w)
		}
	}
	if !c.TrustedProxies[1].Contains(netip.MustParseAddr("172.20.0.9")) {
		t.Error("172.16.0.0/12 should contain 172.20.0.9")
	}
	loadErr(t, map[string]string{"TRUSTED_PROXIES": "127.0.0.1/32,garbage"})
	loadErr(t, map[string]string{"TRUSTED_PROXIES": "300.0.0.1/8"})
}

func TestEvenDimensions(t *testing.T) {
	loadErr(t, map[string]string{"TRANSCODE_MAX_WIDTH": "1281"})
	loadErr(t, map[string]string{"TRANSCODE_MAX_HEIGHT": "721"})
	loadErr(t, map[string]string{"TRANSCODE_MAX_WIDTH": "0"})
	loadErr(t, map[string]string{"TRANSCODE_MAX_HEIGHT": "1"})
	c := mustLoad(t, map[string]string{"TRANSCODE_MAX_WIDTH": "1920", "TRANSCODE_MAX_HEIGHT": "1080"})
	if c.Transcode.MaxW != 1920 || c.Transcode.MaxH != 1080 {
		t.Errorf("box = %dx%d", c.Transcode.MaxW, c.Transcode.MaxH)
	}
}

func TestBooleansAndEnums(t *testing.T) {
	for in, want := range map[string]bool{"true": true, "1": true, "false": false, "0": false, "TRUE": true} {
		c := mustLoad(t, map[string]string{"S3_FORCE_PATH_STYLE": in})
		if c.S3.ForcePathStyle != want {
			t.Errorf("S3_FORCE_PATH_STYLE=%q → %v", in, c.S3.ForcePathStyle)
		}
	}
	loadErr(t, map[string]string{"S3_FORCE_PATH_STYLE": "yes"})
	// REGISTRATION is a THREE-state enum since §20 erratum 106, and `first`
	// -- not `open` -- is the default (asserted in the defaults table above).
	for in, want := range map[string]RegistrationMode{
		"open": RegistrationOpen, "first": RegistrationFirst, "closed": RegistrationClosed,
		"OPEN": RegistrationOpen,
	} {
		got := mustLoad(t, map[string]string{"REGISTRATION": in}).Registration
		if got != want {
			t.Errorf("REGISTRATION=%q → %q, want %q", in, got, want)
		}
	}
	if RegistrationOpen.NeedsFirstUser() || RegistrationClosed.NeedsFirstUser() || !RegistrationFirst.NeedsFirstUser() {
		t.Error("NeedsFirstUser must be true for `first` alone")
	}
	if !RegistrationOpen.AlwaysOpen() || RegistrationFirst.AlwaysOpen() || RegistrationClosed.AlwaysOpen() {
		t.Error("AlwaysOpen must be true for `open` alone")
	}
	if RegistrationMode("").String() != "first" {
		t.Errorf("the zero mode reads as %q, want first", RegistrationMode("").String())
	}
	loadErr(t, map[string]string{"REGISTRATION": "maybe"})
	// The two Discord modes (§20 erratum 341): `members` and `role:<id>`, the id
	// a snowflake; both keep the web form closed and neither needs the users table.
	for in, want := range map[string]RegistrationMode{
		"members": RegistrationMembers, "MEMBERS": RegistrationMembers,
		"role:123456789012345678": "role:123456789012345678", "ROLE:123456789012345678": "role:123456789012345678",
	} {
		got := mustLoad(t, map[string]string{"REGISTRATION": in}).Registration
		if got != want || !got.DiscordSignIn() || got.AlwaysOpen() || got.NeedsFirstUser() {
			t.Errorf("REGISTRATION=%q → %q (signin=%v open=%v first=%v), want %q, signin only", in, got,
				got.DiscordSignIn(), got.AlwaysOpen(), got.NeedsFirstUser(), want)
		}
	}
	if r := RegistrationMode("role:123456789012345678").SignInRole(); r != "123456789012345678" {
		t.Errorf("SignInRole = %q", r)
	}
	if RegistrationMembers.SignInRole() != "" || RegistrationOpen.DiscordSignIn() || RegistrationClosed.DiscordSignIn() || RegistrationFirst.DiscordSignIn() {
		t.Error("only members and role:<id> are Discord sign-in modes")
	}
	for _, bad := range []string{"role:", "role:moderators", "role:12", "role:1234567890123456789012345678"} {
		loadErr(t, map[string]string{"REGISTRATION": bad})
	}
	loadErr(t, map[string]string{"LOG_LEVEL": "verbose"})
	loadErr(t, map[string]string{"LOG_FORMAT": "xml"})
	loadErr(t, map[string]string{"TRANSCODE_PRESET": "turbo"})
	c := mustLoad(t, map[string]string{"LOG_LEVEL": "DEBUG", "LOG_FORMAT": "json", "TRANSCODE_PRESET": "medium"})
	if c.LogLevel != "debug" || c.LogFormat != "json" || c.Transcode.Preset != "medium" {
		t.Errorf("enums: %+v", c)
	}
}

func TestNumericValidation(t *testing.T) {
	loadErr(t, map[string]string{"TRANSCODE_CRF": "52"})
	loadErr(t, map[string]string{"TRANSCODE_CRF": "-1"})
	loadErr(t, map[string]string{"TRANSCODE_CONCURRENCY": "0"})
	loadErr(t, map[string]string{"SWEEP_BATCH": "0"})
	loadErr(t, map[string]string{"MAX_UPLOAD_BYTES": "0"})
	loadErr(t, map[string]string{"MAX_UPLOAD_BYTES": "4G"})
	// The chunk of the web upload is bounded both ways (§20 erratum 344).
	loadErr(t, map[string]string{"UPLOAD_CHUNK_BYTES": "5242879"})
	loadErr(t, map[string]string{"UPLOAD_CHUNK_BYTES": "95000001"})
	if c := mustLoad(t, map[string]string{"UPLOAD_CHUNK_BYTES": "5242880"}); c.UploadChunkBytes != 5<<20 {
		t.Errorf("the smallest chunk = %d", c.UploadChunkBytes)
	}
	// "unlimited" written as math.MaxInt64: the +1 MiB multipart slack would
	// wrap negative and MaxBytesReader would 413 every upload (§20 erratum 33).
	if err := loadErr(t, map[string]string{"MAX_UPLOAD_BYTES": "9223372036854775807"}); !strings.Contains(err.Error(), "MAX_UPLOAD_BYTES") {
		t.Errorf("MaxInt64 upload cap: %v", err)
	}
	if c := mustLoad(t, map[string]string{"MAX_UPLOAD_BYTES": "4611686018427387904"}); c.MaxUploadBytes != 1<<62 {
		t.Errorf("4 EiB upload cap = %d", c.MaxUploadBytes)
	}
	loadErr(t, map[string]string{"TRANSCODE_MAX_ATTEMPTS": "0"})
	loadErr(t, map[string]string{"RATE_UPLOAD_PER_HOUR_PER_IP": "0"})
	if c := mustLoad(t, map[string]string{"RATE_UPLOAD_PER_HOUR_PER_IP": "5"}); c.RateUploadPerHourPerIP != 5 {
		t.Errorf("RATE_UPLOAD_PER_HOUR_PER_IP = %d", c.RateUploadPerHourPerIP)
	}
	loadErr(t, map[string]string{"STORAGE_IDLE_TIMEOUT": "0s"})
	loadErr(t, map[string]string{"STORAGE_IDLE_TIMEOUT": "2 minutes"})
	loadErr(t, map[string]string{"SESSION_TTL": "-1h"})
	c := mustLoad(t, map[string]string{"SCRATCH_MIN_FREE_BYTES": "0", "STORAGE_IDLE_TIMEOUT": "90s", "TRANSCODE_MAX_TRANSIENT": "4"})
	if c.ScratchMinFree != 0 || c.S3.IdleTimeout != 90*time.Second || c.Transcode.MaxTransient != 4 {
		t.Errorf("numerics: %+v", c)
	}
}

// TestCaptureReadyWait: the one §4 duration whose ZERO is a setting rather
// than a misconfiguration, and the only one with a hard ceiling (§20 erratum
// 97). Both ends must fail loudly rather than be clamped silently at boot.
func TestCaptureReadyWait(t *testing.T) {
	if c := mustLoad(t, map[string]string{"CAPTURE_READY_WAIT": "0"}); c.CaptureReadyWait != 0 {
		t.Errorf("CAPTURE_READY_WAIT=0 → %s, want 0 (the way to switch the wait off)", c.CaptureReadyWait)
	}
	if c := mustLoad(t, map[string]string{"CAPTURE_READY_WAIT": "20s"}); c.CaptureReadyWait != 20*time.Second {
		t.Errorf("CAPTURE_READY_WAIT=20s → %s", c.CaptureReadyWait)
	}
	if c := mustLoad(t, map[string]string{"CAPTURE_READY_WAIT": "60s"}); c.CaptureReadyWait != CaptureReadyWaitMax {
		t.Errorf("CAPTURE_READY_WAIT at the ceiling → %s", c.CaptureReadyWait)
	}
	for _, bad := range []string{"61s", "5m", "-1s", "8", "eight seconds"} {
		if err := loadErr(t, map[string]string{"CAPTURE_READY_WAIT": bad}); !strings.Contains(err.Error(), "CAPTURE_READY_WAIT") {
			t.Errorf("CAPTURE_READY_WAIT=%q: error %q does not name the variable", bad, err)
		}
	}
}

// TestTranscodeThreads: 0 is AUTO and not "no threads" (§20 erratum 215), and a
// number past the ceiling is a boot error rather than a thousand x264 threads.
func TestTranscodeThreads(t *testing.T) {
	if c := mustLoad(t, map[string]string{"TRANSCODE_THREADS": "0"}); c.Transcode.Threads != 0 {
		t.Errorf("TRANSCODE_THREADS=0 → %d, want 0 (auto)", c.Transcode.Threads)
	}
	if c := mustLoad(t, map[string]string{"TRANSCODE_THREADS": "4"}); c.Transcode.Threads != 4 {
		t.Errorf("TRANSCODE_THREADS=4 → %d", c.Transcode.Threads)
	}
	for _, bad := range []string{"-1", "129", "four"} {
		if err := loadErr(t, map[string]string{"TRANSCODE_THREADS": bad}); !strings.Contains(err.Error(), "TRANSCODE_THREADS") {
			t.Errorf("TRANSCODE_THREADS=%q: error %q does not name the variable", bad, err)
		}
	}
}

// TestStorageUploadThreads: 6 by default, 1 the sequential path, and 0 or a
// number past the memory ceiling a boot error naming the variable (§20
// erratum 337).
func TestStorageUploadThreads(t *testing.T) {
	if c := mustLoad(t, map[string]string{}); c.S3.UploadThreads != 6 {
		t.Errorf("default STORAGE_UPLOAD_THREADS → %d, want 6", c.S3.UploadThreads)
	}
	if c := mustLoad(t, map[string]string{"STORAGE_UPLOAD_THREADS": "1"}); c.S3.UploadThreads != 1 {
		t.Errorf("STORAGE_UPLOAD_THREADS=1 → %d", c.S3.UploadThreads)
	}
	for _, bad := range []string{"0", "33", "six"} {
		if err := loadErr(t, map[string]string{"STORAGE_UPLOAD_THREADS": bad}); !strings.Contains(err.Error(), "STORAGE_UPLOAD_THREADS") {
			t.Errorf("STORAGE_UPLOAD_THREADS=%q: error %q does not name the variable", bad, err)
		}
	}
}

// TestTranscodeHWAccel: empty and `none` are the CPU argv, `cuda` is the only
// hardware path this build renders, and anything else fails at boot with a
// message naming the one value that works (§20 erratum 216).
func TestTranscodeHWAccel(t *testing.T) {
	for _, off := range []string{"", "none", "NONE"} {
		if c := mustLoad(t, map[string]string{"TRANSCODE_HWACCEL": off}); c.Transcode.HWAccel != HWAccelNone {
			t.Errorf("TRANSCODE_HWACCEL=%q → %q, want the CPU argv", off, c.Transcode.HWAccel)
		}
	}
	for _, on := range []string{"cuda", "CUDA", " cuda "} {
		if c := mustLoad(t, map[string]string{"TRANSCODE_HWACCEL": on}); c.Transcode.HWAccel != HWAccelCUDA {
			t.Errorf("TRANSCODE_HWACCEL=%q → %q, want cuda", on, c.Transcode.HWAccel)
		}
	}
	for _, bad := range []string{"nvenc", "qsv", "vaapi", "yes"} {
		err := loadErr(t, map[string]string{"TRANSCODE_HWACCEL": bad})
		if !strings.Contains(err.Error(), "TRANSCODE_HWACCEL") || !strings.Contains(err.Error(), "cuda") {
			t.Errorf("TRANSCODE_HWACCEL=%q: error %q must name the variable and the one value that works", bad, err)
		}
	}
}

// TestLightTagText: the template, the off-switch, and the one thing refused —
// a value that cannot be ONE line, because the text is burned into a frame
// (§20 erratum 214).
func TestLightTagText(t *testing.T) {
	if c := mustLoad(t, map[string]string{}); c.LightTag.Text != DefaultLightTagText {
		t.Errorf("absent → %q, want the shipped template", c.LightTag.Text)
	}
	for _, off := range []string{"off", "OFF", "Off"} {
		if c := mustLoad(t, map[string]string{"LIGHT_TAG_TEXT": off}); c.LightTag.Text != "" {
			t.Errorf("LIGHT_TAG_TEXT=%q → %q, want the tag off", off, c.LightTag.Text)
		}
	}
	const own = "{light} · {best} at the link"
	if c := mustLoad(t, map[string]string{"LIGHT_TAG_TEXT": own}); c.LightTag.Text != own {
		t.Errorf("an operator's own template → %q", c.LightTag.Text)
	}
	for _, bad := range []string{"two\nlines", "a\ttab", strings.Repeat("x", MaxLightTagRunes+1)} {
		if err := loadErr(t, map[string]string{"LIGHT_TAG_TEXT": bad}); !strings.Contains(err.Error(), "LIGHT_TAG_TEXT") {
			t.Errorf("LIGHT_TAG_TEXT=%q: error %q does not name the variable", bad, err)
		}
	}
	// The font is never read from the environment: serve.go resolves it from the
	// fonts the image ships, and clears the text when it finds none.
	if c := mustLoad(t, map[string]string{"LIGHT_TAG_FONT": "/nope.ttf"}); c.LightTag.Font != "" {
		t.Errorf("LightTag.Font = %q, want empty — it is resolved at boot, not configured", c.LightTag.Font)
	}
}

func TestParseTTLDays(t *testing.T) {
	i := func(v int) *int { return &v }
	s := func(v int64) *int64 { return &v }
	def := s(7 * 86400)
	cases := []struct {
		name string
		v    *int
		def  *int64
		want *int64
		err  error
	}{
		{"absent+default", nil, def, s(7 * 86400), nil},
		{"absent+never", nil, nil, nil, nil},
		{"zero", i(0), def, nil, nil},
		{"one", i(1), def, s(86400), nil},
		{"seven", i(7), nil, s(7 * 86400), nil},
		{"thirty", i(30), nil, s(30 * 86400), nil},
		{"ninety", i(90), nil, s(90 * 86400), nil},
		{"two", i(2), def, nil, ErrInvalidTTL},
		{"negative", i(-1), def, nil, ErrInvalidTTL},
		{"huge", i(365), def, nil, ErrInvalidTTL},
	}
	for _, c := range cases {
		got, err := ParseTTLDays(c.v, c.def)
		if !errors.Is(err, c.err) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.err)
			continue
		}
		switch {
		case got == nil && c.want != nil:
			t.Errorf("%s: got never, want %d", c.name, *c.want)
		case got != nil && c.want == nil:
			t.Errorf("%s: got %d, want never", c.name, *got)
		case got != nil && *got != *c.want:
			t.Errorf("%s: got %d, want %d", c.name, *got, *c.want)
		}
	}
	// The default is copied, never aliased.
	got, _ := ParseTTLDays(nil, def)
	*got = 1
	if *def != 7*86400 {
		t.Error("ParseTTLDays aliased the default pointer")
	}
}

// TestDiscordWebhookAccepted: every host of the allowlist, and the id/token
// split, on the shape Discord's "Copy Webhook URL" button produces.
func TestDiscordWebhookAccepted(t *testing.T) {
	for _, host := range []string{"discord.com", "discordapp.com", "ptb.discord.com", "canary.discord.com"} {
		raw := "https://" + host + "/api/webhooks/1234567890123456789/aBcD-_eF0123456789xyz"
		c := mustLoad(t, map[string]string{"DISCORD_WEBHOOK_URL": raw})
		w := c.DiscordWebhook
		if !w.Configured() || w.URL() != raw || w.Host() != host || w.ID() != "1234567890123456789" {
			t.Fatalf("%s: %+v (url=%q host=%q id=%q)", host, w, w.URL(), w.Host(), w.ID())
		}
	}
	// Upper-case host is normalised; surrounding whitespace is trimmed like
	// every other variable.
	c := mustLoad(t, map[string]string{"DISCORD_WEBHOOK_URL": "  https://Discord.com/api/webhooks/12/tok  "})
	if c.DiscordWebhook.Host() != "discord.com" || c.DiscordWebhook.ID() != "12" {
		t.Fatalf("normalisation: %+v", c.DiscordWebhook)
	}
	// Unset is the default and is not "configured".
	if w := mustLoad(t, nil).DiscordWebhook; w.Configured() || w.URL() != "" || w.String() != "(unset)" {
		t.Fatalf("unset: %+v %q", w, w.String())
	}
}

// TestDiscordWebhookRejections: one case per rejection class, and every
// message names the rule without echoing the secret.
func TestDiscordWebhookRejections(t *testing.T) {
	const token = "s3cr3t-token-value"
	cases := []struct {
		name, raw, want string
	}{
		{"not a URL", "https://discord.com/api/webhooks/1/" + token + "\n\x7f%zz", "not a valid URL"},
		{"no scheme", "discord.com/api/webhooks/1/" + token, "scheme must be https"},
		{"http", "http://discord.com/api/webhooks/1/" + token, "scheme must be https"},
		{"foreign host", "https://evil.example/api/webhooks/1/" + token, "host must be exactly one of"},
		{"lookalike host", "https://discord.com.evil.example/api/webhooks/1/" + token, "host must be exactly one of"},
		{"subdomain", "https://cdn.discord.com/api/webhooks/1/" + token, "host must be exactly one of"},
		{"port", "https://discord.com:8443/api/webhooks/1/" + token, "host must be exactly one of"},
		// U+0130 folds to an ASCII "i" under strings.ToLower; the host is
		// folded for ASCII case alone (§20 erratum 327), raw or encoded.
		{"U+0130 raw", "https://dİscord.com/api/webhooks/1/" + token, "host must be exactly one of"},
		{"U+0130 encoded", "https://d%C4%B0scord.com/api/webhooks/1/" + token, "host must be exactly one of"},
		{"userinfo", "https://u:p@discord.com/api/webhooks/1/" + token, "must not contain userinfo"},
		{"query", "https://discord.com/api/webhooks/1/" + token + "?wait=true", "query string or fragment"},
		{"fragment", "https://discord.com/api/webhooks/1/" + token + "#x", "query string or fragment"},
		// A trailing '?' parses as ForceQuery with an EMPTY RawQuery and a
		// trailing '#' leaves Fragment empty, so both used to slip through —
		// and both then mangled the "?wait=true" the notifier appends.
		{"trailing question mark", "https://discord.com/api/webhooks/1/" + token + "?", "query string or fragment"},
		{"trailing hash", "https://discord.com/api/webhooks/1/" + token + "#", "query string or fragment"},
		// The token is one URL-safe segment. Anything net/url would re-escape
		// on the wire is rejected here, so the configured spelling and the
		// spelling Discord sees are the same string.
		{"space in the token", "https://discord.com/api/webhooks/1/tok en", "path must be /api/webhooks/"},
		{"percent-encoded space in the token", "https://discord.com/api/webhooks/1/tok%20en", "path must be /api/webhooks/"},
		{"colon in the token", "https://discord.com/api/webhooks/1/tok:en", "path must be /api/webhooks/"},
		{"plus in the token", "https://discord.com/api/webhooks/1/tok+en", "path must be /api/webhooks/"},
		{"wrong path", "https://discord.com/api/v10/webhooks/1/" + token, "path must be /api/webhooks/"},
		{"root path", "https://discord.com/", "path must be /api/webhooks/"},
		{"non-numeric id", "https://discord.com/api/webhooks/abc/" + token, "path must be /api/webhooks/"},
		{"no token segment", "https://discord.com/api/webhooks/1234567890", "path must be /api/webhooks/"},
		{"empty token segment", "https://discord.com/api/webhooks/1234567890/", "path must be /api/webhooks/"},
		{"slack variant", "https://discord.com/api/webhooks/1/" + token + "/slack", "path must be /api/webhooks/"},
	}
	for _, c := range cases {
		err := loadErr(t, map[string]string{"DISCORD_WEBHOOK_URL": c.raw})
		msg := err.Error()
		if !strings.HasPrefix(msg, "DISCORD_WEBHOOK_URL: ") {
			t.Errorf("%s: %q does not name the variable", c.name, msg)
		}
		if !strings.Contains(msg, c.want) {
			t.Errorf("%s: %q does not name the rule %q", c.name, msg, c.want)
		}
		// The value is a secret: no rejection may echo the token or the URL.
		if strings.Contains(msg, token) || strings.Contains(msg, c.raw) {
			t.Errorf("%s: the error echoes the secret: %q", c.name, msg)
		}
	}
}

// TestDiscordWebhookRedaction: the token never survives a %v, a %s or an
// slog value — the three ways a config value accidentally reaches a log.
func TestDiscordWebhookRedaction(t *testing.T) {
	const raw = "https://discord.com/api/webhooks/99/TOKEN-THAT-MUST-NOT-LEAK"
	c := mustLoad(t, map[string]string{"DISCORD_WEBHOOK_URL": raw})
	renders := []string{
		fmt.Sprintf("%v", c.DiscordWebhook),
		fmt.Sprintf("%s", c.DiscordWebhook), //nolint:staticcheck // S1025: the %s verb is the path under test
		c.DiscordWebhook.String(),
		c.DiscordWebhook.LogValue().String(),
		fmt.Sprintf("%v", c), // the whole Config, printed by accident
	}
	for i, s := range renders {
		if strings.Contains(s, "TOKEN-THAT-MUST-NOT-LEAK") {
			t.Fatalf("render %d leaks the webhook token: %s", i, s)
		}
		if i < 4 && !strings.Contains(s, "discord.com/99") {
			t.Fatalf("render %d dropped the useful half: %s", i, s)
		}
	}
	if c.DiscordWebhook.URL() != raw {
		t.Fatal("URL() must return the raw value — it is what internal/notify puts on the wire")
	}
}

// TestSocialAndBitrateKnobs are the two §17 #3 variables (§20 errata 121,
// 124). Both have a ZERO that is a SETTING — "no social rendition at all" and
// "no bitrate ceiling" — so neither may be rejected or clamped at boot; the
// bitrate's non-zero range is bounded because a value like `2500` (an
// operator thinking in kilobits) would otherwise ruin every clip silently.
func TestSocialAndBitrateKnobs(t *testing.T) {
	c := mustLoad(t, nil)
	if c.SocialMaxBytes != 25<<20 {
		t.Errorf("SOCIAL_MAX_BYTES default = %d, want %d", c.SocialMaxBytes, 25<<20)
	}
	if c.Transcode.MaxBitrate != 0 {
		t.Errorf("TRANSCODE_MAX_BITRATE default = %d, want 0 (off)", c.Transcode.MaxBitrate)
	}
	if c := mustLoad(t, map[string]string{"SOCIAL_MAX_BYTES": "0"}); c.SocialMaxBytes != 0 {
		t.Errorf("SOCIAL_MAX_BYTES=0 → %d, want 0 (the way to switch the feature off)", c.SocialMaxBytes)
	}
	if c := mustLoad(t, map[string]string{"SOCIAL_MAX_BYTES": "104857600"}); c.SocialMaxBytes != 100<<20 {
		t.Errorf("SOCIAL_MAX_BYTES=100 MiB → %d", c.SocialMaxBytes)
	}
	loadErr(t, map[string]string{"SOCIAL_MAX_BYTES": "-1"})
	loadErr(t, map[string]string{"SOCIAL_MAX_BYTES": "25M"})

	if c := mustLoad(t, map[string]string{"TRANSCODE_MAX_BITRATE": "2500000"}); c.Transcode.MaxBitrate != 2_500_000 {
		t.Errorf("TRANSCODE_MAX_BITRATE = %d", c.Transcode.MaxBitrate)
	}
	// Kilobits, not bits: the mistake that must fail at boot rather than
	// encode 2.5 kbps of video.
	if err := loadErr(t, map[string]string{"TRANSCODE_MAX_BITRATE": "2500"}); !strings.Contains(err.Error(), "TRANSCODE_MAX_BITRATE") {
		t.Errorf("a kilobit value must name the variable: %v", err)
	}
	loadErr(t, map[string]string{"TRANSCODE_MAX_BITRATE": "-1"})
	loadErr(t, map[string]string{"TRANSCODE_MAX_BITRATE": "2500k"})
	loadErr(t, map[string]string{"TRANSCODE_MAX_BITRATE": "999999999999999"})
}

// TestFullCodec (§20 erratum 204): `h264` is the default because it is the only
// codec every browser, every Discord client and every embedded player decodes,
// `av1` is the other value, and anything else is fatal at boot rather than a
// guess — an operator who typed `hevc` must learn that this build does not offer
// it before a clip is encoded, not after.
func TestFullCodec(t *testing.T) {
	if c := mustLoad(t, nil); c.Transcode.FullCodec != FullCodecH264 {
		t.Errorf("the default FULL_CODEC = %q, want %q", c.Transcode.FullCodec, FullCodecH264)
	}
	for in, want := range map[string]string{
		"av1":  FullCodecAV1,
		"AV1":  FullCodecAV1,
		"h264": FullCodecH264,
		"H264": FullCodecH264,
		"":     FullCodecH264,
	} {
		if c := mustLoad(t, map[string]string{"FULL_CODEC": in}); c.Transcode.FullCodec != want {
			t.Errorf("FULL_CODEC=%q -> %q, want %q", in, c.Transcode.FullCodec, want)
		}
	}
	for _, bad := range []string{"hevc", "vp9", "libx264", "av2"} {
		loadErr(t, map[string]string{"FULL_CODEC": bad})
	}
}

// TestLibraryScope (§20 erratum 113): `shared` is the default because it is
// what every deployment did before the variable existed, `own` is the other
// value, and anything else is fatal at boot rather than a guess.
func TestLibraryScope(t *testing.T) {
	if c := mustLoad(t, nil); c.LibraryScope != LibraryScopeShared {
		t.Errorf("the default LIBRARY_SCOPE = %q, want %q", c.LibraryScope, LibraryScopeShared)
	}
	for in, want := range map[string]string{
		"own":    LibraryScopeOwn,
		"OWN":    LibraryScopeOwn,
		"shared": LibraryScopeShared,
		"":       LibraryScopeShared,
	} {
		if c := mustLoad(t, map[string]string{"LIBRARY_SCOPE": in}); c.LibraryScope != want {
			t.Errorf("LIBRARY_SCOPE=%q → %q, want %q", in, c.LibraryScope, want)
		}
	}
	loadErr(t, map[string]string{"LIBRARY_SCOPE": "private"})
	loadErr(t, map[string]string{"LIBRARY_SCOPE": "mine"})
}

// TestEveryBadVariableIsReported is §20 erratum 104: the loader used to keep
// only the FIRST error, so an operator with three typos in .env learned about
// them one crash-loop restart at a time — while the loader had in fact already
// evaluated every one of them.
func TestEveryBadVariableIsReported(t *testing.T) {
	err := loadErr(t, map[string]string{
		"TRANSCODE_CRF":         "52",
		"SWEEP_INTERVAL":        "60 seconds",
		"LOG_FORMAT":            "yaml",
		"SESSION_COOKIE_NAME":   "bad name",
		"TRANSCODE_CONCURRENCY": "0",
	})
	var ce *Errors
	if !errors.As(err, &ce) {
		t.Fatalf("Load returned %T, want a *config.Errors", err)
	}
	if len(ce.Errs) != 5 {
		t.Fatalf("%d errors, want 5:\n%s", len(ce.Errs), err)
	}
	for _, name := range []string{"TRANSCODE_CRF", "SWEEP_INTERVAL", "LOG_FORMAT", "SESSION_COOKIE_NAME", "TRANSCODE_CONCURRENCY"} {
		found := false
		for _, e := range ce.Errs {
			if strings.HasPrefix(e.Error(), name+": ") {
				found = true
			}
		}
		if !found {
			t.Errorf("no error names %s:\n%s", name, err)
		}
	}
	// A single failure still reads as one message, exactly as before.
	single := loadErr(t, map[string]string{"TRANSCODE_CRF": "52"})
	if !strings.HasPrefix(single.Error(), "TRANSCODE_CRF: ") || strings.Contains(single.Error(), ";") {
		t.Errorf("a lone error must print as itself: %q", single)
	}
}

// TRANSCODE_CONCURRENCY has a ceiling now (§20 erratum 104): every slot is a
// live ffmpeg in the process that also serves HTTP, so a typo must fail at
// boot rather than at the OOM killer.
func TestTranscodeConcurrencyCeiling(t *testing.T) {
	if c := mustLoad(t, map[string]string{"TRANSCODE_CONCURRENCY": "16"}); c.Transcode.Concurrency != MaxTranscodeConcurrency {
		t.Errorf("TRANSCODE_CONCURRENCY=16 = %d", c.Transcode.Concurrency)
	}
	err := loadErr(t, map[string]string{"TRANSCODE_CONCURRENCY": "40"})
	if !strings.Contains(err.Error(), "TRANSCODE_CONCURRENCY") || !strings.Contains(err.Error(), "16") {
		t.Errorf("TRANSCODE_CONCURRENCY=40: %v", err)
	}
}

// The two optional storage prices (ROADMAP #39, §20 erratum 270). Three things
// are being pinned: UNSET is nil and not zero (the cost line is then absent
// everywhere, which is the whole rule the feature rests on), a price is held
// exactly in micros whatever it was written as, and every rejection names its
// own variable.
func TestStoragePrices(t *testing.T) {
	c := mustLoad(t, nil)
	if c.StoragePriceMicros != nil || c.EgressPriceMicros != nil {
		t.Fatalf("unset prices must be nil (absent, not zero): %v %v", c.StoragePriceMicros, c.EgressPriceMicros)
	}
	cases := []struct {
		in   string
		want int64
	}{
		{"0", 0},                          // "this costs me nothing" IS an answer
		{"0.015", 15_000},                 // R2 storage
		{"0.01", 10_000},                  // B2 egress
		{"1", 1_000_000},                  //
		{"0.5", 500_000},                  // a short fraction is right-padded
		{".015", 15_000},                  // somebody typing fast
		{"0.000001", 1},                   // the finest price there is
		{"12.34", 12_340_000},             //
		{"1000000", 1_000_000_000_000},    // at the ceiling
		{" 0.36 ", 360_000},               // .env lines carry spaces
		{"00000015", 15_000_000},          // leading zeros are padding, not magnitude (§20 erratum 275)
		{"000.015", 15_000},               // …and the fraction behind them is still exact
		{"0001000000", 1_000_000_000_000}, // the ceiling, written wide
	}
	for _, tc := range cases {
		c := mustLoad(t, map[string]string{"STORAGE_PRICE_PER_GB_MONTH": tc.in, "EGRESS_PRICE_PER_GB": tc.in})
		if c.StoragePriceMicros == nil || *c.StoragePriceMicros != tc.want {
			t.Errorf("STORAGE_PRICE_PER_GB_MONTH=%q → %v micros, want %d", tc.in, c.StoragePriceMicros, tc.want)
		}
		if c.EgressPriceMicros == nil || *c.EgressPriceMicros != tc.want {
			t.Errorf("EGRESS_PRICE_PER_GB=%q → %v micros, want %d", tc.in, c.EgressPriceMicros, tc.want)
		}
	}
	// Either one alone: a friend on R2 prices storage and nothing else, which is
	// DEPLOY §31's worked example.
	only := mustLoad(t, map[string]string{"STORAGE_PRICE_PER_GB_MONTH": "0.015"})
	if only.StoragePriceMicros == nil || only.EgressPriceMicros != nil {
		t.Errorf("one price set alone: %v %v", only.StoragePriceMicros, only.EgressPriceMicros)
	}
	bad := []string{
		"-1",        // a negative price is a typo, never a discount
		"0.0000001", // a seventh digit could not be stored
		"1000001",   // past the typo guard
		"1e-3",      // no exponent
		"$0.015",    // the app is never told a currency
		"0,015",     // no separator
		"1.2.3",     //
		"0.",        // a trailing point
		"abc",       //
		"0.015 /GB", //
	}
	for _, v := range bad {
		err := loadErr(t, map[string]string{"STORAGE_PRICE_PER_GB_MONTH": v})
		if !strings.Contains(err.Error(), "STORAGE_PRICE_PER_GB_MONTH") {
			t.Errorf("STORAGE_PRICE_PER_GB_MONTH=%q: error %q does not name the variable", v, err)
		}
		err = loadErr(t, map[string]string{"EGRESS_PRICE_PER_GB": v})
		if !strings.Contains(err.Error(), "EGRESS_PRICE_PER_GB") {
			t.Errorf("EGRESS_PRICE_PER_GB=%q: error %q does not name the variable", v, err)
		}
	}
}

// TestStoragePricingPresetName: STORAGE_PRICING takes one of a closed set or
// nothing, and a value outside it is a boot error that NAMES the set — the
// shape every other closed-set variable here has (§20 erratum 277).
func TestStoragePricingPresetName(t *testing.T) {
	if c := mustLoad(t, nil); c.StoragePricing != "" {
		t.Errorf("the shipped default is %q, want the custom case (empty)", c.StoragePricing)
	}
	for _, name := range PricingPresets {
		if c := mustLoad(t, map[string]string{"STORAGE_PRICING": name}); c.StoragePricing != name {
			t.Errorf("STORAGE_PRICING=%s → %q", name, c.StoragePricing)
		}
		// The loader lower-cases what it reads, so an operator shouting at
		// their `.env` still gets the row.
		up := strings.ToUpper(name)
		if c := mustLoad(t, map[string]string{"STORAGE_PRICING": up}); c.StoragePricing != name {
			t.Errorf("STORAGE_PRICING=%s → %q, want %q", up, c.StoragePricing, name)
		}
	}
	// `.env` lines carry spaces and the loader trims them, exactly as it does
	// for a price: this is the same variable an operator edits by hand.
	if c := mustLoad(t, map[string]string{"STORAGE_PRICING": "  r2  "}); c.StoragePricing != PricingR2 {
		t.Errorf("a padded value → %q", c.StoragePricing)
	}
	for _, bad := range []string{"gcs", "wasabi", "azure", "r2x", "0.015", "none"} {
		err := loadErr(t, map[string]string{"STORAGE_PRICING": bad})
		if !strings.Contains(err.Error(), "STORAGE_PRICING") {
			t.Errorf("STORAGE_PRICING=%q: error %q does not name the variable", bad, err)
		}
		for _, name := range PricingPresets {
			if !strings.Contains(err.Error(), name) {
				t.Errorf("STORAGE_PRICING=%q: error %q does not name the accepted value %q", bad, err, name)
			}
		}
	}
}

// TestStorageBudgetHardNeedsABudget: the cap is opt-in, off by default, and
// refuses to boot without a line to cap at — `1` with no budget could only mean
// "refuse at zero bytes", which is a total outage from one variable nobody
// meant that way (§20 erratum 277).
func TestStorageBudgetHardNeedsABudget(t *testing.T) {
	if c := mustLoad(t, nil); c.StorageBudgetHard {
		t.Error("the cap is on by default")
	}
	if c := mustLoad(t, map[string]string{"STORAGE_BUDGET_BYTES": "1000"}); c.StorageBudgetHard {
		t.Error("a budget alone armed the cap")
	}
	c := mustLoad(t, map[string]string{"STORAGE_BUDGET_BYTES": "1000", "STORAGE_BUDGET_HARD": "1"})
	if !c.StorageBudgetHard || c.StorageBudgetBytes != 1000 {
		t.Errorf("the armed pair = hard:%v budget:%d", c.StorageBudgetHard, c.StorageBudgetBytes)
	}
	if c := mustLoad(t, map[string]string{"STORAGE_BUDGET_BYTES": "1000", "STORAGE_BUDGET_HARD": "0"}); c.StorageBudgetHard {
		t.Error("STORAGE_BUDGET_HARD=0 armed the cap")
	}
	for _, env := range []map[string]string{
		{"STORAGE_BUDGET_HARD": "1"},
		{"STORAGE_BUDGET_HARD": "true", "STORAGE_BUDGET_BYTES": "0"},
	} {
		err := loadErr(t, env)
		if !strings.Contains(err.Error(), "STORAGE_BUDGET_HARD") || !strings.Contains(err.Error(), "STORAGE_BUDGET_BYTES") {
			t.Errorf("%v: the error %q must name both variables", env, err)
		}
	}
	if err := loadErr(t, map[string]string{"STORAGE_BUDGET_HARD": "maybe"}); !strings.Contains(err.Error(), "STORAGE_BUDGET_HARD") {
		t.Errorf("a non-boolean value: %q", err)
	}
}

// --- STORAGE_BACKEND / STORAGE_DIR (ROADMAP #37, §20 erratum 276) -------------

// fsEnv is the `fs` configuration: no S3_* at all, which is the state the
// backend exists to make possible.
func fsEnv(overrides map[string]string) func(string) string {
	m := map[string]string{
		"PUBLIC_BASE_URL": "http://localhost:8481",
		"STORAGE_BACKEND": "fs",
		"STORAGE_DIR":     "/media",
		"S3_ENDPOINT":     "",
		"S3_BUCKET":       "",
		"S3_ACCESS_KEY":   "",
		"S3_SECRET_KEY":   "",
	}
	for k, v := range overrides {
		m[k] = v
	}
	return func(k string) string { return m[k] }
}

// §20 erratum 345: STORAGE_BACKEND unset follows the four bucket variables —
// any of them set is `s3` (every `.env` from before the folder became the
// default; TestRequiredVars keeps a bucket missing one line on `s3`), none set
// is the folder store.
func TestStorageBackendUnsetFollowsTheEndpoint(t *testing.T) {
	c := mustLoad(t, map[string]string{"S3_ENDPOINT": "", "S3_BUCKET": "", "S3_ACCESS_KEY": "", "S3_SECRET_KEY": "", "STORAGE_DIR": "/media"})
	if c.StorageBackend != BackendFS {
		t.Fatalf("STORAGE_BACKEND unset with no S3_* = %q, want %q", c.StorageBackend, BackendFS)
	}
	c = mustLoad(t, map[string]string{"S3_ENDPOINT": "https://acc.r2.cloudflarestorage.com"})
	if c.StorageBackend != BackendS3 {
		t.Fatalf("STORAGE_BACKEND unset with S3_ENDPOINT = %q, want %q", c.StorageBackend, BackendS3)
	}
	// A bucket missing one of its lines is still a bucket: the error names the line.
	err := loadErr(t, map[string]string{"S3_ENDPOINT": ""})
	if !strings.Contains(err.Error(), "S3_ENDPOINT") || strings.Contains(err.Error(), "STORAGE_BACKEND=fs") {
		t.Fatalf("S3_ENDPOINT empty beside the other three: %q", err)
	}
}

// TestBucketWithoutEndpointNamesTheUpgrade is §20 erratum 348 (d): an old
// `.env` with the bundled server's active `S3_BUCKET=clips` and no endpoint is
// told which section moves those clips, not a bare "required" — and an
// endpoint missing with no bucket either stays the plain "required".
func TestBucketWithoutEndpointNamesTheUpgrade(t *testing.T) {
	err := loadErr(t, map[string]string{"S3_ENDPOINT": "", "S3_ACCESS_KEY": "", "S3_SECRET_KEY": ""})
	for _, want := range []string{"S3_ENDPOINT: required: S3_BUCKET is set but S3_ENDPOINT is not", "bundled bucket server", "DEPLOY.md §32.6"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("an old .env's bucket line alone: %q lacks %q", err, want)
		}
	}
	err = loadErr(t, map[string]string{"STORAGE_BACKEND": "s3", "S3_ENDPOINT": "", "S3_BUCKET": ""})
	if !strings.Contains(err.Error(), "S3_ENDPOINT: required") || strings.Contains(err.Error(), "§32.6") {
		t.Fatalf("no endpoint and no bucket: %q", err)
	}
}

func TestStorageBackendDefaultsToS3(t *testing.T) {
	c := mustLoad(t, nil) // the fixture names a bucket endpoint
	if c.StorageBackend != BackendS3 {
		t.Fatalf("STORAGE_BACKEND unset = %q, want %q", c.StorageBackend, BackendS3)
	}
	if c.StorageDir != "" {
		t.Fatalf("STORAGE_DIR unset = %q", c.StorageDir)
	}
	// An explicit `s3` is the same configuration, and STORAGE_DIR is allowed
	// there — it is what `admin storage migrate --to fs` copies into.
	c = mustLoad(t, map[string]string{"STORAGE_BACKEND": "s3", "STORAGE_DIR": "/media"})
	if c.StorageBackend != BackendS3 || c.StorageDir != "/media" {
		t.Fatalf("s3 with a STORAGE_DIR: %q %q", c.StorageBackend, c.StorageDir)
	}
	if err := loadErr(t, map[string]string{"STORAGE_BACKEND": "nas"}); !strings.Contains(err.Error(), "STORAGE_BACKEND") {
		t.Fatalf("an unknown backend: %v", err)
	}
}

func TestStorageBackendFS(t *testing.T) {
	c, err := Load(fsEnv(nil))
	if err != nil {
		t.Fatalf("Load(fs): %v", err)
	}
	if c.StorageBackend != BackendFS || c.StorageDir != "/media" {
		t.Fatalf("fs: %q %q", c.StorageBackend, c.StorageDir)
	}
	// The key space and the clock still apply; the bucket does not exist.
	if c.S3.Endpoint != "" || c.S3.Bucket != "" || c.S3.AccessKey != "" || c.S3.SecretKey != "" {
		t.Fatalf("fs carried a bucket: %+v", c.S3)
	}
	if c.S3.OpTimeout == 0 {
		t.Fatal("fs lost STORAGE_OP_TIMEOUT")
	}
}

// TestStorageFSRefusesEveryS3Var is the §15 half: on `fs` a leftover bucket
// variable is a BOOT ERROR, so an operator reading `.env` can see that this
// instance has no outbound destination for media.
func TestStorageFSRefusesEveryS3Var(t *testing.T) {
	for _, name := range S3Vars {
		_, err := Load(fsEnv(map[string]string{name: "something"}))
		if err == nil {
			t.Fatalf("STORAGE_BACKEND=fs accepted %s", name)
		}
		if !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "takes no S3_* variable") {
			t.Fatalf("%s on fs: error %q", name, err)
		}
	}
	// All four at once are all four errors (§20 erratum 104).
	_, err := Load(fsEnv(map[string]string{
		"S3_ENDPOINT": "http://minio:9000", "S3_BUCKET": "clips",
		"S3_ACCESS_KEY": "ak", "S3_SECRET_KEY": "sk",
	}))
	var errs *Errors
	if !errors.As(err, &errs) || len(errs.Errs) != 4 {
		t.Fatalf("four leftovers = %v", err)
	}
}

// TestLoadForMigrateAcceptsBoth: the one entry point that may see both stores,
// and the only way to reach it is `admin storage migrate` (cmd/clippet/main.go).
func TestLoadForMigrateAcceptsBoth(t *testing.T) {
	getenv := fsEnv(map[string]string{
		"S3_ENDPOINT": "http://minio:9000", "S3_BUCKET": "clips",
		"S3_ACCESS_KEY": "ak", "S3_SECRET_KEY": "sk",
	})
	c, err := LoadForMigrate(getenv)
	if err != nil {
		t.Fatalf("LoadForMigrate: %v", err)
	}
	if c.StorageBackend != BackendFS || c.StorageDir != "/media" || c.S3.Bucket != "clips" || c.S3.Endpoint != "http://minio:9000" {
		t.Fatalf("LoadForMigrate = %+v", c)
	}
	// It still validates the shape of what it read, and it still does not
	// REQUIRE any of it (a migration to fs needs no bucket).
	if _, err := LoadForMigrate(fsEnv(map[string]string{"S3_ENDPOINT": "minio:9000"})); err == nil {
		t.Fatal("LoadForMigrate accepted an endpoint with no scheme")
	}
	if _, err := LoadForMigrate(fsEnv(nil)); err != nil {
		t.Fatalf("LoadForMigrate with no bucket at all: %v", err)
	}
	// On `s3` it is exactly Load: the four are still required.
	if _, err := LoadForMigrate(env(map[string]string{"S3_BUCKET": ""})); err == nil {
		t.Fatal("LoadForMigrate dropped the s3 requirement")
	}
}

// TestMigrateS3VarsAreLoadForMigrateOnly: the seven MIGRATE_S3_* variables are
// read by LoadForMigrate and by nothing else (§20 erratum 335) — Load never
// asks for one, so `serve` neither uses nor mentions them.
func TestMigrateS3VarsAreLoadForMigrateOnly(t *testing.T) {
	m := map[string]string{
		"MIGRATE_S3_ENDPOINT": "http://other:9000/", "MIGRATE_S3_BUCKET": "next",
		"MIGRATE_S3_ACCESS_KEY": "ak2", "MIGRATE_S3_SECRET_KEY": "sk2",
		"MIGRATE_S3_REGION": "auto", "MIGRATE_S3_FORCE_PATH_STYLE": "true", "MIGRATE_S3_KEY_PREFIX": "moved",
	}
	base := env(m)
	var asked []string
	spy := func(k string) string { asked = append(asked, k); return base(k) }
	c, err := Load(spy)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, k := range asked {
		if strings.HasPrefix(k, "MIGRATE_S3_") {
			t.Fatalf("Load read %s", k)
		}
	}
	if c.MigrateS3 != (S3Config{}) || c.MigrateS3Set() {
		t.Fatalf("Load filled MigrateS3: %+v", c.MigrateS3)
	}
	c, err = LoadForMigrate(base)
	if err != nil {
		t.Fatalf("LoadForMigrate: %v", err)
	}
	want := S3Config{Endpoint: "http://other:9000", Bucket: "next", AccessKey: "ak2", SecretKey: "sk2",
		Region: "auto", ForcePathStyle: true, KeyPrefix: "moved/"}
	if c.MigrateS3 != want || !c.MigrateS3Set() || len(c.MigrateS3Missing()) != 0 {
		t.Fatalf("LoadForMigrate MigrateS3 = %+v", c.MigrateS3)
	}
	// The defaults: region us-east-1, virtual-host style, and the source's own
	// key prefix.
	c, err = LoadForMigrate(env(map[string]string{"S3_KEY_PREFIX": "pre/",
		"MIGRATE_S3_ENDPOINT": "http://other:9000", "MIGRATE_S3_BUCKET": "next",
		"MIGRATE_S3_ACCESS_KEY": "ak2", "MIGRATE_S3_SECRET_KEY": "sk2"}))
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if c.MigrateS3.Region != "us-east-1" || c.MigrateS3.ForcePathStyle || c.MigrateS3.KeyPrefix != "pre/" {
		t.Fatalf("defaults = %+v", c.MigrateS3)
	}
	// Shapes are validated as the S3_* ones are.
	for name, v := range map[string]string{
		"MIGRATE_S3_ENDPOINT": "other:9000", "MIGRATE_S3_FORCE_PATH_STYLE": "maybe", "MIGRATE_S3_KEY_PREFIX": "../x",
	} {
		if _, err := LoadForMigrate(env(map[string]string{name: v})); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s=%q: %v", name, v, err)
		}
	}
}

// TestMigrateS3RequiredTogether: the four bucket variables come together —
// MigrateS3Missing names every empty one, sorted, the moment any is set; and
// none set is not a bucket-to-bucket run at all.
func TestMigrateS3RequiredTogether(t *testing.T) {
	c, err := LoadForMigrate(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.MigrateS3Set() {
		t.Fatal("none set reads as set")
	}
	c, err = LoadForMigrate(env(map[string]string{"MIGRATE_S3_BUCKET": "next", "MIGRATE_S3_ENDPOINT": "http://other:9000"}))
	if err != nil {
		t.Fatal(err)
	}
	if !c.MigrateS3Set() {
		t.Fatal("two set reads as none")
	}
	if got := strings.Join(c.MigrateS3Missing(), ","); got != "MIGRATE_S3_ACCESS_KEY,MIGRATE_S3_SECRET_KEY" {
		t.Fatalf("missing = %s", got)
	}
	if strings.Join(MigrateS3Vars, ",") != "MIGRATE_S3_ENDPOINT,MIGRATE_S3_BUCKET,MIGRATE_S3_ACCESS_KEY,MIGRATE_S3_SECRET_KEY" {
		t.Fatalf("MigrateS3Vars = %v", MigrateS3Vars)
	}
}

func TestStorageDirRules(t *testing.T) {
	bad := map[string]string{
		"":               "required",
		"media":          "absolute",
		"./media":        "absolute",
		"/media/../etc":  "clean",
		"/":              "root",
		"/data":          "DATA_DIR",
		"/data/media":    "DATA_DIR",
		"/scratch":       "SCRATCH_DIR",
		"/scratch/media": "SCRATCH_DIR",
	}
	for v, want := range bad {
		_, err := Load(fsEnv(map[string]string{"STORAGE_DIR": v}))
		if err == nil {
			t.Fatalf("STORAGE_DIR=%q was accepted", v)
		}
		if !strings.Contains(err.Error(), "STORAGE_DIR") || !strings.Contains(err.Error(), want) {
			t.Fatalf("STORAGE_DIR=%q: error %q does not mention %q", v, err, want)
		}
	}
	// A STORAGE_DIR that CONTAINS one of them is refused from the other side.
	_, err := Load(fsEnv(map[string]string{"STORAGE_DIR": "/media", "DATA_DIR": "/media/db"}))
	if err == nil || !strings.Contains(err.Error(), "CONTAINS") {
		t.Fatalf("a STORAGE_DIR around DATA_DIR: %v", err)
	}
	// Good ones, including a trailing slash and a sibling that merely SHARES a
	// prefix with DATA_DIR's name.
	for _, v := range []string{"/media", "/media/", "/mnt/nas/clippet", "/datax"} {
		c, err := Load(fsEnv(map[string]string{"STORAGE_DIR": v}))
		if err != nil {
			t.Fatalf("STORAGE_DIR=%q: %v", v, err)
		}
		if strings.HasSuffix(c.StorageDir, "/") {
			t.Fatalf("STORAGE_DIR=%q was not cleaned: %q", v, c.StorageDir)
		}
	}
}

// TestMediaDelivery (§20 erratum 278) pins the whole of the new block: the
// shipped default, the three refusals, and what the presign origin resolves to
// in each mode.
func TestMediaDelivery(t *testing.T) {
	// The default is byte-for-byte the behaviour every version before this
	// one had, and it exposes no media origin at all.
	c := mustLoad(t, nil)
	if c.MediaDelivery != DeliveryProxy || c.RedirectsMedia() {
		t.Fatalf("default MEDIA_DELIVERY = %q", c.MediaDelivery)
	}
	if c.MediaPresignTTL != DefaultMediaPresignTTL {
		t.Errorf("default MEDIA_PRESIGN_TTL = %s", c.MediaPresignTTL)
	}
	if c.MediaOrigin() != "" || c.S3.PresignEndpoint != "" {
		t.Errorf("proxy mode exposes a media origin: %q", c.MediaOrigin())
	}
	// MediaOrigin() is the RESOLVED origin and nothing else: the loader cannot
	// know it, because it depends on minio-go's bucket-lookup rule (§20 erratum
	// 278, amended). It stays empty until `serve`/`admin doctor` fill it in
	// from store.MediaOrigin, and it is empty in proxy mode whatever is in it.
	c.ResolvedMediaOrigin = "https://clips.acct.example"
	if c.MediaOrigin() != "" {
		t.Errorf("proxy mode answered a resolved origin: %q", c.MediaOrigin())
	}
	// A presign endpoint set while proxy is in force is IGNORED, not refused:
	// it is what `.env` looks like while an operator gets ready to flip the
	// switch. It is also never silent — MediaNote is what the boot line and
	// doctor's `media` row print.
	c = mustLoad(t, map[string]string{"MEDIA_PRESIGN_ENDPOINT": "https://clips.example.test"})
	if c.MediaOrigin() != "" {
		t.Errorf("proxy mode used MEDIA_PRESIGN_ENDPOINT: %q", c.MediaOrigin())
	}
	note := MediaNote(c, env(map[string]string{"MEDIA_PRESIGN_ENDPOINT": "https://clips.example.test"}))
	if !strings.Contains(note, "MEDIA_PRESIGN_ENDPOINT") || !strings.Contains(note, "ignored") {
		t.Errorf("note = %q", note)
	}
	if MediaNote(mustLoad(t, nil), env(nil)) != "" {
		t.Error("a note with nothing set")
	}
	// Redirect defaults the presign origin to S3_ENDPOINT, which is right for
	// R2/B2/S3 (one origin for both) and what the compose stack overrides.
	c = mustLoad(t, map[string]string{"MEDIA_DELIVERY": "redirect"})
	if !c.RedirectsMedia() || c.S3.PresignEndpoint != "http://minio:9000" {
		t.Fatalf("redirect: presign endpoint = %q", c.S3.PresignEndpoint)
	}
	if c.MediaOrigin() != "" {
		t.Errorf("MediaOrigin() answered before anything resolved it: %q", c.MediaOrigin())
	}
	c.ResolvedMediaOrigin = "http://clips.minio:9000" // what a virtual-host presign lands on
	if c.MediaOrigin() != "http://clips.minio:9000" {
		t.Errorf("MediaOrigin() = %q, want the RESOLVED origin and not the endpoint", c.MediaOrigin())
	}
	if MediaNote(c, env(map[string]string{"MEDIA_PRESIGN_TTL": "5m"})) != "" {
		t.Error("redirect mode says a MEDIA_PRESIGN_* variable is ignored")
	}
	c = mustLoad(t, map[string]string{"MEDIA_DELIVERY": "redirect",
		"MEDIA_PRESIGN_ENDPOINT": "https://clips.example.test/", "MEDIA_PRESIGN_TTL": "15m"})
	if c.S3.PresignEndpoint != "https://clips.example.test" {
		t.Errorf("presign endpoint = %q (a trailing slash is trimmed)", c.S3.PresignEndpoint)
	}
	if c.MediaPresignTTL != 15*time.Minute {
		t.Errorf("MEDIA_PRESIGN_TTL = %s", c.MediaPresignTTL)
	}
	// (1) An unknown mode names the two that exist.
	if err := loadErr(t, map[string]string{"MEDIA_DELIVERY": "public-base"}); !strings.Contains(err.Error(), "MEDIA_DELIVERY") ||
		!strings.Contains(err.Error(), DeliveryRedirect) {
		t.Errorf("bad mode: %v", err)
	}
	// (2) A directory has no URL to redirect to.
	err := loadErr(t, map[string]string{"MEDIA_DELIVERY": "redirect", "STORAGE_BACKEND": "fs",
		"STORAGE_DIR": "/media", "S3_ENDPOINT": "", "S3_BUCKET": "", "S3_ACCESS_KEY": "", "S3_SECRET_KEY": ""})
	if !strings.Contains(err.Error(), "MEDIA_DELIVERY") || !strings.Contains(err.Error(), "STORAGE_BACKEND=fs") {
		t.Errorf("redirect on fs: %v", err)
	}
	// (3) Plain-http media under an https origin is active mixed content: the
	// card would work and the watch page would not, which is the worst shape a
	// misconfiguration can have.
	err = loadErr(t, map[string]string{"PUBLIC_BASE_URL": "https://clips.example",
		"MEDIA_DELIVERY": "redirect", "MEDIA_PRESIGN_ENDPOINT": "http://minio:9000"})
	if !strings.Contains(err.Error(), "MEDIA_PRESIGN_ENDPOINT") || !strings.Contains(err.Error(), "mixed content") {
		t.Errorf("mixed content: %v", err)
	}
	// The same shape over plain http is fine — that is the dev stack.
	if c := mustLoad(t, map[string]string{"MEDIA_DELIVERY": "redirect", "MEDIA_PRESIGN_ENDPOINT": "http://minio:9000"}); c.S3.PresignEndpoint != "http://minio:9000" {
		t.Errorf("http base + http bucket: %q", c.S3.PresignEndpoint)
	}
	// And so is an https bucket under an https origin.
	if c := mustLoad(t, map[string]string{"PUBLIC_BASE_URL": "https://clips.example",
		"MEDIA_DELIVERY": "redirect", "S3_ENDPOINT": "https://acct.r2.cloudflarestorage.com"}); c.S3.PresignEndpoint != "https://acct.r2.cloudflarestorage.com" {
		t.Errorf("https pair: %q", c.S3.PresignEndpoint)
	}
	// The TTL is bounded on BOTH sides and refused rather than clamped: a
	// value under the floor is somebody who meant minutes and typed seconds,
	// and serving them an hour would hide it until a viewer's seek broke.
	for _, bad := range []string{"10s", "48h", "0", "-1h", "nonsense"} {
		if err := loadErr(t, map[string]string{"MEDIA_PRESIGN_TTL": bad}); !strings.Contains(err.Error(), "MEDIA_PRESIGN_TTL") {
			t.Errorf("MEDIA_PRESIGN_TTL=%s: %v", bad, err)
		}
	}
	// A malformed presign endpoint is refused for its SHAPE in either mode,
	// exactly as S3_ENDPOINT is: a path or a query would be dropped silently.
	for _, bad := range []string{"minio:9000", "https://clips.example.test/bucket", "https://u:p@clips.example.test"} {
		if err := loadErr(t, map[string]string{"MEDIA_PRESIGN_ENDPOINT": bad}); !strings.Contains(err.Error(), "MEDIA_PRESIGN_ENDPOINT") {
			t.Errorf("MEDIA_PRESIGN_ENDPOINT=%s: %v", bad, err)
		}
	}
}

// TestInstanceBadge (§20 erratum 285): the word an instance that is not the
// production one wears. Unset is the production instance and the default.
func TestInstanceBadge(t *testing.T) {
	if c := mustLoad(t, nil); c.InstanceBadge != "" {
		t.Errorf("InstanceBadge defaults to %q, want empty (the production instance)", c.InstanceBadge)
	}
	for _, v := range []string{"dev", "staging", "lab-test", "d", "box2", "a1-b2-c3", "123456789012"} {
		if c := mustLoad(t, map[string]string{"INSTANCE_BADGE": v}); c.InstanceBadge != v {
			t.Errorf("INSTANCE_BADGE=%q loaded as %q", v, c.InstanceBadge)
		}
	}
	// Surrounding whitespace is the loader's own TrimSpace, as for every other
	// variable: `INSTANCE_BADGE= dev` in a .env is the word `dev`.
	if c := mustLoad(t, map[string]string{"INSTANCE_BADGE": "  dev  "}); c.InstanceBadge != "dev" {
		t.Errorf("INSTANCE_BADGE=\"  dev  \" loaded as %q, want \"dev\"", c.InstanceBadge)
	}
	// Everything else is a BOOT ERROR naming the variable and the rule — never a
	// silent lowercasing or a trim, because an operator who wrote `Dev` meant
	// something and a page that quietly disagrees with the file that configured
	// it is the failure this whole feature exists to prevent.
	for _, v := range []string{
		"Dev",                  // uppercase
		"dev box",              // a space
		"dev_box",              // an underscore
		"dev.box",              // a dot
		"dév",                  // not ASCII
		"dev/",                 // a path separator
		"<b>",                  // markup
		"dev\"",                // a quote — it lands in an HTML attribute
		"1234567890123",        // thirteen
		"aaaaaaaaaaaaaaaaaaaa", // twenty
	} {
		err := loadErr(t, map[string]string{"INSTANCE_BADGE": v})
		if !strings.Contains(err.Error(), "INSTANCE_BADGE") {
			t.Errorf("INSTANCE_BADGE=%q: error %q does not name the variable", v, err)
		}
	}
	// The length rule says so in its own words, and the charset rule in its own.
	if err := loadErr(t, map[string]string{"INSTANCE_BADGE": "1234567890123"}); !strings.Contains(err.Error(), "at most 12") {
		t.Errorf("the length refusal does not state the bound: %v", err)
	}
	if err := loadErr(t, map[string]string{"INSTANCE_BADGE": "Dev"}); !strings.Contains(err.Error(), "lowercase") {
		t.Errorf("the charset refusal does not state the rule: %v", err)
	}
}

// --- DROP_FOLDER_OWNER (ROADMAP #42 (b), §20 erratum 296) --------------------

// The default is OFF, which is every instance that existed before it.
func TestDropFolderOwnerDefaultsToOff(t *testing.T) {
	if c := mustLoad(t, nil); c.DropFolderOwner != "" {
		t.Fatalf("DROP_FOLDER_OWNER unset = %q", c.DropFolderOwner)
	}
	c, err := Load(fsEnv(nil))
	if err != nil {
		t.Fatalf("Load(fs): %v", err)
	}
	if c.DropFolderOwner != "" {
		t.Fatalf("DROP_FOLDER_OWNER unset on fs = %q", c.DropFolderOwner)
	}
}

// On `fs` it names the one account the folder ingests as.
func TestDropFolderOwnerOnFS(t *testing.T) {
	c, err := Load(fsEnv(map[string]string{"DROP_FOLDER_OWNER": "carol"}))
	if err != nil {
		t.Fatalf("Load(fs, drop): %v", err)
	}
	if c.DropFolderOwner != "carol" {
		t.Fatalf("DROP_FOLDER_OWNER = %q", c.DropFolderOwner)
	}
}

// On `s3` with a media DIRECTORY it is accepted, and the clips still go to the
// bucket (§20 erratum 296 (m)): the watcher wants a folder on this box, which
// is a thing a bucket deployment can have and STORAGE_DIR already names.
func TestDropFolderOwnerOnS3WithAMediaDirectory(t *testing.T) {
	c := mustLoad(t, map[string]string{"DROP_FOLDER_OWNER": "carol", "STORAGE_DIR": "/media"})
	if c.DropFolderOwner != "carol" {
		t.Fatalf("DROP_FOLDER_OWNER on s3 = %q", c.DropFolderOwner)
	}
	if c.StorageBackend != BackendS3 || c.StorageDir != "/media" {
		t.Fatalf("the backend moved: backend=%q dir=%q", c.StorageBackend, c.StorageDir)
	}
}

// On `s3` with NO directory it is refused, naming the variable that supplies
// one. The old refusal said "switch the backend", which was the wrong fix: the
// watcher never needed `fs`, it needed a folder.
func TestDropFolderOwnerOnS3NeedsAMediaDirectory(t *testing.T) {
	err := loadErr(t, map[string]string{"DROP_FOLDER_OWNER": "carol"})
	if !strings.Contains(err.Error(), "DROP_FOLDER_OWNER") {
		t.Fatalf("the refusal must name the variable: %v", err)
	}
	if !strings.Contains(err.Error(), "STORAGE_DIR") {
		t.Fatalf("the refusal must name the variable that supplies the folder: %v", err)
	}
}

// The SHAPE is register's own rule, and that is as far as this package can go:
// whether the account exists is `clippet serve`'s question at boot.
func TestDropFolderOwnerShape(t *testing.T) {
	for _, bad := range []string{"ab", "has space", "wat!", strings.Repeat("x", 33), "b-o-b"} {
		if _, err := Load(fsEnv(map[string]string{"DROP_FOLDER_OWNER": bad})); err == nil {
			t.Fatalf("DROP_FOLDER_OWNER=%q was accepted", bad)
		}
	}
	for _, ok := range []string{"bob", "Carol_9", strings.Repeat("x", 32)} {
		c, err := Load(fsEnv(map[string]string{"DROP_FOLDER_OWNER": ok}))
		if err != nil {
			t.Fatalf("DROP_FOLDER_OWNER=%q: %v", ok, err)
		}
		if c.DropFolderOwner != ok {
			t.Fatalf("DROP_FOLDER_OWNER=%q parsed as %q", ok, c.DropFolderOwner)
		}
	}
}

// CARD_COMPONENTS (§20 erratum 316): ON by default since the owner's real-channel
// test, `on`/`off` in any case, anything else a boot error naming the variable.
func TestCardComponents(t *testing.T) {
	if !mustLoad(t, nil).CardComponents {
		t.Error("CARD_COMPONENTS is off by default")
	}
	for v, want := range map[string]bool{"on": true, "ON": true, "off": false, "OFF": false, "": true} {
		if c := mustLoad(t, map[string]string{"CARD_COMPONENTS": v}); c.CardComponents != want {
			t.Errorf("CARD_COMPONENTS=%q loaded as %v", v, c.CardComponents)
		}
	}
	if err := loadErr(t, map[string]string{"CARD_COMPONENTS": "yes"}); err == nil || !strings.Contains(err.Error(), "CARD_COMPONENTS") {
		t.Errorf("CARD_COMPONENTS=yes: %v", err)
	}
}

// --- timed backups (§20 erratum 321) -----------------------------------------

func TestBackupOffByDefault(t *testing.T) {
	c := mustLoad(t, nil)
	if c.BackupOn() || c.BackupEvery != 0 || c.BackupDir != "" || c.BackupKeep != 0 {
		t.Fatalf("timed backups must be off by default: %+v %q %d", c.BackupEvery, c.BackupDir, c.BackupKeep)
	}
	// BACKUP_KEEP alone is accepted and unused: commenting out the pair must
	// not turn the third line into a boot error.
	for _, v := range []string{"7", "0", "nonsense"} {
		c = mustLoad(t, map[string]string{"BACKUP_KEEP": v})
		if c.BackupOn() || c.BackupKeep != 0 {
			t.Fatalf("BACKUP_KEEP=%q while off: %v %d", v, c.BackupOn(), c.BackupKeep)
		}
	}
}

func TestBackupOn(t *testing.T) {
	c := mustLoad(t, map[string]string{"BACKUP_EVERY": "24h", "BACKUP_DIR": "/data/backups"})
	if !c.BackupOn() || c.BackupEvery != 24*time.Hour || c.BackupDir != "/data/backups" || c.BackupKeep != 30 {
		t.Fatalf("on: %v %v %q %d", c.BackupOn(), c.BackupEvery, c.BackupDir, c.BackupKeep)
	}
	c = mustLoad(t, map[string]string{"BACKUP_EVERY": "10s", "BACKUP_DIR": "/data/", "BACKUP_KEEP": "1"})
	if c.BackupEvery != 10*time.Second || c.BackupDir != "/data" || c.BackupKeep != 1 {
		t.Fatalf("the floor, DATA_DIR itself and keep=1: %v %q %d", c.BackupEvery, c.BackupDir, c.BackupKeep)
	}
	// DATA_DIR inside BACKUP_DIR is allowed too: rotation touches minted names only.
	if c := mustLoad(t, map[string]string{"BACKUP_EVERY": "1h", "BACKUP_DIR": "/srv", "DATA_DIR": "/srv/data"}); c.BackupDir != "/srv" {
		t.Fatalf("a BACKUP_DIR around DATA_DIR: %q", c.BackupDir)
	}
}

func TestBackupRefusals(t *testing.T) {
	on := func(m map[string]string) map[string]string {
		out := map[string]string{"BACKUP_EVERY": "1h", "BACKUP_DIR": "/backups"}
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"BACKUP_EVERY": "1h"}, "BACKUP_EVERY: is set and BACKUP_DIR is not: timed backups need both (DEPLOY.md §15.6)"},
		{map[string]string{"BACKUP_DIR": "/backups"}, "BACKUP_DIR: is set and BACKUP_EVERY is not: timed backups need both (DEPLOY.md §15.6)"},
		{on(map[string]string{"BACKUP_EVERY": "9s"}), "BACKUP_EVERY: must be at least 10s, got 9s"},
		{on(map[string]string{"BACKUP_EVERY": "daily"}), `BACKUP_EVERY: invalid duration "daily"`},
		{on(map[string]string{"BACKUP_EVERY": "-1h"}), "BACKUP_EVERY: must be positive"},
		{on(map[string]string{"BACKUP_KEEP": "0"}), "BACKUP_KEEP: must be >= 1, got 0"},
		{on(map[string]string{"BACKUP_KEEP": "x"}), `BACKUP_KEEP: invalid integer "x"`},
		{on(map[string]string{"BACKUP_DIR": "backups"}), `BACKUP_DIR: must be an absolute path, got "backups"`},
		{on(map[string]string{"BACKUP_DIR": "/data/../backups"}), "BACKUP_DIR: must be a clean absolute path with no `.` or `..` segment"},
		{on(map[string]string{"BACKUP_DIR": "/"}), "BACKUP_DIR: must not be the root of the filesystem"},
		{on(map[string]string{"BACKUP_DIR": "/scratch"}), "BACKUP_DIR: is /scratch, which is also SCRATCH_DIR=/scratch: the snapshots need a directory of their own (DEPLOY.md §15.6)"},
		{on(map[string]string{"BACKUP_DIR": "/scratch/b"}), "BACKUP_DIR: is /scratch/b, which sits INSIDE SCRATCH_DIR=/scratch: put it beside that directory, not under it (DEPLOY.md §15.6)"},
		{on(map[string]string{"BACKUP_DIR": "/srv", "SCRATCH_DIR": "/srv/scratch"}), "BACKUP_DIR: is /srv, which CONTAINS SCRATCH_DIR=/srv/scratch: put it beside that directory, not around it (DEPLOY.md §15.6)"},
		{on(map[string]string{"BACKUP_DIR": "/media", "STORAGE_DIR": "/media"}), "which is also STORAGE_DIR=/media"},
		{on(map[string]string{"BACKUP_DIR": "/media/b", "STORAGE_DIR": "/media"}), "which sits INSIDE STORAGE_DIR=/media"},
		{on(map[string]string{"BACKUP_DIR": "/srv", "STORAGE_DIR": "/srv/media"}), "which CONTAINS STORAGE_DIR=/srv/media"},
		// The path rules run in the half-pair case too: both errors at once.
		{map[string]string{"BACKUP_DIR": "relative"}, "BACKUP_DIR: must be an absolute path"},
	} {
		err := loadErr(t, tc.env)
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: error %q does not contain %q", tc.env, err, tc.want)
		}
	}
	err := loadErr(t, map[string]string{"BACKUP_DIR": "relative"})
	if !strings.Contains(err.Error(), "is set and BACKUP_EVERY is not") {
		t.Errorf("a malformed half-pair must report both: %q", err)
	}
}

// TestStorageDirShapeSentencesUnchanged: absDir is shared with BACKUP_DIR, and
// STORAGE_DIR's sentences are grepped by tests and operators.
func TestStorageDirShapeSentencesUnchanged(t *testing.T) {
	for v, want := range map[string]string{
		"media":    `STORAGE_DIR: must be an absolute path, got "media"`,
		"/a/../b":  "STORAGE_DIR: must be a clean absolute path with no `.` or `..` segment, got \"/a/../b\" (did you mean \"/b\"?)",
		"/":        "STORAGE_DIR: must not be the root of the filesystem",
		"/scratch": "STORAGE_DIR: is /scratch, which is also SCRATCH_DIR=/scratch: the media store and that directory must be two places (DEPLOY.md §32)",
	} {
		_, err := Load(fsEnv(map[string]string{"STORAGE_DIR": v}))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("STORAGE_DIR=%q: %v (want %q)", v, err, want)
		}
	}
}

// TestStorageDirDriveLetter (run 15, R15B-1): the desktop app runs the server
// on Windows, so a drive-letter path loads (slashed and cleaned) for
// STORAGE_DIR and BACKUP_DIR alike; `C:` alone is drive-relative and `C:/` is
// a root; the overlap rules hold across slash styles and case.
func TestStorageDirDriveLetter(t *testing.T) {
	for v, want := range map[string]string{
		`C:/x/y`:    "C:/x/y",
		`C:\x\y`:    "C:/x/y",
		`c:\x\y\`:   "c:/x/y",
		`D:/Clips/`: "D:/Clips",
	} {
		c, err := Load(fsEnv(map[string]string{"STORAGE_DIR": v}))
		if err != nil {
			t.Errorf("STORAGE_DIR=%q: %v", v, err)
			continue
		}
		if c.StorageDir != want {
			t.Errorf("STORAGE_DIR=%q loaded as %q, want %q", v, c.StorageDir, want)
		}
	}
	for v, want := range map[string]string{
		`C:`:        `STORAGE_DIR: must be an absolute path, got "C:"`,
		`C:x`:       `STORAGE_DIR: must be an absolute path, got "C:x"`,
		`1:/x`:      `STORAGE_DIR: must be an absolute path, got "1:/x"`,
		`C:/`:       "STORAGE_DIR: must not be the root of the filesystem",
		`C:\`:       "STORAGE_DIR: must not be the root of the filesystem",
		`C:\a\..\b`: "STORAGE_DIR: must be a clean absolute path with no `.` or `..` segment, got \"C:\\\\a\\\\..\\\\b\" (did you mean \"C:/b\"?)",
	} {
		_, err := Load(fsEnv(map[string]string{"STORAGE_DIR": v}))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("STORAGE_DIR=%q: %v (want %q)", v, err, want)
		}
	}
	// The overlap rules across the two spellings and case.
	_, err := Load(fsEnv(map[string]string{"SCRATCH_DIR": `C:\Clippet\scratch`, "STORAGE_DIR": "c:/clippet/scratch/media"}))
	if err == nil || !strings.Contains(err.Error(), "sits INSIDE SCRATCH_DIR=C:/Clippet/scratch") {
		t.Errorf("a drive-letter STORAGE_DIR inside SCRATCH_DIR loaded: %v", err)
	}
	_, err = Load(fsEnv(map[string]string{"DATA_DIR": `C:\Clippet\data`, "STORAGE_DIR": "C:/clippet/DATA"}))
	if err == nil || !strings.Contains(err.Error(), "which is also DATA_DIR=C:/Clippet/data") {
		t.Errorf("a drive-letter STORAGE_DIR equal to DATA_DIR loaded: %v", err)
	}
	c, err := Load(fsEnv(map[string]string{"DATA_DIR": `C:\Clippet\data`, "SCRATCH_DIR": `C:\Clippet\scratch`, "STORAGE_DIR": `C:\Clippet\media`}))
	if err != nil || c.StorageDir != "C:/Clippet/media" {
		t.Errorf("three drive-letter siblings: %v, %q", err, c.StorageDir)
	}
	c, err = Load(env(map[string]string{"BACKUP_DIR": `C:\Backups\clippet`, "BACKUP_EVERY": "1h"}))
	if err != nil || c.BackupDir != "C:/Backups/clippet" {
		t.Errorf("a drive-letter BACKUP_DIR: %v, %q", err, c.BackupDir)
	}
}

// TestStorageDirUNC (run 15d, lane uncpath): the desktop app takes a NAS share
// in the same field as a folder, so a UNC path loads in both spellings, held
// slashed with its TWO leading slashes kept (path.Clean alone would fold them
// into a POSIX path); a bare server, a device path and a `..` that climbs out
// of the share are not absolute; the overlap rules hold across spellings and
// case.
func TestStorageDirUNC(t *testing.T) {
	for v, want := range map[string]string{
		`\\nas\clips`:               "//nas/clips",
		`\\nas\clips\`:              "//nas/clips",
		`\\nas\clips\sub\dir`:       "//nas/clips/sub/dir",
		`//nas/clips/sub/dir`:       "//nas/clips/sub/dir",
		`//nas/clips/sub/dir/`:      "//nas/clips/sub/dir",
		`\\nas/clips\sub`:           "//nas/clips/sub",
		`\\localhost\C$\Clips\x\cl`: "//localhost/C$/Clips/x/cl",
	} {
		c, err := Load(fsEnv(map[string]string{"STORAGE_DIR": v}))
		if err != nil {
			t.Errorf("STORAGE_DIR=%q: %v", v, err)
			continue
		}
		if c.StorageDir != want {
			t.Errorf("STORAGE_DIR=%q loaded as %q, want %q", v, c.StorageDir, want)
		}
	}
	for v, want := range map[string]string{
		`\\nas`:              `STORAGE_DIR: must be an absolute path, got "\\\\nas"`,
		`\\nas\`:             `STORAGE_DIR: must be an absolute path, got "\\\\nas\\"`,
		`//nas`:              `STORAGE_DIR: must be an absolute path, got "//nas"`,
		`//nas/`:             `STORAGE_DIR: must be an absolute path, got "//nas/"`,
		`\\.\C:\x`:           `STORAGE_DIR: must be an absolute path, got "\\\\.\\C:\\x"`,
		`\\?\C:\x`:           `STORAGE_DIR: must be an absolute path, got "\\\\?\\C:\\x"`,
		`//nas/clips/..`:     `STORAGE_DIR: must be an absolute path, got "//nas/clips/.."`,
		`//nas/../x`:         `STORAGE_DIR: must be an absolute path, got "//nas/../x"`,
		`\\nas\clips\a\..\b`: "STORAGE_DIR: must be a clean absolute path with no `.` or `..` segment, got \"\\\\\\\\nas\\\\clips\\\\a\\\\..\\\\b\" (did you mean \"//nas/clips/b\"?)",
	} {
		_, err := Load(fsEnv(map[string]string{"STORAGE_DIR": v}))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("STORAGE_DIR=%q: %v (want %q)", v, err, want)
		}
	}
	// dirForm keeps the leading pair where path.Clean folds it.
	if got := dirForm(`\\nas\clips\.\x`); got != "//nas/clips/x" {
		t.Errorf("dirForm of a UNC path = %q, want //nas/clips/x", got)
	}
	if got := path.Clean("//nas/clips"); got != "/nas/clips" {
		t.Errorf("path.Clean changed: %q (the reason dirForm special-cases UNC)", got)
	}
	// The overlap rules with a UNC DATA_DIR and SCRATCH_DIR, across spellings and case.
	_, err := Load(fsEnv(map[string]string{"DATA_DIR": `\\NAS\Clippet\data`, "STORAGE_DIR": "//nas/clippet/data"}))
	if err == nil || !strings.Contains(err.Error(), "which is also DATA_DIR=//NAS/Clippet/data") {
		t.Errorf("a UNC STORAGE_DIR equal to a UNC DATA_DIR loaded: %v", err)
	}
	_, err = Load(fsEnv(map[string]string{"DATA_DIR": `\\nas\clippet\data`, "STORAGE_DIR": `\\nas\clippet\data\media`}))
	if err == nil || !strings.Contains(err.Error(), "sits INSIDE DATA_DIR=//nas/clippet/data") {
		t.Errorf("a UNC STORAGE_DIR inside a UNC DATA_DIR loaded: %v", err)
	}
	_, err = Load(fsEnv(map[string]string{"DATA_DIR": `\\nas\clippet\data`, "STORAGE_DIR": `\\nas\clippet`}))
	if err == nil || !strings.Contains(err.Error(), "CONTAINS DATA_DIR=//nas/clippet/data") {
		t.Errorf("a UNC STORAGE_DIR around a UNC DATA_DIR loaded: %v", err)
	}
	_, err = Load(fsEnv(map[string]string{"SCRATCH_DIR": `\\nas\clippet\scratch`, "STORAGE_DIR": "//NAS/clippet/scratch/media"}))
	if err == nil || !strings.Contains(err.Error(), "sits INSIDE SCRATCH_DIR=//nas/clippet/scratch") {
		t.Errorf("a UNC STORAGE_DIR inside a UNC SCRATCH_DIR loaded: %v", err)
	}
	c, err := Load(fsEnv(map[string]string{"DATA_DIR": `C:\Clippet\data`, "SCRATCH_DIR": `C:\Clippet\scratch`, "STORAGE_DIR": `\\nas\clippet\data`}))
	if err != nil || c.StorageDir != "//nas/clippet/data" {
		t.Errorf("a UNC STORAGE_DIR beside drive-letter siblings: %v, %q", err, c.StorageDir)
	}
	c, err = Load(fsEnv(map[string]string{"DATA_DIR": `\\nas\clippet\data`, "STORAGE_DIR": `\\nas\clippet\datax`}))
	if err != nil || c.StorageDir != "//nas/clippet/datax" {
		t.Errorf("a UNC sibling that shares a prefix: %v, %q", err, c.StorageDir)
	}
	c, err = Load(env(map[string]string{"BACKUP_DIR": `\\nas\backups\clippet`, "BACKUP_EVERY": "1h"}))
	if err != nil || c.BackupDir != "//nas/backups/clippet" {
		t.Errorf("a UNC BACKUP_DIR: %v, %q", err, c.BackupDir)
	}
	_, err = Load(env(map[string]string{"BACKUP_DIR": `\\nas`, "BACKUP_EVERY": "1h"}))
	if err == nil || !strings.Contains(err.Error(), `BACKUP_DIR: must be an absolute path`) {
		t.Errorf("a bare-server BACKUP_DIR loaded: %v", err)
	}
}

// DISCORD_ENRICH_UPLOAD (§20 erratum 323): OFF by default, `on`/`off` in any
// case, anything else a boot error naming the variable, inert with no bot.
func TestEnrichUpload(t *testing.T) {
	if mustLoad(t, nil).EnrichUpload {
		t.Error("DISCORD_ENRICH_UPLOAD is on by default")
	}
	for v, want := range map[string]bool{"on": true, "ON": true, "On": true, "off": false, "OFF": false, "": false} {
		if c := mustLoad(t, map[string]string{"DISCORD_ENRICH_UPLOAD": v}); c.EnrichUpload != want {
			t.Errorf("DISCORD_ENRICH_UPLOAD=%q loaded as %v", v, c.EnrichUpload)
		}
	}
	for _, bad := range []string{"yes", "true", "1", "video"} {
		if err := loadErr(t, map[string]string{"DISCORD_ENRICH_UPLOAD": bad}); err == nil || !strings.Contains(err.Error(), "DISCORD_ENRICH_UPLOAD") {
			t.Errorf("DISCORD_ENRICH_UPLOAD=%s: %v", bad, err)
		}
	}
}

// SETUP_WIZARD (§20 erratum 335 (d)): ON by default, `on`/`off` in any case,
// anything else a boot error naming the variable.
func TestSetupWizard(t *testing.T) {
	if mustLoad(t, nil).SetupWizardOff {
		t.Error("SETUP_WIZARD is off by default")
	}
	for v, off := range map[string]bool{"on": false, "ON": false, "off": true, "Off": true, "": false} {
		if c := mustLoad(t, map[string]string{"SETUP_WIZARD": v}); c.SetupWizardOff != off {
			t.Errorf("SETUP_WIZARD=%q loaded as off=%v", v, c.SetupWizardOff)
		}
	}
	for _, bad := range []string{"yes", "true", "1", "managed"} {
		if err := loadErr(t, map[string]string{"SETUP_WIZARD": bad}); err == nil || !strings.Contains(err.Error(), "SETUP_WIZARD") {
			t.Errorf("SETUP_WIZARD=%s: %v", bad, err)
		}
	}
}

// DISCORD_UPLOAD_FORM (§20 erratum 320): ON by default (run 6 decision 3a),
// `on`/`off` in any case, anything else a boot error naming the variable — and
// inert, never an orphan, with no bot configured (mustLoad sets no bot).
func TestUploadForm(t *testing.T) {
	if !mustLoad(t, nil).UploadForm {
		t.Error("DISCORD_UPLOAD_FORM is off by default")
	}
	for v, want := range map[string]bool{"on": true, "ON": true, "off": false, "Off": false, "": true} {
		if c := mustLoad(t, map[string]string{"DISCORD_UPLOAD_FORM": v}); c.UploadForm != want {
			t.Errorf("DISCORD_UPLOAD_FORM=%q loaded as %v", v, c.UploadForm)
		}
	}
	for _, bad := range []string{"yes", "true", "1", "modal"} {
		if err := loadErr(t, map[string]string{"DISCORD_UPLOAD_FORM": bad}); err == nil || !strings.Contains(err.Error(), "DISCORD_UPLOAD_FORM") {
			t.Errorf("DISCORD_UPLOAD_FORM=%s: %v", bad, err)
		}
	}
}

// --- DB_SNAPSHOT / DB_SNAPSHOT_EVERY (§20 erratum 343) ---------------------------

// TestDBSnapshotDefaults: on by default on both backends with a ten-minute
// debounce; `off` turns it off on either (§20 errata 343, 348 — a folder home
// keeps its copy under the folder's own `_clippet/`).
func TestDBSnapshotDefaults(t *testing.T) {
	c := mustLoad(t, nil)
	if !c.DBSnapshot || c.DBSnapshotEvery != 10*time.Minute {
		t.Fatalf("s3 default: snapshot=%v every=%v, want on and 10m", c.DBSnapshot, c.DBSnapshotEvery)
	}
	if c := mustLoad(t, map[string]string{"DB_SNAPSHOT": "off"}); c.DBSnapshot {
		t.Fatal("DB_SNAPSHOT=off left it on")
	}
	if c := mustLoad(t, map[string]string{"DB_SNAPSHOT": "ON", "DB_SNAPSHOT_EVERY": "5s"}); !c.DBSnapshot || c.DBSnapshotEvery != 5*time.Second {
		t.Fatalf("on at the floor: %v %v", c.DBSnapshot, c.DBSnapshotEvery)
	}
	for v, want := range map[string]bool{"": true, "on": true, "off": false} {
		c, err := Load(fsEnv(map[string]string{"DB_SNAPSHOT": v}))
		if err != nil {
			t.Fatalf("fs with DB_SNAPSHOT=%q: %v", v, err)
		}
		if c.DBSnapshot != want {
			t.Fatalf("fs with DB_SNAPSHOT=%q: snapshot=%v, want %v (a folder home keeps its copy in the folder)", v, c.DBSnapshot, want)
		}
	}
}

// TestDBSnapshotRefusals: a word that is not on|off, and an interval under the floor.
func TestDBSnapshotRefusals(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"DB_SNAPSHOT": "yes"}, `DB_SNAPSHOT: must be one of on|off, got "yes"`},
		{map[string]string{"DB_SNAPSHOT_EVERY": "4s"}, "DB_SNAPSHOT_EVERY: must be at least 5s, got 4s"},
		{map[string]string{"DB_SNAPSHOT_EVERY": "soon"}, "DB_SNAPSHOT_EVERY: invalid duration"},
	} {
		if err := loadErr(t, tc.env); !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.env, err, tc.want)
		}
	}
}
