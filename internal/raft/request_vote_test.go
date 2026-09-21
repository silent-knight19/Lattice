package raft_test

import (
	stdErrors "errors"
	"fmt"
	"testing"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/raft"
	"github.com/silent-knight19/lattice/internal/transport"
)

func newTestNodeWithPeers(t *testing.T, id cluster.NodeID, peers []cluster.NodeID) (*raft.Node, *raft.Storage, func()) {
	t.Helper()
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}

	n, err := raft.NewNode(raft.NodeConfig{
		LocalID: id,
		Storage: s,
		Peers:   peers,
	})
	if err != nil {
		_ = s.Close()
		t.Fatalf("NewNode failed: %v", err)
	}

	cleanup := func() {
		_ = n.Close()
		_ = s.Close()
	}

	return n, s, cleanup
}

func TestNode_HandleRequestVote_Matrix(t *testing.T) {
	// Section 56: RequestVote test matrix

	// 1. Sender identity binding mismatch (Section 26)
	t.Run("sender_mismatch", func(t *testing.T) {
		n, _, cleanup := newTestNodeWithPeers(t, 1, []cluster.NodeID{2, 3})
		defer cleanup()

		req := &transport.RequestVoteRequest{
			Term:         1,
			CandidateID:  3, // declared candidate
			LastLogIndex: 0,
			LastLogTerm:  0,
			Nonce:        100,
		}

		// Authenticated peer is 2, but candidate in payload is 3
		resp, err := n.HandleRequestVote(2, req)
		if err == nil || !stdErrors.Is(err, errors.ErrRaftSenderMismatch) {
			t.Fatalf("expected ErrRaftSenderMismatch, got (%v, %v)", resp, err)
		}
	})

	// 2. Self request rejected (Section 27)
	t.Run("self_request_rejected", func(t *testing.T) {
		n, _, cleanup := newTestNodeWithPeers(t, 1, []cluster.NodeID{2, 3})
		defer cleanup()

		req := &transport.RequestVoteRequest{
			Term:         1,
			CandidateID:  1, // self
			LastLogIndex: 0,
			LastLogTerm:  0,
			Nonce:        100,
		}

		resp, err := n.HandleRequestVote(1, req)
		if err == nil || !stdErrors.Is(err, errors.ErrRaftSelfVoteRPC) {
			t.Fatalf("expected ErrRaftSelfVoteRPC, got (%v, %v)", resp, err)
		}
	})

	// 3. Unknown peer rejected (Section 28)
	t.Run("unknown_peer_rejected", func(t *testing.T) {
		n, _, cleanup := newTestNodeWithPeers(t, 1, []cluster.NodeID{2, 3})
		defer cleanup()

		req := &transport.RequestVoteRequest{
			Term:         1,
			CandidateID:  99, // unknown
			LastLogIndex: 0,
			LastLogTerm:  0,
			Nonce:        100,
		}

		resp, err := n.HandleRequestVote(99, req)
		var unkErr *errors.UnknownPeerError
		if err == nil || !stdErrors.As(err, &unkErr) {
			t.Fatalf("expected UnknownPeerError, got (%v, %v)", resp, err)
		}
	})

	// 4. Stale term rejected (Section 29)
	t.Run("stale_term_rejected", func(t *testing.T) {
		n, s, cleanup := newTestNodeWithPeers(t, 1, []cluster.NodeID{2, 3})
		defer cleanup()

		// Advance local term to 5
		if err := s.SetTerm(5); err != nil {
			t.Fatalf("SetTerm failed: %v", err)
		}

		req := &transport.RequestVoteRequest{
			Term:         3, // Stale: 3 < 5
			CandidateID:  2,
			LastLogIndex: 10,
			LastLogTerm:  3,
			Nonce:        101,
		}

		resp, err := n.HandleRequestVote(2, req)
		if err != nil {
			t.Fatalf("HandleRequestVote failed: %v", err)
		}
		if resp.VoteGranted {
			t.Fatalf("expected VoteGranted = false for stale term")
		}
		if resp.Term != 5 {
			t.Fatalf("expected response.Term = 5, got %d", resp.Term)
		}

		// Persistent state must NOT be modified
		term, _ := s.Term()
		if term != 5 {
			t.Fatalf("term modified on stale request: got %d, want 5", term)
		}
	})

	// 5. Higher term updates term and grants vote if candidate log is up to date (Section 30, 34)
	t.Run("higher_term_granted", func(t *testing.T) {
		n, s, cleanup := newTestNodeWithPeers(t, 1, []cluster.NodeID{2, 3})
		defer cleanup()

		// Local state: term 2, votedFor = 3
		_ = s.SetHardState(raft.HardState{Term: 2, VotedFor: 3})

		req := &transport.RequestVoteRequest{
			Term:         6, // Higher: 6 > 2
			CandidateID:  2,
			LastLogIndex: 0,
			LastLogTerm:  0,
			Nonce:        102,
		}

		resp, err := n.HandleRequestVote(2, req)
		if err != nil {
			t.Fatalf("HandleRequestVote failed: %v", err)
		}
		if !resp.VoteGranted {
			t.Fatalf("expected VoteGranted = true for higher term with equal log")
		}
		if resp.Term != 6 {
			t.Fatalf("expected response.Term = 6, got %d", resp.Term)
		}

		// Verify disk state
		hs, err := s.HardState()
		if err != nil {
			t.Fatalf("HardState failed: %v", err)
		}
		if hs.Term != 6 {
			t.Fatalf("term not updated on disk: got %d, want 6", hs.Term)
		}
		if hs.VotedFor != 2 {
			t.Fatalf("vote not persisted on disk: got %d, want 2", hs.VotedFor)
		}
		if n.Role() != raft.RoleFollower {
			t.Fatalf("role not Follower after higher term: got %s", n.Role())
		}
	})

	// 6. Higher term rejected if candidate log is behind (Section 30, 32)
	t.Run("higher_term_log_behind_rejected", func(t *testing.T) {
		n, s, cleanup := newTestNodeWithPeers(t, 1, []cluster.NodeID{2, 3})
		defer cleanup()

		// Populate local log with entries at term 2
		_ = s.SetTerm(2)
		_ = s.Append(
			raft.LogEntry{Index: 1, Term: 2, Type: transport.PeerEntryNormal, Data: []byte("a")},
			raft.LogEntry{Index: 2, Term: 2, Type: transport.PeerEntryNormal, Data: []byte("b")},
		)

		// Candidate has higher Raft term 4, but last log entry is at term 1 (behind)
		req := &transport.RequestVoteRequest{
			Term:         4,
			CandidateID:  2,
			LastLogIndex: 5,
			LastLogTerm:  1, // Behind local log term 2!
			Nonce:        103,
		}

		resp, err := n.HandleRequestVote(2, req)
		if err != nil {
			t.Fatalf("HandleRequestVote failed: %v", err)
		}
		if resp.VoteGranted {
			t.Fatalf("expected VoteGranted = false when candidate log term is behind")
		}
		if resp.Term != 4 {
			t.Fatalf("expected response.Term = 4, got %d", resp.Term)
		}

		// Node stepped down to term 4, but did NOT vote for candidate 2
		hs, _ := s.HardState()
		if hs.Term != 4 {
			t.Fatalf("term should be 4, got %d", hs.Term)
		}
		if hs.VotedFor != cluster.NodeIDNil {
			t.Fatalf("votedFor should be nil, got %d", hs.VotedFor)
		}
	})

	// 7. Same term: vote available and granted (Section 31, 34)
	t.Run("same_term_vote_granted", func(t *testing.T) {
		n, s, cleanup := newTestNodeWithPeers(t, 1, []cluster.NodeID{2, 3})
		defer cleanup()

		_ = s.SetTerm(3)

		req := &transport.RequestVoteRequest{
			Term:         3,
			CandidateID:  2,
			LastLogIndex: 0,
			LastLogTerm:  0,
			Nonce:        104,
		}

		resp, err := n.HandleRequestVote(2, req)
		if err != nil {
			t.Fatalf("HandleRequestVote failed: %v", err)
		}
		if !resp.VoteGranted {
			t.Fatalf("expected VoteGranted = true")
		}
		if resp.Term != 3 {
			t.Fatalf("expected Term = 3, got %d", resp.Term)
		}

		vote, _ := s.VotedFor()
		if vote != 2 {
			t.Fatalf("expected votedFor = 2, got %d", vote)
		}
	})

	// 8. Same term: already voted for this candidate (idempotent re-vote, Section 35)
	t.Run("same_term_same_candidate_idempotent", func(t *testing.T) {
		n, s, cleanup := newTestNodeWithPeers(t, 1, []cluster.NodeID{2, 3})
		defer cleanup()

		_ = s.SetHardState(raft.HardState{Term: 3, VotedFor: 2})

		req := &transport.RequestVoteRequest{
			Term:         3,
			CandidateID:  2,
			LastLogIndex: 0,
			LastLogTerm:  0,
			Nonce:        105,
		}

		resp, err := n.HandleRequestVote(2, req)
		if err != nil {
			t.Fatalf("HandleRequestVote failed: %v", err)
		}
		if !resp.VoteGranted {
			t.Fatalf("expected idempotent VoteGranted = true")
		}
	})

	// 9. Same term: already voted for a different candidate (Section 36)
	t.Run("same_term_different_candidate_rejected", func(t *testing.T) {
		n, s, cleanup := newTestNodeWithPeers(t, 1, []cluster.NodeID{2, 3})
		defer cleanup()

		_ = s.SetHardState(raft.HardState{Term: 3, VotedFor: 3}) // voted for 3

		req := &transport.RequestVoteRequest{
			Term:         3,
			CandidateID:  2, // 2 is asking
			LastLogIndex: 0,
			LastLogTerm:  0,
			Nonce:        106,
		}

		resp, err := n.HandleRequestVote(2, req)
		if err != nil {
			t.Fatalf("HandleRequestVote failed: %v", err)
		}
		if resp.VoteGranted {
			t.Fatalf("expected VoteGranted = false when already voted for different candidate")
		}
		if resp.Term != 3 {
			t.Fatalf("expected Term = 3, got %d", resp.Term)
		}

		// Vote remains unchanged
		vote, _ := s.VotedFor()
		if vote != 3 {
			t.Fatalf("vote modified: got %d, want 3", vote)
		}
	})

	// 10. Candidate log comparison: same term equal log term, lower index rejected (Section 56)
	t.Run("candidate_log_equal_term_lower_index", func(t *testing.T) {
		n, s, cleanup := newTestNodeWithPeers(t, 1, []cluster.NodeID{2, 3})
		defer cleanup()

		_ = s.SetTerm(2)
		_ = s.Append(
			raft.LogEntry{Index: 1, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("1")},
			raft.LogEntry{Index: 2, Term: 2, Type: transport.PeerEntryNormal, Data: []byte("2")},
			raft.LogEntry{Index: 3, Term: 2, Type: transport.PeerEntryNormal, Data: []byte("3")},
		)

		// Candidate has lastLogTerm 2, but lastLogIndex 2 (< voter lastLogIndex 3)
		req := &transport.RequestVoteRequest{
			Term:         2,
			CandidateID:  2,
			LastLogIndex: 2,
			LastLogTerm:  2,
			Nonce:        107,
		}

		resp, err := n.HandleRequestVote(2, req)
		if err != nil {
			t.Fatalf("HandleRequestVote failed: %v", err)
		}
		if resp.VoteGranted {
			t.Fatalf("expected VoteGranted = false for candidate with smaller index")
		}
	})

	// 11. Candidate log comparison: equal term equal index granted (Section 56)
	t.Run("candidate_log_equal_term_equal_index", func(t *testing.T) {
		n, s, cleanup := newTestNodeWithPeers(t, 1, []cluster.NodeID{2, 3})
		defer cleanup()

		_ = s.SetTerm(2)
		_ = s.Append(
			raft.LogEntry{Index: 1, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("1")},
			raft.LogEntry{Index: 2, Term: 2, Type: transport.PeerEntryNormal, Data: []byte("2")},
		)

		req := &transport.RequestVoteRequest{
			Term:         2,
			CandidateID:  2,
			LastLogIndex: 2,
			LastLogTerm:  2,
			Nonce:        108,
		}

		resp, err := n.HandleRequestVote(2, req)
		if err != nil {
			t.Fatalf("HandleRequestVote failed: %v", err)
		}
		if !resp.VoteGranted {
			t.Fatalf("expected VoteGranted = true for candidate with equal index")
		}
	})

	// 12. Candidate impossible coordinates (index > 0, term 0) (Section 33)
	t.Run("candidate_impossible_coordinates_rejected", func(t *testing.T) {
		n, _, cleanup := newTestNodeWithPeers(t, 1, []cluster.NodeID{2, 3})
		defer cleanup()

		req := &transport.RequestVoteRequest{
			Term:         1,
			CandidateID:  2,
			LastLogIndex: 10,
			LastLogTerm:  0, // Impossible: index 10 > 0 with term 0
			Nonce:        109,
		}

		resp, err := n.HandleRequestVote(2, req)
		if err == nil || !stdErrors.Is(err, errors.ErrRaftInvalidLogEntry) {
			t.Fatalf("expected ErrRaftInvalidLogEntry, got (%v, %v)", resp, err)
		}
	})

	// 13. Closed node rejected
	t.Run("closed_node_rejected", func(t *testing.T) {
		n, _, cleanup := newTestNodeWithPeers(t, 1, []cluster.NodeID{2, 3})
		_ = n.Close()
		defer cleanup()

		req := &transport.RequestVoteRequest{
			Term:         1,
			CandidateID:  2,
			LastLogIndex: 0,
			LastLogTerm:  0,
			Nonce:        110,
		}

		resp, err := n.HandleRequestVote(2, req)
		if err == nil || !stdErrors.Is(err, errors.ErrRaftStateClosed) {
			t.Fatalf("expected ErrRaftStateClosed, got (%v, %v)", resp, err)
		}
	})

	// 14. Storage failure during higher-term transition (Section 40)
	t.Run("storage_failure_higher_term", func(t *testing.T) {
		n, s, cleanup := newTestNodeWithPeers(t, 1, []cluster.NodeID{2, 3})
		defer cleanup()

		_ = s.SetTerm(5)

		restore := raft.SetRaftRenameFnForTesting(func(oldpath, newpath string) error {
			return fmt.Errorf("injected disk failure on higher term update")
		})

		req := &transport.RequestVoteRequest{
			Term:         8,
			CandidateID:  2,
			LastLogIndex: 0,
			LastLogTerm:  0,
			Nonce:        111,
		}

		resp, err := n.HandleRequestVote(2, req)
		restore()

		if err == nil {
			t.Fatalf("expected error on storage failure, got response: %+v", resp)
		}

		// State on disk MUST NOT claim term 8
		hs, _ := s.HardState()
		if hs.Term != 5 {
			t.Fatalf("term falsely updated on failed persistence: got %d, want 5", hs.Term)
		}
		if hs.VotedFor != cluster.NodeIDNil {
			t.Fatalf("vote falsely updated on failed persistence: got %d", hs.VotedFor)
		}
	})

	// 15. Storage failure during vote grant (Section 41)
	t.Run("storage_failure_vote_grant", func(t *testing.T) {
		n, s, cleanup := newTestNodeWithPeers(t, 1, []cluster.NodeID{2, 3})
		defer cleanup()

		_ = s.SetTerm(5)

		restore := raft.SetRaftRenameFnForTesting(func(oldpath, newpath string) error {
			return fmt.Errorf("injected disk failure on vote write")
		})

		req := &transport.RequestVoteRequest{
			Term:         5,
			CandidateID:  2,
			LastLogIndex: 0,
			LastLogTerm:  0,
			Nonce:        112,
		}

		resp, err := n.HandleRequestVote(2, req)
		restore()

		if err == nil {
			t.Fatalf("expected error on vote grant storage failure, got: %+v", resp)
		}

		// Vote must remain nil
		vote, _ := s.VotedFor()
		if vote != cluster.NodeIDNil {
			t.Fatalf("vote falsely updated on failed persistence: got %d", vote)
		}
	})
}

func TestNode_TimerResetOnGrantedVote(t *testing.T) {
	// Section 42, 59: Timer reset test
	// Demonstrate: follower timer armed -> valid vote granted -> timer reset -> old expiration cannot trigger election
	var (
		timerExpCount int
	)

	n, s, cleanup := newTestNodeWithPeers(t, 1, []cluster.NodeID{2, 3})
	defer cleanup()

	_ = s.SetTerm(1)

	// Inject a timer with deterministic duration provider
	timer := n.ElectionTimer()
	timer.Reset()
	genBefore := timer.CurrentGen()

	// Grant vote to Candidate 2
	req := &transport.RequestVoteRequest{
		Term:         1,
		CandidateID:  2,
		LastLogIndex: 0,
		LastLogTerm:  0,
		Nonce:        201,
	}

	resp, err := n.HandleRequestVote(2, req)
	if err != nil {
		t.Fatalf("HandleRequestVote failed: %v", err)
	}
	if !resp.VoteGranted {
		t.Fatalf("expected vote to be granted")
	}

	genAfter := timer.CurrentGen()
	if genAfter <= genBefore {
		t.Fatalf("expected timer generation to advance after vote grant: genBefore=%d, genAfter=%d",
			genBefore, genAfter)
	}

	// Rejected request must NOT reset election timer (Section 34, 36)
	genBeforeRejected := timer.CurrentGen()
	rejectReq := &transport.RequestVoteRequest{
		Term:         0, // Stale term
		CandidateID:  3,
		LastLogIndex: 0,
		LastLogTerm:  0,
		Nonce:        202,
	}

	resp, err = n.HandleRequestVote(3, rejectReq)
	if err != nil {
		t.Fatalf("HandleRequestVote failed: %v", err)
	}
	if resp.VoteGranted {
		t.Fatalf("expected vote rejected for stale term")
	}

	genAfterRejected := timer.CurrentGen()
	if genAfterRejected != genBeforeRejected {
		t.Fatalf("timer generation should NOT advance on rejected request: before=%d, after=%d",
			genBeforeRejected, genAfterRejected)
	}

	_ = timerExpCount
}
