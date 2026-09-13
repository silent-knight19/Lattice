package sstable_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// h006BuildFinished builds a small SSTable at dst and finishes it.
func h006BuildFinished(t *testing.T, dst string, tag int) {
	t.Helper()
	w, err := sstable.NewTableWriter(dst, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("NewTableWriter(%s) failed: %v", dst, err)
	}
	for i := 0; i < 5; i++ {
		ik, err := binary.NewInternalKey(
			[]byte(fmt.Sprintf("t%d-key:%04d", tag, i)),
			binary.SeqNum(uint64(100-i)),
			binary.OpTypePut,
		)
		if err != nil {
			t.Fatalf("NewInternalKey failed: %v", err)
		}
		if err := w.Add(ik, []byte(fmt.Sprintf("v%d", i))); err != nil {
			t.Fatalf("Add failed: %v", err)
		}
	}
	if _, err := w.Finish(); err != nil {
		t.Fatalf("Finish(%s) failed: %v", dst, err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

// TestINDH006_PredictableNameSquatIgnored proves the IND-H-006 verdict at its
// root: staging names are unpredictable (os.CreateTemp with O_EXCL), so an
// attacker squatting the audit's predictable name (table-staging.data) — as a
// regular file or as a symlink to a victim — cannot hijack, stall, or redirect
// a flush. The flush succeeds; the squat and the victim are untouched.
func TestINDH006_PredictableNameSquatIgnored(t *testing.T) {
	dir := t.TempDir()
	squat := filepath.Join(dir, "table-staging.data")
	victim := filepath.Join(dir, "victim.dat")
	const sentinel = "attacker-target-sentinel"
	if err := os.WriteFile(victim, []byte(sentinel), 0600); err != nil {
		t.Fatalf("WriteFile victim failed: %v", err)
	}

	for _, squatKind := range []string{"regular", "symlink"} {
		_ = os.Remove(squat)
		if squatKind == "regular" {
			if err := os.WriteFile(squat, []byte("squat-placeholder"), 0600); err != nil {
				t.Fatalf("WriteFile squat failed: %v", err)
			}
		} else {
			if err := os.Symlink(victim, squat); err != nil {
				t.Skipf("symlinks not supported: %v", err)
			}
		}

		dst := filepath.Join(dir, fmt.Sprintf("flush-%s.sst", squatKind))
		h006BuildFinished(t, dst, len(squatKind))

		if _, err := os.Stat(dst); err != nil {
			t.Errorf("[%s] published SSTable missing: %v", squatKind, err)
		}
		content, err := os.ReadFile(victim)
		if err != nil {
			t.Fatalf("[%s] ReadFile victim failed: %v", squatKind, err)
		}
		if string(content) != sentinel {
			t.Errorf("[%s] victim = %q, want %q: flush followed squatted name", squatKind, content, sentinel)
		}
		_ = os.Remove(squat)
	}
}

// TestINDH006_RapidSymlinkChurnDuringFlush runs the audit's suggested
// regression scenario: flushes proceed while an external actor rapidly
// creates and removes symlinks with staging-like names. Every flush must
// complete promptly (no stall) and the victim must remain byte-identical.
func TestINDH006_RapidSymlinkChurnDuringFlush(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "churn-victim.dat")
	const sentinel = "churn-victim-sentinel"
	if err := os.WriteFile(victim, []byte(sentinel), 0600); err != nil {
		t.Fatalf("WriteFile victim failed: %v", err)
	}

	stop := make(chan struct{})
	var churned atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				link := filepath.Join(dir, fmt.Sprintf(".tmp_churn_%d", i%8))
				_ = os.Remove(link)
				_ = os.Symlink(victim, link)
				churned.Add(1)
				_ = os.Remove(link)
				i++
			}
		}
	}()

	const flushes = 10
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < flushes; i++ {
			h006BuildFinished(t, filepath.Join(dir, fmt.Sprintf("churn-%02d.sst", i)), 1000+i)
		}
	}()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		close(stop)
		wg.Wait()
		t.Fatalf("flushes stalled under symlink churn (IND-H-006 DoS)")
	}
	close(stop)
	wg.Wait()

	if churned.Load() == 0 {
		t.Fatalf("churn goroutine never ran; test is vacuous")
	}
	content, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("ReadFile victim failed: %v", err)
	}
	if string(content) != sentinel {
		t.Errorf("victim = %q, want %q: churn redirected a flush", content, sentinel)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	sstCount := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".sst" {
			sstCount++
		}
	}
	if sstCount != flushes {
		t.Errorf("published SSTables = %d, want %d", sstCount, flushes)
	}
}
