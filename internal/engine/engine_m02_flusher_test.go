package engine_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
	"github.com/silent-knight19/lattice/internal/version"
	"github.com/silent-knight19/lattice/internal/wal"
)

// noopWAL satisfies engine walWriter without I/O for bulk throughput tests.
type noopWAL struct{}

func (noopWAL) AppendSync(rec wal.Record) error { return nil }
func (noopWAL) Close() error                    { return nil }

func newFlushEngine(t *testing.T, threshold uint64) (*engine.Engine, string) {
	t.Helper()
	dir := t.TempDir()
	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath: dir,
		Backpressure: engine.BackpressureConfig{
			MaxMemoryBytes: 256 * 1024 * 1024, HighWatermark: 0.80, HardWatermark: 0.95, MaxWaitTimeout: 5 * time.Second,
		},
		WAL: noopWAL{},
	})
	if threshold > 0 {
		eng.SetFlushThresholdForTesting(threshold)
	}
	if err := eng.Open(); err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	return eng, dir
}

func waitFlushEmpty(t *testing.T, eng *engine.Engine, timeoutMs int) {
	t.Helper()
	if !eng.WaitForFlushQueueEmptyForTesting(timeoutMs) {
		t.Fatalf("flush queue did not drain (len=%d err=%v)", eng.FlushQueueLen(), eng.FlushError())
	}
	// Allow VersionSet publication to settle (retirement happens synchronously
	// after publish, but poll briefly for count visibility).
	deadline := 2000
	for deadline > 0 {
		if eng.FlushQueueLen() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
		deadline -= 5
	}
}

func l0FileNums(t *testing.T, eng *engine.Engine) []uint64 {
	t.Helper()
	ver := eng.VersionSet().Current()
	if ver == nil {
		return nil
	}
	defer ver.Unref()
	files := ver.Files(0)
	nums := make([]uint64, 0, len(files))
	for _, f := range files {
		nums = append(nums, f.FileNum)
	}
	return nums
}

func TestEngineM02_RotationAtThreshold(t *testing.T) {
	eng, _ := newFlushEngine(t, 4096)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	before := eng.ActiveMemTable()
	i := 0
	rotated := false
	for i = 0; i < 500; i++ {
		k := fmt.Sprintf("rk_%04d", i)
		v := bytes.Repeat([]byte{byte(i)}, 128)
		if err := eng.Put(ctx, []byte(k), v); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
		if eng.ActiveMemTable() != before {
			rotated = true
			break
		}
	}
	if !rotated {
		t.Fatalf("expected rotation with 4KB threshold after %d puts", i)
	}
	if n := eng.FlushQueueLen(); n < 1 {
		t.Fatalf("expected >=1 immutable after rotation, got %d", n)
	}
	// Old active is frozen and readable during flush.
	if got, err := eng.Get([]byte("rk_0000")); err != nil || len(got) == 0 {
		t.Fatalf("Get during flush failed: %q %v", got, err)
	}
	// Writes continue to fresh active.
	if err := eng.Put(ctx, []byte("after_rotate"), []byte("v")); err != nil {
		t.Fatalf("Put after rotation failed: %v", err)
	}
	waitFlushEmpty(t, eng, 30000)
	if eng.FlushQueueLen() != 0 {
		t.Fatalf("expected queue drained, got %d", eng.FlushQueueLen())
	}
	if eng.FlushCount() < 1 {
		t.Fatalf("expected >=1 successful flush, got %d", eng.FlushCount())
	}
	if nums := l0FileNums(t, eng); len(nums) < 1 {
		t.Fatalf("expected >=1 L0 file, got %v", nums)
	}
	if got, err := eng.Get([]byte("rk_0000")); err != nil || len(got) == 0 {
		t.Fatalf("post-flush Get failed: %q %v", got, err)
	}
	if got, err := eng.Get([]byte("after_rotate")); err != nil || string(got) != "v" {
		t.Fatalf("post-flush fresh-active Get failed: %q %v", got, err)
	}
}

func TestEngineM02_NoOverRotate(t *testing.T) {
	eng, _ := newFlushEngine(t, 64<<20)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	for i := 0; i < 20; i++ {
		if err := eng.Put(ctx, []byte(fmt.Sprintf("small_%d", i)), []byte("v")); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
	}
	if n := eng.FlushQueueLen(); n != 0 {
		t.Fatalf("must not rotate below 64MB threshold, queue=%d", n)
	}
}

func TestEngineM02_TombstoneFlushShadows(t *testing.T) {
	eng, _ := newFlushEngine(t, 4096)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	if err := eng.Put(ctx, []byte("tk"), []byte("V1")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	waitFlushEmpty(t, eng, 30000)
	// Ensure V1 is in L0 (force rotation if still in active).
	if eng.FlushQueueLen() != 0 {
		waitFlushEmpty(t, eng, 30000)
	}
	if err := eng.Delete(ctx, []byte("tk")); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	// Tombstone in active/imm must shadow L0 immediately, before second flush.
	if _, err := eng.Get([]byte("tk")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("expected NotFound after tombstone, got %v", err)
	}
	waitFlushEmpty(t, eng, 30000)
	if _, err := eng.Get([]byte("tk")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("tombstone lost after flush, got %v", err)
	}
	// Resurrect with V2 across another flush.
	if err := eng.Put(ctx, []byte("tk"), []byte("V2")); err != nil {
		t.Fatalf("re-Put failed: %v", err)
	}
	waitFlushEmpty(t, eng, 30000)
	if got, err := eng.Get([]byte("tk")); err != nil || string(got) != "V2" {
		t.Fatalf("expected V2, got %q %v", got, err)
	}
}

func TestEngineM02_MultiWriteSameKeyBeforeRotation(t *testing.T) {
	eng, _ := newFlushEngine(t, 64<<20)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	// Multiple revisions in one generation; no dedup on flush.
	if err := eng.Put(ctx, []byte("mk"), []byte("V1")); err != nil {
		t.Fatalf("Put V1: %v", err)
	}
	if err := eng.Put(ctx, []byte("mk"), []byte("V2")); err != nil {
		t.Fatalf("Put V2: %v", err)
	}
	if err := eng.Delete(ctx, []byte("mk")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Force flush by lowering threshold and issuing one more write.
	eng.SetFlushThresholdForTesting(1)
	if err := eng.Put(ctx, []byte("mk_trigger"), []byte("x")); err != nil {
		t.Fatalf("trigger Put: %v", err)
	}
	waitFlushEmpty(t, eng, 30000)
	if _, err := eng.Get([]byte("mk")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("expected tombstone to survive flush, got %v", err)
	}
}

func TestEngineM02_UpdatesAcrossFlushes(t *testing.T) {
	eng, _ := newFlushEngine(t, 4096)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	if err := eng.Put(ctx, []byte("uk"), []byte("V1")); err != nil {
		t.Fatalf("Put V1: %v", err)
	}
	waitFlushEmpty(t, eng, 30000)
	if err := eng.Put(ctx, []byte("uk"), []byte("V2")); err != nil {
		t.Fatalf("Put V2: %v", err)
	}
	waitFlushEmpty(t, eng, 30000)
	got, err := eng.Get([]byte("uk"))
	if err != nil || string(got) != "V2" {
		t.Fatalf("expected V2 across flushes, got %q %v", got, err)
	}
	nums := l0FileNums(t, eng)
	seen := map[uint64]bool{}
	for _, n := range nums {
		if n == 0 || seen[n] {
			t.Fatalf("duplicate/zero FileNum in %v", nums)
		}
		seen[n] = true
	}
}

func TestEngineM02_L0Integrity(t *testing.T) {
	eng, dir := newFlushEngine(t, 4096)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	for i := 0; i < 100; i++ {
		if err := eng.Put(ctx, []byte(fmt.Sprintf("ik_%04d", i)), []byte(fmt.Sprintf("val_%d", i))); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	waitFlushEmpty(t, eng, 30000)
	for _, n := range l0FileNums(t, eng) {
		path := version.TablePath(dir, n)
		r, err := sstable.NewTableReader(path)
		if err != nil {
			t.Fatalf("TableReader open %d failed: %v", n, err)
		}
		it, err := r.NewIterator()
		if err != nil {
			_ = r.Close()
			t.Fatalf("iterator failed: %v", err)
		}
		count := 0
		for it.Next() {
			count++
		}
		if err := it.Err(); err != nil {
			_ = it.Close()
			_ = r.Close()
			t.Fatalf("iteration failed for %d: %v", n, err)
		}
		_ = it.Close()
		_ = r.Close()
		if count == 0 {
			t.Fatalf("L0 file %d has no records", n)
		}
	}
	// Spot-check reads through Engine.
	for _, i := range []int{0, 50, 99} {
		k := fmt.Sprintf("ik_%04d", i)
		if got, err := eng.Get([]byte(k)); err != nil || string(got) != fmt.Sprintf("val_%d", i) {
			t.Fatalf("Get %s = %q %v", k, got, err)
		}
	}
}

func TestEngineM02_FlushFailureRetainsImm(t *testing.T) {
	eng, _ := newFlushEngine(t, 4096)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	boom := stdErrors.New("injected table writer failure")
	restore := eng.SetTableWriterFactoryForTesting(func(path string, opts sstable.TableWriterOptions) (*sstable.TableWriter, error) {
		return nil, boom
	})
	defer restore()

	for i := 0; i < 50; i++ {
		if err := eng.Put(ctx, []byte(fmt.Sprintf("fk_%04d", i)), []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// Wait for worker to attempt and record failure (queue retained).
	deadline := 5000
	for deadline > 0 && eng.FlushError() == nil {
		time.Sleep(5 * time.Millisecond)
		deadline -= 5
	}
	if eng.FlushError() == nil {
		t.Fatalf("expected flush error, got nil (queue=%d)", eng.FlushQueueLen())
	}
	if eng.FlushQueueLen() < 1 {
		t.Fatalf("failed flush must retain imm, queue=%d", eng.FlushQueueLen())
	}
	if nums := l0FileNums(t, eng); len(nums) != 0 {
		t.Fatalf("failed flush must publish no SSTable, got %v", nums)
	}
	// Reads still see imm (no visibility gap, no resurrect).
	if _, err := eng.Get([]byte("fk_0000")); err != nil {
		t.Fatalf("imm must remain readable after failure: %v", err)
	}
}

func TestEngineM02_VersionApplyFailureRetainsImm(t *testing.T) {
	eng, dir := newFlushEngine(t, 4096)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	applyBoom := stdErrors.New("injected version apply failure")
	restore := eng.SetVersionApplyForTesting(func(edit *version.VersionEdit) error { return applyBoom })
	defer restore()

	for i := 0; i < 50; i++ {
		if err := eng.Put(ctx, []byte(fmt.Sprintf("vk_%04d", i)), []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	deadline := 5000
	for deadline > 0 && eng.FlushError() == nil {
		time.Sleep(5 * time.Millisecond)
		deadline -= 5
	}
	if eng.FlushError() == nil {
		t.Fatalf("expected version apply error")
	}
	if eng.FlushQueueLen() < 1 {
		t.Fatalf("imm must be retained after publish failure")
	}
	// Orphan SSTable may exist on disk but must be invisible.
	entries, _ := os.ReadDir(dir)
	l0count := 0
	ver := eng.VersionSet().Current()
	if ver != nil {
		l0count = ver.NumFiles(0)
		ver.Unref()
	}
	_ = entries
	if l0count != 0 {
		t.Fatalf("failed publish must not install Version, L0=%d", l0count)
	}
	if _, err := eng.Get([]byte("vk_0000")); err != nil {
		t.Fatalf("imm readable after publish failure: %v", err)
	}
}

func TestEngineM02_ConcurrentRotation(t *testing.T) {
	eng, _ := newFlushEngine(t, 8192)
	defer func() { _ = eng.Close() }()

	const writers = 8
	const per = 200
	ref := sync.Map{}
	var wg sync.WaitGroup
	errCh := make(chan error, writers*per)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			ctx := testCtx()
			for i := 0; i < per; i++ {
				k := fmt.Sprintf("cw%d_%04d", w, i)
				v := fmt.Sprintf("v%d", i)
				if err := eng.Put(ctx, []byte(k), []byte(v)); err != nil {
					errCh <- err
					return
				}
				ref.Store(k, v)
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("writer error: %v", err)
	}
	waitFlushEmpty(t, eng, 60000)
	var mismatches int
	ref.Range(func(k, v any) bool {
		ks, ok := k.(string)
		if !ok {
			mismatches++
			return true
		}
		vs, ok := v.(string)
		if !ok {
			mismatches++
			return true
		}
		got, err := eng.Get([]byte(ks))
		if err != nil || string(got) != vs {
			mismatches++
		}
		return true
	})
	if mismatches != 0 {
		t.Fatalf("%d mismatches after concurrent rotation", mismatches)
	}
	// FileNums unique.
	nums := l0FileNums(t, eng)
	seen := map[uint64]bool{}
	for _, n := range nums {
		if seen[n] || n == 0 {
			t.Fatalf("bad FileNums %v", nums)
		}
		seen[n] = true
	}
}
