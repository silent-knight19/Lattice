package sstable

import (
	"math"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

const (
	// DefaultRestartInterval is the default number of consecutive records grouped
	// under a single prefix-compression run before emitting a restart point.
	// Every 16th record (indexes 0, 16, 32, ...) begins with SharedKeyLen = 0.
	DefaultRestartInterval = 16

	// TargetBlockSize is the default target capacity in bytes for uncompressed SSTable data blocks.
	TargetBlockSize = 4096

	// RestartOffsetSize is the serialized byte length of a single 32-bit restart point offset.
	RestartOffsetSize = 4

	// RestartCountSize is the serialized byte length of the 32-bit restart point count field.
	RestartCountSize = 4

	// BlockTrailerSize is the serialized byte length of the CRC32-IEEE checksum trailer.
	BlockTrailerSize = 4
)

// BlockBuilder constructs prefix-compressed SSTable data blocks from consecutive sorted records.
//
// Layout of a Canonical SSTable Data Block (P04-S01-M02):
//
//	+-------------------------------+
//	| Entry Data Region             |
//	|   Record 0 (Restart Point 0)  |
//	|   Record 1                    |
//	|   ...                         |
//	|   Record N-1                  |
//	+-------------------------------+
//	| Restart Offset 0   (uint32)   |
//	| Restart Offset 1   (uint32)   |
//	| ...                           |
//	| Restart Offset M-1 (uint32)   |
//	+-------------------------------+
//	| Restart Count      (uint32)   |
//	+-------------------------------+
//	| CRC32-IEEE         (uint32)   |
//	+-------------------------------+
//
// Each record entry in the Entry Data Region is framed as:
//   - SharedKeyLen   (7-bit varint, 1-10 bytes): number of bytes shared with previous encoded key
//   - UnsharedKeyLen (7-bit varint, 1-10 bytes): number of unshared key suffix bytes in this record
//   - ValueLength    (7-bit varint, 1-10 bytes): byte length of the value payload
//   - KeyDeltaBytes  (unshared key bytes): suffix of the encoded InternalKey
//   - ValueBytes     (raw value bytes)
//
// At restart points (record index 0, 16, 32, ...):
//   - SharedKeyLen = 0
//   - UnsharedKeyLen = len(encoded InternalKey)
//   - KeyDeltaBytes contains the complete encoded InternalKey
//
// The trailer appended by Finish() consists of:
//   - Restart Offsets: uint32 Big-Endian offsets into the Entry Data Region where each restart entry begins.
//     Invariant: restartOffsets[0] == 0 for any non-empty block, and offsets are strictly increasing.
//   - Restart Count: uint32 Big-Endian count equal to len(restartOffsets).
//   - CRC32-IEEE: 4-byte Big-Endian checksum computed over [Entry Data || Restart Offsets || Restart Count].
//     The CRC field itself is strictly excluded from its own checksum calculation.
//
// Invariants:
//   - Monotonic Ordering: Entries must be added in strictly increasing canonical order
//     (UserKey ASC, SeqNum DESC, OpType DESC).
//   - Failure Atomicity: If an Add operation fails (e.g. unsorted key or oversized payload),
//     the builder's state is completely unmodified.
//   - Caller Isolation: Input slices are never retained across Add calls; Finish returns an
//     owned defensive copy.
//   - Determinism: Identical records added in identical sequence produce byte-for-byte identical output.
//   - Repeated Finish Idempotence: Calling Finish() multiple times returns identical byte slices.
//
// Concurrency:
// BlockBuilder is single-threaded and not safe for concurrent use by multiple goroutines.
type BlockBuilder struct {
	restartInterval     int
	entriesSinceRestart int
	entryCount          int
	restartOffsets      []uint32
	buf                 []byte
	prevKey             []byte
	prevKeyValid        bool
	prevInternalKey     binary.InternalKey
	scratchKey          []byte
	finished            bool
	finishedBuf         []byte
}

// NewBlockBuilder creates a BlockBuilder configured with DefaultRestartInterval (16).
func NewBlockBuilder() *BlockBuilder {
	b, _ := NewBlockBuilderWithInterval(DefaultRestartInterval)
	return b
}

// NewBlockBuilderWithInterval creates a BlockBuilder with a custom restart interval.
// Rejects restartInterval <= 0 with errors.ErrInvalidRestartInterval.
func NewBlockBuilderWithInterval(restartInterval int) (*BlockBuilder, error) {
	if restartInterval <= 0 {
		return nil, errors.ErrInvalidRestartInterval
	}
	return &BlockBuilder{
		restartInterval: restartInterval,
		restartOffsets:  make([]uint32, 0, 16),
		buf:             make([]byte, 0, TargetBlockSize),
		scratchKey:      make([]byte, 0, 64),
		prevKey:         make([]byte, 0, 64),
	}, nil
}

// Add appends a key-value record to the data block in prefix-compressed format.
//
// Contract:
//   - Input keys must sort strictly after the preceding key under binary.CompareInternalKey.
//   - If key sorts before or equal to previous key, returns *errors.KeyOutOfOrderError.
//   - Key and Value must satisfy boundary limits (Key: 1..65,535 bytes; Value: 0..4,194,304 bytes).
//   - If the builder has already been finished, returns errors.ErrBlockFinished.
//   - Guarantees failure atomicity: failed Add calls leave builder state completely untouched.
func (b *BlockBuilder) Add(key binary.InternalKey, value []byte) error {
	if b == nil {
		return errors.ErrNilReceiver
	}
	if b.finished {
		return errors.ErrBlockFinished
	}
	if err := binary.ValidateKey(key.UserKey); err != nil {
		return err
	}
	if err := key.OpType.Validate(); err != nil {
		return err
	}
	if err := binary.ValidateValue(value); err != nil {
		return err
	}

	if b.prevKeyValid {
		if binary.CompareInternalKey(b.prevInternalKey, key) >= 0 {
			return &errors.KeyOutOfOrderError{
				PrevKeyLen: len(b.prevInternalKey.UserKey),
				CurrKeyLen: len(key.UserKey),
			}
		}
	}

	b.scratchKey = binary.AppendInternalKey(b.scratchKey[:0], key)
	return b.addEncodedKey(b.scratchKey, key, value)
}

// AddRaw appends an already-encoded InternalKey byte slice and value to the data block.
// The encodedKey is validated using binary.DecodeInternalKey to ensure structural integrity
// and strict canonical ordering.
func (b *BlockBuilder) AddRaw(encodedKey []byte, value []byte) error {
	if b == nil {
		return errors.ErrNilReceiver
	}
	if b.finished {
		return errors.ErrBlockFinished
	}

	key, err := binary.DecodeInternalKey(encodedKey)
	if err != nil {
		return err
	}
	if err := binary.ValidateValue(value); err != nil {
		return err
	}

	if b.prevKeyValid {
		if binary.CompareInternalKey(b.prevInternalKey, key) >= 0 {
			return &errors.KeyOutOfOrderError{
				PrevKeyLen: len(b.prevInternalKey.UserKey),
				CurrKeyLen: len(key.UserKey),
			}
		}
	}

	return b.addEncodedKey(encodedKey, key, value)
}

// addEncodedKey executes prefix compression calculation and appends entry bytes to the buffer.
func (b *BlockBuilder) addEncodedKey(currKeyBytes []byte, key binary.InternalKey, value []byte) error {
	var shared int
	isRestart := (b.entriesSinceRestart == 0)

	if isRestart {
		shared = 0
	} else {
		shared = commonPrefix(b.prevKey, currKeyBytes)
	}

	unshared := len(currKeyBytes) - shared
	valueLen := len(value)

	var varintBuf [binary.MaxVarintLen64 * 3]byte
	n1 := binary.PutVarint64(varintBuf[0:], uint64(shared))
	n2 := binary.PutVarint64(varintBuf[n1:], uint64(unshared))
	n3 := binary.PutVarint64(varintBuf[n1+n2:], uint64(valueLen))
	headerLen := n1 + n2 + n3

	entryTotalBytes := headerLen + unshared + valueLen

	// Check for 32-bit addressable capacity overflow for entry data + metadata trailer
	additionalRestarts := 0
	if isRestart {
		additionalRestarts = 1
	}
	projectedRestarts := len(b.restartOffsets) + additionalRestarts
	projectedTrailerBytes := uint64(projectedRestarts)*uint64(RestartOffsetSize) + uint64(RestartCountSize) + uint64(BlockTrailerSize)
	if uint64(len(b.buf))+uint64(entryTotalBytes)+projectedTrailerBytes > math.MaxUint32 {
		return errors.ErrBlockOverflow
	}

	currentOffset := uint32(len(b.buf))

	// Commit entry bytes to buffer
	b.buf = append(b.buf, varintBuf[:headerLen]...)
	b.buf = append(b.buf, currKeyBytes[shared:]...)
	b.buf = append(b.buf, value...)

	// Update restart metadata
	if isRestart {
		b.restartOffsets = append(b.restartOffsets, currentOffset)
	}

	// Update builder state
	b.prevKey = append(b.prevKey[:0], currKeyBytes...)
	b.prevInternalKey = key.Clone()
	b.prevKeyValid = true
	b.entryCount++
	b.entriesSinceRestart++
	if b.entriesSinceRestart >= b.restartInterval {
		b.entriesSinceRestart = 0
	}

	return nil
}

// Finish seals the BlockBuilder and returns an owned defensive copy of the fully serialized
// canonical SSTable data block, including entry data, restart offset array, restart count,
// and CRC32-IEEE checksum trailer.
//
// Subsequent Add operations are rejected with errors.ErrBlockFinished.
// Repeated Finish calls are idempotent and return identical bytes.
// If the builder is empty (zero entries added), Finish returns an empty slice ([]byte{}).
func (b *BlockBuilder) Finish() []byte {
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
	if len(b.buf) == 0 {
		b.finishedBuf = nil
		return []byte{}
	}

	// Serialize trailer into b.buf:
	// 1. Restart offsets (uint32 each, Big-Endian)
	var uint32Buf [4]byte
	for _, offset := range b.restartOffsets {
		binary.PutUint32(uint32Buf[:], offset)
		b.buf = append(b.buf, uint32Buf[:]...)
	}

	// 2. Restart count (uint32, Big-Endian)
	binary.PutUint32(uint32Buf[:], uint32(len(b.restartOffsets)))
	b.buf = append(b.buf, uint32Buf[:]...)

	// 3. CRC32-IEEE checksum computed over [entry data || restart offsets || restart count]
	checksum := binary.Checksum(b.buf)
	binary.PutUint32(uint32Buf[:], checksum)
	b.buf = append(b.buf, uint32Buf[:]...)

	// Cache finished bytes to ensure repeated Finish() calls are idempotent
	b.finishedBuf = b.buf

	// Return owned defensive copy to isolate caller mutations from internal state
	out := make([]byte, len(b.finishedBuf))
	copy(out, b.finishedBuf)
	return out
}

// Reset resets the BlockBuilder to its initial empty state, retaining allocated slice
// capacities to eliminate garbage-collection overhead across successive blocks.
func (b *BlockBuilder) Reset() {
	if b == nil {
		return
	}
	b.entriesSinceRestart = 0
	b.entryCount = 0
	b.restartOffsets = b.restartOffsets[:0]
	b.buf = b.buf[:0]
	b.prevKey = b.prevKey[:0]
	b.prevKeyValid = false
	b.prevInternalKey = binary.InternalKey{}
	b.finished = false
	b.finishedBuf = nil
}

// RestartOffsets returns a defensive copy of the byte offsets where each restart entry begins.
func (b *BlockBuilder) RestartOffsets() []uint32 {
	if b == nil || len(b.restartOffsets) == 0 {
		return []uint32{}
	}
	out := make([]uint32, len(b.restartOffsets))
	copy(out, b.restartOffsets)
	return out
}

// RestartCount returns the number of restart points emitted into the block.
func (b *BlockBuilder) RestartCount() int {
	if b == nil {
		return 0
	}
	return len(b.restartOffsets)
}

// EntryCount returns the total number of records successfully added to the block.
func (b *BlockBuilder) EntryCount() int {
	if b == nil {
		return 0
	}
	return b.entryCount
}

// RestartInterval returns the configured restart interval for this builder.
func (b *BlockBuilder) RestartInterval() int {
	if b == nil {
		return 0
	}
	return b.restartInterval
}

// DataSize returns the exact number of entry-data bytes currently in the buffer
// (prior to trailer serialization).
func (b *BlockBuilder) DataSize() int {
	if b == nil {
		return 0
	}
	// If finished, calculate the entry data size by subtracting trailer length
	if b.finished {
		if len(b.finishedBuf) == 0 {
			return 0
		}
		trailerLen := len(b.restartOffsets)*RestartOffsetSize + RestartCountSize + BlockTrailerSize
		if len(b.finishedBuf) >= trailerLen {
			return len(b.finishedBuf) - trailerLen
		}
		return len(b.finishedBuf)
	}
	return len(b.buf)
}

// CurrentSizeEstimate returns the estimated total size of the finished block in bytes,
// including entry data, restart offset array, restart count, and CRC32 checksum trailer.
// Calculation: DataSize + (len(restartOffsets) * 4) + 4 (count) + 4 (CRC32).
func (b *BlockBuilder) CurrentSizeEstimate() int {
	if b == nil {
		return 0
	}
	if b.finished {
		return len(b.finishedBuf)
	}
	if len(b.buf) == 0 {
		return 0
	}
	return len(b.buf) + (len(b.restartOffsets) * RestartOffsetSize) + RestartCountSize + BlockTrailerSize
}

// IsEmpty reports whether the builder contains zero entries.
func (b *BlockBuilder) IsEmpty() bool {
	if b == nil {
		return true
	}
	return b.entryCount == 0
}

// Finished reports whether the builder has transitioned to the sealed/finished state.
func (b *BlockBuilder) Finished() bool {
	if b == nil {
		return false
	}
	return b.finished
}

// commonPrefix returns the length in bytes of the longest common prefix of a and b.
// It is strictly binary-safe and executes without heap allocations.
func commonPrefix(a, b []byte) int {
	minLen := len(a)
	if len(b) < minLen {
		minLen = len(b)
	}
	i := 0
	for i < minLen && a[i] == b[i] {
		i++
	}
	return i
}
