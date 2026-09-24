package main

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/transport"
)

// TestDaemon_ClusterMode_EndToEndWiring verifies the complete Phase 16 end-to-end
// clustered write path in a real daemon process:
//
//	client -> transport -> proposal router -> raft proposal -> durable log
//	       -> commit -> apply loop -> engine mutation -> client GET
func TestDaemon_ClusterMode_EndToEndWiring(t *testing.T) {
	tempDir := t.TempDir()

	// Ephemeral port for client transport
	l1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to bind server port: %v", err)
	}
	serverPort := l1.Addr().(*net.TCPAddr).Port
	_ = l1.Close()

	// Ephemeral port for peer address
	l2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to bind peer port: %v", err)
	}
	peerPort := l2.Addr().(*net.TCPAddr).Port
	_ = l2.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readyCh := make(chan struct{})
	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)

	args := []string{
		"--data-dir", tempDir,
		"--port", strconv.Itoa(serverPort),
		"--node-id", "1",
		"--peer-address", net.JoinHostPort("127.0.0.1", strconv.Itoa(peerPort)),
	}

	exitCh := make(chan int, 1)
	go func() {
		exitCode := runWithContext(ctx, args, stdout, stderr, readyCh)
		exitCh <- exitCode
	}()

	select {
	case <-readyCh:
		// Daemon initialized, Raft storage opened, node became leader, apply loop running
	case code := <-exitCh:
		t.Fatalf("daemon exited prematurely with code %d: stderr=%s", code, stderr.String())
	case <-time.After(5 * time.Second):
		t.Fatalf("daemon timed out waiting for readiness")
	}

	serverAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(serverPort))
	conn, err := net.DialTimeout("tcp", serverAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to daemon: %v", err)
	}
	defer conn.Close()

	// 1. Client PUT through Raft consensus pipeline
	putReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  101,
		Key:    []byte("cluster_key_1"),
		Value:  []byte("cluster_val_1"),
	}
	if err := transport.WriteRequest(conn, putReq); err != nil {
		t.Fatalf("failed to write PUT request: %v", err)
	}
	putResp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("failed to read PUT response: %v", err)
	}
	if putResp.Status != transport.StatusOk {
		t.Fatalf("PUT returned status %v: %s", putResp.Status, putResp.Message)
	}
	if putResp.SeqID != 101 {
		t.Fatalf("expected SeqID 101, got %d", putResp.SeqID)
	}

	// Wait briefly for apply loop to process committed entry into Engine
	// (for N=1 cluster, commit and apply occur in milliseconds)
	time.Sleep(50 * time.Millisecond)

	// 2. Client GET: verify value applied to Engine
	getReq := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  102,
		Key:    []byte("cluster_key_1"),
	}
	if err := transport.WriteRequest(conn, getReq); err != nil {
		t.Fatalf("failed to write GET request: %v", err)
	}
	getResp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("failed to read GET response: %v", err)
	}
	if getResp.Status != transport.StatusOk {
		t.Fatalf("GET returned status %v: %s", getResp.Status, getResp.Message)
	}
	if !bytes.Equal(getResp.Value, []byte("cluster_val_1")) {
		t.Fatalf("expected 'cluster_val_1', got %q", string(getResp.Value))
	}

	// 3. Client DELETE through Raft consensus pipeline
	delReq := &transport.Request{
		OpCode: transport.OpDelete,
		SeqID:  103,
		Key:    []byte("cluster_key_1"),
	}
	if err := transport.WriteRequest(conn, delReq); err != nil {
		t.Fatalf("failed to write DELETE request: %v", err)
	}
	delResp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("failed to read DELETE response: %v", err)
	}
	if delResp.Status != transport.StatusOk {
		t.Fatalf("DELETE returned status %v: %s", delResp.Status, delResp.Message)
	}

	time.Sleep(50 * time.Millisecond)

	// 4. Client GET after DELETE: key should not be found
	getReq2 := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  104,
		Key:    []byte("cluster_key_1"),
	}
	if err := transport.WriteRequest(conn, getReq2); err != nil {
		t.Fatalf("failed to write second GET request: %v", err)
	}
	getResp2, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("failed to read second GET response: %v", err)
	}
	if getResp2.Status != transport.StatusKeyNotFound {
		t.Fatalf("expected StatusKeyNotFound, got %v: %s", getResp2.Status, getResp2.Message)
	}

	// 5. Graceful shutdown
	cancel()
	select {
	case code := <-exitCh:
		if code != ExitSuccess {
			t.Fatalf("daemon exited with error code %d: stderr=%s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon timed out during graceful shutdown")
	}
}

// TestDaemon_StandaloneMode_PermitsDirectWrite verifies that standalone mode (no cluster flags)
// continues to dispatch mutations directly to the local Engine.
func TestDaemon_StandaloneMode_PermitsDirectWrite(t *testing.T) {
	tempDir := t.TempDir()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to bind server port: %v", err)
	}
	serverPort := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readyCh := make(chan struct{})
	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)

	// Standalone mode: only data-dir and port, no node-id or cluster flags
	args := []string{
		"--data-dir", tempDir,
		"--port", strconv.Itoa(serverPort),
	}

	exitCh := make(chan int, 1)
	go func() {
		exitCode := runWithContext(ctx, args, stdout, stderr, readyCh)
		exitCh <- exitCode
	}()

	select {
	case <-readyCh:
	case code := <-exitCh:
		t.Fatalf("daemon exited prematurely with code %d: stderr=%s", code, stderr.String())
	case <-time.After(5 * time.Second):
		t.Fatalf("daemon timed out waiting for readiness")
	}

	serverAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(serverPort))
	conn, err := net.DialTimeout("tcp", serverAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to daemon: %v", err)
	}
	defer conn.Close()

	// Direct PUT
	putReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  201,
		Key:    []byte("standalone_key"),
		Value:  []byte("standalone_val"),
	}
	if err := transport.WriteRequest(conn, putReq); err != nil {
		t.Fatalf("failed to write PUT request: %v", err)
	}
	putResp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("failed to read PUT response: %v", err)
	}
	if putResp.Status != transport.StatusOk {
		t.Fatalf("PUT returned status %v: %s", putResp.Status, putResp.Message)
	}

	// Direct GET
	getReq := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  202,
		Key:    []byte("standalone_key"),
	}
	if err := transport.WriteRequest(conn, getReq); err != nil {
		t.Fatalf("failed to write GET request: %v", err)
	}
	getResp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("failed to read GET response: %v", err)
	}
	if getResp.Status != transport.StatusOk || !bytes.Equal(getResp.Value, []byte("standalone_val")) {
		t.Fatalf("GET returned unexpected response: %v, val=%q", getResp.Status, string(getResp.Value))
	}

	// Direct DELETE
	delReq := &transport.Request{
		OpCode: transport.OpDelete,
		SeqID:  203,
		Key:    []byte("standalone_key"),
	}
	if err := transport.WriteRequest(conn, delReq); err != nil {
		t.Fatalf("failed to write DELETE request: %v", err)
	}
	delResp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("failed to read DELETE response: %v", err)
	}
	if delResp.Status != transport.StatusOk {
		t.Fatalf("DELETE returned status %v: %s", delResp.Status, delResp.Message)
	}

	cancel()
	select {
	case code := <-exitCh:
		if code != ExitSuccess {
			t.Fatalf("daemon exited with error code %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon timed out during shutdown")
	}
}

// TestDaemon_ClusterMode_MissingRouterFailsClosed verifies that if cluster mode is active
// on a transport server without a router, direct Engine writes are strictly blocked.
func TestDaemon_ClusterMode_MissingRouterFailsClosed(t *testing.T) {
	cfg := transport.DefaultServerConfig()
	cfg.ClusterMode = true
	cfg.ProposalRouter = nil // missing router
	cfg.InsecureTransport = true
	cfg.Address = "127.0.0.1:0"

	// Mock engine that tracks calls
	eng := &testCountingEngine{}
	srv, err := transport.NewServer(cfg, eng)
	if err != nil {
		t.Fatal(err)
	}

	// PUT must be rejected fail-closed
	putResp := srv.TestDispatch(&transport.Request{
		OpCode: transport.OpPut,
		Key:    []byte("k"),
		Value:  []byte("v"),
		SeqID:  1,
	})
	if putResp.Status != transport.StatusError {
		t.Fatalf("expected StatusError when router is nil in cluster mode, got %v", putResp.Status)
	}
	if eng.putCalls != 0 {
		t.Fatalf("Engine.Put was called directly (%d times) — bypass violation!", eng.putCalls)
	}

	// DELETE must also be rejected fail-closed
	delResp := srv.TestDispatch(&transport.Request{
		OpCode: transport.OpDelete,
		Key:    []byte("k"),
		SeqID:  2,
	})
	if delResp.Status != transport.StatusError {
		t.Fatalf("expected StatusError for DELETE when router is nil in cluster mode, got %v", delResp.Status)
	}
	if eng.delCalls != 0 {
		t.Fatalf("Engine.Delete was called directly (%d times) — bypass violation!", eng.delCalls)
	}

	// BATCH must also be rejected fail-closed
	batchResp := srv.TestDispatch(&transport.Request{
		OpCode: transport.OpBatch,
		Batch: []transport.BatchOp{
			{Type: transport.BatchOpPut, Key: []byte("k"), Value: []byte("v")},
		},
		SeqID: 3,
	})
	if batchResp.Status != transport.StatusError {
		t.Fatalf("expected StatusError for BATCH when router is nil in cluster mode, got %v", batchResp.Status)
	}
	if eng.batchCalls != 0 {
		t.Fatalf("Engine.Batch was called directly (%d times) — bypass violation!", eng.batchCalls)
	}
}

type testCountingEngine struct {
	putCalls   int
	delCalls   int
	batchCalls int
}

func (e *testCountingEngine) Put(ctx context.Context, key, val []byte) error {
	e.putCalls++
	return nil
}

func (e *testCountingEngine) Get(key []byte) ([]byte, error) {
	return nil, nil
}

func (e *testCountingEngine) Delete(ctx context.Context, key []byte) error {
	e.delCalls++
	return nil
}

func (e *testCountingEngine) Batch(ctx context.Context, batch []binary.BatchOp) error {
	e.batchCalls++
	return nil
}

func (e *testCountingEngine) Exists(key []byte) (bool, error) {
	return false, nil
}

func (e *testCountingEngine) Stats() (transport.EngineStats, transport.MemoryStats, transport.StorageStats, transport.CacheStats, error) {
	return transport.EngineStats{State: "open"}, transport.MemoryStats{}, transport.StorageStats{}, transport.CacheStats{}, nil
}
