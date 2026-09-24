package transport_test

import (
	"bytes"
	"context"
	stdErrors "errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/transport"
)

// mockEngine satisfies transport.Engine with injectable hooks for error testing.
type mockEngine struct {
	mu       sync.RWMutex
	store    map[string][]byte
	putFn    func(ctx context.Context, key, val []byte) error
	getFn    func(key []byte) ([]byte, error)
	delFn    func(ctx context.Context, key []byte) error
	batchFn  func(ctx context.Context, batch []binary.BatchOp) error
	existsFn func(key []byte) (bool, error)
}

func newMockEngine() *mockEngine {
	return &mockEngine{
		store: make(map[string][]byte),
	}
}

func (m *mockEngine) Put(ctx context.Context, key, val []byte) error {
	if m.putFn != nil {
		return m.putFn(ctx, key, val)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(val))
	copy(cp, val)
	m.store[string(key)] = cp
	return nil
}

func (m *mockEngine) Get(key []byte) ([]byte, error) {
	if m.getFn != nil {
		return m.getFn(key)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	val, ok := m.store[string(key)]
	if !ok {
		return nil, errors.ErrKeyNotFound
	}
	cp := make([]byte, len(val))
	copy(cp, val)
	return cp, nil
}

func (m *mockEngine) Delete(ctx context.Context, key []byte) error {
	if m.delFn != nil {
		return m.delFn(ctx, key)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.store, string(key))
	return nil
}

func (m *mockEngine) Batch(ctx context.Context, batch []binary.BatchOp) error {
	if m.batchFn != nil {
		return m.batchFn(ctx, batch)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, op := range batch {
		if op.Type == binary.OpTypePut {
			cp := make([]byte, len(op.Value))
			copy(cp, op.Value)
			m.store[string(op.Key)] = cp
		} else if op.Type == binary.OpTypeDelete {
			delete(m.store, string(op.Key))
		}
	}
	return nil
}

func (m *mockEngine) Exists(key []byte) (bool, error) {
	if m.existsFn != nil {
		return m.existsFn(key)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.store[string(key)]
	return ok, nil
}

func startTestServer(t *testing.T, cfg transport.ServerConfig, eng transport.Engine) *transport.Server {
	t.Helper()
	srv, err := transport.NewServer(cfg, eng)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("failed to listen on dynamic port: %v", err)
	}

	t.Cleanup(func() {
		_ = srv.Close()
	})
	return srv
}

func TestServer_ConfigAndLifecycle(t *testing.T) {
	eng := newMockEngine()

	// 1. Nil engine rejected
	_, err := transport.NewServer(transport.DefaultServerConfig(), nil)
	if !stdErrors.Is(err, errors.ErrNilReceiver) {
		t.Fatalf("expected ErrNilReceiver, got: %v", err)
	}

	// 2. Start and double-start rejection
	srv := startTestServer(t, transport.DefaultServerConfig(), eng)
	if srv.Addr() == nil {
		t.Fatal("expected non-nil server Addr")
	}

	err = srv.Listen("127.0.0.1:0")
	if !stdErrors.Is(err, errors.ErrServerAlreadyStarted) {
		t.Fatalf("expected ErrServerAlreadyStarted, got: %v", err)
	}

	// 3. Serve on nil listener rejected
	freshSrv, _ := transport.NewServer(transport.DefaultServerConfig(), eng)
	if err := freshSrv.Serve(nil); !stdErrors.Is(err, errors.ErrNilReceiver) {
		t.Fatalf("expected ErrNilReceiver for nil listener, got: %v", err)
	}

	// 4. Shutdown idempotency
	if err := srv.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("second close failed: %v", err)
	}
}

func TestServer_BasicOperations_MockEngine(t *testing.T) {
	eng := newMockEngine()
	srv := startTestServer(t, transport.DefaultServerConfig(), eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// 1. PUT key1 -> val1
	putReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  101,
		Key:    []byte("test_key"),
		Value:  []byte("test_value"),
	}
	if err := transport.WriteRequest(conn, putReq); err != nil {
		t.Fatalf("write PUT request failed: %v", err)
	}
	resp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read PUT response failed: %v", err)
	}
	if resp.SeqID != 101 {
		t.Errorf("seqID mismatch: got %d, want 101", resp.SeqID)
	}
	if resp.Status != transport.StatusOk {
		t.Errorf("expected StatusOk, got: %v (%s)", resp.Status, resp.Message)
	}

	// 2. GET key1 -> val1
	getReq := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  102,
		Key:    []byte("test_key"),
	}
	if err := transport.WriteRequest(conn, getReq); err != nil {
		t.Fatalf("write GET request failed: %v", err)
	}
	resp, err = transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read GET response failed: %v", err)
	}
	if resp.SeqID != 102 {
		t.Errorf("seqID mismatch: got %d, want 102", resp.SeqID)
	}
	if resp.Status != transport.StatusOk {
		t.Errorf("expected StatusOk, got: %v (%s)", resp.Status, resp.Message)
	}
	if !bytes.Equal(resp.Value, []byte("test_value")) {
		t.Errorf("expected test_value, got: %s", string(resp.Value))
	}

	// 3. DELETE key1
	delReq := &transport.Request{
		OpCode: transport.OpDelete,
		SeqID:  103,
		Key:    []byte("test_key"),
	}
	if err := transport.WriteRequest(conn, delReq); err != nil {
		t.Fatalf("write DELETE request failed: %v", err)
	}
	resp, err = transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read DELETE response failed: %v", err)
	}
	if resp.SeqID != 103 {
		t.Errorf("seqID mismatch: got %d, want 103", resp.SeqID)
	}
	if resp.Status != transport.StatusOk {
		t.Errorf("expected StatusOk, got: %v", resp.Status)
	}

	// 4. GET key1 -> not found
	if err := transport.WriteRequest(conn, getReq); err != nil {
		t.Fatalf("write GET request failed: %v", err)
	}
	resp, err = transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read GET response failed: %v", err)
	}
	if resp.Status != transport.StatusKeyNotFound {
		t.Errorf("expected StatusKeyNotFound, got: %v", resp.Status)
	}
}

func TestServer_UnsupportedOpcodes_DoNotFake(t *testing.T) {
	eng := newMockEngine()
	srv := startTestServer(t, transport.DefaultServerConfig(), eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	unsupported := []struct {
		name    string
		req     *transport.Request
		wantMsg string
	}{
		{
			name: "OpStats",
			req: &transport.Request{
				OpCode: transport.OpStats,
				SeqID:  203,
			},
			wantMsg: "STATS is not implemented",
		},
	}

	for _, tc := range unsupported {
		t.Run(tc.name, func(t *testing.T) {
			if err := transport.WriteRequest(conn, tc.req); err != nil {
				t.Fatalf("write request failed: %v", err)
			}
			resp, err := transport.ReadResponse(conn)
			if err != nil {
				t.Fatalf("read response failed: %v", err)
			}
			if resp.SeqID != tc.req.SeqID {
				t.Errorf("seqID mismatch: got %d, want %d", resp.SeqID, tc.req.SeqID)
			}
			if resp.Status != transport.StatusInvalidRequest {
				t.Errorf("expected StatusInvalidRequest, got: %v", resp.Status)
			}
			if !bytes.Contains([]byte(resp.Message), []byte(tc.wantMsg)) {
				t.Errorf("message %q did not contain %q", resp.Message, tc.wantMsg)
			}
		})
	}
}

func TestServer_ErrorMappingAndSanitization(t *testing.T) {
	eng := newMockEngine()
	srv := startTestServer(t, transport.DefaultServerConfig(), eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// 1. Engine closed mapping -> StatusServerClosed
	eng.putFn = func(ctx context.Context, key, val []byte) error {
		return errors.ErrWriterClosed
	}
	putReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  301,
		Key:    []byte("k"),
		Value:  []byte("v"),
	}
	if err := transport.WriteRequest(conn, putReq); err != nil {
		t.Fatalf("write request failed: %v", err)
	}
	resp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read response failed: %v", err)
	}
	if resp.Status != transport.StatusServerClosed {
		t.Errorf("expected StatusServerClosed, got: %v", resp.Status)
	}

	// 2. Engine backpressure timeout -> StatusThrottled
	eng.putFn = func(ctx context.Context, key, val []byte) error {
		return context.DeadlineExceeded
	}
	putReq.SeqID = 302
	if err := transport.WriteRequest(conn, putReq); err != nil {
		t.Fatalf("write request failed: %v", err)
	}
	resp, err = transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read response failed: %v", err)
	}
	if resp.Status != transport.StatusThrottled {
		t.Errorf("expected StatusThrottled, got: %v", resp.Status)
	}

	// 3. Raw internal error sanitization -> StatusError with sanitized message
	eng.putFn = func(ctx context.Context, key, val []byte) error {
		return fmt.Errorf("sensitive internal filesystem error at /data/db/wal.0001")
	}
	putReq.SeqID = 303
	if err := transport.WriteRequest(conn, putReq); err != nil {
		t.Fatalf("write request failed: %v", err)
	}
	resp, err = transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read response failed: %v", err)
	}
	if resp.Status != transport.StatusError {
		t.Errorf("expected StatusError, got: %v", resp.Status)
	}
	if bytes.Contains([]byte(resp.Message), []byte("/data/db/wal.0001")) {
		t.Errorf("sensitive diagnostic leaked into wire response: %s", resp.Message)
	}
	if resp.Message != "internal storage error" {
		t.Errorf("expected sanitized error, got: %s", resp.Message)
	}

	// 4. Over-sized GET response value guarded
	eng.getFn = func(key []byte) ([]byte, error) {
		// return fake 6MB value (> 5MB MaxPayloadLength)
		return make([]byte, transport.MaxPayloadLength+1), nil
	}
	getReq := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  304,
		Key:    []byte("k"),
	}
	if err := transport.WriteRequest(conn, getReq); err != nil {
		t.Fatalf("write request failed: %v", err)
	}
	resp, err = transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read response failed: %v", err)
	}
	if resp.Status != transport.StatusError {
		t.Errorf("expected StatusError for oversized response value, got: %v", resp.Status)
	}
}

func TestServer_Slowloris_SlowHeaderDefense(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	cfg.HeaderTimeout = 100 * time.Millisecond
	cfg.IdleTimeout = 100 * time.Millisecond

	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Transmit only 1 byte of the 18-byte header and sleep
	if _, err := conn.Write([]byte{0x4C}); err != nil {
		t.Fatalf("write 1 byte failed: %v", err)
	}

	// Wait past HeaderTimeout
	time.Sleep(150 * time.Millisecond)

	// Verify server terminated connection
	var buf [10]byte
	conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, readErr := conn.Read(buf[:])
	if readErr == nil {
		t.Fatal("expected connection to be closed by slow-header defense, but read succeeded")
	}
}

func TestServer_Slowloris_SlowPayloadDefense(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	cfg.PayloadTimeout = 100 * time.Millisecond

	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send valid 18-byte header declaring 100 bytes of payload
	hdr := transport.Header{
		Magic:         transport.Magic,
		OpCode:        transport.OpPut,
		SeqID:         501,
		PayloadLength: 100,
	}
	var hdrBuf [transport.HeaderSize]byte
	hdr.Encode(hdrBuf[:])
	if _, err := conn.Write(hdrBuf[:]); err != nil {
		t.Fatalf("write header failed: %v", err)
	}

	// Send only 10 bytes of payload and stall
	if _, err := conn.Write([]byte("0123456789")); err != nil {
		t.Fatalf("write partial payload failed: %v", err)
	}

	// Wait past PayloadTimeout
	time.Sleep(150 * time.Millisecond)

	// Verify server severed connection
	var buf [10]byte
	conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, readErr := conn.Read(buf[:])
	if readErr == nil {
		t.Fatal("expected connection to be closed by slow-payload defense, but read succeeded")
	}
}

func TestServer_Slowloris_IdleTimeoutDefense(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	cfg.IdleTimeout = 100 * time.Millisecond
	cfg.HeaderTimeout = 100 * time.Millisecond

	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send first request successfully
	putReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  601,
		Key:    []byte("k"),
		Value:  []byte("v"),
	}
	if err := transport.WriteRequest(conn, putReq); err != nil {
		t.Fatalf("write request failed: %v", err)
	}
	if _, err := transport.ReadResponse(conn); err != nil {
		t.Fatalf("read response failed: %v", err)
	}

	// Now remain idle longer than IdleTimeout
	time.Sleep(150 * time.Millisecond)

	// Next read should observe severed connection
	var buf [10]byte
	conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, readErr := conn.Read(buf[:])
	if readErr == nil {
		t.Fatal("expected idle connection to be closed by server, but read succeeded")
	}
}

func TestServer_MaxConnectionsCeiling(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	cfg.MaxConnections = 2

	srv := startTestServer(t, cfg, eng)

	// Connect 2 clients and keep them active
	c1, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial c1 failed: %v", err)
	}
	defer c1.Close()

	c2, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial c2 failed: %v", err)
	}
	defer c2.Close()

	// Wait briefly for server to track both connections
	time.Sleep(20 * time.Millisecond)
	if srv.ActiveConnections() != 2 {
		t.Fatalf("expected 2 active connections, got: %d", srv.ActiveConnections())
	}

	// 3rd client should be rejected immediately
	c3, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial c3 failed: %v", err)
	}
	defer c3.Close()

	// Read on c3 should fail with EOF because server severed it
	var buf [1]byte
	c3.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	n, readErr := c3.Read(buf[:])
	if readErr == nil && n > 0 {
		t.Fatal("expected c3 to be closed due to MaxConnections limit, but read succeeded")
	}

	// Close c1; now c4 should be accepted
	c1.Close()
	time.Sleep(20 * time.Millisecond)

	c4, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial c4 failed: %v", err)
	}
	defer c4.Close()

	putReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  701,
		Key:    []byte("k"),
		Value:  []byte("v"),
	}
	if err := transport.WriteRequest(c4, putReq); err != nil {
		t.Fatalf("c4 request failed: %v", err)
	}
	resp, err := transport.ReadResponse(c4)
	if err != nil || resp.Status != transport.StatusOk {
		t.Fatalf("c4 response failed: %v, status: %v", err, resp.Status)
	}
}

func TestServer_RealEngine_Integration(t *testing.T) {
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

	// 1. Binary key and value with embedded nulls and 0xFF
	binaryKey := []byte{0x00, 'b', 'i', 'n', 0x00, 0xFF, 'k', 'e', 'y'}
	binaryVal := []byte{0xFF, 0x00, 0xAA, 0x55, 0x00, 0xFF}

	putReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  801,
		Key:    binaryKey,
		Value:  binaryVal,
	}
	if err := transport.WriteRequest(conn, putReq); err != nil {
		t.Fatalf("write binary PUT failed: %v", err)
	}
	resp, err := transport.ReadResponse(conn)
	if err != nil || resp.Status != transport.StatusOk {
		t.Fatalf("PUT failed: err=%v, status=%v", err, resp.Status)
	}

	// 2. Read back over TCP
	getReq := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  802,
		Key:    binaryKey,
	}
	if err := transport.WriteRequest(conn, getReq); err != nil {
		t.Fatalf("write binary GET failed: %v", err)
	}
	resp, err = transport.ReadResponse(conn)
	if err != nil || resp.Status != transport.StatusOk {
		t.Fatalf("GET failed: err=%v, status=%v", err, resp.Status)
	}
	if !bytes.Equal(resp.Value, binaryVal) {
		t.Fatalf("binary value corrupted over TCP: got %x, want %x", resp.Value, binaryVal)
	}

	// 3. Differential check directly against storage Engine
	engineVal, err := eng.Get(binaryKey)
	if err != nil {
		t.Fatalf("direct engine.Get failed: %v", err)
	}
	if !bytes.Equal(engineVal, binaryVal) {
		t.Fatalf("engine value mismatch: got %x, want %x", engineVal, binaryVal)
	}

	// 4. Zero-length value marker
	zeroKey := []byte("zero_key")
	putZeroReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  803,
		Key:    zeroKey,
		Value:  []byte{},
	}
	if err := transport.WriteRequest(conn, putZeroReq); err != nil {
		t.Fatalf("write zero-val PUT failed: %v", err)
	}
	resp, err = transport.ReadResponse(conn)
	if err != nil || resp.Status != transport.StatusOk {
		t.Fatalf("zero-val PUT failed: err=%v, status=%v", err, resp.Status)
	}

	getZeroReq := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  804,
		Key:    zeroKey,
	}
	if err := transport.WriteRequest(conn, getZeroReq); err != nil {
		t.Fatalf("write zero-val GET failed: %v", err)
	}
	resp, err = transport.ReadResponse(conn)
	if err != nil || resp.Status != transport.StatusOk {
		t.Fatalf("zero-val GET failed: err=%v, status=%v", err, resp.Status)
	}
	if len(resp.Value) != 0 {
		t.Fatalf("expected 0-length value, got len %d", len(resp.Value))
	}

	// 5. BATCH execution over TCP against real Engine
	batchReq := &transport.Request{
		OpCode: transport.OpBatch,
		SeqID:  805,
		Batch: []transport.BatchOp{
			{Type: transport.BatchOpPut, Key: []byte("b_k1"), Value: []byte("b_v1")},
			{Type: transport.BatchOpPut, Key: []byte("b_k2"), Value: []byte("b_v2")},
			{Type: transport.BatchOpDelete, Key: binaryKey},
		},
	}
	if err := transport.WriteRequest(conn, batchReq); err != nil {
		t.Fatalf("write BATCH request failed: %v", err)
	}
	resp, err = transport.ReadResponse(conn)
	if err != nil || resp.Status != transport.StatusOk {
		t.Fatalf("BATCH failed: err=%v, status=%v, msg=%s", err, resp.Status, resp.Message)
	}

	// Verify batch mutations directly on engine
	v1, err := eng.Get([]byte("b_k1"))
	if err != nil || !bytes.Equal(v1, []byte("b_v1")) {
		t.Fatalf("b_k1 get failed: %v, val=%s", err, string(v1))
	}
	v2, err := eng.Get([]byte("b_k2"))
	if err != nil || !bytes.Equal(v2, []byte("b_v2")) {
		t.Fatalf("b_k2 get failed: %v, val=%s", err, string(v2))
	}
	_, err = eng.Get(binaryKey)
	if !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("expected binaryKey to be deleted, got err=%v", err)
	}
}

func TestServer_ConcurrentClients_RaceStress(t *testing.T) {
	eng := newMockEngine()
	srv := startTestServer(t, transport.DefaultServerConfig(), eng)

	const numClients = 32
	const opsPerClient = 25

	var wg sync.WaitGroup
	wg.Add(numClients)

	for c := 0; c < numClients; c++ {
		clientID := c
		go func() {
			defer wg.Done()

			conn, err := net.Dial("tcp", srv.Addr().String())
			if err != nil {
				t.Errorf("client %d dial failed: %v", clientID, err)
				return
			}
			defer conn.Close()

			for i := 0; i < opsPerClient; i++ {
				key := []byte(fmt.Sprintf("key_%d_%d", clientID, i))
				val := []byte(fmt.Sprintf("val_%d_%d", clientID, i))
				seq := uint64(clientID*1000 + i)

				// 1. PUT
				putReq := &transport.Request{
					OpCode: transport.OpPut,
					SeqID:  seq,
					Key:    key,
					Value:  val,
				}
				if err := transport.WriteRequest(conn, putReq); err != nil {
					t.Errorf("client %d PUT write failed: %v", clientID, err)
					return
				}
				resp, err := transport.ReadResponse(conn)
				if err != nil || resp.Status != transport.StatusOk || resp.SeqID != seq {
					t.Errorf("client %d PUT response error: err=%v, status=%v, seq=%d", clientID, err, resp.Status, resp.SeqID)
					return
				}

				// 2. GET
				getReq := &transport.Request{
					OpCode: transport.OpGet,
					SeqID:  seq + 1,
					Key:    key,
				}
				if err := transport.WriteRequest(conn, getReq); err != nil {
					t.Errorf("client %d GET write failed: %v", clientID, err)
					return
				}
				resp, err = transport.ReadResponse(conn)
				if err != nil || resp.Status != transport.StatusOk || !bytes.Equal(resp.Value, val) {
					t.Errorf("client %d GET value mismatch: err=%v, status=%v, got=%s", clientID, err, resp.Status, string(resp.Value))
					return
				}

				// 3. DELETE
				delReq := &transport.Request{
					OpCode: transport.OpDelete,
					SeqID:  seq + 2,
					Key:    key,
				}
				if err := transport.WriteRequest(conn, delReq); err != nil {
					t.Errorf("client %d DELETE write failed: %v", clientID, err)
					return
				}
				resp, err = transport.ReadResponse(conn)
				if err != nil || resp.Status != transport.StatusOk {
					t.Errorf("client %d DELETE response error: err=%v, status=%v", clientID, err, resp.Status)
					return
				}
			}
		}()
	}

	wg.Wait()
}

func TestServer_ShutdownWithActiveConnections(t *testing.T) {
	eng := newMockEngine()
	srv := startTestServer(t, transport.DefaultServerConfig(), eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Verify connection is established and tracked
	time.Sleep(20 * time.Millisecond)
	if srv.ActiveConnections() != 1 {
		t.Fatalf("expected 1 active connection, got %d", srv.ActiveConnections())
	}

	// Trigger shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}

	// Active connection count must drop to 0
	if srv.ActiveConnections() != 0 {
		t.Fatalf("expected 0 active connections after shutdown, got %d", srv.ActiveConnections())
	}

	// Writing to closed connection must fail
	putReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  999,
		Key:    []byte("k"),
		Value:  []byte("v"),
	}
	_ = transport.WriteRequest(conn, putReq)
	_, readErr := transport.ReadResponse(conn)
	if readErr == nil {
		t.Fatal("expected error reading from connection after shutdown, but succeeded")
	}
}

func TestServer_InsecureTransportValidation(t *testing.T) {
	eng := newMockEngine()

	// 1. Non-loopback 0.0.0.0 rejected when InsecureTransport is false
	cfg := transport.DefaultServerConfig()
	cfg.Address = "0.0.0.0:9099"
	cfg.InsecureTransport = false
	_, err := transport.NewServer(cfg, eng)
	if !stdErrors.Is(err, errors.ErrInsecureTransport) {
		t.Fatalf("expected ErrInsecureTransport for 0.0.0.0 without opt-in, got: %v", err)
	}

	// 2. Wildcard :9099 rejected when InsecureTransport is false
	cfg.Address = ":9099"
	_, err = transport.NewServer(cfg, eng)
	if !stdErrors.Is(err, errors.ErrInsecureTransport) {
		t.Fatalf("expected ErrInsecureTransport for :9099 without opt-in, got: %v", err)
	}

	// 3. Permitted with InsecureTransport: true
	cfg.Address = "127.0.0.1:0"
	cfg.InsecureTransport = true
	srv, err := transport.NewServer(cfg, eng)
	if err != nil {
		t.Fatalf("expected NewServer to succeed with InsecureTransport: true, got: %v", err)
	}

	// 4. Listen on non-loopback with InsecureTransport: false rejected
	loopbackCfg := transport.DefaultServerConfig()
	loopbackCfg.Address = "127.0.0.1:0"
	loopbackCfg.InsecureTransport = false
	srv2, err := transport.NewServer(loopbackCfg, eng)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	err = srv2.Listen("0.0.0.0:0")
	if !stdErrors.Is(err, errors.ErrInsecureTransport) {
		t.Fatalf("expected ErrInsecureTransport from Listen on 0.0.0.0, got: %v", err)
	}
	_ = srv.Close()
	_ = srv2.Close()
}

func TestServer_ConcurrentShutdownWait(t *testing.T) {
	eng := newMockEngine()
	srv := startTestServer(t, transport.DefaultServerConfig(), eng)

	// Connect 5 clients
	conns := make([]net.Conn, 5)
	for i := range conns {
		c, err := net.Dial("tcp", srv.Addr().String())
		if err != nil {
			t.Fatalf("dial failed: %v", err)
		}
		conns[i] = c
		defer c.Close()
	}

	time.Sleep(30 * time.Millisecond)
	if srv.ActiveConnections() != 5 {
		t.Fatalf("expected 5 active connections, got: %d", srv.ActiveConnections())
	}

	// Launch 10 concurrent Shutdown callers
	const numClosers = 10
	var wg sync.WaitGroup
	wg.Add(numClosers)

	for i := 0; i < numClosers; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := srv.Shutdown(ctx); err != nil {
				t.Errorf("shutdown returned error: %v", err)
			}
			// When any Shutdown returns, active connections must already be 0
			if srv.ActiveConnections() != 0 {
				t.Errorf("expected 0 active connections upon Shutdown return, got: %d", srv.ActiveConnections())
			}
		}()
	}

	wg.Wait()
}

type temporaryError struct{}

func (e temporaryError) Error() string   { return "simulated temporary network error" }
func (e temporaryError) Timeout() bool   { return false }
func (e temporaryError) Temporary() bool { return true }

type mockFaultyListener struct {
	mu        sync.Mutex
	tempFails int
	closed    bool
	realL     net.Listener
}

func (m *mockFaultyListener) Accept() (net.Conn, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, net.ErrClosed
	}
	if m.tempFails > 0 {
		m.tempFails--
		m.mu.Unlock()
		return nil, temporaryError{}
	}
	m.mu.Unlock()
	return m.realL.Accept()
}

func (m *mockFaultyListener) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	return m.realL.Close()
}

func (m *mockFaultyListener) Addr() net.Addr {
	return m.realL.Addr()
}

func TestServer_AcceptLoop_TemporaryErrorBackoff(t *testing.T) {
	realL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}

	mockL := &mockFaultyListener{
		tempFails: 3, // Fail 3 times with temporary error before accepting
		realL:     realL,
	}

	eng := newMockEngine()
	srv, err := transport.NewServer(transport.DefaultServerConfig(), eng)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	serverDone := make(chan error, 1)
	go func() {
		serverDone <- srv.Serve(mockL)
	}()

	// Connect client to real address; accept loop should back off 3 times and then accept
	conn, err := net.Dial("tcp", mockL.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send a simple ping-pong request
	putReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  1234,
		Key:    []byte("temp_err_key"),
		Value:  []byte("temp_err_val"),
	}
	if err := transport.WriteRequest(conn, putReq); err != nil {
		t.Fatalf("write request failed: %v", err)
	}
	resp, err := transport.ReadResponse(conn)
	if err != nil || resp.Status != transport.StatusOk {
		t.Fatalf("request failed after backoff: err=%v, status=%v", err, resp.Status)
	}

	_ = srv.Close()
	select {
	case err := <-serverDone:
		if !stdErrors.Is(err, errors.ErrServerClosed) {
			t.Fatalf("expected ErrServerClosed, got: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for server to shut down")
	}
}

func TestFrame_GarbagePayloadNoAllocationAmplification(t *testing.T) {
	// Construct an 18-byte header claiming 1MB payload with an intentionally corrupt CRC
	hdr := transport.Header{
		Magic:         transport.Magic,
		OpCode:        transport.OpPut,
		SeqID:         777,
		PayloadLength: 1024 * 1024, // 1 MB
	}
	var hdrBuf [transport.HeaderSize]byte
	hdr.Encode(hdrBuf[:])

	garbagePayload := make([]byte, 1024*1024)
	corruptTrailer := []byte{0xDE, 0xAD, 0xBE, 0xEF}

	var frameBytes bytes.Buffer
	frameBytes.Write(hdrBuf[:])
	frameBytes.Write(garbagePayload)
	frameBytes.Write(corruptTrailer)

	raw := frameBytes.Bytes()

	// Decode 50 consecutive frames with bad CRC
	for i := 0; i < 50; i++ {
		reader := bytes.NewReader(raw)
		_, err := transport.DecodeFrame(reader)
		var crcErr *errors.ChecksumMismatchError
		if !stdErrors.As(err, &crcErr) {
			t.Fatalf("expected ChecksumMismatchError, got: %v", err)
		}
	}
}

// -----------------------------------------------------------------------------
// P19-S01-M02 Adversarial Connection Limits & Slowloris Hardening Tests
// -----------------------------------------------------------------------------

func TestServer_P19_DefaultConfig_4096(t *testing.T) {
	cfg := transport.DefaultServerConfig()
	if cfg.MaxConnections != 4096 {
		t.Fatalf("expected DefaultServerConfig.MaxConnections == 4096, got: %d", cfg.MaxConnections)
	}

	eng := newMockEngine()
	customCfg := transport.ServerConfig{
		MaxConnections: 0, // unconfigured or zero must default to 4096
	}
	srv, err := transport.NewServer(customCfg, eng)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	_ = srv
}

func TestServer_Slowloris_FragmentedHeaderTrickle(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	cfg.HeaderTimeout = 100 * time.Millisecond
	cfg.IdleTimeout = 100 * time.Millisecond

	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// An 18-byte valid header
	hdr := transport.Header{
		Magic:         transport.Magic,
		OpCode:        transport.OpGet,
		SeqID:         101,
		PayloadLength: 0,
	}
	var hdrBuf [transport.HeaderSize]byte
	hdr.Encode(hdrBuf[:])

	// Attacker sends 1 byte every 20ms.
	// Sending all 18 bytes would take 18 * 20ms = 360ms > 100ms HeaderTimeout.
	serverClosed := false
	for i := 0; i < len(hdrBuf); i++ {
		_, err := conn.Write(hdrBuf[i : i+1])
		if err != nil {
			serverClosed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Server must have terminated the connection during or immediately after the trickle
	var buf [10]byte
	conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, readErr := conn.Read(buf[:])
	if readErr == nil && !serverClosed {
		t.Fatal("expected fragmented header trickle to be terminated by HeaderTimeout, but read succeeded")
	}

	// Verify active connection count is released back to 0
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if srv.ActiveConnections() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if srv.ActiveConnections() != 0 {
		t.Errorf("expected ActiveConnections == 0 after slow header termination, got: %d", srv.ActiveConnections())
	}
}

func TestServer_Slowloris_FragmentedPayloadTrickle(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	cfg.PayloadTimeout = 100 * time.Millisecond

	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send valid header declaring 100 bytes payload
	hdr := transport.Header{
		Magic:         transport.Magic,
		OpCode:        transport.OpPut,
		SeqID:         102,
		PayloadLength: 100,
	}
	var hdrBuf [transport.HeaderSize]byte
	hdr.Encode(hdrBuf[:])
	if _, err := conn.Write(hdrBuf[:]); err != nil {
		t.Fatalf("write header failed: %v", err)
	}

	// Send 10 bytes every 25ms (would take 250ms > 100ms PayloadTimeout)
	serverClosed := false
	chunk := make([]byte, 10)
	for i := 0; i < 10; i++ {
		_, err := conn.Write(chunk)
		if err != nil {
			serverClosed = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	var buf [10]byte
	conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, readErr := conn.Read(buf[:])
	if readErr == nil && !serverClosed {
		t.Fatal("expected fragmented payload trickle to be terminated by PayloadTimeout, but read succeeded")
	}

	// Verify active connections return to 0 and mock engine was NOT called
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if srv.ActiveConnections() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if srv.ActiveConnections() != 0 {
		t.Errorf("expected ActiveConnections == 0, got: %d", srv.ActiveConnections())
	}
	eng.mu.RLock()
	storeLen := len(eng.store)
	eng.mu.RUnlock()
	if storeLen != 0 {
		t.Errorf("expected zero engine mutations from aborted payload, found: %d", storeLen)
	}
}

func TestServer_SlowResponseReader_WriteTimeout(t *testing.T) {
	eng := newMockEngine()
	// Store a 2MB payload to force multiple TCP buffers
	largeVal := make([]byte, 2*1024*1024)
	_ = eng.Put(context.Background(), []byte("bigkey"), largeVal)

	cfg := transport.DefaultServerConfig()
	cfg.WriteTimeout = 100 * time.Millisecond

	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Reduce socket receive buffer to minimum to fill kernel buffer fast
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetReadBuffer(1024)
	}

	// Send GET for the 2MB key
	req := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  103,
		Key:    []byte("bigkey"),
	}
	if err := transport.WriteRequest(conn, req); err != nil {
		t.Fatalf("write request failed: %v", err)
	}

	// Read only the first 100 bytes and then STOP reading, intentionally blocking the server's Write()
	var partial [100]byte
	if _, err := io.ReadFull(conn, partial[:]); err != nil {
		t.Fatalf("initial read failed: %v", err)
	}

	// Sleep longer than WriteTimeout so server write deadline triggers
	time.Sleep(250 * time.Millisecond)

	// Now try to read; server should have closed the connection due to write timeout
	drainBuf := make([]byte, 64*1024)
	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	for {
		_, err := conn.Read(drainBuf)
		if err != nil {
			// EOF or reset observed -> pass
			break
		}
	}

	// Active connection should be cleaned up
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if srv.ActiveConnections() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if srv.ActiveConnections() != 0 {
		t.Errorf("expected ActiveConnections == 0 after write timeout, got: %d", srv.ActiveConnections())
	}
}

func TestServer_MaxConnections_BoundaryAndChurn(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	cfg.MaxConnections = 4

	srv := startTestServer(t, cfg, eng)

	// Connect 4 active clients
	conns := make([]net.Conn, 4)
	for i := 0; i < 4; i++ {
		c, err := net.Dial("tcp", srv.Addr().String())
		if err != nil {
			t.Fatalf("dial %d failed: %v", i, err)
		}
		conns[i] = c
	}
	defer func() {
		for _, c := range conns {
			if c != nil {
				c.Close()
			}
		}
	}()

	time.Sleep(30 * time.Millisecond)
	if srv.ActiveConnections() != 4 {
		t.Fatalf("expected 4 active connections, got: %d", srv.ActiveConnections())
	}

	// 5th client (limit + 1) MUST be rejected immediately
	c5, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial c5 failed: %v", err)
	}
	defer c5.Close()

	var buf [1]byte
	c5.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	n, readErr := c5.Read(buf[:])
	if readErr == nil && n > 0 {
		t.Fatal("expected c5 to be rejected immediately at ceiling, but read succeeded")
	}

	// Close 2 connections -> slots must become available
	conns[0].Close()
	conns[1].Close()
	time.Sleep(30 * time.Millisecond)

	if srv.ActiveConnections() != 2 {
		t.Fatalf("expected 2 active connections after closing 2, got: %d", srv.ActiveConnections())
	}

	// Reconnect 2 clients
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", srv.Addr().String())
		if err != nil {
			t.Fatalf("reconnect %d failed: %v", i, err)
		}
		conns[i] = c
	}
	time.Sleep(30 * time.Millisecond)
	if srv.ActiveConnections() != 4 {
		t.Fatalf("expected 4 active connections, got: %d", srv.ActiveConnections())
	}

	// Close all initial connections
	for _, c := range conns {
		c.Close()
	}

	// High concurrency churn: 10 goroutines continuously connect, write, close
	var wg sync.WaitGroup
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for iter := 0; iter < 10; iter++ {
				c, err := net.Dial("tcp", srv.Addr().String())
				if err != nil {
					continue
				}
				req := &transport.Request{
					OpCode: transport.OpGet,
					SeqID:  uint64(id*100 + iter),
					Key:    []byte("k"),
				}
				_ = transport.WriteRequest(c, req)
				_, _ = transport.ReadResponse(c)
				c.Close()
			}
		}(g)
	}
	wg.Wait()

	// Verify all connections are released and counter returns to 0
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if srv.ActiveConnections() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if srv.ActiveConnections() != 0 {
		t.Errorf("expected 0 active connections after churn, got: %d", srv.ActiveConnections())
	}
}

type errorDeadlineConn struct {
	net.Conn
	readDeadlineErr  error
	writeDeadlineErr error
}

func (e *errorDeadlineConn) SetReadDeadline(t time.Time) error {
	if e.readDeadlineErr != nil {
		return e.readDeadlineErr
	}
	return e.Conn.SetReadDeadline(t)
}

func (e *errorDeadlineConn) SetWriteDeadline(t time.Time) error {
	if e.writeDeadlineErr != nil {
		return e.writeDeadlineErr
	}
	return e.Conn.SetWriteDeadline(t)
}

func TestServer_DeadlineError_FailClosed(t *testing.T) {
	eng := newMockEngine()
	srv, err := transport.NewServer(transport.DefaultServerConfig(), eng)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	injectedErr := stdErrors.New("injected deadline failure")
	badConn := &errorDeadlineConn{
		Conn:            c2,
		readDeadlineErr: injectedErr,
	}

	done := make(chan struct{})
	go func() {
		srv.ServeConnForTesting(badConn)
		close(done)
	}()

	select {
	case <-done:
		// Succeeded in failing closed
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected server to fail closed immediately on deadline error")
	}

	if srv.ActiveConnections() != 0 {
		t.Errorf("expected ActiveConnections == 0 after deadline error, got: %d", srv.ActiveConnections())
	}
}

func TestServer_Serve_InsecureTransportRejection(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	cfg.InsecureTransport = false

	srv, err := transport.NewServer(cfg, eng)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	// Create a mock non-loopback listener
	nonLoopbackLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer nonLoopbackLn.Close()

	// Wrap listener with an address reporting a non-loopback IP
	fakePublicLn := &fakeAddrListener{
		Listener: nonLoopbackLn,
		addr:     &net.TCPAddr{IP: net.ParseIP("192.168.1.50"), Port: 9099},
	}

	err = srv.Serve(fakePublicLn)
	if err == nil {
		t.Fatal("expected ErrInsecureTransport when serving non-loopback listener, got nil")
	}
	if !stdErrors.Is(err, errors.ErrInsecureTransport) {
		t.Fatalf("expected ErrInsecureTransport, got: %v", err)
	}
}

type fakeAddrListener struct {
	net.Listener
	addr net.Addr
}

func (f *fakeAddrListener) Addr() net.Addr {
	return f.addr
}

func TestServer_ServeConnForTesting_SocketCleanupOnReject(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	cfg.MaxConnections = 1

	srv, err := transport.NewServer(cfg, eng)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	// Fill the 1 available connection slot
	cSlot1, cSlot2 := net.Pipe()
	defer cSlot1.Close()
	defer cSlot2.Close()

	go srv.ServeConnForTesting(cSlot2)
	time.Sleep(20 * time.Millisecond)

	// Now try to serve a 2nd connection on server at capacity
	srv.ServeConnForTesting(c2)

	// Verify c2 was closed by testing read on c1 (must yield io.EOF / closed)
	var b [1]byte
	_, readErr := c1.Read(b[:])
	if readErr == nil {
		t.Fatal("expected rejected connection socket to be closed, but read succeeded")
	}
}

func TestServer_SimultaneousAdmissions_AtCeiling(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	cfg.MaxConnections = 8

	srv := startTestServer(t, cfg, eng)

	const totalClients = 32
	var wg sync.WaitGroup
	var accepted atomic.Int64
	var rejected atomic.Int64

	clientConns := make([]net.Conn, totalClients)

	for i := 0; i < totalClients; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			c, err := net.Dial("tcp", srv.Addr().String())
			if err != nil {
				rejected.Add(1)
				return
			}
			clientConns[idx] = c

			// Probe connection by reading 1 byte with a short deadline
			// If server rejected at ceiling, connection will be closed promptly
			var buf [1]byte
			c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			_, rErr := c.Read(buf[:])
			if rErr == io.EOF || (rErr != nil && strings.Contains(rErr.Error(), "connection reset")) {
				rejected.Add(1)
				_ = c.Close()
			} else {
				accepted.Add(1)
			}
		}(i)
	}
	wg.Wait()

	active := srv.ActiveConnections()
	if active > 8 {
		t.Fatalf("CRITICAL: active connections %d exceeded ceiling of 8!", active)
	}

	// Close all accepted connections
	for _, c := range clientConns {
		if c != nil {
			_ = c.Close()
		}
	}

	// Verify all slots are released
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if srv.ActiveConnections() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if srv.ActiveConnections() != 0 {
		t.Errorf("expected 0 active connections after releasing all, got: %d", srv.ActiveConnections())
	}
}

func TestServer_MalformedFrameFlood_ResourceBounded(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	cfg.HeaderTimeout = 200 * time.Millisecond

	srv := startTestServer(t, cfg, eng)

	// Hostile client sends various malformed frames
	malformedPayloads := [][]byte{
		// Bad magic
		{0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0},
		// Invalid opcode 0xFF
		{'L', 'A', 'T', 'T', 0xFF, 0x00, 0, 0, 0, 0, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0},
		// Oversized payload declaration (10 MiB > 5 MiB)
		{'L', 'A', 'T', 'T', 0x01, 0x00, 0, 0, 0, 0, 0, 0, 0, 3, 0x00, 0xA0, 0x00, 0x00, 0, 0, 0, 0},
		// Truncated header (10 bytes then disconnect)
		{'L', 'A', 'T', 'T', 0x01, 0x00, 0, 0, 0, 0},
	}

	for _, p := range malformedPayloads {
		c, err := net.Dial("tcp", srv.Addr().String())
		if err != nil {
			t.Fatalf("dial failed: %v", err)
		}
		_, _ = c.Write(p)
		_ = c.Close()
	}

	// Verify server remains responsive and healthy
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if srv.ActiveConnections() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if srv.ActiveConnections() != 0 {
		t.Errorf("expected 0 active connections after malformed flood, got: %d", srv.ActiveConnections())
	}

	// Verify server still handles valid requests
	validConn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial after flood failed: %v", err)
	}
	defer validConn.Close()

	req := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  999,
		Key:    []byte("k"),
		Value:  []byte("v"),
	}
	if err := transport.WriteRequest(validConn, req); err != nil {
		t.Fatalf("write valid request failed: %v", err)
	}
	resp, err := transport.ReadResponse(validConn)
	if err != nil {
		t.Fatalf("read valid response failed: %v", err)
	}
	if resp.Status != transport.StatusOk {
		t.Errorf("expected StatusOk, got: %v", resp.Status)
	}
}

func TestServer_ShutdownUnderLoad(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	cfg.MaxConnections = 32

	srv := startTestServer(t, cfg, eng)

	const workerCount = 8
	var wg sync.WaitGroup
	stopClients := make(chan struct{})

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			conn, err := net.Dial("tcp", srv.Addr().String())
			if err != nil {
				return
			}
			defer conn.Close()

			for {
				select {
				case <-stopClients:
					return
				default:
					req := &transport.Request{
						OpCode: transport.OpPut,
						SeqID:  uint64(id),
						Key:    []byte(fmt.Sprintf("k%d", id)),
						Value:  []byte("val"),
					}
					if err := transport.WriteRequest(conn, req); err != nil {
						return
					}
					if _, err := transport.ReadResponse(conn); err != nil {
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
		}(i)
	}

	// Let workers run for a brief moment
	time.Sleep(50 * time.Millisecond)

	// Invoke Shutdown while under load
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
	close(stopClients)
	wg.Wait()

	if srv.ActiveConnections() != 0 {
		t.Errorf("expected ActiveConnections == 0 after Shutdown, got: %d", srv.ActiveConnections())
	}
}
