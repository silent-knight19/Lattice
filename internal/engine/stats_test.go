package engine_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
)

// -----------------------------------------------------------------------------
// 1. EMPTY ENGINE STATS
// -----------------------------------------------------------------------------

func TestEngine_Stats_EmptyEngine(t *testing.T) {
	eng, _ := newFlushEngine(t, 0)
	defer func() { _ = eng.Close() }()

	engStats, memStats, storStats, cacheStats, err := eng.Stats()
	if err != nil {
		t.Fatalf("expected Stats() to succeed on empty engine, got: %v", err)
	}

	if engStats.State != "open" {
		t.Errorf("expected engine state 'open', got %q", engStats.State)
	}
	if engStats.SequenceNumber != 0 {
		t.Errorf("expected sequence number 0 on empty engine, got %d", engStats.SequenceNumber)
	}
	if memStats.ActiveMemTableEntries != 0 {
		t.Errorf("expected 0 active memtable entries, got %d", memStats.ActiveMemTableEntries)
	}
	if memStats.ImmutableMemTableCount != 0 {
		t.Errorf("expected 0 immutable memtables, got %d", memStats.ImmutableMemTableCount)
	}
	if storStats.L0Files != 0 {
		t.Errorf("expected 0 L0 files, got %d", storStats.L0Files)
	}
	if storStats.TotalSSTableFiles != 0 {
		t.Errorf("expected 0 SSTable files, got %d", storStats.TotalSSTableFiles)
	}
	if cacheStats.Hits != 0 || cacheStats.Misses != 0 {
		t.Errorf("expected 0 cache hits/misses, got hits=%d misses=%d", cacheStats.Hits, cacheStats.Misses)
	}
}

// -----------------------------------------------------------------------------
// 2. ZERO MUTATION PROPERTY
// -----------------------------------------------------------------------------

func TestEngine_Stats_ZeroMutationProperty(t *testing.T) {
	eng, _ := newFlushEngine(t, 0)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	// Populate some state
	for i := 0; i < 10; i++ {
		key := []byte(fmt.Sprintf("zm-key-%03d", i))
		val := []byte(fmt.Sprintf("zm-val-%03d", i))
		if err := eng.Put(ctx, key, val); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
	}

	// Capture initial stats
	eng1, mem1, stor1, cache1, err := eng.Stats()
	if err != nil {
		t.Fatalf("Stats 1 failed: %v", err)
	}

	// Invoke Stats repeatedly
	for i := 0; i < 50; i++ {
		engN, memN, storN, cacheN, err := eng.Stats()
		if err != nil {
			t.Fatalf("Stats %d failed: %v", i, err)
		}
		if engN.SequenceNumber != eng1.SequenceNumber {
			t.Fatalf("iteration %d: sequence number mutated: %d != %d", i, engN.SequenceNumber, eng1.SequenceNumber)
		}
		if memN.ActiveMemTableEntries != mem1.ActiveMemTableEntries {
			t.Fatalf("iteration %d: active memtable entries mutated: %d != %d", i, memN.ActiveMemTableEntries, mem1.ActiveMemTableEntries)
		}
		if memN.ActiveMemTableBytes != mem1.ActiveMemTableBytes {
			t.Fatalf("iteration %d: active memtable bytes mutated: %d != %d", i, memN.ActiveMemTableBytes, mem1.ActiveMemTableBytes)
		}
		if storN.TotalSSTableFiles != stor1.TotalSSTableFiles {
			t.Fatalf("iteration %d: total sstables mutated: %d != %d", i, storN.TotalSSTableFiles, stor1.TotalSSTableFiles)
		}
		if cacheN.Hits != cache1.Hits || cacheN.Misses != cache1.Misses {
			t.Fatalf("iteration %d: cache stats mutated", i)
		}
	}
}

// -----------------------------------------------------------------------------
// 3. STATS ACCURATELY REFLECTS RUNTIME MUTATIONS
// -----------------------------------------------------------------------------

func TestEngine_Stats_ReflectsMutations(t *testing.T) {
	eng, _ := newFlushEngine(t, 0)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	// 1. Initial
	eng0, mem0, _, _, err := eng.Stats()
	if err != nil {
		t.Fatalf("Stats failed: %v", err)
	}
	if mem0.ActiveMemTableEntries != 0 {
		t.Fatalf("expected 0 entries initially, got %d", mem0.ActiveMemTableEntries)
	}

	// 2. PUT
	if err := eng.Put(ctx, []byte("key1"), []byte("value1")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	eng1, mem1, _, _, err := eng.Stats()
	if err != nil {
		t.Fatalf("Stats failed: %v", err)
	}
	if eng1.SequenceNumber <= eng0.SequenceNumber {
		t.Errorf("expected sequence number to increase after PUT: %d <= %d", eng1.SequenceNumber, eng0.SequenceNumber)
	}
	if mem1.ActiveMemTableEntries != 1 {
		t.Errorf("expected 1 active memtable entry, got %d", mem1.ActiveMemTableEntries)
	}
	if mem1.ActiveMemTableBytes <= mem0.ActiveMemTableBytes {
		t.Errorf("expected active memtable bytes to increase")
	}

	// 3. DELETE (writes a tombstone to memtable, which increments seq and entry count)
	if err := eng.Delete(ctx, []byte("key1")); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	eng2, mem2, _, _, err := eng.Stats()
	if err != nil {
		t.Fatalf("Stats failed: %v", err)
	}
	if eng2.SequenceNumber <= eng1.SequenceNumber {
		t.Errorf("expected sequence number to increase after DELETE: %d <= %d", eng2.SequenceNumber, eng1.SequenceNumber)
	}
	if mem2.ActiveMemTableEntries != 2 {
		t.Errorf("expected 2 active memtable entries (including tombstone), got %d", mem2.ActiveMemTableEntries)
	}

	// 4. BATCH
	batchOps := []binary.BatchOp{
		{Type: binary.OpTypePut, Key: []byte("bk1"), Value: []byte("bv1")},
		{Type: binary.OpTypePut, Key: []byte("bk2"), Value: []byte("bv2")},
		{Type: binary.OpTypeDelete, Key: []byte("bk3")},
	}
	if err := eng.Batch(ctx, batchOps); err != nil {
		t.Fatalf("Batch failed: %v", err)
	}
	eng3, mem3, _, _, err := eng.Stats()
	if err != nil {
		t.Fatalf("Stats failed: %v", err)
	}
	if eng3.SequenceNumber <= eng2.SequenceNumber {
		t.Errorf("expected sequence number to increase after BATCH: %d <= %d", eng3.SequenceNumber, eng2.SequenceNumber)
	}
	if mem3.ActiveMemTableEntries != 5 { // 2 prior + 3 batch ops
		t.Errorf("expected 5 active memtable entries, got %d", mem3.ActiveMemTableEntries)
	}
}

// -----------------------------------------------------------------------------
// 4. STATS REFLECTS FLUSH & SSTABLE METRICS
// -----------------------------------------------------------------------------

func TestEngine_Stats_ReflectsFlush(t *testing.T) {
	// threshold = 100 bytes to trigger auto-flush
	eng, _ := newFlushEngine(t, 100)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	// Write enough data to trigger flush
	for i := 0; i < 20; i++ {
		k := []byte(fmt.Sprintf("flush-key-%04d", i))
		v := []byte(fmt.Sprintf("flush-val-%04d-padding-data-here", i))
		if err := eng.Put(ctx, k, v); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
	}

	waitFlushEmpty(t, eng, 5000)

	_, _, stor, _, err := eng.Stats()
	if err != nil {
		t.Fatalf("Stats failed: %v", err)
	}

	if stor.FlushesCompleted == 0 {
		t.Errorf("expected FlushesCompleted > 0, got %d", stor.FlushesCompleted)
	}
	if stor.L0Files == 0 {
		t.Errorf("expected L0Files > 0 after flush, got %d", stor.L0Files)
	}
	if stor.TotalSSTableFiles == 0 {
		t.Errorf("expected TotalSSTableFiles > 0 after flush, got %d", stor.TotalSSTableFiles)
	}
	if stor.TotalSSTableBytes == 0 {
		t.Errorf("expected TotalSSTableBytes > 0 after flush, got %d", stor.TotalSSTableBytes)
	}
}

// -----------------------------------------------------------------------------
// 5. STATS AFTER CLOSE
// -----------------------------------------------------------------------------

func TestEngine_Stats_AfterClose(t *testing.T) {
	eng, _ := newFlushEngine(t, 0)
	if err := eng.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	engStats, _, _, _, err := eng.Stats()
	if err != nil {
		t.Fatalf("Stats failed after close: %v", err)
	}
	if engStats.State != "closed" {
		t.Errorf("expected engine state 'closed' after Close(), got %q", engStats.State)
	}
}

// -----------------------------------------------------------------------------
// 6. CONCURRENT STATS AND MUTATIONS RACE DETECTOR
// -----------------------------------------------------------------------------

func TestEngine_Stats_ConcurrentOperations(t *testing.T) {
	eng, _ := newFlushEngine(t, 0)
	defer func() { _ = eng.Close() }()

	var wg sync.WaitGroup
	stopCh := make(chan struct{})

	// Writers
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			ctx := context.Background()
			iter := 0
			for {
				select {
				case <-stopCh:
					return
				default:
					k := []byte(fmt.Sprintf("w%d-k%d", workerID, iter))
					v := []byte(fmt.Sprintf("w%d-v%d", workerID, iter))
					_ = eng.Put(ctx, k, v)
					iter++
				}
			}
		}(i)
	}

	// Readers (Stats & Exists & Get)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					_, _, _, _, err := eng.Stats()
					if err != nil {
						t.Errorf("concurrent Stats() failed: %v", err)
					}
					_, _ = eng.Exists([]byte("w0-k0"))
					_, _ = eng.Get([]byte("w0-k0"))
				}
			}
		}(i)
	}

	time.Sleep(300 * time.Millisecond)
	close(stopCh)
	wg.Wait()
}
