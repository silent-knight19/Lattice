package wal

import (
	stdErrors "errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/silent-knight19/lattice/internal/errors"
)

// WALReader sequentially streams and decodes Write-Ahead Log records from a segment file
// from byte offset 0 forward to clean end-of-file.
//
// Architectural Responsibilities & Scope Boundary:
//  1. Read-Only Classification:
//     WALReader is strictly a read-only stream iterator. It reads and classifies records,
//     surfacing clean EOF, propagating checksum corruption, or identifying incomplete/torn
//     records. It NEVER truncates files, modifies bytes, or attempts in-line repairs.
//  2. Sequential Offset Model:
//     Maintains the exact logical byte offset in the WAL segment. Positioned at 0 at start.
//     Advanced by the physical wire size of each record if and only if Next() successfully
//     consumes that record. If Next() encounters EOF, truncation, or corruption, Offset()
//     remains fixed at the start of that unconsumed record.
//  3. Clean EOF vs Torn Tail:
//     Returns io.EOF if and only if the physical end of file aligns exactly with a record boundary.
//     If the file terminates mid-record (in header, key, or value bytes), it returns the authoritative
//     truncation error (e.g. errors.ErrHeaderTruncated, io.ErrUnexpectedEOF).
//  4. Non-Destructive Error Propagation:
//     On middle-log checksum mismatches (errors.ErrChecksumMismatch) or structural invalidity,
//     WALReader stops deterministically and returns the error. It never silently skips corruptions.
//  5. Concurrency Model:
//     WALReader is a single-consumer forward iterator and is NOT safe for concurrent calls
//     to Next() from multiple goroutines.
type WALReader struct {
	file   *os.File
	path   string
	offset int64
	closed bool
}

// OpenReader opens an existing WAL segment file for sequential, read-only iteration.
//
// Invariants & Pre-Conditions:
//   - Fails fast with os.ErrInvalid if path is empty.
//   - Preserves fs.ErrNotExist if the target WAL file does not exist.
//   - Rejects symbolic links at the target path to prevent symlink redirection hazards.
//   - Rejects directories with *errors.NotADirectoryError.
//   - Rejects non-regular filesystem objects (devices, sockets, named pipes).
//   - Uses read-only mode (os.O_RDONLY); never modifies or creates files.
//   - Pins the opened descriptor's inode against os.Lstat to prevent file-swap TOCTOU attacks.
func OpenReader(path string) (*WALReader, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: path cannot be empty", os.ErrInvalid)
	}

	cleanPath := filepath.Clean(path)

	// Pre-open inspection: verify target exists and is a regular file, rejecting symlinks and directories
	info, err := os.Lstat(cleanPath)
	if err != nil {
		return nil, fmt.Errorf("wal: failed to inspect reader path %s: %w", cleanPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("wal: cannot open symlink %s: %w", cleanPath, os.ErrInvalid)
	}
	if info.IsDir() {
		return nil, &errors.NotADirectoryError{
			Path: cleanPath,
			Mode: info.Mode(),
		}
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("wal: path %s is not a regular file (mode: %s): %w", cleanPath, info.Mode(), os.ErrInvalid)
	}

	f, err := os.OpenFile(cleanPath, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("wal: failed to open reader file %s: %w", cleanPath, err)
	}

	// Post-open verification: prove the open descriptor matches the inode on disk
	finfo, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		return nil, fmt.Errorf("wal: failed to stat opened reader file %s: %w", cleanPath, statErr)
	}
	if !finfo.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("wal: path %s is not a regular file (mode: %s): %w", cleanPath, finfo.Mode(), os.ErrInvalid)
	}

	postInfo, lstatErr := os.Lstat(cleanPath)
	if lstatErr != nil {
		_ = f.Close()
		return nil, fmt.Errorf("wal: failed to lstat reader file %s: %w", cleanPath, lstatErr)
	}
	if !os.SameFile(finfo, postInfo) {
		_ = f.Close()
		return nil, fmt.Errorf("wal: reader file %s was replaced during open: %w", cleanPath, os.ErrInvalid)
	}

	return &WALReader{
		file:   f,
		path:   cleanPath,
		offset: 0,
		closed: false,
	}, nil
}

// OpenSegmentReader opens an existing WAL segment file under dbPath using its 12-digit segment ID.
// The segment path is constructed as <db_path>/wal/wal_<000000000001>.log.
func OpenSegmentReader(dbPath string, id uint64) (*WALReader, error) {
	return OpenReader(SegmentPath(dbPath, id))
}

// Path returns the canonical filesystem path of the WAL segment being read.
func (r *WALReader) Path() string {
	return r.path
}

// Offset returns the current logical byte offset in the WAL segment file.
//
// Semantics:
//   - At creation, Offset() is 0.
//   - After each successful Next() call, Offset() advances by exactly the physical byte size
//     of that decoded record (HeaderSize + 2 + len(Key) + 4 + len(Value)).
//   - If Next() returns an error (io.EOF, truncation, or corruption), Offset() remains unchanged,
//     pointing precisely to the start of the unconsumed record.
func (r *WALReader) Offset() int64 {
	return r.offset
}

// Close closes the underlying file descriptor and marks the reader as closed.
// Subsequent calls to Next() return errors.ErrReaderClosed.
// Close is idempotent; subsequent calls return nil.
func (r *WALReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true

	if r.file == nil {
		return nil
	}
	if err := r.file.Close(); err != nil {
		return fmt.Errorf("wal: close reader %s: %w", r.path, err)
	}

	return nil
}

// Next reads and decodes the next sequential Record from the WAL segment.
//
// Return Contract:
//   - On success: returns (Record, nil) and advances Offset() by the record's physical wire length.
//   - On clean EOF at record boundary: returns (Record{}, io.EOF); Offset() is NOT advanced.
//   - If closed: returns (Record{}, errors.ErrReaderClosed).
//   - On truncation/torn tail: returns (Record{}, err) where err wraps errors.ErrHeaderTruncated
//     or io.ErrUnexpectedEOF; Offset() is NOT advanced.
//   - On checksum corruption: returns (Record{}, *errors.ChecksumMismatchError); Offset() is NOT advanced.
//   - On structural invalidity: returns (Record{}, err); Offset() is NOT advanced.
func (r *WALReader) Next() (Record, error) {
	if r.closed {
		return Record{}, errors.ErrReaderClosed
	}

	rec, err := DecodeRecord(r.file)
	if err != nil {
		if stdErrors.Is(err, io.EOF) {
			return Record{}, io.EOF
		}
		return Record{}, err
	}

	// Advance offset by exact physical record length
	recLen := int64(MinRecordSize + len(rec.Key) + len(rec.Value))
	r.offset += recLen

	return rec, nil
}
