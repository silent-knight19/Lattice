package engine

import (
	"context"
	stdErrors "errors"
	"sync"
	"sync/atomic"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
)

// Engine coordinates the active MemTable, immutable flush candidates, and backpressure gating (SEC-003).
type Engine struct {
	mu           sync.RWMutex
	activeMem    *memtable.SkipList
	immMems      []*memtable.SkipList
	backpressure *BackpressureController
	nextSeqNum   atomic.Uint64
	closed       atomic.Bool
}

// NewEngine constructs an Engine instance backed by the given backpressure configuration.
func NewEngine(cfg BackpressureConfig) *Engine {
	bc := NewBackpressureController(cfg)
	return &Engine{
		activeMem:    memtable.NewSkipList(),
		backpressure: bc,
	}
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
