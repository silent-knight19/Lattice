package transport_test

import (
	"bytes"
	"context"
	stdErrors "errors"
	"net"
	"sync/atomic"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/transport"
)

type mockRouter struct {
	writeCalled  atomic.Int32
	routeWriteFn func(ctx context.Context, req *transport.Request) (*transport.Response, error)
}

func (m *mockRouter) RouteWrite(ctx context.Context, req *transport.Request) (*transport.Response, error) {
	m.writeCalled.Add(1)
	if m.routeWriteFn != nil {
		return m.routeWriteFn(ctx, req)
	}
	return &transport.Response{
		OpCode: req.OpCode,
		Status: transport.StatusOk,
		SeqID:  req.SeqID,
	}, nil
}

func TestServer_Batch_Dispatch_Standalone(t *testing.T) {
	eng := newMockEngine()
	srv := startTestServer(t, transport.DefaultServerConfig(), eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Initial PUT
	_ = eng.Put(context.Background(), []byte("del_key"), []byte("to_delete"))

	req := &transport.Request{
		OpCode: transport.OpBatch,
		SeqID:  901,
		Batch: []transport.BatchOp{
			{Type: transport.BatchOpPut, Key: []byte("k1"), Value: []byte("v1")},
			{Type: transport.BatchOpPut, Key: []byte("k2"), Value: []byte("v2")},
			{Type: transport.BatchOpDelete, Key: []byte("del_key")},
		},
	}
	if err := transport.WriteRequest(conn, req); err != nil {
		t.Fatalf("write BATCH failed: %v", err)
	}
	resp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read BATCH response failed: %v", err)
	}
	if resp.SeqID != 901 {
		t.Errorf("expected SeqID 901, got %d", resp.SeqID)
	}
	if resp.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk, got %v: %s", resp.Status, resp.Message)
	}

	// Verify engine state
	val, err := eng.Get([]byte("k1"))
	if err != nil || !bytes.Equal(val, []byte("v1")) {
		t.Errorf("k1 get failed: val=%s, err=%v", string(val), err)
	}
	val, err = eng.Get([]byte("k2"))
	if err != nil || !bytes.Equal(val, []byte("v2")) {
		t.Errorf("k2 get failed: val=%s, err=%v", string(val), err)
	}
	_, err = eng.Get([]byte("del_key"))
	if !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("expected del_key deleted, got err=%v", err)
	}
}

func TestServer_Batch_Dispatch_ClusterMode(t *testing.T) {
	eng := newMockEngine()
	router := &mockRouter{}
	cfg := transport.DefaultServerConfig()
	cfg.ClusterMode = true
	cfg.ProposalRouter = router

	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	req := &transport.Request{
		OpCode: transport.OpBatch,
		SeqID:  902,
		Batch: []transport.BatchOp{
			{Type: transport.BatchOpPut, Key: []byte("k1"), Value: []byte("v1")},
		},
	}
	if err := transport.WriteRequest(conn, req); err != nil {
		t.Fatalf("write BATCH failed: %v", err)
	}
	resp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read BATCH response failed: %v", err)
	}
	if resp.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk, got %v: %s", resp.Status, resp.Message)
	}
	if router.writeCalled.Load() != 1 {
		t.Errorf("expected router.RouteWrite called once, got %d", router.writeCalled.Load())
	}
}

func TestServer_Batch_Dispatch_ClusterMode_FailClosed(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	cfg.ClusterMode = true
	cfg.ProposalRouter = nil // No router in cluster mode!

	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	req := &transport.Request{
		OpCode: transport.OpBatch,
		SeqID:  903,
		Batch: []transport.BatchOp{
			{Type: transport.BatchOpPut, Key: []byte("k1"), Value: []byte("v1")},
		},
	}
	if err := transport.WriteRequest(conn, req); err != nil {
		t.Fatalf("write BATCH failed: %v", err)
	}
	resp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read response failed: %v", err)
	}
	if resp.Status != transport.StatusError {
		t.Errorf("expected StatusError in cluster mode without router, got %v", resp.Status)
	}
}

func TestServer_Batch_ErrorMapping(t *testing.T) {
	eng := newMockEngine()
	srv := startTestServer(t, transport.DefaultServerConfig(), eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// 1. Storage engine closed
	eng.batchFn = func(ctx context.Context, batch []binary.BatchOp) error {
		return errors.ErrWriterClosed
	}
	req := &transport.Request{
		OpCode: transport.OpBatch,
		SeqID:  904,
		Batch:  []transport.BatchOp{{Type: transport.BatchOpPut, Key: []byte("k"), Value: []byte("v")}},
	}
	if err := transport.WriteRequest(conn, req); err != nil {
		t.Fatalf("write request failed: %v", err)
	}
	resp, err := transport.ReadResponse(conn)
	if err != nil || resp.Status != transport.StatusServerClosed {
		t.Errorf("expected StatusServerClosed, got %v (err=%v)", resp.Status, err)
	}

	// 2. Storage backpressure timeout
	eng.batchFn = func(ctx context.Context, batch []binary.BatchOp) error {
		return context.DeadlineExceeded
	}
	req.SeqID = 905
	if err := transport.WriteRequest(conn, req); err != nil {
		t.Fatalf("write request failed: %v", err)
	}
	resp, err = transport.ReadResponse(conn)
	if err != nil || resp.Status != transport.StatusThrottled {
		t.Errorf("expected StatusThrottled, got %v (err=%v)", resp.Status, err)
	}

	// 3. Invalid payload error
	eng.batchFn = func(ctx context.Context, batch []binary.BatchOp) error {
		return errors.ErrInvalidPayload
	}
	req.SeqID = 906
	if err := transport.WriteRequest(conn, req); err != nil {
		t.Fatalf("write request failed: %v", err)
	}
	resp, err = transport.ReadResponse(conn)
	if err != nil || resp.Status != transport.StatusInvalidRequest {
		t.Errorf("expected StatusInvalidRequest, got %v (err=%v)", resp.Status, err)
	}
}

func TestServer_Batch_RealEngine_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	eng := engine.NewEngineWithOptions(engine.EngineOptions{DBPath: dir})
	if err := eng.Open(); err != nil {
		t.Fatalf("failed to open engine: %v", err)
	}
	defer eng.Close()

	srv := startTestServer(t, transport.DefaultServerConfig(), eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Initial PUT
	putReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  1,
		Key:    []byte("key_A"),
		Value:  []byte("val_A_old"),
	}
	_ = transport.WriteRequest(conn, putReq)
	_, _ = transport.ReadResponse(conn)

	// Execute BATCH: update key_A, insert key_B, delete key_C (non-existent), and then delete key_A
	batchReq := &transport.Request{
		OpCode: transport.OpBatch,
		SeqID:  2,
		Batch: []transport.BatchOp{
			{Type: transport.BatchOpPut, Key: []byte("key_A"), Value: []byte("val_A_new")},
			{Type: transport.BatchOpPut, Key: []byte("key_B"), Value: []byte("val_B")},
			{Type: transport.BatchOpDelete, Key: []byte("key_C")},
			{Type: transport.BatchOpDelete, Key: []byte("key_A")}, // key_A should end up deleted!
		},
	}
	if err := transport.WriteRequest(conn, batchReq); err != nil {
		t.Fatalf("write BATCH failed: %v", err)
	}
	resp, err := transport.ReadResponse(conn)
	if err != nil || resp.Status != transport.StatusOk {
		t.Fatalf("BATCH failed: err=%v, status=%v, msg=%s", err, resp.Status, resp.Message)
	}

	// Verify over TCP: key_A is not found, key_B is val_B
	getA := &transport.Request{OpCode: transport.OpGet, SeqID: 3, Key: []byte("key_A")}
	_ = transport.WriteRequest(conn, getA)
	respA, _ := transport.ReadResponse(conn)
	if respA.Status != transport.StatusKeyNotFound {
		t.Errorf("expected key_A deleted, got status=%v", respA.Status)
	}

	getB := &transport.Request{OpCode: transport.OpGet, SeqID: 4, Key: []byte("key_B")}
	_ = transport.WriteRequest(conn, getB)
	respB, _ := transport.ReadResponse(conn)
	if respB.Status != transport.StatusOk || !bytes.Equal(respB.Value, []byte("val_B")) {
		t.Errorf("expected key_B val_B, got status=%v, val=%s", respB.Status, string(respB.Value))
	}
}
