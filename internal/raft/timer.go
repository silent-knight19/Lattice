package raft

import (
	cryptorand "crypto/rand"
	"encoding/binary"
	"sync"
	"time"
)

const (
	// DefaultMinElectionTimeout is the lower bound of the election timeout interval (150ms).
	DefaultMinElectionTimeout = 150 * time.Millisecond

	// DefaultMaxElectionTimeout is the upper bound of the election timeout interval (300ms).
	DefaultMaxElectionTimeout = 300 * time.Millisecond
)

// DurationProvider generates a randomized or deterministic duration for an election round.
type DurationProvider func() time.Duration

// DefaultDurationProvider generates a uniformly distributed election timeout duration
// within [150ms, 300ms] using cryptographic entropy.
func DefaultDurationProvider() time.Duration {
	var b [8]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		// Defensive fallback using high-resolution timestamp if OS entropy source is unavailable
		spanNs := int64(DefaultMaxElectionTimeout - DefaultMinElectionTimeout)
		offset := time.Now().UnixNano() % (spanNs + 1)
		return DefaultMinElectionTimeout + time.Duration(offset)
	}

	val := binary.BigEndian.Uint64(b[:])
	spanNs := uint64(DefaultMaxElectionTimeout - DefaultMinElectionTimeout)
	offset := val % (spanNs + 1)
	return DefaultMinElectionTimeout + time.Duration(offset)
}

// ElectionTimer manages a resettable, stoppable, generation-tracked election countdown.
//
// Invariants enforced (P15-S02-M01):
//  1. Election timeout duration is chosen dynamically per election round via DurationProvider.
//  2. Monotonically increasing generation counter invalidates stale timer firings from prior rounds.
//  3. Thread-safe: Reset, Stop, and Close can be called concurrently from any goroutine.
//  4. No leaked timers or lingering goroutines across reset/stop cycles.
//  5. Stale timer callbacks cannot deliver ticks after Reset, Stop, or Close.
type ElectionTimer struct {
	mu       sync.Mutex
	provider DurationProvider
	timer    *time.Timer
	gen      uint64
	notifyCh chan uint64
	stopped  bool
	closed   bool
}

// NewElectionTimer constructs a new ElectionTimer using the given provider.
// If provider is nil, DefaultDurationProvider is used.
// The timer is constructed in a stopped state until Reset() is explicitly invoked.
func NewElectionTimer(provider DurationProvider) *ElectionTimer {
	if provider == nil {
		provider = DefaultDurationProvider
	}
	return &ElectionTimer{
		provider: provider,
		notifyCh: make(chan uint64, 1),
		stopped:  true,
	}
}

// Reset cancels any pending timer, increments the generation counter, drains prior notifications,
// and arms the timer with a freshly chosen timeout duration.
func (t *ElectionTimer) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return
	}

	if t.timer != nil {
		t.timer.Stop()
	}

	t.gen++
	t.drainLocked()
	t.stopped = false

	curGen := t.gen
	dur := t.provider()

	t.timer = time.AfterFunc(dur, func() {
		t.mu.Lock()
		defer t.mu.Unlock()

		if t.closed || t.stopped || t.gen != curGen {
			return
		}

		select {
		case t.notifyCh <- curGen:
		default:
		}
	})
}

// Stop halts the election timer, increments the generation counter to invalidate in-flight callbacks,
// and drains any queued notifications.
func (t *ElectionTimer) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return
	}

	if t.timer != nil {
		t.timer.Stop()
	}

	t.gen++
	t.drainLocked()
	t.stopped = true
}

// Close permanently shuts down the election timer.
func (t *ElectionTimer) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return
	}

	t.closed = true
	t.stopped = true
	if t.timer != nil {
		t.timer.Stop()
	}

	t.gen++
	t.drainLocked()
}

// C returns the receive-only channel carrying generation identifiers when the timer expires.
func (t *ElectionTimer) C() <-chan uint64 {
	return t.notifyCh
}

// CurrentGen returns the current active generation counter.
func (t *ElectionTimer) CurrentGen() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.gen
}

// IsStopped reports whether the timer is currently stopped or closed.
func (t *ElectionTimer) IsStopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopped || t.closed
}

// drainLocked removes any buffered generation from notifyCh. Caller must hold t.mu.
func (t *ElectionTimer) drainLocked() {
	for {
		select {
		case <-t.notifyCh:
		default:
			return
		}
	}
}
