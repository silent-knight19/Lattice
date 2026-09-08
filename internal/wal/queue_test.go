package wal

import (
	"bytes"
	"context"
	stdErrors "errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// helper to create a valid PUT record
func makeValidPutRecord(key, val string, seq uint64) Record {
	return Record{
		Type:      RecordTypePut,
		Key:       []byte(key),
		Value:     []byte(val),
		SeqNum:    binary.SeqNum(seq),
		Timestamp: uint64(time.Now().UnixNano()),
	}
}

// 1. TestCreateOneTask & 3. TestTaskRecordValidityPreserved & 22. TestSequenceMetadataPreserved
func TestCreateOneTask(t *testing.T) {
	rec := makeValidPutRecord("test-key", "test-val", 42)
	task, err := NewWriteTask(rec)
	if err != nil {
		t.Fatalf("unexpected error creating task: %v", err)
	}

	if task == nil {
		t.Fatal("expected non-nil WriteTask")
	}

	gotRec := task.Record()
	if gotRec.Type != rec.Type {
		t.Errorf("got RecordType %v, want %v", gotRec.Type, rec.Type)
	}
	if gotRec.SeqNum != rec.SeqNum {
		t.Errorf("got SeqNum %d, want %d", gotRec.SeqNum, rec.SeqNum)
	}
	if gotRec.Timestamp != rec.Timestamp {
		t.Errorf("got Timestamp %d, want %d", gotRec.Timestamp, rec.Timestamp)
	}
	if !bytes.Equal(gotRec.Key, rec.Key) {
		t.Errorf("got Key %q, want %q", gotRec.Key, rec.Key)
	}
	if !bytes.Equal(gotRec.Value, rec.Value) {
		t.Errorf("got Value %q, want %q", gotRec.Value, rec.Value)
	}
	if task.IsDone() {
		t.Error("newly created task must not be done")
	}
	if task.Err() != nil {
		t.Errorf("newly created task err must be nil, got %v", task.Err())
	}
}

// 2. TestCreateMultipleTasks
func TestCreateMultipleTasks(t *testing.T) {
	const count = 50
	tasks := make([]*WriteTask, count)
	for i := 0; i < count; i++ {
		rec := makeValidPutRecord(fmt.Sprintf("key-%d", i), fmt.Sprintf("val-%d", i), uint64(i+1))
		task, err := NewWriteTask(rec)
		if err != nil {
			t.Fatalf("task %d: unexpected error: %v", i, err)
		}
		tasks[i] = task
	}

	for i, task := range tasks {
		rec := task.Record()
		wantKey := fmt.Sprintf("key-%d", i)
		if string(rec.Key) != wantKey {
			t.Errorf("task %d: got key %q, want %q", i, rec.Key, wantKey)
		}
		if uint64(rec.SeqNum) != uint64(i+1) {
			t.Errorf("task %d: got seq %d, want %d", i, rec.SeqNum, i+1)
		}
	}
}

// 4. TestTaskPayloadOwnership & Adversarial: caller mutating Record payload after enqueue
func TestTaskPayloadOwnership(t *testing.T) {
	key := []byte("original-key")
	val := []byte("original-val")
	rec := Record{
		Type:      RecordTypePut,
		Key:       key,
		Value:     val,
		SeqNum:    100,
		Timestamp: 1000,
	}

	task, err := NewWriteTask(rec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Mutate the caller's slice
	key[0] = 'X'
	val[0] = 'Y'

	// The task's record must remain unchanged
	got := task.Record()
	if string(got.Key) != "original-key" {
		t.Errorf("payload aliasing detected! got key %q, want %q", got.Key, "original-key")
	}
	if string(got.Value) != "original-val" {
		t.Errorf("payload aliasing detected! got val %q, want %q", got.Value, "original-val")
	}
}

// 5. TestTaskCompletionSuccess
func TestTaskCompletionSuccess(t *testing.T) {
	task, err := NewWriteTask(makeValidPutRecord("k", "v", 1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- task.Wait()
	}()

	select {
	case <-doneCh:
		t.Fatal("Wait should block until Complete is called")
	case <-time.After(20 * time.Millisecond):
	}

	if err := task.Complete(nil); err != nil {
		t.Fatalf("unexpected error on Complete: %v", err)
	}

	select {
	case err := <-doneCh:
		if err != nil {
			t.Errorf("Wait returned unexpected error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait timed out after Complete(nil)")
	}

	if !task.IsDone() {
		t.Error("IsDone must report true after Complete")
	}
	if task.Err() != nil {
		t.Errorf("Err must be nil, got %v", task.Err())
	}
}

// 6. TestTaskCompletionFailure & 7. TestWaiterSeesExactError
func TestTaskCompletionFailure(t *testing.T) {
	task, err := NewWriteTask(makeValidPutRecord("k", "v", 1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	durabilityErr := errors.ErrChecksumMismatch

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- task.Wait()
	}()

	if err := task.Complete(durabilityErr); err != nil {
		t.Fatalf("unexpected error on Complete: %v", err)
	}

	select {
	case err := <-doneCh:
		if !stdErrors.Is(err, errors.ErrChecksumMismatch) {
			t.Errorf("Wait returned %v, want %v", err, durabilityErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait timed out after Complete(err)")
	}

	if !task.IsDone() {
		t.Error("IsDone must report true after Complete")
	}
	if !stdErrors.Is(task.Err(), errors.ErrChecksumMismatch) {
		t.Errorf("Err returned %v, want %v", task.Err(), durabilityErr)
	}
}

// 18. TestTaskCannotBeCompletedTwice (Adversarial: task completed twice)
func TestTaskCannotBeCompletedTwice(t *testing.T) {
	task, err := NewWriteTask(makeValidPutRecord("k", "v", 1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := task.Complete(nil); err != nil {
		t.Fatalf("first Complete failed: %v", err)
	}

	// Second complete with different error must be rejected with ErrTaskAlreadyCompleted
	err2 := task.Complete(errors.ErrWriterClosed)
	if !stdErrors.Is(err2, errors.ErrTaskAlreadyCompleted) {
		t.Errorf("second Complete returned %v, want %v", err2, errors.ErrTaskAlreadyCompleted)
	}

	// Original error (nil) must be preserved
	if task.Err() != nil {
		t.Errorf("task error corrupted by second complete: got %v, want nil", task.Err())
	}
}

// 8. TestFIFOEnqueueDequeue
func TestFIFOEnqueueDequeue(t *testing.T) {
	q, err := NewWriteQueue(10)
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}

	const n = 5
	tasks := make([]*WriteTask, n)
	for i := 0; i < n; i++ {
		rec := makeValidPutRecord(fmt.Sprintf("k-%d", i), "v", uint64(i+1))
		task, err := NewWriteTask(rec)
		if err != nil {
			t.Fatalf("unexpected task error: %v", err)
		}
		tasks[i] = task
		if err := q.Enqueue(task); err != nil {
			t.Fatalf("enqueue %d failed: %v", i, err)
		}
	}

	if q.Len() != n {
		t.Errorf("got Len() = %d, want %d", q.Len(), n)
	}

	for i := 0; i < n; i++ {
		got, err := q.Dequeue()
		if err != nil {
			t.Fatalf("dequeue %d failed: %v", i, err)
		}
		if got != tasks[i] {
			t.Errorf("task %d: FIFO violated! got %v, want %v", i, got, tasks[i])
		}
	}

	if q.Len() != 0 {
		t.Errorf("expected empty queue, got Len() = %d", q.Len())
	}
}

// 9. TestEmptyQueueBehavior
func TestEmptyQueueBehavior(t *testing.T) {
	q, err := NewWriteQueue(5)
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}

	// Non-blocking TryDequeue must return ErrQueueEmpty
	_, err = q.TryDequeue()
	if !stdErrors.Is(err, errors.ErrQueueEmpty) {
		t.Errorf("TryDequeue returned %v, want %v", err, errors.ErrQueueEmpty)
	}

	// Blocking Dequeue should block until task arrives
	dequeuedCh := make(chan *WriteTask, 1)
	go func() {
		item, _ := q.Dequeue()
		dequeuedCh <- item
	}()

	select {
	case <-dequeuedCh:
		t.Fatal("Dequeue should have blocked on empty queue")
	case <-time.After(20 * time.Millisecond):
	}

	task, _ := NewWriteTask(makeValidPutRecord("k", "v", 1))
	if err := q.Enqueue(task); err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}

	select {
	case got := <-dequeuedCh:
		if got != task {
			t.Errorf("got %v, want %v", got, task)
		}
	case <-time.After(time.Second):
		t.Fatal("Dequeue did not wake up after Enqueue")
	}
}

// 10. TestQueueCapacityBehaviorBounded & Adversarial: queue capacity overflow
func TestQueueCapacityBehaviorBounded(t *testing.T) {
	const cap = 3
	q, err := NewWriteQueue(cap)
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}

	for i := 0; i < cap; i++ {
		task, _ := NewWriteTask(makeValidPutRecord(fmt.Sprintf("k%d", i), "v", uint64(i+1)))
		if err := q.TryEnqueue(task); err != nil {
			t.Fatalf("TryEnqueue %d failed: %v", i, err)
		}
	}

	// Queue is now full. TryEnqueue must return ErrQueueFull immediately
	overflowTask, _ := NewWriteTask(makeValidPutRecord("overflow", "v", 99))
	err = q.TryEnqueue(overflowTask)
	if !stdErrors.Is(err, errors.ErrQueueFull) {
		t.Errorf("TryEnqueue on full queue returned %v, want %v", err, errors.ErrQueueFull)
	}

	// Blocking Enqueue should block until space is freed
	enqueuedCh := make(chan error, 1)
	go func() {
		enqueuedCh <- q.Enqueue(overflowTask)
	}()

	select {
	case <-enqueuedCh:
		t.Fatal("Enqueue should have blocked on full queue")
	case <-time.After(20 * time.Millisecond):
	}

	// Dequeue one item to create room
	_, err = q.Dequeue()
	if err != nil {
		t.Fatalf("Dequeue failed: %v", err)
	}

	select {
	case err := <-enqueuedCh:
		if err != nil {
			t.Errorf("Enqueue returned unexpected error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Enqueue did not unblock after Dequeue")
	}
}

// 11. TestEnqueueAfterClose
func TestEnqueueAfterClose(t *testing.T) {
	q, err := NewWriteQueue(5)
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}

	if err := q.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	task, _ := NewWriteTask(makeValidPutRecord("k", "v", 1))

	err = q.Enqueue(task)
	if !stdErrors.Is(err, errors.ErrQueueClosed) {
		t.Errorf("Enqueue after Close returned %v, want %v", err, errors.ErrQueueClosed)
	}

	task2, _ := NewWriteTask(makeValidPutRecord("k2", "v", 2))
	err = q.TryEnqueue(task2)
	if !stdErrors.Is(err, errors.ErrQueueClosed) {
		t.Errorf("TryEnqueue after Close returned %v, want %v", err, errors.ErrQueueClosed)
	}
}

// 12. TestCloseWithQueuedTasks (Graceful drain)
func TestCloseWithQueuedTasks(t *testing.T) {
	q, err := NewWriteQueue(5)
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}

	t1, _ := NewWriteTask(makeValidPutRecord("k1", "v", 1))
	t2, _ := NewWriteTask(makeValidPutRecord("k2", "v", 2))

	_ = q.Enqueue(t1)
	_ = q.Enqueue(t2)

	// Close the queue
	if err := q.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Existing tasks must still be drainable
	got1, err := q.Dequeue()
	if err != nil || got1 != t1 {
		t.Fatalf("got %v, err %v; want %v", got1, err, t1)
	}

	got2, err := q.Dequeue()
	if err != nil || got2 != t2 {
		t.Fatalf("got %v, err %v; want %v", got2, err, t2)
	}

	// Now that queue is empty and closed, Dequeue must return ErrQueueClosed
	got3, err := q.Dequeue()
	if got3 != nil || !stdErrors.Is(err, errors.ErrQueueClosed) {
		t.Errorf("Dequeue returned (%v, %v), want (nil, %v)", got3, err, errors.ErrQueueClosed)
	}

	_, err = q.TryDequeue()
	if !stdErrors.Is(err, errors.ErrQueueClosed) {
		t.Errorf("TryDequeue returned %v, want %v", err, errors.ErrQueueClosed)
	}
}

// 13. TestCloseWithErrorNoTaskStarvation
func TestCloseWithErrorNoTaskStarvation(t *testing.T) {
	q, err := NewWriteQueue(5)
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}

	const n = 3
	tasks := make([]*WriteTask, n)
	for i := 0; i < n; i++ {
		task, _ := NewWriteTask(makeValidPutRecord(fmt.Sprintf("k%d", i), "v", uint64(i+1)))
		tasks[i] = task
		_ = q.Enqueue(task)
	}

	abortErr := stdErrors.New("fatal WAL disk error")
	if err := q.CloseWithError(abortErr); err != nil {
		t.Fatalf("CloseWithError failed: %v", err)
	}

	// Every queued task must have been completed with abortErr
	for i, task := range tasks {
		if !task.IsDone() {
			t.Errorf("task %d was not completed by CloseWithError", i)
		}
		if !stdErrors.Is(task.Err(), abortErr) {
			t.Errorf("task %d: got err %v, want %v", i, task.Err(), abortErr)
		}
	}

	if q.Len() != 0 {
		t.Errorf("expected queue to be empty after CloseWithError, got Len() = %d", q.Len())
	}
}

// 14. TestConcurrentProducers & 15. TestConcurrentConsumerAndProducers & 17. TestExactlyOnceDequeue
func TestConcurrentConsumerAndProducers(t *testing.T) {
	const producers = 10
	const itemsPerProducer = 100
	const totalItems = producers * itemsPerProducer

	q, err := NewWriteQueue(32) // smaller than total to exercise backpressure
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}

	var startWg sync.WaitGroup
	startWg.Add(1)

	var producersWg sync.WaitGroup
	producersWg.Add(producers)

	for p := 0; p < producers; p++ {
		go func(prodID int) {
			defer producersWg.Done()
			startWg.Wait() // all producers start simultaneously

			for i := 0; i < itemsPerProducer; i++ {
				rec := makeValidPutRecord(fmt.Sprintf("k-%d-%d", prodID, i), "v", uint64(prodID*itemsPerProducer+i+1))
				task, err := NewWriteTask(rec)
				if err != nil {
					t.Errorf("producer %d: task error: %v", prodID, err)
					return
				}
				if err := q.Enqueue(task); err != nil {
					t.Errorf("producer %d: enqueue error: %v", prodID, err)
					return
				}
			}
		}(p)
	}

	var dequeuedCount int64
	seenTasks := make(map[*WriteTask]bool)
	var seenMu sync.Mutex

	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for {
			task, err := q.Dequeue()
			if err != nil {
				if stdErrors.Is(err, errors.ErrQueueClosed) {
					return
				}
				t.Errorf("unexpected Dequeue error: %v", err)
				return
			}
			seenMu.Lock()
			if seenTasks[task] {
				t.Errorf("task dequeued twice: %v", task)
			}
			seenTasks[task] = true
			seenMu.Unlock()

			atomic.AddInt64(&dequeuedCount, 1)
			_ = task.Complete(nil) // simulate durability sync
		}
	}()

	startWg.Done() // release all producers
	producersWg.Wait()

	// Graceful close after producers finish
	if err := q.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	<-consumerDone

	if dequeuedCount != totalItems {
		t.Errorf("dequeued %d items, want %d", dequeuedCount, totalItems)
	}
}

// 16. TestConcurrentCloseAndEnqueue
func TestConcurrentCloseAndEnqueue(t *testing.T) {
	q, err := NewWriteQueue(16)
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines + 1)

	// Close after short delay
	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		_ = q.Close()
	}()

	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				task, err := NewWriteTask(makeValidPutRecord(fmt.Sprintf("k-%d-%d", id, i), "v", uint64(i+1)))
				if err != nil {
					return
				}
				err = q.Enqueue(task)
				if err != nil && !stdErrors.Is(err, errors.ErrQueueClosed) {
					t.Errorf("unexpected error: %v", err)
				}
			}
		}(g)
	}

	wg.Wait()
}

// 19. TestLargePayloadTask (4 MiB boundary)
func TestLargePayloadTask(t *testing.T) {
	largeVal := make([]byte, binary.MaxValueLen)
	largeVal[0] = 0xAA
	largeVal[len(largeVal)-1] = 0xBB

	rec := Record{
		Type:      RecordTypePut,
		Key:       []byte("large-key"),
		Value:     largeVal,
		SeqNum:    100,
		Timestamp: 1000,
	}

	task, err := NewWriteTask(rec)
	if err != nil {
		t.Fatalf("unexpected error creating large payload task: %v", err)
	}

	got := task.Record()
	if len(got.Value) != binary.MaxValueLen {
		t.Fatalf("got val len %d, want %d", len(got.Value), binary.MaxValueLen)
	}
	if got.Value[0] != 0xAA || got.Value[len(got.Value)-1] != 0xBB {
		t.Errorf("payload corruption in large value")
	}

	// 4 MiB + 1 must be rejected by NewWriteTask
	oversizedVal := make([]byte, binary.MaxValueLen+1)
	oversizedRec := Record{
		Type:      RecordTypePut,
		Key:       []byte("k"),
		Value:     oversizedVal,
		SeqNum:    101,
		Timestamp: 1001,
	}
	_, err = NewWriteTask(oversizedRec)
	if !stdErrors.Is(err, errors.ErrValueTooLarge) {
		t.Errorf("NewWriteTask with oversized val returned %v, want ErrValueTooLarge", err)
	}
}

// 20. TestZeroAndNilPayloadHandling (DELETE tombstone, BATCH markers)
func TestZeroAndNilPayloadHandling(t *testing.T) {
	// DELETE tombstone
	delRec := Record{
		Type:      RecordTypeDelete,
		Key:       []byte("del-key"),
		SeqNum:    10,
		Timestamp: 100,
	}
	delTask, err := NewWriteTask(delRec)
	if err != nil {
		t.Fatalf("unexpected error on valid DELETE: %v", err)
	}
	if len(delTask.Record().Value) != 0 {
		t.Errorf("DELETE task must have empty value, got %d", len(delTask.Record().Value))
	}

	// BATCH_START marker
	startRec := Record{
		Type:      RecordTypeBatchStart,
		SeqNum:    11,
		Timestamp: 101,
	}
	startTask, err := NewWriteTask(startRec)
	if err != nil {
		t.Fatalf("unexpected error on valid BATCH_START: %v", err)
	}
	if len(startTask.Record().Key) != 0 || len(startTask.Record().Value) != 0 {
		t.Errorf("BATCH_START task must have zero key and value")
	}

	// BATCH_COMMIT marker
	commitRec := Record{
		Type:      RecordTypeBatchCommit,
		SeqNum:    12,
		Timestamp: 102,
	}
	commitTask, err := NewWriteTask(commitRec)
	if err != nil {
		t.Fatalf("unexpected error on valid BATCH_COMMIT: %v", err)
	}
	if len(commitTask.Record().Key) != 0 || len(commitTask.Record().Value) != 0 {
		t.Errorf("BATCH_COMMIT task must have zero key and value")
	}
}

// 21. TestInvalidRecordRejection
func TestInvalidRecordRejection(t *testing.T) {
	// Empty key for PUT
	_, err := NewWriteTask(Record{Type: RecordTypePut, Key: nil, Value: []byte("v"), SeqNum: 1})
	if !stdErrors.Is(err, errors.ErrEmptyKey) {
		t.Errorf("empty key returned %v, want ErrEmptyKey", err)
	}

	// Invalid record type (0)
	_, err = NewWriteTask(Record{Type: RecordType(0), Key: []byte("k"), Value: []byte("v"), SeqNum: 1})
	if !stdErrors.Is(err, errors.ErrInvalidRecordType) {
		t.Errorf("invalid record type returned %v, want ErrInvalidRecordType", err)
	}

	// DELETE with non-empty value
	_, err = NewWriteTask(Record{Type: RecordTypeDelete, Key: []byte("k"), Value: []byte("v"), SeqNum: 1})
	if !stdErrors.Is(err, errors.ErrInvalidRecordPayload) {
		t.Errorf("DELETE with value returned %v, want ErrInvalidRecordPayload", err)
	}

	// BATCH_START with key
	_, err = NewWriteTask(Record{Type: RecordTypeBatchStart, Key: []byte("k"), SeqNum: 1})
	if !stdErrors.Is(err, errors.ErrInvalidRecordPayload) {
		t.Errorf("BATCH_START with key returned %v, want ErrInvalidRecordPayload", err)
	}
}

// Adversarial: task enqueued twice
func TestTaskEnqueuedTwice(t *testing.T) {
	q, err := NewWriteQueue(5)
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}

	task, _ := NewWriteTask(makeValidPutRecord("k", "v", 1))
	if err := q.Enqueue(task); err != nil {
		t.Fatalf("first enqueue failed: %v", err)
	}

	// Second enqueue of same task must return ErrTaskAlreadyEnqueued
	err = q.Enqueue(task)
	if !stdErrors.Is(err, errors.ErrTaskAlreadyEnqueued) {
		t.Errorf("second enqueue returned %v, want %v", err, errors.ErrTaskAlreadyEnqueued)
	}

	err = q.TryEnqueue(task)
	if !stdErrors.Is(err, errors.ErrTaskAlreadyEnqueued) {
		t.Errorf("second TryEnqueue returned %v, want %v", err, errors.ErrTaskAlreadyEnqueued)
	}
}

// Adversarial: enqueue completed task
func TestEnqueueCompletedTask(t *testing.T) {
	q, err := NewWriteQueue(5)
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}

	task, _ := NewWriteTask(makeValidPutRecord("k", "v", 1))
	_ = task.Complete(nil)

	err = q.Enqueue(task)
	if !stdErrors.Is(err, errors.ErrTaskAlreadyCompleted) {
		t.Errorf("enqueue completed task returned %v, want %v", err, errors.ErrTaskAlreadyCompleted)
	}

	err = q.TryEnqueue(task)
	if !stdErrors.Is(err, errors.ErrTaskAlreadyCompleted) {
		t.Errorf("TryEnqueue completed task returned %v, want %v", err, errors.ErrTaskAlreadyCompleted)
	}
}

// Adversarial: queue capacity set to invalid value
func TestInvalidQueueCapacity(t *testing.T) {
	_, err := NewWriteQueue(0)
	if !stdErrors.Is(err, errors.ErrInvalidQueueCapacity) {
		t.Errorf("NewWriteQueue(0) returned %v, want ErrInvalidQueueCapacity", err)
	}

	_, err = NewWriteQueue(-10)
	if !stdErrors.Is(err, errors.ErrInvalidQueueCapacity) {
		t.Errorf("NewWriteQueue(-10) returned %v, want ErrInvalidQueueCapacity", err)
	}
}

// Adversarial: nil task handling
func TestNilTaskHandling(t *testing.T) {
	q, _ := NewWriteQueue(5)

	if err := q.Enqueue(nil); !stdErrors.Is(err, errors.ErrNilTask) {
		t.Errorf("Enqueue(nil) returned %v, want ErrNilTask", err)
	}
	if err := q.TryEnqueue(nil); !stdErrors.Is(err, errors.ErrNilTask) {
		t.Errorf("TryEnqueue(nil) returned %v, want ErrNilTask", err)
	}

	var nilTask *WriteTask
	if err := nilTask.Wait(); !stdErrors.Is(err, errors.ErrNilTask) {
		t.Errorf("nilTask.Wait() returned %v, want ErrNilTask", err)
	}
	if err := nilTask.WaitContext(context.Background()); !stdErrors.Is(err, errors.ErrNilTask) {
		t.Errorf("nilTask.WaitContext() returned %v, want ErrNilTask", err)
	}
	if nilTask.Done() != nil {
		t.Errorf("nilTask.Done() returned %v, want nil", nilTask.Done())
	}
	if err := nilTask.Complete(nil); !stdErrors.Is(err, errors.ErrNilTask) {
		t.Errorf("nilTask.Complete() returned %v, want ErrNilTask", err)
	}
	if err := nilTask.Err(); !stdErrors.Is(err, errors.ErrNilTask) {
		t.Errorf("nilTask.Err() returned %v, want ErrNilTask", err)
	}
	if nilTask.IsDone() {
		t.Errorf("nilTask.IsDone() returned true, want false")
	}
	if !nilTask.Record().Equal(Record{}) {
		t.Errorf("nilTask.Record() returned non-zero record: %v", nilTask.Record())
	}
}

// Adversarial: zero-value task handling
func TestZeroValueTaskHandling(t *testing.T) {
	q, _ := NewWriteQueue(5)
	var zeroTask WriteTask

	if err := zeroTask.Wait(); !stdErrors.Is(err, errors.ErrNilTask) {
		t.Errorf("zeroTask.Wait() returned %v, want ErrNilTask", err)
	}
	if err := zeroTask.WaitContext(context.Background()); !stdErrors.Is(err, errors.ErrNilTask) {
		t.Errorf("zeroTask.WaitContext() returned %v, want ErrNilTask", err)
	}
	if zeroTask.Done() != nil {
		t.Errorf("zeroTask.Done() returned %v, want nil", zeroTask.Done())
	}
	if err := zeroTask.Complete(nil); !stdErrors.Is(err, errors.ErrNilTask) {
		t.Errorf("zeroTask.Complete() returned %v, want ErrNilTask", err)
	}
	if err := q.Enqueue(&zeroTask); !stdErrors.Is(err, errors.ErrNilTask) {
		t.Errorf("Enqueue(&zeroTask) returned %v, want ErrNilTask", err)
	}
}

// Adversarial: zero-value queue handling
func TestZeroValueQueueHandling(t *testing.T) {
	var zeroQueue WriteQueue
	task, _ := NewWriteTask(makeValidPutRecord("k", "v", 1))

	if err := zeroQueue.Enqueue(task); !stdErrors.Is(err, errors.ErrQueueClosed) {
		t.Errorf("zeroQueue.Enqueue() returned %v, want ErrQueueClosed", err)
	}
	if err := zeroQueue.TryEnqueue(task); !stdErrors.Is(err, errors.ErrQueueClosed) {
		t.Errorf("zeroQueue.TryEnqueue() returned %v, want ErrQueueClosed", err)
	}
	if _, err := zeroQueue.Dequeue(); !stdErrors.Is(err, errors.ErrQueueClosed) {
		t.Errorf("zeroQueue.Dequeue() returned %v, want ErrQueueClosed", err)
	}
	if _, err := zeroQueue.TryDequeue(); !stdErrors.Is(err, errors.ErrQueueClosed) {
		t.Errorf("zeroQueue.TryDequeue() returned %v, want ErrQueueClosed", err)
	}
	if err := zeroQueue.Close(); err != nil {
		t.Errorf("zeroQueue.Close() returned %v, want nil", err)
	}
	if err := zeroQueue.CloseWithError(nil); err != nil {
		t.Errorf("zeroQueue.CloseWithError() returned %v, want nil", err)
	}
	if zeroQueue.Len() != 0 {
		t.Errorf("zeroQueue.Len() = %d, want 0", zeroQueue.Len())
	}
	if zeroQueue.Cap() != 0 {
		t.Errorf("zeroQueue.Cap() = %d, want 0", zeroQueue.Cap())
	}
}

// WaitContext cancellation test
func TestWaitContextCancellation(t *testing.T) {
	task, err := NewWriteTask(makeValidPutRecord("k", "v", 1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err = task.WaitContext(ctx)
	if !stdErrors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WaitContext returned %v, want %v", err, context.DeadlineExceeded)
	}

	// Task should still be incomplete
	if task.IsDone() {
		t.Error("task should not be done after context cancellation")
	}

	// Completing the task later works normally
	if err := task.Complete(nil); err != nil {
		t.Fatalf("Complete failed: %v", err)
	}

	// Waiting on the already-completed task succeeds
	if err := task.Wait(); err != nil {
		t.Errorf("Wait returned %v, want nil", err)
	}
}

// 23. TestDeterministicBehaviorUnderRepeatedRuns
func TestDeterministicBehaviorUnderRepeatedRuns(t *testing.T) {
	for run := 0; run < 10; run++ {
		q, err := NewWriteQueue(16)
		if err != nil {
			t.Fatalf("run %d: NewWriteQueue failed: %v", run, err)
		}

		for i := 0; i < 16; i++ {
			task, err := NewWriteTask(makeValidPutRecord(fmt.Sprintf("k%d", i), "v", uint64(i+1)))
			if err != nil {
				t.Fatalf("run %d task %d failed: %v", run, i, err)
			}
			if err := q.TryEnqueue(task); err != nil {
				t.Fatalf("run %d enqueue %d failed: %v", run, i, err)
			}
		}

		for i := 0; i < 16; i++ {
			task, err := q.TryDequeue()
			if err != nil {
				t.Fatalf("run %d dequeue %d failed: %v", run, i, err)
			}
			if uint64(task.Record().SeqNum) != uint64(i+1) {
				t.Fatalf("run %d: expected seq %d, got %d", run, i+1, task.Record().SeqNum)
			}
		}

		if q.Len() != 0 {
			t.Fatalf("run %d: expected empty queue, got %d", run, q.Len())
		}
	}
}

// 1. TestTaskRecordDoesNotExposeInternalKey
func TestTaskRecordDoesNotExposeInternalKey(t *testing.T) {
	key := []byte("original-key")
	task, err := NewWriteTask(makeValidPutRecord(string(key), "val", 1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r := task.Record()
	if !bytes.Equal(r.Key, key) {
		t.Fatalf("got key %q, want %q", r.Key, key)
	}

	// Mutate returned copy
	r.Key[0] = 'X'

	// Second Record() call must observe original bytes
	r2 := task.Record()
	if string(r2.Key) != "original-key" {
		t.Fatalf("internal key mutated! got %q, want %q", r2.Key, "original-key")
	}
}

// 2. TestTaskRecordDoesNotExposeInternalValue
func TestTaskRecordDoesNotExposeInternalValue(t *testing.T) {
	val := []byte("original-val")
	task, err := NewWriteTask(makeValidPutRecord("key", string(val), 1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r := task.Record()
	if !bytes.Equal(r.Value, val) {
		t.Fatalf("got val %q, want %q", r.Value, val)
	}

	// Mutate returned copy
	r.Value[0] = 'Z'

	// Second Record() call must observe original bytes
	r2 := task.Record()
	if string(r2.Value) != "original-val" {
		t.Fatalf("internal value mutated! got %q, want %q", r2.Value, "original-val")
	}
}

// 3. TestTaskRecordMetadataPreserved
func TestTaskRecordMetadataPreserved(t *testing.T) {
	rec := Record{
		CRC:       0x12345678,
		Type:      RecordTypePut,
		SeqNum:    999,
		Timestamp: 123456789,
		Key:       []byte("meta-key"),
		Value:     []byte("meta-val"),
	}

	task, err := NewWriteTask(rec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r := task.Record()
	if r.CRC != rec.CRC {
		t.Errorf("got CRC 0x%08x, want 0x%08x", r.CRC, rec.CRC)
	}
	if r.Type != rec.Type {
		t.Errorf("got Type %v, want %v", r.Type, rec.Type)
	}
	if r.SeqNum != rec.SeqNum {
		t.Errorf("got SeqNum %d, want %d", r.SeqNum, rec.SeqNum)
	}
	if r.Timestamp != rec.Timestamp {
		t.Errorf("got Timestamp %d, want %d", r.Timestamp, rec.Timestamp)
	}
	if !bytes.Equal(r.Key, rec.Key) {
		t.Errorf("got Key %q, want %q", r.Key, rec.Key)
	}
	if !bytes.Equal(r.Value, rec.Value) {
		t.Errorf("got Value %q, want %q", r.Value, rec.Value)
	}
}

// 4. TestTaskRecordNilAndEmptyPayload
func TestTaskRecordNilAndEmptyPayload(t *testing.T) {
	// DELETE record: key non-empty, value nil/empty
	delTask, err := NewWriteTask(Record{
		Type:      RecordTypeDelete,
		Key:       []byte("del-key"),
		SeqNum:    10,
		Timestamp: 100,
	})
	if err != nil {
		t.Fatalf("unexpected error on DELETE: %v", err)
	}
	delRec := delTask.Record()
	if string(delRec.Key) != "del-key" {
		t.Errorf("got del key %q, want %q", delRec.Key, "del-key")
	}
	if len(delRec.Value) != 0 {
		t.Errorf("expected empty value for DELETE, got %d bytes", len(delRec.Value))
	}

	// BATCH_START record: key nil, value nil
	startTask, err := NewWriteTask(Record{
		Type:      RecordTypeBatchStart,
		SeqNum:    11,
		Timestamp: 101,
	})
	if err != nil {
		t.Fatalf("unexpected error on BATCH_START: %v", err)
	}
	startRec := startTask.Record()
	if len(startRec.Key) != 0 || len(startRec.Value) != 0 {
		t.Errorf("expected empty key/val for BATCH_START, got key=%d, val=%d", len(startRec.Key), len(startRec.Value))
	}

	// BATCH_COMMIT record: key nil, value nil
	commitTask, err := NewWriteTask(Record{
		Type:      RecordTypeBatchCommit,
		SeqNum:    12,
		Timestamp: 102,
	})
	if err != nil {
		t.Fatalf("unexpected error on BATCH_COMMIT: %v", err)
	}
	commitRec := commitTask.Record()
	if len(commitRec.Key) != 0 || len(commitRec.Value) != 0 {
		t.Errorf("expected empty key/val for BATCH_COMMIT, got key=%d, val=%d", len(commitRec.Key), len(commitRec.Value))
	}

	// PUT with empty value: key non-empty, value empty
	putEmptyValTask, err := NewWriteTask(Record{
		Type:      RecordTypePut,
		Key:       []byte("empty-val-key"),
		Value:     []byte{},
		SeqNum:    13,
		Timestamp: 103,
	})
	if err != nil {
		t.Fatalf("unexpected error on PUT empty value: %v", err)
	}
	putEmptyRec := putEmptyValTask.Record()
	if string(putEmptyRec.Key) != "empty-val-key" {
		t.Errorf("got key %q, want %q", putEmptyRec.Key, "empty-val-key")
	}
	if len(putEmptyRec.Value) != 0 {
		t.Errorf("expected empty value, got %d bytes", len(putEmptyRec.Value))
	}
}

// 5. TestTaskRecordRepeatedIsolation
func TestTaskRecordRepeatedIsolation(t *testing.T) {
	task, err := NewWriteTask(makeValidPutRecord("shared-key", "shared-val", 1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r1 := task.Record()
	r2 := task.Record()
	r3 := task.Record()

	// Mutate r1
	r1.Key[0] = '1'
	r1.Value[0] = '1'

	// r2 and r3 must be unchanged
	if string(r2.Key) != "shared-key" || string(r2.Value) != "shared-val" {
		t.Errorf("r2 corrupted by r1 mutation: key=%q, val=%q", r2.Key, r2.Value)
	}
	if string(r3.Key) != "shared-key" || string(r3.Value) != "shared-val" {
		t.Errorf("r3 corrupted by r1 mutation: key=%q, val=%q", r3.Key, r3.Value)
	}

	// Mutate r2
	r2.Key[0] = '2'
	r2.Value[0] = '2'

	// r3 must remain unchanged
	if string(r3.Key) != "shared-key" || string(r3.Value) != "shared-val" {
		t.Errorf("r3 corrupted by r2 mutation: key=%q, val=%q", r3.Key, r3.Value)
	}

	// A fresh call must still have pristine data
	rFresh := task.Record()
	if string(rFresh.Key) != "shared-key" || string(rFresh.Value) != "shared-val" {
		t.Errorf("task internal corrupted: key=%q, val=%q", rFresh.Key, rFresh.Value)
	}
}

// 6. TestConcurrentRecordAccess
func TestConcurrentRecordAccess(t *testing.T) {
	task, err := NewWriteTask(makeValidPutRecord("concurrent-key", "concurrent-val", 500))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	const goroutines = 20
	const iterations = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				r := task.Record()
				if string(r.Key) != "concurrent-key" {
					t.Errorf("goroutine %d iter %d saw corrupted key: %q", id, i, r.Key)
					return
				}
				if string(r.Value) != "concurrent-val" {
					t.Errorf("goroutine %d iter %d saw corrupted val: %q", id, i, r.Value)
					return
				}
				// Mutate only our returned copy
				r.Key[0] = byte('A' + (id % 26))
				r.Value[0] = byte('0' + (i % 10))
			}
		}(g)
	}

	wg.Wait()

	// Task must still retain original data
	finalRec := task.Record()
	if string(finalRec.Key) != "concurrent-key" {
		t.Errorf("final key corrupted: got %q, want %q", finalRec.Key, "concurrent-key")
	}
	if string(finalRec.Value) != "concurrent-val" {
		t.Errorf("final val corrupted: got %q, want %q", finalRec.Value, "concurrent-val")
	}
}

// 7. TestTaskRecordMutationAgainstCompletion
func TestTaskRecordMutationAgainstCompletion(t *testing.T) {
	task, err := NewWriteTask(makeValidPutRecord("race-key", "race-val", 777))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine 1: repeatedly calls Record() and mutates returned slices
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			r := task.Record()
			if string(r.Key) != "race-key" {
				t.Errorf("iter %d corrupted key: %q", i, r.Key)
			}
			if len(r.Key) > 0 {
				r.Key[0] = 'M'
			}
			if len(r.Value) > 0 {
				r.Value[0] = 'V'
			}
		}
	}()

	// Goroutine 2: completes the task after brief churn
	go func() {
		defer wg.Done()
		time.Sleep(2 * time.Millisecond)
		_ = task.Complete(nil)
	}()

	wg.Wait()

	if err := task.Wait(); err != nil {
		t.Errorf("Wait returned unexpected error: %v", err)
	}

	finalRec := task.Record()
	if string(finalRec.Key) != "race-key" || string(finalRec.Value) != "race-val" {
		t.Errorf("task corrupted during concurrent complete: key=%q, val=%q", finalRec.Key, finalRec.Value)
	}
}

// 8. TestTaskRawRecordInternalAccessor
func TestTaskRawRecordInternalAccessor(t *testing.T) {
	task, err := NewWriteTask(makeValidPutRecord("raw-key", "raw-val", 42))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// rawRecord returns internal storage without copy
	raw1 := task.rawRecord()
	raw2 := task.rawRecord()

	// Proving rawRecord refers to task-owned storage:
	// The backing array pointer of raw1.Key must match raw2.Key
	if &raw1.Key[0] != &raw2.Key[0] {
		t.Errorf("rawRecord must return internal storage without copying")
	}
	if &raw1.Value[0] != &raw2.Value[0] {
		t.Errorf("rawRecord must return internal storage without copying")
	}

	// Record() must return independent storage:
	pub := task.Record()
	if &pub.Key[0] == &raw1.Key[0] {
		t.Errorf("public Record() must NOT share backing array with rawRecord()")
	}
	if &pub.Value[0] == &raw1.Value[0] {
		t.Errorf("public Record() must NOT share backing array with rawRecord()")
	}

	// Mutating public Record does not affect rawRecord
	pub.Key[0] = 'X'
	pub.Value[0] = 'Y'

	rawAfter := task.rawRecord()
	if string(rawAfter.Key) != "raw-key" {
		t.Errorf("rawRecord Key corrupted by public Record mutation: got %q, want %q", rawAfter.Key, "raw-key")
	}
	if string(rawAfter.Value) != "raw-val" {
		t.Errorf("rawRecord Value corrupted by public Record mutation: got %q, want %q", rawAfter.Value, "raw-val")
	}

	// Nil task safety on rawRecord
	var nilTask *WriteTask
	if !nilTask.rawRecord().Equal(Record{}) {
		t.Errorf("nilTask.rawRecord() must return zero-value Record")
	}
}
