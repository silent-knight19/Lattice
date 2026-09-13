//go:build !linux && !darwin

package version

import (
	"os"
	"path/filepath"
)

// createTempAt provides a pathname-based temporary file creation fallback for non-Unix platforms.
//
// NOTE: On platforms using this fallback, pathname resolution is subject to potential
// parent-directory replacement races if an attacker can manipulate directory paths concurrently.
func createTempAt(dirFile *os.File, name string, perm os.FileMode) (*os.File, error) {
	if dirFile == nil {
		return nil, os.ErrInvalid
	}
	path := filepath.Join(dirFile.Name(), name)
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
}

// renameAt provides a pathname-based rename fallback for non-Unix platforms.
//
// NOTE: On platforms using this fallback, pathname resolution is subject to potential
// parent-directory replacement races if an attacker can manipulate directory paths concurrently.
func renameAt(dirFile *os.File, oldName, newName string) error {
	if dirFile == nil {
		return os.ErrInvalid
	}
	oldPath := filepath.Join(dirFile.Name(), oldName)
	newPath := filepath.Join(dirFile.Name(), newName)
	return os.Rename(oldPath, newPath)
}

// removeAt provides a pathname-based removal fallback for non-Unix platforms.
//
// NOTE: On platforms using this fallback, pathname resolution is subject to potential
// parent-directory replacement races if an attacker can manipulate directory paths concurrently.
func removeAt(dirFile *os.File, name string) error {
	if dirFile == nil {
		return os.ErrInvalid
	}
	return os.Remove(filepath.Join(dirFile.Name(), name))
}
