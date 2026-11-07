//go:build windows

package worker

import "golang.org/x/sys/windows"

// freeBytes reports the bytes available to the caller on the volume holding
// dir (the §10.2 scratch check before encoding), §20 erratum 308.
//
// This package's contract is its OWN and is not cmd/clippet's: a NEGATIVE
// value means "cannot tell" and requireScratch skips the check, while an error
// fails the stage that asked. This file answers exactly as the unix one does —
// a real number, or the API's error — so on Windows the check is now made
// instead of skipped. freebytes_other.go's `-1, nil` used to cover every
// non-unix platform, which meant an encode that could not fit was discovered
// by ffmpeg running the disk out rather than refused before it started.
//
// GetDiskFreeSpaceEx's first out-parameter is the free space available TO THE
// CALLER, which is what a per-user quota makes different from the volume's own
// free space (freebytes_windows.go in cmd/clippet says the same, for the same
// reason: both are prechecks, and a precheck must not promise room a quota
// will not give).
func freeBytes(dir string) (int64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var availToCaller, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &availToCaller, &totalBytes, &totalFree); err != nil {
		return 0, err
	}
	// Saturate rather than wrap: a wrapped count would be negative, and a
	// negative figure is this package's "unknown, skip the check" — the one
	// answer this file exists to stop giving.
	const maxInt64 = uint64(1)<<63 - 1
	if availToCaller > maxInt64 {
		return int64(maxInt64), nil
	}
	return int64(availToCaller), nil
}
