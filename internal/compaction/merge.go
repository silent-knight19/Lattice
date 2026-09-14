package compaction

import (
	"bytes"
	"container/heap"
	stdErrors "errors"
	"fmt"
	"io"
	"sync"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
)

var _ sstable.Iterator = (*MergingIterator)(nil)

// MaxMergingIterators defines the maximum allowable number of child iterators
// in a single MergingIterator (10,000) to prevent min-heap resource exhaustion (P08-SEC-018).
const MaxMergingIterators = 10000

// Iterator represents the sequential record traversal interface consumed by MergingIterator.
// It is directly satisfied by sstable.Iterator, memtable.Iterator, and nested merge iterators.
type Iterator = sstable.Iterator

type mergeState int

const (
	mergeStateUninitialized mergeState = iota
	mergeStateValid
	mergeStateExhausted
	mergeStateFailed
	mergeStateClosed
)

// heapItem encapsulates the current record state of one active child iterator in the min-heap.
type heapItem struct {
	iter  Iterator
	key   binary.InternalKey
	value []byte
	index int // stable child index from the input slice, used as deterministic tie-breaker
}

// mergeHeap implements heap.Interface for a collection of *heapItem entries.
// The root of the heap is the smallest record under canonical binary.CompareInternalKey order.
// When two records have identical InternalKeys (same UserKey, SeqNum, and OpType), the record
// from the lower child index wins the tie-break, ensuring strict, reproducible determinism.
type mergeHeap []*heapItem

func (h mergeHeap) Len() int      { return len(h) }
func (h mergeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h mergeHeap) Less(i, j int) bool {
	cmp := binary.CompareInternalKey(h[i].key, h[j].key)
	if cmp != 0 {
		return cmp < 0
	}
	// Deterministic, address-independent tie-breaker: lower input slice index wins.
	return h[i].index < h[j].index
}

func (h *mergeHeap) Push(x any) {
	if item, ok := x.(*heapItem); ok {
		*h = append(*h, item)
	}
}

func (h *mergeHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil // avoid pointer retention in backing array
	*h = old[:n-1]
	return item
}

// MergingIterator combines multiple pre-sorted child iterators into a single deterministically
// ordered logical stream using a standard-library min-heap (container/heap).
//
// Invariants & Operational Semantics:
//  1. Ordering: Records are emitted in strictly increasing canonical order governed by
//     binary.CompareInternalKey:
//     - UserKey ascending (lexicographical bytes.Compare)
//     - SeqNum descending (newer revisions precede older revisions)
//     - OpType descending (DELETE/tombstone precedes PUT for identical SeqNum)
//  2. Tie-Breaking: When two child iterators present identical canonical InternalKeys,
//     the record from the lower original child index is emitted first, guaranteeing
//     absolute determinism independent of pointer addresses, map order, or goroutine timing.
//  3. Deduplication: In default mode (NewMergingIterator), for each unique UserKey, only the
//     newest record revision (highest SeqNum) is emitted. Older revisions of the same UserKey
//     are suppressed/discarded. In raw mode (NewRawMergingIterator), all revisions are yielded.
//  4. Tombstone Transparency: Tombstones (OpTypeDelete) are emitted verbatim as standard records
//     with Value() == nil when they represent the newest revision under the merge contract.
//     Tombstone dropping decisions belong exclusively to subsequent compaction safety stages (P08-S02-M02).
//  5. Error Propagation: If any child iterator reports an error during advancement or decoding,
//     the merge iterator transitions to mergeStateFailed, aborts iteration, and surfaces the
//     underlying corruption error via Err(). Corruption is never masked as clean EOF.
//  6. Memory Boundedness: Retains at most one current record per live child iterator in memory
//     (O(N) heap footprint for N child iterators). Records are streamed on demand.
//  7. Key & Value Ownership: Key(), RawKey(), and Value() return independent defensive copies
//     guaranteeing that callers and downstream consumers retain valid byte slices across
//     subsequent Next() advancements or Close().
//  8. Child Lifecycle & Ownership: Close() closes all child iterators implementing io.Closer
//     or Close() error and is strictly idempotent.
type MergingIterator struct {
	mu sync.RWMutex

	h        mergeHeap
	children []Iterator

	raw bool // if true, suppresses user-key deduplication (yields all revisions)

	state mergeState
	err   error

	// Emitted record state
	currKey      binary.InternalKey
	currKeyBytes []byte
	currValue    []byte

	// Deduplication tracking
	hasEmitted         bool
	lastEmittedUserKey []byte

	// Child iterator tracking and monotonic progression invariants (P08-SEC-019)
	childPrevKeys    []binary.InternalKey
	childRepeatCount []int

	// Child iterator whose record is currently being emitted and must be advanced on next Next()
	lastEmittedIter  Iterator
	lastEmittedIndex int
}

// NewMergingIterator constructs a k-way merge iterator over the provided child iterators.
// In accordance with the Phase 08 compaction contract, it yields the newest revision per UserKey
// and suppresses duplicate older revisions.
//
// Nil children and exhausted children (Valid() == false with nil Err()) are ignored.
// If any child is already in an error state (Err() != nil), the merge iterator initializes
// directly into a failed state and surfaces that error.
func NewMergingIterator(iters []Iterator) *MergingIterator {
	return newMergingIteratorInternal(iters, false)
}

// NewRawMergingIterator constructs a k-way merge iterator that yields every record in canonical
// InternalKey order without suppressing older revisions of the same UserKey.
// This is used for multi-version inspection, raw streaming, and differential testing.
func NewRawMergingIterator(iters []Iterator) *MergingIterator {
	return newMergingIteratorInternal(iters, true)
}

func newMergingIteratorInternal(iters []Iterator, raw bool) *MergingIterator {
	if len(iters) > MaxMergingIterators {
		return &MergingIterator{
			raw:   raw,
			state: mergeStateFailed,
			err:   fmt.Errorf("compaction: iterator count %d exceeds maximum limit of %d", len(iters), MaxMergingIterators),
		}
	}

	it := &MergingIterator{
		raw:                raw,
		state:              mergeStateUninitialized,
		children:           make([]Iterator, 0, len(iters)),
		childPrevKeys:      make([]binary.InternalKey, len(iters)),
		childRepeatCount:   make([]int, len(iters)),
		lastEmittedUserKey: make([]byte, 0, 64),
	}

	for i, child := range iters {
		if child == nil {
			continue
		}
		it.children = append(it.children, child)

		// Check if child is already in a failed state
		if er, ok := child.(interface{ Err() error }); ok {
			if err := er.Err(); err != nil {
				it.state = mergeStateFailed
				it.err = err
				it.h = nil
				return it
			}
		}

		// Child must be currently positioned at a valid record to enter the initial heap.
		// Exhausted or unpositioned child iterators are skipped.
		if !child.Valid() {
			continue
		}

		k := child.Key()
		it.childPrevKeys[i] = k.Clone()

		item := &heapItem{
			iter:  child,
			key:   k,
			value: child.Value(),
			index: i,
		}
		it.h = append(it.h, item)
	}

	heap.Init(&it.h)
	return it
}

// Valid reports whether the iterator is currently positioned at a valid record.
// Returns false if uninitialized, exhausted (EOF), failed, or closed.
func (it *MergingIterator) Valid() bool {
	if it == nil {
		return false
	}
	it.mu.RLock()
	defer it.mu.RUnlock()
	return it.state == mergeStateValid
}

// Next advances the merge iterator to the next winning record.
//
// Semantics:
//  1. If raw == false: suppresses duplicate older revisions of already-emitted UserKeys.
//  2. If raw == true: yields all records in strictly sorted canonical InternalKey order.
//  3. If a child encounters an error, transitions to mergeStateFailed and returns false.
//  4. Upon clean exhaustion of all children, transitions to mergeStateExhausted and returns false.
func (it *MergingIterator) Next() bool {
	if it == nil {
		return false
	}
	it.mu.Lock()
	defer it.mu.Unlock()

	if it.state == mergeStateClosed || it.state == mergeStateFailed || it.state == mergeStateExhausted {
		return false
	}

	// Step 1: If an iterator was previously emitted, advance it now before selecting the next record.
	if it.lastEmittedIter != nil {
		prevIter := it.lastEmittedIter
		prevIndex := it.lastEmittedIndex
		it.lastEmittedIter = nil

		if !it.advanceAndPush(prevIter, prevIndex) {
			if it.state == mergeStateFailed {
				it.clearEmittedRecord()
				return false
			}
		}
	}

	dedupLoops := 0
	// Step 2: Pop candidates from the min-heap until finding a record to emit or exhausting the heap.
	for len(it.h) > 0 {
		popped := heap.Pop(&it.h)
		item, ok := popped.(*heapItem)
		if !ok || item == nil {
			continue
		}

		// Check for older revision deduplication
		if !it.raw && it.hasEmitted && bytes.Equal(item.key.UserKey, it.lastEmittedUserKey) {
			// Loop defense (P08-SEC-019): Bounded suppression count per Next() invocation.
			dedupLoops++
			if dedupLoops > 1000000 {
				it.state = mergeStateFailed
				it.err = fmt.Errorf("compaction: excessive deduplication iterations (%d) for user key %q (loop detected)", dedupLoops, it.lastEmittedUserKey)
				it.clearEmittedRecord()
				return false
			}

			// This record has the same UserKey as an already emitted record.
			// Because of canonical ordering (SeqNum descending), this item is strictly an older
			// revision and must be suppressed. Advance this child and push back to heap if valid.
			if !it.advanceAndPush(item.iter, item.index) {
				if it.state == mergeStateFailed {
					it.clearEmittedRecord()
					return false
				}
			}
			continue
		}

		// Found the winning next record!
		it.currKey = item.key.Clone()
		it.currKeyBytes = binary.EncodeInternalKey(it.currKey)
		if item.value != nil {
			it.currValue = make([]byte, len(item.value))
			copy(it.currValue, item.value)
		} else {
			it.currValue = nil
		}

		it.hasEmitted = true
		it.lastEmittedUserKey = append(it.lastEmittedUserKey[:0], item.key.UserKey...)
		it.lastEmittedIter = item.iter
		it.lastEmittedIndex = item.index
		it.state = mergeStateValid
		return true
	}

	// All child iterators are exhausted and no candidates remain in the heap.
	it.state = mergeStateExhausted
	it.clearEmittedRecord()
	return false
}

// advanceAndPush advances child iter, and if still valid, pushes its new record back onto the heap.
// If child encounters an error, transitions it to mergeStateFailed and returns false.
// If child cleanly exhausts, returns true without pushing to the heap.
func (it *MergingIterator) advanceAndPush(child Iterator, index int) bool {
	if child.Next() {
		currKey := child.Key()

		// Monotonic progression invariant (P08-SEC-019):
		// Each child iterator must yield records in canonical InternalKey order.
		// If a child iterator yields a key strictly smaller than its previous key (going backwards),
		// or repeats the exact same internal key excessively, it is corrupted or hostile;
		// fail closed to prevent infinite loops in deduplication.
		if index >= 0 && index < len(it.childPrevKeys) && it.childPrevKeys[index].UserKey != nil {
			cmp := binary.CompareInternalKey(it.childPrevKeys[index], currKey)
			if cmp > 0 {
				it.state = mergeStateFailed
				it.err = fmt.Errorf("compaction: child iterator %d yielded keys out of canonical order: prev > curr", index)
				return false
			}
			if cmp == 0 {
				it.childRepeatCount[index]++
				if it.childRepeatCount[index] > 8 {
					it.state = mergeStateFailed
					it.err = fmt.Errorf("compaction: child iterator %d repeated identical key %d times (loop detected)", index, it.childRepeatCount[index])
					return false
				}
			} else {
				it.childRepeatCount[index] = 0
			}
		}
		if index >= 0 && index < len(it.childPrevKeys) {
			it.childPrevKeys[index] = currKey.Clone()
		}

		item := &heapItem{
			iter:  child,
			key:   currKey,
			value: child.Value(),
			index: index,
		}
		heap.Push(&it.h, item)
		return true
	}

	// Child.Next() returned false: check whether it was clean EOF or corruption.
	if er, ok := child.(interface{ Err() error }); ok {
		if err := er.Err(); err != nil {
			it.state = mergeStateFailed
			it.err = err
			return false
		}
	}

	return true
}

func (it *MergingIterator) clearEmittedRecord() {
	it.currKey = binary.InternalKey{}
	it.currKeyBytes = nil
	it.currValue = nil
	it.lastEmittedIter = nil
}

// Key returns a defensive deep copy of the InternalKey at the current merged position.
// Returns an empty InternalKey if the iterator is not Valid().
func (it *MergingIterator) Key() binary.InternalKey {
	if it == nil {
		return binary.InternalKey{}
	}
	it.mu.RLock()
	defer it.mu.RUnlock()
	if it.state != mergeStateValid {
		return binary.InternalKey{}
	}
	return it.currKey.Clone()
}

// RawKey returns an independent copy of the encoded canonical internal key bytes at the
// current position. Returns nil if the iterator is not Valid().
func (it *MergingIterator) RawKey() []byte {
	if it == nil {
		return nil
	}
	it.mu.RLock()
	defer it.mu.RUnlock()
	if it.state != mergeStateValid || it.currKeyBytes == nil {
		return nil
	}
	out := make([]byte, len(it.currKeyBytes))
	copy(out, it.currKeyBytes)
	return out
}

// Value returns a defensive copy of the value byte slice at the current merged position.
// Returns nil if the iterator is not Valid() or if the record is a tombstone (OpTypeDelete).
func (it *MergingIterator) Value() []byte {
	if it == nil {
		return nil
	}
	it.mu.RLock()
	defer it.mu.RUnlock()
	if it.state != mergeStateValid || it.currValue == nil {
		return nil
	}
	out := make([]byte, len(it.currValue))
	copy(out, it.currValue)
	return out
}

// Err returns the error encountered during iteration, if any.
// Returns nil if iteration completed cleanly (exhausted / EOF).
func (it *MergingIterator) Err() error {
	if it == nil {
		return errors.ErrNilReceiver
	}
	it.mu.RLock()
	defer it.mu.RUnlock()
	return it.err
}

// Close invalidates the MergingIterator and closes all child iterators implementing io.Closer
// or Close() error.
// Calling Close multiple times is safe and strictly idempotent.
func (it *MergingIterator) Close() error {
	if it == nil {
		return errors.ErrNilReceiver
	}
	it.mu.Lock()
	defer it.mu.Unlock()

	if it.state == mergeStateClosed {
		return nil
	}

	it.state = mergeStateClosed
	it.h = nil
	it.clearEmittedRecord()
	it.lastEmittedUserKey = nil
	it.childPrevKeys = nil

	var closeErrs []error
	for _, child := range it.children {
		if child == nil {
			continue
		}
		if c, ok := child.(io.Closer); ok {
			if err := c.Close(); err != nil {
				closeErrs = append(closeErrs, err)
			}
		} else if cErr, ok := child.(interface{ Close() error }); ok {
			if err := cErr.Close(); err != nil {
				closeErrs = append(closeErrs, err)
			}
		} else if cVoid, ok := child.(interface{ Close() }); ok {
			cVoid.Close()
		}
	}
	it.children = nil

	if len(closeErrs) > 0 {
		return stdErrors.Join(closeErrs...)
	}
	return nil
}
