//go:build !unix && !windows

package worker

// freeBytes is implemented for unix and windows targets (§20 erratum 308
// narrowed this file's constraint from `!unix`); the binary runs in a Linux
// container (DESIGN.md §2) and a Windows operator runs clippet.exe. Everywhere
// else a negative value skips the scratch check.
func freeBytes(dir string) (int64, error) {
	return -1, nil
}
