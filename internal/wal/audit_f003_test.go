package wal_test

import (
	stdErrors "errors"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

// TestAudit_F003_NilReaderNeverPanics is the regression test for AUDIT-F-003:
// every method on a nil *WALReader must fail visibly instead of panicking
// with a nil-pointer dereference.
func TestAudit_F003_NilReaderNeverPanics(t *testing.T) {
	var r *wal.WALReader

	if got := r.Path(); got != "" {
		t.Errorf("nil Path() = %q, want empty", got)
	}
	if got := r.Offset(); got != 0 {
		t.Errorf("nil Offset() = %d, want 0", got)
	}
	if err := r.Close(); !stdErrors.Is(err, errors.ErrReaderClosed) {
		t.Errorf("nil Close() = %v, want ErrReaderClosed", err)
	}
	if _, err := r.Next(); !stdErrors.Is(err, errors.ErrReaderClosed) {
		t.Errorf("nil Next() = %v, want ErrReaderClosed", err)
	}
}

// TestAudit_F003_NilWriterNeverPanics is the regression test for AUDIT-F-003:
// every method on a nil *WALWriter must fail visibly instead of panicking.
func TestAudit_F003_NilWriterNeverPanics(t *testing.T) {
	var w *wal.WALWriter
	rec := wal.Record{Type: wal.RecordTypePut, SeqNum: 1, Key: []byte("k"), Value: []byte("v")}

	if got := w.Path(); got != "" {
		t.Errorf("nil Path() = %q, want empty", got)
	}
	if w.IsPoisoned() {
		t.Errorf("nil IsPoisoned() = true, want false")
	}
	if _, err := w.Size(); !stdErrors.Is(err, errors.ErrWriterClosed) {
		t.Errorf("nil Size() = %v, want ErrWriterClosed", err)
	}
	if err := w.Close(); !stdErrors.Is(err, errors.ErrWriterClosed) {
		t.Errorf("nil Close() = %v, want ErrWriterClosed", err)
	}
	if err := w.Append(rec); !stdErrors.Is(err, errors.ErrWriterClosed) {
		t.Errorf("nil Append() = %v, want ErrWriterClosed", err)
	}
	if err := w.Sync(); !stdErrors.Is(err, errors.ErrWriterClosed) {
		t.Errorf("nil Sync() = %v, want ErrWriterClosed", err)
	}
	if err := w.AppendSync(rec); !stdErrors.Is(err, errors.ErrWriterClosed) {
		t.Errorf("nil AppendSync() = %v, want ErrWriterClosed", err)
	}
}

// TestAudit_F003_ZeroWriterReportsNotInitialized covers the zero-value
// *WALWriter (never constructed): pre-fix, Append/Close/Sync panicked calling
// the nil syncFn/writeFn. Post-fix they report ErrNotInitialized.
func TestAudit_F003_ZeroWriterReportsNotInitialized(t *testing.T) {
	w := &wal.WALWriter{}
	rec := wal.Record{Type: wal.RecordTypePut, SeqNum: 1, Key: []byte("k"), Value: []byte("v")}

	if _, err := w.Size(); !stdErrors.Is(err, errors.ErrNotInitialized) {
		t.Errorf("zero Size() = %v, want ErrNotInitialized", err)
	}
	if err := w.Close(); !stdErrors.Is(err, errors.ErrNotInitialized) {
		t.Errorf("zero Close() = %v, want ErrNotInitialized", err)
	}
	if err := w.Append(rec); !stdErrors.Is(err, errors.ErrNotInitialized) {
		t.Errorf("zero Append() = %v, want ErrNotInitialized", err)
	}
	if err := w.Sync(); !stdErrors.Is(err, errors.ErrNotInitialized) {
		t.Errorf("zero Sync() = %v, want ErrNotInitialized", err)
	}
	if err := w.AppendSync(rec); !stdErrors.Is(err, errors.ErrNotInitialized) {
		t.Errorf("zero AppendSync() = %v, want ErrNotInitialized", err)
	}
}

// TestAudit_F004_DecodeRecordNilReader is the regression test for AUDIT-F-004:
// DecodeRecord(nil) panicked inside io.ReadFull. It must return an error.
func TestAudit_F004_DecodeRecordNilReader(t *testing.T) {
	if _, err := wal.DecodeRecord(nil); err == nil {
		t.Fatalf("DecodeRecord(nil) = nil error, want non-nil")
	}
}

// TestAudit_Reaudit_NilQueueNeverPanics extends AUDIT-F-003 to *WriteQueue:
// every method on a nil queue must fail visibly instead of panicking.
func TestAudit_Reaudit_NilQueueNeverPanics(t *testing.T) {
	var q *wal.WriteQueue
	task, err := wal.NewWriteTask(wal.Record{Type: wal.RecordTypePut, SeqNum: 1, Key: []byte("k"), Value: []byte("v")})
	if err != nil {
		t.Fatalf("NewWriteTask failed: %v", err)
	}

	if err := q.Enqueue(task); !stdErrors.Is(err, errors.ErrQueueClosed) {
		t.Errorf("nil Enqueue() = %v, want ErrQueueClosed", err)
	}
	if err := q.TryEnqueue(task); !stdErrors.Is(err, errors.ErrQueueClosed) {
		t.Errorf("nil TryEnqueue() = %v, want ErrQueueClosed", err)
	}
	// Rejected enqueues must not consume the task: it stays re-enqueueable.
	if task.IsDone() {
		t.Errorf("task marked done/enqueued after rejected nil-queue enqueue")
	}
	if _, err := q.Dequeue(); !stdErrors.Is(err, errors.ErrQueueClosed) {
		t.Errorf("nil Dequeue() = %v, want ErrQueueClosed", err)
	}
	if _, err := q.TryDequeue(); !stdErrors.Is(err, errors.ErrQueueClosed) {
		t.Errorf("nil TryDequeue() = %v, want ErrQueueClosed", err)
	}
	if _, err := q.DequeueBatch(8, 1024); !stdErrors.Is(err, errors.ErrQueueClosed) {
		t.Errorf("nil DequeueBatch() = %v, want ErrQueueClosed", err)
	}
	if _, err := q.TryDequeueBatch(8, 1024); !stdErrors.Is(err, errors.ErrQueueClosed) {
		t.Errorf("nil TryDequeueBatch() = %v, want ErrQueueClosed", err)
	}
	if err := q.Close(); err != nil {
		t.Errorf("nil Close() = %v, want nil", err)
	}
	if err := q.CloseWithError(nil); err != nil {
		t.Errorf("nil CloseWithError() = %v, want nil", err)
	}
	if q.Len() != 0 || q.Cap() != 0 || !q.IsClosed() {
		t.Errorf("nil Len/Cap/IsClosed wrong: %d %d %v", q.Len(), q.Cap(), q.IsClosed())
	}
}

// TestAudit_Reaudit_NilReplayFuncIsNoop covers ReplayFunc(nil).Apply: pre-fix
// it panicked calling the nil func value. It now matches validation-only
// (nil sink) semantics.
func TestAudit_Reaudit_NilReplayFuncIsNoop(t *testing.T) {
	var f wal.ReplayFunc
	rec := wal.Record{Type: wal.RecordTypePut, SeqNum: 1, Key: []byte("k"), Value: []byte("v")}
	if err := f.Apply(rec); err != nil {
		t.Errorf("nil ReplayFunc.Apply() = %v, want nil", err)
	}
}

// TestAudit_Reaudit_StopWithoutStartFailsTasksFast covers the AUDIT-F-002
// follow-up: stopping a never-started runner must fail pending tasks instead
// of leaving Wait blocked forever.
func TestAudit_Reaudit_StopWithoutStartFailsTasksFast(t *testing.T) {
	q, err := wal.NewWriteQueue(16)
	if err != nil {
		t.Fatalf("NewWriteQueue failed: %v", err)
	}
	task, err := wal.NewWriteTask(wal.Record{Type: wal.RecordTypePut, SeqNum: 1, Key: []byte("k"), Value: []byte("v")})
	if err != nil {
		t.Fatalf("NewWriteTask failed: %v", err)
	}
	if err := q.Enqueue(task); err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}
	r, err := wal.NewGroupCommitRunner(q, &failSyncWriter{}, wal.DefaultRunnerOptions())
	if err != nil {
		t.Fatalf("NewGroupCommitRunner failed: %v", err)
	}
	// Never started.
	if err := r.Stop(); err != nil {
		t.Fatalf("Stop without Start failed: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- task.Wait() }()
	select {
	case err := <-done:
		if !stdErrors.Is(err, errors.ErrRunnerClosed) {
			t.Fatalf("task Wait after Stop-without-Start = %v, want ErrRunnerClosed", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("task Wait hung after Stop-without-Start")
	}
}
