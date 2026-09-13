package version

import (
	"os"
	"sync/atomic"
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

func BenchmarkParseCurrentManifest(b *testing.B) {
	data := []byte("MANIFEST-000042\n")

	b.ResetTimer()
	b.ReportAllocs()

	var total uint64
	for i := 0; i < b.N; i++ {
		num, err := ParseCurrentManifest(data)
		if err != nil {
			b.Fatalf("ParseCurrentManifest failed: %v", err)
		}
		total += num
	}
	_ = total
}

func BenchmarkReadCurrentManifest(b *testing.B) {
	dir := b.TempDir()
	if err := SetCurrentManifest(dir, 42); err != nil {
		b.Fatalf("SetCurrentManifest failed: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()

	var total uint64
	for i := 0; i < b.N; i++ {
		num, err := ReadCurrentManifest(dir)
		if err != nil {
			b.Fatalf("ReadCurrentManifest failed: %v", err)
		}
		total += num
	}
	_ = total
}

func BenchmarkSetCurrentManifest_ConcurrentWriters(b *testing.B) {
	dir := b.TempDir()

	// Bypass actual hardware sync to measure lock contention, framing, and rename throughput
	restoreSync := SetCurrentSyncFnForTesting(func(f *os.File) error { return nil })
	defer restoreSync()
	restoreDirSync := SetCurrentSyncDirFnForTesting(func(dirPath string) error { return nil })
	defer restoreDirSync()

	b.ResetTimer()
	b.ReportAllocs()

	var counter uint64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			manifestNum := atomic.AddUint64(&counter, 1)
			if err := SetCurrentManifest(dir, manifestNum); err != nil {
				b.Fatalf("SetCurrentManifest failed: %v", err)
			}
		}
	})
}
