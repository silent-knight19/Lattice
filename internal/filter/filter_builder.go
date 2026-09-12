package filter

import (
	"github.com/silent-knight19/lattice/internal/errors"
)

// FilterBlockBuilder constructs and serializes a persistent SSTable filter block.
//
// In Lattice's SSTable architecture (ADR-004), a filter block contains a serialized Murmur3
// Bloom filter, recording bitset probes for all keys written into the table's data blocks.
//
// Lifecycle:
//
//	Created -> Populating (AddKey) -> Finished (Finish) -> Reset
//
// Invariants:
//   - Monotonic State: Once Finish() is called, subsequent AddKey calls are rejected
//     with errors.ErrFilterFinished.
//   - Idempotent Finish: Calling Finish() multiple times returns identical byte slices.
//   - Defensive Copy: Finish() returns an owned byte slice; mutations by the caller
//     do not affect internal builder or filter state.
//   - Reusability: Reset() clears all set bits and restores the builder to its initial
//     populating state while retaining allocated bitset capacity.
//
// Concurrency:
// FilterBlockBuilder is single-threaded and not safe for concurrent use by multiple goroutines.
type FilterBlockBuilder struct {
	filter      *BloomFilter
	finished    bool
	finishedBuf []byte
	addedKeys   int
}

// NewFilterBlockBuilder initializes a FilterBlockBuilder sized for expectedKeys keys.
// If expectedKeys < 0 or expectedKeys > MaxKeyCount, returns nil.
func NewFilterBlockBuilder(expectedKeys int) *FilterBlockBuilder {
	bf := NewBloomFilter(expectedKeys)
	if bf == nil {
		return nil
	}
	return &FilterBlockBuilder{
		filter: bf,
	}
}

// NewFilterBlockBuilderWithFilter creates a FilterBlockBuilder wrapping an existing BloomFilter.
// If f is nil, returns nil.
func NewFilterBlockBuilderWithFilter(f *BloomFilter) *FilterBlockBuilder {
	if f == nil {
		return nil
	}
	return &FilterBlockBuilder{
		filter: f,
	}
}

// AddKey inserts key into the underlying Bloom filter.
//
// Contract:
//   - If the builder is already finished, returns errors.ErrFilterFinished.
//   - If the receiver is nil, returns errors.ErrNilReceiver.
//   - Safe with nil or empty key slices ([]byte{}).
func (b *FilterBlockBuilder) AddKey(key []byte) error {
	if b == nil {
		return errors.ErrNilReceiver
	}
	if b.finished {
		return errors.ErrFilterFinished
	}
	b.filter.Add(key)
	b.addedKeys++
	return nil
}

// Finish seals the FilterBlockBuilder and returns an owned defensive copy of the fully
// serialized SSTable filter block.
//
// Subsequent AddKey operations are rejected with errors.ErrFilterFinished.
// Repeated Finish calls are idempotent and return identical byte slices.
func (b *FilterBlockBuilder) Finish() []byte {
	if b == nil {
		return nil
	}
	if b.finished {
		out := make([]byte, len(b.finishedBuf))
		copy(out, b.finishedBuf)
		return out
	}

	b.finished = true
	b.finishedBuf = b.filter.Encode()

	out := make([]byte, len(b.finishedBuf))
	copy(out, b.finishedBuf)
	return out
}

// Reset restores the FilterBlockBuilder to its initial populating state, zeroing the bitset
// and clearing added key counters while retaining allocated slice capacities.
func (b *FilterBlockBuilder) Reset() {
	if b == nil {
		return
	}
	b.finished = false
	b.finishedBuf = nil
	b.addedKeys = 0
	if b.filter != nil && len(b.filter.bitset) > 0 {
		for i := range b.filter.bitset {
			b.filter.bitset[i] = 0
		}
	}
}

// Filter returns the underlying BloomFilter instance.
func (b *FilterBlockBuilder) Filter() *BloomFilter {
	if b == nil {
		return nil
	}
	return b.filter
}

// AddedKeys returns the number of keys added via AddKey since construction or the last Reset.
func (b *FilterBlockBuilder) AddedKeys() int {
	if b == nil {
		return 0
	}
	return b.addedKeys
}

// Finished reports whether the builder has transitioned to the sealed/finished state.
func (b *FilterBlockBuilder) Finished() bool {
	if b == nil {
		return false
	}
	return b.finished
}

// IsEmpty reports whether the builder contains zero added keys.
func (b *FilterBlockBuilder) IsEmpty() bool {
	if b == nil || b.filter == nil {
		return true
	}
	return b.addedKeys == 0
}

// CurrentSizeEstimate returns the physical byte size of the finished filter block.
// For a filter with N bitset bytes, this is N + FilterBlockTrailerSize (N + 13).
func (b *FilterBlockBuilder) CurrentSizeEstimate() int {
	if b == nil || b.filter == nil {
		return FilterBlockTrailerSize
	}
	return b.filter.ByteSize() + FilterBlockTrailerSize
}
