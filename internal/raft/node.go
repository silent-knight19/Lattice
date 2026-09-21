package raft

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/transport"
)

// TransitionHook is an optional callback invoked synchronously when a role transition completes.
// Passed previous role, new role, and new term.
type TransitionHook func(from, to Role, term Term)

// PeerSender transmits framed Raft protocol messages to cluster peers.
// Implemented by *transport.PeerConnectionManager.
type PeerSender interface {
	NextSeqID() uint64
	Send(ctx context.Context, peerID cluster.NodeID, frame *transport.Frame) error
}

// Node represents the core Raft consensus state machine governing role transitions
// (Follower <-> Candidate <-> Leader) and integrating with persistent storage.
//
// Invariants enforced (P15-S01-M02 & P15-S02-M01):
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
//  9. Election timer is dynamically randomized per round ([150ms, 300ms]), generation-tracked,
//     and stopped when RoleLeader is active.
//
// 10. Outbound RequestVote broadcasts are transmitted to remote peers without holding the Node mutex.
type Node struct {
	mu             sync.RWMutex
	localID        cluster.NodeID
	storage        *Storage
	role           Role
	leaderID       cluster.NodeID // Known current leader if any, or NodeIDNil
	closed         atomic.Bool
	transitionHook TransitionHook

	// Election subsystem (P15-S02-M01)
	topology         *cluster.Topology
	peers            []cluster.NodeID // Remote peer identities strictly excluding localID
	peerSender       PeerSender
	electionTimer    *ElectionTimer
	electionCtx      context.Context
	electionCancel   context.CancelFunc
	electionLoopWg   sync.WaitGroup
	timerLifecycleMu sync.Mutex
	timerRunning     bool
}

// NodeConfig provides initialization parameters for a Raft Node.
type NodeConfig struct {
	LocalID          cluster.NodeID
	Storage          *Storage
	TransitionHook   TransitionHook
	Topology         *cluster.Topology
	Peers            []cluster.NodeID
	PeerSender       PeerSender
	DurationProvider DurationProvider
}

// NewNode initializes a Raft node.
//
// Startup Semantics:
//   - LocalID must be valid (> 0).
//   - Storage must be non-nil and open.
//   - Initial role is strictly RoleFollower.
//   - Initial leaderID is NodeIDNil (unknown).
//   - Election timer is initialized with DurationProvider (or DefaultDurationProvider if nil).
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

	var remotePeers []cluster.NodeID
	if cfg.Topology != nil {
		if cfg.Topology.LocalID() != cfg.LocalID {
			return nil, fmt.Errorf("%w: topology local ID %d does not match node local ID %d",
				errors.ErrInvalidManagerConfig, cfg.Topology.LocalID(), cfg.LocalID)
		}
		for _, p := range cfg.Topology.RemotePeers() {
			remotePeers = append(remotePeers, p.ID)
		}
	} else if len(cfg.Peers) > 0 {
		seen := make(map[cluster.NodeID]struct{}, len(cfg.Peers))
		for _, p := range cfg.Peers {
			if p == cfg.LocalID || !p.IsValid() {
				continue
			}
			if _, exists := seen[p]; !exists {
				seen[p] = struct{}{}
				remotePeers = append(remotePeers, p)
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	n := &Node{
		localID:        cfg.LocalID,
		storage:        cfg.Storage,
		role:           RoleFollower,
		leaderID:       cluster.NodeIDNil,
		transitionHook: cfg.TransitionHook,
		topology:       cfg.Topology,
		peers:          remotePeers,
		peerSender:     cfg.PeerSender,
		electionTimer:  NewElectionTimer(cfg.DurationProvider),
		electionCtx:    ctx,
		electionCancel: cancel,
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
//   - Invokes TransitionHook outside of n.mu to prevent reentrancy deadlock.
func (n *Node) BecomeCandidate() error {
	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	n.mu.Lock()
	if n.closed.Load() {
		n.mu.Unlock()
		return errors.ErrRaftStateClosed
	}

	// Validate transition from current role
	if n.role == RoleLeader {
		n.mu.Unlock()
		return fmt.Errorf("%w: leader cannot transition directly to candidate", errors.ErrRaftInvalidRoleTransition)
	}

	// Idempotent if already candidate
	if n.role == RoleCandidate {
		n.mu.Unlock()
		return nil
	}

	oldRole, newRole, term, shouldHook, err := n.campaignLocked()
	n.mu.Unlock()

	if err != nil {
		return err
	}

	if shouldHook && n.transitionHook != nil {
		n.transitionHook(oldRole, newRole, term)
	}

	return nil
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
//   - Invokes TransitionHook outside of n.mu to prevent reentrancy deadlock.
func (n *Node) StartNewElection() error {
	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	n.mu.Lock()
	if n.closed.Load() {
		n.mu.Unlock()
		return errors.ErrRaftStateClosed
	}

	if n.role == RoleLeader {
		n.mu.Unlock()
		return fmt.Errorf("%w: leader cannot transition directly to candidate", errors.ErrRaftInvalidRoleTransition)
	}

	oldRole, newRole, term, shouldHook, err := n.campaignLocked()
	n.mu.Unlock()

	if err != nil {
		return err
	}

	if shouldHook && n.transitionHook != nil {
		n.transitionHook(oldRole, newRole, term)
	}

	return nil
}

// campaignLocked advances the term by 1, persists a self-vote, and publishes RoleCandidate.
// Caller MUST hold n.mu. Does NOT invoke transitionHook (returned for caller to invoke outside lock).
func (n *Node) campaignLocked() (oldRole, newRole Role, term Term, shouldHook bool, err error) {
	currTerm, err := n.storage.Term()
	if err != nil {
		return 0, 0, 0, false, err
	}

	// Check overflow on term increment
	if currTerm == math.MaxUint64 {
		return 0, 0, 0, false, fmt.Errorf("%w: current term %d cannot be incremented", errors.ErrRaftTermOverflow, currTerm)
	}
	newTerm := currTerm + 1

	// Durably persist new term and self-vote BEFORE updating volatile role
	newHS := HardState{
		Term:     newTerm,
		VotedFor: n.localID,
	}
	if err := n.storage.SetHardState(newHS); err != nil {
		return 0, 0, 0, false, fmt.Errorf("raft: failed to persist candidate hard state: %w", err)
	}

	oldRole = n.role
	n.role = RoleCandidate
	n.leaderID = cluster.NodeIDNil

	return oldRole, RoleCandidate, newTerm, true, nil
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
//   - Invokes TransitionHook outside of n.mu.
//
// API Safety Notice (Section 10):
// BecomeLeader is strictly an internal role-transition primitive and does NOT assert election
// safety or quorum proof. Quorum vote verification is the sole responsibility of the
// election subsystem (P15-S02-M02).
func (n *Node) BecomeLeader() error {
	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	n.mu.Lock()
	if n.closed.Load() {
		n.mu.Unlock()
		return errors.ErrRaftStateClosed
	}

	if n.role == RoleLeader {
		n.mu.Unlock()
		return nil // Idempotent
	}

	if n.role != RoleCandidate {
		currRole := n.role
		n.mu.Unlock()
		return fmt.Errorf("%w: cannot transition to leader from %s", errors.ErrRaftInvalidRoleTransition, currRole)
	}

	currTerm, err := n.storage.Term()
	if err != nil {
		n.mu.Unlock()
		return err
	}

	oldRole := n.role
	n.role = RoleLeader
	n.leaderID = n.localID
	shouldHook := (n.transitionHook != nil)
	n.mu.Unlock()

	// Section 17 Event D: Leader must not run follower election timer
	if n.electionTimer != nil {
		n.electionTimer.Stop()
	}

	if shouldHook && n.transitionHook != nil {
		n.transitionHook(oldRole, RoleLeader, currTerm)
	}

	return nil
}

// BecomeFollower transitions the server to Follower in the given term.
//
// Semantics:
//   - If leaderID != NodeIDNil, validates leaderID is valid and != localID (cannot recognize self as external leader).
//   - If newTerm > currentTerm: durably advances term and clears vote (SetTerm(newTerm)),
//     clears leaderID, and sets role to RoleFollower.
//   - If newTerm == currentTerm:
//   - If role is already RoleFollower: idempotent update of leaderID.
//   - If role is RoleCandidate or RoleLeader: transitions to RoleFollower, leaves term
//     and vote completely unchanged, and records leaderID.
//   - If newTerm < currentTerm: rejected fail-closed with ErrRaftTermRegressed.
//   - Invokes TransitionHook outside of n.mu.
//   - Resets election timer upon transitioning to Follower.
func (n *Node) BecomeFollower(newTerm Term, leaderID cluster.NodeID) error {
	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	if leaderID != cluster.NodeIDNil {
		if !leaderID.IsValid() {
			return &errors.InvalidNodeIDError{NodeID: uint64(leaderID), Reason: "leader node ID is invalid"}
		}
		if leaderID == n.localID {
			return fmt.Errorf("%w: node %d cannot recognize self as external leader on stepdown",
				errors.ErrRaftInvalidRoleTransition, n.localID)
		}
	}

	n.mu.Lock()
	if n.closed.Load() {
		n.mu.Unlock()
		return errors.ErrRaftStateClosed
	}

	currTerm, err := n.storage.Term()
	if err != nil {
		n.mu.Unlock()
		return err
	}

	if newTerm < currTerm {
		n.mu.Unlock()
		return fmt.Errorf("%w: attempted term %d < current term %d", errors.ErrRaftTermRegressed, newTerm, currTerm)
	}

	if newTerm > currTerm {
		// Higher term: durably persist new term and clear vote
		if err := n.storage.SetTerm(newTerm); err != nil {
			n.mu.Unlock()
			return fmt.Errorf("raft: failed to persist higher term on stepdown: %w", err)
		}
	}

	oldRole := n.role
	n.role = RoleFollower
	n.leaderID = leaderID

	shouldHook := (oldRole != RoleFollower || newTerm != currTerm)
	n.mu.Unlock()

	if shouldHook && n.transitionHook != nil {
		n.transitionHook(oldRole, RoleFollower, newTerm)
	}

	n.ResetElectionTimer()

	return nil
}

// ObserveHigherTerm steps down the node to Follower if incomingTerm > currentTerm.
// If incomingTerm <= currentTerm, returns false without modifying role or term.
// If incomingTerm > currentTerm, durably persists new term, clears vote, sets role to Follower,
// and returns true. Invokes TransitionHook outside of n.mu.
func (n *Node) ObserveHigherTerm(incomingTerm Term) (bool, error) {
	if n.closed.Load() {
		return false, errors.ErrRaftStateClosed
	}

	n.mu.Lock()
	if n.closed.Load() {
		n.mu.Unlock()
		return false, errors.ErrRaftStateClosed
	}

	currTerm, err := n.storage.Term()
	if err != nil {
		n.mu.Unlock()
		return false, err
	}

	if incomingTerm <= currTerm {
		n.mu.Unlock()
		return false, nil
	}

	// Persist higher term and clear vote
	if err := n.storage.SetTerm(incomingTerm); err != nil {
		n.mu.Unlock()
		return false, fmt.Errorf("raft: failed to persist higher term %d: %w", incomingTerm, err)
	}

	oldRole := n.role
	n.role = RoleFollower
	n.leaderID = cluster.NodeIDNil
	n.mu.Unlock()

	if n.transitionHook != nil {
		n.transitionHook(oldRole, RoleFollower, incomingTerm)
	}

	n.ResetElectionTimer()

	return true, nil
}

// StepDownSameTerm steps down a Candidate or Leader to Follower in the same term
// (e.g. upon discovering a legitimate current-term leader).
// Leaves currentTerm and votedFor completely unchanged on disk.
// Invokes TransitionHook outside of n.mu.
func (n *Node) StepDownSameTerm(leaderID cluster.NodeID) error {
	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	if leaderID != cluster.NodeIDNil {
		if !leaderID.IsValid() {
			return &errors.InvalidNodeIDError{NodeID: uint64(leaderID), Reason: "leader node ID is invalid"}
		}
		if leaderID == n.localID {
			return fmt.Errorf("%w: node %d cannot recognize self as external leader on stepdown",
				errors.ErrRaftInvalidRoleTransition, n.localID)
		}
	}

	n.mu.Lock()
	if n.closed.Load() {
		n.mu.Unlock()
		return errors.ErrRaftStateClosed
	}

	currTerm, err := n.storage.Term()
	if err != nil {
		n.mu.Unlock()
		return err
	}

	oldRole := n.role
	n.role = RoleFollower
	n.leaderID = leaderID
	shouldHook := (oldRole != RoleFollower)
	n.mu.Unlock()

	if shouldHook && n.transitionHook != nil {
		n.transitionHook(oldRole, RoleFollower, currTerm)
	}

	n.ResetElectionTimer()

	return nil
}

// Close cleanly shuts down the Node and synchronizes with any in-flight transitions.
// Does not close underlying Storage (caller retains ownership).
func (n *Node) Close() error {
	if !n.closed.CompareAndSwap(false, true) {
		return nil
	}

	// Cancel election context so election loop terminates
	if n.electionCancel != nil {
		n.electionCancel()
	}

	// Permanently stop election timer
	if n.electionTimer != nil {
		n.electionTimer.Close()
	}

	// Wait for election loop goroutine to exit
	n.electionLoopWg.Wait()

	// Synchronize with any active state transition so Close cannot return while
	// an in-flight transition is mutating Node state (Section 7).
	n.mu.Lock()
	defer n.mu.Unlock()

	return nil
}

// StartElectionTimer begins the election timeout countdown.
// Idempotent if already started. Returns ErrRaftStateClosed if Node is closed.
func (n *Node) StartElectionTimer() error {
	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	n.timerLifecycleMu.Lock()
	defer n.timerLifecycleMu.Unlock()

	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	if n.timerRunning {
		return nil // Idempotent (Section 19)
	}

	n.timerRunning = true
	n.electionTimer.Reset()
	n.electionLoopWg.Add(1)
	go n.runElectionLoop(n.electionCtx)

	return nil
}

// StopElectionTimer halts the background election loop and timer. Idempotent.
func (n *Node) StopElectionTimer() {
	n.timerLifecycleMu.Lock()
	defer n.timerLifecycleMu.Unlock()

	if !n.timerRunning {
		return
	}

	n.timerRunning = false
	n.electionTimer.Stop()
}

// ResetElectionTimer resets the election countdown with a freshly randomized duration.
// Safe to invoke from RequestVote handler, future AppendEntries handler, or election loop.
// No-op if node is closed or currently RoleLeader (Section 17 Event D).
func (n *Node) ResetElectionTimer() {
	if n.closed.Load() {
		return
	}
	if n.Role() == RoleLeader {
		return
	}
	if n.electionTimer != nil {
		n.electionTimer.Reset()
	}
}

// ElectionTimer returns the underlying ElectionTimer instance for introspection.
func (n *Node) ElectionTimer() *ElectionTimer {
	return n.electionTimer
}

// runElectionLoop listens for timer expirations and triggers election rounds.
func (n *Node) runElectionLoop(ctx context.Context) {
	defer n.electionLoopWg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case gen, ok := <-n.electionTimer.C():
			if !ok {
				return
			}
			if n.closed.Load() {
				return
			}

			// Discard stale timer generation (Section 45)
			if gen != n.electionTimer.CurrentGen() {
				continue
			}

			n.handleElectionTimeout()
		}
	}
}

// handleElectionTimeout handles an election timeout event.
func (n *Node) handleElectionTimeout() {
	if n.closed.Load() {
		return
	}

	// Section 21: If already Leader, discard stale timeout
	if n.Role() == RoleLeader {
		return
	}

	// Section 20: Both Follower and Candidate advance term and start a new election round
	if err := n.StartNewElection(); err != nil {
		return
	}

	// Section 17 Event C: Candidate starts new election round -> reset timer with fresh timeout
	n.ResetElectionTimer()

	// Read state for RequestVote broadcast without holding Node lock (Section 44)
	term, err := n.Term()
	if err != nil {
		return
	}
	lastIdx, lastTerm, err := n.storage.LastIndexAndTerm()
	if err != nil {
		return
	}

	// Section 25: Broadcast RequestVote to all configured remote peers asynchronously
	n.broadcastRequestVote(term, lastIdx, lastTerm)
}

// broadcastRequestVote constructs and sends RequestVote frames to all remote peers.
func (n *Node) broadcastRequestVote(term Term, lastIdx LogIndex, lastTerm Term) {
	if n.peerSender == nil || len(n.peers) == 0 {
		return
	}

	for _, peerID := range n.peers {
		if peerID == n.localID {
			continue // Section 25: Never dial self over network
		}

		nonce, err := transport.GenerateNonce()
		if err != nil {
			continue
		}

		seqID := n.peerSender.NextSeqID()
		req := &transport.RequestVoteRequest{
			Term:         uint64(term),
			CandidateID:  n.localID,
			LastLogIndex: uint64(lastIdx),
			LastLogTerm:  uint64(lastTerm),
			Nonce:        nonce,
		}

		frame, err := transport.EncodeRequestVote(req, seqID)
		if err != nil {
			continue
		}

		// Transmit frame to peer. Transport errors do not fail local election state.
		_ = n.peerSender.Send(n.electionCtx, peerID, frame)
	}
}

// BroadcastRequestVote manually broadcasts RequestVote to all configured remote peers.
// Can be called directly by tests or external drivers.
func (n *Node) BroadcastRequestVote() error {
	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}
	term, err := n.Term()
	if err != nil {
		return err
	}
	lastIdx, lastTerm, err := n.storage.LastIndexAndTerm()
	if err != nil {
		return err
	}
	n.broadcastRequestVote(term, lastIdx, lastTerm)
	return nil
}

// HandleRequestVote processes an incoming RequestVote RPC from a peer.
//
// Protocol Invariants Enforced (P15-S02-M01):
//  1. Sender Identity Binding (Section 26): req.CandidateID must equal fromPeerID.
//  2. Self Request Rejection (Section 27): Remote requests from or for self are rejected.
//  3. Topology Boundary (Section 28): Unregistered / unknown peers are rejected fail-closed.
//  4. Log Invariant Enforcement (Section 33): Candidate coordinates (index > 0, term 0) are rejected.
//  5. Stale Term (Section 29): If req.Term < currentTerm, returns VoteGranted=false, Term=currentTerm.
//  6. Higher Term (Section 30, 40): If req.Term > currentTerm, durably persists new term, clears vote,
//     and steps down to Follower BEFORE evaluating vote. Persistence failure fails closed.
//  7. Same-Term Vote Safety (Section 31, 35): Vote granted iff votedFor is nil or candidateID.
//  8. Log Freshness Rule (Section 32): Candidate log must be at least as up-to-date as voter log.
//  9. Vote Durability (Section 34, 41): Vote is durably persisted BEFORE returning VoteGranted=true.
//
// 10. Timer Reset (Section 34, 42): ResetElectionTimer is invoked outside Node mutex upon successful grant.
func (n *Node) HandleRequestVote(fromPeerID cluster.NodeID, req *transport.RequestVoteRequest) (*transport.RequestVoteResponse, error) {
	if n.closed.Load() {
		return nil, errors.ErrRaftStateClosed
	}
	if req == nil {
		return nil, errors.ErrNilReceiver
	}

	// Validate sender peer ID
	if !fromPeerID.IsValid() {
		return nil, &errors.InvalidNodeIDError{NodeID: uint64(fromPeerID), Reason: "sender peer ID must be greater than zero"}
	}
	if !req.CandidateID.IsValid() {
		return nil, &errors.InvalidNodeIDError{NodeID: uint64(req.CandidateID), Reason: "candidate ID in RequestVote must be greater than zero"}
	}

	// Section 26: Transport Identity Binding
	if fromPeerID != req.CandidateID {
		return nil, fmt.Errorf("%w: sender peer ID %d does not match candidate ID %d",
			errors.ErrRaftSenderMismatch, fromPeerID, req.CandidateID)
	}

	// Section 27: Self Request Rejection
	if fromPeerID == n.localID || req.CandidateID == n.localID {
		return nil, fmt.Errorf("%w: received remote RequestVote for local node ID %d",
			errors.ErrRaftSelfVoteRPC, n.localID)
	}

	// Section 28: Unknown Peer Rejection
	if n.topology != nil && !n.topology.Contains(fromPeerID) {
		return nil, &errors.UnknownPeerError{NodeID: uint64(fromPeerID)}
	}
	if n.topology == nil && len(n.peers) > 0 {
		known := false
		for _, p := range n.peers {
			if p == fromPeerID {
				known = true
				break
			}
		}
		if !known {
			return nil, &errors.UnknownPeerError{NodeID: uint64(fromPeerID)}
		}
	}

	// Section 33: Log invariant validation
	if err := ValidateCandidateLogCoordinates(req.LastLogIndex, req.LastLogTerm); err != nil {
		return nil, err
	}

	n.mu.Lock()
	if n.closed.Load() {
		n.mu.Unlock()
		return nil, errors.ErrRaftStateClosed
	}

	currTerm, err := n.storage.Term()
	if err != nil {
		n.mu.Unlock()
		return nil, fmt.Errorf("raft: failed to read current term: %w", err)
	}

	// Section 29: Stale Term
	if req.Term < uint64(currTerm) {
		n.mu.Unlock()
		return &transport.RequestVoteResponse{
			Term:        uint64(currTerm),
			VoteGranted: false,
		}, nil
	}

	var (
		oldRole            = n.role
		termTransitioned   = false
		stepDownTargetTerm Term
	)

	// Section 30, 40: Higher Term
	if req.Term > uint64(currTerm) {
		stepDownTargetTerm = Term(req.Term)
		// Persist higher term and clear vote on disk
		if err := n.storage.SetTerm(stepDownTargetTerm); err != nil {
			n.mu.Unlock()
			return nil, fmt.Errorf("raft: failed to persist higher term %d on RequestVote: %w", req.Term, err)
		}

		n.role = RoleFollower
		n.leaderID = cluster.NodeIDNil
		currTerm = stepDownTargetTerm
		termTransitioned = true
	}

	// Read vote status in the current term
	votedFor, err := n.storage.VotedFor()
	if err != nil {
		n.mu.Unlock()
		if termTransitioned && n.transitionHook != nil {
			n.transitionHook(oldRole, RoleFollower, currTerm)
		}
		return nil, fmt.Errorf("raft: failed to read votedFor: %w", err)
	}

	// Section 31, 35: Same-term vote availability
	canVote := (votedFor == cluster.NodeIDNil || votedFor == req.CandidateID)
	if !canVote {
		n.mu.Unlock()
		if termTransitioned && n.transitionHook != nil {
			n.transitionHook(oldRole, RoleFollower, currTerm)
		}
		return &transport.RequestVoteResponse{
			Term:        uint64(currTerm),
			VoteGranted: false,
		}, nil
	}

	// Section 32: Candidate Log Up-To-Date check
	localLastIdx, localLastTerm, err := n.storage.LastIndexAndTerm()
	if err != nil {
		n.mu.Unlock()
		if termTransitioned && n.transitionHook != nil {
			n.transitionHook(oldRole, RoleFollower, currTerm)
		}
		return nil, fmt.Errorf("raft: failed to read local last log index and term: %w", err)
	}

	if !IsCandidateLogUpToDate(req.LastLogTerm, req.LastLogIndex, uint64(localLastTerm), uint64(localLastIdx)) {
		n.mu.Unlock()
		if termTransitioned && n.transitionHook != nil {
			n.transitionHook(oldRole, RoleFollower, currTerm)
		}
		return &transport.RequestVoteResponse{
			Term:        uint64(currTerm),
			VoteGranted: false,
		}, nil
	}

	// Section 34, 41: Persist vote before granting
	if votedFor == cluster.NodeIDNil {
		if err := n.storage.SetVote(req.CandidateID); err != nil {
			n.mu.Unlock()
			if termTransitioned && n.transitionHook != nil {
				n.transitionHook(oldRole, RoleFollower, currTerm)
			}
			return nil, fmt.Errorf("raft: failed to persist vote for candidate %d: %w", req.CandidateID, err)
		}
	}

	n.mu.Unlock()

	// Invoke hook outside Node lock if role or term changed
	if termTransitioned && n.transitionHook != nil {
		n.transitionHook(oldRole, RoleFollower, currTerm)
	}

	// Section 34, 42: Reset election timer upon granting vote (strictly outside Node mutex!)
	n.ResetElectionTimer()

	return &transport.RequestVoteResponse{
		Term:        uint64(currTerm),
		VoteGranted: true,
	}, nil
}
