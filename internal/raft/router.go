package raft

import (
	"context"
	stdErrors "errors"
	"fmt"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/metrics"
	"github.com/silent-knight19/lattice/internal/transport"
)

// ProposalRouter intercepts client write requests (OpPut, OpDelete) and read requests (OpGet)
// routing them to consensus or returning leader redirection information (P16-S01-M02, P17-S01-M02).
// Satisfies transport.ProposalRouter and transport.ReadRouter.
type ProposalRouter struct {
	node     *Node
	topology *cluster.Topology
	engine   transport.Engine
}

// NewProposalRouter constructs a ProposalRouter bound to a Node, optional cluster Topology, and optional Engine.
// If topology is nil, the router falls back to node.Topology().
func NewProposalRouter(node *Node, topology *cluster.Topology, engine ...transport.Engine) *ProposalRouter {
	if topology == nil && node != nil {
		topology = node.Topology()
	}
	var eng transport.Engine
	if len(engine) > 0 {
		eng = engine[0]
	}
	return &ProposalRouter{
		node:     node,
		topology: topology,
		engine:   eng,
	}
}

// Engine returns the configured storage engine.
func (r *ProposalRouter) Engine() transport.Engine {
	if r == nil {
		return nil
	}
	return r.engine
}

// RouteWrite intercepts client mutations, routing to consensus on the leader or returning
// leader redirection on followers and candidates (P16-S01-M02).
func (r *ProposalRouter) RouteWrite(ctx context.Context, req *transport.Request) (*transport.Response, error) {
	if r == nil || r.node == nil {
		if req == nil {
			return nil, errors.ErrNilReceiver
		}
		return &transport.Response{
			OpCode:  req.OpCode,
			Status:  transport.StatusServerClosed,
			SeqID:   req.SeqID,
			Message: "server is closed",
		}, nil
	}
	return r.node.routeWriteWithTopology(ctx, req, r.topology)
}

// RouteRead intercepts client read requests (OpGet), performing linearizable read verification on the
// leader (ReadIndex -> WaitForApplied -> ValidateLeadership -> engine.Get) or returning
// leader redirection on followers and candidates (P17-S01-M02).
func (r *ProposalRouter) RouteRead(ctx context.Context, req *transport.Request) (*transport.Response, error) {
	if r == nil || r.node == nil {
		if req == nil {
			return nil, errors.ErrNilReceiver
		}
		return &transport.Response{
			OpCode:  req.OpCode,
			Status:  transport.StatusServerClosed,
			SeqID:   req.SeqID,
			Message: "server is closed",
		}, nil
	}
	return r.node.routeReadWithTopology(ctx, req, r.topology, r.engine)
}

// Topology returns the configured cluster topology, or nil if none.
func (n *Node) Topology() *cluster.Topology {
	if n == nil {
		return nil
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.topology
}

// SetTopology updates the cluster topology associated with the node.
func (n *Node) SetTopology(t *cluster.Topology) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.topology = t
}

// RouteWrite implements transport.ProposalRouter directly on Node.
func (n *Node) RouteWrite(ctx context.Context, req *transport.Request) (*transport.Response, error) {
	if n == nil {
		if req == nil {
			return nil, errors.ErrNilReceiver
		}
		return &transport.Response{
			OpCode:  req.OpCode,
			Status:  transport.StatusServerClosed,
			SeqID:   req.SeqID,
			Message: "server is closed",
		}, nil
	}
	return n.routeWriteWithTopology(ctx, req, n.Topology())
}

// routeWriteWithTopology encapsulates authoritative write routing logic.
func (n *Node) routeWriteWithTopology(ctx context.Context, req *transport.Request, top *cluster.Topology) (*transport.Response, error) {
	if req == nil {
		return nil, errors.ErrNilReceiver
	}

	resp := &transport.Response{
		OpCode: req.OpCode,
		SeqID:  req.SeqID,
	}

	if n == nil || n.closed.Load() {
		resp.Status = transport.StatusServerClosed
		resp.Message = "server is closed"
		return resp, nil
	}

	// 1. Validate operation code
	if req.OpCode != transport.OpPut && req.OpCode != transport.OpDelete && req.OpCode != transport.OpBatch {
		resp.Status = transport.StatusInvalidRequest
		resp.Message = fmt.Sprintf("unsupported operation for write router: 0x%02x", byte(req.OpCode))
		return resp, nil
	}

	// 2. Validate request bounds
	if req.OpCode == transport.OpBatch {
		if len(req.Batch) == 0 {
			resp.Status = transport.StatusInvalidRequest
			resp.Message = "BATCH must contain at least one operation"
			return resp, nil
		}
		if len(req.Batch) > transport.MaxBatchOps {
			resp.Status = transport.StatusInvalidRequest
			resp.Message = fmt.Sprintf("BATCH operation count %d exceeds maximum %d", len(req.Batch), transport.MaxBatchOps)
			return resp, nil
		}
		for i, bOp := range req.Batch {
			if !bOp.Type.Valid() {
				resp.Status = transport.StatusInvalidRequest
				resp.Message = fmt.Sprintf("batch op %d has invalid type: 0x%02x", i, byte(bOp.Type))
				return resp, nil
			}
			if err := binary.ValidateKey(bOp.Key); err != nil {
				resp.Status = transport.StatusInvalidRequest
				resp.Message = fmt.Sprintf("batch op %d key: %v", i, err)
				return resp, nil
			}
			if bOp.Type == transport.BatchOpPut {
				if err := binary.ValidateValue(bOp.Value); err != nil {
					resp.Status = transport.StatusInvalidRequest
					resp.Message = fmt.Sprintf("batch op %d value: %v", i, err)
					return resp, nil
				}
			} else if len(bOp.Value) != 0 {
				resp.Status = transport.StatusInvalidRequest
				resp.Message = fmt.Sprintf("batch op %d: DELETE cannot contain non-empty value", i)
				return resp, nil
			}
		}
	} else {
		if err := binary.ValidateKey(req.Key); err != nil {
			resp.Status = transport.StatusInvalidRequest
			resp.Message = err.Error()
			return resp, nil
		}
		switch req.OpCode {
		case transport.OpPut:
			if err := binary.ValidateValue(req.Value); err != nil {
				resp.Status = transport.StatusInvalidRequest
				resp.Message = err.Error()
				return resp, nil
			}
		case transport.OpDelete:
			if len(req.Value) != 0 {
				resp.Status = transport.StatusInvalidRequest
				resp.Message = "DELETE command cannot contain non-empty value"
				return resp, nil
			}
		}
	}

	// Check context cancellation before proposal I/O
	if ctx.Err() != nil {
		resp.Status = transport.StatusThrottled
		resp.Message = "request timed out before proposal"
		return resp, nil
	}

	// 3. Inspect role
	role := n.Role()

	// 4. Case: Candidate (election in progress, no confirmed leader)
	if role == RoleCandidate {
		resp.Status = transport.StatusNotLeader
		resp.Message = "not leader: node is candidate, retry later"
		return resp, nil
	}

	// 5. Case: Follower (or non-leader role)
	if role != RoleLeader {
		return n.routeNonLeader(req, top)
	}

	// 6. Case: Leader -> Encode to canonical Raft command
	var cmd Command
	if req.OpCode == transport.OpBatch {
		cmdOps := make([]CommandOp, len(req.Batch))
		for i, op := range req.Batch {
			opType := binary.OpTypePut
			if op.Type == transport.BatchOpDelete {
				opType = binary.OpTypeDelete
			}
			cmdOps[i] = CommandOp{
				Op:    opType,
				Key:   op.Key,
				Value: op.Value,
			}
		}
		cmd = Command{
			Op:    binary.OpTypeBatch,
			Batch: cmdOps,
		}
	} else {
		var op binary.OpType
		if req.OpCode == transport.OpPut {
			op = binary.OpTypePut
		} else {
			op = binary.OpTypeDelete
		}
		cmd = Command{
			Op:    op,
			Key:   req.Key,
			Value: req.Value,
		}
	}

	cmdBytes, err := EncodeCommand(cmd)
	if err != nil {
		resp.Status = transport.StatusInvalidRequest
		resp.Message = "failed to encode command payload"
		return resp, nil
	}

	// Submit proposal to leader's durable Raft log (P16-SEC-F03: context-aware)
	start := time.Now()
	_, err = n.ProposeWithContext(ctx, cmdBytes)
	if err == nil {
		// Proposal accepted and durably persisted in leader's local Raft log (P16-S01-M02)
		opStr := "put"
		if req.OpCode == transport.OpDelete {
			opStr = "delete"
		} else if req.OpCode == transport.OpBatch {
			opStr = "batch"
		}
		metrics.RaftProposalLatency.WithLabelValues(opStr).ObserveDuration(time.Since(start))
		resp.Status = transport.StatusOk
		return resp, nil
	}

	// Handle Propose error
	if stdErrors.Is(err, errors.ErrRaftStateClosed) {
		resp.Status = transport.StatusServerClosed
		resp.Message = "server is closed"
		return resp, nil
	}

	// GAP B: Context cancellation or deadline expiration must be reported as StatusThrottled.
	// It MUST NOT be misclassified as leadership loss and must NEVER generate a leader redirect.
	if ctx.Err() != nil || stdErrors.Is(err, context.Canceled) || stdErrors.Is(err, context.DeadlineExceeded) {
		resp.Status = transport.StatusThrottled
		resp.Message = "request context cancelled or timed out under proposal write"
		return resp, nil
	}

	if stdErrors.Is(err, errors.ErrRaftInvalidRoleTransition) {
		// Leadership was lost concurrently during proposal; fall back to follower redirect
		return n.routeNonLeader(req, top)
	}

	// Sanitize general proposal error
	resp.Status = transport.StatusError
	resp.Message = "internal proposal error"
	return resp, nil
}

// routeNonLeader generates the appropriate redirection response for non-leader nodes.
func (n *Node) routeNonLeader(req *transport.Request, top *cluster.Topology) (*transport.Response, error) {
	resp := &transport.Response{
		OpCode: req.OpCode,
		SeqID:  req.SeqID,
	}

	if n.closed.Load() {
		resp.Status = transport.StatusServerClosed
		resp.Message = "server is closed"
		return resp, nil
	}

	n.mu.RLock()
	leaderID := n.leaderID
	localID := n.localID
	if top == nil {
		top = n.topology
	}
	n.mu.RUnlock()

	// 1. Leader unknown or nil
	if !leaderID.IsValid() || leaderID == cluster.NodeIDNil {
		resp.Status = transport.StatusNotLeader
		resp.Message = "not leader: leader is unknown, retry later"
		return resp, nil
	}

	// 2. Leader points to self, but node stepped down
	if leaderID == localID {
		resp.Status = transport.StatusNotLeader
		resp.Message = "not leader: stepped down, retry later"
		return resp, nil
	}

	// 3. Topology lookup
	if top == nil {
		resp.Status = transport.StatusError
		resp.Message = fmt.Sprintf("not leader: cluster topology unavailable for leader %d", leaderID)
		return resp, nil
	}

	peer, ok := top.LookupPeer(leaderID)
	if !ok {
		resp.Status = transport.StatusError
		resp.Message = fmt.Sprintf("not leader: leader node %d not found in topology", leaderID)
		return resp, nil
	}

	if peer.Address == "" {
		resp.Status = transport.StatusError
		resp.Message = fmt.Sprintf("not leader: leader node %d has no configured address", leaderID)
		return resp, nil
	}

	// 4. Valid leader address resolved from trusted topology
	resp.Status = transport.StatusNotLeader
	resp.Message = transport.FormatRedirectMessage(uint64(leaderID), peer.Address)
	resp.LeaderID = uint64(leaderID)
	resp.LeaderAddr = peer.Address
	return resp, nil
}

// RouteRead implements transport.ReadRouter directly on Node.
func (n *Node) RouteRead(ctx context.Context, req *transport.Request) (*transport.Response, error) {
	if n == nil {
		if req == nil {
			return nil, errors.ErrNilReceiver
		}
		return &transport.Response{
			OpCode:  req.OpCode,
			Status:  transport.StatusServerClosed,
			SeqID:   req.SeqID,
			Message: "server is closed",
		}, nil
	}
	var eng transport.Engine
	if e, ok := n.stateMachine.(transport.Engine); ok {
		eng = e
	}
	return n.routeReadWithTopology(ctx, req, n.Topology(), eng)
}

// routeReadWithTopology encapsulates authoritative linearizable read routing logic.
func (n *Node) routeReadWithTopology(ctx context.Context, req *transport.Request, top *cluster.Topology, eng transport.Engine) (*transport.Response, error) {
	if req == nil {
		return nil, errors.ErrNilReceiver
	}

	resp := &transport.Response{
		OpCode: req.OpCode,
		SeqID:  req.SeqID,
	}

	if n == nil || n.closed.Load() {
		resp.Status = transport.StatusServerClosed
		resp.Message = "server is closed"
		return resp, nil
	}

	// 1. Validate operation code
	if req.OpCode != transport.OpGet {
		resp.Status = transport.StatusInvalidRequest
		resp.Message = fmt.Sprintf("unsupported operation for read router: 0x%02x", byte(req.OpCode))
		return resp, nil
	}

	// 2. Validate request bounds
	if err := binary.ValidateKey(req.Key); err != nil {
		resp.Status = transport.StatusInvalidRequest
		resp.Message = err.Error()
		return resp, nil
	}

	// Check context cancellation before ReadIndex verification
	if ctx.Err() != nil {
		resp.Status = transport.StatusThrottled
		resp.Message = "request timed out before read index verification"
		return resp, nil
	}

	// 3. Inspect role
	role := n.Role()

	// 4. Case: Candidate (election in progress, no confirmed leader)
	if role == RoleCandidate {
		resp.Status = transport.StatusNotLeader
		resp.Message = "not leader: node is candidate, retry later"
		return resp, nil
	}

	// 5. Case: Follower (or non-leader role)
	if role != RoleLeader {
		return n.routeNonLeader(req, top)
	}

	// 6. Case: Leader -> Execute linearizable read sequence
	// Step A: ReadIndex quorum verification (P17-S01-M01)
	readStart := time.Now()
	readRes, err := n.ReadIndex(ctx)
	if err == nil {
		metrics.RaftReadIndexLatency.ObserveDuration(time.Since(readStart))
	}
	if err != nil {
		if n.closed.Load() || stdErrors.Is(err, errors.ErrRaftStateClosed) {
			resp.Status = transport.StatusServerClosed
			resp.Message = "server is closed"
			return resp, nil
		}
		if stdErrors.Is(err, errors.ErrReadIndexThrottled) {
			resp.Status = transport.StatusThrottled
			resp.Message = "read throttled under load"
			return resp, nil
		}
		if ctx.Err() != nil || stdErrors.Is(err, context.Canceled) || stdErrors.Is(err, context.DeadlineExceeded) {
			resp.Status = transport.StatusThrottled
			resp.Message = "request context cancelled or timed out under read index verification"
			return resp, nil
		}
		if stdErrors.Is(err, errors.ErrRaftInvalidRoleTransition) {
			// Leadership was lost concurrently during ReadIndex; fall back to follower redirect
			return n.routeNonLeader(req, top)
		}
		resp.Status = transport.StatusError
		resp.Message = "internal read index error"
		return resp, nil
	}

	// Step B: Wait for state machine to apply up to read index (P17-S01-M02)
	if err := n.WaitForApplied(ctx, readRes.Index); err != nil {
		if n.closed.Load() || stdErrors.Is(err, errors.ErrRaftStateClosed) {
			resp.Status = transport.StatusServerClosed
			resp.Message = "server is closed"
			return resp, nil
		}
		if ctx.Err() != nil || stdErrors.Is(err, context.Canceled) || stdErrors.Is(err, context.DeadlineExceeded) {
			resp.Status = transport.StatusThrottled
			resp.Message = "request context cancelled or timed out waiting for state machine barrier"
			return resp, nil
		}
		if stdErrors.Is(err, errors.ErrRaftApplyFailed) || stdErrors.Is(err, errors.ErrRaftCorruptedState) {
			resp.Status = transport.StatusError
			resp.Message = "state machine apply failed"
			return resp, nil
		}
		resp.Status = transport.StatusError
		resp.Message = "internal apply wait error"
		return resp, nil
	}

	// Step C: Revalidate leadership authority after barrier wait (Section 6)
	if err := n.ValidateLeadership(readRes.Term, readRes.Epoch); err != nil {
		// Leadership was lost during the barrier wait; fall back to follower redirect
		return n.routeNonLeader(req, top)
	}

	// Step D: Execute state machine read
	if eng == nil {
		resp.Status = transport.StatusError
		resp.Message = "storage engine unavailable for read router"
		return resp, nil
	}

	val, err := eng.Get(req.Key)
	if err == nil {
		if uint32(len(val)) > transport.MaxPayloadLength {
			resp.Status = transport.StatusError
			resp.Message = "response value exceeds maximum protocol frame limit"
			return resp, nil
		}
		resp.Status = transport.StatusOk
		resp.Value = val
		return resp, nil
	}

	if stdErrors.Is(err, errors.ErrKeyNotFound) {
		resp.Status = transport.StatusKeyNotFound
		return resp, nil
	}

	resp.Status = transport.StatusError
	resp.Message = "internal storage error"
	return resp, nil
}
