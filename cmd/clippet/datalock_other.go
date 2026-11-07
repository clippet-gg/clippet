//go:build !unix && !windows

package main

import (
	"errors"
	"os"
)

// errNoFileLock is every platform that is neither unix nor windows: the lock
// cannot be attempted, which the server and the restore word as "cannot tell"
// (§20 erratum 343).
var errNoFileLock = errors.New("file locking is not supported on this platform")

func tryLockFile(*os.File, bool) error { return errNoFileLock }
func unlockFile(*os.File) error        { return nil }
