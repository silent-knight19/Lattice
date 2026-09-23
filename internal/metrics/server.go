package metrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultMetricsPort is the canonical Prometheus metrics HTTP port (:9100).
	DefaultMetricsPort = 9100

	// DefaultMaxScraperConnections limits concurrent HTTP scraper connections to prevent socket exhaustion.
	DefaultMaxScraperConnections int64 = 256

	// ContentTypePrometheus is the standard Prometheus text exposition format content type.
	ContentTypePrometheus = "text/plain; version=0.0.4; charset=utf-8"
)

// Handler returns an http.Handler that formats and writes metrics from the given Registry.
// Only HTTP GET is permitted; other methods are rejected with HTTP 405 Method Not Allowed.
func Handler(reg *Registry) http.Handler {
	if reg == nil {
		reg = DefaultRegistry
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", ContentTypePrometheus)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_ = reg.WritePrometheus(w)
	})
}

// Server manages an isolated HTTP server exposing the /metrics Prometheus endpoint.
type Server struct {
	listener    net.Listener
	server      *http.Server
	registry    *Registry
	started     atomic.Bool
	closed      atomic.Bool
	activeConns *atomic.Int64
	maxConns    int64
	serveErr    error
	done        chan struct{}
	mu          sync.Mutex
}

// connLimiterListener limits the number of concurrent active TCP connections.
type connLimiterListener struct {
	net.Listener
	active   *atomic.Int64
	maxConns int64
}

type trackedConn struct {
	net.Conn
	active *atomic.Int64
	once   sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() {
		c.active.Add(-1)
	})
	return c.Conn.Close()
}

func (l *connLimiterListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.maxConns > 0 && l.active.Load() >= l.maxConns {
			_ = conn.Close()
			continue
		}
		l.active.Add(1)
		return &trackedConn{
			Conn:   conn,
			active: l.active,
		}, nil
	}
}

// NewServer creates and synchronously binds a new metrics HTTP Server on the specified address.
func NewServer(addr string, reg *Registry) (*Server, error) {
	if reg == nil {
		reg = DefaultRegistry
	}

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		// Attempt port-only normalization
		if p, pErr := strconv.Atoi(addr); pErr == nil && p >= 0 && p <= 65535 {
			addr = net.JoinHostPort("127.0.0.1", addr)
			host = "127.0.0.1"
			portStr = strconv.Itoa(p)
		} else {
			return nil, fmt.Errorf("metrics server error: invalid address %q (expected host:port): %w", addr, err)
		}
	}

	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("metrics server error: invalid port in address %q", addr)
	}
	_ = host

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("metrics server error: failed to listen on %s: %w", addr, err)
	}

	var activeConns atomic.Int64
	limitedLn := &connLimiterListener{
		Listener: ln,
		active:   &activeConns,
		maxConns: DefaultMaxScraperConnections,
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", Handler(reg))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/metrics", http.StatusMovedPermanently)
			return
		}
		http.NotFound(w, r)
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB
	}

	return &Server{
		listener:    limitedLn,
		server:      srv,
		registry:    reg,
		activeConns: &activeConns,
		maxConns:    DefaultMaxScraperConnections,
		done:        make(chan struct{}),
	}, nil
}

// Start initiates the HTTP server accept loop in a background goroutine.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed.Load() {
		return errors.New("metrics server already closed")
	}
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("metrics server already started")
	}

	go func() {
		defer close(s.done)
		err := s.server.Serve(s.listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.mu.Lock()
			s.serveErr = err
			s.mu.Unlock()
		}
	}()

	return nil
}

// Err returns any unrecoverable error encountered by the background accept loop.
func (s *Server) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.serveErr
}

// Addr returns the bound network address of the metrics listener, or nil if uninitialized.
func (s *Server) Addr() net.Addr {
	if s.listener != nil {
		return s.listener.Addr()
	}
	return nil
}

// Shutdown gracefully stops the metrics server, closing the listener and draining active scraper requests.
func (s *Server) Shutdown(ctx context.Context) error {
	if !s.closed.CompareAndSwap(false, true) {
		select {
		case <-s.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	s.mu.Lock()
	started := s.started.Load()
	if !started {
		close(s.done)
		var err error
		if s.listener != nil {
			err = s.listener.Close()
		}
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	shutdownErr := make(chan error, 1)
	go func() {
		shutdownErr <- s.server.Shutdown(ctx)
	}()

	select {
	case err := <-shutdownErr:
		<-s.done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		_ = s.server.Close()
		<-s.done
		return ctx.Err()
	}
}

// Close forcefully terminates the metrics server immediately.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	return s.Shutdown(ctx)
}
