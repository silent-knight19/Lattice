package version_test

import (
	"math"
	"testing"

	"github.com/silent-knight19/lattice/internal/version"
)

// TestIND002_VersionSet_64BitRefCountOverflowProtection verifies that Version
// uses a 64-bit integer reference count, making 32-bit overflow impossible,
// and correctly enforces guards against math.MaxInt64 and underflow.
func TestIND002_VersionSet_64BitRefCountOverflowProtection(t *testing.T) {
	v := version.NewVersion([version.NumLevels][]version.FileMetadata{})

	// Initial refCount must be 1 (int64)
	if v.RefCount() != 1 {
		t.Fatalf("expected initial refCount = 1, got %d", v.RefCount())
	}

	// Verify refCount can exceed math.MaxInt32 without wraparound
	v.SetRefCountForTesting(int64(math.MaxInt32) + 1000)
	if v.RefCount() != int64(math.MaxInt32)+1000 {
		t.Fatalf("expected 64-bit refCount > MaxInt32: got %d", v.RefCount())
	}

	// Ref and Unref above 32-bit integer range
	v.Ref()
	if v.RefCount() != int64(math.MaxInt32)+1001 {
		t.Fatalf("Ref above MaxInt32 failed: got %d", v.RefCount())
	}
	v.Unref()
	if v.RefCount() != int64(math.MaxInt32)+1000 {
		t.Fatalf("Unref above MaxInt32 failed: got %d", v.RefCount())
	}

	// Guard at math.MaxInt64: TryRef returns false
	v.SetRefCountForTesting(math.MaxInt64)
	if v.TryRef() {
		t.Fatalf("expected TryRef() at MaxInt64 to fail")
	}

	// Underflow protection: Unref on dead version panics
	deadVersion := version.NewVersion([version.NumLevels][]version.FileMetadata{})
	deadVersion.Unref() // transitions 1 -> 0
	if deadVersion.RefCount() != 0 {
		t.Fatalf("expected deadVersion refCount == 0, got %d", deadVersion.RefCount())
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected Unref on dead version to panic with underflow")
		}
	}()
	deadVersion.Unref()
}
