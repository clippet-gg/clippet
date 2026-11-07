//go:build windows

package store

// syncDir is a no-op on Windows (§20 erratum 336). There is no directory
// fsync there: os.Open on a directory yields a handle without write access,
// and FlushFileBuffers on it fails with ERROR_ACCESS_DENIED — so the Linux
// body made every PutFile report a failure AFTER its object was already in
// place, and the desktop app's first Move logged every clip as FAILED. NTFS
// journals the rename itself; the object's own bytes are made durable by the
// temp file's Sync before the rename, which is the half of the promise this
// platform can keep.
func syncDir(dir string) error { return nil }
