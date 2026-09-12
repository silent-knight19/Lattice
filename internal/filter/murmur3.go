package filter

import (
	"encoding/binary"
	"math/bits"
)

const (
	// DefaultMurmur3Seed is the canonical default 64-bit seed (0) for MurmurHash3_x64_128.
	DefaultMurmur3Seed uint64 = 0

	// Murmur3 constants per Austin Appleby's SMHasher reference.
	c1Murmur3 = 0x87c37b91114253d5
	c2Murmur3 = 0x4cf5ad432745937f
)

// fmix64 forces all bits of a 64-bit hash block to avalanche.
func fmix64(k uint64) uint64 {
	k ^= k >> 33
	k *= 0xff51afd7ed558ccd
	k ^= k >> 33
	k *= 0xc4ceb9fe1a85ec53
	k ^= k >> 33
	return k
}

// Murmur3_128 computes the 128-bit hash of data using the canonical MurmurHash3_x64_128 algorithm.
// It returns two 64-bit unsigned integers (h1, h2) derived from the input byte sequence and seed.
//
// Algorithmic Properties:
//   - Canonical Reference: Matches Austin Appleby's reference C++ implementation (SMHasher).
//   - Byte Order: Explicitly little-endian for 64-bit word reads; platform- and architecture-independent.
//   - Binary Safety: Operates on opaque byte sequences; safe for nil, empty, embedded-null, and arbitrary high-bit bytes.
//   - Determinism: Repeated invocations on identical byte sequences yield bitwise identical (h1, h2).
//   - Allocation: Inlines hot paths and executes with zero heap allocations (0 B/op).
func Murmur3_128(data []byte, seed uint64) (uint64, uint64) {
	length := len(data)
	nblocks := length / 16

	h1 := seed
	h2 := seed

	// Body: process 16-byte (128-bit) blocks
	for i := 0; i < nblocks; i++ {
		offset := i * 16
		k1 := binary.LittleEndian.Uint64(data[offset : offset+8])
		k2 := binary.LittleEndian.Uint64(data[offset+8 : offset+16])

		k1 *= c1Murmur3
		k1 = bits.RotateLeft64(k1, 31)
		k1 *= c2Murmur3
		h1 ^= k1

		h1 = bits.RotateLeft64(h1, 27)
		h1 += h2
		h1 = h1*5 + 0x52dce729

		k2 *= c2Murmur3
		k2 = bits.RotateLeft64(k2, 33)
		k2 *= c1Murmur3
		h2 ^= k2

		h2 = bits.RotateLeft64(h2, 31)
		h2 += h1
		h2 = h2*5 + 0x38495ab5
	}

	// Tail: process remaining 1 to 15 bytes in little-endian order
	tail := data[nblocks*16:]
	var k1, k2 uint64

	switch len(tail) {
	case 15:
		k2 ^= uint64(tail[14]) << 48
		fallthrough
	case 14:
		k2 ^= uint64(tail[13]) << 40
		fallthrough
	case 13:
		k2 ^= uint64(tail[12]) << 32
		fallthrough
	case 12:
		k2 ^= uint64(tail[11]) << 24
		fallthrough
	case 11:
		k2 ^= uint64(tail[10]) << 16
		fallthrough
	case 10:
		k2 ^= uint64(tail[9]) << 8
		fallthrough
	case 9:
		k2 ^= uint64(tail[8])
		k2 *= c2Murmur3
		k2 = bits.RotateLeft64(k2, 33)
		k2 *= c1Murmur3
		h2 ^= k2
		fallthrough
	case 8:
		k1 ^= uint64(tail[7]) << 56
		fallthrough
	case 7:
		k1 ^= uint64(tail[6]) << 48
		fallthrough
	case 6:
		k1 ^= uint64(tail[5]) << 40
		fallthrough
	case 5:
		k1 ^= uint64(tail[4]) << 32
		fallthrough
	case 4:
		k1 ^= uint64(tail[3]) << 24
		fallthrough
	case 3:
		k1 ^= uint64(tail[2]) << 16
		fallthrough
	case 2:
		k1 ^= uint64(tail[1]) << 8
		fallthrough
	case 1:
		k1 ^= uint64(tail[0])
		k1 *= c1Murmur3
		k1 = bits.RotateLeft64(k1, 31)
		k1 *= c2Murmur3
		h1 ^= k1
	}

	// Finalization: mix lengths and avalanche bits
	h1 ^= uint64(length)
	h2 ^= uint64(length)

	h1 += h2
	h2 += h1

	h1 = fmix64(h1)
	h2 = fmix64(h2)

	h1 += h2
	h2 += h1

	return h1, h2
}
