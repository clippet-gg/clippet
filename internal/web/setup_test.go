package web

// setup_test.go — the pins under /setup (ROADMAP #45, §20 errata 297-299, 309-311).
//
// Four of them are the walls the design rests on, and each fails on the code
// that existed before this lane: no URL anywhere in the table or the rendered
// page, every block TOTAL and accepted by config.Load, the block bytes
// themselves as a golden, and — since §20 erratum 348 (b) withdrew the
// Cloudflare refusal — the folder walking behind a tunnel with its risk named.

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"clippet/internal/auth"
	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/urls"
)

var setupURLRE = regexp.MustCompile(`https?://`)

// TestSetupTableCarriesNoURL is check.sh's stage-1 tripwire as a Go test, so a
// walk that grows a link fails `go test ./internal/web` on the machine that
// wrote it instead of at the merge gate. It reads every string the table can
// put on the page — the block, the labels, the caveats and both halves of
// every step — and it is the reason §20 erratum 298's two placeholder
// grammars exist at all.
func TestSetupTableCarriesNoURL(t *testing.T) {
	for _, o := range setupOptions() {
		check := func(what, s string) {
			t.Helper()
			if setupURLRE.MatchString(s) {
				t.Errorf("%s: %s carries a URL: %q", o.ID, what, s)
			}
		}
		check("Label", o.Label)
		check("Hint", o.Hint)
		check("Head", o.Head)
		check("Caveat", o.Caveat)
		check("Block", o.Block)
		check("BlockCF", o.BlockCF)
		check("Note", o.Note)
		check("NoteCF", o.NoteCF)
		check("Services", o.Services)
		for i, s := range o.Steps {
			check("step text", s.Text)
			check("step hint", s.Hint)
			if strings.TrimSpace(s.Text) == "" {
				t.Errorf("%s: step %d has no instruction", o.ID, i)
			}
		}
	}
	for _, s := range []string{blockHint, localTunnelNote, passRelease, passNote, stopFor(currentFS, setupOptions()[1], 5).Text(),
		stopFor(currentBucket, setupOptions()[1], 5).Text()} {
		if setupURLRE.MatchString(s) {
			t.Errorf("a page sentence carries a URL: %q", s)
		}
	}
}

// TestSetupStopText pins the forms of the STOP callout's text (§20 errata
// 309-311): the bold lead is the fact — the count, and that it stays put —
// the verb agreeing with the count, then what does not happen and what comes
// first; the conditional form between two buckets, which ends by saying the
// page cannot tell — and the section and the verb an operator has to go and
// read, with no runnable command.
func TestSetupStopText(t *testing.T) {
	fs, _ := setupOptionByID(config.BackendFS)
	r2, _ := setupOptionByID("r2")
	cases := []struct {
		current string
		target  setupOption
		clips   int
		lead    string
		want    string
	}{
		{currentBucket, fs, 5, "5 clips stay where they are.", "5 clips stay where they are. Switching does not move them. Move them first: admin storage migrate (DEPLOY.md §32.3)."},
		{currentBucket, fs, 1, "1 clip stays where it is.", "1 clip stays where it is. Switching does not move it. Move it first: admin storage migrate (DEPLOY.md §32.3)."},
		{currentBucket, r2, 5, "5 clips stay where they are.", "5 clips stay where they are. Switching does not move them. If the new bucket is a different one, move them first: admin storage migrate (DEPLOY.md §32.3). This page cannot tell whether it is."},
		{currentBucket, r2, 1, "1 clip stays where it is.", "1 clip stays where it is. Switching does not move it. If the new bucket is a different one, move it first: admin storage migrate (DEPLOY.md §32.3). This page cannot tell whether it is."},
		{currentBucket, fs, 3, "3 clips stay where they are.", "3 clips stay where they are. Switching does not move them. Move them first: admin storage migrate (DEPLOY.md §32.3)."},
	}
	for _, c := range cases {
		st := stopFor(c.current, c.target, c.clips)
		if st == nil {
			t.Errorf("stopFor(%s, %s, %d) = nil", c.current, c.target.ID, c.clips)
			continue
		}
		if st.Lead != c.lead {
			t.Errorf("stopFor(%s, %s, %d).Lead = %q, want %q", c.current, c.target.ID, c.clips, st.Lead, c.lead)
		}
		if got := st.Text(); got != c.want {
			t.Errorf("stopFor(%s, %s, %d) = %q, want %q", c.current, c.target.ID, c.clips, got, c.want)
		}
		// The three parts are the template's contract: the verb is set in
		// <code> between Before and After, so neither may carry it.
		if strings.Contains(st.Before, stopVerb) || strings.Contains(st.After, stopVerb) || strings.Contains(st.Lead, stopVerb) {
			t.Errorf("stopFor(%s, %s, %d): a part carries the verb: %+v", c.current, c.target.ID, c.clips, *st)
		}
	}
	if st := stopFor(currentBucket, fs, 0); st != nil {
		t.Errorf("no clips printed a STOP: %q", st.Text())
	}
	if st := stopFor(currentFS, fs, 5); st != nil {
		t.Errorf("the diagonal printed a STOP: %q", st.Text())
	}
	if got := (*setupStop)(nil).Text(); got != "" {
		t.Errorf("a nil STOP has text: %q", got)
	}
}

// TestSetupBlocksAreTotal is the total-block rule as a test (§20 erratum 298):
// every block ASSIGNS every variable that decides the outcome, exactly once,
// and never instructs a deletion.
//
// `COMPOSE_FILE` is the one that matters and the one that cannot be checked
// any other way: it is read by compose on the HOST, never passed into the
// container, and therefore invisible to this app — so a block that told
// somebody to REMOVE a line could not be verified by the page, by `Check now`
// or by `admin doctor`, and an `fs` instance moving to B2 would keep booting
// `fs` off compose.fs.yaml's hard-set environment with no boot error at all.
func TestSetupBlocksAreTotal(t *testing.T) {
	for _, c := range setupBlockCases() {
		vals := parseEnvBlock(t, c.Block)
		// COMPOSE_PATH_SEPARATOR first, then COMPOSE_FILE (§20 erratum 348
		// (e)): on Windows compose splits the file list on `;` unless told
		// otherwise, so a block without the line names one file nobody has.
		if vals["COMPOSE_PATH_SEPARATOR"] != ":" || countAssignments(c.Block, "COMPOSE_PATH_SEPARATOR") != 1 {
			t.Errorf("%s: COMPOSE_PATH_SEPARATOR=: is not assigned exactly once", c.What)
		}
		if i, j := strings.Index(c.Block, "\nCOMPOSE_PATH_SEPARATOR="), strings.Index(c.Block, "\nCOMPOSE_FILE="); i < 0 || j < 0 || i > j {
			t.Errorf("%s: COMPOSE_PATH_SEPARATOR must come before COMPOSE_FILE", c.What)
		}
		for _, name := range []string{"COMPOSE_FILE", "STORAGE_BACKEND", "MEDIA_DELIVERY"} {
			if n := countAssignments(c.Block, name); n != 1 {
				t.Errorf("%s: %s is assigned %d times, want exactly 1", c.What, name, n)
			}
			if vals[name] == "" {
				t.Errorf("%s: %s is assigned an empty value", c.What, name)
			}
		}
		if got := vals["STORAGE_BACKEND"]; got != c.Option.Backend {
			t.Errorf("%s: block says STORAGE_BACKEND=%s, table says %s", c.What, got, c.Option.Backend)
		}
		// Every S3_* this app READS, on every block that lands on a bucket —
		// the half of the total-block rule the four outcome variables above do
		// not cover (§20 erratum 298 (a)). An omitted one does not fall back
		// to anything: the value in the `.env` you are editing SURVIVES, and
		// compose's `${NAME:-default}` hands it straight to the app, and a
		// walk from one bucket to another would keep talking to the bucket
		// you are leaving.
		for _, name := range setupS3Vars {
			n := countAssignments(c.Block, name)
			if vals["STORAGE_BACKEND"] == config.BackendS3 && n != 1 {
				t.Errorf("%s: %s is assigned %d times, want exactly 1 (a bucket block assigns every S3_* the app reads)", c.What, name, n)
			}
			// …and a block that lands on a DIRECTORY assigns none of them: on
			// `fs` any of the four is a boot error naming itself, and
			// compose.fs.yaml hard-sets them empty on the app service.
			if vals["STORAGE_BACKEND"] == config.BackendFS && n != 0 {
				t.Errorf("%s: an fs block assigns %s", c.What, name)
			}
		}
		// The observable half of the COMPOSE_FILE assignment: `docker compose
		// ps` shows one service on every flavour since §20 erratum 345 (three
		// on the bundled MinIO before it), so a services line reading three is
		// a row that still assumes the withdrawn server.
		wantThree := vals["COMPOSE_FILE"] == "compose.yaml"
		if got := strings.HasPrefix(c.Option.Services, "3 "); got != wantThree {
			t.Errorf("%s: COMPOSE_FILE=%s but the services line reads %q", c.What, vals["COMPOSE_FILE"], c.Option.Services)
		}
		if got := vals["MEDIA_DELIVERY"]; got != c.Delivery {
			t.Errorf("%s: MEDIA_DELIVERY=%s, want %s", c.What, got, c.Delivery)
		}
		// Behind a tunnel — Cloudflare's or a Pass name's — every viewer arrives
		// from one address, so the block names the proxy (DEPLOY.md §4); reached
		// directly it names none, because there is no proxy to trust.
		wantProxies := 0
		if c.Exposure != exposureOther {
			wantProxies = 1
		}
		if n := countAssignments(c.Block, "TRUSTED_PROXIES"); n != wantProxies {
			t.Errorf("%s: TRUSTED_PROXIES is assigned %d times, want %d", c.What, n, wantProxies)
		}
		if strings.Contains(strings.ToLower(c.Block), "storage migrate") {
			t.Errorf("%s: the block prints a data-moving command", c.What)
		}
	}
}

// setupS3Vars is every `S3_*` this app reads: the four of `config.S3Vars` —
// which is the list `fs` refuses to boot with — plus the three that have a
// default (`internal/config/config.go`'s storage block). The last three matter
// to a walk exactly as much as the first four do, because each of them decides
// an outcome and each of them SURVIVES a `.env` edit that does not name it: a
// `false` S3_FORCE_PATH_STYLE left behind by a hosted provider breaks a
// self-hosted server, a stale S3_REGION signs against the wrong one, and a
// stale S3_KEY_PREFIX silently moves where every object in the bucket lands.
var setupS3Vars = append(append([]string{}, config.S3Vars...),
	"S3_REGION", "S3_FORCE_PATH_STYLE", "S3_KEY_PREFIX")

// setupBlockCase is one block a walk can print, and the exposure that prints
// it: every option on every exposure (§20 erratum 348 (b) — the folder walks
// on a tunnel too). "Any other S3-compatible bucket" is the one walk whose
// MEDIA_DELIVERY changes with the exposure, because its endpoint may be one a
// viewer's browser cannot reach (§20 erratum 298 (f)).
type setupBlockCase struct {
	Option   setupOption
	What     string // the option id, plus the exposure when there are two
	Exposure string
	Block    string
	Delivery string // the MEDIA_DELIVERY this block must assign
}

func setupBlockCases() []setupBlockCase {
	var out []setupBlockCase
	for _, o := range setupOptions() {
		// `redirect` on a directory is a boot error, so the local option asks
		// for `proxy`; so does the one bucket walk that may be pointed at a NAS.
		want := config.DeliveryRedirect
		if o.Local || o.BlockCF != "" {
			want = config.DeliveryProxy
		}
		out = append(out, setupBlockCase{Option: o, What: o.ID, Exposure: exposureOther, Block: o.Block, Delivery: want})
		// …and on a tunnel. Each block there ends with the one line the tunnel
		// decides, TRUSTED_PROXIES (DEPLOY.md §4); the `other` walk also asks
		// for `redirect`, because there the bytes should not come out of this
		// box (erratum 299), and its note is what says the endpoint must then
		// be public. The folder stays on `proxy` — `redirect` on a directory is
		// a boot error — and its note names the risk (erratum 348 (b)).
		tunnel := config.DeliveryRedirect
		if o.Local {
			tunnel = config.DeliveryProxy
		}
		block, _ := setupBlockFor(o, exposureCloudflare)
		out = append(out, setupBlockCase{Option: o, What: o.ID + " (cloudflare)",
			Exposure: exposureCloudflare, Block: block, Delivery: tunnel})
		// …and on a Clippet Pass name (§20 erratum 303): the Cloudflare block
		// plus the release and the name's lines, so its delivery mode is that
		// block's.
		block, _ = setupBlockFor(o, exposurePass)
		out = append(out, setupBlockCase{Option: o, What: o.ID + " (pass)",
			Exposure: exposurePass, Block: block, Delivery: tunnel})
	}
	return out
}

// TestSetupBlocksLoad is the pin the whole page is for: every walk ends in a
// block that config.Load ACCEPTS. The placeholders are filled with values of
// the shape each one describes — that substitution is the test's, never the
// page's — so what is proven is that the block names the right variables, no
// wrong ones, and values the loader's own rules admit.
func TestSetupBlocksLoad(t *testing.T) {
	for _, c := range setupBlockCases() {
		env := setupBlockEnv(t, c.Block, "http://localhost:8481")
		cfg, err := config.Load(func(k string) string { return env[k] })
		if err != nil {
			t.Errorf("%s: the block does not load: %v", c.What, err)
			continue
		}
		if cfg.StorageBackend != c.Option.Backend {
			t.Errorf("%s: loaded STORAGE_BACKEND=%s, want %s", c.What, cfg.StorageBackend, c.Option.Backend)
		}
		if c.Option.Local && cfg.RedirectsMedia() {
			t.Errorf("%s: a local store loaded in redirect mode", c.What)
		}
	}
}

// TestSetupOtherWalkBootsAgainstASelfHostedEndpoint is the case that walk's
// own caveat names FIRST — MinIO on the NAS, a LAN address — against the rule
// that decides whether its block boots at all (§20 erratum 298 (f)).
//
// `MEDIA_DELIVERY=redirect` sends the VIEWER'S browser to the endpoint, so
// under an https instance a plain-http one is active mixed content and
// `config.Load` refuses to boot, naming MEDIA_PRESIGN_ENDPOINT; on a plain-http
// instance it boots and every viewer off the LAN is redirected to a host they
// cannot reach. Nothing else in this package or in the E2E exercises that
// combination — step 92 boots the R2 block on a loopback base URL, which dodges
// the refusal — so the walk shipped a block that could not boot for the case it
// was written for.
func TestSetupOtherWalkBootsAgainstASelfHostedEndpoint(t *testing.T) {
	o, ok := setupOptionByID("other")
	if !ok {
		t.Fatal("the table has no `other` option")
	}
	const nas = "http://nas.lan:9000" // a plain-http origin on somebody's own network
	load := func(block, endpoint string) error {
		env := setupBlockEnv(t, block, "https://clips.example.test")
		env["S3_ENDPOINT"] = endpoint
		_, err := config.Load(func(k string) string { return env[k] })
		return err
	}
	if err := load(o.Block, nas); err != nil {
		t.Errorf("the `other` block does not boot against a self-hosted endpoint: %v", err)
	}
	// …and the proof that the one line is what decides it: the same block with
	// the hosted walks' `redirect` refuses, naming the variable.
	redirect := strings.Replace(o.Block,
		"MEDIA_DELIVERY="+config.DeliveryProxy, "MEDIA_DELIVERY="+config.DeliveryRedirect, 1)
	err := load(redirect, nas)
	if err == nil {
		t.Fatal("redirect to a plain-http endpoint under an https instance loaded")
	}
	if !strings.Contains(err.Error(), "MEDIA_PRESIGN_ENDPOINT") {
		t.Errorf("the refusal does not name the variable: %v", err)
	}
	// The Cloudflare block DOES ask for `redirect` — there the bytes may not
	// come out of this box — and a bucket with a secure public origin is what
	// its note says it then needs.
	if err := load(o.BlockCF, "https://bucket.example.test"); err != nil {
		t.Errorf("the `other` block on a tunnel does not boot against a public bucket: %v", err)
	}
}

// setupBlockEnv turns a generated block into the environment `config.Load`
// would see, with placeholders filled with values of the shape each one
// describes. That substitution is the TEST's, never the page's.
func setupBlockEnv(t *testing.T, block, base string) map[string]string {
	t.Helper()
	vals := parseEnvBlock(t, block)
	env := map[string]string{
		"PUBLIC_BASE_URL": base,
		"DATA_DIR":        "/data",
		"SCRATCH_DIR":     "/scratch",
	}
	for k, v := range vals {
		if k == "COMPOSE_FILE" || k == "COMPOSE_PATH_SEPARATOR" || k == "STORAGE_DIR_HOST" ||
			k == "CLIPPET_PASS_TOKEN" || k == "CLIPPET_COMMIT" {
			// None of these is the app's: COMPOSE_FILE and its separator
			// select the files, STORAGE_DIR_HOST is the bind's host side,
			// CLIPPET_COMMIT is a BUILD argument (DEPLOY.md §16), and
			// CLIPPET_PASS_TOKEN belongs to the Pass container, which is a
			// separate image this app never speaks to (§20 erratum 303).
			// Dropping the token here is itself an assertion: the block loads
			// WITHOUT it, so nothing in the app reads it.
			continue
		}
		env[k] = fillPlaceholder(t, k, v)
	}
	if vals["STORAGE_BACKEND"] == config.BackendFS {
		env["STORAGE_DIR"] = "/media"
	}
	return env
}

// setupBlockGolden is the byte-for-byte text of every block this table can
// print: the five walks, the five a Cloudflare tunnel turns them into (each
// ends with TRUSTED_PROXIES, DEPLOY.md §4, and `other` also switches to
// redirect), and the five a Clippet Pass name turns them into (§20 errata 303,
// 348 — the folder walks on both since erratum 348 (b), and every block opens
// with COMPOSE_PATH_SEPARATOR, every Pass block with the release).
// A block is the one thing on this page a person PASTES, so it is pinned as
// bytes and not as a set of properties: a stray space, a reordered line or a
// lost comment is a change to somebody's `.env`, and it should be read in a
// diff rather than discovered in a paste.
const setupBlockGolden = `--- fs ---
# .env — a directory on this machine, one container, no bucket
COMPOSE_PATH_SEPARATOR=:
COMPOSE_FILE=compose.yaml:compose.fs.yaml
STORAGE_BACKEND=fs
#STORAGE_DIR_HOST=<the host directory, or your NAS mount point>
MEDIA_DELIVERY=proxy
STORAGE_PRICING=local
# The four S3_* lines are unused on this flavour and compose supplies them
# EMPTY.
# If your uplink is METERED the disk is free and the household bill is not:
# type EGRESS_PRICE_PER_GB over the preset.
--- fs (cloudflare) ---
# .env — a directory on this machine, one container, no bucket
COMPOSE_PATH_SEPARATOR=:
COMPOSE_FILE=compose.yaml:compose.fs.yaml
STORAGE_BACKEND=fs
#STORAGE_DIR_HOST=<the host directory, or your NAS mount point>
MEDIA_DELIVERY=proxy
STORAGE_PRICING=local
# The four S3_* lines are unused on this flavour and compose supplies them
# EMPTY.
# If your uplink is METERED the disk is free and the household bill is not:
# type EGRESS_PRICE_PER_GB over the preset.
TRUSTED_PROXIES=172.16.0.0/12
--- fs (pass) ---
# .env — a directory on this machine, one container, no bucket, on a Clippet Pass name
# First: check out the latest vYYYY.MM.DD tag and set CLIPPET_COMMIT to its
# commit (DEPLOY.md §36.2).
CLIPPET_COMMIT=<the 40-hex commit of that tag: git rev-parse vYYYY.MM.DD^{commit}>
COMPOSE_PATH_SEPARATOR=:
COMPOSE_FILE=compose.yaml:compose.fs.yaml:compose.pass.yaml
STORAGE_BACKEND=fs
#STORAGE_DIR_HOST=<the host directory, or your NAS mount point>
MEDIA_DELIVERY=proxy
STORAGE_PRICING=local
# The four S3_* lines are unused on this flavour and compose supplies them
# EMPTY.
# If your uplink is METERED the disk is free and the household bill is not:
# type EGRESS_PRICE_PER_GB over the preset.
TRUSTED_PROXIES=172.16.0.0/12
# ...and the name itself. The first two are this app's own and have to be
# right from the FIRST boot on the name: every card tag and every share
# link is built from the base URL, and the cookie prefix is refused on a
# plain-http base. The third belongs to the Pass container and reaches
# this app never.
PUBLIC_BASE_URL=https://<name>.clippet.gg
SESSION_COOKIE_NAME=__Host-clippet_session
CLIPPET_PASS_TOKEN=<paste the token from your Clippet portal>
--- r2 ---
# .env — Cloudflare R2
COMPOSE_PATH_SEPARATOR=:
COMPOSE_FILE=compose.yaml:compose.nobucket.yaml
STORAGE_BACKEND=s3
S3_ENDPOINT=<paste the endpoint the token dialog showed — whole, scheme and all>
S3_REGION=auto
S3_BUCKET=<your bucket>
S3_ACCESS_KEY=<access key id>
S3_SECRET_KEY=<secret access key>
S3_FORCE_PATH_STYLE=false
S3_KEY_PREFIX=
MEDIA_DELIVERY=redirect
STORAGE_PRICING=r2
--- r2 (cloudflare) ---
# .env — Cloudflare R2
COMPOSE_PATH_SEPARATOR=:
COMPOSE_FILE=compose.yaml:compose.nobucket.yaml
STORAGE_BACKEND=s3
S3_ENDPOINT=<paste the endpoint the token dialog showed — whole, scheme and all>
S3_REGION=auto
S3_BUCKET=<your bucket>
S3_ACCESS_KEY=<access key id>
S3_SECRET_KEY=<secret access key>
S3_FORCE_PATH_STYLE=false
S3_KEY_PREFIX=
MEDIA_DELIVERY=redirect
STORAGE_PRICING=r2
TRUSTED_PROXIES=172.16.0.0/12
--- r2 (pass) ---
# .env — Cloudflare R2, on a Clippet Pass name
# First: check out the latest vYYYY.MM.DD tag and set CLIPPET_COMMIT to its
# commit (DEPLOY.md §36.2).
CLIPPET_COMMIT=<the 40-hex commit of that tag: git rev-parse vYYYY.MM.DD^{commit}>
COMPOSE_PATH_SEPARATOR=:
COMPOSE_FILE=compose.yaml:compose.nobucket.yaml:compose.pass.yaml
STORAGE_BACKEND=s3
S3_ENDPOINT=<paste the endpoint the token dialog showed — whole, scheme and all>
S3_REGION=auto
S3_BUCKET=<your bucket>
S3_ACCESS_KEY=<access key id>
S3_SECRET_KEY=<secret access key>
S3_FORCE_PATH_STYLE=false
S3_KEY_PREFIX=
MEDIA_DELIVERY=redirect
STORAGE_PRICING=r2
TRUSTED_PROXIES=172.16.0.0/12
# ...and the name itself. The first two are this app's own and have to be
# right from the FIRST boot on the name: every card tag and every share
# link is built from the base URL, and the cookie prefix is refused on a
# plain-http base. The third belongs to the Pass container and reaches
# this app never.
PUBLIC_BASE_URL=https://<name>.clippet.gg
SESSION_COOKIE_NAME=__Host-clippet_session
CLIPPET_PASS_TOKEN=<paste the token from your Clippet portal>
--- b2 ---
# .env — Backblaze B2
COMPOSE_PATH_SEPARATOR=:
COMPOSE_FILE=compose.yaml:compose.nobucket.yaml
STORAGE_BACKEND=s3
S3_ENDPOINT=<paste the bucket's endpoint — whole, scheme and all>
S3_REGION=<the region string inside that endpoint>
S3_BUCKET=<your bucket>
S3_ACCESS_KEY=<the key id>
S3_SECRET_KEY=<the application key>
S3_FORCE_PATH_STYLE=false
S3_KEY_PREFIX=
MEDIA_DELIVERY=redirect
STORAGE_PRICING=b2
--- b2 (cloudflare) ---
# .env — Backblaze B2
COMPOSE_PATH_SEPARATOR=:
COMPOSE_FILE=compose.yaml:compose.nobucket.yaml
STORAGE_BACKEND=s3
S3_ENDPOINT=<paste the bucket's endpoint — whole, scheme and all>
S3_REGION=<the region string inside that endpoint>
S3_BUCKET=<your bucket>
S3_ACCESS_KEY=<the key id>
S3_SECRET_KEY=<the application key>
S3_FORCE_PATH_STYLE=false
S3_KEY_PREFIX=
MEDIA_DELIVERY=redirect
STORAGE_PRICING=b2
TRUSTED_PROXIES=172.16.0.0/12
--- b2 (pass) ---
# .env — Backblaze B2, on a Clippet Pass name
# First: check out the latest vYYYY.MM.DD tag and set CLIPPET_COMMIT to its
# commit (DEPLOY.md §36.2).
CLIPPET_COMMIT=<the 40-hex commit of that tag: git rev-parse vYYYY.MM.DD^{commit}>
COMPOSE_PATH_SEPARATOR=:
COMPOSE_FILE=compose.yaml:compose.nobucket.yaml:compose.pass.yaml
STORAGE_BACKEND=s3
S3_ENDPOINT=<paste the bucket's endpoint — whole, scheme and all>
S3_REGION=<the region string inside that endpoint>
S3_BUCKET=<your bucket>
S3_ACCESS_KEY=<the key id>
S3_SECRET_KEY=<the application key>
S3_FORCE_PATH_STYLE=false
S3_KEY_PREFIX=
MEDIA_DELIVERY=redirect
STORAGE_PRICING=b2
TRUSTED_PROXIES=172.16.0.0/12
# ...and the name itself. The first two are this app's own and have to be
# right from the FIRST boot on the name: every card tag and every share
# link is built from the base URL, and the cookie prefix is refused on a
# plain-http base. The third belongs to the Pass container and reaches
# this app never.
PUBLIC_BASE_URL=https://<name>.clippet.gg
SESSION_COOKIE_NAME=__Host-clippet_session
CLIPPET_PASS_TOKEN=<paste the token from your Clippet portal>
--- s3 ---
# .env — Amazon S3
COMPOSE_PATH_SEPARATOR=:
COMPOSE_FILE=compose.yaml:compose.nobucket.yaml
STORAGE_BACKEND=s3
S3_ENDPOINT=<scheme>://s3.<your region>.amazonaws.com
S3_REGION=<your region>
S3_BUCKET=<your bucket>
S3_ACCESS_KEY=<access key id>
S3_SECRET_KEY=<secret access key>
S3_FORCE_PATH_STYLE=false
S3_KEY_PREFIX=
MEDIA_DELIVERY=redirect
# The s3 preset models NO free storage on purpose. What a new account gets is a
# credit, not an allowance, and pricing a credit nobody is still on would
# understate every later bill.
STORAGE_PRICING=s3
--- s3 (cloudflare) ---
# .env — Amazon S3
COMPOSE_PATH_SEPARATOR=:
COMPOSE_FILE=compose.yaml:compose.nobucket.yaml
STORAGE_BACKEND=s3
S3_ENDPOINT=<scheme>://s3.<your region>.amazonaws.com
S3_REGION=<your region>
S3_BUCKET=<your bucket>
S3_ACCESS_KEY=<access key id>
S3_SECRET_KEY=<secret access key>
S3_FORCE_PATH_STYLE=false
S3_KEY_PREFIX=
MEDIA_DELIVERY=redirect
# The s3 preset models NO free storage on purpose. What a new account gets is a
# credit, not an allowance, and pricing a credit nobody is still on would
# understate every later bill.
STORAGE_PRICING=s3
TRUSTED_PROXIES=172.16.0.0/12
--- s3 (pass) ---
# .env — Amazon S3, on a Clippet Pass name
# First: check out the latest vYYYY.MM.DD tag and set CLIPPET_COMMIT to its
# commit (DEPLOY.md §36.2).
CLIPPET_COMMIT=<the 40-hex commit of that tag: git rev-parse vYYYY.MM.DD^{commit}>
COMPOSE_PATH_SEPARATOR=:
COMPOSE_FILE=compose.yaml:compose.nobucket.yaml:compose.pass.yaml
STORAGE_BACKEND=s3
S3_ENDPOINT=<scheme>://s3.<your region>.amazonaws.com
S3_REGION=<your region>
S3_BUCKET=<your bucket>
S3_ACCESS_KEY=<access key id>
S3_SECRET_KEY=<secret access key>
S3_FORCE_PATH_STYLE=false
S3_KEY_PREFIX=
MEDIA_DELIVERY=redirect
# The s3 preset models NO free storage on purpose. What a new account gets is a
# credit, not an allowance, and pricing a credit nobody is still on would
# understate every later bill.
STORAGE_PRICING=s3
TRUSTED_PROXIES=172.16.0.0/12
# ...and the name itself. The first two are this app's own and have to be
# right from the FIRST boot on the name: every card tag and every share
# link is built from the base URL, and the cookie prefix is refused on a
# plain-http base. The third belongs to the Pass container and reaches
# this app never.
PUBLIC_BASE_URL=https://<name>.clippet.gg
SESSION_COOKIE_NAME=__Host-clippet_session
CLIPPET_PASS_TOKEN=<paste the token from your Clippet portal>
--- other ---
# .env — any S3-compatible bucket
COMPOSE_PATH_SEPARATOR=:
COMPOSE_FILE=compose.yaml:compose.nobucket.yaml
STORAGE_BACKEND=s3
S3_ENDPOINT=<scheme>://<host>[:<port>]
S3_REGION=<whatever the provider calls it; MinIO ignores it>
S3_BUCKET=<your bucket>
S3_ACCESS_KEY=<key id>
S3_SECRET_KEY=<secret>
S3_FORCE_PATH_STYLE=<true for a self-hosted server, false for a hosted one>
S3_KEY_PREFIX=<optional, if you are sharing a bucket>
MEDIA_DELIVERY=proxy
STORAGE_PRICE_PER_GB_MONTH=<what keeping 1 GB for a month costs you>
EGRESS_PRICE_PER_GB=<what sending 1 GB costs you; omit if it is free>
--- other (cloudflare) ---
# .env — any S3-compatible bucket, on a Cloudflare tunnel
COMPOSE_PATH_SEPARATOR=:
COMPOSE_FILE=compose.yaml:compose.nobucket.yaml
STORAGE_BACKEND=s3
S3_ENDPOINT=<scheme>://<host>[:<port>]
S3_REGION=<whatever the provider calls it; MinIO ignores it>
S3_BUCKET=<your bucket>
S3_ACCESS_KEY=<key id>
S3_SECRET_KEY=<secret>
S3_FORCE_PATH_STYLE=<true for a self-hosted server, false for a hosted one>
S3_KEY_PREFIX=<optional, if you are sharing a bucket>
MEDIA_DELIVERY=redirect
STORAGE_PRICE_PER_GB_MONTH=<what keeping 1 GB for a month costs you>
EGRESS_PRICE_PER_GB=<what sending 1 GB costs you; omit if it is free>
TRUSTED_PROXIES=172.16.0.0/12
--- other (pass) ---
# .env — any S3-compatible bucket, on a Clippet Pass name
# First: check out the latest vYYYY.MM.DD tag and set CLIPPET_COMMIT to its
# commit (DEPLOY.md §36.2).
CLIPPET_COMMIT=<the 40-hex commit of that tag: git rev-parse vYYYY.MM.DD^{commit}>
COMPOSE_PATH_SEPARATOR=:
COMPOSE_FILE=compose.yaml:compose.nobucket.yaml:compose.pass.yaml
STORAGE_BACKEND=s3
S3_ENDPOINT=<scheme>://<host>[:<port>]
S3_REGION=<whatever the provider calls it; MinIO ignores it>
S3_BUCKET=<your bucket>
S3_ACCESS_KEY=<key id>
S3_SECRET_KEY=<secret>
S3_FORCE_PATH_STYLE=<true for a self-hosted server, false for a hosted one>
S3_KEY_PREFIX=<optional, if you are sharing a bucket>
MEDIA_DELIVERY=redirect
STORAGE_PRICE_PER_GB_MONTH=<what keeping 1 GB for a month costs you>
EGRESS_PRICE_PER_GB=<what sending 1 GB costs you; omit if it is free>
TRUSTED_PROXIES=172.16.0.0/12
# ...and the name itself. The first two are this app's own and have to be
# right from the FIRST boot on the name: every card tag and every share
# link is built from the base URL, and the cookie prefix is refused on a
# plain-http base. The third belongs to the Pass container and reaches
# this app never.
PUBLIC_BASE_URL=https://<name>.clippet.gg
SESSION_COOKIE_NAME=__Host-clippet_session
CLIPPET_PASS_TOKEN=<paste the token from your Clippet portal>
`

func TestSetupBlocksAreTheGolden(t *testing.T) {
	var b strings.Builder
	for _, c := range setupBlockCases() {
		b.WriteString("--- " + c.What + " ---\n")
		b.WriteString(c.Block)
	}
	if b.String() != setupBlockGolden {
		t.Errorf("the generated blocks have changed:\n%s", b.String())
	}
}

// ---- the page ------------------------------------------------------------

// setupHandler builds a handler on a fixture's database with a backend of the
// caller's choosing, so one test can be an `fs` instance and the next a
// bucket without a second fixture.
func setupHandler(t *testing.T, f *fixture, backend, endpoint string, a auth.Authenticator) http.Handler {
	t.Helper()
	base, _ := url.Parse(testBase)
	h, err := New(Deps{
		DB: f.db, Auth: a, URLs: urls.New(base, "b1"), BuildID: "b1",
		Cfg: config.Config{SSRTimeout: f.ssrTimeout, Registration: f.registration,
			SessionCookie:  "clippet_session",
			StorageBackend: backend, StorageDir: "/media", SetupWizardOff: f.setupWizardOff,
			S3: config.S3Config{Endpoint: endpoint, KeyPrefix: "p/"}},
		Now:    func() time.Time { return time.Unix(f.now, 0) },
		Health: &httpx.StorageHealth{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

// TestSetupIsSignedInOrUnconfigured pins who may open the wizard: a session,
// or anybody at all on an instance nobody has registered on yet — a first-run
// wizard you have to log in to see is not a first-run wizard. The moment the
// first account exists the door closes.
func TestSetupIsSignedInOrUnconfigured(t *testing.T) {
	f := newFixture(t)
	rec := get(setupHandler(t, f, config.BackendS3, "", passAuth{}), "/setup")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("anonymous on a configured instance = %d, want 303", rec.Code)
	}
	// The unescaped form redirectToLogin has always written: next is one of
	// this package's own route paths, so it needs no escaping (server.go).
	if loc := rec.Header().Get("Location"); loc != "/login?next=/setup" {
		t.Errorf("Location %q", loc)
	}
	rec = get(setupHandler(t, f, config.BackendS3, "", session(f.owner)), "/setup")
	if rec.Code != http.StatusOK {
		t.Fatalf("signed in = %d, want 200", rec.Code)
	}

	// …and the same page, anonymously, on an instance with no users at all.
	empty := emptyFixture(t)
	rec = get(setupHandler(t, empty, config.BackendS3, "", passAuth{}), "/setup")
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous on an unconfigured instance = %d, want 200", rec.Code)
	}
	mustContain(t, rec.Body.String(), "Where do clips live?")
	// With nobody signed in there is nobody for `Check now` to run as, and the
	// page says so rather than drawing a button that would 401.
	mustNotContain(t, rec.Body.String(), "data-setup-check>")
}

// TestSetupChooserRendersEverything is the page's own shape (§20 errata 309,
// 311): the four state rows, the five radio cards each with its fact in its
// state cell, the Reached seg of three radios with `guessed` in its state cell
// until `via` is chosen, no hint under either control until a rule bites, the
// two muted heads of the steps not reached, the `labels read` date once under
// a walk's list, no inline anything, and an unknown `?store=` answering 200
// with the chooser and no panel.
func TestSetupChooserRendersEverything(t *testing.T) {
	f := newFixture(t)
	h := setupHandler(t, f, config.BackendS3, "", session(f.owner))
	rec := get(h, "/setup")
	assertPageHeaders(t, rec, http.StatusOK, "private, no-store")
	body := rec.Body.String()
	assertNoInline(t, "setup", body)
	// The panels read as a numbered sequence (§20 errata 310, 311): the facts
	// panel is headed `Now` and unnumbered — it is not a step — then `1 · Pick
	// a storage backend`, and before any pick the muted heads `2 · Set up` and
	// `3 · Apply` alone, so the route is visible before the first step. The
	// `clips` row is on the Now panel: the fact the STOP names later.
	mustContain(t, body, `data-setup-backend="s3"`, "a bucket (s3)", "<h2>Now</h2>",
		`<h2><span class="n">1</span> Pick a storage backend</h2>`,
		`<div class="panel later" id="walk-later" data-setup-later>`, `<h2><span class="n">2</span> Set up</h2>`,
		`<div class="panel later" id="apply-later" data-setup-later>`, `<h2><span class="n">3</span> Apply</h2>`,
		`<span class="rl">clips</span>`, `<span class="rs">guessed</span>`)
	mustNotContain(t, body, "Current store", ">Pick a store<", "Storage backend</", `<label for="store"`,
		`<select id="store"`, `id="walk"`, `id="apply"`)
	// The five choices are five radio cards shown at once — a closed `<select>`
	// hid the one thing a first-time reader needs to compare — each its name,
	// then its one fact in the state cell (never a suffix on the name).
	if n := strings.Count(body, `type="radio" name="store"`); n != 5 {
		t.Errorf("the chooser has %d store radios, want 5", n)
	}
	mustContain(t, body, `<div class="choices" data-setup-store>`)
	for _, o := range setupOptions() {
		mustContain(t, body, `<label class="choice"><input type="radio" name="store" value="`+o.ID+`"><span class="dot"></span><span class="name">`+o.Label+`</span><span class="rs">`+o.Hint+`</span></label>`)
		if strings.Contains(o.Label, " — ") {
			t.Errorf("%s: the card's name carries its fact: %q", o.ID, o.Label)
		}
	}
	// The exposure is a closed set of three, so it is a `.seg` of radios and
	// never a <select> (web/STYLE.md); each chip reads as a sentence with the
	// row's label, and the control carries no hint of its own.
	if n := strings.Count(body, `type="radio" name="via"`); n != 3 {
		t.Errorf("the Reached seg has %d radios, want 3", n)
	}
	mustContain(t, body, `role="radiogroup" aria-label="Reached"`, ">directly<", ">via Cloudflare<", ">via a Clippet Pass<")
	mustNotContain(t, body, `<select id="via"`, "data-setup-refusal", "data-setup-walk=", "Guessed from this request", "walks read")
	if setupURLRE.MatchString(strings.ReplaceAll(body, testBase, "")) {
		t.Errorf("the rendered chooser carries a URL other than this instance's own base")
	}
	// Once `via` is chosen the Reached state cell is empty, the chosen card is
	// checked and still says its fact, step 2 is headed by the backend's own
	// name, step 3 is Apply, the muted heads are gone, and the dated `labels
	// read` line is under the walk's list — once, and nowhere on the bare
	// chooser.
	walk := get(h, "/setup?store=r2&via="+exposureOther).Body.String()
	mustContain(t, walk, "labels read "+walksRead,
		`<input type="radio" name="store" value="r2" checked><span class="dot"></span><span class="name">Cloudflare R2</span><span class="rs">10 GB free · egress free</span>`,
		`<h2><span class="n">2</span> Cloudflare R2</h2>`, `<h2><span class="n">3</span> Apply</h2>`, `id="walk"`, `id="apply"`)
	// The list runs full width in the walk's body — no label column, no
	// `steps` label — the dated line under it in the state cell's face, then
	// `your .env` with its Copy beside it, then the block (§20 erratum 311).
	mustContain(t, walk, `<div class="walk-body">`, `<ol class="walk">`,
		`<div class="walk-meta"><span class="rs">labels read `+walksRead+`</span></div>`,
		`<div class="env-head"><span class="rl">your .env</span><button type="button" class="btn small" data-setup-copy>Copy</button></div>`)
	mustNotContain(t, walk, `<span class="rl">steps</span>`, `row top steps`, "<h2>Apply</h2>", "<h2>Cloudflare R2</h2>",
		"Set up Cloudflare R2", `id="walk-later"`, `id="apply-later"`, "panel later")
	// Every card's state cell fits `.rs`'s 26ch on one line: a wrapped mono
	// state cell reads as broken (the review's own finding on the 30ch one).
	for _, o := range setupOptions() {
		if n := len([]rune(o.Hint)); n > 26 {
			t.Errorf("%s: the state cell %q is %d characters, the cell is 26ch", o.ID, o.Hint, n)
		}
	}
	mustNotContain(t, walk, `<span class="rs">guessed</span>`)
	if n := strings.Count(walk, "labels read "); n != 1 {
		t.Errorf("the walk prints the date %d times, want once", n)
	}

	rec = get(h, "/setup?store=zzz")
	if rec.Code != http.StatusOK {
		t.Fatalf("an unknown store = %d, want 200", rec.Code)
	}
	mustContain(t, rec.Body.String(), "That is not one of the five.", `id="walk-later"`, `id="apply-later"`)
	mustNotContain(t, rec.Body.String(), "data-setup-walk=", `id="apply"`)
}

// TestSetupWalkRendersItsBlock walks every option and asserts the rendered
// page carries exactly one COMPOSE_FILE and one STORAGE_BACKEND line, no URL
// but this instance's own, and no `storage migrate` on any surface (§20
// erratum 297: the page prints no data-moving command, in either direction).
func TestSetupWalkRendersItsBlock(t *testing.T) {
	f := newFixture(t)
	h := setupHandler(t, f, config.BackendS3, "", session(f.owner))
	for _, o := range setupOptions() {
		rec := get(h, "/setup?store="+o.ID+"&via="+exposureOther)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", o.ID, rec.Code)
		}
		body := rec.Body.String()
		assertNoInline(t, "setup "+o.ID, body)
		if n := countAssignments(body, "COMPOSE_FILE"); n != 1 {
			t.Errorf("%s: the page renders %d COMPOSE_FILE lines", o.ID, n)
		}
		if n := countAssignments(body, "STORAGE_BACKEND"); n != 1 {
			t.Errorf("%s: the page renders %d STORAGE_BACKEND lines", o.ID, n)
		}
		// No PASTEABLE migrate command, in either direction (§20 erratum
		// 297). The STOP row names the verb and DEPLOY §32.3 so an operator
		// can find them; what it must never print is a form somebody could
		// copy, because the documented one carries a bind whose omission
		// silently throws the copy away.
		if strings.Contains(body, "storage migrate --to") || strings.Contains(body, "migrate --dry-run") {
			t.Errorf("%s: the page prints a runnable migrate command", o.ID)
		}
		if setupURLRE.MatchString(strings.ReplaceAll(body, testBase, "")) {
			t.Errorf("%s: the rendered walk carries a URL", o.ID)
		}
	}
	// The one that closes MAJOR 1's direction inside this package too: on an
	// `fs` instance, the B2 panel's block still names the external-bucket
	// flavour rather than leaving the `fs` one in place.
	fsh := setupHandler(t, f, config.BackendFS, "", session(f.owner))
	mustContain(t, get(fsh, "/setup?store=b2&via="+exposureOther).Body.String(),
		"COMPOSE_FILE=compose.yaml:compose.nobucket.yaml")

	// The one walk whose MEDIA_DELIVERY is not a function of the target alone
	// (§20 erratum 298 (f)): a bucket whose endpoint may be a NAS proxies by
	// default, and asks for a redirect only where the terms require one.
	mustContain(t, get(h, "/setup?store=other&via="+exposureOther).Body.String(),
		"MEDIA_DELIVERY="+config.DeliveryProxy)
	cf := get(h, "/setup?store=other&via="+exposureCloudflare).Body.String()
	mustContain(t, cf, "MEDIA_DELIVERY="+config.DeliveryRedirect, "a LAN address or a NAS will not work")
	mustNotContain(t, cf, "MEDIA_DELIVERY="+config.DeliveryProxy)
}

// TestSetupFolderBehindCloudflareWalks is §20 erratum 348 (b), the owner's verdict
// of 2026-10-14 (never refuse; name the risk), which withdrew erratum 299's
// refusal. Behind a request that came through Cloudflare (CF-Ray) the folder
// option WALKS: its card keeps its own fact, the block is the folder block plus
// TRUSTED_PROXIES and still `proxy`, and the risk — the desktop app's own
// folder-card words — is the walk's closing line, said once. No `refused`
// anywhere, no refusal callout. The control still overrides the header.
func TestSetupFolderBehindCloudflareWalks(t *testing.T) {
	f := newFixture(t)
	h := setupHandler(t, f, config.BackendS3, "", session(f.owner))
	o, _ := setupOptionByID(config.BackendFS)
	rec := do(h, http.MethodGet, "/setup?store=fs", "CF-Ray", "8f0c1d2e3f4a5b6c-LHR")
	body := rec.Body.String()
	mustContain(t, body, `data-setup-walk="fs"`, `id="apply"`,
		"COMPOSE_PATH_SEPARATOR=:", "COMPOSE_FILE=compose.yaml:compose.fs.yaml", "MEDIA_DELIVERY=proxy", "TRUSTED_PROXIES=172.16.0.0/12",
		`value="fs" checked><span class="dot"></span><span class="name">`+o.Label+`</span><span class="rs">`+o.Hint+`</span>`,
		`<p class="hint">`+html.EscapeString(localTunnelNote)+`</p>`)
	mustNotContain(t, body, "data-setup-refusal", "refused", "rs danger", "cannot be served through")
	if n := strings.Count(body, "Their terms reserve video for a bucket"); n != 1 {
		t.Errorf("the risk is said %d times, want once", n)
	}
	// Reached directly, the folder walk has no tunnel line and no risk line.
	body = do(h, http.MethodGet, "/setup?store=fs&via="+exposureOther, "CF-Ray", "8f0c1d2e3f4a5b6c-LHR").Body.String()
	mustContain(t, body, "COMPOSE_FILE=compose.yaml:compose.fs.yaml")
	mustNotContain(t, body, "TRUSTED_PROXIES", "Their terms reserve video")
	// …and `via=cloudflare` without the header is the tunnel answer again.
	mustContain(t, get(h, "/setup?store=fs&via="+exposureCloudflare).Body.String(), "Their terms reserve video for a bucket")
	// A bucket behind the same tunnel walks exactly as it always did, with no
	// folder risk line.
	body = do(h, http.MethodGet, "/setup?store=r2", "CF-Ray", "8f0c1d2e3f4a5b6c-LHR").Body.String()
	mustContain(t, body, "COMPOSE_FILE=compose.yaml:compose.nobucket.yaml")
	mustNotContain(t, body, "Their terms reserve video")
}

// TestSetupStopCallout pins the state-gated STOP (§20 errata 297, 311): it
// renders when this instance holds clips and the target is not what it is on,
// as a callout FIRST in the walk's body whose bold lead is the fact, it names
// the section that DOES move clips and the verb in <code>, it prints no
// command and holds no control — and it is absent on the diagonal.
func TestSetupStopCallout(t *testing.T) {
	f := newFixture(t)
	// This fixture's instance is on a directory and holds clips.
	h := setupHandler(t, f, config.BackendFS, "", session(f.owner))
	body := get(h, "/setup?store=r2&via="+exposureOther).Body.String()
	mustContain(t, body, "data-setup-stop", "§32.3", "admin storage migrate",
		`<div class="callout" data-setup-stop><b>`,
		`clips stay where they are.</b> Switching does not move them. Move them first: <code>admin storage migrate</code> (DEPLOY.md §32.3).</div>`)
	// Not a row and not a state word (errata 310, 311): no `stop` verdict, no
	// red line in the head — and the Now panel's `clips` row carries the same
	// count as a plain fact.
	mustNotContain(t, body, `<span class="rs danger">stop</span>`, `hint danger`, "in the current storage.", "run admin storage migrate first")
	mustContain(t, body, `<span class="rl">clips</span>`)
	if strings.Contains(body, "docker compose run --rm --no-deps -v") || strings.Contains(body, "storage migrate --to") {
		t.Errorf("the STOP callout printed a migrate command")
	}
	// FIRST in the walk's body, before the list.
	wb, st, ol := strings.Index(body, `<div class="walk-body">`), strings.Index(body, "data-setup-stop"), strings.Index(body, `<ol class="walk">`)
	if wb < 0 || st < wb || ol < st {
		t.Errorf("the STOP is not the first thing in the walk's body (body %d, stop %d, list %d)", wb, st, ol)
	}
	// …and it is a sentence: no button, no link, no field, no form (erratum
	// 297 (f)) — the only markup is the bold lead and the verb in <code>.
	inner := strings.SplitN(strings.SplitN(body, "data-setup-stop>", 2)[1], "</div>", 2)[0]
	for _, tag := range []string{"<button", "<a ", "<input", "<form", "<select"} {
		if strings.Contains(inner, tag) {
			t.Errorf("the STOP callout carries a control: %q", inner)
		}
	}
	mustContain(t, body, "a directory (fs)")
	// The diagonal: the option this instance is already on renders no STOP and
	// its card's state cell says `on now` (§20 errata 309, 311).
	diag := get(h, "/setup?store=fs&via="+exposureOther).Body.String()
	mustNotContain(t, diag, "data-setup-stop", "This is what you are on now.", "already", "stay where they are")
	mustContain(t, diag, `<span class="name">A folder or NAS on this machine</span><span class="rs">on now</span>`, `data-setup-walk="fs"`)
	// Between two buckets the page cannot tell whether the target is the bucket
	// it is already on, and says so — the conditional form, still no gate.
	bh := setupHandler(t, f, config.BackendS3, "", session(f.owner))
	bb := get(bh, "/setup?store=r2&via="+exposureOther).Body.String()
	mustContain(t, bb, "data-setup-stop", `clips stay where they are.</b> Switching does not move them. If the new bucket is a different one, move them first: <code>admin storage migrate</code> (DEPLOY.md §32.3). This page cannot tell whether it is.</div>`)
	mustNotContain(t, bb, "Move them first")
	// And an instance with no clips at all has nothing to stop, and says
	// `none` on the Now panel. It has no account either, so the anonymous
	// first-run door is what opens it.
	empty := emptyFixture(t)
	eh := setupHandler(t, empty, config.BackendS3, "", passAuth{})
	eb := get(eh, "/setup?store=fs").Body.String()
	mustNotContain(t, eb, "data-setup-stop")
	mustContain(t, eb, `<span class="rl">clips</span>
      <span class="rc">none</span>`)
}

// TestSetupPrintsNoSecret is C5 and §15 on this page: the endpoint host, the
// bucket name, the key prefix and the credentials this instance holds reach
// nobody through it, whichever walk is open.
func TestSetupPrintsNoSecret(t *testing.T) {
	f := newFixture(t)
	base, _ := url.Parse(testBase)
	h, err := New(Deps{
		DB: f.db, Auth: session(f.owner), URLs: urls.New(base, "b1"), BuildID: "b1",
		Cfg: config.Config{SSRTimeout: f.ssrTimeout, SessionCookie: "clippet_session",
			StorageBackend: config.BackendS3, StorageDir: "/media",
			S3: config.S3Config{Endpoint: "http://secret-host.example:9000", Bucket: "secret-bucket",
				AccessKey: "secret-key-id", SecretKey: "clippet-dev-secret", KeyPrefix: "secret-prefix/"}},
		Now: func() time.Time { return time.Unix(f.now, 0) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, id := range []string{"", "fs", "r2", "b2", "s3", "other"} {
		body := get(h, "/setup?store="+id).Body.String()
		mustNotContain(t, body, "secret-host.example", "secret-bucket", "secret-key-id",
			"clippet-dev-secret", "secret-prefix/")
	}
}

// TestSetupPerTenGB pins the one arithmetic row: DECIMAL gigabytes (the unit
// both free tiers are quoted in, and about 7 % less flattering than the binary
// reading), and absent below twenty clips rather than a guess.
func TestSetupPerTenGB(t *testing.T) {
	if got := perTenGB(19, 19*11*1024*1024); got != "" {
		t.Errorf("nineteen clips printed %q, want nothing", got)
	}
	if got := perTenGB(0, 0); got != "" {
		t.Errorf("an empty instance printed %q", got)
	}
	// 11.4 MiB a clip: 10^10 / (11.4 * 2^20) = 836.
	const avg = int64(11953766) // 11.4 MiB a clip
	got := perTenGB(100, avg*100)
	if !strings.HasPrefix(got, "about 836 clips") {
		t.Errorf("perTenGB = %q, want it to start 'about 836 clips'", got)
	}
}

// ---- helpers -------------------------------------------------------------

// parseEnvBlock reads a generated block into a map, ignoring comments and
// blank lines, and fails on a duplicate assignment.
func parseEnvBlock(t *testing.T, block string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("block line is not an assignment: %q", line)
		}
		if _, dup := out[k]; dup {
			t.Fatalf("block assigns %s twice", k)
		}
		out[k] = v
	}
	return out
}

// countAssignments counts `NAME=` at the start of a line.
func countAssignments(s, name string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), name+"=") {
			n++
		}
	}
	return n
}

// fillPlaceholder turns one of the page's placeholders into a value of the
// shape it describes. It lives in the TEST, never in the page: the page prints
// a placeholder precisely because it has no business inventing a value.
func fillPlaceholder(t *testing.T, name, v string) string {
	t.Helper()
	if !strings.Contains(v, "<") {
		return v
	}
	switch name {
	case "S3_ENDPOINT":
		return "https://bucket.example.test"
	case "S3_REGION":
		return "us-east-1"
	case "S3_BUCKET":
		return "clips"
	case "S3_ACCESS_KEY":
		return "AKIAEXAMPLE"
	case "S3_SECRET_KEY":
		return "s3cret-value-long-enough"
	case "S3_KEY_PREFIX":
		return "clippet/"
	case "S3_FORCE_PATH_STYLE":
		return "true"
	case "STORAGE_PRICE_PER_GB_MONTH", "EGRESS_PRICE_PER_GB":
		return "0.015"
	case "PUBLIC_BASE_URL":
		// The Pass block's own line, with the `<name>` filled in. It has to be
		// an https base, which is what makes the block's
		// SESSION_COOKIE_NAME=__Host-… loadable at all: the loader refuses
		// that prefix on a plain-http origin (§20 erratum 28's prefix rule),
		// so the two lines only work together and this is where that is
		// proven.
		return "https://northlight.clippet.gg"
	}
	t.Fatalf("no test value for the placeholder in %s=%s", name, v)
	return ""
}

// emptyFixture is a migrated database with no users and no clips: an instance
// nothing has happened on yet, which is the state the first-run door exists
// for and the state with nothing for the STOP row to warn about.
func emptyFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, now: 1_700_000_000, registration: config.RegistrationOpen, ssrTimeout: 5 * time.Second}
	path := filepath.Join(t.TempDir(), "clippet.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	f.db, f.path = d, path
	return f
}

// ---- the Clippet Pass walk (§20 erratum 303) -----------------------------

// TestSetupPassWalkIsTheCloudflareWalkPlusThreeLines pins the third exposure:
// the store's own Cloudflare block with `compose.pass.yaml` on the file list
// and the three lines a name needs, still exactly one COMPOSE_FILE, and the
// closing line that says what the Pass reads from this instance.
func TestSetupPassWalkIsTheCloudflareWalkPlusThreeLines(t *testing.T) {
	f := newFixture(t)
	h := setupHandler(t, f, config.BackendS3, "", session(f.owner))

	// The option is on the chooser at all.
	mustContain(t, get(h, "/setup").Body.String(),
		`value="`+exposurePass+`"`, ">via a Clippet Pass<")

	for _, id := range []string{"r2", "b2", "s3", "other"} {
		rec := get(h, "/setup?store="+id+"&via="+exposurePass)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s on a Pass name = %d", id, rec.Code)
		}
		body := rec.Body.String()
		assertNoInline(t, "setup "+id+" (pass)", body)
		// Law 2 at runtime: ONE file list, one backend, one delivery mode. The
		// COMPOSE_FILE line is the target's own with the Pass file appended —
		// a second line would be a list somebody has to merge by hand, and
		// the last one would silently win.
		if n := countAssignments(body, "COMPOSE_FILE"); n != 1 {
			t.Errorf("%s (pass): the page renders %d COMPOSE_FILE lines", id, n)
		}
		for _, name := range []string{"STORAGE_BACKEND", "MEDIA_DELIVERY", "PUBLIC_BASE_URL", "SESSION_COOKIE_NAME", "CLIPPET_PASS_TOKEN", "TRUSTED_PROXIES"} {
			if n := countAssignments(body, name); n != 1 {
				t.Errorf("%s (pass): the page renders %d %s lines", id, n, name)
			}
		}
		mustContain(t, body,
			"COMPOSE_FILE=compose.yaml:compose.nobucket.yaml:"+passComposeFile,
			"MEDIA_DELIVERY="+config.DeliveryRedirect,
			"SESSION_COOKIE_NAME="+passSessionCookie,
			"CLIPPET_PASS_TOKEN=",
			"TRUSTED_PROXIES=172.16.0.0/12",
			"on a Clippet Pass name",
		)
		// A printed LINE, never a field: the wizard has no input to type a
		// credential into and this changes nothing about that
		// (the Pass setup page's design, §0.1).
		mustNotContain(t, body, `name="CLIPPET_PASS_TOKEN"`, `id="CLIPPET_PASS_TOKEN"`, "data-setup-token")
		// The closing line, and no refusal: a bucket on a Pass name walks.
		mustContain(t, body, "Set CLIPPET_COMMIT beside CLIPPET_VERSION (DEPLOY.md §16), or the portal calls this Clippet behind.")
		mustNotContain(t, body, "data-setup-refusal")
		// The one absolute URL this page may now carry is the Pass base of the
		// block itself (law 1's exception); its own origin aside, there is no
		// other.
		stripped := strings.ReplaceAll(body, testBase, "")
		stripped = strings.ReplaceAll(stripped, passScheme+"://&lt;name&gt;.clippet.gg", "")
		stripped = strings.ReplaceAll(stripped, passScheme+"://<name>.clippet.gg", "")
		if setupURLRE.MatchString(stripped) {
			t.Errorf("%s (pass): the page carries a URL beyond this origin and the Pass base: %q", id,
				setupURLRE.FindString(stripped))
		}
	}

	// The one walk with a Cloudflare note of its own keeps it, ABOVE the
	// Pass's: the target's line is about this block and the Pass's is about
	// every Pass block.
	body := get(h, "/setup?store=other&via="+exposurePass).Body.String()
	mustContain(t, body, "Behind Cloudflare the endpoint must be public https: a LAN address or a NAS will not work.")
	if i, j := strings.Index(body, "a LAN address or a NAS will not work"), strings.Index(body, html.EscapeString(passNote)); i < 0 || j < 0 || i > j {
		t.Errorf("the target's closing line must render above the Pass's (at %d and %d)", i, j)
	}
}

// TestSetupPassFolderWalks is the same verdict on a Clippet Pass name (§20
// erratum 348 (b)): a Pass name IS a Cloudflare tunnel, so the folder walks
// there too — the folder block with `compose.pass.yaml` on its file list, the
// release and the name's lines, and the risk line ABOVE the Pass's own — and
// the select still overrides the header in both directions.
func TestSetupPassFolderWalks(t *testing.T) {
	f := newFixture(t)
	h := setupHandler(t, f, config.BackendS3, "", session(f.owner))
	body := get(h, "/setup?store=fs&via="+exposurePass).Body.String()
	mustContain(t, body, `data-setup-walk="fs"`, "COMPOSE_FILE=compose.yaml:compose.fs.yaml:"+passComposeFile,
		"MEDIA_DELIVERY=proxy", "CLIPPET_PASS_TOKEN=", html.EscapeString(localTunnelNote))
	mustNotContain(t, body, "data-setup-refusal", "rs danger", "cannot be served through")
	if i, j := strings.Index(body, "Their terms reserve video"), strings.Index(body, html.EscapeString(passNote)); i < 0 || j < 0 || i > j {
		t.Errorf("the folder's risk line must render above the Pass's (at %d and %d)", i, j)
	}
	// …and `via=pass` wins over a CF-Ray, like every other value of the select.
	rec := do(h, http.MethodGet, "/setup?store=r2&via="+exposurePass, "CF-Ray", "8f0c1d2e3f4a5b6c-LHR")
	mustContain(t, rec.Body.String(), passComposeFile)
	// A bare CF-Ray is still the plain Cloudflare answer and NOT a Pass name:
	// the Pass is a thing somebody bought, never something detected.
	mustNotContain(t, do(h, http.MethodGet, "/setup?store=r2", "CF-Ray", "8f0c1d2e3f4a5b6c-LHR").Body.String(),
		passComposeFile)
}

// TestSetupPassBlockOpensWithTheRelease is §20 erratum 348 (e): every Pass
// block's first lines under its name say which tree to build and assign
// CLIPPET_COMMIT, before anything else — a box built from an arbitrary
// checkout is one the portal calls behind for ever.
func TestSetupPassBlockOpensWithTheRelease(t *testing.T) {
	for _, o := range setupOptions() {
		block, _ := setupBlockFor(o, exposurePass)
		lines := strings.SplitAfterN(block, "\n", 2)
		if len(lines) != 2 || !strings.HasPrefix(lines[1], passRelease) {
			t.Errorf("%s (pass): the block does not open with the release:\n%s", o.ID, block)
		}
		if !strings.Contains(passRelease, "check out the latest vYYYY.MM.DD tag and set CLIPPET_COMMIT to its") {
			t.Fatalf("the release line lost its words: %q", passRelease)
		}
		if n := countAssignments(block, "CLIPPET_COMMIT"); n != 1 {
			t.Errorf("%s (pass): CLIPPET_COMMIT assigned %d times", o.ID, n)
		}
		for _, other := range []string{exposureOther, exposureCloudflare} {
			if b, _ := setupBlockFor(o, other); strings.Contains(b, "CLIPPET_COMMIT") {
				t.Errorf("%s (%s): a non-Pass block names CLIPPET_COMMIT", o.ID, other)
			}
		}
	}
}

// TestSetupStepsView is `?view=steps&store=<id>` (§20 erratum 335): the
// option's head, its caveat as the head hint, the numbered steps with their
// dated hints and `labels read`, and NOTHING else — no chooser, no Now, no
// STOP, no refusal, no block, no command, no Check now. The gate is the
// page's own, and without a valid store it is the ordinary page.
func TestSetupStepsView(t *testing.T) {
	f := newFixture(t)
	// A bucket instance holding clips and reached through Cloudflare: the
	// ordinary page would print a STOP for `fs` and refuse `fs` outright — the
	// steps view does neither.
	h := setupHandler(t, f, config.BackendS3, "", session(f.owner))
	for _, o := range setupOptions() {
		rec := do(h, http.MethodGet, "/setup?view=steps&store="+o.ID, "CF-Ray", "8f0c1d2e3f4a5b6c-LHR")
		assertPageHeaders(t, rec, http.StatusOK, "private, no-store")
		body := rec.Body.String()
		assertNoInline(t, "setup", body)
		mustContain(t, body, `data-setup-steps="`+o.ID+`"`, `<ol class="walk">`, "labels read "+walksRead)
		head := `<h2>` + o.Head + `</h2>`
		if o.Caveat != "" {
			head += `<span class="hint">` + htmlText(o.Caveat) + `</span>`
		}
		mustContain(t, body, head)
		if n := strings.Count(body, "<li>"); n < len(o.Steps) {
			t.Errorf("%s: %d steps rendered, want %d", o.ID, n, len(o.Steps))
		}
		for _, st := range o.Steps {
			mustContain(t, body, htmlText(st.Text))
			if st.Hint != "" {
				mustContain(t, body, `<span class="hint">`+htmlText(st.Hint)+`</span>`)
			}
		}
		mustNotContain(t, body, "pre class=\"env", "data-setup-cmd", "data-setup-stop", `name="store"`,
			"data-setup-refusal", "data-setup-check", "data-setup-form", "data-setup-block", "data-setup ",
			"Where do clips live?", "<h2>Now</h2>", `class="n"`, "Apply", "docker compose", "S3_SECRET_KEY",
			"a bucket (s3)", "a directory (fs)", "cannot be served through")
		if setupURLRE.MatchString(strings.ReplaceAll(body, testBase, "")) {
			t.Errorf("%s: the steps view carries a URL", o.ID)
		}
	}
	// No valid store: the ordinary page.
	for _, q := range []string{"/setup?view=steps", "/setup?view=steps&store=zzz"} {
		body := get(h, q).Body.String()
		mustContain(t, body, "Where do clips live?", `name="store"`)
		mustNotContain(t, body, "data-setup-steps")
	}
	// The gate is the page's own: anonymous on a configured instance goes to
	// log in; anonymous on an instance with no users is let in.
	if rec := get(setupHandler(t, f, config.BackendS3, "", passAuth{}), "/setup?view=steps&store=b2"); rec.Code != http.StatusSeeOther {
		t.Fatalf("anonymous steps view on a configured instance = %d, want 303", rec.Code)
	}
	if rec := get(setupHandler(t, emptyFixture(t), config.BackendS3, "", passAuth{}), "/setup?view=steps&store=b2"); rec.Code != http.StatusOK {
		t.Fatalf("anonymous steps view on an unconfigured instance = %d, want 200", rec.Code)
	}
}

// htmlText is s as html/template writes it into an element's text.
func htmlText(s string) string {
	return strings.ReplaceAll(html.EscapeString(s), "+", "&#43;")
}

// TestSetupIsTheOwnersPage: `/setup` belongs to the instance's owner — the
// first account (§20 erratum 349). Another signed-in account gets 403, the
// heading and the one sentence, and nothing the page reads about the instance:
// no backend, no chooser, no Check now, no clip count; the steps-only view is
// the same door. The owner's page is unchanged.
func TestSetupIsTheOwnersPage(t *testing.T) {
	f := newFixture(t) // carol first (the owner), then friend
	h := setupHandler(t, f, config.BackendS3, "", session(f.other))
	for _, path := range []string{"/setup", "/setup?store=b2&via=other", "/setup?view=steps&store=b2"} {
		rec := get(h, path)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("a member's GET %s = %d, want 403", path, rec.Code)
		}
		body := rec.Body.String()
		mustContain(t, body, "Where do clips live?", "data-setup-owner-only",
			"Only the account that set up this Clippet can see this.")
		mustNotContain(t, body, "data-setup-backend", "data-setup-check", "Pick a storage backend",
			"data-setup-steps", "labels read", "<h2>Now</h2>")
		if cc := rec.Header().Get("Cache-Control"); cc != "private, no-store" {
			t.Errorf("a member's %s Cache-Control %q", path, cc)
		}
	}
	rec := get(setupHandler(t, f, config.BackendS3, "", session(f.owner)), "/setup")
	if rec.Code != http.StatusOK {
		t.Fatalf("the owner = %d, want 200", rec.Code)
	}
	mustContain(t, rec.Body.String(), "data-setup-backend", "Pick a storage backend", "<h2>Now</h2>")
	mustNotContain(t, rec.Body.String(), "data-setup-owner-only", "Only the account that set up")
}

// TestSetupWizardOff: with SETUP_WIZARD=off (§20 erratum 335 (d); the
// owner's call, 2026-11-02 — the desktop app sets it, because the app does the setup) the
// wizard answers the ordinary 404 — anonymously before the first account, and
// to the owner after it — while the steps-only view the app opens stays
// exactly as it is, behind the same gate; /settings' Storage head drops its
// link to it. The default keeps the wizard and the link.
func TestSetupWizardOff(t *testing.T) {
	notFound := func(t *testing.T, h http.Handler, path, who string) {
		t.Helper()
		rec := get(h, path)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("off, %s, GET %s = %d, want 404", who, path, rec.Code)
		}
		mustContain(t, rec.Body.String(), "Not found")
		mustNotContain(t, rec.Body.String(), "Where do clips live?", "data-setup", "labels read")
	}
	wizard := []string{"/setup", "/setup?store=b2", "/setup?store=b2&via=other",
		"/setup?view=steps", "/setup?view=steps&store=zzz"}

	empty := emptyFixture(t)
	empty.setupWizardOff = true
	anon := setupHandler(t, empty, config.BackendS3, "", passAuth{})
	for _, path := range wizard {
		notFound(t, anon, path, "anonymous with no users")
	}
	rec := get(anon, "/setup?view=steps&store=r2")
	if rec.Code != http.StatusOK {
		t.Fatalf("off, anonymous with no users, steps view = %d, want 200", rec.Code)
	}
	mustContain(t, rec.Body.String(), `data-setup-steps="r2"`)

	f, _ := countingFixture(t) // counting: /settings draws its Storage panel
	f.setupWizardOff = true
	owner := setupHandler(t, f, config.BackendS3, "", session(f.owner))
	for _, path := range wizard {
		notFound(t, owner, path, "the owner")
	}
	rec = get(owner, "/setup?view=steps&store=r2")
	if rec.Code != http.StatusOK {
		t.Fatalf("off, the owner, steps view = %d, want 200", rec.Code)
	}
	mustContain(t, rec.Body.String(), `data-setup-steps="r2"`)
	// The steps view's gate is unchanged: anonymous once there is an account
	// goes to log in, and a member gets the owner-only 403.
	if rec := get(setupHandler(t, f, config.BackendS3, "", passAuth{}), "/setup?view=steps&store=r2"); rec.Code != http.StatusSeeOther {
		t.Fatalf("off, anonymous with users, steps view = %d, want 303", rec.Code)
	}
	if rec := get(setupHandler(t, f, config.BackendS3, "", session(f.other)), "/setup?view=steps&store=r2"); rec.Code != http.StatusForbidden {
		t.Fatalf("off, a member, steps view = %d, want 403", rec.Code)
	}
	// /settings' Storage head links to the wizard only when it is on.
	link := `<a href="/setup">where clips live</a>`
	f.budget = &httpx.StorageBudget{}
	f.budget.Set(25_585_254, 190_000_000)
	body := get(f.handler(session(f.owner)), "/settings").Body.String()
	mustContain(t, body, `<h2>Storage</h2>`)
	mustNotContain(t, body, link, `href="/setup"`)

	// The default: the wizard as before, for both doors, and the link.
	g, _ := countingFixture(t)
	if rec := get(setupHandler(t, g, config.BackendS3, "", session(g.owner)), "/setup"); rec.Code != http.StatusOK {
		t.Fatalf("on, the owner = %d, want 200", rec.Code)
	} else {
		mustContain(t, rec.Body.String(), "Where do clips live?", "Pick a storage backend")
	}
	if rec := get(setupHandler(t, emptyFixture(t), config.BackendS3, "", passAuth{}), "/setup"); rec.Code != http.StatusOK {
		t.Fatalf("on, anonymous with no users = %d, want 200", rec.Code)
	}
	g.budget = &httpx.StorageBudget{}
	g.budget.Set(25_585_254, 190_000_000)
	mustContain(t, get(g.handler(session(g.owner)), "/settings").Body.String(), `<h2>Storage</h2>`, link)
}
