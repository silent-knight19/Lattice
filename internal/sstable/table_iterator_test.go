package sstable

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
)

type testRecord struct {
	key   binary.InternalKey
	value []byte
}

func buildTestSSTable(t *testing.T, opts TableWriterOptions, records []testRecord) (string, *TableReader) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.sst")

	w, err := NewTableWriter(path, opts)
	if err != nil {
		t.Fatalf("failed to create TableWriter: %v", err)
	}

	for _, rec := range records {
		if err := w.Add(rec.key, rec.value); err != nil {
			t.Fatalf("failed to add record: %v", err)
		}
	}

	meta, err := w.Finish()
	if err != nil {
		t.Fatalf("failed to finish TableWriter: %v", err)
	}
	if meta == nil {
		t.Fatalf("expected non-nil SSTableMetadata")
	}

	reader, err := NewTableReader(path)
	if err != nil {
		t.Fatalf("failed to open TableReader: %v", err)
	}

	return path, reader
}

// Test A — Single-block SSTable
func TestTableIterator_A_SingleBlock(t *testing.T) {
	records := []testRecord{
		{key: binary.InternalKey{UserKey: []byte("apple"), SeqNum: 10, OpType: binary.OpTypePut}, value: []byte("red")},
		{key: binary.InternalKey{UserKey: []byte("banana"), SeqNum: 20, OpType: binary.OpTypePut}, value: []byte("yellow")},
		{key: binary.InternalKey{UserKey: []byte("cherry"), SeqNum: 30, OpType: binary.OpTypePut}, value: []byte("dark-red")},
	}

	opts := DefaultTableWriterOptions()
	opts.TargetBlockSize = 4096
	_, reader := buildTestSSTable(t, opts, records)
	defer func() { _ = reader.Close() }()

	it, err := reader.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator: %v", err)
	}
	defer func() { _ = it.Close() }()

	if it.Valid() {
		t.Fatalf("expected iterator to be invalid before first Next()")
	}

	for i, expected := range records {
		if !it.Next() {
			t.Fatalf("Next() returned false at index %d, err: %v", i, it.Err())
		}
		if !it.Valid() {
			t.Fatalf("Valid() returned false at index %d", i)
		}
		if binary.CompareInternalKey(it.Key(), expected.key) != 0 {
			t.Fatalf("key mismatch at index %d: got %s, want %s", i, it.Key(), expected.key)
		}
		if !bytes.Equal(it.Value(), expected.value) {
			t.Fatalf("value mismatch at index %d: got %s, want %s", i, it.Value(), expected.value)
		}
		rawKey := it.RawKey()
		if rawKey == nil {
			t.Fatalf("RawKey() returned nil at index %d", i)
		}
	}

	if it.Next() {
		t.Fatalf("Next() returned true past the end of table")
	}
	if it.Valid() {
		t.Fatalf("Valid() must be false after exhaustion")
	}
	if err := it.Err(); err != nil {
		t.Fatalf("expected nil Err() at EOF, got: %v", err)
	}
}

// Test B — Multi-block SSTable
func TestTableIterator_B_MultiBlock(t *testing.T) {
	const count = 60
	records := make([]testRecord, count)
	for i := 0; i < count; i++ {
		records[i] = testRecord{
			key:   binary.InternalKey{UserKey: []byte(fmt.Sprintf("key-%04d", i)), SeqNum: binary.SeqNum(count - i), OpType: binary.OpTypePut},
			value: []byte(fmt.Sprintf("value-%04d-payload-padding-data", i)),
		}
	}

	opts := DefaultTableWriterOptions()
	opts.TargetBlockSize = 256 // force many small data blocks
	_, reader := buildTestSSTable(t, opts, records)
	defer func() { _ = reader.Close() }()

	if reader.Index().EntryCount() <= 1 {
		t.Fatalf("expected multiple data blocks, got %d", reader.Index().EntryCount())
	}

	it, err := reader.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator: %v", err)
	}
	defer func() { _ = it.Close() }()

	visited := 0
	for it.Next() {
		if visited >= count {
			t.Fatalf("emitted more records than written (%d)", count)
		}
		expected := records[visited]
		if binary.CompareInternalKey(it.Key(), expected.key) != 0 {
			t.Fatalf("key mismatch at index %d: got %s, want %s", visited, it.Key(), expected.key)
		}
		if !bytes.Equal(it.Value(), expected.value) {
			t.Fatalf("value mismatch at index %d: got %s, want %s", visited, it.Value(), expected.value)
		}
		visited++
	}

	if err := it.Err(); err != nil {
		t.Fatalf("unexpected error during multi-block iteration: %v", err)
	}
	if visited != count {
		t.Fatalf("expected to visit %d records, visited %d", count, visited)
	}
}

// Test C — Empty / zero-record behavior
func TestTableIterator_C_EmptyTable(t *testing.T) {
	opts := DefaultTableWriterOptions()
	_, reader := buildTestSSTable(t, opts, nil)
	defer func() { _ = reader.Close() }()

	if reader.Index().EntryCount() != 0 {
		t.Fatalf("expected 0 index entries for empty table, got %d", reader.Index().EntryCount())
	}

	it, err := reader.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator: %v", err)
	}
	defer func() { _ = it.Close() }()

	if it.Valid() {
		t.Fatalf("expected Valid() == false on empty table")
	}
	if it.Next() {
		t.Fatalf("expected Next() == false on empty table")
	}
	if err := it.Err(); err != nil {
		t.Fatalf("expected nil Err() on empty table, got: %v", err)
	}
	if it.Valid() {
		t.Fatalf("expected Valid() == false after Next() on empty table")
	}

	// Seek on empty table
	if err := it.SeekToFirst(); err != nil {
		t.Fatalf("SeekToFirst on empty table failed: %v", err)
	}
	if it.Valid() {
		t.Fatalf("expected Valid() == false after SeekToFirst on empty table")
	}

	if err := it.Seek([]byte("nonexistent")); err != nil {
		t.Fatalf("Seek on empty table failed: %v", err)
	}
	if it.Valid() {
		t.Fatalf("expected Valid() == false after Seek on empty table")
	}
}

// Test D — Cross-block ordering and continuity
func TestTableIterator_D_CrossBlockOrdering(t *testing.T) {
	const count = 100
	records := make([]testRecord, count)
	for i := 0; i < count; i++ {
		records[i] = testRecord{
			key:   binary.InternalKey{UserKey: []byte(fmt.Sprintf("user-%05d", i)), SeqNum: 1000, OpType: binary.OpTypePut},
			value: []byte(fmt.Sprintf("val-%05d", i)),
		}
	}

	opts := DefaultTableWriterOptions()
	opts.TargetBlockSize = 256
	_, reader := buildTestSSTable(t, opts, records)
	defer func() { _ = reader.Close() }()

	it, err := reader.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator: %v", err)
	}
	defer func() { _ = it.Close() }()

	var prevKey *binary.InternalKey
	seen := 0
	for it.Next() {
		curr := it.Key()
		if prevKey != nil {
			if binary.CompareInternalKey(*prevKey, curr) >= 0 {
				t.Fatalf("monotonic ordering violated at entry %d: prev=%s, curr=%s", seen, prevKey, curr)
			}
		}
		k := curr
		prevKey = &k
		seen++
	}

	if err := it.Err(); err != nil {
		t.Fatalf("iteration failed: %v", err)
	}
	if seen != count {
		t.Fatalf("expected %d records, got %d", count, seen)
	}
}

// Test E — Repeated user-key revisions
func TestTableIterator_E_RepeatedUserKeyRevisions(t *testing.T) {
	records := []testRecord{
		{key: binary.InternalKey{UserKey: []byte("alpha"), SeqNum: 100, OpType: binary.OpTypePut}, value: []byte("v100")},
		{key: binary.InternalKey{UserKey: []byte("alpha"), SeqNum: 90, OpType: binary.OpTypeDelete}, value: nil},
		{key: binary.InternalKey{UserKey: []byte("alpha"), SeqNum: 80, OpType: binary.OpTypePut}, value: []byte("v80")},
		{key: binary.InternalKey{UserKey: []byte("beta"), SeqNum: 50, OpType: binary.OpTypePut}, value: []byte("v50")},
	}

	opts := DefaultTableWriterOptions()
	_, reader := buildTestSSTable(t, opts, records)
	defer func() { _ = reader.Close() }()

	it, err := reader.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator: %v", err)
	}
	defer func() { _ = it.Close() }()

	for i, expected := range records {
		if !it.Next() {
			t.Fatalf("expected record %d, got EOF", i)
		}
		if it.Key().SeqNum != expected.key.SeqNum {
			t.Fatalf("seqnum mismatch at index %d: got %d, want %d", i, it.Key().SeqNum, expected.key.SeqNum)
		}
		if it.Key().OpType != expected.key.OpType {
			t.Fatalf("optype mismatch at index %d: got %v, want %v", i, it.Key().OpType, expected.key.OpType)
		}
	}
}

// Test F — PUT + DELETE records (tombstone preservation)
func TestTableIterator_F_TombstonesPreserved(t *testing.T) {
	records := []testRecord{
		{key: binary.InternalKey{UserKey: []byte("k1"), SeqNum: 20, OpType: binary.OpTypePut}, value: []byte("alive")},
		{key: binary.InternalKey{UserKey: []byte("k2"), SeqNum: 30, OpType: binary.OpTypeDelete}, value: nil},
		{key: binary.InternalKey{UserKey: []byte("k3"), SeqNum: 10, OpType: binary.OpTypePut}, value: []byte("also-alive")},
	}

	opts := DefaultTableWriterOptions()
	_, reader := buildTestSSTable(t, opts, records)
	defer func() { _ = reader.Close() }()

	it, err := reader.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator: %v", err)
	}
	defer func() { _ = it.Close() }()

	// Record 0: PUT
	if !it.Next() || it.Key().OpType != binary.OpTypePut || string(it.Value()) != "alive" {
		t.Fatalf("record 0 mismatch")
	}

	// Record 1: DELETE (Tombstone)
	if !it.Next() {
		t.Fatalf("failed to advance to tombstone")
	}
	if it.Key().OpType != binary.OpTypeDelete {
		t.Fatalf("expected tombstone OpTypeDelete, got %v", it.Key().OpType)
	}
	if it.Value() != nil {
		t.Fatalf("expected nil value for tombstone, got %v", it.Value())
	}

	// Record 2: PUT
	if !it.Next() || it.Key().OpType != binary.OpTypePut || string(it.Value()) != "also-alive" {
		t.Fatalf("record 2 mismatch")
	}

	if it.Next() {
		t.Fatalf("expected EOF")
	}
}

// Test G — Large values across block boundaries
func TestTableIterator_G_LargeValues(t *testing.T) {
	val32K := make([]byte, 32*1024)
	val64K := make([]byte, 64*1024)
	for i := range val32K {
		val32K[i] = byte(i % 251)
	}
	for i := range val64K {
		val64K[i] = byte((i + 7) % 251)
	}

	records := []testRecord{
		{key: binary.InternalKey{UserKey: []byte("big-1"), SeqNum: 10, OpType: binary.OpTypePut}, value: val32K},
		{key: binary.InternalKey{UserKey: []byte("big-2"), SeqNum: 20, OpType: binary.OpTypePut}, value: val64K},
	}

	opts := DefaultTableWriterOptions()
	opts.TargetBlockSize = 4096
	_, reader := buildTestSSTable(t, opts, records)
	defer func() { _ = reader.Close() }()

	it, err := reader.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator: %v", err)
	}
	defer func() { _ = it.Close() }()

	if !it.Next() {
		t.Fatalf("record 1 Next() failed: %v", it.Err())
	}
	if !bytes.Equal(it.Value(), val32K) {
		t.Fatalf("record 1 32KB value corrupted")
	}

	if !it.Next() {
		t.Fatalf("record 2 Next() failed: %v", it.Err())
	}
	if !bytes.Equal(it.Value(), val64K) {
		t.Fatalf("record 2 64KB value corrupted")
	}

	if it.Next() {
		t.Fatalf("unexpected record 3")
	}
}

// Test H — Corrupt block payload
func TestTableIterator_H_CorruptBlock(t *testing.T) {
	records := []testRecord{
		{key: binary.InternalKey{UserKey: []byte("k1"), SeqNum: 1, OpType: binary.OpTypePut}, value: []byte("v1")},
		{key: binary.InternalKey{UserKey: []byte("k2"), SeqNum: 2, OpType: binary.OpTypePut}, value: []byte("v2")},
	}
	opts := DefaultTableWriterOptions()
	path, reader := buildTestSSTable(t, opts, records)
	_ = reader.Close()

	// Read file bytes, corrupt byte at data block entry payload offset 2, recompute CRC? No, just corrupt payload.
	data, err := os.ReadFile(path) // #nosec G304 -- test file path
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	// Corrupt first byte of data block
	data[0] ^= 0xFF
	if err := os.WriteFile(path, data, 0600); err != nil { // #nosec G703 -- test file write
		t.Fatalf("failed to write corrupted file: %v", err)
	}

	r, err := NewTableReader(path)
	if err != nil {
		t.Fatalf("failed to open corrupted table: %v", err)
	}
	defer func() { _ = r.Close() }()

	it, err := r.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator: %v", err)
	}
	defer func() { _ = it.Close() }()

	if it.Next() {
		t.Fatalf("expected Next() to fail on corrupted block")
	}
	if it.Valid() {
		t.Fatalf("expected Valid() == false on corrupted block")
	}
	if it.Err() == nil {
		t.Fatalf("expected non-nil Err() on corrupted block")
	}
}

// Test I — Truncated block
func TestTableIterator_I_TruncatedBlock(t *testing.T) {
	records := []testRecord{
		{key: binary.InternalKey{UserKey: []byte("k1"), SeqNum: 1, OpType: binary.OpTypePut}, value: []byte("v1")},
	}
	opts := DefaultTableWriterOptions()
	path, reader := buildTestSSTable(t, opts, records)
	_ = reader.Close()

	// Truncate file so footer is truncated
	if err := os.Truncate(path, 10); err != nil {
		t.Fatalf("failed to truncate: %v", err)
	}

	_, err := OpenTableIterator(path)
	if err == nil {
		t.Fatalf("expected OpenTableIterator to fail on truncated file")
	}
}

// Test J — Corrupt restart metadata
func TestTableIterator_J_CorruptRestartMetadata(t *testing.T) {
	records := []testRecord{
		{key: binary.InternalKey{UserKey: []byte("k1"), SeqNum: 1, OpType: binary.OpTypePut}, value: []byte("v1")},
		{key: binary.InternalKey{UserKey: []byte("k2"), SeqNum: 2, OpType: binary.OpTypePut}, value: []byte("v2")},
	}
	opts := DefaultTableWriterOptions()
	path, reader := buildTestSSTable(t, opts, records)
	handle := reader.Index().Entries()[0].Handle
	_ = reader.Close()

	// Read file bytes, corrupt restart count (set restart count to 0) and recompute CRC
	data, err := os.ReadFile(path) // #nosec G304 -- test file path
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	blockBytes := data[handle.Offset : handle.Offset+handle.Size]
	// restart count is at [len-8 : len-4]
	binary.PutUint32(blockBytes[len(blockBytes)-8:len(blockBytes)-4], 0)
	// recompute CRC
	newCRC := binary.Checksum(blockBytes[:len(blockBytes)-4])
	binary.PutUint32(blockBytes[len(blockBytes)-4:], newCRC)

	if err := os.WriteFile(path, data, 0600); err != nil { // #nosec G703 -- test file write
		t.Fatalf("failed to write corrupted file: %v", err)
	}

	r, err := NewTableReader(path)
	if err != nil {
		t.Fatalf("failed to open table: %v", err)
	}
	defer func() { _ = r.Close() }()

	it, err := r.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator: %v", err)
	}
	defer func() { _ = it.Close() }()

	if it.Next() {
		t.Fatalf("expected Next() to fail on corrupt restart count")
	}
	if it.Err() == nil {
		t.Fatalf("expected non-nil Err() on corrupt restart count")
	}
	var corruptErr *errors.DataBlockCorruptedError
	if !stdErrors.As(it.Err(), &corruptErr) {
		t.Fatalf("expected DataBlockCorruptedError, got: %v", it.Err())
	}
}

// Test K — Corrupt block handle
func TestTableIterator_K_CorruptBlockHandle(t *testing.T) {
	records := []testRecord{
		{key: binary.InternalKey{UserKey: []byte("k1"), SeqNum: 1, OpType: binary.OpTypePut}, value: []byte("v1")},
	}
	opts := DefaultTableWriterOptions()
	_, reader := buildTestSSTable(t, opts, records)
	defer func() { _ = reader.Close() }()

	// Overwrite reader's index entry handle with offset exceeding file boundary
	reader.index.entries[0].Handle.Offset = uint64(reader.FileSize()) + 100 // #nosec G115 -- test offset

	it, err := reader.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator: %v", err)
	}
	defer func() { _ = it.Close() }()

	if it.Next() {
		t.Fatalf("expected Next() to fail on out-of-bounds block handle")
	}
	if it.Err() == nil {
		t.Fatalf("expected non-nil Err() on out-of-bounds block handle")
	}
}

// Test L — CRC failure
func TestTableIterator_L_CRCFailure(t *testing.T) {
	records := []testRecord{
		{key: binary.InternalKey{UserKey: []byte("k1"), SeqNum: 1, OpType: binary.OpTypePut}, value: []byte("v1")},
	}
	opts := DefaultTableWriterOptions()
	path, reader := buildTestSSTable(t, opts, records)
	_ = reader.Close()

	data, err := os.ReadFile(path) // #nosec G304 -- test file path
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	// Flip byte in data block without updating CRC
	data[2] ^= 0x01
	if err := os.WriteFile(path, data, 0600); err != nil { // #nosec G703 -- test file write
		t.Fatalf("failed to write corrupted file: %v", err)
	}

	r, err := NewTableReader(path)
	if err != nil {
		t.Fatalf("failed to open table: %v", err)
	}
	defer func() { _ = r.Close() }()

	it, err := r.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator: %v", err)
	}
	defer func() { _ = it.Close() }()

	if it.Next() {
		t.Fatalf("expected Next() to fail on CRC mismatch")
	}
	if it.Err() == nil {
		t.Fatalf("expected non-nil Err() on CRC mismatch")
	}
	var crcErr *errors.ChecksumMismatchError
	if !stdErrors.As(it.Err(), &crcErr) {
		t.Fatalf("expected ChecksumMismatchError, got: %T (%v)", it.Err(), it.Err())
	}
}

// Test M — Iterator Close idempotence
func TestTableIterator_M_CloseIdempotence(t *testing.T) {
	records := []testRecord{
		{key: binary.InternalKey{UserKey: []byte("k1"), SeqNum: 1, OpType: binary.OpTypePut}, value: []byte("v1")},
	}
	opts := DefaultTableWriterOptions()
	_, reader := buildTestSSTable(t, opts, records)
	defer func() { _ = reader.Close() }()

	it, err := reader.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator: %v", err)
	}

	for i := 0; i < 5; i++ {
		if err := it.Close(); err != nil {
			t.Fatalf("Close() call %d failed: %v", i+1, err)
		}
	}
}

// Test N — Use-after-Close behavior
func TestTableIterator_N_UseAfterClose(t *testing.T) {
	records := []testRecord{
		{key: binary.InternalKey{UserKey: []byte("k1"), SeqNum: 1, OpType: binary.OpTypePut}, value: []byte("v1")},
	}
	opts := DefaultTableWriterOptions()
	_, reader := buildTestSSTable(t, opts, records)
	defer func() { _ = reader.Close() }()

	it, err := reader.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator: %v", err)
	}

	if err := it.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if it.Next() {
		t.Fatalf("Next() returned true after Close()")
	}
	if it.Valid() {
		t.Fatalf("Valid() returned true after Close()")
	}
	if it.Key().UserKey != nil {
		t.Fatalf("Key() returned non-zero after Close()")
	}
	if it.Value() != nil {
		t.Fatalf("Value() returned non-nil after Close()")
	}
	if it.RawKey() != nil {
		t.Fatalf("RawKey() returned non-nil after Close()")
	}
	if err := it.Seek([]byte("k1")); !stdErrors.Is(err, errors.ErrIteratorClosed) {
		t.Fatalf("expected ErrIteratorClosed from Seek after Close, got: %v", err)
	}
	if err := it.SeekToFirst(); !stdErrors.Is(err, errors.ErrIteratorClosed) {
		t.Fatalf("expected ErrIteratorClosed from SeekToFirst after Close, got: %v", err)
	}
	targetIK := binary.InternalKey{UserKey: []byte("k1"), SeqNum: 1, OpType: binary.OpTypePut}
	if err := it.SeekInternalKey(targetIK); !stdErrors.Is(err, errors.ErrIteratorClosed) {
		t.Fatalf("expected ErrIteratorClosed from SeekInternalKey after Close, got: %v", err)
	}
}

// Test O — Two concurrent iterators over one SSTable
func TestTableIterator_O_ConcurrentIterators(t *testing.T) {
	const count = 150
	records := make([]testRecord, count)
	for i := 0; i < count; i++ {
		records[i] = testRecord{
			key:   binary.InternalKey{UserKey: []byte(fmt.Sprintf("key-%05d", i)), SeqNum: binary.SeqNum(i + 1), OpType: binary.OpTypePut},
			value: []byte(fmt.Sprintf("val-%05d", i)),
		}
	}

	opts := DefaultTableWriterOptions()
	opts.TargetBlockSize = 256
	_, reader := buildTestSSTable(t, opts, records)
	defer func() { _ = reader.Close() }()

	const numWorkers = 8
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	errChan := make(chan error, numWorkers)

	for w := 0; w < numWorkers; w++ {
		go func() {
			defer wg.Done()
			it, err := reader.NewIterator()
			if err != nil {
				errChan <- err
				return
			}
			defer func() { _ = it.Close() }()

			visited := 0
			for it.Next() {
				if visited >= count {
					errChan <- fmt.Errorf("visited > count")
					return
				}
				expected := records[visited]
				if binary.CompareInternalKey(it.Key(), expected.key) != 0 {
					errChan <- fmt.Errorf("key mismatch at %d", visited)
					return
				}
				if !bytes.Equal(it.Value(), expected.value) {
					errChan <- fmt.Errorf("value mismatch at %d", visited)
					return
				}
				visited++
			}
			if err := it.Err(); err != nil {
				errChan <- err
				return
			}
			if visited != count {
				errChan <- fmt.Errorf("visited %d != %d", visited, count)
				return
			}
		}()
	}

	wg.Wait()
	close(errChan)

	for err := range errChan {
		if err != nil {
			t.Fatalf("concurrent iterator worker failed: %v", err)
		}
	}
}

// Test P — Differential writer -> iterator comparison
func TestTableIterator_P_DifferentialComparison(t *testing.T) {
	const count = 200
	records := make([]testRecord, count)
	for i := 0; i < count; i++ {
		records[i] = testRecord{
			key:   binary.InternalKey{UserKey: []byte(fmt.Sprintf("diff-key-%04d", i)), SeqNum: binary.SeqNum(count - i), OpType: binary.OpTypePut},
			value: []byte(fmt.Sprintf("diff-val-%04d", i)),
		}
	}

	opts := DefaultTableWriterOptions()
	opts.TargetBlockSize = 512
	_, reader := buildTestSSTable(t, opts, records)
	defer func() { _ = reader.Close() }()

	it, err := reader.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator: %v", err)
	}
	defer func() { _ = it.Close() }()

	diffIdx := 0
	for it.Next() {
		if diffIdx >= count {
			t.Fatalf("iterator produced more records than expected")
		}
		expected := records[diffIdx]
		if binary.CompareInternalKey(it.Key(), expected.key) != 0 {
			t.Fatalf("diff mismatch at %d: got %s, want %s", diffIdx, it.Key(), expected.key)
		}
		if !bytes.Equal(it.Value(), expected.value) {
			t.Fatalf("diff value mismatch at %d: got %s, want %s", diffIdx, it.Value(), expected.value)
		}
		diffIdx++
	}

	if err := it.Err(); err != nil {
		t.Fatalf("iteration error: %v", err)
	}
	if diffIdx != count {
		t.Fatalf("expected %d records, got %d", count, diffIdx)
	}
}

// Test R — Large streaming scan
func TestTableIterator_R_LargeStreamingScan(t *testing.T) {
	const count = 1000
	records := make([]testRecord, count)
	for i := 0; i < count; i++ {
		records[i] = testRecord{
			key:   binary.InternalKey{UserKey: []byte(fmt.Sprintf("stream-key-%06d", i)), SeqNum: binary.SeqNum(count - i), OpType: binary.OpTypePut},
			value: []byte(fmt.Sprintf("stream-val-%06d", i)),
		}
	}

	opts := DefaultTableWriterOptions()
	opts.TargetBlockSize = 512
	_, reader := buildTestSSTable(t, opts, records)
	defer func() { _ = reader.Close() }()

	it, err := reader.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator: %v", err)
	}
	defer func() { _ = it.Close() }()

	countVisited := 0
	for it.Next() {
		countVisited++
	}
	if err := it.Err(); err != nil {
		t.Fatalf("streaming scan error: %v", err)
	}
	if countVisited != count {
		t.Fatalf("expected %d records, visited %d", count, countVisited)
	}
}

// Test S — Error-after-valid-prefix behavior
func TestTableIterator_S_ErrorAfterValidPrefix(t *testing.T) {
	// Build an SSTable with 2 data blocks (e.g. 20 records with small target block size)
	const count = 20
	records := make([]testRecord, count)
	for i := 0; i < count; i++ {
		records[i] = testRecord{
			key:   binary.InternalKey{UserKey: []byte(fmt.Sprintf("pref-%04d", i)), SeqNum: 100, OpType: binary.OpTypePut},
			value: []byte(fmt.Sprintf("val-%04d", i)),
		}
	}

	opts := DefaultTableWriterOptions()
	opts.TargetBlockSize = 128
	path, reader := buildTestSSTable(t, opts, records)

	if reader.Index().EntryCount() < 2 {
		t.Fatalf("expected at least 2 data blocks, got %d", reader.Index().EntryCount())
	}
	secondHandle := reader.Index().Entries()[1].Handle
	_ = reader.Close()

	// Corrupt Block 1 (second block) on disk: flip a byte in Block 1
	data, err := os.ReadFile(path) // #nosec G304 -- test file path
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	data[secondHandle.Offset+2] ^= 0xFF
	if err := os.WriteFile(path, data, 0600); err != nil { // #nosec G703 -- test file write
		t.Fatalf("failed to write corrupted file: %v", err)
	}

	r, err := NewTableReader(path)
	if err != nil {
		t.Fatalf("failed to open table: %v", err)
	}
	defer func() { _ = r.Close() }()

	it, err := r.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator: %v", err)
	}
	defer func() { _ = it.Close() }()

	// First block should yield valid records
	validCount := 0
	for it.Next() {
		validCount++
	}

	// Must NOT be clean EOF!
	if it.Err() == nil {
		t.Fatalf("corruption in second block was masked as clean EOF (Err() == nil)")
	}
	if validCount == 0 {
		t.Fatalf("expected first block to succeed and emit valid prefix")
	}
	if validCount >= count {
		t.Fatalf("expected failure before all %d records were emitted, got %d", count, validCount)
	}
}

// Test Seek, SeekToFirst, and SeekInternalKey
func TestTableIterator_SeekOperations(t *testing.T) {
	records := []testRecord{
		{key: binary.InternalKey{UserKey: []byte("c"), SeqNum: 30, OpType: binary.OpTypePut}, value: []byte("val-c")},
		{key: binary.InternalKey{UserKey: []byte("e"), SeqNum: 50, OpType: binary.OpTypePut}, value: []byte("val-e")},
		{key: binary.InternalKey{UserKey: []byte("g"), SeqNum: 70, OpType: binary.OpTypePut}, value: []byte("val-g")},
		{key: binary.InternalKey{UserKey: []byte("i"), SeqNum: 90, OpType: binary.OpTypePut}, value: []byte("val-i")},
	}

	opts := DefaultTableWriterOptions()
	opts.TargetBlockSize = 64 // multiple blocks
	_, reader := buildTestSSTable(t, opts, records)
	defer func() { _ = reader.Close() }()

	it, err := reader.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator: %v", err)
	}
	defer func() { _ = it.Close() }()

	// 1. Seek exact key
	if err := it.Seek([]byte("e")); err != nil {
		t.Fatalf("Seek('e') failed: %v", err)
	}
	if !it.Valid() || string(it.Key().UserKey) != "e" {
		t.Fatalf("expected to position at 'e', got %s (valid=%v)", it.Key().UserKey, it.Valid())
	}

	// 2. Seek inexact key (between 'e' and 'g')
	if err := it.Seek([]byte("f")); err != nil {
		t.Fatalf("Seek('f') failed: %v", err)
	}
	if !it.Valid() || string(it.Key().UserKey) != "g" {
		t.Fatalf("expected to position at 'g', got %s", it.Key().UserKey)
	}

	// 3. Seek before first key
	if err := it.Seek([]byte("a")); err != nil {
		t.Fatalf("Seek('a') failed: %v", err)
	}
	if !it.Valid() || string(it.Key().UserKey) != "c" {
		t.Fatalf("expected to position at 'c', got %s", it.Key().UserKey)
	}

	// 4. Seek past last key
	if err := it.Seek([]byte("z")); err != nil {
		t.Fatalf("Seek('z') failed: %v", err)
	}
	if it.Valid() {
		t.Fatalf("expected Valid() == false when seeking past end")
	}

	// 5. SeekToFirst
	if err := it.SeekToFirst(); err != nil {
		t.Fatalf("SeekToFirst failed: %v", err)
	}
	if !it.Valid() || string(it.Key().UserKey) != "c" {
		t.Fatalf("expected to position at 'c', got %s", it.Key().UserKey)
	}

	// 6. SeekInternalKey
	target := binary.InternalKey{UserKey: []byte("g"), SeqNum: 70, OpType: binary.OpTypePut}
	if err := it.SeekInternalKey(target); err != nil {
		t.Fatalf("SeekInternalKey failed: %v", err)
	}
	if !it.Valid() || binary.CompareInternalKey(it.Key(), target) != 0 {
		t.Fatalf("expected to position at target, got %s", it.Key())
	}
}

// Test OpenTableIterator ownership
func TestTableIterator_OpenTableIteratorOwnership(t *testing.T) {
	records := []testRecord{
		{key: binary.InternalKey{UserKey: []byte("k1"), SeqNum: 1, OpType: binary.OpTypePut}, value: []byte("v1")},
	}
	opts := DefaultTableWriterOptions()
	path, reader := buildTestSSTable(t, opts, records)
	_ = reader.Close()

	it, err := OpenTableIterator(path)
	if err != nil {
		t.Fatalf("OpenTableIterator failed: %v", err)
	}

	if !it.Next() {
		t.Fatalf("Next failed: %v", it.Err())
	}
	if string(it.Key().UserKey) != "k1" {
		t.Fatalf("unexpected key: %s", it.Key().UserKey)
	}

	// Closing it should close the underlying reader
	if err := it.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Attempting to use reader should fail with ErrTableReaderClosed
	if _, err := it.reader.Seek([]byte("k1")); !stdErrors.Is(err, errors.ErrNilReceiver) && !stdErrors.Is(err, errors.ErrTableReaderClosed) {
		t.Fatalf("expected closed/nil reader error, got: %v", err)
	}
}

// Test nil receiver defenses
func TestTableIterator_NilReceiver(t *testing.T) {
	var it *TableIterator
	if it.Valid() {
		t.Fatalf("nil iterator must report Valid() == false")
	}
	if it.Next() {
		t.Fatalf("nil iterator must report Next() == false")
	}
	if it.Key().UserKey != nil {
		t.Fatalf("nil iterator must return empty Key()")
	}
	if it.RawKey() != nil {
		t.Fatalf("nil iterator must return nil RawKey()")
	}
	if it.Value() != nil {
		t.Fatalf("nil iterator must return nil Value()")
	}
	if err := it.Err(); !stdErrors.Is(err, errors.ErrNilReceiver) {
		t.Fatalf("expected ErrNilReceiver, got: %v", err)
	}
	if err := it.Close(); !stdErrors.Is(err, errors.ErrNilReceiver) {
		t.Fatalf("expected ErrNilReceiver, got: %v", err)
	}
	if err := it.Seek([]byte("key")); !stdErrors.Is(err, errors.ErrNilReceiver) {
		t.Fatalf("expected ErrNilReceiver, got: %v", err)
	}
	if err := it.SeekToFirst(); !stdErrors.Is(err, errors.ErrNilReceiver) {
		t.Fatalf("expected ErrNilReceiver, got: %v", err)
	}
	if err := it.SeekInternalKey(binary.InternalKey{}); !stdErrors.Is(err, errors.ErrNilReceiver) {
		t.Fatalf("expected ErrNilReceiver, got: %v", err)
	}
}
