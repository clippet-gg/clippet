//go:build !unix && !windows

package main

import "errors"

// errNoDiskStats is what the three disk helpers answer on a platform that is
// neither unix nor windows (§20 erratum 308 narrowed this file's constraint
// from `!unix`); the binary runs in a Linux container (DESIGN.md §2) and every
// caller degrades to "unavailable" rather than failing.
var errNoDiskStats = errors.New("disk statistics: unsupported platform")

// freeBytes is implemented for unix and windows targets; the binary runs in a
// Linux container (DESIGN.md §2), and a Windows operator runs clippet.exe
// (freebytes_windows.go).
func freeBytes(dir string) (int64, error) {
	return 0, errNoDiskStats
}

// diskUsage is not implemented here (see freeBytes).
func diskUsage(dir string) (free, total int64, err error) {
	return 0, 0, errNoDiskStats
}

// sameDevice is not implemented here (see freeBytes).
func sameDevice(a, b string) (bool, error) {
	return false, errNoDiskStats
}
