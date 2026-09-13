package memtable_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/memtable"
)

// TestIND004_ConcurrentWritersAndIterators verifies that concurrent writers (Insert)
// and readers traversing with Iterator are synchronized with sync.RWMutex without data races,
// deadlocks, or segmentation faults.
func TestIND004_ConcurrentWritersAndIterators(t *testing.T) {
	sl := memtable.NewSkipList()
	const numWriters = 8
	const numIterators = 8
	const writesPerWorker = 150

	startBarrier := make(chan struct{})
	var wg sync.WaitGroup
	var completedInserts atomic.Int64
	var completedScans atomic.Int64

	// Launch concurrent writers
	for w := 0; w < numWriters; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			<-startBarrier

			for i := 0; i < writesPerWorker; i++ {
				uKey := []byte(fmt.Sprintf("w%02d-key%04d", workerID, i))
				ik, err := binary.NewInternalKey(uKey, binary.SeqNum(i+1), binary.OpTypePut)
				if err != nil {
					continue
				}
				if err := sl.Insert(ik, []byte("val-data")); err == nil {
					completedInserts.Add(1)
				}
			}
		}(w)
	}

	// Launch concurrent iterator readers
	for r := 0; r < numIterators; r++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			<-startBarrier

			it := sl.NewIterator()
			defer it.Close()

			for scan := 0; scan < 20; scan++ {
				it.SeekToFirst()
				count := 0
				for it.Valid() {
					_ = it.Key()
					_ = it.Value()
					count++
					if count > numWriters*writesPerWorker {
						break
					}
					it.Next()
				}
				completedScans.Add(1)
			}
		}(r)
	}

	close(startBarrier)
	wg.Wait()

	if completedInserts.Load() == 0 {
		t.Fatalf("no inserts completed")
	}
	if completedScans.Load() == 0 {
		t.Fatalf("no scans completed")
	}
}
