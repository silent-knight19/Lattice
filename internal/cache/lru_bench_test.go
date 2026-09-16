package cache_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/silent-knight19/lattice/internal/cache"
)

func BenchmarkLRUShard_Get_Hit(b *testing.B) {
	for _, capVal := range []int{64, 1024} {
		b.Run(fmt.Sprintf("Capacity_%d", capVal), func(b *testing.B) {
			shard, _ := cache.NewLRUShard(capVal)
			payload := bytes.Repeat([]byte("B"), 4096)

			// Pre-fill cache
			for i := 0; i < capVal; i++ {
				key := cache.NewBlockKey(1, uint64(i*4096))
				shard.Put(key, payload)
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				key := cache.NewBlockKey(1, uint64((i%capVal)*4096))
				_, _ = shard.Get(key)
			}
		})
	}
}

func BenchmarkLRUShard_Get_Miss(b *testing.B) {
	for _, capVal := range []int{64, 1024} {
		b.Run(fmt.Sprintf("Capacity_%d", capVal), func(b *testing.B) {
			shard, _ := cache.NewLRUShard(capVal)
			payload := bytes.Repeat([]byte("B"), 4096)

			// Pre-fill cache
			for i := 0; i < capVal; i++ {
				key := cache.NewBlockKey(1, uint64(i*4096))
				shard.Put(key, payload)
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				// Query keys from file 99 (absent)
				key := cache.NewBlockKey(99, uint64(i*4096))
				_, _ = shard.Get(key)
			}
		})
	}
}

func BenchmarkLRUShard_Put_New(b *testing.B) {
	for _, capVal := range []int{64, 1024} {
		b.Run(fmt.Sprintf("Capacity_%d", capVal), func(b *testing.B) {
			payload := bytes.Repeat([]byte("B"), 4096)

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				b.StopTimer()
				shard, _ := cache.NewLRUShard(capVal)
				b.StartTimer()

				for j := 0; j < capVal; j++ {
					key := cache.NewBlockKey(1, uint64(j*4096))
					shard.Put(key, payload)
				}
			}
		})
	}
}

func BenchmarkLRUShard_Put_Existing(b *testing.B) {
	for _, capVal := range []int{64, 1024} {
		b.Run(fmt.Sprintf("Capacity_%d", capVal), func(b *testing.B) {
			shard, _ := cache.NewLRUShard(capVal)
			payload := bytes.Repeat([]byte("B"), 4096)

			for i := 0; i < capVal; i++ {
				key := cache.NewBlockKey(1, uint64(i*4096))
				shard.Put(key, payload)
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				key := cache.NewBlockKey(1, uint64((i%capVal)*4096))
				shard.Put(key, payload)
			}
		})
	}
}

func BenchmarkLRUShard_Put_Eviction(b *testing.B) {
	for _, capVal := range []int{64, 1024} {
		b.Run(fmt.Sprintf("Capacity_%d", capVal), func(b *testing.B) {
			shard, _ := cache.NewLRUShard(capVal)
			payload := bytes.Repeat([]byte("B"), 4096)

			for i := 0; i < capVal; i++ {
				key := cache.NewBlockKey(1, uint64(i*4096))
				shard.Put(key, payload)
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				// Continuous new keys trigger eviction on every put
				key := cache.NewBlockKey(uint64(i+10), uint64(i*4096))
				shard.Put(key, payload)
			}
		})
	}
}
