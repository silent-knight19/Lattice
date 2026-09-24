package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCA holds an ephemeral in-memory Certificate Authority for test fixtures.
type TestCA struct {
	Cert     *x509.Certificate
	Key      *ecdsa.PrivateKey
	CertPEM  []byte
	CertPath string
}

// CertOptions parameterizes ephemeral certificate generation for hermetic tests.
type CertOptions struct {
	CommonName         string
	Organization       []string
	OrganizationalUnit []string
	DNSNames           []string
	IPAddresses        []net.IP
	URIs               []*url.URL
	NotBefore          time.Time
	NotAfter           time.Time
	IsClient           bool
	IsServer           bool
	MalformedCert      bool
	MalformedKey       bool
}

// NewTestCA creates a self-signed root CA in temp storage with automatic test cleanup.
func NewTestCA(t testing.TB, name string) *TestCA {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate CA private key: %v", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("failed to generate CA serial: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   name,
			Organization: []string{"Lattice Test Authority"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create CA certificate: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	caFile := filepath.Join(t.TempDir(), name+"-ca.crt")
	if err := os.WriteFile(caFile, certPEM, 0600); err != nil {
		t.Fatalf("failed to write CA cert file: %v", err)
	}

	cert, err := x509.ParseCertificate(derBytes)
	if err != nil {
		t.Fatalf("failed to parse generated CA cert: %v", err)
	}

	return &TestCA{
		Cert:     cert,
		Key:      priv,
		CertPEM:  certPEM,
		CertPath: caFile,
	}
}

// IssueCert creates a signed certificate using the given TestCA and writes it to a temporary directory.
func (ca *TestCA) IssueCert(t testing.TB, name string, opts CertOptions) (certPath, keyPath string) {
	t.Helper()

	dir := t.TempDir()
	certPath = filepath.Join(dir, name+".crt")
	keyPath = filepath.Join(dir, name+".key")

	if opts.MalformedCert {
		if err := os.WriteFile(certPath, []byte("--- INVALID MALFORMED CERT PEM ---"), 0600); err != nil {
			t.Fatalf("failed to write malformed cert: %v", err)
		}
		if err := os.WriteFile(keyPath, []byte("--- INVALID MALFORMED KEY PEM ---"), 0600); err != nil {
			t.Fatalf("failed to write malformed key: %v", err)
		}
		return certPath, keyPath
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key for %s: %v", name, err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("failed to generate serial for %s: %v", name, err)
	}

	notBefore := opts.NotBefore
	if notBefore.IsZero() {
		notBefore = time.Now().Add(-1 * time.Hour)
	}
	notAfter := opts.NotAfter
	if notAfter.IsZero() {
		notAfter = time.Now().Add(24 * time.Hour)
	}

	keyUsage := x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
	extKeyUsage := []x509.ExtKeyUsage{}
	if opts.IsServer {
		extKeyUsage = append(extKeyUsage, x509.ExtKeyUsageServerAuth)
	}
	if opts.IsClient {
		extKeyUsage = append(extKeyUsage, x509.ExtKeyUsageClientAuth)
	}
	if len(extKeyUsage) == 0 {
		extKeyUsage = append(extKeyUsage, x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:         opts.CommonName,
			Organization:       opts.Organization,
			OrganizationalUnit: opts.OrganizationalUnit,
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              keyUsage,
		ExtKeyUsage:           extKeyUsage,
		BasicConstraintsValid: true,
		DNSNames:              opts.DNSNames,
		IPAddresses:           opts.IPAddresses,
		URIs:                  opts.URIs,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, ca.Cert, &priv.PublicKey, ca.Key)
	if err != nil {
		t.Fatalf("failed to create certificate for %s: %v", name, err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		t.Fatalf("failed to write cert file %s: %v", certPath, err)
	}

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("failed to marshal private key for %s: %v", name, err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if opts.MalformedKey {
		keyPEM = []byte("--- CORRUPT PRIVATE KEY BYTES ---")
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		t.Fatalf("failed to write key file %s: %v", keyPath, err)
	}

	return certPath, keyPath
}

// IssueServerCert issues a server certificate valid for localhost and 127.0.0.1.
func (ca *TestCA) IssueServerCert(t testing.TB, name string) (certPath, keyPath string) {
	return ca.IssueCert(t, name, CertOptions{
		CommonName:   name,
		Organization: []string{"Lattice Server"},
		DNSNames:     []string{"localhost", name},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
		IsServer:     true,
	})
}

// IssueClientCert issues a client certificate for client mTLS.
func (ca *TestCA) IssueClientCert(t testing.TB, clientName string) (certPath, keyPath string) {
	return ca.IssueCert(t, clientName, CertOptions{
		CommonName:         clientName,
		Organization:       []string{"Lattice Client"},
		OrganizationalUnit: []string{ClientCertRoleOU},
		IsClient:           true,
	})
}

// IssuePeerCert issues an authenticated Raft peer certificate.
func (ca *TestCA) IssuePeerCert(t testing.TB, nodeID uint64) (certPath, keyPath string) {
	nodeName := fmt.Sprintf("node-%d", nodeID)
	u, _ := url.Parse(fmt.Sprintf("spiffe://lattice/node/%d", nodeID))
	return ca.IssueCert(t, nodeName, CertOptions{
		CommonName:         nodeName,
		Organization:       []string{"Lattice Cluster"},
		OrganizationalUnit: []string{PeerCertRoleOU},
		DNSNames:           []string{"localhost", nodeName},
		IPAddresses:        []net.IP{net.ParseIP("127.0.0.1")},
		URIs:               []*url.URL{u},
		IsServer:           true,
		IsClient:           true,
	})
}
