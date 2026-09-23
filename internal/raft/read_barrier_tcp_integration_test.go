package raft_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/raft"
	"github.com/silent-knight19/lattice/internal/transport"
)

// controllableEngineSM implements both raft.StateMachine and transport.Engine
// with deterministic pause/lag hooks for testing read barriers.
type controllableEngineSM struct {
	mu           sync.Mutex
	data         map[string][]byte
	pauseOnKey   string
	applyEntered chan struct{}
	applyRelease chan struct{}
}

func newControllableEngineSM() *controllableEngineSM {
	return &controllableEngineSM{
		data:         make(map[string][]byte),
		applyEntered: make(chan struct{}, 10),
		applyRelease: make(chan struct{}, 10),
	}
}

func (c *controllableEngineSM) SetPauseKey(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pauseOnKey = key
}

func (c *controllableEngineSM) Put(ctx context.Context, key, val []byte) error {
	c.mu.Lock()
	pause := (c.pauseOnKey != "" && string(key) == c.pauseOnKey)
	c.mu.Unlock()

	if pause {
		select {
		case c.applyEntered <- struct{}{}:
		default:
		}
		select {
		case <-c.applyRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]byte, len(val))
	copy(cp, val)
	c.data[string(key)] = cp
	return nil
}

func (c *controllableEngineSM) Delete(ctx context.Context, key []byte) error {
	c.mu.Lock()
	pause := (c.pauseOnKey != "" && string(key) == c.pauseOnKey)
	c.mu.Unlock()

	if pause {
		select {
		case c.applyEntered <- struct{}{}:
		default:
		}
		select {
		case <-c.applyRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, string(key))
	return nil
}

func (c *controllableEngineSM) Get(key []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.data[string(key)]
	if !ok {
		return nil, errors.ErrKeyNotFound
	}
	cp := make([]byte, len(v))
	copy(cp, v)
	return cp, nil
}

type barrierTCPNode struct {
	id      cluster.NodeID
	addr    string
	ln      net.Listener
	mgr     *transport.PeerConnectionManager
	storage *raft.Storage
	node    *raft.Node
	sm      *controllableEngineSM
	router  *raft.ProposalRouter
	server  *transport.Server
}

type barrierTCPCluster struct {
	nodes map[cluster.NodeID]*barrierTCPNode
}

func createBarrierTCPCluster(t *testing.T, count int, senderWrapper func(id cluster.NodeID, inner raft.PeerSender) raft.PeerSender) (*barrierTCPCluster, func()) {
	t.Helper()

	peerListeners := make(map[cluster.NodeID]net.Listener, count)
	peerAddrs := make(map[cluster.NodeID]string, count)

	for i := 1; i <= count; i++ {
		id := cluster.NodeID(i)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("failed to bind tcp listener for node %d: %v", id, err)
		}
		peerListeners[id] = ln
		peerAddrs[id] = ln.Addr().String()
	}

	var rawPeers []cluster.PeerConfig
	for i := 1; i <= count; i++ {
		id := cluster.NodeID(i)
		rawPeers = append(rawPeers, cluster.PeerConfig{
			ID:      id,
			Address: peerAddrs[id],
		})
	}

	clusterObj := &barrierTCPCluster{
		nodes: make(map[cluster.NodeID]*barrierTCPNode, count),
	}

	for i := 1; i <= count; i++ {
		id := cluster.NodeID(i)
		topo, err := cluster.NewTopology(id, peerAddrs[id], rawPeers)
		if err != nil {
			t.Fatalf("failed to create topology for node %d: %v", id, err)
		}

		storageDir := filepath.Join(t.TempDir(), fmt.Sprintf("raft-storage-%d", id))
		if err := os.MkdirAll(storageDir, 0750); err != nil {
			t.Fatalf("failed to create storage dir for node %d: %v", id, err)
		}
		st, err := raft.OpenStorage(storageDir)
		if err != nil {
			t.Fatalf("failed to open storage for node %d: %v", id, err)
		}

		sm := newControllableEngineSM()

		nodeEntry := &barrierTCPNode{
			id:      id,
			addr:    peerAddrs[id],
			ln:      peerListeners[id],
			storage: st,
			sm:      sm,
		}

		peerCfg := transport.DefaultPeerConnectionConfig()
		peerCfg.InsecureTransport = true
		peerCfg.DialTimeout = 2 * time.Second
		peerCfg.ReconnectMin = 20 * time.Millisecond
		peerCfg.ReconnectMax = 100 * time.Millisecond

		peerCfg.OnFrameReceived = func(peerID cluster.NodeID, frame *transport.Frame) {
			if nodeEntry.node != nil {
				_ = nodeEntry.node.HandlePeerFrame(peerID, frame)
			}
		}

		mgr, err := transport.NewPeerConnectionManager(topo, peerCfg)
		if err != nil {
			t.Fatalf("failed to create PeerConnectionManager for node %d: %v", id, err)
		}
		nodeEntry.mgr = mgr

		if err := mgr.ServeListener(peerListeners[id]); err != nil {
			t.Fatalf("failed to serve listener for node %d: %v", id, err)
		}

		var peerSender raft.PeerSender = mgr
		if senderWrapper != nil {
			peerSender = senderWrapper(id, mgr)
		}

		raftCfg := raft.NodeConfig{
			LocalID:      id,
			Storage:      st,
			Topology:     topo,
			StateMachine: sm,
			PeerSender:   peerSender,
		}
		rNode, err := raft.NewNode(raftCfg)
		if err != nil {
			t.Fatalf("failed to create raft.Node for node %d: %v", id, err)
		}
		nodeEntry.node = rNode

		router := raft.NewProposalRouter(rNode, topo, sm)
		nodeEntry.router = router

		// Create client-facing transport.Server in cluster mode
		srvCfg := transport.DefaultServerConfig()
		srvCfg.ClusterMode = true
		srvCfg.RequestTimeout = 500 * time.Millisecond
		srvCfg.ProposalRouter = router
		srvCfg.ReadRouter = router
		srv, err := transport.NewServer(srvCfg, sm)
		if err != nil {
			t.Fatalf("failed to create transport.Server for node %d: %v", id, err)
		}
		if err := srv.Listen("127.0.0.1:0"); err != nil {
			t.Fatalf("failed to start transport.Server listener for node %d: %v", id, err)
		}
		nodeEntry.server = srv

		if err := mgr.Start(); err != nil {
			t.Fatalf("failed to start PeerConnectionManager for node %d: %v", id, err)
		}

		clusterObj.nodes[id] = nodeEntry
	}

	cleanup := func() {
		for i := 1; i <= count; i++ {
			id := cluster.NodeID(i)
			if n, ok := clusterObj.nodes[id]; ok {
				if n.server != nil {
					_ = n.server.Close()
				}
				if n.mgr != nil {
					_ = n.mgr.Close()
				}
				if n.node != nil {
					_ = n.node.Close()
				}
				if n.storage != nil {
					_ = n.storage.Close()
				}
			}
		}
	}

	return clusterObj, cleanup
}

func syncFollowersFromLeader(t *testing.T, leader *barrierTCPNode, followers ...*barrierTCPNode) {
	t.Helper()
	leaderLast, lastTerm, err := leader.storage.LastIndexAndTerm()
	if err != nil {
		t.Fatalf("failed to get leader log coordinates: %v", err)
	}

	for _, f := range followers {
		fLast, err := f.storage.LastIndex()
		if err != nil {
			t.Fatalf("failed to get follower %d last index: %v", f.id, err)
		}
		if leaderLast <= fLast {
			continue
		}
		for idx := fLast + 1; idx <= leaderLast; idx++ {
			curLast, _ := f.storage.LastIndex()
			if curLast >= idx {
				continue
			}
			e, err := leader.storage.Entry(idx)
			if err != nil {
				t.Fatalf("failed to read leader entry %d: %v", idx, err)
			}
			_ = f.storage.SetTerm(lastTerm)
			_ = f.storage.Append(e)
		}
	}
}

func waitForBarrierClusterConnections(t *testing.T, c *barrierTCPCluster, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		allConnected := true
		for i := 1; i <= count; i++ {
			id := cluster.NodeID(i)
			mgr := c.nodes[id].mgr
			for j := 1; j <= count; j++ {
				if i == j {
					continue
				}
				if !mgr.IsConnected(cluster.NodeID(j)) {
					allConnected = false
					break
				}
			}
			if !allConnected {
				break
			}
		}
		if allConnected {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for barrier cluster TCP connections to establish")
}

// -----------------------------------------------------------------------------
// Test 1: Real 3-Node TCP Cluster — Lagging State Machine Read Barrier (Section 11)
// -----------------------------------------------------------------------------
func TestReadBarrier_3Node_RealTCP_LaggingStateMachine(t *testing.T) {
	c, cleanup := createBarrierTCPCluster(t, 3, nil)
	defer cleanup()

	waitForBarrierClusterConnections(t, c, 3)

	node1 := c.nodes[1]
	node2 := c.nodes[2]
	node3 := c.nodes[3]

	// 1. Promote Node 1 to leader
	if err := node1.node.BecomeCandidate(); err != nil {
		t.Fatalf("failed to promote node 1 to candidate: %v", err)
	}
	if err := node1.node.BecomeLeader(); err != nil {
		t.Fatalf("failed to promote node 1 to leader: %v", err)
	}

	// 2. Sync leader's election no-op entry to followers so logs match
	syncFollowersFromLeader(t, node1, node2, node3)

	// Wait for Node 1 to apply election no-op entry
	for i := 0; i < 50; i++ {
		if node1.node.LastApplied() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 3. Configure Node 1 state machine to pause on key "user:profile"
	node1.sm.SetPauseKey("user:profile")

	// 4. Propose a write for "user:profile" = "v2_committed"
	writeReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  101,
		Key:    []byte("user:profile"),
		Value:  []byte("v2_committed"),
	}
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelWrite()

	writeResp, err := node1.router.RouteWrite(writeCtx, writeReq)
	if err != nil {
		t.Fatalf("RouteWrite failed: %v", err)
	}
	if writeResp.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk, got: %s (%s)", writeResp.Status, writeResp.Message)
	}

	// 5. Sync the new proposal entry to followers so follower logs match
	syncFollowersFromLeader(t, node1, node2, node3)

	// Simulate follower acks for the new entry so commitIndex advances on Node 1
	lastIdx, err := node1.storage.LastIndex()
	if err != nil {
		t.Fatal(err)
	}
	_ = node1.node.HandleAppendEntriesResponse(2, &transport.AppendEntriesResponse{
		Term:       1,
		Success:    true,
		MatchIndex: uint64(lastIdx),
	})

	if node1.node.CommitIndex() < lastIdx {
		t.Fatalf("expected commitIndex >= %d, got: %d", lastIdx, node1.node.CommitIndex())
	}

	// 6. Verify that state machine apply loop is paused on "user:profile"
	select {
	case <-node1.sm.applyEntered:
		// State machine paused in Put!
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for state machine to enter pause")
	}

	// At this point, commitIndex >= lastIdx, but lastApplied is still < lastIdx!
	if node1.node.LastApplied() >= lastIdx {
		t.Fatalf("expected lastApplied < %d while paused, got %d", lastIdx, node1.node.LastApplied())
	}

	// 7. Client connects to Node 1 transport.Server over real TCP and sends GET
	clientConn, err := net.Dial("tcp", node1.server.Addr().String())
	if err != nil {
		t.Fatalf("failed to dial client server: %v", err)
	}
	defer clientConn.Close()

	getReq := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  202,
		Key:    []byte("user:profile"),
	}

	type getResult struct {
		resp *transport.Response
		err  error
	}
	resultCh := make(chan getResult, 1)

	go func() {
		if err := transport.WriteRequest(clientConn, getReq); err != nil {
			resultCh <- getResult{err: err}
			return
		}
		resp, err := transport.ReadResponse(clientConn)
		resultCh <- getResult{resp: resp, err: err}
	}()

	// 8. Deterministic check: GET must NOT have completed yet because lastApplied < commitIndex
	select {
	case res := <-resultCh:
		t.Fatalf("linearizable GET returned prematurely before apply caught up! res: %+v, err: %v", res.resp, res.err)
	case <-time.After(150 * time.Millisecond):
		// Expected: GET is safely blocked on WaitForApplied!
	}

	// Verify state machine still has not applied
	if node1.node.LastApplied() >= lastIdx {
		t.Fatalf("lastApplied unexpectedly advanced: %d", node1.node.LastApplied())
	}

	// 9. Now unpause the state machine
	node1.sm.applyRelease <- struct{}{}

	// 10. GET must now unblock and return the committed value "v2_committed"
	select {
	case res := <-resultCh:
		if res.err != nil {
			t.Fatalf("client read error: %v", res.err)
		}
		if res.resp.Status != transport.StatusOk {
			t.Fatalf("expected StatusOk, got: %s (%s)", res.resp.Status, res.resp.Message)
		}
		if !bytes.Equal(res.resp.Value, []byte("v2_committed")) {
			t.Fatalf("stale data read! expected 'v2_committed', got: '%s'", string(res.resp.Value))
		}
		if res.resp.SeqID != 202 {
			t.Fatalf("expected SeqID 202, got %d", res.resp.SeqID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for GET to complete after releasing apply")
	}

	// Verify lastApplied has now reached or exceeded lastIdx
	if node1.node.LastApplied() < lastIdx {
		t.Fatalf("expected lastApplied >= %d, got %d", lastIdx, node1.node.LastApplied())
	}
}

// -----------------------------------------------------------------------------
// Test 2: Real 3-Node TCP Cluster — DELETE Followed by Linearizable GET (Section 10)
// -----------------------------------------------------------------------------
func TestReadBarrier_3Node_RealTCP_DeleteThenLinearizableGet(t *testing.T) {
	c, cleanup := createBarrierTCPCluster(t, 3, nil)
	defer cleanup()

	waitForBarrierClusterConnections(t, c, 3)

	node1 := c.nodes[1]
	node2 := c.nodes[2]
	node3 := c.nodes[3]

	if err := node1.node.BecomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := node1.node.BecomeLeader(); err != nil {
		t.Fatal(err)
	}
	syncFollowersFromLeader(t, node1, node2, node3)

	// Put initial value
	ctx := context.Background()
	putReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  1,
		Key:    []byte("key_to_delete"),
		Value:  []byte("alive"),
	}
	_, _ = node1.router.RouteWrite(ctx, putReq)
	syncFollowersFromLeader(t, node1, node2, node3)
	lastIdx, _ := node1.storage.LastIndex()
	_ = node1.node.HandleAppendEntriesResponse(2, &transport.AppendEntriesResponse{Term: 1, Success: true, MatchIndex: uint64(lastIdx)})

	// Wait for put to apply
	for i := 0; i < 50; i++ {
		if node1.node.LastApplied() >= lastIdx {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Propose DELETE with pause on key
	node1.sm.SetPauseKey("key_to_delete")
	delReq := &transport.Request{
		OpCode: transport.OpDelete,
		SeqID:  2,
		Key:    []byte("key_to_delete"),
	}
	_, _ = node1.router.RouteWrite(ctx, delReq)
	syncFollowersFromLeader(t, node1, node2, node3)
	delIdx, _ := node1.storage.LastIndex()
	_ = node1.node.HandleAppendEntriesResponse(2, &transport.AppendEntriesResponse{Term: 1, Success: true, MatchIndex: uint64(delIdx)})

	<-node1.sm.applyEntered

	// Send client GET for key_to_delete over TCP
	clientConn, err := net.Dial("tcp", node1.server.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	getReq := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  303,
		Key:    []byte("key_to_delete"),
	}
	respCh := make(chan *transport.Response, 1)
	go func() {
		_ = transport.WriteRequest(clientConn, getReq)
		resp, _ := transport.ReadResponse(clientConn)
		respCh <- resp
	}()

	// Verify GET is blocked
	select {
	case <-respCh:
		t.Fatal("GET returned before delete was applied")
	case <-time.After(100 * time.Millisecond):
	}

	// Release delete
	node1.sm.applyRelease <- struct{}{}

	// GET must return StatusKeyNotFound
	select {
	case resp := <-respCh:
		if resp.Status != transport.StatusKeyNotFound {
			t.Fatalf("expected StatusKeyNotFound after delete, got: %s", resp.Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for GET after delete")
	}
}

// -----------------------------------------------------------------------------
// Test 3: Real 3-Node TCP Cluster — Partitioned Leader Fails Closed (Section 12)
// -----------------------------------------------------------------------------
func TestReadBarrier_3Node_RealTCP_PartitionedLeaderFailsClosed(t *testing.T) {
	var partitioned atomic.Bool

	c, cleanup := createBarrierTCPCluster(t, 3, func(id cluster.NodeID, inner raft.PeerSender) raft.PeerSender {
		if id == 1 {
			return &partitionableSender{PeerSender: inner, partitioned: &partitioned}
		}
		return inner
	})
	defer cleanup()

	waitForBarrierClusterConnections(t, c, 3)

	node1 := c.nodes[1]
	node2 := c.nodes[2]
	node3 := c.nodes[3]

	if err := node1.node.BecomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := node1.node.BecomeLeader(); err != nil {
		t.Fatal(err)
	}
	syncFollowersFromLeader(t, node1, node2, node3)

	// Pre-populate Node 1's engine directly with data so we prove it does NOT serve it
	_ = node1.sm.Put(context.Background(), []byte("secret_key"), []byte("stale_val"))

	// Partition Leader 1 from Followers 2 and 3
	partitioned.Store(true)

	// Send GET request to partitioned leader with timeout
	clientConn, err := net.Dial("tcp", node1.server.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	getReq := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  404,
		Key:    []byte("secret_key"),
	}

	// In partition, ReadIndex cannot reach majority over TCP, so it fails.
	// Server responds with StatusThrottled or StatusNotLeader.
	_ = transport.WriteRequest(clientConn, getReq)

	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp, err := transport.ReadResponse(clientConn)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	// Must NOT return StatusOk with stale data!
	if resp.Status == transport.StatusOk {
		t.Fatalf("CRITICAL: Partitioned leader served stale local data! Value: %s", string(resp.Value))
	}
	if resp.Status != transport.StatusThrottled && resp.Status != transport.StatusNotLeader {
		t.Fatalf("expected StatusThrottled or StatusNotLeader, got: %s (%s)", resp.Status, resp.Message)
	}
}

// -----------------------------------------------------------------------------
// Test 4: Real 3-Node TCP Cluster — Leader Steps Down During Barrier Wait (Section 6 & 12)
// -----------------------------------------------------------------------------
func TestReadBarrier_3Node_RealTCP_LeaderStepsDownDuringWait(t *testing.T) {
	c, cleanup := createBarrierTCPCluster(t, 3, nil)
	defer cleanup()

	waitForBarrierClusterConnections(t, c, 3)

	node1 := c.nodes[1]
	node2 := c.nodes[2]
	node3 := c.nodes[3]

	if err := node1.node.BecomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := node1.node.BecomeLeader(); err != nil {
		t.Fatal(err)
	}
	syncFollowersFromLeader(t, node1, node2, node3)

	// Pre-set key in engine
	_ = node1.sm.Put(context.Background(), []byte("key_stepdown"), []byte("stale_data"))

	// Configure pause on key
	node1.sm.SetPauseKey("key_stepdown")

	// Propose a write
	ctx := context.Background()
	putReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  1,
		Key:    []byte("key_stepdown"),
		Value:  []byte("new_data"),
	}
	_, _ = node1.router.RouteWrite(ctx, putReq)
	syncFollowersFromLeader(t, node1, node2, node3)
	lastIdx, _ := node1.storage.LastIndex()
	_ = node1.node.HandleAppendEntriesResponse(2, &transport.AppendEntriesResponse{Term: 1, Success: true, MatchIndex: uint64(lastIdx)})

	<-node1.sm.applyEntered

	// Client sends GET over TCP
	clientConn, err := net.Dial("tcp", node1.server.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	getReq := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  505,
		Key:    []byte("key_stepdown"),
	}
	respCh := make(chan *transport.Response, 1)
	go func() {
		_ = transport.WriteRequest(clientConn, getReq)
		resp, _ := transport.ReadResponse(clientConn)
		respCh <- resp
	}()

	// Ensure GET is blocked in WaitForApplied
	time.Sleep(100 * time.Millisecond)

	// Node 2 becomes leader in Term 2 and sends AppendEntries to Node 1
	_ = node1.storage.SetTerm(2)
	_, _ = node1.node.HandleAppendEntries(2, &transport.AppendEntriesRequest{
		Term:     2,
		LeaderID: 2,
	})

	// Now unpause apply loop so WaitForApplied finishes
	node1.sm.applyRelease <- struct{}{}

	// ValidateLeadership must fail! Client must receive StatusNotLeader with redirect to Node 2!
	select {
	case resp := <-respCh:
		if resp.Status != transport.StatusNotLeader {
			t.Fatalf("expected StatusNotLeader after stepdown, got: %s (%s)", resp.Status, resp.Message)
		}
		if resp.LeaderID != 2 {
			t.Fatalf("expected LeaderID 2, got %d", resp.LeaderID)
		}
		if resp.LeaderAddr != node2.addr {
			t.Fatalf("expected LeaderAddr %s, got %s", node2.addr, resp.LeaderAddr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for GET after stepdown")
	}
}

// -----------------------------------------------------------------------------
// Test 5: Real 3-Node TCP Cluster — Concurrent Linearizable Readers (Section 10)
// -----------------------------------------------------------------------------
func TestReadBarrier_3Node_RealTCP_ConcurrentReaders(t *testing.T) {
	c, cleanup := createBarrierTCPCluster(t, 3, nil)
	defer cleanup()

	waitForBarrierClusterConnections(t, c, 3)

	node1 := c.nodes[1]
	node2 := c.nodes[2]
	node3 := c.nodes[3]

	if err := node1.node.BecomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := node1.node.BecomeLeader(); err != nil {
		t.Fatal(err)
	}
	syncFollowersFromLeader(t, node1, node2, node3)

	// Commit key "concurrent_key" = "stable_val"
	ctx := context.Background()
	putReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  1,
		Key:    []byte("concurrent_key"),
		Value:  []byte("stable_val"),
	}
	_, _ = node1.router.RouteWrite(ctx, putReq)
	syncFollowersFromLeader(t, node1, node2, node3)
	lastIdx, _ := node1.storage.LastIndex()
	_ = node1.node.HandleAppendEntriesResponse(2, &transport.AppendEntriesResponse{Term: 1, Success: true, MatchIndex: uint64(lastIdx)})

	for i := 0; i < 50; i++ {
		if node1.node.LastApplied() >= lastIdx {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	const readerCount = 10
	var wg sync.WaitGroup
	errCh := make(chan error, readerCount)

	for i := 0; i < readerCount; i++ {
		wg.Add(1)
		go func(seq uint64) {
			defer wg.Done()
			conn, err := net.Dial("tcp", node1.server.Addr().String())
			if err != nil {
				errCh <- fmt.Errorf("dial failed: %w", err)
				return
			}
			defer conn.Close()

			req := &transport.Request{
				OpCode: transport.OpGet,
				SeqID:  seq,
				Key:    []byte("concurrent_key"),
			}
			if err := transport.WriteRequest(conn, req); err != nil {
				errCh <- fmt.Errorf("write failed: %w", err)
				return
			}
			resp, err := transport.ReadResponse(conn)
			if err != nil {
				errCh <- fmt.Errorf("read failed: %w", err)
				return
			}
			if resp.Status != transport.StatusOk {
				errCh <- fmt.Errorf("bad status: %s", resp.Status)
				return
			}
			if !bytes.Equal(resp.Value, []byte("stable_val")) {
				errCh <- fmt.Errorf("bad value: %s", string(resp.Value))
				return
			}
		}(uint64(1000 + i))
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}
}
