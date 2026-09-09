package memtable

import (
	"math"
	"sync/atomic"
	"unsafe"
)

// Layout constants derived directly from struct definitions via unsafe.Sizeof.
// These constants are platform-adaptive across 64-bit and 32-bit architectures.
var (
	// NodeStructSize is the heap byte size of a skipListNode struct header.
	// On 64-bit platforms:
	//   key binary.InternalKey (40 bytes: UserKey []byte 24B + SeqNum 8B + OpType 1B + padding 7B)
	//   value atomic.Pointer[nodeValue] (8 bytes)
	//   forward []atomic.Pointer[skipListNode] (24 bytes slice header)
	// Total = 72 bytes.
	NodeStructSize = uint64(unsafe.Sizeof(skipListNode{}))

	// NodeValueStructSize is the heap byte size of a nodeValue struct container.
	// On 64-bit platforms:
	//   data []byte (24 bytes slice header)
	// Total = 24 bytes.
	NodeValueStructSize = uint64(unsafe.Sizeof(nodeValue{}))

	// PointerSize is the byte size of a single forward pointer element
	// (atomic.Pointer[skipListNode]) in the variable-height tower backing array.
	// On 64-bit platforms: 8 bytes.
	PointerSize = uint64(unsafe.Sizeof(atomic.Pointer[skipListNode]{}))
)

// nodeMemoryBytes computes the exact heap bytes directly owned by a newly allocated skipListNode,
// including:
//  1. Node struct header (NodeStructSize)
//  2. Key backing array (len(UserKey))
//  3. Forward pointer tower backing array (height * PointerSize)
//  4. Value container and backing array (if value is non-empty)
//
// Excluded categories:
//   - Go runtime allocator size-class slack / fragmentation
//   - GC metadata and write barrier structures
//   - Goroutine stacks and synchronization primitives
//   - Head sentinel node infrastructure (governed under Model A: user-record tracking)
func nodeMemoryBytes(keyLen, valueLen, height int) uint64 {
	// Guard against negative inputs
	if keyLen < 0 {
		keyLen = 0
	}
	if valueLen < 0 {
		valueLen = 0
	}
	if height < 0 {
		height = 0
	}

	bytes := NodeStructSize + uint64(keyLen) + uint64(height)*PointerSize
	if valueLen > 0 {
		bytes += NodeValueStructSize + uint64(valueLen)
	}
	return bytes
}

// valueMemoryBytes computes the heap bytes owned by a value container and its backing array.
// Returns 0 if valueLen <= 0 (nil nodeValue container for deletions or zero-length values).
func valueMemoryBytes(valueLen int) uint64 {
	if valueLen <= 0 {
		return 0
	}
	return NodeValueStructSize + uint64(valueLen)
}

// safeAddUint64 adds delta to counter atomically using a CAS loop.
// If adding delta would exceed math.MaxUint64, the counter saturates at math.MaxUint64
// to strictly prevent unchecked integer wraparound (P03-S02-M02-SEC-INV-01).
func safeAddUint64(counter *atomic.Uint64, delta uint64) {
	for {
		curr := counter.Load()
		if math.MaxUint64-curr < delta {
			if counter.CompareAndSwap(curr, math.MaxUint64) {
				return
			}
			continue
		}
		if counter.CompareAndSwap(curr, curr+delta) {
			return
		}
	}
}

// safeSubUint64 subtracts delta from counter atomically using a CAS loop.
// If subtracting delta would underflow below 0, the counter saturates at 0
// to strictly prevent unchecked unsigned underflow (P03-S02-M02-SEC-INV-02).
func safeSubUint64(counter *atomic.Uint64, delta uint64) {
	for {
		curr := counter.Load()
		if curr < delta {
			if counter.CompareAndSwap(curr, 0) {
				return
			}
			continue
		}
		if counter.CompareAndSwap(curr, curr-delta) {
			return
		}
	}
}
