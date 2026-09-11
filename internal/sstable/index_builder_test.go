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

func testIKBytes(userKey string) []byte {
	ik, _ := binary.NewInternalKey([]byte(userKey), 100, binary.OpTypePut)
	return binary.AppendInternalKey(nil, ik)
}

// TestIndexBuilder_50DataBlocks verifies the primary architectural invariant of P04-S02-M01:
// constructing a sparse Two-Level Block Index across 50 sequentially emitted data blocks,
// and asserting that all 50 handles accurately point to the physical offsets and sizes of the blocks.
func TestIndexBuilder_50DataBlocks(t *testing.T) {
	indexBuilder := sstable.NewIndexBuilder()
	if !indexBuilder.IsEmpty() {
		t.Fatal("expected newly initialized IndexBuilder to be empty")
	}
	if indexBuilder.EntryCount() != 0 {
		t.Fatalf("expected 0 entries, got %d", indexBuilder.EntryCount())
	}

	const numBlocks = 50
	type blockRecord struct {
		largestKey []byte
		handle     sstable.BlockHandle
		data       []byte
	}
	blocks := make([]blockRecord, numBlocks)

	var currentFileOffset uint64 = 0

	// Generate 50 realistic SSTable data blocks using BlockBuilder
	for b := 0; b < numBlocks; b++ {
		dataBuilder := sstable.NewBlockBuilder()

		// Each data block contains 10 records with common prefixes
		var blockLargestKey []byte
		for r := 0; r < 10; r++ {
			userKey := fmt.Sprintf("partition:%04d:record:%04d", b, r)
			seqNum := uint64(100 - r) // descending sequence numbers
			ik, err := binary.NewInternalKey([]byte(userKey), binary.SeqNum(seqNum), binary.OpTypePut)
			if err != nil {
				t.Fatalf("block %d record %d NewInternalKey failed: %v", b, r, err)
			}
			val := fmt.Sprintf("value-payload-%04d-%04d", b, r)
			if err := dataBuilder.Add(ik, []byte(val)); err != nil {
				t.Fatalf("block %d record %d Add failed: %v", b, r, err)
			}
			if r == 9 {
				// Record 9 is the last and largest key in this block under canonical ordering
				blockLargestKey = binary.AppendInternalKey(nil, ik)
			}
		}

		finishedBlock := dataBuilder.Finish()
		blockSize := uint64(len(finishedBlock))
		if blockSize == 0 {
			t.Fatalf("block %d produced empty finished block", b)
		}

		handle := sstable.BlockHandle{
			Offset: currentFileOffset,
			Size:   blockSize,
		}

		blocks[b] = blockRecord{
			largestKey: blockLargestKey,
			handle:     handle,
			data:       finishedBlock,
		}

		// Register block in sparse index
		if err := indexBuilder.AddBlock(blockLargestKey, handle); err != nil {
			t.Fatalf("failed to AddBlock %d: %v", b, err)
		}

		currentFileOffset += blockSize
	}

	// Verify builder state
	if indexBuilder.IsEmpty() {
		t.Fatal("expected index builder to not be empty after adding 50 blocks")
	}
	if indexBuilder.EntryCount() != numBlocks {
		t.Fatalf("expected %d entries, got %d", numBlocks, indexBuilder.EntryCount())
	}

	// Verify in-memory entries match physical data
	entries := indexBuilder.Entries()
	if len(entries) != numBlocks {
		t.Fatalf("expected %d entries from Entries(), got %d", numBlocks, len(entries))
	}
	for i := 0; i < numBlocks; i++ {
		if !bytes.Equal(entries[i].LargestKey, blocks[i].largestKey) {
			t.Fatalf("entry %d largest key mismatch:\ngot:  %x\nwant: %x", i, entries[i].LargestKey, blocks[i].largestKey)
		}
		if entries[i].Handle != blocks[i].handle {
			t.Fatalf("entry %d handle mismatch: got %+v, want %+v", i, entries[i].Handle, blocks[i].handle)
		}
	}

	// Serialize index block
	serializedIndex := indexBuilder.Finish()
	if len(serializedIndex) == 0 {
		t.Fatal("expected non-empty serialized index block")
	}
	if !indexBuilder.Finished() {
		t.Fatal("expected builder to report finished")
	}

	// Repeated Finish idempotence check
	repeated := indexBuilder.Finish()
	if !bytes.Equal(serializedIndex, repeated) {
		t.Fatal("repeated Finish() returned non-identical byte slices")
	}

	// Subsequent AddBlock rejected
	extraHandle := sstable.BlockHandle{Offset: currentFileOffset, Size: 100}
	if err := indexBuilder.AddBlock(testIKBytes("extra"), extraHandle); !stdErrors.Is(err, errors.ErrIndexFinished) {
		t.Fatalf("expected ErrIndexFinished after Finish, got %v", err)
	}

	// Decode and verify with independent reader
	decodedIndex, err := sstable.DecodeBlockIndex(serializedIndex)
	if err != nil {
		t.Fatalf("DecodeBlockIndex failed: %v", err)
	}
	if decodedIndex.EntryCount() != numBlocks {
		t.Fatalf("decoded entry count mismatch: got %d, want %d", decodedIndex.EntryCount(), numBlocks)
	}

	decodedEntries := decodedIndex.Entries()
	for i := 0; i < numBlocks; i++ {
		if !bytes.Equal(decodedEntries[i].LargestKey, blocks[i].largestKey) {
			t.Fatalf("decoded entry %d key mismatch", i)
		}
		if decodedEntries[i].Handle != blocks[i].handle {
			t.Fatalf("decoded entry %d handle mismatch: got %+v, want %+v", i, decodedEntries[i].Handle, blocks[i].handle)
		}
	}

	// Binary search verification across all 50 blocks
	for i := 0; i < numBlocks; i++ {
		// 1. Query exact largest key of block i (using FindBlockInternalKey for encoded internal key)
		handle, found := decodedIndex.FindBlockInternalKey(blocks[i].largestKey)
		if !found {
			t.Fatalf("block %d largest key not found via FindBlockInternalKey", i)
		}
		if handle != blocks[i].handle {
			t.Fatalf("FindBlockInternalKey(%d) returned handle %+v, want %+v", i, handle, blocks[i].handle)
		}

		// 2. Query record 0 from block i via internal key
		probeKey, _ := binary.NewInternalKey([]byte(fmt.Sprintf("partition:%04d:record:0000", i)), 100, binary.OpTypePut)
		probeKeyBytes := binary.AppendInternalKey(nil, probeKey)
		hProbe, foundProbe := decodedIndex.FindBlockInternalKey(probeKeyBytes)
		if !foundProbe {
			t.Fatalf("probe key for block %d not found", i)
		}
		if hProbe != blocks[i].handle {
			t.Fatalf("probe key for block %d resolved to wrong handle: got %+v, want %+v", i, hProbe, blocks[i].handle)
		}

		// 3. Query record from block i via bare UserKey (the primary TableReader.Seek path)
		userKey := []byte(fmt.Sprintf("partition:%04d:record:0005", i))
		hUser, foundUser := decodedIndex.FindBlock(userKey)
		if !foundUser {
			t.Fatalf("bare user key for block %d not found via FindBlock", i)
		}
		if hUser != blocks[i].handle {
			t.Fatalf("bare user key for block %d resolved to wrong handle: got %+v, want %+v", i, hUser, blocks[i].handle)
		}
	}

	// Query key smaller than all keys in SSTable (should resolve to block 0)
	minKey, _ := binary.NewInternalKey([]byte("partition:0000:record:!first"), 100, binary.OpTypePut)
	hMin, foundMin := decodedIndex.FindBlockInternalKey(binary.AppendInternalKey(nil, minKey))
	if !foundMin || hMin != blocks[0].handle {
		t.Fatalf("expected minKey to resolve to block 0, got found=%v, handle=%+v", foundMin, hMin)
	}

	// Query bare user key smaller than all keys in SSTable (should resolve to block 0)
	hMinUser, foundMinUser := decodedIndex.FindBlock([]byte("partition:0000:record:!first"))
	if !foundMinUser || hMinUser != blocks[0].handle {
		t.Fatalf("expected minUserKey to resolve to block 0, got found=%v, handle=%+v", foundMinUser, hMinUser)
	}

	// Query key strictly larger than the largest key in the entire SSTable (should return false)
	maxKey, _ := binary.NewInternalKey([]byte("partition:9999:record:9999"), 100, binary.OpTypePut)
	_, foundMax := decodedIndex.FindBlockInternalKey(binary.AppendInternalKey(nil, maxKey))
	if foundMax {
		t.Fatal("expected FindBlockInternalKey to return false for key exceeding all block boundaries")
	}

	// Query bare user key strictly larger than the largest key in the entire SSTable (should return false)
	_, foundMaxUser := decodedIndex.FindBlock([]byte("partition:9999:record:9999"))
	if foundMaxUser {
		t.Fatal("expected FindBlock to return false for user key exceeding all block boundaries")
	}
}

// TestIndexBuilder_AddBlockKey_Convenience verifies AddBlockKey helper with binary.InternalKey.
func TestIndexBuilder_AddBlockKey_Convenience(t *testing.T) {
	builder := sstable.NewIndexBuilder()

	k1, _ := binary.NewInternalKey([]byte("key-01"), 10, binary.OpTypePut)
	h1 := sstable.BlockHandle{Offset: 0, Size: 4096}
	if err := builder.AddBlockKey(k1, h1); err != nil {
		t.Fatalf("AddBlockKey k1 failed: %v", err)
	}

	k2, _ := binary.NewInternalKey([]byte("key-02"), 10, binary.OpTypePut)
	h2 := sstable.BlockHandle{Offset: 4096, Size: 4096}
	if err := builder.AddBlockKey(k2, h2); err != nil {
		t.Fatalf("AddBlockKey k2 failed: %v", err)
	}

	if builder.EntryCount() != 2 {
		t.Fatalf("expected 2 entries, got %d", builder.EntryCount())
	}

	// Verify FindBlock
	foundH, found := builder.FindBlock([]byte("key-01"))
	if !found || foundH != h1 {
		t.Fatalf("expected h1, got found=%v, handle=%+v", found, foundH)
	}
}

// TestIndexBuilder_Invariants verifies ordering, nil receiver, boundary values, and error behavior.
func TestIndexBuilder_Invariants(t *testing.T) {
	t.Run("nil receiver error handling", func(t *testing.T) {
		var nilBuilder *sstable.IndexBuilder
		handle := sstable.BlockHandle{Offset: 0, Size: 100}
		if err := nilBuilder.AddBlock(testIKBytes("key"), handle); !stdErrors.Is(err, errors.ErrNilReceiver) {
			t.Fatalf("expected ErrNilReceiver, got %v", err)
		}
		if nilBuilder.Finish() != nil {
			t.Fatal("expected nil from Finish on nil receiver")
		}
		if nilBuilder.EntryCount() != 0 {
			t.Fatalf("expected 0 entries on nil receiver")
		}
		if !nilBuilder.IsEmpty() {
			t.Fatal("expected IsEmpty to be true on nil receiver")
		}
		if nilBuilder.Finished() {
			t.Fatal("expected Finished to be false on nil receiver")
		}
		if nilBuilder.Entries() != nil && len(nilBuilder.Entries()) != 0 {
			t.Fatal("expected empty entries on nil receiver")
		}
		if _, found := nilBuilder.FindBlock([]byte("key")); found {
			t.Fatal("expected found=false from FindBlock on nil receiver")
		}
		if nilBuilder.CurrentSizeEstimate() != 0 {
			t.Fatal("expected 0 size estimate on nil receiver")
		}
		// Reset on nil should not panic
		nilBuilder.Reset()
	})

	t.Run("empty key rejected", func(t *testing.T) {
		builder := sstable.NewIndexBuilder()
		err := builder.AddBlock([]byte{}, sstable.BlockHandle{Offset: 0, Size: 100})
		if !stdErrors.Is(err, errors.ErrEmptyKey) {
			t.Fatalf("expected ErrEmptyKey, got %v", err)
		}
		if builder.EntryCount() != 0 {
			t.Fatal("failed AddBlock must not mutate entry count")
		}
	})

	t.Run("key too large rejected", func(t *testing.T) {
		builder := sstable.NewIndexBuilder()
		oversizedKey := make([]byte, binary.MaxEncodedInternalKeyLen+1)
		err := builder.AddBlock(oversizedKey, sstable.BlockHandle{Offset: 0, Size: 100})
		if !stdErrors.Is(err, errors.ErrKeyTooLarge) {
			t.Fatalf("expected ErrKeyTooLarge, got %v", err)
		}
		if builder.EntryCount() != 0 {
			t.Fatal("failed AddBlock must not mutate entry count")
		}
	})

	t.Run("invalid handle rejected", func(t *testing.T) {
		builder := sstable.NewIndexBuilder()
		err := builder.AddBlock(testIKBytes("valid-key"), sstable.BlockHandle{Offset: 0, Size: 0})
		if !stdErrors.Is(err, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected ErrInvalidBlockHandle, got %v", err)
		}
		if builder.EntryCount() != 0 {
			t.Fatal("failed AddBlock must not mutate entry count")
		}
	})

	t.Run("keys must be strictly increasing", func(t *testing.T) {
		builder := sstable.NewIndexBuilder()
		h1 := sstable.BlockHandle{Offset: 0, Size: 100}
		h2 := sstable.BlockHandle{Offset: 100, Size: 100}

		if err := builder.AddBlock(testIKBytes("key-b"), h1); err != nil {
			t.Fatalf("AddBlock key-b failed: %v", err)
		}

		// Duplicate key rejected
		errDup := builder.AddBlock(testIKBytes("key-b"), h2)
		if !stdErrors.Is(errDup, errors.ErrKeyOutOfOrder) {
			t.Fatalf("expected ErrKeyOutOfOrder for duplicate key, got %v", errDup)
		}
		var oooErr *errors.KeyOutOfOrderError
		if !stdErrors.As(errDup, &oooErr) {
			t.Fatalf("expected *errors.KeyOutOfOrderError, got %v", errDup)
		}

		// Regressing key rejected
		errReg := builder.AddBlock(testIKBytes("key-a"), h2)
		if !stdErrors.Is(errReg, errors.ErrKeyOutOfOrder) {
			t.Fatalf("expected ErrKeyOutOfOrder for regressing key, got %v", errReg)
		}

		// State must remain unmodified with exactly 1 entry
		if builder.EntryCount() != 1 {
			t.Fatalf("expected 1 entry, got %d", builder.EntryCount())
		}

		// Strictly greater key succeeds
		if err := builder.AddBlock(testIKBytes("key-c"), h2); err != nil {
			t.Fatalf("AddBlock key-c failed: %v", err)
		}
		if builder.EntryCount() != 2 {
			t.Fatalf("expected 2 entries, got %d", builder.EntryCount())
		}
	})

	t.Run("empty builder finish produces empty slice", func(t *testing.T) {
		builder := sstable.NewIndexBuilder()
		finished := builder.Finish()
		if finished == nil || len(finished) != 0 {
			t.Fatalf("expected empty non-nil slice, got %v", finished)
		}
		if !builder.Finished() {
			t.Fatal("expected Finished() = true")
		}

		// Repeated finish remains empty
		if len(builder.Finish()) != 0 {
			t.Fatal("repeated Finish() on empty builder should remain empty")
		}

		// Decode empty slice
		decoded, err := sstable.DecodeBlockIndex(finished)
		if err != nil {
			t.Fatalf("DecodeBlockIndex on empty slice failed: %v", err)
		}
		if decoded.EntryCount() != 0 {
			t.Fatalf("expected 0 decoded entries, got %d", decoded.EntryCount())
		}
		if !decoded.IsEmpty() {
			t.Fatal("expected decoded index to be empty")
		}
	})
}

// TestIndexBuilder_FailureAtomicity_And_CallerIsolation asserts that:
// 1. A failed AddBlock operation leaves no residual bytes or state in the builder.
// 2. Caller modifications to input slices after AddBlock do not mutate internal builder state.
// 3. Modifying slices returned by Entries() does not mutate internal builder state.
func TestIndexBuilder_FailureAtomicity_And_CallerIsolation(t *testing.T) {
	builder := sstable.NewIndexBuilder()

	key := testIKBytes("apple")
	expectedKey := make([]byte, len(key))
	copy(expectedKey, key)
	h1 := sstable.BlockHandle{Offset: 0, Size: 100}
	if err := builder.AddBlock(key, h1); err != nil {
		t.Fatalf("AddBlock failed: %v", err)
	}

	// Mutate caller's slice
	key[0] ^= 0xFF
	entries := builder.Entries()
	if bytes.Equal(entries[0].LargestKey, key) {
		t.Fatal("builder failed caller isolation: internal key mutated by external caller write")
	}
	if !bytes.Equal(entries[0].LargestKey, expectedKey) {
		t.Fatalf("expected %x, got %x", expectedKey, entries[0].LargestKey)
	}

	// Mutate slice returned from Entries()
	entries[0].LargestKey[0] ^= 0xFF
	entries2 := builder.Entries()
	if bytes.Equal(entries2[0].LargestKey, entries[0].LargestKey) {
		t.Fatal("Entries() failed defensive copy isolation")
	}

	// Failure atomicity check
	initialEstimate := builder.CurrentSizeEstimate()
	initialCount := builder.EntryCount()

	// Attempt invalid addition (out of order key "aardvark" < "apple")
	err := builder.AddBlock(testIKBytes("aardvark"), sstable.BlockHandle{Offset: 100, Size: 50})
	if err == nil {
		t.Fatal("expected error on out of order key")
	}

	if builder.EntryCount() != initialCount {
		t.Fatalf("failure atomicity violated: entry count changed from %d to %d", initialCount, builder.EntryCount())
	}
	if builder.CurrentSizeEstimate() != initialEstimate {
		t.Fatalf("failure atomicity violated: size estimate changed from %d to %d", initialEstimate, builder.CurrentSizeEstimate())
	}
}

// TestIndexBuilder_Reset verifies that Reset() clears state and restores reusability.
func TestIndexBuilder_Reset(t *testing.T) {
	builder := sstable.NewIndexBuilder()

	// Populate first round
	_ = builder.AddBlock(testIKBytes("key-1"), sstable.BlockHandle{Offset: 0, Size: 100})
	_ = builder.AddBlock(testIKBytes("key-2"), sstable.BlockHandle{Offset: 100, Size: 100})
	_ = builder.Finish()

	if !builder.Finished() {
		t.Fatal("expected Finished() to be true")
	}

	// Reset
	builder.Reset()
	if builder.Finished() {
		t.Fatal("expected Finished() to be false after Reset()")
	}
	if builder.EntryCount() != 0 {
		t.Fatalf("expected 0 entries after Reset(), got %d", builder.EntryCount())
	}
	if !builder.IsEmpty() {
		t.Fatal("expected IsEmpty() to be true after Reset()")
	}

	// Populate second round
	if err := builder.AddBlock(testIKBytes("new-a"), sstable.BlockHandle{Offset: 0, Size: 500}); err != nil {
		t.Fatalf("AddBlock after reset failed: %v", err)
	}
	if err := builder.AddBlock(testIKBytes("new-b"), sstable.BlockHandle{Offset: 500, Size: 500}); err != nil {
		t.Fatalf("AddBlock after reset failed: %v", err)
	}

	secondFinish := builder.Finish()
	decoded, err := sstable.DecodeBlockIndex(secondFinish)
	if err != nil {
		t.Fatalf("DecodeBlockIndex failed on reused builder: %v", err)
	}
	if decoded.EntryCount() != 2 {
		t.Fatalf("expected 2 decoded entries, got %d", decoded.EntryCount())
	}
}

// TestIndexBlock_Corruption verifies that all forms of corruption in the serialized index block
// are detected deterministically without panicking.
func TestIndexBlock_Corruption(t *testing.T) {
	builder := sstable.NewIndexBuilder()
	_ = builder.AddBlock(testIKBytes("alpha"), sstable.BlockHandle{Offset: 0, Size: 100})
	_ = builder.AddBlock(testIKBytes("bravo"), sstable.BlockHandle{Offset: 100, Size: 100})
	_ = builder.AddBlock(testIKBytes("charlie"), sstable.BlockHandle{Offset: 200, Size: 100})
	validData := builder.Finish()

	t.Run("single-bit flip anywhere detected by CRC32", func(t *testing.T) {
		for i := 0; i < len(validData); i++ {
			corrupted := make([]byte, len(validData))
			copy(corrupted, validData)
			corrupted[i] ^= 0x01 // flip single bit

			_, err := sstable.DecodeBlockIndex(corrupted)
			if err == nil {
				t.Fatalf("byte %d corrupted with 1-bit flip but DecodeBlockIndex reported success", i)
			}
			// Must match ErrChecksumMismatch or ErrIndexBlockCorrupted
			if !stdErrors.Is(err, errors.ErrChecksumMismatch) && !stdErrors.Is(err, errors.ErrIndexBlockCorrupted) {
				t.Fatalf("expected ChecksumMismatch or Corrupted error at byte %d, got %v", i, err)
			}
		}
	})

	t.Run("truncated trailer rejected", func(t *testing.T) {
		for l := 1; l < sstable.IndexTrailerSize; l++ {
			_, err := sstable.DecodeBlockIndex(validData[:l])
			if !stdErrors.Is(err, errors.ErrIndexBlockTruncated) {
				t.Fatalf("expected ErrIndexBlockTruncated for len %d, got %v", l, err)
			}
		}
	})

	t.Run("entry count exceeds buffer capacity", func(t *testing.T) {
		corrupted := make([]byte, len(validData))
		copy(corrupted, validData)
		// Set huge count at offset len-8
		binary.PutUint32(corrupted[len(corrupted)-8:len(corrupted)-4], 0x7FFFFFFF)
		// Recalculate CRC over prefix to isolate count validation logic
		newCRC := binary.Checksum(corrupted[:len(corrupted)-4])
		binary.PutUint32(corrupted[len(corrupted)-4:], newCRC)

		_, err := sstable.DecodeBlockIndex(corrupted)
		if !stdErrors.Is(err, errors.ErrIndexBlockCorrupted) {
			t.Fatalf("expected ErrIndexBlockCorrupted for oversized count, got %v", err)
		}
	})

	t.Run("non-zero first offset rejected", func(t *testing.T) {
		// Valid index has 3 entries: offsetsStart is len - 8 - 12 = len - 20
		corrupted := make([]byte, len(validData))
		copy(corrupted, validData)
		offsetsStart := len(corrupted) - 8 - (3 * 4)
		binary.PutUint32(corrupted[offsetsStart:offsetsStart+4], 5) // offset 0 should be 0, set to 5
		newCRC := binary.Checksum(corrupted[:len(corrupted)-4])
		binary.PutUint32(corrupted[len(corrupted)-4:], newCRC)

		_, err := sstable.DecodeBlockIndex(corrupted)
		if !stdErrors.Is(err, errors.ErrIndexBlockCorrupted) {
			t.Fatalf("expected ErrIndexBlockCorrupted for non-zero first offset, got %v", err)
		}
	})

	t.Run("non-monotonic offsets rejected", func(t *testing.T) {
		corrupted := make([]byte, len(validData))
		copy(corrupted, validData)
		offsetsStart := len(corrupted) - 8 - (3 * 4)
		// offset 0 is 0; set offset 1 to 0 as well (non-increasing)
		binary.PutUint32(corrupted[offsetsStart+4:offsetsStart+8], 0)
		newCRC := binary.Checksum(corrupted[:len(corrupted)-4])
		binary.PutUint32(corrupted[len(corrupted)-4:], newCRC)

		_, err := sstable.DecodeBlockIndex(corrupted)
		if !stdErrors.Is(err, errors.ErrIndexBlockCorrupted) {
			t.Fatalf("expected ErrIndexBlockCorrupted for non-monotonic offsets, got %v", err)
		}
	})
}
