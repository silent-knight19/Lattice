package cache_test

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cache"
)

// referenceLRU is an independent reference implementation of an LRU cache
// backed by a slice of keys (index 0 = MRU, index len-1 = LRU) and a value map.
// It uses no linked list nodes, no pointers, and no sentinels.
type referenceLRU struct {
	capacity int
	keys     []cache.BlockKey
	values   map[cache.BlockKey][]byte
}

func newReferenceLRU(capacity int) *referenceLRU {
	return &referenceLRU{
		capacity: capacity,
		keys:     make([]cache.BlockKey, 0, capacity),
		values:   make(map[cache.BlockKey][]byte, capacity),
	}
}

func (r *referenceLRU) Get(k cache.BlockKey) ([]byte, bool) {
	val, exists := r.values[k]
	if !exists {
		return nil, false
	}
	// Move k to index 0 (MRU)
	r.moveToMRU(k)
	return bytes.Clone(val), true
}

func (r *referenceLRU) Put(k cache.BlockKey, v []byte) {
	if r.capacity == 0 {
		return
	}

	if _, exists := r.values[k]; exists {
		r.values[k] = bytes.Clone(v)
		r.moveToMRU(k)
		return
	}

	if len(r.keys) >= r.capacity {
		// Evict LRU (last element)
		lastIdx := len(r.keys) - 1
		lruKey := r.keys[lastIdx]
		r.keys = r.keys[:lastIdx]
		delete(r.values, lruKey)
	}

	// Insert at MRU (index 0)
	r.keys = append([]cache.BlockKey{k}, r.keys...)
	r.values[k] = bytes.Clone(v)
}

func (r *referenceLRU) Remove(k cache.BlockKey) bool {
	if _, exists := r.values[k]; !exists {
		return false
	}
	delete(r.values, k)
	for i, key := range r.keys {
		if key == k {
			r.keys = append(r.keys[:i], r.keys[i+1:]...)
			break
		}
	}
	return true
}

func (r *referenceLRU) moveToMRU(k cache.BlockKey) {
	for i, key := range r.keys {
		if key == k {
			r.keys = append(r.keys[:i], r.keys[i+1:]...)
			break
		}
	}
	r.keys = append([]cache.BlockKey{k}, r.keys...)
}

// TestLRUShard_Differential executes 10,000+ randomized operations against both
// the production LRUShard and the independent referenceLRU model with varying capacities (0..32).
func TestLRUShard_Differential(t *testing.T) {
	const totalOperations = 15000
	capacities := []int{0, 1, 2, 3, 5, 8, 16, 32}

	// #nosec G404 - Pseudo-random generator for reproducible differential testing
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	for _, capVal := range capacities {
		t.Run(fmt.Sprintf("Capacity_%d", capVal), func(t *testing.T) {
			shard, err := cache.NewLRUShard(capVal)
			if err != nil {
				t.Fatalf("failed to create shard: %v", err)
			}
			ref := newReferenceLRU(capVal)

			const keyUniverse = 40

			for op := 0; op < totalOperations/len(capacities); op++ {
				fileNum := uint64(rng.Intn(4) + 1)             // #nosec G115 - bounded positive random integer
				offset := uint64(rng.Intn(keyUniverse) * 4096) // #nosec G115 - bounded positive random integer
				key := cache.NewBlockKey(fileNum, offset)

				opType := rng.Intn(10)
				switch {
				case opType < 5: // 50% Get
					gotVal, gotFound := shard.Get(key)
					refVal, refFound := ref.Get(key)

					if gotFound != refFound {
						t.Fatalf("op %d Get(%v): found mismatch: got %v, ref %v", op, key, gotFound, refFound)
					}
					if !bytes.Equal(gotVal, refVal) {
						t.Fatalf("op %d Get(%v): value mismatch: got %q, ref %q", op, key, gotVal, refVal)
					}

				case opType < 8: // 30% Put
					payload := []byte(fmt.Sprintf("val-%d-%d", op, rng.Intn(1000)))
					shard.Put(key, payload)
					ref.Put(key, payload)

				case opType == 8: // 10% Remove
					gotRemoved := shard.Remove(key)
					refRemoved := ref.Remove(key)
					if gotRemoved != refRemoved {
						t.Fatalf("op %d Remove(%v): mismatch: got %v, ref %v", op, key, gotRemoved, refRemoved)
					}

				case opType == 9: // 10% Peek / Contains
					gotPeek, gotPeekFound := shard.Peek(key)
					refVal, refFound := ref.values[key]
					if gotPeekFound != refFound {
						t.Fatalf("op %d Peek(%v): found mismatch: got %v, ref %v", op, key, gotPeekFound, refFound)
					}
					if !bytes.Equal(gotPeek, refVal) {
						t.Fatalf("op %d Peek(%v): value mismatch", op, key)
					}

					gotContains := shard.Contains(key)
					if gotContains != refFound {
						t.Fatalf("op %d Contains(%v): mismatch: got %v, ref %v", op, key, gotContains, refFound)
					}
				}

				// Verify length parity
				if shard.Len() != len(ref.keys) {
					t.Fatalf("op %d: len mismatch: shard %d, ref %d", op, shard.Len(), len(ref.keys))
				}

				// Verify exact MRU-to-LRU ordering
				shardKeys := shard.KeysMRUtoLRU()
				if len(shardKeys) != len(ref.keys) {
					t.Fatalf("op %d: keys length mismatch: shard %d, ref %d", op, len(shardKeys), len(ref.keys))
				}
				for i := range shardKeys {
					if shardKeys[i] != ref.keys[i] {
						t.Fatalf("op %d: ordering mismatch at index %d: shard %v, ref %v", op, i, shardKeys[i], ref.keys[i])
					}
				}

				// Verify internal structural invariants
				if err := shard.CheckInvariants(); err != nil {
					t.Fatalf("op %d: invariant violation: %v", op, err)
				}
			}
		})
	}
}
