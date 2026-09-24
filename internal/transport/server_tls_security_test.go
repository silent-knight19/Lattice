package transport

import (
	"context"
	"crypto/tls"
	stdErrors "errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// countingEngine wraps mockEngine with an atomic counter tracking total application-level requests processed.
type countingEngine struct {
	*mockEngine
	requests atomic.Int64
}

func newCountingEngine() *countingEngine {
	return &countingEngine{
		mockEngine: newMockEngine(),
	}
}

func (c *countingEngine) Put(ctx context.Context, key, val []byte) error {
	c.requests.Add(1)
	return c.mockEngine.Put(ctx, key, val)
}

func (c *countingEngine) Get(key []byte) ([]byte, error) {
	c.requests.Add(1)
	return c.mockEngine.Get(key)
}

func (c *countingEngine) Delete(ctx context.Context, key []byte) error {
	c.requests.Add(1)
	return c.mockEngine.Delete(ctx, key)
}

func (c *countingEngine) Batch(ctx context.Context, batch []binary.BatchOp) error {
	c.requests.Add(1)
	return c.mockEngine.Batch(ctx, batch)
}

func (c *countingEngine) Exists(key []byte) (bool, error) {
	c.requests.Add(1)
	return c.mockEngine.Exists(key)
}

// setupClientTLSSecurityTest initializes legitimate and attacker PKI environments for regression testing.
func setupClientTLSSecurityTest(t *testing.T) (
	legitCA *TestCA,
	attackerCA *TestCA,
	serverKP tls.Certificate,
	legitClientKP tls.Certificate,
	attackerClientKP tls.Certificate,
	peerKP tls.Certificate,
	legitPool *tls.Config,
) {
	t.Helper()

	legitCA = NewTestCA(t, "Lattice-Legitimate-Client-CA")
	attackerCA = NewTestCA(t, "Attacker-Rogue-Client-CA")

	serverCert, serverKey := legitCA.IssueServerCert(t, "localhost")
	var err error
	serverKP, err = tls.LoadX509KeyPair(serverCert, serverKey)
	if err != nil {
		t.Fatalf("LoadX509KeyPair server failed: %v", err)
	}

	clientCert, clientKey := legitCA.IssueClientCert(t, "client-app-1")
	legitClientKP, err = tls.LoadX509KeyPair(clientCert, clientKey)
	if err != nil {
		t.Fatalf("LoadX509KeyPair legit client failed: %v", err)
	}

	attackerCert, attackerKey := attackerCA.IssueClientCert(t, "rogue-client")
	attackerClientKP, err = tls.LoadX509KeyPair(attackerCert, attackerKey)
	if err != nil {
		t.Fatalf("LoadX509KeyPair attacker client failed: %v", err)
	}

	peerCert, peerKey := legitCA.IssuePeerCert(t, 101)
	peerKP, err = tls.LoadX509KeyPair(peerCert, peerKey)
	if err != nil {
		t.Fatalf("LoadX509KeyPair peer cert failed: %v", err)
	}

	pool, err := LoadCertPool(legitCA.CertPath)
	if err != nil {
		t.Fatalf("LoadCertPool failed: %v", err)
	}

	legitPool = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{serverKP},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}

	return legitCA, attackerCA, serverKP, legitClientKP, attackerClientKP, peerKP, legitPool
}

// TestServer_GetConfigForClientSecurity implements exhaustive handshake-level and application reachability tests.
func TestServer_GetConfigForClientSecurity(t *testing.T) {
	legitCA, attackerCA, serverKP, legitClientKP, attackerClientKP, peerKP, baseValidTLS := setupClientTLSSecurityTest(t)

	attackerPool, err := LoadCertPool(attackerCA.CertPath)
	if err != nil {
		t.Fatalf("LoadCertPool attacker failed: %v", err)
	}
	legitCertPool, err := LoadCertPool(legitCA.CertPath)
	if err != nil {
		t.Fatalf("LoadCertPool legit failed: %v", err)
	}

	// Helper to spawn a test server with non-loopback policy semantics
	startServerWithTLS := func(t *testing.T, srvTLS *tls.Config, eng *countingEngine) (string, func()) {
		t.Helper()
		cfg := DefaultServerConfig()
		cfg.Address = "198.51.100.1:9099" // Non-loopback production address
		cfg.TLSConfig = srvTLS

		srv, err := NewServer(cfg, eng)
		if err != nil {
			t.Fatalf("NewServer failed: %v", err)
		}

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("net.Listen failed: %v", err)
		}

		go func() { _ = srv.Serve(ln) }()

		cleanup := func() {
			_ = srv.Shutdown(context.Background())
			_ = ln.Close()
		}
		return ln.Addr().String(), cleanup
	}

	// =========================================================================
	// Attack 1: Disable client authentication via GetConfigForClient
	// =========================================================================
	t.Run("Attack 1: Disable client authentication => Rejected, 0 Requests", func(t *testing.T) {
		eng := newCountingEngine()
		serverTLS := baseValidTLS.Clone()
		serverTLS.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			return &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{serverKP},
				ClientAuth:   tls.NoClientCert, // Malicious bypass attempt
			}, nil
		}

		addr, cleanup := startServerWithTLS(t, serverTLS, eng)
		defer cleanup()

		// Unauthenticated client (no certificate) dials server
		clientTLS := &tls.Config{
			MinVersion: tls.VersionTLS13,
			RootCAs:    legitCertPool,
			ServerName: "localhost",
		}
		conn, err := tls.Dial("tcp", addr, clientTLS)
		if err == nil {
			_ = conn.Close()
			t.Fatal("SECURITY VIOLATION: Handshake succeeded with unauthenticated client when GetConfigForClient returned NoClientCert")
		}

		// Verify zero application requests processed
		time.Sleep(20 * time.Millisecond)
		if count := eng.requests.Load(); count != 0 {
			t.Fatalf("SECURITY VIOLATION: %d application requests reached engine after failed handshake", count)
		}
	})

	// =========================================================================
	// Attack 2: Weaken authentication (RequestClientCert, VerifyClientCertIfGiven, RequireAnyClientCert)
	// =========================================================================
	t.Run("Attack 2: Weaken authentication => Rejected, 0 Requests", func(t *testing.T) {
		weakModes := []struct {
			name string
			auth tls.ClientAuthType
		}{
			{"RequestClientCert", tls.RequestClientCert},
			{"VerifyClientCertIfGiven", tls.VerifyClientCertIfGiven},
			{"RequireAnyClientCert", tls.RequireAnyClientCert},
		}

		for _, wm := range weakModes {
			t.Run(wm.name, func(t *testing.T) {
				eng := newCountingEngine()
				serverTLS := baseValidTLS.Clone()
				serverTLS.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
					return &tls.Config{
						MinVersion:   tls.VersionTLS13,
						Certificates: []tls.Certificate{serverKP},
						ClientAuth:   wm.auth, // Weakened auth mode
						ClientCAs:    legitCertPool,
					}, nil
				}

				addr, cleanup := startServerWithTLS(t, serverTLS, eng)
				defer cleanup()

				clientTLS := &tls.Config{
					MinVersion: tls.VersionTLS13,
					RootCAs:    legitCertPool,
					ServerName: "localhost",
				}
				conn, err := tls.Dial("tcp", addr, clientTLS)
				if err == nil {
					_ = conn.Close()
					t.Fatalf("SECURITY VIOLATION: Handshake succeeded when GetConfigForClient returned %s", wm.name)
				}

				time.Sleep(20 * time.Millisecond)
				if count := eng.requests.Load(); count != 0 {
					t.Fatalf("SECURITY VIOLATION: %d application requests reached engine after weakened auth", count)
				}
			})
		}
	})

	// =========================================================================
	// Attack 3: Replace trusted CA with attacker-controlled CA
	// =========================================================================
	t.Run("Attack 3: Replace trusted CA => Attacker cert rejected, 0 Requests", func(t *testing.T) {
		eng := newCountingEngine()
		serverTLS := baseValidTLS.Clone()
		serverTLS.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			return &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{serverKP},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    attackerPool, // Malicious rogue CA pool substitution
			}, nil
		}

		addr, cleanup := startServerWithTLS(t, serverTLS, eng)
		defer cleanup()

		// Attacker client connects presenting certificate signed by attacker CA
		clientTLS := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{attackerClientKP},
			RootCAs:      legitCertPool,
			ServerName:   "localhost",
		}
		conn, err := tls.Dial("tcp", addr, clientTLS)
		if err == nil {
			_ = conn.Close()
			t.Fatal("SECURITY VIOLATION: Handshake succeeded with attacker client cert signed by rogue CA via GetConfigForClient")
		}

		time.Sleep(20 * time.Millisecond)
		if count := eng.requests.Load(); count != 0 {
			t.Fatalf("SECURITY VIOLATION: %d application requests reached engine from attacker cert", count)
		}
	})

	// =========================================================================
	// Attack 4: Omit ClientCAs (nil ClientCAs)
	// =========================================================================
	t.Run("Attack 4: Omit ClientCAs => Rejected, 0 Requests", func(t *testing.T) {
		eng := newCountingEngine()
		serverTLS := baseValidTLS.Clone()
		serverTLS.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			return &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{serverKP},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    nil, // Maliciously omitted ClientCAs
			}, nil
		}

		addr, cleanup := startServerWithTLS(t, serverTLS, eng)
		defer cleanup()

		clientTLS := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{legitClientKP},
			RootCAs:      legitCertPool,
			ServerName:   "localhost",
		}
		conn, err := tls.Dial("tcp", addr, clientTLS)
		if err == nil {
			_ = conn.Close()
			t.Fatal("SECURITY VIOLATION: Handshake succeeded when GetConfigForClient returned nil ClientCAs")
		}

		time.Sleep(20 * time.Millisecond)
		if count := eng.requests.Load(); count != 0 {
			t.Fatalf("SECURITY VIOLATION: %d application requests reached engine with nil ClientCAs", count)
		}
	})

	// =========================================================================
	// Attack 5: Remove mandatory VerifyConnection
	// =========================================================================
	t.Run("Attack 5: Remove mandatory VerifyConnection => PKI separation and base verification enforced, 0 Requests", func(t *testing.T) {
		t.Run("5a: Raft peer certificate rejected on client transport even if dynamic VerifyConnection=nil", func(t *testing.T) {
			eng := newCountingEngine()
			serverTLS := baseValidTLS.Clone()
			serverTLS.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
				return &tls.Config{
					MinVersion:       tls.VersionTLS13,
					Certificates:     []tls.Certificate{serverKP},
					ClientAuth:       tls.RequireAndVerifyClientCert,
					ClientCAs:        legitCertPool,
					VerifyConnection: nil, // Maliciously stripped verifier
				}, nil
			}

			addr, cleanup := startServerWithTLS(t, serverTLS, eng)
			defer cleanup()

			// Client presents a valid Raft Peer certificate (OU='Lattice Raft Peer')
			clientTLS := &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{peerKP},
				RootCAs:      legitCertPool,
				ServerName:   "localhost",
			}
			conn, err := tls.Dial("tcp", addr, clientTLS)
			if err == nil {
				defer conn.Close()
				req := &Request{OpCode: OpGet, SeqID: 1, Key: []byte("k")}
				writeErr := WriteRequest(conn, req)
				var readErr error
				if writeErr == nil {
					_, readErr = ReadResponse(conn)
				}
				t.Logf("5a: writeErr=%v, readErr=%v", writeErr, readErr)
				if writeErr == nil && readErr == nil {
					t.Fatal("SECURITY VIOLATION: Raft peer certificate was admitted and request processed when GetConfigForClient returned VerifyConnection=nil")
				}
			}

			time.Sleep(20 * time.Millisecond)
			if count := eng.requests.Load(); count != 0 {
				t.Fatalf("SECURITY VIOLATION: %d requests processed from peer cert", count)
			}
		})

		t.Run("5b: Base caller VerifyConnection cannot be stripped by dynamic config", func(t *testing.T) {
			eng := newCountingEngine()
			serverTLS := baseValidTLS.Clone()
			baseCalled := false
			serverTLS.VerifyConnection = func(cs tls.ConnectionState) error {
				baseCalled = true
				return stdErrors.New("rejected by mandatory base verification policy")
			}
			serverTLS.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
				return &tls.Config{
					MinVersion:       tls.VersionTLS13,
					Certificates:     []tls.Certificate{serverKP},
					ClientAuth:       tls.RequireAndVerifyClientCert,
					ClientCAs:        legitCertPool,
					VerifyConnection: nil, // Attempt to erase base verification
				}, nil
			}

			addr, cleanup := startServerWithTLS(t, serverTLS, eng)
			defer cleanup()

			clientTLS := &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{legitClientKP},
				RootCAs:      legitCertPool,
				ServerName:   "localhost",
			}
			conn, err := tls.Dial("tcp", addr, clientTLS)
			if err == nil {
				defer conn.Close()
				req := &Request{OpCode: OpGet, SeqID: 1, Key: []byte("k")}
				writeErr := WriteRequest(conn, req)
				var readErr error
				if writeErr == nil {
					_, readErr = ReadResponse(conn)
				}
				if writeErr == nil && readErr == nil {
					t.Fatal("SECURITY VIOLATION: Connection succeeded and request processed when dynamic config erased base VerifyConnection")
				}
			}
			time.Sleep(20 * time.Millisecond)
			if !baseCalled {
				t.Fatal("SECURITY VIOLATION: Base VerifyConnection was not invoked")
			}
			if count := eng.requests.Load(); count != 0 {
				t.Fatalf("SECURITY VIOLATION: %d requests processed after base verifier failure", count)
			}
		})
	})

	// =========================================================================
	// Attack 6: TLS downgrade via GetConfigForClient
	// =========================================================================
	t.Run("Attack 6: TLS downgrade to TLS 1.2 => Rejected, 0 Requests", func(t *testing.T) {
		eng := newCountingEngine()
		serverTLS := baseValidTLS.Clone()
		serverTLS.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			return &tls.Config{
				MinVersion:   tls.VersionTLS12,
				MaxVersion:   tls.VersionTLS12, // Downgrade attempt
				Certificates: []tls.Certificate{serverKP},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    legitCertPool,
			}, nil
		}

		addr, cleanup := startServerWithTLS(t, serverTLS, eng)
		defer cleanup()

		clientTLS := &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{legitClientKP},
			RootCAs:      legitCertPool,
			ServerName:   "localhost",
		}
		conn, err := tls.Dial("tcp", addr, clientTLS)
		if err == nil {
			_ = conn.Close()
			t.Fatal("SECURITY VIOLATION: TLS downgrade to 1.2 succeeded via GetConfigForClient")
		}

		time.Sleep(20 * time.Millisecond)
		if count := eng.requests.Load(); count != 0 {
			t.Fatalf("SECURITY VIOLATION: %d application requests reached engine after TLS downgrade", count)
		}
	})

	// =========================================================================
	// Attack 7: Dynamic trust substitution after initial validation
	// =========================================================================
	t.Run("Attack 7: Dynamic trust substitution after constructor => Rejected at runtime TLS boundary", func(t *testing.T) {
		eng := newCountingEngine()
		serverTLS := baseValidTLS.Clone()

		// Base configuration passes constructor validation completely
		if err := ValidateClientServerTLSConfig(serverTLS); err != nil {
			t.Fatalf("Base config unexpectedly failed validation: %v", err)
		}

		serverTLS.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			// At handshake time, dynamic callback substitutes rogue CA
			return &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{serverKP},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    attackerPool,
			}, nil
		}

		addr, cleanup := startServerWithTLS(t, serverTLS, eng)
		defer cleanup()

		clientTLS := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{attackerClientKP},
			RootCAs:      legitCertPool,
			ServerName:   "localhost",
		}
		conn, err := tls.Dial("tcp", addr, clientTLS)
		if err == nil {
			_ = conn.Close()
			t.Fatal("SECURITY VIOLATION: Runtime dynamic trust substitution admitted rogue client cert")
		}

		time.Sleep(20 * time.Millisecond)
		if count := eng.requests.Load(); count != 0 {
			t.Fatalf("SECURITY VIOLATION: %d application requests reached engine after dynamic trust substitution", count)
		}
	})

	// =========================================================================
	// Attack 8: Legitimate dynamic configuration succeeds
	// =========================================================================
	t.Run("Attack 8: Legitimate dynamic configuration => Handshake succeeds, 1 Request processed", func(t *testing.T) {
		eng := newCountingEngine()
		serverTLS := baseValidTLS.Clone()
		dynamicInvoked := false
		serverTLS.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			dynamicInvoked = true
			return &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{serverKP},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    legitCertPool,
			}, nil
		}

		addr, cleanup := startServerWithTLS(t, serverTLS, eng)
		defer cleanup()

		clientTLS := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{legitClientKP},
			RootCAs:      legitCertPool,
			ServerName:   "localhost",
		}
		conn, err := tls.Dial("tcp", addr, clientTLS)
		if err != nil {
			t.Fatalf("Legitimate dynamic TLS connection failed: %v", err)
		}
		defer conn.Close()

		if !dynamicInvoked {
			t.Fatal("GetConfigForClient was not invoked for legitimate client")
		}

		if conn.ConnectionState().Version != tls.VersionTLS13 {
			t.Fatalf("expected negotiated TLS 1.3, got 0x%04x", conn.ConnectionState().Version)
		}

		// Perform real application request
		req := &Request{
			OpCode: OpPut,
			SeqID:  42,
			Key:    []byte("sec_test_key"),
			Value:  []byte("sec_test_value"),
		}
		if err := WriteRequest(conn, req); err != nil {
			t.Fatalf("WriteRequest failed: %v", err)
		}

		resp, err := ReadResponse(conn)
		if err != nil {
			t.Fatalf("ReadResponse failed: %v", err)
		}
		if resp.Status != StatusOk {
			t.Fatalf("expected StatusOk, got %v: %s", resp.Status, resp.Message)
		}

		// Assert exactly 1 request reached the engine handler
		if count := eng.requests.Load(); count != 1 {
			t.Fatalf("expected exactly 1 application request processed, got %d", count)
		}
	})
}

// TestServer_PositiveNegativeMatrix implements the required verification matrix from Section 10.
func TestServer_PositiveNegativeMatrix(t *testing.T) {
	legitCA, attackerCA, serverKP, legitClientKP, attackerClientKP, _, baseValidTLS := setupClientTLSSecurityTest(t)

	attackerPool, err := LoadCertPool(attackerCA.CertPath)
	if err != nil {
		t.Fatalf("LoadCertPool attacker failed: %v", err)
	}
	legitCertPool, err := LoadCertPool(legitCA.CertPath)
	if err != nil {
		t.Fatalf("LoadCertPool legit failed: %v", err)
	}

	eng := newCountingEngine()

	// 1. Non-loopback, TLS absent, insecure false => REJECT
	t.Run("Matrix: Non-loopback, TLS absent, insecure false => REJECT", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.Address = "198.51.100.1:9099"
		cfg.InsecureTransport = false
		_, err := NewServer(cfg, eng)
		if !stdErrors.Is(err, errors.ErrInsecureTransport) {
			t.Fatalf("expected ErrInsecureTransport, got %v", err)
		}
	})

	// 2. Non-loopback, TLS 1.2 => REJECT
	t.Run("Matrix: Non-loopback, TLS 1.2 => REJECT", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.Address = "198.51.100.1:9099"
		cfg.TLSConfig = &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{serverKP},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    legitCertPool,
		}
		_, err := NewServer(cfg, eng)
		if !stdErrors.Is(err, errors.ErrInsecureTransport) {
			t.Fatalf("expected ErrInsecureTransport, got %v", err)
		}
	})

	// 3. Non-loopback, one-way TLS => REJECT
	t.Run("Matrix: Non-loopback, one-way TLS => REJECT", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.Address = "198.51.100.1:9099"
		cfg.TLSConfig = &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{serverKP},
			ClientAuth:   tls.NoClientCert,
		}
		_, err := NewServer(cfg, eng)
		if !stdErrors.Is(err, errors.ErrInsecureTransport) {
			t.Fatalf("expected ErrInsecureTransport, got %v", err)
		}
	})

	// 4. Non-loopback, NoClientCert => REJECT
	t.Run("Matrix: Non-loopback, NoClientCert => REJECT", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.Address = "198.51.100.1:9099"
		cfg.TLSConfig = &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{serverKP},
			ClientAuth:   tls.NoClientCert,
			ClientCAs:    legitCertPool,
		}
		_, err := NewServer(cfg, eng)
		if !stdErrors.Is(err, errors.ErrInsecureTransport) {
			t.Fatalf("expected ErrInsecureTransport, got %v", err)
		}
	})

	// 5. Non-loopback, RequestClientCert => REJECT
	t.Run("Matrix: Non-loopback, RequestClientCert => REJECT", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.Address = "198.51.100.1:9099"
		cfg.TLSConfig = &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{serverKP},
			ClientAuth:   tls.RequestClientCert,
			ClientCAs:    legitCertPool,
		}
		_, err := NewServer(cfg, eng)
		if !stdErrors.Is(err, errors.ErrInsecureTransport) {
			t.Fatalf("expected ErrInsecureTransport, got %v", err)
		}
	})

	// 6. Non-loopback, VerifyClientCertIfGiven => REJECT
	t.Run("Matrix: Non-loopback, VerifyClientCertIfGiven => REJECT", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.Address = "198.51.100.1:9099"
		cfg.TLSConfig = &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{serverKP},
			ClientAuth:   tls.VerifyClientCertIfGiven,
			ClientCAs:    legitCertPool,
		}
		_, err := NewServer(cfg, eng)
		if !stdErrors.Is(err, errors.ErrInsecureTransport) {
			t.Fatalf("expected ErrInsecureTransport, got %v", err)
		}
	})

	// 7. Non-loopback, RequireAnyClientCert => REJECT
	t.Run("Matrix: Non-loopback, RequireAnyClientCert => REJECT", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.Address = "198.51.100.1:9099"
		cfg.TLSConfig = &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{serverKP},
			ClientAuth:   tls.RequireAnyClientCert,
			ClientCAs:    legitCertPool,
		}
		_, err := NewServer(cfg, eng)
		if !stdErrors.Is(err, errors.ErrInsecureTransport) {
			t.Fatalf("expected ErrInsecureTransport, got %v", err)
		}
	})

	// 8. Non-loopback, nil ClientCAs => REJECT
	t.Run("Matrix: Non-loopback, nil ClientCAs => REJECT", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.Address = "198.51.100.1:9099"
		cfg.TLSConfig = &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{serverKP},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    nil,
		}
		_, err := NewServer(cfg, eng)
		if !stdErrors.Is(err, errors.ErrInsecureTransport) {
			t.Fatalf("expected ErrInsecureTransport, got %v", err)
		}
	})

	// 9. Non-loopback, valid mTLS => ACCEPT
	t.Run("Matrix: Non-loopback, valid mTLS => ACCEPT", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.Address = "198.51.100.1:9099"
		cfg.TLSConfig = baseValidTLS.Clone()
		srv, err := NewServer(cfg, eng)
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if srv == nil {
			t.Fatal("expected non-nil server")
		}
	})

	// 10. Dynamic config variations at runtime
	t.Run("Matrix: Dynamic config at runtime", func(t *testing.T) {
		cases := []struct {
			name        string
			dynCfg      func() *tls.Config
			clientKP    *tls.Certificate
			expectAdmin bool
		}{
			{
				name: "Dynamic config -> NoClientCert => REJECT",
				dynCfg: func() *tls.Config {
					return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverKP}, ClientAuth: tls.NoClientCert}
				},
				clientKP:    nil,
				expectAdmin: false,
			},
			{
				name: "Dynamic config -> RequestClientCert => REJECT",
				dynCfg: func() *tls.Config {
					return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverKP}, ClientAuth: tls.RequestClientCert, ClientCAs: legitCertPool}
				},
				clientKP:    nil,
				expectAdmin: false,
			},
			{
				name: "Dynamic config -> RequireAnyClientCert => REJECT",
				dynCfg: func() *tls.Config {
					return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverKP}, ClientAuth: tls.RequireAnyClientCert, ClientCAs: legitCertPool}
				},
				clientKP:    &attackerClientKP,
				expectAdmin: false,
			},
			{
				name: "Dynamic config -> rogue ClientCAs => REJECT",
				dynCfg: func() *tls.Config {
					return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverKP}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: attackerPool}
				},
				clientKP:    &attackerClientKP,
				expectAdmin: false,
			},
			{
				name: "Dynamic config -> nil ClientCAs => REJECT",
				dynCfg: func() *tls.Config {
					return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverKP}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: nil}
				},
				clientKP:    &legitClientKP,
				expectAdmin: false,
			},
			{
				name: "Dynamic config -> removes mandatory verifier => REJECT",
				dynCfg: func() *tls.Config {
					return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverKP}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: legitCertPool, VerifyConnection: nil}
				},
				clientKP:    nil,
				expectAdmin: false,
			},
			{
				name: "Dynamic config -> TLS 1.2 => REJECT",
				dynCfg: func() *tls.Config {
					return &tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12, Certificates: []tls.Certificate{serverKP}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: legitCertPool}
				},
				clientKP:    &legitClientKP,
				expectAdmin: false,
			},
			{
				name: "Dynamic config -> valid mTLS config => ACCEPT",
				dynCfg: func() *tls.Config {
					return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverKP}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: legitCertPool}
				},
				clientKP:    &legitClientKP,
				expectAdmin: true,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				srvEng := newCountingEngine()
				srvTLS := baseValidTLS.Clone()
				srvTLS.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
					return tc.dynCfg(), nil
				}

				cfg := DefaultServerConfig()
				cfg.Address = "198.51.100.1:9099"
				cfg.TLSConfig = srvTLS
				srv, err := NewServer(cfg, srvEng)
				if err != nil {
					t.Fatalf("NewServer failed: %v", err)
				}

				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("net.Listen failed: %v", err)
				}
				defer ln.Close()

				go func() { _ = srv.Serve(ln) }()
				defer func() { _ = srv.Shutdown(context.Background()) }()

				clientTLS := &tls.Config{
					MinVersion: tls.VersionTLS13,
					RootCAs:    legitCertPool,
					ServerName: "localhost",
				}
				if tc.clientKP != nil {
					clientTLS.Certificates = []tls.Certificate{*tc.clientKP}
				}

				conn, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
				if tc.expectAdmin {
					if err != nil {
						t.Fatalf("expected handshake success, got: %v", err)
					}
					defer conn.Close()
					req := &Request{OpCode: OpPut, SeqID: 1, Key: []byte("k"), Value: []byte("v")}
					if err := WriteRequest(conn, req); err != nil {
						t.Fatalf("WriteRequest failed: %v", err)
					}
					resp, err := ReadResponse(conn)
					if err != nil {
						t.Fatalf("ReadResponse failed: %v", err)
					}
					if resp.Status != StatusOk {
						t.Fatalf("expected StatusOk, got: %v", resp.Status)
					}
					if srvEng.requests.Load() != 1 {
						t.Fatalf("expected 1 request, got %d", srvEng.requests.Load())
					}
				} else {
					if err == nil {
						defer conn.Close()
						req := &Request{OpCode: OpGet, SeqID: 1, Key: []byte("k")}
						writeErr := WriteRequest(conn, req)
						var readErr error
						if writeErr == nil {
							_, readErr = ReadResponse(conn)
						}
						if writeErr == nil && readErr == nil {
							t.Fatalf("expected handshake or request rejection for %s, but connection and request succeeded", tc.name)
						}
					}
					time.Sleep(10 * time.Millisecond)
					if srvEng.requests.Load() != 0 {
						t.Fatalf("expected 0 requests, got %d", srvEng.requests.Load())
					}
				}
			})
		}
	})

	// 11. Loopback development behavior => Preserve existing policy
	t.Run("Matrix: Loopback development behavior preserved", func(t *testing.T) {
		// Plaintext loopback
		cfgPlain := DefaultServerConfig()
		cfgPlain.Address = "127.0.0.1:9099"
		srv1, err := NewServer(cfgPlain, eng)
		if err != nil {
			t.Fatalf("expected plaintext loopback success, got %v", err)
		}
		if srv1 == nil {
			t.Fatal("expected non-nil server")
		}

		// Server-only TLS on loopback
		cfgServerTLS := DefaultServerConfig()
		cfgServerTLS.Address = "127.0.0.1:9099"
		cfgServerTLS.TLSConfig = &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{serverKP},
			ClientAuth:   tls.NoClientCert,
		}
		srv2, err := NewServer(cfgServerTLS, eng)
		if err != nil {
			t.Fatalf("expected server-only TLS loopback success, got %v", err)
		}
		if srv2 == nil {
			t.Fatal("expected non-nil server")
		}
	})
}

// TestServer_ListenLifecycle_GetConfigForClientSecurity verifies that the Listen() lifecycle path
// enforces identical security wrapping as Serve(), preventing asymmetric bypasses.
func TestServer_ListenLifecycle_GetConfigForClientSecurity(t *testing.T) {
	legitCA, attackerCA, serverKP, legitClientKP, attackerClientKP, _, baseValidTLS := setupClientTLSSecurityTest(t)

	attackerPool, err := LoadCertPool(attackerCA.CertPath)
	if err != nil {
		t.Fatalf("LoadCertPool attacker failed: %v", err)
	}
	legitCertPool, err := LoadCertPool(legitCA.CertPath)
	if err != nil {
		t.Fatalf("LoadCertPool legit failed: %v", err)
	}

	t.Run("Listen() with GetConfigForClient -> NoClientCert => Rejected, 0 Requests", func(t *testing.T) {
		eng := newCountingEngine()
		srvTLS := baseValidTLS.Clone()
		srvTLS.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			return &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{serverKP},
				ClientAuth:   tls.NoClientCert,
			}, nil
		}

		cfg := DefaultServerConfig()
		cfg.Address = "198.51.100.1:9099" // Non-loopback production address
		cfg.TLSConfig = srvTLS

		srv, err := NewServer(cfg, eng)
		if err != nil {
			t.Fatalf("NewServer failed: %v", err)
		}

		// Listen on local port
		if err := srv.Listen("127.0.0.1:0"); err != nil {
			t.Fatalf("Listen failed: %v", err)
		}
		defer func() { _ = srv.Shutdown(context.Background()) }()

		clientTLS := &tls.Config{
			MinVersion: tls.VersionTLS13,
			RootCAs:    legitCertPool,
			ServerName: "localhost",
		}
		conn, err := tls.Dial("tcp", srv.Addr().String(), clientTLS)
		if err == nil {
			_ = conn.Close()
			t.Fatal("SECURITY VIOLATION: Handshake succeeded on Listen() listener when GetConfigForClient returned NoClientCert")
		}

		time.Sleep(10 * time.Millisecond)
		if eng.requests.Load() != 0 {
			t.Fatalf("expected 0 requests, got %d", eng.requests.Load())
		}
	})

	t.Run("Listen() with GetConfigForClient -> Rogue CA => Rejected, 0 Requests", func(t *testing.T) {
		eng := newCountingEngine()
		srvTLS := baseValidTLS.Clone()
		srvTLS.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			return &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{serverKP},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    attackerPool,
			}, nil
		}

		cfg := DefaultServerConfig()
		cfg.Address = "198.51.100.1:9099"
		cfg.TLSConfig = srvTLS

		srv, err := NewServer(cfg, eng)
		if err != nil {
			t.Fatalf("NewServer failed: %v", err)
		}

		if err := srv.Listen("127.0.0.1:0"); err != nil {
			t.Fatalf("Listen failed: %v", err)
		}
		defer func() { _ = srv.Shutdown(context.Background()) }()

		clientTLS := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{attackerClientKP},
			RootCAs:      legitCertPool,
			ServerName:   "localhost",
		}
		conn, err := tls.Dial("tcp", srv.Addr().String(), clientTLS)
		if err == nil {
			_ = conn.Close()
			t.Fatal("SECURITY VIOLATION: Rogue CA admitted on Listen() listener")
		}

		time.Sleep(10 * time.Millisecond)
		if eng.requests.Load() != 0 {
			t.Fatalf("expected 0 requests, got %d", eng.requests.Load())
		}
	})

	t.Run("Listen() with valid GetConfigForClient => Accepted, 1 Request processed", func(t *testing.T) {
		eng := newCountingEngine()
		srvTLS := baseValidTLS.Clone()
		srvTLS.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			return &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{serverKP},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    legitCertPool,
			}, nil
		}

		cfg := DefaultServerConfig()
		cfg.Address = "198.51.100.1:9099"
		cfg.TLSConfig = srvTLS

		srv, err := NewServer(cfg, eng)
		if err != nil {
			t.Fatalf("NewServer failed: %v", err)
		}

		if err := srv.Listen("127.0.0.1:0"); err != nil {
			t.Fatalf("Listen failed: %v", err)
		}
		defer func() { _ = srv.Shutdown(context.Background()) }()

		clientTLS := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{legitClientKP},
			RootCAs:      legitCertPool,
			ServerName:   "localhost",
		}
		conn, err := tls.Dial("tcp", srv.Addr().String(), clientTLS)
		if err != nil {
			t.Fatalf("expected handshake success on Listen(), got: %v", err)
		}
		defer conn.Close()

		req := &Request{OpCode: OpPut, SeqID: 10, Key: []byte("listen_key"), Value: []byte("listen_val")}
		if err := WriteRequest(conn, req); err != nil {
			t.Fatalf("WriteRequest failed: %v", err)
		}
		resp, err := ReadResponse(conn)
		if err != nil {
			t.Fatalf("ReadResponse failed: %v", err)
		}
		if resp.Status != StatusOk {
			t.Fatalf("expected StatusOk, got: %v", resp.Status)
		}

		if count := eng.requests.Load(); count != 1 {
			t.Fatalf("expected 1 request processed, got %d", count)
		}
	})
}

// TestServer_HandleConn_DefenseInDepth_PlaintextRejected verifies that unencrypted connections
// cannot slip through to application request processing on non-loopback servers.
func TestServer_HandleConn_DefenseInDepth_PlaintextRejected(t *testing.T) {
	_, _, serverKP, _, _, _, baseValidTLS := setupClientTLSSecurityTest(t)

	eng := newCountingEngine()
	cfg := DefaultServerConfig()
	cfg.Address = "198.51.100.1:9099"
	cfg.TLSConfig = baseValidTLS.Clone()
	cfg.TLSConfig.Certificates = []tls.Certificate{serverKP}

	srv, err := NewServer(cfg, eng)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer ln.Close()

	// Run accept loop directly on plain listener to test handleConn defense-in-depth
	go srv.acceptLoop(ln)
	defer func() { _ = srv.Shutdown(context.Background()) }()

	// Plaintext dial
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial failed: %v", err)
	}
	defer conn.Close()

	// Try writing a plaintext request
	req := &Request{OpCode: OpGet, SeqID: 1, Key: []byte("unauthorized")}
	_ = WriteRequest(conn, req)

	time.Sleep(20 * time.Millisecond)
	if count := eng.requests.Load(); count != 0 {
		t.Fatalf("SECURITY VIOLATION: %d plaintext requests reached engine on non-loopback server", count)
	}
}
