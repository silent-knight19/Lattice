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

// TestBloomFilter_ZeroFalseNegatives_10K verifies the fundamental Bloom filter invariant:
// Every inserted key MUST return MayContain(k) == true (zero false negatives).
func TestBloomFilter_ZeroFalseNegatives_10K(t *testing.T) {
	const n = 10000
	f := filter.NewBloomFilter(n)
	if f == nil {
		t.Fatalf("NewBloomFilter(%d) failed", n)
	}

	keys := make([][]byte, n)
	for i := 0; i < n; i++ {
		keys[i] = []byte(fmt.Sprintf("lattice_user_key_%08d", i))
		f.Add(keys[i])
	}

	for i, key := range keys {
		if !f.MayContain(key) {
			t.Fatalf("false negative detected at key index %d (%q)", i, key)
		}
	}
}

// TestBloomFilter_ProbePositions_InBounds verifies that all generated probes satisfy
// 0 <= probe < bitCount and map to byte indices within len(bitset).
func TestBloomFilter_ProbePositions_InBounds(t *testing.T) {
	cardinalities := []int{1, 2, 7, 8, 10, 16, 100, 1000}

	for _, n := range cardinalities {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			f := filter.NewBloomFilter(n)
			if f == nil {
				t.Fatalf("NewBloomFilter(%d) returned nil", n)
			}

			for i := 0; i < 50; i++ {
				key := []byte(fmt.Sprintf("probe_test_key_%d_%d", n, i))
				probes := f.Probes(key)
				if len(probes) != filter.DefaultHashFunctions {
					t.Fatalf("probes count mismatch: got %d, want %d", len(probes), filter.DefaultHashFunctions)
				}

				for pIdx, probe := range probes {
					if probe >= f.BitCount() {
						t.Fatalf("probe %d (%d) exceeds bitCount (%d)", pIdx, probe, f.BitCount())
					}
					byteIdx := probe / 8
					if byteIdx >= uint64(f.ByteSize()) {
						t.Fatalf("probe %d byteIdx %d exceeds ByteSize %d", pIdx, byteIdx, f.ByteSize())
					}
				}
			}
		})
	}
}

// TestBloomFilter_ByteBoundaryBitPositions verifies that bit indices landing on byte boundaries
// (bit 0, bit 7, bit 8, bit 9, and the last valid bit) are set and tested accurately without off-by-one errors.
func TestBloomFilter_ByteBoundaryBitPositions(t *testing.T) {
	f := filter.NewBloomFilter(10) // 100 bits, 13 bytes
	if f == nil {
		t.Fatal("NewBloomFilter(10) failed")
	}

	bitset := f.Bitset()
	lastBit := f.BitCount() - 1 // bit 99

	// Direct bit test via bitset verification
	testBits := []uint64{0, 7, 8, 9, 15, 16, lastBit}
	for _, bit := range testBits {
		byteIdx := bit / 8
		bitMask := byte(1 << (bit % 8))

		// Bit starts unset
		if (bitset[byteIdx] & bitMask) != 0 {
			t.Fatalf("bit %d initially set", bit)
		}

		// Manually set bit
		bitset[byteIdx] |= bitMask

		// Verify bit is now set
		if (bitset[byteIdx] & bitMask) == 0 {
			t.Fatalf("bit %d not set after manual set", bit)
		}
	}
}

// TestBloomFilter_AddIdempotence verifies that adding the same key multiple times
// leaves the underlying bitset completely unchanged.
func TestBloomFilter_AddIdempotence(t *testing.T) {
	f1 := filter.NewBloomFilter(1000)
	f2 := filter.NewBloomFilter(1000)

	key := []byte("idempotent_test_key_12345")

	f1.Add(key)

	f2.Add(key)
	f2.Add(key)
	f2.Add(key)

	if !bytes.Equal(f1.Bitset(), f2.Bitset()) {
		t.Fatal("Add is not idempotent; multiple additions altered the bitset")
	}
}

// TestBloomFilter_InsertionOrderIndependence verifies that Bloom filter state
// is independent of the order in which keys are inserted (set semantics).
func TestBloomFilter_InsertionOrderIndependence(t *testing.T) {
	f1 := filter.NewBloomFilter(1000)
	f2 := filter.NewBloomFilter(1000)

	keys := [][]byte{
		[]byte("alpha"),
		[]byte("beta"),
		[]byte("gamma"),
		[]byte("delta"),
		[]byte("epsilon"),
	}

	// Insert in forward order in f1
	for _, k := range keys {
		f1.Add(k)
	}

	// Insert in reverse order in f2
	for i := len(keys) - 1; i >= 0; i-- {
		f2.Add(keys[i])
	}

	if !bytes.Equal(f1.Bitset(), f2.Bitset()) {
		t.Fatal("bitsets differ when inserting same keys in different order")
	}
}

// TestBloomFilter_CrossFilterIsolation verifies that two independently constructed filters
// do not share internal storage or leak state across instances.
func TestBloomFilter_CrossFilterIsolation(t *testing.T) {
	f1 := filter.NewBloomFilter(100)
	f2 := filter.NewBloomFilter(100)

	key := []byte("isolated_key")
	f1.Add(key)

	if !f1.MayContain(key) {
		t.Fatal("f1 should contain inserted key")
	}

	// f2 was never modified; must remain all zeros
	zeroBlock := make([]byte, f2.ByteSize())
	if !bytes.Equal(f2.Bitset(), zeroBlock) {
		t.Fatal("f2 bitset was modified by action on f1")
	}
	if f2.MayContain(key) {
		t.Fatal("empty f2 returned true on MayContain")
	}
}

// TestBloomFilter_EmptyFilterSemantics verifies that a zero-capacity filter (n=0)
// and a nil receiver handle Add, MayContain, and Probes safely without panicking.
func TestBloomFilter_EmptyFilterSemantics(t *testing.T) {
	t.Run("zero capacity filter n=0", func(t *testing.T) {
		f := filter.NewBloomFilter(0)
		if f == nil {
			t.Fatal("NewBloomFilter(0) returned nil")
		}

		// Add must be a safe no-op
		f.Add([]byte("test_key"))
		f.Add(nil)

		// MayContain must return false (empty set contains nothing)
		if f.MayContain([]byte("test_key")) {
			t.Fatal("empty filter MayContain returned true; want false")
		}
		if f.MayContain(nil) {
			t.Fatal("empty filter MayContain(nil) returned true; want false")
		}

		if f.Probes([]byte("test_key")) != nil {
			t.Fatal("empty filter Probes must return nil")
		}
	})

	t.Run("nil receiver safety", func(t *testing.T) {
		var fNil *filter.BloomFilter

		// Add on nil receiver must not panic
		fNil.Add([]byte("test_key"))
		fNil.Add(nil)

		// MayContain on nil receiver must return false
		if fNil.MayContain([]byte("test_key")) {
			t.Fatal("nil filter MayContain returned true; want false")
		}
		if fNil.MayContain(nil) {
			t.Fatal("nil filter MayContain(nil) returned true; want false")
		}

		// Probes on nil receiver must return nil
		if fNil.Probes([]byte("test_key")) != nil {
			t.Fatal("nil filter Probes must return nil")
		}
	})
}

// TestBloomFilter_NilAndEmptyKeySemantics verifies that nil and empty byte slices
// are treated as valid binary keys and do not cause errors.
func TestBloomFilter_NilAndEmptyKeySemantics(t *testing.T) {
	f := filter.NewBloomFilter(100)
	if f == nil {
		t.Fatal("NewBloomFilter(100) failed")
	}

	f.Add(nil)
	if !f.MayContain(nil) {
		t.Fatal("MayContain(nil) returned false after Add(nil)")
	}
	if !f.MayContain([]byte{}) {
		t.Fatal("MayContain([]byte{}) returned false after Add(nil)")
	}

	// An absent key should still return false with high probability
	if f.MayContain([]byte("some_non_empty_absent_key")) {
		t.Log("Note: unexpected hit on absent key (permitted by false-positive bound)")
	}
}

// TestBloomFilter_SanityCheck_AbsentKeysRejected provides a bounded sanity check
// demonstrating that absent keys are rejected with high probability (filter is not saturated).
func TestBloomFilter_SanityCheck_AbsentKeysRejected(t *testing.T) {
	const n = 1000
	f := filter.NewBloomFilter(n)
	if f == nil {
		t.Fatalf("NewBloomFilter(%d) failed", n)
	}

	// Insert 1000 keys
	for i := 0; i < n; i++ {
		f.Add([]byte(fmt.Sprintf("present_key_%06d", i)))
	}

	// Query 1000 distinct absent keys
	falsePositives := 0
	for i := 0; i < n; i++ {
		if f.MayContain([]byte(fmt.Sprintf("absent_key_%06d", i))) {
			falsePositives++
		}
	}

	// Theoretical FPR for 10 bits/key, k=7 is ~0.82%. Out of 1000 queries, expect ~8 false positives.
	// We assert that the filter is non-saturated and has rejected at least 95% of absent keys (FPR < 5%).
	if falsePositives > 50 {
		t.Fatalf("excessive false positives: %d/1000 (>5%%); expected ~8", falsePositives)
	}
	t.Logf("Sanity check passed: %d false positives out of %d absent queries (observed rate: %.2f%%)",
		falsePositives, n, float64(falsePositives)/float64(n)*100.0)
}

// TestBloomFilter_ZeroAllocations_Membership verifies that Add and MayContain
// execute with zero heap allocations.
func TestBloomFilter_ZeroAllocations_Membership(t *testing.T) {
	f := filter.NewBloomFilter(1000)
	key := []byte("hot_path_key")
	f.Add(key)

	allocsAdd := testing.AllocsPerRun(1000, func() {
		f.Add(key)
	})
	if allocsAdd > 0 {
		t.Fatalf("f.Add allocated %f objects/op; want 0", allocsAdd)
	}

	allocsMayContain := testing.AllocsPerRun(1000, func() {
		_ = f.MayContain(key)
	})
	if allocsMayContain > 0 {
		t.Fatalf("f.MayContain allocated %f objects/op; want 0", allocsMayContain)
	}
}
