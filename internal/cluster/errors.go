package cluster

import (
	"github.com/silent-knight19/lattice/internal/errors"
)

// Domain sentinel errors for cluster identity and topology.
var (
	// ErrInvalidNodeID indicates that a cluster node identifier is 0 or unparseable.
	ErrInvalidNodeID = errors.ErrInvalidNodeID

	// ErrDuplicateNodeID indicates that multiple peers in a cluster topology share the same numeric node ID.
	ErrDuplicateNodeID = errors.ErrDuplicateNodeID

	// ErrDuplicatePeerAddress indicates that multiple peers in a cluster topology share the same network address.
	ErrDuplicatePeerAddress = errors.ErrDuplicatePeerAddress

	// ErrInvalidPeerAddress indicates that a peer network address is malformed or has an invalid port.
	ErrInvalidPeerAddress = errors.ErrInvalidPeerAddress

	// ErrClusterTooLarge indicates that the number of configured peers exceeds the maximum permitted ceiling.
	ErrClusterTooLarge = errors.ErrClusterTooLarge

	// ErrSelfNotFound indicates that the local node ID was neither declared in cluster peers nor had a local address provided.
	ErrSelfNotFound = errors.ErrSelfNotFound

	// ErrSelfAddressMismatch indicates that the local peer address does not match the address declared for the local node in cluster peers.
	ErrSelfAddressMismatch = errors.ErrSelfAddressMismatch

	// ErrWildcardAddress indicates that a wildcard IP (e.g. 0.0.0.0, ::) was provided where a specific peer target is required.
	ErrWildcardAddress = errors.ErrWildcardAddress

	// ErrEmptyTopology indicates that a cluster topology contains zero nodes.
	ErrEmptyTopology = errors.ErrEmptyTopology
)
