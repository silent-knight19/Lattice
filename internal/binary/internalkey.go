package binary

import (
	"bytes"
	"fmt"

	"github.com/silent-knight19/lattice/internal/errors"
)

// InternalKeyTrailerLen is the fixed size in bytes of the sequence number (8 bytes)
// and operation type (1 byte) trailer appended to user keys in internal storage.
const InternalKeyTrailerLen = 9

// InternalKey represents the multi-version storage-engine key combining a user key,
// a 64-bit sequence number, and an operation type (PUT or DELETE/TOMBSTONE).
//
// In Lattice's LSM architecture, InternalKey establishes total deterministic version
// ordering across MemTable nodes, SSTable index blocks, and compaction merge heaps.
type InternalKey struct {
	UserKey []byte
	SeqNum  SeqNum
	OpType  OpType
}

// NewInternalKey validates the user key and operation type, returning an InternalKey
// with an owned (defensively copied) copy of userKey.
//
// Callers requiring zero-allocation construction in hot read paths may initialize
// the InternalKey struct directly, borrowing the underlying backing slice.
func NewInternalKey(userKey []byte, seqNum SeqNum, opType OpType) (InternalKey, error) {
	if err := ValidateKey(userKey); err != nil {
		return InternalKey{}, err
	}
	if err := opType.Validate(); err != nil {
		return InternalKey{}, err
	}

	userKeyCopy := make([]byte, len(userKey))
	copy(userKeyCopy, userKey)

	return InternalKey{
		UserKey: userKeyCopy,
		SeqNum:  seqNum,
		OpType:  opType,
	}, nil
}

// Clone returns an independent deep copy of the InternalKey with a newly allocated
// UserKey slice.
func (k InternalKey) Clone() InternalKey {
	if k.UserKey == nil {
		return InternalKey{
			UserKey: nil,
			SeqNum:  k.SeqNum,
			OpType:  k.OpType,
		}
	}
	userKeyCopy := make([]byte, len(k.UserKey))
	copy(userKeyCopy, k.UserKey)
	return InternalKey{
		UserKey: userKeyCopy,
		SeqNum:  k.SeqNum,
		OpType:  k.OpType,
	}
}

// String returns a human-readable representation of the InternalKey with the user key
// formatted using Go-quoted string syntax to safely represent binary or arbitrary bytes.
func (k InternalKey) String() string {
	return fmt.Sprintf("InternalKey(%q, seq=%s, op=%s)", k.UserKey, k.SeqNum.String(), k.OpType.String())
}

// Equal reports whether k and other sort equivalently under CompareInternalKey.
func (k InternalKey) Equal(other InternalKey) bool {
	return CompareInternalKey(k, other) == 0
}

// Compare compares k against other using canonical storage engine ordering.
func (k InternalKey) Compare(other InternalKey) int {
	return CompareInternalKey(k, other)
}

// CompareInternalKey compares two InternalKeys according to canonical storage engine ordering:
//  1. UserKey ascending (raw byte lexicographical order)
//  2. SeqNum descending (higher/newer sequence numbers sort before lower/older sequence numbers)
//  3. OpType descending (tie-breaker if user key and sequence number are identical:
//     OpTypeDelete (0x02) sorts before OpTypePut (0x01))
//
// Returns:
//
//	-1 if a sorts before b
//	 0 if a and b are equivalent
//	+1 if a sorts after b
//
// This comparator guarantees the foundational LSM invariant that point lookups
// and range iterators encounter the latest version of a key first.
func CompareInternalKey(a, b InternalKey) int {
	if cmp := bytes.Compare(a.UserKey, b.UserKey); cmp != 0 {
		return cmp
	}
	if a.SeqNum > b.SeqNum {
		return -1
	}
	if a.SeqNum < b.SeqNum {
		return 1
	}
	if a.OpType > b.OpType {
		return -1
	}
	if a.OpType < b.OpType {
		return 1
	}
	return 0
}

// AppendInternalKey serializes key into dst and returns the extended slice.
// Layout: [ UserKey (Var bytes) | SeqNum (8 bytes, Big-Endian) | OpType (1 byte) ]
// Total appended length is len(key.UserKey) + InternalKeyTrailerLen.
func AppendInternalKey(dst []byte, key InternalKey) []byte {
	dst = append(dst, key.UserKey...)
	var trailer [InternalKeyTrailerLen]byte
	PutUint64(trailer[0:8], uint64(key.SeqNum))
	trailer[8] = byte(key.OpType)
	return append(dst, trailer[:]...)
}

// EncodeInternalKey serializes key into a newly allocated byte slice of length
// len(key.UserKey) + InternalKeyTrailerLen.
func EncodeInternalKey(key InternalKey) []byte {
	buf := make([]byte, 0, len(key.UserKey)+InternalKeyTrailerLen)
	return AppendInternalKey(buf, key)
}

// DecodeInternalKey deserializes a byte slice into an InternalKey.
// The input slice must contain at least MinKeyLen + InternalKeyTrailerLen (10) bytes.
//
// DecodeInternalKey creates an owned copy of the UserKey slice so the returned
// InternalKey does not retain a reference to a potentially large transient buffer.
func DecodeInternalKey(data []byte) (InternalKey, error) {
	if len(data) < MinKeyLen+InternalKeyTrailerLen {
		return InternalKey{}, errors.ErrInternalKeyTruncated
	}

	userKeyLen := len(data) - InternalKeyTrailerLen
	userKey := data[:userKeyLen]
	if err := ValidateKey(userKey); err != nil {
		return InternalKey{}, err
	}

	seqNum := SeqNum(GetUint64(data[userKeyLen : userKeyLen+8]))
	opType, err := ParseOpType(data[userKeyLen+8])
	if err != nil {
		return InternalKey{}, err
	}

	userKeyCopy := make([]byte, userKeyLen)
	copy(userKeyCopy, userKey)

	return InternalKey{
		UserKey: userKeyCopy,
		SeqNum:  seqNum,
		OpType:  opType,
	}, nil
}
