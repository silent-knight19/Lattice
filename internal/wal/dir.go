package wal

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/silent-knight19/lattice/internal/errors"
)

// DirName is the canonical subdirectory name for Write-Ahead Log segments under a database root.
const DirName = "wal"

// DirMode defines the restrictive POSIX directory permission mode (0700) for the WAL directory:
// owner: read + write + execute (rwx); group: none (---); others: none (---).
// This guarantees that other local system users cannot read, list, or tamper with WAL log files.
const DirMode os.FileMode = 0700

// Dir returns the platform-aware path to the WAL directory inside dbPath.
// It joins dbPath and DirName using filepath.Join, ensuring platform-specific separator handling
// and standard path lexical normalization without altering legitimate relative/absolute semantics.
func Dir(dbPath string) string {
	return filepath.Join(dbPath, DirName)
}

// DirPath is an alias for Dir, conforming to alternative naming conventions.
func DirPath(dbPath string) string {
	return Dir(dbPath)
}

// InitDir safely and idempotently initializes the WAL directory under dbPath with 0700 permissions.
//
// Invariants & Operational Semantics:
//  1. Path Construction:
//     Constructs the WAL directory path using platform-aware filepath.Join(dbPath, DirName).
//     Supports absolute paths, relative paths, nested hierarchies, and paths with spaces.
//     Fails fast with os.ErrInvalid if dbPath is empty.
//  2. Atomic Creation & TOCTOU Prevention:
//     Executes a direct atomic creation attempt via os.Mkdir(walPath, DirMode).
//     Avoids unsafe time-of-check-to-time-of-use (TOCTOU) sequences (e.g. Stat followed by Mkdir)
//     by relying strictly on kernel-level atomic creation failure modes.
//  3. Idempotent Convergence:
//     If walPath already exists, it verifies that the existing object is a genuine directory.
//     Pre-existing WAL segments and auxiliary files within the directory are never deleted or truncated.
//  4. Conflicting Object Rejection:
//     If walPath already exists as a regular file, special device, named pipe, or socket,
//     initialization immediately aborts with *errors.NotADirectoryError (matching errors.ErrNotADirectory).
//     The conflicting object is preserved untouched.
//  5. Symlink Rejection:
//     Uses os.Lstat to inspect existing paths without following symlinks. An existing symlink at walPath
//     is strictly rejected as a non-directory to eliminate symlink redirection and traversal hazards.
//  6. Permission Hardening:
//     If the WAL directory already exists but was initialized with looser permissions (e.g. 0755 or 0777),
//     it tightens permissions to DirMode (0700) via os.Chmod, removing group/other access rights.
//  7. Parent Directory Enforcement:
//     Does not blindly create arbitrary parent hierarchies. If the parent dbPath does not exist,
//     os.Mkdir returns an error wrapping fs.ErrNotExist, preserving authoritative OS error identity.
//  8. Concurrency Safety:
//     Concurrent invocations from multiple goroutines safely converge on the valid directory without
//     requiring global mutexes or producing race conditions.
func InitDir(dbPath string) (string, error) {
	if dbPath == "" {
		return "", fmt.Errorf("%w: db path cannot be empty", os.ErrInvalid)
	}

	walPath := Dir(dbPath)

	// Direct atomic creation attempt:
	// Eliminates TOCTOU race condition by letting the OS kernel authoritatively create the directory.
	err := os.Mkdir(walPath, DirMode)
	if err == nil {
		return walPath, nil
	}

	// If the error is anything other than "already exists", fail immediately
	// (e.g. fs.ErrNotExist if parent dbPath does not exist, fs.ErrPermission if parent is read-only).
	if !os.IsExist(err) {
		return "", fmt.Errorf("wal: failed to create directory %s: %w", walPath, err)
	}

	// The path already exists. Authoritatively inspect the existing filesystem entry using Lstat.
	// Lstat does not follow symlinks, which allows detecting if walPath is a symlink.
	info, lstatErr := os.Lstat(walPath)
	if lstatErr != nil {
		return "", fmt.Errorf("wal: failed to inspect existing path %s: %w", walPath, lstatErr)
	}

	// Security Defense: Symlinks at the WAL path are rejected to prevent redirection attacks.
	if info.Mode()&os.ModeSymlink != 0 {
		return "", &errors.NotADirectoryError{
			Path: walPath,
			Mode: info.Mode(),
		}
	}

	// Rejection of non-directories (regular files, devices, pipes, sockets)
	if !info.IsDir() {
		return "", &errors.NotADirectoryError{
			Path: walPath,
			Mode: info.Mode(),
		}
	}

	// Existing directory: tighten permissions if group or others possess any permission bits.
	if info.Mode().Perm()&0077 != 0 {
		if chmodErr := os.Chmod(walPath, DirMode); chmodErr != nil {
			return "", fmt.Errorf("wal: failed to tighten permissions on %s: %w", walPath, chmodErr)
		}
	}

	return walPath, nil
}
