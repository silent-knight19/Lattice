package version

import (
	stdErrors "errors"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// TestIND006_DeleteFile_ZeroFileNum verifies that DeleteFile rejects fileNum == 0.
func TestIND006_DeleteFile_ZeroFileNum(t *testing.T) {
	edit := NewVersionEdit()
	err := edit.DeleteFile(0, 0)
	if !stdErrors.Is(err, errors.ErrInvalidFileNum) {
		t.Fatalf("expected ErrInvalidFileNum for DeleteFile(0, 0), got: %v", err)
	}

	err = edit.DeleteFile(1, 0)
	if !stdErrors.Is(err, errors.ErrInvalidFileNum) {
		t.Fatalf("expected ErrInvalidFileNum for DeleteFile(1, 0), got: %v", err)
	}
}

// TestIND006_Decode_DeleteFile_ZeroFileNum verifies that DecodeVersionEdit rejects
// a serialized TagDeleteFile record specifying fileNum == 0.
func TestIND006_Decode_DeleteFile_ZeroFileNum(t *testing.T) {
	// Construct raw payload: Level=1 (0x01), FileNum=0 (0x00)
	var pBuf [2 * binary.MaxVarintLen64]byte
	n1 := binary.PutVarint64(pBuf[:], 1)
	n2 := binary.PutVarint64(pBuf[n1:], 0)

	raw := []byte{VersionEditFormatV1}
	raw = appendTLV(raw, TagDeleteFile, pBuf[:n1+n2])

	_, err := DecodeVersionEdit(raw)
	if !stdErrors.Is(err, errors.ErrInvalidFileNum) {
		t.Fatalf("expected ErrInvalidFileNum on decoded DeleteFile with fileNum=0, got: %v", err)
	}
}

// TestIND006_DuplicateAdditions verifies that duplicate file additions are rejected by Validate().
func TestIND006_DuplicateAdditions(t *testing.T) {
	t.Run("SameLevel", func(t *testing.T) {
		edit := NewVersionEdit()
		meta1 := FileMetadata{
			FileNum:        10,
			FileSize:       1024,
			SmallestKey:    makeTestIK("keyA", 1, binary.OpTypePut),
			LargestKey:     makeTestIK("keyM", 2, binary.OpTypePut),
			SmallestSeqNum: 1,
			LargestSeqNum:  2,
		}
		meta2 := FileMetadata{
			FileNum:        10,
			FileSize:       2048,
			SmallestKey:    makeTestIK("keyN", 3, binary.OpTypePut),
			LargestKey:     makeTestIK("keyZ", 4, binary.OpTypePut),
			SmallestSeqNum: 3,
			LargestSeqNum:  4,
		}
		if err := edit.AddFile(1, meta1); err != nil {
			t.Fatalf("unexpected AddFile error: %v", err)
		}
		if err := edit.AddFile(1, meta2); err != nil {
			t.Fatalf("unexpected AddFile error: %v", err)
		}

		err := edit.Validate()
		if !stdErrors.Is(err, errors.ErrCorruptedVersionEdit) {
			t.Fatalf("expected ErrCorruptedVersionEdit for duplicate file addition at same level, got: %v", err)
		}

		// Decode path must also fail
		enc := edit.Encode()
		_, decErr := DecodeVersionEdit(enc)
		if !stdErrors.Is(decErr, errors.ErrCorruptedVersionEdit) {
			t.Fatalf("expected ErrCorruptedVersionEdit on decode of duplicate file addition, got: %v", decErr)
		}
	})

	t.Run("CrossLevel", func(t *testing.T) {
		edit := NewVersionEdit()
		meta1 := FileMetadata{
			FileNum:        15,
			FileSize:       1024,
			SmallestKey:    makeTestIK("keyA", 1, binary.OpTypePut),
			LargestKey:     makeTestIK("keyM", 2, binary.OpTypePut),
			SmallestSeqNum: 1,
			LargestSeqNum:  2,
		}
		meta2 := FileMetadata{
			FileNum:        15,
			FileSize:       2048,
			SmallestKey:    makeTestIK("keyN", 3, binary.OpTypePut),
			LargestKey:     makeTestIK("keyZ", 4, binary.OpTypePut),
			SmallestSeqNum: 3,
			LargestSeqNum:  4,
		}
		if err := edit.AddFile(1, meta1); err != nil {
			t.Fatalf("unexpected AddFile error: %v", err)
		}
		if err := edit.AddFile(2, meta2); err != nil {
			t.Fatalf("unexpected AddFile error: %v", err)
		}

		err := edit.Validate()
		if !stdErrors.Is(err, errors.ErrCorruptedVersionEdit) {
			t.Fatalf("expected ErrCorruptedVersionEdit for duplicate file addition cross levels, got: %v", err)
		}
	})
}

// TestIND006_DuplicateDeletions verifies that duplicate file deletions at the same level are rejected.
func TestIND006_DuplicateDeletions(t *testing.T) {
	edit := NewVersionEdit()
	if err := edit.DeleteFile(2, 50); err != nil {
		t.Fatalf("unexpected DeleteFile error: %v", err)
	}
	if err := edit.DeleteFile(2, 50); err != nil {
		t.Fatalf("unexpected DeleteFile error: %v", err)
	}

	err := edit.Validate()
	if !stdErrors.Is(err, errors.ErrCorruptedVersionEdit) {
		t.Fatalf("expected ErrCorruptedVersionEdit for duplicate deletion, got: %v", err)
	}

	enc := edit.Encode()
	_, decErr := DecodeVersionEdit(enc)
	if !stdErrors.Is(decErr, errors.ErrCorruptedVersionEdit) {
		t.Fatalf("expected ErrCorruptedVersionEdit on decode of duplicate deletion, got: %v", decErr)
	}
}

// TestIND006_MutualExclusion_AddAndDelete verifies that adding and deleting the same file
// at the same level in the same edit is rejected.
func TestIND006_MutualExclusion_AddAndDelete(t *testing.T) {
	edit := NewVersionEdit()
	if err := edit.DeleteFile(1, 42); err != nil {
		t.Fatalf("unexpected DeleteFile error: %v", err)
	}
	meta := FileMetadata{
		FileNum:        42,
		FileSize:       1024,
		SmallestKey:    makeTestIK("keyA", 1, binary.OpTypePut),
		LargestKey:     makeTestIK("keyZ", 2, binary.OpTypePut),
		SmallestSeqNum: 1,
		LargestSeqNum:  2,
	}
	if err := edit.AddFile(1, meta); err != nil {
		t.Fatalf("unexpected AddFile error: %v", err)
	}

	err := edit.Validate()
	if !stdErrors.Is(err, errors.ErrCorruptedVersionEdit) {
		t.Fatalf("expected ErrCorruptedVersionEdit for add+delete mutual exclusion violation, got: %v", err)
	}

	enc := edit.Encode()
	_, decErr := DecodeVersionEdit(enc)
	if !stdErrors.Is(decErr, errors.ErrCorruptedVersionEdit) {
		t.Fatalf("expected ErrCorruptedVersionEdit on decode of mutual exclusion violation, got: %v", decErr)
	}
}

// TestIND006_LevelOverlappingKeyRanges verifies that Level >= 1 files added in the same edit
// cannot have overlapping key ranges, while Level 0 files may overlap.
func TestIND006_LevelOverlappingKeyRanges(t *testing.T) {
	t.Run("Level1_Overlapping_Rejected", func(t *testing.T) {
		edit := NewVersionEdit()
		meta1 := FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    makeTestIK("keyA", 1, binary.OpTypePut),
			LargestKey:     makeTestIK("keyP", 2, binary.OpTypePut),
			SmallestSeqNum: 1,
			LargestSeqNum:  2,
		}
		meta2 := FileMetadata{
			FileNum:        2,
			FileSize:       1024,
			SmallestKey:    makeTestIK("keyH", 3, binary.OpTypePut), // overlaps with keyA..keyP
			LargestKey:     makeTestIK("keyZ", 4, binary.OpTypePut),
			SmallestSeqNum: 3,
			LargestSeqNum:  4,
		}
		if err := edit.AddFile(1, meta1); err != nil {
			t.Fatalf("unexpected AddFile error: %v", err)
		}
		if err := edit.AddFile(1, meta2); err != nil {
			t.Fatalf("unexpected AddFile error: %v", err)
		}

		err := edit.Validate()
		if !stdErrors.Is(err, errors.ErrInvalidKeyRange) {
			t.Fatalf("expected ErrInvalidKeyRange for overlapping keys at L1, got: %v", err)
		}

		enc := edit.Encode()
		_, decErr := DecodeVersionEdit(enc)
		if !stdErrors.Is(decErr, errors.ErrInvalidKeyRange) {
			t.Fatalf("expected ErrInvalidKeyRange on decode of overlapping L1 keys, got: %v", decErr)
		}
	})

	t.Run("Level1_SameBoundaryUserKey_Rejected", func(t *testing.T) {
		edit := NewVersionEdit()
		meta1 := FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    makeTestIK("keyA", 1, binary.OpTypePut),
			LargestKey:     makeTestIK("keyM", 2, binary.OpTypePut),
			SmallestSeqNum: 1,
			LargestSeqNum:  2,
		}
		meta2 := FileMetadata{
			FileNum:        2,
			FileSize:       1024,
			SmallestKey:    makeTestIK("keyM", 3, binary.OpTypePut), // same user key "keyM"
			LargestKey:     makeTestIK("keyZ", 4, binary.OpTypePut),
			SmallestSeqNum: 3,
			LargestSeqNum:  4,
		}
		if err := edit.AddFile(1, meta1); err != nil {
			t.Fatalf("unexpected AddFile error: %v", err)
		}
		if err := edit.AddFile(1, meta2); err != nil {
			t.Fatalf("unexpected AddFile error: %v", err)
		}

		err := edit.Validate()
		if !stdErrors.Is(err, errors.ErrInvalidKeyRange) {
			t.Fatalf("expected ErrInvalidKeyRange for shared boundary user key at L1, got: %v", err)
		}
	})

	t.Run("Level0_Overlapping_Allowed", func(t *testing.T) {
		edit := NewVersionEdit()
		meta1 := FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    makeTestIK("keyA", 1, binary.OpTypePut),
			LargestKey:     makeTestIK("keyP", 2, binary.OpTypePut),
			SmallestSeqNum: 1,
			LargestSeqNum:  2,
		}
		meta2 := FileMetadata{
			FileNum:        2,
			FileSize:       1024,
			SmallestKey:    makeTestIK("keyH", 3, binary.OpTypePut), // overlaps with keyA..keyP
			LargestKey:     makeTestIK("keyZ", 4, binary.OpTypePut),
			SmallestSeqNum: 3,
			LargestSeqNum:  4,
		}
		if err := edit.AddFile(0, meta1); err != nil {
			t.Fatalf("unexpected AddFile error: %v", err)
		}
		if err := edit.AddFile(0, meta2); err != nil {
			t.Fatalf("unexpected AddFile error: %v", err)
		}

		if err := edit.Validate(); err != nil {
			t.Fatalf("overlapping keys at L0 must be allowed, got: %v", err)
		}

		enc := edit.Encode()
		decoded, err := DecodeVersionEdit(enc)
		if err != nil {
			t.Fatalf("decode failed for valid L0 overlapping edit: %v", err)
		}
		if decoded.NumAddedFiles() != 2 {
			t.Fatalf("expected 2 added files, got %d", decoded.NumAddedFiles())
		}
	})
}

// TestIND006_LastSeqNum_Consistency verifies that added files cannot have LargestSeqNum
// exceeding the edit's explicit LastSeqNum.
func TestIND006_LastSeqNum_Consistency(t *testing.T) {
	edit := NewVersionEdit()
	edit.SetLastSeqNum(50)

	meta := FileMetadata{
		FileNum:        1,
		FileSize:       1024,
		SmallestKey:    makeTestIK("keyA", 10, binary.OpTypePut),
		LargestKey:     makeTestIK("keyZ", 100, binary.OpTypePut),
		SmallestSeqNum: 10,
		LargestSeqNum:  100, // 100 > 50
	}
	if err := edit.AddFile(1, meta); err != nil {
		t.Fatalf("unexpected AddFile error: %v", err)
	}

	err := edit.Validate()
	if !stdErrors.Is(err, errors.ErrCorruptedVersionEdit) {
		t.Fatalf("expected ErrCorruptedVersionEdit when LargestSeqNum exceeds LastSeqNum, got: %v", err)
	}

	enc := edit.Encode()
	_, decErr := DecodeVersionEdit(enc)
	if !stdErrors.Is(decErr, errors.ErrCorruptedVersionEdit) {
		t.Fatalf("expected ErrCorruptedVersionEdit on decode when LargestSeqNum exceeds LastSeqNum, got: %v", decErr)
	}
}

// TestIND006_ValidComplexEdit verifies that a well-formed complex VersionEdit passes validation cleanly.
func TestIND006_ValidComplexEdit(t *testing.T) {
	edit := NewVersionEdit()
	edit.SetNextFileNum(100)
	edit.SetLastSeqNum(500)

	if err := edit.DeleteFile(0, 5); err != nil {
		t.Fatal(err)
	}
	if err := edit.DeleteFile(1, 10); err != nil {
		t.Fatal(err)
	}

	metaL1_A := FileMetadata{
		FileNum:        20,
		FileSize:       2048,
		SmallestKey:    makeTestIK("alpha", 100, binary.OpTypePut),
		LargestKey:     makeTestIK("beta", 150, binary.OpTypePut),
		SmallestSeqNum: 100,
		LargestSeqNum:  150,
	}
	metaL1_B := FileMetadata{
		FileNum:        21,
		FileSize:       2048,
		SmallestKey:    makeTestIK("delta", 200, binary.OpTypePut),
		LargestKey:     makeTestIK("gamma", 250, binary.OpTypePut),
		SmallestSeqNum: 200,
		LargestSeqNum:  250,
	}

	if err := edit.AddFile(1, metaL1_A); err != nil {
		t.Fatal(err)
	}
	if err := edit.AddFile(1, metaL1_B); err != nil {
		t.Fatal(err)
	}

	if err := edit.Validate(); err != nil {
		t.Fatalf("valid complex edit failed validation: %v", err)
	}

	enc := edit.Encode()
	decoded, err := DecodeVersionEdit(enc)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if !edit.Equal(decoded) {
		t.Fatalf("decoded edit does not equal original")
	}
}
