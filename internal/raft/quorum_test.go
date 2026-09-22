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

// quorumMockSender captures outbound frames for test assertion.
type quorumMockSender struct {
	mu       sync.Mutex
	seqID    uint64
	sentMsgs map[cluster.NodeID][]*transport.Frame
}

func newQuorumMockSender() *quorumMockSender {
	return &quorumMockSender{
		sentMsgs: make(map[cluster.NodeID][]*transport.Frame),
	}
}

func (m *quorumMockSender) NextSeqID() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seqID++
	return m.seqID
}

func (m *quorumMockSender) Send(ctx context.Context, peerID cluster.NodeID, frame *transport.Frame) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sentMsgs[peerID] = append(m.sentMsgs[peerID], frame)
	return nil
}

func (m *quorumMockSender) GetSent(peerID cluster.NodeID) []*transport.Frame {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*transport.Frame(nil), m.sentMsgs[peerID]...)
}

func (m *quorumMockSender) TotalSent() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := 0
	for _, frames := range m.sentMsgs {
		total += len(frames)
	}
	return total
}

func newTestQuorumNode(t *testing.T, localID cluster.NodeID, peers []cluster.NodeID, sender raft.PeerSender, hook raft.TransitionHook) (*raft.Node, *raft.Storage) {
	t.Helper()
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}

	n, err := raft.NewNode(raft.NodeConfig{
		LocalID:        localID,
		Storage:        s,
		Peers:          peers,
		PeerSender:     sender,
		TransitionHook: hook,
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
// Section 26.A: Quorum Formula Tests
// -----------------------------------------------------------------------------

func TestNode_QuorumCalculation(t *testing.T) {
	tests := []struct {
		name       string
		localID    cluster.NodeID
		peers      []cluster.NodeID
		wantQuorum int
	}{
		{
			name:       "N=1 (single node)",
			localID:    1,
			peers:      nil,
			wantQuorum: 1, // floor(1/2) + 1 = 1
		},
		{
			name:       "N=2 cluster",
			localID:    1,
			peers:      []cluster.NodeID{2},
			wantQuorum: 2, // floor(2/2) + 1 = 2
		},
		{
			name:       "N=3 cluster",
			localID:    1,
			peers:      []cluster.NodeID{2, 3},
			wantQuorum: 2, // floor(3/2) + 1 = 2
		},
		{
			name:       "N=4 cluster",
			localID:    1,
			peers:      []cluster.NodeID{2, 3, 4},
			wantQuorum: 3, // floor(4/2) + 1 = 3
		},
		{
			name:       "N=5 cluster",
			localID:    1,
			peers:      []cluster.NodeID{2, 3, 4, 5},
			wantQuorum: 3, // floor(5/2) + 1 = 3
		},
		{
			name:       "duplicate peer in config deduplicated",
			localID:    1,
			peers:      []cluster.NodeID{2, 2, 3},
			wantQuorum: 2, // 2 unique peers + self = 3 => floor(3/2) + 1 = 2
		},
		{
			name:       "self included in peers list excluded",
			localID:    1,
			peers:      []cluster.NodeID{1, 2, 3},
			wantQuorum: 2, // 2 remote peers + self = 3 => floor(3/2) + 1 = 2
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n, _ := newTestQuorumNode(t, tc.localID, tc.peers, nil, nil)
			if q := n.QuorumSize(); q != tc.wantQuorum {
				t.Fatalf("QuorumSize = %d, want %d", q, tc.wantQuorum)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Section 26.B: Self-Vote Counting
// -----------------------------------------------------------------------------

func TestNode_SelfVoteCounting(t *testing.T) {
	sender := newQuorumMockSender()
	n, _ := newTestQuorumNode(t, 1, []cluster.NodeID{2, 3}, sender, nil)

	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}

	// Local self-vote is counted as exactly 1
	if votes := n.GrantedVotesCount(); votes != 1 {
		t.Fatalf("GrantedVotesCount = %d, want 1", votes)
	}

	// Self-vote is never sent over the network
	if total := sender.TotalSent(); total != 0 {
		t.Fatalf("expected 0 network frames for self-vote, got %d", total)
	}
}

// -----------------------------------------------------------------------------
// Section 26.M: Single-Node N=1 Election
// -----------------------------------------------------------------------------

func TestNode_SingleNodeElection_ImmediatePromotion(t *testing.T) {
	sender := newQuorumMockSender()
	var (
		mu          sync.Mutex
		transitions []string
	)
	hook := func(from, to raft.Role, term raft.Term) {
		mu.Lock()
		defer mu.Unlock()
		transitions = append(transitions, fmt.Sprintf("%s->%s(T%d)", from, to, term))
	}

	n, _ := newTestQuorumNode(t, 1, nil, sender, hook)

	// Single node election timeout
	if err := n.StartElectionTimer(); err != nil {
		t.Fatalf("StartElectionTimer failed: %v", err)
	}

	// Trigger election timeout via internal timer countdown
	// Wait for election to transition node to Leader
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n.Role() == raft.RoleLeader {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if r := n.Role(); r != raft.RoleLeader {
		t.Fatalf("Role = %s, want RoleLeader", r)
	}
	if leaderID := n.LeaderID(); leaderID != 1 {
		t.Fatalf("LeaderID = %d, want 1", leaderID)
	}

	// Verify no network messages sent (no remote peers)
	if total := sender.TotalSent(); total != 0 {
		t.Fatalf("expected 0 sent frames for N=1 cluster, got %d", total)
	}

	// Verify transition hook sequence contains promotion to Leader
	mu.Lock()
	leaderTransitionFound := false
	for _, tr := range transitions {
		if tr == "Candidate->Leader(T1)" {
			leaderTransitionFound = true
			break
		}
	}
	trCopy := append([]string(nil), transitions...)
	mu.Unlock()
	if !leaderTransitionFound {
		t.Fatalf("expected Candidate->Leader transition hook, got: %v", trCopy)
	}
}

// -----------------------------------------------------------------------------
// Section 26.C: Happy-Path 3-Node Election
// -----------------------------------------------------------------------------

func TestNode_ThreeNodeQuorumElection_HappyPath(t *testing.T) {
	sender := newQuorumMockSender()
	var (
		mu          sync.Mutex
		transitions []string
	)
	hook := func(from, to raft.Role, term raft.Term) {
		mu.Lock()
		defer mu.Unlock()
		transitions = append(transitions, fmt.Sprintf("%s->%s(T%d)", from, to, term))
	}

	// Candidate is node 1, peers are 2 and 3 (N=3, quorum=2)
	n, s := newTestQuorumNode(t, 1, []cluster.NodeID{2, 3}, sender, hook)

	// Put 2 log entries in storage in term 1 to verify leader replication state
	if err := s.SetTerm(1); err != nil {
		t.Fatalf("SetTerm 1 failed: %v", err)
	}
	entry1 := raft.LogEntry{Index: 1, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("cmd1")}
	entry2 := raft.LogEntry{Index: 2, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("cmd2")}
	if err := s.Append(entry1); err != nil {
		t.Fatalf("Append 1 failed: %v", err)
	}
	if err := s.Append(entry2); err != nil {
		t.Fatalf("Append 2 failed: %v", err)
	}

	// BecomeCandidate advances term 1 -> 2
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}

	term, _ := n.Term()
	if term != 2 {
		t.Fatalf("term = %d, want 2", term)
	}

	// Peer 2 grants vote for Term 2
	respPeer2 := &transport.RequestVoteResponse{
		Term:        2,
		VoteGranted: true,
	}
	if err := n.HandleRequestVoteResponse(2, respPeer2); err != nil {
		t.Fatalf("HandleRequestVoteResponse peer 2 failed: %v", err)
	}

	// Quorum reached! (Self = 1, Peer 2 = 1 => 2/3 >= quorum 2)
	if r := n.Role(); r != raft.RoleLeader {
		t.Fatalf("Role = %s, want RoleLeader", r)
	}
	if leaderID := n.LeaderID(); leaderID != 1 {
		t.Fatalf("LeaderID = %d, want 1", leaderID)
	}
	currTerm, _ := n.Term()
	if currTerm != 2 {
		t.Fatalf("Term after leader transition = %d, want 2", currTerm)
	}

	// Verify leader volatile replication state (Section 26.Q):
	// no-op appended at index 3, so nextIndex = 3 + 1 = 4
	// matchIndex = 0
	nextIdx := n.NextIndex()
	matchIdx := n.MatchIndex()
	if len(nextIdx) != 2 || nextIdx[2] != 4 || nextIdx[3] != 4 {
		t.Fatalf("unexpected nextIndex: %+v", nextIdx)
	}
	if len(matchIdx) != 2 || matchIdx[2] != 0 || matchIdx[3] != 0 {
		t.Fatalf("unexpected matchIndex: %+v", matchIdx)
	}

	// Verify the election no-op is durable at index 3 in term 2.
	noop, err := s.Entry(3)
	if err != nil {
		t.Fatalf("no-op Entry(3) failed: %v", err)
	}
	if noop.Term != 2 || noop.Type != transport.PeerEntryNoop {
		t.Fatalf("unexpected no-op entry: %+v", noop)
	}

	// Verify immediate heartbeat broadcast (Section 26.R):
	// Exactly one empty AppendEntries frame sent to each peer (2 and 3),
	// anchored at the no-op (PrevLog 3/2).
	frames2 := sender.GetSent(2)
	frames3 := sender.GetSent(3)
	if len(frames2) != 1 || len(frames3) != 1 {
		t.Fatalf("expected 1 heartbeat per peer, got peer2=%d, peer3=%d", len(frames2), len(frames3))
	}

	ae2, err := transport.DecodeAppendEntries(frames2[0])
	if err != nil {
		t.Fatalf("DecodeAppendEntries peer 2 failed: %v", err)
	}
	if ae2.Term != 2 || ae2.LeaderID != 1 || ae2.PrevLogIndex != 3 || ae2.PrevLogTerm != 2 || len(ae2.Entries) != 0 {
		t.Fatalf("unexpected AppendEntries payload: %+v", ae2)
	}
	if ae2.Nonce == 0 {
		t.Fatalf("expected non-zero nonce in heartbeat")
	}

	// Peer 3's late response arrives after leadership is established
	respPeer3 := &transport.RequestVoteResponse{
		Term:        2,
		VoteGranted: true,
	}
	if err := n.HandleRequestVoteResponse(3, respPeer3); err != nil {
		t.Fatalf("HandleRequestVoteResponse peer 3 failed: %v", err)
	}

	// Verify no second leadership transition or duplicate heartbeat
	mu.Lock()
	countLeaderTransitions := 0
	for _, tr := range transitions {
		if tr == "Candidate->Leader(T2)" {
			countLeaderTransitions++
		}
	}
	mu.Unlock()
	if countLeaderTransitions != 1 {
		t.Fatalf("expected exactly 1 leader transition hook, got %d", countLeaderTransitions)
	}

	if len(sender.GetSent(2)) != 1 || len(sender.GetSent(3)) != 1 {
		t.Fatalf("expected no duplicate heartbeats after late vote")
	}
}

// -----------------------------------------------------------------------------
// Section 26.D: Duplicate Vote Deduplication
// -----------------------------------------------------------------------------

func TestNode_DuplicateVote_Deduplication(t *testing.T) {
	sender := newQuorumMockSender()
	var (
		mu          sync.Mutex
		transitions []string
	)
	hook := func(from, to raft.Role, term raft.Term) {
		mu.Lock()
		defer mu.Unlock()
		transitions = append(transitions, fmt.Sprintf("%s->%s(T%d)", from, to, term))
	}

	// 5-node cluster: local=1, peers=2,3,4,5 (quorum=3)
	n, _ := newTestQuorumNode(t, 1, []cluster.NodeID{2, 3, 4, 5}, sender, hook)

	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	// Initial count: self = 1
	if votes := n.GrantedVotesCount(); votes != 1 {
		t.Fatalf("votes = %d, want 1", votes)
	}

	respGranted := &transport.RequestVoteResponse{
		Term:        1,
		VoteGranted: true,
	}

	// Peer 2 grants vote -> count = 2
	if err := n.HandleRequestVoteResponse(2, respGranted); err != nil {
		t.Fatalf("first vote grant failed: %v", err)
	}
	if votes := n.GrantedVotesCount(); votes != 2 {
		t.Fatalf("votes after peer 2 = %d, want 2", votes)
	}

	// Peer 2 sends duplicate response -> count remains 2
	if err := n.HandleRequestVoteResponse(2, respGranted); err != nil {
		t.Fatalf("duplicate vote grant failed: %v", err)
	}
	if votes := n.GrantedVotesCount(); votes != 2 {
		t.Fatalf("votes after duplicate peer 2 = %d, want 2", votes)
	}
	if r := n.Role(); r != raft.RoleCandidate {
		t.Fatalf("expected RoleCandidate, got %s", r)
	}

	// Peer 3 grants vote -> count = 3 -> quorum reached -> Leader!
	if err := n.HandleRequestVoteResponse(3, respGranted); err != nil {
		t.Fatalf("peer 3 vote grant failed: %v", err)
	}
	if r := n.Role(); r != raft.RoleLeader {
		t.Fatalf("expected RoleLeader, got %s", r)
	}

	// Sending duplicate from Peer 3 after leadership
	if err := n.HandleRequestVoteResponse(3, respGranted); err != nil {
		t.Fatalf("post-leader duplicate failed: %v", err)
	}

	mu.Lock()
	leaderCount := 0
	for _, tr := range transitions {
		if tr == "Candidate->Leader(T1)" {
			leaderCount++
		}
	}
	mu.Unlock()
	if leaderCount != 1 {
		t.Fatalf("expected 1 leader transition, got %d", leaderCount)
	}
}

// -----------------------------------------------------------------------------
// Section 26.E: False Vote Rejected
// -----------------------------------------------------------------------------

func TestNode_FalseVote_Rejected(t *testing.T) {
	n, _ := newTestQuorumNode(t, 1, []cluster.NodeID{2, 3}, nil, nil)

	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}

	respRejected := &transport.RequestVoteResponse{
		Term:        1,
		VoteGranted: false,
	}

	if err := n.HandleRequestVoteResponse(2, respRejected); err != nil {
		t.Fatalf("HandleRequestVoteResponse failed: %v", err)
	}

	// Vote count remains 1 (only self-vote)
	if votes := n.GrantedVotesCount(); votes != 1 {
		t.Fatalf("votes = %d, want 1", votes)
	}
	if r := n.Role(); r != raft.RoleCandidate {
		t.Fatalf("expected RoleCandidate, got %s", r)
	}
}

// -----------------------------------------------------------------------------
// Section 26.F: Stale Term Ignored
// -----------------------------------------------------------------------------

func TestNode_StaleTermResponse_Ignored(t *testing.T) {
	n, s := newTestQuorumNode(t, 1, []cluster.NodeID{2, 3}, nil, nil)

	// Set initial term to 3
	_ = s.SetTerm(3)

	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	// Term is now 4
	term, _ := n.Term()
	if term != 4 {
		t.Fatalf("term = %d, want 4", term)
	}

	// Stale response for Term 3 with VoteGranted=true
	staleResp := &transport.RequestVoteResponse{
		Term:        3,
		VoteGranted: true,
	}
	if err := n.HandleRequestVoteResponse(2, staleResp); err != nil {
		t.Fatalf("stale response handling failed: %v", err)
	}

	// No mutation to vote count or role
	if votes := n.GrantedVotesCount(); votes != 1 {
		t.Fatalf("votes = %d, want 1", votes)
	}
	if r := n.Role(); r != raft.RoleCandidate {
		t.Fatalf("expected RoleCandidate, got %s", r)
	}
	currentTerm, _ := n.Term()
	if currentTerm != 4 {
		t.Fatalf("expected term unchanged (4), got %d", currentTerm)
	}
}

// -----------------------------------------------------------------------------
// Section 26.G & 26.H: Higher Term Stepdown & Storage Failure
// -----------------------------------------------------------------------------

func TestNode_HigherTermResponse_DurableStepdown(t *testing.T) {
	var transitions []string
	hook := func(from, to raft.Role, term raft.Term) {
		transitions = append(transitions, fmt.Sprintf("%s->%s(T%d)", from, to, term))
	}

	n, s := newTestQuorumNode(t, 1, []cluster.NodeID{2, 3}, nil, hook)

	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	term, _ := n.Term()
	if term != 1 {
		t.Fatalf("term = %d, want 1", term)
	}

	// Higher term response from peer 2
	higherResp := &transport.RequestVoteResponse{
		Term:        5,
		VoteGranted: false,
	}
	if err := n.HandleRequestVoteResponse(2, higherResp); err != nil {
		t.Fatalf("HandleRequestVoteResponse higher term failed: %v", err)
	}

	// Must step down to Follower
	if r := n.Role(); r != raft.RoleFollower {
		t.Fatalf("Role = %s, want RoleFollower", r)
	}
	// Term must be durably persisted as 5
	newTerm, _ := n.Term()
	if newTerm != 5 {
		t.Fatalf("Term = %d, want 5", newTerm)
	}
	// Vote must be cleared on disk for the higher term
	votedFor, _ := s.VotedFor()
	if votedFor != cluster.NodeIDNil {
		t.Fatalf("votedFor = %d, want NodeIDNil", votedFor)
	}
	// Election votes map must be invalidated
	if votes := n.GrantedVotesCount(); votes != 0 {
		t.Fatalf("votes = %d, want 0 after stepdown", votes)
	}

	// Transition hook must have fired for Candidate -> Follower(T5)
	if len(transitions) != 2 || transitions[1] != "Candidate->Follower(T5)" {
		t.Fatalf("unexpected transitions: %v", transitions)
	}
}

func TestNode_HigherTermResponse_StorageFailureFailsClosed(t *testing.T) {
	n, s := newTestQuorumNode(t, 1, []cluster.NodeID{2, 3}, nil, nil)

	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}

	// Close storage to simulate disk I/O failure on durable term persistence
	_ = s.Close()

	higherResp := &transport.RequestVoteResponse{
		Term:        10,
		VoteGranted: false,
	}

	err := n.HandleRequestVoteResponse(2, higherResp)
	if err == nil {
		t.Fatalf("expected storage failure error, got nil")
	}

	// Should not have promoted to leader
	if r := n.Role(); r == raft.RoleLeader {
		t.Fatalf("node promoted to leader despite storage failure")
	}
}

// -----------------------------------------------------------------------------
// Section 26.I & 26.J: Unknown and Self Peer Rejection
// -----------------------------------------------------------------------------

func TestNode_UnknownPeer_Rejected(t *testing.T) {
	n, _ := newTestQuorumNode(t, 1, []cluster.NodeID{2, 3}, nil, nil)

	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}

	resp := &transport.RequestVoteResponse{
		Term:        1,
		VoteGranted: true,
	}

	// Peer 99 is not in cluster
	err := n.HandleRequestVoteResponse(99, resp)
	if err == nil {
		t.Fatalf("expected error for unknown peer, got nil")
	}
	var unknownErr *errors.UnknownPeerError
	if !stdErrorsAs(err, &unknownErr) {
		t.Fatalf("expected UnknownPeerError, got %v", err)
	}

	// Vote count must not change
	if votes := n.GrantedVotesCount(); votes != 1 {
		t.Fatalf("votes = %d, want 1", votes)
	}
}

func TestNode_SelfPeer_Rejected(t *testing.T) {
	n, _ := newTestQuorumNode(t, 1, []cluster.NodeID{2, 3}, nil, nil)

	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}

	resp := &transport.RequestVoteResponse{
		Term:        1,
		VoteGranted: true,
	}

	// Peer 1 is self
	err := n.HandleRequestVoteResponse(1, resp)
	if err == nil {
		t.Fatalf("expected error for self peer response, got nil")
	}

	// Vote count must not change
	if votes := n.GrantedVotesCount(); votes != 1 {
		t.Fatalf("votes = %d, want 1", votes)
	}
}

// -----------------------------------------------------------------------------
// Section 26.K: Stale Election Round Response
// -----------------------------------------------------------------------------

func TestNode_StaleElectionRound_Ignored(t *testing.T) {
	n, _ := newTestQuorumNode(t, 1, []cluster.NodeID{2, 3}, nil, nil)

	// Start term 1
	if err := n.StartNewElection(); err != nil {
		t.Fatalf("StartNewElection T1 failed: %v", err)
	}
	// Start term 2 (e.g. election timeout fired before receiving votes)
	if err := n.StartNewElection(); err != nil {
		t.Fatalf("StartNewElection T2 failed: %v", err)
	}

	term, _ := n.Term()
	if term != 2 {
		t.Fatalf("term = %d, want 2", term)
	}

	// Delayed response from term 1 arrives
	respT1 := &transport.RequestVoteResponse{
		Term:        1,
		VoteGranted: true,
	}
	if err := n.HandleRequestVoteResponse(2, respT1); err != nil {
		t.Fatalf("HandleRequestVoteResponse failed: %v", err)
	}

	// Term 2 vote count must remain 1 (only self)
	if votes := n.GrantedVotesCount(); votes != 1 {
		t.Fatalf("votes = %d, want 1", votes)
	}
	if r := n.Role(); r != raft.RoleCandidate {
		t.Fatalf("expected RoleCandidate, got %s", r)
	}
}

// -----------------------------------------------------------------------------
// Section 26.N: Concurrent Responses Under Race Detector
// -----------------------------------------------------------------------------

func TestNode_ConcurrentVoteResponses(t *testing.T) {
	sender := newQuorumMockSender()
	var (
		mu          sync.Mutex
		transitions []string
	)
	hook := func(from, to raft.Role, term raft.Term) {
		mu.Lock()
		defer mu.Unlock()
		transitions = append(transitions, fmt.Sprintf("%s->%s(T%d)", from, to, term))
	}

	// 5-node cluster: local=1, remote=2,3,4,5 (quorum=3)
	n, _ := newTestQuorumNode(t, 1, []cluster.NodeID{2, 3, 4, 5}, sender, hook)

	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}

	var wg sync.WaitGroup
	peers := []cluster.NodeID{2, 3, 4, 5}

	// Launch concurrent vote deliveries including duplicates, false votes, and stale terms
	for _, p := range peers {
		peerID := p
		wg.Add(3)
		// Valid positive vote
		go func() {
			defer wg.Done()
			_ = n.HandleRequestVoteResponse(peerID, &transport.RequestVoteResponse{
				Term:        1,
				VoteGranted: true,
			})
		}()
		// Duplicate positive vote
		go func() {
			defer wg.Done()
			_ = n.HandleRequestVoteResponse(peerID, &transport.RequestVoteResponse{
				Term:        1,
				VoteGranted: true,
			})
		}()
		// Stale term vote
		go func() {
			defer wg.Done()
			_ = n.HandleRequestVoteResponse(peerID, &transport.RequestVoteResponse{
				Term:        0,
				VoteGranted: true,
			})
		}()
	}

	wg.Wait()

	// Must have transitioned to Leader exactly once
	if r := n.Role(); r != raft.RoleLeader {
		t.Fatalf("Role = %s, want RoleLeader", r)
	}

	mu.Lock()
	leaderCount := 0
	for _, tr := range transitions {
		if tr == "Candidate->Leader(T1)" {
			leaderCount++
		}
	}
	mu.Unlock()

	if leaderCount != 1 {
		t.Fatalf("expected exactly 1 leader transition hook, got %d", leaderCount)
	}
}

// -----------------------------------------------------------------------------
// Section 22 & 23: HandlePeerFrame End-to-End Integration
// -----------------------------------------------------------------------------

func TestNode_HandlePeerFrame_EndToEnd(t *testing.T) {
	// 3-Node Cluster: Node 1 (Candidate), Node 2 (Follower), Node 3 (Follower)
	sender1 := newQuorumMockSender()
	sender2 := newQuorumMockSender()

	n1, _ := newTestQuorumNode(t, 1, []cluster.NodeID{2, 3}, sender1, nil)
	n2, _ := newTestQuorumNode(t, 2, []cluster.NodeID{1, 3}, sender2, nil)

	// Node 1 becomes Candidate and broadcasts RequestVote
	if err := n1.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := n1.BroadcastRequestVote(); err != nil {
		t.Fatalf("BroadcastRequestVote failed: %v", err)
	}

	framesToNode2 := sender1.GetSent(2)
	if len(framesToNode2) != 1 {
		t.Fatalf("expected 1 frame sent to node 2, got %d", len(framesToNode2))
	}

	// Node 2 receives RequestVote frame via HandlePeerFrame
	reqFrame := framesToNode2[0]
	if err := n2.HandlePeerFrame(1, reqFrame); err != nil {
		t.Fatalf("n2.HandlePeerFrame failed: %v", err)
	}

	// Node 2 should have sent back a RequestVoteResponse frame to Node 1
	framesBackToNode1 := sender2.GetSent(1)
	if len(framesBackToNode1) != 1 {
		t.Fatalf("expected 1 response frame sent to node 1, got %d", len(framesBackToNode1))
	}

	respFrame := framesBackToNode1[0]
	if transport.PeerMessageType(respFrame.Header.OpCode) != transport.PeerOpRequestVoteResponse {
		t.Fatalf("expected PeerOpRequestVoteResponse opcode, got 0x%02x", respFrame.Header.OpCode)
	}

	// Node 1 receives the response frame via HandlePeerFrame
	if err := n1.HandlePeerFrame(2, respFrame); err != nil {
		t.Fatalf("n1.HandlePeerFrame response failed: %v", err)
	}

	// Quorum (2/3) reached! Node 1 must be Leader!
	if r := n1.Role(); r != raft.RoleLeader {
		t.Fatalf("Role = %s, want RoleLeader", r)
	}
	if leaderID := n1.LeaderID(); leaderID != 1 {
		t.Fatalf("LeaderID = %d, want 1", leaderID)
	}
}

// stdErrorsAs helper to avoid importing errors alias conflict
func stdErrorsAs(err error, target interface{}) bool {
	return stdErrors.As(err, target)
}
