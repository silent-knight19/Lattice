package engine_test

import (
	"context"
	stdErrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/version"
	"github.com/silent-knight19/lattice/internal/wal"
)

// defaultEngineCfg returns standard BackpressureConfig for testing.
func defaultEngineCfg() engine.BackpressureConfig {
	return engine.BackpressureConfig{
		MaxMemoryBytes: 64 * 1024 * 1024, // 64 MiB
		HighWatermark:  0.80,
		HardWatermark:  0.90,
		MaxWaitTimeout: 100 * time.Millisecond,
	}
}

// setupTestManifestWithCheckpoint writes a valid MANIFEST with LastSeqNum = lastSeq and sets CURRENT.
func setupTestManifestWithCheckpoint(t *testing.T, dir string, lastSeq uint64) *version.ReplayResult {
	t.Helper()
	edit := version.NewVersionEdit()
	edit.SetNextFileNum(100)
	edit.SetLastSeqNum(binary.SeqNum(lastSeq))

	// Add dummy L0 SSTable so the manifest has a valid level state
	sk, _ := binary.NewInternalKey([]byte("init_a"), 1, binary.OpTypePut)
	lk, _ := binary.NewInternalKey([]byte("init_z"), binary.SeqNum(lastSeq), binary.OpTypePut)
	_ = edit.AddFile(0, version.FileMetadata{
		FileNum:        1,
		FileSize:       1024,
		SmallestKey:    binary.EncodeInternalKey(sk),
		LargestKey:     binary.EncodeInternalKey(lk),
		SmallestSeqNum: 1,
		LargestSeqNum:  lastSeq,
	})

	// Create dummy physical SSTable file
	sstPath := version.TablePath(dir, 1)
	if err := os.WriteFile(sstPath, []byte("sstable-dummy-content"), 0600); err != nil {
		t.Fatalf("failed to write dummy sstable: %v", err)
	}

	manifestPath := version.ManifestPath(dir, 1)
	w, err := version.CreateManifestWriter(manifestPath)
	if err != nil {
		t.Fatalf("CreateManifestWriter failed: %v", err)
	}
	if err := w.LogEditPtr(edit); err != nil {
		_ = w.Close()
		t.Fatalf("LogEditPtr failed: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if err := version.SetCurrentManifest(dir, 1); err != nil {
		t.Fatalf("SetCurrentManifest failed: %v", err)
	}

	disc, err := version.DiscoverActiveManifest(dir)
	if err != nil {
		t.Fatalf("DiscoverActiveManifest failed: %v", err)
	}
	defer func() { _ = disc.Close() }()

	res, err := version.ReplayManifest(disc)
	if err != nil {
		t.Fatalf("ReplayManifest failed: %v", err)
	}
	return res
}

// writeWALSegment initializes the WAL directory if needed, opens a segment, writes records, and closes it.
func writeWALSegment(t *testing.T, dbPath string, segID uint64, records ...wal.Record) {
	t.Helper()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("failed to init wal dir: %v", err)
	}
	w, err := wal.CreateSegmentWriter(dbPath, segID)
	if err != nil {
		t.Fatalf("failed to create segment %d: %v", segID, err)
	}
	for _, rec := range records {
		if err := w.AppendSync(rec); err != nil {
			_ = w.Close()
			t.Fatalf("failed to append record to segment %d: %v", segID, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("failed to close segment %d: %v", segID, err)
	}
}

// corruptByteAt flips bits at a specific offset in a file on disk.
func corruptByteAt(t *testing.T, path string, offset int64, bitMask byte) {
	t.Helper()
	cleanPath := filepath.Clean(path)
	data, err := os.ReadFile(cleanPath) // #nosec G304 - test-only fault injection
	if err != nil {
		t.Fatalf("failed to read file %s for corruption: %v", path, err)
	}
	if offset < 0 || offset >= int64(len(data)) {
		t.Fatalf("corrupt offset %d out of range (file size %d)", offset, len(data))
	}
	data[offset] ^= bitMask
	if err := os.WriteFile(cleanPath, data, 0600); err != nil { // #nosec G703 - test-only fault injection
		t.Fatalf("failed to write corrupted file %s: %v", path, err)
	}
}

// makePutRecord helper
func makePutRecord(seq uint64, key, val string) wal.Record {
	return wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(seq),
		Timestamp: 1700000000000000 + seq,
		Key:       []byte(key),
		Value:     []byte(val),
	}
}

// makeDeleteRecord helper
func makeDeleteRecord(seq uint64, key string) wal.Record {
	return wal.Record{
		Type:      wal.RecordTypeDelete,
		SeqNum:    binary.SeqNum(seq),
		Timestamp: 1700000000000000 + seq,
		Key:       []byte(key),
		Value:     nil,
	}
}

// makeBatchMarker helper
func makeBatchMarker(seq uint64, rType wal.RecordType) wal.Record {
	return wal.Record{
		Type:      rType,
		SeqNum:    binary.SeqNum(seq),
		Timestamp: 1700000000000000 + seq,
	}
}

// -----------------------------------------------------------------------------
// A. EMPTY WAL DIRECTORY
// -----------------------------------------------------------------------------

func TestEngineRecoverWAL_A_EmptyWAL(t *testing.T) {
	dir := t.TempDir()
	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	// Clean directory with neither MANIFEST nor WAL
	if err := eng.RecoverWAL(); err != nil {
		t.Fatalf("expected successful recovery on clean empty directory, got: %v", err)
	}

	if eng.ActiveMemTable().Len() != 0 {
		t.Fatalf("expected 0 entries in active MemTable, got %d", eng.ActiveMemTable().Len())
	}
	if eng.NextSeqNum() != 0 {
		t.Fatalf("expected nextSeqNum 0, got %d", eng.NextSeqNum())
	}
}

// -----------------------------------------------------------------------------
// B. 500-RECORD ACCEPTANCE TEST
// -----------------------------------------------------------------------------

func TestEngineRecoverWAL_B_500Records(t *testing.T) {
	dir := t.TempDir()

	const count = 500
	records := make([]wal.Record, count)
	for i := 0; i < count; i++ {
		seq := uint64(i + 1)
		k := fmt.Sprintf("user_%04d", seq)
		v := fmt.Sprintf("value_%04d", seq)
		records[i] = makePutRecord(seq, k, v)
	}

	// Write all 500 records to WAL segment 1
	writeWALSegment(t, dir, 1, records...)

	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	// Execute RecoverWAL
	if err := eng.RecoverWAL(); err != nil {
		t.Fatalf("RecoverWAL failed on 500 records: %v", err)
	}

	if eng.ActiveMemTable().Len() != count {
		t.Fatalf("expected %d entries in active MemTable, got %d", count, eng.ActiveMemTable().Len())
	}
	if eng.NextSeqNum() != count {
		t.Fatalf("expected NextSeqNum=%d, got %d", count, eng.NextSeqNum())
	}

	// Verify all 500 keys and values are retrievable via Get
	for i := 0; i < count; i++ {
		seq := uint64(i + 1)
		k := []byte(fmt.Sprintf("user_%04d", seq))
		expectedVal := []byte(fmt.Sprintf("value_%04d", seq))

		val, err := eng.Get(k)
		if err != nil {
			t.Fatalf("Get(%s) failed: %v", string(k), err)
		}
		if string(val) != string(expectedVal) {
			t.Fatalf("Get(%s) mismatch: expected %s, got %s", string(k), string(expectedVal), string(val))
		}
	}
}

// -----------------------------------------------------------------------------
// C. DUPLICATE-PREVENTION ACCEPTANCE TEST (INV-03, INV-04)
// -----------------------------------------------------------------------------

func TestEngineRecoverWAL_C_DuplicatePrevention(t *testing.T) {
	dir := t.TempDir()

	// 1. Establish durable MANIFEST checkpoint at LastSeqNum = 500
	replayRes := setupTestManifestWithCheckpoint(t, dir, 500)
	replayRes.Version.Unref()

	// 2. WAL contains records 1..550:
	//    - 1..500 are already committed in SSTable/MANIFEST.
	//    - 501..550 are uncommitted pending writes.
	records := make([]wal.Record, 550)
	for i := 0; i < 550; i++ {
		seq := uint64(i + 1)
		records[i] = makePutRecord(seq, fmt.Sprintf("k_%04d", seq), fmt.Sprintf("v_%04d", seq))
	}
	writeWALSegment(t, dir, 1, records...)

	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	if err := eng.RecoverWAL(); err != nil {
		t.Fatalf("RecoverWAL failed: %v", err)
	}

	// 3. Verify that ONLY records 501..550 (50 entries) are in the active MemTable
	if eng.ActiveMemTable().Len() != 50 {
		t.Fatalf("expected exactly 50 entries in active MemTable, got %d", eng.ActiveMemTable().Len())
	}
	if eng.NextSeqNum() != 550 {
		t.Fatalf("expected NextSeqNum=550, got %d", eng.NextSeqNum())
	}

	// Assert pre-checkpoint records (1..500) are NOT in the active MemTable
	for seq := 1; seq <= 500; seq++ {
		k := []byte(fmt.Sprintf("k_%04d", seq))
		_, err := eng.ActiveMemTable().SearchConcurrent(k)
		if !stdErrors.Is(err, errors.ErrKeyNotFound) {
			t.Fatalf("record %s was unexpectedly inserted into active MemTable: err=%v", string(k), err)
		}
	}

	// Assert uncommitted records (501..550) ARE in the active MemTable
	for seq := 501; seq <= 550; seq++ {
		k := []byte(fmt.Sprintf("k_%04d", seq))
		val, err := eng.Get(k)
		if err != nil {
			t.Fatalf("uncommitted record %s not found in engine: %v", string(k), err)
		}
		expected := fmt.Sprintf("v_%04d", seq)
		if string(val) != expected {
			t.Fatalf("value mismatch for %s: expected %s, got %s", string(k), expected, string(val))
		}
	}
}

// -----------------------------------------------------------------------------
// D. MULTI-SEGMENT SUFFIX REPLAY (INV-01)
// -----------------------------------------------------------------------------

func TestEngineRecoverWAL_D_MultiSegmentSuffix(t *testing.T) {
	dir := t.TempDir()

	// Manifest checkpoint at 180
	res := setupTestManifestWithCheckpoint(t, dir, 180)
	res.Version.Unref()

	// Segment 1: seq 1..100
	seg1 := make([]wal.Record, 100)
	for i := 0; i < 100; i++ {
		seq := uint64(i + 1)
		seg1[i] = makePutRecord(seq, fmt.Sprintf("m_%03d", seq), fmt.Sprintf("v_%03d", seq))
	}
	writeWALSegment(t, dir, 1, seg1...)

	// Segment 2: seq 101..200
	seg2 := make([]wal.Record, 100)
	for i := 0; i < 100; i++ {
		seq := uint64(101 + i)
		seg2[i] = makePutRecord(seq, fmt.Sprintf("m_%03d", seq), fmt.Sprintf("v_%03d", seq))
	}
	writeWALSegment(t, dir, 2, seg2...)

	// Segment 3: seq 201..250
	seg3 := make([]wal.Record, 50)
	for i := 0; i < 50; i++ {
		seq := uint64(201 + i)
		seg3[i] = makePutRecord(seq, fmt.Sprintf("m_%03d", seq), fmt.Sprintf("v_%03d", seq))
	}
	writeWALSegment(t, dir, 3, seg3...)

	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	if err := eng.RecoverWAL(); err != nil {
		t.Fatalf("RecoverWAL failed: %v", err)
	}

	// Records 181..250 should be replayed (total: 70 records)
	if eng.ActiveMemTable().Len() != 70 {
		t.Fatalf("expected 70 replayed records, got %d", eng.ActiveMemTable().Len())
	}
	if eng.NextSeqNum() != 250 {
		t.Fatalf("expected NextSeqNum=250, got %d", eng.NextSeqNum())
	}

	// Verify boundary: 180 absent from activeMem, 181 present
	_, err180 := eng.ActiveMemTable().SearchConcurrent([]byte("m_180"))
	if !stdErrors.Is(err180, errors.ErrKeyNotFound) {
		t.Fatalf("m_180 should not be in activeMem")
	}
	val181, err181 := eng.Get([]byte("m_181"))
	if err181 != nil || string(val181) != "v_181" {
		t.Fatalf("m_181 expected v_181, got %s (err: %v)", string(val181), err181)
	}
	val250, err250 := eng.Get([]byte("m_250"))
	if err250 != nil || string(val250) != "v_250" {
		t.Fatalf("m_250 expected v_250, got %s (err: %v)", string(val250), err250)
	}
}

// -----------------------------------------------------------------------------
// E. CORRUPT PRE-CHECKPOINT RECORD FAILS CLOSED (INV-02)
// -----------------------------------------------------------------------------

func TestEngineRecoverWAL_E_CorruptPreCheckpointRecord(t *testing.T) {
	dir := t.TempDir()

	// Checkpoint at 100
	res := setupTestManifestWithCheckpoint(t, dir, 100)
	res.Version.Unref()

	r1 := makePutRecord(10, "k10", "v10")
	r2 := makePutRecord(110, "k110", "v110")
	writeWALSegment(t, dir, 1, r1, r2)

	// Corrupt CRC of record 10 (pre-checkpoint) on disk
	seg1Path := wal.SegmentPath(dir, 1)
	corruptByteAt(t, seg1Path, 0, 0xFF)

	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	err := eng.RecoverWAL()
	if err == nil {
		t.Fatalf("expected RecoverWAL to fail closed on corrupt pre-checkpoint record")
	}
	if !stdErrors.Is(err, errors.ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch, got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// F. CORRUPT POST-CHECKPOINT RECORD FAILS CLOSED
// -----------------------------------------------------------------------------

func TestEngineRecoverWAL_F_CorruptPostCheckpointRecord(t *testing.T) {
	dir := t.TempDir()

	res := setupTestManifestWithCheckpoint(t, dir, 50)
	res.Version.Unref()

	r1 := makePutRecord(55, "k55", "v55")
	writeWALSegment(t, dir, 1, r1)

	// Corrupt post-checkpoint record on disk
	seg1Path := wal.SegmentPath(dir, 1)
	corruptByteAt(t, seg1Path, 0, 0xFF)

	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	err := eng.RecoverWAL()
	if err == nil || !stdErrors.Is(err, errors.ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch, got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// G. SEQUENCE REGRESSION FAILS CLOSED (INV-05)
// -----------------------------------------------------------------------------

func TestEngineRecoverWAL_G_SequenceRegression(t *testing.T) {
	dir := t.TempDir()

	r1 := makePutRecord(10, "k10", "v10")
	r2 := makePutRecord(8, "k8", "v8") // Sequence regression: 10 -> 8

	writeWALSegment(t, dir, 1, r1, r2)

	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	err := eng.RecoverWAL()
	if err == nil {
		t.Fatalf("expected RecoverWAL to fail on sequence regression")
	}
	var seqErr *errors.SequenceOutOfOrderError
	if !stdErrors.As(err, &seqErr) {
		t.Fatalf("expected *errors.SequenceOutOfOrderError, got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// H. HISTORICAL TORN TAIL FAILS CLOSED (INV-07)
// -----------------------------------------------------------------------------

func TestEngineRecoverWAL_H_HistoricalTornTail(t *testing.T) {
	dir := t.TempDir()

	r1 := makePutRecord(1, "k1", "v1")
	writeWALSegment(t, dir, 1, r1)

	// Append torn bytes to historical segment 1
	seg1Path := wal.SegmentPath(dir, 1)
	f, err := os.OpenFile(filepath.Clean(seg1Path), os.O_WRONLY|os.O_APPEND, 0600) // #nosec G304 - test-only torn tail injection
	if err != nil {
		t.Fatalf("failed to open segment: %v", err)
	}
	_, _ = f.Write([]byte{0x01, 0x02, 0x03, 0x04}) // partial 4-byte header
	_ = f.Close()

	// Segment 2 (latest)
	r2 := makePutRecord(2, "k2", "v2")
	writeWALSegment(t, dir, 2, r2)

	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	err = eng.RecoverWAL()
	if err == nil {
		t.Fatalf("expected failure on historical segment torn tail")
	}
}

// -----------------------------------------------------------------------------
// I. LATEST SEGMENT TORN TAIL IS RECOVERED (INV-06)
// -----------------------------------------------------------------------------

func TestEngineRecoverWAL_I_LatestSegmentTornTail(t *testing.T) {
	dir := t.TempDir()

	r1 := makePutRecord(1, "k1", "v1")
	writeWALSegment(t, dir, 1, r1)

	// Append torn tail to the latest segment 1
	seg1Path := wal.SegmentPath(dir, 1)
	f, err := os.OpenFile(filepath.Clean(seg1Path), os.O_WRONLY|os.O_APPEND, 0600) // #nosec G304 - test-only torn tail injection
	if err != nil {
		t.Fatalf("failed to open segment: %v", err)
	}
	_, _ = f.Write([]byte{0xAA, 0xBB, 0xCC}) // 3-byte torn fragment
	_ = f.Close()

	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	// Replay should truncate latest segment torn tail and recover r1
	if err := eng.RecoverWAL(); err != nil {
		t.Fatalf("RecoverWAL failed on latest segment torn tail: %v", err)
	}

	val, err := eng.Get([]byte("k1"))
	if err != nil || string(val) != "v1" {
		t.Fatalf("expected k1 -> v1, got %s (err: %v)", string(val), err)
	}
}

// -----------------------------------------------------------------------------
// J. MISSING / INVALID WAL SEGMENT (INV-01)
// -----------------------------------------------------------------------------

func TestEngineRecoverWAL_J_SegmentGapAndInvalidObject(t *testing.T) {
	// 1. Segment Gap: 1 and 3 (missing 2)
	dir := t.TempDir()
	writeWALSegment(t, dir, 1, makePutRecord(1, "k1", "v1"))
	writeWALSegment(t, dir, 3, makePutRecord(2, "k2", "v2"))

	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	err := eng.RecoverWAL()
	if err == nil {
		t.Fatalf("expected error on segment gap")
	}
	var gapErr *errors.SegmentGapError
	if !stdErrors.As(err, &gapErr) {
		t.Fatalf("expected *errors.SegmentGapError, got: %v", err)
	}

	// 2. Segment is a directory
	dir2 := t.TempDir()
	if _, err := wal.InitDir(dir2); err != nil {
		t.Fatalf("failed to init wal dir: %v", err)
	}
	fakeSegDir := wal.SegmentPath(dir2, 1)
	if err := os.Mkdir(fakeSegDir, 0700); err != nil {
		t.Fatalf("failed to mkdir: %v", err)
	}

	eng2 := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir2,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng2.Close() }()

	if err := eng2.RecoverWAL(); err == nil {
		t.Fatalf("expected error for segment directory masquerade")
	}
}

// -----------------------------------------------------------------------------
// K. MID-REPLAY FAILURE STATE ISOLATION (INV-09, INV-10)
// -----------------------------------------------------------------------------

func TestEngineRecoverWAL_K_MidReplayStateIsolation(t *testing.T) {
	dir := t.TempDir()

	// Initial engine state with pre-existing data
	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	ctx := context.Background()
	if err := eng.Put(ctx, []byte("pre_existing"), []byte("pre_val")); err != nil {
		t.Fatalf("initial Put failed: %v", err)
	}

	// Create WAL where second record has bad CRC on disk
	r1 := makePutRecord(1, "k1", "v1")
	r2 := makePutRecord(2, "k2", "v2")
	writeWALSegment(t, dir, 1, r1, r2)

	// Corrupt r2 on disk (r1 size is 21 + 4+2 + 4+2 = 33 bytes; r2 starts at offset 33)
	seg1Path := wal.SegmentPath(dir, 1)
	corruptByteAt(t, seg1Path, 33, 0xFF)

	// Attempt RecoverWAL - must fail
	err := eng.RecoverWAL()
	if err == nil {
		t.Fatalf("expected recovery to fail on corrupt record")
	}

	// Verify pre-existing engine state was NOT wiped or polluted
	val, getErr := eng.Get([]byte("pre_existing"))
	if getErr != nil || string(val) != "pre_val" {
		t.Fatalf("pre-existing key was lost or mutated: %v", getErr)
	}

	// Verify k1 was NOT published into live engine state
	_, k1Err := eng.Get([]byte("k1"))
	if !stdErrors.Is(k1Err, errors.ErrKeyNotFound) {
		t.Fatalf("k1 was published despite mid-replay failure")
	}
}

// -----------------------------------------------------------------------------
// L. MIXED PUT / DELETE STATE (INV-08)
// -----------------------------------------------------------------------------

func TestEngineRecoverWAL_L_MixedPutDeleteTombstones(t *testing.T) {
	dir := t.TempDir()

	r1 := makePutRecord(1, "keyA", "valA1")
	r2 := makePutRecord(2, "keyB", "valB")
	r3 := makeDeleteRecord(3, "keyA") // tombstone deleting keyA
	r4 := makePutRecord(4, "keyC", "valC")
	r5 := makePutRecord(5, "keyA", "valA2") // re-insert keyA with higher seqNum
	r6 := makeDeleteRecord(6, "keyA")       // final delete of keyA

	writeWALSegment(t, dir, 1, r1, r2, r3, r4, r5, r6)

	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	if err := eng.RecoverWAL(); err != nil {
		t.Fatalf("RecoverWAL failed: %v", err)
	}

	// keyA should be deleted (tombstone)
	_, errA := eng.Get([]byte("keyA"))
	if !stdErrors.Is(errA, errors.ErrKeyNotFound) {
		t.Fatalf("expected keyA to be deleted, got err: %v", errA)
	}

	// keyB should be present
	valB, errB := eng.Get([]byte("keyB"))
	if errB != nil || string(valB) != "valB" {
		t.Fatalf("keyB expected valB, got %s (err: %v)", string(valB), errB)
	}

	// keyC should be present
	valC, errC := eng.Get([]byte("keyC"))
	if errC != nil || string(valC) != "valC" {
		t.Fatalf("keyC expected valC, got %s (err: %v)", string(valC), errC)
	}
}

// -----------------------------------------------------------------------------
// M. ATOMIC BATCH RECOVERY
// -----------------------------------------------------------------------------

func TestEngineRecoverWAL_M_BatchAtomicity(t *testing.T) {
	dir := t.TempDir()

	// Batch 1: Committed (seq 1..4)
	b1Start := makeBatchMarker(1, wal.RecordTypeBatchStart)
	b1R1 := makePutRecord(2, "batch1_k1", "batch1_v1")
	b1R2 := makePutRecord(3, "batch1_k2", "batch1_v2")
	b1Commit := makeBatchMarker(4, wal.RecordTypeBatchCommit)

	// Batch 2: Uncommitted (torn before commit marker at EOF) (seq 5..6)
	b2Start := makeBatchMarker(5, wal.RecordTypeBatchStart)
	b2R1 := makePutRecord(6, "batch2_uncommitted", "val_uncommitted")

	writeWALSegment(t, dir, 1, b1Start, b1R1, b1R2, b1Commit, b2Start, b2R1)

	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	if err := eng.RecoverWAL(); err != nil {
		t.Fatalf("RecoverWAL failed: %v", err)
	}

	// Batch 1 records must be present
	val1, err1 := eng.Get([]byte("batch1_k1"))
	if err1 != nil || string(val1) != "batch1_v1" {
		t.Fatalf("batch1_k1 not found: %v", err1)
	}
	val2, err2 := eng.Get([]byte("batch1_k2"))
	if err2 != nil || string(val2) != "batch1_v2" {
		t.Fatalf("batch1_k2 not found: %v", err2)
	}

	// Batch 2 records must NOT be present (uncommitted batch dropped)
	_, errUncommitted := eng.Get([]byte("batch2_uncommitted"))
	if !stdErrors.Is(errUncommitted, errors.ErrKeyNotFound) {
		t.Fatalf("uncommitted batch record was unexpectedly applied: err=%v", errUncommitted)
	}
}

// -----------------------------------------------------------------------------
// N. SEQUENCE NUMBER CONTINUATION AFTER RECOVERY (INV-12)
// -----------------------------------------------------------------------------

func TestEngineRecoverWAL_N_SequenceContinuation(t *testing.T) {
	dir := t.TempDir()

	res := setupTestManifestWithCheckpoint(t, dir, 100)
	res.Version.Unref()

	// WAL records up to 140
	r1 := makePutRecord(110, "k110", "v110")
	r2 := makePutRecord(140, "k140", "v140")
	writeWALSegment(t, dir, 1, r1, r2)

	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	if err := eng.RecoverWAL(); err != nil {
		t.Fatalf("RecoverWAL failed: %v", err)
	}

	if eng.NextSeqNum() != 140 {
		t.Fatalf("expected NextSeqNum=140, got %d", eng.NextSeqNum())
	}

	// Next write must allocate sequence 141 (> 140)
	ctx := context.Background()
	if err := eng.Put(ctx, []byte("next_key"), []byte("next_val")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	if eng.NextSeqNum() != 141 {
		t.Fatalf("expected NextSeqNum=141, got %d", eng.NextSeqNum())
	}
}

// -----------------------------------------------------------------------------
// O. DIRECT COMPOSITION WITH P07-S01-M02 ReplayResult (SECTION 5)
// -----------------------------------------------------------------------------

func TestEngineRecoverWAL_O_DirectResultComposition(t *testing.T) {
	dir := t.TempDir()

	// Replay manifest directly to obtain ReplayResult
	replayRes := setupTestManifestWithCheckpoint(t, dir, 200)

	// Write uncommitted WAL records (seq 201..210)
	records := make([]wal.Record, 10)
	for i := 0; i < 10; i++ {
		seq := uint64(201 + i)
		records[i] = makePutRecord(seq, fmt.Sprintf("direct_%d", seq), fmt.Sprintf("val_%d", seq))
	}
	writeWALSegment(t, dir, 1, records...)

	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	// Compose with pre-computed ReplayResult without re-reading the MANIFEST
	if err := eng.RecoverWALWithManifestResult(replayRes); err != nil {
		t.Fatalf("RecoverWALWithManifestResult failed: %v", err)
	}

	if eng.ActiveMemTable().Len() != 10 {
		t.Fatalf("expected 10 records, got %d", eng.ActiveMemTable().Len())
	}
	if eng.NextSeqNum() != 210 {
		t.Fatalf("expected NextSeqNum=210, got %d", eng.NextSeqNum())
	}
}
