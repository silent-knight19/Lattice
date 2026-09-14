package compaction

import (
	"fmt"
	"os"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/sstable"
	"github.com/silent-knight19/lattice/internal/version"
)

func buildBenchmarkVersion(filesPerLevel int) *version.Version {
	var levels [version.NumLevels][]version.FileMetadata
	var fileNum uint64 = 1

	for lvl := 0; lvl < version.NumLevels; lvl++ {
		for f := 0; f < filesPerLevel; f++ {
			fileNum++
			minK := fmt.Sprintf("k-%02d-%06d", lvl, f*10)
			maxK := fmt.Sprintf("k-%02d-%06d", lvl, f*10+9)
			levels[lvl] = append(levels[lvl], makeTestFileMeta(
				fileNum,
				minK,
				maxK,
				uint64(f*10+1),
				uint64(f*10+9),
			))
		}
	}

	return version.NewVersion(levels)
}

func BenchmarkCompactor_SmallVersion(b *testing.B) {
	// 5 files per level = 35 total files
	v := buildBenchmarkVersion(5)
	defer v.Unref()

	c, err := NewCompactor(v)
	if err != nil {
		b.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	key := []byte("k-01-000025")

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = c.CanDropTombstone(key, 1)
	}
}

func BenchmarkCompactor_MediumVersion(b *testing.B) {
	// 50 files per level = 350 total files
	v := buildBenchmarkVersion(50)
	defer v.Unref()

	c, err := NewCompactor(v)
	if err != nil {
		b.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	key := []byte("k-03-000250")

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = c.CanDropTombstone(key, 1)
	}
}

func BenchmarkCompactor_LargeVersion(b *testing.B) {
	// 200 files per level = 1,400 total files
	v := buildBenchmarkVersion(200)
	defer v.Unref()

	c, err := NewCompactor(v)
	if err != nil {
		b.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	key := []byte("k-04-001500")

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = c.CanDropTombstone(key, 1)
	}
}

func BenchmarkCompactor_BottomLevelTarget(b *testing.B) {
	v := buildBenchmarkVersion(50)
	defer v.Unref()

	c, err := NewCompactor(v)
	if err != nil {
		b.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	key := []byte("k-06-000250")

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = c.CanDropTombstone(key, version.NumLevels-1)
	}
}

func BenchmarkCompactor_ExactLookup_CandidateFound(b *testing.B) {
	dir := b.TempDir()

	recs := []testRecord{
		{key: makeKey("apple", 100, binary.OpTypePut), value: []byte("val1")},
		{key: makeKey("zebra", 100, binary.OpTypePut), value: []byte("val2")},
	}
	reader, _ := buildTestSSTableFile(&testing.T{}, dir, 1001, recs)
	defer func() { _ = reader.Close() }()

	var levels [version.NumLevels][]version.FileMetadata
	levels[2] = []version.FileMetadata{
		makeTestFileMeta(1001, "apple", "zebra", 100, 100),
	}
	v := version.NewVersion(levels)
	defer v.Unref()

	opener := func(fileNum uint64) (*sstable.TableReader, error) {
		if fileNum == 1001 {
			return reader, nil
		}
		return nil, os.ErrNotExist
	}

	c, err := NewCompactor(v, WithTableOpener(opener))
	if err != nil {
		b.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	key := []byte("apple")

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = c.CanDropTombstone(key, 1)
	}
}

func BenchmarkCompactor_ExactLookup_CandidateNotFound(b *testing.B) {
	dir := b.TempDir()

	recs := []testRecord{
		{key: makeKey("apple", 100, binary.OpTypePut), value: []byte("val1")},
		{key: makeKey("zebra", 100, binary.OpTypePut), value: []byte("val2")},
	}
	reader, _ := buildTestSSTableFile(&testing.T{}, dir, 1001, recs)
	defer func() { _ = reader.Close() }()

	var levels [version.NumLevels][]version.FileMetadata
	levels[2] = []version.FileMetadata{
		makeTestFileMeta(1001, "apple", "zebra", 100, 100),
	}
	v := version.NewVersion(levels)
	defer v.Unref()

	opener := func(fileNum uint64) (*sstable.TableReader, error) {
		if fileNum == 1001 {
			return reader, nil
		}
		return nil, os.ErrNotExist
	}

	c, err := NewCompactor(v, WithTableOpener(opener))
	if err != nil {
		b.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	// "mango" falls in range ["apple", "zebra"] but physical seek proves it does not exist
	key := []byte("mango")

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = c.CanDropTombstone(key, 1)
	}
}
