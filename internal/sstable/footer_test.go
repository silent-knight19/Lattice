package sstable_test

import (
	"bytes"
	stdErrors "errors"
	"math"
	"testing"

	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// TestFooter_ExactSize asserts that FooterSize is exactly 48 bytes and that every
// serialized footer is strictly 48 bytes.
func TestFooter_ExactSize(t *testing.T) {
	if sstable.FooterSize != 48 {
		t.Fatalf("expected FooterSize to be 48, got %d", sstable.FooterSize)
	}

	footer := sstable.Footer{
		MetaIndexHandle: sstable.BlockHandle{Offset: 100, Size: 200},
		IndexHandle:     sstable.BlockHandle{Offset: 300, Size: 400},
	}
	encoded := footer.Encode()
	if len(encoded) != 48 {
		t.Fatalf("encoded footer length must be exactly 48 bytes, got %d", len(encoded))
	}
	if encoded == [sstable.FooterSize]byte{} {
		t.Fatalf("encoded footer must not be zero")
	}
}

// TestFooter_ExactBinaryLayout_IndependentOracle uses an independently hand-constructed
// byte fixture (without using Footer.Encode) to verify byte-for-byte fidelity of both
// the encoder and the decoder.
func TestFooter_ExactBinaryLayout_IndependentOracle(t *testing.T) {
	meta := sstable.BlockHandle{
		Offset: 0x0102030405060708,
		Size:   0x1112131415161718,
	}
	index := sstable.BlockHandle{
		Offset: 0x2122232425262728,
		Size:   0x3132333435363738,
	}
	footer := sstable.Footer{
		MetaIndexHandle: meta,
		IndexHandle:     index,
	}

	// Hand-calculated exact 48-byte oracle:
	expectedBytes := []byte{
		// 00..07: MetaIndex Offset (0x0102030405060708)
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		// 08..15: MetaIndex Size   (0x1112131415161718)
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
		// 16..23: Index Offset     (0x2122232425262728)
		0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28,
		// 24..31: Index Size       (0x3132333435363738)
		0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, 0x38,
		// 32..39: Padding          (8 zero bytes)
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		// 40..47: Magic Number     (0x4C41545453535401 -> "LATT_SST_1")
		0x4C, 0x41, 0x54, 0x54, 0x53, 0x53, 0x54, 0x01,
	}

	// Verify Encode produces exact oracle bytes
	encoded := footer.Encode()
	if !bytes.Equal(encoded[:], expectedBytes) {
		t.Fatalf("encoded footer does not match independent oracle:\ngot:  %x\nwant: %x", encoded[:], expectedBytes)
	}

	// Verify AppendTo
	dst := []byte{0xDE, 0xAD}
	appended := footer.AppendTo(dst)
	if len(appended) != len(dst)+sstable.FooterSize {
		t.Fatalf("unexpected appended length: %d", len(appended))
	}
	if !bytes.Equal(appended[:2], dst) {
		t.Fatalf("dst prefix corrupted: %x", appended[:2])
	}
	if !bytes.Equal(appended[2:], expectedBytes) {
		t.Fatalf("appended payload mismatch:\ngot:  %x\nwant: %x", appended[2:], expectedBytes)
	}

	// Verify DecodeFooter on independent oracle bytes
	decoded, err := sstable.DecodeFooter(expectedBytes)
	if err != nil {
		t.Fatalf("DecodeFooter on independent oracle failed: %v", err)
	}
	if decoded.MetaIndexHandle != meta {
		t.Fatalf("decoded MetaIndexHandle mismatch: got %+v, want %+v", decoded.MetaIndexHandle, meta)
	}
	if decoded.IndexHandle != index {
		t.Fatalf("decoded IndexHandle mismatch: got %+v, want %+v", decoded.IndexHandle, index)
	}
}

// TestFooter_EncodeDecode_RoundTrip tests round-trip encoding and decoding across
// varied offsets, sizes, and extreme 64-bit boundaries.
func TestFooter_EncodeDecode_RoundTrip(t *testing.T) {
	testCases := []struct {
		name   string
		footer sstable.Footer
	}{
		{
			name: "standard 4KB table footer",
			footer: sstable.Footer{
				MetaIndexHandle: sstable.BlockHandle{Offset: 4096, Size: 256},
				IndexHandle:     sstable.BlockHandle{Offset: 4352, Size: 512},
			},
		},
		{
			name: "large multi-gigabyte offsets",
			footer: sstable.Footer{
				MetaIndexHandle: sstable.BlockHandle{Offset: 10737418240, Size: 65536},
				IndexHandle:     sstable.BlockHandle{Offset: 10737483776, Size: 131072},
			},
		},
		{
			name: "size 1 minimum valid payload",
			footer: sstable.Footer{
				MetaIndexHandle: sstable.BlockHandle{Offset: 0, Size: 1},
				IndexHandle:     sstable.BlockHandle{Offset: 1, Size: 1},
			},
		},
		{
			name: "handles near 64-bit uint boundary",
			footer: sstable.Footer{
				MetaIndexHandle: sstable.BlockHandle{Offset: math.MaxUint64 - 1000, Size: 500},
				IndexHandle:     sstable.BlockHandle{Offset: math.MaxUint64 - 500, Size: 500},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			encoded := tc.footer.Encode()
			decoded, err := sstable.DecodeFooter(encoded[:])
			if err != nil {
				t.Fatalf("unexpected decode error: %v", err)
			}
			if decoded != tc.footer {
				t.Fatalf("footer round-trip mismatch:\ngot:  %+v\nwant: %+v", decoded, tc.footer)
			}

			// Also verify method receiver Decode
			var f sstable.Footer
			if err := f.Decode(encoded[:]); err != nil {
				t.Fatalf("f.Decode failed: %v", err)
			}
			if f != tc.footer {
				t.Fatalf("f.Decode mismatch: got %+v, want %+v", f, tc.footer)
			}
		})
	}
}

// TestFooter_MagicValidation verifies that invalid magic numbers are rejected deterministically.
func TestFooter_MagicValidation(t *testing.T) {
	baseFooter := sstable.Footer{
		MetaIndexHandle: sstable.BlockHandle{Offset: 0, Size: 100},
		IndexHandle:     sstable.BlockHandle{Offset: 100, Size: 100},
	}
	validBytes := baseFooter.Encode()

	t.Run("single-bit flip in magic bytes rejected", func(t *testing.T) {
		for byteIdx := 40; byteIdx < 48; byteIdx++ {
			corrupted := validBytes
			corrupted[byteIdx] ^= 0x01

			_, err := sstable.DecodeFooter(corrupted[:])
			if err == nil {
				t.Fatalf("byte %d bit flip was not rejected", byteIdx)
			}
			if !stdErrors.Is(err, errors.ErrInvalidFooterMagic) {
				t.Fatalf("expected ErrInvalidFooterMagic for byte %d flip, got %v", byteIdx, err)
			}
			if !stdErrors.Is(err, errors.ErrInvalidFooter) {
				t.Fatalf("expected ErrInvalidFooter for byte %d flip, got %v", byteIdx, err)
			}
			var magicErr *errors.InvalidFooterMagicError
			if !stdErrors.As(err, &magicErr) {
				t.Fatalf("expected *errors.InvalidFooterMagicError, got %v", err)
			}
			if magicErr.Expected != sstable.FooterMagic {
				t.Fatalf("expected magic mismatch: got %x, want %x", magicErr.Expected, sstable.FooterMagic)
			}
		}
	})

	t.Run("all zero magic rejected", func(t *testing.T) {
		corrupted := validBytes
		for i := 40; i < 48; i++ {
			corrupted[i] = 0x00
		}
		_, err := sstable.DecodeFooter(corrupted[:])
		if !stdErrors.Is(err, errors.ErrInvalidFooterMagic) {
			t.Fatalf("expected ErrInvalidFooterMagic for zero magic, got %v", err)
		}
	})

	t.Run("all FF magic rejected", func(t *testing.T) {
		corrupted := validBytes
		for i := 40; i < 48; i++ {
			corrupted[i] = 0xFF
		}
		_, err := sstable.DecodeFooter(corrupted[:])
		if !stdErrors.Is(err, errors.ErrInvalidFooterMagic) {
			t.Fatalf("expected ErrInvalidFooterMagic for 0xFF magic, got %v", err)
		}
	})

	t.Run("byte-swapped Little-Endian magic rejected", func(t *testing.T) {
		corrupted := validBytes
		// Swap endianness of the magic bytes
		for i := 0; i < 4; i++ {
			corrupted[40+i], corrupted[47-i] = corrupted[47-i], corrupted[40+i]
		}
		_, err := sstable.DecodeFooter(corrupted[:])
		if !stdErrors.Is(err, errors.ErrInvalidFooterMagic) {
			t.Fatalf("expected ErrInvalidFooterMagic for endian-swapped magic, got %v", err)
		}
	})
}

// TestFooter_PaddingValidation asserts that reserved padding bytes [32:40] must be zero.
func TestFooter_PaddingValidation(t *testing.T) {
	baseFooter := sstable.Footer{
		MetaIndexHandle: sstable.BlockHandle{Offset: 0, Size: 100},
		IndexHandle:     sstable.BlockHandle{Offset: 100, Size: 100},
	}
	validBytes := baseFooter.Encode()

	// Verify valid padding passes
	if _, err := sstable.DecodeFooter(validBytes[:]); err != nil {
		t.Fatalf("valid footer failed to decode: %v", err)
	}

	// Verify each padding byte position with non-zero values
	for padIdx := 32; padIdx < 40; padIdx++ {
		corrupted := validBytes
		corrupted[padIdx] = 0x01

		_, err := sstable.DecodeFooter(corrupted[:])
		if err == nil {
			t.Fatalf("padding byte %d = 0x01 was not rejected", padIdx)
		}
		if !stdErrors.Is(err, errors.ErrInvalidFooterPadding) {
			t.Fatalf("expected ErrInvalidFooterPadding at byte %d, got %v", padIdx, err)
		}
		if !stdErrors.Is(err, errors.ErrInvalidFooter) {
			t.Fatalf("expected ErrInvalidFooter at byte %d, got %v", padIdx, err)
		}

		var padErr *errors.InvalidFooterPaddingError
		if !stdErrors.As(err, &padErr) {
			t.Fatalf("expected *errors.InvalidFooterPaddingError, got %v", err)
		}
	}
}

// TestFooter_HandleValidation verifies that invalid BlockHandles are rejected during decode and validate.
func TestFooter_HandleValidation(t *testing.T) {
	t.Run("meta handle zero size rejected", func(t *testing.T) {
		f := sstable.Footer{
			MetaIndexHandle: sstable.BlockHandle{Offset: 100, Size: 0},
			IndexHandle:     sstable.BlockHandle{Offset: 200, Size: 100},
		}
		if err := f.Validate(); !stdErrors.Is(err, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected ErrInvalidBlockHandle for meta size=0, got %v", err)
		}

		encoded := f.Encode()
		_, decodeErr := sstable.DecodeFooter(encoded[:])
		if !stdErrors.Is(decodeErr, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected DecodeFooter to reject zero-size meta handle, got %v", decodeErr)
		}
	})

	t.Run("index handle zero size rejected", func(t *testing.T) {
		f := sstable.Footer{
			MetaIndexHandle: sstable.BlockHandle{Offset: 100, Size: 100},
			IndexHandle:     sstable.BlockHandle{Offset: 200, Size: 0},
		}
		if err := f.Validate(); !stdErrors.Is(err, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected ErrInvalidBlockHandle for index size=0, got %v", err)
		}

		encoded := f.Encode()
		_, decodeErr := sstable.DecodeFooter(encoded[:])
		if !stdErrors.Is(decodeErr, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected DecodeFooter to reject zero-size index handle, got %v", decodeErr)
		}
	})

	t.Run("meta handle overflow rejected", func(t *testing.T) {
		f := sstable.Footer{
			MetaIndexHandle: sstable.BlockHandle{Offset: math.MaxUint64 - 5, Size: 10},
			IndexHandle:     sstable.BlockHandle{Offset: 100, Size: 100},
		}
		if err := f.Validate(); !stdErrors.Is(err, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected ErrInvalidBlockHandle for meta overflow, got %v", err)
		}

		encoded := f.Encode()
		_, decodeErr := sstable.DecodeFooter(encoded[:])
		if !stdErrors.Is(decodeErr, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected DecodeFooter to reject overflowing meta handle, got %v", decodeErr)
		}
	})

	t.Run("index handle overflow rejected", func(t *testing.T) {
		f := sstable.Footer{
			MetaIndexHandle: sstable.BlockHandle{Offset: 100, Size: 100},
			IndexHandle:     sstable.BlockHandle{Offset: math.MaxUint64 - 5, Size: 10},
		}
		if err := f.Validate(); !stdErrors.Is(err, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected ErrInvalidBlockHandle for index overflow, got %v", err)
		}

		encoded := f.Encode()
		_, decodeErr := sstable.DecodeFooter(encoded[:])
		if !stdErrors.Is(decodeErr, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected DecodeFooter to reject overflowing index handle, got %v", decodeErr)
		}
	})
}

// TestFooter_Truncation asserts that every input shorter than 48 bytes is rejected with ErrFooterTruncated.
func TestFooter_Truncation(t *testing.T) {
	baseFooter := sstable.Footer{
		MetaIndexHandle: sstable.BlockHandle{Offset: 100, Size: 100},
		IndexHandle:     sstable.BlockHandle{Offset: 200, Size: 100},
	}
	validBytes := baseFooter.Encode()

	lengthsToTest := []int{0, 1, 15, 16, 31, 32, 39, 40, 47}
	for _, l := range lengthsToTest {
		truncated := validBytes[:l]
		_, err := sstable.DecodeFooter(truncated)
		if err == nil {
			t.Fatalf("expected error for truncated buffer of len %d, got nil", l)
		}
		if !stdErrors.Is(err, errors.ErrFooterTruncated) {
			t.Fatalf("expected ErrFooterTruncated for len %d, got %v", l, err)
		}
		if !stdErrors.Is(err, errors.ErrInvalidFooter) {
			t.Fatalf("expected ErrInvalidFooter for len %d, got %v", l, err)
		}
		var sizeErr *errors.InvalidFooterSizeError
		if !stdErrors.As(err, &sizeErr) {
			t.Fatalf("expected *errors.InvalidFooterSizeError for len %d, got %v", l, err)
		}
		if sizeErr.Actual != l || sizeErr.Expected != sstable.FooterSize {
			t.Fatalf("sizeErr mismatch: got Actual=%d, Expected=%d", sizeErr.Actual, sizeErr.Expected)
		}
	}
}

// TestFooter_TrailingBytes asserts that buffers exceeding 48 bytes are rejected with ErrInvalidFooterSize.
func TestFooter_TrailingBytes(t *testing.T) {
	baseFooter := sstable.Footer{
		MetaIndexHandle: sstable.BlockHandle{Offset: 100, Size: 100},
		IndexHandle:     sstable.BlockHandle{Offset: 200, Size: 100},
	}
	validBytes := baseFooter.Encode()

	oversized := make([]byte, 49)
	copy(oversized, validBytes[:])
	oversized[48] = 0xAA

	_, err := sstable.DecodeFooter(oversized)
	if err == nil {
		t.Fatal("expected error for 49-byte buffer, got nil")
	}
	if !stdErrors.Is(err, errors.ErrInvalidFooterSize) {
		t.Fatalf("expected ErrInvalidFooterSize for 49 bytes, got %v", err)
	}
	if stdErrors.Is(err, errors.ErrFooterTruncated) {
		t.Fatal("oversized buffer must not match ErrFooterTruncated")
	}
	if !stdErrors.Is(err, errors.ErrInvalidFooter) {
		t.Fatalf("expected ErrInvalidFooter for 49 bytes, got %v", err)
	}
}

// TestFooter_NilAndZeroValue asserts safety on nil receiver and all-zero bytes.
func TestFooter_NilAndZeroValue(t *testing.T) {
	t.Run("nil receiver Decode returns ErrNilReceiver", func(t *testing.T) {
		var nilFooter *sstable.Footer
		buf := make([]byte, sstable.FooterSize)
		err := nilFooter.Decode(buf)
		if !stdErrors.Is(err, errors.ErrNilReceiver) {
			t.Fatalf("expected ErrNilReceiver, got %v", err)
		}
	})

	t.Run("all zero 48-byte buffer rejected due to invalid magic", func(t *testing.T) {
		zeroBuf := make([]byte, sstable.FooterSize)
		_, err := sstable.DecodeFooter(zeroBuf)
		if err == nil {
			t.Fatal("expected decode error on zero bytes, got nil")
		}
		if !stdErrors.Is(err, errors.ErrInvalidFooterMagic) {
			t.Fatalf("expected ErrInvalidFooterMagic for zero bytes, got %v", err)
		}
	})

	t.Run("zero value Footer.Validate rejects zero handles", func(t *testing.T) {
		var zeroFooter sstable.Footer
		if err := zeroFooter.Validate(); !stdErrors.Is(err, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected ErrInvalidBlockHandle for zero value footer, got %v", err)
		}
	})
}

// TestFooter_ValidateAgainstFileSize verifies physical file bounds and footer exclusion invariants.
func TestFooter_ValidateAgainstFileSize(t *testing.T) {
	footer := sstable.Footer{
		MetaIndexHandle: sstable.BlockHandle{Offset: 1000, Size: 500},
		IndexHandle:     sstable.BlockHandle{Offset: 1500, Size: 500},
	}

	t.Run("file size smaller than footer rejected", func(t *testing.T) {
		err := footer.ValidateAgainstFileSize(47)
		if !stdErrors.Is(err, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected ErrInvalidBlockHandle for fileSize < 48, got %v", err)
		}
	})

	t.Run("index handle overlapping footer boundary rejected", func(t *testing.T) {
		// Index spans [1500, 2000). Total file size is 2047.
		// Allowed boundary is fileSize - 48 = 2047 - 48 = 1999.
		// Since 2000 > 1999, handle overlaps footer!
		err := footer.ValidateAgainstFileSize(2047)
		if !stdErrors.Is(err, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected ErrInvalidBlockHandle when handle overlaps footer, got %v", err)
		}
	})

	t.Run("exact physical boundary accepted", func(t *testing.T) {
		// Index spans [1500, 2000). Total file size is 2048.
		// Allowed boundary is 2048 - 48 = 2000.
		// Index ends exactly at 2000; footer occupies [2000, 2048).
		if err := footer.ValidateAgainstFileSize(2048); err != nil {
			t.Fatalf("expected valid footer at exact file boundary, got %v", err)
		}
	})

	t.Run("meta handle exceeding boundary rejected", func(t *testing.T) {
		badFooter := sstable.Footer{
			MetaIndexHandle: sstable.BlockHandle{Offset: 2000, Size: 100}, // ends at 2100
			IndexHandle:     sstable.BlockHandle{Offset: 1500, Size: 500}, // ends at 2000
		}
		// File size 2100 -> boundary is 2100 - 48 = 2052. Meta ends at 2100 > 2052.
		err := badFooter.ValidateAgainstFileSize(2100)
		if !stdErrors.Is(err, errors.ErrInvalidBlockHandle) {
			t.Fatalf("expected ErrInvalidBlockHandle when meta exceeds boundary, got %v", err)
		}
	})

	t.Run("well within bounds accepted", func(t *testing.T) {
		if err := footer.ValidateAgainstFileSize(10000); err != nil {
			t.Fatalf("expected valid footer within large file, got %v", err)
		}
	})
}
