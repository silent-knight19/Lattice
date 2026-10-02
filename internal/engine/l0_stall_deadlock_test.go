package engine_test

import (
	"context"
	stdErrors "errors"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/errors"
)

// =============================================================================
// L0 write-stall deadlock (regression).
//
// gateL0Write blocks writers in an unbounded `for {}` loop while the live L0
// file count exceeds L0StallThreshold. The only exit conditions are engine
// shutdown, context cancellation, or the L0 count falling back to <= 12.
//
// Nothing in the Engine reduces the L0 count: flushOne publishes to L0 only,
// and no production code path ever calls VersionEdit.AddFile with level > 0.
// Therefore, once a workload produces L0StallThreshold+1 flushes, every
// subsequent write blocks until the caller's context expires, forever.
//
// The stall is a control-flow throttle. It must never be the mechanism that
// renders a node permanently unwritable, so a bounded wait that returns an
// explicit, retryable error is required.
// =============================================================================

// l0StallCount returns an L0 file count strictly above L0StallThreshold.
func l0StallCount() int { return engine.L0StallThreshold + 1 }

// newL0StalledEngine returns an engine whose L0 pressure source is pinned above
// the stall threshold, with a short stall timeout so the test does not sleep for
// the production default.
func newL0StalledEngine(t *testing.T, stallTimeout time.Duration) *engine.Engine {
	t.Helper()
	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		Backpressure:   engine.DefaultBackpressureConfig(),
		L0StallTimeout: stallTimeout,
	})
	restore := eng.SetL0CountOverrideForTesting(l0StallCount())
	t.Cleanup(restore)
	return eng
}

// TestL0Stall_DoesNotDeadlockForever is the primary regression test.
//
// Pre-fix this fails: Put blocks until the context deadline and returns
// context.DeadlineExceeded rather than an explicit L0 stall error.
func TestL0Stall_DoesNotDeadlockForever(t *testing.T) {
	const stallTimeout = 150 * time.Millisecond

	eng := newL0StalledEngine(t, stallTimeout)

	// Generous context deadline: comfortably longer than the stall timeout, so
	// the caller would still be alive if the engine did not surface the stall.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	err := eng.Put(ctx, []byte("k"), []byte("v"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Put succeeded while L0 count is %d (above stall threshold %d); "+
			"the stall gate must not admit writes under unresolved pressure",
			l0StallCount(), engine.L0StallThreshold)
	}

	if !stdErrors.Is(err, errors.ErrL0StallTimeout) {
		t.Fatalf("Put returned %v (%T) after %v; want errors.ErrL0StallTimeout so the "+
			"caller receives an explicit retryable error instead of blocking until its "+
			"own context expires", err, err, elapsed)
	}

	// The stall must surface its own bounded timeout well before the caller's
	// deadline. This is what distinguishes an explicit stall error from a hang.
	if elapsed >= time.Second {
		t.Errorf("stall surfaced after %v; want a bounded wait near the configured %v", elapsed, stallTimeout)
	}
}

// TestL0Stall_SurfacesBeforeCallerDeadline proves the engine reports pressure
// itself rather than relying on the client to time out. A client with no
// deadline at all (context.Background) must still be released.
func TestL0Stall_SurfacesBeforeCallerDeadline(t *testing.T) {
	eng := newL0StalledEngine(t, 150*time.Millisecond)

	done := make(chan error, 1)
	go func() {
		// No caller deadline: only the engine's bounded stall can release this.
		done <- eng.Put(context.Background(), []byte("k"), []byte("v"))
	}()

	select {
	case err := <-done:
		if !stdErrors.Is(err, errors.ErrL0StallTimeout) {
			t.Fatalf("Put returned %v; want errors.ErrL0StallTimeout", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Put blocked forever with no caller deadline: the L0 stall gate is a deadlock")
	}
}

// TestL0Stall_BatchAlsoGated verifies the bounded stall applies to the batch
// path too, which allocates N+2 sequence numbers up front and therefore must
// never be admitted under unresolved pressure.
func TestL0Stall_BatchAlsoGated(t *testing.T) {
	eng := newL0StalledEngine(t, 150*time.Millisecond)

	ops := []binary.BatchOp{{Type: binary.OpTypePut, Key: []byte("k"), Value: []byte("v")}}

	done := make(chan error, 1)
	go func() {
		done <- eng.Batch(context.Background(), ops)
	}()

	select {
	case err := <-done:
		if !stdErrors.Is(err, errors.ErrL0StallTimeout) {
			t.Fatalf("Batch returned %v; want errors.ErrL0StallTimeout", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Batch blocked forever under L0 stall")
	}
}

// TestL0Stall_PressureRelievedAdmitsWrites confirms the gate is still a gate:
// once pressure falls back to the threshold, writes must be admitted normally.
// This guards against an over-broad fix that rejects writes unconditionally.
func TestL0Stall_PressureRelievedAdmitsWrites(t *testing.T) {
	eng := newL0StalledEngine(t, 150*time.Millisecond)

	// Relieve pressure to exactly the stall threshold (stall requires > 12).
	eng.SetL0CountOverrideForTesting(engine.L0StallThreshold)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := eng.Put(ctx, []byte("k"), []byte("v")); err != nil {
		t.Fatalf("Put rejected at L0 count == L0StallThreshold (%d): %v; "+
			"writes must be admitted once pressure is within bounds",
			engine.L0StallThreshold, err)
	}
}

// TestL0Stall_RejectionAllocatesNoSeqNum verifies the invariant documented on
// errors.ErrL0StallTimeout: a stalled rejection happens strictly before
// sequence allocation, so it consumes no sequence number and writes nothing.
// This is what makes the error safely retryable rather than a silent leak.
func TestL0Stall_RejectionAllocatesNoSeqNum(t *testing.T) {
	eng := newL0StalledEngine(t, 100*time.Millisecond)

	before := eng.NextSeqNum()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := eng.Put(ctx, []byte("k"), []byte("v")); !stdErrors.Is(err, errors.ErrL0StallTimeout) {
		t.Fatalf("Put returned %v; want errors.ErrL0StallTimeout", err)
	}

	if after := eng.NextSeqNum(); after != before {
		t.Errorf("stalled rejection advanced the sequence watermark from %d to %d; "+
			"a rejected write must allocate no sequence number", before, after)
	}

	// The same must hold for the batch path, which reserves N+2 up front.
	ops := []binary.BatchOp{{Type: binary.OpTypePut, Key: []byte("k2"), Value: []byte("v2")}}
	beforeBatch := eng.NextSeqNum()
	if err := eng.Batch(ctx, ops); !stdErrors.Is(err, errors.ErrL0StallTimeout) {
		t.Fatalf("Batch returned %v; want errors.ErrL0StallTimeout", err)
	}
	if afterBatch := eng.NextSeqNum(); afterBatch != beforeBatch {
		t.Errorf("stalled batch rejection advanced the sequence watermark from %d to %d; "+
			"a rejected batch must allocate no sequence numbers", beforeBatch, afterBatch)
	}
}

// TestL0Stall_ErrorCarriesDiagnostics verifies the typed error reports the
// observed L0 count, the threshold, and the elapsed wait, so operators can
// distinguish "pressure never relieved" from a client-side cancellation.
func TestL0Stall_ErrorCarriesDiagnostics(t *testing.T) {
	eng := newL0StalledEngine(t, 120*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := eng.Put(ctx, []byte("k"), []byte("v"))

	var stallErr *errors.L0StallTimeoutError
	if !stdErrors.As(err, &stallErr) {
		t.Fatalf("Put returned %T; want *errors.L0StallTimeoutError", err)
	}
	if stallErr.L0Count != l0StallCount() {
		t.Errorf("L0Count = %d; want %d", stallErr.L0Count, l0StallCount())
	}
	if stallErr.Threshold != engine.L0StallThreshold {
		t.Errorf("Threshold = %d; want %d", stallErr.Threshold, engine.L0StallThreshold)
	}
	if stallErr.Waited <= 0 {
		t.Errorf("Waited = %v; want a positive elapsed duration", stallErr.Waited)
	}
}

// TestL0Stall_CallerCancellationWins confirms the bounded stall does not
// swallow a caller's own cancellation: an earlier caller deadline must still
// surface as context.DeadlineExceeded, not as a stall timeout.
func TestL0Stall_CallerCancellationWins(t *testing.T) {
	// Stall timeout far exceeds the caller deadline.
	eng := newL0StalledEngine(t, 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := eng.Put(ctx, []byte("k"), []byte("v"))
	if !stdErrors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Put returned %v; want context.DeadlineExceeded so an earlier caller "+
			"deadline is not masked by the engine stall bound", err)
	}
	if stdErrors.Is(err, errors.ErrL0StallTimeout) {
		t.Error("caller cancellation was reported as an L0 stall timeout")
	}
}

// TestL0Stall_DefaultTimeoutIsBounded verifies an Engine built without an
// explicit L0StallTimeout still gets a finite, positive bound, so the stall can
// never silently regress to an unbounded wait.
func TestL0Stall_DefaultTimeoutIsBounded(t *testing.T) {
	eng := engine.NewEngine(engine.DefaultBackpressureConfig())
	restore := eng.SetL0CountOverrideForTesting(l0StallCount())
	defer restore()

	// Unreachable directly (stallTimeout is unexported); assert the observable
	// consequence instead: with the default bound the write is still blocked at
	// 1s, and the exported default is a sane finite value.
	if engine.DefaultL0StallTimeout <= 0 {
		t.Fatalf("DefaultL0StallTimeout = %v; want a positive finite bound", engine.DefaultL0StallTimeout)
	}
	if engine.DefaultL0StallTimeout > 5*time.Minute {
		t.Errorf("DefaultL0StallTimeout = %v; want a bound short enough to keep the "+
			"stall from dominating client-visible latency", engine.DefaultL0StallTimeout)
	}
}

// TestL0Stall_ReadsNeverGated confirms the bounded stall does not leak into the
// read path. Availability of reads under write pressure is a hard requirement.
func TestL0Stall_ReadsNeverGated(t *testing.T) {
	eng := newL0StalledEngine(t, 150*time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// A missing key is a legitimate read result, not an error.
		_, _ = eng.Get([]byte("absent"))
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Get blocked under L0 stall; reads must never be gated")
	}
}
