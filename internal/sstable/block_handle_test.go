package sstable_test

import (
	"bytes"
	stdErrors "errors"
	"math"
	"testing"

	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
)

func TestBlockHandle_EncodeDecode_RoundTrip(t *testing.T) {
	testCases := []struct {
		name   string
		handle sstable.BlockHandle
	}{
		{
			name:   "standard 4KB block at offset 0",
			handle: sstable.BlockHandle{Offset: 0, Size: 4096},
		},
		{
			name:   "block at 1MB offset",
			handle: sstable.BlockHandle{Offset: 1048576, Size: 8192},
		},
		{
			name:   "size 1 minimum valid payload",
			handle: sstable.BlockHandle{Offset: 42, Size: 1},
		},
		{
			name:   "large 64-bit offset and size",
			handle: sstable.BlockHandle{Offset: 0x123456789ABCDEF0, Size: 0x000000000000FFFF},
		},
		{
			name:   "max valid address boundary",
			handle: sstable.BlockHandle{Offset: math.MaxUint64 - 100, Size: 100},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			encoded := tc.handle.Encode()
			if len(encoded) != sstable.BlockHandleSize {
				t.Fatalf("expected encoded length %d, got %d", sstable.BlockHandleSize, len(encoded))
			}

			decoded, err := sstable.DecodeBlockHandle(encoded[:])
			if err != nil {
				t.Fatalf("unexpected decode error: %v", err)
			}

			if decoded != tc.handle {
				t.Fatalf("handle mismatch: got %+v, want %+v", decoded, tc.handle)
			}
		})
	}
}

func TestBlockHandle_ExactByteLayout(t *testing.T) {
	handle := sstable.BlockHandle{
		Offset: 0x0102030405060708,
		Size:   0x1112131415161718,
	}

	expected := []byte{
		// Offset (8B, Big-Endian)
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		// Size (8B, Big-Endian)
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
	}

	encoded := handle.Encode()
	if !bytes.Equal(encoded[:], expected) {
		t.Fatalf("byte layout mismatch:\ngot:  %x\nwant: %x", encoded[:], expected)
	}

	// Also verify AppendTo
	dst := []byte{0xAA, 0xBB}
	appended := handle.AppendTo(dst)
	if len(appended) != len(dst)+sstable.BlockHandleSize {
		t.Fatalf("unexpected appended length: %d", len(appended))
	}
	if !bytes.Equal(appended[:2], dst) {
		t.Fatalf("original buffer prefix corrupted: %x", appended[:2])
	}
	if !bytes.Equal(appended[2:], expected) {
		t.Fatalf("appended payload mismatch:\ngot:  %x\nwant: %x", appended[2:], expected)
	}
}

func TestBlockHandle_Decode_Truncated(t *testing.T) {
	fullBuf := make([]byte, sstable.BlockHandleSize)
	for length := 0; length < sstable.BlockHandleSize; length++ {
		subBuf := fullBuf[:length]
		_, err := sstable.DecodeBlockHandle(subBuf)
		if err == nil {
			t.Fatalf("expected error for truncated buffer of len %d, got nil", length)
		}
		if !stdErrors.Is(err, errors.ErrBlockHandleTruncated) {
			t.Fatalf("expected ErrBlockHandleTruncated for len %d, got %v", length, err)
		}
	}
}

func TestBlockHandle_Validation(t *testing.T) {
	t.Run("zero size rejected", func(t *testing.T) {
		h := sstable.BlockHandle{Offset: 100, Size: 0}
		err := h.Validate()
		if err == nil {
			t.Fatal("expected error for size=0, got nil")
		}
		if !stdErrors.Is(err, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected ErrInvalidBlockHandle, got %v", err)
		}

		// Also verify DecodeBlockHandle rejects it
		encoded := h.Encode()
		_, decodeErr := sstable.DecodeBlockHandle(encoded[:])
		if decodeErr == nil || !stdErrors.Is(decodeErr, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected ErrInvalidBlockHandle from decode, got %v", decodeErr)
		}
	})

	t.Run("offset + size overflow rejected", func(t *testing.T) {
		h := sstable.BlockHandle{Offset: math.MaxUint64 - 5, Size: 10}
		err := h.Validate()
		if err == nil {
			t.Fatal("expected error for overflow, got nil")
		}
		if !stdErrors.Is(err, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected ErrInvalidBlockHandle, got %v", err)
		}

		encoded := h.Encode()
		_, decodeErr := sstable.DecodeBlockHandle(encoded[:])
		if decodeErr == nil || !stdErrors.Is(decodeErr, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected ErrInvalidBlockHandle from decode, got %v", decodeErr)
		}
	})

	t.Run("max valid uint64 boundary accepted", func(t *testing.T) {
		h := sstable.BlockHandle{Offset: math.MaxUint64 - 10, Size: 10}
		if err := h.Validate(); err != nil {
			t.Fatalf("expected valid handle at uint64 boundary, got %v", err)
		}
	})
}

func TestBlockHandle_ValidateAgainstFileSize(t *testing.T) {
	handle := sstable.BlockHandle{Offset: 1000, Size: 500}

	t.Run("negative file size rejected", func(t *testing.T) {
		err := handle.ValidateAgainstFileSize(-1)
		if err == nil || !stdErrors.Is(err, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected ErrInvalidBlockHandle for negative file size, got %v", err)
		}
	})

	t.Run("exceeds physical file size rejected", func(t *testing.T) {
		// handle spans [1000, 1500), file size is 1499
		err := handle.ValidateAgainstFileSize(1499)
		if err == nil || !stdErrors.Is(err, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected ErrInvalidBlockHandle when exceeding file size, got %v", err)
		}
	})

	t.Run("exact physical boundary accepted", func(t *testing.T) {
		// handle spans [1000, 1500), file size is 1500
		if err := handle.ValidateAgainstFileSize(1500); err != nil {
			t.Fatalf("expected valid handle at exact file boundary, got %v", err)
		}
	})

	t.Run("well within file size accepted", func(t *testing.T) {
		if err := handle.ValidateAgainstFileSize(10000); err != nil {
			t.Fatalf("expected valid handle within file bounds, got %v", err)
		}
	})
}
