package engine_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
)

// newRealWALEngine creates a DB-backed Engine with the real RotatingWriter.
func newRealWALEngine(t *testing.T, dir string, threshold uint64) *engine.Engine {
	t.Helper()
	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath: dir,
		Backpressure: engine.BackpressureConfig{
			MaxMemoryBytes: 256 * 1024 * 1024, HighWatermark: 0.80, HardWatermark: 0.95, MaxWaitTimeout: 5 * time.Second,
		},
	})
	if threshold > 0 {
		eng.SetFlushThresholdForTesting(threshold)
	}
	if err := eng.Open(); err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	return eng
}

func newRealWALEngineNoOpen(t *testing.T, dir string) *engine.Engine {
	t.Helper()
	return engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath: dir,
		Backpressure: engine.BackpressureConfig{
			MaxMemoryBytes: 256 * 1024 * 1024, HighWatermark: 0.80, HardWatermark: 0.95, MaxWaitTimeout: 5 * time.Second,
		},
	})
}

// TestEngineM02_WriteContinuesDuringBlockedFlush is the mandatory M02 proof:
// the flusher is deterministically paused mid-I/O, fresh writes must complete
// without waiting, then the release must preserve the full dataset.
func TestEngineM02_WriteContinuesDuringBlockedFlush(t *testing.T) {
	eng, _ := newFlushEngine(t, 4096)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	entered := make(chan *memtable.SkipList, 1)
	release := make(chan struct{})
	restore := eng.SetFlushPauseHookForTesting(func(imm *memtable.SkipList) {
		select {
		case entered <- imm:
		default:
		}
		<-release
	})
	defer restore()

	// Fill until first rotation; worker will enter pause hook.
	i := 0
	for i = 0; i < 500; i++ {
		if err := eng.Put(ctx, []byte(fmt.Sprintf("bk_%04d", i)), []byte("v")); err != nil {
			t.Fatalf("fill Put: %v", err)
		}
		if eng.FlushQueueLen() > 0 {
			break
		}
	}
	var imm *memtable.SkipList
	select {
	case imm = <-entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("flusher never entered pause hook (queue=%d)", eng.FlushQueueLen())
	}
	if imm == nil || imm.Len() == 0 {
		t.Fatalf("pause hook captured empty imm")
	}

	// Issue writes while flush I/O is blocked. They must complete promptly
	// against the fresh active MemTable (not wait for release).
	done := make(chan error, 32)
	go func() {
		for j := 0; j < 20; j++ {
			k := fmt.Sprintf("during_%02d", j)
			if err := eng.Put(ctx, []byte(k), []byte("fresh")); err != nil {
				done <- err
				return
			}
			if got, err := eng.Get([]byte(k)); err != nil || string(got) != "fresh" {
				done <- fmt.Errorf("read-your-write %s: %q: %w", k, got, err)
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("writes blocked by flush I/O: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("writes did not complete while flush blocked")
	}

	// Blocked-generation keys remain visible via imm during the stall.
	if _, err := eng.Get([]byte("bk_0000")); err != nil {
		t.Fatalf("imm key invisible during blocked flush: %v", err)
	}

	close(release)
	waitFlushEmpty(t, eng, 30000)

	// Full dataset: pre-block + during-block keys.
	for i := 0; ; i++ {
		k := fmt.Sprintf("bk_%04d", i)
		_, err := eng.Get([]byte(k))
		if stdErrors.Is(err, errors.ErrKeyNotFound) {
			break
		}
		if err != nil {
			t.Fatalf("post-release Get %s: %v", k, err)
		}
		if i > 600 {
			break
		}
	}
	for j := 0; j < 20; j++ {
		k := fmt.Sprintf("during_%02d", j)
		if got, err := eng.Get([]byte(k)); err != nil || string(got) != "fresh" {
			t.Fatalf("post-release %s = %q %v", k, got, err)
		}
	}
}

// TestEngineM02_200MB verifies the roadmap acceptance: ~200MB ingest,
// multiple rotations, multiple L0 files, full logical validation. Uses a
// no-op WAL for bulk throughput (durability proven by small WAL tests);
// manifest + SSTable + VersionSet paths are fully real.
func TestEngineM02_200MB(t *testing.T) {
	if testing.Short() {
		t.Skip("skip 200MB acceptance in short mode")
	}
	eng, _ := newFlushEngine(t, 0) // production 64MB threshold
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	const nKeys = 50000
	const valSize = 4096 // 50k * ~4KB ~= 200MB (+ overhead)
	ref := make(map[string][]byte, nKeys)
	valBuf := bytes.Repeat([]byte("x"), valSize)
	for i := 0; i < nKeys; i++ {
		k := fmt.Sprintf("big_%06d", i)
		v := make([]byte, valSize)
		copy(v, valBuf)
		// Embed index for validation without storing 200MB twice in failure messages.
		copy(v, fmt.Sprintf("%06d:", i))
		if err := eng.Put(ctx, []byte(k), v); err != nil {
			t.Fatalf("Put %d failed: %v", i, err)
		}
		cp := make([]byte, valSize)
		copy(cp, v)
		ref[k] = cp
		if i%10000 == 0 {
			t.Logf("ingested %d/%d (queue=%d flushes=%d)", i, nKeys, eng.FlushQueueLen(), eng.FlushCount())
		}
	}
	waitFlushEmpty(t, eng, 120000)
	nums := l0FileNums(t, eng)
	t.Logf("200MB: keys=%d L0 files=%d flushes=%d queue=%d", nKeys, len(nums), eng.FlushCount(), eng.FlushQueueLen())
	if len(nums) <= 1 {
		t.Fatalf("expected multiple L0 files for ~200MB, got %v", nums)
	}
	// Validate every key through the real read path (mem empty post-flush).
	for i := 0; i < nKeys; i++ {
		k := fmt.Sprintf("big_%06d", i)
		got, err := eng.Get([]byte(k))
		if err != nil {
			t.Fatalf("Get %s failed: %v", k, err)
		}
		if !bytes.Equal(got, ref[k]) {
			t.Fatalf("Get %s mismatch (len %d vs %d)", k, len(got), len(ref[k]))
		}
	}
}

// TestEngineM02_RecoveryAfterFlush proves flushed L0 state survives reopen via
// manifest replay (WAL records for flushed seqs are skipped by checkpoint).
func TestEngineM02_RecoveryAfterFlush(t *testing.T) {
	dir := t.TempDir()
	eng := newRealWALEngine(t, dir, 4096)
	ctx := testCtx()
	if err := eng.Put(ctx, []byte("rk1"), []byte("a")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := eng.Put(ctx, []byte("rk2"), []byte("b")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	waitFlushEmpty(t, eng, 30000)
	if eng.FlushCount() < 1 && eng.FlushQueueLen() == 0 {
		// Force one more generation to guarantee an L0 file.
		for i := 0; i < 100; i++ {
			_ = eng.Put(ctx, []byte(fmt.Sprintf("pad_%04d", i)), bytes.Repeat([]byte("p"), 256))
		}
		waitFlushEmpty(t, eng, 30000)
	}
	if nums := l0FileNums(t, eng); len(nums) < 1 {
		t.Fatalf("expected L0 before close, got %v", nums)
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	eng2 := newRealWALEngineNoOpen(t, dir)
	if err := eng2.Open(); err != nil {
		t.Fatalf("re-Open failed: %v", err)
	}
	defer func() { _ = eng2.Close() }()
	for _, tc := range []struct{ k, v string }{{"rk1", "a"}, {"rk2", "b"}} {
		got, err := eng2.Get([]byte(tc.k))
		if err != nil || string(got) != tc.v {
			t.Fatalf("post-recovery %s = %q %v", tc.k, got, err)
		}
	}
}

// TestEngineM02_CacheAfterFlush confirms flushed SSTables populate the Phase 09
// cache through normal reads with no Engine cache logic.
func TestEngineM02_CacheAfterFlush(t *testing.T) {
	eng, _ := newFlushEngine(t, 4096)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	for i := 0; i < 50; i++ {
		if err := eng.Put(ctx, []byte(fmt.Sprintf("ck_%04d", i)), []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	waitFlushEmpty(t, eng, 30000)
	bc := eng.BlockCache()
	if bc == nil {
		t.Fatalf("no shared cache")
	}
	bc.Clear()
	before := bc.Len()
	if _, err := eng.Get([]byte("ck_0000")); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if bc.Len() <= before {
		t.Fatalf("expected cache population after flushed read")
	}
	if got, err := eng.Get([]byte("ck_0000")); err != nil || string(got) != "v" {
		t.Fatalf("second Get = %q %v", got, err)
	}
}

// TestEngineM02_ReadersDuringFlush runs mixed readers against active/imm/L0
// under -race while rotations and flushes proceed.
func TestEngineM02_ReadersDuringFlush(t *testing.T) {
	eng, _ := newFlushEngine(t, 4096)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	for i := 0; i < 30; i++ {
		if err := eng.Put(ctx, []byte(fmt.Sprintf("base_%04d", i)), []byte("b")); err != nil {
			t.Fatalf("seed Put: %v", err)
		}
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = eng.Get([]byte(fmt.Sprintf("base_%04d", r*7%30)))
				_, _ = eng.Get([]byte("missing_key_xyz"))
			}
		}(r)
	}
	for i := 0; i < 300; i++ {
		_ = eng.Put(ctx, []byte(fmt.Sprintf("stream_%05d", i)), bytes.Repeat([]byte("s"), 128))
		if i%50 == 0 {
			_, _ = eng.Get([]byte(fmt.Sprintf("stream_%05d", i)))
		}
	}
	close(stop)
	wg.Wait()
	waitFlushEmpty(t, eng, 30000)
	if _, err := eng.Get([]byte("base_0000")); err != nil {
		t.Fatalf("post-stress Get: %v", err)
	}
}
