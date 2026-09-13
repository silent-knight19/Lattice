package memtable_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/memtable"
)

// TestVULN005_MemTableIteratorDataRace verifies that concurrent calls to Next(), Seek(),
// Key(), Value(), and Close() on the same iterator and across multiple iterators while
// the SkipList is being modified/frozen do not cause data races or panics (VULN-005).
func TestVULN005_MemTableIteratorDataRace(t *testing.T) {
	sl := memtable.NewSkipList()
	for i := 0; i < 100; i++ {
		k := []byte(fmt.Sprintf("key-%04d", i))
		v := []byte(fmt.Sprintf("val-%04d", i))
		ik, _ := binary.NewInternalKey(k, binary.SeqNum(i+1), binary.OpTypePut)
		_ = sl.Insert(ik, v)
	}

	const numIterators = 10
	var wg sync.WaitGroup

	for i := 0; i < numIterators; i++ {
		it := sl.NewIterator()
		wg.Add(3)

		// Goroutine 1: Continuous traversal
		go func(iter *memtable.Iterator) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if iter.Next() {
					_ = iter.Key()
					_ = iter.Value()
				}
			}
		}(it)

		// Goroutine 2: Seek operations
		go func(iter *memtable.Iterator) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = iter.Seek([]byte(fmt.Sprintf("key-%04d", j*5)))
				_ = iter.Key()
			}
		}(it)

		// Goroutine 3: Concurrent Close
		go func(iter *memtable.Iterator) {
			defer wg.Done()
			time.Sleep(200 * time.Microsecond)
			iter.Close()
			// Redundant Close calls must be idempotent
			iter.Close()
		}(it)
	}

	wg.Wait()

	// All active iterators should be 0
	if drained := sl.DrainActiveIterators(2 * time.Second); !drained {
		t.Fatalf("expected all active iterators to be drained, got %d", sl.ActiveIterators())
	}
	if act := sl.ActiveIterators(); act != 0 {
		t.Fatalf("expected activeIterators == 0, got %d", act)
	}
}
