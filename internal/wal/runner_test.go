package wal_test

import (
	"bytes"
	"context"
	stdErrors "errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

// mockBatchWriter is an instrumented BatchWriter for fault injection and exact barrier assertions.
type mockBatchWriter struct {
	mu           sync.Mutex
	appended     []wal.Record
	appendErrFn  func(rec wal.Record, index int) error
	syncErrFn    func(callCount int) error
	syncCalls    int64
	appendCalls  int64
	syncCallback func()
}

func (m *mockBatchWriter) Append(rec wal.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := int(m.appendCalls)
	m.appendCalls++
	if m.appendErrFn != nil {
		if err := m.appendErrFn(rec, idx); err != nil {
			return err
		}
	}
	m.appended = append(m.appended, rec)
	return nil
}

func (m *mockBatchWriter) Sync() error {
	calls := atomic.AddInt64(&m.syncCalls, 1)
	m.mu.Lock()
	fn := m.syncErrFn
	cb := m.syncCallback
	m.mu.Unlock()

	if cb != nil {
		cb()
	}
	if fn != nil {
		return fn(int(calls))
	}
	return nil
}

func (m *mockBatchWriter) SyncCalls() int64 {
	return atomic.LoadInt64(&m.syncCalls)
}

func (m *mockBatchWriter) AppendCalls() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.appendCalls
}

func (m *mockBatchWriter) AppendedRecords() []wal.Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]wal.Record, len(m.appended))
	copy(cp, m.appended)
	return cp
}

func createTestTask(t *testing.T, seq uint64, key string, val string) *wal.WriteTask {
	t.Helper()
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(seq),
		Timestamp: 1725800000 + seq,
		Key:       []byte(key),
		Value:     []byte(val),
	}
	task, err := wal.NewWriteTask(rec)
	if err != nil {
		t.Fatalf("NewWriteTask failed: %v", err)
	}
	return task
}

func createRealWriter(t *testing.T, dbPath string) (*wal.WALWriter, string) {
	t.Helper()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}
	w, err := wal.OpenSegmentWriter(dbPath, 1)
	if err != nil {
		t.Fatalf("OpenSegmentWriter failed: %v", err)
	}
	return w, w.Path()
}

func readAllRecordsFromSegment(t *testing.T, path string) []wal.Record {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("failed to open segment %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	var records []wal.Record
	for {
		rec, err := wal.DecodeRecord(f)
		if err != nil {
			if stdErrors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("DecodeRecord failed at record %d: %v", len(records), err)
		}
		records = append(records, rec)
	}
	return records
}

// Test 1: Single task batch succeeds.
func TestRunner_1_SingleTaskBatchSucceeds(t *testing.T) {
	dbPath := t.TempDir()
	w, segPath := createRealWriter(t, dbPath)
	defer func() { _ = w.Close() }()

	q, err := wal.NewWriteQueue(16)
	if err != nil {
		t.Fatalf("NewWriteQueue failed: %v", err)
	}

	runner, err := wal.NewGroupCommitRunner(q, w, wal.DefaultRunnerOptions())
	if err != nil {
		t.Fatalf("NewGroupCommitRunner failed: %v", err)
	}

	if err := runner.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer func() { _ = runner.Stop() }()

	task := createTestTask(t, 1, "k1", "v1")
	if err := q.Enqueue(task); err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}

	if err := task.Wait(); err != nil {
		t.Fatalf("task.Wait failed: %v", err)
	}

	_ = runner.Stop()

	if runner.SyncCount() != 1 {
		t.Errorf("SyncCount mismatch: got %d, want 1", runner.SyncCount())
	}
	if runner.BatchesExecuted() != 1 {
		t.Errorf("BatchesExecuted mismatch: got %d, want 1", runner.BatchesExecuted())
	}
	if runner.TasksExecuted() != 1 {
		t.Errorf("TasksExecuted mismatch: got %d, want 1", runner.TasksExecuted())
	}

	// Physical read verification
	recs := readAllRecordsFromSegment(t, segPath)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record in WAL, got %d", len(recs))
	}
	if !bytes.Equal(recs[0].Key, []byte("k1")) || !bytes.Equal(recs[0].Value, []byte("v1")) {
		t.Errorf("record mismatch: got key=%s, val=%s", recs[0].Key, recs[0].Value)
	}
}

// Test 2: Multiple tasks form one batch.
func TestRunner_2_MultipleTasksFormOneBatch(t *testing.T) {
	q, _ := wal.NewWriteQueue(16)
	mock := &mockBatchWriter{}
	runner, err := wal.NewGroupCommitRunner(q, mock, wal.DefaultRunnerOptions())
	if err != nil {
		t.Fatalf("NewGroupCommitRunner failed: %v", err)
	}

	// Pre-enqueue 5 tasks before starting runner
	tasks := make([]*wal.WriteTask, 5)
	for i := 0; i < 5; i++ {
		tasks[i] = createTestTask(t, uint64(i+1), fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i))
		if err := q.Enqueue(tasks[i]); err != nil {
			t.Fatalf("Enqueue failed: %v", err)
		}
	}

	if err := runner.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer func() { _ = runner.Stop() }()

	for i, task := range tasks {
		if err := task.Wait(); err != nil {
			t.Fatalf("task %d Wait failed: %v", i, err)
		}
	}

	_ = runner.Stop()

	// Proves 5 tasks = 1 batch = 1 sync
	if runner.SyncCount() != 1 {
		t.Errorf("SyncCount mismatch: got %d, want 1", runner.SyncCount())
	}
	if runner.BatchesExecuted() != 1 {
		t.Errorf("BatchesExecuted mismatch: got %d, want 1", runner.BatchesExecuted())
	}
	if runner.TasksExecuted() != 5 {
		t.Errorf("TasksExecuted mismatch: got %d, want 5", runner.TasksExecuted())
	}
}

// Test 3: FIFO order preserved.
func TestRunner_3_FIFOOrderPreserved(t *testing.T) {
	dbPath := t.TempDir()
	w, segPath := createRealWriter(t, dbPath)
	defer func() { _ = w.Close() }()

	q, _ := wal.NewWriteQueue(100)
	runner, _ := wal.NewGroupCommitRunner(q, w, wal.DefaultRunnerOptions())

	const numTasks = 20
	tasks := make([]*wal.WriteTask, numTasks)
	for i := 0; i < numTasks; i++ {
		tasks[i] = createTestTask(t, uint64(i+1), fmt.Sprintf("k_%03d", i+1), fmt.Sprintf("v_%03d", i+1))
		_ = q.Enqueue(tasks[i])
	}

	_ = runner.Start()
	for _, task := range tasks {
		if err := task.Wait(); err != nil {
			t.Fatalf("task wait failed: %v", err)
		}
	}
	_ = runner.Stop()

	recs := readAllRecordsFromSegment(t, segPath)
	if len(recs) != numTasks {
		t.Fatalf("expected %d records, got %d", numTasks, len(recs))
	}

	for i, rec := range recs {
		expectedSeq := uint64(i + 1)
		if rec.SeqNum != binary.SeqNum(expectedSeq) {
			t.Errorf("record %d out of FIFO order: got seq %d, want %d", i, rec.SeqNum, expectedSeq)
		}
	}
}

// Test 4: Batch stops at 1,024 tasks.
func TestRunner_4_BatchStopsAt1024Tasks(t *testing.T) {
	q, _ := wal.NewWriteQueue(2048)
	mock := &mockBatchWriter{}
	runner, _ := wal.NewGroupCommitRunner(q, mock, wal.DefaultRunnerOptions())

	// Enqueue 1,050 tasks (all small, total bytes < 64 KiB)
	const totalTasks = 1050
	tasks := make([]*wal.WriteTask, totalTasks)
	for i := 0; i < totalTasks; i++ {
		tasks[i] = createTestTask(t, uint64(i+1), "k", "v")
		if err := q.Enqueue(tasks[i]); err != nil {
			t.Fatalf("Enqueue failed: %v", err)
		}
	}

	_ = runner.Start()
	for _, task := range tasks {
		if err := task.Wait(); err != nil {
			t.Fatalf("task wait failed: %v", err)
		}
	}
	_ = runner.Stop()

	// 1,050 tasks with 1,024 batch cap must result in exactly 2 batches and 2 syncs
	if runner.BatchesExecuted() != 2 {
		t.Errorf("BatchesExecuted mismatch: got %d, want 2", runner.BatchesExecuted())
	}
	if runner.SyncCount() != 2 {
		t.Errorf("SyncCount mismatch: got %d, want 2", runner.SyncCount())
	}
	if runner.TasksExecuted() != totalTasks {
		t.Errorf("TasksExecuted mismatch: got %d, want %d", runner.TasksExecuted(), totalTasks)
	}
}

// Test 5: Batch stops before encoded size exceeds 64 KiB.
func TestRunner_5_BatchStopsBeforeEncodedSizeExceeds64KiB(t *testing.T) {
	q, _ := wal.NewWriteQueue(16)
	mock := &mockBatchWriter{}
	runner, _ := wal.NewGroupCommitRunner(q, mock, wal.DefaultRunnerOptions())

	// 2 tasks of 40 KiB each. 40 KiB + 40 KiB = 80 KiB > 64 KiB.
	// Must split into 2 batches.
	val40k := string(make([]byte, 40*1024))
	t1 := createTestTask(t, 1, "k1", val40k)
	t2 := createTestTask(t, 2, "k2", val40k)

	_ = q.Enqueue(t1)
	_ = q.Enqueue(t2)

	_ = runner.Start()
	if err := t1.Wait(); err != nil {
		t.Fatalf("t1 wait failed: %v", err)
	}
	if err := t2.Wait(); err != nil {
		t.Fatalf("t2 wait failed: %v", err)
	}
	_ = runner.Stop()

	if runner.BatchesExecuted() != 2 {
		t.Errorf("BatchesExecuted mismatch: got %d, want 2", runner.BatchesExecuted())
	}
	if runner.SyncCount() != 2 {
		t.Errorf("SyncCount mismatch: got %d, want 2", runner.SyncCount())
	}
}

// Test 6: Exact 64 KiB boundary behavior.
func TestRunner_6_Exact64KiBBoundary(t *testing.T) {
	q, _ := wal.NewWriteQueue(16)
	mock := &mockBatchWriter{}
	runner, _ := wal.NewGroupCommitRunner(q, mock, wal.DefaultRunnerOptions())

	// Task 1: exactly 65,536 bytes wire size.
	// Wire size = MinRecordSize (27) + len(key) (2) + len(val) -> val = 65536 - 29 = 65507
	valExact := string(make([]byte, wal.MaxBatchBytes-wal.MinRecordSize-2))
	t1 := createTestTask(t, 1, "k1", valExact)
	if wal.RecordWireSize(t1.Record()) != wal.MaxBatchBytes {
		t.Fatalf("expected wire size %d, got %d", wal.MaxBatchBytes, wal.RecordWireSize(t1.Record()))
	}

	// Task 2: small task (100 bytes)
	t2 := createTestTask(t, 2, "k2", "small_payload")

	_ = q.Enqueue(t1)
	_ = q.Enqueue(t2)

	_ = runner.Start()
	_ = t1.Wait()
	_ = t2.Wait()
	_ = runner.Stop()

	// Task 1 reached exact 64 KiB boundary, so Task 2 must be in batch 2
	if runner.BatchesExecuted() != 2 {
		t.Errorf("BatchesExecuted mismatch: got %d, want 2", runner.BatchesExecuted())
	}
	if runner.SyncCount() != 2 {
		t.Errorf("SyncCount mismatch: got %d, want 2", runner.SyncCount())
	}
}

// Test 7: One-byte-over-64-KiB boundary behavior.
func TestRunner_7_OneByteOver64KiBBoundary(t *testing.T) {
	q, _ := wal.NewWriteQueue(16)
	mock := &mockBatchWriter{}
	runner, _ := wal.NewGroupCommitRunner(q, mock, wal.DefaultRunnerOptions())

	// Task 1: 32 KiB
	val32k := string(make([]byte, 32*1024-wal.MinRecordSize-2))
	t1 := createTestTask(t, 1, "k1", val32k) // Exactly 32 KiB wire size

	// Task 2: 32 KiB + 1 byte (32769 bytes wire size)
	val32kPlus1 := string(make([]byte, 32*1024-wal.MinRecordSize-2+1))
	t2 := createTestTask(t, 2, "k2", val32kPlus1)

	// Total = 65,537 bytes (1 byte over 64 KiB)
	_ = q.Enqueue(t1)
	_ = q.Enqueue(t2)

	_ = runner.Start()
	_ = t1.Wait()
	_ = t2.Wait()
	_ = runner.Stop()

	if runner.BatchesExecuted() != 2 {
		t.Errorf("BatchesExecuted mismatch: got %d, want 2", runner.BatchesExecuted())
	}
	if runner.SyncCount() != 2 {
		t.Errorf("SyncCount mismatch: got %d, want 2", runner.SyncCount())
	}
}

// Test 8: Oversized single record executes as one singleton batch.
func TestRunner_8_OversizedSingleRecordSingletonBatch(t *testing.T) {
	dbPath := t.TempDir()
	w, segPath := createRealWriter(t, dbPath)
	defer func() { _ = w.Close() }()

	q, _ := wal.NewWriteQueue(16)
	runner, _ := wal.NewGroupCommitRunner(q, w, wal.DefaultRunnerOptions())

	// Task 1: 100 KiB payload (exceeds 64 KiB batch limit, but valid WAL record)
	val100k := string(make([]byte, 100*1024))
	t1 := createTestTask(t, 1, "k_oversized", val100k)
	t2 := createTestTask(t, 2, "k_normal", "regular_val")

	_ = q.Enqueue(t1)
	_ = q.Enqueue(t2)

	_ = runner.Start()
	if err := t1.Wait(); err != nil {
		t.Fatalf("oversized task wait failed: %v", err)
	}
	if err := t2.Wait(); err != nil {
		t.Fatalf("normal task wait failed: %v", err)
	}
	_ = runner.Stop()

	// Proves oversized record executed as singleton batch, and normal record executed in next batch
	if runner.BatchesExecuted() != 2 {
		t.Errorf("BatchesExecuted mismatch: got %d, want 2", runner.BatchesExecuted())
	}
	if runner.SyncCount() != 2 {
		t.Errorf("SyncCount mismatch: got %d, want 2", runner.SyncCount())
	}

	recs := readAllRecordsFromSegment(t, segPath)
	if len(recs) != 2 {
		t.Fatalf("expected 2 records, got %d", len(recs))
	}
	if len(recs[0].Value) != 100*1024 {
		t.Errorf("oversized record value length mismatch: got %d, want %d", len(recs[0].Value), 100*1024)
	}
	if string(recs[1].Value) != "regular_val" {
		t.Errorf("regular record value mismatch: got %s", recs[1].Value)
	}
}

// Test 9: Two logical batches cause exactly two sync calls.
func TestRunner_9_TwoLogicalBatchesCauseTwoSyncCalls(t *testing.T) {
	q, _ := wal.NewWriteQueue(16)
	mock := &mockBatchWriter{}
	runner, _ := wal.NewGroupCommitRunner(q, mock, wal.DefaultRunnerOptions())
	_ = runner.Start()
	defer func() { _ = runner.Stop() }()

	// Batch 1
	t1 := createTestTask(t, 1, "k1", "v1")
	_ = q.Enqueue(t1)
	_ = t1.Wait()

	if runner.SyncCount() != 1 {
		t.Fatalf("expected 1 sync after batch 1, got %d", runner.SyncCount())
	}

	// Batch 2
	t2 := createTestTask(t, 2, "k2", "v2")
	_ = q.Enqueue(t2)
	_ = t2.Wait()

	if runner.SyncCount() != 2 {
		t.Fatalf("expected 2 syncs after batch 2, got %d", runner.SyncCount())
	}
}

// Test 10: 1,024 tasks in one batch cause exactly one sync call.
func TestRunner_10_1024TasksInOneBatchCauseOneSyncCall(t *testing.T) {
	q, _ := wal.NewWriteQueue(1024)
	mock := &mockBatchWriter{}
	runner, _ := wal.NewGroupCommitRunner(q, mock, wal.DefaultRunnerOptions())

	tasks := make([]*wal.WriteTask, 1024)
	for i := 0; i < 1024; i++ {
		tasks[i] = createTestTask(t, uint64(i+1), "k", "v")
		_ = q.Enqueue(tasks[i])
	}

	_ = runner.Start()
	for _, task := range tasks {
		if err := task.Wait(); err != nil {
			t.Fatalf("task wait failed: %v", err)
		}
	}
	_ = runner.Stop()

	if runner.BatchesExecuted() != 1 {
		t.Errorf("BatchesExecuted mismatch: got %d, want 1", runner.BatchesExecuted())
	}
	if runner.SyncCount() != 1 {
		t.Errorf("SyncCount mismatch: got %d, want 1", runner.SyncCount())
	}
	if runner.TasksExecuted() != 1024 {
		t.Errorf("TasksExecuted mismatch: got %d, want 1024", runner.TasksExecuted())
	}
}

// Test 11: Successful sync completes every task.
func TestRunner_11_SuccessfulSyncCompletesEveryTask(t *testing.T) {
	q, _ := wal.NewWriteQueue(50)
	mock := &mockBatchWriter{}
	runner, _ := wal.NewGroupCommitRunner(q, mock, wal.DefaultRunnerOptions())

	tasks := make([]*wal.WriteTask, 50)
	for i := 0; i < 50; i++ {
		tasks[i] = createTestTask(t, uint64(i+1), "k", "v")
		_ = q.Enqueue(tasks[i])
	}

	_ = runner.Start()
	for i, task := range tasks {
		if err := task.Wait(); err != nil {
			t.Errorf("task %d failed: %v", i, err)
		}
		if !task.IsDone() {
			t.Errorf("task %d IsDone is false", i)
		}
	}
	_ = runner.Stop()
}

// Test 12: Write failure fails all affected tasks.
func TestRunner_12_WriteFailureFailsAllAffectedTasks(t *testing.T) {
	q, _ := wal.NewWriteQueue(16)
	simulatedErr := stdErrors.New("simulated disk I/O write error")
	mock := &mockBatchWriter{
		appendErrFn: func(rec wal.Record, index int) error {
			if index == 2 {
				return simulatedErr
			}
			return nil
		},
	}
	runner, _ := wal.NewGroupCommitRunner(q, mock, wal.DefaultRunnerOptions())

	tasks := make([]*wal.WriteTask, 5)
	for i := 0; i < 5; i++ {
		tasks[i] = createTestTask(t, uint64(i+1), "k", "v")
		_ = q.Enqueue(tasks[i])
	}

	_ = runner.Start()

	for i, task := range tasks {
		err := task.Wait()
		if err == nil {
			t.Fatalf("task %d unexpectedly succeeded", i)
		}
		if !stdErrors.Is(err, simulatedErr) {
			t.Errorf("task %d error does not unwrap to simulatedErr: %v", i, err)
		}
		var appendErr *wal.BatchAppendError
		if !stdErrors.As(err, &appendErr) {
			t.Errorf("task %d error is not *wal.BatchAppendError: %v", i, err)
		}
	}
	_ = runner.Stop()

	// Durability barrier should NOT have been invoked
	if mock.SyncCalls() != 0 {
		t.Errorf("expected 0 sync calls after append failure, got %d", mock.SyncCalls())
	}
}

// Test 13: Sync failure fails every task in that batch.
func TestRunner_13_SyncFailureFailsEveryTaskInBatch(t *testing.T) {
	q, _ := wal.NewWriteQueue(16)
	simulatedSyncErr := stdErrors.New("simulated fdatasync hardware failure")
	mock := &mockBatchWriter{
		syncErrFn: func(callCount int) error {
			return simulatedSyncErr
		},
	}
	runner, _ := wal.NewGroupCommitRunner(q, mock, wal.DefaultRunnerOptions())

	tasks := make([]*wal.WriteTask, 5)
	for i := 0; i < 5; i++ {
		tasks[i] = createTestTask(t, uint64(i+1), "k", "v")
		_ = q.Enqueue(tasks[i])
	}

	_ = runner.Start()

	for i, task := range tasks {
		err := task.Wait()
		if err == nil {
			t.Fatalf("task %d unexpectedly succeeded after sync failure", i)
		}
		if !stdErrors.Is(err, simulatedSyncErr) {
			t.Errorf("task %d error does not unwrap to simulatedSyncErr: %v", i, err)
		}
		var syncErr *wal.BatchSyncError
		if !stdErrors.As(err, &syncErr) {
			t.Errorf("task %d error is not *wal.BatchSyncError: %v", i, err)
		}
	}
	_ = runner.Stop()

	if mock.SyncCalls() != 1 {
		t.Errorf("expected exactly 1 sync call, got %d", mock.SyncCalls())
	}
}

// Test 14: Later batch can proceed independently after earlier successful batch.
func TestRunner_14_LaterBatchProceedsIndependentlyAfterEarlierBatch(t *testing.T) {
	dbPath := t.TempDir()
	w, segPath := createRealWriter(t, dbPath)
	defer func() { _ = w.Close() }()

	q, _ := wal.NewWriteQueue(16)
	runner, _ := wal.NewGroupCommitRunner(q, w, wal.DefaultRunnerOptions())
	_ = runner.Start()
	defer func() { _ = runner.Stop() }()

	// Batch 1
	t1 := createTestTask(t, 1, "k1", "v1")
	_ = q.Enqueue(t1)
	if err := t1.Wait(); err != nil {
		t.Fatalf("batch 1 failed: %v", err)
	}

	// Batch 2
	t2 := createTestTask(t, 2, "k2", "v2")
	_ = q.Enqueue(t2)
	if err := t2.Wait(); err != nil {
		t.Fatalf("batch 2 failed: %v", err)
	}

	// Batch 3
	t3 := createTestTask(t, 3, "k3", "v3")
	_ = q.Enqueue(t3)
	if err := t3.Wait(); err != nil {
		t.Fatalf("batch 3 failed: %v", err)
	}

	_ = runner.Stop()

	if runner.SyncCount() != 3 {
		t.Errorf("expected 3 syncs, got %d", runner.SyncCount())
	}
	recs := readAllRecordsFromSegment(t, segPath)
	if len(recs) != 3 {
		t.Fatalf("expected 3 records, got %d", len(recs))
	}
}

// Test 15: Queue FIFO remains correct with multiple producers.
func TestRunner_15_QueueFIFORemainsCorrectWithMultipleProducers(t *testing.T) {
	q, _ := wal.NewWriteQueue(256)
	mock := &mockBatchWriter{}
	runner, _ := wal.NewGroupCommitRunner(q, mock, wal.DefaultRunnerOptions())
	_ = runner.Start()
	defer func() { _ = runner.Stop() }()

	const producers = 4
	const tasksPerProducer = 25
	var wg sync.WaitGroup

	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(prodID int) {
			defer wg.Done()
			for i := 0; i < tasksPerProducer; i++ {
				seq := uint64(prodID*tasksPerProducer + i + 1)
				task := createTestTask(t, seq, fmt.Sprintf("p%d_k%d", prodID, i), "val")
				if err := q.Enqueue(task); err != nil {
					t.Errorf("Enqueue failed: %v", err)
					return
				}
				if err := task.Wait(); err != nil {
					t.Errorf("task wait failed: %v", err)
					return
				}
			}
		}(p)
	}

	wg.Wait()
	_ = runner.Stop()

	if runner.TasksExecuted() != producers*tasksPerProducer {
		t.Errorf("TasksExecuted mismatch: got %d, want %d", runner.TasksExecuted(), producers*tasksPerProducer)
	}
}

// Test 16: Concurrent producers + single runner under race detector.
func TestRunner_16_ConcurrentProducersAndSingleRunner(t *testing.T) {
	dbPath := t.TempDir()
	w, segPath := createRealWriter(t, dbPath)
	defer func() { _ = w.Close() }()

	q, _ := wal.NewWriteQueue(128)
	runner, _ := wal.NewGroupCommitRunner(q, w, wal.DefaultRunnerOptions())
	_ = runner.Start()
	defer func() { _ = runner.Stop() }()

	const numProducers = 8
	const numTasksPerProd = 50
	const totalTasks = numProducers * numTasksPerProd

	var wg sync.WaitGroup
	for p := 0; p < numProducers; p++ {
		wg.Add(1)
		go func(prodID int) {
			defer wg.Done()
			for i := 0; i < numTasksPerProd; i++ {
				seq := uint64(prodID*numTasksPerProd + i + 1)
				task := createTestTask(t, seq, fmt.Sprintf("prod_%d_key_%03d", prodID, i), fmt.Sprintf("val_%d_%d", prodID, i))
				if err := q.Enqueue(task); err != nil {
					t.Errorf("Enqueue failed: %v", err)
					return
				}
				if err := task.Wait(); err != nil {
					t.Errorf("Wait failed: %v", err)
					return
				}
			}
		}(p)
	}

	wg.Wait()
	_ = runner.Stop()

	if runner.TasksExecuted() != totalTasks {
		t.Errorf("TasksExecuted mismatch: got %d, want %d", runner.TasksExecuted(), totalTasks)
	}

	// Physical verification: read all records
	recs := readAllRecordsFromSegment(t, segPath)
	if len(recs) != totalTasks {
		t.Fatalf("expected %d records in segment, got %d", totalTasks, len(recs))
	}

	// Verify no duplicates
	seen := make(map[string]bool)
	for _, rec := range recs {
		k := string(rec.Key)
		if seen[k] {
			t.Fatalf("duplicate record key encountered: %s", k)
		}
		seen[k] = true
	}
}

// Test 17: Runner shutdown with empty queue.
func TestRunner_17_ShutdownWithEmptyQueue(t *testing.T) {
	q, _ := wal.NewWriteQueue(16)
	mock := &mockBatchWriter{}
	runner, _ := wal.NewGroupCommitRunner(q, mock, wal.DefaultRunnerOptions())

	if err := runner.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if !runner.IsRunning() {
		t.Errorf("expected IsRunning == true")
	}

	if err := runner.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if runner.IsRunning() {
		t.Errorf("expected IsRunning == false after Stop")
	}

	// Repeated Stop is idempotent
	if err := runner.Stop(); err != nil {
		t.Errorf("repeated Stop failed: %v", err)
	}

	// Start after Stop returns ErrRunnerClosed
	if err := runner.Start(); !stdErrors.Is(err, errors.ErrRunnerClosed) {
		t.Errorf("expected ErrRunnerClosed, got %v", err)
	}
}

// Test 18: Runner shutdown with queued work (graceful drain).
func TestRunner_18_ShutdownWithQueuedWork(t *testing.T) {
	q, _ := wal.NewWriteQueue(50)
	mock := &mockBatchWriter{}
	runner, _ := wal.NewGroupCommitRunner(q, mock, wal.DefaultRunnerOptions())

	tasks := make([]*wal.WriteTask, 30)
	for i := 0; i < 30; i++ {
		tasks[i] = createTestTask(t, uint64(i+1), "k", "v")
		_ = q.Enqueue(tasks[i])
	}

	_ = runner.Start()
	// Immediately stop runner
	_ = runner.Stop()

	// Every queued task must have been drained and completed
	for i, task := range tasks {
		if err := task.Wait(); err != nil {
			t.Errorf("task %d failed during graceful drain: %v", i, err)
		}
		if !task.IsDone() {
			t.Errorf("task %d was not completed during shutdown", i)
		}
	}

	if runner.TasksExecuted() != 30 {
		t.Errorf("TasksExecuted mismatch: got %d, want 30", runner.TasksExecuted())
	}
}

// Test 19: Shutdown while a batch is executing.
func TestRunner_19_ShutdownWhileBatchIsExecuting(t *testing.T) {
	inSync := make(chan struct{})
	releaseSync := make(chan struct{})

	mock := &mockBatchWriter{
		syncCallback: func() {
			close(inSync)
			<-releaseSync
		},
	}

	q, _ := wal.NewWriteQueue(16)
	runner, _ := wal.NewGroupCommitRunner(q, mock, wal.DefaultRunnerOptions())
	_ = runner.Start()

	task := createTestTask(t, 1, "k1", "v1")
	_ = q.Enqueue(task)

	// Wait until runner is inside Sync()
	<-inSync

	// Call Stop in background while runner is in sync
	stopDone := make(chan struct{})
	go func() {
		_ = runner.Stop()
		close(stopDone)
	}()

	// Release sync barrier
	close(releaseSync)
	<-stopDone

	if err := task.Wait(); err != nil {
		t.Fatalf("task wait failed: %v", err)
	}
	if !task.IsDone() {
		t.Errorf("task not completed")
	}
}

// Test 20: No permanently blocked task waiter after failure or shutdown.
func TestRunner_20_NoPermanentlyBlockedTaskWaiter(t *testing.T) {
	q, _ := wal.NewWriteQueue(16)
	task := createTestTask(t, 1, "k", "v")
	_ = q.Enqueue(task)

	shutdownErr := stdErrors.New("abrupt system shutdown")
	_ = q.CloseWithError(shutdownErr)

	// Wait must return immediately with shutdownErr
	err := task.Wait()
	if !stdErrors.Is(err, shutdownErr) {
		t.Errorf("expected shutdownErr, got %v", err)
	}
}

// Test 21: Task waiter context timeout does not cancel actual execution.
func TestRunner_21_TaskWaiterContextTimeoutDoesNotCancelExecution(t *testing.T) {
	inSync := make(chan struct{})
	releaseSync := make(chan struct{})

	mock := &mockBatchWriter{
		syncCallback: func() {
			select {
			case <-inSync:
			default:
				close(inSync)
			}
			<-releaseSync
		},
	}

	q, _ := wal.NewWriteQueue(16)
	runner, _ := wal.NewGroupCommitRunner(q, mock, wal.DefaultRunnerOptions())
	_ = runner.Start()
	defer func() { _ = runner.Stop() }()

	task := createTestTask(t, 1, "k", "v")
	_ = q.Enqueue(task)

	<-inSync

	// Caller context times out
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	waitErr := task.WaitContext(ctx)
	if !stdErrors.Is(waitErr, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got %v", waitErr)
	}

	// Release runner sync
	close(releaseSync)

	// The actual task execution must still succeed and complete with nil
	if err := task.Wait(); err != nil {
		t.Fatalf("task should have completed with nil, got: %v", err)
	}
}

// Test 22: No task completes twice.
func TestRunner_22_NoTaskCompletesTwice(t *testing.T) {
	task := createTestTask(t, 1, "k", "v")
	if err := task.Complete(nil); err != nil {
		t.Fatalf("first complete failed: %v", err)
	}
	if err := task.Complete(nil); !stdErrors.Is(err, errors.ErrTaskAlreadyCompleted) {
		t.Errorf("second complete must return ErrTaskAlreadyCompleted, got: %v", err)
	}
}

// Test 23: No duplicate physical record emission.
func TestRunner_23_NoDuplicatePhysicalRecordEmission(t *testing.T) {
	dbPath := t.TempDir()
	w, segPath := createRealWriter(t, dbPath)
	defer func() { _ = w.Close() }()

	q, _ := wal.NewWriteQueue(100)
	runner, _ := wal.NewGroupCommitRunner(q, w, wal.DefaultRunnerOptions())
	_ = runner.Start()

	const N = 50
	tasks := make([]*wal.WriteTask, N)
	for i := 0; i < N; i++ {
		tasks[i] = createTestTask(t, uint64(i+1), fmt.Sprintf("k%d", i), "val")
		_ = q.Enqueue(tasks[i])
	}

	for _, task := range tasks {
		_ = task.Wait()
	}
	_ = runner.Stop()

	recs := readAllRecordsFromSegment(t, segPath)
	if len(recs) != N {
		t.Fatalf("expected exactly %d records, got %d", N, len(recs))
	}
	for i, rec := range recs {
		if rec.SeqNum != binary.SeqNum(uint64(i+1)) {
			t.Errorf("record %d seq mismatch: got %d, want %d", i, rec.SeqNum, i+1)
		}
	}
}

// Test 24: No record is lost during batch-boundary transitions.
func TestRunner_24_NoRecordLostDuringBatchBoundaryTransitions(t *testing.T) {
	dbPath := t.TempDir()
	w, segPath := createRealWriter(t, dbPath)
	defer func() { _ = w.Close() }()

	// Configure runner with tiny batch limits to force frequent boundary transitions
	q, _ := wal.NewWriteQueue(100)
	opts := wal.RunnerOptions{
		MaxBatchTasks: 5,
		MaxBatchBytes: 1024,
	}
	runner, _ := wal.NewGroupCommitRunner(q, w, opts)
	_ = runner.Start()

	const totalTasks = 37
	tasks := make([]*wal.WriteTask, totalTasks)
	for i := 0; i < totalTasks; i++ {
		tasks[i] = createTestTask(t, uint64(i+1), fmt.Sprintf("boundary_key_%03d", i), "val")
		_ = q.Enqueue(tasks[i])
	}

	for _, task := range tasks {
		if err := task.Wait(); err != nil {
			t.Fatalf("task wait failed: %v", err)
		}
	}
	_ = runner.Stop()

	recs := readAllRecordsFromSegment(t, segPath)
	if len(recs) != totalTasks {
		t.Fatalf("expected %d records, got %d (records lost during boundary transitions)", totalTasks, len(recs))
	}
	for i, rec := range recs {
		expectedKey := fmt.Sprintf("boundary_key_%03d", i)
		if string(rec.Key) != expectedKey {
			t.Errorf("record %d key mismatch: got %s, want %s", i, rec.Key, expectedKey)
		}
	}
}

// Test 25: Segment rotation interaction with a batch.
func TestRunner_25_SegmentRotationInteractionWithBatch(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	// Rotating writer with 1 KiB segment size
	rw, err := wal.OpenRotatingWriter(dbPath, wal.Options{SegmentSize: 1024})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	q, _ := wal.NewWriteQueue(100)
	runner, _ := wal.NewGroupCommitRunner(q, rw, wal.DefaultRunnerOptions())
	_ = runner.Start()

	// Enqueue tasks whose total size will trigger at least one rotation
	const N = 20
	tasks := make([]*wal.WriteTask, N)
	for i := 0; i < N; i++ {
		// Each record is ~100 bytes; 20 records = ~2000 bytes > 1024 segment size
		tasks[i] = createTestTask(t, uint64(i+1), fmt.Sprintf("rot_key_%03d", i), string(make([]byte, 80)))
		_ = q.Enqueue(tasks[i])
	}

	for _, task := range tasks {
		if err := task.Wait(); err != nil {
			t.Fatalf("task wait failed: %v", err)
		}
	}
	_ = runner.Stop()

	// Verify segment rotation occurred
	if rw.ActiveSegmentID() < 2 {
		t.Errorf("expected rotation to segment 2 or higher, active is %d", rw.ActiveSegmentID())
	}

	// Verify all records across all segments using recovery coordinator
	var recovered []wal.Record
	sink := wal.ReplayFunc(func(rec wal.Record) error {
		recovered = append(recovered, rec)
		return nil
	})
	report, err := wal.RecoverWAL(dbPath, sink)
	if err != nil {
		t.Fatalf("RecoverWAL failed: %v", err)
	}
	if report.ValidRecords != N {
		t.Errorf("expected %d valid records in report, got %d", N, report.ValidRecords)
	}
	if len(recovered) != N {
		t.Fatalf("expected %d total recovered records across rotated segments, got %d", N, len(recovered))
	}
	for i, rec := range recovered {
		if rec.SeqNum != binary.SeqNum(uint64(i+1)) {
			t.Errorf("recovered record %d seq mismatch: got %d, want %d", i, rec.SeqNum, i+1)
		}
	}
}

// Test 26: Sync count verification test seam.
func TestRunner_26_SyncCountVerificationSeam(t *testing.T) {
	q, _ := wal.NewWriteQueue(16)
	mock := &mockBatchWriter{}
	runner, _ := wal.NewGroupCommitRunner(q, mock, wal.DefaultRunnerOptions())
	_ = runner.Start()
	defer func() { _ = runner.Stop() }()

	// 1 task per batch x 3
	for i := 0; i < 3; i++ {
		task := createTestTask(t, uint64(i+1), "k", "v")
		_ = q.Enqueue(task)
		_ = task.Wait()
	}

	if runner.SyncCount() != 3 {
		t.Errorf("expected 3 syncs for 3 distinct batches, got %d", runner.SyncCount())
	}
	if mock.SyncCalls() != 3 {
		t.Errorf("expected mock 3 sync calls, got %d", mock.SyncCalls())
	}
}
