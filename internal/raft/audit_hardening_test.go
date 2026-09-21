package raft_test

import (
	"context"
	stdErrors "errors"
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/raft"
	"github.com/silent-knight19/lattice/internal/transport"
)

// auditSender is an isolated mock sender for hardening regression tests.
type auditSender struct {
	mu       sync.Mutex
	seqID    uint64
	sentMsgs map[cluster.NodeID][]*transport.Frame
}

func newAuditSender() *auditSender {
	return &auditSender{sentMsgs: make(map[cluster.NodeID][]*transport.Frame)}
}

func (m *auditSender) NextSeqID() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seqID++
	return m.seqID
}

func (m *auditSender) Send(ctx context.Context, peerID cluster.NodeID, frame *transport.Frame) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sentMsgs[peerID] = append(m.sentMsgs[peerID], frame)
	return nil
}

func (m *auditSender) GetSent(peerID cluster.NodeID) []*transport.Frame {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*transport.Frame(nil), m.sentMsgs[peerID]...)
}

func (m *auditSender) TotalSent() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := 0
	for _, f := range m.sentMsgs {
		total += len(f)
	}
	return total
}

func newAuditNode(t *testing.T, localID cluster.NodeID, peers []cluster.NodeID, sender raft.PeerSender, hook raft.TransitionHook) (*raft.Node, *raft.Storage) {
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

// FINDING A: index==0 with term>0 must be rejected as incoherent.
func TestAudit_InvalidLogCoordinates_ZeroIndexNonZeroTerm(t *testing.T) {
	if err := raft.ValidateCandidateLogCoordinates(0, 1); err == nil {
		t.Fatalf("expected error for (index=0, term=1)")
	}
	if err := raft.ValidateCandidateLogCoordinates(0, 1<<63); err == nil {
		t.Fatalf("expected error for (index=0, huge term)")
	}
	// Sentinel must remain valid.
	if err := raft.ValidateCandidateLogCoordinates(0, 0); err != nil {
		t.Fatalf("(0,0) should be valid: %v", err)
	}
	// Existing direction must still be rejected.
	if err := raft.ValidateCandidateLogCoordinates(5, 0); err == nil {
		t.Fatalf("expected error for (index=5, term=0)")
	}
}

func TestAudit_RequestVote_ZeroIndexNonZeroTermRejected(t *testing.T) {
	n, _ := newAuditNode(t, 1, []cluster.NodeID{2}, nil, nil)
	req := &transport.RequestVoteRequest{
		Term:         1,
		CandidateID:  2,
		LastLogIndex: 0,
		LastLogTerm:  99,
		Nonce:        7,
	}
	if _, err := n.HandleRequestVote(2, req); err == nil {
		t.Fatalf("expected rejection of RequestVote with (index=0, term=99)")
	}
}

func TestAudit_AppendEntries_ZeroIndexNonZeroTermRejected(t *testing.T) {
	n, _ := newAuditNode(t, 1, []cluster.NodeID{2}, nil, nil)
	req := &transport.AppendEntriesRequest{
		Term:         1,
		LeaderID:     2,
		PrevLogIndex: 0,
		PrevLogTerm:  77,
		Nonce:        7,
	}
	if _, err := n.HandleAppendEntries(2, req); err == nil {
		t.Fatalf("expected rejection of AppendEntries with (index=0, term=77)")
	}
}

// Lagging follower must still reset its election timer on a valid heartbeat
// even when PrevLog does not match (liveness vs replication separation).
func TestAudit_LaggingFollower_TimerResetOnLogMismatch(t *testing.T) {
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
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

	// Leader log is ahead (index 5); follower log is empty. Every heartbeat
	// carries PrevLogIndex=5 which the follower cannot match.
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
					PrevLogIndex: 5,
					PrevLogTerm:  1,
					Nonce:        4242,
				}
				resp, err := follower.HandleAppendEntries(1, req)
				if err != nil {
					t.Errorf("HandleAppendEntries failed: %v", err)
					return
				}
				// Log mismatch must be reported as replication failure...
				if resp.Success {
					t.Errorf("expected Success=false for lagging follower")
					return
				}
				// ...but leaderID must still be recorded for liveness.
				if follower.LeaderID() != 1 {
					t.Errorf("expected LeaderID=1, got %d", follower.LeaderID())
					return
				}
			}
		}
	}()

	time.Sleep(200 * time.Millisecond)
	close(stopCh)
	<-doneCh

	if follower.Role() != raft.RoleFollower {
		t.Fatalf("lagging follower timed out despite valid heartbeats: role=%s", follower.Role())
	}
	term, err := follower.Term()
	if err != nil {
		t.Fatalf("Term failed: %v", err)
	}
	if term != 1 {
		t.Fatalf("expected term 1, got %d", term)
	}
}

// Non-empty AppendEntries boundary (evolved in P15-S03-M02): valid entries
// from the legitimate leader must now be durably replicated and genuinely
// acknowledged, while malformed or mismatching requests must still fail
// without false success or partial mutation.
func TestAudit_NonEmptyAppendEntries_NeverAcked(t *testing.T) {
	n, s := newAuditNode(t, 1, []cluster.NodeID{2}, nil, nil)
	if err := s.SetTerm(1); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}
	req := &transport.AppendEntriesRequest{
		Term:         1,
		LeaderID:     2,
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Nonce:        999,
		Entries: []transport.PeerLogEntry{
			{Term: 1, Type: transport.PeerEntryNormal, Data: []byte("cmd")},
		},
	}
	resp, err := n.HandleAppendEntries(2, req)
	if err != nil {
		t.Fatalf("HandleAppendEntries failed: %v", err)
	}
	// M02: valid non-empty replication is genuinely persisted and acked.
	if !resp.Success {
		t.Fatalf("valid non-empty AppendEntries must succeed after M02")
	}
	if resp.MatchIndex != 1 {
		t.Fatalf("expected MatchIndex 1, got %d", resp.MatchIndex)
	}
	stored, err := s.Entry(1)
	if err != nil {
		t.Fatalf("replicated entry missing: %v", err)
	}
	if string(stored.Data) != "cmd" || stored.Term != 1 {
		t.Fatalf("replicated entry mismatch: %+v", stored)
	}
	// Liveness must still hold: sender recognized as leader.
	if n.LeaderID() != 2 {
		t.Fatalf("expected LeaderID=2, got %d", n.LeaderID())
	}

	// Malformed non-empty requests must still fail without false success.
	bad := &transport.AppendEntriesRequest{
		Term:         1,
		LeaderID:     2,
		PrevLogIndex: 5, // beyond follower log
		PrevLogTerm:  1,
		Nonce:        1000,
		Entries: []transport.PeerLogEntry{
			{Term: 1, Type: transport.PeerEntryNormal, Data: []byte("bad")},
		},
	}
	badResp, err := n.HandleAppendEntries(2, bad)
	if err != nil {
		t.Fatalf("mismatching HandleAppendEntries failed: %v", err)
	}
	if badResp.Success {
		t.Fatalf("mismatching non-empty AppendEntries must not succeed")
	}
	lastIdx, _, err := s.LastIndexAndTerm()
	if err != nil {
		t.Fatalf("LastIndexAndTerm failed: %v", err)
	}
	if lastIdx != 1 {
		t.Fatalf("mismatching request mutated the log: lastIdx=%d", lastIdx)
	}
}

// Unknown senders must not inject higher terms via AppendEntriesResponse.
func TestAudit_AppendEntriesResponse_UnknownSenderRejected(t *testing.T) {
	sender := newAuditSender()
	n, s := newAuditNode(t, 1, []cluster.NodeID{2}, sender, nil)
	if err := s.SetTerm(2); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	termBefore, _ := n.Term()
	resp := &transport.AppendEntriesResponse{Term: uint64(termBefore) + 5, Success: false, MatchIndex: 0}
	err := n.HandleAppendEntriesResponse(99, resp)
	if err == nil {
		t.Fatalf("expected UnknownPeerError for peer 99, got nil")
	}
	if !isUnknownPeerErr(err) {
		t.Fatalf("expected UnknownPeerError, got %T: %v", err, err)
	}
	termAfter, _ := n.Term()
	if termAfter != termBefore {
		t.Fatalf("unknown sender mutated term: before=%d after=%d", termBefore, termAfter)
	}
	if n.Role() != raft.RoleCandidate {
		t.Fatalf("unknown sender changed role to %s", n.Role())
	}
}

func isUnknownPeerErr(err error) bool {
	var unknown *errors.UnknownPeerError
	return stdErrors.As(err, &unknown)
}

// Stale immediate heartbeat must be suppressed when the transition hook (or a
// concurrent RPC) steps the node down before the one-time heartbeat fires.
func TestAudit_BecomeLeader_HookStepdownSuppressesStaleHeartbeat(t *testing.T) {
	sender := newAuditSender()
	var node *raft.Node
	hook := func(from, to raft.Role, term raft.Term) {
		if from == raft.RoleCandidate && to == raft.RoleLeader && node != nil {
			// Simulate a concurrent higher-term observation arriving
			// during the hook window.
			_, _ = node.ObserveHigherTerm(term + 1)
		}
	}
	var s *raft.Storage
	func() {
		dir := t.TempDir()
		var err error
		s, err = raft.OpenStorage(dir)
		if err != nil {
			t.Fatalf("OpenStorage failed: %v", err)
		}
		var err2 error
		node, err2 = raft.NewNode(raft.NodeConfig{
			LocalID:           1,
			Storage:           s,
			Peers:             []cluster.NodeID{2},
			PeerSender:        sender,
			TransitionHook:    hook,
			HeartbeatInterval: 50 * time.Millisecond,
		})
		if err2 != nil {
			_ = s.Close()
			t.Fatalf("NewNode failed: %v", err2)
		}
		t.Cleanup(func() {
			_ = node.Close()
			_ = s.Close()
		})
	}()

	if err := s.SetTerm(1); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}
	if err := node.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	termBeforeLeader, _ := node.Term()
	if err := node.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}
	// Node stepped down inside the hook: must be follower in higher term.
	if node.Role() != raft.RoleFollower {
		t.Fatalf("expected Follower after hook stepdown, got %s", node.Role())
	}
	termAfter, _ := node.Term()
	if termAfter != termBeforeLeader+1 {
		t.Fatalf("expected term %d, got %d", termBeforeLeader+1, termAfter)
	}
	// No stale term-2 heartbeat may have been emitted after invalidation.
	if total := sender.TotalSent(); total != 0 {
		t.Fatalf("stale immediate heartbeat sent after stepdown: %d frames", total)
	}
	if node.HeartbeatRunning() {
		t.Fatalf("scheduler must not run after hook stepdown")
	}
}

// Same stale-suppression guarantee for the quorum vote-counting path.
func TestAudit_VoteQuorum_HookStepdownSuppressesStaleHeartbeat(t *testing.T) {
	sender := newAuditSender()
	var node *raft.Node
	hook := func(from, to raft.Role, term raft.Term) {
		if from == raft.RoleCandidate && to == raft.RoleLeader && node != nil {
			_, _ = node.ObserveHigherTerm(term + 3)
		}
	}
	var s *raft.Storage
	func() {
		dir := t.TempDir()
		var err error
		s, err = raft.OpenStorage(dir)
		if err != nil {
			t.Fatalf("OpenStorage failed: %v", err)
		}
		var err2 error
		node, err2 = raft.NewNode(raft.NodeConfig{
			LocalID:        1,
			Storage:        s,
			Peers:          []cluster.NodeID{2, 3},
			PeerSender:     sender,
			TransitionHook: hook,
		})
		if err2 != nil {
			_ = s.Close()
			t.Fatalf("NewNode failed: %v", err2)
		}
		t.Cleanup(func() {
			_ = node.Close()
			_ = s.Close()
		})
	}()

	if err := s.SetTerm(1); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}
	if err := node.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	termAtElection, _ := node.Term()
	vote := &transport.RequestVoteResponse{Term: uint64(termAtElection), VoteGranted: true}
	if err := node.HandleRequestVoteResponse(2, vote); err != nil {
		t.Fatalf("HandleRequestVoteResponse failed: %v", err)
	}
	if node.Role() != raft.RoleFollower {
		t.Fatalf("expected Follower after hook stepdown, got %s", node.Role())
	}
	if total := sender.TotalSent(); total != 0 {
		t.Fatalf("stale quorum heartbeat sent after stepdown: %d frames", total)
	}
}

// Vote handling must fail closed without counting votes from invalid senders
// and without mutating state on storage failure.
func TestAudit_VoteResponse_FailClosed(t *testing.T) {
	n, s := newAuditNode(t, 1, []cluster.NodeID{2, 3}, nil, nil)
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if votes := n.GrantedVotesCount(); votes != 1 {
		t.Fatalf("expected self-vote=1, got %d", votes)
	}
	_ = s.Close()
	resp := &transport.RequestVoteResponse{Term: 1, VoteGranted: true}
	if err := n.HandleRequestVoteResponse(2, resp); err == nil {
		t.Fatalf("expected storage-failure error, got nil")
	}
	if votes := n.GrantedVotesCount(); votes != 1 {
		t.Fatalf("failed vote must not mutate count: got %d, want 1", votes)
	}
	if n.Role() == raft.RoleLeader {
		t.Fatalf("must not promote to leader on storage failure")
	}
}

// Rapid Leader -> Follower -> Leader restarts must converge to a single
// scheduler session carrying the latest term.
func TestAudit_Heartbeat_RapidRestartConverges(t *testing.T) {
	sender := newAuditSender()
	n, s := newAuditNode(t, 1, []cluster.NodeID{2}, sender, nil)
	for round := 0; round < 5; round++ {
		termBefore, _ := s.Term()
		if err := n.BecomeCandidate(); err != nil {
			t.Fatalf("round %d BecomeCandidate failed: %v", round, err)
		}
		if err := n.BecomeLeader(); err != nil {
			t.Fatalf("round %d BecomeLeader failed: %v", round, err)
		}
		if !n.HeartbeatRunning() {
			t.Fatalf("round %d expected scheduler running", round)
		}
		time.Sleep(60 * time.Millisecond)
		termAfter, _ := s.Term()
		if termAfter <= termBefore {
			t.Fatalf("round %d term did not advance: %d -> %d", round, termBefore, termAfter)
		}
		if err := n.BecomeFollower(termAfter+1, cluster.NodeIDNil); err != nil {
			t.Fatalf("round %d BecomeFollower failed: %v", round, err)
		}
		if n.HeartbeatRunning() {
			t.Fatalf("round %d expected scheduler stopped", round)
		}
	}
	// Final leadership session must restart cleanly.
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("final BecomeCandidate failed: %v", err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatalf("final BecomeLeader failed: %v", err)
	}
	if !n.HeartbeatRunning() {
		t.Fatalf("final session expected scheduler running")
	}
	time.Sleep(70 * time.Millisecond)
	finalTerm, _ := n.Term()
	foundFinal := false
	for _, f := range sender.GetSent(2) {
		req, err := transport.DecodeAppendEntries(f)
		if err != nil {
			continue
		}
		if req.Term == uint64(finalTerm) && len(req.Entries) == 0 && req.LeaderID == 1 {
			foundFinal = true
			break
		}
	}
	if !foundFinal {
		t.Fatalf("no heartbeat with final term %d found", finalTerm)
	}
}
