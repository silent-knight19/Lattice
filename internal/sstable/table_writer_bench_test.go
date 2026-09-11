package sstable_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// BenchmarkTableWriter_Sequential_1K measures creating, streaming 1,000 records, and finalizing
// a complete persistent SSTable file on storage.
func BenchmarkTableWriter_Sequential_1K(b *testing.B) {
	dir := b.TempDir()
	opts := sstable.DefaultTableWriterOptions()

	// Pre-build test records to exclude key formatting from measured loop
	const count = 1000
	keys := make([]binary.InternalKey, count)
	vals := make([][]byte, count)
	for i := 0; i < count; i++ {
		keys[i] = makeTestIK(fmt.Sprintf("user:%08d:profile", i), uint64(count-i), binary.OpTypePut)
		vals[i] = []byte(fmt.Sprintf("payload-data-value-string-%08d", i))
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		sstPath := filepath.Join(dir, fmt.Sprintf("bench_1k_%d.sst", i))
		w, err := sstable.NewTableWriter(sstPath, opts)
		if err != nil {
			b.Fatalf("NewTableWriter failed: %v", err)
		}

		for j := 0; j < count; j++ {
			if err := w.Add(keys[j], vals[j]); err != nil {
				b.Fatalf("Add failed: %v", err)
			}
		}

		if _, err := w.Finish(); err != nil {
			b.Fatalf("Finish failed: %v", err)
		}
		_ = os.Remove(sstPath)
	}
}

// BenchmarkTableWriter_Sequential_10K measures building a ~1MB SSTable with 10,000 records.
func BenchmarkTableWriter_Sequential_10K(b *testing.B) {
	dir := b.TempDir()
	opts := sstable.DefaultTableWriterOptions()

	const count = 10000
	keys := make([]binary.InternalKey, count)
	vals := make([][]byte, count)
	for i := 0; i < count; i++ {
		keys[i] = makeTestIK(fmt.Sprintf("user:%08d:profile", i), uint64(count-i), binary.OpTypePut)
		vals[i] = []byte(fmt.Sprintf("payload-data-value-string-%08d", i))
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		sstPath := filepath.Join(dir, fmt.Sprintf("bench_10k_%d.sst", i))
		w, err := sstable.NewTableWriter(sstPath, opts)
		if err != nil {
			b.Fatalf("NewTableWriter failed: %v", err)
		}

		for j := 0; j < count; j++ {
			if err := w.Add(keys[j], vals[j]); err != nil {
				b.Fatalf("Add failed: %v", err)
			}
		}

		if _, err := w.Finish(); err != nil {
			b.Fatalf("Finish failed: %v", err)
		}
		_ = os.Remove(sstPath)
	}
}

// BenchmarkTableWriter_Add_DirectFile measures raw ingestion rate without file rename overhead.
func BenchmarkTableWriter_Add_DirectFile(b *testing.B) {
	file, err := os.CreateTemp(b.TempDir(), "direct_bench_*.sst")
	if err != nil {
		b.Fatalf("CreateTemp failed: %v", err)
	}
	defer func() { _ = file.Close() }()
	defer func() { _ = os.Remove(file.Name()) }()

	opts := sstable.DefaultTableWriterOptions()
	w, err := sstable.NewTableWriterWithFile(file, opts)
	if err != nil {
		b.Fatalf("NewTableWriterWithFile failed: %v", err)
	}

	key := makeTestIK("benchmark-key-constant", 1, binary.OpTypePut)
	val := []byte("benchmark-value-payload-64-bytes-long-for-realistic-storage-density")

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		key.SeqNum = binary.SeqNum(uint64(b.N - i))
		_ = w.Add(key, val)
	}
}
