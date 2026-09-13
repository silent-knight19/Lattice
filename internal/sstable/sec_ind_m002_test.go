package sstable_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	stdErrors "errors"

	"github.com/silent-knight19/lattice/internal/sstable"
)

// h002FDCount returns the number of open file descriptors for this process.
// It uses /dev/fd (present on Linux and Darwin); other platforms skip.
func h002FDCount(t *testing.T) int {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fd counting via /dev/fd unavailable on windows")
	}
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skipf("/dev/fd unavailable: %v", err)
	}
	// Reading /dev/fd opens one transient descriptor for the read itself.
	return len(entries) - 1
}

// TestINDM002_FailedOpensLeakNoDescriptors proves the IND-M-002 verdict using
// the audit's exact scenario: repeatedly opening SSTables whose later
// components/validations fail must not accumulate file descriptors from the
// already-opened earlier stages.
func TestINDM002_FailedOpensLeakNoDescriptors(t *testing.T) {
	dir := t.TempDir()

	// Case 1: tiny files failing footer validation (post-open, post-fstat).
	tiny := filepath.Join(dir, "tiny.sst")
	if err := os.WriteFile(tiny, []byte{0xA5, 0x5A}, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	// Case 2: full-size bad-magic file failing footer decode (later stage).
	badmagic := filepath.Join(dir, "badmagic.sst")
	payload := make([]byte, sstable.FooterSize)
	for i := range payload {
		payload[i] = byte(i + 1)
	}
	if err := os.WriteFile(badmagic, payload, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	before := h002FDCount(t)
	const rounds = 200
	for i := 0; i < rounds; i++ {
		if r, err := sstable.NewTableReader(tiny); err == nil {
			_ = r.Close()
			t.Fatalf("round %d: tiny file unexpectedly accepted", i)
		}
		if r, err := sstable.NewTableReader(badmagic); err == nil {
			_ = r.Close()
			t.Fatalf("round %d: bad-magic file unexpectedly accepted", i)
		}
	}
	after := h002FDCount(t)
	if after != before {
		t.Fatalf("fd count drifted %d -> %d over %d failed opens (IND-M-002 leak)", before, after, 2*rounds)
	}
}

// TestINDM002_PostOpenHookFailureCloses exercises the fault-injection failure
// branch between open and validation: the descriptor must still be closed.
func TestINDM002_PostOpenHookFailureCloses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "victim.sst")
	if err := os.WriteFile(path, make([]byte, sstable.FooterSize), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	injected := stdErrors.New("simulated IND-M-002 post-open failure")
	restore := sstable.SetPostOpenHookForTesting(func() error {
		return injected
	})
	defer restore()

	before := h002FDCount(t)
	for i := 0; i < 50; i++ {
		if r, err := sstable.NewTableReader(path); err == nil {
			_ = r.Close()
			t.Fatalf("round %d: hook failure unexpectedly ignored", i)
		} else if !stdErrors.Is(err, injected) {
			t.Fatalf("round %d: want injected error, got %v", i, err)
		}
	}
	after := h002FDCount(t)
	if after != before {
		t.Fatalf("fd count drifted %d -> %d over hook-failed opens (IND-M-002 leak)", before, after)
	}
}
