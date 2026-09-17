package transport_test

import (
	"bytes"
	"context"
	stdErrors "errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/transport"
)

// mockEngine satisfies transport.Engine with injectable hooks for error testing.
type mockEngine struct {
	mu    sync.RWMutex
	store map[string][]byte
	putFn func(ctx context.Context, key, val []byte) error
	getFn func(key []byte) ([]byte, error)
	delFn func(ctx context.Context, key []byte) error
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
			name: "OpExists",
			req: &transport.Request{
				OpCode: transport.OpExists,
				SeqID:  201,
				Key:    []byte("any_key"),
			},
			wantMsg: "EXISTS is not implemented",
		},
		{
			name: "OpBatch",
			req: &transport.Request{
				OpCode: transport.OpBatch,
				SeqID:  202,
				Batch: []transport.BatchOp{
					{Type: transport.BatchOpPut, Key: []byte("k"), Value: []byte("v")},
				},
			},
			wantMsg: "BATCH is not implemented",
		},
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
