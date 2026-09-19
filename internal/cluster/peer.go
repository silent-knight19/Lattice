package cluster

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/silent-knight19/lattice/internal/errors"
)

// PeerConfig encapsulates unvalidated, raw peer configuration parsed from
// configuration files or CLI parameters.
type PeerConfig struct {
	ID      NodeID `json:"id"`
	Address string `json:"address"`
}

// Peer represents an immutable, validated cluster peer identity and endpoint.
type Peer struct {
	ID      NodeID
	Address string // Syntactically canonicalized "host:port"
}

// String returns a human-readable representation of the peer (e.g. "1=10.0.0.1:9098").
func (p Peer) String() string {
	return fmt.Sprintf("%d=%s", p.ID, p.Address)
}

// ValidateAndCanonicalizeAddress verifies that rawAddr is a syntactically valid
// "host:port" endpoint and returns its canonical structural form.
//
// Invariants enforced without network or DNS side effects:
//   - Must contain both host and numeric port via net.SplitHostPort.
//   - Host must not be empty.
//   - Port must be strictly between 1 and 65535. Port 0 is rejected.
//   - Wildcard target IPs (0.0.0.0, ::) are rejected fail-closed.
//   - Valid IP addresses (IPv4, IPv6) are normalized to their standard text representation.
//   - Hostnames are normalized to lowercase.
func ValidateAndCanonicalizeAddress(rawAddr string) (string, error) {
	trimmed := strings.TrimSpace(rawAddr)
	if trimmed == "" {
		return "", &errors.InvalidPeerAddressError{
			Address: rawAddr,
			Reason:  "address cannot be empty",
		}
	}

	host, portStr, err := net.SplitHostPort(trimmed)
	if err != nil {
		return "", &errors.InvalidPeerAddressError{
			Address: rawAddr,
			Reason:  fmt.Sprintf("expected host:port format: %v", err),
		}
	}

	host = strings.TrimSpace(host)
	if host == "" {
		return "", &errors.InvalidPeerAddressError{
			Address: rawAddr,
			Reason:  "host cannot be empty",
		}
	}

	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", &errors.InvalidPeerAddressError{
			Address: rawAddr,
			Reason:  fmt.Sprintf("invalid port %q: must be between 1 and 65535", portStr),
		}
	}

	// Security: Wildcard IPs are prohibited as peer targets (only valid for listeners)
	if host == "0.0.0.0" || host == "::" || host == "[::]" {
		return "", fmt.Errorf("%w: wildcard IP address %q forbidden as peer target", errors.ErrWildcardAddress, host)
	}

	var canonicalHost string
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsUnspecified() {
			return "", fmt.Errorf("%w: wildcard IP address %q forbidden as peer target", errors.ErrWildcardAddress, host)
		}
		canonicalHost = ip.String()
	} else {
		canonicalHost = strings.ToLower(host)
	}

	return net.JoinHostPort(canonicalHost, strconv.Itoa(port)), nil
}
