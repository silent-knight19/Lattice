package wal

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/silent-knight19/lattice/internal/errors"
)

// WriteTask represents an individual write operation submitted by a caller
// for durable inclusion in the Write-Ahead Log.
//
// Durability Contract:
// A WriteTask is NOT complete when enqueued, nor when copied to memory or
// written to the OS page cache. A WriteTask is complete ONLY when the underlying
// WAL segment has successfully synchronized all pending bytes to non-volatile
// storage (via fdatasync / Flush) or encountered an unrecoverable failure.
//
// Payload Immutability & Ownership:
//  1. Caller Input: NewWriteTask creates independent defensive copies of rec.Key and rec.Value.
//     Subsequent mutations of the caller's input slices do not affect the queued record.
//  2. Task Internal: The WriteTask owns its copied payload bytes until garbage collected.
//  3. Public Access: Record() returns independent defensive copies of the payload bytes,
//     ensuring external caller inspection can never mutate the task's internal state.
//  4. Group-Commit Executor: Package-internal unexported rawRecord() provides zero-copy
//     read-only access for high-throughput batch serialization in the future M02 executor.
type WriteTask struct {
	rec      Record
	done     chan struct{}
	errMu    sync.RWMutex
	err      error
	closed   bool
	enqueued uint32
}

// NewWriteTask constructs a new WriteTask from rec.
// It validates rec against physical framing and storage bounds using rec.Validate().
// If valid, it creates independent defensive copies of rec.Key and rec.Value,
// ensuring caller payload immutability.
//
// Returns an error if rec fails validation (e.g. invalid type, empty key, oversized key/val).
func NewWriteTask(rec Record) (*WriteTask, error) {
	if err := rec.Validate(); err != nil {
		return nil, err
	}

	var keyCopy []byte
	if len(rec.Key) > 0 {
		keyCopy = make([]byte, len(rec.Key))
		copy(keyCopy, rec.Key)
	}

	var valCopy []byte
	if len(rec.Value) > 0 {
		valCopy = make([]byte, len(rec.Value))
		copy(valCopy, rec.Value)
	}

	taskRec := Record{
		CRC:       rec.CRC,
		Type:      rec.Type,
		SeqNum:    rec.SeqNum,
		Timestamp: rec.Timestamp,
		Key:       keyCopy,
		Value:     valCopy,
	}

	return &WriteTask{
		rec:  taskRec,
		done: make(chan struct{}),
	}, nil
}

// Record returns a defensive copy of the Record payload held by this task.
// Any subsequent mutation of the returned Record's Key or Value byte slices
// does not affect the task's internally owned storage.
//
// Thread Safety:
// Record acquires a read lock on t.errMu, ensuring safe concurrent invocation
// alongside Complete() and other accessors.
// If t is nil, Record returns a zero-value Record.
func (t *WriteTask) Record() Record {
	if t == nil {
		return Record{}
	}

	t.errMu.RLock()
	defer t.errMu.RUnlock()

	var keyCopy []byte
	if len(t.rec.Key) > 0 {
		keyCopy = make([]byte, len(t.rec.Key))
		copy(keyCopy, t.rec.Key)
	}

	var valCopy []byte
	if len(t.rec.Value) > 0 {
		valCopy = make([]byte, len(t.rec.Value))
		copy(valCopy, t.rec.Value)
	}

	return Record{
		CRC:       t.rec.CRC,
		Type:      t.rec.Type,
		SeqNum:    t.rec.SeqNum,
		Timestamp: t.rec.Timestamp,
		Key:       keyCopy,
		Value:     valCopy,
	}
}

// rawRecord returns the internally owned Record without copying.
//
// PACKAGE-INTERNAL ONLY (M02 group-commit executor):
// This method is strictly unexported and must only be called by the WAL
// group commit batch executor. The caller MUST treat the returned Record's
// Key and Value byte slices as strictly read-only to avoid corrupting task state.
// If t is nil, rawRecord returns a zero-value Record.
func (t *WriteTask) rawRecord() Record {
	if t == nil {
		return Record{}
	}
	t.errMu.RLock()
	defer t.errMu.RUnlock()
	return t.rec
}

// Wait blocks until the task completes (durably written and synchronized, or failed).
// It returns nil on successful durability synchronization, or the durability error.
// If t is nil or uninitialized (zero-value task), it returns errors.ErrNilTask.
func (t *WriteTask) Wait() error {
	if t == nil {
		return errors.ErrNilTask
	}
	t.errMu.RLock()
	done := t.done
	t.errMu.RUnlock()

	if done == nil {
		return errors.ErrNilTask
	}

	<-done

	t.errMu.RLock()
	defer t.errMu.RUnlock()
	return t.err
}

// WaitContext blocks until either the task completes or ctx is canceled / timed out.
// If the task completes before ctx cancellation, it returns the task's durability error.
// If ctx is canceled before the task completes, it returns ctx.Err().
// If t is nil or uninitialized, it returns errors.ErrNilTask.
func (t *WriteTask) WaitContext(ctx context.Context) error {
	if t == nil {
		return errors.ErrNilTask
	}
	if ctx == nil {
		return t.Wait()
	}
	t.errMu.RLock()
	done := t.done
	t.errMu.RUnlock()

	if done == nil {
		return errors.ErrNilTask
	}

	select {
	case <-done:
		t.errMu.RLock()
		defer t.errMu.RUnlock()
		return t.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Done returns a receive-only channel that is closed when the task completes.
// If t is nil or uninitialized, Done returns nil.
func (t *WriteTask) Done() <-chan struct{} {
	if t == nil {
		return nil
	}
	t.errMu.RLock()
	defer t.errMu.RUnlock()
	return t.done
}

// Err returns the completion error if the task is complete, or nil if still pending or succeeded.
// To determine if a nil return indicates success or in-progress, check IsDone().
// If t is nil, Err returns errors.ErrNilTask.
func (t *WriteTask) Err() error {
	if t == nil {
		return errors.ErrNilTask
	}
	t.errMu.RLock()
	defer t.errMu.RUnlock()
	return t.err
}

// IsDone reports whether the task has completed (either with success or failure).
// If t is nil, IsDone returns false.
func (t *WriteTask) IsDone() bool {
	if t == nil {
		return false
	}
	t.errMu.RLock()
	defer t.errMu.RUnlock()
	return t.closed
}

// Complete marks the task as completed with the specified error (or nil on success)
// and closes the done notification channel.
//
// Complete can only be called once. Subsequent invocations return errors.ErrTaskAlreadyCompleted
// without modifying the original completion error.
// If t is nil or uninitialized, it returns errors.ErrNilTask.
func (t *WriteTask) Complete(err error) error {
	if t == nil {
		return errors.ErrNilTask
	}
	t.errMu.Lock()
	defer t.errMu.Unlock()

	if t.done == nil {
		return errors.ErrNilTask
	}
	if t.closed {
		return errors.ErrTaskAlreadyCompleted
	}

	t.err = err
	t.closed = true
	close(t.done)
	return nil
}

// markEnqueued atomically marks the task as having been enqueued.
// Returns true if this is the first enqueue attempt, or false if already enqueued.
func (t *WriteTask) markEnqueued() bool {
	if t == nil {
		return false
	}
	return atomic.CompareAndSwapUint32(&t.enqueued, 0, 1)
}

// resetEnqueued clears the enqueued mark if enqueue failed before queue insertion.
func (t *WriteTask) resetEnqueued() {
	if t == nil {
		return
	}
	atomic.StoreUint32(&t.enqueued, 0)
}
