package wal_test

import (
	stdErrors "errors"
	"os"
	"testing"

	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

// TestAudit_F006_ActiveWriterIsReadOnly is the regression test for AUDIT-F-006:
// ActiveWriter must expose only read-only inspection. Pre-fix it returned the
// mutable *WALWriter, letting callers append behind the RotatingWriter's back
// (stale rotation accounting) or close it out from under the owner.
func TestAudit_F006_ActiveWriterIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 1 << 20})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	h := rw.ActiveWriter()
	if h == nil {
		t.Fatalf("ActiveWriter() = nil on live writer, want non-nil handle")
	}
	// Read-only inspection must work through the handle.
	if h.Path() == "" {
		t.Errorf("ActiveSegment.Path() empty")
	}
	if _, err := h.Size(); err != nil {
		t.Errorf("ActiveSegment.Size() failed: %v", err)
	}
	if h.IsPoisoned() {
		t.Errorf("ActiveSegment.IsPoisoned() = true on fresh writer")
	}
	if err := h.Sync(); err != nil {
		t.Errorf("ActiveSegment.Sync() failed: %v", err)
	}

	// The handle must not expose mutation: compile-time guarantee asserted
	// via the interface — a *wal.WALWriter is assignable where mutation is
	// intended, but ActiveWriter's static type is wal.ActiveSegment.
	var _ wal.ActiveSegment = h

	if err := rw.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if rw.ActiveWriter() != nil {
		t.Errorf("ActiveWriter() after Close = non-nil, want nil")
	}
}

// TestAudit_F006_NilRotatingWriterNeverPanics covers nil *RotatingWriter and
// nil *GroupCommitRunner: all methods must fail visibly, never panic.
func TestAudit_F006_NilRotatingWriterNeverPanics(t *testing.T) {
	var rw *wal.RotatingWriter
	rec := wal.Record{Type: wal.RecordTypePut, SeqNum: 1, Key: []byte("k"), Value: []byte("v")}

	if rw.ActiveWriter() != nil {
		t.Errorf("nil ActiveWriter() non-nil")
	}
	if got := rw.ActiveSegmentID(); got != 0 {
		t.Errorf("nil ActiveSegmentID() = %d, want 0", got)
	}
	if got := rw.ActiveSegmentSize(); got != 0 {
		t.Errorf("nil ActiveSegmentSize() = %d, want 0", got)
	}
	if got := rw.ActivePath(); got != "" {
		t.Errorf("nil ActivePath() = %q, want empty", got)
	}
	if got := rw.DBPath(); got != "" {
		t.Errorf("nil DBPath() = %q, want empty", got)
	}
	if _, err := rw.Segments(); !stdErrors.Is(err, errors.ErrWriterClosed) {
		t.Errorf("nil Segments() = %v, want ErrWriterClosed", err)
	}
	if err := rw.Append(rec); !stdErrors.Is(err, errors.ErrWriterClosed) {
		t.Errorf("nil Append() = %v, want ErrWriterClosed", err)
	}
	if err := rw.AppendSync(rec); !stdErrors.Is(err, errors.ErrWriterClosed) {
		t.Errorf("nil AppendSync() = %v, want ErrWriterClosed", err)
	}
	if err := rw.Sync(); !stdErrors.Is(err, errors.ErrWriterClosed) {
		t.Errorf("nil Sync() = %v, want ErrWriterClosed", err)
	}
	if err := rw.Rotate(); !stdErrors.Is(err, errors.ErrWriterClosed) {
		t.Errorf("nil Rotate() = %v, want ErrWriterClosed", err)
	}
	if err := rw.Close(); !stdErrors.Is(err, errors.ErrWriterClosed) {
		t.Errorf("nil Close() = %v, want ErrWriterClosed", err)
	}

	var r *wal.GroupCommitRunner
	if err := r.Start(); !stdErrors.Is(err, errors.ErrRunnerClosed) {
		t.Errorf("nil Start() = %v, want ErrRunnerClosed", err)
	}
	if err := r.Stop(); !stdErrors.Is(err, errors.ErrRunnerClosed) {
		t.Errorf("nil Stop() = %v, want ErrRunnerClosed", err)
	}
	if err := r.Close(); !stdErrors.Is(err, errors.ErrRunnerClosed) {
		t.Errorf("nil Close() = %v, want ErrRunnerClosed", err)
	}
	if r.IsRunning() {
		t.Errorf("nil IsRunning() = true, want false")
	}
	if r.SyncCount() != 0 || r.BatchesExecuted() != 0 || r.TasksExecuted() != 0 || r.BytesWritten() != 0 {
		t.Errorf("nil counters non-zero")
	}
}

// TestAudit_F007_SegmentIDZeroRejected is the regression test for AUDIT-F-007:
// id 0 constructors must fail because SegmentName(0) is unlistable and
// unrecoverable (ParseSegmentID requires ID >= 1).
func TestAudit_F007_SegmentIDZeroRejected(t *testing.T) {
	dir := t.TempDir()

	if _, err := wal.OpenSegmentWriter(dir, 0); !stdErrors.Is(err, os.ErrInvalid) {
		t.Errorf("OpenSegmentWriter(0) = %v, want os.ErrInvalid", err)
	}
	if _, err := wal.CreateSegmentWriter(dir, 0); !stdErrors.Is(err, os.ErrInvalid) {
		t.Errorf("CreateSegmentWriter(0) = %v, want os.ErrInvalid", err)
	}
	if _, err := wal.OpenSegmentReader(dir, 0); !stdErrors.Is(err, os.ErrInvalid) {
		t.Errorf("OpenSegmentReader(0) = %v, want os.ErrInvalid", err)
	}
	// No recovery-invisible file may have been created as a side effect.
	if _, err := os.Stat(wal.SegmentPath(dir, 0)); !os.IsNotExist(err) {
		t.Errorf("segment-0 file exists on disk after rejected construction")
	}
}

// failSyncWriter fails Sync to prove failed barriers are not counted (F-008).
type failSyncWriter struct{ appends int }

func (f *failSyncWriter) Append(wal.Record) error { f.appends++; return nil }
func (f *failSyncWriter) Sync() error             { return os.ErrInvalid }

// TestAudit_F008_FailedSyncNotCounted is the regression test for AUDIT-F-008:
// SyncCount must reflect successful durability barriers only.
func TestAudit_F008_FailedSyncNotCounted(t *testing.T) {
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
	fw := &failSyncWriter{}
	r, err := wal.NewGroupCommitRunner(q, fw, wal.DefaultRunnerOptions())
	if err != nil {
		t.Fatalf("NewGroupCommitRunner failed: %v", err)
	}
	if err := r.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if err := task.Wait(); err == nil {
		t.Fatalf("expected task failure from failing Sync, got nil")
	}
	if err := r.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if got := r.SyncCount(); got != 0 {
		t.Errorf("SyncCount() = %d after failed barrier, want 0", got)
	}
}
