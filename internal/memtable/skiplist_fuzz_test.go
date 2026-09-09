package memtable_test

import (
	stdErrors "errors"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
)

func FuzzSkipList_SequentialOperations(f *testing.F) {
	// Seed corpus with various sequences of operations
	f.Add([]byte{0x01, 0x03, 'k', 'e', 'y', 0x04, 'd', 'a', 't', 'a'})
	f.Add([]byte{0x02, 0x03, 'k', 'e', 'y'})
	f.Add([]byte{0x01, 0x01, 'a', 0x01, 'b', 0x01, '0', 0x01, 'c'})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2 {
			return
		}

		sl := memtable.NewSkipList()
		cursor := 0
		seq := uint64(1)
		opCount := 0
		const maxOps = 60 // Bound operations per fuzz iteration to prevent unbounded execution

		for cursor < len(data) && opCount < maxOps {
			opCount++
			action := data[cursor] % 3
			cursor++

			if cursor >= len(data) {
				break
			}

			keyLen := int(data[cursor] % 32) // Bounded key length [0, 31]
			cursor++
			if cursor+keyLen > len(data) {
				break
			}
			keyBytes := data[cursor : cursor+keyLen]
			cursor += keyLen

			switch action {
			case 0: // PUT
				valLen := 0
				var valBytes []byte
				if cursor < len(data) {
					valLen = int(data[cursor] % 64) // Bounded value length [0, 63]
					cursor++
					if cursor+valLen <= len(data) {
						valBytes = data[cursor : cursor+valLen]
						cursor += valLen
					}
				}

				key, keyErr := binary.NewInternalKey(keyBytes, binary.SeqNum(seq), binary.OpTypePut)
				seq++
				if keyErr != nil {
					// Failure atomicity: invalid key must return error and not corrupt state
					countBefore := sl.Len()
					err := sl.Insert(key, valBytes)
					if err == nil {
						t.Fatalf("expected error inserting invalid key %q", keyBytes)
					}
					if sl.Len() != countBefore {
						t.Fatalf("list count modified after failed insert")
					}
					continue
				}

				if err := sl.Insert(key, valBytes); err != nil {
					t.Fatalf("unexpected error inserting valid key: %v", err)
				}

			case 1: // DELETE (Tombstone)
				key, keyErr := binary.NewInternalKey(keyBytes, binary.SeqNum(seq), binary.OpTypeDelete)
				seq++
				if keyErr != nil {
					continue
				}
				if err := sl.Insert(key, nil); err != nil {
					t.Fatalf("unexpected error inserting tombstone: %v", err)
				}

			case 2: // SEARCH
				got, err := sl.Search(keyBytes)
				if err != nil && !stdErrors.Is(err, errors.ErrKeyNotFound) {
					// Invalid key error is expected if keyLen == 0
					if len(keyBytes) != 0 {
						t.Fatalf("unexpected search error for key %q: %v", keyBytes, err)
					}
				}
				_ = got
			}
		}

		// Verify structural invariants after the sequence of fuzz mutations
		if err := sl.ValidateStructureForTesting(); err != nil {
			t.Fatalf("SkipList structural invariant violated after fuzz execution: %v", err)
		}
	})
}
