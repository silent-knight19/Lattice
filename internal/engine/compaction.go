package engine

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/silent-knight19/lattice/internal/cache"
	"github.com/silent-knight19/lattice/internal/compaction"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
	"github.com/silent-knight19/lattice/internal/version"
)

// Background leveled compaction.
//
// internal/compaction already provides the full leveled machinery: a scoring
// CompactionPolicy, a deterministic Planner (including the L0 transitive overlap
// closure), an immutable CompactionPlan, a k-way merging iterator with
// per-user-key deduplication, tombstone-safety evaluation, and crash-safe SSTable
// output generation. None of it was reachable from the Engine, so L0 was
// append-only: every flush added an L0 file, nothing ever moved data to L1..LN,
// nothing was ever reclaimed, and write pacing eventually rejected every writer.
//
// This file adds the missing coordinator: a single background worker that pins
// the current Version, asks the Planner for work, executes it, and commits the
// result through VersionSet.LogAndApply.
//
// Safety invariants:
//
//  1. Version Pinning: the source Version is pinned for the entire duration of a
//     compaction. Obsolete-file reclamation is driven by Version refcounts
//     (Version.finalize -> VersionSet.collectObsoleteFilesLocked), so a pinned
//     Version guarantees no input file is unlinked while it is being read.
//  2. Staleness Rejection: the plan is re-validated against the pinned Version
//     immediately before the merge. A concurrent flush that publishes a new
//     Version does not invalidate the plan (the pinned snapshot is immutable),
//     but any inconsistency is treated as retryable rather than committed.
//  3. Single Writer: exactly one compaction runs at a time, so Level ordering and
//     file-number allocation are serialized without extra locking.
//  4. Atomic Publication: output SSTables are durable on disk before
//     LogAndApply, and become visible to readers only when the VersionEdit is
//     committed. A crash before commit leaves unreferenced files that the
//     existing orphan cleaner reclaims.
//  5. Fail-Closed Output: compaction output validation is never skipped, so a
//     truncated or corrupt output table can never be published.
//  6. No L0 Progress Requirement: if no level scores >= 1.0 the worker idles.
//     It never compacts speculatively.

const (
	// DefaultCompactionCheckInterval is how often the compaction worker re-evaluates
	// the Version when it has not been signalled by a flush.
	DefaultCompactionCheckInterval = 2 * time.Second

	// DefaultCompactionMaxInputFiles bounds the total number of input SSTables a
	// single compaction may open at once. It caps file-descriptor and memory use
	// on a pathological Version and matches compaction.MaxMergingIterators as the
	// merge fan-in ceiling.
	DefaultCompactionMaxInputFiles = compaction.MaxMergingIterators
)

// compactionWorker owns the background leveled compaction loop.
type compactionWorker struct {
	eng *Engine

	planner *compaction.Planner
	policy  compaction.CompactionPolicy

	triggerCh chan struct{}
	stopCh    chan struct{}
	doneCh    chan struct{}

	started atomic.Bool
	stopped atomic.Bool

	mu        sync.Mutex
	lastErr   error
	runCount  uint64
	failCount uint64
	// lastObs is the L0..LN file-count snapshot from the most recent successful
	// compaction, for diagnostics and tests.
	lastObs string
}

func newCompactionWorker(eng *Engine, policy compaction.CompactionPolicy) (*compactionWorker, error) {
	if eng == nil {
		return nil, errors.ErrNilReceiver
	}
	planner, err := compaction.NewPlanner(policy)
	if err != nil {
		return nil, err
	}
	return &compactionWorker{
		eng:       eng,
		planner:   planner,
		policy:    policy,
		triggerCh: make(chan struct{}, 1),
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}, nil
}

// SignalCompaction asks the worker to re-evaluate the Version. It never blocks:
// the channel has depth 1 and a pending signal already guarantees a re-check.
func (e *Engine) SignalCompaction() {
	if e == nil {
		return
	}
	e.mu.RLock()
	w := e.compactor
	e.mu.RUnlock()
	if w == nil {
		return
	}
	w.signal()
}

func (w *compactionWorker) signal() {
	if w == nil || w.stopped.Load() {
		return
	}
	select {
	case w.triggerCh <- struct{}{}:
	default:
		// A re-check is already pending.
	}
}

func (w *compactionWorker) start() {
	if w == nil || !w.started.CompareAndSwap(false, true) {
		return
	}
	go w.run()
}

func (w *compactionWorker) run() {
	defer close(w.doneCh)

	ticker := time.NewTicker(DefaultCompactionCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.stopCh:
			return
		case <-w.triggerCh:
		case <-ticker.C:
		}
		w.compactAvailable()
	}
}

// stop halts the worker and waits for the in-flight compaction to finish or for
// the drain timeout to elapse. Safe to call repeatedly and on a nil worker.
func (w *compactionWorker) stop(timeout time.Duration) {
	if w == nil || !w.started.Load() {
		return
	}
	if w.stopped.CompareAndSwap(false, true) {
		close(w.stopCh)
	}
	if timeout <= 0 {
		timeout = DefaultShutdownTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-w.doneCh:
	case <-timer.C:
		// The worker is stuck in I/O. Returning without waiting is safe: the
		// Engine is closing and no further Versions will be published, so an
		// abandoned compaction can never commit.
	}
}

// compactAvailable drains compaction work, bounded by maxRuns so a burst of
// pressure cannot monopolize the worker.
func (w *compactionWorker) compactAvailable() {
	const maxRuns = 16
	for i := 0; i < maxRuns; i++ {
		select {
		case <-w.stopCh:
			return
		default:
		}
		did, err := w.compactOnce()
		if err != nil {
			w.recordFailure(err)
			// A failure is not retried in a tight loop: the next tick or flush
			// signal will try again. Retrying immediately would spin on a
			// persistent condition such as a corrupt input table.
			return
		}
		if !did {
			return
		}
	}
}

func (w *compactionWorker) recordFailure(err error) {
	w.mu.Lock()
	w.failCount++
	w.lastErr = err
	w.mu.Unlock()
}

// compactOnce executes at most one compaction. It reports whether work was done.
func (w *compactionWorker) compactOnce() (bool, error) {
	e := w.eng

	vset := e.VersionSet()
	if vset == nil || !vset.HasCurrent() {
		return false, nil
	}
	dbPath := e.DBPath()
	if dbPath == "" {
		return false, nil
	}

	// Pin the Version for the whole compaction. Current() hands back a +1
	// reference that must be released exactly once.
	ver := vset.Current()
	if ver == nil {
		return false, nil
	}
	defer ver.Unref()

	// Do not compact while a recovery is publishing a new Version.
	e.mu.RLock()
	recovering := e.state == engineStateRecovering
	closing := e.state == engineStateClosing || e.state == engineStateClosed
	e.mu.RUnlock()
	if recovering || closing || e.closed.Load() {
		return false, nil
	}

	plan, err := w.planner.PickCompaction(ver)
	if err != nil {
		return false, fmt.Errorf("engine: compaction planning failed: %w", err)
	}
	if plan == nil {
		return false, nil
	}

	if err := plan.ValidateAgainstVersion(ver); err != nil {
		// The plan is inconsistent with the pinned snapshot. Treat as retryable
		// rather than committing anything.
		return false, fmt.Errorf("engine: compaction plan rejected: %w", err)
	}

	inputs := plan.AllInputFiles()
	if len(inputs) == 0 {
		return false, fmt.Errorf("engine: compaction plan has no input files: %w", errors.ErrInvalidCompactionPlan)
	}
	if len(inputs) > DefaultCompactionMaxInputFiles {
		return false, fmt.Errorf("engine: compaction input fan-in %d exceeds ceiling %d: %w",
			len(inputs), DefaultCompactionMaxInputFiles, errors.ErrInvalidCompactionPlan)
	}

	e.mu.RLock()
	bc := e.blockCache
	e.mu.RUnlock()

	// Open every input table and take an iterator. All readers are closed on every
	// exit path.
	openFiles := func(fileNum uint64) (*sstable.TableReader, error) {
		return openCompactionTable(dbPath, fileNum, bc)
	}

	iters := make([]compaction.Iterator, 0, len(inputs))
	readers := make([]*sstable.TableReader, 0, len(inputs))
	closeAll := func() {
		for _, it := range iters {
			if c, ok := it.(interface{ Close() error }); ok {
				_ = c.Close()
			}
		}
		for _, r := range readers {
			_ = r.Close()
		}
	}

	for _, meta := range inputs {
		r, err := openFiles(meta.FileNum)
		if err != nil {
			closeAll()
			return false, fmt.Errorf("engine: failed to open compaction input %d: %w", meta.FileNum, err)
		}
		readers = append(readers, r)
		it, err := r.NewIterator()
		if err != nil {
			closeAll()
			return false, fmt.Errorf("engine: failed to iterate compaction input %d: %w", meta.FileNum, err)
		}
		// Position each child before handing it to the merging iterator. The merge
		// treats a child with Valid() == false as exhausted, so an unpositioned
		// iterator would silently drop the entire file. Must be SeekToFirst, never
		// Seek, so records below a stale position are not skipped.
		if err := it.SeekToFirst(); err != nil {
			closeAll()
			return false, fmt.Errorf("engine: failed to seek compaction input %d to first record: %w", meta.FileNum, err)
		}
		iters = append(iters, it)
	}
	defer closeAll()

	// NewMergingIterator (not the raw variant) collapses each user key to its
	// newest revision. Deeper levels always carry older-or-equal sequence numbers
	// because compaction preserves sequence numbers and never invents them, so
	// keeping only the newest revision in the compaction inputs is safe.
	mergeIter := compaction.NewMergingIterator(iters)

	// Tombstone oracle pinned to the same Version. WithTransientTableOpener makes
	// it open and immediately close its own probe readers, so it cannot leak
	// descriptors.
	oracle, err := compaction.NewCompactor(ver, compaction.WithTransientTableOpener(openFiles))
	if err != nil {
		return false, fmt.Errorf("engine: failed to construct tombstone oracle: %w", err)
	}
	defer func() { _ = oracle.Close() }()

	// File numbers come from the Engine allocator so they stay globally unique and
	// monotonic with flush allocations.
	var maxAllocated uint64
	alloc := func() (uint64, error) {
		n := e.AllocateFileNum()
		if n > maxAllocated {
			maxAllocated = n
		}
		return n, nil
	}

	outCfg := compaction.DefaultCompactionOutputConfig()
	outCfg.DbDir = dbPath
	outCfg.TargetLevel = plan.TargetLevel()
	// Validation is mandatory: never publish unverified compaction output.
	outCfg.SkipValidation = false
	outCfg.CloseIterator = false

	output, err := compaction.BuildCompactionOutput(mergeIter, oracle, alloc, outCfg)
	if err != nil {
		return false, fmt.Errorf("engine: compaction output generation failed: %w", err)
	}

	// Build the VersionEdit: delete every input at the level it actually occupies,
	// then add every output at the target level. Deleting a target file while
	// claiming it sits at the source level is rejected by VersionSet as
	// "not found at level N for deletion", so the two sets must be kept distinct.
	// LastSeqNum is deliberately untouched: compaction preserves existing sequence
	// numbers and must never move the durability watermark.
	edit := version.NewVersionEdit()
	for _, meta := range plan.SourceFiles() {
		if err := edit.DeleteFile(uint32(plan.SourceLevel()), meta.FileNum); err != nil {
			cleanupCompactionOutputs(output, dbPath)
			return false, fmt.Errorf("engine: failed to stage source deletion for file %d: %w", meta.FileNum, err)
		}
	}
	for _, meta := range plan.TargetFiles() {
		if err := edit.DeleteFile(uint32(plan.TargetLevel()), meta.FileNum); err != nil {
			cleanupCompactionOutputs(output, dbPath)
			return false, fmt.Errorf("engine: failed to stage target deletion for file %d: %w", meta.FileNum, err)
		}
	}
	for _, out := range output.FileMetadatas() {
		if err := edit.AddFile(uint32(plan.TargetLevel()), out); err != nil {
			cleanupCompactionOutputs(output, dbPath)
			return false, fmt.Errorf("engine: failed to stage compaction output file %d: %w", out.FileNum, err)
		}
	}
	if maxAllocated != 0 {
		// The flush worker allocates file numbers concurrently and publishes its own
		// watermark. Publishing only this run's maximum could regress the manifest
		// watermark below a value the flusher already committed, which LogAndApply
		// rejects as corrupted. publishNextFileNum keeps it strictly monotonic.
		edit.SetNextFileNum(e.publishNextFileNum(maxAllocated + 1))
	}

	// Re-check lifecycle immediately before publishing. Close may have begun while
	// the merge and SSTable writes were in flight; committing at that point would
	// race the shutdown drain and could leave an output file referenced by no
	// Version. Abort and unlink instead.
	e.mu.RLock()
	aborting := e.state == engineStateClosing || e.state == engineStateClosed || e.closed.Load()
	e.mu.RUnlock()
	if aborting {
		cleanupCompactionOutputs(output, dbPath)
		return false, nil
	}

	if err := vset.LogAndApply(edit); err != nil {
		cleanupCompactionOutputs(output, dbPath)
		return false, fmt.Errorf("engine: failed to commit compaction edit: %w", err)
	}

	// The commit may have dropped the VersionSet's reference on the superseded
	// Version. If no reader still pins it, Version.finalize reclaims the now
	// obsolete input files automatically, gated on refcounts. Explicitly request
	// a pass as well so files whose last reference disappeared earlier are
	// reclaimed too.
	if _, err := vset.CleanObsoleteFiles(); err != nil {
		w.mu.Lock()
		w.lastErr = err
		w.mu.Unlock()
	}

	obs := fmt.Sprintf("L%d->L%d src=%d tgt=%d out=%d bytes=%d",
		plan.SourceLevel(), plan.TargetLevel(),
		len(plan.SourceFiles()), len(plan.TargetFiles()),
		output.Stats.OutputFilesCount, output.Stats.TotalOutputBytes)

	w.mu.Lock()
	w.runCount++
	w.lastObs = obs
	w.mu.Unlock()

	return true, nil
}

// cleanupCompactionOutputs unlinks output files that were never published because
// the compaction failed after they were finalized.
func cleanupCompactionOutputs(output *compaction.CompactionOutput, dbPath string) {
	if output == nil || dbPath == "" {
		return
	}
	for _, f := range output.Files {
		if f.Path == "" {
			continue
		}
		_ = os.Remove(f.Path)
	}
}

// openCompactionTable opens an SSTable by file number for compaction reading,
// sharing the Engine's block cache and pinning the FileNum so cached blocks are
// attributed to the correct file.
func openCompactionTable(dbPath string, fileNum uint64, bc *cache.ShardedCache) (*sstable.TableReader, error) {
	var cacheIf sstable.BlockCache
	if bc != nil {
		cacheIf = bc
	}
	path := version.TablePath(dbPath, fileNum)
	return sstable.NewTableReaderWithOptions(path, sstable.TableReaderOptions{
		FileNum:    fileNum,
		BlockCache: cacheIf,
	})
}

// CompactionStats is a point-in-time snapshot of background compaction activity.
type CompactionStats struct {
	Enabled  bool
	Running  bool
	Runs     uint64
	Failures uint64
	LastErr  error
	LastDesc string
}

// CompactionStats returns a snapshot of the background compaction worker.
func (e *Engine) CompactionStats() CompactionStats {
	if e == nil {
		return CompactionStats{}
	}
	e.mu.RLock()
	w := e.compactor
	enabled := e.compactionEnabled
	e.mu.RUnlock()

	st := CompactionStats{Enabled: enabled}
	if w == nil {
		return st
	}
	w.mu.Lock()
	st.Runs = w.runCount
	st.Failures = w.failCount
	st.LastErr = w.lastErr
	st.LastDesc = w.lastObs
	w.mu.Unlock()
	st.Running = w.started.Load() && !w.stopped.Load()
	return st
}

// RunCompactionOnce executes a single compaction synchronously and reports
// whether work was performed. It exists for deterministic tests and for an
// operator-triggered manual compaction; the background worker uses the same path.
func (e *Engine) RunCompactionOnce() (bool, error) {
	if e == nil {
		return false, errors.ErrNilReceiver
	}
	e.mu.RLock()
	w := e.compactor
	e.mu.RUnlock()
	if w == nil {
		return false, fmt.Errorf("%w: compaction is not enabled", errors.ErrCompactionDisabled)
	}
	did, err := w.compactOnce()
	if err != nil {
		w.recordFailure(err)
		return false, err
	}
	return did, nil
}
