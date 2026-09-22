package memtable_test

import (
	stdErrors "errors"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
)

func auditTestKey(t *testing.T) binary.InternalKey {
	t.Helper()
	k, err := binary.NewInternalKey([]byte("k"), 1, binary.OpTypePut)
	if err != nil {
		t.Fatalf("NewInternalKey failed: %v", err)
	}
	return k
}

// TestAudit_F005_ZeroSkipListNeverPanics is the regression test for
// AUDIT-F-005: the zero value (var s SkipList) previously panicked with a
// nil-pointer dereference on first use (nil head/rnd). Post-fix, mutating and
// read operations report errors.ErrNotInitialized and iterator construction
// returns nil.
func TestAudit_F005_ZeroSkipListNeverPanics(t *testing.T) {
	var s memtable.SkipList
	key := auditTestKey(t)

	if err := s.Insert(key, []byte("v")); !stdErrors.Is(err, errors.ErrNotInitialized) {
		t.Errorf("zero Insert() = %v, want ErrNotInitialized", err)
	}
	if _, err := s.Search([]byte("k")); !stdErrors.Is(err, errors.ErrNotInitialized) {
		t.Errorf("zero Search() = %v, want ErrNotInitialized", err)
	}
	if _, err := s.SearchConcurrent([]byte("k")); !stdErrors.Is(err, errors.ErrNotInitialized) {
		t.Errorf("zero SearchConcurrent() = %v, want ErrNotInitialized", err)
	}
	if it := s.NewIterator(); it != nil {
		t.Errorf("zero NewIterator() = %v, want nil", it)
	}
	if s.Freeze() {
		t.Errorf("zero Freeze() = true, want false")
	}

	// Value-returning accessors on the zero value must remain panic-free.
	_ = s.Height()
	_ = s.Len()
	_ = s.ByteSize()
	_ = s.IsEmpty()
	_ = s.IsFrozen()
	_ = s.ActiveIterators()
	if !s.DrainActiveIterators(0) {
		t.Errorf("zero DrainActiveIterators() = false, want true")
	}
}

// TestAudit_F005_NilGeneratorsAreSafe covers nil *PCG32 / *HeightGenerator /
// zero HeightGenerator: heights must stay within [MinHeight, MaxHeight] and
// never panic, preserving the documented RandomHeight invariant.
func TestAudit_F005_NilGeneratorsAreSafe(t *testing.T) {
	var pcg *memtable.PCG32
	if got := pcg.Uint32(); got != 0 {
		t.Errorf("nil PCG32.Uint32() = %d, want 0", got)
	}

	var hg *memtable.HeightGenerator
	if got := hg.RandomHeight(); got != memtable.MinHeight {
		t.Errorf("nil HeightGenerator.RandomHeight() = %d, want MinHeight", got)
	}

	var zeroHG memtable.HeightGenerator
	for i := 0; i < 100; i++ {
		if got := zeroHG.RandomHeight(); got != memtable.MinHeight {
			t.Fatalf("zero HeightGenerator.RandomHeight() = %d, want MinHeight", got)
		}
	}
}
