package memtable_test

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/memtable"
)

func populateSkipList(b *testing.B, n int) *memtable.SkipList {
	b.Helper()
	sl := memtable.NewSkipList()
	val := []byte("benchmark-test-payload-32-bytes")
	for i := 0; i < n; i++ {
		keyBytes := []byte(fmt.Sprintf("user_key_%08d", i))
		k, err := binary.NewInternalKey(keyBytes, binary.SeqNum(i+1), binary.OpTypePut)
		if err != nil {
			b.Fatalf("failed creating key: %v", err)
		}
		if err := sl.Insert(k, val); err != nil {
			b.Fatalf("failed inserting key: %v", err)
		}
	}
	return sl
}

func BenchmarkIterator_SequentialScan_1000Keys(b *testing.B) {
	sl := populateSkipList(b, 1000)
	it := sl.NewIterator()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		it.SeekToFirst()
		count := 0
		for it.Valid() {
			count++
			it.Next()
		}
		if count != 1000 {
			b.Fatalf("expected 1000 keys, scanned %d", count)
		}
	}
}

func BenchmarkIterator_SequentialScan_10000Keys(b *testing.B) {
	sl := populateSkipList(b, 10000)
	it := sl.NewIterator()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		it.SeekToFirst()
		count := 0
		for it.Valid() {
			count++
			it.Next()
		}
		if count != 10000 {
			b.Fatalf("expected 10000 keys, scanned %d", count)
		}
	}
}

func BenchmarkIterator_Seek_Random_1000Keys(b *testing.B) {
	sl := populateSkipList(b, 1000)
	rng := rand.New(rand.NewSource(42))
	targets := make([][]byte, 1000)
	for i := 0; i < 1000; i++ {
		idx := rng.Intn(1000)
		targets[i] = []byte(fmt.Sprintf("user_key_%08d", idx))
	}

	it := sl.NewIterator()
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		target := targets[i%1000]
		if err := it.Seek(target); err != nil {
			b.Fatalf("Seek failed: %v", err)
		}
		if !it.Valid() {
			b.Fatalf("Seek expected valid position for %s", target)
		}
	}
}

func BenchmarkIterator_Seek_Random_10000Keys(b *testing.B) {
	sl := populateSkipList(b, 10000)
	rng := rand.New(rand.NewSource(42))
	targets := make([][]byte, 1000)
	for i := 0; i < 1000; i++ {
		idx := rng.Intn(10000)
		targets[i] = []byte(fmt.Sprintf("user_key_%08d", idx))
	}

	it := sl.NewIterator()
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		target := targets[i%1000]
		if err := it.Seek(target); err != nil {
			b.Fatalf("Seek failed: %v", err)
		}
		if !it.Valid() {
			b.Fatalf("Seek expected valid position for %s", target)
		}
	}
}

func BenchmarkIterator_Next_PerStep(b *testing.B) {
	sl := populateSkipList(b, 100000)
	it := sl.NewIterator()
	it.SeekToFirst()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if !it.Next() {
			it.SeekToFirst()
		}
	}
}

func BenchmarkIterator_KeyAndValue_Cloning(b *testing.B) {
	sl := populateSkipList(b, 1000)
	it := sl.NewIterator()
	it.SeekToFirst()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		k := it.Key()
		v := it.Value()
		_ = k
		_ = v
	}
}
