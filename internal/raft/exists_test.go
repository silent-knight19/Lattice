package raft

import (
	"context"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/transport"
)

func TestRouter_RouteRead_Exists(t *testing.T) {
	ctx := context.Background()

	// 1. Follower receives OpExists -> returns StatusNotLeader with redirect
	topFollower := newTestTopology(t, 1, map[cluster.NodeID]string{
		1: "127.0.0.1:9101",
		2: "127.0.0.1:9102",
	})
	sm1 := newBatchMockSM()
	n1, _, cleanup1 := newTestNodeWithSM(t, 1, sm1, 10)
	defer cleanup1()
	n1.SetTopology(topFollower)

	// In follower role with known leader 2
	n1.mu.Lock()
	n1.role = RoleFollower
	n1.leaderID = 2
	n1.mu.Unlock()

	router := NewProposalRouter(n1, topFollower, sm1)
	req := &transport.Request{
		OpCode: transport.OpExists,
		SeqID:  101,
		Key:    []byte("test_key"),
	}

	resp, err := router.RouteRead(ctx, req)
	if err != nil {
		t.Fatalf("RouteRead failed: %v", err)
	}
	if resp.Status != transport.StatusNotLeader {
		t.Fatalf("expected StatusNotLeader from follower, got 0x%02x (%s)", resp.Status, resp.Message)
	}
	if resp.LeaderID != 2 || resp.LeaderAddr != "127.0.0.1:9102" {
		t.Fatalf("expected redirect to node 2, got %d (%s)", resp.LeaderID, resp.LeaderAddr)
	}

	// 2. Candidate receives OpExists -> returns StatusNotLeader
	n1.mu.Lock()
	n1.role = RoleCandidate
	n1.mu.Unlock()

	resp, err = router.RouteRead(ctx, req)
	if err != nil {
		t.Fatalf("RouteRead candidate failed: %v", err)
	}
	if resp.Status != transport.StatusNotLeader {
		t.Fatalf("expected StatusNotLeader from candidate, got 0x%02x", resp.Status)
	}

	// 3. Leader receives OpExists on a single-node cluster (can achieve ReadIndex quorum immediately)
	singleTop := newTestTopology(t, 1, map[cluster.NodeID]string{1: "127.0.0.1:9101"})
	smLeader := newBatchMockSM()
	nLeader, _, cleanupLeader := newTestNodeWithSM(t, 1, smLeader, 10)
	defer cleanupLeader()
	nLeader.SetTopology(singleTop)

	if err := nLeader.BecomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := nLeader.BecomeLeader(); err != nil {
		t.Fatal(err)
	}

	// Wait for election no-op entry to apply
	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := nLeader.WaitForApplied(waitCtx, 1); err != nil {
		t.Fatalf("WaitForApplied failed: %v", err)
	}

	leaderRouter := NewProposalRouter(nLeader, singleTop, smLeader)

	// A. Check non-existent key
	req = &transport.Request{
		OpCode: transport.OpExists,
		SeqID:  201,
		Key:    []byte("absent_key"),
	}
	resp, err = leaderRouter.RouteRead(ctx, req)
	if err != nil {
		t.Fatalf("RouteRead leader failed: %v", err)
	}
	if resp.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk, got 0x%02x (%s)", resp.Status, resp.Message)
	}
	if resp.Exists {
		t.Fatalf("expected Exists=false for absent key, got true")
	}

	// B. Propose PUT key and wait for applied
	putReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  202,
		Key:    []byte("replicated_key"),
		Value:  []byte("replicated_val"),
	}
	putResp, err := leaderRouter.RouteWrite(ctx, putReq)
	if err != nil || putResp.Status != transport.StatusOk {
		t.Fatalf("RouteWrite failed: resp=%v, err=%v", putResp, err)
	}
	if err := nLeader.WaitForApplied(waitCtx, 2); err != nil {
		t.Fatalf("WaitForApplied failed: %v", err)
	}

	// C. Check existence of replicated key
	req = &transport.Request{
		OpCode: transport.OpExists,
		SeqID:  203,
		Key:    []byte("replicated_key"),
	}
	resp, err = leaderRouter.RouteRead(ctx, req)
	if err != nil {
		t.Fatalf("RouteRead leader failed: %v", err)
	}
	if resp.Status != transport.StatusOk || !resp.Exists {
		t.Fatalf("expected StatusOk with Exists=true, got status=0x%02x exists=%v", resp.Status, resp.Exists)
	}

	// D. Propose DELETE key and wait for applied
	delReq := &transport.Request{
		OpCode: transport.OpDelete,
		SeqID:  204,
		Key:    []byte("replicated_key"),
	}
	delResp, err := leaderRouter.RouteWrite(ctx, delReq)
	if err != nil || delResp.Status != transport.StatusOk {
		t.Fatalf("RouteWrite delete failed: resp=%v, err=%v", delResp, err)
	}
	if err := nLeader.WaitForApplied(waitCtx, 3); err != nil {
		t.Fatalf("WaitForApplied failed: %v", err)
	}

	// E. Check existence of deleted key -> must be false
	req = &transport.Request{
		OpCode: transport.OpExists,
		SeqID:  205,
		Key:    []byte("replicated_key"),
	}
	resp, err = leaderRouter.RouteRead(ctx, req)
	if err != nil {
		t.Fatalf("RouteRead leader failed: %v", err)
	}
	if resp.Status != transport.StatusOk || resp.Exists {
		t.Fatalf("expected StatusOk with Exists=false, got status=0x%02x exists=%v", resp.Status, resp.Exists)
	}
}
