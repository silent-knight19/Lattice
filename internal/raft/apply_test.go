package raft

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/engine"
	latticeErrors "github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/transport"
)

// mockStateMachine records applied commands and allows injecting failures.
type mockStateMachine struct {
	mu           sync.Mutex
	appliedOps   []Command
	appliedKeys  []string
	failOnKey    map[string]error
	failOnCount  int // fails when len(appliedOps) + 1 == failOnCount
	currentCalls int
	failErr      error
}

func newMockSM() *mockStateMachine {
	return &mockStateMachine{
		failOnKey: make(map[string]error),
	}
}

func (m *mockStateMachine) Put(ctx context.Context, key, val []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.currentCalls++
	if m.failOnCount > 0 && m.currentCalls == m.failOnCount {
		err := m.failErr
		if err == nil {
			err = errors.New("mock state machine put failure")
		}
		return err
	}
	if err, ok := m.failOnKey[string(key)]; ok {
		return err
	}

	cmd := Command{
		Op:    binary.OpTypePut,
		Key:   append([]byte(nil), key...),
		Value: append([]byte(nil), val...),
	}
	m.appliedOps = append(m.appliedOps, cmd)
	m.appliedKeys = append(m.appliedKeys, string(key))
	return nil
}

func (m *mockStateMachine) Delete(ctx context.Context, key []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.currentCalls++
	if m.failOnCount > 0 && m.currentCalls == m.failOnCount {
		err := m.failErr
		if err == nil {
			err = errors.New("mock state machine delete failure")
		}
		return err
	}
	if err, ok := m.failOnKey[string(key)]; ok {
		return err
	}

	cmd := Command{
		Op:  binary.OpTypeDelete,
		Key: append([]byte(nil), key...),
	}
	m.appliedOps = append(m.appliedOps, cmd)
	m.appliedKeys = append(m.appliedKeys, string(key))
	return nil
}

func (m *mockStateMachine) Batch(ctx context.Context, ops []binary.BatchOp) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.currentCalls++
	if m.failOnCount > 0 && m.currentCalls == m.failOnCount {
		err := m.failErr
		if err == nil {
			err = errors.New("mock state machine batch failure")
		}
		return err
	}
	for _, op := range ops {
		if err, ok := m.failOnKey[string(op.Key)]; ok {
			return err
		}
	}

	for _, op := range ops {
		cmd := Command{
			Op:    op.Type,
			Key:   append([]byte(nil), op.Key...),
			Value: append([]byte(nil), op.Value...),
		}
		m.appliedOps = append(m.appliedOps, cmd)
		m.appliedKeys = append(m.appliedKeys, string(op.Key))
	}
	return nil
}

func (m *mockStateMachine) opsCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.appliedOps)
}

func (m *mockStateMachine) getOps() []Command {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]Command, len(m.appliedOps))
	copy(cp, m.appliedOps)
	return cp
}

// helper to create an active test node with a state machine
func newTestNodeWithSM(t *testing.T, id cluster.NodeID, sm StateMachine, batchSize int) (*Node, *Storage, func()) {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	if err := s.SetTerm(1); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}
	cfg := NodeConfig{
		LocalID:        id,
		Storage:        s,
		StateMachine:   sm,
		ApplyBatchSize: batchSize,
	}
	n, err := NewNode(cfg)
	if err != nil {
		_ = s.Close()
		t.Fatalf("NewNode failed: %v", err)
	}
	cleanup := func() {
		_ = n.Close()
		_ = s.Close()
	}
	return n, s, cleanup
}

// helper to wait for lastApplied with timeout
func waitForApplied(t *testing.T, n *Node, target LogIndex, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if n.LastApplied() >= target {
			return
		}
		if err := n.ApplyError(); err != nil {
			t.Fatalf("ApplyError observed while waiting for %d: %v", target, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for lastApplied >= %d (got %d)", target, n.LastApplied())
}

// -----------------------------------------------------------------------------
// Test 1: No committed entries
// -----------------------------------------------------------------------------
func TestApplyLoop_IdleNoCommitted(t *testing.T) {
	sm := newMockSM()
	n, _, cleanup := newTestNodeWithSM(t, 1, sm, 10)
	defer cleanup()

	time.Sleep(50 * time.Millisecond)

	if got := n.LastApplied(); got != 0 {
		t.Fatalf("expected lastApplied = 0, got %d", got)
	}
	if sm.opsCount() != 0 {
		t.Fatalf("expected 0 ops applied, got %d", sm.opsCount())
	}
	if err := n.ApplyError(); err != nil {
		t.Fatalf("unexpected ApplyError: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Test 2: Single committed entry
// -----------------------------------------------------------------------------
func TestApplyLoop_SingleCommittedEntry(t *testing.T) {
	sm := newMockSM()
	n, s, cleanup := newTestNodeWithSM(t, 1, sm, 10)
	defer cleanup()

	cmdData, err := EncodeCommand(Command{
		Op:    binary.OpTypePut,
		Key:   []byte("key-1"),
		Value: []byte("val-1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	entry := LogEntry{
		Index: 1,
		Term:  1,
		Type:  transport.PeerEntryNormal,
		Data:  cmdData,
	}
	if err := s.Append(entry); err != nil {
		t.Fatal(err)
	}

	n.mu.Lock()
	n.commitIndex = 1
	n.signalApplyLocked()
	n.mu.Unlock()

	waitForApplied(t, n, 1, time.Second)

	if got := n.LastApplied(); got != 1 {
		t.Fatalf("expected lastApplied = 1, got %d", got)
	}
	ops := sm.getOps()
	if len(ops) != 1 {
		t.Fatalf("expected 1 op, got %d", len(ops))
	}
	if !bytes.Equal(ops[0].Key, []byte("key-1")) || !bytes.Equal(ops[0].Value, []byte("val-1")) {
		t.Fatalf("unexpected op: %+v", ops[0])
	}
}

// -----------------------------------------------------------------------------
// Test 3: Multiple committed entries in exact order
// -----------------------------------------------------------------------------
func TestApplyLoop_MultipleCommittedEntries(t *testing.T) {
	sm := newMockSM()
	n, s, cleanup := newTestNodeWithSM(t, 1, sm, 10)
	defer cleanup()

	for i := 1; i <= 5; i++ {
		cmdData, err := EncodeCommand(Command{
			Op:    binary.OpTypePut,
			Key:   []byte(fmt.Sprintf("key-%d", i)),
			Value: []byte(fmt.Sprintf("val-%d", i)),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Append(LogEntry{
			Index: LogIndex(i),
			Term:  1,
			Type:  transport.PeerEntryNormal,
			Data:  cmdData,
		}); err != nil {
			t.Fatal(err)
		}
	}

	n.mu.Lock()
	n.commitIndex = 5
	n.signalApplyLocked()
	n.mu.Unlock()

	waitForApplied(t, n, 5, time.Second)

	ops := sm.getOps()
	if len(ops) != 5 {
		t.Fatalf("expected 5 ops, got %d", len(ops))
	}
	for i := 0; i < 5; i++ {
		expectedKey := fmt.Sprintf("key-%d", i+1)
		if string(ops[i].Key) != expectedKey {
			t.Fatalf("op %d: expected key %s, got %s", i, expectedKey, ops[i].Key)
		}
	}
}

// -----------------------------------------------------------------------------
// Test 4: Apply order harness (monotonicity)
// -----------------------------------------------------------------------------
func TestApplyLoop_StrictOrderMonotonicity(t *testing.T) {
	sm := newMockSM()
	n, s, cleanup := newTestNodeWithSM(t, 1, sm, 4) // small batch size to test batch boundaries
	defer cleanup()

	count := 25
	for i := 1; i <= count; i++ {
		cmdData, _ := EncodeCommand(Command{
			Op:    binary.OpTypePut,
			Key:   []byte(fmt.Sprintf("seq-%04d", i)),
			Value: []byte(fmt.Sprintf("v-%04d", i)),
		})
		_ = s.Append(LogEntry{
			Index: LogIndex(i),
			Term:  1,
			Type:  transport.PeerEntryNormal,
			Data:  cmdData,
		})
	}

	n.mu.Lock()
	n.commitIndex = LogIndex(count)
	n.signalApplyLocked()
	n.mu.Unlock()

	waitForApplied(t, n, LogIndex(count), 2*time.Second)

	ops := sm.getOps()
	if len(ops) != count {
		t.Fatalf("expected %d ops, got %d", count, len(ops))
	}
	for i := 0; i < count; i++ {
		want := fmt.Sprintf("seq-%04d", i+1)
		if string(ops[i].Key) != want {
			t.Fatalf("index %d: expected %s, got %s", i, want, ops[i].Key)
		}
	}
}

// -----------------------------------------------------------------------------
// Test 5: Commit jump from 0 -> 20
// -----------------------------------------------------------------------------
func TestApplyLoop_CommitJump(t *testing.T) {
	sm := newMockSM()
	n, s, cleanup := newTestNodeWithSM(t, 1, sm, 8)
	defer cleanup()

	for i := 1; i <= 20; i++ {
		cmdData, _ := EncodeCommand(Command{
			Op:    binary.OpTypePut,
			Key:   []byte(fmt.Sprintf("jump-%d", i)),
			Value: []byte(fmt.Sprintf("val-%d", i)),
		})
		_ = s.Append(LogEntry{
			Index: LogIndex(i),
			Term:  1,
			Type:  transport.PeerEntryNormal,
			Data:  cmdData,
		})
	}

	// Direct jump
	n.mu.Lock()
	n.commitIndex = 20
	n.signalApplyLocked()
	n.mu.Unlock()

	waitForApplied(t, n, 20, 2*time.Second)

	if sm.opsCount() != 20 {
		t.Fatalf("expected 20 ops applied, got %d", sm.opsCount())
	}
}

// -----------------------------------------------------------------------------
// Test 6: Partial failure halts loop, does not skip, observable error
// -----------------------------------------------------------------------------
func TestApplyLoop_PartialFailure(t *testing.T) {
	sm := newMockSM()
	sm.failOnKey["key-3"] = errors.New("injected storage error at key-3")

	n, s, cleanup := newTestNodeWithSM(t, 1, sm, 10)
	defer cleanup()

	for i := 1; i <= 5; i++ {
		cmdData, _ := EncodeCommand(Command{
			Op:    binary.OpTypePut,
			Key:   []byte(fmt.Sprintf("key-%d", i)),
			Value: []byte(fmt.Sprintf("val-%d", i)),
		})
		_ = s.Append(LogEntry{
			Index: LogIndex(i),
			Term:  1,
			Type:  transport.PeerEntryNormal,
			Data:  cmdData,
		})
	}

	n.mu.Lock()
	n.commitIndex = 5
	n.signalApplyLocked()
	n.mu.Unlock()

	// Wait for error to surface
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if n.ApplyError() != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	applyErr := n.ApplyError()
	if applyErr == nil {
		t.Fatal("expected ApplyError to be non-nil")
	}
	if !errors.Is(applyErr, latticeErrors.ErrRaftApplyFailed) {
		t.Fatalf("expected ErrRaftApplyFailed, got %v", applyErr)
	}

	// Must have applied exactly 1 and 2
	if got := n.LastApplied(); got != 2 {
		t.Fatalf("expected lastApplied = 2 (stopped before failing entry 3), got %d", got)
	}
	if sm.opsCount() != 2 {
		t.Fatalf("expected exactly 2 ops applied, got %d", sm.opsCount())
	}

	// Advance commit further; loop must remain halted
	n.mu.Lock()
	n.commitIndex = 6
	n.signalApplyLocked()
	n.mu.Unlock()

	time.Sleep(50 * time.Millisecond)
	if got := n.LastApplied(); got != 2 {
		t.Fatalf("apply loop continued after failure: lastApplied = %d", got)
	}
}

// -----------------------------------------------------------------------------
// Test 7: Retry after failure (node restart or recovered apply loop)
// -----------------------------------------------------------------------------
func TestApplyLoop_RetryAfterFailure(t *testing.T) {
	sm := newMockSM()
	sm.failOnCount = 3 // fails on 3rd call
	sm.failErr = errors.New("transient sm error")

	dir := t.TempDir()
	s, err := OpenStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.SetTerm(1); err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= 5; i++ {
		cmdData, _ := EncodeCommand(Command{
			Op:    binary.OpTypePut,
			Key:   []byte(fmt.Sprintf("k-%d", i)),
			Value: []byte(fmt.Sprintf("v-%d", i)),
		})
		_ = s.Append(LogEntry{
			Index: LogIndex(i),
			Term:  1,
			Type:  transport.PeerEntryNormal,
			Data:  cmdData,
		})
	}

	n, err := NewNode(NodeConfig{
		LocalID:      1,
		Storage:      s,
		StateMachine: sm,
	})
	if err != nil {
		t.Fatal(err)
	}

	n.mu.Lock()
	n.commitIndex = 5
	n.signalApplyLocked()
	n.mu.Unlock()

	// Wait for failure on entry 3
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if n.ApplyError() != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n.LastApplied() != 2 {
		t.Fatalf("expected lastApplied = 2, got %d", n.LastApplied())
	}
	_ = n.Close()

	// Clear failure condition on state machine
	sm.mu.Lock()
	sm.failOnCount = 0
	sm.currentCalls = 0
	sm.mu.Unlock()

	// Recreate node with same storage and now-healthy state machine
	n2, err := NewNode(NodeConfig{
		LocalID:      1,
		Storage:      s,
		StateMachine: sm,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n2.Close() }()

	// Resume application from lastApplied point (which restarts at 2 if initialized)
	n2.mu.Lock()
	n2.lastApplied = 2 // recovered progress
	n2.commitIndex = 5
	n2.signalApplyLocked()
	n2.mu.Unlock()

	waitForApplied(t, n2, 5, time.Second)

	if got := n2.LastApplied(); got != 5 {
		t.Fatalf("expected lastApplied = 5 after recovery, got %d", got)
	}
	ops := sm.getOps()
	if len(ops) != 5 {
		t.Fatalf("expected all 5 ops to be applied, got %d", len(ops))
	}
}

// -----------------------------------------------------------------------------
// Test 8: Malformed entry halts loop safely without panic
// -----------------------------------------------------------------------------
func TestApplyLoop_MalformedEntry(t *testing.T) {
	t.Run("invalid entry type", func(t *testing.T) {
		sm := newMockSM()
		n, _, cleanup := newTestNodeWithSM(t, 1, sm, 10)
		defer cleanup()

		err := n.applySingleEntry(LogEntry{
			Index: 1,
			Term:  1,
			Type:  0x99, // unknown entry type
			Data:  []byte("bad"),
		})
		if err == nil {
			t.Fatal("expected error for invalid entry type")
		}
		if !errors.Is(err, latticeErrors.ErrRaftInvalidEntryType) {
			t.Fatalf("expected ErrRaftInvalidEntryType, got %v", err)
		}
	})

	t.Run("corrupted command payload", func(t *testing.T) {
		sm := newMockSM()
		n, s, cleanup := newTestNodeWithSM(t, 1, sm, 10)
		defer cleanup()

		if err := s.Append(LogEntry{
			Index: 1,
			Term:  1,
			Type:  transport.PeerEntryNormal,
			Data:  []byte{0x01}, // truncated command header
		}); err != nil {
			t.Fatal(err)
		}

		n.mu.Lock()
		n.commitIndex = 1
		n.signalApplyLocked()
		n.mu.Unlock()

		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if n.ApplyError() != nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}

		if n.LastApplied() != 0 {
			t.Fatalf("expected lastApplied = 0, got %d", n.LastApplied())
		}
		if !errors.Is(n.ApplyError(), latticeErrors.ErrRaftCorruptedState) {
			t.Fatalf("expected ErrRaftCorruptedState, got %v", n.ApplyError())
		}
	})

	t.Run("invalid command opcode", func(t *testing.T) {
		sm := newMockSM()
		n, s, cleanup := newTestNodeWithSM(t, 1, sm, 10)
		defer cleanup()

		// Opcode 0x77 is not Put or Delete
		badPayload := []byte{0x77, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 'k'}
		if err := s.Append(LogEntry{
			Index: 1,
			Term:  1,
			Type:  transport.PeerEntryNormal,
			Data:  badPayload,
		}); err != nil {
			t.Fatal(err)
		}

		n.mu.Lock()
		n.commitIndex = 1
		n.signalApplyLocked()
		n.mu.Unlock()

		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if n.ApplyError() != nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}

		if n.LastApplied() != 0 {
			t.Fatalf("expected lastApplied = 0, got %d", n.LastApplied())
		}
		if !errors.Is(n.ApplyError(), latticeErrors.ErrRaftCorruptedState) {
			t.Fatalf("expected ErrRaftCorruptedState, got %v", n.ApplyError())
		}
	})
}

// -----------------------------------------------------------------------------
// Test 9: No-op entry handling (PeerEntryNoop)
// -----------------------------------------------------------------------------
func TestApplyLoop_NoopEntry(t *testing.T) {
	sm := newMockSM()
	n, s, cleanup := newTestNodeWithSM(t, 1, sm, 10)
	defer cleanup()

	// Entry 1: PeerEntryNoop (consensus election alignment)
	_ = s.Append(LogEntry{
		Index: 1,
		Term:  1,
		Type:  transport.PeerEntryNoop,
		Data:  nil,
	})

	// Entry 2: PeerEntryNormal (Put mutation)
	cmdData, _ := EncodeCommand(Command{
		Op:    binary.OpTypePut,
		Key:   []byte("real-key"),
		Value: []byte("real-val"),
	})
	_ = s.Append(LogEntry{
		Index: 2,
		Term:  1,
		Type:  transport.PeerEntryNormal,
		Data:  cmdData,
	})

	n.mu.Lock()
	n.commitIndex = 2
	n.signalApplyLocked()
	n.mu.Unlock()

	waitForApplied(t, n, 2, time.Second)

	if got := n.LastApplied(); got != 2 {
		t.Fatalf("expected lastApplied = 2, got %d", got)
	}

	// State machine must only have received the single PUT mutation, NOT the Noop
	ops := sm.getOps()
	if len(ops) != 1 {
		t.Fatalf("expected exactly 1 state machine mutation, got %d", len(ops))
	}
	if string(ops[0].Key) != "real-key" {
		t.Fatalf("expected key 'real-key', got %s", ops[0].Key)
	}
}

// -----------------------------------------------------------------------------
// Test 10: Role change does not stop apply loop
// -----------------------------------------------------------------------------
func TestApplyLoop_RoleChangeContinuesApply(t *testing.T) {
	sm := newMockSM()
	n, s, cleanup := newTestNodeWithSM(t, 1, sm, 10)
	defer cleanup()

	// Become leader
	if err := n.BecomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatal(err)
	}

	// Leader steps down to follower at term 2
	if err := n.BecomeFollower(2, cluster.NodeID(2)); err != nil {
		t.Fatal(err)
	}
	if n.Role() != RoleFollower {
		t.Fatalf("expected RoleFollower, got %v", n.Role())
	}

	// As a follower, receive committed entry
	cmdData, _ := EncodeCommand(Command{
		Op:    binary.OpTypePut,
		Key:   []byte("follower-key"),
		Value: []byte("follower-val"),
	})
	if err := s.Append(LogEntry{
		Index: 2, // index 1 is the leader election no-op
		Term:  2,
		Type:  transport.PeerEntryNormal,
		Data:  cmdData,
	}); err != nil {
		t.Fatal(err)
	}

	// Follower commitIndex advances
	n.mu.Lock()
	n.commitIndex = 2
	n.signalApplyLocked()
	n.mu.Unlock()

	waitForApplied(t, n, 2, time.Second)

	if got := n.LastApplied(); got != 2 {
		t.Fatalf("expected lastApplied = 2 while follower, got %d", got)
	}
	if sm.opsCount() != 1 {
		t.Fatalf("expected 1 op applied, got %d", sm.opsCount())
	}
}

// -----------------------------------------------------------------------------
// Test 11: Shutdown safety under various states
// -----------------------------------------------------------------------------
func TestApplyLoop_ShutdownSafety(t *testing.T) {
	t.Run("shutdown while idle", func(t *testing.T) {
		sm := newMockSM()
		n, _, cleanup := newTestNodeWithSM(t, 1, sm, 10)
		defer cleanup()

		if err := n.Close(); err != nil {
			t.Fatalf("Close failed: %v", err)
		}
		// Repeated close must be safe
		if err := n.Close(); err != nil {
			t.Fatalf("repeated Close failed: %v", err)
		}
	})

	t.Run("shutdown with pending backlog", func(t *testing.T) {
		sm := newMockSM()
		n, s, cleanup := newTestNodeWithSM(t, 1, sm, 10)
		defer cleanup()

		for i := 1; i <= 100; i++ {
			cmdData, _ := EncodeCommand(Command{
				Op:    binary.OpTypePut,
				Key:   []byte(fmt.Sprintf("k-%d", i)),
				Value: []byte(fmt.Sprintf("v-%d", i)),
			})
			_ = s.Append(LogEntry{
				Index: LogIndex(i),
				Term:  1,
				Type:  transport.PeerEntryNormal,
				Data:  cmdData,
			})
		}

		n.mu.Lock()
		n.commitIndex = 100
		n.signalApplyLocked()
		n.mu.Unlock()

		// Close immediately while apply loop is draining
		if err := n.Close(); err != nil {
			t.Fatalf("Close failed: %v", err)
		}
	})
}

// -----------------------------------------------------------------------------
// Test 12: Concurrent commit and apply under race detector
// -----------------------------------------------------------------------------
func TestApplyLoop_ConcurrentCommitAndApply(t *testing.T) {
	sm := newMockSM()
	n, s, cleanup := newTestNodeWithSM(t, 1, sm, 5)
	defer cleanup()

	totalEntries := 50
	for i := 1; i <= totalEntries; i++ {
		cmdData, _ := EncodeCommand(Command{
			Op:    binary.OpTypePut,
			Key:   []byte(fmt.Sprintf("ck-%04d", i)),
			Value: []byte(fmt.Sprintf("cv-%04d", i)),
		})
		_ = s.Append(LogEntry{
			Index: LogIndex(i),
			Term:  1,
			Type:  transport.PeerEntryNormal,
			Data:  cmdData,
		})
	}

	var wg sync.WaitGroup
	wg.Add(1)

	// Goroutine A: incrementally advances commitIndex
	go func() {
		defer wg.Done()
		for i := 1; i <= totalEntries; i++ {
			n.mu.Lock()
			n.commitIndex = LogIndex(i)
			n.signalApplyLocked()
			n.mu.Unlock()
			time.Sleep(time.Duration(i%3) * time.Millisecond)
		}
	}()

	wg.Wait()
	waitForApplied(t, n, LogIndex(totalEntries), 3*time.Second)

	if got := n.LastApplied(); got != LogIndex(totalEntries) {
		t.Fatalf("expected lastApplied = %d, got %d", totalEntries, got)
	}
	ops := sm.getOps()
	if len(ops) != totalEntries {
		t.Fatalf("expected %d ops, got %d", totalEntries, len(ops))
	}
}

// -----------------------------------------------------------------------------
// Test 13: Restart / Reapplication idempotent recovery
// -----------------------------------------------------------------------------
func TestApplyLoop_RestartReapplication(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTerm(1); err != nil {
		t.Fatal(err)
	}

	// Prepare entries
	for i := 1; i <= 5; i++ {
		cmdData, _ := EncodeCommand(Command{
			Op:    binary.OpTypePut,
			Key:   []byte(fmt.Sprintf("re-key-%d", i)),
			Value: []byte(fmt.Sprintf("re-val-%d", i)),
		})
		_ = s.Append(LogEntry{
			Index: LogIndex(i),
			Term:  1,
			Type:  transport.PeerEntryNormal,
			Data:  cmdData,
		})
	}

	sm1 := newMockSM()
	n1, err := NewNode(NodeConfig{
		LocalID:      1,
		Storage:      s,
		StateMachine: sm1,
	})
	if err != nil {
		t.Fatal(err)
	}

	n1.mu.Lock()
	n1.commitIndex = 5
	n1.signalApplyLocked()
	n1.mu.Unlock()

	waitForApplied(t, n1, 5, time.Second)
	_ = n1.Close()

	// Simulate restart with new Node instance replaying from index 1
	sm2 := newMockSM()
	n2, err := NewNode(NodeConfig{
		LocalID:      1,
		Storage:      s,
		StateMachine: sm2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = n2.Close()
		_ = s.Close()
	}()

	n2.mu.Lock()
	n2.commitIndex = 5
	n2.signalApplyLocked()
	n2.mu.Unlock()

	waitForApplied(t, n2, 5, time.Second)

	// Replayed state machine must have identical final content
	ops1 := sm1.getOps()
	ops2 := sm2.getOps()
	if len(ops1) != len(ops2) {
		t.Fatalf("replayed op count mismatch: %d vs %d", len(ops1), len(ops2))
	}
	for i := range ops1 {
		if !bytes.Equal(ops1[i].Key, ops2[i].Key) || !bytes.Equal(ops1[i].Value, ops2[i].Value) {
			t.Fatalf("op %d mismatch between runs: %+v vs %+v", i, ops1[i], ops2[i])
		}
	}
}

// -----------------------------------------------------------------------------
// Test 14: Reference model comparison against real LSM Engine
// -----------------------------------------------------------------------------
func TestApplyLoop_ReferenceModelOracle(t *testing.T) {
	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		Backpressure: engine.BackpressureConfig{
			MaxMemoryBytes: 32 * 1024 * 1024,
			HighWatermark:  0.80,
			HardWatermark:  0.95,
			MaxWaitTimeout: 100 * time.Millisecond,
		},
	})
	defer func() { _ = eng.Close() }()

	n, s, cleanup := newTestNodeWithSM(t, 1, eng, 8)
	defer cleanup()

	// Reference model: simple Go map
	refMap := make(map[string][]byte)

	// Sequence of deterministic PUT and DELETE operations
	operations := []struct {
		op    binary.OpType
		key   string
		value string
	}{
		{binary.OpTypePut, "user:1", "alice"},
		{binary.OpTypePut, "user:2", "bob"},
		{binary.OpTypePut, "user:3", "charlie"},
		{binary.OpTypePut, "user:1", "alice_updated"},
		{binary.OpTypeDelete, "user:2", ""},
		{binary.OpTypePut, "user:4", "david"},
		{binary.OpTypeDelete, "user:4", ""},
		{binary.OpTypePut, "user:5", "eve"},
	}

	for i, op := range operations {
		idx := LogIndex(i + 1)
		var valBytes []byte
		if op.op == binary.OpTypePut {
			valBytes = []byte(op.value)
			refMap[op.key] = valBytes
		} else {
			delete(refMap, op.key)
		}

		cmdData, err := EncodeCommand(Command{
			Op:    op.op,
			Key:   []byte(op.key),
			Value: valBytes,
		})
		if err != nil {
			t.Fatal(err)
		}

		_ = s.Append(LogEntry{
			Index: idx,
			Term:  1,
			Type:  transport.PeerEntryNormal,
			Data:  cmdData,
		})
	}

	total := LogIndex(len(operations))
	n.mu.Lock()
	n.commitIndex = total
	n.signalApplyLocked()
	n.mu.Unlock()

	waitForApplied(t, n, total, 2*time.Second)

	// Compare Engine state against Reference Model Oracle
	allKeys := []string{"user:1", "user:2", "user:3", "user:4", "user:5"}
	for _, k := range allKeys {
		expectedVal, shouldExist := refMap[k]
		gotVal, err := eng.Get([]byte(k))
		if shouldExist {
			if err != nil {
				t.Fatalf("key %s: expected %q, got error: %v", k, expectedVal, err)
			}
			if !bytes.Equal(gotVal, expectedVal) {
				t.Fatalf("key %s: expected %q, got %q", k, expectedVal, gotVal)
			}
		} else {
			if err == nil {
				t.Fatalf("key %s: expected not-found, got value: %q", k, gotVal)
			}
			if !errors.Is(err, latticeErrors.ErrKeyNotFound) {
				t.Fatalf("key %s: expected ErrKeyNotFound, got %v", k, err)
			}
		}
	}
}

// -----------------------------------------------------------------------------
// Test 15: Property testing (invariants)
// -----------------------------------------------------------------------------
func TestApplyLoop_InvariantsProperty(t *testing.T) {
	sm := newMockSM()
	n, s, cleanup := newTestNodeWithSM(t, 1, sm, 4)
	defer cleanup()

	var maxApplied atomic.Uint64

	count := 30
	for i := 1; i <= count; i++ {
		cmdData, _ := EncodeCommand(Command{
			Op:    binary.OpTypePut,
			Key:   []byte(fmt.Sprintf("prop-%d", i)),
			Value: []byte(fmt.Sprintf("pval-%d", i)),
		})
		_ = s.Append(LogEntry{
			Index: LogIndex(i),
			Term:  1,
			Type:  transport.PeerEntryNormal,
			Data:  cmdData,
		})
	}

	// Invariant watcher goroutine
	stopWatcher := make(chan struct{})
	var watcherErr atomic.Pointer[error]
	go func() {
		for {
			select {
			case <-stopWatcher:
				return
			default:
				n.mu.RLock()
				ci := n.commitIndex
				la := n.lastApplied
				n.mu.RUnlock()

				// Invariant: lastApplied never exceeds commitIndex
				if la > ci {
					err := fmt.Errorf("invariant violated: lastApplied (%d) > commitIndex (%d)", la, ci)
					watcherErr.Store(&err)
					return
				}

				// Invariant: lastApplied never regresses
				prev := maxApplied.Load()
				if uint64(la) < prev {
					err := fmt.Errorf("invariant violated: lastApplied regressed from %d to %d", prev, la)
					watcherErr.Store(&err)
					return
				}
				if uint64(la) > prev {
					maxApplied.Store(uint64(la))
				}
				time.Sleep(1 * time.Millisecond)
			}
		}
	}()

	// Feed commits in chunks
	for c := 5; c <= count; c += 5 {
		n.mu.Lock()
		n.commitIndex = LogIndex(c)
		n.signalApplyLocked()
		n.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}

	waitForApplied(t, n, LogIndex(count), 2*time.Second)
	close(stopWatcher)

	if pErr := watcherErr.Load(); pErr != nil {
		t.Fatal(*pErr)
	}

	if got := n.LastApplied(); got != LogIndex(count) {
		t.Fatalf("expected lastApplied = %d, got %d", count, got)
	}
}
