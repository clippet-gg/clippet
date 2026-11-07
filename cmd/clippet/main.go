// Command clippet is the single binary that runs the HTTP server, the
// transcode worker and the expiry sweeper (`serve`, the default), applies
// migrations standalone (`migrate`) and hosts the operator CLI (`admin …`).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
)

// version is set at build time via -ldflags "-X main.version=…".
var version = "dev"

// commit is the git object name this binary was built from, set at build time
// via -ldflags "-X main.commit=…" from the Dockerfile's `COMMIT` argument
// (§20 erratum 303). It defaults to EMPTY rather than to a word: `version`
// can honestly call an unstamped build `dev`, while there is no honest stand-in
// for a commit, and `GET /api/instance` publishes an absent one as JSON `null`.
// The release runbook sets it from `git rev-parse HEAD` (DEPLOY.md §17.4).
var commit = ""

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: clippet [serve | migrate | admin <subcommand> … | version]")
}

// run dispatches the subcommand and returns the exit code.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	case "version", "-version", "--version":
		fmt.Fprintln(stdout, "clippet "+version)
		return 0
	}
	// `admin restore-check <file>` inspects ONE file read-only and talks to
	// nothing else, so it deliberately runs BEFORE configuration is loaded:
	// its whole point is to validate a backup in a throwaway container that
	// has no `.env`, no bucket and no database of its own (§20 erratum 83).
	if cmd == "admin" && len(args) > 0 && args[0] == "restore-check" {
		return restoreCheck(args[1:], adminEnv{stdout: stdout, stderr: stderr, now: time.Now})
	}
	// `admin doctor` runs before the loader too, for the opposite reason
	// (§20 erratum 143): a broken configuration is the thing it is there to
	// DESCRIBE. Dispatched after config.Load, the first typo in `.env` would
	// exit 2 with one line and the preflight would never run — which is
	// precisely the situation an operator reaches for it in.
	if cmd == "admin" && len(args) > 0 && args[0] == "doctor" {
		return runDoctor(args[1:], getenv, adminEnv{stdout: stdout, stderr: stderr, now: time.Now, getenv: getenv})
	}
	// `admin storage migrate` is the one verb that holds BOTH stores open, so
	// it is the one place STORAGE_BACKEND=fs may be read beside a configured
	// bucket (§20 erratum 276). Routing it here — rather than letting an
	// environment variable relax the rule — is what makes the relaxation
	// unreachable from `serve`.
	load := config.Load
	if cmd == "admin" && isStorageMigrate(args) {
		load = config.LoadForMigrate
	}
	cfg, err := load(getenv)
	if err != nil {
		// One line per bad variable (§20 erratum 104). With
		// `restart: unless-stopped` an operator reads these out of
		// `docker compose logs` after a crash loop, and a list they can fix in
		// one pass is worth a few extra lines; a single failure prints exactly
		// what it always did.
		for _, e := range configErrors(err) {
			fmt.Fprintln(stderr, "clippet: config:", e)
		}
		return 2
	}
	switch cmd {
	case "serve":
		return runServe(cfg)
	case "migrate":
		return runMigrate(cfg)
	case "admin":
		return runAdmin(args, cfg)
	}
	fmt.Fprintf(stderr, "clippet: unknown command %q\n", cmd)
	usage(stderr)
	return 2
}

// isStorageMigrate reports whether these `admin` arguments are the migrate
// verb. It matches the SUBCOMMAND only — flags may follow in any order, and
// `admin storage` with anything else after it is not it.
func isStorageMigrate(args []string) bool {
	return len(args) >= 2 && args[0] == "storage" && args[1] == "migrate"
}

// configErrors flattens what config.Load returned into the lines to print.
// Anything that is not the aggregate prints as itself.
func configErrors(err error) []error {
	var ce *config.Errors
	if errors.As(err, &ce) && len(ce.Errs) > 0 {
		return ce.Errs
	}
	return []error{err}
}

// dbPath is where the SQLite file lives under DATA_DIR.
func dbPath(cfg config.Config) string {
	return filepath.Join(cfg.DataDir, "clippet.db")
}

// runMigrate opens the database, applies pending migrations and prints the
// applied versions. Exit code 0 on success, 1 on any failure.
func runMigrate(cfg config.Config) int {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "migrate: data dir:", err)
		return 1
	}
	d, err := db.Open(dbPath(cfg))
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate: open:", err)
		return 1
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.Migrate(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		return 1
	}
	versions, err := d.AppliedVersions(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate: read versions:", err)
		return 1
	}
	fmt.Printf("migrate: %s applied=%v\n", dbPath(cfg), versions)
	return 0
}
