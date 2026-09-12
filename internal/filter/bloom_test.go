package filter_test

import (
	"bytes"
	"fmt"
	"math"
	"testing"

	"github.com/silent-knight19/lattice/internal/filter"
)

// independentOracle calculates expected Bloom filter parameters independently
// using floating-point math and alternative bit arithmetic to prevent shared-bug validation.
func independentOracle(n int) (expectedBits uint64, expectedBytes int) {
	bits := uint64(n) * 10
	bytesNeeded := int(math.Ceil(float64(bits) / 8.0))
	return bits, bytesNeeded
}

// TestBloomFilter_BasicSizing verifies the exact mathematical sizing contract across
// the full spectrum of representative cardinalities specified in the implementation plan.
func TestBloomFilter_BasicSizing(t *testing.T) {
	testCases := []struct {
		n             int
		expectedBits  uint64
		expectedBytes int
	}{
		{n: 1, expectedBits: 10, expectedBytes: 2},
		{n: 2, expectedBits: 20, expectedBytes: 3},
		{n: 7, expectedBits: 70, expectedBytes: 9},
		{n: 8, expectedBits: 80, expectedBytes: 10},
		{n: 10, expectedBits: 100, expectedBytes: 13},
		{n: 16, expectedBits: 160, expectedBytes: 20},
		{n: 100, expectedBits: 1000, expectedBytes: 125},
		{n: 1000, expectedBits: 10000, expectedBytes: 1250},
		{n: 10000, expectedBits: 100000, expectedBytes: 12500},
		{n: 100000, expectedBits: 1000000, expectedBytes: 125000},
		{n: 1000000, expectedBits: 10000000, expectedBytes: 1250000},
		{n: 10000000, expectedBits: 100000000, expectedBytes: 12500000},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("n=%d", tc.n), func(t *testing.T) {
			// 1. Verify OptimalBitsetSize calculation
			bits, byteCount, ok := filter.OptimalBitsetSize(tc.n)
			if !ok {
				t.Fatalf("OptimalBitsetSize(%d) returned ok=false", tc.n)
			}
			if bits != tc.expectedBits {
				t.Fatalf("OptimalBitsetSize(%d) bitCount mismatch: got %d, want %d", tc.n, bits, tc.expectedBits)
			}
			if byteCount != tc.expectedBytes {
				t.Fatalf("OptimalBitsetSize(%d) byteCount mismatch: got %d, want %d", tc.n, byteCount, tc.expectedBytes)
			}

			// 2. Cross-verify against independent floating-point math oracle
			oracleBits, oracleBytes := independentOracle(tc.n)
			if bits != oracleBits || byteCount != oracleBytes {
				t.Fatalf("independent oracle disagreement for n=%d: got (%d, %d), oracle (%d, %d)",
					tc.n, bits, byteCount, oracleBits, oracleBytes)
			}

			// 3. Verify NewBloomFilter allocator
			f := filter.NewBloomFilter(tc.n)
			if f == nil {
				t.Fatalf("NewBloomFilter(%d) returned nil", tc.n)
			}
			if f.KeyCount() != tc.n {
				t.Fatalf("KeyCount mismatch: got %d, want %d", f.KeyCount(), tc.n)
			}
			if f.BitCount() != tc.expectedBits {
				t.Fatalf("BitCount mismatch: got %d, want %d", f.BitCount(), tc.expectedBits)
			}
			if f.ByteSize() != tc.expectedBytes {
				t.Fatalf("ByteSize mismatch: got %d, want %d", f.ByteSize(), tc.expectedBytes)
			}
			if f.HashCount() != filter.DefaultHashFunctions {
				t.Fatalf("HashCount mismatch: got %d, want %d", f.HashCount(), filter.DefaultHashFunctions)
			}
			if len(f.Bitset()) != tc.expectedBytes {
				t.Fatalf("Bitset slice length mismatch: got %d, want %d", len(f.Bitset()), tc.expectedBytes)
			}
			if f.IsEmpty() {
				t.Fatalf("IsEmpty() was true for non-empty filter n=%d", tc.n)
			}
		})
	}
}

// TestBloomFilter_ByteRoundingBoundaries verifies ceil(m / 8) across all possible modulo remainders
// (10n mod 8 = 0, 2, 4, 6) for consecutive small integers.
func TestBloomFilter_ByteRoundingBoundaries(t *testing.T) {
	for n := 1; n <= 100; n++ {
		bits, byteCount, ok := filter.OptimalBitsetSize(n)
		if !ok {
			t.Fatalf("OptimalBitsetSize(%d) failed", n)
		}

		oracleBits, oracleBytes := independentOracle(n)
		if bits != oracleBits {
			t.Fatalf("n=%d: bits=%d != oracleBits=%d", n, bits, oracleBits)
		}
		if byteCount != oracleBytes {
			t.Fatalf("n=%d: byteCount=%d != oracleBytes=%d", n, byteCount, oracleBytes)
		}

		// Ensure byte count satisfies strict ceiling bounds:
		// (byteCount - 1) * 8 < bits <= byteCount * 8
		if int(bits) > byteCount*8 {
			t.Fatalf("n=%d: bits %d exceeds byte capacity %d", n, bits, byteCount*8)
		}
		if int(bits) <= (byteCount-1)*8 {
			t.Fatalf("n=%d: over-allocated bytes: bits %d fits in %d bytes", n, bits, byteCount-1)
		}

		f := filter.NewBloomFilter(n)
		if f == nil {
			t.Fatalf("NewBloomFilter(%d) returned nil", n)
		}
		if f.ByteSize() != byteCount {
			t.Fatalf("n=%d: f.ByteSize()=%d != byteCount=%d", n, f.ByteSize(), byteCount)
		}
	}
}

// TestBloomFilter_ZeroInputSemantics verifies that keyCount == 0 produces a valid, non-nil,
// empty Bloom filter with 0 bits and 0 bytes.
func TestBloomFilter_ZeroInputSemantics(t *testing.T) {
	bits, byteCount, ok := filter.OptimalBitsetSize(0)
	if !ok {
		t.Fatal("OptimalBitsetSize(0) returned ok=false")
	}
	if bits != 0 {
		t.Fatalf("expected 0 bits for n=0, got %d", bits)
	}
	if byteCount != 0 {
		t.Fatalf("expected 0 bytes for n=0, got %d", byteCount)
	}

	f := filter.NewBloomFilter(0)
	if f == nil {
		t.Fatal("NewBloomFilter(0) returned nil; expected valid empty filter")
	}
	if f.KeyCount() != 0 {
		t.Fatalf("KeyCount mismatch for n=0: got %d, want 0", f.KeyCount())
	}
	if f.BitCount() != 0 {
		t.Fatalf("BitCount mismatch for n=0: got %d, want 0", f.BitCount())
	}
	if f.ByteSize() != 0 {
		t.Fatalf("ByteSize mismatch for n=0: got %d, want 0", f.ByteSize())
	}
	if f.HashCount() != filter.DefaultHashFunctions {
		t.Fatalf("HashCount mismatch for n=0: got %d, want %d", f.HashCount(), filter.DefaultHashFunctions)
	}
	if len(f.Bitset()) != 0 {
		t.Fatalf("Bitset len mismatch for n=0: got %d, want 0", len(f.Bitset()))
	}
	if !f.IsEmpty() {
		t.Fatal("IsEmpty() was false for empty filter n=0")
	}
}

// TestBloomFilter_NegativeInputSemantics verifies that keyCount < 0 is rejected as invalid input,
// safely returning nil without panicking or creating invalid state.
func TestBloomFilter_NegativeInputSemantics(t *testing.T) {
	negativeInputs := []int{
		-1,
		-2,
		-7,
		-10,
		-100,
		-1000000,
		math.MinInt32,
		math.MinInt64,
		math.MinInt,
	}

	for _, n := range negativeInputs {
		bits, byteCount, ok := filter.OptimalBitsetSize(n)
		if ok {
			t.Fatalf("OptimalBitsetSize(%d) returned ok=true; expected false for negative input", n)
		}
		if bits != 0 || byteCount != 0 {
			t.Fatalf("OptimalBitsetSize(%d) returned non-zero (%d, %d)", n, bits, byteCount)
		}

		f := filter.NewBloomFilter(n)
		if f != nil {
			t.Fatalf("NewBloomFilter(%d) returned non-nil %v; expected nil for negative input", n, f)
		}
	}
}

// TestBloomFilter_IntegerBoundaryAndOverflow verifies that arithmetic multiplication and rounding
// do not silently wrap around, and impossible/pathological allocations fail closed returning nil.
func TestBloomFilter_IntegerBoundaryAndOverflow(t *testing.T) {
	t.Run("arithmetic overflow beyond math.MaxInt", func(t *testing.T) {
		overflowInputs := []int{
			math.MaxInt,
			math.MaxInt - 1,
			math.MaxInt - 7,
			math.MaxInt / 2,
			(math.MaxInt-7)/filter.BitsPerKey + 1,
			(math.MaxInt-7)/filter.BitsPerKey + 2,
		}

		for _, n := range overflowInputs {
			bits, byteCount, ok := filter.OptimalBitsetSize(n)
			if ok {
				t.Fatalf("OptimalBitsetSize(%d) returned ok=true for overflowing input; got bits=%d, bytes=%d", n, bits, byteCount)
			}

			f := filter.NewBloomFilter(n)
			if f != nil {
				t.Fatalf("NewBloomFilter(%d) returned non-nil filter for overflowing input", n)
			}
		}
	})

	t.Run("allocation boundary MaxKeyCount", func(t *testing.T) {
		// Just beyond MaxKeyCount must return nil to prevent memory exhaustion DoS
		fExceeded := filter.NewBloomFilter(filter.MaxKeyCount + 1)
		if fExceeded != nil {
			t.Fatalf("NewBloomFilter(MaxKeyCount+1) must return nil, got %v", fExceeded)
		}

		// At MaxKeyCount, calculation must succeed and match MaxBitsetBytes
		bits, bytesNeeded, ok := filter.OptimalBitsetSize(filter.MaxKeyCount)
		if !ok {
			t.Fatalf("OptimalBitsetSize(MaxKeyCount) failed")
		}
		expectedBits := uint64(filter.MaxKeyCount) * filter.BitsPerKey
		if bits != expectedBits {
			t.Fatalf("bits mismatch at MaxKeyCount: got %d, want %d", bits, expectedBits)
		}
		expectedBytes := int((expectedBits + 7) / 8)
		if bytesNeeded != expectedBytes {
			t.Fatalf("bytes mismatch at MaxKeyCount: got %d, want %d", bytesNeeded, expectedBytes)
		}
		if bytesNeeded > filter.MaxBitsetBytes {
			t.Fatalf("bytesNeeded %d exceeds MaxBitsetBytes %d", bytesNeeded, filter.MaxBitsetBytes)
		}
	})
}

// TestBloomFilter_ZeroInitialization verifies that a newly constructed valid filter contains
// strictly zero-initialized storage (no bits set).
func TestBloomFilter_ZeroInitialization(t *testing.T) {
	sizes := []int{1, 2, 7, 8, 15, 16, 17, 100, 1024, 10000}

	for _, n := range sizes {
		f := filter.NewBloomFilter(n)
		if f == nil {
			t.Fatalf("NewBloomFilter(%d) failed", n)
		}

		bitset := f.Bitset()
		zeroBlock := make([]byte, len(bitset))
		if !bytes.Equal(bitset, zeroBlock) {
			t.Fatalf("NewBloomFilter(%d) bitset was not zero-initialized", n)
		}
	}
}

// TestBloomFilter_NilReceiverSafety verifies that all getter methods on a nil *BloomFilter
// execute safely returning default zero values without panicking.
func TestBloomFilter_NilReceiverSafety(t *testing.T) {
	var f *filter.BloomFilter

	if f.KeyCount() != 0 {
		t.Fatalf("nil KeyCount mismatch: got %d, want 0", f.KeyCount())
	}
	if f.BitCount() != 0 {
		t.Fatalf("nil BitCount mismatch: got %d, want 0", f.BitCount())
	}
	if f.HashCount() != 0 {
		t.Fatalf("nil HashCount mismatch: got %d, want 0", f.HashCount())
	}
	if f.ByteSize() != 0 {
		t.Fatalf("nil ByteSize mismatch: got %d, want 0", f.ByteSize())
	}
	if f.Bitset() != nil {
		t.Fatalf("nil Bitset mismatch: got %v, want nil", f.Bitset())
	}
	if !f.IsEmpty() {
		t.Fatal("nil IsEmpty mismatch: expected true")
	}
}

// TestBloomFilter_PolicyConstants verifies that package-level constants adhere strictly
// to the architectural specification (10 bits/key, 7 hash functions).
func TestBloomFilter_PolicyConstants(t *testing.T) {
	if filter.BitsPerKey != 10 {
		t.Fatalf("BitsPerKey must be 10, got %d", filter.BitsPerKey)
	}
	if filter.DefaultHashFunctions != 7 {
		t.Fatalf("DefaultHashFunctions must be 7, got %d", filter.DefaultHashFunctions)
	}
	if filter.HashFunctions != 7 {
		t.Fatalf("HashFunctions must be 7, got %d", filter.HashFunctions)
	}
	if filter.MaxBitsetBytes != 256*1024*1024 {
		t.Fatalf("MaxBitsetBytes must be 256 MiB, got %d", filter.MaxBitsetBytes)
	}
}
