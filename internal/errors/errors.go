package errors

import (
	stdErrors "errors"
	"fmt"
	"io/fs"
)

// Sentinel errors representing fundamental domain failure conditions in Lattice.
// All sentinels can be inspected across wrapping chains using errors.Is().
var (
	// ErrKeyNotFound indicates that the requested key does not exist in any storage layer
	// (active MemTable, immutable MemTables, or SSTables).
	ErrKeyNotFound = stdErrors.New("key not found")

	// ErrEmptyKey indicates that a zero-length key was provided where a valid key is required.
	ErrEmptyKey = stdErrors.New("key cannot be empty")

	// ErrKeyTooLarge indicates that a key exceeds the maximum allowed size (65,535 bytes / 64 KB - 1).
	ErrKeyTooLarge = stdErrors.New("key exceeds maximum allowed size")

	// ErrValueTooLarge indicates that a value exceeds the maximum allowed size (4 MB).
	ErrValueTooLarge = stdErrors.New("value exceeds maximum allowed size")

	// ErrChecksumMismatch indicates that a CRC32 checksum verification failed,
	// signalling data bit-rot, torn block, or transmission corruption.
	ErrChecksumMismatch = stdErrors.New("checksum mismatch: data corrupted")

	// ErrTornWrite indicates that a partial, uncommitted write was detected at the
	// physical end-of-file of an active WAL during crash recovery replay.
	ErrTornWrite = stdErrors.New("torn write detected at tail of log")

	// ErrCompactionRunning indicates that a requested compaction operation cannot
	// proceed because another compaction worker is actively processing the targeted levels.
	ErrCompactionRunning = stdErrors.New("compaction already in progress")

	// ErrVarintOverflow indicates that a varint byte sequence exceeds the maximum
	// 64-bit unsigned integer representation (exceeds 10 bytes, or the 10th byte
	// contains invalid payload bits).
	ErrVarintOverflow = stdErrors.New("varint exceeds maximum 64-bit integer size")

	// ErrVarintTruncated indicates that a buffer ended prematurely while decoding
	// a varint before encountering a terminating byte.
	ErrVarintTruncated = stdErrors.New("varint buffer truncated or incomplete")

	// ErrInvalidOpType indicates that an operation type byte does not correspond
	// to any recognized database operation (only PUT and DELETE/TOMBSTONE are valid).
	ErrInvalidOpType = stdErrors.New("invalid operation type")

	// ErrSeqNumOverflow indicates that incrementing a sequence number would exceed
	// the maximum 64-bit unsigned integer representation (wraparound prohibited).
	ErrSeqNumOverflow = stdErrors.New("sequence number overflow")

	// ErrInternalKeyTruncated indicates that an encoded internal key byte sequence
	// is shorter than the minimum required size (user key + 9-byte sequence/op trailer).
	ErrInternalKeyTruncated = stdErrors.New("internal key buffer truncated: missing trailer or user key")

	// ErrHeaderTruncated indicates that a buffer provided to decode a WAL record header
	// is shorter than the required 21 bytes.
	ErrHeaderTruncated = stdErrors.New("wal record header truncated: buffer smaller than 21 bytes")

	// ErrInvalidRecordType indicates that a WAL record type byte does not correspond
	// to any recognized record type (PUT, DELETE, BATCH_START, BATCH_COMMIT).
	ErrInvalidRecordType = stdErrors.New("invalid wal record type")

	// ErrInvalidRecordPayload indicates that a WAL record payload does not conform to its record type
	// (e.g. non-empty value for DELETE/tombstone, or non-empty key/value for batch markers).
	ErrInvalidRecordPayload = stdErrors.New("invalid wal record payload")

	// ErrNotADirectory indicates that a filesystem path expected to be a directory
	// is a regular file, symlink, or other non-directory object.
	ErrNotADirectory = stdErrors.New("path is not a directory")

	// ErrWriterClosed indicates that an operation was attempted on a closed WAL writer.
	ErrWriterClosed = stdErrors.New("wal writer is closed")
)

// KeyTooLargeError provides structured context when a key violates maximum size limits.
// It matches ErrKeyTooLarge when interrogated with errors.Is().
// Note: To prevent sensitive data leakage, raw key bytes are deliberately omitted.
type KeyTooLargeError struct {
	KeySize uint32
	MaxSize uint32
}

func (e *KeyTooLargeError) Error() string {
	if e == nil {
		return ErrKeyTooLarge.Error()
	}
	return fmt.Sprintf("key size %d bytes exceeds maximum allowed size of %d bytes", e.KeySize, e.MaxSize)
}

// Is reports whether this error matches target sentinel ErrKeyTooLarge.
func (e *KeyTooLargeError) Is(target error) bool {
	return target == ErrKeyTooLarge
}

// ValueTooLargeError provides structured context when a value violates maximum size limits.
// It matches ErrValueTooLarge when interrogated with errors.Is().
// Note: To prevent sensitive data leakage, raw value bytes are deliberately omitted.
type ValueTooLargeError struct {
	ValueSize uint32
	MaxSize   uint32
}

func (e *ValueTooLargeError) Error() string {
	if e == nil {
		return ErrValueTooLarge.Error()
	}
	return fmt.Sprintf("value size %d bytes exceeds maximum allowed size of %d bytes", e.ValueSize, e.MaxSize)
}

// Is reports whether this error matches target sentinel ErrValueTooLarge.
func (e *ValueTooLargeError) Is(target error) bool {
	return target == ErrValueTooLarge
}

// ChecksumMismatchError provides structured diagnostics when a CRC32 checksum verification fails.
// It matches ErrChecksumMismatch when interrogated with errors.Is().
type ChecksumMismatchError struct {
	Offset   int64
	Expected uint32
	Actual   uint32
}

func (e *ChecksumMismatchError) Error() string {
	if e == nil {
		return ErrChecksumMismatch.Error()
	}
	return fmt.Sprintf("checksum mismatch at offset %d: expected 0x%08x, got 0x%08x", e.Offset, e.Expected, e.Actual)
}

// Is reports whether this error matches target sentinel ErrChecksumMismatch.
func (e *ChecksumMismatchError) Is(target error) bool {
	return target == ErrChecksumMismatch
}

// TornWriteError provides structured diagnostics when an incomplete write is detected
// at the tail of a log file during crash recovery.
// It matches ErrTornWrite when interrogated with errors.Is().
type TornWriteError struct {
	Offset int64
	Reason string
}

func (e *TornWriteError) Error() string {
	if e == nil {
		return ErrTornWrite.Error()
	}
	if e.Reason != "" {
		return fmt.Sprintf("torn write detected at tail offset %d: %s", e.Offset, e.Reason)
	}
	return fmt.Sprintf("torn write detected at tail offset %d", e.Offset)
}

// Is reports whether this error matches target sentinel ErrTornWrite.
func (e *TornWriteError) Is(target error) bool {
	return target == ErrTornWrite
}

// InvalidOpTypeError provides structured context when an unrecognized operation type byte is encountered.
// It matches ErrInvalidOpType when interrogated with errors.Is().
type InvalidOpTypeError struct {
	Op byte
}

func (e *InvalidOpTypeError) Error() string {
	if e == nil {
		return ErrInvalidOpType.Error()
	}
	return fmt.Sprintf("invalid operation type: 0x%02x", e.Op)
}

// Is reports whether this error matches target sentinel ErrInvalidOpType.
func (e *InvalidOpTypeError) Is(target error) bool {
	return target == ErrInvalidOpType
}

// SeqNumOverflowError provides structured context when incrementing a sequence number
// exceeds the maximum 64-bit unsigned integer limit.
// It matches ErrSeqNumOverflow when interrogated with errors.Is().
type SeqNumOverflowError struct {
	Current uint64
}

func (e *SeqNumOverflowError) Error() string {
	if e == nil {
		return ErrSeqNumOverflow.Error()
	}
	return fmt.Sprintf("sequence number overflow: current %d is at maximum uint64 limit", e.Current)
}

// Is reports whether this error matches target sentinel ErrSeqNumOverflow.
func (e *SeqNumOverflowError) Is(target error) bool {
	return target == ErrSeqNumOverflow
}

// InvalidRecordTypeError provides structured context when an unrecognized WAL record type byte is encountered.
// It matches ErrInvalidRecordType when interrogated with errors.Is().
type InvalidRecordTypeError struct {
	Type byte
}

func (e *InvalidRecordTypeError) Error() string {
	if e == nil {
		return ErrInvalidRecordType.Error()
	}
	return fmt.Sprintf("invalid wal record type: 0x%02x", e.Type)
}

// Is reports whether this error matches target sentinel ErrInvalidRecordType.
func (e *InvalidRecordTypeError) Is(target error) bool {
	return target == ErrInvalidRecordType
}

// InvalidRecordPayloadError provides structured context when a WAL record payload
// violates constraints for its record type.
// It matches ErrInvalidRecordPayload when interrogated with errors.Is().
type InvalidRecordPayloadError struct {
	Type   byte
	Reason string
}

func (e *InvalidRecordPayloadError) Error() string {
	if e == nil {
		return ErrInvalidRecordPayload.Error()
	}
	if e.Reason != "" {
		return fmt.Sprintf("invalid wal record payload for type 0x%02x: %s", e.Type, e.Reason)
	}
	return fmt.Sprintf("invalid wal record payload for type 0x%02x", e.Type)
}

// Is reports whether this error matches target sentinel ErrInvalidRecordPayload.
func (e *InvalidRecordPayloadError) Is(target error) bool {
	return target == ErrInvalidRecordPayload
}

// NotADirectoryError provides structured context when an expected directory path
// is a regular file, symlink, or other non-directory object.
// It matches ErrNotADirectory when interrogated with errors.Is().
type NotADirectoryError struct {
	Path string
	Mode fs.FileMode
}

func (e *NotADirectoryError) Error() string {
	if e == nil {
		return ErrNotADirectory.Error()
	}
	if e.Path != "" {
		return fmt.Sprintf("path %q is not a directory (mode: %s)", e.Path, e.Mode)
	}
	return ErrNotADirectory.Error()
}

// Is reports whether this error matches target sentinel ErrNotADirectory.
func (e *NotADirectoryError) Is(target error) bool {
	return target == ErrNotADirectory
}
