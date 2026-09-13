package binary_test

import (
	stdErrors "errors"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// TestINDL001_DualVarintPolicy pins the IND-L-001 decision (keep lenient):
// the standard decoder accepts non-canonical (overlong) encodings for wire
// compatibility while decoding the mathematically correct value, and the
// canonical-strict variant rejects the same bytes. Both must agree on all
// canonical inputs.
func TestINDL001_DualVarintPolicy(t *testing.T) {
	overlong := []struct {
		name  string
		bytes []byte
		value uint64
	}{
		{"zero_as_80_00", []byte{0x80, 0x00}, 0},
		{"one_as_81_00", []byte{0x81, 0x00}, 1},
		{"127_as_81_7F", []byte{0xFF, 0x00}, 127},
	}

	for _, tc := range overlong {
		t.Run(tc.name, func(t *testing.T) {
			val, n, err := binary.GetVarint64(tc.bytes)
			if err != nil {
				t.Fatalf("lenient decoder rejected overlong input: %v", err)
			}
			if val != tc.value || n != len(tc.bytes) {
				t.Fatalf("lenient decode = (%d, %d), want (%d, %d)", val, n, tc.value, len(tc.bytes))
			}
			if _, _, err := binary.GetVarint64Canonical(tc.bytes); !stdErrors.Is(err, errors.ErrVarintNonCanonical) {
				t.Fatalf("strict decoder must reject overlong with ErrVarintNonCanonical, got %v", err)
			}
		})
	}

	t.Run("CanonicalAgreement", func(t *testing.T) {
		for _, v := range []uint64{0, 1, 127, 128, 300, 1 << 32, ^uint64(0)} {
			var buf [binary.MaxVarintLen64]byte
			n := binary.PutVarint64(buf[:], v) // encoder always emits canonical
			lv, ln, err := binary.GetVarint64(buf[:n])
			if err != nil || lv != v || ln != n {
				t.Fatalf("lenient round-trip failed for %d: (%d, %d, %v)", v, lv, ln, err)
			}
			sv, sn, err := binary.GetVarint64Canonical(buf[:n])
			if err != nil || sv != v || sn != n {
				t.Fatalf("strict round-trip failed for %d: (%d, %d, %v)", v, sv, sn, err)
			}
		}
	})
}
