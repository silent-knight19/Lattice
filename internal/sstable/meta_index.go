package sstable

import (
	"sort"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

const (
	// MetaIndexTrailerSize is the serialized length of the trailing entry count and CRC32 fields.
	// 4 bytes (Entry Count, uint32 Big-Endian) + 4 bytes (CRC32-IEEE, uint32 Big-Endian) = 8 bytes.
	MetaIndexTrailerSize = 8
)

// BuildMetaIndexBlock serializes an SSTable MetaIndex block from a map of metadata keys to BlockHandles.
//
// In Lattice's SSTable format (ADR-004), the MetaIndex block records pointers to auxiliary
// metadata regions, such as the Bloom filter block ("filter.bloom") and table properties.
//
// Format:
//
//	+-----------------------------------------------------------+
//	| Entry Data Region                                         |
//	|   Entry 0: KeyLen (varint), KeyBytes, BlockHandle (16B)   |
//	|   Entry 1: KeyLen (varint), KeyBytes, BlockHandle (16B)   |
//	|   ...                                                     |
//	+-----------------------------------------------------------+
//	| Entry Offsets Region (uint32 * N, Big-Endian)             |
//	+-----------------------------------------------------------+
//	| Entry Count (uint32, Big-Endian)                          |
//	+-----------------------------------------------------------+
//	| CRC32-IEEE  (uint32, Big-Endian)                          |
//	+-----------------------------------------------------------+
//
// Invariants:
//   - Determinism: Keys are sorted lexicographically so identical entries produce byte-identical output.
//   - Empty representation: When len(entries) == 0, produces an exact 8-byte block (entryCount=0 + CRC32).
func BuildMetaIndexBlock(entries map[string]BlockHandle) []byte {
	if len(entries) == 0 {
		return emptyMetaIndexBlock()
	}

	// Sort keys for deterministic serialization
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf []byte
	offsets := make([]uint32, 0, len(keys))
	var varintBuf [binary.MaxVarintLen64]byte

	for _, k := range keys {
		handle := entries[k]
		currentOffset := uint32(len(buf))
		offsets = append(offsets, currentOffset)

		n := binary.PutVarint64(varintBuf[:], uint64(len(k)))
		buf = append(buf, varintBuf[:n]...)
		buf = append(buf, k...)
		buf = handle.AppendTo(buf)
	}

	// Append offset array
	var u32Buf [4]byte
	for _, off := range offsets {
		binary.PutUint32(u32Buf[:], off)
		buf = append(buf, u32Buf[:]...)
	}

	// Append entry count
	binary.PutUint32(u32Buf[:], uint32(len(keys)))
	buf = append(buf, u32Buf[:]...)

	// Append CRC32-IEEE
	crc := binary.Checksum(buf)
	binary.PutUint32(u32Buf[:], crc)
	buf = append(buf, u32Buf[:]...)

	out := make([]byte, len(buf))
	copy(out, buf)
	return out
}

// DecodeMetaIndexBlock parses and validates an SSTable MetaIndex block from raw bytes.
//
// Validation Contract:
//   - If len(data) == 0: returns empty map, nil error.
//   - If len(data) < MetaIndexTrailerSize (8 bytes): returns errors.ErrIndexBlockTruncated.
//   - Verifies CRC32-IEEE checksum over all preceding bytes. Returns *errors.ChecksumMismatchError on mismatch.
//   - Verifies entry count does not exceed buffer capacity.
//   - Verifies entry offsets are strictly monotonic, within bounds, and offsets[0] == 0 for non-empty blocks.
//   - Parses every entry, validating varint key lengths, key bytes, and 16-byte BlockHandle bounds.
func DecodeMetaIndexBlock(data []byte) (map[string]BlockHandle, error) {
	if len(data) == 0 {
		return make(map[string]BlockHandle), nil
	}
	if len(data) < MetaIndexTrailerSize {
		return nil, errors.ErrIndexBlockTruncated
	}

	// 1. Verify CRC32-IEEE checksum
	checksumOffset := len(data) - 4
	expectedCRC := binary.GetUint32(data[checksumOffset:])
	actualCRC := binary.Checksum(data[:checksumOffset])
	if expectedCRC != actualCRC {
		return nil, &errors.ChecksumMismatchError{
			Offset:   int64(checksumOffset),
			Expected: expectedCRC,
			Actual:   actualCRC,
		}
	}

	// 2. Read Entry Count
	entryCount := binary.GetUint32(data[len(data)-8 : len(data)-4])
	if entryCount == 0 {
		if len(data) != MetaIndexTrailerSize {
			return nil, &errors.IndexBlockCorruptedError{Reason: "non-empty metaindex block with zero entry count"}
		}
		return make(map[string]BlockHandle), nil
	}

	// 3. Verify offsets bounds
	offsetsByteLen := uint64(entryCount) * 4
	if offsetsByteLen+uint64(MetaIndexTrailerSize) > uint64(len(data)) {
		return nil, &errors.IndexBlockCorruptedError{Reason: "metaindex entry count exceeds block capacity"}
	}

	offsetsStart := len(data) - MetaIndexTrailerSize - int(offsetsByteLen)

	// 4. Parse offsets
	offsets := make([]uint32, entryCount)
	for i := 0; i < int(entryCount); i++ {
		pos := offsetsStart + (i * 4)
		offsets[i] = binary.GetUint32(data[pos : pos+4])

		if i == 0 && offsets[0] != 0 {
			return nil, &errors.IndexBlockCorruptedError{Reason: "first metaindex entry offset is not zero"}
		}
		if i > 0 && offsets[i] <= offsets[i-1] {
			return nil, &errors.IndexBlockCorruptedError{Reason: "metaindex entry offsets are not strictly increasing"}
		}
		if uint64(offsets[i]) >= uint64(offsetsStart) {
			return nil, &errors.IndexBlockCorruptedError{Reason: "metaindex entry offset exceeds data region boundary"}
		}
	}

	// 5. Decode entries
	result := make(map[string]BlockHandle, entryCount)
	for i := 0; i < int(entryCount); i++ {
		start := int(offsets[i])
		var end int
		if i+1 < int(entryCount) {
			end = int(offsets[i+1])
		} else {
			end = offsetsStart
		}

		if start >= end || end > offsetsStart {
			return nil, &errors.IndexBlockCorruptedError{Reason: "invalid metaindex entry slice boundaries"}
		}

		entrySlice := data[start:end]

		// Decode varint KeyLen
		keyLen, varintLen, err := binary.GetVarint64(entrySlice)
		if err != nil {
			return nil, &errors.IndexBlockCorruptedError{Reason: "corrupted metaindex key length varint"}
		}

		// 1. Validate key length domain before arithmetic or conversion (eliminates integer overflow & zero-length keys)
		if keyLen == 0 || keyLen > binary.MaxEncodedInternalKeyLen {
			return nil, &errors.IndexBlockCorruptedError{Reason: "metaindex key length outside valid boundaries"}
		}

		// 2. Ensure entrySlice can accommodate at least the varint header and BlockHandle trailer
		minEntryLen := varintLen + BlockHandleSize
		if len(entrySlice) < minEntryLen {
			return nil, &errors.IndexBlockCorruptedError{Reason: "metaindex entry buffer too small for header and handle"}
		}

		// 3. Overflow-safe length validation: verify keyLen matches available payload bytes via subtraction
		remainingForKey := uint64(len(entrySlice) - minEntryLen)
		if keyLen != remainingForKey {
			return nil, &errors.IndexBlockCorruptedError{Reason: "metaindex entry size mismatch"}
		}

		// 4. Bounded integer conversion: keyLen is proven in [1, len(entrySlice)-minEntryLen]
		keyLenInt := int(keyLen)
		keyStart := varintLen
		keyEnd := keyStart + keyLenInt
		keyStr := string(entrySlice[keyStart:keyEnd])

		handleSlice := entrySlice[keyEnd : keyEnd+BlockHandleSize]
		handle, err := DecodeBlockHandle(handleSlice)
		if err != nil {
			return nil, err
		}

		result[keyStr] = handle
	}

	return result, nil
}

// FindMetaIndexEntry parses data and searches for the BlockHandle corresponding to targetKey.
// If the key is found, returns (handle, true, nil).
// If the key is absent, returns (BlockHandle{}, false, nil).
// If the block is malformed or corrupted, returns an error (fail-closed).
func FindMetaIndexEntry(data []byte, targetKey string) (BlockHandle, bool, error) {
	entries, err := DecodeMetaIndexBlock(data)
	if err != nil {
		return BlockHandle{}, false, err
	}
	handle, found := entries[targetKey]
	return handle, found, nil
}
