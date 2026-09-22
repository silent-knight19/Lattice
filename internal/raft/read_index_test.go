package raft_test

import (
	"context"
	stdErrors "errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/raft"
	"github.com/silent-knight19/lattice/internal/transport"
)

type mockReadSender struct {
	mu       sync.Mutex
	seqID    uint64
	onSend   func(ctx context.Context, peerID cluster.NodeID, req *transport.AppendEntriesRequest)
	sentReqs []*transport.AppendEntriesRequest
}

func newMockReadSender(onSend func(ctx context.Context, peerID cluster.NodeID, req *transport.AppendEntriesRequest)) *mockReadSender {
	return &mockReadSender{
		onSend: onSend,
	}
}

func (m *mockReadSender) NextSeqID() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seqID++
	return m.seqID
}

func (m *mockReadSender) Send(ctx context.Context, peerID cluster.NodeID, frame *transport.Frame) error {
	m.mu.Lock()
	req, err := transport.DecodeAppendEntries(frame)
	if err == nil {
		m.sentReqs = append(m.sentReqs, req)
	}
	cb := m.onSend
	m.mu.Unlock()

	if cb != nil && req != nil {
		cb(ctx, peerID, req)
	}
	return nil
}

func (m *mockReadSender) GetSentCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sentReqs)
}

func newTestReadNode(t *testing.T, localID cluster.NodeID, peers []cluster.NodeID, sender raft.PeerSender) (*raft.Node, *raft.Storage) {
	t.Helper()
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	n, err := raft.NewNode(raft.NodeConfig{
		LocalID:    localID,
		Storage:    s,
		Peers:      peers,
		PeerSender: sender,
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

func leadReadNode(t *testing.T, n *raft.Node) raft.Term {
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

// TestReadIndex_LeaderWithMajority verifies that a healthy leader in a 3-node cluster
// receives quorum confirmation (leader + 1 follower = 2 >= 2) and returns the committed position.
func TestReadIndex_LeaderWithMajority(t *testing.T) {
	var n *raft.Node
	sender := newMockReadSender(func(ctx context.Context, peerID cluster.NodeID, req *transport.AppendEntriesRequest) {
		// Follower 2 responds immediately with success and echoes the nonce
		if peerID == 2 {
			go func() {
				_ = n.HandleAppendEntriesResponse(2, &transport.AppendEntriesResponse{
					Term:       req.Term,
					Success:    true,
					MatchIndex: req.PrevLogIndex,
					Nonce:      req.Nonce,
				})
			}()
		}
	})

	var s *raft.Storage
	n, s = newTestReadNode(t, 1, []cluster.NodeID{2, 3}, sender)
	term := leadReadNode(t, n)
	_ = s

	// Ensure leader's election no-op (index 1) is committed before testing ReadIndex
	if err := n.HandleAppendEntriesResponse(2, &transport.AppendEntriesResponse{
		Term:       uint64(term),
		Success:    true,
		MatchIndex: 1,
	}); err != nil {
		t.Fatalf("HandleAppendEntriesResponse failed: %v", err)
	}

	commitIdx := n.CommitIndex()
	if commitIdx != 1 {
		t.Fatalf("commitIndex = %d, want 1", commitIdx)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := n.ReadIndex(ctx)
	if err != nil {
		t.Fatalf("ReadIndex failed: %v", err)
	}
	if res.Index != commitIdx {
		t.Fatalf("ReadIndex = %d, want %d", res.Index, commitIdx)
	}
	if res.Term != term {
		t.Fatalf("ReadTerm = %d, want %d", res.Term, term)
	}
}

// TestReadIndex_SingleNode verifies that for N=1, the leader alone satisfies quorum
// immediately without waiting for peers or sending network frames.
func TestReadIndex_SingleNode(t *testing.T) {
	sender := newMockReadSender(nil)
	n, _ := newTestReadNode(t, 1, nil, sender)
	term := leadReadNode(t, n)

	commitIdx := n.CommitIndex()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	res, err := n.ReadIndex(ctx)
	if err != nil {
		t.Fatalf("ReadIndex single node failed: %v", err)
	}
	if res.Index != commitIdx {
		t.Fatalf("ReadIndex = %d, want %d", res.Index, commitIdx)
	}
	if res.Term != term {
		t.Fatalf("ReadTerm = %d, want %d", res.Term, term)
	}
	if sender.GetSentCount() != 0 {
		t.Fatalf("expected 0 frames sent for single-node cluster, got %d", sender.GetSentCount())
	}
}

// TestReadIndex_OneFollowerUnavailable verifies that in a 3-node cluster, if 1 of 2 followers
// is down, the single responding follower + leader satisfies quorum (2/3) and succeeds.
func TestReadIndex_OneFollowerUnavailable(t *testing.T) {
	var n *raft.Node
	sender := newMockReadSender(func(ctx context.Context, peerID cluster.NodeID, req *transport.AppendEntriesRequest) {
		if peerID == 2 {
			// Peer 2 responds; peer 3 drops message
			go func() {
				_ = n.HandleAppendEntriesResponse(2, &transport.AppendEntriesResponse{
					Term:       req.Term,
					Success:    true,
					MatchIndex: req.PrevLogIndex,
					Nonce:      req.Nonce,
				})
			}()
		}
	})

	n, _ = newTestReadNode(t, 1, []cluster.NodeID{2, 3}, sender)
	term := leadReadNode(t, n)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := n.ReadIndex(ctx)
	if err != nil {
		t.Fatalf("ReadIndex with 1 follower unavailable failed: %v", err)
	}
	if res.Term != term {
		t.Fatalf("ReadTerm = %d, want %d", res.Term, term)
	}
}

// TestReadIndex_NoMajority verifies that when all remote followers are unreachable,
// ReadIndex blocks until the context deadline expires and returns a context error.
func TestReadIndex_NoMajority(t *testing.T) {
	// No follower responds
	sender := newMockReadSender(nil)
	n, _ := newTestReadNode(t, 1, []cluster.NodeID{2, 3}, sender)
	leadReadNode(t, n)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := n.ReadIndex(ctx)
	if err == nil {
		t.Fatal("expected ReadIndex to fail with timeout when no majority responds, got nil")
	}
	if !stdErrors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got: %v", err)
	}
	if stdErrors.Is(err, errors.ErrRaftInvalidRoleTransition) {
		t.Fatalf("timeout must not be reported as ErrRaftInvalidRoleTransition: %v", err)
	}
}

// TestReadIndex_MajorityUnavailable verifies that in an N=5 cluster (quorum = 3),
// 1 responding follower + leader (2 < 3) cannot confirm ReadIndex.
func TestReadIndex_MajorityUnavailable(t *testing.T) {
	var n *raft.Node
	sender := newMockReadSender(func(ctx context.Context, peerID cluster.NodeID, req *transport.AppendEntriesRequest) {
		// Only peer 2 responds; peers 3, 4, 5 are unavailable
		if peerID == 2 {
			go func() {
				_ = n.HandleAppendEntriesResponse(2, &transport.AppendEntriesResponse{
					Term:       req.Term,
					Success:    true,
					MatchIndex: req.PrevLogIndex,
					Nonce:      req.Nonce,
				})
			}()
		}
	})

	n, _ = newTestReadNode(t, 1, []cluster.NodeID{2, 3, 4, 5}, sender)
	leadReadNode(t, n)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := n.ReadIndex(ctx)
	if err == nil {
		t.Fatal("expected ReadIndex to fail when quorum (3) is not reached in N=5 cluster")
	}
	if !stdErrors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got: %v", err)
	}
}

// TestReadIndex_StepdownDuringConfirmation tests leadership loss while awaiting quorum.
// The test hook injects a stepdown after probes are sent; responses arrive afterward.
func TestReadIndex_StepdownDuringConfirmation(t *testing.T) {
	var n *raft.Node
	var capturedReq atomic.Pointer[transport.AppendEntriesRequest]

	sender := newMockReadSender(func(ctx context.Context, peerID cluster.NodeID, req *transport.AppendEntriesRequest) {
		capturedReq.Store(req)
	})

	n, _ = newTestReadNode(t, 1, []cluster.NodeID{2, 3}, sender)
	leadReadNode(t, n)

	// Injected test hook: steps down to Follower immediately after probes are transmitted
	n.SetReadIndexTestHook(func() {
		_ = n.StepDownSameTerm(cluster.NodeIDNil)
		// Responses arrive AFTER stepdown has occurred
		req := capturedReq.Load()
		if req != nil {
			_ = n.HandleAppendEntriesResponse(2, &transport.AppendEntriesResponse{
				Term:       req.Term,
				Success:    true,
				MatchIndex: req.PrevLogIndex,
				Nonce:      req.Nonce,
			})
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, err := n.ReadIndex(ctx)
	if err == nil {
		t.Fatal("expected ReadIndex to fail after stepdown during confirmation")
	}
	if !stdErrors.Is(err, errors.ErrRaftInvalidRoleTransition) {
		t.Fatalf("expected ErrRaftInvalidRoleTransition, got: %v", err)
	}
}

// TestReadIndex_HigherTermResponseForcesStepdown tests that if a follower responds with
// a higher term during ReadIndex confirmation, the node steps down and ReadIndex fails.
func TestReadIndex_HigherTermResponseForcesStepdown(t *testing.T) {
	var n *raft.Node
	sender := newMockReadSender(func(ctx context.Context, peerID cluster.NodeID, req *transport.AppendEntriesRequest) {
		go func() {
			// Follower responds with higher term!
			_ = n.HandleAppendEntriesResponse(peerID, &transport.AppendEntriesResponse{
				Term:       req.Term + 1,
				Success:    false,
				MatchIndex: 0,
				Nonce:      req.Nonce,
			})
		}()
	})

	n, _ = newTestReadNode(t, 1, []cluster.NodeID{2, 3}, sender)
	leadReadNode(t, n)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, err := n.ReadIndex(ctx)
	if err == nil {
		t.Fatal("expected ReadIndex to fail when follower responds with higher term")
	}
	if !stdErrors.Is(err, errors.ErrRaftInvalidRoleTransition) {
		t.Fatalf("expected ErrRaftInvalidRoleTransition, got: %v", err)
	}
	if n.Role() != raft.RoleFollower {
		t.Fatalf("node role = %s, want RoleFollower after higher term", n.Role())
	}
}

// TestReadIndex_LeadershipEpochChangeInvalidatesRound tests that changing leadership epoch
// during ReadIndex invalidates any pending round.
func TestReadIndex_LeadershipEpochChangeInvalidatesRound(t *testing.T) {
	var n *raft.Node
	sender := newMockReadSender(nil)
	n, _ = newTestReadNode(t, 1, []cluster.NodeID{2, 3}, sender)
	leadReadNode(t, n)

	n.SetReadIndexTestHook(func() {
		// Advance term via ObserveHigherTerm to invalidate epoch
		_, _ = n.ObserveHigherTerm(10)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, err := n.ReadIndex(ctx)
	if err == nil {
		t.Fatal("expected ReadIndex to fail after epoch invalidation")
	}
	if !stdErrors.Is(err, errors.ErrRaftInvalidRoleTransition) {
		t.Fatalf("expected ErrRaftInvalidRoleTransition, got: %v", err)
	}
}

// TestReadIndex_StaleHeartbeatResponseIgnored tests that responses with old term,
// zero nonce, or incorrect round nonce are completely ignored and do not count toward quorum.
func TestReadIndex_StaleHeartbeatResponseIgnored(t *testing.T) {
	var n *raft.Node
	sender := newMockReadSender(nil)
	n, _ = newTestReadNode(t, 1, []cluster.NodeID{2, 3}, sender)
	term := leadReadNode(t, n)

	n.SetReadIndexTestHook(func() {
		// 1. Response with zero nonce (legacy heartbeat) -> ignored
		_ = n.HandleAppendEntriesResponse(2, &transport.AppendEntriesResponse{
			Term:       uint64(term),
			Success:    true,
			MatchIndex: 1,
			Nonce:      0,
		})
		// 2. Response with wrong nonce -> ignored
		_ = n.HandleAppendEntriesResponse(2, &transport.AppendEntriesResponse{
			Term:       uint64(term),
			Success:    true,
			MatchIndex: 1,
			Nonce:      9999999,
		})
		// 3. Response with lower term -> ignored
		_ = n.HandleAppendEntriesResponse(2, &transport.AppendEntriesResponse{
			Term:       uint64(term) - 1,
			Success:    true,
			MatchIndex: 1,
			Nonce:      123,
		})
	})

	// Wait with small deadline; since stale responses are ignored, quorum is never reached
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := n.ReadIndex(ctx)
	if !stdErrors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded due to ignored stale responses, got: %v", err)
	}
}

// TestReadIndex_PreviousRoundResponseIgnored tests that an old response from round A
// arriving during round B does not count toward round B's quorum.
func TestReadIndex_PreviousRoundResponseIgnored(t *testing.T) {
	var n *raft.Node
	var round1Nonce uint64
	var capturedNonce atomic.Uint64

	sender := newMockReadSender(func(ctx context.Context, peerID cluster.NodeID, req *transport.AppendEntriesRequest) {
		capturedNonce.Store(req.Nonce)
	})

	n, _ = newTestReadNode(t, 1, []cluster.NodeID{2, 3}, sender)
	term := leadReadNode(t, n)

	// Round 1: captures round 1 nonce, but fails by timeout
	ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel1()
	_, _ = n.ReadIndex(ctx1)
	round1Nonce = capturedNonce.Load()
	if round1Nonce == 0 {
		t.Fatal("failed to capture round 1 nonce")
	}

	// Round 2: started. We inject response with round 1's nonce!
	n.SetReadIndexTestHook(func() {
		_ = n.HandleAppendEntriesResponse(2, &transport.AppendEntriesResponse{
			Term:       uint64(term),
			Success:    true,
			MatchIndex: 1,
			Nonce:      round1Nonce, // Old nonce from round 1!
		})
	})

	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()

	_, err := n.ReadIndex(ctx2)
	if !stdErrors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded because old round nonce was ignored, got: %v", err)
	}
}

// TestReadIndex_ContextCancelledBeforeStart verifies early exit on cancelled context.
func TestReadIndex_ContextCancelledBeforeStart(t *testing.T) {
	sender := newMockReadSender(nil)
	n, _ := newTestReadNode(t, 1, []cluster.NodeID{2, 3}, sender)
	leadReadNode(t, n)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Pre-cancel

	_, err := n.ReadIndex(ctx)
	if err == nil {
		t.Fatal("expected ReadIndex to fail on pre-cancelled context")
	}
	if !stdErrors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	if n.ActiveReadRoundsCount() != 0 {
		t.Fatalf("expected 0 active read rounds after pre-cancelled context, got %d", n.ActiveReadRoundsCount())
	}
}

// TestReadIndex_ContextDeadlineDuringWait verifies cancellation during wait and clean round cleanup.
func TestReadIndex_ContextDeadlineDuringWait(t *testing.T) {
	sender := newMockReadSender(nil)
	n, _ := newTestReadNode(t, 1, []cluster.NodeID{2, 3}, sender)
	leadReadNode(t, n)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := n.ReadIndex(ctx)
	if !stdErrors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got: %v", err)
	}
	if n.ActiveReadRoundsCount() != 0 {
		t.Fatalf("expected 0 active read rounds after deadline cleanup, got %d", n.ActiveReadRoundsCount())
	}
}

// TestReadIndex_CloseDuringConfirmation tests that closing the Node aborts in-flight ReadIndex.
func TestReadIndex_CloseDuringConfirmation(t *testing.T) {
	var n *raft.Node
	sender := newMockReadSender(nil)
	n, _ = newTestReadNode(t, 1, []cluster.NodeID{2, 3}, sender)
	leadReadNode(t, n)

	n.SetReadIndexTestHook(func() {
		_ = n.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, err := n.ReadIndex(ctx)
	if err == nil {
		t.Fatal("expected ReadIndex to fail when node closes during confirmation")
	}
	if !stdErrors.Is(err, errors.ErrRaftStateClosed) {
		t.Fatalf("expected ErrRaftStateClosed, got: %v", err)
	}
}

// TestReadIndex_NonLeaderRejected tests that followers and candidates reject ReadIndex fail-closed.
func TestReadIndex_NonLeaderRejected(t *testing.T) {
	sender := newMockReadSender(nil)
	n, _ := newTestReadNode(t, 1, []cluster.NodeID{2, 3}, sender)

	// 1. RoleFollower
	ctx := context.Background()
	_, err := n.ReadIndex(ctx)
	if err == nil || !stdErrors.Is(err, errors.ErrRaftInvalidRoleTransition) {
		t.Fatalf("follower must reject ReadIndex with ErrRaftInvalidRoleTransition, got: %v", err)
	}

	// 2. RoleCandidate
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	_, err = n.ReadIndex(ctx)
	if err == nil || !stdErrors.Is(err, errors.ErrRaftInvalidRoleTransition) {
		t.Fatalf("candidate must reject ReadIndex with ErrRaftInvalidRoleTransition, got: %v", err)
	}
}

// TestReadIndex_Partition_LeaderCannotConfirm simulates a 3-node cluster partition (Section 20).
// Leader (Node 1) is partitioned from Followers (Nodes 2 & 3).
// Proves that Node 1 cannot successfully establish a ReadIndex confirmation without majority.
func TestReadIndex_Partition_LeaderCannotConfirm(t *testing.T) {
	var partitionActive atomic.Bool
	partitionActive.Store(true)

	var n *raft.Node
	sender := newMockReadSender(func(ctx context.Context, peerID cluster.NodeID, req *transport.AppendEntriesRequest) {
		if partitionActive.Load() {
			// Partition active: drop all outbound frames
			return
		}
		// Partition healed: respond
		go func() {
			_ = n.HandleAppendEntriesResponse(peerID, &transport.AppendEntriesResponse{
				Term:       req.Term,
				Success:    true,
				MatchIndex: req.PrevLogIndex,
				Nonce:      req.Nonce,
			})
		}()
	})

	n, _ = newTestReadNode(t, 1, []cluster.NodeID{2, 3}, sender)
	leadReadNode(t, n)

	// Attempt 1: under partition, ReadIndex MUST fail to confirm leadership
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := n.ReadIndex(ctx)
	if err == nil {
		t.Fatal("partitioned leader must NOT be able to establish ReadIndex confirmation")
	}
	if !stdErrors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded for partitioned leader, got: %v", err)
	}

	// Attempt 2: heal partition -> ReadIndex succeeds
	partitionActive.Store(false)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel2()

	res, err := n.ReadIndex(ctx2)
	if err != nil {
		t.Fatalf("healed cluster ReadIndex failed: %v", err)
	}
	if res.Index != n.CommitIndex() {
		t.Fatalf("ReadIndex = %d, want %d", res.Index, n.CommitIndex())
	}
}

// TestReadIndex_ConcurrentReads verifies multiple concurrent ReadIndex calls with race detector.
func TestReadIndex_ConcurrentReads(t *testing.T) {
	var n *raft.Node
	sender := newMockReadSender(func(ctx context.Context, peerID cluster.NodeID, req *transport.AppendEntriesRequest) {
		go func() {
			_ = n.HandleAppendEntriesResponse(peerID, &transport.AppendEntriesResponse{
				Term:       req.Term,
				Success:    true,
				MatchIndex: req.PrevLogIndex,
				Nonce:      req.Nonce,
			})
		}()
	})

	n, _ = newTestReadNode(t, 1, []cluster.NodeID{2, 3}, sender)
	term := leadReadNode(t, n)

	const numReaders = 20
	var wg sync.WaitGroup
	wg.Add(numReaders)

	errCh := make(chan error, numReaders)

	for i := 0; i < numReaders; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			res, err := n.ReadIndex(ctx)
			if err != nil {
				errCh <- err
				return
			}
			if res.Term != term {
				errCh <- stdErrors.New("mismatched term")
				return
			}
		}()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent reader failed: %v", err)
	}
}
