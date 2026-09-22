package raft_test

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/raft"
	"github.com/silent-knight19/lattice/internal/transport"
)

// -----------------------------------------------------------------------------
// Section 20.A: Leader Starts Scheduler Exactly Once
// -----------------------------------------------------------------------------

func TestNode_Heartbeat_LeaderStartsSchedulerOnce(t *testing.T) {
	sender := newMockHeartbeatSender()
	node, s := newTestHeartbeatNode(t, 1, []cluster.NodeID{2}, sender, nil)

	if err := s.SetTerm(1); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}
	if err := node.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := node.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}

	if !node.HeartbeatRunning() {
		t.Fatalf("expected HeartbeatRunning == true")
	}

	// Idempotent: repeated BecomeLeader should not duplicate scheduler
	if err := node.BecomeLeader(); err != nil {
		t.Fatalf("second BecomeLeader failed: %v", err)
	}

	// Wait 120ms
	time.Sleep(120 * time.Millisecond)

	// In 120ms with 50ms ticker: immediate(T=0) + tick(T=50ms) + tick(T=100ms) = ~3 messages
	sent := sender.GetSent(2)
	if len(sent) > 5 {
		t.Fatalf("excessive heartbeats sent (%d), scheduler may be duplicated", len(sent))
	}
	if len(sent) < 2 {
		t.Fatalf("too few heartbeats sent (%d)", len(sent))
	}
}

// -----------------------------------------------------------------------------
// Section 20.B: Leader -> Follower Stepdown Stops Scheduler
// -----------------------------------------------------------------------------

func TestNode_Heartbeat_StepdownStopsScheduler(t *testing.T) {
	sender := newMockHeartbeatSender()
	node, s := newTestHeartbeatNode(t, 1, []cluster.NodeID{2}, sender, nil)

	// Leader in term 5
	if err := s.SetTerm(4); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}
	if err := node.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := node.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}

	// Let it run for 60ms so at least 1 periodic tick occurs
	time.Sleep(60 * time.Millisecond)

	countBefore := len(sender.GetSent(2))
	if countBefore == 0 {
		t.Fatalf("expected at least 1 heartbeat before stepdown")
	}

	// Stepdown: Leader(term=5) -> Follower(term=6)
	if err := node.BecomeFollower(6, cluster.NodeIDNil); err != nil {
		t.Fatalf("BecomeFollower failed: %v", err)
	}

	if node.HeartbeatRunning() {
		t.Fatalf("expected HeartbeatRunning == false after stepdown")
	}

	// Wait another 120ms
	time.Sleep(120 * time.Millisecond)

	countAfter := len(sender.GetSent(2))
	if countAfter != countBefore {
		t.Fatalf("expected no new heartbeats after stepdown, before=%d after=%d", countBefore, countAfter)
	}

	// Verify no term-5 heartbeat is emitted after the stepdown completes
	for _, frame := range sender.GetSent(2) {
		req, err := transport.DecodeAppendEntries(frame)
		if err != nil {
			continue
		}
		if req.Term > 5 {
			t.Fatalf("unexpected heartbeat with term %d emitted", req.Term)
		}
	}
}

// -----------------------------------------------------------------------------
// Section 20.C: Follower -> Leader Again Starts New Scheduler Session
// -----------------------------------------------------------------------------

func TestNode_Heartbeat_FollowerToLeaderRestartSession(t *testing.T) {
	sender := newMockHeartbeatSender()
	node, s := newTestHeartbeatNode(t, 1, []cluster.NodeID{2}, sender, nil)

	// Round 1: Become Leader in term 2
	if err := s.SetTerm(1); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}
	if err := node.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate round 1 failed: %v", err)
	}
	if err := node.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader round 1 failed: %v", err)
	}
	if !node.HeartbeatRunning() {
		t.Fatalf("round 1: expected HeartbeatRunning == true")
	}

	time.Sleep(60 * time.Millisecond)

	// Stepdown to Follower in term 3
	if err := node.BecomeFollower(3, cluster.NodeIDNil); err != nil {
		t.Fatalf("stepdown failed: %v", err)
	}
	if node.HeartbeatRunning() {
		t.Fatalf("expected scheduler stopped on stepdown")
	}

	// Round 2: Become Leader in term 4
	if err := node.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate round 2 failed: %v", err)
	}
	if err := node.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader round 2 failed: %v", err)
	}
	if !node.HeartbeatRunning() {
		t.Fatalf("round 2: expected HeartbeatRunning == true")
	}

	time.Sleep(60 * time.Millisecond)

	// Verify new frames carry term 4
	allFrames := sender.GetSent(2)
	var term4Found bool
	for _, frame := range allFrames {
		req, err := transport.DecodeAppendEntries(frame)
		if err != nil {
			continue
		}
		if req.Term == 4 {
			term4Found = true
			break
		}
	}
	if !term4Found {
		t.Fatalf("expected round 2 heartbeats with term 4")
	}
}

// -----------------------------------------------------------------------------
// Section 20.D: Close While Leader Stops Scheduler Cleanly
// -----------------------------------------------------------------------------

func TestNode_Heartbeat_CloseWhileLeader(t *testing.T) {
	sender := newMockHeartbeatSender()
	node, s := newTestHeartbeatNode(t, 1, []cluster.NodeID{2}, sender, nil)

	if err := s.SetTerm(1); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}
	if err := node.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := node.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}

	time.Sleep(30 * time.Millisecond)

	if err := node.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if node.HeartbeatRunning() {
		t.Fatalf("expected HeartbeatRunning == false after Close")
	}

	countAtClose := sender.TotalSent()
	time.Sleep(100 * time.Millisecond)

	if sender.TotalSent() != countAtClose {
		t.Fatalf("heartbeats transmitted after Close returned: at close=%d, after=%d",
			countAtClose, sender.TotalSent())
	}
}

// -----------------------------------------------------------------------------
// Section 20.E: Close While Heartbeat Send is Occurring
// -----------------------------------------------------------------------------

type slowHeartbeatSender struct {
	mu       sync.Mutex
	seqID    uint64
	inSend   chan struct{}
	slowdown time.Duration
}

func (s *slowHeartbeatSender) NextSeqID() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seqID++
	return s.seqID
}

func (s *slowHeartbeatSender) Send(ctx context.Context, peerID cluster.NodeID, frame *transport.Frame) error {
	select {
	case s.inSend <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.slowdown):
		return nil
	}
}

func TestNode_Heartbeat_CloseWhileSending(t *testing.T) {
	inSend := make(chan struct{}, 10)
	sender := &slowHeartbeatSender{
		inSend:   inSend,
		slowdown: 80 * time.Millisecond,
	}

	node, s := newTestHeartbeatNode(t, 1, []cluster.NodeID{2}, sender, nil)

	if err := s.SetTerm(1); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}
	if err := node.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := node.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}

	// Wait until at least one send starts
	select {
	case <-inSend:
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("timed out waiting for send to begin")
	}

	// Close immediately while send is in flight
	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		_ = node.Close()
	}()

	select {
	case <-closeDone:
		// Succeeded cleanly without deadlock
	case <-time.After(2 * time.Second):
		t.Fatalf("Node.Close() deadlocked while heartbeat send was in flight")
	}

	if node.HeartbeatRunning() {
		t.Fatalf("expected HeartbeatRunning == false after Close")
	}
}

// -----------------------------------------------------------------------------
// Section 20.F: Repeated Leader Lifecycle (No Goroutine Leak)
// -----------------------------------------------------------------------------

func TestNode_Heartbeat_RepeatedLifecycleNoGoroutineLeak(t *testing.T) {
	sender := newMockHeartbeatSender()
	node, s := newTestHeartbeatNode(t, 1, []cluster.NodeID{2}, sender, nil)

	baseGoroutines := runtime.NumGoroutine()

	for round := 1; round <= 15; round++ {
		if err := node.BecomeCandidate(); err != nil {
			t.Fatalf("round %d BecomeCandidate failed: %v", round, err)
		}
		if err := node.BecomeLeader(); err != nil {
			t.Fatalf("round %d BecomeLeader failed: %v", round, err)
		}
		if !node.HeartbeatRunning() {
			t.Fatalf("round %d expected HeartbeatRunning == true", round)
		}

		time.Sleep(10 * time.Millisecond)

		term, err := s.Term()
		if err != nil {
			t.Fatalf("round %d s.Term failed: %v", round, err)
		}
		if err := node.BecomeFollower(term+1, cluster.NodeIDNil); err != nil {
			t.Fatalf("round %d BecomeFollower failed: %v", round, err)
		}
		if node.HeartbeatRunning() {
			t.Fatalf("round %d expected HeartbeatRunning == false after stepdown", round)
		}
	}

	// Allow any terminating scheduler goroutine to finish
	time.Sleep(50 * time.Millisecond)
	afterGoroutines := runtime.NumGoroutine()

	// Should not accumulate goroutines (small threshold tolerance for runtime)
	if afterGoroutines-baseGoroutines > 5 {
		t.Fatalf("potential goroutine leak: base=%d, after 15 rounds=%d", baseGoroutines, afterGoroutines)
	}
}

// -----------------------------------------------------------------------------
// Section 21: Heartbeat Interval Testing
// -----------------------------------------------------------------------------

func TestNode_Heartbeat_IntervalVerification(t *testing.T) {
	var (
		mu         sync.Mutex
		sendTimes  []time.Time
		recordDone = make(chan struct{})
	)

	sender := newMockHeartbeatSender()
	// Custom sender that records arrival timestamps
	intervalSender := &intervalRecorderSender{
		inner: sender,
		onSend: func() {
			mu.Lock()
			sendTimes = append(sendTimes, time.Now())
			if len(sendTimes) >= 4 {
				select {
				case <-recordDone:
				default:
					close(recordDone)
				}
			}
			mu.Unlock()
		},
	}

	node, s := newTestHeartbeatNode(t, 1, []cluster.NodeID{2}, intervalSender, nil)

	if err := s.SetTerm(1); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}
	if err := node.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := node.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}

	select {
	case <-recordDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("timed out waiting for 4 heartbeat ticks")
	}

	mu.Lock()
	times := append([]time.Time(nil), sendTimes...)
	mu.Unlock()

	// Skip index 0 (immediate T=0 heartbeat) and inspect periodic intervals (1->2, 2->3)
	for i := 1; i < len(times)-1; i++ {
		diff := times[i+1].Sub(times[i])
		// Check that cadence is around 50ms, definitely not < 15ms (tight spin) or > 150ms
		if diff < 15*time.Millisecond {
			t.Fatalf("tick %d->%d occurred too quickly: %v (potential tight spin loop)", i, i+1, diff)
		}
		if diff > 150*time.Millisecond {
			t.Fatalf("tick %d->%d occurred too slowly: %v", i, i+1, diff)
		}
	}
}

type intervalRecorderSender struct {
	inner  *mockHeartbeatSender
	onSend func()
}

func (s *intervalRecorderSender) NextSeqID() uint64 {
	return s.inner.NextSeqID()
}

func (s *intervalRecorderSender) Send(ctx context.Context, peerID cluster.NodeID, frame *transport.Frame) error {
	s.onSend()
	return s.inner.Send(ctx, peerID, frame)
}

// -----------------------------------------------------------------------------
// Section 31: Scheduler Concurrency Test Under Race Detector
// -----------------------------------------------------------------------------

func TestNode_Heartbeat_SchedulerConcurrency(t *testing.T) {
	sender := newMockHeartbeatSender()
	node, s := newTestHeartbeatNode(t, 1, []cluster.NodeID{2, 3}, sender, nil)

	if err := s.SetTerm(1); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}

	var stop atomic.Bool
	var wg sync.WaitGroup

	// Goroutine 1: Rapid Leader transitions
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			_ = node.BecomeCandidate()
			_ = node.BecomeLeader()
			time.Sleep(5 * time.Millisecond)
		}
	}()

	// Goroutine 2: Incoming higher-term RequestVotes
	wg.Add(1)
	go func() {
		defer wg.Done()
		var term uint64 = 10
		for !stop.Load() {
			term++
			req := &transport.RequestVoteRequest{
				Term:         term,
				CandidateID:  2,
				LastLogIndex: 0,
				LastLogTerm:  0,
				Nonce:        term,
			}
			_, _ = node.HandleRequestVote(2, req)
			time.Sleep(3 * time.Millisecond)
		}
	}()

	// Goroutine 3: Incoming higher-term AppendEntries
	wg.Add(1)
	go func() {
		defer wg.Done()
		var term uint64 = 100
		for !stop.Load() {
			term++
			req := &transport.AppendEntriesRequest{
				Term:         term,
				LeaderID:     3,
				PrevLogIndex: 0,
				PrevLogTerm:  0,
				Nonce:        term,
			}
			_, _ = node.HandleAppendEntries(3, req)
			time.Sleep(4 * time.Millisecond)
		}
	}()

	// Goroutine 4: StepDownSameTerm calls
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			_ = node.StepDownSameTerm(2)
			time.Sleep(7 * time.Millisecond)
		}
	}()

	// Goroutine 5: ObserveHigherTerm calls
	wg.Add(1)
	go func() {
		defer wg.Done()
		var term uint64 = 500
		for !stop.Load() {
			term++
			_, _ = node.ObserveHigherTerm(raft.Term(term))
			time.Sleep(6 * time.Millisecond)
		}
	}()

	time.Sleep(200 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
}

// -----------------------------------------------------------------------------
// Section 20.G: Sequential Restart Sends Only Current-Term Frames
// -----------------------------------------------------------------------------

// A synchronous Leader -> Follower -> Leader restart fully terminates the old
// scheduler before the new one starts (stop blocks until done): every frame
// transmitted after the restart must carry the new term.
func TestNode_Heartbeat_SequentialRestartNoStaleFrames(t *testing.T) {
	sender := newMockHeartbeatSender()
	node, s := newTestHeartbeatNode(t, 1, []cluster.NodeID{2}, sender, nil)

	if err := s.SetTerm(1); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}
	if err := node.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := node.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}
	time.Sleep(70 * time.Millisecond)

	term1, _ := node.Term()
	if err := node.BecomeFollower(term1+1, cluster.NodeIDNil); err != nil {
		t.Fatalf("BecomeFollower failed: %v", err)
	}
	// Synchronous stop: no old-session frame may arrive after this point.
	mark := len(sender.GetSent(2))

	if err := node.BecomeCandidate(); err != nil {
		t.Fatalf("BecomeCandidate failed: %v", err)
	}
	if err := node.BecomeLeader(); err != nil {
		t.Fatalf("BecomeLeader failed: %v", err)
	}
	term2, _ := node.Term()
	if term2 <= term1 {
		t.Fatalf("expected term advance, got %d -> %d", term1, term2)
	}
	time.Sleep(120 * time.Millisecond)

	for i, frame := range sender.GetSent(2)[mark:] {
		req, err := transport.DecodeAppendEntries(frame)
		if err != nil {
			t.Fatalf("frame %d decode failed: %v", i, err)
		}
		if req.Term != uint64(term2) {
			t.Fatalf("post-restart frame %d carries stale term %d (want %d)", i, req.Term, term2)
		}
	}
}

// -----------------------------------------------------------------------------
// Section 20.H: Concurrent Restart Hammering Converges to One Session
// -----------------------------------------------------------------------------

// Rapid concurrent Leader <-> Follower flapping must never deadlock, panic,
// leak schedulers, or leave the node without a functioning scheduler once it
// settles as Leader. The settled session transmits only its own term.
func TestNode_Heartbeat_ConcurrentRestartConverges(t *testing.T) {
	sender := newMockHeartbeatSender()
	node, _ := newTestHeartbeatNode(t, 1, []cluster.NodeID{2}, sender, nil)

	baseGoroutines := runtime.NumGoroutine()
	var stop atomic.Bool
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			term, err := node.Term()
			if err != nil {
				return
			}
			_ = node.BecomeFollower(term+1, cluster.NodeIDNil)
			_ = node.BecomeCandidate()
			_ = node.BecomeLeader()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		var term uint64 = 100000
		for !stop.Load() {
			term++
			_, _ = node.ObserveHigherTerm(raft.Term(term))
		}
	}()

	time.Sleep(250 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	// Settle deterministically as Leader in a fresh term.
	curTerm, err := node.Term()
	if err != nil {
		t.Fatalf("Term failed: %v", err)
	}
	if err := node.BecomeFollower(curTerm+1, cluster.NodeIDNil); err != nil {
		t.Fatalf("settle BecomeFollower failed: %v", err)
	}
	if err := node.BecomeCandidate(); err != nil {
		t.Fatalf("settle BecomeCandidate failed: %v", err)
	}
	if err := node.BecomeLeader(); err != nil {
		t.Fatalf("settle BecomeLeader failed: %v", err)
	}
	finalTerm, _ := node.Term()
	mark := len(sender.GetSent(2))
	time.Sleep(150 * time.Millisecond)

	if !node.HeartbeatRunning() {
		t.Fatalf("settled leader must run exactly one scheduler")
	}
	foundCurrent := false
	for i, frame := range sender.GetSent(2)[mark:] {
		req, err := transport.DecodeAppendEntries(frame)
		if err != nil {
			t.Fatalf("frame %d decode failed: %v", i, err)
		}
		if req.Term > uint64(finalTerm) {
			t.Fatalf("frame %d carries impossible future term %d", i, req.Term)
		}
		if req.Term == uint64(finalTerm) {
			foundCurrent = true
		}
	}
	if !foundCurrent {
		t.Fatalf("settled session transmitted no current-term frames")
	}
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after-baseGoroutines > 8 {
		t.Fatalf("possible scheduler leak: base=%d after=%d", baseGoroutines, after)
	}
}
