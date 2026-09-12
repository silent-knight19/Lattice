package sstable_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/filter"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// TestSSTable_FilterBlockIntegration tests full SSTable end-to-end integration of the Bloom filter block.
func TestSSTable_FilterBlockIntegration(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "000001.sst")

	keyCount := 500
	keys := make([][]byte, keyCount)
	for i := 0; i < keyCount; i++ {
		keys[i] = []byte(fmt.Sprintf("user_key_%06d", i))
	}

	filterBuilder := filter.NewFilterBlockBuilder(keyCount)
	opts := sstable.DefaultTableWriterOptions()
	opts.FilterBuilder = filterBuilder

	writer, err := sstable.NewTableWriter(sstPath, opts)
	if err != nil {
		t.Fatalf("failed to create table writer: %v", err)
	}

	for i, k := range keys {
		ik := binary.InternalKey{
			UserKey: k,
			SeqNum:  binary.SeqNum(i + 1),
			OpType:  binary.OpTypePut,
		}
		val := []byte(fmt.Sprintf("val_%06d", i))
		if err := writer.Add(ik, val); err != nil {
			t.Fatalf("failed to add key %d: %v", i, err)
		}
	}

	meta, err := writer.Finish()
	if err != nil {
		t.Fatalf("failed to finish table writer: %v", err)
	}

	// 1. Verify physical region invariants
	// Layout: [Data Blocks] -> [Filter Block] -> [MetaIndex Block] -> [Index Block] -> [48-Byte Footer]
	if meta.FilterHandle.Size == 0 {
		t.Fatalf("expected non-zero FilterHandle size, got 0")
	}

	rawBytes, err := os.ReadFile(sstPath)
	if err != nil {
		t.Fatalf("failed to read SSTable file: %v", err)
	}
	fileSize := uint64(len(rawBytes))
	if fileSize != meta.FileSize {
		t.Fatalf("file size mismatch: raw=%d, meta=%d", fileSize, meta.FileSize)
	}

	footerOffset := fileSize - uint64(sstable.FooterSize)
	footerBytes := rawBytes[footerOffset:]
	footer, err := sstable.DecodeFooter(footerBytes)
	if err != nil {
		t.Fatalf("failed to decode footer: %v", err)
	}

	// Verify non-overlapping contiguous boundaries:
	// Filter block ends at MetaIndex block offset
	filterEnd := meta.FilterHandle.Offset + meta.FilterHandle.Size
	if filterEnd != footer.MetaIndexHandle.Offset {
		t.Fatalf("region overlap/gap: filter end %d != metaindex offset %d", filterEnd, footer.MetaIndexHandle.Offset)
	}

	// MetaIndex block ends at Index block offset
	metaEnd := footer.MetaIndexHandle.Offset + footer.MetaIndexHandle.Size
	if metaEnd != footer.IndexHandle.Offset {
		t.Fatalf("region overlap/gap: metaindex end %d != index offset %d", metaEnd, footer.IndexHandle.Offset)
	}

	// Index block ends at Footer offset
	indexEnd := footer.IndexHandle.Offset + footer.IndexHandle.Size
	if indexEnd != footerOffset {
		t.Fatalf("region overlap/gap: index end %d != footer offset %d", indexEnd, footerOffset)
	}

	// 2. Open with TableReader and verify ReadFilterBlock
	reader, err := sstable.OpenTableReader(sstPath)
	if err != nil {
		t.Fatalf("failed to open table reader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	bf, err := reader.ReadFilterBlock()
	if err != nil {
		t.Fatalf("ReadFilterBlock failed: %v", err)
	}
	if bf == nil {
		t.Fatalf("expected non-nil BloomFilter from ReadFilterBlock")
	}

	// Verify all keys are present in filter (zero false negatives)
	for _, k := range keys {
		if !bf.MayContain(k) {
			t.Fatalf("Bloom filter false negative on key %q", k)
		}
	}

	// Verify absent key
	absentKey := []byte("non_existent_user_key_999999")
	if bf.MayContain(absentKey) {
		t.Logf("informational: false positive on %q (probabilistically possible)", absentKey)
	}

	// Normal point lookup continues to work via Seek
	val, err := reader.Seek(keys[0])
	if err != nil {
		t.Fatalf("failed to seek key 0: %v", err)
	}
	if !bytes.Equal(val, []byte("val_000000")) {
		t.Fatalf("unexpected value: got %q, want %q", val, "val_000000")
	}
}

// TestSSTable_NoFilter_BackwardCompatibility verifies that SSTables written without a filter
// behave identically to Phase 04 tables and ReadFilterBlock returns (nil, nil).
func TestSSTable_NoFilter_BackwardCompatibility(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "000002.sst")

	opts := sstable.DefaultTableWriterOptions() // FilterBuilder is nil
	writer, err := sstable.NewTableWriter(sstPath, opts)
	if err != nil {
		t.Fatalf("failed to create table writer: %v", err)
	}

	ik := binary.InternalKey{
		UserKey: []byte("foo"),
		SeqNum:  1,
		OpType:  binary.OpTypePut,
	}
	if err := writer.Add(ik, []byte("bar")); err != nil {
		t.Fatalf("failed to add: %v", err)
	}

	meta, err := writer.Finish()
	if err != nil {
		t.Fatalf("failed to finish: %v", err)
	}

	if meta.FilterHandle != (sstable.BlockHandle{}) {
		t.Fatalf("expected zero FilterHandle, got %+v", meta.FilterHandle)
	}
	if meta.MetaIndexHandle.Size != sstable.MetaIndexTrailerSize {
		t.Fatalf("expected 8-byte empty metaindex block, got %d", meta.MetaIndexHandle.Size)
	}

	reader, err := sstable.OpenTableReader(sstPath)
	if err != nil {
		t.Fatalf("failed to open reader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	bf, err := reader.ReadFilterBlock()
	if err != nil {
		t.Fatalf("expected nil error for table without filter, got: %v", err)
	}
	if bf != nil {
		t.Fatalf("expected nil BloomFilter for table without filter, got %+v", bf)
	}
}

// TestSSTable_CorruptedFilterBlockOnDisk verifies fail-closed behavior when the filter block
// payload on disk has been corrupted.
func TestSSTable_CorruptedFilterBlockOnDisk(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "000003.sst")

	filterBuilder := filter.NewFilterBlockBuilder(20)
	opts := sstable.DefaultTableWriterOptions()
	opts.FilterBuilder = filterBuilder

	writer, err := sstable.NewTableWriter(sstPath, opts)
	if err != nil {
		t.Fatalf("failed to create writer: %v", err)
	}

	for i := 0; i < 20; i++ {
		ik := binary.InternalKey{
			UserKey: []byte(fmt.Sprintf("corrupt_test_%d", i)),
			SeqNum:  binary.SeqNum(i + 1),
			OpType:  binary.OpTypePut,
		}
		_ = writer.Add(ik, []byte("v"))
	}

	meta, err := writer.Finish()
	if err != nil {
		t.Fatalf("failed to finish: %v", err)
	}

	// Corrupt one byte inside the filter block region on disk
	data, err := os.ReadFile(sstPath)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}

	corruptOffset := meta.FilterHandle.Offset + 2
	data[corruptOffset] ^= 0xFF

	corruptPath := filepath.Join(dir, "000003_corrupted.sst")
	if err := os.WriteFile(corruptPath, data, 0600); err != nil {
		t.Fatalf("failed to write corrupted sstable: %v", err)
	}

	reader, err := sstable.OpenTableReader(corruptPath)
	if err != nil {
		t.Fatalf("failed to open corrupted table reader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	// ReadFilterBlock must fail closed and return ChecksumMismatchError
	bf, err := reader.ReadFilterBlock()
	if err == nil {
		t.Fatalf("expected error reading corrupted filter block, got nil")
	}
	if bf != nil {
		t.Fatalf("expected nil filter on corruption, got %+v", bf)
	}
	if !stdErrors.Is(err, errors.ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch, got: %v", err)
	}
}

// TestMetaIndexBlock_Codec tests MetaIndex block serialization and decoding.
func TestMetaIndexBlock_Codec(t *testing.T) {
	t.Run("EmptyMetaIndexBlock", func(t *testing.T) {
		emptyBytes := sstable.BuildMetaIndexBlock(nil)
		if len(emptyBytes) != sstable.MetaIndexTrailerSize {
			t.Fatalf("expected %d bytes, got %d", sstable.MetaIndexTrailerSize, len(emptyBytes))
		}

		decoded, err := sstable.DecodeMetaIndexBlock(emptyBytes)
		if err != nil {
			t.Fatalf("failed to decode empty metaindex: %v", err)
		}
		if len(decoded) != 0 {
			t.Fatalf("expected 0 entries in decoded map, got %d", len(decoded))
		}
	})

	t.Run("SingleEntry", func(t *testing.T) {
		entries := map[string]sstable.BlockHandle{
			filter.FilterMetaKey: {Offset: 4096, Size: 128},
		}
		encoded := sstable.BuildMetaIndexBlock(entries)

		decoded, err := sstable.DecodeMetaIndexBlock(encoded)
		if err != nil {
			t.Fatalf("decode failed: %v", err)
		}
		if len(decoded) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(decoded))
		}
		h, ok := decoded[filter.FilterMetaKey]
		if !ok || h != entries[filter.FilterMetaKey] {
			t.Fatalf("entry mismatch: got %+v, want %+v", h, entries[filter.FilterMetaKey])
		}

		// Test FindMetaIndexEntry
		foundHandle, found, err := sstable.FindMetaIndexEntry(encoded, filter.FilterMetaKey)
		if err != nil || !found || foundHandle != entries[filter.FilterMetaKey] {
			t.Fatalf("FindMetaIndexEntry failed: handle=%+v, found=%v, err=%v", foundHandle, found, err)
		}

		// Absent key
		_, found, err = sstable.FindMetaIndexEntry(encoded, "non.existent")
		if err != nil || found {
			t.Fatalf("expected not found for non.existent, got found=%v, err=%v", found, err)
		}
	})

	t.Run("MultipleEntries_DeterministicOrdering", func(t *testing.T) {
		entries := map[string]sstable.BlockHandle{
			"filter.bloom": {Offset: 1000, Size: 50},
			"stats.keys":   {Offset: 1050, Size: 80},
			"compact.info": {Offset: 1130, Size: 40},
		}
		enc1 := sstable.BuildMetaIndexBlock(entries)
		enc2 := sstable.BuildMetaIndexBlock(entries)

		if !bytes.Equal(enc1, enc2) {
			t.Fatalf("metaindex encoding is not deterministic")
		}

		decoded, err := sstable.DecodeMetaIndexBlock(enc1)
		if err != nil {
			t.Fatalf("failed to decode: %v", err)
		}
		if len(decoded) != 3 {
			t.Fatalf("expected 3 entries, got %d", len(decoded))
		}
		for k, expectedHandle := range entries {
			if decoded[k] != expectedHandle {
				t.Errorf("key %q: got handle %+v, want %+v", k, decoded[k], expectedHandle)
			}
		}
	})

	t.Run("CorruptionMatrix", func(t *testing.T) {
		entries := map[string]sstable.BlockHandle{
			"filter.bloom": {Offset: 100, Size: 50},
		}
		valid := sstable.BuildMetaIndexBlock(entries)

		// Truncated buffer (< 8 bytes)
		for l := 0; l < 8; l++ {
			_, err := sstable.DecodeMetaIndexBlock(valid[:l])
			if l == 0 {
				if err != nil {
					t.Errorf("len 0 must return nil error, got %v", err)
				}
			} else {
				if !stdErrors.Is(err, errors.ErrIndexBlockTruncated) {
					t.Errorf("len %d: expected ErrIndexBlockTruncated, got %v", l, err)
				}
			}
		}

		// Corrupted CRC
		corruptedCRC := make([]byte, len(valid))
		copy(corruptedCRC, valid)
		corruptedCRC[len(corruptedCRC)-1] ^= 0xFF
		_, err := sstable.DecodeMetaIndexBlock(corruptedCRC)
		if !stdErrors.Is(err, errors.ErrChecksumMismatch) {
			t.Errorf("expected ErrChecksumMismatch, got %v", err)
		}
	})
}
