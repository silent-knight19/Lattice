package wal

import (
	stdErrors "errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/silent-knight19/lattice/internal/errors"
)

// RecoveryResult contains the diagnostic outcome of a WAL segment recovery operation.
type RecoveryResult struct {
	// Path is the canonical filesystem path of the recovered WAL segment file.
	Path string

	// ValidRecords is the count of complete, checksum-verified records preserved in the segment.
	ValidRecords int

	// RecoveredOffset is the final physical byte size of the valid prefix.
	// If a torn tail was truncated, this marks the exact truncation boundary.
	// If the file was clean, this equals the initial file size.
	RecoveredOffset int64

	// Truncated reports whether physical truncation was performed on the file.
	// Accurately reflects whether physical file mutation occurred, even if subsequent
	// synchronization or post-condition verification returns an error.
	// False if the segment ended at a clean record boundary, was empty, or truncation failed.
	Truncated bool
}

// RecoverSegment inspects a WAL segment file, preserves all valid preceding records,
// detects an incomplete/torn record at EOF, and safely truncates the segment to the
// last valid record boundary.
//
// Operational Contract & Recovery Invariants:
//  1. Quiescent Segment Invariant:
//     Assumes the target segment is quiescent (not concurrently appended to by a WALWriter).
//  2. Valid Prefix Preservation:
//     If a segment contains valid records A, B, C followed by an incomplete tail at EOF,
//     recovery preserves A, B, C exactly and removes only the incomplete tail bytes.
//  3. Clean EOF Semantics (No Mutation):
//     If a segment ends cleanly at a record boundary (or is empty), no truncation or
//     synchronization is performed; Truncated is reported as false.
//  4. Complete Corrupt Records vs. Torn Tails:
//     A complete record with a bad CRC (errors.ErrChecksumMismatch) or invalid type byte
//     (errors.ErrInvalidRecordType) is treated as FATAL corruption, NOT an automatically
//     recoverable torn write. Recovery fails closed without modifying the file.
//  5. Middle Corruption Fails Closed:
//     If corruption occurs anywhere prior to EOF, recovery halts immediately and propagates
//     the error. It never skips corrupt records to resume replaying later records.
//  6. In-Place Descriptor Mutation & Inode Pinning:
//     The target file is opened once in os.O_RDWR mode without O_CREATE or O_TRUNC.
//     Inode identity is validated via os.SameFile. Truncation, synchronization, and
//     post-condition verification occur strictly through the verified open descriptor.
//  7. Post-Condition Verification:
//     After truncation and f.Sync(), recovery verifies that the file size matches the valid
//     prefix offset, rewinds the descriptor, and verifies that all valid records decode
//     cleanly and terminate at io.EOF before returning success.
func RecoverSegment(path string) (RecoveryResult, error) {
	return recoverSegmentWithSeams(path, (*os.File).Sync, (*os.File).Truncate)
}

// RecoverSegmentByID resolves <db_path>/wal/wal_%012d.log and executes RecoverSegment.
func RecoverSegmentByID(dbPath string, id uint64) (RecoveryResult, error) {
	return RecoverSegment(SegmentPath(dbPath, id))
}

// recoverSegmentWithSeams allows deterministic injection of sync and truncate behaviors for testing.
func recoverSegmentWithSeams(
	path string,
	syncFn func(f *os.File) error,
	truncateFn func(f *os.File, size int64) error,
) (res RecoveryResult, retErr error) {
	if path == "" {
		return RecoveryResult{}, fmt.Errorf("%w: path cannot be empty", os.ErrInvalid)
	}

	cleanPath := filepath.Clean(path)
	res.Path = cleanPath

	// Pre-open inspection: verify target exists and is a regular file, rejecting symlinks and directories
	info, err := os.Lstat(cleanPath)
	if err != nil {
		return res, fmt.Errorf("wal: failed to inspect recovery path %s: %w", cleanPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return res, fmt.Errorf("wal: cannot recover symlink %s: %w", cleanPath, os.ErrInvalid)
	}
	if info.IsDir() {
		return res, &errors.NotADirectoryError{
			Path: cleanPath,
			Mode: info.Mode(),
		}
	}
	if !info.Mode().IsRegular() {
		return res, fmt.Errorf("wal: path %s is not a regular file (mode: %s): %w", cleanPath, info.Mode(), os.ErrInvalid)
	}

	// Open descriptor for in-place read/truncate without O_CREATE or O_TRUNC
	f, err := os.OpenFile(cleanPath, os.O_RDWR, 0)
	if err != nil {
		return res, fmt.Errorf("wal: failed to open recovery file %s: %w", cleanPath, err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && retErr == nil {
			retErr = fmt.Errorf("wal: close recovery file %s: %w", cleanPath, closeErr)
		}
	}()

	// Post-open verification: prove descriptor matches disk inode and is regular
	finfo, statErr := f.Stat()
	if statErr != nil {
		return res, fmt.Errorf("wal: failed to stat opened recovery file %s: %w", cleanPath, statErr)
	}
	if !finfo.Mode().IsRegular() {
		return res, fmt.Errorf("wal: recovery file %s is not a regular file (mode: %s): %w", cleanPath, finfo.Mode(), os.ErrInvalid)
	}

	postInfo, lstatErr := os.Lstat(cleanPath)
	if lstatErr != nil {
		return res, fmt.Errorf("wal: failed to lstat recovery file %s: %w", cleanPath, lstatErr)
	}
	if !os.SameFile(finfo, postInfo) {
		return res, fmt.Errorf("wal: recovery file %s was replaced during open: %w", cleanPath, os.ErrInvalid)
	}

	initialFileSize := finfo.Size()
	var validOffset int64
	var validCount int
	var isTornTail bool

	for {
		rec, decodeErr := DecodeRecord(f)
		if decodeErr == nil {
			recLen := int64(MinRecordSize + len(rec.Key) + len(rec.Value))
			validOffset += recLen
			validCount++
			continue
		}

		if stdErrors.Is(decodeErr, io.EOF) {
			// Clean EOF at record boundary (or empty file)
			break
		}

		if stdErrors.Is(decodeErr, errors.ErrHeaderTruncated) || stdErrors.Is(decodeErr, io.ErrUnexpectedEOF) {
			// Incomplete record at EOF -> recoverable torn tail
			isTornTail = true
			break
		}

		// Any other error (ChecksumMismatchError, InvalidRecordTypeError,
		// InvalidRecordPayloadError, KeyTooLargeError, ValueTooLargeError, or OS read error)
		// represents mid-log corruption, structural corruption, or hardware I/O failure.
		// Invariant: Fail closed, do NOT truncate, do NOT skip.
		res.ValidRecords = validCount
		res.RecoveredOffset = validOffset
		res.Truncated = false
		return res, decodeErr
	}

	res.ValidRecords = validCount
	res.RecoveredOffset = validOffset

	if !isTornTail {
		// Clean EOF at record boundary: no mutation required
		res.Truncated = false
		return res, nil
	}

	// Verify physical file size exceeds valid prefix offset
	if validOffset >= initialFileSize {
		res.Truncated = false
		return res, fmt.Errorf("wal: torn tail indicated but valid offset %d equals or exceeds file size %d", validOffset, initialFileSize)
	}

	// Perform physical truncation on open descriptor
	if err := truncateFn(f, validOffset); err != nil {
		res.Truncated = false
		return res, fmt.Errorf("wal: failed to truncate torn tail at offset %d in %s: %w", validOffset, cleanPath, err)
	}

	// Physical truncation has succeeded on disk. From this point onward,
	// Truncated must remain true to accurately reflect that physical file mutation occurred,
	// even if subsequent synchronization or post-condition verification returns an error.
	res.Truncated = true

	// Synchronize file data and inode metadata to stable storage
	if err := syncFn(f); err != nil {
		return res, fmt.Errorf("wal: failed to sync truncated file %s: %w", cleanPath, err)
	}

	// Invariant 9: Post-condition verification - verify file size
	postStat, err := f.Stat()
	if err != nil {
		return res, fmt.Errorf("wal: failed to stat truncated file %s: %w", cleanPath, err)
	}
	if postStat.Size() != validOffset {
		return res, fmt.Errorf("wal: post-truncation file size %d does not match expected valid offset %d", postStat.Size(), validOffset)
	}

	// Invariant 9: Post-condition verification - rewind and verify valid records to EOF
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return res, fmt.Errorf("wal: failed to rewind file %s for post-truncation verification: %w", cleanPath, err)
	}

	for i := 0; i < validCount; i++ {
		if _, err := DecodeRecord(f); err != nil {
			return res, fmt.Errorf("wal: post-truncation verification failed at record %d: %w", i, err)
		}
	}

	if _, err := DecodeRecord(f); !stdErrors.Is(err, io.EOF) {
		return res, fmt.Errorf("wal: post-truncation verification expected io.EOF after record %d, got: %w", validCount, err)
	}

	return res, nil
}
