package sstable

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/silent-knight19/lattice/internal/errors"
)

// isSystemSymlinkPrefix checks if a path component is a standard Darwin/macOS system symlink
// (such as /var -> /private/var, /tmp -> /private/tmp, /etc -> /private/etc).
func isSystemSymlinkPrefix(path string) bool {
	if runtime.GOOS == "darwin" {
		clean := filepath.Clean(path)
		// Try direct match first
		if clean == "/var" || clean == "/tmp" || clean == "/etc" {
			return true
		}
		// For relative paths, resolve to absolute and check
		abs, err := filepath.Abs(clean)
		if err == nil {
			if abs == "/var" || abs == "/tmp" || abs == "/etc" {
				return true
			}
		}
	}
	return false
}

// validatePathNoSymlinks inspects each existing path component from the root down to dir.
// If any component is an unpermitted symbolic link, it returns an error wrapping ErrParentDirectorySymlink.
// If any component is not a directory, it returns an error wrapping ErrNotADirectory.
func validatePathNoSymlinks(dir string) error {
	clean := filepath.Clean(dir)
	if clean == "." || clean == "" {
		return nil
	}
	vol := filepath.VolumeName(clean)
	rest := clean[len(vol):]
	if rest == "" || rest == string(filepath.Separator) {
		return nil
	}

	parts := strings.Split(rest, string(filepath.Separator))
	curr := vol
	if strings.HasPrefix(rest, string(filepath.Separator)) {
		curr += string(filepath.Separator)
	}

	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		curr = filepath.Join(curr, part)
		fi, err := os.Lstat(curr)
		if err != nil {
			if os.IsNotExist(err) {
				// Component does not exist yet (will be created by MkdirAll)
				continue
			}
			return err
		}
		if isSystemSymlinkPrefix(curr) {
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: path component %q is a symlink", errors.ErrParentDirectorySymlink, curr)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%w: path component %q is not a directory", errors.ErrNotADirectory, curr)
		}
	}
	return nil
}
