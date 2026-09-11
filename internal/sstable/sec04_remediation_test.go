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
