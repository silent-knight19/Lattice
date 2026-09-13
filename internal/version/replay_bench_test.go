package version

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
)

func BenchmarkReplayManifest_100Edits(b *testing.B) {
	dir := b.TempDir()

	const numEdits = 100
	edits := make([]*VersionEdit, numEdits)
	var fileCounter uint64 = 1

	for i := 0; i < numEdits; i++ {
		e := NewVersionEdit()
		e.SetNextFileNum(fileCounter + 10)
		e.SetLastSeqNum(binary.SeqNum(i*10 + 10))

		fileNum := fileCounter
		fileCounter++
		sk := fmt.Sprintf("bench:key:%06d:min", i)
		lk := fmt.Sprintf("bench:key:%06d:max", i)

		_ = e.AddFile(0, FileMetadata{
			FileNum:        fileNum,
			FileSize:       2048,
			SmallestKey:    makeTestIK(sk, uint64(i*10), binary.OpTypePut),
			LargestKey:     makeTestIK(lk, uint64(i*10+5), binary.OpTypePut),
			SmallestSeqNum: uint64(i * 10),
			LargestSeqNum:  uint64(i*10 + 5),
		})
		edits[i] = e

		// Create dummy SSTable file
		p := TablePath(dir, fileNum)
		_ = os.WriteFile(p, []byte("sstable-dummy-content"), 0600)
	}

	manifestPath := ManifestPath(dir, 1)
	w, err := CreateManifestWriter(manifestPath)
	if err != nil {
		b.Fatalf("CreateManifestWriter failed: %v", err)
	}
	for _, edit := range edits {
		_ = w.LogEditPtr(edit)
	}
	_ = w.Close()
	_ = SetCurrentManifest(dir, 1)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		disc, err := DiscoverActiveManifest(dir)
		if err != nil {
			b.Fatalf("DiscoverActiveManifest failed: %v", err)
		}
		res, err := ReplayManifest(disc)
		if err != nil {
			_ = disc.Close()
			b.Fatalf("ReplayManifest failed: %v", err)
		}
		res.Version.Unref()
		_ = disc.Close()
	}
}

func BenchmarkReplayManifest_1000Edits(b *testing.B) {
	dir := b.TempDir()

	const numEdits = 1000
	edits := make([]*VersionEdit, numEdits)
	var fileCounter uint64 = 1

	for i := 0; i < numEdits; i++ {
		e := NewVersionEdit()
		e.SetNextFileNum(fileCounter + 10)
		e.SetLastSeqNum(binary.SeqNum(i*10 + 10))

		fileNum := fileCounter
		fileCounter++
		sk := fmt.Sprintf("bench:key:%06d:min", i)
		lk := fmt.Sprintf("bench:key:%06d:max", i)

		_ = e.AddFile(0, FileMetadata{
			FileNum:        fileNum,
			FileSize:       2048,
			SmallestKey:    makeTestIK(sk, uint64(i*10), binary.OpTypePut),
			LargestKey:     makeTestIK(lk, uint64(i*10+5), binary.OpTypePut),
			SmallestSeqNum: uint64(i * 10),
			LargestSeqNum:  uint64(i*10 + 5),
		})
		edits[i] = e

		// Create dummy SSTable file
		p := filepath.Join(dir, fmt.Sprintf("%06d.sst", fileNum))
		_ = os.WriteFile(p, []byte("sstable-dummy-content"), 0600)
	}

	manifestPath := ManifestPath(dir, 1)
	w, err := CreateManifestWriter(manifestPath)
	if err != nil {
		b.Fatalf("CreateManifestWriter failed: %v", err)
	}
	for _, edit := range edits {
		_ = w.LogEditPtr(edit)
	}
	_ = w.Close()
	_ = SetCurrentManifest(dir, 1)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		disc, err := DiscoverActiveManifest(dir)
		if err != nil {
			b.Fatalf("DiscoverActiveManifest failed: %v", err)
		}
		res, err := ReplayManifest(disc)
		if err != nil {
			_ = disc.Close()
			b.Fatalf("ReplayManifest failed: %v", err)
		}
		res.Version.Unref()
		_ = disc.Close()
	}
}
