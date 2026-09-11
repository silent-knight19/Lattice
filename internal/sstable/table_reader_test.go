package sstable_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// helperBuildSSTable creates an SSTable file with the provided entries.
func helperBuildSSTable(t *testing.T, dir string, filename string, entries []struct {
	ik  binary.InternalKey
	val []byte
}) string {
	t.Helper()
	path := filepath.Join(dir, filename)
	writer, err := sstable.NewTableWriter(path, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("failed to create TableWriter: %v", err)
	}

	for _, e := range entries {
		if err := writer.Add(e.ik, e.val); err != nil {
			t.Fatalf("failed to add entry %s: %v", e.ik.String(), err)
		}
	}

	_, err = writer.Finish()
	if err != nil {
		t.Fatalf("failed to finalize TableWriter: %v", err)
	}
	return path
}

func TestTableReader_Initialization(t *testing.T) {
	tempDir := t.TempDir()

	t.Run("open_valid_sstable", func(t *testing.T) {
		path := helperBuildSSTable(t, tempDir, "valid.sst", []struct {
			ik  binary.InternalKey
			val []byte
		}{
			{ik: binary.InternalKey{UserKey: []byte("k1"), SeqNum: 10, OpType: binary.OpTypePut}, val: []byte("v1")},
		})

		reader, err := sstable.NewTableReader(path)
		if err != nil {
			t.Fatalf("failed to open TableReader: %v", err)
		}
		defer func() { _ = reader.Close() }()

		if reader.FileSize() <= 0 {
			t.Errorf("expected positive FileSize, got %d", reader.FileSize())
		}
		if reader.Index() == nil || reader.Index().EntryCount() == 0 {
			t.Errorf("expected non-empty index")
		}
	})

	t.Run("open_nonexistent_file", func(t *testing.T) {
		_, err := sstable.NewTableReader(filepath.Join(tempDir, "nonexistent.sst"))
		if err == nil {
			t.Fatalf("expected error opening nonexistent file, got nil")
		}
	})

	t.Run("open_empty_file", func(t *testing.T) {
		emptyPath := filepath.Join(tempDir, "empty.sst")
		if err := os.WriteFile(emptyPath, []byte{}, 0644); err != nil {
			t.Fatalf("failed to create empty file: %v", err)
		}

		_, err := sstable.NewTableReader(emptyPath)
		if err == nil {
			t.Fatalf("expected error opening empty file, got nil")
		}
		if !stdErrors.Is(err, errors.ErrInvalidFooter) && !stdErrors.Is(err, errors.ErrFooterTruncated) {
			t.Errorf("expected footer truncated/invalid error, got %v", err)
		}
	})

	t.Run("open_truncated_file_less_than_48_bytes", func(t *testing.T) {
		truncPath := filepath.Join(tempDir, "truncated.sst")
		if err := os.WriteFile(truncPath, bytes.Repeat([]byte{0xFF}, 30), 0644); err != nil {
			t.Fatalf("failed to create truncated file: %v", err)
		}

		_, err := sstable.NewTableReader(truncPath)
		if err == nil {
			t.Fatalf("expected error opening truncated file, got nil")
		}
		if !stdErrors.Is(err, errors.ErrInvalidFooter) {
			t.Errorf("expected ErrInvalidFooter, got %v", err)
		}
	})

	t.Run("open_empty_sstable_finalized_with_zero_records", func(t *testing.T) {
		emptySSTPath := filepath.Join(tempDir, "empty_sstable.sst")
		writer, err := sstable.NewTableWriter(emptySSTPath, sstable.DefaultTableWriterOptions())
		if err != nil {
			t.Fatalf("failed to create TableWriter: %v", err)
		}
		_, err = writer.Finish()
		if err != nil {
			t.Fatalf("failed to finalize empty TableWriter: %v", err)
		}

		reader, err := sstable.NewTableReader(emptySSTPath)
		if err != nil {
			t.Fatalf("failed to open TableReader on empty SSTable: %v", err)
		}
		defer func() { _ = reader.Close() }()

		val, err := reader.Seek([]byte("any_key"))
		if !stdErrors.Is(err, errors.ErrKeyNotFound) {
			t.Errorf("expected ErrKeyNotFound on empty SSTable, got val=%v, err=%v", val, err)
		}
	})

	t.Run("nil_file_rejected", func(t *testing.T) {
		_, err := sstable.NewTableReaderWithFile(nil)
		if !stdErrors.Is(err, errors.ErrNilReceiver) {
			t.Errorf("expected ErrNilReceiver, got %v", err)
		}
	})
}

func TestTableReader_FooterValidation(t *testing.T) {
	tempDir := t.TempDir()

	// Build a baseline valid SSTable
	baselinePath := helperBuildSSTable(t, tempDir, "footer_baseline.sst", []struct {
		ik  binary.InternalKey
		val []byte
	}{
		{ik: binary.InternalKey{UserKey: []byte("alpha"), SeqNum: 10, OpType: binary.OpTypePut}, val: []byte("val_alpha")},
	})

	rawBytes, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatalf("failed to read baseline SSTable: %v", err)
	}

	t.Run("corrupt_magic_number", func(t *testing.T) {
		corrupted := make([]byte, len(rawBytes))
		copy(corrupted, rawBytes)

		// Magic number is the last 8 bytes of the file
		corrupted[len(corrupted)-1] ^= 0xFF

		badPath := filepath.Join(tempDir, "bad_magic.sst")
		if err := os.WriteFile(badPath, corrupted, 0644); err != nil {
			t.Fatalf("failed to write corrupted file: %v", err)
		}

		_, err := sstable.NewTableReader(badPath)
		if err == nil {
			t.Fatalf("expected error on bad footer magic, got nil")
		}
		if !stdErrors.Is(err, errors.ErrInvalidFooterMagic) && !stdErrors.Is(err, errors.ErrInvalidFooter) {
			t.Errorf("expected ErrInvalidFooterMagic, got %v", err)
		}
	})

	t.Run("corrupt_padding", func(t *testing.T) {
		corrupted := make([]byte, len(rawBytes))
		copy(corrupted, rawBytes)

		// Padding is bytes 32..39 of the 48-byte footer (i.e. len - 16 .. len - 9)
		paddingBytePos := len(corrupted) - 16
		corrupted[paddingBytePos] = 0x01

		badPath := filepath.Join(tempDir, "bad_padding.sst")
		if err := os.WriteFile(badPath, corrupted, 0644); err != nil {
			t.Fatalf("failed to write corrupted file: %v", err)
		}

		_, err := sstable.NewTableReader(badPath)
		if err == nil {
			t.Fatalf("expected error on bad footer padding, got nil")
		}
		if !stdErrors.Is(err, errors.ErrInvalidFooterPadding) && !stdErrors.Is(err, errors.ErrInvalidFooter) {
			t.Errorf("expected ErrInvalidFooterPadding, got %v", err)
		}
	})

	t.Run("corrupt_index_handle_out_of_bounds", func(t *testing.T) {
		corrupted := make([]byte, len(rawBytes))
		copy(corrupted, rawBytes)

		// IndexHandle is bytes 16..31 of footer (len - 32 .. len - 17)
		// Offset is 8 bytes Big-Endian (len - 32 .. len - 25)
		binary.PutUint64(corrupted[len(corrupted)-32:len(corrupted)-24], 99999999)

		badPath := filepath.Join(tempDir, "bad_index_handle.sst")
		if err := os.WriteFile(badPath, corrupted, 0644); err != nil {
			t.Fatalf("failed to write corrupted file: %v", err)
		}

		_, err := sstable.NewTableReader(badPath)
		if err == nil {
			t.Fatalf("expected error on out-of-bounds index handle, got nil")
		}
		if !stdErrors.Is(err, errors.ErrInvalidBlockHandle) {
			t.Errorf("expected ErrInvalidBlockHandle, got %v", err)
		}
	})
}

func TestTableReader_IndexValidation(t *testing.T) {
	tempDir := t.TempDir()

	baselinePath := helperBuildSSTable(t, tempDir, "index_baseline.sst", []struct {
		ik  binary.InternalKey
		val []byte
	}{
		{ik: binary.InternalKey{UserKey: []byte("key1"), SeqNum: 1, OpType: binary.OpTypePut}, val: []byte("val1")},
		{ik: binary.InternalKey{UserKey: []byte("key2"), SeqNum: 2, OpType: binary.OpTypePut}, val: []byte("val2")},
	})

	reader, err := sstable.NewTableReader(baselinePath)
	if err != nil {
		t.Fatalf("failed to open baseline SSTable: %v", err)
	}
	indexHandle := reader.Footer().IndexHandle
	_ = reader.Close()

	rawBytes, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatalf("failed to read baseline bytes: %v", err)
	}

	t.Run("corrupt_index_checksum", func(t *testing.T) {
		corrupted := make([]byte, len(rawBytes))
		copy(corrupted, rawBytes)

		// Flip a byte in the index block
		corrupted[indexHandle.Offset] ^= 0xFF

		badPath := filepath.Join(tempDir, "bad_index_crc.sst")
		if err := os.WriteFile(badPath, corrupted, 0644); err != nil {
			t.Fatalf("failed to write corrupted file: %v", err)
		}

		_, err := sstable.NewTableReader(badPath)
		if err == nil {
			t.Fatalf("expected error on corrupted index CRC, got nil")
		}
		if !stdErrors.Is(err, errors.ErrChecksumMismatch) {
			t.Errorf("expected ErrChecksumMismatch, got %v", err)
		}
	})
}

func TestTableReader_PointLookup_SingleBlock(t *testing.T) {
	tempDir := t.TempDir()

	keys := []string{"apple", "banana", "cherry", "date", "elderberry", "fig", "grape"}
	entries := make([]struct {
		ik  binary.InternalKey
		val []byte
	}, len(keys))

	for i, k := range keys {
		entries[i] = struct {
			ik  binary.InternalKey
			val []byte
		}{
			ik:  binary.InternalKey{UserKey: []byte(k), SeqNum: binary.SeqNum(100 + i), OpType: binary.OpTypePut},
			val: []byte("value_" + k),
		}
	}

	path := helperBuildSSTable(t, tempDir, "single_block.sst", entries)

	reader, err := sstable.NewTableReader(path)
	if err != nil {
		t.Fatalf("failed to open TableReader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	// 1. Verify exact lookups for every key
	for _, k := range keys {
		val, err := reader.Seek([]byte(k))
		if err != nil {
			t.Fatalf("failed to seek key %q: %v", k, err)
		}
		expected := "value_" + k
		if string(val) != expected {
			t.Errorf("key %q: expected value %q, got %q", k, expected, string(val))
		}
	}

	// 2. Lookup first key
	firstVal, err := reader.Seek([]byte(keys[0]))
	if err != nil || string(firstVal) != "value_apple" {
		t.Errorf("first key lookup mismatch: val=%q, err=%v", string(firstVal), err)
	}

	// 3. Lookup last key
	lastVal, err := reader.Seek([]byte(keys[len(keys)-1]))
	if err != nil || string(lastVal) != "value_grape" {
		t.Errorf("last key lookup mismatch: val=%q, err=%v", string(lastVal), err)
	}

	// 4. Absent key before first key
	val, err := reader.Seek([]byte("aardvark"))
	if !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("expected ErrKeyNotFound for key before first, got val=%v, err=%v", val, err)
	}

	// 5. Absent key between existing keys
	val, err = reader.Seek([]byte("blueberry"))
	if !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("expected ErrKeyNotFound for key between existing, got val=%v, err=%v", val, err)
	}

	// 6. Absent key after last key (proves sparse index false return with zero block reads)
	val, err = reader.Seek([]byte("zebra"))
	if !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("expected ErrKeyNotFound for key after last, got val=%v, err=%v", val, err)
	}

	// 7. Invalid key constraints
	_, err = reader.Seek([]byte{})
	if !stdErrors.Is(err, errors.ErrEmptyKey) {
		t.Errorf("expected ErrEmptyKey, got %v", err)
	}
	_, err = reader.Seek(nil)
	if !stdErrors.Is(err, errors.ErrEmptyKey) {
		t.Errorf("expected ErrEmptyKey for nil key, got %v", err)
	}
}

func TestTableReader_PointLookup_MultiBlock(t *testing.T) {
	tempDir := t.TempDir()

	// Generate 1,000 keys to force multiple 4KB data blocks
	totalKeys := 1000
	entries := make([]struct {
		ik  binary.InternalKey
		val []byte
	}, totalKeys)

	for i := 0; i < totalKeys; i++ {
		keyStr := fmt.Sprintf("key_%06d", i)
		valStr := fmt.Sprintf("val_%06d_%s", i, bytes.Repeat([]byte("x"), 50))
		entries[i] = struct {
			ik  binary.InternalKey
			val []byte
		}{
			ik:  binary.InternalKey{UserKey: []byte(keyStr), SeqNum: binary.SeqNum(i + 1), OpType: binary.OpTypePut},
			val: []byte(valStr),
		}
	}

	path := helperBuildSSTable(t, tempDir, "multi_block.sst", entries)

	reader, err := sstable.NewTableReader(path)
	if err != nil {
		t.Fatalf("failed to open TableReader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	if reader.Index().EntryCount() <= 1 {
		t.Fatalf("expected multiple data blocks in index, got %d", reader.Index().EntryCount())
	}

	// Verify all 1,000 keys return 100% correct values
	for i := 0; i < totalKeys; i++ {
		keyStr := fmt.Sprintf("key_%06d", i)
		expectedVal := fmt.Sprintf("val_%06d_%s", i, bytes.Repeat([]byte("x"), 50))

		val, err := reader.Seek([]byte(keyStr))
		if err != nil {
			t.Fatalf("failed to seek key %q at index %d: %v", keyStr, i, err)
		}
		if string(val) != expectedVal {
			t.Fatalf("key %q value mismatch: expected len %d, got len %d", keyStr, len(expectedVal), len(val))
		}
	}

	// Verify absent keys
	absentProbes := []string{
		"key_000000_a",
		"key_000500_mid",
		"key_999999",
		"aaa_before",
		"zzz_after",
	}
	for _, probe := range absentProbes {
		val, err := reader.Seek([]byte(probe))
		if !stdErrors.Is(err, errors.ErrKeyNotFound) {
			t.Errorf("probe %q: expected ErrKeyNotFound, got val=%v, err=%v", probe, val, err)
		}
	}
}

func TestTableReader_RevisionsAndTombstones(t *testing.T) {
	tempDir := t.TempDir()

	// Canonical LSM ordering requires:
	// Key ASC, SeqNum DESC, OpType DESC
	entries := []struct {
		ik  binary.InternalKey
		val []byte
	}{
		// Key "k1" has two PUT versions: seq 20 (v2) and seq 10 (v1)
		{ik: binary.InternalKey{UserKey: []byte("k1"), SeqNum: 20, OpType: binary.OpTypePut}, val: []byte("v2")},
		{ik: binary.InternalKey{UserKey: []byte("k1"), SeqNum: 10, OpType: binary.OpTypePut}, val: []byte("v1")},

		// Key "k2" was deleted at seq 30, but had an older PUT at seq 15
		{ik: binary.InternalKey{UserKey: []byte("k2"), SeqNum: 30, OpType: binary.OpTypeDelete}, val: []byte{}},
		{ik: binary.InternalKey{UserKey: []byte("k2"), SeqNum: 15, OpType: binary.OpTypePut}, val: []byte("v_old")},

		// Key "k3" is a normal PUT
		{ik: binary.InternalKey{UserKey: []byte("k3"), SeqNum: 25, OpType: binary.OpTypePut}, val: []byte("v3")},
	}

	path := helperBuildSSTable(t, tempDir, "revisions.sst", entries)

	reader, err := sstable.NewTableReader(path)
	if err != nil {
		t.Fatalf("failed to open TableReader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	// 1. "k1" must return latest version "v2", NOT older "v1"
	val1, err := reader.Seek([]byte("k1"))
	if err != nil {
		t.Fatalf("seek k1 failed: %v", err)
	}
	if string(val1) != "v2" {
		t.Errorf("k1: expected latest value 'v2', got %q", string(val1))
	}

	// 2. "k2" must return ErrKeyNotFound due to tombstone, NOT older "v_old"
	val2, err := reader.Seek([]byte("k2"))
	if !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("k2: expected ErrKeyNotFound for tombstone, got val=%q, err=%v", string(val2), err)
	}

	// 3. "k3" must return "v3"
	val3, err := reader.Seek([]byte("k3"))
	if err != nil || string(val3) != "v3" {
		t.Errorf("k3: expected 'v3', got val=%q, err=%v", string(val3), err)
	}
}

func TestTableReader_DataBlockCorruption(t *testing.T) {
	tempDir := t.TempDir()

	path := helperBuildSSTable(t, tempDir, "data_corruption.sst", []struct {
		ik  binary.InternalKey
		val []byte
	}{
		{ik: binary.InternalKey{UserKey: []byte("foo"), SeqNum: 1, OpType: binary.OpTypePut}, val: []byte("bar")},
	})

	reader, err := sstable.NewTableReader(path)
	if err != nil {
		t.Fatalf("failed to open TableReader: %v", err)
	}
	handle, found := reader.Index().FindBlock([]byte("foo"))
	if !found {
		t.Fatalf("failed to find data block for foo")
	}
	_ = reader.Close()

	rawBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read SSTable bytes: %v", err)
	}

	t.Run("data_block_crc_mismatch", func(t *testing.T) {
		corrupted := make([]byte, len(rawBytes))
		copy(corrupted, rawBytes)

		// Flip byte inside the data block payload
		corrupted[handle.Offset] ^= 0xFF

		badPath := filepath.Join(tempDir, "bad_data_crc.sst")
		if err := os.WriteFile(badPath, corrupted, 0644); err != nil {
			t.Fatalf("failed to write corrupted file: %v", err)
		}

		r, err := sstable.NewTableReader(badPath)
		if err != nil {
			t.Fatalf("failed to open reader: %v", err)
		}
		defer func() { _ = r.Close() }()

		_, seekErr := r.Seek([]byte("foo"))
		if seekErr == nil {
			t.Fatalf("expected error on corrupted data block CRC, got nil")
		}
		if !stdErrors.Is(seekErr, errors.ErrChecksumMismatch) {
			t.Errorf("expected ErrChecksumMismatch, got %v", seekErr)
		}
	})

	t.Run("data_block_corrupted_restart_count", func(t *testing.T) {
		// Test SearchDataBlockForTesting with crafted corrupted buffers
		block := make([]byte, 16)
		// Restart count = 0 (at len - 8)
		binary.PutUint32(block[8:12], 0)
		crc := binary.Checksum(block[:12])
		binary.PutUint32(block[12:16], crc)

		_, err := sstable.SearchDataBlockForTesting(block, []byte("target"), 0)
		if err == nil {
			t.Fatalf("expected error for zero restart count, got nil")
		}
		if !stdErrors.Is(err, errors.ErrDataBlockCorrupted) {
			t.Errorf("expected ErrDataBlockCorrupted, got %v", err)
		}
	})

	t.Run("data_block_truncated_buffer", func(t *testing.T) {
		shortBlock := []byte{0x01, 0x02}
		_, err := sstable.SearchDataBlockForTesting(shortBlock, []byte("target"), 0)
		if err == nil {
			t.Fatalf("expected error for truncated data block, got nil")
		}
		if !stdErrors.Is(err, errors.ErrDataBlockCorrupted) {
			t.Errorf("expected ErrDataBlockCorrupted, got %v", err)
		}
	})
}

func TestTableReader_LifecycleAndResourceManagement(t *testing.T) {
	tempDir := t.TempDir()

	path := helperBuildSSTable(t, tempDir, "lifecycle.sst", []struct {
		ik  binary.InternalKey
		val []byte
	}{
		{ik: binary.InternalKey{UserKey: []byte("k"), SeqNum: 1, OpType: binary.OpTypePut}, val: []byte("v")},
	})

	reader, err := sstable.NewTableReader(path)
	if err != nil {
		t.Fatalf("failed to open reader: %v", err)
	}

	// 1. Initial lookup succeeds
	val, err := reader.Seek([]byte("k"))
	if err != nil || string(val) != "v" {
		t.Fatalf("initial seek failed: val=%q, err=%v", string(val), err)
	}

	// 2. Close releases descriptor
	if err := reader.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}

	// 3. Repeated Close is idempotent and returns nil
	if err := reader.Close(); err != nil {
		t.Fatalf("repeated close failed: %v", err)
	}

	// 4. Seek after close returns ErrTableReaderClosed
	_, err = reader.Seek([]byte("k"))
	if !stdErrors.Is(err, errors.ErrTableReaderClosed) {
		t.Errorf("expected ErrTableReaderClosed after close, got %v", err)
	}

	// 5. Nil receiver safety
	var nilReader *sstable.TableReader
	if err := nilReader.Close(); !stdErrors.Is(err, errors.ErrNilReceiver) {
		t.Errorf("expected ErrNilReceiver on nil Close, got %v", err)
	}
	if _, err := nilReader.Seek([]byte("k")); !stdErrors.Is(err, errors.ErrNilReceiver) {
		t.Errorf("expected ErrNilReceiver on nil Seek, got %v", err)
	}
}

func TestTableReader_ConcurrentSeek(t *testing.T) {
	tempDir := t.TempDir()

	// Build an SSTable with 200 keys
	count := 200
	entries := make([]struct {
		ik  binary.InternalKey
		val []byte
	}, count)

	for i := 0; i < count; i++ {
		entries[i] = struct {
			ik  binary.InternalKey
			val []byte
		}{
			ik:  binary.InternalKey{UserKey: []byte(fmt.Sprintf("ckey_%04d", i)), SeqNum: binary.SeqNum(i + 1), OpType: binary.OpTypePut},
			val: []byte(fmt.Sprintf("cval_%04d", i)),
		}
	}

	path := helperBuildSSTable(t, tempDir, "concurrent.sst", entries)

	reader, err := sstable.NewTableReader(path)
	if err != nil {
		t.Fatalf("failed to open reader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	// Launch 20 concurrent goroutines querying random keys
	goroutines := 20
	queriesPerGoroutine := 100

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(gid int) {
			defer wg.Done()
			for q := 0; q < queriesPerGoroutine; q++ {
				idx := (gid*queriesPerGoroutine + q) % count
				keyStr := fmt.Sprintf("ckey_%04d", idx)
				expectedVal := fmt.Sprintf("cval_%04d", idx)

				val, err := reader.Seek([]byte(keyStr))
				if err != nil {
					t.Errorf("concurrent seek failed for key %q: %v", keyStr, err)
					return
				}
				if string(val) != expectedVal {
					t.Errorf("concurrent seek value mismatch: expected %q, got %q", expectedVal, string(val))
					return
				}

				// Also test concurrent absent key probe
				_, err = reader.Seek([]byte("nonexistent_key"))
				if !stdErrors.Is(err, errors.ErrKeyNotFound) {
					t.Errorf("expected ErrKeyNotFound for absent probe, got %v", err)
					return
				}
			}
		}(g)
	}

	wg.Wait()
}

func TestTableReader_DifferentialTesting_ReferenceModel(t *testing.T) {
	tempDir := t.TempDir()

	// Reference model: in-memory map
	refModel := make(map[string][]byte)

	// Apply deterministic sequence of PUT and DELETE operations
	keyCount := 300
	entries := make([]struct {
		ik  binary.InternalKey
		val []byte
	}, 0, keyCount)

	for i := 0; i < keyCount; i++ {
		keyStr := fmt.Sprintf("diff_key_%05d", i)
		if i%7 == 0 {
			// Tombstone deletion
			entries = append(entries, struct {
				ik  binary.InternalKey
				val []byte
			}{
				ik:  binary.InternalKey{UserKey: []byte(keyStr), SeqNum: binary.SeqNum(i + 1), OpType: binary.OpTypeDelete},
				val: []byte{},
			})
			// Absent from reference model
		} else {
			val := []byte(fmt.Sprintf("diff_val_%05d", i))
			refModel[keyStr] = val
			entries = append(entries, struct {
				ik  binary.InternalKey
				val []byte
			}{
				ik:  binary.InternalKey{UserKey: []byte(keyStr), SeqNum: binary.SeqNum(i + 1), OpType: binary.OpTypePut},
				val: val,
			})
		}
	}

	path := helperBuildSSTable(t, tempDir, "differential.sst", entries)

	reader, err := sstable.NewTableReader(path)
	if err != nil {
		t.Fatalf("failed to open reader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	// Compare reference model against TableReader.Seek
	for i := 0; i < keyCount; i++ {
		keyStr := fmt.Sprintf("diff_key_%05d", i)
		expectedVal, exists := refModel[keyStr]

		actualVal, err := reader.Seek([]byte(keyStr))
		if exists {
			if err != nil {
				t.Fatalf("key %q: expected value, got error %v", keyStr, err)
			}
			if !bytes.Equal(actualVal, expectedVal) {
				t.Fatalf("key %q: value mismatch: expected %q, got %q", keyStr, expectedVal, actualVal)
			}
		} else {
			if !stdErrors.Is(err, errors.ErrKeyNotFound) {
				t.Fatalf("key %q: expected ErrKeyNotFound, got val=%q, err=%v", keyStr, actualVal, err)
			}
		}
	}
}

func TestTableReader_100000Keys_Verification(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 100,000-key test in short mode")
	}

	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "100k.sst")

	totalKeys := 100000
	t.Logf("Building SSTable with %d keys...", totalKeys)

	writer, err := sstable.NewTableWriter(path, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("failed to create TableWriter: %v", err)
	}

	for i := 0; i < totalKeys; i++ {
		key := binary.InternalKey{
			UserKey: []byte(fmt.Sprintf("user_key_%08d", i)),
			SeqNum:  binary.SeqNum(i + 1),
			OpType:  binary.OpTypePut,
		}
		val := []byte(fmt.Sprintf("user_val_%08d", i))

		if err := writer.Add(key, val); err != nil {
			t.Fatalf("failed to add key %d: %v", i, err)
		}
	}

	meta, err := writer.Finish()
	if err != nil {
		t.Fatalf("failed to finalize SSTable: %v", err)
	}

	t.Logf("SSTable built successfully: entries=%d, data_blocks=%d, file_size=%d",
		meta.EntryCount, meta.DataBlockCount, meta.FileSize)

	// Open reader
	reader, err := sstable.NewTableReader(path)
	if err != nil {
		t.Fatalf("failed to open TableReader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	if reader.Index().EntryCount() != int(meta.DataBlockCount) {
		t.Errorf("index entry count mismatch: expected %d, got %d", meta.DataBlockCount, reader.Index().EntryCount())
	}

	t.Logf("Verifying 100%% point lookup correctness across all %d keys...", totalKeys)

	correctCount := 0
	for i := 0; i < totalKeys; i++ {
		keyStr := fmt.Sprintf("user_key_%08d", i)
		expectedVal := fmt.Sprintf("user_val_%08d", i)

		val, err := reader.Seek([]byte(keyStr))
		if err != nil {
			t.Fatalf("lookup failed for key %q at index %d: %v", keyStr, i, err)
		}
		if string(val) != expectedVal {
			t.Fatalf("value mismatch for key %q: expected %q, got %q", keyStr, expectedVal, string(val))
		}
		correctCount++
	}

	if correctCount != totalKeys {
		t.Fatalf("expected %d correct lookups, got %d", totalKeys, correctCount)
	}

	t.Logf("Verification completed: %d / %d keys verified (100.0%% correct)", correctCount, totalKeys)

	// Verify absent keys
	absentProbes := []string{
		"user_key_00000000_absent",
		"user_key_00049999_mid",
		"user_key_00099999_after",
		"user_key_99999999",
		"aaa_before_all",
		"zzz_after_all",
	}

	for _, probe := range absentProbes {
		val, err := reader.Seek([]byte(probe))
		if !stdErrors.Is(err, errors.ErrKeyNotFound) {
			t.Errorf("expected ErrKeyNotFound for absent probe %q, got val=%v, err=%v", probe, val, err)
		}
	}
}
