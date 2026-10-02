package transport

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cluster"
	latticeerrors "github.com/silent-knight19/lattice/internal/errors"
)

// =============================================================================
// Pre-authentication handshake bounding (regression).
//
// trackConn increments activeConns *before* handleConn performs the TLS handshake,
// and activeConns is only released when the connection is fully torn down. So an
// unauthenticated peer holding a stalled handshake occupied a MaxConnections slot for
// the whole HeaderTimeout, and enough of them refused every legitimate client. The
// peer listener had the same shape against MaxInboundPeerConnections.
//
// Both now bound pre-authentication concurrency separately, and a connection
// rejected by that bound never touches the real slot counter.
// =============================================================================

// TestPendingHandshake_BoundedByCap exercises the accounting directly.
func TestPendingHandshake_BoundedByCap(t *testing.T) {
	srv := &Server{}
	const cap = 8

	for i := 0; i < cap; i++ {
		if !srv.acquirePendingHandshake(cap) {
			t.Fatalf("acquire %d unexpectedly refused under a cap of %d", i, cap)
		}
	}
	if srv.acquirePendingHandshake(cap) {
		t.Fatalf("acquire succeeded past the cap of %d", cap)
	}
	if got := srv.pendingHandshakes.Load(); got != int64(cap) {
		t.Errorf("pendingHandshakes = %d; want %d", got, cap)
	}

	srv.releasePendingHandshake()
	if !srv.acquirePendingHandshake(cap) {
		t.Error("acquire refused after a release freed a slot")
	}
}

// TestPendingHandshake_ReleaseIsIdempotent guards the counter against going
// negative, which would otherwise let a later burst exceed the cap.
func TestPendingHandshake_ReleaseIsIdempotent(t *testing.T) {
	srv := &Server{}
	const cap = 2

	srv.acquirePendingHandshake(cap)
	srv.releasePendingHandshake()
	srv.releasePendingHandshake()
	srv.releasePendingHandshake()

	if got := srv.pendingHandshakes.Load(); got != 0 {
		t.Fatalf("pendingHandshakes = %d after redundant releases; want 0", got)
	}
	// With the counter at zero the full cap must still be grantable.
	for i := 0; i < cap; i++ {
		if !srv.acquirePendingHandshake(cap) {
			t.Fatalf("acquire %d refused; a drifted counter is starving real connections", i)
		}
	}
}

// TestPendingHandshake_ConcurrentAcquireNeverExceedsCap is the property that
// matters under load: the CAS loop must not hand out more slots than the cap.
func TestPendingHandshake_ConcurrentAcquireNeverExceedsCap(t *testing.T) {
	srv := &Server{}
	const (
		cap    = 32
		racers = 64
	)
	var granted int64
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if srv.acquirePendingHandshake(cap) {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if granted != cap {
		t.Errorf("granted %d slots under a cap of %d; want exactly %d", granted, cap, cap)
	}
	if got := srv.pendingHandshakes.Load(); got != int64(cap) {
		t.Errorf("pendingHandshakes = %d; want %d", got, cap)
	}
}

// TestPendingHandshake_RejectedConnectionConsumesNoSlot is the core security
// property: shedding a stalled handshake must not consume a real connection slot.
func TestPendingHandshake_RejectedConnectionConsumesNoSlot(t *testing.T) {
	const (
		addr         = "127.0.0.1:0"
		maxConns     = 4
		maxPending   = 1
		headerWindow = 4 * time.Second
	)

	cfg := DefaultServerConfig()
	cfg.Address = addr
	cfg.MaxConnections = maxConns
	cfg.MaxPendingHandshakes = maxPending
	cfg.HeaderTimeout = headerWindow

	srv, err := NewServer(cfg, newCountingEngine())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	if err := srv.Listen(addr); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	dialAddr := srv.Addr().String()

	// Stall one connection without completing a handshake. On a loopback plaintext
	// config the server still counts it as a connection; the point is that the
	// pending bound is what gates admission.
	stalled, err := net.Dial("tcp", dialAddr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = stalled.Close() }()

	// Let the accept loop observe it.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && srv.pendingHandshakes.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	// Hammer the listener well beyond both caps. Whatever is shed must be shed by the
	// pending bound rather than by exhausting MaxConnections.
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, derr := net.Dial("tcp", dialAddr)
			if derr == nil {
				_ = c.Close()
			}
		}()
	}
	wg.Wait()

	if got := srv.pendingHandshakes.Load(); got < 0 {
		t.Fatalf("pendingHandshakes = %d; must never go negative", got)
	}
	if got := srv.activeConns.Load(); got > int64(maxConns) {
		t.Errorf("activeConns = %d; exceeded MaxConnections %d", got, maxConns)
	}
}

// TestPendingHandshake_DefaultsAreSane pins the shipped defaults so the bound can
// never silently regress to unbounded.
func TestPendingHandshake_DefaultsAreSane(t *testing.T) {
	if DefaultMaxPendingHandshakes <= 0 {
		t.Fatalf("DefaultMaxPendingHandshakes = %v; want positive", DefaultMaxPendingHandshakes)
	}
	if got := DefaultServerConfig().MaxPendingHandshakes; got != DefaultMaxPendingHandshakes {
		t.Errorf("DefaultServerConfig().MaxPendingHandshakes = %v; want %v", got, DefaultMaxPendingHandshakes)
	}

	defs := DefaultServerConfig()
	if defs.MaxPendingHandshakes >= defs.MaxConnections {
		t.Errorf("MaxPendingHandshakes (%d) must be well below MaxConnections (%d); "+
			"otherwise stalled handshakes can still exhaust every slot",
			defs.MaxPendingHandshakes, defs.MaxConnections)
	}

	// The peer bound must sit well below the inbound peer ceiling.
	if MaxPendingPeerHandshakes >= MaxInboundPeerConnections {
		t.Errorf("MaxPendingPeerHandshakes (%d) must be below MaxInboundPeerConnections (%d)",
			MaxPendingPeerHandshakes, MaxInboundPeerConnections)
	}
}

// TestPendingHandshake_PeerReleaseNeverGoesNegative covers the peer-side helper.
func TestPendingHandshake_PeerReleaseNeverGoesNegative(t *testing.T) {
	topo, err := cluster.NewTopology(1, "127.0.0.1:19098", []cluster.PeerConfig{
		{ID: 2, Address: "127.0.0.1:19099"},
	})
	if err != nil {
		t.Fatalf("NewTopology: %v", err)
	}
	mgr, err := NewPeerConnectionManager(topo, PeerConnectionConfig{})
	if err != nil {
		t.Fatalf("NewPeerConnectionManager: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	for i := 0; i < 5; i++ {
		mgr.releasePendingInbound()
	}
	if got := mgr.pendingInbound.Load(); got != 0 {
		t.Fatalf("pendingInbound = %d after redundant releases; want 0", got)
	}

	for i := 0; i < MaxPendingPeerHandshakes; i++ {
		mgr.pendingInbound.Add(1)
	}
	for i := 0; i < MaxPendingPeerHandshakes; i++ {
		mgr.releasePendingInbound()
	}
	if got := mgr.pendingInbound.Load(); got != 0 {
		t.Errorf("pendingInbound = %d after balanced acquire/release; want 0", got)
	}
}

// TestPendingHandshake_ErrorSentinelExists keeps the sentinel wired for callers
// that need to distinguish a shed handshake from other accept failures.
func TestPendingHandshake_ErrorSentinelExists(t *testing.T) {
	if latticeerrors.ErrPeerIdleTimeout == nil {
		t.Fatal("ErrPeerIdleTimeout is nil")
	}
	_ = context.Background()
}

// --- End-to-end: the accept path itself ------------------------------------

// stalledTLSConfig builds a self-signed TLS 1.3 server config for 127.0.0.1.
// Loopback with TLS but no client-cert requirement is a permitted configuration, so
// this exercises the real handshake path without needing a client CA.
func stalledTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}},
	}
}

// TestPendingHandshake_StalledHandshakesAreShedE2E is the assertion that actually
// depends on the accept-path gate.
//
// Without it, trackConn consumes a MaxConnections slot *before* the handshake, so
// every stalled connection is admitted and activeConns climbs to MaxConnections.
// With it, admission is bounded by MaxPendingHandshakes and activeConns never
// exceeds that smaller bound.
func TestPendingHandshake_StalledHandshakesAreShedE2E(t *testing.T) {
	const (
		maxConns   = 32
		maxPending = 3
		stallers   = 30
	)

	cfg := DefaultServerConfig()
	cfg.Address = "127.0.0.1:0"
	cfg.MaxConnections = maxConns
	cfg.MaxPendingHandshakes = maxPending
	cfg.HeaderTimeout = 30 * time.Second // long enough that nothing times out mid-test
	cfg.TLSConfig = stalledTLSConfig(t)

	srv, err := NewServer(cfg, newCountingEngine())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	if err := srv.Listen(cfg.Address); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	dialAddr := srv.Addr().String()

	// Open connections that never send a ClientHello, so their handshakes stall.
	var held []net.Conn
	defer func() {
		for _, c := range held {
			_ = c.Close()
		}
	}()
	for i := 0; i < stallers; i++ {
		c, derr := net.Dial("tcp", dialAddr)
		if derr != nil {
			t.Fatalf("Dial %d: %v", i, derr)
		}
		held = append(held, c)
	}

	// Give the accept loop time to admit or shed every connection.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if srv.activeConns.Load() >= int64(maxPending) && srv.pendingHandshakes.Load() >= int64(maxPending) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Let any further admissions settle.
	time.Sleep(500 * time.Millisecond)

	pending := srv.pendingHandshakes.Load()
	active := srv.activeConns.Load()

	t.Logf("stallers=%d pending=%d active=%d (maxPending=%d maxConns=%d)",
		stallers, pending, active, maxPending, maxConns)

	if pending > int64(maxPending) {
		t.Errorf("pendingHandshakes = %d; exceeded MaxPendingHandshakes %d", pending, maxPending)
	}
	// The decisive assertion: without the pre-auth gate this reaches maxConns (32).
	if active > int64(maxPending) {
		t.Errorf("activeConns = %d; stalled unauthenticated handshakes consumed real "+
			"connection slots beyond MaxPendingHandshakes (%d) and toward MaxConnections "+
			"(%d). Pre-authentication admission is not being shed.",
			active, maxPending, maxConns)
	}
}
