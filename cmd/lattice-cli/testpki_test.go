package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/transport"
)

type cliTestCA struct {
	Cert     *x509.Certificate
	Key      *ecdsa.PrivateKey
	CertPEM  []byte
	CertPath string
}

func newCLITestCA(t *testing.T, name string) *cliTestCA {
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
			Organization: []string{"Lattice CLI Test Authority"},
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

	return &cliTestCA{
		Cert:     cert,
		Key:      priv,
		CertPEM:  certPEM,
		CertPath: caFile,
	}
}

func (ca *cliTestCA) issueCert(t *testing.T, name string, isClient, isServer bool) (certPath, keyPath string) {
	t.Helper()

	dir := t.TempDir()
	certPath = filepath.Join(dir, name+".crt")
	keyPath = filepath.Join(dir, name+".key")

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key for %s: %v", name, err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("failed to generate serial for %s: %v", name, err)
	}

	extKeyUsage := []x509.ExtKeyUsage{}
	if isServer {
		extKeyUsage = append(extKeyUsage, x509.ExtKeyUsageServerAuth)
	}
	if isClient {
		extKeyUsage = append(extKeyUsage, x509.ExtKeyUsageClientAuth)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:         name,
			Organization:       []string{"Lattice Test"},
			OrganizationalUnit: []string{transport.ClientCertRoleOU},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           extKeyUsage,
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost", name},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, ca.Cert, &priv.PublicKey, ca.Key)
	if err != nil {
		t.Fatalf("failed to create certificate for %s: %v", name, err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		t.Fatalf("failed to write cert file: %v", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("failed to marshal private key for %s: %v", name, err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		t.Fatalf("failed to write key file: %v", err)
	}

	return certPath, keyPath
}
