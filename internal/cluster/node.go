package cluster

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/silent-knight19/lattice/internal/errors"
)

// NodeID represents a unique 64-bit unsigned identifier for a cluster node.
// Aligns with canonical Raft consensus specifications and 8-byte big-endian primitives.
type NodeID uint64

const (
	// NodeIDNil represents an uninitialized or unset node identifier (0).
	// Value 0 is strictly reserved and prohibited as an active cluster node identity.
	NodeIDNil NodeID = 0
)

// String returns the decimal string representation of the NodeID.
func (id NodeID) String() string {
	return strconv.FormatUint(uint64(id), 10)
}

// IsValid reports whether the NodeID is a valid, non-reserved cluster identity (id > 0).
func (id NodeID) IsValid() bool {
	return id > NodeIDNil
}

// ParseNodeID parses a decimal string representation of a NodeID.
// Returns ErrInvalidNodeID if the string is empty, malformed, negative, or evaluates to 0.
func ParseNodeID(s string) (NodeID, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return NodeIDNil, &errors.InvalidNodeIDError{
			NodeID: 0,
			Reason: "node ID cannot be empty",
		}
	}
	v, err := strconv.ParseUint(trimmed, 10, 64)
	if err != nil {
		return NodeIDNil, &errors.InvalidNodeIDError{
			NodeID: 0,
			Reason: fmt.Sprintf("invalid numeric format %q: %v", trimmed, err),
		}
	}
	id := NodeID(v)
	if !id.IsValid() {
		return NodeIDNil, &errors.InvalidNodeIDError{
			NodeID: 0,
			Reason: "must be greater than zero",
		}
	}
	return id, nil
}
