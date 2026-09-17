package engine

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	lerrors "github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
	"github.com/silent-knight19/lattice/internal/version"
)

// TestAudit_SeqOverflowFailsClosed verifies sequence allocation refuses to wrap.
func TestAudit_SeqOverflowFailsClosed(t *testing.T) {
	e := NewEngine(DefaultBackpressureConfig())
	e.nextSeqNum.Store(uint64(binary.MaxSeqNum))
	if _, err := e.allocSeqNumLocked(); !errors.Is(err, lerrors.ErrSeqNumOverflow) {
		t.Fatalf("expected ErrSeqNumOverflow, got %v", err)
	}
	if got := e.NextSeqNum(); got != uint64(binary.MaxSeqNum) {
		t.Fatalf("overflow attempt consumed a number: watermark=%d", got)
	}
	e.nextSeqNum.Store(uint64(binary.MaxSeqNum) - 1)
	seq, err := e.allocSeqNumLocked()
	if err != nil {
		t.Fatalf("expected success at Max-1, got %v", err)
	}
	if seq != binary.MaxSeqNum {
		t.Fatalf("expected seq MaxSeqNum, got %d", seq)
	}
	if _, err := e.allocSeqNumLocked(); !errors.Is(err, lerrors.ErrSeqNumOverflow) {
		t.Fatalf("expected overflow after Max, got %v", err)
	}
}

// TestAudit_SafeUint64FromInt verifies negative clamping (gosec G115).
func TestAudit_SafeUint64FromInt(t *testing.T) {
	if got := safeUint64FromInt(-1); got != 0 {
		t.Fatalf("negative must saturate to 0, got %d", got)
	}
	if got := safeUint64FromInt(41); got != 41 {
		t.Fatalf("positive must convert exactly, got %d", got)
	}
}

// TestAudit_FlushErrorClearedOnSuccess verifies FlushError does not go stale.
func TestAudit_FlushErrorClearedOnSuccess(t *testing.T) {
	e := NewEngine(DefaultBackpressureConfig())
	if err := e.FlushError(); err != nil {
		t.Fatalf("fresh engine must have nil FlushError, got %v", err)
	}
	e.recordFlushError(os.ErrInvalid)
	if e.FlushError() == nil {
		t.Fatal("recorded error must be observable")
	}
	e.clearFlushError()
	if e.FlushError() != nil {
		t.Fatalf("successful retirement must clear FlushError, got %v", e.FlushError())
	}
	// Clearing twice must be safe.
	e.clearFlushError()
}

// TestAudit_DrainClearsFlushErrorAfterRetry drives the synchronous shutdown
// drain through failure then success and verifies the retained error clears.
func TestAudit_DrainClearsFlushErrorAfterRetry(t *testing.T) {
	dir := t.TempDir()
	e := NewEngineWithOptions(EngineOptions{DBPath: dir, Backpressure: DefaultBackpressureConfig()})
	boom := errors.New("injected apply failure")
	e.flushCfgMu.Lock()
	e.versionApply = func(edit *version.VersionEdit) error { return boom }
	e.flushCfgMu.Unlock()

	imm := memtable.NewSkipList()
	ik, err := binary.NewInternalKey([]byte("k"), 1, binary.OpTypePut)
	if err != nil {
		t.Fatalf("NewInternalKey: %v", err)
	}
	if err := imm.Insert(ik, []byte("v")); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	imm.Freeze()
	e.mu.Lock()
	e.immMems = append(e.immMems, imm)
	e.mu.Unlock()

	if err := e.drainForShutdown(); !errors.Is(err, boom) {
		t.Fatalf("expected injected failure, got %v", err)
	}
	if e.FlushError() == nil {
		t.Fatal("failure must be retained in FlushError")
	}
	if n := e.FlushQueueLen(); n != 1 {
		t.Fatalf("failed generation must stay queued, len=%d", n)
	}

	e.flushCfgMu.Lock()
	e.versionApply = func(edit *version.VersionEdit) error { return nil }
	e.flushCfgMu.Unlock()
	if err := e.drainForShutdown(); err != nil {
		t.Fatalf("retry must succeed, got %v", err)
	}
	if n := e.FlushQueueLen(); n != 0 {
		t.Fatalf("queue must drain, len=%d", n)
	}
	if e.FlushError() != nil {
		t.Fatalf("success must clear FlushError, got %v", e.FlushError())
	}
	if c := e.FlushCount(); c != 1 {
		t.Fatalf("exactly one generation retired, count=%d", c)
	}
}

// TestAudit_FlushOneFileNumOverflowPreCheck verifies the allocator-exhausted
// path fails before creating any SSTable file on disk.
func TestAudit_FlushOneFileNumOverflowPreCheck(t *testing.T) {
	dir := t.TempDir()
	e := NewEngineWithOptions(EngineOptions{DBPath: dir, Backpressure: DefaultBackpressureConfig()})
	e.nextFileNum.Store(^uint64(0))
	imm := memtable.NewSkipList()
	ik, err := binary.NewInternalKey([]byte("k"), 1, binary.OpTypePut)
	if err != nil {
		t.Fatalf("NewInternalKey: %v", err)
	}
	if err := imm.Insert(ik, []byte("v")); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	imm.Freeze()
	if err := e.flushOne(imm); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("expected file-number overflow (ErrInvalid), got %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, en := range entries {
		t.Fatalf("overflow must not create files, found %s", en.Name())
	}
}

// TestAudit_DrainForShutdown_Timeout verifies that a stalled flush causes drainForShutdown
// to abort cleanly within the configured ShutdownTimeout (SEC-P10-003).
func TestAudit_DrainForShutdown_Timeout(t *testing.T) {
	dir := t.TempDir()
	e := NewEngineWithOptions(EngineOptions{
		DBPath:          dir,
		Backpressure:    DefaultBackpressureConfig(),
		ShutdownTimeout: 30 * time.Millisecond,
	})

	// Inject a slow versionApply that sleeps longer than ShutdownTimeout
	e.flushCfgMu.Lock()
	e.versionApply = func(edit *version.VersionEdit) error {
		time.Sleep(100 * time.Millisecond)
		return nil
	}
	e.flushCfgMu.Unlock()

	// Queue two immutable memtables
	for i := 0; i < 2; i++ {
		imm := memtable.NewSkipList()
		ik, err := binary.NewInternalKey([]byte(fmt.Sprintf("k%d", i)), binary.SeqNum(i+1), binary.OpTypePut)
		if err != nil {
			t.Fatalf("NewInternalKey: %v", err)
		}
		if err := imm.Insert(ik, []byte("v")); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		imm.Freeze()
		e.mu.Lock()
		e.immMems = append(e.immMems, imm)
		e.mu.Unlock()
	}

	start := time.Now()
	err := e.drainForShutdown()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected drainForShutdown to return timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "shutdown drain timeout exceeded") {
		t.Fatalf("expected timeout error message, got: %v", err)
	}
	// Verify that the second generation was not flushed due to timeout abort
	if qLen := e.FlushQueueLen(); qLen == 0 {
		t.Fatal("expected remaining generations to stay queued upon timeout abort")
	}
	t.Logf("drain timeout aborted in %v as expected: %v", elapsed, err)
}
