package raft_test

import (
	"context"
	stdErrors "errors"
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

// partitionFilter manages bidirectional network traffic cuts between cluster nodes.
type partitionFilter struct {
	mu            sync.RWMutex
	cuts          map[[2]cluster.NodeID]bool
	droppedCount  atomic.Uint64
	droppedByEdge sync.Map // [2]cluster.NodeID -> *atomic.Uint64
	onCut         func(a, b cluster.NodeID)
}

func newPartitionFilter() *partitionFilter {
	return &partitionFilter{
		cuts: make(map[[2]cluster.NodeID]bool),
	}
}

// Cut isolates node A and node B bidirectionally.
func (f *partitionFilter) Cut(a, b cluster.NodeID) {
	f.mu.Lock()
	f.cuts[[2]cluster.NodeID{a, b}] = true
	f.cuts[[2]cluster.NodeID{b, a}] = true
	cb := f.onCut
	f.mu.Unlock()
	if cb != nil {
		cb(a, b)
	}
}

// CutDirectional cuts traffic strictly in the from -> to direction.
func (f *partitionFilter) CutDirectional(from, to cluster.NodeID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cuts[[2]cluster.NodeID{from, to}] = true
}

// Heal restores bidirectional connectivity between node A and node B.
func (f *partitionFilter) Heal(a, b cluster.NodeID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.cuts, [2]cluster.NodeID{a, b})
	delete(f.cuts, [2]cluster.NodeID{b, a})
}

// HealAll clears all network partitions across the entire cluster.
func (f *partitionFilter) HealAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cuts = make(map[[2]cluster.NodeID]bool)
}

// IsCut reports whether traffic from -> to is currently partitioned.
func (f *partitionFilter) IsCut(from, to cluster.NodeID) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.cuts[[2]cluster.NodeID{from, to}]
}

// RecordDrop registers a dropped packet on edge from -> to.
func (f *partitionFilter) RecordDrop(from, to cluster.NodeID) {
	f.droppedCount.Add(1)
	edge := [2]cluster.NodeID{from, to}
	val, _ := f.droppedByEdge.LoadOrStore(edge, &atomic.Uint64{})
	val.(*atomic.Uint64).Add(1)
}

// DroppedCount returns the total number of dropped messages across all edges.
func (f *partitionFilter) DroppedCount() uint64 {
	return f.droppedCount.Load()
}

// EdgeDroppedCount returns the number of dropped messages from -> to.
func (f *partitionFilter) EdgeDroppedCount(from, to cluster.NodeID) uint64 {
	val, ok := f.droppedByEdge.Load([2]cluster.NodeID{from, to})
	if !ok {
		return 0
	}
	return val.(*atomic.Uint64).Load()
}

// filteredPeerSender intercepts outbound peer frames and enforces the partition filter.
type filteredPeerSender struct {
	inner  raft.PeerSender
	from   cluster.NodeID
	filter *partitionFilter
}

func (s *filteredPeerSender) NextSeqID() uint64 {
	return s.inner.NextSeqID()
}

func (s *filteredPeerSender) Send(ctx context.Context, peerID cluster.NodeID, frame *transport.Frame) error {
	if s.filter.IsCut(s.from, peerID) {
		s.filter.RecordDrop(s.from, peerID)
		return &errors.PeerUnavailableError{
			NodeID: uint64(peerID),
			State:  "Partitioned",
		}
	}
	return s.inner.Send(ctx, peerID, frame)
}

// trackedConn is a net.Conn wrapper that executes a callback upon Close (Finding L).
type trackedConn struct {
	net.Conn
	onClose func()
	once    sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() {
		if c.onClose != nil {
			c.onClose()
		}
	})
	return c.Conn.Close()
}

// partitionNetwork coordinates connection-level fault injection across peer TCP transports (Finding L).
type partitionNetwork struct {
	mu           sync.Mutex
	filter       *partitionFilter
	addrToNode   map[string]cluster.NodeID
	activeConns  map[[2]cluster.NodeID]map[net.Conn]struct{}
	dialAttempts atomic.Uint64
	dialsCut     atomic.Uint64
}

func newPartitionNetwork(filter *partitionFilter) *partitionNetwork {
	pn := &partitionNetwork{
		filter:      filter,
		addrToNode:  make(map[string]cluster.NodeID),
		activeConns: make(map[[2]cluster.NodeID]map[net.Conn]struct{}),
	}
	filter.onCut = pn.SeverEdge
	return pn
}

func (pn *partitionNetwork) registerAddr(id cluster.NodeID, addr string) {
	pn.mu.Lock()
	defer pn.mu.Unlock()
	pn.addrToNode[addr] = id
}

func (pn *partitionNetwork) dialFunc(localID cluster.NodeID) transport.DialFunc {
	return func(ctx context.Context, addr string) (net.Conn, error) {
		pn.dialAttempts.Add(1)

		pn.mu.Lock()
		targetID, ok := pn.addrToNode[addr]
		pn.mu.Unlock()

		if ok && pn.filter.IsCut(localID, targetID) {
			pn.dialsCut.Add(1)
			pn.filter.RecordDrop(localID, targetID)
			return nil, &errors.PeerUnavailableError{
				NodeID: uint64(targetID),
				State:  "ConnectionRefused (Partitioned)",
			}
		}

		dialer := net.Dialer{Timeout: 2 * time.Second}
		rawConn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}

		if ok && pn.filter.IsCut(localID, targetID) {
			_ = rawConn.Close()
			pn.dialsCut.Add(1)
			pn.filter.RecordDrop(localID, targetID)
			return nil, &errors.PeerUnavailableError{
				NodeID: uint64(targetID),
				State:  "ConnectionRefused (Partitioned)",
			}
		}

		if ok {
			pn.registerConn(localID, targetID, rawConn)
		}

		return &trackedConn{
			Conn: rawConn,
			onClose: func() {
				if ok {
					pn.unregisterConn(localID, targetID, rawConn)
				}
			},
		}, nil
	}
}

func (pn *partitionNetwork) registerConn(from, to cluster.NodeID, c net.Conn) {
	pn.mu.Lock()
	defer pn.mu.Unlock()
	edge := [2]cluster.NodeID{from, to}
	if pn.activeConns[edge] == nil {
		pn.activeConns[edge] = make(map[net.Conn]struct{})
	}
	pn.activeConns[edge][c] = struct{}{}
}

func (pn *partitionNetwork) unregisterConn(from, to cluster.NodeID, c net.Conn) {
	pn.mu.Lock()
	defer pn.mu.Unlock()
	edge := [2]cluster.NodeID{from, to}
	if m := pn.activeConns[edge]; m != nil {
		delete(m, c)
	}
}

func (pn *partitionNetwork) SeverEdge(a, b cluster.NodeID) {
	pn.mu.Lock()
	defer pn.mu.Unlock()

	edges := [][2]cluster.NodeID{
		{a, b},
		{b, a},
	}
	for _, edge := range edges {
		if conns, exists := pn.activeConns[edge]; exists {
			for c := range conns {
				_ = c.Close()
			}
			delete(pn.activeConns, edge)
		}
	}
}

// partitionClusterNode encapsulates a running node in the partition test cluster.
type partitionClusterNode struct {
	id      cluster.NodeID
	addr    string
	ln      net.Listener
	mgr     *transport.PeerConnectionManager
	sender  *filteredPeerSender
	storage *raft.Storage
	node    *raft.Node
	sm      *controllableEngineSM
	router  *raft.ProposalRouter
	server  *transport.Server
}

// partitionCluster represents a multi-node cluster with full peer and client TCP networking.
type partitionCluster struct {
	nodes   map[cluster.NodeID]*partitionClusterNode
	filter  *partitionFilter
	network *partitionNetwork
}

// clusterOptions configures election timing and transition notifications for the test cluster.
type clusterOptions struct {
	durationProviders map[cluster.NodeID]raft.DurationProvider
	transitionHooks   map[cluster.NodeID]raft.TransitionHook
	heartbeatInterval time.Duration
}

// createPartitionCluster instantiates an N-node real TCP cluster wired with a partitionFilter.
func createPartitionCluster(t *testing.T, count int, opts clusterOptions) (*partitionCluster, func()) {
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

	filter := newPartitionFilter()
	pn := newPartitionNetwork(filter)
	for i := 1; i <= count; i++ {
		id := cluster.NodeID(i)
		pn.registerAddr(id, peerAddrs[id])
	}

	clusterObj := &partitionCluster{
		nodes:   make(map[cluster.NodeID]*partitionClusterNode, count),
		filter:  filter,
		network: pn,
	}

	hbInterval := opts.heartbeatInterval
	if hbInterval <= 0 {
		hbInterval = 15 * time.Millisecond
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

		nodeEntry := &partitionClusterNode{
			id:      id,
			addr:    peerAddrs[id],
			ln:      peerListeners[id],
			storage: st,
			sm:      sm,
		}

		peerCfg := transport.DefaultPeerConnectionConfig()
		peerCfg.InsecureTransport = true
		peerCfg.DialTimeout = 2 * time.Second
		peerCfg.ReconnectMin = 10 * time.Millisecond
		peerCfg.ReconnectMax = 50 * time.Millisecond
		peerCfg.DialFunc = pn.dialFunc(id)

		// Inbound filter check
		peerCfg.OnFrameReceived = func(peerID cluster.NodeID, frame *transport.Frame) {
			if filter.IsCut(peerID, id) {
				filter.RecordDrop(peerID, id)
				return
			}
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

		// Outbound filter check
		peerSender := &filteredPeerSender{
			inner:  mgr,
			from:   id,
			filter: filter,
		}
		nodeEntry.sender = peerSender

		raftCfg := raft.NodeConfig{
			LocalID:           id,
			Storage:           st,
			Topology:          topo,
			StateMachine:      sm,
			PeerSender:        peerSender,
			HeartbeatInterval: hbInterval,
		}
		if opts.durationProviders != nil && opts.durationProviders[id] != nil {
			raftCfg.DurationProvider = opts.durationProviders[id]
		}
		if opts.transitionHooks != nil && opts.transitionHooks[id] != nil {
			raftCfg.TransitionHook = opts.transitionHooks[id]
		}

		rNode, err := raft.NewNode(raftCfg)
		if err != nil {
			t.Fatalf("failed to create raft.Node for node %d: %v", id, err)
		}
		nodeEntry.node = rNode

		router := raft.NewProposalRouter(rNode, topo, sm)
		nodeEntry.router = router

		// Client-facing transport.Server
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
					n.node.StopElectionTimer()
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

// waitForClusterPeerMesh waits until all nodes report connected states to all remote peers.
func waitForClusterPeerMesh(t *testing.T, c *partitionCluster, count int) {
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
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for cluster peer connections to establish")
}

// sendClientWrite performs an end-to-end client TCP request to a node's transport.Server.
func sendClientWrite(t *testing.T, addr string, opCode transport.OpCode, key, value []byte, seqID uint64) (*transport.Response, error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	req := &transport.Request{
		OpCode: opCode,
		SeqID:  seqID,
		Key:    key,
		Value:  value,
	}
	if err := transport.WriteRequest(conn, req); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	return transport.ReadResponse(conn)
}

// sendClientRead performs an end-to-end client TCP GET request.
func sendClientRead(t *testing.T, addr string, key []byte, seqID uint64) (*transport.Response, error) {
	t.Helper()
	return sendClientWrite(t, addr, transport.OpGet, key, nil, seqID)
}

// writeAuditRecord documents the full consensus and client state for each test operation.
type writeAuditRecord struct {
	OpID          string
	ClientStatus  transport.StatusCode
	ClientMsg     string
	OriginNode    cluster.NodeID
	LeaderTerm    raft.Term
	LeaderEpoch   uint64
	LogIndex      raft.LogIndex
	LocalAppended bool
	Replicated    bool
	CommitIndex   raft.LogIndex
	LastApplied   raft.LogIndex
	EngineValue   string
}

// waitForCondition polls fn until it returns true or timeout expires.
func waitForCondition(timeout time.Duration, fn func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// -----------------------------------------------------------------------------
// Test 1: Full Jepsen-Style Network Partition Simulation (End-to-End Safety)
// -----------------------------------------------------------------------------
// Exercises the complete failure/recovery lifecycle:
//  1. 3-node cluster established; Node 1 elected leader in Term 1.
//  2. Baseline write committed across quorum.
//  3. Bidirectional network partition isolates Node 1 from {Node 2, Node 3}.
//  4. Majority {Node 2, Node 3} autonomously elects Node 2 in Term 2.
//  5. Isolated leader (Node 1) attempts split-brain write ("split_key", "bad_val"):
//     - Accepted into local WAL (Index 3, Term 1).
//     - commitIndex DOES NOT advance (stays at baseline).
//     - lastApplied DOES NOT advance (stays at baseline).
//     - State machine DOES NOT apply "split_key".
//  6. Majority leader (Node 2) commits majority write ("maj_key", "good_val"):
//     - Replicated to Node 3.
//     - commitIndex advances on Node 2 & Node 3.
//     - Both state machines apply "maj_key".
//  7. Partition heals:
//     - Node 1 receives Term 2 heartbeat, steps down to RoleFollower.
//     - Log reconciliation replaces conflicting Term 1 entry with Term 2 history.
//     - All 3 nodes converge on committed log and state machine state.
//     - ZERO split-brain writes committed!
func TestPartition_3Node_IsolatedLeader_ZeroSplitBrainCommit(t *testing.T) {
	node2LeaderCh := make(chan raft.Term, 1)
	node1StepDownCh := make(chan raft.Term, 1)

	opts := clusterOptions{
		heartbeatInterval: 15 * time.Millisecond,
		durationProviders: map[cluster.NodeID]raft.DurationProvider{
			1: func() time.Duration { return 150 * time.Millisecond },
			2: func() time.Duration { return 35 * time.Millisecond }, // Node 2 times out first
			3: func() time.Duration { return 70 * time.Millisecond },
		},
		transitionHooks: map[cluster.NodeID]raft.TransitionHook{
			1: func(oldRole, newRole raft.Role, term raft.Term) {
				if oldRole == raft.RoleLeader && newRole == raft.RoleFollower {
					select {
					case node1StepDownCh <- term:
					default:
					}
				}
			},
			2: func(oldRole, newRole raft.Role, term raft.Term) {
				if newRole == raft.RoleLeader {
					select {
					case node2LeaderCh <- term:
					default:
					}
				}
			},
		},
	}

	c, cleanup := createPartitionCluster(t, 3, opts)
	defer cleanup()

	waitForClusterPeerMesh(t, c, 3)

	node1 := c.nodes[1]
	node2 := c.nodes[2]
	node3 := c.nodes[3]

	// Phase 1: Establish Node 1 as initial leader
	if err := node1.node.BecomeCandidate(); err != nil {
		t.Fatalf("failed to promote node 1 to candidate: %v", err)
	}
	if err := node1.node.BecomeLeader(); err != nil {
		t.Fatalf("failed to promote node 1 to leader: %v", err)
	}

	// Start election timers on followers (they won't fire while Node 1 heartbeats at 15ms)
	if err := node2.node.StartElectionTimer(); err != nil {
		t.Fatalf("failed to start election timer on node 2: %v", err)
	}
	if err := node3.node.StartElectionTimer(); err != nil {
		t.Fatalf("failed to start election timer on node 3: %v", err)
	}

	// Phase 2: Issue baseline write through Node 1 over client TCP socket
	resp0, err := sendClientWrite(t, node1.server.Addr().String(), transport.OpPut, []byte("k0"), []byte("v0"), 100)
	if err != nil {
		t.Fatalf("baseline client write failed: %v", err)
	}
	if resp0.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk for baseline write, got: %s (%s)", resp0.Status, resp0.Message)
	}

	// Wait for baseline write to replicate and commit on all nodes
	baselineCommitted := waitForCondition(3*time.Second, func() bool {
		return node1.node.CommitIndex() >= 2 &&
			node2.node.CommitIndex() >= 2 &&
			node3.node.CommitIndex() >= 2 &&
			node1.node.LastApplied() >= 2 &&
			node2.node.LastApplied() >= 2 &&
			node3.node.LastApplied() >= 2
	})
	if !baselineCommitted {
		t.Fatalf("baseline write failed to commit across cluster: node1=(c:%d, a:%d), node2=(c:%d, a:%d), node3=(c:%d, a:%d)",
			node1.node.CommitIndex(), node1.node.LastApplied(),
			node2.node.CommitIndex(), node2.node.LastApplied(),
			node3.node.CommitIndex(), node3.node.LastApplied())
	}

	baselineCommitIdx := node1.node.CommitIndex()
	baselineAppliedIdx := node1.node.LastApplied()

	// Verify all 3 state machines have the baseline key
	for _, n := range []*partitionClusterNode{node1, node2, node3} {
		val, err := n.sm.Get([]byte("k0"))
		if err != nil || string(val) != "v0" {
			t.Fatalf("node %d missing baseline key: val=%s, err=%v", n.id, string(val), err)
		}
	}

	// Record Baseline Audit
	baselineAudit := writeAuditRecord{
		OpID:          "OP-BASELINE",
		ClientStatus:  resp0.Status,
		OriginNode:    1,
		LeaderTerm:    1,
		CommitIndex:   baselineCommitIdx,
		LastApplied:   baselineAppliedIdx,
		LocalAppended: true,
		Replicated:    true,
		EngineValue:   "v0",
	}
	t.Logf("Audit: Baseline write committed: %+v", baselineAudit)

	// Phase 3: INJECT BIDIRECTIONAL NETWORK PARTITION (Connection-Level Fault Injection - Finding L)
	// Node 1 isolated from {Node 2, Node 3}
	c.filter.Cut(1, 2)
	c.filter.Cut(1, 3)

	t.Log("Partition active: Node 1 isolated from Node 2 and Node 3 (TCP connections severed)")

	// Assert connection-level severance on isolated node (Finding M)
	disconnected := waitForCondition(2*time.Second, func() bool {
		return !node1.mgr.IsConnected(2) && !node1.mgr.IsConnected(3)
	})
	if !disconnected {
		t.Fatal("expected Node 1 peer connections to 2 and 3 to be severed at TCP connection level")
	}

	// Assert majority peer connection remains connected (Finding M)
	if !node2.mgr.IsConnected(3) || !node3.mgr.IsConnected(2) {
		t.Fatal("majority nodes 2 and 3 erroneously disconnected")
	}

	// Phase 4: Await autonomous election of Node 2 by surviving majority {Node 2, Node 3}
	var newTerm raft.Term
	select {
	case newTerm = <-node2LeaderCh:
		t.Logf("Node 2 successfully elected leader in Term %d by surviving majority", newTerm)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for surviving majority to elect Node 2 as new leader")
	}

	if newTerm <= 1 {
		t.Fatalf("expected new leader term > 1, got %d", newTerm)
	}
	if node2.node.Role() != raft.RoleLeader {
		t.Fatalf("expected node 2 role RoleLeader, got %s", node2.node.Role())
	}

	// Phase 5: Split-Brain Proposal issued to ISOLATED OLD LEADER (Node 1)
	respSplit, err := sendClientWrite(t, node1.server.Addr().String(), transport.OpPut, []byte("split_key"), []byte("bad_value"), 101)
	if err != nil {
		t.Fatalf("unexpected error communicating with isolated leader: %v", err)
	}

	// Under Phase 16 write semantics, Node 1 appends locally to its WAL and returns StatusOk
	// because it still believes it is leader (has not yet seen higher term).
	// BUT IT MUST NOT ADVANCE commitIndex or lastApplied!
	lastIdx1, err := node1.storage.LastIndex()
	if err != nil {
		t.Fatalf("failed to read last index on node 1: %v", err)
	}
	splitAppendedTerm, err := node1.storage.TermOf(lastIdx1)
	if err != nil {
		t.Fatalf("failed to read last index term on node 1: %v", err)
	}

	splitAudit := writeAuditRecord{
		OpID:          "OP-SPLIT-BRAIN",
		ClientStatus:  respSplit.Status,
		OriginNode:    1,
		LeaderTerm:    splitAppendedTerm,
		LogIndex:      lastIdx1,
		LocalAppended: true,
		Replicated:    false,
		CommitIndex:   node1.node.CommitIndex(),
		LastApplied:   node1.node.LastApplied(),
	}
	t.Logf("Audit: Split-brain proposal to isolated leader: %+v", splitAudit)

	// CRITICAL SAFETY ASSERTIONS on Isolated Leader:
	// 1. commitIndex MUST NOT advance past baseline!
	if node1.node.CommitIndex() > baselineCommitIdx {
		t.Fatalf("CRITICAL SAFETY VIOLATION: Isolated leader advanced commitIndex from %d to %d without majority quorum!",
			baselineCommitIdx, node1.node.CommitIndex())
	}
	// 2. lastApplied MUST NOT advance past baseline!
	if node1.node.LastApplied() > baselineAppliedIdx {
		t.Fatalf("CRITICAL SAFETY VIOLATION: Isolated leader applied uncommitted entry: lastApplied advanced to %d!",
			node1.node.LastApplied())
	}
	// 3. State machine MUST NOT contain split_key!
	if _, err := node1.sm.Get([]byte("split_key")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("CRITICAL SAFETY VIOLATION: Isolated leader applied uncommitted 'split_key' to state machine!")
	}
	// 4. Traffic was actually dropped by partition filter
	if c.filter.EdgeDroppedCount(1, 2) == 0 && c.filter.EdgeDroppedCount(1, 3) == 0 {
		t.Fatal("expected dropped outbound frames from isolated leader, but drop count was zero")
	}

	// Phase 6: Valid Majority Write through New Leader (Node 2)
	respMaj, err := sendClientWrite(t, node2.server.Addr().String(), transport.OpPut, []byte("maj_key"), []byte("good_value"), 102)
	if err != nil {
		t.Fatalf("majority leader client write failed: %v", err)
	}
	if respMaj.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk on majority leader, got: %s (%s)", respMaj.Status, respMaj.Message)
	}

	// Wait for majority write to commit and apply on surviving majority (Nodes 2 & 3)
	majCommitted := waitForCondition(3*time.Second, func() bool {
		return node2.node.CommitIndex() > baselineCommitIdx &&
			node3.node.CommitIndex() > baselineCommitIdx &&
			node2.node.LastApplied() > baselineAppliedIdx &&
			node3.node.LastApplied() > baselineAppliedIdx
	})
	if !majCommitted {
		t.Fatalf("majority write failed to commit on Nodes 2 and 3: node2=(c:%d, a:%d), node3=(c:%d, a:%d)",
			node2.node.CommitIndex(), node2.node.LastApplied(),
			node3.node.CommitIndex(), node3.node.LastApplied())
	}

	majAudit := writeAuditRecord{
		OpID:          "OP-MAJORITY-WRITE",
		ClientStatus:  respMaj.Status,
		OriginNode:    2,
		LeaderTerm:    newTerm,
		CommitIndex:   node2.node.CommitIndex(),
		LastApplied:   node2.node.LastApplied(),
		LocalAppended: true,
		Replicated:    true,
		EngineValue:   "good_value",
	}
	t.Logf("Audit: Majority write committed on Nodes 2 & 3: %+v", majAudit)

	// Verify Nodes 2 and 3 state machines have "maj_key" and NEVER saw "split_key"
	for _, n := range []*partitionClusterNode{node2, node3} {
		val, err := n.sm.Get([]byte("maj_key"))
		if err != nil || string(val) != "good_value" {
			t.Fatalf("node %d missing majority key: val=%s, err=%v", n.id, string(val), err)
		}
		if _, err := n.sm.Get([]byte("split_key")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
			t.Fatalf("node %d contaminated with uncommitted 'split_key'!", n.id)
		}
	}

	// Re-verify Node 1 remained completely uncommitted
	if node1.node.CommitIndex() != baselineCommitIdx || node1.node.LastApplied() != baselineAppliedIdx {
		t.Fatalf("CRITICAL: Isolated leader mutated commit/apply state during majority write!")
	}

	// Phase 7: HEAL THE PARTITION (Restore full connectivity)
	t.Log("Healing network partition...")
	c.filter.HealAll()

	// Wait for peer connections to re-establish across all edges (Finding M)
	waitForClusterPeerMesh(t, c, 3)
	t.Log("Cluster peer mesh fully re-established at TCP connection level")

	// Wait for Node 1 to observe higher term and step down to RoleFollower
	select {
	case stepDownTerm := <-node1StepDownCh:
		t.Logf("Node 1 successfully stepped down to RoleFollower upon observing Term %d", stepDownTerm)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Node 1 to step down after partition heal")
	}

	if node1.node.Role() != raft.RoleFollower {
		t.Fatalf("expected node 1 role RoleFollower, got %s", node1.node.Role())
	}

	// Phase 8: Wait for Log Reconciliation & Convergence Across All 3 Nodes
	converged := waitForCondition(5*time.Second, func() bool {
		targetCommit := node2.node.CommitIndex()
		return node1.node.CommitIndex() == targetCommit &&
			node3.node.CommitIndex() == targetCommit &&
			node1.node.LastApplied() == targetCommit &&
			node2.node.LastApplied() == targetCommit &&
			node3.node.LastApplied() == targetCommit
	})
	if !converged {
		t.Fatalf("cluster failed to converge after heal: node1=(c:%d, a:%d), node2=(c:%d, a:%d), node3=(c:%d, a:%d)",
			node1.node.CommitIndex(), node1.node.LastApplied(),
			node2.node.CommitIndex(), node2.node.LastApplied(),
			node3.node.CommitIndex(), node3.node.LastApplied())
	}

	// Phase 9: FINAL SYSTEM-WIDE SAFETY ASSERTIONS
	// 1. All 3 nodes have exact same CommitIndex and LastApplied
	finalCommitIdx := node2.node.CommitIndex()
	if node1.node.CommitIndex() != finalCommitIdx || node3.node.CommitIndex() != finalCommitIdx {
		t.Fatalf("commit index mismatch: n1=%d, n2=%d, n3=%d",
			node1.node.CommitIndex(), node2.node.CommitIndex(), node3.node.CommitIndex())
	}
	if node1.node.LastApplied() != finalCommitIdx || node3.node.LastApplied() != finalCommitIdx {
		t.Fatalf("last applied mismatch: n1=%d, n2=%d, n3=%d",
			node1.node.LastApplied(), node2.node.LastApplied(), node3.node.LastApplied())
	}

	// 2. All 3 nodes have exact same log entries (prefix consistency)
	for idx := raft.LogIndex(1); idx <= finalCommitIdx; idx++ {
		e1, err1 := node1.storage.Entry(idx)
		e2, err2 := node2.storage.Entry(idx)
		e3, err3 := node3.storage.Entry(idx)
		if err1 != nil || err2 != nil || err3 != nil {
			t.Fatalf("failed to read entry %d: err1=%v, err2=%v, err3=%v", idx, err1, err2, err3)
		}
		if e1.Term != e2.Term || e2.Term != e3.Term {
			t.Fatalf("term divergence at log index %d: n1=%d, n2=%d, n3=%d", idx, e1.Term, e2.Term, e3.Term)
		}
		if string(e1.Data) != string(e2.Data) || string(e2.Data) != string(e3.Data) {
			t.Fatalf("payload divergence at log index %d", idx)
		}
	}

	// 3. In ALL 3 nodes' state machines:
	//    - "k0" == "v0"
	//    - "maj_key" == "good_value"
	//    - "split_key" DOES NOT EXIST (ZERO split-brain writes committed!)
	for _, n := range []*partitionClusterNode{node1, node2, node3} {
		v0, err := n.sm.Get([]byte("k0"))
		if err != nil || string(v0) != "v0" {
			t.Fatalf("node %d corrupted on 'k0': val=%s, err=%v", n.id, string(v0), err)
		}
		vMaj, err := n.sm.Get([]byte("maj_key"))
		if err != nil || string(vMaj) != "good_value" {
			t.Fatalf("node %d missing 'maj_key': val=%s, err=%v", n.id, string(vMaj), err)
		}
		if _, err := n.sm.Get([]byte("split_key")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
			t.Fatalf("CRITICAL STATE-MACHINE SAFETY VIOLATION: node %d contains 'split_key' in state machine after reconciliation!", n.id)
		}
	}

	t.Logf("ZERO SPLIT-BRAIN WRITES COMMITTED verified across %d drops! Total cluster convergence established.", c.filter.DroppedCount())
}

// -----------------------------------------------------------------------------
// Test 2: Bidirectional Traffic Blocked Verification (Test 5 in prompt)
// -----------------------------------------------------------------------------
// Verifies that:
//   - Traffic from isolated leader to followers is blocked.
//   - Traffic from followers to isolated leader is blocked.
//   - Traffic between surviving followers (majority side) continues uninterrupted.
func TestPartition_3Node_BidirectionalTrafficBlocked(t *testing.T) {
	c, cleanup := createPartitionCluster(t, 3, clusterOptions{})
	defer cleanup()

	waitForClusterPeerMesh(t, c, 3)

	node1 := c.nodes[1]
	node2 := c.nodes[2]
	node3 := c.nodes[3]

	// Establish Node 1 as leader
	if err := node1.node.BecomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := node1.node.BecomeLeader(); err != nil {
		t.Fatal(err)
	}

	// Isolate Node 1 bidirectionally from {Node 2, Node 3}
	c.filter.Cut(1, 2)
	c.filter.Cut(1, 3)

	// Outbound from 1 to 2 & 3 must be blocked
	req := &transport.AppendEntriesRequest{
		Term:         1,
		LeaderID:     1,
		PrevLogIndex: 0,
		PrevLogTerm:  0,
	}
	frame1, _ := transport.EncodeAppendEntries(req, 1)

	err1to2 := node1.sender.Send(context.Background(), 2, frame1)
	if err1to2 == nil {
		t.Fatal("expected Send 1->2 to fail while partitioned, but it succeeded")
	}
	err1to3 := node1.sender.Send(context.Background(), 3, frame1)
	if err1to3 == nil {
		t.Fatal("expected Send 1->3 to fail while partitioned, but it succeeded")
	}

	// Inbound from 2 to 1 and 3 to 1 must be blocked
	rvReq := &transport.RequestVoteRequest{
		Term:        2,
		CandidateID: 2,
	}
	frame2, _ := transport.EncodeRequestVote(rvReq, 2)
	err2to1 := node2.sender.Send(context.Background(), 1, frame2)
	if err2to1 == nil {
		t.Fatal("expected Send 2->1 to fail while partitioned, but it succeeded")
	}

	rvReq3 := &transport.RequestVoteRequest{
		Term:        2,
		CandidateID: 3,
	}
	frame3, _ := transport.EncodeRequestVote(rvReq3, 3)
	err3to1 := node3.sender.Send(context.Background(), 1, frame3)
	if err3to1 == nil {
		t.Fatal("expected Send 3->1 to fail while partitioned, but it succeeded")
	}

	// Verify drop counts
	if c.filter.EdgeDroppedCount(1, 2) == 0 || c.filter.EdgeDroppedCount(1, 3) == 0 ||
		c.filter.EdgeDroppedCount(2, 1) == 0 || c.filter.EdgeDroppedCount(3, 1) == 0 {
		t.Fatalf("expected non-zero drop counts on all severed edges: 1->2: %d, 1->3: %d, 2->1: %d, 3->1: %d",
			c.filter.EdgeDroppedCount(1, 2), c.filter.EdgeDroppedCount(1, 3),
			c.filter.EdgeDroppedCount(2, 1), c.filter.EdgeDroppedCount(3, 1))
	}

	// Verify majority-internal traffic (2 <-> 3) is 100% UNBLOCKED
	beforeMajDrops := c.filter.EdgeDroppedCount(2, 3) + c.filter.EdgeDroppedCount(3, 2)
	err2to3 := node2.sender.Send(context.Background(), 3, frame2)
	if err2to3 != nil {
		t.Fatalf("expected Send 2->3 to succeed between majority nodes, got: %v", err2to3)
	}
	err3to2 := node3.sender.Send(context.Background(), 2, frame3)
	if err3to2 != nil {
		t.Fatalf("expected Send 3->2 to succeed between majority nodes, got: %v", err3to2)
	}
	afterMajDrops := c.filter.EdgeDroppedCount(2, 3) + c.filter.EdgeDroppedCount(3, 2)
	if afterMajDrops != beforeMajDrops {
		t.Fatal("majority traffic between Node 2 and Node 3 was erroneously dropped")
	}

	t.Log("Bidirectional partition and majority-internal preservation verified.")
}

// -----------------------------------------------------------------------------
// Test 3: Higher-Term Fencing (Test 4 in prompt)
// -----------------------------------------------------------------------------
// Verifies that when an isolated leader observes a message with a higher term,
// it immediately increments leaderEpoch, steps down, halts heartbeats, and
// rejects subsequent client proposals with StatusNotLeader.
func TestPartition_3Node_HigherTermFencing(t *testing.T) {
	node1StepDownCh := make(chan struct{}, 1)

	opts := clusterOptions{
		transitionHooks: map[cluster.NodeID]raft.TransitionHook{
			1: func(oldRole, newRole raft.Role, term raft.Term) {
				if oldRole == raft.RoleLeader && newRole == raft.RoleFollower {
					select {
					case node1StepDownCh <- struct{}{}:
					default:
					}
				}
			},
		},
	}

	c, cleanup := createPartitionCluster(t, 3, opts)
	defer cleanup()

	waitForClusterPeerMesh(t, c, 3)

	node1 := c.nodes[1]

	if err := node1.node.BecomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := node1.node.BecomeLeader(); err != nil {
		t.Fatal(err)
	}

	epochBefore := node1.node.LeaderEpochForTest()
	c.filter.Cut(1, 2)
	c.filter.Cut(1, 3)

	// Simulate receiving higher-term AppendEntries from Node 2 (Term 5)
	higherTermReq := &transport.AppendEntriesRequest{
		Term:         5,
		LeaderID:     2,
		PrevLogIndex: 0,
		PrevLogTerm:  0,
	}
	resp, err := node1.node.HandleAppendEntries(2, higherTermReq)
	if err != nil {
		t.Fatalf("HandleAppendEntries failed: %v", err)
	}
	if resp.Term != 5 {
		t.Fatalf("expected response term 5, got %d", resp.Term)
	}

	// Assert immediate stepdown
	select {
	case <-node1StepDownCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for transition hook on stepdown")
	}

	if node1.node.Role() != raft.RoleFollower {
		t.Fatalf("expected RoleFollower, got %s", node1.node.Role())
	}
	if node1.node.LeaderEpochForTest() <= epochBefore {
		t.Fatalf("expected leaderEpoch to increment on higher-term stepdown: before=%d, after=%d",
			epochBefore, node1.node.LeaderEpochForTest())
	}

	// Subsequent client write must be rejected with StatusNotLeader
	clientResp, err := sendClientWrite(t, node1.server.Addr().String(), transport.OpPut, []byte("fenced_key"), []byte("val"), 200)
	if err != nil {
		t.Fatalf("client write failed: %v", err)
	}
	if clientResp.Status != transport.StatusNotLeader {
		t.Fatalf("expected StatusNotLeader for stepped-down leader, got %s (%s)", clientResp.Status, clientResp.Message)
	}

	t.Log("Higher-term fencing and epoch invalidation verified.")
}

// -----------------------------------------------------------------------------
// Test 4: Linearizable Read During Partition Fails Closed
// -----------------------------------------------------------------------------
// Verifies that linearizable GET on an isolated leader cannot reach majority
// confirmation, fails closed (StatusThrottled/StatusNotLeader), and never
// exposes uncommitted local writes.
func TestPartition_3Node_LinearizableRead_FailsClosed(t *testing.T) {
	c, cleanup := createPartitionCluster(t, 3, clusterOptions{})
	defer cleanup()

	waitForClusterPeerMesh(t, c, 3)

	node1 := c.nodes[1]

	if err := node1.node.BecomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := node1.node.BecomeLeader(); err != nil {
		t.Fatal(err)
	}

	// Pre-populate stale local value directly in engine
	_ = node1.sm.Put(context.Background(), []byte("read_key"), []byte("stale_value"))

	// Partition Node 1
	c.filter.Cut(1, 2)
	c.filter.Cut(1, 3)

	// Send linearizable GET over client TCP
	resp, err := sendClientRead(t, node1.server.Addr().String(), []byte("read_key"), 300)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	if resp.Status == transport.StatusOk {
		t.Fatalf("CRITICAL LINEARIZABILITY VIOLATION: Isolated leader served stale data! Value: %s", string(resp.Value))
	}
	if resp.Status != transport.StatusThrottled && resp.Status != transport.StatusNotLeader {
		t.Fatalf("expected StatusThrottled or StatusNotLeader, got: %s (%s)", resp.Status, resp.Message)
	}

	t.Log("Linearizable read during partition fails closed successfully.")
}

// -----------------------------------------------------------------------------
// Test 5: Isolated Leader Cannot Commit Multiple Writes (Test 2 in prompt)
// -----------------------------------------------------------------------------
// Demonstrates that even when an isolated leader receives a batch of multiple
// proposals while partitioned, NONE of them ever advance commitIndex or lastApplied.
func TestPartition_3Node_IsolatedLeader_CannotCommit(t *testing.T) {
	c, cleanup := createPartitionCluster(t, 3, clusterOptions{})
	defer cleanup()

	waitForClusterPeerMesh(t, c, 3)

	node1 := c.nodes[1]

	if err := node1.node.BecomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := node1.node.BecomeLeader(); err != nil {
		t.Fatal(err)
	}

	baselineCommit := node1.node.CommitIndex()
	baselineApplied := node1.node.LastApplied()

	// Isolate Node 1
	c.filter.Cut(1, 2)
	c.filter.Cut(1, 3)

	// Send 5 distinct writes to isolated leader
	for i := 1; i <= 5; i++ {
		key := fmt.Sprintf("iso_k%d", i)
		val := fmt.Sprintf("iso_v%d", i)
		resp, err := sendClientWrite(t, node1.server.Addr().String(), transport.OpPut, []byte(key), []byte(val), uint64(500+i))
		if err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}
		if resp.Status != transport.StatusOk {
			t.Fatalf("expected StatusOk on local WAL append, got: %s", resp.Status)
		}
	}

	// Verify all 5 entries exist in local WAL
	lastIdx, err := node1.storage.LastIndex()
	if err != nil {
		t.Fatalf("failed to read last index: %v", err)
	}
	if lastIdx < raft.LogIndex(baselineCommit)+5 {
		t.Fatalf("expected lastIndex >= %d, got %d", baselineCommit+5, lastIdx)
	}

	// Wait 100ms to allow any rogue replication or commit loops to fire
	time.Sleep(100 * time.Millisecond)

	// Invariant: commitIndex must NOT advance
	if node1.node.CommitIndex() != baselineCommit {
		t.Fatalf("commitIndex advanced on isolated leader without quorum: baseline=%d, current=%d",
			baselineCommit, node1.node.CommitIndex())
	}

	// Invariant: lastApplied must NOT advance
	if node1.node.LastApplied() != baselineApplied {
		t.Fatalf("lastApplied advanced on isolated leader without quorum: baseline=%d, current=%d",
			baselineApplied, node1.node.LastApplied())
	}

	// Invariant: zero keys applied to engine
	for i := 1; i <= 5; i++ {
		key := fmt.Sprintf("iso_k%d", i)
		if _, err := node1.sm.Get([]byte(key)); !stdErrors.Is(err, errors.ErrKeyNotFound) {
			t.Fatalf("uncommitted key %s was applied to state machine!", key)
		}
	}

	t.Log("Zero commits across 5 isolated proposals verified.")
}

// -----------------------------------------------------------------------------
// Test 6: Connection-Level Severance & Reconnection Verification (Findings L & M)
// -----------------------------------------------------------------------------
// Explicitly verifies:
//  1. Active TCP connections between partitioned peers are severed immediately.
//  2. While partitioned, background reconnection attempts fail closed.
//  3. Majority-side connections remain healthy.
//  4. Upon healing, reconnection succeeds and the full mesh is restored.
func TestPartition_ConnectionLevelSeveranceAndReconnection(t *testing.T) {
	c, cleanup := createPartitionCluster(t, 3, clusterOptions{})
	defer cleanup()

	waitForClusterPeerMesh(t, c, 3)

	node1 := c.nodes[1]
	node2 := c.nodes[2]
	node3 := c.nodes[3]

	// 1. Initial state: all connected
	for _, id := range []cluster.NodeID{2, 3} {
		if !node1.mgr.IsConnected(id) {
			t.Fatalf("expected node 1 initially connected to %d", id)
		}
	}
	if !node2.mgr.IsConnected(3) {
		t.Fatal("expected node 2 initially connected to 3")
	}

	dialsBefore := c.network.dialsCut.Load()

	// 2. Sever edges {1-2, 1-3}
	c.filter.Cut(1, 2)
	c.filter.Cut(1, 3)

	// Connections must be closed immediately
	severed := waitForCondition(2*time.Second, func() bool {
		return !node1.mgr.IsConnected(2) && !node1.mgr.IsConnected(3)
	})
	if !severed {
		t.Fatal("expected TCP connections to be severed upon partition cut")
	}

	// 3. Majority connectivity remains intact
	if !node2.mgr.IsConnected(3) || !node3.mgr.IsConnected(2) {
		t.Fatal("majority nodes 2 and 3 erroneously disconnected")
	}

	// 4. While partitioned, reconnection attempts must fail
	// Allow manager's reconnect loop (10-50ms) to attempt dials
	time.Sleep(150 * time.Millisecond)

	if node1.mgr.IsConnected(2) || node1.mgr.IsConnected(3) {
		t.Fatal("reconnect succeeded while edge was partitioned!")
	}
	dialsAfter := c.network.dialsCut.Load()
	if dialsAfter <= dialsBefore {
		t.Logf("Note: dials intercepted by partition: %d -> %d", dialsBefore, dialsAfter)
	}

	// 5. Heal partition
	c.filter.HealAll()

	// 6. Full mesh must be re-established
	waitForClusterPeerMesh(t, c, 3)

	for _, id := range []cluster.NodeID{2, 3} {
		if !node1.mgr.IsConnected(id) {
			t.Fatalf("expected node 1 reconnected to %d after heal", id)
		}
	}
	t.Log("Connection-level severance, rejection, and post-heal reconnection verified.")
}
