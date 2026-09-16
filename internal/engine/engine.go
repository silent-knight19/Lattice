package engine

import (
	"bytes"
	"context"
	stdErrors "errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/cache"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
	"github.com/silent-knight19/lattice/internal/sstable"
	"github.com/silent-knight19/lattice/internal/version"
	"github.com/silent-knight19/lattice/internal/wal"
)

// walWriter is the minimal WAL durability interface required by Engine for
// P10-S01-M01. It is satisfied by *wal.RotatingWriter and *wal.WALWriter.
// Tests may inject failing implementations to verify failure propagation.
type walWriter interface {
	AppendSync(rec wal.Record) error
	Close() error
}

type engineState uint32

const (
	engineStateNotRecovering engineState = iota
	engineStateRecovering
	engineStateRecovered
	engineStateClosed
)

const (
	// MaxRecoveryBatchRecords defines the maximum count of records permitted within a single WAL batch during recovery (10,000 records).
	MaxRecoveryBatchRecords = 10000

	// MaxRecoveryBatchBytes defines the maximum cumulative memory budget permitted for a single WAL batch during recovery (64 MiB).
	MaxRecoveryBatchBytes uint64 = 64 * 1024 * 1024
)

var (
	recoveryBatchMaxRecords = MaxRecoveryBatchRecords
	recoveryBatchMaxBytes   = MaxRecoveryBatchBytes
	recoveryBatchLimitsMu   sync.Mutex
)

// SetRecoveryBatchLimitsForTesting configures custom recovery batch limits for testing and returns a restore function.
func SetRecoveryBatchLimitsForTesting(maxRecords int, maxBytes uint64) func() {
	recoveryBatchLimitsMu.Lock()
	prevRecords := recoveryBatchMaxRecords
	prevBytes := recoveryBatchMaxBytes
	recoveryBatchMaxRecords = maxRecords
	recoveryBatchMaxBytes = maxBytes
	recoveryBatchLimitsMu.Unlock()
	return func() {
		recoveryBatchLimitsMu.Lock()
		recoveryBatchMaxRecords = prevRecords
		recoveryBatchMaxBytes = prevBytes
		recoveryBatchLimitsMu.Unlock()
	}
}

var (
	recoveryPrePublishHookMu sync.Mutex
	recoveryPrePublishHook   func(*Engine)
)

// SetRecoveryPrePublishHookForTesting registers a testing hook called right before recovery acquires mu.Lock() to publish.
func SetRecoveryPrePublishHookForTesting(hook func(*Engine)) func() {
	recoveryPrePublishHookMu.Lock()
	recoveryPrePublishHook = hook
	recoveryPrePublishHookMu.Unlock()
	return func() {
		recoveryPrePublishHookMu.Lock()
		recoveryPrePublishHook = nil
		recoveryPrePublishHookMu.Unlock()
	}
}

// Engine coordinates the active MemTable, immutable flush candidates,
// VersionSet snapshot management, and backpressure gating (SEC-003).
//
// P10-S01-M01 ownership:
//   - Owns directly: dbPath, activeMem, immMems slice management, nextSeqNum /
//     nextFileNum watermarks, lifecycle state, backpressure accounting.
//   - Coordinates (does not duplicate): WAL durability, VersionSet current
//     Version, transient TableReaders, and the single shared ShardedCache.
//   - Engine never maintains []SSTable, map[key]value, a second WAL/cache/
//     VersionSet, nor does it perform raw file I/O, manifest writes, or
//     compaction output construction.
type Engine struct {
	mu                sync.RWMutex
	dbPath            string
	activeMem         *memtable.SkipList
	immMems           []*memtable.SkipList
	backpressure      *BackpressureController
	nextSeqNum        atomic.Uint64
	nextFileNum       atomic.Uint64
	closed            atomic.Bool
	state             engineState
	vset              *version.VersionSet
	lastCleanerReport CleanOrphanReport
	wal               walWriter
	blockCache        *cache.ShardedCache
	manifest          *version.ManifestWriter

	// P10-S01-M02 background flusher state. The worker is started by Open
	// for DB-backed engines; memory-only engines have nil channels and no
	// goroutine. flushCh is a coalescing signal (cap 1); stopCh is closed
	// once by Close.
	flushCh      chan struct{}
	stopCh       chan struct{}
	flushWG      sync.WaitGroup
	flushStarted bool

	// flushCfgMu guards test-injectable flusher configuration. It is a leaf
	// lock never held across e.mu or any disk I/O.
	flushCfgMu             sync.Mutex
	flushThresholdOverride uint64
	tableWriterFactory     func(path string, opts sstable.TableWriterOptions) (*sstable.TableWriter, error)
	versionApply           func(edit *version.VersionEdit) error
	flushPauseHook         func(imm *memtable.SkipList)

	flushErrMu sync.Mutex
	flushErr   error
	flushCount atomic.Uint64
}

// EngineOptions specifies configuration parameters for initializing an Engine instance.
type EngineOptions struct {
	DBPath       string
	Backpressure BackpressureConfig
	VersionSet   *version.VersionSet
	// WAL is the optional durability writer. When nil, Put/Delete operate on
	// memory only (preserving pre-M01 in-memory behavior for unit tests).
	// Open() initializes a RotatingWriter when WAL is nil and DBPath is set.
	WAL walWriter
	// BlockCache is the single shared Phase 09 ShardedCache passed to transient
	// TableReaders. When nil, persistent reads go directly to disk.
	BlockCache *cache.ShardedCache
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
		wal:          opts.WAL,
		blockCache:   opts.BlockCache,
	}
}

// WAL returns the Engine's configured WAL writer, or nil if operating in
// memory-only mode (no durability).
func (e *Engine) WAL() walWriter {
	if e == nil {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.wal
}

// SetWALForTesting installs a WAL writer for testing and returns a restore
// function. It is used to inject failing writers for failure-propagation tests.
func (e *Engine) SetWALForTesting(w walWriter) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.wal = w
}

// BlockCache returns the Engine's shared block cache, or nil if caching is disabled.
func (e *Engine) BlockCache() *cache.ShardedCache {
	if e == nil {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.blockCache
}

// SetBlockCache installs the shared block cache passed to transient TableReaders.
func (e *Engine) SetBlockCache(c *cache.ShardedCache) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.blockCache = c
}

// Open initializes database directories, recovers durable state using existing
// recovery machinery, and opens the WAL for subsequent mutations.
//
// Steps (in order to preserve quiescence):
//  1. Validate DBPath and create the database directory (0700).
//  2. Point the VersionSet at DBPath for physical SSTable validation.
//  3. If not yet recovered, run RecoverWAL() (manifest replay + WAL replay).
//  4. Create a default shared block cache if none is configured.
//  5. Open a RotatingWriter if none is configured.
//
// Open is idempotent: repeated calls on a recovered Engine with an open WAL
// return nil. Initialization failures leave already-created resources owned by
// the Engine for Close() to release; no partially usable state is presented
// beyond the returned error.
func (e *Engine) Open() error {
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
	if err := os.MkdirAll(dbPath, 0700); err != nil {
		return fmt.Errorf("engine: failed to create database directory: %w", err)
	}
	if vs := e.VersionSet(); vs != nil {
		vs.SetDBPath(dbPath)
	}

	e.mu.RLock()
	st := e.state
	hasWAL := e.wal != nil
	hasCache := e.blockCache != nil
	e.mu.RUnlock()
	if st == engineStateClosed || e.closed.Load() {
		return errors.ErrWriterClosed
	}
	if st == engineStateRecovering {
		return errors.ErrRecoveryInProgress
	}
	if st != engineStateRecovered {
		if err := e.RecoverWAL(); err != nil {
			// RecoverWAL on a clean empty directory succeeds; any other error
			// (including AlreadyComplete from a concurrent Open) is propagated
			// unless the Engine is now recovered.
			e.mu.RLock()
			cur := e.state
			e.mu.RUnlock()
			if cur != engineStateRecovered {
				return err
			}
		}
	}
	if !hasCache {
		c, err := cache.NewShardedCache(1024)
		if err != nil {
			return err
		}
		e.mu.Lock()
		if e.blockCache == nil {
			e.blockCache = c
		}
		e.mu.Unlock()
	}
	if !hasWAL {
		rw, err := wal.OpenRotatingWriter(dbPath, wal.Options{})
		if err != nil {
			return fmt.Errorf("engine: failed to open WAL: %w", err)
		}
		e.mu.Lock()
		if e.wal == nil {
			e.wal = rw
		} else {
			_ = rw.Close()
		}
		e.mu.Unlock()
	}
	// P10-S01-M02: durable VersionSet publication requires an active MANIFEST
	// writer. Set it up after recovery so flushes can LogAndApply.
	if err := e.ensureManifestWriter(dbPath); err != nil {
		return err
	}
	// Start the single background flusher and drain any immutable generations
	// left by recovery.
	e.mu.Lock()
	e.startFlushWorkerLocked()
	needSignal := len(e.immMems) > 0
	e.mu.Unlock()
	if needSignal {
		e.signalFlush()
	}
	return nil
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

// NextFileNum returns the current next file number watermark (P07-SEC-006).
func (e *Engine) NextFileNum() uint64 {
	return e.nextFileNum.Load()
}

// AllocateFileNum atomically allocates and returns a new unique file number (P07-SEC-006).
// Allocations start from the recovered nextFileNum watermark and advance monotonically.
func (e *Engine) AllocateFileNum() uint64 {
	return e.nextFileNum.Add(1) - 1
}

// LastCleanerReport returns the diagnostic report from the most recent orphan cleanup pass (P07-SEC-005).
func (e *Engine) LastCleanerReport() CleanOrphanReport {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastCleanerReport
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
//
// Durability (P10-S01-M01): when a WAL is configured, Put allocates exactly
// one sequence number, appends one WAL PUT record and synchronizes it
// (AppendSync) before mutating the MemTable. Success is reported only after
// both the WAL barrier and the MemTable insertion succeed. WAL append/sync
// failures return the underlying error without mutating memory; MemTable
// failures after a durable WAL append return the insert error (the record
// remains recoverable via WAL replay).
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
	rotated := false
	defer func() {
		e.mu.Unlock()
		if rotated {
			e.signalFlush()
		}
	}()

	if e.closed.Load() || e.state == engineStateClosed {
		e.backpressure.Release(estBytes)
		return errors.ErrWriterClosed
	}
	if e.state == engineStateRecovering {
		e.backpressure.Release(estBytes)
		return errors.ErrRecoveryInProgress
	}

	seq := binary.SeqNum(e.nextSeqNum.Add(1))
	if e.wal != nil {
		rec := wal.Record{
			Type:      wal.RecordTypePut,
			SeqNum:    seq,
			Timestamp: uint64(time.Now().UnixNano()),
			Key:       key,
			Value:     val,
		}
		if err := e.wal.AppendSync(rec); err != nil {
			e.backpressure.Release(estBytes)
			return err
		}
	}
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
			rotated = true

			// Retry insert in fresh active MemTable
			insertErr = e.activeMem.Insert(ik, val)
		}
	}

	if insertErr != nil {
		e.backpressure.Release(estBytes)
		return insertErr
	}

	// P10-S01-M02: proactive rotation when the successful insert filled the
	// active table to the threshold. No I/O under the mutex; the background
	// worker flushes asynchronously.
	if e.maybeRotateLocked() {
		rotated = true
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
//
// Tombstone semantics (P10-S01-M01): Delete always creates a tombstone with a
// fresh sequence number, even for nonexistent keys, because an older SSTable
// value may exist beneath memory. The tombstone shadows older revisions until
// compaction provably drops it. Durability mirrors Put: WAL AppendSync first,
// then MemTable tombstone; failures propagate without reporting success.
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
	rotated := false
	defer func() {
		e.mu.Unlock()
		if rotated {
			e.signalFlush()
		}
	}()

	if e.closed.Load() || e.state == engineStateClosed {
		e.backpressure.Release(estBytes)
		return errors.ErrWriterClosed
	}
	if e.state == engineStateRecovering {
		e.backpressure.Release(estBytes)
		return errors.ErrRecoveryInProgress
	}

	seq := binary.SeqNum(e.nextSeqNum.Add(1))
	if e.wal != nil {
		rec := wal.Record{
			Type:      wal.RecordTypeDelete,
			SeqNum:    seq,
			Timestamp: uint64(time.Now().UnixNano()),
			Key:       key,
		}
		if err := e.wal.AppendSync(rec); err != nil {
			e.backpressure.Release(estBytes)
			return err
		}
	}
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
			rotated = true
			insertErr = e.activeMem.Insert(ik, nil)
		}
	}

	if insertErr != nil {
		e.backpressure.Release(estBytes)
		return insertErr
	}

	if e.maybeRotateLocked() {
		rotated = true
	}

	totalBytes := e.activeMem.ByteSize()
	for _, imm := range e.immMems {
		totalBytes += imm.ByteSize()
	}
	e.backpressure.RecordUsage(totalBytes)

	return nil
}

// Get retrieves the newest value associated with key across memory and disk.
//
// Precedence (newest wins, tombstone shadows):
//  1. Active MemTable (newest layer).
//  2. Immutable MemTables, newest frozen first.
//  3. Persistent Version: L0 newest FileNum first (overlapping), then L1..L6
//     in order (each non-overlapping, at most one file per level via key-range).
//
// A tombstone at any layer stops the search and returns ErrKeyNotFound without
// consulting older layers. Storage failures (missing file, checksum, I/O,
// corruption, closed reader) are returned as errors and never mapped to
// not-found. Returned values are defensive copies.
func (e *Engine) Get(key []byte) ([]byte, error) {
	if e == nil {
		return nil, errors.ErrNilReceiver
	}
	if err := binary.ValidateKey(key); err != nil {
		return nil, err
	}

	e.mu.RLock()
	if e.state == engineStateRecovering {
		e.mu.RUnlock()
		return nil, errors.ErrRecoveryInProgress
	}
	active := e.activeMem
	imm := make([]*memtable.SkipList, len(e.immMems))
	copy(imm, e.immMems)
	vset := e.vset
	dbPath := e.dbPath
	bc := e.blockCache
	e.mu.RUnlock()

	// 1. Active MemTable (tombstone-aware).
	if active != nil {
		val, put, tomb := lookupMemLayer(active, key)
		if put {
			return val, nil
		}
		if tomb {
			return nil, errors.ErrKeyNotFound
		}
	}
	// 2. Immutable MemTables, newest first.
	for i := len(imm) - 1; i >= 0; i-- {
		if imm[i] == nil {
			continue
		}
		val, put, tomb := lookupMemLayer(imm[i], key)
		if put {
			return val, nil
		}
		if tomb {
			return nil, errors.ErrKeyNotFound
		}
	}

	// 3. Persistent Version via transient TableReaders + shared block cache.
	if vset == nil || dbPath == "" || !vset.HasCurrent() {
		return nil, errors.ErrKeyNotFound
	}
	ver := vset.Current()
	if ver == nil {
		return nil, errors.ErrKeyNotFound
	}
	defer ver.Unref()
	return e.getFromVersion(key, ver, dbPath, bc)
}

// lookupMemLayer inspects a single SkipList for key, distinguishing PUT,
// tombstone, and absence. It uses Iterator.Seek so the newest revision's
// OpType decides: PUT returns (value, true, false), DELETE returns
// (nil, false, true), absence returns (nil, false, false).
func lookupMemLayer(sl *memtable.SkipList, key []byte) (val []byte, put bool, tomb bool) {
	if sl == nil {
		return nil, false, false
	}
	it := sl.NewIterator()
	if it == nil {
		return nil, false, false
	}
	defer it.Close()
	if err := it.Seek(key); err != nil {
		return nil, false, false
	}
	if !it.Valid() {
		return nil, false, false
	}
	ik := it.Key()
	if !bytes.Equal(ik.UserKey, key) {
		return nil, false, false
	}
	if ik.OpType == binary.OpTypeDelete {
		return nil, false, true
	}
	return it.Value(), true, false
}

// getFromVersion searches a pinned Version newest-to-oldest. L0 files overlap
// and are searched newest FileNum first; L1.. files are non-overlapping and
// pruned by decoded user-key range. The first containing file decides: PUT
// returns its value, tombstone returns ErrKeyNotFound without consulting
// older files/levels. Range misses continue. Open/read/decode failures are
// returned as errors, never as not-found.
func (e *Engine) getFromVersion(key []byte, ver *version.Version, dbPath string, bc *cache.ShardedCache) ([]byte, error) {
	// L0: overlapping, newest first.
	l0 := ver.Files(0)
	if len(l0) > 1 {
		cp := make([]version.FileMetadata, len(l0))
		copy(cp, l0)
		// Sort FileNum descending (newest flush first; FileNums are monotonic).
		for i := 1; i < len(cp); i++ {
			for j := i; j > 0 && cp[j].FileNum > cp[j-1].FileNum; j-- {
				cp[j], cp[j-1] = cp[j-1], cp[j]
			}
		}
		l0 = cp
	}
	for _, meta := range l0 {
		found, tomb, val, err := lookupSSTableFile(dbPath, meta, key, bc)
		if err != nil {
			return nil, err
		}
		if tomb {
			return nil, errors.ErrKeyNotFound
		}
		if found {
			return val, nil
		}
	}
	// L1..L6: at most one file per level can contain the key.
	for lvl := 1; lvl < version.NumLevels; lvl++ {
		for _, meta := range ver.Files(lvl) {
			ok, err := fileRangeContains(meta, key)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			found, tomb, val, err := lookupSSTableFile(dbPath, meta, key, bc)
			if err != nil {
				return nil, err
			}
			if tomb {
				return nil, errors.ErrKeyNotFound
			}
			if found {
				return val, nil
			}
			// Range matched but key absent: non-overlapping level, no other
			// file in this level can contain it.
			break
		}
	}
	return nil, errors.ErrKeyNotFound
}

// fileRangeContains reports whether key falls within the SSTable's decoded
// [SmallestUserKey, LargestUserKey] range. Decode failures are storage errors.
func fileRangeContains(meta version.FileMetadata, key []byte) (bool, error) {
	smallIK, err := binary.DecodeInternalKey(meta.SmallestKey)
	if err != nil {
		return false, err
	}
	largeIK, err := binary.DecodeInternalKey(meta.LargestKey)
	if err != nil {
		return false, err
	}
	if bytes.Compare(key, smallIK.UserKey) < 0 {
		return false, nil
	}
	if bytes.Compare(key, largeIK.UserKey) > 0 {
		return false, nil
	}
	return true, nil
}

// lookupSSTableFile opens a transient TableReader for meta, seeks key via a
// TableIterator (to observe OpType), and closes the reader before returning.
// It uses the Engine's shared block cache without exposing shard internals.
func lookupSSTableFile(dbPath string, meta version.FileMetadata, key []byte, bc *cache.ShardedCache) (found bool, tomb bool, val []byte, err error) {
	var cacheIf sstable.BlockCache
	if bc != nil {
		cacheIf = bc
	}
	path := version.TablePath(dbPath, meta.FileNum)
	reader, err := sstable.NewTableReaderWithOptions(path, sstable.TableReaderOptions{
		FileNum:    meta.FileNum,
		BlockCache: cacheIf,
	})
	if err != nil {
		return false, false, nil, err
	}
	defer func() { _ = reader.Close() }()
	it, err := reader.NewIterator()
	if err != nil {
		return false, false, nil, err
	}
	defer func() { _ = it.Close() }()
	if err := it.Seek(key); err != nil {
		return false, false, nil, err
	}
	if err := it.Err(); err != nil {
		return false, false, nil, err
	}
	if !it.Valid() {
		return false, false, nil, nil
	}
	ik := it.Key()
	if !bytes.Equal(ik.UserKey, key) {
		return false, false, nil, nil
	}
	if ik.OpType == binary.OpTypeDelete {
		return false, true, nil, nil
	}
	return true, false, it.Value(), nil
}

// Close freezes active tables, stops the background flusher (waiting for any
// in-flight flush to finish without starting new work), and closes WAL,
// manifest, and backpressure resources. Close is idempotent. It does not
// implement M04 flush-on-close drain, compaction coordination, or manifest
// sync beyond releasing M01/M02 resources; remaining immutable generations
// stay readable in memory and WAL-durable for future recovery.
func (e *Engine) Close() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	if e.state == engineStateClosed {
		e.mu.Unlock()
		return nil
	}
	e.closed.Store(true)
	e.state = engineStateClosed
	stopCh := e.stopCh
	e.mu.Unlock()

	// Minimal M02 interaction: signal the worker and wait for any in-flight
	// flushOne to finish so it never touches resources we are about to close.
	// The worker exits promptly without draining the whole queue (no M04 drain).
	if stopCh != nil {
		close(stopCh)
	}
	e.flushWG.Wait()

	e.mu.Lock()
	defer e.mu.Unlock()

	e.activeMem.Freeze()
	var firstErr error
	if e.wal != nil {
		if err := e.wal.Close(); err != nil {
			firstErr = err
		}
	}
	if e.manifest != nil {
		if err := e.manifest.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	e.backpressure.Close()
	return firstErr
}

func (e *Engine) beginRecovery() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed.Load() || e.state == engineStateClosed {
		return errors.ErrWriterClosed
	}
	if e.state == engineStateRecovering {
		return errors.ErrRecoveryInProgress
	}
	if e.state == engineStateRecovered {
		return errors.ErrRecoveryAlreadyComplete
	}
	// SEC-P07-01: Prohibit recovery if engine contains live mutations or uncommitted state
	if e.activeMem.Len() > 0 || len(e.immMems) > 0 || e.nextSeqNum.Load() > 0 || e.nextFileNum.Load() > 0 {
		return errors.ErrRecoveryInvalidState
	}
	if e.vset != nil && e.vset.HasCurrent() {
		return errors.ErrRecoveryInvalidState
	}

	e.state = engineStateRecovering
	return nil
}

func (e *Engine) abortRecovery() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state == engineStateRecovering {
		e.state = engineStateNotRecovering
	}
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
	if err := e.beginRecovery(); err != nil {
		return err
	}

	dbPath := e.DBPath()
	if dbPath == "" {
		e.abortRecovery()
		return fmt.Errorf("%w: engine dbPath cannot be empty", os.ErrInvalid)
	}

	var (
		checkpoint binary.SeqNum
		replayRes  *version.ReplayResult
	)

	// Step 1: Discover and replay active MANIFEST to establish the durable checkpoint
	disc, err := version.DiscoverActiveManifest(dbPath)
	if err != nil {
		if stdErrors.Is(err, errors.ErrCurrentNotFound) {
			// Clean fresh database environment: no CURRENT exists yet.
			// Durable sequence checkpoint defaults to 0.
			checkpoint = 0
		} else {
			// Any directory corruption or ambiguous state fails closed immediately
			e.abortRecovery()
			return fmt.Errorf("engine: failed to discover active manifest: %w", err)
		}
	} else {
		defer func() { _ = disc.Close() }()

		res, replayErr := version.ReplayManifest(disc)
		if replayErr != nil {
			e.abortRecovery()
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
	// Individual orphan cleanup failures (e.g. symlinks, permission errors) do not invalidate
	// successfully recovered durable state and must not cause startup denial of service (P07-SEC-005).
	cleanReport, cleanErr := e.CleanOrphanedFilesWithReport()
	e.mu.Lock()
	e.lastCleanerReport = cleanReport
	e.mu.Unlock()
	if cleanErr != nil {
		if isCriticalCleanerError(cleanErr, cleanReport) {
			return fmt.Errorf("engine: failed to clean orphaned temporary files: %w", cleanErr)
		}
	}

	return nil
}

// RecoverWALFromCheckpoint executes WAL recovery with an explicitly supplied sequence watermark.
// Records with SeqNum <= checkpoint are filtered out; records with SeqNum > checkpoint are applied.
func (e *Engine) RecoverWALFromCheckpoint(checkpoint binary.SeqNum) error {
	if e == nil {
		return errors.ErrNilReceiver
	}
	if err := e.beginRecovery(); err != nil {
		return err
	}

	dbPath := e.DBPath()
	if dbPath == "" {
		e.abortRecovery()
		return fmt.Errorf("%w: engine dbPath cannot be empty", os.ErrInvalid)
	}

	if err := e.recoverWALInternal(dbPath, checkpoint, nil); err != nil {
		return err
	}
	cleanReport, cleanErr := e.CleanOrphanedFilesWithReport()
	e.mu.Lock()
	e.lastCleanerReport = cleanReport
	e.mu.Unlock()
	if cleanErr != nil {
		if isCriticalCleanerError(cleanErr, cleanReport) {
			return fmt.Errorf("engine: failed to clean orphaned temporary files: %w", cleanErr)
		}
	}
	return nil
}

// RecoverWALWithManifestResult executes WAL recovery composing directly with a pre-computed
// *version.ReplayResult from P07-S01-M02 without re-reading the MANIFEST from disk (Section 5).
func (e *Engine) RecoverWALWithManifestResult(res *version.ReplayResult) error {
	if e == nil {
		return errors.ErrNilReceiver
	}
	if res == nil {
		return fmt.Errorf("%w: replay result cannot be nil", os.ErrInvalid)
	}
	if err := e.beginRecovery(); err != nil {
		if res.Version != nil {
			res.Version.Unref()
		}
		return err
	}

	dbPath := e.DBPath()
	if dbPath == "" {
		if res.Version != nil {
			res.Version.Unref()
		}
		e.abortRecovery()
		return fmt.Errorf("%w: engine dbPath cannot be empty", os.ErrInvalid)
	}

	if err := e.recoverWALInternal(dbPath, res.LastSeqNum, res); err != nil {
		return err
	}
	cleanReport, cleanErr := e.CleanOrphanedFilesWithReport()
	e.mu.Lock()
	e.lastCleanerReport = cleanReport
	e.mu.Unlock()
	if cleanErr != nil {
		if isCriticalCleanerError(cleanErr, cleanReport) {
			return fmt.Errorf("engine: failed to clean orphaned temporary files: %w", cleanErr)
		}
	}
	return nil
}

func (e *Engine) recoverWALInternal(dbPath string, checkpoint binary.SeqNum, replayRes *version.ReplayResult) error {
	recoveryMem := memtable.NewSkipList()
	var recoveryImm []*memtable.SkipList

	var (
		inBatch          bool
		batchBuffer      []wal.Record
		batchRecordCount int
		batchByteSize    uint64
	)

	// Set up the ReplaySink adapter
	sink := wal.ReplayFunc(func(rec wal.Record) error {
		switch rec.Type {
		case wal.RecordTypeBatchStart:
			inBatch = true
			batchBuffer = nil
			batchRecordCount = 0
			batchByteSize = 0
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
			batchRecordCount = 0
			batchByteSize = 0
			return nil

		case wal.RecordTypePut, wal.RecordTypeDelete:
			if inBatch {
				recoveryBatchLimitsMu.Lock()
				maxRecs := recoveryBatchMaxRecords
				maxBytes := recoveryBatchMaxBytes
				recoveryBatchLimitsMu.Unlock()

				// Pre-allocation checks (P07-SEC-007)
				if batchRecordCount+1 > maxRecs {
					return &errors.RecoveryBatchLimitError{
						LimitType: "records",
						Limit:     uint64(maxRecs),
						Actual:    uint64(batchRecordCount + 1),
					}
				}
				recBytes := uint64(len(rec.Key)) + uint64(len(rec.Value))
				if math.MaxUint64-batchByteSize < recBytes || batchByteSize+recBytes > maxBytes {
					return &errors.RecoveryBatchLimitError{
						LimitType: "bytes",
						Limit:     maxBytes,
						Actual:    batchByteSize + recBytes,
					}
				}
				batchBuffer = append(batchBuffer, rec)
				batchRecordCount++
				batchByteSize += recBytes
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
		e.abortRecovery()
		return fmt.Errorf("engine: wal recovery failed: %w", err)
	}

	// Discard any incomplete batch left uncommitted before EOF
	_ = inBatch
	batchBuffer = nil

	recoveryPrePublishHookMu.Lock()
	hook := recoveryPrePublishHook
	recoveryPrePublishHookMu.Unlock()
	if hook != nil {
		hook(e)
	}

	// State publication atomicity: publish under e.mu.Lock()
	e.mu.Lock()
	defer e.mu.Unlock()

	// If engine was closed while recovery was running, abort publication without mutating engine state
	if e.closed.Load() || e.state == engineStateClosed {
		if replayRes != nil && replayRes.Version != nil {
			replayRes.Version.Unref()
		}
		e.state = engineStateClosed
		return errors.ErrWriterClosed
	}

	// 1. Install active and immutable MemTables (replace, do not append to prevent accumulation)
	e.activeMem = recoveryMem
	e.immMems = recoveryImm

	// 2. Monotonic sequence counter advancement: max(current, checkpoint, wal.LastSeqNum)
	highestSeq := uint64(checkpoint)
	if uint64(report.LastSeqNum) > highestSeq {
		highestSeq = uint64(report.LastSeqNum)
	}
	curSeq := e.nextSeqNum.Load()
	if highestSeq > curSeq {
		e.nextSeqNum.Store(highestSeq)
	}

	// 3. Install reconstructed Version into VersionSet (SEC-P07-04)
	if replayRes != nil && replayRes.Version != nil {
		if e.vset != nil && !e.vset.HasCurrent() {
			if err := e.vset.AppendVersion(replayRes.Version); err != nil {
				replayRes.Version.Unref()
				e.state = engineStateNotRecovering
				return fmt.Errorf("engine: failed to install reconstructed version: %w", err)
			}
		} else {
			replayRes.Version.Unref()
		}
	}

	// 4. File number watermark initialization and advancement (P07-SEC-004, P07-SEC-006)
	// Scan dbPath for existing canonical SSTables to detect any uncommitted crash-window files
	var maxPhysicalFileNum uint64
	if entries, readErr := os.ReadDir(dbPath); readErr == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				if num, ok := version.ParseTableFilename(entry.Name()); ok {
					if num > maxPhysicalFileNum {
						maxPhysicalFileNum = num
					}
				}
			}
		}
	}

	var replayedNextFile uint64
	if replayRes != nil {
		replayedNextFile = replayRes.NextFileNum
	}
	targetFileNum := replayedNextFile
	if maxPhysicalFileNum >= targetFileNum {
		targetFileNum = maxPhysicalFileNum + 1
	}
	if targetFileNum < 1 {
		targetFileNum = 1
	}
	curFileNum := e.nextFileNum.Load()
	if targetFileNum > curFileNum {
		e.nextFileNum.Store(targetFileNum)
	}

	// 5. Synchronize backpressure accounting with recovered memory
	if e.backpressure != nil {
		totalBytes := e.activeMem.ByteSize()
		for _, imm := range e.immMems {
			totalBytes += imm.ByteSize()
		}
		e.backpressure.RecordUsage(totalBytes)
	}

	// 6. State transition to recovered
	e.state = engineStateRecovered

	return nil
}

func isCriticalCleanerError(err error, report CleanOrphanReport) bool {
	if err == nil {
		return false
	}
	// If the error originated solely from individual candidate refusal/removal failures,
	// it is a best-effort cleanup failure and not critical to engine recovery (P07-SEC-005).
	if len(report.Failures) > 0 && !report.DirectorySyncFailed {
		return false
	}
	return true
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
