package version

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"math"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// helper to build encoded InternalKey for testing
func makeTestIK(userKey string, seqNum uint64, opType binary.OpType) []byte {
	ik, err := binary.NewInternalKey([]byte(userKey), binary.SeqNum(seqNum), opType)
	if err != nil {
		panic(err)
	}
	return binary.EncodeInternalKey(ik)
}

// -----------------------------------------------------------------------------
// EXACT-BYTE TEST FIXTURES & INDEPENDENT ORACLE (§25 & §26)
// -----------------------------------------------------------------------------

func TestFixtureA_EmptyVersionEdit(t *testing.T) {
	edit := NewVersionEdit()

	// Fixture A: Empty VersionEdit produces exactly 1 byte: VersionEditFormatV1 (0x01)
	expectedBytes := []byte{0x01}

	encoded := edit.Encode()
	if !bytes.Equal(encoded, expectedBytes) {
		t.Fatalf("Fixture A mismatch: got %x, want %x", encoded, expectedBytes)
	}

	decoded, err := DecodeVersionEdit(encoded)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if !decoded.IsEmpty() {
		t.Fatalf("expected empty edit, got %+v", decoded)
	}

	reEncoded := decoded.Encode()
	if !bytes.Equal(reEncoded, expectedBytes) {
		t.Fatalf("Re-encode mismatch: got %x, want %x", reEncoded, expectedBytes)
	}
}

func TestFixtureB_ScalarsOnly(t *testing.T) {
	edit := NewVersionEdit()
	edit.SetNextFileNum(42)
	edit.SetLastSeqNum(100)

	// Independently constructed expected byte stream:
	// FormatVersion: 0x01
	// TagNextFileNum (1): Tag(1)=0x01, Len(1)=0x01, Val(42)=0x2a
	// TagLastSeqNum (2):  Tag(2)=0x02, Len(1)=0x01, Val(100)=0x64
	expectedBytes := []byte{
		0x01,             // FormatVersion
		0x01, 0x01, 0x2a, // Tag 1, Len 1, NextFileNum = 42
		0x02, 0x01, 0x64, // Tag 2, Len 1, LastSeqNum = 100
	}

	encoded := edit.Encode()
	if !bytes.Equal(encoded, expectedBytes) {
		t.Fatalf("Fixture B mismatch: got %x, want %x", encoded, expectedBytes)
	}

	decoded, err := DecodeVersionEdit(encoded)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	nextNum, hasNext := decoded.NextFileNum()
	if !hasNext || nextNum != 42 {
		t.Fatalf("NextFileNum mismatch: got (%d, %v), want (42, true)", nextNum, hasNext)
	}

	lastSeq, hasSeq := decoded.LastSeqNum()
	if !hasSeq || lastSeq != 100 {
		t.Fatalf("LastSeqNum mismatch: got (%d, %v), want (100, true)", lastSeq, hasSeq)
	}

	if !edit.Equal(decoded) {
		t.Fatalf("edit not equal to decoded")
	}
}

func TestFixtureC_MultipleDeleteFile(t *testing.T) {
	edit := NewVersionEdit()
	// Insert in non-canonical order to verify deterministic sorting
	if err := edit.DeleteFile(2, 50); err != nil {
		t.Fatal(err)
	}
	if err := edit.DeleteFile(0, 10); err != nil {
		t.Fatal(err)
	}

	// Canonical Order: Level 0 File 10, then Level 2 File 50
	// FormatVersion: 0x01
	// Delete 1: Tag(3)=0x03, Len(2)=0x02, Level(0)=0x00, FileNum(10)=0x0a
	// Delete 2: Tag(3)=0x03, Len(2)=0x02, Level(2)=0x02, FileNum(50)=0x32
	expectedBytes := []byte{
		0x01,                   // FormatVersion
		0x03, 0x02, 0x00, 0x0a, // Level 0, File 10
		0x03, 0x02, 0x02, 0x32, // Level 2, File 50
	}

	encoded := edit.Encode()
	if !bytes.Equal(encoded, expectedBytes) {
		t.Fatalf("Fixture C mismatch: got %x, want %x", encoded, expectedBytes)
	}

	decoded, err := DecodeVersionEdit(encoded)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if !edit.Equal(decoded) {
		t.Fatalf("edit not equal to decoded")
	}
}

func TestFixtureD_SingleAddFile(t *testing.T) {
	edit := NewVersionEdit()
	sk := []byte("keyA")
	lk := []byte("keyZ")

	meta := FileMetadata{
		FileNum:        7,
		FileSize:       1024,
		SmallestKey:    sk,
		LargestKey:     lk,
		SmallestSeqNum: 10,
		LargestSeqNum:  20,
	}
	if err := edit.AddFile(1, meta); err != nil {
		t.Fatal(err)
	}

	// Independently constructed payload:
	// Level: 1 (0x01)
	// FileNum: 7 (0x07)
	// FileSize: 1024 -> 0x00 | (0x08 << 7) -> varint: [0x80, 0x08]
	// SmallestSeq: 10 (0x0a)
	// LargestSeq: 20 (0x14)
	// SmallestKeyLen: 4 (0x04)
	// SmallestKey: 'k', 'e', 'y', 'A' (4 bytes)
	// LargestKeyLen: 4 (0x04)
	// LargestKey: 'k', 'e', 'y', 'Z' (4 bytes)
	// Total payload length: 1 + 1 + 2 + 1 + 1 + 1 + 4 + 1 + 4 = 16 bytes (0x10)
	// Tag: 4 (0x04)
	expectedBytes := []byte{
		0x01,       // FormatVersion
		0x04, 0x10, // Tag 4, Len 16
		0x01, 0x07, 0x80, 0x08, 0x0a, 0x14, 0x04, 'k', 'e', 'y', 'A', 0x04, 'k', 'e', 'y', 'Z',
	}

	encoded := edit.Encode()
	if !bytes.Equal(encoded, expectedBytes) {
		t.Fatalf("Fixture D mismatch: got %x, want %x", encoded, expectedBytes)
	}

	decoded, err := DecodeVersionEdit(encoded)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if !edit.Equal(decoded) {
		t.Fatalf("edit not equal to decoded")
	}
}

func TestFixtureE_ComplexMixedEdit(t *testing.T) {
	edit := NewVersionEdit()
	edit.SetNextFileNum(100)
	edit.SetLastSeqNum(5000)

	_ = edit.DeleteFile(1, 15)
	_ = edit.DeleteFile(0, 5)

	meta1 := FileMetadata{
		FileNum:        20,
		FileSize:       2048,
		SmallestKey:    []byte("a"),
		LargestKey:     []byte("b"),
		SmallestSeqNum: 100,
		LargestSeqNum:  200,
	}
	_ = edit.AddFile(1, meta1)

	encoded := edit.Encode()
	decoded, err := DecodeVersionEdit(encoded)
	if err != nil {
		t.Fatalf("Decode complex edit failed: %v", err)
	}
	if !edit.Equal(decoded) {
		t.Fatalf("complex edit not equal to decoded")
	}

	// Canonical re-encode must be identical
	reEncoded := decoded.Encode()
	if !bytes.Equal(encoded, reEncoded) {
		t.Fatalf("canonical re-encode failed: %x vs %x", encoded, reEncoded)
	}
}

func TestFixtureF_ExplicitZeroScalarsWithPresence(t *testing.T) {
	edit := NewVersionEdit()
	edit.SetNextFileNum(0)
	edit.SetLastSeqNum(0)

	// FormatVersion: 0x01
	// Tag 1 (NextFileNum): Tag 1, Len 1, Val 0 -> [0x01, 0x01, 0x00]
	// Tag 2 (LastSeqNum):  Tag 2, Len 1, Val 0 -> [0x02, 0x01, 0x00]
	expectedBytes := []byte{
		0x01,             // FormatVersion
		0x01, 0x01, 0x00, // NextFileNum = 0
		0x02, 0x01, 0x00, // LastSeqNum = 0
	}

	encoded := edit.Encode()
	if !bytes.Equal(encoded, expectedBytes) {
		t.Fatalf("Fixture F mismatch: got %x, want %x", encoded, expectedBytes)
	}

	decoded, err := DecodeVersionEdit(encoded)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	nextNum, hasNext := decoded.NextFileNum()
	if !hasNext || nextNum != 0 {
		t.Fatalf("expected (0, true), got (%d, %v)", nextNum, hasNext)
	}

	lastSeq, hasSeq := decoded.LastSeqNum()
	if !hasSeq || lastSeq != 0 {
		t.Fatalf("expected (0, true), got (%d, %v)", lastSeq, hasSeq)
	}

	if !edit.Equal(decoded) {
		t.Fatalf("edit not equal to decoded")
	}
}

// -----------------------------------------------------------------------------
// ROUND-TRIP MATRIX (§27)
// -----------------------------------------------------------------------------

func TestRoundTripMatrix(t *testing.T) {
	testCases := []struct {
		name  string
		build func() *VersionEdit
	}{
		{
			name: "Empty",
			build: func() *VersionEdit {
				return NewVersionEdit()
			},
		},
		{
			name: "ScalarOnly_NextFileNum",
			build: func() *VersionEdit {
				e := NewVersionEdit()
				e.SetNextFileNum(12345)
				return e
			},
		},
		{
			name: "ScalarOnly_LastSeqNum",
			build: func() *VersionEdit {
				e := NewVersionEdit()
				e.SetLastSeqNum(987654321)
				return e
			},
		},
		{
			name: "OneAddFile",
			build: func() *VersionEdit {
				e := NewVersionEdit()
				_ = e.AddFile(0, FileMetadata{
					FileNum:        1,
					FileSize:       4096,
					SmallestKey:    makeTestIK("alpha", 1, binary.OpTypePut),
					LargestKey:     makeTestIK("omega", 1, binary.OpTypePut),
					SmallestSeqNum: 1,
					LargestSeqNum:  10,
				})
				return e
			},
		},
		{
			name: "OneDeleteFile",
			build: func() *VersionEdit {
				e := NewVersionEdit()
				_ = e.DeleteFile(3, 42)
				return e
			},
		},
		{
			name: "MultipleAddFilesAcrossLevels",
			build: func() *VersionEdit {
				e := NewVersionEdit()
				for lvl := uint32(0); lvl < NumLevels; lvl++ {
					_ = e.AddFile(lvl, FileMetadata{
						FileNum:        uint64(lvl*10 + 1),
						FileSize:       uint64((lvl + 1) * 1024),
						SmallestKey:    makeTestIK(fmt.Sprintf("level-%d-min", lvl), 1, binary.OpTypePut),
						LargestKey:     makeTestIK(fmt.Sprintf("level-%d-max", lvl), 2, binary.OpTypePut),
						SmallestSeqNum: 1,
						LargestSeqNum:  100,
					})
				}
				return e
			},
		},
		{
			name: "MultipleDeleteFilesAcrossLevels",
			build: func() *VersionEdit {
				e := NewVersionEdit()
				for lvl := uint32(0); lvl < NumLevels; lvl++ {
					_ = e.DeleteFile(lvl, uint64(lvl*100+1))
					_ = e.DeleteFile(lvl, uint64(lvl*100+2))
				}
				return e
			},
		},
		{
			name: "LargeFileAndSequenceNumbers",
			build: func() *VersionEdit {
				e := NewVersionEdit()
				e.SetNextFileNum(math.MaxUint64 - 1)
				e.SetLastSeqNum(binary.SeqNum(math.MaxUint64 - 1))
				_ = e.DeleteFile(6, math.MaxUint64-5)
				_ = e.AddFile(6, FileMetadata{
					FileNum:        math.MaxUint64 - 2,
					FileSize:       math.MaxUint64 - 1000,
					SmallestKey:    makeTestIK("huge", math.MaxUint64-1, binary.OpTypePut),
					LargestKey:     makeTestIK("huge-max", math.MaxUint64-1, binary.OpTypePut),
					SmallestSeqNum: math.MaxUint64 - 50,
					LargestSeqNum:  math.MaxUint64 - 1,
				})
				return e
			},
		},
		{
			name: "EmptyKeyMetadata",
			build: func() *VersionEdit {
				e := NewVersionEdit()
				_ = e.AddFile(0, FileMetadata{
					FileNum:        1,
					FileSize:       0,
					SmallestKey:    nil,
					LargestKey:     nil,
					SmallestSeqNum: 0,
					LargestSeqNum:  0,
				})
				return e
			},
		},
		{
			name: "LargeValidKeyMetadata",
			build: func() *VersionEdit {
				e := NewVersionEdit()
				largeUserKey := bytes.Repeat([]byte("k"), 65000)
				sk := makeTestIK(string(largeUserKey), 1, binary.OpTypePut)
				lk := makeTestIK(string(largeUserKey)+"z", 2, binary.OpTypePut)
				_ = e.AddFile(2, FileMetadata{
					FileNum:        99,
					FileSize:       5000000,
					SmallestKey:    sk,
					LargestKey:     lk,
					SmallestSeqNum: 1,
					LargestSeqNum:  2,
				})
				return e
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			orig := tc.build()
			enc1 := orig.Encode()

			decoded, err := DecodeVersionEdit(enc1)
			if err != nil {
				t.Fatalf("decode failed: %v", err)
			}

			if !orig.Equal(decoded) {
				t.Fatalf("semantic equality check failed between original and decoded")
			}

			enc2 := decoded.Encode()
			if !bytes.Equal(enc1, enc2) {
				t.Fatalf("re-encode did not match original canonical encoding: %x vs %x", enc1, enc2)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// CORRUPTION TEST MATRIX (§29)
// -----------------------------------------------------------------------------

func TestCorruption_TruncationAtEveryByte(t *testing.T) {
	edit := NewVersionEdit()
	edit.SetNextFileNum(100)
	edit.SetLastSeqNum(500)
	_ = edit.DeleteFile(1, 10)
	_ = edit.AddFile(2, FileMetadata{
		FileNum:        25,
		FileSize:       4096,
		SmallestKey:    []byte("smallest"),
		LargestKey:     []byte("largest"),
		SmallestSeqNum: 1,
		LargestSeqNum:  500,
	})

	fullBytes := edit.Encode()
	if len(fullBytes) < 10 {
		t.Fatalf("fullBytes unexpectedly short: %d", len(fullBytes))
	}

	// Determine valid field boundaries in fullBytes
	boundaries := make(map[int]bool)
	boundaries[1] = true // After version byte: valid empty edit

	// Scan fullBytes to identify all valid field boundary offsets
	off := 1
	for off < len(fullBytes) {
		_, nTag, err := binary.GetVarint64(fullBytes[off:])
		if err != nil {
			break
		}
		off += nTag
		length, nLen, err := binary.GetVarint64(fullBytes[off:])
		if err != nil {
			break
		}
		off += nLen
		off += int(length)
		boundaries[off] = true
	}

	for i := 0; i < len(fullBytes); i++ {
		truncated := fullBytes[:i]
		decoded, err := DecodeVersionEdit(truncated)
		if boundaries[i] {
			// At valid record boundaries, the prefix forms a valid edit with fewer fields
			if err != nil {
				t.Fatalf("unexpected error at valid boundary offset %d: %v", i, err)
			}
			if decoded == nil {
				t.Fatalf("decoded must not be nil at valid boundary %d", i)
			}
		} else {
			// Inside a tag, length, or payload: MUST fail closed
			if err == nil {
				t.Fatalf("expected error on truncated byte count %d (intra-field), got nil", i)
			}
			if !stdErrors.Is(err, errors.ErrTruncatedVersionEdit) && !stdErrors.Is(err, errors.ErrCorruptedVersionEdit) {
				t.Fatalf("unexpected error type on byte %d: %v", i, err)
			}
		}
	}
}

func TestCorruption_UnsupportedVersion(t *testing.T) {
	edit := NewVersionEdit()
	edit.SetNextFileNum(10)
	enc := edit.Encode()

	// Mutate version byte from 0x01 to 0x02
	enc[0] = 0x02
	_, err := DecodeVersionEdit(enc)
	if !stdErrors.Is(err, errors.ErrUnsupportedVersionEdit) {
		t.Fatalf("expected ErrUnsupportedVersionEdit, got %v", err)
	}

	var uvErr *errors.UnsupportedVersionEditError
	if !stdErrors.As(err, &uvErr) {
		t.Fatalf("expected *UnsupportedVersionEditError")
	}
	if uvErr.Version != 0x02 {
		t.Fatalf("expected version 0x02 in error, got 0x%02x", uvErr.Version)
	}
}

func TestCorruption_DuplicateScalars(t *testing.T) {
	t.Run("DuplicateNextFileNum", func(t *testing.T) {
		// Construct raw bytes with two TagNextFileNum fields
		raw := []byte{
			0x01,             // Version
			0x01, 0x01, 0x0a, // Tag 1, Len 1, Val 10
			0x01, 0x01, 0x14, // Tag 1, Len 1, Val 20 (Duplicate!)
		}
		_, err := DecodeVersionEdit(raw)
		if !stdErrors.Is(err, errors.ErrDuplicateScalarField) {
			t.Fatalf("expected ErrDuplicateScalarField, got %v", err)
		}
	})

	t.Run("DuplicateLastSeqNum", func(t *testing.T) {
		raw := []byte{
			0x01,             // Version
			0x02, 0x01, 0x0a, // Tag 2, Len 1, Val 10
			0x02, 0x01, 0x14, // Tag 2, Len 1, Val 20 (Duplicate!)
		}
		_, err := DecodeVersionEdit(raw)
		if !stdErrors.Is(err, errors.ErrDuplicateScalarField) {
			t.Fatalf("expected ErrDuplicateScalarField, got %v", err)
		}
	})
}

func TestCorruption_InvalidLevel(t *testing.T) {
	t.Run("DeleteFile_InvalidLevel", func(t *testing.T) {
		raw := []byte{
			0x01,                   // Version
			0x03, 0x02, 0x07, 0x05, // Tag 3, Len 2, Level=7 (Invalid! Max is 6), FileNum=5
		}
		_, err := DecodeVersionEdit(raw)
		if !stdErrors.Is(err, errors.ErrInvalidLevel) {
			t.Fatalf("expected ErrInvalidLevel, got %v", err)
		}
	})

	t.Run("AddFile_InvalidLevel", func(t *testing.T) {
		raw := []byte{
			0x01,       // Version
			0x04, 0x07, // Tag 4, Len 7
			0x07,                   // Level = 7 (Invalid!)
			0x01, 0x01, 0x01, 0x01, // FileNum, FileSize, SmallestSeq, LargestSeq
			0x00, 0x00, // SmallestKeyLen 0, LargestKeyLen 0
		}
		_, err := DecodeVersionEdit(raw)
		if !stdErrors.Is(err, errors.ErrInvalidLevel) {
			t.Fatalf("expected ErrInvalidLevel, got %v", err)
		}
	})
}

func TestCorruption_LengthOversized(t *testing.T) {
	// TLV length claims 2 MB (exceeds MaxFieldPayloadLen = 1 MB)
	raw := []byte{
		0x01,                   // Version
		0x01,                   // Tag 1
		0x80, 0x80, 0x80, 0x01, // Length = 2,097,152 (2 MB)
	}
	_, err := DecodeVersionEdit(raw)
	if !stdErrors.Is(err, errors.ErrCorruptedVersionEdit) {
		t.Fatalf("expected ErrCorruptedVersionEdit on oversized length, got %v", err)
	}
}

func TestCorruption_TrailingBytesInPayload(t *testing.T) {
	// DeleteFile payload has 3 bytes instead of 2 varints
	raw := []byte{
		0x01,                         // Version
		0x03, 0x03, 0x00, 0x05, 0xff, // Tag 3, Len 3, Level 0, File 5, Trailing byte 0xff!
	}
	_, err := DecodeVersionEdit(raw)
	if !stdErrors.Is(err, errors.ErrCorruptedVersionEdit) {
		t.Fatalf("expected ErrCorruptedVersionEdit on trailing bytes, got %v", err)
	}
}

// -----------------------------------------------------------------------------
// FORWARD COMPATIBILITY: UNKNOWN TAG SKIPPING (§17)
// -----------------------------------------------------------------------------

func TestForwardCompatibility_UnknownTag(t *testing.T) {
	// Construct a stream with known TagNextFileNum, followed by unknown Tag 99 with valid length,
	// followed by known TagLastSeqNum
	raw := []byte{
		0x01,             // Version
		0x01, 0x01, 0x2a, // Tag 1 (NextFileNum=42)
		0x63, 0x04, 'a', 'b', 'c', 'd', // Tag 99, Len 4, Payload "abcd" (Unknown Tag!)
		0x02, 0x01, 0x64, // Tag 2 (LastSeqNum=100)
	}

	decoded, err := DecodeVersionEdit(raw)
	if err != nil {
		t.Fatalf("unexpected decode failure with unknown tag: %v", err)
	}

	num, hasNum := decoded.NextFileNum()
	if !hasNum || num != 42 {
		t.Fatalf("expected NextFileNum 42, got %d", num)
	}

	seq, hasSeq := decoded.LastSeqNum()
	if !hasSeq || seq != 100 {
		t.Fatalf("expected LastSeqNum 100, got %d", seq)
	}
}

// -----------------------------------------------------------------------------
// MEMORY & OWNERSHIP ISOLATION TESTS (§24 & §31)
// -----------------------------------------------------------------------------

func TestMemoryOwnershipIsolation(t *testing.T) {
	sk := []byte("orig-small")
	lk := []byte("orig-large")

	meta := FileMetadata{
		FileNum:     1,
		SmallestKey: sk,
		LargestKey:  lk,
	}

	edit := NewVersionEdit()
	_ = edit.AddFile(0, meta)

	// Mutate original keys
	sk[0] = 'X'
	lk[0] = 'Y'

	// AddedFiles() should still have original values
	adds := edit.AddedFiles()
	if string(adds[0].Meta.SmallestKey) != "orig-small" {
		t.Fatalf("AddFile aliased original SmallestKey slice!")
	}
	if string(adds[0].Meta.LargestKey) != "orig-large" {
		t.Fatalf("AddFile aliased original LargestKey slice!")
	}

	// Mutate returned slice from AddedFiles
	adds[0].Meta.SmallestKey[0] = 'Z'
	adds2 := edit.AddedFiles()
	if string(adds2[0].Meta.SmallestKey) != "orig-small" {
		t.Fatalf("AddedFiles() leaked mutable internal reference!")
	}

	// Decode isolation: mutating encoded buffer does not affect decoded edit
	encoded := edit.Encode()
	decoded, err := DecodeVersionEdit(encoded)
	if err != nil {
		t.Fatal(err)
	}

	// Mutate encoded buffer
	for i := range encoded {
		encoded[i] = 0xff
	}

	decAdds := decoded.AddedFiles()
	if string(decAdds[0].Meta.SmallestKey) != "orig-small" {
		t.Fatalf("DecodeVersionEdit aliased input byte buffer!")
	}
}

// -----------------------------------------------------------------------------
// ADAPTER FROM SSTABLE METADATA
// -----------------------------------------------------------------------------

func TestNewFileMetadataFromSSTable(t *testing.T) {
	sstMeta := &sstable.SSTableMetadata{
		Path:           "/var/data/000042.sst",
		FileSize:       8192,
		SmallestKey:    []byte("smallest-key"),
		LargestKey:     []byte("largest-key"),
		SmallestSeqNum: 10,
		LargestSeqNum:  20,
	}

	fMeta := NewFileMetadataFromSSTable(42, sstMeta)
	if fMeta.FileNum != 42 {
		t.Errorf("FileNum mismatch: %d", fMeta.FileNum)
	}
	if fMeta.FileSize != 8192 {
		t.Errorf("FileSize mismatch: %d", fMeta.FileSize)
	}
	if !bytes.Equal(fMeta.SmallestKey, []byte("smallest-key")) {
		t.Errorf("SmallestKey mismatch")
	}
	if !bytes.Equal(fMeta.LargestKey, []byte("largest-key")) {
		t.Errorf("LargestKey mismatch")
	}

	// Nil sstable metadata test
	fMetaNil := NewFileMetadataFromSSTable(99, nil)
	if fMetaNil.FileNum != 99 || fMetaNil.FileSize != 0 {
		t.Errorf("unexpected nil handling: %+v", fMetaNil)
	}
}

// -----------------------------------------------------------------------------
// RESET & CLONE TESTS
// -----------------------------------------------------------------------------

func TestVersionEdit_ResetAndClone(t *testing.T) {
	e := NewVersionEdit()
	e.SetNextFileNum(10)
	e.SetLastSeqNum(20)
	_ = e.DeleteFile(0, 1)
	_ = e.AddFile(1, FileMetadata{FileNum: 2})

	clone := e.Clone()
	if !e.Equal(clone) {
		t.Fatalf("cloned edit not equal to original")
	}

	e.Reset()
	if !e.IsEmpty() {
		t.Fatalf("reset edit is not empty")
	}
	if clone.IsEmpty() {
		t.Fatalf("clone was affected by reset of original")
	}
}

// -----------------------------------------------------------------------------
// VALIDATION TESTS
// -----------------------------------------------------------------------------

func TestVersionEdit_Validation(t *testing.T) {
	e := NewVersionEdit()

	// Invalid Level
	if err := e.AddFile(7, FileMetadata{FileNum: 1}); err == nil {
		t.Errorf("expected error on AddFile level 7")
	}
	if err := e.DeleteFile(7, 1); err == nil {
		t.Errorf("expected error on DeleteFile level 7")
	}

	// Oversized keys
	hugeKey := make([]byte, binary.MaxEncodedInternalKeyLen+1)
	if err := e.AddFile(0, FileMetadata{FileNum: 1, SmallestKey: hugeKey}); err == nil {
		t.Errorf("expected error on AddFile oversized SmallestKey")
	}
	if err := e.AddFile(0, FileMetadata{FileNum: 1, LargestKey: hugeKey}); err == nil {
		t.Errorf("expected error on AddFile oversized LargestKey")
	}
}

// -----------------------------------------------------------------------------
// FUZZ TESTING (§30)
// -----------------------------------------------------------------------------

func FuzzDecodeVersionEdit(f *testing.F) {
	// Seed 1: Empty
	f.Add([]byte{0x01})

	// Seed 2: NextFileNum = 42, LastSeqNum = 100
	edit1 := NewVersionEdit()
	edit1.SetNextFileNum(42)
	edit1.SetLastSeqNum(100)
	f.Add(edit1.Encode())

	// Seed 3: Complex Edit
	edit2 := NewVersionEdit()
	edit2.SetNextFileNum(100)
	_ = edit2.DeleteFile(1, 10)
	_ = edit2.AddFile(2, FileMetadata{
		FileNum:        5,
		FileSize:       1024,
		SmallestKey:    []byte("min"),
		LargestKey:     []byte("max"),
		SmallestSeqNum: 1,
		LargestSeqNum:  10,
	})
	f.Add(edit2.Encode())

	// Seed 4: Corrupt versions
	f.Add([]byte{0x00})
	f.Add([]byte{0x02, 0x01, 0x01, 0x0a})

	// Seed 5: Truncated
	f.Add([]byte{0x01, 0x01, 0x05, 0x01})

	f.Fuzz(func(t *testing.T, data []byte) {
		// Decoder must NEVER panic on any input
		decoded, err := DecodeVersionEdit(data)
		if err != nil {
			if decoded != nil {
				t.Fatalf("decoded must be nil on error")
			}
			return
		}

		// If decode succeeded, re-encoding must succeed and be deterministic
		reEncoded := decoded.Encode()
		reDecoded, err2 := DecodeVersionEdit(reEncoded)
		if err2 != nil {
			t.Fatalf("failed to decode re-encoded bytes: %v", err2)
		}
		if !decoded.Equal(reDecoded) {
			t.Fatalf("re-decoded edit not semantically equal to decoded edit")
		}
	})
}

// -----------------------------------------------------------------------------
// BENCHMARKS (§36)
// -----------------------------------------------------------------------------

func BenchmarkVersionEdit_Encode_Small(b *testing.B) {
	edit := NewVersionEdit()
	edit.SetNextFileNum(100)
	edit.SetLastSeqNum(500)
	_ = edit.DeleteFile(0, 10)
	_ = edit.AddFile(0, FileMetadata{
		FileNum:        11,
		FileSize:       4096,
		SmallestKey:    []byte("user:000001"),
		LargestKey:     []byte("user:000050"),
		SmallestSeqNum: 100,
		LargestSeqNum:  500,
	})

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = edit.Encode()
	}
}

func BenchmarkVersionEdit_Decode_Small(b *testing.B) {
	edit := NewVersionEdit()
	edit.SetNextFileNum(100)
	edit.SetLastSeqNum(500)
	_ = edit.DeleteFile(0, 10)
	_ = edit.AddFile(0, FileMetadata{
		FileNum:        11,
		FileSize:       4096,
		SmallestKey:    []byte("user:000001"),
		LargestKey:     []byte("user:000050"),
		SmallestSeqNum: 100,
		LargestSeqNum:  500,
	})
	data := edit.Encode()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := DecodeVersionEdit(data)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkVersionEdit_Encode_Complex(b *testing.B) {
	edit := NewVersionEdit()
	edit.SetNextFileNum(1000)
	edit.SetLastSeqNum(50000)
	for i := 0; i < 10; i++ {
		_ = edit.DeleteFile(uint32(i%NumLevels), uint64(i+1))
		_ = edit.AddFile(uint32(i%NumLevels), FileMetadata{
			FileNum:        uint64(i + 100),
			FileSize:       uint64((i + 1) * 64 * 1024),
			SmallestKey:    []byte(fmt.Sprintf("user:key:%06d", i*100)),
			LargestKey:     []byte(fmt.Sprintf("user:key:%06d", i*100+99)),
			SmallestSeqNum: uint64(i * 1000),
			LargestSeqNum:  uint64(i*1000 + 999),
		})
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = edit.Encode()
	}
}

func BenchmarkVersionEdit_Decode_Complex(b *testing.B) {
	edit := NewVersionEdit()
	edit.SetNextFileNum(1000)
	edit.SetLastSeqNum(50000)
	for i := 0; i < 10; i++ {
		_ = edit.DeleteFile(uint32(i%NumLevels), uint64(i+1))
		_ = edit.AddFile(uint32(i%NumLevels), FileMetadata{
			FileNum:        uint64(i + 100),
			FileSize:       uint64((i + 1) * 64 * 1024),
			SmallestKey:    []byte(fmt.Sprintf("user:key:%06d", i*100)),
			LargestKey:     []byte(fmt.Sprintf("user:key:%06d", i*100+99)),
			SmallestSeqNum: uint64(i * 1000),
			LargestSeqNum:  uint64(i*1000 + 999),
		})
	}
	data := edit.Encode()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := DecodeVersionEdit(data)
		if err != nil {
			b.Fatal(err)
		}
	}
}
