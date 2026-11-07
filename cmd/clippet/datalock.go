package main

// datalock.go — DATA_DIR/serve.lock, the one way a second process can tell
// that `clippet serve` has this database open (§20 erratum 343).
//
// `admin restore --from-store` replaces clippet.db, and replacing a SQLite file
// under a process that holds it open is how a database is lost: the server
// keeps writing into the inode it has, its WAL is then applied to a file it was
// never written against, or both. So every server holds a SHARED advisory lock
// on a file beside the database for its whole life, and the restore takes it
// EXCLUSIVE before it touches anything, holding it until it is done. Shared,
// because more than one server over one database is a supported shape (WAL; the
// E2E runs two in one container); exclusive for the restore, so it is refused
// while any of them runs, and a server started DURING a restore is refused at
// boot rather than opening a half-swapped directory.
//
// The lock is the OS's (flock on unix, LockFileEx on Windows): a process that
// dies, however it dies, releases it, so there is no stale-lock state to clean
// up by hand. A filesystem that cannot lock at all (some network mounts) is not
// a refusal for the server — it boots, with one WARN — and is a sentence for
// the restore, which then says it cannot tell and asks the operator to make sure.

import (
	"errors"
	"os"
	"path/filepath"
)

// dataLockName is the lock file's name under DATA_DIR. Empty, never read: the
// lock is the point, not the content.
const dataLockName = "serve.lock"

// errDataLockHeld is the one answer that means "another process has it" in a
// way that conflicts with what was asked for.
var errDataLockHeld = errors.New("another clippet process holds DATA_DIR's serve.lock")

// lockDataDir takes DATA_DIR/serve.lock without waiting, shared or exclusive.
// It returns the release func on success, errDataLockHeld when another process
// holds it in a conflicting mode, and any other error when the lock could not
// be attempted (the file could not be opened, or the filesystem does not lock).
func lockDataDir(dataDir string, exclusive bool) (release func(), err error) {
	f, err := os.OpenFile(filepath.Join(dataDir, dataLockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := tryLockFile(f, exclusive); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}
