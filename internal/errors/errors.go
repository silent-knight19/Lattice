package errors

import (
	stdErrors "errors"
	"fmt"
)

// Sentinel errors representing fundamental domain failure conditions in Lattice.
// All sentinels can be inspected across wrapping chains using errors.Is().
var (
	// ErrKeyNotFound indicates that the requested key does not exist in any storage layer
	// (active MemTable, immutable MemTables, or SSTables).
	ErrKeyNotFound = stdErrors.New("key not found")

	// ErrEmptyKey indicates that a zero-length key was provided where a valid key is required.
	ErrEmptyKey = stdErrors.New("key cannot be empty")

	// ErrKeyTooLarge indicates that a key exceeds the maximum allowed size (64 KB).
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
