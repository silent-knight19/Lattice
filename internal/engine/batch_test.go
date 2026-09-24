package engine_test

import (
	stdErrors "errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

// mockFailingBatchWAL allows injecting errors on Append or Sync calls to verify failure propagation.
type mockFailingBatchWAL struct {
	failOnType  wal.RecordType
	failOnSync  bool
	appendCalls atomic.Int32
	syncCalls   atomic.Int32
	records     []wal.Record
	mu          sync.Mutex
	err         error
}

func (m *mockFailingBatchWAL) Append(rec wal.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appendCalls.Add(1)
	if m.err != nil && rec.Type == m.failOnType {
		return m.err
	}
	m.records = append(m.records, rec)
	return nil
}

func (m *mockFailingBatchWAL) Sync() error {
	m.syncCalls.Add(1)
	if m.err != nil && m.failOnSync {
		return m.err
	}
	return nil
}

func (m *mockFailingBatchWAL) AppendSync(rec wal.Record) error {
	if err := m.Append(rec); err != nil {
		return err
	}
	return m.Sync()
}

func (m *mockFailingBatchWAL) Close() error {
	return nil
}

// -----------------------------------------------------------------------------
// 1. INPUT VALIDATION TESTS
// -----------------------------------------------------------------------------

func TestEngineBatch_Validation(t *testing.T) {
	eng := newMemEngine()
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	// 1. Nil receiver
	var nilEng *engine.Engine
	if err := nilEng.Batch(ctx, []binary.BatchOp{{Type: binary.OpTypePut, Key: []byte("k"), Value: []byte("v")}}); !stdErrors.Is(err, errors.ErrNilReceiver) {
		t.Fatalf("nilEng.Batch got %v, want ErrNilReceiver", err)
	}

	// 2. Empty batch
	if err := eng.Batch(ctx, nil); err == nil {
		t.Fatalf("empty batch expected error, got nil")
	}
	if err := eng.Batch(ctx, []binary.BatchOp{}); err == nil {
		t.Fatalf("empty batch slice expected error, got nil")
	}

	// 3. Batch operation count > 1024
	hugeOps := make([]binary.BatchOp, 1025)
	for i := range hugeOps {
		hugeOps[i] = binary.BatchOp{Type: binary.OpTypePut, Key: []byte(fmt.Sprintf("k%d", i)), Value: []byte("v")}
	}
	if err := eng.Batch(ctx, hugeOps); err == nil {
		t.Fatalf("expected error for >1024 ops, got nil")
	}

	// 4. Invalid key length (empty key)
	if err := eng.Batch(ctx, []binary.BatchOp{
		{Type: binary.OpTypePut, Key: []byte("valid"), Value: []byte("val")},
		{Type: binary.OpTypePut, Key: []byte(""), Value: []byte("val")},
	}); err == nil {
		t.Fatalf("expected error for empty key in batch, got nil")
	}

	// 5. Invalid key length (> 65535 bytes)
	tooLongKey := make([]byte, 65536)
	if err := eng.Batch(ctx, []binary.BatchOp{
		{Type: binary.OpTypePut, Key: tooLongKey, Value: []byte("val")},
	}); err == nil {
		t.Fatalf("expected error for oversized key in batch, got nil")
	}

	// 6. Invalid DELETE with non-empty value
	if err := eng.Batch(ctx, []binary.BatchOp{
		{Type: binary.OpTypeDelete, Key: []byte("k"), Value: []byte("unexpected_value")},
	}); err == nil {
		t.Fatalf("expected error for DELETE with non-empty value, got nil")
	}

	// 7. Invalid OpType
	if err := eng.Batch(ctx, []binary.BatchOp{
		{Type: binary.OpType(0x99), Key: []byte("k"), Value: []byte("v")},
	}); err == nil {
		t.Fatalf("expected error for invalid op type, got nil")
	}
}

// -----------------------------------------------------------------------------
// 2. DETERMINISTIC ORDERING & REPEATED KEYS
// -----------------------------------------------------------------------------

func TestEngineBatch_DeterministicOrderingAndRepeatedKeys(t *testing.T) {
	eng := newMemEngine()
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	// Scenario A: PUT A=1, PUT A=2, DELETE A, PUT A=3 -> Final state must be A=3
	opsA := []binary.BatchOp{
		{Type: binary.OpTypePut, Key: []byte("keyA"), Value: []byte("val1")},
		{Type: binary.OpTypePut, Key: []byte("keyA"), Value: []byte("val2")},
		{Type: binary.OpTypeDelete, Key: []byte("keyA")},
		{Type: binary.OpTypePut, Key: []byte("keyA"), Value: []byte("val3")},
		{Type: binary.OpTypePut, Key: []byte("keyB"), Value: []byte("valB")},
	}
	if err := eng.Batch(ctx, opsA); err != nil {
		t.Fatalf("Batch failed: %v", err)
	}

	gotA, err := eng.Get([]byte("keyA"))
	if err != nil {
		t.Fatalf("Get(keyA) failed: %v", err)
	}
	if string(gotA) != "val3" {
		t.Fatalf("Get(keyA) got %q, want val3", string(gotA))
	}

	gotB, err := eng.Get([]byte("keyB"))
	if err != nil || string(gotB) != "valB" {
		t.Fatalf("Get(keyB) got %q %v, want valB", string(gotB), err)
	}

	// Scenario B: PUT A=4, DELETE A -> Final state must be NotFound (tombstone shadows)
	opsB := []binary.BatchOp{
		{Type: binary.OpTypePut, Key: []byte("keyA"), Value: []byte("val4")},
		{Type: binary.OpTypeDelete, Key: []byte("keyA")},
	}
	if err := eng.Batch(ctx, opsB); err != nil {
		t.Fatalf("Batch failed: %v", err)
	}

	_, errNotFound := eng.Get([]byte("keyA"))
	if !stdErrors.Is(errNotFound, errors.ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound after DELETE in batch, got %v", errNotFound)
	}
}

// -----------------------------------------------------------------------------
// 3. PHYSICAL WAL DURABILITY & MONOTONIC SEQUENCE NUMBERS
// -----------------------------------------------------------------------------

func TestEngineBatch_WALDurabilityAndMonotonicSequence(t *testing.T) {
	dir := t.TempDir()
	eng := newRealWALEngine(t, dir, 0)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	initSeq := eng.NextSeqNum()

	ops := []binary.BatchOp{
		{Type: binary.OpTypePut, Key: []byte("w_key1"), Value: []byte("w_val1")},
		{Type: binary.OpTypePut, Key: []byte("w_key2"), Value: []byte("w_val2")},
		{Type: binary.OpTypeDelete, Key: []byte("w_key1")},
		{Type: binary.OpTypePut, Key: []byte("w_key3"), Value: []byte("w_val3")},
	}

	if err := eng.Batch(ctx, ops); err != nil {
		t.Fatalf("Batch failed: %v", err)
	}

	// NextSeqNum must have advanced by len(ops) + 2 (BATCH_START + 4 ops + BATCH_COMMIT = 6)
	expectedNewSeq := initSeq + uint64(len(ops)+2)
	if eng.NextSeqNum() != expectedNewSeq {
		t.Fatalf("expected NextSeqNum=%d, got %d", expectedNewSeq, eng.NextSeqNum())
	}

	// Verify physical WAL records on disk
	segIDs, err := wal.ListSegments(dir)
	if err != nil || len(segIDs) == 0 {
		t.Fatalf("failed to list WAL segments: ids=%v, err=%v", segIDs, err)
	}

	reader, err := wal.OpenSegmentReader(dir, segIDs[0])
	if err != nil {
		t.Fatalf("failed to open segment reader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	var readRecords []wal.Record
	for {
		rec, err := reader.Next()
		if err != nil {
			break
		}
		readRecords = append(readRecords, rec)
	}

	// Expect exactly 6 records
	if len(readRecords) != 6 {
		t.Fatalf("expected 6 WAL records, got %d", len(readRecords))
	}

	// Record 0: BATCH_START
	if readRecords[0].Type != wal.RecordTypeBatchStart {
		t.Fatalf("record 0: expected BATCH_START, got %v", readRecords[0].Type)
	}
	if readRecords[0].SeqNum != binary.SeqNum(initSeq+1) {
		t.Fatalf("record 0: expected SeqNum %d, got %d", initSeq+1, readRecords[0].SeqNum)
	}

	// Record 1: PUT w_key1
	if readRecords[1].Type != wal.RecordTypePut || string(readRecords[1].Key) != "w_key1" {
		t.Fatalf("record 1: unexpected %v %s", readRecords[1].Type, string(readRecords[1].Key))
	}
	if readRecords[1].SeqNum != binary.SeqNum(initSeq+2) {
		t.Fatalf("record 1: expected SeqNum %d, got %d", initSeq+2, readRecords[1].SeqNum)
	}

	// Record 2: PUT w_key2
	if readRecords[2].Type != wal.RecordTypePut || string(readRecords[2].Key) != "w_key2" {
		t.Fatalf("record 2: unexpected %v", readRecords[2].Type)
	}
	if readRecords[2].SeqNum != binary.SeqNum(initSeq+3) {
		t.Fatalf("record 2: expected SeqNum %d, got %d", initSeq+3, readRecords[2].SeqNum)
	}

	// Record 3: DELETE w_key1
	if readRecords[3].Type != wal.RecordTypeDelete || string(readRecords[3].Key) != "w_key1" {
		t.Fatalf("record 3: unexpected %v", readRecords[3].Type)
	}
	if readRecords[3].SeqNum != binary.SeqNum(initSeq+4) {
		t.Fatalf("record 3: expected SeqNum %d, got %d", initSeq+4, readRecords[3].SeqNum)
	}

	// Record 4: PUT w_key3
	if readRecords[4].Type != wal.RecordTypePut || string(readRecords[4].Key) != "w_key3" {
		t.Fatalf("record 4: unexpected %v", readRecords[4].Type)
	}
	if readRecords[4].SeqNum != binary.SeqNum(initSeq+5) {
		t.Fatalf("record 4: expected SeqNum %d, got %d", initSeq+5, readRecords[4].SeqNum)
	}

	// Record 5: BATCH_COMMIT
	if readRecords[5].Type != wal.RecordTypeBatchCommit {
		t.Fatalf("record 5: expected BATCH_COMMIT, got %v", readRecords[5].Type)
	}
	if readRecords[5].SeqNum != binary.SeqNum(initSeq+6) {
		t.Fatalf("record 5: expected SeqNum %d, got %d", initSeq+6, readRecords[5].SeqNum)
	}
}

// -----------------------------------------------------------------------------
// 4. FAILURE PROPAGATION (ZERO PARTIAL MEMORY VISIBILITY)
// -----------------------------------------------------------------------------

func TestEngineBatch_WALFailurePropagation(t *testing.T) {
	boom := stdErrors.New("injected wal error")

	// Subtest A: Failure during BATCH_START append
	t.Run("FailOnBatchStart", func(t *testing.T) {
		eng := newMemEngine()
		defer func() { _ = eng.Close() }()
		mock := &mockFailingBatchWAL{failOnType: wal.RecordTypeBatchStart, err: boom}
		eng.SetWALForTesting(mock)

		ops := []binary.BatchOp{
			{Type: binary.OpTypePut, Key: []byte("k1"), Value: []byte("v1")},
		}
		err := eng.Batch(testCtx(), ops)
		if !stdErrors.Is(err, boom) {
			t.Fatalf("expected injected error, got %v", err)
		}

		// In-memory must not have mutated
		if _, err := eng.Get([]byte("k1")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
			t.Fatalf("expected key not found in memory, got %v", err)
		}
	})

	// Subtest B: Failure during record append
	t.Run("FailOnRecordAppend", func(t *testing.T) {
		eng := newMemEngine()
		defer func() { _ = eng.Close() }()
		mock := &mockFailingBatchWAL{failOnType: wal.RecordTypePut, err: boom}
		eng.SetWALForTesting(mock)

		ops := []binary.BatchOp{
			{Type: binary.OpTypePut, Key: []byte("k1"), Value: []byte("v1")},
		}
		err := eng.Batch(testCtx(), ops)
		if !stdErrors.Is(err, boom) {
			t.Fatalf("expected injected error, got %v", err)
		}

		if _, err := eng.Get([]byte("k1")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
			t.Fatalf("expected key not found in memory, got %v", err)
		}
	})

	// Subtest C: Failure during BATCH_COMMIT append
	t.Run("FailOnCommitAppend", func(t *testing.T) {
		eng := newMemEngine()
		defer func() { _ = eng.Close() }()
		mock := &mockFailingBatchWAL{failOnType: wal.RecordTypeBatchCommit, err: boom}
		eng.SetWALForTesting(mock)

		ops := []binary.BatchOp{
			{Type: binary.OpTypePut, Key: []byte("k1"), Value: []byte("v1")},
		}
		err := eng.Batch(testCtx(), ops)
		if !stdErrors.Is(err, boom) {
			t.Fatalf("expected injected error, got %v", err)
		}

		if _, err := eng.Get([]byte("k1")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
			t.Fatalf("expected key not found in memory, got %v", err)
		}
	})

	// Subtest D: Failure during Sync
	t.Run("FailOnSync", func(t *testing.T) {
		eng := newMemEngine()
		defer func() { _ = eng.Close() }()
		mock := &mockFailingBatchWAL{failOnSync: true, err: boom}
		eng.SetWALForTesting(mock)

		ops := []binary.BatchOp{
			{Type: binary.OpTypePut, Key: []byte("k1"), Value: []byte("v1")},
		}
		err := eng.Batch(testCtx(), ops)
		if !stdErrors.Is(err, boom) {
			t.Fatalf("expected injected error, got %v", err)
		}

		if _, err := eng.Get([]byte("k1")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
			t.Fatalf("expected key not found in memory, got %v", err)
		}
	})
}

// -----------------------------------------------------------------------------
// 5. CRASH & RESTART RECOVERY ATOMICITY
// -----------------------------------------------------------------------------

func TestEngineBatch_CrashAndRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	eng := newRealWALEngine(t, dir, 0)
	ctx := testCtx()

	// 1. Normal writes followed by a batch
	if err := eng.Put(ctx, []byte("pre_key"), []byte("pre_val")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	batch1 := []binary.BatchOp{
		{Type: binary.OpTypePut, Key: []byte("b1_k1"), Value: []byte("b1_v1")},
		{Type: binary.OpTypePut, Key: []byte("b1_k2"), Value: []byte("b1_v2")},
		{Type: binary.OpTypeDelete, Key: []byte("pre_key")},
	}
	if err := eng.Batch(ctx, batch1); err != nil {
		t.Fatalf("Batch1 failed: %v", err)
	}

	// 2. Batch followed by normal write
	if err := eng.Put(ctx, []byte("post_key"), []byte("post_val")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// 3. Second batch
	batch2 := []binary.BatchOp{
		{Type: binary.OpTypePut, Key: []byte("b2_k1"), Value: []byte("b2_v1")},
		{Type: binary.OpTypePut, Key: []byte("b1_k1"), Value: []byte("b1_v1_updated")},
	}
	if err := eng.Batch(ctx, batch2); err != nil {
		t.Fatalf("Batch2 failed: %v", err)
	}

	// Close cleanly
	if err := eng.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Reopen engine and verify recovery
	engReopened := openExistingDir(t, dir)
	defer func() { _ = engReopened.Close() }()

	// pre_key must be deleted
	if _, err := engReopened.Get([]byte("pre_key")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("pre_key expected ErrKeyNotFound, got %v", err)
	}

	// b1_k1 must be updated
	if val, err := engReopened.Get([]byte("b1_k1")); err != nil || string(val) != "b1_v1_updated" {
		t.Fatalf("b1_k1 got %q %v, want b1_v1_updated", string(val), err)
	}

	// b1_k2 must be present
	if val, err := engReopened.Get([]byte("b1_k2")); err != nil || string(val) != "b1_v2" {
		t.Fatalf("b1_k2 got %q %v, want b1_v2", string(val), err)
	}

	// post_key must be present
	if val, err := engReopened.Get([]byte("post_key")); err != nil || string(val) != "post_val" {
		t.Fatalf("post_key got %q %v, want post_val", string(val), err)
	}

	// b2_k1 must be present
	if val, err := engReopened.Get([]byte("b2_k1")); err != nil || string(val) != "b2_v1" {
		t.Fatalf("b2_k1 got %q %v, want b2_v1", string(val), err)
	}
}

// TestEngineBatch_TornBatchRecovery verifies that an interrupted/torn batch in WAL is cleanly discarded.
func TestEngineBatch_TornBatchRecovery(t *testing.T) {
	dir := t.TempDir()

	// Manually write a WAL segment with a committed batch and an uncommitted/torn batch
	b1Start := wal.Record{Type: wal.RecordTypeBatchStart, SeqNum: 1, Timestamp: 100}
	b1R1 := wal.Record{Type: wal.RecordTypePut, SeqNum: 2, Timestamp: 101, Key: []byte("c_k1"), Value: []byte("c_v1")}
	b1Commit := wal.Record{Type: wal.RecordTypeBatchCommit, SeqNum: 3, Timestamp: 102}

	// Torn batch: BATCH_START + PUT without BATCH_COMMIT
	tornStart := wal.Record{Type: wal.RecordTypeBatchStart, SeqNum: 4, Timestamp: 103}
	tornPut := wal.Record{Type: wal.RecordTypePut, SeqNum: 5, Timestamp: 104, Key: []byte("torn_k"), Value: []byte("torn_v")}

	writeWALSegment(t, dir, 1, b1Start, b1R1, b1Commit, tornStart, tornPut)

	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	if err := eng.RecoverWAL(); err != nil {
		t.Fatalf("RecoverWAL failed: %v", err)
	}

	// Committed batch record must exist
	if val, err := eng.Get([]byte("c_k1")); err != nil || string(val) != "c_v1" {
		t.Fatalf("c_k1 got %q %v, want c_v1", string(val), err)
	}

	// Torn batch record must NOT exist
	if _, err := eng.Get([]byte("torn_k")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("torn_k must not be present after recovery: %v", err)
	}
}

// -----------------------------------------------------------------------------
// 6. CONCURRENT READ ATOMICITY
// -----------------------------------------------------------------------------

func parseVer(val []byte) int {
	if len(val) == 0 {
		return 0
	}
	var ver int
	_, _ = fmt.Sscanf(string(val), "ver_%d", &ver)
	return ver
}

func TestEngineBatch_ConcurrentReaderAtomicity(t *testing.T) {
	eng := newMemEngine()
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	const iterations = 100
	const numKeys = 10

	var stop atomic.Bool
	var violationCount atomic.Int64
	var wg sync.WaitGroup

	// Reader goroutines: test that no reader ever observes a partially applied batch.
	// If key 0 is observed at version V, then key N-1 (which was written AFTER key 0 in the batch)
	// must already be at least version V (never an older version or missing).
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			firstKey := []byte(fmt.Sprintf("atom_key_%02d", 0))
			lastKey := []byte(fmt.Sprintf("atom_key_%02d", numKeys-1))

			for !stop.Load() {
				if readerID%2 == 0 {
					// Forward read: read first key, then last key
					valFirst, errFirst := eng.Get(firstKey)
					valLast, errLast := eng.Get(lastKey)

					if errFirst == nil {
						vFirst := parseVer(valFirst)
						if errLast != nil {
							// First key is at vFirst > 0, but last key is missing -> partial batch visibility!
							violationCount.Add(1)
						} else {
							vLast := parseVer(valLast)
							if vLast < vFirst {
								// Last key has an older version than first key -> partial batch visibility!
								violationCount.Add(1)
							}
						}
					}
				} else {
					// Reverse read: read last key, then first key
					valLast, errLast := eng.Get(lastKey)
					valFirst, errFirst := eng.Get(firstKey)

					if errLast == nil {
						vLast := parseVer(valLast)
						if errFirst != nil {
							violationCount.Add(1)
						} else {
							vFirst := parseVer(valFirst)
							if vFirst < vLast {
								violationCount.Add(1)
							}
						}
					}
				}
				time.Sleep(10 * time.Microsecond)
			}
		}(r)
	}

	// Writer: repeatedly apply multi-key batches with incrementing version tags
	for iter := 1; iter <= iterations; iter++ {
		verStr := fmt.Sprintf("ver_%d", iter)
		ops := make([]binary.BatchOp, numKeys)
		for k := 0; k < numKeys; k++ {
			ops[k] = binary.BatchOp{
				Type:  binary.OpTypePut,
				Key:   []byte(fmt.Sprintf("atom_key_%02d", k)),
				Value: []byte(verStr),
			}
		}
		if err := eng.Batch(ctx, ops); err != nil {
			t.Fatalf("Batch failed at iteration %d: %v", iter, err)
		}
		time.Sleep(50 * time.Microsecond)
	}

	stop.Store(true)
	wg.Wait()

	if violations := violationCount.Load(); violations > 0 {
		t.Fatalf("ATOMICITY VIOLATION: readers observed partial batch states %d times!", violations)
	}
}

// -----------------------------------------------------------------------------
// 7. MEMTABLE CAPACITY & PROACTIVE ROTATION
// -----------------------------------------------------------------------------

func TestEngineBatch_MemTableRotation(t *testing.T) {
	// Engine with tiny flush threshold (4 KiB) to trigger MemTable rotation
	eng, _ := newFlushEngine(t, 4096)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	// Large batch (100 ops of 64 bytes each ≈ 6.4KB > 4KB threshold)
	ops := make([]binary.BatchOp, 100)
	for i := range ops {
		ops[i] = binary.BatchOp{
			Type:  binary.OpTypePut,
			Key:   []byte(fmt.Sprintf("rot_key_%03d", i)),
			Value: []byte(fmt.Sprintf("rot_val_%03d_with_padding_payload_data_here", i)),
		}
	}

	if err := eng.Batch(ctx, ops); err != nil {
		t.Fatalf("Batch failed: %v", err)
	}

	// Verify all keys are retrievable
	for i := range ops {
		key := fmt.Sprintf("rot_key_%03d", i)
		expected := fmt.Sprintf("rot_val_%03d_with_padding_payload_data_here", i)
		val, err := eng.Get([]byte(key))
		if err != nil {
			t.Fatalf("Get(%s) failed: %v", key, err)
		}
		if string(val) != expected {
			t.Fatalf("Get(%s) got %q, want %q", key, string(val), expected)
		}
	}
}
