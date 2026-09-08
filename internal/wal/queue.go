package wal

import (
	"sync"

	"github.com/silent-knight19/lattice/internal/errors"
)

// DefaultQueueCapacity is the default maximum number of queued WriteTasks
// waiting for group commit execution, matching Architecture Spec Section 14.1.
const DefaultQueueCapacity = 1024

// GroupCommitQueue is an alias for WriteQueue, aligning with storage terminology.
type GroupCommitQueue = WriteQueue

// WriteQueue is a bounded, thread-safe, FIFO queue for WriteTasks awaiting
// group commit synchronization.
//
// Queue Semantics:
//   - FIFO Ordering: Tasks are dequeued in strict first-in, first-out order.
//   - Bounded Capacity: Prevents unbounded memory growth under sustained write pressure.
//   - Blocking Backpressure: Enqueue blocks when the queue reaches capacity until space is freed.
//   - Non-Blocking Options: TryEnqueue and TryDequeue provide non-blocking alternatives.
//   - Graceful Close: Close stops accepting new tasks (Enqueue returns ErrQueueClosed),
//     while allowing existing queued tasks to be drained by consumers.
//   - Abrupt Close: CloseWithError stops accepting new tasks AND immediately completes
//     all remaining queued tasks with the provided error, guaranteeing zero hung waiters.
type WriteQueue struct {
	mu       sync.Mutex
	notEmpty *sync.Cond
	notFull  *sync.Cond
	buffer   []*WriteTask
	head     int
	tail     int
	count    int
	capacity int
	closed   bool
}

// NewWriteQueue creates a new WriteQueue with the given capacity.
// capacity must be strictly greater than 0, otherwise an *errors.InvalidQueueCapacityError is returned.
func NewWriteQueue(capacity int) (*WriteQueue, error) {
	if capacity <= 0 {
		return nil, &errors.InvalidQueueCapacityError{Capacity: capacity}
	}

	q := &WriteQueue{
		buffer:   make([]*WriteTask, capacity),
		capacity: capacity,
	}
	q.notEmpty = sync.NewCond(&q.mu)
	q.notFull = sync.NewCond(&q.mu)
	return q, nil
}

// Enqueue adds task to the tail of the queue, blocking if the queue is full until
// space becomes available or the queue is closed.
//
// Returns:
//   - nil on successful enqueue.
//   - errors.ErrNilTask if task is nil or uninitialized.
//   - errors.ErrTaskAlreadyCompleted if task has already been completed.
//   - errors.ErrTaskAlreadyEnqueued if task was already enqueued.
//   - errors.ErrQueueClosed if the queue is closed before or while waiting.
func (q *WriteQueue) Enqueue(task *WriteTask) error {
	if task == nil || task.done == nil {
		return errors.ErrNilTask
	}
	if task.IsDone() {
		return errors.ErrTaskAlreadyCompleted
	}
	if !task.markEnqueued() {
		return errors.ErrTaskAlreadyEnqueued
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	// Guard against uninitialized / zero-value queue
	if q.capacity <= 0 || q.buffer == nil || q.notFull == nil {
		task.resetEnqueued()
		return errors.ErrQueueClosed
	}

	for q.count == q.capacity && !q.closed {
		q.notFull.Wait()
	}

	if q.closed {
		task.resetEnqueued()
		return errors.ErrQueueClosed
	}

	q.buffer[q.tail] = task
	q.tail = (q.tail + 1) % q.capacity
	q.count++

	q.notEmpty.Signal()
	return nil
}

// TryEnqueue attempts to add task to the queue without blocking.
// If the queue is full, it returns errors.ErrQueueFull immediately.
func (q *WriteQueue) TryEnqueue(task *WriteTask) error {
	if task == nil || task.done == nil {
		return errors.ErrNilTask
	}
	if task.IsDone() {
		return errors.ErrTaskAlreadyCompleted
	}
	if !task.markEnqueued() {
		return errors.ErrTaskAlreadyEnqueued
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if q.capacity <= 0 || q.buffer == nil || q.notFull == nil {
		task.resetEnqueued()
		return errors.ErrQueueClosed
	}

	if q.closed {
		task.resetEnqueued()
		return errors.ErrQueueClosed
	}

	if q.count == q.capacity {
		task.resetEnqueued()
		return errors.ErrQueueFull
	}

	q.buffer[q.tail] = task
	q.tail = (q.tail + 1) % q.capacity
	q.count++

	q.notEmpty.Signal()
	return nil
}

// Dequeue removes and returns the task at the head of the queue, blocking until
// a task is available or the queue is closed and empty.
//
// If the queue is closed, Dequeue drains all remaining queued tasks before
// returning (nil, errors.ErrQueueClosed).
func (q *WriteQueue) Dequeue() (*WriteTask, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.capacity <= 0 || q.buffer == nil || q.notEmpty == nil {
		return nil, errors.ErrQueueClosed
	}

	for q.count == 0 && !q.closed {
		q.notEmpty.Wait()
	}

	if q.count == 0 {
		return nil, errors.ErrQueueClosed
	}

	task := q.buffer[q.head]
	q.buffer[q.head] = nil // Avoid GC reference leak
	q.head = (q.head + 1) % q.capacity
	q.count--

	q.notFull.Signal()
	return task, nil
}

// TryDequeue attempts to remove and return the task at the head of the queue without blocking.
// If the queue is empty, it returns (nil, errors.ErrQueueEmpty).
// If the queue is closed and empty, it returns (nil, errors.ErrQueueClosed).
func (q *WriteQueue) TryDequeue() (*WriteTask, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.capacity <= 0 || q.buffer == nil {
		return nil, errors.ErrQueueClosed
	}

	if q.count == 0 {
		if q.closed {
			return nil, errors.ErrQueueClosed
		}
		return nil, errors.ErrQueueEmpty
	}

	task := q.buffer[q.head]
	q.buffer[q.head] = nil // Avoid GC reference leak
	q.head = (q.head + 1) % q.capacity
	q.count--

	if q.notFull != nil {
		q.notFull.Signal()
	}
	return task, nil
}

// Close closes the queue for new writes. Future Enqueue calls will return ErrQueueClosed.
// Any tasks currently in the queue can still be dequeued by consumers until empty.
// Calling Close on an already closed queue returns nil.
func (q *WriteQueue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return nil
	}
	q.closed = true

	if q.notEmpty != nil {
		q.notEmpty.Broadcast()
	}
	if q.notFull != nil {
		q.notFull.Broadcast()
	}
	return nil
}

// CloseWithError closes the queue and immediately fails all remaining queued tasks
// with the provided err, ensuring no task waiters are left hanging.
// If err is nil, errors.ErrQueueClosed is used.
func (q *WriteQueue) CloseWithError(err error) error {
	if err == nil {
		err = errors.ErrQueueClosed
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	q.closed = true

	// Drain and complete all pending tasks in buffer
	for q.count > 0 {
		task := q.buffer[q.head]
		q.buffer[q.head] = nil
		q.head = (q.head + 1) % q.capacity
		q.count--

		if task != nil {
			_ = task.Complete(err)
		}
	}

	if q.notEmpty != nil {
		q.notEmpty.Broadcast()
	}
	if q.notFull != nil {
		q.notFull.Broadcast()
	}
	return nil
}

// Len returns the current number of tasks waiting in the queue.
func (q *WriteQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.count
}

// Cap returns the maximum capacity of the queue.
func (q *WriteQueue) Cap() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.capacity
}

// IsClosed reports whether the queue has been closed.
func (q *WriteQueue) IsClosed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}
