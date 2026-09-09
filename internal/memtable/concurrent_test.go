package memtable_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
)

// TestSkipList_SearchConcurrent_BasicEquivalence verifies that SearchConcurrent
// produces identical results to Search across all standard operations.
func TestSkipList_SearchConcurrent_BasicEquivalence(t *testing.T) {
	sl := memtable.NewSkipList()

	// 1. Empty search
	got, err := sl.SearchConcurrent([]byte("key-empty"))
	if got != nil || !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("expected ErrKeyNotFound on empty SearchConcurrent, got %v (err: %v)", got, err)
	}

	// 2. Insert items
	keys := []string{"orange", "apple", "banana", "grape", "peach"}
	for i, k := range keys {
		ikey := sampleKey(t, k, uint64(i+1), binary.OpTypePut)
		if err := sl.Insert(ikey, []byte("val-"+k)); err != nil {
			t.Fatalf("insert %s failed: %v", k, err)
		}
	}

	// 3. Search and SearchConcurrent comparison
	for _, k := range keys {
		uKey := []byte(k)
		syncVal, syncErr := sl.Search(uKey)
		concVal, concErr := sl.SearchConcurrent(uKey)

		if !stdErrors.Is(syncErr, concErr) {
			t.Errorf("key %s error mismatch: sync=%v, conc=%v", k, syncErr, concErr)
		}
		if !bytes.Equal(syncVal, concVal) {
			t.Errorf("key %s value mismatch: sync=%q, conc=%q", k, syncVal, concVal)
		}
	}

	// 4. Tombstone masking equivalence
	tombKey := sampleKey(t, "apple", 100, binary.OpTypeDelete)
	if err := sl.Insert(tombKey, nil); err != nil {
		t.Fatalf("insert tombstone failed: %v", err)
	}

	syncVal, syncErr := sl.Search([]byte("apple"))
	concVal, concErr := sl.SearchConcurrent([]byte("apple"))
	if !stdErrors.Is(syncErr, errors.ErrKeyNotFound) || !stdErrors.Is(concErr, errors.ErrKeyNotFound) {
		t.Errorf("expected tombstone ErrKeyNotFound: syncErr=%v, concErr=%v", syncErr, concErr)
	}
	if syncVal != nil || concVal != nil {
		t.Errorf("expected nil values: syncVal=%v, concVal=%v", syncVal, concVal)
	}
}

// TestSkipList_SearchConcurrent_LockFreeIndependence proves that SearchConcurrent
// traverses the SkipList completely lock-free and does NOT acquire the writer mutation lock.
func TestSkipList_SearchConcurrent_LockFreeIndependence(t *testing.T) {
	sl := memtable.NewSkipList()
	k := sampleKey(t, "lockfree-test-key", 10, binary.OpTypePut)
	if err := sl.Insert(k, []byte("lockfree-val")); err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	// Intentionally acquire the writer lock exclusively
	sl.WriterLockForTesting()

	searchConcurrentDone := make(chan struct{})
	var concVal []byte
	var concErr error

	// A concurrent reader executing SearchConcurrent must NOT block on the writer lock
	go func() {
		concVal, concErr = sl.SearchConcurrent([]byte("lockfree-test-key"))
		close(searchConcurrentDone)
	}()

	select {
	case <-searchConcurrentDone:
		// SearchConcurrent completed successfully while writer lock was held!
		if concErr != nil || string(concVal) != "lockfree-val" {
			t.Errorf("SearchConcurrent failed while writer lock held: val=%q, err=%v", concVal, concErr)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("FAILURE: SearchConcurrent blocked on writer lock! Readers are not lock-free.")
	}

	// Now verify that Search (which takes s.mu.RLock) DOES block until writer unlock
	searchRLockDone := make(chan struct{})
	go func() {
		_, _ = sl.Search([]byte("lockfree-test-key"))
		close(searchRLockDone)
	}()

	select {
	case <-searchRLockDone:
		t.Fatalf("FAILURE: Search did not wait for writer lock release!")
	case <-time.After(50 * time.Millisecond):
		// Expected: Search is waiting for writer lock release
	}

	// Release writer lock
	sl.WriterUnlockForTesting()

	// Search can now proceed
	select {
	case <-searchRLockDone:
		// Succeeded after unlock
	case <-time.After(1 * time.Second):
		t.Fatalf("Search failed to complete after writer unlock")
	}
}

// TestSkipList_SearchConcurrent_ActiveHeightGrowth tests that concurrent readers
// safely observe active-height expansions (1 -> 2 -> 8 -> 16) without races or out-of-bound indexes.
func TestSkipList_SearchConcurrent_ActiveHeightGrowth(t *testing.T) {
	sl := memtable.NewSkipList()
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 8 concurrent readers continuously searching
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					key := []byte(fmt.Sprintf("grow-key-%02d", readerID))
					_, _ = sl.SearchConcurrent(key)
				}
			}
		}(r)
	}

	// Writer inserts nodes with progressively taller heights: 1, 2, 8, 16
	growthHeights := []int{1, 2, 8, 16, 4, 12, 16}
	for i, h := range growthHeights {
		time.Sleep(5 * time.Millisecond)
		k := sampleKey(t, fmt.Sprintf("grow-key-%02d", i), uint64(i+1), binary.OpTypePut)
		if err := sl.InsertWithHeightForTesting(k, []byte(fmt.Sprintf("val-%d", i)), h); err != nil {
			t.Fatalf("insert height %d failed: %v", h, err)
		}
	}

	close(stop)
	wg.Wait()

	if sl.Height() != 16 {
		t.Errorf("expected final height 16, got %d", sl.Height())
	}
	if err := sl.ValidateStructureForTesting(); err != nil {
		t.Errorf("structural validation failed after height growth: %v", err)
	}
}

// TestSkipList_SearchConcurrent_PublicationBoundary tests that a newly allocated node
// is completely initialized before becoming reader-visible, and readers never see uninitialized memory.
func TestSkipList_SearchConcurrent_PublicationBoundary(t *testing.T) {
	sl := memtable.NewSkipList()
	const iterations = 500
	var wg sync.WaitGroup

	stopReaders := make(chan struct{})

	// Readers continuously search for target keys
	var readCount atomic.Int64
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
					for i := 0; i < iterations; i++ {
						target := []byte(fmt.Sprintf("boundary:%04d", i))
						val, err := sl.SearchConcurrent(target)
						readCount.Add(1)
						if err == nil {
							// If found, the value must be complete and valid, never partial or empty
							expectedVal := fmt.Sprintf("val:%04d", i)
							if string(val) != expectedVal {
								t.Errorf("observed corrupted/partial value for %s: got %q, want %q",
									target, val, expectedVal)
							}
						}
					}
				}
			}
		}()
	}

	// Writer inserts items sequentially
	for i := 0; i < iterations; i++ {
		k := sampleKey(t, fmt.Sprintf("boundary:%04d", i), uint64(i+1), binary.OpTypePut)
		v := []byte(fmt.Sprintf("val:%04d", i))
		if err := sl.Insert(k, v); err != nil {
			t.Fatalf("insert %d failed: %v", i, err)
		}
	}

	close(stopReaders)
	wg.Wait()

	if readCount.Load() == 0 {
		t.Errorf("expected readers to execute lookups")
	}
}

// TestSkipList_SearchConcurrent_ExactDuplicateAtomicity tests that updating an exact duplicate
// InternalKey (same UserKey, SeqNum, OpType) is completely race-free with concurrent readers.
func TestSkipList_SearchConcurrent_ExactDuplicateAtomicity(t *testing.T) {
	sl := memtable.NewSkipList()
	key := sampleKey(t, "exact-dup-key", 42, binary.OpTypePut)
	if err := sl.Insert(key, []byte("v0")); err != nil {
		t.Fatalf("initial insert failed: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Concurrent readers continuously read value
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					val, err := sl.SearchConcurrent([]byte("exact-dup-key"))
					if err != nil {
						t.Errorf("unexpected error on duplicate key: %v", err)
					}
					// Value must start with 'v' and be complete
					if len(val) == 0 || val[0] != 'v' {
						t.Errorf("corrupted value observed: %q", val)
					}
				}
			}
		}()
	}

	// Writer repeatedly updates the value for the exact same InternalKey
	for i := 1; i <= 200; i++ {
		newVal := []byte(fmt.Sprintf("v%d", i))
		if err := sl.Insert(key, newVal); err != nil {
			t.Fatalf("duplicate insert %d failed: %v", i, err)
		}
	}

	close(stop)
	wg.Wait()

	// Final value must be v200
	finalVal, err := sl.SearchConcurrent([]byte("exact-dup-key"))
	if err != nil || string(finalVal) != "v200" {
		t.Errorf("expected final value v200, got %q (err: %v)", finalVal, err)
	}
	if sl.Len() != 1 {
		t.Errorf("expected Len() == 1, got %d", sl.Len())
	}
}

// TestSkipList_ConcurrentStress_16Readers1Writer fulfills the roadmap requirement:
// 16 reader goroutines + 1 writer goroutine running simultaneously under race detector.
func TestSkipList_ConcurrentStress_16Readers1Writer(t *testing.T) {
	sl := memtable.NewSkipList()

	const (
		totalWrites   = 3000
		keySpace      = 500 // Re-writes exercise multi-versioning
		readerWorkers = 16
	)

	type oracleEntry struct {
		val       []byte
		isDeleted bool
	}

	// Thread-safe oracle tracking latest committed state
	var oracleMu sync.RWMutex
	oracle := make(map[string]oracleEntry, keySpace)

	stopReaders := make(chan struct{})
	var readerWg sync.WaitGroup
	var totalLookups atomic.Int64
	var foundCount atomic.Int64
	var notFoundCount atomic.Int64

	// Start 16 reader goroutines
	for r := 0; r < readerWorkers; r++ {
		readerWg.Add(1)
		go func(workerID int) {
			defer readerWg.Done()
			rng := memtable.NewPCG32(uint64(workerID*1000+1), 0xABCD)

			for {
				select {
				case <-stopReaders:
					return
				default:
					totalLookups.Add(1)
					// Search existing keys, nonexistent keys, or random keys
					keyID := rng.Uint32() % (keySpace + 200)
					var uKey []byte
					if keyID >= keySpace {
						// Guaranteed absent key
						uKey = []byte(fmt.Sprintf("absent:%04d", keyID))
					} else {
						uKey = []byte(fmt.Sprintf("user:%04d", keyID))
					}

					val, err := sl.SearchConcurrent(uKey)

					if err != nil {
						if !stdErrors.Is(err, errors.ErrKeyNotFound) {
							t.Errorf("unexpected error type: %v", err)
						}
						notFoundCount.Add(1)
					} else {
						foundCount.Add(1)
						// Linearization check: if found, the value must match some validly written payload
						if len(val) == 0 || !bytes.HasPrefix(val, []byte("payload-")) {
							t.Errorf("corrupted payload observed: %q", val)
						}
					}
				}
			}
		}(r)
	}

	// 1 writer goroutine performing writes
	writerRng := memtable.NewPCG32(0xCAFEBABE, 0xDEADBEEF)
	for i := 1; i <= totalWrites; i++ {
		keyID := writerRng.Uint32() % keySpace
		uKey := []byte(fmt.Sprintf("user:%04d", keyID))
		seqNum := binary.SeqNum(i)

		var op binary.OpType
		var val []byte
		if i%10 == 0 {
			op = binary.OpTypeDelete
			val = nil
		} else {
			op = binary.OpTypePut
			val = []byte(fmt.Sprintf("payload-v%d-k%04d", i, keyID))
		}

		key, err := binary.NewInternalKey(uKey, seqNum, op)
		if err != nil {
			t.Fatalf("failed to create key %d: %v", i, err)
		}

		if err := sl.Insert(key, val); err != nil {
			t.Fatalf("writer insert %d failed: %v", i, err)
		}

		// Update test oracle
		oracleMu.Lock()
		oracle[string(uKey)] = oracleEntry{
			val:       val,
			isDeleted: (op == binary.OpTypeDelete),
		}
		oracleMu.Unlock()
	}

	// Stop readers and await completion
	close(stopReaders)
	readerWg.Wait()

	// Post-run validation: verify final state against oracle
	oracleMu.RLock()
	for kStr, exp := range oracle {
		uKey := []byte(kStr)
		got, err := sl.SearchConcurrent(uKey)

		if exp.isDeleted {
			if got != nil || !stdErrors.Is(err, errors.ErrKeyNotFound) {
				t.Errorf("key %s was deleted, but SearchConcurrent returned val=%q, err=%v",
					kStr, got, err)
			}
		} else {
			if err != nil {
				t.Errorf("key %s search failed: %v", kStr, err)
			}
			if !bytes.Equal(got, exp.val) {
				t.Errorf("key %s value mismatch: got %q, want %q", kStr, got, exp.val)
			}
		}
	}
	oracleMu.RUnlock()

	// Verify all absent keys return ErrKeyNotFound
	for i := 0; i < 200; i++ {
		absentKey := []byte(fmt.Sprintf("absent:%04d", keySpace+i))
		got, err := sl.SearchConcurrent(absentKey)
		if got != nil || !stdErrors.Is(err, errors.ErrKeyNotFound) {
			t.Errorf("absent key returned val=%v, err=%v", got, err)
		}
	}

	// Validate structural integrity
	if err := sl.ValidateStructureForTesting(); err != nil {
		t.Fatalf("structural validation failed after concurrent stress: %v", err)
	}

	if sl.Len() != totalWrites {
		t.Errorf("expected Len() == %d, got %d", totalWrites, sl.Len())
	}
}

// TestSkipList_ConcurrentStress_TombstonesAndRePut tests interleaving PUTs and DELETEs
// on the same keys while readers concurrently query them.
func TestSkipList_ConcurrentStress_TombstonesAndRePut(t *testing.T) {
	sl := memtable.NewSkipList()
	const userKeyStr = "frequent-toggle-key"
	uKey := []byte(userKeyStr)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Readers query the key repeatedly
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					val, err := sl.SearchConcurrent(uKey)
					if err != nil {
						if !stdErrors.Is(err, errors.ErrKeyNotFound) {
							t.Errorf("unexpected error: %v", err)
						}
					} else {
						// Must be a valid put value
						if !bytes.HasPrefix(val, []byte("val-put-")) {
							t.Errorf("corrupted value observed: %q", val)
						}
					}
				}
			}
		}()
	}

	// Writer toggles PUT and DELETE 100 times with ascending sequence numbers
	for i := 1; i <= 100; i++ {
		seqNum := binary.SeqNum(i)
		if i%2 == 1 {
			// PUT
			k, _ := binary.NewInternalKey(uKey, seqNum, binary.OpTypePut)
			if err := sl.Insert(k, []byte(fmt.Sprintf("val-put-%d", i))); err != nil {
				t.Fatalf("put %d failed: %v", i, err)
			}
		} else {
			// DELETE
			k, _ := binary.NewInternalKey(uKey, seqNum, binary.OpTypeDelete)
			if err := sl.Insert(k, nil); err != nil {
				t.Fatalf("delete %d failed: %v", i, err)
			}
		}
	}

	close(stop)
	wg.Wait()

	// SeqNum 100 was DELETE: final lookup must report ErrKeyNotFound
	finalVal, finalErr := sl.SearchConcurrent(uKey)
	if finalVal != nil || !stdErrors.Is(finalErr, errors.ErrKeyNotFound) {
		t.Errorf("expected final ErrKeyNotFound for tombstone, got val=%v, err=%v", finalVal, finalErr)
	}

	// Now insert SeqNum 101 as PUT: key must become visible again
	k101, _ := binary.NewInternalKey(uKey, 101, binary.OpTypePut)
	if err := sl.Insert(k101, []byte("val-put-101")); err != nil {
		t.Fatalf("insert 101 failed: %v", err)
	}

	reputVal, reputErr := sl.SearchConcurrent(uKey)
	if reputErr != nil || string(reputVal) != "val-put-101" {
		t.Errorf("expected val-put-101, got %q (err: %v)", reputVal, reputErr)
	}
}
