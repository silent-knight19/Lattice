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

// Add inserts a key into the Bloom filter by setting the bits corresponding to its k hash probes.
//
// Probe Generation (Kirsch-Mitzenmacher Double-Hashing):
//
//	h1, h2 = Murmur3_128(key, DefaultMurmur3Seed)
//	g_i(key) = ((h1 % m) + i * (h2 % m)) % m    for i in [0, hashCount-1]
//
// This modular reduction formulation guarantees exact mathematical values in Z/mZ and avoids
// 64-bit integer addition or multiplication overflow prior to modulo reduction.
//
// Concurrency & Safety:
//   - If the receiver is nil, this method safely returns (no-op).
//   - If the filter has zero bit capacity (f.bitCount == 0 || len(f.bitset) == 0), this is a safe no-op.
//   - Nil and empty byte keys ([]byte{}) are treated as valid binary inputs.
//   - BloomFilter is designed for single-writer construction/population followed by concurrent
//     immutable reading. Add is NOT safe for concurrent execution without external synchronization.
func (f *BloomFilter) Add(key []byte) {
	if f == nil || f.bitCount == 0 || len(f.bitset) == 0 {
		return
	}

	h1, h2 := Murmur3_128(key, DefaultMurmur3Seed)
	m := f.bitCount
	h1Mod := h1 % m
	h2Mod := h2 % m

	for i := 0; i < f.hashCount; i++ {
		probe := (h1Mod + uint64(i)*h2Mod) % m
		byteIdx := probe / 8
		bitMask := byte(1 << (probe % 8))
		if byteIdx < uint64(len(f.bitset)) {
			f.bitset[byteIdx] |= bitMask
		}
	}
}

// MayContain queries whether key might be a member of the Bloom filter.
//
// Mathematical Invariants:
//   - Zero False Negatives: If the key was previously added via Add(key) and the filter
//     has not been externally modified, MayContain is mathematically guaranteed to return true.
//   - Probabilistic False Positives: If the key was NOT added, MayContain may return true with
//     a theoretical false positive probability p ≈ 0.0082 (under 1% for 10 bits/key and k=7).
//
// Return Contract:
//   - If the receiver is nil or has zero capacity (f.bitCount == 0), returns false (empty set contains nothing).
//   - Fails fast: returns false immediately upon encountering the first probe whose bit is 0.
//   - Returns true if and only if all k probe bits are 1.
//
// Performance:
//   - Executes in O(k) time (at most 7 bit probes).
//   - Zero heap allocations (0 B/op).
//
// Concurrency:
//   - MayContain is safe for concurrent read access across multiple goroutines once construction is complete.
func (f *BloomFilter) MayContain(key []byte) bool {
	if f == nil || f.bitCount == 0 || len(f.bitset) == 0 {
		return false
	}

	h1, h2 := Murmur3_128(key, DefaultMurmur3Seed)
	m := f.bitCount
	h1Mod := h1 % m
	h2Mod := h2 % m

	for i := 0; i < f.hashCount; i++ {
		probe := (h1Mod + uint64(i)*h2Mod) % m
		byteIdx := probe / 8
		bitMask := byte(1 << (probe % 8))
		if byteIdx >= uint64(len(f.bitset)) || (f.bitset[byteIdx]&bitMask) == 0 {
			return false
		}
	}
	return true
}

// Probes returns the k bit-indices generated for key in this BloomFilter.
// This method is provided for diagnostics, testing, and mathematical verification.
// If the receiver is nil or has zero bit capacity, it returns nil.
func (f *BloomFilter) Probes(key []byte) []uint64 {
	if f == nil || f.bitCount == 0 {
		return nil
	}

	h1, h2 := Murmur3_128(key, DefaultMurmur3Seed)
	m := f.bitCount
	h1Mod := h1 % m
	h2Mod := h2 % m

	probes := make([]uint64, f.hashCount)
	for i := 0; i < f.hashCount; i++ {
		probes[i] = (h1Mod + uint64(i)*h2Mod) % m
	}
	return probes
}
