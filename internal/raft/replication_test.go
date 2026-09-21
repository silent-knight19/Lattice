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

// replSpec describes an expected log entry (term + payload).
type replSpec struct {
	term uint64
	data string
}

func newReplNode(t *testing.T, localID cluster.NodeID, peers []cluster.NodeID) (*raft.Node, *raft.Storage) {
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

// seedReplLog sets the storage term and appends the given entries starting at
// index 1. Terms must be non-decreasing commitment to Storage rules: the term
// is advanced to the maximum entry term first.
func seedReplLog(t *testing.T, s *raft.Storage, specs []replSpec) {
	t.Helper()
	var maxTerm uint64 = 1
	for _, sp := range specs {
		if sp.term > maxTerm {
			maxTerm = sp.term
		}
	}
	if err := s.SetTerm(raft.Term(maxTerm)); err != nil {
		t.Fatalf("SetTerm(%d) failed: %v", maxTerm, err)
	}
	for i, sp := range specs {
		e := raft.LogEntry{
			Index: raft.LogIndex(i + 1),
			Term:  raft.Term(sp.term),
			Type:  transport.PeerEntryNormal,
			Data:  []byte(sp.data),
		}
		if err := s.Append(e); err != nil {
			t.Fatalf("Append idx %d failed: %v", i+1, err)
		}
	}
}

func peerEntries(term uint64, datas ...string) []transport.PeerLogEntry {
	out := make([]transport.PeerLogEntry, len(datas))
	for i, d := range datas {
		out[i] = transport.PeerLogEntry{Term: term, Type: transport.PeerEntryNormal, Data: []byte(d)}
	}
	return out
}

func replRequest(term uint64, leader cluster.NodeID, prevIdx, prevTerm uint64, entries []transport.PeerLogEntry) *transport.AppendEntriesRequest {
	return &transport.AppendEntriesRequest{
		Term:         term,
		LeaderID:     leader,
		PrevLogIndex: prevIdx,
		PrevLogTerm:  prevTerm,
		Nonce:        424242,
		Entries:      entries,
	}
}

func assertReplLog(t *testing.T, s *raft.Storage, want []replSpec) {
	t.Helper()
	lastIdx, _, err := s.LastIndexAndTerm()
	if err != nil {
		t.Fatalf("LastIndexAndTerm failed: %v", err)
	}
	if lastIdx != raft.LogIndex(len(want)) {
		t.Fatalf("log length = %d, want %d", lastIdx, len(want))
	}
	for i, sp := range want {
		got, err := s.Entry(raft.LogIndex(i + 1))
		if err != nil {
			t.Fatalf("Entry(%d) failed: %v", i+1, err)
		}
		if uint64(got.Term) != sp.term || string(got.Data) != sp.data {
			t.Fatalf("index %d = (term %d, %q), want (term %d, %q)",
				i+1, got.Term, got.Data, sp.term, sp.data)
		}
		if got.Type != transport.PeerEntryNormal {
			t.Fatalf("index %d: unexpected type %s", i+1, got.Type)
		}
	}
}

// Test 1 — matching previous entry: append succeeds durably.
func TestReplicate_MatchingPrevAppends(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2})
	seedReplLog(t, s, []replSpec{{1, "A"}, {1, "B"}})
	_ = s.SetTerm(2)

	resp, err := n.HandleAppendEntries(2, replRequest(2, 2, 2, 1, peerEntries(2, "C")))
	if err != nil {
		t.Fatalf("HandleAppendEntries failed: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success")
	}
	if resp.Term != 2 {
		t.Fatalf("expected term 2, got %d", resp.Term)
	}
	if resp.MatchIndex != 3 {
		t.Fatalf("expected MatchIndex 3, got %d", resp.MatchIndex)
	}
	assertReplLog(t, s, []replSpec{{1, "A"}, {1, "B"}, {2, "C"}})
	if n.LeaderID() != 2 {
		t.Fatalf("expected LeaderID 2, got %d", n.LeaderID())
	}
}

// Test 2 — PrevLogIndex beyond follower log: failure without mutation.
func TestReplicate_PrevIndexBeyondLog(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2})
	seedReplLog(t, s, []replSpec{{1, "A"}})
	_ = s.SetTerm(2)

	resp, err := n.HandleAppendEntries(2, replRequest(2, 2, 9, 2, peerEntries(2, "X")))
	if err != nil {
		t.Fatalf("HandleAppendEntries failed: %v", err)
	}
	if resp.Success {
		t.Fatalf("expected failure for PrevLog beyond log")
	}
	assertReplLog(t, s, []replSpec{{1, "A"}})
}

// Test 3 — PrevLog term mismatch: failure, no truncation, no append.
func TestReplicate_PrevTermMismatch(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2})
	seedReplLog(t, s, []replSpec{{1, "A"}, {1, "B"}, {1, "X"}})
	_ = s.SetTerm(2)

	resp, err := n.HandleAppendEntries(2, replRequest(2, 2, 3, 2, peerEntries(2, "C")))
	if err != nil {
		t.Fatalf("HandleAppendEntries failed: %v", err)
	}
	if resp.Success {
		t.Fatalf("expected failure for term mismatch at PrevLog")
	}
	assertReplLog(t, s, []replSpec{{1, "A"}, {1, "B"}, {1, "X"}})
}

// Test 4 — append to empty follower via (0,0) sentinel.
func TestReplicate_EmptyFollowerSentinel(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2})
	_ = s.SetTerm(1)

	resp, err := n.HandleAppendEntries(2, replRequest(1, 2, 0, 0, peerEntries(1, "X", "Y")))
	if err != nil {
		t.Fatalf("HandleAppendEntries failed: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success")
	}
	if resp.MatchIndex != 2 {
		t.Fatalf("expected MatchIndex 2, got %d", resp.MatchIndex)
	}
	assertReplLog(t, s, []replSpec{{1, "X"}, {1, "Y"}})
}

// Test 5 — matching entries are not duplicated or rewritten.
func TestReplicate_MatchingPrefixNoDuplication(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2})
	seedReplLog(t, s, []replSpec{{1, "A"}, {2, "B"}})

	resp, err := n.HandleAppendEntries(2, replRequest(2, 2, 1, 1, peerEntries(2, "B")))
	if err != nil {
		t.Fatalf("HandleAppendEntries failed: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success")
	}
	assertReplLog(t, s, []replSpec{{1, "A"}, {2, "B"}})
}

// Test 6 — conflicting suffix replacement with durability.
func TestReplicate_ConflictingSuffixReplaced(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2})
	seedReplLog(t, s, []replSpec{{1, "A"}, {1, "B"}, {1, "X"}, {1, "Y"}})
	_ = s.SetTerm(2)

	entries := []transport.PeerLogEntry{
		{Term: 2, Type: transport.PeerEntryNormal, Data: []byte("C")},
		{Term: 2, Type: transport.PeerEntryNormal, Data: []byte("D")},
		{Term: 2, Type: transport.PeerEntryNormal, Data: []byte("E")},
	}
	resp, err := n.HandleAppendEntries(2, replRequest(2, 2, 2, 1, entries))
	if err != nil {
		t.Fatalf("HandleAppendEntries failed: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success")
	}
	if resp.MatchIndex != 5 {
		t.Fatalf("expected MatchIndex 5, got %d", resp.MatchIndex)
	}
	assertReplLog(t, s, []replSpec{{1, "A"}, {1, "B"}, {2, "C"}, {2, "D"}, {2, "E"}})
}

// Test 7 — extra follower suffix survives a shorter empty heartbeat.
func TestReplicate_ExtraSuffixSurvivesHeartbeat(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2})
	seedReplLog(t, s, []replSpec{{1, "A"}, {1, "B"}, {1, "C"}, {1, "D"}})

	resp, err := n.HandleAppendEntries(2, replRequest(1, 2, 3, 1, nil))
	if err != nil {
		t.Fatalf("HandleAppendEntries failed: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected heartbeat success")
	}
	assertReplLog(t, s, []replSpec{{1, "A"}, {1, "B"}, {1, "C"}, {1, "D"}})
}

// Test 7b — extra follower suffix survives a shorter non-empty request.
func TestReplicate_ExtraSuffixSurvivesShortReplication(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2})
	seedReplLog(t, s, []replSpec{{1, "A"}, {1, "B"}, {2, "C"}, {2, "D"}, {2, "E"}})

	// Leader suffix ends at 3; follower tail 4,5 must be preserved.
	resp, err := n.HandleAppendEntries(2, replRequest(2, 2, 2, 1, peerEntries(2, "C")))
	if err != nil {
		t.Fatalf("HandleAppendEntries failed: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success")
	}
	assertReplLog(t, s, []replSpec{{1, "A"}, {1, "B"}, {2, "C"}, {2, "D"}, {2, "E"}})
}

// Test 8 — duplicate delivery is idempotent.
func TestReplicate_DuplicateDeliveryIdempotent(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2})
	seedReplLog(t, s, []replSpec{{1, "A"}, {1, "B"}})
	_ = s.SetTerm(2)

	req := replRequest(2, 2, 2, 1, peerEntries(2, "C", "D"))
	for i := 0; i < 3; i++ {
		resp, err := n.HandleAppendEntries(2, req)
		if err != nil {
			t.Fatalf("delivery %d failed: %v", i, err)
		}
		if !resp.Success {
			t.Fatalf("delivery %d: expected success", i)
		}
		if resp.MatchIndex != 4 {
			t.Fatalf("delivery %d: expected MatchIndex 4, got %d", i, resp.MatchIndex)
		}
	}
	assertReplLog(t, s, []replSpec{{1, "A"}, {1, "B"}, {2, "C"}, {2, "D"}})
}

// Test 9 — malformed coordinates and entries stay invalid without mutation.
func TestReplicate_MalformedCoordinatesRejected(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2})
	seedReplLog(t, s, []replSpec{{1, "A"}})

	// (0, term>0) sentinel violation.
	if _, err := n.HandleAppendEntries(2, replRequest(1, 2, 0, 7, nil)); err == nil {
		t.Fatalf("expected error for (PrevLogIndex=0, PrevLogTerm=7)")
	}
	// (index>0, term 0) violation.
	if _, err := n.HandleAppendEntries(2, replRequest(1, 2, 1, 0, nil)); err == nil {
		t.Fatalf("expected error for (PrevLogIndex=1, PrevLogTerm=0)")
	}
	// Zero-term entry (wire carries no validity guarantee on terms).
	zeroTerm := replRequest(1, 2, 0, 0, []transport.PeerLogEntry{
		{Term: 0, Type: transport.PeerEntryNormal, Data: []byte("z")},
	})
	resp, err := n.HandleAppendEntries(2, zeroTerm)
	if err != nil {
		t.Fatalf("zero-term request failed with error: %v", err)
	}
	if resp.Success {
		t.Fatalf("zero-term entry must not succeed")
	}
	// Entry from a term beyond the leader's stated term.
	futureTerm := replRequest(1, 2, 0, 0, []transport.PeerLogEntry{
		{Term: 9, Type: transport.PeerEntryNormal, Data: []byte("f")},
	})
	resp, err = n.HandleAppendEntries(2, futureTerm)
	if err != nil {
		t.Fatalf("future-term request failed with error: %v", err)
	}
	if resp.Success {
		t.Fatalf("future-term entry must not succeed")
	}
	// Invalid entry type (transport decode would also reject; Raft must too).
	badType := replRequest(1, 2, 0, 0, []transport.PeerLogEntry{
		{Term: 1, Type: transport.PeerEntryType(0xFF), Data: []byte("t")},
	})
	resp, err = n.HandleAppendEntries(2, badType)
	if err != nil {
		t.Fatalf("bad-type request failed with error: %v", err)
	}
	if resp.Success {
		t.Fatalf("invalid-type entry must not succeed")
	}
	assertReplLog(t, s, []replSpec{{1, "A"}})
}

// Test 10 — oversize entry data rejected without partial mutation.
func TestReplicate_OversizeEntryRejected(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2})
	seedReplLog(t, s, []replSpec{{1, "A"}, {1, "X"}}) // conflicting tail present
	_ = s.SetTerm(1)

	big := make([]byte, raft.MaxLogEntryDataSize+1)
	req := replRequest(1, 2, 1, 1, []transport.PeerLogEntry{
		{Term: 1, Type: transport.PeerEntryNormal, Data: big},
	})
	resp, err := n.HandleAppendEntries(2, req)
	if err != nil {
		t.Fatalf("oversize request failed with error: %v", err)
	}
	if resp.Success {
		t.Fatalf("oversize entry must not succeed")
	}
	// Validation precedes truncation: conflicting tail must be intact.
	assertReplLog(t, s, []replSpec{{1, "A"}, {1, "X"}})
}

// Test 12 — stale term cannot mutate the log.
func TestReplicate_StaleTermNoMutation(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2})
	seedReplLog(t, s, []replSpec{{5, "A"}})
	_ = s.SetTerm(5)

	resp, err := n.HandleAppendEntries(2, replRequest(4, 2, 0, 0, peerEntries(4, "S")))
	if err != nil {
		t.Fatalf("HandleAppendEntries failed: %v", err)
	}
	if resp.Success {
		t.Fatalf("stale term must not succeed")
	}
	if resp.Term != 5 {
		t.Fatalf("expected term 5, got %d", resp.Term)
	}
	assertReplLog(t, s, []replSpec{{5, "A"}})
	if n.LeaderID() != cluster.NodeIDNil {
		t.Fatalf("stale leader must not be recorded: %d", n.LeaderID())
	}
}

// Test 13 — higher term steps down, replicates under the new term.
func TestReplicate_HigherTermStepdownAndReplicate(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2})
	seedReplLog(t, s, []replSpec{{2, "A"}})

	// Become leader first so scheduler shutdown is exercised.
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}
	if !n.HeartbeatRunning() {
		t.Fatalf("expected scheduler running")
	}

	resp, err := n.HandleAppendEntries(2, replRequest(5, 2, 0, 0, peerEntries(5, "N")))
	if err != nil {
		t.Fatalf("HandleAppendEntries failed: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success under new term")
	}
	if n.Role() != raft.RoleFollower {
		t.Fatalf("expected Follower, got %s", n.Role())
	}
	term, _ := n.Term()
	if term != 5 {
		t.Fatalf("expected term 5, got %d", term)
	}
	if n.HeartbeatRunning() {
		t.Fatalf("scheduler must stop on stepdown")
	}
	// PrevLog=(0,0) anchors the leader suffix at index 1, so the old
	// term-2 entry conflicts and is replaced.
	assertReplLog(t, s, []replSpec{{5, "N"}})
}

// Test 14 — unknown sender cannot replicate or inject terms.
func TestReplicate_UnknownSenderNoReplication(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2})
	seedReplLog(t, s, []replSpec{{1, "A"}})

	req := replRequest(9, 99, 0, 0, peerEntries(9, "evil"))
	_, err := n.HandleAppendEntries(99, req)
	if err == nil {
		t.Fatalf("expected UnknownPeerError, got nil")
	}
	var unknown *errors.UnknownPeerError
	if !stdErrors.As(err, &unknown) {
		t.Fatalf("expected UnknownPeerError, got %T: %v", err, err)
	}
	term, _ := n.Term()
	if term != 1 {
		t.Fatalf("unknown sender injected term: %d", term)
	}
	assertReplLog(t, s, []replSpec{{1, "A"}})
}

// Test 15 — lagging follower with non-empty mismatch: rejection + liveness.
func TestReplicate_LaggingFollowerLivenessWithEntries(t *testing.T) {
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
				req := replRequest(1, 1, 5, 1, peerEntries(1, "late"))
				resp, err := follower.HandleAppendEntries(1, req)
				if err != nil {
					t.Errorf("HandleAppendEntries failed: %v", err)
					return
				}
				if resp.Success {
					t.Errorf("expected mismatch failure for lagging follower")
					return
				}
			}
		}
	}()

	time.Sleep(200 * time.Millisecond)
	close(stopCh)
	<-doneCh

	if follower.Role() != raft.RoleFollower {
		t.Fatalf("lagging follower timed out: role=%s", follower.Role())
	}
	lastIdx, _, err := s.LastIndexAndTerm()
	if err != nil {
		t.Fatalf("LastIndexAndTerm failed: %v", err)
	}
	if lastIdx != 0 {
		t.Fatalf("mismatching replication mutated log: lastIdx=%d", lastIdx)
	}
}

// Test 16 — replicated state (including conflict replacement) survives restart.
func TestReplicate_RestartRecovery(t *testing.T) {
	dir := t.TempDir()

	open := func() (*raft.Node, *raft.Storage) {
		s, err := raft.OpenStorage(dir)
		if err != nil {
			t.Fatalf("OpenStorage failed: %v", err)
		}
		n, err := raft.NewNode(raft.NodeConfig{
			LocalID: 1,
			Storage: s,
			Peers:   []cluster.NodeID{2},
		})
		if err != nil {
			_ = s.Close()
			t.Fatalf("NewNode failed: %v", err)
		}
		return n, s
	}

	n, s := open()
	seedReplLog(t, s, []replSpec{{1, "A"}, {1, "X"}, {1, "Y"}})
	_ = s.SetTerm(2)
	entries := []transport.PeerLogEntry{
		{Term: 2, Type: transport.PeerEntryNormal, Data: []byte("C")},
		{Term: 2, Type: transport.PeerEntryNormal, Data: []byte("D")},
	}
	resp, err := n.HandleAppendEntries(2, replRequest(2, 2, 1, 1, entries))
	if err != nil || !resp.Success {
		t.Fatalf("replication failed: resp=%+v err=%v", resp, err)
	}
	_ = n.Close()
	_ = s.Close()

	n2, s2 := open()
	defer func() {
		_ = n2.Close()
		_ = s2.Close()
	}()
	assertReplLog(t, s2, []replSpec{{1, "A"}, {2, "C"}, {2, "D"}})
	term, _ := s2.Term()
	if term != 2 {
		t.Fatalf("expected term 2 after recovery, got %d", term)
	}
}

// Test 17 — persistence failure paths fail closed without phantom state.
func TestReplicate_PersistenceFailureFailClosed(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2})
	seedReplLog(t, s, []replSpec{{1, "A"}})

	_ = s.Close()
	resp, err := n.HandleAppendEntries(2, replRequest(1, 2, 0, 0, peerEntries(1, "N")))
	if err == nil && (resp != nil && resp.Success) {
		t.Fatalf("closed storage must not report success")
	}
	// Either an error or Success=false is acceptable; success is not.
	if err == nil && resp.Success {
		t.Fatalf("false success on persistence failure")
	}
}

// Test 18 — concurrent replications: no races, log stays valid + contiguous.
func TestReplicate_ConcurrentReplicationsStayValid(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2})
	_ = s.SetTerm(3)

	const writers = 8
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			term := uint64(3)
			// Distinct terms at the same index force serialized conflict
			// replacement; identical terms exercise no-op matching.
			req := replRequest(term, 2, 0, 0, []transport.PeerLogEntry{
				{Term: term, Type: transport.PeerEntryNormal, Data: []byte(fmt.Sprintf("w%d", w))},
				{Term: term, Type: transport.PeerEntryNormal, Data: []byte(fmt.Sprintf("w%d-b", w))},
			})
			_, _ = n.HandleAppendEntries(2, req)
		}(w)
	}
	wg.Wait()

	// Structural validity: contiguous 1..N, every entry valid.
	lastIdx, _, err := s.LastIndexAndTerm()
	if err != nil {
		t.Fatalf("LastIndexAndTerm failed: %v", err)
	}
	if lastIdx == 0 {
		t.Fatalf("expected some replicated entries")
	}
	for idx := raft.LogIndex(1); idx <= lastIdx; idx++ {
		e, err := s.Entry(idx)
		if err != nil {
			t.Fatalf("gap at index %d: %v", idx, err)
		}
		if err := e.Validate(); err != nil {
			t.Fatalf("invalid entry at %d: %v", idx, err)
		}
	}
}

// Leadership race: AppendEntries racing stepdown/Close must not panic,
// deadlock, corrupt storage, or falsely acknowledge.
func TestReplicate_RacingStepdownNoCorruption(t *testing.T) {
	n, s := newReplNode(t, 1, []cluster.NodeID{2})
	seedReplLog(t, s, []replSpec{{1, "A"}, {1, "B"}})
	_ = s.SetTerm(2)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		var term uint64 = 100
		for {
			select {
			case <-stop:
				return
			default:
			}
			term++
			req := replRequest(term, 2, 2, 1, peerEntries(term, "r"))
			resp, err := n.HandleAppendEntries(2, req)
			if err == nil && resp != nil && resp.Success {
				if _, serr := s.Entry(raft.LogIndex(resp.MatchIndex)); serr != nil {
					t.Errorf("success without durable entry at %d: %v", resp.MatchIndex, serr)
					return
				}
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		var term uint64 = 1000
		for {
			select {
			case <-stop:
				return
			default:
			}
			term++
			_, _ = n.ObserveHigherTerm(raft.Term(term))
		}
	}()

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	// Final structural check.
	lastIdx, _, err := s.LastIndexAndTerm()
	if err != nil {
		t.Fatalf("LastIndexAndTerm failed: %v", err)
	}
	for idx := raft.LogIndex(1); idx <= lastIdx; idx++ {
		if _, err := s.Entry(idx); err != nil {
			t.Fatalf("gap at index %d: %v", idx, err)
		}
	}
}

// Propose + replication interop: leader entries flow to followers verbatim.
func TestReplicate_ProposeFlowsToFollower(t *testing.T) {
	ln, ls := newReplNode(t, 1, []cluster.NodeID{2})
	_ = ls.SetTerm(4)
	if err := ln.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := ln.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}
	prop, err := ln.Propose([]byte("client-cmd"))
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}

	fn, fs := newReplNode(t, 2, []cluster.NodeID{1})
	_ = fs.SetTerm(prop.Term - 1)

	prevIdx := uint64(prop.Index) - 1
	var prevTerm uint64
	if prevIdx > 0 {
		prevTerm = uint64(prop.Term)
	}
	req := &transport.AppendEntriesRequest{
		Term:         uint64(prop.Term),
		LeaderID:     1,
		PrevLogIndex: prevIdx,
		PrevLogTerm:  prevTerm,
		Nonce:        777,
		Entries: []transport.PeerLogEntry{
			{Term: uint64(prop.Term), Type: prop.Type, Data: prop.Data},
		},
	}
	// Follower log is empty but proposal is index 1: adjust to a coherent
	// scenario — leader's first entry replicates onto the empty follower.
	if prop.Index != 1 {
		t.Fatalf("expected first proposal at index 1, got %d", prop.Index)
	}
	resp, err := fn.HandleAppendEntries(1, req)
	if err != nil {
		t.Fatalf("HandleAppendEntries failed: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected replication success")
	}
	stored, err := fs.Entry(1)
	if err != nil {
		t.Fatalf("follower missing entry: %v", err)
	}
	if string(stored.Data) != "client-cmd" || stored.Term != prop.Term {
		t.Fatalf("follower entry mismatch: %+v", stored)
	}
}
