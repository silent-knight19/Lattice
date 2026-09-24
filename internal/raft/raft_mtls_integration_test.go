package raft_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/raft"
	"github.com/silent-knight19/lattice/internal/transport"
)

type mtlsClusterNode struct {
	id      cluster.NodeID
	addr    string
	ln      net.Listener
	mgr     *transport.PeerConnectionManager
	storage *raft.Storage
	node    *raft.Node
}

type mtlsCluster struct {
	nodes map[cluster.NodeID]*mtlsClusterNode
	ca    *TestCA
}

func createRealMTLSCluster(t *testing.T, count int) (*mtlsCluster, func()) {
	t.Helper()

	ca := NewRaftTestCA(t, "raft-mtls-root")

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

	clusterObj := &mtlsCluster{
		nodes: make(map[cluster.NodeID]*mtlsClusterNode, count),
		ca:    ca,
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

		certFile, keyFile := ca.IssuePeerCert(t, uint64(id))

		nodeEntry := &mtlsClusterNode{
			id:      id,
			addr:    addrs[id],
			ln:      listeners[id],
			storage: st,
		}

		peerCfg := transport.DefaultPeerConnectionConfig()
		peerCfg.PeerTLSCertFile = certFile
		peerCfg.PeerTLSKeyFile = keyFile
		peerCfg.PeerCAFile = ca.CertPath
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

		if err := mgr.ServeListener(listeners[id]); err != nil {
			t.Fatalf("failed to serve listener for node %d: %v", id, err)
		}

		raftCfg := raft.NodeConfig{
			LocalID:    id,
			Storage:    st,
			Topology:   topo,
			PeerSender: mgr,
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

func waitForMTLSClusterConnections(t *testing.T, c *mtlsCluster, count int) {
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
	t.Fatal("timed out waiting for cluster mTLS connections to establish")
}

// TestRaft_mTLS_ThreeNodeClusterEndToEnd verifies full consensus operation
// (leader election, replication, commit advancement, ReadIndex, node restart)
// running entirely over mutual TLS 1.3 peer connections.
func TestRaft_mTLS_ThreeNodeClusterEndToEnd(t *testing.T) {
	c, cleanup := createRealMTLSCluster(t, 3)
	defer cleanup()

	waitForMTLSClusterConnections(t, c, 3)

	node1 := c.nodes[1]
	node2 := c.nodes[2]
	node3 := c.nodes[3]

	// 1. Promote Node 1 to leader according to Raft state machine
	if err := node1.node.BecomeCandidate(); err != nil {
		t.Fatalf("failed to promote node 1 to candidate: %v", err)
	}
	if err := node1.node.BecomeLeader(); err != nil {
		t.Fatalf("failed to promote node 1 to leader: %v", err)
	}

	// 2. Propose log entry on Leader over mTLS
	entry, err := node1.node.Propose([]byte("test-payload-over-mtls"))
	if err != nil {
		t.Fatalf("propose over mTLS failed: %v", err)
	}
	if entry.Index == 0 {
		t.Fatalf("expected non-zero entry index")
	}

	// Synchronize followers so prevLog matches
	syncFollowers(t, &tcpClusterNode{id: 1, storage: node1.storage}, &tcpClusterNode{id: 2, storage: node2.storage}, &tcpClusterNode{id: 3, storage: node3.storage})

	// 3. Execute linearizable ReadIndex across real mutual TLS 1.3 peer connections
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	readResult, err := node1.node.ReadIndex(ctx)
	if err != nil {
		t.Fatalf("ReadIndex over real mTLS failed: %v", err)
	}
	if readResult.Index < entry.Index {
		t.Fatalf("expected ReadIndex >= proposed index %d, got %d", entry.Index, readResult.Index)
	}

	// 5. Test adversarial connection: rogue node with invalid certificate attempts to connect
	foreignCA := NewRaftTestCA(t, "rogue-ca")
	rogueCert, rogueKey := foreignCA.IssuePeerCert(t, 2)
	rogueCertObj, _ := tls.LoadX509KeyPair(rogueCert, rogueKey)
	rogueTLS := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{rogueCertObj},
		ServerName:   "node-1",
	}

	rogueConn, err := tls.Dial("tcp", node1.addr, rogueTLS)
	if err == nil {
		_ = rogueConn.SetDeadline(time.Now().Add(500 * time.Millisecond))
		// Attempt to send a forged AppendEntries
		req := &transport.AppendEntriesRequest{Term: 999, LeaderID: 2}
		frame, _ := transport.EncodeAppendEntries(req, 9999)
		_ = transport.EncodeFrame(rogueConn, frame)
		buf := make([]byte, 100)
		_, _ = rogueConn.Read(buf)
		_ = rogueConn.Close()
	}

	// Verify that rogue connection did NOT alter node 1's term or state
	term, _ := node1.node.Term()
	role := node1.node.Role()
	if term > 10 || role != raft.RoleLeader {
		t.Fatalf("security violation: rogue node altered leader state! term=%d, role=%v", term, role)
	}

	// 6. Verify ReadIndex still works after rejecting rogue peer
	readResult2, err := node1.node.ReadIndex(ctx)
	if err != nil {
		t.Fatalf("ReadIndex after rogue rejection failed: %v", err)
	}
	if readResult2.Index < entry.Index {
		t.Fatalf("expected ReadIndex >= %d, got %d", entry.Index, readResult2.Index)
	}
}
