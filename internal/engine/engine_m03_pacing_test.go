package engine_test

import (
	"context"
	stdErrors "errors"
	"fmt"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/compaction"
	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
	"github.com/silent-knight19/lattice/internal/version"
)

// installL0Files builds count real SSTables (one key each) in dir and installs
// them into eng's VersionSet at L0, returning file numbers. Keys are sorted
// per file (single key), files appended in FileNum order.
func installL0Files(t *testing.T, eng *engine.Engine, dir string, startFileNum uint64, count int, keyPrefix string) []uint64 {
	t.Helper()
	nums := make([]uint64, 0, count)
	vs := eng.VersionSet()
	for i := 0; i < count; i++ {
		fn := startFileNum + uint64(i)
		k := fmt.Sprintf("%s_%05d", keyPrefix, i)
		path := version.TablePath(dir, fn)
		w, err := sstable.NewTableWriter(path, sstable.DefaultTableWriterOptions())
		if err != nil {
			t.Fatalf("NewTableWriter: %v", err)
		}
		ik, err := binary.NewInternalKey([]byte(k), binary.SeqNum(1000+uint64(i)), binary.OpTypePut)
		if err != nil {
			t.Fatalf("NewInternalKey: %v", err)
		}
		if err := w.Add(ik, []byte("v")); err != nil {
			_ = w.Close()
			t.Fatalf("Add: %v", err)
		}
		meta, err := w.Finish()
		if err != nil {
			t.Fatalf("Finish: %v", err)
		}
		fm := version.NewFileMetadataFromSSTable(fn, meta)
		cur := vs.Current()
		var levels [version.NumLevels][]version.FileMetadata
		if cur != nil {
			for lvl := 0; lvl < version.NumLevels; lvl++ {
				levels[lvl] = append(levels[lvl], cur.Files(lvl)...)
			}
			cur.Unref()
		}
		levels[0] = append(levels[0], fm)
		v := version.NewVersion(levels)
		if err := vs.AppendVersion(v); err != nil {
			v.Unref()
			t.Fatalf("AppendVersion: %v", err)
		}
		nums = append(nums, fn)
	}
	return nums
}

// dropL0Files mimics compaction relief: publishes a new Version without the
// given file numbers.
func dropL0Files(t *testing.T, eng *engine.Engine, drop map[uint64]bool) {
	t.Helper()
	vs := eng.VersionSet()
	cur := vs.Current()
	if cur == nil {
		t.Fatalf("no current version")
	}
	var levels [version.NumLevels][]version.FileMetadata
	for lvl := 0; lvl < version.NumLevels; lvl++ {
		for _, f := range cur.Files(lvl) {
			if lvl == 0 && drop[f.FileNum] {
				continue
			}
			levels[lvl] = append(levels[lvl], f)
		}
	}
	cur.Unref()
	v := version.NewVersion(levels)
	if err := vs.AppendVersion(v); err != nil {
		v.Unref()
		t.Fatalf("relief AppendVersion: %v", err)
	}
}

func l0Count(t *testing.T, eng *engine.Engine) int {
	t.Helper()
	ver := eng.VersionSet().Current()
	if ver == nil {
		return 0
	}
	defer ver.Unref()
	return ver.NumFiles(0)
}

func TestEngineM03_ThresholdBoundaries(t *testing.T) {
	// L0 == 8 is normal: completes promptly with no relief.
	eng := newMemEngine()
	eng.SetL0CountOverrideForTesting(8)
	done := make(chan error, 1)
	go func() { done <- eng.Put(testCtx(), []byte("b8"), []byte("v")) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("L0=8 Put: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("L0=8 Put stalled; 8 must be normal")
	}
	_ = eng.Close()

	// L0 == 12 is paced, not stalled: completes without relief.
	eng12 := newMemEngine()
	defer func() { _ = eng12.Close() }()
	eng12.SetL0CountOverrideForTesting(12)
	done12 := make(chan error, 1)
	go func() { done12 <- eng12.Put(testCtx(), []byte("b12"), []byte("v")) }()
	select {
	case err := <-done12:
		if err != nil {
			t.Fatalf("L0=12 Put: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("L0=12 Put stalled; 12 must be paced, not stalled")
	}

	// L0 == 13 stalls: needs relief.
	eng13 := newMemEngine()
	defer func() { _ = eng13.Close() }()
	eng13.SetL0CountOverrideForTesting(13)
	done13 := make(chan error, 1)
	go func() { done13 <- eng13.Put(testCtx(), []byte("b13"), []byte("v")) }()
	select {
	case err := <-done13:
		t.Fatalf("L0=13 Put completed without relief: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	eng13.SetL0CountOverrideForTesting(7)
	select {
	case err := <-done13:
		if err != nil {
			t.Fatalf("L0=13 Put after relief to 7: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("L0=13 Put did not resume after relief")
	}
}

func TestEngineM03_PacingMonotonic(t *testing.T) {
	// Measure Put latency at 8 (baseline), 9, 12 with generous bands to avoid
	// flake: higher pressure must not be faster than lower by more than noise,
	// and 12 must show a clearly larger delay than 8.
	measure := func(l0 int) time.Duration {
		eng := newMemEngine()
		defer func() { _ = eng.Close() }()
		eng.SetL0CountOverrideForTesting(l0)
		start := time.Now()
		if err := eng.Put(testCtx(), []byte(fmt.Sprintf("k%d", l0)), []byte("v")); err != nil {
			t.Fatalf("Put at L0=%d: %v", l0, err)
		}
		return time.Since(start)
	}
	d8 := measure(8)
	d9 := measure(9)
	d12 := measure(12)
	t.Logf("L0=8:%v L0=9:%v L0=12:%v", d8, d9, d12)
	if d12 < 20*time.Millisecond {
		t.Fatalf("L0=12 should carry the largest pacing delay, got %v", d12)
	}
	if d9 < 500*time.Microsecond {
		t.Fatalf("L0=9 should carry a measurable pacing delay, got %v", d9)
	}
	// 8 must be fast (no timer allocated).
	if d8 > 500*time.Millisecond {
		t.Fatalf("L0=8 baseline too slow: %v", d8)
	}
}

func TestEngineM03_StallBlocksAndResumes(t *testing.T) {
	eng := newMemEngine()
	defer func() { _ = eng.Close() }()
	eng.SetL0CountOverrideForTesting(13)

	done := make(chan error, 1)
	go func() { done <- eng.Put(testCtx(), []byte("sk"), []byte("SV")) }()
	select {
	case err := <-done:
		t.Fatalf("Put completed while stalled: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	// Relief to hysteresis boundary (<=12) resumes.
	eng.SetL0CountOverrideForTesting(12)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Put after relief failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Put did not resume after relief to 12")
	}
	got, err := eng.Get([]byte("sk"))
	if err != nil || string(got) != "SV" {
		t.Fatalf("Get after stall resume = %q %v", got, err)
	}
}

func TestEngineM03_DeleteStallsIdentically(t *testing.T) {
	eng := newMemEngine()
	defer func() { _ = eng.Close() }()
	if err := eng.Put(testCtx(), []byte("dk"), []byte("v")); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	eng.SetL0CountOverrideForTesting(13)
	done := make(chan error, 1)
	go func() { done <- eng.Delete(testCtx(), []byte("dk")) }()
	select {
	case err := <-done:
		t.Fatalf("Delete completed while stalled: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	eng.SetL0CountOverrideForTesting(0)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Delete after relief: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Delete did not resume")
	}
	if _, err := eng.Get([]byte("dk")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("expected tombstone after stalled delete, got %v", err)
	}
}

func TestEngineM03_ReadsAvailableDuringStall(t *testing.T) {
	eng := newMemEngine()
	defer func() { _ = eng.Close() }()
	if err := eng.Put(testCtx(), []byte("rk"), []byte("rv")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	eng.SetL0CountOverrideForTesting(25)
	done := make(chan error, 1)
	go func() { done <- eng.Put(testCtx(), []byte("blocked"), []byte("b")) }()
	time.Sleep(50 * time.Millisecond)
	// Gets must complete promptly despite write stall.
	got, err := eng.Get([]byte("rk"))
	if err != nil || string(got) != "rv" {
		t.Fatalf("Get during stall = %q %v", got, err)
	}
	if _, err := eng.Get([]byte("absent_xyz")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("miss during stall = %v", err)
	}
	eng.SetL0CountOverrideForTesting(0)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("blocked Put after relief: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("blocked Put never resumed")
	}
}

func TestEngineM03_ValidationBeforePacing(t *testing.T) {
	eng := newMemEngine()
	defer func() { _ = eng.Close() }()
	eng.SetL0CountOverrideForTesting(13)
	start := time.Now()
	err := eng.Put(testCtx(), nil, []byte("v"))
	if !stdErrors.Is(err, errors.ErrEmptyKey) {
		t.Fatalf("expected validation error, got %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("invalid request was stalled instead of fast-rejected")
	}
	if err := eng.Delete(testCtx(), []byte{}); !stdErrors.Is(err, errors.ErrEmptyKey) {
		t.Fatalf("expected validation error on Delete, got %v", err)
	}
}

func TestEngineM03_CloseWhileStalled(t *testing.T) {
	eng := newMemEngine()
	eng.SetL0CountOverrideForTesting(13)
	done := make(chan error, 1)
	go func() { done <- eng.Put(testCtx(), []byte("ck"), []byte("v")) }()
	time.Sleep(50 * time.Millisecond)
	if err := eng.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if !stdErrors.Is(err, errors.ErrWriterClosed) {
			t.Fatalf("stalled Put after Close = %v, want ErrWriterClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("stalled Put did not terminate on Close")
	}
}

func TestEngineM03_ContextCancelWhileStalled(t *testing.T) {
	eng := newMemEngine()
	defer func() { _ = eng.Close() }()
	eng.SetL0CountOverrideForTesting(13)
	ctx, cancel := context.WithCancel(testCtx())
	done := make(chan error, 1)
	go func() { done <- eng.Put(ctx, []byte("xk"), []byte("v")) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !stdErrors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("stalled Put ignored context cancellation")
	}
}

func TestEngineM03_ManyStalledWriters(t *testing.T) {
	eng := newMemEngine()
	defer func() { _ = eng.Close() }()
	eng.SetL0CountOverrideForTesting(13)
	const n = 32
	done := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			done <- eng.Put(testCtx(), []byte(fmt.Sprintf("mw_%02d", i)), []byte("v"))
		}(i)
	}
	time.Sleep(150 * time.Millisecond)
	if got := len(done); got != 0 {
		t.Fatalf("%d writers completed while stalled", got)
	}
	eng.SetL0CountOverrideForTesting(0)
	timeout := time.After(10 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("writer failed after relief: %v", err)
			}
		case <-timeout:
			t.Fatalf("only %d/%d writers resumed", i, n)
		}
	}
	for i := 0; i < n; i++ {
		if _, err := eng.Get([]byte(fmt.Sprintf("mw_%02d", i))); err != nil {
			t.Fatalf("Get mw_%02d: %v", i, err)
		}
	}
}

func TestEngineM03_RealL0PressureAndRelief(t *testing.T) {
	dir := t.TempDir()
	eng := newMemEngine()
	eng.SetDBPath(dir)
	defer func() { _ = eng.Close() }()

	nums := installL0Files(t, eng, dir, 1, 9, "press")
	if got := l0Count(t, eng); got != 9 {
		t.Fatalf("L0 = %d, want 9", got)
	}
	// Existing compaction planner must observe the same pressure.
	ver := eng.VersionSet().Current()
	planner, err := compaction.NewPlanner(compaction.DefaultCompactionPolicy())
	if err != nil {
		t.Fatalf("planner: %v", err)
	}
	lvl, _, should, err := planner.PickCompactionLevel(ver)
	ver.Unref()
	if err != nil || !should || lvl != 0 {
		t.Fatalf("planner should pick L0 (lvl=%d should=%v err=%v)", lvl, should, err)
	}
	// L0=9 paces but completes.
	start := time.Now()
	if err := eng.Put(testCtx(), []byte("paced_real"), []byte("v")); err != nil {
		t.Fatalf("paced Put: %v", err)
	}
	if time.Since(start) < 500*time.Microsecond {
		t.Fatalf("expected measurable pacing at real L0=9")
	}
	// Push to stall territory with 4 more real files.
	installL0Files(t, eng, dir, 10, 4, "press2")
	if got := l0Count(t, eng); got != 13 {
		t.Fatalf("L0 = %d, want 13", got)
	}
	_ = nums
	blocked := make(chan error, 1)
	go func() { blocked <- eng.Put(testCtx(), []byte("stalled_real"), []byte("v")) }()
	select {
	case err := <-blocked:
		t.Fatalf("Put completed at real L0=13: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	// Compaction-like relief: drop 5 files through a real Version publication.
	drop := map[uint64]bool{1: true, 2: true, 3: true, 4: true, 5: true}
	dropL0Files(t, eng, drop)
	if got := l0Count(t, eng); got != 8 {
		t.Fatalf("L0 after relief = %d, want 8", got)
	}
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatalf("blocked Put after real relief: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("blocked Put did not resume after real relief")
	}
	if got, err := eng.Get([]byte("stalled_real")); err != nil || string(got) != "v" {
		t.Fatalf("Get after relief = %q %v", got, err)
	}
}

func TestEngineM03_SameKeyAcrossPressure(t *testing.T) {
	eng := newMemEngine()
	defer func() { _ = eng.Close() }()
	if err := eng.Put(testCtx(), []byte("sk"), []byte("V1")); err != nil {
		t.Fatalf("V1: %v", err)
	}
	eng.SetL0CountOverrideForTesting(13)
	v2done := make(chan error, 1)
	go func() { v2done <- eng.Put(testCtx(), []byte("sk"), []byte("V2")) }()
	time.Sleep(80 * time.Millisecond)
	// V1 still visible while V2 waits (no partial mutation).
	if got, err := eng.Get([]byte("sk")); err != nil || string(got) != "V1" {
		t.Fatalf("during stall Get = %q %v, want V1", got, err)
	}
	eng.SetL0CountOverrideForTesting(0)
	select {
	case err := <-v2done:
		if err != nil {
			t.Fatalf("V2 after relief: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("V2 never applied")
	}
	if got, err := eng.Get([]byte("sk")); err != nil || string(got) != "V2" {
		t.Fatalf("final Get = %q %v, want V2", got, err)
	}
	// Stalled delete likewise applies atomically after relief.
	eng.SetL0CountOverrideForTesting(13)
	ddone := make(chan error, 1)
	go func() { ddone <- eng.Delete(testCtx(), []byte("sk")) }()
	time.Sleep(80 * time.Millisecond)
	if got, err := eng.Get([]byte("sk")); err != nil || string(got) != "V2" {
		t.Fatalf("delete must not partially apply while stalled: %q %v", got, err)
	}
	eng.SetL0CountOverrideForTesting(0)
	select {
	case err := <-ddone:
		if err != nil {
			t.Fatalf("delete after relief: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("delete never applied")
	}
	if _, err := eng.Get([]byte("sk")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("expected tombstone after relief, got %v", err)
	}
}

// TestEngineM03_Acceptance ties the full arc with real VersionSet state:
// normal writes -> real L0 growth -> stall -> VersionSet relief (as compaction
// would publish) -> resume with correct data.
func TestEngineM03_Acceptance(t *testing.T) {
	dir := t.TempDir()
	eng := newMemEngine()
	eng.SetDBPath(dir)
	defer func() { _ = eng.Close() }()

	if err := eng.Put(testCtx(), []byte("acc_base"), []byte("b")); err != nil {
		t.Fatalf("baseline Put: %v", err)
	}
	installL0Files(t, eng, dir, 100, 13, "acc")
	if got := l0Count(t, eng); got != 13 {
		t.Fatalf("L0 = %d, want 13", got)
	}
	// Writers block; readers stay available.
	w1 := make(chan error, 1)
	w2 := make(chan error, 1)
	go func() { w1 <- eng.Put(testCtx(), []byte("acc_w1"), []byte("1")) }()
	go func() { w2 <- eng.Delete(testCtx(), []byte("acc_base")) }()
	time.Sleep(150 * time.Millisecond)
	select {
	case err := <-w1:
		t.Fatalf("w1 completed while stalled: %v", err)
	default:
	}
	select {
	case err := <-w2:
		t.Fatalf("w2 completed while stalled: %v", err)
	default:
	}
	if got, err := eng.Get([]byte("acc_base")); err != nil || string(got) != "b" {
		t.Fatalf("Get during stall = %q %v", got, err)
	}
	// Relief: drop 6 oldest files (compaction would merge+replace; count drop
	// is the observable writers key on).
	drop := map[uint64]bool{100: true, 101: true, 102: true, 103: true, 104: true, 105: true}
	dropL0Files(t, eng, drop)
	if got := l0Count(t, eng); got != 7 {
		t.Fatalf("L0 after relief = %d, want 7", got)
	}
	for i, ch := range []chan error{w1, w2} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("writer %d after relief: %v", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("writer %d did not resume", i)
		}
	}
	if got, err := eng.Get([]byte("acc_w1")); err != nil || string(got) != "1" {
		t.Fatalf("w1 value = %q %v", got, err)
	}
	if _, err := eng.Get([]byte("acc_base")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("stalled delete lost: %v", err)
	}
}
