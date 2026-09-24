package main

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

	"github.com/silent-knight19/lattice/internal/transport"
)

type daemonTestCA struct {
	Cert     *x509.Certificate
	Key      *ecdsa.PrivateKey
	CertPEM  []byte
	CertPath string
}

func newDaemonTestCA(t *testing.T, name string) *daemonTestCA {
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
			Organization: []string{"Lattice Daemon Test Authority"},
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

	return &daemonTestCA{
		Cert:     cert,
		Key:      priv,
		CertPEM:  certPEM,
		CertPath: caFile,
	}
}

func (ca *daemonTestCA) issueCert(t *testing.T, cn string, org, ou []string, isServer, isClient bool, ips []net.IP, dns []string, uris []*url.URL) (string, string) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate private key: %v", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("failed to generate serial: %v", err)
	}

	extKeyUsage := make([]x509.ExtKeyUsage, 0)
	if isServer {
		extKeyUsage = append(extKeyUsage, x509.ExtKeyUsageServerAuth)
	}
	if isClient {
		extKeyUsage = append(extKeyUsage, x509.ExtKeyUsageClientAuth)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:         cn,
			Organization:       org,
			OrganizationalUnit: ou,
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           extKeyUsage,
		BasicConstraintsValid: true,
		IPAddresses:           ips,
		DNSNames:              dns,
		URIs:                  uris,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, ca.Cert, &priv.PublicKey, ca.Key)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("failed to marshal private key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	dir := t.TempDir()
	certPath := filepath.Join(dir, cn+".crt")
	keyPath := filepath.Join(dir, cn+".key")

	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		t.Fatalf("failed to write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		t.Fatalf("failed to write key: %v", err)
	}

	return certPath, keyPath
}

func (ca *daemonTestCA) issueServerCert(t *testing.T, name string) (string, string) {
	return ca.issueCert(t, name, []string{"Lattice Server"}, nil, true, false,
		[]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("192.168.1.10")}, []string{"localhost", name}, nil)
}

func (ca *daemonTestCA) issueClientCert(t *testing.T, name string) (string, string) {
	return ca.issueCert(t, name, []string{"Lattice Client"}, []string{transport.ClientCertRoleOU}, false, true,
		nil, []string{name}, nil)
}

func (ca *daemonTestCA) issuePeerCert(t *testing.T, nodeID uint64) (string, string) {
	nodeName := fmt.Sprintf("node-%d", nodeID)
	u, _ := url.Parse(fmt.Sprintf("spiffe://lattice/node/%d", nodeID))
	return ca.issueCert(t, nodeName, []string{"Lattice Cluster"}, []string{transport.PeerCertRoleOU}, true, true,
		[]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("192.168.1.100")}, []string{"localhost", nodeName}, []*url.URL{u})
}
