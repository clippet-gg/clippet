//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

// freeBytes reports the bytes available to the unprivileged caller on the
// filesystem holding dir (the §8.3 scratch precheck).
func freeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// diskUsage reports the bytes available to the unprivileged caller AND the
// filesystem's total size for the filesystem holding dir (ROADMAP #13, §20
// erratum 147). `admin stats` and `admin doctor` print both, because "512 MiB
// free" means one thing on a 20 GiB volume and another on a 4 TiB one, and
// `507 insufficient_scratch` is a documented error whose whole cause is this
// number.
func diskUsage(dir string) (free, total int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize), nil
}

// sameDevice reports whether two paths live on the same filesystem. With the
// shipped compose file DATA_DIR and SCRATCH_DIR are two named volumes on one
// Docker filesystem, so "the scratch disk is full" and "the database has
// nowhere to write" are the same sentence — which is worth saying out loud
// rather than leaving an operator to infer it.
func sameDevice(a, b string) (bool, error) {
	da, err := deviceOf(a)
	if err != nil {
		return false, err
	}
	db, err := deviceOf(b)
	if err != nil {
		return false, err
	}
	return da == db, nil
}

func deviceOf(path string) (uint64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("stat %s: no unix file info", path)
	}
	return uint64(st.Dev), nil
}
