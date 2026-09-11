package sstable_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"sort"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
)

func FuzzBlockBuilder_ValidSequence(f *testing.F) {
	// Seed corpus with various byte patterns
	f.Add([]byte{0x03, 'a', 'b', 'c', 0x02, 'a', 'b', 0x01, 'z'})
	f.Add([]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06})
	f.Add([]byte("user:1001:profile|val1|user:1002:profile|val2"))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 4 {
			return
		}

		// Partition fuzz data into a sequence of user keys
		var rawKeys [][]byte
		cursor := 0
		for cursor < len(data) && len(rawKeys) < 50 {
			kLen := int(data[cursor]%16) + 1 // 1..16 bytes
			cursor++
			if cursor+kLen > len(data) {
				break
			}
			rawKeys = append(rawKeys, data[cursor:cursor+kLen])
			cursor += kLen
		}

		if len(rawKeys) == 0 {
			return
		}

		// Sort keys according to canonical ordering:
		// UserKey ASC, SeqNum DESC, OpType DESC
		type kvItem struct {
			key binary.InternalKey
			val []byte
		}

		var items []kvItem
		for i, kBytes := range rawKeys {
			seqNum := binary.SeqNum(uint64(len(rawKeys)-i) + 1)
			op := binary.OpTypePut
			if i%5 == 0 {
				op = binary.OpTypeDelete
			}
			ik, err := binary.NewInternalKey(kBytes, seqNum, op)
			if err != nil {
				continue
			}
			val := []byte(fmt.Sprintf("v-%d-%d", i, len(kBytes)))
			if op == binary.OpTypeDelete {
				val = nil
			}
			items = append(items, kvItem{key: ik, val: val})
		}

		if len(items) == 0 {
			return
		}

		// Sort items uniquely using CompareInternalKey
		sort.Slice(items, func(i, j int) bool {
			return binary.CompareInternalKey(items[i].key, items[j].key) < 0
		})

		// Deduplicate exact duplicate InternalKeys
		var deduped []kvItem
		for i := 0; i < len(items); i++ {
			if i > 0 && binary.CompareInternalKey(items[i-1].key, items[i].key) == 0 {
				continue
			}
			deduped = append(deduped, items[i])
		}

		// Test across multiple restart intervals
		restartInterval := int(data[0]%16) + 1 // 1..16
		b, err := sstable.NewBlockBuilderWithInterval(restartInterval)
		if err != nil {
			t.Fatalf("failed to create builder: %v", err)
		}

		for _, item := range deduped {
			if err := b.Add(item.key, item.val); err != nil {
				t.Fatalf("Add failed for valid sorted key %v: %v", item.key, err)
			}
		}

		blockBytes := b.Finish()
		offsets := b.RestartOffsets()

		if len(offsets) == 0 {
			t.Fatalf("expected non-empty restart offsets")
		}
		if offsets[0] != 0 {
			t.Fatalf("offset[0] must be 0, got %d", offsets[0])
		}

		// Monotonic restart offsets
		for i := 1; i < len(offsets); i++ {
			if offsets[i] <= offsets[i-1] {
				t.Fatalf("restart offsets must be strictly increasing: %v", offsets)
			}
		}

		// Decode and verify
		entries := parseBlockData(t, blockBytes)
		if len(entries) != len(deduped) {
			t.Fatalf("entry count mismatch: got %d, want %d", len(entries), len(deduped))
		}

		for i, entry := range entries {
			if !entry.key.Equal(deduped[i].key) {
				t.Fatalf("entry %d key mismatch: got %v, want %v", i, entry.key, deduped[i].key)
			}
			if !bytes.Equal(entry.value, deduped[i].val) {
				t.Fatalf("entry %d value mismatch: got %q, want %q", i, entry.value, deduped[i].val)
			}

			// Invariant: at restart boundaries, shared must be 0
			if i%restartInterval == 0 {
				if entry.shared != 0 {
					t.Fatalf("entry %d at restart boundary has shared=%d, expected 0", i, entry.shared)
				}
			}
		}
	})
}

func FuzzBlockBuilder_AdversarialOrdering(f *testing.F) {
	f.Add([]byte("alpha"), []byte("bravo"), uint64(10), uint64(20))
	f.Add([]byte("same"), []byte("same"), uint64(10), uint64(5))
	f.Add([]byte("key-z"), []byte("key-a"), uint64(1), uint64(1))

	f.Fuzz(func(t *testing.T, k1, k2 []byte, seq1, seq2 uint64) {
		if len(k1) == 0 || len(k1) > 1024 || len(k2) == 0 || len(k2) > 1024 {
			return
		}

		ik1, err1 := binary.NewInternalKey(k1, binary.SeqNum(seq1), binary.OpTypePut)
		ik2, err2 := binary.NewInternalKey(k2, binary.SeqNum(seq2), binary.OpTypePut)
		if err1 != nil || err2 != nil {
			return
		}

		b := sstable.NewBlockBuilder()
		if err := b.Add(ik1, []byte("val1")); err != nil {
			t.Fatalf("Add ik1 failed: %v", err)
		}

		sizeBefore := b.DataSize()
		countBefore := b.EntryCount()
		restartCountBefore := b.RestartCount()

		cmp := binary.CompareInternalKey(ik1, ik2)
		err := b.Add(ik2, []byte("val2"))

		if cmp >= 0 {
			// ik2 sorts before or equal to ik1 -> must fail with ErrKeyOutOfOrder
			if !stdErrors.Is(err, errors.ErrKeyOutOfOrder) {
				t.Fatalf("expected ErrKeyOutOfOrder for cmp=%d: got %v", cmp, err)
			}
			// Failure atomicity: builder must be 100% unchanged!
			if b.DataSize() != sizeBefore {
				t.Fatalf("DataSize mutated on ordering failure: before=%d, after=%d", sizeBefore, b.DataSize())
			}
			if b.EntryCount() != countBefore {
				t.Fatalf("EntryCount mutated on ordering failure: before=%d, after=%d", countBefore, b.EntryCount())
			}
			if b.RestartCount() != restartCountBefore {
				t.Fatalf("RestartCount mutated on ordering failure")
			}
		} else {
			// ik2 sorts after ik1 -> must succeed
			if err != nil {
				t.Fatalf("Add ik2 failed for valid ordering: %v", err)
			}
			if b.EntryCount() != countBefore+1 {
				t.Fatalf("EntryCount expected %d, got %d", countBefore+1, b.EntryCount())
			}
		}
	})
}
