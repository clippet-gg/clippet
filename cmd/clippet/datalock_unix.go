//go:build unix

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLockFile takes a flock on f without blocking (§20 erratum 343). flock
// locks belong to the open file description, so a second open of the same file
// — another container on the same volume, or the same process — conflicts
// exactly as another process would.
func tryLockFile(f *os.File, exclusive bool) error {
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return errDataLockHeld
	}
	return err
}

func unlockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }
