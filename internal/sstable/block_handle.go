package sstable

import (
	"math"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// BlockHandleSize is the exact serialized byte length of a BlockHandle.
// 8 bytes (Offset, Big-Endian uint64) + 8 bytes (Size, Big-Endian uint64).
const BlockHandleSize = 16

// BlockHandle represents a pointer to a physical block within an SSTable file on disk.
// It specifies the byte offset where the block starts and its total length in bytes.
//
// Binary Serialization (16 Bytes Fixed):
//
//	+-----------------------------------+-----------------------------------+
//	| Offset (8B, Big-Endian uint64)    | Size (8B, Big-Endian uint64)      |
//	+-----------------------------------+-----------------------------------+
type BlockHandle struct {
	Offset uint64
	Size   uint64
}

// Encode serializes the BlockHandle into a fixed 16-byte array in strict Big-Endian byte order.
func (h BlockHandle) Encode() [BlockHandleSize]byte {
	var buf [BlockHandleSize]byte
	binary.PutUint64(buf[0:8], h.Offset)
	binary.PutUint64(buf[8:16], h.Size)
	return buf
}

// AppendTo serializes the BlockHandle into dst using 16 Big-Endian bytes and returns the extended slice.
func (h BlockHandle) AppendTo(dst []byte) []byte {
	var buf [BlockHandleSize]byte
	binary.PutUint64(buf[0:8], h.Offset)
	binary.PutUint64(buf[8:16], h.Size)
	return append(dst, buf[:]...)
}

// DecodeBlockHandle decodes a BlockHandle from the prefix of src in strict Big-Endian order.
//
// Validation Contract:
//   - Requires len(src) >= BlockHandleSize (16 bytes). Returns errors.ErrBlockHandleTruncated if smaller.
//   - Requires Size > 0. A block cannot have zero bytes. Returns errors.ErrInvalidBlockHandle if Size == 0.
//   - Requires Offset + Size to not overflow uint64. Returns errors.ErrInvalidBlockHandle on overflow.
func DecodeBlockHandle(src []byte) (BlockHandle, error) {
	if len(src) < BlockHandleSize {
		return BlockHandle{}, errors.ErrBlockHandleTruncated
	}
	offset := binary.GetUint64(src[0:8])
	size := binary.GetUint64(src[8:16])

	handle := BlockHandle{
		Offset: offset,
		Size:   size,
	}
	if err := handle.Validate(); err != nil {
		return BlockHandle{}, err
	}
	return handle, nil
}

// Validate checks internal consistency and integrity of the BlockHandle.
// It ensures:
//  1. Size > 0 (blocks cannot be empty).
//  2. Offset + Size does not overflow uint64 (integer wrap protection).
func (h BlockHandle) Validate() error {
	if h.Size == 0 {
		return &errors.InvalidBlockHandleError{
			Offset: h.Offset,
			Size:   h.Size,
			Reason: "block size must be greater than zero",
		}
	}
	if h.Offset > math.MaxUint64-h.Size {
		return &errors.InvalidBlockHandleError{
			Offset: h.Offset,
			Size:   h.Size,
			Reason: "block handle offset + size overflows 64-bit address space",
		}
	}
	return nil
}

// ValidateAgainstFileSize checks whether the block referenced by h falls completely within
// the physical bounds of an SSTable file of length fileSize.
//
// Defends against corrupted block offset attacks (ADR-004 Section 7).
func (h BlockHandle) ValidateAgainstFileSize(fileSize int64) error {
	if fileSize < 0 {
		return &errors.InvalidBlockHandleError{
			Offset: h.Offset,
			Size:   h.Size,
			Reason: "negative physical file size",
		}
	}
	if err := h.Validate(); err != nil {
		return err
	}
	if h.Offset+h.Size > uint64(fileSize) {
		return &errors.InvalidBlockHandleError{
			Offset: h.Offset,
			Size:   h.Size,
			Reason: "block handle exceeds physical file boundary",
		}
	}
	return nil
}
