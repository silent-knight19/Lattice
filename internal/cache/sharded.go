package cache

import (
	"encoding/binary"

	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/filter"
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
type ShardedCache struct {
	capacity int
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
//   - Extracts 4 high bits: int((h1 >> 28) & 0x0F) guaranteeing [0, 15] range.
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
func (c *ShardedCache) Get(key BlockKey) ([]byte, bool) {
	idx := ShardIndex(key)
	return c.shards[idx].Get(key)
}

// GetBlock is a convenience method for querying by raw SSTable file number and block byte offset.
func (c *ShardedCache) GetBlock(sstableID uint64, offset uint64) ([]byte, bool) {
	return c.Get(NewBlockKey(sstableID, offset))
}

// Put inserts or updates a key-value entry in its routed shard.
// Locks only the targeted shard mutex.
func (c *ShardedCache) Put(key BlockKey, val []byte) {
	idx := ShardIndex(key)
	c.shards[idx].Put(key, val)
}

// PutBlock is a convenience method for inserting by raw SSTable file number and block byte offset.
func (c *ShardedCache) PutBlock(sstableID uint64, offset uint64, val []byte) {
	c.Put(NewBlockKey(sstableID, offset), val)
}

// Peek retrieves the cached value for key from its routed shard without modifying recency.
func (c *ShardedCache) Peek(key BlockKey) ([]byte, bool) {
	idx := ShardIndex(key)
	return c.shards[idx].Peek(key)
}

// Contains reports whether key exists in its routed shard without modifying recency.
func (c *ShardedCache) Contains(key BlockKey) bool {
	idx := ShardIndex(key)
	return c.shards[idx].Contains(key)
}

// Remove deletes key and its corresponding node from its routed shard if present.
func (c *ShardedCache) Remove(key BlockKey) bool {
	idx := ShardIndex(key)
	return c.shards[idx].Remove(key)
}

// Len returns the total number of cached entries across all 16 shards.
// Locks shards sequentially to prevent deadlocks and maintain lock independence.
func (c *ShardedCache) Len() int {
	total := 0
	for i := 0; i < NumShards; i++ {
		total += c.shards[i].Len()
	}
	return total
}

// Capacity returns the total configured global capacity across all 16 shards.
func (c *ShardedCache) Capacity() int {
	return c.capacity
}

// Clear purges all entries from all 16 shards.
// Locks and resets shards sequentially (0..15).
func (c *ShardedCache) Clear() {
	for i := 0; i < NumShards; i++ {
		c.shards[i].Clear()
	}
}

// Shard returns a pointer to the i-th LRUShard (0 <= i < NumShards) for inspection and white-box testing.
// Returns nil if i is out of bounds.
func (c *ShardedCache) Shard(i int) *LRUShard {
	if i < 0 || i >= NumShards {
		return nil
	}
	return &c.shards[i].LRUShard
}
