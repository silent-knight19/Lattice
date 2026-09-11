package sstable_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// FuzzBlockHandle_Decode verifies that DecodeBlockHandle never panics on arbitrary byte slices,
// and that any successfully decoded handle satisfies round-trip encoding invariants.
func FuzzBlockHandle_Decode(f *testing.F) {
	// Seed valid handles
	h1 := sstable.BlockHandle{Offset: 0, Size: 4096}
	b1 := h1.Encode()
	f.Add(b1[:])

	h2 := sstable.BlockHandle{Offset: 1048576, Size: 65536}
	b2 := h2.Encode()
	f.Add(b2[:])

	// Seed edge cases
	f.Add([]byte{})
	f.Add(make([]byte, 15)) // truncated
	f.Add(make([]byte, 16)) // all zeros (invalid size=0)
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01})

	f.Fuzz(func(t *testing.T, data []byte) {
		handle, err := sstable.DecodeBlockHandle(data)
		if err != nil {
			return
		}

		// Invariant 1: Any successfully decoded handle must satisfy Validate()
		if err := handle.Validate(); err != nil {
			t.Fatalf("DecodeBlockHandle succeeded on invalid handle %+v: %v", handle, err)
		}

		// Invariant 2: Round-trip encode/decode must be byte-for-byte identical
		reEncoded := handle.Encode()
		decodedAgain, err := sstable.DecodeBlockHandle(reEncoded[:])
		if err != nil {
			t.Fatalf("failed to decode re-encoded handle %+v: %v", handle, err)
		}
		if decodedAgain != handle {
			t.Fatalf("round-trip handle mismatch: got %+v, want %+v", decodedAgain, handle)
		}
	})
}

// FuzzBlockIndex_Decode verifies that DecodeBlockIndex is robust against adversarial byte sequences,
// never panics or crashes, and strictly enforces all internal invariants on accepted payloads.
func FuzzBlockIndex_Decode(f *testing.F) {
	// Seed 1: Empty index block
	builderEmpty := sstable.NewIndexBuilder()
	f.Add(builderEmpty.Finish())

	// Seed 2: 1 entry index block
	b1 := sstable.NewIndexBuilder()
	ik1, _ := binary.NewInternalKey([]byte("seed-key-1"), 10, binary.OpTypePut)
	_ = b1.AddBlock(binary.EncodeInternalKey(ik1), sstable.BlockHandle{Offset: 0, Size: 4096})
	f.Add(b1.Finish())

	// Seed 3: 5 entries index block
	b5 := sstable.NewIndexBuilder()
	for i := 0; i < 5; i++ {
		ik, _ := binary.NewInternalKey([]byte(fmt.Sprintf("key-%02d", i)), binary.SeqNum(i+1), binary.OpTypePut)
		_ = b5.AddBlock(binary.EncodeInternalKey(ik), sstable.BlockHandle{Offset: uint64(i * 4096), Size: 4096})
	}
	f.Add(b5.Finish())

	// Seed edge cases
	f.Add([]byte{})
	f.Add(make([]byte, 7))  // truncated trailer
	f.Add(make([]byte, 8))  // zero trailer
	f.Add(make([]byte, 16)) // dummy trailer

	f.Fuzz(func(t *testing.T, data []byte) {
		idx, err := sstable.DecodeBlockIndex(data)
		if err != nil {
			return
		}

		// If successfully decoded:
		entryCount := idx.EntryCount()
		if entryCount < 0 {
			t.Fatalf("negative entry count: %d", entryCount)
		}
		if idx.IsEmpty() != (entryCount == 0) {
			t.Fatalf("IsEmpty() mismatch: IsEmpty=%v, count=%d", idx.IsEmpty(), entryCount)
		}

		entries := idx.Entries()
		if len(entries) != entryCount {
			t.Fatalf("entries length %d != entryCount %d", len(entries), entryCount)
		}

		// Invariant: Keys must be strictly increasing InternalKeys
		for i := 0; i < entryCount; i++ {
			if len(entries[i].Key.UserKey) == 0 {
				t.Fatalf("decoded entry %d has empty user key", i)
			}
			if err := entries[i].Key.OpType.Validate(); err != nil {
				t.Fatalf("decoded entry %d has invalid op type: %v", i, err)
			}
			if !bytes.Equal(entries[i].UserKey(), entries[i].Key.UserKey) {
				t.Fatalf("decoded entry %d UserKey() mismatch", i)
			}
			if i > 0 {
				if binary.CompareInternalKey(entries[i-1].Key, entries[i].Key) >= 0 {
					t.Fatalf("decoded index contains non-increasing keys: %v >= %v", entries[i-1].Key, entries[i].Key)
				}
			}
		}

		// Invariant: Every handle must be valid
		for i, entry := range entries {
			if err := entry.Handle.Validate(); err != nil {
				t.Fatalf("decoded entry %d has invalid handle %+v: %v", i, entry.Handle, err)
			}
		}

		// Invariant: FindBlock, FindBlockKey, FindBlockInternalKey must execute without panic
		if entryCount > 0 {
			_, _ = idx.FindBlock(entries[0].UserKey())
			_, _ = idx.FindBlockKey(entries[0].Key)
			_, _ = idx.FindBlockInternalKey(entries[0].LargestKey)
			_, _ = idx.FindBlock([]byte("arbitrary-search-key"))
		}
	})
}
