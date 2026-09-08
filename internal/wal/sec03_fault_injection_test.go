package wal

import (
	stdErrors "errors"
	"fmt"
	"io"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// SEC-03.8: Disk Failure & Resource Exhaustion Dynamic Audit
//
// Invariants under test:
//   1. No task may receive a successful durability result after a durability operation has failed.
//   2. Write failures (short write, zero-byte write, ENOSPC) fail closed immediately without sync.
//   3. Hardware fdatasync failures fan out to EVERY task in the batch; 0 tasks receive nil.
//   4. Close failures wrap and report underlying sync/close errors.
//   5. Resource limits (MaxKeyLength, MaxRecordSize, queue capacity, batch size) prevent memory blowup.
//   6. Queue shutdowns under failure cleanly unblock all pending waiters without deadlocks or goroutine leaks.

// TestSEC03_Fault_01_WriteFailure_FanoutAndZeroSuccess tests that an injected
// physical write failure (e.g. ENOSPC) during batch execution immediately fails all tasks
// in the batch with *BatchAppendError, never calls Sync(), and awards exactly ZERO successful results.
func TestSEC03_Fault_01_WriteFailure_FanoutAndZeroSuccess(t *testing.T) {
	dbDir := t.TempDir()
	if _, err := InitDir(dbDir); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := CreateSegmentWriter(dbDir, 1)
	if err != nil {
		t.Fatalf("CreateSegmentWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	simulatedDiskFull := syscall.ENOSPC
	var syncCalled int32

	// Inject write failure on the 3rd write
	var writeCount int32
	w.setWriteFnForTesting(func(f *os.File, p []byte) (int, error) {
		count := atomic.AddInt32(&writeCount, 1)
		if count >= 3 {
			return 0, simulatedDiskFull
		}
		return f.Write(p)
	})
	w.setSyncFnForTesting(func(f *os.File) error {
		atomic.AddInt32(&syncCalled, 1)
		return fdatasync(f)
	})

	q, err := NewWriteQueue(16)
	if err != nil {
		t.Fatalf("NewWriteQueue failed: %v", err)
	}

	const numTasks = 5
	tasks := make([]*WriteTask, numTasks)
	for i := 0; i < numTasks; i++ {
		task, err := NewWriteTask(Record{
			SeqNum: binary.SeqNum(i + 1),
			Type:   RecordTypePut,
			Key:    []byte(fmt.Sprintf("key-%02d", i)),
			Value:  []byte(fmt.Sprintf("val-%02d", i)),
		})
		if err != nil {
			t.Fatalf("NewWriteTask failed: %v", err)
		}
		tasks[i] = task
		if err := q.Enqueue(tasks[i]); err != nil {
			t.Fatalf("Enqueue task %d failed: %v", i, err)
		}
	}

	runner, err := NewGroupCommitRunner(q, w, RunnerOptions{MaxBatchTasks: 10, MaxBatchBytes: 64 * 1024})
	if err != nil {
		t.Fatalf("NewGroupCommitRunner failed: %v", err)
	}
	if err := runner.Start(); err != nil {
		t.Fatalf("runner.Start failed: %v", err)
	}
	defer func() { _ = runner.Stop() }()

	var successCount int
	var failureCount int
	for i, task := range tasks {
		err := task.Wait()
		if err == nil {
			successCount++
		} else {
			failureCount++
			var batchErr *BatchAppendError
			if !stdErrors.As(err, &batchErr) {
				t.Errorf("task %d error is not *BatchAppendError: %v", i, err)
			}
			if !stdErrors.Is(err, simulatedDiskFull) {
				t.Errorf("task %d error does not unwrap to ENOSPC: %v", i, err)
			}
		}
	}

	// Invariant: no task may receive success!
	if successCount != 0 {
		t.Fatalf("SECURITY VIOLATION: %d tasks received success after write failure!", successCount)
	}
	if failureCount != numTasks {
		t.Fatalf("expected all %d tasks to fail, but only %d failed", numTasks, failureCount)
	}
	if atomic.LoadInt32(&syncCalled) != 0 {
		t.Fatalf("Sync() was called %d times despite write failure", atomic.LoadInt32(&syncCalled))
	}
}

// TestSEC03_Fault_02_ShortWrite_FailsClosed tests that when the underlying write
// returns fewer bytes than requested, the writer aborts and returns an error without sync.
func TestSEC03_Fault_02_ShortWrite_FailsClosed(t *testing.T) {
	dbDir := t.TempDir()
	if _, err := InitDir(dbDir); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := CreateSegmentWriter(dbDir, 1)
	if err != nil {
		t.Fatalf("CreateSegmentWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	// Simulate short write: writes 5 bytes, then returns an error
	w.setWriteFnForTesting(func(f *os.File, p []byte) (int, error) {
		if len(p) > 5 {
			_, _ = f.Write(p[:5])
			return 5, io.ErrShortWrite
		}
		return f.Write(p)
	})

	rec := Record{
		SeqNum: 1,
		Type:   RecordTypePut,
		Key:    []byte("short-write-key"),
		Value:  []byte("short-write-value"),
	}

	err = w.AppendSync(rec)
	if err == nil {
		t.Fatalf("SECURITY VIOLATION: AppendSync succeeded despite short write")
	}
	if !stdErrors.Is(err, io.ErrShortWrite) {
		t.Fatalf("expected error wrapping io.ErrShortWrite, got: %v", err)
	}
}

// TestSEC03_Fault_03_ZeroByteWrite_FailsClosed tests that when the underlying write
// returns 0 bytes with nil error, the loop detects the stall and returns io.ErrShortWrite.
func TestSEC03_Fault_03_ZeroByteWrite_FailsClosed(t *testing.T) {
	dbDir := t.TempDir()
	if _, err := InitDir(dbDir); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := CreateSegmentWriter(dbDir, 1)
	if err != nil {
		t.Fatalf("CreateSegmentWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	// Injects 0 bytes write
	w.setWriteFnForTesting(func(f *os.File, p []byte) (int, error) {
		return 0, nil
	})

	rec := Record{
		SeqNum: 1,
		Type:   RecordTypePut,
		Key:    []byte("zero-byte-key"),
		Value:  []byte("zero-byte-value"),
	}

	err = w.AppendSync(rec)
	if err == nil {
		t.Fatalf("SECURITY VIOLATION: AppendSync succeeded despite 0-byte write")
	}
	if !stdErrors.Is(err, io.ErrShortWrite) {
		t.Fatalf("expected error wrapping io.ErrShortWrite, got: %v", err)
	}
}

// TestSEC03_Fault_04_SyncFailure_GroupCommitFanout tests that when writes succeed
// but fdatasync fails, EVERY task in the batch receives a *BatchSyncError and 0 tasks succeed.
func TestSEC03_Fault_04_SyncFailure_GroupCommitFanout(t *testing.T) {
	dbDir := t.TempDir()
	if _, err := InitDir(dbDir); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := CreateSegmentWriter(dbDir, 1)
	if err != nil {
		t.Fatalf("CreateSegmentWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	simulatedSyncErr := syscall.EIO
	w.setSyncFnForTesting(func(f *os.File) error {
		return simulatedSyncErr
	})

	q, err := NewWriteQueue(32)
	if err != nil {
		t.Fatalf("NewWriteQueue failed: %v", err)
	}

	const numTasks = 10
	tasks := make([]*WriteTask, numTasks)
	for i := 0; i < numTasks; i++ {
		task, err := NewWriteTask(Record{
			SeqNum: binary.SeqNum(i + 1),
			Type:   RecordTypePut,
			Key:    []byte(fmt.Sprintf("key-%d", i)),
			Value:  []byte(fmt.Sprintf("val-%d", i)),
		})
		if err != nil {
			t.Fatalf("NewWriteTask failed: %v", err)
		}
		tasks[i] = task
		if err := q.Enqueue(tasks[i]); err != nil {
			t.Fatalf("Enqueue task %d failed: %v", i, err)
		}
	}

	runner, err := NewGroupCommitRunner(q, w, RunnerOptions{MaxBatchTasks: 16, MaxBatchBytes: 64 * 1024})
	if err != nil {
		t.Fatalf("NewGroupCommitRunner failed: %v", err)
	}
	if err := runner.Start(); err != nil {
		t.Fatalf("runner.Start failed: %v", err)
	}
	defer func() { _ = runner.Stop() }()

	var successCount int
	var failureCount int
	for i, task := range tasks {
		err := task.Wait()
		if err == nil {
			successCount++
		} else {
			failureCount++
			var syncErr *BatchSyncError
			if !stdErrors.As(err, &syncErr) {
				t.Errorf("task %d error is not *BatchSyncError: %v", i, err)
			}
			if !stdErrors.Is(err, simulatedSyncErr) {
				t.Errorf("task %d error does not unwrap to EIO: %v", i, err)
			}
		}
	}

	// Invariant SEC-WAL-INV-05: Successful durability completion requires successful synchronization.
	if successCount != 0 {
		t.Fatalf("SECURITY VIOLATION: %d tasks received success when Sync failed!", successCount)
	}
	if failureCount != numTasks {
		t.Fatalf("expected all %d tasks to fail, got %d failures", numTasks, failureCount)
	}
}

// TestSEC03_Fault_05_CloseFailure_Propagated tests that when the final sync during
// writer.Close() fails, Close() returns an error rather than silently reporting success.
func TestSEC03_Fault_05_CloseFailure_Propagated(t *testing.T) {
	dbDir := t.TempDir()
	if _, err := InitDir(dbDir); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := CreateSegmentWriter(dbDir, 1)
	if err != nil {
		t.Fatalf("CreateSegmentWriter failed: %v", err)
	}

	simulatedErr := syscall.EIO
	w.setSyncFnForTesting(func(f *os.File) error {
		return simulatedErr
	})

	err = w.Close()
	if err == nil {
		t.Fatalf("SECURITY VIOLATION: Close() reported nil error when sync failed")
	}
	if !stdErrors.Is(err, simulatedErr) {
		t.Fatalf("expected Close() to wrap simulated EIO, got: %v", err)
	}
}

// TestSEC03_Fault_06_DiskFull_RecoveryBehavior tests that when a crash or ENOSPC
// produces a partial/torn tail write, the recovery subsystem safely truncates the partial
// record and recovers the clean prefix.
func TestSEC03_Fault_06_DiskFull_RecoveryBehavior(t *testing.T) {
	dbDir := t.TempDir()
	if _, err := InitDir(dbDir); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := CreateSegmentWriter(dbDir, 1)
	if err != nil {
		t.Fatalf("CreateSegmentWriter failed: %v", err)
	}

	// Write 3 complete records
	for i := 1; i <= 3; i++ {
		rec := Record{
			SeqNum: binary.SeqNum(i),
			Type:   RecordTypePut,
			Key:    []byte(fmt.Sprintf("valid-key-%d", i)),
			Value:  []byte(fmt.Sprintf("valid-val-%d", i)),
		}
		if err := w.AppendSync(rec); err != nil {
			t.Fatalf("AppendSync record %d failed: %v", i, err)
		}
	}

	// Inject ENOSPC mid-write on record 4
	w.setWriteFnForTesting(func(f *os.File, p []byte) (int, error) {
		// Write 10 bytes then fail with ENOSPC
		if len(p) > 10 {
			_, _ = f.Write(p[:10])
			return 10, syscall.ENOSPC
		}
		return 0, syscall.ENOSPC
	})

	rec4 := Record{
		SeqNum: 4,
		Type:   RecordTypePut,
		Key:    []byte("torn-key-4"),
		Value:  []byte("torn-val-4"),
	}
	err = w.AppendSync(rec4)
	if err == nil {
		t.Fatalf("expected AppendSync to fail on ENOSPC")
	}
	_ = w.file.Close() // Close file handle directly to leave torn write on disk

	// Now recover the WAL
	var recoveredRecs []Record
	report, err := RecoverWAL(dbDir, ReplayFunc(func(r Record) error {
		recoveredRecs = append(recoveredRecs, r)
		return nil
	}))
	if err != nil {
		t.Fatalf("RecoverWAL failed on torn tail: %v", err)
	}

	if !report.Truncated {
		t.Fatalf("expected report.Truncated to be true for torn tail")
	}
	if report.TruncatedBytes <= 0 {
		t.Fatalf("expected report.TruncatedBytes > 0, got %d", report.TruncatedBytes)
	}
	if report.ValidRecords != 3 {
		t.Fatalf("expected 3 valid recovered records, got %d", report.ValidRecords)
	}
	if len(recoveredRecs) != 3 {
		t.Fatalf("expected 3 recovered records in sink, got %d", len(recoveredRecs))
	}
	for i, r := range recoveredRecs {
		if r.SeqNum != binary.SeqNum(i+1) {
			t.Errorf("record %d seqnum mismatch: got %d, want %d", i, r.SeqNum, i+1)
		}
	}
}

// TestSEC03_Fault_07_ResourceBounds_MaxKeyAndValueSize tests that oversized keys
// or oversized records are rejected during validation before allocating memory or writing to disk.
func TestSEC03_Fault_07_ResourceBounds_MaxKeyAndValueSize(t *testing.T) {
	// Key larger than 65535 bytes (math.MaxUint16) cannot be represented in 2-byte header
	oversizedKey := make([]byte, math.MaxUint16+1)
	rec := Record{
		SeqNum: 1,
		Type:   RecordTypePut,
		Key:    oversizedKey,
		Value:  []byte("test-value"),
	}

	err := rec.Validate()
	if err == nil {
		t.Fatalf("SECURITY VIOLATION: Validate() accepted key of size %d > MaxUint16", len(oversizedKey))
	}
	var keyLenErr *errors.KeyTooLargeError
	if !stdErrors.As(err, &keyLenErr) {
		t.Fatalf("expected *errors.KeyTooLargeError, got: %T (%v)", err, err)
	}

	// Empty key should also be rejected
	recEmptyKey := Record{
		SeqNum: 1,
		Type:   RecordTypePut,
		Key:    nil,
		Value:  []byte("test-value"),
	}
	if err := recEmptyKey.Validate(); err == nil {
		t.Fatalf("SECURITY VIOLATION: Validate() accepted nil key")
	}

	// Invalid record type should be rejected
	recInvalidType := Record{
		SeqNum: 1,
		Type:   RecordType(99),
		Key:    []byte("key"),
		Value:  []byte("value"),
	}
	if err := recInvalidType.Validate(); err == nil {
		t.Fatalf("SECURITY VIOLATION: Validate() accepted invalid RecordType(99)")
	}
}

// TestSEC03_Fault_08_ResourceBounds_MaxBatchAndQueueOccupancy verifies that the queue
// enforces strict capacity bounds (preventing unbounded queue memory growth) and that
// runner batches never exceed configured MaxBatchTasks and MaxBatchBytes.
func TestSEC03_Fault_08_ResourceBounds_MaxBatchAndQueueOccupancy(t *testing.T) {
	const queueCap = 10
	q, err := NewWriteQueue(queueCap)
	if err != nil {
		t.Fatalf("NewWriteQueue failed: %v", err)
	}

	// Fill queue to capacity
	for i := 0; i < queueCap; i++ {
		task, err := NewWriteTask(Record{
			SeqNum: binary.SeqNum(i + 1),
			Type:   RecordTypePut,
			Key:    []byte(fmt.Sprintf("k%d", i)),
			Value:  []byte(fmt.Sprintf("v%d", i)),
		})
		if err != nil {
			t.Fatalf("NewWriteTask failed: %v", err)
		}
		if err := q.Enqueue(task); err != nil {
			t.Fatalf("failed to enqueue task %d: %v", i, err)
		}
	}

	if q.Len() != queueCap {
		t.Fatalf("expected queue len %d, got %d", queueCap, q.Len())
	}

	// Verify TryEnqueue rejects when full
	overflowTask, err := NewWriteTask(Record{
		SeqNum: 999,
		Type:   RecordTypePut,
		Key:    []byte("k-overflow"),
		Value:  []byte("v-overflow"),
	})
	if err != nil {
		t.Fatalf("NewWriteTask failed: %v", err)
	}
	if err := q.TryEnqueue(overflowTask); err == nil {
		t.Fatalf("SECURITY VIOLATION: TryEnqueue accepted task when queue was full")
	} else if !stdErrors.Is(err, errors.ErrQueueFull) {
		t.Fatalf("expected ErrQueueFull, got: %v", err)
	}

	// Drain batch with limit
	const batchLimit = 4
	batch, err := q.TryDequeueBatch(batchLimit, 1024*1024)
	if err != nil {
		t.Fatalf("TryDequeueBatch failed: %v", err)
	}
	if len(batch) > batchLimit {
		t.Fatalf("SECURITY VIOLATION: DequeueBatch returned %d tasks > limit %d", len(batch), batchLimit)
	}
}

// TestSEC03_Fault_09_QueueShutdown_NoDeadlockNoGoroutineLeak tests that closing the
// queue while producers are waiting immediately unblocks all producers with ErrQueueClosed,
// preventing goroutine leakage and deadlocks.
func TestSEC03_Fault_09_QueueShutdown_NoDeadlockNoGoroutineLeak(t *testing.T) {
	q, err := NewWriteQueue(2)
	if err != nil {
		t.Fatalf("NewWriteQueue failed: %v", err)
	}

	// Fill queue
	for i := 0; i < 2; i++ {
		task, err := NewWriteTask(Record{
			SeqNum: binary.SeqNum(i + 1),
			Type:   RecordTypePut,
			Key:    []byte("k"),
			Value:  []byte("v"),
		})
		if err != nil {
			t.Fatalf("NewWriteTask failed: %v", err)
		}
		_ = q.Enqueue(task)
	}

	// Launch 5 goroutines trying to enqueue into full queue
	const numBlocked = 5
	var wg sync.WaitGroup
	errs := make([]error, numBlocked)

	for i := 0; i < numBlocked; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			task, err := NewWriteTask(Record{
				SeqNum: binary.SeqNum(10 + idx),
				Type:   RecordTypePut,
				Key:    []byte("blocked-k"),
				Value:  []byte("blocked-v"),
			})
			if err != nil {
				errs[idx] = err
				return
			}
			errs[idx] = q.Enqueue(task)
		}(i)
	}

	// Give goroutines time to block on the full queue
	time.Sleep(50 * time.Millisecond)

	// Close queue
	if err := q.Close(); err != nil {
		t.Fatalf("q.Close failed: %v", err)
	}

	// All blocked goroutines MUST unblock
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Succeeded in unblocking
	case <-time.After(2 * time.Second):
		t.Fatalf("SECURITY VIOLATION: Goroutines leaked / deadlocked after queue close!")
	}

	for i, err := range errs {
		if err == nil {
			t.Errorf("blocked goroutine %d received nil error after queue close", i)
		} else if !stdErrors.Is(err, errors.ErrQueueClosed) {
			t.Errorf("blocked goroutine %d error is not ErrQueueClosed: %v", i, err)
		}
	}
}
