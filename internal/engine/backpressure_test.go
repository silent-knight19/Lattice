package engine_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/engine"
	domainErrors "github.com/silent-knight19/lattice/internal/errors"
)

func TestBackpressure_BasicLifecycle(t *testing.T) {
	cfg := engine.BackpressureConfig{
		MaxMemoryBytes: 10 * 1024, // 10 KiB
		HighWatermark:  0.80,      // 8 KiB
		HardWatermark:  0.90,      // 9 KiB
		MaxWaitTimeout: 100 * time.Millisecond,
	}

	bc := engine.NewBackpressureController(cfg)
	defer bc.Close()

	ctx := context.Background()

	// 1. Acquire below HighWatermark (e.g. 5 KiB)
	if err := bc.Acquire(ctx, 5*1024); err != nil {
		t.Fatalf("unexpected acquire error: %v", err)
	}
	if bc.IsThrottled() {
		t.Fatalf("expected IsThrottled == false at 5 KiB")
	}

	// 2. Acquire into HighWatermark zone (e.g. +3.5 KiB -> 8.5 KiB, which is > 8.0 KiB but < 9.0 KiB)
	if err := bc.Acquire(ctx, 3500); err != nil {
		t.Fatalf("unexpected acquire error in throttle zone: %v", err)
	}
	if !bc.IsThrottled() {
		t.Fatalf("expected IsThrottled == true at 8.5 KiB")
	}

	// 3. Attempting to acquire more than remaining hard capacity must fail fail-closed
	// Total hard ceiling is 9216 bytes. Current is 8620 bytes. Asking for 2000 bytes exceeds hard ceiling.
	err := bc.Acquire(ctx, 2000)
	if !errors.Is(err, domainErrors.ErrMemoryLimitExceeded) {
		t.Fatalf("expected ErrMemoryLimitExceeded, got: %v", err)
	}

	// 4. Release memory and verify acquire succeeds again
	bc.Release(4000)
	if bc.IsThrottled() {
		t.Fatalf("expected IsThrottled == false after releasing 4 KiB")
	}

	if err := bc.Acquire(ctx, 2000); err != nil {
		t.Fatalf("expected acquire to succeed after release, got: %v", err)
	}
}

func TestBackpressure_ConcurrentWritersWithBlocking(t *testing.T) {
	cfg := engine.BackpressureConfig{
		MaxMemoryBytes: 20 * 1024, // 20 KiB
		HighWatermark:  0.70,
		HardWatermark:  0.85, // 17 KiB
		MaxWaitTimeout: 2 * time.Second,
	}

	bc := engine.NewBackpressureController(cfg)
	defer bc.Close()

	ctx := context.Background()

	// Fill up to hard capacity
	if err := bc.Acquire(ctx, 16*1024); err != nil {
		t.Fatalf("initial fill failed: %v", err)
	}

	var wg sync.WaitGroup
	var writerBlocked sync.WaitGroup
	writerBlocked.Add(1)
	writerFinished := make(chan error, 1)

	// Background writer attempts to acquire 3 KiB (16 + 3 = 19 KiB > 17 KiB hard ceiling)
	wg.Add(1)
	go func() {
		defer wg.Done()
		writerBlocked.Done()
		err := bc.Acquire(ctx, 3*1024)
		writerFinished <- err
	}()

	writerBlocked.Wait()
	// Give background writer time to block on condition
	time.Sleep(50 * time.Millisecond)

	// Release 5 KiB from another thread
	bc.Release(5 * 1024)

	select {
	case err := <-writerFinished:
		if err != nil {
			t.Fatalf("blocked writer failed to resume after release: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("timed out waiting for blocked writer to resume")
	}

	wg.Wait()
}

func TestEngine_PutGetBackpressureEnforcement(t *testing.T) {
	cfg := engine.BackpressureConfig{
		MaxMemoryBytes: 15 * 1024, // 15 KiB
		HighWatermark:  0.80,
		HardWatermark:  0.90,
		MaxWaitTimeout: 50 * time.Millisecond,
	}

	eng := engine.NewEngine(cfg)
	defer func() { _ = eng.Close() }()

	ctx := context.Background()

	// Write items until backpressure triggers
	var count int
	var backpressureHit bool

	for i := 0; i < 500; i++ {
		k := []byte(fmt.Sprintf("user-key-%05d", i))
		v := make([]byte, 128)
		err := eng.Put(ctx, k, v)
		if err != nil {
			if errors.Is(err, domainErrors.ErrMemoryLimitExceeded) {
				backpressureHit = true
				break
			}
			t.Fatalf("unexpected put error at index %d: %v", i, err)
		}
		count++
	}

	if !backpressureHit {
		t.Fatalf("expected memory limit backpressure to be hit, but all writes passed (count=%d)", count)
	}

	// Verify existing written keys are retrievable
	val, err := eng.Get([]byte("user-key-00000"))
	if err != nil {
		t.Fatalf("failed to retrieve key-0: %v", err)
	}
	if len(val) != 128 {
		t.Fatalf("expected 128 bytes, got %d", len(val))
	}
}

func TestBackpressure_NoGoroutineLeakOnTimeout(t *testing.T) {
	cfg := engine.BackpressureConfig{
		MaxMemoryBytes: 10 * 1024, // 10 KiB
		HighWatermark:  0.80,
		HardWatermark:  0.90, // 9 KiB
		MaxWaitTimeout: 20 * time.Millisecond,
	}

	bc := engine.NewBackpressureController(cfg)
	defer bc.Close()

	ctx := context.Background()

	// Fill to hard capacity
	if err := bc.Acquire(ctx, 9*1024); err != nil {
		t.Fatalf("failed to acquire initial bytes: %v", err)
	}

	// Launch 20 concurrent writers that will all exceed capacity and time out
	var wg sync.WaitGroup
	timeoutErrors := make([]error, 20)

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			timeoutErrors[idx] = bc.Acquire(ctx, 1024)
		}(i)
	}

	wg.Wait()

	for i, err := range timeoutErrors {
		if !errors.Is(err, domainErrors.ErrMemoryLimitExceeded) {
			t.Fatalf("writer %d expected ErrMemoryLimitExceeded, got: %v", i, err)
		}
	}

	// Verify that release subsequently works without deadlocks
	bc.Release(5 * 1024)
	if err := bc.Acquire(ctx, 2*1024); err != nil {
		t.Fatalf("failed to acquire after release: %v", err)
	}
}

func TestBackpressure_TimerResourceManagementAndOversizedRejection(t *testing.T) {
	cfg := engine.BackpressureConfig{
		MaxMemoryBytes: 10 * 1024,
		HighWatermark:  0.80,
		HardWatermark:  0.90,
		MaxWaitTimeout: 50 * time.Millisecond,
	}
	bc := engine.NewBackpressureController(cfg)
	defer bc.Close()

	ctx := context.Background()

	// 1. Verify oversized request exceeds MaxMemoryBytes rejected immediately (LAT-003)
	if err := bc.Acquire(ctx, 20*1024); !errors.Is(err, domainErrors.ErrMemoryLimitExceeded) {
		t.Fatalf("expected ErrMemoryLimitExceeded for > MaxMemoryBytes, got: %v", err)
	}

	// 2. Fill to hard capacity
	if err := bc.Acquire(ctx, 9*1024); err != nil {
		t.Fatalf("failed initial acquire: %v", err)
	}

	// 3. Test context cancellation while blocked on timer (LAT-001 cleanup)
	cancelCtx, cancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- bc.Acquire(cancelCtx, 512)
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for cancelled Acquire")
	}

	// 4. Release and verify subsequent acquire succeeds smoothly
	bc.Release(5 * 1024)
	if err := bc.Acquire(ctx, 1024); err != nil {
		t.Fatalf("expected acquire after release to succeed: %v", err)
	}
}
