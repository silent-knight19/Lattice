package sstable_test

import (
	"fmt"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/sstable"
)

var (
	sinkBytes []byte
	sinkInt   int
)

func BenchmarkBlockBuilder_Add_1K(b *testing.B) {
	const count = 1000
	keys := make([]binary.InternalKey, count)
	vals := make([][]byte, count)

	for i := 0; i < count; i++ {
		k, err := binary.NewInternalKey([]byte(fmt.Sprintf("user:session:key:%06d", i)), binary.SeqNum(count-i), binary.OpTypePut)
		if err != nil {
			b.Fatalf("failed to create key: %v", err)
		}
		keys[i] = k
		vals[i] = []byte(fmt.Sprintf("payload-%06d", i))
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		builder := sstable.NewBlockBuilder()
		for j := 0; j < count; j++ {
			if err := builder.Add(keys[j], vals[j]); err != nil {
				b.Fatalf("Add failed: %v", err)
			}
		}
		sinkInt = builder.DataSize()
	}
}

func BenchmarkBlockBuilder_Add_10K(b *testing.B) {
	const count = 10000
	keys := make([]binary.InternalKey, count)
	vals := make([][]byte, count)

	for i := 0; i < count; i++ {
		k, err := binary.NewInternalKey([]byte(fmt.Sprintf("user:session:key:%06d", i)), binary.SeqNum(count-i), binary.OpTypePut)
		if err != nil {
			b.Fatalf("failed to create key: %v", err)
		}
		keys[i] = k
		vals[i] = []byte(fmt.Sprintf("payload-%06d", i))
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		builder := sstable.NewBlockBuilder()
		for j := 0; j < count; j++ {
			if err := builder.Add(keys[j], vals[j]); err != nil {
				b.Fatalf("Add failed: %v", err)
			}
		}
		sinkInt = builder.DataSize()
	}
}

func BenchmarkBlockBuilder_Finish(b *testing.B) {
	const count = 100
	builder := sstable.NewBlockBuilder()
	for i := 0; i < count; i++ {
		k, err := binary.NewInternalKey([]byte(fmt.Sprintf("key:%04d", i)), binary.SeqNum(count-i), binary.OpTypePut)
		if err != nil {
			b.Fatalf("failed to create key: %v", err)
		}
		if err := builder.Add(k, []byte("val")); err != nil {
			b.Fatalf("Add failed: %v", err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		data := builder.Finish()
		sinkBytes = data
	}
}

func BenchmarkBlockBuilder_SharedPrefix(b *testing.B) {
	// Keys with a 32-byte shared prefix
	const count = 1000
	keys := make([]binary.InternalKey, count)
	val := []byte("val")

	for i := 0; i < count; i++ {
		k, err := binary.NewInternalKey([]byte(fmt.Sprintf("cluster:us-east-1:datacenter:01:node:%04d", i)), binary.SeqNum(count-i), binary.OpTypePut)
		if err != nil {
			b.Fatalf("failed to create key: %v", err)
		}
		keys[i] = k
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		builder := sstable.NewBlockBuilder()
		for j := 0; j < count; j++ {
			if err := builder.Add(keys[j], val); err != nil {
				b.Fatalf("Add failed: %v", err)
			}
		}
		sinkInt = builder.DataSize()
	}
}

func BenchmarkBlockBuilder_NoSharedPrefix(b *testing.B) {
	// Keys with zero shared prefix (single varying character prefixes)
	const count = 1000
	keys := make([]binary.InternalKey, count)
	val := []byte("val")

	for i := 0; i < count; i++ {
		// Generate distinct initial letters to minimize shared prefix
		kStr := fmt.Sprintf("%06d-key-payload", i)
		k, err := binary.NewInternalKey([]byte(kStr), binary.SeqNum(count-i), binary.OpTypePut)
		if err != nil {
			b.Fatalf("failed to create key: %v", err)
		}
		keys[i] = k
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		builder := sstable.NewBlockBuilder()
		for j := 0; j < count; j++ {
			if err := builder.Add(keys[j], val); err != nil {
				b.Fatalf("Add failed: %v", err)
			}
		}
		sinkInt = builder.DataSize()
	}
}

// TestCompressionCharacteristics empirically compares the encoded size of records with
// shared prefixes vs raw uncompressed size.
func TestCompressionCharacteristics(t *testing.T) {
	const count = 100
	builder := sstable.NewBlockBuilder()

	rawUncompressedBytes := 0
	for i := 0; i < count; i++ {
		uKey := []byte(fmt.Sprintf("service:production:cluster:region:us-west:tenant:100:sensor:%06d", i))
		k, err := binary.NewInternalKey(uKey, binary.SeqNum(count-i), binary.OpTypePut)
		if err != nil {
			t.Fatalf("failed to create key: %v", err)
		}
		v := []byte(fmt.Sprintf("measurement-payload-data-value-epoch-timestamp-%06d", i))
		rawUncompressedBytes += len(uKey) + binary.InternalKeyTrailerLen + len(v)

		if err := builder.Add(k, v); err != nil {
			t.Fatalf("Add failed: %v", err)
		}
	}

	compressedBytes := builder.DataSize()
	ratio := float64(rawUncompressedBytes) / float64(compressedBytes)

	t.Logf("Empirical Compression Characteristics:")
	t.Logf("  Raw Uncompressed Size : %d bytes", rawUncompressedBytes)
	t.Logf("  BlockBuilder Size     : %d bytes", compressedBytes)
	t.Logf("  Compression Ratio     : %.2fx (%.1f%% space reduction)", ratio, (1.0-1.0/ratio)*100.0)

	// Invariant: For keys with shared prefix length > 50 bytes, prefix compression must yield > 1.5x ratio
	if ratio <= 1.5 {
		t.Errorf("expected compression ratio > 1.5x, got %.2fx", ratio)
	}
}
