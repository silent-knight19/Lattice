package sstable_test

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/sstable"
)

func setupBenchmarkTable(b *testing.B, keyCount int) (*sstable.TableReader, [][]byte) {
	b.Helper()
	tempDir := b.TempDir()
	path := filepath.Join(tempDir, fmt.Sprintf("bench_%d.sst", keyCount))

	writer, err := sstable.NewTableWriter(path, sstable.DefaultTableWriterOptions())
	if err != nil {
		b.Fatalf("failed to create writer: %v", err)
	}

	keys := make([][]byte, keyCount)
	for i := 0; i < keyCount; i++ {
		keyBytes := []byte(fmt.Sprintf("bench_key_%08d", i))
		keys[i] = keyBytes
		valBytes := []byte(fmt.Sprintf("bench_val_%08d", i))

		if err := writer.Add(binary.InternalKey{
			UserKey: keyBytes,
			SeqNum:  binary.SeqNum(i + 1),
			OpType:  binary.OpTypePut,
		}, valBytes); err != nil {
			b.Fatalf("failed to add entry: %v", err)
		}
	}

	_, err = writer.Finish()
	if err != nil {
		b.Fatalf("failed to finish writer: %v", err)
	}

	reader, err := sstable.NewTableReader(path)
	if err != nil {
		b.Fatalf("failed to open reader: %v", err)
	}

	return reader, keys
}

func BenchmarkTableReader_Seek_HotBlock(b *testing.B) {
	reader, keys := setupBenchmarkTable(b, 100)
	defer func() { _ = reader.Close() }()

	targetKey := keys[50] // Hot middle key

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		val, err := reader.Seek(targetKey)
		if err != nil || len(val) == 0 {
			b.Fatalf("seek failed: %v", err)
		}
	}
}

func BenchmarkTableReader_Seek_Random(b *testing.B) {
	keyCount := 10000
	reader, keys := setupBenchmarkTable(b, keyCount)
	defer func() { _ = reader.Close() }()

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	indices := make([]int, b.N)
	for i := 0; i < b.N; i++ {
		indices[i] = rng.Intn(keyCount)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		targetKey := keys[indices[i]]
		val, err := reader.Seek(targetKey)
		if err != nil || len(val) == 0 {
			b.Fatalf("seek failed: %v", err)
		}
	}
}

func BenchmarkTableReader_Seek_Missing(b *testing.B) {
	reader, _ := setupBenchmarkTable(b, 10000)
	defer func() { _ = reader.Close() }()

	missingKey := []byte("bench_missing_key_99999999")

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := reader.Seek(missingKey)
		if err == nil {
			b.Fatalf("expected ErrKeyNotFound, got nil")
		}
	}
}
