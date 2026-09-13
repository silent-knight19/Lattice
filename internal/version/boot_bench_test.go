package version

import (
	"testing"
)

func BenchmarkDiscoverActiveManifest_Populated(b *testing.B) {
	dir := b.TempDir()
	_ = helperCreatePopulatedManifest(&testing.T{}, dir, 1, 10)
	if err := SetCurrentManifest(dir, 1); err != nil {
		b.Fatalf("SetCurrentManifest: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		disc, err := DiscoverActiveManifest(dir)
		if err != nil {
			b.Fatalf("DiscoverActiveManifest: %v", err)
		}
		_ = disc.Close()
	}
}

func BenchmarkDiscoverActiveManifest_Empty(b *testing.B) {
	dir := b.TempDir()
	manifestPath := ManifestPath(dir, 1)
	_ = helperCreatePopulatedManifest(&testing.T{}, dir, 1, 0)
	// Create clean 0-byte file
	_ = SetCurrentManifest(dir, 1)
	_ = manifestPath

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		disc, err := DiscoverActiveManifest(dir)
		if err != nil {
			b.Fatalf("DiscoverActiveManifest: %v", err)
		}
		_ = disc.Close()
	}
}
