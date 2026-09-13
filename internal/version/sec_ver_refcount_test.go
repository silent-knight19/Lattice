package version

import (
	"math"
	"testing"
)

// SEC-VER-01: refcount uses Int64 CAS, rejects dead resurrection and MaxInt64 overflow.
func TestSEC_VER01_RefcountSafety(t *testing.T) {
	v := NewVersion([NumLevels][]FileMetadata{})
	if v.RefCount() != 1 {
		t.Fatalf("initial refcount = %d, want 1", v.RefCount())
	}
	v.Ref()
	v.Unref()
	// Drive to dead and verify resurrection panics / TryRef fails.
	v.Unref() // 1->0 finalize
	if v.TryRef() {
		t.Fatal("TryRef resurrected dead version")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("Ref on dead version did not panic")
			}
		}()
		v.Ref()
	}()
	// Overflow boundary: set to MaxInt64 directly (test seam via atomic store).
	v2 := NewVersion([NumLevels][]FileMetadata{})
	v2.refCount.Store(math.MaxInt64)
	if v2.TryRef() {
		t.Fatal("TryRef at MaxInt64 succeeded, want false")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("Ref at MaxInt64 did not panic")
			}
		}()
		v2.Ref()
	}()
}
