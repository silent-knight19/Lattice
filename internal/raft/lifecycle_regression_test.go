package raft_test

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/raft"
	"github.com/silent-knight19/lattice/internal/transport"
)

// -----------------------------------------------------------------------------
// Section 2 & 26.O: Lifecycle Regression Tests
// -----------------------------------------------------------------------------

func TestNode_ElectionTimerLifecycle_RepeatedCycles(t *testing.T) {
	// Baseline goroutine count after runtime settles
	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	initialGoroutines := runtime.NumGoroutine()

	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	defer func() { _ = s.Close() }()

	n, err := raft.NewNode(raft.NodeConfig{
		LocalID: 1,
		Storage: s,
		Peers:   []cluster.NodeID{2, 3},
		DurationProvider: func() time.Duration {
			return 100 * time.Millisecond
		},
	})
	if err != nil {
		t.Fatalf("NewNode failed: %v", err)
	}
	defer func() { _ = n.Close() }()

	// Perform repeated Start -> Stop -> Start cycles
	// Must not accumulate goroutines
	cycles := 50
	for i := 0; i < cycles; i++ {
		if err := n.StartElectionTimer(); err != nil {
			t.Fatalf("cycle %d: StartElectionTimer failed: %v", i, err)
		}

		// Verify Start is idempotent while running
		if err := n.StartElectionTimer(); err != nil {
			t.Fatalf("cycle %d: idempotent StartElectionTimer failed: %v", i, err)
		}

		// Allow loop to briefly run
		time.Sleep(2 * time.Millisecond)

		n.StopElectionTimer()

		// Verify Stop is idempotent
		n.StopElectionTimer()
	}

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	finalGoroutines := runtime.NumGoroutine()

	// Difference between initial and final should be at most 2 (testing runner overhead)
	// Definitely NOT accumulating 50 goroutines!
	diff := finalGoroutines - initialGoroutines
	if diff > 3 {
		t.Fatalf("goroutine leak detected: initial=%d, final=%d (diff=%d)", initialGoroutines, finalGoroutines, diff)
	}
}

func TestNode_ElectionTimerLifecycle_StopWhileExpiring(t *testing.T) {
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	defer func() { _ = s.Close() }()

	var (
		mu          sync.Mutex
		transitions []string
	)
	hook := func(from, to raft.Role, term raft.Term) {
		mu.Lock()
		defer mu.Unlock()
		transitions = append(transitions, fmt.Sprintf("%s->%s(T%d)", from, to, term))
	}

	n, err := raft.NewNode(raft.NodeConfig{
		LocalID:        1,
		Storage:        s,
		Peers:          []cluster.NodeID{2, 3},
		TransitionHook: hook,
		DurationProvider: func() time.Duration {
			return 15 * time.Millisecond // Short expiration
		},
	})
	if err != nil {
		t.Fatalf("NewNode failed: %v", err)
	}
	defer func() { _ = n.Close() }()

	// Start timer and stop it right around expiration time
	if err := n.StartElectionTimer(); err != nil {
		t.Fatalf("StartElectionTimer failed: %v", err)
	}

	time.Sleep(14 * time.Millisecond)
	n.StopElectionTimer()

	// Once stopped, wait 50ms to ensure no election actions execute post-stop
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	countAfterStop := len(transitions)
	mu.Unlock()

	// Wait another 50ms: transitions count must not increase
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	countLater := len(transitions)
	mu.Unlock()

	if countLater != countAfterStop {
		t.Fatalf("election action executed after Stop completed: countAfterStop=%d, countLater=%d", countAfterStop, countLater)
	}
}

func TestNode_ElectionTimerLifecycle_CloseAfterStop(t *testing.T) {
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	defer func() { _ = s.Close() }()

	n, err := raft.NewNode(raft.NodeConfig{
		LocalID: 1,
		Storage: s,
		Peers:   []cluster.NodeID{2, 3},
	})
	if err != nil {
		t.Fatalf("NewNode failed: %v", err)
	}

	if err := n.StartElectionTimer(); err != nil {
		t.Fatalf("StartElectionTimer failed: %v", err)
	}
	n.StopElectionTimer()

	// Close after Stop must succeed cleanly without hanging
	if err := n.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Repeated Close is idempotent
	if err := n.Close(); err != nil {
		t.Fatalf("repeated Close failed: %v", err)
	}

	// Start after Close must return ErrRaftStateClosed
	if err := n.StartElectionTimer(); err == nil {
		t.Fatalf("expected error starting timer on closed node, got nil")
	}
}

// -----------------------------------------------------------------------------
// Section 26.P: Close Races
// -----------------------------------------------------------------------------

func TestNode_CloseRaces_ConcurrentStartStopAndVote(t *testing.T) {
	for iter := 0; iter < 10; iter++ {
		dir := t.TempDir()
		s, err := raft.OpenStorage(dir)
		if err != nil {
			t.Fatalf("OpenStorage failed: %v", err)
		}

		n, err := raft.NewNode(raft.NodeConfig{
			LocalID: 1,
			Storage: s,
			Peers:   []cluster.NodeID{2, 3},
			DurationProvider: func() time.Duration {
				return 10 * time.Millisecond
			},
		})
		if err != nil {
			_ = s.Close()
			t.Fatalf("NewNode failed: %v", err)
		}

		var wg sync.WaitGroup

		// Goroutine 1: Rapid Start / Stop
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				_ = n.StartElectionTimer()
				time.Sleep(500 * time.Microsecond)
				n.StopElectionTimer()
			}
		}()

		// Goroutine 2: Incoming vote responses
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				_ = n.HandleRequestVoteResponse(2, &transport.RequestVoteResponse{
					Term:        uint64(i + 1),
					VoteGranted: true,
				})
				time.Sleep(500 * time.Microsecond)
			}
		}()

		// Goroutine 3: Concurrent Close midway
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(5 * time.Millisecond)
			_ = n.Close()
		}()

		wg.Wait()
		_ = n.Close()
		_ = s.Close()
	}
}

func TestNode_Stepdown_RearmsTimerIfActive(t *testing.T) {
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	defer func() { _ = s.Close() }()

	n, err := raft.NewNode(raft.NodeConfig{
		LocalID: 1,
		Storage: s,
		Peers:   []cluster.NodeID{2, 3},
	})
	if err != nil {
		t.Fatalf("NewNode failed: %v", err)
	}
	defer func() { _ = n.Close() }()

	// Case 1: Subsystem is started. Leader steps down -> timer is re-armed
	if err := n.StartElectionTimer(); err != nil {
		t.Fatalf("StartElectionTimer failed: %v", err)
	}

	// Become candidate then leader
	if err := n.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}

	timer := n.ElectionTimer()
	genBefore := timer.CurrentGen()

	// Leader steps down due to higher term
	if _, err := n.ObserveHigherTerm(5); err != nil {
		t.Fatalf("ObserveHigherTerm failed: %v", err)
	}

	if r := n.Role(); r != raft.RoleFollower {
		t.Fatalf("Role = %s, want RoleFollower", r)
	}

	// Generation should have advanced because ResetElectionTimer was invoked on stepdown
	genAfter := timer.CurrentGen()
	if genAfter <= genBefore {
		t.Fatalf("expected timer to advance after stepdown: before=%d, after=%d", genBefore, genAfter)
	}
}
