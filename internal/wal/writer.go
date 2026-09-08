package wal

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/silent-knight19/lattice/internal/errors"
)

// FileMode defines the restrictive POSIX file permission mode (0600) for WAL segment files:
// owner: read + write (rw-); group: none (---); others: none (---).
// This ensures that unprivileged local users cannot inspect or tamper with sensitive WAL log bytes.
const FileMode os.FileMode = 0600

// SegmentName returns the canonical WAL segment filename for a given segment ID.
// Per Section 18.2 of docs/architecture-spec.md, names use the format:
//
//	wal_<000000000001>.log
//
// with zero-padded 12-digit sequential IDs (e.g. wal_000000000001.log).
func SegmentName(id uint64) string {
	return fmt.Sprintf("wal_%012d.log", id)
}

// SegmentPath returns the platform-aware path to a WAL segment within dbPath:
//
//	<db_path>/wal/wal_<000000000001>.log
func SegmentPath(dbPath string, id uint64) string {
	return filepath.Join(Dir(dbPath), SegmentName(id))
}

// WALWriter sequentially appends Write-Ahead Log records to an active segment file
// with immediate, synchronous hardware durability ("Strict Sync").
//
// Invariants & Operational Semantics:
//  1. Strict Sync Contract:
//     AppendSync(rec) returns nil IF AND ONLY IF:
//     a. The complete serialized record bytes have been written to the underlying file.
//     b. The data synchronization barrier (fdatasync) has successfully completed.
//     Bytes accepted by the OS page cache are never reported as committed until fdatasync succeeds.
//  2. Sequential Append Ordering:
//     File opened with os.O_WRONLY | os.O_CREATE | os.O_APPEND. Every write automatically
//     targets EOF without overwriting or truncating prior records.
//  3. Restart & Non-Destructive Reopen:
//     Reopening an existing segment file never erases, truncates, or resets existing records.
//  4. Inode Pinning & Symlink Defense:
//     Verifies via os.Lstat and os.SameFile that the target path is a genuine regular file
//     and not a substituted symlink or directory.
//  5. Concurrency Model:
//     Thread-safe per writer. An internal sync.Mutex serializes concurrent AppendSync calls,
//     guaranteeing that records from concurrent goroutines are never interleaved.
//  6. Partial Write Handling:
//     Loops until all record bytes are consumed. If a write fails mid-record, AppendSync returns
//     an error without claiming success. In accordance with LSM architecture, torn tail cleanup
//     is deferred to the recovery subsystem at startup.
//  7. Resource Management:
//     Owns the underlying *os.File descriptor. Close() flushes uncommitted buffers, invokes the
//     durability barrier, and closes the descriptor cleanly.
type WALWriter struct {
	mu     sync.Mutex
	file   *os.File
	path   string
	closed bool

	// Internal test seams for deterministic fault injection
	syncFn  func(f *os.File) error
	writeFn func(f *os.File, p []byte) (int, error)
}

// OpenWriter opens or creates a WAL segment file at the specified filesystem path
// for sequential, synchronous appending with 0600 permissions.
//
// If the file already exists, existing contents are preserved intact (no truncation).
// If the target path is a directory or symlink, opening is rejected with an error.
func OpenWriter(path string) (*WALWriter, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: path cannot be empty", os.ErrInvalid)
	}

	cleanPath := filepath.Clean(path)

	// Pre-open inspection: reject symlinks and directories
	if info, err := os.Lstat(cleanPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("wal: cannot open symlink %s: %w", cleanPath, os.ErrInvalid)
		}
		if info.IsDir() {
			return nil, &errors.NotADirectoryError{
				Path: cleanPath,
				Mode: info.Mode(),
			}
		}
	}

	flags := os.O_WRONLY | os.O_CREATE | os.O_APPEND
	f, err := os.OpenFile(cleanPath, flags, FileMode)
	if err != nil {
		return nil, fmt.Errorf("wal: failed to open file %s: %w", cleanPath, err)
	}

	// Verify the opened file descriptor references a genuine regular file
	finfo, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		return nil, fmt.Errorf("wal: failed to stat opened file %s: %w", cleanPath, statErr)
	}
	if !finfo.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("wal: path %s is not a regular file (mode: %s): %w", cleanPath, finfo.Mode(), os.ErrInvalid)
	}

	// Post-open verification: prove the open file descriptor matches the inode on disk
	postInfo, lstatErr := os.Lstat(cleanPath)
	if lstatErr != nil {
		_ = f.Close()
		return nil, fmt.Errorf("wal: failed to lstat file %s: %w", cleanPath, lstatErr)
	}
	if !os.SameFile(finfo, postInfo) {
		_ = f.Close()
		return nil, fmt.Errorf("wal: file %s was replaced during open: %w", cleanPath, os.ErrInvalid)
	}

	w := &WALWriter{
		file:    f,
		path:    cleanPath,
		syncFn:  fdatasync,
		writeFn: func(file *os.File, p []byte) (int, error) { return file.Write(p) },
	}

	return w, nil
}

// OpenSegmentWriter opens or creates a WAL segment file under dbPath using its 12-digit segment ID.
// The segment path is constructed as <db_path>/wal/wal_<000000000001>.log.
func OpenSegmentWriter(dbPath string, id uint64) (*WALWriter, error) {
	return OpenWriter(SegmentPath(dbPath, id))
}

// Path returns the canonical filesystem path of the active WAL segment file.
func (w *WALWriter) Path() string {
	return w.path
}

// Close flushes data, executes the durability barrier, and closes the underlying file descriptor.
// Subsequent operations on the writer will return errors.ErrWriterClosed.
// Close is idempotent; subsequent calls return nil.
func (w *WALWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return nil
	}
	w.closed = true

	// Best-effort flush and sync before closing descriptor
	syncErr := w.syncFn(w.file)
	closeErr := w.file.Close()

	if syncErr != nil {
		return fmt.Errorf("wal: sync during close %s: %w", w.path, syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("wal: close %s: %w", w.path, closeErr)
	}

	return nil
}

// AppendSync encodes record into physical wire format, writes all bytes to disk,
// and executes an fdatasync durability barrier before returning.
//
// Return Contract:
//   - Returns nil if and only if all bytes were written AND fdatasync succeeded.
//   - If the writer is closed, returns errors.ErrWriterClosed.
//   - If rec.Validate() fails, returns the validation error without modifying the file.
//   - If write fails or produces a short write, returns an error.
//   - If fdatasync fails, returns the synchronization error.
//   - Caller's rec.Key and rec.Value slices are never mutated.
//   - Concurrent invocations are serialized by an internal mutex to prevent record interleaving.
func (w *WALWriter) AppendSync(rec Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return errors.ErrWriterClosed
	}

	// Validate logical record invariants before touching disk buffers
	if err := rec.Validate(); err != nil {
		return err
	}

	// Serialize record into wire format (single heap allocation, non-mutating)
	buf, err := EncodeRecord(rec)
	if err != nil {
		return fmt.Errorf("wal: encode record failed: %w", err)
	}

	// Write loop: ensure all record bytes are transferred
	var written int
	for written < len(buf) {
		n, writeErr := w.writeFn(w.file, buf[written:])
		if n > 0 {
			written += n
		}
		if writeErr != nil {
			return fmt.Errorf("wal: write failed after %d/%d bytes: %w", written, len(buf), writeErr)
		}
		if n == 0 {
			return fmt.Errorf("wal: short write with 0 bytes after %d/%d bytes: %w", written, len(buf), io.ErrShortWrite)
		}
	}

	// Durability barrier: flush data to non-volatile storage
	if syncErr := w.syncFn(w.file); syncErr != nil {
		return fmt.Errorf("wal: sync failed: %w", syncErr)
	}

	return nil
}

// setSyncFnForTesting injects a custom synchronization function for fault injection tests.
func (w *WALWriter) setSyncFnForTesting(fn func(f *os.File) error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.syncFn = fn
}

// setWriteFnForTesting injects a custom write function for fault injection tests.
func (w *WALWriter) setWriteFnForTesting(fn func(f *os.File, p []byte) (int, error)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeFn = fn
}
