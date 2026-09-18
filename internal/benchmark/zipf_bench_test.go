package benchmark_test

import (
	"fmt"
	"testing"

	"github.com/silent-knight19/lattice/internal/benchmark"
)

// BenchmarkZipf_Next measures the raw sampling throughput and latency of Next().
func BenchmarkZipf_Next(b *testing.B) {
	for _, n := range []uint64{100, 1000, 10000, 100000, 1000000} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			gen, err := benchmark.NewDefaultZipfGenerator(n, 42)
			if err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()
			b.ReportAllocs()

			var dummy uint64
			for i := 0; i < b.N; i++ {
				dummy += gen.Next()
			}
			_ = dummy
		})
	}
}

// BenchmarkZipf_NextKeyBuf measures key generation using a caller-provided scratch buffer.
func BenchmarkZipf_NextKeyBuf(b *testing.B) {
	gen, err := benchmark.NewDefaultZipfGenerator(100000, 42)
	if err != nil {
		b.Fatal(err)
	}

	buf := make([]byte, 0, 32)
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		buf = gen.NextKeyBuf(buf)
	}
}

// BenchmarkZipf_NextKey measures allocating key generation.
func BenchmarkZipf_NextKey(b *testing.B) {
	gen, err := benchmark.NewDefaultZipfGenerator(100000, 42)
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = gen.NextKey()
	}
}

// BenchmarkZipf_Constructor measures the initialization overhead of NewDefaultZipfGenerator.
func BenchmarkZipf_Constructor(b *testing.B) {
	for _, n := range []uint64{100, 1000, 10000, 100000, 1000000, 10000000} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				_, err := benchmark.NewDefaultZipfGenerator(n, 42)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
