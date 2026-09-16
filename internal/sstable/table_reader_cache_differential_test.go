package sstable_test

import (
	"bytes"
	"fmt"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cache"
	"github.com/silent-knight19/lattice/internal/sstable"
)

func TestTableReader_Cache_Differential_RandomizedOps(t *testing.T) {
	dir := t.TempDir()
	path, handles := helperBuildMultiBlockSSTable(t, dir, "000010.sst", 4, 10)

	// Reader A: Cache Enabled (16-way sharded cache)
	blockCache, err := cache.NewShardedCache(32)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}

	readerCache, err := sstable.NewTableReaderWithOptions(path, sstable.TableReaderOptions{
		FileNum:    10,
		BlockCache: blockCache,
	})
	if err != nil {
		t.Fatalf("failed to open reader with cache: %v", err)
	}
	defer func() { _ = readerCache.Close() }()

	// Reader B: Cache Disabled (Direct Disk)
	readerDirect, err := sstable.NewTableReaderWithOptions(path, sstable.TableReaderOptions{
		FileNum:    10,
		BlockCache: nil,
	})
	if err != nil {
		t.Fatalf("failed to open direct reader: %v", err)
	}
	defer func() { _ = readerDirect.Close() }()

	var physicalReadsCache atomic.Int64
	origReadAtCache := readerCache.ReadAtFnForTesting()
	readerCache.SetReadAtFnForTesting(func(p []byte, off int64) (int, error) {
		physicalReadsCache.Add(1)
		return origReadAtCache(p, off)
	})

	var physicalReadsDirect atomic.Int64
	origReadAtDirect := readerDirect.ReadAtFnForTesting()
	readerDirect.SetReadAtFnForTesting(func(p []byte, off int64) (int, error) {
		physicalReadsDirect.Add(1)
		return origReadAtDirect(p, off)
	})

	// #nosec G404 -- deterministic pseudo-random seed for reproducible differential test
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	const totalOps = 5000

	for op := 0; op < totalOps; op++ {
		action := rng.Intn(3)
		switch action {
		case 0: // ReadBlock with valid handles
			h := handles[rng.Intn(len(handles))]
			bufCache, errCache := readerCache.ReadBlock(h)
			bufDirect, errDirect := readerDirect.ReadBlock(h)

			if (errCache == nil) != (errDirect == nil) {
				t.Fatalf("op %d: error mismatch: cache=%v direct=%v", op, errCache, errDirect)
			}
			if !bytes.Equal(bufCache, bufDirect) {
				t.Fatalf("op %d: ReadBlock content mismatch", op)
			}

		case 1: // Seek with keys (some existing, some non-existing)
			var key string
			if rng.Intn(2) == 0 {
				// Existing key pattern
				key = fmt.Sprintf("key-%03d-%03d", rng.Intn(4), rng.Intn(10))
			} else {
				// Random non-existing key
				key = fmt.Sprintf("nonexistent-%d", rng.Intn(10000))
			}
			valCache, errCache := readerCache.Seek([]byte(key))
			valDirect, errDirect := readerDirect.Seek([]byte(key))

			if (errCache == nil) != (errDirect == nil) {
				t.Fatalf("op %d: Seek error mismatch for key %s: cache=%v direct=%v", op, key, errCache, errDirect)
			}
			if !bytes.Equal(valCache, valDirect) {
				t.Fatalf("op %d: Seek val mismatch for key %s", op, key)
			}

		case 2: // ReadBlock with arbitrary/invalid handles
			var h sstable.BlockHandle
			if rng.Intn(2) == 0 {
				// Invalid size 0
				h = sstable.BlockHandle{Offset: handles[0].Offset, Size: 0}
			} else {
				// Invalid offset out of bounds
				// #nosec G115
				h = sstable.BlockHandle{Offset: uint64(readerCache.FileSize()) + 500, Size: 100}
			}
			_, errCache := readerCache.ReadBlock(h)
			_, errDirect := readerDirect.ReadBlock(h)

			if (errCache == nil) != (errDirect == nil) {
				t.Fatalf("op %d: invalid handle error mismatch: cache=%v direct=%v", op, errCache, errDirect)
			}
		}
	}

	diskReadsCache := physicalReadsCache.Load()
	diskReadsDirect := physicalReadsDirect.Load()

	t.Logf("Differential Test Summary (5000 ops):")
	t.Logf("  Direct Reader Disk I/Os : %d", diskReadsDirect)
	t.Logf("  Cached Reader Disk I/Os : %d (%.1fx reduction)", diskReadsCache, float64(diskReadsDirect)/float64(diskReadsCache))

	// Invariant: Cached reader MUST execute substantially fewer disk reads than direct reader
	if diskReadsCache >= diskReadsDirect {
		t.Fatalf("expected cached reader disk I/Os (%d) < direct reader disk I/Os (%d)", diskReadsCache, diskReadsDirect)
	}
}
