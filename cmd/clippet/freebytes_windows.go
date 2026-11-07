//go:build windows

package main

import (
	"golang.org/x/sys/windows"
)

// The three disk helpers on Windows (§20 erratum 308).
//
// This package's contract is unchanged and is NOT the worker's: every helper
// returns its values AND an error, and every caller — `admin doctor`'s two
// disk rows and `admin stats` — prints "unavailable" and carries on when the
// error is non-nil. What changes on Windows is that the error stops being the
// only answer: freebytes_other.go answers errNoDiskStats for every platform
// that is neither unix nor windows, so `clippet.exe admin doctor` used to
// print "free space unavailable" on every row it has, and
// httpapi.scratchHasRoom — which treats a statfs failure as "cannot tell, let
// it through" — skipped the §8.3 precheck outright. The documented
// `507 insufficient_scratch` then could not be produced before the spool ran
// the disk out, which is the promise this file restores.
//
// GetDiskFreeSpaceEx is the API for all three numbers because its FIRST
// out-parameter is the free space available TO THE CALLER — what a per-user
// quota makes different from the volume's own free space, and the same
// distinction statfs draws between `Bavail` and `Bfree`. A precheck that read
// the volume's figure would promise room a quota will not give.

// freeBytes reports the bytes available to the caller on the volume holding
// dir (the §8.3 scratch precheck).
func freeBytes(dir string) (int64, error) {
	free, _, err := diskUsage(dir)
	return free, err
}

// diskUsage reports the bytes available to the caller AND the volume's total
// size for the volume holding dir (ROADMAP #13, §20 erratum 147) — the two
// numbers `admin stats` and `admin doctor` print side by side, because
// "512 MiB free" means one thing on a 20 GiB volume and another on a 4 TiB one.
func diskUsage(dir string) (free, total int64, err error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0, err
	}
	var availToCaller, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &availToCaller, &totalBytes, &totalFree); err != nil {
		return 0, 0, err
	}
	return int64Cap(availToCaller), int64Cap(totalBytes), nil
}

// int64Cap converts one of the API's uint64 byte counts to the int64 the rest
// of this program speaks, saturating rather than wrapping. No volume is 8 EiB,
// but a wrapped count would be NEGATIVE, and a negative free figure means
// "unknown, skip the check" to internal/worker — a silent loss of exactly the
// guard this file exists to provide.
func int64Cap(v uint64) int64 {
	const maxInt64 = uint64(1)<<63 - 1
	if v > maxInt64 {
		return int64(maxInt64)
	}
	return int64(v)
}

// sameDevice reports whether two paths live on the same volume. With the
// shipped compose file DATA_DIR and SCRATCH_DIR are two named volumes on one
// Docker filesystem, so "the scratch disk is full" and "the database has
// nowhere to write" are the same sentence — which is worth saying out loud
// rather than leaving an operator to infer it. On Windows the same question is
// "are these two paths on the same volume", and the answer is the MOUNT POINT
// each path lives under.
//
// GetVolumePathName is that answer, and it is the one chosen over the volume
// SERIAL (GetVolumeInformation) for three reasons:
//
//   - It is the only one of the two that distinguishes a volume MOUNTED INTO A
//     FOLDER from the drive it is mounted on: a data disk at `C:\clippet\data`
//     answers `C:\clippet\data\` while `C:\clippet\scratch` answers `C:\`.
//     Comparing drive letters — or asking GetVolumeInformation about `C:\` —
//     would call those two the same device and print the wrong sentence in
//     exactly the deployment that went to the trouble of separating them.
//   - It takes a PATH and opens nothing. GetVolumeInformation wants a volume
//     ROOT, which means resolving the path to its root first — i.e. calling
//     this — so the serial is a second call and a second failure mode on top
//     of this one, for an answer this one already gave.
//   - It answers for a directory this process may not be able to open, which
//     is the state `admin doctor` is most often run to diagnose.
func sameDevice(a, b string) (bool, error) {
	va, err := volumePathName(a)
	if err != nil {
		return false, err
	}
	vb, err := volumePathName(b)
	if err != nil {
		return false, err
	}
	return sameVolumePath(va, vb), nil
}

// volumePathName returns the mount point path lives under — `C:\`, or
// `C:\mounted\folder\` for a volume mounted into a directory.
//
// The buffer is sized from the input because the answer is always a PREFIX of
// the path that was asked about: one UTF-16 unit per UTF-8 byte is always
// enough (a BMP rune is 1 unit and at least 1 byte; an astral rune is 2 units
// and 4 bytes), and the +1 is the terminator. MAX_PATH is the floor for a
// short relative path, which the API answers for with the boot volume's root.
// Sizing it any other way is how this call returns
// ERROR_FILENAME_EXCED_RANGE on a long path and the doctor reports a disk
// problem that is really a buffer.
func volumePathName(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	n := len(path) + 1
	if n < windows.MAX_PATH+1 {
		n = windows.MAX_PATH + 1
	}
	buf := make([]uint16, n)
	if err := windows.GetVolumePathName(p, &buf[0], uint32(len(buf))); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf), nil
}
