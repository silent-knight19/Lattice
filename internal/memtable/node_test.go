package memtable_test

import (
	stdErrors "errors"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
)

func sampleKey(t *testing.T, userKey string, seq uint64, op binary.OpType) binary.InternalKey {
	t.Helper()
	k, err := binary.NewInternalKey([]byte(userKey), binary.SeqNum(seq), op)
	if err != nil {
		t.Fatalf("failed to create sample key: %v", err)
	}
	return k
}

func TestSkipListNode_ConstructionValidHeights(t *testing.T) {
	key := sampleKey(t, "user:1001", 42, binary.OpTypePut)
	val := []byte("payload-data")

	testHeights := []int{
		memtable.MinHeight, // 1
		2,
		8,
		memtable.MaxHeight - 1, // 15
		memtable.MaxHeight,     // 16
	}

	for _, h := range testHeights {
		node, err := memtable.NewSkipListNodeForTesting(key, val, h)
		if err != nil {
			t.Fatalf("expected valid construction for height %d, got err: %v", h, err)
		}
		if node == nil {
			t.Fatalf("expected non-nil node for height %d", h)
		}
		if node.HeightForTesting() != h {
			t.Errorf("height mismatch: got %d, want %d", node.HeightForTesting(), h)
		}

		// Verify initial forward pointers are all nil
		for lvl := 0; lvl < h; lvl++ {
			ptr, err := node.ForwardAtForTesting(lvl)
			if err != nil {
				t.Fatalf("unexpected error accessing level %d for height %d: %v", lvl, h, err)
			}
			if ptr != nil {
				t.Errorf("expected initial forward pointer at level %d to be nil, got %v", lvl, ptr)
			}
		}

		// Verify key and value integrity
		if !node.KeyForTesting().Equal(key) {
			t.Errorf("stored key mismatch: got %v, want %v", node.KeyForTesting(), key)
		}
		if string(node.ValueForTesting()) != string(val) {
			t.Errorf("stored value mismatch: got %q, want %q", string(node.ValueForTesting()), string(val))
		}
	}
}

func TestSkipListNode_ConstructionInvalidHeights(t *testing.T) {
	key := sampleKey(t, "user:1001", 1, binary.OpTypePut)
	val := []byte("val")

	invalidHeights := []int{
		-100,
		-1,
		0,
		memtable.MaxHeight + 1, // 17
		50,
		1000,
		1000000,
	}

	for _, h := range invalidHeights {
		node, err := memtable.NewSkipListNodeForTesting(key, val, h)
		if err == nil {
			t.Fatalf("expected error for invalid height %d, got nil node: %v", h, node)
		}
		if node != nil {
			t.Fatalf("expected nil node on error for height %d", h)
		}
		if !stdErrors.Is(err, errors.ErrInvalidSkipListHeight) {
			t.Errorf("expected ErrInvalidSkipListHeight for height %d, got: %v", h, err)
		}

		var typedErr *errors.InvalidSkipListHeightError
		if !stdErrors.As(err, &typedErr) {
			t.Errorf("expected *errors.InvalidSkipListHeightError, got: %T", err)
		} else {
			if typedErr.Height != h {
				t.Errorf("typed error height mismatch: got %d, want %d", typedErr.Height, h)
			}
			if typedErr.MinHeight != memtable.MinHeight || typedErr.MaxHeight != memtable.MaxHeight {
				t.Errorf("typed error bounds mismatch: got [%d, %d], want [%d, %d]",
					typedErr.MinHeight, typedErr.MaxHeight, memtable.MinHeight, memtable.MaxHeight)
			}
		}
	}
}

func TestSkipListNode_SentinelNodeConstruction(t *testing.T) {
	// Valid sentinel node
	sentinel, err := memtable.NewSentinelNodeForTesting(memtable.MaxHeight)
	if err != nil {
		t.Fatalf("failed to create valid sentinel: %v", err)
	}
	if sentinel.HeightForTesting() != memtable.MaxHeight {
		t.Errorf("sentinel height mismatch: got %d, want %d", sentinel.HeightForTesting(), memtable.MaxHeight)
	}

	// Invalid sentinel heights
	for _, h := range []int{0, -1, 17, 100} {
		s, err := memtable.NewSentinelNodeForTesting(h)
		if err == nil || s != nil {
			t.Errorf("expected error for invalid sentinel height %d", h)
		}
		if !stdErrors.Is(err, errors.ErrInvalidSkipListHeight) {
			t.Errorf("expected ErrInvalidSkipListHeight for sentinel height %d, got %v", h, err)
		}
	}
}

func TestSkipListNode_ForwardPointerSplicingAndBounds(t *testing.T) {
	keyA := sampleKey(t, "keyA", 10, binary.OpTypePut)
	keyB := sampleKey(t, "keyB", 10, binary.OpTypePut)

	nodeA, err := memtable.NewSkipListNodeForTesting(keyA, []byte("valA"), 4)
	if err != nil {
		t.Fatalf("failed to construct nodeA: %v", err)
	}
	nodeB, err := memtable.NewSkipListNodeForTesting(keyB, []byte("valB"), 2)
	if err != nil {
		t.Fatalf("failed to construct nodeB: %v", err)
	}

	// Splice nodeA level 0 and level 1 to nodeB
	if err := nodeA.SetForwardForTesting(0, nodeB); err != nil {
		t.Fatalf("unexpected error setting forward level 0: %v", err)
	}
	if err := nodeA.SetForwardForTesting(1, nodeB); err != nil {
		t.Fatalf("unexpected error setting forward level 1: %v", err)
	}

	// Verify links
	ptr0, err := nodeA.ForwardAtForTesting(0)
	if err != nil || ptr0 != nodeB {
		t.Errorf("level 0 pointer mismatch: got %v, want %v (err: %v)", ptr0, nodeB, err)
	}
	ptr1, err := nodeA.ForwardAtForTesting(1)
	if err != nil || ptr1 != nodeB {
		t.Errorf("level 1 pointer mismatch: got %v, want %v (err: %v)", ptr1, nodeB, err)
	}
	ptr2, err := nodeA.ForwardAtForTesting(2)
	if err != nil || ptr2 != nil {
		t.Errorf("level 2 pointer should be nil: got %v (err: %v)", ptr2, err)
	}

	// Out-of-bounds forwardAt checks
	for _, invalidLvl := range []int{-2, -1, 4, 5, 16} {
		ptr, err := nodeA.ForwardAtForTesting(invalidLvl)
		if err == nil || ptr != nil {
			t.Errorf("expected error accessing forward pointer at invalid level %d", invalidLvl)
		}
		if !stdErrors.Is(err, errors.ErrInvalidSkipListLevel) {
			t.Errorf("expected ErrInvalidSkipListLevel for level %d, got %v", invalidLvl, err)
		}
		var typedErr *errors.InvalidSkipListLevelError
		if !stdErrors.As(err, &typedErr) {
			t.Errorf("expected *errors.InvalidSkipListLevelError, got %T", err)
		} else if typedErr.Level != invalidLvl || typedErr.MaxLevel != 3 {
			t.Errorf("typed level error mismatch: got %+v", typedErr)
		}
	}

	// Out-of-bounds setForward checks
	for _, invalidLvl := range []int{-2, -1, 4, 5, 16} {
		err := nodeA.SetForwardForTesting(invalidLvl, nodeB)
		if err == nil {
			t.Errorf("expected error setting forward pointer at invalid level %d", invalidLvl)
		}
		if !stdErrors.Is(err, errors.ErrInvalidSkipListLevel) {
			t.Errorf("expected ErrInvalidSkipListLevel for level %d, got %v", invalidLvl, err)
		}
	}
}

func TestSkipListNode_DefensiveCopying_IngressAliasing(t *testing.T) {
	// SEC-MEM-INV-01: External mutable buffers cannot corrupt stored node state
	rawKeyBytes := []byte("immutable-key")
	rawValBytes := []byte("immutable-val")

	key := binary.InternalKey{
		UserKey: rawKeyBytes,
		SeqNum:  100,
		OpType:  binary.OpTypePut,
	}

	node, err := memtable.NewSkipListNodeForTesting(key, rawValBytes, 4)
	if err != nil {
		t.Fatalf("failed to create node: %v", err)
	}

	// Mutate external buffers
	rawKeyBytes[0] = 'X'
	rawValBytes[0] = 'Y'

	// Verify node stored key was not mutated
	storedKey := node.KeyForTesting()
	if string(storedKey.UserKey) != "immutable-key" {
		t.Errorf("ingress key aliasing detected: node key was mutated to %q", string(storedKey.UserKey))
	}

	// Verify node stored value was not mutated
	storedVal := node.ValueForTesting()
	if string(storedVal) != "immutable-val" {
		t.Errorf("ingress value aliasing detected: node value was mutated to %q", string(storedVal))
	}
}

func TestSkipListNode_DefensiveCopying_EgressAliasing(t *testing.T) {
	// SEC-04 Scenario 3: Returned memory mutation must not alter internal node state
	key := sampleKey(t, "user:2002", 50, binary.OpTypePut)
	val := []byte("original-payload")

	node, err := memtable.NewSkipListNodeForTesting(key, val, 3)
	if err != nil {
		t.Fatalf("failed to create node: %v", err)
	}

	// Retrieve value and mutate it
	returnedVal := node.ValueForTesting()
	returnedVal[0] = 'Z'

	// Second retrieval must still see original bytes
	secondVal := node.ValueForTesting()
	if string(secondVal) != "original-payload" {
		t.Errorf("egress value aliasing detected: internal value was mutated to %q", string(secondVal))
	}
}

func TestSkipListNode_TombstoneNodeWithNilValue(t *testing.T) {
	key := sampleKey(t, "deleted-key", 200, binary.OpTypeDelete)

	node, err := memtable.NewSkipListNodeForTesting(key, nil, 2)
	if err != nil {
		t.Fatalf("failed to create tombstone node with nil value: %v", err)
	}
	if node.ValueForTesting() != nil {
		t.Errorf("expected nil value for tombstone node, got: %v", node.ValueForTesting())
	}
	if node.RawValueForTesting() != nil {
		t.Errorf("expected nil raw value for tombstone node, got: %v", node.RawValueForTesting())
	}
}

func TestSkipListNode_ValidationRejections(t *testing.T) {
	// Empty key rejection
	emptyKey := binary.InternalKey{
		UserKey: []byte{},
		SeqNum:  1,
		OpType:  binary.OpTypePut,
	}
	if _, err := memtable.NewSkipListNodeForTesting(emptyKey, []byte("v"), 1); !stdErrors.Is(err, errors.ErrEmptyKey) {
		t.Errorf("expected ErrEmptyKey for empty key, got: %v", err)
	}

	// Invalid OpType rejection
	badOpKey := binary.InternalKey{
		UserKey: []byte("k"),
		SeqNum:  1,
		OpType:  binary.OpTypeInvalid,
	}
	if _, err := memtable.NewSkipListNodeForTesting(badOpKey, []byte("v"), 1); !stdErrors.Is(err, errors.ErrInvalidOpType) {
		t.Errorf("expected ErrInvalidOpType for invalid op type, got: %v", err)
	}

	// Oversized key rejection
	hugeKey := binary.InternalKey{
		UserKey: make([]byte, binary.MaxKeyLen+1),
		SeqNum:  1,
		OpType:  binary.OpTypePut,
	}
	if _, err := memtable.NewSkipListNodeForTesting(hugeKey, []byte("v"), 1); !stdErrors.Is(err, errors.ErrKeyTooLarge) {
		t.Errorf("expected ErrKeyTooLarge for huge key, got: %v", err)
	}

	// Oversized value rejection
	validKey := sampleKey(t, "k", 1, binary.OpTypePut)
	hugeVal := make([]byte, binary.MaxValueLen+1)
	if _, err := memtable.NewSkipListNodeForTesting(validKey, hugeVal, 1); !stdErrors.Is(err, errors.ErrValueTooLarge) {
		t.Errorf("expected ErrValueTooLarge for huge value, got: %v", err)
	}
}
