package binary

import (
	"hash/crc32"
)

// Checksum computes the 32-bit Cyclic Redundancy Check (CRC32) checksum of data
// using the standard IEEE 802.3 polynomial (0xEDB88320).
//
// Properties:
//   - Concurrency-safe: contains no mutable shared state.
//   - Zero heap allocations: executes in-place without memory allocation.
//   - Non-mutating: does not modify the provided byte slice.
//   - Deterministic: returns identical checksums across all CPU architectures.
func Checksum(data []byte) uint32 {
	return crc32.ChecksumIEEE(data)
}

// Verify calculates the CRC32-IEEE checksum of data and checks whether it matches
// expected. It returns true if and only if the calculated checksum is identical to
// expected.
//
// Properties:
//   - Concurrency-safe: contains no mutable shared state.
//   - Zero heap allocations: executes in-place without memory allocation.
//   - Non-mutating: does not modify the provided byte slice.
func Verify(data []byte, expected uint32) bool {
	return crc32.ChecksumIEEE(data) == expected
}
