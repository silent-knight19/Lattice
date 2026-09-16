package engine

import (
	"context"
	"time"

	"github.com/silent-knight19/lattice/internal/errors"
)

// P10-S01-M03 progressive write pacing and stall thresholds.
//
// Boundary semantics follow the roadmap literally ("exceeds"):
//   - L0 <= 8: normal writes, no added delay.
//   - L0 9..12: progressive pacing delay, monotonically increasing.
//   - L0 > 12: hard stall until pressure falls back to L0 <= 12.
//
// L0 == 8 is normal. L0 == 12 is paced (largest pacing delay), not stalled.
const (
	// L0PacingThreshold is the L0 file count above which writes are paced.
	L0PacingThreshold = 8
	// L0StallThreshold is the L0 file count above which writes stall.
	L0StallThreshold = 12

	// l0StallPollInterval bounds each stall wait slice. Writers never spin;
	// they sleep in bounded intervals and re-read authoritative pressure.
	l0StallPollInterval = 20 * time.Millisecond
)

// Pacing delays are fixed constants (no arithmetic on pressure input, no
// overflow, no unbounded sleep). Monotonic: 9 < 10 < 11 < 12.
const (
	l0Delay9  = 1 * time.Millisecond
	l0Delay10 = 5 * time.Millisecond
	l0Delay11 = 15 * time.Millisecond
	l0Delay12 = 30 * time.Millisecond
)

// l0PacingDelay is the pure policy function: pressure count in, added delay
// out. Counts above the stall threshold return 0 because they are handled by
// the stall path, not pacing. Separated from sleeping so policy is
// unit-testable without wall-clock timing.
func l0PacingDelay(l0Count int) time.Duration {
	switch {
	case l0Count <= L0PacingThreshold:
		return 0
	case l0Count == 9:
		return l0Delay9
	case l0Count == 10:
		return l0Delay10
	case l0Count == 11:
		return l0Delay11
	case l0Count == 12:
		return l0Delay12
	default:
		return 0
	}
}

// l0NeedsStall reports whether the count requires a hard stall (> 12).
func l0NeedsStall(l0Count int) bool {
	return l0Count > L0StallThreshold
}

// SetL0CountOverrideForTesting forces the pressure source to n for
// deterministic policy tests. Negative n clears the override (real VersionSet
// state). Returns a restore function.
func (e *Engine) SetL0CountOverrideForTesting(n int) func() {
	if e == nil {
		return func() {}
	}
	if n < 0 {
		e.l0Override.Store(-1)
		return func() {}
	}
	prev := e.l0Override.Swap(int64(n))
	return func() { e.l0Override.Store(prev) }
}

// l0FileCount returns the authoritative L0 pressure: the number of L0 files
// in the pinned current Version. It acquires the Version, inspects it, and
// releases it before returning; it never retains the reference across sleeps
// and holds no Engine/WAL/MemTable/VersionSet/cache locks while the caller
// waits. A test override, when set, takes precedence without touching the
// VersionSet.
func (e *Engine) l0FileCount() int {
	if e == nil {
		return 0
	}
	if ov := e.l0Override.Load(); ov >= 0 {
		return int(ov)
	}
	vs := e.VersionSet()
	if vs == nil || !vs.HasCurrent() {
		return 0
	}
	ver := vs.Current()
	if ver == nil {
		return 0
	}
	n := ver.NumFiles(0)
	ver.Unref()
	if n < 0 {
		return 0
	}
	return n
}

// gateL0Write enforces M03 backpressure before sequence allocation. It must be
// called after key/value validation and before backpressure.Acquire/sequence
// allocation so stalled writers reserve no sequence numbers and retain no
// internal copies (only caller-owned args). It holds no storage locks while
// waiting. Reads are never gated. Stall is a control behavior, not a write
// failure: it returns nil once pressure allows, ctx.Err() on cancellation, or
// ErrWriterClosed when the Engine is unusable.
func (e *Engine) gateL0Write(ctx context.Context) error {
	if e == nil {
		return errors.ErrNilReceiver
	}
	// Stall phase: wait until L0 <= 12. Bounded polls; each slice honors
	// cancellation and lifecycle without holding any lock.
	for {
		if e.closed.Load() {
			return errors.ErrWriterClosed
		}
		select {
		case <-e.stopChan():
			return errors.ErrWriterClosed
		default:
		}
		if ctx != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		if !l0NeedsStall(e.l0FileCount()) {
			break
		}
		if ctx != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-e.stopChan():
				return errors.ErrWriterClosed
			case <-time.After(l0StallPollInterval):
			}
		} else {
			select {
			case <-e.stopChan():
				return errors.ErrWriterClosed
			case <-time.After(l0StallPollInterval):
			}
		}
		if e.closed.Load() {
			return errors.ErrWriterClosed
		}
	}
	// Pacing phase: single bounded delay for the fresh count. Zero-delay
	// writes allocate no timer.
	delay := l0PacingDelay(e.l0FileCount())
	if delay <= 0 {
		return nil
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.stopChan():
			return errors.ErrWriterClosed
		case <-time.After(delay):
		}
	} else {
		select {
		case <-e.stopChan():
			return errors.ErrWriterClosed
		case <-time.After(delay):
		}
	}
	if e.closed.Load() {
		return errors.ErrWriterClosed
	}
	return nil
}

// stopChan returns the Engine stop channel without exposing the field. A nil
// channel blocks forever in selects, which is the correct behavior for
// memory-only engines with no worker lifecycle.
func (e *Engine) stopChan() <-chan struct{} {
	if e == nil {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.stopCh
}
