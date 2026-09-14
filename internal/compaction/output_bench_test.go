package compaction

import (
	"bytes"
	"fmt"
	"os"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
)

// makeBenchRecords generates n sorted mockRecords with average payload size.
func makeBenchRecords(n int, valSize int, tombstoneRatio float64) []mockRecord {
	records := make([]mockRecord, n)
	valPayload := bytes.Repeat([]byte("x"), valSize)

	for i := 0; i < n; i++ {
		isDelete := tombstoneRatio > 0 && (float64(i%100)/100.0 < tombstoneRatio)
		op := binary.OpTypePut
		var v []byte
		if isDelete {
			op = binary.OpTypeDelete
			v = nil
		} else {
			v = valPayload
		}

		diff := n - i
		if diff < 0 {
			diff = 0
		}
		records[i] = mockRecord{
			key: binary.InternalKey{
				UserKey: []byte(fmt.Sprintf("key_%08d", i)),
				SeqNum:  binary.SeqNum(uint64(diff)),
				OpType:  op,
			},
			val: v,
		}
	}
	return records
}

// BenchmarkCompactionOutput_Small_1K benchmarks generating ~1,000 records into a single SSTable.
func BenchmarkCompactionOutput_Small_1K(b *testing.B) {
	records := makeBenchRecords(1000, 100, 0.0)
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool { return false })

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		dbDir, err := os.MkdirTemp("", "bench_output_small_*")
		if err != nil {
			b.Fatalf("failed to create temp dir: %v", err)
		}
		alloc := simpleSeqAllocator(1)
		cfg := DefaultCompactionOutputConfig()
		cfg.DbDir = dbDir
		cfg.TargetLevel = 1
		cfg.SkipValidation = true // avoid re-reading file in write benchmark
		iter := newMockSliceIterator(records)
		b.StartTimer()

		out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
		if err != nil {
			b.Fatalf("BuildCompactionOutput failed: %v", err)
		}

		b.StopTimer()
		if len(out.Files) == 0 {
			b.Fatalf("expected output files")
		}
		_ = os.RemoveAll(dbDir)
		b.StartTimer()
	}
	b.SetBytes(int64(len(records) * 108)) // key + val
}

// BenchmarkCompactionOutput_Medium_100K benchmarks streaming 100,000 records across multiple ~2 MiB SSTables.
func BenchmarkCompactionOutput_Medium_100K(b *testing.B) {
	records := makeBenchRecords(100_000, 100, 0.0)
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool { return false })

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		dbDir, err := os.MkdirTemp("", "bench_output_medium_*")
		if err != nil {
			b.Fatalf("failed to create temp dir: %v", err)
		}
		alloc := simpleSeqAllocator(1)
		cfg := DefaultCompactionOutputConfig()
		cfg.DbDir = dbDir
		cfg.TargetLevel = 1
		cfg.SkipValidation = true
		iter := newMockSliceIterator(records)
		b.StartTimer()

		out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
		if err != nil {
			b.Fatalf("BuildCompactionOutput failed: %v", err)
		}

		b.StopTimer()
		if len(out.Files) == 0 {
			b.Fatalf("expected output files")
		}
		_ = os.RemoveAll(dbDir)
		b.StartTimer()
	}
	b.SetBytes(int64(len(records) * 108))
}

// BenchmarkCompactionOutput_Large_MultiplePartitions benchmarks 2 MiB partitioning on large payloads (~10 MB).
func BenchmarkCompactionOutput_Large_MultiplePartitions(b *testing.B) {
	records := makeBenchRecords(20_000, 500, 0.0) // ~10 MB total data
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool { return false })

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		dbDir, err := os.MkdirTemp("", "bench_output_large_*")
		if err != nil {
			b.Fatalf("failed to create temp dir: %v", err)
		}
		alloc := simpleSeqAllocator(1)
		cfg := DefaultCompactionOutputConfig()
		cfg.DbDir = dbDir
		cfg.TargetLevel = 1
		cfg.TargetPartitionSize = DefaultTargetPartitionSize // 2 MiB
		cfg.SkipValidation = true
		iter := newMockSliceIterator(records)
		b.StartTimer()

		out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
		if err != nil {
			b.Fatalf("BuildCompactionOutput failed: %v", err)
		}

		b.StopTimer()
		if len(out.Files) < 2 {
			b.Fatalf("expected multiple files, got %d", len(out.Files))
		}
		_ = os.RemoveAll(dbDir)
		b.StartTimer()
	}
	b.SetBytes(int64(len(records) * 508))
}

// BenchmarkCompactionOutput_TombstoneOmission benchmarks physical omission of 90% droppable tombstones.
func BenchmarkCompactionOutput_TombstoneOmission(b *testing.B) {
	records := makeBenchRecords(50_000, 100, 0.90) // 90% tombstones
	safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool {
		// All tombstones are safely droppable
		return true
	})

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		dbDir, err := os.MkdirTemp("", "bench_output_tombstone_*")
		if err != nil {
			b.Fatalf("failed to create temp dir: %v", err)
		}
		alloc := simpleSeqAllocator(1)
		cfg := DefaultCompactionOutputConfig()
		cfg.DbDir = dbDir
		cfg.TargetLevel = 1
		cfg.SkipValidation = true
		iter := newMockSliceIterator(records)
		b.StartTimer()

		out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
		if err != nil {
			b.Fatalf("BuildCompactionOutput failed: %v", err)
		}

		b.StopTimer()
		if out.Stats.OmittedTombstones == 0 {
			b.Fatalf("expected omitted tombstones")
		}
		_ = os.RemoveAll(dbDir)
		b.StartTimer()
	}
	b.SetBytes(int64(len(records) * 108))
}
