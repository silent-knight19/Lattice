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

	// OpTypeBatch represents an atomic multi-operation batch.
	OpTypeBatch OpType = 0x05
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
	case OpTypeBatch:
		return "BATCH"
	default:
		return fmt.Sprintf("UNKNOWN(0x%02x)", byte(op))
	}
}

// BatchOp represents a single mutation operation within a multi-operation WriteBatch.
type BatchOp struct {
	Type  OpType
	Key   []byte
	Value []byte
}

// Validate verifies that the batch operation satisfies key and value constraints.
func (b BatchOp) Validate() error {
	if b.Type != OpTypePut && b.Type != OpTypeDelete {
		return fmt.Errorf("%w: invalid batch op type 0x%02x", errors.ErrInvalidOpType, byte(b.Type))
	}
	if err := ValidateKey(b.Key); err != nil {
		return err
	}
	if b.Type == OpTypePut {
		if err := ValidateValue(b.Value); err != nil {
			return err
		}
	} else if len(b.Value) != 0 {
		return fmt.Errorf("%w: DELETE batch op cannot contain non-empty value", errors.ErrInvalidOpType)
	}
	return nil
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
