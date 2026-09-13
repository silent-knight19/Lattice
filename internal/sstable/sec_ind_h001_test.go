package sstable_test

import (
	stdErrors "errors"
	"math"
	"strings"
	"testing"

	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// TestINDH001_BlockHandleOverflowRejected proves the IND-H-001 verdict: a
// malicious block handle with offset=MaxUint64-10 and size=20 (whose naive sum
// wraps to 9) must be rejected at decode time with ErrInvalidBlockHandle —
// never decoded into a small wrapped end position enabling out-of-bounds reads.
func TestINDH001_BlockHandleOverflowRejected(t *testing.T) {
	var wire [sstable.BlockHandleSize]byte
	putBE(wire[0:8], math.MaxUint64-10)
	putBE(wire[8:16], 20)

	if _, err := sstable.DecodeBlockHandle(wire[:]); err == nil {
		t.Fatalf("DecodeBlockHandle accepted wrapping handle (offset=MaxUint64-10, size=20)")
	} else {
		if !stdErrors.Is(err, errors.ErrInvalidBlockHandle) {
			t.Errorf("expected ErrInvalidBlockHandle, got %T (%v)", err, err)
		}
		var typed *errors.InvalidBlockHandleError
		if !stdErrors.As(err, &typed) {
			t.Fatalf("expected *InvalidBlockHandleError, got %T (%v)", err, err)
		}
		if !strings.Contains(typed.Reason, "overflow") {
			t.Errorf("expected overflow guard to fire, reason = %q", typed.Reason)
		}
	}

	// Defense in depth: the reader-level file-size check must also reject,
	// and must do so without ever computing a wrapped end offset.
	h := sstable.BlockHandle{Offset: math.MaxUint64 - 10, Size: 20}
	if err := h.ValidateAgainstFileSize(1 << 20); err == nil {
		t.Fatalf("ValidateAgainstFileSize accepted wrapping handle")
	} else if !stdErrors.Is(err, errors.ErrInvalidBlockHandle) {
		t.Errorf("expected ErrInvalidBlockHandle, got %v", err)
	}
}

// TestINDH001_BlockHandleOverflowBoundaryMatrix pins the exact overflow
// boundary: offset+size == MaxUint64 is representable (no wrap) while any sum
// one byte beyond it is rejected — proving the guard is `>` and not `>=`.
func TestINDH001_BlockHandleOverflowBoundaryMatrix(t *testing.T) {
	// Exact-fit at the address-space ceiling must pass the overflow guard
	// itself (it may still fail other guards such as arch bounds or file size;
	// here size is tiny and file size is huge, so only the overflow guard matters).
	fit := sstable.BlockHandle{Offset: math.MaxUint64 - 20, Size: 20}
	if err := fit.Validate(); err != nil {
		t.Errorf("exact-fit handle (end == MaxUint64) wrongly rejected: %v", err)
	}

	cases := []struct {
		name   string
		offset uint64
		size   uint64
	}{
		{"AuditScenario_MaxUint64Minus10_Plus20", math.MaxUint64 - 10, 20},
		{"MaxUint64_Plus1", math.MaxUint64, 1},
		{"MaxUint64_PlusMaxSmall", math.MaxUint64 - 1, 2},
		{"HalfPlusHalf", math.MaxUint64/2 + 1, math.MaxUint64/2 + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := sstable.BlockHandle{Offset: tc.offset, Size: tc.size}
			if err := h.Validate(); err == nil {
				t.Fatalf("overflow guard missed %+v", h)
			} else if !stdErrors.Is(err, errors.ErrInvalidBlockHandle) {
				t.Fatalf("expected ErrInvalidBlockHandle, got %v", err)
			}
			enc := h.Encode()
			if _, err := sstable.DecodeBlockHandle(enc[:]); err == nil {
				t.Fatalf("DecodeBlockHandle accepted %+v", h)
			}
		})
	}
}

func putBE(b []byte, v uint64) {
	for i := 7; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
}
