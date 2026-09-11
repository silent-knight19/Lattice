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
)

// BlockBuilder constructs prefix-compressed SSTable data blocks from consecutive sorted records.
//
// Layout within a Data Block (P04-S01-M01):
// Each record entry is framed as:
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
// Invariants:
//   - Monotonic Ordering: Entries must be added in strictly increasing canonical order
//     (UserKey ASC, SeqNum DESC, OpType DESC).
//   - Failure Atomicity: If an Add operation fails (e.g. unsorted key or oversized payload),
//     the builder's state is completely unmodified.
//   - Caller Isolation: Input slices are never retained across Add calls; Finish returns an
//     owned defensive copy.
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
	if uint64(len(b.buf))+uint64(entryTotalBytes) > math.MaxUint32 {
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

// Finish seals the BlockBuilder and returns an owned defensive copy of the constructed
// entry-data bytes. Subsequent Add operations are rejected with errors.ErrBlockFinished.
// Repeated Finish calls are idempotent and return identical bytes.
func (b *BlockBuilder) Finish() []byte {
	if b == nil {
		return nil
	}
	b.finished = true
	if len(b.buf) == 0 {
		return []byte{}
	}
	out := make([]byte, len(b.buf))
	copy(out, b.buf)
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

// DataSize returns the exact number of entry-data bytes currently in the buffer.
func (b *BlockBuilder) DataSize() int {
	if b == nil {
		return 0
	}
	return len(b.buf)
}

// CurrentSizeEstimate returns the estimated total size of the block in bytes,
// including entry data and the future restart array trailer (offsets + count).
// Calculation: DataSize + (len(restartOffsets) * 4) + 4 bytes.
func (b *BlockBuilder) CurrentSizeEstimate() int {
	if b == nil {
		return 0
	}
	return len(b.buf) + (len(b.restartOffsets) * 4) + 4
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
