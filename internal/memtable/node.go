package memtable

import (
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

// skipListNode represents an individual multi-level entry in the SkipList.
//
// Ownership & Immutability Model:
//   - key: Owned, defensively cloned binary.InternalKey. External callers cannot
//     mutate internal key state via slice aliasing (enforcing SEC-MEM-INV-01).
//   - value: Owned, defensively copied byte slice (nil or empty for tombstones/deletions).
//   - forward: Private pointer tower sized exactly to the node's declared height.
//     Nodes allocate only the pointers necessary for their height (1 <= height <= MaxHeight).
//
// Concurrency Model Preparation:
//   - In this micro-phase (P03-S01-M01), pointer storage is represented as []*skipListNode.
//   - Future micro-phases introduce atomic pointer reads (atomic.LoadPointer) for lock-free
//     traversal without mutexes, and atomic pointer publication (atomic.StorePointer)
//     splicing pointers bottom-up (Level 0 to Level H-1).
type skipListNode struct {
	key     binary.InternalKey
	value   []byte
	forward []*skipListNode
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

	var valCopy []byte
	if len(value) > 0 {
		valCopy = make([]byte, len(value))
		copy(valCopy, value)
	}

	return &skipListNode{
		key:     key.Clone(),
		value:   valCopy,
		forward: make([]*skipListNode, height),
	}, nil
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
		forward: make([]*skipListNode, height),
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
	return n.forward[level], nil
}

// setForward sets the forward pointer at the specified 0-indexed level.
// Returns an *errors.InvalidSkipListLevelError if level is outside [0, height()-1].
func (n *skipListNode) setForward(level int, next *skipListNode) error {
	if level < 0 || level >= len(n.forward) {
		return &errors.InvalidSkipListLevelError{
			Level:    level,
			MaxLevel: len(n.forward) - 1,
		}
	}
	n.forward[level] = next
	return nil
}

// getKey returns the node's InternalKey.
func (n *skipListNode) getKey() binary.InternalKey {
	return n.key
}

// getValue returns a defensive copy of the node's value slice.
// Returns nil if the node stores a nil value (e.g. tombstone).
func (n *skipListNode) getValue() []byte {
	if n.value == nil {
		return nil
	}
	valCopy := make([]byte, len(n.value))
	copy(valCopy, n.value)
	return valCopy
}

// rawValue returns the internal value slice directly for internal, non-escaping read operations.
func (n *skipListNode) rawValue() []byte {
	return n.value
}
