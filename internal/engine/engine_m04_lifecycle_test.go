package engine_test

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/compaction"
	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/sstable"
	"github.com/silent-knight19/lattice/internal/version"
)

// compactOnce runs a single real L0->L1 compaction against eng's current
// Version using existing Planner/Compactor/TableReader machinery and publishes
// via VersionSet.LogAndApply. It returns true when a plan existed and was
// published. Callers must quiesce Engine flushes around it: the Engine never
// publishes concurrently with itself, but external publishers are serialized
// only (no merge), so tests avoid overlapping them with Close drains.
func compactOnce(t *testing.T, eng *engine.Engine, dir string) (bool, error) {
	t.Helper()
	vs := eng.VersionSet()
	ver := vs.Current()
	if ver == nil {
		return false, nil
	}
	planner, err := compaction.NewPlanner(compaction.DefaultCompactionPolicy())
	if err != nil {
		ver.Unref()
		return false, err
	}
	plan, err := planner.PickCompaction(ver)
	ver.Unref()
	if err != nil || plan == nil {
		return false, err
	}
	inputs := plan.AllInputFiles()
	readers := make([]*sstable.TableReader, 0, len(inputs))
	iters := make([]sstable.Iterator, 0, len(inputs))
	cleanup := func() {
		for _, it := range iters {
			if c, ok := any(it).(interface{ Close() error }); ok {
				_ = c.Close()
			}
		}
		for _, r := range readers {
			_ = r.Close()
		}
	}
	for _, f := range inputs {
		r, err := sstable.NewTableReaderWithOptions(
			version.TablePath(dir, f.FileNum),
			sstable.TableReaderOptions{FileNum: f.FileNum},
		)
		if err != nil {
			cleanup()
			return false, err
		}
		readers = append(readers, r)
		it, err := r.NewIterator()
		if err != nil {
			cleanup()
			return false, err
		}
		// Merge children must be positioned before entering the heap;
		// unpositioned children are silently skipped.
		if err := it.SeekToFirst(); err != nil {
			_ = it.Close()
			cleanup()
			return false, err
		}
		if !it.Valid() {
			_ = it.Close()
			cleanup()
			return false, fmt.Errorf("compaction input %d is empty", f.FileNum)
		}
		iters = append(iters, it)
	}
	cver := vs.Current()
	if cver == nil {
		cleanup()
		return false, nil
	}
	compactor, err := compaction.NewCompactor(cver)
	cver.Unref()
	if err != nil {
		cleanup()
		return false, err
	}
	defer func() { _ = compactor.Close() }()
	miter := compaction.NewMergingIterator(iters)
	alloc := func() (uint64, error) { return eng.AllocateFileNum(), nil }
	out, err := compactor.BuildOutput(miter, plan, dir, alloc)
	cleanup()
	if err != nil {
		return false, err
	}
	edit := version.NewVersionEdit()
	var maxFile uint64
	for _, m := range out.FileMetadatas() {
		// #nosec G115 -- compaction levels are 0..6 by planner construction
		if err := edit.AddFile(uint32(plan.TargetLevel()), m); err != nil {
			return false, err
		}
		if m.FileNum > maxFile {
			maxFile = m.FileNum
		}
	}
	for _, f := range plan.SourceFiles() {
		// #nosec G115 -- compaction levels are 0..6 by planner construction
		if err := edit.DeleteFile(uint32(plan.SourceLevel()), f.FileNum); err != nil {
			return false, err
		}
	}
	for _, f := range plan.TargetFiles() {
		// #nosec G115 -- compaction levels are 0..6 by planner construction
		if err := edit.DeleteFile(uint32(plan.TargetLevel()), f.FileNum); err != nil {
			return false, err
		}
	}
	if maxFile > 0 {
		edit.SetNextFileNum(maxFile + 1)
	}
	if err := vs.LogAndApply(edit); err != nil {
		return false, err
	}
	return true, nil
}

// TestEngineM04_CompactionThenShutdown runs a real compaction to completion
// before Close: flushed L0 files are valid compaction inputs, the L1 output
// survives shutdown, and reopen serves the compacted state.
func TestEngineM04_CompactionThenShutdown(t *testing.T) {
	dir := t.TempDir()
	eng := newRealWALEngine(t, dir, 4096)
	ctx := testCtx()
	ref := map[string]string{}
	for i := 0; i < 240; i++ {
		k := fmt.Sprintf("cx_%04d", i)
		v := fmt.Sprintf("v_%d", i)
		ref[k] = v
		if err := eng.Put(ctx, []byte(k), []byte(v)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	waitFlushEmpty(t, eng, 30000)
	compacted, err := compactOnce(t, eng, dir)
	if err != nil {
		t.Fatalf("compactOnce: %v", err)
	}
	if !compacted {
		t.Fatalf("expected a compaction plan with %d L0 files", len(l0FileNums(t, eng)))
	}
	ver := eng.VersionSet().Current()
	l1n := ver.NumFiles(1)
	ver.Unref()
	if l1n < 1 {
		t.Fatalf("expected L1 output, got %d files", l1n)
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	eng2 := openExistingDir(t, dir)
	defer func() { _ = eng2.Close() }()
	for k, v := range ref {
		got, err := eng2.Get([]byte(k))
		if err != nil || string(got) != v {
			t.Fatalf("reopened Get(%s) = %q %v, want %q", k, got, err, v)
		}
	}
}

// TestEngineM04_CompactionRaceWithClose runs read-side compaction work
// (planning, pinned-Version TableReader I/O, tombstone checks) concurrently
// with Close. Either side may win; the requirement is no panic, no data race,
// no use-after-close, and a recoverable database afterwards.
func TestEngineM04_CompactionRaceWithClose(t *testing.T) {
	dir := t.TempDir()
	eng := newRealWALEngine(t, dir, 4096)
	ctx := testCtx()
	for i := 0; i < 120; i++ {
		if err := eng.Put(ctx, []byte(fmt.Sprintf("rx_%04d", i)), []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	waitFlushEmpty(t, eng, 30000)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			ver := eng.VersionSet().Current()
			if ver == nil {
				return
			}
			planner, err := compaction.NewPlanner(compaction.DefaultCompactionPolicy())
			if err != nil {
				ver.Unref()
				return
			}
			plan, _ := planner.PickCompaction(ver)
			if plan != nil {
				for _, f := range plan.AllInputFiles() {
					r, err := sstable.NewTableReaderWithOptions(
						version.TablePath(dir, f.FileNum),
						sstable.TableReaderOptions{FileNum: f.FileNum},
					)
					if err != nil {
						continue
					}
					it, err := r.NewIterator()
					if err != nil {
						_ = r.Close()
						continue
					}
					_ = it.Seek([]byte("rx_0000"))
					_ = it.Close()
					_ = r.Close()
				}
			}
			ver.Unref()
		}
	}()
	// Main thread keeps reading while the race runs, then closes.
	for i := 0; i < 20; i++ {
		_, _ = eng.Get([]byte("rx_0000"))
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("Close during compaction reads: %v", err)
	}
	close(stop)
	<-done
	eng2 := openExistingDir(t, dir)
	defer func() { _ = eng2.Close() }()
	if got, err := eng2.Get([]byte("rx_0000")); err != nil || string(got) != "v" {
		t.Fatalf("reopened Get = %q %v", got, err)
	}
}

// TestEngineM04_OpenCloseStress repeats Open/workload/Close on one directory,
// asserting no goroutine or logical-state accumulation bugs and exact final state.
func TestEngineM04_OpenCloseStress(t *testing.T) {
	dir := t.TempDir()
	// Warmup cycle to stabilize runtime goroutines before sampling baseline.
	warm := newRealWALEngineNoOpen(t, dir)
	if err := warm.Open(); err != nil {
		t.Fatalf("warmup Open: %v", err)
	}
	if err := warm.Close(); err != nil {
		t.Fatalf("warmup Close: %v", err)
	}
	baseRoutines := runtime.NumGoroutine()

	ref := map[string]string{}
	const iters = 10
	for n := 0; n < iters; n++ {
		eng := openExistingDir(t, dir)
		ctx := testCtx()
		for i := 0; i < 20; i++ {
			k := fmt.Sprintf("iter%02d_%02d", n, i)
			ref[k] = "v"
			if err := eng.Put(ctx, []byte(k), []byte("v")); err != nil {
				t.Fatalf("iter %d Put: %v", n, err)
			}
		}
		if err := eng.Close(); err != nil {
			t.Fatalf("iter %d Close: %v", n, err)
		}
		if eng.FlushQueueLen() != 0 {
			t.Fatalf("iter %d: queue not drained", n)
		}
	}
	// Quiesce and compare goroutines: each engine owns exactly one flusher
	// worker, joined by Close, so growth must stay negligible.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseRoutines+5 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseRoutines+5 {
		t.Fatalf("goroutine growth: baseline %d, now %d", baseRoutines, got)
	}
	eng := openExistingDir(t, dir)
	defer func() { _ = eng.Close() }()
	for k, v := range ref {
		got, err := eng.Get([]byte(k))
		if err != nil || string(got) != v {
			t.Fatalf("final Get(%s) = %q %v", k, got, err)
		}
	}
	if got := len(ref); got != iters*20 {
		t.Fatalf("reference size = %d", got)
	}
}

// TestEngineM04_FullLifecycleAcceptance exercises the complete Phase 10 arc:
// Open -> workload with rotations and background flushes -> L0 growth with
// pacing -> manual compaction relief -> more writes -> Close drain -> reopen
// with full reference verification.
func TestEngineM04_FullLifecycleAcceptance(t *testing.T) {
	dir := t.TempDir()
	eng := newRealWALEngine(t, dir, 8192)
	ctx := testCtx()
	ref := map[string]string{}

	// Phase 1: workload with background flushing (stays below pacing: <9 L0).
	for i := 0; i < 150; i++ {
		k := fmt.Sprintf("life_%05d", i)
		v := fmt.Sprintf("v1_%d", i)
		ref[k] = v
		if err := eng.Put(ctx, []byte(k), []byte(v)); err != nil {
			t.Fatalf("phase1 Put: %v", err)
		}
		if i%50 == 0 {
			delete(ref, fmt.Sprintf("life_%05d", i))
			if err := eng.Delete(ctx, []byte(fmt.Sprintf("life_%05d", i))); err != nil {
				t.Fatalf("phase1 Delete: %v", err)
			}
		}
	}
	waitFlushEmpty(t, eng, 60000)
	nL0 := len(l0FileNums(t, eng))
	t.Logf("phase1 complete: L0 files=%d flushes=%d", nL0, eng.FlushCount())
	if nL0 < 1 {
		t.Fatalf("expected background L0 files, got none")
	}

	// Phase 2: relieve via a real compaction (as the system would).
	if ok, err := compactOnce(t, eng, dir); err != nil {
		t.Fatalf("compaction: %v", err)
	} else if ok {
		t.Logf("phase2 compacted: L0=%d", len(l0FileNums(t, eng)))
	}

	// Phase 3: updates, new keys, deletes on top of compacted state.
	for i := 0; i < 60; i++ {
		k := fmt.Sprintf("life_%05d", i)
		ref[k] = "v2"
		if err := eng.Put(ctx, []byte(k), []byte("v2")); err != nil {
			t.Fatalf("phase3 update: %v", err)
		}
	}
	for i := 150; i < 200; i++ {
		k := fmt.Sprintf("life_%05d", i)
		ref[k] = "new"
		if err := eng.Put(ctx, []byte(k), []byte("new")); err != nil {
			t.Fatalf("phase3 Put: %v", err)
		}
	}
	delete(ref, "life_00100")
	if err := eng.Delete(ctx, []byte("life_00100")); err != nil {
		t.Fatalf("phase3 Delete: %v", err)
	}

	if err := eng.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if eng.FlushQueueLen() != 0 {
		t.Fatalf("queue not drained")
	}

	eng2 := openExistingDir(t, dir)
	defer func() { _ = eng2.Close() }()
	for k, v := range ref {
		got, err := eng2.Get([]byte(k))
		if err != nil || string(got) != v {
			t.Fatalf("final Get(%s) = %q %v, want %q", k, got, err, v)
		}
	}
	// Spot-check absence beyond the reference range.
	if _, err := eng2.Get([]byte("life_99999")); err == nil {
		t.Fatalf("unexpected key found")
	}
}

func BenchmarkEngineM04_CloseEmpty(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		dir := b.TempDir()
		eng := engine.NewEngineWithOptions(engine.EngineOptions{DBPath: dir})
		b.StartTimer()
		if err := eng.Open(); err != nil {
			b.Fatalf("Open: %v", err)
		}
		if err := eng.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
	}
}

func BenchmarkEngineM04_CloseWithData(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		dir := b.TempDir()
		eng := engine.NewEngineWithOptions(engine.EngineOptions{DBPath: dir})
		if err := eng.Open(); err != nil {
			b.Fatalf("Open: %v", err)
		}
		ctx := context.Background()
		for k := 0; k < 100; k++ {
			if err := eng.Put(ctx, []byte(fmt.Sprintf("bk_%04d", k)), []byte("v")); err != nil {
				b.Fatalf("Put: %v", err)
			}
		}
		b.StartTimer()
		if err := eng.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
	}
}
