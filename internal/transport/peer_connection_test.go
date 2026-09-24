package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cluster"
	errs "github.com/silent-knight19/lattice/internal/errors"
)

// Helper to create a test topology with 1 local node and specified remote peers.
func createTestTopology(t *testing.T, localID cluster.NodeID, localAddr string, remotes map[cluster.NodeID]string) *cluster.Topology {
	t.Helper()
	var rawPeers []cluster.PeerConfig
	rawPeers = append(rawPeers, cluster.PeerConfig{ID: localID, Address: localAddr})
	for id, addr := range remotes {
		rawPeers = append(rawPeers, cluster.PeerConfig{ID: id, Address: addr})
	}
	topo, err := cluster.NewTopology(localID, localAddr, rawPeers)
	if err != nil {
		t.Fatalf("failed to create test topology: %v", err)
	}
	return topo
}

// Helper to allocate a local TCP listener on loopback.
func createTestListener(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create test listener: %v", err)
	}
	return l
}

func TestPeerConnectionManager_ConstructionValidation(t *testing.T) {
	// 1. Nil topology rejected
	_, err := NewPeerConnectionManager(nil, DefaultPeerConnectionConfig())
	if !errors.Is(err, errs.ErrNilReceiver) {
		t.Fatalf("expected ErrNilReceiver for nil topology, got: %v", err)
	}

	// 2. Single-node topology: 0 remote workers
	topo1 := createTestTopology(t, 1, "127.0.0.1:9001", nil)
	mgr1, err := NewPeerConnectionManager(topo1, DefaultPeerConnectionConfig())
	if err != nil {
		t.Fatalf("unexpected error constructing single-node manager: %v", err)
	}
	if len(mgr1.supervisors) != 0 {
		t.Fatalf("single-node topology must have 0 supervisors, got %d", len(mgr1.supervisors))
	}
	if err := mgr1.Start(); err != nil {
		t.Fatalf("failed to start single-node manager: %v", err)
	}
	if err := mgr1.Close(); err != nil {
		t.Fatalf("failed to close single-node manager: %v", err)
	}

	// 3. Negative timeout rejected
	topo2 := createTestTopology(t, 1, "127.0.0.1:9001", map[cluster.NodeID]string{2: "127.0.0.1:9002"})
	badCfg1 := DefaultPeerConnectionConfig()
	badCfg1.DialTimeout = -1 * time.Second
	_, err = NewPeerConnectionManager(topo2, badCfg1)
	if !errors.Is(err, errs.ErrInvalidManagerConfig) {
		t.Fatalf("expected ErrInvalidManagerConfig for negative timeout, got: %v", err)
	}

	// 4. ReconnectMin > ReconnectMax rejected
	badCfg2 := DefaultPeerConnectionConfig()
	badCfg2.ReconnectMin = 10 * time.Second
	badCfg2.ReconnectMax = 1 * time.Second
	_, err = NewPeerConnectionManager(topo2, badCfg2)
	if !errors.Is(err, errs.ErrInvalidManagerConfig) {
		t.Fatalf("expected ErrInvalidManagerConfig for ReconnectMin > ReconnectMax, got: %v", err)
	}

	// 5. Self is never dialed / self never receives a supervisor
	mgr2, err := NewPeerConnectionManager(topo2, DefaultPeerConnectionConfig())
	if err != nil {
		t.Fatalf("failed to construct valid manager: %v", err)
	}
	if _, ok := mgr2.supervisors[topo2.LocalID()]; ok {
		t.Fatalf("local ID %d must NEVER have a supervisor worker", topo2.LocalID())
	}
	if len(mgr2.supervisors) != 1 {
		t.Fatalf("expected exactly 1 supervisor for 1 remote peer, got %d", len(mgr2.supervisors))
	}
	_ = mgr2.Close()
}

func TestPeerConnectionManager_InitialConnectionAndState(t *testing.T) {
	ln := createTestListener(t)
	defer ln.Close()

	topo := createTestTopology(t, 1, "127.0.0.1:9001", map[cluster.NodeID]string{
		2: ln.Addr().String(),
	})

	var acceptedConn net.Conn
	var acceptMu sync.Mutex
	acceptCh := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			acceptMu.Lock()
			acceptedConn = conn
			acceptMu.Unlock()
			close(acceptCh)
		}
	}()

	cfg := DefaultPeerConnectionConfig()
	cfg.DialTimeout = 2 * time.Second
	cfg.ReconnectMin = 20 * time.Millisecond
	cfg.ReconnectMax = 100 * time.Millisecond

	mgr, err := NewPeerConnectionManager(topo, cfg)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	defer mgr.Close()

	// Before Start, state should be Disconnected
	st, err := mgr.GetPeerState(2)
	if err != nil {
		t.Fatalf("failed to get peer state: %v", err)
	}
	if st != PeerStateDisconnected {
		t.Fatalf("expected initial state Disconnected, got: %v", st)
	}

	if err := mgr.Start(); err != nil {
		t.Fatalf("failed to start manager: %v", err)
	}

	// Double start should fail
	if err := mgr.Start(); !errors.Is(err, errs.ErrManagerAlreadyStarted) {
		t.Fatalf("expected ErrManagerAlreadyStarted, got: %v", err)
	}

	// Wait for connection to be accepted
	select {
	case <-acceptCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for listener to accept peer connection")
	}
	defer func() {
		acceptMu.Lock()
		if acceptedConn != nil {
			acceptedConn.Close()
		}
		acceptMu.Unlock()
	}()

	// Wait for peer state to transition to Connected
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.IsConnected(2) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !mgr.IsConnected(2) {
		st, _ := mgr.GetPeerState(2)
		t.Fatalf("peer 2 failed to reach Connected state, currently: %v", st)
	}

	connected := mgr.ConnectedPeers()
	if len(connected) != 1 || connected[0] != 2 {
		t.Fatalf("unexpected connected peers: %v", connected)
	}
}

func TestPeerConnectionManager_SendAndReceiveFrames(t *testing.T) {
	ln := createTestListener(t)
	defer ln.Close()

	topo := createTestTopology(t, 1, "127.0.0.1:9001", map[cluster.NodeID]string{
		2: ln.Addr().String(),
	})

	serverConnCh := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			serverConnCh <- conn
		}
	}()

	receivedFrames := make(chan *Frame, 10)
	cfg := DefaultPeerConnectionConfig()
	cfg.DialTimeout = 2 * time.Second
	cfg.ReconnectMin = 20 * time.Millisecond
	cfg.ReconnectMax = 100 * time.Millisecond
	cfg.OnFrameReceived = func(peerID cluster.NodeID, frame *Frame) {
		if peerID != 2 {
			t.Errorf("expected frame from peer 2, got %d", peerID)
		}
		receivedFrames <- frame
	}

	mgr, err := NewPeerConnectionManager(topo, cfg)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	defer mgr.Close()

	if err := mgr.Start(); err != nil {
		t.Fatalf("failed to start manager: %v", err)
	}

	var serverConn net.Conn
	select {
	case serverConn = <-serverConnCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for server connection")
	}
	defer serverConn.Close()

	// Wait until client manager reaches connected state
	for i := 0; i < 100; i++ {
		if mgr.IsConnected(2) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !mgr.IsConnected(2) {
		t.Fatal("client did not reach connected state")
	}

	// 1. Client sends RequestVoteRequest frame to server
	rvReq := &RequestVoteRequest{
		Term:         10,
		CandidateID:  1,
		LastLogIndex: 100,
		LastLogTerm:  9,
		Nonce:        99999,
	}
	rvFrame, err := EncodeRequestVote(rvReq, 42)
	if err != nil {
		t.Fatalf("failed to encode RequestVote: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := mgr.Send(ctx, 2, rvFrame); err != nil {
		t.Fatalf("failed to send frame via manager: %v", err)
	}

	// Server decodes frame from wire
	serverFrame, err := DecodeFrame(serverConn)
	if err != nil {
		t.Fatalf("server failed to decode frame: %v", err)
	}
	if serverFrame.Header.OpCode != OpCode(PeerOpRequestVote) {
		t.Fatalf("opcode mismatch: got %v, expected %v", serverFrame.Header.OpCode, PeerOpRequestVote)
	}
	if serverFrame.Header.SeqID != 42 {
		t.Fatalf("seqID mismatch: got %d, expected 42", serverFrame.Header.SeqID)
	}
	decodedReq, err := DecodeRequestVote(serverFrame)
	if err != nil {
		t.Fatalf("failed to decode RequestVote payload: %v", err)
	}
	if decodedReq.Term != 10 || decodedReq.CandidateID != 1 || decodedReq.Nonce != 99999 {
		t.Fatalf("decoded RequestVote mismatch: %+v", decodedReq)
	}

	// 2. Server sends response frame back to client; client's OnFrameReceived must receive it
	rvResp := &RequestVoteResponse{
		Term:        10,
		VoteGranted: true,
	}
	respFrame, err := EncodeRequestVoteResponse(rvResp, 42)
	if err != nil {
		t.Fatalf("failed to encode response: %v", err)
	}
	if err := EncodeFrame(serverConn, respFrame); err != nil {
		t.Fatalf("server failed to send response: %v", err)
	}

	select {
	case clientFrame := <-receivedFrames:
		if clientFrame.Header.OpCode != OpCode(PeerOpRequestVoteResponse) {
			t.Fatalf("client received wrong opcode: %v", clientFrame.Header.OpCode)
		}
		if clientFrame.Header.SeqID != 42 {
			t.Fatalf("client received wrong seqID: %d", clientFrame.Header.SeqID)
		}
		decodedResp, err := DecodeRequestVoteResponse(clientFrame)
		if err != nil {
			t.Fatalf("failed to decode client response: %v", err)
		}
		if decodedResp.Term != 10 || !decodedResp.VoteGranted {
			t.Fatalf("decoded response mismatch: %+v", decodedResp)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for client OnFrameReceived callback")
	}

	// 3. Client sends AppendEntriesRequest
	aeReq := &AppendEntriesRequest{
		Term:         11,
		LeaderID:     1,
		PrevLogIndex: 100,
		PrevLogTerm:  9,
		LeaderCommit: 95,
		Nonce:        12345,
		Entries: []PeerLogEntry{
			{Term: 11, Type: PeerEntryNormal, Data: []byte("raft-command-payload")},
		},
	}
	aeFrame, err := EncodeAppendEntries(aeReq, 43)
	if err != nil {
		t.Fatalf("failed to encode AppendEntries: %v", err)
	}
	if err := mgr.Send(ctx, 2, aeFrame); err != nil {
		t.Fatalf("failed to send AppendEntries: %v", err)
	}

	serverAeFrame, err := DecodeFrame(serverConn)
	if err != nil {
		t.Fatalf("server failed to decode AppendEntries: %v", err)
	}
	decodedAe, err := DecodeAppendEntries(serverAeFrame)
	if err != nil {
		t.Fatalf("failed to decode AppendEntries payload: %v", err)
	}
	if decodedAe.Term != 11 || len(decodedAe.Entries) != 1 || string(decodedAe.Entries[0].Data) != "raft-command-payload" {
		t.Fatalf("decoded AppendEntries mismatch: %+v", decodedAe)
	}
}

func TestPeerConnectionManager_SendErrors(t *testing.T) {
	ln := createTestListener(t)
	defer ln.Close()

	topo := createTestTopology(t, 1, "127.0.0.1:9001", map[cluster.NodeID]string{
		2: ln.Addr().String(),
	})

	cfg := DefaultPeerConnectionConfig()
	mgr, err := NewPeerConnectionManager(topo, cfg)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	frame := &Frame{Header: Header{Magic: Magic, OpCode: OpCode(PeerOpRequestVote)}}

	// 1. Send with nil frame rejected
	if err := mgr.Send(context.Background(), 2, nil); !errors.Is(err, errs.ErrNilReceiver) {
		t.Fatalf("expected ErrNilReceiver for nil frame, got: %v", err)
	}

	// 2. Send before Start rejected with ErrManagerNotStarted
	if err := mgr.Send(context.Background(), 2, frame); !errors.Is(err, errs.ErrManagerNotStarted) {
		t.Fatalf("expected ErrManagerNotStarted, got: %v", err)
	}

	// 3. Send to unknown peer rejected with ErrPeerNotFound
	_ = mgr.Start()
	if err := mgr.Send(context.Background(), 99, frame); !errors.Is(err, errs.ErrPeerNotFound) {
		t.Fatalf("expected ErrPeerNotFound for unknown peer 99, got: %v", err)
	}

	// 4. Send to self rejected with ErrPeerNotFound (self is not in RemotePeers)
	if err := mgr.Send(context.Background(), 1, frame); !errors.Is(err, errs.ErrPeerNotFound) {
		t.Fatalf("expected ErrPeerNotFound for self, got: %v", err)
	}

	// 5. Send when peer is disconnected rejected with ErrPeerUnavailable
	if err := mgr.Send(context.Background(), 2, frame); !errors.Is(err, errs.ErrPeerUnavailable) {
		t.Fatalf("expected ErrPeerUnavailable for disconnected peer, got: %v", err)
	}

	// 6. Send on closed manager rejected with ErrManagerClosed
	_ = mgr.Close()
	if err := mgr.Send(context.Background(), 2, frame); !errors.Is(err, errs.ErrManagerClosed) {
		t.Fatalf("expected ErrManagerClosed, got: %v", err)
	}
}

func TestPeerConnectionManager_ConcurrentWritesSerialization(t *testing.T) {
	ln := createTestListener(t)
	defer ln.Close()

	topo := createTestTopology(t, 1, "127.0.0.1:9001", map[cluster.NodeID]string{
		2: ln.Addr().String(),
	})

	serverConnCh := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			serverConnCh <- conn
		}
	}()

	cfg := DefaultPeerConnectionConfig()
	cfg.DialTimeout = 2 * time.Second
	cfg.WriteTimeout = 5 * time.Second

	mgr, err := NewPeerConnectionManager(topo, cfg)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	defer mgr.Close()

	if err := mgr.Start(); err != nil {
		t.Fatalf("failed to start manager: %v", err)
	}

	var serverConn net.Conn
	select {
	case serverConn = <-serverConnCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for server conn")
	}
	defer serverConn.Close()

	// Wait until client connected
	for i := 0; i < 100; i++ {
		if mgr.IsConnected(2) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !mgr.IsConnected(2) {
		t.Fatal("client did not connect")
	}

	const numGoroutines = 20
	const msgsPerGoroutine = 10
	totalExpected := numGoroutines * msgsPerGoroutine

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for g := 0; g < numGoroutines; g++ {
		go func(goroutineID int) {
			defer wg.Done()
			for m := 0; m < msgsPerGoroutine; m++ {
				seq := uint64(goroutineID*1000 + m)
				req := &RequestVoteRequest{
					Term:         1,
					CandidateID:  1,
					LastLogIndex: seq,
					LastLogTerm:  1,
					Nonce:        seq,
				}
				frame, encErr := EncodeRequestVote(req, seq)
				if encErr != nil {
					t.Errorf("encode failed: %v", encErr)
					return
				}

				if sendErr := mgr.Send(context.Background(), 2, frame); sendErr != nil {
					t.Errorf("send failed: %v", sendErr)
					return
				}
			}
		}(g)
	}

	// Server receives all frames and verifies they are structurally valid and not interleaved
	receivedCount := 0
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for receivedCount < totalExpected {
			f, err := DecodeFrame(serverConn)
			if err != nil {
				t.Errorf("server DecodeFrame failed at count %d: %v", receivedCount, err)
				return
			}
			if f.Header.OpCode != OpCode(PeerOpRequestVote) {
				t.Errorf("unexpected opcode: %v", f.Header.OpCode)
				return
			}
			receivedCount++
		}
	}()

	wg.Wait()

	select {
	case <-serverDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("server timed out; received %d/%d frames", receivedCount, totalExpected)
	}

	if receivedCount != totalExpected {
		t.Fatalf("expected %d frames, server got %d", totalExpected, receivedCount)
	}
}

func TestPeerConnectionManager_AutomaticReconnectAndBackoff(t *testing.T) {
	// Create first listener on a fixed port by binding to 0 and grabbing the port
	ln1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	addrStr := ln1.Addr().String()

	topo := createTestTopology(t, 1, "127.0.0.1:9001", map[cluster.NodeID]string{
		2: addrStr,
	})

	var connCount atomic.Int32
	var closeServerConn atomic.Pointer[net.Conn]

	acceptLoop := func(ln net.Listener, stopCh chan struct{}) {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			connCount.Add(1)
			closeServerConn.Store(&conn)
			select {
			case <-stopCh:
				conn.Close()
				return
			default:
			}
		}
	}

	stop1 := make(chan struct{})
	go acceptLoop(ln1, stop1)

	cfg := DefaultPeerConnectionConfig()
	cfg.DialTimeout = 500 * time.Millisecond
	cfg.ReconnectMin = 30 * time.Millisecond
	cfg.ReconnectMax = 150 * time.Millisecond

	mgr, err := NewPeerConnectionManager(topo, cfg)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	defer mgr.Close()

	if err := mgr.Start(); err != nil {
		t.Fatalf("failed to start manager: %v", err)
	}

	// 1. Await initial connection
	for i := 0; i < 100; i++ {
		if mgr.IsConnected(2) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !mgr.IsConnected(2) {
		t.Fatal("failed to establish initial connection")
	}
	if connCount.Load() != 1 {
		t.Fatalf("expected 1 connection, got %d", connCount.Load())
	}

	// 2. Kill server 1
	close(stop1)
	if c := closeServerConn.Load(); c != nil && *c != nil {
		(*c).Close()
	}
	ln1.Close()

	// Wait for client to detect disconnect
	for i := 0; i < 100; i++ {
		if !mgr.IsConnected(2) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if mgr.IsConnected(2) {
		t.Fatal("client should have detected connection closure")
	}

	// Reconnect attempts will fail while listener is down (backoff will be exercised)
	time.Sleep(100 * time.Millisecond)

	// 3. Restart server on the SAME address
	ln2, err := net.Listen("tcp", addrStr)
	if err != nil {
		t.Fatalf("failed to restart listener on %s: %v", addrStr, err)
	}
	defer ln2.Close()

	stop2 := make(chan struct{})
	defer close(stop2)
	go acceptLoop(ln2, stop2)

	// 4. Client should automatically reconnect and server should accept!
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.IsConnected(2) && connCount.Load() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !mgr.IsConnected(2) {
		st, _ := mgr.GetPeerState(2)
		t.Fatalf("client failed to reconnect after server restarted, current state: %v", st)
	}

	if connCount.Load() < 2 {
		t.Fatalf("expected at least 2 accepted connections, got %d", connCount.Load())
	}
}

func TestPeerConnectionManager_StaleGenerationDefense(t *testing.T) {
	ln := createTestListener(t)
	defer ln.Close()

	topo := createTestTopology(t, 1, "127.0.0.1:9001", map[cluster.NodeID]string{
		2: ln.Addr().String(),
	})

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c
		}
	}()

	cfg := DefaultPeerConnectionConfig()
	mgr, err := NewPeerConnectionManager(topo, cfg)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	defer mgr.Close()

	if err := mgr.Start(); err != nil {
		t.Fatalf("failed to start: %v", err)
	}

	// Wait for connected
	for i := 0; i < 100; i++ {
		if mgr.IsConnected(2) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !mgr.IsConnected(2) {
		t.Fatal("not connected")
	}

	sup := mgr.supervisors[2]
	sup.mu.RLock()
	activeGen := sup.generation
	activeConn := sup.conn
	sup.mu.RUnlock()

	// Simulate a stale disconnect event with generation (activeGen - 1)
	sup.disconnect(activeGen-1, io.EOF)

	// Verify that active connection and state remain unaffected!
	sup.mu.RLock()
	newGen := sup.generation
	newConn := sup.conn
	state := sup.state
	sup.mu.RUnlock()

	if newGen != activeGen {
		t.Fatalf("generation changed unexpectedly: got %d, expected %d", newGen, activeGen)
	}
	if newConn != activeConn || newConn == nil {
		t.Fatal("active connection was erroneously cleared by stale disconnect!")
	}
	if state != PeerStateConnected {
		t.Fatalf("peer state became %v, expected Connected", state)
	}
}

func TestPeerConnectionManager_OneFailingPeerDoesNotBlockHealthyPeer(t *testing.T) {
	lnHealthy := createTestListener(t)
	defer lnHealthy.Close()

	go func() {
		for {
			c, err := lnHealthy.Accept()
			if err != nil {
				return
			}
			_ = c
		}
	}()

	// Peer 2 is healthy, Peer 3 is permanently unreachable (port that rejects immediately)
	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	deadAddr := deadLn.Addr().String()
	deadLn.Close() // closed immediately so dialing will fail

	topo := createTestTopology(t, 1, "127.0.0.1:9001", map[cluster.NodeID]string{
		2: lnHealthy.Addr().String(),
		3: deadAddr,
	})

	cfg := DefaultPeerConnectionConfig()
	cfg.DialTimeout = 500 * time.Millisecond
	cfg.ReconnectMin = 20 * time.Millisecond
	cfg.ReconnectMax = 100 * time.Millisecond

	mgr, err := NewPeerConnectionManager(topo, cfg)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	defer mgr.Close()

	if err := mgr.Start(); err != nil {
		t.Fatalf("failed to start: %v", err)
	}

	// Peer 2 must become connected despite Peer 3 failing
	for i := 0; i < 100; i++ {
		if mgr.IsConnected(2) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !mgr.IsConnected(2) {
		t.Fatal("healthy peer 2 failed to connect while peer 3 was failing")
	}

	// Peer 3 must not be connected
	if mgr.IsConnected(3) {
		t.Fatal("dead peer 3 should not be connected")
	}

	// Can send to healthy peer 2 successfully
	req := &RequestVoteRequest{Term: 1, CandidateID: 1, Nonce: 1}
	f, _ := EncodeRequestVote(req, 1)
	if err := mgr.Send(context.Background(), 2, f); err != nil {
		t.Fatalf("failed to send to healthy peer 2: %v", err)
	}

	// Sending to peer 3 returns PeerUnavailable
	if err := mgr.Send(context.Background(), 3, f); !errors.Is(err, errs.ErrPeerUnavailable) {
		t.Fatalf("expected ErrPeerUnavailable for peer 3, got: %v", err)
	}
}

func TestPeerConnectionManager_ReaderTeardownOnCorruptedFrame(t *testing.T) {
	ln := createTestListener(t)
	defer ln.Close()

	topo := createTestTopology(t, 1, "127.0.0.1:9001", map[cluster.NodeID]string{
		2: ln.Addr().String(),
	})

	serverConnCh := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			serverConnCh <- conn
		}
	}()

	cfg := DefaultPeerConnectionConfig()
	cfg.DialTimeout = 2 * time.Second
	cfg.ReconnectMin = 50 * time.Millisecond

	mgr, err := NewPeerConnectionManager(topo, cfg)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	defer mgr.Close()

	if err := mgr.Start(); err != nil {
		t.Fatalf("failed to start: %v", err)
	}

	var serverConn net.Conn
	select {
	case serverConn = <-serverConnCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for server conn")
	}

	for i := 0; i < 100; i++ {
		if mgr.IsConnected(2) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !mgr.IsConnected(2) {
		t.Fatal("client did not connect")
	}

	// Inject corrupted bytes from server to client: invalid magic
	corrupted := []byte{0x00, 0x00, 0x00, 0x00, 0x81, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	_, _ = serverConn.Write(corrupted)

	// Client reader must detect framing failure and tear down connection
	for i := 0; i < 100; i++ {
		if !mgr.IsConnected(2) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if mgr.IsConnected(2) {
		t.Fatal("client should have disconnected upon receiving corrupted frame")
	}
}

func TestPeerConnectionManager_ShutdownIdempotencyAndLeakSafety(t *testing.T) {
	ln := createTestListener(t)
	defer ln.Close()

	topo := createTestTopology(t, 1, "127.0.0.1:9001", map[cluster.NodeID]string{
		2: ln.Addr().String(),
	})

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c
		}
	}()

	cfg := DefaultPeerConnectionConfig()
	cfg.DialTimeout = 1 * time.Second
	cfg.ReconnectMin = 20 * time.Millisecond
	cfg.ReconnectMax = 50 * time.Millisecond

	mgr, err := NewPeerConnectionManager(topo, cfg)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	if err := mgr.Start(); err != nil {
		t.Fatalf("failed to start: %v", err)
	}

	// Wait for connected
	for i := 0; i < 100; i++ {
		if mgr.IsConnected(2) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Concurrent invocations of Close must be safe and idempotent
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := mgr.Close(); err != nil {
				t.Errorf("Close failed: %v", err)
			}
		}()
	}
	wg.Wait()

	// Post-shutdown checks:
	if mgr.IsConnected(2) {
		t.Fatal("peer must not be connected after manager shutdown")
	}

	// Send must fail closed
	frame := &Frame{Header: Header{Magic: Magic, OpCode: OpCode(PeerOpRequestVote)}}
	if err := mgr.Send(context.Background(), 2, frame); !errors.Is(err, errs.ErrManagerClosed) {
		t.Fatalf("expected ErrManagerClosed, got: %v", err)
	}

	// Start must fail closed
	if err := mgr.Start(); !errors.Is(err, errs.ErrManagerClosed) {
		t.Fatalf("expected ErrManagerClosed, got: %v", err)
	}

	// Close again must be no-op
	if err := mgr.Close(); err != nil {
		t.Fatalf("repeated Close failed: %v", err)
	}
}

func TestPeerConnectionManager_StartCloseRace(t *testing.T) {
	ln := createTestListener(t)
	defer ln.Close()

	topo := createTestTopology(t, 1, "127.0.0.1:9001", map[cluster.NodeID]string{
		2: ln.Addr().String(),
	})

	for iter := 0; iter < 20; iter++ {
		cfg := DefaultPeerConnectionConfig()
		cfg.DialTimeout = 50 * time.Millisecond
		cfg.ReconnectMin = 10 * time.Millisecond
		cfg.ReconnectMax = 20 * time.Millisecond

		mgr, err := NewPeerConnectionManager(topo, cfg)
		if err != nil {
			t.Fatalf("iter %d: failed to create manager: %v", iter, err)
		}

		var wg sync.WaitGroup
		// 10 goroutines calling Start()
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = mgr.Start()
			}()
		}
		// 10 goroutines calling Close()
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = mgr.Close()
			}()
		}
		wg.Wait()

		// Final check: Close must be completed
		_ = mgr.Close()
		if mgr.IsConnected(2) {
			t.Fatalf("iter %d: peer connected after close", iter)
		}
		if err := mgr.Start(); !errors.Is(err, errs.ErrManagerClosed) {
			t.Fatalf("iter %d: expected ErrManagerClosed, got %v", iter, err)
		}
	}
}

func TestPeerConnectionManager_SendSharedFrameConcurrentNoRace(t *testing.T) {
	ln2 := createTestListener(t)
	defer ln2.Close()
	ln3 := createTestListener(t)
	defer ln3.Close()

	acceptAndDrain := func(ln net.Listener) {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(conn)
		}
	}
	go acceptAndDrain(ln2)
	go acceptAndDrain(ln3)

	topo := createTestTopology(t, 1, "127.0.0.1:9001", map[cluster.NodeID]string{
		2: ln2.Addr().String(),
		3: ln3.Addr().String(),
	})

	cfg := DefaultPeerConnectionConfig()
	cfg.DialTimeout = 1 * time.Second
	mgr, err := NewPeerConnectionManager(topo, cfg)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	defer mgr.Close()

	if err := mgr.Start(); err != nil {
		t.Fatalf("failed to start manager: %v", err)
	}

	// Await connections
	for i := 0; i < 100; i++ {
		if mgr.IsConnected(2) && mgr.IsConnected(3) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !mgr.IsConnected(2) || !mgr.IsConnected(3) {
		t.Fatal("failed to establish connections to both peers")
	}

	// Construct a single shared frame pointer
	req := &RequestVoteRequest{Term: 5, CandidateID: 1, Nonce: 12345}
	sharedFrame, err := EncodeRequestVote(req, 100)
	if err != nil {
		t.Fatalf("EncodeRequestVote failed: %v", err)
	}

	// Concurrently broadcast sharedFrame to both peer 2 and peer 3 across 50 goroutines
	const workers = 50
	var wg sync.WaitGroup
	wg.Add(workers)
	ctx := context.Background()

	for i := 0; i < workers; i++ {
		peerID := cluster.NodeID(2 + (i % 2))
		go func(pid cluster.NodeID) {
			defer wg.Done()
			if err := mgr.Send(ctx, pid, sharedFrame); err != nil {
				t.Errorf("Send to peer %d failed: %v", pid, err)
			}
		}(peerID)
	}
	wg.Wait()
}

func TestPeerConnectionManager_InsecureTransportPolicy(t *testing.T) {
	topo := createTestTopology(t, 1, "127.0.0.1:9001", map[cluster.NodeID]string{
		2: "192.168.1.100:9098",
	})

	// 1. InsecureTransport = false (default) with default dialer must fail closed
	cfg := DefaultPeerConnectionConfig()
	cfg.InsecureTransport = false
	cfg.DialFunc = nil

	_, err := NewPeerConnectionManager(topo, cfg)
	if !errors.Is(err, errs.ErrInsecureTransport) {
		t.Fatalf("expected ErrInsecureTransport for remote peer, got: %v", err)
	}

	// 2. Finding B: InsecureTransport = true on non-loopback peer MUST STILL fail closed
	cfg.InsecureTransport = true
	_, err = NewPeerConnectionManager(topo, cfg)
	if !errors.Is(err, errs.ErrInsecureTransport) {
		t.Fatalf("expected ErrInsecureTransport for non-loopback peer even with InsecureTransport=true, got: %v", err)
	}

	// 3. Loopback peer topology succeeds with plaintext TCP for local development/testing
	loopbackTopo := createTestTopology(t, 1, "127.0.0.1:9001", map[cluster.NodeID]string{
		2: "127.0.0.1:9098",
	})
	mgr, err := NewPeerConnectionManager(loopbackTopo, cfg)
	if err != nil {
		t.Fatalf("expected success for loopback peer with InsecureTransport=true, got: %v", err)
	}
	_ = mgr.Close()
}

func TestPeerConnectionManager_NilConnDialer(t *testing.T) {
	topo := createTestTopology(t, 1, "127.0.0.1:9001", map[cluster.NodeID]string{
		2: "127.0.0.1:9098",
	})

	cfg := DefaultPeerConnectionConfig()
	cfg.DialTimeout = 50 * time.Millisecond
	cfg.ReconnectMin = 10 * time.Millisecond
	cfg.ReconnectMax = 20 * time.Millisecond
	// Malicious dialer returns (nil, nil)
	cfg.DialFunc = func(ctx context.Context, addr string) (net.Conn, error) {
		return nil, nil
	}

	mgr, err := NewPeerConnectionManager(topo, cfg)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	defer mgr.Close()

	if err := mgr.Start(); err != nil {
		t.Fatalf("failed to start manager: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if mgr.IsConnected(2) {
		t.Fatal("peer should not be connected when dialer returns nil conn")
	}
}

func TestPeerConnectionManager_BackoffOverflowSafety(t *testing.T) {
	cfg := DefaultPeerConnectionConfig()
	cfg.ReconnectMin = 50 * time.Millisecond
	cfg.ReconnectMax = 5 * time.Second

	sup := &peerSupervisor{cfg: cfg}

	testFailures := []int{
		0, 1, 2, 5, 10, 20, 30, 31, 32, 62, 63, 64, 100, 1000, math.MaxInt32, math.MaxInt,
	}

	for _, f := range testFailures {
		delay := sup.calculateBackoff(f)
		if delay <= 0 {
			t.Errorf("calculateBackoff(%d) produced non-positive delay: %v", f, delay)
		}
		if delay > cfg.ReconnectMax {
			t.Errorf("calculateBackoff(%d) exceeded ReconnectMax: got %v, max %v", f, delay, cfg.ReconnectMax)
		}
	}
}

func TestPeerConnectionManager_ReplayFilterDirect(t *testing.T) {
	rf := newPeerReplayFilter(2)

	// 1. Valid first frame with SeqID 1
	f1 := &Frame{Header: Header{SeqID: 1}}
	if err := rf.CheckAndRecord(f1); err != nil {
		t.Fatalf("first frame rejected: %v", err)
	}

	// 2. Exact same SeqID must be rejected as duplicate
	if err := rf.CheckAndRecord(f1); !errors.Is(err, errs.ErrReplayedFrame) {
		t.Fatalf("expected ErrReplayedFrame for duplicate seqID, got: %v", err)
	}

	// 3. Sequential frames must succeed
	for s := uint64(2); s <= 100; s++ {
		f := &Frame{Header: Header{SeqID: s}}
		if err := rf.CheckAndRecord(f); err != nil {
			t.Fatalf("seq %d rejected: %v", s, err)
		}
	}

	// 4. Stale frame far behind window (> 4096) must be rejected
	rf.maxSeqID = 10000
	rf.seenSeqs[10000] = struct{}{}
	staleFrame := &Frame{Header: Header{SeqID: 10000 - DefaultReplayWindowSize - 1}}
	if err := rf.CheckAndRecord(staleFrame); !errors.Is(err, errs.ErrReplayedFrame) {
		t.Fatalf("expected ErrReplayedFrame for stale seqID, got: %v", err)
	}

	// 5. Nonce duplicate detection on RequestVote
	req1 := &RequestVoteRequest{Term: 1, CandidateID: 1, Nonce: 0xCAFEBABE}
	rv1, _ := EncodeRequestVote(req1, 20000)
	if err := rf.CheckAndRecord(rv1); err != nil {
		t.Fatalf("rv1 rejected: %v", err)
	}

	// Replay rv1 with different SeqID but SAME Nonce -> must be rejected
	rv1Replay, _ := EncodeRequestVote(req1, 20001)
	if err := rf.CheckAndRecord(rv1Replay); !errors.Is(err, errs.ErrReplayedFrame) {
		t.Fatalf("expected ErrReplayedFrame for duplicate nonce, got: %v", err)
	}

	// 6. Memory bounding: insert 10,000 nonces and verify map does not grow unboundedly
	for i := 0; i < 10000; i++ {
		req := &RequestVoteRequest{Term: 1, CandidateID: 1, Nonce: uint64(1000000 + i)}
		f, _ := EncodeRequestVote(req, uint64(30000+i))
		_ = rf.CheckAndRecord(f)
	}
	rf.mu.Lock()
	nonceCount := len(rf.nonceSet)
	seqCount := len(rf.seenSeqs)
	rf.mu.Unlock()

	if nonceCount > DefaultMaxNoncesTracked {
		t.Fatalf("nonceSet unbounded: got %d, max %d", nonceCount, DefaultMaxNoncesTracked)
	}
	if seqCount > int(DefaultReplayWindowSize)+1 {
		t.Fatalf("seenSeqs unbounded: got %d, max %d", seqCount, DefaultReplayWindowSize)
	}
}

func TestPeerConnectionManager_ReplayDropIntegration(t *testing.T) {
	ln := createTestListener(t)
	defer ln.Close()

	topo := createTestTopology(t, 1, "127.0.0.1:9001", map[cluster.NodeID]string{
		2: ln.Addr().String(),
	})

	serverConnCh := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			serverConnCh <- conn
		}
	}()

	var receivedCount atomic.Int32
	cfg := DefaultPeerConnectionConfig()
	cfg.DialTimeout = 2 * time.Second
	cfg.OnFrameReceived = func(peerID cluster.NodeID, frame *Frame) {
		receivedCount.Add(1)
	}

	mgr, err := NewPeerConnectionManager(topo, cfg)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	defer mgr.Close()

	if err := mgr.Start(); err != nil {
		t.Fatalf("failed to start manager: %v", err)
	}

	var serverConn net.Conn
	select {
	case serverConn = <-serverConnCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for server conn")
	}

	for i := 0; i < 100; i++ {
		if mgr.IsConnected(2) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !mgr.IsConnected(2) {
		t.Fatal("client did not connect")
	}

	// 1. Send first legitimate frame
	req := &RequestVoteRequest{Term: 1, CandidateID: 2, Nonce: 88888}
	f, err := EncodeRequestVote(req, 100)
	if err != nil {
		t.Fatalf("EncodeRequestVote failed: %v", err)
	}
	if err := EncodeFrame(serverConn, f); err != nil {
		t.Fatalf("failed to send frame from server: %v", err)
	}

	// Await delivery
	for i := 0; i < 100; i++ {
		if receivedCount.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if receivedCount.Load() != 1 {
		t.Fatalf("expected 1 frame delivered, got %d", receivedCount.Load())
	}

	// 2. Re-send the EXACT SAME frame (replay attack)
	if err := EncodeFrame(serverConn, f); err != nil {
		t.Fatalf("failed to re-send frame: %v", err)
	}

	// Allow reader to process
	time.Sleep(100 * time.Millisecond)

	// Replayed frame must have been dropped by replayFilter; receivedCount MUST still be 1!
	if receivedCount.Load() != 1 {
		t.Fatalf("replayed frame was erroneously delivered to OnFrameReceived! count = %d", receivedCount.Load())
	}
}

func BenchmarkPeerConnectionManager_Send(b *testing.B) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()

	topo, err := cluster.NewTopology(1, "127.0.0.1:9001", []cluster.PeerConfig{
		{ID: 1, Address: "127.0.0.1:9001"},
		{ID: 2, Address: ln.Addr().String()},
	})
	if err != nil {
		b.Fatalf("failed to create topology: %v", err)
	}

	go func() {
		conn, aErr := ln.Accept()
		if aErr != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 64*1024)
		for {
			_, rErr := conn.Read(buf)
			if rErr != nil {
				return
			}
		}
	}()

	cfg := DefaultPeerConnectionConfig()
	mgr, err := NewPeerConnectionManager(topo, cfg)
	if err != nil {
		b.Fatalf("failed to create manager: %v", err)
	}
	defer mgr.Close()

	if err := mgr.Start(); err != nil {
		b.Fatalf("failed to start manager: %v", err)
	}

	for i := 0; i < 100; i++ {
		if mgr.IsConnected(2) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !mgr.IsConnected(2) {
		b.Fatal("not connected")
	}

	req := &RequestVoteRequest{Term: 1, CandidateID: 1, Nonce: 1}
	frame, err := EncodeRequestVote(req, 1)
	if err != nil {
		b.Fatalf("failed to encode: %v", err)
	}

	ctx := context.Background()
	b.ResetTimer()

	b.Run("SequentialSend", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if err := mgr.Send(ctx, 2, frame); err != nil {
				b.Fatalf("send failed: %v", err)
			}
		}
	})

	b.Run("ParallelSend", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if err := mgr.Send(ctx, 2, frame); err != nil {
					b.Errorf("send failed: %v", err)
				}
			}
		})
	})
}

func TestPeerConnectionManager_ResurrectionDefense(t *testing.T) {
	topo := createTestTopology(t, 1, "127.0.0.1:9001", map[cluster.NodeID]string{
		2: "127.0.0.1:9002",
	})

	dialBlocked := make(chan struct{})
	dialFinished := make(chan struct{})
	var dialedConn net.Conn

	cfg := DefaultPeerConnectionConfig()
	cfg.DialFunc = func(ctx context.Context, addr string) (net.Conn, error) {
		<-dialBlocked
		c1, c2 := net.Pipe()
		go func() {
			// drain c2
			io.Copy(io.Discard, c2)
		}()
		dialedConn = c1
		close(dialFinished)
		return c1, nil
	}

	mgr, err := NewPeerConnectionManager(topo, cfg)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	if err := mgr.Start(); err != nil {
		t.Fatalf("failed to start: %v", err)
	}

	// Close manager while dial is blocked
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = mgr.Close()
	}()

	time.Sleep(40 * time.Millisecond)
	close(dialBlocked) // unblock dial after Close initiated
	<-dialFinished

	// Wait for Close to finish
	time.Sleep(50 * time.Millisecond)

	state, err := mgr.GetPeerState(2)
	if !errors.Is(err, errs.ErrManagerClosed) {
		t.Fatalf("expected ErrManagerClosed, got err=%v, state=%v", err, state)
	}

	// Verify that dialed socket was closed and not left active/leaked
	if dialedConn != nil {
		var b [1]byte
		_, rErr := dialedConn.Read(b[:])
		if rErr == nil {
			t.Fatal("resurrected connection was not closed upon shutdown")
		}
	}
}

func TestPeerConnectionManager_InvalidOpCodeTeardown(t *testing.T) {
	serverPipe, clientPipe := net.Pipe()
	defer serverPipe.Close()

	topo := createTestTopology(t, 1, "127.0.0.1:9001", map[cluster.NodeID]string{
		2: "127.0.0.1:9002",
	})

	var receivedFrame atomic.Pointer[Frame]
	cfg := DefaultPeerConnectionConfig()
	cfg.DialFunc = func(ctx context.Context, addr string) (net.Conn, error) {
		return clientPipe, nil
	}
	cfg.OnFrameReceived = func(peerID cluster.NodeID, frame *Frame) {
		receivedFrame.Store(frame)
	}

	mgr, err := NewPeerConnectionManager(topo, cfg)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	defer mgr.Close()

	if err := mgr.Start(); err != nil {
		t.Fatalf("failed to start: %v", err)
	}

	// Await connected
	for i := 0; i < 50; i++ {
		if mgr.IsConnected(2) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Send an invalid client OpCode (OpPut = 0x01) over the peer connection from the server side
	clientFrame := &Frame{
		Header: Header{
			Magic:         Magic,
			OpCode:        OpPut,
			Flags:         FlagNone,
			SeqID:         1,
			PayloadLength: 0,
		},
		Payload: nil,
	}
	if err := EncodeFrame(serverPipe, clientFrame); err != nil {
		t.Fatalf("failed to send frame: %v", err)
	}

	// Verify manager detects invalid opcode and tears down connection
	for i := 0; i < 50; i++ {
		if !mgr.IsConnected(2) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if mgr.IsConnected(2) {
		t.Fatal("expected peer connection to be torn down upon invalid opcode")
	}

	if receivedFrame.Load() != nil {
		t.Fatalf("OnFrameReceived must not be invoked for invalid opcode: got %+v", receivedFrame.Load())
	}
}

func TestPeerConnectionManager_ReplayFilter_ReconnectSafe(t *testing.T) {
	rf := newPeerReplayFilter(2)

	// Send frames with SeqID 1..5 on first connection
	for seq := uint64(1); seq <= 5; seq++ {
		f := &Frame{
			Header: Header{
				Magic:  Magic,
				OpCode: OpCode(PeerOpRequestVote),
				Flags:  FlagNone,
				SeqID:  seq,
			},
		}
		if err := rf.CheckAndRecord(f); err != nil {
			t.Fatalf("seq %d rejected on conn 1: %v", seq, err)
		}
	}

	// In-connection duplicate must fail
	dupFrame := &Frame{
		Header: Header{
			Magic:  Magic,
			OpCode: OpCode(PeerOpRequestVote),
			Flags:  FlagNone,
			SeqID:  3,
		},
	}
	if err := rf.CheckAndRecord(dupFrame); err == nil {
		t.Fatal("expected duplicate sequence error, got nil")
	}

	// Reconnect resets sequence tracking
	rf.ResetSequence()

	// Restarted remote node starts again from SeqID 1
	for seq := uint64(1); seq <= 5; seq++ {
		f := &Frame{
			Header: Header{
				Magic:  Magic,
				OpCode: OpCode(PeerOpRequestVote),
				Flags:  FlagNone,
				SeqID:  seq,
			},
		}
		if err := rf.CheckAndRecord(f); err != nil {
			t.Fatalf("seq %d rejected on reconnected conn: %v", seq, err)
		}
	}
}

func TestPeerReplayFilter_ZeroSeqIDRejection(t *testing.T) {
	rf := newPeerReplayFilter(2)
	f := &Frame{
		Header: Header{
			Magic:  Magic,
			OpCode: OpCode(PeerOpRequestVote),
			Flags:  FlagNone,
			SeqID:  0,
		},
	}
	err := rf.CheckAndRecord(f)
	if err == nil {
		t.Fatal("expected error on SeqID=0, got nil")
	}
	if !errors.Is(err, errs.ErrReplayedFrame) {
		t.Fatalf("expected ErrReplayedFrame, got %v", err)
	}
}

func TestPeerConnectionManager_InboundConnectionLimit(t *testing.T) {
	ln := createTestListener(t)
	defer ln.Close()

	topo := createTestTopology(t, 1, ln.Addr().String(), nil)
	cfg := DefaultPeerConnectionConfig()
	cfg.DialTimeout = 5 * time.Second

	mgr, err := NewPeerConnectionManager(topo, cfg)
	if err != nil {
		t.Fatalf("failed to construct manager: %v", err)
	}
	defer mgr.Close()

	if err := mgr.ServeListener(ln); err != nil {
		t.Fatalf("failed to serve listener: %v", err)
	}

	// Dial MaxInboundPeerConnections (64)
	conns := make([]net.Conn, MaxInboundPeerConnections)
	for i := 0; i < MaxInboundPeerConnections; i++ {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("failed to dial conn %d: %v", i, err)
		}
		conns[i] = c
	}

	// Wait for all 64 to be active
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.ActiveInboundConnections() == int64(MaxInboundPeerConnections) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := mgr.ActiveInboundConnections(); got != int64(MaxInboundPeerConnections) {
		t.Fatalf("expected %d active inbound conns, got %d", MaxInboundPeerConnections, got)
	}

	// Dial the 65th connection - should be closed immediately at ceiling
	extraConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		// Connection refused or closed immediately is fine
		return
	}
	defer extraConn.Close()

	// Probing read on extraConn should yield EOF or closed error
	extraConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	var buf [1]byte
	n, readErr := extraConn.Read(buf[:])
	if readErr == nil && n > 0 {
		t.Fatalf("expected 65th connection to be rejected and closed, but read %d bytes", n)
	}

	if got := mgr.ActiveInboundConnections(); got > int64(MaxInboundPeerConnections) {
		t.Fatalf("active inbound connections %d exceeded ceiling of %d", got, MaxInboundPeerConnections)
	}

	// Close all connections
	for _, c := range conns {
		if c != nil {
			_ = c.Close()
		}
	}

	// Verify all slots freed
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.ActiveInboundConnections() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := mgr.ActiveInboundConnections(); got != 0 {
		t.Errorf("expected 0 active inbound conns after closing all, got %d", got)
	}
}

func TestPeerConnectionManager_RejectOneWayTLS_NonLoopback(t *testing.T) {
	topo := createTestTopology(t, 1, "127.0.0.1:9001", map[cluster.NodeID]string{
		2: "192.168.1.100:9098",
	})

	ca := NewTestCA(t, "Reject One-Way Integration CA")
	node1Cert, node1Key := ca.IssuePeerCert(t, 1)
	kp, err := tls.LoadX509KeyPair(node1Cert, node1Key)
	if err != nil {
		t.Fatalf("LoadX509KeyPair failed: %v", err)
	}
	caPool, err := LoadCertPool(ca.CertPath)
	if err != nil {
		t.Fatalf("LoadCertPool failed: %v", err)
	}

	// 1. Deliberately constructed one-way TLS 1.3 configuration on TLSConfig
	cfg := DefaultPeerConnectionConfig()
	cfg.TLSConfig = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		ClientAuth:   tls.NoClientCert, // one-way TLS
		Certificates: []tls.Certificate{kp},
		ClientCAs:    caPool,
	}
	_, err = NewPeerConnectionManager(topo, cfg)
	if !errors.Is(err, errs.ErrInsecureTransport) {
		t.Fatalf("expected ErrInsecureTransport for one-way TLS on TLSConfig, got: %v", err)
	}

	// 2. Deliberately constructed one-way TLS 1.3 configuration on ListenerTLSConfig
	cfg = DefaultPeerConnectionConfig()
	cfg.ListenerTLSConfig = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		ClientAuth:   tls.NoClientCert, // one-way TLS
		Certificates: []tls.Certificate{kp},
		ClientCAs:    caPool,
	}
	_, err = NewPeerConnectionManager(topo, cfg)
	if !errors.Is(err, errs.ErrInsecureTransport) {
		t.Fatalf("expected ErrInsecureTransport for one-way TLS on ListenerTLSConfig, got: %v", err)
	}
}
