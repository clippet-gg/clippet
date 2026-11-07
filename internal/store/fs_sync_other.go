//go:build !windows

package store

import (
	"errors"
	"os"
	"syscall"
)

// syncDir fsyncs a directory so a rename into it survives a power cut.
//
// It TOLERATES the errnos that mean "this filesystem does not implement
// that", because the deployment this backend exists for is a mounted share and
// SMB/CIFS is one of them (DEPLOY §32 says so out loud rather than leaving an
// operator to discover that every upload fails). The rename itself is still
// atomic; what is lost on such a share is the promise about the metadata
// reaching the platter, which is the share's promise to make, not ours.
//
// Windows has its own file (fs_sync_windows.go, §20 erratum 336).
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if err != nil && fsSyncUnsupported(err) {
		return nil
	}
	return err
}

func fsSyncUnsupported(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.ENOSYS) || errors.Is(err, errors.ErrUnsupported)
}
