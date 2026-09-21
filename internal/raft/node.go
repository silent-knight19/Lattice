package raft

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
)

// TransitionHook is an optional callback invoked synchronously when a role transition completes.
// Passed previous role, new role, and new term.
type TransitionHook func(from, to Role, term Term)

// Node represents the core Raft consensus state machine governing role transitions
// (Follower <-> Candidate <-> Leader) and integrating with persistent storage.
//
// Invariants enforced (P15-S01-M02):
//  1. Role state is volatile: at reboot, Node always initializes in RoleFollower.
//  2. Exactly one role is active at any time.
//  3. Monotonic term progression: term never regresses; term overflow is rejected cleanly.
//  4. Candidate self-voting: entering Candidate atomically advances term by 1 and persists
//     self-vote using the local NodeID before publishing the role change.
//  5. Transition from Candidate to Leader preserves term and self-vote.
//  6. Higher-term observation (ObserveHigherTerm) transitions the node to Follower and
//     clears the vote for the higher term.
//  7. Same-term stepdown (StepDownSameTerm) returns Candidate or Leader to Follower without
//     modifying term or vote.
//  8. If persistence fails during a transition, the transition fails closed and the node remains
//     in its prior coherent role and state.
type Node struct {
	mu             sync.RWMutex
	localID        cluster.NodeID
	storage        *Storage
	role           Role
	leaderID       cluster.NodeID // Known current leader if any, or NodeIDNil
	closed         atomic.Bool
	transitionHook TransitionHook
}

// NodeConfig provides initialization parameters for a Raft Node.
type NodeConfig struct {
	LocalID        cluster.NodeID
	Storage        *Storage
	TransitionHook TransitionHook
}

// NewNode initializes a Raft node.
//
// Startup Semantics:
//   - LocalID must be valid (> 0).
//   - Storage must be non-nil and open.
//   - Initial role is strictly RoleFollower.
//   - Initial leaderID is NodeIDNil (unknown).
func NewNode(cfg NodeConfig) (*Node, error) {
	if !cfg.LocalID.IsValid() {
		return nil, &errors.InvalidNodeIDError{
			NodeID: uint64(cfg.LocalID),
			Reason: "local node ID must be greater than zero",
		}
	}
	if cfg.Storage == nil {
		return nil, fmt.Errorf("%w: storage cannot be nil", osErrInvalid())
	}

	// Verify storage is readable and get recovered term
	_, err := cfg.Storage.HardState()
	if err != nil {
		return nil, fmt.Errorf("raft: failed to read recovered hard state: %w", err)
	}

	n := &Node{
		localID:        cfg.LocalID,
		storage:        cfg.Storage,
		role:           RoleFollower,
		leaderID:       cluster.NodeIDNil,
		transitionHook: cfg.TransitionHook,
	}

	return n, nil
}

// osErrInvalid helper returns standard invalid argument error without extra imports
func osErrInvalid() error {
	return errors.ErrInvalidManagerConfig
}

// LocalID returns the local node ID.
func (n *Node) LocalID() cluster.NodeID {
	return n.localID
}

// Role returns the currently active server role.
func (n *Node) Role() Role {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.role
}

// Term returns the current term from storage.
func (n *Node) Term() (Term, error) {
	if n.closed.Load() {
		return 0, errors.ErrRaftStateClosed
	}
	return n.storage.Term()
}

// VotedFor returns candidate voted for in current term.
func (n *Node) VotedFor() (cluster.NodeID, error) {
	if n.closed.Load() {
		return cluster.NodeIDNil, errors.ErrRaftStateClosed
	}
	return n.storage.VotedFor()
}

// LeaderID returns the currently known leader node ID, or NodeIDNil if unknown.
func (n *Node) LeaderID() cluster.NodeID {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.leaderID
}

// Storage returns the underlying persistent storage instance.
func (n *Node) Storage() *Storage {
	return n.storage
}

// BecomeCandidate transitions the server from Follower to Candidate.
//
// Invariants enforced (P15-S01-M02 / Sections 23 & 33):
//   - Disallowed from RoleLeader (a leader does not directly become candidate).
//   - If already in RoleCandidate, it is an idempotent no-op (preserves existing term and vote).
//   - From RoleFollower, increments currentTerm by 1, persists self-vote, and transitions to RoleCandidate.
func (n *Node) BecomeCandidate() error {
	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	// Validate transition from current role
	if n.role == RoleLeader {
		return fmt.Errorf("%w: leader cannot transition directly to candidate", errors.ErrRaftInvalidRoleTransition)
	}

	// Idempotent if already candidate
	if n.role == RoleCandidate {
		return nil
	}

	return n.campaignLocked()
}

// StartNewElection explicitly initiates a new election term from Follower or Candidate.
// Used by election timer timeouts (Phase 15-S02-M01).
//
// Invariants enforced:
//   - Disallowed from RoleLeader.
//   - Increments currentTerm by exactly 1.
//   - Checks for term arithmetic overflow (cannot exceed math.MaxUint64).
//   - Atomically persists HardState{Term: currentTerm+1, VotedFor: localID}.
//   - If persistence fails, node remains in its previous role.
//   - Resets known leaderID to NodeIDNil and sets role to RoleCandidate.
func (n *Node) StartNewElection() error {
	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	if n.role == RoleLeader {
		return fmt.Errorf("%w: leader cannot transition directly to candidate", errors.ErrRaftInvalidRoleTransition)
	}

	return n.campaignLocked()
}

// campaignLocked advances the term by 1, persists a self-vote, and publishes RoleCandidate.
// Caller MUST hold n.mu.
func (n *Node) campaignLocked() error {
	currTerm, err := n.storage.Term()
	if err != nil {
		return err
	}

	// Check overflow on term increment
	if currTerm == math.MaxUint64 {
		return fmt.Errorf("%w: current term %d cannot be incremented", errors.ErrRaftTermOverflow, currTerm)
	}
	newTerm := currTerm + 1

	// Durably persist new term and self-vote BEFORE updating volatile role
	newHS := HardState{
		Term:     newTerm,
		VotedFor: n.localID,
	}
	if err := n.storage.SetHardState(newHS); err != nil {
		return fmt.Errorf("raft: failed to persist candidate hard state: %w", err)
	}

	oldRole := n.role
	n.role = RoleCandidate
	n.leaderID = cluster.NodeIDNil

	if n.transitionHook != nil {
		n.transitionHook(oldRole, RoleCandidate, newTerm)
	}

	return nil
}

// BecomeLeader transitions the server from Candidate to Leader.
//
// Invariants enforced:
//   - Legal ONLY from RoleCandidate (Follower cannot directly become Leader).
//   - Does NOT increment term.
//   - Does NOT clear vote.
//   - Does NOT modify log or commit index.
//   - Sets leaderID to localID.
//   - Idempotent if already RoleLeader.
func (n *Node) BecomeLeader() error {
	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	if n.role == RoleLeader {
		return nil // Idempotent
	}

	if n.role != RoleCandidate {
		return fmt.Errorf("%w: cannot transition to leader from %s", errors.ErrRaftInvalidRoleTransition, n.role)
	}

	currTerm, err := n.storage.Term()
	if err != nil {
		return err
	}

	oldRole := n.role
	n.role = RoleLeader
	n.leaderID = n.localID

	if n.transitionHook != nil {
		n.transitionHook(oldRole, RoleLeader, currTerm)
	}

	return nil
}

// BecomeFollower transitions the server to Follower in the given term.
//
// Semantics:
//   - If newTerm > currentTerm: durably advances term and clears vote (SetTerm(newTerm)),
//     clears leaderID, and sets role to RoleFollower.
//   - If newTerm == currentTerm:
//   - If role is already RoleFollower: idempotent update of leaderID.
//   - If role is RoleCandidate or RoleLeader: transitions to RoleFollower, leaves term
//     and vote completely unchanged, and records leaderID.
//   - If newTerm < currentTerm: rejected fail-closed with ErrRaftTermRegressed.
func (n *Node) BecomeFollower(newTerm Term, leaderID cluster.NodeID) error {
	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	currTerm, err := n.storage.Term()
	if err != nil {
		return err
	}

	if newTerm < currTerm {
		return fmt.Errorf("%w: attempted term %d < current term %d", errors.ErrRaftTermRegressed, newTerm, currTerm)
	}

	if newTerm > currTerm {
		// Higher term: durably persist new term and clear vote
		if err := n.storage.SetTerm(newTerm); err != nil {
			return fmt.Errorf("raft: failed to persist higher term on stepdown: %w", err)
		}
	}

	oldRole := n.role
	n.role = RoleFollower
	n.leaderID = leaderID

	if n.transitionHook != nil && (oldRole != RoleFollower || newTerm != currTerm) {
		n.transitionHook(oldRole, RoleFollower, newTerm)
	}

	return nil
}

// ObserveHigherTerm steps down the node to Follower if incomingTerm > currentTerm.
// If incomingTerm <= currentTerm, returns false without modifying role or term.
// If incomingTerm > currentTerm, durably persists new term, clears vote, sets role to Follower,
// and returns true.
func (n *Node) ObserveHigherTerm(incomingTerm Term) (bool, error) {
	if n.closed.Load() {
		return false, errors.ErrRaftStateClosed
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	if n.closed.Load() {
		return false, errors.ErrRaftStateClosed
	}

	currTerm, err := n.storage.Term()
	if err != nil {
		return false, err
	}

	if incomingTerm <= currTerm {
		return false, nil
	}

	// Persist higher term and clear vote
	if err := n.storage.SetTerm(incomingTerm); err != nil {
		return false, fmt.Errorf("raft: failed to persist higher term %d: %w", incomingTerm, err)
	}

	oldRole := n.role
	n.role = RoleFollower
	n.leaderID = cluster.NodeIDNil

	if n.transitionHook != nil {
		n.transitionHook(oldRole, RoleFollower, incomingTerm)
	}

	return true, nil
}

// StepDownSameTerm steps down a Candidate or Leader to Follower in the same term
// (e.g. upon discovering a legitimate current-term leader).
// Leaves currentTerm and votedFor completely unchanged on disk.
func (n *Node) StepDownSameTerm(leaderID cluster.NodeID) error {
	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	currTerm, err := n.storage.Term()
	if err != nil {
		return err
	}

	oldRole := n.role
	n.role = RoleFollower
	n.leaderID = leaderID

	if n.transitionHook != nil && oldRole != RoleFollower {
		n.transitionHook(oldRole, RoleFollower, currTerm)
	}

	return nil
}

// Close marks the Node as closed. Does not close underlying Storage (caller retains ownership).
func (n *Node) Close() error {
	n.closed.Store(true)
	return nil
}
