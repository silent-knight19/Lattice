package raft

import (
	"context"
	"fmt"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/transport"
)

const (
	// DefaultApplyBatchSize defines the maximum number of committed entries
	// fetched and applied in a single batch iteration (P16-S01-M01).
	DefaultApplyBatchSize = 64

	// DefaultApplyTimeout defines the per-entry context execution deadline.
	DefaultApplyTimeout = 5 * time.Second
)

// StateMachine defines the storage engine contract required by the Raft apply loop.
// It is satisfied by *engine.Engine and test mocks.
type StateMachine interface {
	Put(ctx context.Context, key, val []byte) error
	Delete(ctx context.Context, key []byte) error
}

// StartApplyLoop initializes and starts the authoritative state machine apply loop.
// Idempotent if already running. Returns ErrServerAlreadyStarted if already started,
// or ErrRaftStateClosed if Node is closed.
func (n *Node) StartApplyLoop(sm StateMachine, batchSize int) error {
	if n == nil {
		return errors.ErrNilReceiver
	}
	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}
	if sm == nil {
		return errors.ErrNilReceiver
	}

	n.applyLifecycleMu.Lock()
	defer n.applyLifecycleMu.Unlock()

	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}
	if n.applyRunning.Load() {
		return errors.ErrRaftApplyLoopAlreadyStarted
	}

	if batchSize <= 0 {
		batchSize = DefaultApplyBatchSize
	}

	n.stateMachine = sm
	n.applyBatchSize = batchSize
	n.applyNotifyCh = make(chan struct{}, 1)
	n.applyStopCh = make(chan struct{})
	n.applyCtx, n.applyCancel = context.WithCancel(context.Background())
	n.applyRunning.Store(true)

	n.applyWg.Add(1)
	go n.applyLoop()

	return nil
}

// stopApplyLoop halts the apply loop and synchronously waits for termination.
// Safe for repeated and concurrent invocation.
func (n *Node) stopApplyLoop() {
	if n == nil {
		return
	}

	n.applyLifecycleMu.Lock()
	if !n.applyRunning.CompareAndSwap(true, false) {
		n.applyLifecycleMu.Unlock()
		return
	}

	if n.applyCancel != nil {
		n.applyCancel()
	}
	close(n.applyStopCh)
	n.applyLifecycleMu.Unlock()

	n.applyWg.Wait()
}

// LastApplied returns the highest Raft log index applied to the state machine.
// Guaranteed to be <= CommitIndex() at all times.
func (n *Node) LastApplied() LogIndex {
	if n == nil {
		return 0
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.lastApplied
}

// ApplyError returns the terminal state machine apply error that halted the loop, if any.
func (n *Node) ApplyError() error {
	if n == nil {
		return errors.ErrNilReceiver
	}
	n.applyErrMu.RLock()
	defer n.applyErrMu.RUnlock()
	return n.applyErr
}

// setApplyError records a terminal state machine apply error.
func (n *Node) setApplyError(err error) {
	if err == nil {
		return
	}
	n.applyErrMu.Lock()
	if n.applyErr == nil {
		n.applyErr = err
	}
	n.applyErrMu.Unlock()
}

// signalApplyLocked notifies the apply loop that commitIndex has advanced.
// Caller must hold n.mu (or have verified that commitIndex advanced under n.mu).
func (n *Node) signalApplyLocked() {
	if !n.applyRunning.Load() || n.applyNotifyCh == nil {
		return
	}
	select {
	case n.applyNotifyCh <- struct{}{}:
	default:
	}
}

// applyLoop is the single authoritative state-machine application goroutine per Node.
func (n *Node) applyLoop() {
	defer n.applyWg.Done()

	// Initial drain on startup in case commitIndex is already ahead of lastApplied
	n.drainCommittedEntries()

	for {
		if n.ApplyError() != nil {
			return
		}
		select {
		case <-n.applyStopCh:
			return
		case <-n.applyNotifyCh:
			if n.ApplyError() != nil {
				return
			}
			n.drainCommittedEntries()
		}
	}
}

// drainCommittedEntries consumes eligible committed entries sequentially and applies them to the state machine.
func (n *Node) drainCommittedEntries() {
	for {
		if n.closed.Load() || n.ApplyError() != nil {
			return
		}
		select {
		case <-n.applyStopCh:
			return
		default:
		}

		// Snapshot commitIndex and lastApplied under brief RLock
		n.mu.RLock()
		commitIdx := n.commitIndex
		appliedIdx := n.lastApplied
		n.mu.RUnlock()

		if appliedIdx >= commitIdx {
			return // caught up
		}

		// Calculate batch range [from, to)
		from := appliedIdx + 1
		to := commitIdx + 1
		batchLimit := LogIndex(n.applyBatchSize)
		if to-from > batchLimit {
			to = from + batchLimit
		}

		// Fetch entries from persistent storage
		entries, err := n.storage.Entries(from, to)
		if err != nil {
			n.setApplyError(fmt.Errorf("raft: failed to fetch entries [%d, %d) for apply: %w", from, to, err))
			return
		}
		if len(entries) == 0 {
			n.setApplyError(fmt.Errorf("%w: storage returned 0 entries for committed range [%d, %d)",
				errors.ErrRaftLogIndexOutOfBounds, from, to))
			return
		}

		// Apply entries strictly in log order
		for _, entry := range entries {
			if n.closed.Load() {
				return
			}
			select {
			case <-n.applyStopCh:
				return
			default:
			}

			// Invariant: strict contiguous Raft index ordering
			if entry.Index != appliedIdx+1 {
				n.setApplyError(fmt.Errorf("%w: entry index %d does not match expected contiguous apply index %d",
					errors.ErrRaftLogIndexGap, entry.Index, appliedIdx+1))
				return
			}

			if err := n.applySingleEntry(entry); err != nil {
				n.setApplyError(err)
				return // HALT immediately! lastApplied is NOT advanced
			}

			// Advance lastApplied ONLY after successful application!
			n.mu.Lock()
			n.lastApplied = entry.Index
			appliedIdx = entry.Index
			n.mu.Unlock()
		}
	}
}

// applySingleEntry validates and executes a single committed entry against the state machine.
func (n *Node) applySingleEntry(entry LogEntry) error {
	if n.stateMachine == nil {
		return errors.ErrNilReceiver
	}

	switch entry.Type {
	case transport.PeerEntryNoop:
		// Consensus election no-op: advances Raft log alignment without state machine mutation.
		return nil

	case transport.PeerEntryConfiguration:
		// Cluster membership change: reserved for dynamic reconfiguration; no state machine mutation.
		return nil

	case transport.PeerEntryNormal:
		// Application state machine command
		cmd, err := DecodeCommand(entry.Data)
		if err != nil {
			return fmt.Errorf("%w: malformed command at index %d: %v", errors.ErrRaftCorruptedState, entry.Index, err)
		}

		parentCtx := n.applyCtx
		if parentCtx == nil {
			parentCtx = context.Background()
		}
		ctx, cancel := context.WithTimeout(parentCtx, DefaultApplyTimeout)
		defer cancel()

		switch cmd.Op {
		case binary.OpTypePut:
			if err := n.stateMachine.Put(ctx, cmd.Key, cmd.Value); err != nil {
				return fmt.Errorf("%w: state machine Put failed at index %d: %v", errors.ErrRaftApplyFailed, entry.Index, err)
			}
			return nil

		case binary.OpTypeDelete:
			if err := n.stateMachine.Delete(ctx, cmd.Key); err != nil {
				return fmt.Errorf("%w: state machine Delete failed at index %d: %v", errors.ErrRaftApplyFailed, entry.Index, err)
			}
			return nil

		default:
			return fmt.Errorf("%w: invalid command opcode 0x%02x at index %d",
				errors.ErrInvalidOpType, byte(cmd.Op), entry.Index)
		}

	default:
		return fmt.Errorf("%w: unrecognized entry type 0x%02x at index %d",
			errors.ErrRaftInvalidEntryType, byte(entry.Type), entry.Index)
	}
}
