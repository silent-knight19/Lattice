package memtable

import (
	"bytes"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

type iteratorState byte

const (
	stateUnpositioned iteratorState = iota
	statePositioned
	stateExhausted
)

// Iterator provides forward sequential and point-seek traversal over the SkipList's
// Level-0 physical sequence.
//
// Concurrency & Consistency Model:
//   - Lock-Free: Traversal along forward pointers uses atomic pointer loads (atomic.Pointer.Load)
//     without acquiring mutex locks, ensuring readers never block writers and writers never block readers.
//   - Live / Weakly-Consistent: Operates over a live mutable SkipList. Insertions ahead of the current
//     position will be observed; insertions behind the current position will not.
//   - Duplicate Updates: Exact duplicate InternalKey updates atomically replace the value container;
//     Value() returns the value at the instant of the call.
//   - Memory Safety: Key() and Value() return defensive copies, guaranteeing callers cannot mutate
//     internal SkipList state.
//   - Physical Sequence: Exposes all physical entries in canonical order (UserKey ASC, SeqNum DESC,
//     OpType DESC). Tombstones (OpTypeDelete) are yielded with Value() == nil.
type Iterator struct {
	sl    *SkipList
	curr  *skipListNode
	state iteratorState
}

// NewIterator creates a new forward iterator over the SkipList.
// The iterator starts unpositioned (Valid() == false).
// Callers may either advance using Next() (which moves to the first element)
// or position explicitly using Seek() or SeekToFirst().
func (s *SkipList) NewIterator() *Iterator {
	return &Iterator{
		sl:    s,
		curr:  nil,
		state: stateUnpositioned,
	}
}

// Valid reports whether the iterator is currently positioned at a valid SkipList entry.
func (it *Iterator) Valid() bool {
	return it.state == statePositioned && it.curr != nil
}

// Next advances the iterator to the next entry in the SkipList.
// Returns true if positioned at a valid entry, or false if the end of the list is reached.
//
// Behavior:
//   - If unpositioned (newly created), Next() advances to the first entry in the SkipList.
//   - If positioned, Next() advances to the next entry along Level 0.
//   - If already exhausted (at EOF), Next() is a safe no-op returning false.
func (it *Iterator) Next() bool {
	switch it.state {
	case stateUnpositioned:
		if it.sl == nil {
			it.state = stateExhausted
			return false
		}
		it.curr = it.sl.head.forward[0].Load()
		if it.curr != nil {
			it.state = statePositioned
			return true
		}
		it.state = stateExhausted
		return false
	case statePositioned:
		if it.curr == nil {
			it.state = stateExhausted
			return false
		}
		it.curr = it.curr.forward[0].Load()
		if it.curr != nil {
			it.state = statePositioned
			return true
		}
		it.state = stateExhausted
		return false
	case stateExhausted:
		return false
	default:
		it.state = stateExhausted
		it.curr = nil
		return false
	}
}

// Key returns a defensive copy of the InternalKey at the current iterator position.
// If the iterator is not valid (Valid() == false), returns a zero-value InternalKey.
//
// Memory Ownership:
// The returned InternalKey owns its UserKey slice, guaranteeing callers mutating
// the returned key slice cannot corrupt internal SkipList state (enforcing SEC-MEM-INV-01).
func (it *Iterator) Key() binary.InternalKey {
	if !it.Valid() {
		return binary.InternalKey{}
	}
	return it.curr.key.Clone()
}

// Value returns a defensive copy of the value slice at the current iterator position.
// If the iterator is not valid (Valid() == false), returns nil.
// For tombstones (OpTypeDelete) or empty values, returns nil.
//
// Memory Ownership:
// The returned slice is a newly allocated defensive copy, guaranteeing callers mutating
// the returned slice cannot corrupt internal SkipList state.
func (it *Iterator) Value() []byte {
	if !it.Valid() {
		return nil
	}
	return it.curr.getValue()
}

// Seek positions the iterator at the first entry whose UserKey is greater than or equal to userKey.
// If userKey is invalid (empty or exceeds MaxKeyLen), the iterator is invalidated and an error is returned.
//
// Behavior:
//   - If one or more entries with userKey exist, Seek positions on the newest revision (highest SeqNum)
//     of that user key, because revisions sort descending by SeqNum.
//   - If no entry with UserKey == userKey exists, Seek positions on the next greater UserKey.
//   - If all entries have UserKey < userKey, the iterator is exhausted (Valid() == false) and returns nil.
//
// Complexity:
// Expected O(log N) comparisons and pointer loads using the multi-level express-lane hierarchy.
func (it *Iterator) Seek(userKey []byte) error {
	if it.sl == nil {
		it.curr = nil
		it.state = stateExhausted
		return errors.ErrIteratorClosed
	}
	if err := binary.ValidateKey(userKey); err != nil {
		it.curr = nil
		it.state = stateExhausted
		return err
	}

	curr := it.sl.head
	h := int(it.sl.height.Load())
	if h > MaxHeight {
		h = MaxHeight
	} else if h < MinHeight {
		h = MinHeight
	}

	for i := h - 1; i >= 0; i-- {
		next := curr.forward[i].Load()
		for next != nil && bytes.Compare(next.key.UserKey, userKey) < 0 {
			curr = next
			next = curr.forward[i].Load()
		}
	}

	candidate := curr.forward[0].Load()
	if candidate != nil {
		it.curr = candidate
		it.state = statePositioned
	} else {
		it.curr = nil
		it.state = stateExhausted
	}
	return nil
}

// SeekToFirst positions the iterator at the first entry in the SkipList.
// If the SkipList is empty, the iterator is exhausted (Valid() == false).
func (it *Iterator) SeekToFirst() {
	if it.sl == nil {
		it.curr = nil
		it.state = stateExhausted
		return
	}
	it.curr = it.sl.head.forward[0].Load()
	if it.curr != nil {
		it.state = statePositioned
	} else {
		it.state = stateExhausted
	}
}

// SeekInternalKey positions the iterator at the first entry whose InternalKey is greater than or equal
// to target according to canonical binary.CompareInternalKey ordering.
// If target.UserKey or target.OpType is invalid, the iterator is invalidated and an error is returned.
func (it *Iterator) SeekInternalKey(target binary.InternalKey) error {
	if it.sl == nil {
		it.curr = nil
		it.state = stateExhausted
		return errors.ErrIteratorClosed
	}
	if err := binary.ValidateKey(target.UserKey); err != nil {
		it.curr = nil
		it.state = stateExhausted
		return err
	}
	if err := target.OpType.Validate(); err != nil {
		it.curr = nil
		it.state = stateExhausted
		return err
	}

	curr := it.sl.head
	h := int(it.sl.height.Load())
	if h > MaxHeight {
		h = MaxHeight
	} else if h < MinHeight {
		h = MinHeight
	}

	for i := h - 1; i >= 0; i-- {
		next := curr.forward[i].Load()
		for next != nil && binary.CompareInternalKey(next.key, target) < 0 {
			curr = next
			next = curr.forward[i].Load()
		}
	}

	candidate := curr.forward[0].Load()
	if candidate != nil {
		it.curr = candidate
		it.state = statePositioned
	} else {
		it.curr = nil
		it.state = stateExhausted
	}
	return nil
}

// Close invalidates the iterator and releases references to internal SkipList nodes.
// Calling Close multiple times is safe and idempotent.
func (it *Iterator) Close() {
	it.curr = nil
	it.sl = nil
	it.state = stateExhausted
}
