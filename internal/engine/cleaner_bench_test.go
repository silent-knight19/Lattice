package engine_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/engine"
)

func benchmarkCleanOrphanedFiles(b *testing.B, totalEntries, orphanCount int) {
	b.Helper()

	// Pre-create template directory with totalEntries
	baseDir := b.TempDir()

	// Populate persistent / non-orphan entries
	nonOrphanCount := totalEntries - orphanCount
	for i := 0; i < nonOrphanCount; i++ {
		p := filepath.Join(baseDir, fmt.Sprintf("%06d.sst", i+1))
		if err := os.WriteFile(p, []byte("data"), 0600); err != nil { // #nosec G304
			b.Fatalf("failed to write base entry: %v", err)
		}
	}

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		b.StopTimer()
		// Prepare test directory with orphans
		dir := b.TempDir()
		// Link or copy non-orphans
		for i := 0; i < nonOrphanCount; i++ {
			name := fmt.Sprintf("%06d.sst", i+1)
			_ = os.Link(filepath.Join(baseDir, name), filepath.Join(dir, name))
		}
		// Create orphan files
		for i := 0; i < orphanCount; i++ {
			name := fmt.Sprintf(".tmp_%06d.sst_%06d", i+1, i+1)
			_ = os.WriteFile(filepath.Join(dir, name), []byte("orphan"), 0600) // #nosec G304
		}
		b.StartTimer()

		report, err := engine.CleanOrphanedFilesDir(dir)
		if err != nil {
			b.Fatalf("CleanOrphanedFilesDir failed: %v", err)
		}
		if report.FilesCleaned != orphanCount {
			b.Fatalf("expected %d cleaned, got %d", orphanCount, report.FilesCleaned)
		}
	}
}

func BenchmarkCleanOrphanedFiles_100Entries(b *testing.B) {
	benchmarkCleanOrphanedFiles(b, 100, 20)
}

func BenchmarkCleanOrphanedFiles_1000Entries(b *testing.B) {
	benchmarkCleanOrphanedFiles(b, 1000, 100)
}

func BenchmarkCleanOrphanedFiles_10000Entries(b *testing.B) {
	benchmarkCleanOrphanedFiles(b, 10000, 500)
}
