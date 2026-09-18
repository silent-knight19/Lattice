package benchmark_test

import (
	"testing"

	"github.com/silent-knight19/lattice/internal/benchmark"
)

// FuzzNewZipfGenerator fuzzes generator initialization and sampling across
// arbitrary keyspaces, skew parameters, and seeds.
func FuzzNewZipfGenerator(f *testing.F) {
	// Seed corpus with valid, boundary, and pathological inputs
	f.Add(uint64(1), 0.99, int64(42))
	f.Add(uint64(2), 0.50, int64(0))
	f.Add(uint64(10), 0.01, int64(-1))
	f.Add(uint64(100), 0.999, int64(12345))
	f.Add(uint64(1000), 0.80, int64(999999))
	f.Add(uint64(0), 0.99, int64(1))             // Invalid N = 0
	f.Add(uint64(10), -0.5, int64(2))            // Invalid negative theta
	f.Add(uint64(10), 1.0, int64(3))             // Invalid theta = 1.0
	f.Add(uint64(10), 2.5, int64(4))             // Invalid theta > 1.0
	f.Add(uint64(2_000_000_000), 0.99, int64(5)) // Exceeds MaxKeyspace

	f.Fuzz(func(t *testing.T, n uint64, theta float64, seed int64) {
		gen, err := benchmark.NewZipfGenerator(n, theta, seed)
		if err != nil {
			// Must return clean typed error without panicking
			return
		}

		if gen == nil {
			t.Fatal("expected non-nil generator when err == nil")
		}

		// Perform bounded sampling
		const samples = 50
		scratch := make([]byte, 0, 32)
		for i := 0; i < samples; i++ {
			rank := gen.Next()
			if rank >= n {
				t.Fatalf("sampled rank %d out of bounds for n=%d", rank, n)
			}

			key := gen.NextKeyBuf(scratch)
			if len(key) == 0 {
				t.Fatal("generated empty key")
			}
		}
	})
}
