package engine

import (
	stdErrors "errors"
	"fmt"
	"os"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
	"github.com/silent-knight19/lattice/internal/sstable"
	"github.com/silent-knight19/lattice/internal/version"
)

// flushThreshold returns the active MemTable size that triggers rotation.
// Production uses memtable.MaxMemTableSize (64 MiB). Tests may override via
// SetFlushThresholdForTesting to force rotations without allocating 64 MB.
func (e *Engine) flushThreshold() uint64 {
	e.flushCfgMu.Lock()
	defer e.flushCfgMu.Unlock()
	if e.flushThresholdOverride > 0 {
		return e.flushThresholdOverride
	}
	return memtable.MaxMemTableSize
}

// SetFlushThresholdForTesting overrides the rotation threshold and returns a
// restore function. A value <= 0 restores production behavior.
func (e *Engine) SetFlushThresholdForTesting(n uint64) func() {
	if e == nil {
		return func() {}
	}
	e.flushCfgMu.Lock()
	prev := e.flushThresholdOverride
	e.flushThresholdOverride = n
	e.flushCfgMu.Unlock()
	return func() {
		e.flushCfgMu.Lock()
		e.flushThresholdOverride = prev
		e.flushCfgMu.Unlock()
	}
}

// SetTableWriterFactoryForTesting injects a TableWriter constructor for
// deterministic SSTable creation failures. Nil restores the default.
func (e *Engine) SetTableWriterFactoryForTesting(fn func(path string, opts sstable.TableWriterOptions) (*sstable.TableWriter, error)) func() {
	if e == nil {
		return func() {}
	}
	e.flushCfgMu.Lock()
	prev := e.tableWriterFactory
	if fn != nil {
		e.tableWriterFactory = fn
	} else {
		e.tableWriterFactory = func(path string, opts sstable.TableWriterOptions) (*sstable.TableWriter, error) {
			return sstable.NewTableWriter(path, opts)
		}
	}
	e.flushCfgMu.Unlock()
	return func() {
		e.flushCfgMu.Lock()
		e.tableWriterFactory = prev
		e.flushCfgMu.Unlock()
	}
}

// SetVersionApplyForTesting injects the VersionSet publication function.
// Nil restores the default (vset.LogAndApply).
func (e *Engine) SetVersionApplyForTesting(fn func(edit *version.VersionEdit) error) func() {
	if e == nil {
		return func() {}
	}
	e.flushCfgMu.Lock()
	prev := e.versionApply
	e.versionApply = fn
	e.flushCfgMu.Unlock()
	return func() {
		e.flushCfgMu.Lock()
		e.versionApply = prev
		e.flushCfgMu.Unlock()
	}
}

// SetFlushPauseHookForTesting installs a hook called synchronously by the
// background worker after capturing an immutable MemTable and before any
// TableWriter I/O, without holding the Engine write mutex. Tests use it to
// deterministically block flush I/O and prove writes continue. Nil disables.
func (e *Engine) SetFlushPauseHookForTesting(fn func(imm *memtable.SkipList)) func() {
	if e == nil {
		return func() {}
	}
	e.flushCfgMu.Lock()
	prev := e.flushPauseHook
	e.flushPauseHook = fn
	e.flushCfgMu.Unlock()
	return func() {
		e.flushCfgMu.Lock()
		e.flushPauseHook = prev
		e.flushCfgMu.Unlock()
	}
}

// FlushError returns the last background flush error, or nil if all flushed
// generations succeeded. Failures retain the immutable MemTable; they never
// discard in-memory state.
func (e *Engine) FlushError() error {
	if e == nil {
		return nil
	}
	e.flushErrMu.Lock()
	defer e.flushErrMu.Unlock()
	return e.flushErr
}

// FlushCount returns the number of successfully retired immutable generations.
func (e *Engine) FlushCount() uint64 {
	if e == nil {
		return 0
	}
	return e.flushCount.Load()
}

// FlushQueueLen returns the number of immutable MemTables awaiting or
// undergoing flush.
func (e *Engine) FlushQueueLen() int {
	if e == nil {
		return 0
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.immMems)
}

func (e *Engine) recordFlushError(err error) {
	if err == nil {
		return
	}
	e.flushErrMu.Lock()
	e.flushErr = err
	e.flushErrMu.Unlock()
}

func (e *Engine) signalFlush() {
	if e == nil {
		return
	}
	e.mu.RLock()
	ch := e.flushCh
	closed := e.closed.Load()
	st := e.state
	e.mu.RUnlock()
	if ch == nil || closed || st == engineStateClosed {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// maybeRotateLocked freezes the active MemTable into the immutable queue when
// its size reaches the threshold. Caller must hold e.mu.Lock(). It performs
// no I/O; the background worker flushes asynchronously. Returns true if a
// rotation occurred.
func (e *Engine) maybeRotateLocked() bool {
	if e.activeMem == nil || e.activeMem.Len() == 0 {
		return false
	}
	threshold := e.flushThreshold()
	if e.activeMem.ByteSize() < threshold {
		return false
	}
	e.activeMem.Freeze()
	e.immMems = append(e.immMems, e.activeMem)
	e.activeMem = memtable.NewSkipList()
	return true
}

// takeOldestImm peeks at the oldest immutable generation without removing it.
// Removal happens only after successful VersionSet publication.
func (e *Engine) takeOldestImm() *memtable.SkipList {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if len(e.immMems) == 0 {
		return nil
	}
	return e.immMems[0]
}

// removeFlushedImm removes the specific flushed pointer from the queue (by
// identity, normally at index 0) and refreshes backpressure accounting.
func (e *Engine) removeFlushedImm(imm *memtable.SkipList) {
	if imm == nil {
		return
	}
	e.mu.Lock()
	idx := -1
	for i, m := range e.immMems {
		if m == imm {
			idx = i
			break
		}
	}
	if idx >= 0 {
		e.immMems = append(e.immMems[:idx], e.immMems[idx+1:]...)
	}
	total := uint64(0)
	if e.activeMem != nil {
		total = e.activeMem.ByteSize()
	}
	for _, m := range e.immMems {
		total += m.ByteSize()
	}
	bp := e.backpressure
	e.mu.Unlock()
	if bp != nil {
		bp.RecordUsage(total)
	}
}

func (e *Engine) startFlushWorkerLocked() {
	if e.flushStarted || e.flushCh != nil || e.stopCh != nil {
		return
	}
	e.flushCh = make(chan struct{}, 1)
	e.stopCh = make(chan struct{})
	e.flushWG.Add(1)
	e.flushStarted = true
	go e.flushLoop()
}

// flushLoop is the single background worker. It waits for coalesced flush
// signals, captures immutable generations by pointer, and flushes oldest-first
// without holding the Engine write mutex during any disk I/O or manifest work.
// On stop it finishes the in-flight flushOne (if any) and exits, leaving any
// remaining queue readable and WAL-durable.
func (e *Engine) flushLoop() {
	defer e.flushWG.Done()
	for {
		select {
		case <-e.stopCh:
			return
		case <-e.flushCh:
		}
		for {
			select {
			case <-e.stopCh:
				return
			default:
			}
			imm := e.takeOldestImm()
			if imm == nil {
				break
			}
			if err := e.flushOne(imm); err != nil {
				e.recordFlushError(err)
				// Retain imm for reads and future retry; break to avoid a
				// tight failure loop. The next rotation signal retries oldest-first.
				break
			}
			e.removeFlushedImm(imm)
			e.flushCount.Add(1)
		}
	}
}

// flushOne persists a captured immutable MemTable to L0 and publishes it.
// It holds no Engine write mutex during I/O; ownership of imm is by pointer.
func (e *Engine) flushOne(imm *memtable.SkipList) error {
	if imm == nil {
		return nil
	}
	e.flushCfgMu.Lock()
	pauseHook := e.flushPauseHook
	writerFactory := e.tableWriterFactory
	applyFn := e.versionApply
	e.flushCfgMu.Unlock()

	if pauseHook != nil {
		pauseHook(imm)
	}

	// Snapshot paths and VersionSet without holding locks across I/O.
	e.mu.RLock()
	dbPath := e.dbPath
	vset := e.vset
	st := e.state
	e.mu.RUnlock()
	// During CLOSING the synchronous shutdown drain owns this flush; only a
	// fully CLOSED engine refuses work (no worker or drain can be running
	// then, so this is purely defensive).
	if st == engineStateClosed {
		return fmt.Errorf("%w: engine closed during flush", os.ErrClosed)
	}
	if dbPath == "" {
		return fmt.Errorf("%w: engine dbPath cannot be empty", os.ErrInvalid)
	}
	if vset == nil {
		return fmt.Errorf("%w: version set is not configured", os.ErrInvalid)
	}
	if imm.Len() == 0 {
		return nil
	}

	if writerFactory == nil {
		writerFactory = func(path string, opts sstable.TableWriterOptions) (*sstable.TableWriter, error) {
			return sstable.NewTableWriter(path, opts)
		}
	}

	fileNum := e.AllocateFileNum()
	if fileNum == 0 {
		// First allocation on a non-recovered engine yields the 0 sentinel;
		// advance once so FileNum is valid and cache-safe.
		fileNum = e.AllocateFileNum()
		if fileNum == 0 {
			return fmt.Errorf("%w: file number allocation returned sentinel 0", os.ErrInvalid)
		}
	}
	path := version.TablePath(dbPath, fileNum)
	w, err := writerFactory(path, sstable.DefaultTableWriterOptions())
	if err != nil {
		return err
	}
	// Stream in canonical InternalKey order, preserving original sequence
	// numbers and tombstones. A flush is persistence, not a new mutation.
	var maxSeq binary.SeqNum
	wrote := 0
	it := imm.NewIterator()
	if it == nil {
		_ = w.Close()
		return fmt.Errorf("%w: failed to iterate immutable memtable", os.ErrInvalid)
	}
	flushFailed := false
	var flushErr error
	for it.Next() {
		ik := it.Key()
		val := it.Value()
		if ik.SeqNum > maxSeq {
			maxSeq = ik.SeqNum
		}
		if err := w.Add(ik, val); err != nil {
			flushFailed = true
			flushErr = err
			break
		}
		wrote++
	}
	it.Close()
	if flushFailed {
		_ = w.Close()
		return flushErr
	}
	if wrote == 0 {
		_ = w.Close()
		return nil
	}
	sstMeta, err := w.Finish()
	if err != nil {
		_ = w.Close()
		return err
	}
	fm := version.NewFileMetadataFromSSTable(fileNum, sstMeta)
	edit := version.NewVersionEdit()
	if err := edit.AddFile(0, fm); err != nil {
		return err
	}
	if fileNum == ^uint64(0) {
		return fmt.Errorf("%w: file number overflow", os.ErrInvalid)
	}
	edit.SetNextFileNum(fileNum + 1)
	edit.SetLastSeqNum(maxSeq)

	if applyFn == nil {
		applyFn = vset.LogAndApply
	}
	if err := applyFn(edit); err != nil {
		// SSTable is finalized but invisible (orphan). Leave it on disk;
		// recovery bumps nextFileNum past it and manifest replay ignores it.
		// The immutable MemTable is retained by the caller.
		return err
	}
	return nil
}

// ensureManifestWriter opens or creates the active MANIFEST writer and points
// the VersionSet at it. Fresh databases get MANIFEST-000001 + CURRENT.
func (e *Engine) ensureManifestWriter(dbPath string) error {
	vs := e.VersionSet()
	if vs == nil {
		return fmt.Errorf("%w: version set is not configured", os.ErrInvalid)
	}
	if mw := vs.ManifestWriter(); mw != nil && !mw.IsClosed() && !mw.IsPoisoned() {
		return nil
	}
	manifestNum, err := version.ReadCurrentManifest(dbPath)
	if err != nil {
		if !stdErrors.Is(err, errors.ErrCurrentNotFound) && !stdErrors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("engine: failed to read CURRENT: %w", err)
		}
		// Fresh database: create MANIFEST-000001 and point CURRENT at it.
		manifestNum = 1
		mp := version.ManifestPath(dbPath, manifestNum)
		mw, err := version.CreateManifestWriter(mp)
		if err != nil {
			return fmt.Errorf("engine: failed to create manifest: %w", err)
		}
		if err := version.SetCurrentManifest(dbPath, manifestNum); err != nil {
			_ = mw.Close()
			return fmt.Errorf("engine: failed to set CURRENT: %w", err)
		}
		vs.SetManifestWriter(mw)
		e.mu.Lock()
		e.manifest = mw
		e.mu.Unlock()
		return nil
	}
	mp := version.ManifestPath(dbPath, manifestNum)
	mw, err := version.OpenManifestWriter(mp)
	if err != nil {
		return fmt.Errorf("engine: failed to open manifest: %w", err)
	}
	vs.SetManifestWriter(mw)
	e.mu.Lock()
	e.manifest = mw
	e.mu.Unlock()
	return nil
}

// drainForShutdown performs the M04 final persistence pass: rotate a
// non-empty active MemTable into the immutable queue, then flush every queued
// generation oldest-first through flushOne. It runs synchronously in the Close
// goroutine after the background worker has exited, so no generation is
// flushed twice. The first flush error stops the drain (remaining generations
// stay queued and WAL-durable for future recovery); the caller aggregates it.
func (e *Engine) drainForShutdown() error {
	if e == nil {
		return nil
	}
	// Final rotation under the mutex, no I/O: reuse the M02 mechanism.
	e.mu.Lock()
	if e.activeMem != nil && e.activeMem.Len() > 0 {
		e.activeMem.Freeze()
		e.immMems = append(e.immMems, e.activeMem)
		e.activeMem = memtable.NewSkipList()
	}
	e.mu.Unlock()

	var firstErr error
	for {
		imm := e.takeOldestImm()
		if imm == nil {
			break
		}
		if imm.Len() == 0 {
			e.removeFlushedImm(imm)
			continue
		}
		if err := e.flushOne(imm); err != nil {
			e.recordFlushError(err)
			firstErr = err
			break
		}
		e.removeFlushedImm(imm)
		e.flushCount.Add(1)
	}
	return firstErr
}

// WaitForFlushQueueEmptyForTesting polls until the immutable queue drains or
// timeout elapses. Returns true if drained.
func (e *Engine) WaitForFlushQueueEmptyForTesting(timeoutMs int) bool {
	if e == nil {
		return true
	}
	deadline := timeoutMs
	for deadline > 0 {
		if e.FlushQueueLen() == 0 {
			return true
		}
		time.Sleep(5 * time.Millisecond)
		deadline -= 5
	}
	return e.FlushQueueLen() == 0
}
