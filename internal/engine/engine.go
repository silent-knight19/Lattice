package engine

import (
	"context"
	stdErrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
	"github.com/silent-knight19/lattice/internal/version"
	"github.com/silent-knight19/lattice/internal/wal"
)

// Engine coordinates the active MemTable, immutable flush candidates,
// VersionSet snapshot management, and backpressure gating (SEC-003).
type Engine struct {
	mu           sync.RWMutex
	dbPath       string
	activeMem    *memtable.SkipList
	immMems      []*memtable.SkipList
	backpressure *BackpressureController
	nextSeqNum   atomic.Uint64
	closed       atomic.Bool
	vset         *version.VersionSet
}

// EngineOptions specifies configuration parameters for initializing an Engine instance.
type EngineOptions struct {
	DBPath       string
	Backpressure BackpressureConfig
	VersionSet   *version.VersionSet
}

// NewEngine constructs an Engine instance backed by the given backpressure configuration.
func NewEngine(cfg BackpressureConfig) *Engine {
	return NewEngineWithOptions(EngineOptions{Backpressure: cfg})
}

// NewEngineWithOptions constructs an Engine instance configured with EngineOptions.
func NewEngineWithOptions(opts EngineOptions) *Engine {
	bc := NewBackpressureController(opts.Backpressure)
	vset := opts.VersionSet
	if vset == nil {
		vset = version.NewVersionSet()
	}
	cleanDBPath := ""
	if opts.DBPath != "" {
		cleanDBPath = filepath.Clean(opts.DBPath)
	}
	return &Engine{
		dbPath:       cleanDBPath,
		activeMem:    memtable.NewSkipList(),
		backpressure: bc,
		vset:         vset,
	}
}

// SetDBPath sets the root database directory path for the Engine.
func (e *Engine) SetDBPath(path string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if path == "" {
		e.dbPath = ""
		return
	}
	e.dbPath = filepath.Clean(path)
}

// DBPath returns the cleaned root database directory path configured on the Engine.
func (e *Engine) DBPath() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.dbPath
}

// ActiveMemTable returns the currently active in-memory MemTable.
func (e *Engine) ActiveMemTable() *memtable.SkipList {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.activeMem
}

// ImmMemTables returns a defensive copy of the immutable MemTables slice.
func (e *Engine) ImmMemTables() []*memtable.SkipList {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if len(e.immMems) == 0 {
		return nil
	}
	cp := make([]*memtable.SkipList, len(e.immMems))
	copy(cp, e.immMems)
	return cp
}

// VersionSet returns the Engine's VersionSet coordinator.
func (e *Engine) VersionSet() *version.VersionSet {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.vset
}

// NextSeqNum returns the current sequence number watermark.
func (e *Engine) NextSeqNum() uint64 {
	return e.nextSeqNum.Load()
}

// Backpressure returns the engine's BackpressureController.
func (e *Engine) Backpressure() *BackpressureController {
	if e == nil {
		return nil
	}
	return e.backpressure
}

// Put writes a key-value pair under backpressure control (SEC-003).
// If total engine memory approaches or exceeds configured thresholds,
// Put throttles or rejects the write fail-closed.
func (e *Engine) Put(ctx context.Context, key, val []byte) error {
	if e == nil {
		return errors.ErrNilReceiver
	}
	if e.closed.Load() {
		return errors.ErrWriterClosed
	}

	if err := binary.ValidateKey(key); err != nil {
		return err
	}
	if err := binary.ValidateValue(val); err != nil {
		return err
	}

	// Approximate wire allocation: node structure + key + pointers + value
	estBytes := memtable.NodeStructSize + uint64(len(key)) + 8*8 + uint64(len(val))

	if err := e.backpressure.Acquire(ctx, estBytes); err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed.Load() {
		e.backpressure.Release(estBytes)
		return errors.ErrWriterClosed
	}

	seq := binary.SeqNum(e.nextSeqNum.Add(1))
	ik, err := binary.NewInternalKey(key, seq, binary.OpTypePut)
	if err != nil {
		e.backpressure.Release(estBytes)
		return err
	}

	insertErr := e.activeMem.Insert(ik, val)
	if insertErr != nil {
		if stdErrors.Is(insertErr, errors.ErrMemTableFull) {
			// MemTable reached maximum capacity: freeze and rotate
			e.activeMem.Freeze()
			e.immMems = append(e.immMems, e.activeMem)
			e.activeMem = memtable.NewSkipList()

			// Retry insert in fresh active MemTable
			insertErr = e.activeMem.Insert(ik, val)
		}
	}

	if insertErr != nil {
		e.backpressure.Release(estBytes)
		return insertErr
	}

	// Synchronize backpressure controller with exact active heap usage
	totalBytes := e.activeMem.ByteSize()
	for _, imm := range e.immMems {
		totalBytes += imm.ByteSize()
	}
	e.backpressure.RecordUsage(totalBytes)

	return nil
}

// Delete appends a tombstone deletion marker under backpressure control.
func (e *Engine) Delete(ctx context.Context, key []byte) error {
	if e == nil {
		return errors.ErrNilReceiver
	}
	if e.closed.Load() {
		return errors.ErrWriterClosed
	}

	if err := binary.ValidateKey(key); err != nil {
		return err
	}

	estBytes := memtable.NodeStructSize + uint64(len(key)) + 8*8
	if err := e.backpressure.Acquire(ctx, estBytes); err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed.Load() {
		e.backpressure.Release(estBytes)
		return errors.ErrWriterClosed
	}

	seq := binary.SeqNum(e.nextSeqNum.Add(1))
	ik, err := binary.NewInternalKey(key, seq, binary.OpTypeDelete)
	if err != nil {
		e.backpressure.Release(estBytes)
		return err
	}

	insertErr := e.activeMem.Insert(ik, nil)
	if insertErr != nil {
		if stdErrors.Is(insertErr, errors.ErrMemTableFull) {
			e.activeMem.Freeze()
			e.immMems = append(e.immMems, e.activeMem)
			e.activeMem = memtable.NewSkipList()
			insertErr = e.activeMem.Insert(ik, nil)
		}
	}

	if insertErr != nil {
		e.backpressure.Release(estBytes)
		return insertErr
	}

	totalBytes := e.activeMem.ByteSize()
	for _, imm := range e.immMems {
		totalBytes += imm.ByteSize()
	}
	e.backpressure.RecordUsage(totalBytes)

	return nil
}

// Get retrieves the newest value associated with key across active and immutable MemTables.
func (e *Engine) Get(key []byte) ([]byte, error) {
	if e == nil {
		return nil, errors.ErrNilReceiver
	}
	if err := binary.ValidateKey(key); err != nil {
		return nil, err
	}

	e.mu.RLock()
	defer e.mu.RUnlock()

	// 1. Search active MemTable
	val, err := e.activeMem.SearchConcurrent(key)
	if err == nil {
		return val, nil
	}

	// 2. Search immutable MemTables in reverse chronological order
	for i := len(e.immMems) - 1; i >= 0; i-- {
		val, err := e.immMems[i].SearchConcurrent(key)
		if err == nil {
			return val, nil
		}
	}

	return nil, errors.ErrKeyNotFound
}

// Close gracefully flushes state, freezes active tables, and closes the backpressure controller.
func (e *Engine) Close() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed.Swap(true) {
		return nil
	}

	e.activeMem.Freeze()
	e.backpressure.Close()
	return nil
}

// RecoverWAL executes startup crash recovery by:
//  1. Discovering the active MANIFEST via boot discovery (P07-S01-M01).
//  2. Sequentially replaying the MANIFEST to reconstruct the durable Version and obtain
//     the durable sequence watermark (P07-S01-M02).
//  3. Scanning and physically validating all WAL segments under <dbPath>/wal/ (wal.RecoverWAL).
//  4. Filtering out WAL records with SeqNum <= manifest checkpoint.
//  5. Replaying uncommitted records (SeqNum > checkpoint) into a fresh, private MemTable.
//  6. Atomically installing the recovered MemTable and advancing the sequence watermark upon success.
//
// Invariants & Operational Semantics:
//   - P07-S02-M01-INV-01: WAL segments processed in ascending numeric order.
//   - P07-S02-M01-INV-02: Every WAL record physically validated before filtering.
//   - P07-S02-M01-INV-03 & INV-04: Only records > checkpoint are logically applied; durable records skipped.
//   - P07-S02-M01-INV-05: Global sequence monotonicity enforced across all segments.
//   - P07-S02-M01-INV-06 & INV-07: Latest torn-tail truncated; historical corruption fails closed.
//   - P07-S02-M01-INV-08: Recovered MemTable contains exact logical state of uncommitted records.
//   - P07-S02-M01-INV-09 & INV-10: Replay failure never publishes partial state; zero disk mutation.
//   - P07-S02-M01-INV-11: Streaming replay with bounded memory.
//   - P07-S02-M01-INV-12: Recovered sequence state never moves backwards.
//   - P07-S02-M01-INV-13: Descriptors and resources safely released on all paths.
//   - P07-S02-M01-INV-14: Filtering based strictly on logical SeqNum, never timestamps or file names.
func (e *Engine) RecoverWAL() error {
	if e == nil {
		return errors.ErrNilReceiver
	}
	if e.closed.Load() {
		return errors.ErrWriterClosed
	}

	dbPath := e.DBPath()
	if dbPath == "" {
		return fmt.Errorf("%w: engine dbPath cannot be empty", os.ErrInvalid)
	}

	var (
		checkpoint binary.SeqNum
		replayRes  *version.ReplayResult
	)

	// Step 1: Discover and replay active MANIFEST to establish the durable checkpoint
	disc, err := version.DiscoverActiveManifest(dbPath)
	if err != nil {
		if stdErrors.Is(err, errors.ErrCurrentNotFound) || stdErrors.Is(err, errors.ErrManifestNotFound) || stdErrors.Is(err, os.ErrNotExist) {
			// Clean fresh database environment: no MANIFEST or CURRENT exists yet.
			// Durable sequence checkpoint defaults to 0.
			checkpoint = 0
		} else {
			// Any directory corruption or ambiguous state fails closed immediately
			return fmt.Errorf("engine: failed to discover active manifest: %w", err)
		}
	} else {
		defer func() { _ = disc.Close() }()

		res, replayErr := version.ReplayManifest(disc)
		if replayErr != nil {
			return fmt.Errorf("engine: failed to replay manifest: %w", replayErr)
		}
		replayRes = res
		checkpoint = res.LastSeqNum
	}

	// Step 2: Recover uncommitted WAL records newer than checkpoint
	if err := e.recoverWALInternal(dbPath, checkpoint, replayRes); err != nil {
		return err
	}

	// Step 3: Purge unreferenced crash-window temporary files left by interrupted writes/flushes
	if err := e.CleanOrphanedFiles(); err != nil {
		return fmt.Errorf("engine: failed to clean orphaned temporary files: %w", err)
	}

	return nil
}

// RecoverWALFromCheckpoint executes WAL recovery with an explicitly supplied sequence watermark.
// Records with SeqNum <= checkpoint are filtered out; records with SeqNum > checkpoint are applied.
func (e *Engine) RecoverWALFromCheckpoint(checkpoint binary.SeqNum) error {
	if e == nil {
		return errors.ErrNilReceiver
	}
	if e.closed.Load() {
		return errors.ErrWriterClosed
	}

	dbPath := e.DBPath()
	if dbPath == "" {
		return fmt.Errorf("%w: engine dbPath cannot be empty", os.ErrInvalid)
	}

	if err := e.recoverWALInternal(dbPath, checkpoint, nil); err != nil {
		return err
	}
	return e.CleanOrphanedFiles()
}

// RecoverWALWithManifestResult executes WAL recovery composing directly with a pre-computed
// *version.ReplayResult from P07-S01-M02 without re-reading the MANIFEST from disk (Section 5).
func (e *Engine) RecoverWALWithManifestResult(res *version.ReplayResult) error {
	if e == nil {
		return errors.ErrNilReceiver
	}
	if e.closed.Load() {
		return errors.ErrWriterClosed
	}
	if res == nil {
		return fmt.Errorf("%w: replay result cannot be nil", os.ErrInvalid)
	}

	dbPath := e.DBPath()
	if dbPath == "" {
		return fmt.Errorf("%w: engine dbPath cannot be empty", os.ErrInvalid)
	}

	if err := e.recoverWALInternal(dbPath, res.LastSeqNum, res); err != nil {
		return err
	}
	return e.CleanOrphanedFiles()
}

func (e *Engine) recoverWALInternal(dbPath string, checkpoint binary.SeqNum, replayRes *version.ReplayResult) error {
	recoveryMem := memtable.NewSkipList()
	var recoveryImm []*memtable.SkipList

	var (
		inBatch     bool
		batchBuffer []wal.Record
	)

	// Set up the ReplaySink adapter
	sink := wal.ReplayFunc(func(rec wal.Record) error {
		switch rec.Type {
		case wal.RecordTypeBatchStart:
			inBatch = true
			batchBuffer = nil
			return nil

		case wal.RecordTypeBatchCommit:
			if !inBatch {
				return fmt.Errorf("wal: BATCH_COMMIT without preceding BATCH_START at seq %s", rec.SeqNum)
			}
			inBatch = false
			for _, bRec := range batchBuffer {
				if bRec.SeqNum > checkpoint {
					if err := insertRecordToMemTable(&recoveryMem, &recoveryImm, bRec); err != nil {
						return err
					}
				}
			}
			batchBuffer = nil
			return nil

		case wal.RecordTypePut, wal.RecordTypeDelete:
			if inBatch {
				batchBuffer = append(batchBuffer, rec)
				return nil
			}
			// Standalone PUT or DELETE
			if rec.SeqNum > checkpoint {
				return insertRecordToMemTable(&recoveryMem, &recoveryImm, rec)
			}
			// SeqNum <= checkpoint: already durable in SSTable/MANIFEST. Intentionally skipped!
			return nil

		default:
			return &errors.InvalidRecordTypeError{Type: byte(rec.Type)}
		}
	})

	// Invoke existing WAL recovery coordinator
	report, err := wal.RecoverWAL(dbPath, sink)
	if err != nil {
		// Clean up uninstalled version reference on failure if replayRes was provided
		if replayRes != nil && replayRes.Version != nil {
			replayRes.Version.Unref()
		}
		return fmt.Errorf("engine: wal recovery failed: %w", err)
	}

	// Discard any incomplete batch left uncommitted before EOF
	_ = inBatch
	batchBuffer = nil

	// State publication atomicity: publish under e.mu.Lock()
	e.mu.Lock()
	defer e.mu.Unlock()

	// 1. Install active and immutable MemTables
	e.activeMem = recoveryMem
	if len(recoveryImm) > 0 {
		e.immMems = append(e.immMems, recoveryImm...)
	}

	// 2. Monotonic sequence counter advancement: max(current, checkpoint, wal.LastSeqNum)
	highestSeq := uint64(checkpoint)
	if uint64(report.LastSeqNum) > highestSeq {
		highestSeq = uint64(report.LastSeqNum)
	}
	curSeq := e.nextSeqNum.Load()
	if highestSeq > curSeq {
		e.nextSeqNum.Store(highestSeq)
	}

	// 3. Install reconstructed Version into VersionSet (if supplied and not yet installed)
	if replayRes != nil && replayRes.Version != nil {
		if e.vset != nil && e.vset.Current() == nil {
			_ = e.vset.AppendVersion(replayRes.Version)
		} else {
			replayRes.Version.Unref()
		}
	}

	// 4. Synchronize backpressure accounting with recovered memory
	if e.backpressure != nil {
		totalBytes := e.activeMem.ByteSize()
		for _, imm := range e.immMems {
			totalBytes += imm.ByteSize()
		}
		e.backpressure.RecordUsage(totalBytes)
	}

	return nil
}

func insertRecordToMemTable(activeMem **memtable.SkipList, immMems *[]*memtable.SkipList, rec wal.Record) error {
	var op binary.OpType
	switch rec.Type {
	case wal.RecordTypePut:
		op = binary.OpTypePut
	case wal.RecordTypeDelete:
		op = binary.OpTypeDelete
	default:
		return &errors.InvalidRecordTypeError{Type: byte(rec.Type)}
	}

	ik, err := binary.NewInternalKey(rec.Key, rec.SeqNum, op)
	if err != nil {
		return err
	}

	var val []byte
	if op == binary.OpTypePut {
		val = rec.Value
	}

	insertErr := (*activeMem).Insert(ik, val)
	if insertErr != nil {
		if stdErrors.Is(insertErr, errors.ErrMemTableFull) {
			(*activeMem).Freeze()
			*immMems = append(*immMems, *activeMem)
			*activeMem = memtable.NewSkipList()
			insertErr = (*activeMem).Insert(ik, val)
		}
	}

	return insertErr
}
