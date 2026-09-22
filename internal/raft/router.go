package raft

import (
	"context"
	stdErrors "errors"
	"fmt"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/transport"
)

// ProposalRouter intercepts client write requests (OpPut, OpDelete) and routes them to the
// Raft leader or returns leader redirection information (P16-S01-M02).
// Satisfies transport.ProposalRouter.
type ProposalRouter struct {
	node     *Node
	topology *cluster.Topology
}

// NewProposalRouter constructs a ProposalRouter bound to a Node and optional cluster Topology.
// If topology is nil, the router falls back to node.Topology().
func NewProposalRouter(node *Node, topology *cluster.Topology) *ProposalRouter {
	if topology == nil && node != nil {
		topology = node.Topology()
	}
	return &ProposalRouter{
		node:     node,
		topology: topology,
	}
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
	if req.OpCode != transport.OpPut && req.OpCode != transport.OpDelete {
		resp.Status = transport.StatusInvalidRequest
		resp.Message = fmt.Sprintf("unsupported operation for write router: 0x%02x", byte(req.OpCode))
		return resp, nil
	}

	// 2. Validate request bounds
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
	var op binary.OpType
	if req.OpCode == transport.OpPut {
		op = binary.OpTypePut
	} else {
		op = binary.OpTypeDelete
	}

	cmd := Command{
		Op:    op,
		Key:   req.Key,
		Value: req.Value,
	}
	cmdBytes, err := EncodeCommand(cmd)
	if err != nil {
		resp.Status = transport.StatusInvalidRequest
		resp.Message = "failed to encode command payload"
		return resp, nil
	}

	// Submit proposal to leader's durable Raft log
	_, err = n.Propose(cmdBytes)
	if err == nil {
		// Proposal accepted and durably persisted in leader's local Raft log (P16-S01-M02)
		resp.Status = transport.StatusOk
		return resp, nil
	}

	// Handle Propose error
	if stdErrors.Is(err, errors.ErrRaftStateClosed) {
		resp.Status = transport.StatusServerClosed
		resp.Message = "server is closed"
		return resp, nil
	}
	if stdErrors.Is(err, errors.ErrRaftInvalidRoleTransition) {
		// Leadership was lost concurrently during proposal; fall back to follower redirect
		return n.routeNonLeader(req, top)
	}
	if ctx.Err() != nil {
		resp.Status = transport.StatusThrottled
		resp.Message = "request timed out under proposal write"
		return resp, nil
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
