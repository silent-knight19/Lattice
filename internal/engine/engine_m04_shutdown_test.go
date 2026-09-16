package engine_test

import (
	stdErrors "errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
	"github.com/silent-knight19/lattice/internal/sstable"
	"github.com/silent-knight19/lattice/internal/version"
	"github.com/silent-knight19/lattice/internal/wal"
)

// openExistingDir opens a fresh Engine over an existing database directory.
func openExistingDir(t *testing.T, dir string) *engine.Engine {
	t.Helper()
	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath: dir,
		Backpressure: engine.BackpressureConfig{
			MaxMemoryBytes: 256 * 1024 * 1024, HighWatermark: 0.80, HardWatermark: 0.95, MaxWaitTimeout: 5 * time.Second,
		},
	})
	if err := eng.Open(); err != nil {
		t.Fatalf("re-Open failed: %v", err)
	}
	return eng
}

func TestEngineM04_IdempotentConcurrentClose(t *testing.T) {
	eng, _ := newFlushEngine(t, 4096)
	ctx := testCtx()
	if err := eng.Put(ctx, []byte("k"), []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = eng.Close()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Close %d returned %v, want nil", i, err)
		}
	}
	// Repeated sequential Close after concurrent Close: same nil result.
	if err := eng.Close(); err != nil {
		t.Fatalf("final Close: %v", err)
	}
}

func TestEngineM04_RejectsMutationsServesGets(t *testing.T) {
	eng, _ := newFlushEngine(t, 4096)
	ctx := testCtx()
	if err := eng.Put(ctx, []byte("gk"), []byte("gv")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := eng.Put(ctx, []byte("k2"), []byte("v")); !stdErrors.Is(err, errors.ErrWriterClosed) {
		t.Fatalf("Put after Close = %v, want ErrWriterClosed", err)
	}
	if err := eng.Delete(ctx, []byte("gk")); !stdErrors.Is(err, errors.ErrWriterClosed) {
		t.Fatalf("Delete after Close = %v, want ErrWriterClosed", err)
	}
	// Established contract: Get keeps serving from retired/published state.
	got, err := eng.Get([]byte("gk"))
	if err != nil || string(got) != "gv" {
		t.Fatalf("Get after Close = %q %v, want gv", got, err)
	}
	if err := eng.Open(); !stdErrors.Is(err, errors.ErrWriterClosed) {
		t.Fatalf("Open after Close = %v, want ErrWriterClosed", err)
	}
}

func TestEngineM04_CloseEmpty(t *testing.T) {
	// Fresh directory, no writes: no bogus SSTables, nil error.
	eng, dir := newFlushEngine(t, 0)
	if err := eng.Close(); err != nil {
		t.Fatalf("Close empty: %v", err)
	}
	if nums := l0FileNums(t, eng); len(nums) != 0 {
		t.Fatalf("empty Close created L0 files: %v", nums)
	}
	// Recovered directory, no new writes.
	eng2 := openExistingDir(t, dir)
	if err := eng2.Close(); err != nil {
		t.Fatalf("Close recovered-empty: %v", err)
	}
	// Memory-only mode: safe, no SSTable possible.
	mem := newMemEngine()
	if err := mem.Close(); err != nil {
		t.Fatalf("Close memory-only: %v", err)
	}
}

func TestEngineM04_CloseDrainsActiveBelowThreshold(t *testing.T) {
	dir := t.TempDir()
	eng := newRealWALEngine(t, dir, 0) // production 64MB threshold: no rotation
	defer func() { _ = eng.Close() }()
	ctx := testCtx()
	ref := map[string]string{"a": "1", "b": "2", "c": "3"}
	for k, v := range ref {
		if err := eng.Put(ctx, []byte(k), []byte(v)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := eng.Delete(ctx, []byte("gone")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n := eng.FlushQueueLen(); n != 0 {
		t.Fatalf("queue should be empty pre-close, got %d", n)
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if nums := l0FileNums(t, eng); len(nums) != 1 {
		t.Fatalf("active drain should publish exactly 1 L0 file, got %v", nums)
	}
	if eng.FlushQueueLen() != 0 {
		t.Fatalf("queue not empty after Close")
	}

	eng2 := openExistingDir(t, dir)
	defer func() { _ = eng2.Close() }()
	for k, v := range ref {
		got, err := eng2.Get([]byte(k))
		if err != nil || string(got) != v {
			t.Fatalf("reopened Get(%s) = %q %v", k, got, err)
		}
	}
	if _, err := eng2.Get([]byte("gone")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("tombstone lost across Close/reopen: %v", err)
	}
}

func TestEngineM04_CloseDrainsImmQueue(t *testing.T) {
	dir := t.TempDir()
	eng := newRealWALEngine(t, dir, 4096)
	ctx := testCtx()
	ref := map[string]string{}
	for i := 0; i < 120; i++ {
		k := fmt.Sprintf("q_%04d", i)
		v := fmt.Sprintf("v_%d", i)
		ref[k] = v
		if err := eng.Put(ctx, []byte(k), []byte(v)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// Update + delete across generations.
	ref["q_0001"] = "updated"
	if err := eng.Put(ctx, []byte("q_0001"), []byte("updated")); err != nil {
		t.Fatalf("update: %v", err)
	}
	delete(ref, "q_0002")
	if err := eng.Delete(ctx, []byte("q_0002")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("Close with queued imm: %v", err)
	}
	if eng.FlushQueueLen() != 0 {
		t.Fatalf("queue not drained by Close")
	}
	if nums := l0FileNums(t, eng); len(nums) < 1 {
		t.Fatalf("expected L0 files after drain, got none")
	}

	eng2 := openExistingDir(t, dir)
	defer func() { _ = eng2.Close() }()
	for k, v := range ref {
		got, err := eng2.Get([]byte(k))
		if err != nil || string(got) != v {
			t.Fatalf("reopened Get(%s) = %q %v, want %q", k, got, err, v)
		}
	}
	if _, err := eng2.Get([]byte("q_0002")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("deleted key resurrected after reopen: %v", err)
	}
}

// TestEngineM04_CloseDuringActiveFlush proves Close coordinates with an
// in-flight flush: it waits (no premature WAL close underneath the worker)
// and completes once the blocked I/O resolves.
func TestEngineM04_CloseDuringActiveFlush(t *testing.T) {
	eng, _ := newFlushEngine(t, 4096)
	ctx := testCtx()
	// Install the hook before any write so the worker deterministically parks
	// inside flushOne (no race with pre-hook background flushes).
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	restore := eng.SetFlushPauseHookForTesting(func(imm *memtable.SkipList) {
		_ = imm
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	})
	defer restore()

	for i := 0; i < 120 && eng.FlushQueueLen() == 0; i++ {
		if err := eng.Put(ctx, []byte(fmt.Sprintf("f_%04d", i)), []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if eng.FlushQueueLen() == 0 {
		t.Fatalf("no rotation queued")
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("worker never entered blocked flush")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- eng.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned while flush I/O blocked: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close after release: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("Close did not complete after flush unblocked")
	}
	if eng.FlushQueueLen() != 0 {
		t.Fatalf("queue not drained")
	}
}

// TestEngineM04_CloseDuringStallAndFlush combines a parked flush with an M03
// stalled writer: the writer must fail fast with ErrWriterClosed while Close
// still waits for the flush I/O it owns.
func TestEngineM04_CloseDuringStallAndFlush(t *testing.T) {
	eng, _ := newFlushEngine(t, 4096)
	ctx := testCtx()
	if err := eng.Put(ctx, []byte("pre"), []byte("v")); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	restoreHook := eng.SetFlushPauseHookForTesting(func(imm *memtable.SkipList) {
		_ = imm
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	})
	defer restoreHook()
	for i := 0; i < 60 && eng.FlushQueueLen() == 0; i++ {
		if err := eng.Put(ctx, []byte(fmt.Sprintf("h_%04d", i)), []byte("v")); err != nil {
			t.Fatalf("fill: %v", err)
		}
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("worker never parked")
	}

	eng.SetL0CountOverrideForTesting(13)
	stalled := make(chan error, 1)
	go func() { stalled <- eng.Put(ctx, []byte("stalled_key"), []byte("v")) }()

	closeDone := make(chan error, 1)
	time.Sleep(50 * time.Millisecond)
	go func() { closeDone <- eng.Close() }()

	select {
	case err := <-stalled:
		if !stdErrors.Is(err, errors.ErrWriterClosed) {
			t.Fatalf("stalled writer = %v, want ErrWriterClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("stalled writer not released by Close")
	}
	select {
	case err := <-closeDone:
		t.Fatalf("Close finished while flush I/O parked: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close after unblock: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("Close hung after flush unblocked")
	}
	if _, err := eng.Get([]byte("stalled_key")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("rejected stalled write became visible: %v", err)
	}
	if got, err := eng.Get([]byte("pre")); err != nil || string(got) != "v" {
		t.Fatalf("seed lost: %q %v", got, err)
	}
}

// failingWALClose delegates writes to a real writer but fails Close.
type walIface interface {
	AppendSync(wal.Record) error
	Close() error
}

type failingWALClose struct {
	inner walIface
	err   error
	calls atomic.Int32
}

func (f *failingWALClose) AppendSync(r wal.Record) error { return f.inner.AppendSync(r) }
func (f *failingWALClose) Close() error {
	f.calls.Add(1)
	_ = f.inner.Close()
	return f.err
}

func TestEngineM04_CloseFlushFailure(t *testing.T) {
	dir := t.TempDir()
	eng := newRealWALEngine(t, dir, 0)
	ctx := testCtx()
	boom := stdErrors.New("injected table writer failure")
	restore := eng.SetTableWriterFactoryForTesting(func(path string, opts sstable.TableWriterOptions) (*sstable.TableWriter, error) {
		return nil, boom
	})
	defer restore()
	ref := map[string]string{}
	for i := 0; i < 20; i++ {
		k := fmt.Sprintf("ff_%02d", i)
		ref[k] = "v"
		if err := eng.Put(ctx, []byte(k), []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	err := eng.Close()
	if !stdErrors.Is(err, boom) {
		t.Fatalf("Close with failed drain = %v, want %v", err, boom)
	}
	// Terminal state: repeated Close returns the same error, no retry storm.
	if err2 := eng.Close(); !stdErrors.Is(err2, boom) {
		t.Fatalf("second Close = %v, want same %v", err2, boom)
	}
	// In-memory state retained: Get still serves.
	if got, err := eng.Get([]byte("ff_00")); err != nil || string(got) != "v" {
		t.Fatalf("retained Get = %q %v", got, err)
	}
	// Fresh engine recovers every accepted mutation from durable WAL.
	eng2 := openExistingDir(t, dir)
	defer func() { _ = eng2.Close() }()
	for k, v := range ref {
		got, err := eng2.Get([]byte(k))
		if err != nil || string(got) != v {
			t.Fatalf("recovered Get(%s) = %q %v", k, got, err)
		}
	}
}

func TestEngineM04_CloseManifestFailure(t *testing.T) {
	dir := t.TempDir()
	eng := newRealWALEngine(t, dir, 0)
	ctx := testCtx()
	applyBoom := stdErrors.New("injected version apply failure")
	restore := eng.SetVersionApplyForTesting(func(edit *version.VersionEdit) error { return applyBoom })
	defer restore()
	for i := 0; i < 10; i++ {
		if err := eng.Put(ctx, []byte(fmt.Sprintf("mf_%02d", i)), []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := eng.Close(); !stdErrors.Is(err, applyBoom) {
		t.Fatalf("Close with failed publish = %v, want %v", err, applyBoom)
	}
	if nums := l0FileNums(t, eng); len(nums) != 0 {
		t.Fatalf("failed publish installed Version: %v", nums)
	}
	eng2 := openExistingDir(t, dir)
	defer func() { _ = eng2.Close() }()
	for i := 0; i < 10; i++ {
		k := fmt.Sprintf("mf_%02d", i)
		if got, err := eng2.Get([]byte(k)); err != nil || string(got) != "v" {
			t.Fatalf("recovered Get(%s) = %q %v", k, got, err)
		}
	}
}

func TestEngineM04_CloseWALCloseFailure(t *testing.T) {
	dir := t.TempDir()
	eng := newRealWALEngine(t, dir, 0)
	ctx := testCtx()
	for i := 0; i < 10; i++ {
		if err := eng.Put(ctx, []byte(fmt.Sprintf("wf_%02d", i)), []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	boom := stdErrors.New("injected wal close failure")
	inner, ok := eng.WAL().(walIface)
	if !ok || inner == nil {
		t.Fatalf("cannot capture real WAL for wrapper")
	}
	eng.SetWALForTesting(&failingWALClose{inner: inner, err: boom})
	if err := eng.Close(); !stdErrors.Is(err, boom) {
		t.Fatalf("Close with WAL failure = %v, want %v", err, boom)
	}
	// Manifest stayed durable: reopen sees the drained L0 state.
	eng2 := openExistingDir(t, dir)
	defer func() { _ = eng2.Close() }()
	for i := 0; i < 10; i++ {
		k := fmt.Sprintf("wf_%02d", i)
		if got, err := eng2.Get([]byte(k)); err != nil || string(got) != "v" {
			t.Fatalf("reopened Get(%s) = %q %v", k, got, err)
		}
	}
}
