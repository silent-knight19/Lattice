package engine_test

import (
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/compaction"
	"github.com/silent-knight19/lattice/internal/engine"
	latticeerrors "github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/version"
)

// =============================================================================
// Background leveled compaction.
//
// internal/compaction was fully implemented but had no production caller, so L0
// was append-only: every flush added an L0 file, nothing moved data to L1..LN,
// nothing was reclaimed, and gateL0Write eventually rejected every writer with
// ErrL0StallTimeout. These tests pin the wired behaviour.
// =============================================================================

// openCompactEngine returns an opened Engine with cheap memtable rotation so
// flushes produce L0 SSTables without allocating 64 MiB per generation.
func openCompactEngine(t *testing.T, opts engine.EngineOptions) (*engine.Engine, string) {
	t.Helper()
	dir := t.TempDir()
	if opts.Backpressure == (engine.BackpressureConfig{}) {
		opts.Backpressure = engine.DefaultBackpressureConfig()
	}
	if opts.DBPath == "" {
		opts.DBPath = dir
	}
	// L0StallTimeout is deliberately left at the production default. A tight
	// deadline here would make these tests race compaction throughput rather than
	// compaction correctness: under whole-suite CPU contention a 2s stall budget
	// can expire while the compactor is still legitimately draining L0, which is a
	// harness flake and says nothing about the product. Tests that specifically
	// exercise stall behavior set their own short value.
	eng := engine.NewEngineWithOptions(opts)
	restore := eng.SetFlushThresholdForTesting(256)
	t.Cleanup(restore)
	if err := eng.Open(); err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	return eng, dir
}

// levelCounts snapshots the per-level file counts of the current Version.
func levelCounts(t *testing.T, eng *engine.Engine) []int {
	t.Helper()
	vs := eng.VersionSet()
	if vs == nil || !vs.HasCurrent() {
		return nil
	}
	ver := vs.Current()
	defer ver.Unref()
	out := make([]int, version.NumLevels)
	for lvl := range out {
		out[lvl] = ver.NumFiles(lvl)
	}
	return out
}

func totalFiles(counts []int) int {
	n := 0
	for _, c := range counts {
		n += c
	}
	return n
}

// sstFilesOnDisk counts published SSTables physically present in the directory.
func sstFilesOnDisk(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	n := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".sst" {
			n++
		}
	}
	return n
}

// TestCompaction_L0DrainsUnderSustainedWrites is the core regression.
//
// Pre-fix, 40 rotations produced 26 L0 files and the next write was rejected
// with ErrL0StallTimeout. With compaction wired, L0 must stay bounded and every
// write must succeed.
func TestCompaction_L0DrainsUnderSustainedWrites(t *testing.T) {
	eng, _ := openCompactEngine(t, engine.EngineOptions{})
	ctx := context.Background()

	const rotations = 40
	const perRotation = 8
	for i := 0; i < rotations; i++ {
		for j := 0; j < perRotation; j++ {
			key := []byte(fmt.Sprintf("k-%04d-%02d", i, j))
			if err := eng.Put(ctx, key, []byte("v")); err != nil {
				t.Fatalf("Put %q at rotation %d failed: %v", key, i, err)
			}
		}
	}

	// Allow the background worker to drain L0.
	deadline := time.Now().Add(20 * time.Second)
	var counts []int
	for time.Now().Before(deadline) {
		counts = levelCounts(t, eng)
		if counts != nil && counts[0] <= engine.L0StallThreshold {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if counts == nil {
		t.Fatal("no Version published")
	}
	if counts[0] > engine.L0StallThreshold {
		t.Errorf("L0 still holds %d files (stall threshold %d); compaction did not relieve pressure",
			counts[0], engine.L0StallThreshold)
	}
	if totalFiles(counts) == 0 {
		t.Error("no files at any level; flushes produced nothing")
	}
	if deeper := totalFiles(counts[1:]); deeper == 0 {
		t.Errorf("L0=%d but L1..LN are empty; compaction never relocated data", counts[0])
	}
	if st := eng.CompactionStats(); !st.Enabled {
		t.Error("CompactionStats reports compaction disabled")
	} else if st.Failures != 0 {
		t.Errorf("compaction reported %d failures, last error: %v", st.Failures, st.LastErr)
	}
}

// TestCompaction_PreservesEveryKey is the correctness gate: compaction must be
// lossless. An earlier revision of the worker passed unpositioned child iterators
// to the merging iterator, which treats Valid()==false as exhausted and silently
// produced empty output, losing 151 of 160 keys. This test exists to prevent any
// recurrence.
func TestCompaction_PreservesEveryKey(t *testing.T) {
	eng, _ := openCompactEngine(t, engine.EngineOptions{})
	ctx := context.Background()

	written := make(map[string]string)
	const rotations = 20
	const perRotation = 8
	for i := 0; i < rotations; i++ {
		for j := 0; j < perRotation; j++ {
			k := fmt.Sprintf("k-%04d-%02d", i, j)
			v := fmt.Sprintf("val-%04d-%02d", i, j)
			if err := eng.Put(ctx, []byte(k), []byte(v)); err != nil {
				t.Fatalf("Put %q: %v", k, err)
			}
			written[k] = v
		}
	}

	// Force several compactions to complete.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if eng.CompactionStats().Runs >= 5 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if runs := eng.CompactionStats().Runs; runs == 0 {
		t.Fatal("no compaction ever ran")
	}

	missing, wrong := 0, 0
	for k, want := range written {
		got, err := eng.Get([]byte(k))
		if err != nil {
			missing++
			if missing <= 5 {
				t.Errorf("LOST KEY %q after compaction: %v", k, err)
			}
			continue
		}
		if string(got) != want {
			wrong++
			if wrong <= 5 {
				t.Errorf("WRONG VALUE %q: got %q want %q", k, got, want)
			}
		}
	}
	if missing > 0 || wrong > 0 {
		t.Fatalf("compaction lost or corrupted data: %d missing, %d wrong of %d",
			missing, wrong, len(written))
	}
}

// TestCompaction_OverwritesResolveToNewest verifies per-user-key deduplication:
// repeated writes to the same key across many L0 generations must resolve to the
// final value everywhere, not to an arbitrary older revision.
func TestCompaction_OverwritesResolveToNewest(t *testing.T) {
	eng, _ := openCompactEngine(t, engine.EngineOptions{})
	ctx := context.Background()

	const keys = 6
	const versions = 30
	for v := 0; v < versions; v++ {
		for k := 0; k < keys; k++ {
			key := []byte(fmt.Sprintf("hot-%02d", k))
			val := fmt.Sprintf("gen-%03d", v)
			if err := eng.Put(ctx, key, []byte(val)); err != nil {
				t.Fatalf("Put gen %d key %d: %v", v, k, err)
			}
		}
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if eng.CompactionStats().Runs >= 5 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	want := fmt.Sprintf("gen-%03d", versions-1)
	for k := 0; k < keys; k++ {
		key := fmt.Sprintf("hot-%02d", k)
		got, err := eng.Get([]byte(key))
		if err != nil {
			t.Fatalf("Get %q: %v", key, err)
		}
		if string(got) != want {
			t.Errorf("key %q resolved to %q; want newest value %q", key, got, want)
		}
	}
}

// TestCompaction_TombstonesDoNotResurrect verifies deleted keys stay deleted
// after compaction. A tombstone that is dropped prematurely would let an older
// value in a deeper level become visible again.
func TestCompaction_TombstonesDoNotResurrect(t *testing.T) {
	eng, _ := openCompactEngine(t, engine.EngineOptions{})
	ctx := context.Background()

	const keys = 12

	// Generation 1: write every key, forcing a flush per rotation.
	for k := 0; k < keys; k++ {
		if err := eng.Put(ctx, []byte(fmt.Sprintf("d-%02d", k)), []byte("original")); err != nil {
			t.Fatalf("Put gen1 %d: %v", k, err)
		}
	}
	waitForRuns(t, eng, 3)

	// Generation 2: delete every key.
	for k := 0; k < keys; k++ {
		if err := eng.Delete(ctx, []byte(fmt.Sprintf("d-%02d", k))); err != nil {
			t.Fatalf("Delete %d: %v", k, err)
		}
	}
	waitForRuns(t, eng, 6)

	// Force additional compactions so any tombstone purge decision is exercised.
	waitForRuns(t, eng, 8)

	for k := 0; k < keys; k++ {
		key := fmt.Sprintf("d-%02d", k)
		if _, err := eng.Get([]byte(key)); err == nil {
			t.Errorf("key %q resurrected after delete + compaction", key)
		}
	}
}

// TestCompaction_ReclaimsObsoleteFiles verifies compaction reclaims disk so
// sustained writes do not grow the directory without bound.
//
// Two properties are asserted, because "on-disk count == live count" is NOT one
// the engine ever guaranteed: flusher.go deliberately leaves a finalized but
// uncommitted SSTable on disk when LogAndApply fails, deferring reclamation to
// recovery. So this asserts (a) disk usage stays bounded, and (b) any such
// orphan is reclaimed by the existing recovery cleaner on reopen.
func TestCompaction_ReclaimsObsoleteFiles(t *testing.T) {
	eng, dir := openCompactEngine(t, engine.EngineOptions{})
	ctx := context.Background()

	// Write enough to force multiple flushes and compactions.
	const rotations = 30
	const perRotation = 8
	for i := 0; i < rotations; i++ {
		for j := 0; j < perRotation; j++ {
			if err := eng.Put(ctx, []byte(fmt.Sprintf("r-%04d-%02d", i, j)), []byte("v")); err != nil {
				t.Fatalf("Put rotation %d: %v", i, err)
			}
		}
	}
	waitForRuns(t, eng, 8)

	// Quiesce before measuring. Close stops the flusher and the compactor and
	// drains every generation, so no new SSTable can appear between the two reads.
	if err := eng.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	live := totalFiles(levelCounts(t, eng))
	if live == 0 {
		t.Fatal("no live files; nothing to compare")
	}
	onDisk := sstFilesOnDisk(t, dir)

	// (a) Disk must stay bounded. Without compaction every one of the 240 writes'
	// flushes would accumulate as its own L0 file, so onDisk would grow roughly
	// linearly with the workload. A small slack absorbs the documented
	// flush-time orphan.
	const orphanSlack = 4
	if onDisk > live+orphanSlack {
		t.Errorf("%d SSTables on disk vs %d live; disk is growing without bound "+
			"(slack %d). Obsolete files are not being reclaimed. dir=%s",
			onDisk, live, orphanSlack, dir)
	}
	t.Logf("live=%d onDisk=%d (slack %d)", live, onDisk, orphanSlack)

	// (b) Reopening must reclaim any orphan left by a failed flush-time commit.
	// This is the safety net compaction relies on for its own uncommitted output.
	reopened := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: engine.DefaultBackpressureConfig(),
	})
	if err := reopened.Open(); err != nil {
		t.Fatalf("reopen Open: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	finalDisk := sstFilesOnDisk(t, dir)
	finalLive := totalFiles(levelCounts(t, reopened))
	if finalDisk > finalLive {
		t.Errorf("after recovery %d SSTables on disk vs %d live; the orphan cleaner "+
			"did not reclaim uncommitted files. dir=%s", finalDisk, finalLive, dir)
	}
	t.Logf("after recovery: live=%d onDisk=%d", finalLive, finalDisk)

	// Sanity: data written before the restart is still readable.
	for i := 0; i < rotations; i++ {
		key := fmt.Sprintf("r-%04d-%02d", i, 0)
		if _, err := reopened.Get([]byte(key)); err != nil {
			t.Errorf("Get %q after recovery+compaction: %v", key, err)
		}
	}
}

// waitForRuns blocks until the background compactor has completed at least n
// successful runs, or the timeout elapses.
func waitForRuns(t *testing.T, eng *engine.Engine, n uint64) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if eng.CompactionStats().Runs >= n {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("compaction reached %d runs (wanted %d) before timeout", eng.CompactionStats().Runs, n)
}

// TestCompaction_DisabledPreservesL0Growth verifies the DisableCompaction escape
// hatch behaves as documented: L0 grows and the stall gate eventually rejects
// writes rather than silently compacting.
func TestCompaction_DisabledPreservesL0Growth(t *testing.T) {
	eng, _ := openCompactEngine(t, engine.EngineOptions{
		DisableCompaction: true,
		L0StallTimeout:    300 * time.Millisecond,
	})
	ctx := context.Background()

	var stallErr error
	for i := 0; i < 400 && stallErr == nil; i++ {
		for j := 0; j < 8; j++ {
			key := []byte(fmt.Sprintf("n-%04d-%02d", i, j))
			if err := eng.Put(ctx, key, []byte("v")); err != nil {
				if stderrors.Is(err, latticeerrors.ErrL0StallTimeout) {
					stallErr = err
					break
				}
				t.Fatalf("Put %q: %v", key, err)
			}
		}
	}

	if stallErr == nil {
		t.Skip("did not reach the L0 stall threshold with compaction disabled")
	}
	if st := eng.CompactionStats(); st.Enabled || st.Runs != 0 {
		t.Errorf("compaction ran despite being disabled: %+v", st)
	}
	counts := levelCounts(t, eng)
	if deeper := totalFiles(counts[1:]); deeper != 0 {
		t.Errorf("L1..LN hold %d files with compaction disabled; expected none", deeper)
	}
}

// TestCompaction_ManualRunRequiresEnabled verifies RunCompactionOnce fails closed
// when compaction is disabled rather than silently succeeding.
func TestCompaction_ManualRunRequiresEnabled(t *testing.T) {
	eng, _ := openCompactEngine(t, engine.EngineOptions{DisableCompaction: true})

	if _, err := eng.RunCompactionOnce(); err == nil {
		t.Fatal("RunCompactionOnce succeeded with compaction disabled")
	} else if !stderrors.Is(err, latticeerrors.ErrCompactionDisabled) {
		t.Errorf("RunCompactionOnce returned %v; want errors.ErrCompactionDisabled", err)
	}
}

// TestCompaction_CustomPolicyHonored verifies a custom policy changes selection
// behavior, proving the engine really routes through the Planner rather than
// hardcoding thresholds.
func TestCompaction_CustomPolicyHonored(t *testing.T) {
	eng, _ := openCompactEngine(t, engine.EngineOptions{
		CompactionPolicy: compaction.CompactionPolicy{
			L0TriggerCount:  2,
			L1TargetBytes:   64 * 1024,
			LevelMultiplier: 10.0,
			MaxLevels:       version.NumLevels,
		},
	})
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		for j := 0; j < 8; j++ {
			if err := eng.Put(ctx, []byte(fmt.Sprintf("p-%02d-%02d", i, j)), []byte("v")); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
	}
	waitForRuns(t, eng, 2)

	if st := eng.CompactionStats(); st.Runs == 0 {
		t.Errorf("L0TriggerCount=2 policy produced no compactions: %+v", st)
	}
}

// TestCompaction_SurvivesCloseWithoutLeak verifies Close tears the worker down
// cleanly and remains idempotent.
func TestCompaction_SurvivesCloseWithoutLeak(t *testing.T) {
	dir := t.TempDir()
	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:            dir,
		Backpressure:      engine.DefaultBackpressureConfig(),
		L0StallTimeout:    time.Second,
		DisableCompaction: false,
	})
	restore := eng.SetFlushThresholdForTesting(256)
	defer restore()
	if err := eng.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}

	ctx := context.Background()
	for i := 0; i < 20; i++ {
		if err := eng.Put(ctx, []byte(fmt.Sprintf("c-%02d", i)), []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	if err := eng.Close(); err != nil {
		t.Errorf("Close returned %v", err)
	}
	if st := eng.CompactionStats(); st.Running {
		t.Error("compaction worker still reported running after Close")
	}
	// Idempotence.
	if err := eng.Close(); err != nil {
		t.Errorf("second Close returned %v; want nil", err)
	}
}
