package filter

import (
	"math"
)

const (
	// BitsPerKey is the fixed project policy for Bloom filter sizing: 10 bits per key (m/n = 10).
	// Combined with k = 7 hash functions, this yields a theoretical false positive probability
	// of approximately p ≈ 0.0082 (under 1%).
	BitsPerKey = 10

	// DefaultHashFunctions is the fixed project policy for the Bloom filter probe count (k = 7).
	DefaultHashFunctions = 7

	// HashFunctions is an alias for DefaultHashFunctions, matching storage engine terminology.
	HashFunctions = DefaultHashFunctions

	// MaxBitsetBytes is the maximum physical byte size permitted for an in-memory Bloom filter (256 MiB).
	// This bounds heap allocations to prevent denial-of-service memory exhaustion attacks while easily
	// accommodating up to ~209.7 million keys per SSTable filter block.
	MaxBitsetBytes = 256 * 1024 * 1024

	// MaxKeyCount is the maximum key count supported for physical Bloom filter allocation.
	// Calculated as (MaxBitsetBytes * 8) / BitsPerKey = 209,715,200 keys.
	MaxKeyCount = (MaxBitsetBytes * 8) / BitsPerKey
)

// BloomFilter represents a space-efficient probabilistic data structure
// allocated according to the project sizing policy (10 bits/key, 7 hash functions).
//
// In Lattice's LSM-Tree storage architecture, Bloom filters eliminate unnecessary disk reads
// by testing set membership in memory prior to searching SSTable block indexes and data blocks.
type BloomFilter struct {
	keyCount  int
	bitCount  uint64
	hashCount int
	bitset    []byte
}

// OptimalBitsetSize computes the bit count and byte count required for keyCount keys
// according to the Bloom filter mathematical sizing policy:
//
//	m = keyCount * BitsPerKey
//	bytes = ceil(m / 8) = (m + 7) / 8
//
// Return Contract:
//   - If keyCount < 0: returns (0, 0, false) as negative key cardinality is invalid.
//   - If keyCount == 0: returns (0, 0, true) for a valid empty filter.
//   - If keyCount > 0: verifies that keyCount * BitsPerKey does not overflow integer bounds.
//     If multiplication or rounding would overflow math.MaxInt, returns (0, 0, false).
//   - Otherwise: returns (bitCount, byteCount, true).
//
// This function operates purely on sizing arithmetic without allocating heap memory.
func OptimalBitsetSize(keyCount int) (uint64, int, bool) {
	if keyCount < 0 {
		return 0, 0, false
	}
	if keyCount == 0 {
		return 0, 0, true
	}

	// Guard against integer multiplication and rounding overflow.
	// Enforce keyCount <= (math.MaxInt - 7) / BitsPerKey.
	if keyCount > (math.MaxInt-7)/BitsPerKey {
		return 0, 0, false
	}

	bits := uint64(keyCount) * uint64(BitsPerKey)
	bytes := int((bits + 7) / 8)

	return bits, bytes, true
}

// NewBloomFilter allocates a new BloomFilter sized for keyCount expected keys.
//
// Mathematical Sizing Policy:
//   - BitsPerKey = 10
//   - HashFunctions = 7
//   - bitCount = keyCount * 10
//   - byteCount = ceil(bitCount / 8) = (bitCount + 7) / 8
//
// Input Semantics & Safety Boundaries:
//   - If keyCount < 0: returns nil (negative key cardinality is invalid; fail-closed).
//   - If keyCount == 0: returns a valid empty BloomFilter with 0 bits, 0 bytes, and 7 hash functions.
//   - If keyCount > MaxKeyCount (~209.7M keys): returns nil to prevent memory exhaustion DoS and
//     guarantee integer arithmetic bounds.
//   - For valid keyCount in [0, MaxKeyCount]: allocates a zero-initialized byte slice of length
//     byteCount, ensuring the filter represents an empty set where no bits are set.
//
// Concurrency:
// Construction is thread-safe and contains no shared mutable state.
func NewBloomFilter(keyCount int) *BloomFilter {
	if keyCount < 0 {
		return nil
	}
	if keyCount == 0 {
		return &BloomFilter{
			keyCount:  0,
			bitCount:  0,
			hashCount: DefaultHashFunctions,
			bitset:    make([]byte, 0),
		}
	}
	if keyCount > MaxKeyCount {
		return nil
	}

	bits, bytes, ok := OptimalBitsetSize(keyCount)
	if !ok {
		return nil
	}

	return &BloomFilter{
		keyCount:  keyCount,
		bitCount:  bits,
		hashCount: DefaultHashFunctions,
		bitset:    make([]byte, bytes),
	}
}

// KeyCount returns the expected number of keys configured when this filter was allocated.
// Returns 0 if the receiver is nil.
func (f *BloomFilter) KeyCount() int {
	if f == nil {
		return 0
	}
	return f.keyCount
}

// BitCount returns the number of bits allocated in the Bloom filter bitset (m = n * 10).
// Returns 0 if the receiver is nil.
func (f *BloomFilter) BitCount() uint64 {
	if f == nil {
		return 0
	}
	return f.bitCount
}

// HashCount returns the number of hash functions configured for this filter (k = 7).
// Returns 0 if the receiver is nil.
func (f *BloomFilter) HashCount() int {
	if f == nil {
		return 0
	}
	return f.hashCount
}

// ByteSize returns the physical byte length of the allocated bitset buffer (len(bitset)).
// Returns 0 if the receiver is nil.
func (f *BloomFilter) ByteSize() int {
	if f == nil {
		return 0
	}
	return len(f.bitset)
}

// Bitset returns a slice to the underlying bitset storage.
// Returns nil if the receiver is nil.
func (f *BloomFilter) Bitset() []byte {
	if f == nil {
		return nil
	}
	return f.bitset
}

// IsEmpty reports whether the filter represents an empty set (zero bits or nil receiver).
func (f *BloomFilter) IsEmpty() bool {
	if f == nil {
		return true
	}
	return f.bitCount == 0
}
