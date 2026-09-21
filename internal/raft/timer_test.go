package raft_test

import (
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/raft"
)

func TestDefaultDurationProvider_BoundsAndDistribution(t *testing.T) {
	// Section 13, 15: 150ms <= election timeout <= 300ms
	const samples = 10000
	var (
		minObserved = time.Duration(1<<63 - 1)
		maxObserved = time.Duration(0)
	)

	// Bucket counts for uniform distribution sanity
	const numBuckets = 5
	bucketSpan := (raft.DefaultMaxElectionTimeout - raft.DefaultMinElectionTimeout) / numBuckets
	buckets := make([]int, numBuckets)

	for i := 0; i < samples; i++ {
		d := raft.DefaultDurationProvider()
		if d < raft.DefaultMinElectionTimeout {
			t.Fatalf("sample %d produced %v < min %v", i, d, raft.DefaultMinElectionTimeout)
		}
		if d > raft.DefaultMaxElectionTimeout {
			t.Fatalf("sample %d produced %v > max %v", i, d, raft.DefaultMaxElectionTimeout)
		}
		if d < minObserved {
			minObserved = d
		}
		if d > maxObserved {
			maxObserved = d
		}

		offset := d - raft.DefaultMinElectionTimeout
		bIdx := int(offset / bucketSpan)
		if bIdx >= numBuckets {
			bIdx = numBuckets - 1
		}
		buckets[bIdx]++
	}

	// Verify reasonable variation across the entire interval
	for i, count := range buckets {
		if count < (samples/numBuckets)/3 {
			t.Errorf("bucket %d count %d suspiciously low out of %d samples", i, count, samples)
		}
	}
}

func TestElectionTimer_LifecycleAndGenerations(t *testing.T) {
	// Section 16, 45: Resettable timer with generation tracking
	timer := raft.NewElectionTimer(func() time.Duration {
		return 10 * time.Millisecond
	})
	defer timer.Close()

	if !timer.IsStopped() {
		t.Fatalf("expected timer to be initially stopped")
	}

	// 1. Reset arms the timer and increments generation
	timer.Reset()
	gen1 := timer.CurrentGen()
	if gen1 != 1 {
		t.Fatalf("expected generation 1, got %d", gen1)
	}
	if timer.IsStopped() {
		t.Fatalf("timer should not be stopped after Reset")
	}

	// Wait for expiration
	select {
	case gen := <-timer.C():
		if gen != gen1 {
			t.Fatalf("expected generation %d from channel, got %d", gen1, gen)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("timed out waiting for timer expiration")
	}

	// 2. Stop halts the timer and increments generation
	timer.Stop()
	if !timer.IsStopped() {
		t.Fatalf("expected timer to be stopped after Stop()")
	}
	gen2 := timer.CurrentGen()
	if gen2 <= gen1 {
		t.Fatalf("expected generation to increment on Stop(): gen2=%d, gen1=%d", gen2, gen1)
	}

	// Ensure no ticks arrive after Stop
	select {
	case gen := <-timer.C():
		t.Fatalf("unexpected tick received after Stop(): gen=%d", gen)
	case <-time.After(30 * time.Millisecond):
		// Expected
	}

	// 3. Restart / Reset after Stop
	timer.Reset()
	gen3 := timer.CurrentGen()
	if gen3 <= gen2 {
		t.Fatalf("expected generation to increment on Reset(): gen3=%d, gen2=%d", gen3, gen2)
	}

	select {
	case gen := <-timer.C():
		if gen != gen3 {
			t.Fatalf("expected generation %d, got %d", gen3, gen)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("timed out waiting for restarted timer expiration")
	}

	// 4. Stale timer cancellation on rapid resets
	for i := 0; i < 10; i++ {
		timer.Reset()
	}
	lastGen := timer.CurrentGen()

	// Wait and verify only the final generation arrives
	select {
	case gen := <-timer.C():
		if gen != lastGen {
			t.Fatalf("expected only the latest generation %d, got stale %d", lastGen, gen)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("timed out waiting for final expiration")
	}

	// Channel should be empty now
	select {
	case gen := <-timer.C():
		t.Fatalf("duplicate tick received: gen=%d", gen)
	default:
		// Expected
	}
}

func TestElectionTimer_CloseIdempotentAndStopsTicks(t *testing.T) {
	timer := raft.NewElectionTimer(func() time.Duration {
		return 5 * time.Millisecond
	})

	timer.Reset()
	timer.Close()
	// Idempotent Close
	timer.Close()

	if !timer.IsStopped() {
		t.Fatalf("expected timer to be stopped/closed")
	}

	// Reset after close must be ignored
	timer.Reset()

	select {
	case gen := <-timer.C():
		t.Fatalf("received tick after Close(): gen=%d", gen)
	case <-time.After(30 * time.Millisecond):
		// Expected
	}
}

func TestElectionTimer_ConcurrentResetAndStop(t *testing.T) {
	// Concurrency race test across Reset, Stop, and Close
	timer := raft.NewElectionTimer(func() time.Duration {
		return 2 * time.Millisecond
	})
	defer timer.Close()

	var wg sync.WaitGroup
	const workers = 8
	stopCh := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					if id%2 == 0 {
						timer.Reset()
					} else {
						timer.Stop()
					}
					_ = timer.CurrentGen()
					_ = timer.IsStopped()
				}
			}
		}(i)
	}

	// Drainer goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopCh:
				return
			case <-timer.C():
			}
		}
	}()

	time.Sleep(100 * time.Millisecond)
	close(stopCh)
	wg.Wait()
}
