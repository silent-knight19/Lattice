package memtable_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
	"github.com/silent-knight19/lattice/internal/wal"
)

// hostileAllZeroSource returns 0 for every Uint32 call, which causes (val & 3 == 0)
// to succeed on every iteration, attempting to force an unbounded tower height.
type hostileAllZeroSource struct{}

func (h *hostileAllZeroSource) Uint32() uint32 {
	return 0
}

// hostileMaxUintSource returns math.MaxUint32, attempting to trigger sign or overflow errors.
type hostileMaxUintSource struct{}

func (h *hostileMaxUintSource) Uint32() uint32 {
	return math.MaxUint32
}

// TestAudit_SEC_P03_01_HostileHeightGenerator verifies that pathological or hostile
// random number generators cannot force tower height beyond MaxHeight (16) or loop infinitely.
func TestAudit_SEC_P03_01_HostileHeightGenerator(t *testing.T) {
	// 1. Hostile source attempting to promote infinitely
	zeroGen := memtable.NewHeightGenerator(&hostileAllZeroSource{})
	for i := 0; i < 1000; i++ {
		h := zeroGen.RandomHeight()
		if h != memtable.MaxHeight {
			t.Fatalf("expected clamped MaxHeight %d, got %d", memtable.MaxHeight, h)
		}
	}

	// 2. Hostile source with all bits set (never promotes past MinHeight)
	maxGen := memtable.NewHeightGenerator(&hostileMaxUintSource{})
	for i := 0; i < 1000; i++ {
		h := maxGen.RandomHeight()
		if h != memtable.MinHeight {
			t.Fatalf("expected MinHeight %d, got %d", memtable.MinHeight, h)
		}
	}

	// 3. SkipList backed by hostile generator operates safely
	sl := memtable.NewSkipListWithGenerator(zeroGen)
	for i := 1; i <= 50; i++ {
		key, err := binary.NewInternalKey([]byte(fmt.Sprintf("key-%04d", i)), binary.SeqNum(i), binary.OpTypePut)
		if err != nil {
			t.Fatalf("unexpected key error: %v", err)
		}
		if err := sl.Insert(key, []byte("val")); err != nil {
			t.Fatalf("unexpected insert error: %v", err)
		}
	}

	if sl.Height() > memtable.MaxHeight {
		t.Fatalf("active height %d exceeded MaxHeight %d", sl.Height(), memtable.MaxHeight)
	}
	if err := sl.ValidateStructureForTesting(); err != nil {
		t.Fatalf("structural integrity violated under hostile generator: %v", err)
	}
}

// TestAudit_SEC_P03_02_MemoryAccountingDriftAndSaturation verifies exact byte-level
// accounting across duplicate updates, deletions, and saturation arithmetic boundaries.
func TestAudit_SEC_P03_02_MemoryAccountingDriftAndSaturation(t *testing.T) {
	sl := memtable.NewSkipList()

	key, err := binary.NewInternalKey([]byte("account-key"), 100, binary.OpTypePut)
	if err != nil {
		t.Fatalf("key error: %v", err)
	}

	initialVal := []byte("12345678") // 8 bytes
	if err := sl.Insert(key, initialVal); err != nil {
		t.Fatalf("insert error: %v", err)
	}

	initialByteSize := sl.ByteSize()
	if initialByteSize == 0 {
		t.Fatalf("expected non-zero initial byte size")
	}

	// Exact duplicate with larger payload (+10 bytes)
	largerVal := []byte("123456789012345678") // 18 bytes
	if err := sl.Insert(key, largerVal); err != nil {
		t.Fatalf("insert error: %v", err)
	}
	if sl.ByteSize() != initialByteSize+10 {
		t.Fatalf("expected byte size %d, got %d", initialByteSize+10, sl.ByteSize())
	}

	// Exact duplicate with smaller payload (-14 bytes from larger)
	smallerVal := []byte("1234") // 4 bytes
	if err := sl.Insert(key, smallerVal); err != nil {
		t.Fatalf("insert error: %v", err)
	}
	if sl.ByteSize() != initialByteSize-4 {
		t.Fatalf("expected byte size %d, got %d", initialByteSize-4, sl.ByteSize())
	}

	// Exact duplicate with identical payload (delta = 0)
	if err := sl.Insert(key, smallerVal); err != nil {
		t.Fatalf("insert error: %v", err)
	}
	if sl.ByteSize() != initialByteSize-4 {
		t.Fatalf("expected unchanged byte size %d, got %d", initialByteSize-4, sl.ByteSize())
	}

	// Test saturation arithmetic bounds
	var counter atomic.Uint64
	counter.Store(math.MaxUint64 - 5)
	memtable.SafeAddUint64ForTesting(&counter, 10)
	if counter.Load() != math.MaxUint64 {
		t.Fatalf("expected safeAdd to saturate at MaxUint64, got %d", counter.Load())
	}

	counter.Store(5)
	memtable.SafeSubUint64ForTesting(&counter, 10)
	if counter.Load() != 0 {
		t.Fatalf("expected safeSub to saturate at 0, got %d", counter.Load())
	}
}

// TestAudit_SEC_P03_03_FreezeLinearizationUnderAdversarialConcurrency creates high-frequency
// concurrent insertions, lock-free searches, iterators, and an asynchronous Freeze event,
// verifying zero lost writes prior to freeze, zero mutations accepted post-freeze,
// and structural invariance post-freeze.
func TestAudit_SEC_P03_03_FreezeLinearizationUnderAdversarialConcurrency(t *testing.T) {
	sl := memtable.NewSkipList()
	const numWriters = 16
	const numReaders = 16
	const writesPerWorker = 300

	startBarrier := make(chan struct{})
	var wg sync.WaitGroup

	var acceptedWrites atomic.Int64
	var rejectedWrites atomic.Int64

	// Launch writers
	for w := 0; w < numWriters; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			<-startBarrier

			for i := 0; i < writesPerWorker; i++ {
				uKey := []byte(fmt.Sprintf("worker-%02d-key-%04d", workerID, i))
				ik, err := binary.NewInternalKey(uKey, binary.SeqNum(i+1), binary.OpTypePut)
				if err != nil {
					continue
				}

				insertErr := sl.Insert(ik, []byte("data"))
				if insertErr == nil {
					acceptedWrites.Add(1)
				} else if stdErrors.Is(insertErr, errors.ErrMemTableFrozen) {
					rejectedWrites.Add(1)
				}
			}
		}(w)
	}

	// Launch lock-free readers
	stopReaders := make(chan struct{})
	var readerPanics atomic.Int64

	for r := 0; r < numReaders; r++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			<-startBarrier

			defer func() {
				if rec := recover(); rec != nil {
					readerPanics.Add(1)
				}
			}()

			for {
				select {
				case <-stopReaders:
					return
				default:
					target := []byte(fmt.Sprintf("worker-%02d-key-%04d", readerID%numWriters, 50))
					_, _ = sl.SearchConcurrent(target)
				}
			}
		}(r)
	}

	// Start all concurrent goroutines
	close(startBarrier)

	// Trigger Freeze during active concurrency
	transitioned := sl.Freeze()
	if !transitioned && !sl.IsFrozen() {
		t.Fatalf("Freeze transition failed")
	}

	// Stop readers and wait for all goroutines to finish
	close(stopReaders)
	wg.Wait()

	if readerPanics.Load() > 0 {
		t.Fatalf("CONCURRENCY FAILURE: %d readers panicked during concurrent freeze!", readerPanics.Load())
	}

	// Post-freeze invariants
	frozenCount := sl.Len()
	frozenByteSize := sl.ByteSize()
	frozenHeight := sl.Height()

	if int64(frozenCount) != acceptedWrites.Load() {
		t.Fatalf("LINEARIZATION ERROR: SkipList count %d != accepted writes %d (rejected: %d)",
			frozenCount, acceptedWrites.Load(), rejectedWrites.Load())
	}

	// Post-freeze mutation attempt MUST fail
	postKey, _ := binary.NewInternalKey([]byte("post-freeze-key"), 9999, binary.OpTypePut)
	if err := sl.Insert(postKey, []byte("val")); !stdErrors.Is(err, errors.ErrMemTableFrozen) {
		t.Fatalf("expected ErrMemTableFrozen on post-freeze insert, got: %v", err)
	}

	// Duplicate update on frozen table MUST fail
	if acceptedWrites.Load() > 0 {
		dupKey, _ := binary.NewInternalKey([]byte("worker-00-key-0000"), 1, binary.OpTypePut)
		if err := sl.Insert(dupKey, []byte("overwrite")); !stdErrors.Is(err, errors.ErrMemTableFrozen) {
			t.Fatalf("expected ErrMemTableFrozen on post-freeze duplicate update, got: %v", err)
		}
	}

	// Invariance check: state must not drift
	if sl.Len() != frozenCount || sl.ByteSize() != frozenByteSize || sl.Height() != frozenHeight {
		t.Fatalf("INVARIANCE VIOLATION: frozen metadata changed after rejected mutations!")
	}

	// Verify structure is 100% acyclic and canonical
	if err := sl.ValidateStructureForTesting(); err != nil {
		t.Fatalf("structural integrity violated post-freeze: %v", err)
	}
}

// TestAudit_SEC_P03_04_IteratorAcyclicityAndTerminationUnderLiveWriters verifies that
// live iterators traversing a mutating SkipList never encounter cycles, infinite loops,
// or order inversions.
func TestAudit_SEC_P03_04_IteratorAcyclicityAndTerminationUnderLiveWriters(t *testing.T) {
	sl := memtable.NewSkipList()
	const numInsertions = 500
	const numIterators = 8

	startBarrier := make(chan struct{})
	var wg sync.WaitGroup

	// Writer goroutine inserting sequential keys
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-startBarrier

		for i := 0; i < numInsertions; i++ {
			k, _ := binary.NewInternalKey([]byte(fmt.Sprintf("k-%05d", i)), binary.SeqNum(i+1), binary.OpTypePut)
			_ = sl.Insert(k, []byte("data"))
		}
	}()

	// Reader iterators performing scans
	var iteratorFailures atomic.Int64

	for itIdx := 0; itIdx < numIterators; itIdx++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-startBarrier

			it := sl.NewIterator()
			defer it.Close()

			for scan := 0; scan < 20; scan++ {
				it.SeekToFirst()
				steps := 0
				var prevKey binary.InternalKey
				hasPrev := false

				for it.Valid() {
					steps++
					if steps > numInsertions*2 {
						iteratorFailures.Add(1)
						return
					}

					currKey := it.Key()
					if hasPrev {
						if binary.CompareInternalKey(prevKey, currKey) >= 0 {
							iteratorFailures.Add(1)
							return
						}
					}
					prevKey = currKey
					hasPrev = true

					it.Next()
				}
			}
		}(itIdx)
	}

	close(startBarrier)
	wg.Wait()

	if iteratorFailures.Load() > 0 {
		t.Fatalf("ITERATOR FAILURE: %d iterator violations (cycles or order inversions) detected under live mutation!", iteratorFailures.Load())
	}
}

// TestAudit_SEC_P03_05_EmptyValueVsTombstoneSemantics evaluates the subtle distinction
// between Put with empty value ([]byte{}) and Delete (tombstone, nil value).
func TestAudit_SEC_P03_05_EmptyValueVsTombstoneSemantics(t *testing.T) {
	sl := memtable.NewSkipList()

	// 1. Insert Put with empty slice []byte{}
	putKey, err := binary.NewInternalKey([]byte("empty-put"), 1, binary.OpTypePut)
	if err != nil {
		t.Fatalf("key error: %v", err)
	}
	if err := sl.Insert(putKey, []byte{}); err != nil {
		t.Fatalf("insert error: %v", err)
	}

	// Search must find the key (err == nil) even though value is nil/empty
	val, err := sl.SearchConcurrent([]byte("empty-put"))
	if err != nil {
		t.Fatalf("expected empty Put to be found, got error: %v", err)
	}
	if len(val) != 0 {
		t.Fatalf("expected len(val) == 0, got %d", len(val))
	}

	// 2. Insert Delete with nil
	delKey, err := binary.NewInternalKey([]byte("tombstone-key"), 2, binary.OpTypeDelete)
	if err != nil {
		t.Fatalf("key error: %v", err)
	}
	if err := sl.Insert(delKey, nil); err != nil {
		t.Fatalf("insert error: %v", err)
	}

	// Search must return ErrKeyNotFound for tombstone
	_, err = sl.SearchConcurrent([]byte("tombstone-key"))
	if !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound for tombstone, got: %v", err)
	}

	// Iterator must expose both, with tombstone identified via OpType
	it := sl.NewIterator()
	defer it.Close()

	it.SeekToFirst()
	foundPut := false
	foundDel := false

	for it.Valid() {
		k := it.Key()
		if bytes.Equal(k.UserKey, []byte("empty-put")) {
			foundPut = true
			if k.OpType != binary.OpTypePut {
				t.Fatalf("expected OpTypePut for empty-put")
			}
		}
		if bytes.Equal(k.UserKey, []byte("tombstone-key")) {
			foundDel = true
			if k.OpType != binary.OpTypeDelete {
				t.Fatalf("expected OpTypeDelete for tombstone-key")
			}
			if it.Value() != nil {
				t.Fatalf("expected nil value for tombstone")
			}
		}
		it.Next()
	}

	if !foundPut || !foundDel {
		t.Fatalf("failed to observe both entries in iterator (foundPut=%v, foundDel=%v)", foundPut, foundDel)
	}
}

// TestAudit_SEC_P03_06_InternalKeyStringInformationDisclosure tests whether InternalKey.String()
// discloses raw binary or sensitive user key bytes.
func TestAudit_SEC_P03_06_InternalKeyStringInformationDisclosure(t *testing.T) {
	secretPayload := []byte("SESSION_TOKEN_SUPER_SECRET_987654321")
	ik, err := binary.NewInternalKey(secretPayload, 42, binary.OpTypePut)
	if err != nil {
		t.Fatalf("key error: %v", err)
	}

	strRep := ik.String()
	// InternalKey.String() exposes the raw key bytes in quotes for debug purposes:
	if !bytes.Contains([]byte(strRep), secretPayload) {
		t.Fatalf("expected raw secret payload to be present in InternalKey.String()")
	}

	// Remediated via SEC-P03-WEAK-01: RedactedString() and Redact() strictly mask the user key payload:
	redactedStr := ik.RedactedString()
	if bytes.Contains([]byte(redactedStr), secretPayload) {
		t.Fatalf("DEFECT: InternalKey.RedactedString() leaked secret payload: %s", redactedStr)
	}
	redactedView, ok := ik.Redact().(binary.InternalKeyLogView)
	if !ok || redactedView.UserKeyLen != len(secretPayload) {
		t.Fatalf("DEFECT: InternalKey.Redact() invalid view: %+v", ik.Redact())
	}

	// Compare with Record.String() in WAL package which masks payload
	walRec := wal.Record{
		Type:   wal.RecordTypePut,
		SeqNum: ik.SeqNum,
		Key:    ik.UserKey,
		Value:  []byte("secret_value"),
	}
	walStr := walRec.String()
	if bytes.Contains([]byte(walStr), secretPayload) {
		t.Fatalf("DEFECT: WAL Record.String() leaked user key!")
	}
	if bytes.Contains([]byte(walStr), []byte("secret_value")) {
		t.Fatalf("DEFECT: WAL Record.String() leaked value!")
	}
}

// TestAudit_SEC_P03_07_DefensiveCopiesCallerIsolation proves that mutating external
// slices before/after Insert or after Search/Iterator never corrupts stored SkipList state.
func TestAudit_SEC_P03_07_DefensiveCopiesCallerIsolation(t *testing.T) {
	sl := memtable.NewSkipList()

	rawKey := []byte("shared-key")
	rawVal := []byte("shared-val")

	ik, _ := binary.NewInternalKey(rawKey, 1, binary.OpTypePut)
	_ = sl.Insert(ik, rawVal)

	// Mutate caller buffers
	rawKey[0] = 'X'
	rawVal[0] = 'X'

	// Verify SkipList still holds original values
	readVal, err := sl.SearchConcurrent([]byte("shared-key"))
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if string(readVal) != "shared-val" {
		t.Fatalf("Caller mutation corrupted stored value: got %q", readVal)
	}

	// Mutate returned slice
	readVal[0] = 'Z'
	readVal2, _ := sl.SearchConcurrent([]byte("shared-key"))
	if string(readVal2) != "shared-val" {
		t.Fatalf("Returned slice mutation corrupted internal value: got %q", readVal2)
	}

	// Iterator caller mutation isolation
	it := sl.NewIterator()
	defer it.Close()
	it.SeekToFirst()
	if !it.Valid() {
		t.Fatalf("iterator should be valid")
	}

	itKey := it.Key()
	itVal := it.Value()
	itKey.UserKey[0] = 'Q'
	itVal[0] = 'Q'

	readVal3, _ := sl.SearchConcurrent([]byte("shared-key"))
	if string(readVal3) != "shared-val" {
		t.Fatalf("Iterator defensive copy mutation corrupted internal value: got %q", readVal3)
	}
}
