package compaction

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
	"github.com/silent-knight19/lattice/internal/version"
)

// mockRecord represents an in-memory record for testing iterators.
type mockRecord struct {
	key binary.InternalKey
	val []byte
}

// mockSliceIterator provides a controlled in-memory implementation of sstable.Iterator.
type mockSliceIterator struct {
	records []mockRecord
	idx     int
	err     error
	closed  bool
}

func newMockSliceIterator(records []mockRecord) *mockSliceIterator {
	return &mockSliceIterator{
		records: records,
		idx:     -1,
	}
}

func (m *mockSliceIterator) Valid() bool {
	return !m.closed && m.err == nil && m.idx >= 0 && m.idx < len(m.records)
}

func (m *mockSliceIterator) Next() bool {
	if m.closed || m.err != nil {
		return false
	}
	m.idx++
	return m.idx < len(m.records)
}

func (m *mockSliceIterator) Key() binary.InternalKey {
	if !m.Valid() {
		return binary.InternalKey{}
	}
	return m.records[m.idx].key
}

func (m *mockSliceIterator) Value() []byte {
	if !m.Valid() {
		return nil
	}
	return m.records[m.idx].val
}

func (m *mockSliceIterator) Err() error {
	return m.err
}

func (m *mockSliceIterator) Close() error {
	m.closed = true
	return nil
}

// simpleSeqAllocator returns a monotonic allocator starting at initialNum.
func simpleSeqAllocator(initialNum uint64) FileNumAllocator {
	cur := initialNum
	return func() (uint64, error) {
		num := atomic.AddUint64(&cur, 1) - 1
		return num, nil
	}
}

// ============================================================================
// TEST MATRIX: A THROUGH T
// ============================================================================

// TestCompactionOutput_A_EmptyInput verifies that empty input produces zero output files.
func TestCompactionOutput_A_EmptyInput(t *testing.T) {
	dbDir := t.TempDir()
	alloc := simpleSeqAllocator(1)
	iter := newMockSliceIterator(nil)
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool { return true })

	cfg := DefaultCompactionOutputConfig()
	cfg.DbDir = dbDir
	cfg.TargetLevel = 1

	out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Files) != 0 {
		t.Fatalf("expected 0 files, got %d", len(out.Files))
	}
	if out.Stats.WrittenRecords != 0 || out.Stats.OutputFilesCount != 0 {
		t.Fatalf("expected 0 written records, got %d", out.Stats.WrittenRecords)
	}

	// Verify no .sst files exist in directory
	entries, err := os.ReadDir(dbDir)
	if err != nil {
		t.Fatalf("failed to read dbDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 directory entries, found %d", len(entries))
	}
}

// TestCompactionOutput_B_SinglePut verifies that a single PUT produces exactly 1 valid SSTable.
func TestCompactionOutput_B_SinglePut(t *testing.T) {
	dbDir := t.TempDir()
	alloc := simpleSeqAllocator(1)
	records := []mockRecord{
		{
			key: binary.InternalKey{UserKey: []byte("key_01"), SeqNum: 100, OpType: binary.OpTypePut},
			val: []byte("val_01"),
		},
	}
	iter := newMockSliceIterator(records)
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool { return true })

	cfg := DefaultCompactionOutputConfig()
	cfg.DbDir = dbDir
	cfg.TargetLevel = 1

	out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
	if err != nil {
		t.Fatalf("BuildCompactionOutput failed: %v", err)
	}

	if len(out.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(out.Files))
	}
	f := out.Files[0]
	if f.FileNum != 1 {
		t.Fatalf("expected FileNum 1, got %d", f.FileNum)
	}
	if f.EntryCount != 1 {
		t.Fatalf("expected EntryCount 1, got %d", f.EntryCount)
	}
	if !bytes.Equal(f.SmallestUserKey, []byte("key_01")) || !bytes.Equal(f.LargestUserKey, []byte("key_01")) {
		t.Fatalf("user key mismatch: got smallest=%s, largest=%s", f.SmallestUserKey, f.LargestUserKey)
	}

	// Reopen via TableReader and verify contents
	reader, err := sstable.NewTableReader(f.Path)
	if err != nil {
		t.Fatalf("failed to reopen generated sstable: %v", err)
	}
	defer func() { _ = reader.Close() }()

	rit, err := reader.NewIterator()
	if err != nil {
		t.Fatalf("failed to create reader iterator: %v", err)
	}
	defer func() { _ = rit.Close() }()

	if !rit.Next() {
		t.Fatalf("expected 1 record in table")
	}
	if !bytes.Equal(rit.Key().UserKey, []byte("key_01")) || !bytes.Equal(rit.Value(), []byte("val_01")) {
		t.Fatalf("record content mismatch")
	}
	if rit.Next() {
		t.Fatalf("unexpected extra record in table")
	}
}

// TestCompactionOutput_C_MultipleRecords verifies canonical ordering preservation across multiple records.
func TestCompactionOutput_C_MultipleRecords(t *testing.T) {
	dbDir := t.TempDir()
	alloc := simpleSeqAllocator(1)
	records := []mockRecord{
		{key: binary.InternalKey{UserKey: []byte("a"), SeqNum: 300, OpType: binary.OpTypePut}, val: []byte("val_a")},
		{key: binary.InternalKey{UserKey: []byte("b"), SeqNum: 250, OpType: binary.OpTypePut}, val: []byte("val_b")},
		{key: binary.InternalKey{UserKey: []byte("c"), SeqNum: 200, OpType: binary.OpTypePut}, val: []byte("val_c")},
		{key: binary.InternalKey{UserKey: []byte("d"), SeqNum: 150, OpType: binary.OpTypePut}, val: []byte("val_d")},
	}
	iter := newMockSliceIterator(records)
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool { return true })

	cfg := DefaultCompactionOutputConfig()
	cfg.DbDir = dbDir
	cfg.TargetLevel = 1

	out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
	if err != nil {
		t.Fatalf("failed: %v", err)
	}
	if len(out.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(out.Files))
	}
	if out.Files[0].EntryCount != 4 {
		t.Fatalf("expected 4 entries, got %d", out.Files[0].EntryCount)
	}
	if !bytes.Equal(out.Files[0].SmallestUserKey, []byte("a")) || !bytes.Equal(out.Files[0].LargestUserKey, []byte("d")) {
		t.Fatalf("range mismatch")
	}
}

// TestCompactionOutput_D_SafeTombstoneOmission verifies that a provably safe tombstone is physically omitted.
func TestCompactionOutput_D_SafeTombstoneOmission(t *testing.T) {
	dbDir := t.TempDir()
	alloc := simpleSeqAllocator(1)
	records := []mockRecord{
		{key: binary.InternalKey{UserKey: []byte("key_alive"), SeqNum: 200, OpType: binary.OpTypePut}, val: []byte("val")},
		{key: binary.InternalKey{UserKey: []byte("key_dead"), SeqNum: 250, OpType: binary.OpTypeDelete}, val: nil},
	}
	iter := newMockSliceIterator(records)
	// Safety decision: key_dead is safe to drop
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool {
		return bytes.Equal(userKey, []byte("key_dead"))
	})

	cfg := DefaultCompactionOutputConfig()
	cfg.DbDir = dbDir
	cfg.TargetLevel = 1

	out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
	if err != nil {
		t.Fatalf("failed: %v", err)
	}
	if out.Stats.OmittedTombstones != 1 {
		t.Fatalf("expected 1 omitted tombstone, got %d", out.Stats.OmittedTombstones)
	}
	if out.Stats.RetainedTombstones != 0 {
		t.Fatalf("expected 0 retained tombstones, got %d", out.Stats.RetainedTombstones)
	}
	if out.Files[0].EntryCount != 1 {
		t.Fatalf("expected 1 record in output SSTable, got %d", out.Files[0].EntryCount)
	}

	// Verify key_dead is not present in output file
	reader, err := sstable.NewTableReader(out.Files[0].Path)
	if err != nil {
		t.Fatalf("failed to open reader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	rit, err := reader.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator: %v", err)
	}
	defer func() { _ = rit.Close() }()

	if !rit.Next() {
		t.Fatalf("expected 1 record")
	}
	if !bytes.Equal(rit.Key().UserKey, []byte("key_alive")) {
		t.Fatalf("unexpected record: %s", rit.Key().UserKey)
	}
	if rit.Next() {
		t.Fatalf("expected no more records, found extra")
	}
}

// TestCompactionOutput_E_UnsafeTombstonePreservation verifies that an unsafe tombstone is physically retained.
func TestCompactionOutput_E_UnsafeTombstonePreservation(t *testing.T) {
	dbDir := t.TempDir()
	alloc := simpleSeqAllocator(1)
	records := []mockRecord{
		{key: binary.InternalKey{UserKey: []byte("key_alive"), SeqNum: 200, OpType: binary.OpTypePut}, val: []byte("val")},
		{key: binary.InternalKey{UserKey: []byte("key_tombstone"), SeqNum: 250, OpType: binary.OpTypeDelete}, val: nil},
	}
	iter := newMockSliceIterator(records)
	// Safety decision: key_tombstone is UNSAFE to drop (returns false)
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool {
		return false
	})

	cfg := DefaultCompactionOutputConfig()
	cfg.DbDir = dbDir
	cfg.TargetLevel = 1

	out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
	if err != nil {
		t.Fatalf("failed: %v", err)
	}
	if out.Stats.OmittedTombstones != 0 {
		t.Fatalf("expected 0 omitted tombstones, got %d", out.Stats.OmittedTombstones)
	}
	if out.Stats.RetainedTombstones != 1 {
		t.Fatalf("expected 1 retained tombstone, got %d", out.Stats.RetainedTombstones)
	}
	if out.Files[0].EntryCount != 2 {
		t.Fatalf("expected 2 records, got %d", out.Files[0].EntryCount)
	}
	if out.Files[0].TombstoneCount != 1 {
		t.Fatalf("expected 1 tombstone count in descriptor, got %d", out.Files[0].TombstoneCount)
	}
}

// TestCompactionOutput_F_RangeFalsePositive verifies integration with Compactor under range false positive.
func TestCompactionOutput_F_RangeFalsePositive(t *testing.T) {
	dbDir := t.TempDir()
	alloc := simpleSeqAllocator(1)

	// Construct an SSTable file in L2 spanning [a, z], containing only "a" and "z" (not "m").
	sstPath := filepath.Join(dbDir, "000099.sst")
	w, err := sstable.NewTableWriter(sstPath, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("failed to create writer: %v", err)
	}
	if err := w.Add(binary.InternalKey{UserKey: []byte("a"), SeqNum: 10, OpType: binary.OpTypePut}, []byte("va")); err != nil {
		t.Fatalf("failed to add: %v", err)
	}
	if err := w.Add(binary.InternalKey{UserKey: []byte("z"), SeqNum: 10, OpType: binary.OpTypePut}, []byte("vz")); err != nil {
		t.Fatalf("failed to add: %v", err)
	}
	sstMeta, err := w.Finish()
	if err != nil {
		t.Fatalf("failed to finish: %v", err)
	}

	deeperFile := version.NewFileMetadataFromSSTable(99, sstMeta)
	var levels [version.NumLevels][]version.FileMetadata
	levels[2] = []version.FileMetadata{deeperFile}

	v := version.NewVersion(levels)

	// Case 1: Conservative Compactor without TableOpener -> retains tombstone for "m"
	compactorConservative, err := NewCompactor(v)
	if err != nil {
		t.Fatalf("failed to create compactor: %v", err)
	}
	defer func() { _ = compactorConservative.Close() }()

	records := []mockRecord{
		{key: binary.InternalKey{UserKey: []byte("m"), SeqNum: 100, OpType: binary.OpTypeDelete}, val: nil},
	}
	iter1 := newMockSliceIterator(records)
	cfg1 := DefaultCompactionOutputConfig()
	cfg1.DbDir = dbDir
	cfg1.TargetLevel = 1

	out1, err := BuildCompactionOutput(iter1, compactorConservative, alloc, cfg1)
	if err != nil {
		t.Fatalf("failed: %v", err)
	}
	if out1.Stats.RetainedTombstones != 1 || len(out1.Files) != 1 {
		t.Fatalf("conservative mode should retain tombstone: files=%d, retained=%d", len(out1.Files), out1.Stats.RetainedTombstones)
	}

	// Case 2: Exact Compactor with TableOpener -> discovers "m" is absent in L2 -> safely drops tombstone
	opener := func(fileNum uint64) (*sstable.TableReader, error) {
		return sstable.NewTableReader(filepath.Join(dbDir, fmt.Sprintf("%06d.sst", fileNum)))
	}
	compactorExact, err := NewCompactor(v, WithTableOpener(opener), WithCloseTableReaders(true))
	if err != nil {
		t.Fatalf("failed to create compactor: %v", err)
	}
	defer func() { _ = compactorExact.Close() }()

	iter2 := newMockSliceIterator(records)
	cfg2 := DefaultCompactionOutputConfig()
	cfg2.DbDir = dbDir
	cfg2.TargetLevel = 1

	out2, err := BuildCompactionOutput(iter2, compactorExact, alloc, cfg2)
	if err != nil {
		t.Fatalf("failed: %v", err)
	}
	if out2.Stats.OmittedTombstones != 1 || len(out2.Files) != 0 {
		t.Fatalf("exact mode should safely omit tombstone: files=%d, omitted=%d", len(out2.Files), out2.Stats.OmittedTombstones)
	}
}

// TestCompactionOutput_G_AllTombstonesSafelyDroppable verifies zero output SSTables when all tombstones are dropped.
func TestCompactionOutput_G_AllTombstonesSafelyDroppable(t *testing.T) {
	dbDir := t.TempDir()
	alloc := simpleSeqAllocator(1)
	records := []mockRecord{
		{key: binary.InternalKey{UserKey: []byte("k1"), SeqNum: 10, OpType: binary.OpTypeDelete}, val: nil},
		{key: binary.InternalKey{UserKey: []byte("k2"), SeqNum: 20, OpType: binary.OpTypeDelete}, val: nil},
		{key: binary.InternalKey{UserKey: []byte("k3"), SeqNum: 30, OpType: binary.OpTypeDelete}, val: nil},
	}
	iter := newMockSliceIterator(records)
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool { return true })

	cfg := DefaultCompactionOutputConfig()
	cfg.DbDir = dbDir
	cfg.TargetLevel = 1

	out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
	if err != nil {
		t.Fatalf("failed: %v", err)
	}
	if len(out.Files) != 0 {
		t.Fatalf("expected 0 files, got %d", len(out.Files))
	}
	if out.Stats.OmittedTombstones != 3 {
		t.Fatalf("expected 3 omitted tombstones, got %d", out.Stats.OmittedTombstones)
	}
	if out.Stats.WrittenRecords != 0 {
		t.Fatalf("expected 0 written records, got %d", out.Stats.WrittenRecords)
	}
}

// TestCompactionOutput_H_TombstoneMixedWithPuts verifies selective omission when tombstones and PUTs are mixed.
func TestCompactionOutput_H_TombstoneMixedWithPuts(t *testing.T) {
	dbDir := t.TempDir()
	alloc := simpleSeqAllocator(1)
	records := []mockRecord{
		{key: binary.InternalKey{UserKey: []byte("a"), SeqNum: 50, OpType: binary.OpTypePut}, val: []byte("va")},
		{key: binary.InternalKey{UserKey: []byte("b"), SeqNum: 60, OpType: binary.OpTypeDelete}, val: nil}, // safe (drop)
		{key: binary.InternalKey{UserKey: []byte("c"), SeqNum: 70, OpType: binary.OpTypePut}, val: []byte("vc")},
		{key: binary.InternalKey{UserKey: []byte("d"), SeqNum: 80, OpType: binary.OpTypeDelete}, val: nil}, // unsafe (keep)
		{key: binary.InternalKey{UserKey: []byte("e"), SeqNum: 90, OpType: binary.OpTypePut}, val: []byte("ve")},
	}
	iter := newMockSliceIterator(records)
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool {
		return bytes.Equal(userKey, []byte("b")) // only 'b' is safe to drop
	})

	cfg := DefaultCompactionOutputConfig()
	cfg.DbDir = dbDir
	cfg.TargetLevel = 1

	out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
	if err != nil {
		t.Fatalf("failed: %v", err)
	}
	if out.Stats.OmittedTombstones != 1 || out.Stats.RetainedTombstones != 1 || out.Stats.WrittenRecords != 4 {
		t.Fatalf("unexpected stats: %+v", out.Stats)
	}
	if len(out.Files) != 1 || out.Files[0].EntryCount != 4 {
		t.Fatalf("expected 1 file with 4 entries, got %d files", len(out.Files))
	}
}

// TestCompactionOutput_I_MultiplePartitions verifies that partition boundary splits output across multiple SSTables.
func TestCompactionOutput_I_MultiplePartitions(t *testing.T) {
	dbDir := t.TempDir()
	alloc := simpleSeqAllocator(10)

	// Create 100 records of ~100 bytes each
	var records []mockRecord
	for i := 0; i < 100; i++ {
		records = append(records, mockRecord{
			key: binary.InternalKey{
				UserKey: []byte(fmt.Sprintf("user_key_%05d", i)),
				SeqNum:  binary.SeqNum(1000 - i),
				OpType:  binary.OpTypePut,
			},
			val: bytes.Repeat([]byte("v"), 80),
		})
	}
	iter := newMockSliceIterator(records)
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool { return true })

	// Set small TargetPartitionSize (e.g. 2048 bytes) to trigger multiple partitions
	cfg := DefaultCompactionOutputConfig()
	cfg.DbDir = dbDir
	cfg.TargetLevel = 1
	cfg.TargetPartitionSize = 2048

	out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
	if err != nil {
		t.Fatalf("failed: %v", err)
	}

	if len(out.Files) < 2 {
		t.Fatalf("expected >= 2 partitions, got %d", len(out.Files))
	}

	// Verify all records are present, distinct, and in canonical order across files
	var readRecords []mockRecord
	for _, f := range out.Files {
		reader, err := sstable.NewTableReader(f.Path)
		if err != nil {
			t.Fatalf("failed to read table %s: %v", f.Path, err)
		}
		rit, err := reader.NewIterator()
		if err != nil {
			_ = reader.Close()
			t.Fatalf("failed to create iterator: %v", err)
		}
		for rit.Next() {
			readRecords = append(readRecords, mockRecord{
				key: rit.Key().Clone(),
				val: rit.Value(),
			})
		}
		_ = rit.Close()
		_ = reader.Close()
	}

	if len(readRecords) != len(records) {
		t.Fatalf("record count mismatch: expected %d, got %d", len(records), len(readRecords))
	}
	for i := range records {
		if !bytes.Equal(readRecords[i].key.UserKey, records[i].key.UserKey) {
			t.Fatalf("record %d user key mismatch: got %s, expected %s", i, readRecords[i].key.UserKey, records[i].key.UserKey)
		}
	}
}

// TestCompactionOutput_J_BoundarySizedRecord verifies handling of a record near partition boundary.
func TestCompactionOutput_J_BoundarySizedRecord(t *testing.T) {
	dbDir := t.TempDir()
	alloc := simpleSeqAllocator(1)

	// Partition target 4096 bytes
	targetSize := uint64(4096)
	// Record of ~2000 bytes
	records := []mockRecord{
		{key: binary.InternalKey{UserKey: []byte("rec_1"), SeqNum: 10, OpType: binary.OpTypePut}, val: bytes.Repeat([]byte("x"), 2000)},
		{key: binary.InternalKey{UserKey: []byte("rec_2"), SeqNum: 10, OpType: binary.OpTypePut}, val: bytes.Repeat([]byte("y"), 2000)},
	}
	iter := newMockSliceIterator(records)
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool { return true })

	cfg := DefaultCompactionOutputConfig()
	cfg.DbDir = dbDir
	cfg.TargetLevel = 1
	cfg.TargetPartitionSize = targetSize

	out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
	if err != nil {
		t.Fatalf("failed: %v", err)
	}

	// Two 2KB records with block/index/footer overhead exceed 4KB -> partitioned into 2 files
	if len(out.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(out.Files))
	}
}

// TestCompactionOutput_K_OversizedSingleRecord verifies forward progress when a single record exceeds target size.
func TestCompactionOutput_K_OversizedSingleRecord(t *testing.T) {
	dbDir := t.TempDir()
	alloc := simpleSeqAllocator(1)

	// Partition target 1024 bytes; record is 5000 bytes (exceeds target by ~5x)
	records := []mockRecord{
		{key: binary.InternalKey{UserKey: []byte("huge_key"), SeqNum: 100, OpType: binary.OpTypePut}, val: bytes.Repeat([]byte("z"), 5000)},
		{key: binary.InternalKey{UserKey: []byte("next_key"), SeqNum: 100, OpType: binary.OpTypePut}, val: []byte("small_val")},
	}
	iter := newMockSliceIterator(records)
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool { return true })

	cfg := DefaultCompactionOutputConfig()
	cfg.DbDir = dbDir
	cfg.TargetLevel = 1
	cfg.TargetPartitionSize = 1024 // very small target

	out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
	if err != nil {
		t.Fatalf("failed: %v", err)
	}

	// Must produce forward progress: huge_key in file 1, next_key in file 2
	if len(out.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(out.Files))
	}
	if out.Files[0].EntryCount != 1 || !bytes.Equal(out.Files[0].SmallestUserKey, []byte("huge_key")) {
		t.Fatalf("first file should contain huge_key")
	}
	if out.Files[1].EntryCount != 1 || !bytes.Equal(out.Files[1].SmallestUserKey, []byte("next_key")) {
		t.Fatalf("second file should contain next_key")
	}
}

// TestCompactionOutput_L_PartitionExactness verifies explicit partition boundary policy.
func TestCompactionOutput_L_PartitionExactness(t *testing.T) {
	dbDir := t.TempDir()
	alloc := simpleSeqAllocator(1)

	var records []mockRecord
	for i := 0; i < 30; i++ {
		records = append(records, mockRecord{
			key: binary.InternalKey{
				UserKey: []byte(fmt.Sprintf("key_%03d", i)),
				SeqNum:  binary.SeqNum(100 - i),
				OpType:  binary.OpTypePut,
			},
			val: bytes.Repeat([]byte("v"), 200),
		})
	}
	iter := newMockSliceIterator(records)
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool { return true })

	cfg := DefaultCompactionOutputConfig()
	cfg.DbDir = dbDir
	cfg.TargetLevel = 1
	cfg.TargetPartitionSize = 1500

	out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
	if err != nil {
		t.Fatalf("failed: %v", err)
	}

	// Verify all files have EntryCount > 0 and file ranges do not overlap
	for i := 0; i < len(out.Files)-1; i++ {
		cur := out.Files[i]
		next := out.Files[i+1]
		if bytes.Compare(cur.LargestUserKey, next.SmallestUserKey) >= 0 {
			t.Fatalf("partition overlap between file %d and %d: cur largest %s >= next smallest %s",
				cur.FileNum, next.FileNum, cur.LargestUserKey, next.SmallestUserKey)
		}
	}
}

// TestCompactionOutput_M_WriterFailure verifies fail-closed behavior on writer creation failure.
func TestCompactionOutput_M_WriterFailure(t *testing.T) {
	// Provide a non-existent invalid path with permission issues
	dbDir := "/dev/null/invalid_path"
	alloc := simpleSeqAllocator(1)
	records := []mockRecord{
		{key: binary.InternalKey{UserKey: []byte("k"), SeqNum: 1, OpType: binary.OpTypePut}, val: []byte("v")},
	}
	iter := newMockSliceIterator(records)
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool { return true })

	cfg := DefaultCompactionOutputConfig()
	cfg.DbDir = dbDir
	cfg.TargetLevel = 1

	out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
	if err == nil {
		t.Fatalf("expected error on invalid dbDir, got nil")
	}
	if out != nil {
		t.Fatalf("expected nil output on error")
	}
}

// TestCompactionOutput_N_FinalizationFailure verifies cleanup and error propagation on invalid mode.
func TestCompactionOutput_N_FinalizationFailure(t *testing.T) {
	dbDir := t.TempDir()
	alloc := simpleSeqAllocator(1)
	records := []mockRecord{
		{key: binary.InternalKey{UserKey: []byte("k"), SeqNum: 1, OpType: binary.OpTypePut}, val: []byte("v")},
	}
	iter := newMockSliceIterator(records)
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool { return true })

	cfg := DefaultCompactionOutputConfig()
	cfg.DbDir = dbDir
	cfg.TargetLevel = 1
	// Insecure file mode triggers error on NewTableWriter
	cfg.TableWriterOptions.FileMode = 0777

	out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
	if err == nil {
		t.Fatalf("expected error on insecure file mode")
	}
	if out != nil {
		t.Fatalf("expected nil output")
	}
}

// TestCompactionOutput_O_IteratorCorruption verifies that iterator errors propagate and abort output.
func TestCompactionOutput_O_IteratorCorruption(t *testing.T) {
	dbDir := t.TempDir()
	alloc := simpleSeqAllocator(1)
	records := []mockRecord{
		{key: binary.InternalKey{UserKey: []byte("k1"), SeqNum: 1, OpType: binary.OpTypePut}, val: []byte("v1")},
		{key: binary.InternalKey{UserKey: []byte("k2"), SeqNum: 2, OpType: binary.OpTypePut}, val: []byte("v2")},
	}
	iter := newMockSliceIterator(records)
	// Inject corruption error on iterator
	injectedErr := fmt.Errorf("simulated child block corruption")
	iter.err = injectedErr

	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool { return true })

	cfg := DefaultCompactionOutputConfig()
	cfg.DbDir = dbDir
	cfg.TargetLevel = 1

	out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
	if err == nil {
		t.Fatalf("expected error from corrupted iterator, got nil")
	}
	if out != nil {
		t.Fatalf("expected nil output on error")
	}

	// Assert no persistent SSTables were committed
	entries, err := os.ReadDir(dbDir)
	if err != nil {
		t.Fatalf("failed to read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 files left behind on failure, found %d", len(entries))
	}
}

// TestCompactionOutput_P_FileCollisionSafety verifies that allocator returning existing file fails closed.
func TestCompactionOutput_P_FileCollisionSafety(t *testing.T) {
	dbDir := t.TempDir()

	// Pre-create 000001.sst
	collidingPath := filepath.Join(dbDir, "000001.sst")
	if err := os.WriteFile(collidingPath, []byte("pre-existing dummy data"), 0600); err != nil {
		t.Fatalf("failed to write dummy file: %v", err)
	}

	// Allocator tries to return 1 (which collides!)
	alloc := func() (uint64, error) {
		return 1, nil
	}

	records := []mockRecord{
		{key: binary.InternalKey{UserKey: []byte("k"), SeqNum: 1, OpType: binary.OpTypePut}, val: []byte("v")},
	}
	iter := newMockSliceIterator(records)
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool { return true })

	cfg := DefaultCompactionOutputConfig()
	cfg.DbDir = dbDir
	cfg.TargetLevel = 1

	out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
	if err == nil {
		t.Fatalf("expected ErrSSTableExists collision error, got nil")
	}
	if !stdErrors.Is(err, errors.ErrSSTableExists) {
		t.Fatalf("expected ErrSSTableExists, got: %v", err)
	}
	if out != nil {
		t.Fatalf("expected nil output")
	}

	// Verify existing file was not overwritten
	cleanCollidingPath := filepath.Clean(collidingPath)
	content, err := os.ReadFile(cleanCollidingPath)
	if err != nil {
		t.Fatalf("failed to read colliding path: %v", err)
	}
	if string(content) != "pre-existing dummy data" {
		t.Fatalf("SECURITY VIOLATION: colliding SSTable was overwritten!")
	}
}

// TestCompactionOutput_Q_MetadataCorrectness verifies returned metadata equals physical file properties.
func TestCompactionOutput_Q_MetadataCorrectness(t *testing.T) {
	dbDir := t.TempDir()
	alloc := simpleSeqAllocator(42)
	records := []mockRecord{
		{key: binary.InternalKey{UserKey: []byte("alpha"), SeqNum: 500, OpType: binary.OpTypePut}, val: []byte("v_alpha")},
		{key: binary.InternalKey{UserKey: []byte("beta"), SeqNum: 400, OpType: binary.OpTypePut}, val: []byte("v_beta")},
		{key: binary.InternalKey{UserKey: []byte("gamma"), SeqNum: 300, OpType: binary.OpTypePut}, val: []byte("v_gamma")},
	}
	iter := newMockSliceIterator(records)
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool { return true })

	cfg := DefaultCompactionOutputConfig()
	cfg.DbDir = dbDir
	cfg.TargetLevel = 2

	out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
	if err != nil {
		t.Fatalf("failed: %v", err)
	}

	if len(out.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(out.Files))
	}
	f := out.Files[0]
	if f.FileNum != 42 {
		t.Fatalf("expected FileNum 42, got %d", f.FileNum)
	}
	if f.Level != 2 {
		t.Fatalf("expected Level 2, got %d", f.Level)
	}
	if f.EntryCount != 3 {
		t.Fatalf("expected EntryCount 3, got %d", f.EntryCount)
	}

	// Verify physical file matches metadata
	fi, err := os.Lstat(f.Path)
	if err != nil {
		t.Fatalf("failed to stat file: %v", err)
	}
	// #nosec G115 -- guarded by fi.Size() >= 0
	if fi.Size() < 0 || uint64(fi.Size()) != f.Meta.FileSize {
		t.Fatalf("file size mismatch: stat=%d, meta=%d", fi.Size(), f.Meta.FileSize)
	}
	if f.Meta.SmallestSeqNum != 300 || f.Meta.LargestSeqNum != 500 {
		t.Fatalf("seqnum mismatch: smallest=%d, largest=%d", f.Meta.SmallestSeqNum, f.Meta.LargestSeqNum)
	}
}

// TestCompactionOutput_R_ReaderRoundTrip opens every generated SSTable using TableReader and scans all keys.
func TestCompactionOutput_R_ReaderRoundTrip(t *testing.T) {
	dbDir := t.TempDir()
	alloc := simpleSeqAllocator(1)

	var records []mockRecord
	for i := 0; i < 50; i++ {
		records = append(records, mockRecord{
			key: binary.InternalKey{
				UserKey: []byte(fmt.Sprintf("key_%04d", i)),
				SeqNum:  binary.SeqNum(i + 1),
				OpType:  binary.OpTypePut,
			},
			val: []byte(fmt.Sprintf("value_%04d", i)),
		})
	}
	iter := newMockSliceIterator(records)
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool { return true })

	cfg := DefaultCompactionOutputConfig()
	cfg.DbDir = dbDir
	cfg.TargetLevel = 1
	cfg.TargetPartitionSize = 1024 // forces multiple partitions

	out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
	if err != nil {
		t.Fatalf("failed: %v", err)
	}

	// Round-trip validation on each generated file
	var totalRead uint64
	for _, f := range out.Files {
		if err := ValidateSSTableOutput(f.Path, f.Meta); err != nil {
			t.Fatalf("validation failed for %s: %v", f.Path, err)
		}
		totalRead += f.EntryCount
	}
	if totalRead != uint64(len(records)) {
		t.Fatalf("total read entries %d != expected %d", totalRead, len(records))
	}
}

// TestCompactionOutput_S_Determinism verifies identical inputs and allocations produce identical partitions and metadata.
func TestCompactionOutput_S_Determinism(t *testing.T) {
	records := []mockRecord{
		{key: binary.InternalKey{UserKey: []byte("apple"), SeqNum: 10, OpType: binary.OpTypePut}, val: []byte("red")},
		{key: binary.InternalKey{UserKey: []byte("banana"), SeqNum: 20, OpType: binary.OpTypePut}, val: []byte("yellow")},
		{key: binary.InternalKey{UserKey: []byte("cherry"), SeqNum: 30, OpType: binary.OpTypePut}, val: []byte("red")},
		{key: binary.InternalKey{UserKey: []byte("date"), SeqNum: 40, OpType: binary.OpTypePut}, val: []byte("brown")},
	}
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool { return true })

	run := func() (*CompactionOutput, error) {
		dbDir := t.TempDir()
		alloc := simpleSeqAllocator(1)
		iter := newMockSliceIterator(records)
		cfg := DefaultCompactionOutputConfig()
		cfg.DbDir = dbDir
		cfg.TargetLevel = 1
		cfg.TargetPartitionSize = 1024
		return BuildCompactionOutput(iter, safety, alloc, cfg)
	}

	out1, err1 := run()
	if err1 != nil {
		t.Fatalf("run 1 failed: %v", err1)
	}
	out2, err2 := run()
	if err2 != nil {
		t.Fatalf("run 2 failed: %v", err2)
	}

	if len(out1.Files) != len(out2.Files) {
		t.Fatalf("partition count mismatch: %d vs %d", len(out1.Files), len(out2.Files))
	}
	for i := range out1.Files {
		f1 := out1.Files[i]
		f2 := out2.Files[i]
		if f1.FileNum != f2.FileNum || f1.EntryCount != f2.EntryCount || f1.Meta.FileSize != f2.Meta.FileSize {
			t.Fatalf("file %d mismatch: %+v vs %+v", i, f1, f2)
		}
		if !bytes.Equal(f1.SmallestKey, f2.SmallestKey) || !bytes.Equal(f1.LargestKey, f2.LargestKey) {
			t.Fatalf("key range mismatch on file %d", i)
		}
	}
}

// TestCompactionOutput_T_ResourceLifecycle verifies all writers and iterators are closed without leaks.
func TestCompactionOutput_T_ResourceLifecycle(t *testing.T) {
	dbDir := t.TempDir()
	alloc := simpleSeqAllocator(1)
	records := []mockRecord{
		{key: binary.InternalKey{UserKey: []byte("k"), SeqNum: 1, OpType: binary.OpTypePut}, val: []byte("v")},
	}
	iter := newMockSliceIterator(records)
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool { return true })

	cfg := DefaultCompactionOutputConfig()
	cfg.DbDir = dbDir
	cfg.TargetLevel = 1
	cfg.CloseIterator = true

	out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
	if err != nil {
		t.Fatalf("failed: %v", err)
	}
	if len(out.Files) != 1 {
		t.Fatalf("expected 1 file")
	}

	// Verify iterator was closed via WithCloseIterator
	if !iter.closed {
		t.Fatalf("expected iterator to be closed by generator")
	}

	// Verify no temporary staging files remain in dbDir
	entries, err := os.ReadDir(dbDir)
	if err != nil {
		t.Fatalf("failed to read dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".sst" {
			t.Fatalf("unexpected non-sst file left open or stranded: %s", e.Name())
		}
	}
}

// TestCompactor_BuildOutput_Integration verifies the Compactor.BuildOutput wrapper using CompactionPlan.
func TestCompactor_BuildOutput_Integration(t *testing.T) {
	dbDir := t.TempDir()
	alloc := simpleSeqAllocator(100)

	var levels [version.NumLevels][]version.FileMetadata
	v := version.NewVersion(levels)
	compactor, err := NewCompactor(v)
	if err != nil {
		t.Fatalf("failed to create compactor: %v", err)
	}
	defer func() { _ = compactor.Close() }()

	srcFile := version.FileMetadata{
		FileNum:        1,
		FileSize:       1000,
		SmallestKey:    binary.EncodeInternalKey(binary.InternalKey{UserKey: []byte("a"), SeqNum: 10, OpType: binary.OpTypePut}),
		LargestKey:     binary.EncodeInternalKey(binary.InternalKey{UserKey: []byte("z"), SeqNum: 10, OpType: binary.OpTypePut}),
		SmallestSeqNum: 10,
		LargestSeqNum:  10,
	}
	plan, err := NewCompactionPlan(
		0, 1,
		[]version.FileMetadata{srcFile},
		nil,
		binary.InternalKey{UserKey: []byte("a"), SeqNum: 10, OpType: binary.OpTypePut},
		binary.InternalKey{UserKey: []byte("z"), SeqNum: 10, OpType: binary.OpTypePut},
		[]byte("a"),
		[]byte("z"),
		1000, 0, 1.5,
	)
	if err != nil {
		t.Fatalf("failed to create plan: %v", err)
	}

	records := []mockRecord{
		{key: binary.InternalKey{UserKey: []byte("k1"), SeqNum: 10, OpType: binary.OpTypePut}, val: []byte("v1")},
		{key: binary.InternalKey{UserKey: []byte("k2"), SeqNum: 10, OpType: binary.OpTypeDelete}, val: nil}, // safe in empty deeper levels
	}
	iter := newMockSliceIterator(records)

	out, err := compactor.BuildOutput(iter, plan, dbDir, alloc)
	if err != nil {
		t.Fatalf("compactor.BuildOutput failed: %v", err)
	}

	if len(out.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(out.Files))
	}
	if out.Stats.OmittedTombstones != 1 {
		t.Fatalf("expected 1 tombstone omitted via Compactor.CanDropTombstone")
	}
}

// TestCompactionOutput_ConcurrentBuilds verifies thread-safety of independent output generation runs.
func TestCompactionOutput_ConcurrentBuilds(t *testing.T) {
	var wg sync.WaitGroup
	workers := 4

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			dbDir := t.TempDir()
			// #nosec G115 -- workerID >= 0
			alloc := simpleSeqAllocator(uint64(workerID)*1000 + 1)
			records := []mockRecord{
				{key: binary.InternalKey{UserKey: []byte(fmt.Sprintf("worker_%d_k1", workerID)), SeqNum: 1, OpType: binary.OpTypePut}, val: []byte("v1")},
				{key: binary.InternalKey{UserKey: []byte(fmt.Sprintf("worker_%d_k2", workerID)), SeqNum: 2, OpType: binary.OpTypePut}, val: []byte("v2")},
			}
			iter := newMockSliceIterator(records)
			safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool { return true })

			cfg := DefaultCompactionOutputConfig()
			cfg.DbDir = dbDir
			cfg.TargetLevel = 1

			out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
			if err != nil {
				t.Errorf("worker %d failed: %v", workerID, err)
				return
			}
			if len(out.Files) != 1 {
				t.Errorf("worker %d expected 1 file, got %d", workerID, len(out.Files))
			}
		}(i)
	}
	wg.Wait()
}
