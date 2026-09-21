package raft_test

import (
	"context"
	stdErrors "errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/raft"
	"github.com/silent-knight19/lattice/internal/transport"
)

// mockHeartbeatSender tracks outbound frames with concurrency safety.
type mockHeartbeatSender struct {
	mu       sync.Mutex
	seqID    uint64
	sentMsgs map[cluster.NodeID][]*transport.Frame
}

func newMockHeartbeatSender() *mockHeartbeatSender {
	return &mockHeartbeatSender{
		sentMsgs: make(map[cluster.NodeID][]*transport.Frame),
	}
}

func (m *mockHeartbeatSender) NextSeqID() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seqID++
	return m.seqID
}

func (m *mockHeartbeatSender) Send(ctx context.Context, peerID cluster.NodeID, frame *transport.Frame) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sentMsgs[peerID] = append(m.sentMsgs[peerID], frame)
	return nil
}

func (m *mockHeartbeatSender) GetSent(peerID cluster.NodeID) []*transport.Frame {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*transport.Frame(nil), m.sentMsgs[peerID]...)
}

func (m *mockHeartbeatSender) TotalSent() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := 0
	for _, frames := range m.sentMsgs {
		total += len(frames)
	}
	return total
}

func newTestHeartbeatNode(t *testing.T, localID cluster.NodeID, peers []cluster.NodeID, sender raft.PeerSender, hook raft.TransitionHook) (*raft.Node, *raft.Storage) {
	t.Helper()
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}

	n, err := raft.NewNode(raft.NodeConfig{
		LocalID:           localID,
		Storage:           s,
		Peers:             peers,
		PeerSender:        sender,
		TransitionHook:    hook,
		HeartbeatInterval: 50 * time.Millisecond,
	})
	if err != nil {
		_ = s.Close()
		t.Fatalf("NewNode failed: %v", err)
	}

	t.Cleanup(func() {
		_ = n.Close()
		_ = s.Close()
	})

	return n, s
}

// -----------------------------------------------------------------------------
// Section 22: Three-Node Heartbeat Test
// -----------------------------------------------------------------------------

func TestNode_Heartbeat_ThreeNodeRecurringCadence(t *testing.T) {
	sender := newMockHeartbeatSender()
	node1, s1 := newTestHeartbeatNode(t, 1, []cluster.NodeID{2, 3}, sender, nil)

	// Set initial term to 2
	if err := s1.SetTerm(2); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}

	// Transition Node 1 Candidate -> Leader
	if err := node1.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	term, err := node1.Term()
	if err != nil {
		t.Fatalf("Term failed: %v", err)
	}
	if term != 3 {
		t.Fatalf("expected term 3, got %d", term)
	}

	if err := node1.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}

	// Verify scheduler started
	if !node1.HeartbeatRunning() {
		t.Fatalf("expected HeartbeatRunning == true after BecomeLeader")
	}

	// Wait 130ms: enough for T=0 immediate heartbeat + ~2 periodic 50ms ticks
	time.Sleep(130 * time.Millisecond)

	// Invariant 1: No heartbeat sent to self (Node 1)
	selfFrames := sender.GetSent(1)
	if len(selfFrames) != 0 {
		t.Fatalf("expected 0 frames sent to self, got %d", len(selfFrames))
	}

	// Invariant 2: Node 2 and Node 3 receive heartbeats
	f2 := sender.GetSent(2)
	f3 := sender.GetSent(3)

	if len(f2) < 2 {
		t.Fatalf("expected at least 2 heartbeats to node 2, got %d", len(f2))
	}
	if len(f3) < 2 {
		t.Fatalf("expected at least 2 heartbeats to node 3, got %d", len(f3))
	}

	// Validate heartbeat frame contents
	nonces := make(map[uint64]struct{})
	var prevSeq uint64

	for i, frame := range f2 {
		if transport.PeerMessageType(frame.Header.OpCode) != transport.PeerOpAppendEntries {
			t.Fatalf("frame %d: expected OpCode PeerOpAppendEntries, got 0x%02x", i, frame.Header.OpCode)
		}
		if frame.Header.SeqID <= prevSeq {
			t.Fatalf("frame %d: sequence ID %d not monotonic (previous %d)", i, frame.Header.SeqID, prevSeq)
		}
		prevSeq = frame.Header.SeqID

		req, err := transport.DecodeAppendEntries(frame)
		if err != nil {
			t.Fatalf("frame %d: DecodeAppendEntries failed: %v", i, err)
		}

		if req.Term != 3 {
			t.Fatalf("frame %d: expected Term 3, got %d", i, req.Term)
		}
		if req.LeaderID != 1 {
			t.Fatalf("frame %d: expected LeaderID 1, got %d", i, req.LeaderID)
		}
		if len(req.Entries) != 0 {
			t.Fatalf("frame %d: expected 0 entries (empty heartbeat), got %d", i, len(req.Entries))
		}
		if req.Nonce == 0 {
			t.Fatalf("frame %d: expected non-zero nonce", i)
		}
		if _, seen := nonces[req.Nonce]; seen {
			t.Fatalf("frame %d: duplicate nonce %d", i, req.Nonce)
		}
		nonces[req.Nonce] = struct{}{}
	}
}

// -----------------------------------------------------------------------------
// Section 23: Follower Timer Reset Test
// -----------------------------------------------------------------------------

func TestNode_Heartbeat_FollowerTimerReset(t *testing.T) {
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}

	// Election timeout of 80ms
	durProvider := func() time.Duration { return 80 * time.Millisecond }
	follower, err := raft.NewNode(raft.NodeConfig{
		LocalID:          2,
		Storage:          s,
		Peers:            []cluster.NodeID{1},
		DurationProvider: durProvider,
	})
	if err != nil {
		_ = s.Close()
		t.Fatalf("NewNode failed: %v", err)
	}
	defer func() {
		_ = follower.Close()
		_ = s.Close()
	}()

	if err := follower.StartElectionTimer(); err != nil {
		t.Fatalf("StartElectionTimer failed: %v", err)
	}

	// Send heartbeats every 35ms for 200ms (>2x election timeout of 80ms)
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})

	go func() {
		defer close(doneCh)
		ticker := time.NewTicker(35 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				req := &transport.AppendEntriesRequest{
					Term:         1,
					LeaderID:     1,
					PrevLogIndex: 0,
					PrevLogTerm:  0,
					Nonce:        12345,
				}
				resp, err := follower.HandleAppendEntries(1, req)
				if err != nil {
					t.Errorf("HandleAppendEntries failed: %v", err)
					return
				}
				if !resp.Success {
					t.Errorf("expected success=true, got false")
					return
				}
			}
		}
	}()

	time.Sleep(200 * time.Millisecond)
	close(stopCh)
	<-doneCh

	// Follower must remain Follower in term 1 (no election started!)
	if follower.Role() != raft.RoleFollower {
		t.Fatalf("expected role RoleFollower, got %s", follower.Role())
	}
	term, err := follower.Term()
	if err != nil {
		t.Fatalf("Term failed: %v", err)
	}
	if term != 1 {
		t.Fatalf("expected term 1, got %d", term)
	}
}

// -----------------------------------------------------------------------------
// Section 24: Stale Heartbeat Test
// -----------------------------------------------------------------------------

func TestNode_Heartbeat_StaleHeartbeatRejected(t *testing.T) {
	node, s := newTestHeartbeatNode(t, 2, []cluster.NodeID{1}, nil, nil)

	// Set follower term to 5
	if err := s.SetTerm(5); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}

	// Incoming heartbeat with term 4 < 5
	req := &transport.AppendEntriesRequest{
		Term:         4,
		LeaderID:     1,
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Nonce:        999,
	}

	resp, err := node.HandleAppendEntries(1, req)
	if err != nil {
		t.Fatalf("HandleAppendEntries returned error: %v", err)
	}

	if resp.Success {
		t.Fatalf("expected Success == false for stale term")
	}
	if resp.Term != 5 {
		t.Fatalf("expected response Term 5, got %d", resp.Term)
	}

	// Role and leaderID must remain unchanged
	if node.Role() != raft.RoleFollower {
		t.Fatalf("expected role Follower, got %s", node.Role())
	}
	if node.LeaderID() != cluster.NodeIDNil {
		t.Fatalf("expected leaderID nil, got %d", node.LeaderID())
	}
}

// -----------------------------------------------------------------------------
// Section 25: Higher-Term Heartbeat Test (Follower, Candidate, Leader)
// -----------------------------------------------------------------------------

func TestNode_Heartbeat_HigherTermStepdown(t *testing.T) {
	roles := []struct {
		name      string
		setupRole func(t *testing.T, n *raft.Node, s *raft.Storage)
	}{
		{
			name: "receiver is Follower",
			setupRole: func(t *testing.T, n *raft.Node, s *raft.Storage) {
				// already follower
			},
		},
		{
			name: "receiver is Candidate",
			setupRole: func(t *testing.T, n *raft.Node, s *raft.Storage) {
				if err := n.BecomeCandidate(); err != nil {
					t.Fatalf("BecomeCandidate failed: %v", err)
				}
				if n.Role() != raft.RoleCandidate {
					t.Fatalf("setup failed: expected Candidate")
				}
			},
		},
		{
			name: "receiver is Leader",
			setupRole: func(t *testing.T, n *raft.Node, s *raft.Storage) {
				if err := n.BecomeCandidate(); err != nil {
					t.Fatalf("BecomeCandidate failed: %v", err)
				}
				if err := n.BecomeLeader(); err != nil {
					t.Fatalf("BecomeLeader failed: %v", err)
				}
				if n.Role() != raft.RoleLeader {
					t.Fatalf("setup failed: expected Leader")
				}
				if !n.HeartbeatRunning() {
					t.Fatalf("setup failed: expected HeartbeatRunning == true")
				}
			},
		},
	}

	for _, tc := range roles {
		t.Run(tc.name, func(t *testing.T) {
			node, s := newTestHeartbeatNode(t, 1, []cluster.NodeID{2}, nil, nil)
			if err := s.SetTerm(5); err != nil {
				t.Fatalf("SetTerm failed: %v", err)
			}

			tc.setupRole(t, node, s)

			currTerm, err := node.Term()
			if err != nil {
				t.Fatalf("node.Term failed: %v", err)
			}
			higherTerm := uint64(currTerm + 1)

			// Receive heartbeat with higher term from peer 2
			req := &transport.AppendEntriesRequest{
				Term:         higherTerm,
				LeaderID:     2,
				PrevLogIndex: 0,
				PrevLogTerm:  0,
				Nonce:        888,
			}

			resp, err := node.HandleAppendEntries(2, req)
			if err != nil {
				t.Fatalf("HandleAppendEntries failed: %v", err)
			}

			if !resp.Success {
				t.Fatalf("expected Success == true, got false")
			}
			if resp.Term != higherTerm {
				t.Fatalf("expected response Term %d, got %d", higherTerm, resp.Term)
			}

			// Invariants check
			if node.Role() != raft.RoleFollower {
				t.Fatalf("expected RoleFollower, got %s", node.Role())
			}
			if node.LeaderID() != 2 {
				t.Fatalf("expected LeaderID 2, got %d", node.LeaderID())
			}

			durableTerm, err := s.Term()
			if err != nil {
				t.Fatalf("s.Term failed: %v", err)
			}
			if uint64(durableTerm) != higherTerm {
				t.Fatalf("expected durable term %d, got %d", higherTerm, durableTerm)
			}

			votedFor, err := s.VotedFor()
			if err != nil {
				t.Fatalf("s.VotedFor failed: %v", err)
			}
			if votedFor != cluster.NodeIDNil {
				t.Fatalf("expected votedFor cleared to NodeIDNil, got %d", votedFor)
			}

			// If it was Leader, scheduler must have stopped
			if node.HeartbeatRunning() {
				t.Fatalf("expected HeartbeatRunning == false after higher-term stepdown")
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Section 26: Higher-Term Persistence Failure Test
// -----------------------------------------------------------------------------

func TestNode_Heartbeat_HigherTermPersistenceFailure(t *testing.T) {
	node, s := newTestHeartbeatNode(t, 1, []cluster.NodeID{2}, nil, nil)
	if err := s.SetTerm(5); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}

	// Close storage underneath to induce persistence error
	_ = s.Close()

	req := &transport.AppendEntriesRequest{
		Term:         6,
		LeaderID:     2,
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Nonce:        777,
	}

	resp, err := node.HandleAppendEntries(2, req)
	if err == nil {
		t.Fatalf("expected persistence failure error, got nil (resp=%+v)", resp)
	}

	if resp != nil {
		t.Fatalf("expected nil response on error, got %+v", resp)
	}
}

// -----------------------------------------------------------------------------
// Section 27: Same-Term Candidate Step-Down Test
// -----------------------------------------------------------------------------

func TestNode_Heartbeat_SameTermCandidateStepDown(t *testing.T) {
	node, s := newTestHeartbeatNode(t, 1, []cluster.NodeID{2}, nil, nil)

	// Candidate in term 7 (has voted for self)
	if err := s.SetTerm(6); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}
	if err := node.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}

	term, err := node.Term()
	if err != nil {
		t.Fatalf("Term failed: %v", err)
	}
	if term != 7 {
		t.Fatalf("expected term 7, got %d", term)
	}

	votedFor, err := s.VotedFor()
	if err != nil {
		t.Fatalf("VotedFor failed: %v", err)
	}
	if votedFor != 1 {
		t.Fatalf("expected votedFor 1, got %d", votedFor)
	}

	// Valid same-term heartbeat from peer 2
	req := &transport.AppendEntriesRequest{
		Term:         7,
		LeaderID:     2,
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Nonce:        666,
	}

	resp, err := node.HandleAppendEntries(2, req)
	if err != nil {
		t.Fatalf("HandleAppendEntries failed: %v", err)
	}

	if !resp.Success {
		t.Fatalf("expected Success == true, got false")
	}

	// Candidate -> Follower
	if node.Role() != raft.RoleFollower {
		t.Fatalf("expected RoleFollower, got %s", node.Role())
	}
	if node.LeaderID() != 2 {
		t.Fatalf("expected LeaderID 2, got %d", node.LeaderID())
	}

	// Term remains 7
	termAfter, err := node.Term()
	if err != nil {
		t.Fatalf("Term failed: %v", err)
	}
	if termAfter != 7 {
		t.Fatalf("expected term 7, got %d", termAfter)
	}

	// Vote MUST NOT be cleared on same-term stepdown
	votedForAfter, err := s.VotedFor()
	if err != nil {
		t.Fatalf("VotedFor failed: %v", err)
	}
	if votedForAfter != 1 {
		t.Fatalf("expected votedFor 1 preserved, got %d", votedForAfter)
	}
}

// -----------------------------------------------------------------------------
// Section 28: Same-Term Leader Conflict Test
// -----------------------------------------------------------------------------

func TestNode_Heartbeat_SameTermLeaderConflict(t *testing.T) {
	node, _ := newTestHeartbeatNode(t, 1, []cluster.NodeID{2}, nil, nil)

	// Promote Node 1 to Leader
	if err := node.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := node.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}
	term, err := node.Term()
	if err != nil {
		t.Fatalf("Term failed: %v", err)
	}

	if !node.HeartbeatRunning() {
		t.Fatalf("expected scheduler running")
	}

	// Foreign leader sends valid heartbeat in same term
	req := &transport.AppendEntriesRequest{
		Term:         uint64(term),
		LeaderID:     2,
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Nonce:        555,
	}

	resp, err := node.HandleAppendEntries(2, req)
	if err != nil {
		t.Fatalf("HandleAppendEntries failed: %v", err)
	}

	if !resp.Success {
		t.Fatalf("expected success")
	}

	// Node 1 must step down to Follower
	if node.Role() != raft.RoleFollower {
		t.Fatalf("expected RoleFollower, got %s", node.Role())
	}
	if node.LeaderID() != 2 {
		t.Fatalf("expected LeaderID 2, got %d", node.LeaderID())
	}

	// Scheduler must be stopped
	if node.HeartbeatRunning() {
		t.Fatalf("expected HeartbeatRunning == false after leader conflict stepdown")
	}
}

// -----------------------------------------------------------------------------
// Section 29: Unknown / Self Heartbeat Tests
// -----------------------------------------------------------------------------

func TestNode_Heartbeat_UnknownAndSelfPeerRejected(t *testing.T) {
	node, _ := newTestHeartbeatNode(t, 1, []cluster.NodeID{2}, nil, nil)

	tests := []struct {
		name       string
		fromPeerID cluster.NodeID
		req        *transport.AppendEntriesRequest
		wantErr    error
	}{
		{
			name:       "unknown peer",
			fromPeerID: 99,
			req: &transport.AppendEntriesRequest{
				Term:     1,
				LeaderID: 99,
				Nonce:    1,
			},
			wantErr: errors.ErrPeerNotFound,
		},
		{
			name:       "self sender",
			fromPeerID: 1,
			req: &transport.AppendEntriesRequest{
				Term:     1,
				LeaderID: 1,
				Nonce:    1,
			},
			wantErr: errors.ErrRaftSelfVoteRPC,
		},
		{
			name:       "sender and leader ID mismatch",
			fromPeerID: 2,
			req: &transport.AppendEntriesRequest{
				Term:     1,
				LeaderID: 3,
				Nonce:    1,
			},
			wantErr: errors.ErrRaftSenderMismatch,
		},
		{
			name:       "invalid sender ID 0",
			fromPeerID: 0,
			req: &transport.AppendEntriesRequest{
				Term:     1,
				LeaderID: 2,
				Nonce:    1,
			},
			wantErr: errors.ErrInvalidNodeID,
		},
		{
			name:       "nil request",
			fromPeerID: 2,
			req:        nil,
			wantErr:    errors.ErrNilReceiver,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := node.HandleAppendEntries(tc.fromPeerID, tc.req)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !stdErrors.Is(err, tc.wantErr) {
				t.Fatalf("expected error %v, got %v", tc.wantErr, err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Section 30: Duplicate Heartbeat Idempotence
// -----------------------------------------------------------------------------

func TestNode_Heartbeat_DuplicateHeartbeatsIdempotent(t *testing.T) {
	node, s := newTestHeartbeatNode(t, 1, []cluster.NodeID{2}, nil, nil)

	req := &transport.AppendEntriesRequest{
		Term:         1,
		LeaderID:     2,
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Nonce:        444,
	}

	// Fire 30 heartbeats concurrently
	var wg sync.WaitGroup
	errCh := make(chan error, 30)

	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := node.HandleAppendEntries(2, req)
			if err != nil {
				errCh <- err
				return
			}
			if !resp.Success {
				errCh <- fmt.Errorf("expected success=true")
				return
			}
		}()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent heartbeat failed: %v", err)
	}

	if node.Role() != raft.RoleFollower {
		t.Fatalf("expected RoleFollower, got %s", node.Role())
	}
	if node.LeaderID() != 2 {
		t.Fatalf("expected LeaderID 2, got %d", node.LeaderID())
	}
	term, err := s.Term()
	if err != nil {
		t.Fatalf("Term failed: %v", err)
	}
	if term != 1 {
		t.Fatalf("expected term 1, got %d", term)
	}
}

// -----------------------------------------------------------------------------
// Section 15: Preceding Log Coordinates
// -----------------------------------------------------------------------------

func TestNode_Heartbeat_PrecedingLogMatching(t *testing.T) {
	node, s := newTestHeartbeatNode(t, 1, []cluster.NodeID{2}, nil, nil)
	if err := s.SetTerm(1); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}

	// Append log entry at index 1 with term 1
	entry := raft.LogEntry{
		Index: 1,
		Term:  1,
		Type:  transport.PeerEntryNormal,
		Data:  []byte("cmd1"),
	}
	if err := s.Append(entry); err != nil {
		t.Fatalf("Append failed: %v", err)
	}

	// Case A: PrevLogIndex == 0 -> match
	reqA := &transport.AppendEntriesRequest{
		Term:         1,
		LeaderID:     2,
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Nonce:        101,
	}
	respA, err := node.HandleAppendEntries(2, reqA)
	if err != nil || !respA.Success {
		t.Fatalf("Case A failed: resp=%+v, err=%v", respA, err)
	}

	// Case B: PrevLogIndex == 1, PrevLogTerm == 1 -> match
	reqB := &transport.AppendEntriesRequest{
		Term:         1,
		LeaderID:     2,
		PrevLogIndex: 1,
		PrevLogTerm:  1,
		Nonce:        102,
	}
	respB, err := node.HandleAppendEntries(2, reqB)
	if err != nil || !respB.Success {
		t.Fatalf("Case B failed: resp=%+v, err=%v", respB, err)
	}
	if respB.MatchIndex != 1 {
		t.Fatalf("expected MatchIndex 1, got %d", respB.MatchIndex)
	}

	// Case C: PrevLogIndex == 2, PrevLogTerm == 1 (log shorter than prevLogIndex) -> mismatch
	reqC := &transport.AppendEntriesRequest{
		Term:         1,
		LeaderID:     2,
		PrevLogIndex: 2,
		PrevLogTerm:  1,
		Nonce:        103,
	}
	respC, err := node.HandleAppendEntries(2, reqC)
	if err != nil {
		t.Fatalf("Case C error: %v", err)
	}
	if respC.Success {
		t.Fatalf("Case C: expected Success == false for log shorter than PrevLogIndex")
	}

	// Case D: PrevLogIndex == 1, PrevLogTerm == 2 (term mismatch at index 1) -> mismatch
	reqD := &transport.AppendEntriesRequest{
		Term:         1,
		LeaderID:     2,
		PrevLogIndex: 1,
		PrevLogTerm:  2,
		Nonce:        104,
	}
	respD, err := node.HandleAppendEntries(2, reqD)
	if err != nil {
		t.Fatalf("Case D error: %v", err)
	}
	if respD.Success {
		t.Fatalf("Case D: expected Success == false for term mismatch")
	}
}

// -----------------------------------------------------------------------------
// Section 32: Transport Wire Frame Test
// -----------------------------------------------------------------------------

func TestNode_Heartbeat_WireTransportDispatch(t *testing.T) {
	sender := newMockHeartbeatSender()
	node, _ := newTestHeartbeatNode(t, 1, []cluster.NodeID{2}, sender, nil)

	// Encode an incoming AppendEntries frame from Node 2
	req := &transport.AppendEntriesRequest{
		Term:         1,
		LeaderID:     2,
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Nonce:        333,
	}
	frame, err := transport.EncodeAppendEntries(req, 42)
	if err != nil {
		t.Fatalf("EncodeAppendEntries failed: %v", err)
	}

	// Dispatch via HandlePeerFrame
	if err := node.HandlePeerFrame(2, frame); err != nil {
		t.Fatalf("HandlePeerFrame failed: %v", err)
	}

	// Node 1 should respond to Node 2 with PeerOpAppendEntriesResponse
	sentTo2 := sender.GetSent(2)
	if len(sentTo2) != 1 {
		t.Fatalf("expected 1 response frame sent to Node 2, got %d", len(sentTo2))
	}

	respFrame := sentTo2[0]
	if transport.PeerMessageType(respFrame.Header.OpCode) != transport.PeerOpAppendEntriesResponse {
		t.Fatalf("expected PeerOpAppendEntriesResponse, got 0x%02x", respFrame.Header.OpCode)
	}

	aeResp, err := transport.DecodeAppendEntriesResponse(respFrame)
	if err != nil {
		t.Fatalf("DecodeAppendEntriesResponse failed: %v", err)
	}
	if !aeResp.Success {
		t.Fatalf("expected response Success == true")
	}
	if aeResp.Term != 1 {
		t.Fatalf("expected response Term 1, got %d", aeResp.Term)
	}

	// Test response frame dispatch: incoming higher-term response causes stepdown
	respFrameIn, err := transport.EncodeAppendEntriesResponse(&transport.AppendEntriesResponse{
		Term:       10,
		Success:    false,
		MatchIndex: 0,
	}, 99)
	if err != nil {
		t.Fatalf("EncodeAppendEntriesResponse failed: %v", err)
	}

	if err := node.HandlePeerFrame(2, respFrameIn); err != nil {
		t.Fatalf("HandlePeerFrame response dispatch failed: %v", err)
	}

	term, err := node.Term()
	if err != nil {
		t.Fatalf("Term failed: %v", err)
	}
	if term != 10 {
		t.Fatalf("expected term 10 after higher-term response, got %d", term)
	}
}
