package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// PprofServer manages an isolated HTTP diagnostics server exposing Go runtime/pprof
// endpoints strictly bound to a local loopback interface.
type PprofServer struct {
	listener net.Listener
	server   *http.Server
	started  atomic.Bool
	closed   atomic.Bool
	serveErr error
	done     chan struct{}
	mu       sync.Mutex
}

// NewPprofServer creates and binds a new PprofServer on the specified loopback address.
// The TCP listener is bound synchronously so port conflicts or illegal address bindings
// fail-fast during initialization.
func NewPprofServer(addr string) (*PprofServer, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("pprof server error: invalid address %q (expected host:port): %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("pprof server error: invalid port in address %q", addr)
	}
	if !isLoopback(host) {
		return nil, fmt.Errorf("pprof server error: address %q must be a loopback interface (127.0.0.1, ::1, localhost)", addr)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("pprof server error: failed to listen on %s: %w", addr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))
	mux.Handle("/debug/pprof/block", pprof.Handler("block"))
	mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	mux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
	mux.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))

	// Operational convenience redirects
	mux.HandleFunc("/debug/pprof", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/debug/pprof/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/debug/pprof/", http.StatusMovedPermanently)
			return
		}
		http.NotFound(w, r)
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	return &PprofServer{
		listener: ln,
		server:   srv,
		done:     make(chan struct{}),
	}, nil
}

// Start initiates the HTTP server accept loop in a background goroutine.
func (s *PprofServer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed.Load() {
		return errors.New("pprof server already closed")
	}
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("pprof server already started")
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

// Addr returns the bound network address of the pprof listener, or nil if uninitialized.
func (s *PprofServer) Addr() net.Addr {
	if s.listener != nil {
		return s.listener.Addr()
	}
	return nil
}

// Shutdown gracefully shuts down the pprof server. It is safe for concurrent
// and repeated invocation (idempotent). If the provided context expires before
// active connections are drained, it forcefully closes the server.
func (s *PprofServer) Shutdown(ctx context.Context) error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}

	if !s.started.Load() {
		if s.listener != nil {
			return s.listener.Close()
		}
		return nil
	}

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
