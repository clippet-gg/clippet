package main

// doctor.go — `clippet admin doctor` (ROADMAP #9, DESIGN.md §20 erratum 143).
//
// One preflight that answers "will this configuration boot, and will it work
// once it has?" BEFORE `docker compose up -d`, from a throwaway container:
//
//	docker compose run --rm --no-deps app admin doctor
//
// (that form is the one DEPLOY §24.1 recommends: compose resolves the same
// `${VAR:-default}` fallbacks the real container gets, and the container joins
// the compose network so a service-name S3_ENDPOINT resolves)
//
// Every ingredient existed and none was reachable as a check: configuration
// was validated only by booting, ffmpeg was resolved only inside `serve`,
// storage reachability was only `/readyz` (which needs a running app), free
// disk was only inside the upload path, and the resolved configuration could
// only be inspected as `docker compose config`, which prints S3_SECRET_KEY in
// the clear.
//
// Three rules the implementation keeps:
//
//   - It runs BEFORE config.Load can kill the process, and reports EVERY
//     invalid variable (config.Errors, erratum 104) rather than the first.
//     A doctor that dies on the first typo is a doctor you run four times.
//   - It is READ-ONLY except for one probe object it puts in the bucket and
//     immediately removes, and one temp file it writes and removes under each
//     of DATA_DIR and SCRATCH_DIR — and, on STORAGE_BACKEND=fs, under
//     STORAGE_DIR instead of the bucket (§20 erratum 276). It never creates
//     the database, never migrates, never creates a missing STORAGE_DIR and
//     never writes a clip key.
//   - It prints no secret. No check formats a credential — that is an
//     invariant the tests assert — and every error text is passed through
//     safe() first, because a storage library's error can quote the credential
//     it signed with. The Discord webhook is rendered by
//     config.DiscordWebhook.String(), which is host + numeric id and never the
//     token.
//
// Output: one line per fact, `<check> <verdict> <detail>`, so an operator can
// `grep -v ' ok '`. Verdicts are `ok`, `warn`, `FAIL` and `skip`. Exit 0 when
// nothing failed (warnings do not fail), 1 when anything did, 2 for usage.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"clippet/internal/config"
	"clippet/internal/cost"
	"clippet/internal/db"
	"clippet/internal/drop"
	"clippet/internal/httpapi"
	"clippet/internal/notify"
	"clippet/internal/store"
)

// doctorTimeout bounds the whole preflight. The bucket probe is the only part
// that can hang, and it is already bounded by STORAGE_OP_TIMEOUT.
const doctorTimeout = 2 * time.Minute

// Verdicts, in the order of severity that decides the exit code.
const (
	vOK   = "ok"
	vWarn = "warn"
	vFail = "FAIL"
	vSkip = "skip"
)

// doctorRun accumulates the report.
type doctorRun struct {
	env    adminEnv
	ok     int
	warn   int
	fail   int
	skip   int
	secret []string // strings that must never reach the output
}

// say prints one fact. It does NOT redact: no check below formats a credential
// (that is an invariant the tests assert), and blanket-redacting every line
// would corrupt honest output the moment a secret happened to be a common word
// — the published development access key is literally `clippet`, which appears in
// `/data/clippet.db`. Text that came from OUTSIDE this package goes through
// safe() at its own call site instead.
func (r *doctorRun) say(check, verdict, format string, args ...any) {
	fmt.Fprintf(r.env.stdout, "%-8s %-4s %s\n", check, verdict, fmt.Sprintf(format, args...))
}

// count records a check's verdict (one per check, however many lines it
// printed).
func (r *doctorRun) count(verdict string) {
	switch verdict {
	case vOK:
		r.ok++
	case vWarn:
		r.warn++
	case vFail:
		r.fail++
	default:
		r.skip++
	}
}

// safe renders an error for the report with every configured SECRET replaced
// by a marker. It is applied to error text and to nothing else, because error
// text is the only thing here that this package did not write: a storage
// library signs a request with the credential and can quote it back inside a
// 403, and the loader's own messages are formatted from values an operator
// supplied.
//
// The set is the SECRETS, not the identifiers: `S3_ACCESS_KEY` is deliberately
// absent, because it is a key id rather than a credential and because the
// shipped one is the word `clippet`, which would otherwise be struck out of
// half the paths in the report. A secret shorter than minSecretLen is skipped
// for the same reason — it is not a plausible credential, and matching it
// would corrupt more than it protects.
func (r *doctorRun) safe(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	for _, sec := range r.secret {
		if len(sec) < minSecretLen {
			continue
		}
		s = strings.ReplaceAll(s, sec, "(redacted)")
	}
	return s
}

// minSecretLen is the shortest string the redaction will match. Anything
// shorter is not a credential worth protecting and is very likely a substring
// of something else in the report.
const minSecretLen = 8

// routeSecrets pulls every candidate secret out of a RAW DISCORD_WEBHOOK_ROUTES
// value, without parsing it — the point is to have the set ready before the
// loader has said whether the value is even well-formed.
//
// BOTH halves of each rule go in, not just the URL half. config.go's own
// warning is that the dangerous typo is `<url>=<tag>` rather than
// `<tag>=<url>`, which puts the live webhook in the TAG half — the half a
// URL-only sweep would skip. minSecretLen keeps short tags out of the
// substitution, and a long tag redacted out of doctor's output costs nothing
// next to a webhook token printed in full.
func routeSecrets(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := []string{raw}
	for _, rule := range strings.Split(raw, ";") {
		rule = strings.TrimSpace(rule)
		if rule == "" {
			continue
		}
		out = append(out, rule)
		if left, right, ok := strings.Cut(rule, "="); ok {
			out = append(out, strings.TrimSpace(left), strings.TrimSpace(right))
		}
	}
	return out
}

// runDoctor is the subcommand. getenv is the process environment (main passes
// its own so tests can drive a whole configuration through a map).
func runDoctor(args []string, getenv func(string) string, env adminEnv) int {
	fs := flag.NewFlagSet("clippet admin doctor", flag.ContinueOnError)
	fs.SetOutput(env.stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(env.stderr, "usage: clippet admin doctor        (reads the environment; writes nothing but one bucket probe)")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
	defer cancel()
	r := &doctorRun{env: env}

	cfg, cfgErr := config.Load(getenv)
	// The credentials are registered as secrets even when the load failed:
	// a partially valid configuration still carries the ones that parsed.
	r.secret = append(r.secret, getenv("S3_SECRET_KEY"),
		getenv("MINIO_ROOT_PASSWORD"), getenv("DISCORD_WEBHOOK_URL"),
		// The bot token is the second secret of the Discord feature (ROADMAP
		// #33, §20 erratum 254). Registered from the RAW environment for the
		// reason the webhook is: a configuration that failed to load still
		// carries the values that parsed.
		getenv(config.EnvDiscordBotToken),
		// …and the login's client secret is the third of the same class
		// (ROADMAP #44, §20 erratum 302), registered the same way.
		getenv(config.EnvDiscordLoginClientSecret),
		// The clip webhook's two values (§20 erratum 319): the URL is a
		// credential and the key is one, registered raw for the same reason.
		getenv(config.EnvClipWebhookURL), getenv(config.EnvClipWebhookSecret),
		// Clippet Auto's token (§20 erratum 333), the same class, the same way.
		getenv(config.EnvDiscordAutoToken))
	// DISCORD_WEBHOOK_ROUTES carries a webhook per rule, and each of them is
	// exactly as secret as the default one. The parsed loop below registers
	// them — but it sits AFTER the cfgErr early return, which is precisely the
	// moment an unvetted raw value is closest to stderr. Nothing leaks today
	// (config.go's route rejections name a rule POSITION and never echo a
	// value), so this is defence in depth against the next rejection message
	// somebody writes (N111).
	r.secret = append(r.secret, routeSecrets(getenv("DISCORD_WEBHOOK_ROUTES"))...)

	if cfgErr != nil {
		errs := configErrors(cfgErr)
		for _, e := range errs {
			r.say("config", vFail, "%s", r.safe(e))
		}
		r.say("config", vFail, "%d variable(s) invalid; nothing below could be checked", len(errs))
		r.count(vFail)
		for _, check := range doctorChecks[1:] {
			r.say(check, vSkip, "the configuration did not parse")
			r.count(vSkip)
		}
		return r.finish()
	}
	r.secret = append(r.secret, cfg.S3.SecretKey, cfg.DiscordWebhook.URL(), cfg.DiscordBot.Token(), cfg.DiscordLogin.Secret())
	// Every per-tag route carries a webhook URL of its own (ROADMAP #26, §20
	// erratum 158), and each of them is exactly as secret as the default one.
	// The TAG is not secret and is deliberately left alone: it is what a log
	// line and a job's params_json name a route by.
	for _, rt := range cfg.DiscordRoutes {
		r.secret = append(r.secret, rt.Webhook.URL())
	}
	r.say("config", vOK, "every variable parsed (listen=%s registration=%s log=%s/%s)",
		cfg.Listen, cfg.Registration, cfg.LogLevel, cfg.LogFormat)
	r.count(vOK)

	r.count(r.checkBaseURL(cfg))
	r.count(r.checkPaths(cfg))
	r.count(r.checkDB(ctx, cfg))
	r.count(r.checkFFmpeg(cfg))
	r.count(r.checkCredentials(cfg))
	r.count(r.checkStore(ctx, cfg, getenv))
	r.count(r.checkBucket(ctx, cfg))
	r.count(r.checkMedia(ctx, cfg, getenv))
	r.count(r.checkDrop(ctx, cfg))
	r.count(r.checkProxies(cfg, getenv))
	r.count(r.checkWebhook(cfg))
	r.count(r.checkBot(ctx, cfg))
	r.count(r.checkAuto(ctx, cfg))
	r.count(r.checkLogin(cfg))
	r.count(r.checkRepair(cfg))
	r.count(r.checkBackup(cfg))
	r.count(r.checkSnapshot(ctx, cfg))
	r.count(r.checkPrices(cfg))
	return r.finish()
}

func (r *doctorRun) finish() int {
	total := r.ok + r.warn + r.fail + r.skip
	fmt.Fprintf(r.env.stdout, "doctor:  %d checks, %d ok, %d warn, %d fail, %d skipped\n",
		total, r.ok, r.warn, r.fail, r.skip)
	if r.fail > 0 {
		return 1
	}
	return 0
}

// --- the checks -------------------------------------------------------------

// checkBaseURL: PUBLIC_BASE_URL parsed as a bare origin at load time, so what
// is left is the DEPLOY §3/§5 sanity an operator gets wrong — a plain-http
// PUBLIC origin (X renders no player card and the cookie loses Secure), and a
// loopback origin (nothing on the internet can fetch the card's URLs).
func (r *doctorRun) checkBaseURL(cfg config.Config) string {
	u := cfg.PublicBaseURL
	switch {
	case u.Scheme == "https":
		r.say("baseurl", vOK, "%s (https: player cards work on X and the session cookie is Secure)", u)
		return vOK
	case isLoopbackHost(u.Hostname()):
		r.say("baseurl", vOK, "%s (loopback dev origin: no crawler can reach it, so no card unfurls - DEPLOY.md §3)", u)
		return vOK
	default:
		r.say("baseurl", vWarn, "%s is a PUBLIC origin on plain http: X renders no player card, og:video:secure_url is not emitted and the session cookie is not Secure (DEPLOY.md §5)", u)
		return vWarn
	}
}

// dataDirMinFloor is the smallest advisory floor checkPaths will hold DATA_DIR
// to: enough room for a `VACUUM INTO` snapshot and a WAL checkpoint of any
// database this design targets, with nothing else on the device to argue with.
const dataDirMinFloor = 64 << 20

// dataDirRoom is the DATA_DIR half of the free-space check (N034).
//
// SCRATCH_MIN_FREE_BYTES governs INGEST and nothing else, so the scratch
// verdict cannot speak for the database. On the shipped compose that gap is
// invisible — both directories are one Docker filesystem, `admin stats` prints
// same_device=true, and the scratch floor therefore guards the database too —
// which is why this only has anything to say when the two are on DIFFERENT
// devices. There, a database with no room to write is just as fatal as a full
// scratch disk and had no threshold of its own.
//
// The floor is derived rather than configured: twice the database file, never
// under dataDirMinFloor. Twice, because SQLite's own worst case here is a
// snapshot beside the live file; derived, because a DATA_MIN_FREE_BYTES
// variable is a config change and this is a warning, not a refusal.
//
// It is a WARN and never a FAIL: doctor's exit code must not turn on a
// heuristic, and an operator running it in a pre-deploy script has to be able
// to trust that a non-zero exit means something is actually broken.
func dataDirRoom(free, dbSize int64, sameDevice bool) (floor int64, low bool) {
	if sameDevice {
		return 0, false
	}
	floor = 2 * dbSize
	if floor < dataDirMinFloor {
		floor = dataDirMinFloor
	}
	return floor, free < floor
}

// checkPaths: DATA_DIR and SCRATCH_DIR exist and are writable, and each
// filesystem has room — SCRATCH_MIN_FREE_BYTES on the scratch device (the
// whole cause of the documented `507 insufficient_scratch`), and a derived
// advisory floor on DATA_DIR when it is on a device of its own.
func (r *doctorRun) checkPaths(cfg config.Config) string {
	verdict := vOK
	worse := func(v string) {
		if v == vFail || (v == vWarn && verdict == vOK) {
			verdict = v
		}
	}
	// One stat each, taken before the loop so the DATA_DIR branch does not
	// repeat them. An error either way means "cannot tell", and the check
	// stays quiet: a warning nobody can act on is worse than no warning.
	sameDev, sameErr := sameDevice(cfg.DataDir, cfg.ScratchDir)
	var dbSize int64
	if fi, err := os.Stat(dbPath(cfg)); err == nil {
		dbSize = fi.Size()
	}
	for _, p := range []struct{ name, dir string }{{"DATA_DIR", cfg.DataDir}, {"SCRATCH_DIR", cfg.ScratchDir}} {
		fi, err := os.Stat(p.dir)
		switch {
		case errors.Is(err, os.ErrNotExist):
			r.say("paths", vWarn, "%s=%s does not exist yet (`clippet serve` creates it at boot)", p.name, p.dir)
			worse(vWarn)
			continue
		case err != nil:
			r.say("paths", vFail, "%s=%s: %s", p.name, p.dir, r.safe(err))
			worse(vFail)
			continue
		case !fi.IsDir():
			r.say("paths", vFail, "%s=%s is not a directory", p.name, p.dir)
			worse(vFail)
			continue
		}
		if err := writable(p.dir); err != nil {
			r.say("paths", vFail, "%s=%s is not writable by this process: %s", p.name, p.dir, r.safe(err))
			worse(vFail)
			continue
		}
		free, total, err := diskUsage(p.dir)
		if err != nil {
			r.say("paths", vOK, "%s=%s writable (free space unavailable: %s)", p.name, p.dir, r.safe(err))
			continue
		}
		detail := fmt.Sprintf("%s=%s writable, free=%d (%s) size=%d (%s)", p.name, p.dir, free, humanBytes(free), total, humanBytes(total))
		if p.dir == cfg.ScratchDir && free < cfg.ScratchMinFree {
			r.say("paths", vFail, "%s - below SCRATCH_MIN_FREE_BYTES=%d (%s): every upload would answer 507 insufficient_scratch",
				detail, cfg.ScratchMinFree, humanBytes(cfg.ScratchMinFree))
			worse(vFail)
			continue
		}
		if p.name == "DATA_DIR" && sameErr == nil {
			if floor, low := dataDirRoom(free, dbSize, sameDev); low {
				r.say("paths", vWarn, "%s - under %d (%s) on a device SCRATCH_DIR does not share, so SCRATCH_MIN_FREE_BYTES does not guard it: the database has little room to write (floor is twice the .db, never under %s)",
					detail, floor, humanBytes(floor), humanBytes(dataDirMinFloor))
				worse(vWarn)
				continue
			}
		}
		r.say("paths", vOK, "%s", detail)
	}
	return verdict
}

// writable proves a directory is writable by creating and removing one file
// in it. `os.Stat` says nothing about permissions for THIS uid, and the image
// runs as uid 10001 against volumes an operator may have chowned.
func writable(dir string) error {
	f, err := os.CreateTemp(dir, ".clippet-doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	closeErr := f.Close()
	if err := os.Remove(name); err != nil {
		return err
	}
	return closeErr
}

// checkDB opens the SQLite file if it EXISTS and reports its integrity and
// applied schema. A missing file is a warning, not a failure: running this
// before the first `up -d` is the point, and the file is created at boot.
// Nothing here creates or migrates it.
func (r *doctorRun) checkDB(ctx context.Context, cfg config.Config) string {
	path := dbPath(cfg)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		r.say("db", vWarn, "%s does not exist yet (`clippet serve` creates and migrates it at boot)", path)
		return vWarn
	} else if err != nil {
		r.say("db", vFail, "%s: %s", path, r.safe(err))
		return vFail
	}
	d, err := db.Open(path)
	if err != nil {
		r.say("db", vFail, "%s: open: %s", path, r.safe(err))
		return vFail
	}
	defer d.Close()
	integrity, err := d.IntegrityCheck(ctx)
	if err != nil {
		r.say("db", vFail, "%s: %s", path, r.safe(err))
		return vFail
	}
	versions, err := d.AppliedVersions(ctx)
	if err != nil {
		r.say("db", vFail, "%s: read schema_migrations: %s", path, r.safe(err))
		return vFail
	}
	if integrity != "ok" {
		r.say("db", vFail, "%s integrity=%q applied=%s - do NOT start on this file (DEPLOY.md §15)", path, integrity, joinInts(versions))
		return vFail
	}
	if len(versions) == 0 {
		r.say("db", vWarn, "%s integrity=ok but no migration is recorded (boot applies them)", path)
		return vWarn
	}
	r.say("db", vOK, "%s integrity=ok applied=%s", path, joinInts(versions))
	return vOK
}

// checkFFmpeg resolves both binaries the way `serve` does and reports the
// version /healthz would report. A missing binary is fatal at boot, so it is
// a FAIL here.
func (r *doctorRun) checkFFmpeg(cfg config.Config) string {
	ffmpeg, ffprobe, version, err := resolveFFmpeg(cfg.Transcode)
	if err != nil {
		r.say("ffmpeg", vFail, "%s", r.safe(err))
		return vFail
	}
	r.say("ffmpeg", vOK, "ffmpeg %s at %s, ffprobe at %s", version, ffmpeg, ffprobe)
	return vOK
}

// checkCredentials is the first of the two known footguns (ROADMAP #9): the
// development pair printed in this repository reaching a bucket it was never
// meant for. Until §20 erratum 345 compose handed that pair to the app whenever
// `S3_ACCESS_KEY=` was empty, because the bundled MinIO ran on it; compose no
// longer falls back to any key, so today the pair only arrives by being typed —
// and the test harness's own bucket server still uses it. The row is therefore
// about WHERE the pair points, never about which server answers there: a server
// on this machine or this network (the harness's, by compose service name) is
// the one place it belongs. It never prints a credential — only whether the
// resolved pair is the published one.
func (r *doctorRun) checkCredentials(cfg config.Config) string {
	if cfg.StorageBackend == config.BackendFS {
		// There is no credential to get wrong: the four bucket variables are a
		// boot error on `fs` (§20 erratum 276), so the footgun this check
		// exists for cannot be reached from here.
		r.say("creds", vOK, "STORAGE_BACKEND=fs: this instance holds no bucket credential at all (the four S3_* variables are refused at boot)")
		return vOK
	}
	shipped := cfg.S3.AccessKey == shippedAccessKey || cfg.S3.SecretKey == shippedSecretKey
	if !shipped {
		r.say("creds", vOK, "S3_ACCESS_KEY/S3_SECRET_KEY are operator-supplied (values are never printed)")
		return vOK
	}
	if !localEndpoint(cfg.S3.Endpoint) {
		// COPY? (§20 erratum 345: "is not the bundled MinIO - an empty
		// `S3_ACCESS_KEY=` line counts as unset and falls back to them" became
		// the clause below; compose falls back to no key any more)
		r.say("creds", vFail, "the bucket credentials are the ones PUBLISHED in this repository, but S3_ENDPOINT=%s is not a server on this machine or network (DEPLOY.md §7)", cfg.S3.Endpoint)
		return vFail
	}
	if cfg.PublicBaseURL != nil && !isLoopbackHost(cfg.PublicBaseURL.Hostname()) {
		// COPY? (§20 erratum 345: "the bundled MinIO credentials" became "the
		// bucket credentials", as serve's boot warning words it, and the
		// MINIO_ROOT_* alternative went with that server)
		r.say("creds", vWarn, "the bucket credentials are the ones published in this repository and PUBLIC_BASE_URL is not loopback - set S3_ACCESS_KEY/S3_SECRET_KEY and recreate (DEPLOY.md §7)")
		return vWarn
	}
	// COPY? (§20 erratum 345: "the bundled MinIO dev credentials on a loopback
	// origin" became the line below)
	r.say("creds", vOK, "the development credentials published in this repository, on a loopback origin (fine here, nowhere else - DEPLOY.md §7)")
	return vOK
}

// localEndpoint reports whether the bucket endpoint is a server on this
// machine or this network — a loopback or private address, or a single-label
// name such as a compose service — by the same heuristic the media row uses
// (isPrivateHost). It replaced a test for the bundled MinIO's own service name
// when §20 erratum 345 withdrew that server, so the harness's bucket server is
// recognised whatever its service is called.
func localEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && isPrivateHost(u.Hostname())
}

// checkBucket is reachability AND writability: `/readyz` only ever proved a
// BucketExists, and a read-only key answers that call happily and then fails
// the first upload. The probe object lives under `{S3_KEY_PREFIX}doctor/`,
// never under `clips/`, so nothing that walks the clip prefixes (the sweeper,
// `admin orphans`) can ever see it, and it is removed before the check returns.
func (r *doctorRun) checkBucket(ctx context.Context, cfg config.Config) string {
	if cfg.StorageBackend == config.BackendFS {
		// One line and a skip, rather than a check that reports nothing: the
		// summary's counts are what an operator reads first, and a row that
		// silently vanished on one backend would make two runs incomparable
		// (§20 erratum 276). The directory is checked on the `store` row.
		r.say("bucket", vSkip, "STORAGE_BACKEND=fs: there is no bucket - the media directory is checked on the `store` row above")
		return vSkip
	}
	s3 := cfg.S3
	st, err := r.openStore(cfg)
	if err != nil {
		r.say("bucket", vFail, "cannot build the storage client: %s", r.safe(err))
		return vFail
	}
	// Ping, put, delete — `store.Probe` (§20 erratum 297). Those three calls
	// in that order are what this function performed inline until the setup
	// page needed the same proof; one implementation, two surfaces, and still
	// no read-back, so what the row below claims is only what was actually
	// done. Each sentence stays this row's own, chosen by the STEP the probe
	// failed at, and none of them changed — the two temp-file lines became
	// one, the probe having one way to fail at preparing a file now.
	if err := store.Probe(ctx, st, s3.KeyPrefix); err != nil {
		switch store.ProbeStep(err) {
		case store.ProbeStepPing:
			r.say("bucket", vFail, "%s at %s: %s: %s", s3.Bucket, s3.Endpoint, storeClass(err), r.safe(err))
			return vFail
		case store.ProbeStepPrepare:
			r.say("bucket", vWarn, "%s at %s is reachable; the write probe could not be prepared: %s", s3.Bucket, s3.Endpoint, r.safe(err))
			return vWarn
		case store.ProbeStepDelete:
			r.say("bucket", vWarn, "%s at %s is writable, but the probe object could not be removed (%s: %s) - delete it by hand",
				s3.Bucket, s3.Endpoint, storeClass(err), r.safe(err))
			return vWarn
		default:
			r.say("bucket", vFail, "%s at %s is reachable but NOT writable: %s: %s", s3.Bucket, s3.Endpoint, storeClass(err), r.safe(err))
			return vFail
		}
	}
	prefix := s3.KeyPrefix
	if prefix == "" {
		prefix = "(none)"
	}
	r.say("bucket", vOK, "%s at %s reachable, writable and cleanable (probe object put and removed; key prefix %s)",
		s3.Bucket, s3.Endpoint, prefix)
	return vOK
}

// openStore honours the adminEnv injection so tests drive a Fake.
func (r *doctorRun) openStore(cfg config.Config) (store.Store, error) {
	if r.env.openStore != nil {
		return r.env.openStore(cfg)
	}
	return store.Open(cfg)
}

// checkStore is the backend row (ROADMAP #37, §20 erratum 276). On `s3` it is
// one line naming the backend and pointing at the `bucket` row below it; on
// `fs` it is the whole preflight for a DIRECTORY, which is a different set of
// questions from "is the bucket reachable" and is the reason this check exists
// rather than `bucket` quietly changing meaning:
//
//   - does STORAGE_DIR exist, and is it a directory;
//   - can THIS process write to it (a probe file, the way the bucket check
//     puts and removes a probe object — the image runs as uid 10001 and a NAS
//     share is mounted with whatever owner the operator gave it);
//   - how much room is left on it, which on `fs` is the only number that
//     bounds how many clips this instance can hold;
//   - is it on the same filesystem as SCRATCH_DIR — reported, never judged,
//     because both answers are legitimate and only the operator knows which
//     one they meant to deploy;
//   - and does it carry the store marker. Its ABSENCE on an instance whose
//     database already accounts for stored bytes is the shape a mis-mounted
//     bind takes: an empty directory where the media used to be, and an app
//     that answers 404 for every clip it still lists. That is a WARN and not a
//     FAIL, because the innocent version of it — an upgrade from a build older
//     than the marker — looks exactly the same from here.
func (r *doctorRun) checkStore(ctx context.Context, cfg config.Config, getenv func(string) string) string {
	if cfg.StorageBackend != config.BackendFS {
		r.say("store", vOK, "STORAGE_BACKEND=s3: the media lives in the bucket, which is checked on the `bucket` row below; STORAGE_DIR is unused")
		return vOK
	}
	verdict := vOK
	worse := func(v string) {
		if v == vFail || (v == vWarn && verdict == vOK) {
			verdict = v
		}
	}
	dir := cfg.StorageDir
	fi, err := os.Stat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		r.say("store", vWarn, "STORAGE_DIR=%s does not exist yet (`clippet serve` creates it at boot - but if it is a BIND MOUNT, this is what a missing host directory looks like: DEPLOY.md §32)", dir)
		return vWarn
	case err != nil:
		r.say("store", vFail, "STORAGE_DIR=%s: %s", dir, r.safe(err))
		return vFail
	case !fi.IsDir():
		r.say("store", vFail, "STORAGE_DIR=%s is not a directory", dir)
		return vFail
	}
	st, err := r.openStore(cfg)
	if err != nil {
		r.say("store", vFail, "cannot build the storage client: %s", r.safe(err))
		return vFail
	}
	if err := st.Ping(ctx); err != nil {
		r.say("store", vFail, "STORAGE_DIR=%s is not writable by this process (uid %d): %s: %s", dir, os.Geteuid(), storeClass(err), r.safe(err))
		worse(vFail)
	} else {
		free, total, derr := diskUsage(dir)
		if derr != nil {
			r.say("store", vOK, "STORAGE_DIR=%s writable (probe file created and removed; free space unavailable: %s)", dir, r.safe(derr))
		} else {
			r.say("store", vOK, "STORAGE_DIR=%s writable (probe file created and removed), free=%d (%s) size=%d (%s)",
				dir, free, humanBytes(free), total, humanBytes(total))
		}
	}
	if same, serr := sameDevice(dir, cfg.ScratchDir); serr != nil {
		r.say("store", vOK, "whether STORAGE_DIR and SCRATCH_DIR=%s share a filesystem could not be determined: %s", cfg.ScratchDir, r.safe(serr))
	} else if same {
		r.say("store", vOK, "STORAGE_DIR shares a filesystem with SCRATCH_DIR=%s: one encode's scratch copy and every stored clip draw on the same free space (fine on one disk; not what a mounted share is for)", cfg.ScratchDir)
	} else {
		r.say("store", vOK, "STORAGE_DIR is on a DIFFERENT filesystem from SCRATCH_DIR=%s: a full scratch disk cannot fill the store, or the other way round", cfg.ScratchDir)
	}
	worse(r.checkStoreMarker(ctx, cfg))
	worse(r.checkStoreHost(cfg, getenv))
	// The sentence the whole feature is for, said where an operator can check
	// it (§15, DEPLOY §32). Its second half is about every outbound feature,
	// and with the clip webhook set it would be false (§20 erratum 319).
	if cfg.ClipWebhook.Configured() {
		r.say("store", vOK, "on `fs` the media path has NO outbound destination")
	} else {
		r.say("store", vOK, "on `fs` the media path has NO outbound destination: with the webhook and the bot unset this instance opens no outbound connection at all (§15)")
	}
	return verdict
}

// The two rows compose.fs.yaml passes down about the HOST side of the bind
// (ROADMAP #42 (a), §20 erratum 295). They are not §4 configuration — nothing
// in internal/config reads them and nothing in this binary uses them for
// anything but the sentence below — which is why they are spelled here, beside
// their only reader, rather than in the Config struct.
const (
	// envDropFolderOwner is §4 configuration and internal/config validates it;
	// it is named here so the row's own sentences and the variable cannot
	// drift apart.
	envDropFolderOwner   = "DROP_FOLDER_OWNER"
	envStorageDirHost    = "STORAGE_DIR_HOST"
	envStorageDirHostSet = "STORAGE_DIR_HOST_SET"
	// rootVideosDefault is what the desktop default resolves to when neither
	// HOME nor USERPROFILE is exported: `${HOME:-${USERPROFILE}}` is empty and
	// the `/Videos/clippet` tail is left standing at the filesystem root.
	rootVideosDefault = "/Videos/clippet"
)

// checkDrop is the drop folder's row (ROADMAP #42 (b), §20 erratum 296): off,
// or on for one account, and what is sitting in the folder right now.
//
// Three things an operator wants from it, and the second is why it is a row of
// its own rather than a sentence on `store`:
//
//   - is it ON, and for WHOM. The whole feature is one variable, and "whose
//     library do the files I drop end up in" is the only question it answers;
//   - does that account EXIST. `clippet serve` refuses to boot when it does
//     not (a folder watched for nobody is the one state this must never be in),
//     and a doctor that reported the refusal AFTER the restart would be telling
//     an operator what the logs already told them. This is the preflight;
//   - what is in the folder now — how many files are waiting, how many have a
//     `.failed.txt` beside them, and whether anything is stuck in the staging
//     directory. That last one is the shape a crash mid-ingest takes, and the
//     watcher puts it back at its next boot.
func (r *doctorRun) checkDrop(ctx context.Context, cfg config.Config) string {
	if cfg.DropFolderOwner == "" {
		r.say("drop", vOK, "%s is unset: the drop folder is off and nothing at the root of the media directory is ever read (DEPLOY.md §34)", envDropFolderOwner)
		return vOK
	}
	// The row reads the same on BOTH backends since §20 erratum 296 (m): what
	// the watcher needs is STORAGE_DIR, and `config.Load` has already refused
	// an owner named without one. On `s3` that directory holds nothing but the
	// staging directory and the clips go to the bucket; the sentence below is
	// true of either, so there is no second wording to keep in step.
	if !r.dropOwnerExists(ctx, cfg) {
		r.say("drop", vFail, "%s=%s names no account on this instance: `clippet serve` will refuse to start - register that account, or remove the line (DEPLOY.md §34)",
			envDropFolderOwner, cfg.DropFolderOwner)
		return vFail
	}
	r.say("drop", vOK, "on: a video left at the ROOT of %s becomes a clip owned by %s, with that account's own defaults (DEPLOY.md §34)",
		cfg.StorageDir, cfg.DropFolderOwner)
	waiting, failed, staged, err := dropFolderState(cfg.StorageDir)
	if err != nil {
		r.say("drop", vWarn, "the folder could not be listed: %s", r.safe(err))
		return vWarn
	}
	r.say("drop", vOK, "the folder holds %d file(s) waiting, %d with a %s beside them, %d staged mid-ingest (a staged file is put back at the next boot)",
		waiting, failed, drop.FailedSuffix, staged)
	return vOK
}

// dropOwnerExists asks the database the one question `serve` asks at boot. A
// database that cannot be read is NOT a missing account: the `db` row above
// already says so, and a FAIL here would send an operator to the wrong problem.
func (r *doctorRun) dropOwnerExists(ctx context.Context, cfg config.Config) bool {
	d, err := db.Open(dbPath(cfg))
	if err != nil {
		return true
	}
	defer d.Close()
	if _, err := d.GetUserByUsername(ctx, cfg.DropFolderOwner); err != nil {
		return !errors.Is(err, db.ErrNotFound)
	}
	return true
}

// dropFolderState counts what is in the folder, by the watcher's own rules: one
// level, no recursion, dotfiles and directories skipped. It reads names and
// nothing else.
func dropFolderState(dir string) (waiting, failed, staged int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, 0, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if strings.HasSuffix(strings.ToLower(name), drop.FailedSuffix) {
			failed++
			continue
		}
		if drop.IsNote(name) { // a duplicate's `.clippet.txt`: neither waiting nor failed
			continue
		}
		waiting++
	}
	stagedEntries, serr := os.ReadDir(filepath.Join(dir, drop.StageDir))
	if serr == nil {
		for _, e := range stagedEntries {
			if !e.IsDir() {
				staged++
			}
		}
	}
	return waiting, failed, staged, nil
}

// checkStoreHost says WHICH DIRECTORY ON THE HOST /media actually is, and which
// variable supplied it (ROADMAP #42 (a), §20 erratum 295).
//
// The container knows only its own side of a bind mount, so this is the one
// fact `admin doctor` could not previously answer and the one an operator on a
// desktop most needs: `compose.fs.yaml` now DEFAULTS STORAGE_DIR_HOST to the
// person's Videos folder, and a default nobody can read from inside is a
// default nobody can trust. compose passes the resolved path down beside a flag
// saying whether the operator named it; both are informational and neither is
// ever joined into a path by this binary.
//
// Silence when the pair is absent: that is every `s3` deployment, every
// hand-rolled `docker run`, and every stack older than erratum 295. A row
// claiming to know the host path when nothing told it one would be the only
// lie on this report.
//
// The WARN is the one gap the compose file cannot close itself. The default is
// spelled `${STORAGE_DIR_HOST:-${HOME:-${USERPROFILE}}/Videos/clippet}`, and
// Compose v2 evaluates a nested `:?` guard EAGERLY (measured; `scripts/
// fsdefault.sh`), so a guard for "this machine exports neither" would refuse on
// every Linux and macOS box, where USERPROFILE is never set. With neither
// variable exported — a scheduler, a service account, `env -i` — the default
// therefore resolves to `/Videos/clippet` at the root of the host filesystem.
// That is exactly detectable here, and only here: the default supplied the
// path, AND what it supplied is that path.
func (r *doctorRun) checkStoreHost(cfg config.Config, getenv func(string) string) string {
	host := strings.TrimSpace(getenv(envStorageDirHost))
	if host == "" {
		return vOK
	}
	if strings.TrimSpace(getenv(envStorageDirHostSet)) != "" {
		r.say("store", vOK, "the host directory bound at %s is %s (%s in .env)", cfg.StorageDir, host, envStorageDirHost)
		return vOK
	}
	if host == rootVideosDefault {
		r.say("store", vWarn, "the host directory bound at %s is %s, at the ROOT of the host filesystem: the desktop default is your Videos folder and it is spelled from HOME (or USERPROFILE on Windows), neither of which this environment exported - set %s in .env to the directory you meant (DEPLOY.md §32.1)",
			cfg.StorageDir, host, envStorageDirHost)
		return vWarn
	}
	r.say("store", vOK, "the host directory bound at %s is %s (the desktop default: your Videos folder, from HOME or USERPROFILE - set %s in .env to move it, DEPLOY.md §32.1)",
		cfg.StorageDir, host, envStorageDirHost)
	return vOK
}

// checkStoreMarker reports store.MarkerName, and WARNs when it is missing on a
// database that already accounts for stored bytes. `StorageUsed` is the same
// sum `admin stats` and the budget line read, and it is one aggregate over the
// clip table on a command an operator runs by hand.
func (r *doctorRun) checkStoreMarker(ctx context.Context, cfg config.Config) string {
	present, err := store.HasMarker(cfg.StorageDir)
	if err != nil {
		r.say("store", vWarn, "%s could not be read: %s", store.MarkerName, r.safe(err))
		return vWarn
	}
	if present {
		r.say("store", vOK, "%s is present: this directory is the one an instance has been writing to", store.MarkerName)
		return vOK
	}
	stored, known := r.storedBytes(ctx, cfg)
	if known && stored > 0 {
		r.say("store", vWarn, "%s is MISSING while the database accounts for %d (%s) of stored clips: either this is not the directory that database knows (check the bind mount - DEPLOY.md §32.4), or this instance predates the marker: `clippet serve` never writes it over a folder the database says holds clips, so if this IS the directory create an empty file named %s at its root",
			store.MarkerName, stored, humanBytes(stored), store.MarkerName)
		return vWarn
	}
	r.say("store", vOK, "%s is absent and the database accounts for no stored clips: a fresh store (`clippet serve` writes the marker at boot)", store.MarkerName)
	return vOK
}

// storedBytes is the database's own sum of what the store should hold, or
// known=false when there is no database yet or it cannot be read — in which
// case the marker check says nothing rather than guessing.
func (r *doctorRun) storedBytes(ctx context.Context, cfg config.Config) (int64, bool) {
	path := dbPath(cfg)
	if _, err := os.Stat(path); err != nil {
		return 0, false
	}
	d, err := db.Open(path)
	if err != nil {
		return 0, false
	}
	defer d.Close()
	used, err := d.StorageUsed(ctx)
	if err != nil {
		return 0, false
	}
	return used, true
}

// storeClass names the bounded failure class of a storage error, so the line
// stays greppable even when the library's own text changes.
func storeClass(err error) string {
	var pe *store.PingError
	if errors.As(err, &pe) {
		return pe.HealthClass()
	}
	if c := store.PublicClass(err); c != "" {
		return c
	}
	return "error"
}

// checkProxies is the second known footgun (DEPLOY §4): TRUSTED_PROXIES says
// "anything from this network may tell me who the client is", and that is only
// safe while the app port is bound to loopback. A container cannot see its own
// port map, but the operator runs THIS command with the same `.env` compose
// reads, so CLIPPET_PUBLISH is usually right there — and when it is, the
// dangerous combination is a failure rather than a reminder.
func (r *doctorRun) checkProxies(cfg config.Config, getenv func(string) string) string {
	if len(cfg.TrustedProxies) == 0 {
		r.say("proxies", vOK, "TRUSTED_PROXIES is empty: X-Real-IP and X-Forwarded-For are ignored, every limit is keyed on the TCP peer")
		return vOK
	}
	prefixes := make([]string, len(cfg.TrustedProxies))
	for i, p := range cfg.TrustedProxies {
		prefixes[i] = p.String()
	}
	list := strings.Join(prefixes, ",")
	publish := strings.TrimSpace(getenv("CLIPPET_PUBLISH"))
	if publish != "" && !publishIsLoopback(publish) {
		r.say("proxies", vFail, "TRUSTED_PROXIES=%s together with CLIPPET_PUBLISH=%s: the port is reachable beyond loopback, so any caller can spoof X-Real-IP past every per-IP limit and every view dedupe (DEPLOY.md §4)", list, publish)
		return vFail
	}
	if publish == "" {
		r.say("proxies", vWarn, "TRUSTED_PROXIES=%s is believed for X-Real-IP/X-Forwarded-For; CLIPPET_PUBLISH is not in this environment, so make sure the app port is bound to loopback (DEPLOY.md §4)", list)
		return vWarn
	}
	r.say("proxies", vOK, "TRUSTED_PROXIES=%s with CLIPPET_PUBLISH=%s (loopback bind, which is what makes trusting a prefix safe - DEPLOY.md §4)", list, publish)
	return vOK
}

// publishIsLoopback reads a compose `ports:` left-hand side ("127.0.0.1:8481",
// "8481", "0.0.0.0:8481", "[::1]:8481") and reports whether the app is
// reachable only from this machine.
//
// It deliberately does NOT reuse isLoopbackHost: that helper answers a
// question about an ORIGIN, where the unspecified address means "this box",
// and here the same string means the exact opposite — `0.0.0.0` binds EVERY
// interface, which is the widened bind DEPLOY §4 warns about. A bare port
// means the same thing.
func publishIsLoopback(publish string) bool {
	i := strings.LastIndex(publish, ":")
	if i < 0 {
		return false // a bare port publishes on every interface
	}
	host := strings.TrimSuffix(strings.TrimPrefix(publish[:i], "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return addr.IsLoopback()
}

// checkWebhook validates the SHAPE and never calls it: a preflight that posted
// to the channel would put a message in the operator's Discord every time somebody
// ran it. The value is a secret and is rendered through the redaction that
// exists for it.
//
// The clip webhook (§20 erratum 319) shares the check — the summary's count of
// seventeen is pinned by four E2E bundles — and prints LAST, after every Discord
// row; with it unset every row here is what it was before it existed.
func (r *doctorRun) checkWebhook(cfg config.Config) string {
	v := r.checkDiscordWebhook(cfg)
	if w := cfg.ClipWebhook; w.Configured() {
		// `ok` for http as well: a permanent warn for a receiver on the
		// operator's own network would teach them to ignore warns. The cost is
		// said in the row instead.
		tail := ""
		if w.Scheme() == "http" {
			tail = "; plain http can be read by anyone on the network path"
		}
		r.say("webhook", vOK, "CLIP_WEBHOOK_URL parses as %s - every clip that becomes ready is sent there, unlisted ones included, signed with CLIP_WEBHOOK_SECRET (DEPLOY.md §11.1); the shape is checked, nothing is sent%s",
			w.String(), tail)
	}
	return v
}

// checkDiscordWebhook is the Discord half of the `webhook` check.
func (r *doctorRun) checkDiscordWebhook(cfg config.Config) string {
	if !cfg.DiscordEnabled() {
		if cfg.ClipWebhook.Configured() {
			// The sentence below would be false: the clip webhook IS an
			// outbound connection, and a clip IS announced somewhere.
			r.say("webhook", vOK, "DISCORD_WEBHOOK_URL is unset: no clip is posted to Discord")
			return vOK
		}
		// The tail of that sentence depends on the backend, and two rows above
		// this one `store` has just said so: on `fs` there is no S3 to make an
		// exception for (§20 erratum 276).
		rest := "the binary opens no outbound connection but S3 (§15)"
		if cfg.StorageBackend == config.BackendFS {
			rest = "with STORAGE_BACKEND=fs the binary opens no outbound connection at all (§15)"
		}
		r.say("webhook", vOK, "DISCORD_WEBHOOK_URL is unset: no clip is announced anywhere and %s", rest)
		return vOK
	}
	if cfg.DiscordWebhook.Configured() {
		r.say("webhook", vOK, "DISCORD_WEBHOOK_URL parses as %s - every LISTED clip that becomes ready is posted there (DEPLOY.md §11); the shape is checked, nothing is sent",
			cfg.DiscordWebhook.String())
	} else {
		r.say("webhook", vOK, "DISCORD_WEBHOOK_URL is unset, but DISCORD_WEBHOOK_ROUTES is not: only a clip a rule matches is announced (DEPLOY.md §25)")
	}
	// The per-tag routes (ROADMAP #26, §20 errata 158-159). The TAG is what
	// gets printed — it is the name a log line and a job's params_json use —
	// and each rule's own URL goes through the same redaction the default one
	// does. `unmatched` is printed too, because "everything else goes nowhere"
	// is the half of this configuration an operator most often did not mean.
	for _, rt := range cfg.DiscordRoutes {
		r.say("webhook", vOK, "route %q -> %s", rt.Tag, rt.Webhook.String())
	}
	if len(cfg.DiscordRoutes) > 0 {
		r.say("webhook", vOK, "%d route(s); a clip no rule matches goes to %s",
			len(cfg.DiscordRoutes), unmatchedWords(cfg))
	}
	return vOK
}

// checkBot reports the Discord bot (ROADMAP #33, §20 errata 253-255). Like
// checkWebhook it validates the SHAPE and calls nothing: a preflight that
// registered a command would change what a Discord server shows every time
// somebody ran it.
//
// The one line worth reading when the bot is ON is the ENDPOINT URL: it is
// what an operator pastes into the developer portal, it is derived from
// PUBLIC_BASE_URL, and getting it wrong is the single most likely reason a
// green `.env` still produces "The application did not respond".
//
// On a MANAGED instance (DISCORD_BOT_TOKEN=managed, §20 erratum 279) the
// operator pastes nothing anywhere: the official bot's forwarder reaches this
// instance at the same endpoint, and what matters instead is WHICH servers
// are bound — that is what the claim page's origin check answers 200 for —
// so the row lists them, read from the database the same way checkDB does.
//
// Since §20 erratum 284 it also prints the command's NAME, which is
// `DISCORD_COMMAND_NAME` and no longer always `/clip`: an operator running a dev
// instance beside a production one needs to see which word each box answers,
// and this row is where the rest of the bot's facts already are. Since erratum
// 315, all three names (`DISCORD_UPLOAD_COMMAND_NAME` and
// `DISCORD_ENRICH_COMMAND_NAME` too).
func (r *doctorRun) checkBot(ctx context.Context, cfg config.Config) string {
	if !cfg.DiscordBot.Configured() {
		r.say("bot", vOK, "discord bot: off (%s, %s and %s are unset): %s is not served and no slash command exists",
			config.EnvDiscordAppID, config.EnvDiscordPublicKey, config.EnvDiscordBotToken, httpapi.InteractionsPath())
		return vOK
	}
	verdict := vOK
	// The COMMAND's name rides on this row rather than on one of its own
	// (§20 erratum 284): it is a fact about the bot that is already configured,
	// and `doctorChecks` is a stable list an operator greps by name.
	//
	// Since §20 erratum 315 there are THREE names, and the row prints them all,
	// upload first — `commands: /upload, /clip and Enrich`.
	upload := "/" + cfg.DiscordBot.UploadCommandName()
	command := "/" + cfg.DiscordBot.CommandName()
	enrich := cfg.DiscordBot.EnrichCommandName()
	// …and since §20 erratum 318 a fourth, `/settings` (the link defaults).
	settings := "/" + cfg.DiscordBot.SettingsCommandName()
	if cfg.DiscordBot.Managed() {
		r.say("bot", vOK, "discord bot: on - %s; commands: %s, %s, %s and %s; this instance publishes no command and sends no token anywhere: the official bot's /upload, /clip and Enrich are routed to it, and its /settings panel relays the link-defaults clicks (DEPLOY.md §30.8)",
			cfg.DiscordBot.String(), upload, command, enrich, settings)
	} else {
		r.say("bot", vOK, "discord bot: on - %s; commands: %s, %s, %s and %s; the token is used only to publish those commands and is never sent anywhere else",
			cfg.DiscordBot.String(), upload, command, enrich, settings)
	}
	// The upload form (§20 erratum 320): one line, whichever way it is set.
	if cfg.UploadForm {
		r.say("bot", vOK, "upload form: on - %s opens a form (Title, Delete after, Tags) while the upload runs; %s=off removes it",
			upload, config.EnvDiscordUploadForm)
	} else {
		r.say("bot", vOK, "upload form: off (%s=off) - %s opens no form", config.EnvDiscordUploadForm, upload)
	}
	// Enrich's upload (§20 erratum 323, off by default): one line, either way
	// (the settled words; the command's configured name).
	if cfg.EnrichUpload {
		r.say("bot", vOK, "enrich upload: on - %s on a message with a video uploads it, for its poster or Manage Server; %s=off stops it",
			enrich, config.EnvDiscordEnrichUpload)
	} else {
		r.say("bot", vOK, "enrich upload: off - %s answers links only; %s=on lets it upload a video",
			enrich, config.EnvDiscordEnrichUpload)
	}
	if u := cfg.PublicBaseURL; u != nil {
		endpoint := u.String() + httpapi.InteractionsPath()
		if cfg.DiscordBot.Managed() {
			r.say("bot", vOK, "the official bot reaches this instance at %s; the claim page's origin check is GET %s%s?guild_id=<server id> (200 for a bound server, 404 otherwise)",
				endpoint, u.String(), httpapi.BotClaimPath())
		} else {
			r.say("bot", vOK, "Interactions Endpoint URL (paste this into the Developer Portal → General Information): %s", endpoint)
		}
		if u.Scheme != "https" || isLoopbackHost(u.Hostname()) {
			// Discord POSTs interactions from its own network: it cannot
			// reach a loopback origin at all, and it requires https. This is
			// the difference between "the bot is configured" and "the bot
			// works", so it is a warning rather than a fact. The official
			// bot's forwarder is in the same position.
			r.say("bot", vWarn, "Discord can only POST to a PUBLIC https origin, and PUBLIC_BASE_URL is %s - the endpoint will never be reached (DEPLOY.md §30)", u.String())
			verdict = vWarn
		}
	}
	if cfg.DiscordBot.Managed() {
		r.say("bot", vOK, "%s", r.boundServers(ctx, cfg))
	}
	r.say("bot", vOK, "a bound server's %s uploads into that account's library and its %s posts a clip already here (a link, never fetched); attachments are fetched from %s and nothing else (§15)",
		upload, command, strings.Join(config.DiscordCDNHosts(), ", "))
	return verdict
}

// boundServers renders the managed row's binding list. It reads the database
// the way checkDB does and never changes the verdict: a database that cannot
// be read is checkDB's finding, and this line only says it could not look.
func (r *doctorRun) boundServers(ctx context.Context, cfg config.Config) string {
	d, err := db.Open(dbPath(cfg))
	if err != nil {
		return "bound servers: could not be read (see the db check)"
	}
	defer d.Close()
	rows, err := d.ListGuildBindings(ctx)
	if err != nil {
		return "bound servers: could not be read (see the db check)"
	}
	if len(rows) == 0 {
		return "bound servers: none yet - run `clippet admin bot bind <server id> <username>` before connecting the server on the claim page"
	}
	parts := make([]string, 0, len(rows))
	for _, g := range rows {
		parts = append(parts, g.GuildID+" → "+g.Username)
	}
	return fmt.Sprintf("bound server(s): %s", strings.Join(parts, ", "))
}

// doctorChecks is the stable set of check names, in report order. DEPLOY §24
// documents them as the strings an operator greps for, and the tests assert
// every one of them appears in every run — a check that silently stopped
// running would otherwise look like a clean bill of health.
var doctorChecks = []string{"config", "baseurl", "paths", "db", "ffmpeg", "creds", "store", "bucket", "media", "drop", "proxies", "webhook", "bot", "auto", "login", "repair", "backup", "snapshot", "prices"}

// autoProbe is checkAuto's one network read (a seam for the tests): GET
// /users/@me and GET /applications/@me with the Auto app's token, reduced to
// two booleans by internal/notify.
var autoProbe = func(ctx context.Context, token string) (tokenOK, contentIntent bool, err error) {
	c := notify.NewAutoClient(notify.AutoDeps{Token: token, APIHost: config.DiscordAPIHost(), AllowGateway: config.ValidGatewayURL})
	if c == nil {
		return false, false, errors.New("no client")
	}
	return c.Me(ctx)
}

// checkAuto reports Clippet Auto on the box (§20 erratum 333, DEPLOY §30.10).
// Off, it calls nothing. On, it is the doctor's ONE Discord call, and it is
// read-only: does the token answer GET /users/@me, and is the Message Content
// intent on (the application's flags) — without it the gateway closes with
// 4014 and Clippet Auto halts. Nothing of either answer is printed.
func (r *doctorRun) checkAuto(ctx context.Context, cfg config.Config) string {
	a := cfg.DiscordAuto
	if !a.On() {
		r.say("auto", vOK, "clippet auto: off (%s=%s): no gateway socket is opened and no channel is read",
			config.EnvDiscordAutoMode, a.Mode())
		return vOK
	}
	r.say("auto", vOK, "clippet auto: %s - acts only in bound servers; the socket goes to %s and nowhere else (DEPLOY.md §30.10)",
		a.String(), config.DiscordGatewayHost())
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tokenOK, intent, err := autoProbe(pctx, a.Token())
	switch {
	case err != nil:
		r.say("auto", vWarn, "Discord could not be asked whether %s works (no answer from %s); the app will retry on its own",
			config.EnvDiscordAutoToken, config.DiscordAPIHost())
		return vWarn
	case !tokenOK:
		r.say("auto", vFail, "%s is refused by Discord: reset the token on the Auto app's Bot page and paste the new one (DEPLOY.md §30.10)",
			config.EnvDiscordAutoToken)
		return vFail
	case !intent:
		r.say("auto", vFail, "the token answers, but the Message Content intent is off: turn it on under the Auto app's Bot page → Privileged Gateway Intents, or the gateway refuses the socket (4014) (DEPLOY.md §30.10)")
		return vFail
	}
	r.say("auto", vOK, "the token answers and the Message Content intent is on")
	return vOK
}

// checkLogin reports "Sign in with Discord" (ROADMAP #44, §20 erratum 302).
// Like checkBot it validates the SHAPE and calls nothing. The one line worth
// reading when it is ON is the REDIRECT URI: it is what the operator pastes
// into the developer portal, it is derived from PUBLIC_BASE_URL, and Discord
// compares it byte-exact — a mismatch is refused at Discord's own page, which
// is the single most likely reason a green `.env` still produces "Invalid
// OAuth2 redirect_uri". The secret is never printed: the row renders
// config.DiscordLogin.String(), which is the client id and the word redacted.
func (r *doctorRun) checkLogin(cfg config.Config) string {
	signin := r.checkSignin(cfg)
	oauth := r.checkDiscordLogin(cfg)
	if signin == vWarn || oauth == vWarn {
		return vWarn
	}
	return oauth
}

// checkSignin is the `login` row's first line since §20 erratum 341: the bot's
// `/signin` door, named by its REGISTRATION mode. A Discord mode with the bot
// OFF is a warning — /register is closed under it, so nobody new gets in at
// all but through `admin users create`.
func (r *doctorRun) checkSignin(cfg config.Config) string {
	mode := cfg.Registration.String()
	switch {
	case !cfg.Registration.DiscordSignIn():
		r.say("login", vOK, "/signin: off (REGISTRATION=%s; members or role:<id> turns it on, DEPLOY.md §19.1)", mode)
	case !cfg.DiscordBot.Configured():
		r.say("login", vWarn, "/signin: REGISTRATION=%s but the Discord bot is off - nobody can sign in that way, and /register is closed (DEPLOY.md §30.12)", mode)
		return vWarn
	case cfg.Registration.SignInRole() != "":
		r.say("login", vOK, "/signin: on - REGISTRATION=%s: a member of a bound server holding that role signs in with a one-time link; /register is closed (DEPLOY.md §30.12)", mode)
	default:
		r.say("login", vOK, "/signin: on - REGISTRATION=%s: any member of a bound server signs in with a one-time link; /register is closed (DEPLOY.md §30.12)", mode)
	}
	return vOK
}

// checkDiscordLogin is the OAuth key's half of the `login` row (erratum 302).
func (r *doctorRun) checkDiscordLogin(cfg config.Config) string {
	if !cfg.DiscordLogin.Configured() {
		r.say("login", vOK, "discord login: off (%s and %s are unset): %s is not served and /login shows no Discord button",
			config.EnvDiscordLoginClientID, config.EnvDiscordLoginClientSecret, httpapi.DiscordLoginStartPath())
		return vOK
	}
	verdict := vOK
	r.say("login", vOK, "discord login: on - %s; the secret is sent to Discord's token endpoint and nowhere else; a Discord login opens an account that is LINKED and never creates one (DEPLOY.md §19.6)",
		cfg.DiscordLogin.String())
	if u := cfg.PublicBaseURL; u != nil {
		r.say("login", vOK, "Redirect URI (paste this into the Developer Portal → OAuth2 → Redirects, byte for byte): %s", u.String()+httpapi.DiscordLoginCallbackPath())
		if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
			// Discord accepts a plain-http redirect only on localhost: a
			// quick-tunnel typo or a LAN address here is refused at Discord's
			// page, after the person has already consented.
			r.say("login", vWarn, "Discord accepts a plain-http redirect URI only on localhost, and PUBLIC_BASE_URL is %s - the callback will be refused at Discord's own page (DEPLOY.md §19.6)", u.String())
			verdict = vWarn
		}
	}
	return verdict
}

// checkMedia reports how media is DELIVERED (ROADMAP #34 lane F, §20 erratum
// 278) — which is a different question from where it is STORED, and the one
// an operator who has just switched `.env` wants answered from inside the
// container before any viewer sees it.
//
// It sits directly under `bucket` because that is the thing it depends on, and
// it cannot FAIL: `proxy` is the shipped state, `redirect` on a directory is
// already a boot error, and the one thing worth flagging — a presign origin
// only this machine can reach, under a public PUBLIC_BASE_URL — is an operator
// decision the app cannot prove wrong from inside (a private name can resolve
// publicly).
func (r *doctorRun) checkMedia(ctx context.Context, cfg config.Config, getenv func(string) string) string {
	if note := config.MediaNote(cfg, getenv); note != "" {
		// A variable that was read and then ignored. NOT a warning: it is what
		// `.env` looks like while an operator is getting ready to flip the
		// switch, and the reason this row exists is so "I set it and nothing
		// happened" has an answer that is not a viewer.
		r.say("media", vOK, "MEDIA_DELIVERY=%s: this instance serves every media byte itself, exactly as every version before ROADMAP #34 did. NOTE: %s",
			cfg.MediaDelivery, note)
		return vOK
	}
	if !cfg.RedirectsMedia() {
		r.say("media", vOK, "MEDIA_DELIVERY=%s: this instance serves every media byte itself (DEPLOY.md §33 is the other mode)", cfg.MediaDelivery)
		return vOK
	}
	// The origin is asked of the SIGNER, exactly as `serve` asks it, because
	// the configured endpoint is not it whenever the bucket lookup is
	// virtual-host (§20 erratum 278, amended). Signing is local HMAC, so this
	// row stays as read-only and as offline as every other one.
	st, err := r.openStore(cfg)
	if err != nil {
		r.say("media", vWarn, "MEDIA_DELIVERY=%s, but the storage client could not be built, so the origin a viewer would be redirected to cannot be shown here: %s (the `store`/`bucket` rows above have the reason)",
			cfg.MediaDelivery, r.safe(err))
		return vWarn
	}
	origin, err := store.MediaOrigin(ctx, st, cfg.MediaPresignTTL)
	if err != nil {
		r.say("media", vWarn, "MEDIA_DELIVERY=%s, but no presigned URL could be minted, so a viewer would be redirected nowhere: %s",
			cfg.MediaDelivery, r.safe(err))
		return vWarn
	}
	// The presign TTL is printed because it is the one value here whose
	// consequence is invisible until somebody seeks: a `<video>` keeps fetching
	// the SAME src for as long as its page is open. The endpoint is printed
	// BESIDE the origin whenever they differ, because "why does the CSP say
	// bucket.acct.example when I set acct.example" is the question this row
	// exists to answer.
	endpoint := ""
	if cfg.S3.PresignEndpoint != origin {
		endpoint = fmt.Sprintf(" (signed against %s; the bucket is in the host because the lookup is virtual-host - S3_FORCE_PATH_STYLE=%v)",
			cfg.S3.PresignEndpoint, cfg.S3.ForcePathStyle)
	}
	line := fmt.Sprintf("MEDIA_DELIVERY=%s: video.mp4, social.mp4 and poster.jpg answer 302 to %s%s, signed for %s per request; /original, preview.gif and filmstrip.jpg stay app-served",
		cfg.MediaDelivery, origin, endpoint, cfg.MediaPresignTTL)
	// BOTH origins are tested, and that is not belt-and-braces: with
	// virtual-host lookup the bucket is prefixed to the host, so a compose
	// service name `minio` — a single label, which is what the heuristic
	// catches — arrives here as `clips.minio`, which has a dot and would slip
	// through. The ENDPOINT is also the thing an operator actually sets, so it
	// is the one the warning names.
	if cfg.PublicBaseURL != nil && !isLoopbackHost(cfg.PublicBaseURL.Hostname()) {
		if host := privateMediaHost(origin, cfg.S3.PresignEndpoint); host != "" {
			r.say("media", vWarn, "%s - but %s is a name only this machine or this network can reach, while PUBLIC_BASE_URL=%s is public: a viewer would follow the redirect to nothing. Set MEDIA_PRESIGN_ENDPOINT to the origin VIEWERS reach the bucket at (DEPLOY.md §33)",
				line, host, cfg.PublicBaseURL)
			return vWarn
		}
	}
	r.say("media", vOK, "%s (DEPLOY.md §33)", line)
	return vOK
}

// privateMediaHost returns the first of the given origins whose HOST the public
// internet cannot be expected to reach, or "" when none is. It takes more than
// one because a virtual-host presign and the endpoint it was signed against are
// different strings and either can be the operator's mistake.
func privateMediaHost(origins ...string) string {
	for _, o := range origins {
		u, err := url.Parse(o)
		if err != nil || u.Hostname() == "" {
			continue
		}
		if isPrivateHost(u.Hostname()) {
			return u.Hostname()
		}
	}
	return ""
}

// isPrivateHost reports whether a host is one the public internet cannot be
// expected to resolve or route to: a literal private, loopback or link-local
// address, or a single-label name like `minio` — which is exactly what a
// compose service is called, and exactly what an operator leaves in
// S3_ENDPOINT when they switch delivery modes without thinking about who
// follows the redirect.
//
// It is deliberately a HEURISTIC, and it only ever produces a warning: a
// single-label name can be resolvable inside somebody's own DNS, and a
// private address is correct for an instance whose viewers are on the LAN.
func isPrivateHost(host string) bool {
	if host == "" {
		return false
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.IsLoopback() || addr.IsUnspecified() || addr.IsPrivate() ||
			addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast()
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	return !strings.Contains(host, ".")
}

// checkPrices reports the two optional storage prices (ROADMAP #39, §20 errata
// 270 and 275). Like `repair` it cannot fail: unset is the shipped state and a
// legitimate one — with no price there is no cost line anywhere — and a price
// that did not parse never reaches here, because config.Load names the variable
// and refuses to boot.
//
// It prints what the app PARSED rather than what the variable said, which is the
// only thing worth checking about a value that round-trips through micros: an
// operator who typed `0,015` gets a boot failure, and one who typed `.015` wants
// to see `0.015` come back. It prints NO AMOUNT — a cost estimate belongs to
// `admin stats`, where the stored and served byte counts are; this row is the
// configuration, and DEPLOY §31 is the page that explains it.
func (r *doctorRun) checkPrices(cfg config.Config) string {
	p := cost.PricesOf(cfg)
	if !p.Any() {
		r.say("prices", vOK, "STORAGE_PRICE_PER_GB_MONTH and EGRESS_PRICE_PER_GB are unset and STORAGE_PRICING names no preset: no cost estimate is shown anywhere (unset is ABSENT, never 0.00 - DEPLOY.md §31)")
		return vOK
	}
	half := func(micros *int64, unit string, typed bool) string {
		if micros == nil {
			return "unset"
		}
		s := cost.PriceString(*micros) + unit
		if typed && p.HasPreset() {
			s += " (yours, over the preset)"
		}
		return s
	}
	storage, egress := half(p.StoragePerGBMonth, "/GB-month", p.StorageTyped), half(p.EgressPerGB, "/GB", p.EgressTyped)
	// A free allowance is part of what the figure beside it MEANS and is not a
	// price, so it survives an override and is printed beside whichever number
	// is in force (§20 erratum 277).
	if words := p.Preset.FreeStorageWords(); words != "" && p.StoragePerGBMonth != nil {
		storage += " " + words
	}
	if words := p.Preset.Allowance.Words(); words != "" && p.EgressPerGB != nil {
		egress += " " + words
	}
	if !p.HasPreset() {
		r.say("prices", vOK, "storage %s, egress %s, in your own currency (no symbol is stored): the estimate is on `admin stats` and /settings, never on the anonymous /healthz - DEPLOY.md §31",
			storage, egress)
		return vOK
	}
	// A preset: the row is a dated copy of somebody else's list, so the report
	// says which list and when it was read. List prices drift; the date is how
	// an operator knows whether to check (DEPLOY §31.5).
	r.say("prices", vOK, "preset %s (STORAGE_PRICING, read %s%s): storage %s, egress %s - a dated copy of the provider's list, never fetched; the estimate is on `admin stats` and /settings, never on the anonymous /healthz - DEPLOY.md §31.5",
		p.Preset.Name, p.Preset.Read, currencyNote(p.Preset), storage, egress)
	return vOK
}

// currencyNote is the `, USD` a preset's row carries and the custom case never
// does: the app is told a currency only when it is quoting somebody else's
// published list (§20 erratum 277; erratum 270's "never told one" holds for the
// operator's own prices, which are still the default).
func currencyNote(p cost.Preset) string {
	if p.Currency == "" {
		return ""
	}
	return ", " + p.Currency
}

// checkRepair reports the automatic stuck-job repair (ROADMAP #12, §20 errata
// 230-231). It is the one place the SETTING is printed: /healthz counts the
// repairs but names no configured value, because it is anonymous (§15), so an
// operator asking "will this instance heal its own queue, and how fast" has to
// be able to ask from inside the container — which is exactly what doctor is.
//
// Nothing here can fail: `off` is a legitimate choice (the state every version
// before this one was in), and neither of the two things worth saying about a
// live setting is an error — a value under the sanity floor is raised, and a
// value under TRANSCODE_HARD_TIMEOUT is obeyed with a warning about the one case
// it costs (a second process on the same database).
func (r *doctorRun) checkRepair(cfg config.Config) string {
	p := cfg.UnstickPlan()
	if !p.On() {
		r.say("repair", vOK, "JOBS_UNSTICK_AFTER=0: a job left `running` with no worker waits for a restart or for `clippet admin jobs unstick` (DEPLOY.md §24.6)")
		return vOK
	}
	verdict := vOK
	if p.Raised() {
		r.say("repair", vWarn, "JOBS_UNSTICK_AFTER=%s is under the %s floor and is RAISED to it", p.Configured, p.Floor)
		verdict = vWarn
	}
	r.say("repair", verdict, "stuck jobs repaired after %s (JOBS_UNSTICK_AFTER), checked every %s; a job this process runs is never repaired",
		p.After, p.Every)
	if p.UnderHardTimeout() {
		// Not a mistake, and not this process's risk: a job it has claimed is
		// excluded by the claim and not by the age. It is the risk of a SECOND
		// process on the same database, which DEPLOY §18 does not support and
		// people do anyway.
		r.say("repair", vWarn, "%s is shorter than TRANSCODE_HARD_TIMEOUT (%s): a live job of a SECOND process sharing this database could be repaired while it is still running - set JOBS_UNSTICK_AFTER=0 there (DEPLOY.md §24.6)",
			p.After, p.HardTimeout)
		verdict = vWarn
	}
	return verdict
}

// checkBackup reports timed backups (§20 erratum 321): off — the default, and
// said so in every run like every other feature — or the settings, then the
// last run and the next as the SERVER recorded them in DATA_DIR's status file.
// Doctor is another process and cannot ask the server; it writes nothing and
// probes nothing for this row, and never lists BACKUP_DIR.
//
// A BACKUP_DIR that is not a directory is a FAIL and ends the row: every run
// would fail, and the app never creates it. A last run that failed is a
// warning — the server keeps serving, and the next run may succeed.
func (r *doctorRun) checkBackup(cfg config.Config) string {
	if !cfg.BackupOn() {
		r.say("backup", vOK, "off: BACKUP_EVERY and BACKUP_DIR are unset (DEPLOY.md §15.6)")
		return vOK
	}
	r.say("backup", vOK, "on: a database snapshot every %s into %s, newest %d kept (no media)", cfg.BackupEvery, cfg.BackupDir, cfg.BackupKeep)
	if fi, err := os.Stat(cfg.BackupDir); err != nil || !fi.IsDir() {
		r.say("backup", vFail, "%s is not a directory: every run fails until it is one (the app never creates it)", cfg.BackupDir)
		return vFail
	}
	// The write probe (run 8): a directory the app cannot write to is caught
	// here, not by the first run. One empty temp file, removed at once.
	if err := probeWritable(cfg.BackupDir); err != nil {
		r.say("backup", vFail, "%s cannot be written to: every run fails until it can (%s)", cfg.BackupDir, r.safe(err))
		return vFail
	}
	st, ok := readBackupStatus(cfg.DataDir)
	if !ok || st.Dir != cfg.BackupDir {
		r.say("backup", vOK, "no run recorded yet")
		return vOK
	}
	verdict := vOK
	switch {
	case st.Last == nil:
		r.say("backup", vOK, "no run recorded yet")
	case st.Last.Result == backupResultOK:
		r.say("backup", vOK, "last: %s ok, %s (%s)", st.Last.At, st.Last.File, humanBytes(st.Last.Bytes))
	default:
		r.say("backup", vWarn, "last: %s failed: %s", st.Last.At, r.safe(errors.New(st.Last.Reason)))
		verdict = vWarn
	}
	if next, err := parseRFCTime(st.Next); err == nil {
		now := time.Now
		if r.env.now != nil {
			now = r.env.now
		}
		if next.Before(now()) {
			r.say("backup", vOK, "next: was due %s", st.Next)
		} else {
			r.say("backup", vOK, "next: %s", st.Next)
		}
	}
	return verdict
}

// checkSnapshot is the `snapshot` row (§20 errata 343, 348): the database copy
// in the store — the bucket, or on `fs` the media folder's `_clippet/`. One
// line, one of four shapes — `on · <age> · <size>`, `off (…)`, `never: …` or
// a FAIL with its reason. It READS two objects' metadata and the server's
// status file; it writes nothing.
//
// `off` is a WARN, not an ok, when timed backups are off too (erratum 348):
// then nothing at all copies this database anywhere, and an instance with no
// backup of any kind is the one state a doctor must not call green. The line
// names the two knobs that each end it.
func (r *doctorRun) checkSnapshot(ctx context.Context, cfg config.Config) string {
	home := "the bucket"
	if cfg.StorageBackend == config.BackendFS {
		home = "the media folder"
	}
	if !cfg.DBSnapshot {
		if !cfg.BackupOn() {
			r.say("snapshot", vWarn, "off (DB_SNAPSHOT=off) and timed backups are off too: this database has NO backup of any kind - unset DB_SNAPSHOT, or set BACKUP_EVERY and BACKUP_DIR (DEPLOY.md §15)")
			return vWarn
		}
		r.say("snapshot", vOK, "off (DB_SNAPSHOT=off): %s alone cannot bring this instance back (DEPLOY.md §15)", home)
		return vOK
	}
	// The server's last word, when it is a failure newer than its last
	// success, is the row's answer whatever the bucket holds: an old copy
	// there is exactly what a failing writer leaves behind. RFC 3339 UTC
	// strings order as their times do.
	if status, ok := readSnapshotStatus(cfg.DataDir); ok && status.FailedAt != "" && status.FailedAt > status.LastOK {
		r.say("snapshot", vFail, "%s (at %s; the server retries every %s)", r.safe(errors.New(status.Reason)), status.FailedAt, cfg.DBSnapshotEvery)
		return vFail
	}
	st, err := r.openStore(cfg)
	if err != nil {
		r.say("snapshot", vFail, "cannot build the storage client: %s", r.safe(err))
		return vFail
	}
	sctx, cancel := context.WithTimeout(ctx, cfg.S3.OpTimeout)
	defer cancel()
	prefix := cfg.S3.KeyPrefix
	sum, err := st.Stat(sctx, store.SnapshotSumKey(prefix))
	if errors.Is(err, store.ErrNotFound) {
		r.say("snapshot", vOK, "never: %s holds no %s yet (the server writes one at boot, then at most every %s)",
			home, store.SnapshotKey(prefix), cfg.DBSnapshotEvery)
		return vOK
	}
	if err != nil {
		r.say("snapshot", vFail, "%s: %s: %s", store.SnapshotSumKey(prefix), storeClass(err), r.safe(err))
		return vFail
	}
	dbi, err := st.Stat(sctx, store.SnapshotKey(prefix))
	if err != nil {
		r.say("snapshot", vFail, "%s: %s: %s", store.SnapshotKey(prefix), storeClass(err), r.safe(err))
		return vFail
	}
	now := time.Now
	if r.env.now != nil {
		now = r.env.now
	}
	r.say("snapshot", vOK, "on · %s · %s (%s, at most every %s)", snapshotAgo(now(), sum.LastModified), humanBytes(dbi.Size),
		store.SnapshotKey(prefix), cfg.DBSnapshotEvery)
	return vOK
}

// probeWritable creates one empty temp file in dir and removes it. A dotted
// name that is not a snapshot's, so rotation and the skip rule never read it.
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".clippet-doctor-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// unmatchedWords renders DISCORD_WEBHOOK_UNMATCHED as the sentence `doctor`
// prints beside the route list. It names no URL: the default webhook is
// already reported on its own line, redacted, and this one only has to say
// WHETHER an unmatched clip is announced at all.
func unmatchedWords(cfg config.Config) string {
	if cfg.DiscordUnmatched == config.DiscordUnmatchedSkip {
		return "nowhere (DISCORD_WEBHOOK_UNMATCHED=skip)"
	}
	if !cfg.DiscordWebhook.Configured() {
		return "nowhere (DISCORD_WEBHOOK_URL is unset, so there is no default channel)"
	}
	return "DISCORD_WEBHOOK_URL's channel"
}
