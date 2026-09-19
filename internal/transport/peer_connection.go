package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

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

// DialFunc abstracts network dialing, allowing loopback, mocks, or future mTLS dialers.
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

	// DialFunc is the pluggable dialer function (defaults to standard TCP dialer with keep-alive).
	DialFunc DialFunc

	// OnFrameReceived is invoked when a valid frame is received from a remote peer.
	OnFrameReceived PeerFrameHandler
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

// peerSupervisor manages the complete connection lifecycle for a single remote peer.
type peerSupervisor struct {
	peerID  cluster.NodeID
	address string
	cfg     PeerConnectionConfig

	mu         sync.RWMutex
	state      PeerState
	conn       net.Conn
	generation uint64
	connDoneCh chan struct{}
	failures   int

	writeMu sync.Mutex
}

func newPeerSupervisor(p cluster.Peer, cfg PeerConnectionConfig) *peerSupervisor {
	return &peerSupervisor{
		peerID:   p.ID,
		address:  p.Address,
		cfg:      cfg,
		state:    PeerStateDisconnected,
		failures: 0,
	}
}

// getState returns the current lifecycle state of the supervisor.
func (s *peerSupervisor) getState() PeerState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// calculateBackoff computes exponential backoff delay based on consecutive failure count.
func (s *peerSupervisor) calculateBackoff(failures int) time.Duration {
	if failures <= 1 {
		return s.cfg.ReconnectMin
	}
	shift := failures - 1
	if shift > 30 {
		shift = 30
	}
	delay := s.cfg.ReconnectMin * (1 << shift)
	if delay > s.cfg.ReconnectMax || delay < 0 {
		delay = s.cfg.ReconnectMax
	}
	return delay
}

// disconnect tears down the connection for the given generation token.
// Guarantees that stale failure notifications from older connections cannot disconnect
// a newer active connection (P14-M03-INV-07).
func (s *peerSupervisor) disconnect(gen uint64, reason error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// If the generation does not match the active generation, this is a stale teardown event.
	if s.generation != gen || s.conn == nil {
		return
	}

	connToClose := s.conn
	s.conn = nil
	s.state = PeerStateDisconnected

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
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.state = PeerStateClosing
			s.mu.Unlock()
			return
		default:
		}

		// Transition to Connecting
		s.mu.Lock()
		s.state = PeerStateConnecting
		s.mu.Unlock()

		// Attempt dial with timeout
		dialCtx, dialCancel := context.WithTimeout(ctx, s.cfg.DialTimeout)
		conn, err := s.cfg.DialFunc(dialCtx, s.address)
		dialCancel()

		if err != nil {
			select {
			case <-ctx.Done():
				s.mu.Lock()
				s.state = PeerStateClosing
				s.mu.Unlock()
				return
			default:
			}

			s.mu.Lock()
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

		// Dial succeeded; check if context was cancelled during dial
		select {
		case <-ctx.Done():
			_ = conn.Close()
			s.mu.Lock()
			s.state = PeerStateClosing
			s.mu.Unlock()
			return
		default:
		}

		// Configure TCP keep-alive
		configureKeepAlive(conn, s.cfg.KeepAlivePeriod)

		// Install new active connection with monotonic generation
		s.mu.Lock()
		s.generation++
		curGen := s.generation
		s.conn = conn
		s.state = PeerStateConnected
		s.failures = 0
		doneCh := make(chan struct{})
		s.connDoneCh = doneCh
		s.mu.Unlock()

		// Launch reader loop
		wg.Add(1)
		go s.runReader(ctx, curGen, conn, wg)

		// Await connection termination or manager shutdown
		select {
		case <-ctx.Done():
			s.disconnect(curGen, ctx.Err())
			s.mu.Lock()
			s.state = PeerStateClosing
			s.mu.Unlock()
			return
		case <-doneCh:
			if ctx.Err() != nil {
				s.mu.Lock()
				s.state = PeerStateClosing
				s.mu.Unlock()
				return
			}

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
	}
}

// runReader continuously consumes framed messages from conn using DecodeFrame.
func (s *peerSupervisor) runReader(ctx context.Context, gen uint64, conn net.Conn, wg *sync.WaitGroup) {
	defer wg.Done()
	defer s.disconnect(gen, io.EOF)

	for {
		// Existing DecodeFrame enforces Magic, MaxPayloadLength, and CRC32-IEEE integrity
		frame, err := DecodeFrame(conn)
		if err != nil {
			return
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

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

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

	if cfg.DialFunc == nil {
		dialTimeout := cfg.DialTimeout
		keepAlivePeriod := cfg.KeepAlivePeriod
		cfg.DialFunc = func(ctx context.Context, addr string) (net.Conn, error) {
			dialer := &net.Dialer{
				Timeout:   dialTimeout,
				KeepAlive: keepAlivePeriod,
			}
			conn, err := dialer.DialContext(ctx, "tcp", addr)
			if err != nil {
				return nil, err
			}
			configureKeepAlive(conn, keepAlivePeriod)
			return conn, nil
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
		supervisors[p.ID] = newPeerSupervisor(p, cfg)
	}

	return &PeerConnectionManager{
		topology:    topology,
		cfg:         cfg,
		supervisors: supervisors,
		ctx:         ctx,
		cancel:      cancel,
	}, nil
}

// Start initiates supervisor lifecycle loops for all configured remote peers.
// Returns ErrManagerAlreadyStarted if called more than once, or ErrManagerClosed if already closed.
func (m *PeerConnectionManager) Start() error {
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
	if ds, ok := conn.(deadlineSetter); ok {
		_ = ds.SetWriteDeadline(writeDeadline)
		defer func() { _ = ds.SetWriteDeadline(time.Time{}) }()
	}

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
	if !m.closed.CompareAndSwap(false, true) {
		return nil
	}

	// Cancel root context to interrupt all pending dials and backoff sleeps
	if m.cancel != nil {
		m.cancel()
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

	// Wait for all supervisor and reader goroutines to terminate
	m.wg.Wait()

	return nil
}
