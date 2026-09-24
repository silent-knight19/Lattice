//go:build linux || darwin

package transport

import (
	"os"
	"syscall"
)

// openFileNoFollow opens path with O_NOFOLLOW to fail closed (ELOOP) if the
// final path component is a symlink.
func openFileNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag|syscall.O_NOFOLLOW, perm)
}
