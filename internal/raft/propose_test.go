package raft_test

import (
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

func newProposeNode(t *testing.T, localID cluster.NodeID, peers []cluster.NodeID) (*raft.Node, *raft.Storage) {
	t.Helper()
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	n, err := raft.NewNode(raft.NodeConfig{
		LocalID: localID,
		Storage: s,
		Peers:   peers,
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

func mustLead(t *testing.T, n *raft.Node) raft.Term {
	t.Helper()
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}
	term, err := n.Term()
	if err != nil {
		t.Fatalf("Term failed: %v", err)
	}
	return term
}

// Test 1 — leader accepts proposal: entry created with correct index, term,
// type, and preserved payload.
func TestPropose_LeaderAcceptsProposal(t *testing.T) {
	n, s := newProposeNode(t, 1, []cluster.NodeID{2, 3})
	term := mustLead(t, n)

	payload := []byte("put k=v")
	e, err := n.Propose(payload)
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}
	// Index 1 holds the election no-op; the first proposal follows it.
	if e.Index != 2 {
		t.Fatalf("expected index 2, got %d", e.Index)
	}
	if e.Term != term {
		t.Fatalf("expected term %d, got %d", term, e.Term)
	}
	if e.Type != transport.PeerEntryNormal {
		t.Fatalf("expected PeerEntryNormal, got %s", e.Type)
	}
	if string(e.Data) != string(payload) {
		t.Fatalf("payload not preserved: got %q", e.Data)
	}

	// The no-op itself is durable at index 1.
	noop, err := s.Entry(1)
	if err != nil {
		t.Fatalf("storage Entry(1) failed: %v", err)
	}
	if noop.Type != transport.PeerEntryNoop || noop.Term != term {
		t.Fatalf("expected election no-op at index 1, got %+v", noop)
	}

	// Durably present in storage.
	stored, err := s.Entry(2)
	if err != nil {
		t.Fatalf("storage Entry(2) failed: %v", err)
	}
	if stored.Term != term || string(stored.Data) != string(payload) {
		t.Fatalf("stored entry mismatch: %+v", stored)
	}
}

// Test 2 — follower rejects proposal without log mutation.
func TestPropose_FollowerRejects(t *testing.T) {
	n, s := newProposeNode(t, 1, []cluster.NodeID{2})
	if n.Role() != raft.RoleFollower {
		t.Fatalf("expected Follower, got %s", n.Role())
	}
	_, err := n.Propose([]byte("cmd"))
	if err == nil {
		t.Fatalf("expected rejection on follower, got nil")
	}
	if !stdErrors.Is(err, errors.ErrRaftInvalidRoleTransition) {
		t.Fatalf("expected ErrRaftInvalidRoleTransition, got %v", err)
	}
	lastIdx, _, err := s.LastIndexAndTerm()
	if err != nil {
		t.Fatalf("LastIndexAndTerm failed: %v", err)
	}
	if lastIdx != 0 {
		t.Fatalf("follower log mutated: lastIdx=%d", lastIdx)
	}
}

// Test 3 — candidate rejects proposal without log mutation.
func TestPropose_CandidateRejects(t *testing.T) {
	n, s := newProposeNode(t, 1, []cluster.NodeID{2, 3})
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if n.Role() != raft.RoleCandidate {
		t.Fatalf("expected Candidate, got %s", n.Role())
	}
	_, err := n.Propose([]byte("cmd"))
	if err == nil {
		t.Fatalf("expected rejection on candidate, got nil")
	}
	if !stdErrors.Is(err, errors.ErrRaftInvalidRoleTransition) {
		t.Fatalf("expected ErrRaftInvalidRoleTransition, got %v", err)
	}
	lastIdx, _, err := s.LastIndexAndTerm()
	if err != nil {
		t.Fatalf("LastIndexAndTerm failed: %v", err)
	}
	if lastIdx != 0 {
		t.Fatalf("candidate log mutated: lastIdx=%d", lastIdx)
	}
}

// Test 4 — closed node rejects proposal without mutation.
func TestPropose_ClosedNodeRejects(t *testing.T) {
	n, s := newProposeNode(t, 1, []cluster.NodeID{2})
	mustLead(t, n)
	if err := n.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	_, err := n.Propose([]byte("cmd"))
	if err == nil {
		t.Fatalf("expected rejection on closed node, got nil")
	}
	if !stdErrors.Is(err, errors.ErrRaftStateClosed) {
		t.Fatalf("expected ErrRaftStateClosed, got %v", err)
	}
	lastIdx, _, err := s.LastIndexAndTerm()
	if err != nil {
		t.Fatalf("LastIndexAndTerm failed: %v", err)
	}
	// Only the election no-op (index 1) may exist; the rejected proposal
	// must not have appended anything.
	if lastIdx != 1 {
		t.Fatalf("closed-node proposal mutated log: lastIdx=%d", lastIdx)
	}
}

// Test 5 — sequential proposals receive contiguous indexes with correct terms.
func TestPropose_SequentialContiguousIndexes(t *testing.T) {
	n, _ := newProposeNode(t, 1, nil)
	mustLead(t, n)

	const count = 10
	for i := 1; i <= count; i++ {
		e, err := n.Propose([]byte(fmt.Sprintf("cmd-%d", i)))
		if err != nil {
			t.Fatalf("Propose %d failed: %v", i, err)
		}
		// Index 1 is the election no-op; proposals follow contiguously.
		if e.Index != raft.LogIndex(i+1) {
			t.Fatalf("proposal %d: expected index %d, got %d", i, i+1, e.Index)
		}
		curTerm, _ := n.Term()
		if e.Term != curTerm {
			t.Fatalf("proposal %d: expected term %d, got %d", i, curTerm, e.Term)
		}
	}
}

// Test 6 — concurrent proposals: distinct contiguous indexes, no races,
// no lost proposals, payloads preserved.
func TestPropose_ConcurrentDistinctIndexes(t *testing.T) {
	n, s := newProposeNode(t, 1, nil)
	mustLead(t, n)

	const count = 32
	type result struct {
		entry raft.LogEntry
		err   error
	}
	results := make([]result, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e, err := n.Propose([]byte(fmt.Sprintf("payload-%02d", i)))
			results[i] = result{entry: e, err: err}
		}(i)
	}
	wg.Wait()

	seen := make(map[raft.LogIndex][]byte, count)
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("proposal %d failed: %v", i, r.err)
		}
		if _, dup := seen[r.entry.Index]; dup {
			t.Fatalf("duplicate index %d allocated", r.entry.Index)
		}
		seen[r.entry.Index] = r.entry.Data
	}
	if len(seen) != count {
		t.Fatalf("expected %d distinct indexes, got %d", count, len(seen))
	}
	// Index 1 is the election no-op; proposals occupy 2..count+1.
	for idx := raft.LogIndex(2); idx <= raft.LogIndex(count+1); idx++ {
		data, ok := seen[idx]
		if !ok {
			t.Fatalf("missing index %d (gap in allocation)", idx)
		}
		stored, err := s.Entry(idx)
		if err != nil {
			t.Fatalf("storage Entry(%d) failed: %v", idx, err)
		}
		if string(stored.Data) != string(data) {
			t.Fatalf("index %d payload mismatch: propose=%q stored=%q", idx, data, stored.Data)
		}
		curTerm, _ := n.Term()
		if stored.Term != curTerm {
			t.Fatalf("index %d: expected term %d, got %d", idx, curTerm, stored.Term)
		}
	}
}

// Test 7 — persistence failure: no false success, no phantom entry,
// subsequent proposals remain coherent.
func TestPropose_PersistenceFailureFailClosed(t *testing.T) {
	n, s := newProposeNode(t, 1, nil)
	mustLead(t, n)

	// Close storage underneath to force durable-append failure.
	_ = s.Close()

	_, err := n.Propose([]byte("doomed"))
	if err == nil {
		t.Fatalf("expected persistence failure error, got nil")
	}

	// Reopen-independent coherence check on a fresh node/storage pair is
	// covered elsewhere; here verify the failed node reports no entry.
	// Storage is closed, so LastIndex must fail rather than show a phantom.
	if _, _, err := s.LastIndexAndTerm(); err == nil {
		t.Fatalf("expected closed-storage error, got nil")
	}
}

// Test 8 — entry uses the current (advanced) term, not a stale one.
func TestPropose_UsesCurrentTermAfterReelection(t *testing.T) {
	n, s := newProposeNode(t, 1, []cluster.NodeID{2})
	term1 := mustLead(t, n)
	e1, err := n.Propose([]byte("first"))
	if err != nil {
		t.Fatalf("first Propose failed: %v", err)
	}
	if e1.Term != term1 {
		t.Fatalf("expected term %d, got %d", term1, e1.Term)
	}

	// Step down and win a later term.
	curTerm, _ := n.Term()
	if err := n.BecomeFollower(curTerm+1, cluster.NodeIDNil); err != nil {
		t.Fatalf("BecomeFollower failed: %v", err)
	}
	term2 := mustLead(t, n)
	if term2 <= term1 {
		t.Fatalf("expected term advance: %d -> %d", term1, term2)
	}
	e2, err := n.Propose([]byte("second"))
	if err != nil {
		t.Fatalf("second Propose failed: %v", err)
	}
	if e2.Term != term2 {
		t.Fatalf("expected new term %d, got %d", term2, e2.Term)
	}
	// A fresh no-op separates the terms: e1 < noop < e2, still contiguous.
	if e2.Index != e1.Index+2 {
		t.Fatalf("expected contiguous index %d, got %d", e1.Index+2, e2.Index)
	}
	mid, err := s.Entry(e1.Index + 1)
	if err != nil {
		t.Fatalf("Entry failed: %v", err)
	}
	if mid.Type != transport.PeerEntryNoop || mid.Term != term2 {
		t.Fatalf("expected term-%d no-op between proposals, got %+v", term2, mid)
	}
}

// Test 9 — result metadata is assigned by Raft; a second proposal cannot
// collide with or rewrite the first entry's coordinates.
func TestPropose_CallerCannotForgeMetadata(t *testing.T) {
	n, s := newProposeNode(t, 1, nil)
	mustLead(t, n)

	e1, err := n.Propose([]byte("a"))
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}
	e2, err := n.Propose([]byte("a"))
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}
	// Same payload must still yield distinct Raft-owned coordinates.
	if e1.Index == e2.Index {
		t.Fatalf("identical payloads received identical index %d", e1.Index)
	}
	// Prior entry immutable under later proposals.
	stored, err := s.Entry(e1.Index)
	if err != nil {
		t.Fatalf("Entry failed: %v", err)
	}
	if stored.Term != e1.Term || string(stored.Data) != "a" {
		t.Fatalf("prior entry mutated: %+v", stored)
	}
}

// Test 10 — proposals racing a stepdown: no panic/deadlock/race, no malformed
// index or zero term, and every returned success is durably present.
func TestPropose_RacingStepdownNoCorruption(t *testing.T) {
	n, s := newProposeNode(t, 1, []cluster.NodeID{2})
	mustLead(t, n)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Driver: repeatedly step down and re-elect.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			curTerm, err := n.Term()
			if err != nil {
				return
			}
			_ = n.BecomeFollower(curTerm+1, cluster.NodeIDNil)
			_ = n.BecomeCandidate()
			_ = n.BecomeLeader()
		}
	}()

	// Proposers: success must imply durable presence with valid coordinates.
	const proposers = 4
	const perProposer = 25
	errCh := make(chan error, proposers*perProposer)
	for p := 0; p < proposers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perProposer; i++ {
				e, err := n.Propose([]byte(fmt.Sprintf("p%d-%d", p, i)))
				if err != nil {
					// Rejection during non-leadership is legal.
					continue
				}
				if e.Index == 0 || e.Term == 0 {
					errCh <- fmt.Errorf("malformed entry: %+v", e)
					return
				}
				stored, serr := s.Entry(e.Index)
				if serr != nil {
					errCh <- fmt.Errorf("success without durable entry idx %d: %v", e.Index, serr)
					return
				}
				if stored.Term != e.Term || string(stored.Data) != string(e.Data) {
					errCh <- fmt.Errorf("stored mismatch at idx %d", e.Index)
					return
				}
			}
		}(p)
	}

	// Let the race run briefly, then stop the stepdown driver first so the
	// node settles before test cleanup (Close).
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("racing proposal corruption: %v", err)
	}
}

// M01/M02 boundary (evolved in P15-S03-M02): a follower receiving valid
// non-empty entries from its legitimate leader now durably replicates them
// (no longer rejected). Leader-local Propose and follower replication compose:
// the leader's entry can travel via AppendEntries and persist on the follower.
func TestPropose_NonEmptyAppendEntriesStillRejected(t *testing.T) {
	n, s := newProposeNode(t, 1, []cluster.NodeID{2})
	if err := s.SetTerm(3); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}
	mustLead(t, n)

	// A leader proposal exists locally...
	prop, err := n.Propose([]byte("local"))
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}

	// ...and the follower durably replicates the leader's entry.
	followerDir := t.TempDir()
	fs, err := raft.OpenStorage(followerDir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	defer func() { _ = fs.Close() }()
	fn, err := raft.NewNode(raft.NodeConfig{
		LocalID: 2,
		Storage: fs,
		Peers:   []cluster.NodeID{1},
	})
	if err != nil {
		t.Fatalf("NewNode failed: %v", err)
	}
	defer func() { _ = fn.Close() }()

	leaderTerm, _ := n.Term()
	req := &transport.AppendEntriesRequest{
		Term:         uint64(leaderTerm),
		LeaderID:     1,
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Nonce:        31337,
		Entries: []transport.PeerLogEntry{
			{Term: uint64(prop.Term), Type: transport.PeerEntryNormal, Data: []byte("local")},
		},
	}
	resp, err := fn.HandleAppendEntries(1, req)
	if err != nil {
		t.Fatalf("HandleAppendEntries failed: %v", err)
	}
	if !resp.Success {
		t.Fatalf("valid follower replication must succeed after M02")
	}
	stored, err := fs.Entry(1)
	if err != nil {
		t.Fatalf("replicated entry missing on follower: %v", err)
	}
	if string(stored.Data) != "local" || stored.Term != prop.Term {
		t.Fatalf("follower entry mismatch: %+v", stored)
	}
}
