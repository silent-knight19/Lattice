package version

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
)

func BenchmarkManifestWriter_LogEdit_Small_Sync(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "MANIFEST-000001")

	w, err := CreateManifestWriter(path)
	if err != nil {
		b.Fatalf("CreateManifestWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	edit := NewVersionEdit()
	edit.SetNextFileNum(100)
	edit.SetLastSeqNum(200)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if err := w.LogEdit(*edit); err != nil {
			b.Fatalf("LogEdit failed: %v", err)
		}
	}
}

func BenchmarkManifestWriter_LogEdit_Small_NoSync(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "MANIFEST-000001")

	w, err := CreateManifestWriter(path)
	if err != nil {
		b.Fatalf("CreateManifestWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	// Bypass actual fdatasync barrier to measure CPU framing and OS buffer write latency
	w.SetSyncFnForTesting(func(f *os.File) error {
		return nil
	})

	edit := NewVersionEdit()
	edit.SetNextFileNum(100)
	edit.SetLastSeqNum(200)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if err := w.LogEdit(*edit); err != nil {
			b.Fatalf("LogEdit failed: %v", err)
		}
	}
}

func BenchmarkManifestWriter_LogEdit_Complex_Sync(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "MANIFEST-000001")

	w, err := CreateManifestWriter(path)
	if err != nil {
		b.Fatalf("CreateManifestWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	edit := NewVersionEdit()
	edit.SetNextFileNum(1000)
	edit.SetLastSeqNum(50000)
	for lvl := uint32(0); lvl < NumLevels; lvl++ {
		_ = edit.DeleteFile(lvl, uint64(lvl+1))
		_ = edit.AddFile(lvl, FileMetadata{
			FileNum:        uint64(lvl + 100),
			FileSize:       1024 * 1024,
			SmallestKey:    makeTestIK(fmt.Sprintf("bench_key_start_%d", lvl), 1000, binary.OpTypePut),
			LargestKey:     makeTestIK(fmt.Sprintf("bench_key_end___%d", lvl), 2000, binary.OpTypePut),
			SmallestSeqNum: 1000,
			LargestSeqNum:  2000,
		})
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if err := w.LogEdit(*edit); err != nil {
			b.Fatalf("LogEdit failed: %v", err)
		}
	}
}

func BenchmarkManifestWriter_LogEdit_Complex_NoSync(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "MANIFEST-000001")

	w, err := CreateManifestWriter(path)
	if err != nil {
		b.Fatalf("CreateManifestWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	// Bypass actual fdatasync barrier to isolate CPU serialization + framing cost
	w.SetSyncFnForTesting(func(f *os.File) error {
		return nil
	})

	edit := NewVersionEdit()
	edit.SetNextFileNum(1000)
	edit.SetLastSeqNum(50000)
	for lvl := uint32(0); lvl < NumLevels; lvl++ {
		_ = edit.DeleteFile(lvl, uint64(lvl+1))
		_ = edit.AddFile(lvl, FileMetadata{
			FileNum:        uint64(lvl + 100),
			FileSize:       1024 * 1024,
			SmallestKey:    makeTestIK(fmt.Sprintf("bench_key_start_%d", lvl), 1000, binary.OpTypePut),
			LargestKey:     makeTestIK(fmt.Sprintf("bench_key_end___%d", lvl), 2000, binary.OpTypePut),
			SmallestSeqNum: 1000,
			LargestSeqNum:  2000,
		})
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if err := w.LogEdit(*edit); err != nil {
			b.Fatalf("LogEdit failed: %v", err)
		}
	}
}
