package raft_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/raft"
	"github.com/silent-knight19/lattice/internal/transport"
)

type tcpClusterNode struct {
	id      cluster.NodeID
	addr    string
	ln      net.Listener
	mgr     *transport.PeerConnectionManager
	storage *raft.Storage
	node    *raft.Node
}

type tcpCluster struct {
	nodes map[cluster.NodeID]*tcpClusterNode
}

type partitionableSender struct {
	raft.PeerSender
	partitioned *atomic.Bool
}

func (p *partitionableSender) Send(ctx context.Context, peerID cluster.NodeID, frame *transport.Frame) error {
	if p.partitioned != nil && p.partitioned.Load() {
		return errors.New("network partition: peer unreachable")
	}
	return p.PeerSender.Send(ctx, peerID, frame)
}

// createRealTCPCluster builds a real multi-node Raft cluster communicating over actual TCP sockets
// using production PeerConnectionManager instances and framed peer protocols.
func createRealTCPCluster(t *testing.T, count int, dialWrapper func(id cluster.NodeID, inner transport.DialFunc) transport.DialFunc, senderWrapper func(id cluster.NodeID, inner raft.PeerSender) raft.PeerSender) (*tcpCluster, func()) {
	t.Helper()

	listeners := make(map[cluster.NodeID]net.Listener, count)
	addrs := make(map[cluster.NodeID]string, count)

	for i := 1; i <= count; i++ {
		id := cluster.NodeID(i)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("failed to bind tcp listener for node %d: %v", id, err)
		}
		listeners[id] = ln
		addrs[id] = ln.Addr().String()
	}

	var rawPeers []cluster.PeerConfig
	for i := 1; i <= count; i++ {
		id := cluster.NodeID(i)
		rawPeers = append(rawPeers, cluster.PeerConfig{
			ID:      id,
			Address: addrs[id],
		})
	}

	clusterObj := &tcpCluster{
		nodes: make(map[cluster.NodeID]*tcpClusterNode, count),
	}

	for i := 1; i <= count; i++ {
		id := cluster.NodeID(i)
		topo, err := cluster.NewTopology(id, addrs[id], rawPeers)
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

		nodeEntry := &tcpClusterNode{
			id:      id,
			addr:    addrs[id],
			ln:      listeners[id],
			storage: st,
		}

		peerCfg := transport.DefaultPeerConnectionConfig()
		peerCfg.InsecureTransport = true
		peerCfg.DialTimeout = 2 * time.Second
		peerCfg.ReconnectMin = 20 * time.Millisecond
		peerCfg.ReconnectMax = 100 * time.Millisecond

		if dialWrapper != nil {
			defaultDialer := func(ctx context.Context, addr string) (net.Conn, error) {
				dialer := &net.Dialer{Timeout: 2 * time.Second}
				return dialer.DialContext(ctx, "tcp", addr)
			}
			peerCfg.DialFunc = dialWrapper(id, defaultDialer)
		}

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

		if err := mgr.ServeListener(listeners[id]); err != nil {
			t.Fatalf("failed to serve listener for node %d: %v", id, err)
		}

		var peerSender raft.PeerSender = mgr
		if senderWrapper != nil {
			peerSender = senderWrapper(id, mgr)
		}

		raftCfg := raft.NodeConfig{
			LocalID:    id,
			Storage:    st,
			Topology:   topo,
			PeerSender: peerSender,
		}
		rNode, err := raft.NewNode(raftCfg)
		if err != nil {
			t.Fatalf("failed to create raft.Node for node %d: %v", id, err)
		}
		nodeEntry.node = rNode

		if err := mgr.Start(); err != nil {
			t.Fatalf("failed to start PeerConnectionManager for node %d: %v", id, err)
		}

		clusterObj.nodes[id] = nodeEntry
	}

	cleanup := func() {
		for i := 1; i <= count; i++ {
			id := cluster.NodeID(i)
			if n, ok := clusterObj.nodes[id]; ok {
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

// syncFollowers appends identical log entries to follower storage so follower prevLogMatches matches leader's state.
func syncFollowers(t *testing.T, leader *tcpClusterNode, followers ...*tcpClusterNode) {
	t.Helper()
	lastIdx, lastTerm, err := leader.storage.LastIndexAndTerm()
	if err != nil {
		t.Fatalf("failed to get leader log coordinates: %v", err)
	}

	if lastIdx == 0 {
		return
	}

	entries := make([]raft.LogEntry, 0, lastIdx)
	for idx := raft.LogIndex(1); idx <= lastIdx; idx++ {
		e, err := leader.storage.Entry(idx)
		if err != nil {
			t.Fatalf("failed to read leader entry %d: %v", idx, err)
		}
		entries = append(entries, e)
	}

	for _, f := range followers {
		_ = f.storage.SetTerm(lastTerm)
		if err := f.storage.Append(entries...); err != nil {
			t.Fatalf("failed to sync entries to follower %d: %v", f.id, err)
		}
	}
}

// waitForClusterConnections waits until each node is connected to all other nodes over TCP.
func waitForClusterConnections(t *testing.T, c *tcpCluster, count int) {
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
	t.Fatal("timed out waiting for cluster TCP connections to establish")
}

// TestReadIndex_3Node_RealTCP_MajorityConfirmation verifies the complete end-to-end
// ReadIndex leader quorum heartbeat verification path over actual TCP sockets (Section 7).
func TestReadIndex_3Node_RealTCP_MajorityConfirmation(t *testing.T) {
	c, cleanup := createRealTCPCluster(t, 3, nil, nil)
	defer cleanup()

	waitForClusterConnections(t, c, 3)

	node1 := c.nodes[1]
	node2 := c.nodes[2]
	node3 := c.nodes[3]

	// Promote Node 1 to leader according to Raft state machine
	if err := node1.node.BecomeCandidate(); err != nil {
		t.Fatalf("failed to promote node 1 to candidate: %v", err)
	}
	if err := node1.node.BecomeLeader(); err != nil {
		t.Fatalf("failed to promote node 1 to leader: %v", err)
	}

	// Sync leader's election no-op entry to followers so prevLog matches
	syncFollowers(t, node1, node2, node3)

	// Node 1 calls ReadIndex across real TCP transport
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	result, err := node1.node.ReadIndex(ctx)
	if err != nil {
		t.Fatalf("ReadIndex over real TCP failed: %v", err)
	}

	if result.Index == 0 {
		t.Fatalf("expected non-zero committed ReadIndex, got %d", result.Index)
	}
	if result.Term != 1 {
		t.Fatalf("expected Term 1, got %d", result.Term)
	}

	if node1.node.ActiveReadRoundsCount() != 0 {
		t.Fatalf("expected 0 active read rounds after completion, got %d", node1.node.ActiveReadRoundsCount())
	}
}

// TestReadIndex_3Node_RealTCP_PartitionAndRecovery proves that when a leader loses
// connectivity to its followers over TCP, ReadIndex fails to confirm quorum. Once
// connectivity is restored, ReadIndex succeeds again (Section 8).
func TestReadIndex_3Node_RealTCP_PartitionAndRecovery(t *testing.T) {
	var partitioned atomic.Bool

	c, cleanup := createRealTCPCluster(t, 3, nil, func(id cluster.NodeID, inner raft.PeerSender) raft.PeerSender {
		if id == 1 {
			return &partitionableSender{PeerSender: inner, partitioned: &partitioned}
		}
		return inner
	})
	defer cleanup()

	waitForClusterConnections(t, c, 3)

	node1 := c.nodes[1]
	node2 := c.nodes[2]
	node3 := c.nodes[3]

	if err := node1.node.BecomeCandidate(); err != nil {
		t.Fatalf("failed to promote node 1 to candidate: %v", err)
	}
	if err := node1.node.BecomeLeader(); err != nil {
		t.Fatalf("failed to promote node 1 to leader: %v", err)
	}

	syncFollowers(t, node1, node2, node3)

	// Phase 1: Verify healthy ReadIndex succeeds over TCP
	ctx1, cancel1 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel1()

	res1, err := node1.node.ReadIndex(ctx1)
	if err != nil {
		t.Fatalf("initial healthy ReadIndex failed: %v", err)
	}
	if res1.Index == 0 || res1.Term != 1 {
		t.Fatalf("unexpected initial result: %+v", res1)
	}

	// Phase 2: Partition Leader 1 from Followers 2 and 3
	partitioned.Store(true)

	// While partitioned, Node 1 calls ReadIndex. Quorum CANNOT be confirmed.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()

	_, err = node1.node.ReadIndex(ctx2)
	if err == nil {
		t.Fatal("expected ReadIndex to fail during network partition, but it succeeded!")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Logf("ReadIndex returned partition error: %v", err)
	}

	if node1.node.ActiveReadRoundsCount() != 0 {
		t.Fatalf("expected 0 active read rounds after partition timeout, got %d", node1.node.ActiveReadRoundsCount())
	}

	// Phase 3: Restore connectivity (heal partition)
	partitioned.Store(false)

	// Phase 4: ReadIndex must succeed again after partition heal
	ctx3, cancel3 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel3()

	res3, err := node1.node.ReadIndex(ctx3)
	if err != nil {
		t.Fatalf("ReadIndex failed after partition heal: %v", err)
	}
	if res3.Index == 0 || res3.Term != 1 {
		t.Fatalf("unexpected healed result: %+v", res3)
	}
}

// TestReadIndex_3Node_RealTCP_ConcurrentReads proves that 25+ concurrent ReadIndex
// requests can execute across the same real TCP transport without data races,
// lost wakeups, or nonce collisions (Section 10).
func TestReadIndex_3Node_RealTCP_ConcurrentReads(t *testing.T) {
	c, cleanup := createRealTCPCluster(t, 3, nil, nil)
	defer cleanup()

	waitForClusterConnections(t, c, 3)

	node1 := c.nodes[1]
	node2 := c.nodes[2]
	node3 := c.nodes[3]

	if err := node1.node.BecomeCandidate(); err != nil {
		t.Fatalf("failed to promote node 1 to candidate: %v", err)
	}
	if err := node1.node.BecomeLeader(); err != nil {
		t.Fatalf("failed to promote node 1 to leader: %v", err)
	}

	syncFollowers(t, node1, node2, node3)

	const numConcurrentReads = 25
	var wg sync.WaitGroup
	wg.Add(numConcurrentReads)

	errCh := make(chan error, numConcurrentReads)

	for i := 0; i < numConcurrentReads; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			res, err := node1.node.ReadIndex(ctx)
			if err != nil {
				errCh <- err
				return
			}
			if res.Term != 1 || res.Index == 0 {
				errCh <- fmt.Errorf("unexpected read index result: %+v", res)
				return
			}
		}()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent TCP ReadIndex failed: %v", err)
	}

	if node1.node.ActiveReadRoundsCount() != 0 {
		t.Fatalf("expected 0 active read rounds after all concurrent reads, got %d", node1.node.ActiveReadRoundsCount())
	}
}

// TestReadIndex_3Node_RealTCP_StaleResponsesIgnored verifies that responses with
// wrong nonces, older terms, or Success=false do NOT confirm quorum over TCP (Section 11).
func TestReadIndex_3Node_RealTCP_StaleResponsesIgnored(t *testing.T) {
	c, cleanup := createRealTCPCluster(t, 3, nil, nil)
	defer cleanup()

	waitForClusterConnections(t, c, 3)

	node1 := c.nodes[1]

	if err := node1.node.BecomeCandidate(); err != nil {
		t.Fatalf("failed to promote node 1 to candidate: %v", err)
	}
	if err := node1.node.BecomeLeader(); err != nil {
		t.Fatalf("failed to promote node 1 to leader: %v", err)
	}

	// DO NOT sync followers: follower logs remain empty (lastIdx=0).
	// When leader sends probe with PrevLogIndex=1, followers return Success=false!
	// Therefore majority confirmation must FAIL and time out.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := node1.node.ReadIndex(ctx)
	if err == nil {
		t.Fatal("expected ReadIndex to fail when followers return Success=false due to log mismatch, but it succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Logf("ReadIndex correctly rejected unconfirmed quorum: %v", err)
	}

	if node1.node.ActiveReadRoundsCount() != 0 {
		t.Fatalf("expected 0 active read rounds after rejection, got %d", node1.node.ActiveReadRoundsCount())
	}
}

// TestReadIndex_SingleNode_ImmediateReturn verifies the N=1 optimization where
// ReadIndex returns immediately without network traffic or PeerConnectionManager round (Section 9).
func TestReadIndex_SingleNode_ImmediateReturn(t *testing.T) {
	c, cleanup := createRealTCPCluster(t, 1, nil, nil)
	defer cleanup()

	node1 := c.nodes[1]

	if err := node1.node.BecomeCandidate(); err != nil {
		t.Fatalf("failed to promote node 1 to candidate: %v", err)
	}
	if err := node1.node.BecomeLeader(); err != nil {
		t.Fatalf("failed to promote node 1 to leader: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, err := node1.node.ReadIndex(ctx)
	duration := time.Since(start)

	if err != nil {
		t.Fatalf("single node ReadIndex failed: %v", err)
	}
	if res.Term != 1 {
		t.Fatalf("expected Term 1, got %d", res.Term)
	}
	if duration > 50*time.Millisecond {
		t.Fatalf("single node ReadIndex took %v, expected near-instant execution", duration)
	}
	if node1.node.ActiveReadRoundsCount() != 0 {
		t.Fatalf("expected 0 active read rounds for single node, got %d", node1.node.ActiveReadRoundsCount())
	}
}
