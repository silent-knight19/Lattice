package sstable_test

import (
	"bytes"
	stdErrors "errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// mustNotPanic is an adversarial test helper ensuring fn never panics.
func mustNotPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PANIC SAFETY VIOLATION [%s]: unexpected panic: %v", name, r)
		}
	}()
	fn()
}

// buildMaliciousMetaIndexBlock creates a MetaIndex block containing
// an entry with arbitrary raw bytes, a valid offsets region, entry count, and a valid CRC32 trailer.
func buildMaliciousMetaIndexBlock(entrySlice []byte) []byte {
	// Offsets region: 1 entry starting at offset 0
	var offsetsBuf [4]byte
	binary.PutUint32(offsetsBuf[:], 0)

	// Entry count: 1
	var countBuf [4]byte
	binary.PutUint32(countBuf[:], 1)

	payload := append([]byte{}, entrySlice...)
	payload = append(payload, offsetsBuf[:]...)
	payload = append(payload, countBuf[:]...)

	// CRC32 trailer
	crc := binary.Checksum(payload)
	var crcBuf [4]byte
	binary.PutUint32(crcBuf[:], crc)

	return append(payload, crcBuf[:]...)
}

// encodeVarint64Raw encodes a uint64 into a byte slice without bounds restrictions.
func encodeVarint64Raw(v uint64) []byte {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutVarint64(buf[:], v)
	return buf[:n]
}

// TestSecurity_Remediation_SEC_001_IntegerOverflowPanicPoC verifies that the exact
// arithmetic exploit (keyLen = 2^64 - 6 with entryLen = 20) is safely rejected
// with an IndexBlockCorruptedError without panicking.
func TestSecurity_Remediation_SEC_001_IntegerOverflowPanicPoC(t *testing.T) {
	// 10-byte varint encoding of uint64(18446744073709551610) == 2^64 - 6
	craftedVarint := []byte{0xfa, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}
	entrySlice := make([]byte, 20)
	copy(entrySlice, craftedVarint)
	craftedBlock := buildMaliciousMetaIndexBlock(entrySlice)

	mustNotPanic(t, "SEC-001 Integer Wrap PoC", func() {
		_, err := sstable.DecodeMetaIndexBlock(craftedBlock)
		if err == nil {
			t.Fatalf("expected error on crafted integer-wrap input, got nil")
		}

		var corruptedErr *errors.IndexBlockCorruptedError
		if !stdErrors.As(err, &corruptedErr) && !stdErrors.Is(err, errors.ErrIndexBlockCorrupted) {
			t.Fatalf("expected ErrIndexBlockCorrupted, got: %v", err)
		}
	})
}

// TestSecurity_Remediation_SEC_001_BoundaryMatrix exercises all integer boundary
// values, overflow edges, valid keys, and malformed inputs against DecodeMetaIndexBlock.
func TestSecurity_Remediation_SEC_001_BoundaryMatrix(t *testing.T) {
	validHandle := sstable.BlockHandle{Offset: 100, Size: 200}
	encodedHandle := validHandle.Encode()

	tests := []struct {
		name        string
		keyLen      uint64
		entryLen    int
		customEntry []byte
		expectErr   bool
		errSentinel error
	}{
		{
			name:      "Valid_SmallestKey",
			keyLen:    1,
			entryLen:  1 + len(encodedHandle) + 1, // 1 byte varint (0x01) + 1 byte key + 16 byte handle = 18
			expectErr: false,
		},
		{
			name:      "Valid_StandardKey",
			keyLen:    12,                          // "filter.bloom"
			entryLen:  1 + len(encodedHandle) + 12, // 1 + 16 + 12 = 29
			expectErr: false,
		},
		{
			name:      "Valid_MaxEncodedInternalKeyLen",
			keyLen:    binary.MaxEncodedInternalKeyLen,
			entryLen:  3 + len(encodedHandle) + binary.MaxEncodedInternalKeyLen,
			expectErr: false,
		},
		{
			name:        "Invalid_KeyLenZero",
			keyLen:      0,
			entryLen:    1 + len(encodedHandle), // 1 byte varint + 0 key + 16 handle = 17
			expectErr:   true,
			errSentinel: errors.ErrIndexBlockCorrupted,
		},
		{
			name:        "Invalid_KeyLenExceedsMax",
			keyLen:      binary.MaxEncodedInternalKeyLen + 1,
			entryLen:    3 + len(encodedHandle) + binary.MaxEncodedInternalKeyLen + 1,
			expectErr:   true,
			errSentinel: errors.ErrIndexBlockCorrupted,
		},
		{
			name:        "Invalid_MaxUint64",
			keyLen:      math.MaxUint64,
			entryLen:    32,
			expectErr:   true,
			errSentinel: errors.ErrIndexBlockCorrupted,
		},
		{
			name:        "Invalid_MaxUint64Minus1",
			keyLen:      math.MaxUint64 - 1,
			entryLen:    32,
			expectErr:   true,
			errSentinel: errors.ErrIndexBlockCorrupted,
		},
		{
			name:        "Invalid_MaxUint64Minus6",
			keyLen:      math.MaxUint64 - 6,
			entryLen:    20,
			expectErr:   true,
			errSentinel: errors.ErrIndexBlockCorrupted,
		},
		{
			name:        "Invalid_MaxUint64Minus7",
			keyLen:      math.MaxUint64 - 7,
			entryLen:    20,
			expectErr:   true,
			errSentinel: errors.ErrIndexBlockCorrupted,
		},
		{
			name:        "Invalid_LengthMismatch_KeyLenLargerThanRemaining",
			keyLen:      15,
			entryLen:    1 + len(encodedHandle) + 10, // declared 15, remaining 10
			expectErr:   true,
			errSentinel: errors.ErrIndexBlockCorrupted,
		},
		{
			name:        "Invalid_LengthMismatch_KeyLenSmallerThanRemaining",
			keyLen:      5,
			entryLen:    1 + len(encodedHandle) + 10, // declared 5, remaining 10
			expectErr:   true,
			errSentinel: errors.ErrIndexBlockCorrupted,
		},
		{
			name:        "Invalid_TruncatedEntry_LessThanMinEntryLen",
			customEntry: []byte{0x05, 0x01, 0x02, 0x03}, // only 4 bytes (needs at least 17)
			expectErr:   true,
			errSentinel: errors.ErrIndexBlockCorrupted,
		},
		{
			name: "Invalid_MalformedBlockHandleInEntry",
			// keyLen 4, key "test", handle with size=0 (invalid)
			customEntry: append(append([]byte{0x04}, []byte("test")...), make([]byte, 16)...),
			expectErr:   true,
			errSentinel: errors.ErrInvalidBlockHandle,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var entry []byte
			if tc.customEntry != nil {
				entry = tc.customEntry
			} else {
				vBytes := encodeVarint64Raw(tc.keyLen)
				entry = make([]byte, tc.entryLen)
				copy(entry, vBytes)
				// Fill key bytes with dummy 'k'
				keyStart := len(vBytes)
				keyEnd := keyStart + int(tc.keyLen)
				if keyEnd <= tc.entryLen-len(encodedHandle) {
					for i := keyStart; i < keyEnd; i++ {
						entry[i] = 'k'
					}
					copy(entry[keyEnd:], encodedHandle[:])
				}
			}

			block := buildMaliciousMetaIndexBlock(entry)

			mustNotPanic(t, tc.name, func() {
				res, err := sstable.DecodeMetaIndexBlock(block)
				if tc.expectErr {
					if err == nil {
						t.Fatalf("expected error, got nil (res=%+v)", res)
					}
					if tc.errSentinel != nil && !stdErrors.Is(err, tc.errSentinel) {
						t.Fatalf("expected error matching %v, got: %v", tc.errSentinel, err)
					}
				} else {
					if err != nil {
						t.Fatalf("expected success, got error: %v", err)
					}
					if len(res) != 1 {
						t.Fatalf("expected 1 entry, got %d", len(res))
					}
				}
			})
		})
	}
}

// TestSecurity_Remediation_SEC_001_MutationTesting performs systematic 1-byte mutations
// across valid MetaIndex blocks, verifying that every corrupted input fails closed and never panics.
func TestSecurity_Remediation_SEC_001_MutationTesting(t *testing.T) {
	entries := map[string]sstable.BlockHandle{
		"filter.bloom": {Offset: 1024, Size: 256},
		"stats.keys":   {Offset: 1280, Size: 64},
		"compact.meta": {Offset: 1344, Size: 128},
	}
	validBlock := sstable.BuildMetaIndexBlock(entries)

	bitFlipMasks := []byte{0x01, 0x02, 0x80, 0xFF}

	// 1. Mutate single bytes with recomputed valid CRC (simulating deliberate adversarial tampering)
	for i := 0; i < len(validBlock)-4; i++ {
		for _, mask := range bitFlipMasks {
			mutated := bytes.Clone(validBlock)
			mutated[i] ^= mask

			// Recompute valid CRC for mutated payload
			payloadLen := len(mutated) - 4
			newCRC := binary.Checksum(mutated[:payloadLen])
			binary.PutUint32(mutated[payloadLen:], newCRC)

			mustNotPanic(t, "Adversarial Single-Byte Mutation", func() {
				// Must either succeed (if semantic meaning preserved) or return a clean error. Never panic.
				_, _ = sstable.DecodeMetaIndexBlock(mutated)
			})
		}
	}

	// 2. Truncate at every possible byte boundary
	for l := 0; l <= len(validBlock); l++ {
		truncated := validBlock[:l]
		mustNotPanic(t, "Truncation Boundary", func() {
			_, err := sstable.DecodeMetaIndexBlock(truncated)
			if l > 0 && l < sstable.MetaIndexTrailerSize {
				if !stdErrors.Is(err, errors.ErrIndexBlockTruncated) {
					t.Fatalf("expected ErrIndexBlockTruncated at len %d, got %v", l, err)
				}
			}
		})
	}
}

// TestSecurity_Remediation_SEC_001_TableReader_ReadFilterBlockPath tests the full
// end-to-end exploit path: TableReader.ReadFilterBlock -> FindMetaIndexEntry -> DecodeMetaIndexBlock.
// It creates a real SSTable on disk, replaces its MetaIndex block with the malicious wrapped integer
// vector (with valid CRC), and proves that TableReader.ReadFilterBlock() returns a clean
// IndexBlockCorruptedError without crashing the process.
func TestSecurity_Remediation_SEC_001_TableReader_ReadFilterBlockPath(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "malicious_metaindex_table.sst")

	// 1. Build a valid SSTable first
	writer, err := sstable.NewTableWriter(sstPath, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("failed to create table writer: %v", err)
	}
	ik := binary.InternalKey{
		UserKey: []byte("secure-key-001"),
		SeqNum:  1,
		OpType:  binary.OpTypePut,
	}
	if err := writer.Add(ik, []byte("secure-val-001")); err != nil {
		t.Fatalf("failed to add record: %v", err)
	}
	meta, err := writer.Finish()
	if err != nil {
		t.Fatalf("failed to finish table: %v", err)
	}

	// 2. Read raw SSTable bytes from disk
	rawBytes, err := os.ReadFile(sstPath)
	if err != nil {
		t.Fatalf("failed to read SSTable: %v", err)
	}

	// 3. Construct malicious MetaIndex block with keyLen = 2^64 - 6
	craftedVarint := []byte{0xfa, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}
	entrySlice := make([]byte, 20)
	copy(entrySlice, craftedVarint)
	craftedMetaBlock := buildMaliciousMetaIndexBlock(entrySlice)

	// 4. Reconstruct SSTable:
	// Replace MetaIndex region with craftedMetaBlock, then Index, then Footer
	metaOffset := meta.MetaIndexHandle.Offset
	indexOffset := metaOffset + uint64(len(craftedMetaBlock))

	// Extract original index block
	origIndexBlock := rawBytes[meta.IndexHandle.Offset : meta.IndexHandle.Offset+meta.IndexHandle.Size]

	// Rebuild Footer with updated handles
	newFooter := sstable.Footer{
		MetaIndexHandle: sstable.BlockHandle{Offset: metaOffset, Size: uint64(len(craftedMetaBlock))},
		IndexHandle:     sstable.BlockHandle{Offset: indexOffset, Size: uint64(len(origIndexBlock))},
	}
	newFooterBytes := newFooter.Encode()

	// Assemble modified SSTable file
	rebuiltTable := make([]byte, 0, len(rawBytes)+len(craftedMetaBlock))
	rebuiltTable = append(rebuiltTable, rawBytes[:metaOffset]...)
	rebuiltTable = append(rebuiltTable, craftedMetaBlock...)
	rebuiltTable = append(rebuiltTable, origIndexBlock...)
	rebuiltTable = append(rebuiltTable, newFooterBytes[:]...)

	if err := os.WriteFile(sstPath, rebuiltTable, 0600); err != nil {
		t.Fatalf("failed to overwrite SSTable with malicious fixture: %v", err)
	}

	// 5. Open TableReader and call ReadFilterBlock()
	reader, err := sstable.NewTableReader(sstPath)
	if err != nil {
		t.Fatalf("failed to open TableReader on crafted SSTable: %v", err)
	}
	defer func() { _ = reader.Close() }()

	mustNotPanic(t, "TableReader.ReadFilterBlock on Malicious SSTable", func() {
		filterBlock, err := reader.ReadFilterBlock()
		if err == nil {
			t.Fatalf("expected error on malicious MetaIndex table, got nil (filter=%+v)", filterBlock)
		}

		var corruptedErr *errors.IndexBlockCorruptedError
		if !stdErrors.As(err, &corruptedErr) && !stdErrors.Is(err, errors.ErrIndexBlockCorrupted) {
			t.Fatalf("expected ErrIndexBlockCorrupted from ReadFilterBlock, got: %v", err)
		}
	})
}
