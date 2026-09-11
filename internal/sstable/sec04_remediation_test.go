package sstable_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// TestSecurity_Remediation1_SparseIndexTypeConfusion verifies that sparse index lookup
// correctly handles arbitrary binary keys, specifically keys >= 10 bytes ending in 0x01 or 0x02,
// zero bytes, and trailer-like suffixes, across multiple data blocks.
func TestSecurity_Remediation1_SparseIndexTypeConfusion(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "type_confusion.sst")

	// Configure small block size to force multiple data blocks across binary keys
	opts := sstable.DefaultTableWriterOptions()
	opts.TargetBlockSize = 256

	writer, err := sstable.NewTableWriter(sstPath, opts)
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}

	// Generate a sequence of binary user keys including known adversarial patterns
	testKeys := [][]byte{
		// Pattern 1: Short keys
		[]byte("bin:001"),
		[]byte("bin:002"),
		// Pattern 2: 10+ byte keys ending in 0x01 (previously confused with OpTypePut)
		[]byte("adversarial_key_ending_with_\x01"),
		[]byte("adversarial_key_ending_with_\x01_b"),
		// Pattern 3: 10+ byte keys ending in 0x02 (previously confused with OpTypeDelete)
		[]byte("adversarial_key_ending_with_\x02"),
		[]byte("adversarial_key_ending_with_\x02_b"),
		// Pattern 4: Binary keys containing zero bytes
		[]byte("binary\x00with\x00zero\x00bytes\x01"),
		[]byte("binary\x00with\x00zero\x00bytes\x02"),
		// Pattern 5: Key whose last 9 bytes resemble an InternalKey trailer (8B seqnum + 1B op)
		append([]byte("trailer_mimic_"), 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x42, 0x01),
		append([]byte("trailer_mimic_"), 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x42, 0x02),
		// Pattern 6: Arbitrary binary sequences with bytes 0x00..0xFF
		{0x10, 0x20, 0x30, 0x40, 0x50, 0x60, 0x70, 0x80, 0x90, 0x01},
		{0x10, 0x20, 0x30, 0x40, 0x50, 0x60, 0x70, 0x80, 0x90, 0x02},
		{0x20, 0x00, 0xFF, 0xEE, 0xDD, 0xCC, 0xBB, 0xAA, 0x99, 0x01},
		{0x30, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x02},
		// Suffix sentinel
		[]byte("zzzz_sentinel_end"),
	}

	// Sort keys canonically to satisfy monotonic SSTable key ordering
	// Note: SSTable ordering is bytes.Compare for user keys
	for i := 0; i < len(testKeys); i++ {
		for j := i + 1; j < len(testKeys); j++ {
			if bytes.Compare(testKeys[i], testKeys[j]) > 0 {
				testKeys[i], testKeys[j] = testKeys[j], testKeys[i]
			}
		}
	}

	// Write all keys with distinct values
	expectedValues := make(map[string][]byte)
	for i, k := range testKeys {
		val := []byte(fmt.Sprintf("val-%d", i))
		expectedValues[string(k)] = val
		ik, err := binary.NewInternalKey(k, binary.SeqNum(uint64(1000-i)), binary.OpTypePut)
		if err != nil {
			t.Fatalf("NewInternalKey(%q) failed: %v", k, err)
		}
		if err := writer.Add(ik, val); err != nil {
			t.Fatalf("writer.Add(%q) failed: %v", k, err)
		}
	}

	meta, err := writer.Finish()
	if err != nil {
		t.Fatalf("writer.Finish failed: %v", err)
	}
	if meta.DataBlockCount < 2 {
		t.Fatalf("expected multi-block SSTable, got %d blocks", meta.DataBlockCount)
	}

	// Open TableReader and verify point lookups
	reader, err := sstable.NewTableReader(sstPath)
	if err != nil {
		t.Fatalf("NewTableReader failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	t.Run("exact point lookups across all binary keys", func(t *testing.T) {
		for _, k := range testKeys {
			val, err := reader.Seek(k)
			if err != nil {
				t.Fatalf("Seek(%x) failed unexpectedly: %v", k, err)
			}
			expectedVal := expectedValues[string(k)]
			if !bytes.Equal(val, expectedVal) {
				t.Fatalf("Seek(%x) value mismatch: got %q, want %q", k, val, expectedVal)
			}
		}
	})

	t.Run("missing keys between boundaries return ErrKeyNotFound", func(t *testing.T) {
		// Key before first block
		minProbe := []byte{0x00, 0x01}
		if _, err := reader.Seek(minProbe); !stdErrors.Is(err, errors.ErrKeyNotFound) {
			t.Fatalf("expected ErrKeyNotFound for key before first block, got %v", err)
		}

		// Key between existing keys
		betweenProbe := append([]byte(nil), testKeys[0]...)
		betweenProbe = append(betweenProbe, 0xFF)
		if _, err := reader.Seek(betweenProbe); !stdErrors.Is(err, errors.ErrKeyNotFound) {
			t.Fatalf("expected ErrKeyNotFound for non-existent interleaved key, got %v", err)
		}

		// Key after last block
		maxProbe := []byte{0xFF, 0xFF, 0xFF, 0xFF}
		if _, err := reader.Seek(maxProbe); !stdErrors.Is(err, errors.ErrKeyNotFound) {
			t.Fatalf("expected ErrKeyNotFound for key beyond last block, got %v", err)
		}
	})
}

// TestSecurity_Remediation2_Permissions verifies that SSTable files are created with 0600
// permissions, and parent directories are created with 0700 permissions.
func TestSecurity_Remediation2_Permissions(t *testing.T) {
	baseDir := t.TempDir()
	nestedDir := filepath.Join(baseDir, "secure_sstable_dir")
	sstPath := filepath.Join(nestedDir, "secure.sst")

	writer, err := sstable.NewTableWriter(sstPath, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}

	ik, _ := binary.NewInternalKey([]byte("perm-test"), 1, binary.OpTypePut)
	if err := writer.Add(ik, []byte("val")); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	if _, err := writer.Finish(); err != nil {
		t.Fatalf("Finish failed: %v", err)
	}

	// 1. Verify parent directory permission is 0700
	dirInfo, err := os.Stat(nestedDir)
	if err != nil {
		t.Fatalf("Stat directory failed: %v", err)
	}
	dirMode := dirInfo.Mode().Perm()
	if dirMode&0777 != 0700 {
		t.Fatalf("directory permission insecure: got %04o, want 0700", dirMode&0777)
	}

	// 2. Verify SSTable file permission is 0600
	fileInfo, err := os.Stat(sstPath)
	if err != nil {
		t.Fatalf("Stat sstable file failed: %v", err)
	}
	fileMode := fileInfo.Mode().Perm()
	if fileMode&0777 != 0600 {
		t.Fatalf("SSTable file permission insecure: got %04o, want 0600", fileMode&0777)
	}
}

// TestSecurity_Remediation3_StagingFileHardening verifies that staging file creation
// uses unique names, detects symlinks, handles concurrent writers cleanly, and prevents
// destination overwrites.
func TestSecurity_Remediation3_StagingFileHardening(t *testing.T) {
	t.Run("concurrent writers targeting same destination", func(t *testing.T) {
		dir := t.TempDir()
		targetPath := filepath.Join(dir, "contended.sst")

		const concurrentWriters = 2
		var wg sync.WaitGroup
		wg.Add(concurrentWriters)

		successCount := 0
		existErrCount := 0
		var mu sync.Mutex

		for i := 0; i < concurrentWriters; i++ {
			writerID := i
			go func() {
				defer wg.Done()
				w, err := sstable.NewTableWriter(targetPath, sstable.DefaultTableWriterOptions())
				if err != nil {
					mu.Lock()
					if stdErrors.Is(err, errors.ErrSSTableExists) {
						existErrCount++
					}
					mu.Unlock()
					return
				}

				ik, _ := binary.NewInternalKey([]byte(fmt.Sprintf("key-writer-%d", writerID)), 1, binary.OpTypePut)
				_ = w.Add(ik, []byte("val"))

				_, finishErr := w.Finish()
				mu.Lock()
				defer mu.Unlock()
				if finishErr == nil {
					successCount++
				} else if stdErrors.Is(finishErr, errors.ErrSSTableExists) {
					existErrCount++
				}
			}()
		}

		wg.Wait()

		// Exactly one writer must succeed, the other must fail cleanly with ErrSSTableExists
		if successCount != 1 {
			t.Fatalf("expected exactly 1 successful writer, got %d (existErrCount=%d)", successCount, existErrCount)
		}
		if existErrCount != 1 {
			t.Fatalf("expected exactly 1 writer rejected with ErrSSTableExists, got %d", existErrCount)
		}

		// Final file must be intact and readable
		reader, err := sstable.NewTableReader(targetPath)
		if err != nil {
			t.Fatalf("failed to open finalized table: %v", err)
		}
		_ = reader.Close()
	})

	t.Run("destination created after writer init is not silently overwritten", func(t *testing.T) {
		dir := t.TempDir()
		targetPath := filepath.Join(dir, "overwrite_test.sst")

		writer, err := sstable.NewTableWriter(targetPath, sstable.DefaultTableWriterOptions())
		if err != nil {
			t.Fatalf("NewTableWriter failed: %v", err)
		}

		ik, _ := binary.NewInternalKey([]byte("early-key"), 1, binary.OpTypePut)
		if err := writer.Add(ik, []byte("val")); err != nil {
			t.Fatalf("Add failed: %v", err)
		}

		// An external entity creates destination file before Finish()
		if err := os.WriteFile(targetPath, []byte("external-content"), 0600); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}

		// Finish must fail closed with ErrSSTableExists without overwriting external file
		_, err = writer.Finish()
		if !stdErrors.Is(err, errors.ErrSSTableExists) {
			t.Fatalf("expected ErrSSTableExists on destination collision at Finish, got %v", err)
		}

		// Verify external content was NOT destroyed
		content, err := os.ReadFile(targetPath)
		if err != nil {
			t.Fatalf("ReadFile failed: %v", err)
		}
		if string(content) != "external-content" {
			t.Fatalf("destination file content was modified: got %q", string(content))
		}
	})

	t.Run("pre-existing destination symlink is rejected at writer creation", func(t *testing.T) {
		dir := t.TempDir()
		targetPath := filepath.Join(dir, "symlink_dst.sst")
		decoyPath := filepath.Join(dir, "decoy.txt")
		_ = os.WriteFile(decoyPath, []byte("decoy"), 0600)

		// Create symlink targetPath -> decoyPath
		if err := os.Symlink(decoyPath, targetPath); err != nil {
			t.Skipf("symlink not supported on this platform: %v", err)
		}

		_, err := sstable.NewTableWriter(targetPath, sstable.DefaultTableWriterOptions())
		if !stdErrors.Is(err, errors.ErrSSTableExists) {
			t.Fatalf("expected ErrSSTableExists for pre-existing destination symlink, got %v", err)
		}
	})
}

// TestSecurity_Remediation4_MaxKeyLengthBoundary verifies that:
// - 65,535-byte user keys are fully supported through Add, block flush, index, Finish, and Seek
// - 65,544-byte encoded InternalKeys are supported in the sparse index
// - 65,536-byte user keys are rejected
// - Encoded InternalKeys > 65,544 bytes are rejected
func TestSecurity_Remediation4_MaxKeyLengthBoundary(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "max_key_boundary.sst")

	writer, err := sstable.NewTableWriter(sstPath, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}

	// 1. Boundary key: exactly 65,535 bytes (binary.MaxKeyLen / MaxUserKeyLen)
	maxKey := bytes.Repeat([]byte("M"), binary.MaxUserKeyLen)
	ik, err := binary.NewInternalKey(maxKey, 100, binary.OpTypePut)
	if err != nil {
		t.Fatalf("NewInternalKey with MaxUserKeyLen failed: %v", err)
	}

	val := []byte("max-key-payload")
	if err := writer.Add(ik, val); err != nil {
		t.Fatalf("writer.Add with MaxUserKeyLen (65,535 bytes) failed: %v", err)
	}

	meta, err := writer.Finish()
	if err != nil {
		t.Fatalf("writer.Finish with MaxUserKeyLen failed: %v", err)
	}

	// Encoded InternalKey length in index must be 65,535 + 9 = 65,544 bytes
	if len(meta.LargestKey) != binary.MaxEncodedInternalKeyLen {
		t.Fatalf("LargestKey len mismatch: got %d, want %d", len(meta.LargestKey), binary.MaxEncodedInternalKeyLen)
	}

	// Read back and Seek
	reader, err := sstable.NewTableReader(sstPath)
	if err != nil {
		t.Fatalf("NewTableReader failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	retrievedVal, err := reader.Seek(maxKey)
	if err != nil {
		t.Fatalf("Seek with MaxUserKeyLen failed: %v", err)
	}
	if !bytes.Equal(retrievedVal, val) {
		t.Fatalf("retrieved value mismatch: got %q, want %q", retrievedVal, val)
	}

	// 2. Oversized user key (65,536 bytes) rejected at TableWriter.Add
	oversizedUserKey := bytes.Repeat([]byte("O"), binary.MaxUserKeyLen+1)
	oversizedIK := binary.InternalKey{UserKey: oversizedUserKey, SeqNum: 1, OpType: binary.OpTypePut}
	sstPath2 := filepath.Join(dir, "oversized.sst")
	w2, _ := sstable.NewTableWriter(sstPath2, sstable.DefaultTableWriterOptions())
	defer func() { _ = w2.Close() }()

	errAdd := w2.Add(oversizedIK, []byte("v"))
	if !stdErrors.Is(errAdd, errors.ErrKeyTooLarge) {
		t.Fatalf("expected ErrKeyTooLarge for 65,536-byte user key, got %v", errAdd)
	}

	// 3. Encoded InternalKey boundary testing in IndexBuilder
	ib := sstable.NewIndexBuilder()
	validMaxIKBytes := make([]byte, binary.MaxEncodedInternalKeyLen)
	validMaxIKBytes[len(validMaxIKBytes)-1] = 0x01 // OpTypePut
	if err := ib.AddBlock(validMaxIKBytes, sstable.BlockHandle{Offset: 0, Size: 100}); err != nil {
		t.Fatalf("IndexBuilder.AddBlock rejected 65,544-byte encoded InternalKey: %v", err)
	}

	oversizedIndexKey := make([]byte, binary.MaxEncodedInternalKeyLen+1)
	errIndexAdd := ib.AddBlock(oversizedIndexKey, sstable.BlockHandle{Offset: 100, Size: 100})
	if !stdErrors.Is(errIndexAdd, errors.ErrKeyTooLarge) {
		t.Fatalf("expected ErrKeyTooLarge for 65,545-byte index key, got %v", errIndexAdd)
	}
}

// TestSecurity_Remediation5_ValidationOrderAtomicity verifies that TableWriter.Add
// validates all inputs (key, opType, value) BEFORE performing block flushes or state mutations.
func TestSecurity_Remediation5_ValidationOrderAtomicity(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "validation_atomicity.sst")

	opts := sstable.DefaultTableWriterOptions()
	opts.TargetBlockSize = 512

	writer, err := sstable.NewTableWriter(sstPath, opts)
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}
	defer func() { _ = writer.Close() }()

	// Fill data block right to the flush boundary
	ik1, _ := binary.NewInternalKey([]byte("key-001"), 10, binary.OpTypePut)
	val1 := bytes.Repeat([]byte("A"), 500)
	if err := writer.Add(ik1, val1); err != nil {
		t.Fatalf("initial Add failed: %v", err)
	}

	initialBlockCount := writer.BlockCount()
	initialBytesWritten := writer.BytesWritten()

	// Attempt to Add an invalid record (value > 4MB MaxValueLen)
	ikInvalid, _ := binary.NewInternalKey([]byte("key-002"), 9, binary.OpTypePut)
	oversizedValue := make([]byte, binary.MaxValueLen+1)

	errInvalid := writer.Add(ikInvalid, oversizedValue)
	if !stdErrors.Is(errInvalid, errors.ErrValueTooLarge) {
		t.Fatalf("expected ErrValueTooLarge, got %v", errInvalid)
	}

	// Verify that failure atomicity is preserved: no premature block flush occurred
	if writer.BlockCount() != initialBlockCount {
		t.Fatalf("BlockCount mutated after failed Add: got %d, want %d", writer.BlockCount(), initialBlockCount)
	}
	if writer.BytesWritten() != initialBytesWritten {
		t.Fatalf("BytesWritten mutated after failed Add: got %d, want %d", writer.BytesWritten(), initialBytesWritten)
	}

	// Subsequent valid Add must succeed and finalize properly
	ik2, _ := binary.NewInternalKey([]byte("key-003"), 8, binary.OpTypePut)
	if err := writer.Add(ik2, []byte("valid-payload")); err != nil {
		t.Fatalf("subsequent Add failed: %v", err)
	}

	meta, err := writer.Finish()
	if err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	if meta.EntryCount != 2 {
		t.Fatalf("EntryCount mismatch: got %d, want 2", meta.EntryCount)
	}
}

// TestSecurity_Remediation6_InternalKeyRedaction verifies that InternalKey default
// formatting (%v, %s) does not expose cleartext user keys.
func TestSecurity_Remediation6_InternalKeyRedaction(t *testing.T) {
	secretKey := []byte("SECRET_PASSWORD_OR_TOKEN_12345")
	ik, err := binary.NewInternalKey(secretKey, 42, binary.OpTypePut)
	if err != nil {
		t.Fatalf("NewInternalKey failed: %v", err)
	}

	defaultStr := ik.String()
	fmtV := fmt.Sprintf("val: %v", ik)
	fmtS := fmt.Sprintf("str: %s", ik)

	for _, formatted := range []string{defaultStr, fmtV, fmtS} {
		if bytes.Contains([]byte(formatted), secretKey) {
			t.Fatalf("cleartext user key exposed in default formatting: %q", formatted)
		}
	}

	// Verify debug representation explicitly includes key for deliberate forensic audit
	debugStr := ik.DebugString()
	if !bytes.Contains([]byte(debugStr), secretKey) {
		t.Fatalf("DebugString expected to contain key, got %q", debugStr)
	}
}

// TestSecurity_Remediation1_DecodeBlockIndex_MalformedKeys tests that DecodeBlockIndex
// strictly rejects malformed, truncated, or invalid InternalKeys.
func TestSecurity_Remediation1_DecodeBlockIndex_MalformedKeys(t *testing.T) {
	// Helper to build a minimal index block with given raw keys and handles
	buildRawIndexBlock := func(keys [][]byte, handles []sstable.BlockHandle) []byte {
		var buf []byte
		restarts := make([]uint32, len(keys))
		for i, k := range keys {
			restarts[i] = uint32(len(buf))
			var klenBuf [10]byte
			n := binary.PutVarint64(klenBuf[:], uint64(len(k)))
			buf = append(buf, klenBuf[:n]...)
			buf = append(buf, k...)
			buf = handles[i].AppendTo(buf)
		}
		for _, r := range restarts {
			var rBuf [4]byte
			binary.PutUint32(rBuf[:], r)
			buf = append(buf, rBuf[:]...)
		}
		var numRestartsBuf [4]byte
		binary.PutUint32(numRestartsBuf[:], uint32(len(restarts)))
		buf = append(buf, numRestartsBuf[:]...)
		checksum := binary.Checksum(buf)
		var crcBuf [4]byte
		binary.PutUint32(crcBuf[:], checksum)
		buf = append(buf, crcBuf[:]...)
		return buf
	}

	validIK, err := binary.NewInternalKey([]byte("valid-user-key"), 100, binary.OpTypePut)
	if err != nil {
		t.Fatalf("NewInternalKey failed: %v", err)
	}
	validEncoded := binary.EncodeInternalKey(validIK)

	h1 := sstable.BlockHandle{Offset: 0, Size: 100}
	h2 := sstable.BlockHandle{Offset: 100, Size: 100}

	t.Run("ValidIndexDecodesSuccessfully", func(t *testing.T) {
		blockData := buildRawIndexBlock([][]byte{validEncoded}, []sstable.BlockHandle{h1})
		idx, err := sstable.DecodeBlockIndex(blockData)
		if err != nil {
			t.Fatalf("DecodeBlockIndex failed on valid index: %v", err)
		}
		if idx.EntryCount() != 1 {
			t.Fatalf("expected 1 entry, got %d", idx.EntryCount())
		}
		entries := idx.Entries()
		if !bytes.Equal(entries[0].UserKey(), []byte("valid-user-key")) {
			t.Fatalf("user key mismatch: got %q", entries[0].UserKey())
		}
	})

	t.Run("TruncatedInternalKey_LessThan9Bytes", func(t *testing.T) {
		shortKey := []byte("short") // 5 bytes < 9
		blockData := buildRawIndexBlock([][]byte{shortKey}, []sstable.BlockHandle{h1})
		_, err := sstable.DecodeBlockIndex(blockData)
		if err == nil {
			t.Fatal("expected error decoding index with truncated key, got nil")
		}
		if !stdErrors.Is(err, errors.ErrIndexBlockCorrupted) {
			t.Fatalf("expected ErrIndexBlockCorrupted, got %v", err)
		}
	})

	t.Run("InvalidOpTypeInInternalKey", func(t *testing.T) {
		// Valid length (10 bytes: 1 byte key + 8 byte seq + 1 byte invalid op)
		badOpKey := append([]byte("k"), 0, 0, 0, 0, 0, 0, 0, 1, 0x99)
		blockData := buildRawIndexBlock([][]byte{badOpKey}, []sstable.BlockHandle{h1})
		_, err := sstable.DecodeBlockIndex(blockData)
		if err == nil {
			t.Fatal("expected error decoding index with invalid op type, got nil")
		}
		if !stdErrors.Is(err, errors.ErrIndexBlockCorrupted) {
			t.Fatalf("expected ErrIndexBlockCorrupted, got %v", err)
		}
	})

	t.Run("EmptyUserKeyInInternalKey", func(t *testing.T) {
		// Exactly 9 bytes: empty user key + 8 byte seq + 1 byte op
		emptyUserKeyIK := []byte{0, 0, 0, 0, 0, 0, 0, 1, byte(binary.OpTypePut)}
		blockData := buildRawIndexBlock([][]byte{emptyUserKeyIK}, []sstable.BlockHandle{h1})
		_, err := sstable.DecodeBlockIndex(blockData)
		if err == nil {
			t.Fatal("expected error decoding index with empty user key, got nil")
		}
		if !stdErrors.Is(err, errors.ErrIndexBlockCorrupted) {
			t.Fatalf("expected ErrIndexBlockCorrupted, got %v", err)
		}
	})

	t.Run("NonMonotonicInternalKeys", func(t *testing.T) {
		ikA, _ := binary.NewInternalKey([]byte("key-z"), 100, binary.OpTypePut)
		ikB, _ := binary.NewInternalKey([]byte("key-a"), 100, binary.OpTypePut)
		// ikA > ikB, violates strict monotonic ordering
		blockData := buildRawIndexBlock([][]byte{binary.EncodeInternalKey(ikA), binary.EncodeInternalKey(ikB)}, []sstable.BlockHandle{h1, h2})
		_, err := sstable.DecodeBlockIndex(blockData)
		if err == nil {
			t.Fatal("expected error for non-monotonic index keys, got nil")
		}
		if !stdErrors.Is(err, errors.ErrIndexBlockCorrupted) {
			t.Fatalf("expected ErrIndexBlockCorrupted, got %v", err)
		}
	})

	t.Run("EqualInternalKeys", func(t *testing.T) {
		// Duplicate largest keys in adjacent blocks violate strict ordering
		blockData := buildRawIndexBlock([][]byte{validEncoded, validEncoded}, []sstable.BlockHandle{h1, h2})
		_, err := sstable.DecodeBlockIndex(blockData)
		if err == nil {
			t.Fatal("expected error for duplicate index keys, got nil")
		}
		if !stdErrors.Is(err, errors.ErrIndexBlockCorrupted) {
			t.Fatalf("expected ErrIndexBlockCorrupted, got %v", err)
		}
	})
}

// TestSecurity_Remediation1_IndexBuilder_AddBlock_Validation verifies that IndexBuilder
// rejects invalid keys before state mutation and preserves failure atomicity.
func TestSecurity_Remediation1_IndexBuilder_AddBlock_Validation(t *testing.T) {
	builder := sstable.NewIndexBuilder()
	h := sstable.BlockHandle{Offset: 0, Size: 100}

	// Attempt adding truncated key
	err := builder.AddBlock([]byte("short"), h)
	if err == nil {
		t.Fatal("expected error for truncated key, got nil")
	}
	if builder.EntryCount() != 0 {
		t.Fatalf("EntryCount mutated after failed AddBlock: got %d, want 0", builder.EntryCount())
	}

	// Attempt adding invalid op type
	badOpKey := append([]byte("k"), 0, 0, 0, 0, 0, 0, 0, 1, 0xFF)
	err = builder.AddBlock(badOpKey, h)
	if err == nil {
		t.Fatal("expected error for invalid op type, got nil")
	}
	if builder.EntryCount() != 0 {
		t.Fatalf("EntryCount mutated after failed AddBlock: got %d, want 0", builder.EntryCount())
	}

	// Valid add succeeds
	validIK, _ := binary.NewInternalKey([]byte("valid-k"), 10, binary.OpTypePut)
	err = builder.AddBlock(binary.EncodeInternalKey(validIK), h)
	if err != nil {
		t.Fatalf("expected AddBlock to succeed, got %v", err)
	}
	if builder.EntryCount() != 1 {
		t.Fatalf("EntryCount mismatch: got %d, want 1", builder.EntryCount())
	}

	// Adding a smaller or equal key violates monotonic ordering and preserves atomicity
	smallerIK, _ := binary.NewInternalKey([]byte("aaa"), 10, binary.OpTypePut)
	h2 := sstable.BlockHandle{Offset: 100, Size: 100}
	err = builder.AddBlock(binary.EncodeInternalKey(smallerIK), h2)
	if err == nil {
		t.Fatal("expected error for non-monotonic key, got nil")
	}
	if !stdErrors.Is(err, errors.ErrKeyOutOfOrder) {
		t.Fatalf("expected ErrKeyOutOfOrder, got %v", err)
	}
	if builder.EntryCount() != 1 {
		t.Fatalf("EntryCount mutated after non-monotonic AddBlock: got %d, want 1", builder.EntryCount())
	}
}

// TestSecurity_Remediation1_PropertyInvariant_DecodedKeysAreInternalKeys verifies the property:
// Any successfully decoded BlockIndex contains ONLY valid, canonical InternalKeys.
func TestSecurity_Remediation1_PropertyInvariant_DecodedKeysAreInternalKeys(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "property_test.sst")

	opts := sstable.DefaultTableWriterOptions()
	opts.TargetBlockSize = 256
	writer, err := sstable.NewTableWriter(sstPath, opts)
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}

	// Write multiple entries across multiple blocks
	for i := 0; i < 50; i++ {
		keyStr := fmt.Sprintf("property-user-key-%04d", i)
		ik, _ := binary.NewInternalKey([]byte(keyStr), binary.SeqNum(i+1), binary.OpTypePut)
		if err := writer.Add(ik, []byte("value-payload")); err != nil {
			t.Fatalf("Add failed: %v", err)
		}
	}
	_, err = writer.Finish()
	if err != nil {
		t.Fatalf("Finish failed: %v", err)
	}

	// Read table and inspect its index
	f, err := os.Open(sstPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer func() { _ = f.Close() }()

	reader, err := sstable.NewTableReaderWithFile(f)
	if err != nil {
		t.Fatalf("NewTableReaderWithFile failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	idx := reader.Index()
	if idx == nil || idx.EntryCount() == 0 {
		t.Fatal("expected non-empty index")
	}

	// Verify property: every stored index key is a valid InternalKey
	var prevIK *binary.InternalKey
	entries := idx.Entries()
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		// 1. Structure must be valid
		if len(entry.Key.UserKey) == 0 {
			t.Fatalf("entry %d has empty user key", i)
		}
		if entry.Key.OpType != binary.OpTypePut && entry.Key.OpType != binary.OpTypeDelete {
			t.Fatalf("entry %d has invalid op type %v", i, entry.Key.OpType)
		}
		// 2. UserKey() must match entry.Key.UserKey
		if !bytes.Equal(entry.UserKey(), entry.Key.UserKey) {
			t.Fatalf("entry %d UserKey() mismatch", i)
		}
		// 3. Monotonic ordering must hold
		if prevIK != nil {
			if binary.CompareInternalKey(*prevIK, entry.Key) >= 0 {
				t.Fatalf("index entries %d and %d not strictly monotonic", i-1, i)
			}
		}
		curr := entry.Key
		prevIK = &curr
	}
}

// TestSecurity_Remediation2_AllocationLimits tests that memory allocation bounds
// are strictly enforced for both index blocks and data blocks BEFORE allocation.
func TestSecurity_Remediation2_AllocationLimits(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "alloc_limit_test.sst")

	// Create a valid small SSTable
	opts := sstable.DefaultTableWriterOptions()
	writer, err := sstable.NewTableWriter(sstPath, opts)
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}

	ik, _ := binary.NewInternalKey([]byte("key-001"), 1, binary.OpTypePut)
	if err := writer.Add(ik, []byte("value-001")); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	_, err = writer.Finish()
	if err != nil {
		t.Fatalf("Finish failed: %v", err)
	}

	rawBytes, err := os.ReadFile(sstPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	t.Run("IndexBlockExceedsMaxIndexBlockSize", func(t *testing.T) {
		// Tamper with footer: set IndexHandle.Size to MaxIndexBlockSize + 1
		tampered := make([]byte, len(rawBytes))
		copy(tampered, rawBytes)

		footerOffset := len(tampered) - sstable.FooterSize
		footerBytes := tampered[footerOffset:]

		var footer sstable.Footer
		if err := footer.Decode(footerBytes); err != nil {
			t.Fatalf("Decode failed: %v", err)
		}

		// Create a handle with size > MaxIndexBlockSize
		tooBigHandle := sstable.BlockHandle{Offset: footer.IndexHandle.Offset, Size: sstable.MaxIndexBlockSize + 1}
		badFooter := sstable.Footer{IndexHandle: tooBigHandle, MetaIndexHandle: footer.MetaIndexHandle}
		enc := badFooter.Encode()
		copy(tampered[footerOffset:], enc[:])

		// Pad file so file size doesn't immediately fail before checking MaxIndexBlockSize
		padded := make([]byte, len(tampered)+int(sstable.MaxIndexBlockSize)+1024)
		copy(padded, tampered[:footerOffset])
		copy(padded[len(padded)-sstable.FooterSize:], enc[:])

		tmpFile := filepath.Join(dir, "oversized_index.sst")
		if err := os.WriteFile(tmpFile, padded, 0600); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}
		defer func() { _ = os.Remove(tmpFile) }()

		f, err := os.Open(tmpFile)
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer func() { _ = f.Close() }()

		var memBefore runtime.MemStats
		runtime.ReadMemStats(&memBefore)

		_, err = sstable.NewTableReaderWithFile(f)
		if err == nil {
			t.Fatal("expected error for oversized index block, got nil")
		}
		if !stdErrors.Is(err, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected ErrInvalidBlockHandle, got %v", err)
		}

		var memAfter runtime.MemStats
		runtime.ReadMemStats(&memAfter)
		// Ensure no huge 8+ MiB buffer was allocated
		if memAfter.TotalAlloc-memBefore.TotalAlloc > 4*1024*1024 {
			t.Fatalf("excessive memory allocated before rejection: %d bytes", memAfter.TotalAlloc-memBefore.TotalAlloc)
		}
	})

	t.Run("IndexBlockHandleHugeUint64", func(t *testing.T) {
		tampered := make([]byte, len(rawBytes))
		copy(tampered, rawBytes)
		footerOffset := len(tampered) - sstable.FooterSize
		footerBytes := tampered[footerOffset:]
		var footer sstable.Footer
		if err := footer.Decode(footerBytes); err != nil {
			t.Fatalf("Decode failed: %v", err)
		}

		hugeHandle := sstable.BlockHandle{Offset: 0, Size: math.MaxUint64}
		badFooter := sstable.Footer{IndexHandle: hugeHandle, MetaIndexHandle: footer.MetaIndexHandle}
		enc := badFooter.Encode()
		copy(tampered[footerOffset:], enc[:])

		tmpFile := filepath.Join(dir, "huge_uint64_index.sst")
		if err := os.WriteFile(tmpFile, tampered, 0600); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}
		defer func() { _ = os.Remove(tmpFile) }()

		f, err := os.Open(tmpFile)
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer func() { _ = f.Close() }()

		_, err = sstable.NewTableReaderWithFile(f)
		if err == nil {
			t.Fatal("expected error for MaxUint64 index handle, got nil")
		}
		if !stdErrors.Is(err, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected ErrInvalidBlockHandle, got %v", err)
		}
	})

	t.Run("DataBlockExceedsMaxDataBlockSize", func(t *testing.T) {
		// Build raw index block containing a handle that claims size = MaxDataBlockSize + 1
		ikValid, _ := binary.NewInternalKey([]byte("zzz"), 10, binary.OpTypePut)
		oversizedHandle := sstable.BlockHandle{Offset: 0, Size: sstable.MaxDataBlockSize + 1}

		// Helper to build index bytes
		var buf []byte
		var klenBuf [10]byte
		encodedKey := binary.EncodeInternalKey(ikValid)
		n := binary.PutVarint64(klenBuf[:], uint64(len(encodedKey)))
		buf = append(buf, klenBuf[:n]...)
		buf = append(buf, encodedKey...)
		buf = oversizedHandle.AppendTo(buf)

		// Restart
		var rBuf [4]byte
		binary.PutUint32(rBuf[:], 0)
		buf = append(buf, rBuf[:]...)
		// Num restarts
		binary.PutUint32(rBuf[:], 1)
		buf = append(buf, rBuf[:]...)
		// CRC
		checksum := binary.Checksum(buf)
		binary.PutUint32(rBuf[:], checksum)
		buf = append(buf, rBuf[:]...)

		// Construct an SSTable file with this index block
		var fileBuf bytes.Buffer
		fileBuf.WriteString("minimal-data-block")
		indexOffset := uint64(fileBuf.Len())
		fileBuf.Write(buf)
		indexSize := uint64(len(buf))

		idxHandle := sstable.BlockHandle{Offset: indexOffset, Size: indexSize}
		metaHandle := sstable.BlockHandle{Offset: 0, Size: 1} // Non-zero for valid footer
		// Write dummy meta block
		metaOffset := uint64(fileBuf.Len())
		fileBuf.WriteString("meta")
		metaHandle.Offset = metaOffset
		metaHandle.Size = 4

		footer := sstable.Footer{IndexHandle: idxHandle, MetaIndexHandle: metaHandle}
		footerEnc := footer.Encode()
		fileBuf.Write(footerEnc[:])

		tmpFile := filepath.Join(dir, "oversized_data_block.sst")
		if err := os.WriteFile(tmpFile, fileBuf.Bytes(), 0600); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}
		defer func() { _ = os.Remove(tmpFile) }()

		f, err := os.Open(tmpFile)
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer func() { _ = f.Close() }()

		reader, err := sstable.NewTableReaderWithFile(f)
		if err != nil {
			t.Fatalf("NewTableReaderWithFile failed: %v", err)
		}
		defer func() { _ = reader.Close() }()

		// Calling Seek must encounter the oversized block handle and reject it BEFORE allocation
		var memBefore runtime.MemStats
		runtime.ReadMemStats(&memBefore)

		_, err = reader.Seek([]byte("zzz"))
		if err == nil {
			t.Fatal("expected error seeking oversized data block, got nil")
		}
		if !stdErrors.Is(err, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected ErrInvalidBlockHandle, got %v", err)
		}

		var memAfter runtime.MemStats
		runtime.ReadMemStats(&memAfter)
		if memAfter.TotalAlloc-memBefore.TotalAlloc > 4*1024*1024 {
			t.Fatalf("excessive memory allocated during rejected Seek: %d bytes", memAfter.TotalAlloc-memBefore.TotalAlloc)
		}
	})
}

// TestSecurity_Remediation3_FilePermissions_Baseline tests that TableWriter enforces
// owner-only file permissions and strictly rejects insecure modes (e.g. 0644, 0666, 0755, 0777).
func TestSecurity_Remediation3_FilePermissions_Baseline(t *testing.T) {
	dir := t.TempDir()

	insecureModes := []os.FileMode{
		0644, // Group/other readable
		0666, // Group/other writable
		0755, // Executable + group/other readable
		0777, // World readable/writable/executable
		0640, // Group readable
		0604, // Other readable
		0700, // Executable bit set
	}

	for _, mode := range insecureModes {
		t.Run(fmt.Sprintf("RejectMode_%04o", mode), func(t *testing.T) {
			sstPath := filepath.Join(dir, fmt.Sprintf("insecure_%04o.sst", mode))
			opts := sstable.DefaultTableWriterOptions()
			opts.FileMode = mode

			_, err := sstable.NewTableWriter(sstPath, opts)
			if err == nil {
				t.Fatalf("expected NewTableWriter to reject mode %04o, but it succeeded", mode)
			}
			if !stdErrors.Is(err, errors.ErrInsecureFileMode) {
				t.Fatalf("expected ErrInsecureFileMode, got %v", err)
			}

			// Ensure no destination file or temporary file remains
			if _, err := os.Stat(sstPath); !os.IsNotExist(err) {
				t.Fatalf("insecure file %s was created despite error", sstPath)
			}
			tmpPath := sstPath + ".tmp"
			if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
				t.Fatalf("staging file %s was left behind after error", tmpPath)
			}
		})
	}

	t.Run("DefaultAndExplicitOwnerOnlyAccepted", func(t *testing.T) {
		validModes := []os.FileMode{
			0,    // Default (0600)
			0600, // Explicit owner read/write
			0400, // Owner read-only
		}

		for _, mode := range validModes {
			sstPath := filepath.Join(dir, fmt.Sprintf("secure_%04o.sst", mode))
			opts := sstable.DefaultTableWriterOptions()
			opts.FileMode = mode

			writer, err := sstable.NewTableWriter(sstPath, opts)
			if err != nil {
				t.Fatalf("NewTableWriter failed for valid mode %04o: %v", mode, err)
			}
			ik, _ := binary.NewInternalKey([]byte("key"), 1, binary.OpTypePut)
			_ = writer.Add(ik, []byte("val"))
			_, err = writer.Finish()
			if err != nil {
				t.Fatalf("Finish failed: %v", err)
			}

			info, err := os.Stat(sstPath)
			if err != nil {
				t.Fatalf("Stat failed: %v", err)
			}
			perm := info.Mode().Perm()
			// Must not have group or other bits set
			if perm&0077 != 0 {
				t.Fatalf("file %s has insecure permissions %04o", sstPath, perm)
			}
		}
	})
}

// TestSecurity_Remediation1_FindBlock_Ordering verifies that FindBlock, FindBlockKey,
// and FindBlockInternalKey adhere to canonical SSTable ordering invariants.
func TestSecurity_Remediation1_FindBlock_Ordering(t *testing.T) {
	builder := sstable.NewIndexBuilder()

	// Block 1 largest: ("apple", seq=100, OpTypePut)
	ik1, _ := binary.NewInternalKey([]byte("apple"), 100, binary.OpTypePut)
	h1 := sstable.BlockHandle{Offset: 0, Size: 100}
	if err := builder.AddBlock(binary.EncodeInternalKey(ik1), h1); err != nil {
		t.Fatalf("AddBlock 1 failed: %v", err)
	}

	// Block 2 largest: ("banana", seq=50, OpTypeDelete)
	ik2, _ := binary.NewInternalKey([]byte("banana"), 50, binary.OpTypeDelete)
	h2 := sstable.BlockHandle{Offset: 100, Size: 100}
	if err := builder.AddBlock(binary.EncodeInternalKey(ik2), h2); err != nil {
		t.Fatalf("AddBlock 2 failed: %v", err)
	}

	// Block 3 largest: ("banana", seq=10, OpTypePut)
	// Note: in LSM ordering, older seqnum (10) for same user key sorts AFTER newer seqnum (50)
	ik3, _ := binary.NewInternalKey([]byte("banana"), 10, binary.OpTypePut)
	h3 := sstable.BlockHandle{Offset: 200, Size: 100}
	if err := builder.AddBlock(binary.EncodeInternalKey(ik3), h3); err != nil {
		t.Fatalf("AddBlock 3 failed: %v", err)
	}

	// Block 4 largest: ("cherry", seq=200, OpTypePut)
	ik4, _ := binary.NewInternalKey([]byte("cherry"), 200, binary.OpTypePut)
	h4 := sstable.BlockHandle{Offset: 300, Size: 100}
	if err := builder.AddBlock(binary.EncodeInternalKey(ik4), h4); err != nil {
		t.Fatalf("AddBlock 4 failed: %v", err)
	}

	indexBytes := builder.Finish()
	idx, err := sstable.DecodeBlockIndex(indexBytes)
	if err != nil {
		t.Fatalf("DecodeBlockIndex failed: %v", err)
	}

	t.Run("FindBlock_UserKeyLookup", func(t *testing.T) {
		// Lookup "ant" -> should land in Block 1 (apple)
		h, ok := idx.FindBlock([]byte("ant"))
		if !ok || h != h1 {
			t.Fatalf("expected block 1 for 'ant', got %v, ok=%v", h, ok)
		}

		// Lookup "apple" -> should land in Block 1
		h, ok = idx.FindBlock([]byte("apple"))
		if !ok || h != h1 {
			t.Fatalf("expected block 1 for 'apple', got %v, ok=%v", h, ok)
		}

		// Lookup "banana" -> should land in Block 2 (first block with banana)
		h, ok = idx.FindBlock([]byte("banana"))
		if !ok || h != h2 {
			t.Fatalf("expected block 2 for 'banana', got %v, ok=%v", h, ok)
		}

		// Lookup "carrot" -> should land in Block 4 (cherry)
		h, ok = idx.FindBlock([]byte("carrot"))
		if !ok || h != h4 {
			t.Fatalf("expected block 4 for 'carrot', got %v, ok=%v", h, ok)
		}

		// Lookup "date" (greater than all largest keys) -> should return false
		_, ok = idx.FindBlock([]byte("date"))
		if ok {
			t.Fatal("expected false for key beyond all blocks ('date')")
		}
	})

	t.Run("FindBlockKey_InternalKeyLookup", func(t *testing.T) {
		// Target ("banana", seq=75, Put): sorts before ("banana", seq=50, Delete), lands in Block 2
		targetIK, _ := binary.NewInternalKey([]byte("banana"), 75, binary.OpTypePut)
		h, ok := idx.FindBlockKey(targetIK)
		if !ok || h != h2 {
			t.Fatalf("expected block 2 for banana@75, got %v, ok=%v", h, ok)
		}

		// Target ("banana", seq=50, Delete): exact match for Block 2 largest key
		h, ok = idx.FindBlockKey(ik2)
		if !ok || h != h2 {
			t.Fatalf("expected block 2 for exact ik2, got %v, ok=%v", h, ok)
		}

		// Target ("banana", seq=30, Put): sorts after Block 2, before Block 3 (banana@10), lands in Block 3
		targetIK30, _ := binary.NewInternalKey([]byte("banana"), 30, binary.OpTypePut)
		h, ok = idx.FindBlockKey(targetIK30)
		if !ok || h != h3 {
			t.Fatalf("expected block 3 for banana@30, got %v, ok=%v", h, ok)
		}

		// Target ("banana", seq=5, Put): sorts after Block 3, lands in Block 4 (cherry)
		targetIK5, _ := binary.NewInternalKey([]byte("banana"), 5, binary.OpTypePut)
		h, ok = idx.FindBlockKey(targetIK5)
		if !ok || h != h4 {
			t.Fatalf("expected block 4 for banana@5, got %v, ok=%v", h, ok)
		}
	})

	t.Run("FindBlockInternalKey_EncodedKeyLookup", func(t *testing.T) {
		targetIK, _ := binary.NewInternalKey([]byte("banana"), 30, binary.OpTypePut)
		h, ok := idx.FindBlockInternalKey(binary.EncodeInternalKey(targetIK))
		if !ok || h != h3 {
			t.Fatalf("expected block 3 for encoded banana@30, got %v, ok=%v", h, ok)
		}

		// Malformed encoded internal key returns false
		_, ok = idx.FindBlockInternalKey([]byte("too-short"))
		if ok {
			t.Fatal("expected false for malformed encoded key")
		}
	})
}

// TestSecurity_Remediation7_DataBlock_InternalKey_Validation verifies that data block parsing
// strictly validates InternalKey structure, enforces strict monotonic ordering during scan,
// and rejects values exceeding MaxValueLen.
func TestSecurity_Remediation7_DataBlock_InternalKey_Validation(t *testing.T) {
	assembleDataBlock := func(entryData []byte, restarts []uint32) []byte {
		var buf bytes.Buffer
		buf.Write(entryData)
		for _, r := range restarts {
			var rBuf [4]byte
			binary.PutUint32(rBuf[:], r)
			buf.Write(rBuf[:])
		}
		var rCountBuf [4]byte
		binary.PutUint32(rCountBuf[:], uint32(len(restarts)))
		buf.Write(rCountBuf[:])
		crc := binary.Checksum(buf.Bytes())
		var crcBuf [4]byte
		binary.PutUint32(crcBuf[:], crc)
		buf.Write(crcBuf[:])
		return buf.Bytes()
	}

	encodeEntry := func(shared uint64, key []byte, val []byte) []byte {
		var buf bytes.Buffer
		var numBuf [10]byte
		n := binary.PutVarint64(numBuf[:], shared)
		buf.Write(numBuf[:n])
		n = binary.PutVarint64(numBuf[:], uint64(len(key)))
		buf.Write(numBuf[:n])
		n = binary.PutVarint64(numBuf[:], uint64(len(val)))
		buf.Write(numBuf[:n])
		buf.Write(key)
		buf.Write(val)
		return buf.Bytes()
	}

	t.Run("TruncatedInternalKeyAtRestartPoint", func(t *testing.T) {
		badKey := []byte{0, 0, 0, 0, 0, 0, 0, 1, 0x01} // 9 bytes < 10 (empty user key)
		e1 := encodeEntry(0, badKey, []byte("val"))
		block := assembleDataBlock(e1, []uint32{0})

		_, err := sstable.SearchDataBlockForTesting(block, []byte("k"), 0)
		if err == nil {
			t.Fatal("expected error for data block with truncated internal key, got nil")
		}
		if !stdErrors.Is(err, errors.ErrDataBlockCorrupted) {
			t.Fatalf("expected ErrDataBlockCorrupted, got %v", err)
		}
	})

	t.Run("NonMonotonicKeysInLinearScan", func(t *testing.T) {
		ikA, _ := binary.NewInternalKey([]byte("bbb"), 10, binary.OpTypePut)
		ikB, _ := binary.NewInternalKey([]byte("aaa"), 10, binary.OpTypePut) // sorts before bbb!
		e1 := encodeEntry(0, binary.EncodeInternalKey(ikA), []byte("v1"))
		e2 := encodeEntry(0, binary.EncodeInternalKey(ikB), []byte("v2"))
		block := assembleDataBlock(append(e1, e2...), []uint32{0})

		// Searching for "aaa" will encounter "bbb" first (cmp > 0) or continue; but searching for "zzz"
		// will scan past "bbb" into "aaa", triggering the monotonic violation check
		_, err := sstable.SearchDataBlockForTesting(block, []byte("zzz"), 0)
		if err == nil {
			t.Fatal("expected error for non-monotonic keys in data block, got nil")
		}
		if !stdErrors.Is(err, errors.ErrDataBlockCorrupted) {
			t.Fatalf("expected ErrDataBlockCorrupted, got %v", err)
		}
	})

	t.Run("OversizedValueInEntry", func(t *testing.T) {
		ik, _ := binary.NewInternalKey([]byte("valid"), 10, binary.OpTypePut)
		var buf bytes.Buffer
		var numBuf [10]byte
		n := binary.PutVarint64(numBuf[:], 0)
		buf.Write(numBuf[:n])
		encodedIK := binary.EncodeInternalKey(ik)
		n = binary.PutVarint64(numBuf[:], uint64(len(encodedIK)))
		buf.Write(numBuf[:n])
		// Claim value length > MaxValueLen (4 MiB)
		n = binary.PutVarint64(numBuf[:], binary.MaxValueLen+1)
		buf.Write(numBuf[:n])
		buf.Write(encodedIK)
		buf.WriteString("dummy-short-value")

		block := assembleDataBlock(buf.Bytes(), []uint32{0})
		_, err := sstable.SearchDataBlockForTesting(block, []byte("valid"), 0)
		if err == nil {
			t.Fatal("expected error for oversized value, got nil")
		}
		if !stdErrors.Is(err, errors.ErrDataBlockCorrupted) {
			t.Fatalf("expected ErrDataBlockCorrupted, got %v", err)
		}
	})
}

// TestSecurity_Issue5_NewTableReaderWithFile_DescriptorLeak asserts that:
//  1. On EVERY early initialization failure, NewTableReaderWithFile closes the provided file descriptor.
//  2. Resource leakage is provably prevented across truncated files, corrupted footers, oversized handles,
//     and corrupted indexes.
func TestSecurity_Issue5_NewTableReaderWithFile_DescriptorLeak(t *testing.T) {
	dir := t.TempDir()

	isFileClosed := func(f *os.File) bool {
		var b [1]byte
		_, err := f.Read(b[:])
		return stdErrors.Is(err, os.ErrClosed)
	}

	t.Run("truncated file (< 48 bytes) closes descriptor", func(t *testing.T) {
		p := filepath.Join(dir, "short.sst")
		if err := os.WriteFile(p, []byte("too short to contain footer"), 0600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		_, err = sstable.NewTableReaderWithFile(f)
		if err == nil {
			t.Fatal("expected error for truncated file, got nil")
		}
		if !isFileClosed(f) {
			t.Fatal("expected file descriptor to be closed after truncated file error")
		}
	})

	t.Run("corrupted footer magic closes descriptor", func(t *testing.T) {
		p := filepath.Join(dir, "bad_magic.sst")
		badFooter := bytes.Repeat([]byte{0xAA}, 48)
		if err := os.WriteFile(p, badFooter, 0600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		_, err = sstable.NewTableReaderWithFile(f)
		if err == nil {
			t.Fatal("expected error for corrupted magic, got nil")
		}
		if !isFileClosed(f) {
			t.Fatal("expected file descriptor to be closed after bad magic error")
		}
	})

	t.Run("oversized index block handle closes descriptor", func(t *testing.T) {
		p := filepath.Join(dir, "oversized_index.sst")
		footer := sstable.Footer{
			MetaIndexHandle: sstable.BlockHandle{Offset: 0, Size: 8},
			IndexHandle:     sstable.BlockHandle{Offset: 8, Size: sstable.MaxIndexBlockSize + 1},
		}
		footerBytes := footer.Encode()
		fileBytes := append(bytes.Repeat([]byte{0}, 100), footerBytes[:]...)
		if err := os.WriteFile(p, fileBytes, 0600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		_, err = sstable.NewTableReaderWithFile(f)
		if err == nil {
			t.Fatal("expected error for oversized index handle, got nil")
		}
		if !isFileClosed(f) {
			t.Fatal("expected file descriptor to be closed after oversized index error")
		}
	})

	t.Run("corrupted index CRC closes descriptor", func(t *testing.T) {
		p := filepath.Join(dir, "bad_index_crc.sst")
		dummyIndex := []byte{0, 0, 0, 0, 0xDE, 0xAD, 0xBE, 0xEF}
		footer := sstable.Footer{
			MetaIndexHandle: sstable.BlockHandle{Offset: 0, Size: 8},
			IndexHandle:     sstable.BlockHandle{Offset: 8, Size: 8},
		}
		footerBytes := footer.Encode()
		fileBytes := append(dummyIndex, dummyIndex...)
		fileBytes = append(fileBytes, footerBytes[:]...)
		if err := os.WriteFile(p, fileBytes, 0600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		_, err = sstable.NewTableReaderWithFile(f)
		if err == nil {
			t.Fatal("expected error for bad index CRC, got nil")
		}
		if !isFileClosed(f) {
			t.Fatal("expected file descriptor to be closed after corrupted index CRC")
		}
	})

	t.Run("repeated failures do not leak descriptors", func(t *testing.T) {
		p := filepath.Join(dir, "leak_test.sst")
		badFooter := bytes.Repeat([]byte{0x00}, 48)
		if err := os.WriteFile(p, badFooter, 0600); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 50; i++ {
			f, err := os.Open(p)
			if err != nil {
				t.Fatalf("os.Open failed at iteration %d: %v", i, err)
			}
			_, err = sstable.NewTableReaderWithFile(f)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !isFileClosed(f) {
				t.Fatalf("file descriptor leaked at iteration %d", i)
			}
		}
	})
}

// TestSecurity_Issue6_IntegerConversionSafety asserts that:
// 1. Restart offsets with MSB set (0x80000000) do not wrap to negative indices on 32-bit platforms.
// 2. Linear scan varint shared key length == math.MaxUint64 does not wrap to negative slicing index or panic.
// 3. Unshared/value lengths close to math.MaxUint64 do not overflow payload arithmetic.
// 4. DecodeBlockIndex validates entryCount and offsets in unsigned domain before conversion.
func TestSecurity_Issue6_IntegerConversionSafety(t *testing.T) {
	assembleBlock := func(entryBytes []byte, restartOffsets []uint32) []byte {
		var buf bytes.Buffer
		buf.Write(entryBytes)
		for _, off := range restartOffsets {
			var offBuf [4]byte
			binary.PutUint32(offBuf[:], off)
			buf.Write(offBuf[:])
		}
		var rCountBuf [4]byte
		binary.PutUint32(rCountBuf[:], uint32(len(restartOffsets)))
		buf.Write(rCountBuf[:])
		crc := binary.Checksum(buf.Bytes())
		var crcBuf [4]byte
		binary.PutUint32(crcBuf[:], crc)
		buf.Write(crcBuf[:])
		return buf.Bytes()
	}

	encodeEntryHelper := func(shared uint64, key []byte, val []byte) []byte {
		var buf bytes.Buffer
		var numBuf [10]byte
		n := binary.PutVarint64(numBuf[:], shared)
		buf.Write(numBuf[:n])
		n = binary.PutVarint64(numBuf[:], uint64(len(key)))
		buf.Write(numBuf[:n])
		n = binary.PutVarint64(numBuf[:], uint64(len(val)))
		buf.Write(numBuf[:n])
		buf.Write(key)
		buf.Write(val)
		return buf.Bytes()
	}

	t.Run("restart offset with MSB set (0x80000000) does not panic on 32-bit", func(t *testing.T) {
		ik, _ := binary.NewInternalKey([]byte("valid"), 1, binary.OpTypePut)
		entry := encodeEntryHelper(0, binary.EncodeInternalKey(ik), []byte("val"))
		block := assembleBlock(entry, []uint32{0, 0x80000000})

		_, err := sstable.SearchDataBlockForTesting(block, []byte("valid"), 0)
		if err == nil {
			t.Fatal("expected corruption error for restart offset 0x80000000, got nil")
		}
		if !stdErrors.Is(err, errors.ErrDataBlockCorrupted) {
			t.Fatalf("expected ErrDataBlockCorrupted, got %v", err)
		}
	})

	t.Run("linear scan with varint shared == math.MaxUint64 does not wrap to negative index or panic", func(t *testing.T) {
		ik, _ := binary.NewInternalKey([]byte("valid"), 1, binary.OpTypePut)
		e1 := encodeEntryHelper(0, binary.EncodeInternalKey(ik), []byte("v1"))

		var e2 bytes.Buffer
		var numBuf [10]byte
		n := binary.PutVarint64(numBuf[:], math.MaxUint64)
		e2.Write(numBuf[:n])
		n = binary.PutVarint64(numBuf[:], 10)
		e2.Write(numBuf[:n])
		n = binary.PutVarint64(numBuf[:], 2)
		e2.Write(numBuf[:n])
		e2.Write(make([]byte, 10)) // delta key
		e2.Write([]byte("v2"))     // value

		block := assembleBlock(append(e1, e2.Bytes()...), []uint32{0})

		_, err := sstable.SearchDataBlockForTesting(block, []byte("zzzz"), 0)
		if err == nil {
			t.Fatal("expected corruption error, got nil")
		}
		if !stdErrors.Is(err, errors.ErrDataBlockCorrupted) {
			t.Fatalf("expected ErrDataBlockCorrupted, got %v", err)
		}
	})

	t.Run("linear scan with unshared key length == math.MaxUint64 does not overflow payload or panic", func(t *testing.T) {
		var e bytes.Buffer
		var numBuf [10]byte
		n := binary.PutVarint64(numBuf[:], 0) // shared = 0
		e.Write(numBuf[:n])
		n = binary.PutVarint64(numBuf[:], math.MaxUint64-2) // unshared close to max uint64
		e.Write(numBuf[:n])
		n = binary.PutVarint64(numBuf[:], 0) // valLen = 0
		e.Write(numBuf[:n])

		block := assembleBlock(e.Bytes(), []uint32{0})
		_, err := sstable.SearchDataBlockForTesting(block, []byte("target"), 0)
		if err == nil {
			t.Fatal("expected corruption error, got nil")
		}
		if !stdErrors.Is(err, errors.ErrDataBlockCorrupted) {
			t.Fatalf("expected ErrDataBlockCorrupted, got %v", err)
		}
	})

	t.Run("DecodeBlockIndex with offset 0x80000000 rejected cleanly", func(t *testing.T) {
		var buf bytes.Buffer
		buf.WriteString("dummy_entry_data")
		var offBuf [4]byte
		binary.PutUint32(offBuf[:], 0x80000000)
		buf.Write(offBuf[:])
		var countBuf [4]byte
		binary.PutUint32(countBuf[:], 1)
		buf.Write(countBuf[:])
		crc := binary.Checksum(buf.Bytes())
		var crcBuf [4]byte
		binary.PutUint32(crcBuf[:], crc)
		buf.Write(crcBuf[:])

		_, err := sstable.DecodeBlockIndex(buf.Bytes())
		if err == nil {
			t.Fatal("expected error for index offset 0x80000000, got nil")
		}
	})
}

// TestSecurity_Issue7_FilesystemErrorHandling_AndPublicationSemantics asserts that:
// 1. Destination paths that exist are rejected with ErrSSTableExists.
// 2. Permission errors are cleanly distinguished from ErrNotExist and not treated as absent files.
// 3. TableWriter.Finish clears tmpPath after successful rename, and preserves finalized state on directory sync failure.
func TestSecurity_Issue7_FilesystemErrorHandling_AndPublicationSemantics(t *testing.T) {
	dir := t.TempDir()

	t.Run("destination file already exists is rejected on NewTableWriter", func(t *testing.T) {
		p := filepath.Join(dir, "exists.sst")
		if err := os.WriteFile(p, []byte("content"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := sstable.NewTableWriter(p, sstable.DefaultTableWriterOptions())
		if !stdErrors.Is(err, errors.ErrSSTableExists) {
			t.Fatalf("expected ErrSSTableExists, got %v", err)
		}
	})

	t.Run("destination directory permission denied distinguishes ErrNotExist", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("skipping POSIX permission test on Windows")
		}
		restrictedDir := filepath.Join(dir, "no_access_dir")
		if err := os.Mkdir(restrictedDir, 0700); err != nil {
			t.Fatal(err)
		}
		subDir := filepath.Join(restrictedDir, "sub")
		if err := os.Mkdir(subDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(restrictedDir, 0000); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(restrictedDir, 0700) }()

		_, err := sstable.NewTableWriter(filepath.Join(subDir, "test.sst"), sstable.DefaultTableWriterOptions())
		if err == nil {
			t.Fatal("expected error for unsearchable path, got nil")
		}
		if stdErrors.Is(err, errors.ErrSSTableExists) {
			t.Fatalf("permission error must not be confused with ErrSSTableExists: %v", err)
		}
	})

	t.Run("Finish publication succeeds then syncDir fails preserves finalized state", func(t *testing.T) {
		p := filepath.Join(dir, "sync_dir_failure.sst")
		w, err := sstable.NewTableWriter(p, sstable.DefaultTableWriterOptions())
		if err != nil {
			t.Fatal(err)
		}

		ik, _ := binary.NewInternalKey([]byte("key"), 1, binary.OpTypePut)
		if err := w.Add(ik, []byte("val")); err != nil {
			t.Fatal(err)
		}

		injectedSyncErr := stdErrors.New("injected directory sync error")
		w.SetSyncDirFnForTesting(func(dirPath string) error {
			return injectedSyncErr
		})

		meta, err := w.Finish()
		if err == nil {
			t.Fatal("expected error when syncDir fails, got nil")
		}
		if !stdErrors.Is(err, injectedSyncErr) {
			t.Fatalf("expected error wrapping injectedSyncErr, got %v", err)
		}
		if meta != nil {
			t.Fatalf("expected nil metadata on error, got %v", meta)
		}

		if fi, err := os.Stat(p); err != nil || fi.Size() == 0 {
			t.Fatalf("file must be published at %q even if directory sync failed: %v", p, err)
		}

		_, errSecond := w.Finish()
		if !stdErrors.Is(errSecond, errors.ErrTableWriterFinalized) {
			t.Fatalf("expected ErrTableWriterFinalized on retry after publication, got %v", errSecond)
		}

		if err := w.Close(); err != nil {
			t.Fatalf("Close after publication must be safe, got %v", err)
		}
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("published file must still exist after Close: %v", err)
		}
	})
}
