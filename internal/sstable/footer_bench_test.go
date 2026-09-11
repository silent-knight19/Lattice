package sstable_test

import (
	"testing"

	"github.com/silent-knight19/lattice/internal/sstable"
)

// BenchmarkFooter_Encode measures serialization of a Footer into a fixed 48-byte array.
func BenchmarkFooter_Encode(b *testing.B) {
	footer := sstable.Footer{
		MetaIndexHandle: sstable.BlockHandle{Offset: 1048576, Size: 65536},
		IndexHandle:     sstable.BlockHandle{Offset: 2097152, Size: 131072},
	}

	b.ResetTimer()
	b.ReportAllocs()

	var sink [sstable.FooterSize]byte
	for i := 0; i < b.N; i++ {
		sink = footer.Encode()
	}
	_ = sink
}

// BenchmarkFooter_AppendTo measures appending a serialized 48-byte footer into an existing slice buffer.
func BenchmarkFooter_AppendTo(b *testing.B) {
	footer := sstable.Footer{
		MetaIndexHandle: sstable.BlockHandle{Offset: 1048576, Size: 65536},
		IndexHandle:     sstable.BlockHandle{Offset: 2097152, Size: 131072},
	}
	buf := make([]byte, 0, sstable.FooterSize)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		buf = buf[:0]
		buf = footer.AppendTo(buf)
	}
}

// BenchmarkFooter_Decode measures parsing and validating a raw 48-byte slice into a Footer struct.
func BenchmarkFooter_Decode(b *testing.B) {
	footer := sstable.Footer{
		MetaIndexHandle: sstable.BlockHandle{Offset: 1048576, Size: 65536},
		IndexHandle:     sstable.BlockHandle{Offset: 2097152, Size: 131072},
	}
	raw := footer.Encode()
	src := raw[:]

	b.ResetTimer()
	b.ReportAllocs()

	var decoded sstable.Footer
	for i := 0; i < b.N; i++ {
		if err := decoded.Decode(src); err != nil {
			b.Fatalf("Decode failed: %v", err)
		}
	}
}

// BenchmarkFooter_RoundTrip measures the full Encode -> Decode cycle for a Footer.
func BenchmarkFooter_RoundTrip(b *testing.B) {
	footer := sstable.Footer{
		MetaIndexHandle: sstable.BlockHandle{Offset: 1048576, Size: 65536},
		IndexHandle:     sstable.BlockHandle{Offset: 2097152, Size: 131072},
	}

	b.ResetTimer()
	b.ReportAllocs()

	var decoded sstable.Footer
	for i := 0; i < b.N; i++ {
		encoded := footer.Encode()
		if err := decoded.Decode(encoded[:]); err != nil {
			b.Fatalf("Decode failed: %v", err)
		}
	}
}
