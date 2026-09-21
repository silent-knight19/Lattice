package raft_test

import (
	stdErrors "errors"
	"fmt"
	"math"
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/raft"
)

func newTestNode(t *testing.T, id cluster.NodeID) (*raft.Node, *raft.Storage, func()) {
	t.Helper()
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}

	n, err := raft.NewNode(raft.NodeConfig{
		LocalID: id,
		Storage: s,
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

func TestNode_InitialState(t *testing.T) {
	n, s, cleanup := newTestNode(t, 1)
	defer cleanup()

	// Initial volatile role must be Follower
	if r := n.Role(); r != raft.RoleFollower {
		t.Fatalf("initial role = %s, want %s", r, raft.RoleFollower)
	}

	// Initial term must be 0
	term, err := n.Term()
	if err != nil || term != 0 {
		t.Fatalf("initial term = (%d, %v), want (0, nil)", term, err)
	}

	// Initial vote must be NodeIDNil (0)
	vote, err := n.VotedFor()
	if err != nil || vote != cluster.NodeIDNil {
		t.Fatalf("initial vote = (%d, %v), want (0, nil)", vote, err)
	}

	// Initial leader must be NodeIDNil
	if l := n.LeaderID(); l != cluster.NodeIDNil {
		t.Fatalf("initial leader = %d, want 0", l)
	}

	// LocalID matches
	if lid := n.LocalID(); lid != 1 {
		t.Fatalf("LocalID = %d, want 1", lid)
	}

	if n.Storage() != s {
		t.Fatalf("Storage instance mismatch")
	}
}

func TestNode_ConstructionValidation(t *testing.T) {
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	defer func() { _ = s.Close() }()

	// Invalid LocalID == 0 rejected
	_, err = raft.NewNode(raft.NodeConfig{
		LocalID: cluster.NodeIDNil,
		Storage: s,
	})
	if err == nil || !stdErrors.Is(err, errors.ErrInvalidNodeID) {
		t.Fatalf("expected ErrInvalidNodeID for NodeIDNil, got %v", err)
	}

	// Nil storage rejected
	_, err = raft.NewNode(raft.NodeConfig{
		LocalID: 1,
		Storage: nil,
	})
	if err == nil {
		t.Fatalf("expected error for nil storage")
	}
}

func TestNode_FollowerToCandidate(t *testing.T) {
	var transitions []string
	hook := func(from, to raft.Role, term raft.Term) {
		transitions = append(transitions, fmt.Sprintf("%s->%s(T%d)", from, to, term))
	}

	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	defer func() { _ = s.Close() }()

	n, err := raft.NewNode(raft.NodeConfig{
		LocalID:        3,
		Storage:        s,
		TransitionHook: hook,
	})
	if err != nil {
		t.Fatalf("NewNode failed: %v", err)
	}
	defer func() { _ = n.Close() }()

	// BecomeCandidate increments term 0 -> 1, votes for self (node 3)
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}

	if r := n.Role(); r != raft.RoleCandidate {
		t.Fatalf("Role = %s, want %s", r, raft.RoleCandidate)
	}

	term, _ := n.Term()
	vote, _ := n.VotedFor()
	if term != 1 || vote != 3 {
		t.Fatalf("Term=%d (want 1), VotedFor=%d (want 3)", term, vote)
	}

	if len(transitions) != 1 || transitions[0] != "Follower->Candidate(T1)" {
		t.Fatalf("transitions mismatch: %v", transitions)
	}

	// Calling BecomeCandidate again starts a new election cycle (Candidate -> Candidate)
	// Term increments 1 -> 2, self-vote is renewed for term 2
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate 2 failed: %v", err)
	}
	term, _ = n.Term()
	vote, _ = n.VotedFor()
	if term != 2 || vote != 3 {
		t.Fatalf("Term=%d (want 2), VotedFor=%d (want 3)", term, vote)
	}
}

func TestNode_CandidateToLeader(t *testing.T) {
	n, _, cleanup := newTestNode(t, 2)
	defer cleanup()

	// Direct Follower -> Leader is illegal
	err := n.BecomeLeader()
	if err == nil || !stdErrors.Is(err, errors.ErrRaftInvalidRoleTransition) {
		t.Fatalf("expected ErrRaftInvalidRoleTransition from Follower to Leader, got %v", err)
	}

	// Enter Candidate
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}

	// Enter Leader
	if err := n.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}

	if r := n.Role(); r != raft.RoleLeader {
		t.Fatalf("Role = %s, want %s", r, raft.RoleLeader)
	}

	// Term must NOT have incremented, vote remains self
	term, _ := n.Term()
	vote, _ := n.VotedFor()
	if term != 1 || vote != 2 {
		t.Fatalf("Term=%d (want 1), VotedFor=%d (want 2)", term, vote)
	}

	if l := n.LeaderID(); l != 2 {
		t.Fatalf("LeaderID = %d, want 2", l)
	}

	// BecomeLeader from Leader is idempotent
	if err := n.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader idempotent failed: %v", err)
	}

	// Direct Leader -> Candidate is illegal
	err = n.BecomeCandidate()
	if err == nil || !stdErrors.Is(err, errors.ErrRaftInvalidRoleTransition) {
		t.Fatalf("expected ErrRaftInvalidRoleTransition from Leader to Candidate, got %v", err)
	}
}

func TestNode_HigherTermDemotion(t *testing.T) {
	n, _, cleanup := newTestNode(t, 1)
	defer cleanup()

	_ = n.BecomeCandidate() // term 1, vote 1
	_ = n.BecomeLeader()    // leader, term 1, vote 1

	// Observe higher term 5
	steppedDown, err := n.ObserveHigherTerm(5)
	if err != nil || !steppedDown {
		t.Fatalf("ObserveHigherTerm(5) = (%v, %v), want (true, nil)", steppedDown, err)
	}

	if r := n.Role(); r != raft.RoleFollower {
		t.Fatalf("Role = %s, want %s", r, raft.RoleFollower)
	}

	term, _ := n.Term()
	vote, _ := n.VotedFor()
	if term != 5 || vote != cluster.NodeIDNil {
		t.Fatalf("Term=%d (want 5), VotedFor=%d (want 0)", term, vote)
	}

	// Observing lower or equal term does not step down or error
	steppedDown, err = n.ObserveHigherTerm(5)
	if err != nil || steppedDown {
		t.Fatalf("ObserveHigherTerm(5) same-term = (%v, %v), want (false, nil)", steppedDown, err)
	}
	steppedDown, err = n.ObserveHigherTerm(3)
	if err != nil || steppedDown {
		t.Fatalf("ObserveHigherTerm(3) lower-term = (%v, %v), want (false, nil)", steppedDown, err)
	}
}

func TestNode_StepDownSameTerm(t *testing.T) {
	n, _, cleanup := newTestNode(t, 4)
	defer cleanup()

	_ = n.BecomeCandidate() // term 1, vote 4

	// Discovers current-term leader (e.g. node 2)
	if err := n.StepDownSameTerm(2); err != nil {
		t.Fatalf("StepDownSameTerm failed: %v", err)
	}

	if r := n.Role(); r != raft.RoleFollower {
		t.Fatalf("Role = %s, want %s", r, raft.RoleFollower)
	}

	// Term and vote MUST be completely unchanged
	term, _ := n.Term()
	vote, _ := n.VotedFor()
	if term != 1 || vote != 4 {
		t.Fatalf("Term=%d (want 1), VotedFor=%d (want 4)", term, vote)
	}

	if l := n.LeaderID(); l != 2 {
		t.Fatalf("LeaderID = %d, want 2", l)
	}
}

func TestNode_TermOverflowDefense(t *testing.T) {
	n, s, cleanup := newTestNode(t, 1)
	defer cleanup()

	// Manually set term to math.MaxUint64
	if err := s.SetHardState(raft.HardState{
		Term:     raft.Term(math.MaxUint64),
		VotedFor: cluster.NodeIDNil,
	}); err != nil {
		t.Fatalf("SetHardState failed: %v", err)
	}

	// Becoming candidate must cleanly reject overflow rather than wrapping to 0
	err := n.BecomeCandidate()
	if err == nil || !stdErrors.Is(err, errors.ErrRaftTermOverflow) {
		t.Fatalf("expected ErrRaftTermOverflow, got %v", err)
	}

	// Role remains Follower
	if r := n.Role(); r != raft.RoleFollower {
		t.Fatalf("Role = %s, want %s", r, raft.RoleFollower)
	}
}

func TestNode_RestartRoleIsAlwaysFollower(t *testing.T) {
	dir := t.TempDir()

	// Session 1: Become candidate and then leader
	s1, _ := raft.OpenStorage(dir)
	n1, _ := raft.NewNode(raft.NodeConfig{LocalID: 1, Storage: s1})
	_ = n1.BecomeCandidate()
	_ = n1.BecomeLeader()

	term1, _ := n1.Term()
	vote1, _ := n1.VotedFor()
	_ = n1.Close()
	_ = s1.Close()

	// Session 2: Reopen storage and construct new Node
	s2, _ := raft.OpenStorage(dir)
	defer func() { _ = s2.Close() }()
	n2, _ := raft.NewNode(raft.NodeConfig{LocalID: 1, Storage: s2})
	defer func() { _ = n2.Close() }()

	// Critical invariant: role is volatile, starts in Follower on reboot
	if r := n2.Role(); r != raft.RoleFollower {
		t.Fatalf("reboot role = %s, want %s", r, raft.RoleFollower)
	}

	// Durable term and vote are fully preserved
	term2, _ := n2.Term()
	vote2, _ := n2.VotedFor()
	if term2 != term1 || vote2 != vote1 {
		t.Fatalf("durable metadata mismatch: (%d, %d) vs (%d, %d)", term2, vote2, term1, vote1)
	}
}

func TestNode_ConcurrentTransitionsNoRace(t *testing.T) {
	n, _, cleanup := newTestNode(t, 1)
	defer cleanup()

	var wg sync.WaitGroup
	const workers = 10

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = n.Role()
				_, _ = n.Term()
				_, _ = n.VotedFor()
				_ = n.LeaderID()

				if workerID%3 == 0 {
					_ = n.BecomeCandidate()
				} else if workerID%3 == 1 {
					_ = n.BecomeLeader()
				} else {
					_, _ = n.ObserveHigherTerm(raft.Term(j + 10))
				}
			}
		}(i)
	}

	wg.Wait()
}
