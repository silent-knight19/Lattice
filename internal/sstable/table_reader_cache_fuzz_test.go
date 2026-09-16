package sstable_test

import (
	"bytes"
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/cache"
	"github.com/silent-knight19/lattice/internal/sstable"
)

var (
	fuzzOnce   sync.Once
	fuzzPath   string
	fuzzCache  *cache.ShardedCache
	fuzzDirect *sstable.TableReader
	fuzzCached *sstable.TableReader
)

func initFuzzTable(t *testing.T) {
	t.Helper()
	fuzzOnce.Do(func() {
		dir := t.TempDir()
		p, _ := helperBuildMultiBlockSSTable(t, dir, "000042.sst", 4, 10)
		fuzzPath = p

		var err error
		fuzzCache, err = cache.NewShardedCache(64)
		if err != nil {
			t.Fatalf("failed to create cache: %v", err)
		}

		fuzzDirect, err = sstable.NewTableReaderWithOptions(fuzzPath, sstable.TableReaderOptions{
			FileNum:    42,
			BlockCache: nil,
		})
		if err != nil {
			t.Fatalf("failed to open direct reader: %v", err)
		}

		fuzzCached, err = sstable.NewTableReaderWithOptions(fuzzPath, sstable.TableReaderOptions{
			FileNum:    42,
			BlockCache: fuzzCache,
		})
		if err != nil {
			t.Fatalf("failed to open cached reader: %v", err)
		}
	})
}

func FuzzTableReader_ReadBlock(f *testing.F) {
	// Seed corpus with representative offsets and sizes
	f.Add(uint64(0), uint64(256), []byte("key-000-000"))
	f.Add(uint64(512), uint64(256), []byte("key-001-005"))
	f.Add(uint64(1024), uint64(256), []byte("key-002-008"))
	f.Add(uint64(0), uint64(0), []byte("nonexistent"))
	f.Add(uint64(99999), uint64(5000), []byte(""))
	f.Add(uint64(100), uint64(8*1024*1024+1), []byte("oversized"))

	f.Fuzz(func(t *testing.T, offset uint64, size uint64, queryKey []byte) {
		initFuzzTable(t)

		handle := sstable.BlockHandle{
			Offset: offset,
			Size:   size,
		}

		// 1. ReadBlock comparison
		bufCache, errCache := fuzzCached.ReadBlock(handle)
		bufDirect, errDirect := fuzzDirect.ReadBlock(handle)

		// Parity invariant: error presence must match
		if (errCache == nil) != (errDirect == nil) {
			t.Fatalf("ReadBlock error parity failure for handle %v: cache=%v direct=%v", handle, errCache, errDirect)
		}
		if errCache == nil {
			if !bytes.Equal(bufCache, bufDirect) {
				t.Fatalf("ReadBlock content mismatch for handle %v", handle)
			}
		}

		// 2. Seek comparison
		if len(queryKey) > 0 && len(queryKey) <= 65535 {
			valCache, sErrCache := fuzzCached.Seek(queryKey)
			valDirect, sErrDirect := fuzzDirect.Seek(queryKey)

			if (sErrCache == nil) != (sErrDirect == nil) {
				t.Fatalf("Seek error parity failure for key %s: cache=%v direct=%v", string(queryKey), sErrCache, sErrDirect)
			}
			if sErrCache == nil {
				if !bytes.Equal(valCache, valDirect) {
					t.Fatalf("Seek value mismatch for key %s", string(queryKey))
				}
			}
		}
	})
}
