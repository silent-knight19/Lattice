package cluster

import (
	"fmt"
	"strings"

	"github.com/silent-knight19/lattice/internal/errors"
)

// ParsePeersString parses a delimited list of cluster peers.
//
// Format:
//   - Items delimited by commas, semicolons, or whitespace.
//   - Item format: "id=host:port", "id@host:port", or "id:host:port".
//   - Example: "1=10.0.0.1:9098,2=10.0.0.2:9098,3=10.0.0.3:9098"
//
// Returns a slice of raw PeerConfig structs ready for topology validation.
// Returns an empty slice if s is empty or whitespace-only.
func ParsePeersString(s string) ([]PeerConfig, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil, nil
	}

	// Split by commas, semicolons, or whitespace/newlines
	splitter := func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	}
	tokens := strings.FieldsFunc(trimmed, splitter)

	if len(tokens) > MaxClusterSize {
		return nil, &errors.ClusterTooLargeError{
			Count: len(tokens),
			Max:   MaxClusterSize,
		}
	}

	peers := make([]PeerConfig, 0, len(tokens))
	for i, token := range tokens {
		item := strings.TrimSpace(token)
		if item == "" {
			continue
		}

		var idPart, addrPart string
		if idx := strings.IndexAny(item, "=@"); idx != -1 {
			idPart = strings.TrimSpace(item[:idx])
			addrPart = strings.TrimSpace(item[idx+1:])
		} else if idx := strings.Index(item, ":"); idx != -1 {
			idPart = strings.TrimSpace(item[:idx])
			addrPart = strings.TrimSpace(item[idx+1:])
		} else {
			return nil, &errors.InvalidPeerAddressError{
				Address: item,
				Reason:  fmt.Sprintf("peer entry %d %q missing '=' or '@' delimiter (expected id=host:port)", i+1, item),
			}
		}

		if idPart == "" {
			return nil, &errors.InvalidNodeIDError{
				NodeID: 0,
				Reason: fmt.Sprintf("peer entry %d %q has empty node ID", i+1, item),
			}
		}
		if addrPart == "" {
			return nil, &errors.InvalidPeerAddressError{
				Address: item,
				Reason:  fmt.Sprintf("peer entry %d %q has empty address", i+1, item),
			}
		}

		nodeID, err := ParseNodeID(idPart)
		if err != nil {
			return nil, fmt.Errorf("peer entry %d %q: %w", i+1, item, err)
		}

		peers = append(peers, PeerConfig{
			ID:      nodeID,
			Address: addrPart,
		})
	}

	return peers, nil
}
