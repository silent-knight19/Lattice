package sstable_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	stdErrors "errors"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// countStagingFiles returns the names of leftover `.tmp_*` staging files in dir.
func countStagingFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	var leftovers []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp_") {
			leftovers = append(leftovers, e.Name())
		}
	}
	return leftovers
}

func buildH002Writer(t *testing.T, dst string, n int) *sstable.TableWriter {
	t.Helper()
	w, err := sstable.NewTableWriter(dst, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}
	for i := 0; i < n; i++ {
		ik, err := binary.NewInternalKey([]byte(fmt.Sprintf("key:%04d", i)), binary.SeqNum(uint64(100-i)), binary.OpTypePut)
		if err != nil {
			t.Fatalf("NewInternalKey failed: %v", err)
		}
		if err := w.Add(ik, []byte(fmt.Sprintf("val:%04d", i))); err != nil {
			t.Fatalf("Add %d failed: %v", i, err)
		}
	}
	return w
}

// TestINDH002_SuccessLeavesNoStagingResidue proves the IND-H-002 baseline: a
// successful flush publishes exactly one file and leaves zero staging files
// behind for compaction to miss and disk usage to leak.
func TestINDH002_SuccessLeavesNoStagingResidue(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "000001.sst")

	w := buildH002Writer(t, dst, 20)
	if _, err := w.Finish(); err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close after Finish failed: %v", err)
	}

	if leftovers := countStagingFiles(t, dir); len(leftovers) != 0 {
		t.Errorf("orphaned staging files after success: %v", leftovers)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("published SSTable missing: %v", err)
	}
}

// TestINDH002_LinkFailureCleansUpStaging proves crash-safety of the publish
// step: if atomic publication fails, the error surfaces, no partial file
// appears at the destination, and staging is removed (no orphans).
func TestINDH002_LinkFailureCleansUpStaging(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "000002.sst")

	w := buildH002Writer(t, dst, 20)
	injected := stdErrors.New("simulated IND-H-002 link device failure")
	w.SetLinkFnForTesting(func(oldname, newname string) error {
		return injected
	})

	if _, err := w.Finish(); err == nil {
		t.Fatalf("Finish succeeded despite link failure")
	} else if !stdErrors.Is(err, injected) {
		t.Fatalf("Finish error does not wrap root cause: %v", err)
	}
	if _, statErr := os.Lstat(dst); !os.IsNotExist(statErr) {
		t.Errorf("partial destination file exists after failed publish")
	}
	if leftovers := countStagingFiles(t, dir); len(leftovers) != 0 {
		t.Errorf("orphaned staging files after link failure: %v", leftovers)
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close after failed Finish failed: %v", err)
	}
	if leftovers := countStagingFiles(t, dir); len(leftovers) != 0 {
		t.Errorf("orphaned staging files after Close: %v", leftovers)
	}
}

// TestINDH002_ConcurrentDestinationAppearanceNeverOverwrites exercises the
// TOCTOU window between staging and publication: a file appearing at the
// destination mid-flush must abort the publish with ErrSSTableExists while
// preserving the victim's bytes exactly.
func TestINDH002_ConcurrentDestinationAppearanceNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "000003.sst")

	w := buildH002Writer(t, dst, 20)
	const sentinel = "concurrent-victim-sentinel"
	w.SetPreLinkHookForTesting(func() error {
		// Simulate a concurrent writer's file appearing at dst after our
		// staging completed but before our publication check.
		if err := os.WriteFile(dst, []byte(sentinel), 0600); err != nil {
			return err
		}
		return nil
	})

	if _, err := w.Finish(); err == nil {
		t.Fatalf("Finish succeeded despite destination appearing mid-flush")
	} else if !stdErrors.Is(err, errors.ErrSSTableExists) {
		t.Fatalf("expected ErrSSTableExists, got %v", err)
	}
	content, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile victim failed: %v", err)
	}
	if string(content) != sentinel {
		t.Errorf("victim = %q, want %q: publish overwrote concurrent file", content, sentinel)
	}
	if leftovers := countStagingFiles(t, dir); len(leftovers) != 0 {
		t.Errorf("orphaned staging files after aborted publish: %v", leftovers)
	}
	_ = w.Close()
}
