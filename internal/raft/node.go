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

	// Election subsystem & Quorum (P15-S02-M01 & P15-S02-M02)
	topology         *cluster.Topology
	peers            []cluster.NodeID // Remote peer identities strictly excluding localID
	peerSender       PeerSender
	electionTimer    *ElectionTimer
	electionCtx      context.Context
	electionCancel   context.CancelFunc
	electionLoopWg   sync.WaitGroup
	timerLifecycleMu sync.Mutex
	timerRunning     atomic.Bool
	loopCancel       context.CancelFunc
	loopDone         chan struct{}

	// Volatile election round & leader replication state (P15-S02-M02)
	electionVotes     map[cluster.NodeID]struct{} // Set of peers that granted votes in this round
	electionRoundTerm Term                        // Term of current election round
	electionRoundGen  uint64                      // Monotonically increasing round identifier
	nextIndex         map[cluster.NodeID]LogIndex // Leader tracking: next log index to send to each remote peer
	matchIndex        map[cluster.NodeID]LogIndex // Leader tracking: highest log index known replicated on each remote peer
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

// QuorumSize returns the majority quorum size based on configured cluster membership.
// For N cluster members: quorum = floor(N/2) + 1.
func (n *Node) QuorumSize() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.quorumSizeLocked()
}

func (n *Node) quorumSizeLocked() int {
	clusterSize := len(n.peers) + 1
	return (clusterSize / 2) + 1
}

// GrantedVotesCount returns the number of granted votes recorded in the current election round.
// Returns 0 if not currently candidate or round is invalid.
func (n *Node) GrantedVotesCount() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return len(n.electionVotes)
}

// NextIndex returns a snapshot of the leader's nextIndex map for remote peers.
// Returns nil if node is not currently leader.
func (n *Node) NextIndex() map[cluster.NodeID]LogIndex {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.nextIndex == nil {
		return nil
	}
	cp := make(map[cluster.NodeID]LogIndex, len(n.nextIndex))
	for k, v := range n.nextIndex {
		cp[k] = v
	}
	return cp
}

// MatchIndex returns a snapshot of the leader's matchIndex map for remote peers.
// Returns nil if node is not currently leader.
func (n *Node) MatchIndex() map[cluster.NodeID]LogIndex {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.matchIndex == nil {
		return nil
	}
	cp := make(map[cluster.NodeID]LogIndex, len(n.matchIndex))
	for k, v := range n.matchIndex {
		cp[k] = v
	}
	return cp
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
// Initializes volatile election round state (localID counted as self-vote).
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

	// Volatile election round state (Section 5, 6):
	// Fresh vote map with self-vote counted
	n.electionVotes = map[cluster.NodeID]struct{}{
		n.localID: {},
	}
	n.electionRoundTerm = newTerm
	n.electionRoundGen++

	return oldRole, RoleCandidate, newTerm, true, nil
}

// becomeLeaderLocked sets role to Leader, initializes volatile leader replication state,
// invalidates candidate vote counting state, and stops the follower election timer.
// Caller MUST hold n.mu.
func (n *Node) becomeLeaderLocked(term Term, lastLogIdx LogIndex) {
	n.role = RoleLeader
	n.leaderID = n.localID

	// Initialize leader volatile replication state for each configured remote peer (Section 14)
	n.nextIndex = make(map[cluster.NodeID]LogIndex, len(n.peers))
	n.matchIndex = make(map[cluster.NodeID]LogIndex, len(n.peers))
	for _, peerID := range n.peers {
		if peerID == n.localID {
			continue
		}
		n.nextIndex[peerID] = lastLogIdx + 1
		n.matchIndex[peerID] = 0
	}

	// Invalidate candidate vote-counting state
	n.electionVotes = nil

	// Leader must not run follower election timer (Section 17 Event D / Section 19)
	if n.electionTimer != nil {
		n.electionTimer.Stop()
	}
}

// clearLeaderAndElectionStateLocked clears volatile leader replication state and candidate vote tracking.
// Caller MUST hold n.mu.
func (n *Node) clearLeaderAndElectionStateLocked() {
	n.electionVotes = nil
	n.electionRoundTerm = 0
	n.nextIndex = nil
	n.matchIndex = nil
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
//   - Initializes volatile leader replication state (nextIndex/matchIndex).
//   - Invokes TransitionHook outside of n.mu.
//   - Triggers one-time immediate empty AppendEntries heartbeat broadcast outside of n.mu.
//
// API Safety Notice (Section 13):
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

	lastIdx, lastTerm, err := n.storage.LastIndexAndTerm()
	if err != nil {
		n.mu.Unlock()
		return err
	}

	oldRole := n.role
	n.becomeLeaderLocked(currTerm, lastIdx)
	shouldHook := (n.transitionHook != nil)
	n.mu.Unlock()

	if shouldHook && n.transitionHook != nil {
		n.transitionHook(oldRole, RoleLeader, currTerm)
	}

	// Broadcast one-time immediate empty AppendEntries heartbeats (without holding Node.mu)
	n.sendImmediateHeartbeats(currTerm, lastIdx, lastTerm)

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
	n.clearLeaderAndElectionStateLocked()

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
	n.clearLeaderAndElectionStateLocked()
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
	n.clearLeaderAndElectionStateLocked()
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

	n.timerRunning.Store(false)

	// Cancel top-level election context and active loop context
	if n.electionCancel != nil {
		n.electionCancel()
	}

	n.timerLifecycleMu.Lock()
	if n.loopCancel != nil {
		n.loopCancel()
		n.loopCancel = nil
	}
	n.timerLifecycleMu.Unlock()

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

	if n.timerRunning.Load() {
		return nil // Idempotent (Section 19)
	}

	loopCtx, cancel := context.WithCancel(n.electionCtx)
	done := make(chan struct{})
	n.loopCancel = cancel
	n.loopDone = done
	n.timerRunning.Store(true)
	n.electionTimer.Reset()
	n.electionLoopWg.Add(1)
	go n.runElectionLoop(loopCtx, done)

	return nil
}

// StopElectionTimer halts the background election loop and timer. Idempotent.
// Blocks until the election loop goroutine has completely terminated (P15-S02-M02 corrective hardening).
func (n *Node) StopElectionTimer() {
	n.timerLifecycleMu.Lock()
	defer n.timerLifecycleMu.Unlock()

	if !n.timerRunning.Load() {
		return
	}

	n.timerRunning.Store(false)
	if n.loopCancel != nil {
		n.loopCancel()
		n.loopCancel = nil
	}
	n.electionTimer.Stop()

	done := n.loopDone
	n.loopDone = nil
	if done != nil {
		<-done
	}
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
func (n *Node) runElectionLoop(ctx context.Context, done chan struct{}) {
	defer n.electionLoopWg.Done()
	defer close(done)

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
			if !n.timerRunning.Load() {
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
	if !n.timerRunning.Load() {
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

	// Section 7: Single-Node N=1 Cluster Quorum Check
	// If candidate's self-vote immediately satisfies quorum (quorum == 1)
	n.mu.Lock()
	if n.role == RoleCandidate && n.quorumSizeLocked() <= len(n.electionVotes) {
		currTerm, err := n.storage.Term()
		if err != nil {
			n.mu.Unlock()
			return
		}
		lastIdx, lastTerm, err := n.storage.LastIndexAndTerm()
		if err != nil {
			n.mu.Unlock()
			return
		}
		oldRole := n.role
		n.becomeLeaderLocked(currTerm, lastIdx)
		shouldHook := (n.transitionHook != nil)
		n.mu.Unlock()

		if shouldHook && n.transitionHook != nil {
			n.transitionHook(oldRole, RoleLeader, currTerm)
		}
		n.sendImmediateHeartbeats(currTerm, lastIdx, lastTerm)
		return
	}
	n.mu.Unlock()

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

// sendImmediateHeartbeats broadcasts a one-time empty AppendEntries frame to each configured remote peer.
// Invariants enforced (P15-S02-M02 / Section 15):
//   - Must execute strictly outside of Node.mu (no mutex held during network I/O).
//   - Uses current leader term and localID as LeaderID.
//   - Entries is nil (empty heartbeat).
//   - Includes fresh cryptographic nonce and monotonically increasing sequence ID.
//   - Never targets self.
//   - Broadcasts to configured remote peers.
func (n *Node) sendImmediateHeartbeats(term Term, lastIdx LogIndex, lastTerm Term) {
	if n.peerSender == nil || len(n.peers) == 0 {
		return
	}

	for _, peerID := range n.peers {
		if peerID == n.localID {
			continue // Never send to self
		}

		nonce, err := transport.GenerateNonce()
		if err != nil {
			continue
		}

		seqID := n.peerSender.NextSeqID()
		req := &transport.AppendEntriesRequest{
			Term:         uint64(term),
			LeaderID:     n.localID,
			PrevLogIndex: uint64(lastIdx),
			PrevLogTerm:  uint64(lastTerm),
			LeaderCommit: 0,
			Nonce:        nonce,
			Entries:      nil, // Empty heartbeat
		}

		frame, err := transport.EncodeAppendEntries(req, seqID)
		if err != nil {
			continue
		}

		_ = n.peerSender.Send(n.electionCtx, peerID, frame)
	}
}

// HandleRequestVoteResponse processes an incoming RequestVote response from a peer.
//
// Invariants enforced (P15-S02-M02 / Sections 8-12, 16-19):
//  1. Rejects if node is closed.
//  2. Validates fromPeerID: must be valid (> 0), cannot be self, and must belong to cluster membership.
//  3. If resp == nil: rejected with ErrNilReceiver.
//  4. Stale Term: if resp.Term < currentTerm, response is discarded without state mutation.
//  5. Higher Term: if resp.Term > currentTerm, durably advances to the higher term, clears votedFor,
//     steps down to RoleFollower, invalidates election state, re-arms election timer, and returns nil.
//     If durable storage fails, fails closed and returns error.
//  6. Same Term:
//     - If role != RoleCandidate or electionRoundTerm != currentTerm: ignored (stale/post-leadership response).
//     - If resp.VoteGranted == false: ignored.
//     - Positive votes are deduplicated per peer identity (each peer counts at most once).
//     - When vote count reaches quorum (floor(N/2) + 1), candidate transitions to RoleLeader exactly once,
//     initializes leader volatile replication state (nextIndex/matchIndex), invalidates election round state,
//     stops election timer, invokes transition hook, and broadcasts immediate empty AppendEntries heartbeats.
//  7. Network I/O and user callbacks execute strictly outside of Node.mu.
func (n *Node) HandleRequestVoteResponse(fromPeerID cluster.NodeID, resp *transport.RequestVoteResponse) error {
	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}
	if resp == nil {
		return errors.ErrNilReceiver
	}

	// Validate sender peer ID (Section 9.2)
	if !fromPeerID.IsValid() {
		return &errors.InvalidNodeIDError{NodeID: uint64(fromPeerID), Reason: "sender peer ID must be greater than zero"}
	}
	if fromPeerID == n.localID {
		return fmt.Errorf("%w: received vote response from local node ID %d", errors.ErrRaftSelfVoteRPC, n.localID)
	}

	// Validate cluster membership
	if n.topology != nil && !n.topology.Contains(fromPeerID) {
		return &errors.UnknownPeerError{NodeID: uint64(fromPeerID)}
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
			return &errors.UnknownPeerError{NodeID: uint64(fromPeerID)}
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
		return fmt.Errorf("raft: failed to read current term: %w", err)
	}

	// Section 9.5: Stale Term (resp.Term < currentTerm) -> ignore without mutating state
	if resp.Term < uint64(currTerm) {
		n.mu.Unlock()
		return nil
	}

	// Section 9.5: Higher Term (resp.Term > currentTerm) -> durable stepdown to Follower
	if resp.Term > uint64(currTerm) {
		newTerm := Term(resp.Term)
		// Persist higher term and clear vote BEFORE updating volatile role
		if err := n.storage.SetTerm(newTerm); err != nil {
			n.mu.Unlock()
			return fmt.Errorf("raft: failed to persist higher term %d on vote response: %w", resp.Term, err)
		}

		oldRole := n.role
		n.role = RoleFollower
		n.leaderID = cluster.NodeIDNil
		n.clearLeaderAndElectionStateLocked()
		n.mu.Unlock()

		if n.transitionHook != nil {
			n.transitionHook(oldRole, RoleFollower, newTerm)
		}

		n.ResetElectionTimer()
		return nil
	}

	// Section 9.4 & 16: Same Term (resp.Term == currentTerm)
	// If node is no longer candidate (e.g. already Leader or stepped down), ignore
	if n.role != RoleCandidate || n.electionRoundTerm != currTerm {
		n.mu.Unlock()
		return nil
	}

	// Section 11: Only positive votes count
	if !resp.VoteGranted {
		n.mu.Unlock()
		return nil
	}

	// Section 10: Deduplication per peer identity
	if _, alreadyVoted := n.electionVotes[fromPeerID]; alreadyVoted {
		n.mu.Unlock()
		return nil
	}

	n.electionVotes[fromPeerID] = struct{}{}
	voteCount := len(n.electionVotes)
	quorum := n.quorumSizeLocked()

	// Quorum not yet reached
	if voteCount < quorum {
		n.mu.Unlock()
		return nil
	}

	// Section 12: Quorum reached! Transition Candidate -> Leader
	oldRole := n.role
	lastIdx, lastTerm, err := n.storage.LastIndexAndTerm()
	if err != nil {
		n.mu.Unlock()
		return fmt.Errorf("raft: failed to read last index and term on leadership transition: %w", err)
	}

	n.becomeLeaderLocked(currTerm, lastIdx)
	shouldHook := (n.transitionHook != nil)
	n.mu.Unlock()

	if shouldHook && n.transitionHook != nil {
		n.transitionHook(oldRole, RoleLeader, currTerm)
	}

	// Section 15: Send one-time immediate empty AppendEntries heartbeats (no Node.mu held)
	n.sendImmediateHeartbeats(currTerm, lastIdx, lastTerm)

	return nil
}

// HandlePeerFrame dispatches incoming peer protocol frames to their corresponding Raft handlers.
// Completes the request-response dispatch path between transport and consensus (Section 22, 23).
func (n *Node) HandlePeerFrame(fromPeerID cluster.NodeID, frame *transport.Frame) error {
	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}
	if frame == nil {
		return errors.ErrNilReceiver
	}

	switch transport.PeerMessageType(frame.Header.OpCode) {
	case transport.PeerOpRequestVote:
		req, err := transport.DecodeRequestVote(frame)
		if err != nil {
			return err
		}

		resp, err := n.HandleRequestVote(fromPeerID, req)
		if err != nil {
			return err
		}

		if n.peerSender != nil && fromPeerID.IsValid() && fromPeerID != n.localID {
			seqID := n.peerSender.NextSeqID()
			respFrame, encErr := transport.EncodeRequestVoteResponse(resp, seqID)
			if encErr == nil {
				_ = n.peerSender.Send(n.electionCtx, fromPeerID, respFrame)
			}
		}
		return nil

	case transport.PeerOpRequestVoteResponse:
		resp, err := transport.DecodeRequestVoteResponse(frame)
		if err != nil {
			return err
		}
		return n.HandleRequestVoteResponse(fromPeerID, resp)

	default:
		return nil
	}
}
