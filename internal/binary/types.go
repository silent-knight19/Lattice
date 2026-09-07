package binary

import (
	"fmt"
	"math"
	"strconv"

	"github.com/silent-knight19/lattice/internal/errors"
)

// OpType represents the database operation type stored in Write-Ahead Log records,
// SkipList MemTable nodes, and SSTable entry metadata.
type OpType byte

const (
	// OpTypeInvalid represents an uninitialized or invalid operation type.
	OpTypeInvalid OpType = 0x00

	// OpTypePut represents a standard key-value insertion or overwrite.
	OpTypePut OpType = 0x01

	// OpTypeDelete represents a tombstone deletion marker.
	OpTypeDelete OpType = 0x02

	// OpTypeTombstone is an alias for OpTypeDelete, matching storage engine terminology.
	OpTypeTombstone OpType = OpTypeDelete
)

// Valid reports whether the operation type is one of the recognized operations (PUT or DELETE/TOMBSTONE).
func (op OpType) Valid() bool {
	return op == OpTypePut || op == OpTypeDelete
}

// Validate returns nil if the operation type is valid, or an *errors.InvalidOpTypeError
// matching errors.ErrInvalidOpType if invalid.
func (op OpType) Validate() error {
	if op.Valid() {
		return nil
	}
	return &errors.InvalidOpTypeError{Op: byte(op)}
}

// String returns a human-readable string representation of the operation type.
func (op OpType) String() string {
	switch op {
	case OpTypePut:
		return "PUT"
	case OpTypeDelete:
		return "DELETE"
	default:
		return fmt.Sprintf("UNKNOWN(0x%02x)", byte(op))
	}
}

// ParseOpType parses a raw byte into an OpType, validating that it matches a known operation.
// Returns OpTypeInvalid and an error if the byte is unrecognized.
func ParseOpType(b byte) (OpType, error) {
	op := OpType(b)
	if err := op.Validate(); err != nil {
		return OpTypeInvalid, err
	}
	return op, nil
}

// SeqNum represents a 64-bit monotonically increasing sequence number providing
// a total ordering over all database operations for MVCC and conflict resolution.
type SeqNum uint64

const (
	// MinSeqNum is the minimum representable sequence number (0).
	MinSeqNum SeqNum = 0

	// MaxSeqNum is the maximum representable sequence number (2^64 - 1).
	MaxSeqNum SeqNum = math.MaxUint64
)

// Next returns the immediately following sequence number (s + 1).
// If s is already at MaxSeqNum, it returns an *errors.SeqNumOverflowError
// matching errors.ErrSeqNumOverflow rather than silently wrapping around to 0.
func (s SeqNum) Next() (SeqNum, error) {
	if s == MaxSeqNum {
		return MaxSeqNum, &errors.SeqNumOverflowError{Current: uint64(s)}
	}
	return s + 1, nil
}

// String returns the decimal string representation of the sequence number.
func (s SeqNum) String() string {
	return strconv.FormatUint(uint64(s), 10)
}
