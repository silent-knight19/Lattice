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

	// Repeated BecomeCandidate call on Candidate is idempotent (Sections 23 & 52)
	// Term remains 1, vote remains 3, no duplicate transition hook
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate idempotent re-call failed: %v", err)
	}
	term, _ = n.Term()
	vote, _ = n.VotedFor()
	if term != 1 || vote != 3 {
		t.Fatalf("idempotent BecomeCandidate modified state: Term=%d (want 1), VotedFor=%d (want 3)", term, vote)
	}
	if len(transitions) != 1 {
		t.Fatalf("unexpected transition hook on idempotent BecomeCandidate: %v", transitions)
	}

	// Calling StartNewElection explicitly starts a new election cycle (Candidate -> Candidate)
	// Term increments 1 -> 2, self-vote is renewed for term 2
	if err := n.StartNewElection(); err != nil {
		t.Fatalf("StartNewElection failed: %v", err)
	}
	term, _ = n.Term()
	vote, _ = n.VotedFor()
	if term != 2 || vote != 3 {
		t.Fatalf("Term=%d (want 2), VotedFor=%d (want 3)", term, vote)
	}
	if len(transitions) != 2 || transitions[1] != "Candidate->Candidate(T2)" {
		t.Fatalf("transitions mismatch after StartNewElection: %v", transitions)
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

func TestNode_RoleTransitionMatrix(t *testing.T) {
	t.Run("Follower_To_Leader_Rejected", func(t *testing.T) {
		n, _, cleanup := newTestNode(t, 1)
		defer cleanup()

		err := n.BecomeLeader()
		if err == nil || !stdErrors.Is(err, errors.ErrRaftInvalidRoleTransition) {
			t.Fatalf("expected ErrRaftInvalidRoleTransition for Follower -> Leader, got %v", err)
		}
		if n.Role() != raft.RoleFollower {
			t.Fatalf("role modified on rejected transition: got %s, want %s", n.Role(), raft.RoleFollower)
		}
	})

	t.Run("Leader_To_Candidate_Rejected", func(t *testing.T) {
		n, _, cleanup := newTestNode(t, 1)
		defer cleanup()

		_ = n.BecomeCandidate()
		_ = n.BecomeLeader()

		err := n.BecomeCandidate()
		if err == nil || !stdErrors.Is(err, errors.ErrRaftInvalidRoleTransition) {
			t.Fatalf("expected ErrRaftInvalidRoleTransition for Leader -> Candidate, got %v", err)
		}
		err = n.StartNewElection()
		if err == nil || !stdErrors.Is(err, errors.ErrRaftInvalidRoleTransition) {
			t.Fatalf("expected ErrRaftInvalidRoleTransition for Leader -> StartNewElection, got %v", err)
		}
		if n.Role() != raft.RoleLeader {
			t.Fatalf("role modified on rejected transition: got %s, want %s", n.Role(), raft.RoleLeader)
		}
	})

	t.Run("Follower_To_Follower_Idempotent", func(t *testing.T) {
		n, _, cleanup := newTestNode(t, 1)
		defer cleanup()

		err := n.BecomeFollower(0, cluster.NodeIDNil)
		if err != nil {
			t.Fatalf("BecomeFollower same-term failed: %v", err)
		}
		if n.Role() != raft.RoleFollower {
			t.Fatalf("Role = %s, want %s", n.Role(), raft.RoleFollower)
		}
	})

	t.Run("Leader_To_Leader_Idempotent", func(t *testing.T) {
		n, _, cleanup := newTestNode(t, 1)
		defer cleanup()

		_ = n.BecomeCandidate()
		_ = n.BecomeLeader()

		err := n.BecomeLeader()
		if err != nil {
			t.Fatalf("BecomeLeader idempotent failed: %v", err)
		}
		if n.Role() != raft.RoleLeader {
			t.Fatalf("Role = %s, want %s", n.Role(), raft.RoleLeader)
		}
	})
}

func TestNode_PersistenceFailureSafety(t *testing.T) {
	// Candidate transition persistence failure (Section 29, 54)
	t.Run("candidate_persistence_failure", func(t *testing.T) {
		n, s, cleanup := newTestNode(t, 1)
		defer cleanup()

		// Inject rename failure when updating state file
		restore := raft.SetRaftRenameFnForTesting(func(oldpath, newpath string) error {
			return fmt.Errorf("injected disk failure during state update")
		})

		err := n.BecomeCandidate()
		if err == nil {
			restore()
			t.Fatalf("expected BecomeCandidate to fail on persistence failure")
		}
		restore()

		// Node role MUST remain Follower
		if r := n.Role(); r != raft.RoleFollower {
			t.Fatalf("role advanced to %s on persistence failure, expected %s", r, raft.RoleFollower)
		}

		// Term and vote MUST NOT have falsely advanced in storage
		hs, err := s.HardState()
		if err != nil {
			t.Fatalf("HardState failed: %v", err)
		}
		if hs.Term != 0 || hs.VotedFor != cluster.NodeIDNil {
			t.Fatalf("persistent state mutated on failed candidate transition: %+v", hs)
		}
	})

	// Higher-term stepdown persistence failure (Section 54)
	t.Run("stepdown_persistence_failure", func(t *testing.T) {
		n, s, cleanup := newTestNode(t, 1)
		defer cleanup()

		_ = n.BecomeCandidate() // term 1, vote 1
		_ = n.BecomeLeader()    // leader, term 1, vote 1

		restore := raft.SetRaftRenameFnForTesting(func(oldpath, newpath string) error {
			return fmt.Errorf("injected disk failure during higher-term stepdown")
		})

		steppedDown, err := n.ObserveHigherTerm(10)
		if err == nil || steppedDown {
			restore()
			t.Fatalf("expected ObserveHigherTerm to fail on persistence failure, got (%v, %v)", steppedDown, err)
		}
		restore()

		// Role remains Leader
		if r := n.Role(); r != raft.RoleLeader {
			t.Fatalf("role altered on persistence failure: got %s, want %s", r, raft.RoleLeader)
		}

		// Term remains 1
		hs, _ := s.HardState()
		if hs.Term != 1 {
			t.Fatalf("term falsely altered on failed stepdown: got %d, want 1", hs.Term)
		}
	})
}

func TestNode_TransitionHook_ReentrantInspection(t *testing.T) {
	// Section 6: Regression Test - Reentrant Transition Hook
	// Hook must be called outside Node.mu so it can safely call Node inspection methods
	// without deadlocking.
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	defer func() { _ = s.Close() }()

	var (
		hookCalled    int
		inspectedRole raft.Role
		inspectedTerm raft.Term
		inspectedVote cluster.NodeID
		inspectedLead cluster.NodeID
		n             *raft.Node
	)

	n, err = raft.NewNode(raft.NodeConfig{
		LocalID: 1,
		Storage: s,
		TransitionHook: func(oldRole, newRole raft.Role, term raft.Term) {
			hookCalled++
			// Re-entrant read calls on Node:
			inspectedRole = n.Role()
			inspectedLead = n.LeaderID()
			inspectedTerm, _ = n.Term()
			inspectedVote, _ = n.VotedFor()
		},
	})
	if err != nil {
		t.Fatalf("NewNode failed: %v", err)
	}
	defer func() { _ = n.Close() }()

	// 1. Follower -> Candidate
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if hookCalled != 1 || inspectedRole != raft.RoleCandidate || inspectedTerm != 1 || inspectedVote != 1 || inspectedLead != cluster.NodeIDNil {
		t.Fatalf("unexpected hook values after BecomeCandidate: called=%d, role=%s, term=%d, vote=%d, lead=%d",
			hookCalled, inspectedRole, inspectedTerm, inspectedVote, inspectedLead)
	}

	// 2. Candidate -> Leader
	if err := n.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}
	if hookCalled != 2 || inspectedRole != raft.RoleLeader || inspectedTerm != 1 || inspectedVote != 1 || inspectedLead != 1 {
		t.Fatalf("unexpected hook values after BecomeLeader: called=%d, role=%s, term=%d, vote=%d, lead=%d",
			hookCalled, inspectedRole, inspectedTerm, inspectedVote, inspectedLead)
	}

	// 3. Leader -> Follower via ObserveHigherTerm
	steppedDown, err := n.ObserveHigherTerm(5)
	if err != nil || !steppedDown {
		t.Fatalf("ObserveHigherTerm failed: (%v, %v)", steppedDown, err)
	}
	if hookCalled != 3 || inspectedRole != raft.RoleFollower || inspectedTerm != 5 || inspectedVote != cluster.NodeIDNil || inspectedLead != cluster.NodeIDNil {
		t.Fatalf("unexpected hook values after ObserveHigherTerm: called=%d, role=%s, term=%d, vote=%d, lead=%d",
			hookCalled, inspectedRole, inspectedTerm, inspectedVote, inspectedLead)
	}

	// 4. Candidate -> Follower via StepDownSameTerm
	if err := n.StartNewElection(); err != nil {
		t.Fatalf("StartNewElection failed: %v", err)
	}
	if err := n.StepDownSameTerm(2); err != nil {
		t.Fatalf("StepDownSameTerm failed: %v", err)
	}
	if inspectedRole != raft.RoleFollower || inspectedLead != 2 {
		t.Fatalf("unexpected hook values after StepDownSameTerm: role=%s, lead=%d", inspectedRole, inspectedLead)
	}
}

func TestNode_Close_LifecycleAndConcurrency(t *testing.T) {
	// Section 8: Close tests
	t.Run("idempotent_and_closed_rejections", func(t *testing.T) {
		n, _, cleanup := newTestNode(t, 1)
		defer cleanup()

		if err := n.Close(); err != nil {
			t.Fatalf("initial Close failed: %v", err)
		}
		// Close() again must be safe and idempotent
		if err := n.Close(); err != nil {
			t.Fatalf("second Close failed: %v", err)
		}

		// Mutations after Close must return ErrRaftStateClosed
		if err := n.BecomeCandidate(); !stdErrors.Is(err, errors.ErrRaftStateClosed) {
			t.Fatalf("expected ErrRaftStateClosed, got %v", err)
		}
		if err := n.StartNewElection(); !stdErrors.Is(err, errors.ErrRaftStateClosed) {
			t.Fatalf("expected ErrRaftStateClosed, got %v", err)
		}
		if err := n.BecomeLeader(); !stdErrors.Is(err, errors.ErrRaftStateClosed) {
			t.Fatalf("expected ErrRaftStateClosed, got %v", err)
		}
		if err := n.BecomeFollower(10, cluster.NodeIDNil); !stdErrors.Is(err, errors.ErrRaftStateClosed) {
			t.Fatalf("expected ErrRaftStateClosed, got %v", err)
		}
		if _, err := n.ObserveHigherTerm(10); !stdErrors.Is(err, errors.ErrRaftStateClosed) {
			t.Fatalf("expected ErrRaftStateClosed, got %v", err)
		}
		if err := n.StepDownSameTerm(2); !stdErrors.Is(err, errors.ErrRaftStateClosed) {
			t.Fatalf("expected ErrRaftStateClosed, got %v", err)
		}
	})

	t.Run("concurrent_close_and_transitions", func(t *testing.T) {
		// Run concurrent transitions while invoking Close
		n, _, cleanup := newTestNode(t, 1)
		defer cleanup()

		var wg sync.WaitGroup
		const numWorkers = 8

		startCh := make(chan struct{})

		for i := 0; i < numWorkers; i++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()
				<-startCh
				for j := 0; j < 50; j++ {
					switch j % 4 {
					case 0:
						_ = n.BecomeCandidate()
					case 1:
						_ = n.BecomeLeader()
					case 2:
						_, _ = n.ObserveHigherTerm(raft.Term(j + 1))
					case 3:
						_ = n.StepDownSameTerm(2)
					}
				}
			}(i)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startCh
			_ = n.Close()
		}()

		close(startCh)
		wg.Wait()

		// Final state must be closed
		if err := n.BecomeCandidate(); !stdErrors.Is(err, errors.ErrRaftStateClosed) {
			t.Fatalf("expected ErrRaftStateClosed after Close, got %v", err)
		}
	})
}

func TestNode_LeaderIdentity_Boundary(t *testing.T) {
	// Section 9: Fix C - Leader identity boundary tests
	n, _, cleanup := newTestNode(t, 1)
	defer cleanup()

	// 1. Self ID rejected as external leader in BecomeFollower
	err := n.BecomeFollower(1, 1)
	if err == nil || !stdErrors.Is(err, errors.ErrRaftInvalidRoleTransition) {
		t.Fatalf("expected ErrRaftInvalidRoleTransition when setting leader to self, got %v", err)
	}

	// 2. Self ID rejected as external leader in StepDownSameTerm
	err = n.StepDownSameTerm(1)
	if err == nil || !stdErrors.Is(err, errors.ErrRaftInvalidRoleTransition) {
		t.Fatalf("expected ErrRaftInvalidRoleTransition when setting leader to self in StepDownSameTerm, got %v", err)
	}

	// 3. NodeIDNil is allowed (unknown leader) in StepDownSameTerm
	err = n.StepDownSameTerm(cluster.NodeIDNil)
	if err != nil {
		t.Fatalf("StepDownSameTerm(NodeIDNil) failed: %v", err)
	}
	if n.LeaderID() != cluster.NodeIDNil {
		t.Fatalf("expected LeaderID to be NodeIDNil, got %d", n.LeaderID())
	}

	// 4. NodeIDNil is allowed in BecomeFollower
	err = n.BecomeFollower(1, cluster.NodeIDNil)
	if err != nil {
		t.Fatalf("BecomeFollower(1, NodeIDNil) failed: %v", err)
	}
	if n.LeaderID() != cluster.NodeIDNil {
		t.Fatalf("expected LeaderID to be NodeIDNil, got %d", n.LeaderID())
	}

	// 5. Valid peer ID is allowed in StepDownSameTerm
	err = n.StepDownSameTerm(2)
	if err != nil {
		t.Fatalf("StepDownSameTerm(2) failed: %v", err)
	}
	if n.LeaderID() != 2 {
		t.Fatalf("expected LeaderID to be 2, got %d", n.LeaderID())
	}

	// 6. Valid peer ID is allowed in BecomeFollower
	err = n.BecomeFollower(2, 3)
	if err != nil {
		t.Fatalf("BecomeFollower(2, 3) failed: %v", err)
	}
	if n.LeaderID() != 3 {
		t.Fatalf("expected LeaderID to be 3, got %d", n.LeaderID())
	}
}
