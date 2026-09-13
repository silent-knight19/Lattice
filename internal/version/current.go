package version

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"io"
	"math"
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

	// MinCurrentFileSize defines the minimal valid byte length of a canonical CURRENT file:
	// "MANIFEST-000001\n" = 16 bytes.
	MinCurrentFileSize = 16

	// MaxCurrentFileSize defines the maximum valid byte length of a canonical CURRENT file:
	// "MANIFEST-" (9 bytes) + 20 decimal digits (math.MaxUint64 = 18446744073709551615) + "\n" (1 byte) = 30 bytes.
	MaxCurrentFileSize = 30
)

// Pluggable filesystem seams for deterministic fault-injection testing
var (
	currentWriteFn   = func(f *os.File, p []byte) (int, error) { return f.Write(p) }
	currentSyncFn    = fdatasync
	currentCloseFn   = func(f *os.File) error { return f.Close() }
	currentRenameFn  = os.Rename
	currentSyncDirFn = syncDir
	currentOpenFn    = func(name string) (*os.File, error) { return os.Open(name) }
	currentReadFn    = func(f *os.File, p []byte) (int, error) { return f.Read(p) }
	currentLstatFn   = os.Lstat
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

// ParseCurrentManifest strictly parses and validates the in-memory raw bytes of a CURRENT pointer.
//
// Validation & Parsing Rules:
//  1. Size Bounds: data length must be between MinCurrentFileSize (16) and MaxCurrentFileSize (30) bytes.
//  2. Trailing Newline: data must terminate with exactly one '\n' (0x0A) byte.
//  3. Canonical Prefix: data must begin with "MANIFEST-".
//  4. Sequence Digits: digits between prefix and '\n' must be between 6 and 20 ASCII decimal chars ('0'..'9').
//  5. No Overflow: decimal number must not overflow uint64 (math.MaxUint64 = 18446744073709551615).
//  6. Non-Zero Sequence: manifest numbers start at 1; 0 is strictly rejected.
//  7. Canonical Form: the byte sequence must match ManifestFilename(num) + "\n" byte-for-byte,
//     preventing superfluous leading zeros, whitespace, or non-canonical variations.
func ParseCurrentManifest(data []byte) (uint64, error) {
	if len(data) < MinCurrentFileSize || len(data) > MaxCurrentFileSize {
		return 0, fmt.Errorf("%w: byte length %d out of canonical range [%d, %d]",
			errors.ErrCurrentCorrupted, len(data), MinCurrentFileSize, MaxCurrentFileSize)
	}
	if data[len(data)-1] != '\n' {
		return 0, fmt.Errorf("%w: missing trailing newline", errors.ErrCurrentCorrupted)
	}
	if !bytes.HasPrefix(data, []byte(ManifestFilenamePrefix)) {
		return 0, fmt.Errorf("%w: missing %q prefix", errors.ErrCurrentCorrupted, ManifestFilenamePrefix)
	}

	digits := data[len(ManifestFilenamePrefix) : len(data)-1]
	if len(digits) < 6 || len(digits) > 20 {
		return 0, fmt.Errorf("%w: digit length %d out of canonical range [6, 20]",
			errors.ErrCurrentCorrupted, len(digits))
	}

	var num uint64
	for i := 0; i < len(digits); i++ {
		b := digits[i]
		if b < '0' || b > '9' {
			return 0, fmt.Errorf("%w: non-digit byte 0x%02x at offset %d",
				errors.ErrCurrentCorrupted, b, len(ManifestFilenamePrefix)+i)
		}
		d := uint64(b - '0')
		if num > (math.MaxUint64-d)/10 {
			return 0, fmt.Errorf("%w: manifest sequence number overflows uint64", errors.ErrCurrentCorrupted)
		}
		num = num*10 + d
	}

	if num == 0 {
		return 0, fmt.Errorf("%w: %w", errors.ErrCurrentCorrupted, errors.ErrInvalidManifestNum)
	}

	// Verify exact canonical serialization (eliminates superfluous leading zeros like MANIFEST-0000001\n)
	expected := ManifestFilename(num) + "\n"
	if !bytes.Equal(data, []byte(expected)) {
		return 0, fmt.Errorf("%w: non-canonical representation (got %q, expected %q)",
			errors.ErrCurrentCorrupted, data, expected)
	}

	return num, nil
}

// currentNotFoundError wraps errors when the CURRENT pointer file does not exist.
// It matches both errors.ErrCurrentNotFound and os.ErrNotExist with errors.Is().
type currentNotFoundError struct {
	path       string
	underlying error
}

func (e *currentNotFoundError) Error() string {
	return fmt.Sprintf("%s: %s: %v", errors.ErrCurrentNotFound, e.path, e.underlying)
}

func (e *currentNotFoundError) Is(target error) bool {
	return target == errors.ErrCurrentNotFound || target == os.ErrNotExist
}

func (e *currentNotFoundError) Unwrap() error {
	return e.underlying
}

// ReadCurrentManifest safely locates <dir>/CURRENT, verifies filesystem object invariants,
// reads its contents under bounded allocation limits, and strictly parses the active manifest number.
//
// Safety & Security Invariants:
//  1. Path Sanitization: cleans dir and joins with CurrentFilename ("CURRENT").
//  2. Directory Verification: verifies dir exists, is a genuine directory, and is not a symlink.
//  3. Filesystem Object-Type Security: rejects symbolic links, directories, and non-regular files.
//  4. Size Ceiling Check: verifies on-disk file size does not exceed MaxCurrentFileSize before opening.
//  5. TOCTOU Defense: confirms via os.SameFile that the opened descriptor matches the inspected inode.
//     Under concurrent atomic updates (os.Rename), re-verifies post-open disk state with bounded retry.
//  6. Bounded Stack Buffer: reads up to MaxCurrentFileSize+1 bytes into a fixed stack buffer without heap allocations.
//  7. Missing vs Corrupted: returns ErrCurrentNotFound if CURRENT is absent; returns ErrCurrentCorrupted on malformed data.
func ReadCurrentManifest(dir string) (uint64, error) {
	if dir == "" {
		return 0, fmt.Errorf("%w: directory path cannot be empty", os.ErrInvalid)
	}

	cleanDir := filepath.Clean(dir)

	// 1. Verify parent directory exists, is a genuine directory, and is not a symlink
	dirInfo, err := currentLstatFn(cleanDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, &currentNotFoundError{path: cleanDir, underlying: err}
		}
		return 0, fmt.Errorf("current: failed to inspect directory %s: %w", cleanDir, err)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 {
		return 0, fmt.Errorf("%w: directory %s cannot be a symbolic link", os.ErrInvalid, cleanDir)
	}
	if !dirInfo.IsDir() {
		return 0, &errors.NotADirectoryError{
			Path: cleanDir,
			Mode: dirInfo.Mode(),
		}
	}

	currentPath := filepath.Join(cleanDir, CurrentFilename)

	var f *os.File
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		// 2. Pre-open inspection of CURRENT
		cInfo, err := currentLstatFn(currentPath)
		if err != nil {
			if os.IsNotExist(err) {
				return 0, &currentNotFoundError{path: currentPath, underlying: err}
			}
			return 0, fmt.Errorf("current: failed to inspect %s: %w", currentPath, err)
		}
		if cInfo.Mode()&os.ModeSymlink != 0 {
			return 0, fmt.Errorf("%w: target %s is a symlink", errors.ErrCurrentSymlink, currentPath)
		}
		if cInfo.IsDir() {
			return 0, &errors.NotADirectoryError{
				Path: currentPath,
				Mode: cInfo.Mode(),
			}
		}
		if !cInfo.Mode().IsRegular() {
			return 0, fmt.Errorf("%w: target %s is not a regular file (mode=%v)", errors.ErrCurrentCorrupted, currentPath, cInfo.Mode())
		}
		if cInfo.Size() < MinCurrentFileSize || cInfo.Size() > MaxCurrentFileSize {
			return 0, fmt.Errorf("%w: file size %d bytes out of canonical range [%d, %d]",
				errors.ErrCurrentCorrupted, cInfo.Size(), MinCurrentFileSize, MaxCurrentFileSize)
		}

		// 3. Open file descriptor
		openFile, err := currentOpenFn(currentPath)
		if err != nil {
			if os.IsNotExist(err) {
				return 0, &currentNotFoundError{path: currentPath, underlying: err}
			}
			return 0, fmt.Errorf("current: failed to open %s: %w", currentPath, err)
		}

		// 4. Post-open TOCTOU verification: verify opened descriptor references the exact inspected inode
		finfo, err := openFile.Stat()
		if err != nil {
			_ = currentCloseFn(openFile)
			return 0, fmt.Errorf("current: failed to stat open file %s: %w", currentPath, err)
		}
		if !finfo.Mode().IsRegular() {
			_ = currentCloseFn(openFile)
			return 0, fmt.Errorf("%w: target %s is not a regular file (mode=%v)", errors.ErrCurrentCorrupted, currentPath, finfo.Mode())
		}

		if os.SameFile(cInfo, finfo) {
			f = openFile
			break
		}

		// Concurrent atomic rename occurred between Lstat and Open.
		// Verify that the opened file matches current on-disk state and is NOT a symlink.
		postInfo, lstatErr := currentLstatFn(currentPath)
		if lstatErr == nil {
			if postInfo.Mode()&os.ModeSymlink != 0 {
				_ = currentCloseFn(openFile)
				return 0, fmt.Errorf("%w: target %s is a symlink", errors.ErrCurrentSymlink, currentPath)
			}
			if os.SameFile(postInfo, finfo) {
				f = openFile
				break
			}
		}

		// If still in flux under high concurrent contention, close and retry
		_ = currentCloseFn(openFile)
		if attempt == maxRetries-1 {
			return 0, fmt.Errorf("%w: file %s was replaced during open (TOCTOU race detected)", os.ErrInvalid, currentPath)
		}
	}

	// 5. Bounded read into fixed stack buffer (zero heap allocations)
	var buf [MaxCurrentFileSize + 1]byte
	var totalRead int
	for totalRead < len(buf) {
		n, readErr := currentReadFn(f, buf[totalRead:])
		if n > 0 {
			totalRead += n
		}
		if readErr != nil {
			if stdErrors.Is(readErr, io.EOF) {
				break
			}
			_ = currentCloseFn(f)
			return 0, fmt.Errorf("current: read from %s failed: %w", currentPath, readErr)
		}
		if n == 0 {
			break
		}
	}

	// 6. Close file descriptor
	if closeErr := currentCloseFn(f); closeErr != nil {
		return 0, fmt.Errorf("current: close of %s failed: %w", currentPath, closeErr)
	}

	// Fail closed if content exceeded maximum canonical size
	if totalRead > MaxCurrentFileSize {
		return 0, fmt.Errorf("%w: file %s content length %d exceeds maximum canonical size %d",
			errors.ErrCurrentCorrupted, currentPath, totalRead, MaxCurrentFileSize)
	}

	// 7. Parse and validate canonical byte representation
	manifestNum, err := ParseCurrentManifest(buf[:totalRead])
	if err != nil {
		return 0, fmt.Errorf("current: failed to parse %s: %w", currentPath, err)
	}

	return manifestNum, nil
}
