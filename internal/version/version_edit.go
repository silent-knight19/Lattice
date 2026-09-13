package version

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
)

const (
	// NumLevels defines the fixed number of LSM-tree levels (L0 through L6) in Lattice.
	NumLevels = 7

	// VersionEditFormatV1 is the format version byte for VersionEdit binary serialization.
	VersionEditFormatV1 = 0x01

	// TagNextFileNum is the TLV tag for the next available file number scalar.
	TagNextFileNum = 1

	// TagLastSeqNum is the TLV tag for the highest committed sequence number scalar.
	TagLastSeqNum = 2

	// TagDeleteFile is the TLV tag for a deleted SSTable file entry (Level, FileNum).
	TagDeleteFile = 3

	// TagAddFile is the TLV tag for an added SSTable file entry (Level, FileMetadata).
	TagAddFile = 4

	// MaxVersionEditBytes defines the maximum physical byte size permitted for a
	// serialized VersionEdit record (16 MiB). This bounds allocation to prevent DoS.
	MaxVersionEditBytes = 16 * 1024 * 1024

	// MaxFieldPayloadLen defines the maximum permissible byte length of an individual
	// TLV field payload (1 MiB).
	MaxFieldPayloadLen = 1 * 1024 * 1024

	// MaxAddFilesPerEdit defines the maximum number of AddFile records permitted in a single edit (10,000).
	MaxAddFilesPerEdit = 10000

	// MaxDeleteFilesPerEdit defines the maximum number of DeleteFile records permitted in a single edit (10,000).
	MaxDeleteFilesPerEdit = 10000
)

// FileMetadata encapsulates the immutable persistent metadata identity of an SSTable file
// tracked in the VersionSet manifest layer.
//
// Persistent vs. Runtime Contract:
//   - Persisted: FileNum, FileSize, SmallestKey, LargestKey, SmallestSeqNum, LargestSeqNum.
//   - Excluded (Runtime-Only): File descriptors, open reader handles, block caches, staging paths,
//     mutexes, internal BlockHandles (Index/Filter/MetaIndex are embedded in footer), and BloomFilter objects.
type FileMetadata struct {
	FileNum        uint64
	FileSize       uint64
	SmallestKey    []byte // encoded binary.InternalKey
	LargestKey     []byte // encoded binary.InternalKey
	SmallestSeqNum uint64
	LargestSeqNum  uint64
}

// Clone returns an independent deep copy of FileMetadata with cloned key slices.
func (m FileMetadata) Clone() FileMetadata {
	return FileMetadata{
		FileNum:        m.FileNum,
		FileSize:       m.FileSize,
		SmallestKey:    bytes.Clone(m.SmallestKey),
		LargestKey:     bytes.Clone(m.LargestKey),
		SmallestSeqNum: m.SmallestSeqNum,
		LargestSeqNum:  m.LargestSeqNum,
	}
}

// Equal reports whether m and other represent identical persistent metadata.
func (m FileMetadata) Equal(other FileMetadata) bool {
	return m.FileNum == other.FileNum &&
		m.FileSize == other.FileSize &&
		bytes.Equal(m.SmallestKey, other.SmallestKey) &&
		bytes.Equal(m.LargestKey, other.LargestKey) &&
		m.SmallestSeqNum == other.SmallestSeqNum &&
		m.LargestSeqNum == other.LargestSeqNum
}

// NewFileMetadataFromSSTable extracts the persistent metadata properties from a finalized
// sstable.SSTableMetadata struct. Transient runtime fields (Path, BlockHandles) are ignored.
func NewFileMetadataFromSSTable(fileNum uint64, meta *sstable.SSTableMetadata) FileMetadata {
	if meta == nil {
		return FileMetadata{FileNum: fileNum}
	}
	return FileMetadata{
		FileNum:        fileNum,
		FileSize:       meta.FileSize,
		SmallestKey:    bytes.Clone(meta.SmallestKey),
		LargestKey:     bytes.Clone(meta.LargestKey),
		SmallestSeqNum: meta.SmallestSeqNum,
		LargestSeqNum:  meta.LargestSeqNum,
	}
}

// AddFileEntry represents an SSTable addition to a specific LSM-tree level.
type AddFileEntry struct {
	Level uint32
	Meta  FileMetadata
}

// Clone returns an independent deep copy of AddFileEntry.
func (a AddFileEntry) Clone() AddFileEntry {
	return AddFileEntry{
		Level: a.Level,
		Meta:  a.Meta.Clone(),
	}
}

// Equal reports whether a and other represent identical file additions.
func (a AddFileEntry) Equal(other AddFileEntry) bool {
	return a.Level == other.Level && a.Meta.Equal(other.Meta)
}

// DeleteFileEntry represents an SSTable deletion from a specific LSM-tree level.
type DeleteFileEntry struct {
	Level   uint32
	FileNum uint64
}

// Equal reports whether d and other represent identical file deletions.
func (d DeleteFileEntry) Equal(other DeleteFileEntry) bool {
	return d.Level == other.Level && d.FileNum == other.FileNum
}

// VersionEdit represents an atomic delta transition to Lattice's version state.
//
// In Lattice's LSM-Tree architecture, a VersionEdit is a state delta (not a snapshot).
// It records new SSTables added via flushes or compactions, obsolete SSTables deleted,
// and updates to monotonically increasing file and sequence counters.
type VersionEdit struct {
	hasNextFileNum bool
	nextFileNum    uint64

	hasLastSeqNum bool
	lastSeqNum    binary.SeqNum

	deletedFiles []DeleteFileEntry
	addedFiles   []AddFileEntry
}

// NewVersionEdit initializes an empty VersionEdit.
func NewVersionEdit() *VersionEdit {
	return &VersionEdit{}
}

// SetNextFileNum explicitly records the next available file number in the edit.
func (e *VersionEdit) SetNextFileNum(num uint64) {
	e.nextFileNum = num
	e.hasNextFileNum = true
}

// NextFileNum returns the next available file number and a boolean indicating whether
// the field was explicitly set.
func (e *VersionEdit) NextFileNum() (uint64, bool) {
	return e.nextFileNum, e.hasNextFileNum
}

// HasNextFileNum reports whether NextFileNum was explicitly set.
func (e *VersionEdit) HasNextFileNum() bool {
	return e.hasNextFileNum
}

// ClearNextFileNum unsets NextFileNum from the edit.
func (e *VersionEdit) ClearNextFileNum() {
	e.nextFileNum = 0
	e.hasNextFileNum = false
}

// SetLastSeqNum explicitly records the highest committed sequence number in the edit.
func (e *VersionEdit) SetLastSeqNum(seq binary.SeqNum) {
	e.lastSeqNum = seq
	e.hasLastSeqNum = true
}

// LastSeqNum returns the highest committed sequence number and a boolean indicating
// whether the field was explicitly set.
func (e *VersionEdit) LastSeqNum() (binary.SeqNum, bool) {
	return e.lastSeqNum, e.hasLastSeqNum
}

// HasLastSeqNum reports whether LastSeqNum was explicitly set.
func (e *VersionEdit) HasLastSeqNum() bool {
	return e.hasLastSeqNum
}

// ClearLastSeqNum unsets LastSeqNum from the edit.
func (e *VersionEdit) ClearLastSeqNum() {
	e.lastSeqNum = 0
	e.hasLastSeqNum = false
}

// AddFile records an SSTable addition at the designated level with the provided metadata.
//
// Validation & Security Contracts:
//   - Level must satisfy 0 <= level < NumLevels (7).
//   - SmallestKey and LargestKey byte slices must not exceed binary.MaxEncodedInternalKeyLen.
//   - Key byte slices are defensively cloned to ensure memory isolation.
func (e *VersionEdit) AddFile(level uint32, meta FileMetadata) error {
	if level >= NumLevels {
		return &errors.InvalidLevelError{Level: level, MaxLevel: NumLevels - 1}
	}
	if len(meta.SmallestKey) > binary.MaxEncodedInternalKeyLen {
		return &errors.KeyTooLargeError{KeySize: uint32(len(meta.SmallestKey)), MaxSize: binary.MaxEncodedInternalKeyLen}
	}
	if len(meta.LargestKey) > binary.MaxEncodedInternalKeyLen {
		return &errors.KeyTooLargeError{KeySize: uint32(len(meta.LargestKey)), MaxSize: binary.MaxEncodedInternalKeyLen}
	}

	e.addedFiles = append(e.addedFiles, AddFileEntry{
		Level: level,
		Meta:  meta.Clone(),
	})
	return nil
}

// DeleteFile records an SSTable deletion from the designated level.
// Level must satisfy 0 <= level < NumLevels (7).
func (e *VersionEdit) DeleteFile(level uint32, fileNum uint64) error {
	if level >= NumLevels {
		return &errors.InvalidLevelError{Level: level, MaxLevel: NumLevels - 1}
	}
	e.deletedFiles = append(e.deletedFiles, DeleteFileEntry{
		Level:   level,
		FileNum: fileNum,
	})
	return nil
}

// AddedFiles returns an independent slice of all added SSTable entries.
func (e *VersionEdit) AddedFiles() []AddFileEntry {
	out := make([]AddFileEntry, len(e.addedFiles))
	for i, entry := range e.addedFiles {
		out[i] = entry.Clone()
	}
	return out
}

// DeletedFiles returns a copy of all deleted SSTable entries.
func (e *VersionEdit) DeletedFiles() []DeleteFileEntry {
	out := make([]DeleteFileEntry, len(e.deletedFiles))
	copy(out, e.deletedFiles)
	return out
}

// NumAddedFiles returns the number of SSTable additions recorded in the edit.
func (e *VersionEdit) NumAddedFiles() int {
	return len(e.addedFiles)
}

// NumDeletedFiles returns the number of SSTable deletions recorded in the edit.
func (e *VersionEdit) NumDeletedFiles() int {
	return len(e.deletedFiles)
}

// IsEmpty reports whether the VersionEdit contains no changes.
func (e *VersionEdit) IsEmpty() bool {
	return !e.hasNextFileNum && !e.hasLastSeqNum && len(e.deletedFiles) == 0 && len(e.addedFiles) == 0
}

// Reset clears all fields, returning the VersionEdit to an empty state.
func (e *VersionEdit) Reset() {
	e.hasNextFileNum = false
	e.nextFileNum = 0
	e.hasLastSeqNum = false
	e.lastSeqNum = 0
	e.deletedFiles = nil
	e.addedFiles = nil
}

// Clone returns an independent deep copy of the VersionEdit.
func (e *VersionEdit) Clone() *VersionEdit {
	if e == nil {
		return nil
	}
	clone := &VersionEdit{
		hasNextFileNum: e.hasNextFileNum,
		nextFileNum:    e.nextFileNum,
		hasLastSeqNum:  e.hasLastSeqNum,
		lastSeqNum:     e.lastSeqNum,
		deletedFiles:   make([]DeleteFileEntry, len(e.deletedFiles)),
		addedFiles:     make([]AddFileEntry, len(e.addedFiles)),
	}
	copy(clone.deletedFiles, e.deletedFiles)
	for i, a := range e.addedFiles {
		clone.addedFiles[i] = a.Clone()
	}
	return clone
}

// Equal reports whether e and other represent semantically identical edits.
func (e *VersionEdit) Equal(other *VersionEdit) bool {
	if e == nil || other == nil {
		return e == other
	}
	if e.hasNextFileNum != other.hasNextFileNum || e.nextFileNum != other.nextFileNum {
		return false
	}
	if e.hasLastSeqNum != other.hasLastSeqNum || e.lastSeqNum != other.lastSeqNum {
		return false
	}

	// Sort copies canonically to verify logical equality regardless of insertion order
	deletesA := slices.Clone(e.deletedFiles)
	deletesB := slices.Clone(other.deletedFiles)
	sortDeleteEntries(deletesA)
	sortDeleteEntries(deletesB)

	if len(deletesA) != len(deletesB) {
		return false
	}
	for i := range deletesA {
		if !deletesA[i].Equal(deletesB[i]) {
			return false
		}
	}

	addsA := slices.Clone(e.addedFiles)
	addsB := slices.Clone(other.addedFiles)
	sortAddEntries(addsA)
	sortAddEntries(addsB)

	if len(addsA) != len(addsB) {
		return false
	}
	for i := range addsA {
		if !addsA[i].Equal(addsB[i]) {
			return false
		}
	}

	return true
}

// Encode serializes the VersionEdit into a deterministic, canonical byte slice.
//
// Binary Framing Format (P06-S01-M01 Specification):
//
//	Offset 0: Format Version (1 byte, 0x01)
//	Followed by a sequence of TLV (Tag-Length-Value) records in canonical order:
//	  [ Tag (varint) | Length (varint) | Payload (Length bytes) ]
//
// Canonical Tag Order:
//  1. TagNextFileNum (1) - if hasNextFileNum
//  2. TagLastSeqNum (2)  - if hasLastSeqNum
//  3. TagDeleteFile (3)  - sorted by (Level ASC, FileNum ASC)
//  4. TagAddFile (4)     - sorted by (Level ASC, FileNum ASC, ...)
func (e *VersionEdit) Encode() []byte {
	return e.AppendEncode(nil)
}

// AppendEncode serializes the VersionEdit and appends the result to dst, returning
// the extended slice.
func (e *VersionEdit) AppendEncode(dst []byte) []byte {
	dst = append(dst, VersionEditFormatV1)
	if e == nil {
		return dst
	}

	// 1. TagNextFileNum
	if e.hasNextFileNum {
		var pBuf [binary.MaxVarintLen64]byte
		pN := binary.PutVarint64(pBuf[:], e.nextFileNum)
		dst = appendTLV(dst, TagNextFileNum, pBuf[:pN])
	}

	// 2. TagLastSeqNum
	if e.hasLastSeqNum {
		var pBuf [binary.MaxVarintLen64]byte
		pN := binary.PutVarint64(pBuf[:], uint64(e.lastSeqNum))
		dst = appendTLV(dst, TagLastSeqNum, pBuf[:pN])
	}

	// 3. TagDeleteFile (sorted canonically)
	if len(e.deletedFiles) > 0 {
		sortedDeletes := slices.Clone(e.deletedFiles)
		sortDeleteEntries(sortedDeletes)
		for _, d := range sortedDeletes {
			var pBuf [2 * binary.MaxVarintLen64]byte
			n1 := binary.PutVarint64(pBuf[:], uint64(d.Level))
			n2 := binary.PutVarint64(pBuf[n1:], d.FileNum)
			dst = appendTLV(dst, TagDeleteFile, pBuf[:n1+n2])
		}
	}

	// 4. TagAddFile (sorted canonically)
	if len(e.addedFiles) > 0 {
		sortedAdds := slices.Clone(e.addedFiles)
		sortAddEntries(sortedAdds)
		for _, a := range sortedAdds {
			payloadCap := 70 + len(a.Meta.SmallestKey) + len(a.Meta.LargestKey)
			payload := make([]byte, 0, payloadCap)

			var vBuf [binary.MaxVarintLen64]byte

			n := binary.PutVarint64(vBuf[:], uint64(a.Level))
			payload = append(payload, vBuf[:n]...)

			n = binary.PutVarint64(vBuf[:], a.Meta.FileNum)
			payload = append(payload, vBuf[:n]...)

			n = binary.PutVarint64(vBuf[:], a.Meta.FileSize)
			payload = append(payload, vBuf[:n]...)

			n = binary.PutVarint64(vBuf[:], a.Meta.SmallestSeqNum)
			payload = append(payload, vBuf[:n]...)

			n = binary.PutVarint64(vBuf[:], a.Meta.LargestSeqNum)
			payload = append(payload, vBuf[:n]...)

			n = binary.PutVarint64(vBuf[:], uint64(len(a.Meta.SmallestKey)))
			payload = append(payload, vBuf[:n]...)
			payload = append(payload, a.Meta.SmallestKey...)

			n = binary.PutVarint64(vBuf[:], uint64(len(a.Meta.LargestKey)))
			payload = append(payload, vBuf[:n]...)
			payload = append(payload, a.Meta.LargestKey...)

			dst = appendTLV(dst, TagAddFile, payload)
		}
	}

	return dst
}

// DecodeVersionEdit deserializes a binary byte slice into a validated VersionEdit.
//
// Error & Security Guarantees:
//   - Rejects truncated buffers, truncated tags, truncated lengths, and truncated payloads.
//   - Rejects format versions other than VersionEditFormatV1 (0x01).
//   - Rejects duplicate scalar fields (NextFileNum, LastSeqNum).
//   - Rejects invalid levels outside [0, NumLevels-1].
//   - Rejects payload lengths or key lengths exceeding architectural bounds.
//   - Unknown TLV tags with structurally valid lengths are safely skipped.
//   - Fails closed on any error; never returns partially decoded or corrupted edits.
func DecodeVersionEdit(data []byte) (*VersionEdit, error) {
	if len(data) == 0 {
		return nil, &errors.TruncatedVersionEditError{Expected: 1, Actual: 0}
	}
	if len(data) > MaxVersionEditBytes {
		return nil, &errors.CorruptedVersionEditError{
			Offset: 0,
			Reason: fmt.Sprintf("version edit size %d exceeds maximum allowed %d bytes", len(data), MaxVersionEditBytes),
		}
	}
	if data[0] != VersionEditFormatV1 {
		return nil, &errors.UnsupportedVersionEditError{Version: data[0]}
	}

	edit := &VersionEdit{}
	offset := 1
	seenNextFileNum := false
	seenLastSeqNum := false

	for offset < len(data) {
		fieldStart := offset

		// 1. Decode Tag
		tag, nTag, err := binary.GetVarint64(data[offset:])
		if err != nil {
			return nil, &errors.TruncatedVersionEditError{
				Expected: offset + 1,
				Actual:   len(data),
			}
		}
		offset += nTag

		// 2. Decode Length
		if offset >= len(data) {
			return nil, &errors.TruncatedVersionEditError{
				Expected: offset + 1,
				Actual:   len(data),
			}
		}
		length, nLen, err := binary.GetVarint64(data[offset:])
		if err != nil {
			return nil, &errors.TruncatedVersionEditError{
				Expected: offset + 1,
				Actual:   len(data),
			}
		}
		offset += nLen

		// 3. Length Validations
		if length > uint64(MaxFieldPayloadLen) {
			return nil, &errors.CorruptedVersionEditError{
				Offset: int64(fieldStart),
				Reason: fmt.Sprintf("field payload length %d exceeds maximum limit %d", length, MaxFieldPayloadLen),
			}
		}
		if uint64(len(data)-offset) < length {
			return nil, &errors.TruncatedVersionEditError{
				Expected: offset + int(length),
				Actual:   len(data),
			}
		}

		payload := data[offset : offset+int(length)]
		offset += int(length)

		// 4. Process Field
		switch tag {
		case TagNextFileNum:
			if seenNextFileNum {
				return nil, &errors.DuplicateScalarFieldError{Tag: TagNextFileNum, FieldName: "NextFileNum"}
			}
			if len(payload) == 0 {
				return nil, &errors.CorruptedVersionEditError{
					Offset: int64(fieldStart),
					Reason: "empty NextFileNum payload",
				}
			}
			val, n, err := binary.GetVarint64(payload)
			if err != nil || n != len(payload) {
				return nil, &errors.CorruptedVersionEditError{
					Offset: int64(fieldStart),
					Reason: "malformed NextFileNum varint",
				}
			}
			edit.nextFileNum = val
			edit.hasNextFileNum = true
			seenNextFileNum = true

		case TagLastSeqNum:
			if seenLastSeqNum {
				return nil, &errors.DuplicateScalarFieldError{Tag: TagLastSeqNum, FieldName: "LastSeqNum"}
			}
			if len(payload) == 0 {
				return nil, &errors.CorruptedVersionEditError{
					Offset: int64(fieldStart),
					Reason: "empty LastSeqNum payload",
				}
			}
			val, n, err := binary.GetVarint64(payload)
			if err != nil || n != len(payload) {
				return nil, &errors.CorruptedVersionEditError{
					Offset: int64(fieldStart),
					Reason: "malformed LastSeqNum varint",
				}
			}
			edit.lastSeqNum = binary.SeqNum(val)
			edit.hasLastSeqNum = true
			seenLastSeqNum = true

		case TagDeleteFile:
			if len(edit.deletedFiles) >= MaxDeleteFilesPerEdit {
				return nil, &errors.CorruptedVersionEditError{
					Offset: int64(fieldStart),
					Reason: fmt.Sprintf("too many DeleteFile entries (exceeds %d)", MaxDeleteFilesPerEdit),
				}
			}
			pOff := 0
			lvl, n1, err := binary.GetVarint64(payload[pOff:])
			if err != nil {
				return nil, &errors.CorruptedVersionEditError{
					Offset: int64(fieldStart),
					Reason: "malformed DeleteFile level",
				}
			}
			pOff += n1
			if lvl >= NumLevels {
				return nil, &errors.InvalidLevelError{Level: uint32(lvl), MaxLevel: NumLevels - 1}
			}

			if pOff >= len(payload) {
				return nil, &errors.CorruptedVersionEditError{
					Offset: int64(fieldStart),
					Reason: "truncated DeleteFile fileNum",
				}
			}
			fNum, n2, err := binary.GetVarint64(payload[pOff:])
			if err != nil {
				return nil, &errors.CorruptedVersionEditError{
					Offset: int64(fieldStart),
					Reason: "malformed DeleteFile fileNum",
				}
			}
			pOff += n2
			if pOff != len(payload) {
				return nil, &errors.CorruptedVersionEditError{
					Offset: int64(fieldStart),
					Reason: "trailing bytes in DeleteFile payload",
				}
			}
			edit.deletedFiles = append(edit.deletedFiles, DeleteFileEntry{
				Level:   uint32(lvl),
				FileNum: fNum,
			})

		case TagAddFile:
			if len(edit.addedFiles) >= MaxAddFilesPerEdit {
				return nil, &errors.CorruptedVersionEditError{
					Offset: int64(fieldStart),
					Reason: fmt.Sprintf("too many AddFile entries (exceeds %d)", MaxAddFilesPerEdit),
				}
			}
			pOff := 0
			lvl, n, err := binary.GetVarint64(payload[pOff:])
			if err != nil {
				return nil, &errors.CorruptedVersionEditError{Offset: int64(fieldStart), Reason: "malformed AddFile level"}
			}
			pOff += n
			if lvl >= NumLevels {
				return nil, &errors.InvalidLevelError{Level: uint32(lvl), MaxLevel: NumLevels - 1}
			}

			if pOff >= len(payload) {
				return nil, &errors.CorruptedVersionEditError{Offset: int64(fieldStart), Reason: "truncated AddFile fileNum"}
			}
			fNum, n, err := binary.GetVarint64(payload[pOff:])
			if err != nil {
				return nil, &errors.CorruptedVersionEditError{Offset: int64(fieldStart), Reason: "malformed AddFile fileNum"}
			}
			pOff += n

			if pOff >= len(payload) {
				return nil, &errors.CorruptedVersionEditError{Offset: int64(fieldStart), Reason: "truncated AddFile fileSize"}
			}
			fSize, n, err := binary.GetVarint64(payload[pOff:])
			if err != nil {
				return nil, &errors.CorruptedVersionEditError{Offset: int64(fieldStart), Reason: "malformed AddFile fileSize"}
			}
			pOff += n

			if pOff >= len(payload) {
				return nil, &errors.CorruptedVersionEditError{Offset: int64(fieldStart), Reason: "truncated AddFile smallestSeqNum"}
			}
			sSeq, n, err := binary.GetVarint64(payload[pOff:])
			if err != nil {
				return nil, &errors.CorruptedVersionEditError{Offset: int64(fieldStart), Reason: "malformed AddFile smallestSeqNum"}
			}
			pOff += n

			if pOff >= len(payload) {
				return nil, &errors.CorruptedVersionEditError{Offset: int64(fieldStart), Reason: "truncated AddFile largestSeqNum"}
			}
			lSeq, n, err := binary.GetVarint64(payload[pOff:])
			if err != nil {
				return nil, &errors.CorruptedVersionEditError{Offset: int64(fieldStart), Reason: "malformed AddFile largestSeqNum"}
			}
			pOff += n

			if pOff >= len(payload) {
				return nil, &errors.CorruptedVersionEditError{Offset: int64(fieldStart), Reason: "truncated AddFile smallestKeyLen"}
			}
			skLen, n, err := binary.GetVarint64(payload[pOff:])
			if err != nil {
				return nil, &errors.CorruptedVersionEditError{Offset: int64(fieldStart), Reason: "malformed AddFile smallestKeyLen"}
			}
			pOff += n
			if skLen > binary.MaxEncodedInternalKeyLen {
				return nil, &errors.KeyTooLargeError{KeySize: uint32(skLen), MaxSize: binary.MaxEncodedInternalKeyLen}
			}
			if uint64(len(payload)-pOff) < skLen {
				return nil, &errors.CorruptedVersionEditError{Offset: int64(fieldStart), Reason: "truncated AddFile smallestKey payload"}
			}
			smallestKey := bytes.Clone(payload[pOff : pOff+int(skLen)])
			pOff += int(skLen)

			if pOff >= len(payload) {
				return nil, &errors.CorruptedVersionEditError{Offset: int64(fieldStart), Reason: "truncated AddFile largestKeyLen"}
			}
			lkLen, n, err := binary.GetVarint64(payload[pOff:])
			if err != nil {
				return nil, &errors.CorruptedVersionEditError{Offset: int64(fieldStart), Reason: "malformed AddFile largestKeyLen"}
			}
			pOff += n
			if lkLen > binary.MaxEncodedInternalKeyLen {
				return nil, &errors.KeyTooLargeError{KeySize: uint32(lkLen), MaxSize: binary.MaxEncodedInternalKeyLen}
			}
			if uint64(len(payload)-pOff) < lkLen {
				return nil, &errors.CorruptedVersionEditError{Offset: int64(fieldStart), Reason: "truncated AddFile largestKey payload"}
			}
			largestKey := bytes.Clone(payload[pOff : pOff+int(lkLen)])
			pOff += int(lkLen)

			if pOff != len(payload) {
				return nil, &errors.CorruptedVersionEditError{Offset: int64(fieldStart), Reason: "trailing bytes in AddFile payload"}
			}

			edit.addedFiles = append(edit.addedFiles, AddFileEntry{
				Level: uint32(lvl),
				Meta: FileMetadata{
					FileNum:        fNum,
					FileSize:       fSize,
					SmallestKey:    smallestKey,
					LargestKey:     largestKey,
					SmallestSeqNum: sSeq,
					LargestSeqNum:  lSeq,
				},
			})

		default:
			// Forward compatibility: safely skip unknown tags with valid lengths per Section 17.
		}
	}

	return edit, nil
}

func appendTLV(dst []byte, tag uint64, payload []byte) []byte {
	var tagBuf [binary.MaxVarintLen64]byte
	tagN := binary.PutVarint64(tagBuf[:], tag)
	dst = append(dst, tagBuf[:tagN]...)

	var lenBuf [binary.MaxVarintLen64]byte
	lenN := binary.PutVarint64(lenBuf[:], uint64(len(payload)))
	dst = append(dst, lenBuf[:lenN]...)

	return append(dst, payload...)
}

func sortDeleteEntries(entries []DeleteFileEntry) {
	slices.SortFunc(entries, func(a, b DeleteFileEntry) int {
		if c := cmp.Compare(a.Level, b.Level); c != 0 {
			return c
		}
		return cmp.Compare(a.FileNum, b.FileNum)
	})
}

func sortAddEntries(entries []AddFileEntry) {
	slices.SortFunc(entries, func(a, b AddFileEntry) int {
		if c := cmp.Compare(a.Level, b.Level); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Meta.FileNum, b.Meta.FileNum); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Meta.FileSize, b.Meta.FileSize); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Meta.SmallestSeqNum, b.Meta.SmallestSeqNum); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Meta.LargestSeqNum, b.Meta.LargestSeqNum); c != 0 {
			return c
		}
		if c := bytes.Compare(a.Meta.SmallestKey, b.Meta.SmallestKey); c != 0 {
			return c
		}
		return bytes.Compare(a.Meta.LargestKey, b.Meta.LargestKey)
	})
}
