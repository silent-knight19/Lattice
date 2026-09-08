package wal

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/silent-knight19/lattice/internal/errors"
)

// BatchWriter encapsulates the sequential append and synchronization barrier operations
// required by the group commit batch runner. Both *WALWriter and *RotatingWriter
// satisfy this interface.
type BatchWriter interface {
	// Append serializes and writes record to the active segment without an fsync barrier.
	Append(rec Record) error

	// Sync executes a durability synchronization barrier on the active segment.
	Sync() error
}

// RunnerOptions configures execution parameters for GroupCommitRunner.
type RunnerOptions struct {
	// MaxBatchTasks is the maximum number of tasks coalesced into a single batch.
	// Defaults to MaxBatchTasks (1,024). Clamped to MaxBatchTasks if <= 0 or > MaxBatchTasks.
	MaxBatchTasks int

	// MaxBatchBytes is the maximum encoded wire size in bytes coalesced into a single batch.
	// Defaults to MaxBatchBytes (64 KiB). Clamped to MaxBatchBytes if <= 0 or > MaxBatchBytes.
	MaxBatchBytes int64
}

// DefaultRunnerOptions returns the standard architectural configuration for GroupCommitRunner:
// MaxBatchTasks = 1024, MaxBatchBytes = 64 KiB.
func DefaultRunnerOptions() RunnerOptions {
	return RunnerOptions{
		MaxBatchTasks: MaxBatchTasks,
		MaxBatchBytes: MaxBatchBytes,
	}
}

// BatchAppendError records an error encountered when appending an individual record in a batch.
type BatchAppendError struct {
	Index int
	Err   error
}

func (e *BatchAppendError) Error() string {
	return fmt.Sprintf("wal: batch append failed at index %d: %v", e.Index, e.Err)
}

func (e *BatchAppendError) Unwrap() error {
	return e.Err
}

// BatchSyncError records an error encountered during the durability synchronization barrier of a batch.
type BatchSyncError struct {
	Err error
}

func (e *BatchSyncError) Error() string {
	return fmt.Sprintf("wal: batch sync barrier failed: %v", e.Err)
}

func (e *BatchSyncError) Unwrap() error {
	return e.Err
}

// GroupCommitRunner is the execution engine for WAL group commit.
//
// Architectural Responsibilities:
//   - Acts as the single local serialization point for physical WAL writes.
//   - Consumes pending WriteTasks from a bounded FIFO WriteQueue.
//   - Forms bounded logical batches (at most 1,024 tasks or 64 KiB wire bytes).
//   - Preserves FIFO ordering under concurrent producers.
//   - Appends records sequentially to the active WAL segment via zero-copy rawRecord().
//   - Performs exactly ONE hardware durability barrier (fdatasync) per successful batch.
//   - Completes every task in the batch if and only if the synchronization barrier succeeds.
//   - Propagates write or synchronization failures to every task in the affected batch.
//   - Supports graceful shutdown by draining queued tasks to completion.
//   - Intercepts panics to unblock pending waiters, preventing deadlocks.
type GroupCommitRunner struct {
	queue  *WriteQueue
	writer BatchWriter
	opts   RunnerOptions

	mu      sync.Mutex
	running bool
	stopped bool
	stopCh  chan struct{}
	doneCh  chan struct{}

	// Observability & Seam Counters (atomic 64-bit access)
	syncsCount   int64
	batchesCount int64
	tasksCount   int64
	bytesCount   int64
}

// NewGroupCommitRunner initializes a GroupCommitRunner connected to the given queue and writer.
// Returns an error if queue or writer is nil.
func NewGroupCommitRunner(queue *WriteQueue, writer BatchWriter, opts RunnerOptions) (*GroupCommitRunner, error) {
	if queue == nil {
		return nil, fmt.Errorf("wal: queue cannot be nil")
	}
	if writer == nil {
		return nil, fmt.Errorf("wal: writer cannot be nil")
	}

	if opts.MaxBatchTasks <= 0 || opts.MaxBatchTasks > MaxBatchTasks {
		opts.MaxBatchTasks = MaxBatchTasks
	}
	if opts.MaxBatchBytes <= 0 || opts.MaxBatchBytes > MaxBatchBytes {
		opts.MaxBatchBytes = MaxBatchBytes
	}

	return &GroupCommitRunner{
		queue:  queue,
		writer: writer,
		opts:   opts,
	}, nil
}

// Start launches the group commit background execution loop in a dedicated goroutine.
//
// Invariants:
//   - Returns errors.ErrRunnerRunning if the runner is already running.
//   - Returns errors.ErrRunnerClosed if the runner has been stopped.
//   - Exactly one runner goroutine executes at any time.
func (r *GroupCommitRunner) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.stopped {
		return errors.ErrRunnerClosed
	}
	if r.running {
		return errors.ErrRunnerRunning
	}

	r.running = true
	r.stopCh = make(chan struct{})
	r.doneCh = make(chan struct{})

	go r.run()
	return nil
}

// Stop signals the runner to stop accepting new work, gracefully drains all remaining
// queued tasks to completion, and waits for the runner loop to exit cleanly.
// Stop is idempotent; repeated calls return nil.
func (r *GroupCommitRunner) Stop() error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return nil
	}
	if !r.running {
		r.stopped = true
		r.mu.Unlock()
		return nil
	}

	r.stopped = true
	close(r.stopCh)
	// Gracefully close queue to wake any blocked DequeueBatch
	_ = r.queue.Close()
	doneCh := r.doneCh
	r.mu.Unlock()

	<-doneCh
	return nil
}

// Close is an alias for Stop, satisfying io.Closer.
func (r *GroupCommitRunner) Close() error {
	return r.Stop()
}

// IsRunning reports whether the runner is currently executing.
func (r *GroupCommitRunner) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running && !r.stopped
}

// SyncCount returns the total number of physical durability synchronization barriers
// executed by this runner.
func (r *GroupCommitRunner) SyncCount() int64 {
	return atomic.LoadInt64(&r.syncsCount)
}

// BatchesExecuted returns the total number of batches executed by this runner.
func (r *GroupCommitRunner) BatchesExecuted() int64 {
	return atomic.LoadInt64(&r.batchesCount)
}

// TasksExecuted returns the total number of write tasks completed by this runner.
func (r *GroupCommitRunner) TasksExecuted() int64 {
	return atomic.LoadInt64(&r.tasksCount)
}

// BytesWritten returns the total physical wire bytes appended by this runner.
func (r *GroupCommitRunner) BytesWritten() int64 {
	return atomic.LoadInt64(&r.bytesCount)
}

// run is the internal event loop for the group commit runner.
func (r *GroupCommitRunner) run() {
	defer func() {
		// Panic interception per Architecture Spec Section 1474:
		// Prevent hung task waiters if an unforeseen panic occurs.
		if p := recover(); p != nil {
			panicErr := fmt.Errorf("wal: group commit runner panicked: %v", p)
			_ = r.queue.CloseWithError(panicErr)
		}

		r.mu.Lock()
		r.running = false
		r.stopped = true
		r.mu.Unlock()

		close(r.doneCh)
	}()

	for {
		// Check if cancellation requested
		select {
		case <-r.stopCh:
			r.drainRemaining()
			return
		default:
		}

		// Dequeue next bounded batch (blocks until work is available or queue is closed)
		batch, err := r.queue.DequeueBatch(r.opts.MaxBatchTasks, r.opts.MaxBatchBytes)
		if err != nil {
			// Queue is closed or error encountered
			r.drainRemaining()
			return
		}

		if len(batch) == 0 {
			continue
		}

		r.executeBatch(batch)
	}
}

// drainRemaining processes any remaining tasks in the queue using non-blocking TryDequeueBatch.
func (r *GroupCommitRunner) drainRemaining() {
	for {
		batch, err := r.queue.TryDequeueBatch(r.opts.MaxBatchTasks, r.opts.MaxBatchBytes)
		if err != nil || len(batch) == 0 {
			break
		}
		r.executeBatch(batch)
	}
}

// executeBatch executes a formed batch:
//  1. Appends each record sequentially to the writer without sync.
//  2. If any append fails, fails all tasks in the batch and aborts.
//  3. Executes exactly ONE durability barrier (Sync).
//  4. If Sync fails, fails all tasks in the batch.
//  5. If Sync succeeds, completes every task with nil.
func (r *GroupCommitRunner) executeBatch(batch []*WriteTask) {
	if len(batch) == 0 {
		return
	}

	var batchBytes int64
	var appendErr error

	// Step 1: Append all batch records to physical segment
	for i, task := range batch {
		rec := task.rawRecord()
		wireSize := RecordWireSize(rec)

		if err := r.writer.Append(rec); err != nil {
			appendErr = &BatchAppendError{Index: i, Err: err}
			break
		}
		batchBytes += wireSize
	}

	// Failure Case B: Append failed mid-batch or before any bytes appended
	if appendErr != nil {
		for _, task := range batch {
			_ = task.Complete(appendErr)
		}
		return
	}

	// Step 2: Durability synchronization barrier
	syncErr := r.writer.Sync()
	atomic.AddInt64(&r.syncsCount, 1)

	// Failure Case C: Sync barrier failed
	if syncErr != nil {
		err := &BatchSyncError{Err: syncErr}
		for _, task := range batch {
			_ = task.Complete(err)
		}
		return
	}

	// Success: Durability established on non-volatile storage
	for _, task := range batch {
		_ = task.Complete(nil)
	}

	// Observability updates
	atomic.AddInt64(&r.batchesCount, 1)
	atomic.AddInt64(&r.tasksCount, int64(len(batch)))
	atomic.AddInt64(&r.bytesCount, batchBytes)
}
