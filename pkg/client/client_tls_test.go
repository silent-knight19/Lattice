package client_test

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/transport"
	"github.com/silent-knight19/lattice/pkg/client"
)

func startTLSServer(t *testing.T, requireClientCert bool) (srv *transport.Server, addr string, caPath, serverCert, serverKey, clientCert, clientKey string, cleanup func()) {
	t.Helper()
	ca := newClientTestCA(t, "sdk-test-ca")
	sCert, sKey := ca.issueCert(t, "server", false, true, "Lattice Server")
	cCert, cKey := ca.issueCert(t, "client", true, false, transport.ClientCertRoleOU)

	dir := t.TempDir()
	eng := engine.NewEngineWithOptions(engine.EngineOptions{DBPath: dir})
	if err := eng.Open(); err != nil {
		t.Fatalf("failed to open engine: %v", err)
	}

	cfg := transport.DefaultServerConfig()
	cfg.Address = "127.0.0.1:0"
	cfg.TLSCertFile = sCert
	cfg.TLSKeyFile = sKey
	if requireClientCert {
		cfg.ClientCAFile = ca.CertPath
		cfg.RequireClientCert = true
	}

	s, err := transport.NewServer(cfg, eng)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	go func() { _ = s.Serve(ln) }()

	cleanupFn := func() {
		_ = s.Shutdown(context.Background())
		_ = ln.Close()
		_ = eng.Close()
	}

	return s, ln.Addr().String(), ca.CertPath, sCert, sKey, cCert, cKey, cleanupFn
}

// TestClient_TLS_ServerAuthentication verifies client SDK establishing a secure TLS 1.3
// connection with server authentication and executing all standard client operations.
func TestClient_TLS_ServerAuthentication(t *testing.T) {
	_, addr, caPath, _, _, _, _, cleanup := startTLSServer(t, false)
	defer cleanup()

	opts := client.Options{
		Timeout:    3 * time.Second,
		EnableTLS:  true,
		CAFile:     caPath,
		ServerName: "localhost",
	}

	c, err := client.DialWithOptions(addr, opts)
	if err != nil {
		t.Fatalf("failed to dial TLS server: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 1. Put
	if err := c.Put(ctx, []byte("tls-key"), []byte("tls-value")); err != nil {
		t.Fatalf("Put over TLS failed: %v", err)
	}

	// 2. Get
	val, err := c.Get(ctx, []byte("tls-key"))
	if err != nil {
		t.Fatalf("Get over TLS failed: %v", err)
	}
	if string(val) != "tls-value" {
		t.Fatalf("expected 'tls-value', got %q", string(val))
	}

	// 3. Exists
	exists, err := c.Exists(ctx, []byte("tls-key"))
	if err != nil || !exists {
		t.Fatalf("Exists over TLS failed: err=%v, exists=%v", err, exists)
	}

	// 4. Batch
	batch := client.NewWriteBatch()
	batch.Put([]byte("b1"), []byte("v1"))
	batch.Put([]byte("b2"), []byte("v2"))
	batch.Delete([]byte("tls-key"))
	if err := c.Batch(ctx, batch); err != nil {
		t.Fatalf("Batch over TLS failed: %v", err)
	}

	// Verify key was deleted
	_, err = c.Get(ctx, []byte("tls-key"))
	if err != client.ErrKeyNotFound {
		t.Fatalf("expected ErrKeyNotFound, got %v", err)
	}

	// 5. Stats
	stats, err := c.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats over TLS failed: %v", err)
	}
	if stats == nil {
		t.Fatalf("expected non-nil stats over TLS")
	}

	// 6. Delete
	if err := c.Delete(ctx, []byte("b1")); err != nil {
		t.Fatalf("Delete over TLS failed: %v", err)
	}
}

// TestClient_mTLS_Authentication verifies client SDK establishing mutual TLS (mTLS).
func TestClient_mTLS_Authentication(t *testing.T) {
	_, addr, caPath, _, _, clientCert, clientKey, cleanup := startTLSServer(t, true)
	defer cleanup()

	t.Run("Valid Client Cert Succeeds", func(t *testing.T) {
		opts := client.Options{
			Timeout:    3 * time.Second,
			CAFile:     caPath,
			CertFile:   clientCert,
			KeyFile:    clientKey,
			ServerName: "localhost",
		}

		c, err := client.DialWithOptions(addr, opts)
		if err != nil {
			t.Fatalf("mTLS dial failed: %v", err)
		}
		defer c.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		if err := c.Put(ctx, []byte("mtls-sdk-key"), []byte("mtls-sdk-val")); err != nil {
			t.Fatalf("Put over mTLS failed: %v", err)
		}

		val, err := c.Get(ctx, []byte("mtls-sdk-key"))
		if err != nil || string(val) != "mtls-sdk-val" {
			t.Fatalf("Get over mTLS failed: val=%s, err=%v", string(val), err)
		}
	})

	t.Run("Missing Client Cert Fails", func(t *testing.T) {
		opts := client.Options{
			Timeout:    1 * time.Second,
			CAFile:     caPath,
			ServerName: "localhost",
			// No CertFile/KeyFile
		}

		c, err := client.DialWithOptions(addr, opts)
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()
			err = c.Put(ctx, []byte("k"), []byte("v"))
			_ = c.Close()
		}
		if err == nil {
			t.Fatalf("expected server to reject connection without client certificate")
		}
	})
}

// TestClient_Pipeline_Over_TLS verifies SDK Pipeline execution over TLS 1.3 and mTLS.
func TestClient_Pipeline_Over_TLS(t *testing.T) {
	_, addr, caPath, _, _, clientCert, clientKey, cleanup := startTLSServer(t, true)
	defer cleanup()

	opts := client.Options{
		Timeout:    5 * time.Second,
		CAFile:     caPath,
		CertFile:   clientCert,
		KeyFile:    clientKey,
		ServerName: "localhost",
	}

	c, err := client.DialWithOptions(addr, opts)
	if err != nil {
		t.Fatalf("mTLS dial failed: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p := c.Pipeline()
	const count = 25
	futures := make([]*client.PutFuture, count)
	for i := 0; i < count; i++ {
		futures[i] = p.Put([]byte(fmt.Sprintf("pk-%d", i+1)), []byte(fmt.Sprintf("pv-%d", i+1)))
	}

	if err := p.Execute(ctx); err != nil {
		t.Fatalf("Pipeline.Execute failed over mTLS: %v", err)
	}

	for i, f := range futures {
		if err := f.Result(); err != nil {
			t.Fatalf("pipeline future %d failed: %v", i+1, err)
		}
	}
}
