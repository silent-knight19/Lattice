package raft

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	latticeErrors "github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/transport"
)

// =============================================================================
// Apply-loop liveness under retryable state-machine errors (regression).
//
// The apply loop is single-shot: applyLoop returns on the first recorded ApplyError,
// and setApplyError never clears it. Any state-machine error was therefore terminal.
//
// That became materially wrong once Engine.Put could return ErrL0StallTimeout, an
// explicitly retryable bounded throttle. A committed Raft entry met with transient
// write pressure would permanently freeze lastApplied and strand every committed
// entry behind it, on a replica that was otherwise healthy.
// =============================================================================

// pressureStateMachine declines the first declineCount Puts with a retryable error
// shaped exactly like Engine.Put's bounded L0 stall, then succeeds.
type pressureStateMachine struct {
	mu        sync.Mutex
	remaining int
	applied   []string
	puts      atomic.Int64
}

func newPressureStateMachine(declineCount int) *pressureStateMachine {
	return &pressureStateMachine{remaining: declineCount}
}

func (p *pressureStateMachine) Put(_ context.Context, key, _ []byte) error {
	p.puts.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.remaining > 0 {
		p.remaining--
		return &latticeErrors.L0StallTimeoutError{L0Count: 16, Threshold: 12, Waited: time.Millisecond}
	}
	p.applied = append(p.applied, string(key))
	return nil
}

func (p *pressureStateMachine) appliedKeys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.applied))
	copy(out, p.applied)
	return out
}

func (p *pressureStateMachine) Get(key []byte) ([]byte, error) {
	for _, k := range p.appliedKeys() {
		if k == string(key) {
			return []byte("v"), nil
		}
	}
	return nil, latticeErrors.ErrKeyNotFound
}

func (p *pressureStateMachine) Delete(context.Context, []byte) error          { return nil }
func (p *pressureStateMachine) Batch(context.Context, []binary.BatchOp) error { return nil }
func (p *pressureStateMachine) Exists(key []byte) (bool, error) {
	_, e := p.Get(key)
	return e == nil, nil
}

func (p *pressureStateMachine) Stats() (transport.EngineStats, transport.MemoryStats, transport.StorageStats, transport.CacheStats, error) {
	return transport.EngineStats{}, transport.MemoryStats{}, transport.StorageStats{}, transport.CacheStats{}, nil
}

// alwaysFailingStateMachine refuses every Put with a terminal, non-retryable error.
type alwaysFailingStateMachine struct{ puts atomic.Int64 }

func (a *alwaysFailingStateMachine) Put(context.Context, []byte, []byte) error {
	a.puts.Add(1)
	return fmt.Errorf("state machine is fundamentally broken")
}
func (a *alwaysFailingStateMachine) Get([]byte) ([]byte, error)                    { return nil, nil }
func (a *alwaysFailingStateMachine) Delete(context.Context, []byte) error          { return nil }
func (a *alwaysFailingStateMachine) Batch(context.Context, []binary.BatchOp) error { return nil }
func (a *alwaysFailingStateMachine) Exists([]byte) (bool, error)                   { return false, nil }

func (a *alwaysFailingStateMachine) Stats() (transport.EngineStats, transport.MemoryStats, transport.StorageStats, transport.CacheStats, error) {
	return transport.EngineStats{}, transport.MemoryStats{}, transport.StorageStats{}, transport.CacheStats{}, nil
}

// putCommand builds an encoded Put command for key.
func putCommand(t *testing.T, key string) []byte {
	t.Helper()
	data, err := EncodeCommand(Command{
		Op:    binary.OpTypePut,
		Key:   []byte(key),
		Value: []byte("v"),
	})
	if err != nil {
		t.Fatalf("EncodeCommand: %v", err)
	}
	return data
}

// commitAndApply appends one committed Put entry and lets the apply loop drain it.
func commitAndApply(t *testing.T, n *Node, s *Storage, key string) {
	t.Helper()
	if err := s.Append(LogEntry{
		Index: 1,
		Term:  1,
		Type:  transport.PeerEntryNormal,
		Data:  putCommand(t, key),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	n.mu.Lock()
	n.commitIndex = 1
	n.signalApplyLocked()
	n.mu.Unlock()
}

// TestApplyLoop_RetryableStallDoesNotHalt is the core regression.
//
// Pre-fix, three ErrL0StallTimeout refusals recorded a terminal ApplyError and the
// apply loop exited, so the committed entry was never applied.
func TestApplyLoop_RetryableStallDoesNotHalt(t *testing.T) {
	sm := newPressureStateMachine(3)
	n, s, cleanup := newTestNodeWithSM(t, 1, sm, 16)
	defer cleanup()

	commitAndApply(t, n, s, "k")

	waitForAppliedCount(t, sm, 1, 20*time.Second)

	if got := sm.appliedKeys(); len(got) != 1 || got[0] != "k" {
		t.Fatalf("applied %v; want [k] after 3 retryable refusals", got)
	}
	if n.ApplyError() != nil {
		t.Errorf("ApplyError = %v; a retryable stall must not be recorded as terminal", n.ApplyError())
	}
	if got := n.LastApplied(); got != 1 {
		t.Errorf("LastApplied = %d; want 1", got)
	}
}

// TestApplyLoop_TerminalErrorStillHalts guards the other direction: a genuine defect
// must not be retried forever, or the loop would spin and mask the fault.
func TestApplyLoop_TerminalErrorStillHalts(t *testing.T) {
	sm := &alwaysFailingStateMachine{}
	n, s, cleanup := newTestNodeWithSM(t, 1, sm, 16)
	defer cleanup()

	commitAndApply(t, n, s, "k")

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && n.ApplyError() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	if n.ApplyError() == nil {
		t.Fatal("a permanently failing state machine produced no terminal ApplyError; " +
			"the retry path must not swallow genuine defects")
	}

	// It must halt rather than spin.
	first := sm.puts.Load()
	time.Sleep(400 * time.Millisecond)
	if grew := sm.puts.Load() - first; grew > 2 {
		t.Errorf("terminal error kept retrying (%d more attempts); the apply loop should halt", grew)
	}
}

// TestApplyLoop_RetryThenSuccessResetsBackoff verifies the exponential backoff does
// not ratchet up permanently across separate transient stalls.
func TestApplyLoop_RetryThenSuccessResetsBackoff(t *testing.T) {
	sm := newPressureStateMachine(2)
	n, s, cleanup := newTestNodeWithSM(t, 1, sm, 16)
	defer cleanup()

	commitAndApply(t, n, s, "first")
	waitForAppliedCount(t, sm, 1, 20*time.Second)

	// A second, independent transient stall must also be survivable.
	sm.mu.Lock()
	sm.remaining = 2
	sm.mu.Unlock()

	if err := s.Append(LogEntry{
		Index: 2, Term: 1, Type: transport.PeerEntryNormal,
		Data: putCommand(t, "second"),
	}); err != nil {
		t.Fatalf("Append(2): %v", err)
	}
	n.mu.Lock()
	n.commitIndex = 2
	n.signalApplyLocked()
	n.mu.Unlock()

	waitForAppliedCount(t, sm, 2, 20*time.Second)

	if n.ApplyError() != nil {
		t.Errorf("ApplyError = %v after a second transient stall", n.ApplyError())
	}
	n.applyErrMu.Lock()
	attempts := n.applyRetryAttempts
	n.applyErrMu.Unlock()
	if attempts != 0 {
		t.Errorf("applyRetryAttempts = %d after a successful apply; backoff should reset", attempts)
	}
}

// TestIsRetryableStateMachineError pins the classifier the loop depends on.
func TestIsRetryableStateMachineError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"l0 stall", &latticeErrors.L0StallTimeoutError{L0Count: 16, Threshold: 12}, true},
		{"wrapped l0 stall", fmt.Errorf("apply: %w",
			&latticeErrors.L0StallTimeoutError{L0Count: 16, Threshold: 12}), true},
		{"memory limit", fmt.Errorf("x: %w", latticeErrors.ErrMemoryLimitExceeded), true},
		{"write throttled", fmt.Errorf("x: %w", latticeErrors.ErrWriteThrottled), true},
		{"poisoned storage", fmt.Errorf("x: %w", latticeErrors.ErrRaftStoragePoisoned), false},
		{"corrupted state", fmt.Errorf("x: %w", latticeErrors.ErrRaftCorruptedState), false},
		{"arbitrary error", fmt.Errorf("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableStateMachineError(tc.err); got != tc.want {
				t.Errorf("isRetryableStateMachineError(%v) = %v; want %v", tc.err, got, tc.want)
			}
		})
	}
}

// waitForAppliedCount waits until the state machine has applied want entries.
func waitForAppliedCount(t *testing.T, sm *pressureStateMachine, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(sm.appliedKeys()) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
