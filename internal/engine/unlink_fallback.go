//go:build !linux && !darwin

package engine

import (
	"os"
	"path/filepath"
)

// unlinkAt provides a pathname-based deletion fallback for non-Unix operating systems
// (such as Windows) that lack a native descriptor-relative unlinkat system call in Go stdlib.
//
// NOTE: On platforms using this fallback, pathname resolution is subject to potential
// parent-directory replacement races if an attacker can manipulate directory paths concurrently.
func unlinkAt(dirFile *os.File, name string) error {
	if dirFile == nil {
		return os.ErrInvalid
	}
	return os.Remove(filepath.Join(dirFile.Name(), name))
}
