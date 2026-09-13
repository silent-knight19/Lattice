package sstable_test

import (
	"math"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// FuzzMetaIndexBlock_Decode exercises DecodeMetaIndexBlock with arbitrary raw byte sequences,
// crafted integer overflow boundaries, corrupted trailers, and malformed varints.
//
// Invariant: DecodeMetaIndexBlock must never panic under any untrusted input.
// It must either return a valid decoded map or a structured error.
func FuzzMetaIndexBlock_Decode(f *testing.F) {
	// Seed 1: Empty block
	f.Add([]byte{})

	// Seed 2: Truncated blocks
	f.Add([]byte{0x00})
	f.Add(make([]byte, sstable.MetaIndexTrailerSize-1))
	f.Add(make([]byte, sstable.MetaIndexTrailerSize))

	// Seed 3: Valid BuildMetaIndexBlock output (empty map)
	f.Add(sstable.BuildMetaIndexBlock(map[string]sstable.BlockHandle{}))

	// Seed 4: Valid BuildMetaIndexBlock output (single entry)
	f.Add(sstable.BuildMetaIndexBlock(map[string]sstable.BlockHandle{
		"filter.lattice.default": {Offset: 4096, Size: 1024},
	}))

	// Seed 5: Valid BuildMetaIndexBlock output (multiple entries)
	f.Add(sstable.BuildMetaIndexBlock(map[string]sstable.BlockHandle{
		"filter.bloom.1": {Offset: 100, Size: 200},
		"filter.bloom.2": {Offset: 300, Size: 400},
		"stats.meta":     {Offset: 700, Size: 50},
	}))

	// Seed 6: SEC-001 crafted exploit input (keyLen = 2^64 - 6)
	craftedVarint := []byte{0xfa, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}
	entrySlice := make([]byte, 20)
	copy(entrySlice, craftedVarint)
	f.Add(buildMaliciousMetaIndexBlock(entrySlice))

	// Seed 7: MaxUint64 keyLen
	maxUintVarint := encodeVarint64Raw(math.MaxUint64)
	entryMaxUint := make([]byte, len(maxUintVarint)+16)
	copy(entryMaxUint, maxUintVarint)
	f.Add(buildMaliciousMetaIndexBlock(entryMaxUint))

	// Seed 8: Zero keyLen
	zeroVarint := encodeVarint64Raw(0)
	entryZero := make([]byte, len(zeroVarint)+16)
	copy(entryZero, zeroVarint)
	f.Add(buildMaliciousMetaIndexBlock(entryZero))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Invariant: Must never panic
		entries, err := sstable.DecodeMetaIndexBlock(data)
		if err != nil {
			return
		}

		// If decoding succeeded, verify entry structural invariants
		for k, handle := range entries {
			if len(k) == 0 {
				t.Fatalf("DecodeMetaIndexBlock returned empty key on valid result")
			}
			if len(k) > binary.MaxEncodedInternalKeyLen {
				t.Fatalf("DecodeMetaIndexBlock returned key length %d > max %d", len(k), binary.MaxEncodedInternalKeyLen)
			}
			if err := handle.Validate(); err != nil {
				t.Fatalf("DecodeMetaIndexBlock returned invalid BlockHandle %+v: %v", handle, err)
			}
		}

		// Also exercise FindMetaIndexEntry path
		_, _, _ = sstable.FindMetaIndexEntry(data, "filter.lattice.default")
	})
}
