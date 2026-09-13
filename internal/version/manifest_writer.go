package version

import (
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

const (
	// ManifestHeaderSize is the fixed size in bytes of the MANIFEST binary record framing header.
	// Binary Framing Layout:
	//   Offset 0..3: CRC32-IEEE (4 bytes, uint32, Big-Endian)
	//   Offset 4..7: PayloadLength (4 bytes, uint32, Big-Endian)
	// Followed by:
	//   Offset 8..8+PayloadLength-1: VersionEdit serialized binary payload
	ManifestHeaderSize = 8

	// ManifestFilenamePrefix defines the standard filename prefix for manifest log files.
	ManifestFilenamePrefix = "MANIFEST-"

	// ManifestFilenamePattern defines the standard zero-padded 6-digit manifest filename format.
	// Example: MANIFEST-000001
	ManifestFilenamePattern = "MANIFEST-%06d"

	// ManifestFileMode defines the restrictive POSIX file permissions (0600) for MANIFEST log files:
	// owner: read + write (rw-); group: none (---); others: none (---).
	// This ensures unprivileged local processes cannot read or tamper with database metadata.
	ManifestFileMode os.FileMode = 0600

	// MinManifestRecordSize defines the minimum serialized size in bytes of a valid MANIFEST record:
	// 8-byte framing header + 1-byte minimal VersionEdit format byte (empty edit: 0x01) = 9 bytes.
	MinManifestRecordSize = ManifestHeaderSize + 1

	// MaxManifestRecordSize defines the maximum permissible size in bytes of a MANIFEST record:
	// 8-byte framing header + 16 MiB MaxVersionEditBytes = 16,777,224 bytes.
	MaxManifestRecordSize = ManifestHeaderSize + MaxVersionEditBytes
)

// ManifestFilename returns the canonical MANIFEST filename for a given sequential manifest number.
// In accordance with Section 18.2 of the architecture specification, the filename is formatted
// as MANIFEST-000001 with 6 zero-padded digits.
func ManifestFilename(manifestNum uint64) string {
	return fmt.Sprintf(ManifestFilenamePattern, manifestNum)
}

// ManifestPath returns the platform-aware path to a MANIFEST file within the designated dbPath:
//
//	<db_path>/MANIFEST-<000001>
func ManifestPath(dbPath string, manifestNum uint64) string {
	return filepath.Join(dbPath, ManifestFilename(manifestNum))
}

// ManifestWriter sequentially appends CRC32-framed VersionEdit records to an active
// MANIFEST file with immediate, synchronous hardware durability via fdatasync().
//
// Invariants & Operational Semantics:
//  1. Strict Hardware Durability Contract:
//     LogEdit(edit) returns nil IF AND ONLY IF:
//     a. The complete 8-byte framing header and serialized VersionEdit payload have been transferred to disk.
//     b. The data synchronization barrier (fdatasync on Linux, f.Sync fallback elsewhere) has succeeded.
//     Records accepted by the operating system page cache are never reported as committed until
//     physical disk synchronization finishes successfully.
//  2. Sequential Append-Only Guarantee:
//     Files are opened strictly with os.O_WRONLY | os.O_CREATE | os.O_APPEND.
//     Every write operation targets the end-of-file. Prior records are never overwritten,
//     truncated, or seeked backwards.
//  3. CRC32 Integrity Framing:
//     Every record is framed with an 8-byte header:
//     [ CRC32 (4B, Big-Endian) | PayloadLength (4B, Big-Endian) ] [ VersionEdit Payload (N B) ]
//     CRC32-IEEE is computed over the 4-byte PayloadLength field and the N-byte payload (record[4:]),
//     guarding against both payload bit-rot and header length corruption.
//  4. Restart & Non-Destructive Reopen:
//     Reopening an existing MANIFEST file preserves all previously written records without truncation.
//  5. Inode Pinning & Symlink Defense:
//     Validates via os.Lstat and os.SameFile that the target path is a genuine regular file
//     and not a substituted symlink or directory.
//  6. Fail-Closed Poison State Machine:
//     If a disk write fails, produces a short write, or fdatasync fails, the writer enters
//     an unrecoverable poisoned state and all subsequent operations immediately fail-closed.
//  7. Concurrency Safety:
//     An internal sync.Mutex serializes calls, guaranteeing sequential ordering and atomic
//     record boundaries without interleaving.
//  8. Resource Ownership:
//     Owns the underlying *os.File descriptor. Close() flushes, synchronizes, and cleanly
//     closes the descriptor.
type ManifestWriter struct {
	mu          sync.Mutex
	file        *os.File
	path        string
	offset      int64
	recordCount uint64
	closed      bool
	poisoned    bool
	poisonErr   error

	// Internal test seams for deterministic fault injection
	syncFn  func(f *os.File) error
	writeFn func(f *os.File, p []byte) (int, error)
	closeFn func(f *os.File) error
}

// OpenManifestWriter opens or creates a MANIFEST log file at the specified filesystem path
// for sequential, synchronous appending with 0600 permissions.
//
// Invariants & Security Guarantees:
//   - If the file already exists, existing contents are preserved intact without truncation.
//   - Existing file size is inspected to initialize the record offset.
//   - If the target path is a directory or symlink, opening is rejected with an error.
//   - File descriptor is pinned to the disk inode via os.SameFile to prevent TOCTOU substitution.
func OpenManifestWriter(path string) (*ManifestWriter, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: path cannot be empty", os.ErrInvalid)
	}

	cleanPath := filepath.Clean(path)

	// Pre-open inspection: reject symlinks and directories
	if info, err := os.Lstat(cleanPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("manifest: cannot open symlink %s: %w", cleanPath, os.ErrInvalid)
		}
		if info.IsDir() {
			return nil, &errors.NotADirectoryError{
				Path: cleanPath,
				Mode: info.Mode(),
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("manifest: failed to inspect path %s: %w", cleanPath, err)
	}

	flags := os.O_WRONLY | os.O_CREATE | os.O_APPEND
	f, err := os.OpenFile(cleanPath, flags, ManifestFileMode)
	if err != nil {
		return nil, fmt.Errorf("manifest: failed to open file %s: %w", cleanPath, err)
	}

	// Verify the opened file descriptor references a genuine regular file
	finfo, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		return nil, fmt.Errorf("manifest: failed to stat opened file %s: %w", cleanPath, statErr)
	}
	if !finfo.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("manifest: path %s is not a regular file (mode: %s): %w", cleanPath, finfo.Mode(), os.ErrInvalid)
	}

	// Post-open verification: prove the open file descriptor matches the inode on disk
	postInfo, lstatErr := os.Lstat(cleanPath)
	if lstatErr != nil {
		_ = f.Close()
		return nil, fmt.Errorf("manifest: failed to lstat file %s: %w", cleanPath, lstatErr)
	}
	if !os.SameFile(finfo, postInfo) {
		_ = f.Close()
		return nil, fmt.Errorf("manifest: file %s was replaced during open: %w", cleanPath, os.ErrInvalid)
	}

	w := &ManifestWriter{
		file:    f,
		path:    cleanPath,
		offset:  finfo.Size(),
		syncFn:  fdatasync,
		writeFn: func(file *os.File, p []byte) (int, error) { return file.Write(p) },
		closeFn: func(file *os.File) error { return file.Close() },
	}

	return w, nil
}

// CreateManifestWriter creates a brand-new MANIFEST file at path with exclusive creation
// semantics (os.O_EXCL | os.O_CREATE) and 0600 permissions.
//
// Invariants & Security Guarantees:
//   - If a file, directory, or symlink already exists at the target path, creation
//     aborts immediately with an error wrapping errors.ErrManifestExists and os.ErrExist.
//   - Existing file contents are NEVER overwritten or truncated.
//   - Pins the opened file descriptor to the disk inode via os.SameFile.
func CreateManifestWriter(path string) (*ManifestWriter, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: path cannot be empty", os.ErrInvalid)
	}

	cleanPath := filepath.Clean(path)

	// Pre-creation inspection: reject existing paths immediately before open attempt
	if info, err := os.Lstat(cleanPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("manifest: cannot create manifest over existing symlink %s: %w", cleanPath, os.ErrInvalid)
		}
		if info.IsDir() {
			return nil, &errors.NotADirectoryError{
				Path: cleanPath,
				Mode: info.Mode(),
			}
		}
		return nil, fmt.Errorf("%w: %w: manifest file %s already exists", errors.ErrManifestExists, os.ErrExist, cleanPath)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("manifest: failed to inspect path %s: %w", cleanPath, err)
	}

	// Atomic exclusive creation: kernel guarantees fail-fast if file exists concurrently
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL | os.O_APPEND
	f, err := os.OpenFile(cleanPath, flags, ManifestFileMode)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("%w: %w: failed to create manifest file %s", errors.ErrManifestExists, err, cleanPath)
		}
		return nil, fmt.Errorf("manifest: failed to create manifest file %s: %w", cleanPath, err)
	}

	// Verify the opened file descriptor references a genuine regular file
	finfo, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		return nil, fmt.Errorf("manifest: failed to stat created file %s: %w", cleanPath, statErr)
	}
	if !finfo.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("manifest: path %s is not a regular file (mode: %s): %w", cleanPath, finfo.Mode(), os.ErrInvalid)
	}

	// Post-create verification: prove the open file descriptor matches the inode on disk
	postInfo, lstatErr := os.Lstat(cleanPath)
	if lstatErr != nil {
		_ = f.Close()
		return nil, fmt.Errorf("manifest: failed to lstat created file %s: %w", cleanPath, lstatErr)
	}
	if !os.SameFile(finfo, postInfo) {
		_ = f.Close()
		return nil, fmt.Errorf("manifest: file %s was replaced during create: %w", cleanPath, os.ErrInvalid)
	}

	w := &ManifestWriter{
		file:    f,
		path:    cleanPath,
		offset:  0,
		syncFn:  fdatasync,
		writeFn: func(file *os.File, p []byte) (int, error) { return file.Write(p) },
		closeFn: func(file *os.File) error { return file.Close() },
	}

	return w, nil
}

// NewManifestWriter wraps an already-opened *os.File descriptor as a ManifestWriter.
// The caller retains initial file creation responsibility, while the returned ManifestWriter
// assumes ownership of descriptor lifecycle and durability synchronization.
func NewManifestWriter(file *os.File) (*ManifestWriter, error) {
	if file == nil {
		return nil, fmt.Errorf("%w: file descriptor cannot be nil", os.ErrInvalid)
	}

	finfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("manifest: failed to stat file descriptor: %w", err)
	}
	if !finfo.Mode().IsRegular() {
		return nil, fmt.Errorf("manifest: file is not a regular file (mode: %s): %w", finfo.Mode(), os.ErrInvalid)
	}

	return &ManifestWriter{
		file:    file,
		path:    file.Name(),
		offset:  finfo.Size(),
		syncFn:  fdatasync,
		writeFn: func(f *os.File, p []byte) (int, error) { return f.Write(p) },
		closeFn: func(f *os.File) error { return f.Close() },
	}, nil
}

// Path returns the canonical filesystem path of the active MANIFEST file.
func (w *ManifestWriter) Path() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.path
}

// Offset returns the current logical write offset (file size) of the MANIFEST file.
func (w *ManifestWriter) Offset() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.offset
}

// RecordCount returns the total number of VersionEdit records successfully logged and synced.
func (w *ManifestWriter) RecordCount() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.recordCount
}

// IsClosed reports whether the writer has been closed.
func (w *ManifestWriter) IsClosed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

// IsPoisoned reports whether the writer has entered an unrecoverable error state due to a write or sync failure.
func (w *ManifestWriter) IsPoisoned() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.poisoned
}

// poisonLocked transitions the writer into an unrecoverable poisoned state,
// preserving the first root-cause I/O or sync error.
// Must be called with w.mu held.
func (w *ManifestWriter) poisonLocked(err error) {
	if !w.poisoned {
		w.poisoned = true
		w.poisonErr = err
	}
}

// checkPoisonLocked returns a structured ManifestWriterPoisonedError if the writer is poisoned.
// Must be called with w.mu held.
func (w *ManifestWriter) checkPoisonLocked() error {
	if w.poisoned {
		return &errors.ManifestWriterPoisonedError{
			Path:   w.path,
			Reason: w.poisonErr,
		}
	}
	return nil
}

// LogEdit serializes edit, frames it with CRC32 integrity framing, appends all bytes
// to the MANIFEST file, and executes the fdatasync durability barrier before returning.
//
// Return Contract:
//   - Returns nil IF AND ONLY IF all record bytes were written AND fdatasync succeeded.
//   - If the writer is closed, returns errors.ErrManifestWriterClosed.
//   - If the writer is poisoned, returns errors.ErrManifestWriterPoisoned.
//   - If write fails or produces a short write, poisons the writer and returns the write error.
//   - If fdatasync fails, poisons the writer and returns the synchronization error.
//   - Thread-safe: concurrent invocations are serialized by an internal mutex to ensure
//     monotonic append ordering and atomic record boundaries.
func (w *ManifestWriter) LogEdit(edit VersionEdit) error {
	return w.logEditInternal(&edit)
}

// LogEditPtr appends edit to the MANIFEST file with durability synchronization,
// accepting a pointer to avoid copying the VersionEdit struct.
func (w *ManifestWriter) LogEditPtr(edit *VersionEdit) error {
	if edit == nil {
		return fmt.Errorf("%w: version edit cannot be nil", os.ErrInvalid)
	}
	return w.logEditInternal(edit)
}

func (w *ManifestWriter) logEditInternal(edit *VersionEdit) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return errors.ErrManifestWriterClosed
	}
	if err := w.checkPoisonLocked(); err != nil {
		return err
	}

	// 1. Serialize VersionEdit payload via canonical codec (bounded by MaxVersionEditBytes = 16 MiB)
	payload := edit.Encode()
	payloadLen := len(payload)
	if payloadLen > MaxVersionEditBytes {
		return fmt.Errorf("manifest: version edit payload size %d exceeds maximum limit %d", payloadLen, MaxVersionEditBytes)
	}

	// 2. Protect offset arithmetic against 64-bit integer overflow
	recordLen := int64(ManifestHeaderSize + payloadLen)
	if w.offset > math.MaxInt64-recordLen {
		err := fmt.Errorf("manifest: file offset overflow: offset %d + record size %d exceeds int64 limit", w.offset, recordLen)
		w.poisonLocked(err)
		return err
	}

	// 3. Assemble binary framing:
	//    Offset 0..3: CRC32-IEEE (4B, Big-Endian)
	//    Offset 4..7: PayloadLength (4B, Big-Endian)
	//    Offset 8..8+N-1: Payload (N bytes)
	record := make([]byte, recordLen)
	binary.PutUint32(record[4:8], uint32(payloadLen))
	copy(record[ManifestHeaderSize:], payload)

	// Compute CRC32-IEEE over PayloadLength (4B) + Payload (N B)
	crc := crc32.ChecksumIEEE(record[4:])
	binary.PutUint32(record[0:4], crc)

	// 4. Sequential write loop: ensure all record bytes are fully committed
	var written int
	for written < len(record) {
		n, writeErr := w.writeFn(w.file, record[written:])
		if n > 0 {
			written += n
		}
		if writeErr != nil {
			err := fmt.Errorf("manifest: write failed at offset %d after %d/%d bytes: %w",
				w.offset+int64(written), written, len(record), writeErr)
			w.poisonLocked(err)
			return err
		}
		if n == 0 {
			err := fmt.Errorf("manifest: short write with 0 bytes at offset %d after %d/%d bytes: %w",
				w.offset+int64(written), written, len(record), io.ErrShortWrite)
			w.poisonLocked(err)
			return err
		}
	}

	// 5. Durability barrier: flush modified in-core data blocks to non-volatile storage
	if syncErr := w.syncFn(w.file); syncErr != nil {
		err := fmt.Errorf("manifest: sync failed at offset %d: %w", w.offset+recordLen, syncErr)
		w.poisonLocked(err)
		return err
	}

	// 6. Advance logical offset and record counter upon proven durability
	w.offset += recordLen
	w.recordCount++

	return nil
}

// Sync executes the fdatasync durability barrier on the active MANIFEST file,
// ensuring that all previously written bytes are persisted to stable physical media.
func (w *ManifestWriter) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return errors.ErrManifestWriterClosed
	}
	if err := w.checkPoisonLocked(); err != nil {
		return err
	}

	if syncErr := w.syncFn(w.file); syncErr != nil {
		err := fmt.Errorf("manifest: sync failed: %w", syncErr)
		w.poisonLocked(err)
		return err
	}
	return nil
}

// Close flushes data, executes the durability barrier, and closes the underlying file descriptor.
// Subsequent operations on the writer return errors.ErrManifestWriterClosed.
// Close is idempotent: repeated calls return nil.
func (w *ManifestWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return nil
	}
	w.closed = true

	// If writer was poisoned, skip durability sync to avoid cascading failures,
	// ensure descriptor is closed cleanly to prevent resource leak,
	// and propagate the poisoning error.
	if w.poisoned {
		if w.file != nil {
			_ = w.closeFn(w.file)
		}
		return &errors.ManifestWriterPoisonedError{
			Path:   w.path,
			Reason: w.poisonErr,
		}
	}

	// Best-effort flush and sync before closing descriptor
	syncErr := w.syncFn(w.file)
	closeErr := w.closeFn(w.file)

	if syncErr != nil {
		return fmt.Errorf("manifest: sync during close %s: %w", w.path, syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("manifest: close %s: %w", w.path, closeErr)
	}

	return nil
}
