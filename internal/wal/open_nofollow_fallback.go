//go:build !linux && !darwin

package wal

import "os"

// openFileNoFollow fallback for platforms without O_NOFOLLOW (e.g. Windows):
// plain open; Lstat + double SameFile pinning in callers remains the defense.
func openFileNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag, perm)
}
