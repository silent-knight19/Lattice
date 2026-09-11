package sstable_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// decodedEntry represents an independently parsed block entry.
type decodedEntry struct {
	offset   int
	shared   uint64
	unshared uint64
	valueLen uint64
	rawKey   []byte
	key      binary.InternalKey
	value    []byte
}

// parseBlockData is an independent test-side oracle that parses prefix-compressed data blocks
// without using any production decode functions.
func parseBlockData(t *testing.T, data []byte) []decodedEntry {
	t.Helper()
	var entries []decodedEntry
	var prevKey []byte
	cursor := 0

	for cursor < len(data) {
		entryOffset := cursor

		// 1. Read shared varint
		shared, n1, err := binary.GetVarint64Canonical(data[cursor:])
		if err != nil {
			t.Fatalf("offset %d: failed to read shared varint: %v", cursor, err)
		}
		cursor += n1

		// 2. Read unshared varint
		unshared, n2, err := binary.GetVarint64Canonical(data[cursor:])
		if err != nil {
			t.Fatalf("offset %d: failed to read unshared varint: %v", cursor, err)
		}
		cursor += n2

		// 3. Read valueLen varint
		valueLen, n3, err := binary.GetVarint64Canonical(data[cursor:])
		if err != nil {
			t.Fatalf("offset %d: failed to read valueLen varint: %v", cursor, err)
		}
		cursor += n3

		if int(shared) > len(prevKey) {
			t.Fatalf("offset %d: shared (%d) > len(prevKey) (%d)", entryOffset, shared, len(prevKey))
		}
		if cursor+int(unshared) > len(data) {
			t.Fatalf("offset %d: unshared key extends beyond data boundary", entryOffset)
		}

		suffix := data[cursor : cursor+int(unshared)]
		cursor += int(unshared)

		if cursor+int(valueLen) > len(data) {
			t.Fatalf("offset %d: value extends beyond data boundary", entryOffset)
		}

		val := data[cursor : cursor+int(valueLen)]
		cursor += int(valueLen)

		fullKey := make([]byte, int(shared)+int(unshared))
		copy(fullKey, prevKey[:shared])
		copy(fullKey[shared:], suffix)

		ikey, err := binary.DecodeInternalKey(fullKey)
		if err != nil {
			t.Fatalf("offset %d: failed to decode reconstructed InternalKey: %v", entryOffset, err)
		}

		entries = append(entries, decodedEntry{
			offset:   entryOffset,
			shared:   shared,
			unshared: unshared,
			valueLen: valueLen,
			rawKey:   fullKey,
			key:      ikey,
			value:    val,
		})

		prevKey = fullKey
	}

	return entries
}

func mustInternalKey(t *testing.T, userKey string, seq uint64, op binary.OpType) binary.InternalKey {
	t.Helper()
	k, err := binary.NewInternalKey([]byte(userKey), binary.SeqNum(seq), op)
	if err != nil {
		t.Fatalf("mustInternalKey failed for %s: %v", userKey, err)
	}
	return k
}

// -----------------------------------------------------------------------------
// Basic & Lifecycle Tests
// -----------------------------------------------------------------------------

func TestBlockBuilder_EmptyBlock(t *testing.T) {
	b := sstable.NewBlockBuilder()
	if !b.IsEmpty() {
		t.Errorf("expected empty builder")
	}
	if b.EntryCount() != 0 {
		t.Errorf("expected EntryCount == 0, got %d", b.EntryCount())
	}
	if b.RestartCount() != 0 {
		t.Errorf("expected RestartCount == 0, got %d", b.RestartCount())
	}
	if len(b.RestartOffsets()) != 0 {
		t.Errorf("expected empty RestartOffsets, got %v", b.RestartOffsets())
	}
	if b.DataSize() != 0 {
		t.Errorf("expected DataSize == 0, got %d", b.DataSize())
	}
	if b.Finished() {
		t.Errorf("expected finished == false initially")
	}

	data := b.Finish()
	if len(data) != 0 {
		t.Errorf("expected empty slice from Finish, got len %d", len(data))
	}
	if !b.Finished() {
		t.Errorf("expected finished == true after Finish")
	}

	// Repeated finish is idempotent
	data2 := b.Finish()
	if len(data2) != 0 {
		t.Errorf("expected empty slice on repeated Finish")
	}

	// Add after finish rejected
	k := mustInternalKey(t, "key", 1, binary.OpTypePut)
	err := b.Add(k, []byte("val"))
	if !stdErrors.Is(err, errors.ErrBlockFinished) {
		t.Errorf("expected ErrBlockFinished, got %v", err)
	}
}

func TestBlockBuilder_SingleEntry(t *testing.T) {
	b := sstable.NewBlockBuilder()
	k := mustInternalKey(t, "user:0001", 100, binary.OpTypePut)
	val := []byte("payload-1")

	if err := b.Add(k, val); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	if b.IsEmpty() {
		t.Errorf("expected non-empty builder")
	}
	if b.EntryCount() != 1 {
		t.Errorf("expected EntryCount == 1, got %d", b.EntryCount())
	}
	if b.RestartCount() != 1 {
		t.Errorf("expected RestartCount == 1, got %d", b.RestartCount())
	}
	offsets := b.RestartOffsets()
	if len(offsets) != 1 || offsets[0] != 0 {
		t.Errorf("expected restartOffsets == [0], got %v", offsets)
	}

	data := b.Finish()
	entries := parseBlockData(t, data)
	if len(entries) != 1 {
		t.Fatalf("expected 1 parsed entry, got %d", len(entries))
	}

	e := entries[0]
	if e.shared != 0 {
		t.Errorf("first entry must have shared=0, got %d", e.shared)
	}
	if !e.key.Equal(k) {
		t.Errorf("key mismatch: got %v, want %v", e.key, k)
	}
	if !bytes.Equal(e.value, val) {
		t.Errorf("value mismatch: got %q, want %q", e.value, val)
	}
	if e.offset != 0 {
		t.Errorf("expected entry offset 0, got %d", e.offset)
	}
}

func TestBlockBuilder_InvalidRestartInterval(t *testing.T) {
	invalidIntervals := []int{0, -1, -100}
	for _, interval := range invalidIntervals {
		b, err := sstable.NewBlockBuilderWithInterval(interval)
		if b != nil || !stdErrors.Is(err, errors.ErrInvalidRestartInterval) {
			t.Errorf("expected ErrInvalidRestartInterval for %d, got b=%v, err=%v", interval, b, err)
		}
	}

	valid, err := sstable.NewBlockBuilderWithInterval(1)
	if err != nil || valid == nil {
		t.Fatalf("expected success for interval 1, got err: %v", err)
	}
	if valid.RestartInterval() != 1 {
		t.Errorf("expected restart interval 1, got %d", valid.RestartInterval())
	}
}

func TestBlockBuilder_ResetLifecycle(t *testing.T) {
	b := sstable.NewBlockBuilder()
	k1 := mustInternalKey(t, "key:001", 10, binary.OpTypePut)
	k2 := mustInternalKey(t, "key:002", 10, binary.OpTypePut)

	if err := b.Add(k1, []byte("val1")); err != nil {
		t.Fatalf("Add k1 failed: %v", err)
	}
	if err := b.Add(k2, []byte("val2")); err != nil {
		t.Fatalf("Add k2 failed: %v", err)
	}

	data1 := b.Finish()
	if len(data1) == 0 {
		t.Fatalf("expected non-empty data1")
	}

	// Reset builder
	b.Reset()
	if !b.IsEmpty() {
		t.Errorf("expected builder to be empty after Reset")
	}
	if b.Finished() {
		t.Errorf("expected finished == false after Reset")
	}
	if b.EntryCount() != 0 {
		t.Errorf("expected EntryCount == 0 after Reset")
	}
	if b.RestartCount() != 0 {
		t.Errorf("expected RestartCount == 0 after Reset")
	}
	if b.DataSize() != 0 {
		t.Errorf("expected DataSize == 0 after Reset")
	}

	// Builder can be reused immediately after Reset
	k3 := mustInternalKey(t, "fresh:001", 50, binary.OpTypePut)
	if err := b.Add(k3, []byte("fresh-val")); err != nil {
		t.Fatalf("Add after Reset failed: %v", err)
	}

	data2 := b.Finish()
	entries := parseBlockData(t, data2)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after Reset, got %d", len(entries))
	}
	if !entries[0].key.Equal(k3) {
		t.Errorf("expected fresh key, got %v", entries[0].key)
	}
}

// -----------------------------------------------------------------------------
// Exact Binary Format Verification (Step 19)
// -----------------------------------------------------------------------------

func TestBlockBuilder_ExactBinaryFormat(t *testing.T) {
	// Hand-calculated deterministic example:
	//
	// Record 0:
	//   UserKey: "apple" (5B), SeqNum: 1 (8B), OpType: PUT (1B) -> 14 bytes encoded InternalKey
	//   Encoded Key: [0x61, 0x70, 0x70, 0x6c, 0x65, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x01]
	//   Value: "red" (3B) [0x72, 0x65, 0x64]
	//   Restart point 0 (entry 0): shared = 0 (0x00), unshared = 14 (0x0e), valueLen = 3 (0x03)
	//   Entry 0 bytes (20B):
	//     0x00, 0x0e, 0x03,
	//     0x61, 0x70, 0x70, 0x6c, 0x65, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x01,
	//     0x72, 0x65, 0x64
	//
	// Record 1:
	//   UserKey: "application" (11B), SeqNum: 1 (8B), OpType: PUT (1B) -> 20 bytes encoded InternalKey
	//   Value: "app" (3B) [0x61, 0x70, 0x70]
	//   Common prefix with "apple...": "appl" = 4 bytes
	//   shared = 4 (0x04), unshared = 20 - 4 = 16 (0x10), valueLen = 3 (0x03)
	//   Key delta (16B): "ication" (7B) + 8-byte SeqNum (1) + 1-byte OpType (1):
	//     0x69, 0x63, 0x61, 0x74, 0x69, 0x6f, 0x6e, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x01
	//   Value bytes (3B): 0x61, 0x70, 0x70
	//   Entry 1 bytes (22B):
	//     0x04, 0x10, 0x03,
	//     0x69, 0x63, 0x61, 0x74, 0x69, 0x6f, 0x6e, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x01,
	//     0x61, 0x70, 0x70
	//
	// Record 2:
	//   UserKey: "banana" (6B), SeqNum: 1 (8B), OpType: PUT (1B) -> 15 bytes encoded InternalKey
	//   Value: nil (0B)
	//   Common prefix with "application...": 0 bytes
	//   shared = 0 (0x00), unshared = 15 (0x0f), valueLen = 0 (0x00)
	//   Key delta (15B): full encoded InternalKey:
	//     0x62, 0x61, 0x6e, 0x61, 0x6e, 0x61, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x01
	//   Entry 2 bytes (18B):
	//     0x00, 0x0f, 0x00,
	//     0x62, 0x61, 0x6e, 0x61, 0x6e, 0x61, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x01
	//
	// Total expected block bytes = 20 + 22 + 18 = 60 bytes.

	expectedBytes := []byte{
		// Entry 0 (offset 0)
		0x00, 0x0e, 0x03,
		'a', 'p', 'p', 'l', 'e', 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x01,
		'r', 'e', 'd',

		// Entry 1 (offset 20)
		0x04, 0x10, 0x03,
		'i', 'c', 'a', 't', 'i', 'o', 'n', 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x01,
		'a', 'p', 'p',

		// Entry 2 (offset 42)
		0x00, 0x0f, 0x00,
		'b', 'a', 'n', 'a', 'n', 'a', 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x01,
	}

	b := sstable.NewBlockBuilder()
	k0 := mustInternalKey(t, "apple", 1, binary.OpTypePut)
	k1 := mustInternalKey(t, "application", 1, binary.OpTypePut)
	k2 := mustInternalKey(t, "banana", 1, binary.OpTypePut)

	if err := b.Add(k0, []byte("red")); err != nil {
		t.Fatalf("Add k0 failed: %v", err)
	}
	if err := b.Add(k1, []byte("app")); err != nil {
		t.Fatalf("Add k1 failed: %v", err)
	}
	if err := b.Add(k2, nil); err != nil {
		t.Fatalf("Add k2 failed: %v", err)
	}

	actualBytes := b.Finish()

	if !bytes.Equal(actualBytes, expectedBytes) {
		t.Fatalf("exact binary mismatch:\ngot:  %x\nwant: %x", actualBytes, expectedBytes)
	}

	// Verify restart offsets
	offsets := b.RestartOffsets()
	if len(offsets) != 1 || offsets[0] != 0 {
		t.Fatalf("expected restart offsets [0], got %v", offsets)
	}

	// Verify independent decoder parses it identically
	entries := parseBlockData(t, actualBytes)
	if len(entries) != 3 {
		t.Fatalf("expected 3 parsed entries, got %d", len(entries))
	}

	if !entries[0].key.Equal(k0) || string(entries[0].value) != "red" {
		t.Errorf("entry 0 parsed mismatch")
	}
	if !entries[1].key.Equal(k1) || string(entries[1].value) != "app" {
		t.Errorf("entry 1 parsed mismatch")
	}
	if !entries[2].key.Equal(k2) || len(entries[2].value) != 0 {
		t.Errorf("entry 2 parsed mismatch")
	}
}

// -----------------------------------------------------------------------------
// Restart Point & Group Invariant Tests (Step 7 & 8)
// -----------------------------------------------------------------------------

func TestBlockBuilder_RestartInterval_16Records(t *testing.T) {
	b := sstable.NewBlockBuilder() // Default restart interval 16

	const numRecords = 40
	keys := make([]binary.InternalKey, numRecords)
	values := make([][]byte, numRecords)

	for i := 0; i < numRecords; i++ {
		// Keys with shared prefix "prefix:common:key:0000"
		keys[i] = mustInternalKey(t, fmt.Sprintf("prefix:common:key:%04d", i), 10, binary.OpTypePut)
		values[i] = []byte(fmt.Sprintf("value-%04d", i))
		if err := b.Add(keys[i], values[i]); err != nil {
			t.Fatalf("Add record %d failed: %v", i, err)
		}
	}

	if b.EntryCount() != numRecords {
		t.Errorf("expected %d entries, got %d", numRecords, b.EntryCount())
	}

	// With interval 16 and 40 records:
	// Restart entries must occur at indexes: 0, 16, 32 -> exactly 3 restart points.
	if b.RestartCount() != 3 {
		t.Fatalf("expected 3 restart points, got %d", b.RestartCount())
	}

	offsets := b.RestartOffsets()
	if len(offsets) != 3 {
		t.Fatalf("expected 3 restart offsets, got %d", len(offsets))
	}

	// Offset 0 must be 0
	if offsets[0] != 0 {
		t.Errorf("offsets[0] must be 0, got %d", offsets[0])
	}
	// Later offsets must be strictly increasing
	if offsets[1] <= offsets[0] || offsets[2] <= offsets[1] {
		t.Errorf("restart offsets must be strictly increasing, got %v", offsets)
	}

	data := b.Finish()
	entries := parseBlockData(t, data)
	if len(entries) != numRecords {
		t.Fatalf("expected %d parsed entries, got %d", numRecords, len(entries))
	}

	// Invariant: At every restart point (0, 16, 32), shared must be exactly 0!
	restartIndices := map[int]bool{0: true, 16: true, 32: true}
	for idx, entry := range entries {
		if restartIndices[idx] {
			if entry.shared != 0 {
				t.Errorf("entry %d is a restart point but has shared=%d (must be 0)", idx, entry.shared)
			}
			// Verify entry offset matches recorded restart offset
			expectedOffset := offsets[idx/16]
			if uint32(entry.offset) != expectedOffset {
				t.Errorf("entry %d offset %d != restart offset %d", idx, entry.offset, expectedOffset)
			}
		} else {
			// Non-restart entries must share prefix with preceding key
			if entry.shared == 0 {
				t.Errorf("entry %d is non-restart with shared prefix but got shared=0", idx)
			}
		}

		// Verify key and value integrity
		if !entry.key.Equal(keys[idx]) {
			t.Errorf("entry %d key mismatch: got %v, want %v", idx, entry.key, keys[idx])
		}
		if !bytes.Equal(entry.value, values[idx]) {
			t.Errorf("entry %d value mismatch: got %q, want %q", idx, entry.value, values[idx])
		}
	}
}

func TestBlockBuilder_CustomRestartInterval_1(t *testing.T) {
	// Interval 1 means every single entry is a restart point (shared = 0 always).
	b, err := sstable.NewBlockBuilderWithInterval(1)
	if err != nil {
		t.Fatalf("failed to create builder: %v", err)
	}

	for i := 0; i < 10; i++ {
		k := mustInternalKey(t, fmt.Sprintf("key:%02d", i), 1, binary.OpTypePut)
		if err := b.Add(k, []byte("v")); err != nil {
			t.Fatalf("Add %d failed: %v", i, err)
		}
	}

	if b.RestartCount() != 10 {
		t.Errorf("expected 10 restart points for interval 1, got %d", b.RestartCount())
	}

	data := b.Finish()
	entries := parseBlockData(t, data)
	for i, e := range entries {
		if e.shared != 0 {
			t.Errorf("entry %d: expected shared=0 for interval 1, got %d", i, e.shared)
		}
	}
}

// -----------------------------------------------------------------------------
// Prefix Compression Edge Cases
// -----------------------------------------------------------------------------

func TestBlockBuilder_PrefixCompressionEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		keys     []string
		expected []uint64 // expected shared prefix lengths
	}{
		{
			name:     "ZeroSharedPrefix",
			keys:     []string{"alpha", "bravo", "charlie", "delta"},
			expected: []uint64{0, 0, 0, 0},
		},
		{
			name:     "OneByteSharedPrefix",
			keys:     []string{"ba", "bb", "bc", "bd"},
			expected: []uint64{0, 1, 1, 1},
		},
		{
			name: "LongSharedPrefix",
			keys: []string{
				"cluster:us-east:region:datacenter-1:node:001",
				"cluster:us-east:region:datacenter-1:node:002",
				"cluster:us-east:region:datacenter-1:node:003",
			},
			expected: []uint64{0, 43, 43},
		},
		{
			name: "CurrentKeyShorterThanPrevious",
			keys: []string{
				"prefix:long:extended:key:name",
				"prefix:long:short",
			},
			// "prefix:long:extended:key:name" vs "prefix:long:short"
			// Common prefix is "prefix:long:" (12 bytes)
			expected: []uint64{0, 12},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := sstable.NewBlockBuilder()
			for _, kStr := range tc.keys {
				k := mustInternalKey(t, kStr, 1, binary.OpTypePut)
				if err := b.Add(k, []byte("val")); err != nil {
					t.Fatalf("Add %s failed: %v", kStr, err)
				}
			}

			data := b.Finish()
			entries := parseBlockData(t, data)
			if len(entries) != len(tc.expected) {
				t.Fatalf("entry count mismatch: got %d, want %d", len(entries), len(tc.expected))
			}

			for i, expShared := range tc.expected {
				if entries[i].shared != expShared {
					t.Errorf("entry %d (%s): expected shared=%d, got %d", i, tc.keys[i], expShared, entries[i].shared)
				}
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Multi-Version InternalKey Ordering & Compression
// -----------------------------------------------------------------------------

func TestBlockBuilder_InternalKey_MultiVersion(t *testing.T) {
	b := sstable.NewBlockBuilder()

	// In LSM engines, multiple versions of the same user key sort by SeqNum DESC.
	// Version 100 sorts before Version 50, which sorts before Version 10.
	v100 := mustInternalKey(t, "user:account:101", 100, binary.OpTypePut)
	v50 := mustInternalKey(t, "user:account:101", 50, binary.OpTypePut)
	v10Tomb := mustInternalKey(t, "user:account:101", 10, binary.OpTypeDelete)

	if err := b.Add(v100, []byte("data-v100")); err != nil {
		t.Fatalf("Add v100 failed: %v", err)
	}
	if err := b.Add(v50, []byte("data-v50")); err != nil {
		t.Fatalf("Add v50 failed: %v", err)
	}
	if err := b.Add(v10Tomb, nil); err != nil {
		t.Fatalf("Add v10Tomb failed: %v", err)
	}

	data := b.Finish()
	entries := parseBlockData(t, data)

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// Verify version ordering and reconstruction
	if !entries[0].key.Equal(v100) {
		t.Errorf("entry 0 key mismatch")
	}
	if !entries[1].key.Equal(v50) {
		t.Errorf("entry 1 key mismatch")
	}
	if !entries[2].key.Equal(v10Tomb) {
		t.Errorf("entry 2 key mismatch")
	}
	if entries[2].key.OpType != binary.OpTypeDelete {
		t.Errorf("expected tombstone OpTypeDelete")
	}
	if len(entries[2].value) != 0 {
		t.Errorf("expected empty value for tombstone")
	}

	// Verify prefix compression across multi-version keys:
	// "user:account:101" + 8-byte seqnum + 1-byte optype = 16 + 8 + 1 = 25 bytes.
	// Between v100 (seq 100) and v50 (seq 50), the 16-byte user key AND top 7 bytes of seqnum (all 0x00)
	// match! Thus shared prefix length is 16 + 7 = 23 bytes!
	if entries[1].shared < 16 {
		t.Errorf("expected multi-version shared prefix >= 16 bytes, got %d", entries[1].shared)
	}
}

func TestBlockBuilder_InternalKey_BinaryAndHighBit(t *testing.T) {
	b := sstable.NewBlockBuilder()

	// Keys with null bytes, 0xFF, and high-bit sequences
	k1 := binary.InternalKey{UserKey: []byte("\x00\x00\x01\x02"), SeqNum: 10, OpType: binary.OpTypePut}
	k2 := binary.InternalKey{UserKey: []byte("\x00\x00\x01\x03"), SeqNum: 10, OpType: binary.OpTypePut}
	k3 := binary.InternalKey{UserKey: []byte("\xff\xfe\xfd"), SeqNum: 10, OpType: binary.OpTypePut}
	k4 := binary.InternalKey{UserKey: []byte("\xff\xff\x00"), SeqNum: 10, OpType: binary.OpTypePut}

	keys := []binary.InternalKey{k1, k2, k3, k4}
	for i, k := range keys {
		if err := b.Add(k, []byte(fmt.Sprintf("v-%d", i))); err != nil {
			t.Fatalf("Add key %d failed: %v", i, err)
		}
	}

	data := b.Finish()
	entries := parseBlockData(t, data)

	if len(entries) != len(keys) {
		t.Fatalf("entry count mismatch: got %d, want %d", len(entries), len(keys))
	}

	for i, k := range keys {
		if !entries[i].key.Equal(k) {
			t.Errorf("binary key %d mismatch: got %v, want %v", i, entries[i].key, k)
		}
	}
}

// -----------------------------------------------------------------------------
// Ordering Violations & Failure Atomicity (Step 6)
// -----------------------------------------------------------------------------

func TestBlockBuilder_OrderingFailures_Atomicity(t *testing.T) {
	t.Run("DescendingUserKey", func(t *testing.T) {
		b := sstable.NewBlockBuilder()
		k1 := mustInternalKey(t, "bravo", 10, binary.OpTypePut)
		k2 := mustInternalKey(t, "alpha", 10, binary.OpTypePut) // Violates UserKey ASC

		if err := b.Add(k1, []byte("v1")); err != nil {
			t.Fatalf("Add k1 failed: %v", err)
		}

		sizeBefore := b.DataSize()
		countBefore := b.EntryCount()

		err := b.Add(k2, []byte("v2"))
		if !stdErrors.Is(err, errors.ErrKeyOutOfOrder) {
			t.Fatalf("expected ErrKeyOutOfOrder, got %v", err)
		}

		// Failure Atomicity verification: builder state must be 100% unmodified!
		if b.DataSize() != sizeBefore {
			t.Errorf("DataSize mutated on failure: before=%d, after=%d", sizeBefore, b.DataSize())
		}
		if b.EntryCount() != countBefore {
			t.Errorf("EntryCount mutated on failure: before=%d, after=%d", countBefore, b.EntryCount())
		}

		// Valid key added subsequently must succeed
		k3 := mustInternalKey(t, "charlie", 10, binary.OpTypePut)
		if err := b.Add(k3, []byte("v3")); err != nil {
			t.Fatalf("subsequent valid Add failed: %v", err)
		}
	})

	t.Run("SameUserKeyAscendingSeqNum", func(t *testing.T) {
		b := sstable.NewBlockBuilder()
		// For identical UserKeys, SeqNum must be DESCENDING (newest first).
		// Adding SeqNum 10 then SeqNum 20 violates this order!
		k1 := mustInternalKey(t, "same-user", 10, binary.OpTypePut)
		k2 := mustInternalKey(t, "same-user", 20, binary.OpTypePut)

		if err := b.Add(k1, []byte("v1")); err != nil {
			t.Fatalf("Add k1 failed: %v", err)
		}

		err := b.Add(k2, []byte("v2"))
		if !stdErrors.Is(err, errors.ErrKeyOutOfOrder) {
			t.Fatalf("expected ErrKeyOutOfOrder for increasing SeqNum, got %v", err)
		}
	})

	t.Run("ExactDuplicateKey", func(t *testing.T) {
		b := sstable.NewBlockBuilder()
		k1 := mustInternalKey(t, "exact-dup", 10, binary.OpTypePut)
		k2 := mustInternalKey(t, "exact-dup", 10, binary.OpTypePut)

		if err := b.Add(k1, []byte("v1")); err != nil {
			t.Fatalf("Add k1 failed: %v", err)
		}

		err := b.Add(k2, []byte("v2"))
		if !stdErrors.Is(err, errors.ErrKeyOutOfOrder) {
			t.Fatalf("expected ErrKeyOutOfOrder on exact duplicate, got %v", err)
		}
	})

	t.Run("InvalidOpTypeOrdering", func(t *testing.T) {
		b := sstable.NewBlockBuilder()
		// If UserKey and SeqNum are identical, OpTypeDelete (0x02) must sort before OpTypePut (0x01).
		// Adding OpTypePut first then OpTypeDelete violates this order!
		kPut := mustInternalKey(t, "key", 10, binary.OpTypePut)
		kDel := mustInternalKey(t, "key", 10, binary.OpTypeDelete)

		if err := b.Add(kPut, []byte("v")); err != nil {
			t.Fatalf("Add kPut failed: %v", err)
		}

		err := b.Add(kDel, nil)
		if !stdErrors.Is(err, errors.ErrKeyOutOfOrder) {
			t.Fatalf("expected ErrKeyOutOfOrder for OpType ordering inversion, got %v", err)
		}
	})
}

// -----------------------------------------------------------------------------
// Boundary Validation & Error Defenses
// -----------------------------------------------------------------------------

func TestBlockBuilder_BoundaryValidations(t *testing.T) {
	b := sstable.NewBlockBuilder()

	// 1. Empty key
	err := b.Add(binary.InternalKey{UserKey: []byte{}, SeqNum: 1, OpType: binary.OpTypePut}, []byte("v"))
	if !stdErrors.Is(err, errors.ErrEmptyKey) {
		t.Errorf("expected ErrEmptyKey, got %v", err)
	}

	// 2. Oversized key (> 65,535 bytes)
	hugeKey := make([]byte, binary.MaxKeyLen+1)
	err = b.Add(binary.InternalKey{UserKey: hugeKey, SeqNum: 1, OpType: binary.OpTypePut}, []byte("v"))
	if !stdErrors.Is(err, errors.ErrKeyTooLarge) {
		t.Errorf("expected ErrKeyTooLarge, got %v", err)
	}

	// 3. Oversized value (> 4MB)
	hugeVal := make([]byte, binary.MaxValueLen+1)
	validKey := mustInternalKey(t, "valid-key", 1, binary.OpTypePut)
	err = b.Add(validKey, hugeVal)
	if !stdErrors.Is(err, errors.ErrValueTooLarge) {
		t.Errorf("expected ErrValueTooLarge, got %v", err)
	}

	// 4. Invalid OpType
	err = b.Add(binary.InternalKey{UserKey: []byte("k"), SeqNum: 1, OpType: binary.OpType(99)}, []byte("v"))
	if !stdErrors.Is(err, errors.ErrInvalidOpType) {
		t.Errorf("expected ErrInvalidOpType, got %v", err)
	}

	// 5. Nil receiver check
	var nilBuilder *sstable.BlockBuilder
	if err := nilBuilder.Add(validKey, []byte("v")); !stdErrors.Is(err, errors.ErrNilReceiver) {
		t.Errorf("expected ErrNilReceiver, got %v", err)
	}
	if nilBuilder.Finish() != nil {
		t.Errorf("expected nil from nilBuilder.Finish()")
	}
	if nilBuilder.DataSize() != 0 {
		t.Errorf("expected 0 from nilBuilder.DataSize()")
	}
	if !nilBuilder.IsEmpty() {
		t.Errorf("expected true from nilBuilder.IsEmpty()")
	}
}

// -----------------------------------------------------------------------------
// Defensive Copying & Caller Isolation (Step 10)
// -----------------------------------------------------------------------------

func TestBlockBuilder_CallerIsolation(t *testing.T) {
	b := sstable.NewBlockBuilder()

	mutableKey := []byte("original-key")
	mutableVal := []byte("original-val")

	k := mustInternalKey(t, string(mutableKey), 1, binary.OpTypePut)
	if err := b.Add(k, mutableVal); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	// Mutate caller buffers immediately after Add
	mutableKey[0] = 'X'
	mutableVal[0] = 'X'

	data := b.Finish()

	// Mutate returned slice after Finish
	if len(data) > 0 {
		data[0] = 0xFF
	}

	// Verify builder state remains uncontaminated
	dataReRead := b.Finish()
	if dataReRead[0] == 0xFF {
		t.Fatalf("Finish did not return defensive copy: internal buffer was mutated!")
	}

	entries := parseBlockData(t, dataReRead)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if string(entries[0].key.UserKey) != "original-key" {
		t.Errorf("caller key mutation contaminated builder: got %q", string(entries[0].key.UserKey))
	}
	if string(entries[0].value) != "original-val" {
		t.Errorf("caller value mutation contaminated builder: got %q", string(entries[0].value))
	}
}

// -----------------------------------------------------------------------------
// Determinism (Step 20)
// -----------------------------------------------------------------------------

func TestBlockBuilder_Determinism(t *testing.T) {
	// Build identical records across 3 independent builders
	buildOnce := func() []byte {
		b := sstable.NewBlockBuilder()
		for i := 0; i < 50; i++ {
			k := mustInternalKey(t, fmt.Sprintf("det:key:%04d", i), uint64(100-i), binary.OpTypePut)
			v := []byte(fmt.Sprintf("det-val-%04d", i))
			if err := b.Add(k, v); err != nil {
				panic(err)
			}
		}
		return b.Finish()
	}

	b1 := buildOnce()
	b2 := buildOnce()
	b3 := buildOnce()

	if !bytes.Equal(b1, b2) || !bytes.Equal(b2, b3) {
		t.Fatalf("block building is non-deterministic across executions!")
	}
}

// -----------------------------------------------------------------------------
// AddRaw Coverage
// -----------------------------------------------------------------------------

func TestBlockBuilder_AddRaw(t *testing.T) {
	b := sstable.NewBlockBuilder()

	k1 := mustInternalKey(t, "user:1", 10, binary.OpTypePut)
	k2 := mustInternalKey(t, "user:2", 10, binary.OpTypePut)

	raw1 := binary.EncodeInternalKey(k1)
	raw2 := binary.EncodeInternalKey(k2)

	if err := b.AddRaw(raw1, []byte("v1")); err != nil {
		t.Fatalf("AddRaw 1 failed: %v", err)
	}
	if err := b.AddRaw(raw2, []byte("v2")); err != nil {
		t.Fatalf("AddRaw 2 failed: %v", err)
	}

	data := b.Finish()
	entries := parseBlockData(t, data)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if !entries[0].key.Equal(k1) || !entries[1].key.Equal(k2) {
		t.Errorf("AddRaw key mismatch")
	}

	// Rejection of invalid raw key
	b2 := sstable.NewBlockBuilder()
	if err := b2.AddRaw([]byte("short"), []byte("v")); !stdErrors.Is(err, errors.ErrInternalKeyTruncated) {
		t.Errorf("expected ErrInternalKeyTruncated, got %v", err)
	}
}
