package sstable

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

var _ Iterator = (*TableIterator)(nil)

type iteratorState int

const (
	iterStateUninitialized iteratorState = iota
	iterStateValid
	iterStateExhausted
	iterStateFailed
	iterStateClosed
)

// TableIterator provides an ordered, memory-bounded, streaming sequential iterator over
// the records stored in an immutable SSTable file.
//
// Streaming Traversal & Memory Model:
//   - Memory footprint is bounded to at most one active data block in memory at any point.
//     When advancing to subsequent data blocks, preceding block buffers are released.
//   - Traversal proceeds in strictly increasing canonical order (UserKey ASC, SeqNum DESC, OpType DESC)
//     governed by binary.CompareInternalKey.
//   - Monotonic ordering is verified across consecutive records within each block AND across
//     inter-block boundaries. If any out-of-order record is encountered, the iterator fails closed.
//   - Tombstones (OpTypeDelete) are emitted verbatim as standard records with Value() == nil.
//
// Key & Value Ownership Contract:
//   - Key() returns a defensive deep copy (via binary.InternalKey.Clone()) with an owned UserKey slice.
//   - RawKey() returns an independent copy of the encoded internal key bytes.
//   - Value() returns a defensive copy of the value byte slice.
//   - Callers and concurrent consumers (such as compaction merge priority queues) are guaranteed
//     that advancing or closing the iterator will not mutate or invalidate previously returned keys or values.
//
// Concurrency Model:
//   - Multiple independent TableIterator instances can safely traverse the same TableReader
//     concurrently without locks or interference, because TableReader disk reads execute exclusively
//     via stateless positional ReadAt calls.
//   - Methods on a single TableIterator instance are protected by an internal RWMutex.
type TableIterator struct {
	mu         sync.RWMutex
	reader     *TableReader
	ownsReader bool

	totalBlocks int

	// Current data block state
	blockIndex   int
	blockBuf     []byte
	blockOffset  uint64
	entryDataEnd int

	// Restart offsets in current data block
	restartOffsets []uint32

	// Record decoding state within current data block
	currOffset       int
	currKey          binary.InternalKey
	currKeyBytes     []byte
	currValue        []byte
	reconstructedKey []byte
	prevKey          *binary.InternalKey

	state  iteratorState
	err    error
	closed bool
}

// NewTableIterator creates an unpositioned sequential streaming TableIterator over the provided TableReader.
//
// Ownership:
// The caller retains ownership of reader; calling Close on the returned iterator will release iterator
// buffers without closing the underlying TableReader.
func NewTableIterator(reader *TableReader) (*TableIterator, error) {
	return newTableIterator(reader, false)
}

// OpenTableIterator opens the SSTable file at path with full security validation
// and creates an unpositioned sequential streaming TableIterator.
//
// Ownership:
// The returned iterator assumes ownership of the underlying TableReader; calling Close on the iterator
// will close the TableReader and its file descriptor, preventing resource leaks.
func OpenTableIterator(path string) (*TableIterator, error) {
	reader, err := NewTableReader(path)
	if err != nil {
		return nil, err
	}
	return newTableIterator(reader, true)
}

func newTableIterator(reader *TableReader, ownsReader bool) (*TableIterator, error) {
	if reader == nil {
		return nil, errors.ErrNilReceiver
	}

	reader.mu.RLock()
	closed := reader.closed
	var totalBlocks int
	if reader.index != nil {
		totalBlocks = reader.index.EntryCount()
	}
	reader.mu.RUnlock()

	if closed {
		if ownsReader {
			_ = reader.Close()
		}
		return nil, errors.ErrTableReaderClosed
	}

	return &TableIterator{
		reader:           reader,
		ownsReader:       ownsReader,
		totalBlocks:      totalBlocks,
		blockIndex:       -1,
		reconstructedKey: make([]byte, 0, 64),
		state:            iterStateUninitialized,
	}, nil
}

// Valid reports whether the iterator is currently positioned at a valid record.
func (it *TableIterator) Valid() bool {
	if it == nil {
		return false
	}
	it.mu.RLock()
	defer it.mu.RUnlock()
	return !it.closed && it.state == iterStateValid
}

// Next advances the iterator to the next sequential record in the SSTable.
// Returns true if positioned at a valid record, or false if the table is exhausted or an error occurred.
//
// Failure Contract:
// If a data block is corrupted, truncated, fails CRC checksum verification, or violates canonical key ordering,
// Next() transitions the iterator to failed, returns false, and subsequent calls to Err() return the
// non-nil corruption error. Corruption is never masked as clean EOF.
func (it *TableIterator) Next() bool {
	if it == nil {
		return false
	}
	it.mu.Lock()
	defer it.mu.Unlock()

	if it.closed || it.state == iterStateClosed {
		return false
	}
	if it.state == iterStateFailed || it.state == iterStateExhausted {
		return false
	}

	if it.totalBlocks == 0 {
		it.state = iterStateExhausted
		return false
	}

	switch it.state {
	case iterStateUninitialized:
		if err := it.loadBlock(0); err != nil {
			it.state = iterStateFailed
			it.err = err
			return false
		}
		if err := it.decodeNextRecord(); err != nil {
			it.state = iterStateFailed
			it.err = err
			return false
		}
		it.state = iterStateValid
		return true

	case iterStateValid:
		if it.currOffset < it.entryDataEnd {
			if err := it.decodeNextRecord(); err != nil {
				it.state = iterStateFailed
				it.err = err
				return false
			}
			it.state = iterStateValid
			return true
		}

		if it.currOffset > it.entryDataEnd {
			it.state = iterStateFailed
			it.err = &errors.DataBlockCorruptedError{
				Offset: it.entryOffset(),
				Reason: "entry parsing overran entry data boundary",
			}
			return false
		}

		// Current block exhausted cleanly; advance to next block
		nextBlock := it.blockIndex + 1
		if nextBlock < it.totalBlocks {
			if err := it.loadBlock(nextBlock); err != nil {
				it.state = iterStateFailed
				it.err = err
				return false
			}
			if err := it.decodeNextRecord(); err != nil {
				it.state = iterStateFailed
				it.err = err
				return false
			}
			it.state = iterStateValid
			return true
		}

		// All blocks exhausted; clean EOF
		it.blockBuf = nil
		it.restartOffsets = nil
		it.reconstructedKey = nil
		it.currKey = binary.InternalKey{}
		it.currKeyBytes = nil
		it.currValue = nil
		it.state = iterStateExhausted
		return false

	default:
		return false
	}
}

// Key returns a defensive deep copy of the InternalKey at the current iterator position.
// Returns an empty InternalKey if the iterator is not positioned at a valid record.
func (it *TableIterator) Key() binary.InternalKey {
	if it == nil {
		return binary.InternalKey{}
	}
	it.mu.RLock()
	defer it.mu.RUnlock()
	if it.closed || it.state != iterStateValid {
		return binary.InternalKey{}
	}
	return it.currKey.Clone()
}

// RawKey returns an owned defensive copy of the raw encoded InternalKey bytes at the current position.
// Returns nil if the iterator is not positioned at a valid record.
func (it *TableIterator) RawKey() []byte {
	if it == nil {
		return nil
	}
	it.mu.RLock()
	defer it.mu.RUnlock()
	if it.closed || it.state != iterStateValid || len(it.currKeyBytes) == 0 {
		return nil
	}
	cp := make([]byte, len(it.currKeyBytes))
	copy(cp, it.currKeyBytes)
	return cp
}

// Value returns a defensive copy of the value bytes at the current iterator position.
// Returns nil for tombstones (OpTypeDelete), empty values, or if the iterator is not valid.
func (it *TableIterator) Value() []byte {
	if it == nil {
		return nil
	}
	it.mu.RLock()
	defer it.mu.RUnlock()
	if it.closed || it.state != iterStateValid || len(it.currValue) == 0 {
		return nil
	}
	cp := make([]byte, len(it.currValue))
	copy(cp, it.currValue)
	return cp
}

// Err returns the error encountered during iteration, if any.
// Returns nil if the iterator reached the end of the table cleanly (EOF).
func (it *TableIterator) Err() error {
	if it == nil {
		return errors.ErrNilReceiver
	}
	it.mu.RLock()
	defer it.mu.RUnlock()
	return it.err
}

// Close invalidates the iterator and releases allocated block buffers and state.
// If the iterator owns the underlying TableReader (e.g. created via OpenTableIterator),
// Close also closes the TableReader.
// Calling Close multiple times is safe and idempotent.
func (it *TableIterator) Close() error {
	if it == nil {
		return errors.ErrNilReceiver
	}
	it.mu.Lock()
	defer it.mu.Unlock()

	if it.closed {
		return nil
	}
	it.closed = true
	it.state = iterStateClosed
	it.blockBuf = nil
	it.restartOffsets = nil
	it.reconstructedKey = nil
	it.currKey = binary.InternalKey{}
	it.currKeyBytes = nil
	it.currValue = nil
	it.prevKey = nil

	var closeErr error
	if it.ownsReader && it.reader != nil {
		closeErr = it.reader.Close()
		it.reader = nil
	}
	return closeErr
}

// SeekToFirst positions the iterator at the very first record of the SSTable.
// If the table is empty, the iterator is marked exhausted (Valid() == false) and nil is returned.
func (it *TableIterator) SeekToFirst() error {
	if it == nil {
		return errors.ErrNilReceiver
	}
	it.mu.Lock()
	defer it.mu.Unlock()

	if it.closed {
		return errors.ErrIteratorClosed
	}
	it.err = nil
	it.prevKey = nil

	if it.totalBlocks == 0 {
		it.state = iterStateExhausted
		return nil
	}

	if err := it.loadBlock(0); err != nil {
		it.state = iterStateFailed
		it.err = err
		return err
	}
	if err := it.decodeNextRecord(); err != nil {
		it.state = iterStateFailed
		it.err = err
		return err
	}
	it.state = iterStateValid
	return nil
}

// Seek positions the iterator at the first entry whose UserKey is greater than or equal to userKey.
// If userKey is invalid, the iterator transitions to failed and returns the validation error.
// If all records in the table have UserKey < userKey, the iterator is exhausted (Valid() == false) and returns nil.
func (it *TableIterator) Seek(userKey []byte) error {
	if it == nil {
		return errors.ErrNilReceiver
	}
	if err := binary.ValidateKey(userKey); err != nil {
		it.mu.Lock()
		it.state = iterStateFailed
		it.err = err
		it.mu.Unlock()
		return err
	}

	it.mu.Lock()
	defer it.mu.Unlock()

	if it.closed {
		return errors.ErrIteratorClosed
	}
	it.err = nil
	it.prevKey = nil

	if it.totalBlocks == 0 {
		it.state = iterStateExhausted
		return nil
	}

	entries := it.reader.index.entries
	candidateIdx := sort.Search(len(entries), func(i int) bool {
		return bytes.Compare(entries[i].UserKey(), userKey) >= 0
	})
	if candidateIdx >= len(entries) {
		it.state = iterStateExhausted
		it.blockBuf = nil
		it.restartOffsets = nil
		return nil
	}

	if err := it.loadBlock(candidateIdx); err != nil {
		it.state = iterStateFailed
		it.err = err
		return err
	}

	// Binary search restart points to locate candidate restart interval
	left := 0
	right := len(it.restartOffsets) - 1
	for left < right {
		mid := (left + right + 1) / 2
		midKey, err := it.decodeRestartUserKey(mid)
		if err != nil {
			it.state = iterStateFailed
			it.err = err
			return err
		}
		if bytes.Compare(midKey, userKey) < 0 {
			left = mid
		} else {
			right = mid - 1
		}
	}

	it.currOffset = int(it.restartOffsets[left]) // #nosec G115 -- restart offsets fit in int
	it.reconstructedKey = it.reconstructedKey[:0]
	it.prevKey = nil

	for it.currOffset < it.entryDataEnd {
		if err := it.decodeNextRecord(); err != nil {
			it.state = iterStateFailed
			it.err = err
			return err
		}
		if bytes.Compare(it.currKey.UserKey, userKey) >= 0 {
			it.state = iterStateValid
			return nil
		}
	}

	// Candidate block did not yield target; try next block if present
	nextBlock := candidateIdx + 1
	if nextBlock < it.totalBlocks {
		if err := it.loadBlock(nextBlock); err != nil {
			it.state = iterStateFailed
			it.err = err
			return err
		}
		if err := it.decodeNextRecord(); err != nil {
			it.state = iterStateFailed
			it.err = err
			return err
		}
		it.state = iterStateValid
		return nil
	}

	it.state = iterStateExhausted
	it.blockBuf = nil
	it.restartOffsets = nil
	return nil
}

// SeekInternalKey positions the iterator at the first entry whose InternalKey is greater than or equal
// to target under canonical binary.CompareInternalKey ordering.
func (it *TableIterator) SeekInternalKey(target binary.InternalKey) error {
	if it == nil {
		return errors.ErrNilReceiver
	}
	if err := binary.ValidateKey(target.UserKey); err != nil {
		it.mu.Lock()
		it.state = iterStateFailed
		it.err = err
		it.mu.Unlock()
		return err
	}
	if err := target.OpType.Validate(); err != nil {
		it.mu.Lock()
		it.state = iterStateFailed
		it.err = err
		it.mu.Unlock()
		return err
	}

	it.mu.Lock()
	defer it.mu.Unlock()

	if it.closed {
		return errors.ErrIteratorClosed
	}
	it.err = nil
	it.prevKey = nil

	if it.totalBlocks == 0 {
		it.state = iterStateExhausted
		return nil
	}

	entries := it.reader.index.entries
	candidateIdx := sort.Search(len(entries), func(i int) bool {
		return binary.CompareInternalKey(entries[i].Key, target) >= 0
	})
	if candidateIdx >= len(entries) {
		it.state = iterStateExhausted
		it.blockBuf = nil
		it.restartOffsets = nil
		return nil
	}

	if err := it.loadBlock(candidateIdx); err != nil {
		it.state = iterStateFailed
		it.err = err
		return err
	}

	// Binary search restart points
	left := 0
	right := len(it.restartOffsets) - 1
	for left < right {
		mid := (left + right + 1) / 2
		midKey, err := it.decodeRestartInternalKey(mid)
		if err != nil {
			it.state = iterStateFailed
			it.err = err
			return err
		}
		if binary.CompareInternalKey(midKey, target) < 0 {
			left = mid
		} else {
			right = mid - 1
		}
	}

	it.currOffset = int(it.restartOffsets[left]) // #nosec G115 -- restart offsets fit in int
	it.reconstructedKey = it.reconstructedKey[:0]
	it.prevKey = nil

	for it.currOffset < it.entryDataEnd {
		if err := it.decodeNextRecord(); err != nil {
			it.state = iterStateFailed
			it.err = err
			return err
		}
		if binary.CompareInternalKey(it.currKey, target) >= 0 {
			it.state = iterStateValid
			return nil
		}
	}

	nextBlock := candidateIdx + 1
	if nextBlock < it.totalBlocks {
		if err := it.loadBlock(nextBlock); err != nil {
			it.state = iterStateFailed
			it.err = err
			return err
		}
		if err := it.decodeNextRecord(); err != nil {
			it.state = iterStateFailed
			it.err = err
			return err
		}
		it.state = iterStateValid
		return nil
	}

	it.state = iterStateExhausted
	it.blockBuf = nil
	it.restartOffsets = nil
	return nil
}

func (it *TableIterator) entryOffset() uint64 {
	if it.currOffset < 0 {
		return it.blockOffset
	}
	return it.blockOffset + uint64(it.currOffset) // #nosec G115 -- guarded by currOffset >= 0
}

func (it *TableIterator) restartEntryOffset(off int) uint64 {
	if off < 0 {
		return it.blockOffset
	}
	return it.blockOffset + uint64(off) // #nosec G115 -- guarded by off >= 0
}

// loadBlock reads the data block at index idx from disk, validates its CRC32 checksum,
// parses its restart offsets, and prepares the iterator for decoding records from the block.
func (it *TableIterator) loadBlock(idx int) error {
	if idx < 0 || idx >= it.totalBlocks {
		return &errors.InvalidBlockHandleError{
			Reason: fmt.Sprintf("block index %d out of bounds (total: %d)", idx, it.totalBlocks),
		}
	}

	entry := it.reader.index.entries[idx]
	blockBuf, err := it.reader.ReadDataBlock(entry.Handle)
	if err != nil {
		return err
	}

	// Minimum data block trailer: RestartCount (4B) + CRC32 (4B) = 8B
	if len(blockBuf) < 8 {
		return &errors.DataBlockCorruptedError{
			Offset: entry.Handle.Offset,
			Reason: "data block buffer smaller than minimum trailer length",
		}
	}

	// 1. Verify CRC32-IEEE checksum
	expectedCRC := binary.GetUint32(blockBuf[len(blockBuf)-4:])
	actualCRC := binary.Checksum(blockBuf[:len(blockBuf)-4])
	if expectedCRC != actualCRC {
		var offInt64 int64
		if entry.Handle.Offset <= math.MaxInt64 {
			offInt64 = int64(entry.Handle.Offset) // #nosec G115 -- guarded by <= MaxInt64
		}
		return &errors.ChecksumMismatchError{
			Offset:   offInt64,
			Expected: expectedCRC,
			Actual:   actualCRC,
		}
	}

	// 2. Parse and validate restart count
	restartCount := binary.GetUint32(blockBuf[len(blockBuf)-8 : len(blockBuf)-4])
	if restartCount == 0 {
		return &errors.DataBlockCorruptedError{
			Offset: entry.Handle.Offset,
			Reason: "data block restart count is zero",
		}
	}
	if restartCount > MaxRestartCount {
		return &errors.DataBlockCorruptedError{
			Offset: entry.Handle.Offset,
			Reason: fmt.Sprintf("data block restart count %d exceeds MaxRestartCount %d", restartCount, MaxRestartCount),
		}
	}

	// 3. Verify restart array boundaries
	restartBytes := uint64(restartCount) * 4
	if restartBytes+8 > uint64(len(blockBuf)) {
		return &errors.DataBlockCorruptedError{
			Offset: entry.Handle.Offset,
			Reason: "data block restart array exceeds block size",
		}
	}
	if restartBytes > math.MaxInt {
		return &errors.DataBlockCorruptedError{
			Offset: entry.Handle.Offset,
			Reason: "restart array size exceeds architecture integer bounds",
		}
	}
	entryDataEnd := len(blockBuf) - 8 - int(restartBytes) // #nosec G115 -- guarded by <= MaxInt

	// 4. Parse and validate restart offsets
	restartOffsetsStart := entryDataEnd
	restartOffsets := make([]uint32, restartCount)
	for i := 0; i < int(restartCount); i++ {
		offPos := restartOffsetsStart + i*4
		off := binary.GetUint32(blockBuf[offPos : offPos+4])

		if i == 0 && off != 0 {
			return &errors.DataBlockCorruptedError{
				Offset: entry.Handle.Offset,
				Reason: "first restart offset must be zero",
			}
		}
		if i > 0 && off <= restartOffsets[i-1] {
			return &errors.DataBlockCorruptedError{
				Offset: entry.Handle.Offset,
				Reason: "restart offsets are not strictly increasing",
			}
		}
		if uint64(off) >= uint64(entryDataEnd) {
			return &errors.DataBlockCorruptedError{
				Offset: entry.Handle.Offset,
				Reason: "restart offset exceeds entry data boundary",
			}
		}
		restartOffsets[i] = off
	}

	it.blockIndex = idx
	it.blockBuf = blockBuf
	it.blockOffset = entry.Handle.Offset
	it.entryDataEnd = entryDataEnd
	it.restartOffsets = restartOffsets
	it.currOffset = 0
	it.reconstructedKey = it.reconstructedKey[:0]

	return nil
}

// decodeNextRecord parses the prefix-compressed entry at currOffset in the current data block.
func (it *TableIterator) decodeNextRecord() error {
	if it.currOffset >= it.entryDataEnd {
		return &errors.DataBlockCorruptedError{
			Offset: it.entryOffset(),
			Reason: "attempted to decode record past entry data boundary",
		}
	}

	entrySlice := it.blockBuf[it.currOffset:it.entryDataEnd]

	// 1. Decode varint lengths: shared, unshared, valueLength
	shared, n1, err := binary.GetVarint64Canonical(entrySlice)
	if err != nil {
		return &errors.DataBlockCorruptedError{
			Offset: it.entryOffset(),
			Reason: "varint shared key length corrupted",
		}
	}

	unshared, n2, err := binary.GetVarint64Canonical(entrySlice[n1:])
	if err != nil {
		return &errors.DataBlockCorruptedError{
			Offset: it.entryOffset(),
			Reason: "varint unshared key length corrupted",
		}
	}

	valueLen, n3, err := binary.GetVarint64Canonical(entrySlice[n1+n2:])
	if err != nil {
		return &errors.DataBlockCorruptedError{
			Offset: it.entryOffset(),
			Reason: "varint value length corrupted",
		}
	}

	// 2. Validate payload limits
	if unshared > binary.MaxEncodedInternalKeyLen {
		return &errors.DataBlockCorruptedError{
			Offset: it.entryOffset(),
			Reason: "entry unshared key length exceeds MaxEncodedInternalKeyLen",
		}
	}
	if valueLen > binary.MaxValueLen {
		return &errors.DataBlockCorruptedError{
			Offset: it.entryOffset(),
			Reason: "entry value length exceeds MaxValueLen",
		}
	}

	hdrLen := n1 + n2 + n3
	totalPayload := uint64(hdrLen) + unshared + valueLen // #nosec G115 -- hdrLen is <= 30
	if totalPayload > uint64(len(entrySlice)) {
		return &errors.DataBlockCorruptedError{
			Offset: it.entryOffset(),
			Reason: "entry payload exceeds entry data boundary",
		}
	}

	// 3. Check restart point invariant: restart entries must have shared == 0
	if it.isRestartOffset(it.currOffset) && shared != 0 {
		return &errors.DataBlockCorruptedError{
			Offset: it.entryOffset(),
			Reason: "shared key length at restart point must be zero",
		}
	}

	// 4. Validate shared prefix bounds
	if shared > uint64(len(it.reconstructedKey)) {
		return &errors.DataBlockCorruptedError{
			Offset: it.entryOffset(),
			Reason: "shared prefix length exceeds reconstructed key length",
		}
	}

	// 5. Reconstruct InternalKey
	deltaKey := entrySlice[hdrLen : hdrLen+int(unshared)]
	it.reconstructedKey = append(it.reconstructedKey[:shared], deltaKey...)

	if len(it.reconstructedKey) < binary.MinKeyLen+binary.InternalKeyTrailerLen || len(it.reconstructedKey) > binary.MaxEncodedInternalKeyLen {
		return &errors.DataBlockCorruptedError{
			Offset: it.entryOffset(),
			Reason: "reconstructed key length outside valid internal key boundaries",
		}
	}

	currIK, err := binary.DecodeInternalKey(it.reconstructedKey)
	if err != nil {
		return &errors.DataBlockCorruptedError{
			Offset: it.entryOffset(),
			Reason: fmt.Sprintf("invalid reconstructed internal key: %v", err),
		}
	}

	// 6. Monotonic ordering verification (strictly increasing canonical InternalKey order)
	if it.prevKey != nil {
		if binary.CompareInternalKey(*it.prevKey, currIK) >= 0 {
			return &errors.DataBlockCorruptedError{
				Offset: it.entryOffset(),
				Reason: "entries violate strictly increasing canonical key ordering",
			}
		}
	}

	// 7. Verify entry key does not exceed the block's largest key in the index
	blockEntry := it.reader.index.entries[it.blockIndex]
	if binary.CompareInternalKey(currIK, blockEntry.Key) > 0 {
		return &errors.DataBlockCorruptedError{
			Offset: it.entryOffset(),
			Reason: "entry key exceeds block's largest key in index",
		}
	}

	// 8. Extract value
	valStart := hdrLen + int(unshared)
	valBytes := entrySlice[valStart : valStart+int(valueLen)]
	if currIK.OpType == binary.OpTypeDelete || len(valBytes) == 0 {
		it.currValue = nil
	} else {
		valCopy := make([]byte, len(valBytes))
		copy(valCopy, valBytes)
		it.currValue = valCopy
	}

	// 9. Update state
	it.currKey = currIK
	currCopy := currIK
	it.prevKey = &currCopy

	rawKeyCopy := make([]byte, len(it.reconstructedKey))
	copy(rawKeyCopy, it.reconstructedKey)
	it.currKeyBytes = rawKeyCopy

	it.currOffset += hdrLen + int(unshared) + int(valueLen)

	// 10. If this is the final entry in the block, verify it exactly matches the index entry's largest key
	if it.currOffset == it.entryDataEnd {
		if binary.CompareInternalKey(currIK, blockEntry.Key) != 0 {
			return &errors.DataBlockCorruptedError{
				Offset: it.entryOffset(),
				Reason: "final entry key in block does not match index entry largest key",
			}
		}
	}

	return nil
}

// isRestartOffset reports whether off is one of the recorded restart offsets in the current block.
func (it *TableIterator) isRestartOffset(off int) bool {
	if off < 0 || off > math.MaxUint32 {
		return false
	}
	target := uint32(off) // #nosec G115 -- guarded by off >= 0 and off <= math.MaxUint32
	idx := sort.Search(len(it.restartOffsets), func(i int) bool {
		return it.restartOffsets[i] >= target
	})
	return idx < len(it.restartOffsets) && it.restartOffsets[idx] == target
}

// decodeRestartUserKey decodes the user key portion of the entry at restartOffsets[restartIdx].
func (it *TableIterator) decodeRestartUserKey(restartIdx int) ([]byte, error) {
	ik, err := it.decodeRestartInternalKey(restartIdx)
	if err != nil {
		return nil, err
	}
	return ik.UserKey, nil
}

// decodeRestartInternalKey decodes the full InternalKey of the entry at restartOffsets[restartIdx].
func (it *TableIterator) decodeRestartInternalKey(restartIdx int) (binary.InternalKey, error) {
	if restartIdx < 0 || restartIdx >= len(it.restartOffsets) {
		return binary.InternalKey{}, &errors.DataBlockCorruptedError{
			Offset: it.blockOffset,
			Reason: "restart index out of bounds",
		}
	}
	off := int(it.restartOffsets[restartIdx]) // #nosec G115 -- restart offset fits in int
	entrySlice := it.blockBuf[off:it.entryDataEnd]

	shared, n1, err := binary.GetVarint64Canonical(entrySlice)
	if err != nil {
		return binary.InternalKey{}, &errors.DataBlockCorruptedError{
			Offset: it.restartEntryOffset(off),
			Reason: "varint shared key length corrupted at restart point",
		}
	}
	if shared != 0 {
		return binary.InternalKey{}, &errors.DataBlockCorruptedError{
			Offset: it.restartEntryOffset(off),
			Reason: "shared key length at restart point must be zero",
		}
	}

	unshared, n2, err := binary.GetVarint64Canonical(entrySlice[n1:])
	if err != nil {
		return binary.InternalKey{}, &errors.DataBlockCorruptedError{
			Offset: it.restartEntryOffset(off),
			Reason: "varint unshared key length corrupted at restart point",
		}
	}

	valueLen, n3, err := binary.GetVarint64Canonical(entrySlice[n1+n2:])
	if err != nil {
		return binary.InternalKey{}, &errors.DataBlockCorruptedError{
			Offset: it.restartEntryOffset(off),
			Reason: "varint value length corrupted at restart point",
		}
	}

	if unshared < binary.MinKeyLen+binary.InternalKeyTrailerLen || unshared > binary.MaxEncodedInternalKeyLen {
		return binary.InternalKey{}, &errors.DataBlockCorruptedError{
			Offset: it.restartEntryOffset(off),
			Reason: "restart entry key length outside valid internal key boundaries",
		}
	}
	if valueLen > binary.MaxValueLen {
		return binary.InternalKey{}, &errors.DataBlockCorruptedError{
			Offset: it.restartEntryOffset(off),
			Reason: "restart entry value length exceeds MaxValueLen",
		}
	}

	hdrLen := n1 + n2 + n3
	totalPayload := uint64(hdrLen) + unshared + valueLen // #nosec G115 -- hdrLen is <= 30
	if totalPayload > uint64(len(entrySlice)) {
		return binary.InternalKey{}, &errors.DataBlockCorruptedError{
			Offset: it.restartEntryOffset(off),
			Reason: "restart entry payload exceeds entry data boundary",
		}
	}

	keyBytes := entrySlice[hdrLen : hdrLen+int(unshared)]
	return binary.DecodeInternalKey(keyBytes)
}
