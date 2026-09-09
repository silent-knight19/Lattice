package memtable_test

import (
	"bytes"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/memtable"
)

func FuzzIterator(f *testing.F) {
	// Seed corpus with various sequences of mutations and iterator operations
	f.Add([]byte{0x00, 0x03, 'f', 'o', 'o', 0x03, 'b', 'a', 'r', 0x02, 0x01, 0x04, 'b', 'a', 'z'})
	f.Add([]byte{0x00, 0x01, 'a', 0x01, '1', 0x00, 0x01, 'b', 0x01, '2', 0x01, 0x01, 'a', 0x02, 0x03, 0x01, 'a'})
	f.Add([]byte{0x02, 0x03, 0x04, 0x05, 0x00, 0x02, 0xff, 0x00, 0x01, 0x01})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2 {
			return
		}

		sl := memtable.NewSkipList()
		cursor := 0
		seq := uint64(1)
		opCount := 0
		const maxOps = 50 // Bound operations per fuzz iteration

		it := sl.NewIterator()

		for cursor < len(data) && opCount < maxOps {
			opCount++
			action := data[cursor] % 6
			cursor++

			switch action {
			case 0: // Insert PUT
				if cursor >= len(data) {
					break
				}
				kLen := int(data[cursor] % 16)
				cursor++
				if cursor+kLen > len(data) {
					break
				}
				keyBytes := data[cursor : cursor+kLen]
				cursor += kLen

				vLen := 0
				var valBytes []byte
				if cursor < len(data) {
					vLen = int(data[cursor] % 32)
					cursor++
					if cursor+vLen <= len(data) {
						valBytes = data[cursor : cursor+vLen]
						cursor += vLen
					}
				}

				k, err := binary.NewInternalKey(keyBytes, binary.SeqNum(seq), binary.OpTypePut)
				seq++
				if err == nil {
					_ = sl.Insert(k, valBytes)
				}

			case 1: // Insert DELETE (tombstone)
				if cursor >= len(data) {
					break
				}
				kLen := int(data[cursor] % 16)
				cursor++
				if cursor+kLen > len(data) {
					break
				}
				keyBytes := data[cursor : cursor+kLen]
				cursor += kLen

				k, err := binary.NewInternalKey(keyBytes, binary.SeqNum(seq), binary.OpTypeDelete)
				seq++
				if err == nil {
					_ = sl.Insert(k, nil)
				}

			case 2: // Seek
				if cursor >= len(data) {
					break
				}
				kLen := int(data[cursor] % 16)
				cursor++
				if cursor+kLen > len(data) {
					break
				}
				keyBytes := data[cursor : cursor+kLen]
				cursor += kLen

				seekErr := it.Seek(keyBytes)
				if seekErr == nil && it.Valid() {
					k := it.Key()
					if bytes.Compare(k.UserKey, keyBytes) < 0 {
						t.Fatalf("Seek(%q) landed on key %q which is smaller than target", keyBytes, k.UserKey)
					}
				}

			case 3: // SeekToFirst
				it.SeekToFirst()
				if it.Valid() {
					_ = it.Key()
					_ = it.Value()
				}

			case 4: // Next
				_ = it.Next()
				if it.Valid() {
					k := it.Key()
					v := it.Value()
					// Mutate defensive copies to verify memory isolation
					if len(k.UserKey) > 0 {
						k.UserKey[0] ^= 0xff
					}
					if len(v) > 0 {
						v[0] ^= 0xff
					}
				} else {
					// Safe calls on invalid iterator
					_ = it.Key()
					_ = it.Value()
				}

			case 5: // Close and reopen
				it.Close()
				if it.Valid() {
					t.Fatalf("iterator Valid() must be false after Close()")
				}
				if it.Next() {
					t.Fatalf("iterator Next() must return false after Close()")
				}
				it = sl.NewIterator()
			}
		}

		// Invariant: Full traversal of the SkipList terminates in at most (total nodes + 1) steps
		// and maintains non-decreasing InternalKey order.
		fullScanIt := sl.NewIterator()
		fullScanIt.SeekToFirst()

		var prevKey binary.InternalKey
		hasPrev := false
		steps := 0
		maxSteps := opCount + 10

		for fullScanIt.Valid() {
			steps++
			if steps > maxSteps {
				t.Fatalf("potential infinite loop detected in iterator traversal: steps=%d exceeded maxSteps=%d", steps, maxSteps)
			}

			currKey := fullScanIt.Key()
			if hasPrev {
				if binary.CompareInternalKey(prevKey, currKey) >= 0 {
					t.Fatalf("invariant violation: keys not strictly increasing: prev=%v curr=%v", prevKey, currKey)
				}
			}
			prevKey = currKey
			hasPrev = true

			fullScanIt.Next()
		}

		if fullScanIt.Valid() {
			t.Fatalf("iterator must not be valid after full traversal loop")
		}
		// Subsequent Next calls must be safe and return false
		if fullScanIt.Next() {
			t.Fatalf("Next() on exhausted iterator must return false")
		}
		if k := fullScanIt.Key(); len(k.UserKey) != 0 {
			t.Fatalf("Key() on exhausted iterator must be empty, got: %v", k)
		}
		if v := fullScanIt.Value(); v != nil {
			t.Fatalf("Value() on exhausted iterator must be nil, got: %v", v)
		}
	})
}
