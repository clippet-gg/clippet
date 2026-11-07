package main

// admin.go — the operator CLI (DESIGN.md §8.8, §18.4):
//
//	clippet admin doctor                               (doctor.go; needs no database)
//	clippet admin expire <id> [--ago 2h | --at 2026-08-31T00:00:00Z]
//	clippet admin import <dir> --owner <username> [--recursive] [--dry-run]
//	clippet admin ls [<id>] [--stat] [--state s] [--owner name]
//	clippet admin stats [--no-storage]
//	clippet admin jobs [--kind k] [--state s] [--clip id] [--since D] [--limit n]
//	clippet admin jobs unstick <job-id> | --older-than D
//	clippet admin retry <id>… | --all-failed
//	clippet admin orphans [--delete] [--min-age 1h]
//	clippet admin objects <id> [--count]               (the bucket only; needs no database)
//	clippet admin objects --prefix <p> [--count] | init | put <key> | rm <key> | rm --prefix <p>
//	clippet admin users
//	clippet admin users create <username>              (password on stdin)
//	clippet admin users link <username> <discord-id> | unlink <username>
//	clippet admin users owner <username>
//	clippet admin users prune <pattern> --older-than D --no-clips
//	clippet admin collections ls [--owner name]
//	clippet admin collections rm <id> [--owner name]
//	clippet admin sessions list|revoke <username> | revoke --all
//	clippet admin tokens list <username> | revoke <username> <tid>|--all
//	clippet admin tokens mint <username> [--name n] [--scopes upload[,read]|inbound|autofetch] [--sxcu]
//	clippet admin storage migrate --to fs|s3 [--dry-run] [--manifest p] [--stop-file p] [--max-bytes N]
//	clippet admin backup <path> | <dir> [--rotate N]
//	clippet admin restore-check <path>
//	clippet admin restore --from-store [--yes]         (admin_restore.go; needs no database)
//
// It opens the same SQLite file as `serve` (WAL + busy_timeout make the
// second process safe) and, for stats/orphans/objects/ls --stat, the bucket. It
// never creates the database and never migrates (that is `clippet migrate`).
// Exit codes: 0 ok, 1 not found / failed, 2 usage or configuration.
//
// `restore-check` is the one exception to all of that: it reads ONE file
// read-only and touches neither the live database nor the bucket, so it needs
// no configuration and runs in a throwaway container (§20 erratum 83).
//
// `objects` is the mirror image of it: it touches the BUCKET and never the
// database, because the question it answers is most often asked about a clip
// whose row has already gone (§20 erratum 281, admin_objects.go).

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"clippet/internal/auth"
	"clippet/internal/config"
	"clippet/internal/cost"
	"clippet/internal/db"
	"clippet/internal/httpapi"
	"clippet/internal/ids"
	"clippet/internal/store"
	"clippet/internal/transcode"
	"clippet/internal/urls"
)

const (
	// adminTimeout bounds one whole admin invocation — every verb but the two
	// that MOVE media, `import` (§20 erratum 307) and `storage migrate` (§20
	// erratum 350), which take a context of their own sized to the work.
	adminTimeout = 10 * time.Minute
	// adminOpTimeout floors STORAGE_OP_TIMEOUT for the CLI: stats and
	// orphans list the whole clips/ prefix in one call, which a
	// sweeper-sized budget could cut short on a large bucket.
	adminOpTimeout = 2 * time.Minute
	// orphanMinAge: a prefix younger than this is never reported as an
	// orphan — an upload in flight writes its original before its row
	// exists (§8.3), so a fresh row-less prefix is not orphaned yet.
	orphanMinAge = time.Hour
)

// adminEnv is what runAdmin injects so tests can drive the CLI with a Fake
// store, fixed clock and captured output.
type adminEnv struct {
	stdout, stderr io.Writer
	// stdin is where `users create` reads the new password from — never a
	// command-line argument, which every shell writes into its history and
	// every `ps` on the box can read (§20 erratum 109).
	stdin io.Reader
	// openStore takes the WHOLE configuration since ROADMAP #37: which store
	// to build is STORAGE_BACKEND's answer, not S3Config's (§20 erratum 276).
	openStore func(config.Config) (store.Store, error)
	now       func() time.Time
	// getenv is what `doctor` reads its configuration from. Only that
	// subcommand needs it: everything else is handed an already-loaded Config.
	getenv func(string) string
	// probe is the ffprobe gate `import` applies to a candidate file (§20
	// erratum 307). nil — which is every production call — means
	// `transcode.Probe(FFPROBE_PATH)`; a test injects one for the reason
	// openStore exists, so the verb can be driven without a video encoder.
	probe func(ctx context.Context, path string) (*transcode.ProbeResult, error)
	// strip is the audio strip `import` applies (§20 erratum 328); nil means
	// `transcode.StripVerified(FFMPEG_PATH, FFPROBE_PATH, threads)`.
	strip func(ctx context.Context, p *transcode.ProbeResult, in, out string) (transcode.StripTarget, error)
	// timeout replaces adminTimeout for the invocation's context, and
	// objectTimeout the per-object deadline of `storage migrate` (§20 erratum
	// 350). Both are test seams: 0 — every production call — means the
	// constant and STORAGE_TRANSFER_MAX respectively.
	timeout, objectTimeout time.Duration
}

func runAdmin(args []string, cfg config.Config) int {
	return runAdminWith(args, cfg, adminEnv{stdout: os.Stdout, stderr: os.Stderr, stdin: os.Stdin,
		openStore: store.Open, now: time.Now, getenv: os.Getenv})
}

// doctorUsage is the usage line's `doctor` row. The count is the doctor's own
// list (doctorChecks), so the line cannot drift from what the doctor runs —
// it named seven checks while the doctor ran nineteen (a nit sweep's
// `[docs, cli]` row).
func doctorUsage() string {
	return fmt.Sprintf("preflight: %d checks across configuration, storage, ffmpeg and the integrations (needs no database)", len(doctorChecks))
}

func adminUsage(w io.Writer) {
	fmt.Fprint(w, `usage: clippet admin <subcommand> [args]
  doctor                                 `+doctorUsage()+`
  expire <id> [--ago D | --at RFC3339]   set the clip's expires_at (default: now); the sweeper destroys it on its next tick
  import <dir> --owner <username> [--recursive] [--dry-run]
                                         one-time: walk a folder of existing videos through this instance's own upload path and
                                         make each one a clip owned by that account. The files are READ — nothing in that folder
                                         is ever moved, renamed or deleted — and a second run over the same folder imports
                                         nothing (it recognises the content it already holds). --dry-run prints the verdict for
                                         every file and imports nothing (DEPLOY.md §34.4)
  ls [<id>] [--stat] [--state s] [--owner name]
                                         list every clip (--state/--owner narrow it), or print one clip's row, keys and latest job
  stats [--no-storage]                   users, clips by state, queue depth, the storage quota and which of the two
                                         things it is (advisory or a cap), the bytes served today and this month
                                         (with a cost estimate when priced), free disk and (unless --no-storage)
                                         bucket usage under clips/
  jobs [--kind k] [--state s] [--clip id] [--since D] [--limit n]
                                         the job table, newest first, with a per-kind/state summary
  jobs unstick <job-id> | --older-than D repair a job left `+"`running`"+` with no worker: released if it checkpointed, else a crash attempt
  retry <id>… | --all-failed             re-queue a FAILED clip's transcode (the original must still be in the bucket)
  orphans [--delete] [--min-age D] [--clip id]
                                         bucket prefixes without a row (removed with --delete), rows whose objects are missing (never
                                         removed), and objects under a LIVE clip that no column names (removed with --delete);
                                         --clip lists ONE clip's prefix and scopes all three reports (and the delete) to it
  objects <id> [--count]                 what is in the store under clips/<id>/: key, size and modified time per object, then a
                                         count (--count prints the number alone). It asks the BUCKET and never the database,
                                         so it answers for a clip whose row has already gone; "objects _clippet" lists the
                                         instance's own reserved prefix (the database snapshot) under its own heading
  objects --prefix <prefix> [--count]    the same listing from the bucket's root, not under S3_KEY_PREFIX
  objects init | put <key> | rm <key> | rm --prefix <prefix>
                                         create the bucket if it is missing; store stdin under a key; remove one key, or
                                         a test probe's whole e2e-<word>-<epoch>/ prefix (keys are from the bucket's root)
  users                                  every user: name, registered, clips, collections, live tokens, linked Discord id, owner
  users create <username>                create an account; the password is read from stdin, never from the command line
  users password <username>              set the account's password (stdin, never the command line) and sign it out of
                                         every browser — the recovery verb, also for an account /signin made with none
  users link <username> <discord-id>     let that Discord user sign in to the account (DEPLOY.md §19.6); refused if either
                                         side already has a link — one explicit change at a time; users unlink first
  users unlink <username>                remove the account's Discord link; the password and every session stay;
                                         refused while a person binding names the account (bot unbind first)
  users owner <username>                 make that account this instance's owner (the one account that may open
                                         /setup (unless SETUP_WIZARD=off) and read the Storage panel; the first
                                         account is the owner by itself)
  users prune <pattern> --older-than D --no-clips
                                         delete users whose NAME matches <pattern> (SQL LIKE: % is any run), who registered
                                         more than D ago and who own NO clips and NO collections; --no-clips is required
  collections ls [--owner <username>]    every collection: id, owner, clips, created, title
  collections rm <id> [--owner <username>]
                                         delete that COLLECTION ROW only — its clips are never touched (the one thing
                                         a users prune cannot clear on its own)
  sessions list <username>               that user's live browser sessions (started, last seen, expires) — never a cookie value
  sessions revoke <username> | --all     sign one user, or everybody, out of every browser at once
  tokens list <username>                 that user's live API tokens: id, name, prefix, scopes, created, last used
  tokens revoke <username> <tid> | <username> --all
                                         revoke one API token, or every live token of that user; a selector is required
  tokens mint <username> [--name n] [--scopes upload[,read]|inbound|autofetch] [--sxcu]
                                         mint a capture token for that user without a browser: the raw token is on STDOUT and
                                         is shown ONCE, every other word on stderr. --sxcu prints the ShareX file instead
                                         (mint --sxcu > clippet.sxcu is an importable uploader); --scopes inbound prints an
                                         inbound URL instead, --scopes autofetch a Clippet Auto URL. DEPLOY.md §9
  bot bind <guild-id> <username> [--person] [--no-register]
                                         connect a Discord server to an account and publish the slash command there (DEPLOY.md §30);
                                         --person: for the account's linked Discord user alone (users link first; §30.8 step 3b)
  bot unbind <guild-id>                  disconnect it: the slash command in that server is refused again
  bot ls                                 every connected server: guild id, account, when it was bound, server|person
  bot register [<guild-id>]              (re)publish the slash command in one bound server, or in all of them
  storage migrate --to fs|s3 [--dry-run] copy every live clip's objects from the store this instance uses to the OTHER one, verify each
                                         one, and print a summary. The source is NEVER touched; you flip STORAGE_BACKEND afterwards (DEPLOY.md §32)
    [--manifest <path>] [--stop-file <path>] [--max-bytes N]   bucket to bucket with MIGRATE_S3_*; a stop or the cap is exit 3 (DEPLOY.md §32.3)
  backup <path>                          consistent snapshot of the live database to <path> (never overwrites; copy it out of the container)
  backup <dir> [--rotate N]              …or into a directory as clippet-<UTC>.db, keeping the newest N (nothing it did not name is touched)
  restore-check <path>                   open a backup read-only: integrity check, applied schema and row counts (needs no configuration)
  restore --from-store [--yes]           fetch the database snapshot the bucket holds (_clippet/db.sqlite), verify its sha256 and print
                                         what it found; --yes moves the current database aside as clippet.db.before-restore-<UTC> and
                                         puts the snapshot in place. The server must be stopped (DEPLOY.md §15)
`)
}

func runAdminWith(args []string, cfg config.Config, env adminEnv) int {
	if len(args) == 0 {
		adminUsage(env.stderr)
		return 2
	}
	switch args[0] {
	case "help", "-h", "--help":
		adminUsage(env.stdout)
		return 0
	case "restore-check":
		// Deliberately before the database is opened: the file being checked
		// is the argument, and the container running this may have no
		// database, no bucket and no `.env` at all (§20 erratum 83).
		return restoreCheck(args[1:], env)
	case "doctor":
		// Also before the database: doctor REPORTS on the database rather than
		// needing one, and `main` already routes it past config.Load so a
		// broken `.env` is something it can describe instead of dying of
		// (§20 erratum 143). Reaching it here means the configuration parsed.
		getenv := env.getenv
		if getenv == nil {
			getenv = os.Getenv
		}
		return runDoctor(args[1:], getenv, env)
	case "objects":
		// Deliberately before the database too, and for a reason of its own
		// (§20 erratum 281): this is a question about the BUCKET, and the case it
		// exists for — "is this deleted clip's prefix really empty" — is one where
		// there is no row left to open the database for. Requiring one would make
		// the verb useless exactly where it is wanted.
		return adminObjects(args[1:], cfg, env)
	case "restore":
		// Before the database as well (§20 erratum 343): the machine this verb
		// exists for — a new PC with only the bucket's keys — has no database
		// yet, and on one that has, the verb must hold DATA_DIR's lock before
		// anything opens the file it is about to replace.
		return adminRestore(args[1:], cfg, env)
	case "expire", "ls", "stats", "jobs", "retry", "orphans", "backup", "users", "collections", "sessions", "tokens", "bot", "storage", "import":
	default:
		fmt.Fprintf(env.stderr, "admin: unknown subcommand %q\n", args[0])
		adminUsage(env.stderr)
		return 2
	}
	path := dbPath(cfg)
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(env.stderr, "admin: no database at %s (run `clippet migrate` or `clippet serve` first)\n", path)
		return 2
	}
	database, err := db.Open(path)
	if err != nil {
		fmt.Fprintln(env.stderr, "admin: open:", err)
		return 1
	}
	defer database.Close()
	budget := env.timeout
	if budget <= 0 {
		budget = adminTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	a := &admin{cfg: cfg, db: database, env: env, ctx: ctx}
	switch args[0] {
	case "expire":
		return a.expire(args[1:])
	case "ls":
		return a.ls(args[1:])
	case "stats":
		return a.stats(args[1:])
	case "jobs":
		return a.jobs(args[1:])
	case "retry":
		return a.retry(args[1:])
	case "backup":
		return a.backup(args[1:])
	case "users":
		return a.users(args[1:])
	case "collections":
		return a.collections(args[1:])
	case "sessions":
		return a.sessions(args[1:])
	case "tokens":
		return a.tokens(args[1:])
	case "bot":
		return a.bot(args[1:])
	case "storage":
		return a.storage(args[1:])
	case "import":
		return a.importDir(args[1:])
	default:
		return a.orphans(args[1:])
	}
}

type admin struct {
	cfg config.Config
	db  *db.DB
	env adminEnv
	ctx context.Context
	st  store.Store // opened on first use
	// beforeVacuum, when set, runs on the snapshot's `.part-` path after the
	// part is created and before `VACUUM INTO` writes it. A test seam only
	// (nil in every production path): it is how a test reads the part's mode
	// at creation and forces the VACUUM to fail (§20 erratum 321 (k)).
	beforeVacuum func(part string)
}

func (a *admin) store() (store.Store, error) {
	if a.st != nil {
		return a.st, nil
	}
	st, err := a.env.openStore(withAdminTimeout(a.cfg))
	if err != nil {
		return nil, err
	}
	a.st = st
	return st, nil
}

// withAdminTimeout floors STORAGE_OP_TIMEOUT for the CLI (see adminOpTimeout).
// It is a copy: nothing the CLI does may change the configuration the rest of
// the process reads.
func withAdminTimeout(cfg config.Config) config.Config {
	if cfg.S3.OpTimeout < adminOpTimeout {
		cfg.S3.OpTimeout = adminOpTimeout
	}
	return cfg
}

func (a *admin) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("clippet admin "+name, flag.ContinueOnError)
	fs.SetOutput(a.env.stderr)
	return fs
}

// parseWithID parses fs from args, allowing at most one positional <id>
// before, between or after the flags (`expire <id> --ago 2h` and
// `expire --ago 2h <id>` both work). A second positional is an error.
func parseWithID(fs *flag.FlagSet, args []string) (string, error) {
	id := ""
	for {
		if err := fs.Parse(args); err != nil {
			return "", err
		}
		if fs.NArg() == 0 {
			return id, nil
		}
		if id != "" {
			return "", fmt.Errorf("unexpected argument %q", fs.Arg(0))
		}
		id, args = fs.Arg(0), fs.Args()[1:]
	}
}

func (a *admin) notFound(id string) int {
	fmt.Fprintf(a.env.stdout, "%s: not found\n", id)
	return 1
}

func (a *admin) fail(what string, err error) int {
	fmt.Fprintf(a.env.stderr, "admin %s: %v\n", what, err)
	return 1
}

// --- expire ----------------------------------------------------------------

func (a *admin) expire(args []string) int {
	fs := a.flags("expire")
	ago := fs.Duration("ago", 0, "set expires_at this long ago (e.g. 2h)")
	at := fs.String("at", "", "set expires_at to this RFC3339 time")
	id, err := parseWithID(fs, args)
	if err != nil || id == "" || (*at != "" && *ago != 0) {
		fmt.Fprintln(a.env.stderr, "usage: clippet admin expire <id> [--ago D | --at RFC3339]")
		return 2
	}
	when := a.env.now().Add(-*ago)
	if *at != "" {
		t, err := time.Parse(time.RFC3339, *at)
		if err != nil {
			fmt.Fprintf(a.env.stderr, "admin expire: --at %q is not RFC3339: %v\n", *at, err)
			return 2
		}
		when = t
	}
	if !ids.Valid(id) {
		return a.notFound(id)
	}
	err = a.db.ForceExpire(a.ctx, id, when.Unix())
	switch {
	case errors.Is(err, db.ErrNotFound):
		return a.notFound(id)
	case err != nil:
		return a.fail("expire", err)
	}
	fmt.Fprintf(a.env.stdout, "expired %s expires_at=%s\n", id, rfc(when.Unix()))
	return 0
}

// --- ls --------------------------------------------------------------------

func (a *admin) ls(args []string) int {
	fs := a.flags("ls")
	stat := fs.Bool("stat", false, "check each key in the bucket")
	state := fs.String("state", "", "only clips in this state: queued|processing|ready|failed|deleting")
	owner := fs.String("owner", "", "only clips owned by this username")
	id, err := parseWithID(fs, args)
	if err != nil {
		a.lsUsage()
		return 2
	}
	if id != "" && (*state != "" || *owner != "") {
		fmt.Fprintln(a.env.stderr, "admin ls: --state and --owner filter the LISTING; they cannot be combined with a single <id>")
		a.lsUsage()
		return 2
	}
	if id == "" {
		return a.lsAll(*state, *owner)
	}
	if !ids.Valid(id) {
		return a.notFound(id)
	}
	c, err := a.db.GetClip(a.ctx, id)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return a.notFound(id)
	case err != nil:
		return a.fail("ls", err)
	}
	a.printClip(c)
	j, err := a.db.GetLatestJob(a.ctx, id)
	switch {
	case errors.Is(err, db.ErrNotFound):
		fmt.Fprintln(a.env.stdout, "job:       (none)")
	case err != nil:
		return a.fail("ls", err)
	default:
		fmt.Fprintf(a.env.stdout, "job:       #%d kind=%s state=%s stage=%s attempts=%d transient=%d run_after=%s checkpoint=%v started=%s finished=%s last_error=%s\n",
			j.ID, j.Kind, j.State, j.Stage, j.Attempts, j.TransientFailures, rfc(j.RunAfter), j.CheckpointJSON != nil,
			rfcp(j.StartedAt), rfcp(j.FinishedAt), strp(j.LastError))
	}
	if !*stat {
		return 0
	}
	st, err := a.store()
	if err != nil {
		return a.fail("ls", err)
	}
	rc := 0
	// The same label `stats` uses (§20 erratum 276(l)): on `fs` there is no
	// bucket, and a row that said otherwise would be the one place this feature
	// lied. Both spellings are eight characters, so the columns do not move.
	label := a.storeLabel()
	for _, key := range clipObjectKeys(c) {
		info, err := st.Stat(a.ctx, key)
		switch {
		case errors.Is(err, store.ErrNotFound):
			fmt.Fprintf(a.env.stdout, "%s   MISSING %s\n", label, key)
		case err != nil:
			fmt.Fprintf(a.env.stdout, "%s   error %s: %v\n", label, key, err)
			rc = 1
		default:
			// A filesystem store reports no ETag (§20 erratum 276: inventing
			// one would mean hashing the object on every `ls --stat`), and an
			// empty field beside a populated one reads as a bug rather than as
			// an absence — so it is spelled.
			etag := info.ETag
			if etag == "" {
				etag = "(not stored)"
			}
			fmt.Fprintf(a.env.stdout, "%s   present %s size=%d etag=%s modified=%s\n", label, key, info.Size, etag, info.LastModified.UTC().Format(time.RFC3339))
		}
	}
	return rc
}

// storeLabel is the column every per-object line of the CLI starts with:
// `bucket:` where there is a bucket, `store:` where there is a directory (§20
// erratum 276(l)). Both are padded to eight characters so no other column
// moves with the backend.
func (a *admin) storeLabel() string {
	if a.cfg.StorageBackend == config.BackendFS {
		return "store:  "
	}
	return "bucket: "
}

func (a *admin) lsUsage() {
	fmt.Fprintln(a.env.stderr, "usage: clippet admin ls [<id>] [--stat]")
	fmt.Fprintln(a.env.stderr, "       clippet admin ls [--state queued|processing|ready|failed|deleting] [--owner <username>]")
}

// lsAll lists the clip table, newest first. The two filters (ROADMAP #10) are
// applied here rather than in SQL: this listing already reads every row to sort
// it, the whole table is a few thousand rows on the instances this design
// targets, and keeping the query untouched keeps `admin ls` free of a second
// definition of what a clip listing is.
//
// An unknown --state is a usage error rather than an empty listing: "no clips
// in state redy" is the wrong answer to a typo.
func (a *admin) lsAll(state, owner string) int {
	if state != "" && !validClipState(state) {
		fmt.Fprintf(a.env.stderr, "admin ls: --state %q is not a clip state (queued|processing|ready|failed|deleting)\n", state)
		return 2
	}
	ownerID := ""
	if owner != "" {
		u, ok := a.resolveUser(owner)
		if !ok {
			return 1
		}
		ownerID = u.ID
	}
	rows, err := a.db.ListAllClipIDsAndKeys(a.ctx)
	if err != nil {
		return a.fail("ls", err)
	}
	clips := make([]db.Clip, 0, len(rows))
	for _, r := range rows {
		if state != "" && r.State != state {
			continue
		}
		c, err := a.db.GetClip(a.ctx, r.ID)
		if errors.Is(err, db.ErrNotFound) {
			continue // deleted between the two reads
		}
		if err != nil {
			return a.fail("ls", err)
		}
		if ownerID != "" && c.OwnerID != ownerID {
			continue
		}
		clips = append(clips, c)
	}
	sort.Slice(clips, func(i, j int) bool {
		if clips[i].CreatedAt != clips[j].CreatedAt {
			return clips[i].CreatedAt > clips[j].CreatedAt
		}
		return clips[i].ID < clips[j].ID
	})
	if len(clips) == 0 {
		fmt.Fprintf(a.env.stdout, "no clips%s\n", lsFilterSuffix(state, owner))
		return 0
	}
	fmt.Fprintf(a.env.stdout, "%-12s %-10s %-20s %-20s %s\n", "ID", "STATE", "EXPIRES", "CREATED", "TITLE")
	for _, c := range clips {
		fmt.Fprintf(a.env.stdout, "%-12s %-10s %-20s %-20s %s\n", c.ID, c.State, expiresp(c.ExpiresAt), rfc(c.CreatedAt), c.Title)
	}
	fmt.Fprintf(a.env.stdout, "%d clips%s\n", len(clips), lsFilterSuffix(state, owner))
	return 0
}

// lsFilterSuffix names the filter on the count line, so a short listing can
// never be mistaken for an empty instance.
func lsFilterSuffix(state, owner string) string {
	var parts []string
	if state != "" {
		parts = append(parts, "state="+state)
	}
	if owner != "" {
		parts = append(parts, "owner="+owner)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, " ") + ")"
}

// validClipState is the closed §5 set.
func validClipState(s string) bool {
	switch s {
	case db.ClipQueued, db.ClipProcessing, db.ClipReady, db.ClipFailed, db.ClipDeleting:
		return true
	}
	return false
}

// validJobState is the closed §5 job set.
func validJobState(s string) bool {
	switch s {
	case db.JobQueued, db.JobRunning, db.JobDone, db.JobFailed, db.JobCancelled:
		return true
	}
	return false
}

// validJobKind is the closed set of kinds the worker knows about.
// jobKinds is the vocabulary of `jobs.kind` in the order `admin jobs` prints
// its per-kind totals: the transcode first because it is the only kind
// coupled to clips.state, then the announcement, then the media kinds that
// re-derive an object of an already-ready clip (§8.4, DESIGN.md §5), and the
// clip webhook last (§20 erratum 319). It is the single list every kind flag,
// help string and summary reads, so another kind is added in one place rather
// than five.
var jobKinds = []string{db.KindTranscode, db.KindNotify, db.KindTrim, db.KindGIF,
	db.KindSocial, db.KindReplace, db.KindPoster, db.KindFilmstrip, db.KindFull, db.KindHook}

// jobKindList is the vocabulary as an operator sees it in usage text.
var jobKindList = strings.Join(jobKinds, "|")

func validJobKind(s string) bool {
	for _, k := range jobKinds {
		if s == k {
			return true
		}
	}
	return false
}

func (a *admin) printClip(c db.Clip) {
	w := a.env.stdout
	fmt.Fprintf(w, "clip:      %s\n", c.ID)
	fmt.Fprintf(w, "state:     %s", c.State)
	if c.FailureReason != nil {
		fmt.Fprintf(w, " failure=%q", *c.FailureReason)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "title:     %s\n", c.Title)
	fmt.Fprintf(w, "owner:     %s unlisted=%v views=%d\n", c.OwnerID, c.Unlisted, c.Views)
	fmt.Fprintf(w, "created:   %s updated=%s ready=%s\n", rfc(c.CreatedAt), rfc(c.UpdatedAt), rfcp(c.ReadyAt))
	fmt.Fprintf(w, "expires:   %s ttl=%s\n", expiresp(c.ExpiresAt), ttlp(c.TTLSeconds))
	fmt.Fprintf(w, "source:    %s size=%d type=%s etag=%s filename=%q\n", c.SourceKey, c.SourceSize, c.SourceContentType, c.SourceETag, c.SourceFilename)
	if c.RenditionKey != nil {
		fmt.Fprintf(w, "rendition: %s size=%d etag=%s\n", *c.RenditionKey, i64p(c.RenditionSize), strp(c.RenditionETag))
	} else if c.OriginalPlayable && !c.OriginalRemuxed {
		// The remux fast path: there IS no rendition object, and that is the
		// feature rather than a gap — /video.mp4 streams the upload (§20 erratum
		// 201). Saying "(none)" alone would read as a broken clip.
		fmt.Fprintln(w, "rendition: (none: the original is web-playable and is served as it stands)")
	} else {
		fmt.Fprintln(w, "rendition: (none)")
	}
	if c.PosterKey != nil {
		fmt.Fprintf(w, "poster:    %s size=%d etag=%s dims=%dx%d\n", *c.PosterKey, i64p(c.PosterSize), strp(c.PosterETag), intp(c.PosterWidth), intp(c.PosterHeight))
	} else {
		fmt.Fprintln(w, "poster:    (none)")
	}
	if c.SocialKey != nil {
		fmt.Fprintf(w, "social:    %s size=%d etag=%s dims=%dx%d fps=%d/%d\n", *c.SocialKey, i64p(c.SocialSize), strp(c.SocialETag),
			intp(c.SocialWidth), intp(c.SocialHeight), intp(c.SocialFPSNum), intp(c.SocialFPSDen))
	} else {
		fmt.Fprintln(w, "social:    (none: the card points at the playable file itself)")
	}
	if c.GIFKey != nil {
		fmt.Fprintf(w, "gif:       %s size=%d etag=%s dims=%dx%d\n", *c.GIFKey, i64p(c.GIFSize), strp(c.GIFETag), intp(c.GIFWidth), intp(c.GIFHeight))
	} else {
		fmt.Fprintln(w, "gif:       (none: preview.gif generates one on demand)")
	}
	if c.FilmstripKey != nil {
		fmt.Fprintf(w, "filmstrip: %s size=%d etag=%s tiles=%d tile=%dx%d interval_ms=%d\n",
			*c.FilmstripKey, i64p(c.FilmstripSize), strp(c.FilmstripETag), intp(c.FilmstripTiles),
			intp(c.FilmstripTileW), intp(c.FilmstripTileH), i64p(c.FilmstripIntervalMS))
	} else {
		fmt.Fprintln(w, "filmstrip: (none: filmstrip.jpg generates one on demand)")
	}
	// The clip's kind (§20 erratum 328). A row of its own rather than a column of
	// the listing, whose shape the E2E's own sweep reads by position.
	if c.IsAudio() {
		fmt.Fprintf(w, "kind:      audio codec=%s sample_rate=%d bit_depth=%d channels=%d\n",
			c.AudioCodec, intp(c.SampleRate), intp(c.BitDepth), intp(c.AudioChannels))
	} else {
		fmt.Fprintln(w, "kind:      video")
	}
	fmt.Fprintf(w, "media:     %dx%d fps=%d/%d duration_ms=%d has_audio=%s\n",
		intp(c.Width), intp(c.Height), intp(c.FPSNum), intp(c.FPSDen), i64p(c.DurationMS), boolp(c.HasAudio))
	// The quality tiers (§20 errata 200-206). It is one line because it answers
	// one question — "why does this clip have the objects it has" — and the two
	// tier states are the whole of it: `full=skipped` beside `playable=true` is a
	// clip that needed no encode at all, and `full=encoding` is one whose card is
	// already right while the better copy runs.
	fmt.Fprintf(w, "tiers:     original playable=%v remuxed=%v %dx%d fps=%d/%d codec=%s · full=%s%s · light=%s\n",
		c.OriginalPlayable, c.OriginalRemuxed,
		intp(c.OriginalWidth), intp(c.OriginalHeight), intp(c.OriginalFPSNum), intp(c.OriginalFPSDen),
		orNone(c.OriginalCodec), orUnknown(c.FullState), codecSuffix(c.FullCodec), orUnknown(c.LightState))
	// The trim contract (§20 erratum 68), which is the pair an operator needs
	// to make sense of a duration that does not match the upload: the
	// ORIGINAL's length, and the window of it this rendition was cut from.
	// A source length of 0 means the original has not been measured yet — a
	// row from before migration 0003 that has already been trimmed.
	switch {
	case c.TrimStartMS != nil && c.TrimEndMS != nil:
		fmt.Fprintf(w, "trim:      %d..%d ms of the original (source_duration_ms=%d)\n",
			*c.TrimStartMS, *c.TrimEndMS, i64p(c.SourceDurationMS))
	case c.SourceDurationMS != nil:
		fmt.Fprintf(w, "trim:      (none: the whole original, %d ms)\n", *c.SourceDurationMS)
	default:
		fmt.Fprintln(w, "trim:      (none; the original has not been measured)")
	}
}

// clipObjectKeys lists the keys a row references. It is db.KeysOf().Keys()
// and nothing else on purpose: `ls --stat` and `orphans` used to build that
// list separately, `orphans` forgot social_key, and the two would have drifted
// again the moment gif_key started being filled. One definition, both
// subcommands (a nit sweep, round 6).
func clipObjectKeys(c db.Clip) []string { return db.KeysOf(c).Keys() }

// --- users -----------------------------------------------------------------

// users is the inventory (`admin users`) and its one destructive verb
// (`admin users prune`). §20 erratum 86; DEPLOY §8.
func (a *admin) users(args []string) int {
	if len(args) > 0 && args[0] == "prune" {
		return a.usersPrune(args[1:])
	}
	if len(args) > 0 && args[0] == "create" {
		return a.usersCreate(args[1:])
	}
	if len(args) > 0 && args[0] == "password" {
		return a.usersPassword(args[1:])
	}
	if len(args) > 0 && args[0] == "link" {
		return a.usersLink(args[1:])
	}
	if len(args) > 0 && args[0] == "unlink" {
		return a.usersUnlink(args[1:])
	}
	if len(args) > 0 && args[0] == "owner" {
		return a.usersOwner(args[1:])
	}
	fs := a.flags("users")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(a.env.stderr, "usage: clippet admin users            (or: clippet admin users prune <pattern> --older-than D --no-clips)")
		return 2
	}
	rows, err := a.db.ListUserSummaries(a.ctx)
	if err != nil {
		return a.fail("users", err)
	}
	a.printUsers(rows)
	fmt.Fprintf(a.env.stdout, "%d user(s)\n", len(rows))
	return 0
}

// usersPrune deletes users who own nothing. The three guards are deliberate
// and none of them has a default:
//
//   - <pattern> is required and may not be blank. This is the only command in
//     the binary that removes a user, and "no pattern" must not be spellable
//     as "all of them" -- an operator who means everybody types `%`.
//   - --older-than is required. Whatever the pattern matches, somebody who
//     registered while the command was being typed is not in it.
//   - --no-clips is required and takes no other value. It reads as the
//     assertion the command actually makes: only users with zero clips and
//     zero collections are ever removed, because `clips.owner_id` CASCADEs
//     from `users` and deleting an owner would drop the rows while their
//     objects stayed in the bucket with nothing left to name them.
//
// Sessions and API tokens go with each user through their own cascade.
func (a *admin) usersPrune(args []string) int {
	fs := a.flags("users prune")
	olderThan := fs.Duration("older-than", 0, "only users who registered longer ago than this (e.g. 24h)")
	noClips := fs.Bool("no-clips", false, "required: acknowledges that only users owning no clips and no collections are removed")
	pattern, err := parseWithID(fs, args)
	if err != nil || strings.TrimSpace(pattern) == "" || *olderThan <= 0 || !*noClips {
		fmt.Fprintln(a.env.stderr, "usage: clippet admin users prune <pattern> --older-than D --no-clips")
		fmt.Fprintln(a.env.stderr, "  <pattern> is a SQL LIKE pattern over the username (% is any run, _ is one character);")
		fmt.Fprintln(a.env.stderr, "  it may not be empty, --older-than must be positive, and --no-clips is required.")
		return 2
	}
	before := a.env.now().Add(-*olderThan)
	gone, err := a.db.PruneUsers(a.ctx, pattern, before.Unix())
	switch {
	case errors.Is(err, db.ErrInvalidPattern):
		fmt.Fprintln(a.env.stderr, "admin users prune: the pattern is empty")
		return 2
	case err != nil:
		return a.fail("users prune", err)
	}
	a.printUsers(gone)
	fmt.Fprintf(a.env.stdout, "pruned %d user(s) matching %q registered before %s\n", len(gone), pattern, rfc(before.Unix()))
	return 0
}

// usersOwner moves the instance's owner role (§20 erratum 349): the account
// that may open `/setup` (unless SETUP_WIZARD=off) and read the Storage panel. The first account created
// is the owner by itself (migration 0025), and a deleted owner hands it to the
// oldest remaining account; this verb is for the operator whose first account
// was not the one they meant — a test account, a friend's.
func (a *admin) usersOwner(args []string) int {
	fs := a.flags("users owner")
	name, err := parseWithID(fs, args)
	if err != nil || strings.TrimSpace(name) == "" {
		fmt.Fprintln(a.env.stderr, "usage: clippet admin users owner <username>")
		return 2
	}
	u, err := a.db.SetOwner(a.ctx, name)
	switch {
	case errors.Is(err, db.ErrNotFound):
		fmt.Fprintf(a.env.stderr, "admin users owner: no account named %q\n", name)
		return 1
	case err != nil:
		return a.fail("users owner", err)
	}
	fmt.Fprintf(a.env.stdout, "owner: %s is this instance's owner\n", u.Username)
	return 0
}

// printUsers is the one row format both verbs print, so what `users` shows
// and what `users prune` reports it removed cannot drift apart.
func (a *admin) printUsers(rows []db.UserSummary) {
	for _, u := range rows {
		discord := u.DiscordID
		if discord == "" {
			discord = "-"
		}
		// owner= is LAST (§20 erratum 349): the E2E's and the visual pass's
		// sweeps read the name by position, and the Discord checks grep
		// `discord=<id>` as a substring.
		owner := "no"
		if u.Owner {
			owner = "yes"
		}
		fmt.Fprintf(a.env.stdout, "%s  %s  created=%s clips=%d collections=%d tokens=%d discord=%s owner=%s\n",
			u.ID, u.Username, rfc(u.CreatedAt), u.Clips, u.Collections, u.Tokens, discord, owner)
	}
}

// usersLink is the operator's half of "Sign in with Discord" (ROADMAP #44, §20
// erratum 302): `admin users link <username> <discord-id>` lets that Discord
// user open that account. The other half is the owner, from Settings, with
// their password. Both refuse an id already linked to another account and a
// second link on an account that has one — one explicit state change at a
// time, never a silent replacement. This is the ONE surface that names the
// OTHER account an id is linked to: the operator sees every account anyway,
// and the wire answer never does.
func (a *admin) usersLink(args []string) int {
	fs := a.flags("users link")
	pos, err := parsePositionals(fs, args, 2)
	if err != nil || len(pos) != 2 {
		fmt.Fprintln(a.env.stderr, "usage: clippet admin users link <username> <discord-id>")
		fmt.Fprintln(a.env.stderr, "  <discord-id> is the Discord USER's id (Developer Mode → right-click the user → Copy User ID), 15-25 digits.")
		return 2
	}
	name, id := strings.TrimSpace(pos[0]), strings.TrimSpace(pos[1])
	if !validGuildID(id) {
		fmt.Fprintf(a.env.stderr, "admin users link: %q is not a Discord user id (15-25 digits)\n", id)
		return 2
	}
	u, ok := a.resolveUser(name)
	if !ok {
		return 1
	}
	if u.DiscordID != nil {
		fmt.Fprintf(a.env.stderr, "admin users link: %s is already linked to Discord user %s - run `clippet admin users unlink %s` first\n", u.Username, *u.DiscordID, u.Username)
		return 1
	}
	if other, err := a.db.GetUserByDiscordID(a.ctx, id); err == nil {
		fmt.Fprintf(a.env.stderr, "admin users link: Discord user %s is already linked to %s\n", id, other.Username)
		return 1
	} else if !errors.Is(err, db.ErrNotFound) {
		return a.fail("users link", err)
	}
	if err := a.db.LinkDiscord(a.ctx, u.ID, id, "", a.env.now().Unix()); err != nil {
		if errors.Is(err, db.ErrConflict) {
			// The two reads above raced a link made between them.
			fmt.Fprintf(a.env.stderr, "admin users link: a link was made under this command; re-run `clippet admin users` and try again\n")
			return 1
		}
		return a.fail("users link", err)
	}
	fmt.Fprintf(a.env.stdout, "linked %s  %s  discord=%s (Sign in with Discord opens this account for that user from now on)\n", u.ID, u.Username, id)
	return 0
}

// usersUnlink removes an account's link and reports whether there was one,
// like `bot unbind`. The password and every session stay: it removes a way
// in, not the browsers already in.
//
// It is refused while a person binding names the account (`admin bot bind
// --person`, §20 erratum 342): that binding answers for the linked id alone,
// and without one it would refuse every member of the server, silently. The
// operator unbinds first, in one command the refusal names.
func (a *admin) usersUnlink(args []string) int {
	fs := a.flags("users unlink")
	name, err := parseWithID(fs, args)
	if err != nil || strings.TrimSpace(name) == "" {
		fmt.Fprintln(a.env.stderr, "usage: clippet admin users unlink <username>")
		return 2
	}
	u, ok := a.resolveUser(strings.TrimSpace(name))
	if !ok {
		return 1
	}
	bindings, err := a.db.ListGuildBindings(a.ctx)
	if err != nil {
		return a.fail("users unlink", err)
	}
	for _, g := range bindings {
		if g.UserID == u.ID && g.Person() {
			fmt.Fprintf(a.env.stderr, "admin users unlink: %s has a person binding (guild %s) - run `clippet admin bot unbind %s` first\n", u.Username, g.GuildID, g.GuildID)
			return 1
		}
	}
	had, err := a.db.UnlinkDiscord(a.ctx, u.ID)
	if err != nil {
		return a.fail("users unlink", err)
	}
	if !had {
		fmt.Fprintf(a.env.stdout, "%s: no Discord link\n", u.Username)
		return 0
	}
	fmt.Fprintf(a.env.stdout, "%s unlinked from Discord user %s (the password still works; no session was touched)\n", u.Username, *u.DiscordID)
	return 0
}

// usersCreate adds an account from the CLI (ROADMAP #21, §20 erratum 106). It is
// what `REGISTRATION=first` leans on: the door closes after the first
// account, and this is how the operator lets the next friend in without
// reopening it to the internet for the minutes in between.
//
// The password is read from STDIN and never from argv. A command line is
// written into shell history, is visible to every `ps` on the host, and is
// echoed by `docker compose exec` in its own logs; a pipe is none of those.
// Interactively that means:
//
//	read -rsp 'password: ' PW; echo; printf '%s' "$PW" | \
//	  docker compose exec -T app clippet admin users create alice
//
// The rules are the API's, to the byte, because they are the same account:
// the username regexp of §14 and the 8–72 byte bcrypt-truncation guard.
func (a *admin) usersCreate(args []string) int {
	fs := a.flags("users create")
	name, err := parseWithID(fs, args)
	if err != nil || strings.TrimSpace(name) == "" {
		a.usersCreateUsage()
		return 2
	}
	if !usernameOK(name) {
		fmt.Fprintln(a.env.stderr, "admin users create: username must be 3-32 characters of letters, digits or underscore")
		return 2
	}
	if a.env.stdin == nil {
		fmt.Fprintln(a.env.stderr, "admin users create: no stdin to read the password from")
		return 2
	}
	raw, err := io.ReadAll(io.LimitReader(a.env.stdin, 1<<10))
	if err != nil {
		return a.fail("users create", err)
	}
	// One trailing newline is the shell's, not the operator's; anything else
	// is kept, because a password may legitimately contain spaces.
	password := strings.TrimRight(string(raw), "\r\n")
	if len(password) < 8 || len(password) > 72 {
		fmt.Fprintln(a.env.stderr, "admin users create: password must be 8-72 bytes (read from stdin)")
		return 2
	}
	if _, err := a.db.GetUserByUsername(a.ctx, name); err == nil {
		fmt.Fprintf(a.env.stderr, "admin users create: %q is taken\n", name)
		return 1
	} else if !errors.Is(err, db.ErrNotFound) {
		return a.fail("users create", err)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return a.fail("users create", err)
	}
	u, err := a.db.CreateUser(a.ctx, ids.New(), name, hash, a.env.now().Unix())
	if err != nil {
		if errors.Is(err, db.ErrConflict) {
			fmt.Fprintf(a.env.stderr, "admin users create: %q is taken\n", name)
			return 1
		}
		return a.fail("users create", err)
	}
	fmt.Fprintf(a.env.stdout, "created %s  %s  created=%s\n", u.ID, u.Username, rfc(u.CreatedAt))
	return 0
}

// usersPassword is the recovery verb (§20 erratum 341; DEPLOY §19): `admin
// users password <username>` sets the account's password from STDIN, exactly as
// `users create` reads one, and signs the account out of EVERY browser in the
// same transaction — a password change's rule (§19.2), with no browser of the
// operator's to keep. It is how somebody who forgot their password gets back in,
// and how an account `/signin` made with no password gains one.
func (a *admin) usersPassword(args []string) int {
	fs := a.flags("users password")
	name, err := parseWithID(fs, args)
	if err != nil || strings.TrimSpace(name) == "" {
		a.usersPasswordUsage()
		return 2
	}
	if a.env.stdin == nil {
		fmt.Fprintln(a.env.stderr, "admin users password: no stdin to read the password from")
		return 2
	}
	raw, err := io.ReadAll(io.LimitReader(a.env.stdin, 1<<10))
	if err != nil {
		return a.fail("users password", err)
	}
	// One trailing newline is the shell's, not the operator's (users create's rule).
	password := strings.TrimRight(string(raw), "\r\n")
	if len(password) < 8 || len(password) > 72 {
		fmt.Fprintln(a.env.stderr, "admin users password: password must be 8-72 bytes (read from stdin)")
		return 2
	}
	u, ok := a.resolveUser(strings.TrimSpace(name))
	if !ok {
		return 1
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return a.fail("users password", err)
	}
	removed, err := a.db.SetPassword(a.ctx, u.ID, hash, "")
	switch {
	case errors.Is(err, db.ErrNotFound):
		fmt.Fprintf(a.env.stdout, "%s: no such user\n", u.Username)
		return 1
	case err != nil:
		return a.fail("users password", err)
	}
	fmt.Fprintf(a.env.stdout, "password set for %s  %s; signed out of %d browser(s)\n", u.ID, u.Username, removed)
	return 0
}

func (a *admin) usersPasswordUsage() {
	_, _ = io.WriteString(a.env.stderr, `usage: clippet admin users password <username>   (the new password is read from STDIN)
  read -rsp 'password: ' PW; echo; printf '%s' "$PW" |
    docker compose exec -T app clippet admin users password alice
`)
}

func (a *admin) usersCreateUsage() {
	_, _ = io.WriteString(a.env.stderr, `usage: clippet admin users create <username>   (the password is read from STDIN)
  read -rsp 'password: ' PW; echo; printf '%s' "$PW" |
    docker compose exec -T app clippet admin users create alice
`)
}

// usernameOK is §14's rule, spelled here rather than imported so the CLI and
// the API cannot silently disagree about what a username is: httpapi's
// regexp is unexported and this is the only other place that mints one.
func usernameOK(s string) bool {
	if len(s) < 3 || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
		default:
			return false
		}
	}
	return true
}

// --- sessions and tokens (ROADMAP #1, §20 erratum 109) ---------------------

// resolveUser turns a username into its row for the session/token verbs. It
// is an EXACT lookup (case-insensitively, like registration), never a
// pattern: `users prune` takes a LIKE because it reports what it will remove
// first, and these do not.
func (a *admin) resolveUser(name string) (db.User, bool) {
	return a.resolveUserTo(a.env.stdout, name)
}

// resolveUserTo is resolveUser writing its refusal wherever the caller's
// reader is. Every verb here reports "no such user" on STDOUT, because stdout
// is where their answers go — except `tokens mint`, whose stdout is a FILE a
// shell redirects (§20 erratum 308): `clippet admin tokens mint typo >
// clippet.sxcu` must not write a sentence about a typo into the uploader file.
func (a *admin) resolveUserTo(w io.Writer, name string) (db.User, bool) {
	u, err := a.db.GetUserByUsername(a.ctx, name)
	switch {
	case errors.Is(err, db.ErrNotFound):
		fmt.Fprintf(w, "%s: no such user\n", name)
		return db.User{}, false
	case err != nil:
		a.fail("user lookup", err)
		return db.User{}, false
	}
	return u, true
}

func (a *admin) sessionsUsage() {
	fmt.Fprintln(a.env.stderr, "usage: clippet admin sessions list <username>")
	fmt.Fprintln(a.env.stderr, "       clippet admin sessions revoke <username>")
	fmt.Fprintln(a.env.stderr, "       clippet admin sessions revoke --all")
	fmt.Fprintln(a.env.stderr, "  a selector is required: either a username or --all. There is no default.")
}

func (a *admin) sessions(args []string) int {
	if len(args) == 0 {
		a.sessionsUsage()
		return 2
	}
	switch args[0] {
	case "list":
		return a.sessionsList(args[1:])
	case "revoke":
		return a.sessionsRevoke(args[1:])
	}
	a.sessionsUsage()
	return 2
}

// sessionsList prints one user's live sessions. It prints no cookie value and
// no hash: the hash IS the credential's shadow, and an operator has no use for
// it that revoking does not serve better.
func (a *admin) sessionsList(args []string) int {
	fs := a.flags("sessions list")
	name, err := parseWithID(fs, args)
	if err != nil || strings.TrimSpace(name) == "" {
		a.sessionsUsage()
		return 2
	}
	u, ok := a.resolveUser(name)
	if !ok {
		return 1
	}
	rows, err := a.db.ListSessions(a.ctx, u.ID, a.env.now().Unix())
	if err != nil {
		return a.fail("sessions list", err)
	}
	for _, s := range rows {
		// `max_expires_at <= 0` is migration 0005's "no ceiling recorded"
		// SENTINEL, which every read guards for; printed through rfc() it
		// rendered as 1970-01-01T00:00:00Z and read as a session that died
		// decades ago. A row like that can only come from an older binary
		// writing against a migrated file -- auth.CreateSession always writes
		// a ceiling -- but ListSessions does return it, so it says what it is.
		max := "none"
		if s.MaxExpiresAt > 0 {
			max = rfc(s.MaxExpiresAt)
		}
		fmt.Fprintf(a.env.stdout, "%s  started=%s last_seen=%s expires=%s max=%s ua=%q\n",
			u.Username, rfc(s.CreatedAt), rfc(s.LastSeenAt), rfc(s.ExpiresAt), max, s.UserAgent)
	}
	fmt.Fprintf(a.env.stdout, "%d live session(s) for %s\n", len(rows), u.Username)
	return 0
}

// sessionsRevoke signs one user, or everybody, out of every browser. The
// guard is `users prune`'s: an explicit selector, and no default that means
// "everything" — `--all` has to be typed, and it may not be combined with a
// username, so a fat-fingered command cannot mean more than it says.
func (a *admin) sessionsRevoke(args []string) int {
	fs := a.flags("sessions revoke")
	all := fs.Bool("all", false, "required to sign out EVERY user (mutually exclusive with <username>)")
	name, err := parseWithID(fs, args)
	name = strings.TrimSpace(name)
	if err != nil || (name == "") == !*all {
		a.sessionsUsage()
		return 2
	}
	if *all {
		n, err := a.db.DeleteAllSessions(a.ctx)
		if err != nil {
			return a.fail("sessions revoke", err)
		}
		fmt.Fprintf(a.env.stdout, "revoked %d session(s) across every account\n", n)
		return 0
	}
	u, ok := a.resolveUser(name)
	if !ok {
		return 1
	}
	// keepHash "" keeps nothing: an operator revoking a user's sessions means
	// all of them, including whichever one is in front of them.
	n, err := a.db.DeleteOtherSessions(a.ctx, u.ID, "")
	if err != nil {
		return a.fail("sessions revoke", err)
	}
	fmt.Fprintf(a.env.stdout, "revoked %d session(s) for %s\n", n, u.Username)
	return 0
}

func (a *admin) tokensUsage() {
	fmt.Fprintln(a.env.stderr, "usage: clippet admin tokens list <username>")
	fmt.Fprintln(a.env.stderr, "       clippet admin tokens mint <username> [--name n] [--scopes upload[,read]|inbound|autofetch] [--sxcu]")
	fmt.Fprintln(a.env.stderr, "       clippet admin tokens revoke <username> <tid>")
	fmt.Fprintln(a.env.stderr, "       clippet admin tokens revoke <username> --all")
	fmt.Fprintln(a.env.stderr, "  revoke needs a token id or --all; a username alone is not a selector.")
	fmt.Fprintln(a.env.stderr, "  mint writes the raw token (or, with --sxcu, the ShareX file) to STDOUT, once.")
}

func (a *admin) tokens(args []string) int {
	if len(args) == 0 {
		a.tokensUsage()
		return 2
	}
	switch args[0] {
	case "list":
		return a.tokensList(args[1:])
	case "mint":
		return a.tokensMint(args[1:])
	case "revoke":
		return a.tokensRevoke(args[1:])
	}
	a.tokensUsage()
	return 2
}

// tokensList prints one user's live tokens. `prefix` is the display half a
// token's owner also sees; the hash is not printed here or anywhere.
func (a *admin) tokensList(args []string) int {
	fs := a.flags("tokens list")
	name, err := parseWithID(fs, args)
	if err != nil || strings.TrimSpace(name) == "" {
		a.tokensUsage()
		return 2
	}
	u, ok := a.resolveUser(name)
	if !ok {
		return 1
	}
	toks, err := a.db.ListTokens(a.ctx, u.ID)
	if err != nil {
		return a.fail("tokens list", err)
	}
	for _, t := range toks {
		used := "never"
		if t.LastUsedAt != nil {
			used = rfc(*t.LastUsedAt)
		}
		fmt.Fprintf(a.env.stdout, "%s  %s  name=%q scopes=%s created=%s last_used=%s\n",
			t.ID, displayPrefix(t)+"…", t.Name, strings.Join(t.Scopes, "+"), rfc(t.CreatedAt), used)
	}
	fmt.Fprintf(a.env.stdout, "%d live token(s) for %s\n", len(toks), u.Username)
	return 0
}

// tokensRevoke revokes one token of a user, or every live token of that user
// with --all. The username alone is deliberately NOT a selector: "revoke
// alice's tokens" is a sentence an operator can type by accident while
// meaning "show me alice's tokens", so the destructive reading has to be
// spelled out.
func (a *admin) tokensRevoke(args []string) int {
	fs := a.flags("tokens revoke")
	all := fs.Bool("all", false, "required to revoke EVERY live token of that user (instead of one <tid>)")
	positional, err := parsePositionals(fs, args, 2)
	if err != nil || len(positional) == 0 {
		a.tokensUsage()
		return 2
	}
	name := strings.TrimSpace(positional[0])
	tid := ""
	if len(positional) > 1 {
		tid = strings.TrimSpace(positional[1])
	}
	if name == "" || (tid == "") == !*all {
		a.tokensUsage()
		return 2
	}
	u, ok := a.resolveUser(name)
	if !ok {
		return 1
	}
	if *all {
		n, err := a.db.RevokeUserTokens(a.ctx, u.ID, a.env.now().Unix())
		if err != nil {
			return a.fail("tokens revoke", err)
		}
		fmt.Fprintf(a.env.stdout, "revoked %d token(s) for %s\n", n, u.Username)
		return 0
	}
	err = a.db.RevokeToken(a.ctx, u.ID, tid, a.env.now().Unix())
	switch {
	case errors.Is(err, db.ErrNotFound):
		// Unknown, already revoked, or somebody else's: one answer, the same
		// one the owner's own DELETE gives (§8.1).
		fmt.Fprintf(a.env.stdout, "%s: no such live token for %s\n", tid, u.Username)
		return 1
	case err != nil:
		return a.fail("tokens revoke", err)
	}
	fmt.Fprintf(a.env.stdout, "revoked 1 token(s) for %s\n", u.Username)
	return 0
}

// mintTokenDefaultName is what a shell-minted token is called when nobody said
// otherwise. It is deliberately not "API token" or the username: the one thing
// a person reading /settings wants from this row is how it got there, and this
// row got there from a shell. `--name` is how you tell two machines apart.
const mintTokenDefaultName = "shell"

// tokensMint mints a capture token from the operator's shell (§20 erratum 308).
//
// `POST /api/auth/tokens` needs a browser SESSION, and the two situations a
// capture token is most often wanted in have none: a machine being set up by
// somebody who is not the account's owner, and a shell on a box whose browser
// cannot reach this instance. The rules are not re-implemented here — the name,
// the scope set and the ShareX file all come from internal/httpapi, which says
// why beside them — so a token minted at this door behaves exactly like one
// minted at the other, and `admin tokens list` and `admin tokens revoke` see
// nothing special about it.
//
// STDOUT carries the ARTEFACT and nothing else: the raw token, or the ShareX
// file with --sxcu. That is what makes
// `clippet admin tokens mint carol --sxcu > clippet.sxcu` an importable
// uploader and `clippet admin tokens mint carol > token.txt` a token. Every
// human word goes to stderr and NONE of them carries the raw value — the
// confirmation prints the same `ck_<prefix>…` display half `tokens list`
// prints. The raw token is shown exactly once (§14); this program has no
// logger, so there is nowhere else for it to go, and there must never be.
func (a *admin) tokensMint(args []string) int {
	fs := a.flags("tokens mint")
	// No back-quoted word in any usage string here: flag reads one as the name of
	// the operand and prints it in place of the value, so a nod to `tokens list`
	// would render as "-name tokens list".
	name := fs.String("name", mintTokenDefaultName, "the token's name, as /settings and the token list show it")
	scopes := fs.String("scopes", auth.ScopeUpload,
		"comma-separated scopes from "+strings.Join(auth.Scopes, ", ")+"; or "+auth.ScopeInbound+
			" alone, which mints an inbound URL; or "+auth.ScopeAutoFetch+" alone, which mints a Clippet Auto URL")
	sxcu := fs.Bool("sxcu", false, "write the ShareX custom-uploader file to stdout instead of the raw token")
	user, err := parseWithID(fs, args)
	if err != nil || strings.TrimSpace(user) == "" {
		a.tokensUsage()
		return 2
	}
	// An ABSENT --scopes is the route's absent `scopes` array, which means
	// ["upload"]; an explicitly EMPTY one is the route's `[]`, which is an
	// error rather than "no permissions" (§20 erratum 111). flag cannot tell
	// those apart from the value alone, so ask which flags were actually set.
	var requested []string
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "scopes" {
			requested = splitScopes(*scopes)
		}
	})
	tokenName, err := httpapi.CleanTokenName(*name)
	if err != nil {
		fmt.Fprintln(a.env.stderr, "admin tokens mint: --name must be 1-64 characters and carry no control characters")
		return 2
	}
	granted, err := httpapi.CleanScopes(requested)
	if err != nil {
		fmt.Fprintln(a.env.stderr, "admin tokens mint: --"+httpapi.ScopesRule)
		return 2
	}
	// A ShareX destination that cannot upload imports cleanly and then answers
	// 403 on the first capture, which is why the route sends `sxcu: null` for a
	// token without the scope (§20 erratum 111). A file is not something that
	// can be null on stdout, so this is refused BEFORE anything is minted: a
	// token nobody asked for is worse than a command that did nothing.
	if *sxcu && !slices.Contains(granted, auth.ScopeUpload) {
		fmt.Fprintf(a.env.stderr, "admin tokens mint: --sxcu needs the %s scope; a ShareX destination that cannot upload answers 403 on the first capture\n", auth.ScopeUpload)
		return 2
	}
	u, ok := a.resolveUserTo(a.env.stderr, strings.TrimSpace(user))
	if !ok {
		return 1
	}
	// The route's own decision of which kind of secret this row gets (§20
	// errata 322, 324), on this instance's own PUBLIC_BASE_URL: `--scopes inbound`
	// mints an inbound URL, `--scopes autofetch` a Clippet Auto URL, every other
	// set a `ck_` token.
	base := urls.New(a.cfg.PublicBaseURL, "admin")
	raw, pathURL := httpapi.NewTokenValue(granted, base)
	tok := db.APIToken{
		UserID:    u.ID,
		Name:      tokenName,
		TokenHash: auth.HashToken(raw),
		Prefix:    auth.TokenPrefixOf(raw),
		Scopes:    granted,
		CreatedAt: a.env.now().Unix(),
	}
	// The route's own id-collision retry, for the route's own reason: an id is
	// random and a clash is a retry, not a failure. Two attempts and then a
	// refusal — a third would be evidence of something other than luck.
	for attempt := 0; ; attempt++ {
		tok.ID = ids.New()
		err := a.db.CreateToken(a.ctx, tok)
		if err == nil {
			break
		}
		if !errors.Is(err, db.ErrConflict) {
			return a.fail("tokens mint", err)
		}
		if attempt >= 1 {
			return a.fail("tokens mint", errors.New("could not allocate a token id; run it again"))
		}
	}
	if *sxcu {
		// The SAME generator the browser's download is built from, on this
		// instance's own PUBLIC_BASE_URL — which is what makes the file work
		// from a machine that has never seen this instance's settings page.
		doc, err := json.MarshalIndent(httpapi.NewSXCU(base, raw), "", "  ")
		if err != nil {
			return a.fail("tokens mint", err)
		}
		fmt.Fprintf(a.env.stdout, "%s\n", doc)
	} else if pathURL != "" {
		// The URL IS the artefact for an inbound or Clippet Auto row: the thing
		// a tool is configured with, and nothing else on stdout.
		fmt.Fprintln(a.env.stdout, pathURL)
	} else {
		fmt.Fprintln(a.env.stdout, raw)
	}
	fmt.Fprintf(a.env.stderr, "minted %s for %s: %s name=%q scopes=%s\n",
		tok.ID, u.Username, displayPrefix(tok)+"…", tok.Name, strings.Join(tok.Scopes, "+"))
	what := "the token on stdout is shown ONCE"
	switch {
	case *sxcu:
		what = "the file on stdout carries the token and is shown ONCE; treat it as a password"
	case pathURL != "":
		what = "the URL on stdout is shown ONCE; anyone who has it can upload as " + u.Username
	}
	fmt.Fprintf(a.env.stderr, "%s. Revoke it with: clippet admin tokens revoke %s %s\n", what, u.Username, tok.ID)
	return 0
}

// displayPrefix is the display half of a row's secret as people read it: `ci_`
// for an inbound URL (§20 erratum 322), `ca_` for a Clippet Auto URL (§20
// erratum 324), `ck_` for every other token. The prefix column stores only the
// 8 characters after any of them.
func displayPrefix(t db.APIToken) string {
	if httpapi.IsInboundSet(t.Scopes) {
		return ids.InboundPrefix + t.Prefix
	}
	if httpapi.IsAutoFetchSet(t.Scopes) {
		return ids.AutoFetchPrefix + t.Prefix
	}
	return ids.TokenPrefix + t.Prefix
}

// splitScopes turns `--scopes upload, read` into the array the route's JSON
// would have carried. Trimming is a shell affordance and not a second rule:
// what is or is not a scope is httpapi.CleanScopes' answer, and an empty
// element survives this on purpose so `--scopes ""` and `--scopes upload,` are
// refused there rather than silently narrowed here.
func splitScopes(v string) []string {
	parts := strings.Split(v, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return parts
}

// parsePositionals is parseWithID for a verb that takes up to max positional
// arguments in any position relative to the flags. It keeps
// `tokens revoke alice --all` and `tokens revoke --all alice` meaning the
// same thing, which is what every other verb in this file already does.
func parsePositionals(fs *flag.FlagSet, args []string, max int) ([]string, error) {
	var out []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return out, nil
		}
		if len(out) >= max {
			return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
		}
		out, args = append(out, fs.Arg(0)), fs.Args()[1:]
	}
}

// --- stats -----------------------------------------------------------------

func (a *admin) stats(args []string) int {
	fs := a.flags("stats")
	// The flag's name reads wider than what it does, and it is deliberately
	// NOT renamed: `--no-storage` is published in DEPLOY (§15.4's restore
	// rehearsal, §21 and §25) and in DESIGN §7, and an alias pair would be
	// worse than the mismatch. What it skips is the BUCKET LIST; the
	// `storage:` line above it is a database sum that needs no bucket
	// credentials, which is exactly why DEPLOY tells an operator holding
	// placeholder S3 values to run this form (N068).
	noStorage := fs.Bool("no-storage", false, "skip the BUCKET listing (the storage: line above it is a database sum and is still printed)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(a.env.stderr, "usage: clippet admin stats [--no-storage]")
		return 2
	}
	users, err := a.db.CountUsers(a.ctx)
	if err != nil {
		return a.fail("stats", err)
	}
	rows, err := a.db.ListAllClipIDsAndKeys(a.ctx)
	if err != nil {
		return a.fail("stats", err)
	}
	byState := map[string]int{}
	for _, r := range rows {
		byState[r.State]++
	}
	queue, err := a.db.QueueHealth(a.ctx, a.env.now().Unix())
	if err != nil {
		return a.fail("stats", err)
	}
	kinds, err := a.db.JobKindCounts(a.ctx)
	if err != nil {
		return a.fail("stats", err)
	}
	versions, err := a.db.AppliedVersions(a.ctx)
	if err != nil {
		return a.fail("stats", err)
	}
	w := a.env.stdout
	// The applied migration list is the observable that says which schema this
	// database is on — an operator upgrading an existing install, and E2E
	// step 22, both need to see 0002 land on a volume that was created by
	// 0001 alone.
	fmt.Fprintf(w, "schema:  applied=%s\n", joinInts(versions))
	fmt.Fprintf(w, "users:   %d\n", users)
	fmt.Fprintf(w, "clips:   %d (queued=%d processing=%d ready=%d failed=%d deleting=%d)\n", len(rows),
		byState[db.ClipQueued], byState[db.ClipProcessing], byState[db.ClipReady], byState[db.ClipFailed], byState[db.ClipDeleting])
	// The per-kind line is also the DISCORD_WEBHOOK_URL observable: with the
	// variable unset, `notify=0` is the proof no announcement was ever queued
	// (E2E step 20). Counts are all states, so a delivered job still shows.
	//
	// It names EVERY kind in `jobKinds`, which is the point of the line: an
	// operator reading it must not have to know that some kind is missing from
	// it. `full` is rare — since §20 erratum 217 both tiers come out of one
	// ffmpeg, so neither producer enqueues one on the ordinary path — and rare
	// is exactly the kind whose count is worth having when it is not zero.
	fmt.Fprintf(w, "jobs:    queued=%d running=%d (transcode=%d notify=%d trim=%d gif=%d social=%d replace=%d poster=%d filmstrip=%d full=%d hook=%d)\n",
		queue.Queued, queue.Running,
		kinds[db.KindTranscode], kinds[db.KindNotify], kinds[db.KindTrim], kinds[db.KindGIF], kinds[db.KindSocial],
		kinds[db.KindReplace], kinds[db.KindPoster], kinds[db.KindFilmstrip], kinds[db.KindFull], kinds[db.KindHook])
	// The three observables /healthz gained (§20 erratum 145), on the line an
	// operator is already looking at. `failed_24h` is the backlog neither
	// counter above can show; the two ages are what tell a busy queue from a
	// stalled one (DEPLOY.md §24).
	fmt.Fprintf(w, "queue:   failed_24h=%d oldest_queued_age_s=%d longest_running_age_s=%d\n",
		queue.Failed24h, queue.OldestQueuedAgeS, queue.LongestRunningAgeS)
	// The sweeper's destroy-failure memory (§20 erratum 232). Until now its
	// only operator surface was a WARN in the log, so an instance that had been
	// failing to empty one prefix for a week said so nowhere an operator looks
	// on purpose. Zero is the healthy answer and is printed as one: a line that
	// appears only when something is wrong teaches nobody what it means.
	backoff, err := a.db.CountSweepFailures(a.ctx)
	if err != nil {
		return a.fail("stats", err)
	}
	fmt.Fprintf(w, "backoff: clips=%d waiting out a destroy failure (DEPLOY.md §10)\n", backoff)
	// The advisory storage budget (ROADMAP #24, §20 erratum 157). `used` is
	// what the DATABASE says is in the bucket — the same number /healthz
	// reports and the same one the warning is raised on — so this line needs
	// no bucket credentials and is printed with --no-storage too. It is never
	// a cap: `OVER` changes nothing the app does, and the line says so rather
	// than leaving an operator to wonder whether uploads have stopped.
	used, err := a.db.StorageUsed(a.ctx)
	if err != nil {
		return a.fail("stats", err)
	}
	a.printQuota(w, used)
	if budget := a.cfg.StorageBudgetBytes; budget > 0 {
		verdict := "ok"
		if used > budget {
			verdict = "OVER (advisory only: no upload is ever refused, unless STORAGE_BUDGET_HARD=1)"
			if a.cfg.StorageBudgetHard {
				verdict = "OVER (STORAGE_BUDGET_HARD=1: a new clip is refused with 507 until this is back under the line)"
			}
		}
		fmt.Fprintf(w, "storage: used=%d (%.1f MiB) budget=%d (%.1f MiB) %s%s\n",
			used, float64(used)/(1<<20), budget, float64(budget)/(1<<20), verdict, a.storeSuffix())
	} else {
		fmt.Fprintf(w, "storage: used=%d (%.1f MiB) budget=off (STORAGE_BUDGET_BYTES)%s\n",
			used, float64(used)/(1<<20), a.storeSuffix())
	}
	a.printServed(w, used)
	a.printDisk(w)
	if *noStorage {
		return 0
	}
	st, err := a.store()
	if err != nil {
		return a.fail("stats", err)
	}
	// `bucket:` on s3 and `store:` on fs. The label is what an operator greps
	// and what this repository's own scripts parse, and on `fs` there is no
	// bucket to name — a line that said `bucket:` about a directory would be
	// the one place this feature lied (§20 erratum 276).
	label := a.storeLabel()
	objs, err := st.List(a.ctx, a.cfg.S3.KeyPrefix+"clips/")
	if err != nil {
		fmt.Fprintf(w, "%s error: %v\n", label, err)
		return 1
	}
	var bytes int64
	prefixes := map[string]bool{}
	for _, o := range objs {
		bytes += o.Size
		if id, ok := a.clipIDOf(o.Key); ok {
			prefixes[id] = true
		}
	}
	fmt.Fprintf(w, "%s objects=%d bytes=%d (%.1f MiB) clip_prefixes=%d\n", label, len(objs), bytes, float64(bytes)/(1<<20), len(prefixes))
	return 0
}

// storeSuffix is the `fs` half of the reconciliation ROADMAP #37 asked for.
//
// On `s3` the budget line (a DATABASE sum of what the bucket should hold) and
// the disk line (the container's own filesystem) describe two different
// places, and printing both is the whole point. On `fs` they describe ONE
// place, and an operator reading "used=40 GiB" beside "scratch_free=2 GiB" has
// to be told which disk the 40 GiB is on. So the budget line carries the
// directory and its free space, and the disk line below it goes on being about
// the database and the scratch disk.
//
// It appends rather than replaces, so `used=` and `budget=` stay exactly where
// every existing parser (and E2E step 87) looks for them.
func (a *admin) storeSuffix() string {
	if a.cfg.StorageBackend != config.BackendFS {
		return ""
	}
	free, total, err := diskUsage(a.cfg.StorageDir)
	if err != nil {
		return fmt.Sprintf(" backend=fs dir=%s (free space unavailable: %v)", a.cfg.StorageDir, err)
	}
	return fmt.Sprintf(" backend=fs dir=%s free=%d (%s) size=%d (%s)",
		a.cfg.StorageDir, free, humanBytes(free), total, humanBytes(total))
}

// printDisk is the free-disk line of `admin stats` (ROADMAP #13, §20 erratum
// 147). `admin stats` reported bucket bytes and never free disk, though
// `507 insufficient_scratch` is a documented error whose whole cause is free
// space. Raw bytes so the line can be parsed, human sizes so it can be read,
// and `same_device` because with the shipped compose file DATA_DIR and
// SCRATCH_DIR are two named volumes on ONE filesystem — so "the scratch disk
// is full" and "the database has nowhere to write" are the same sentence.
// printServed is `admin stats`' storage-cost line (ROADMAP #39, §20 erratum
// 270): what this instance has sent to viewers today and this calendar month,
// and — only when the operator has set a price — what that and the stored bytes
// come to.
//
// Two lines, and the second one exists only when there is a price. An operator
// who has configured nothing sees the `served:` counts and no cost at all,
// because a 0.00 with no price behind it reads as "this is free" (the rule the
// whole feature is built on: unset is ABSENT, not zero).
//
// The numbers are the DATABASE's. A live `serve` process holds up to one flush
// window of served bytes in memory (internal/cost), so this CLI — a second
// process that shares the file — can be a window behind; `/healthz` and the
// settings page, which are that process, are exact. It is a cost estimate to
// two decimals, and the window is 64 MiB.
// printQuota is the HEADLINE of the storage block (§20 erratum 277): how much
// room is left, in the figures a person reads, and which of the two things the
// budget is on this instance.
//
// It sits ABOVE the `storage:` line rather than replacing it, and the pair is
// the shape `disk:` already uses two lines below: one line a person reads
// (human sizes, a percentage, the mode in words) and one a script parses
// (`used=` and `budget=` as raw integers, which four E2E steps and DEPLOY §25
// already do). Printed only when there is a budget — with none there is no
// quota to be a headline about, and `storage:` says `budget=off` on its own.
func (a *admin) printQuota(w io.Writer, used int64) {
	budget := a.cfg.StorageBudgetBytes
	if budget <= 0 {
		return
	}
	mode := "advisory (STORAGE_BUDGET_HARD=0: no upload is ever refused because of it)"
	if a.cfg.StorageBudgetHard {
		mode = "HARD (STORAGE_BUDGET_HARD=1: a new clip is refused with 507 at or past the line)"
	}
	fmt.Fprintf(w, "quota:   %s of %s (%d %%) %s\n",
		humanBytes(used), humanBytes(budget), used*100/budget, mode)
}

func (a *admin) printServed(w io.Writer, stored int64) {
	now := a.env.now()
	start, end := cost.MonthRange(now)
	today, month, err := a.db.BytesServed(a.ctx, cost.Day(now), start, end)
	if err != nil {
		fmt.Fprintf(w, "served:  unavailable: %v\n", err)
		return
	}
	// In `redirect` mode the app does not WRITE the bytes it counts: the media
	// leaves the bucket directly, and each request is charged its whole object
	// (§20 erratum 278(i)). The line says so where the number is read, because
	// the `cost:` line below turns it into money. `/healthz` stays bare
	// integers — it is a machine surface, and DEPLOY §31.3 carries the sentence.
	sent, estimate := "bytes sent to viewers", ""
	if a.cfg.RedirectsMedia() {
		sent = "bytes a viewer was redirected to fetch"
		estimate = " — estimated (redirect): each request is charged its whole object"
	}
	fmt.Fprintf(w, "served:  today=%d (%s) month=%d (%s) — %s, aggregate per UTC day, %d-day history%s\n",
		today, humanBytes(today), month, humanBytes(month), sent, cost.RetentionDays, estimate)
	prices := cost.PricesOf(a.cfg)
	if !prices.Any() {
		return
	}
	storedText, servedText := "unpriced", "unpriced"
	if amount := prices.Stored(stored); amount != nil {
		storedText = "≈" + amount.String() + " /month"
	}
	if amount := prices.Served(month, stored); amount != nil {
		servedText = "≈" + amount.String() + " this month"
	}
	// Where the prices came from. A preset is somebody else's dated list and
	// the line says whose and when, because an estimate from an undated list
	// is a number nobody can check (§20 erratum 277); with no preset the
	// prices are the operator's own and the two variables are the attribution.
	source := "STORAGE_PRICE_PER_GB_MONTH and EGRESS_PRICE_PER_GB"
	if s := prices.Source(); s != "" {
		source = s
	}
	redirectNote := ""
	if a.cfg.RedirectsMedia() {
		redirectNote = "; the served half is estimated (redirect)"
	}
	fmt.Fprintf(w, "cost:    stored %s  served %s  (an ESTIMATE at 1 GB = 10^9 bytes, from %s — never a bill%s)\n",
		storedText, servedText, source, redirectNote)
}

func (a *admin) printDisk(w io.Writer) {
	dataFree, dataSize, dataErr := diskUsage(a.cfg.DataDir)
	scratchFree, scratchSize, scratchErr := diskUsage(a.cfg.ScratchDir)
	if dataErr != nil || scratchErr != nil {
		err := dataErr
		if err == nil {
			err = scratchErr
		}
		fmt.Fprintf(w, "disk:    unavailable: %v\n", err)
		return
	}
	same, err := sameDevice(a.cfg.DataDir, a.cfg.ScratchDir)
	sameText := fmt.Sprint(same)
	if err != nil {
		sameText = "unknown"
	}
	fmt.Fprintf(w, "disk:    data_free=%d data_size=%d scratch_free=%d scratch_size=%d min_free=%d same_device=%s\n",
		dataFree, dataSize, scratchFree, scratchSize, a.cfg.ScratchMinFree, sameText)
	fmt.Fprintf(w, "disk:    data=%s free of %s (%s), scratch=%s free of %s (%s), SCRATCH_MIN_FREE_BYTES=%s\n",
		humanBytes(dataFree), humanBytes(dataSize), a.cfg.DataDir,
		humanBytes(scratchFree), humanBytes(scratchSize), a.cfg.ScratchDir, humanBytes(a.cfg.ScratchMinFree))
	if scratchFree < a.cfg.ScratchMinFree {
		fmt.Fprintf(w, "disk:    WARNING scratch free space is below SCRATCH_MIN_FREE_BYTES: every upload answers 507 insufficient_scratch\n")
	}
}

// clipIDOf extracts the clip id from a key under {P}clips/; ok is false for
// anything that is not shaped like one of ours.
func (a *admin) clipIDOf(key string) (string, bool) {
	rest, found := strings.CutPrefix(key, a.cfg.S3.KeyPrefix+"clips/")
	if !found {
		return "", false
	}
	id, _, found := strings.Cut(rest, "/")
	if !found || !ids.Valid(id) {
		return "", false
	}
	return id, true
}

// --- orphans ---------------------------------------------------------------

func (a *admin) orphans(args []string) int {
	fs := a.flags("orphans")
	del := fs.Bool("delete", false, "remove orphaned prefixes (never a row's missing objects)")
	minAge := fs.Duration("min-age", orphanMinAge, "ignore row-less prefixes whose newest object is younger than this")
	clip := fs.String("clip", "", "scope every report, and --delete, to this one clip's prefix")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 || (*clip != "" && !ids.Valid(*clip)) {
		fmt.Fprintln(a.env.stderr, "usage: clippet admin orphans [--delete] [--min-age D] [--clip id]")
		return 2
	}
	if *del {
		// --delete is NOT bounded by the invocation's ten minutes (§20 erratum
		// 350): on a large store it is one DeletePrefix after another, and a
		// deadline that expired half way turned every remaining prefix into
		// `delete failed` with nothing wrong. Each store call keeps its own
		// per-operation timeout (STORAGE_OP_TIMEOUT, floored for the CLI), and
		// the verb is idempotent — a second run removes what the first left.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		a.ctx = ctx
	}
	// The live-job set is read BEFORE the columns, and the order is the whole
	// point: a job that finishes between the two reads is caught by the FIRST
	// one (it was still running) instead of falling through both — running
	// then, and its outputs not yet referenced by the stale column snapshot.
	// Read the other way round, a job that finalized in that window would
	// leave its brand-new rendition looking like garbage of arbitrary age,
	// which is the one thing --min-age cannot bound (§20 erratum 150).
	live, err := a.db.ClipIDsWithLiveJobs(a.ctx)
	if err != nil {
		return a.fail("orphans", err)
	}
	rows, err := a.db.ListAllClipIDsAndKeys(a.ctx)
	if err != nil {
		return a.fail("orphans", err)
	}
	st, err := a.store()
	if err != nil {
		return a.fail("orphans", err)
	}
	// --clip <id> (§20 erratum 290, AUDIT #4): the LISTING is one clip's
	// prefix rather than the whole of clips/, and the rows are that clip's row
	// or none — so every report below (row-less prefix, missing keys, extras)
	// and every --delete is bounded by that prefix by construction. The E2E's
	// step 54 runs `--delete` this way on the shared stack: a bucket-wide
	// delete there was the one unattended destructive call in the suite.
	root := a.cfg.S3.KeyPrefix + "clips/"
	if *clip != "" {
		root = store.ClipPrefix(a.cfg.S3.KeyPrefix, *clip)
		kept := rows[:0]
		for _, r := range rows {
			if r.ID == *clip {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	objs, err := st.List(a.ctx, root)
	if err != nil {
		return a.fail("orphans", err)
	}

	rowIDs := make(map[string]bool, len(rows))
	for _, r := range rows {
		rowIDs[r.ID] = true
	}
	present := make(map[string]bool, len(objs))
	byClip := map[string][]store.ObjectInfo{}
	var foreign []string
	for _, o := range objs {
		// The instance's own prefix is never an orphan, never foreign and never
		// deleted (§20 erratum 343). It lies outside clips/, so a listing of
		// that root cannot return it; this is the same rule said where a wider
		// root, should one ever be listed, would meet it.
		if store.IsReserved(a.cfg.S3.KeyPrefix, o.Key) {
			continue
		}
		present[o.Key] = true
		id, ok := a.clipIDOf(o.Key)
		if !ok {
			foreign = append(foreign, o.Key)
			continue
		}
		byClip[id] = append(byClip[id], o)
	}

	w := a.env.stdout
	now := a.env.now()
	if *clip != "" {
		fmt.Fprintf(w, "scope   %s (--clip: no other prefix was listed)\n", root)
	}
	var candidates []string
	for id := range byClip {
		if !rowIDs[id] {
			candidates = append(candidates, id)
		}
	}
	sort.Strings(candidates)
	orphans, deleted, recent, errs := 0, 0, 0, 0
	for _, id := range candidates {
		prefix := store.ClipPrefix(a.cfg.S3.KeyPrefix, id)
		var newest time.Time
		var bytes int64
		for _, o := range byClip[id] {
			bytes += o.Size
			if o.LastModified.After(newest) {
				newest = o.LastModified
			}
		}
		if age := now.Sub(newest); age < *minAge {
			recent++
			fmt.Fprintf(w, "recent  %s objects=%d age=%s (skipped: younger than --min-age %s)\n", prefix, len(byClip[id]), age.Round(time.Second), *minAge)
			continue
		}
		orphans++
		fmt.Fprintf(w, "orphan  %s objects=%d bytes=%d\n", prefix, len(byClip[id]), bytes)
		if !*del {
			continue
		}
		n, err := st.DeletePrefix(a.ctx, prefix)
		if err != nil {
			errs++
			fmt.Fprintf(w, "        delete failed after %d objects: %v\n", n, err)
			continue
		}
		deleted++
		fmt.Fprintf(w, "        deleted %d objects\n", n)
	}

	missing := 0
	for _, r := range rows {
		for _, k := range r.Keys() {
			if !present[k] {
				missing++
				fmt.Fprintf(w, "missing %s state=%s key=%s\n", r.ID, r.State, k)
			}
		}
	}
	extras, extraBytes, extraDeleted, extraErrs := a.reportExtras(w, st, rows, byClip, live, now, *minAge, *del)
	errs += extraErrs
	for _, k := range foreign {
		fmt.Fprintf(w, "foreign %s (not a clip prefix; left alone)\n", k)
	}
	// The three `extra_*` counters are APPENDED (§20 erratum 148) rather than
	// grouped where they read best: the first five fields are what an operator's
	// notes and this repository's tests already grep for, and a report whose
	// summary line changed shape would be a needless break.
	fmt.Fprintf(w, "orphans: prefixes=%d deleted=%d recent=%d missing=%d foreign=%d extra=%d extra_bytes=%d extra_deleted=%d\n",
		orphans, deleted, recent, missing, len(foreign), extras, extraBytes, extraDeleted)
	if errs > 0 {
		return 1
	}
	return 0
}

// reportExtras is `orphans`' THIRD report (ROADMAP #13, §20 erratum 148):
// objects under a LIVE clip's prefix that no column names.
//
// Every trim replaces the rendition, poster and preview and deletes the
// superseded keys AFTER the commit, best-effort and never retried; a crash in
// that window leaves objects under a live clip prefix that nothing references,
// and the two existing reports cannot see them — one looks for prefixes with
// no row, the other for referenced keys with no object. `db.KeysOf(clip).Keys()`
// is already the single definition of what a row references, and
// `meta/probe.json` is the only key that legitimately has none, so the
// allowlist is one entry.
//
// It honours --min-age for the same reason the orphan report does, and the
// reason is sharper here: an in-flight transcode or trim uploads its outputs
// BEFORE the commit that starts referencing them, so a fresh unreferenced
// object under a live clip is usually a job that is still running.
//
// A CLIP WITH A LIVE OBJECT-WRITING JOB IS OFF LIMITS ENTIRELY (§20 erratum
// 150), and this is the guard --min-age cannot stand in for. A transcode
// checkpoints its rendition and poster keys (§10.3 step 8) BEFORE FinalizeTranscode writes
// those columns, no failure path clears the checkpoint, and the retry of a
// checkpointed job is finalize-only — so a job that has been stuck for hours,
// which is the only kind anybody runs `jobs unstick` on, holds hours-old
// objects that no column names and that the very next attempt will publish.
// An age threshold cannot separate those from garbage; "is anybody still on
// this clip" can. Such an object is printed with its reason, never counted and
// never deleted. A queued or running `notify` job does not count: it posts to
// a webhook and writes nothing to the bucket, so it is the one kind that
// cannot reference an object (§20 erratum 150).
//
// `--delete` never touches anything under `original/`: the upload is sacred
// (§11), and the one code path allowed to remove it is lifecycle.Destroy
// deleting the whole clip. Such an object is reported and left alone.
func (a *admin) reportExtras(w io.Writer, st store.Store, rows []db.ClipKeysRow, byClip map[string][]store.ObjectInfo, live map[string]bool, now time.Time, minAge time.Duration, del bool) (count int, bytes int64, deleted, errs int) {
	clipIDs := make([]string, 0, len(rows))
	referenced := make(map[string]map[string]bool, len(rows))
	states := make(map[string]string, len(rows))
	for _, r := range rows {
		set := map[string]bool{store.ProbeKey(a.cfg.S3.KeyPrefix, r.ID): true}
		for _, k := range r.Keys() {
			set[k] = true
		}
		referenced[r.ID] = set
		states[r.ID] = r.State
		clipIDs = append(clipIDs, r.ID)
	}
	sort.Strings(clipIDs)
	for _, id := range clipIDs {
		var clipBytes int64
		clipCount := 0
		for _, o := range byClip[id] {
			if referenced[id][o.Key] {
				continue
			}
			if live[id] {
				fmt.Fprintf(w, "extra   %s state=%s key=%s bytes=%d (skipped: the clip has a live job, which can reference this object through its checkpoint)\n",
					id, states[id], o.Key, o.Size)
				continue
			}
			if age := now.Sub(o.LastModified); age < minAge {
				fmt.Fprintf(w, "extra   %s state=%s key=%s bytes=%d age=%s (skipped: younger than --min-age %s)\n",
					id, states[id], o.Key, o.Size, age.Round(time.Second), minAge)
				continue
			}
			count++
			clipCount++
			clipBytes += o.Size
			bytes += o.Size
			fmt.Fprintf(w, "extra   %s state=%s key=%s bytes=%d\n", id, states[id], o.Key, o.Size)
			if !del {
				continue
			}
			if strings.Contains(o.Key, "/original/") {
				fmt.Fprintf(w, "        refusing to delete an original; left alone\n")
				continue
			}
			// A key is its own prefix (§20.6), which is how the worker already
			// removes one superseded object.
			n, err := st.DeletePrefix(a.ctx, o.Key)
			if err != nil {
				errs++
				fmt.Fprintf(w, "        delete failed after %d objects: %v\n", n, err)
				continue
			}
			deleted++
			fmt.Fprintf(w, "        deleted\n")
		}
		if clipCount > 0 {
			fmt.Fprintf(w, "extra   %s subtotal objects=%d bytes=%d\n", id, clipCount, clipBytes)
		}
	}
	return count, bytes, deleted, errs
}

// --- backup / restore-check ------------------------------------------------

// backup snapshots the live database (db.Backup = `VACUUM INTO`, consistent
// while `serve` keeps writing) to a temp name BESIDE the destination and
// renames it into place. Two properties fall out of that: an interrupted run
// leaves a `.part-…` file rather than a truncated file under the name an
// operator would restore from, and the rename is atomic on the same
// filesystem, so the destination either does not exist or is a whole backup.
// It never overwrites — a backup command that can destroy the previous backup
// is a footgun, and `rm` is right there.
func (a *admin) backup(args []string) int {
	fs := a.flags("backup")
	rotate := fs.Int("rotate", 0, "with a DIRECTORY argument: keep only the newest N snapshots this command made")
	// parsePositionals, not fs.Parse: `backup <dir> --rotate 2` puts the flag
	// AFTER the path, and flag.Parse stops at the first non-flag argument.
	positional, err := parsePositionals(fs, args, 1)
	if err != nil || len(positional) != 1 || positional[0] == "" {
		fmt.Fprintln(a.env.stderr, "usage: clippet admin backup <path>")
		fmt.Fprintln(a.env.stderr, "       clippet admin backup <dir> [--rotate N]")
		return 2
	}
	dst := positional[0]
	if *rotate < 0 {
		fmt.Fprintln(a.env.stderr, "admin backup: --rotate must be 0 (keep everything) or more")
		return 2
	}
	// A DIRECTORY argument mints the filename, which is what makes rotation
	// possible at all (§20 erratum 149): the name is derived, so the command
	// knows which files it made and can leave every other file alone.
	if fi, err := os.Stat(dst); err == nil && fi.IsDir() {
		_, code := a.backupInto(dst, *rotate)
		return code
	}
	if *rotate > 0 {
		fmt.Fprintf(a.env.stderr, "admin backup: --rotate needs a DIRECTORY argument (%s is not one); a single named file is never rotated\n", dst)
		return 2
	}
	size, code := a.writeSnapshot(dst)
	if code != 0 {
		return code
	}
	return a.reportBackup(dst, size)
}

// writeSnapshot is the single-file half, unchanged: snapshot to a `.part-…`
// file BESIDE the destination and rename it into place, so an interrupted run
// leaves a `.part-` file rather than a truncated file under the name an
// operator would restore from. It returns the finished size, and an exit code
// that is 0 only when the file is in place.
func (a *admin) writeSnapshot(dst string) (int64, int) {
	switch _, err := os.Lstat(dst); {
	case err == nil:
		fmt.Fprintf(a.env.stderr, "admin backup: %s already exists (refusing to overwrite)\n", dst)
		return 0, 1
	case !errors.Is(err, os.ErrNotExist):
		return 0, a.fail("backup", err)
	}
	tmp := dst + ".part-" + ids.New()
	// The part is CREATED 0600, empty, before `VACUUM INTO` writes it (§20
	// errata 96 and 321 (k)): SQLite writes into an existing EMPTY file, and
	// opens it rather than creating it, so the file that will hold every
	// bcrypt password hash, every session token hash and every API token hash
	// is never readable by anybody else — not for the write's whole duration,
	// and not if the run dies. The process umask (0022 in the image) can only
	// take bits away from 0600. Until erratum 321 (k) the file was made by
	// SQLite at 0644 and chmod'd after a SUCCESSFUL vacuum only.
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, a.fail("backup", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, a.fail("backup", err)
	}
	if a.beforeVacuum != nil {
		a.beforeVacuum(tmp)
	}
	if err := a.db.Backup(a.ctx, tmp); err != nil {
		// Every failure removes the part — a cancel (a clean SIGTERM under the
		// timed loop, run 8) and every other error alike: the half-written
		// part is this run's own and nobody will finish it (§20 erratum 321
		// (k)). Only a process killed mid-write still strands one, and that
		// one is 0600 from its first byte; rotation removes it once stale.
		_ = os.Remove(tmp)
		return 0, a.fail("backup", err)
	}
	fi, err := os.Stat(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return 0, a.fail("backup", err)
	}
	// Re-check under the smallest window we can: something else may have
	// created dst while VACUUM ran, and os.Rename would silently replace it.
	if _, err := os.Lstat(dst); err == nil {
		_ = os.Remove(tmp)
		fmt.Fprintf(a.env.stderr, "admin backup: %s appeared while the snapshot was being written; nothing was overwritten\n", dst)
		return 0, 1
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return 0, a.fail("backup", err)
	}
	return fi.Size(), 0
}

func (a *admin) reportBackup(dst string, size int64) int {
	versions, err := a.db.AppliedVersions(a.ctx)
	if err != nil {
		return a.fail("backup", err)
	}
	w := a.env.stdout
	fmt.Fprintf(w, "backup:  %s bytes=%d (%.1f MiB)\n", dst, size, float64(size)/(1<<20))
	fmt.Fprintf(w, "schema:  applied=%s\n", joinInts(versions))
	fmt.Fprintf(w, "source:  %s\n", dbPath(a.cfg))
	fmt.Fprintf(w, "warning: this file contains password and token hashes - keep it where you keep passwords\n")
	return 0
}

// restoreCheck is the dry run of a restore: it opens the named file read-only
// and reports whether it is a database at all, which migrations it carries and
// how many rows it holds. It is a package-level function, not a method,
// because it must work with no configuration, no live database and no bucket —
// the point is to check a backup somewhere else, in a throwaway container.
func restoreCheck(args []string, env adminEnv) int {
	fs := flag.NewFlagSet("clippet admin restore-check", flag.ContinueOnError)
	fs.SetOutput(env.stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || fs.Arg(0) == "" {
		fmt.Fprintln(env.stderr, "usage: clippet admin restore-check <path>")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	s, err := db.Inspect(ctx, fs.Arg(0))
	if err != nil {
		fmt.Fprintln(env.stderr, "admin restore-check:", err)
		return 1
	}
	w := env.stdout
	fmt.Fprintf(w, "file:      %s bytes=%d (%.1f MiB)\n", s.Path, s.Size, float64(s.Size)/(1<<20))
	fmt.Fprintf(w, "integrity: %s\n", s.Integrity)
	fmt.Fprintf(w, "schema:    applied=%s\n", joinInts(s.Applied))
	fmt.Fprintf(w, "rows:      users=%d clips=%d jobs=%d\n", s.Users, s.Clips, s.Jobs)
	if !s.OK() {
		fmt.Fprintln(env.stderr, "admin restore-check: this file is NOT safe to restore from")
		return 1
	}
	return 0
}

// --- rendering helpers -----------------------------------------------------

func rfc(ts int64) string { return time.Unix(ts, 0).UTC().Format(time.RFC3339) }

// joinInts renders the applied migration versions as "1,2" ("none" when the
// database has somehow none).
func joinInts(v []int) string {
	if len(v) == 0 {
		return "none"
	}
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

func rfcp(p *int64) string {
	if p == nil {
		return "-"
	}
	return rfc(*p)
}

func expiresp(p *int64) string {
	if p == nil {
		return "never"
	}
	return rfc(*p)
}

func ttlp(p *int64) string {
	if p == nil {
		return "never"
	}
	return (time.Duration(*p) * time.Second).String()
}

func strp(p *string) string {
	if p == nil {
		return "-"
	}
	return *p
}

func i64p(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func intp(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func boolp(p *bool) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprint(*p)
}

// The three tier-line formatters (§20 errata 200-206). Each turns an empty
// column into a word an operator can act on rather than a blank: "-" for a codec
// nobody probed, "unknown" for a tier state a row predating migration 0012 has,
// and nothing at all for a codec that has not been chosen yet.
func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func codecSuffix(codec string) string {
	if codec == "" {
		return ""
	}
	return "(" + codec + ")"
}
