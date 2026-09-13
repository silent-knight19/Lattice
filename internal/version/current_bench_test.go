package version

import (
	"os"
	"testing"
)

func BenchmarkSetCurrentManifest_Sync(b *testing.B) {
	dir := b.TempDir()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		manifestNum := uint64(i%1000 + 1)
		if err := SetCurrentManifest(dir, manifestNum); err != nil {
			b.Fatalf("SetCurrentManifest failed: %v", err)
		}
	}
}

func BenchmarkSetCurrentManifest_NoSync(b *testing.B) {
	dir := b.TempDir()

	// Bypass actual hardware sync to measure CPU framing, serialization, and OS rename latency
	restoreSync := SetCurrentSyncFnForTesting(func(f *os.File) error {
		return nil
	})
	defer restoreSync()

	restoreDirSync := SetCurrentSyncDirFnForTesting(func(dirPath string) error {
		return nil
	})
	defer restoreDirSync()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		manifestNum := uint64(i%1000 + 1)
		if err := SetCurrentManifest(dir, manifestNum); err != nil {
			b.Fatalf("SetCurrentManifest failed: %v", err)
		}
	}
}
