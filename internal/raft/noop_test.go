package raft_test

import (
	"testing"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/raft"
	"github.com/silent-knight19/lattice/internal/transport"
)

// Test 1 — a new leader appends exactly one current-term no-op.
func TestNoOp_AppendedOnElection(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2, 3})
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}
	term, _ := n.Term()
	lastIdx, _, err := s.LastIndexAndTerm()
	if err != nil {
		t.Fatalf("LastIndexAndTerm failed: %v", err)
	}
	if lastIdx != 1 {
		t.Fatalf("expected exactly one entry (the no-op), got lastIdx=%d", lastIdx)
	}
	e, err := s.Entry(1)
	if err != nil {
		t.Fatalf("Entry(1) failed: %v", err)
	}
	if e.Term != term || e.Index != 1 {
		t.Fatalf("no-op coordinates wrong: %+v (leader term %d)", e, term)
	}
	if e.Type != transport.PeerEntryNoop {
		t.Fatalf("expected PeerEntryNoop, got %s", e.Type)
	}
	if len(e.Data) != 0 {
		t.Fatalf("no-op must carry empty payload, got %d bytes", len(e.Data))
	}
}

// Test 2 — the no-op is durable across close/reopen.
func TestNoOp_DurableAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	n, err := raft.NewNode(raft.NodeConfig{LocalID: 1, Storage: s, Peers: []cluster.NodeID{2}})
	if err != nil {
		_ = s.Close()
		t.Fatalf("NewNode failed: %v", err)
	}
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}
	term, _ := n.Term()
	_ = n.Close()
	_ = s.Close()

	s2, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer func() { _ = s2.Close() }()
	e, err := s2.Entry(1)
	if err != nil {
		t.Fatalf("no-op lost across restart: %v", err)
	}
	if e.Type != transport.PeerEntryNoop || e.Term != term || e.Index != 1 {
		t.Fatalf("recovered no-op mismatch: %+v", e)
	}
}

// Test 3 — repeated leadership initialization never duplicates the no-op.
func TestNoOp_NoDuplicateOnRepeatedPromotion(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2, 3})
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}
	// Idempotent re-entry must not append again.
	if err := n.BecomeLeader(); err != nil {
		t.Fatalf("second BecomeLeader failed: %v", err)
	}
	lastIdx, _, err := s.LastIndexAndTerm()
	if err != nil {
		t.Fatalf("LastIndexAndTerm failed: %v", err)
	}
	if lastIdx != 1 {
		t.Fatalf("duplicate no-op appended: lastIdx=%d", lastIdx)
	}
}

// Test 4 — no-op index is contiguous with pre-existing entries.
func TestNoOp_IndexContiguousWithPrefix(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2})
	seedReplLog(t, s, []replSpec{{1, "A"}, {1, "B"}})
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}
	term, _ := n.Term()
	for i, want := range []string{"A", "B"} {
		got, err := s.Entry(raft.LogIndex(i + 1))
		if err != nil {
			t.Fatalf("Entry(%d) failed: %v", i+1, err)
		}
		if string(got.Data) != want || got.Term != 1 {
			t.Fatalf("prefix entry %d changed: %+v", i+1, got)
		}
	}
	noop, err := s.Entry(3)
	if err != nil {
		t.Fatalf("no-op Entry(3) failed: %v", err)
	}
	if noop.Type != transport.PeerEntryNoop || noop.Term != term {
		t.Fatalf("no-op mismatch at index 3: %+v", noop)
	}
	// A later proposal continues contiguously.
	prop, err := n.Propose([]byte("c"))
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}
	if prop.Index != 4 {
		t.Fatalf("expected proposal at index 4, got %d", prop.Index)
	}
}

// Test 5 — no-op term always equals the leader's current term.
func TestNoOp_TermEqualsLeaderTerm(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2})
	if err := s.SetTerm(7); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}
	term, _ := n.Term()
	if term != 8 {
		t.Fatalf("expected term 8, got %d", term)
	}
	noop, err := s.Entry(1)
	if err != nil {
		t.Fatalf("Entry(1) failed: %v", err)
	}
	if noop.Term != 8 || noop.Type != transport.PeerEntryNoop {
		t.Fatalf("no-op must carry current term 8: %+v", noop)
	}
}

// Test 6 — an old-term quorum-replicated prefix becomes committable once the
// current-term no-op reaches quorum, with zero client proposals.
func TestNoOp_OldPrefixCommittableWithoutClientTraffic(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2, 3})
	seedReplLog(t, s, []replSpec{{1, "old1"}, {1, "old2"}})
	term := mustCommitLead(t, n) // term 2; log: old,old,noop
	if term != 2 {
		t.Fatalf("expected term 2, got %d", term)
	}
	if got := n.CommitIndex(); got != 0 {
		t.Fatalf("no-op alone must not commit: %d", got)
	}
	// Replicate ONLY the no-op (index 3) to one follower: quorum (leader+1).
	if err := n.HandleAppendEntriesResponse(2, commitResp(2, true, 3)); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if got := n.CommitIndex(); got != 3 {
		t.Fatalf("commitIndex = %d, want 3 (prefix implicitly committed)", got)
	}
}

// Test 7 — the no-op replicates through AppendEntries like any entry.
func TestNoOp_ReplicatesToFollower(t *testing.T) {
	ln, _ := newReplNode(t, 1, []cluster.NodeID{2})
	mustCommitLead(t, ln)
	leaderTerm, _ := ln.Term()

	fn, fs := newReplNode(t, 2, []cluster.NodeID{1})
	_ = fs.SetTerm(leaderTerm - 1)
	req := replRequest(uint64(leaderTerm), 1, 0, 0, []transport.PeerLogEntry{
		{Term: uint64(leaderTerm), Type: transport.PeerEntryNoop},
	})
	resp, err := fn.HandleAppendEntries(1, req)
	if err != nil {
		t.Fatalf("HandleAppendEntries failed: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected no-op replication success")
	}
	stored, err := fs.Entry(1)
	if err != nil {
		t.Fatalf("follower missing no-op: %v", err)
	}
	if stored.Type != transport.PeerEntryNoop || stored.Term != leaderTerm {
		t.Fatalf("follower no-op mismatch: %+v", stored)
	}
}

// Test 8 — N=1 leader commits its no-op immediately without any traffic.
func TestNoOp_SingleNodeCommitsImmediately(t *testing.T) {
	n, s := newReplNode(t, 1, nil)
	mustCommitLead(t, n)
	if got := n.CommitIndex(); got != 1 {
		t.Fatalf("N=1 must commit its no-op: commitIndex = %d", got)
	}
	noop, err := s.Entry(1)
	if err != nil {
		t.Fatalf("Entry(1) failed: %v", err)
	}
	if noop.Type != transport.PeerEntryNoop {
		t.Fatalf("expected no-op, got %+v", noop)
	}
}

// Test 9 — quorum-path promotion mints the no-op (not just manual BecomeLeader).
func TestNoOp_QuorumPathMintsNoOp(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2, 3})
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	term, _ := n.Term()
	if err := n.HandleRequestVoteResponse(2, &transport.RequestVoteResponse{Term: uint64(term), VoteGranted: true}); err != nil {
		t.Fatalf("vote response failed: %v", err)
	}
	if n.Role() != raft.RoleLeader {
		t.Fatalf("expected Leader, got %s", n.Role())
	}
	noop, err := s.Entry(1)
	if err != nil {
		t.Fatalf("quorum-path no-op missing: %v", err)
	}
	if noop.Type != transport.PeerEntryNoop || noop.Term != term {
		t.Fatalf("quorum-path no-op mismatch: %+v", noop)
	}
	if got := n.NextIndex()[2]; got != 2 {
		t.Fatalf("nextIndex[2] = %d, want 2 (past the no-op)", got)
	}
}

// Test 10 — persistence failure during promotion never publishes leadership.
func TestNoOp_PersistenceFailureNoFalseLeader(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2})
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	// Fail the entire durable substrate: reads fail first, so promotion —
	// including its no-op append — cannot proceed.
	_ = s.Close()
	if err := n.BecomeLeader(); err == nil {
		t.Fatalf("expected promotion failure on closed storage, got nil")
	}
	if n.Role() != raft.RoleCandidate {
		t.Fatalf("failed promotion must leave Candidate, got %s", n.Role())
	}
	if n.HeartbeatRunning() {
		t.Fatalf("scheduler must not start on failed promotion")
	}
}

// Test 11 — stepdown racing promotion still leaves exactly one durable no-op
// and emits no stale leadership traffic.
func TestNoOp_StepdownRaceLeavesSingleNoOp(t *testing.T) {
	sender := newAuditSender()
	var node *raft.Node
	var s *raft.Storage
	func() {
		dir := t.TempDir()
		var err error
		s, err = raft.OpenStorage(dir)
		if err != nil {
			t.Fatalf("OpenStorage failed: %v", err)
		}
		hook := func(from, to raft.Role, term raft.Term) {
			if from == raft.RoleCandidate && to == raft.RoleLeader && node != nil {
				_, _ = node.ObserveHigherTerm(term + 1)
			}
		}
		var err2 error
		node, err2 = raft.NewNode(raft.NodeConfig{
			LocalID: 1, Storage: s, Peers: []cluster.NodeID{2},
			PeerSender: sender, TransitionHook: hook,
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
	if err := node.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := node.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}
	if node.Role() != raft.RoleFollower {
		t.Fatalf("expected Follower after hook stepdown, got %s", node.Role())
	}
	// Exactly one no-op was durably appended before the stepdown.
	lastIdx, _, err := s.LastIndexAndTerm()
	if err != nil {
		t.Fatalf("LastIndexAndTerm failed: %v", err)
	}
	if lastIdx != 1 {
		t.Fatalf("expected exactly one no-op entry, got lastIdx=%d", lastIdx)
	}
	noop, err := s.Entry(1)
	if err != nil || noop.Type != transport.PeerEntryNoop {
		t.Fatalf("expected durable no-op, got %+v err=%v", noop, err)
	}
	if total := sender.TotalSent(); total != 0 {
		t.Fatalf("stale promotion traffic after stepdown: %d frames", total)
	}
}

// Test 12 — restart preserves the no-op; re-election mints the next term's
// no-op and recommits the prefix without client traffic.
func TestNoOp_RestartThenRecommitWithoutTraffic(t *testing.T) {
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	n, err := raft.NewNode(raft.NodeConfig{LocalID: 1, Storage: s})
	if err != nil {
		_ = s.Close()
		t.Fatalf("NewNode failed: %v", err)
	}
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}
	t1, _ := n.Term()
	if got := n.CommitIndex(); got != 1 {
		t.Fatalf("N=1 commit = %d, want 1", got)
	}
	_ = n.Close()
	_ = s.Close()

	s2, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	n2, err := raft.NewNode(raft.NodeConfig{LocalID: 1, Storage: s2})
	if err != nil {
		_ = s2.Close()
		t.Fatalf("NewNode failed: %v", err)
	}
	defer func() {
		_ = n2.Close()
		_ = s2.Close()
	}()
	// Volatile commit resets; the log (with no-op) survives.
	if got := n2.CommitIndex(); got != 0 {
		t.Fatalf("fresh commitIndex = %d, want volatile 0", got)
	}
	old, err := s2.Entry(1)
	if err != nil || old.Type != transport.PeerEntryNoop || old.Term != t1 {
		t.Fatalf("no-op lost across restart: %+v err=%v", old, err)
	}
	// Re-election mints the new term's no-op and recommits the whole prefix.
	if err := n2.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := n2.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}
	t2, _ := n2.Term()
	if t2 <= t1 {
		t.Fatalf("expected higher term, got %d (was %d)", t2, t1)
	}
	fresh, err := s2.Entry(2)
	if err != nil || fresh.Type != transport.PeerEntryNoop || fresh.Term != t2 {
		t.Fatalf("new-term no-op missing: %+v err=%v", fresh, err)
	}
	if got := n2.CommitIndex(); got != 2 {
		t.Fatalf("N=1 must recommit prefix via no-op: commitIndex = %d", got)
	}
}
