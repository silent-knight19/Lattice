package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
)

// PeerState represents the lifecycle state of a peer connection.
type PeerState uint8

const (
	// PeerStateDisconnected indicates no active network connection exists to the peer.
	PeerStateDisconnected PeerState = iota

	// PeerStateConnecting indicates an active dial attempt is in progress.
	PeerStateConnecting

	// PeerStateConnected indicates a validated, healthy connection is established and active.
	PeerStateConnected

	// PeerStateClosing indicates the connection or manager is shutting down.
	PeerStateClosing
)

// String returns the human-readable string representation of the PeerState.
func (s PeerState) String() string {
	switch s {
	case PeerStateDisconnected:
		return "Disconnected"
	case PeerStateConnecting:
		return "Connecting"
	case PeerStateConnected:
		return "Connected"
	case PeerStateClosing:
		return "Closing"
	default:
		return fmt.Sprintf("PeerState(%d)", s)
	}
}

// RawDialFunc is a low-level network dialer function that establishes the underlying raw
// network transport (e.g. TCP connection or in-memory socket).
//
// Security Invariant: For non-loopback Raft peers, Lattice strictly owns the TLS layer.
// Lattice wraps the raw connection returned by RawDialFunc with its own manager-owned
// TLS 1.3 mTLS configuration and executes the handshake and identity verification.
// A RawDialFunc (or DialFunc) is never permitted to return an already-authenticated *tls.Conn
// for non-loopback peers; attempting to supply a pre-authenticated TLS connection fails closed.
type RawDialFunc func(ctx context.Context, addr string) (net.Conn, error)

// DialFunc is the pluggable dialer type preserved for backward compatibility and treated
// strictly as a raw transport dialer.
type DialFunc func(ctx context.Context, addr string) (net.Conn, error)

// PeerFrameHandler is invoked when a valid protocol frame is received from a peer.
type PeerFrameHandler func(peerID cluster.NodeID, frame *Frame)

// PeerConnectionConfig configures the outbound peer connection manager.
type PeerConnectionConfig struct {
	// DialTimeout is the maximum duration permitted for a connection attempt (default: 3s).
	DialTimeout time.Duration

	// ReconnectMin is the minimum backoff delay after connection failure (default: 50ms).
	ReconnectMin time.Duration

	// ReconnectMax is the maximum backoff delay after consecutive failures (default: 5s).
	ReconnectMax time.Duration

	// KeepAlivePeriod is the duration between TCP keep-alive probes (default: 15s).
	KeepAlivePeriod time.Duration

	// WriteTimeout is the deadline applied to individual frame writes (default: 5s).
	WriteTimeout time.Duration

	// RawDialFunc is an optional raw network dialer seam (e.g. for connection tracking,
	// partition simulation, or proxying). On non-loopback peers, Lattice always owns the
	// TLS 1.3 mTLS handshake and upgrades the connection using its manager-owned peer TLS configuration.
	RawDialFunc RawDialFunc

	// DialFunc is an alias/fallback for RawDialFunc preserved for compatibility. It is treated
	// strictly as a raw transport dialer. On non-loopback peers, returning an already-authenticated
	// *tls.Conn is rejected fail-closed to prevent custom dialers from bypassing manager-owned TLS.
	DialFunc DialFunc

	// OnFrameReceived is invoked when a valid frame is received from a remote peer.
	OnFrameReceived PeerFrameHandler

	// InsecureTransport explicitly permits unauthenticated, plaintext TCP transport for local loopback
	// test and development topologies. Remote/non-loopback peer addresses strictly mandate mutual TLS 1.3
	// regardless of this setting (Finding B).
	InsecureTransport bool

	// TLSConfig is the optional TLS configuration for outbound dials.
	// If nil and PeerTLSCertFile/PeerTLSKeyFile/PeerCAFile are provided, manager initializes it.
	TLSConfig *tls.Config

	// ListenerTLSConfig is the optional TLS configuration for inbound peer listener.
	ListenerTLSConfig *tls.Config

	// PeerTLSCertFile and PeerTLSKeyFile provide paths to X.509 certificate and private key files for peer mTLS.
	PeerTLSCertFile string
	PeerTLSKeyFile  string

	// PeerCAFile provides the path to the trusted CA certificate bundle for Raft peer mutual authentication.
	PeerCAFile string
}

// DefaultPeerConnectionConfig returns production-hardened defaults for peer connections.
func DefaultPeerConnectionConfig() PeerConnectionConfig {
	return PeerConnectionConfig{
		DialTimeout:     3 * time.Second,
		ReconnectMin:    50 * time.Millisecond,
		ReconnectMax:    5 * time.Second,
		KeepAlivePeriod: 15 * time.Second,
		WriteTimeout:    5 * time.Second,
	}
}

// configureKeepAlive configures TCP keep-alive on conn or its underlying network socket.
func configureKeepAlive(conn net.Conn, period time.Duration) {
	if period <= 0 {
		return
	}
	type netConnGetter interface {
		NetConn() net.Conn
	}
	cur := conn
	for cur != nil {
		if tcpConn, ok := cur.(*net.TCPConn); ok {
			_ = tcpConn.SetKeepAlive(true)
			_ = tcpConn.SetKeepAlivePeriod(period)
			return
		}
		if getter, ok := cur.(netConnGetter); ok {
			cur = getter.NetConn()
		} else {
			break
		}
	}
}

const (
	// DefaultReplayWindowSize is the maximum number of recent sequence numbers tracked per peer.
	DefaultReplayWindowSize = 4096
	// DefaultMaxNoncesTracked is the maximum number of cryptographic nonces retained in memory per peer.
	DefaultMaxNoncesTracked = 4096
)

// peerReplayFilter maintains bounded sliding-window sequence tracking and duplicate nonce detection
// to defend against message replay and duplication attacks.
type peerReplayFilter struct {
	mu         sync.Mutex
	peerID     cluster.NodeID
	maxSeqID   uint64
	seenSeqs   map[uint64]struct{}
	seqRing    []uint64
	seqHead    int
	nonceSet   map[uint64]struct{}
	nonceRing  []uint64
	ringHead   int
	windowSize uint64
	maxNonces  int
}

func newPeerReplayFilter(peerID cluster.NodeID) *peerReplayFilter {
	return &peerReplayFilter{
		peerID:     peerID,
		seenSeqs:   make(map[uint64]struct{}, 128),
		seqRing:    make([]uint64, 0, DefaultReplayWindowSize),
		nonceSet:   make(map[uint64]struct{}, 128),
		nonceRing:  make([]uint64, 0, DefaultMaxNoncesTracked),
		windowSize: DefaultReplayWindowSize,
		maxNonces:  DefaultMaxNoncesTracked,
	}
}

// ResetSequence resets connection-scoped sequence tracking upon new connection establishment.
func (rf *peerReplayFilter) ResetSequence() {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	rf.maxSeqID = 0
	rf.seenSeqs = make(map[uint64]struct{}, 128)
	rf.seqRing = rf.seqRing[:0]
	rf.seqHead = 0
}

func (rf *peerReplayFilter) CheckAndRecord(f *Frame) error {
	if f == nil {
		return errors.ErrNilReceiver
	}

	rf.mu.Lock()
	defer rf.mu.Unlock()

	seqID := f.Header.SeqID

	// 1. Sequence Monotonicity & Sliding Window Check (O(1) amortized using ring buffer)
	if seqID == 0 {
		return &errors.ReplayedFrameError{
			NodeID: uint64(rf.peerID),
			SeqID:  0,
			Reason: "invalid sequence ID: 0 is prohibited",
		}
	}

	if rf.maxSeqID == 0 {
		rf.maxSeqID = seqID
		rf.seenSeqs[seqID] = struct{}{}
		rf.seqRing = append(rf.seqRing, seqID)
	} else if seqID <= rf.maxSeqID {
		if rf.maxSeqID-seqID >= rf.windowSize {
			return &errors.ReplayedFrameError{
				NodeID: uint64(rf.peerID),
				SeqID:  seqID,
				Reason: fmt.Sprintf("stale sequence ID: %d falls behind window [%d, %d]", seqID, rf.maxSeqID-rf.windowSize+1, rf.maxSeqID),
			}
		}
		if _, exists := rf.seenSeqs[seqID]; exists {
			return &errors.ReplayedFrameError{
				NodeID: uint64(rf.peerID),
				SeqID:  seqID,
				Reason: fmt.Sprintf("duplicate sequence ID: %d already processed", seqID),
			}
		}
		rf.seenSeqs[seqID] = struct{}{}
		if len(rf.seqRing) < int(rf.windowSize) {
			rf.seqRing = append(rf.seqRing, seqID)
		} else {
			oldest := rf.seqRing[rf.seqHead]
			delete(rf.seenSeqs, oldest)
			rf.seqRing[rf.seqHead] = seqID
			rf.seqHead = (rf.seqHead + 1) % int(rf.windowSize)
		}
	} else {
		rf.maxSeqID = seqID
		rf.seenSeqs[seqID] = struct{}{}
		if len(rf.seqRing) < int(rf.windowSize) {
			rf.seqRing = append(rf.seqRing, seqID)
		} else {
			oldest := rf.seqRing[rf.seqHead]
			delete(rf.seenSeqs, oldest)
			rf.seqRing[rf.seqHead] = seqID
			rf.seqHead = (rf.seqHead + 1) % int(rf.windowSize)
		}
	}

	// 2. Nonce Replay Check for Peer Control Plane Requests
	var nonce uint64
	switch PeerMessageType(f.Header.OpCode) {
	case PeerOpRequestVote:
		if len(f.Payload) >= RequestVoteRequestSize {
			nonce = binary.GetUint64(f.Payload[32:40])
		}
	case PeerOpAppendEntries:
		if len(f.Payload) >= AppendEntriesRequestHeaderSize {
			nonce = binary.GetUint64(f.Payload[40:48])
		}
	}

	if nonce != 0 {
		if _, exists := rf.nonceSet[nonce]; exists {
			return &errors.ReplayedFrameError{
				NodeID: uint64(rf.peerID),
				SeqID:  seqID,
				Nonce:  nonce,
				Reason: fmt.Sprintf("duplicate nonce: 0x%016x already seen", nonce),
			}
		}
		if len(rf.nonceRing) < rf.maxNonces {
			rf.nonceRing = append(rf.nonceRing, nonce)
		} else {
			oldest := rf.nonceRing[rf.ringHead]
			delete(rf.nonceSet, oldest)
			rf.nonceRing[rf.ringHead] = nonce
			rf.ringHead = (rf.ringHead + 1) % rf.maxNonces
		}
		rf.nonceSet[nonce] = struct{}{}
	}

	return nil
}

// peerSupervisor manages the complete connection lifecycle for a single remote peer.
type peerSupervisor struct {
	peerID   cluster.NodeID
	address  string
	cfg      PeerConnectionConfig
	topology *cluster.Topology

	mu           sync.RWMutex
	state        PeerState
	conn         net.Conn
	generation   uint64
	connDoneCh   chan struct{}
	failures     int
	replayFilter *peerReplayFilter

	readerWg sync.WaitGroup
	writeMu  sync.Mutex
}

func newPeerSupervisor(p cluster.Peer, cfg PeerConnectionConfig, topology *cluster.Topology) *peerSupervisor {
	return &peerSupervisor{
		peerID:       p.ID,
		address:      p.Address,
		cfg:          cfg,
		topology:     topology,
		state:        PeerStateDisconnected,
		failures:     0,
		replayFilter: newPeerReplayFilter(p.ID),
	}
}

// getState returns the current lifecycle state of the supervisor.
func (s *peerSupervisor) getState() PeerState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// calculateBackoff computes exponential backoff delay based on consecutive failure count.
// Uses safe doubling to prevent integer overflow for any arbitrary failure count.
func (s *peerSupervisor) calculateBackoff(failures int) time.Duration {
	if failures <= 1 {
		return s.cfg.ReconnectMin
	}
	delay := s.cfg.ReconnectMin
	for i := 1; i < failures; i++ {
		if delay >= s.cfg.ReconnectMax/2 {
			return s.cfg.ReconnectMax
		}
		delay *= 2
	}
	if delay > s.cfg.ReconnectMax {
		return s.cfg.ReconnectMax
	}
	return delay
}

// disconnect tears down the connection for the given generation token.
// Guarantees that stale failure notifications from older connections cannot disconnect
// a newer active connection (P14-M03-INV-07), and preserves PeerStateClosing as a terminal state.
func (s *peerSupervisor) disconnect(gen uint64, reason error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// If the generation does not match the active generation, this is a stale teardown event.
	if s.generation != gen || s.conn == nil {
		return
	}

	connToClose := s.conn
	s.conn = nil
	if s.state != PeerStateClosing {
		s.state = PeerStateDisconnected
	}

	if s.connDoneCh != nil {
		select {
		case <-s.connDoneCh:
		default:
			close(s.connDoneCh)
		}
	}

	_ = connToClose.Close()
}

// run is the persistent supervisor loop. It attempts connection, monitors health,
// and reconnects using bounded exponential backoff.
func (s *peerSupervisor) run(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		s.mu.Lock()
		if s.state == PeerStateClosing || ctx.Err() != nil {
			s.state = PeerStateClosing
			s.mu.Unlock()
			return
		}
		if s.state == PeerStateConnected && s.conn != nil {
			doneCh := s.connDoneCh
			curGen := s.generation
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				s.disconnect(curGen, ctx.Err())
				s.mu.Lock()
				s.state = PeerStateClosing
				s.mu.Unlock()
				s.readerWg.Wait()
				return
			case <-doneCh:
				s.mu.Lock()
				if s.state == PeerStateClosing || ctx.Err() != nil {
					s.state = PeerStateClosing
					s.mu.Unlock()
					return
				}
				if s.state == PeerStateConnected && s.conn != nil {
					s.mu.Unlock()
					continue
				}
				s.mu.Unlock()

				s.readerWg.Wait()

				s.mu.Lock()
				if s.state == PeerStateClosing || ctx.Err() != nil {
					s.state = PeerStateClosing
					s.mu.Unlock()
					return
				}
				if s.state == PeerStateConnected && s.conn != nil {
					s.mu.Unlock()
					continue
				}
				s.failures++
				failures := s.failures
				s.mu.Unlock()

				delay := s.calculateBackoff(failures)
				select {
				case <-ctx.Done():
					s.mu.Lock()
					s.state = PeerStateClosing
					s.mu.Unlock()
					return
				case <-time.After(delay):
					continue
				}
			}
		}
		s.state = PeerStateConnecting
		s.mu.Unlock()

		// Attempt dial with timeout using raw transport dialer
		dialCtx, dialCancel := context.WithTimeout(ctx, s.cfg.DialTimeout)
		rawDialer := s.cfg.RawDialFunc
		if rawDialer == nil && s.cfg.DialFunc != nil {
			rawDialer = RawDialFunc(s.cfg.DialFunc)
		}
		rawConn, err := rawDialer(dialCtx, s.address)
		dialCancel()

		if err == nil && rawConn == nil {
			err = errors.ErrPeerUnavailable
		}

		if err != nil {
			s.mu.Lock()
			if s.state == PeerStateClosing || ctx.Err() != nil {
				s.state = PeerStateClosing
				s.mu.Unlock()
				return
			}
			s.state = PeerStateDisconnected
			s.failures++
			failures := s.failures
			s.mu.Unlock()

			delay := s.calculateBackoff(failures)
			select {
			case <-ctx.Done():
				s.mu.Lock()
				s.state = PeerStateClosing
				s.mu.Unlock()
				return
			case <-time.After(delay):
				continue
			}
		}

		// Dial succeeded; check if context was cancelled or manager closed during dial
		s.mu.Lock()
		if s.state == PeerStateClosing || ctx.Err() != nil {
			s.state = PeerStateClosing
			s.mu.Unlock()
			_ = rawConn.Close()
			return
		}
		if s.state == PeerStateConnected && s.conn != nil {
			s.mu.Unlock()
			_ = rawConn.Close()
			continue
		}
		s.mu.Unlock()

		var activeConn net.Conn
		if !isLoopbackAddress(s.address) {
			// Invariant: On non-loopback peers, custom dialers must NOT return an already-authenticated *tls.Conn.
			// Lattice mandates that the transport layer itself establishes and verifies the TLS 1.3 mTLS connection.
			if _, isTLS := rawConn.(*tls.Conn); isTLS {
				_ = rawConn.Close()
				s.mu.Lock()
				s.failures++
				failures := s.failures
				s.mu.Unlock()
				delay := s.calculateBackoff(failures)
				select {
				case <-ctx.Done():
					s.mu.Lock()
					s.state = PeerStateClosing
					s.mu.Unlock()
					return
				case <-time.After(delay):
					continue
				}
			}

			if s.cfg.TLSConfig == nil {
				_ = rawConn.Close()
				s.mu.Lock()
				s.failures++
				failures := s.failures
				s.mu.Unlock()
				delay := s.calculateBackoff(failures)
				select {
				case <-ctx.Done():
					s.mu.Lock()
					s.state = PeerStateClosing
					s.mu.Unlock()
					return
				case <-time.After(delay):
					continue
				}
			}

			// Manager-owned TLS handshake over the raw transport connection
			handshakeCtx, handshakeCancel := context.WithTimeout(ctx, s.cfg.DialTimeout)
			tlsConn := tls.Client(rawConn, s.cfg.TLSConfig)
			handshakeErr := tlsConn.HandshakeContext(handshakeCtx)
			handshakeCancel()
			if handshakeErr != nil {
				_ = rawConn.Close()
				s.mu.Lock()
				s.failures++
				failures := s.failures
				s.mu.Unlock()
				delay := s.calculateBackoff(failures)
				select {
				case <-ctx.Done():
					s.mu.Lock()
					s.state = PeerStateClosing
					s.mu.Unlock()
					return
				case <-time.After(delay):
					continue
				}
			}

			// Verify connection admission requirements
			if err := assertAdmissiblePeerConnection(tlsConn, s.peerID, s.address, s.topology, s.trustedRoots()); err != nil {
				_ = tlsConn.Close()
				s.mu.Lock()
				s.failures++
				failures := s.failures
				s.mu.Unlock()
				delay := s.calculateBackoff(failures)
				select {
				case <-ctx.Done():
					s.mu.Lock()
					s.state = PeerStateClosing
					s.mu.Unlock()
					return
				case <-time.After(delay):
					continue
				}
			}

			activeConn = tlsConn
		} else {
			// Loopback peer:
			if s.cfg.TLSConfig != nil {
				if _, isTLS := rawConn.(*tls.Conn); !isTLS {
					handshakeCtx, handshakeCancel := context.WithTimeout(ctx, s.cfg.DialTimeout)
					tlsConn := tls.Client(rawConn, s.cfg.TLSConfig)
					handshakeErr := tlsConn.HandshakeContext(handshakeCtx)
					handshakeCancel()
					if handshakeErr != nil {
						_ = rawConn.Close()
						s.mu.Lock()
						s.failures++
						failures := s.failures
						s.mu.Unlock()
						delay := s.calculateBackoff(failures)
						select {
						case <-ctx.Done():
							s.mu.Lock()
							s.state = PeerStateClosing
							s.mu.Unlock()
							return
						case <-time.After(delay):
							continue
						}
					}
					activeConn = tlsConn
				} else {
					activeConn = rawConn
				}
			} else {
				// Unencrypted loopback (permitted exclusively for local development and unit tests)
				activeConn = rawConn
			}
		}

		// Configure TCP keep-alive
		configureKeepAlive(activeConn, s.cfg.KeepAlivePeriod)

		// Admission into active supervisor connection
		s.mu.Lock()
		if s.state == PeerStateClosing || ctx.Err() != nil {
			s.state = PeerStateClosing
			s.mu.Unlock()
			_ = activeConn.Close()
			return
		}
		if s.state == PeerStateConnected && s.conn != nil {
			s.mu.Unlock()
			_ = activeConn.Close()
			continue
		}

		// Mandatory security assertion at connection admission
		if err := assertAdmissiblePeerConnection(activeConn, s.peerID, s.address, s.topology, s.trustedRoots()); err != nil {
			s.mu.Unlock()
			_ = activeConn.Close()
			continue
		}

		// Install new active connection with monotonic generation
		s.generation++
		curGen := s.generation
		s.conn = activeConn
		s.state = PeerStateConnected
		s.failures = 0
		doneCh := make(chan struct{})
		s.connDoneCh = doneCh
		s.replayFilter.ResetSequence()
		s.mu.Unlock()

		// Launch reader loop tracked by supervisor's readerWg
		s.readerWg.Add(1)
		go s.runReader(ctx, curGen, activeConn)

		// Await connection termination or manager shutdown
		select {
		case <-ctx.Done():
			s.disconnect(curGen, ctx.Err())
			s.mu.Lock()
			s.state = PeerStateClosing
			s.mu.Unlock()
			s.readerWg.Wait()
			return
		case <-doneCh:
			s.mu.Lock()
			if s.state == PeerStateClosing || ctx.Err() != nil {
				s.state = PeerStateClosing
				s.mu.Unlock()
				return
			}
			if s.state == PeerStateConnected && s.conn != nil {
				s.mu.Unlock()
				continue
			}
			s.mu.Unlock()

			s.readerWg.Wait()

			s.mu.Lock()
			if s.state == PeerStateClosing || ctx.Err() != nil {
				s.state = PeerStateClosing
				s.mu.Unlock()
				return
			}
			if s.state == PeerStateConnected && s.conn != nil {
				s.mu.Unlock()
				continue
			}
			s.failures++
			failures := s.failures
			s.mu.Unlock()

			delay := s.calculateBackoff(failures)
			select {
			case <-ctx.Done():
				s.mu.Lock()
				s.state = PeerStateClosing
				s.mu.Unlock()
				return
			case <-time.After(delay):
				continue
			}
		}
	}
}

// assertAdmissiblePeerConnection performs strict cryptographic and identity verification before
// any connection is admitted into a peer supervisor (s.conn = conn; s.state = PeerStateConnected).
//
// For non-loopback peers:
// 1. Connection MUST be a *tls.Conn.
// 2. TLS handshake MUST be complete.
// 3. Negotiated protocol MUST be TLS 1.3 or higher.
// 4. Remote peer MUST have presented at least one certificate.
// 5. The certificate MUST have cryptographically chained to the configured peer CA (VerifiedChains > 0).
// 6. The leaf certificate MUST assert the Lattice peer role (OU = Lattice Raft Peer) and NOT a client role.
// 7. The leaf certificate MUST assert an unambiguous NodeID matching the expected target peer ID.
// 8. The extracted NodeID MUST belong to the configured topology and MUST NOT be the local node (non-self).
// 9. If trustedRoots is non-nil, the leaf certificate MUST cryptographically chain to trustedRoots.
func assertAdmissiblePeerConnection(conn net.Conn, expectedPeerID cluster.NodeID, addr string, topo *cluster.Topology, trustedRoots *x509.CertPool) error {
	if conn == nil {
		return errors.ErrNilReceiver
	}
	if isLoopbackAddress(addr) {
		return nil
	}
	tc, ok := conn.(*tls.Conn)
	if !ok {
		return fmt.Errorf("security violation: non-loopback peer connection %q must be *tls.Conn", addr)
	}
	cs := tc.ConnectionState()
	if !cs.HandshakeComplete {
		return fmt.Errorf("security violation: non-loopback peer connection %q handshake incomplete", addr)
	}
	if cs.Version < tls.VersionTLS13 {
		return fmt.Errorf("security violation: non-loopback peer connection %q requires TLS 1.3+, got 0x%04x", addr, cs.Version)
	}
	if len(cs.PeerCertificates) == 0 {
		return fmt.Errorf("security violation: non-loopback peer connection %q missing remote certificate", addr)
	}
	if len(cs.VerifiedChains) == 0 {
		return fmt.Errorf("security violation: non-loopback peer connection %q missing verified certificate chain to trusted peer CA", addr)
	}
	leaf := cs.PeerCertificates[0]
	if !IsPeerCertificate(leaf) || IsClientCertificate(leaf) {
		return fmt.Errorf("security violation: non-loopback peer connection %q certificate missing required peer role", addr)
	}
	nodeID, err := ExtractNodeIDFromCert(leaf)
	if err != nil {
		return fmt.Errorf("security violation: non-loopback peer connection %q NodeID extraction failed: %w", addr, err)
	}
	if nodeID != expectedPeerID {
		return fmt.Errorf("security violation: non-loopback peer connection %q authenticated NodeID %d != expected %d", addr, nodeID, expectedPeerID)
	}
	if topo != nil {
		if !topo.Contains(nodeID) {
			return fmt.Errorf("security violation: non-loopback peer connection %q NodeID %d not in cluster topology", addr, nodeID)
		}
		if topo.IsSelf(nodeID) {
			return fmt.Errorf("security violation: non-loopback peer connection %q NodeID %d is local node (self-connection prohibited)", addr, nodeID)
		}
	}
	if trustedRoots != nil {
		intermediates := x509.NewCertPool()
		for _, cert := range cs.PeerCertificates[1:] {
			intermediates.AddCert(cert)
		}
		opts := x509.VerifyOptions{
			Roots:         trustedRoots,
			Intermediates: intermediates,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		}
		if _, err := leaf.Verify(opts); err != nil {
			return fmt.Errorf("security violation: non-loopback peer connection %q certificate chain not trusted by Lattice peer CA: %w", addr, err)
		}
	}
	return nil
}

func (s *peerSupervisor) trustedRoots() *x509.CertPool {
	if s.cfg.ListenerTLSConfig != nil && s.cfg.ListenerTLSConfig.ClientCAs != nil {
		return s.cfg.ListenerTLSConfig.ClientCAs
	}
	if s.cfg.TLSConfig != nil {
		if s.cfg.TLSConfig.RootCAs != nil {
			return s.cfg.TLSConfig.RootCAs
		}
		if s.cfg.TLSConfig.ClientCAs != nil {
			return s.cfg.TLSConfig.ClientCAs
		}
	}
	return nil
}

// adoptInboundConnection installs an accepted inbound TCP connection into the supervisor,
// replacing any stale connection, and starts a reader loop.
// Returns true if connection was adopted, false if rejected.
func (s *peerSupervisor) adoptInboundConnection(ctx context.Context, conn net.Conn, firstFrame *Frame, localID cluster.NodeID) bool {
	// Mandatory security assertion at inbound connection admission:
	if err := assertAdmissiblePeerConnection(conn, s.peerID, s.address, s.topology, s.trustedRoots()); err != nil {
		_ = conn.Close()
		return false
	}

	s.mu.Lock()
	if s.state == PeerStateClosing || ctx.Err() != nil {
		s.mu.Unlock()
		_ = conn.Close()
		return false
	}

	// If already connected with a live connection:
	// Tie-break: if localID < s.peerID, our local outbound connection is authoritative. Reject incoming.
	// Otherwise (localID > s.peerID), remote peer's connection is authoritative. Replace local connection.
	if s.state == PeerStateConnected && s.conn != nil {
		if localID < s.peerID {
			s.mu.Unlock()
			_ = conn.Close()
			return false
		}
	}

	oldConn := s.conn
	s.conn = nil
	if s.connDoneCh != nil {
		select {
		case <-s.connDoneCh:
		default:
			close(s.connDoneCh)
		}
	}
	if oldConn != nil {
		_ = oldConn.Close()
	}

	configureKeepAlive(conn, s.cfg.KeepAlivePeriod)

	s.generation++
	curGen := s.generation
	s.conn = conn
	s.state = PeerStateConnected
	s.failures = 0
	doneCh := make(chan struct{})
	s.connDoneCh = doneCh
	s.replayFilter.ResetSequence()
	s.mu.Unlock()

	// Launch reader loop for subsequent frames on conn
	s.readerWg.Add(1)
	go s.runReader(ctx, curGen, conn)

	// Process firstFrame if provided
	if firstFrame != nil {
		if err := s.replayFilter.CheckAndRecord(firstFrame); err == nil {
			if s.cfg.OnFrameReceived != nil {
				s.cfg.OnFrameReceived(s.peerID, firstFrame)
			}
		}
	}

	return true
}

// runReader continuously consumes framed messages from conn using DecodeFrame.
func (s *peerSupervisor) runReader(ctx context.Context, gen uint64, conn net.Conn) {
	defer s.readerWg.Done()
	defer s.disconnect(gen, io.EOF)

	for {
		// Existing DecodeFrame enforces Magic, MaxPayloadLength, and CRC32-IEEE integrity
		frame, err := DecodeFrame(conn)
		if err != nil {
			return
		}

		// Opcode namespace check: peer transport connections only accept valid peer RPCs (0x81..0x84)
		if !PeerMessageType(frame.Header.OpCode).Valid() {
			s.disconnect(gen, errors.ErrInvalidPeerMessage)
			return
		}

		// Flags/status validation
		switch PeerMessageType(frame.Header.OpCode) {
		case PeerOpRequestVote, PeerOpAppendEntries:
			if frame.Header.Flags != FlagNone {
				s.disconnect(gen, errors.ErrInvalidPeerPayload)
				return
			}
		case PeerOpRequestVoteResponse, PeerOpAppendEntriesResponse:
			if frame.Header.Status != StatusOk || frame.Header.Flags != FlagNone {
				s.disconnect(gen, errors.ErrInvalidPeerPayload)
				return
			}
		}

		// Replay & Duplicate rejection before invoking callbacks or consensus processing
		if err := s.replayFilter.CheckAndRecord(frame); err != nil {
			continue
		}

		if s.cfg.OnFrameReceived != nil {
			s.cfg.OnFrameReceived(s.peerID, frame)
		}
	}
}

// PeerConnectionManager manages persistent outbound connections to all remote peers in a cluster topology.
type PeerConnectionManager struct {
	topology    *cluster.Topology
	cfg         PeerConnectionConfig
	supervisors map[cluster.NodeID]*peerSupervisor

	started atomic.Bool
	closed  atomic.Bool

	lifecycleMu sync.Mutex
	nextSeqID   atomic.Uint64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	listener           net.Listener
	activeInboundConns atomic.Int64
}

// MaxInboundPeerConnections is the maximum number of concurrent inbound connections permitted on the peer listener.
const MaxInboundPeerConnections = 64

// NewPeerConnectionManager constructs a new PeerConnectionManager.
//
// Invariants enforced:
//   - topology must be non-nil.
//   - Config timeouts and intervals must be non-negative.
//   - ReconnectMin must be <= ReconnectMax.
//   - Self is excluded from supervisor allocation (never dialed).
//   - Exactly one supervisor is allocated per remote peer in topology.
//   - No network I/O is performed during construction.
func NewPeerConnectionManager(topology *cluster.Topology, cfg PeerConnectionConfig) (*PeerConnectionManager, error) {
	if topology == nil {
		return nil, errors.ErrNilReceiver
	}

	defaults := DefaultPeerConnectionConfig()
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = defaults.DialTimeout
	}
	if cfg.ReconnectMin == 0 {
		cfg.ReconnectMin = defaults.ReconnectMin
	}
	if cfg.ReconnectMax == 0 {
		cfg.ReconnectMax = defaults.ReconnectMax
	}
	if cfg.KeepAlivePeriod == 0 {
		cfg.KeepAlivePeriod = defaults.KeepAlivePeriod
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = defaults.WriteTimeout
	}

	if cfg.DialTimeout < 0 || cfg.ReconnectMin < 0 || cfg.ReconnectMax < 0 || cfg.KeepAlivePeriod < 0 || cfg.WriteTimeout < 0 {
		return nil, fmt.Errorf("%w: timeouts and intervals must be non-negative", errors.ErrInvalidManagerConfig)
	}

	if cfg.ReconnectMin > cfg.ReconnectMax {
		return nil, fmt.Errorf("%w: ReconnectMin (%v) cannot exceed ReconnectMax (%v)",
			errors.ErrInvalidManagerConfig, cfg.ReconnectMin, cfg.ReconnectMax)
	}

	if cfg.PeerTLSCertFile != "" || cfg.PeerTLSKeyFile != "" || cfg.PeerCAFile != "" {
		if cfg.PeerTLSCertFile == "" || cfg.PeerTLSKeyFile == "" || cfg.PeerCAFile == "" {
			return nil, fmt.Errorf("all of PeerTLSCertFile, PeerTLSKeyFile, and PeerCAFile must be provided for peer mTLS")
		}
		if cfg.ListenerTLSConfig == nil {
			listenerTLS, err := PeerServerTLSConfig(cfg.PeerTLSCertFile, cfg.PeerTLSKeyFile, cfg.PeerCAFile, topology)
			if err != nil {
				return nil, fmt.Errorf("failed to configure peer listener TLS: %w", err)
			}
			cfg.ListenerTLSConfig = listenerTLS
		}
	}

	if cfg.ListenerTLSConfig == nil && cfg.TLSConfig != nil {
		cfg.ListenerTLSConfig = cfg.TLSConfig
	}
	if cfg.TLSConfig == nil && cfg.ListenerTLSConfig != nil {
		cfg.TLSConfig = cfg.ListenerTLSConfig
	}

	if cfg.ListenerTLSConfig != nil {
		cfg.ListenerTLSConfig = WrapPeerServerTLSConfig(cfg.ListenerTLSConfig, topology)
	}

	// Non-loopback peer endpoints strictly mandate mutual TLS 1.3.
	// Plaintext and arbitrary one-way TLS configurations are prohibited on non-loopback peer destinations
	// regardless of InsecureTransport setting.
	for _, p := range topology.RemotePeers() {
		if topology.IsSelf(p.ID) {
			continue
		}
		if !isLoopbackAddress(p.Address) {
			if cfg.PeerTLSCertFile == "" && cfg.TLSConfig == nil && cfg.ListenerTLSConfig == nil {
				return nil, fmt.Errorf("%w: remote peer %d address %q is non-loopback; Raft peer transport mandates mutual TLS 1.3",
					errors.ErrInsecureTransport, p.ID, p.Address)
			}
			if cfg.ListenerTLSConfig != nil {
				if err := ValidatePeerTLSConfig(cfg.ListenerTLSConfig); err != nil {
					return nil, fmt.Errorf("%w: remote peer %d address %q listener TLS invalid: %v",
						errors.ErrInsecureTransport, p.ID, p.Address, err)
				}
			} else if cfg.PeerTLSCertFile == "" {
				return nil, fmt.Errorf("%w: remote peer %d address %q missing listener TLS config",
					errors.ErrInsecureTransport, p.ID, p.Address)
			}

			if cfg.TLSConfig != nil {
				if err := ValidatePeerDialerTLSConfig(cfg.TLSConfig); err != nil {
					return nil, fmt.Errorf("%w: remote peer %d address %q dialer TLS invalid: %v",
						errors.ErrInsecureTransport, p.ID, p.Address, err)
				}
			} else if cfg.PeerTLSCertFile == "" {
				return nil, fmt.Errorf("%w: remote peer %d address %q missing dialer TLS config",
					errors.ErrInsecureTransport, p.ID, p.Address)
			}
			break
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	remotePeers := topology.RemotePeers()
	supervisors := make(map[cluster.NodeID]*peerSupervisor, len(remotePeers))
	for _, p := range remotePeers {
		// Strict invariant: self is never dialed
		if topology.IsSelf(p.ID) {
			continue
		}
		peerCfg := cfg

		// Manager-owned outbound peer TLS configuration for remote peer p:
		if cfg.PeerTLSCertFile != "" {
			peerTLS, err := PeerClientTLSConfig(cfg.PeerTLSCertFile, cfg.PeerTLSKeyFile, cfg.PeerCAFile, p.ID, p.Address, topology)
			if err != nil {
				cancel()
				return nil, fmt.Errorf("failed to configure peer TLS dialer for node %d: %w", p.ID, err)
			}
			peerCfg.TLSConfig = peerTLS
		} else if cfg.TLSConfig != nil {
			peerTLS := WrapPeerClientTLSConfig(cfg.TLSConfig, p.ID, p.Address, topology)
			peerCfg.TLSConfig = peerTLS
		} else if !isLoopbackAddress(p.Address) {
			cancel()
			return nil, fmt.Errorf("%w: remote peer %d address %q is non-loopback; Raft peer transport mandates mutual TLS 1.3",
				errors.ErrInsecureTransport, p.ID, p.Address)
		}

		// Configure the raw transport dialer (seam for socket injection / test tracking):
		rawDialer := cfg.RawDialFunc
		if rawDialer == nil && cfg.DialFunc != nil {
			rawDialer = RawDialFunc(cfg.DialFunc)
		}
		if rawDialer == nil {
			dialTimeout := cfg.DialTimeout
			keepAlivePeriod := cfg.KeepAlivePeriod
			rawDialer = func(ctx context.Context, addr string) (net.Conn, error) {
				dialer := &net.Dialer{
					Timeout:   dialTimeout,
					KeepAlive: keepAlivePeriod,
				}
				return dialer.DialContext(ctx, "tcp", addr)
			}
		}
		peerCfg.RawDialFunc = rawDialer
		peerCfg.DialFunc = DialFunc(rawDialer)

		supervisors[p.ID] = newPeerSupervisor(p, peerCfg, topology)
	}

	return &PeerConnectionManager{
		topology:    topology,
		cfg:         cfg,
		supervisors: supervisors,
		ctx:         ctx,
		cancel:      cancel,
	}, nil
}

// NextSeqID returns a monotonically increasing sequence ID for framing peer messages.
func (m *PeerConnectionManager) NextSeqID() uint64 {
	return m.nextSeqID.Add(1)
}

// Start initiates supervisor lifecycle loops for all configured remote peers.
// Returns ErrManagerAlreadyStarted if called more than once, or ErrManagerClosed if already closed.
func (m *PeerConnectionManager) Start() error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	if m.closed.Load() {
		return errors.ErrManagerClosed
	}
	if !m.started.CompareAndSwap(false, true) {
		return errors.ErrManagerAlreadyStarted
	}

	for _, sup := range m.supervisors {
		m.wg.Add(1)
		go sup.run(m.ctx, &m.wg)
	}

	return nil
}

// StartListener binds a TCP listener on addr (or topology.LocalAddress() if addr is empty)
// and begins accepting incoming peer connections in the background.
func (m *PeerConnectionManager) StartListener(addr string) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	if m.closed.Load() {
		return errors.ErrManagerClosed
	}

	if addr == "" {
		if m.topology != nil {
			addr = m.topology.LocalAddress()
		}
	}
	if addr == "" {
		return nil
	}

	if !isLoopbackAddress(addr) {
		if err := ValidatePeerTLSConfig(m.cfg.ListenerTLSConfig); err != nil {
			return fmt.Errorf("%w: peer listener address %q is non-loopback; Raft peer transport mandates mutual TLS 1.3: %v",
				errors.ErrInsecureTransport, addr, err)
		}
	}

	var ln net.Listener
	var err error
	if m.cfg.ListenerTLSConfig != nil {
		listenerTLS := WrapPeerServerTLSConfig(m.cfg.ListenerTLSConfig, m.topology)
		ln, err = tls.Listen("tcp", addr, listenerTLS)
	} else {
		ln, err = net.Listen("tcp", addr)
	}
	if err != nil {
		return err
	}

	m.listener = ln
	m.wg.Add(1)
	go m.acceptLoop(ln)

	return nil
}

// ServeListener accepts incoming connections from an existing listener.
func (m *PeerConnectionManager) ServeListener(ln net.Listener) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	if m.closed.Load() {
		return errors.ErrManagerClosed
	}
	if ln == nil {
		return errors.ErrNilReceiver
	}

	if !isLoopbackAddress(ln.Addr().String()) {
		if err := ValidatePeerTLSConfig(m.cfg.ListenerTLSConfig); err != nil {
			return fmt.Errorf("%w: peer listener address %q is non-loopback; Raft peer transport mandates mutual TLS 1.3: %v",
				errors.ErrInsecureTransport, ln.Addr().String(), err)
		}
	}

	if m.cfg.ListenerTLSConfig != nil {
		listenerTLS := WrapPeerServerTLSConfig(m.cfg.ListenerTLSConfig, m.topology)
		ln = tls.NewListener(ln, listenerTLS)
	}

	m.listener = ln
	m.wg.Add(1)
	go m.acceptLoop(ln)

	return nil
}

// ListenerAddr returns the network address the peer listener is bound to,
// or nil if no listener is currently running.
func (m *PeerConnectionManager) ListenerAddr() net.Addr {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if m.listener != nil {
		return m.listener.Addr()
	}
	return nil
}

// ActiveInboundConnections returns the current count of active inbound peer connections.
func (m *PeerConnectionManager) ActiveInboundConnections() int64 {
	return m.activeInboundConns.Load()
}

func (m *PeerConnectionManager) acceptLoop(ln net.Listener) {
	defer m.wg.Done()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if m.closed.Load() {
				return
			}
			// Transient accept error
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
				continue
			}
		}

		if m.activeInboundConns.Load() >= MaxInboundPeerConnections {
			_ = conn.Close()
			continue
		}

		m.activeInboundConns.Add(1)
		m.wg.Add(1)
		go func(c net.Conn) {
			defer m.activeInboundConns.Add(-1)
			m.handleInboundConn(c)
		}(conn)
	}
}

func (m *PeerConnectionManager) handleInboundConn(conn net.Conn) {
	defer m.wg.Done()

	var authenticatedNodeID cluster.NodeID
	if tc, ok := conn.(*tls.Conn); ok {
		handshakeTimeout := m.cfg.DialTimeout
		if handshakeTimeout <= 0 {
			handshakeTimeout = 3 * time.Second
		}
		handshakeCtx, handshakeCancel := context.WithTimeout(m.ctx, handshakeTimeout)
		err := tc.HandshakeContext(handshakeCtx)
		handshakeCancel()
		if err != nil {
			_ = conn.Close()
			return
		}

		cs := tc.ConnectionState()
		if len(cs.PeerCertificates) == 0 {
			_ = conn.Close()
			return
		}
		leaf := cs.PeerCertificates[0]
		nodeID, err := ExtractNodeIDFromCert(leaf)
		if err != nil {
			_ = conn.Close()
			return
		}
		authenticatedNodeID = nodeID
	}

	if !isLoopbackAddress(conn.RemoteAddr().String()) && authenticatedNodeID == 0 {
		_ = conn.Close()
		return
	}

	// Deadline for first request frame to defend against Slowloris
	if err := conn.SetReadDeadline(time.Now().Add(m.cfg.DialTimeout)); err != nil {
		_ = conn.Close()
		return
	}

	frame, err := DecodeFrame(conn)
	if err != nil {
		_ = conn.Close()
		return
	}

	// Reset read deadline after first frame read
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return
	}

	// Validate opcode
	op := PeerMessageType(frame.Header.OpCode)
	if !op.Valid() {
		_ = conn.Close()
		return
	}
	if frame.Header.Flags != FlagNone {
		_ = conn.Close()
		return
	}

	var fromPeerID cluster.NodeID
	switch op {
	case PeerOpAppendEntries:
		req, err := DecodeAppendEntries(frame)
		if err != nil {
			_ = conn.Close()
			return
		}
		fromPeerID = cluster.NodeID(req.LeaderID)
	case PeerOpRequestVote:
		req, err := DecodeRequestVote(frame)
		if err != nil {
			_ = conn.Close()
			return
		}
		fromPeerID = cluster.NodeID(req.CandidateID)
	default:
		_ = conn.Close()
		return
	}

	if !fromPeerID.IsValid() || m.topology.IsSelf(fromPeerID) {
		_ = conn.Close()
		return
	}

	// If mTLS is authenticated, verify that the frame's claimed NodeID matches certificate identity!
	if authenticatedNodeID != 0 && fromPeerID != authenticatedNodeID {
		_ = conn.Close()
		return
	}

	sup, ok := m.supervisors[fromPeerID]
	if !ok {
		_ = conn.Close()
		return
	}

	// Hand over connection to supervisor
	sup.adoptInboundConnection(m.ctx, conn, frame, m.topology.LocalID())
}

// Send serializes and transmits a protocol frame to target peerID.
//
// Invariants enforced:
//   - Frame must be non-nil.
//   - Manager must be started and not closed.
//   - peerID must exist in cluster topology.
//   - Concurrent sends on the same connection are serialized via a per-peer mutex.
//   - Writes are deadline-bounded by WriteTimeout.
//   - Uses existing EncodeFrame without modifying headers or checksum semantics.
//   - Failed write triggers connection teardown with generation check.
func (m *PeerConnectionManager) Send(ctx context.Context, peerID cluster.NodeID, frame *Frame) error {
	if frame == nil {
		return errors.ErrNilReceiver
	}
	if m.closed.Load() {
		return errors.ErrManagerClosed
	}
	if !m.started.Load() {
		return errors.ErrManagerNotStarted
	}

	sup, ok := m.supervisors[peerID]
	if !ok {
		return &errors.UnknownPeerError{NodeID: uint64(peerID)}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// Serialize concurrent writes per connection without global manager lock contention
	sup.writeMu.Lock()
	defer sup.writeMu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	sup.mu.RLock()
	state := sup.state
	conn := sup.conn
	gen := sup.generation
	sup.mu.RUnlock()

	if state != PeerStateConnected || conn == nil {
		return &errors.PeerUnavailableError{NodeID: uint64(peerID), State: state.String()}
	}

	// Apply write deadline
	writeDeadline := time.Now().Add(sup.cfg.WriteTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(writeDeadline) {
		writeDeadline = ctxDeadline
	}
	if err := conn.SetWriteDeadline(writeDeadline); err != nil {
		sup.disconnect(gen, err)
		return err
	}
	defer func() { _ = conn.SetWriteDeadline(time.Time{}) }()

	// Encode and write frame using existing generic framing encoder
	if err := EncodeFrame(conn, frame); err != nil {
		sup.disconnect(gen, err)
		return err
	}

	return nil
}

// GetPeerState returns the current lifecycle state of the target peer.
func (m *PeerConnectionManager) GetPeerState(peerID cluster.NodeID) (PeerState, error) {
	if m.closed.Load() {
		return PeerStateDisconnected, errors.ErrManagerClosed
	}
	sup, ok := m.supervisors[peerID]
	if !ok {
		return PeerStateDisconnected, &errors.UnknownPeerError{NodeID: uint64(peerID)}
	}
	return sup.getState(), nil
}

// ConnectedPeers returns a deterministically sorted slice of NodeIDs currently in PeerStateConnected.
func (m *PeerConnectionManager) ConnectedPeers() []cluster.NodeID {
	if m.closed.Load() || !m.started.Load() {
		return nil
	}

	var connected []cluster.NodeID
	for id, sup := range m.supervisors {
		if sup.getState() == PeerStateConnected {
			connected = append(connected, id)
		}
	}

	sort.Slice(connected, func(i, j int) bool {
		return connected[i] < connected[j]
	})

	return connected
}

// IsConnected reports whether the target peer currently has an established, healthy connection.
func (m *PeerConnectionManager) IsConnected(peerID cluster.NodeID) bool {
	state, err := m.GetPeerState(peerID)
	if err != nil {
		return false
	}
	return state == PeerStateConnected
}

// Close gracefully and deterministically shuts down the connection manager.
// Idempotent: safe to invoke concurrently and repeatedly.
// Closes all active sockets, interrupts backoffs and dials, and drains all goroutines.
func (m *PeerConnectionManager) Close() error {
	m.lifecycleMu.Lock()
	if !m.closed.CompareAndSwap(false, true) {
		m.lifecycleMu.Unlock()
		return nil
	}

	// Cancel root context to interrupt all pending dials and backoff sleeps
	if m.cancel != nil {
		m.cancel()
	}

	// Close listener if active
	if m.listener != nil {
		_ = m.listener.Close()
		m.listener = nil
	}

	// Close all currently active sockets immediately to unblock blocked readers/writers
	for _, sup := range m.supervisors {
		sup.mu.Lock()
		sup.state = PeerStateClosing
		if sup.conn != nil {
			_ = sup.conn.Close()
			sup.conn = nil
			if sup.connDoneCh != nil {
				select {
				case <-sup.connDoneCh:
				default:
					close(sup.connDoneCh)
				}
			}
		}
		sup.mu.Unlock()
	}
	m.lifecycleMu.Unlock()

	// Wait for all supervisor and reader goroutines to terminate
	m.wg.Wait()

	return nil
}
