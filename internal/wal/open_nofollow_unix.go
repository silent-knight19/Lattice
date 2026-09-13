//go:build linux || darwin

package wal

import (
	"os"
	"syscall"
)

// openFileNoFollow opens path with O_NOFOLLOW to fail closed (ELOOP) if the
// final component is a symlink. Falls back to plain open if kernel returns
// ENOSYS (should not happen on linux/darwin).
func openFileNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag|syscall.O_NOFOLLOW, perm)
}
