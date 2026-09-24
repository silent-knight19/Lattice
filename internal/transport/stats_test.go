package transport_test

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	"github.com/silent-knight19/lattice/internal/transport"
)

type mockClusterStatsRouter struct {
	stats transport.ClusterStats
}

func (m *mockClusterStatsRouter) RouteWrite(ctx context.Context, req *transport.Request) (*transport.Response, error) {
	return nil, nil
}

func (m *mockClusterStatsRouter) ClusterStats() transport.ClusterStats {
	return m.stats
}

func TestCodec_StatsRequest_Validation(t *testing.T) {
	// 1. Valid STATS request (zero payload frame)
	frame := &transport.Frame{
		Header: transport.Header{
			Magic:         transport.Magic,
			OpCode:        transport.OpStats,
			PayloadLength: 0,
			SeqID:         123,
		},
		Payload: nil,
	}
	req, err := transport.DecodeRequest(frame)
	if err != nil {
		t.Fatalf("expected valid STATS frame to decode, got: %v", err)
	}
	if req.OpCode != transport.OpStats || req.SeqID != 123 {
		t.Errorf("decoded request mismatch: op=%v seq=%d", req.OpCode, req.SeqID)
	}

	// 2. Invalid STATS request (non-zero payload frame rejected)
	badFrame := &transport.Frame{
		Header: transport.Header{
			Magic:         transport.Magic,
			OpCode:        transport.OpStats,
			PayloadLength: 4,
			SeqID:         124,
		},
		Payload: []byte("fail"),
	}
	if _, err := transport.DecodeRequest(badFrame); err == nil {
		t.Fatalf("expected STATS frame with non-zero payload to fail decoding, got nil")
	}
}

func TestServer_Stats_Standalone(t *testing.T) {
	eng := newMockEngine()
	eng.store["k1"] = []byte("v1")
	eng.store["k2"] = []byte("v2")

	srv := startTestServer(t, transport.DefaultServerConfig(), eng)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	req := &transport.Request{
		OpCode: transport.OpStats,
		SeqID:  777,
	}
	if err := transport.WriteRequest(conn, req); err != nil {
		t.Fatalf("write STATS request failed: %v", err)
	}

	resp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read STATS response failed: %v", err)
	}

	if resp.SeqID != 777 {
		t.Errorf("expected SeqID 777, got %d", resp.SeqID)
	}
	if resp.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk, got 0x%02x (%s)", resp.Status, resp.Message)
	}

	// Verify response size is bounded
	if len(resp.Value) == 0 {
		t.Fatalf("expected non-empty stats JSON in resp.Value")
	}
	if len(resp.Value) > 4096 {
		t.Errorf("expected stats payload < 4KB, got %d bytes", len(resp.Value))
	}

	var snap transport.StatsSnapshot
	if err := json.Unmarshal(resp.Value, &snap); err != nil {
		t.Fatalf("failed to unmarshal StatsSnapshot JSON: %v, raw: %s", err, string(resp.Value))
	}

	if snap.Engine.State != "open" {
		t.Errorf("expected engine state 'open', got %q", snap.Engine.State)
	}
	if snap.Memory.ActiveMemTableEntries != 2 {
		t.Errorf("expected 2 active entries from mockEngine, got %d", snap.Memory.ActiveMemTableEntries)
	}
	if snap.Connections.Active < 1 {
		t.Errorf("expected at least 1 active connection, got %d", snap.Connections.Active)
	}
	if snap.Cluster.Enabled {
		t.Errorf("expected cluster.enabled=false in standalone mode")
	}
}

func TestServer_Stats_ClusterMode(t *testing.T) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	cfg.ClusterMode = true
	srv := startTestServer(t, cfg, eng)

	// Attach mock cluster stats router
	clusterRouter := &mockClusterStatsRouter{
		stats: transport.ClusterStats{
			Enabled:     true,
			Role:        "leader",
			Term:        5,
			LocalID:     1,
			LeaderID:    1,
			CommitIndex: 100,
			LastApplied: 100,
		},
	}
	srv.SetProposalRouter(clusterRouter)

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	req := &transport.Request{
		OpCode: transport.OpStats,
		SeqID:  888,
	}
	if err := transport.WriteRequest(conn, req); err != nil {
		t.Fatalf("write STATS request failed: %v", err)
	}

	resp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read STATS response failed: %v", err)
	}

	if resp.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk, got 0x%02x (%s)", resp.Status, resp.Message)
	}

	var snap transport.StatsSnapshot
	if err := json.Unmarshal(resp.Value, &snap); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	if !snap.Cluster.Enabled {
		t.Errorf("expected cluster.enabled=true")
	}
	if snap.Cluster.Role != "leader" {
		t.Errorf("expected role 'leader', got %q", snap.Cluster.Role)
	}
	if snap.Cluster.Term != 5 {
		t.Errorf("expected term 5, got %d", snap.Cluster.Term)
	}
	if snap.Cluster.CommitIndex != 100 {
		t.Errorf("expected commit_index 100, got %d", snap.Cluster.CommitIndex)
	}
}

func TestCodec_StatsResponse_PayloadBounds(t *testing.T) {
	// 1. Oversized response payload rejected on encode (> 4096 bytes)
	oversizedResp := &transport.Response{
		OpCode: transport.OpStats,
		Status: transport.StatusOk,
		SeqID:  999,
		Value:  make([]byte, transport.MaxStatsPayloadLength+1),
	}
	if _, err := transport.EncodeResponse(oversizedResp); err == nil {
		t.Fatalf("expected EncodeResponse to reject oversized STATS payload, got nil")
	}

	// 2. Oversized response payload rejected on decode (> 4096 bytes)
	oversizedFrame := &transport.Frame{
		Header: transport.Header{
			Magic:         transport.Magic,
			OpCode:        transport.OpStats,
			Status:        transport.StatusOk,
			SeqID:         999,
			PayloadLength: transport.MaxStatsPayloadLength + 1,
		},
		Payload: make([]byte, transport.MaxStatsPayloadLength+1),
	}
	if _, err := transport.DecodeResponse(oversizedFrame); err == nil {
		t.Fatalf("expected DecodeResponse to reject oversized STATS frame, got nil")
	}

	// 3. Valid sized payload accepted (<= 4096 bytes)
	validResp := &transport.Response{
		OpCode: transport.OpStats,
		Status: transport.StatusOk,
		SeqID:  1000,
		Value:  []byte(`{"engine":{"state":"open"}}`),
	}
	frame, err := transport.EncodeResponse(validResp)
	if err != nil {
		t.Fatalf("expected EncodeResponse to succeed for valid STATS payload: %v", err)
	}
	decoded, err := transport.DecodeResponse(frame)
	if err != nil {
		t.Fatalf("expected DecodeResponse to succeed for valid STATS frame: %v", err)
	}
	if string(decoded.Value) != string(validResp.Value) {
		t.Errorf("decoded value mismatch: got %q, want %q", string(decoded.Value), string(validResp.Value))
	}
}
