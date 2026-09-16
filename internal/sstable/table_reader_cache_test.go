package sstable_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/cache"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// helperBuildMultiBlockSSTable creates an SSTable containing multiple data blocks.
func helperBuildMultiBlockSSTable(t testing.TB, dir string, filename string, numBlocks int, entriesPerBlock int) (string, []sstable.BlockHandle) {
	t.Helper()
	path := filepath.Join(dir, filename)

	opts := sstable.DefaultTableWriterOptions()
	opts.TargetBlockSize = 256 // small target to force frequent block boundaries

	writer, err := sstable.NewTableWriter(path, opts)
	if err != nil {
		t.Fatalf("failed to create TableWriter: %v", err)
	}

	for b := 0; b < numBlocks; b++ {
		for e := 0; e < entriesPerBlock; e++ {
			userKey := fmt.Sprintf("key-%03d-%03d", b, e)
			val := fmt.Sprintf("val-%03d-%03d-%s", b, e, bytes.Repeat([]byte("x"), 64))
			ik := binary.InternalKey{
				UserKey: []byte(userKey),
				SeqNum:  binary.SeqNum(b*1000 + e + 1),
				OpType:  binary.OpTypePut,
			}
			if err := writer.Add(ik, []byte(val)); err != nil {
				t.Fatalf("failed to add entry: %v", err)
			}
		}
	}

	meta, err := writer.Finish()
	if err != nil {
		t.Fatalf("failed to finalize TableWriter: %v", err)
	}
	// #nosec G115
	if int(meta.DataBlockCount) < numBlocks {
		t.Fatalf("expected at least %d data blocks, got %d", numBlocks, meta.DataBlockCount)
	}

	// Open reader to collect block handles from index
	reader, err := sstable.NewTableReader(path)
	if err != nil {
		t.Fatalf("failed to open TableReader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	indexEntries := reader.Index().Entries()
	handles := make([]sstable.BlockHandle, len(indexEntries))
	for i, entry := range indexEntries {
		handles[i] = entry.Handle
	}

	return path, handles
}

func TestTableReader_Cache_ColdAndWarmRead(t *testing.T) {
	dir := t.TempDir()
	path, handles := helperBuildMultiBlockSSTable(t, dir, "000001.sst", 3, 10)

	blockCache, err := cache.NewShardedCache(64)
	if err != nil {
		t.Fatalf("failed to create ShardedCache: %v", err)
	}

	reader, err := sstable.NewTableReaderWithOptions(path, sstable.TableReaderOptions{
		FileNum:    1,
		BlockCache: blockCache,
	})
	if err != nil {
		t.Fatalf("failed to open TableReader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	// Track physical reads using SetReadAtFnForTesting
	var physicalReads atomic.Int64
	origReadAt := reader.ReadAtFnForTesting()
	reader.SetReadAtFnForTesting(func(p []byte, off int64) (int, error) {
		physicalReads.Add(1)
		return origReadAt(p, off)
	})

	h0 := handles[0]
	h1 := handles[1]

	// 1. Cold read of block 0 (miss -> 1 physical disk read)
	buf0, err := reader.ReadBlock(h0)
	if err != nil {
		t.Fatalf("cold ReadBlock(h0) failed: %v", err)
	}
	// #nosec G115
	if len(buf0) != int(h0.Size) {
		t.Fatalf("expected length %d, got %d", h0.Size, len(buf0))
	}
	if physicalReads.Load() != 1 {
		t.Fatalf("expected physicalReads == 1, got %d", physicalReads.Load())
	}

	// 2. Warm read of block 0 (hit -> 0 additional physical disk reads!)
	buf0Warm, err := reader.ReadBlock(h0)
	if err != nil {
		t.Fatalf("warm ReadBlock(h0) failed: %v", err)
	}
	if !bytes.Equal(buf0, buf0Warm) {
		t.Fatalf("warm read content mismatch")
	}
	if physicalReads.Load() != 1 {
		t.Fatalf("expected physicalReads to remain 1 on cache hit, got %d", physicalReads.Load())
	}

	// 3. Cold read of block 1 (miss -> physicalReads == 2)
	buf1, err := reader.ReadBlock(h1)
	if err != nil {
		t.Fatalf("cold ReadBlock(h1) failed: %v", err)
	}
	if physicalReads.Load() != 2 {
		t.Fatalf("expected physicalReads == 2, got %d", physicalReads.Load())
	}

	// 4. Warm read of block 1 (hit -> physicalReads == 2)
	buf1Warm, err := reader.ReadBlock(h1)
	if err != nil {
		t.Fatalf("warm ReadBlock(h1) failed: %v", err)
	}
	if !bytes.Equal(buf1, buf1Warm) {
		t.Fatalf("warm read content mismatch for block 1")
	}
	if physicalReads.Load() != 2 {
		t.Fatalf("expected physicalReads to remain 2, got %d", physicalReads.Load())
	}

	// 5. Interleaved warm read of block 0 again (hit -> physicalReads == 2)
	buf0Again, err := reader.ReadBlock(h0)
	if err != nil {
		t.Fatalf("ReadBlock(h0) again failed: %v", err)
	}
	if !bytes.Equal(buf0, buf0Again) {
		t.Fatalf("interleaved read content mismatch")
	}
	if physicalReads.Load() != 2 {
		t.Fatalf("expected physicalReads to remain 2, got %d", physicalReads.Load())
	}
}

func TestTableReader_Cache_Disabled(t *testing.T) {
	dir := t.TempDir()
	path, handles := helperBuildMultiBlockSSTable(t, dir, "000002.sst", 2, 5)

	// Open reader with NO cache (nil BlockCache)
	reader, err := sstable.NewTableReaderWithOptions(path, sstable.TableReaderOptions{
		FileNum:    2,
		BlockCache: nil,
	})
	if err != nil {
		t.Fatalf("failed to open TableReader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	if reader.BlockCache() != nil {
		t.Fatalf("expected nil BlockCache")
	}

	var physicalReads atomic.Int64
	origReadAt := reader.ReadAtFnForTesting()
	reader.SetReadAtFnForTesting(func(p []byte, off int64) (int, error) {
		physicalReads.Add(1)
		return origReadAt(p, off)
	})

	// Reading without cache should hit disk on every read
	h0 := handles[0]
	_, err = reader.ReadBlock(h0)
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}
	if physicalReads.Load() != 1 {
		t.Fatalf("expected physicalReads == 1, got %d", physicalReads.Load())
	}

	_, err = reader.ReadBlock(h0)
	if err != nil {
		t.Fatalf("ReadBlock again failed: %v", err)
	}
	if physicalReads.Load() != 2 {
		t.Fatalf("expected physicalReads == 2 with cache disabled, got %d", physicalReads.Load())
	}
}

func TestTableReader_Cache_PoisonProof(t *testing.T) {
	dir := t.TempDir()
	path, handles := helperBuildMultiBlockSSTable(t, dir, "000003.sst", 2, 5)

	blockCache, _ := cache.NewShardedCache(16)
	reader, err := sstable.NewTableReaderWithOptions(path, sstable.TableReaderOptions{
		FileNum:    3,
		BlockCache: blockCache,
	})
	if err != nil {
		t.Fatalf("failed to open TableReader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	h0 := handles[0]
	h1 := handles[1]

	// 1. Read block 0 to warm cache
	val0, err := reader.ReadBlock(h0)
	if err != nil {
		t.Fatalf("initial ReadBlock failed: %v", err)
	}

	// 2. Poison the underlying reader so any physical disk read fails immediately
	poisonErr := stdErrors.New("disk hardware fault injection")
	reader.SetReadAtFnForTesting(func(p []byte, off int64) (int, error) {
		return 0, poisonErr
	})

	// 3. Warm read of block 0 must SUCCEED from cache without invoking poisoned disk reader
	val0Warm, err := reader.ReadBlock(h0)
	if err != nil {
		t.Fatalf("expected warm read to succeed from cache despite poisoned disk, got: %v", err)
	}
	if !bytes.Equal(val0, val0Warm) {
		t.Fatalf("content mismatch from cache")
	}

	// 4. Cold read of un-cached block 1 must FAIL with the injected disk error
	_, err = reader.ReadBlock(h1)
	if err == nil || !stdErrors.Is(err, poisonErr) {
		t.Fatalf("expected poisoned error %v, got %v", poisonErr, err)
	}
}

func TestTableReader_Cache_CorruptionNotCached(t *testing.T) {
	dir := t.TempDir()
	path, handles := helperBuildMultiBlockSSTable(t, dir, "000004.sst", 2, 5)

	blockCache, _ := cache.NewShardedCache(16)
	reader, err := sstable.NewTableReaderWithOptions(path, sstable.TableReaderOptions{
		FileNum:    4,
		BlockCache: blockCache,
	})
	if err != nil {
		t.Fatalf("failed to open TableReader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	h0 := handles[0]
	origReadAt := reader.ReadAtFnForTesting()

	// Inject corrupted CRC on block 0
	reader.SetReadAtFnForTesting(func(p []byte, off int64) (int, error) {
		n, err := origReadAt(p, off)
		// #nosec G115
		if off == int64(h0.Offset) && n > 4 {
			// Invert last byte of CRC trailer
			p[n-1] ^= 0xFF
		}
		return n, err
	})

	// ReadBlock must return ChecksumMismatchError
	_, err = reader.ReadBlock(h0)
	if err == nil {
		t.Fatalf("expected checksum error on corrupted block, got nil")
	}
	var cErr *errors.ChecksumMismatchError
	if !stdErrors.As(err, &cErr) {
		t.Fatalf("expected ChecksumMismatchError, got: %T (%v)", err, err)
	}

	// Verify corrupted block was NEVER inserted into the cache
	key := cache.NewBlockKey(4, h0.Offset)
	if blockCache.Contains(key) {
		t.Fatalf("corrupted block was improperly inserted into cache!")
	}
}

func TestTableReader_Cache_HandleBoundsValidationBeforeCache(t *testing.T) {
	dir := t.TempDir()
	path, handles := helperBuildMultiBlockSSTable(t, dir, "000005.sst", 2, 5)

	blockCache, _ := cache.NewShardedCache(16)
	reader, err := sstable.NewTableReaderWithOptions(path, sstable.TableReaderOptions{
		FileNum:    5,
		BlockCache: blockCache,
	})
	if err != nil {
		t.Fatalf("failed to open TableReader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	h0 := handles[0]

	// 1. Warm block 0
	_, err = reader.ReadBlock(h0)
	if err != nil {
		t.Fatalf("warm read failed: %v", err)
	}

	// 2. Query with invalid handle at the same offset but invalid size (e.g. Size = 0)
	invalidHandle := sstable.BlockHandle{
		Offset: h0.Offset,
		Size:   0, // invalid
	}
	_, err = reader.ReadBlock(invalidHandle)
	if err == nil {
		t.Fatalf("expected InvalidBlockHandleError for size 0")
	}
	var hErr *errors.InvalidBlockHandleError
	if !stdErrors.As(err, &hErr) {
		t.Fatalf("expected InvalidBlockHandleError, got: %v", err)
	}

	// 3. Query with handle extending beyond file boundary
	// #nosec G115
	overflowHandle := sstable.BlockHandle{
		Offset: h0.Offset,
		Size:   uint64(reader.FileSize()) + 1000,
	}
	_, err = reader.ReadBlock(overflowHandle)
	if err == nil {
		t.Fatalf("expected InvalidBlockHandleError for overflow size")
	}
}

func TestTableReader_Cache_CrossSSTableIsolation(t *testing.T) {
	dir := t.TempDir()

	// Build two SSTables with different data
	path1, handles1 := helperBuildMultiBlockSSTable(t, dir, "000001.sst", 1, 5)
	path2, handles2 := helperBuildMultiBlockSSTable(t, dir, "000002.sst", 1, 5)

	sharedCache, _ := cache.NewShardedCache(32)

	reader1, err := sstable.NewTableReaderWithOptions(path1, sstable.TableReaderOptions{
		FileNum:    1,
		BlockCache: sharedCache,
	})
	if err != nil {
		t.Fatalf("failed to open reader 1: %v", err)
	}
	defer func() { _ = reader1.Close() }()

	reader2, err := sstable.NewTableReaderWithOptions(path2, sstable.TableReaderOptions{
		FileNum:    2,
		BlockCache: sharedCache,
	})
	if err != nil {
		t.Fatalf("failed to open reader 2: %v", err)
	}
	defer func() { _ = reader2.Close() }()

	h1 := handles1[0]
	h2 := handles2[0]

	// Offsets should be equal (both are first block: offset 0)
	if h1.Offset != h2.Offset {
		t.Logf("offsets differ: %d vs %d, continuing...", h1.Offset, h2.Offset)
	}

	buf1, err := reader1.ReadBlock(h1)
	if err != nil {
		t.Fatalf("reader 1 ReadBlock failed: %v", err)
	}

	buf2, err := reader2.ReadBlock(h2)
	if err != nil {
		t.Fatalf("reader 2 ReadBlock failed: %v", err)
	}

	// Verify cache contains distinct entries for FileNum 1 and FileNum 2
	key1 := cache.NewBlockKey(1, h1.Offset)
	key2 := cache.NewBlockKey(2, h2.Offset)

	cached1, found1 := sharedCache.Get(key1)
	cached2, found2 := sharedCache.Get(key2)

	if !found1 || !found2 {
		t.Fatalf("expected both blocks in shared cache")
	}
	if !bytes.Equal(cached1, buf1) {
		t.Fatalf("cached 1 mismatch")
	}
	if !bytes.Equal(cached2, buf2) {
		t.Fatalf("cached 2 mismatch")
	}
}

func TestTableReader_Cache_DefensiveCopying(t *testing.T) {
	dir := t.TempDir()
	path, handles := helperBuildMultiBlockSSTable(t, dir, "000006.sst", 1, 5)

	blockCache, _ := cache.NewShardedCache(16)
	reader, _ := sstable.NewTableReaderWithOptions(path, sstable.TableReaderOptions{
		FileNum:    6,
		BlockCache: blockCache,
	})
	defer func() { _ = reader.Close() }()

	h0 := handles[0]

	// 1. Read block and mutate the returned slice
	buf, err := reader.ReadBlock(h0)
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}
	originalCopy := bytes.Clone(buf)

	// Mutate caller buffer
	for i := range buf {
		buf[i] = 0xAA
	}

	// 2. Read block again from cache -> must match uncorrupted originalCopy!
	bufWarm, err := reader.ReadBlock(h0)
	if err != nil {
		t.Fatalf("warm ReadBlock failed: %v", err)
	}
	if !bytes.Equal(bufWarm, originalCopy) {
		t.Fatalf("caller buffer mutation corrupted cached data!")
	}
}

func TestTableReader_Cache_Seek_PointLookup(t *testing.T) {
	dir := t.TempDir()
	path, _ := helperBuildMultiBlockSSTable(t, dir, "000007.sst", 2, 5)

	blockCache, _ := cache.NewShardedCache(16)
	reader, err := sstable.NewTableReaderWithOptions(path, sstable.TableReaderOptions{
		FileNum:    7,
		BlockCache: blockCache,
	})
	if err != nil {
		t.Fatalf("failed to open TableReader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	var physicalReads atomic.Int64
	origReadAt := reader.ReadAtFnForTesting()
	reader.SetReadAtFnForTesting(func(p []byte, off int64) (int, error) {
		physicalReads.Add(1)
		return origReadAt(p, off)
	})

	// Seek key in block 0
	val1, err := reader.Seek([]byte("key-000-000"))
	if err != nil {
		t.Fatalf("Seek key-000-000 failed: %v", err)
	}
	if !bytes.HasPrefix(val1, []byte("val-000-000")) {
		t.Fatalf("expected val-000-000 prefix, got %s", string(val1))
	}
	if physicalReads.Load() != 1 {
		t.Fatalf("expected 1 physical read on first Seek, got %d", physicalReads.Load())
	}

	// Seek same key again -> must be served from cache (0 new physical reads!)
	val1Again, err := reader.Seek([]byte("key-000-000"))
	if err != nil {
		t.Fatalf("Seek again failed: %v", err)
	}
	if !bytes.Equal(val1, val1Again) {
		t.Fatalf("val mismatch on repeated seek")
	}
	if physicalReads.Load() != 1 {
		t.Fatalf("expected physicalReads == 1 on second Seek, got %d", physicalReads.Load())
	}

	// Seek different key in the SAME block 0 -> must also be served from cache!
	val2, err := reader.Seek([]byte("key-000-001"))
	if err != nil {
		t.Fatalf("Seek key-000-001 failed: %v", err)
	}
	if !bytes.HasPrefix(val2, []byte("val-000-001")) {
		t.Fatalf("expected val-000-001 prefix, got %s", string(val2))
	}
	if physicalReads.Load() != 1 {
		t.Fatalf("expected physicalReads to stay 1 for same-block key lookup, got %d", physicalReads.Load())
	}
}

func TestTableReader_Cache_TableIterator(t *testing.T) {
	dir := t.TempDir()
	path, _ := helperBuildMultiBlockSSTable(t, dir, "000008.sst", 3, 5)

	blockCache, _ := cache.NewShardedCache(32)
	reader, err := sstable.NewTableReaderWithOptions(path, sstable.TableReaderOptions{
		FileNum:    8,
		BlockCache: blockCache,
	})
	if err != nil {
		t.Fatalf("failed to open TableReader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	var physicalReads atomic.Int64
	origReadAt := reader.ReadAtFnForTesting()
	reader.SetReadAtFnForTesting(func(p []byte, off int64) (int, error) {
		physicalReads.Add(1)
		return origReadAt(p, off)
	})

	// Pass 1: Iterate all entries (warms cache)
	iter1, err := reader.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator: %v", err)
	}
	if err := iter1.SeekToFirst(); err != nil {
		t.Fatalf("SeekToFirst failed on iter1: %v", err)
	}
	count1 := 0
	for iter1.Valid() {
		count1++
		iter1.Next()
	}
	if err := iter1.Err(); err != nil {
		t.Fatalf("iterator 1 error: %v", err)
	}
	if count1 != 15 {
		t.Fatalf("expected 15 entries, got %d", count1)
	}
	readsPass1 := physicalReads.Load()
	if readsPass1 == 0 {
		t.Fatalf("expected physical reads during pass 1")
	}

	// Pass 2: Iterate again with a new iterator on the same reader -> 0 new physical reads!
	iter2, err := reader.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator 2: %v", err)
	}
	if err := iter2.SeekToFirst(); err != nil {
		t.Fatalf("SeekToFirst failed on iter2: %v", err)
	}
	count2 := 0
	for iter2.Valid() {
		count2++
		iter2.Next()
	}
	if err := iter2.Err(); err != nil {
		t.Fatalf("iterator 2 error: %v", err)
	}
	if count2 != 15 {
		t.Fatalf("expected 15 entries in pass 2, got %d", count2)
	}

	readsPass2 := physicalReads.Load()
	if readsPass2 != readsPass1 {
		t.Fatalf("expected zero new physical reads during pass 2 (pass 1: %d, pass 2: %d)", readsPass1, readsPass2)
	}
}

func TestTableReader_Cache_ConcurrentStress_64Workers(t *testing.T) {
	dir := t.TempDir()
	path, handles := helperBuildMultiBlockSSTable(t, dir, "000009.sst", 8, 10)

	blockCache, _ := cache.NewShardedCache(64)
	reader, err := sstable.NewTableReaderWithOptions(path, sstable.TableReaderOptions{
		FileNum:    9,
		BlockCache: blockCache,
	})
	if err != nil {
		t.Fatalf("failed to open TableReader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	const workers = 64
	const opsPerWorker = 200

	var wg sync.WaitGroup
	startBarrier := make(chan struct{})

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			<-startBarrier

			for i := 0; i < opsPerWorker; i++ {
				// Mix between ReadBlock and Seek
				if (workerID+i)%2 == 0 {
					h := handles[(workerID+i)%len(handles)]
					buf, err := reader.ReadBlock(h)
					if err != nil {
						t.Errorf("worker %d ReadBlock failed: %v", workerID, err)
						return
					}
					if len(buf) == 0 {
						t.Errorf("worker %d empty block", workerID)
						return
					}
				} else {
					key := fmt.Sprintf("key-%03d-%03d", (workerID+i)%8, (workerID*3+i)%10)
					val, err := reader.Seek([]byte(key))
					if err != nil {
						t.Errorf("worker %d Seek(%s) failed: %v", workerID, key, err)
						return
					}
					if len(val) == 0 {
						t.Errorf("worker %d empty value for %s", workerID, key)
						return
					}
				}
			}
		}(w)
	}

	close(startBarrier)
	wg.Wait()
}
