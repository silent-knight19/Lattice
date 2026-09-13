package binary_test

import (
	stdErrors "errors"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// TestPDF_IND008_Varint32CodecSymmetry proves the PDF IND-008 verdict: the
// 32-bit varint encoder and decoder are exact inverses — decode(encode(x))
// == x for every boundary value including the int32/uint32 edges — and
// corrupted truncations fail closed instead of yielding a smaller number.
func TestPDF_IND008_Varint32CodecSymmetry(t *testing.T) {
	boundaries := []uint32{
		0, 1, 127, 128, 255, 256, 16383, 16384,
		2097151, 2097152, 268435455, 268435456,
		uint32(1<<31 - 1), uint32(1 << 31), ^uint32(0) - 1, ^uint32(0),
	}
	for _, v := range boundaries {
		var buf [binary.MaxVarintLen32]byte
		n := binary.PutVarint32(buf[:], v)
		if n != binary.VarintLen32(v) {
			t.Fatalf("PutVarint32(%d) wrote %d bytes, want VarintLen32 %d", v, n, binary.VarintLen32(v))
		}
		got, m, err := binary.GetVarint32(buf[:n])
		if err != nil || got != v || m != n {
			t.Fatalf("round-trip(%d) = (%d, %d, %v), want (%d, %d, nil)", v, got, m, err, v, n)
		}
		// Strict variant agrees on canonical (encoder-emitted) inputs.
		sgot, sm, err := binary.GetVarint32Canonical(buf[:n])
		if err != nil || sgot != v || sm != n {
			t.Fatalf("canonical round-trip(%d) = (%d, %d, %v)", v, sgot, sm, err)
		}
	}
}

// TestPDF_IND008_Varint32CorruptionFailsClosed verifies a flipped
// continuation bit (truncated tail) is reported, never misread as small.
func TestPDF_IND008_Varint32CorruptionFailsClosed(t *testing.T) {
	var full [binary.MaxVarintLen32]byte
	n := binary.PutVarint32(full[:], ^uint32(0)) // 5-byte encoding
	for cut := 1; cut < n; cut++ {
		if _, _, err := binary.GetVarint32(full[:cut]); !stdErrors.Is(err, errors.ErrVarintTruncated) {
			t.Fatalf("cut %d/%d: want ErrVarintTruncated, got %v", cut, n, err)
		}
	}
	if _, _, err := binary.GetVarint32(nil); !stdErrors.Is(err, errors.ErrVarintTruncated) {
		t.Fatalf("empty: want ErrVarintTruncated, got %v", err)
	}
}
