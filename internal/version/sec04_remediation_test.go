package version

import (
	"bytes"
	stdErrors "errors"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// Helper to encode a VersionEdit with a raw TagAddFile payload directly into wire format.
// This allows constructing syntactically valid or structurally targeted TLV records
// that may contain semantically invalid metadata.
func buildRawAddFileEdit(level uint32, fileNum, fileSize, smallestSeq, largestSeq uint64, smallestKey, largestKey []byte) []byte {
	payloadCap := 70 + len(smallestKey) + len(largestKey)
	payload := make([]byte, 0, payloadCap)

	var vBuf [binary.MaxVarintLen64]byte

	n := binary.PutVarint64(vBuf[:], uint64(level))
	payload = append(payload, vBuf[:n]...)

	n = binary.PutVarint64(vBuf[:], fileNum)
	payload = append(payload, vBuf[:n]...)

	n = binary.PutVarint64(vBuf[:], fileSize)
	payload = append(payload, vBuf[:n]...)

	n = binary.PutVarint64(vBuf[:], smallestSeq)
	payload = append(payload, vBuf[:n]...)

	n = binary.PutVarint64(vBuf[:], largestSeq)
	payload = append(payload, vBuf[:n]...)

	n = binary.PutVarint64(vBuf[:], uint64(len(smallestKey)))
	payload = append(payload, vBuf[:n]...)
	payload = append(payload, smallestKey...)

	n = binary.PutVarint64(vBuf[:], uint64(len(largestKey)))
	payload = append(payload, vBuf[:n]...)
	payload = append(payload, largestKey...)

	// Wrap in VersionEdit format: [0x01, TagAddFile (varint), Len (varint), payload]
	res := []byte{VersionEditFormatV1}
	res = appendTLV(res, TagAddFile, payload)
	return res
}

// -----------------------------------------------------------------------------
// 1. VALID METADATA TESTS (§17)
// -----------------------------------------------------------------------------

func TestSEC004_ValidMetadata(t *testing.T) {
	validCases := []struct {
		name        string
		level       uint32
		fileNum     uint64
		fileSize    uint64
		smallestSeq uint64
		largestSeq  uint64
		smallestKey []byte
		largestKey  []byte
	}{
		{
			name:        "Level0_Standard",
			level:       0,
			fileNum:     1,
			fileSize:    1024,
			smallestSeq: 10,
			largestSeq:  20,
			smallestKey: makeTestIK("key_a", 10, binary.OpTypePut),
			largestKey:  makeTestIK("key_z", 20, binary.OpTypePut),
		},
		{
			name:        "MaxLevel_Level6",
			level:       NumLevels - 1,
			fileNum:     999999,
			fileSize:    64 * 1024 * 1024,
			smallestSeq: 100,
			largestSeq:  5000,
			smallestKey: makeTestIK("l6_a", 100, binary.OpTypePut),
			largestKey:  makeTestIK("l6_z", 5000, binary.OpTypePut),
		},
		{
			name:        "MinValidInternalKeys_1ByteUserKey",
			level:       1,
			fileNum:     42,
			fileSize:    512,
			smallestSeq: 1,
			largestSeq:  1,
			smallestKey: makeTestIK("a", 1, binary.OpTypePut),
			largestKey:  makeTestIK("b", 1, binary.OpTypePut),
		},
		{
			name:        "SingleKeySSTable_EqualKeysAndEqualSeq",
			level:       2,
			fileNum:     77,
			fileSize:    256,
			smallestSeq: 50,
			largestSeq:  50,
			smallestKey: makeTestIK("exact_same_key", 50, binary.OpTypePut),
			largestKey:  makeTestIK("exact_same_key", 50, binary.OpTypePut),
		},
		{
			name:        "EqualUserKeys_DifferentSequences_ValidOrdering",
			level:       3,
			fileNum:     88,
			fileSize:    4096,
			smallestSeq: 5,
			largestSeq:  20,
			// In Lattice comparator: higher sequence sorts BEFORE lower sequence!
			// So ik(seq=20) < ik(seq=5), meaning SmallestKey=ik(20) and LargestKey=ik(5) is valid!
			smallestKey: makeTestIK("same_user_key", 20, binary.OpTypePut),
			largestKey:  makeTestIK("same_user_key", 5, binary.OpTypePut),
		},
		{
			name:        "EqualUserKeys_EqualSeq_DeleteSortsBeforePut",
			level:       3,
			fileNum:     89,
			fileSize:    4096,
			smallestSeq: 10,
			largestSeq:  10,
			// OpTypeDelete (0x02) sorts BEFORE OpTypePut (0x01)
			smallestKey: makeTestIK("same_user_key", 10, binary.OpTypeDelete),
			largestKey:  makeTestIK("same_user_key", 10, binary.OpTypePut),
		},
		{
			name:        "MaxPermittedKeyLength",
			level:       4,
			fileNum:     1234,
			fileSize:    1048576,
			smallestSeq: 1,
			largestSeq:  2,
			smallestKey: makeTestIK(string(bytes.Repeat([]byte("a"), binary.MaxUserKeyLen)), 1, binary.OpTypePut),
			largestKey:  makeTestIK(string(bytes.Repeat([]byte("z"), binary.MaxUserKeyLen)), 2, binary.OpTypePut),
		},
	}

	for _, tc := range validCases {
		t.Run(tc.name, func(t *testing.T) {
			edit := NewVersionEdit()
			meta := FileMetadata{
				FileNum:        tc.fileNum,
				FileSize:       tc.fileSize,
				SmallestKey:    tc.smallestKey,
				LargestKey:     tc.largestKey,
				SmallestSeqNum: tc.smallestSeq,
				LargestSeqNum:  tc.largestSeq,
			}

			// Path A: Direct AddFile
			if err := edit.AddFile(tc.level, meta); err != nil {
				t.Fatalf("AddFile unexpectedly failed: %v", err)
			}
			if edit.NumAddedFiles() != 1 {
				t.Fatalf("expected 1 added file, got %d", edit.NumAddedFiles())
			}

			// Path B: Encode & Decode
			enc := edit.Encode()
			decoded, err := DecodeVersionEdit(enc)
			if err != nil {
				t.Fatalf("DecodeVersionEdit failed on valid edit: %v", err)
			}
			if !edit.Equal(decoded) {
				t.Fatalf("decoded edit not equal to original")
			}
		})
	}
}

// -----------------------------------------------------------------------------
// 2. INVALID SCALAR TESTS (§17)
// -----------------------------------------------------------------------------

func TestSEC004_InvalidScalars(t *testing.T) {
	validSK := makeTestIK("keyA", 1, binary.OpTypePut)
	validLK := makeTestIK("keyZ", 10, binary.OpTypePut)

	t.Run("Level_EqualNumLevels", func(t *testing.T) {
		edit := NewVersionEdit()
		err := edit.AddFile(NumLevels, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    validSK,
			LargestKey:     validLK,
			SmallestSeqNum: 1,
			LargestSeqNum:  10,
		})
		if !stdErrors.Is(err, errors.ErrInvalidLevel) {
			t.Fatalf("expected ErrInvalidLevel, got %v", err)
		}
	})

	t.Run("Level_VeryLarge", func(t *testing.T) {
		edit := NewVersionEdit()
		err := edit.AddFile(999, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    validSK,
			LargestKey:     validLK,
			SmallestSeqNum: 1,
			LargestSeqNum:  10,
		})
		if !stdErrors.Is(err, errors.ErrInvalidLevel) {
			t.Fatalf("expected ErrInvalidLevel, got %v", err)
		}
	})

	t.Run("FileNum_Zero", func(t *testing.T) {
		edit := NewVersionEdit()
		err := edit.AddFile(0, FileMetadata{
			FileNum:        0,
			FileSize:       1024,
			SmallestKey:    validSK,
			LargestKey:     validLK,
			SmallestSeqNum: 1,
			LargestSeqNum:  10,
		})
		if !stdErrors.Is(err, errors.ErrInvalidFileNum) {
			t.Fatalf("expected ErrInvalidFileNum, got %v", err)
		}
	})

	t.Run("FileSize_Zero", func(t *testing.T) {
		edit := NewVersionEdit()
		err := edit.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       0,
			SmallestKey:    validSK,
			LargestKey:     validLK,
			SmallestSeqNum: 1,
			LargestSeqNum:  10,
		})
		if !stdErrors.Is(err, errors.ErrInvalidFileSize) {
			t.Fatalf("expected ErrInvalidFileSize, got %v", err)
		}
	})

	t.Run("SmallestSeqNum_GreaterThan_LargestSeqNum", func(t *testing.T) {
		edit := NewVersionEdit()
		err := edit.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    validSK,
			LargestKey:     validLK,
			SmallestSeqNum: 20,
			LargestSeqNum:  10,
		})
		if !stdErrors.Is(err, errors.ErrInvalidSeqNumRange) {
			t.Fatalf("expected ErrInvalidSeqNumRange, got %v", err)
		}
	})
}

// -----------------------------------------------------------------------------
// 3. INVALID KEY TESTS (§17)
// -----------------------------------------------------------------------------

func TestSEC004_InvalidKeys(t *testing.T) {
	validSK := makeTestIK("keyA", 1, binary.OpTypePut)
	validLK := makeTestIK("keyZ", 10, binary.OpTypePut)

	t.Run("SmallestKey_Nil", func(t *testing.T) {
		edit := NewVersionEdit()
		err := edit.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    nil,
			LargestKey:     validLK,
			SmallestSeqNum: 1,
			LargestSeqNum:  10,
		})
		if err == nil {
			t.Fatalf("expected error on nil SmallestKey")
		}
	})

	t.Run("SmallestKey_Empty", func(t *testing.T) {
		edit := NewVersionEdit()
		err := edit.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    []byte{},
			LargestKey:     validLK,
			SmallestSeqNum: 1,
			LargestSeqNum:  10,
		})
		if err == nil {
			t.Fatalf("expected error on empty SmallestKey")
		}
	})

	t.Run("SmallestKey_Truncated_Under10Bytes", func(t *testing.T) {
		for l := 1; l < 10; l++ {
			edit := NewVersionEdit()
			err := edit.AddFile(0, FileMetadata{
				FileNum:        1,
				FileSize:       1024,
				SmallestKey:    bytes.Repeat([]byte{0x01}, l),
				LargestKey:     validLK,
				SmallestSeqNum: 1,
				LargestSeqNum:  10,
			})
			if !stdErrors.Is(err, errors.ErrInternalKeyTruncated) {
				t.Fatalf("len %d: expected ErrInternalKeyTruncated, got %v", l, err)
			}
		}
	})

	t.Run("SmallestKey_Oversized", func(t *testing.T) {
		hugeKey := make([]byte, binary.MaxEncodedInternalKeyLen+1)
		edit := NewVersionEdit()
		err := edit.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    hugeKey,
			LargestKey:     validLK,
			SmallestSeqNum: 1,
			LargestSeqNum:  10,
		})
		if err == nil {
			t.Fatalf("expected error on oversized SmallestKey")
		}
	})

	t.Run("SmallestKey_InvalidOpType", func(t *testing.T) {
		badKey := bytes.Clone(validSK)
		badKey[len(badKey)-1] = 0x05 // Invalid OpType
		edit := NewVersionEdit()
		err := edit.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    badKey,
			LargestKey:     validLK,
			SmallestSeqNum: 1,
			LargestSeqNum:  10,
		})
		if !stdErrors.Is(err, errors.ErrInvalidOpType) {
			t.Fatalf("expected ErrInvalidOpType, got %v", err)
		}
	})

	t.Run("LargestKey_Nil", func(t *testing.T) {
		edit := NewVersionEdit()
		err := edit.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    validSK,
			LargestKey:     nil,
			SmallestSeqNum: 1,
			LargestSeqNum:  10,
		})
		if err == nil {
			t.Fatalf("expected error on nil LargestKey")
		}
	})

	t.Run("LargestKey_Empty", func(t *testing.T) {
		edit := NewVersionEdit()
		err := edit.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    validSK,
			LargestKey:     []byte{},
			SmallestSeqNum: 1,
			LargestSeqNum:  10,
		})
		if err == nil {
			t.Fatalf("expected error on empty LargestKey")
		}
	})

	t.Run("LargestKey_Truncated_Under10Bytes", func(t *testing.T) {
		for l := 1; l < 10; l++ {
			edit := NewVersionEdit()
			err := edit.AddFile(0, FileMetadata{
				FileNum:        1,
				FileSize:       1024,
				SmallestKey:    validSK,
				LargestKey:     bytes.Repeat([]byte{0x01}, l),
				SmallestSeqNum: 1,
				LargestSeqNum:  10,
			})
			if !stdErrors.Is(err, errors.ErrInternalKeyTruncated) {
				t.Fatalf("len %d: expected ErrInternalKeyTruncated, got %v", l, err)
			}
		}
	})

	t.Run("LargestKey_Oversized", func(t *testing.T) {
		hugeKey := make([]byte, binary.MaxEncodedInternalKeyLen+1)
		edit := NewVersionEdit()
		err := edit.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    validSK,
			LargestKey:     hugeKey,
			SmallestSeqNum: 1,
			LargestSeqNum:  10,
		})
		if err == nil {
			t.Fatalf("expected error on oversized LargestKey")
		}
	})

	t.Run("LargestKey_InvalidOpType", func(t *testing.T) {
		badKey := bytes.Clone(validLK)
		badKey[len(badKey)-1] = 0xff // Invalid OpType
		edit := NewVersionEdit()
		err := edit.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    validSK,
			LargestKey:     badKey,
			SmallestSeqNum: 1,
			LargestSeqNum:  10,
		})
		if !stdErrors.Is(err, errors.ErrInvalidOpType) {
			t.Fatalf("expected ErrInvalidOpType, got %v", err)
		}
	})
}

// -----------------------------------------------------------------------------
// 4. KEY RANGE ORDERING TESTS (§17)
// -----------------------------------------------------------------------------

func TestSEC004_KeyRangeOrdering(t *testing.T) {
	t.Run("UserKey_BackwardsRange", func(t *testing.T) {
		edit := NewVersionEdit()
		// "zzz" > "aaa", so SmallestKey sorts after LargestKey
		err := edit.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    makeTestIK("zzz", 10, binary.OpTypePut),
			LargestKey:     makeTestIK("aaa", 10, binary.OpTypePut),
			SmallestSeqNum: 1,
			LargestSeqNum:  10,
		})
		if !stdErrors.Is(err, errors.ErrInvalidKeyRange) {
			t.Fatalf("expected ErrInvalidKeyRange, got %v", err)
		}
	})

	t.Run("EqualUserKey_BackwardsSequenceOrdering", func(t *testing.T) {
		edit := NewVersionEdit()
		// In Lattice comparator: higher sequence number sorts FIRST/BEFORE lower sequence number.
		// If SmallestKey has seq 5 and LargestKey has seq 10:
		// ik("k", seq=10) < ik("k", seq=5)!
		// Therefore SmallestKey sorts AFTER LargestKey!
		err := edit.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    makeTestIK("same_key", 5, binary.OpTypePut),
			LargestKey:     makeTestIK("same_key", 10, binary.OpTypePut),
			SmallestSeqNum: 5,
			LargestSeqNum:  10,
		})
		if !stdErrors.Is(err, errors.ErrInvalidKeyRange) {
			t.Fatalf("expected ErrInvalidKeyRange, got %v", err)
		}
	})

	t.Run("EqualUserKey_EqualSeq_BackwardsOpTypeOrdering", func(t *testing.T) {
		edit := NewVersionEdit()
		// OpTypeDelete (0x02) sorts BEFORE OpTypePut (0x01).
		// If SmallestKey is Put and LargestKey is Delete, SmallestKey sorts after LargestKey!
		err := edit.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    makeTestIK("same_key", 10, binary.OpTypePut),
			LargestKey:     makeTestIK("same_key", 10, binary.OpTypeDelete),
			SmallestSeqNum: 10,
			LargestSeqNum:  10,
		})
		if !stdErrors.Is(err, errors.ErrInvalidKeyRange) {
			t.Fatalf("expected ErrInvalidKeyRange, got %v", err)
		}
	})

	t.Run("IdenticalKeys_SingleEntryTable_Accepted", func(t *testing.T) {
		edit := NewVersionEdit()
		k := makeTestIK("exact_key", 42, binary.OpTypePut)
		err := edit.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    k,
			LargestKey:     k,
			SmallestSeqNum: 42,
			LargestSeqNum:  42,
		})
		if err != nil {
			t.Fatalf("expected identical keys to be accepted, got %v", err)
		}
	})
}

// -----------------------------------------------------------------------------
// 5. ATOMICITY TESTS (§15)
// -----------------------------------------------------------------------------

func TestSEC004_Atomicity(t *testing.T) {
	validSK := makeTestIK("alpha", 1, binary.OpTypePut)
	validLK := makeTestIK("omega", 10, binary.OpTypePut)

	edit := NewVersionEdit()

	// Pre-populate with one valid AddFile
	if err := edit.AddFile(0, FileMetadata{
		FileNum:        1,
		FileSize:       1024,
		SmallestKey:    validSK,
		LargestKey:     validLK,
		SmallestSeqNum: 1,
		LargestSeqNum:  10,
	}); err != nil {
		t.Fatalf("pre-population failed: %v", err)
	}

	initialCount := edit.NumAddedFiles()
	if initialCount != 1 {
		t.Fatalf("expected 1 file, got %d", initialCount)
	}

	// Attempt to add various invalid entries: count MUST remain exactly initialCount
	invalidMetas := []struct {
		name  string
		level uint32
		meta  FileMetadata
	}{
		{"FileNum0", 0, FileMetadata{FileNum: 0, FileSize: 1024, SmallestKey: validSK, LargestKey: validLK, SmallestSeqNum: 1, LargestSeqNum: 10}},
		{"FileSize0", 0, FileMetadata{FileNum: 2, FileSize: 0, SmallestKey: validSK, LargestKey: validLK, SmallestSeqNum: 1, LargestSeqNum: 10}},
		{"InvalidLevel", 7, FileMetadata{FileNum: 2, FileSize: 1024, SmallestKey: validSK, LargestKey: validLK, SmallestSeqNum: 1, LargestSeqNum: 10}},
		{"BackwardsSeq", 0, FileMetadata{FileNum: 2, FileSize: 1024, SmallestKey: validSK, LargestKey: validLK, SmallestSeqNum: 20, LargestSeqNum: 10}},
		{"TruncatedSK", 0, FileMetadata{FileNum: 2, FileSize: 1024, SmallestKey: []byte{1, 2}, LargestKey: validLK, SmallestSeqNum: 1, LargestSeqNum: 10}},
		{"BackwardsKeys", 0, FileMetadata{FileNum: 2, FileSize: 1024, SmallestKey: validLK, LargestKey: validSK, SmallestSeqNum: 1, LargestSeqNum: 10}},
	}

	for _, tc := range invalidMetas {
		t.Run(tc.name, func(t *testing.T) {
			err := edit.AddFile(tc.level, tc.meta)
			if err == nil {
				t.Fatalf("expected error on invalid metadata %s", tc.name)
			}
			if edit.NumAddedFiles() != initialCount {
				t.Fatalf("atomicity violation: NumAddedFiles changed from %d to %d", initialCount, edit.NumAddedFiles())
			}
			adds := edit.AddedFiles()
			if len(adds) != initialCount {
				t.Fatalf("atomicity violation: AddedFiles() length changed from %d to %d", initialCount, len(adds))
			}
			if adds[0].Meta.FileNum != 1 {
				t.Fatalf("atomicity violation: existing entry modified")
			}
		})
	}
}

// -----------------------------------------------------------------------------
// 6. DEFENSIVE COPYING TESTS (§16)
// -----------------------------------------------------------------------------

func TestSEC004_DefensiveCopying(t *testing.T) {
	sk := makeTestIK("defensive_a", 1, binary.OpTypePut)
	lk := makeTestIK("defensive_z", 10, binary.OpTypePut)

	meta := FileMetadata{
		FileNum:        100,
		FileSize:       2048,
		SmallestKey:    sk,
		LargestKey:     lk,
		SmallestSeqNum: 1,
		LargestSeqNum:  10,
	}

	edit := NewVersionEdit()
	if err := edit.AddFile(0, meta); err != nil {
		t.Fatalf("AddFile failed: %v", err)
	}

	origSK := bytes.Clone(sk)
	origLK := bytes.Clone(lk)

	// Mutate caller's buffers
	sk[0] = 0xff
	lk[0] = 0xee

	adds := edit.AddedFiles()
	if !bytes.Equal(adds[0].Meta.SmallestKey, origSK) {
		t.Fatalf("SmallestKey aliased caller buffer")
	}
	if !bytes.Equal(adds[0].Meta.LargestKey, origLK) {
		t.Fatalf("LargestKey aliased caller buffer")
	}

	// Mutate returned slice
	adds[0].Meta.SmallestKey[0] = 0xdd
	adds[0].Meta.LargestKey[0] = 0xcc

	adds2 := edit.AddedFiles()
	if !bytes.Equal(adds2[0].Meta.SmallestKey, origSK) {
		t.Fatalf("AddedFiles() returned mutable reference to internal state")
	}
	if !bytes.Equal(adds2[0].Meta.LargestKey, origLK) {
		t.Fatalf("AddedFiles() returned mutable reference to internal state")
	}
}

// -----------------------------------------------------------------------------
// 7. DECODER CORRUPTION & REJECTION TESTS (§18)
// -----------------------------------------------------------------------------

func TestSEC004_DecoderRejection(t *testing.T) {
	validSK := makeTestIK("alpha", 1, binary.OpTypePut)
	validLK := makeTestIK("omega", 10, binary.OpTypePut)

	cases := []struct {
		name        string
		level       uint32
		fileNum     uint64
		fileSize    uint64
		smallestSeq uint64
		largestSeq  uint64
		smallestKey []byte
		largestKey  []byte
		wantErr     error
	}{
		{
			name:        "Decode_FileNumZero",
			level:       0,
			fileNum:     0,
			fileSize:    1024,
			smallestSeq: 1,
			largestSeq:  10,
			smallestKey: validSK,
			largestKey:  validLK,
			wantErr:     errors.ErrInvalidFileNum,
		},
		{
			name:        "Decode_FileSizeZero",
			level:       0,
			fileNum:     1,
			fileSize:    0,
			smallestSeq: 1,
			largestSeq:  10,
			smallestKey: validSK,
			largestKey:  validLK,
			wantErr:     errors.ErrInvalidFileSize,
		},
		{
			name:        "Decode_BackwardsSeqRange",
			level:       0,
			fileNum:     1,
			fileSize:    1024,
			smallestSeq: 50,
			largestSeq:  10,
			smallestKey: validSK,
			largestKey:  validLK,
			wantErr:     errors.ErrInvalidSeqNumRange,
		},
		{
			name:        "Decode_TruncatedSmallestKey",
			level:       0,
			fileNum:     1,
			fileSize:    1024,
			smallestSeq: 1,
			largestSeq:  10,
			smallestKey: []byte("short"),
			largestKey:  validLK,
			wantErr:     errors.ErrInternalKeyTruncated,
		},
		{
			name:        "Decode_TruncatedLargestKey",
			level:       0,
			fileNum:     1,
			fileSize:    1024,
			smallestSeq: 1,
			largestSeq:  10,
			smallestKey: validSK,
			largestKey:  []byte("short"),
			wantErr:     errors.ErrInternalKeyTruncated,
		},
		{
			name:        "Decode_BackwardsKeyRange",
			level:       0,
			fileNum:     1,
			fileSize:    1024,
			smallestSeq: 1,
			largestSeq:  10,
			smallestKey: validLK,
			largestKey:  validSK,
			wantErr:     errors.ErrInvalidKeyRange,
		},
		{
			name:        "Decode_InvalidLevel",
			level:       7,
			fileNum:     1,
			fileSize:    1024,
			smallestSeq: 1,
			largestSeq:  10,
			smallestKey: validSK,
			largestKey:  validLK,
			wantErr:     errors.ErrInvalidLevel,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := buildRawAddFileEdit(tc.level, tc.fileNum, tc.fileSize, tc.smallestSeq, tc.largestSeq, tc.smallestKey, tc.largestKey)
			decoded, err := DecodeVersionEdit(raw)
			if err == nil {
				t.Fatalf("expected decode error, got nil")
			}
			if decoded != nil {
				t.Fatalf("expected decoded to be nil on error, got %+v", decoded)
			}
			if !stdErrors.Is(err, tc.wantErr) {
				t.Fatalf("expected error %v, got %v", tc.wantErr, err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// 8. ADVERSARIAL COMBINATIONS MATRIX (§20)
// -----------------------------------------------------------------------------

func TestSEC004_AdversarialTable(t *testing.T) {
	validSK := makeTestIK("a", 1, binary.OpTypePut)
	validLK := makeTestIK("z", 10, binary.OpTypePut)

	type testRow struct {
		name        string
		level       uint32
		fileNum     uint64
		fileSize    uint64
		smallestSeq uint64
		largestSeq  uint64
		smallestKey []byte
		largestKey  []byte
		expectPass  bool
	}

	table := []testRow{
		{"FileNum=0", 0, 0, 1024, 1, 10, validSK, validLK, false},
		{"FileSize=0", 0, 1, 0, 1, 10, validSK, validLK, false},
		{"bad Level", 7, 1, 1024, 1, 10, validSK, validLK, false},
		{"invalid SmallestKey", 0, 1, 1024, 1, 10, []byte("bad"), validLK, false},
		{"invalid LargestKey", 0, 1, 1024, 1, 10, validSK, []byte("bad"), false},
		{"SmallestKey > LargestKey", 0, 1, 1024, 1, 10, validLK, validSK, false},
		{"SmallestSeqNum > LargestSeqNum", 0, 1, 1024, 20, 10, validSK, validLK, false},
		{"all scalar fields valid + valid keys", 0, 1, 1024, 1, 10, validSK, validLK, true},
		{"multiple violations (FileNum=0 and SmallestKey>LargestKey)", 0, 0, 1024, 1, 10, validLK, validSK, false},
		{"multiple violations (FileSize=0 and SeqNum backwards)", 0, 1, 0, 20, 10, validSK, validLK, false},
	}

	for _, row := range table {
		t.Run(row.name, func(t *testing.T) {
			// Test admission via AddFile
			edit := NewVersionEdit()
			errAdd := edit.AddFile(row.level, FileMetadata{
				FileNum:        row.fileNum,
				FileSize:       row.fileSize,
				SmallestKey:    row.smallestKey,
				LargestKey:     row.largestKey,
				SmallestSeqNum: row.smallestSeq,
				LargestSeqNum:  row.largestSeq,
			})

			// Test admission via Decode
			raw := buildRawAddFileEdit(row.level, row.fileNum, row.fileSize, row.smallestSeq, row.largestSeq, row.smallestKey, row.largestKey)
			decoded, errDec := DecodeVersionEdit(raw)

			if row.expectPass {
				if errAdd != nil {
					t.Fatalf("AddFile unexpectedly failed: %v", errAdd)
				}
				if errDec != nil {
					t.Fatalf("Decode unexpectedly failed: %v", errDec)
				}
				if decoded == nil || decoded.NumAddedFiles() != 1 {
					t.Fatalf("expected 1 admitted file in decoded edit")
				}
			} else {
				if errAdd == nil {
					t.Fatalf("AddFile unexpectedly succeeded")
				}
				if errDec == nil {
					t.Fatalf("Decode unexpectedly succeeded")
				}
				if decoded != nil {
					t.Fatalf("decoded must be nil on failure")
				}
			}
		})
	}
}

// -----------------------------------------------------------------------------
// 9. ADMISSION PARITY TESTS (§21)
// -----------------------------------------------------------------------------

func TestSEC004_AdmissionParity(t *testing.T) {
	validSK := makeTestIK("alpha", 1, binary.OpTypePut)
	validLK := makeTestIK("omega", 10, binary.OpTypePut)

	testCases := []struct {
		name  string
		level uint32
		meta  FileMetadata
	}{
		{"ValidStandard", 1, FileMetadata{FileNum: 10, FileSize: 4096, SmallestKey: validSK, LargestKey: validLK, SmallestSeqNum: 1, LargestSeqNum: 10}},
		{"InvalidFileNumZero", 1, FileMetadata{FileNum: 0, FileSize: 4096, SmallestKey: validSK, LargestKey: validLK, SmallestSeqNum: 1, LargestSeqNum: 10}},
		{"InvalidFileSizeZero", 1, FileMetadata{FileNum: 10, FileSize: 0, SmallestKey: validSK, LargestKey: validLK, SmallestSeqNum: 1, LargestSeqNum: 10}},
		{"InvalidLevelSeven", 7, FileMetadata{FileNum: 10, FileSize: 4096, SmallestKey: validSK, LargestKey: validLK, SmallestSeqNum: 1, LargestSeqNum: 10}},
		{"InvalidSeqRange", 1, FileMetadata{FileNum: 10, FileSize: 4096, SmallestKey: validSK, LargestKey: validLK, SmallestSeqNum: 50, LargestSeqNum: 10}},
		{"InvalidSmallestKeyTruncated", 1, FileMetadata{FileNum: 10, FileSize: 4096, SmallestKey: []byte{1, 2, 3}, LargestKey: validLK, SmallestSeqNum: 1, LargestSeqNum: 10}},
		{"InvalidLargestKeyTruncated", 1, FileMetadata{FileNum: 10, FileSize: 4096, SmallestKey: validSK, LargestKey: []byte{1, 2, 3}, SmallestSeqNum: 1, LargestSeqNum: 10}},
		{"InvalidKeyRangeBackwards", 1, FileMetadata{FileNum: 10, FileSize: 4096, SmallestKey: validLK, LargestKey: validSK, SmallestSeqNum: 1, LargestSeqNum: 10}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			edit := NewVersionEdit()
			errAdd := edit.AddFile(tc.level, tc.meta)

			raw := buildRawAddFileEdit(tc.level, tc.meta.FileNum, tc.meta.FileSize, tc.meta.SmallestSeqNum, tc.meta.LargestSeqNum, tc.meta.SmallestKey, tc.meta.LargestKey)
			decoded, errDec := DecodeVersionEdit(raw)

			// Both paths must yield identical pass/fail outcomes
			if (errAdd == nil) != (errDec == nil) {
				t.Fatalf("parity mismatch: AddFile err=%v, Decode err=%v", errAdd, errDec)
			}

			if errAdd != nil {
				// Assert same sentinel / error classification
				if errAdd.Error() != errDec.Error() && !stdErrors.Is(errDec, errAdd) && !stdErrors.Is(errAdd, errDec) {
					t.Fatalf("error classification divergence: AddFile=%v, Decode=%v", errAdd, errDec)
				}
				if decoded != nil {
					t.Fatalf("decoded must be nil on failure")
				}
			} else {
				if !edit.Equal(decoded) {
					t.Fatalf("admitted edit divergence: %+v != %+v", edit, decoded)
				}
			}
		})
	}
}
