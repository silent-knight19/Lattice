package cache

import (
	"encoding/binary"
	"sync/atomic"

	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/filter"
	"github.com/silent-knight19/lattice/internal/metrics"
)

// NumShards is the fixed count of independent LRU cache shards (16).
// Partitioning into 16 shards eliminates global mutex contention across concurrent CPU cores.
const NumShards = 16

// CacheLineSize is the standard hardware L1 CPU cache line size in bytes (64 bytes).
const CacheLineSize = 64

// PaddedShardSize is the total padded memory stride for each shard (128 bytes = 2 * CacheLineSize).
const PaddedShardSize = 128

// paddedShard embeds an LRUShard and appends explicit trailing padding to ensure
// its stride in contiguous memory is an exact multiple of CacheLineSize (128 bytes).
// This guarantees that adjacent shard mutexes and metadata reside on disjoint
// 64-byte hardware cache lines, completely eliminating false sharing.
type paddedShard struct {
	LRUShard
	_ [48]byte // 80 bytes (LRUShard) + 48 bytes = 128 bytes (2 x 64-byte cache lines)
}

// ShardedCache implements a high-throughput, 16-shard concurrent LRU Block Cache
// with 64-byte hardware cache line padding to prevent false sharing.
//
// Concurrency & Routing:
//  1. Keys are routed deterministically across 16 independent LRU shards via Murmur3_128(BlockKey).
//  2. Normal key operations (Get, Put, Peek, Contains, Remove) lock ONLY the targeted shard mutex.
//     There is NO global cache lock on the hot path, enabling concurrent throughput across CPU cores.
//  3. The global capacity is partitioned deterministically across the 16 shards (Model A).
//
// Must not be copied after initialization: ShardedCache contains 16 shard
// mutexes. Always use *ShardedCache. Copying duplicates mutexes and breaks
// synchronization.
type ShardedCache struct {
	capacity int
	hits     atomic.Uint64
	misses   atomic.Uint64
	shards   [NumShards]paddedShard
}

// ShardedBlockCache is an alias for ShardedCache matching storage engine roadmap naming.
type ShardedBlockCache = ShardedCache

// NewShardedCache constructs an initialized ShardedCache with the given total block capacity.
//
// Capacity Semantics (Model A — Distributed Global Capacity):
//   - If capacity < 0, returns *errors.InvalidCacheCapacityError.
//   - If capacity == 0, all 16 shards receive capacity 0 (non-retaining pass-through).
//   - If capacity > 0, the total capacity is partitioned deterministically across the 16 shards:
//     base = capacity / 16, remainder = capacity % 16.
//     Shards 0 through remainder-1 receive base + 1 capacity; the remaining shards receive base.
//     The sum of all shard capacities strictly equals the configured global capacity.
//
// Capacity Accounting Model:
// Capacity is tracked in number of block entries (assuming ~4KB standard SSTable data blocks).
// If SSTables contain oversized data blocks (up to sstable.MaxDataBlockSize), memory footprint
// scales accordingly as each block counts as 1 entry.
func NewShardedCache(capacity int) (*ShardedCache, error) {
	if capacity < 0 {
		return nil, &errors.InvalidCacheCapacityError{Capacity: capacity}
	}

	c := &ShardedCache{
		capacity: capacity,
	}

	base := capacity / NumShards
	rem := capacity % NumShards

	for i := 0; i < NumShards; i++ {
		shardCap := base
		if i < rem {
			shardCap++
		}
		initLRUShard(&c.shards[i].LRUShard, shardCap)
	}

	return c, nil
}

// NewShardedBlockCache is an alias for NewShardedCache.
func NewShardedBlockCache(capacity int) (*ShardedCache, error) {
	return NewShardedCache(capacity)
}

// ShardIndex returns the deterministic shard index in [0, NumShards-1] for the given BlockKey.
//
// Routing Algorithm (architecture-spec.md Section 34.2):
//
//	ShardIndex = (Murmur3_128(FileNum || Offset) >> 28) % 16
//
// Implementation:
//   - Serializes FileNum (8B, Big-Endian) and Offset (8B, Big-Endian) into a 16-byte stack buffer.
//   - Hashes using filter.Murmur3_128 with canonical seed 0 (zero heap allocations).
//   - Extracts 4 bits (bits 28..31) from the 64-bit hash h1: int((h1 >> 28) & 0x0F),
//     guaranteeing a uniform, deterministic shard index in [0, 15] across the 16 shards.
func ShardIndex(key BlockKey) int {
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[0:8], key.FileNum)
	binary.BigEndian.PutUint64(buf[8:16], key.Offset)
	h1, _ := filter.Murmur3_128(buf[:], filter.DefaultMurmur3Seed)
	return int((h1 >> 28) & 0x0F)
}

// Get retrieves the cached value associated with key from its routed shard.
// Returns (defensiveCopy, true) on hit, or (nil, false) on miss.
// Locks only the targeted shard mutex.
// A nil receiver reports a miss.
func (c *ShardedCache) Get(key BlockKey) ([]byte, bool) {
	if c == nil {
		metrics.BlockCacheMisses.Inc()
		return nil, false
	}
	idx := ShardIndex(key)
	val, ok := c.shards[idx].Get(key)
	if ok {
		c.hits.Add(1)
		metrics.BlockCacheHits.Inc()
	} else {
		c.misses.Add(1)
		metrics.BlockCacheMisses.Inc()
	}
	return val, ok
}

// Hits returns the authoritative number of cache hits recorded by this cache instance.
func (c *ShardedCache) Hits() uint64 {
	if c == nil {
		return 0
	}
	return c.hits.Load()
}

// Misses returns the authoritative number of cache misses recorded by this cache instance.
func (c *ShardedCache) Misses() uint64 {
	if c == nil {
		return 0
	}
	return c.misses.Load()
}

// GetBlock is a convenience method for querying by raw SSTable file number and block byte offset.
func (c *ShardedCache) GetBlock(sstableID uint64, offset uint64) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	return c.Get(NewBlockKey(sstableID, offset))
}

// Put inserts or updates a key-value entry in its routed shard.
// Locks only the targeted shard mutex.
// A nil receiver is a no-op.
func (c *ShardedCache) Put(key BlockKey, val []byte) {
	if c == nil {
		return
	}
	idx := ShardIndex(key)
	c.shards[idx].Put(key, val)
}

// PutBlock is a convenience method for inserting by raw SSTable file number and block byte offset.
func (c *ShardedCache) PutBlock(sstableID uint64, offset uint64, val []byte) {
	if c == nil {
		return
	}
	c.Put(NewBlockKey(sstableID, offset), val)
}

// Peek retrieves the cached value for key from its routed shard without modifying recency.
// A nil receiver reports a miss.
func (c *ShardedCache) Peek(key BlockKey) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	idx := ShardIndex(key)
	return c.shards[idx].Peek(key)
}

// Contains reports whether key exists in its routed shard without modifying recency.
// A nil receiver reports false.
func (c *ShardedCache) Contains(key BlockKey) bool {
	if c == nil {
		return false
	}
	idx := ShardIndex(key)
	return c.shards[idx].Contains(key)
}

// Remove deletes key and its corresponding node from its routed shard if present.
// A nil receiver reports false.
func (c *ShardedCache) Remove(key BlockKey) bool {
	if c == nil {
		return false
	}
	idx := ShardIndex(key)
	return c.shards[idx].Remove(key)
}

// Len returns the total number of cached entries across all 16 shards.
// Locks shards sequentially to prevent deadlocks and maintain lock independence.
// A nil receiver reports 0. The result is a point-in-time sum observed
// without a global lock; concurrent Put/Remove/Clear may interleave.
func (c *ShardedCache) Len() int {
	if c == nil {
		return 0
	}
	total := 0
	for i := 0; i < NumShards; i++ {
		total += c.shards[i].Len()
	}
	return total
}

// Capacity returns the total configured global capacity across all 16 shards.
// A nil receiver reports 0.
func (c *ShardedCache) Capacity() int {
	if c == nil {
		return 0
	}
	return c.capacity
}

// Clear purges all entries from all 16 shards.
// Locks and resets shards sequentially (0..15).
// A nil receiver is a no-op.
//
// Concurrency Semantics:
// Clear acquires and releases each shard mutex sequentially without holding a global
// cache lock, avoiding global contention and deadlocks. However, Clear does NOT provide
// an atomic barrier against concurrent writers; if concurrent goroutines invoke Put
// while Clear is running, previously cleared shards may receive new entries. Callers
// requiring a strictly quiescent, empty cache state (such as during test setup or engine
// shutdown) must synchronize or pause concurrent write operations.
func (c *ShardedCache) Clear() {
	if c == nil {
		return
	}
	for i := 0; i < NumShards; i++ {
		c.shards[i].Clear()
	}
}

// Shard returns a pointer to the i-th LRUShard (0 <= i < NumShards).
// Returns nil if i is out of bounds or the parent cache is nil.
//
// Test-only inspection: mutating the returned shard directly (notably Put)
// bypasses deterministic shard routing and can create entries that are
// unreachable via routed Get/Put while still consuming that shard's capacity
// and Len accounting. Production callers must use the routed methods
// (Get/Put/Peek/Contains/Remove) and use Shard only for read-only inspection
// (Len/Capacity/invariant checks).
func (c *ShardedCache) Shard(i int) *LRUShard {
	if c == nil {
		return nil
	}
	if i < 0 || i >= NumShards {
		return nil
	}
	return &c.shards[i].LRUShard
}
