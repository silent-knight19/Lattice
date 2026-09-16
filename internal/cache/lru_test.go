package cache_test

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cache"
	lerrors "github.com/silent-knight19/lattice/internal/errors"
)

// TestLRUShard_Constructor_Validation verifies capacity boundary contracts.
func TestLRUShard_Constructor_Validation(t *testing.T) {
	t.Run("NegativeCapacityRejected", func(t *testing.T) {
		shard, err := cache.NewLRUShard(-1)
		if shard != nil {
			t.Fatalf("expected nil shard on negative capacity, got %v", shard)
		}
		if err == nil {
			t.Fatalf("expected error on negative capacity, got nil")
		}
		if !errors.Is(err, lerrors.ErrInvalidCacheCapacity) {
			t.Fatalf("expected ErrInvalidCacheCapacity, got %v", err)
		}
		var typedErr *lerrors.InvalidCacheCapacityError
		if !errors.As(err, &typedErr) {
			t.Fatalf("expected *InvalidCacheCapacityError, got %T", err)
		}
		if typedErr.Capacity != -1 {
			t.Fatalf("expected Capacity=-1, got %d", typedErr.Capacity)
		}
	})

	t.Run("ZeroCapacityAllowed", func(t *testing.T) {
		shard, err := cache.NewLRUShard(0)
		if err != nil {
			t.Fatalf("unexpected error for capacity=0: %v", err)
		}
		if shard == nil {
			t.Fatalf("expected non-nil shard for capacity=0")
		}
		if shard.Capacity() != 0 {
			t.Fatalf("expected Capacity=0, got %d", shard.Capacity())
		}
		if err := shard.CheckInvariants(); err != nil {
			t.Fatalf("invariant violation: %v", err)
		}
	})

	t.Run("PositiveCapacity", func(t *testing.T) {
		shard, err := cache.NewLRUShard(64)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if shard.Capacity() != 64 {
			t.Fatalf("expected Capacity=64, got %d", shard.Capacity())
		}
		if shard.Len() != 0 {
			t.Fatalf("expected initial Len=0, got %d", shard.Len())
		}
		if err := shard.CheckInvariants(); err != nil {
			t.Fatalf("invariant violation: %v", err)
		}
	})
}

// TestLRUShard_EmptyShard verifies operations on an empty shard.
func TestLRUShard_EmptyShard(t *testing.T) {
	shard, err := cache.NewLRUShard(10)
	if err != nil {
		t.Fatalf("failed to create shard: %v", err)
	}

	key := cache.NewBlockKey(1, 0)

	val, found := shard.Get(key)
	if found || val != nil {
		t.Fatalf("expected miss on empty shard, got found=%v, val=%v", found, val)
	}

	val, found = shard.Peek(key)
	if found || val != nil {
		t.Fatalf("expected miss on Peek on empty shard, got found=%v, val=%v", found, val)
	}

	if shard.Contains(key) {
		t.Fatalf("expected Contains=false on empty shard")
	}

	if shard.Remove(key) {
		t.Fatalf("expected Remove=false on empty shard")
	}

	if shard.Len() != 0 {
		t.Fatalf("expected Len=0, got %d", shard.Len())
	}

	if err := shard.CheckInvariants(); err != nil {
		t.Fatalf("invariant violation: %v", err)
	}
}

// TestLRUShard_CapacityZero verifies the non-retaining semantics of capacity=0.
func TestLRUShard_CapacityZero(t *testing.T) {
	shard, err := cache.NewLRUShard(0)
	if err != nil {
		t.Fatalf("failed to create shard: %v", err)
	}

	key := cache.NewBlockKey(1, 4096)
	shard.Put(key, []byte("data"))

	if shard.Len() != 0 {
		t.Fatalf("expected Len=0 after Put in zero-capacity shard, got %d", shard.Len())
	}

	val, found := shard.Get(key)
	if found || val != nil {
		t.Fatalf("expected miss on zero-capacity shard, got found=%v", found)
	}

	if shard.Contains(key) {
		t.Fatalf("expected Contains=false on zero-capacity shard")
	}

	if err := shard.CheckInvariants(); err != nil {
		t.Fatalf("invariant violation: %v", err)
	}
}

// TestLRUShard_SingleEntry verifies single-node list transitions.
func TestLRUShard_SingleEntry(t *testing.T) {
	shard, err := cache.NewLRUShard(1)
	if err != nil {
		t.Fatalf("failed to create shard: %v", err)
	}

	keyA := cache.NewBlockKey(1, 100)
	valA := []byte("blockA")

	shard.Put(keyA, valA)

	if shard.Len() != 1 {
		t.Fatalf("expected Len=1, got %d", shard.Len())
	}

	got, found := shard.Get(keyA)
	if !found {
		t.Fatalf("expected keyA to be found")
	}
	if !bytes.Equal(got, valA) {
		t.Fatalf("got %q, want %q", got, valA)
	}

	if err := shard.CheckInvariants(); err != nil {
		t.Fatalf("invariant violation: %v", err)
	}

	// Put keyB should evict keyA immediately under capacity=1
	keyB := cache.NewBlockKey(1, 200)
	valB := []byte("blockB")
	shard.Put(keyB, valB)

	if shard.Len() != 1 {
		t.Fatalf("expected Len=1 after eviction, got %d", shard.Len())
	}

	if shard.Contains(keyA) {
		t.Fatalf("expected keyA to be evicted")
	}

	gotB, found := shard.Get(keyB)
	if !found || !bytes.Equal(gotB, valB) {
		t.Fatalf("expected keyB present with correct value")
	}

	if err := shard.CheckInvariants(); err != nil {
		t.Fatalf("invariant violation: %v", err)
	}
}

// TestLRUShard_TwoEntries_Recency verifies MRU/LRU transitions with two entries.
func TestLRUShard_TwoEntries_Recency(t *testing.T) {
	shard, err := cache.NewLRUShard(2)
	if err != nil {
		t.Fatalf("failed to create shard: %v", err)
	}

	keyA := cache.NewBlockKey(10, 0)
	keyB := cache.NewBlockKey(10, 4096)

	shard.Put(keyA, []byte("A"))
	shard.Put(keyB, []byte("B"))

	// Expect MRU: B, LRU: A
	keys := shard.KeysMRUtoLRU()
	if len(keys) != 2 || keys[0] != keyB || keys[1] != keyA {
		t.Fatalf("expected MRU [B, A], got %v", keys)
	}

	// Access A via Get -> A becomes MRU
	valA, found := shard.Get(keyA)
	if !found || !bytes.Equal(valA, []byte("A")) {
		t.Fatalf("failed to Get A: found=%v, val=%s", found, valA)
	}

	keys = shard.KeysMRUtoLRU()
	if len(keys) != 2 || keys[0] != keyA || keys[1] != keyB {
		t.Fatalf("expected MRU [A, B] after Get(A), got %v", keys)
	}

	// Put C -> should evict B (LRU)
	keyC := cache.NewBlockKey(10, 8192)
	shard.Put(keyC, []byte("C"))

	if shard.Contains(keyB) {
		t.Fatalf("expected keyB to be evicted")
	}
	if !shard.Contains(keyA) || !shard.Contains(keyC) {
		t.Fatalf("expected keyA and keyC to be retained")
	}

	keys = shard.KeysMRUtoLRU()
	if len(keys) != 2 || keys[0] != keyC || keys[1] != keyA {
		t.Fatalf("expected MRU [C, A], got %v", keys)
	}

	if err := shard.CheckInvariants(); err != nil {
		t.Fatalf("invariant violation: %v", err)
	}
}

// TestLRUShard_Capacity50_Acceptance verifies the explicit roadmap target:
// Insert 100 blocks into capacity-50 shard, verify first 50 evicted in order.
func TestLRUShard_Capacity50_Acceptance(t *testing.T) {
	const capacity = 50
	const totalBlocks = 100

	shard, err := cache.NewLRUShard(capacity)
	if err != nil {
		t.Fatalf("failed to create shard: %v", err)
	}

	// Insert blocks 0..99 sequentially
	for i := 0; i < totalBlocks; i++ {
		key := cache.NewBlockKey(1, uint64(i*4096))
		val := []byte(fmt.Sprintf("block-%04d", i))
		shard.Put(key, val)

		if err := shard.CheckInvariants(); err != nil {
			t.Fatalf("invariant broken at step %d: %v", i, err)
		}
	}

	if shard.Len() != capacity {
		t.Fatalf("expected Len=%d, got %d", capacity, shard.Len())
	}

	// Blocks 0..49 must be completely evicted
	for i := 0; i < 50; i++ {
		key := cache.NewBlockKey(1, uint64(i*4096))
		if shard.Contains(key) {
			t.Fatalf("block %d should have been evicted", i)
		}
		val, found := shard.Get(key)
		if found || val != nil {
			t.Fatalf("block %d returned data despite eviction", i)
		}
	}

	// Blocks 50..99 must all be retained with correct values
	for i := 50; i < totalBlocks; i++ {
		key := cache.NewBlockKey(1, uint64(i*4096))
		expectedVal := []byte(fmt.Sprintf("block-%04d", i))

		val, found := shard.Get(key)
		if !found {
			t.Fatalf("block %d should be retained", i)
		}
		if !bytes.Equal(val, expectedVal) {
			t.Fatalf("block %d value mismatch: got %q, want %q", i, val, expectedVal)
		}
	}

	if err := shard.CheckInvariants(); err != nil {
		t.Fatalf("final invariant violation: %v", err)
	}
}

// TestLRUShard_GetPromotesMRU verifies that Get promotes accessed keys to MRU.
func TestLRUShard_GetPromotesMRU(t *testing.T) {
	shard, err := cache.NewLRUShard(3)
	if err != nil {
		t.Fatalf("failed to create shard: %v", err)
	}

	kA := cache.NewBlockKey(1, 100)
	kB := cache.NewBlockKey(1, 200)
	kC := cache.NewBlockKey(1, 300)

	shard.Put(kA, []byte("A"))
	shard.Put(kB, []byte("B"))
	shard.Put(kC, []byte("C"))

	// Initial order: C (MRU), B, A (LRU)
	keys := shard.KeysMRUtoLRU()
	expected := []cache.BlockKey{kC, kB, kA}
	for i, k := range keys {
		if k != expected[i] {
			t.Fatalf("initial order mismatch at %d: got %v, want %v", i, k, expected[i])
		}
	}

	// Get(kA) -> order becomes: A (MRU), C, B (LRU)
	if _, ok := shard.Get(kA); !ok {
		t.Fatalf("failed to Get kA")
	}

	keys = shard.KeysMRUtoLRU()
	expected = []cache.BlockKey{kA, kC, kB}
	for i, k := range keys {
		if k != expected[i] {
			t.Fatalf("order mismatch after Get(kA) at %d: got %v, want %v", i, k, expected[i])
		}
	}

	// Put kD -> kB should be evicted
	kD := cache.NewBlockKey(1, 400)
	shard.Put(kD, []byte("D"))

	if shard.Contains(kB) {
		t.Fatalf("expected kB to be evicted")
	}
	if !shard.Contains(kA) || !shard.Contains(kC) || !shard.Contains(kD) {
		t.Fatalf("expected kA, kC, kD retained")
	}

	keys = shard.KeysMRUtoLRU()
	expected = []cache.BlockKey{kD, kA, kC}
	for i, k := range keys {
		if k != expected[i] {
			t.Fatalf("order mismatch after Put(kD) at %d: got %v, want %v", i, k, expected[i])
		}
	}

	if err := shard.CheckInvariants(); err != nil {
		t.Fatalf("invariant violation: %v", err)
	}
}

// TestLRUShard_MissDoesNotAffectOrdering verifies that a cache miss does not modify LRU ordering.
func TestLRUShard_MissDoesNotAffectOrdering(t *testing.T) {
	shard, err := cache.NewLRUShard(3)
	if err != nil {
		t.Fatalf("failed to create shard: %v", err)
	}

	kA := cache.NewBlockKey(1, 100)
	kB := cache.NewBlockKey(1, 200)

	shard.Put(kA, []byte("A"))
	shard.Put(kB, []byte("B"))

	before := shard.KeysMRUtoLRU()

	// Query absent key
	kMissing := cache.NewBlockKey(1, 9999)
	val, found := shard.Get(kMissing)
	if found || val != nil {
		t.Fatalf("expected miss on absent key")
	}

	after := shard.KeysMRUtoLRU()

	if len(before) != len(after) {
		t.Fatalf("length changed after miss: before=%d, after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("order changed after miss at %d: %v != %v", i, before[i], after[i])
		}
	}

	if err := shard.CheckInvariants(); err != nil {
		t.Fatalf("invariant violation: %v", err)
	}
}

// TestLRUShard_PutExistingKey verifies replacement and MRU promotion for existing keys.
func TestLRUShard_PutExistingKey(t *testing.T) {
	shard, err := cache.NewLRUShard(3)
	if err != nil {
		t.Fatalf("failed to create shard: %v", err)
	}

	kA := cache.NewBlockKey(1, 100)
	kB := cache.NewBlockKey(1, 200)
	kC := cache.NewBlockKey(1, 300)

	shard.Put(kA, []byte("A_v1"))
	shard.Put(kB, []byte("B_v1"))
	shard.Put(kC, []byte("C_v1"))

	// Replace kB with new value
	shard.Put(kB, []byte("B_v2"))

	if shard.Len() != 3 {
		t.Fatalf("expected Len=3 after replacing existing key, got %d", shard.Len())
	}

	// kB should now be MRU, kA is LRU
	keys := shard.KeysMRUtoLRU()
	expected := []cache.BlockKey{kB, kC, kA}
	for i, k := range keys {
		if k != expected[i] {
			t.Fatalf("order mismatch after update at %d: got %v, want %v", i, k, expected[i])
		}
	}

	// Verify updated value
	valB, found := shard.Get(kB)
	if !found || !bytes.Equal(valB, []byte("B_v2")) {
		t.Fatalf("expected updated value B_v2, got %s", valB)
	}

	// Put kD -> kA (LRU) must be evicted, not kB
	kD := cache.NewBlockKey(1, 400)
	shard.Put(kD, []byte("D"))

	if shard.Contains(kA) {
		t.Fatalf("expected kA to be evicted")
	}
	if !shard.Contains(kB) {
		t.Fatalf("expected updated kB to be retained")
	}

	if err := shard.CheckInvariants(); err != nil {
		t.Fatalf("invariant violation: %v", err)
	}
}

// TestLRUShard_MutableInputBufferIsolation verifies Section 37:
// Mutating the slice passed to Put does not corrupt the cached data.
func TestLRUShard_MutableInputBufferIsolation(t *testing.T) {
	shard, err := cache.NewLRUShard(10)
	if err != nil {
		t.Fatalf("failed to create shard: %v", err)
	}

	key := cache.NewBlockKey(1, 100)
	buf := []byte("original_value")

	shard.Put(key, buf)

	// Caller mutates original buffer in place
	buf[0] = 'X'
	buf[1] = 'Y'
	buf[2] = 'Z'

	got, found := shard.Get(key)
	if !found {
		t.Fatalf("expected key to be found")
	}
	if !bytes.Equal(got, []byte("original_value")) {
		t.Fatalf("cache was corrupted by caller buffer mutation! got %q, want %q", got, "original_value")
	}
}

// TestLRUShard_MutableReturnedBufferIsolation verifies Section 38:
// Mutating the slice returned by Get does not corrupt subsequent Get calls.
func TestLRUShard_MutableReturnedBufferIsolation(t *testing.T) {
	shard, err := cache.NewLRUShard(10)
	if err != nil {
		t.Fatalf("failed to create shard: %v", err)
	}

	key := cache.NewBlockKey(1, 100)
	shard.Put(key, []byte("immutable_data"))

	// First reader acquires buffer and mutates it
	v1, found := shard.Get(key)
	if !found {
		t.Fatalf("expected key to be found")
	}
	v1[0] = 'X'
	v1[1] = 'X'

	// Second reader must receive pristine original data
	v2, found := shard.Get(key)
	if !found {
		t.Fatalf("expected key to be found")
	}
	if !bytes.Equal(v2, []byte("immutable_data")) {
		t.Fatalf("cache was corrupted by reader mutation! got %q, want %q", v2, "immutable_data")
	}
}

// TestLRUShard_LargeValues verifies correct handling of realistic SSTable data block sizes (4 KB to 8 MiB).
func TestLRUShard_LargeValues(t *testing.T) {
	shard, err := cache.NewLRUShard(4)
	if err != nil {
		t.Fatalf("failed to create shard: %v", err)
	}

	sizes := []int{
		4 * 1024,        // 4 KB target block
		64 * 1024,       // 64 KB
		1 * 1024 * 1024, // 1 MB
		8 * 1024 * 1024, // 8 MB MaxDataBlockSize
	}

	for i, size := range sizes {
		key := cache.NewBlockKey(uint64(i+1), uint64(i*4096))
		payload := bytes.Repeat([]byte{byte(i + 1)}, size)

		shard.Put(key, payload)

		got, found := shard.Get(key)
		if !found {
			t.Fatalf("size %d: failed to retrieve key", size)
		}
		if len(got) != size {
			t.Fatalf("size %d: length mismatch: got %d", size, len(got))
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("size %d: content mismatch", size)
		}

		if err := shard.CheckInvariants(); err != nil {
			t.Fatalf("size %d invariant violation: %v", size, err)
		}
	}
}

// TestLRUShard_Remove verifies explicit removal semantics.
func TestLRUShard_Remove(t *testing.T) {
	shard, err := cache.NewLRUShard(5)
	if err != nil {
		t.Fatalf("failed to create shard: %v", err)
	}

	kA := cache.NewBlockKey(1, 100)
	kB := cache.NewBlockKey(1, 200)
	kC := cache.NewBlockKey(1, 300)

	shard.Put(kA, []byte("A"))
	shard.Put(kB, []byte("B"))
	shard.Put(kC, []byte("C"))

	// Remove middle element (kB)
	if !shard.Remove(kB) {
		t.Fatalf("expected Remove(kB) == true")
	}
	if shard.Remove(kB) {
		t.Fatalf("expected second Remove(kB) == false")
	}
	if shard.Contains(kB) {
		t.Fatalf("kB still present after remove")
	}
	if shard.Len() != 2 {
		t.Fatalf("expected Len=2, got %d", shard.Len())
	}

	if err := shard.CheckInvariants(); err != nil {
		t.Fatalf("invariant violation after removing middle: %v", err)
	}

	// Remove head (kC)
	if !shard.Remove(kC) {
		t.Fatalf("expected Remove(kC) == true")
	}
	if err := shard.CheckInvariants(); err != nil {
		t.Fatalf("invariant violation after removing head: %v", err)
	}

	// Remove tail (kA)
	if !shard.Remove(kA) {
		t.Fatalf("expected Remove(kA) == true")
	}
	if shard.Len() != 0 {
		t.Fatalf("expected Len=0, got %d", shard.Len())
	}
	if err := shard.CheckInvariants(); err != nil {
		t.Fatalf("invariant violation after removing all: %v", err)
	}
}

// TestLRUShard_Clear verifies purging all entries and resetting sentinel pointers.
func TestLRUShard_Clear(t *testing.T) {
	shard, err := cache.NewLRUShard(10)
	if err != nil {
		t.Fatalf("failed to create shard: %v", err)
	}

	for i := 0; i < 10; i++ {
		shard.Put(cache.NewBlockKey(1, uint64(i*4096)), []byte("val"))
	}

	if shard.Len() != 10 {
		t.Fatalf("expected Len=10, got %d", shard.Len())
	}

	shard.Clear()

	if shard.Len() != 0 {
		t.Fatalf("expected Len=0 after Clear, got %d", shard.Len())
	}

	if err := shard.CheckInvariants(); err != nil {
		t.Fatalf("invariant violation after Clear: %v", err)
	}

	// Shard should remain completely usable after Clear
	key := cache.NewBlockKey(2, 0)
	shard.Put(key, []byte("fresh"))
	if shard.Len() != 1 {
		t.Fatalf("expected Len=1 after post-clear Put, got %d", shard.Len())
	}
	if err := shard.CheckInvariants(); err != nil {
		t.Fatalf("invariant violation: %v", err)
	}
}

// TestLRUShard_RepeatedEvictionChurn verifies memory recycling under repeated churn cycles.
func TestLRUShard_RepeatedEvictionChurn(t *testing.T) {
	const capacity = 20
	const iterations = 2000

	shard, err := cache.NewLRUShard(capacity)
	if err != nil {
		t.Fatalf("failed to create shard: %v", err)
	}

	for i := 0; i < iterations; i++ {
		key := cache.NewBlockKey(uint64(i/100), uint64(i*4096))
		shard.Put(key, []byte("churn_payload"))

		if i%100 == 0 {
			if err := shard.CheckInvariants(); err != nil {
				t.Fatalf("invariant violated at iteration %d: %v", i, err)
			}
		}
	}

	if shard.Len() != capacity {
		t.Fatalf("expected Len=%d, got %d", capacity, shard.Len())
	}

	if err := shard.CheckInvariants(); err != nil {
		t.Fatalf("final invariant violation: %v", err)
	}
}

// TestLRUShard_ConcurrentStress validates thread safety under heavy concurrent access across 8, 16, 32 goroutines.
func TestLRUShard_ConcurrentStress(t *testing.T) {
	concurrencyLevels := []int{8, 16, 32}

	for _, workers := range concurrencyLevels {
		t.Run(fmt.Sprintf("Workers_%d", workers), func(t *testing.T) {
			const capacity = 64
			const opsPerWorker = 1000

			shard, err := cache.NewLRUShard(capacity)
			if err != nil {
				t.Fatalf("failed to create shard: %v", err)
			}

			var wg sync.WaitGroup
			var hitCount atomic.Int64
			var missCount atomic.Int64

			startBarrier := make(chan struct{})

			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func(workerID int) {
					defer wg.Done()
					<-startBarrier

					// #nosec G404 - Pseudo-random generator for concurrent testing
					rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))

					for i := 0; i < opsPerWorker; i++ {
						// Keys span a range that induces both hits and evictions
						keyID := rng.Intn(128)
						key := cache.NewBlockKey(uint64(keyID/32), uint64((keyID%32)*4096)) // #nosec G115 - bounded positive test integer

						switch rng.Intn(4) {
						case 0, 1: // 50% Get
							val, found := shard.Get(key)
							if found {
								hitCount.Add(1)
								if len(val) == 0 {
									t.Errorf("empty value returned on hit")
								}
							} else {
								missCount.Add(1)
							}
						case 2: // 25% Put
							val := []byte(fmt.Sprintf("worker-%d-op-%d", workerID, i))
							shard.Put(key, val)
						case 3: // 25% Peek / Contains
							if rng.Intn(2) == 0 {
								shard.Peek(key)
							} else {
								shard.Contains(key)
							}
						}
					}
				}(w)
			}

			close(startBarrier)
			wg.Wait()

			if shard.Len() > capacity {
				t.Fatalf("shard size %d exceeded capacity %d", shard.Len(), capacity)
			}

			if err := shard.CheckInvariants(); err != nil {
				t.Fatalf("concurrent invariant violation: %v", err)
			}

			t.Logf("Workers: %d, Hits: %d, Misses: %d, FinalLen: %d", workers, hitCount.Load(), missCount.Load(), shard.Len())
		})
	}
}

// TestLRUShard_FutureShardHandoff verifies Section 44:
// The shard abstraction is completely isolated with zero global state,
// allowing multiple instances to operate independently.
func TestLRUShard_FutureShardHandoff(t *testing.T) {
	const shardCount = 16
	shards := make([]*cache.LRUShard, shardCount)

	for i := 0; i < shardCount; i++ {
		var err error
		shards[i], err = cache.NewLRUShard(10)
		if err != nil {
			t.Fatalf("failed to create shard %d: %v", i, err)
		}
	}

	// Insert items into specific shards
	for i := 0; i < shardCount; i++ {
		key := cache.NewBlockKey(uint64(i), 0)
		val := []byte(fmt.Sprintf("shard-%d-data", i))
		shards[i].Put(key, val)
	}

	// Verify each shard holds only its item
	for i := 0; i < shardCount; i++ {
		if shards[i].Len() != 1 {
			t.Fatalf("shard %d len=%d, want 1", i, shards[i].Len())
		}
		key := cache.NewBlockKey(uint64(i), 0)
		val, found := shards[i].Get(key)
		if !found || !bytes.Equal(val, []byte(fmt.Sprintf("shard-%d-data", i))) {
			t.Fatalf("shard %d failed to retrieve its key", i)
		}

		// Shard i should not contain item from shard (i+1)%16
		otherKey := cache.NewBlockKey(uint64((i+1)%shardCount), 0)
		if shards[i].Contains(otherKey) {
			t.Fatalf("shard %d contains key belonging to other shard", i)
		}

		if err := shards[i].CheckInvariants(); err != nil {
			t.Fatalf("shard %d invariant violation: %v", i, err)
		}
	}
}
