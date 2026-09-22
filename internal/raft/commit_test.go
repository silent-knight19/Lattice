package raft_test

import (
	stdErrors "errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/raft"
	"github.com/silent-knight19/lattice/internal/transport"
)

func mustCommitLead(t *testing.T, n *raft.Node) raft.Term {
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

func commitResp(term uint64, success bool, match uint64) *transport.AppendEntriesResponse {
	return &transport.AppendEntriesResponse{Term: term, Success: success, MatchIndex: match}
}

// Test 1 — N=1 commits a local current-term proposal immediately.
func TestCommit_SingleNodeImmediateCommit(t *testing.T) {
	n, _ := newReplNode(t, 1, nil)
	mustCommitLead(t, n)
	// The election no-op commits immediately (leader alone is quorum).
	if got := n.CommitIndex(); got != 1 {
		t.Fatalf("initial commitIndex = %d, want 1 (election no-op)", got)
	}
	e1, err := n.Propose([]byte("a"))
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}
	if got := n.CommitIndex(); got != e1.Index {
		t.Fatalf("commitIndex = %d, want %d", got, e1.Index)
	}
	e2, err := n.Propose([]byte("b"))
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}
	if got := n.CommitIndex(); got != e2.Index {
		t.Fatalf("commitIndex = %d, want %d", got, e2.Index)
	}
}

// Test 2 — N=3 local append alone does not commit.
func TestCommit_NoCommitWithoutQuorum(t *testing.T) {
	n, _ := newReplNode(t, 1, []cluster.NodeID{2, 3})
	mustCommitLead(t, n)
	e, err := n.Propose([]byte("a"))
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}
	if got := n.CommitIndex(); got != 0 {
		t.Fatalf("commitIndex = %d, want 0 (no follower acks)", got)
	}
	_ = e
}

// Test 3 — N=3 leader + one follower commits; progress state exact.
func TestCommit_LeaderPlusOneFollowerCommits(t *testing.T) {
	n, _ := newReplNode(t, 1, []cluster.NodeID{2, 3})
	term := mustCommitLead(t, n)
	e, err := n.Propose([]byte("a"))
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}
	if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(term), true, uint64(e.Index))); err != nil {
		t.Fatalf("HandleAppendEntriesResponse failed: %v", err)
	}
	if got := n.MatchIndex()[2]; got != e.Index {
		t.Fatalf("matchIndex[2] = %d, want %d", got, e.Index)
	}
	if got := n.NextIndex()[2]; got != e.Index+1 {
		t.Fatalf("nextIndex[2] = %d, want %d", got, e.Index+1)
	}
	if got := n.CommitIndex(); got != e.Index {
		t.Fatalf("commitIndex = %d, want %d", got, e.Index)
	}
}

// Test 4 — follower-only progress on old-term entries cannot commit; the
// leader's own current-term coverage is what counts.
func TestCommit_FollowerOnlyOldTermCannotCommit(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2, 3})
	seedReplLog(t, s, []replSpec{{1, "old"}})
	term := mustCommitLead(t, n) // term 2, log holds only a term-1 entry
	if term != 2 {
		t.Fatalf("expected term 2, got %d", term)
	}
	if err := n.HandleAppendEntriesResponse(2, commitResp(2, true, 1)); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if err := n.HandleAppendEntriesResponse(3, commitResp(2, true, 1)); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if got := n.CommitIndex(); got != 0 {
		t.Fatalf("old-term quorum must not commit directly: commitIndex = %d", got)
	}
}

// Test 5 — duplicate success responses are harmless no-ops.
func TestCommit_DuplicateResponseIdempotent(t *testing.T) {
	n, _ := newReplNode(t, 1, []cluster.NodeID{2, 3})
	term := mustCommitLead(t, n)
	e, err := n.Propose([]byte("a"))
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(term), true, uint64(e.Index))); err != nil {
			t.Fatalf("response %d failed: %v", i, err)
		}
	}
	if got := n.MatchIndex()[2]; got != e.Index {
		t.Fatalf("matchIndex[2] = %d, want %d", got, e.Index)
	}
	if got := n.CommitIndex(); got != e.Index {
		t.Fatalf("commitIndex = %d, want %d", got, e.Index)
	}
}

// Test 6 — out-of-order responses cannot regress progress or commit.
func TestCommit_OutOfOrderResponseNoRegression(t *testing.T) {
	n, _ := newReplNode(t, 1, []cluster.NodeID{2, 3})
	mustCommitLead(t, n)
	// Build log 1..5 in the current term via proposals.
	for i := 0; i < 5; i++ {
		if _, err := n.Propose([]byte(fmt.Sprintf("e%d", i))); err != nil {
			t.Fatalf("Propose failed: %v", err)
		}
	}
	curTerm, _ := n.Term()
	if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(curTerm), true, 5)); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if got := n.CommitIndex(); got != 5 {
		t.Fatalf("commitIndex = %d, want 5", got)
	}
	// Delayed lower response: progress and commit must stand.
	if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(curTerm), true, 3)); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if got := n.MatchIndex()[2]; got != 5 {
		t.Fatalf("matchIndex regressed to %d", got)
	}
	if got := n.CommitIndex(); got != 5 {
		t.Fatalf("commitIndex regressed to %d", got)
	}
}

// Test 7 — N=5 commits with leader + any two followers.
func TestCommit_FiveNodeQuorumCombinations(t *testing.T) {
	peers := []cluster.NodeID{2, 3, 4, 5}
	for _, combo := range [][]cluster.NodeID{{2, 3}, {2, 5}, {4, 5}, {3, 4}} {
		t.Run(fmt.Sprintf("quorum-%v", combo), func(t *testing.T) {
			n, _ := newReplNode(t, 1, peers)
			term := mustCommitLead(t, n)
			e, err := n.Propose([]byte("a"))
			if err != nil {
				t.Fatalf("Propose failed: %v", err)
			}
			if got := n.CommitIndex(); got != 0 {
				t.Fatalf("premature commit: %d", got)
			}
			for _, p := range combo {
				if err := n.HandleAppendEntriesResponse(p, commitResp(uint64(term), true, uint64(e.Index))); err != nil {
					t.Fatalf("response from %d failed: %v", p, err)
				}
			}
			if got := n.CommitIndex(); got != e.Index {
				t.Fatalf("commitIndex = %d, want %d", got, e.Index)
			}
		})
	}
}

// Test 8 — N=5 leader + one follower is insufficient.
func TestCommit_FiveNodeNoMajority(t *testing.T) {
	n, _ := newReplNode(t, 1, []cluster.NodeID{2, 3, 4, 5})
	term := mustCommitLead(t, n)
	e, err := n.Propose([]byte("a"))
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}
	if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(term), true, uint64(e.Index))); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if got := n.CommitIndex(); got != 0 {
		t.Fatalf("commitIndex = %d, want 0 (2/5 < quorum 3)", got)
	}
}

// Test 9 — old-term entries replicated to full quorum still cannot commit.
func TestCommit_OldTermQuorumCannotCommit(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2, 3})
	seedReplLog(t, s, []replSpec{{1, "a"}, {2, "b"}})
	term := mustCommitLead(t, n) // term 3; log holds terms 1,2 only
	if term != 3 {
		t.Fatalf("expected term 3, got %d", term)
	}
	for _, p := range []cluster.NodeID{2, 3} {
		if err := n.HandleAppendEntriesResponse(p, commitResp(3, true, 2)); err != nil {
			t.Fatalf("response from %d failed: %v", p, err)
		}
	}
	if got := n.CommitIndex(); got != 0 {
		t.Fatalf("old-term entries must not commit directly: %d", got)
	}
}

// Test 10 — old-term prefix commits implicitly via a newer current-term entry.
func TestCommit_OldPrefixCommitsThroughNewEntry(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2, 3})
	seedReplLog(t, s, []replSpec{{1, "a"}})
	term := mustCommitLead(t, n) // term 2; log: [1:t1, 2:noop-t2]
	e, err := n.Propose([]byte("b"))
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}
	if e.Index != 3 {
		t.Fatalf("expected index 3 (old + no-op + proposal), got %d", e.Index)
	}
	if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(term), true, 3)); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if got := n.CommitIndex(); got != 3 {
		t.Fatalf("commitIndex = %d, want 3 (prefix incl. idx 1,2)", got)
	}
}

// Test 11 — commitIndex never decreases across acks, failures, and stepdown.
func TestCommit_MonotonicAcrossEvents(t *testing.T) {
	n, _ := newReplNode(t, 1, []cluster.NodeID{2, 3})
	mustCommitLead(t, n)
	for i := 0; i < 3; i++ {
		if _, err := n.Propose([]byte(fmt.Sprintf("e%d", i))); err != nil {
			t.Fatalf("Propose failed: %v", err)
		}
	}
	ct, _ := n.Term()
	if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(ct), true, 2)); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if got := n.CommitIndex(); got != 2 {
		t.Fatalf("commitIndex = %d, want 2", got)
	}
	if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(ct), true, 3)); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if got := n.CommitIndex(); got != 3 {
		t.Fatalf("commitIndex = %d, want 3", got)
	}
	// Lower duplicate, stale-term, and failure responses must not regress.
	_ = n.HandleAppendEntriesResponse(2, commitResp(uint64(ct), true, 1))
	_ = n.HandleAppendEntriesResponse(2, commitResp(uint64(ct)-1, true, 3))
	_ = n.HandleAppendEntriesResponse(2, commitResp(uint64(ct), false, 0))
	if got := n.CommitIndex(); got != 3 {
		t.Fatalf("commitIndex regressed to %d", got)
	}
	// Stepdown preserves the committed prefix.
	if err := n.BecomeFollower(ct+1, cluster.NodeIDNil); err != nil {
		t.Fatalf("BecomeFollower failed: %v", err)
	}
	if got := n.CommitIndex(); got != 3 {
		t.Fatalf("stepdown must preserve commitIndex 3, got %d", got)
	}
}

// Test 12 — higher-term response steps the leader down without committing.
func TestCommit_HigherTermResponseStepdown(t *testing.T) {
	n, _ := newReplNode(t, 1, []cluster.NodeID{2, 3})
	term := mustCommitLead(t, n)
	if _, err := n.Propose([]byte("a")); err != nil {
		t.Fatalf("Propose failed: %v", err)
	}
	if !n.HeartbeatRunning() {
		t.Fatalf("expected scheduler running")
	}
	higher := uint64(term) + 4
	if err := n.HandleAppendEntriesResponse(2, commitResp(higher, true, 1)); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if n.Role() != raft.RoleFollower {
		t.Fatalf("expected Follower, got %s", n.Role())
	}
	if n.HeartbeatRunning() {
		t.Fatalf("scheduler must stop on stepdown")
	}
	if got := n.CommitIndex(); got != 0 {
		t.Fatalf("higher-term response must not commit: %d", got)
	}
	// The old term's traffic is now irrelevant.
	_ = n.HandleAppendEntriesResponse(3, commitResp(uint64(term), true, 1))
	if got := n.CommitIndex(); got != 0 {
		t.Fatalf("post-stepdown old response committed: %d", got)
	}
}

// Test 13 — unknown senders cannot affect replication or commit state.
func TestCommit_UnknownSenderCannotCommit(t *testing.T) {
	n, _ := newReplNode(t, 1, []cluster.NodeID{2, 3})
	term := mustCommitLead(t, n)
	if _, err := n.Propose([]byte("a")); err != nil {
		t.Fatalf("Propose failed: %v", err)
	}
	err := n.HandleAppendEntriesResponse(99, commitResp(uint64(term), true, 1))
	if err == nil {
		t.Fatalf("expected UnknownPeerError, got nil")
	}
	var unknown *errors.UnknownPeerError
	if !stdErrors.As(err, &unknown) {
		t.Fatalf("expected UnknownPeerError, got %T: %v", err, err)
	}
	if got := n.CommitIndex(); got != 0 {
		t.Fatalf("unknown sender advanced commit: %d", got)
	}
	// Unknown higher terms must not inject either.
	err = n.HandleAppendEntriesResponse(99, commitResp(uint64(term)+9, true, 1))
	if err == nil {
		t.Fatalf("expected UnknownPeerError for higher term, got nil")
	}
	curTerm, _ := n.Term()
	if curTerm != term {
		t.Fatalf("unknown sender injected term: %d", curTerm)
	}
}

// Test 14 — responses from an old leader session are isolated by term.
func TestCommit_OldSessionResponseIsolated(t *testing.T) {
	n, _ := newReplNode(t, 1, []cluster.NodeID{2, 3})
	mustCommitLead(t, n)
	// Session A (term 1): replicate idx 2 (proposal; idx 1 is the no-op).
	e, err := n.Propose([]byte("a"))
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}
	t1, _ := n.Term()
	if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(t1), true, uint64(e.Index))); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if got := n.CommitIndex(); got != e.Index {
		t.Fatalf("session A commit = %d, want %d", got, e.Index)
	}
	// Step down and start session B two terms later with fresh progress.
	if err := n.BecomeFollower(t1+1, cluster.NodeIDNil); err != nil {
		t.Fatalf("BecomeFollower failed: %v", err)
	}
	tb, _ := n.Term()
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}
	t2, _ := n.Term()
	if t2 <= tb {
		t.Fatalf("expected new session term, got %d", t2)
	}
	if got := len(n.MatchIndex()); got != 2 {
		t.Fatalf("new session must reset match state, got %v", n.MatchIndex())
	}
	for _, p := range []cluster.NodeID{2, 3} {
		if got := n.MatchIndex()[p]; got != 0 {
			t.Fatalf("stale match leaked into new session: peer %d = %d", p, got)
		}
	}
	// Delayed session-A response (old term, high match) must be ignored.
	if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(t1), true, 10)); err != nil {
		t.Fatalf("old response failed: %v", err)
	}
	if got := n.MatchIndex()[2]; got != 0 {
		t.Fatalf("old session corrupted match: %d", got)
	}
	// Commit from session A survives (monotonic), but nothing new commits.
	if got := n.CommitIndex(); got != e.Index {
		t.Fatalf("commitIndex = %d, want preserved %d", got, e.Index)
	}
}

// Test 15 — mismatch failures adjust nextIndex only, never match/commit.
func TestCommit_MismatchAdjustsNextIndexOnly(t *testing.T) {
	n, _ := newReplNode(t, 1, []cluster.NodeID{2, 3})
	mustCommitLead(t, n)
	for i := 0; i < 4; i++ {
		if _, err := n.Propose([]byte(fmt.Sprintf("e%d", i))); err != nil {
			t.Fatalf("Propose failed: %v", err)
		}
	}
	termNow, _ := n.Term()
	// Establish progress first: success advances match to 4 and next to 5.
	if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(termNow), true, 4)); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if got := n.NextIndex()[2]; got != 5 {
		t.Fatalf("nextIndex[2] = %d, want 5", got)
	}
	if got := n.CommitIndex(); got != 4 {
		t.Fatalf("commitIndex = %d, want 4", got)
	}
	// A subsequent mismatch steps nextIndex back exactly once.
	if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(termNow), false, 0)); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if got := n.NextIndex()[2]; got != 4 {
		t.Fatalf("nextIndex[2] = %d, want 4", got)
	}
	if got := n.MatchIndex()[2]; got != 4 {
		t.Fatalf("failure must not move match: %d", got)
	}
	if got := n.CommitIndex(); got != 4 {
		t.Fatalf("failure must not move commit: %d", got)
	}
	// Repeated failures converge to the floor without touching match/commit.
	for i := 0; i < 10; i++ {
		_ = n.HandleAppendEntriesResponse(2, commitResp(uint64(termNow), false, 0))
	}
	if got := n.NextIndex()[2]; got != 1 {
		t.Fatalf("nextIndex[2] = %d, want converged floor 1", got)
	}
	if got := n.MatchIndex()[2]; got != 4 {
		t.Fatalf("failures must not move match: %d", got)
	}
	if got := n.CommitIndex(); got != 4 {
		t.Fatalf("failures must not move commit: %d", got)
	}
}

// Test 16 — N=5 exact progress without instant quorum.
func TestCommit_ExactProgressThenQuorum(t *testing.T) {
	n, _ := newReplNode(t, 1, []cluster.NodeID{2, 3, 4, 5})
	term := mustCommitLead(t, n)
	for i := 0; i < 3; i++ {
		if _, err := n.Propose([]byte(fmt.Sprintf("e%d", i))); err != nil {
			t.Fatalf("Propose failed: %v", err)
		}
	}
	if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(term), true, 3)); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if got := n.MatchIndex()[2]; got != 3 {
		t.Fatalf("matchIndex[2] = %d, want 3", got)
	}
	if got := n.CommitIndex(); got != 0 {
		t.Fatalf("2/5 must not commit: %d", got)
	}
	if err := n.HandleAppendEntriesResponse(4, commitResp(uint64(term), true, 3)); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if got := n.CommitIndex(); got != 3 {
		t.Fatalf("commitIndex = %d, want 3", got)
	}
}

// Test 17 — end-to-end via the periodic replication sender.
func TestCommit_EndToEndViaReplicationSender(t *testing.T) {
	sender := newAuditSender()
	dir := t.TempDir()
	ls, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	leader, err := raft.NewNode(raft.NodeConfig{
		LocalID:    1,
		Storage:    ls,
		Peers:      []cluster.NodeID{2},
		PeerSender: sender,
	})
	if err != nil {
		_ = ls.Close()
		t.Fatalf("NewNode failed: %v", err)
	}
	defer func() {
		_ = leader.Close()
		_ = ls.Close()
	}()
	mustCommitLead(t, leader)

	fdir := t.TempDir()
	fs, err := raft.OpenStorage(fdir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	follower, err := raft.NewNode(raft.NodeConfig{
		LocalID: 2,
		Storage: fs,
		Peers:   []cluster.NodeID{1},
	})
	if err != nil {
		_ = fs.Close()
		t.Fatalf("NewNode failed: %v", err)
	}
	defer func() {
		_ = follower.Close()
		_ = fs.Close()
	}()

	prop, err := leader.Propose([]byte("e2e"))
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}
	if prop.Index != 2 {
		t.Fatalf("expected proposal at index 2 (after no-op), got %d", prop.Index)
	}

	// Round 1: nextIndex was initialized past the no-op (2), so the sender
	// emits the proposal alone with PrevLog (1,term). The empty follower
	// rejects it — feed that rejection back to drive the leader's backoff.
	deadline := time.Now().Add(5 * time.Second)
	var first *transport.AppendEntriesRequest
	for time.Now().Before(deadline) {
		for _, f := range sender.GetSent(2) {
			req, derr := transport.DecodeAppendEntries(f)
			if derr != nil {
				continue
			}
			if len(req.Entries) == 1 && string(req.Entries[0].Data) == "e2e" {
				first = req
			}
		}
		if first != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if first == nil {
		t.Fatalf("timed out waiting for replication frame with the proposal")
	}
	if first.PrevLogIndex != 1 || first.PrevLogTerm != uint64(prop.Term) {
		t.Fatalf("unexpected PrevLog: %d/%d", first.PrevLogIndex, first.PrevLogTerm)
	}
	fresp, err := follower.HandleAppendEntries(1, &transport.AppendEntriesRequest{
		Term:         first.Term,
		LeaderID:     first.LeaderID,
		PrevLogIndex: first.PrevLogIndex,
		PrevLogTerm:  first.PrevLogTerm,
		LeaderCommit: first.LeaderCommit,
		Nonce:        999001,
		Entries:      first.Entries,
	})
	if err != nil {
		t.Fatalf("follower HandleAppendEntries failed: %v", err)
	}
	if fresp.Success {
		t.Fatalf("empty follower must reject PrevLog beyond its log")
	}
	if err := leader.HandleAppendEntriesResponse(2, fresp); err != nil {
		t.Fatalf("leader response handling failed: %v", err)
	}
	if got := leader.NextIndex()[2]; got != 1 {
		t.Fatalf("nextIndex[2] = %d, want backed-off 1", got)
	}

	// Round 2: the sender now transmits no-op + proposal; the follower
	// replicates both, responds success, and the leader commits.
	var wire *transport.AppendEntriesRequest
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range sender.GetSent(2) {
			req, derr := transport.DecodeAppendEntries(f)
			if derr != nil {
				continue
			}
			if len(req.Entries) == 2 {
				wire = req
			}
		}
		if wire != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if wire == nil {
		t.Fatalf("timed out waiting for backoff replication frame")
	}
	if wire.Entries[0].Type != transport.PeerEntryNoop || string(wire.Entries[1].Data) != "e2e" {
		t.Fatalf("unexpected batch contents: %+v", wire.Entries)
	}
	if wire.PrevLogIndex != 0 || wire.PrevLogTerm != 0 {
		t.Fatalf("unexpected PrevLog: %d/%d", wire.PrevLogIndex, wire.PrevLogTerm)
	}

	// Deliver to the follower through the real wire handler path.
	frame, err := transport.EncodeAppendEntries(&transport.AppendEntriesRequest{
		Term:         wire.Term,
		LeaderID:     wire.LeaderID,
		PrevLogIndex: wire.PrevLogIndex,
		PrevLogTerm:  wire.PrevLogTerm,
		LeaderCommit: wire.LeaderCommit,
		Nonce:        999002,
		Entries:      wire.Entries,
	}, 777)
	if err != nil {
		t.Fatalf("EncodeAppendEntries failed: %v", err)
	}
	if err := follower.HandlePeerFrame(1, frame); err != nil {
		t.Fatalf("follower HandlePeerFrame failed: %v", err)
	}
	stored, err := fs.Entry(1)
	if err != nil {
		t.Fatalf("follower missing no-op: %v", err)
	}
	if stored.Type != transport.PeerEntryNoop {
		t.Fatalf("follower entry 1 type = %s, want no-op", stored.Type)
	}
	stored, err = fs.Entry(2)
	if err != nil {
		t.Fatalf("follower missing entry: %v", err)
	}
	if string(stored.Data) != "e2e" {
		t.Fatalf("follower payload mismatch: %q", stored.Data)
	}

	// Feed the follower's response back to the leader: must commit.
	resp, err := follower.HandleAppendEntries(1, &transport.AppendEntriesRequest{
		Term:         wire.Term,
		LeaderID:     wire.LeaderID,
		PrevLogIndex: wire.PrevLogIndex,
		PrevLogTerm:  wire.PrevLogTerm,
		LeaderCommit: wire.LeaderCommit,
		Nonce:        999003,
		Entries:      wire.Entries,
	})
	if err != nil {
		t.Fatalf("follower HandleAppendEntries failed: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected follower success")
	}
	if err := leader.HandleAppendEntriesResponse(2, resp); err != nil {
		t.Fatalf("leader response handling failed: %v", err)
	}
	if got := leader.CommitIndex(); got != prop.Index {
		t.Fatalf("commitIndex = %d, want %d", got, prop.Index)
	}
}

// Test 18 — progressive replication of A,B,C tracks the commit prefix.
func TestCommit_ProgressivePrefixTracking(t *testing.T) {
	n, _ := newReplNode(t, 1, []cluster.NodeID{2, 3})
	term := mustCommitLead(t, n)
	var idx [3]raft.LogIndex
	for i, d := range []string{"A", "B", "C"} {
		e, err := n.Propose([]byte(d))
		if err != nil {
			t.Fatalf("Propose %s failed: %v", d, err)
		}
		idx[i] = e.Index
	}
	for k, want := range []raft.LogIndex{idx[0], idx[1], idx[2]} {
		if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(term), true, uint64(idx[k]))); err != nil {
			t.Fatalf("response %d failed: %v", k, err)
		}
		if got := n.CommitIndex(); got != want {
			t.Fatalf("after ack %d: commitIndex = %d, want %d", k, got, want)
		}
	}
}

// Test 19 — commitment reflects quorum coverage, not leader log length.
func TestCommit_PartialSuffixCoverage(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2, 3})
	_ = s.SetTerm(4)
	term := mustCommitLead(t, n) // term 5
	if term != 5 {
		t.Fatalf("expected term 5, got %d", term)
	}
	for i := 0; i < 7; i++ {
		if _, err := n.Propose([]byte(fmt.Sprintf("e%d", i))); err != nil {
			t.Fatalf("Propose failed: %v", err)
		}
	}
	// idx5 → A only; idx6 → A+B; idx7 → leader only.
	if err := n.HandleAppendEntriesResponse(2, commitResp(5, true, 5)); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if err := n.HandleAppendEntriesResponse(2, commitResp(5, true, 6)); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if err := n.HandleAppendEntriesResponse(3, commitResp(5, true, 6)); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if got := n.CommitIndex(); got != 6 {
		t.Fatalf("commitIndex = %d, want 6 (quorum coverage, not log length 7)", got)
	}
}

// Test 20 — concurrent proposals and responses: contiguous log, monotonic
// commit within LastIndex, no races or deadlocks.
func TestCommit_ConcurrentProposeAndResponses(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2, 3})
	term := mustCommitLead(t, n)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, 64)

	for p := 0; p < 3; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if _, err := n.Propose([]byte(fmt.Sprintf("p%d-%d", p, i))); err != nil {
					return // stepped down or closed: legal
				}
			}
		}(p)
	}

	for _, peer := range []cluster.NodeID{2, 3} {
		wg.Add(1)
		go func(peer cluster.NodeID) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				lastIdx, _, err := s.LastIndexAndTerm()
				if err != nil {
					return
				}
				_ = n.HandleAppendEntriesResponse(peer, commitResp(uint64(term), true, uint64(lastIdx)))
			}
		}(peer)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		var prev raft.LogIndex
		for i := 0; i < 200; i++ {
			got := n.CommitIndex()
			if got < prev {
				errCh <- fmt.Errorf("commitIndex regressed %d -> %d", prev, got)
				return
			}
			prev = got
			if lastIdx, _, err := s.LastIndexAndTerm(); err == nil && got > lastIdx {
				errCh <- fmt.Errorf("commitIndex %d exceeds LastIndex %d", got, lastIdx)
				return
			}
		}
	}()

	wg.Wait()
	close(stop)
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent commit violation: %v", err)
	}

	// Final contiguity check.
	lastIdx, _, err := s.LastIndexAndTerm()
	if err != nil {
		t.Fatalf("LastIndex failed: %v", err)
	}
	for idx := raft.LogIndex(1); idx <= lastIdx; idx++ {
		if _, err := s.Entry(idx); err != nil {
			t.Fatalf("gap at %d: %v", idx, err)
		}
	}
}

// Inflated MatchIndex values (beyond the leader's own LastIndex) must not
// manufacture quorum coverage: progress from such a response is ignored.
func TestCommit_InflatedMatchIndexIgnored(t *testing.T) {
	n, _ := newReplNode(t, 1, []cluster.NodeID{2, 3})
	term := mustCommitLead(t, n)
	e, err := n.Propose([]byte("a"))
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}
	// Peer claims far more than the leader holds (leader lastIdx = e.Index).
	if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(term), true, uint64(e.Index)+100)); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if got := n.MatchIndex()[2]; got != 0 {
		t.Fatalf("inflated match recorded: %d", got)
	}
	if got := n.CommitIndex(); got != 0 {
		t.Fatalf("inflated match committed: %d", got)
	}
	// MaxUint64 is likewise implausible and must be ignored safely.
	if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(term), true, ^uint64(0))); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if got := n.MatchIndex()[2]; got != 0 {
		t.Fatalf("MaxUint64 match recorded: %d", got)
	}
	if got := n.NextIndex()[2]; got == 0 {
		t.Fatalf("nextIndex corrupted by inflated match")
	}
	// Honest progress still works afterwards.
	if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(term), true, uint64(e.Index))); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if got := n.CommitIndex(); got != e.Index {
		t.Fatalf("commitIndex = %d, want %d", got, e.Index)
	}
}

// Even cluster sizes use the same floor(N/2)+1 quorum rule.
func TestCommit_EvenClusterSizes(t *testing.T) {
	t.Run("N=2", func(t *testing.T) {
		n, _ := newReplNode(t, 1, []cluster.NodeID{2})
		if q := n.QuorumSize(); q != 2 {
			t.Fatalf("N=2 quorum = %d, want 2", q)
		}
		term := mustCommitLead(t, n)
		e, err := n.Propose([]byte("a"))
		if err != nil {
			t.Fatalf("Propose failed: %v", err)
		}
		if got := n.CommitIndex(); got != 0 {
			t.Fatalf("leader alone must not commit in N=2: %d", got)
		}
		if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(term), true, uint64(e.Index))); err != nil {
			t.Fatalf("response failed: %v", err)
		}
		if got := n.CommitIndex(); got != e.Index {
			t.Fatalf("commitIndex = %d, want %d", got, e.Index)
		}
	})
	t.Run("N=4", func(t *testing.T) {
		peers := []cluster.NodeID{2, 3, 4}
		n, _ := newReplNode(t, 1, peers)
		if q := n.QuorumSize(); q != 3 {
			t.Fatalf("N=4 quorum = %d, want 3", q)
		}
		term := mustCommitLead(t, n)
		e, err := n.Propose([]byte("a"))
		if err != nil {
			t.Fatalf("Propose failed: %v", err)
		}
		if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(term), true, uint64(e.Index))); err != nil {
			t.Fatalf("response failed: %v", err)
		}
		if got := n.CommitIndex(); got != 0 {
			t.Fatalf("2/4 must not commit: %d", got)
		}
		if err := n.HandleAppendEntriesResponse(3, commitResp(uint64(term), true, uint64(e.Index))); err != nil {
			t.Fatalf("response failed: %v", err)
		}
		if got := n.CommitIndex(); got != e.Index {
			t.Fatalf("commitIndex = %d, want %d", got, e.Index)
		}
	})
}

// A failure-after-success dips nextIndex, and the next successful round heals
// it — the delayed/failure-then-success correlation the design relies on.
func TestCommit_FailureThenSuccessHealsNextIndex(t *testing.T) {
	n, _ := newReplNode(t, 1, []cluster.NodeID{2})
	term := mustCommitLead(t, n)
	prop, err := n.Propose([]byte("v"))
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}
	if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(term), true, uint64(prop.Index))); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	nextAfterSuccess := n.NextIndex()[2]
	if nextAfterSuccess != prop.Index+1 {
		t.Fatalf("nextIndex = %d, want %d", nextAfterSuccess, prop.Index+1)
	}
	// Delayed failure for an older request: nextIndex dips, match stands.
	if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(term), false, 0)); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if got := n.NextIndex()[2]; got != nextAfterSuccess-1 {
		t.Fatalf("nextIndex = %d, want %d", got, nextAfterSuccess-1)
	}
	if got := n.MatchIndex()[2]; got != prop.Index {
		t.Fatalf("match must stand at %d, got %d", prop.Index, got)
	}
	// Next successful round restores nextIndex; commit never moved wrongly.
	if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(term), true, uint64(prop.Index))); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if got := n.NextIndex()[2]; got != nextAfterSuccess {
		t.Fatalf("nextIndex = %d, want healed %d", got, nextAfterSuccess)
	}
	if got := n.CommitIndex(); got != prop.Index {
		t.Fatalf("commitIndex = %d, want %d", got, prop.Index)
	}
}

// Test 21 — follower restart preserves replicated log; leader commit stands.
func TestCommit_FollowerRestartPreservesLog(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2})
	term := mustCommitLead(t, n)
	e, err := n.Propose([]byte("durable"))
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}

	fdir := t.TempDir()
	fs, err := raft.OpenStorage(fdir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	fn, err := raft.NewNode(raft.NodeConfig{LocalID: 2, Storage: fs, Peers: []cluster.NodeID{1}})
	if err != nil {
		_ = fs.Close()
		t.Fatalf("NewNode failed: %v", err)
	}
	req := replRequest(uint64(term), 1, 0, 0, peerEntries(uint64(term), "durable"))
	resp, err := fn.HandleAppendEntries(1, req)
	if err != nil || !resp.Success {
		t.Fatalf("replication failed: resp=%+v err=%v", resp, err)
	}
	_ = fn.Close()
	_ = fs.Close()

	fs2, err := raft.OpenStorage(fdir)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer func() { _ = fs2.Close() }()
	stored, err := fs2.Entry(1)
	if err != nil {
		t.Fatalf("entry lost across restart: %v", err)
	}
	if string(stored.Data) != "durable" {
		t.Fatalf("payload mismatch after restart: %q", stored.Data)
	}

	// Leader commitment never depended on follower volatile memory.
	if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(term), true, uint64(e.Index))); err != nil {
		t.Fatalf("response failed: %v", err)
	}
	if got := n.CommitIndex(); got != e.Index {
		t.Fatalf("commitIndex = %d, want %d", got, e.Index)
	}
	_ = s
}

// Property test (seeded, deterministic): randomized quorum/match matrices must
// agree with a reference commitment model, with monotonic commit bounded by
// LastIndex and gated on the current term.
func TestCommit_QuorumMatrixProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	refCommit := func(logTerms []uint64, currTerm uint64, matches []uint64, quorum int) uint64 {
		var best uint64
		for n := len(logTerms); n >= 1; n-- {
			if logTerms[n-1] != currTerm {
				continue
			}
			count := 1 // leader holds its whole log
			for _, m := range matches {
				if m >= uint64(n) {
					count++
				}
			}
			if count >= quorum {
				best = uint64(n)
				break
			}
		}
		return best
	}

	// Kept deliberately quick: the scenario matrix is deterministically seeded,
	// so repetition adds no coverage; the full suite must fit `count=20`
	// inside the default 10m go test timeout.
	for iter := 0; iter < 20; iter++ {
		for size := 1; size <= 7; size++ {
			peers := make([]cluster.NodeID, 0, size-1)
			for i := 2; i <= size; i++ {
				peers = append(peers, cluster.NodeID(i))
			}
			n, s := newReplNode(t, 1, peers)

			// Random log: 0..6 entries, non-decreasing terms in [1,3].
			nEntries := rng.Intn(7)
			var specs []replSpec
			var tval uint64 = 1
			for i := 0; i < nEntries; i++ {
				if rng.Intn(2) == 0 && tval < 3 {
					tval++
				}
				specs = append(specs, replSpec{tval, fmt.Sprintf("e%d", i)})
			}
			seedReplLog(t, s, specs)
			// Elect a leader in term 4 (above any seeded term), then append a
			// random number of current-term proposals so the commit path —
			// not just the no-commit path — is exercised.
			cur, _ := s.Term()
			for cur < 4 {
				if err := n.BecomeCandidate(); err != nil {
					t.Fatalf("iter %d: BecomeCandidate failed: %v", iter, err)
				}
				if err := n.BecomeLeader(); err != nil {
					t.Fatalf("iter %d: BecomeLeader failed: %v", iter, err)
				}
				if nt, _ := n.Term(); nt >= 4 {
					break
				}
				if err := n.BecomeFollower(cur+1, cluster.NodeIDNil); err != nil {
					t.Fatalf("iter %d: BecomeFollower failed: %v", iter, err)
				}
				cur, _ = s.Term()
			}
			leaderTerm, _ := n.Term()
			for k := 0; k < rng.Intn(4); k++ {
				if _, err := n.Propose([]byte(fmt.Sprintf("iter%d-new%d", iter, k))); err != nil {
					t.Fatalf("iter %d: Propose failed: %v", iter, err)
				}
			}
			var logTerms []uint64
			lastIdx, _, _ := s.LastIndexAndTerm()
			for idx := raft.LogIndex(1); idx <= lastIdx; idx++ {
				e, _ := s.Entry(idx)
				logTerms = append(logTerms, uint64(e.Term))
			}
			quorum := size/2 + 1

			// Random match vectors per peer, applied in random order.
			type ack struct {
				peer  cluster.NodeID
				match uint64
			}
			var acks []ack
			matches := make([]uint64, len(peers))
			for i, p := range peers {
				_ = p
				m := uint64(0)
				if lastIdx > 0 {
					// Deterministic pseudo-random match in 0..lastIdx.
					m = rng.Uint64() % (uint64(lastIdx) + 1)
				}
				matches[i] = m
				acks = append(acks, ack{peers[i], m})
			}
			rng.Shuffle(len(acks), func(a, b int) { acks[a], acks[b] = acks[b], acks[a] })
			var prevCommit raft.LogIndex
			for _, a := range acks {
				if err := n.HandleAppendEntriesResponse(a.peer, commitResp(uint64(leaderTerm), true, a.match)); err != nil {
					t.Fatalf("iter %d size %d: response failed: %v", iter, size, err)
				}
				got := n.CommitIndex()
				if got < prevCommit {
					t.Fatalf("iter %d size %d: commit regressed %d -> %d", iter, size, prevCommit, got)
				}
				prevCommit = got
				if got > lastIdx {
					t.Fatalf("iter %d size %d: commit %d exceeds LastIndex %d", iter, size, got, lastIdx)
				}
				// nextIndex invariant while leader: floored at 1 and never
				// past one beyond the durable tail.
				for _, p := range peers {
					if nx := n.NextIndex()[p]; nx < 1 || nx > lastIdx+1 {
						t.Fatalf("iter %d size %d: nextIndex[%d] = %d out of [1, %d]",
							iter, size, p, nx, lastIdx+1)
					}
					if m := n.MatchIndex()[p]; m > lastIdx {
						t.Fatalf("iter %d size %d: matchIndex[%d] = %d exceeds LastIndex %d",
							iter, size, p, m, lastIdx)
					}
				}
			}
			want := refCommit(logTerms, uint64(leaderTerm), matches, quorum)
			if got := n.CommitIndex(); got != raft.LogIndex(want) {
				t.Fatalf("iter %d size %d: commit = %d, model = %d (log=%v matches=%v quorum=%d term=%d)",
					iter, size, got, want, logTerms, matches, quorum, leaderTerm)
			}
		}
	}
}
