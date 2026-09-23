package transport

import (
	"context"
	stdErrors "errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
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
	// When false (default), binding to any non-loopback address (e.g. 0.0.0.0, public IP) is rejected with ErrInsecureTransport.
	InsecureTransport bool

	// MaxConnections is the maximum number of concurrent client connections (default: 4096).
	MaxConnections int

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

// DefaultServerConfig returns a production-hardened ServerConfig with safe default timeouts and limits.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		Address:         "127.0.0.1:9099",
		MaxConnections:  4096,
		HeaderTimeout:   5 * time.Second,
		PayloadTimeout:  10 * time.Second,
		IdleTimeout:     60 * time.Second,
		WriteTimeout:    5 * time.Second,
		RequestTimeout:  5 * time.Second,
		ShutdownTimeout: 5 * time.Second,
	}
}

// isLoopbackAddress reports whether the given TCP address specifies a loopback interface.
func isLoopbackAddress(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "pipe" || host == "local" {
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

	routerMu    sync.RWMutex
	router      ProposalRouter
	readRouter  ReadRouter
	clusterMode bool // immutable after construction (P16-SEC-F01)
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
	if !cfg.InsecureTransport && !isLoopbackAddress(cfg.Address) {
		return nil, errors.ErrInsecureTransport
	}
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = defaults.MaxConnections
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
		cfg:          cfg,
		engine:       eng,
		router:       cfg.ProposalRouter,
		readRouter:   readRouter,
		clusterMode:  cfg.ClusterMode,
		conns:        make(map[net.Conn]struct{}),
		shutdownCh:   make(chan struct{}),
		shutdownDone: make(chan struct{}),
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
	if !s.cfg.InsecureTransport && !isLoopbackAddress(bindAddr) {
		s.started.Store(false)
		return errors.ErrInsecureTransport
	}

	l, err := net.Listen("tcp", bindAddr)
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
	if !s.cfg.InsecureTransport && !isLoopbackAddress(addrStr) {
		s.started.Store(false)
		return errors.ErrInsecureTransport
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

// handleConn is the per-connection request-processing loop.
func (s *Server) handleConn(conn net.Conn) {
	defer func() {
		conn.Close()
		s.untrackConn(conn)
	}()

	isFirst := true
	for {
		if s.closed.Load() {
			return
		}

		frame, err := s.readFrameWithDeadlines(conn, isFirst)
		if err != nil {
			// Normal EOF, read timeout (Slowloris/idle), or fatal framing error.
			return
		}
		isFirst = false

		req, err := DecodeRequest(frame)
		if err != nil {
			// Frame was structurally valid, but payload violated application constraints.
			resp := &Response{
				OpCode:  frame.Header.OpCode,
				SeqID:   frame.Header.SeqID,
				Status:  StatusInvalidRequest,
				Message: err.Error(),
			}
			if writeErr := s.writeResponseWithDeadline(conn, resp); writeErr != nil {
				return
			}
			continue
		}

		resp := s.dispatch(req)
		if writeErr := s.writeResponseWithDeadline(conn, resp); writeErr != nil {
			return
		}
	}
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
func (s *Server) dispatch(req *Request) (finalResp *Response) {
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

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.RequestTimeout)
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
		resp.Status = StatusInvalidRequest
		resp.Message = "unsupported operation: EXISTS is not implemented by storage engine"

	case OpBatch:
		resp.Status = StatusInvalidRequest
		resp.Message = "unsupported operation: BATCH is not implemented by storage engine"

	case OpStats:
		resp.Status = StatusInvalidRequest
		resp.Message = "unsupported operation: STATS is not implemented by storage engine"

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
