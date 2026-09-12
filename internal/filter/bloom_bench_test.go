package filter_test

import (
	"testing"

	"github.com/silent-knight19/lattice/internal/filter"
)

func BenchmarkNewBloomFilter_100(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		f := filter.NewBloomFilter(100)
		if f == nil {
			b.Fatal("unexpected nil filter")
		}
	}
}

func BenchmarkNewBloomFilter_1K(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		f := filter.NewBloomFilter(1000)
		if f == nil {
			b.Fatal("unexpected nil filter")
		}
	}
}

func BenchmarkNewBloomFilter_10K(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		f := filter.NewBloomFilter(10000)
		if f == nil {
			b.Fatal("unexpected nil filter")
		}
	}
}

func BenchmarkNewBloomFilter_100K(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		f := filter.NewBloomFilter(100000)
		if f == nil {
			b.Fatal("unexpected nil filter")
		}
	}
}

func BenchmarkNewBloomFilter_1M(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		f := filter.NewBloomFilter(1000000)
		if f == nil {
			b.Fatal("unexpected nil filter")
		}
	}
}

func BenchmarkOptimalBitsetSize(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, ok := filter.OptimalBitsetSize(100000)
		if !ok {
			b.Fatal("unexpected calculation failure")
		}
	}
}

func BenchmarkMurmur3_16B(b *testing.B) {
	data := make([]byte, 16)
	b.SetBytes(16)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = filter.Murmur3_128(data, 0)
	}
}

func BenchmarkMurmur3_64B(b *testing.B) {
	data := make([]byte, 64)
	b.SetBytes(64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = filter.Murmur3_128(data, 0)
	}
}

func BenchmarkMurmur3_256B(b *testing.B) {
	data := make([]byte, 256)
	b.SetBytes(256)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = filter.Murmur3_128(data, 0)
	}
}

func BenchmarkMurmur3_1KB(b *testing.B) {
	data := make([]byte, 1024)
	b.SetBytes(1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = filter.Murmur3_128(data, 0)
	}
}

func BenchmarkMurmur3_4KB(b *testing.B) {
	data := make([]byte, 4096)
	b.SetBytes(4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = filter.Murmur3_128(data, 0)
	}
}

func BenchmarkBloomFilter_Add_10K(b *testing.B) {
	f := filter.NewBloomFilter(10000)
	key := []byte("benchmark_add_key")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.Add(key)
	}
}

func BenchmarkBloomFilter_MayContain_Hit(b *testing.B) {
	f := filter.NewBloomFilter(10000)
	key := []byte("benchmark_hit_key")
	f.Add(key)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = f.MayContain(key)
	}
}

func BenchmarkBloomFilter_MayContain_Miss(b *testing.B) {
	f := filter.NewBloomFilter(10000)
	presentKey := []byte("benchmark_present_key")
	absentKey := []byte("benchmark_absent_key")
	f.Add(presentKey)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = f.MayContain(absentKey)
	}
}
