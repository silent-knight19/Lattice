package version

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/silent-knight19/lattice/internal/errors"
)

const (
	// CurrentFilename is the canonical name of the active manifest pointer file.
	// In an LSM-tree database directory, CURRENT identifies which MANIFEST-NNNNNN
	// log contains the authoritative state of SSTable levels and sequence numbers.
	CurrentFilename = "CURRENT"

	// CurrentTempFilename defines the deterministic staging filename used for atomic replacement.
	// Staging changes in CURRENT.tmp prevents incomplete or torn writes from corrupting the pointer.
	CurrentTempFilename = "CURRENT.tmp"

	// CurrentFileMode defines the restrictive POSIX file permissions (0600) for the CURRENT pointer:
	// owner: read + write (rw-); group: none (---); others: none (---).
	// This ensures unprivileged local users cannot inspect or redirect active database metadata.
	CurrentFileMode os.FileMode = 0600
)

// Pluggable filesystem seams for deterministic fault-injection testing
var (
	currentWriteFn   = func(f *os.File, p []byte) (int, error) { return f.Write(p) }
	currentSyncFn    = fdatasync
	currentCloseFn   = func(f *os.File) error { return f.Close() }
	currentRenameFn  = os.Rename
	currentSyncDirFn = syncDir
)

// syncDir flushes modified directory entries to stable storage media.
// On Unix/Linux/Darwin, opening the directory descriptor and calling Sync() forces directory
// block updates (including the atomic rename of CURRENT.tmp -> CURRENT) down to disk.
// On Windows, the operating system does not support fsync on directory handles, so directory
// syncing is safely bypassed without returning a false-positive error.
func syncDir(dirPath string) error {
	df, err := os.Open(dirPath)
	if err != nil {
		return err
	}
	defer func() { _ = df.Close() }()

	if err := df.Sync(); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return nil
}

// SetCurrentManifest atomically and durably updates the CURRENT pointer file within dir
// to point to the designated manifest number.
//
// Operational Lifecycle & Durability Guarantee:
//  1. Argument Validation:
//     Fails fast if dir is empty or manifestNum is 0 (manifest numbers start at 1).
//  2. Directory Verification:
//     Ensures dir exists, is a genuine directory, and is not a symbolic link.
//  3. Symlink Defense:
//     Rejects existing symlinks at both CURRENT and CURRENT.tmp to eliminate symlink
//     redirection, path traversal, and unauthorized file overwrite vulnerabilities.
//  4. Stale Temporary File Cleanup:
//     Safely purges any unlinked, stale regular file at CURRENT.tmp left behind by a prior crash.
//  5. Exclusive Temporary Creation:
//     Creates CURRENT.tmp with os.O_WRONLY | os.O_CREATE | os.O_EXCL and 0600 permissions.
//     Pins the opened descriptor to the disk inode via os.SameFile.
//  6. Deterministic Serialization:
//     Writes the canonical manifest filename followed by a newline: "MANIFEST-%06d\n".
//  7. Hardware Durability Barrier:
//     Synchronizes CURRENT.tmp via fdatasync() (or f.Sync() fallback) to ensure content
//     pages are fully committed to physical non-volatile storage media.
//  8. File Descriptor Cleanup:
//     Closes the temporary file handle prior to atomic replacement.
//  9. Atomic Pointer Swap:
//     Atomically replaces CURRENT via os.Rename(CURRENT.tmp, CURRENT). At no point does
//     CURRENT exist in an empty, partially written, or torn state.
//  10. Parent Directory Durability Barrier:
//     Flushes the parent directory's modified entries to persistent storage, guaranteeing
//     that the rename operation itself survives host power interruption.
//  11. Failure Safety & Invariant:
//     If any failure occurs prior to the atomic rename, CURRENT.tmp is unlinked and
//     the pre-existing CURRENT pointer remains completely untouched.
func SetCurrentManifest(dir string, manifestNum uint64) error {
	if dir == "" {
		return fmt.Errorf("%w: directory path cannot be empty", os.ErrInvalid)
	}
	if manifestNum == 0 {
		return errors.ErrInvalidManifestNum
	}

	cleanDir := filepath.Clean(dir)

	// 1. Verify parent directory exists, is a genuine directory, and is not a symlink
	dirInfo, err := os.Lstat(cleanDir)
	if err != nil {
		return fmt.Errorf("current: failed to inspect directory %s: %w", cleanDir, err)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: directory %s cannot be a symbolic link", os.ErrInvalid, cleanDir)
	}
	if !dirInfo.IsDir() {
		return &errors.NotADirectoryError{
			Path: cleanDir,
			Mode: dirInfo.Mode(),
		}
	}

	currentPath := filepath.Join(cleanDir, CurrentFilename)
	tmpPath := filepath.Join(cleanDir, CurrentTempFilename)

	// 2. Symlink & object defense on target currentPath
	if cInfo, err := os.Lstat(currentPath); err == nil {
		if cInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: target %s is a symlink", errors.ErrCurrentSymlink, currentPath)
		}
		if cInfo.IsDir() {
			return &errors.NotADirectoryError{
				Path: currentPath,
				Mode: cInfo.Mode(),
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("current: failed to inspect %s: %w", currentPath, err)
	}

	// 3. Symlink defense on tmpPath and cleanup of stale regular file
	if tInfo, err := os.Lstat(tmpPath); err == nil {
		if tInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: temporary file path %s is a symlink", errors.ErrCurrentSymlink, tmpPath)
		}
		if tInfo.IsDir() {
			return &errors.NotADirectoryError{
				Path: tmpPath,
				Mode: tInfo.Mode(),
			}
		}
		// Stale regular file left over from a prior interrupted write: clean up
		if err := os.Remove(tmpPath); err != nil {
			return fmt.Errorf("current: failed to remove stale temporary file %s: %w", tmpPath, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("current: failed to inspect temporary file %s: %w", tmpPath, err)
	}

	// 4. Create CURRENT.tmp with O_WRONLY | O_CREATE | os.O_EXCL and 0600 mode
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	f, err := os.OpenFile(tmpPath, flags, CurrentFileMode)
	if err != nil {
		return fmt.Errorf("current: failed to create temporary file %s: %w", tmpPath, err)
	}

	// Track cleanup responsibility: if anything fails before rename, unlink tmpPath
	needsCleanup := true
	defer func() {
		if needsCleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	// Inode pinning & regular file verification
	finfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("current: failed to stat created temporary file: %w", err)
	}
	if !finfo.Mode().IsRegular() {
		_ = f.Close()
		return fmt.Errorf("current: temporary path is not a regular file: %w", os.ErrInvalid)
	}
	postInfo, err := os.Lstat(tmpPath)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("current: failed to lstat temporary file: %w", err)
	}
	if !os.SameFile(finfo, postInfo) {
		_ = f.Close()
		return fmt.Errorf("current: temporary file was replaced during creation: %w", os.ErrInvalid)
	}

	// 5. Serialize canonical manifest filename: "MANIFEST-%06d\n"
	manifestName := ManifestFilename(manifestNum)
	content := []byte(manifestName + "\n")

	var written int
	for written < len(content) {
		n, writeErr := currentWriteFn(f, content[written:])
		if n > 0 {
			written += n
		}
		if writeErr != nil {
			_ = f.Close()
			return fmt.Errorf("current: write to %s failed after %d/%d bytes: %w", tmpPath, written, len(content), writeErr)
		}
		if n == 0 {
			_ = f.Close()
			return fmt.Errorf("current: short write with 0 bytes to %s: %w", tmpPath, io.ErrShortWrite)
		}
	}

	// 6. Synchronize temporary file to non-volatile storage
	if err := currentSyncFn(f); err != nil {
		_ = f.Close()
		return fmt.Errorf("current: sync of %s failed: %w", tmpPath, err)
	}

	// 7. Close temporary file descriptor before atomic rename
	if err := currentCloseFn(f); err != nil {
		return fmt.Errorf("current: close of %s failed: %w", tmpPath, err)
	}

	// 8. Atomically replace CURRENT with CURRENT.tmp
	if err := currentRenameFn(tmpPath, currentPath); err != nil {
		return fmt.Errorf("current: rename %s to %s failed: %w", tmpPath, currentPath, err)
	}

	// Atomic rename succeeded! Staging file is now at currentPath; cleanup is disengaged
	needsCleanup = false

	// 9. Parent directory durability barrier
	if err := currentSyncDirFn(cleanDir); err != nil {
		return fmt.Errorf("%w: directory %s: %w", errors.ErrCurrentDirectorySync, cleanDir, err)
	}

	return nil
}
