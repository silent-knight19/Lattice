package filter_test

import (
	"fmt"
	"math"
	"testing"
	"time"

	stdErrors "errors"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/filter"
)

// pdf007CraftBlock builds a raw filter block: [bitset | bitCount u64 | k u8 | crc32 u32].
func pdf007CraftBlock(bitset []byte, bitCount uint64, k byte, corruptCRC bool) []byte {
	buf := append([]byte(nil), bitset...)
	var tmp [8]byte
	binary.PutUint64(tmp[:], bitCount)
	buf = append(buf, tmp[:]...)
	buf = append(buf, k)
	crc := binary.Checksum(buf)
	if corruptCRC {
		crc ^= 0xFFFFFFFF
	}
	var cbuf [4]byte
	binary.PutUint32(cbuf[:], crc)
	return append(buf, cbuf[:]...)
}

// TestPDF_IND007_AllocationBombRejectedFast proves the PDF IND-007 verdict:
// attacker-controlled bit counts (including the audit's exact 1<<30 scenario
// and MaxUint64 wraparound values) are rejected BEFORE any allocation, so the
// decoder terminates promptly instead of attempting a ~128MB+ heap grab.
func TestPDF_IND007_AllocationBombRejectedFast(t *testing.T) {
	cases := []struct {
		name     string
		bitCount uint64
	}{
		{"AuditScenario_1BillionBits", 1 << 30},
		{"MaxUint64NearOverflow", math.MaxUint64 - 3},
		{"MaxUint64", math.MaxUint64},
		{"JustOverCap", uint64(filter.MaxBitsetBytes)*8 + 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := pdf007CraftBlock(nil, tc.bitCount, 7, false)
			done := make(chan error, 1)
			go func() {
				_, err := filter.DecodeFilterBlock(raw)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatalf("accepted allocation-bomb bitCount %d", tc.bitCount)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("decoder stalled on bitCount %d (allocation attempt?)", tc.bitCount)
			}
		})
	}
}

// TestPDF_IND007_ParserHardeningMatrix covers the remaining hostile shapes:
// truncation, checksum mismatch, wrong hash count, and declared-length lies.
func TestPDF_IND007_ParserHardeningMatrix(t *testing.T) {
	t.Run("Truncated", func(t *testing.T) {
		if _, err := filter.DecodeFilterBlock(make([]byte, 12)); !stdErrors.Is(err, errors.ErrFilterBlockTruncated) {
			t.Fatalf("want ErrFilterBlockTruncated, got %v", err)
		}
	})
	t.Run("BadCRC", func(t *testing.T) {
		raw := pdf007CraftBlock(make([]byte, 8), 64, 7, true)
		if _, err := filter.DecodeFilterBlock(raw); !stdErrors.Is(err, errors.ErrChecksumMismatch) {
			t.Fatalf("want ErrChecksumMismatch, got %v", err)
		}
	})
	t.Run("WrongHashCount", func(t *testing.T) {
		raw := pdf007CraftBlock(make([]byte, 8), 64, 6, false)
		if _, err := filter.DecodeFilterBlock(raw); !stdErrors.Is(err, errors.ErrUnsupportedHashCount) {
			t.Fatalf("want ErrUnsupportedHashCount, got %v", err)
		}
	})
	t.Run("LengthMismatch", func(t *testing.T) {
		// Declares 64 bits (8 bytes) but carries 16 bitset bytes.
		raw := pdf007CraftBlock(make([]byte, 16), 64, 7, false)
		if _, err := filter.DecodeFilterBlock(raw); !stdErrors.Is(err, errors.ErrFilterBlockCorrupted) {
			t.Fatalf("want ErrFilterBlockCorrupted, got %v", err)
		}
	})
}

// TestPDF_IND007_ValidRoundTripNoFalseNegatives proves the control: a
// honestly built filter decodes and never reports a false negative.
func TestPDF_IND007_ValidRoundTripNoFalseNegatives(t *testing.T) {
	b := filter.NewFilterBlockBuilder(100)
	if b == nil {
		t.Fatalf("NewFilterBlockBuilder returned nil")
	}
	keys := make([][]byte, 100)
	for i := range keys {
		keys[i] = []byte(fmt.Sprintf("pdf007-key:%03d", i))
		if err := b.AddKey(keys[i]); err != nil {
			t.Fatalf("AddKey failed: %v", err)
		}
	}
	f, err := filter.DecodeFilterBlock(b.Finish())
	if err != nil {
		t.Fatalf("valid block rejected: %v", err)
	}
	for _, k := range keys {
		if !f.MayContain(k) {
			t.Fatalf("false negative for %q", k)
		}
	}
}
