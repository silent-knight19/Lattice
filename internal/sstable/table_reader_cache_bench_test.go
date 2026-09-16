package sstable_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/cache"
	"github.com/silent-knight19/lattice/internal/sstable"
)

func BenchmarkTableReader_ReadBlock(b *testing.B) {
	dir := b.TempDir()
	path, handles := helperBuildMultiBlockSSTableBench(b, dir, "000099.sst", 4, 10)

	blockCache, err := cache.NewShardedCache(64)
	if err != nil {
		b.Fatalf("failed to create cache: %v", err)
	}

	readerDirect, err := sstable.NewTableReaderWithOptions(path, sstable.TableReaderOptions{
		FileNum:    99,
		BlockCache: nil,
	})
	if err != nil {
		b.Fatalf("failed to open direct reader: %v", err)
	}
	defer func() { _ = readerDirect.Close() }()

	readerCached, err := sstable.NewTableReaderWithOptions(path, sstable.TableReaderOptions{
		FileNum:    99,
		BlockCache: blockCache,
	})
	if err != nil {
		b.Fatalf("failed to open cached reader: %v", err)
	}
	defer func() { _ = readerCached.Close() }()

	// Warm up cached reader
	for _, h := range handles {
		if _, err := readerCached.ReadBlock(h); err != nil {
			b.Fatalf("failed to warm block: %v", err)
		}
	}

	b.Run("Cold_DirectDisk", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			h := handles[i%len(handles)]
			if _, err := readerDirect.ReadBlock(h); err != nil {
				b.Fatalf("ReadBlock failed: %v", err)
			}
		}
	})

	b.Run("Warm_ShardedCache", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			h := handles[i%len(handles)]
			if _, err := readerCached.ReadBlock(h); err != nil {
				b.Fatalf("ReadBlock failed: %v", err)
			}
		}
	})
}

func BenchmarkTableReader_Seek(b *testing.B) {
	dir := b.TempDir()
	path, _ := helperBuildMultiBlockSSTableBench(b, dir, "000098.sst", 4, 10)

	blockCache, _ := cache.NewShardedCache(64)

	readerDirect, _ := sstable.NewTableReaderWithOptions(path, sstable.TableReaderOptions{
		FileNum:    98,
		BlockCache: nil,
	})
	defer func() { _ = readerDirect.Close() }()

	readerCached, _ := sstable.NewTableReaderWithOptions(path, sstable.TableReaderOptions{
		FileNum:    98,
		BlockCache: blockCache,
	})
	defer func() { _ = readerCached.Close() }()

	// Warm up
	for i := 0; i < 4; i++ {
		key := fmt.Sprintf("key-%03d-000", i)
		_, _ = readerCached.Seek([]byte(key))
	}

	b.Run("Seek_DirectDisk", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			key := fmt.Sprintf("key-%03d-%03d", i%4, (i*3)%10)
			if _, err := readerDirect.Seek([]byte(key)); err != nil {
				b.Fatalf("Seek failed: %v", err)
			}
		}
	})

	b.Run("Seek_ShardedCache", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			key := fmt.Sprintf("key-%03d-%03d", i%4, (i*3)%10)
			if _, err := readerCached.Seek([]byte(key)); err != nil {
				b.Fatalf("Seek failed: %v", err)
			}
		}
	})
}

func BenchmarkTableReader_Concurrent_64Workers(b *testing.B) {
	dir := b.TempDir()
	path, handles := helperBuildMultiBlockSSTableBench(b, dir, "000097.sst", 8, 10)

	blockCache, _ := cache.NewShardedCache(64)
	readerDirect, _ := sstable.NewTableReaderWithOptions(path, sstable.TableReaderOptions{
		FileNum:    97,
		BlockCache: nil,
	})
	defer func() { _ = readerDirect.Close() }()

	readerCached, _ := sstable.NewTableReaderWithOptions(path, sstable.TableReaderOptions{
		FileNum:    97,
		BlockCache: blockCache,
	})
	defer func() { _ = readerCached.Close() }()

	// Warm up
	for _, h := range handles {
		_, _ = readerCached.ReadBlock(h)
	}

	const workers = 64

	b.Run("Concurrent_DirectDisk", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()

		var wg sync.WaitGroup
		opsPerWorker := b.N / workers
		if opsPerWorker == 0 {
			opsPerWorker = 1
		}

		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()
				for i := 0; i < opsPerWorker; i++ {
					h := handles[(workerID+i)%len(handles)]
					_, _ = readerDirect.ReadBlock(h)
				}
			}(w)
		}
		wg.Wait()
	})

	b.Run("Concurrent_ShardedCache", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()

		var wg sync.WaitGroup
		opsPerWorker := b.N / workers
		if opsPerWorker == 0 {
			opsPerWorker = 1
		}

		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()
				for i := 0; i < opsPerWorker; i++ {
					h := handles[(workerID+i)%len(handles)]
					_, _ = readerCached.ReadBlock(h)
				}
			}(w)
		}
		wg.Wait()
	})
}

// helperBuildMultiBlockSSTableBench creates an SSTable for benchmarks.
func helperBuildMultiBlockSSTableBench(b *testing.B, dir string, filename string, numBlocks int, entriesPerBlock int) (string, []sstable.BlockHandle) {
	b.Helper()
	return helperBuildMultiBlockSSTable(b, dir, filename, numBlocks, entriesPerBlock)
}
