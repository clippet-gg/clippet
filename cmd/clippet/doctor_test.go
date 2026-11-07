package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/store"
)

// doctorFixture drives `admin doctor` with a configuration built from a map,
// a Fake bucket and real temp directories — the same injection the rest of the
// admin tests use.
type doctorFixture struct {
	env    map[string]string
	fake   *store.Fake
	out    bytes.Buffer
	errOut bytes.Buffer
	// openErr, when set, is what the injected store constructor returns.
	openErr error
}

// stubFFmpeg writes two executable scripts that answer `-version` the way the
// real binaries do, so the check is exercised without depending on what is
// installed in whatever image runs the tests.
func stubFFmpeg(t *testing.T, dir string) (ffmpeg, ffprobe string) {
	t.Helper()
	write := func(name, banner string) string {
		p := filepath.Join(dir, name)
		body := "#!/bin/sh\necho \"" + banner + "\"\n"
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return write("ffmpeg-stub", "ffmpeg version 7.1.1 Copyright (c) 2000-2025"),
		write("ffprobe-stub", "ffprobe version 7.1.1 Copyright (c) 2000-2025")
}

func newDoctorFixture(t *testing.T) *doctorFixture {
	t.Helper()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	scratch := filepath.Join(root, "scratch")
	for _, d := range []string{data, scratch} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ffmpeg, ffprobe := stubFFmpeg(t, root)
	return &doctorFixture{
		fake: store.NewFake(),
		env: map[string]string{
			"PUBLIC_BASE_URL": "https://clips.example",
			"S3_ENDPOINT":     "https://acct.r2.cloudflarestorage.com",
			"S3_BUCKET":       "clips",
			"S3_ACCESS_KEY":   "real-access-key",
			"S3_SECRET_KEY":   "sup3r-secret-value",
			"DATA_DIR":        data,
			"SCRATCH_DIR":     scratch,
			"FFMPEG_PATH":     ffmpeg,
			"FFPROBE_PATH":    ffprobe,
		},
	}
}

func (fx *doctorFixture) run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	fx.out.Reset()
	fx.errOut.Reset()
	env := adminEnv{
		stdout: &fx.out, stderr: &fx.errOut,
		openStore: func(config.Config) (store.Store, error) {
			if fx.openErr != nil {
				return nil, fx.openErr
			}
			return fx.fake, nil
		},
		now: func() time.Time { return time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC) },
	}
	getenv := func(k string) string { return fx.env[k] }
	env.getenv = getenv
	code := runDoctor(args, getenv, env)
	return code, fx.out.String() + fx.errOut.String()
}

// verdictOf returns the WORST verdict a check printed. A check may print more
// than one line (paths prints one per directory), and the summary counts the
// worst of them — so a test asserting "paths is ok" must not be satisfied by
// the first of two lines.
func verdictOf(out, check string) string {
	rank := map[string]int{"": 0, vOK: 1, vWarn: 2, vSkip: 3, vFail: 4}
	worst := ""
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == check && rank[f[1]] > rank[worst] {
			worst = f[1]
		}
	}
	return worst
}

// TestDoctorUsageCountsTheChecksItRuns: `clippet admin help`'s doctor row says
// how many checks the doctor runs, and the number is the one the doctor's own
// summary line prints — the row used to enumerate seven while it ran nineteen
// (a nit sweep's `[docs, cli]` row).
func TestDoctorUsageCountsTheChecksItRuns(t *testing.T) {
	fx := newDoctorFixture(t)
	cfg, err := config.Load(func(k string) string { return fx.env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if code := runMigrate(cfg); code != 0 {
		t.Fatal("migrate")
	}
	_, out := fx.run(t)
	ran := regexp.MustCompile(`(?m)^doctor:  (\d+) checks,`).FindStringSubmatch(out)
	if ran == nil {
		t.Fatalf("no summary line:\n%s", out)
	}
	var help bytes.Buffer
	if code := runAdminWith([]string{"help"}, cfg, adminEnv{stdout: &help, stderr: &help}); code != 0 {
		t.Fatalf("admin help = %d", code)
	}
	want := "  doctor                                 preflight: " + ran[1] +
		" checks across configuration, storage, ffmpeg and the integrations (needs no database)\n"
	if !strings.Contains(help.String(), want) {
		t.Fatalf("the usage's doctor row does not say the %s checks the doctor ran:\n%s", ran[1], help.String())
	}
}

func TestDoctorGreen(t *testing.T) {
	fx := newDoctorFixture(t)
	// A stack that has booted once: the database exists and is migrated, which
	// is what an operator's `docker compose exec app clippet admin doctor`
	// looks at.
	cfg, err := config.Load(func(k string) string { return fx.env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if code := runMigrate(cfg); code != 0 {
		t.Fatal("migrate")
	}
	code, out := fx.run(t)
	if code != 0 {
		t.Fatalf("doctor: %d\n%s", code, out)
	}
	// Every check ran. A check that silently stopped running would otherwise
	// read as a clean bill of health.
	for _, check := range doctorChecks {
		if v := verdictOf(out, check); v == "" {
			t.Fatalf("check %q did not appear:\n%s", check, out)
		} else if v != vOK {
			t.Fatalf("check %q = %s on a healthy configuration:\n%s", check, v, out)
		}
	}
	if !strings.Contains(out, "doctor:  19 checks, 19 ok, 0 warn, 0 fail, 0 skipped") {
		t.Fatalf("summary line:\n%s", out)
	}
	// The bucket check is a WRITE probe, and it cleans up after itself.
	if !strings.Contains(out, "probe object put and removed") {
		t.Fatalf("the bucket check did not probe writability:\n%s", out)
	}
	for _, key := range fx.fake.Keys() {
		if strings.Contains(key, "doctor/") {
			t.Fatalf("the probe object %q was left in the bucket", key)
		}
	}
	if !strings.Contains(out, "integrity=ok applied=1") {
		t.Fatalf("the db line does not report integrity and schema:\n%s", out)
	}
}

func TestDoctorNeverPrintsASecret(t *testing.T) {
	fx := newDoctorFixture(t)
	fx.env["DISCORD_WEBHOOK_URL"] = "https://discord.com/api/webhooks/123456789/tok3n-that-must-never-print"
	// A store failure is the one place a library message could quote the
	// credential it signed with; make it do exactly that.
	fx.fake.FailNext("Ping", errors.New(`store: ping: 403 SignatureDoesNotMatch (key sup3r-secret-value)`))
	code, out := fx.run(t)
	if code != 1 {
		t.Fatalf("a failing ping must exit 1: %d\n%s", code, out)
	}
	for _, secret := range []string{"sup3r-secret-value", "tok3n-that-must-never-print"} {
		if strings.Contains(out, secret) {
			t.Fatalf("doctor printed a secret (%q):\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "(redacted)") {
		t.Fatalf("the credential in the error was not redacted:\n%s", out)
	}
	// The webhook is still reported — by its non-secret half.
	if !strings.Contains(out, "discord.com/123456789/(token redacted)") {
		t.Fatalf("webhook line:\n%s", out)
	}
	if verdictOf(out, "bucket") != vFail {
		t.Fatalf("bucket verdict = %s, want FAIL:\n%s", verdictOf(out, "bucket"), out)
	}
}

// TestDoctorRedactsOnlyErrorText pins the shape of the redaction. Blanket-
// striking every configured secret out of every line corrupts honest output the
// moment a credential is a common word — the published development access
// key is literally `clippet`, and `/data/clippet.db` is a path the db check must
// print intact.
func TestDoctorRedactsOnlyErrorText(t *testing.T) {
	fx := newDoctorFixture(t)
	root := t.TempDir()
	fx.env["S3_ACCESS_KEY"] = shippedAccessKey // "clippet"
	fx.env["S3_SECRET_KEY"] = "db"             // absurdly short, and a substring of everything
	fx.env["DATA_DIR"] = filepath.Join(root, "clippet-data")
	fx.env["SCRATCH_DIR"] = filepath.Join(root, "clippet-scratch")
	fx.env["S3_ENDPOINT"] = "http://minio:9000"
	for _, d := range []string{fx.env["DATA_DIR"], fx.env["SCRATCH_DIR"]} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load(func(k string) string { return fx.env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if code := runMigrate(cfg); code != 0 {
		t.Fatal("migrate")
	}
	_, out := fx.run(t)
	if strings.Contains(out, "(redacted)") {
		t.Fatalf("a short secret and an access key were struck out of honest output:\n%s", out)
	}
	if !strings.Contains(out, dbPath(cfg)) {
		t.Fatalf("the db check must print its path intact:\n%s", out)
	}
	if !strings.Contains(out, fx.env["SCRATCH_DIR"]) {
		t.Fatalf("the paths check must print SCRATCH_DIR intact:\n%s", out)
	}
}

func TestDoctorReportsEveryConfigErrorAndSkipsTheRest(t *testing.T) {
	fx := newDoctorFixture(t)
	fx.env["PUBLIC_BASE_URL"] = "not a url at all"
	fx.env["TRANSCODE_CRF"] = "99"
	fx.env["SWEEP_INTERVAL"] = "banana"
	code, out := fx.run(t)
	if code != 1 {
		t.Fatalf("broken config: %d\n%s", code, out)
	}
	for _, name := range []string{"PUBLIC_BASE_URL", "TRANSCODE_CRF", "SWEEP_INTERVAL"} {
		if !strings.Contains(out, name) {
			t.Fatalf("config error for %s is missing (the loader reports ALL of them):\n%s", name, out)
		}
	}
	if !strings.Contains(out, "3 variable(s) invalid") {
		t.Fatalf("the count of bad variables:\n%s", out)
	}
	// Nothing downstream is guessed at, and every check still shows up.
	for _, check := range doctorChecks[1:] {
		if v := verdictOf(out, check); v != vSkip {
			t.Fatalf("check %q = %q, want skip when the configuration did not parse:\n%s", check, v, out)
		}
	}
	if fx.fake.CallsSnapshot() != nil && len(fx.fake.CallsSnapshot()) != 0 {
		t.Fatalf("doctor talked to the bucket with an unparsable configuration: %v", fx.fake.Calls)
	}
}

func TestDoctorScratchBelowTheFloorFails(t *testing.T) {
	fx := newDoctorFixture(t)
	// A minimum larger than any filesystem: the check must say what the
	// consequence is rather than reporting a number.
	fx.env["SCRATCH_MIN_FREE_BYTES"] = "9223372036854775807"
	code, out := fx.run(t)
	if code != 1 || verdictOf(out, "paths") != vFail {
		t.Fatalf("paths: %d %s\n%s", code, verdictOf(out, "paths"), out)
	}
	if !strings.Contains(out, "507 insufficient_scratch") {
		t.Fatalf("the scratch failure does not name the error it causes:\n%s", out)
	}
}

func TestDoctorMissingDirectoriesAreWarningsNotFailures(t *testing.T) {
	fx := newDoctorFixture(t)
	root := t.TempDir()
	fx.env["DATA_DIR"] = filepath.Join(root, "nope-data")
	fx.env["SCRATCH_DIR"] = filepath.Join(root, "nope-scratch")
	code, out := fx.run(t)
	if code != 0 {
		t.Fatalf("running before the first `up -d` must not fail: %d\n%s", code, out)
	}
	if verdictOf(out, "paths") != vWarn || verdictOf(out, "db") != vWarn {
		t.Fatalf("paths=%s db=%s, want warnings:\n%s", verdictOf(out, "paths"), verdictOf(out, "db"), out)
	}
	if !strings.Contains(out, "creates it at boot") {
		t.Fatalf("the warning does not say who creates the directory:\n%s", out)
	}
	// Nothing was created by looking.
	if _, err := os.Stat(fx.env["DATA_DIR"]); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("doctor created DATA_DIR")
	}
}

func TestDoctorDatabaseIntegrityAndSchema(t *testing.T) {
	fx := newDoctorFixture(t)
	cfg, err := config.Load(func(k string) string { return fx.env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if code := runMigrate(cfg); code != 0 {
		t.Fatal("migrate")
	}
	code, out := fx.run(t)
	if code != 0 || verdictOf(out, "db") != vOK {
		t.Fatalf("db check on a migrated file: %d %s\n%s", code, verdictOf(out, "db"), out)
	}
	if !strings.Contains(out, "integrity=ok applied=1") {
		t.Fatalf("db line:\n%s", out)
	}

	// A file that is not a database at all.
	if err := os.WriteFile(dbPath(cfg), []byte("this is not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out = fx.run(t)
	if code != 1 || verdictOf(out, "db") != vFail {
		t.Fatalf("db check on a corrupt file: %d %s\n%s", code, verdictOf(out, "db"), out)
	}
}

func TestDoctorTrustedProxiesAgainstThePublishBind(t *testing.T) {
	// Trusting a prefix while the port is reachable beyond loopback is the
	// DEPLOY §4 footgun: any caller can then spoof X-Real-IP past every
	// per-IP limit.
	fx := newDoctorFixture(t)
	fx.env["TRUSTED_PROXIES"] = "172.16.0.0/12"
	fx.env["CLIPPET_PUBLISH"] = "0.0.0.0:8481"
	code, out := fx.run(t)
	if code != 1 || verdictOf(out, "proxies") != vFail {
		t.Fatalf("widened bind + trusted prefix: %d %s\n%s", code, verdictOf(out, "proxies"), out)
	}
	if !strings.Contains(out, "spoof X-Real-IP") {
		t.Fatalf("the proxies failure does not say what goes wrong:\n%s", out)
	}

	// The same prefix on the shipped loopback bind is fine.
	fx.env["CLIPPET_PUBLISH"] = "127.0.0.1:8481"
	if code, out := fx.run(t); code != 0 || verdictOf(out, "proxies") != vOK {
		t.Fatalf("loopback bind: %d %s\n%s", code, verdictOf(out, "proxies"), out)
	}
	// A bare port publishes on every interface.
	fx.env["CLIPPET_PUBLISH"] = "8481"
	if code, _ := fx.run(t); code != 1 {
		t.Fatalf("a bare CLIPPET_PUBLISH port is a widened bind: %d", code)
	}
	// Not knowing is a warning, never silence.
	delete(fx.env, "CLIPPET_PUBLISH")
	if code, out := fx.run(t); code != 0 || verdictOf(out, "proxies") != vWarn {
		t.Fatalf("unknown bind: %d %s\n%s", code, verdictOf(out, "proxies"), out)
	}
	// No trusted prefix at all: nothing to warn about.
	delete(fx.env, "TRUSTED_PROXIES")
	if code, out := fx.run(t); code != 0 || verdictOf(out, "proxies") != vOK {
		t.Fatalf("no trusted prefixes: %d %s\n%s", code, verdictOf(out, "proxies"), out)
	}
}

func TestDoctorShippedCredentialsFootgun(t *testing.T) {
	fx := newDoctorFixture(t)
	// The published development pair pointed at R2 — the shape compose's old
	// fallback produced before §20 erratum 345, and still wrong when typed.
	fx.env["S3_ACCESS_KEY"] = shippedAccessKey
	fx.env["S3_SECRET_KEY"] = shippedSecretKey
	code, out := fx.run(t)
	if code != 1 || verdictOf(out, "creds") != vFail {
		t.Fatalf("shipped credentials against a remote endpoint: %d %s\n%s", code, verdictOf(out, "creds"), out)
	}
	if !strings.Contains(out, "PUBLISHED in this repository") {
		t.Fatalf("the creds failure does not explain itself:\n%s", out)
	}

	// The same pair against a server on this machine or network on a public
	// origin: a warning, whatever the server's compose service is called (the
	// harness's bucket server is no longer named `minio`, §20 erratum 345).
	for _, ep := range []string{"http://minio:9000", "http://rustfs:9000", "http://192.168.1.20:9000", "http://localhost:9000"} {
		fx.env["S3_ENDPOINT"] = ep
		if code, out := fx.run(t); code != 0 || verdictOf(out, "creds") != vWarn {
			t.Fatalf("shipped credentials against %s on a public origin: %d %s\n%s", ep, code, verdictOf(out, "creds"), out)
		}
	}
	// …and on the loopback dev stack: fine, and it says why.
	fx.env["PUBLIC_BASE_URL"] = "http://localhost:8481"
	if code, out := fx.run(t); code != 0 || verdictOf(out, "creds") != vOK {
		t.Fatalf("shipped credentials on loopback: %d %s\n%s", code, verdictOf(out, "creds"), out)
	}
}

func TestDoctorBaseURLSanity(t *testing.T) {
	fx := newDoctorFixture(t)
	// A public origin on plain http: warned about, never fatal — the operator
	// may be terminating TLS somewhere this process cannot see.
	fx.env["PUBLIC_BASE_URL"] = "http://clips.example"
	fx.env["S3_ENDPOINT"] = "http://minio:9000"
	fx.env["S3_ACCESS_KEY"] = "real"
	code, out := fx.run(t)
	if code != 0 || verdictOf(out, "baseurl") != vWarn {
		t.Fatalf("public http origin: %d %s\n%s", code, verdictOf(out, "baseurl"), out)
	}
	if !strings.Contains(out, "no player card") {
		t.Fatalf("the baseurl warning does not name the consequence:\n%s", out)
	}
	// Loopback http is the documented dev stack and stays quiet.
	fx.env["PUBLIC_BASE_URL"] = "http://localhost:8481"
	if code, out := fx.run(t); code != 0 || verdictOf(out, "baseurl") != vOK {
		t.Fatalf("loopback origin: %d %s\n%s", code, verdictOf(out, "baseurl"), out)
	}
}

func TestDoctorBucketFailures(t *testing.T) {
	fx := newDoctorFixture(t)
	// A client that cannot even be built.
	fx.openErr = errors.New("bad endpoint")
	if code, out := fx.run(t); code != 1 || verdictOf(out, "bucket") != vFail {
		t.Fatalf("unbuildable client: %d %s\n%s", code, verdictOf(out, "bucket"), out)
	}
	fx.openErr = nil

	// Reachable but read-only: exactly the case /readyz could never catch,
	// because BucketExists succeeds for a key that cannot write.
	fx.fake.FailNext("PutFile", errors.New("AccessDenied"))
	code, out := fx.run(t)
	if code != 1 || verdictOf(out, "bucket") != vFail {
		t.Fatalf("read-only bucket: %d %s\n%s", code, verdictOf(out, "bucket"), out)
	}
	if !strings.Contains(out, "reachable but NOT writable") {
		t.Fatalf("the bucket failure does not distinguish reachable from writable:\n%s", out)
	}
}

func TestDoctorMissingFFmpegFails(t *testing.T) {
	fx := newDoctorFixture(t)
	fx.env["FFMPEG_PATH"] = filepath.Join(t.TempDir(), "no-such-ffmpeg")
	code, out := fx.run(t)
	if code != 1 || verdictOf(out, "ffmpeg") != vFail {
		t.Fatalf("missing ffmpeg: %d %s\n%s", code, verdictOf(out, "ffmpeg"), out)
	}
}

func TestDoctorRejectsArguments(t *testing.T) {
	fx := newDoctorFixture(t)
	if code, out := fx.run(t, "everything"); code != 2 || !strings.Contains(out, "usage:") {
		t.Fatalf("stray argument: %d %q", code, out)
	}
}

// TestDoctorRunsBeforeConfigLoad pins the dispatch: `clippet admin doctor`
// with an environment that cannot be loaded must still produce a report, not
// main's two-line config death (§20 erratum 143).
func TestDoctorRunsBeforeConfigLoad(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"admin", "doctor"}, func(string) string { return "" }, &out, &errOut)
	all := out.String() + errOut.String()
	if code != 1 {
		t.Fatalf("doctor with an empty environment: %d\n%s", code, all)
	}
	if !strings.Contains(all, "config") || !strings.Contains(all, "PUBLIC_BASE_URL") {
		t.Fatalf("doctor did not report the configuration:\n%s", all)
	}
	if !strings.Contains(all, "doctor:  19 checks") {
		t.Fatalf("doctor did not finish its report:\n%s", all)
	}
	if strings.Contains(all, "clippet: config:") {
		t.Fatalf("main's config death ran instead of the preflight:\n%s", all)
	}
}

// TestDoctorPrices (§20 erratum 275): the row says `unset` when nothing is
// priced, prints the PARSED value when something is, and never prints an
// amount — a cost estimate is `admin stats`' business, not a preflight's.
func TestDoctorPrices(t *testing.T) {
	fx := newDoctorFixture(t)
	_, out := fx.run(t)
	if v := verdictOf(out, "prices"); v != vOK {
		t.Fatalf("prices = %q on an unpriced instance:\n%s", v, out)
	}
	for _, want := range []string{"STORAGE_PRICE_PER_GB_MONTH and EGRESS_PRICE_PER_GB are unset", "never 0.00"} {
		if !strings.Contains(out, want) {
			t.Errorf("the unpriced row is missing %q:\n%s", want, out)
		}
	}

	// Both prices, one of them written in a form that must come back normalised.
	fx.env["STORAGE_PRICE_PER_GB_MONTH"] = ".015"
	fx.env["EGRESS_PRICE_PER_GB"] = "0"
	_, out = fx.run(t)
	if !strings.Contains(out, "prices   ok   storage 0.015/GB-month, egress 0/GB") {
		t.Fatalf("the priced row:\n%s", out)
	}

	// One alone: the other half says `unset` rather than 0.
	delete(fx.env, "EGRESS_PRICE_PER_GB")
	fx.env["STORAGE_PRICE_PER_GB_MONTH"] = "00000015"
	_, out = fx.run(t)
	if !strings.Contains(out, "storage 15/GB-month, egress unset") {
		t.Fatalf("a half-priced instance:\n%s", out)
	}
	// No amount, ever: the row carries the configuration and nothing derived.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "prices") && strings.Contains(line, "≈") {
			t.Fatalf("the prices row printed a cost: %q", line)
		}
	}
}

// TestDoctorPricesNamesThePreset (§20 erratum 277): with STORAGE_PRICING set,
// the row says WHICH provider's list the figures came from, WHEN a person read
// it, the currency that list quotes, and the free-allowance rule the egress
// figure means nothing without. List prices drift, so the date is the operator's
// cue to go and check one.
func TestDoctorPricesNamesThePreset(t *testing.T) {
	fx := newDoctorFixture(t)
	fx.env["STORAGE_PRICING"] = "b2"
	_, out := fx.run(t)
	if v := verdictOf(out, "prices"); v != vOK {
		t.Fatalf("prices = %q with a preset:\n%s", v, out)
	}
	for _, want := range []string{
		"preset b2 (STORAGE_PRICING, read 2026-09-13, USD)",
		"storage 0.00695/GB-month after 10 GB a month",
		"egress 0.01/GB after 3x what is stored",
		"never fetched",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the preset row is missing %q:\n%s", want, out)
		}
	}

	// A typed price OVERRIDES its half and the row says so — an operator who
	// corrected a drifted list price must be able to see that it took. The
	// ALLOWANCE survives: it is a billing rule, not a number anybody retyped.
	fx.env["EGRESS_PRICE_PER_GB"] = "0.02"
	_, out = fx.run(t)
	for _, want := range []string{
		"storage 0.00695/GB-month after 10 GB a month",
		"egress 0.02/GB (yours, over the preset) after 3x what is stored",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the overridden row is missing %q:\n%s", want, out)
		}
	}

	// `local` is a row like any other, and the one with no currency: nobody
	// quotes a price for a disk you already own.
	fx2 := newDoctorFixture(t)
	fx2.env["STORAGE_PRICING"] = "local"
	_, out = fx2.run(t)
	if !strings.Contains(out, "preset local (STORAGE_PRICING, read 2026-09-13): storage 0/GB-month, egress 0/GB") {
		t.Fatalf("the local row:\n%s", out)
	}
	if strings.Contains(out, "USD") {
		t.Errorf("the local row names a currency:\n%s", out)
	}
	// Still no amount on any of them.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "prices") && strings.Contains(line, "≈") {
			t.Fatalf("the prices row printed a cost: %q", line)
		}
	}
}

// TestDoctorBackup (§20 erratum 321): off in every run, or the settings, the
// last run and the next as the server recorded them. Doctor writes nothing.
func TestDoctorBackup(t *testing.T) {
	fx := newDoctorFixture(t)
	_, out := fx.run(t)
	if !strings.Contains(out, "backup   ok   off: BACKUP_EVERY and BACKUP_DIR are unset (DEPLOY.md §15.6)") {
		t.Fatalf("the off line:\n%s", out)
	}

	dir := filepath.Join(filepath.Dir(fx.env["DATA_DIR"]), "backups")
	fx.env["BACKUP_EVERY"] = "24h"
	fx.env["BACKUP_DIR"] = dir
	_, out = fx.run(t)
	if !strings.Contains(out, "backup   ok   on: a database snapshot every 24h0m0s into "+dir+", newest 30 kept (no media)") ||
		!strings.Contains(out, "backup   FAIL "+dir+" is not a directory: every run fails until it is one (the app never creates it)") {
		t.Fatalf("a missing BACKUP_DIR:\n%s", out)
	}
	if strings.Contains(out, "no run recorded yet") {
		t.Fatalf("the FAIL line must end the row:\n%s", out)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("doctor created BACKUP_DIR: %v", err)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, out = fx.run(t)
	if v := verdictOf(out, "backup"); v != vOK || !strings.Contains(out, "backup   ok   no run recorded yet") {
		t.Fatalf("no status file (%s):\n%s", v, out)
	}
	// The write probe leaves nothing behind.
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("doctor left %s in BACKUP_DIR", left[0].Name())
	}
	// A directory nothing can be written to (even as root): a FAIL now, not at
	// the first run (run 8).
	if runtime.GOOS == "linux" {
		fx.env["BACKUP_DIR"] = "/proc"
		_, out = fx.run(t)
		if v := verdictOf(out, "backup"); v != vFail || !strings.Contains(out, "backup   FAIL /proc cannot be written to: every run fails until it can (") {
			t.Fatalf("an unwritable BACKUP_DIR (%s):\n%s", v, out)
		}
		fx.env["BACKUP_DIR"] = dir
	}

	write := func(st backupStatus) {
		t.Helper()
		if err := writeBackupStatus(fx.env["DATA_DIR"], st); err != nil {
			t.Fatal(err)
		}
	}
	// The fixture's clock is 2026-09-07T12:00:00Z.
	write(backupStatus{Dir: dir, Next: "2026-09-08T00:00:00Z", Last: &backupLast{At: "2026-09-07T00:00:00Z", Result: "ok",
		File: filepath.Join(dir, "clippet-20260907T000000Z.db"), Bytes: 2048}})
	_, out = fx.run(t)
	if v := verdictOf(out, "backup"); v != vOK ||
		!strings.Contains(out, "backup   ok   last: 2026-09-07T00:00:00Z ok, "+filepath.Join(dir, "clippet-20260907T000000Z.db")+" (2.0 KiB)") ||
		!strings.Contains(out, "backup   ok   next: 2026-09-08T00:00:00Z") {
		t.Fatalf("an ok last run (%s):\n%s", v, out)
	}

	write(backupStatus{Dir: dir, Next: "2026-09-07T06:00:00Z", Last: &backupLast{At: "2026-09-06T06:00:00Z", Result: "failed",
		Reason: "admin backup: failed with sup3r-secret-value in it"}})
	_, out = fx.run(t)
	if v := verdictOf(out, "backup"); v != vWarn ||
		!strings.Contains(out, "backup   warn last: 2026-09-06T06:00:00Z failed: admin backup: failed with (redacted) in it") ||
		!strings.Contains(out, "backup   ok   next: was due 2026-09-07T06:00:00Z") {
		t.Fatalf("a failed last run (%s):\n%s", v, out)
	}

	// A record of another directory is not this one's.
	write(backupStatus{Dir: "/elsewhere", Next: "2026-09-08T00:00:00Z", Last: &backupLast{At: "2026-09-07T00:00:00Z", Result: "failed", Reason: "x"}})
	_, out = fx.run(t)
	if v := verdictOf(out, "backup"); v != vOK || !strings.Contains(out, "no run recorded yet") || strings.Contains(out, "next:") {
		t.Fatalf("another directory's record (%s):\n%s", v, out)
	}
	// A malformed file is absent.
	if err := os.WriteFile(filepath.Join(fx.env["DATA_DIR"], backupStatusName), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out = fx.run(t)
	if !strings.Contains(out, "no run recorded yet") {
		t.Fatalf("a malformed status file:\n%s", out)
	}
}
