package memtable_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
)

func TestSkipList_Scale_10000KeysWithVersionsAndOracle(t *testing.T) {
	// Deterministic pseudo-random number generator for reproducible execution
	rng := memtable.NewPCG32(0x12345678, 0x9ABCDEF0)
	sl := memtable.NewSkipList()

	const (
		totalInserts = 10000
		keySpace     = 3000 // Deliberate duplicate UserKeys to exercise multi-versioning
	)

	type oracleRecord struct {
		seqNum binary.SeqNum
		opType binary.OpType
		value  []byte
	}

	oracle := make(map[string]oracleRecord, keySpace)

	// Step 1: Insert 10,000 keys with monotonically increasing sequence numbers
	for i := 1; i <= totalInserts; i++ {
		keyID := rng.Uint32() % keySpace
		userKey := []byte(fmt.Sprintf("user:%06d", keyID))
		seqNum := binary.SeqNum(i)

		// 10% of insertions are deletes/tombstones to exercise versioned deletion
		op := binary.OpTypePut
		var val []byte
		if i%10 == 0 {
			op = binary.OpTypeDelete
			val = nil
		} else {
			val = []byte(fmt.Sprintf("payload-v%d-for-key-%06d", i, keyID))
		}

		key, err := binary.NewInternalKey(userKey, seqNum, op)
		if err != nil {
			t.Fatalf("failed creating InternalKey at step %d: %v", i, err)
		}

		if err := sl.Insert(key, val); err != nil {
			t.Fatalf("insert failed at step %d: %v", i, err)
		}

		// Update oracle: monotonic sequence number means the current insertion
		// is unconditionally the newest version for this UserKey.
		oracle[string(userKey)] = oracleRecord{
			seqNum: seqNum,
			opType: op,
			value:  val,
		}
	}

	// Verify total entry count (all 10,000 multi-version records preserved)
	if sl.Len() != totalInserts {
		t.Fatalf("expected Len() == %d, got %d", totalInserts, sl.Len())
	}

	// Step 2: Validate search correctness for every single key in the key space against oracle
	for keyStr, expected := range oracle {
		uKey := []byte(keyStr)
		gotVal, err := sl.Search(uKey)

		if expected.opType == binary.OpTypeDelete {
			if gotVal != nil {
				t.Errorf("key %s was deleted at seq %d, but Search returned value %q",
					keyStr, expected.seqNum, gotVal)
			}
			if !stdErrors.Is(err, errors.ErrKeyNotFound) {
				t.Errorf("key %s was deleted at seq %d, expected ErrKeyNotFound, got %v",
					keyStr, expected.seqNum, err)
			}
		} else {
			if err != nil {
				t.Errorf("key %s search failed: %v", keyStr, err)
			}
			if !bytes.Equal(gotVal, expected.value) {
				t.Errorf("key %s value mismatch: got %q, want %q", keyStr, gotVal, expected.value)
			}
		}
	}

	// Step 3: Search for 1,000 nonexistent keys (guaranteed absent)
	for i := 0; i < 1000; i++ {
		nonexistentKey := []byte(fmt.Sprintf("absent:%06d", i))
		got, err := sl.Search(nonexistentKey)
		if got != nil || !stdErrors.Is(err, errors.ErrKeyNotFound) {
			t.Errorf("search for nonexistent key %s returned val=%v, err=%v", nonexistentKey, got, err)
		}
	}

	// Step 4: Validate structural invariants across all SkipList levels
	if err := sl.ValidateStructureForTesting(); err != nil {
		t.Fatalf("structural invariant validation failed after 10,000 inserts: %v", err)
	}

	// Step 5: Verify height bounds
	if sl.Height() < memtable.MinHeight || sl.Height() > memtable.MaxHeight {
		t.Errorf("active height %d out of bounds [%d, %d]", sl.Height(), memtable.MinHeight, memtable.MaxHeight)
	}
}
