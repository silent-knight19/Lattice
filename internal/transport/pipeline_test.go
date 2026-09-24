package transport_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/transport"
)

// TestPipeline_BasicPipelining sends multiple requests without waiting for intermediate responses.
// Verifies that all requests are accepted, executed, and returned with exact SeqIDs preserved.
func TestPipeline_BasicPipelining(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	const nReqs = 4
	reqs := make([]*transport.Request, nReqs)
	for i := 0; i < nReqs; i++ {
		reqs[i] = &transport.Request{
			OpCode: transport.OpPut,
			SeqID:  uint64(1001 + i),
			Key:    []byte(fmt.Sprintf("key_%d", i)),
			Value:  []byte(fmt.Sprintf("val_%d", i)),
		}
	}

	// Transmit all requests back-to-back without reading any response
	for _, req := range reqs {
		if err := transport.WriteRequest(conn, req); err != nil {
			t.Fatalf("write request (seq %d) failed: %v", req.SeqID, err)
		}
	}

	// Read all responses and correlate by SeqID
	received := make(map[uint64]*transport.Response)
	for i := 0; i < nReqs; i++ {
		resp, err := transport.ReadResponse(conn)
		if err != nil {
			t.Fatalf("read response %d/%d failed: %v", i+1, nReqs, err)
		}
		if resp.Status != transport.StatusOk {
			t.Fatalf("unexpected response status: %v (msg: %s)", resp.Status, resp.Message)
		}
		received[resp.SeqID] = resp
	}

	for _, req := range reqs {
		resp, ok := received[req.SeqID]
		if !ok {
			t.Errorf("missing response for SeqID %d", req.SeqID)
			continue
		}
		if resp.OpCode != req.OpCode {
			t.Errorf("opcode mismatch for SeqID %d: expected %s, got %s", req.SeqID, req.OpCode, resp.OpCode)
		}
	}
}

// TestPipeline_OutOfOrderCompletion proves that responses can be emitted out of order
// when a later request finishes execution before an earlier delayed request.
func TestPipeline_OutOfOrderCompletion(t *testing.T) {
	eng := newMockEngine()

	// Slow down "slow_key" point lookup
	eng.getFn = func(key []byte) ([]byte, error) {
		if string(key) == "slow_key" {
			time.Sleep(80 * time.Millisecond)
			return []byte("slow_value"), nil
		}
		return []byte("fast_value"), nil
	}

	cfg := transport.DefaultServerConfig()
	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Req 1 is slow (SeqID 10)
	req1 := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  10,
		Key:    []byte("slow_key"),
	}
	// Req 2 is fast (SeqID 20)
	req2 := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  20,
		Key:    []byte("fast_key"),
	}

	// Write Req 1 then Req 2 back-to-back
	if err := transport.WriteRequest(conn, req1); err != nil {
		t.Fatalf("write req1 failed: %v", err)
	}
	if err := transport.WriteRequest(conn, req2); err != nil {
		t.Fatalf("write req2 failed: %v", err)
	}

	// First response to arrive MUST be Req 2 (fast), because Req 1 is sleeping
	resp1, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read first response failed: %v", err)
	}
	if resp1.SeqID != 20 {
		t.Fatalf("expected first response to be fast request (SeqID 20), got SeqID %d", resp1.SeqID)
	}
	if !bytes.Equal(resp1.Value, []byte("fast_value")) {
		t.Fatalf("expected fast_value, got %s", string(resp1.Value))
	}

	// Second response to arrive is Req 1 (slow)
	resp2, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read second response failed: %v", err)
	}
	if resp2.SeqID != 10 {
		t.Fatalf("expected second response to be slow request (SeqID 10), got SeqID %d", resp2.SeqID)
	}
	if !bytes.Equal(resp2.Value, []byte("slow_value")) {
		t.Fatalf("expected slow_value, got %s", string(resp2.Value))
	}
}

// TestPipeline_ResponseFrameIntegrity stresses concurrent pipelined requests across multiple
// connections to ensure response frame bytes never interleave and CRCs remain valid under -race.
func TestPipeline_ResponseFrameIntegrity(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	srv := startTestServer(t, cfg, eng)

	const clients = 8
	const opsPerClient = 20

	var wg sync.WaitGroup
	wg.Add(clients)

	for c := 0; c < clients; c++ {
		cid := c
		go func() {
			defer wg.Done()
			conn, err := net.Dial("tcp", srv.Addr().String())
			if err != nil {
				t.Errorf("client %d dial: %v", cid, err)
				return
			}
			defer conn.Close()

			for batch := 0; batch < 4; batch++ {
				var reqs []*transport.Request
				for i := 0; i < opsPerClient; i++ {
					seq := uint64(cid*10000 + batch*1000 + i + 1)
					req := &transport.Request{
						OpCode: transport.OpPut,
						SeqID:  seq,
						Key:    []byte(fmt.Sprintf("k_%d_%d", cid, i)),
						Value:  bytes.Repeat([]byte{byte(i)}, 128),
					}
					reqs = append(reqs, req)
					if err := transport.WriteRequest(conn, req); err != nil {
						t.Errorf("client %d write req %d: %v", cid, seq, err)
						return
					}
				}

				for i := 0; i < opsPerClient; i++ {
					resp, err := transport.ReadResponse(conn)
					if err != nil {
						t.Errorf("client %d read resp %d: %v", cid, i, err)
						return
					}
					if resp.Status != transport.StatusOk {
						t.Errorf("client %d resp status %v: %s", cid, resp.Status, resp.Message)
					}
				}
			}
		}()
	}

	wg.Wait()
}

// TestPipeline_DuplicateSeqIDRejection verifies that two simultaneously active requests
// with the same SeqID on the same connection deterministically reject the duplicate with StatusInvalidRequest.
// Also verifies that once a request finishes, its SeqID may be safely reused.
func TestPipeline_DuplicateSeqIDRejection(t *testing.T) {
	eng := newMockEngine()
	pauseCh := make(chan struct{})
	enteredCh := make(chan struct{}, 1)

	// Delay the first request so it remains active
	eng.putFn = func(ctx context.Context, key, val []byte) error {
		if string(key) == "dup_key" {
			select {
			case enteredCh <- struct{}{}:
			default:
			}
			<-pauseCh
		}
		return nil
	}

	cfg := transport.DefaultServerConfig()
	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send Req 1 with SeqID = 777
	req1 := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  777,
		Key:    []byte("dup_key"),
		Value:  []byte("val1"),
	}
	if err := transport.WriteRequest(conn, req1); err != nil {
		t.Fatalf("write req1: %v", err)
	}

	// Wait until Req 1 enters dispatch
	<-enteredCh

	// Send Req 2 with the SAME SeqID = 777 while Req 1 is still in flight
	req2 := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  777,
		Key:    []byte("other_key"),
		Value:  []byte("val2"),
	}
	if err := transport.WriteRequest(conn, req2); err != nil {
		t.Fatalf("write req2: %v", err)
	}

	// Req 2 should be rejected immediately as duplicate active SeqID
	respErr, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read duplicate response: %v", err)
	}
	if respErr.Status != transport.StatusInvalidRequest {
		t.Fatalf("expected StatusInvalidRequest for duplicate SeqID, got %v", respErr.Status)
	}
	if respErr.SeqID != 777 {
		t.Fatalf("expected response SeqID 777, got %d", respErr.SeqID)
	}

	// Now unpause Req 1
	close(pauseCh)

	// Req 1 completes with StatusOk
	respOk, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read req1 response: %v", err)
	}
	if respOk.Status != transport.StatusOk || respOk.SeqID != 777 {
		t.Fatalf("expected StatusOk for req1 (SeqID 777), got status %v, seq %d", respOk.Status, respOk.SeqID)
	}

	// Now that Req 1 has completely finished, verify SeqID 777 can be reused safely
	req3 := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  777,
		Key:    []byte("reuse_key"),
		Value:  []byte("val3"),
	}
	if err := transport.WriteRequest(conn, req3); err != nil {
		t.Fatalf("write req3: %v", err)
	}
	respReuse, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read reuse response: %v", err)
	}
	if respReuse.Status != transport.StatusOk || respReuse.SeqID != 777 {
		t.Fatalf("expected reuse of SeqID 777 to succeed, got status %v, seq %d", respReuse.Status, respReuse.SeqID)
	}
}

// TestPipeline_LimitBackpressure proves that in-flight requests are strictly bounded
// by MaxInFlightPerConn at all times, exerting TCP backpressure on the sender.
func TestPipeline_LimitBackpressure(t *testing.T) {
	eng := newMockEngine()
	var inFlight atomic.Int64
	var maxObservedInFlight atomic.Int64

	eng.putFn = func(ctx context.Context, key, val []byte) error {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)

		for {
			old := maxObservedInFlight.Load()
			if cur <= old || maxObservedInFlight.CompareAndSwap(old, cur) {
				break
			}
		}

		time.Sleep(30 * time.Millisecond)
		return nil
	}

	cfg := transport.DefaultServerConfig()
	cfg.MaxInFlightPerConn = 2 // Strict limit of 2 in-flight requests
	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	const nReqs = 6
	// Send 6 requests concurrently in pipeline
	for i := 0; i < nReqs; i++ {
		req := &transport.Request{
			OpCode: transport.OpPut,
			SeqID:  uint64(i + 1),
			Key:    []byte(fmt.Sprintf("bp_%d", i)),
			Value:  []byte("val"),
		}
		if err := transport.WriteRequest(conn, req); err != nil {
			t.Fatalf("write req %d: %v", i, err)
		}
	}

	// Drain all responses
	for i := 0; i < nReqs; i++ {
		resp, err := transport.ReadResponse(conn)
		if err != nil {
			t.Fatalf("read resp %d: %v", i, err)
		}
		if resp.Status != transport.StatusOk {
			t.Fatalf("resp %d unexpected status: %v", i, resp.Status)
		}
	}

	peak := maxObservedInFlight.Load()
	if peak > 2 {
		t.Fatalf("observed in-flight %d exceeded MaxInFlightPerConn (2)", peak)
	}
}

// TestPipeline_MixedOperations pipelines a diverse set of commands:
// PUT, GET, EXISTS, BATCH, DELETE, and STATS across a single connection.
func TestPipeline_MixedOperations(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Pre-populate key1
	_ = eng.Put(context.Background(), []byte("key1"), []byte("val1"))

	requests := []*transport.Request{
		{OpCode: transport.OpGet, SeqID: 1, Key: []byte("key1")},
		{OpCode: transport.OpExists, SeqID: 2, Key: []byte("key1")},
		{OpCode: transport.OpExists, SeqID: 3, Key: []byte("missing_key")},
		{OpCode: transport.OpPut, SeqID: 4, Key: []byte("key2"), Value: []byte("val2")},
		{
			OpCode: transport.OpBatch,
			SeqID:  5,
			Batch: []transport.BatchOp{
				{Type: transport.BatchOpPut, Key: []byte("b1"), Value: []byte("bv1")},
				{Type: transport.BatchOpDelete, Key: []byte("batch_key_to_del")},
			},
		},
		{OpCode: transport.OpStats, SeqID: 6},
		{OpCode: transport.OpDelete, SeqID: 7, Key: []byte("key2")},
	}

	for _, req := range requests {
		if err := transport.WriteRequest(conn, req); err != nil {
			t.Fatalf("write req %d: %v", req.SeqID, err)
		}
	}

	resps := make(map[uint64]*transport.Response)
	for i := 0; i < len(requests); i++ {
		resp, err := transport.ReadResponse(conn)
		if err != nil {
			t.Fatalf("read resp %d: %v", i, err)
		}
		resps[resp.SeqID] = resp
	}

	if resps[1].Status != transport.StatusOk || !bytes.Equal(resps[1].Value, []byte("val1")) {
		t.Errorf("GET (seq 1) failed: status=%v, val=%s", resps[1].Status, string(resps[1].Value))
	}
	if resps[2].Status != transport.StatusOk || !resps[2].Exists {
		t.Errorf("EXISTS (seq 2) failed: status=%v, exists=%v", resps[2].Status, resps[2].Exists)
	}
	if resps[3].Status != transport.StatusOk || resps[3].Exists {
		t.Errorf("EXISTS (seq 3) failed: status=%v, exists=%v", resps[3].Status, resps[3].Exists)
	}
	if resps[4].Status != transport.StatusOk {
		t.Errorf("PUT (seq 4) failed: status=%v", resps[4].Status)
	}
	if resps[5].Status != transport.StatusOk {
		t.Errorf("BATCH (seq 5) failed: status=%v", resps[5].Status)
	}
	if resps[6].Status != transport.StatusOk || len(resps[6].Value) == 0 {
		t.Errorf("STATS (seq 6) failed: status=%v", resps[6].Status)
	}
	if resps[7].Status != transport.StatusOk {
		t.Errorf("DELETE (seq 7) failed: status=%v", resps[7].Status)
	}
}

// TestPipeline_ServerShutdownWithInFlight proves that graceful server shutdown
// cancels in-flight requests cleanly and drains connection resources without deadlock.
func TestPipeline_ServerShutdownWithInFlight(t *testing.T) {
	eng := newMockEngine()
	// Slow down PUT
	eng.putFn = func(ctx context.Context, key, val []byte) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
			return nil
		}
	}

	cfg := transport.DefaultServerConfig()
	cfg.ShutdownTimeout = 500 * time.Millisecond
	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send 4 slow requests
	for i := 0; i < 4; i++ {
		req := &transport.Request{
			OpCode: transport.OpPut,
			SeqID:  uint64(i + 1),
			Key:    []byte("slow"),
			Value:  []byte("val"),
		}
		_ = transport.WriteRequest(conn, req)
	}

	// Trigger server shutdown while requests are in flight
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer shutdownCancel()

	shutdownErr := srv.Shutdown(shutdownCtx)
	if shutdownErr != nil && shutdownErr != context.Canceled {
		t.Fatalf("unexpected shutdown error: %v", shutdownErr)
	}

	if srv.ActiveConnections() != 0 {
		t.Fatalf("expected 0 active connections after shutdown, got %d", srv.ActiveConnections())
	}
}

// TestPipeline_ClientDisconnect_ResourceCleanup verifies that abrupt client disconnect
// cleanly terminates in-flight processing and releases all connection resources.
func TestPipeline_ClientDisconnect_ResourceCleanup(t *testing.T) {
	eng := newMockEngine()
	startedCh := make(chan struct{}, 4)

	eng.putFn = func(ctx context.Context, key, val []byte) error {
		startedCh <- struct{}{}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
			return nil
		}
	}

	cfg := transport.DefaultServerConfig()
	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Send 3 requests
	for i := 0; i < 3; i++ {
		req := &transport.Request{
			OpCode: transport.OpPut,
			SeqID:  uint64(i + 1),
			Key:    []byte("disconnect"),
			Value:  []byte("v"),
		}
		_ = transport.WriteRequest(conn, req)
	}

	// Wait for at least one to start
	<-startedCh

	// Abruptly close the client socket
	_ = conn.Close()

	// Verify server untracks the connection promptly
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if srv.ActiveConnections() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if srv.ActiveConnections() != 0 {
		t.Fatalf("expected 0 active connections after client disconnect, got %d", srv.ActiveConnections())
	}
}

// TestPipeline_MaliciousInput verifies robust handling of framing errors and malformed requests
// in a pipelined stream without server panic or corruption.
func TestPipeline_MaliciousInput(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	srv := startTestServer(t, cfg, eng)
	_ = eng.Put(context.Background(), []byte("k"), []byte("v_init"))

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send valid request A
	reqA := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  101,
		Key:    []byte("k"),
		Value:  []byte("v"),
	}
	if err := transport.WriteRequest(conn, reqA); err != nil {
		t.Fatalf("write reqA: %v", err)
	}

	// Send malformed application request B (invalid opcode 0xFF)
	frameB := &transport.Frame{
		Header: transport.Header{
			Magic:         transport.Magic,
			OpCode:        0xFF,
			SeqID:         102,
			PayloadLength: 0,
		},
	}
	if err := transport.EncodeFrame(conn, frameB); err != nil {
		t.Fatalf("encode frameB: %v", err)
	}

	// Send valid request C
	reqC := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  103,
		Key:    []byte("k"),
	}
	if err := transport.WriteRequest(conn, reqC); err != nil {
		t.Fatalf("write reqC: %v", err)
	}

	resps := make(map[uint64]*transport.Response)
	for i := 0; i < 3; i++ {
		resp, err := transport.ReadResponse(conn)
		if err != nil {
			t.Fatalf("read response %d/3: %v", i+1, err)
		}
		resps[resp.SeqID] = resp
	}

	respA := resps[101]
	if respA == nil || respA.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk for reqA, got %v", respA)
	}

	respB := resps[102]
	if respB == nil || respB.Status != transport.StatusInvalidRequest {
		t.Fatalf("expected StatusInvalidRequest for frameB, got %v", respB)
	}

	respC := resps[103]
	if respC == nil || respC.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk for reqC, got %v", respC)
	}
}

// TestPipeline_CausalitySemantics explicitly tests and documents the concurrency model:
// Independent requests pipelined on the same connection execute concurrently without
// an implicit application-level happens-before relationship. A client requiring strict
// read-after-write causality must await previous responses before dispatching dependent reads.
func TestPipeline_CausalitySemantics(t *testing.T) {
	eng := newMockEngine()
	// Pre-populate initial value
	_ = eng.Put(context.Background(), []byte("causal_key"), []byte("initial_value"))

	// Intentionally delay PUT so that a concurrent GET sent in the same pipeline executes first
	putEntered := make(chan struct{}, 1)
	putRelease := make(chan struct{})
	eng.putFn = func(ctx context.Context, key, val []byte) error {
		select {
		case putEntered <- struct{}{}:
		default:
		}
		<-putRelease
		eng.mu.Lock()
		defer eng.mu.Unlock()
		eng.store[string(key)] = val
		return nil
	}

	cfg := transport.DefaultServerConfig()
	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Pipeline: 1. PUT causal_key = updated_value, 2. GET causal_key
	reqPut := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  1001,
		Key:    []byte("causal_key"),
		Value:  []byte("updated_value"),
	}
	reqGet := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  1002,
		Key:    []byte("causal_key"),
	}

	if err := transport.WriteRequest(conn, reqPut); err != nil {
		t.Fatalf("write put: %v", err)
	}
	// Wait until PUT enters its delayed execution
	<-putEntered

	if err := transport.WriteRequest(conn, reqGet); err != nil {
		t.Fatalf("write get: %v", err)
	}

	// Read GET response first: because GET completes while PUT is still delayed,
	// GET observes "initial_value" rather than "updated_value".
	// This proves that pipelined operations do NOT guarantee execution order == send order.
	respGet, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read get response: %v", err)
	}
	if respGet.SeqID != 1002 {
		t.Fatalf("expected GET response (SeqID 1002), got %d", respGet.SeqID)
	}
	if !bytes.Equal(respGet.Value, []byte("initial_value")) {
		t.Fatalf("expected initial_value due to concurrent execution, got %s", string(respGet.Value))
	}

	// Release PUT
	close(putRelease)

	respPut, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read put response: %v", err)
	}
	if respPut.SeqID != 1001 || respPut.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk for PUT (SeqID 1001), got status %v, seq %d", respPut.Status, respPut.SeqID)
	}
}

type mockClusterReadRouter struct {
	mockRouter
	readCalled  atomic.Int32
	routeReadFn func(ctx context.Context, req *transport.Request) (*transport.Response, error)
}

func (m *mockClusterReadRouter) RouteRead(ctx context.Context, req *transport.Request) (*transport.Response, error) {
	m.readCalled.Add(1)
	if m.routeReadFn != nil {
		return m.routeReadFn(ctx, req)
	}
	return &transport.Response{
		OpCode: req.OpCode,
		Status: transport.StatusOk,
		SeqID:  req.SeqID,
		Value:  []byte("cluster_read_val"),
	}, nil
}

// TestPipeline_Clustered verifies that pipelined requests work correctly in cluster mode,
// routing through ProposalRouter and ReadRouter, handling leader redirects cleanly,
// and preserving SeqID correlation on every response.
func TestPipeline_Clustered(t *testing.T) {
	eng := newMockEngine()
	router := &mockClusterReadRouter{}
	router.routeWriteFn = func(ctx context.Context, req *transport.Request) (*transport.Response, error) {
		if string(req.Key) == "follower_key" {
			// Simulate follower redirect
			return &transport.Response{
				OpCode:  req.OpCode,
				Status:  transport.StatusNotLeader,
				SeqID:   req.SeqID,
				Message: "redirect to leader node 2",
			}, nil
		}
		return &transport.Response{
			OpCode: req.OpCode,
			Status: transport.StatusOk,
			SeqID:  req.SeqID,
		}, nil
	}

	cfg := transport.DefaultServerConfig()
	cfg.ClusterMode = true
	cfg.ProposalRouter = router
	cfg.ReadRouter = router
	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Pipeline: 1. PUT follower_key (returns StatusNotLeader), 2. PUT leader_key (returns StatusOk), 3. GET k (returns StatusOk)
	reqs := []*transport.Request{
		{OpCode: transport.OpPut, SeqID: 501, Key: []byte("follower_key"), Value: []byte("v1")},
		{OpCode: transport.OpPut, SeqID: 502, Key: []byte("leader_key"), Value: []byte("v2")},
		{OpCode: transport.OpGet, SeqID: 503, Key: []byte("read_key")},
	}

	for _, req := range reqs {
		if err := transport.WriteRequest(conn, req); err != nil {
			t.Fatalf("write req %d: %v", req.SeqID, err)
		}
	}

	resps := make(map[uint64]*transport.Response)
	for i := 0; i < len(reqs); i++ {
		resp, err := transport.ReadResponse(conn)
		if err != nil {
			t.Fatalf("read resp %d: %v", i, err)
		}
		resps[resp.SeqID] = resp
	}

	if resps[501].Status != transport.StatusNotLeader || resps[501].Message != "redirect to leader node 2" {
		t.Errorf("expected StatusNotLeader for seq 501, got %v (msg: %s)", resps[501].Status, resps[501].Message)
	}
	if resps[502].Status != transport.StatusOk {
		t.Errorf("expected StatusOk for seq 502, got %v", resps[502].Status)
	}
	if resps[503].Status != transport.StatusOk || !bytes.Equal(resps[503].Value, []byte("cluster_read_val")) {
		t.Errorf("expected StatusOk with cluster_read_val for seq 503, got %v (%s)", resps[503].Status, string(resps[503].Value))
	}
}

// TestPipeline_SeqIDReuse_AfterFullResponseCompletion verifies that once a response
// is fully written to the client, the SeqID is released and can immediately be reused.
func TestPipeline_SeqIDReuse_AfterFullResponseCompletion(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	const targetSeqID = uint64(888)

	for iteration := 1; iteration <= 3; iteration++ {
		req := &transport.Request{
			OpCode: transport.OpPut,
			SeqID:  targetSeqID,
			Key:    []byte(fmt.Sprintf("reuse_key_%d", iteration)),
			Value:  []byte(fmt.Sprintf("reuse_val_%d", iteration)),
		}
		if err := transport.WriteRequest(conn, req); err != nil {
			t.Fatalf("iteration %d write failed: %v", iteration, err)
		}

		resp, err := transport.ReadResponse(conn)
		if err != nil {
			t.Fatalf("iteration %d read failed: %v", iteration, err)
		}
		if resp.SeqID != targetSeqID || resp.Status != transport.StatusOk {
			t.Fatalf("iteration %d unexpected response: status=%v, seq=%d", iteration, resp.Status, resp.SeqID)
		}
	}
}

// TestPipeline_SeqIDReuse_WhileResponseBuffered verifies that an active SeqID
// is NOT released when the response has been generated, but is still buffered in the writer channel.
func TestPipeline_SeqIDReuse_WhileResponseBuffered(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	srv := startTestServer(t, cfg, eng)

	hookEntered := make(chan struct{}, 1)
	releaseHook := make(chan struct{})

	srv.SetResponseWriteHookForTesting(func(resp *transport.Response) {
		if resp.SeqID == 999 && resp.Status == transport.StatusOk {
			select {
			case hookEntered <- struct{}{}:
			default:
			}
			<-releaseHook
		}
	})

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Req 1: SeqID = 999
	req1 := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  999,
		Key:    []byte("k1"),
		Value:  []byte("v1"),
	}
	if err := transport.WriteRequest(conn, req1); err != nil {
		t.Fatalf("write req1: %v", err)
	}

	// Wait until Req 1 has executed and entered the response writer hook (buffered/pending write)
	<-hookEntered

	// While response 1 is pending/buffered, attempt to reuse SeqID 999
	req2 := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  999,
		Key:    []byte("k2"),
		Value:  []byte("v2"),
	}
	if err := transport.WriteRequest(conn, req2); err != nil {
		t.Fatalf("write req2: %v", err)
	}

	// Give reader thread a moment to process req2 while writer is still blocked on releaseHook
	time.Sleep(50 * time.Millisecond)

	// Now unblock the writer
	close(releaseHook)

	// Resp 1 should be the original StatusOk
	resp1, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read resp1: %v", err)
	}
	if resp1.SeqID != 999 || resp1.Status != transport.StatusOk {
		t.Fatalf("expected resp1 StatusOk, got %v (seq %d)", resp1.Status, resp1.SeqID)
	}

	// Resp 2 MUST be StatusInvalidRequest (duplicate active seq_id: 999)
	resp2, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read resp2: %v", err)
	}
	if resp2.SeqID != 999 || resp2.Status != transport.StatusInvalidRequest {
		t.Fatalf("expected resp2 StatusInvalidRequest for buffered reuse attempt, got %v (seq %d)", resp2.Status, resp2.SeqID)
	}

	// Clear testing hook
	srv.SetResponseWriteHookForTesting(nil)

	// Now that both responses have been completely written, SeqID 999 must be reusable
	req3 := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  999,
		Key:    []byte("k3"),
		Value:  []byte("v3"),
	}
	if err := transport.WriteRequest(conn, req3); err != nil {
		t.Fatalf("write req3: %v", err)
	}
	resp3, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read resp3: %v", err)
	}
	if resp3.SeqID != 999 || resp3.Status != transport.StatusOk {
		t.Fatalf("expected resp3 StatusOk on reuse after completion, got %v", resp3.Status)
	}
}

// TestPipeline_SeqIDReuse_WhileWriterBlocked verifies that while the response writer
// is blocked writing bytes to the socket, any reuse of that SeqID is rejected.
func TestPipeline_SeqIDReuse_WhileWriterBlocked(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	srv := startTestServer(t, cfg, eng)

	writerBlocked := make(chan struct{})
	writerEntered := make(chan struct{}, 1)

	srv.SetResponseWriteHookForTesting(func(resp *transport.Response) {
		if resp.SeqID == 7777 && resp.Status == transport.StatusOk {
			select {
			case writerEntered <- struct{}{}:
			default:
			}
			<-writerBlocked
		}
	})

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send Req 1 with SeqID 7777
	req1 := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  7777,
		Key:    []byte("wb1"),
		Value:  []byte("val1"),
	}
	if err := transport.WriteRequest(conn, req1); err != nil {
		t.Fatalf("write req1: %v", err)
	}

	// Await writer entering hook
	<-writerEntered

	// Send Req 2 with same SeqID 7777
	req2 := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  7777,
		Key:    []byte("wb2"),
		Value:  []byte("val2"),
	}
	if err := transport.WriteRequest(conn, req2); err != nil {
		t.Fatalf("write req2: %v", err)
	}

	// Reader will process req2 and see SeqID 7777 is still active, queuing a rejection envelope.
	time.Sleep(50 * time.Millisecond)

	// Unblock writer
	close(writerBlocked)

	resp1, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read resp1: %v", err)
	}
	if resp1.SeqID != 7777 || resp1.Status != transport.StatusOk {
		t.Fatalf("expected resp1 StatusOk, got status %v", resp1.Status)
	}

	resp2, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read resp2: %v", err)
	}
	if resp2.SeqID != 7777 || resp2.Status != transport.StatusInvalidRequest {
		t.Fatalf("expected resp2 StatusInvalidRequest, got %v", resp2.Status)
	}

	srv.SetResponseWriteHookForTesting(nil)
}

// TestPipeline_SeqIDReuse_ConcurrentRepeatedReuse tests multiple concurrent goroutines
// attempting to claim and execute the same SeqID simultaneously on one connection.
// Exactly one request must succeed, and all other duplicate attempts must be rejected.
func TestPipeline_SeqIDReuse_ConcurrentRepeatedReuse(t *testing.T) {
	eng := newMockEngine()
	// Add slight delay to engine put to widen the race window
	eng.putFn = func(ctx context.Context, key, val []byte) error {
		time.Sleep(30 * time.Millisecond)
		return nil
	}

	cfg := transport.DefaultServerConfig()
	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	const targetSeqID = uint64(54321)
	const concurrentAttempts = 10

	var writeMu sync.Mutex
	var startWg sync.WaitGroup
	startWg.Add(1)

	var sendWg sync.WaitGroup
	sendWg.Add(concurrentAttempts)

	for i := 0; i < concurrentAttempts; i++ {
		go func(idx int) {
			defer sendWg.Done()
			startWg.Wait()

			req := &transport.Request{
				OpCode: transport.OpPut,
				SeqID:  targetSeqID,
				Key:    []byte(fmt.Sprintf("conc_key_%d", idx)),
				Value:  []byte(fmt.Sprintf("conc_val_%d", idx)),
			}
			writeMu.Lock()
			_ = transport.WriteRequest(conn, req)
			writeMu.Unlock()
		}(i)
	}

	// Release all goroutines simultaneously
	startWg.Done()
	sendWg.Wait()

	// Read all responses
	var okCount, rejectCount int
	for i := 0; i < concurrentAttempts; i++ {
		resp, err := transport.ReadResponse(conn)
		if err != nil {
			t.Fatalf("read resp %d: %v", i, err)
		}
		if resp.SeqID != targetSeqID {
			t.Fatalf("expected seq %d, got %d", targetSeqID, resp.SeqID)
		}
		if resp.Status == transport.StatusOk {
			okCount++
		} else if resp.Status == transport.StatusInvalidRequest {
			rejectCount++
		} else {
			t.Fatalf("unexpected status %v", resp.Status)
		}
	}

	// Exactly 1 must have owned the SeqID, the other 9 must have been rejected
	if okCount != 1 {
		t.Fatalf("expected exactly 1 StatusOk, got %d", okCount)
	}
	if rejectCount != concurrentAttempts-1 {
		t.Fatalf("expected %d StatusInvalidRequest, got %d", concurrentAttempts-1, rejectCount)
	}
}

// TestPipeline_Lifecycle_ServerShutdownWithFullPipeline tests server shutdown
// while a saturated pipeline of requests is actively executing.
func TestPipeline_Lifecycle_ServerShutdownWithFullPipeline(t *testing.T) {
	eng := newMockEngine()
	eng.putFn = func(ctx context.Context, key, val []byte) error {
		select {
		case <-time.After(50 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	cfg := transport.DefaultServerConfig()
	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	const nReqs = 16
	for i := 0; i < nReqs; i++ {
		req := &transport.Request{
			OpCode: transport.OpPut,
			SeqID:  uint64(i + 1),
			Key:    []byte(fmt.Sprintf("shut_key_%d", i)),
			Value:  []byte("val"),
		}
		if err := transport.WriteRequest(conn, req); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	// Trigger server shutdown while requests are in flight
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()

	shutdownErr := srv.Shutdown(shutdownCtx)
	if shutdownErr != nil && !errors.Is(shutdownErr, context.Canceled) {
		t.Fatalf("shutdown failed: %v", shutdownErr)
	}
}

// TestPipeline_Lifecycle_ClientDisconnectWithFullPipeline tests immediate client disconnect
// after sending a pipeline of requests, proving server cleans up all in-flight resources without leaks.
func TestPipeline_Lifecycle_ClientDisconnectWithFullPipeline(t *testing.T) {
	eng := newMockEngine()
	eng.putFn = func(ctx context.Context, key, val []byte) error {
		select {
		case <-time.After(30 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	cfg := transport.DefaultServerConfig()
	srv := startTestServer(t, cfg, eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	const nReqs = 20
	for i := 0; i < nReqs; i++ {
		req := &transport.Request{
			OpCode: transport.OpPut,
			SeqID:  uint64(i + 1),
			Key:    []byte(fmt.Sprintf("dc_key_%d", i)),
			Value:  []byte("val"),
		}
		_ = transport.WriteRequest(conn, req)
	}

	// Abruptly close client connection
	_ = conn.Close()

	// Wait for server to process disconnection and clean up
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.ActiveConnections() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if srv.ActiveConnections() != 0 {
		t.Fatalf("expected 0 active connections after disconnect, got %d", srv.ActiveConnections())
	}
}
