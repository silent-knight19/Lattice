package cluster

import (
	"fmt"
	"sort"

	"github.com/silent-knight19/lattice/internal/errors"
)

const (
	// MaxClusterSize is the maximum permitted number of nodes in a static cluster topology (256).
	// Raft consensus topologies typically operate with 3 to 7 nodes. 256 nodes provides ample
	// testing headroom while strictly bounding memory allocations against hostile configuration inputs.
	MaxClusterSize = 256
)

// Topology represents a validated, immutable cluster membership model.
//
// Guaranteed Invariants (P14-M01-INV-01 through INV-10):
//   - Local node ID is strictly greater than zero and mapped to LocalAddress.
//   - Every configured peer has a syntactically valid "host:port" endpoint.
//   - NodeID <-> Address mapping is strictly one-to-one (no duplicate IDs or endpoints).
//   - Peers are sorted deterministically in ascending NodeID order.
//   - RemotePeers strictly excludes self, sorted in ascending NodeID order.
//   - Topology is completely immutable after validation; all slice accessors return defensive copies.
//   - No network I/O or DNS lookups are performed during validation.
type Topology struct {
	localID      NodeID
	localAddress string
	peers        []Peer            // All peers including self, sorted deterministically by NodeID asc
	remotePeers  []Peer            // Remote peers strictly excluding self, sorted by NodeID asc
	peerMap      map[NodeID]Peer   // Fast ID -> Peer lookup
	addrMap      map[string]NodeID // Canonical Address -> ID lookup
}

// NewTopology constructs and validates an immutable cluster topology from raw configuration.
//
// Self-peer reconciliation semantics:
//   - If rawPeers explicitly declares the local node, its address must match localAddr (if localAddr is non-empty).
//   - If rawPeers omits the local node, localAddr must be provided; the local node is automatically synthesized.
//   - If rawPeers omits the local node and localAddr is empty, validation fails with ErrSelfNotFound.
func NewTopology(localID NodeID, localAddr string, rawPeers []PeerConfig) (*Topology, error) {
	if !localID.IsValid() {
		return nil, &errors.InvalidNodeIDError{
			NodeID: uint64(localID),
			Reason: "local node ID must be greater than zero",
		}
	}

	if len(rawPeers) > MaxClusterSize {
		return nil, &errors.ClusterTooLargeError{
			Count: len(rawPeers),
			Max:   MaxClusterSize,
		}
	}

	var canonicalLocalAddr string
	if localAddr != "" {
		var err error
		canonicalLocalAddr, err = ValidateAndCanonicalizeAddress(localAddr)
		if err != nil {
			return nil, fmt.Errorf("local peer address invalid: %w", err)
		}
	}

	seenIDs := make(map[NodeID]string, len(rawPeers)+1)
	seenAddrs := make(map[string]NodeID, len(rawPeers)+1)

	var selfInPeers bool
	var selfPeerAddr string

	for _, rp := range rawPeers {
		if !rp.ID.IsValid() {
			return nil, &errors.InvalidNodeIDError{
				NodeID: uint64(rp.ID),
				Reason: "peer node ID must be greater than zero",
			}
		}

		cAddr, err := ValidateAndCanonicalizeAddress(rp.Address)
		if err != nil {
			return nil, fmt.Errorf("peer %d address invalid: %w", rp.ID, err)
		}

		if prevAddr, exists := seenIDs[rp.ID]; exists {
			return nil, &errors.DuplicateNodeIDError{
				NodeID: uint64(rp.ID),
				Addr1:  prevAddr,
				Addr2:  cAddr,
			}
		}

		if prevID, exists := seenAddrs[cAddr]; exists {
			return nil, &errors.DuplicatePeerAddressError{
				Address: cAddr,
				Node1:   uint64(prevID),
				Node2:   uint64(rp.ID),
			}
		}

		seenIDs[rp.ID] = cAddr
		seenAddrs[cAddr] = rp.ID

		if rp.ID == localID {
			selfInPeers = true
			selfPeerAddr = cAddr
		}
	}

	// Self-peer reconciliation
	if selfInPeers {
		if canonicalLocalAddr != "" && canonicalLocalAddr != selfPeerAddr {
			return nil, fmt.Errorf("%w: local address %q conflicts with declared address %q for node %d",
				errors.ErrSelfAddressMismatch, canonicalLocalAddr, selfPeerAddr, localID)
		}
		if canonicalLocalAddr == "" {
			canonicalLocalAddr = selfPeerAddr
		}
	} else {
		// Self omitted from peers list: require localAddr to synthesize self
		if canonicalLocalAddr == "" {
			return nil, fmt.Errorf("%w: local node %d not found in cluster peers and no local peer address specified",
				errors.ErrSelfNotFound, localID)
		}

		if conflictingID, exists := seenAddrs[canonicalLocalAddr]; exists {
			return nil, &errors.DuplicatePeerAddressError{
				Address: canonicalLocalAddr,
				Node1:   uint64(conflictingID),
				Node2:   uint64(localID),
			}
		}

		if len(seenIDs) >= MaxClusterSize {
			return nil, &errors.ClusterTooLargeError{
				Count: len(seenIDs) + 1,
				Max:   MaxClusterSize,
			}
		}

		seenIDs[localID] = canonicalLocalAddr
		seenAddrs[canonicalLocalAddr] = localID
	}

	if len(seenIDs) == 0 {
		return nil, errors.ErrEmptyTopology
	}

	// Build deterministically ordered slices (ascending NodeID)
	peers := make([]Peer, 0, len(seenIDs))
	for id, addr := range seenIDs {
		peers = append(peers, Peer{ID: id, Address: addr})
	}
	sort.Slice(peers, func(i, j int) bool {
		return peers[i].ID < peers[j].ID
	})

	remotePeers := make([]Peer, 0, len(peers)-1)
	peerMap := make(map[NodeID]Peer, len(peers))
	for _, p := range peers {
		peerMap[p.ID] = p
		if p.ID != localID {
			remotePeers = append(remotePeers, p)
		}
	}

	return &Topology{
		localID:      localID,
		localAddress: canonicalLocalAddr,
		peers:        peers,
		remotePeers:  remotePeers,
		peerMap:      peerMap,
		addrMap:      seenAddrs,
	}, nil
}

// LocalID returns the local node identifier.
func (t *Topology) LocalID() NodeID {
	return t.localID
}

// LocalAddress returns the syntactically canonicalized endpoint for the local node.
func (t *Topology) LocalAddress() string {
	return t.localAddress
}

// Peers returns a defensively cloned slice of all cluster peers (including self),
// deterministically ordered by ascending NodeID.
func (t *Topology) Peers() []Peer {
	return append([]Peer(nil), t.peers...)
}

// RemotePeers returns a defensively cloned slice of remote peers (strictly excluding self),
// deterministically ordered by ascending NodeID.
func (t *Topology) RemotePeers() []Peer {
	return append([]Peer(nil), t.remotePeers...)
}

// Size returns the total count of nodes in the cluster topology.
func (t *Topology) Size() int {
	return len(t.peers)
}

// RemoteSize returns the count of remote peers (excluding self).
func (t *Topology) RemoteSize() int {
	return len(t.remotePeers)
}

// LookupPeer searches for a peer by its NodeID. Returns (Peer, true) if found.
func (t *Topology) LookupPeer(id NodeID) (Peer, bool) {
	p, ok := t.peerMap[id]
	return p, ok
}

// LookupByAddress searches for a peer by its network address.
// Automatically canonicalizes addr syntactically before lookup.
func (t *Topology) LookupByAddress(addr string) (Peer, bool) {
	cAddr, err := ValidateAndCanonicalizeAddress(addr)
	if err != nil {
		return Peer{}, false
	}
	id, ok := t.addrMap[cAddr]
	if !ok {
		return Peer{}, false
	}
	return t.peerMap[id], true
}

// Contains reports whether id is a registered member of the cluster topology.
func (t *Topology) Contains(id NodeID) bool {
	_, ok := t.peerMap[id]
	return ok
}

// IsSelf reports whether id matches the local node identifier.
func (t *Topology) IsSelf(id NodeID) bool {
	return id == t.localID
}
