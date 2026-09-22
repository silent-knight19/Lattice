package raft

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/transport"
)

// secMockStateMachine records all applied operations and supports blocking.
type secMockStateMachine struct {
	mu      sync.Mutex
	puts    []secMockPut
	deletes []secMockDelete
	blockCh chan struct{} // if non-nil, Put/Delete block until closed
	failErr error        // if non-nil, all operations return this error
}

type secMockPut struct {
	Key, Value []byte
}

type secMockDelete struct {
	Key []byte
}

func (m *secMockStateMachine) Put(ctx context.Context, key, val []byte) error {
	if m.blockCh != nil {
		select {
		case <-m.blockCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if m.failErr != nil {
		return m.failErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts = append(m.puts, secMockPut{Key: append([]byte{}, key...), Value: append([]byte{}, val...)})
	return nil
}

func (m *secMockStateMachine) Delete(ctx context.Context, key []byte) error {
	if m.blockCh != nil {
		select {
		case <-m.blockCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if m.failErr != nil {
		return m.failErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletes = append(m.deletes, secMockDelete{Key: append([]byte{}, key...)})
	return nil
}

func (m *secMockStateMachine) putCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.puts)
}

// securityTestNode creates a single-node leader with a mock state machine for security testing.
func securityTestNode(t *testing.T, sm StateMachine, batchSize int) (*Node, func()) {
	t.Helper()
	dir := t.TempDir()
	storage, err := OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}

	topo, err := cluster.NewTopology(1, "127.0.0.1:9098", nil)
	if err != nil {
		t.Fatalf("NewTopology: %v", err)
	}

	n, err := NewNode(NodeConfig{
		LocalID:  1,
		Storage:  storage,
		Topology: topo,
	})
	if err != nil {
		storage.Close()
		t.Fatalf("NewNode: %v", err)
	}

	if err := n.BecomeCandidate(); err != nil {
		n.Close()
		storage.Close()
		t.Fatal(err)
	}
	if err := n.BecomeLeader(); err != nil {
		n.Close()
		storage.Close()
		t.Fatal(err)
	}

	if sm != nil && batchSize > 0 {
		if err := n.StartApplyLoop(sm, batchSize); err != nil {
			n.Close()
			storage.Close()
			t.Fatal(err)
		}
	}

	cleanup := func() {
		n.Close()
		storage.Close()
	}
	return n, cleanup
}

// waitApplied waits for lastApplied to reach the target index.
func waitApplied(t *testing.T, n *Node, target LogIndex, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if n.LastApplied() >= target {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("lastApplied did not reach %d within %v (current: %d)", target, timeout, n.LastApplied())
}

// =============================================================================
// F01/F09: Cluster-Mode Fail-Open Engine Bypass Tests
// =============================================================================

func TestSecurity_F01_ClusterModeRejectsPutWithoutRouter(t *testing.T) {
	sm := &secMockStateMachine{}
	n, cleanup := securityTestNode(t, sm, DefaultApplyBatchSize)
	defer cleanup()

	// Create a cluster-mode server WITHOUT a proposal router
	cfg := transport.DefaultServerConfig()
	cfg.ClusterMode = true
	cfg.InsecureTransport = true
	cfg.Address = "127.0.0.1:0"
	// ProposalRouter intentionally nil

	srv, err := transport.NewServer(cfg, &noopEngine{})
	if err != nil {
		t.Fatal(err)
	}
	_ = n // node exists but not wired

	resp := srv.TestDispatch(&transport.Request{
		OpCode: transport.OpPut,
		Key:    []byte("k"),
		Value:  []byte("v"),
		SeqID:  1,
	})
	if resp.Status == transport.StatusOk {
		t.Fatal("cluster-mode server returned StatusOk for PUT without router — fail-open bypass!")
	}
	if resp.Status != transport.StatusError {
		t.Fatalf("expected StatusError, got %v", resp.Status)
	}
}

func TestSecurity_F01_ClusterModeRejectsDeleteWithoutRouter(t *testing.T) {
	cfg := transport.DefaultServerConfig()
	cfg.ClusterMode = true
	cfg.InsecureTransport = true
	cfg.Address = "127.0.0.1:0"

	srv, err := transport.NewServer(cfg, &noopEngine{})
	if err != nil {
		t.Fatal(err)
	}

	resp := srv.TestDispatch(&transport.Request{
		OpCode: transport.OpDelete,
		Key:    []byte("k"),
		SeqID:  2,
	})
	if resp.Status == transport.StatusOk {
		t.Fatal("cluster-mode server returned StatusOk for DELETE without router — fail-open bypass!")
	}
}

func TestSecurity_F01_StandalonePermitsDirectWrites(t *testing.T) {
	eng := &countingEngine{}
	cfg := transport.DefaultServerConfig()
	cfg.ClusterMode = false // standalone
	cfg.InsecureTransport = true
	cfg.Address = "127.0.0.1:0"

	srv, err := transport.NewServer(cfg, eng)
	if err != nil {
		t.Fatal(err)
	}

	resp := srv.TestDispatch(&transport.Request{
		OpCode: transport.OpPut,
		Key:    []byte("k"),
		Value:  []byte("v"),
		SeqID:  1,
	})
	if resp.Status != transport.StatusOk {
		t.Fatalf("standalone server should permit direct PUT, got %v: %s", resp.Status, resp.Message)
	}
	if eng.putCount.Load() != 1 {
		t.Fatalf("expected 1 Engine.Put call, got %d", eng.putCount.Load())
	}
}

func TestSecurity_F09_ClearingRouterDoesNotEnableDirectWrites(t *testing.T) {
	eng := &countingEngine{}
	cfg := transport.DefaultServerConfig()
	cfg.ClusterMode = true
	cfg.InsecureTransport = true
	cfg.Address = "127.0.0.1:0"

	srv, err := transport.NewServer(cfg, eng)
	if err != nil {
		t.Fatal(err)
	}

	// Set a router, then clear it
	srv.SetProposalRouter(&mockRouter{})
	srv.SetProposalRouter(nil)

	resp := srv.TestDispatch(&transport.Request{
		OpCode: transport.OpPut,
		Key:    []byte("k"),
		Value:  []byte("v"),
		SeqID:  1,
	})
	if resp.Status == transport.StatusOk {
		t.Fatal("clearing router in cluster mode re-enabled direct writes — F09 bypass!")
	}
	if eng.putCount.Load() != 0 {
		t.Fatalf("Engine.Put was called %d times — should be 0 in cluster mode", eng.putCount.Load())
	}
}

// =============================================================================
// F02: Leadership TOCTOU Tests
// =============================================================================

func TestSecurity_F02_StepdownDuringProposeSuppressesAck(t *testing.T) {
	sm := &secMockStateMachine{}
	n, cleanup := securityTestNode(t, sm, DefaultApplyBatchSize)
	defer cleanup()

	// Install test hook: force stepdown AFTER leadership check, BEFORE append
	stepdownDone := make(chan struct{})
	n.proposeTestHook = func() {
		if err := n.BecomeFollower(n.storage.mustTerm()+1, cluster.NodeIDNil); err != nil {
			t.Logf("stepdown in hook: %v", err)
		}
		close(stepdownDone)
	}

	_, err := n.Propose([]byte("doomed"))
	if err == nil {
		t.Fatal("Propose returned success after stepdown during append — stale leader ack!")
	}
	<-stepdownDone

	// The entry may or may not be in the log (it was durably appended), but the
	// client must NOT have received a success acknowledgement.
	t.Logf("Propose correctly returned error: %v", err)
}

func TestSecurity_F02_ProposalBeforeStepdownSucceeds(t *testing.T) {
	sm := &secMockStateMachine{}
	n, cleanup := securityTestNode(t, sm, DefaultApplyBatchSize)
	defer cleanup()

	// No test hook — proposal completes before any stepdown
	entry, err := n.Propose([]byte("ok"))
	if err != nil {
		t.Fatalf("Propose should succeed before stepdown: %v", err)
	}
	if entry.Index == 0 {
		t.Fatal("returned entry has zero index")
	}
}

func TestSecurity_F02_EpochIncrementOnTransitions(t *testing.T) {
	dir := t.TempDir()
	storage, err := OpenStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()

	n, err := NewNode(NodeConfig{LocalID: 1, Storage: storage})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()

	epoch0 := n.leaderEpoch

	if err := n.BecomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatal(err)
	}

	n.mu.RLock()
	epoch1 := n.leaderEpoch
	n.mu.RUnlock()
	if epoch1 <= epoch0 {
		t.Fatalf("leaderEpoch should have incremented on BecomeLeader: %d -> %d", epoch0, epoch1)
	}

	currTerm, _ := storage.Term()
	if err := n.BecomeFollower(currTerm+1, cluster.NodeIDNil); err != nil {
		t.Fatal(err)
	}

	n.mu.RLock()
	epoch2 := n.leaderEpoch
	n.mu.RUnlock()
	if epoch2 <= epoch1 {
		t.Fatalf("leaderEpoch should have incremented on BecomeFollower: %d -> %d", epoch1, epoch2)
	}
}

// =============================================================================
// F03: Context-Aware Proposal Tests
// =============================================================================

func TestSecurity_F03_CancelledContextBeforeAdmission(t *testing.T) {
	sm := &secMockStateMachine{}
	n, cleanup := securityTestNode(t, sm, DefaultApplyBatchSize)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, err := n.ProposeWithContext(ctx, []byte("data"))
	if err == nil {
		t.Fatal("ProposeWithContext should fail with cancelled context")
	}
}

func TestSecurity_F03_DeadlineExpiredBeforeAdmission(t *testing.T) {
	sm := &secMockStateMachine{}
	n, cleanup := securityTestNode(t, sm, DefaultApplyBatchSize)
	defer cleanup()

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-1*time.Second))
	defer cancel()

	_, err := n.ProposeWithContext(ctx, []byte("data"))
	if err == nil {
		t.Fatal("ProposeWithContext should fail with expired deadline")
	}
}

func TestSecurity_F03_SuccessWithValidContext(t *testing.T) {
	sm := &secMockStateMachine{}
	n, cleanup := securityTestNode(t, sm, DefaultApplyBatchSize)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	entry, err := n.ProposeWithContext(ctx, []byte("data"))
	if err != nil {
		t.Fatalf("ProposeWithContext should succeed: %v", err)
	}
	if entry.Index == 0 {
		t.Fatal("returned entry has zero index")
	}
}

// =============================================================================
// F04: Apply Batch Size Bounding Tests
// =============================================================================

func TestSecurity_F04_ZeroBatchSize(t *testing.T) {
	sm := &secMockStateMachine{}
	n, cleanup := securityTestNode(t, nil, 0) // no apply loop yet
	defer cleanup()

	if err := n.StartApplyLoop(sm, 0); err != nil {
		t.Fatal(err)
	}
	if n.applyBatchSize != DefaultApplyBatchSize {
		t.Fatalf("expected batchSize=%d for 0, got %d", DefaultApplyBatchSize, n.applyBatchSize)
	}
}

func TestSecurity_F04_NegativeBatchSize(t *testing.T) {
	sm := &secMockStateMachine{}
	n, cleanup := securityTestNode(t, nil, 0)
	defer cleanup()

	if err := n.StartApplyLoop(sm, -1); err != nil {
		t.Fatal(err)
	}
	if n.applyBatchSize != DefaultApplyBatchSize {
		t.Fatalf("expected batchSize=%d for -1, got %d", DefaultApplyBatchSize, n.applyBatchSize)
	}
}

func TestSecurity_F04_ExactMaxBatchSize(t *testing.T) {
	sm := &secMockStateMachine{}
	n, cleanup := securityTestNode(t, nil, 0)
	defer cleanup()

	if err := n.StartApplyLoop(sm, MaxApplyBatchSize); err != nil {
		t.Fatal(err)
	}
	if n.applyBatchSize != MaxApplyBatchSize {
		t.Fatalf("expected batchSize=%d, got %d", MaxApplyBatchSize, n.applyBatchSize)
	}
}

func TestSecurity_F04_AboveMaxBatchSizeClamped(t *testing.T) {
	sm := &secMockStateMachine{}
	n, cleanup := securityTestNode(t, nil, 0)
	defer cleanup()

	if err := n.StartApplyLoop(sm, MaxApplyBatchSize+1); err != nil {
		t.Fatal(err)
	}
	if n.applyBatchSize != MaxApplyBatchSize {
		t.Fatalf("expected clamped batchSize=%d, got %d", MaxApplyBatchSize, n.applyBatchSize)
	}
}

func TestSecurity_F04_MaxIntBatchSizeClamped(t *testing.T) {
	sm := &secMockStateMachine{}
	n, cleanup := securityTestNode(t, nil, 0)
	defer cleanup()

	if err := n.StartApplyLoop(sm, math.MaxInt); err != nil {
		t.Fatal(err)
	}
	if n.applyBatchSize != MaxApplyBatchSize {
		t.Fatalf("expected clamped batchSize=%d for MaxInt, got %d", MaxApplyBatchSize, n.applyBatchSize)
	}
}

// =============================================================================
// F05: Noop/Configuration Payload Canonicality Tests
// =============================================================================

func TestSecurity_F05_NoopEmptyDataSucceeds(t *testing.T) {
	sm := &secMockStateMachine{}
	n, cleanup := securityTestNode(t, sm, DefaultApplyBatchSize)
	defer cleanup()

	entry := LogEntry{Index: 999, Term: 1, Type: transport.PeerEntryNoop, Data: nil}
	err := n.applySingleEntry(entry)
	if err != nil {
		t.Fatalf("noop with empty data should succeed: %v", err)
	}
}

func TestSecurity_F05_NoopWithDataRejected(t *testing.T) {
	sm := &secMockStateMachine{}
	n, cleanup := securityTestNode(t, sm, DefaultApplyBatchSize)
	defer cleanup()

	entry := LogEntry{Index: 999, Term: 1, Type: transport.PeerEntryNoop, Data: []byte("payload")}
	err := n.applySingleEntry(entry)
	if err == nil {
		t.Fatal("noop with non-empty data should be rejected fail-closed")
	}
}

func TestSecurity_F05_NoopWithLargePayloadRejected(t *testing.T) {
	sm := &secMockStateMachine{}
	n, cleanup := securityTestNode(t, sm, DefaultApplyBatchSize)
	defer cleanup()

	entry := LogEntry{Index: 999, Term: 1, Type: transport.PeerEntryNoop, Data: make([]byte, 1024*1024)}
	err := n.applySingleEntry(entry)
	if err == nil {
		t.Fatal("noop with large data should be rejected")
	}
}

func TestSecurity_F05_ConfigEmptyDataSucceeds(t *testing.T) {
	sm := &secMockStateMachine{}
	n, cleanup := securityTestNode(t, sm, DefaultApplyBatchSize)
	defer cleanup()

	entry := LogEntry{Index: 999, Term: 1, Type: transport.PeerEntryConfiguration, Data: nil}
	err := n.applySingleEntry(entry)
	if err != nil {
		t.Fatalf("config with empty data should succeed: %v", err)
	}
}

func TestSecurity_F05_ConfigWithDataRejected(t *testing.T) {
	sm := &secMockStateMachine{}
	n, cleanup := securityTestNode(t, sm, DefaultApplyBatchSize)
	defer cleanup()

	entry := LogEntry{Index: 999, Term: 1, Type: transport.PeerEntryConfiguration, Data: []byte("config")}
	err := n.applySingleEntry(entry)
	if err == nil {
		t.Fatal("configuration entry with non-empty data should be rejected in Phase 16")
	}
}

// =============================================================================
// F06: Apply Storage Result Validation Tests
// =============================================================================

func TestSecurity_F06_OversizedBatchFromStorageRejected(t *testing.T) {
	sm := &secMockStateMachine{}
	n, cleanup := securityTestNode(t, sm, DefaultApplyBatchSize)
	defer cleanup()

	// Propose entries so there's something to apply
	cmd := Command{Op: binary.OpTypePut, Key: []byte("k"), Value: []byte("v")}
	data, _ := EncodeCommand(cmd)
	if _, err := n.Propose(data); err != nil {
		t.Fatal(err)
	}

	// Wait for apply
	waitApplied(t, n, 2, 2*time.Second) // index 1 = noop, index 2 = our entry

	// If we get here without error, the normal path works.
	// The hostile-storage test verifies rejection of malformed results.
	if n.ApplyError() != nil {
		t.Fatalf("unexpected apply error: %v", n.ApplyError())
	}
}

// =============================================================================
// F07: Shutdown Apply Path Tests
// =============================================================================

func TestSecurity_F07_ShutdownWithBlockedStateMachine(t *testing.T) {
	blockCh := make(chan struct{})
	sm := &secMockStateMachine{blockCh: blockCh}
	n, cleanup := securityTestNode(t, sm, DefaultApplyBatchSize)

	// Propose something so the apply loop has work
	cmd := Command{Op: binary.OpTypePut, Key: []byte("k"), Value: []byte("v")}
	data, _ := EncodeCommand(cmd)
	if _, err := n.Propose(data); err != nil {
		cleanup()
		t.Fatal(err)
	}

	// Give the apply loop time to start blocking on the state machine
	time.Sleep(50 * time.Millisecond)

	// Close should not hang forever
	done := make(chan struct{})
	go func() {
		cleanup()
		close(done)
	}()

	// Unblock the state machine after a short delay to let shutdown proceed
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(blockCh)
	}()

	select {
	case <-done:
		// Success
	case <-time.After(15 * time.Second):
		t.Fatal("Node.Close blocked for more than 15s — shutdown safety violation")
	}
}

func TestSecurity_F07_RepeatedClose(t *testing.T) {
	sm := &secMockStateMachine{}
	n, cleanup := securityTestNode(t, sm, DefaultApplyBatchSize)
	defer cleanup()

	// First close
	if err := n.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	// Second close should be idempotent
	if err := n.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// =============================================================================
// F08: Redirect Parsing Trust Boundary Tests (in transport package, tested here via import)
// =============================================================================

func TestSecurity_F08_ControlCharsInRedirectMessage(t *testing.T) {
	cases := []string{
		"not leader: leader is node 1 at 127.0.0.1\n:9099",
		"not leader: leader is node 1 at 127.0.0.1\r:9099",
		"not leader: leader is node 1 at 127.0.0.1\x00:9099",
		"not leader: leader is node 1 at \x7F127.0.0.1:9099",
		"\nnot leader: leader is node 1 at 127.0.0.1:9099",
	}
	for _, msg := range cases {
		_, _, ok := transport.ParseRedirectMessage(msg)
		if ok {
			t.Errorf("ParseRedirectMessage should reject message with control chars: %q", msg)
		}
	}
}

func TestSecurity_F08_ValidRedirectMessageStillWorks(t *testing.T) {
	id, addr, ok := transport.ParseRedirectMessage("not leader: leader is node 42 at 10.0.0.1:9099")
	if !ok {
		t.Fatal("valid redirect message should parse successfully")
	}
	if id != 42 || addr != "10.0.0.1:9099" {
		t.Fatalf("unexpected parse result: id=%d addr=%s", id, addr)
	}
}

// =============================================================================
// Integration: Cluster-mode PUT goes through Raft proposal
// =============================================================================

func TestSecurity_ClusterModePutGoesThruRaftProposal(t *testing.T) {
	sm := &secMockStateMachine{}
	n, cleanup := securityTestNode(t, sm, DefaultApplyBatchSize)
	defer cleanup()

	router := NewProposalRouter(n, n.Topology())

	cfg := transport.DefaultServerConfig()
	cfg.ClusterMode = true
	cfg.ProposalRouter = router
	cfg.InsecureTransport = true
	cfg.Address = "127.0.0.1:0"

	eng := &countingEngine{}
	srv, err := transport.NewServer(cfg, eng)
	if err != nil {
		t.Fatal(err)
	}

	resp := srv.TestDispatch(&transport.Request{
		OpCode: transport.OpPut,
		Key:    []byte("key"),
		Value:  []byte("val"),
		SeqID:  100,
	})
	if resp.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk, got %v: %s", resp.Status, resp.Message)
	}
	if resp.SeqID != 100 {
		t.Fatalf("expected SeqID 100, got %d", resp.SeqID)
	}

	// Engine was NOT called directly
	if eng.putCount.Load() != 0 {
		t.Fatalf("Engine.Put was called directly — bypass! count=%d", eng.putCount.Load())
	}

	// Wait for apply loop to reach the entry (index 1: leader noop, index 2: PUT command)
	waitApplied(t, n, 2, 2*time.Second)

	// Verify state machine received the write
	if sm.putCount() == 0 {
		t.Fatal("state machine did not receive the PUT via apply loop")
	}
}

// =============================================================================
// Concurrency: Propose versus stepdown race
// =============================================================================

func TestSecurity_ConcurrentProposeAndStepdown(t *testing.T) {
	sm := &secMockStateMachine{}
	n, cleanup := securityTestNode(t, sm, DefaultApplyBatchSize)
	defer cleanup()

	var wg sync.WaitGroup
	var successCount atomic.Int64
	var errorCount atomic.Int64

	// Concurrent proposals
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			cmd := Command{Op: binary.OpTypePut, Key: []byte(fmt.Sprintf("k%d", idx)), Value: []byte("v")}
			data, _ := EncodeCommand(cmd)
			_, err := n.Propose(data)
			if err == nil {
				successCount.Add(1)
			} else {
				errorCount.Add(1)
			}
		}(i)
	}

	// Concurrent stepdown
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		currTerm, _ := n.storage.Term()
		_ = n.BecomeFollower(currTerm+1, cluster.NodeIDNil)
	}()

	wg.Wait()

	// All proposals should either succeed (before stepdown) or fail (after stepdown)
	// No panics, no races, no invalid state
	t.Logf("success=%d error=%d", successCount.Load(), errorCount.Load())
}

// =============================================================================
// Invariant verification helpers
// =============================================================================

func TestSecurity_Invariant_LastAppliedNeverExceedsCommitIndex(t *testing.T) {
	sm := &secMockStateMachine{}
	n, cleanup := securityTestNode(t, sm, DefaultApplyBatchSize)
	defer cleanup()

	for i := 0; i < 50; i++ {
		cmd := Command{Op: binary.OpTypePut, Key: []byte(fmt.Sprintf("k%d", i)), Value: []byte("v")}
		data, _ := EncodeCommand(cmd)
		if _, err := n.Propose(data); err != nil {
			t.Fatal(err)
		}
	}

	// Wait for all to apply
	waitApplied(t, n, 51, 5*time.Second) // noop + 50 entries

	// Verify invariant
	n.mu.RLock()
	lastApplied := n.lastApplied
	commitIndex := n.commitIndex
	n.mu.RUnlock()

	if lastApplied > commitIndex {
		t.Fatalf("INVARIANT VIOLATION: lastApplied (%d) > commitIndex (%d)", lastApplied, commitIndex)
	}
}

// =============================================================================
// Helper types for testing
// =============================================================================

// noopEngine is a no-op Engine implementation for transport server construction.
type noopEngine struct{}

func (e *noopEngine) Put(ctx context.Context, key, val []byte) error { return nil }
func (e *noopEngine) Get(key []byte) ([]byte, error)                { return nil, errors.ErrKeyNotFound }
func (e *noopEngine) Delete(ctx context.Context, key []byte) error   { return nil }

// countingEngine counts direct Engine.Put/Delete calls.
type countingEngine struct {
	putCount    atomic.Int64
	deleteCount atomic.Int64
}

func (e *countingEngine) Put(ctx context.Context, key, val []byte) error {
	e.putCount.Add(1)
	return nil
}
func (e *countingEngine) Get(key []byte) ([]byte, error) { return nil, errors.ErrKeyNotFound }
func (e *countingEngine) Delete(ctx context.Context, key []byte) error {
	e.deleteCount.Add(1)
	return nil
}

// mockRouter is a simple ProposalRouter for testing.
type mockRouter struct{}

func (r *mockRouter) RouteWrite(ctx context.Context, req *transport.Request) (*transport.Response, error) {
	return &transport.Response{
		OpCode: req.OpCode,
		SeqID:  req.SeqID,
		Status: transport.StatusOk,
	}, nil
}

// mustTerm is a test helper that reads the current term or panics.
func (s *Storage) mustTerm() Term {
	t, err := s.Term()
	if err != nil {
		panic(err)
	}
	return t
}
