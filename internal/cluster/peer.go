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

	// Normalize trailing dot in FQDNs and lowercase to prevent syntactic aliasing (e.g. "node1." vs "node1", "0.0.0.0." vs "0.0.0.0")
	h := host
	if strings.HasSuffix(h, ".") && len(h) > 1 {
		h = h[:len(h)-1]
	}
	h = strings.ToLower(h)

	// Security: Wildcard IPs are prohibited as peer targets (only valid for listeners)
	if h == "0.0.0.0" || h == "::" || h == "[::]" {
		return "", fmt.Errorf("%w: wildcard IP address %q forbidden as peer target", errors.ErrWildcardAddress, host)
	}

	var canonicalHost string
	if ip := net.ParseIP(h); ip != nil {
		if ip.IsUnspecified() {
			return "", fmt.Errorf("%w: wildcard IP address %q forbidden as peer target", errors.ErrWildcardAddress, host)
		}
		canonicalHost = ip.String()
	} else {
		// RFC 1123 Hostname Validation
		if len(h) < 1 || len(h) > 253 {
			return "", &errors.InvalidPeerAddressError{
				Address: rawAddr,
				Reason:  fmt.Sprintf("hostname length %d invalid: must be between 1 and 253 characters", len(h)),
			}
		}

		labels := strings.Split(h, ".")
		// In RFC 1123, a top-level domain (last label) cannot be all-numeric (e.g. invalid IPv4 256.0.0.1 or domain ending in .123)
		if len(labels) > 1 && isAllDigits(labels[len(labels)-1]) {
			return "", &errors.InvalidPeerAddressError{
				Address: rawAddr,
				Reason:  fmt.Sprintf("hostname TLD %q cannot be all-numeric", labels[len(labels)-1]),
			}
		}
		for _, label := range labels {
			if len(label) < 1 || len(label) > 63 {
				return "", &errors.InvalidPeerAddressError{
					Address: rawAddr,
					Reason:  fmt.Sprintf("hostname label %q length %d invalid: must be between 1 and 63 characters", label, len(label)),
				}
			}
			// First and last characters of label must be alphanumeric
			first := label[0]
			last := label[len(label)-1]
			if !isAlphanumeric(first) || !isAlphanumeric(last) {
				return "", &errors.InvalidPeerAddressError{
					Address: rawAddr,
					Reason:  fmt.Sprintf("hostname label %q must start and end with an alphanumeric character", label),
				}
			}
			for i := 0; i < len(label); i++ {
				c := label[i]
				if !isAlphanumeric(c) && c != '-' {
					return "", &errors.InvalidPeerAddressError{
						Address: rawAddr,
						Reason:  fmt.Sprintf("hostname label %q contains invalid character %q (only [a-z0-9-] permitted)", label, c),
					}
				}
			}
		}
		canonicalHost = h
	}

	return net.JoinHostPort(canonicalHost, strconv.Itoa(port)), nil
}

func isAlphanumeric(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

func isAllDigits(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
