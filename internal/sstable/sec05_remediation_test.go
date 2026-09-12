package sstable_test

import (
	stdErrors "errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/filter"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// TestSecurity_Remediation_SEC_P05_01_OversizedFilterBlockHandle verifies that
// TableReader.ReadFilterBlock rejects filter block handles exceeding MaxBitsetBytes + FilterBlockTrailerSize
// without allocating heap memory, preventing Out-Of-Memory denial of service attacks.
func TestSecurity_Remediation_SEC_P05_01_OversizedFilterBlockHandle(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "oversized_filter_handle.sst")

	// 1. Build a valid minimal SSTable first
	writer, err := sstable.NewTableWriter(sstPath, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("failed to create table writer: %v", err)
	}
	ik := binary.InternalKey{
		UserKey: []byte("user-key-001"),
		SeqNum:  1,
		OpType:  binary.OpTypePut,
	}
	if err := writer.Add(ik, []byte("val-001")); err != nil {
		t.Fatalf("failed to add record: %v", err)
	}
	meta, err := writer.Finish()
	if err != nil {
		t.Fatalf("failed to finish table: %v", err)
	}

	// 2. Read the raw SSTable and inject a malicious MetaIndex block pointing to an oversized filter handle
	rawBytes, err := os.ReadFile(sstPath)
	if err != nil {
		t.Fatalf("failed to read SSTable: %v", err)
	}

	// Forge an oversized filter handle exceeding MaxBitsetBytes + FilterBlockTrailerSize (256 MiB + 13 B)
	oversizedSize := uint64(filter.MaxBitsetBytes + filter.FilterBlockTrailerSize + 1024)
	maliciousEntries := map[string]sstable.BlockHandle{
		filter.FilterMetaKey: {
			Offset: 0,
			Size:   oversizedSize,
		},
	}
	maliciousMetaBytes := sstable.BuildMetaIndexBlock(maliciousEntries)

	// Reconstruct SSTable:
	// Replace MetaIndex region with maliciousMetaBytes, then write Index, then Footer
	metaOffset := meta.MetaIndexHandle.Offset
	newTable := append([]byte{}, rawBytes[:metaOffset]...)
	newMetaOffset := uint64(len(newTable))
	newTable = append(newTable, maliciousMetaBytes...)
	newMetaSize := uint64(len(maliciousMetaBytes))

	// Re-read index block from original table
	indexOffset := meta.IndexHandle.Offset
	indexSize := meta.IndexHandle.Size
	indexBytes := rawBytes[indexOffset : indexOffset+indexSize]

	newIndexOffset := uint64(len(newTable))
	newTable = append(newTable, indexBytes...)

	// Construct updated footer pointing to new MetaIndex
	footer := sstable.Footer{
		MetaIndexHandle: sstable.BlockHandle{
			Offset: newMetaOffset,
			Size:   newMetaSize,
		},
		IndexHandle: sstable.BlockHandle{
			Offset: newIndexOffset,
			Size:   indexSize,
		},
	}
	newTable = footer.AppendTo(newTable)

	corruptPath := filepath.Join(dir, "malicious_filter_size.sst")
	if err := os.WriteFile(corruptPath, newTable, 0600); err != nil {
		t.Fatalf("failed to write malicious SSTable: %v", err)
	}

	// 3. Open with TableReader and attempt ReadFilterBlock
	reader, err := sstable.OpenTableReader(corruptPath)
	if err != nil {
		t.Fatalf("OpenTableReader failed unexpectedly: %v", err)
	}
	defer func() { _ = reader.Close() }()

	bf, readErr := reader.ReadFilterBlock()
	if readErr == nil {
		t.Fatalf("expected ReadFilterBlock to fail for oversized handle, but got nil error and filter=%+v", bf)
	}

	var invalidHandleErr *errors.InvalidBlockHandleError
	if !stdErrors.As(readErr, &invalidHandleErr) {
		t.Fatalf("expected *errors.InvalidBlockHandleError, got: %T (%v)", readErr, readErr)
	}
	if !strings.Contains(invalidHandleErr.Reason, "exceeds maximum filter block capacity") {
		t.Errorf("expected reason to mention maximum capacity, got: %s", invalidHandleErr.Reason)
	}
	if invalidHandleErr.Size != oversizedSize {
		t.Errorf("expected size %d, got %d", oversizedSize, invalidHandleErr.Size)
	}
}

// TestSecurity_Remediation_SEC_P05_01_ArchitectureIntegerOverflow verifies that
// TableReader.ReadFilterBlock rejects handles with Size > math.MaxInt or Offset > math.MaxInt64
// without panicking on slice bounds.
func TestSecurity_Remediation_SEC_P05_01_ArchitectureIntegerOverflow(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "int_overflow.sst")

	writer, err := sstable.NewTableWriter(sstPath, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("failed to create table writer: %v", err)
	}
	ik := binary.InternalKey{
		UserKey: []byte("key-01"),
		SeqNum:  1,
		OpType:  binary.OpTypePut,
	}
	if err := writer.Add(ik, []byte("val-01")); err != nil {
		t.Fatalf("failed to add record: %v", err)
	}
	meta, err := writer.Finish()
	if err != nil {
		t.Fatalf("failed to finish table: %v", err)
	}

	rawBytes, err := os.ReadFile(sstPath)
	if err != nil {
		t.Fatalf("failed to read SSTable: %v", err)
	}

	// Forge handle with Size = math.MaxUint64
	maliciousEntries := map[string]sstable.BlockHandle{
		filter.FilterMetaKey: {
			Offset: 0,
			Size:   math.MaxUint64,
		},
	}
	maliciousMetaBytes := sstable.BuildMetaIndexBlock(maliciousEntries)

	metaOffset := meta.MetaIndexHandle.Offset
	newTable := append([]byte{}, rawBytes[:metaOffset]...)
	newMetaOffset := uint64(len(newTable))
	newTable = append(newTable, maliciousMetaBytes...)
	newMetaSize := uint64(len(maliciousMetaBytes))

	indexOffset := meta.IndexHandle.Offset
	indexSize := meta.IndexHandle.Size
	indexBytes := rawBytes[indexOffset : indexOffset+indexSize]

	newIndexOffset := uint64(len(newTable))
	newTable = append(newTable, indexBytes...)

	footer := sstable.Footer{
		MetaIndexHandle: sstable.BlockHandle{
			Offset: newMetaOffset,
			Size:   newMetaSize,
		},
		IndexHandle: sstable.BlockHandle{
			Offset: newIndexOffset,
			Size:   indexSize,
		},
	}
	newTable = footer.AppendTo(newTable)

	corruptPath := filepath.Join(dir, "overflow_filter_size.sst")
	if err := os.WriteFile(corruptPath, newTable, 0600); err != nil {
		t.Fatalf("failed to write SSTable: %v", err)
	}

	reader, err := sstable.OpenTableReader(corruptPath)
	if err != nil {
		t.Fatalf("OpenTableReader failed unexpectedly: %v", err)
	}
	defer func() { _ = reader.Close() }()

	_, readErr := reader.ReadFilterBlock()
	if readErr == nil {
		t.Fatalf("expected ReadFilterBlock to fail for math.MaxUint64 handle size")
	}

	var invalidHandleErr *errors.InvalidBlockHandleError
	if !stdErrors.As(readErr, &invalidHandleErr) {
		t.Fatalf("expected *errors.InvalidBlockHandleError, got: %T (%v)", readErr, readErr)
	}
}

// TestSecurity_Remediation_SEC_P05_02_FilterBuilderAddKeyFailurePropagates verifies that
// TableWriter.Add fails closed immediately if filterBuilder.AddKey returns an error,
// preventing silent Bloom filter false negatives and read data loss.
func TestSecurity_Remediation_SEC_P05_02_FilterBuilderAddKeyFailurePropagates(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "filter_add_fail.sst")

	// Create a filter builder and finish it immediately so subsequent AddKey calls fail
	fb := filter.NewFilterBlockBuilder(100)
	_ = fb.Finish() // Transitions fb.finished = true

	opts := sstable.DefaultTableWriterOptions()
	opts.FilterBuilder = fb

	writer, err := sstable.NewTableWriter(sstPath, opts)
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}
	defer func() { _ = writer.Close() }()

	ik := binary.InternalKey{
		UserKey: []byte("user_key_001"),
		SeqNum:  1,
		OpType:  binary.OpTypePut,
	}

	// Attempt to add a key with the sealed filter builder
	addErr := writer.Add(ik, []byte("val_001"))
	if addErr == nil {
		t.Fatalf("expected writer.Add to fail when filterBuilder.AddKey returns error, but succeeded")
	}

	if !stdErrors.Is(addErr, errors.ErrFilterFinished) {
		t.Fatalf("expected addErr to wrap errors.ErrFilterFinished, got: %v", addErr)
	}
}

// TestSecurity_Remediation_SEC_P05_04_TableReaderRejectsNonRegularFile verifies that
// OpenTableReader and NewTableReaderWithFile fail closed when given a directory path or non-regular file descriptor.
func TestSecurity_Remediation_SEC_P05_04_TableReaderRejectsNonRegularFile(t *testing.T) {
	dir := t.TempDir()

	// 1. OpenTableReader on a directory path
	reader, err := sstable.OpenTableReader(dir)
	if err == nil {
		_ = reader.Close()
		t.Fatalf("expected OpenTableReader on directory to fail, but succeeded")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("expected error message to mention 'not a regular file', got: %v", err)
	}

	// 2. NewTableReaderWithFile on an open directory descriptor
	dirFile, openErr := os.Open(dir)
	if openErr != nil {
		t.Fatalf("failed to open directory: %v", openErr)
	}
	// Note: NewTableReaderWithFile assumes ownership and closes file on failure

	reader2, err2 := sstable.NewTableReaderWithFile(dirFile)
	if err2 == nil {
		_ = reader2.Close()
		t.Fatalf("expected NewTableReaderWithFile on directory file descriptor to fail, but succeeded")
	}
	if !strings.Contains(err2.Error(), "not a regular file") {
		t.Errorf("expected error message to mention 'not a regular file', got: %v", err2)
	}
}
