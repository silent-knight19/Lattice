package raft

import (
	"fmt"

	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/transport"
)

// Propose accepts a client/application proposal on the local Raft leader and
// durably appends it to the leader's own Raft log (P15-S03-M01).
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
//     in-memory state. Success is reported only after durable persistence.
//     On append failure an error is returned and no success is claimed;
//     Storage guarantees all-or-nothing (no phantom in-memory entry).
//   - Scope boundary: success means "durably appended locally", NOT committed,
//     applied, or replicated. No commitIndex is advanced (none exists yet),
//     no state machine is touched, and no AppendEntries replication is
//     triggered; those belong to P15-S03-M02/M03 and Phase 16.
//
// Concurrency & linearization:
//   - Concurrent Propose calls are serialized by proposeMu so each accepted
//     proposal receives a distinct contiguous index with no duplicates or
//     gaps. Storage.Append independently re-validates contiguity under its
//     own lock as defense in depth.
//   - Node.mu is held only as a brief RLock for the (role, term, lastIndex)
//     snapshot; it is never held across the slow Storage.Append filesystem
//     I/O, and never across network I/O (none occurs here).
//   - Lock order is proposeMu -> Node.mu(R) -> Storage.mu. No other path
//     acquires proposeMu, so no inversion with stepdown/Close (which take
//     Node.mu without proposeMu) is possible.
//   - The linearization point is the snapshot under proposeMu + Node.mu RLock
//     where (role == Leader, term == T, lastIndex == L) is observed; the
//     proposal is then durably appended as (index L+1, term T). If leadership
//     is lost after that point, the entry legitimately remains in the log
//     with its creation term; conflict handling belongs to P15-S03-M02.
//     A proposal that observes a non-leader role is rejected before any I/O.
func (n *Node) Propose(data []byte) (LogEntry, error) {
	if n.closed.Load() {
		return LogEntry{}, errors.ErrRaftStateClosed
	}

	n.proposeMu.Lock()
	defer n.proposeMu.Unlock()

	if n.closed.Load() {
		return LogEntry{}, errors.ErrRaftStateClosed
	}

	// Single atomic snapshot of leadership, term, and log tail under one
	// RLock so a concurrent stepdown cannot interleave a role change between
	// the role check and the term read (which would otherwise let a follower
	// mint an entry in a term it never led).
	n.mu.RLock()
	isLeader := (n.role == RoleLeader)
	role := n.role
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

	// Durable append WITHOUT holding Node.mu (slow filesystem I/O).
	// Storage.Append validates (term, contiguity, size, type), performs
	// write + fdatasync, and only then mutates in-memory state.
	if err := n.storage.Append(entry); err != nil {
		return LogEntry{}, err
	}

	return entry.Clone(), nil
}
