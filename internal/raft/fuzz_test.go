package raft_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/raft"
	"github.com/silent-knight19/lattice/internal/transport"
)

func FuzzRecoverStorage(f *testing.F) {
	// Seed 1: Empty state
	f.Add([]byte{})

	// Seed 2: Minimal valid state record (28 bytes)
	seedState := make([]byte, raft.StateRecordSize)
	f.Add(seedState)

	// Seed 3: Arbitrary random sequence
	f.Add([]byte("random corrupt bytes with headers and invalid sizes"))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1024*1024 { // Bound memory to 1 MiB
			return
		}

		dir := t.TempDir()

		// Test state file recovery fuzzing
		stateFile := filepath.Join(dir, raft.StateFilename)
		if err := os.WriteFile(stateFile, data, 0600); err != nil {
			return
		}

		// OpenStorage must either succeed or return a clean error without panic
		s, err := raft.OpenStorage(dir)
		if err == nil && s != nil {
			_ = s.Close()
		}

		// Test log file recovery fuzzing
		os.Remove(stateFile)
		logFile := filepath.Join(dir, raft.LogFilename)
		if err := os.WriteFile(logFile, data, 0600); err != nil {
			return
		}

		s2, err := raft.OpenStorage(dir)
		if err == nil && s2 != nil {
			_ = s2.Close()
		}
	})
}

func FuzzRoleTransitions(f *testing.F) {
	// Seed sequences of operations: 0=BecomeCandidate, 1=BecomeLeader, 2=StepDownSameTerm, 3=ObserveHigherTerm
	f.Add(uint64(1), []byte{0, 1, 2, 3})
	f.Add(uint64(5), []byte{0, 0, 1, 0, 3})
	f.Add(uint64(10), []byte{3, 2, 1, 0})

	f.Fuzz(func(t *testing.T, localIDRaw uint64, ops []byte) {
		if localIDRaw == 0 {
			return
		}
		if len(ops) > 20 { // Bound iteration length
			return
		}

		dir := t.TempDir()
		s, err := raft.OpenStorage(dir)
		if err != nil {
			return
		}
		defer func() { _ = s.Close() }()

		node, err := raft.NewNode(raft.NodeConfig{
			LocalID: cluster.NodeID(localIDRaw),
			Storage: s,
		})
		if err != nil {
			return
		}
		defer func() { _ = node.Close() }()

		for _, op := range ops {
			switch op % 4 {
			case 0:
				_ = node.BecomeCandidate()
			case 1:
				_ = node.BecomeLeader()
			case 2:
				_ = node.StepDownSameTerm(cluster.NodeID(1))
			case 3:
				term, err := node.Term()
				if err == nil {
					_, _ = node.ObserveHigherTerm(term + 1)
				}
			}

			// Invariant verification: role must always be valid
			r := node.Role()
			if !r.Valid() {
				t.Fatalf("invalid role observed: %v", r)
			}
		}
	})
}

func FuzzHandleRequestVote(f *testing.F) {
	// Section 61: Fuzz RequestVote decision function with extreme inputs (0, MaxUint64, malformed)
	f.Add(uint64(2), uint64(2), uint64(1), uint64(0), uint64(0), uint64(100))
	f.Add(uint64(2), uint64(3), uint64(5), uint64(10), uint64(2), uint64(200))
	f.Add(uint64(0), uint64(0), uint64(0), uint64(0), uint64(0), uint64(0))
	f.Add(uint64(1), uint64(1), uint64(1<<63), uint64(1<<63), uint64(1<<63), uint64(300))
	f.Add(uint64(2), uint64(2), ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0))
	f.Add(uint64(2), uint64(2), uint64(1), uint64(100), uint64(0), uint64(400)) // invalid coordinates (index > 0, term 0)

	f.Fuzz(func(t *testing.T, fromPeerIDRaw, candIDRaw, term, lastLogIndex, lastLogTerm, nonce uint64) {
		dir := t.TempDir()
		s, err := raft.OpenStorage(dir)
		if err != nil {
			return
		}
		defer func() { _ = s.Close() }()

		node, err := raft.NewNode(raft.NodeConfig{
			LocalID: 1,
			Storage: s,
			Peers:   []cluster.NodeID{2, 3},
		})
		if err != nil {
			return
		}
		defer func() { _ = node.Close() }()

		req := &transport.RequestVoteRequest{
			Term:         term,
			CandidateID:  cluster.NodeID(candIDRaw),
			LastLogIndex: lastLogIndex,
			LastLogTerm:  lastLogTerm,
			Nonce:        nonce,
		}

		// HandleRequestVote must safely evaluate or reject without panicking
		resp, err := node.HandleRequestVote(cluster.NodeID(fromPeerIDRaw), req)
		if err == nil && resp != nil {
			// Invariant: if vote is granted, response term must be >= request term
			if resp.VoteGranted && resp.Term < term {
				t.Fatalf("response term %d < request term %d on granted vote", resp.Term, term)
			}
		}
	})
}

func FuzzHandleRequestVoteResponse(f *testing.F) {
	// Seed corpus with normal, boundary, and extreme inputs
	f.Add(uint64(2), uint64(1), true, uint8(0))
	f.Add(uint64(2), uint64(0), true, uint8(1))
	f.Add(uint64(3), uint64(10), false, uint8(0))
	f.Add(uint64(0), uint64(0), false, uint8(2))
	f.Add(uint64(1), uint64(1), true, uint8(0))
	f.Add(uint64(99), uint64(1), true, uint8(1))
	f.Add(^uint64(0), ^uint64(0), true, uint8(0))
	f.Add(uint64(2), ^uint64(0), false, uint8(0))

	f.Fuzz(func(t *testing.T, fromPeerIDRaw, respTerm uint64, voteGranted bool, initialRoleAction uint8) {
		dir := t.TempDir()
		s, err := raft.OpenStorage(dir)
		if err != nil {
			return
		}
		defer func() { _ = s.Close() }()

		node, err := raft.NewNode(raft.NodeConfig{
			LocalID: 1,
			Storage: s,
			Peers:   []cluster.NodeID{2, 3, 4},
		})
		if err != nil {
			return
		}
		defer func() { _ = node.Close() }()

		// Setup initial role
		switch initialRoleAction % 3 {
		case 0:
			// RoleFollower (term 0)
		case 1:
			// RoleCandidate (term 1)
			_ = node.BecomeCandidate()
		case 2:
			// RoleLeader (term 1)
			_ = node.BecomeCandidate()
			_ = node.BecomeLeader()
		}

		termBefore, _ := node.Term()
		roleBefore := node.Role()
		votesBefore := node.GrantedVotesCount()

		resp := &transport.RequestVoteResponse{
			Term:        respTerm,
			VoteGranted: voteGranted,
		}

		fromPeerID := cluster.NodeID(fromPeerIDRaw)
		err = node.HandleRequestVoteResponse(fromPeerID, resp)

		// Verification of Core Properties (Section 27):
		// Property 8: No panic for arbitrary protocol values (verified by reaching here)

		// Property 7: Term never decreases
		termAfter, _ := node.Term()
		if termAfter < termBefore {
			t.Fatalf("term decreased: termBefore=%d, termAfter=%d", termBefore, termAfter)
		}

		// Property 1: Vote count never exceeds unique remote voters + self (3 + 1 = 4)
		votesAfter := node.GrantedVotesCount()
		if votesAfter > 4 {
			t.Fatalf("vote count %d exceeded maximum possible (4)", votesAfter)
		}

		// Property 9 & 10: Invalid sender / self sender cannot become a vote
		if !fromPeerID.IsValid() || fromPeerID == 1 || fromPeerID > 4 {
			if err == nil {
				t.Fatalf("expected error for invalid or unknown peer %d", fromPeerID)
			}
			if votesAfter > votesBefore {
				t.Fatalf("votes increased after invalid sender %d", fromPeerID)
			}
		}

		// Property 3: Stale term cannot increase vote count
		if respTerm < uint64(termBefore) && votesAfter > votesBefore {
			t.Fatalf("votes increased on stale term %d < %d", respTerm, termBefore)
		}

		// Property 4: Higher term causes stepdown to Follower and clears votes
		if respTerm > uint64(termBefore) && err == nil {
			if r := node.Role(); r != raft.RoleFollower {
				t.Fatalf("node failed to step down on higher term: %s", r)
			}
			if votesAfter != 0 {
				t.Fatalf("votes not cleared on stepdown: %d", votesAfter)
			}
		}

		// Property 5: Only Candidate can transition through quorum
		if roleBefore != raft.RoleCandidate && node.Role() == raft.RoleLeader && roleBefore != raft.RoleLeader {
			t.Fatalf("non-candidate %s transitioned to leader on vote response", roleBefore)
		}

		// Property 2: Duplicate peer response cannot increase vote count
		if err == nil && roleBefore == raft.RoleCandidate && node.Role() == raft.RoleCandidate {
			votesMid := node.GrantedVotesCount()
			_ = node.HandleRequestVoteResponse(fromPeerID, resp)
			votesPostDup := node.GrantedVotesCount()
			if votesPostDup > votesMid {
				t.Fatalf("duplicate response increased vote count: %d -> %d", votesMid, votesPostDup)
			}
		}
	})
}
