package raft_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/raft"
	"github.com/silent-knight19/lattice/internal/transport"
)

func FuzzHandleAppendEntriesResponse(f *testing.F) {
	// Seed corpus: normal, stale, higher-term, unknown-peer, max values.
	f.Add(uint64(2), uint64(1), uint64(1), uint64(0))
	f.Add(uint64(2), uint64(0), uint64(1), uint64(0))
	f.Add(uint64(3), uint64(10), uint64(0), uint64(0))
	f.Add(uint64(99), uint64(50), uint64(1), uint64(5))
	f.Add(uint64(2), ^uint64(0), uint64(1), ^uint64(0))
	f.Add(uint64(0), uint64(1), uint64(0), uint64(0))

	f.Fuzz(func(t *testing.T, fromPeerIDRaw, respTerm, successRaw, matchIndex uint64) {
		success := successRaw%2 != 0
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

		// Elect a leader with one log entry so progress/commit have meaning.
		_ = node.BecomeCandidate()
		_ = node.BecomeLeader()
		termBefore, _ := node.Term()
		commitBefore := node.CommitIndex()

		resp := &transport.AppendEntriesResponse{
			Term:       respTerm,
			Success:    success,
			MatchIndex: matchIndex,
		}
		_ = node.HandleAppendEntriesResponse(cluster.NodeID(fromPeerIDRaw), resp)

		termAfter, _ := node.Term()
		commitAfter := node.CommitIndex()
		lastIdx, _, _ := s.LastIndexAndTerm()

		// Property 1: no panic (reaching here).
		// Property 2: term never decreases.
		if termAfter < termBefore {
			t.Fatalf("term decreased: %d -> %d", termBefore, termAfter)
		}
		// Property 3: commit never decreases (volatile field is monotonic).
		if commitAfter < commitBefore {
			t.Fatalf("commit decreased: %d -> %d", commitBefore, commitAfter)
		}
		// Property 4: commit never exceeds the leader's durable log.
		if commitAfter > lastIdx {
			t.Fatalf("commit %d exceeds LastIndex %d", commitAfter, lastIdx)
		}
		// Property 5: recorded match never exceeds the leader's log
		// (inflated progress is ignored, never stored). nextIndex keeps its
		// floor while leader replication state exists (nil after stepdown).
		stillLeader := node.Role() == raft.RoleLeader
		for _, p := range []cluster.NodeID{2, 3, 4} {
			if m := node.MatchIndex()[p]; m > lastIdx {
				t.Fatalf("matchIndex[%d] = %d exceeds LastIndex %d", p, m, lastIdx)
			}
			if stillLeader {
				if nx := node.NextIndex()[p]; nx < 1 {
					t.Fatalf("nextIndex[%d] = %d below floor 1", p, nx)
				}
			}
		}
		// Property 6: unknown senders cannot advance commit.
		if fromPeerIDRaw == 0 || fromPeerIDRaw == 1 || fromPeerIDRaw > 4 {
			if commitAfter != commitBefore {
				t.Fatalf("invalid sender %d changed commit %d -> %d", fromPeerIDRaw, commitBefore, commitAfter)
			}
		}
	})
}

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

func FuzzHandleAppendEntries(f *testing.F) {
	// Seed 1: Valid empty heartbeat from peer 2
	f.Add(uint64(2), uint64(2), uint64(1), uint64(0), uint64(0), uint64(0), uint64(100), byte(0), byte(0))

	// Seed 2: Stale heartbeat
	f.Add(uint64(2), uint64(2), uint64(0), uint64(0), uint64(0), uint64(0), uint64(101), byte(1), byte(0))

	// Seed 3: Higher term heartbeat
	f.Add(uint64(2), uint64(2), uint64(10), uint64(0), uint64(0), uint64(0), uint64(102), byte(2), byte(0))

	// Seed 4: Mismatched leader and sender ID
	f.Add(uint64(2), uint64(3), uint64(2), uint64(0), uint64(0), uint64(0), uint64(103), byte(0), byte(0))

	// Seed 5: Self sender / leader
	f.Add(uint64(1), uint64(1), uint64(2), uint64(0), uint64(0), uint64(0), uint64(104), byte(0), byte(0))

	// Seed 6: Unknown peer
	f.Add(uint64(99), uint64(99), uint64(2), uint64(0), uint64(0), uint64(0), uint64(105), byte(0), byte(0))

	// Seed 7: Non-empty entries entry count byte
	f.Add(uint64(2), uint64(2), uint64(2), uint64(1), uint64(1), uint64(0), uint64(106), byte(1), byte(1))

	f.Fuzz(func(t *testing.T, fromPeerIDRaw, leaderIDRaw, reqTerm, prevLogIndex, prevLogTerm, leaderCommit, nonce uint64, initialRoleAction byte, hasEntries byte) {
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
			// Follower (term 1)
			_ = s.SetTerm(1)
		case 1:
			// Candidate (term 2)
			_ = s.SetTerm(1)
			_ = node.BecomeCandidate()
		case 2:
			// Leader (term 2)
			_ = s.SetTerm(1)
			_ = node.BecomeCandidate()
			_ = node.BecomeLeader()
		}

		termBefore, _ := node.Term()
		roleBefore := node.Role()
		leaderBefore := node.LeaderID()
		lastIdxBefore, _, _ := s.LastIndexAndTerm()

		// Snapshot full log contents for rejection immutability checks.
		var logBefore []raft.LogEntry
		for idx := raft.LogIndex(1); idx <= lastIdxBefore; idx++ {
			if e, eerr := s.Entry(idx); eerr == nil {
				logBefore = append(logBefore, e)
			}
		}

		var entries []transport.PeerLogEntry
		if hasEntries%2 != 0 {
			entries = []transport.PeerLogEntry{
				{Term: reqTerm, Type: transport.PeerEntryNormal, Data: []byte("test")},
			}
		}

		req := &transport.AppendEntriesRequest{
			Term:         reqTerm,
			LeaderID:     cluster.NodeID(leaderIDRaw),
			PrevLogIndex: prevLogIndex,
			PrevLogTerm:  prevLogTerm,
			LeaderCommit: leaderCommit,
			Nonce:        nonce,
			Entries:      entries,
		}

		fromPeerID := cluster.NodeID(fromPeerIDRaw)
		resp, err := node.HandleAppendEntries(fromPeerID, req)

		// Property 1: No panic (verified by reaching here)

		// Property 2: Term never decreases
		termAfter, _ := node.Term()
		if termAfter < termBefore {
			t.Fatalf("term decreased: termBefore=%d, termAfter=%d", termBefore, termAfter)
		}

		// Property 3: Invalid sender / unknown peer / self / mismatch cannot become leader
		if !fromPeerID.IsValid() || fromPeerID == 1 || fromPeerID > 4 || fromPeerID != req.LeaderID {
			if err == nil {
				t.Fatalf("expected error for invalid/unknown sender %d (leaderID %d)", fromPeerID, req.LeaderID)
			}
			if node.LeaderID() != leaderBefore {
				t.Fatalf("invalid sender %d caused leaderID to change: before=%d, after=%d", fromPeerID, leaderBefore, node.LeaderID())
			}
		}

		// Property 4: Stale term cannot change leaderID
		if reqTerm < uint64(termBefore) {
			if resp != nil && resp.Success {
				t.Fatalf("stale heartbeat succeeded: reqTerm=%d, termBefore=%d", reqTerm, termBefore)
			}
			if node.LeaderID() != leaderBefore {
				t.Fatalf("stale heartbeat changed leaderID: before=%d, after=%d", leaderBefore, node.LeaderID())
			}
		}

		// Property 5: Higher term never leaves node in Leader/Candidate
		if reqTerm > uint64(termBefore) && err == nil {
			if r := node.Role(); r != raft.RoleFollower {
				t.Fatalf("node failed to step down on higher term: %s", r)
			}
			if node.HeartbeatRunning() {
				t.Fatalf("heartbeat scheduler still running after higher term stepdown")
			}
		}

		// Property 6: Same-term valid heartbeat causes Candidate -> Follower
		if reqTerm == uint64(termBefore) && roleBefore == raft.RoleCandidate && err == nil && resp != nil && resp.Success {
			if node.Role() != raft.RoleFollower {
				t.Fatalf("candidate failed to step down on same-term valid heartbeat: %s", node.Role())
			}
		}

		// Property 7 & 8 (M02): rejected requests must not mutate the log at
		// all; accepted requests must leave a structurally valid contiguous
		// log; empty accepted heartbeats must not mutate the log.
		lastIdxAfter, _, _ := s.LastIndexAndTerm()
		accepted := err == nil && resp != nil && resp.Success
		if !accepted {
			if lastIdxAfter != lastIdxBefore {
				t.Fatalf("rejected request mutated log length: before=%d, after=%d", lastIdxBefore, lastIdxAfter)
			}
			// Full-content check: no entry may differ after rejection.
			for i, want := range logBefore {
				got, aerr := s.Entry(raft.LogIndex(i + 1))
				if aerr != nil {
					t.Fatalf("rejected request removed entry %d: %v", i+1, aerr)
				}
				if got.Term != want.Term || got.Type != want.Type || string(got.Data) != string(want.Data) {
					t.Fatalf("rejected request mutated entry %d: before=%+v after=%+v", i+1, want, got)
				}
			}
		} else {
			// Accepted: log must be contiguous 1..lastIdxAfter with valid
			// entries, and MatchIndex must equal PrevLogIndex+len(entries).
			if resp.MatchIndex != prevLogIndex+uint64(len(entries)) {
				t.Fatalf("MatchIndex %d != PrevLogIndex %d + %d entries",
					resp.MatchIndex, prevLogIndex, len(entries))
			}
			for idx := raft.LogIndex(1); idx <= lastIdxAfter; idx++ {
				e, eerr := s.Entry(idx)
				if eerr != nil {
					t.Fatalf("accepted log has gap at index %d: %v", idx, eerr)
				}
				if e.Index != idx || e.Term == 0 {
					t.Fatalf("accepted log has invalid entry at %d: %+v", idx, e)
				}
			}
			if len(entries) == 0 && lastIdxAfter != lastIdxBefore {
				t.Fatalf("empty heartbeat mutated log: before=%d, after=%d", lastIdxBefore, lastIdxAfter)
			}
		}

		// Property 9: Repeated valid heartbeat is idempotent
		if err == nil && resp != nil && resp.Success {
			resp2, err2 := node.HandleAppendEntries(fromPeerID, req)
			if err2 != nil || resp2 == nil || !resp2.Success {
				t.Fatalf("repeated valid heartbeat failed: resp2=%+v, err2=%v", resp2, err2)
			}
			if node.Role() != raft.RoleFollower {
				t.Fatalf("role changed after repeated heartbeat: %s", node.Role())
			}
		}
	})
}
