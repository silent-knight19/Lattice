package memtable_test

import (
	stdErrors "errors"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
)

func FuzzSkipListNodeConstruction(f *testing.F) {
	// Seed corpus with representative combinations
	f.Add(1, []byte("key1"), uint64(1), byte(binary.OpTypePut), []byte("val1"))
	f.Add(16, []byte("key2"), uint64(100), byte(binary.OpTypeDelete), []byte(nil))
	f.Add(0, []byte("zero-height"), uint64(5), byte(binary.OpTypePut), []byte("v"))
	f.Add(17, []byte("over-max"), uint64(10), byte(binary.OpTypePut), []byte("v"))
	f.Add(-5, []byte("negative"), uint64(20), byte(binary.OpTypePut), []byte("v"))
	f.Add(1000000, []byte("bomb"), uint64(30), byte(binary.OpTypePut), []byte("v"))
	f.Add(8, []byte(""), uint64(40), byte(binary.OpTypePut), []byte("empty-key"))

	f.Fuzz(func(t *testing.T, requestedHeight int, userKey []byte, seq uint64, opByte byte, val []byte) {
		key := binary.InternalKey{
			UserKey: userKey,
			SeqNum:  binary.SeqNum(seq),
			OpType:  binary.OpType(opByte),
		}

		node, err := memtable.NewSkipListNodeForTesting(key, val, requestedHeight)

		// 1. Invariant: Heights outside [1, 16] must always be rejected
		if requestedHeight < memtable.MinHeight || requestedHeight > memtable.MaxHeight {
			if err == nil || node != nil {
				t.Fatalf("fuzz: allowed invalid height %d", requestedHeight)
			}
			if !stdErrors.Is(err, errors.ErrInvalidSkipListHeight) {
				t.Fatalf("fuzz: height %d expected ErrInvalidSkipListHeight, got %v", requestedHeight, err)
			}
			return
		}

		// 2. Invariant: Valid heights [1, 16]
		keyValid := binary.ValidateKey(userKey) == nil
		opValid := key.OpType.Validate() == nil
		valValid := binary.ValidateValue(val) == nil

		if !keyValid || !opValid || !valValid {
			if err == nil || node != nil {
				t.Fatalf("fuzz: construction succeeded with invalid input (keyValid=%v, opValid=%v, valValid=%v)",
					keyValid, opValid, valValid)
			}
			return
		}

		// Valid node
		if err != nil || node == nil {
			t.Fatalf("fuzz: unexpected construction failure for valid inputs: %v", err)
		}

		if node.HeightForTesting() != requestedHeight {
			t.Fatalf("fuzz: height mismatch: got %d, want %d", node.HeightForTesting(), requestedHeight)
		}

		// Verify pointer levels
		for lvl := 0; lvl < requestedHeight; lvl++ {
			ptr, err := node.ForwardAtForTesting(lvl)
			if err != nil || ptr != nil {
				t.Fatalf("fuzz: level %d initial pointer invalid (err=%v, ptr=%v)", lvl, err, ptr)
			}
		}

		// Verify out of bounds access returns error
		if _, err := node.ForwardAtForTesting(requestedHeight); !stdErrors.Is(err, errors.ErrInvalidSkipListLevel) {
			t.Fatalf("fuzz: level %d expected ErrInvalidSkipListLevel, got %v", requestedHeight, err)
		}
	})
}
