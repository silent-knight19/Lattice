package cluster_test

import (
	"testing"

	"github.com/silent-knight19/lattice/internal/cluster"
)

// FuzzValidateAndCanonicalizeAddress fuzzes address canonicalization.
// Invariant: Must never panic and must only return valid, non-empty "host:port" on success.
func FuzzValidateAndCanonicalizeAddress(f *testing.F) {
	seeds := []string{
		"127.0.0.1:9098",
		"10.0.0.1:80",
		"[::1]:9098",
		"[2001:db8::1]:9098",
		"localhost:9098",
		"node-1.cluster.internal:9098",
		"0.0.0.0:9098",
		"::1:9098",
		"invalid",
		":9098",
		"127.0.0.1:",
		"127.0.0.1:0",
		"127.0.0.1:70000",
		"127.0.0.1:-1",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, input string) {
		canon, err := cluster.ValidateAndCanonicalizeAddress(input)
		if err == nil {
			if canon == "" {
				t.Fatalf("successful canonicalization returned empty string for input %q", input)
			}
			// Re-canonicalization must be idempotent
			reCanon, reErr := cluster.ValidateAndCanonicalizeAddress(canon)
			if reErr != nil {
				t.Fatalf("re-canonicalizing %q failed: %v", canon, reErr)
			}
			if reCanon != canon {
				t.Fatalf("canonicalization not idempotent: %q vs %q", canon, reCanon)
			}
		}
	})
}

// FuzzParsePeersString fuzzes peer list string parsing.
// Invariants:
//   - Never panics or hangs.
//   - Successful parse returns slice with len <= MaxClusterSize.
//   - Every parsed peer has valid NodeID (> 0).
func FuzzParsePeersString(f *testing.F) {
	seeds := []string{
		"1=10.0.0.1:9098,2=10.0.0.2:9098,3=10.0.0.3:9098",
		"1@127.0.0.1:9098; 2@127.0.0.1:9097",
		"1:10.0.0.1:9098\n2:10.0.0.2:9098",
		"",
		"   ",
		"0=10.0.0.1:9098",
		"invalid=address",
		"1=",
		"=10.0.0.1:9098",
		"18446744073709551615=10.0.0.1:9098",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, input string) {
		peers, err := cluster.ParsePeersString(input)
		if err == nil {
			if len(peers) > cluster.MaxClusterSize {
				t.Fatalf("parsed peers length %d exceeds MaxClusterSize %d", len(peers), cluster.MaxClusterSize)
			}
			for _, p := range peers {
				if !p.ID.IsValid() {
					t.Fatalf("parsed peer with invalid ID: %d", p.ID)
				}
				if p.Address == "" {
					t.Fatalf("parsed peer with empty address")
				}
			}
		}
	})
}
