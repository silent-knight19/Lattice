package raft

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/transport"
)

// DefaultHeartbeatInterval is the standard heartbeat cadence specified by Raft (50ms).
const DefaultHeartbeatInterval = 50 * time.Millisecond

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
//     in its current valid state.
//  9. Role transitions are verifiable and non-blocking under the race detector.
type Node struct {
	mu             sync.RWMutex
	localID        cluster.NodeID
	storage        *Storage
	role           Role
	leaderID       cluster.NodeID // Known current leader if any, or NodeIDNil
	closed         atomic.Bool
	transitionHook TransitionHook

	// P16-SEC-F02: Leadership epoch counter. Monotonically incremented under Node.mu.Lock
	// on every BecomeLeader, BecomeFollower, and ObserveHigherTerm transition.
	// Used by Propose to detect stale-leader acknowledgements after stepdown.
	leaderEpoch uint64

	// proposeTestHook is an optional test injection point called inside Propose
	// AFTER the pre-append leadership check but BEFORE Storage.Append.
	// Allows deterministic stepdown injection to test the TOCTOU window (P16-SEC-F02).
	// Must be nil in production. Protected by proposeMu (only accessed under proposeMu).
	proposeTestHook func()

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

	// Volatile commit state (P15-S03-M03): highest log index known committed
	// (contiguous prefix 1..commitIndex). Never decreases; never exceeds the
	// local LastIndex; advanced only by the leader quorum rule. Survives role
	// transitions (a committed prefix stays committed).
	commitIndex LogIndex

	// State machine apply loop state (P16-S01-M01)
	stateMachine     StateMachine
	lastApplied      LogIndex
	applyBatchSize   int
	applyLifecycleMu sync.Mutex
	applyRunning     atomic.Bool
	applyNotifyCh    chan struct{}
	applyStopCh      chan struct{}
	applyWg          sync.WaitGroup
	applyErrMu       sync.RWMutex
	applyErr         error
	applyCtx         context.Context
	applyCancel      context.CancelFunc

	// Periodic heartbeat scheduler (P15-S02-M03)
	heartbeatInterval    time.Duration
	heartbeatLifecycleMu sync.Mutex
	heartbeatRunning     atomic.Bool
	heartbeatCancel      context.CancelFunc
	heartbeatDone        chan struct{}
	heartbeatWg          sync.WaitGroup
	heartbeatGen         uint64

	// Proposal/replication serialization (P15-S03-M01/M02): serializes
	// concurrent Propose calls and follower log-replication mutations so each
	// accepted proposal receives a distinct contiguous index and suffix
	// replacement cannot interleave with index allocation.
	// Always acquired before Node.mu (brief RLock); never held across
	// network I/O. Stepdown/Close paths never acquire it, so no inversion.
	proposeMu sync.Mutex
}

// NodeConfig provides initialization parameters for a Raft Node.
type NodeConfig struct {
	LocalID           cluster.NodeID
	Storage           *Storage
	TransitionHook    TransitionHook
	Topology          *cluster.Topology
	Peers             []cluster.NodeID
	PeerSender        PeerSender
	DurationProvider  DurationProvider
	HeartbeatInterval time.Duration
	StateMachine      StateMachine // Phase 16: optional StateMachine for applying committed entries
	ApplyBatchSize    int          // Phase 16: max entries to fetch/apply per batch (default 64)
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

	hbInterval := cfg.HeartbeatInterval
	if hbInterval <= 0 {
		hbInterval = DefaultHeartbeatInterval
	}

	ctx, cancel := context.WithCancel(context.Background())
	n := &Node{
		localID:           cfg.LocalID,
		storage:           cfg.Storage,
		role:              RoleFollower,
		leaderID:          cluster.NodeIDNil,
		transitionHook:    cfg.TransitionHook,
		topology:          cfg.Topology,
		peers:             remotePeers,
		peerSender:        cfg.PeerSender,
		electionTimer:     NewElectionTimer(cfg.DurationProvider),
		electionCtx:       ctx,
		electionCancel:    cancel,
		heartbeatInterval: hbInterval,
	}

	if cfg.StateMachine != nil {
		if err := n.StartApplyLoop(cfg.StateMachine, cfg.ApplyBatchSize); err != nil {
			_ = n.Close()
			return nil, err
		}
	}

	return n, nil
}

// osErrInvalid helper returns standard invalid argument error without extra imports
func osErrInvalid() error {
	return errors.ErrInvalidManagerConfig
}

// LocalID returns the local node ID.
func (n *Node) LocalID() cluster.NodeID {
	if n == nil {
		return cluster.NodeIDNil
	}
	return n.localID
}

// Role returns the currently active server role.
func (n *Node) Role() Role {
	if n == nil {
		return RoleFollower
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.role
}

// Term returns the current term from storage.
func (n *Node) Term() (Term, error) {
	if n == nil {
		return 0, errors.ErrNilReceiver
	}
	if n.closed.Load() {
		return 0, errors.ErrRaftStateClosed
	}
	return n.storage.Term()
}

// VotedFor returns candidate voted for in current term.
func (n *Node) VotedFor() (cluster.NodeID, error) {
	if n == nil {
		return cluster.NodeIDNil, errors.ErrNilReceiver
	}
	if n.closed.Load() {
		return cluster.NodeIDNil, errors.ErrRaftStateClosed
	}
	return n.storage.VotedFor()
}

// LeaderID returns the currently known leader node ID, or NodeIDNil if unknown.
func (n *Node) LeaderID() cluster.NodeID {
	if n == nil {
		return cluster.NodeIDNil
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.leaderID
}

// Storage returns the underlying persistent storage instance.
func (n *Node) Storage() *Storage {
	if n == nil {
		return nil
	}
	return n.storage
}

// QuorumSize returns the majority quorum size based on configured cluster membership.
// For N cluster members: quorum = floor(N/2) + 1.
func (n *Node) QuorumSize() int {
	if n == nil {
		return 0
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.quorumSizeLocked()
}

func (n *Node) quorumSizeLocked() int {
	if n == nil {
		return 0
	}
	clusterSize := len(n.peers) + 1
	return (clusterSize / 2) + 1
}

// GrantedVotesCount returns the number of granted votes recorded in the current election round.
// Returns 0 if not currently candidate or round is invalid.
func (n *Node) GrantedVotesCount() int {
	if n == nil {
		return 0
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	return len(n.electionVotes)
}

// CommitIndex returns the highest log index known committed (volatile state,
// P15-S03-M03). The committed prefix is always the contiguous range
// 1..commitIndex; 0 means nothing is committed yet.
func (n *Node) CommitIndex() LogIndex {
	if n == nil {
		return 0
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.commitIndex
}

// advanceCommitIndexLocked applies the Raft leader commitment rule for
// currTerm: the largest N > commitIndex such that a quorum of replication
// positions (leader LastIndex plus follower matchIndex) covers N and
// log[N].term == currTerm. The leader counts itself exactly when its own log
// contains N (the scan never exceeds LastIndex). Older-term entries are never
// a direct basis for advancement; they commit implicitly once a newer
// current-term entry commits over them. commitIndex never decreases.
// Caller MUST hold n.mu. Performs only in-memory storage reads (no I/O).
func (n *Node) advanceCommitIndexLocked(currTerm Term) {
	if n.role != RoleLeader {
		return
	}
	lastIdx, _, err := n.storage.LastIndexAndTerm()
	if err != nil || lastIdx <= n.commitIndex {
		return
	}
	quorum := n.quorumSizeLocked()
	for idx := lastIdx; idx > n.commitIndex; idx-- {
		t, err := n.storage.TermOf(idx)
		if err != nil {
			return
		}
		if t != currTerm {
			continue
		}
		count := 1 // Leader itself replicates every index it holds.
		for _, peerID := range n.peers {
			if peerID == n.localID {
				continue
			}
			if n.matchIndex[peerID] >= idx {
				count++
				if count >= quorum {
					break
				}
			}
		}
		if count >= quorum {
			n.commitIndex = idx
			n.signalApplyLocked()
			return
		}
	}
}

// refreshCommitIndex re-evaluates quorum commitment for the current leader
// session (used after local appends, where no follower response arrives —
// notably N=1 clusters, which commit immediately). No-op unless still leader.
// Never holds Node.mu across I/O.
func (n *Node) refreshCommitIndex() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed.Load() || n.role != RoleLeader {
		return
	}
	currTerm, err := n.storage.Term()
	if err != nil {
		return
	}
	n.advanceCommitIndexLocked(currTerm)
}

// isStillLeader reports whether the node is still Leader in the given term.
// Used to suppress stale one-time heartbeats after a concurrent stepdown.
// Checks role under Node.mu and durable term from storage without holding
// Node.mu across network I/O. Returns false if closed, not leader, storage
// unreadable, or term changed.
func (n *Node) isStillLeader(term Term) bool {
	if n.closed.Load() {
		return false
	}
	n.mu.RLock()
	isLeader := (n.role == RoleLeader)
	n.mu.RUnlock()
	if !isLeader {
		return false
	}
	currTerm, err := n.storage.Term()
	if err != nil {
		return false
	}
	return currTerm == term
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
	if oldRole == RoleLeader {
		n.leaderEpoch++ // Fence stale proposals if campaigning from leader
	}
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
// commitIndex is intentionally preserved: a committed prefix stays committed
// across leader sessions; only the current-term quorum rule may advance it.
// Caller MUST hold n.mu.
func (n *Node) becomeLeaderLocked(term Term, lastLogIdx LogIndex) {
	n.role = RoleLeader
	n.leaderID = n.localID
	n.leaderEpoch++ // P16-SEC-F02: fence stale proposals

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
// commitIndex is intentionally NOT cleared: commitment is monotonic and a
// committed prefix remains committed after stepdown.
// Caller MUST hold n.mu.
func (n *Node) clearLeaderAndElectionStateLocked() {
	n.electionVotes = nil
	n.electionRoundTerm = 0
	n.nextIndex = nil
	n.matchIndex = nil
}

// appendLeaderNoOpLocked durably appends the new leader's current-term no-op
// entry (transport.PeerEntryNoop, empty payload) at lastIdx+1 and returns the
// new last index. Every leadership acquisition mints exactly one such entry
// so quorum commitment can progress without waiting for client traffic: an
// inherited old-term prefix becomes committable once the no-op replicates,
// and N=1 clusters commit it immediately via the post-promotion refresh.
//
// Failure atomicity: called BEFORE becomeLeaderLocked while holding n.mu, so
// a persistence failure leaves the node Candidate (never a partially-promoted
// Leader); the next election timeout retries naturally. Disk I/O under
// Node.mu here is deliberate and precedented (campaignLocked persists
// HardState under mu for the same reason): leadership publication must be
// atomic with its durable prerequisites. No network I/O, no user callbacks.
// Caller MUST hold n.mu.
func (n *Node) appendLeaderNoOpLocked(currTerm Term, lastIdx LogIndex) (LogIndex, error) {
	if n.closed.Load() {
		return 0, errors.ErrRaftStateClosed
	}
	noOp := LogEntry{Index: lastIdx + 1, Term: currTerm, Type: transport.PeerEntryNoop}
	if err := noOp.Validate(); err != nil {
		return 0, err
	}
	if err := n.storage.Append(noOp); err != nil {
		return 0, fmt.Errorf("raft: failed to append leader no-op entry: %w", err)
	}
	return noOp.Index, nil
}

// BecomeLeader transitions the server from Candidate to Leader.
//
// Invariants enforced:
//   - Legal ONLY from RoleCandidate (Follower cannot directly become Leader).
//   - Does NOT increment term.
//   - Does NOT clear vote.
//   - Durably appends exactly one current-term no-op entry before publishing
//     leadership; promotion fails closed (stays Candidate) if persistence fails.
//   - Sets leaderID to localID.
//   - Idempotent if already RoleLeader (no duplicate no-op).
//   - Initializes volatile leader replication state (nextIndex/matchIndex)
//     over the post-no-op log.
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

	lastIdx, _, err := n.storage.LastIndexAndTerm()
	if err != nil {
		n.mu.Unlock()
		return err
	}

	oldRole := n.role
	newLastIdx, err := n.appendLeaderNoOpLocked(currTerm, lastIdx)
	if err != nil {
		n.mu.Unlock()
		return err
	}
	n.becomeLeaderLocked(currTerm, newLastIdx)
	shouldHook := (n.transitionHook != nil)
	n.mu.Unlock()

	if shouldHook && n.transitionHook != nil {
		n.transitionHook(oldRole, RoleLeader, currTerm)
	}

	// Guard against stale leadership actions: the transition hook or a
	// concurrent RPC may have stepped down this node before the one-time
	// heartbeat is emitted. Never hold Node.mu across network I/O; instead
	// re-validate leadership (role + durable term) before transmitting.
	// A stale heartbeat that still slips through the residual TOCTOU window
	// carries the old term and is rejected as stale by followers.
	if !n.isStillLeader(currTerm) {
		return nil
	}

	// New-term commitment can already cover the just-appended no-op (notably
	// N=1, where the leader alone is quorum) before the first heartbeat.
	n.refreshCommitIndex()

	// Broadcast one-time immediate empty AppendEntries heartbeats (without holding Node.mu)
	// anchored at the no-op: followers learn the leader's log head and pull
	// the suffix through the periodic replication rounds.
	n.sendImmediateHeartbeats(currTerm, newLastIdx, currTerm)

	// Start recurring periodic heartbeat scheduler (P15-S02-M03)
	n.startHeartbeatScheduler()

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
	n.leaderEpoch++ // P16-SEC-F02: fence stale proposals
	n.clearLeaderAndElectionStateLocked()

	shouldHook := (oldRole != RoleFollower || newTerm != currTerm)
	n.mu.Unlock()

	if shouldHook && n.transitionHook != nil {
		n.transitionHook(oldRole, RoleFollower, newTerm)
	}

	if oldRole == RoleLeader {
		n.stopHeartbeatScheduler()
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
	n.leaderEpoch++ // P16-SEC-F02: fence stale proposals
	n.clearLeaderAndElectionStateLocked()
	n.mu.Unlock()

	if n.transitionHook != nil {
		n.transitionHook(oldRole, RoleFollower, incomingTerm)
	}

	if oldRole == RoleLeader {
		n.stopHeartbeatScheduler()
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
	if oldRole == RoleLeader {
		n.leaderEpoch++ // Fence stale proposals on same-term stepdown
	}
	n.role = RoleFollower
	n.leaderID = leaderID
	n.clearLeaderAndElectionStateLocked()
	shouldHook := (oldRole != RoleFollower)
	n.mu.Unlock()

	if shouldHook && n.transitionHook != nil {
		n.transitionHook(oldRole, RoleFollower, currTerm)
	}

	if oldRole == RoleLeader {
		n.stopHeartbeatScheduler()
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

	// Stop periodic heartbeat scheduler and wait for goroutine termination (P15-S02-M03)
	n.stopHeartbeatScheduler()
	n.heartbeatWg.Wait()

	// Stop state machine apply loop and wait for worker termination (P16-S01-M01)
	n.stopApplyLoop()

	// Synchronize with any active state transition so Close cannot return while
	// an in-flight transition is mutating Node state (Section 7).
	n.mu.Lock()
	if n.role == RoleLeader {
		n.leaderEpoch++ // Fence stale proposals on node shutdown
	}
	n.mu.Unlock()

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
		lastIdx, _, err := n.storage.LastIndexAndTerm()
		if err != nil {
			n.mu.Unlock()
			return
		}
		oldRole := n.role
		newLastIdx, err := n.appendLeaderNoOpLocked(currTerm, lastIdx)
		if err != nil {
			n.mu.Unlock()
			return
		}
		n.becomeLeaderLocked(currTerm, newLastIdx)
		shouldHook := (n.transitionHook != nil)
		n.mu.Unlock()

		if shouldHook && n.transitionHook != nil {
			n.transitionHook(oldRole, RoleLeader, currTerm)
		}
		// Same stale-leadership guard as BecomeLeader: a concurrent
		// higher-term observation may have invalidated leadership during
		// the hook. Suppress the one-time heartbeat in that case.
		if !n.isStillLeader(currTerm) {
			return
		}
		// N=1 commits the no-op immediately (leader alone is quorum).
		n.refreshCommitIndex()
		n.sendImmediateHeartbeats(currTerm, newLastIdx, currTerm)
		n.startHeartbeatScheduler()
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
		steppedDownLeader  = false
	)
	defer func() {
		if steppedDownLeader {
			n.stopHeartbeatScheduler()
		}
	}()

	// Section 30, 40: Higher Term
	if req.Term > uint64(currTerm) {
		stepDownTargetTerm = Term(req.Term)
		// Persist higher term and clear vote on disk
		if err := n.storage.SetTerm(stepDownTargetTerm); err != nil {
			n.mu.Unlock()
			return nil, fmt.Errorf("raft: failed to persist higher term %d on RequestVote: %w", req.Term, err)
		}

		if oldRole == RoleLeader {
			n.leaderEpoch++ // Fence stale proposals on higher-term vote request
			steppedDownLeader = true
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

// sendHeartbeats broadcasts an empty AppendEntries frame to each configured remote peer.
// Invariants enforced (P15-S02-M03 / Sections 3, 9, 10, 11):
//   - Must execute strictly outside of Node.mu (no mutex held during network I/O).
//   - Uses specified term and localID as LeaderID.
//   - Entries is nil (empty heartbeat).
//   - Includes fresh cryptographic nonce and monotonically increasing sequence ID per peer.
//   - Never targets self.
//   - Tolerates peer send errors without terminating caller.
//   - Aborts remaining sends if leadership is lost mid-broadcast (stale-leadership guard).
//   - LeaderCommit carries the current commit index (followers ignore it in
//     M03; it becomes meaningful in Phase 16).
func (n *Node) sendHeartbeats(ctx context.Context, term Term, lastIdx LogIndex, lastTerm Term) {
	if n.peerSender == nil || len(n.peers) == 0 {
		return
	}

	sendCtx := ctx
	if sendCtx == nil {
		sendCtx = n.electionCtx
	}

	leaderCommit := uint64(n.CommitIndex())

	for _, peerID := range n.peers {
		if sendCtx.Err() != nil {
			return
		}
		// Stale-leadership guard: abort broadcast if this node is no longer
		// Leader in the heartbeat term (concurrent stepdown). Residual
		// TOCTOU between this check and Send is harmless: any stale frame
		// carries the old term and is rejected as stale by followers.
		if !n.isStillLeader(term) {
			return
		}
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
			LeaderCommit: leaderCommit,
			Nonce:        nonce,
			Entries:      nil, // Empty heartbeat
		}

		frame, err := transport.EncodeAppendEntries(req, seqID)
		if err != nil {
			continue
		}

		_ = n.peerSender.Send(sendCtx, peerID, frame)
	}
}

// sendImmediateHeartbeats broadcasts a one-time empty AppendEntries frame to each configured remote peer.
// Invariants enforced (P15-S02-M02 / Section 15):
//   - Executes immediately upon leadership acquisition at T=0.
//   - Reuses sendHeartbeats under the election context.
func (n *Node) sendImmediateHeartbeats(term Term, lastIdx LogIndex, lastTerm Term) {
	n.sendHeartbeats(n.electionCtx, term, lastIdx, lastTerm)
}

// replicateToFollowers transmits the periodic replication round (P15-S03-M03):
// for each configured remote peer, sends the pending log suffix starting at
// nextIndex[peer], or an empty heartbeat when the follower is caught up
// (nextIndex > LastIndex). Follower responses drive matchIndex/nextIndex and
// quorum commitment via HandleAppendEntriesResponse; this function only sends.
//
// Invariants (inherited from sendHeartbeats plus replication scope):
//   - Executes strictly outside of Node.mu and lifecycle mutexes (no mutex
//     held during network I/O or storage reads beyond their own locks).
//   - Per-peer leadership/lifecycle abort via isStillLeader(term).
//   - PrevLog coordinates are coherent by construction: (0,0) sentinel or a
//     stored term at prevIndex. Suffix reads come from durable storage
//     clones; payloads/terms are never synthesized or altered.
//   - Suffix length is size-aware batched (≤MaxPeerEntries and ≤MaxPayloadLength
//     encoded bytes, ≥1 entry per round): every round makes progress and memory
//     stays bounded by one frame. An unencodable round degrades to an empty
//     heartbeat for that peer (liveness preserved, entries retried next round).
//   - Never targets self or unknown peers (peers list is fixed at startup).
func (n *Node) replicateToFollowers(ctx context.Context, term Term) {
	if n.peerSender == nil || len(n.peers) == 0 {
		return
	}

	sendCtx := ctx
	if sendCtx == nil {
		sendCtx = n.electionCtx
	}

	leaderCommit := uint64(n.CommitIndex())

	for _, peerID := range n.peers {
		if sendCtx.Err() != nil {
			return
		}
		if !n.isStillLeader(term) {
			return
		}
		if peerID == n.localID {
			continue // Never send to self
		}

		n.mu.RLock()
		isLeader := (n.role == RoleLeader)
		nextIdx, ok := n.nextIndex[peerID]
		n.mu.RUnlock()
		if !isLeader || n.closed.Load() {
			return
		}
		if !ok {
			continue // Unknown peer snapshot: skip this round.
		}

		n.sendReplicationTo(sendCtx, term, peerID, nextIdx, leaderCommit)
	}
}

// sendReplicationTo sends one peer's replication round: pending suffix or
// empty heartbeat. No Node.mu or lifecycle mutex held; storage provides its
// own locking. Failures for one peer never affect others.
func (n *Node) sendReplicationTo(sendCtx context.Context, term Term, peerID cluster.NodeID, nextIdx LogIndex, leaderCommit uint64) {
	lastIdx, _, err := n.storage.LastIndexAndTerm()
	if err != nil || n.closed.Load() {
		return
	}

	// Caught up (or follower ahead): empty heartbeat anchored at our tail.
	if nextIdx > lastIdx {
		var prevTerm Term
		if lastIdx > 0 {
			prevTerm, err = n.storage.TermOf(lastIdx)
			if err != nil {
				return
			}
		}
		n.sendHeartbeatTo(sendCtx, term, peerID, lastIdx, prevTerm, leaderCommit)
		return
	}

	// Pending suffix: prev = nextIdx-1 with its stored term, then a
	// size-aware batch of the following entries.
	var prevTerm Term
	if nextIdx > 1 {
		prevTerm, err = n.storage.TermOf(nextIdx - 1)
		if err != nil {
			return
		}
	}
	wire := n.buildReplicationBatch(nextIdx, lastIdx)
	if len(wire) == 0 {
		// No encodable entry this round (storage hiccup between reads):
		// preserve liveness with an empty heartbeat; retry next round.
		var tailTerm Term
		if lastIdx > 0 {
			if tailTerm, err = n.storage.TermOf(lastIdx); err != nil {
				return
			}
		}
		n.sendHeartbeatTo(sendCtx, term, peerID, lastIdx, tailTerm, leaderCommit)
		return
	}

	nonce, err := transport.GenerateNonce()
	if err != nil {
		return
	}
	req := &transport.AppendEntriesRequest{
		Term:         uint64(term),
		LeaderID:     n.localID,
		PrevLogIndex: uint64(nextIdx - 1),
		PrevLogTerm:  uint64(prevTerm),
		LeaderCommit: leaderCommit,
		Nonce:        nonce,
		Entries:      wire,
	}
	frame, err := transport.EncodeAppendEntries(req, n.peerSender.NextSeqID())
	if err != nil {
		// Defensive: the batch was pre-bounded, so encoding is expected to
		// succeed. Degrade to an empty heartbeat rather than stalling.
		var tailTerm Term
		if lastIdx > 0 {
			if tailTerm, err = n.storage.TermOf(lastIdx); err != nil {
				return
			}
		}
		n.sendHeartbeatTo(sendCtx, term, peerID, lastIdx, tailTerm, leaderCommit)
		return
	}
	_ = n.peerSender.Send(sendCtx, peerID, frame)
}

// buildReplicationBatch assembles the largest legal AppendEntries batch starting
// at nextIdx (up to lastIdx): at most transport.MaxPeerEntries entries and at
// most transport.MaxPayloadLength encoded bytes including framing headers.
// The first entry is always included: a single entry can never exceed the
// frame limit because MaxLogEntryDataSize (4 MiB) plus its 13-byte entry
// header and the 52-byte request header total well under 5 MiB. Later entries
// stop the batch before the limit is exceeded, so every round makes progress
// (≥1 entry whenever the suffix is non-empty) and memory stays bounded by one
// frame. Stored terms, types, payloads, and ordering pass through unmodified.
// No Node.mu or lifecycle mutex held; storage provides its own locking.
func (n *Node) buildReplicationBatch(nextIdx, lastIdx LogIndex) []transport.PeerLogEntry {
	var wire []transport.PeerLogEntry
	total := uint64(transport.AppendEntriesRequestHeaderSize)
	for idx := nextIdx; idx <= lastIdx; idx++ {
		if len(wire) >= transport.MaxPeerEntries {
			break
		}
		e, err := n.storage.Entry(idx)
		if err != nil {
			break
		}
		// Both operands are bounded small (headers + ≤4 MiB data), so the
		// sum cannot overflow uint64; the comparison below is exact.
		sz := uint64(transport.PeerLogEntryHeaderSize) + uint64(len(e.Data))
		if len(wire) > 0 && total+sz > uint64(transport.MaxPayloadLength) {
			break
		}
		total += sz
		wire = append(wire, transport.PeerLogEntry{Term: uint64(e.Term), Type: e.Type, Data: e.Data})
	}
	return wire
}

// sendHeartbeatTo transmits a single empty AppendEntries heartbeat.
// No Node.mu or lifecycle mutex held.
func (n *Node) sendHeartbeatTo(sendCtx context.Context, term Term, peerID cluster.NodeID, lastIdx LogIndex, lastTerm Term, leaderCommit uint64) {
	nonce, err := transport.GenerateNonce()
	if err != nil {
		return
	}
	req := &transport.AppendEntriesRequest{
		Term:         uint64(term),
		LeaderID:     n.localID,
		PrevLogIndex: uint64(lastIdx),
		PrevLogTerm:  uint64(lastTerm),
		LeaderCommit: leaderCommit,
		Nonce:        nonce,
		Entries:      nil,
	}
	frame, err := transport.EncodeAppendEntries(req, n.peerSender.NextSeqID())
	if err != nil {
		return
	}
	_ = n.peerSender.Send(sendCtx, peerID, frame)
}

// HeartbeatRunning returns true if the periodic heartbeat scheduler is currently running.
func (n *Node) HeartbeatRunning() bool {
	return n.heartbeatRunning.Load()
}

// startHeartbeatScheduler starts the periodic heartbeat scheduler if node is leader.
// Idempotent: at most one active heartbeat scheduler goroutine may run per Node.
func (n *Node) startHeartbeatScheduler() {
	if n.closed.Load() {
		return
	}

	n.heartbeatLifecycleMu.Lock()
	defer n.heartbeatLifecycleMu.Unlock()

	if n.closed.Load() {
		return
	}

	n.mu.RLock()
	isLeader := (n.role == RoleLeader)
	n.mu.RUnlock()
	if !isLeader {
		return
	}

	if n.heartbeatRunning.Load() {
		return // Idempotent: at most one active scheduler
	}

	n.heartbeatGen++
	gen := n.heartbeatGen

	ctx, cancel := context.WithCancel(n.electionCtx)
	done := make(chan struct{})
	n.heartbeatCancel = cancel
	n.heartbeatDone = done
	n.heartbeatRunning.Store(true)
	n.heartbeatWg.Add(1)

	go n.runHeartbeatLoop(ctx, done, gen)
}

// stopHeartbeatScheduler halts the periodic heartbeat scheduler. Idempotent.
// Blocks until the active scheduler goroutine terminates cleanly.
// MUST NOT be called while holding Node.mu.
func (n *Node) stopHeartbeatScheduler() {
	n.heartbeatLifecycleMu.Lock()
	if !n.heartbeatRunning.Load() {
		n.heartbeatLifecycleMu.Unlock()
		return
	}

	n.heartbeatRunning.Store(false)
	if n.heartbeatCancel != nil {
		n.heartbeatCancel()
		n.heartbeatCancel = nil
	}
	done := n.heartbeatDone
	n.heartbeatDone = nil
	n.heartbeatLifecycleMu.Unlock()

	if done != nil {
		<-done
	}
}

// runHeartbeatLoop transmits the periodic replication round at configured cadence (50ms):
// pending log suffix per follower via nextIndex, empty AppendEntries heartbeat
// when caught up (P15-S02-M03 heartbeat, P15-S03-M03 replication).
// Stale-scheduler protection: each scheduler session carries the generation
// captured at start. Any lifecycle transition (stepdown, restart, Close)
// either clears heartbeatRunning or bumps heartbeatGen, causing stale
// sessions to exit without transmitting. A final generation + leadership
// re-check immediately before network I/O narrows the overlap window for
// rapid Leader -> Follower -> Leader restarts: at most the pre-existing
// TOCTOU between the final check and Send remains, and any stale frame that
// slips through carries an old term rejected as stale by followers.
func (n *Node) runHeartbeatLoop(ctx context.Context, done chan struct{}, gen uint64) {
	defer func() {
		n.heartbeatWg.Done()
		close(done)
	}()

	ticker := time.NewTicker(n.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if ctx.Err() != nil || n.closed.Load() || !n.heartbeatRunning.Load() {
			return
		}

		n.heartbeatLifecycleMu.Lock()
		curGen := n.heartbeatGen
		isRunning := n.heartbeatRunning.Load()
		n.heartbeatLifecycleMu.Unlock()
		if !isRunning || curGen != gen || n.closed.Load() {
			return
		}

		n.mu.RLock()
		isLeader := (n.role == RoleLeader)
		n.mu.RUnlock()
		if !isLeader || n.closed.Load() {
			return
		}

		term, err := n.Term()
		if err != nil || n.closed.Load() {
			return
		}

		// Double-check leadership and lifecycle state before network I/O
		n.mu.RLock()
		isLeader = (n.role == RoleLeader)
		n.mu.RUnlock()
		if !isLeader || n.closed.Load() {
			return
		}

		// Final stale-session gate: re-validate generation under the
		// lifecycle mutex and leadership (role + term) immediately before
		// transmitting. replicateToFollowers performs a last per-peer
		// isStillLeader check before each send.
		n.heartbeatLifecycleMu.Lock()
		curGen2 := n.heartbeatGen
		isRunning2 := n.heartbeatRunning.Load()
		n.heartbeatLifecycleMu.Unlock()
		if !isRunning2 || curGen2 != gen || n.closed.Load() {
			return
		}
		if !n.isStillLeader(term) {
			return
		}

		// Periodic replication round: pending suffix per follower based on
		// nextIndex, empty heartbeat when caught up (P15-S03-M03).
		n.replicateToFollowers(ctx, term)
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
		if oldRole == RoleLeader {
			n.leaderEpoch++ // Fence stale proposals on higher-term vote response
		}
		n.role = RoleFollower
		n.leaderID = cluster.NodeIDNil
		n.clearLeaderAndElectionStateLocked()
		n.mu.Unlock()

		if n.transitionHook != nil {
			n.transitionHook(oldRole, RoleFollower, newTerm)
		}
		if oldRole == RoleLeader {
			n.stopHeartbeatScheduler()
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

	// Read-before-mutate ordering: fetch leadership initialization state
	// BEFORE inserting the vote. If the storage read fails, the vote is not
	// recorded, preserving a valid retry path for the same peer. Inserting
	// first and then failing would permanently poison this round with a
	// phantom duplicate entry.
	lastIdx, _, err := n.storage.LastIndexAndTerm()
	if err != nil {
		n.mu.Unlock()
		return fmt.Errorf("raft: failed to read last index and term on leadership transition: %w", err)
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

	newLastIdx, err := n.appendLeaderNoOpLocked(currTerm, lastIdx)
	if err != nil {
		n.mu.Unlock()
		return fmt.Errorf("raft: failed to append leader no-op on quorum promotion: %w", err)
	}
	n.becomeLeaderLocked(currTerm, newLastIdx)
	shouldHook := (n.transitionHook != nil)
	n.mu.Unlock()

	if shouldHook && n.transitionHook != nil {
		n.transitionHook(oldRole, RoleLeader, currTerm)
	}

	// Stale-leadership guard (see BecomeLeader): suppress the one-time
	// heartbeat if leadership was invalidated during the hook.
	if !n.isStillLeader(currTerm) {
		return nil
	}

	// Commit the freshly minted no-op if quorum already covers it.
	n.refreshCommitIndex()

	// Section 15: Send one-time immediate empty AppendEntries heartbeats (no Node.mu held)
	n.sendImmediateHeartbeats(currTerm, newLastIdx, currTerm)

	// Start recurring periodic heartbeat scheduler (P15-S02-M03)
	n.startHeartbeatScheduler()

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

	case transport.PeerOpAppendEntries:
		req, err := transport.DecodeAppendEntries(frame)
		if err != nil {
			return err
		}

		resp, err := n.HandleAppendEntries(fromPeerID, req)
		if err != nil {
			return err
		}

		if n.peerSender != nil && fromPeerID.IsValid() && fromPeerID != n.localID {
			seqID := n.peerSender.NextSeqID()
			respFrame, encErr := transport.EncodeAppendEntriesResponse(resp, seqID)
			if encErr == nil {
				_ = n.peerSender.Send(n.electionCtx, fromPeerID, respFrame)
			}
		}
		return nil

	case transport.PeerOpAppendEntriesResponse:
		resp, err := transport.DecodeAppendEntriesResponse(frame)
		if err != nil {
			return err
		}
		return n.HandleAppendEntriesResponse(fromPeerID, resp)

	default:
		return nil
	}
}

// HandleAppendEntries processes an incoming AppendEntries RPC from a peer.
//
// Invariants enforced (P15-S02-M03 / Sections 8, 9, 10, 13, 14, 15, 16):
//  1. Rejects if node is closed.
//  2. If req == nil: ErrNilReceiver.
//  3. Identity Binding (Section 8): fromPeerID must match req.LeaderID.
//  4. Self Rejection (Section 8): remote requests from or for self are rejected.
//  5. Topology Boundary (Section 8): unknown peers are rejected fail-closed.
//  6. Invariant check: PrevLogIndex > 0 with PrevLogTerm == 0 is rejected.
//  7. Stale Term (Section 13, 24): If req.Term < currentTerm, returns Success=false, Term=currentTerm.
//     No election timer reset, no leaderID change, no role change.
//  8. Higher Term (Section 13, 25, 26): If req.Term > currentTerm, durably persists new term, clears vote,
//     and steps down to RoleFollower BEFORE publishing new term. Persistence failure fails closed.
//  9. Same Term (Section 13, 14, 27, 28): If req.Term == currentTerm, Candidate/Leader steps down to Follower.
//     Sets leaderID to authenticated sender.
//  10. Log Matching (Section 15, P15-S03-M02): If PrevLogIndex > 0, checks that
//     local log contains an entry at PrevLogIndex matching PrevLogTerm. If
//     mismatch, returns Success=false but STILL resets the election timer:
//     liveness (valid leader signal) is distinct from log replication success.
//     A lagging follower must not time out while its legitimate leader is
//     heartbeating.
//  11. Follower Replication (P15-S03-M02): For non-empty requests with matching
//     PrevLog, incoming entries are validated, conflicting suffixes truncated,
//     and new entries durably appended before reporting Success=true with
//     MatchIndex = PrevLogIndex + len(Entries). See replicateEntries.
//  12. Timer Reset (Section 13, 15): Any non-stale AppendEntries from the
//     legitimate current-term leader resets the election timer strictly
//     outside of Node.mu, regardless of log-match outcome.
//  13. Response (Section 16): Returns AppendEntriesResponse{Term: currentTerm,
//     Success: true/false, MatchIndex: last replicated index (or PrevLogIndex
//     for empty heartbeats, 0 on failure)}.
func (n *Node) HandleAppendEntries(fromPeerID cluster.NodeID, req *transport.AppendEntriesRequest) (*transport.AppendEntriesResponse, error) {
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
	if !req.LeaderID.IsValid() {
		return nil, &errors.InvalidNodeIDError{NodeID: uint64(req.LeaderID), Reason: "leader ID in AppendEntries must be greater than zero"}
	}

	// Section 8: Transport Identity Binding
	if fromPeerID != req.LeaderID {
		return nil, fmt.Errorf("%w: sender peer ID %d does not match leader ID %d",
			errors.ErrRaftSenderMismatch, fromPeerID, req.LeaderID)
	}

	// Section 8: Self Request Rejection
	if fromPeerID == n.localID || req.LeaderID == n.localID {
		return nil, fmt.Errorf("%w: received remote AppendEntries for local node ID %d",
			errors.ErrRaftSelfVoteRPC, n.localID)
	}

	// Section 8: Unknown Peer Rejection
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

	// Invariant validation: check log coordinates
	if err := ValidateCandidateLogCoordinates(req.PrevLogIndex, req.PrevLogTerm); err != nil {
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

	// Section 13, 24: Stale Term (req.Term < currentTerm) -> reject fail-closed
	// No timer reset, no leaderID change, no role change
	if Term(req.Term) < currTerm {
		n.mu.Unlock()
		return &transport.AppendEntriesResponse{
			Term:       uint64(currTerm),
			Success:    false,
			MatchIndex: 0,
		}, nil
	}

	oldRole := n.role
	termAdvanced := false
	var steppedDownLeader bool

	// Section 13, 25: Higher Term (req.Term > currentTerm)
	if Term(req.Term) > currTerm {
		newTerm := Term(req.Term)
		// Durably persist higher term and clear vote BEFORE publishing volatile state
		if err := n.storage.SetTerm(newTerm); err != nil {
			n.mu.Unlock()
			return nil, fmt.Errorf("raft: failed to persist higher term %d on AppendEntries: %w", req.Term, err)
		}
		if oldRole == RoleLeader {
			n.leaderEpoch++ // Fence stale proposals on higher-term append entries
			steppedDownLeader = true
		}
		currTerm = newTerm
		termAdvanced = true
		n.role = RoleFollower
		n.leaderID = fromPeerID
		n.clearLeaderAndElectionStateLocked()
	} else {
		// Section 13, 14, 27, 28: Same Term (req.Term == currentTerm)
		if n.role == RoleCandidate {
			// Candidate steps down to Follower; preserve currentTerm and durable vote
			n.role = RoleFollower
			n.leaderID = fromPeerID
			n.clearLeaderAndElectionStateLocked()
		} else if n.role == RoleLeader {
			// Dual leader discovery in same term: step down to Follower
			n.leaderEpoch++ // Fence stale proposals on same-term leader discovery
			n.role = RoleFollower
			n.leaderID = fromPeerID
			n.clearLeaderAndElectionStateLocked()
			steppedDownLeader = true
		} else {
			// Already RoleFollower: record leaderID
			n.leaderID = fromPeerID
		}
	}

	roleChanged := (oldRole != n.role)
	n.mu.Unlock()

	// Post-mutex notifications & lifecycle adjustments
	if (roleChanged || termAdvanced) && n.transitionHook != nil {
		n.transitionHook(oldRole, RoleFollower, currTerm)
	}
	if steppedDownLeader {
		n.stopHeartbeatScheduler()
	}

	// Empty heartbeat path (no log mutation, no proposal serialization):
	// PrevLog match determines success; the election timer resets on any
	// non-stale leader signal regardless of match outcome.
	if len(req.Entries) == 0 {
		if !n.prevLogMatches(req.PrevLogIndex, req.PrevLogTerm) {
			// Section 15: Previous-log check failed. Return Success=false BUT
			// still reset the election timer: an otherwise-valid heartbeat from
			// the legitimate current-term leader proves liveness even when the
			// follower's log is lagging. Without this, a healthy-but-lagging
			// follower would spuriously time out and disrupt the cluster.
			n.ResetElectionTimer()
			return &transport.AppendEntriesResponse{
				Term:       uint64(currTerm),
				Success:    false,
				MatchIndex: 0,
			}, nil
		}

		// Section 13, 15: Preceding log matched (and term is valid current or higher).
		// Reset election timer! No storage writes for heartbeats.
		n.ResetElectionTimer()

		// Follower commit advancement (P16-S01-M01): advance commitIndex up to
		// min(LeaderCommit, PrevLogIndex). Preceding log was verified identical to leader.
		if req.LeaderCommit > 0 {
			n.mu.Lock()
			targetCommit := LogIndex(req.LeaderCommit)
			if targetCommit > LogIndex(req.PrevLogIndex) {
				targetCommit = LogIndex(req.PrevLogIndex)
			}
			if targetCommit > n.commitIndex {
				n.commitIndex = targetCommit
				n.signalApplyLocked()
			}
			n.mu.Unlock()
		}

		return &transport.AppendEntriesResponse{
			Term:       uint64(currTerm),
			Success:    true,
			MatchIndex: req.PrevLogIndex,
		}, nil
	}

	// Non-empty replication path (P15-S03-M02): validate entries purely first
	// (no shared state), then serialize against concurrent Propose/replication
	// writers via proposeMu for conflict detection and durable mutation.
	entries, ok := convertPeerEntries(req)
	if !ok {
		// Malformed entries from an otherwise-legitimate leader: replication
		// rejected (no mutation), liveness preserved via timer reset.
		n.ResetElectionTimer()
		return &transport.AppendEntriesResponse{
			Term:       uint64(currTerm),
			Success:    false,
			MatchIndex: 0,
		}, nil
	}

	matchIdx, ok := n.replicateEntries(req.PrevLogIndex, req.PrevLogTerm, entries)
	// Liveness holds for every non-stale legitimate-leader signal, including
	// mismatches and replication outcomes.
	n.ResetElectionTimer()
	if !ok {
		return &transport.AppendEntriesResponse{
			Term:       uint64(currTerm),
			Success:    false,
			MatchIndex: 0,
		}, nil
	}

	// Follower commit advancement (P16-S01-M01): advance commitIndex up to
	// min(LeaderCommit, matchIdx). Replicated entries are durable on disk.
	if req.LeaderCommit > 0 {
		n.mu.Lock()
		targetCommit := LogIndex(req.LeaderCommit)
		if targetCommit > matchIdx {
			targetCommit = matchIdx
		}
		if targetCommit > n.commitIndex {
			n.commitIndex = targetCommit
			n.signalApplyLocked()
		}
		n.mu.Unlock()
	}

	return &transport.AppendEntriesResponse{
		Term:       uint64(currTerm),
		Success:    true,
		MatchIndex: uint64(matchIdx),
	}, nil
}

// prevLogMatches reports whether the local log contains an entry at prevIndex
// with term prevTerm. The (0,0) sentinel always matches an empty prefix.
// Pure storage reads; no Node.mu held.
func (n *Node) prevLogMatches(prevIndex uint64, prevTerm uint64) bool {
	if prevIndex == 0 {
		return true
	}
	lastIdx, _, err := n.storage.LastIndexAndTerm()
	if err != nil || LogIndex(prevIndex) > lastIdx {
		return false
	}
	termAtPrev, err := n.storage.TermOf(LogIndex(prevIndex))
	return err == nil && termAtPrev == Term(prevTerm)
}

// convertPeerEntries translates wire entries into Raft log entries with
// positional indexes (first entry = PrevLogIndex+1; the wire carries no
// per-entry index, so contiguity holds by construction) and validates every
// entry against Raft structural invariants. Pure computation, no shared state.
// Returns ok=false for arithmetic overflow, zero entry terms, entries from a
// term beyond the leader's stated term, or any LogEntry.Validate failure
// (bad type, oversize data). Callers must not mutate the log on ok=false.
func convertPeerEntries(req *transport.AppendEntriesRequest) ([]LogEntry, bool) {
	if uint64(len(req.Entries)) > math.MaxUint64-req.PrevLogIndex {
		return nil, false
	}
	entries := make([]LogEntry, len(req.Entries))
	for i, pe := range req.Entries {
		if pe.Term == 0 || pe.Term > req.Term {
			return nil, false
		}
		e := LogEntry{
			Index: LogIndex(req.PrevLogIndex) + LogIndex(i) + 1,
			Term:  Term(pe.Term),
			Type:  pe.Type,
			Data:  pe.Data,
		}
		if err := e.Validate(); err != nil {
			return nil, false
		}
		entries[i] = e
	}
	return entries, true
}

// replicateEntries incorporates validated leader entries into the local log.
//
// Preconditions (checked by caller): req already passed identity/term handling
// (node is Follower, leaderID recorded) and entries are structurally valid
// with positional indexes from prevLogIndex+1.
//
// Behavior:
//   - PrevLog re-checked against fresh storage state: on mismatch, returns
//     ok=false without mutating anything.
//   - Walk entries against the local log: matching terms are kept as-is
//     (never rewritten); the first term conflict truncates the local suffix
//     at the conflicting index; entries beyond the local tail are appended.
//     A follower tail extending beyond the leader suffix is preserved (no
//     truncation merely because the request ends earlier).
//   - Success (ok=true) is returned only after the resulting state is
//     durably persisted. Truncate-then-append crash windows are benign: an
//     interrupted replacement leaves a valid durable prefix that the leader's
//     retry re-matches, so no false acknowledgement is possible.
//   - On any storage failure returns ok=false with no success claim; the log
//     remains structurally valid (Storage guarantees all-or-nothing per op).
//
// Locking: serialized via proposeMu (shared with Propose) so concurrent
// proposals and replications cannot interleave index allocation with suffix
// replacement. Node.mu is never held here (no role state is mutated); Storage
// provides its own locking. Lock order: proposeMu -> Storage.mu, consistent
// with Propose. MatchIndex returned is prevLogIndex + len(entries).
func (n *Node) replicateEntries(prevLogIndex uint64, prevLogTerm uint64, entries []LogEntry) (LogIndex, bool) {
	n.proposeMu.Lock()
	defer n.proposeMu.Unlock()

	if n.closed.Load() {
		return 0, false
	}

	// Fresh PrevLog verification: the caller checked this before
	// serialization, but a concurrent writer may have replaced the suffix
	// since. Mismatch here means no mutation and a retryable rejection.
	if prevLogIndex == 0 {
		if prevLogTerm != 0 {
			return 0, false
		}
	} else {
		lastIdx, _, err := n.storage.LastIndexAndTerm()
		if err != nil || LogIndex(prevLogIndex) > lastIdx {
			return 0, false
		}
		termAtPrev, err := n.storage.TermOf(LogIndex(prevLogIndex))
		if err != nil || termAtPrev != Term(prevLogTerm) {
			return 0, false
		}
	}

	lastIdx, _, err := n.storage.LastIndexAndTerm()
	if err != nil {
		return 0, false
	}

	// Walk entries against the local log.
	appendFrom := len(entries)
	for i, e := range entries {
		if e.Index > lastIdx {
			appendFrom = i
			break
		}
		localTerm, err := n.storage.TermOf(e.Index)
		if err != nil {
			return 0, false
		}
		if localTerm != e.Term {
			// Conflict: truncate the divergent suffix, then append this
			// entry and everything after it.
			if err := n.storage.TruncateSuffix(e.Index); err != nil {
				return 0, false
			}
			appendFrom = i
			lastIdx = e.Index - 1
			break
		}
	}

	if appendFrom == len(entries) {
		// Every supplied entry already matches (duplicate or prefix
		// delivery, possibly with extra local tail beyond — preserved).
		// No storage writes needed.
		return LogIndex(prevLogIndex) + LogIndex(len(entries)), true
	}

	// Durably persist the new suffix. Storage.Append re-validates contiguity
	// (entries[appendFrom].Index == lastIdx+1 by construction above) and
	// syncs before mutating memory; on failure the log is untouched.
	if err := n.storage.Append(entries[appendFrom:]...); err != nil {
		return 0, false
	}
	return LogIndex(prevLogIndex) + LogIndex(len(entries)), true
}

// HandleAppendEntriesResponse processes an incoming AppendEntries response from a peer.
// Invariants enforced (P15-S02-M03 / Section 17, extended P15-S03-M03):
//  1. Rejects if node is closed.
//  2. If resp == nil: ErrNilReceiver.
//  3. If sender is self or invalid: rejected.
//  4. Sender must belong to configured cluster membership (topology/peers),
//     consistent with HandleRequestVoteResponse. Unknown senders cannot inject
//     higher terms or mutate consensus state.
//  5. If resp.Term > currentTerm: durably steps down to Follower, halts heartbeat scheduler,
//     and re-arms election timer. Commitment state is preserved (monotonic).
//  6. Stale responses (resp.Term < currentTerm) and responses received while
//     not Leader are ignored without mutating replication or commit state.
//     Role + term jointly identify the leader session: a stepdown always
//     leaves RoleLeader (same-term stepdown) or advances the term, so an old
//     session's delayed response can never satisfy both checks at once.
//  7. Same-term leader responses update follower progress monotonically:
//     Success advances matchIndex (never regresses) and nextIndex, then
//     re-evaluates quorum commitment under the current-term rule; failure
//     steps nextIndex back by one (floor 1) without touching matchIndex
//     or commitIndex. A reported MatchIndex beyond the leader's own LastIndex
//     is implausible (honest followers only echo suffixes the leader sent,
//     and the leader never truncates its log): such progress is ignored
//     without mutating match/next/commit state.
//  8. No state-machine application, no client acknowledgement (Phase 16 scope).
func (n *Node) HandleAppendEntriesResponse(fromPeerID cluster.NodeID, resp *transport.AppendEntriesResponse) error {
	if n.closed.Load() {
		return errors.ErrRaftStateClosed
	}
	if resp == nil {
		return errors.ErrNilReceiver
	}
	if !fromPeerID.IsValid() {
		return &errors.InvalidNodeIDError{NodeID: uint64(fromPeerID), Reason: "sender peer ID must be greater than zero"}
	}
	if fromPeerID == n.localID {
		return fmt.Errorf("%w: received AppendEntries response from self", errors.ErrRaftSelfVoteRPC)
	}

	// Consistent authenticated-sender membership boundary: unknown peers must
	// not be able to inject higher terms or otherwise mutate consensus state.
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
		return err
	}

	if Term(resp.Term) > currTerm {
		newTerm := Term(resp.Term)
		if err := n.storage.SetTerm(newTerm); err != nil {
			n.mu.Unlock()
			return fmt.Errorf("raft: failed to persist higher term %d on append entries response: %w", resp.Term, err)
		}
		oldRole := n.role
		if oldRole == RoleLeader {
			n.leaderEpoch++ // Fence stale proposals on higher-term append entries response
		}
		n.role = RoleFollower
		n.leaderID = cluster.NodeIDNil
		n.clearLeaderAndElectionStateLocked()
		n.mu.Unlock()

		if n.transitionHook != nil {
			n.transitionHook(oldRole, RoleFollower, newTerm)
		}
		if oldRole == RoleLeader {
			n.stopHeartbeatScheduler()
		}
		n.ResetElectionTimer()
		return nil
	}

	// Stale response term: ignore without mutating replication/commit state.
	if Term(resp.Term) < currTerm {
		n.mu.Unlock()
		return nil
	}

	// Same term: only the current leader tracks replication progress.
	// Candidates and followers ignore follower responses (a candidate must
	// never advance commit state from another session's traffic).
	if n.role != RoleLeader {
		n.mu.Unlock()
		return nil
	}
	if n.matchIndex == nil || n.nextIndex == nil {
		n.mu.Unlock()
		return nil
	}

	if resp.Success {
		// Plausibility bound (crash-fault model): an honest follower only
		// reports entries drawn from suffixes this leader sent, which never
		// exceed the leader's own (never self-truncated) LastIndex, read here
		// under the same mutex as the update. Inflated progress is ignored
		// entirely so a faulty peer cannot manufacture quorum coverage for
		// data the leader does not possess.
		lastIdx, err := n.storage.LastIndex()
		if err != nil {
			n.mu.Unlock()
			return fmt.Errorf("raft: failed to read last index on response: %w", err)
		}
		if match := LogIndex(resp.MatchIndex); match <= lastIdx {
			// Monotonic progress: a delayed lower response must not regress an
			// already-established position; duplicates are harmless no-ops.
			if match > n.matchIndex[fromPeerID] {
				n.matchIndex[fromPeerID] = match
			}
			if m := n.matchIndex[fromPeerID]; m < math.MaxUint64 && m+1 > n.nextIndex[fromPeerID] {
				n.nextIndex[fromPeerID] = m + 1
			}
			n.advanceCommitIndexLocked(currTerm)
		}
	} else {
		// Log mismatch: step nextIndex back by one (floor 1) so the next
		// replication attempt uses an earlier PrevLog and converges. This
		// applies even right after a success: the failure refers to an older
		// request than the established position. matchIndex and commitIndex
		// are untouched by failures; a later success restores nextIndex.
		if next := n.nextIndex[fromPeerID]; next > 1 {
			n.nextIndex[fromPeerID] = next - 1
		}
	}

	n.mu.Unlock()
	return nil
}
