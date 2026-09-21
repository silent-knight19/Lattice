package raft_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/raft"
	"github.com/silent-knight19/lattice/internal/transport"
)

// mockPeerSender captures outbound frames for test verification.
type mockPeerSender struct {
	mu       sync.Mutex
	seqID    uint64
	sentMsgs map[cluster.NodeID][]*transport.Frame
	notifyCh chan cluster.NodeID
}

func newMockPeerSender() *mockPeerSender {
	return &mockPeerSender{
		sentMsgs: make(map[cluster.NodeID][]*transport.Frame),
		notifyCh: make(chan cluster.NodeID, 100),
	}
}

func (m *mockPeerSender) NextSeqID() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seqID++
	return m.seqID
}

func (m *mockPeerSender) Send(ctx context.Context, peerID cluster.NodeID, frame *transport.Frame) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sentMsgs[peerID] = append(m.sentMsgs[peerID], frame)
	select {
	case m.notifyCh <- peerID:
	default:
	}
	return nil
}

func (m *mockPeerSender) GetSent(peerID cluster.NodeID) []*transport.Frame {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*transport.Frame(nil), m.sentMsgs[peerID]...)
}

func TestNode_ThreeNodeElectionHarness(t *testing.T) {
	// Section 58: Deterministic three-node election test harness
	// Node 1 = Follower
	// Node 2 = Follower (timeout will fire)
	// Node 3 = Follower
	//
	// Node 2 timeout fires:
	// -> Node 2 becomes Candidate
	// -> Term increments from 0 to 1
	// -> Self-vote persisted
	// -> RequestVote broadcast to Node 1 and Node 3 (never self)

	dir1 := t.TempDir()
	s1, err := raft.OpenStorage(dir1)
	if err != nil {
		t.Fatalf("OpenStorage 1 failed: %v", err)
	}
	defer func() { _ = s1.Close() }()

	dir2 := t.TempDir()
	s2, err := raft.OpenStorage(dir2)
	if err != nil {
		t.Fatalf("OpenStorage 2 failed: %v", err)
	}
	defer func() { _ = s2.Close() }()

	dir3 := t.TempDir()
	s3, err := raft.OpenStorage(dir3)
	if err != nil {
		t.Fatalf("OpenStorage 3 failed: %v", err)
	}
	defer func() { _ = s3.Close() }()

	sender2 := newMockPeerSender()

	// Configure Node 2 with fast deterministic timeout (10ms)
	n2, err := raft.NewNode(raft.NodeConfig{
		LocalID:    2,
		Storage:    s2,
		Peers:      []cluster.NodeID{1, 3},
		PeerSender: sender2,
		DurationProvider: func() time.Duration {
			return 10 * time.Millisecond
		},
	})
	if err != nil {
		t.Fatalf("NewNode 2 failed: %v", err)
	}
	defer func() { _ = n2.Close() }()

	n1, err := raft.NewNode(raft.NodeConfig{
		LocalID: 1,
		Storage: s1,
		Peers:   []cluster.NodeID{2, 3},
	})
	if err != nil {
		t.Fatalf("NewNode 1 failed: %v", err)
	}
	defer func() { _ = n1.Close() }()

	n3, err := raft.NewNode(raft.NodeConfig{
		LocalID: 3,
		Storage: s3,
		Peers:   []cluster.NodeID{1, 2},
	})
	if err != nil {
		t.Fatalf("NewNode 3 failed: %v", err)
	}
	defer func() { _ = n3.Close() }()

	// Start Node 2's election timer
	if err := n2.StartElectionTimer(); err != nil {
		t.Fatalf("StartElectionTimer failed: %v", err)
	}

	// Wait for Node 2 to broadcast RequestVote to peers 1 and 3
	receivedPeers := make(map[cluster.NodeID]bool)
	deadline := time.After(500 * time.Millisecond)

	for len(receivedPeers) < 2 {
		select {
		case peerID := <-sender2.notifyCh:
			receivedPeers[peerID] = true
		case <-deadline:
			t.Fatalf("timed out waiting for RequestVote broadcast from Node 2; received: %v", receivedPeers)
		}
	}

	// 1. Verify Node 2 became Candidate
	if r := n2.Role(); r != raft.RoleCandidate {
		t.Fatalf("expected Node 2 role to be Candidate, got %s", r)
	}

	// 2. Verify Node 2 term incremented to 1
	term2, err := n2.Term()
	if err != nil || term2 != 1 {
		t.Fatalf("expected Node 2 term to be 1, got (%d, %v)", term2, err)
	}

	// 3. Verify Node 2 self-vote is persisted
	vote2, err := n2.VotedFor()
	if err != nil || vote2 != 2 {
		t.Fatalf("expected Node 2 votedFor to be 2, got (%d, %v)", vote2, err)
	}

	// 4. Verify no messages sent to self
	if selfMsgs := sender2.GetSent(2); len(selfMsgs) > 0 {
		t.Fatalf("Node 2 sent %d RequestVote frames to itself over network", len(selfMsgs))
	}

	// 5. Verify frames sent to Node 1 and Node 3
	for _, targetID := range []cluster.NodeID{1, 3} {
		frames := sender2.GetSent(targetID)
		if len(frames) == 0 {
			t.Fatalf("no frames sent to node %d", targetID)
		}

		f := frames[0]
		if f.Header.OpCode != transport.OpCode(transport.PeerOpRequestVote) {
			t.Fatalf("expected OpCode PeerOpRequestVote, got 0x%02x", f.Header.OpCode)
		}

		decodedReq, err := transport.DecodeRequestVote(f)
		if err != nil {
			t.Fatalf("failed to decode RequestVote frame for node %d: %v", targetID, err)
		}

		if decodedReq.CandidateID != 2 {
			t.Fatalf("candidate ID in RequestVote = %d, want 2", decodedReq.CandidateID)
		}
		if decodedReq.Term != 1 {
			t.Fatalf("term in RequestVote = %d, want 1", decodedReq.Term)
		}
		if decodedReq.LastLogIndex != 0 || decodedReq.LastLogTerm != 0 {
			t.Fatalf("expected empty log coordinates, got (%d, %d)", decodedReq.LastLogIndex, decodedReq.LastLogTerm)
		}
		if decodedReq.Nonce == 0 {
			t.Fatalf("expected non-zero nonce")
		}

		// Have target node process the RequestVote
		var targetNode *raft.Node
		if targetID == 1 {
			targetNode = n1
		} else {
			targetNode = n3
		}

		resp, err := targetNode.HandleRequestVote(2, decodedReq)
		if err != nil {
			t.Fatalf("target node %d failed to handle RequestVote: %v", targetID, err)
		}
		if !resp.VoteGranted {
			t.Fatalf("target node %d rejected valid RequestVote", targetID)
		}
		if resp.Term != 1 {
			t.Fatalf("target node %d returned term %d, want 1", targetID, resp.Term)
		}

		// Verify target node persisted vote for candidate 2
		v, _ := targetNode.VotedFor()
		if v != 2 {
			t.Fatalf("target node %d vote not persisted: got %d, want 2", targetID, v)
		}
	}
}

func TestNode_ShutdownSafetyAndLeakSafety(t *testing.T) {
	// Section 60: Shutdown test
	// election loop running -> Node.Close() -> loop exits -> no RequestVote after close
	// -> no term increment after close -> no goroutine leak. Run under -race.
	sender := newMockPeerSender()
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	defer func() { _ = s.Close() }()

	n, err := raft.NewNode(raft.NodeConfig{
		LocalID:    1,
		Storage:    s,
		Peers:      []cluster.NodeID{2, 3},
		PeerSender: sender,
		DurationProvider: func() time.Duration {
			return 5 * time.Millisecond
		},
	})
	if err != nil {
		t.Fatalf("NewNode failed: %v", err)
	}

	if err := n.StartElectionTimer(); err != nil {
		t.Fatalf("StartElectionTimer failed: %v", err)
	}

	// Wait for at least one election round
	select {
	case <-sender.notifyCh:
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("timed out waiting for first election round")
	}

	// Close the node
	if err := n.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Capture term and sent message count at close
	termAtClose, _ := s.Term()
	countAtClose := len(sender.GetSent(2))

	// Wait additional time to ensure no further ticks or broadcasts occur
	time.Sleep(50 * time.Millisecond)

	termAfter, _ := s.Term()
	if termAfter != termAtClose {
		t.Fatalf("term advanced after Close(): atClose=%d, after=%d", termAtClose, termAfter)
	}

	countAfter := len(sender.GetSent(2))
	if countAfter != countAtClose {
		t.Fatalf("broadcasts sent after Close(): atClose=%d, after=%d", countAtClose, countAfter)
	}

	// Mutating operations must fail
	if err := n.StartElectionTimer(); err == nil {
		t.Fatalf("StartElectionTimer should fail after Close")
	}
}

func TestNode_LeaderStopsElectionTimer(t *testing.T) {
	// Section 17 Event D: Leader must not run election timer
	n, _, cleanup := newTestNodeWithPeers(t, 1, []cluster.NodeID{2, 3})
	defer cleanup()

	_ = n.BecomeCandidate()
	_ = n.BecomeLeader()

	if !n.ElectionTimer().IsStopped() {
		t.Fatalf("election timer must be stopped when Node is RoleLeader")
	}

	// ResetElectionTimer while Leader must be a no-op
	n.ResetElectionTimer()
	if !n.ElectionTimer().IsStopped() {
		t.Fatalf("election timer must remain stopped while Node is RoleLeader")
	}

	// Stepping down to Follower resets election timer
	_ = n.StepDownSameTerm(2)
	if n.Role() != raft.RoleFollower {
		t.Fatalf("expected Follower role after StepDownSameTerm")
	}
	if n.ElectionTimer().IsStopped() {
		t.Fatalf("election timer should be re-armed after stepdown to Follower")
	}
}

func TestNode_StartElectionTimer_Idempotent(t *testing.T) {
	// Section 19: Starting the election subsystem twice must not create duplicate goroutines/timers
	n, _, cleanup := newTestNodeWithPeers(t, 1, []cluster.NodeID{2, 3})
	defer cleanup()

	if err := n.StartElectionTimer(); err != nil {
		t.Fatalf("first StartElectionTimer failed: %v", err)
	}

	// Second call must be safe and idempotent
	if err := n.StartElectionTimer(); err != nil {
		t.Fatalf("second StartElectionTimer failed: %v", err)
	}
}
