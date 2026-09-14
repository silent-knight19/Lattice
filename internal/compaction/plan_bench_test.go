package compaction

import (
	"fmt"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/version"
)

func makeBenchFile(fileNum uint64, size uint64, minUser, maxUser string) version.FileMetadata {
	ikSmall, _ := binary.NewInternalKey([]byte(minUser), 10, binary.OpTypePut)
	ikLarge, _ := binary.NewInternalKey([]byte(maxUser), 9, binary.OpTypePut)
	return version.FileMetadata{
		FileNum:        fileNum,
		FileSize:       size,
		SmallestKey:    binary.EncodeInternalKey(ikSmall),
		LargestKey:     binary.EncodeInternalKey(ikLarge),
		SmallestSeqNum: 9,
		LargestSeqNum:  10,
	}
}

func BenchmarkCompactionPlan_Small(b *testing.B) {
	policy := DefaultCompactionPolicy()
	planner, _ := NewPlanner(policy)

	var levels [version.NumLevels][]version.FileMetadata
	// L0: 4 files
	for i := 0; i < 4; i++ {
		levels[0] = append(levels[0], makeBenchFile(uint64(i+1), 1000, fmt.Sprintf("k%03d", i*10), fmt.Sprintf("k%03d", (i+1)*10)))
	}
	// L1: 6 non-overlapping files
	for i := 0; i < 6; i++ {
		levels[1] = append(levels[1], makeBenchFile(uint64(10+i), 5000, fmt.Sprintf("k%03d", i*15), fmt.Sprintf("k%03d", i*15+10)))
	}

	v := version.NewVersion(levels)
	defer v.Unref()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		plan, err := planner.PickCompaction(v)
		if err != nil || plan == nil {
			b.Fatalf("PickCompaction failed: %v", err)
		}
	}
}

func BenchmarkCompactionPlan_Medium(b *testing.B) {
	policy := DefaultCompactionPolicy()
	planner, _ := NewPlanner(policy)

	var levels [version.NumLevels][]version.FileMetadata
	// L0: 10 files
	for i := 0; i < 10; i++ {
		levels[0] = append(levels[0], makeBenchFile(uint64(i+1), 1000, fmt.Sprintf("k%04d", i*50), fmt.Sprintf("k%04d", (i+1)*50)))
	}
	// L1: 30 non-overlapping files
	for i := 0; i < 30; i++ {
		levels[1] = append(levels[1], makeBenchFile(uint64(100+i), 5000, fmt.Sprintf("k%04d", i*30), fmt.Sprintf("k%04d", i*30+20)))
	}
	// L2: 60 non-overlapping files
	for i := 0; i < 60; i++ {
		levels[2] = append(levels[2], makeBenchFile(uint64(200+i), 15000, fmt.Sprintf("k%04d", i*15), fmt.Sprintf("k%04d", i*15+10)))
	}

	v := version.NewVersion(levels)
	defer v.Unref()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		plan, err := planner.PickCompaction(v)
		if err != nil || plan == nil {
			b.Fatalf("PickCompaction failed: %v", err)
		}
	}
}

func BenchmarkCompactionPlan_Large(b *testing.B) {
	policy := DefaultCompactionPolicy()
	planner, _ := NewPlanner(policy)

	var levels [version.NumLevels][]version.FileMetadata
	// L0: 20 files
	for i := 0; i < 20; i++ {
		levels[0] = append(levels[0], makeBenchFile(uint64(i+1), 1000, fmt.Sprintf("k%05d", i*200), fmt.Sprintf("k%05d", (i+1)*200)))
	}
	// L1: 100 files
	for i := 0; i < 100; i++ {
		levels[1] = append(levels[1], makeBenchFile(uint64(100+i), 5000, fmt.Sprintf("k%05d", i*40), fmt.Sprintf("k%05d", i*40+30)))
	}
	// L2: 280 files
	for i := 0; i < 280; i++ {
		levels[2] = append(levels[2], makeBenchFile(uint64(500+i), 15000, fmt.Sprintf("k%05d", i*15), fmt.Sprintf("k%05d", i*15+10)))
	}
	// L3: 600 files
	for i := 0; i < 600; i++ {
		levels[3] = append(levels[3], makeBenchFile(uint64(1000+i), 50000, fmt.Sprintf("k%05d", i*7), fmt.Sprintf("k%05d", i*7+5)))
	}

	v := version.NewVersion(levels)
	defer v.Unref()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		plan, err := planner.PickCompaction(v)
		if err != nil || plan == nil {
			b.Fatalf("PickCompaction failed: %v", err)
		}
	}
}
