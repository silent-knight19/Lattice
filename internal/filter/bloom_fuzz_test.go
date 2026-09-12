package filter_test

import (
	"math"
	"testing"

	"github.com/silent-knight19/lattice/internal/filter"
)

// FuzzOptimalBitsetSize tests arithmetic property invariants across arbitrary integer inputs
// without attempting physical heap memory allocations.
func FuzzOptimalBitsetSize(f *testing.F) {
	// Seed corpus with representative, boundary, and pathological inputs
	seeds := []int{
		0, 1, 2, 7, 8, 9, 10, 15, 16, 17,
		63, 64, 65, 100, 1000, 10000, 100000, 1000000, 10000000,
		-1, -2, -100, -1000000,
		math.MaxInt, math.MaxInt - 1, math.MaxInt / 2, (math.MaxInt - 7) / 10, (math.MaxInt-7)/10 + 1,
		math.MinInt, math.MinInt + 1,
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, n int) {
		bits, bytesNeeded, ok := filter.OptimalBitsetSize(n)

		if n < 0 {
			if ok {
				t.Fatalf("OptimalBitsetSize(%d) returned ok=true for negative input", n)
			}
			if bits != 0 || bytesNeeded != 0 {
				t.Fatalf("OptimalBitsetSize(%d) returned non-zero (%d, %d)", n, bits, bytesNeeded)
			}
			return
		}

		if n == 0 {
			if !ok || bits != 0 || bytesNeeded != 0 {
				t.Fatalf("OptimalBitsetSize(0) mismatch: got (%d, %d, %v), want (0, 0, true)", bits, bytesNeeded, ok)
			}
			return
		}

		// Positive integer check
		if n > (math.MaxInt-7)/filter.BitsPerKey {
			if ok {
				t.Fatalf("OptimalBitsetSize(%d) returned ok=true for overflowing input", n)
			}
			return
		}

		if !ok {
			t.Fatalf("OptimalBitsetSize(%d) unexpectedly returned ok=false", n)
		}

		// Verify exact mathematical formula
		expectedBits := uint64(n) * filter.BitsPerKey
		if bits != expectedBits {
			t.Fatalf("n=%d: bits mismatch: got %d, want %d", n, bits, expectedBits)
		}

		expectedBytes := int((bits + 7) / 8)
		if bytesNeeded != expectedBytes {
			t.Fatalf("n=%d: bytes mismatch: got %d, want %d", n, bytesNeeded, expectedBytes)
		}

		if bytesNeeded < 0 {
			t.Fatalf("n=%d: bytesNeeded wrapped to negative: %d", n, bytesNeeded)
		}
	})
}

// FuzzNewBloomFilter_Bounded tests physical allocation property invariants
// strictly bounded to safe key counts [0, 50000] to prevent uncontrolled memory exhaustion.
func FuzzNewBloomFilter_Bounded(f *testing.F) {
	seeds := []int{0, 1, 2, 7, 8, 15, 16, 100, 500, 1000, 5000, 10000, 50000}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, rawN int) {
		// Bounded to [0, 50000] for safe physical allocation
		if rawN < 0 || rawN > 50000 {
			return
		}

		filterObj := filter.NewBloomFilter(rawN)
		if filterObj == nil {
			t.Fatalf("NewBloomFilter(%d) unexpectedly returned nil", rawN)
		}

		expectedBits := uint64(rawN) * filter.BitsPerKey
		expectedBytes := int((expectedBits + 7) / 8)

		if filterObj.KeyCount() != rawN {
			t.Fatalf("KeyCount mismatch: got %d, want %d", filterObj.KeyCount(), rawN)
		}
		if filterObj.BitCount() != expectedBits {
			t.Fatalf("BitCount mismatch: got %d, want %d", filterObj.BitCount(), expectedBits)
		}
		if filterObj.HashCount() != filter.DefaultHashFunctions {
			t.Fatalf("HashCount mismatch: got %d, want %d", filterObj.HashCount(), filter.DefaultHashFunctions)
		}
		if filterObj.ByteSize() != expectedBytes {
			t.Fatalf("ByteSize mismatch: got %d, want %d", filterObj.ByteSize(), expectedBytes)
		}
		if len(filterObj.Bitset()) != expectedBytes {
			t.Fatalf("Bitset slice len mismatch: got %d, want %d", len(filterObj.Bitset()), expectedBytes)
		}

		// Ensure all allocated bits are zero
		for i, b := range filterObj.Bitset() {
			if b != 0 {
				t.Fatalf("rawN=%d: bitset byte at %d was non-zero: 0x%02x", rawN, i, b)
			}
		}
	})
}
