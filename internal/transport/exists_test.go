package transport_test

import (
	"context"
	"net"
	"sync/atomic"
	"testing"

	"github.com/silent-knight19/lattice/internal/transport"
)

type existsMockReadRouter struct {
	readCalled  atomic.Int32
	routeReadFn func(ctx context.Context, req *transport.Request) (*transport.Response, error)
}

func (m *existsMockReadRouter) RouteRead(ctx context.Context, req *transport.Request) (*transport.Response, error) {
	m.readCalled.Add(1)
	if m.routeReadFn != nil {
		return m.routeReadFn(ctx, req)
	}
	return &transport.Response{
		OpCode: req.OpCode,
		Status: transport.StatusOk,
		SeqID:  req.SeqID,
		Exists: true,
	}, nil
}

func TestServer_Exists_Standalone(t *testing.T) {
	eng := newMockEngine()
	srv := startTestServer(t, transport.DefaultServerConfig(), eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// 1. Missing key
	req := &transport.Request{
		OpCode: transport.OpExists,
		SeqID:  1001,
		Key:    []byte("absent_key"),
	}
	if err := transport.WriteRequest(conn, req); err != nil {
		t.Fatalf("write request failed: %v", err)
	}
	resp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read response failed: %v", err)
	}
	if resp.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk, got 0x%02x (%s)", resp.Status, resp.Message)
	}
	if resp.Exists {
		t.Fatalf("expected Exists=false for absent key, got true")
	}

	// 2. Insert key and check exists
	_ = eng.Put(context.Background(), []byte("present_key"), []byte("val"))
	req = &transport.Request{
		OpCode: transport.OpExists,
		SeqID:  1002,
		Key:    []byte("present_key"),
	}
	if err := transport.WriteRequest(conn, req); err != nil {
		t.Fatalf("write request failed: %v", err)
	}
	resp, err = transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read response failed: %v", err)
	}
	if resp.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk, got 0x%02x (%s)", resp.Status, resp.Message)
	}
	if !resp.Exists {
		t.Fatalf("expected Exists=true for present key, got false")
	}

	// 3. Delete key and check exists
	_ = eng.Delete(context.Background(), []byte("present_key"))
	req = &transport.Request{
		OpCode: transport.OpExists,
		SeqID:  1003,
		Key:    []byte("present_key"),
	}
	if err := transport.WriteRequest(conn, req); err != nil {
		t.Fatalf("write request failed: %v", err)
	}
	resp, err = transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read response failed: %v", err)
	}
	if resp.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk, got 0x%02x (%s)", resp.Status, resp.Message)
	}
	if resp.Exists {
		t.Fatalf("expected Exists=false for deleted key, got true")
	}

	// 4. Binary key
	binKey := []byte{0x00, 0x01, 0xff, 0xfe}
	_ = eng.Put(context.Background(), binKey, []byte("val"))
	req = &transport.Request{
		OpCode: transport.OpExists,
		SeqID:  1004,
		Key:    binKey,
	}
	if err := transport.WriteRequest(conn, req); err != nil {
		t.Fatalf("write request failed: %v", err)
	}
	resp, err = transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read response failed: %v", err)
	}
	if resp.Status != transport.StatusOk || !resp.Exists {
		t.Fatalf("expected Exists=true for binary key, got status=0x%02x exists=%v", resp.Status, resp.Exists)
	}
}

func TestServer_Exists_Cluster_Routing(t *testing.T) {
	eng := newMockEngine()
	router := &existsMockReadRouter{}

	cfg := transport.DefaultServerConfig()
	cfg.ClusterMode = true
	cfg.ReadRouter = router

	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// 1. Leader routing returns success
	req := &transport.Request{
		OpCode: transport.OpExists,
		SeqID:  2001,
		Key:    []byte("cluster_key"),
	}
	if err := transport.WriteRequest(conn, req); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	resp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if resp.Status != transport.StatusOk || !resp.Exists {
		t.Fatalf("expected StatusOk with Exists=true, got status=0x%02x exists=%v", resp.Status, resp.Exists)
	}
	if router.readCalled.Load() != 1 {
		t.Fatalf("expected ReadRouter to be called once, got %d", router.readCalled.Load())
	}

	// 2. Follower routing returns redirect
	router.routeReadFn = func(ctx context.Context, req *transport.Request) (*transport.Response, error) {
		return &transport.Response{
			OpCode:     req.OpCode,
			Status:     transport.StatusNotLeader,
			SeqID:      req.SeqID,
			LeaderID:   2,
			LeaderAddr: "127.0.0.1:9092",
			Message:    transport.FormatRedirectMessage(2, "127.0.0.1:9092"),
		}, nil
	}

	req = &transport.Request{
		OpCode: transport.OpExists,
		SeqID:  2002,
		Key:    []byte("cluster_key"),
	}
	if err := transport.WriteRequest(conn, req); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	resp, err = transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if resp.Status != transport.StatusNotLeader {
		t.Fatalf("expected StatusNotLeader, got 0x%02x", resp.Status)
	}
	if resp.LeaderID != 2 || resp.LeaderAddr != "127.0.0.1:9092" {
		t.Fatalf("expected redirect to node 2 at 127.0.0.1:9092, got %d (%s)", resp.LeaderID, resp.LeaderAddr)
	}
}

func TestServer_Exists_Cluster_FailClosed_WhenNoRouter(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	cfg.ClusterMode = true
	// cfg.ReadRouter is nil!

	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	req := &transport.Request{
		OpCode: transport.OpExists,
		SeqID:  3001,
		Key:    []byte("any_key"),
	}
	if err := transport.WriteRequest(conn, req); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	resp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if resp.Status != transport.StatusError {
		t.Fatalf("expected StatusError when router is missing in cluster mode, got 0x%02x", resp.Status)
	}
}
