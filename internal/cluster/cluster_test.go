package cluster_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/silent-knight19/lattice/internal/cluster"
)

// TestNodeID verifies NodeID string formatting, validity checks, and string parsing.
func TestNodeID(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantID  cluster.NodeID
		wantErr bool
	}{
		{"valid node 1", "1", 1, false},
		{"valid node 42", "42", 42, false},
		{"valid max uint64", "18446744073709551615", 18446744073709551615, false},
		{"empty string", "", 0, true},
		{"whitespace string", "   ", 0, true},
		{"zero ID prohibited", "0", 0, true},
		{"negative number", "-1", 0, true},
		{"non-numeric alpha", "node-1", 0, true},
		{"overflow", "18446744073709551616", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cluster.ParseNodeID(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseNodeID(%q) error = %v, wantErr = %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr {
				if got != tt.wantID {
					t.Errorf("ParseNodeID(%q) = %d, want %d", tt.input, got, tt.wantID)
				}
				if !got.IsValid() {
					t.Errorf("expected NodeID %d to be valid", got)
				}
				if got.String() != tt.input {
					t.Errorf("got.String() = %q, want %q", got.String(), tt.input)
				}
			} else {
				if got != cluster.NodeIDNil {
					t.Errorf("expected NodeIDNil on error, got %d", got)
				}
			}
		})
	}
}

// TestAddressValidation verifies structural and syntactic validation of peer endpoints.
func TestAddressValidation(t *testing.T) {
	tests := []struct {
		name      string
		addr      string
		wantCanon string
		wantErr   error
	}{
		{"valid ipv4", "127.0.0.1:9098", "127.0.0.1:9098", nil},
		{"valid remote ipv4", "10.0.0.10:9098", "10.0.0.10:9098", nil},
		{"valid ipv6 loopback", "[::1]:9098", "[::1]:9098", nil},
		{"valid ipv6 full", "[2001:db8::10]:9098", "[2001:db8::10]:9098", nil},
		{"valid hostname lowercase", "node1.internal:9098", "node1.internal:9098", nil},
		{"hostname normalized to lowercase", "NodeA.CLUSTER.Local:9098", "nodea.cluster.local:9098", nil},
		{"trailing dot normalized", "node1.internal.:9098", "node1.internal:9098", nil},
		{"max legal port", "10.0.0.1:65535", "10.0.0.1:65535", nil},
		{"min legal port", "10.0.0.1:1", "10.0.0.1:1", nil},

		{"empty address", "", "", cluster.ErrInvalidPeerAddress},
		{"missing port", "10.0.0.1", "", cluster.ErrInvalidPeerAddress},
		{"trailing colon missing port", "10.0.0.1:", "", cluster.ErrInvalidPeerAddress},
		{"empty host", ":9098", "", cluster.ErrInvalidPeerAddress},
		{"port 0 rejected", "10.0.0.1:0", "", cluster.ErrInvalidPeerAddress},
		{"negative port", "10.0.0.1:-1", "", cluster.ErrInvalidPeerAddress},
		{"port above max", "10.0.0.1:65536", "", cluster.ErrInvalidPeerAddress},
		{"non-numeric port", "10.0.0.1:abc", "", cluster.ErrInvalidPeerAddress},
		{"unbracketed ipv6", "::1:9098", "", cluster.ErrInvalidPeerAddress},
		{"malformed bracketed ipv6", "[::1:9098", "", cluster.ErrInvalidPeerAddress},
		{"wildcard ipv4 prohibited", "0.0.0.0:9098", "", cluster.ErrWildcardAddress},
		{"wildcard ipv6 prohibited", "[::]:9098", "", cluster.ErrWildcardAddress},
		{"hostname label starts with hyphen", "-node.example.com:9098", "", cluster.ErrInvalidPeerAddress},
		{"hostname label ends with hyphen", "node-.example.com:9098", "", cluster.ErrInvalidPeerAddress},
		{"hostname consecutive dots", "node..example.com:9098", "", cluster.ErrInvalidPeerAddress},
		{"hostname invalid char underscore", "node_bad.com:9098", "", cluster.ErrInvalidPeerAddress},
		{"hostname invalid char symbol", "node@evil.com:9098", "", cluster.ErrInvalidPeerAddress},
		{"hostname label exceeds 63 chars", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.com:9098", "", cluster.ErrInvalidPeerAddress},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cluster.ValidateAndCanonicalizeAddress(tt.addr)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("ValidateAndCanonicalizeAddress(%q) expected error, got nil", tt.addr)
				}
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("ValidateAndCanonicalizeAddress(%q) error = %v, want matching %v", tt.addr, err, tt.wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("ValidateAndCanonicalizeAddress(%q) unexpected error: %v", tt.addr, err)
				}
				if got != tt.wantCanon {
					t.Errorf("ValidateAndCanonicalizeAddress(%q) = %q, want %q", tt.addr, got, tt.wantCanon)
				}
			}
		})
	}
}

// TestParsePeersString verifies parsing delimited peer configuration strings.
func TestParsePeersString(t *testing.T) {
	t.Run("empty string returns nil", func(t *testing.T) {
		peers, err := cluster.ParsePeersString("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if peers != nil {
			t.Errorf("expected nil peers, got %v", peers)
		}
	})

	t.Run("comma-separated valid peers", func(t *testing.T) {
		input := "1=10.0.0.1:9098, 2=10.0.0.2:9098, 3=10.0.0.3:9098"
		peers, err := cluster.ParsePeersString(input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(peers) != 3 {
			t.Fatalf("expected 3 peers, got %d", len(peers))
		}
		if peers[0].ID != 1 || peers[0].Address != "10.0.0.1:9098" {
			t.Errorf("unexpected peer 0: %+v", peers[0])
		}
		if peers[1].ID != 2 || peers[1].Address != "10.0.0.2:9098" {
			t.Errorf("unexpected peer 1: %+v", peers[1])
		}
		if peers[2].ID != 3 || peers[2].Address != "10.0.0.3:9098" {
			t.Errorf("unexpected peer 2: %+v", peers[2])
		}
	})

	t.Run("semicolon and at separator", func(t *testing.T) {
		input := "1@10.0.0.1:9098; 2@10.0.0.2:9098"
		peers, err := cluster.ParsePeersString(input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(peers) != 2 {
			t.Fatalf("expected 2 peers, got %d", len(peers))
		}
		if peers[0].ID != 1 || peers[1].ID != 2 {
			t.Errorf("unexpected IDs: %+v", peers)
		}
	})

	t.Run("colon separator with host port", func(t *testing.T) {
		input := "1:10.0.0.1:9098\n2:10.0.0.2:9098"
		peers, err := cluster.ParsePeersString(input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(peers) != 2 {
			t.Fatalf("expected 2 peers, got %d", len(peers))
		}
	})

	t.Run("missing delimiter returns error", func(t *testing.T) {
		_, err := cluster.ParsePeersString("invalidpeerentry")
		if err == nil {
			t.Fatal("expected error on missing delimiter, got nil")
		}
		if !errors.Is(err, cluster.ErrInvalidPeerAddress) {
			t.Errorf("expected ErrInvalidPeerAddress, got %v", err)
		}
	})

	t.Run("invalid node id returns error", func(t *testing.T) {
		_, err := cluster.ParsePeersString("0=10.0.0.1:9098")
		if err == nil {
			t.Fatal("expected error on node ID 0, got nil")
		}
		if !errors.Is(err, cluster.ErrInvalidNodeID) {
			t.Errorf("expected ErrInvalidNodeID, got %v", err)
		}
	})

	t.Run("empty address returns error", func(t *testing.T) {
		_, err := cluster.ParsePeersString("1=")
		if err == nil {
			t.Fatal("expected error on empty address, got nil")
		}
		if !errors.Is(err, cluster.ErrInvalidPeerAddress) {
			t.Errorf("expected ErrInvalidPeerAddress, got %v", err)
		}
	})

	t.Run("exceeds MaxClusterSize returns error", func(t *testing.T) {
		var b strings.Builder
		for i := 1; i <= cluster.MaxClusterSize+1; i++ {
			fmt.Fprintf(&b, "%d=10.0.0.%d:9098,", i, i)
		}
		_, err := cluster.ParsePeersString(b.String())
		if err == nil {
			t.Fatal("expected error on oversized peer list, got nil")
		}
		if !errors.Is(err, cluster.ErrClusterTooLarge) {
			t.Errorf("expected ErrClusterTooLarge, got %v", err)
		}
	})
}

// TestTopologyValidation verifies comprehensive cluster topology validation scenarios.
func TestTopologyValidation(t *testing.T) {
	t.Run("single node cluster with local address synthesis", func(t *testing.T) {
		topo, err := cluster.NewTopology(1, "127.0.0.1:9098", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if topo.LocalID() != 1 {
			t.Errorf("LocalID() = %d, want 1", topo.LocalID())
		}
		if topo.LocalAddress() != "127.0.0.1:9098" {
			t.Errorf("LocalAddress() = %q, want 127.0.0.1:9098", topo.LocalAddress())
		}
		if topo.Size() != 1 {
			t.Errorf("Size() = %d, want 1", topo.Size())
		}
		if topo.RemoteSize() != 0 {
			t.Errorf("RemoteSize() = %d, want 0", topo.RemoteSize())
		}
		if len(topo.RemotePeers()) != 0 {
			t.Errorf("expected 0 remote peers, got %v", topo.RemotePeers())
		}
	})

	t.Run("3-node symmetric cluster where self is included in peers", func(t *testing.T) {
		raw := []cluster.PeerConfig{
			{ID: 1, Address: "10.0.0.1:9098"},
			{ID: 2, Address: "10.0.0.2:9098"},
			{ID: 3, Address: "10.0.0.3:9098"},
		}
		topo, err := cluster.NewTopology(2, "10.0.0.2:9098", raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if topo.LocalID() != 2 {
			t.Errorf("LocalID() = %d, want 2", topo.LocalID())
		}
		if topo.LocalAddress() != "10.0.0.2:9098" {
			t.Errorf("LocalAddress() = %q, want 10.0.0.2:9098", topo.LocalAddress())
		}
		if topo.Size() != 3 {
			t.Errorf("Size() = %d, want 3", topo.Size())
		}
		if topo.RemoteSize() != 2 {
			t.Errorf("RemoteSize() = %d, want 2", topo.RemoteSize())
		}

		remote := topo.RemotePeers()
		if len(remote) != 2 {
			t.Fatalf("expected 2 remote peers, got %d", len(remote))
		}
		if remote[0].ID != 1 || remote[1].ID != 3 {
			t.Errorf("expected remote peers [1, 3], got [%d, %d]", remote[0].ID, remote[1].ID)
		}
	})

	t.Run("self address inferred from peer list when localAddr is empty", func(t *testing.T) {
		raw := []cluster.PeerConfig{
			{ID: 1, Address: "10.0.0.1:9098"},
			{ID: 2, Address: "10.0.0.2:9098"},
		}
		topo, err := cluster.NewTopology(1, "", raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if topo.LocalAddress() != "10.0.0.1:9098" {
			t.Errorf("expected local address inferred as 10.0.0.1:9098, got %q", topo.LocalAddress())
		}
	})

	t.Run("self address mismatch with peer list fails closed", func(t *testing.T) {
		raw := []cluster.PeerConfig{
			{ID: 1, Address: "10.0.0.1:9098"},
			{ID: 2, Address: "10.0.0.2:9098"},
		}
		_, err := cluster.NewTopology(1, "10.0.0.99:9098", raw)
		if err == nil {
			t.Fatal("expected error on address mismatch, got nil")
		}
		if !errors.Is(err, cluster.ErrSelfAddressMismatch) {
			t.Errorf("expected ErrSelfAddressMismatch, got %v", err)
		}
	})

	t.Run("self omitted from peers and localAddr empty fails closed", func(t *testing.T) {
		raw := []cluster.PeerConfig{
			{ID: 2, Address: "10.0.0.2:9098"},
			{ID: 3, Address: "10.0.0.3:9098"},
		}
		_, err := cluster.NewTopology(1, "", raw)
		if err == nil {
			t.Fatal("expected error when self omitted and localAddr empty, got nil")
		}
		if !errors.Is(err, cluster.ErrSelfNotFound) {
			t.Errorf("expected ErrSelfNotFound, got %v", err)
		}
	})

	t.Run("self omitted from peers synthesized when localAddr provided", func(t *testing.T) {
		raw := []cluster.PeerConfig{
			{ID: 2, Address: "10.0.0.2:9098"},
			{ID: 3, Address: "10.0.0.3:9098"},
		}
		topo, err := cluster.NewTopology(1, "10.0.0.1:9098", raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if topo.Size() != 3 {
			t.Errorf("Size() = %d, want 3", topo.Size())
		}
		if topo.RemoteSize() != 2 {
			t.Errorf("RemoteSize() = %d, want 2", topo.RemoteSize())
		}
		all := topo.Peers()
		if all[0].ID != 1 || all[1].ID != 2 || all[2].ID != 3 {
			t.Errorf("expected synthesized self ordered first, got %v", all)
		}
	})

	t.Run("duplicate node ID in peers fails closed", func(t *testing.T) {
		raw := []cluster.PeerConfig{
			{ID: 1, Address: "10.0.0.1:9098"},
			{ID: 2, Address: "10.0.0.2:9098"},
			{ID: 2, Address: "10.0.0.3:9098"},
		}
		_, err := cluster.NewTopology(1, "10.0.0.1:9098", raw)
		if err == nil {
			t.Fatal("expected error on duplicate node ID, got nil")
		}
		if !errors.Is(err, cluster.ErrDuplicateNodeID) {
			t.Errorf("expected ErrDuplicateNodeID, got %v", err)
		}
	})

	t.Run("duplicate address in peers fails closed", func(t *testing.T) {
		raw := []cluster.PeerConfig{
			{ID: 1, Address: "10.0.0.1:9098"},
			{ID: 2, Address: "10.0.0.2:9098"},
			{ID: 3, Address: "10.0.0.2:9098"},
		}
		_, err := cluster.NewTopology(1, "10.0.0.1:9098", raw)
		if err == nil {
			t.Fatal("expected error on duplicate address, got nil")
		}
		if !errors.Is(err, cluster.ErrDuplicatePeerAddress) {
			t.Errorf("expected ErrDuplicatePeerAddress, got %v", err)
		}
	})

	t.Run("local address duplicates an existing peer address fails closed", func(t *testing.T) {
		raw := []cluster.PeerConfig{
			{ID: 2, Address: "10.0.0.2:9098"},
		}
		// Node 1 tries to use the same address as Node 2
		_, err := cluster.NewTopology(1, "10.0.0.2:9098", raw)
		if err == nil {
			t.Fatal("expected error on local address collision with peer, got nil")
		}
		if !errors.Is(err, cluster.ErrDuplicatePeerAddress) {
			t.Errorf("expected ErrDuplicatePeerAddress, got %v", err)
		}
	})

	t.Run("invalid local node ID fails closed", func(t *testing.T) {
		raw := []cluster.PeerConfig{
			{ID: 1, Address: "10.0.0.1:9098"},
		}
		_, err := cluster.NewTopology(0, "10.0.0.1:9098", raw)
		if err == nil {
			t.Fatal("expected error on local ID 0, got nil")
		}
		if !errors.Is(err, cluster.ErrInvalidNodeID) {
			t.Errorf("expected ErrInvalidNodeID, got %v", err)
		}
	})

	t.Run("invalid peer node ID fails closed", func(t *testing.T) {
		raw := []cluster.PeerConfig{
			{ID: 0, Address: "10.0.0.2:9098"},
		}
		_, err := cluster.NewTopology(1, "10.0.0.1:9098", raw)
		if err == nil {
			t.Fatal("expected error on peer ID 0, got nil")
		}
		if !errors.Is(err, cluster.ErrInvalidNodeID) {
			t.Errorf("expected ErrInvalidNodeID, got %v", err)
		}
	})

	t.Run("invalid peer address in peers fails closed", func(t *testing.T) {
		raw := []cluster.PeerConfig{
			{ID: 2, Address: "missing-port"},
		}
		_, err := cluster.NewTopology(1, "10.0.0.1:9098", raw)
		if err == nil {
			t.Fatal("expected error on invalid peer address, got nil")
		}
		if !errors.Is(err, cluster.ErrInvalidPeerAddress) {
			t.Errorf("expected ErrInvalidPeerAddress, got %v", err)
		}
	})
}

// TestTopologyDeterminism proves that permutations of input peer lists always produce
// the exact same sorted, normalized representation (P14-M01-INV-04).
func TestTopologyDeterminism(t *testing.T) {
	permA := []cluster.PeerConfig{
		{ID: 3, Address: "10.0.0.3:9098"},
		{ID: 1, Address: "10.0.0.1:9098"},
		{ID: 2, Address: "10.0.0.2:9098"},
		{ID: 5, Address: "10.0.0.5:9098"},
		{ID: 4, Address: "10.0.0.4:9098"},
	}

	permB := []cluster.PeerConfig{
		{ID: 5, Address: "10.0.0.5:9098"},
		{ID: 2, Address: "10.0.0.2:9098"},
		{ID: 4, Address: "10.0.0.4:9098"},
		{ID: 1, Address: "10.0.0.1:9098"},
		{ID: 3, Address: "10.0.0.3:9098"},
	}

	topoA, err := cluster.NewTopology(1, "10.0.0.1:9098", permA)
	if err != nil {
		t.Fatalf("topoA failed: %v", err)
	}

	topoB, err := cluster.NewTopology(1, "10.0.0.1:9098", permB)
	if err != nil {
		t.Fatalf("topoB failed: %v", err)
	}

	peersA := topoA.Peers()
	peersB := topoB.Peers()

	if len(peersA) != len(peersB) {
		t.Fatalf("size mismatch: %d vs %d", len(peersA), len(peersB))
	}

	for i := range peersA {
		if peersA[i] != peersB[i] {
			t.Errorf("index %d mismatch: %+v vs %+v", i, peersA[i], peersB[i])
		}
	}

	remoteA := topoA.RemotePeers()
	remoteB := topoB.RemotePeers()

	if len(remoteA) != len(remoteB) {
		t.Fatalf("remote size mismatch: %d vs %d", len(remoteA), len(remoteB))
	}

	for i := range remoteA {
		if remoteA[i] != remoteB[i] {
			t.Errorf("remote index %d mismatch: %+v vs %+v", i, remoteA[i], remoteB[i])
		}
	}
}

// TestTopologyImmutability verifies that caller modifications to returned slices
// cannot mutate internal topology state (P14-M01-INV-07).
func TestTopologyImmutability(t *testing.T) {
	raw := []cluster.PeerConfig{
		{ID: 1, Address: "10.0.0.1:9098"},
		{ID: 2, Address: "10.0.0.2:9098"},
	}
	topo, err := cluster.NewTopology(1, "10.0.0.1:9098", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Mutate slice returned by Peers()
	peers := topo.Peers()
	peers[0].ID = 999
	peers[0].Address = "tampered:9098"

	freshPeers := topo.Peers()
	if freshPeers[0].ID != 1 || freshPeers[0].Address != "10.0.0.1:9098" {
		t.Fatalf("internal Peers() state was mutated: %+v", freshPeers[0])
	}

	// Mutate slice returned by RemotePeers()
	remotes := topo.RemotePeers()
	remotes[0].ID = 888
	remotes[0].Address = "tampered2:9098"

	freshRemotes := topo.RemotePeers()
	if freshRemotes[0].ID != 2 || freshRemotes[0].Address != "10.0.0.2:9098" {
		t.Fatalf("internal RemotePeers() state was mutated: %+v", freshRemotes[0])
	}
}

// TestTopologyLookups verifies lookup by ID and address.
func TestTopologyLookups(t *testing.T) {
	raw := []cluster.PeerConfig{
		{ID: 1, Address: "10.0.0.1:9098"},
		{ID: 2, Address: "10.0.0.2:9098"},
		{ID: 3, Address: "NODE3.Internal:9098"},
	}
	topo, err := cluster.NewTopology(1, "10.0.0.1:9098", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// ID lookups
	p, ok := topo.LookupPeer(2)
	if !ok || p.Address != "10.0.0.2:9098" {
		t.Errorf("LookupPeer(2) = (%+v, %v), want address 10.0.0.2:9098", p, ok)
	}
	if _, ok := topo.LookupPeer(99); ok {
		t.Error("LookupPeer(99) expected false, got true")
	}

	// Address lookups (including case-insensitive hostname normalization)
	p, ok = topo.LookupByAddress("node3.internal:9098")
	if !ok || p.ID != 3 {
		t.Errorf("LookupByAddress(node3.internal:9098) = (%+v, %v), want ID 3", p, ok)
	}
	p, ok = topo.LookupByAddress("Node3.INTERNAL:9098")
	if !ok || p.ID != 3 {
		t.Errorf("LookupByAddress(Node3.INTERNAL:9098) = (%+v, %v), want ID 3", p, ok)
	}
	if _, ok := topo.LookupByAddress("nonexistent:9098"); ok {
		t.Error("LookupByAddress(nonexistent) expected false, got true")
	}

	// Contains and IsSelf
	if !topo.Contains(1) || !topo.Contains(2) || !topo.Contains(3) {
		t.Error("Contains returned false for valid cluster peer")
	}
	if topo.Contains(4) {
		t.Error("Contains(4) expected false, got true")
	}
	if !topo.IsSelf(1) {
		t.Error("IsSelf(1) expected true, got false")
	}
	if topo.IsSelf(2) {
		t.Error("IsSelf(2) expected false, got true")
	}
}
