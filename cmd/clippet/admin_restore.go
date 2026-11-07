package main

// admin_restore.go — `clippet admin restore --from-store [--yes]` (§20 errata
// 343, 348, DEPLOY §15): come up on the snapshot the store holds — the bucket,
// or on `fs` the media folder's own `_clippet/` (erratum 348).
//
// The recovery recipe is a new machine, the bucket's keys in `.env` (or the
// media folder restored from its backup and bound as STORAGE_DIR), and this
// verb: it fetches `_clippet/db.sqlite` and its sha256 into SCRATCH_DIR,
// verifies the one against the other, runs restore-check's own inspection on
// it, and prints what it found. Only with --yes does it change anything, and
// then exactly two things: the database that is there (with its -wal and -shm)
// is moved aside as clippet.db.before-restore-<UTC>, and the snapshot takes its
// place. The links are alive the moment the server starts, because the clips
// never left the bucket.
//
// It runs before the database is opened, like `objects`: on the machine it
// exists for there is no database yet. And it takes DATA_DIR/serve.lock
// EXCLUSIVE first (datalock.go): replacing a SQLite file under a running server
// loses it, so a running server is a refusal, and a server started while this
// runs is refused at its boot.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/store"
)

func restoreUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: clippet admin restore --from-store [--yes]")
	fmt.Fprintln(w, "  fetches the database snapshot the store holds (_clippet/db.sqlite, in the bucket or the media folder), verifies it and prints what it found;")
	fmt.Fprintln(w, "  --yes moves the current database aside as clippet.db.before-restore-<UTC> and puts the snapshot in its place.")
	fmt.Fprintln(w, "  The server must be stopped.")
}

func adminRestore(args []string, cfg config.Config, env adminEnv) int {
	fs := flag.NewFlagSet("clippet admin restore", flag.ContinueOnError)
	fs.SetOutput(env.stderr)
	fromStore := fs.Bool("from-store", false, "restore from the snapshot in the store (the only source there is)")
	yes := fs.Bool("yes", false, "put the snapshot in place (without it nothing is changed)")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || !*fromStore {
		restoreUsage(env.stderr)
		return 2
	}
	for _, dir := range []string{cfg.DataDir, cfg.ScratchDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintln(env.stderr, "admin restore:", err)
			return 1
		}
	}
	release, err := lockDataDir(cfg.DataDir, true)
	switch {
	case errors.Is(err, errDataLockHeld):
		fmt.Fprintf(env.stderr, "admin restore: the server is running on this database (%s is held): stop it first - nothing was changed\n",
			filepath.Join(cfg.DataDir, dataLockName))
		return 1
	case err != nil:
		fmt.Fprintf(env.stderr, "admin restore: cannot tell whether a server is running on this database (%v): make sure it is stopped\n", err)
	default:
		defer release()
	}

	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	a := &admin{cfg: cfg, env: env, ctx: ctx}
	st, err := a.store()
	if err != nil {
		return a.fail("restore", err)
	}
	prefix := cfg.S3.KeyPrefix
	key := store.SnapshotKey(prefix)
	work, err := os.MkdirTemp(cfg.ScratchDir, ".db-restore-")
	if err != nil {
		return a.fail("restore", err)
	}
	defer os.RemoveAll(work)
	file := filepath.Join(work, "db.sqlite")
	if err := st.Download(ctx, key, file, nil); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fmt.Fprintf(env.stderr, "admin restore: the store holds no snapshot at %s - nothing to restore\n", key)
			return 1
		}
		return a.fail("restore", err)
	}
	sumRaw, err := readSmall(ctx, st, store.SnapshotSumKey(prefix))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fmt.Fprintf(env.stderr, "admin restore: %s has no sha256 beside it (a write that never finished) - nothing was changed\n", key)
			return 1
		}
		return a.fail("restore", err)
	}
	got, err := fileSHA256(file)
	if err != nil {
		return a.fail("restore", err)
	}
	if want := trimHex(sumRaw); got != want {
		fmt.Fprintf(env.stderr, "admin restore: %s does not match its sha256 (got %s, the sidecar says %s): a write in progress, or damage - nothing was changed\n",
			key, got, want)
		return 1
	}
	insp, err := db.Inspect(ctx, file)
	if err != nil {
		return a.fail("restore", err)
	}
	w := env.stdout
	fmt.Fprintf(w, "snapshot:  %s bytes=%d (%s) sha256 ok\n", key, insp.Size, humanBytes(insp.Size))
	now := env.now()
	var meta snapshotMeta
	if raw, err := readSmall(ctx, st, store.SnapshotMetaKey(prefix)); err == nil && json.Unmarshal(raw, &meta) == nil && meta.WrittenAt != "" {
		at, _ := parseRFCTime(meta.WrittenAt)
		fmt.Fprintf(w, "written:   %s (%s) commit=%s\n", meta.WrittenAt, snapshotAgo(now, at), orNone(meta.Commit))
		if meta.SHA256 != "" && meta.SHA256 != got {
			fmt.Fprintln(w, "note:      the meta describes a later write that did not finish; this is the one before it, and it verified")
		}
	} else {
		fmt.Fprintln(w, "written:   unknown (no readable meta beside it)")
	}
	fmt.Fprintf(w, "integrity: %s\n", insp.Integrity)
	fmt.Fprintf(w, "schema:    applied=%s\n", joinInts(insp.Applied))
	fmt.Fprintf(w, "rows:      users=%d clips=%d jobs=%d\n", insp.Users, insp.Clips, insp.Jobs)
	if !insp.OK() {
		fmt.Fprintln(env.stderr, "admin restore: this snapshot is NOT safe to restore from - nothing was changed")
		return 1
	}
	dst := dbPath(cfg)
	current := "(none: this machine has no database yet)"
	if fi, err := os.Stat(dst); err == nil {
		current = fmt.Sprintf("%s bytes=%d", dst, fi.Size())
	}
	fmt.Fprintf(w, "current:   %s\n", current)
	stamp := now.UTC().Format(snapshotStamp)
	aside := dst + ".before-restore-" + stamp
	if !*yes {
		fmt.Fprintf(w, "dry run:   nothing was changed; --yes moves the current database aside as %s and puts this snapshot in its place\n", aside)
		return 0
	}

	// The copy lands BESIDE the database first, so the swap below is two
	// renames on one filesystem and the window without a clippet.db is theirs.
	part := dst + ".part-" + ids.New()
	if err := copyFile0600(file, part); err != nil {
		os.Remove(part)
		return a.fail("restore", err)
	}
	moved := false
	for _, suffix := range []string{"", "-wal", "-shm"} {
		// The -wal and -shm travel with their database, renamed the way SQLite
		// looks for them (<file>-wal), so the aside copy opens whole — and no
		// stale WAL is left to be replayed into the snapshot.
		if _, err := os.Lstat(dst + suffix); err != nil {
			continue
		}
		if err := os.Rename(dst+suffix, aside+suffix); err != nil {
			os.Remove(part)
			return a.fail("restore", fmt.Errorf("moving %s aside: %w", dst+suffix, err))
		}
		moved = moved || suffix == ""
	}
	if err := os.Rename(part, dst); err != nil {
		os.Remove(part)
		return a.fail("restore", err)
	}
	if moved {
		fmt.Fprintf(w, "aside:     %s (keep it until the instance looks right)\n", aside)
	}
	fmt.Fprintf(w, "restored:  %s from %s - start the server; the links are alive once it is up\n", dst, key)
	return 0
}

// copyFile0600 copies src to dst (which must not exist), 0600, synced.
func copyFile0600(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
