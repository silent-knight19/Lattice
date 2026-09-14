package version

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
)

// BenchmarkLogAndApply_FullCommit benchmarks the full end-to-end LogAndApply pipeline
// with 4 deleted files, 4 added files, scalar updates, CRC32 framing, and hardware durability sync.
func BenchmarkLogAndApply_FullCommit(b *testing.B) {
	dir := b.TempDir()
	manPath := filepath.Join(dir, ManifestFilename(1))
	w, err := CreateManifestWriter(manPath)
	if err != nil {
		b.Fatalf("failed to create manifest writer: %v", err)
	}
	defer func() { _ = w.Close() }()

	vs := NewVersionSetWithOptions(VersionSetOptions{
		DBPath:         dir,
		ManifestWriter: w,
		NextFileNum:    1,
		LastSeqNum:     0,
	})

	// Setup initial 4 files at L0
	eInit := NewVersionEdit()
	eInit.SetNextFileNum(5)
	eInit.SetLastSeqNum(40)
	for i := uint64(1); i <= 4; i++ {
		p := TablePath(dir, i)
		_ = os.WriteFile(p, make([]byte, 1024), 0600)
		_ = eInit.AddFile(0, makeTestFileMeta(i, 1024, fmt.Sprintf("k%02d", i), fmt.Sprintf("k%02d", i), i*10-9, i*10))
	}
	if err := vs.LogAndApply(eInit); err != nil {
		b.Fatalf("initial edit failed: %v", err)
	}

	// Prepare physical SSTables for the benchmark iterations
	var currentFileNum uint64 = 5
	var currentSeq uint64 = 50

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		edit := NewVersionEdit()
		edit.SetNextFileNum(currentFileNum + 4)
		edit.SetLastSeqNum(binary.SeqNum(currentSeq + 40))

		// Delete 4 old files from L0 (or L1) and add 4 new files to L1
		// For benchmark repeatability, we add and delete from L0
		for j := uint64(0); j < 4; j++ {
			fNum := currentFileNum + j
			p := TablePath(dir, fNum)
			_ = os.WriteFile(p, make([]byte, 1024), 0600)
			_ = edit.AddFile(0, makeTestFileMeta(fNum, 1024, fmt.Sprintf("b_%d_%d", i, j), fmt.Sprintf("b_%d_%d", i, j), currentSeq+j*10+1, currentSeq+j*10+10))
			_ = edit.DeleteFile(0, fNum-4)
		}
		currentFileNum += 4
		currentSeq += 40
		b.StartTimer()

		if err := vs.LogAndApply(edit); err != nil {
			b.Fatalf("LogAndApply failed at iteration %d: %v", i, err)
		}
	}
}

// BenchmarkLogAndApply_DeriveVersion benchmarks the in-memory applyEditToVersion transition
// isolated from disk I/O.
func BenchmarkLogAndApply_DeriveVersion(b *testing.B) {
	// Base version with 10 files in L0 and 50 files in L1
	var levels [NumLevels][]FileMetadata
	for i := uint64(1); i <= 10; i++ {
		levels[0] = append(levels[0], makeTestFileMeta(i, 1024, fmt.Sprintf("k0_%03d", i), fmt.Sprintf("k0_%03d", i), i, i))
	}
	for i := uint64(11); i <= 60; i++ {
		levels[1] = append(levels[1], makeTestFileMeta(i, 1024, fmt.Sprintf("k1_%03d", i), fmt.Sprintf("k1_%03d", i), i, i))
	}
	base := NewVersion(levels)
	defer base.Unref()

	// 4 deleted files, 4 added files
	edit := NewVersionEdit()
	edit.SetNextFileNum(100)
	edit.SetLastSeqNum(500)
	_ = edit.DeleteFile(0, 1)
	_ = edit.DeleteFile(0, 2)
	_ = edit.DeleteFile(0, 3)
	_ = edit.DeleteFile(0, 4)
	_ = edit.AddFile(1, makeTestFileMeta(61, 1024, "k1_000_a", "k1_000_b", 401, 410))
	_ = edit.AddFile(1, makeTestFileMeta(62, 1024, "k1_000_c", "k1_000_d", 411, 420))
	_ = edit.AddFile(1, makeTestFileMeta(63, 1024, "k1_999_a", "k1_999_b", 421, 430))
	_ = edit.AddFile(1, makeTestFileMeta(64, 1024, "k1_999_c", "k1_999_d", 431, 440))

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		v, _, _, err := applyEditToVersion(base, edit, 70, 400)
		if err != nil {
			b.Fatalf("applyEditToVersion failed: %v", err)
		}
		v.Unref()
	}
}

// BenchmarkLogAndApply_ManifestWriteOnly benchmarks the pure ManifestWriter append & sync overhead.
func BenchmarkLogAndApply_ManifestWriteOnly(b *testing.B) {
	dir := b.TempDir()
	manPath := filepath.Join(dir, ManifestFilename(1))
	w, err := CreateManifestWriter(manPath)
	if err != nil {
		b.Fatalf("failed to create manifest writer: %v", err)
	}
	defer func() { _ = w.Close() }()

	edit := NewVersionEdit()
	edit.SetNextFileNum(100)
	edit.SetLastSeqNum(500)
	_ = edit.DeleteFile(0, 1)
	_ = edit.DeleteFile(0, 2)
	_ = edit.DeleteFile(0, 3)
	_ = edit.DeleteFile(0, 4)
	_ = edit.AddFile(1, makeTestFileMeta(61, 1024, "k1_000_a", "k1_000_b", 401, 410))
	_ = edit.AddFile(1, makeTestFileMeta(62, 1024, "k1_000_c", "k1_000_d", 411, 420))
	_ = edit.AddFile(1, makeTestFileMeta(63, 1024, "k1_999_a", "k1_999_b", 421, 430))
	_ = edit.AddFile(1, makeTestFileMeta(64, 1024, "k1_999_c", "k1_999_d", 431, 440))

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if err := w.LogEditPtr(edit); err != nil {
			b.Fatalf("LogEditPtr failed: %v", err)
		}
	}
}
