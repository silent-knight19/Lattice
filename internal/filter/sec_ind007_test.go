package filter_test

import (
	"math"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/filter"
)

// TestIND007_ExcessiveBloomFilterAllocation verifies that DecodeFilterBlock rejects
// attacker-controlled oversized or overflowing bitCount values without attempting massive heap allocations.
func TestIND007_ExcessiveBloomFilterAllocation(t *testing.T) {
	testCases := []struct {
		name     string
		bitCount uint64
	}{
		{
			name:     "AuditScenario_1BillionBits",
			bitCount: 1 << 30, // 1,073,741,824 bits (~128MB) but with mismatched trailer
		},
		{
			name:     "MaxUint64NearOverflow",
			bitCount: math.MaxUint64 - 3,
		},
		{
			name:     "MaxUint64",
			bitCount: math.MaxUint64,
		},
		{
			name:     "ExceedsMaxBitsetBytes",
			bitCount: uint64(filter.MaxBitsetBytes)*8 + 8,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Construct a 13-byte buffer with the test bitCount, k=7, and valid CRC
			var buf [filter.FilterBlockTrailerSize]byte
			binary.PutUint64(buf[0:8], tc.bitCount)
			buf[8] = 7 // k = 7
			crc := binary.Checksum(buf[:9])
			binary.PutUint32(buf[9:13], crc)

			_, err := filter.DecodeFilterBlock(buf[:])
			if err == nil {
				t.Fatalf("expected DecodeFilterBlock to reject excessive bitCount %d", tc.bitCount)
			}
		})
	}
}
