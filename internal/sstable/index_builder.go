package sstable

import (
	"bytes"
	"math"
	"sort"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

const (
	// IndexTrailerSize is the serialized length of the trailing entry count and CRC32 fields.
	// 4 bytes (Entry Count, uint32 Big-Endian) + 4 bytes (CRC32-IEEE, uint32 Big-Endian) = 8 bytes.
	IndexTrailerSize = 8
)

// IndexEntry represents a single mapping in an SSTable sparse block index,
// pairing the largest key contained in a data block with its physical BlockHandle.
type IndexEntry struct {
	LargestKey []byte
	Handle     BlockHandle
}

// Clone returns an independent deep copy of the IndexEntry with an owned key slice.
func (e IndexEntry) Clone() IndexEntry {
	if e.LargestKey == nil {
		return IndexEntry{
			LargestKey: nil,
			Handle:     e.Handle,
		}
	}
	k := make([]byte, len(e.LargestKey))
	copy(k, e.LargestKey)
	return IndexEntry{
		LargestKey: k,
		Handle:     e.Handle,
	}
}

// UserKey returns the bare user key portion of LargestKey.
// If LargestKey is an encoded InternalKey, it extracts ik.UserKey.
// Otherwise, it returns LargestKey as-is.
func (e IndexEntry) UserKey() []byte {
	ik, err := binary.DecodeInternalKey(e.LargestKey)
	if err == nil {
		return ik.UserKey
	}
	return e.LargestKey
}

// IndexBuilder constructs the sparse Two-Level Block Index for an SSTable.
//
// In Lattice's SSTable architecture (ADR-004), a sparse block index records exactly one entry
// per data block: the block's largest key and its physical file handle (offset and size).
//
// Serialized Index Block Layout:
//
//	+-----------------------------------------------------------+
//	| Entry Data Region                                         |
//	|   Entry 0: KeyLen (varint), KeyBytes, BlockHandle (16B)   |
//	|   Entry 1: KeyLen (varint), KeyBytes, BlockHandle (16B)   |
//	|   ...                                                     |
//	|   Entry N-1                                               |
//	+-----------------------------------------------------------+
//	| Entry Offsets Region (uint32 * N, Big-Endian)             |
//	|   Offset 0 (uint32)                                       |
//	|   Offset 1 (uint32)                                       |
//	|   ...                                                     |
//	+-----------------------------------------------------------+
//	| Entry Count (uint32, Big-Endian)                          |
//	+-----------------------------------------------------------+
//	| CRC32-IEEE  (uint32, Big-Endian)                          |
//	+-----------------------------------------------------------+
//
// Invariants:
//   - Exactly one index entry per data block.
//   - Monotonic Key Ordering: Largest keys must be added in strictly increasing canonical order.
//   - Failure Atomicity: If AddBlock fails (e.g. invalid handle or unsorted key),
//     the builder's state is completely unmodified.
//   - Caller Isolation: Input keys and handles are defensively copied; outputs are defensively copied.
//   - Binary Determinism: Identical entries added in identical sequence produce byte-for-byte identical output.
//   - Repeated Finish Idempotence: Calling Finish() multiple times returns identical byte slices.
//
// Concurrency:
// IndexBuilder is single-threaded and not safe for concurrent use by multiple goroutines.
type IndexBuilder struct {
	entries     []IndexEntry
	buf         []byte
	offsets     []uint32
	prevKey     []byte
	scratchKey  []byte
	finished    bool
	finishedBuf []byte
}

// NewIndexBuilder creates an initialized, empty IndexBuilder.
func NewIndexBuilder() *IndexBuilder {
	return &IndexBuilder{
		entries:    make([]IndexEntry, 0, 64),
		buf:        make([]byte, 0, 4096),
		offsets:    make([]uint32, 0, 64),
		prevKey:    make([]byte, 0, 64),
		scratchKey: make([]byte, 0, 64),
	}
}

// AddBlock records an index entry for an emitted data block.
//
// Contract:
//   - largestKey must be non-empty (1..65,535 bytes).
//   - largestKey must sort strictly after the previous block's largest key.
//   - handle must be valid (Size > 0 and Offset + Size does not overflow uint64).
//   - If the builder has already been finished, returns errors.ErrIndexFinished.
//   - Guarantees failure atomicity: failed AddBlock calls leave builder state completely untouched.
func (b *IndexBuilder) AddBlock(largestKey []byte, handle BlockHandle) error {
	if b == nil {
		return errors.ErrNilReceiver
	}
	if b.finished {
		return errors.ErrIndexFinished
	}
	if len(largestKey) == 0 {
		return errors.ErrEmptyKey
	}
	if len(largestKey) > binary.MaxEncodedInternalKeyLen {
		return &errors.KeyTooLargeError{
			KeySize: uint32(len(largestKey)),
			MaxSize: binary.MaxEncodedInternalKeyLen,
		}
	}
	if err := handle.Validate(); err != nil {
		return err
	}

	// Canonical ordering check
	if len(b.entries) > 0 {
		if compareIndexEntryKeys(b.prevKey, largestKey) >= 0 {
			return &errors.KeyOutOfOrderError{
				PrevKeyLen: len(b.prevKey),
				CurrKeyLen: len(largestKey),
			}
		}
	}

	// Calculate entry serialization size: varint key length + key bytes + 16-byte BlockHandle
	var varintBuf [binary.MaxVarintLen64]byte
	n := binary.PutVarint64(varintBuf[:], uint64(len(largestKey)))
	entryBytes := n + len(largestKey) + BlockHandleSize

	// Check 32-bit addressable capacity overflow for entry data + metadata trailer
	projectedOffsets := len(b.offsets) + 1
	projectedTrailerBytes := uint64(projectedOffsets)*4 + uint64(IndexTrailerSize)
	if uint64(len(b.buf))+uint64(entryBytes)+projectedTrailerBytes > math.MaxUint32 {
		return errors.ErrBlockOverflow
	}

	currentOffset := uint32(len(b.buf))

	// Commit entry bytes to buffer
	b.buf = append(b.buf, varintBuf[:n]...)
	b.buf = append(b.buf, largestKey...)
	b.buf = handle.AppendTo(b.buf)

	// Commit offset
	b.offsets = append(b.offsets, currentOffset)

	// Commit in-memory entry with defensive copy
	keyCopy := make([]byte, len(largestKey))
	copy(keyCopy, largestKey)
	b.entries = append(b.entries, IndexEntry{
		LargestKey: keyCopy,
		Handle:     handle,
	})

	// Update state
	b.prevKey = append(b.prevKey[:0], largestKey...)
	return nil
}

// AddBlockKey is a convenience method that appends an index entry using an InternalKey struct.
// The key is encoded via binary.AppendInternalKey and delegated to AddBlock.
func (b *IndexBuilder) AddBlockKey(key binary.InternalKey, handle BlockHandle) error {
	if b == nil {
		return errors.ErrNilReceiver
	}
	b.scratchKey = binary.AppendInternalKey(b.scratchKey[:0], key)
	return b.AddBlock(b.scratchKey, handle)
}

// Finish seals the IndexBuilder and returns an owned defensive copy of the fully serialized
// SSTable index block, including entry data, offset array, entry count, and CRC32-IEEE checksum trailer.
//
// Subsequent AddBlock operations are rejected with errors.ErrIndexFinished.
// Repeated Finish calls are idempotent and return identical byte slices.
// If the builder is empty (zero blocks added), Finish returns an empty slice ([]byte{}).
func (b *IndexBuilder) Finish() []byte {
	if b == nil {
		return nil
	}
	if b.finished {
		if len(b.finishedBuf) == 0 {
			return []byte{}
		}
		out := make([]byte, len(b.finishedBuf))
		copy(out, b.finishedBuf)
		return out
	}

	b.finished = true
	if len(b.entries) == 0 {
		b.finishedBuf = nil
		return []byte{}
	}

	// Serialize trailer into b.buf:
	// 1. Entry offsets (uint32 each, Big-Endian)
	var uint32Buf [4]byte
	for _, offset := range b.offsets {
		binary.PutUint32(uint32Buf[:], offset)
		b.buf = append(b.buf, uint32Buf[:]...)
	}

	// 2. Entry count (uint32, Big-Endian)
	binary.PutUint32(uint32Buf[:], uint32(len(b.entries)))
	b.buf = append(b.buf, uint32Buf[:]...)

	// 3. CRC32-IEEE checksum computed over [entry data || offsets || entry count]
	checksum := binary.Checksum(b.buf)
	binary.PutUint32(uint32Buf[:], checksum)
	b.buf = append(b.buf, uint32Buf[:]...)

	// Cache finished bytes for idempotency
	b.finishedBuf = b.buf

	// Return owned defensive copy
	out := make([]byte, len(b.finishedBuf))
	copy(out, b.finishedBuf)
	return out
}

// Reset resets the IndexBuilder to its initial empty state, retaining allocated slice
// capacities to eliminate garbage-collection overhead across successive SSTables.
func (b *IndexBuilder) Reset() {
	if b == nil {
		return
	}
	b.entries = b.entries[:0]
	b.offsets = b.offsets[:0]
	b.buf = b.buf[:0]
	b.prevKey = b.prevKey[:0]
	b.scratchKey = b.scratchKey[:0]
	b.finished = false
	b.finishedBuf = nil
}

// EntryCount returns the number of data blocks recorded in this index.
func (b *IndexBuilder) EntryCount() int {
	if b == nil {
		return 0
	}
	return len(b.entries)
}

// IsEmpty reports whether the index contains zero entries.
func (b *IndexBuilder) IsEmpty() bool {
	if b == nil {
		return true
	}
	return len(b.entries) == 0
}

// Finished reports whether the builder has transitioned to the sealed/finished state.
func (b *IndexBuilder) Finished() bool {
	if b == nil {
		return false
	}
	return b.finished
}

// Entries returns a defensive copy of all recorded index entries.
func (b *IndexBuilder) Entries() []IndexEntry {
	if b == nil || len(b.entries) == 0 {
		return []IndexEntry{}
	}
	out := make([]IndexEntry, len(b.entries))
	for i, entry := range b.entries {
		out[i] = entry.Clone()
	}
	return out
}

// FindBlock performs binary search over the in-memory index entries to find the single candidate
// data block that could contain targetUserKey.
//
// In an SSTable sparse index, this compares the user key component of each block's LargestKey
// against targetUserKey.
// If targetUserKey is strictly greater than all largest keys in the index, false is returned,
// proving the key cannot exist in this SSTable without performing any disk I/O.
func (b *IndexBuilder) FindBlock(targetUserKey []byte) (BlockHandle, bool) {
	if b == nil || len(b.entries) == 0 || len(targetUserKey) == 0 {
		return BlockHandle{}, false
	}
	idx := sort.Search(len(b.entries), func(i int) bool {
		return bytes.Compare(b.entries[i].UserKey(), targetUserKey) >= 0
	})
	if idx >= len(b.entries) {
		return BlockHandle{}, false
	}
	return b.entries[idx].Handle, true
}

// FindBlockKey performs binary search over the in-memory index entries when the search target
// is a structured binary.InternalKey.
func (b *IndexBuilder) FindBlockKey(key binary.InternalKey) (BlockHandle, bool) {
	if b == nil || len(b.entries) == 0 {
		return BlockHandle{}, false
	}
	idx := sort.Search(len(b.entries), func(i int) bool {
		entryIK, err := binary.DecodeInternalKey(b.entries[i].LargestKey)
		if err != nil {
			return bytes.Compare(b.entries[i].LargestKey, key.UserKey) >= 0
		}
		return binary.CompareInternalKey(entryIK, key) >= 0
	})
	if idx >= len(b.entries) {
		return BlockHandle{}, false
	}
	return b.entries[idx].Handle, true
}

// FindBlockInternalKey performs binary search over the in-memory index entries when the search target
// is an encoded InternalKey byte slice.
func (b *IndexBuilder) FindBlockInternalKey(targetInternalKey []byte) (BlockHandle, bool) {
	ik, err := binary.DecodeInternalKey(targetInternalKey)
	if err != nil {
		return b.FindBlock(targetInternalKey)
	}
	return b.FindBlockKey(ik)
}

// CurrentSizeEstimate returns the estimated total size of the finished index block in bytes,
// including entry data, offset array, entry count, and CRC32 checksum trailer.
func (b *IndexBuilder) CurrentSizeEstimate() int {
	if b == nil {
		return 0
	}
	if b.finished {
		return len(b.finishedBuf)
	}
	if len(b.buf) == 0 {
		return 0
	}
	return len(b.buf) + (len(b.offsets) * 4) + IndexTrailerSize
}

// BlockIndex represents a parsed, immutable RAM-resident SSTable sparse index block.
type BlockIndex struct {
	entries []IndexEntry
}

// DecodeBlockIndex parses and validates a serialized SSTable index block from raw bytes.
//
// Validation Contract:
//   - If len(data) == 0: returns empty index, nil error.
//   - If len(data) < IndexTrailerSize (8 bytes): returns errors.ErrIndexBlockTruncated.
//   - Verifies CRC32-IEEE checksum over [Entry Data || Offsets || Count]. Returns *errors.ChecksumMismatchError on mismatch.
//   - Verifies entry count does not exceed buffer capacity. Returns errors.ErrIndexBlockCorrupted on violation.
//   - Verifies offsets are strictly monotonic, within bounds, and offsets[0] == 0 for non-empty blocks.
//   - Parses every entry, validating varint key lengths, key bytes, 16-byte BlockHandle bounds,
//     and strictly increasing key ordering across entries.
func DecodeBlockIndex(data []byte) (*BlockIndex, error) {
	if len(data) == 0 {
		return &BlockIndex{entries: []IndexEntry{}}, nil
	}
	if len(data) < IndexTrailerSize {
		return nil, errors.ErrIndexBlockTruncated
	}

	// 1. Verify CRC32-IEEE checksum
	expectedCRC := binary.GetUint32(data[len(data)-4:])
	actualCRC := binary.Checksum(data[:len(data)-4])
	if expectedCRC != actualCRC {
		return nil, &errors.ChecksumMismatchError{
			Offset:   int64(len(data) - 4),
			Expected: expectedCRC,
			Actual:   actualCRC,
		}
	}

	// 2. Read Entry Count
	entryCount := binary.GetUint32(data[len(data)-8 : len(data)-4])
	if entryCount == 0 {
		if len(data) != IndexTrailerSize {
			return nil, errors.ErrIndexBlockCorrupted
		}
		return &BlockIndex{entries: []IndexEntry{}}, nil
	}

	// 3. Verify offsets trailer bounds
	offsetsByteLen := uint64(entryCount) * 4
	if offsetsByteLen+uint64(IndexTrailerSize) > uint64(len(data)) {
		return nil, errors.ErrIndexBlockCorrupted
	}

	offsetsStart := len(data) - IndexTrailerSize - int(offsetsByteLen)

	// 4. Parse and validate offset array
	offsets := make([]uint32, entryCount)
	for i := 0; i < int(entryCount); i++ {
		offsetPos := offsetsStart + (i * 4)
		offsets[i] = binary.GetUint32(data[offsetPos : offsetPos+4])

		if i == 0 && offsets[0] != 0 {
			return nil, &errors.IndexBlockCorruptedError{Reason: "first entry offset is not zero"}
		}
		if i > 0 && offsets[i] <= offsets[i-1] {
			return nil, &errors.IndexBlockCorruptedError{Reason: "entry offsets are not strictly increasing"}
		}
		if int(offsets[i]) >= offsetsStart {
			return nil, &errors.IndexBlockCorruptedError{Reason: "entry offset exceeds data region boundary"}
		}
	}

	// 5. Decode entries
	entries := make([]IndexEntry, entryCount)
	for i := 0; i < int(entryCount); i++ {
		start := int(offsets[i])
		var end int
		if i+1 < int(entryCount) {
			end = int(offsets[i+1])
		} else {
			end = offsetsStart
		}

		if start >= end || end > offsetsStart {
			return nil, &errors.IndexBlockCorruptedError{Reason: "invalid entry slice boundaries"}
		}

		entrySlice := data[start:end]
		keyLen, n, err := binary.GetVarint64(entrySlice)
		if err != nil {
			return nil, &errors.IndexBlockCorruptedError{Reason: "varint key length truncated or invalid"}
		}
		if keyLen == 0 || keyLen > binary.MaxEncodedInternalKeyLen {
			return nil, &errors.IndexBlockCorruptedError{Reason: "key length outside valid boundaries"}
		}

		expectedTotal := uint64(n) + keyLen + BlockHandleSize
		if expectedTotal != uint64(len(entrySlice)) {
			return nil, &errors.IndexBlockCorruptedError{Reason: "entry payload length mismatch"}
		}

		keyBytes := entrySlice[n : n+int(keyLen)]
		handleBytes := entrySlice[n+int(keyLen):]

		handle, err := DecodeBlockHandle(handleBytes)
		if err != nil {
			return nil, err
		}

		// Strictly increasing key order verification
		if i > 0 {
			if compareIndexEntryKeys(entries[i-1].LargestKey, keyBytes) >= 0 {
				return nil, &errors.IndexBlockCorruptedError{Reason: "index entries violate strictly increasing key ordering"}
			}
		}

		keyCopy := make([]byte, len(keyBytes))
		copy(keyCopy, keyBytes)
		entries[i] = IndexEntry{
			LargestKey: keyCopy,
			Handle:     handle,
		}
	}

	return &BlockIndex{entries: entries}, nil
}

// EntryCount returns the number of index entries in this block index.
func (idx *BlockIndex) EntryCount() int {
	if idx == nil {
		return 0
	}
	return len(idx.entries)
}

// IsEmpty reports whether the index contains zero entries.
func (idx *BlockIndex) IsEmpty() bool {
	if idx == nil {
		return true
	}
	return len(idx.entries) == 0
}

// Entries returns a defensive copy of the index entries.
func (idx *BlockIndex) Entries() []IndexEntry {
	if idx == nil || len(idx.entries) == 0 {
		return []IndexEntry{}
	}
	out := make([]IndexEntry, len(idx.entries))
	for i, entry := range idx.entries {
		out[i] = entry.Clone()
	}
	return out
}

// FindBlock performs binary search over the index entries to find the single candidate
// data block that could contain targetUserKey.
//
// In an SSTable sparse index, this compares the user key component of each block's LargestKey
// against targetUserKey.
// Returns (handle, true) if found, (BlockHandle{}, false) if targetUserKey > all keys in the SSTable.
func (idx *BlockIndex) FindBlock(targetUserKey []byte) (BlockHandle, bool) {
	if idx == nil || len(idx.entries) == 0 || len(targetUserKey) == 0 {
		return BlockHandle{}, false
	}
	searchIdx := sort.Search(len(idx.entries), func(i int) bool {
		return bytes.Compare(idx.entries[i].UserKey(), targetUserKey) >= 0
	})
	if searchIdx >= len(idx.entries) {
		return BlockHandle{}, false
	}
	return idx.entries[searchIdx].Handle, true
}

// FindBlockKey performs binary search over the index entries when looking up by a structured InternalKey.
func (idx *BlockIndex) FindBlockKey(key binary.InternalKey) (BlockHandle, bool) {
	if idx == nil || len(idx.entries) == 0 {
		return BlockHandle{}, false
	}
	searchIdx := sort.Search(len(idx.entries), func(i int) bool {
		entryIK, err := binary.DecodeInternalKey(idx.entries[i].LargestKey)
		if err != nil {
			return bytes.Compare(idx.entries[i].LargestKey, key.UserKey) >= 0
		}
		return binary.CompareInternalKey(entryIK, key) >= 0
	})
	if searchIdx >= len(idx.entries) {
		return BlockHandle{}, false
	}
	return idx.entries[searchIdx].Handle, true
}

// FindBlockInternalKey performs binary search over the index entries when looking up by an encoded InternalKey byte slice.
func (idx *BlockIndex) FindBlockInternalKey(targetInternalKey []byte) (BlockHandle, bool) {
	ik, err := binary.DecodeInternalKey(targetInternalKey)
	if err != nil {
		return idx.FindBlock(targetInternalKey)
	}
	return idx.FindBlockKey(ik)
}

// compareIndexEntryKeys compares two index entry largest keys for strictly monotonic ordering.
func compareIndexEntryKeys(a, b []byte) int {
	ikA, errA := binary.DecodeInternalKey(a)
	ikB, errB := binary.DecodeInternalKey(b)
	if errA == nil && errB == nil {
		return binary.CompareInternalKey(ikA, ikB)
	}
	return bytes.Compare(a, b)
}
