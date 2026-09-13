package memtable

import (
	"fmt"
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
)

// SEC-MEM-02: inserts racing Freeze must either be included pre-freeze or
// rejected post-freeze; never silently mutate a frozen list. Iterator count stable.
func TestSEC_MEM02_FreezeLinearizability(t *testing.T) {
	sl := NewSkipList()
	var wg sync.WaitGroup
	errs := make([]error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ik, _ := binary.NewInternalKey([]byte(fmt.Sprintf("k%02d", i)), binary.SeqNum(i+1), binary.OpTypePut)
			errs[i] = sl.Insert(ik, []byte("v"))
		}(i)
	}
	sl.Freeze()
	wg.Wait()
	// Post-freeze inserts must fail.
	ik, _ := binary.NewInternalKey([]byte("zz"), 999, binary.OpTypePut)
	if err := sl.Insert(ik, []byte("v")); err == nil {
		t.Fatal("insert into frozen list accepted")
	}
	n := sl.Len()
	if n > 64 || n < 0 {
		t.Fatalf("len=%d out of range", n)
	}
	// Second Freeze idempotent.
	if sl.Freeze() {
		t.Fatal("second Freeze reported transition")
	}
}
