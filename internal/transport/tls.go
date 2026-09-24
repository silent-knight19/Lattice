package transport

import (
	"crypto/tls"
	"crypto/x509"
	stdErrors "errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/security"
)

const (
	// PeerCertRoleOU defines the canonical Subject OrganizationalUnit identifying
	// authenticated Raft peer certificates in mutual TLS communication.
	PeerCertRoleOU = "Lattice Raft Peer"

	// ClientCertRoleOU defines the canonical Subject OrganizationalUnit identifying
	// authenticated client certificates in client mTLS communication.
	ClientCertRoleOU = "Lattice Client"

	// MaxCertificateFileSize bounds the maximum readable size of certificate files to 10 MiB.
	MaxCertificateFileSize = 10 * 1024 * 1024
)

// ValidateCertificateFile ensures that path points to an accessible, non-empty,
// regular file within security bounds.
func ValidateCertificateFile(path string) error {
	cleanPath, err := security.CleanAndValidatePath(path)
	if err != nil {
		return fmt.Errorf("invalid path %q: %w", path, err)
	}

	info, err := os.Stat(cleanPath)
	if err != nil {
		return fmt.Errorf("certificate file %q inaccessible: %w", cleanPath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("certificate path %q is a directory, expected regular file", cleanPath)
	}
	if info.Size() == 0 {
		return fmt.Errorf("certificate file %q is empty", cleanPath)
	}
	if info.Size() > MaxCertificateFileSize {
		return fmt.Errorf("certificate file %q exceeds maximum allowed size (10 MiB)", cleanPath)
	}
	return nil
}

// LoadCertPool parses PEM-encoded CA certificates from caPath into an x509.CertPool.
func LoadCertPool(caPath string) (*x509.CertPool, error) {
	if err := ValidateCertificateFile(caPath); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate file %q: %w", caPath, err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("failed to parse any valid PEM certificates from %q", caPath)
	}
	return pool, nil
}

// ServerTLSConfig constructs a hardened TLS 1.3 server configuration for the client data plane (:9099).
func ServerTLSConfig(certFile, keyFile, clientCAFile string, requireClientCert bool) (*tls.Config, error) {
	if err := ValidateCertificateFile(certFile); err != nil {
		return nil, fmt.Errorf("server certificate: %w", err)
	}
	if err := ValidateCertificateFile(keyFile); err != nil {
		return nil, fmt.Errorf("server private key: %w", err)
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load server TLS key pair: %w", err)
	}

	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
	}

	if clientCAFile != "" {
		pool, err := LoadCertPool(clientCAFile)
		if err != nil {
			return nil, fmt.Errorf("client CA: %w", err)
		}
		cfg.ClientCAs = pool
		if requireClientCert {
			cfg.ClientAuth = tls.RequireAndVerifyClientCert
			cfg.VerifyConnection = func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return stdErrors.New("no client certificate presented during TLS handshake")
				}
				leaf := cs.PeerCertificates[0]
				// Defense-in-depth: Raft peer certificate cannot authenticate as a client certificate
				if IsPeerCertificate(leaf) && !IsClientCertificate(leaf) {
					return stdErrors.New("certificate rejected: Raft peer certificate cannot be used as client certificate (PKI separation violation)")
				}
				return nil
			}
		} else {
			cfg.ClientAuth = tls.VerifyClientCertIfGiven
		}
	} else if requireClientCert {
		return nil, fmt.Errorf("cannot require client certificates without configuring a trusted Client CA file")
	}

	return cfg, nil
}

// ClientTLSConfig constructs a hardened TLS 1.3 client configuration for SDK and CLI connections.
func ClientTLSConfig(caFile, certFile, keyFile, serverName string, insecureSkipVerify bool) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
		ServerName:         serverName,
		InsecureSkipVerify: insecureSkipVerify,
	}

	if caFile != "" {
		pool, err := LoadCertPool(caFile)
		if err != nil {
			return nil, fmt.Errorf("client root CA: %w", err)
		}
		cfg.RootCAs = pool
	}

	if certFile != "" || keyFile != "" {
		if certFile == "" || keyFile == "" {
			return nil, fmt.Errorf("both client certificate and private key files must be specified for client mTLS")
		}
		if err := ValidateCertificateFile(certFile); err != nil {
			return nil, fmt.Errorf("client certificate: %w", err)
		}
		if err := ValidateCertificateFile(keyFile); err != nil {
			return nil, fmt.Errorf("client private key: %w", err)
		}
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load client TLS key pair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}

	return cfg, nil
}

// ExtractNodeIDFromCert inspects X.509 Subject Alternative Names and CommonName
// to extract the authoritative cluster NodeID identity asserted by the certificate.
func ExtractNodeIDFromCert(cert *x509.Certificate) (cluster.NodeID, error) {
	if cert == nil {
		return cluster.NodeIDNil, errors.ErrNilReceiver
	}

	// 1. Check SAN URIs: spiffe://lattice/node/<id> or lattice://node/<id>
	for _, uri := range cert.URIs {
		if uri != nil && (uri.Scheme == "spiffe" || uri.Scheme == "lattice") {
			parts := strings.Split(strings.Trim(uri.Path, "/"), "/")
			if len(parts) >= 2 && parts[0] == "node" {
				if id, err := cluster.ParseNodeID(parts[1]); err == nil && id.IsValid() {
					return id, nil
				}
			}
		}
	}

	// 2. Check SAN DNS Names: node-<id>, node<id>, peer-<id>, <id>
	for _, dns := range cert.DNSNames {
		if id, ok := parseNodeIDString(dns); ok {
			return id, nil
		}
	}

	// 3. Check Subject CommonName
	if id, ok := parseNodeIDString(cert.Subject.CommonName); ok {
		return id, nil
	}

	return cluster.NodeIDNil, fmt.Errorf("no valid NodeID found in certificate (CN=%q, DNS=%v)",
		cert.Subject.CommonName, cert.DNSNames)
}

// parseNodeIDString attempts to extract a valid NodeID from common naming patterns.
func parseNodeIDString(s string) (cluster.NodeID, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return cluster.NodeIDNil, false
	}
	// Extract the host portion if domain-qualified (e.g. "node-1.lattice.cluster" -> "node-1")
	firstPart := strings.Split(s, ".")[0]

	// Strip common prefixes
	prefixes := []string{"node-", "node", "peer-", "peer"}
	candidate := firstPart
	lower := strings.ToLower(firstPart)
	for _, p := range prefixes {
		if strings.HasPrefix(lower, p) {
			candidate = firstPart[len(p):]
			break
		}
	}

	id, err := cluster.ParseNodeID(candidate)
	if err == nil && id.IsValid() {
		return id, true
	}
	return cluster.NodeIDNil, false
}

// IsPeerCertificate reports whether cert asserts membership in the Raft peer trust domain.
func IsPeerCertificate(cert *x509.Certificate) bool {
	if cert == nil {
		return false
	}
	for _, ou := range cert.Subject.OrganizationalUnit {
		if strings.EqualFold(ou, PeerCertRoleOU) || strings.EqualFold(ou, "Raft Peer") || strings.EqualFold(ou, "Peer") {
			return true
		}
	}
	// Also permit certificates whose Organization indicates Lattice Cluster
	for _, org := range cert.Subject.Organization {
		if strings.EqualFold(org, "Lattice Cluster") || strings.EqualFold(org, "Lattice Peer") {
			return true
		}
	}
	return false
}

// IsClientCertificate reports whether cert asserts client identity in the Lattice client trust domain.
func IsClientCertificate(cert *x509.Certificate) bool {
	if cert == nil {
		return false
	}
	for _, ou := range cert.Subject.OrganizationalUnit {
		if strings.EqualFold(ou, ClientCertRoleOU) || strings.EqualFold(ou, "Client") {
			return true
		}
	}
	for _, org := range cert.Subject.Organization {
		if strings.EqualFold(org, "Lattice Client") {
			return true
		}
	}
	return false
}

// PeerServerTLSConfig constructs a hardened TLS 1.3 listener configuration for inbound Raft peer connections.
func PeerServerTLSConfig(certFile, keyFile, peerCAFile string, topology *cluster.Topology) (*tls.Config, error) {
	if err := ValidateCertificateFile(certFile); err != nil {
		return nil, fmt.Errorf("peer server certificate: %w", err)
	}
	if err := ValidateCertificateFile(keyFile); err != nil {
		return nil, fmt.Errorf("peer server private key: %w", err)
	}
	if err := ValidateCertificateFile(peerCAFile); err != nil {
		return nil, fmt.Errorf("peer CA certificate: %w", err)
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load peer TLS key pair: %w", err)
	}

	caPool, err := LoadCertPool(peerCAFile)
	if err != nil {
		return nil, fmt.Errorf("peer CA pool: %w", err)
	}

	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}

	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return stdErrors.New("no client certificate presented during peer handshake")
		}
		leaf := cs.PeerCertificates[0]

		if !IsPeerCertificate(leaf) {
			return stdErrors.New("certificate rejected: missing required Raft peer role (OU='Lattice Raft Peer')")
		}

		nodeID, err := ExtractNodeIDFromCert(leaf)
		if err != nil {
			return fmt.Errorf("failed to extract peer NodeID from certificate: %w", err)
		}

		if topology != nil {
			if topology.IsSelf(nodeID) {
				return fmt.Errorf("inbound peer connection authenticated as local node ID %d; self-connections prohibited", nodeID)
			}
			if !topology.Contains(nodeID) {
				return fmt.Errorf("peer authenticated as NodeID %d which is not a recognized member of cluster topology", nodeID)
			}
		}

		return nil
	}

	return cfg, nil
}

// PeerClientTLSConfig constructs a hardened TLS 1.3 dialer configuration for outbound Raft peer connections.
func PeerClientTLSConfig(certFile, keyFile, peerCAFile string, targetPeerID cluster.NodeID, targetAddr string, topology *cluster.Topology) (*tls.Config, error) {
	if err := ValidateCertificateFile(certFile); err != nil {
		return nil, fmt.Errorf("peer client certificate: %w", err)
	}
	if err := ValidateCertificateFile(keyFile); err != nil {
		return nil, fmt.Errorf("peer client private key: %w", err)
	}
	if err := ValidateCertificateFile(peerCAFile); err != nil {
		return nil, fmt.Errorf("peer CA certificate: %w", err)
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load peer TLS key pair: %w", err)
	}

	caPool, err := LoadCertPool(peerCAFile)
	if err != nil {
		return nil, fmt.Errorf("peer CA pool: %w", err)
	}

	// Determine server name for TLS SNI
	serverName := fmt.Sprintf("node-%d", targetPeerID)
	if targetAddr != "" {
		host, _, err := net.SplitHostPort(targetAddr)
		if err == nil && net.ParseIP(host) == nil && host != "localhost" {
			serverName = host
		}
	}

	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		ServerName:   serverName,
	}

	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return stdErrors.New("no server certificate presented by remote peer")
		}
		leaf := cs.PeerCertificates[0]

		if !IsPeerCertificate(leaf) {
			return stdErrors.New("remote certificate rejected: missing required Raft peer role (OU='Lattice Raft Peer')")
		}

		nodeID, err := ExtractNodeIDFromCert(leaf)
		if err != nil {
			return fmt.Errorf("failed to extract NodeID from remote peer certificate: %w", err)
		}

		if nodeID != targetPeerID {
			return fmt.Errorf("peer certificate NodeID mismatch: expected %d, remote asserted %d", targetPeerID, nodeID)
		}

		if topology != nil && !topology.Contains(nodeID) {
			return fmt.Errorf("remote peer NodeID %d is not in configured cluster topology", nodeID)
		}

		return nil
	}

	return cfg, nil
}

// ValidatePeerTLSConfig verifies that cfg satisfies the mandatory mutual TLS 1.3
// security policy for Lattice Raft peer transport.
//
// Invariants enforced:
//   - cfg must be non-nil.
//   - MinVersion must be tls.VersionTLS13.
//   - MaxVersion, if set, must be >= tls.VersionTLS13.
//   - InsecureSkipVerify must not be true.
//   - ClientAuth must be tls.RequireAndVerifyClientCert (mutual TLS).
//   - ClientCAs must be non-nil.
//   - Certificates must not be empty (or GetCertificate / GetClientCertificate configured).
//
// Rejects:
//   - nil TLS config
//   - TLS < 1.3
//   - TLS 1.3 with NoClientCert
//   - TLS 1.3 with RequestClientCert
//   - TLS 1.3 with VerifyClientCertIfGiven
//   - TLS 1.3 with RequireAnyClientCert
//   - TLS 1.3 with RequireAndVerifyClientCert but nil ClientCAs
//   - missing certificate/key
//
// Accepts:
//   - correctly constructed peer mTLS configuration
func ValidatePeerTLSConfig(cfg *tls.Config) error {
	if cfg == nil {
		return fmt.Errorf("%w: peer TLS configuration is nil; mutual TLS 1.3 required", errors.ErrInsecureTransport)
	}

	if cfg.MinVersion < tls.VersionTLS13 || (cfg.MaxVersion != 0 && cfg.MaxVersion < tls.VersionTLS13) {
		return fmt.Errorf("%w: peer TLS configuration mandates TLS 1.3 (min=%x, max=%x)",
			errors.ErrInsecureTransport, cfg.MinVersion, cfg.MaxVersion)
	}

	if cfg.InsecureSkipVerify {
		return fmt.Errorf("%w: peer TLS InsecureSkipVerify must not be enabled", errors.ErrInsecureTransport)
	}

	switch cfg.ClientAuth {
	case tls.RequireAndVerifyClientCert:
		// Required mutual TLS invariant
	case tls.NoClientCert:
		return fmt.Errorf("%w: peer TLS requires mutual client certificate verification (got NoClientCert)", errors.ErrInsecureTransport)
	case tls.RequestClientCert:
		return fmt.Errorf("%w: peer TLS requires mandatory client certificate verification (got RequestClientCert)", errors.ErrInsecureTransport)
	case tls.VerifyClientCertIfGiven:
		return fmt.Errorf("%w: peer TLS requires mandatory client certificate verification (got VerifyClientCertIfGiven)", errors.ErrInsecureTransport)
	case tls.RequireAnyClientCert:
		return fmt.Errorf("%w: peer TLS requires verified client certificate against trusted CA (got RequireAnyClientCert)", errors.ErrInsecureTransport)
	default:
		return fmt.Errorf("%w: peer TLS invalid ClientAuth mode (%v); RequireAndVerifyClientCert required", errors.ErrInsecureTransport, cfg.ClientAuth)
	}

	if cfg.ClientCAs == nil {
		return fmt.Errorf("%w: peer TLS requires non-nil trusted ClientCAs pool for mutual authentication", errors.ErrInsecureTransport)
	}

	if len(cfg.Certificates) == 0 && cfg.GetCertificate == nil && cfg.GetClientCertificate == nil {
		return fmt.Errorf("%w: peer TLS requires configured X.509 certificate and private key", errors.ErrInsecureTransport)
	}

	return nil
}

// ValidatePeerDialerTLSConfig verifies that outbound dialer TLS config cfg satisfies
// the TLS 1.3 client authentication and trusted CA invariants.
func ValidatePeerDialerTLSConfig(cfg *tls.Config) error {
	if cfg == nil {
		return fmt.Errorf("%w: peer dialer TLS configuration is nil; mutual TLS 1.3 required", errors.ErrInsecureTransport)
	}

	if cfg.MinVersion < tls.VersionTLS13 || (cfg.MaxVersion != 0 && cfg.MaxVersion < tls.VersionTLS13) {
		return fmt.Errorf("%w: peer dialer TLS configuration mandates TLS 1.3 (min=%x, max=%x)",
			errors.ErrInsecureTransport, cfg.MinVersion, cfg.MaxVersion)
	}

	if cfg.InsecureSkipVerify {
		return fmt.Errorf("%w: peer dialer TLS InsecureSkipVerify must not be enabled", errors.ErrInsecureTransport)
	}

	if len(cfg.Certificates) == 0 && cfg.GetCertificate == nil && cfg.GetClientCertificate == nil {
		return fmt.Errorf("%w: peer dialer TLS requires client certificate for mutual authentication", errors.ErrInsecureTransport)
	}

	if cfg.RootCAs == nil && cfg.ClientCAs == nil {
		return fmt.Errorf("%w: peer dialer TLS requires non-nil trusted CA pool", errors.ErrInsecureTransport)
	}

	if cfg.ClientAuth != 0 && cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		return fmt.Errorf("%w: peer dialer TLS ClientAuth must be RequireAndVerifyClientCert if specified (got %v)",
			errors.ErrInsecureTransport, cfg.ClientAuth)
	}

	return nil
}
