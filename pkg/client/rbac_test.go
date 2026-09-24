package client_test

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"testing"

	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/transport"
	"github.com/silent-knight19/lattice/pkg/client"
)

func certFPFromFile(t testing.TB, certPath string) string {
	t.Helper()
	data, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("failed to read cert file %s: %v", certPath, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatalf("failed to decode PEM block from %s", certPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse cert PEM %s: %v", certPath, err)
	}
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

func startTLSAuthzServer(t *testing.T, ca *clientTestCA, policy map[string]string) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	eng := engine.NewEngineWithOptions(engine.EngineOptions{DBPath: dir})
	if err := eng.Open(); err != nil {
		t.Fatalf("failed to open engine: %v", err)
	}

	srvCert, srvKey := ca.issueCert(t, "server", false, true, "")

	cfg := transport.DefaultServerConfig()
	cfg.Address = "127.0.0.1:0"
	cfg.TLSCertFile = srvCert
	cfg.TLSKeyFile = srvKey
	cfg.ClientCAFile = ca.CertPath
	cfg.RequireClientCert = true
	cfg.ClientAuthzPolicy = policy

	srv, err := transport.NewServer(cfg, eng)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	go func() { _ = srv.Serve(ln) }()

	cleanup := func() {
		_ = srv.Shutdown(context.Background())
		_ = ln.Close()
		_ = eng.Close()
	}

	return ln.Addr().String(), cleanup
}

func TestClient_RBAC_Reader(t *testing.T) {
	ca := newClientTestCA(t, "ca")
	readerCert, readerKey := ca.issueCert(t, "reader", true, false, transport.ClientCertRoleOU)
	readerFP := certFPFromFile(t, readerCert)

	policy := map[string]string{readerFP: "reader"}
	addr, cleanup := startTLSAuthzServer(t, ca, policy)
	defer cleanup()

	c, err := client.DialWithOptions(addr, client.Options{
		EnableTLS:  true,
		CAFile:     ca.CertPath,
		CertFile:   readerCert,
		KeyFile:    readerKey,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("DialWithOptions failed: %v", err)
	}
	defer c.Close()

	ctx := context.Background()

	// Reads allowed
	_, err = c.Get(ctx, []byte("missing"))
	if !errors.Is(err, client.ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound, got %v", err)
	}

	exists, err := c.Exists(ctx, []byte("missing"))
	if err != nil || exists {
		t.Fatalf("expected exists=false nil, got %v %v", exists, err)
	}

	snap, err := c.Stats(ctx)
	if err != nil || snap == nil {
		t.Fatalf("expected stats success, got %v", err)
	}

	// Writes denied with client.ErrPermissionDenied
	err = c.Put(ctx, []byte("k"), []byte("v"))
	if !errors.Is(err, client.ErrPermissionDenied) {
		t.Fatalf("PUT: expected client.ErrPermissionDenied, got %v", err)
	}

	err = c.Delete(ctx, []byte("k"))
	if !errors.Is(err, client.ErrPermissionDenied) {
		t.Fatalf("DELETE: expected client.ErrPermissionDenied, got %v", err)
	}

	batch := client.NewWriteBatch()
	batch.Put([]byte("k"), []byte("v"))
	err = c.Batch(ctx, batch)
	if !errors.Is(err, client.ErrPermissionDenied) {
		t.Fatalf("BATCH: expected client.ErrPermissionDenied, got %v", err)
	}
}

func TestClient_RBAC_Writer(t *testing.T) {
	ca := newClientTestCA(t, "ca")
	writerCert, writerKey := ca.issueCert(t, "writer", true, false, transport.ClientCertRoleOU)
	writerFP := certFPFromFile(t, writerCert)

	policy := map[string]string{writerFP: "writer"}
	addr, cleanup := startTLSAuthzServer(t, ca, policy)
	defer cleanup()

	c, err := client.DialWithOptions(addr, client.Options{
		EnableTLS:  true,
		CAFile:     ca.CertPath,
		CertFile:   writerCert,
		KeyFile:    writerKey,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("DialWithOptions failed: %v", err)
	}
	defer c.Close()

	ctx := context.Background()

	// Put
	if err := c.Put(ctx, []byte("wkey"), []byte("wval")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// Get
	val, err := c.Get(ctx, []byte("wkey"))
	if err != nil || string(val) != "wval" {
		t.Fatalf("Get failed: val=%s err=%v", val, err)
	}

	// Exists
	exists, err := c.Exists(ctx, []byte("wkey"))
	if err != nil || !exists {
		t.Fatalf("Exists failed: exists=%v err=%v", exists, err)
	}

	// Batch
	batch := client.NewWriteBatch()
	batch.Put([]byte("bk"), []byte("bv"))
	batch.Delete([]byte("wkey"))
	if err := c.Batch(ctx, batch); err != nil {
		t.Fatalf("Batch failed: %v", err)
	}

	// Stats
	snap, err := c.Stats(ctx)
	if err != nil || snap == nil {
		t.Fatalf("Stats failed: %v", err)
	}
}

func TestClient_RBAC_Pipeline_Reader(t *testing.T) {
	ca := newClientTestCA(t, "ca")
	readerCert, readerKey := ca.issueCert(t, "reader", true, false, transport.ClientCertRoleOU)
	readerFP := certFPFromFile(t, readerCert)

	policy := map[string]string{readerFP: "reader"}
	addr, cleanup := startTLSAuthzServer(t, ca, policy)
	defer cleanup()

	c, err := client.DialWithOptions(addr, client.Options{
		EnableTLS:  true,
		CAFile:     ca.CertPath,
		CertFile:   readerCert,
		KeyFile:    readerKey,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("DialWithOptions failed: %v", err)
	}
	defer c.Close()

	ctx := context.Background()

	pipe := c.Pipeline()
	fGet := pipe.Get([]byte("k"))
	fPut := pipe.Put([]byte("k"), []byte("v"))
	fExists := pipe.Exists([]byte("k"))
	fDelete := pipe.Delete([]byte("k"))
	batch := client.NewWriteBatch()
	batch.Put([]byte("b1"), []byte("v1"))
	fBatch := pipe.Batch(*batch)
	fStats := pipe.Stats()

	if err := pipe.Execute(ctx); err != nil {
		t.Fatalf("pipe.Execute failed: %v", err)
	}

	// Reads succeed or return not found
	_, errGet := fGet.Result()
	if !errors.Is(errGet, client.ErrKeyNotFound) {
		t.Errorf("fGet: expected ErrKeyNotFound, got %v", errGet)
	}

	exists, errExists := fExists.Result()
	if errExists != nil || exists {
		t.Errorf("fExists: expected false nil, got %v %v", exists, errExists)
	}

	snap, errStats := fStats.Result()
	if errStats != nil || snap == nil {
		t.Errorf("fStats: expected success, got %v", errStats)
	}

	// Writes denied with client.ErrPermissionDenied
	if err := fPut.Result(); !errors.Is(err, client.ErrPermissionDenied) {
		t.Errorf("fPut: expected client.ErrPermissionDenied, got %v", err)
	}

	if err := fDelete.Result(); !errors.Is(err, client.ErrPermissionDenied) {
		t.Errorf("fDelete: expected client.ErrPermissionDenied, got %v", err)
	}

	if err := fBatch.Result(); !errors.Is(err, client.ErrPermissionDenied) {
		t.Errorf("fBatch: expected client.ErrPermissionDenied, got %v", err)
	}
}

func TestClient_RBAC_UnknownCert_Denied(t *testing.T) {
	ca := newClientTestCA(t, "ca")
	unknownCert, unknownKey := ca.issueCert(t, "unknown", true, false, transport.ClientCertRoleOU)

	// Server policy has different cert
	policy := map[string]string{"0000000000000000000000000000000000000000000000000000000000000000": "reader"}
	addr, cleanup := startTLSAuthzServer(t, ca, policy)
	defer cleanup()

	c, err := client.DialWithOptions(addr, client.Options{
		EnableTLS:  true,
		CAFile:     ca.CertPath,
		CertFile:   unknownCert,
		KeyFile:    unknownKey,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("DialWithOptions failed: %v", err)
	}
	defer c.Close()

	ctx := context.Background()

	// Both reads and writes are denied
	_, err = c.Get(ctx, []byte("any"))
	if !errors.Is(err, client.ErrPermissionDenied) {
		t.Fatalf("expected ErrPermissionDenied for unknown cert GET, got %v", err)
	}

	err = c.Put(ctx, []byte("any"), []byte("val"))
	if !errors.Is(err, client.ErrPermissionDenied) {
		t.Fatalf("expected ErrPermissionDenied for unknown cert PUT, got %v", err)
	}
}
