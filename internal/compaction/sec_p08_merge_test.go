package compaction

import (
	"strings"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/version"
)

// TestP08_SEC_018_MaxMergingIterators verifies that NewMergingIterator rejects
// iterator counts exceeding MaxMergingIterators (10,000) to prevent min-heap resource exhaustion.
func TestP08_SEC_018_MaxMergingIterators(t *testing.T) {
	iters := make([]Iterator, MaxMergingIterators+1)
	for i := range iters {
		iters[i] = newMockIterator(nil)
	}

	it := NewMergingIterator(iters)
	defer func() { _ = it.Close() }()

	if it.Valid() {
		t.Fatalf("expected it.Valid() == false on oversized iterator list")
	}
	if it.Next() {
		t.Fatalf("expected it.Next() == false on oversized iterator list")
	}
	if it.Err() == nil || !strings.Contains(it.Err().Error(), "exceeds maximum limit") {
		t.Fatalf("expected error mentioning exceeds maximum limit, got: %v", it.Err())
	}
}

// hostileLoopIterator returns the exact same key infinitely on Next().
type hostileLoopIterator struct {
	key binary.InternalKey
}

func (h *hostileLoopIterator) Valid() bool {
	return true
}

func (h *hostileLoopIterator) Next() bool {
	return true
}

func (h *hostileLoopIterator) Key() binary.InternalKey {
	return h.key
}

func (h *hostileLoopIterator) RawKey() []byte {
	return binary.EncodeInternalKey(h.key)
}

func (h *hostileLoopIterator) Value() []byte {
	return []byte("infinite-val")
}

func (h *hostileLoopIterator) Err() error {
	return nil
}

func (h *hostileLoopIterator) Close() error {
	return nil
}

// TestP08_SEC_019_HostileLoopProtection verifies that a hostile child iterator
// returning the same key endlessly is detected and aborted with a loop error.
func TestP08_SEC_019_HostileLoopProtection(t *testing.T) {
	k, err := binary.NewInternalKey([]byte("target_key"), 50, binary.OpTypePut)
	if err != nil {
		t.Fatalf("failed to build internal key: %v", err)
	}

	hostile := &hostileLoopIterator{key: k}
	it := NewMergingIterator([]Iterator{hostile})
	defer func() { _ = it.Close() }()

	// First Next() should emit the initial key
	if !it.Next() {
		t.Fatalf("expected initial Next() to succeed")
	}

	// Subsequent Next() calls must detect repetition and fail closed rather than loop forever
	sawFailure := false
	for i := 0; i < 20; i++ {
		if !it.Next() {
			sawFailure = true
			break
		}
	}

	if !sawFailure {
		t.Fatalf("expected iterator to fail closed on hostile infinite-loop child iterator")
	}
	if it.Err() == nil || !strings.Contains(it.Err().Error(), "loop detected") {
		t.Fatalf("expected error mentioning loop detected, got: %v", it.Err())
	}
}

// TestP08_SEC_019_OutOfOrderChildIterator verifies that if a child iterator yields
// records that move backwards in canonical order, MergingIterator fails closed.
func TestP08_SEC_019_OutOfOrderChildIterator(t *testing.T) {
	k1, _ := binary.NewInternalKey([]byte("b"), 100, binary.OpTypePut)
	k2, _ := binary.NewInternalKey([]byte("a"), 50, binary.OpTypePut) // "a" < "b", out of canonical order!

	child := newMockIterator([]testRecord{
		{key: k1, value: []byte("val-b")},
		{key: k2, value: []byte("val-a")},
	})

	it := NewMergingIterator([]Iterator{child})
	defer func() { _ = it.Close() }()

	if !it.Next() {
		t.Fatalf("expected first Next() to succeed")
	}

	// Second Next() must fail closed when child advances to "a" after "b"
	if it.Next() {
		t.Fatalf("expected Next() to fail on out-of-order child key")
	}
	if it.Err() == nil || !strings.Contains(it.Err().Error(), "out of canonical order") {
		t.Fatalf("expected error mentioning out of canonical order, got: %v", it.Err())
	}
}

// TestP08_SEC_002_TombstoneSafetyInvariant verifies that CanDropTombstone NEVER allows
// a tombstone to be dropped if any deeper level contains or may contain the user key.
func TestP08_SEC_002_TombstoneSafetyInvariant(t *testing.T) {
	var emptyLevels [version.NumLevels][]version.FileMetadata
	v := version.NewVersion(emptyLevels)

	c, err := NewCompactor(v)
	if err != nil {
		t.Fatalf("failed to create compactor: %v", err)
	}
	defer func() { _ = c.Close() }()

	// 1. Bottom level invariant: targeting deepest level (version.NumLevels - 1 = 6)
	// MUST be safe to drop because no deeper levels exist.
	if !c.CanDropTombstone([]byte("key1"), version.NumLevels-1) {
		t.Fatalf("expected CanDropTombstone to return true at bottom level (L6)")
	}

	// 2. Empty deeper levels: targeting L1 with L2..L6 empty
	// MUST be safe to drop because deeper levels are provably empty.
	if !c.CanDropTombstone([]byte("key1"), 1) {
		t.Fatalf("expected CanDropTombstone to return true when deeper levels are empty")
	}

	// 3. Deeper level with candidate file overlapping range:
	// Version with file at L2 covering ["apple", "cherry"]
	var levels [version.NumLevels][]version.FileMetadata
	meta := version.FileMetadata{
		FileNum:        200,
		FileSize:       1024,
		SmallestSeqNum: 1,
		LargestSeqNum:  50,
		SmallestKey:    binary.EncodeInternalKey(mustMakeKey("apple", 50, binary.OpTypePut)),
		LargestKey:     binary.EncodeInternalKey(mustMakeKey("cherry", 1, binary.OpTypePut)),
	}
	levels[2] = []version.FileMetadata{meta}
	v2 := version.NewVersion(levels)

	c2, err := NewCompactor(v2)
	if err != nil {
		t.Fatalf("failed to create compactor: %v", err)
	}
	defer func() { _ = c2.Close() }()

	// Key "banana" is within ["apple", "cherry"] at L2.
	// When targeting L1 (so L2 is strictly deeper), CanDropTombstone MUST NOT drop without exact check!
	if c2.CanDropTombstone([]byte("banana"), 1) {
		t.Fatalf("VIOLATION: CanDropTombstone returned true for overlapping key in deeper level!")
	}

	// Key "zebra" is strictly outside ["apple", "cherry"] at L2.
	// Targeting L1, "zebra" cannot exist in L2, so it IS safe to drop!
	if !c2.CanDropTombstone([]byte("zebra"), 1) {
		t.Fatalf("expected CanDropTombstone to return true for key strictly outside all deeper file ranges")
	}
}

func mustMakeKey(uKey string, seq uint64, op binary.OpType) binary.InternalKey {
	k, err := binary.NewInternalKey([]byte(uKey), binary.SeqNum(seq), op)
	if err != nil {
		panic(err)
	}
	return k
}
