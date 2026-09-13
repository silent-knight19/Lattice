package version

import (
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// -----------------------------------------------------------------------------
// 1. BOUNDARY TESTS (§14)
// -----------------------------------------------------------------------------

// TestSEC005_TryRef_AtMaxInt64 verifies that calling TryRef() on a Version whose
// reference count is already math.MaxInt64 fails cleanly (returns false) without
// mutating the counter, panicking, or wrapping into the negative range (§14.1).
func TestSEC005_TryRef_AtMaxInt64(t *testing.T) {
	v := NewVersion([NumLevels][]FileMetadata{})
	v.SetRefCountForTesting(math.MaxInt64)

	if v.RefCount() != math.MaxInt64 {
		t.Fatalf("setup failed: expected refCount = %d, got %d", int64(math.MaxInt64), v.RefCount())
	}

	ok := v.TryRef()
	if ok {
		t.Fatalf("TryRef() at MaxInt64 unexpectedly succeeded; reference count must not exceed MaxInt64")
	}

	if cur := v.RefCount(); cur != math.MaxInt64 {
		t.Fatalf("TryRef() mutated refCount on failure: expected %d, got %d", int64(math.MaxInt64), cur)
	}
}

// TestSEC005_Ref_AtMaxInt64 verifies that calling Ref() on an exhausted Version
// (refCount == math.MaxInt64) panics with an explicit overflow/exhaustion message
// rather than falsely reporting a dead Version, and leaves the counter unchanged (§14.2).
func TestSEC005_Ref_AtMaxInt64(t *testing.T) {
	v := NewVersion([NumLevels][]FileMetadata{})
	v.SetRefCountForTesting(math.MaxInt64)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected Ref() at MaxInt64 to panic, but it returned normally")
		}

		msg, ok := r.(string)
		if !ok {
			t.Fatalf("unexpected panic type: %T (%v)", r, r)
		}

		if strings.Contains(msg, "dead Version") {
			t.Fatalf("Ref() falsely reported dead Version on live exhausted object: %s", msg)
		}
		if !strings.Contains(msg, "exhausted") && !strings.Contains(msg, "MaxInt64") {
			t.Fatalf("expected exhaustion/MaxInt64 error message, got: %s", msg)
		}

		if cur := v.RefCount(); cur != math.MaxInt64 {
			t.Fatalf("Ref() mutated refCount on panic: expected %d, got %d", int64(math.MaxInt64), cur)
		}
	}()

	v.Ref()
}

// TestSEC005_OneBelowMaximum verifies that a Version at MaxInt64 - 1 successfully
// transitions to MaxInt64 on TryRef(), and that a subsequent acquisition immediately fails (§14.3).
func TestSEC005_OneBelowMaximum(t *testing.T) {
	v := NewVersion([NumLevels][]FileMetadata{})
	v.SetRefCountForTesting(math.MaxInt64 - 1)

	// Transition MaxInt64 - 1 -> MaxInt64 must succeed
	if !v.TryRef() {
		t.Fatalf("expected TryRef() at MaxInt64 - 1 to succeed")
	}
	if cur := v.RefCount(); cur != math.MaxInt64 {
		t.Fatalf("expected refCount = %d, got %d", int64(math.MaxInt64), cur)
	}

	// Subsequent transition must fail
	if v.TryRef() {
		t.Fatalf("subsequent TryRef() unexpectedly succeeded at MaxInt64")
	}
	if cur := v.RefCount(); cur != math.MaxInt64 {
		t.Fatalf("refCount corrupted: expected %d, got %d", int64(math.MaxInt64), cur)
	}
}

// TestSEC005_MaxRelease verifies that a Version at math.MaxInt64 is a valid live
// object from which references can be released via Unref() down to MaxInt64 - 1 (§14.4).
func TestSEC005_MaxRelease(t *testing.T) {
	v := NewVersion([NumLevels][]FileMetadata{})
	v.SetRefCountForTesting(math.MaxInt64)

	// Unref from MaxInt64 -> MaxInt64 - 1
	v.Unref()

	if cur := v.RefCount(); cur != math.MaxInt64-1 {
		t.Fatalf("expected refCount = %d after Unref(), got %d", int64(math.MaxInt64-1), cur)
	}
}

// TestSEC005_BoundaryRoundTrip exercises the complete cycle across the upper boundary:
// MaxInt64 - 2 -> MaxInt64 - 1 -> MaxInt64 -> rejected -> MaxInt64 - 1 -> MaxInt64 - 2 (§14.5).
func TestSEC005_BoundaryRoundTrip(t *testing.T) {
	v := NewVersion([NumLevels][]FileMetadata{})
	v.SetRefCountForTesting(math.MaxInt64 - 2)

	// Step 1: MaxInt64 - 2 -> MaxInt64 - 1
	if !v.TryRef() {
		t.Fatalf("step 1 failed")
	}
	if v.RefCount() != math.MaxInt64-1 {
		t.Fatalf("step 1 count mismatch: got %d", v.RefCount())
	}

	// Step 2: MaxInt64 - 1 -> MaxInt64
	if !v.TryRef() {
		t.Fatalf("step 2 failed")
	}
	if v.RefCount() != math.MaxInt64 {
		t.Fatalf("step 2 count mismatch: got %d", v.RefCount())
	}

	// Step 3: Rejected acquisition at MaxInt64
	if v.TryRef() {
		t.Fatalf("step 3 unexpectedly succeeded")
	}
	if v.RefCount() != math.MaxInt64 {
		t.Fatalf("step 3 count mutated: got %d", v.RefCount())
	}

	// Step 4: MaxInt64 -> MaxInt64 - 1
	v.Unref()
	if v.RefCount() != math.MaxInt64-1 {
		t.Fatalf("step 4 count mismatch: got %d", v.RefCount())
	}

	// Step 5: MaxInt64 - 1 -> MaxInt64 - 2
	v.Unref()
	if v.RefCount() != math.MaxInt64-2 {
		t.Fatalf("step 5 count mismatch: got %d", v.RefCount())
	}
}

// -----------------------------------------------------------------------------
// 2. DEAD VERSION & UNDERFLOW REGRESSION (§15, §16)
// -----------------------------------------------------------------------------

// TestSEC005_DeadVersionDistinction ensures dead versions (count <= 0) and
// live-but-exhausted versions (count == MaxInt32) produce distinctly typed panic
// messages and identical rejection via TryRef() (§15).
func TestSEC005_DeadVersionDistinction(t *testing.T) {
	vDead := NewVersion([NumLevels][]FileMetadata{})
	vDead.Unref() // count is 0

	if vDead.TryRef() {
		t.Fatalf("TryRef on dead version returned true")
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected Ref() on dead version to panic")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("unexpected panic type: %T (%v)", r, r)
		}
		if !strings.Contains(msg, "cannot Ref dead Version") {
			t.Fatalf("expected dead Version panic message, got: %s", msg)
		}
	}()
	vDead.Ref()
}

// TestSEC005_UnderflowRegression verifies that Unref underflow detection is preserved (§16).
func TestSEC005_UnderflowRegression(t *testing.T) {
	v := NewVersion([NumLevels][]FileMetadata{})
	v.Unref() // transitions 1 -> 0

	if v.RefCount() != 0 {
		t.Fatalf("expected refCount = 0, got %d", v.RefCount())
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected double Unref to panic")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("unexpected panic type: %T (%v)", r, r)
		}
		if !strings.Contains(msg, "underflow") {
			t.Fatalf("expected underflow panic message, got: %s", msg)
		}
		if v.RefCount() != 0 {
			t.Fatalf("underflow mutated count: %d", v.RefCount())
		}
	}()
	v.Unref()
}

// -----------------------------------------------------------------------------
// 3. CONCURRENT MAXIMUM-COLLISION TEST (§18)
// -----------------------------------------------------------------------------

// TestSEC005_ConcurrentCollision tests that when multiple goroutines concurrently
// attempt TryRef() on a Version starting at math.MaxInt32 - 1, EXACTLY ONE goroutine
// succeeds in transitioning to MaxInt32, all others fail cleanly, and the final
// count is exactly math.MaxInt64 without wrapping or data races (§18).
func TestSEC005_ConcurrentCollision(t *testing.T) {
	v := NewVersion([NumLevels][]FileMetadata{})
	v.SetRefCountForTesting(math.MaxInt64 - 1)

	const numGoroutines = 50
	var wg sync.WaitGroup
	var successCount atomic.Int32
	var failCount atomic.Int32

	startGate := make(chan struct{})

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startGate // Synchronize launch

			if v.TryRef() {
				successCount.Add(1)
			} else {
				failCount.Add(1)
			}
		}()
	}

	close(startGate) // Release all goroutines simultaneously
	wg.Wait()

	if got := successCount.Load(); got != 1 {
		t.Fatalf("expected exactly 1 goroutine to acquire reference, got %d", got)
	}
	if got := failCount.Load(); got != numGoroutines-1 {
		t.Fatalf("expected %d goroutines to fail, got %d", numGoroutines-1, got)
	}
	if cur := v.RefCount(); cur != math.MaxInt64 {
		t.Fatalf("expected final refCount = %d, got %d", int64(math.MaxInt64), cur)
	}

	// Clean up acquired reference to leave state tidy
	v.Unref()
	if cur := v.RefCount(); cur != math.MaxInt64-1 {
		t.Fatalf("expected refCount = %d after cleanup Unref, got %d", int64(math.MaxInt64-1), cur)
	}
}

// -----------------------------------------------------------------------------
// 4. CONCURRENT STRESS TEST (§17)
// -----------------------------------------------------------------------------

// TestSEC005_ConcurrentStress runs high-contention mixed acquisitions and releases
// near the maximum boundary under the race detector to ensure no lost updates,
// no negative counts, and exact accounting (§17).
func TestSEC005_ConcurrentStress(t *testing.T) {
	const initialCount = math.MaxInt64 - 100
	v := NewVersion([NumLevels][]FileMetadata{})
	v.SetRefCountForTesting(initialCount)

	const numWorkers = 16
	const opsPerWorker = 200
	var wg sync.WaitGroup

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerWorker; j++ {
				if v.TryRef() {
					// Verify we never observe negative or wrapped count
					cur := v.RefCount()
					if cur <= 0 {
						t.Errorf("observed non-positive refCount during acquisition: %d", cur)
					}
					v.Unref()
				}
			}
		}()
	}

	wg.Wait()

	if cur := v.RefCount(); cur != initialCount {
		t.Fatalf("accounting divergence after concurrent stress: expected %d, got %d", initialCount, cur)
	}
}

// -----------------------------------------------------------------------------
// 5. PROPERTY TEST: ARITHMETIC SAFETY (§19)
// -----------------------------------------------------------------------------

// TestSEC005_Property_ArithmeticSafety asserts invariant bounds across arbitrary
// boundary states: [0, 1, MaxInt64-2, MaxInt64-1, MaxInt64].
func TestSEC005_Property_ArithmeticSafety(t *testing.T) {
	testStates := []int64{
		0,
		1,
		math.MaxInt64 - 2,
		math.MaxInt64 - 1,
		math.MaxInt64,
	}

	for _, st := range testStates {
		v := NewVersion([NumLevels][]FileMetadata{})
		v.SetRefCountForTesting(st)

		curBefore := v.RefCount()
		ok := v.TryRef()
		curAfter := v.RefCount()

		if st <= 0 {
			if ok {
				t.Errorf("state %d: TryRef succeeded on non-positive state", st)
			}
			if curAfter != curBefore {
				t.Errorf("state %d: TryRef mutated count on failure: %d -> %d", st, curBefore, curAfter)
			}
		} else if st == math.MaxInt64 {
			if ok {
				t.Errorf("state %d: TryRef succeeded on MaxInt64", st)
			}
			if curAfter != curBefore {
				t.Errorf("state %d: TryRef mutated count on failure: %d -> %d", st, curBefore, curAfter)
			}
		} else {
			if !ok {
				t.Errorf("state %d: TryRef failed on valid state", st)
			}
			if curAfter != curBefore+1 {
				t.Errorf("state %d: expected %d, got %d", st, curBefore+1, curAfter)
			}
			// Decrement back
			v.Unref()
			if v.RefCount() != curBefore {
				t.Errorf("state %d: Unref did not restore initial count: got %d", st, v.RefCount())
			}
		}
	}
}
