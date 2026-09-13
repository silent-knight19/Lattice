package sstable_test

import (
	"math"
	"testing"

	"github.com/silent-knight19/lattice/internal/sstable"
)

// TestIND003_BlockHandle_IntegerOverflowUnderflow verifies that BlockHandle decoding
// and validation reject handles whose offset + size would cause signed or unsigned
// integer overflow or negative indices.
func TestIND003_BlockHandle_IntegerOverflowUnderflow(t *testing.T) {
	t.Run("ValidationRejections", func(t *testing.T) {
		invalidHandles := []struct {
			name   string
			offset uint64
			size   uint64
		}{
			{
				name:   "AuditScenario_3f800000_Uint64Overflow",
				offset: 0x3F800000,
				size:   math.MaxUint64 - 0x3F800000 + 1, // causes uint64 overflow
			},
			{
				name:   "OffsetPlusSizeExceedsMaxUint64",
				offset: math.MaxUint64 - 10,
				size:   20,
			},
			{
				name:   "ZeroSize",
				offset: 100,
				size:   0,
			},
		}

		for _, tc := range invalidHandles {
			t.Run(tc.name, func(t *testing.T) {
				h := sstable.BlockHandle{
					Offset: tc.offset,
					Size:   tc.size,
				}
				if err := h.Validate(); err == nil {
					t.Fatalf("expected handle validation to fail for %+v", h)
				}

				encoded := h.Encode()
				if _, decErr := sstable.DecodeBlockHandle(encoded[:]); decErr == nil {
					t.Fatalf("expected DecodeBlockHandle to reject invalid handle: %+v", h)
				}
			})
		}
	})

	t.Run("FileSizeBoundaryProtection", func(t *testing.T) {
		boundaryCases := []struct {
			name     string
			offset   uint64
			size     uint64
			fileSize int64
		}{
			{
				name:     "AuditScenario_3f800000_With1024FileSize",
				offset:   0x3F800000,
				size:     0x80,
				fileSize: 1024,
			},
			{
				name:     "Signed32BitNearMaxInt32Overflow",
				offset:   0x7FFFFFFF,
				size:     0x80,
				fileSize: 1024,
			},
			{
				name:     "OffsetExceedsMaxInt64",
				offset:   math.MaxInt64 + 1,
				size:     100,
				fileSize: 1024,
			},
			{
				name:     "OffsetPlusSizeExceedsMaxInt64",
				offset:   math.MaxInt64 - 50,
				size:     100,
				fileSize: 1024,
			},
		}

		for _, tc := range boundaryCases {
			t.Run(tc.name, func(t *testing.T) {
				h := sstable.BlockHandle{
					Offset: tc.offset,
					Size:   tc.size,
				}
				if err := h.ValidateAgainstFileSize(tc.fileSize); err == nil {
					t.Fatalf("expected ValidateAgainstFileSize to reject block handle exceeding file size: %+v", h)
				}
			})
		}
	})
}
