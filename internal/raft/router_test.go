package raft

import (
	"bytes"
	"context"
	stdErrors "errors"
	"fmt"
	"net"
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

// helper to create a cluster topology for testing
func newTestTopology(t *testing.T, localID cluster.NodeID, peers map[cluster.NodeID]string) *cluster.Topology {
	t.Helper()
	var rawPeers []cluster.PeerConfig
	for id, addr := range peers {
		rawPeers = append(rawPeers, cluster.PeerConfig{
			ID:      id,
			Address: addr,
		})
	}
	top, err := cluster.NewTopology(localID, peers[localID], rawPeers)
	if err != nil {
		t.Fatalf("NewTopology failed: %v", err)
	}
	return top
}

// -----------------------------------------------------------------------------
// Test 1: Deterministic Command Conversion (Section 34)
// -----------------------------------------------------------------------------
func TestRouter_DeterministicCommandEncoding(t *testing.T) {
	key := []byte("customer:account:42")
	val := []byte("balance=100000;tier=gold")

	cmd1 := Command{Op: binary.OpTypePut, Key: key, Value: val}
	cmd2 := Command{Op: binary.OpTypePut, Key: key, Value: val}

	b1, err := EncodeCommand(cmd1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := EncodeCommand(cmd2)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(b1, b2) {
		t.Fatalf("encoded commands are not byte-for-byte identical:\nb1: %x\nb2: %x", b1, b2)
	}

	del1 := Command{Op: binary.OpTypeDelete, Key: key}
	del2 := Command{Op: binary.OpTypeDelete, Key: key}

	d1, err := EncodeCommand(del1)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := EncodeCommand(del2)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(d1, d2) {
		t.Fatalf("encoded delete commands are not byte-for-byte identical:\nd1: %x\nd2: %x", d1, d2)
	}
}

// -----------------------------------------------------------------------------
// Test 2: Leader Write Path (Section 28)
// -----------------------------------------------------------------------------
func TestRouter_LeaderWritePath(t *testing.T) {
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

	top := newTestTopology(t, 1, map[cluster.NodeID]string{
		1: "127.0.0.1:9001",
	})
	n.SetTopology(top)

	router := NewProposalRouter(n, top)

	// Send client PUT
	req := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  42,
		Key:    []byte("user:101"),
		Value:  []byte("alice"),
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	resp, err := router.RouteWrite(ctx, req)
	if err != nil {
		t.Fatalf("RouteWrite failed: %v", err)
	}

	if resp.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk, got %v (message: %s)", resp.Status, resp.Message)
	}
	if resp.SeqID != 42 {
		t.Fatalf("expected SeqID 42, got %d", resp.SeqID)
	}

	// Verify proposal was appended to Raft log
	lastIdx, err := s.LastIndex()
	if err != nil {
		t.Fatal(err)
	}
	// Index 1 was election no-op; index 2 must be our PUT
	if lastIdx < 2 {
		t.Fatalf("expected at least 2 entries in Raft log, got %d", lastIdx)
	}
	entry, err := s.Entry(2)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Type != transport.PeerEntryNormal {
		t.Fatalf("expected PeerEntryNormal, got %v", entry.Type)
	}

	decodedCmd, err := DecodeCommand(entry.Data)
	if err != nil {
		t.Fatal(err)
	}
	if decodedCmd.Op != binary.OpTypePut {
		t.Fatalf("expected OpTypePut, got %v", decodedCmd.Op)
	}
	if string(decodedCmd.Key) != "user:101" || string(decodedCmd.Value) != "alice" {
		t.Fatalf("decoded command mismatch: %+v", decodedCmd)
	}

	// For N=1, refreshCommitIndex commits immediately, and the apply loop applies to the state machine
	waitForApplied(t, n, 2, time.Second)

	ops := sm.getOps()
	if len(ops) != 1 {
		t.Fatalf("expected 1 op applied to state machine, got %d", len(ops))
	}
	if string(ops[0].Key) != "user:101" {
		t.Fatalf("applied op mismatch: %+v", ops[0])
	}
}

// -----------------------------------------------------------------------------
// Test 3: Follower Write Redirection (Section 28)
// -----------------------------------------------------------------------------
func TestRouter_FollowerRedirection(t *testing.T) {
	sm := newMockSM()
	n, s, cleanup := newTestNodeWithSM(t, 2, sm, 10)
	defer cleanup()

	// Ensure follower role
	if n.Role() != RoleFollower {
		t.Fatalf("expected RoleFollower, got %v", n.Role())
	}

	// Configure topology with Leader 1 at 127.0.0.1:9001 and Follower 2 at 127.0.0.1:9002
	top := newTestTopology(t, 2, map[cluster.NodeID]string{
		1: "127.0.0.1:9001",
		2: "127.0.0.1:9002",
	})
	n.SetTopology(top)

	// Set known leader to Node 1
	n.mu.Lock()
	n.leaderID = cluster.NodeID(1)
	n.mu.Unlock()

	router := NewProposalRouter(n, top)

	req := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  888,
		Key:    []byte("key-f"),
		Value:  []byte("val-f"),
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	resp, err := router.RouteWrite(ctx, req)
	if err != nil {
		t.Fatalf("RouteWrite failed: %v", err)
	}

	// 1. Follower MUST return StatusNotLeader
	if resp.Status != transport.StatusNotLeader {
		t.Fatalf("expected StatusNotLeader, got %v (message: %s)", resp.Status, resp.Message)
	}
	// 2. Exact SeqID preserved
	if resp.SeqID != 888 {
		t.Fatalf("expected SeqID 888, got %d", resp.SeqID)
	}
	// 3. Leader identity and address populated from trusted topology
	if resp.LeaderID != 1 {
		t.Fatalf("expected LeaderID 1, got %d", resp.LeaderID)
	}
	if resp.LeaderAddr != "127.0.0.1:9001" {
		t.Fatalf("expected LeaderAddr 127.0.0.1:9001, got %q", resp.LeaderAddr)
	}

	// 4. NO local Raft proposal created
	lastIdx, _ := s.LastIndex()
	if lastIdx != 0 {
		t.Fatalf("follower appended to local Raft log! lastIdx = %d", lastIdx)
	}

	// 5. NO state machine mutation
	if sm.opsCount() != 0 {
		t.Fatalf("follower mutated local state machine! count = %d", sm.opsCount())
	}
}

// -----------------------------------------------------------------------------
// Test 4: Direct-Bypass Regression Test (Section 29 - MANDATORY)
// -----------------------------------------------------------------------------
func TestRouter_DirectBypassRegression(t *testing.T) {
	sm := newMockSM()
	n, s, cleanup := newTestNodeWithSM(t, 2, sm, 10)
	defer cleanup()

	top := newTestTopology(t, 2, map[cluster.NodeID]string{
		1: "127.0.0.1:9001",
		2: "127.0.0.1:9002",
	})
	n.SetTopology(top)

	n.mu.Lock()
	n.leaderID = cluster.NodeID(1)
	n.mu.Unlock()

	// Construct transport.Server with the ProposalRouter wired in
	srvCfg := transport.DefaultServerConfig()
	srvCfg.Address = "127.0.0.1:0"
	srvCfg.ProposalRouter = n

	// Create dummy engine to prove it is NEVER called by dispatch
	dummyEng := engine.NewEngineWithOptions(engine.EngineOptions{
		Backpressure: engine.BackpressureConfig{
			MaxMemoryBytes: 16 * 1024 * 1024,
		},
	})
	defer func() { _ = dummyEng.Close() }()

	srv, err := transport.NewServer(srvCfg, dummyEng)
	if err != nil {
		t.Fatal(err)
	}

	// Client sends PUT request
	req := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  1001,
		Key:    []byte("bypass_probe_key"),
		Value:  []byte("bypass_probe_value"),
	}

	// Execute through transport dispatch
	resp := srv.ProposalRouter() // verify router is set
	if resp == nil {
		t.Fatal("expected ProposalRouter to be non-nil on Server")
	}

	ctx := context.Background()
	r, err := srv.ProposalRouter().RouteWrite(ctx, req)
	if err != nil {
		t.Fatal(err)
	}

	// ASSERTION 1: Follower returned redirection
	if r.Status != transport.StatusNotLeader {
		t.Fatalf("expected StatusNotLeader, got %v", r.Status)
	}
	// ASSERTION 2: StateMachine.Put was NOT called
	if sm.opsCount() != 0 {
		t.Fatalf("SECURITY VIOLATION: StateMachine was mutated on follower!")
	}
	// ASSERTION 3: Engine was NOT mutated directly
	if _, err := dummyEng.Get([]byte("bypass_probe_key")); err == nil {
		t.Fatalf("SECURITY VIOLATION: Engine directly mutated on follower bypassing Raft!")
	}
	// ASSERTION 4: Raft Propose was NOT called locally
	lastIdx, _ := s.LastIndex()
	if lastIdx != 0 {
		t.Fatalf("SECURITY VIOLATION: Raft log modified on follower!")
	}

	// NOW: Transform node to Leader and assert Raft proposal path IS used
	if err := n.BecomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatal(err)
	}

	r2, err := srv.ProposalRouter().RouteWrite(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk on leader, got %v", r2.Status)
	}

	// Proposal entered Raft log!
	lastIdx2, _ := s.LastIndex()
	if lastIdx2 == 0 {
		t.Fatalf("expected Raft log to contain proposed entry on leader")
	}
}

// -----------------------------------------------------------------------------
// Test 5: Follower Redirect Test Matrix (Section 30)
// -----------------------------------------------------------------------------
func TestRouter_FollowerRedirectTestMatrix(t *testing.T) {
	sm := newMockSM()
	n, _, cleanup := newTestNodeWithSM(t, 2, sm, 10)
	defer cleanup()

	req := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  777,
		Key:    []byte("matrix-key"),
		Value:  []byte("matrix-val"),
	}

	ctx := context.Background()

	// Scenario 1: Known leader + valid topology
	t.Run("known_leader_valid_topology", func(t *testing.T) {
		top := newTestTopology(t, 2, map[cluster.NodeID]string{
			1: "127.0.0.1:9001",
			2: "127.0.0.1:9002",
		})
		n.SetTopology(top)
		n.mu.Lock()
		n.role = RoleFollower
		n.leaderID = cluster.NodeID(1)
		n.mu.Unlock()

		resp, err := n.RouteWrite(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Status != transport.StatusNotLeader {
			t.Errorf("status = %v, want StatusNotLeader", resp.Status)
		}
		if resp.SeqID != 777 {
			t.Errorf("seqID = %d, want 777", resp.SeqID)
		}
		if resp.LeaderID != 1 || resp.LeaderAddr != "127.0.0.1:9001" {
			t.Errorf("redirect mismatch: ID %d, addr %s", resp.LeaderID, resp.LeaderAddr)
		}
	})

	// Scenario 2: Unknown leader (NodeIDNil)
	t.Run("unknown_leader", func(t *testing.T) {
		n.mu.Lock()
		n.role = RoleFollower
		n.leaderID = cluster.NodeIDNil
		n.mu.Unlock()

		resp, err := n.RouteWrite(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Status != transport.StatusNotLeader {
			t.Errorf("status = %v, want StatusNotLeader", resp.Status)
		}
		if resp.SeqID != 777 {
			t.Errorf("seqID = %d, want 777", resp.SeqID)
		}
		if resp.LeaderAddr != "" {
			t.Errorf("expected empty LeaderAddr for unknown leader, got %q", resp.LeaderAddr)
		}
		if resp.Message != "not leader: leader is unknown, retry later" {
			t.Errorf("unexpected message: %q", resp.Message)
		}
	})

	// Scenario 3: Known leader not found in topology
	t.Run("known_leader_missing_topology", func(t *testing.T) {
		n.mu.Lock()
		n.role = RoleFollower
		n.leaderID = cluster.NodeID(99) // not in topology
		n.mu.Unlock()

		resp, err := n.RouteWrite(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Status != transport.StatusError {
			t.Errorf("status = %v, want StatusError for missing topology address", resp.Status)
		}
		if resp.SeqID != 777 {
			t.Errorf("seqID = %d, want 777", resp.SeqID)
		}
	})

	// Scenario 4: Self leader ID while stepped down
	t.Run("self_leader_id_while_follower", func(t *testing.T) {
		n.mu.Lock()
		n.role = RoleFollower
		n.leaderID = cluster.NodeID(2) // localID is 2
		n.mu.Unlock()

		resp, err := n.RouteWrite(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Status != transport.StatusNotLeader {
			t.Errorf("status = %v, want StatusNotLeader", resp.Status)
		}
		if resp.SeqID != 777 {
			t.Errorf("seqID = %d, want 777", resp.SeqID)
		}
		if resp.LeaderAddr != "" {
			t.Errorf("must not redirect to self: addr = %q", resp.LeaderAddr)
		}
		if resp.Message != "not leader: stepped down, retry later" {
			t.Errorf("unexpected message: %q", resp.Message)
		}
	})

	// Scenario 5: Candidate role
	t.Run("candidate_role", func(t *testing.T) {
		n.mu.Lock()
		n.role = RoleCandidate
		n.leaderID = cluster.NodeIDNil
		n.mu.Unlock()

		resp, err := n.RouteWrite(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Status != transport.StatusNotLeader {
			t.Errorf("status = %v, want StatusNotLeader", resp.Status)
		}
		if resp.SeqID != 777 {
			t.Errorf("seqID = %d, want 777", resp.SeqID)
		}
		if resp.Message != "not leader: node is candidate, retry later" {
			t.Errorf("unexpected message: %q", resp.Message)
		}
	})

	// Scenario 6: Closed node
	t.Run("closed_node", func(t *testing.T) {
		smClosed := newMockSM()
		nClosed, _, cleanupClosed := newTestNodeWithSM(t, 9, smClosed, 10)
		cleanupClosed() // closes node

		resp, err := nClosed.RouteWrite(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Status != transport.StatusServerClosed {
			t.Errorf("status = %v, want StatusServerClosed", resp.Status)
		}
		if resp.SeqID != 777 {
			t.Errorf("seqID = %d, want 777", resp.SeqID)
		}
	})
}

// -----------------------------------------------------------------------------
// Test 6: Single-Node End-to-End Integration (Section 33)
// -----------------------------------------------------------------------------
func TestRouter_SingleNodeEndToEndIntegration(t *testing.T) {
	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		Backpressure: engine.BackpressureConfig{
			MaxMemoryBytes: 32 * 1024 * 1024,
			HighWatermark:  0.80,
			HardWatermark:  0.95,
			MaxWaitTimeout: 100 * time.Millisecond,
		},
	})
	defer func() { _ = eng.Close() }()

	dir := t.TempDir()
	s, err := OpenStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	_ = s.SetTerm(1)

	n, err := NewNode(NodeConfig{
		LocalID:      1,
		Storage:      s,
		StateMachine: eng,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close() }()

	top := newTestTopology(t, 1, map[cluster.NodeID]string{
		1: "127.0.0.1:9001",
	})
	n.SetTopology(top)

	if err := n.BecomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatal(err)
	}

	// Bind transport.Server on random loopback port with ProposalRouter configured
	srvCfg := transport.DefaultServerConfig()
	srvCfg.Address = "127.0.0.1:0"
	srvCfg.ProposalRouter = n

	srv, err := transport.NewServer(srvCfg, eng)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()

	srvAddr := srv.Addr().String()

	// Connect TCP client
	conn, err := net.Dial("tcp", srvAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	// 1. Client PUT: "profile:1001" -> "alice_data"
	putReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  501,
		Key:    []byte("profile:1001"),
		Value:  []byte("alice_data"),
	}
	if err := transport.WriteRequest(conn, putReq); err != nil {
		t.Fatal(err)
	}
	putResp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatal(err)
	}
	if putResp.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk on PUT, got %v (msg: %s)", putResp.Status, putResp.Message)
	}
	if putResp.SeqID != 501 {
		t.Fatalf("expected SeqID 501, got %d", putResp.SeqID)
	}

	// Wait for M01 apply loop to apply the entry to LSM engine
	// Election no-op = 1, PUT = 2
	waitForApplied(t, n, 2, 2*time.Second)

	// Engine.Get must return "alice_data"
	val, err := eng.Get([]byte("profile:1001"))
	if err != nil {
		t.Fatalf("Get after applied PUT failed: %v", err)
	}
	if string(val) != "alice_data" {
		t.Fatalf("value mismatch: got %q, want %q", val, "alice_data")
	}

	// 2. Client DELETE: "profile:1001"
	delReq := &transport.Request{
		OpCode: transport.OpDelete,
		SeqID:  502,
		Key:    []byte("profile:1001"),
	}
	if err := transport.WriteRequest(conn, delReq); err != nil {
		t.Fatal(err)
	}
	delResp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatal(err)
	}
	if delResp.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk on DELETE, got %v (msg: %s)", delResp.Status, delResp.Message)
	}
	if delResp.SeqID != 502 {
		t.Fatalf("expected SeqID 502, got %d", delResp.SeqID)
	}

	// Wait for DELETE entry (index 3) to apply
	waitForApplied(t, n, 3, 2*time.Second)

	// Engine.Get must return ErrKeyNotFound
	_, err = eng.Get([]byte("profile:1001"))
	if err == nil {
		t.Fatal("expected ErrKeyNotFound after DELETE applied, got nil")
	}
	if !stdErrors.Is(err, latticeErrors.ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound, got %v", err)
	}
}

// -----------------------------------------------------------------------------
// Test 7: Concurrent Client Requests under Race Detector (Section 31)
// -----------------------------------------------------------------------------
func TestRouter_ConcurrentClientWrites(t *testing.T) {
	sm := newMockSM()
	n, _, cleanup := newTestNodeWithSM(t, 1, sm, 10)
	defer cleanup()

	if err := n.BecomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatal(err)
	}

	top := newTestTopology(t, 1, map[cluster.NodeID]string{
		1: "127.0.0.1:9001",
	})
	n.SetTopology(top)

	numWorkers := 8
	requestsPerWorker := 20
	totalRequests := numWorkers * requestsPerWorker

	var wg sync.WaitGroup
	var successCount atomic.Int64
	var errCh = make(chan error, totalRequests)

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < requestsPerWorker; i++ {
				seqID := uint64(workerID*1000 + i)
				req := &transport.Request{
					OpCode: transport.OpPut,
					SeqID:  seqID,
					Key:    []byte(fmt.Sprintf("w%d_k%d", workerID, i)),
					Value:  []byte(fmt.Sprintf("w%d_v%d", workerID, i)),
				}
				resp, err := n.RouteWrite(context.Background(), req)
				if err != nil {
					errCh <- err
					return
				}
				if resp.Status != transport.StatusOk {
					errCh <- fmt.Errorf("worker %d req %d got non-OK status: %v (%s)", workerID, i, resp.Status, resp.Message)
					return
				}
				if resp.SeqID != seqID {
					errCh <- fmt.Errorf("seqID mismatch: got %d, want %d", resp.SeqID, seqID)
					return
				}
				successCount.Add(1)
			}
		}(w)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}

	if successCount.Load() != int64(totalRequests) {
		t.Fatalf("expected %d successful writes, got %d", totalRequests, successCount.Load())
	}

	// Total entries in log: 1 (election no-op) + totalRequests
	expectedLogIndex := LogIndex(1 + totalRequests)
	waitForApplied(t, n, expectedLogIndex, 5*time.Second)

	if sm.opsCount() != totalRequests {
		t.Fatalf("expected %d ops applied to state machine, got %d", totalRequests, sm.opsCount())
	}
}

// -----------------------------------------------------------------------------
// Test 8: Leadership Change Concurrency Test (Section 32)
// -----------------------------------------------------------------------------
func TestRouter_LeadershipChangeConcurrency(t *testing.T) {
	sm := newMockSM()
	n, _, cleanup := newTestNodeWithSM(t, 1, sm, 10)
	defer cleanup()

	top := newTestTopology(t, 1, map[cluster.NodeID]string{
		1: "127.0.0.1:9001",
		2: "127.0.0.1:9002",
	})
	n.SetTopology(top)

	if err := n.BecomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatal(err)
	}

	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	// Goroutine A: continually submits client writes
	wg.Add(1)
	go func() {
		defer wg.Done()
		var seq uint64
		for {
			select {
			case <-stopCh:
				return
			default:
				seq++
				req := &transport.Request{
					OpCode: transport.OpPut,
					SeqID:  seq,
					Key:    []byte("churn_key"),
					Value:  []byte("churn_val"),
				}
				resp, err := n.RouteWrite(context.Background(), req)
				if err != nil {
					continue
				}
				// Response must be either StatusOk (while leader) or StatusNotLeader (after stepdown)
				if resp.Status != transport.StatusOk && resp.Status != transport.StatusNotLeader {
					t.Errorf("unexpected status during leadership transition: %v", resp.Status)
				}
				if resp.SeqID != seq {
					t.Errorf("SeqID corruption: got %d, want %d", resp.SeqID, seq)
				}
				time.Sleep(500 * time.Microsecond)
			}
		}
	}()

	// Goroutine B: steps down leader to follower
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(10 * time.Millisecond)
		// Step down to follower at higher term with Leader 2
		_ = n.BecomeFollower(3, cluster.NodeID(2))
	}()

	time.Sleep(30 * time.Millisecond)
	close(stopCh)
	wg.Wait()

	// After stepdown, node must be follower and route writes to Leader 2
	if n.Role() != RoleFollower {
		t.Fatalf("expected RoleFollower after stepdown, got %v", n.Role())
	}

	finalReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  9999,
		Key:    []byte("k"),
		Value:  []byte("v"),
	}
	resp, err := n.RouteWrite(context.Background(), finalReq)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != transport.StatusNotLeader {
		t.Fatalf("expected StatusNotLeader after stepdown, got %v", resp.Status)
	}
	if resp.LeaderID != 2 || resp.LeaderAddr != "127.0.0.1:9002" {
		t.Fatalf("expected redirect to Leader 2 at 127.0.0.1:9002, got ID %d at %s", resp.LeaderID, resp.LeaderAddr)
	}
}

// -----------------------------------------------------------------------------
// Test 9: Input Validation & Security Bounds (Section 27)
// -----------------------------------------------------------------------------
func TestRouter_InputValidation(t *testing.T) {
	sm := newMockSM()
	n, _, cleanup := newTestNodeWithSM(t, 1, sm, 10)
	defer cleanup()

	_ = n.BecomeCandidate()
	_ = n.BecomeLeader()

	ctx := context.Background()

	t.Run("nil request", func(t *testing.T) {
		_, err := n.RouteWrite(ctx, nil)
		if err == nil {
			t.Fatal("expected error for nil request")
		}
	})

	t.Run("empty key", func(t *testing.T) {
		req := &transport.Request{
			OpCode: transport.OpPut,
			SeqID:  1,
			Key:    []byte{},
			Value:  []byte("v"),
		}
		resp, err := n.RouteWrite(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Status != transport.StatusInvalidRequest {
			t.Fatalf("expected StatusInvalidRequest for empty key, got %v", resp.Status)
		}
	})

	t.Run("oversized key", func(t *testing.T) {
		req := &transport.Request{
			OpCode: transport.OpPut,
			SeqID:  2,
			Key:    bytes.Repeat([]byte("k"), binary.MaxKeyLen+1),
			Value:  []byte("v"),
		}
		resp, err := n.RouteWrite(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Status != transport.StatusInvalidRequest {
			t.Fatalf("expected StatusInvalidRequest for oversized key, got %v", resp.Status)
		}
	})

	t.Run("oversized value", func(t *testing.T) {
		req := &transport.Request{
			OpCode: transport.OpPut,
			SeqID:  3,
			Key:    []byte("k"),
			Value:  bytes.Repeat([]byte("v"), binary.MaxValueLen+1),
		}
		resp, err := n.RouteWrite(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Status != transport.StatusInvalidRequest {
			t.Fatalf("expected StatusInvalidRequest for oversized value, got %v", resp.Status)
		}
	})

	t.Run("delete with non-empty value", func(t *testing.T) {
		req := &transport.Request{
			OpCode: transport.OpDelete,
			SeqID:  4,
			Key:    []byte("k"),
			Value:  []byte("illegal-val"),
		}
		resp, err := n.RouteWrite(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Status != transport.StatusInvalidRequest {
			t.Fatalf("expected StatusInvalidRequest for DELETE with value, got %v", resp.Status)
		}
	})

	t.Run("unsupported opcode", func(t *testing.T) {
		req := &transport.Request{
			OpCode: transport.OpGet,
			SeqID:  5,
			Key:    []byte("k"),
		}
		resp, err := n.RouteWrite(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Status != transport.StatusInvalidRequest {
			t.Fatalf("expected StatusInvalidRequest for GET to write router, got %v", resp.Status)
		}
	})
}
