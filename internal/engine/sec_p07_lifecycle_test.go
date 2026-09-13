package engine_test

import (
	"context"
	stdErrors "errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/version"
	"github.com/silent-knight19/lattice/internal/wal"
)

// TestSEC_P07_01_ConcurrentRecoverySerialization verifies that concurrent RecoverWAL calls
// are strictly serialized, with one succeeding and concurrent attempts failing deterministically
// with ErrRecoveryInProgress.
func TestSEC_P07_01_ConcurrentRecoverySerialization(t *testing.T) {
	dir := t.TempDir()
	writeWALSegment(t, dir, 1, makePutRecord(1, "k1", "v1"))

	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	inFlight := make(chan struct{})
	releaseRecovery := make(chan struct{})

	resetHook := engine.SetRecoveryPrePublishHookForTesting(func(e *engine.Engine) {
		close(inFlight)
		<-releaseRecovery
	})
	defer resetHook()

	var wg sync.WaitGroup
	var primaryErr error
	var secondaryErr error
	errCh := make(chan error, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		primaryErr = eng.RecoverWAL()
		errCh <- primaryErr
	}()

	// Wait for primary to enter recovery state
	select {
	case <-inFlight:
	case err := <-errCh:
		t.Fatalf("primary recovery returned early with: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for primary recovery to begin")
	}

	// Secondary call must fail immediately with ErrRecoveryInProgress
	secondaryErr = eng.RecoverWAL()
	if !stdErrors.Is(secondaryErr, errors.ErrRecoveryInProgress) {
		t.Fatalf("expected ErrRecoveryInProgress for concurrent recovery, got: %v", secondaryErr)
	}

	// Release primary recovery
	close(releaseRecovery)
	wg.Wait()

	if primaryErr != nil {
		t.Fatalf("primary recovery failed: %v", primaryErr)
	}

	val, err := eng.Get([]byte("k1"))
	if err != nil || string(val) != "v1" {
		t.Fatalf("Get(k1) failed after recovery: %v, val: %q", err, val)
	}
}

// TestSEC_P07_01_MutationsRejectedDuringRecovery verifies that Put, Delete, and Get
// are rejected with ErrRecoveryInProgress while recovery is in flight, preventing silent state loss.
func TestSEC_P07_01_MutationsRejectedDuringRecovery(t *testing.T) {
	dir := t.TempDir()
	writeWALSegment(t, dir, 1, makePutRecord(1, "k1", "v1"))

	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	inFlight := make(chan struct{})
	releaseRecovery := make(chan struct{})

	resetHook := engine.SetRecoveryPrePublishHookForTesting(func(e *engine.Engine) {
		close(inFlight)
		<-releaseRecovery
	})
	defer resetHook()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = eng.RecoverWAL()
	}()

	select {
	case <-inFlight:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for recovery hook")
	}

	ctx := context.Background()

	// Verify Put rejected
	errPut := eng.Put(ctx, []byte("live_k"), []byte("live_v"))
	if !stdErrors.Is(errPut, errors.ErrRecoveryInProgress) {
		t.Fatalf("expected ErrRecoveryInProgress for Put during recovery, got: %v", errPut)
	}

	// Verify Delete rejected
	errDel := eng.Delete(ctx, []byte("k1"))
	if !stdErrors.Is(errDel, errors.ErrRecoveryInProgress) {
		t.Fatalf("expected ErrRecoveryInProgress for Delete during recovery, got: %v", errDel)
	}

	// Verify Get rejected
	_, errGet := eng.Get([]byte("k1"))
	if !stdErrors.Is(errGet, errors.ErrRecoveryInProgress) {
		t.Fatalf("expected ErrRecoveryInProgress for Get during recovery, got: %v", errGet)
	}

	close(releaseRecovery)
	wg.Wait()
}

// TestSEC_P07_01_RecoveryRejectedAfterPriorMutation verifies that calling RecoverWAL
// on an engine that already contains live mutated in-memory state fails with ErrRecoveryInvalidState.
func TestSEC_P07_01_RecoveryRejectedAfterPriorMutation(t *testing.T) {
	dir := t.TempDir()
	writeWALSegment(t, dir, 1, makePutRecord(1, "k1", "v1"))
	ctx := context.Background()

	// Case 1: Prior Put
	{
		eng := engine.NewEngineWithOptions(engine.EngineOptions{
			DBPath:       dir,
			Backpressure: defaultEngineCfg(),
		})

		if err := eng.Put(ctx, []byte("live_key"), []byte("live_val")); err != nil {
			t.Fatalf("Put failed: %v", err)
		}

		err := eng.RecoverWAL()
		if !stdErrors.Is(err, errors.ErrRecoveryInvalidState) {
			t.Fatalf("expected ErrRecoveryInvalidState after prior Put, got: %v", err)
		}

		// Ensure live key was NOT overwritten or corrupted
		val, err := eng.Get([]byte("live_key"))
		if err != nil || string(val) != "live_val" {
			t.Fatalf("live_key missing or corrupted: %v, val: %s", err, string(val))
		}
		_ = eng.Close()
	}

	// Case 2: Prior Delete
	{
		eng := engine.NewEngineWithOptions(engine.EngineOptions{
			DBPath:       dir,
			Backpressure: defaultEngineCfg(),
		})

		if err := eng.Delete(ctx, []byte("live_key")); err != nil {
			t.Fatalf("Delete failed: %v", err)
		}

		err := eng.RecoverWAL()
		if !stdErrors.Is(err, errors.ErrRecoveryInvalidState) {
			t.Fatalf("expected ErrRecoveryInvalidState after prior Delete, got: %v", err)
		}
		_ = eng.Close()
	}
}

// TestSEC_P07_01_RecoveryAlreadyComplete verifies that attempting a second RecoverWAL
// after a successful recovery fails deterministically with ErrRecoveryAlreadyComplete.
func TestSEC_P07_01_RecoveryAlreadyComplete(t *testing.T) {
	dir := t.TempDir()
	writeWALSegment(t, dir, 1, makePutRecord(1, "k1", "v1"))

	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	if err := eng.RecoverWAL(); err != nil {
		t.Fatalf("first RecoverWAL failed: %v", err)
	}

	// Second RecoverWAL must fail with ErrRecoveryAlreadyComplete
	err := eng.RecoverWAL()
	if !stdErrors.Is(err, errors.ErrRecoveryAlreadyComplete) {
		t.Fatalf("expected ErrRecoveryAlreadyComplete, got: %v", err)
	}
}

// TestSEC_P07_01_CloseDuringRecovery verifies that calling Close while recovery is in flight
// cleanly aborts publication without publishing recovered state into a closed engine.
func TestSEC_P07_01_CloseDuringRecovery(t *testing.T) {
	dir := t.TempDir()
	writeWALSegment(t, dir, 1, makePutRecord(1, "k1", "v1"))

	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})

	inFlight := make(chan struct{})
	closedCh := make(chan struct{})

	resetHook := engine.SetRecoveryPrePublishHookForTesting(func(e *engine.Engine) {
		close(inFlight)
		<-closedCh
	})
	defer resetHook()

	var wg sync.WaitGroup
	var recoveryErr error

	wg.Add(1)
	go func() {
		defer wg.Done()
		recoveryErr = eng.RecoverWAL()
	}()

	select {
	case <-inFlight:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for recovery pre-publish hook")
	}

	// Close engine while recovery is paused before publication
	if err := eng.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Release recovery to attempt publication
	close(closedCh)
	wg.Wait()

	// Publication must have failed with ErrWriterClosed
	if !stdErrors.Is(recoveryErr, errors.ErrWriterClosed) {
		t.Fatalf("expected ErrWriterClosed from recovery after Close, got: %v", recoveryErr)
	}

	// ActiveMemTable must be empty or engine closed
	if eng.ActiveMemTable().Len() != 0 {
		t.Fatalf("expected activeMemTable to remain empty on closed engine, got len %d", eng.ActiveMemTable().Len())
	}
}

// TestSEC_P07_01_WatermarkSemanticsPreserved explicitly verifies that the sequence watermark
// contract is preserved: nextSeqNum represents the watermark, and subsequent writes use
// nextSeqNum.Add(1) to allocate strictly monotonic sequence numbers without off-by-one regressions.
func TestSEC_P07_01_WatermarkSemanticsPreserved(t *testing.T) {
	dir := t.TempDir()
	highestSeq := uint64(42)
	writeWALSegment(t, dir, 1, makePutRecord(highestSeq, "k42", "v42"))

	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	if err := eng.RecoverWAL(); err != nil {
		t.Fatalf("RecoverWAL failed: %v", err)
	}

	// Watermark after recovery must be highestSeq (42)
	if eng.NextSeqNum() != highestSeq {
		t.Fatalf("expected NextSeqNum to be %d, got %d", highestSeq, eng.NextSeqNum())
	}

	// Write new key: must receive sequence number 43 (highestSeq + 1)
	ctx := context.Background()
	if err := eng.Put(ctx, []byte("k43"), []byte("v43")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	if eng.NextSeqNum() != 43 {
		t.Fatalf("expected NextSeqNum to advance to 43, got %d", eng.NextSeqNum())
	}

	// Verify key is queryable
	val, err := eng.Get([]byte("k43"))
	if err != nil || string(val) != "v43" {
		t.Fatalf("Get(k43) failed: %v, val: %s", err, string(val))
	}
}

// TestSEC_P07_04_VersionOwnershipInPublication verifies version reference ownership transfers:
// - When VersionSet has no Current, replay Version is installed and retained (RefCount == 1)
// - When VersionSet already has a Current, replay Version is released via Unref without leaking
// - When recovery fails, replay Version is unreferenced cleanly
func TestSEC_P07_04_VersionOwnershipInPublication(t *testing.T) {
	var emptyLevels [version.NumLevels][]version.FileMetadata

	// Case 1: Successful installation when VersionSet has no Current
	{
		vs := version.NewVersionSet()

		v := version.NewVersion(emptyLevels)
		if v.RefCount() != 1 {
			t.Fatalf("expected initial RefCount == 1, got %d", v.RefCount())
		}

		res := &version.ReplayResult{Version: v}

		dir := t.TempDir()
		writeWALSegment(t, dir, 1, makePutRecord(1, "k1", "v1"))

		eng := engine.NewEngineWithOptions(engine.EngineOptions{
			DBPath:       dir,
			Backpressure: defaultEngineCfg(),
			VersionSet:   vs,
		})
		defer func() { _ = eng.Close() }()

		if err := eng.RecoverWALWithManifestResult(res); err != nil {
			t.Fatalf("RecoverWALWithManifestResult failed: %v", err)
		}

		// VersionSet now owns the reference
		if v.RefCount() != 1 {
			t.Fatalf("expected RefCount == 1 after installation, got %d", v.RefCount())
		}

		cur := vs.Current()
		if cur == nil {
			t.Fatal("expected vs.Current() to be non-nil")
		}
		if cur.RefCount() != 2 { // 1 owned by VersionSet + 1 caller pin
			t.Fatalf("expected RefCount == 2 with caller pin, got %d", cur.RefCount())
		}
		cur.Unref()
	}

	// Case 2: When VersionSet already has a Current, replay Version is unref'd without leak
	{
		vs := version.NewVersionSet()

		initialV := version.NewVersion(emptyLevels)
		if err := vs.AppendVersion(initialV); err != nil {
			t.Fatalf("AppendVersion failed: %v", err)
		}

		replayV := version.NewVersion(emptyLevels)
		res := &version.ReplayResult{Version: replayV}

		dir := t.TempDir()
		writeWALSegment(t, dir, 1, makePutRecord(1, "k1", "v1"))

		eng := engine.NewEngineWithOptions(engine.EngineOptions{
			DBPath:       dir,
			Backpressure: defaultEngineCfg(),
			VersionSet:   vs,
		})
		defer func() { _ = eng.Close() }()

		err := eng.RecoverWALWithManifestResult(res)
		if !stdErrors.Is(err, errors.ErrRecoveryInvalidState) {
			t.Fatalf("expected ErrRecoveryInvalidState, got: %v", err)
		}

		// replayV was rejected because Current already exists; RefCount must be 0 (finalized)
		if replayV.RefCount() != 0 {
			t.Fatalf("expected replayV RefCount == 0, got %d", replayV.RefCount())
		}

		// initialV is still the current version
		cur := vs.Current()
		if cur != initialV {
			t.Fatal("expected current version to remain initialV")
		}
		cur.Unref()
	}

	// Case 3: When recovery fails before publication, replay Version is unref'd
	{
		vs := version.NewVersionSet()

		replayV := version.NewVersion(emptyLevels)
		res := &version.ReplayResult{Version: replayV}

		dir := t.TempDir()
		// Write a corrupted segment that will cause recovery failure
		writeWALSegment(t, dir, 1, makePutRecord(1, "k1", "v1"))
		writeWALSegment(t, dir, 2, makePutRecord(2, "k2", "v2"))
		corruptByteAt(t, wal.SegmentPath(dir, 1), 16, 0xFF)

		eng := engine.NewEngineWithOptions(engine.EngineOptions{
			DBPath:       dir,
			Backpressure: defaultEngineCfg(),
			VersionSet:   vs,
		})
		defer func() { _ = eng.Close() }()

		err := eng.RecoverWALWithManifestResult(res)
		if err == nil {
			t.Fatal("expected recovery failure on corrupted segment 1")
		}

		// replayV must have been unref'd on failure
		if replayV.RefCount() != 0 {
			t.Fatalf("expected replayV RefCount == 0 on failure, got %d", replayV.RefCount())
		}
	}
}

// TestSEC_P07_01_ConcurrentRecoveryStress verifies serialization under 10 concurrent
// recovery calls with no prior mutation. Exactly one must succeed and the other 9 must
// return ErrRecoveryInProgress or ErrRecoveryAlreadyComplete.
func TestSEC_P07_01_ConcurrentRecoveryStress(t *testing.T) {
	dir := t.TempDir()
	writeWALSegment(t, dir, 1, makePutRecord(1, "k1", "v1"))

	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	var recoverySuccessCount atomic.Int32
	var recoveryConflictCount atomic.Int32

	var wg sync.WaitGroup
	startBarrier := make(chan struct{})

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startBarrier
			err := eng.RecoverWAL()
			if err == nil {
				recoverySuccessCount.Add(1)
			} else if stdErrors.Is(err, errors.ErrRecoveryInProgress) || stdErrors.Is(err, errors.ErrRecoveryAlreadyComplete) {
				recoveryConflictCount.Add(1)
			}
		}()
	}

	close(startBarrier)
	wg.Wait()

	if recoverySuccessCount.Load() != 1 {
		t.Fatalf("expected exactly 1 recovery to succeed, got %d", recoverySuccessCount.Load())
	}
	if recoveryConflictCount.Load() != 9 {
		t.Fatalf("expected 9 recovery conflicts, got %d", recoveryConflictCount.Load())
	}
}

// TestSEC_P07_01_ConcurrentMutationRaceStress tests that concurrent writes racing with
// recovery never result in silent write loss or corrupt state.
func TestSEC_P07_01_ConcurrentMutationRaceStress(t *testing.T) {
	dir := t.TempDir()
	writeWALSegment(t, dir, 1, makePutRecord(1, "k1", "v1"))

	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	var wg sync.WaitGroup
	startBarrier := make(chan struct{})
	ctx := context.Background()

	var recoveryErr error
	var putSuccessCount atomic.Int32
	var putBlockedCount atomic.Int32

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-startBarrier
		recoveryErr = eng.RecoverWAL()
	}()

	for i := 0; i < 10; i++ {
		wg.Add(1)
		key := []byte("key")
		val := []byte("val")
		go func() {
			defer wg.Done()
			<-startBarrier
			err := eng.Put(ctx, key, val)
			if err == nil {
				putSuccessCount.Add(1)
			} else if stdErrors.Is(err, errors.ErrRecoveryInProgress) {
				putBlockedCount.Add(1)
			}
		}()
	}

	close(startBarrier)
	wg.Wait()

	// If a Put succeeded before recovery, recovery must fail closed with ErrRecoveryInvalidState
	// to prevent overwriting the live write.
	if putSuccessCount.Load() > 0 && recoveryErr != nil {
		if !stdErrors.Is(recoveryErr, errors.ErrRecoveryInvalidState) && !stdErrors.Is(recoveryErr, errors.ErrRecoveryInProgress) {
			t.Fatalf("unexpected recovery error when puts occurred: %v", recoveryErr)
		}
		// Confirm the write was preserved!
		val, err := eng.Get([]byte("key"))
		if err != nil || string(val) != "val" {
			t.Fatalf("Get(key) failed: %v, val: %s", err, string(val))
		}
	} else if recoveryErr == nil {
		// Recovery succeeded: check that recovered key is present
		val, err := eng.Get([]byte("k1"))
		if err != nil || string(val) != "v1" {
			t.Fatalf("Get(k1) failed: %v, val: %s", err, string(val))
		}
	}
}
