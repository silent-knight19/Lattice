package admin

// Small helpers used by the SEC-12 consolidated suite. They exist as thin wrappers so the
// test bodies read as attack descriptions rather than as filesystem plumbing.

import (
	"os"
	"runtime"

	latticeerrors "github.com/silent-knight19/lattice/internal/errors"
)

// osWriteFile writes a file with test permissions.
func osWriteFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}

// osSymlink creates a symbolic link.
func osSymlink(target, link string) error {
	return os.Symlink(target, link)
}

// runtimeNumGoroutine reports the current goroutine count.
func runtimeNumGoroutine() int { return runtime.NumGoroutine() }

// latticeInvalidPathError is a thin alias so the suite reads clearly at the call site while
// still exercising the REAL lattice error type and its path-bearing Error() string.
type latticeInvalidPathError = latticeerrors.InvalidPathError
