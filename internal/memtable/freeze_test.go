package memtable_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
)

func TestFreeze_BasicLifecycle(t *testing.T) {
	sl := memtable.NewSkipList()

	if sl.IsFrozen() {
		t.Fatalf("expected newly created SkipList to be ACTIVE (IsFrozen == false)")
	}

	// Insert an initial entry
	k1 := makeIK(t, "key-1", 1, binary.OpTypePut)
	if err := sl.Insert(k1, []byte("val-1")); err != nil {
		t.Fatalf("unexpected insert error: %v", err)
	}

	lenBefore := sl.Len()
	heightBefore := sl.Height()
	byteSizeBefore := sl.ByteSize()

	// First Freeze must return true (transition from ACTIVE -> FROZEN)
	if transitioned := sl.Freeze(); !transitioned {
		t.Fatalf("expected first Freeze() to return true, got false")
	}

	if !sl.IsFrozen() {
		t.Fatalf("expected SkipList to be FROZEN after Freeze()")
	}

	// Len, Height, and ByteSize must remain invariant across Freeze()
	if sl.Len() != lenBefore {
		t.Fatalf("Len changed across Freeze: before=%d, after=%d", lenBefore, sl.Len())
	}
	if sl.Height() != heightBefore {
		t.Fatalf("Height changed across Freeze: before=%d, after=%d", heightBefore, sl.Height())
	}
	if sl.ByteSize() != byteSizeBefore {
		t.Fatalf("ByteSize changed across Freeze: before=%d, after=%d", byteSizeBefore, sl.ByteSize())
	}

	// Second Freeze must return false (already FROZEN)
	if transitioned := sl.Freeze(); transitioned {
		t.Fatalf("expected second Freeze() to return false, got true")
	}

	if !sl.IsFrozen() {
		t.Fatalf("expected SkipList to remain FROZEN after redundant Freeze()")
	}
}

func TestFreeze_PostFreezeInsertRejected(t *testing.T) {
	sl := memtable.NewSkipList()
	k1 := makeIK(t, "user-key-1", 100, binary.OpTypePut)
	if err := sl.Insert(k1, []byte("original-val")); err != nil {
		t.Fatalf("unexpected insert error: %v", err)
	}

	sl.Freeze()

	lenBefore := sl.Len()
	heightBefore := sl.Height()
	byteSizeBefore := sl.ByteSize()

	// 1. Attempt inserting new key
	k2 := makeIK(t, "user-key-2", 101, binary.OpTypePut)
	err := sl.Insert(k2, []byte("val-2"))
	if !stdErrors.Is(err, errors.ErrMemTableFrozen) {
		t.Fatalf("expected ErrMemTableFrozen for new key, got: %v", err)
	}

	// 2. Attempt inserting duplicate InternalKey (value replacement attempt)
	err = sl.Insert(k1, []byte("mutated-val"))
	if !stdErrors.Is(err, errors.ErrMemTableFrozen) {
		t.Fatalf("expected ErrMemTableFrozen for duplicate key, got: %v", err)
	}
	// Verify original value was NOT mutated
	val, searchErr := sl.Search(k1.UserKey)
	if searchErr != nil || !bytes.Equal(val, []byte("original-val")) {
		t.Fatalf("duplicate update mutated value in frozen SkipList: val=%s, err=%v", val, searchErr)
	}

	// 3. Attempt inserting tombstone
	kTomb := makeIK(t, "user-key-3", 102, binary.OpTypeDelete)
	err = sl.Insert(kTomb, nil)
	if !stdErrors.Is(err, errors.ErrMemTableFrozen) {
		t.Fatalf("expected ErrMemTableFrozen for tombstone, got: %v", err)
	}

	// 4. Attempt inserting invalid key (lifecycle boundary priority)
	kInvalid := binary.InternalKey{UserKey: nil, SeqNum: 103, OpType: binary.OpTypePut}
	err = sl.Insert(kInvalid, []byte("val"))
	if !stdErrors.Is(err, errors.ErrMemTableFrozen) {
		t.Fatalf("expected ErrMemTableFrozen for invalid key on frozen table, got: %v", err)
	}

	// 5. Verify accounting remained completely unchanged
	if sl.Len() != lenBefore {
		t.Fatalf("Len changed after rejected inserts: before=%d, after=%d", lenBefore, sl.Len())
	}
	if sl.Height() != heightBefore {
		t.Fatalf("Height changed after rejected inserts: before=%d, after=%d", heightBefore, sl.Height())
	}
	if sl.ByteSize() != byteSizeBefore {
		t.Fatalf("ByteSize changed after rejected inserts: before=%d, after=%d", byteSizeBefore, sl.ByteSize())
	}
}

func TestFreeze_ReaderContinuity(t *testing.T) {
	sl := memtable.NewSkipList()
	keys := []string{"apple", "banana", "cherry", "date"}

	for i, k := range keys {
		ik := makeIK(t, k, uint64(i+1), binary.OpTypePut)
		if err := sl.Insert(ik, []byte("val-"+k)); err != nil {
			t.Fatalf("insert failed: %v", err)
		}
	}

	// Search before freeze
	for _, k := range keys {
		val, err := sl.SearchConcurrent([]byte(k))
		if err != nil || string(val) != "val-"+k {
			t.Fatalf("pre-freeze SearchConcurrent failed for %s: val=%s, err=%v", k, val, err)
		}
	}

	sl.Freeze()

	// Search after freeze (both Search and SearchConcurrent)
	for _, k := range keys {
		val, err := sl.Search([]byte(k))
		if err != nil || string(val) != "val-"+k {
			t.Fatalf("post-freeze Search failed for %s: val=%s, err=%v", k, val, err)
		}

		valConc, errConc := sl.SearchConcurrent([]byte(k))
		if errConc != nil || string(valConc) != "val-"+k {
			t.Fatalf("post-freeze SearchConcurrent failed for %s: val=%s, err=%v", k, valConc, errConc)
		}
	}

	// Nonexistent key after freeze
	_, err := sl.SearchConcurrent([]byte("nonexistent"))
	if !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound for nonexistent key after freeze, got: %v", err)
	}
}

func TestFreeze_IteratorContinuity(t *testing.T) {
	sl := memtable.NewSkipList()
	for i := 1; i <= 5; i++ {
		ik := makeIK(t, fmt.Sprintf("key-%d", i), uint64(i), binary.OpTypePut)
		_ = sl.Insert(ik, []byte(fmt.Sprintf("val-%d", i)))
	}

	// 1. Iterator created BEFORE Freeze, advanced partially
	itBefore := sl.NewIterator()
	if !itBefore.Next() {
		t.Fatalf("itBefore Next() failed")
	}
	if string(itBefore.Key().UserKey) != "key-1" {
		t.Fatalf("expected key-1, got %s", itBefore.Key().UserKey)
	}

	// Freeze occurs while itBefore is positioned on key-1
	sl.Freeze()

	// Continue advancing itBefore to end
	expectedIdx := 2
	for itBefore.Next() {
		expectedKey := fmt.Sprintf("key-%d", expectedIdx)
		if string(itBefore.Key().UserKey) != expectedKey {
			t.Fatalf("expected %s, got %s", expectedKey, itBefore.Key().UserKey)
		}
		expectedIdx++
	}
	if expectedIdx != 6 {
		t.Fatalf("itBefore failed to traverse all keys, ended at %d", expectedIdx)
	}

	// 2. Iterator created AFTER Freeze
	itAfter := sl.NewIterator()
	itAfter.SeekToFirst()
	count := 0
	for itAfter.Valid() {
		count++
		itAfter.Next()
	}
	if count != 5 {
		t.Fatalf("itAfter expected 5 keys, got %d", count)
	}

	// 3. Seek after Freeze
	if err := itAfter.Seek([]byte("key-3")); err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	if !itAfter.Valid() || string(itAfter.Key().UserKey) != "key-3" {
		t.Fatalf("Seek key-3 landed on: %s", itAfter.Key().UserKey)
	}
}

func TestFreeze_IteratorCloseMemoryReclamation(t *testing.T) {
	sl := memtable.NewSkipList()
	ik := makeIK(t, "user-key", 1, binary.OpTypePut)
	_ = sl.Insert(ik, []byte("value"))

	it := sl.NewIterator()
	it.SeekToFirst()
	if !it.Valid() {
		t.Fatalf("iterator expected valid")
	}

	// Close iterator
	it.Close()

	if it.Valid() {
		t.Fatalf("expected Valid() == false after Close()")
	}
	if it.Next() {
		t.Fatalf("expected Next() == false after Close()")
	}
	if k := it.Key(); len(k.UserKey) != 0 {
		t.Fatalf("expected empty Key() after Close(), got: %v", k)
	}
	if v := it.Value(); v != nil {
		t.Fatalf("expected nil Value() after Close(), got: %v", v)
	}

	// Seek on closed iterator must return ErrIteratorClosed without panicking
	err := it.Seek([]byte("user-key"))
	if !stdErrors.Is(err, errors.ErrIteratorClosed) {
		t.Fatalf("expected ErrIteratorClosed from Seek() on closed iterator, got: %v", err)
	}

	err = it.SeekInternalKey(ik)
	if !stdErrors.Is(err, errors.ErrIteratorClosed) {
		t.Fatalf("expected ErrIteratorClosed from SeekInternalKey() on closed iterator, got: %v", err)
	}

	// SeekToFirst on closed iterator must be a safe no-op
	it.SeekToFirst()
	if it.Valid() {
		t.Fatalf("expected Valid() == false after SeekToFirst() on closed iterator")
	}

	// Redundant Close must be safe and idempotent
	it.Close()
}

func TestFreeze_ConcurrentInsertVsFreezeOracle(t *testing.T) {
	sl := memtable.NewSkipList()
	const numWriters = 10
	const writesPerWorker = 200

	startBarrier := make(chan struct{})
	var wg sync.WaitGroup

	type writeResult struct {
		key string
		err error
	}

	results := make([][]writeResult, numWriters)

	for w := 0; w < numWriters; w++ {
		wg.Add(1)
		results[w] = make([]writeResult, 0, writesPerWorker)
		go func(workerID int) {
			defer wg.Done()
			<-startBarrier

			for i := 0; i < writesPerWorker; i++ {
				kStr := fmt.Sprintf("worker-%02d-key-%04d", workerID, i)
				ik := binary.InternalKey{
					UserKey: []byte(kStr),
					SeqNum:  binary.SeqNum(workerID*writesPerWorker + i + 1),
					OpType:  binary.OpTypePut,
				}
				err := sl.Insert(ik, []byte("data"))
				results[workerID] = append(results[workerID], writeResult{key: kStr, err: err})
			}
		}(w)
	}

	// Freezer goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-startBarrier
		// Let some writes proceed, then freeze
		sl.Freeze()
	}()

	close(startBarrier)
	wg.Wait()

	if !sl.IsFrozen() {
		t.Fatalf("expected SkipList to be FROZEN")
	}

	// Scan frozen SkipList and build map of actually stored keys
	storedKeys := make(map[string]bool)
	it := sl.NewIterator()
	it.SeekToFirst()
	for it.Valid() {
		storedKeys[string(it.Key().UserKey)] = true
		it.Next()
	}

	// Verify the linearizability oracle
	acceptedCount := 0
	rejectedCount := 0

	for workerID := 0; workerID < numWriters; workerID++ {
		for _, res := range results[workerID] {
			if res.err == nil {
				acceptedCount++
				// CASE A: Every accepted insert MUST be present in the frozen structure
				if !storedKeys[res.key] {
					t.Fatalf("linearizability violation: insert for %s reported success but was missing from frozen SkipList", res.key)
				}
			} else if stdErrors.Is(res.err, errors.ErrMemTableFrozen) {
				rejectedCount++
				// CASE B: Every rejected insert MUST NOT be present in the frozen structure
				if storedKeys[res.key] {
					t.Fatalf("linearizability violation: insert for %s returned ErrMemTableFrozen but was found in frozen SkipList", res.key)
				}
			} else {
				t.Fatalf("unexpected error from Insert: %v", res.err)
			}
		}
	}

	if acceptedCount != sl.Len() {
		t.Fatalf("sl.Len() mismatch: accepted=%d, Len=%d", acceptedCount, sl.Len())
	}
	if len(storedKeys) != sl.Len() {
		t.Fatalf("storedKeys count mismatch: stored=%d, Len=%d", len(storedKeys), sl.Len())
	}
}

func TestFreeze_ConcurrentFreezeIdempotence(t *testing.T) {
	sl := memtable.NewSkipList()
	const numCallers = 20

	startBarrier := make(chan struct{})
	var wg sync.WaitGroup
	var transitionCount atomic.Int32

	for i := 0; i < numCallers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startBarrier
			if sl.Freeze() {
				transitionCount.Add(1)
			}
		}()
	}

	close(startBarrier)
	wg.Wait()

	if transitionCount.Load() != 1 {
		t.Fatalf("expected exactly 1 Freeze() caller to return true, got %d", transitionCount.Load())
	}
	if !sl.IsFrozen() {
		t.Fatalf("expected SkipList to be FROZEN")
	}
}

func TestFreeze_ConcurrentReadersDuringFreeze(t *testing.T) {
	sl := memtable.NewSkipList()
	// Prepopulate
	for i := 0; i < 50; i++ {
		ik := makeIK(t, fmt.Sprintf("key-%03d", i), uint64(i+1), binary.OpTypePut)
		_ = sl.Insert(ik, []byte("initial"))
	}

	const numReaders = 16
	stopCh := make(chan struct{})
	var readersWg sync.WaitGroup
	var mutationWg sync.WaitGroup

	// Reader goroutines
	for r := 0; r < numReaders; r++ {
		readersWg.Add(1)
		go func(readerID int) {
			defer readersWg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					target := fmt.Sprintf("key-%03d", readerID%50)
					_, _ = sl.SearchConcurrent([]byte(target))
					_ = sl.ByteSize()
					_ = sl.Len()
					_ = sl.IsFrozen()

					it := sl.NewIterator()
					_ = it.Seek([]byte(target))
					if it.Valid() {
						_ = it.Key()
						_ = it.Value()
					}
					it.Close()
				}
			}
		}(r)
	}

	// Writer goroutine
	mutationWg.Add(1)
	go func() {
		defer mutationWg.Done()
		for i := 50; i < 150; i++ {
			ik := makeIK(t, fmt.Sprintf("key-%03d", i), uint64(i+1), binary.OpTypePut)
			if err := sl.Insert(ik, []byte("concurrent")); err != nil {
				// Once frozen, inserts will fail with ErrMemTableFrozen
				break
			}
		}
	}()

	// Freezer goroutine
	mutationWg.Add(1)
	go func() {
		defer mutationWg.Done()
		sl.Freeze()
	}()

	mutationWg.Wait()
	close(stopCh)
	readersWg.Wait()

	if !sl.IsFrozen() {
		t.Fatalf("expected SkipList to be FROZEN")
	}
}

// === Formal Invariants Tests (P03-S03-M02-INV-01 .. 08) ===

// TestInvariant_P03_S03_M02_INV_01_MonotonicLifecycle verifies that the lifecycle
// state is strictly monotonic: ACTIVE -> FROZEN, and cannot transition back to ACTIVE.
func TestInvariant_P03_S03_M02_INV_01_MonotonicLifecycle(t *testing.T) {
	sl := memtable.NewSkipList()
	if sl.IsFrozen() {
		t.Fatalf("initial state must be ACTIVE")
	}

	sl.Freeze()
	if !sl.IsFrozen() {
		t.Fatalf("state must be FROZEN after Freeze")
	}

	// Repeated freeze
	sl.Freeze()
	if !sl.IsFrozen() {
		t.Fatalf("state must remain FROZEN after repeated Freeze")
	}
}

// TestInvariant_P03_S03_M02_INV_02_PostFreezeMutationImpossibility verifies that once
// Freeze linearizes, no subsequent Insert can mutate the structure.
func TestInvariant_P03_S03_M02_INV_02_PostFreezeMutationImpossibility(t *testing.T) {
	sl := memtable.NewSkipList()
	ik := makeIK(t, "key-a", 1, binary.OpTypePut)
	_ = sl.Insert(ik, []byte("val-a"))

	sl.Freeze()

	// Try 100 random mutations
	for i := 0; i < 100; i++ {
		key := makeIK(t, fmt.Sprintf("key-%d", i), uint64(i+10), binary.OpTypePut)
		err := sl.Insert(key, []byte("payload"))
		if !stdErrors.Is(err, errors.ErrMemTableFrozen) {
			t.Fatalf("post-freeze insert %d did not return ErrMemTableFrozen: %v", i, err)
		}
	}

	if sl.Len() != 1 {
		t.Fatalf("expected Len == 1, got %d", sl.Len())
	}
}

// TestInvariant_P03_S03_M02_INV_03_PreFreezeLinearizationInclusion verifies that every Insert
// that linearizes before Freeze is represented in the final frozen structure.
func TestInvariant_P03_S03_M02_INV_03_PreFreezeLinearizationInclusion(t *testing.T) {
	sl := memtable.NewSkipList()
	const count = 50

	for i := 0; i < count; i++ {
		ik := makeIK(t, fmt.Sprintf("k-%03d", i), uint64(i+1), binary.OpTypePut)
		if err := sl.Insert(ik, []byte("v")); err != nil {
			t.Fatalf("pre-freeze insert failed: %v", err)
		}
	}

	sl.Freeze()

	it := sl.NewIterator()
	it.SeekToFirst()
	seen := 0
	for it.Valid() {
		seen++
		it.Next()
	}
	if seen != count {
		t.Fatalf("expected all %d pre-freeze inserts to be included, saw %d", count, seen)
	}
}

// TestInvariant_P03_S03_M02_INV_04_PostFreezeRejectionInvariance verifies that every
// post-freeze rejected Insert leaves structure, Len, Height, and ByteSize unchanged.
func TestInvariant_P03_S03_M02_INV_04_PostFreezeRejectionInvariance(t *testing.T) {
	sl := memtable.NewSkipList()
	ik := makeIK(t, "seed", 1, binary.OpTypePut)
	_ = sl.Insert(ik, []byte("seed-val"))

	sl.Freeze()

	len0 := sl.Len()
	h0 := sl.Height()
	b0 := sl.ByteSize()

	// Rejected inserts
	_ = sl.Insert(makeIK(t, "new-key", 2, binary.OpTypePut), []byte("val"))
	_ = sl.Insert(ik, []byte("duplicate-swap-attempt"))
	_ = sl.Insert(makeIK(t, "tomb", 3, binary.OpTypeDelete), nil)

	if sl.Len() != len0 || sl.Height() != h0 || sl.ByteSize() != b0 {
		t.Fatalf("invariance violated: Len (%d->%d), Height (%d->%d), ByteSize (%d->%d)",
			len0, sl.Len(), h0, sl.Height(), b0, sl.ByteSize())
	}
}

// TestInvariant_P03_S03_M02_INV_05_ConcurrentFreezeStability verifies that concurrent
// Freeze calls have one stable terminal outcome.
func TestInvariant_P03_S03_M02_INV_05_ConcurrentFreezeStability(t *testing.T) {
	sl := memtable.NewSkipList()
	var wg sync.WaitGroup
	var successCount atomic.Int32

	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if sl.Freeze() {
				successCount.Add(1)
			}
		}()
	}
	wg.Wait()

	if successCount.Load() != 1 {
		t.Fatalf("expected exactly 1 transition, got %d", successCount.Load())
	}
	if !sl.IsFrozen() {
		t.Fatalf("terminal state must be FROZEN")
	}
}

// TestInvariant_P03_S03_M02_INV_06_SearchConcurrentAcrossFreeze verifies that SearchConcurrent
// remains lock-free and race-free across the Freeze transition.
func TestInvariant_P03_S03_M02_INV_06_SearchConcurrentAcrossFreeze(t *testing.T) {
	sl := memtable.NewSkipList()
	ik := makeIK(t, "target-key", 42, binary.OpTypePut)
	_ = sl.Insert(ik, []byte("payload"))

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Continuous readers
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					val, err := sl.SearchConcurrent([]byte("target-key"))
					if err != nil || string(val) != "payload" {
						t.Errorf("SearchConcurrent read corrupted value: %s, %v", val, err)
						return
					}
				}
			}
		}()
	}

	sl.Freeze()
	close(stop)
	wg.Wait()
}

// TestInvariant_P03_S03_M02_INV_07_IteratorContinuityAcrossFreeze verifies that existing
// iterators can safely traverse the structure after Freeze.
func TestInvariant_P03_S03_M02_INV_07_IteratorContinuityAcrossFreeze(t *testing.T) {
	sl := memtable.NewSkipList()
	for i := 0; i < 10; i++ {
		_ = sl.Insert(makeIK(t, fmt.Sprintf("k%02d", i), uint64(i+1), binary.OpTypePut), []byte("v"))
	}

	it := sl.NewIterator()
	it.SeekToFirst()
	_ = it.Next() // at k01

	sl.Freeze()

	// Continue traversal after Freeze
	remaining := 0
	for it.Next() {
		remaining++
	}
	if remaining != 8 {
		t.Fatalf("expected 8 remaining nodes, got %d", remaining)
	}
}

// TestInvariant_P03_S03_M02_INV_08_CompleteStateImmutability verifies that after Freeze,
// all structural and value state is immutable.
func TestInvariant_P03_S03_M02_INV_08_CompleteStateImmutability(t *testing.T) {
	sl := memtable.NewSkipList()
	ik := makeIK(t, "fixed-key", 99, binary.OpTypePut)
	_ = sl.Insert(ik, []byte("immutable-value"))

	sl.Freeze()

	// Attempt exact duplicate swap
	err := sl.Insert(ik, []byte("mutated-value"))
	if !stdErrors.Is(err, errors.ErrMemTableFrozen) {
		t.Fatalf("expected ErrMemTableFrozen, got: %v", err)
	}

	// Verify value remains immutable
	val, err := sl.SearchConcurrent([]byte("fixed-key"))
	if err != nil || string(val) != "immutable-value" {
		t.Fatalf("value mutated post-freeze: val=%s, err=%v", val, err)
	}
}
