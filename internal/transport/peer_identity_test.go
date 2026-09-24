package transport

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cluster"
)

func runTestHandshake(t *testing.T, serverTLS, clientTLS *tls.Config) (serverErr error, clientErr error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()

	serverDone := make(chan error, 1)
	clientDone := make(chan error, 1)

	var sConn net.Conn
	var cConn net.Conn

	go func() {
		var aErr error
		sConn, aErr = ln.Accept()
		if aErr != nil {
			serverDone <- aErr
			return
		}
		serverTLSConn := tls.Server(sConn, serverTLS)
		serverDone <- serverTLSConn.Handshake()
	}()

	go func() {
		var dErr error
		cConn, dErr = net.Dial("tcp", ln.Addr().String())
		if dErr != nil {
			clientDone <- dErr
			return
		}
		clientTLSConn := tls.Client(cConn, clientTLS)
		clientDone <- clientTLSConn.Handshake()
	}()

	select {
	case serverErr = <-serverDone:
	case <-time.After(3 * time.Second):
		serverErr = errors.New("server handshake timed out")
	}

	select {
	case clientErr = <-clientDone:
	case <-time.After(3 * time.Second):
		clientErr = errors.New("client handshake timed out")
	}

	if sConn != nil {
		_ = sConn.Close()
	}
	if cConn != nil {
		_ = cConn.Close()
	}

	return serverErr, clientErr
}

func TestPeerIdentity_InboundMatrix(t *testing.T) {
	ca := NewTestCA(t, "Lattice Inbound Identity CA")
	untrustedCA := NewTestCA(t, "Untrusted External CA")

	// Local node is 1; topology contains nodes 1, 2, 3
	topo := createTestTopology(t, 1, "127.0.0.1:19098", map[cluster.NodeID]string{
		2: "127.0.0.1:19099",
		3: "127.0.0.1:19100",
	})

	node1Cert, node1Key := ca.IssuePeerCert(t, 1)
	node2Cert, node2Key := ca.IssuePeerCert(t, 2)
	node3Cert, node3Key := ca.IssuePeerCert(t, 3)
	_ = node3Cert
	_ = node3Key

	serverTLS, err := PeerServerTLSConfig(node1Cert, node1Key, ca.CertPath, topo)
	if err != nil {
		t.Fatalf("PeerServerTLSConfig failed: %v", err)
	}

	// Case 1: Valid peer certificate + correct NodeID (Node 2) -> accepted
	t.Run("Inbound Case 1: Valid peer cert (Node 2) => Accepted", func(t *testing.T) {
		topo2 := createTestTopology(t, 2, "127.0.0.1:19099", map[cluster.NodeID]string{1: "127.0.0.1:19098"})
		clientTLS, err := PeerClientTLSConfig(node2Cert, node2Key, ca.CertPath, 1, "127.0.0.1:19098", topo2)
		if err != nil {
			t.Fatalf("PeerClientTLSConfig failed: %v", err)
		}

		sErr, cErr := runTestHandshake(t, serverTLS, clientTLS)
		if sErr != nil || cErr != nil {
			t.Fatalf("expected successful handshake, got serverErr=%v, clientErr=%v", sErr, cErr)
		}
	})

	// Case 2: Ordinary client certificate -> rejected
	t.Run("Inbound Case 2: Ordinary client cert => Rejected", func(t *testing.T) {
		clientCert, clientKey := ca.IssueClientCert(t, "ordinary-client")
		clientTLS, err := ClientTLSConfig(ca.CertPath, clientCert, clientKey, "node-1", false)
		if err != nil {
			t.Fatalf("ClientTLSConfig failed: %v", err)
		}

		sErr, _ := runTestHandshake(t, serverTLS, clientTLS)
		if sErr == nil {
			t.Fatal("expected server to reject ordinary client cert, got nil error")
		}
	})

	// Case 3: Certificate with missing peer role -> rejected
	t.Run("Inbound Case 3: Missing peer role => Rejected", func(t *testing.T) {
		noRoleCert, noRoleKey := ca.IssueCert(t, "no-role", CertOptions{
			CommonName:         "node-2",
			Organization:       []string{"Other Org"},
			OrganizationalUnit: []string{"Engineers"},
			IsServer:           true,
			IsClient:           true,
		})
		kp, _ := tls.LoadX509KeyPair(noRoleCert, noRoleKey)
		pool, _ := LoadCertPool(ca.CertPath)
		clientTLS := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{kp},
			RootCAs:      pool,
			ServerName:   "node-1",
		}

		sErr, _ := runTestHandshake(t, serverTLS, clientTLS)
		if sErr == nil {
			t.Fatal("expected server to reject certificate missing peer role, got nil error")
		}
	})

	// Case 4: Certificate signed by untrusted CA -> rejected
	t.Run("Inbound Case 4: Untrusted CA => Rejected", func(t *testing.T) {
		rogueCert, rogueKey := untrustedCA.IssuePeerCert(t, 2)
		kp, _ := tls.LoadX509KeyPair(rogueCert, rogueKey)
		pool, _ := LoadCertPool(ca.CertPath)
		clientTLS := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{kp},
			RootCAs:      pool,
			ServerName:   "node-1",
		}

		sErr, _ := runTestHandshake(t, serverTLS, clientTLS)
		if sErr == nil {
			t.Fatal("expected server to reject untrusted CA cert, got nil error")
		}
	})

	// Case 5: Certificate with unknown NodeID (Node 999) -> rejected
	t.Run("Inbound Case 5: Unknown NodeID (999) => Rejected", func(t *testing.T) {
		node999Cert, node999Key := ca.IssuePeerCert(t, 999)
		kp, _ := tls.LoadX509KeyPair(node999Cert, node999Key)
		pool, _ := LoadCertPool(ca.CertPath)
		clientTLS := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{kp},
			RootCAs:      pool,
			ServerName:   "node-1",
		}

		sErr, _ := runTestHandshake(t, serverTLS, clientTLS)
		if sErr == nil {
			t.Fatal("expected server to reject unknown NodeID cert, got nil error")
		}
	})

	// Case 6: Certificate with local/self NodeID (Node 1) -> rejected
	t.Run("Inbound Case 6: Self NodeID (1) => Rejected", func(t *testing.T) {
		selfCert, selfKey := ca.IssuePeerCert(t, 1)
		kp, _ := tls.LoadX509KeyPair(selfCert, selfKey)
		pool, _ := LoadCertPool(ca.CertPath)
		clientTLS := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{kp},
			RootCAs:      pool,
			ServerName:   "node-1",
		}

		sErr, _ := runTestHandshake(t, serverTLS, clientTLS)
		if sErr == nil {
			t.Fatal("expected server to reject self-connection cert, got nil error")
		}
	})

	// Case 7: Certificate NodeID differs from first Raft frame NodeID -> rejected
	t.Run("Inbound Case 7: Cert NodeID != First Frame LeaderID => Rejected", func(t *testing.T) {
		receivedFrames := make(chan *Frame, 1)
		mgr, err := NewPeerConnectionManager(topo, PeerConnectionConfig{
			PeerTLSCertFile: node1Cert,
			PeerTLSKeyFile:  node1Key,
			PeerCAFile:      ca.CertPath,
			OnFrameReceived: func(peerID cluster.NodeID, frame *Frame) {
				receivedFrames <- frame
			},
		})
		if err != nil {
			t.Fatalf("failed to create manager: %v", err)
		}
		defer mgr.Close()

		if err := mgr.StartListener("127.0.0.1:0"); err != nil {
			t.Fatalf("StartListener failed: %v", err)
		}
		listenAddr := mgr.ListenerAddr().String()

		topo2 := createTestTopology(t, 2, "127.0.0.1:19099", map[cluster.NodeID]string{1: "127.0.0.1:19098"})
		clientTLS, err := PeerClientTLSConfig(node2Cert, node2Key, ca.CertPath, 1, listenAddr, topo2)
		if err != nil {
			t.Fatalf("PeerClientTLSConfig failed: %v", err)
		}

		conn, err := tls.Dial("tcp", listenAddr, clientTLS)
		if err != nil {
			t.Fatalf("tls.Dial failed: %v", err)
		}
		defer conn.Close()

		// Frame claims LeaderID = 3 while certificate asserted NodeID = 2
		req := &AppendEntriesRequest{
			Term:     1,
			LeaderID: 3,
		}
		frame, _ := EncodeAppendEntries(req, 101)
		if err := EncodeFrame(conn, frame); err != nil {
			t.Fatalf("failed to send frame: %v", err)
		}

		// Verify connection was dropped and frame was never delivered
		select {
		case f := <-receivedFrames:
			t.Fatalf("security violation: spoofed frame was delivered: %+v", f)
		case <-time.After(200 * time.Millisecond):
			// Passed: spoofed frame rejected
		}
	})

	// Case 8: Expired certificate -> rejected
	t.Run("Inbound Case 8: Expired cert => Rejected", func(t *testing.T) {
		expiredCert, expiredKey := ca.IssueCert(t, "expired-peer", CertOptions{
			CommonName:         "node-2",
			Organization:       []string{"Lattice Cluster"},
			OrganizationalUnit: []string{PeerCertRoleOU},
			IsServer:           true,
			IsClient:           true,
			NotBefore:          time.Now().Add(-2 * time.Hour),
			NotAfter:           time.Now().Add(-1 * time.Hour),
		})
		kp, _ := tls.LoadX509KeyPair(expiredCert, expiredKey)
		pool, _ := LoadCertPool(ca.CertPath)
		clientTLS := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{kp},
			RootCAs:      pool,
			ServerName:   "node-1",
		}

		sErr, _ := runTestHandshake(t, serverTLS, clientTLS)
		if sErr == nil {
			t.Fatal("expected server to reject expired certificate, got nil error")
		}
	})
}

func TestPeerIdentity_OutboundMatrix(t *testing.T) {
	ca := NewTestCA(t, "Lattice Outbound Identity CA")
	untrustedCA := NewTestCA(t, "Untrusted External CA 2")

	// Local node is 1; topology contains nodes 1, 2, 3
	topo := createTestTopology(t, 1, "127.0.0.1:19098", map[cluster.NodeID]string{
		2: "127.0.0.1:19099",
		3: "127.0.0.1:19100",
	})

	node1Cert, node1Key := ca.IssuePeerCert(t, 1)
	node2Cert, node2Key := ca.IssuePeerCert(t, 2)
	node3Cert, node3Key := ca.IssuePeerCert(t, 3)

	// Dialing expected target Node 2
	clientTLS, err := PeerClientTLSConfig(node1Cert, node1Key, ca.CertPath, 2, "127.0.0.1:19099", topo)
	if err != nil {
		t.Fatalf("PeerClientTLSConfig failed: %v", err)
	}

	// Case 1: Valid peer certificate for expected NodeID (Node 2) -> accepted
	t.Run("Outbound Case 1: Valid peer cert for expected Node 2 => Accepted", func(t *testing.T) {
		topo2 := createTestTopology(t, 2, "127.0.0.1:19099", map[cluster.NodeID]string{1: "127.0.0.1:19098"})
		serverTLS, err := PeerServerTLSConfig(node2Cert, node2Key, ca.CertPath, topo2)
		if err != nil {
			t.Fatalf("PeerServerTLSConfig failed: %v", err)
		}

		sErr, cErr := runTestHandshake(t, serverTLS, clientTLS)
		if sErr != nil || cErr != nil {
			t.Fatalf("expected successful handshake, got serverErr=%v, clientErr=%v", sErr, cErr)
		}
	})

	// Case 2: Valid certificate for different topology NodeID (Node 3) -> rejected
	t.Run("Outbound Case 2: Server presents Node 3 cert when dialing Node 2 => Rejected", func(t *testing.T) {
		topo3 := createTestTopology(t, 3, "127.0.0.1:19100", map[cluster.NodeID]string{1: "127.0.0.1:19098"})
		serverTLS, err := PeerServerTLSConfig(node3Cert, node3Key, ca.CertPath, topo3)
		if err != nil {
			t.Fatalf("PeerServerTLSConfig failed: %v", err)
		}

		_, cErr := runTestHandshake(t, serverTLS, clientTLS)
		if cErr == nil {
			t.Fatal("expected client dialer to reject NodeID mismatch (expected 2, got 3), got nil error")
		}
	})

	// Case 3: Valid certificate for unknown NodeID (Node 999) -> rejected
	t.Run("Outbound Case 3: Server presents unknown Node 999 cert => Rejected", func(t *testing.T) {
		node999Cert, node999Key := ca.IssuePeerCert(t, 999)
		kp, _ := tls.LoadX509KeyPair(node999Cert, node999Key)
		pool, _ := LoadCertPool(ca.CertPath)
		serverTLS := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{kp},
			ClientCAs:    pool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
		}

		_, cErr := runTestHandshake(t, serverTLS, clientTLS)
		if cErr == nil {
			t.Fatal("expected client dialer to reject unknown Node 999 cert, got nil error")
		}
	})

	// Case 4: Ordinary client certificate -> rejected
	t.Run("Outbound Case 4: Server presents ordinary client cert => Rejected", func(t *testing.T) {
		clientCert, clientKey := ca.IssueClientCert(t, "node-2")
		kp, _ := tls.LoadX509KeyPair(clientCert, clientKey)
		pool, _ := LoadCertPool(ca.CertPath)
		serverTLS := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{kp},
			ClientCAs:    pool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
		}

		_, cErr := runTestHandshake(t, serverTLS, clientTLS)
		if cErr == nil {
			t.Fatal("expected client dialer to reject ordinary client cert, got nil error")
		}
	})

	// Case 5: Certificate missing peer role -> rejected
	t.Run("Outbound Case 5: Server presents cert missing peer role => Rejected", func(t *testing.T) {
		noRoleCert, noRoleKey := ca.IssueCert(t, "no-role-node2", CertOptions{
			CommonName:         "node-2",
			Organization:       []string{"Lattice Cluster"},
			OrganizationalUnit: []string{"Finance"},
			IsServer:           true,
			IsClient:           true,
		})
		kp, _ := tls.LoadX509KeyPair(noRoleCert, noRoleKey)
		pool, _ := LoadCertPool(ca.CertPath)
		serverTLS := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{kp},
			ClientCAs:    pool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
		}

		_, cErr := runTestHandshake(t, serverTLS, clientTLS)
		if cErr == nil {
			t.Fatal("expected client dialer to reject cert missing peer role, got nil error")
		}
	})

	// Case 6: Self certificate (Node 1) -> rejected
	t.Run("Outbound Case 6: Server presents local node's own cert (Node 1) => Rejected", func(t *testing.T) {
		kp, _ := tls.LoadX509KeyPair(node1Cert, node1Key)
		pool, _ := LoadCertPool(ca.CertPath)
		serverTLS := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{kp},
			ClientCAs:    pool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
		}

		_, cErr := runTestHandshake(t, serverTLS, clientTLS)
		if cErr == nil {
			t.Fatal("expected client dialer to reject self-cert presentation, got nil error")
		}
	})

	// Case 7: Wrong SAN/CN identity -> rejected
	t.Run("Outbound Case 7: Server presents wrong SAN/CN identity => Rejected", func(t *testing.T) {
		badCert, badKey := ca.IssueCert(t, "bad-identity", CertOptions{
			CommonName:         "database-server",
			Organization:       []string{"Lattice Cluster"},
			OrganizationalUnit: []string{PeerCertRoleOU},
			IsServer:           true,
			IsClient:           true,
		})
		kp, _ := tls.LoadX509KeyPair(badCert, badKey)
		pool, _ := LoadCertPool(ca.CertPath)
		serverTLS := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{kp},
			ClientCAs:    pool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
		}

		_, cErr := runTestHandshake(t, serverTLS, clientTLS)
		if cErr == nil {
			t.Fatal("expected client dialer to reject certificate with unparseable NodeID, got nil error")
		}
	})

	// Case 8: Untrusted CA -> rejected
	t.Run("Outbound Case 8: Server cert signed by untrusted CA => Rejected", func(t *testing.T) {
		rogueCert, rogueKey := untrustedCA.IssuePeerCert(t, 2)
		kp, _ := tls.LoadX509KeyPair(rogueCert, rogueKey)
		pool, _ := LoadCertPool(ca.CertPath)
		serverTLS := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{kp},
			ClientCAs:    pool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
		}

		_, cErr := runTestHandshake(t, serverTLS, clientTLS)
		if cErr == nil {
			t.Fatal("expected client dialer to reject untrusted CA cert, got nil error")
		}
	})
}

func TestPeerIdentity_ProgrammaticConfig_CriticalRegressions(t *testing.T) {
	ca := NewTestCA(t, "Programmatic Regression CA")
	node1Cert, node1Key := ca.IssuePeerCert(t, 1)
	node2Cert, node2Key := ca.IssuePeerCert(t, 2)
	node3Cert, node3Key := ca.IssuePeerCert(t, 3)

	kp1, _ := tls.LoadX509KeyPair(node1Cert, node1Key)
	kp2, _ := tls.LoadX509KeyPair(node2Cert, node2Key)
	kp3, _ := tls.LoadX509KeyPair(node3Cert, node3Key)
	caPool, _ := LoadCertPool(ca.CertPath)

	topo := createTestTopology(t, 1, "127.0.0.1:19098", map[cluster.NodeID]string{
		2: "127.0.0.1:19099",
	})

	// Case A: Programmatic inbound mTLS without VerifyConnection (VerifyConnection = nil)
	// Proves the manager injects the Lattice identity validator automatically.
	t.Run("Case A: Programmatic inbound mTLS with nil VerifyConnection injects validator", func(t *testing.T) {
		cfg := DefaultPeerConnectionConfig()
		cfg.ListenerTLSConfig = &tls.Config{
			MinVersion:       tls.VersionTLS13,
			ClientAuth:       tls.RequireAndVerifyClientCert,
			ClientCAs:        caPool,
			Certificates:     []tls.Certificate{kp1},
			VerifyConnection: nil, // Caller omitted verification callback
		}

		mgr, err := NewPeerConnectionManager(topo, cfg)
		if err != nil {
			t.Fatalf("failed to create manager: %v", err)
		}
		defer mgr.Close()

		if err := mgr.StartListener("127.0.0.1:0"); err != nil {
			t.Fatalf("StartListener failed: %v", err)
		}
		listenAddr := mgr.ListenerAddr().String()

		// 1. Connect with valid peer cert (Node 2) -> accepted
		dialerValid := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{kp2},
			RootCAs:      caPool,
			ServerName:   "node-1",
		}
		conn1, err := tls.Dial("tcp", listenAddr, dialerValid)
		if err != nil {
			t.Fatalf("expected valid peer cert to succeed, got: %v", err)
		}
		_ = conn1.Close()

		// 2. Connect with ordinary client cert -> rejected
		clientCert, clientKey := ca.IssueClientCert(t, "ordinary-client")
		kpClient, _ := tls.LoadX509KeyPair(clientCert, clientKey)
		dialerClient := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{kpClient},
			RootCAs:      caPool,
			ServerName:   "node-1",
		}
		conn2, err := tls.Dial("tcp", listenAddr, dialerClient)
		if err == nil {
			_ = conn2.SetDeadline(time.Now().Add(200 * time.Millisecond))
			var b [1]byte
			_, rErr := conn2.Read(b[:])
			_ = conn2.Close()
			if rErr == nil {
				t.Fatal("expected ordinary client cert to be rejected by injected verifier, but read succeeded")
			}
		}
		sErr2, _ := runTestHandshake(t, mgr.cfg.ListenerTLSConfig, dialerClient)
		if sErr2 == nil {
			t.Fatal("expected server handshake to reject ordinary client cert, got nil error")
		}

		// 3. Connect with rogue peer cert (Node 999, not in topology) -> rejected
		node999Cert, node999Key := ca.IssuePeerCert(t, 999)
		kp999, _ := tls.LoadX509KeyPair(node999Cert, node999Key)
		dialer999 := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{kp999},
			RootCAs:      caPool,
			ServerName:   "node-1",
		}
		conn3, err := tls.Dial("tcp", listenAddr, dialer999)
		if err == nil {
			_ = conn3.SetDeadline(time.Now().Add(200 * time.Millisecond))
			var b [1]byte
			_, rErr := conn3.Read(b[:])
			_ = conn3.Close()
			if rErr == nil {
				t.Fatal("expected rogue Node 999 cert to be rejected by injected verifier, but read succeeded")
			}
		}
		sErr3, _ := runTestHandshake(t, mgr.cfg.ListenerTLSConfig, dialer999)
		if sErr3 == nil {
			t.Fatal("expected server handshake to reject rogue Node 999 cert, got nil error")
		}
	})

	// Case B: Programmatic outbound mTLS without VerifyConnection (VerifyConnection = nil)
	// Server presents Node 3 cert when dialing expected Node 2 -> connection fails.
	t.Run("Case B: Programmatic outbound mTLS with nil VerifyConnection binds expected peer", func(t *testing.T) {
		// Server presents Node 3 cert
		serverTLS := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{kp3},
			ClientCAs:    caPool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
		}
		ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
		if err != nil {
			t.Fatalf("tls.Listen failed: %v", err)
		}
		defer ln.Close()

		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				_ = conn.Close()
			}
		}()

		// Outbound config without VerifyConnection
		outboundTLS := &tls.Config{
			MinVersion:       tls.VersionTLS13,
			Certificates:     []tls.Certificate{kp1},
			RootCAs:          caPool,
			ClientCAs:        caPool,
			ClientAuth:       tls.RequireAndVerifyClientCert,
			VerifyConnection: nil, // Caller omitted
		}

		// Manager for Node 1 configured to dial Node 2 at ln.Addr()
		topoWithRemote := createTestTopology(t, 1, "127.0.0.1:19098", map[cluster.NodeID]string{
			2: ln.Addr().String(),
		})

		cfg := DefaultPeerConnectionConfig()
		cfg.TLSConfig = outboundTLS
		cfg.DialTimeout = 200 * time.Millisecond
		cfg.ReconnectMin = 50 * time.Millisecond
		cfg.ReconnectMax = 100 * time.Millisecond

		mgr, err := NewPeerConnectionManager(topoWithRemote, cfg)
		if err != nil {
			t.Fatalf("NewPeerConnectionManager failed: %v", err)
		}
		defer mgr.Close()

		if err := mgr.Start(); err != nil {
			t.Fatalf("Start failed: %v", err)
		}

		time.Sleep(300 * time.Millisecond)

		// Expected Node 2 must NOT be connected because server presented Node 3 cert
		if mgr.IsConnected(2) {
			t.Fatal("security violation: manager connected to Node 2 despite server presenting Node 3 cert")
		}
	})

	// Case C: Caller VerifyConnection tries to weaken security (returns nil on rogue cert)
	// Connection must STILL be rejected by Lattice mandatory checks.
	t.Run("Case C: Caller VerifyConnection returning nil cannot bypass Lattice checks", func(t *testing.T) {
		callerBypassed := false
		weakListenerTLS := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{kp1},
			ClientCAs:    caPool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
			VerifyConnection: func(cs tls.ConnectionState) error {
				callerBypassed = true
				return nil // Insecure caller callback tries to permit everything
			},
		}

		mgr, err := NewPeerConnectionManager(topo, PeerConnectionConfig{
			ListenerTLSConfig: weakListenerTLS,
		})
		if err != nil {
			t.Fatalf("NewPeerConnectionManager failed: %v", err)
		}
		defer mgr.Close()

		if err := mgr.StartListener("127.0.0.1:0"); err != nil {
			t.Fatalf("StartListener failed: %v", err)
		}
		listenAddr := mgr.ListenerAddr().String()

		// Connect with rogue peer cert (Node 999)
		node999Cert, node999Key := ca.IssuePeerCert(t, 999)
		kp999, _ := tls.LoadX509KeyPair(node999Cert, node999Key)
		dialer := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{kp999},
			RootCAs:      caPool,
			ServerName:   "node-1",
		}

		conn, err := tls.Dial("tcp", listenAddr, dialer)
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(200 * time.Millisecond))
			var b [1]byte
			_, rErr := conn.Read(b[:])
			_ = conn.Close()
			if rErr == nil {
				t.Fatal("expected connection to be rejected by Lattice verifier, but read succeeded")
			}
		}
		sErr, _ := runTestHandshake(t, mgr.cfg.ListenerTLSConfig, dialer)
		if sErr == nil {
			t.Fatal("expected server handshake to reject rogue cert despite caller callback returning nil")
		}
		if !callerBypassed {
			t.Fatal("expected caller verification to have run first")
		}
	})

	// Case D: Caller VerifyConnection intentionally rejects
	// Preserves documented callback semantics and ensures connection is rejected.
	t.Run("Case D: Caller VerifyConnection intentionally rejecting is honored", func(t *testing.T) {
		callerRan := false
		rejectingListenerTLS := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{kp1},
			ClientCAs:    caPool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
			VerifyConnection: func(cs tls.ConnectionState) error {
				callerRan = true
				return fmt.Errorf("custom policy rejection: maintenance mode")
			},
		}

		mgr, err := NewPeerConnectionManager(topo, PeerConnectionConfig{
			ListenerTLSConfig: rejectingListenerTLS,
		})
		if err != nil {
			t.Fatalf("NewPeerConnectionManager failed: %v", err)
		}
		defer mgr.Close()

		if err := mgr.StartListener("127.0.0.1:0"); err != nil {
			t.Fatalf("StartListener failed: %v", err)
		}
		listenAddr := mgr.ListenerAddr().String()

		// Connect with valid Node 2 peer cert
		dialer := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{kp2},
			RootCAs:      caPool,
			ServerName:   "node-1",
		}

		conn, err := tls.Dial("tcp", listenAddr, dialer)
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(200 * time.Millisecond))
			var b [1]byte
			_, rErr := conn.Read(b[:])
			_ = conn.Close()
			if rErr == nil {
				t.Fatal("expected connection to be rejected by caller's VerifyConnection, but read succeeded")
			}
		}
		sErr, _ := runTestHandshake(t, mgr.cfg.ListenerTLSConfig, dialer)
		if sErr == nil {
			t.Fatal("expected server handshake to reject when caller VerifyConnection rejects")
		}
		if !callerRan {
			t.Fatal("expected caller VerifyConnection to have run")
		}
	})
}

func TestExtractNodeIDFromCert_AmbiguityAudit(t *testing.T) {
	ca := NewTestCA(t, "Ambiguity Audit CA")

	t.Run("Conflicting SAN URI vs SAN DNS => Rejected", func(t *testing.T) {
		u, _ := url.Parse("spiffe://lattice/node/1")
		certPath, keyPath := ca.IssueCert(t, "conflict-uri-dns", CertOptions{
			CommonName:         "node-1",
			OrganizationalUnit: []string{PeerCertRoleOU},
			DNSNames:           []string{"node-2"}, // DNS asserts 2, URI asserts 1
			URIs:               []*url.URL{u},
			IsServer:           true,
			IsClient:           true,
		})
		_ = keyPath
		kp, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			t.Fatalf("LoadX509KeyPair failed: %v", err)
		}
		cert, err := x509.ParseCertificate(kp.Certificate[0])
		if err != nil {
			t.Fatalf("ParseCertificate failed: %v", err)
		}

		_, err = ExtractNodeIDFromCert(cert)
		if err == nil {
			t.Fatal("expected conflict error between SAN URI and SAN DNS, got nil")
		}
	})

	t.Run("Conflicting SAN DNS vs CN => Rejected", func(t *testing.T) {
		certPath, keyPath := ca.IssueCert(t, "conflict-dns-cn", CertOptions{
			CommonName:         "node-1", // CN asserts 1
			OrganizationalUnit: []string{PeerCertRoleOU},
			DNSNames:           []string{"node-2"}, // DNS asserts 2
			IsServer:           true,
			IsClient:           true,
		})
		kp, _ := tls.LoadX509KeyPair(certPath, keyPath)
		cert, _ := x509.ParseCertificate(kp.Certificate[0])

		_, err := ExtractNodeIDFromCert(cert)
		if err == nil {
			t.Fatal("expected conflict error between SAN DNS and CN, got nil")
		}
	})

	t.Run("Consistent SAN URI, SAN DNS, and CN => Accepted", func(t *testing.T) {
		u, _ := url.Parse("spiffe://lattice/node/1")
		certPath, keyPath := ca.IssueCert(t, "consistent-cert", CertOptions{
			CommonName:         "node-1",
			OrganizationalUnit: []string{PeerCertRoleOU},
			DNSNames:           []string{"localhost", "node-1", "node-1.lattice.cluster"},
			URIs:               []*url.URL{u},
			IsServer:           true,
			IsClient:           true,
		})
		kp, _ := tls.LoadX509KeyPair(certPath, keyPath)
		cert, _ := x509.ParseCertificate(kp.Certificate[0])

		id, err := ExtractNodeIDFromCert(cert)
		if err != nil {
			t.Fatalf("expected success with consistent cert, got: %v", err)
		}
		if id != 1 {
			t.Fatalf("expected NodeID 1, got %d", id)
		}
	})
}
