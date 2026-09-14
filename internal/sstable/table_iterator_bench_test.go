package sstable

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
)

func prepareBenchmarkTable(b *testing.B, count int, targetBlockSize int) (*TableReader, func()) {
	b.Helper()
	dir := b.TempDir()
	path := filepath.Join(dir, "bench.sst")

	opts := DefaultTableWriterOptions()
	opts.TargetBlockSize = targetBlockSize

	w, err := NewTableWriter(path, opts)
	if err != nil {
		b.Fatalf("failed to create writer: %v", err)
	}

	for i := 0; i < count; i++ {
		key := binary.InternalKey{
			UserKey: []byte(fmt.Sprintf("bench-user-key-%07d", i)),
			SeqNum:  binary.SeqNum(uint64(count - i)), // #nosec G115 -- test count >= i
			OpType:  binary.OpTypePut,
		}
		val := []byte(fmt.Sprintf("bench-value-%07d-payload-content", i))
		if err := w.Add(key, val); err != nil {
			b.Fatalf("failed to add record: %v", err)
		}
	}

	if _, err := w.Finish(); err != nil {
		b.Fatalf("failed to finish writer: %v", err)
	}

	reader, err := NewTableReader(path)
	if err != nil {
		b.Fatalf("failed to open reader: %v", err)
	}

	cleanup := func() {
		_ = reader.Close()
	}
	return reader, cleanup
}

func BenchmarkTableIterator_SequentialScan_Small(b *testing.B) {
	// Small: 1 block, 10 records
	reader, cleanup := prepareBenchmarkTable(b, 10, 4096)
	defer cleanup()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := reader.NewIterator()
		if err != nil {
			b.Fatalf("failed to create iterator: %v", err)
		}
		count := 0
		for it.Next() {
			count++
		}
		if err := it.Err(); err != nil {
			b.Fatalf("iterator error: %v", err)
		}
		if count != 10 {
			b.Fatalf("expected 10 records, got %d", count)
		}
		_ = it.Close()
	}
}

func BenchmarkTableIterator_SequentialScan_Medium(b *testing.B) {
	// Medium: ~10 blocks, 500 records
	reader, cleanup := prepareBenchmarkTable(b, 500, 512)
	defer cleanup()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := reader.NewIterator()
		if err != nil {
			b.Fatalf("failed to create iterator: %v", err)
		}
		count := 0
		for it.Next() {
			count++
		}
		if err := it.Err(); err != nil {
			b.Fatalf("iterator error: %v", err)
		}
		if count != 500 {
			b.Fatalf("expected 500 records, got %d", count)
		}
		_ = it.Close()
	}
}

func BenchmarkTableIterator_SequentialScan_Large(b *testing.B) {
	// Large: ~100 blocks, 5000 records
	reader, cleanup := prepareBenchmarkTable(b, 5000, 512)
	defer cleanup()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := reader.NewIterator()
		if err != nil {
			b.Fatalf("failed to create iterator: %v", err)
		}
		count := 0
		for it.Next() {
			count++
		}
		if err := it.Err(); err != nil {
			b.Fatalf("iterator error: %v", err)
		}
		if count != 5000 {
			b.Fatalf("expected 5000 records, got %d", count)
		}
		_ = it.Close()
	}
}

func BenchmarkTableIterator_Next_PerRecord(b *testing.B) {
	const count = 5000
	reader, cleanup := prepareBenchmarkTable(b, count, 512)
	defer cleanup()

	it, err := reader.NewIterator()
	if err != nil {
		b.Fatalf("failed to create iterator: %v", err)
	}
	defer func() { _ = it.Close() }()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !it.Next() {
			// Rewind
			if err := it.SeekToFirst(); err != nil {
				b.Fatalf("SeekToFirst failed: %v", err)
			}
		}
		_ = it.Key()
		_ = it.Value()
	}
}
