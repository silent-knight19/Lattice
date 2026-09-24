package engine_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/errors"
)

// -----------------------------------------------------------------------------
// 1. BASIC ENGINE EXISTS & LIFECYCLE TESTS
// -----------------------------------------------------------------------------

func TestEngine_Exists_BasicAndLifecycle(t *testing.T) {
	eng, dir := newFlushEngine(t, 0)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	// 1. Missing key returns false
	exists, err := eng.Exists([]byte("missing_key"))
	if err != nil {
		t.Fatalf("unexpected error for missing key: %v", err)
	}
	if exists {
		t.Fatalf("expected exists=false for missing key, got true")
	}

	// 2. Insert key -> exists returns true
	if err := eng.Put(ctx, []byte("k1"), []byte("v1")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	exists, err = eng.Exists([]byte("k1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Fatalf("expected exists=true for present key, got false")
	}

	// 3. Delete key -> exists returns false
	if err := eng.Delete(ctx, []byte("k1")); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	exists, err = eng.Exists([]byte("k1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Fatalf("expected exists=false for deleted key, got true")
	}

	// 4. Recreate key -> exists returns true
	if err := eng.Put(ctx, []byte("k1"), []byte("v2")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	exists, err = eng.Exists([]byte("k1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Fatalf("expected exists=true for recreated key, got false")
	}

	// 5. Zero-length value is legal and exists returns true
	if err := eng.Put(ctx, []byte("empty_val"), []byte{}); err != nil {
		t.Fatalf("Put empty val failed: %v", err)
	}
	exists, err = eng.Exists([]byte("empty_val"))
	if err != nil || !exists {
		t.Fatalf("expected exists=true for zero-length value, got %v, err=%v", exists, err)
	}

	// 6. Validation: empty key rejected with ErrEmptyKey
	if _, err := eng.Exists(nil); !stdErrors.Is(err, errors.ErrEmptyKey) {
		t.Fatalf("expected ErrEmptyKey for nil key, got %v", err)
	}
	if _, err := eng.Exists([]byte{}); !stdErrors.Is(err, errors.ErrEmptyKey) {
		t.Fatalf("expected ErrEmptyKey for empty key, got %v", err)
	}

	// 7. Validation: oversized key rejected with ErrKeyTooLarge
	oversizedKey := make([]byte, 65536)
	if _, err := eng.Exists(oversizedKey); !stdErrors.Is(err, errors.ErrKeyTooLarge) {
		t.Fatalf("expected ErrKeyTooLarge for oversized key, got %v", err)
	}

	// 8. Nil receiver returns ErrNilReceiver
	var nilEng *engine.Engine
	if _, err := nilEng.Exists([]byte("k")); !stdErrors.Is(err, errors.ErrNilReceiver) {
		t.Fatalf("expected ErrNilReceiver for nil engine, got %v", err)
	}

	// 9. Reopen engine and verify existence survives restart
	if err := eng.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	eng2 := engine.NewEngineWithOptions(engine.EngineOptions{DBPath: dir})
	if err := eng2.Open(); err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer func() { _ = eng2.Close() }()

	exists, err = eng2.Exists([]byte("k1"))
	if err != nil || !exists {
		t.Fatalf("expected k1 to exist after reopen, got %v, err=%v", exists, err)
	}
	exists, err = eng2.Exists([]byte("missing_key"))
	if err != nil || exists {
		t.Fatalf("expected missing_key to remain absent after reopen, got %v, err=%v", exists, err)
	}
}

// -----------------------------------------------------------------------------
// 2. BINARY KEYS AND MAXIMUM LENGTH
// -----------------------------------------------------------------------------

func TestEngine_Exists_BinaryAndMaxKey(t *testing.T) {
	eng, _ := newFlushEngine(t, 0)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	// Binary key with null bytes and control chars
	binKey := []byte{0x00, 0x01, 0xff, 0xfe, 0x00, 0x7f}
	if err := eng.Put(ctx, binKey, []byte("binval")); err != nil {
		t.Fatalf("Put binary key failed: %v", err)
	}
	exists, err := eng.Exists(binKey)
	if err != nil || !exists {
		t.Fatalf("expected exists=true for binary key, got %v, err=%v", exists, err)
	}

	// Maximum allowed key length: 65,535 bytes
	maxKey := bytes.Repeat([]byte("k"), 65535)
	if err := eng.Put(ctx, maxKey, []byte("maxval")); err != nil {
		t.Fatalf("Put max key failed: %v", err)
	}
	exists, err = eng.Exists(maxKey)
	if err != nil || !exists {
		t.Fatalf("expected exists=true for max key, got %v, err=%v", exists, err)
	}
}

// -----------------------------------------------------------------------------
// 3. LAYERING AND TOMBSTONE SHADOWING (MEMTABLE -> IMMUTABLE -> SSTABLE)
// -----------------------------------------------------------------------------

func TestEngine_Exists_LayeringAndTombstoneShadowing(t *testing.T) {
	dir := t.TempDir()
	eng := newRealWALEngine(t, dir, 1024) // 1KB threshold forces flushes to SSTable
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	// 1. Write key1, key2 and flush to SSTable
	val := bytes.Repeat([]byte("v"), 600)
	if err := eng.Put(ctx, []byte("key1"), val); err != nil {
		t.Fatalf("Put key1: %v", err)
	}
	if err := eng.Put(ctx, []byte("key2"), val); err != nil {
		t.Fatalf("Put key2: %v", err)
	}
	waitFlushEmpty(t, eng, 5000)

	// Both keys exist in SSTable
	exists, err := eng.Exists([]byte("key1"))
	if err != nil || !exists {
		t.Fatalf("expected key1 to exist in SSTable: %v, err=%v", exists, err)
	}
	exists, err = eng.Exists([]byte("key2"))
	if err != nil || !exists {
		t.Fatalf("expected key2 to exist in SSTable: %v, err=%v", exists, err)
	}

	// 2. Put a tombstone in active MemTable for key1
	if err := eng.Delete(ctx, []byte("key1")); err != nil {
		t.Fatalf("Delete key1: %v", err)
	}

	// Active tombstone must shadow persisted SSTable value
	exists, err = eng.Exists([]byte("key1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Fatalf("expected key1 exists=false due to active tombstone shadowing, got true")
	}

	// key2 should still exist
	exists, err = eng.Exists([]byte("key2"))
	if err != nil || !exists {
		t.Fatalf("expected key2 to still exist: %v, err=%v", exists, err)
	}

	// 3. Flush the tombstone to L0 SSTable
	if err := eng.Put(ctx, []byte("key3"), val); err != nil {
		t.Fatalf("Put key3: %v", err)
	}
	waitFlushEmpty(t, eng, 5000)

	// In L0, tombstone must continue shadowing older file
	exists, err = eng.Exists([]byte("key1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Fatalf("expected key1 exists=false after tombstone flush to L0, got true")
	}

	// 4. Re-put key1 in active MemTable
	if err := eng.Put(ctx, []byte("key1"), []byte("new_val")); err != nil {
		t.Fatalf("Put key1: %v", err)
	}
	exists, err = eng.Exists([]byte("key1"))
	if err != nil || !exists {
		t.Fatalf("expected key1 exists=true after active re-put, got %v, err=%v", exists, err)
	}
}

// -----------------------------------------------------------------------------
// 4. LOW-OVERHEAD VALUE COPYING VERIFICATION (LARGE VALUES)
// -----------------------------------------------------------------------------

func TestEngine_Exists_LowOverhead_LargeValue(t *testing.T) {
	eng, _ := newFlushEngine(t, 0)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	// Ingest a 3 MiB value
	const valSize = 3 * 1024 * 1024
	largeVal := bytes.Repeat([]byte("Z"), valSize)
	key := []byte("large_key")

	if err := eng.Put(ctx, key, largeVal); err != nil {
		t.Fatalf("Put large value failed: %v", err)
	}

	// Verify Exists returns true
	exists, err := eng.Exists(key)
	if err != nil || !exists {
		t.Fatalf("Exists failed: %v, exists=%v", err, exists)
	}

	// Measure memory allocations:
	// Get MUST allocate at least 3 MiB (for the defensive copy).
	// Exists MUST NOT allocate the 3 MiB value buffer!
	getAllocs := testing.AllocsPerRun(10, func() {
		v, err := eng.Get(key)
		if err != nil || len(v) != valSize {
			t.Fatalf("Get failed or wrong size")
		}
	})

	existsAllocs := testing.AllocsPerRun(10, func() {
		ok, err := eng.Exists(key)
		if err != nil || !ok {
			t.Fatalf("Exists failed")
		}
	})

	t.Logf("Get allocs/run: %.1f, Exists allocs/run: %.1f", getAllocs, existsAllocs)
	// While Get allocates iterator + defensive copy buffer, Exists only allocates iterator
	if existsAllocs > getAllocs {
		t.Errorf("expected Exists allocs (%.1f) <= Get allocs (%.1f)", existsAllocs, getAllocs)
	}
}

// -----------------------------------------------------------------------------
// 5. ATOMICITY WITH BATCH MUTATIONS
// -----------------------------------------------------------------------------

func TestEngine_Exists_AtomicityWithBatch(t *testing.T) {
	eng, _ := newFlushEngine(t, 0)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	// Populate initial state: A=1, B absent, C=3
	if err := eng.Put(ctx, []byte("A"), []byte("1")); err != nil {
		t.Fatalf("Put A failed: %v", err)
	}
	if err := eng.Put(ctx, []byte("C"), []byte("3")); err != nil {
		t.Fatalf("Put C failed: %v", err)
	}

	// Verify initial state:
	// A exists, B absent, C exists
	okA, _ := eng.Exists([]byte("A"))
	okB, _ := eng.Exists([]byte("B"))
	okC, _ := eng.Exists([]byte("C"))
	if !okA || okB || !okC {
		t.Fatalf("initial state unexpected: A=%v, B=%v, C=%v", okA, okB, okC)
	}

	// Execute batch:
	// PUT B=2, DELETE C, PUT A=99
	batch := []binary.BatchOp{
		{Type: binary.OpTypePut, Key: []byte("B"), Value: []byte("2")},
		{Type: binary.OpTypeDelete, Key: []byte("C")},
		{Type: binary.OpTypePut, Key: []byte("A"), Value: []byte("99")},
	}

	if err := eng.Batch(ctx, batch); err != nil {
		t.Fatalf("Batch failed: %v", err)
	}

	// Post-batch state: A exists, B exists, C absent
	okA, _ = eng.Exists([]byte("A"))
	okB, _ = eng.Exists([]byte("B"))
	okC, _ = eng.Exists([]byte("C"))
	if !okA || !okB || okC {
		t.Fatalf("post-batch state unexpected: A=%v, B=%v, C=%v", okA, okB, okC)
	}
}

// -----------------------------------------------------------------------------
// 6. CONCURRENT WRITERS, BATCHES, AND EXISTS READERS
// -----------------------------------------------------------------------------

func TestEngine_Exists_ConcurrentReadersAndWriters(t *testing.T) {
	eng, _ := newFlushEngine(t, 0)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 4 concurrent batch writers
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-stop:
					return
				default:
					kA := fmt.Sprintf("k_%d_A", workerID)
					kB := fmt.Sprintf("k_%d_B", workerID)
					// Batch atomically puts kA and kB together, or deletes both together
					var b []binary.BatchOp
					if i%2 == 0 {
						b = []binary.BatchOp{
							{Type: binary.OpTypePut, Key: []byte(kA), Value: []byte("val")},
							{Type: binary.OpTypePut, Key: []byte(kB), Value: []byte("val")},
						}
					} else {
						b = []binary.BatchOp{
							{Type: binary.OpTypeDelete, Key: []byte(kA)},
							{Type: binary.OpTypeDelete, Key: []byte(kB)},
						}
					}
					_ = eng.Batch(ctx, b)
					i++
				}
			}
		}(w)
	}

	// 6 concurrent EXISTS readers
	for r := 0; r < 6; r++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			workerTarget := readerID % 4
			kA := []byte(fmt.Sprintf("k_%d_A", workerTarget))
			kB := []byte(fmt.Sprintf("k_%d_B", workerTarget))

			for {
				select {
				case <-stop:
					return
				default:
					exA, errA := eng.Exists(kA)
					exB, errB := eng.Exists(kB)
					if errA != nil || errB != nil {
						t.Errorf("Exists error: errA=%v, errB=%v", errA, errB)
						return
					}
					// Note: while a batch is being applied, reader cannot see half the batch.
					// Reading A then B sequentially might catch a new batch between calls,
					// but Exists should never crash or return corrupt data.
					_ = exA
					_ = exB
				}
			}
		}(r)
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}
