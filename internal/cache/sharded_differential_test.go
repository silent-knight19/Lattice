package cache_test

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cache"
)

// referenceShardedCache models the sharded cache using 16 independent referenceLRU instances.
type referenceShardedCache struct {
	capacity int
	shards   [cache.NumShards]*referenceLRU
}

func newReferenceShardedCache(capacity int) *referenceShardedCache {
	ref := &referenceShardedCache{
		capacity: capacity,
	}

	base := capacity / cache.NumShards
	rem := capacity % cache.NumShards

	for i := 0; i < cache.NumShards; i++ {
		shardCap := base
		if i < rem {
			shardCap++
		}
		ref.shards[i] = newReferenceLRU(shardCap)
	}

	return ref
}

func (r *referenceShardedCache) Get(k cache.BlockKey) ([]byte, bool) {
	idx := cache.ShardIndex(k)
	return r.shards[idx].Get(k)
}

func (r *referenceShardedCache) Put(k cache.BlockKey, v []byte) {
	idx := cache.ShardIndex(k)
	r.shards[idx].Put(k, v)
}

func (r *referenceShardedCache) Remove(k cache.BlockKey) bool {
	idx := cache.ShardIndex(k)
	return r.shards[idx].Remove(k)
}

func (r *referenceShardedCache) Len() int {
	total := 0
	for i := 0; i < cache.NumShards; i++ {
		total += len(r.shards[i].keys)
	}
	return total
}

// TestShardedCache_Differential runs 15,000 randomized operations comparing
// ShardedCache against referenceShardedCache across diverse capacity configurations.
func TestShardedCache_Differential(t *testing.T) {
	const totalOperations = 15000
	capacities := []int{0, 1, 15, 16, 17, 32, 50, 64, 128}

	// #nosec G404 - Deterministic pseudo-random generator for differential test
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	for _, capVal := range capacities {
		t.Run(fmt.Sprintf("Capacity_%d", capVal), func(t *testing.T) {
			c, err := cache.NewShardedCache(capVal)
			if err != nil {
				t.Fatalf("failed to create ShardedCache with capacity %d: %v", capVal, err)
			}
			ref := newReferenceShardedCache(capVal)

			const keyUniverse = 64

			for op := 0; op < totalOperations/len(capacities); op++ {
				fileNum := uint64(rng.Intn(8) + 1)             // #nosec G115 - bounded positive test integer
				offset := uint64(rng.Intn(keyUniverse) * 4096) // #nosec G115 - bounded positive test integer
				key := cache.NewBlockKey(fileNum, offset)

				opType := rng.Intn(10)
				switch {
				case opType < 5: // 50% Get
					gotVal, gotFound := c.Get(key)
					refVal, refFound := ref.Get(key)

					if gotFound != refFound {
						t.Fatalf("op %d Get(%v): found mismatch: got %v, ref %v", op, key, gotFound, refFound)
					}
					if !bytes.Equal(gotVal, refVal) {
						t.Fatalf("op %d Get(%v): value mismatch: got %q, ref %q", op, key, gotVal, refVal)
					}

				case opType < 8: // 30% Put
					payload := []byte(fmt.Sprintf("sharded-val-%d-%d", op, rng.Intn(500)))
					c.Put(key, payload)
					ref.Put(key, payload)

				case opType == 8: // 10% Remove
					gotRemoved := c.Remove(key)
					refRemoved := ref.Remove(key)
					if gotRemoved != refRemoved {
						t.Fatalf("op %d Remove(%v): mismatch: got %v, ref %v", op, key, gotRemoved, refRemoved)
					}

				case opType == 9: // 10% Peek / Contains
					gotPeek, gotPeekFound := c.Peek(key)
					shardIdx := cache.ShardIndex(key)
					refVal, refFound := ref.shards[shardIdx].values[key]
					if gotPeekFound != refFound {
						t.Fatalf("op %d Peek(%v): found mismatch: got %v, ref %v", op, key, gotPeekFound, refFound)
					}
					if !bytes.Equal(gotPeek, refVal) {
						t.Fatalf("op %d Peek(%v): value mismatch", op, key)
					}

					gotContains := c.Contains(key)
					if gotContains != refFound {
						t.Fatalf("op %d Contains(%v): mismatch: got %v, ref %v", op, key, gotContains, refFound)
					}
				}

				// Verify global length parity
				if c.Len() != ref.Len() {
					t.Fatalf("op %d: global len mismatch: sharded %d, ref %d", op, c.Len(), ref.Len())
				}

				// Verify per-shard length parity
				for s := 0; s < cache.NumShards; s++ {
					if c.Shard(s).Len() != len(ref.shards[s].keys) {
						t.Fatalf("op %d: shard %d len mismatch: sharded %d, ref %d", op, s, c.Shard(s).Len(), len(ref.shards[s].keys))
					}
				}
			}
		})
	}
}
