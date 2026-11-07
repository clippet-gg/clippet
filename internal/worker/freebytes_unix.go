//go:build unix

package worker

import "syscall"

// freeBytes reports the bytes available to the unprivileged caller on the
// filesystem holding dir (the §10.2 scratch check before encoding).
func freeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
