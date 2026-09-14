package compaction

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
)

func generateSortedRecords(count int, keyPrefix string, startSeq uint64) []testRecord {
	recs := make([]testRecord, count)
	for i := 0; i < count; i++ {
		recs[i] = testRecord{
			key: makeKey(
				fmt.Sprintf("%s-key-%06d", keyPrefix, i),
				startSeq+uint64(i),
				binary.OpTypePut,
			),
			value: []byte(fmt.Sprintf("val-%d", i)),
		}
	}
	sort.Slice(recs, func(i, j int) bool {
		return binary.CompareInternalKey(recs[i].key, recs[j].key) < 0
	})
	return recs
}

func BenchmarkMergingIterator_4_Files_1000_Records(b *testing.B) {
	const numIters = 4
	const recsPerIter = 1000

	data := make([][]testRecord, numIters)
	for i := 0; i < numIters; i++ {
		data[i] = generateSortedRecords(recsPerIter, fmt.Sprintf("shard-%d", i%2), uint64(100+i))
	}

	b.ResetTimer()
	b.ReportAllocs()

	for n := 0; n < b.N; n++ {
		childIters := make([]Iterator, numIters)
		for i := 0; i < numIters; i++ {
			childIters[i] = newMockIterator(data[i])
		}

		it := NewMergingIterator(childIters)
		count := 0
		for it.Next() {
			count++
		}
		_ = it.Close()
		if count == 0 {
			b.Fatalf("expected non-zero records")
		}
	}
}

func BenchmarkMergingIterator_16_Files_10000_Records(b *testing.B) {
	const numIters = 16
	const recsPerIter = 10000

	data := make([][]testRecord, numIters)
	for i := 0; i < numIters; i++ {
		data[i] = generateSortedRecords(recsPerIter, fmt.Sprintf("shard-%d", i%4), uint64(100+i))
	}

	b.ResetTimer()
	b.ReportAllocs()

	for n := 0; n < b.N; n++ {
		childIters := make([]Iterator, numIters)
		for i := 0; i < numIters; i++ {
			childIters[i] = newMockIterator(data[i])
		}

		it := NewMergingIterator(childIters)
		count := 0
		for it.Next() {
			count++
		}
		_ = it.Close()
		if count == 0 {
			b.Fatalf("expected non-zero records")
		}
	}
}

func BenchmarkMergingIterator_32_Files_10000_Records(b *testing.B) {
	const numIters = 32
	const recsPerIter = 10000

	data := make([][]testRecord, numIters)
	for i := 0; i < numIters; i++ {
		data[i] = generateSortedRecords(recsPerIter, fmt.Sprintf("shard-%d", i%8), uint64(100+i))
	}

	b.ResetTimer()
	b.ReportAllocs()

	for n := 0; n < b.N; n++ {
		childIters := make([]Iterator, numIters)
		for i := 0; i < numIters; i++ {
			childIters[i] = newMockIterator(data[i])
		}

		it := NewMergingIterator(childIters)
		count := 0
		for it.Next() {
			count++
		}
		_ = it.Close()
		if count == 0 {
			b.Fatalf("expected non-zero records")
		}
	}
}

func BenchmarkMergingIterator_Next_PerRecord(b *testing.B) {
	const numIters = 8
	// #nosec G404 - Deterministic pseudo-random number generator for reproducible benchmarking
	rng := rand.New(rand.NewSource(99))

	data := make([][]testRecord, numIters)
	for i := 0; i < numIters; i++ {
		recs := make([]testRecord, b.N+100)
		for j := 0; j < b.N+100; j++ {
			recs[j] = testRecord{
				key: makeKey(
					fmt.Sprintf("key-%08d", j),
					// #nosec G115 - Non-negative integer fits within uint64
					uint64(rng.Intn(1000)+1),
					binary.OpTypePut,
				),
				value: []byte("val"),
			}
		}
		sort.Slice(recs, func(x, y int) bool {
			return binary.CompareInternalKey(recs[x].key, recs[y].key) < 0
		})
		data[i] = recs
	}

	childIters := make([]Iterator, numIters)
	for i := 0; i < numIters; i++ {
		childIters[i] = newMockIterator(data[i])
	}

	it := NewMergingIterator(childIters)
	defer func() { _ = it.Close() }()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if !it.Next() {
			break
		}
	}
}
