package sstable_test

import (
	"fmt"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// BenchmarkIndexBuilder_AddBlock_50 measures the latency of adding 50 sequential data block handles.
func BenchmarkIndexBuilder_AddBlock_50(b *testing.B) {
	keys := make([][]byte, 50)
	handles := make([]sstable.BlockHandle, 50)
	for i := 0; i < 50; i++ {
		ik, _ := binary.NewInternalKey([]byte(fmt.Sprintf("user:%06d", i)), 1, binary.OpTypePut)
		keys[i] = binary.AppendInternalKey(nil, ik)
		handles[i] = sstable.BlockHandle{Offset: uint64(i * 4096), Size: 4096}
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		builder := sstable.NewIndexBuilder()
		for j := 0; j < 50; j++ {
			_ = builder.AddBlock(keys[j], handles[j])
		}
	}
}

// BenchmarkIndexBuilder_AddBlock_1K measures adding 1,000 data block handles (representing a 4MB SSTable).
func BenchmarkIndexBuilder_AddBlock_1K(b *testing.B) {
	const count = 1000
	keys := make([][]byte, count)
	handles := make([]sstable.BlockHandle, count)
	for i := 0; i < count; i++ {
		ik, _ := binary.NewInternalKey([]byte(fmt.Sprintf("user:%06d", i)), 1, binary.OpTypePut)
		keys[i] = binary.AppendInternalKey(nil, ik)
		handles[i] = sstable.BlockHandle{Offset: uint64(i * 4096), Size: 4096}
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		builder := sstable.NewIndexBuilder()
		for j := 0; j < count; j++ {
			_ = builder.AddBlock(keys[j], handles[j])
		}
	}
}

// BenchmarkIndexBuilder_Finish measures serialization and CRC32 computation of an index block with 50 entries.
func BenchmarkIndexBuilder_Finish(b *testing.B) {
	builder := sstable.NewIndexBuilder()
	for i := 0; i < 50; i++ {
		ik, _ := binary.NewInternalKey([]byte(fmt.Sprintf("user:%06d", i)), 1, binary.OpTypePut)
		k := binary.AppendInternalKey(nil, ik)
		h := sstable.BlockHandle{Offset: uint64(i * 4096), Size: 4096}
		_ = builder.AddBlock(k, h)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = builder.Finish()
	}
}

// BenchmarkBlockIndex_FindBlock_BinarySearch measures in-memory sparse index binary search
// across 1,000 data blocks.
func BenchmarkBlockIndex_FindBlock_BinarySearch(b *testing.B) {
	const count = 1000
	builder := sstable.NewIndexBuilder()
	probeKeys := make([][]byte, count)
	for i := 0; i < count; i++ {
		ik, _ := binary.NewInternalKey([]byte(fmt.Sprintf("user:%06d", i)), 1, binary.OpTypePut)
		k := binary.AppendInternalKey(nil, ik)
		probeKeys[i] = k
		h := sstable.BlockHandle{Offset: uint64(i * 4096), Size: 4096}
		_ = builder.AddBlock(k, h)
	}
	data := builder.Finish()
	idx, err := sstable.DecodeBlockIndex(data)
	if err != nil {
		b.Fatalf("DecodeBlockIndex failed: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		probe := probeKeys[i%count]
		_, _ = idx.FindBlock(probe)
	}
}
