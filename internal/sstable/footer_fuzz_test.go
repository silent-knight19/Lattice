package sstable_test

import (
	"testing"

	"github.com/silent-knight19/lattice/internal/sstable"
)

// FuzzDecodeFooter verifies that DecodeFooter is immune to panics, crashes, and slice bounds
// errors when processing arbitrary untrusted binary input, and that any successfully decoded
// footer strictly satisfies round-trip and validation invariants.
func FuzzDecodeFooter(f *testing.F) {
	// Seed 1: Valid standard footer
	f1 := sstable.Footer{
		MetaIndexHandle: sstable.BlockHandle{Offset: 4096, Size: 256},
		IndexHandle:     sstable.BlockHandle{Offset: 4352, Size: 512},
	}
	b1 := f1.Encode()
	f.Add(b1[:])

	// Seed 2: Valid footer with large offsets
	f2 := sstable.Footer{
		MetaIndexHandle: sstable.BlockHandle{Offset: 1048576, Size: 65536},
		IndexHandle:     sstable.BlockHandle{Offset: 2097152, Size: 131072},
	}
	b2 := f2.Encode()
	f.Add(b2[:])

	// Seed edge cases
	f.Add([]byte{})
	f.Add(make([]byte, 1))
	f.Add(make([]byte, 16))
	f.Add(make([]byte, 32))
	f.Add(make([]byte, 47)) // truncated by 1 byte
	f.Add(make([]byte, 48)) // all zero
	f.Add(make([]byte, 49)) // oversized by 1 byte
	f.Add(make([]byte, 64))

	// All 0xFF
	allFF := make([]byte, 48)
	for i := range allFF {
		allFF[i] = 0xFF
	}
	f.Add(allFF)

	f.Fuzz(func(t *testing.T, data []byte) {
		footer, err := sstable.DecodeFooter(data)
		if err != nil {
			return
		}

		// Invariant 1: Any successfully decoded footer must pass Validate()
		if err := footer.Validate(); err != nil {
			t.Fatalf("DecodeFooter returned footer violating Validate(): %v", err)
		}

		// Invariant 2: Handles must have non-zero size
		if footer.MetaIndexHandle.Size == 0 || footer.IndexHandle.Size == 0 {
			t.Fatalf("DecodeFooter accepted zero-size handle: %+v", footer)
		}

		// Invariant 3: Re-encoding must produce exact 48 bytes that decode back to identical footer
		reEncoded := footer.Encode()
		if len(reEncoded) != sstable.FooterSize {
			t.Fatalf("reEncoded length %d != 48", len(reEncoded))
		}

		decodedAgain, err := sstable.DecodeFooter(reEncoded[:])
		if err != nil {
			t.Fatalf("failed to decode re-encoded footer: %v", err)
		}
		if decodedAgain != footer {
			t.Fatalf("round-trip mismatch: got %+v, want %+v", decodedAgain, footer)
		}
	})
}
