package raft_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cluster"
	latticeErrors "github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/raft"
	"github.com/silent-knight19/lattice/internal/transport"
)

// -----------------------------------------------------------------------------
// Finding D: Test 1 — Standalone Mode GET over client TCP socket
// -----------------------------------------------------------------------------
func TestClientTCP_StandaloneMode_DirectGet(t *testing.T) {
	eng := newControllableEngineSM()
	_ = eng.Put(context.Background(), []byte("standalone_key"), []byte("standalone_val"))

	cfg := transport.DefaultServerConfig()
	cfg.ClusterMode = false

	srv, err := transport.NewServer(cfg, eng)
	if err != nil {
		t.Fatalf("failed to create standalone server: %v", err)
	}
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("failed to start standalone listener: %v", err)
	}
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("failed to dial standalone server: %v", err)
	}
	defer conn.Close()

	req := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  1001,
		Key:    []byte("standalone_key"),
	}
	if err := transport.WriteRequest(conn, req); err != nil {
		t.Fatalf("failed to write request: %v", err)
	}
	resp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	if resp.SeqID != 1001 {
		t.Fatalf("expected SeqID 1001, got %d", resp.SeqID)
	}
	if resp.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk, got %s (%s)", resp.Status, resp.Message)
	}
	if !bytes.Equal(resp.Value, []byte("standalone_val")) {
		t.Fatalf("expected 'standalone_val', got '%s'", string(resp.Value))
	}
}

// -----------------------------------------------------------------------------
// Finding D: Test 2 — Cluster Mode Leader GET over client TCP socket
// -----------------------------------------------------------------------------
func TestClientTCP_ClusterMode_LeaderGet_Succeeds(t *testing.T) {
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

	// Commit key "leader_key" = "leader_val"
	ctx := context.Background()
	putReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  1,
		Key:    []byte("leader_key"),
		Value:  []byte("leader_val"),
	}
	writeResp, err := node1.router.RouteWrite(ctx, putReq)
	if err != nil || writeResp.Status != transport.StatusOk {
		t.Fatalf("RouteWrite failed: %v, resp: %+v", err, writeResp)
	}
	syncFollowersFromLeader(t, node1, node2, node3)
	lastIdx, _ := node1.storage.LastIndex()
	_ = node1.node.HandleAppendEntriesResponse(2, &transport.AppendEntriesResponse{Term: 1, Success: true, MatchIndex: uint64(lastIdx)})

	for i := 0; i < 50; i++ {
		if node1.node.LastApplied() >= lastIdx {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Client sends GET to leader's transport.Server over TCP
	conn, err := net.Dial("tcp", node1.server.Addr().String())
	if err != nil {
		t.Fatalf("failed to dial leader: %v", err)
	}
	defer conn.Close()

	getReq := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  1002,
		Key:    []byte("leader_key"),
	}
	if err := transport.WriteRequest(conn, getReq); err != nil {
		t.Fatalf("failed to write request: %v", err)
	}
	resp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	if resp.SeqID != 1002 {
		t.Fatalf("expected SeqID 1002, got %d", resp.SeqID)
	}
	if resp.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk, got %s (%s)", resp.Status, resp.Message)
	}
	if !bytes.Equal(resp.Value, []byte("leader_val")) {
		t.Fatalf("expected 'leader_val', got '%s'", string(resp.Value))
	}
}

// -----------------------------------------------------------------------------
// Finding D: Test 3 — Cluster Mode Follower Redirect over client TCP socket
// -----------------------------------------------------------------------------
func TestClientTCP_ClusterMode_FollowerRedirect_NoStaleRead(t *testing.T) {
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

	// Inject stale data directly into follower's local engine
	_ = node2.sm.Put(context.Background(), []byte("stale_key"), []byte("stale_follower_val"))

	// Inform follower 2 that node 1 is leader
	_, _ = node2.node.HandleAppendEntries(1, &transport.AppendEntriesRequest{
		Term:     1,
		LeaderID: 1,
	})

	// Client sends GET to follower's transport.Server over TCP
	conn, err := net.Dial("tcp", node2.server.Addr().String())
	if err != nil {
		t.Fatalf("failed to dial follower: %v", err)
	}
	defer conn.Close()

	getReq := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  1003,
		Key:    []byte("stale_key"),
	}
	if err := transport.WriteRequest(conn, getReq); err != nil {
		t.Fatalf("failed to write request: %v", err)
	}
	resp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	if resp.SeqID != 1003 {
		t.Fatalf("expected SeqID 1003, got %d", resp.SeqID)
	}
	if resp.Status != transport.StatusNotLeader {
		t.Fatalf("expected StatusNotLeader, got: %s", resp.Status)
	}
	if resp.LeaderID != 1 {
		t.Fatalf("expected LeaderID 1, got %d", resp.LeaderID)
	}
	if resp.LeaderAddr != node1.addr {
		t.Fatalf("expected LeaderAddr %s, got %s", node1.addr, resp.LeaderAddr)
	}
	if len(resp.Value) > 0 {
		t.Fatalf("follower returned data: '%s'", string(resp.Value))
	}
}

// -----------------------------------------------------------------------------
// Finding D: Test 4 — Cluster Mode Missing Router Fails Closed over client TCP
// -----------------------------------------------------------------------------
func TestClientTCP_ClusterMode_MissingRouter_FailsClosed(t *testing.T) {
	eng := newControllableEngineSM()
	_ = eng.Put(context.Background(), []byte("secret_key"), []byte("secret_val"))

	cfg := transport.DefaultServerConfig()
	cfg.ClusterMode = true
	cfg.ReadRouter = nil

	srv, err := transport.NewServer(cfg, eng)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("failed to dial server: %v", err)
	}
	defer conn.Close()

	getReq := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  1004,
		Key:    []byte("secret_key"),
	}
	if err := transport.WriteRequest(conn, getReq); err != nil {
		t.Fatalf("failed to write request: %v", err)
	}
	resp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	if resp.SeqID != 1004 {
		t.Fatalf("expected SeqID 1004, got %d", resp.SeqID)
	}
	if resp.Status != transport.StatusError {
		t.Fatalf("expected StatusError, got: %s", resp.Status)
	}
	if len(resp.Value) > 0 {
		t.Fatalf("server exposed local engine value: '%s'", string(resp.Value))
	}
}

// -----------------------------------------------------------------------------
// Finding D: Test 5 — Committed-not-applied waits for barrier over client TCP
// -----------------------------------------------------------------------------
func TestClientTCP_ClusterMode_CommittedNotApplied_WaitsForBarrier(t *testing.T) {
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

	for i := 0; i < 50; i++ {
		if node1.node.LastApplied() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	node1.sm.SetPauseKey("barrier_key")

	// Propose a write
	putReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  1,
		Key:    []byte("barrier_key"),
		Value:  []byte("barrier_val"),
	}
	_, _ = node1.router.RouteWrite(context.Background(), putReq)
	syncFollowersFromLeader(t, node1, node2, node3)
	lastIdx, _ := node1.storage.LastIndex()
	_ = node1.node.HandleAppendEntriesResponse(2, &transport.AppendEntriesResponse{Term: 1, Success: true, MatchIndex: uint64(lastIdx)})

	<-node1.sm.applyEntered

	// Client sends GET over TCP
	conn, err := net.Dial("tcp", node1.server.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	getReq := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  1005,
		Key:    []byte("barrier_key"),
	}

	type result struct {
		resp *transport.Response
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		_ = transport.WriteRequest(conn, getReq)
		r, e := transport.ReadResponse(conn)
		resCh <- result{resp: r, err: e}
	}()

	// Verify blocked on barrier
	select {
	case res := <-resCh:
		t.Fatalf("GET returned before barrier completed: %+v, err: %v", res.resp, res.err)
	case <-time.After(150 * time.Millisecond):
	}

	// Release apply
	node1.sm.applyRelease <- struct{}{}

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("read failed: %v", res.err)
		}
		if res.resp.SeqID != 1005 {
			t.Fatalf("expected SeqID 1005, got %d", res.resp.SeqID)
		}
		if res.resp.Status != transport.StatusOk {
			t.Fatalf("expected StatusOk, got %s (%s)", res.resp.Status, res.resp.Message)
		}
		if !bytes.Equal(res.resp.Value, []byte("barrier_val")) {
			t.Fatalf("expected 'barrier_val', got '%s'", string(res.resp.Value))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for GET after barrier release")
	}
}

// -----------------------------------------------------------------------------
// Finding D: Test 6 — DELETE followed by GET observes deletion over client TCP
// -----------------------------------------------------------------------------
func TestClientTCP_ClusterMode_DeleteThenLinearizableGet(t *testing.T) {
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
		Key:    []byte("del_key"),
		Value:  []byte("del_val"),
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

	// Delete key with pause
	node1.sm.SetPauseKey("del_key")
	delReq := &transport.Request{
		OpCode: transport.OpDelete,
		SeqID:  2,
		Key:    []byte("del_key"),
	}
	_, _ = node1.router.RouteWrite(ctx, delReq)
	syncFollowersFromLeader(t, node1, node2, node3)
	delIdx, _ := node1.storage.LastIndex()
	_ = node1.node.HandleAppendEntriesResponse(2, &transport.AppendEntriesResponse{Term: 1, Success: true, MatchIndex: uint64(delIdx)})

	<-node1.sm.applyEntered

	// Client sends GET over TCP
	conn, err := net.Dial("tcp", node1.server.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	getReq := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  1006,
		Key:    []byte("del_key"),
	}
	resCh := make(chan *transport.Response, 1)
	go func() {
		_ = transport.WriteRequest(conn, getReq)
		r, _ := transport.ReadResponse(conn)
		resCh <- r
	}()

	select {
	case <-resCh:
		t.Fatal("GET returned before delete barrier released")
	case <-time.After(100 * time.Millisecond):
	}

	// Release apply
	node1.sm.applyRelease <- struct{}{}

	select {
	case resp := <-resCh:
		if resp.SeqID != 1006 {
			t.Fatalf("expected SeqID 1006, got %d", resp.SeqID)
		}
		if resp.Status != transport.StatusKeyNotFound {
			t.Fatalf("expected StatusKeyNotFound, got %s", resp.Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for GET after delete")
	}
}

// -----------------------------------------------------------------------------
// Finding D: Test 7 — Stepdown during wait redirects over client TCP
// -----------------------------------------------------------------------------
func TestClientTCP_ClusterMode_StepdownDuringWait_Redirects(t *testing.T) {
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

	_ = node1.sm.Put(context.Background(), []byte("step_key"), []byte("stale_val"))
	node1.sm.SetPauseKey("step_key")

	ctx := context.Background()
	putReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  1,
		Key:    []byte("step_key"),
		Value:  []byte("new_val"),
	}
	_, _ = node1.router.RouteWrite(ctx, putReq)
	syncFollowersFromLeader(t, node1, node2, node3)
	lastIdx, _ := node1.storage.LastIndex()
	_ = node1.node.HandleAppendEntriesResponse(2, &transport.AppendEntriesResponse{Term: 1, Success: true, MatchIndex: uint64(lastIdx)})

	<-node1.sm.applyEntered

	// Client sends GET over TCP
	conn, err := net.Dial("tcp", node1.server.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	getReq := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  1007,
		Key:    []byte("step_key"),
	}
	resCh := make(chan *transport.Response, 1)
	go func() {
		_ = transport.WriteRequest(conn, getReq)
		r, _ := transport.ReadResponse(conn)
		resCh <- r
	}()

	time.Sleep(100 * time.Millisecond)

	// Node 2 becomes leader in term 2
	_ = node1.storage.SetTerm(2)
	_, _ = node1.node.HandleAppendEntries(2, &transport.AppendEntriesRequest{
		Term:     2,
		LeaderID: 2,
	})

	node1.sm.applyRelease <- struct{}{}

	select {
	case resp := <-resCh:
		if resp.SeqID != 1007 {
			t.Fatalf("expected SeqID 1007, got %d", resp.SeqID)
		}
		if resp.Status != transport.StatusNotLeader {
			t.Fatalf("expected StatusNotLeader, got %s (%s)", resp.Status, resp.Message)
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
// Section 8: Physical TCP Disconnect — Established peer connections lost
// -----------------------------------------------------------------------------
func TestClientTCP_ClusterMode_PhysicalDisconnect_FailsClosed(t *testing.T) {
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

	// Pre-populate stale value in leader's local engine
	_ = node1.sm.Put(context.Background(), []byte("phys_key"), []byte("stale_local_data"))

	// Physically sever TCP peer connections: close followers' peer managers and listeners
	_ = node2.mgr.Close()
	_ = node2.ln.Close()
	_ = node3.mgr.Close()
	_ = node3.ln.Close()

	// Client sends GET to leader's transport.Server over TCP
	conn, err := net.Dial("tcp", node1.server.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	getReq := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  1008,
		Key:    []byte("phys_key"),
	}
	_ = transport.WriteRequest(conn, getReq)

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	if resp.SeqID != 1008 {
		t.Fatalf("expected SeqID 1008, got %d", resp.SeqID)
	}
	if resp.Status == transport.StatusOk {
		t.Fatalf("CRITICAL: Leader served stale data after physical peer disconnect! Value: %s", string(resp.Value))
	}
	if resp.Status != transport.StatusThrottled && resp.Status != transport.StatusNotLeader {
		t.Fatalf("expected StatusThrottled or StatusNotLeader, got %s (%s)", resp.Status, resp.Message)
	}
}

// -----------------------------------------------------------------------------
// Finding A: Saturated ReadIndex capacity fails fast deterministically
// -----------------------------------------------------------------------------
func TestReadIndex_Saturation_BoundedCapacity(t *testing.T) {
	st, err := raft.OpenStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	top, err := cluster.NewTopology(1, "127.0.0.1:9001", []cluster.PeerConfig{
		{ID: 1, Address: "127.0.0.1:9001"},
		{ID: 2, Address: "127.0.0.1:9002"},
		{ID: 3, Address: "127.0.0.1:9003"},
	})
	if err != nil {
		t.Fatal(err)
	}

	mockSender := &mockBlockSender{}

	cfg := raft.NodeConfig{
		LocalID:             1,
		Storage:             st,
		Topology:            top,
		PeerSender:          mockSender,
		MaxActiveReadRounds: 2, // Set small capacity ceiling
	}

	n, err := raft.NewNode(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()

	if err := n.BecomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatal(err)
	}

	// Launch 2 concurrent reads that block waiting for quorum
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = n.ReadIndex(ctx)
		}()
	}

	// Wait for both rounds to register
	for i := 0; i < 50; i++ {
		if n.ActiveReadRoundsCount() == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if n.ActiveReadRoundsCount() != 2 {
		t.Fatalf("expected 2 active read rounds, got %d", n.ActiveReadRoundsCount())
	}

	// 3rd read must fail fast with ErrReadIndexThrottled
	_, err = n.ReadIndex(ctx)
	if err == nil {
		t.Fatal("expected ErrReadIndexThrottled, got nil")
	}
	if !errors.Is(err, latticeErrors.ErrReadIndexThrottled) {
		t.Fatalf("expected ErrReadIndexThrottled, got %v", err)
	}

	// Cancel context to unblock active readers
	cancel()
	wg.Wait()

	// Wait for cleanup
	for i := 0; i < 50; i++ {
		if n.ActiveReadRoundsCount() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if n.ActiveReadRoundsCount() != 0 {
		t.Fatalf("expected 0 active read rounds after completion, got %d", n.ActiveReadRoundsCount())
	}
}

// -----------------------------------------------------------------------------
// Finding A: Capacity released on cancellation, stepdown, and node close
// -----------------------------------------------------------------------------
func TestReadIndex_CapacityReleasedOnCompletionAndCancel(t *testing.T) {
	st, err := raft.OpenStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	top, err := cluster.NewTopology(1, "127.0.0.1:9001", []cluster.PeerConfig{
		{ID: 1, Address: "127.0.0.1:9001"},
		{ID: 2, Address: "127.0.0.1:9002"},
	})
	if err != nil {
		t.Fatal(err)
	}

	mockSender := &mockBlockSender{}

	cfg := raft.NodeConfig{
		LocalID:             1,
		Storage:             st,
		Topology:            top,
		PeerSender:          mockSender,
		MaxActiveReadRounds: 5,
	}

	n, err := raft.NewNode(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()

	if err := n.BecomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatal(err)
	}

	// 1. Cancelled context releases capacity
	cancelCtx, cancelFn := context.WithCancel(context.Background())
	cancelErrCh := make(chan error, 1)
	go func() {
		_, err := n.ReadIndex(cancelCtx)
		cancelErrCh <- err
	}()

	// Wait until active round is registered
	for i := 0; i < 50; i++ {
		if n.ActiveReadRoundsCount() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancelFn()
	err = <-cancelErrCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}

	if n.ActiveReadRoundsCount() != 0 {
		t.Fatalf("expected 0 active rounds after cancel, got %d", n.ActiveReadRoundsCount())
	}

	// 2. Stepdown releases all capacity
	stepCtx, stepCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stepCancel()
	stepErrCh := make(chan error, 1)
	go func() {
		_, err := n.ReadIndex(stepCtx)
		stepErrCh <- err
	}()

	for i := 0; i < 50; i++ {
		if n.ActiveReadRoundsCount() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Stepdown via higher term (Term: 3)
	_, _ = n.HandleAppendEntries(2, &transport.AppendEntriesRequest{
		Term:     3,
		LeaderID: 2,
	})

	err = <-stepErrCh
	if !errors.Is(err, latticeErrors.ErrRaftInvalidRoleTransition) {
		t.Fatalf("expected ErrRaftInvalidRoleTransition, got: %v", err)
	}

	if n.ActiveReadRoundsCount() != 0 {
		t.Fatalf("expected 0 active rounds after stepdown, got %d", n.ActiveReadRoundsCount())
	}
}

// -----------------------------------------------------------------------------
// Finding B: ValidateLeadership TOCTOU test
// -----------------------------------------------------------------------------
func TestValidateLeadership_ConcurrentStepdown_TOCTOU(t *testing.T) {
	st, err := raft.OpenStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	top, err := cluster.NewTopology(1, "127.0.0.1:9001", []cluster.PeerConfig{
		{ID: 1, Address: "127.0.0.1:9001"},
	})
	if err != nil {
		t.Fatal(err)
	}

	n, err := raft.NewNode(raft.NodeConfig{
		LocalID:  1,
		Storage:  st,
		Topology: top,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()

	if err := n.BecomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatal(err)
	}

	// Valid leadership in term 1, epoch 1
	if err := n.ValidateLeadership(1, 1); err != nil {
		t.Fatalf("expected valid leadership, got: %v", err)
	}

	// Term mismatch fails
	if err := n.ValidateLeadership(2, 1); err == nil {
		t.Fatal("expected failure for term mismatch, got nil")
	}

	// Epoch mismatch fails
	if err := n.ValidateLeadership(1, 2); err == nil {
		t.Fatal("expected failure for epoch mismatch, got nil")
	}

	// Stepdown causes validation to fail closed
	_ = st.SetTerm(2)
	_, _ = n.HandleAppendEntries(2, &transport.AppendEntriesRequest{
		Term:     2,
		LeaderID: 2,
	})

	if err := n.ValidateLeadership(1, 1); err == nil {
		t.Fatal("expected validation failure after stepdown, got nil")
	}
}

type mockBlockSender struct{}

func (m *mockBlockSender) NextSeqID() uint64 {
	return 1
}

func (m *mockBlockSender) Send(ctx context.Context, peerID cluster.NodeID, frame *transport.Frame) error {
	return nil
}
