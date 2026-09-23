package transport_test

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/transport"
)

type mockReadRouter struct {
	mu          sync.Mutex
	onRouteRead func(ctx context.Context, req *transport.Request) (*transport.Response, error)
	readCalls   int
}

func (m *mockReadRouter) RouteRead(ctx context.Context, req *transport.Request) (*transport.Response, error) {
	m.mu.Lock()
	m.readCalls++
	fn := m.onRouteRead
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, req)
	}
	return &transport.Response{
		OpCode: req.OpCode,
		Status: transport.StatusOk,
		SeqID:  req.SeqID,
		Value:  []byte("default-mock-val"),
	}, nil
}

func clientExchange(conn net.Conn, op transport.OpCode, seqID uint64, key, val []byte) (*transport.Response, error) {
	req := &transport.Request{
		OpCode: op,
		SeqID:  seqID,
		Key:    key,
		Value:  val,
	}
	if err := transport.WriteRequest(conn, req); err != nil {
		return nil, err
	}
	return transport.ReadResponse(conn)
}

// 1. Standalone mode serves local engine directly without touching any router.
func TestServer_GET_StandaloneMode_DirectEngine(t *testing.T) {
	eng := newMockEngine()
	_ = eng.Put(context.Background(), []byte("key1"), []byte("val1"))

	srv, err := transport.NewServer(transport.DefaultServerConfig(), eng)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to bind listener: %v", err)
	}
	defer ln.Close()

	go func() { _ = srv.Serve(ln) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	resp, err := clientExchange(conn, transport.OpGet, 1, []byte("key1"), nil)
	if err != nil {
		t.Fatalf("clientExchange failed: %v", err)
	}
	if resp.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk, got: %s", resp.Status)
	}
	if string(resp.Value) != "val1" {
		t.Fatalf("expected val1, got: %s", string(resp.Value))
	}
}

// 2. Cluster mode without read-routing integration does not fall back to direct engine access; fails closed.
func TestServer_GET_ClusterMode_MissingRouter_FailsClosed(t *testing.T) {
	eng := newMockEngine()
	_ = eng.Put(context.Background(), []byte("secret-key"), []byte("stale-val"))

	cfg := transport.DefaultServerConfig()
	cfg.ClusterMode = true // Cluster mode active
	cfg.ReadRouter = nil
	cfg.ProposalRouter = nil

	srv, err := transport.NewServer(cfg, eng)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to bind listener: %v", err)
	}
	defer ln.Close()

	go func() { _ = srv.Serve(ln) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	resp, err := clientExchange(conn, transport.OpGet, 1, []byte("secret-key"), nil)
	if err != nil {
		t.Fatalf("clientExchange failed: %v", err)
	}
	// Must fail closed with StatusError, NEVER returning the engine value
	if resp.Status != transport.StatusError {
		t.Fatalf("expected StatusError in cluster mode without router, got: %s", resp.Status)
	}
	if len(resp.Value) != 0 {
		t.Fatalf("expected empty value on error, got: %s", string(resp.Value))
	}
}

// 3. Leader GET routes via ReadRouter and serves linearizable read response.
func TestServer_GET_Leader_RoutesViaReadRouter(t *testing.T) {
	eng := newMockEngine()

	router := &mockReadRouter{
		onRouteRead: func(ctx context.Context, req *transport.Request) (*transport.Response, error) {
			return &transport.Response{
				OpCode: req.OpCode,
				Status: transport.StatusOk,
				SeqID:  req.SeqID,
				Value:  []byte("linearizable-val"),
			}, nil
		},
	}

	cfg := transport.DefaultServerConfig()
	cfg.ClusterMode = true
	cfg.ReadRouter = router

	srv, err := transport.NewServer(cfg, eng)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to bind listener: %v", err)
	}
	defer ln.Close()

	go func() { _ = srv.Serve(ln) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	resp, err := clientExchange(conn, transport.OpGet, 42, []byte("some-key"), nil)
	if err != nil {
		t.Fatalf("clientExchange failed: %v", err)
	}
	if resp.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk, got: %s", resp.Status)
	}
	if string(resp.Value) != "linearizable-val" {
		t.Fatalf("expected linearizable-val, got: %s", string(resp.Value))
	}
	if resp.SeqID != 42 {
		t.Fatalf("expected SeqID 42, got %d", resp.SeqID)
	}
}

// 4. Follower GET returns StatusNotLeader with leader redirection information.
func TestServer_GET_Follower_RedirectsWithTopology(t *testing.T) {
	eng := newMockEngine()

	router := &mockReadRouter{
		onRouteRead: func(ctx context.Context, req *transport.Request) (*transport.Response, error) {
			return &transport.Response{
				OpCode:     req.OpCode,
				Status:     transport.StatusNotLeader,
				SeqID:      req.SeqID,
				Message:    transport.FormatRedirectMessage(1, "127.0.0.1:9099"),
				LeaderID:   1,
				LeaderAddr: "127.0.0.1:9099",
			}, nil
		},
	}

	cfg := transport.DefaultServerConfig()
	cfg.ClusterMode = true
	cfg.ReadRouter = router

	srv, err := transport.NewServer(cfg, eng)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to bind listener: %v", err)
	}
	defer ln.Close()

	go func() { _ = srv.Serve(ln) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	resp, err := clientExchange(conn, transport.OpGet, 7, []byte("foo"), nil)
	if err != nil {
		t.Fatalf("clientExchange failed: %v", err)
	}
	if resp.Status != transport.StatusNotLeader {
		t.Fatalf("expected StatusNotLeader, got: %s", resp.Status)
	}
	if resp.LeaderID != 1 || resp.LeaderAddr != "127.0.0.1:9099" {
		t.Fatalf("unexpected redirect details: id=%d addr=%s", resp.LeaderID, resp.LeaderAddr)
	}
}

// 5. Concurrent linearizable GETs are race-free.
func TestServer_GET_ConcurrentReads(t *testing.T) {
	eng := newMockEngine()

	router := &mockReadRouter{
		onRouteRead: func(ctx context.Context, req *transport.Request) (*transport.Response, error) {
			return &transport.Response{
				OpCode: req.OpCode,
				Status: transport.StatusOk,
				SeqID:  req.SeqID,
				Value:  append([]byte("val-"), req.Key...),
			}, nil
		},
	}

	cfg := transport.DefaultServerConfig()
	cfg.ClusterMode = true
	cfg.ReadRouter = router

	srv, err := transport.NewServer(cfg, eng)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to bind listener: %v", err)
	}
	defer ln.Close()

	go func() { _ = srv.Serve(ln) }()

	const numConcurrent = 25
	var wg sync.WaitGroup
	wg.Add(numConcurrent)
	errCh := make(chan error, numConcurrent)

	for i := 0; i < numConcurrent; i++ {
		go func(id int) {
			defer wg.Done()
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				errCh <- err
				return
			}
			defer conn.Close()

			resp, err := clientExchange(conn, transport.OpGet, uint64(id), []byte("testkey"), nil)
			if err != nil {
				errCh <- err
				return
			}
			if resp.Status != transport.StatusOk || string(resp.Value) != "val-testkey" {
				t.Errorf("unexpected resp: status=%s val=%s", resp.Status, string(resp.Value))
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent exchange failed: %v", err)
	}
}
