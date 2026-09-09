package memtable_test

import (
	"fmt"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/memtable"
)

func populateForFreezeBench(b *testing.B, count int) *memtable.SkipList {
	b.Helper()
	sl := memtable.NewSkipList()
	val := []byte("benchmark-freeze-value-payload")
	for i := 0; i < count; i++ {
		k, err := binary.NewInternalKey([]byte(fmt.Sprintf("k%07d", i)), binary.SeqNum(i+1), binary.OpTypePut)
		if err != nil {
			b.Fatalf("failed creating key: %v", err)
		}
		if err := sl.Insert(k, val); err != nil {
			b.Fatalf("failed inserting key: %v", err)
		}
	}
	return sl
}

func BenchmarkSkipList_Freeze_1K(b *testing.B) {
	sl := populateForFreezeBench(b, 1000)
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		sl.Freeze()
		sl.UnfreezeForTesting()
	}
}

func BenchmarkSkipList_Freeze_10K(b *testing.B) {
	sl := populateForFreezeBench(b, 10000)
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		sl.Freeze()
		sl.UnfreezeForTesting()
	}
}

func BenchmarkSkipList_Freeze_100K(b *testing.B) {
	sl := populateForFreezeBench(b, 100000)
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		sl.Freeze()
		sl.UnfreezeForTesting()
	}
}

func BenchmarkSkipList_Freeze_IdempotentAlreadyFrozen(b *testing.B) {
	sl := populateForFreezeBench(b, 1000)
	sl.Freeze()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = sl.Freeze()
	}
}

func BenchmarkSkipList_PostFreeze_InsertRejected(b *testing.B) {
	sl := populateForFreezeBench(b, 1000)
	sl.Freeze()

	targetKey, _ := binary.NewInternalKey([]byte("rejected-key"), 9999, binary.OpTypePut)
	val := []byte("payload")

	b.ResetTimer()
	b.ReportAllocs()

	var lastErr error
	for i := 0; i < b.N; i++ {
		lastErr = sl.Insert(targetKey, val)
	}
	if lastErr == nil {
		b.Fatalf("expected insert error on frozen list")
	}
}
