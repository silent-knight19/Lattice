package wal_test

import (
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/wal"
)

// panicAppendWriter panics on Append to simulate a defective BatchWriter.
type panicAppendWriter struct{}

func (panicAppendWriter) Append(wal.Record) error { panic("boom-append") }
func (panicAppendWriter) Sync() error             { return nil }

// panicSyncWriter panics on Sync to cover the post-append panic window.
type panicSyncWriter struct{}

func (panicSyncWriter) Append(wal.Record) error { return nil }
func (panicSyncWriter) Sync() error             { panic("boom-sync") }

// TestAudit_F002_BatchPanicCannotOrphanTasks is the regression test for
// AUDIT-F-002 (HIGH): pre-fix, a panic inside executeBatch unwound past the
// dequeued in-flight batch straight to the run-loop interceptor, which only
// failed the still-queued tasks. The in-flight tasks were never completed, so
// callers blocked in Wait()/WaitContext() forever. Post-fix every in-flight
// task completes with a non-nil error and Wait unblocks.
func TestAudit_F002_BatchPanicCannotOrphanTasks(t *testing.T) {
	for _, writer := range []wal.BatchWriter{panicAppendWriter{}, panicSyncWriter{}} {
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

		r, err := wal.NewGroupCommitRunner(q, writer, wal.DefaultRunnerOptions())
		if err != nil {
			t.Fatalf("NewGroupCommitRunner failed: %v", err)
		}
		if err := r.Start(); err != nil {
			t.Fatalf("Start failed: %v", err)
		}

		done := make(chan error, 1)
		go func() { done <- task.Wait() }()

		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("writer %T: expected non-nil batch panic error, got nil", writer)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("writer %T: in-flight task never completed after batch panic (hung Wait)", writer)
		}

		if !task.IsDone() {
			t.Fatalf("writer %T: task not marked done after batch panic", writer)
		}
		if err := r.Stop(); err != nil {
			t.Fatalf("Stop failed: %v", err)
		}
	}
}
