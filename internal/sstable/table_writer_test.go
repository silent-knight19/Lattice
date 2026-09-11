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
	"github.com/silent-knight19/lattice/internal/memtable"
	"github.com/silent-knight19/lattice/internal/sstable"
)

func makeTestIK(userKey string, seq uint64, op binary.OpType) binary.InternalKey {
	ik, err := binary.NewInternalKey([]byte(userKey), binary.SeqNum(seq), op)
	if err != nil {
		panic(err)
	}
	return ik
}

// TestTableWriter_SingleBlock verifies creation, serialization, and layout of an SSTable
// containing records that fit within a single 4KB data block.
func TestTableWriter_SingleBlock(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "000001.sst")

	opts := sstable.DefaultTableWriterOptions()
	opts.TargetBlockSize = 4096

	writer, err := sstable.NewTableWriter(sstPath, opts)
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}

	const recordCount = 20
	for i := 0; i < recordCount; i++ {
		key := makeTestIK(fmt.Sprintf("key:%04d", i), uint64(100-i), binary.OpTypePut)
		val := []byte(fmt.Sprintf("val:%04d", i))
		if err := writer.Add(key, val); err != nil {
			t.Fatalf("Add failed on record %d: %v", i, err)
		}
	}

	if writer.EntryCount() != recordCount {
		t.Fatalf("expected %d entries, got %d", recordCount, writer.EntryCount())
	}

	meta, err := writer.Finish()
	if err != nil {
		t.Fatalf("Finish failed: %v", err)
	}

	// 1. Verify returned metadata
	if meta.Path != sstPath {
		t.Fatalf("meta.Path %s != %s", meta.Path, sstPath)
	}
	if meta.EntryCount != recordCount {
		t.Fatalf("meta.EntryCount %d != %d", meta.EntryCount, recordCount)
	}
	if meta.DataBlockCount != 1 {
		t.Fatalf("expected 1 data block, got %d", meta.DataBlockCount)
	}

	// 2. Read physical file and independently verify binary structure
	rawBytes, err := os.ReadFile(sstPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if uint64(len(rawBytes)) != meta.FileSize {
		t.Fatalf("physical file size %d != meta.FileSize %d", len(rawBytes), meta.FileSize)
	}

	// 3. Inspect Footer at len - 48
	if len(rawBytes) < sstable.FooterSize {
		t.Fatalf("file too small for footer: %d", len(rawBytes))
	}
	footerBytes := rawBytes[len(rawBytes)-sstable.FooterSize:]
	footer, err := sstable.DecodeFooter(footerBytes)
	if err != nil {
		t.Fatalf("DecodeFooter failed: %v", err)
	}
	if err := footer.ValidateAgainstFileSize(int64(len(rawBytes))); err != nil {
		t.Fatalf("ValidateAgainstFileSize failed: %v", err)
	}

	// 4. Verify MetaIndex block (8 bytes)
	if footer.MetaIndexHandle.Size != 8 {
		t.Fatalf("expected meta handle size 8, got %d", footer.MetaIndexHandle.Size)
	}
	metaBlock := rawBytes[footer.MetaIndexHandle.Offset : footer.MetaIndexHandle.Offset+footer.MetaIndexHandle.Size]
	metaIdx, err := sstable.DecodeBlockIndex(metaBlock)
	if err != nil {
		t.Fatalf("DecodeBlockIndex on meta block failed: %v", err)
	}
	if metaIdx.EntryCount() != 0 {
		t.Fatalf("expected empty meta index, got %d entries", metaIdx.EntryCount())
	}

	// 5. Verify Index block
	indexBlock := rawBytes[footer.IndexHandle.Offset : footer.IndexHandle.Offset+footer.IndexHandle.Size]
	idx, err := sstable.DecodeBlockIndex(indexBlock)
	if err != nil {
		t.Fatalf("DecodeBlockIndex failed: %v", err)
	}
	if idx.EntryCount() != 1 {
		t.Fatalf("expected 1 index entry, got %d", idx.EntryCount())
	}

	// 6. Verify Data Block 0
	dataBlock := rawBytes[0:footer.MetaIndexHandle.Offset]
	if uint64(len(dataBlock)) != footer.MetaIndexHandle.Offset {
		t.Fatalf("data block length mismatch")
	}
	// Verify Data Block CRC32
	expectedCRC := binary.GetUint32(dataBlock[len(dataBlock)-4:])
	actualCRC := binary.Checksum(dataBlock[:len(dataBlock)-4])
	if expectedCRC != actualCRC {
		t.Fatalf("data block CRC mismatch: expected %x, got %x", expectedCRC, actualCRC)
	}
}

// TestTableWriter_MultiBlock verifies SSTable construction with multiple data blocks
// crossing 4KB boundaries, ensuring contiguous offsets, monotonic handles, and correct indexing.
func TestTableWriter_MultiBlock(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "multiblock.sst")

	opts := sstable.DefaultTableWriterOptions()
	opts.TargetBlockSize = 512 // small block size to force many blocks

	writer, err := sstable.NewTableWriter(sstPath, opts)
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}

	const recordCount = 100
	for i := 0; i < recordCount; i++ {
		key := makeTestIK(fmt.Sprintf("prefix:%06d", i), 1, binary.OpTypePut)
		val := bytes.Repeat([]byte{byte(i)}, 64)
		if err := writer.Add(key, val); err != nil {
			t.Fatalf("Add failed on record %d: %v", i, err)
		}
	}

	meta, err := writer.Finish()
	if err != nil {
		t.Fatalf("Finish failed: %v", err)
	}

	if meta.DataBlockCount < 5 {
		t.Fatalf("expected >= 5 data blocks with 512B target, got %d", meta.DataBlockCount)
	}
	if meta.EntryCount != recordCount {
		t.Fatalf("expected %d entries, got %d", recordCount, meta.EntryCount)
	}

	// Inspect file
	rawBytes, err := os.ReadFile(sstPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	footer, err := sstable.DecodeFooter(rawBytes[len(rawBytes)-sstable.FooterSize:])
	if err != nil {
		t.Fatalf("DecodeFooter failed: %v", err)
	}
	if err := footer.ValidateAgainstFileSize(int64(len(rawBytes))); err != nil {
		t.Fatalf("ValidateAgainstFileSize failed: %v", err)
	}

	// Verify Index block
	indexBlock := rawBytes[footer.IndexHandle.Offset : footer.IndexHandle.Offset+footer.IndexHandle.Size]
	idx, err := sstable.DecodeBlockIndex(indexBlock)
	if err != nil {
		t.Fatalf("DecodeBlockIndex failed: %v", err)
	}
	if uint64(idx.EntryCount()) != meta.DataBlockCount {
		t.Fatalf("index entry count %d != meta.DataBlockCount %d", idx.EntryCount(), meta.DataBlockCount)
	}

	entries := idx.Entries()
	var prevEnd uint64 = 0
	for i, entry := range entries {
		// Contiguity invariant: each data block starts exactly where previous ended
		if entry.Handle.Offset != prevEnd {
			t.Fatalf("block %d offset %d != previous end %d", i, entry.Handle.Offset, prevEnd)
		}
		if entry.Handle.Size == 0 {
			t.Fatalf("block %d size is zero", i)
		}

		// Verify CRC of this data block
		blockData := rawBytes[entry.Handle.Offset : entry.Handle.Offset+entry.Handle.Size]
		expCRC := binary.GetUint32(blockData[len(blockData)-4:])
		actCRC := binary.Checksum(blockData[:len(blockData)-4])
		if expCRC != actCRC {
			t.Fatalf("block %d CRC mismatch", i)
		}

		prevEnd = entry.Handle.Offset + entry.Handle.Size
	}

	// MetaIndex block must start exactly at the end of the last data block
	if footer.MetaIndexHandle.Offset != prevEnd {
		t.Fatalf("meta block offset %d != last data block end %d", footer.MetaIndexHandle.Offset, prevEnd)
	}
	// Index block must start exactly at the end of the meta index block
	if footer.IndexHandle.Offset != footer.MetaIndexHandle.Offset+footer.MetaIndexHandle.Size {
		t.Fatalf("index block offset mismatch")
	}
	// Footer must start exactly at the end of the index block
	if uint64(len(rawBytes)-sstable.FooterSize) != footer.IndexHandle.Offset+footer.IndexHandle.Size {
		t.Fatalf("footer offset mismatch")
	}
}

// TestTableWriter_EmptySSTable verifies that finishing an SSTable with 0 records
// produces a valid 64-byte file with valid empty handles and footer.
func TestTableWriter_EmptySSTable(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "empty.sst")

	writer, err := sstable.NewTableWriter(sstPath, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}

	if writer.EntryCount() != 0 {
		t.Fatalf("expected 0 entries")
	}

	meta, err := writer.Finish()
	if err != nil {
		t.Fatalf("Finish on empty writer failed: %v", err)
	}

	// Exactly 8B (meta) + 8B (index) + 48B (footer) = 64B
	const expectedSize = 64
	if meta.FileSize != expectedSize {
		t.Fatalf("expected empty file size %d, got %d", expectedSize, meta.FileSize)
	}
	if meta.DataBlockCount != 0 {
		t.Fatalf("expected 0 data blocks, got %d", meta.DataBlockCount)
	}
	if meta.EntryCount != 0 {
		t.Fatalf("expected 0 entries, got %d", meta.EntryCount)
	}

	rawBytes, err := os.ReadFile(sstPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if len(rawBytes) != expectedSize {
		t.Fatalf("file size mismatch: got %d, want %d", len(rawBytes), expectedSize)
	}

	footer, err := sstable.DecodeFooter(rawBytes[len(rawBytes)-sstable.FooterSize:])
	if err != nil {
		t.Fatalf("DecodeFooter on empty table failed: %v", err)
	}
	if err := footer.ValidateAgainstFileSize(int64(len(rawBytes))); err != nil {
		t.Fatalf("ValidateAgainstFileSize failed: %v", err)
	}

	// Verify both handles point to valid 8B empty blocks
	if footer.MetaIndexHandle != (sstable.BlockHandle{Offset: 0, Size: 8}) {
		t.Fatalf("unexpected meta handle: %+v", footer.MetaIndexHandle)
	}
	if footer.IndexHandle != (sstable.BlockHandle{Offset: 8, Size: 8}) {
		t.Fatalf("unexpected index handle: %+v", footer.IndexHandle)
	}
}

// TestTableWriter_KeyOrderingEnforcement verifies that adding keys out of canonical order
// is strictly rejected with KeyOutOfOrderError.
func TestTableWriter_KeyOrderingEnforcement(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "ordering.sst")

	writer, err := sstable.NewTableWriter(sstPath, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}
	defer func() { _ = writer.Close() }()

	// Add key "b"
	k1 := makeTestIK("b", 1, binary.OpTypePut)
	if err := writer.Add(k1, []byte("val-b")); err != nil {
		t.Fatalf("Add k1 failed: %v", err)
	}

	// Add key "a" (strictly before "b") -> must fail
	k2 := makeTestIK("a", 1, binary.OpTypePut)
	err = writer.Add(k2, []byte("val-a"))
	if err == nil {
		t.Fatalf("expected KeyOutOfOrderError when adding unsorted key")
	}
	var orderErr *errors.KeyOutOfOrderError
	if !stdErrorsAs(err, &orderErr) {
		t.Fatalf("expected error to be *KeyOutOfOrderError, got: %v", err)
	}

	// Add same key "b" with same seq -> must fail (not strictly increasing)
	err = writer.Add(k1, []byte("val-b-duplicate"))
	if err == nil {
		t.Fatalf("expected KeyOutOfOrderError when adding duplicate key")
	}

	// Adding strictly greater key "c" should succeed
	k3 := makeTestIK("c", 1, binary.OpTypePut)
	if err := writer.Add(k3, []byte("val-c")); err != nil {
		t.Fatalf("Add k3 failed: %v", err)
	}
}

// TestTableWriter_CallerMutationIsolation verifies that mutating caller key or value buffers
// after Add does not alter written SSTable contents.
func TestTableWriter_CallerMutationIsolation(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "isolation.sst")

	writer, err := sstable.NewTableWriter(sstPath, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}

	userKey := []byte("user-mutable-key")
	val := []byte("val-mutable-payload")
	ik, _ := binary.NewInternalKey(userKey, 10, binary.OpTypePut)

	if err := writer.Add(ik, val); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	// Mutate caller buffers
	for i := range userKey {
		userKey[i] = 0xFF
	}
	for i := range val {
		val[i] = 0xFF
	}

	meta, err := writer.Finish()
	if err != nil {
		t.Fatalf("Finish failed: %v", err)
	}

	// Check metadata smallest/largest key is uncorrupted
	decodedKey, err := binary.DecodeInternalKey(meta.SmallestKey)
	if err != nil {
		t.Fatalf("DecodeInternalKey failed: %v", err)
	}
	if string(decodedKey.UserKey) != "user-mutable-key" {
		t.Fatalf("smallest key corrupted by caller mutation: %q", string(decodedKey.UserKey))
	}
}

// TestTableWriter_PreExistingFileConflict verifies that NewTableWriter rejects overwriting
// an existing finalized SSTable file.
func TestTableWriter_PreExistingFileConflict(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "existing.sst")

	if err := os.WriteFile(sstPath, []byte("already-exists"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	_, err := sstable.NewTableWriter(sstPath, sstable.DefaultTableWriterOptions())
	if err == nil {
		t.Fatalf("expected ErrSSTableExists, got nil")
	}
	if !stdErrorsIs(err, errors.ErrSSTableExists) {
		t.Fatalf("expected ErrSSTableExists, got %v", err)
	}
}

// TestTableWriter_AtomicStagingAndCleanup verifies that .tmp staging files exist during
// writing, are renamed on Finish, and are removed on Close without Finish.
func TestTableWriter_AtomicStagingAndCleanup(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "atomic.sst")
	tmpPath := sstPath + ".tmp"

	// 1. Staging file exists during writing
	writer, err := sstable.NewTableWriter(sstPath, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}

	if _, err := os.Stat(tmpPath); err != nil {
		t.Fatalf("expected .tmp file to exist during write: %v", err)
	}
	if _, err := os.Stat(sstPath); !os.IsNotExist(err) {
		t.Fatalf("final .sst file must NOT exist before Finish")
	}

	_ = writer.Add(makeTestIK("k1", 1, binary.OpTypePut), []byte("v1"))

	// 2. Finish renames .tmp to final .sst
	if _, err := writer.Finish(); err != nil {
		t.Fatalf("Finish failed: %v", err)
	}

	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf(".tmp file must be removed after Finish")
	}
	if _, err := os.Stat(sstPath); err != nil {
		t.Fatalf("final .sst file must exist after Finish: %v", err)
	}

	// 3. Close without Finish unlinks .tmp
	sstPath2 := filepath.Join(dir, "abandoned.sst")
	tmpPath2 := sstPath2 + ".tmp"
	writer2, err := sstable.NewTableWriter(sstPath2, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}
	_ = writer2.Add(makeTestIK("k2", 1, binary.OpTypePut), []byte("v2"))

	if err := writer2.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if _, err := os.Stat(tmpPath2); !os.IsNotExist(err) {
		t.Fatalf(".tmp file must be removed after Close")
	}
	if _, err := os.Stat(sstPath2); !os.IsNotExist(err) {
		t.Fatalf("final .sst file must NOT exist after abandoned Close")
	}
}

// TestTableWriter_LifecycleStates verifies state machine transitions and error enforcement
// across repeated operations.
func TestTableWriter_LifecycleStates(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "lifecycle.sst")

	writer, err := sstable.NewTableWriter(sstPath, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}

	_ = writer.Add(makeTestIK("k1", 1, binary.OpTypePut), []byte("v1"))
	_, err = writer.Finish()
	if err != nil {
		t.Fatalf("Finish failed: %v", err)
	}

	// Add after Finish -> ErrTableWriterFinalized
	err = writer.Add(makeTestIK("k2", 1, binary.OpTypePut), []byte("v2"))
	if !stdErrorsIs(err, errors.ErrTableWriterFinalized) {
		t.Fatalf("expected ErrTableWriterFinalized on Add after Finish, got %v", err)
	}

	// Finish twice -> ErrTableWriterFinalized
	_, err = writer.Finish()
	if !stdErrorsIs(err, errors.ErrTableWriterFinalized) {
		t.Fatalf("expected ErrTableWriterFinalized on repeated Finish, got %v", err)
	}

	// Close after Finish -> safe no-op
	if err := writer.Close(); err != nil {
		t.Fatalf("Close after Finish returned error: %v", err)
	}

	// Test Add after Close
	sstPath2 := filepath.Join(dir, "closed.sst")
	writer2, _ := sstable.NewTableWriter(sstPath2, sstable.DefaultTableWriterOptions())
	_ = writer2.Close()

	err = writer2.Add(makeTestIK("k1", 1, binary.OpTypePut), []byte("v1"))
	if !stdErrorsIs(err, errors.ErrTableWriterClosed) {
		t.Fatalf("expected ErrTableWriterClosed on Add after Close, got %v", err)
	}
}

// TestTableWriter_BuildFromMemTableIterator verifies end-to-end integration with a real
// memtable.SkipList iterator.
func TestTableWriter_BuildFromMemTableIterator(t *testing.T) {
	sl := memtable.NewSkipList()
	const count = 250
	for i := 0; i < count; i++ {
		ik := makeTestIK(fmt.Sprintf("user:%06d", i), uint64(count-i), binary.OpTypePut)
		val := []byte(fmt.Sprintf("profile-data-%06d", i))
		if err := sl.Insert(ik, val); err != nil {
			t.Fatalf("SkipList Insert failed: %v", err)
		}
	}

	dir := t.TempDir()
	sstPath := filepath.Join(dir, "from_memtable.sst")

	writer, err := sstable.NewTableWriter(sstPath, sstable.TableWriterOptions{
		TargetBlockSize: 1024,
		RestartInterval: 16,
		FileMode:        0644,
	})
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}

	iter := sl.NewIterator()
	meta, err := writer.Build(iter)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	if meta.EntryCount != count {
		t.Fatalf("expected %d entries, got %d", count, meta.EntryCount)
	}
	if meta.DataBlockCount < 2 {
		t.Fatalf("expected multiple data blocks, got %d", meta.DataBlockCount)
	}

	// Verify file is readable and valid
	raw, err := os.ReadFile(sstPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	footer, err := sstable.DecodeFooter(raw[len(raw)-sstable.FooterSize:])
	if err != nil {
		t.Fatalf("DecodeFooter failed: %v", err)
	}
	if err := footer.ValidateAgainstFileSize(int64(len(raw))); err != nil {
		t.Fatalf("ValidateAgainstFileSize failed: %v", err)
	}
}

// TestTableWriter_FaultInjection_DataBlockWriteFailure verifies that a disk write failure
// during block flushing aborts the writer, cleans up the staging file, and leaves no corrupt target file.
func TestTableWriter_FaultInjection_DataBlockWriteFailure(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "fault_write.sst")
	tmpPath := sstPath + ".tmp"

	writer, err := sstable.NewTableWriter(sstPath, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}

	injectedErr := stdErrors.New("simulated disk I/O error on write")
	writer.SetWriteFnForTesting(func(f *os.File, p []byte) (int, error) {
		return 0, injectedErr
	})

	_ = writer.Add(makeTestIK("k1", 1, binary.OpTypePut), []byte("v1"))

	_, err = writer.Finish()
	if err == nil {
		t.Fatalf("expected error from Finish() with injected write failure")
	}
	if !stdErrorsIs(err, injectedErr) {
		t.Fatalf("expected injected error, got: %v", err)
	}

	// Staging file must be cleaned up
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("staging .tmp file was not cleaned up after write error")
	}
	// Target file must not exist
	if _, err := os.Stat(sstPath); !os.IsNotExist(err) {
		t.Fatalf("target .sst file must not exist after write error")
	}

	// Writer should reject subsequent operations
	err = writer.Add(makeTestIK("k2", 1, binary.OpTypePut), []byte("v2"))
	if err == nil {
		t.Fatalf("expected error on Add after error state")
	}
}

// TestTableWriter_FaultInjection_ShortWrite verifies handling of short writes without error.
func TestTableWriter_FaultInjection_ShortWrite(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "short_write.sst")

	writer, err := sstable.NewTableWriter(sstPath, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}

	writer.SetWriteFnForTesting(func(f *os.File, p []byte) (int, error) {
		return 0, nil // 0 bytes written without error -> short write
	})

	_ = writer.Add(makeTestIK("k1", 1, binary.OpTypePut), []byte("v1"))

	_, err = writer.Finish()
	if err == nil {
		t.Fatalf("expected short write error, got nil")
	}
}

// TestTableWriter_FaultInjection_SyncFailure verifies that a sync barrier failure
// aborts publication and unlinks the staging file.
func TestTableWriter_FaultInjection_SyncFailure(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "fault_sync.sst")
	tmpPath := sstPath + ".tmp"

	writer, err := sstable.NewTableWriter(sstPath, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}

	injectedErr := stdErrors.New("simulated fdatasync failure")
	writer.SetSyncFnForTesting(func(f *os.File) error {
		return injectedErr
	})

	_ = writer.Add(makeTestIK("k1", 1, binary.OpTypePut), []byte("v1"))

	_, err = writer.Finish()
	if err == nil {
		t.Fatalf("expected sync error, got nil")
	}
	if !stdErrorsIs(err, injectedErr) {
		t.Fatalf("expected %v, got %v", injectedErr, err)
	}

	// Staging file must be cleaned up
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("staging file not cleaned up after sync failure")
	}
	if _, err := os.Stat(sstPath); !os.IsNotExist(err) {
		t.Fatalf("target file must not exist after sync failure")
	}
}

// TestTableWriter_FaultInjection_CloseFailure verifies handling of close errors.
func TestTableWriter_FaultInjection_CloseFailure(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "fault_close.sst")

	writer, err := sstable.NewTableWriter(sstPath, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}

	injectedErr := stdErrors.New("simulated close failure")
	writer.SetCloseFnForTesting(func(f *os.File) error {
		_ = f.Close()
		return injectedErr
	})

	_ = writer.Add(makeTestIK("k1", 1, binary.OpTypePut), []byte("v1"))

	_, err = writer.Finish()
	if err == nil {
		t.Fatalf("expected close error, got nil")
	}
	if !stdErrorsIs(err, injectedErr) {
		t.Fatalf("expected %v, got %v", injectedErr, err)
	}
}

// TestTableWriter_NewTableWriterWithFile verifies direct writing to an open *os.File.
func TestTableWriter_NewTableWriterWithFile(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "direct_*.sst")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer func() { _ = file.Close() }()

	writer, err := sstable.NewTableWriterWithFile(file, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("NewTableWriterWithFile failed: %v", err)
	}

	for i := 0; i < 10; i++ {
		_ = writer.Add(makeTestIK(fmt.Sprintf("k:%02d", i), 1, binary.OpTypePut), []byte("val"))
	}

	meta, err := writer.Finish()
	if err != nil {
		t.Fatalf("Finish failed: %v", err)
	}

	if meta.EntryCount != 10 {
		t.Fatalf("expected 10 entries, got %d", meta.EntryCount)
	}
}

// TestTableWriter_AddRaw verifies AddRaw with valid and malformed internal key byte buffers.
func TestTableWriter_AddRaw(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "raw.sst")

	writer, err := sstable.NewTableWriter(sstPath, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}
	defer func() { _ = writer.Close() }()

	// 1. Valid raw key
	ik := makeTestIK("rawKey1", 1, binary.OpTypePut)
	rawKey := binary.AppendInternalKey(nil, ik)
	if err := writer.AddRaw(rawKey, []byte("rawVal1")); err != nil {
		t.Fatalf("AddRaw valid failed: %v", err)
	}

	// 2. Malformed raw key (< 9 bytes)
	err = writer.AddRaw([]byte("short"), []byte("val"))
	if err == nil {
		t.Fatalf("expected error from AddRaw with truncated key")
	}
	if !stdErrorsIs(err, errors.ErrInternalKeyTruncated) {
		t.Fatalf("expected ErrInternalKeyTruncated, got: %v", err)
	}
}

// TestTableWriter_Accounting verifies that BlockCount, EntryCount, BytesWritten, and EstimatedSize
// track writes accurately.
func TestTableWriter_Accounting(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "accounting.sst")

	opts := sstable.DefaultTableWriterOptions()
	opts.TargetBlockSize = 256

	writer, err := sstable.NewTableWriter(sstPath, opts)
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}

	if writer.EntryCount() != 0 || writer.BlockCount() != 0 || writer.BytesWritten() != 0 {
		t.Fatalf("initial metrics must be zero")
	}

	const n = 50
	for i := 0; i < n; i++ {
		_ = writer.Add(makeTestIK(fmt.Sprintf("item:%04d", i), 1, binary.OpTypePut), bytes.Repeat([]byte{0xAB}, 32))
	}

	if writer.EntryCount() != n {
		t.Fatalf("entry count %d != %d", writer.EntryCount(), n)
	}
	if writer.BlockCount() == 0 {
		t.Fatalf("expected > 0 flushed blocks with 256B target")
	}
	if writer.BytesWritten() == 0 {
		t.Fatalf("expected > 0 bytes written to file")
	}
	if writer.EstimatedSize() < writer.BytesWritten() {
		t.Fatalf("estimated size %d must be >= bytes written %d", writer.EstimatedSize(), writer.BytesWritten())
	}

	meta, err := writer.Finish()
	if err != nil {
		t.Fatalf("Finish failed: %v", err)
	}

	if meta.FileSize != writer.BytesWritten() {
		t.Fatalf("meta.FileSize %d != final BytesWritten %d", meta.FileSize, writer.BytesWritten())
	}
}

// Helper wrapper functions
func stdErrorsIs(err, target error) bool {
	return stdErrors.Is(err, target)
}

func stdErrorsAs(err error, target any) bool {
	return stdErrors.As(err, target)
}
