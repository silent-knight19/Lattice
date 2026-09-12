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
