package transport

import (
	"context"
	"crypto/tls"
	stdErrors "errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
)

type mockEngine struct {
	mu    sync.RWMutex
	store map[string][]byte
}

func newMockEngine() *mockEngine {
	return &mockEngine{store: make(map[string][]byte)}
}

func (m *mockEngine) Put(ctx context.Context, key, val []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(val))
	copy(cp, val)
	m.store[string(key)] = cp
	return nil
}

func (m *mockEngine) Get(key []byte) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	val, ok := m.store[string(key)]
	if !ok {
		return nil, errors.ErrKeyNotFound
	}
	res := make([]byte, len(val))
	copy(res, val)
	return res, nil
}

func (m *mockEngine) Delete(ctx context.Context, key []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.store, string(key))
	return nil
}

func (m *mockEngine) Batch(ctx context.Context, batch []binary.BatchOp) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, op := range batch {
		if op.Type == binary.OpTypePut {
			cp := make([]byte, len(op.Value))
			copy(cp, op.Value)
			m.store[string(op.Key)] = cp
		} else {
			delete(m.store, string(op.Key))
		}
	}
	return nil
}

func (m *mockEngine) Exists(key []byte) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.store[string(key)]
	return ok, nil
}

func (m *mockEngine) Stats() (EngineStats, MemoryStats, StorageStats, CacheStats, error) {
	return EngineStats{}, MemoryStats{}, StorageStats{}, CacheStats{}, nil
}

// TestTLS_ClientTransport_Comprehensive verifies TLS 1.3 server authentication,
// rejection of TLS 1.2, hostname verification, and CA trust validation.
func TestTLS_ClientTransport_Comprehensive(t *testing.T) {
	ca := NewTestCA(t, "root-ca")
	serverCert, serverKey := ca.IssueServerCert(t, "server")

	eng := newMockEngine()
	cfg := DefaultServerConfig()
	cfg.Address = "127.0.0.1:0"
	cfg.TLSCertFile = serverCert
	cfg.TLSKeyFile = serverKey

	srv, err := NewServer(cfg, eng)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()

	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	addr := ln.Addr().String()

	t.Run("Valid TLS 1.3 Handshake and Command", func(t *testing.T) {
		clientCfg, err := ClientTLSConfig(ca.CertPath, "", "", "localhost", false)
		if err != nil {
			t.Fatalf("failed to create client TLS config: %v", err)
		}

		conn, err := tls.Dial("tcp", addr, clientCfg)
		if err != nil {
			t.Fatalf("failed to dial server over TLS 1.3: %v", err)
		}
		defer conn.Close()

		if conn.ConnectionState().Version != tls.VersionTLS13 {
			t.Fatalf("expected negotiated TLS version 1.3 (0x%04x), got 0x%04x",
				tls.VersionTLS13, conn.ConnectionState().Version)
		}

		// Send a PUT request
		req := &Request{
			OpCode: OpPut,
			SeqID:  1,
			Key:    []byte("tls_key"),
			Value:  []byte("tls_value"),
		}
		if err := WriteRequest(conn, req); err != nil {
			t.Fatalf("write request failed: %v", err)
		}

		resp, err := ReadResponse(conn)
		if err != nil {
			t.Fatalf("read response failed: %v", err)
		}
		if resp.Status != StatusOk || resp.SeqID != 1 {
			t.Fatalf("unexpected response: status=%d, seqID=%d", resp.Status, resp.SeqID)
		}
	})

	t.Run("Reject TLS 1.2 Client", func(t *testing.T) {
		pool, _ := LoadCertPool(ca.CertPath)
		clientCfg := &tls.Config{
			RootCAs:    pool,
			ServerName: "localhost",
			MinVersion: tls.VersionTLS12,
			MaxVersion: tls.VersionTLS12, // Force TLS 1.2
		}

		conn, err := tls.Dial("tcp", addr, clientCfg)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("expected server to reject TLS 1.2 client, but handshake succeeded")
		}
	})

	t.Run("Reject Untrusted CA", func(t *testing.T) {
		foreignCA := NewTestCA(t, "foreign-ca")
		clientCfg, err := ClientTLSConfig(foreignCA.CertPath, "", "", "localhost", false)
		if err != nil {
			t.Fatalf("failed to create client TLS config: %v", err)
		}

		conn, err := tls.Dial("tcp", addr, clientCfg)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("expected dial to fail with untrusted CA")
		}
	})

	t.Run("Reject Hostname Mismatch", func(t *testing.T) {
		clientCfg, err := ClientTLSConfig(ca.CertPath, "", "", "wrong-server-name.com", false)
		if err != nil {
			t.Fatalf("failed to create client TLS config: %v", err)
		}

		conn, err := tls.Dial("tcp", addr, clientCfg)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("expected dial to fail with hostname mismatch")
		}
	})

	t.Run("Reject Plaintext Client", func(t *testing.T) {
		conn, err := net.DialTimeout("tcp", addr, 1*time.Second)
		if err != nil {
			t.Fatalf("failed to dial TCP: %v", err)
		}
		defer conn.Close()

		// Send plaintext application frame
		req := &Request{OpCode: OpPut, SeqID: 1, Key: []byte("k"), Value: []byte("v")}
		_ = WriteRequest(conn, req)

		// Expect immediate EOF or reset
		buf := make([]byte, 100)
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, err = conn.Read(buf)
		if err == nil {
			t.Fatalf("expected plaintext client to be rejected by TLS server")
		}
	})
}

// TestTLS_ClientTransport_mTLS verifies client mutual authentication semantics.
func TestTLS_ClientTransport_mTLS(t *testing.T) {
	ca := NewTestCA(t, "client-ca")
	serverCert, serverKey := ca.IssueServerCert(t, "server")
	validClientCert, validClientKey := ca.IssueClientCert(t, "client-valid")

	foreignCA := NewTestCA(t, "other-ca")
	foreignClientCert, foreignClientKey := foreignCA.IssueClientCert(t, "client-foreign")

	eng := newMockEngine()
	cfg := DefaultServerConfig()
	cfg.Address = "127.0.0.1:0"
	cfg.TLSCertFile = serverCert
	cfg.TLSKeyFile = serverKey
	cfg.ClientCAFile = ca.CertPath
	cfg.RequireClientCert = true // Enforce client mTLS

	srv, err := NewServer(cfg, eng)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()

	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	addr := ln.Addr().String()

	t.Run("Valid Client Certificate Succeeds", func(t *testing.T) {
		clientCfg, err := ClientTLSConfig(ca.CertPath, validClientCert, validClientKey, "localhost", false)
		if err != nil {
			t.Fatalf("failed to create client TLS config: %v", err)
		}

		conn, err := tls.Dial("tcp", addr, clientCfg)
		if err != nil {
			t.Fatalf("mTLS dial failed: %v", err)
		}
		defer conn.Close()

		req := &Request{OpCode: OpPut, SeqID: 10, Key: []byte("mtls_k"), Value: []byte("mtls_v")}
		if err := WriteRequest(conn, req); err != nil {
			t.Fatalf("write failed: %v", err)
		}

		resp, err := ReadResponse(conn)
		if err != nil || resp.Status != StatusOk {
			t.Fatalf("unexpected response: err=%v, resp=%+v", err, resp)
		}
	})

	t.Run("Missing Client Certificate Fails", func(t *testing.T) {
		// Client without certificate
		clientCfg, err := ClientTLSConfig(ca.CertPath, "", "", "localhost", false)
		if err != nil {
			t.Fatalf("failed to create client TLS config: %v", err)
		}

		conn, err := tls.Dial("tcp", addr, clientCfg)
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(1 * time.Second))
			_ = WriteRequest(conn, &Request{OpCode: OpPut, SeqID: 1, Key: []byte("k")})
			_, err = ReadResponse(conn)
			_ = conn.Close()
		}
		if err == nil {
			t.Fatalf("expected server to reject connection without client certificate")
		}
	})

	t.Run("Unauthorized Client CA Fails", func(t *testing.T) {
		// Client certificate signed by unauthorized CA
		clientCfg, err := ClientTLSConfig(ca.CertPath, foreignClientCert, foreignClientKey, "localhost", false)
		if err != nil {
			t.Fatalf("failed to create client TLS config: %v", err)
		}

		conn, err := tls.Dial("tcp", addr, clientCfg)
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(1 * time.Second))
			_ = WriteRequest(conn, &Request{OpCode: OpPut, SeqID: 1, Key: []byte("k")})
			_, err = ReadResponse(conn)
			_ = conn.Close()
		}
		if err == nil {
			t.Fatalf("expected server to reject client signed by unauthorized CA")
		}
	})

	t.Run("Expired Client Certificate Fails", func(t *testing.T) {
		expiredCert, expiredKey := ca.IssueCert(t, "expired-client", CertOptions{
			CommonName:         "expired-client",
			OrganizationalUnit: []string{ClientCertRoleOU},
			NotBefore:          time.Now().Add(-48 * time.Hour),
			NotAfter:           time.Now().Add(-24 * time.Hour), // Expired in past
			IsClient:           true,
		})

		clientCfg, err := ClientTLSConfig(ca.CertPath, expiredCert, expiredKey, "localhost", false)
		if err != nil {
			t.Fatalf("failed to create client TLS config: %v", err)
		}

		conn, err := tls.Dial("tcp", addr, clientCfg)
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(1 * time.Second))
			_ = WriteRequest(conn, &Request{OpCode: OpPut, SeqID: 1, Key: []byte("k")})
			_, err = ReadResponse(conn)
			_ = conn.Close()
		}
		if err == nil {
			t.Fatalf("expected server to reject expired client certificate")
		}
	})

	t.Run("Not Yet Valid Client Certificate Fails", func(t *testing.T) {
		futureCert, futureKey := ca.IssueCert(t, "future-client", CertOptions{
			CommonName:         "future-client",
			OrganizationalUnit: []string{ClientCertRoleOU},
			NotBefore:          time.Now().Add(24 * time.Hour), // Valid in future
			NotAfter:           time.Now().Add(48 * time.Hour),
			IsClient:           true,
		})

		clientCfg, err := ClientTLSConfig(ca.CertPath, futureCert, futureKey, "localhost", false)
		if err != nil {
			t.Fatalf("failed to create client TLS config: %v", err)
		}

		conn, err := tls.Dial("tcp", addr, clientCfg)
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(1 * time.Second))
			_ = WriteRequest(conn, &Request{OpCode: OpPut, SeqID: 1, Key: []byte("k")})
			_, err = ReadResponse(conn)
			_ = conn.Close()
		}
		if err == nil {
			t.Fatalf("expected server to reject not-yet-valid client certificate")
		}
	})
}

// TestTLS_PeerTransport_Comprehensive verifies peer mutual TLS 1.3,
// identity extraction, and rejection of invalid identities.
func TestTLS_PeerTransport_Comprehensive(t *testing.T) {
	ca := NewTestCA(t, "peer-ca")

	// Create 2-node topology
	topo, err := cluster.NewTopology(1, "127.0.0.1:19098", []cluster.PeerConfig{
		{ID: 1, Address: "127.0.0.1:19098"},
		{ID: 2, Address: "127.0.0.1:19099"},
	})
	if err != nil {
		t.Fatalf("failed to create topology: %v", err)
	}

	node1Cert, node1Key := ca.IssuePeerCert(t, 1)
	node2Cert, node2Key := ca.IssuePeerCert(t, 2)

	receivedFrames := make(chan *Frame, 10)
	peerCfg1 := DefaultPeerConnectionConfig()
	peerCfg1.PeerTLSCertFile = node1Cert
	peerCfg1.PeerTLSKeyFile = node1Key
	peerCfg1.PeerCAFile = ca.CertPath
	peerCfg1.OnFrameReceived = func(peerID cluster.NodeID, frame *Frame) {
		receivedFrames <- frame
	}

	mgr1, err := NewPeerConnectionManager(topo, peerCfg1)
	if err != nil {
		t.Fatalf("failed to create peer manager 1: %v", err)
	}
	defer mgr1.Close()

	if err := mgr1.StartListener("127.0.0.1:0"); err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	listenAddr := mgr1.ListenerAddr().String()

	t.Run("Valid Peer Handshake and Frame Exchange", func(t *testing.T) {
		dialerTLS, err := PeerClientTLSConfig(node2Cert, node2Key, ca.CertPath, 1, listenAddr, topo)
		if err != nil {
			t.Fatalf("failed to create peer client TLS config: %v", err)
		}

		conn, err := tls.Dial("tcp", listenAddr, dialerTLS)
		if err != nil {
			t.Fatalf("peer dial failed: %v", err)
		}
		defer conn.Close()

		// Send valid AppendEntries from Node 2
		req := &AppendEntriesRequest{
			Term:         1,
			LeaderID:     2,
			PrevLogIndex: 0,
			PrevLogTerm:  0,
			LeaderCommit: 0,
		}
		frame, err := EncodeAppendEntries(req, 101)
		if err != nil {
			t.Fatalf("failed to encode frame: %v", err)
		}

		if err := EncodeFrame(conn, frame); err != nil {
			t.Fatalf("failed to write frame: %v", err)
		}

		select {
		case f := <-receivedFrames:
			if f.Header.SeqID != 101 {
				t.Fatalf("unexpected frame SeqID: %d", f.Header.SeqID)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timeout waiting for frame delivery to peer manager")
		}
	})

	t.Run("Reject Peer Impersonation (Cert NodeID != Frame LeaderID)", func(t *testing.T) {
		// Peer 2 authenticates with its own cert, but sends an AppendEntries claiming LeaderID = 3
		dialerTLS, err := PeerClientTLSConfig(node2Cert, node2Key, ca.CertPath, 1, listenAddr, topo)
		if err != nil {
			t.Fatalf("failed to create peer client TLS config: %v", err)
		}

		conn, err := tls.Dial("tcp", listenAddr, dialerTLS)
		if err != nil {
			t.Fatalf("peer dial failed: %v", err)
		}
		defer conn.Close()

		// Send AppendEntries with spoofed LeaderID = 3
		req := &AppendEntriesRequest{
			Term:     1,
			LeaderID: 3, // Spoofed! Certificate asserts 2
		}
		frame, err := EncodeAppendEntries(req, 102)
		if err != nil {
			t.Fatalf("failed to encode: %v", err)
		}

		if err := EncodeFrame(conn, frame); err != nil {
			t.Fatalf("failed to send: %v", err)
		}

		// Frame should be rejected and never dispatched to handler
		select {
		case f := <-receivedFrames:
			t.Fatalf("security violation: spoofed frame %v was delivered to consensus!", f)
		case <-time.After(200 * time.Millisecond):
			// Success: rejected
		}
	})

	t.Run("Reject Client Certificate Used as Peer Certificate (PKI Separation)", func(t *testing.T) {
		// Issue a certificate from the SAME CA, but with client role OU (not Raft peer role)
		clientCert, clientKey := ca.IssueClientCert(t, "node-2")

		pool, _ := LoadCertPool(ca.CertPath)
		cert, _ := tls.LoadX509KeyPair(clientCert, clientKey)
		tlsCfg := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			MaxVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			ServerName:   "node-1",
		}

		conn, err := tls.Dial("tcp", listenAddr, tlsCfg)
		if err == nil {
			// Trigger handshake with write
			_ = conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
			req := &AppendEntriesRequest{Term: 1, LeaderID: 2}
			frame, _ := EncodeAppendEntries(req, 103)
			_ = EncodeFrame(conn, frame)
			_ = conn.Close()
		}

		// Verify frame was rejected
		select {
		case f := <-receivedFrames:
			t.Fatalf("security violation: client certificate accepted on peer transport: %v", f)
		case <-time.After(200 * time.Millisecond):
			// Success
		}
	})

	t.Run("Reject Peer Certificate With Unknown Topology NodeID", func(t *testing.T) {
		// Issue a certificate signed by the trusted CA for NodeID 99 (not in topology)
		node99Cert, node99Key := ca.IssuePeerCert(t, 99)

		pool, _ := LoadCertPool(ca.CertPath)
		cert, _ := tls.LoadX509KeyPair(node99Cert, node99Key)
		tlsCfg := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			MaxVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			ServerName:   "node-1",
		}

		conn, err := tls.Dial("tcp", listenAddr, tlsCfg)
		if err == nil {
			req := &AppendEntriesRequest{Term: 1, LeaderID: 99}
			frame, _ := EncodeAppendEntries(req, 104)
			_ = EncodeFrame(conn, frame)
			_ = conn.Close()
		}

		select {
		case f := <-receivedFrames:
			t.Fatalf("security violation: unrecognized node 99 accepted: %v", f)
		case <-time.After(200 * time.Millisecond):
			// Success
		}
	})

	t.Run("Reject Self-Connection Attempt", func(t *testing.T) {
		// Inbound connection presenting node 1 certificate to node 1
		dialerTLS, err := PeerClientTLSConfig(node1Cert, node1Key, ca.CertPath, 1, listenAddr, topo)
		if err != nil {
			t.Fatalf("failed to create peer client TLS config: %v", err)
		}

		conn, err := tls.Dial("tcp", listenAddr, dialerTLS)
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
			req := &AppendEntriesRequest{Term: 1, LeaderID: 1}
			frame, _ := EncodeAppendEntries(req, 105)
			_ = EncodeFrame(conn, frame)
			buf := make([]byte, 100)
			_, err = conn.Read(buf)
			_ = conn.Close()
		}
		if err == nil {
			t.Fatalf("expected self-connection to be rejected")
		}
	})
}

// TestTLS_PipeliningOverTLS validates that TCP request pipelining (Problem #4)
// operates correctly and without corruption over a TLS 1.3 connection.
func TestTLS_PipeliningOverTLS(t *testing.T) {
	ca := NewTestCA(t, "pipeline-ca")
	serverCert, serverKey := ca.IssueServerCert(t, "pipeline-server")

	eng := newMockEngine()
	cfg := DefaultServerConfig()
	cfg.Address = "127.0.0.1:0"
	cfg.TLSCertFile = serverCert
	cfg.TLSKeyFile = serverKey
	cfg.MaxInFlightPerConn = 64

	srv, err := NewServer(cfg, eng)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()

	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	clientCfg, err := ClientTLSConfig(ca.CertPath, "", "", "localhost", false)
	if err != nil {
		t.Fatalf("failed to create client TLS config: %v", err)
	}

	conn, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	// Pipeline 32 concurrent requests
	const numRequests = 32
	for i := 1; i <= numRequests; i++ {
		req := &Request{
			OpCode: OpPut,
			SeqID:  uint64(i),
			Key:    []byte(fmt.Sprintf("pipe_key_%d", i)),
			Value:  []byte(fmt.Sprintf("pipe_val_%d", i)),
		}
		if err := WriteRequest(conn, req); err != nil {
			t.Fatalf("failed to write pipelined request %d: %v", i, err)
		}
	}

	// Read and verify 32 responses
	receivedSeqs := make(map[uint64]bool)
	for i := 1; i <= numRequests; i++ {
		resp, err := ReadResponse(conn)
		if err != nil {
			t.Fatalf("failed to read response %d: %v", i, err)
		}
		if resp.Status != StatusOk {
			t.Fatalf("response %d returned error status %d: %s", i, resp.Status, resp.Message)
		}
		if receivedSeqs[resp.SeqID] {
			t.Fatalf("duplicate response SeqID received: %d", resp.SeqID)
		}
		receivedSeqs[resp.SeqID] = true
	}

	if len(receivedSeqs) != numRequests {
		t.Fatalf("expected %d distinct responses, got %d", numRequests, len(receivedSeqs))
	}
}

// TestTLS_CertificateValidationFailures tests fail-closed behavior on missing,
// unreadable, malformed, or directory paths.
func TestTLS_CertificateValidationFailures(t *testing.T) {
	tempDir := t.TempDir()

	t.Run("Non-existent file", func(t *testing.T) {
		err := ValidateCertificateFile(filepath.Join(tempDir, "missing.crt"))
		if err == nil {
			t.Fatalf("expected error for non-existent file")
		}
	})

	t.Run("Directory instead of file", func(t *testing.T) {
		subDir := filepath.Join(tempDir, "a-dir")
		_ = os.Mkdir(subDir, 0755)
		err := ValidateCertificateFile(subDir)
		if err == nil {
			t.Fatalf("expected error when passing directory")
		}
	})

	t.Run("Empty file", func(t *testing.T) {
		emptyFile := filepath.Join(tempDir, "empty.crt")
		_ = os.WriteFile(emptyFile, []byte{}, 0600)
		err := ValidateCertificateFile(emptyFile)
		if err == nil {
			t.Fatalf("expected error for empty file")
		}
	})

	t.Run("Malformed PEM CA", func(t *testing.T) {
		malformedFile := filepath.Join(tempDir, "bad.crt")
		_ = os.WriteFile(malformedFile, []byte("NOT PEM DATA"), 0600)
		_, err := LoadCertPool(malformedFile)
		if err == nil {
			t.Fatalf("expected error parsing malformed PEM")
		}
	})
}

func TestSecurityPolicy_ClientTransport_Matrix(t *testing.T) {
	ca := NewTestCA(t, "Lattice Policy CA")
	serverCert, serverKey := ca.IssueServerCert(t, "localhost")
	clientCert, clientKey := ca.IssueClientCert(t, "client")
	_ = clientCert
	_ = clientKey

	handler := newMockEngine()
	nonLoopback := "192.168.1.10:9099"
	loopback := "127.0.0.1:9099"

	t.Run("External + No TLS + InsecureTransport=false => Rejected", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.Address = nonLoopback
		cfg.InsecureTransport = false
		_, err := NewServer(cfg, handler)
		if !stdErrors.Is(err, errors.ErrInsecureTransport) {
			t.Fatalf("expected ErrInsecureTransport, got: %v", err)
		}
	})

	t.Run("External + No TLS + InsecureTransport=true => Accepted", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.Address = nonLoopback
		cfg.InsecureTransport = true
		srv, err := NewServer(cfg, handler)
		if err != nil {
			t.Fatalf("expected success, got: %v", err)
		}
		if srv == nil {
			t.Fatal("expected non-nil server")
		}
	})

	t.Run("External + TLS + No Client CA => Rejected", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.Address = nonLoopback
		cfg.TLSCertFile = serverCert
		cfg.TLSKeyFile = serverKey
		cfg.ClientCAFile = ""
		cfg.RequireClientCert = false
		_, err := NewServer(cfg, handler)
		if err == nil {
			t.Fatal("expected error for external listener with TLS but no Client CA, got nil")
		}
		if !stdErrors.Is(err, errors.ErrInsecureTransport) {
			t.Fatalf("expected error wrapping ErrInsecureTransport, got: %v", err)
		}
	})

	t.Run("External + TLS + Client CA + RequireClientCert=false => Rejected", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.Address = nonLoopback
		cfg.TLSCertFile = serverCert
		cfg.TLSKeyFile = serverKey
		cfg.ClientCAFile = ca.CertPath
		cfg.RequireClientCert = false
		_, err := NewServer(cfg, handler)
		if err == nil {
			t.Fatal("expected error for external listener with TLS and ClientCA but RequireClientCert=false, got nil")
		}
		if !stdErrors.Is(err, errors.ErrInsecureTransport) {
			t.Fatalf("expected error wrapping ErrInsecureTransport, got: %v", err)
		}
	})

	t.Run("External + TLS + Client CA + RequireClientCert=true => Accepted", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.Address = nonLoopback
		cfg.TLSCertFile = serverCert
		cfg.TLSKeyFile = serverKey
		cfg.ClientCAFile = ca.CertPath
		cfg.RequireClientCert = true
		srv, err := NewServer(cfg, handler)
		if err != nil {
			t.Fatalf("expected success for external listener with full mTLS, got: %v", err)
		}
		if srv == nil {
			t.Fatal("expected non-nil server")
		}
	})

	t.Run("Loopback + No TLS => Accepted (Local Dev)", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.Address = loopback
		cfg.InsecureTransport = false
		srv, err := NewServer(cfg, handler)
		if err != nil {
			t.Fatalf("expected success for loopback plaintext, got: %v", err)
		}
		if srv == nil {
			t.Fatal("expected non-nil server")
		}
	})

	t.Run("Loopback + TLS Server-Only => Accepted (Local Dev)", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.Address = loopback
		cfg.TLSCertFile = serverCert
		cfg.TLSKeyFile = serverKey
		cfg.ClientCAFile = ""
		cfg.RequireClientCert = false
		srv, err := NewServer(cfg, handler)
		if err != nil {
			t.Fatalf("expected success for loopback server-only TLS, got: %v", err)
		}
		if srv == nil {
			t.Fatal("expected non-nil server")
		}
	})

	t.Run("Loopback + TLS + Client CA + RequireClientCert=true => Accepted", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.Address = loopback
		cfg.TLSCertFile = serverCert
		cfg.TLSKeyFile = serverKey
		cfg.ClientCAFile = ca.CertPath
		cfg.RequireClientCert = true
		srv, err := NewServer(cfg, handler)
		if err != nil {
			t.Fatalf("expected success for loopback mTLS, got: %v", err)
		}
		if srv == nil {
			t.Fatal("expected non-nil server")
		}
	})
}

func TestSecurityPolicy_PeerTransport_Matrix(t *testing.T) {
	ca := NewTestCA(t, "Lattice Peer Policy CA")
	node1Cert, node1Key := ca.IssuePeerCert(t, 1)
	node2Cert, node2Key := ca.IssuePeerCert(t, 2)
	_ = node2Cert
	_ = node2Key

	externalTopo := createTestTopology(t, 1, "127.0.0.1:9098", map[cluster.NodeID]string{
		2: "192.168.1.100:9098",
	})
	loopbackTopo := createTestTopology(t, 1, "127.0.0.1:9098", map[cluster.NodeID]string{
		2: "127.0.0.1:9097",
	})

	t.Run("External Peer + No TLS + InsecureTransport=false => Rejected", func(t *testing.T) {
		cfg := DefaultPeerConnectionConfig()
		cfg.InsecureTransport = false
		_, err := NewPeerConnectionManager(externalTopo, cfg)
		if !stdErrors.Is(err, errors.ErrInsecureTransport) {
			t.Fatalf("expected ErrInsecureTransport, got: %v", err)
		}
	})

	t.Run("External Peer + No TLS + InsecureTransport=true => Rejected (Finding B)", func(t *testing.T) {
		cfg := DefaultPeerConnectionConfig()
		cfg.InsecureTransport = true
		_, err := NewPeerConnectionManager(externalTopo, cfg)
		if !stdErrors.Is(err, errors.ErrInsecureTransport) {
			t.Fatalf("expected ErrInsecureTransport for non-loopback peer even with InsecureTransport=true, got: %v", err)
		}
	})

	t.Run("External Peer + Peer mTLS => Accepted", func(t *testing.T) {
		cfg := DefaultPeerConnectionConfig()
		cfg.PeerTLSCertFile = node1Cert
		cfg.PeerTLSKeyFile = node1Key
		cfg.PeerCAFile = ca.CertPath
		mgr, err := NewPeerConnectionManager(externalTopo, cfg)
		if err != nil {
			t.Fatalf("expected success with peer mTLS, got: %v", err)
		}
		_ = mgr.Close()
	})

	t.Run("External Peer + Peer mTLS + InsecureTransport=true => Accepted (Secure)", func(t *testing.T) {
		cfg := DefaultPeerConnectionConfig()
		cfg.InsecureTransport = true
		cfg.PeerTLSCertFile = node1Cert
		cfg.PeerTLSKeyFile = node1Key
		cfg.PeerCAFile = ca.CertPath
		mgr, err := NewPeerConnectionManager(externalTopo, cfg)
		if err != nil {
			t.Fatalf("expected success with peer mTLS, got: %v", err)
		}
		_ = mgr.Close()
	})

	t.Run("Loopback Peer + No TLS + InsecureTransport=true => Accepted (Local Dev)", func(t *testing.T) {
		cfg := DefaultPeerConnectionConfig()
		cfg.InsecureTransport = true
		mgr, err := NewPeerConnectionManager(loopbackTopo, cfg)
		if err != nil {
			t.Fatalf("expected success for loopback plaintext, got: %v", err)
		}
		_ = mgr.Close()
	})

	t.Run("External Peer Listener without TLS => Rejected", func(t *testing.T) {
		cfg := DefaultPeerConnectionConfig()
		cfg.InsecureTransport = true
		mgr, err := NewPeerConnectionManager(loopbackTopo, cfg)
		if err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		defer mgr.Close()

		err = mgr.StartListener("192.168.1.10:9098")
		if err == nil {
			t.Fatal("expected error binding external peer listener without TLS, got nil")
		}
		if !stdErrors.Is(err, errors.ErrInsecureTransport) {
			t.Fatalf("expected ErrInsecureTransport, got: %v", err)
		}
	})
}

func TestSecurityPolicy_ClientMTLS_PeerCertRejection(t *testing.T) {
	ca := NewTestCA(t, "Lattice Separation CA")
	serverCert, serverKey := ca.IssueServerCert(t, "localhost")
	peerCert, peerKey := ca.IssuePeerCert(t, 1)

	handler := newMockEngine()
	srv, err := NewServer(ServerConfig{
		Address:           "127.0.0.1:0",
		TLSCertFile:       serverCert,
		TLSKeyFile:        serverKey,
		ClientCAFile:      ca.CertPath,
		RequireClientCert: true,
	}, handler)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer ln.Close()

	go func() { _ = srv.Serve(ln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	// Connect presenting the Raft peer certificate as a client certificate
	clientTLS, err := ClientTLSConfig(ca.CertPath, peerCert, peerKey, "localhost", false)
	if err != nil {
		t.Fatalf("ClientTLSConfig failed: %v", err)
	}

	conn, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
	if err == nil {
		// Handshake should fail either on dial or on first write/read
		_ = WriteRequest(conn, &Request{OpCode: OpGet, SeqID: 1, Key: []byte("k")})
		_, readErr := ReadResponse(conn)
		if readErr == nil {
			t.Fatal("expected peer cert to be rejected on client mTLS transport, but read succeeded")
		}
		_ = conn.Close()
	}
}
