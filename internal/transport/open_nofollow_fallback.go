//go:build !linux && !darwin

package transport

import "os"

// openFileNoFollow fallback for platforms without syscall.O_NOFOLLOW (e.g. Windows).
// Plain open is performed; pre/post Lstat + double os.SameFile descriptor pinning
// provides the defense-in-depth symlink and TOCTOU defense.
func openFileNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag, perm)
}
