package memtable

import (
	"sync/atomic"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

const (
	// MinHeight is the minimum valid height of any SkipList node tower (Level 0).
	MinHeight = 1

	// MaxHeight is the maximum permitted height of any SkipList node tower (Levels 0..15).
	// This hard upper bound enforces:
	// 1. Structural invariant: bounds SkipList tower levels to Lmax = 16.
	// 2. Resource-exhaustion defense: strictly caps pointer slice allocation to 16 pointers
	//    (128 bytes on 64-bit platforms), preventing attacker-controlled allocation bombs.
	MaxHeight = 16

	// DefaultPromotionProbability is the geometric distribution parameter p = 0.25 (1/4).
	// With p = 0.25:
	// - Expected average pointers per node is 1 / (1 - p) = 4/3 ≈ 1.33.
	// - Search path crosses on average 1/p = 4 nodes per level.
	// - MaxHeight = 16 accommodates ~4^15 ≈ 1 billion keys with O(log N) search complexity.
	DefaultPromotionProbability = 0.25
)

// nodeValue wraps a byte slice in an immutable container to enable lock-free atomic value
// publishing and race-free reads under concurrent SearchConcurrent invocations.
type nodeValue struct {
	data []byte
}

// skipListNode represents an individual multi-level entry in the SkipList.
//
// Ownership & Immutability Model:
//   - key: Owned, defensively cloned binary.InternalKey. External callers cannot
//     mutate internal key state via slice aliasing (enforcing SEC-MEM-INV-01).
//   - value: Atomic pointer to an immutable nodeValue container holding a defensively
//     copied byte slice (nil or empty for tombstones/deletions).
//   - forward: Private pointer tower sized exactly to the node's declared height.
//     Each forward pointer is an atomic.Pointer[skipListNode], permitting lock-free reader
//     traversal without mutexes while ensuring safe atomic publication by the serialized writer.
type skipListNode struct {
	key     binary.InternalKey
	value   atomic.Pointer[nodeValue]
	forward []atomic.Pointer[skipListNode]
}

// newSkipListNode creates and initializes a new skipListNode with the given key, value, and tower height.
//
// Bounds and Invariant Enforcement:
//   - Height must satisfy 1 <= height <= MaxHeight (16). Heights outside this range are rejected
//     with *errors.InvalidSkipListHeightError without allocating heap memory.
//   - UserKey and OpType are validated via binary.ValidateKey and key.OpType.Validate().
//   - Value is validated via binary.ValidateValue.
//   - Keys and values are defensively copied to prevent external caller buffer mutations from
//     corrupting internal node state (SEC-MEM-INV-01).
//   - The forward slice is allocated with len = cap = height, guaranteeing P03-S01-INV-02.
func newSkipListNode(key binary.InternalKey, value []byte, height int) (*skipListNode, error) {
	if height < MinHeight || height > MaxHeight {
		return nil, &errors.InvalidSkipListHeightError{
			Height:    height,
			MinHeight: MinHeight,
			MaxHeight: MaxHeight,
		}
	}
	if err := binary.ValidateKey(key.UserKey); err != nil {
		return nil, err
	}
	if err := key.OpType.Validate(); err != nil {
		return nil, err
	}
	if err := binary.ValidateValue(value); err != nil {
		return nil, err
	}

	n := &skipListNode{
		key:     key.Clone(),
		forward: make([]atomic.Pointer[skipListNode], height),
	}

	if len(value) > 0 {
		valCopy := make([]byte, len(value))
		copy(valCopy, value)
		n.value.Store(&nodeValue{data: valCopy})
	}

	return n, nil
}

// newSentinelNode creates a head sentinel node with the given height and no key/value.
// Sentinel nodes serve as anchor headers for SkipList traversal.
func newSentinelNode(height int) (*skipListNode, error) {
	if height < MinHeight || height > MaxHeight {
		return nil, &errors.InvalidSkipListHeightError{
			Height:    height,
			MinHeight: MinHeight,
			MaxHeight: MaxHeight,
		}
	}
	return &skipListNode{
		forward: make([]atomic.Pointer[skipListNode], height),
	}, nil
}

// height returns the tower height (number of forward pointer levels) of this node.
func (n *skipListNode) height() int {
	return len(n.forward)
}

// forwardAt returns the forward pointer at the specified 0-indexed level.
// Returns an *errors.InvalidSkipListLevelError if level is outside [0, height()-1].
func (n *skipListNode) forwardAt(level int) (*skipListNode, error) {
	if level < 0 || level >= len(n.forward) {
		return nil, &errors.InvalidSkipListLevelError{
			Level:    level,
			MaxLevel: len(n.forward) - 1,
		}
	}
	return n.forward[level].Load(), nil
}

// setForward sets the forward pointer at the specified 0-indexed level using atomic store.
// Returns an *errors.InvalidSkipListLevelError if level is outside [0, height()-1].
func (n *skipListNode) setForward(level int, next *skipListNode) error {
	if level < 0 || level >= len(n.forward) {
		return &errors.InvalidSkipListLevelError{
			Level:    level,
			MaxLevel: len(n.forward) - 1,
		}
	}
	n.forward[level].Store(next)
	return nil
}

// getKey returns the node's InternalKey.
func (n *skipListNode) getKey() binary.InternalKey {
	return n.key
}

// getValue returns a defensive copy of the node's value slice.
// Returns nil if the node stores a nil value (e.g. tombstone).
func (n *skipListNode) getValue() []byte {
	v := n.value.Load()
	if v == nil || v.data == nil {
		return nil
	}
	valCopy := make([]byte, len(v.data))
	copy(valCopy, v.data)
	return valCopy
}

// rawValue returns the internal value slice directly for internal, non-escaping read operations.
func (n *skipListNode) rawValue() []byte {
	v := n.value.Load()
	if v == nil {
		return nil
	}
	return v.data
}
