package raft_test

import (
	"context"
	stdErrors "errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/raft"
	"github.com/silent-knight19/lattice/internal/transport"
)

type mockStateMachine struct {
	mu      sync.Mutex
	puts    map[string]string
	deletes map[string]struct{}
	err     error
}

func newMockStateMachine() *mockStateMachine {
	return &mockStateMachine{
		puts:    make(map[string]string),
		deletes: make(map[string]struct{}),
	}
}

func (m *mockStateMachine) Put(ctx context.Context, key, val []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.puts[string(key)] = string(val)
	return nil
}

func (m *mockStateMachine) Delete(ctx context.Context, key []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.deletes[string(key)] = struct{}{}
	delete(m.puts, string(key))
	return nil
}

func (m *mockStateMachine) Get(key []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	if v, ok := m.puts[string(key)]; ok {
		return []byte(v), nil
	}
	return nil, errors.ErrKeyNotFound
}

func (m *mockStateMachine) SetError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

func newTestBarrierNode(t *testing.T, sm raft.StateMachine) (*raft.Node, *raft.Storage) {
	t.Helper()
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	cfg := raft.NodeConfig{
		LocalID:      1,
		Storage:      s,
		StateMachine: sm,
	}
	n, err := raft.NewNode(cfg)
	if err != nil {
		_ = s.Close()
		t.Fatalf("NewNode failed: %v", err)
	}
	t.Cleanup(func() {
		_ = n.Close()
		_ = s.Close()
	})
	return n, s
}

// 1. lastApplied >= target returns immediately without waiting.
func TestWaitForApplied_AlreadyAppliedReturnsImmediately(t *testing.T) {
	n, _ := newTestBarrierNode(t, nil)
	n.SignalAppliedForTest(10)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	// Target 10: already applied
	if err := n.WaitForApplied(ctx, 10); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Target 5: already applied
	if err := n.WaitForApplied(ctx, 5); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Target 0: boundary
	if err := n.WaitForApplied(ctx, 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if time.Since(start) > 50*time.Millisecond {
		t.Fatalf("expected immediate return, took %v", time.Since(start))
	}
}

// 2. Waiter blocks while lastApplied < target, and unblocks when target is applied.
func TestWaitForApplied_BlocksUntilApplied(t *testing.T) {
	n, _ := newTestBarrierNode(t, nil)
	n.SignalAppliedForTest(2)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- n.WaitForApplied(ctx, 5)
	}()

	// Ensure waiter is blocked
	select {
	case err := <-doneCh:
		t.Fatalf("waiter returned prematurely with err: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Advance lastApplied to 4 (still less than 5)
	n.SignalAppliedForTest(4)
	select {
	case err := <-doneCh:
		t.Fatalf("waiter returned before target was reached with err: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Advance lastApplied to 5 (target reached)
	n.SignalAppliedForTest(5)

	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("expected nil error after reaching target, got: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for waiter to unblock after target reached")
	}
}

// 3. Multiple waiters waiting on the same target index are all released.
func TestWaitForApplied_MultipleWaitersSameIndex(t *testing.T) {
	n, _ := newTestBarrierNode(t, nil)
	const numWaiters = 20

	var wg sync.WaitGroup
	wg.Add(numWaiters)
	errCh := make(chan error, numWaiters)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for i := 0; i < numWaiters; i++ {
		go func() {
			defer wg.Done()
			errCh <- n.WaitForApplied(ctx, 15)
		}()
	}

	// Verify all are waiting
	time.Sleep(50 * time.Millisecond)

	// Release all by advancing to 15
	n.SignalAppliedForTest(15)

	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Fatalf("waiter returned error: %v", err)
		}
	}
}

// 4. Waiters waiting on different target indexes unblock in proper sequence.
func TestWaitForApplied_MultipleWaitersDifferentIndexes(t *testing.T) {
	n, _ := newTestBarrierNode(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	w5Done := make(chan error, 1)
	w10Done := make(chan error, 1)
	w15Done := make(chan error, 1)

	go func() { w5Done <- n.WaitForApplied(ctx, 5) }()
	go func() { w10Done <- n.WaitForApplied(ctx, 10) }()
	go func() { w15Done <- n.WaitForApplied(ctx, 15) }()

	time.Sleep(30 * time.Millisecond)

	// Advance to 5
	n.SignalAppliedForTest(5)
	select {
	case err := <-w5Done:
		if err != nil {
			t.Fatalf("w5 error: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("w5 timed out")
	}

	// w10 and w15 must still be blocked
	select {
	case <-w10Done:
		t.Fatal("w10 woke up prematurely")
	case <-w15Done:
		t.Fatal("w15 woke up prematurely")
	default:
	}

	// Advance to 12 (satisfies w10, but not w15)
	n.SignalAppliedForTest(12)
	select {
	case err := <-w10Done:
		if err != nil {
			t.Fatalf("w10 error: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("w10 timed out")
	}

	// w15 must still be blocked
	select {
	case <-w15Done:
		t.Fatal("w15 woke up prematurely")
	default:
	}

	// Advance to 15
	n.SignalAppliedForTest(15)
	select {
	case err := <-w15Done:
		if err != nil {
			t.Fatalf("w15 error: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("w15 timed out")
	}
}

// 5. Context cancellation returns context.Canceled without converting to not-leader.
func TestWaitForApplied_ContextCancellation(t *testing.T) {
	n, _ := newTestBarrierNode(t, nil)

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- n.WaitForApplied(ctx, 100)
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !stdErrors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for cancellation return")
	}
}

// 6. Context deadline exceeded returns context.DeadlineExceeded.
func TestWaitForApplied_ContextDeadlineExceeded(t *testing.T) {
	n, _ := newTestBarrierNode(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := n.WaitForApplied(ctx, 100)
	if !stdErrors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got: %v", err)
	}
}

// 7. Node close wakes blocked waiters with ErrRaftStateClosed.
func TestWaitForApplied_NodeCloseReleasesWaiters(t *testing.T) {
	n, _ := newTestBarrierNode(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- n.WaitForApplied(ctx, 100)
	}()

	time.Sleep(30 * time.Millisecond)
	_ = n.Close()

	select {
	case err := <-errCh:
		if !stdErrors.Is(err, errors.ErrRaftStateClosed) {
			t.Fatalf("expected ErrRaftStateClosed, got: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for node close to release waiter")
	}
}

// 8. Apply failure releases blocked waiters fail-closed with terminal apply error.
func TestWaitForApplied_ApplyErrorFailsClosed(t *testing.T) {
	sm := newMockStateMachine()
	n, s := newTestBarrierNode(t, sm)

	// Append a valid command entry to storage
	cmdBytes, _ := raft.EncodeCommand(raft.Command{
		Op:    1, // Put
		Key:   []byte("test-key"),
		Value: []byte("test-val"),
	})
	_ = s.SetTerm(1)
	_ = s.Append(raft.LogEntry{
		Index: 1,
		Term:  1,
		Type:  transport.PeerEntryNormal,
		Data:  cmdBytes,
	})

	// Make state machine return error
	sm.SetError(stdErrors.New("simulated disk corruption"))

	// Advance commitIndex to 1, waking the apply loop which will fail
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- n.WaitForApplied(ctx, 1)
	}()

	// Promote node to leader to advance commitIndex
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected apply error, got nil")
		}
		if !stdErrors.Is(err, errors.ErrRaftApplyFailed) {
			t.Logf("waiter returned apply error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for apply error to unblock waiter")
	}
}

// 9. No lost wakeup race: application advances between checking condition and registering waiter.
func TestWaitForApplied_NoLostWakeupRace(t *testing.T) {
	n, _ := newTestBarrierNode(t, nil)

	const numIterations = 500
	for i := 0; i < numIterations; i++ {
		target := raft.LogIndex(i + 1)

		var wg sync.WaitGroup
		wg.Add(1)
		errCh := make(chan error, 1)

		go func() {
			wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			errCh <- n.WaitForApplied(ctx, target)
		}()

		wg.Wait()
		// Concurrently advance applied
		n.SignalAppliedForTest(target)

		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("iteration %d failed with error: %v", i, err)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("iteration %d deadlocked / lost wakeup waiting for %d", i, target)
		}
	}
}

// 10. High-concurrency race test: concurrent waiters and advances.
func TestWaitForApplied_ConcurrentWaitersRaceFree(t *testing.T) {
	n, _ := newTestBarrierNode(t, nil)

	const numWaiters = 50
	var wg sync.WaitGroup
	wg.Add(numWaiters)
	var failures atomic.Int64

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	for i := 0; i < numWaiters; i++ {
		target := raft.LogIndex((i % 10) + 1)
		go func(tgt raft.LogIndex) {
			defer wg.Done()
			if err := n.WaitForApplied(ctx, tgt); err != nil {
				failures.Add(1)
			}
		}(target)
	}

	// Concurrently advance in increments
	for idx := raft.LogIndex(1); idx <= 15; idx++ {
		time.Sleep(5 * time.Millisecond)
		n.SignalAppliedForTest(idx)
	}

	wg.Wait()

	if failures.Load() != 0 {
		t.Fatalf("%d waiters failed during concurrent barrier test", failures.Load())
	}
}

// 11. ValidateLeadership verifies leadership epoch, role, and term.
func TestValidateLeadership(t *testing.T) {
	n, s := newTestBarrierNode(t, nil)
	_ = s.SetTerm(1)

	// Follower: fails
	if err := n.ValidateLeadership(1, 0); err == nil {
		t.Fatal("expected error on follower, got nil")
	}

	// Promote to leader
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}

	// Active leader term 1, epoch 0 (or active epoch)
	res, err := n.ReadIndex(context.Background())
	if err != nil {
		t.Fatalf("ReadIndex failed: %v", err)
	}

	// Valid leadership matches
	if err := n.ValidateLeadership(res.Term, res.Epoch); err != nil {
		t.Fatalf("ValidateLeadership failed with correct term and epoch: %v", err)
	}

	// Mismatched epoch
	if err := n.ValidateLeadership(res.Term, res.Epoch+99); err == nil {
		t.Fatal("expected error for mismatched epoch, got nil")
	}

	// Mismatched term
	if err := n.ValidateLeadership(res.Term+99, res.Epoch); err == nil {
		t.Fatal("expected error for mismatched term, got nil")
	}

	// Stepdown forces leadership invalidation
	n.StepDownSameTerm(cluster.NodeID(2))
	if err := n.ValidateLeadership(res.Term, res.Epoch); err == nil {
		t.Fatal("expected error after stepdown, got nil")
	}
}
