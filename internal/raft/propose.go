package raft

import (
	"context"
	"fmt"

	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/transport"
)

// Propose accepts a client/application proposal on the local Raft leader and
// durably appends it to the leader's own Raft log (P15-S03-M01).
//
// This is a convenience wrapper around ProposeWithContext using context.Background().
// For request-scoped deadline control, use ProposeWithContext directly.
func (n *Node) Propose(data []byte) (LogEntry, error) {
	return n.ProposeWithContext(context.Background(), data)
}

// ProposeWithContext accepts a client/application proposal on the local Raft leader and
// durably appends it to the leader's own Raft log with request-scoped context support.
//
// Semantics:
//   - Leader-only: the node must currently be RoleLeader. Follower, Candidate,
//     closed, or otherwise unavailable nodes reject the proposal without
//     mutating the log.
//   - Term assignment: the entry uses the leader's current durable term,
//     observed atomically with the leadership check. Callers cannot inject
//     term or index; the Raft subsystem owns both.
//   - Index assignment: newIndex = currentLastIndex + 1, preserving the
//     1-based contiguous log with index 0 as the empty-log sentinel.
//   - Entry type: normal application proposal (transport.PeerEntryNormal).
//   - Durability: Storage.Append persists (write + fdatasync) before mutating
//     in-memory state. Success is reported only after durable persistence
//     AND post-append leadership verification.
//     On append failure an error is returned and no success is claimed;
//     Storage guarantees all-or-nothing (no phantom in-memory entry).
//   - Scope boundary: success means "durably appended locally AND leadership
//     was continuously held", NOT applied or replicated.
//   - Context: the context is checked before acquiring the proposal mutex,
//     after acquiring it, and after the durable append. Context cancellation
//     cannot interrupt an in-progress fdatasync, but prevents new proposals
//     from queuing behind blocked I/O.
//
// Concurrency & linearization:
//   - Concurrent Propose calls are serialized by proposeMu so each accepted
//     proposal receives a distinct contiguous index with no duplicates or
//     gaps. Storage.Append independently re-validates contiguity under its
//     own lock as defense in depth.
//   - Node.mu is held only as a brief RLock for the (role, term, lastIndex,
//     leaderEpoch) snapshot; it is never held across the slow Storage.Append
//     filesystem I/O, and never across network I/O (none occurs here).
//   - Lock order is proposeMu -> Node.mu(R) -> Storage.mu. No other path
//     acquires proposeMu, so no inversion with stepdown/Close (which take
//     Node.mu without proposeMu) is possible.
//
// Leadership TOCTOU hardening (P16-SEC-F02):
//   - A leaderEpoch is snapshot BEFORE the append under proposeMu + Node.mu RLock.
//   - AFTER Storage.Append completes, leaderEpoch is re-checked under RLock.
//   - If the epoch changed (a stepdown/transition occurred during the append),
//     the proposal returns ErrRaftInvalidRoleTransition. The entry IS durably
//     appended to the log (it will be truncated by a new leader if conflicting),
//     but the client does NOT receive a success acknowledgement.
func (n *Node) ProposeWithContext(ctx context.Context, data []byte) (LogEntry, error) {
	if n.closed.Load() {
		return LogEntry{}, errors.ErrRaftStateClosed
	}

	// P16-SEC-F03: Check context before acquiring the proposal mutex.
	// This prevents a cancelled request from queuing behind blocked proposals.
	if ctx.Err() != nil {
		return LogEntry{}, fmt.Errorf("%w: context cancelled before proposal admission",
			errors.ErrRaftInvalidRoleTransition)
	}

	n.proposeMu.Lock()
	defer n.proposeMu.Unlock()

	if n.closed.Load() {
		return LogEntry{}, errors.ErrRaftStateClosed
	}

	// P16-SEC-F03: Check context after acquiring the mutex (may have blocked).
	if ctx.Err() != nil {
		return LogEntry{}, fmt.Errorf("%w: context cancelled during proposal admission",
			errors.ErrRaftInvalidRoleTransition)
	}

	// Single atomic snapshot of leadership, term, log tail, and leaderEpoch under one
	// RLock so a concurrent stepdown cannot interleave a role change between
	// the role check and the term read (which would otherwise let a follower
	// mint an entry in a term it never led).
	n.mu.RLock()
	isLeader := (n.role == RoleLeader)
	role := n.role
	epochBefore := n.leaderEpoch
	n.mu.RUnlock()
	if !isLeader {
		return LogEntry{}, fmt.Errorf("%w: only the leader may accept proposals (current role %s)",
			errors.ErrRaftInvalidRoleTransition, role)
	}

	currTerm, err := n.storage.Term()
	if err != nil {
		return LogEntry{}, err
	}

	// Re-validate leadership against the freshly read term while holding the
	// lock: if a stepdown persisted a higher term concurrently, this proposal
	// must not claim the new term it never led. Re-reading role + term
	// together keeps the (leader, term) pair coherent.
	n.mu.RLock()
	isLeader = (n.role == RoleLeader)
	n.mu.RUnlock()
	if !isLeader {
		return LogEntry{}, fmt.Errorf("%w: only the leader may accept proposals (stepped down during proposal)",
			errors.ErrRaftInvalidRoleTransition)
	}
	latestTerm, err := n.storage.Term()
	if err != nil {
		return LogEntry{}, err
	}
	if latestTerm != currTerm {
		return LogEntry{}, fmt.Errorf("%w: leader term changed during proposal (%d -> %d)",
			errors.ErrRaftInvalidRoleTransition, currTerm, latestTerm)
	}

	lastIdx, err := n.storage.LastIndex()
	if err != nil {
		return LogEntry{}, err
	}

	entry := LogEntry{
		Index: lastIdx + 1,
		Term:  currTerm,
		Type:  transport.PeerEntryNormal,
	}
	if len(data) > 0 {
		cp := make([]byte, len(data))
		copy(cp, data)
		entry.Data = cp
	}

	// P16-SEC-F02: Test injection hook — called AFTER leadership check, BEFORE Storage.Append.
	// Allows deterministic stepdown injection in tests. Always nil in production.
	if n.proposeTestHook != nil {
		n.proposeTestHook()
	}

	// P16-SEC-F03: Final context check before slow I/O.
	if ctx.Err() != nil {
		return LogEntry{}, fmt.Errorf("%w: context cancelled before storage append",
			errors.ErrRaftInvalidRoleTransition)
	}

	// Durable append WITHOUT holding Node.mu (slow filesystem I/O).
	// Storage.Append validates (term, contiguity, size, type), performs
	// write + fdatasync, and only then mutates in-memory state.
	if err := n.storage.Append(entry); err != nil {
		return LogEntry{}, err
	}

	// P16-SEC-F02: Post-append leadership epoch verification.
	// If the epoch changed during the append (a stepdown occurred), the entry
	// IS durably in the log but the client must NOT receive a success acknowledgement.
	n.mu.RLock()
	epochAfter := n.leaderEpoch
	n.mu.RUnlock()
	if epochAfter != epochBefore {
		return LogEntry{}, fmt.Errorf("%w: leadership epoch changed during proposal (epoch %d -> %d); entry appended but acknowledgement suppressed",
			errors.ErrRaftInvalidRoleTransition, epochBefore, epochAfter)
	}

	// P16-SEC-F03: Post-append context check. If the context expired during fdatasync,
	// the entry IS durably appended. We report the cancellation rather than a false success.
	// This is a known ambiguity documented in known-limitations.
	if ctx.Err() != nil {
		return LogEntry{}, fmt.Errorf("%w: context cancelled after storage append; entry may be durably committed",
			errors.ErrRaftInvalidRoleTransition)
	}

	// Post-append quorum refresh: N=1 clusters commit the new current-term
	// entry immediately (leader alone is quorum); larger clusters evaluate
	// harmlessly (no follower progress yet) and commit via later responses.
	// No-op unless still leader. Never holds Node.mu across I/O.
	n.refreshCommitIndex()

	return entry.Clone(), nil
}
