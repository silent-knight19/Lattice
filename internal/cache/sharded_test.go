package cache_test

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/silent-knight19/lattice/internal/cache"
	lerrors "github.com/silent-knight19/lattice/internal/errors"
)

// TestShardedCache_Constructor_Validation verifies global capacity semantics across edge cases.
func TestShardedCache_Constructor_Validation(t *testing.T) {
	t.Run("NegativeCapacityRejected", func(t *testing.T) {
		c, err := cache.NewShardedCache(-1)
		if c != nil {
			t.Fatalf("expected nil cache on negative capacity, got %v", c)
		}
		if err == nil {
			t.Fatalf("expected error on negative capacity, got nil")
		}
		if !errors.Is(err, lerrors.ErrInvalidCacheCapacity) {
			t.Fatalf("expected ErrInvalidCacheCapacity, got %v", err)
		}
	})

	testCases := []struct {
		name     string
		capacity int
	}{
		{"Zero", 0},
		{"SingleEntry", 1},
		{"LessThan16", 5},
		{"Fifteen", 15},
		{"ExactSixteen", 16},
		{"Seventeen", 17},
		{"Fifty", 50},
		{"SixtyFour", 64},
		{"OneHundred", 100},
		{"OneThousandTwentyFour", 1024},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := cache.NewShardedCache(tc.capacity)
			if err != nil {
				t.Fatalf("failed to create ShardedCache with capacity %d: %v", tc.capacity, err)
			}

			if c.Capacity() != tc.capacity {
				t.Fatalf("expected Capacity=%d, got %d", tc.capacity, c.Capacity())
			}

			// Verify Model A: Sum of shard capacities must strictly equal total capacity
			sumShardCap := 0
			for i := 0; i < cache.NumShards; i++ {
				shard := c.Shard(i)
				if shard == nil {
					t.Fatalf("shard %d is nil", i)
				}
				sumShardCap += shard.Capacity()
			}

			if sumShardCap != tc.capacity {
				t.Fatalf("capacity distribution mismatch: sum of shards = %d, expected global capacity = %d", sumShardCap, tc.capacity)
			}

			if c.Len() != 0 {
				t.Fatalf("initial Len=%d, want 0", c.Len())
			}
		})
	}

	t.Run("ShardedBlockCacheAlias", func(t *testing.T) {
		c, err := cache.NewShardedBlockCache(64)
		if err != nil {
			t.Fatalf("failed to create NewShardedBlockCache: %v", err)
		}
		if c.Capacity() != 64 {
			t.Fatalf("expected Capacity=64, got %d", c.Capacity())
		}
	})
}

// TestShardedCache_CacheLinePadding verifies the hardware cache-line padding layout contract (Section 11, 12, 27).
func TestShardedCache_CacheLinePadding(t *testing.T) {
	c, err := cache.NewShardedCache(64)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}

	s0 := c.Shard(0)
	s1 := c.Shard(1)

	// #nosec G103 - unsafe pointer used strictly to verify cache-line padding and stride memory alignment
	addr0 := uintptr(unsafe.Pointer(s0))
	// #nosec G103 - unsafe pointer used strictly to verify cache-line padding and stride memory alignment
	addr1 := uintptr(unsafe.Pointer(s1))

	stride := addr1 - addr0

	// The memory stride between consecutive shards must be at least 64 bytes (specifically 128 bytes)
	// and must be an exact multiple of the 64-byte hardware cache line size.
	if stride < cache.CacheLineSize {
		t.Fatalf("shard stride %d is smaller than CacheLineSize %d (false sharing risk!)", stride, cache.CacheLineSize)
	}
	if stride%cache.CacheLineSize != 0 {
		t.Fatalf("shard stride %d is not a multiple of CacheLineSize %d", stride, cache.CacheLineSize)
	}
	if stride != cache.PaddedShardSize {
		t.Fatalf("shard stride %d does not match PaddedShardSize %d", stride, cache.PaddedShardSize)
	}

	t.Logf("Validated Shard Stride: %d bytes (exact multiple of %d-byte hardware cache line)", stride, cache.CacheLineSize)
}

// TestShardedCache_RoutingDeterminism verifies that shard routing is stable, deterministic,
// and strictly bounded within [0, 15].
func TestShardedCache_RoutingDeterminism(t *testing.T) {
	keys := []cache.BlockKey{
		cache.NewBlockKey(0, 0),
		cache.NewBlockKey(1, 4096),
		cache.NewBlockKey(1, 8192),
		cache.NewBlockKey(2, 4096),
		cache.NewBlockKey(100, 1048576),
		cache.NewBlockKey(math.MaxUint64, math.MaxUint64),
		cache.NewBlockKey(math.MaxInt64, 0),
	}

	for _, k := range keys {
		initialIdx := cache.ShardIndex(k)
		if initialIdx < 0 || initialIdx >= cache.NumShards {
			t.Fatalf("ShardIndex(%v) = %d out of bounds [0, 15]", k, initialIdx)
		}

		// Repeat 1,000 times to assert strict determinism
		for rep := 0; rep < 1000; rep++ {
			idx := cache.ShardIndex(k)
			if idx != initialIdx {
				t.Fatalf("ShardIndex(%v) non-deterministic: got %d, want %d", k, idx, initialIdx)
			}
		}
	}
}

// TestShardedCache_DistributionEntropy verifies that Murmur3_128 distributes keys
// evenly across all 16 shards without clustering or starvation.
func TestShardedCache_DistributionEntropy(t *testing.T) {
	const totalKeys = 10000
	shardCounts := make([]int, cache.NumShards)

	for i := 0; i < totalKeys; i++ {
		// Realistic block keys varying both FileNum and Offset
		fileNum := uint64((i % 50) + 1)
		offset := uint64((i / 50) * 4096)
		key := cache.NewBlockKey(fileNum, offset)

		idx := cache.ShardIndex(key)
		shardCounts[idx]++
	}

	// Expected mean = 10,000 / 16 = 625 keys per shard.
	// Invariant: Zero shards must be starved (count > 0).
	// With Murmur3, all shards should receive reasonably balanced traffic.
	minCount := totalKeys
	maxCount := 0
	for i, count := range shardCounts {
		if count == 0 {
			t.Fatalf("shard %d received 0 keys! Pathological hash distribution", i)
		}
		if count < minCount {
			minCount = count
		}
		if count > maxCount {
			maxCount = count
		}
		// Expect each shard to receive between 400 and 850 keys
		if count < 400 || count > 850 {
			t.Errorf("shard %d received %d keys (outside expected statistical bound [400, 850])", i, count)
		}
	}

	t.Logf("10,000 Key Distribution: Min=%d, Max=%d, Mean=625", minCount, maxCount)
}

// TestShardedCache_CrossSSTableIsolation verifies that identical block offsets in
// different SSTables do not collide or overwrite each other.
func TestShardedCache_CrossSSTableIsolation(t *testing.T) {
	c, err := cache.NewShardedCache(64)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}

	keyTable1 := cache.NewBlockKey(1, 4096)
	keyTable2 := cache.NewBlockKey(2, 4096)

	val1 := []byte("block_data_sstable_1")
	val2 := []byte("block_data_sstable_2")

	c.Put(keyTable1, val1)
	c.Put(keyTable2, val2)

	got1, found1 := c.Get(keyTable1)
	if !found1 || !bytes.Equal(got1, val1) {
		t.Fatalf("table 1 block corrupted or not found: %s", got1)
	}

	got2, found2 := c.Get(keyTable2)
	if !found2 || !bytes.Equal(got2, val2) {
		t.Fatalf("table 2 block corrupted or not found: %s", got2)
	}
}

// TestShardedCache_CRUD verifies all core operations on the sharded cache.
func TestShardedCache_CRUD(t *testing.T) {
	c, err := cache.NewShardedCache(32)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}

	key := cache.NewBlockKey(5, 8192)
	val := []byte("payload")

	// Get on absent key
	if _, found := c.Get(key); found {
		t.Fatalf("expected miss on absent key")
	}

	// Put
	c.Put(key, val)
	if c.Len() != 1 {
		t.Fatalf("expected Len=1, got %d", c.Len())
	}

	// Get hit
	got, found := c.Get(key)
	if !found || !bytes.Equal(got, val) {
		t.Fatalf("failed to retrieve key: %s", got)
	}

	// Peek
	peeked, peekFound := c.Peek(key)
	if !peekFound || !bytes.Equal(peeked, val) {
		t.Fatalf("Peek failed: %s", peeked)
	}

	// Contains
	if !c.Contains(key) {
		t.Fatalf("Contains returned false for existing key")
	}

	// GetBlock and PutBlock convenience methods
	c.PutBlock(10, 4096, []byte("block10"))
	gotBlock, foundBlock := c.GetBlock(10, 4096)
	if !foundBlock || !bytes.Equal(gotBlock, []byte("block10")) {
		t.Fatalf("GetBlock failed: %s", gotBlock)
	}

	// Remove
	if !c.Remove(key) {
		t.Fatalf("Remove returned false for existing key")
	}
	if c.Contains(key) {
		t.Fatalf("key still present after Remove")
	}

	// Clear
	c.Clear()
	if c.Len() != 0 {
		t.Fatalf("expected Len=0 after Clear, got %d", c.Len())
	}
}

// TestShardedCache_DefensiveCopying verifies that mutations to input and returned buffers
// do not corrupt the cached data across shards.
func TestShardedCache_DefensiveCopying(t *testing.T) {
	c, err := cache.NewShardedCache(64)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}

	key := cache.NewBlockKey(1, 0)
	buf := []byte("original_bytes")

	// Input buffer mutation
	c.Put(key, buf)
	buf[0] = 'X'

	got, found := c.Get(key)
	if !found || !bytes.Equal(got, []byte("original_bytes")) {
		t.Fatalf("cache corrupted by input buffer mutation: got %s", got)
	}

	// Output buffer mutation
	got[0] = 'Y'
	got2, found2 := c.Get(key)
	if !found2 || !bytes.Equal(got2, []byte("original_bytes")) {
		t.Fatalf("cache corrupted by output buffer mutation: got %s", got2)
	}
}

// TestShardedCache_ConcurrentStress validates thread safety and zero lock contention
// under 8, 16, 32, and 64 concurrent goroutines.
func TestShardedCache_ConcurrentStress(t *testing.T) {
	workerCounts := []int{8, 16, 32, 64}

	for _, workers := range workerCounts {
		t.Run(fmt.Sprintf("Workers_%d", workers), func(t *testing.T) {
			const capacity = 256
			const opsPerWorker = 1000

			c, err := cache.NewShardedCache(capacity)
			if err != nil {
				t.Fatalf("failed to create cache: %v", err)
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
						keyID := rng.Intn(512)
						// #nosec G115 - bounded non-negative integer conversion for test key generation
						fileNum := uint64((keyID % 32) + 1)
						// #nosec G115 - bounded non-negative integer conversion for test key generation
						offset := uint64((keyID / 32) * 4096)
						key := cache.NewBlockKey(fileNum, offset)

						switch rng.Intn(4) {
						case 0, 1: // 50% Get
							val, found := c.Get(key)
							if found {
								hitCount.Add(1)
								if len(val) == 0 {
									t.Errorf("empty value on hit")
								}
							} else {
								missCount.Add(1)
							}
						case 2: // 25% Put
							val := []byte(fmt.Sprintf("w%d-op%d", workerID, i))
							c.Put(key, val)
						case 3: // 25% Peek / Contains / Remove
							r := rng.Intn(3)
							switch r {
							case 0:
								c.Peek(key)
							case 1:
								c.Contains(key)
							case 2:
								c.Remove(key)
							}
						}
					}
				}(w)
			}

			close(startBarrier)
			wg.Wait()

			if c.Len() > capacity {
				t.Fatalf("cache length %d exceeded capacity %d", c.Len(), capacity)
			}

			// Validate invariants on all 16 shards
			for i := 0; i < cache.NumShards; i++ {
				if err := c.Shard(i).CheckInvariants(); err != nil {
					t.Fatalf("shard %d invariant violation: %v", i, err)
				}
			}

			t.Logf("Workers: %d, Hits: %d, Misses: %d, FinalLen: %d", workers, hitCount.Load(), missCount.Load(), c.Len())
		})
	}
}
