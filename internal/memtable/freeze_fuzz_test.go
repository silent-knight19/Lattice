package memtable_test

import (
	stdErrors "errors"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
)

func FuzzSkipList_FreezeLifecycle(f *testing.F) {
	// Seed corpus with mixed mutation and freeze sequences
	f.Add([]byte{0x00, 0x03, 'f', 'o', 'o', 0x01, 0x03, 'b', 'a', 'r', 0x02, 0x00, 0x03, 'b', 'a', 'z'})
	f.Add([]byte{0x02, 0x00, 0x01, 'a', 0x03, 0x01, 'a', 0x04, 0x02})
	f.Add([]byte{0x00, 0x02, 'k', '1', 0x00, 0x02, 'k', '2', 0x02, 0x02, 0x00, 0x02, 'k', '3'})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2 {
			return
		}

		sl := memtable.NewSkipList()
		cursor := 0
		seq := uint64(1)
		opCount := 0
		const maxOps = 60

		frozenAtOp := -1
		var lenAtFreeze int
		var heightAtFreeze int
		var byteSizeAtFreeze uint64

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
					insertErr := sl.Insert(k, valBytes)
					if sl.IsFrozen() {
						if !stdErrors.Is(insertErr, errors.ErrMemTableFrozen) {
							t.Fatalf("expected ErrMemTableFrozen on frozen SkipList, got: %v", insertErr)
						}
					}
				}

			case 1: // Insert DELETE
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
					insertErr := sl.Insert(k, nil)
					if sl.IsFrozen() {
						if !stdErrors.Is(insertErr, errors.ErrMemTableFrozen) {
							t.Fatalf("expected ErrMemTableFrozen on frozen SkipList, got: %v", insertErr)
						}
					}
				}

			case 2: // Freeze
				wasAlreadyFrozen := sl.IsFrozen()
				transitioned := sl.Freeze()
				if wasAlreadyFrozen && transitioned {
					t.Fatalf("redundant Freeze() returned true")
				}
				if !wasAlreadyFrozen && !transitioned {
					t.Fatalf("initial Freeze() returned false")
				}
				if !sl.IsFrozen() {
					t.Fatalf("IsFrozen() returned false after Freeze()")
				}
				if frozenAtOp == -1 {
					frozenAtOp = opCount
					lenAtFreeze = sl.Len()
					heightAtFreeze = sl.Height()
					byteSizeAtFreeze = sl.ByteSize()
				}

			case 3: // SearchConcurrent
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
				_, _ = sl.SearchConcurrent(keyBytes)

			case 4: // Iterator Seek & Next
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
				_ = it.Seek(keyBytes)
				if it.Valid() {
					_ = it.Key()
					_ = it.Value()
				}
				_ = it.Next()

			case 5: // Read accounting
				_ = sl.Len()
				_ = sl.Height()
				_ = sl.ByteSize()
				_ = sl.IsEmpty()
			}

			// Invariant: Once frozen, Len, Height, and ByteSize must remain invariant
			if frozenAtOp != -1 {
				if sl.Len() != lenAtFreeze {
					t.Fatalf("Len mutated after Freeze: before=%d, after=%d", lenAtFreeze, sl.Len())
				}
				if sl.Height() != heightAtFreeze {
					t.Fatalf("Height mutated after Freeze: before=%d, after=%d", heightAtFreeze, sl.Height())
				}
				if sl.ByteSize() != byteSizeAtFreeze {
					t.Fatalf("ByteSize mutated after Freeze: before=%d, after=%d", byteSizeAtFreeze, sl.ByteSize())
				}
			}
		}

		it.Close()
	})
}
