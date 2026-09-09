package memtable_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
)

// TestSkipList_Invariant_Conc_01_AtomicPointerLoads verifies P03-S02-M01-INV-01:
// Every reader-visible forward-pointer load is atomic.
func TestSkipList_Invariant_Conc_01_AtomicPointerLoads(t *testing.T) {
	sl := memtable.NewSkipList()
	for i := 0; i < 20; i++ {
		k := sampleKey(t, fmt.Sprintf("k:%02d", i), uint64(i+1), binary.OpTypePut)
		if err := sl.Insert(k, []byte("val")); err != nil {
			t.Fatalf("insert failed: %v", err)
		}
	}

	// Traversal across all levels must succeed using ForwardAtForTesting (which uses atomic.Pointer.Load)
	head := sl.HeadForTesting()
	for lvl := 0; lvl < memtable.MaxHeight; lvl++ {
		curr, err := head.ForwardAtForTesting(lvl)
		if err != nil {
			t.Fatalf("atomic load failed at level %d: %v", lvl, err)
		}
		for curr != nil {
			curr, err = curr.ForwardAtForTesting(lvl)
			if err != nil {
				t.Fatalf("atomic load failed: %v", err)
			}
		}
	}
}

// TestSkipList_Invariant_Conc_02_FullInitializationBeforePublication verifies P03-S02-M01-INV-02:
// A published node is fully initialized before becoming reader-visible.
func TestSkipList_Invariant_Conc_02_FullInitializationBeforePublication(t *testing.T) {
	sl := memtable.NewSkipList()
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Reader checks that every node encountered has valid key, valid height, and valid value
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					for i := 0; i < 100; i++ {
						uKey := []byte(fmt.Sprintf("init-key-%04d", i))
						val, err := sl.SearchConcurrent(uKey)
						if err == nil {
							// Value must be fully populated
							if len(val) == 0 {
								t.Errorf("observed uninitialized/empty value for published node %s", uKey)
							}
						}
					}
				}
			}
		}()
	}

	for i := 0; i < 100; i++ {
		k := sampleKey(t, fmt.Sprintf("init-key-%04d", i), uint64(i+1), binary.OpTypePut)
		if err := sl.Insert(k, []byte(fmt.Sprintf("val-%04d", i))); err != nil {
			t.Fatalf("insert failed: %v", err)
		}
	}

	close(stop)
	wg.Wait()
}

// TestSkipList_Invariant_Conc_03_ImmutabilityAfterPublication verifies P03-S02-M01-INV-03:
// Published immutable node fields are never mutated non-atomically after publication.
func TestSkipList_Invariant_Conc_03_ImmutabilityAfterPublication(t *testing.T) {
	sl := memtable.NewSkipList()
	k := sampleKey(t, "immutable-key", 100, binary.OpTypePut)
	if err := sl.Insert(k, []byte("val-init")); err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	node := sl.SearchNodeForTesting([]byte("immutable-key"))
	if node == nil {
		t.Fatalf("expected node to exist")
	}

	keyBefore := node.KeyForTesting()
	hBefore := node.HeightForTesting()

	// Insert other nodes, including duplicates
	for i := 0; i < 50; i++ {
		otherKey := sampleKey(t, fmt.Sprintf("other:%02d", i), uint64(i+1), binary.OpTypePut)
		_ = sl.Insert(otherKey, []byte("val"))
	}
	_ = sl.Insert(k, []byte("val-updated"))

	// Node's key and height must be strictly unchanged
	if binary.CompareInternalKey(node.KeyForTesting(), keyBefore) != 0 {
		t.Errorf("node key was mutated after publication: before=%v, after=%v", keyBefore, node.KeyForTesting())
	}
	if node.HeightForTesting() != hBefore {
		t.Errorf("node height was mutated after publication: before=%d, after=%d", hBefore, node.HeightForTesting())
	}
}

// TestSkipList_Invariant_Conc_04_LockFreeReaderIndependence verifies P03-S02-M01-INV-04:
// Concurrent readers never require the writer mutation lock for forward traversal.
func TestSkipList_Invariant_Conc_04_LockFreeReaderIndependence(t *testing.T) {
	sl := memtable.NewSkipList()
	k := sampleKey(t, "independent-key", 1, binary.OpTypePut)
	_ = sl.Insert(k, []byte("val-independent"))

	sl.WriterLockForTesting()

	done := make(chan struct{})
	go func() {
		val, err := sl.SearchConcurrent([]byte("independent-key"))
		if err != nil || string(val) != "val-independent" {
			t.Errorf("lock-free search failed: val=%s, err=%v", val, err)
		}
		close(done)
	}()

	select {
	case <-done:
		// Success: reader executed without needing writer lock
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("reader hung waiting on writer lock: P03-S02-M01-INV-04 violated")
	}

	sl.WriterUnlockForTesting()
}

// TestSkipList_Invariant_Conc_05_RaceFreeHeightObservation verifies P03-S02-M01-INV-05:
// Concurrent active-height observation is race-free.
func TestSkipList_Invariant_Conc_05_RaceFreeHeightObservation(t *testing.T) {
	sl := memtable.NewSkipList()
	stop := make(chan struct{})
	var wg sync.WaitGroup

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					h := sl.Height()
					if h < memtable.MinHeight || h > memtable.MaxHeight {
						t.Errorf("active height out of bounds: %d", h)
					}
				}
			}
		}()
	}

	for i := 1; i <= 16; i++ {
		k := sampleKey(t, fmt.Sprintf("k-h-%02d", i), uint64(i), binary.OpTypePut)
		_ = sl.InsertWithHeightForTesting(k, []byte("v"), i)
	}

	close(stop)
	wg.Wait()
}

// TestSkipList_Invariant_Conc_06_NoPartiallyInitializedNodes verifies P03-S02-M01-INV-06:
// Concurrent reader traversal cannot observe a partially initialized node.
func TestSkipList_Invariant_Conc_06_NoPartiallyInitializedNodes(t *testing.T) {
	sl := memtable.NewSkipList()
	stop := make(chan struct{})
	var wg sync.WaitGroup

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					for i := 0; i < 50; i++ {
						uKey := []byte(fmt.Sprintf("partial-check:%03d", i))
						val, err := sl.SearchConcurrent(uKey)
						if err == nil {
							if len(val) == 0 || !bytes.HasPrefix(val, []byte("full-data-")) {
								t.Errorf("reader observed partially initialized data for %s: %q", uKey, val)
							}
						}
					}
				}
			}
		}()
	}

	for i := 0; i < 50; i++ {
		k := sampleKey(t, fmt.Sprintf("partial-check:%03d", i), uint64(i+1), binary.OpTypePut)
		v := []byte(fmt.Sprintf("full-data-%03d", i))
		if err := sl.Insert(k, v); err != nil {
			t.Fatalf("insert failed: %v", err)
		}
	}

	close(stop)
	wg.Wait()
}

// TestSkipList_Invariant_Conc_07_OrderingPreservationUnderConcurrency verifies P03-S02-M01-INV-07:
// Concurrent insertion preserves canonical ordering across all levels.
func TestSkipList_Invariant_Conc_07_OrderingPreservationUnderConcurrency(t *testing.T) {
	sl := memtable.NewSkipList()
	stop := make(chan struct{})
	var wg sync.WaitGroup

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = sl.SearchConcurrent([]byte("order-test-key"))
				}
			}
		}()
	}

	rng := memtable.NewPCG32(0x5555, 0xAAAA)
	for i := 0; i < 200; i++ {
		keyNum := rng.Uint32() % 40
		k := sampleKey(t, fmt.Sprintf("k:%04d", keyNum), uint64(i+1), binary.OpTypePut)
		if err := sl.Insert(k, []byte("val")); err != nil {
			t.Fatalf("insert failed: %v", err)
		}
	}

	close(stop)
	wg.Wait()

	if err := sl.ValidateStructureForTesting(); err != nil {
		t.Fatalf("canonical ordering violated after concurrent operations: %v", err)
	}
}

// TestSkipList_Invariant_Conc_08_ValidLogicalStates verifies P03-S02-M01-INV-08:
// Concurrent searches return only valid logical states (either valid old or valid new).
func TestSkipList_Invariant_Conc_08_ValidLogicalStates(t *testing.T) {
	sl := memtable.NewSkipList()
	uKey := []byte("state-key")

	stop := make(chan struct{})
	var wg sync.WaitGroup

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					val, err := sl.SearchConcurrent(uKey)
					if err == nil {
						// Must match pattern v-%d
						if !bytes.HasPrefix(val, []byte("v-")) {
							t.Errorf("impossible logical state observed: %q", val)
						}
					} else if !stdErrors.Is(err, errors.ErrKeyNotFound) {
						t.Errorf("unexpected error: %v", err)
					}
				}
			}
		}()
	}

	for i := 1; i <= 50; i++ {
		k := sampleKey(t, "state-key", uint64(i), binary.OpTypePut)
		if err := sl.Insert(k, []byte(fmt.Sprintf("v-%d", i))); err != nil {
			t.Fatalf("insert %d failed: %v", i, err)
		}
	}

	close(stop)
	wg.Wait()
}

// TestSkipList_Invariant_Conc_09_AcyclicTraversalUnderConcurrency verifies P03-S02-M01-INV-09:
// Concurrent traversal never creates or follows cycles.
func TestSkipList_Invariant_Conc_09_AcyclicTraversalUnderConcurrency(t *testing.T) {
	sl := memtable.NewSkipList()
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Readers search during continuous insertions
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					done := make(chan struct{})
					go func() {
						_, _ = sl.SearchConcurrent([]byte("cyclic-check-target"))
						close(done)
					}()
					select {
					case <-done:
						// Terminated normally without infinite loop
					case <-time.After(1 * time.Second):
						t.Errorf("search hung! Cycle encountered during concurrent traversal")
						return
					}
				}
			}
		}()
	}

	for i := 0; i < 150; i++ {
		k := sampleKey(t, fmt.Sprintf("node-%04d", i), uint64(i+1), binary.OpTypePut)
		_ = sl.Insert(k, []byte("v"))
	}

	close(stop)
	wg.Wait()

	if err := sl.ValidateStructureForTesting(); err != nil {
		t.Fatalf("structural acyclicity check failed: %v", err)
	}
}

// TestSkipList_Invariant_Conc_10_SemanticEquivalencePostCompletion verifies P03-S02-M01-INV-10:
// After writer completion, Search and SearchConcurrent are semantically equivalent.
func TestSkipList_Invariant_Conc_10_SemanticEquivalencePostCompletion(t *testing.T) {
	sl := memtable.NewSkipList()

	for i := 0; i < 50; i++ {
		op := binary.OpTypePut
		var val []byte
		if i%5 == 0 {
			op = binary.OpTypeDelete
		} else {
			val = []byte(fmt.Sprintf("payload-%d", i))
		}
		k := sampleKey(t, fmt.Sprintf("eq-key-%02d", i), uint64(i+1), op)
		_ = sl.Insert(k, val)
	}

	// Compare Search vs SearchConcurrent for all existing and absent keys
	for i := 0; i < 70; i++ {
		uKey := []byte(fmt.Sprintf("eq-key-%02d", i))
		val1, err1 := sl.Search(uKey)
		val2, err2 := sl.SearchConcurrent(uKey)

		if !stdErrors.Is(err1, err2) {
			t.Errorf("key %s error divergence: Search=%v, SearchConcurrent=%v", uKey, err1, err2)
		}
		if !bytes.Equal(val1, val2) {
			t.Errorf("key %s value divergence: Search=%q, SearchConcurrent=%q", uKey, val1, val2)
		}
	}
}
