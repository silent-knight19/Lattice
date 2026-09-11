package sstable_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/sstable"
)

func FuzzSearchDataBlock(f *testing.F) {
	// Seed 1: Truncated buffer
	f.Add([]byte{0x01, 0x02, 0x03}, []byte("key"))

	// Seed 2: Empty block
	f.Add([]byte{}, []byte("key"))

	// Seed 3: Minimal valid data block structure
	validBlock := func() []byte {
		builder := sstable.NewBlockBuilder()
		_ = builder.Add(binary.InternalKey{
			UserKey: []byte("seed_key"),
			SeqNum:  1,
			OpType:  binary.OpTypePut,
		}, []byte("seed_val"))
		return builder.Finish()
	}()
	f.Add(validBlock, []byte("seed_key"))
	f.Add(validBlock, []byte("nonexistent"))

	f.Fuzz(func(t *testing.T, blockData []byte, targetKey []byte) {
		// Cap input length to prevent excessive memory consumption
		if len(blockData) > 65536 || len(targetKey) > 1024 {
			return
		}

		// Must survive arbitrary bytes without panicking
		_, _ = sstable.SearchDataBlockForTesting(blockData, targetKey, 0)
	})
}

func FuzzTableReader_Seek(f *testing.F) {
	tempDir := f.TempDir()
	path := filepath.Join(tempDir, "fuzz_seek.sst")

	writer, err := sstable.NewTableWriter(path, sstable.DefaultTableWriterOptions())
	if err != nil {
		f.Fatalf("failed to create writer: %v", err)
	}

	keys := []string{"apple", "banana", "cherry", "date", "elderberry"}
	for i, k := range keys {
		_ = writer.Add(binary.InternalKey{
			UserKey: []byte(k),
			SeqNum:  binary.SeqNum(i + 1),
			OpType:  binary.OpTypePut,
		}, []byte("val_"+k))
	}
	_, err = writer.Finish()
	if err != nil {
		f.Fatalf("failed to finish writer: %v", err)
	}

	reader, err := sstable.NewTableReader(path)
	if err != nil {
		f.Fatalf("failed to open reader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	// Seed targets
	f.Add([]byte("apple"))
	f.Add([]byte("banana"))
	f.Add([]byte("cherry"))
	f.Add([]byte("nonexistent"))
	f.Add([]byte(""))
	f.Add([]byte("a"))
	f.Add([]byte("zzzzzzzzzzzzzz"))

	f.Fuzz(func(t *testing.T, targetKey []byte) {
		if len(targetKey) > 65536 {
			return
		}
		// Must never panic
		_, _ = reader.Seek(targetKey)
	})
}

func FuzzTableReader_CorruptedFile(f *testing.F) {
	tempDir := f.TempDir()
	path := filepath.Join(tempDir, "fuzz_corrupt.sst")

	writer, err := sstable.NewTableWriter(path, sstable.DefaultTableWriterOptions())
	if err != nil {
		f.Fatalf("failed to create writer: %v", err)
	}

	for i := 0; i < 5; i++ {
		_ = writer.Add(binary.InternalKey{
			UserKey: []byte{byte('a' + i)},
			SeqNum:  binary.SeqNum(i + 1),
			OpType:  binary.OpTypePut,
		}, []byte{byte('v' + i)})
	}
	_, err = writer.Finish()
	if err != nil {
		f.Fatalf("failed to finish writer: %v", err)
	}

	rawBytes, err := os.ReadFile(path)
	if err != nil {
		f.Fatalf("failed to read baseline: %v", err)
	}

	f.Add(rawBytes)
	f.Add([]byte{})
	f.Add(rawBytes[:len(rawBytes)/2])

	f.Fuzz(func(t *testing.T, mutated []byte) {
		// Cap input length to prevent excessive disk/memory usage
		if len(mutated) > 100000 {
			return
		}

		targetFile := filepath.Join(t.TempDir(), "corrupted.sst")
		if err := os.WriteFile(targetFile, mutated, 0644); err != nil {
			return
		}

		reader, err := sstable.NewTableReader(targetFile)
		if err != nil {
			// Expected failure on malformed file
			return
		}
		defer func() { _ = reader.Close() }()

		// If it opened, seeking should survive safely
		_, _ = reader.Seek([]byte("a"))
	})
}
