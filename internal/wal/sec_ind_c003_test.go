package wal_test

import (
	stdErrors "errors"
	"os"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/wal"
)

// TestINDC003_SyncFailureAcknowledgesZeroWriters proves the IND-C-003 verdict:
// when the durability barrier fails mid-batch (e.g. ENOSPC from a filled disk),
// the group-commit leader must propagate the failure to EVERY waiter. Zero
// tasks may observe success; each error must unwrap to the root cause via
// *wal.BatchSyncError.
func TestINDC003_SyncFailureAcknowledgesZeroWriters(t *testing.T) {
	q, err := wal.NewWriteQueue(32)
	if err != nil {
		t.Fatalf("NewWriteQueue failed: %v", err)
	}
	diskFull := stdErrors.New("simulated ENOSPC on fdatasync")
	mock := &mockBatchWriter{
		syncErrFn: func(callCount int) error {
			return diskFull
		},
	}
	runner, err := wal.NewGroupCommitRunner(q, mock, wal.DefaultRunnerOptions())
	if err != nil {
		t.Fatalf("NewGroupCommitRunner failed: %v", err)
	}

	const n = 8
	tasks := make([]*wal.WriteTask, n)
	for i := 0; i < n; i++ {
		tasks[i] = createTestTask(t, uint64(i+1), "k", "v")
		if err := q.Enqueue(tasks[i]); err != nil {
			t.Fatalf("Enqueue %d failed: %v", i, err)
		}
	}

	if err := runner.Start(); err != nil {
		t.Fatalf("runner Start failed: %v", err)
	}
	defer func() { _ = runner.Stop() }()

	succeeded := 0
	for i, task := range tasks {
		err := task.Wait()
		if err == nil {
			succeeded++
			t.Errorf("task %d acknowledged durable despite sync failure (IND-C-003)", i)
			continue
		}
		if !stdErrors.Is(err, diskFull) {
			t.Errorf("task %d error does not unwrap to root cause: %v", i, err)
		}
		var syncErr *wal.BatchSyncError
		if !stdErrors.As(err, &syncErr) {
			t.Errorf("task %d error is not *wal.BatchSyncError: %T (%v)", i, err, err)
		}
		if !task.IsDone() {
			t.Errorf("task %d IsDone is false after completion", i)
		}
	}
	if succeeded != 0 {
		t.Fatalf("acknowledged-write loss: %d/%d tasks reported success on failed sync", succeeded, n)
	}
	if got := mock.SyncCalls(); got != 1 {
		t.Errorf("expected exactly 1 durability barrier attempt, got %d", got)
	}
}

// TestINDC003_RealWriterSyncFailureNeverAcksDurable covers the single-writer
// layer beneath group commit: a failing sync barrier must surface the error
// from AppendSync (never a nil durable ack) and poison the writer so later
// appends fail closed instead of silently losing data.
func TestINDC003_RealWriterSyncFailureNeverAcksDurable(t *testing.T) {
	dbPath := t.TempDir()
	w, _ := createRealWriter(t, dbPath)
	defer func() { _ = w.Close() }()

	barrierErr := stdErrors.New("simulated IND-C-003 device failure")
	w.SetSyncFnForTesting(func(f *os.File) error {
		return barrierErr
	})

	taskRec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(1),
		Timestamp: 1725800001,
		Key:       []byte("k"),
		Value:     []byte("v"),
	}
	if err := w.AppendSync(taskRec); err == nil {
		t.Fatalf("AppendSync acknowledged durable despite sync failure (IND-C-003)")
	} else if !stdErrors.Is(err, barrierErr) {
		t.Fatalf("AppendSync error does not unwrap to root cause: %v", err)
	}
	if !w.IsPoisoned() {
		t.Fatalf("writer must be poisoned after sync failure to prevent silent loss")
	}
	if err := w.AppendSync(taskRec); err == nil {
		t.Fatalf("poisoned writer acknowledged a later append")
	}
}
