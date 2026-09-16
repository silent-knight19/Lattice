package cache_test

import (
	"bytes"
	"fmt"
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/cache"
)

func BenchmarkShardedCache_SingleThreaded(b *testing.B) {
	const capacity = 1024
	c, _ := cache.NewShardedCache(capacity)
	payload := bytes.Repeat([]byte("B"), 4096)

	for i := 0; i < capacity; i++ {
		key := cache.NewBlockKey(1, uint64(i*4096))
		c.Put(key, payload)
	}

	b.Run("Get_Hit", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			key := cache.NewBlockKey(1, uint64((i%capacity)*4096))
			_, _ = c.Get(key)
		}
	})

	b.Run("Get_Miss", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			key := cache.NewBlockKey(999, uint64(i*4096))
			_, _ = c.Get(key)
		}
	})

	b.Run("Put_Existing", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			key := cache.NewBlockKey(1, uint64((i%capacity)*4096))
			c.Put(key, payload)
		}
	})
}

// BenchmarkShardedCache_ConcurrentScalability measures throughput under increasing goroutine counts: 1, 8, 16, 32, 64.
func BenchmarkShardedCache_ConcurrentScalability(b *testing.B) {
	concurrencyLevels := []int{1, 8, 16, 32, 64}
	const capacity = 2048
	payload := bytes.Repeat([]byte("B"), 4096)

	for _, workers := range concurrencyLevels {
		b.Run(fmt.Sprintf("Workers_%d", workers), func(b *testing.B) {
			c, _ := cache.NewShardedCache(capacity)

			// Pre-populate
			for i := 0; i < capacity; i++ {
				key := cache.NewBlockKey(uint64(i%32+1), uint64(i*4096))
				c.Put(key, payload)
			}

			b.ReportAllocs()
			b.ResetTimer()

			b.SetParallelism(workers)
			b.RunParallel(func(pb *testing.PB) {
				idx := 0
				for pb.Next() {
					key := cache.NewBlockKey(uint64(idx%32+1), uint64((idx%capacity)*4096))
					if idx%4 == 0 {
						c.Put(key, payload)
					} else {
						_, _ = c.Get(key)
					}
					idx++
				}
			})
		})
	}
}

// BenchmarkContentionComparison compares a single LRUShard vs 16-way ShardedCache under 64 concurrent goroutines.
func BenchmarkContentionComparison(b *testing.B) {
	const capacity = 1024
	payload := bytes.Repeat([]byte("B"), 4096)
	const workers = 64

	b.Run("Single_LRUShard_64Workers", func(b *testing.B) {
		shard, _ := cache.NewLRUShard(capacity)
		for i := 0; i < capacity; i++ {
			shard.Put(cache.NewBlockKey(uint64(i%16+1), uint64(i*4096)), payload)
		}

		b.ReportAllocs()
		b.ResetTimer()

		var wg sync.WaitGroup
		opsPerWorker := b.N / workers
		if opsPerWorker == 0 {
			opsPerWorker = 1
		}

		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()
				for i := 0; i < opsPerWorker; i++ {
					// #nosec G115 - bounded non-negative integer conversion for benchmark key generation
					key := cache.NewBlockKey(uint64((workerID+i)%16+1), uint64(i*4096))
					if i%4 == 0 {
						shard.Put(key, payload)
					} else {
						_, _ = shard.Get(key)
					}
				}
			}(w)
		}
		wg.Wait()
	})

	b.Run("ShardedCache_16Shards_64Workers", func(b *testing.B) {
		c, _ := cache.NewShardedCache(capacity)
		for i := 0; i < capacity; i++ {
			c.Put(cache.NewBlockKey(uint64(i%16+1), uint64(i*4096)), payload)
		}

		b.ReportAllocs()
		b.ResetTimer()

		var wg sync.WaitGroup
		opsPerWorker := b.N / workers
		if opsPerWorker == 0 {
			opsPerWorker = 1
		}

		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()
				for i := 0; i < opsPerWorker; i++ {
					// #nosec G115 - bounded non-negative integer conversion for benchmark key generation
					key := cache.NewBlockKey(uint64((workerID+i)%16+1), uint64(i*4096))
					if i%4 == 0 {
						c.Put(key, payload)
					} else {
						_, _ = c.Get(key)
					}
				}
			}(w)
		}
		wg.Wait()
	})
}
