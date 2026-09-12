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

// FuzzMurmur3_128 verifies that Murmur3_128 safely processes arbitrary binary payloads
// without panics, memory faults, or non-deterministic variance.
func FuzzMurmur3_128(f *testing.F) {
	seeds := [][]byte{
		{},
		[]byte("a"),
		[]byte("test"),
		[]byte("hello"),
		[]byte("The quick brown fox jumps over the lazy dog"),
		make([]byte, 16),
		make([]byte, 17),
		make([]byte, 31),
		make([]byte, 32),
		make([]byte, 64),
	}
	for _, seed := range seeds {
		f.Add(seed, uint64(0))
		f.Add(seed, uint64(42))
	}

	f.Fuzz(func(t *testing.T, data []byte, seed uint64) {
		h1A, h2A := filter.Murmur3_128(data, seed)
		h1B, h2B := filter.Murmur3_128(data, seed)

		if h1A != h1B || h2A != h2B {
			t.Fatalf("determinism failure: (%x, %x) != (%x, %x)", h1A, h2A, h1B, h2B)
		}
	})
}

// FuzzBloomFilter_AddMayContain verifies the invariant that for any arbitrary key and any
// valid bounded filter size, Add(key) followed by MayContain(key) NEVER produces a false negative.
func FuzzBloomFilter_AddMayContain(f *testing.F) {
	keys := [][]byte{
		{},
		[]byte("key"),
		[]byte("longer_user_key_string"),
		[]byte("\x00\x00\x00"),
		[]byte("\xff\xff\xff"),
	}
	capacities := []int{0, 1, 2, 7, 8, 10, 100, 1000}

	for _, k := range keys {
		for _, c := range capacities {
			f.Add(k, c)
		}
	}

	f.Fuzz(func(t *testing.T, key []byte, rawN int) {
		// Bound filter sizing to [0, 5000] for memory safety during fuzzing
		if rawN < 0 || rawN > 5000 {
			return
		}

		filterObj := filter.NewBloomFilter(rawN)
		if filterObj == nil {
			t.Fatalf("NewBloomFilter(%d) unexpectedly returned nil", rawN)
		}

		filterObj.Add(key)

		// If filter has 0 bits (rawN == 0), MayContain must return false
		if rawN == 0 {
			if filterObj.MayContain(key) {
				t.Fatalf("empty filter (n=0) MayContain returned true; want false")
			}
			return
		}

		// Non-empty filter MUST contain the inserted key (zero false negatives)
		if !filterObj.MayContain(key) {
			t.Fatalf("false negative detected for n=%d, key=%q", rawN, key)
		}
	})
}

// FuzzFilterBlockCodec tests parser robustness against arbitrary malformed byte slices
// and verifies that DecodeFilterBlock never panics or hangs.
func FuzzFilterBlockCodec(f *testing.F) {
	// Seed corpus: empty filter, valid small filter, corrupted trailers, random bytes
	emptyFilter := filter.NewBloomFilter(0)
	f.Add(emptyFilter.Encode())

	smallFilter := filter.NewBloomFilter(5)
	smallFilter.Add([]byte("hello"))
	f.Add(smallFilter.Encode())

	f.Add([]byte{})
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12})
	f.Add(make([]byte, 13))
	f.Add(make([]byte, 32))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Enforce safety cap on fuzz slice length to prevent excessive memory usage
		if len(data) > 65536 {
			return
		}

		decoded, err := filter.DecodeFilterBlock(data)
		if err != nil {
			// Expected for malformed input: fail-closed with error
			if decoded != nil {
				t.Fatalf("expected nil filter on error, got %v", decoded)
			}
			return
		}

		// If decode succeeded:
		// 1. Re-encoding must not panic and must produce identical bytes
		reEncoded := decoded.Encode()
		if len(reEncoded) != len(data) {
			t.Fatalf("re-encoded length mismatch: got %d, want %d", len(reEncoded), len(data))
		}

		// 2. Decode again must succeed
		reDecoded, err := filter.DecodeFilterBlock(reEncoded)
		if err != nil {
			t.Fatalf("second decode failed: %v", err)
		}
		if reDecoded.BitCount() != decoded.BitCount() {
			t.Fatalf("bitCount mismatch across re-decode")
		}
	})
}
