package compaction

import (
	"bytes"
	"sort"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
)

// FuzzMergingIterator fuzzes the k-way merge iterator with pseudo-random byte inputs
// validating memory safety, strictly increasing order, determinism, and absence of panics.
func FuzzMergingIterator(f *testing.F) {
	f.Add([]byte{
		1, 2, 'a', 'b', 10, 0, 0, 0, 0, 0, 0, 0, 1, 3, 'v', 'a', 'l',
		1, 1, 'c', 20, 0, 0, 0, 0, 0, 0, 0, 2, 0,
	})
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0})
	f.Add([]byte{2, 1, 'x', 1, 0, 0, 0, 0, 0, 0, 0, 1, 1, 'y'})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 {
			it := NewMergingIterator(nil)
			if it.Valid() || it.Next() {
				t.Fatalf("empty merge must be invalid")
			}
			_ = it.Close()
			return
		}

		// Parse data into 1 to 4 child iterators
		numChildren := int(data[0]%4) + 1
		offset := 1

		childrenData := make([][]testRecord, numChildren)

		for offset < len(data) {
			childIdx := int(data[offset]) % numChildren
			offset++
			if offset >= len(data) {
				break
			}

			keyLen := int(data[offset]%8) + 1
			offset++
			if offset+keyLen > len(data) {
				break
			}
			userKey := make([]byte, keyLen)
			copy(userKey, data[offset:offset+keyLen])
			offset += keyLen

			var seq uint64 = 1
			if offset < len(data) {
				seq = uint64(data[offset]) + 1
				offset++
			}

			op := binary.OpTypePut
			var val []byte
			if offset < len(data) {
				if data[offset]%3 == 0 {
					op = binary.OpTypeDelete
					val = nil
				} else {
					val = []byte("fuzz-val")
				}
				offset++
			}

			childrenData[childIdx] = append(childrenData[childIdx], testRecord{
				key: binary.InternalKey{
					UserKey: userKey,
					SeqNum:  binary.SeqNum(seq),
					OpType:  op,
				},
				value: val,
			})
		}

		// Sort each child stream canonically
		childIters := make([]Iterator, numChildren)
		childItersRun2 := make([]Iterator, numChildren)

		for c := 0; c < numChildren; c++ {
			sort.Slice(childrenData[c], func(i, j int) bool {
				return binary.CompareInternalKey(childrenData[c][i].key, childrenData[c][j].key) < 0
			})

			// Clone for determinism test
			dup := make([]testRecord, len(childrenData[c]))
			for i, r := range childrenData[c] {
				dup[i] = testRecord{
					key:   r.key.Clone(),
					value: append([]byte(nil), r.value...),
				}
			}
			childIters[c] = newMockIterator(childrenData[c])
			childItersRun2[c] = newMockIterator(dup)
		}

		// Run 1: MergingIterator (deduplicating mode)
		it := NewMergingIterator(childIters)
		var emittedKeys [][]byte
		var emittedRecords []testRecord

		for it.Next() {
			if !it.Valid() {
				t.Fatalf("Valid() must be true after Next() returns true")
			}
			k := it.Key()
			v := it.Value()
			raw := it.RawKey()

			if len(k.UserKey) == 0 {
				t.Fatalf("emitted user key must not be empty")
			}
			if raw == nil {
				t.Fatalf("RawKey() must not be nil")
			}

			// In deduplicated mode: UserKey must be strictly increasing
			if len(emittedKeys) > 0 {
				prev := emittedKeys[len(emittedKeys)-1]
				if bytes.Compare(k.UserKey, prev) <= 0 {
					t.Fatalf("deduplicated merge violation: user key not strictly increasing (prev=%s, curr=%s)",
						prev, k.UserKey)
				}
			}

			emittedKeys = append(emittedKeys, k.UserKey)
			emittedRecords = append(emittedRecords, testRecord{key: k, value: v})
		}

		if err := it.Err(); err != nil {
			t.Fatalf("unexpected Err(): %v", err)
		}
		if err := it.Close(); err != nil {
			t.Fatalf("Close() failed: %v", err)
		}

		// Run 2: Test determinism - must emit identical records
		it2 := NewMergingIterator(childItersRun2)
		var run2Records []testRecord
		for it2.Next() {
			run2Records = append(run2Records, testRecord{
				key:   it2.Key(),
				value: it2.Value(),
			})
		}
		_ = it2.Close()

		if len(emittedRecords) != len(run2Records) {
			t.Fatalf("determinism violation: run1 count=%d != run2 count=%d",
				len(emittedRecords), len(run2Records))
		}
		for i := range emittedRecords {
			if binary.CompareInternalKey(emittedRecords[i].key, run2Records[i].key) != 0 {
				t.Fatalf("determinism key mismatch at record %d", i)
			}
			if !bytes.Equal(emittedRecords[i].value, run2Records[i].value) {
				t.Fatalf("determinism value mismatch at record %d", i)
			}
		}
	})
}
