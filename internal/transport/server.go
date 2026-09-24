package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	stdErrors "errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/metrics"
)

// Engine defines the storage engine contract required by the transport server dispatcher.
// It is satisfied by *engine.Engine.
type Engine interface {
	Put(ctx context.Context, key, val []byte) error
	Get(key []byte) ([]byte, error)
	Delete(ctx context.Context, key []byte) error
	Batch(ctx context.Context, batch []binary.BatchOp) error
	Exists(key []byte) (bool, error)
	Stats() (EngineStats, MemoryStats, StorageStats, CacheStats, error)
}

// ProposalRouter defines the interface for routing client write mutations in replicated mode (P16-S01-M02).
type ProposalRouter interface {
	// RouteWrite handles client write requests (OpPut, OpDelete) in a Raft cluster.
	// If the current node is the leader, the mutation is proposed to consensus.
	// If the current node is a follower/non-leader, leader redirection is returned.
	RouteWrite(ctx context.Context, req *Request) (*Response, error)
}

// ReadRouter defines the interface for routing client read requests in replicated mode (P17-S01-M02).
type ReadRouter interface {
	// RouteRead handles client read requests (OpGet) in a Raft cluster.
	// If the current node is the leader, ReadIndex is executed, waits for lastApplied >= readIndex,
	// revalidates continuous leadership, and serves from the local engine.
	// If the current node is a follower/non-leader, leader redirection is returned.
	RouteRead(ctx context.Context, req *Request) (*Response, error)
}

// ServerConfig configures the TCP transport server.
type ServerConfig struct {
	// Address is the TCP address to bind and listen on (default: "127.0.0.1:9099").
	Address string

	// InsecureTransport explicitly permits unencrypted plaintext TCP on non-loopback network interfaces.
	// When false (default), binding to any non-loopback address (e.g. 0.0.0.0, public IP) without TLS is rejected with ErrInsecureTransport.
	InsecureTransport bool

	// TLSConfig provides full TLS 1.3 configuration. If non-nil, Server runs over TLS.
	TLSConfig *tls.Config

	// TLSCertFile and TLSKeyFile provide paths to X.509 certificate and private key files.
	// If set and TLSConfig is nil, Server automatically builds a hardened TLS 1.3 configuration.
	TLSCertFile string
	TLSKeyFile  string

	// ClientCAFile provides the path to a trusted CA certificate bundle for client mTLS.
	// When provided, client certificates are verified against this CA bundle.
	ClientCAFile string

	// RequireClientCert enforces mandatory client certificate authentication (mTLS) when ClientCAFile is configured.
	RequireClientCert bool

	// MaxConnections is the maximum number of concurrent client connections (default: 4096).
	MaxConnections int

	// MaxInFlightPerConn is the maximum number of concurrent in-flight requests permitted per TCP connection (default: 64).
	MaxInFlightPerConn int

	// MaxGlobalInFlight is the maximum total number of concurrent in-flight requests permitted across all connections (default: 16384).
	MaxGlobalInFlight int

	// HeaderTimeout is the maximum duration allowed to read a frame header (default: 5s).
	HeaderTimeout time.Duration

	// PayloadTimeout is the maximum duration allowed to read a frame payload and trailer (default: 10s).
	PayloadTimeout time.Duration

	// IdleTimeout is the maximum duration an established connection may remain idle between requests (default: 60s).
	IdleTimeout time.Duration

	// WriteTimeout is the maximum duration allowed to write a response frame (default: 5s).
	WriteTimeout time.Duration

	// RequestTimeout is the timeout passed in the context to Engine operations (default: 5s).
	RequestTimeout time.Duration

	// ShutdownTimeout is the maximum duration to wait for active connections to drain during Close (default: 5s).
	ShutdownTimeout time.Duration

	// ProposalRouter routes client write mutations to the consensus subsystem in replicated mode (P16-S01-M02).
	// When nil (default), Server operates in standalone local engine mode.
	ProposalRouter ProposalRouter

	// ReadRouter routes client read requests to consensus in replicated mode (P17-S01-M02).
	// When nil and ClusterMode is true, Server checks if ProposalRouter satisfies ReadRouter.
	// If neither is available in cluster mode, reads fail closed with StatusError.
	ReadRouter ReadRouter

	// ClusterMode indicates that this server is part of a Raft cluster (P16-SEC-F01).
	// When true, all client write mutations (PUT, DELETE) MUST go through the ProposalRouter,
	// and all client reads (GET) MUST go through the ReadRouter.
	ClusterMode bool
}

const (
	// DefaultMaxInFlightPerConn defines the default maximum concurrent in-flight requests per connection.
	DefaultMaxInFlightPerConn = 64

	// DefaultMaxGlobalInFlight defines the default global ceiling for simultaneously active requests across all connections.
	DefaultMaxGlobalInFlight = 16384
)

// DefaultServerConfig returns a production-hardened ServerConfig with safe default timeouts and limits.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		Address:            "127.0.0.1:9099",
		MaxConnections:     4096,
		MaxInFlightPerConn: DefaultMaxInFlightPerConn,
		MaxGlobalInFlight:  DefaultMaxGlobalInFlight,
		HeaderTimeout:      5 * time.Second,
		PayloadTimeout:     10 * time.Second,
		IdleTimeout:        60 * time.Second,
		WriteTimeout:       5 * time.Second,
		RequestTimeout:     5 * time.Second,
		ShutdownTimeout:    5 * time.Second,
	}
}

// isLoopbackAddress reports whether the given TCP address specifies a loopback interface.
func isLoopbackAddress(addr string) bool {
	if addr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") || host == "127.0.0.1" || host == "::1" || host == "pipe" || host == "local" {
		return true
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// Server manages the TCP listener, connection lifecycle, Slowloris defenses, and Engine request dispatching.
type Server struct {
	cfg      ServerConfig
	engine   Engine
	listener net.Listener
	addr     net.Addr

	mu           sync.Mutex
	conns        map[net.Conn]struct{}
	started      atomic.Bool
	closed       atomic.Bool
	shutdownCh   chan struct{}
	shutdownDone chan struct{}
	wg           sync.WaitGroup
	activeConns  atomic.Int64

	globalInFlight    atomic.Int64
	maxGlobalInFlight int64

	responseWriteHookMu sync.Mutex
	responseWriteHook   func(resp *Response)

	routerMu    sync.RWMutex
	router      ProposalRouter
	readRouter  ReadRouter
	clusterMode bool // immutable after construction (P16-SEC-F01)
}

// responseEnvelope packages a Response with its associated SeqID reservation metadata
// for safe asynchronous transmission and post-write resource release.
type responseEnvelope struct {
	resp          *Response
	releaseID     uint64
	shouldRelease bool
}

// SetResponseWriteHookForTesting configures an optional callback invoked by the response
// writer immediately before writing a response frame to the wire.
func (s *Server) SetResponseWriteHookForTesting(fn func(resp *Response)) {
	s.responseWriteHookMu.Lock()
	defer s.responseWriteHookMu.Unlock()
	s.responseWriteHook = fn
}

// NewServer constructs a new transport Server. It validates the configuration and verifies that eng is non-nil.
func NewServer(cfg ServerConfig, eng Engine) (*Server, error) {
	if eng == nil {
		return nil, errors.ErrNilReceiver
	}

	defaults := DefaultServerConfig()
	if cfg.Address == "" {
		cfg.Address = defaults.Address
	}

	isLoopback := isLoopbackAddress(cfg.Address)
	hasTLS := cfg.TLSConfig != nil || cfg.TLSCertFile != ""

	// Finding A: Non-loopback client transport security policy enforcement
	if !isLoopback {
		if !hasTLS && !cfg.InsecureTransport {
			return nil, errors.ErrInsecureTransport
		}
		if hasTLS {
			// External production transport with TLS mandates client mutual TLS (mTLS)
			if cfg.TLSConfig != nil {
				if err := ValidateClientServerTLSConfig(cfg.TLSConfig); err != nil {
					return nil, fmt.Errorf("%w: non-loopback address %q with TLS requires valid mTLS configuration: %v",
						errors.ErrInsecureTransport, cfg.Address, err)
				}
			} else {
				if cfg.ClientCAFile == "" || !cfg.RequireClientCert {
					return nil, fmt.Errorf("%w: non-loopback address %q with TLS requires ClientCAFile and RequireClientCert=true for production mTLS",
						errors.ErrInsecureTransport, cfg.Address)
				}
			}
		}
	}

	if cfg.TLSConfig == nil && cfg.TLSCertFile != "" {
		tlsCfg, err := ServerTLSConfig(cfg.TLSCertFile, cfg.TLSKeyFile, cfg.ClientCAFile, cfg.RequireClientCert)
		if err != nil {
			return nil, err
		}
		cfg.TLSConfig = tlsCfg
	} else if cfg.TLSConfig != nil {
		cfg.TLSConfig = WrapClientServerTLSConfig(cfg.TLSConfig, isLoopback)
	}
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = defaults.MaxConnections
	}
	if cfg.MaxInFlightPerConn <= 0 {
		cfg.MaxInFlightPerConn = defaults.MaxInFlightPerConn
	}
	if cfg.MaxGlobalInFlight <= 0 {
		cfg.MaxGlobalInFlight = defaults.MaxGlobalInFlight
	}
	if cfg.HeaderTimeout <= 0 {
		cfg.HeaderTimeout = defaults.HeaderTimeout
	}
	if cfg.PayloadTimeout <= 0 {
		cfg.PayloadTimeout = defaults.PayloadTimeout
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = defaults.IdleTimeout
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = defaults.WriteTimeout
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = defaults.RequestTimeout
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = defaults.ShutdownTimeout
	}

	readRouter := cfg.ReadRouter
	if readRouter == nil {
		if rr, ok := cfg.ProposalRouter.(ReadRouter); ok {
			readRouter = rr
		}
	}

	return &Server{
		cfg:               cfg,
		engine:            eng,
		router:            cfg.ProposalRouter,
		readRouter:        readRouter,
		clusterMode:       cfg.ClusterMode,
		maxGlobalInFlight: int64(cfg.MaxGlobalInFlight),
		conns:             make(map[net.Conn]struct{}),
		shutdownCh:        make(chan struct{}),
		shutdownDone:      make(chan struct{}),
	}, nil
}

// SetProposalRouter configures the authoritative consensus proposal router for replicated writes (P16-S01-M02).
// When non-nil, PUT and DELETE requests are routed through consensus.
// In cluster mode, setting the router to nil does NOT re-enable direct Engine writes;
// writes remain fail-closed until a valid router is provided (P16-SEC-F09).
func (s *Server) SetProposalRouter(r ProposalRouter) {
	if s == nil {
		return
	}
	s.routerMu.Lock()
	defer s.routerMu.Unlock()
	s.router = r
}

// ProposalRouter returns the currently configured proposal router, or nil if operating in local engine mode.
func (s *Server) ProposalRouter() ProposalRouter {
	if s == nil {
		return nil
	}
	s.routerMu.RLock()
	defer s.routerMu.RUnlock()
	return s.router
}

// SetReadRouter configures the authoritative consensus read router for linearizable reads (P17-S01-M02).
func (s *Server) SetReadRouter(r ReadRouter) {
	if s == nil {
		return
	}
	s.routerMu.Lock()
	defer s.routerMu.Unlock()
	s.readRouter = r
}

// ReadRouter returns the currently configured read router, or nil if operating in local engine mode.
func (s *Server) ReadRouter() ReadRouter {
	if s == nil {
		return nil
	}
	s.routerMu.RLock()
	defer s.routerMu.RUnlock()
	if s.readRouter != nil {
		return s.readRouter
	}
	if rr, ok := s.router.(ReadRouter); ok {
		return rr
	}
	return nil
}

// IsClusterMode reports whether the server was constructed in cluster mode (P16-SEC-F01).
// Cluster mode is immutable after construction.
func (s *Server) IsClusterMode() bool {
	if s == nil {
		return false
	}
	return s.clusterMode
}

// IsServing reports whether the transport server is active and accepting connections.
// Returns false if the server is nil, not yet started, or currently shutting down/closed.
func (s *Server) IsServing() bool {
	if s == nil {
		return false
	}
	return s.started.Load() && !s.closed.Load()
}

// Listen binds on addr and starts the accept loop in a background goroutine.
// If addr is empty, the configured Address is used.
func (s *Server) Listen(addr string) error {
	if !s.started.CompareAndSwap(false, true) {
		return errors.ErrServerAlreadyStarted
	}

	bindAddr := addr
	if bindAddr == "" {
		bindAddr = s.cfg.Address
	}
	isLoopback := isLoopbackAddress(s.cfg.Address) && isLoopbackAddress(bindAddr)
	if !isLoopback {
		if s.cfg.TLSConfig == nil && !s.cfg.InsecureTransport {
			s.started.Store(false)
			return errors.ErrInsecureTransport
		}
		if s.cfg.TLSConfig != nil {
			if err := ValidateClientServerTLSConfig(s.cfg.TLSConfig); err != nil {
				s.started.Store(false)
				return fmt.Errorf("%w: non-loopback address %q requires mutual TLS: %v", errors.ErrInsecureTransport, bindAddr, err)
			}
		}
	}

	var l net.Listener
	var err error
	if s.cfg.TLSConfig != nil {
		tlsCfg := WrapClientServerTLSConfig(s.cfg.TLSConfig, isLoopback)
		l, err = tls.Listen("tcp", bindAddr, tlsCfg)
	} else {
		l, err = net.Listen("tcp", bindAddr)
	}
	if err != nil {
		s.started.Store(false)
		return err
	}

	s.listener = l
	s.addr = l.Addr()

	go s.acceptLoop(l)
	return nil
}

// Serve accepts incoming connections on listener l and runs the accept loop synchronously.
// It blocks until the server is closed or a fatal listener error occurs.
func (s *Server) Serve(l net.Listener) error {
	if l == nil {
		return errors.ErrNilReceiver
	}
	if !s.started.CompareAndSwap(false, true) {
		return errors.ErrServerAlreadyStarted
	}

	addrStr := l.Addr().String()
	isLoopback := isLoopbackAddress(s.cfg.Address) && isLoopbackAddress(addrStr)
	if !isLoopback {
		if s.cfg.TLSConfig == nil && !s.cfg.InsecureTransport {
			s.started.Store(false)
			return errors.ErrInsecureTransport
		}
		if s.cfg.TLSConfig != nil {
			if err := ValidateClientServerTLSConfig(s.cfg.TLSConfig); err != nil {
				s.started.Store(false)
				return fmt.Errorf("%w: non-loopback address %q requires mutual TLS: %v", errors.ErrInsecureTransport, addrStr, err)
			}
		}
	}

	if s.cfg.TLSConfig != nil {
		tlsCfg := WrapClientServerTLSConfig(s.cfg.TLSConfig, isLoopback)
		l = tls.NewListener(l, tlsCfg)
	}

	s.listener = l
	s.addr = l.Addr()

	s.acceptLoop(l)
	return errors.ErrServerClosed
}

// Addr returns the network address the server is bound to, or nil if not listening.
func (s *Server) Addr() net.Addr {
	if s.listener != nil {
		return s.listener.Addr()
	}
	return s.addr
}

// ActiveConnections returns the current count of active client connections.
func (s *Server) ActiveConnections() int {
	return int(s.activeConns.Load())
}

// acceptLoop runs the listener accept loop with backoff on temporary errors.
func (s *Server) acceptLoop(l net.Listener) {
	defer l.Close()

	var tempDelay time.Duration
	for {
		conn, err := l.Accept()
		if err != nil {
			if s.closed.Load() {
				return
			}
			var ne net.Error
			//nolint:staticcheck // Temporary is used intentionally to handle transient network errors (e.g. EMFILE)
			if stdErrors.As(err, &ne) && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				select {
				case <-s.shutdownCh:
					return
				case <-time.After(tempDelay):
				}
				continue
			}
			return
		}
		tempDelay = 0

		// Reject immediately if active connections exceed ceiling
		if s.cfg.MaxConnections > 0 && s.activeConns.Load() >= int64(s.cfg.MaxConnections) {
			conn.Close()
			continue
		}

		if !s.trackConn(conn) {
			conn.Close()
			continue
		}

		go s.handleConn(conn)
	}
}

// trackConn registers an accepted connection under s.mu. Returns false if server is closing or at capacity.
func (s *Server) trackConn(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return false
	}
	if s.cfg.MaxConnections > 0 && len(s.conns) >= s.cfg.MaxConnections {
		return false
	}
	s.conns[conn] = struct{}{}
	s.activeConns.Add(1)
	metrics.ActiveConnections.Add(1)
	s.wg.Add(1)
	return true
}

// untrackConn unregisters a closed connection under s.mu.
func (s *Server) untrackConn(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.conns[conn]; ok {
		delete(s.conns, conn)
		s.activeConns.Add(-1)
		metrics.ActiveConnections.Add(-1)
		s.wg.Done()
	}
}

// handleConn is the per-connection request-processing loop supporting bounded out-of-order request pipelining.
func (s *Server) handleConn(conn net.Conn) {
	connCtx, connCancel := context.WithCancel(context.Background())
	defer func() {
		connCancel()
		_ = conn.Close()
		s.untrackConn(conn)
	}()

	isLoopback := isLoopbackAddress(s.cfg.Address)
	if conn.LocalAddr() != nil && !isLoopbackAddress(conn.LocalAddr().String()) {
		isLoopback = false
	}
	if conn.RemoteAddr() != nil && !isLoopbackAddress(conn.RemoteAddr().String()) {
		isLoopback = false
	}

	if tc, ok := conn.(*tls.Conn); ok {
		handshakeTimeout := s.cfg.HeaderTimeout
		if handshakeTimeout <= 0 {
			handshakeTimeout = 5 * time.Second
		}
		handshakeCtx, handshakeCancel := context.WithTimeout(connCtx, handshakeTimeout)
		err := tc.HandshakeContext(handshakeCtx)
		handshakeCancel()
		if err != nil {
			return
		}

		cs := tc.ConnectionState()
		if !isLoopback || (s.cfg.TLSConfig != nil && s.cfg.TLSConfig.ClientAuth == tls.RequireAndVerifyClientCert) {
			if !cs.HandshakeComplete {
				return
			}
			if cs.Version < tls.VersionTLS13 {
				return
			}
			if len(cs.PeerCertificates) == 0 {
				return
			}
			if len(cs.VerifiedChains) == 0 {
				return
			}
			leaf := cs.PeerCertificates[0]
			if IsPeerCertificate(leaf) && !IsClientCertificate(leaf) {
				return
			}
			if s.cfg.TLSConfig != nil && s.cfg.TLSConfig.ClientCAs != nil {
				opts := x509.VerifyOptions{
					Roots:         s.cfg.TLSConfig.ClientCAs,
					CurrentTime:   time.Now(),
					Intermediates: x509.NewCertPool(),
					KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
				}
				for _, cert := range cs.PeerCertificates[1:] {
					opts.Intermediates.AddCert(cert)
				}
				if _, err := leaf.Verify(opts); err != nil {
					return
				}
			}
		}
	} else if !isLoopback && !s.cfg.InsecureTransport {
		return
	}

	maxInFlight := s.cfg.MaxInFlightPerConn
	if maxInFlight <= 0 {
		maxInFlight = DefaultMaxInFlightPerConn
	}

	inFlightSem := make(chan struct{}, maxInFlight)
	respCh := make(chan responseEnvelope, maxInFlight)
	writerDone := make(chan struct{})

	var inFlightWG sync.WaitGroup
	var activeMu sync.Mutex
	activeSeqIDs := make(map[uint64]struct{})

	// Dedicated response writer goroutine: guarantees exactly one goroutine writes to conn,
	// preventing response frame byte interleaving under concurrent request dispatch.
	// SeqIDs remain strictly reserved until the response frame is completely written to the wire.
	go func() {
		defer close(writerDone)
		for env := range respCh {
			s.responseWriteHookMu.Lock()
			hook := s.responseWriteHook
			s.responseWriteHookMu.Unlock()
			if hook != nil {
				hook(env.resp)
			}

			err := s.writeResponseWithDeadline(conn, env.resp)
			if env.shouldRelease {
				activeMu.Lock()
				delete(activeSeqIDs, env.releaseID)
				activeMu.Unlock()
			}
			if err != nil {
				// Socket write failure (client disconnect, timeout, or broken pipe).
				// Abort the connection context and close the connection descriptor
				// to unblock the reader goroutine immediately.
				connCancel()
				_ = conn.Close()
				return
			}
		}
	}()

	isFirst := true
	for {
		if s.closed.Load() || connCtx.Err() != nil {
			break
		}

		// 1. Connection-level backpressure admission control:
		// Wait until an in-flight slot is available. Natural TCP window flow control
		// prevents unbounded reading and memory allocation when the pipeline is saturated.
		select {
		case inFlightSem <- struct{}{}:
		case <-connCtx.Done():
			break
		case <-s.shutdownCh:
			break
		}

		if connCtx.Err() != nil || s.closed.Load() {
			select {
			case <-inFlightSem:
			default:
			}
			break
		}

		// 2. Read frame with deadlines
		frame, err := s.readFrameWithDeadlines(conn, isFirst)
		if err != nil {
			<-inFlightSem
			// Normal EOF, read timeout (Slowloris/idle), or fatal framing error.
			break
		}
		isFirst = false

		// 3. Global admission control check
		if s.maxGlobalInFlight > 0 && s.globalInFlight.Add(1) > s.maxGlobalInFlight {
			s.globalInFlight.Add(-1)
			metrics.PipelineRejections.Add(1)
			resp := &Response{
				OpCode:  frame.Header.OpCode,
				SeqID:   frame.Header.SeqID,
				Status:  StatusThrottled,
				Message: "server busy: global in-flight request limit reached",
			}
			select {
			case respCh <- responseEnvelope{resp: resp, releaseID: 0, shouldRelease: false}:
			case <-connCtx.Done():
			}
			<-inFlightSem
			continue
		}

		// 4. Duplicate SeqID defense
		seqID := frame.Header.SeqID
		activeMu.Lock()
		if _, exists := activeSeqIDs[seqID]; exists {
			activeMu.Unlock()
			if s.maxGlobalInFlight > 0 {
				s.globalInFlight.Add(-1)
			}
			metrics.PipelineLimitHits.Add(1)
			resp := &Response{
				OpCode:  frame.Header.OpCode,
				SeqID:   seqID,
				Status:  StatusInvalidRequest,
				Message: fmt.Sprintf("duplicate active seq_id: %d", seqID),
			}
			select {
			case respCh <- responseEnvelope{resp: resp, releaseID: 0, shouldRelease: false}:
			case <-connCtx.Done():
			}
			<-inFlightSem
			continue
		}
		activeSeqIDs[seqID] = struct{}{}
		activeMu.Unlock()

		// 5. Decode Request
		req, err := DecodeRequest(frame)
		if err != nil {
			// Frame was structurally valid, but payload violated application constraints.
			if s.maxGlobalInFlight > 0 {
				s.globalInFlight.Add(-1)
			}
			resp := &Response{
				OpCode:  frame.Header.OpCode,
				SeqID:   seqID,
				Status:  StatusInvalidRequest,
				Message: err.Error(),
			}
			env := responseEnvelope{
				resp:          resp,
				releaseID:     seqID,
				shouldRelease: true,
			}
			select {
			case respCh <- env:
			case <-connCtx.Done():
				activeMu.Lock()
				delete(activeSeqIDs, seqID)
				activeMu.Unlock()
			}
			<-inFlightSem
			continue
		}

		// 6. Concurrently dispatch request
		inFlightWG.Add(1)
		metrics.InFlightRequests.Add(1)
		go func(r *Request, rawSeqID uint64) {
			defer inFlightWG.Done()
			var handoffSuccess bool
			defer func() {
				if !handoffSuccess {
					activeMu.Lock()
					delete(activeSeqIDs, rawSeqID)
					activeMu.Unlock()
				}
				metrics.InFlightRequests.Add(-1)
				if s.maxGlobalInFlight > 0 {
					s.globalInFlight.Add(-1)
				}
				<-inFlightSem
			}()

			resp := s.dispatchWithContext(connCtx, r)
			env := responseEnvelope{
				resp:          resp,
				releaseID:     rawSeqID,
				shouldRelease: true,
			}

			select {
			case respCh <- env:
				handoffSuccess = true
			case <-connCtx.Done():
			}
		}(req, seqID)
	}

	// Reader has exited (EOF, read error, or shutdown).
	// Trigger context cancellation for in-flight requests and wait for their completion.
	connCancel()
	inFlightWG.Wait()

	// All in-flight requests have completed and deposited their responses in respCh.
	close(respCh)

	// Await completion of the response writer goroutine.
	<-writerDone
}

// readFrameWithDeadlines reads a complete M01 frame while strictly defending against Slowloris attacks.
func (s *Server) readFrameWithDeadlines(conn net.Conn, isFirst bool) (*Frame, error) {
	// Step 1: Set idle or header deadline before reading first byte
	if isFirst {
		if err := conn.SetReadDeadline(time.Now().Add(s.cfg.HeaderTimeout)); err != nil {
			return nil, err
		}
	} else {
		if err := conn.SetReadDeadline(time.Now().Add(s.cfg.IdleTimeout)); err != nil {
			return nil, err
		}
	}

	var headerBuf [HeaderSize]byte
	// Read first byte to observe activity / wait for idle timeout
	if _, err := io.ReadFull(conn, headerBuf[:1]); err != nil {
		return nil, err
	}

	// First byte received:
	// If transitioning from idle state (!isFirst), enforce HeaderTimeout for the remainder of the header.
	// For the initial request (isFirst == true), HeaderTimeout was already set before the first byte
	// and remains active, bounding the total time to receive the full 18-byte header to HeaderTimeout.
	if !isFirst {
		if err := conn.SetReadDeadline(time.Now().Add(s.cfg.HeaderTimeout)); err != nil {
			return nil, err
		}
	}
	if _, err := io.ReadFull(conn, headerBuf[1:]); err != nil {
		return nil, err
	}

	// Decode header: verifies Magic and PayloadLength <= 5 MiB before allocating payload buffer
	hdr, err := DecodeHeaderBytes(headerBuf[:])
	if err != nil {
		return nil, err
	}

	// Step 2: Enforce PayloadTimeout for payload and trailer read
	if err := conn.SetReadDeadline(time.Now().Add(s.cfg.PayloadTimeout)); err != nil {
		return nil, err
	}

	var payload []byte
	var expectedCRC uint32
	if hdr.PayloadLength > 0 {
		bufPtr, is64K := getPooledBuffer(hdr.PayloadLength)
		tempBuf := (*bufPtr)[:hdr.PayloadLength]

		if _, err := io.ReadFull(conn, tempBuf); err != nil {
			putPooledBuffer(bufPtr, is64K)
			return nil, err
		}

		var trailerBuf [TrailerSize]byte
		if _, err := io.ReadFull(conn, trailerBuf[:]); err != nil {
			putPooledBuffer(bufPtr, is64K)
			return nil, err
		}

		computedCRC := crc32.ChecksumIEEE(headerBuf[:])
		computedCRC = crc32.Update(computedCRC, crc32.IEEETable, tempBuf)

		expectedCRC = binary.GetUint32(trailerBuf[:])
		if computedCRC != expectedCRC {
			putPooledBuffer(bufPtr, is64K)
			return nil, &errors.ChecksumMismatchError{Expected: expectedCRC, Actual: computedCRC}
		}

		payload = make([]byte, hdr.PayloadLength)
		copy(payload, tempBuf)
		putPooledBuffer(bufPtr, is64K)
	} else {
		var trailerBuf [TrailerSize]byte
		if _, err := io.ReadFull(conn, trailerBuf[:]); err != nil {
			return nil, err
		}

		computedCRC := crc32.ChecksumIEEE(headerBuf[:])
		expectedCRC = binary.GetUint32(trailerBuf[:])
		if computedCRC != expectedCRC {
			return nil, &errors.ChecksumMismatchError{Expected: expectedCRC, Actual: computedCRC}
		}
	}

	// Reset read deadline upon successful frame completion
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return nil, err
	}

	return &Frame{
		Header:  *hdr,
		Payload: payload,
		CRC:     expectedCRC,
	}, nil
}

// writeResponseWithDeadline encodes and writes a complete Response frame within WriteTimeout.
func (s *Server) writeResponseWithDeadline(conn net.Conn, resp *Response) error {
	if err := conn.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout)); err != nil {
		return err
	}
	defer func() { _ = conn.SetWriteDeadline(time.Time{}) }()
	return WriteResponse(conn, resp)
}

// dispatch routes a decoded request to the appropriate Engine method and translates outcomes to a Response.
func (s *Server) dispatch(req *Request) *Response {
	return s.dispatchWithContext(context.Background(), req)
}

// dispatchWithContext routes a decoded request with execution lifetime bound by parentCtx and RequestTimeout.
func (s *Server) dispatchWithContext(parentCtx context.Context, req *Request) (finalResp *Response) {
	start := time.Now()
	defer func() {
		if finalResp != nil {
			var opStr string
			switch req.OpCode {
			case OpPut:
				opStr = "put"
			case OpGet:
				opStr = "get"
			case OpDelete:
				opStr = "delete"
			case OpBatch:
				opStr = "batch"
			default:
				return
			}
			var statusStr string
			switch finalResp.Status {
			case StatusOk:
				statusStr = "ok"
			case StatusKeyNotFound:
				statusStr = "not_found"
			case StatusNotLeader:
				statusStr = "not_leader"
			case StatusThrottled:
				statusStr = "throttled"
			default:
				statusStr = "error"
			}
			metrics.RequestDuration.WithLabelValues(opStr, statusStr).ObserveDuration(time.Since(start))
		}
	}()

	resp := &Response{
		OpCode: req.OpCode,
		SeqID:  req.SeqID,
	}

	if s.closed.Load() {
		resp.Status = StatusServerClosed
		resp.Message = "server is closed"
		return resp
	}

	ctx, cancel := context.WithTimeout(parentCtx, s.cfg.RequestTimeout)
	defer cancel()

	router := s.ProposalRouter()

	switch req.OpCode {
	case OpPut:
		if router != nil {
			r, err := router.RouteWrite(ctx, req)
			if err != nil {
				resp.Status = StatusError
				resp.Message = "internal routing error"
				return resp
			}
			return r
		}
		// P16-SEC-F01: In cluster mode, NEVER fall through to direct Engine writes.
		if s.clusterMode {
			resp.Status = StatusError
			resp.Message = "cluster mode active but consensus router unavailable"
			return resp
		}
		err := s.engine.Put(ctx, req.Key, req.Value)
		s.mapEngineError(err, resp)

	case OpGet:
		readRouter := s.ReadRouter()
		if readRouter != nil {
			r, err := readRouter.RouteRead(ctx, req)
			if err != nil {
				resp.Status = StatusError
				resp.Message = "internal routing error"
				return resp
			}
			return r
		}
		// In cluster mode, NEVER fall through to direct unverified Engine reads (P17-S01-M02).
		if s.clusterMode {
			resp.Status = StatusError
			resp.Message = "cluster mode active but consensus router unavailable"
			return resp
		}
		val, err := s.engine.Get(req.Key)
		if err == nil {
			if uint32(len(val)) > MaxPayloadLength {
				resp.Status = StatusError
				resp.Message = "response value exceeds maximum protocol frame limit"
			} else {
				resp.Status = StatusOk
				resp.Value = val
			}
		} else {
			s.mapEngineError(err, resp)
		}

	case OpDelete:
		if router != nil {
			r, err := router.RouteWrite(ctx, req)
			if err != nil {
				resp.Status = StatusError
				resp.Message = "internal routing error"
				return resp
			}
			return r
		}
		// P16-SEC-F01: In cluster mode, NEVER fall through to direct Engine writes.
		if s.clusterMode {
			resp.Status = StatusError
			resp.Message = "cluster mode active but consensus router unavailable"
			return resp
		}
		err := s.engine.Delete(ctx, req.Key)
		s.mapEngineError(err, resp)

	case OpExists:
		readRouter := s.ReadRouter()
		if readRouter != nil {
			r, err := readRouter.RouteRead(ctx, req)
			if err != nil {
				resp.Status = StatusError
				resp.Message = "internal routing error"
				return resp
			}
			return r
		}
		// In cluster mode, NEVER fall through to direct unverified Engine reads.
		if s.clusterMode {
			resp.Status = StatusError
			resp.Message = "cluster mode active but consensus router unavailable"
			return resp
		}
		exists, err := s.engine.Exists(req.Key)
		if err == nil {
			resp.Status = StatusOk
			resp.Exists = exists
		} else {
			s.mapEngineError(err, resp)
		}

	case OpBatch:
		if router != nil {
			r, err := router.RouteWrite(ctx, req)
			if err != nil {
				resp.Status = StatusError
				resp.Message = "internal routing error"
				return resp
			}
			return r
		}
		// In cluster mode, NEVER fall through to direct Engine writes.
		if s.clusterMode {
			resp.Status = StatusError
			resp.Message = "cluster mode active but consensus router unavailable"
			return resp
		}
		bOps := make([]binary.BatchOp, len(req.Batch))
		for i, op := range req.Batch {
			bOps[i] = binary.BatchOp{
				Type:  binary.OpType(op.Type),
				Key:   op.Key,
				Value: op.Value,
			}
		}
		err := s.engine.Batch(ctx, bOps)
		s.mapEngineError(err, resp)

	case OpStats:
		snap, err := s.CollectStats()
		if err != nil {
			s.mapEngineError(err, resp)
			return resp
		}
		data, err := json.MarshalIndent(snap, "", "  ")
		if err != nil {
			resp.Status = StatusError
			resp.Message = "failed to serialize stats snapshot"
			return resp
		}
		if len(data) > MaxStatsPayloadLength {
			resp.Status = StatusError
			resp.Message = fmt.Sprintf("stats snapshot payload length %d exceeds maximum %d", len(data), MaxStatsPayloadLength)
			return resp
		}
		resp.Status = StatusOk
		resp.Value = data

	default:
		resp.Status = StatusInvalidRequest
		resp.Message = fmt.Sprintf("unsupported operation code: 0x%02x", byte(req.OpCode))
	}

	return resp
}

// mapEngineError maps storage engine errors to stable protocol status codes without disclosing internal paths.
func (s *Server) mapEngineError(err error, resp *Response) {
	if err == nil {
		resp.Status = StatusOk
		return
	}
	if stdErrors.Is(err, errors.ErrKeyNotFound) {
		resp.Status = StatusKeyNotFound
		return
	}
	if stdErrors.Is(err, errors.ErrWriterClosed) {
		resp.Status = StatusServerClosed
		resp.Message = "storage engine is closed"
		return
	}
	if stdErrors.Is(err, context.DeadlineExceeded) || stdErrors.Is(err, context.Canceled) {
		resp.Status = StatusThrottled
		resp.Message = "request timed out under storage backpressure"
		return
	}
	if stdErrors.Is(err, errors.ErrKeyTooLarge) {
		resp.Status = StatusInvalidRequest
		resp.Message = errors.ErrKeyTooLarge.Error()
		return
	}
	if stdErrors.Is(err, errors.ErrValueTooLarge) {
		resp.Status = StatusInvalidRequest
		resp.Message = errors.ErrValueTooLarge.Error()
		return
	}
	if stdErrors.Is(err, errors.ErrEmptyKey) {
		resp.Status = StatusInvalidRequest
		resp.Message = errors.ErrEmptyKey.Error()
		return
	}
	if stdErrors.Is(err, errors.ErrInvalidPayload) {
		resp.Status = StatusInvalidRequest
		resp.Message = errors.ErrInvalidPayload.Error()
		return
	}
	if stdErrors.Is(err, errors.ErrInvalidOpType) {
		resp.Status = StatusInvalidRequest
		resp.Message = errors.ErrInvalidOpType.Error()
		return
	}
	// Sanitize general storage errors to avoid disclosing filesystem paths or internal diagnostics
	resp.Status = StatusError
	resp.Message = "internal storage error"
}

// Shutdown gracefully shuts down the server: halts listener, closes active connections,
// and waits for connection goroutines to complete within ctx deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	if !s.closed.CompareAndSwap(false, true) {
		select {
		case <-s.shutdownDone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	close(s.shutdownCh)

	// Close listener to stop accepting new connections
	if s.listener != nil {
		_ = s.listener.Close()
	}

	// Close all currently active connections
	s.mu.Lock()
	for conn := range s.conns {
		_ = conn.SetDeadline(time.Now())
		_ = conn.Close()
	}
	s.mu.Unlock()

	// Await connection goroutine drain
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
		close(s.shutdownDone)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close terminates the server immediately using the configured ShutdownTimeout.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	return s.Shutdown(ctx)
}

// TestDispatch exposes dispatch for testing internal request routing without opening network connections.
func (s *Server) TestDispatch(req *Request) *Response {
	return s.dispatch(req)
}

// ServeConnForTesting runs handleConn synchronously on conn for testing connection error paths.
func (s *Server) ServeConnForTesting(conn net.Conn) {
	if s.trackConn(conn) {
		s.handleConn(conn)
	} else {
		_ = conn.Close()
	}
}

// CollectStats gathers an authoritative point-in-time diagnostic snapshot across
// the local engine, cluster consensus, and transport connection layers.
func (s *Server) CollectStats() (*StatsSnapshot, error) {
	engStats, memStats, storStats, cacheStats, err := s.engine.Stats()
	if err != nil {
		return nil, err
	}

	snap := &StatsSnapshot{
		Engine:      engStats,
		Memory:      memStats,
		Storage:     storStats,
		Cache:       cacheStats,
		Connections: ConnStats{Active: s.activeConns.Load()},
	}

	if s.clusterMode {
		snap.Cluster = ClusterStats{Enabled: true}
		s.routerMu.RLock()
		r := s.router
		rr := s.readRouter
		s.routerMu.RUnlock()

		if cp, ok := r.(ClusterStatsProvider); ok {
			snap.Cluster = cp.ClusterStats()
		} else if cp, ok := rr.(ClusterStatsProvider); ok {
			snap.Cluster = cp.ClusterStats()
		}
	} else {
		snap.Cluster = ClusterStats{Enabled: false}
	}

	return snap, nil
}
